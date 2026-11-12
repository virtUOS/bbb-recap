package exporter

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// ExporterName is exporter.name in the manifest (meeting-package.md §4).
const ExporterName = "recap-exporter"

// Version returns the module version of the binary, for exporter.version
// and recap_exporter_build_info.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// claim moves every spool entry <SPOOL_DIR>/<record_id>.json to
// <STATE_DIR>/<record_id>/spool.json (§4.1 step 1). An entry for a record
// that has a state record already is a duplicate and is removed.
func (d *Daemon) claim() {
	entries, err := os.ReadDir(d.cfg.SpoolDir)
	if err != nil {
		d.log.Error("spool dir cannot be read")
		d.log.Debug("spool dir", "err", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || strings.HasPrefix(name, ".") || !e.Type().IsRegular() {
			continue // temp files of the hook, anything else
		}
		if !ValidRecordID(id) {
			d.log.Warn("spool entry with an invalid name skipped")
			continue
		}
		src := filepath.Join(d.cfg.SpoolDir, name)
		dir := d.store.recordDir(id)
		if _, err := os.Lstat(dir); err == nil {
			if err := os.Remove(src); err != nil {
				d.log.Error("duplicate spool entry cannot be removed", "record_id", id)
				continue
			}
			d.log.Info("duplicate spool entry removed", "record_id", id)
			continue
		}
		if err := os.Mkdir(dir, 0o750); err != nil {
			d.log.Error("state record cannot be created", "record_id", id)
			d.log.Debug("mkdir", "record_id", id, "err", err)
			continue
		}
		if err := os.Rename(src, filepath.Join(dir, spoolFile)); err != nil {
			d.log.Error("spool entry cannot be claimed", "record_id", id)
			d.log.Debug("rename", "record_id", id, "err", err)
			_ = os.Remove(dir)
			continue
		}
		_ = syncDir(dir)
		_ = syncDir(d.cfg.SpoolDir)
		_ = syncDir(d.store.Dir)
		d.log.Info("spool entry claimed", "record_id", id)
	}
}

// eventsHeader is what the build reads from events.xml: the attributes of
// <recording>, <meeting> and <metadata> (meeting-package.md §4;
// docs/reference/upstream-facts.md). Reading stops at the first <event>.
type eventsHeader struct {
	BBBVersion string
	MeetingID  string // <meeting externalId>
	Breakout   bool   // <meeting breakout="true">
	Metadata   map[string]string
}

func readEventsHeader(r io.Reader) (*eventsHeader, error) {
	h := &eventsHeader{}
	dec := xml.NewDecoder(r)
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			name := t.Name.Local
			switch {
			case depth == 1 && name != "recording":
				return nil, errors.New("root element is not recording")
			case depth == 1:
				h.BBBVersion = attr(t, "bbb_version")
			case depth == 2 && name == "meeting":
				h.MeetingID = attr(t, "externalId")
				h.Breakout = attr(t, "breakout") == "true"
			case depth == 2 && name == "metadata":
				h.Metadata = map[string]string{}
				for _, a := range t.Attr {
					h.Metadata[a.Name.Local] = a.Value
				}
			case depth == 2 && name == "event":
				return h, nil
			}
		case xml.EndElement:
			depth--
		}
	}
	if depth != 0 {
		return nil, errors.New("unexpected end of document")
	}
	return h, nil
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// buildResult is the outcome of a build: a manifest, a failure reason, or
// drop (no longer opted in, or a breakout).
type buildResult struct {
	manifest []byte
	reason   string
	drop     bool
}

// build applies §4.1 step 2 to raw/<id>/: V-E01, the opt-in and breakout
// re-check, the allow-list, V-E02, V-E03, then the manifest. An error is an
// I/O problem worth retrying in the next loop.
func (d *Daemon) build(id string) (buildResult, error) {
	dir := filepath.Join(d.cfg.RawDir, id)

	// V-E01: events.xml exists and is a regular file. An unreadable or
	// malformed events.xml fails the same way: the package has no usable
	// events.
	fi, err := os.Lstat(filepath.Join(dir, "events.xml"))
	if err != nil || !fi.Mode().IsRegular() {
		return buildResult{reason: ReasonNoEvents}, nil
	}
	f, err := os.Open(filepath.Join(dir, "events.xml"))
	if err != nil {
		return buildResult{reason: ReasonNoEvents}, nil
	}
	h, err := readEventsHeader(f)
	_ = f.Close()
	if err != nil {
		d.log.Debug("events.xml cannot be parsed", "record_id", id, "err", err)
		return buildResult{reason: ReasonNoEvents}, nil
	}

	// Opt-in and breakout re-check (§4.1 step 2; README "Grammars").
	if h.Metadata[d.cfg.MetaPrefix+"-enabled"] != "true" || h.Breakout {
		return buildResult{drop: true}, nil
	}

	sel, err := manifest.Select(dir)
	if err != nil {
		return buildResult{}, err
	}
	d.log.Debug("files selected", "record_id", id, "meeting_id", h.MeetingID,
		"files", len(sel.Files), "skipped", sel.Skipped)

	// V-E02 (meeting-package.md §6).
	mics := 0
	for _, f := range sel.Files {
		if f.Role == manifest.RoleMicrophone {
			mics++
		}
	}
	if mics == 0 {
		audio, _ := os.ReadDir(filepath.Join(dir, "audio"))
		if len(audio) > 0 {
			return buildResult{reason: ReasonAudioBridge}, nil
		}
		return buildResult{reason: ReasonNoMicrophone}, nil
	}

	// V-E03 (meeting-package.md §8).
	if v := manifest.CheckLimits(sel.Files); v != nil {
		return buildResult{reason: ReasonLimitExceeded}, nil
	}

	meta := map[string]string{}
	for k, v := range h.Metadata {
		if strings.HasPrefix(k, d.cfg.MetaPrefix+"-") || k == "bbb-origin" || k == "bbb-origin-server-name" {
			meta[k] = v
		}
	}
	m := &manifest.Manifest{
		SchemaVersion: "1.0.0",
		CreatedAt:     d.now().UTC().Format(time.RFC3339),
		Exporter:      manifest.Exporter{Name: ExporterName, Version: Version()},
		BBBServerID:   d.cfg.BBBServerID,
		RecordID:      id,
		MeetingID:     h.MeetingID,
		BBBVersion:    h.BBBVersion,
		Breakout:      false,
		MetaPrefix:    d.cfg.MetaPrefix,
		Metadata:      meta,
		Files:         sel.Files,
	}
	m.Totals.FileCount = len(sel.Files)
	for _, f := range sel.Files {
		m.Totals.Size += f.Size
	}
	b, err := manifest.Marshal(m)
	if err != nil {
		return buildResult{}, err
	}
	if len(b) > manifest.MaxManifestSize {
		return buildResult{reason: ReasonLimitExceeded}, nil
	}
	return buildResult{manifest: b}, nil
}

// buildRecord turns a claimed record (spool.json, no state.json) into ready
// or failed, or drops it. A manifest.json that exists already is kept
// (crash between writing it and state.json): it is never rebuilt.
func (d *Daemon) buildRecord(id string) {
	now := d.now()
	rec := &Record{RecordID: id, History: []Entry{}}

	b, err := d.store.Manifest(id)
	if err != nil {
		d.log.Error("manifest cannot be read", "record_id", id)
		return
	}
	if b == nil {
		res, err := d.build(id)
		if err != nil {
			d.log.Error("build failed, retrying in the next loop", "record_id", id)
			d.log.Debug("build", "record_id", id, "err", err)
			return
		}
		if res.drop {
			if err := d.store.removeRecord(id); err != nil {
				d.log.Error("state record cannot be removed", "record_id", id)
				return
			}
			d.log.Info("recording no longer opted in or a breakout, dropped", "record_id", id)
			return
		}
		if res.reason != "" {
			rec.setState(StateFailed, res.reason, now)
			if err := d.store.Save(rec); err != nil {
				d.log.Error("state cannot be saved", "record_id", id)
				return
			}
			d.log.Warn("build failed", "record_id", id, "state", StateFailed, "reason", res.reason)
			return
		}
		b = res.manifest
		if err := writeFileAtomic(filepath.Join(d.store.recordDir(id), manifestFile), b); err != nil {
			d.log.Error("manifest cannot be written", "record_id", id)
			return
		}
	}
	rec.PackageID = manifest.PackageID(b)
	rec.FirstAttemptAt = now.UTC() // §6.5: set when the build succeeds
	rec.setState(StateReady, "", now)
	if err := d.store.Save(rec); err != nil {
		d.log.Error("state cannot be saved", "record_id", id)
		return
	}
	d.log.Info("manifest built", "record_id", id, "package_id", rec.PackageID, "state", StateReady, "size", len(b))
}

// parseManifest decodes the stored manifest.
func parseManifest(b []byte) (*manifest.Manifest, error) {
	var m manifest.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	return &m, nil
}

// fileExists reports whether path exists (without following a symlink).
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
