package exporter

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestConfig pins config validation (docs/reference/bbb-exporter.md §8.1, keys in
// deployment.md §5.4): the daemon and `check-config` validate the whole config
// and exit non-zero listing all errors.
func TestConfig(t *testing.T) {
	t.Run("CT-EXP-07", func(t *testing.T) {
		t.Run("valid config", func(t *testing.T) {
			e := newExporterEnv(t, nil)
			code, out := e.runCommand(t, "check-config")
			if code != 0 {
				t.Fatalf("check-config on a valid config: exit %d\n%s", code, out)
			}
		})
		cases := []struct {
			name  string
			edit  func(vars map[string]string)
			names []string // every one must appear in the output
		}{
			{"missing service URL", func(v map[string]string) { delete(v, "RECAP_EXPORTER_SERVICE_URL") },
				[]string{"RECAP_EXPORTER_SERVICE_URL"}},
			{"missing server ID", func(v map[string]string) { delete(v, "RECAP_EXPORTER_BBB_SERVER_ID") },
				[]string{"RECAP_EXPORTER_BBB_SERVER_ID"}},
			{"unknown key", func(v map[string]string) { v["RECAP_EXPORTER_NO_SUCH_KEY"] = "1" },
				[]string{"RECAP_EXPORTER_NO_SUCH_KEY"}},
			{"http service URL", func(v map[string]string) { v["RECAP_EXPORTER_SERVICE_URL"] = "http://127.0.0.1:1" },
				[]string{"RECAP_EXPORTER_SERVICE_URL"}},
			{"all errors listed", func(v map[string]string) {
				delete(v, "RECAP_EXPORTER_BBB_SERVER_ID")
				v["RECAP_EXPORTER_SERVICE_URL"] = "http://127.0.0.1:1"
				v["RECAP_EXPORTER_NO_SUCH_KEY"] = "1"
			}, []string{"RECAP_EXPORTER_BBB_SERVER_ID", "RECAP_EXPORTER_SERVICE_URL", "RECAP_EXPORTER_NO_SUCH_KEY"}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				e := newExporterEnv(t, nil)
				c.edit(e.vars)
				for _, cmd := range []string{"check-config", "run"} {
					code, out := e.runCommand(t, cmd)
					if code == 0 {
						t.Errorf("%s: exit 0 on an invalid config\n%s", cmd, out)
					}
					for _, n := range c.names {
						if !strings.Contains(out, n) {
							t.Errorf("%s: output does not name %s\n%s", cmd, n, out)
						}
					}
				}
			})
		}
	})
}

// runCommand runs one exporter command to completion (at most 15 s) and
// returns its exit code and combined output.
func (e *exporterEnv) runCommand(t *testing.T, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s %v did not exit within 15 s\n%s", e.bin, args, out)
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("%s %v: %v", e.bin, args, err)
		return -1, ""
	}
}
