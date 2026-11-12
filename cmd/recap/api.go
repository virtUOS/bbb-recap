package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
)

// publicAddr is the public listener (docs/reference/README.md "HTTP": fixed port).
const publicAddr = ":8080"

func runAPI(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "recap api: unexpected argument %q\n", args[0])
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return api(ctx, os.Environ(), stderr)
}

// api serves the public API until ctx ends, then stops accepting and lets
// in-flight requests finish (architecture.md §9). The routes come from the
// mount functions in the routes_*.go files.
func api(ctx context.Context, environ []string, stderr io.Writer) int {
	d, code := openDeps(ctx, config.RoleAPI, environ, stderr)
	if d == nil {
		return code
	}
	defer d.close()

	mux := http.NewServeMux()
	for _, mount := range []func(*http.ServeMux, *deps) error{mountIngest, mountRegistration, mountAdmin, mountAdminUI} {
		if err := mount(mux, d); err != nil {
			d.log.Error("cannot set up routes", "err", err)
			return exitFailure
		}
	}
	d.log.Info("api listening", "addr", publicAddr)
	err := httpapi.Run(ctx, publicAddr, httpapi.LogRequests(d.log, mux))
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// Requests still running after the drain were cut off; the stop
		// itself is still clean (architecture.md §9: exit 0).
		d.log.Warn("api stopped with requests still running")
		return exitOK
	case err != nil:
		d.log.Error("api server failed", "err", err)
		return exitFailure
	}
	d.log.Info("api stopped")
	return exitOK
}
