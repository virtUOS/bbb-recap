package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// listed returns the first word of every indented line in a help text.
func listed(help string) []string {
	var names []string
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "  ") {
			names = append(names, strings.Fields(line)[0])
		}
	}
	return names
}

func TestHelpListsExactlyTheCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--help"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	want := []string{"api", "worker", "migrate", "check-config", "healthcheck", "tool"}
	if got := listed(out.String()); !slices.Equal(got, want) {
		t.Fatalf("help lists %v, want %v", got, want)
	}
}

func TestToolHelpListsExactlyTheTools(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"tool", "--help"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	want := []string{"validate-package", "normalize", "assemble", "asr", "llm", "render", "sink"}
	if got := listed(out.String()); !slices.Equal(got, want) {
		t.Fatalf("tool help lists %v, want %v", got, want)
	}
}

// stubs are the commands whose tasks have not landed yet.
var stubs = [][]string{}

// allCommands answer --help with one usage line, implemented or not.
var allCommands = append([][]string{{"api"}, {"worker"}, {"migrate"}, {"check-config"}, {"healthcheck"},
	{"tool", "validate-package"}, {"tool", "normalize"}, {"tool", "assemble"}, {"tool", "asr"},
	{"tool", "llm"}, {"tool", "render"}, {"tool", "sink"}}, stubs...)

func TestStubsNotImplemented(t *testing.T) {
	for _, args := range stubs {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != exitFailure {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitFailure)
		}
		if !strings.Contains(errOut.String(), "not implemented") {
			t.Errorf("%v: stderr = %q, want \"not implemented\"", args, errOut.String())
		}
	}
}

func TestCommandHelp(t *testing.T) {
	for _, args := range allCommands {
		var out, errOut bytes.Buffer
		args = append(slices.Clone(args), "--help")
		if code := run(args, &out, &errOut); code != exitOK {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitOK)
		}
		want := "Usage: recap " + strings.Join(args[:len(args)-1], " ") + " - "
		if !strings.HasPrefix(out.String(), want) || strings.Count(out.String(), "\n") != 1 {
			t.Errorf("%v: stdout = %q, want one line starting with %q", args, out.String(), want)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"tool"}, {"tool", "nope"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitUsage)
		}
	}
}
