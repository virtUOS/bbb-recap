package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// listenPort returns ":<port>" of a test server, the way compose passes -listen.
func listenPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return ":" + port
}

func TestHealthcheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(healthy.Close)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(failing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedListen := listenPort(t, closed)
	closed.Close()

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"healthy", []string{"-listen", listenPort(t, healthy), "-healthcheck"}, 0},
		{"healthy with host", []string{"-listen", "0.0.0.0" + listenPort(t, healthy), "-healthcheck"}, 0},
		{"not 200", []string{"-listen", listenPort(t, failing), "-healthcheck"}, 1},
		{"nothing listening", []string{"-listen", closedListen, "-healthcheck"}, 1},
		{"no port", []string{"-listen", "localhost", "-healthcheck"}, 2},
		{"extra argument", []string{"-listen", listenPort(t, healthy), "-healthcheck", "x"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(tc.args, &stderr); got != tc.want {
				t.Errorf("run(%q) = %d, want %d; stderr: %s", tc.args, got, tc.want, stderr.String())
			}
			if tc.want == 0 && stderr.Len() > 0 {
				t.Errorf("healthy probe wrote to stderr: %s", stderr.String())
			}
			if tc.want == 1 && !strings.Contains(stderr.String(), "not healthy") {
				t.Errorf("stderr = %q, want a reason", stderr.String())
			}
		})
	}
}
