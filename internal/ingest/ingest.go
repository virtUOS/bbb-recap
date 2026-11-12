// Package ingest is the service side of the push ingest protocol
// (docs/reference/bbb-exporter.md §6, docs/reference/architecture.md §4.1): manifest
// PUT, file PUT by hash, commit and status.
//
// Until commit a package is staging: a packages row (receiving or complete),
// its package_files rows and its blobs under jobs/<job_id>/, where job_id is
// allocated at the first manifest PUT. The canonical manifest bytes are
// stored there too, under their own SHA-256 (the package_id). The commit
// marks the package committed, creates the job with its steps and writes an
// ingest_commit audit row in one transaction; the IngestAck is sent only
// after that transaction has committed.
//
// Logs carry IDs only: never request bodies, manifest paths (they hold BBB
// user IDs) or tokens (privacy.md §6).
package ingest

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// MaxUploads is the number of file uploads one api process serves at once;
// one more gets 429 (architecture.md §7).
const MaxUploads = 8

// retryAfter is the Retry-After of 429 and 503 (bbb-exporter.md §6.2).
const retryAfter = "30"

// Problem slugs of this API besides the manifest rule slugs of package
// manifest (bbb-exporter.md §6.2).
const (
	problemUnknownPackage        = "unknown-package"
	problemFileNotInManifest     = "file-not-in-manifest"
	problemPackageCommitted      = "package-committed"
	problemPackageIncomplete     = "package-incomplete"
	problemRecordAlreadyIngested = "record-already-ingested"
	problemLengthRequired        = "length-required"
	problemRateLimited           = "rate-limited"
	problemUnavailable           = "unavailable"
	problemInternal              = "internal"
)

// Config is what the ingest API needs from the process.
type Config struct {
	Pool       *pgxpool.Pool
	Blobs      blob.Store
	Creds      *httpapi.Credentials
	MetaPrefix string // RECAP_META_PREFIX
	ConfigHash string // config.Hash(), pinned by every job created here
	Log        *slog.Logger
}

// Service serves the ingest API.
type Service struct {
	cfg     Config
	uploads chan struct{} // one token per running file upload

	// beforeCommit, if set, runs inside the commit transaction right before
	// it commits. Tests use it to inject a failure.
	beforeCommit func(ctx context.Context, tx pgx.Tx) error
}

// New returns the ingest service.
func New(cfg Config) *Service {
	return &Service{cfg: cfg, uploads: make(chan struct{}, MaxUploads)}
}

// Mount adds the ingest routes to mux. Only bbb_server credentials get in.
func (s *Service) Mount(mux *http.ServeMux) {
	auth := func(h http.HandlerFunc) http.Handler {
		return s.cfg.Creds.Require(httpapi.KindBBBServer, countRequests(h))
	}
	const base = "/api/v1/ingest/packages/{package_id}"
	mux.Handle("PUT "+base, auth(s.putManifest))
	mux.Handle("GET "+base, auth(s.getStatus))
	mux.Handle("PUT "+base+"/files/{sha256}", auth(s.putFile))
	mux.Handle("POST "+base+"/commit", auth(s.commit))
}

// ackFields are the IngestAck fields beyond package_id and state.
type ackFields struct {
	JobID       string `json:"job_id"`
	RecordID    string `json:"record_id"`
	FileCount   int    `json:"file_count"`
	Size        int64  `json:"size"`
	CommittedAt string `json:"committed_at"`
}

// Status is the IngestStatus body, with the ack fields once committed.
type Status struct {
	PackageID    string   `json:"package_id"`
	State        string   `json:"state"`
	Missing      []string `json:"missing"`
	ReceivedSize int64    `json:"received_size"`
	ExpectedSize int64    `json:"expected_size"`
	*ackFields
}

// Ack is the IngestAck body.
type Ack struct {
	PackageID string `json:"package_id"`
	State     string `json:"state"`
	ackFields
}

func (s *Service) putManifest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	client, _ := httpapi.ClientFrom(ctx)
	packageID := r.PathValue("package_id")

	if r.ContentLength > manifest.MaxManifestSize {
		httpapi.WriteProblem(w, http.StatusRequestEntityTooLarge, manifest.ProblemPackageTooLarge, "manifest over 1 MiB")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, manifest.MaxManifestSize))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpapi.WriteProblem(w, http.StatusRequestEntityTooLarge, manifest.ProblemPackageTooLarge, "manifest over 1 MiB")
		} else {
			httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "")
		}
		return
	}

	m, v := manifest.Check(data, manifest.Options{
		PackageID:   packageID,
		BBBServerID: client.ID,
		MetaPrefix:  s.cfg.MetaPrefix,
	})
	if v != nil {
		// v.Detail may quote manifest values, so only the rule is logged and sent.
		s.cfg.Log.Debug("manifest rejected", "bbb_server_id", client.ID, "rule", v.Rule, "status", v.Status)
		httpapi.WriteProblem(w, v.Status, v.Problem, v.Rule)
		return
	}

	if err := s.touch(ctx, packageID, client.ID); err != nil {
		s.serverError(w, "record activity", err)
		return
	}
	st, err := s.status(ctx, s.cfg.Pool, packageID, client.ID)
	if err == nil {
		writeJSON(w, http.StatusOK, st)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.serverError(w, "read package", err)
		return
	}

	jobID, created, err := s.create(ctx, m, packageID, data)
	if err != nil {
		s.serverError(w, "create package", err)
		return
	}
	if !created {
		// Lost a race: either the same package (known) or another package
		// for the record.
		st, err := s.status(ctx, s.cfg.Pool, packageID, client.ID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, st)
		case errors.Is(err, pgx.ErrNoRows):
			s.cfg.Log.Info("record already ingested", "bbb_server_id", client.ID, "package_id", packageID)
			httpapi.WriteProblem(w, http.StatusConflict, problemRecordAlreadyIngested, "")
		default:
			s.serverError(w, "read package", err)
		}
		return
	}
	s.cfg.Log.Info("package staged", "bbb_server_id", client.ID, "package_id", packageID, "job_id", jobID)
	st, err = s.status(ctx, s.cfg.Pool, packageID, client.ID)
	if err != nil {
		s.serverError(w, "read package", err)
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (s *Service) getStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	client, _ := httpapi.ClientFrom(ctx)
	packageID := r.PathValue("package_id")
	if err := s.touch(ctx, packageID, client.ID); err != nil {
		s.serverError(w, "record activity", err)
		return
	}
	st, err := s.status(ctx, s.cfg.Pool, packageID, client.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.WriteProblem(w, http.StatusNotFound, problemUnknownPackage, "")
	case err != nil:
		s.serverError(w, "read package", err)
	default:
		writeJSON(w, http.StatusOK, st)
	}
}

func (s *Service) putFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	client, _ := httpapi.ClientFrom(ctx)
	packageID, hash := r.PathValue("package_id"), r.PathValue("sha256")

	if r.Header.Get("Content-Length") == "" || r.ContentLength < 0 {
		httpapi.WriteProblem(w, http.StatusLengthRequired, problemLengthRequired, "")
		return
	}
	if r.ContentLength > manifest.MaxFileSize {
		httpapi.WriteProblem(w, http.StatusRequestEntityTooLarge, manifest.ProblemPackageTooLarge, "file over the size limit")
		return
	}
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		w.Header().Set("Retry-After", retryAfter)
		httpapi.WriteProblem(w, http.StatusTooManyRequests, problemRateLimited, "")
		return
	}

	f, err := s.lookupFile(ctx, packageID, client.ID, hash)
	switch {
	case errors.Is(err, errUnknownPackage):
		httpapi.WriteProblem(w, http.StatusNotFound, problemUnknownPackage, "")
		return
	case errors.Is(err, errNotInManifest):
		httpapi.WriteProblem(w, http.StatusNotFound, problemFileNotInManifest, "")
		return
	case err != nil:
		s.serverError(w, "read package", err)
		return
	}
	if !digestMatches(r.Header.Get("Content-Digest"), hash) {
		httpapi.WriteProblem(w, http.StatusUnprocessableEntity, manifest.ProblemFileHashMismatch, "Content-Digest")
		return
	}
	if f.state == stateCommitted {
		httpapi.WriteProblem(w, http.StatusConflict, problemPackageCommitted, "")
		return
	}
	if f.verified {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.ContentLength != f.size {
		httpapi.WriteProblem(w, http.StatusUnprocessableEntity, manifest.ProblemFileHashMismatch, "size")
		return
	}

	if err := s.cfg.Blobs.Put(ctx, f.jobID, hash, f.size, r.Body); err != nil {
		if errors.Is(err, blob.ErrHashMismatch) {
			s.cfg.Log.Debug("upload does not match its hash", "package_id", packageID, "sha256", hash)
			httpapi.WriteProblem(w, http.StatusUnprocessableEntity, manifest.ProblemFileHashMismatch, "")
			return
		}
		if ctx.Err() != nil {
			return // the client went away; nobody reads an answer
		}
		s.serverError(w, "store file", err)
		return
	}
	stored, err := s.markVerified(ctx, packageID, hash)
	if err != nil {
		s.serverError(w, "mark file verified", err)
		return
	}
	s.cfg.Log.Debug("file stored", "package_id", packageID, "job_id", f.jobID, "sha256", hash)
	if stored {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Service) commit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	client, _ := httpapi.ClientFrom(ctx)
	packageID := r.PathValue("package_id")

	missing, err := s.commitPackage(ctx, packageID, client.ID)
	switch {
	case errors.Is(err, errUnknownPackage):
		httpapi.WriteProblem(w, http.StatusNotFound, problemUnknownPackage, "")
		return
	case err != nil:
		s.serverError(w, "commit package", err)
		return
	case len(missing) > 0:
		writeIncomplete(w, missing)
		return
	}

	// The job is committed; only now may the ack go out.
	st, err := s.status(ctx, s.cfg.Pool, packageID, client.ID)
	if err != nil {
		s.serverError(w, "read package", err)
		return
	}
	writeJSON(w, http.StatusOK, Ack{PackageID: st.PackageID, State: st.State, ackFields: *st.ackFields})
}

// serverError answers a database or blob store failure: 500 internal for
// an error that a retry cannot fix (a bad blob key; the database rejecting
// a statement, data or constraint), else 503 unavailable with Retry-After
// (store down, connection lost, injected or other errors).
func (s *Service) serverError(w http.ResponseWriter, what string, err error) {
	s.cfg.Log.Error("ingest: "+what+" failed", "err", err)
	if isInternal(err) {
		httpapi.WriteProblem(w, http.StatusInternalServerError, problemInternal, "")
		return
	}
	w.Header().Set("Retry-After", retryAfter)
	httpapi.WriteProblem(w, http.StatusServiceUnavailable, problemUnavailable, "")
}

// isInternal reports whether err is a programming or data error: an invalid
// blob key, or a Postgres error of class 22 (data exception), 23 (integrity
// constraint violation) or 42 (syntax error or access rule violation).
func isInternal(err error) bool {
	if errors.Is(err, blob.ErrInvalidKey) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 {
		switch pgErr.Code[:2] {
		case "22", "23", "42":
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeIncomplete answers 409 package-incomplete with the missing hashes as
// an RFC 9457 extension member.
func writeIncomplete(w http.ResponseWriter, missing []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(struct {
		httpapi.Problem
		Missing []string `json:"missing"`
	}{
		Problem: httpapi.Problem{
			Type:   httpapi.ProblemTypePrefix + problemPackageIncomplete,
			Title:  http.StatusText(http.StatusConflict),
			Status: http.StatusConflict,
		},
		Missing: missing,
	})
}

// digestMatches checks an RFC 9530 Content-Digest header against the hash
// from the path. Only sha-256 is checked; other algorithms are ignored. A
// sha-256 member that cannot be read counts as a mismatch.
func digestMatches(header, hash string) bool {
	if header == "" {
		return true
	}
	for member := range strings.SplitSeq(header, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(member), "=")
		if strings.ToLower(strings.TrimSpace(key)) != "sha-256" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) < 2 || value[0] != ':' || value[len(value)-1] != ':' {
			return false
		}
		sum, err := base64.StdEncoding.DecodeString(value[1 : len(value)-1])
		if err != nil || hex.EncodeToString(sum) != hash {
			return false
		}
	}
	return true
}
