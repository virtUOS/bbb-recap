package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/queue"
)

// A bad provider profile stops check-config for the worker role with exit 2
// and names the key (providers.md §4.1).
func TestCheckConfigWorkerBadProfile(t *testing.T) {
	env := append(without(validEnv(), "RECAP_PROVIDERS_FILE"),
		"RECAP_PROVIDERS_FILE="+cfgData+"providers-bad-profile.yaml")
	var stdout, stderr bytes.Buffer
	if code := checkConfig([]string{"--role", "worker"}, env, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit %d, want %d\nstderr: %s", code, exitUsage, stderr.String())
	}
	for _, want := range []string{"profiles[0].declaration.retention: required", "profiles[1].temperature: unknown key"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr.String())
		}
	}
	// The api role does not parse the providers file.
	stderr.Reset()
	if code := checkConfig([]string{"--role", "api"}, env, &stdout, &stderr); code != exitOK {
		t.Errorf("api role: exit %d\n%s", code, stderr.String())
	}
}

// Every step has a hook.
func TestStepHooks(t *testing.T) {
	for _, step := range queue.Steps {
		if _, ok := stepHandlers[step]; !ok {
			t.Errorf("no hook for step %s", step)
		}
	}
}
