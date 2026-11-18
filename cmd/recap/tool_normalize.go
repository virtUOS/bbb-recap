package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
	"github.com/virtUOS/bbb-recap/internal/steps/validate"
)

// toolNormalize is `recap tool normalize [--slide-images IMAGES] DIR`
// (contracts/transcript/README.md): the normalized meeting of a raw
// recording or package directory on stdout. The files are selected by the
// allow-list (meeting-package.md §5). With --slide-images it normalizes as
// for a meeting type with slide images on (templates.md §3.2) and writes
// each distinct image as IMAGES/<sha256 hex>.png (an existing, empty
// directory). It needs no configuration; the log goes to stderr at
// RECAP_LOG_LEVEL (default info).
func toolNormalize(args []string, stdout, stderr io.Writer) int {
	const prog = "recap tool normalize"
	images := ""
	if len(args) == 3 && args[0] == "--slide-images" && args[1] != "" {
		images, args = args[1], args[2:]
	}
	if len(args) != 1 || len(args[0]) == 0 || args[0][0] == '-' {
		fmt.Fprintf(stderr, "Usage: %s [--slide-images IMAGES] DIR\n", prog)
		return exitUsage
	}
	if images != "" {
		entries, err := os.ReadDir(images)
		if err != nil || len(entries) > 0 {
			fmt.Fprintf(stderr, "%s: --slide-images: want an existing, empty directory\n", prog)
			return exitUsage
		}
	}
	level := slog.LevelInfo
	if v := os.Getenv("RECAP_LOG_LEVEL"); v != "" {
		var err error
		if level, err = logging.ParseLevel(v); err != nil {
			fmt.Fprintf(stderr, "%s: RECAP_LOG_LEVEL: %v\n", prog, err)
			return exitUsage
		}
	}
	log := logging.New(stderr, level)

	out, err := normalizeDir(args[0], images, log)
	if err != nil {
		log.Error("normalize failed", "err", errorCode(err))
		fmt.Fprintf(stderr, "%s: %s\n", prog, errorCode(err))
		return exitFailure
	}
	_, _ = stdout.Write(out)
	return exitOK
}

func normalizeDir(dir, images string, log *slog.Logger) ([]byte, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sel, err := manifest.Select(abs)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var img *normalize.Images
	if images != "" {
		img = &normalize.Images{Store: func(key string, png []byte) error {
			return os.WriteFile(filepath.Join(images, key+".png"), png, 0o644)
		}}
	}
	m, err := normalize.Normalize(validate.OnlyFiles(root.FS(), sel.Files), sel.Files, img)
	if err != nil {
		return nil, err
	}
	if img != nil {
		log.Debug("slide images", "render_attempts", img.Attempts, "images", img.Stored)
	}
	gaps := 0
	for _, t := range m.Tracks {
		if t.Gap != "" {
			gaps++
		}
	}
	log.Debug("normalized", "tracks", len(m.Tracks), "gaps", gaps, "periods", len(m.RecordingPeriods),
		"participants", len(m.Participants), "chat_messages", len(m.Chat), "slides", len(m.Slides),
		"skipped_files", sel.Skipped)
	return meeting.Marshal(m)
}

// errorCode reduces an error to a message without file paths (they contain
// BBB user IDs) or events.xml content.
func errorCode(err error) string {
	switch {
	case errors.Is(err, normalize.ErrEvents):
		return "events.xml is malformed or has no recording start"
	case errors.Is(err, os.ErrNotExist):
		return "directory or file not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}
	return "cannot read the package"
}
