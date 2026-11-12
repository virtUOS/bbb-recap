package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/virtUOS/bbb-recap/internal/ops"
)

// healthcheckTimeout bounds the probe; container healthchecks run it often.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck probes the local readiness endpoint and exits 0 on 200,
// else 1 (deployment.md §1). It loads no configuration.
func runHealthcheck(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "recap healthcheck: unexpected argument %q\n", args[0])
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if err := ops.Probe(ctx, ops.ReadyURL); err != nil {
		fmt.Fprintf(stderr, "recap healthcheck: not ready: %v\n", err)
		return exitFailure
	}
	return exitOK
}
