package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/logging"
)

// checkConfigHelp is the help text of check-config in the command table.
const checkConfigHelp = "validate configuration and exit"

func runCheckConfig(args []string, stdout, stderr io.Writer) int {
	return checkConfig(args, os.Environ(), stdout, stderr)
}

// checkConfig runs the start-up configuration check for one role or, by
// default, both (deployment.md §5). Errors go to stderr; exit 2 on any.
func checkConfig(args, environ []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recap check-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // --help anywhere prints the same line as dispatch, below
	role := fs.String("role", "", "check only this role: api or worker (default: both)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stdout, "Usage: recap check-config - %s\n", checkConfigHelp)
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "recap check-config: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	var roles []config.Role
	switch *role {
	case "":
		roles = []config.Role{config.RoleAPI, config.RoleWorker}
	case "api", "worker":
		roles = []config.Role{config.Role(*role)}
	default:
		fmt.Fprintf(stderr, "recap check-config: --role: want api or worker, got %q\n", *role)
		return exitUsage
	}

	cfg, err := config.Load(environ, roles...)
	if err != nil {
		fmt.Fprintf(stderr, "recap check-config: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logging.New(stderr, cfg.LogLevel)
	cfg.LogBoot(log)
	config.LogSinkWarnings(log, cfg.Sinks)
	if cfg.Providers != nil { // worker role only
		cfg.Providers.LogWarnings(log)
	}
	fmt.Fprintf(stdout, "recap check-config: configuration is valid for %v\n", roles)
	return exitOK
}
