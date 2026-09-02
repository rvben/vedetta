package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

// healthcheckTimeout bounds the whole probe. Container runtimes kill a
// healthcheck that overruns their own timeout, so finishing first turns an
// ambiguous kill into a definite failure exit code.
const healthcheckTimeout = 5 * time.Second

// runHealthcheck probes the liveness endpoint of an instance running on this
// host and exits non-zero when it does not answer 200. Container images carry
// no shell HTTP client, so the recorder probes itself: the same binary that
// serves the API is the healthcheck client.
//
// The probe targets /api/health/live because it is the only endpoint that is
// both exempt from authentication and exempt from the readiness gate, so it
// reports whether the process is alive rather than whether it has finished
// initializing.
func runHealthcheck() {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	configPath := fs.String("config", "config.yml", "path to configuration file")
	timeout := fs.Duration("timeout", healthcheckTimeout, "probe timeout")

	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}

	// LoadOrDefault, not Load: a server in setup mode has no config file yet and
	// is still expected to answer. Falling back to the defaults probes the same
	// address that setup mode listens on.
	cfg, _, err := config.LoadOrDefault(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: reading %s: %v\n", *configPath, err)
		os.Exit(1)
	}

	url := healthcheckURL(cfg)
	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			// The probe dials the server's own listener over the loopback or its
			// bound address, so a self-signed certificate is expected and carries
			// no trust decision: nothing about the identity of a socket this
			// process can already see is in question.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback probe of this host's own listener
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %s: %v\n", url, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining for connection reuse; the status decides the result

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		os.Exit(1)
	}

	fmt.Printf("ok %s\n", url)
}

// healthcheckURL builds the liveness URL for the configured listener. It uses
// the configured port rather than a hard-coded one, and dials loopback for a
// wildcard bind because the probe runs on the host that serves the API.
func healthcheckURL(cfg *config.Config) string {
	scheme := "http"
	if cfg.API.TLSCert != "" && cfg.API.TLSKey != "" {
		scheme = "https"
	}

	host := cfg.API.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}

	port := cfg.API.Port
	if port == 0 {
		port = config.Defaults().API.Port
	}

	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/api/health/live"
}
