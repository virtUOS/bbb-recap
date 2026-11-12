package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// mountASR serves the whisperx-family endpoint (docs/reference/providers.md
// §2.2): every request with one file part gets the spec-derived list-form
// reply of the fixture tree. With -asr-key it requires Bearer <key>.
func mountASR(mux *http.ServeMux, c config) error {
	reply, err := os.ReadFile(filepath.Join(c.Fixtures, "providers", "whisperx", "list-form.spec-derived.json"))
	if err != nil {
		return err
	}
	key, err := readKey(c.ASRKeyFile)
	if err != nil {
		return err
	}
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Header.Get("X-Request-ID") // <job_id>:<track_ref>:<attempt>
		status, files, size := asrStatus(r, key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write(reply)
		} else {
			_, _ = w.Write([]byte(`{"detail":"` + http.StatusText(status) + `"}`))
		}
		c.Log.Info("mock asr request", "request_id", requestID, "status", status, "files", files,
			"file_bytes", size, "latency_ms", time.Since(start).Milliseconds())
	})
	return nil
}

// asrStatus checks the credential and reads the multipart stream, counting
// file parts and their bytes. Nothing is kept.
func asrStatus(r *http.Request, key string) (status, files int, size int64) {
	if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
		return http.StatusUnauthorized, 0, 0
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return http.StatusBadRequest, 0, 0
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return http.StatusBadRequest, files, size
		}
		n, err := io.Copy(io.Discard, p)
		if err != nil {
			return http.StatusBadRequest, files, size
		}
		if p.FormName() == "file" {
			files++
			size += n
		}
	}
	if files != 1 {
		return http.StatusUnprocessableEntity, files, size
	}
	return http.StatusOK, files, size
}
