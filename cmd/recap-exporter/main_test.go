package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the tests run this test binary as the real command: with
// RECAP_TEST_EXPORTER_MAIN=1 it behaves like main.
func TestMain(m *testing.M) {
	if os.Getenv("RECAP_TEST_EXPORTER_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

var want = []string{"run", "status", "requeue", "delete-raw", "check-config", "healthcheck"}

func TestHelpListsExactlyTheCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--help"}, nil, &out, &errOut); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	var got []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "  ") {
			got = append(got, strings.Fields(line)[0])
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("help lists %v, want %v", got, want)
	}
	for _, name := range want {
		out.Reset()
		if code := run([]string{name, "--help"}, nil, &out, &errOut); code != exitOK || !strings.HasPrefix(out.String(), "Usage: recap-exporter "+name+" - ") {
			t.Errorf("%s --help: exit code = %d, stdout = %q", name, code, out.String())
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"run", "x"}, {"requeue"}, {"delete-raw"}, {"status", "a", "b"}, {"healthcheck", "x"}} {
		var out, errOut bytes.Buffer
		if code := run(args, nil, &out, &errOut); code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitUsage)
		}
	}
}

// env is a valid configuration in a temp dir; the service is never contacted.
func env(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"spool", "state", "raw"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("FIXTURE-NOT-A-SECRET-x\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return []string{
		"RECAP_EXPORTER_ENV_FILE=" + filepath.Join(dir, "missing.env"),
		"RECAP_EXPORTER_SERVICE_URL=https://ingest.recap.test",
		"RECAP_EXPORTER_BBB_SERVER_ID=bbb-fixture-a",
		"RECAP_EXPORTER_TOKEN_FILE=" + filepath.Join(dir, "token"),
		"RECAP_EXPORTER_RAW_DIR=" + filepath.Join(dir, "raw"),
		"RECAP_EXPORTER_SPOOL_DIR=" + filepath.Join(dir, "spool"),
		"RECAP_EXPORTER_STATE_DIR=" + filepath.Join(dir, "state"),
		"RECAP_EXPORTER_LISTEN_ADDR=" + addr,
	}
}

func TestCheckConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"check-config"}, env(t), &out, &errOut); code != exitOK {
		t.Fatalf("valid config: exit %d\n%s", code, errOut.String())
	}
	errOut.Reset()
	bad := append(env(t), "RECAP_EXPORTER_SERVICE_URL=http://x", "RECAP_EXPORTER_TOKEN_FILE=/nonexistent/token")
	if code := run([]string{"check-config"}, bad, &out, &errOut); code != exitUsage {
		t.Fatalf("invalid config: exit %d", code)
	}
	for _, k := range []string{"RECAP_EXPORTER_SERVICE_URL", "RECAP_EXPORTER_TOKEN_FILE"} {
		if !strings.Contains(errOut.String(), k) {
			t.Errorf("output does not name %s:\n%s", k, errOut.String())
		}
	}
	if strings.Contains(out.String()+errOut.String(), "FIXTURE-NOT-A-SECRET") {
		t.Error("token in the output")
	}
}

func TestStatusEmpty(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"status"}, env(t), &out, &errOut); code != exitOK || !strings.Contains(out.String(), `"records": []`) {
		t.Fatalf("status: exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if code := run([]string{"status", "no-such-record"}, env(t), &out, &errOut); code != exitFailure {
		t.Errorf("status of an unknown record: exit %d", code)
	}
}

// start runs this test binary as `recap-exporter run`.
func start(t *testing.T, environ []string) (*exec.Cmd, *bytes.Buffer, chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "run")
	cmd.Env = append(environ, "RECAP_TEST_EXPORTER_MAIN=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		// Wait for the process; after the kill an error is expected.
		if err := <-done; err != nil {
			t.Logf("daemon ended: %v", err)
		}
	})
	return cmd, &out, done
}

func TestSecondRunExitsAndSIGTERM(t *testing.T) {
	e := env(t)
	first, out, done := start(t, e)
	time.Sleep(time.Second) // the first run holds the lock now

	// A second daemon on the same state dir exits non-zero at once.
	second := exec.Command(os.Args[0], "run")
	second.Env = append(slices.Clone(e), "RECAP_TEST_EXPORTER_MAIN=1",
		"RECAP_EXPORTER_LISTEN_ADDR=127.0.0.1:0")
	b, err := second.CombinedOutput()
	if err == nil || second.ProcessState.ExitCode() != exitFailure {
		t.Errorf("second run: %v (exit %d)\n%s", err, second.ProcessState.ExitCode(), b)
	}

	// SIGTERM: exit 0 within 20 s.
	start := time.Now()
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err // for the cleanup
		if err != nil {
			t.Errorf("exit after SIGTERM: %v\n%s", err, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("no exit within 20 s of SIGTERM\n%s", out.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("SIGTERM took %v", d)
	}
}

func TestHealthcheck(t *testing.T) {
	e := env(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"healthcheck"}, e, &out, &errOut); code != exitFailure {
		t.Errorf("no daemon: exit %d, want %d", code, exitFailure)
	}
	_, log, _ := start(t, e)
	deadline := time.Now().Add(10 * time.Second)
	for {
		errOut.Reset()
		code := run([]string{"healthcheck"}, e, &out, &errOut)
		if code == exitOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon not ready: exit %d: %s\n%s", code, errOut.String(), log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A wildcard listen address is probed through loopback.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(e[len(e)-1], "RECAP_EXPORTER_LISTEN_ADDR="))
	wild := append(slices.Clone(e), "RECAP_EXPORTER_LISTEN_ADDR=0.0.0.0:"+port)
	if code := run([]string{"healthcheck"}, wild, &out, &errOut); code != exitOK {
		t.Errorf("wildcard listen address: exit %d: %s", code, errOut.String())
	}
}
