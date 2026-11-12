// Command recap-exporter is the exporter daemon for BBB servers and its admin
// CLI (docs/reference/bbb-exporter.md §8.3).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/virtUOS/bbb-recap/internal/exporter"
	"github.com/virtUOS/bbb-recap/internal/logging"
)

// Exit codes (docs/reference/deployment.md §1).
const (
	exitOK      = 0
	exitFailure = 1 // runtime failure
	exitUsage   = 2 // invalid config or usage error
)

type command struct {
	name, args, help string
	nargs            []int // allowed argument counts
}

// commands drives both the help output and the dispatch in run.
var commands = []command{
	{"run", "", "run the exporter daemon", []int{0}},
	{"status", "[<record_id>]", "show export state as JSON", []int{0, 1}},
	{"requeue", "<record_id>", "requeue a failed record (daemon stopped)", []int{1}},
	{"delete-raw", "<record_id>", "delete the raw recording after the §7 checks (daemon stopped)", []int{1}},
	{"check-config", "", "validate configuration and exit", []int{0}},
	{"healthcheck", "", "probe the running daemon's /readyz, exit 0 or 1", []int{0}},
}

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func run(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	if isHelp(args[0]) {
		usage(stdout)
		return exitOK
	}
	for _, c := range commands {
		if c.name != args[0] {
			continue
		}
		rest := args[1:]
		if len(rest) > 0 && isHelp(rest[0]) {
			fmt.Fprintf(stdout, "Usage: recap-exporter %s - %s\n", c.name, c.help)
			return exitOK
		}
		ok := false
		for _, n := range c.nargs {
			ok = ok || len(rest) == n
		}
		if !ok {
			fmt.Fprintf(stderr, "Usage: recap-exporter %s %s\n", c.name, c.args)
			return exitUsage
		}
		if c.name == "healthcheck" {
			return runHealthcheck(environ, stderr)
		}
		return runCommand(c.name, rest, environ, stdout, stderr)
	}
	fmt.Fprintf(stderr, "recap-exporter: unknown command %q\n", args[0])
	usage(stderr)
	return exitUsage
}

func runCommand(name string, args, environ []string, stdout, stderr io.Writer) int {
	cfg, err := exporter.LoadConfig(environ)
	var token string
	if name == "run" || name == "check-config" {
		// The token file is checked too, so all problems are listed at once.
		var errs exporter.ConfigError
		_ = errors.As(err, &errs)
		tok, terr := cfg.ReadToken()
		if terr != nil {
			errs = append(errs, terr.Error())
		}
		if len(errs) > 0 {
			err = errs
		}
		token = tok
	}
	if err != nil {
		var ce exporter.ConfigError
		if errors.As(err, &ce) {
			for _, e := range ce {
				fmt.Fprintf(stderr, "recap-exporter: config: %s\n", e)
			}
		} else {
			fmt.Fprintf(stderr, "recap-exporter: config: %v\n", err)
		}
		return exitUsage
	}

	// Logs go to stdout as JSON lines (journald, §8.2).
	log := logging.New(stdout, cfg.LogLevel)
	d := exporter.NewDaemon(cfg, token, log)

	switch name {
	case "check-config":
		fmt.Fprintln(stdout, "config OK")
		return exitOK
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := d.Run(ctx); err != nil {
			log.Error("exporter cannot run", "cause", err.Error())
			return exitFailure
		}
		return exitOK
	}

	// Admin commands: plain messages on stderr, JSON only from status.
	var recordID string
	if len(args) > 0 {
		recordID = args[0]
	}
	switch name {
	case "status":
		err = d.Status(stdout, recordID)
	case "requeue":
		err = d.Requeue(recordID)
	case "delete-raw":
		err = d.DeleteRaw(recordID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "recap-exporter %s: %v\n", name, err)
		if errors.Is(err, exporter.ErrUsage) {
			return exitUsage
		}
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: recap-exporter <command> [arguments]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-14s %s\n", c.name, c.help)
	}
}
