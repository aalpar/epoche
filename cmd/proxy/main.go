package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/aalpar/epoche/internal/proxy"
)

func main() {
	var (
		livenessUpstream  string
		readinessUpstream string
		managePort        int
		probePort         int
		statePath         string
	)

	flag.StringVar(&livenessUpstream, "liveness-upstream", "", "URL of the main container's liveness endpoint")
	flag.StringVar(&readinessUpstream, "readiness-upstream", "", "URL of the main container's readiness endpoint")
	flag.IntVar(&managePort, "manage-port", 9901, "Port for management API")
	flag.IntVar(&probePort, "probe-port", 9902, "Port for probe endpoints")
	flag.StringVar(&statePath, "state-path", "/etc/epoche/frozen", "Path to downward API state file")
	flag.Parse()

	if livenessUpstream == "" || readinessUpstream == "" {
		log.Fatal("--liveness-upstream and --readiness-upstream are required")
	}

	p := proxy.New(proxy.Config{
		LivenessUpstream:  livenessUpstream,
		ReadinessUpstream: readinessUpstream,
		StatePath:         statePath,
	})

	log.Printf("Starting epoche-proxy (frozen=%v)", p.Frozen())
	log.Printf("  manage: :%d  probes: :%d", managePort, probePort)
	log.Printf("  liveness upstream:  %s", livenessUpstream)
	log.Printf("  readiness upstream: %s", readinessUpstream)

	errs := make(chan error, 2)
	go func() { errs <- http.ListenAndServe(fmt.Sprintf(":%d", managePort), p.ManageHandler()) }()
	go func() { errs <- http.ListenAndServe(fmt.Sprintf(":%d", probePort), p.ProbeHandler()) }()

	log.Fatal(<-errs)
}
