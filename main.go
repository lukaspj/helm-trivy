package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"golang.org/x/net/context"
)

var debug = false

func getChartImages(chart string, set string, values string, version string) (error, []string) {
	images := []string{}
	cmd := []string{"template"}
	if len(set) > 0 {
		cmd = append(cmd, "--set", set)
	}
	if len(values) > 0 {
		cmd = append(cmd, "--values", values)
	}
	if len(version) > 0 {
		cmd = append(cmd, "--version", version)
	}
	cmd = append(cmd, chart)
	log.Debugf("Running helm cmd: helm %v", cmd)
	out, err := exec.Command("helm", cmd...).Output()
	if err != nil {
		return err, images
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
ScannerLoop:
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "image: ") {
			continue
		}
		image := strings.Split(line, "image: ")[1]
		image = strings.Trim(image, "\"")
		log.Debugf("Found image %v", image)
		for _, v := range images {
			if v == image {
				continue ScannerLoop
			}
		}
		images = append(images, image)
	}
	return nil, images
}

func scanImage(image string, ctx context.Context, cli *client.Client, cacheDir string, json bool, trivyOpts string, trivyUser string, dockerUser string, dockerPass string) string {
	config := container.Config{
		Image: "aquasec/trivy",
		Cmd:   []string{"--cache-dir", "/.cache"},
		Tty:   true,
		User:  trivyUser,
		Env: []string{"TRIVY_USERNAME=" + dockerUser, "TRIVY_PASSWORD=" + dockerPass},
	}
	if json {
		config.Cmd = append(config.Cmd, "-f", "json")
	}
	if debug {
		config.Cmd = append(config.Cmd, "-d")
	} else {
		config.Cmd = append(config.Cmd, "-q")
	}
	config.Cmd = append(config.Cmd, strings.Fields(trivyOpts)...)
	config.Cmd = append(config.Cmd, image)
	resp, err := cli.ContainerCreate(ctx, &config, &container.HostConfig{
		Binds: []string{cacheDir + ":/.cache"},
	}, nil, "")
	if err != nil {
		log.Fatalf("Could not create trivy container: %v", err)
	}
	log.Debugf("Starting container with command: %v", config.Cmd)
	if err := cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		log.Fatalf("Could not start trivy container: %v", err)
	}
	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("Error while waiting for container: %v", err)
		}
	case <-statusCh:
	}

	out, err := cli.ContainerLogs(ctx, resp.ID, types.ContainerLogsOptions{ShowStdout: true, ShowStderr: false})
	if err != nil {
		log.Fatalf("Cannot get container logs: %v", err)
	}
	outputContent, _ := ioutil.ReadAll(out)
	return string(outputContent)
}

func scanChart(chart string, json bool, ctx context.Context, cli *client.Client, cacheDir string, trivyOpts string, trivyUser string, dockerUser string, dockerPass string, templateSet string, templateValues string, chartversion string) {
	log.Infof("Scanning chart %s", chart)
	jsonOutput := ""
	if err, images := getChartImages(chart, templateSet, templateValues, chartversion); err != nil {
		log.Fatalf("Could not find images for chart %v: %v. Did you run 'helm repo update' ?", chart, err)
	} else {
		if len(images) == 0 {
			log.Fatalf("No images found in chart %s.", chart)
		}
		log.Debugf("Found images for chart %v: %v", chart, images)
		for _, image := range images {
			log.Debugf("Scanning image %v", image)
			output := scanImage(image, ctx, cli, cacheDir, json, trivyOpts, trivyUser, dockerUser, dockerPass)
			if json {
				jsonOutput += output
			} else {
				fmt.Println(output)
			}
		}
	}
	if json {
		fmt.Println(strings.ReplaceAll(jsonOutput, "][", ","))
	}
}

func main() {
	var jsonOutput bool
	var noPull bool
	var chart string = ""
	var templateSet = ""
	var templateValues = ""
	var chartVersion = ""
	var trivyArgs = ""
	var trivyUser = ""
	var cacheDir = ""
	
	var dockerUser = ""
	var dockerPass = ""

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: helm trivy [options] <helm chart>\n")
		fmt.Fprintf(os.Stderr, "Example: helm trivy -json stable/mariadb\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	flag.BoolVar(&jsonOutput, "json", false, "Enable JSON output")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&noPull, "nopull", false, "Don't pull latest trivy image")
	flag.StringVar(&trivyArgs, "trivyargs", "", "CLI args to passthrough to trivy")
	flag.StringVar(&trivyUser, "trivyuser", "1000", "Specify user to run Trivy as")
	flag.StringVar(&dockerUser, "dockeruser", "", "Specify Docker Auth username")
	flag.StringVar(&dockerPass, "dockerpass", "", "Specify Docker Auth password")
	flag.StringVar(&templateSet, "set", "", "Values to set for helm chart, format: 'key1=value1,key2=value2'")
	flag.StringVar(&templateValues, "values", "", "Specify chart values in a YAML file or a URL")
	flag.StringVar(&chartVersion, "version", "", "Specify chart version")
	flag.StringVar(&cacheDir, "cachedir", "", "Set vuln cache dir, if empty a tmp dir is used")
	flag.Parse()

	if debug {
		log.SetLevel(log.DebugLevel)
	}

	if len(flag.Args()) == 0 {
		fmt.Fprintf(os.Stderr, "Error: No chart specified.\n")
		flag.Usage()
		os.Exit(2)
	} else {
		chart = flag.Args()[0]
	}

	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Could not get docker client: %v", err)
	}

	if !noPull {
		log.Info("Pulling latest trivy image")
		_, err := cli.ImagePull(ctx, "aquasec/trivy", types.ImagePullOptions{})
		if err != nil {
			panic(err)
		}
		log.Info("Pulled latest trivy image")
	}
	if cacheDir == "" {
		cacheDir, err := ioutil.TempDir("", "helm-trivy")
		if err != nil {
			log.Fatalf("Could not create cache dir: %v", err)
		}
		defer os.RemoveAll(cacheDir)

		go func(cacheDir string) {
			sigCh := make(chan os.Signal)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			<-sigCh
			os.RemoveAll(cacheDir)
			os.Exit(0)
		}(cacheDir)
	}
	log.Debugf("Using %v as cache directory for vuln db", cacheDir)
	log.Debugf("Using %v as user for vulnerability scanning", trivyUser)

	scanChart(chart, jsonOutput, ctx, cli, cacheDir, trivyArgs, trivyUser, dockerUser, dockerPass, templateSet, templateValues, chartVersion)
}
