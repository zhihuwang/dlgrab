package main

import (
	"fmt"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/fsouza/go-dockerclient"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var layerLock sync.Mutex
var layerId string = ""

var logger = &Logger{}

var (
	GITCOMMIT string
)

func main() {
	var outDir string
	var doDebug bool
	var doHelp bool
	var doTagRemove bool
	var regFormat bool

	helpFd := os.Stderr
	flag.Usage = func() {
		fmt.Fprintf(helpFd, "Usage for %s [flags...] LAYER\n", os.Args[0])
		fmt.Fprintf(helpFd, "  LAYER: layer id to export, or image name to export top layer of\n")
		flag.PrintDefaults()
		fmt.Fprintf(helpFd, "\n")
		fmt.Fprintf(helpFd, "  The DOCKER_HOST environment variable overrides the default location to find the docker daemon\n")
	}

	flag.BoolVar(&doHelp, []string{"h", "-help"}, false, "Print this help text")
	flag.StringVar(&outDir, []string{"o", "-outdir"}, ".", "Directory to write layer to")
	flag.BoolVar(&doTagRemove, []string{"-clean"}, false, "Remove the temporary tag after use\nWARNING: can trigger layer deletion if run on a layer with no children or other references")
	flag.BoolVar(&doDebug, []string{"-debug"}, false, "Set log level to debug")
	flag.BoolVar(&regFormat, []string{"-registry-format"}, false, "Output in the format a registry would use, rather than for an image export")
	flag.Parse()

	if len(flag.Args()) != 1 {
		flag.Usage()
		os.Exit(2)
	}
	imgId := flag.Arg(0)
	if len(imgId) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if doHelp {
		helpFd = os.Stdout
		flag.Usage()
		return
	}
	logger.Level = INFO
	if doDebug {
		logger.Level = DEBUG
	}

	logger.Debug("DLGrab version %s", GITCOMMIT)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill, os.Signal(syscall.SIGTERM))
	go func() {
		sig := <-c
		logger.Debug("Received signal '%v', exiting\n", sig)
		os.Exit(1)
	}()

	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	client, err := docker.NewClient(endpoint)
	if err != nil {
		logger.Error("%s", err.Error())
		os.Exit(1)
	}

	layerLock.Lock()
	imgJson, err := client.InspectImage(imgId)
	if err != nil {
		logger.Error("%s", err.Error())
		os.Exit(1)
	}
	layerId = imgJson.ID
	if layerId != imgId {
		logger.Info("Full layer id found: %s", layerId)
	}
	layerLock.Unlock()

	logger.Info("Layer folder will be dumped into %s", outDir)
	layerLock.Lock()
	layerOutDir := filepath.Join(outDir, layerId)
	layerLock.Unlock()
	err = os.Mkdir(layerOutDir, 0755)
	if err != nil {
		logger.Error("%s", err.Error())
		os.Exit(1)
	}
	if !regFormat {
		ioutil.WriteFile(filepath.Join(layerOutDir, "VERSION"), []byte("1.0"), 0644)
	}

	logger.Debug("Attempting to probe for available port")
	laddr := net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	sock, err := net.ListenTCP("tcp", &laddr)
	if err != nil {
		logger.Error("%s", err.Error())
		os.Exit(1)
	}
	listenOn := fmt.Sprintf("127.0.0.1:%d", sock.Addr().(*net.TCPAddr).Port)
	sock.Close()

	logger.Debug("Starting shim registry on %s", listenOn)
	go (func() {
		if err := http.ListenAndServe(listenOn, NewHandler(outDir, regFormat)); err != nil {
			logger.Error("%s", err.Error())
			os.Exit(1)
		}
	})()

	sleeps := []int{1, 5, 10, 100, 200, 500, 1000, 2000}
	pingUrl := "http://" + listenOn + "/v1/_ping"
	apiIsUp := false
	logger.Debug("Waiting for shim registry to start by checking %s", pingUrl)
	for _, ms := range sleeps {
		logger.Debug("Sleeping %d ms before ping", ms)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		resp, err := http.Get(pingUrl)
		if err == nil {
			resp.Body.Close()
			apiIsUp = true
			break
		}
	}
	if !apiIsUp {
		logger.Error("Shim registry took too long to come up")
		os.Exit(1)
	}

	logger.Debug("Shim Registry Started")

	err = dockerMain(client, listenOn, doTagRemove)
	if err != nil {
		logger.Error("%s", err)
		os.Exit(1)
	}

	logger.Info("Export complete")
}

func dockerMain(client *docker.Client, regUrl string, removeTag bool) (err error) {
	imgNiceName := "dlgrab_tmp"
	imgName := regUrl + "/" + "dlgrab_push_staging_tmp"
	imgTag := "latest"

	logger.Debug("Tagging image into temporary repo")
	tagOpts := docker.TagImageOptions{
		Repo:  imgName,
		Tag:   imgTag,
		Force: true,
	}
	layerLock.Lock()
	err = client.TagImage(layerId, tagOpts)
	layerLock.Unlock()
	if err != nil {
		return
	}
	tagOpts.Repo = imgNiceName
	layerLock.Lock()
	err = client.TagImage(layerId, tagOpts)
	layerLock.Unlock()
	if err != nil {
		return
	}

	logger.Debug("Pushing image")
	pushOpts := docker.PushImageOptions{
		Registry: "",
		Name:     imgName,
		Tag:      imgTag,
	}
	err = client.PushImage(pushOpts, docker.AuthConfiguration{})
	if err != nil {
		return
	}

	// Unfortunately even with no-prune docker will remove a layer if the
	// tag we've removed is the only one left. As such, we do a dance to
	// have *two* symbolic tags on the image so we can leave one that looks
	// slightly more aesthetically pleasing in place.
	removeOpts := docker.RemoveImageOptions{Force: false, NoPrune: true}
	logger.Debug("Removing ugly temporary image tag")
	err = client.RemoveImageExtended(imgName, removeOpts)
	if err != nil {
		return
	}
	if removeTag {
		logger.Debug("Removing nice temporary image tag")
		err = client.RemoveImageExtended(imgNiceName, removeOpts)
		if err != nil {
			return
		}
	}

	return nil
}
