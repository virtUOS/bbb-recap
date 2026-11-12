package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "INFO", "trace"} {
		if _, err := ParseLevel(in); err == nil {
			t.Errorf("ParseLevel(%q): want error", in)
		}
	}
}

func TestNewWritesJSONWithKeys(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	log.Debug("hidden")
	log.Info("hello", "job_id", "x")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("want exactly one JSON line, got %q: %v", buf.String(), err)
	}
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := line[k]; !ok {
			t.Errorf("key %q missing in %v", k, line)
		}
	}
	if line["level"] != "INFO" || line["msg"] != "hello" {
		t.Errorf("got %v", line)
	}
}
