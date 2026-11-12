// Command recap is the service binary: api, worker, migrate, check-config,
// healthcheck and the dev/test-only tool sub-commands (docs/reference/README.md
// "Configuration, processes, metrics"). Each entry of the dispatch tables
// calls a function in its own file, so each task owns one file.
package main

import (
	"fmt"
	"io"
	"os"
	_ "time/tzdata" // RECAP_DISPLAY_TZ on a scratch image (deployment.md §1)
)

// Exit codes (docs/reference/deployment.md §1).
const (
	exitOK      = 0
	exitFailure = 1 // runtime failure
	exitUsage   = 2 // invalid config or usage error
	exitMigrate = 3 // migration failure or incompatible schema
)

// handler runs a command with the arguments after its name.
type handler func(args []string, stdout, stderr io.Writer) int

type command struct {
	name, help string
	run        handler
}

// commands drives both the help output and the dispatch in run.
var commands = []command{
	{"api", "serve the public HTTP API (:8080) and ops listener (:9090)", runAPI},
	{"worker", "run pipeline steps from the job queue", runWorker},
	{"migrate", "apply database migrations", runMigrate},
	{"check-config", checkConfigHelp, runCheckConfig},
	{"healthcheck", "probe the local readiness endpoint, exit 0 or 1", runHealthcheck},
	{"tool", "dev/test tools for contract tests (JSON in, JSON out)", nil},
}

// tools drives both the tool help output and the dispatch in runTool.
var tools = []command{
	{"validate-package", "validate a meeting package", toolValidatePackage},
	{"normalize", "normalize a meeting package", toolNormalize},
	{"assemble", "assemble a transcript", toolAssemble},
	{"asr", "run an ASR provider adapter", toolASR},
	{"llm", "run an LLM provider adapter", toolLLM},
	{"render", "render output Markdown", toolRender},
	{"sink", "deliver to a sink", toolSink},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr, "recap <command> [arguments]", "Commands", commands)
		return exitUsage
	}
	if isHelp(args[0]) {
		usage(stdout, "recap <command> [arguments]", "Commands", commands)
		return exitOK
	}
	c, ok := find(commands, args[0])
	if !ok {
		fmt.Fprintf(stderr, "recap: unknown command %q\n", args[0])
		usage(stderr, "recap <command> [arguments]", "Commands", commands)
		return exitUsage
	}
	if c.name == "tool" {
		return runTool(args[1:], stdout, stderr)
	}
	return dispatch("recap "+c.name, c, args[1:], stdout, stderr)
}

func runTool(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr, "recap tool <name> [arguments]", "Tools", tools)
		return exitUsage
	}
	if isHelp(args[0]) {
		usage(stdout, "recap tool <name> [arguments]", "Tools", tools)
		return exitOK
	}
	t, ok := find(tools, args[0])
	if !ok {
		fmt.Fprintf(stderr, "recap tool: unknown tool %q\n", args[0])
		usage(stderr, "recap tool <name> [arguments]", "Tools", tools)
		return exitUsage
	}
	return dispatch("recap tool "+t.name, t, args[1:], stdout, stderr)
}

// dispatch prints a one-line usage for --help, otherwise runs the command.
func dispatch(prog string, c command, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && isHelp(args[0]) {
		fmt.Fprintf(stdout, "Usage: %s - %s\n", prog, c.help)
		return exitOK
	}
	return c.run(args, stdout, stderr)
}

func find(list []command, name string) (command, bool) {
	for _, c := range list {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func usage(w io.Writer, synopsis, heading string, list []command) {
	fmt.Fprintf(w, "Usage: %s\n\n%s:\n", synopsis, heading)
	for _, c := range list {
		fmt.Fprintf(w, "  %-18s %s\n", c.name, c.help)
	}
}
