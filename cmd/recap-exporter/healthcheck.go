package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/virtUOS/bbb-recap/internal/exporter"
)

// healthcheckTimeout bounds the probe; container healthchecks run it often.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck probes /readyz of the running daemon, so the scratch image
// needs no shell or curl (docs/reference/deployment.md §1). It reads the same
// configuration as the daemon but uses only RECAP_EXPORTER_LISTEN_ADDR: the
// daemon is ready only with a valid config anyway. Exit 0 when ready, else 1
// (container runtimes treat any non-zero code as unhealthy).
func runHealthcheck(environ []string, stderr io.Writer) int {
	cfg, _ := exporter.LoadConfig(environ)
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "" {
		fmt.Fprintf(stderr, "recap-exporter healthcheck: listen address %q has no port\n", cfg.ListenAddr)
		return exitFailure
	}
	// A wildcard listen address is reached through loopback.
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/readyz"

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if err := probe(ctx, url); err != nil {
		fmt.Fprintf(stderr, "recap-exporter healthcheck: not ready: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// probe GETs url without any proxy from the environment and returns nil on 200.
func probe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
