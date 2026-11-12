// Command recap-mock serves mock ASR, LLM and Docs (create-for-owner)
// endpoints, and optionally a mock OIDC provider for the admin UI, for dev
// and tests (docs/reference/deployment.md §1). Never used in
// production. It is configured by flags only (no RECAP_* variables) and
// answers from the fixture tree (testdata/). It logs IDs and lengths only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/ops"
)

// healthcheckTimeout bounds the -healthcheck probe; container healthchecks
// run it often.
const healthcheckTimeout = 3 * time.Second

// config is what the mount functions get.
type config struct {
	Fixtures    string // root of the fixture tree (testdata/)
	ASRKeyFile  string // optional: require Bearer <key> on the mock ASR
	LLMKeyFile  string // optional: require Bearer <key> on the mock LLM
	DocsKeyFile string // optional: require the API key on the mock Docs

	OIDCIssuer           string // mock OIDC provider: on when set
	OIDCBrowserURL       string // issuer URL as the browser sees it (authorization endpoint)
	OIDCClientID         string
	OIDCClientSecretFile string
	OIDCRedirectURI      string // the only redirect URI the client may use
	OIDCSubject          string
	OIDCRoles            string // comma-separated; empty: no role claim

	Log *slog.Logger
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("recap-mock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: recap-mock -fixtures DIR [flags]\n\n"+
			"Serves mock ASR, LLM, Docs and (with -oidc-issuer) OIDC endpoints for dev and tests. Never use in production.")
		fs.PrintDefaults()
	}
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	var c config
	fs.StringVar(&c.Fixtures, "fixtures", "", "fixture tree (testdata/), required")
	fs.StringVar(&c.ASRKeyFile, "asr-key", "", "file with the bearer token the mock ASR requires (optional)")
	fs.StringVar(&c.LLMKeyFile, "llm-key", "", "file with the bearer token the mock LLM requires (optional)")
	fs.StringVar(&c.DocsKeyFile, "docs-key", "", "file with the key the mock Docs requires (optional)")
	fs.StringVar(&c.OIDCIssuer, "oidc-issuer", "", "serve a mock OIDC provider with this issuer URL, as the relying party sees it (optional)")
	fs.StringVar(&c.OIDCBrowserURL, "oidc-browser-url", "", "issuer URL as the browser sees it, for the authorization endpoint (default: -oidc-issuer)")
	fs.StringVar(&c.OIDCClientID, "oidc-client-id", "recap-admin-ui", "client ID of the admin UI")
	fs.StringVar(&c.OIDCClientSecretFile, "oidc-client-secret", "", "file with the client secret (required with -oidc-issuer)")
	fs.StringVar(&c.OIDCRedirectURI, "oidc-redirect-uri", "", "the only redirect URI the client may use (required with -oidc-issuer)")
	fs.StringVar(&c.OIDCSubject, "oidc-sub", "fixture-sub-admin-0010", "subject of the fixture admin every login approves (docs/reference/fixtures.md)")
	fs.StringVar(&c.OIDCRoles, "oidc-roles", "recap-admin", "comma-separated roles in the claim \"roles\"; empty leaves the claim out")
	levelName := fs.String("log-level", "info", "debug, info, warn or error")
	health := fs.Bool("healthcheck", false, "probe http://127.0.0.1:<port of -listen>/healthz, exit 0 or 1, and do not serve")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *health {
		if fs.NArg() > 0 {
			fs.Usage()
			return 2
		}
		return healthcheck(*listen, stderr)
	}
	level, err := logging.ParseLevel(*levelName)
	if err != nil || c.Fixtures == "" || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	c.Log = logging.New(stderr, level)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for name, mount := range map[string]func(*http.ServeMux, config) error{
		"asr": mountASR, "llm": mountLLM, "docs": mountDocs, "oidc": mountOIDC,
	} {
		if err := mount(mux, c); err != nil {
			c.Log.Error("cannot set up mock", "mock", name, "err", err)
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	c.Log.Info("recap-mock listening", "addr", *listen)
	select {
	case err := <-errc:
		c.Log.Error("recap-mock stopped", "err", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	c.Log.Info("recap-mock stopped")
	return 0
}

// healthcheck GETs /healthz on the local port of the listen address, so the
// scratch image can be probed without a shell or curl. It exits 0 on 200,
// 1 otherwise, and 2 when the listen address has no port.
func healthcheck(listen string, stderr io.Writer) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		fmt.Fprintf(stderr, "recap-mock -healthcheck: -listen %q has no port\n", listen)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if err := ops.Probe(ctx, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz"); err != nil {
		fmt.Fprintf(stderr, "recap-mock -healthcheck: not healthy: %v\n", err)
		return 1
	}
	return 0
}

// readKey reads a bearer token file, trimmed of one trailing newline. An
// empty path gives "".
func readKey(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}
