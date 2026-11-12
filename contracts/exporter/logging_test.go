package exporter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLogging pins docs/reference/bbb-exporter.md §8.2: structured JSON on stdout;
// at INFO no meeting_id, user ID, file name, meta value, name, token or
// transcript phrase (fixtures.md §8: dialogue phrases are canaries).
func TestLogging(t *testing.T) {
	t.Run("CT-EXP-08", func(t *testing.T) {
		e := newExporterEnv(t, nil)
		e.vars["RECAP_EXPORTER_LOG_LEVEL"] = "info"
		recordID, _ := e.addRecording("F-01")
		e.spool(recordID)
		d := e.start()
		e.waitFinal(d, recordID, "acked", "")
		d.stop()

		stdout := d.stdout.String()
		if strings.TrimSpace(stdout) == "" {
			t.Fatalf("no log output on stdout")
		}
		for i, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Errorf("stdout line %d is not a JSON object: %q", i+1, line)
			}
		}

		canaries := []string{
			// meeting_id and meeting name
			"fixture-meeting-01", "Fixture Widget Meeting",
			// BBB user IDs, external user IDs are too short to search for
			"w_fixada0001", "w_fixben0002",
			// file names in the package
			"events.xml", "microphone-", ".webm", "notes.html", "slide1.svg",
			"d855a7c389c84fc7e1f4fb9859a38068ef109a90-1761732001000",
			// meta values and names
			"meeting-summary", "docs-main", "PILOS", "Ada Testperson", "Ben Testperson",
			// chat and transcript phrases
			"Link to the widget spec", "Welcome to the fixture meeting", "I propose blue",
			"Agreed, blue it is", "update the specification by Friday", "review it on Monday",
			// the credential
			e.token,
		}
		all := d.output()
		for _, c := range canaries {
			if strings.Contains(all, c) {
				t.Errorf("log output at INFO contains %q", c)
			}
		}
	})
}
