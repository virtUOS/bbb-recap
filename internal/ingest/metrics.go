package ingest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/metrics"
)

// Outcomes are the values of the outcome label of
// recap_ingest_requests_total: ok for a 2xx answer, else the problem slug.
// A slug not in this list is counted as other, so the label stays bounded.
var Outcomes = []string{
	metrics.OK,
	"invalid-request",
	problemUnknownPackage,
	problemFileNotInManifest,
	problemPackageCommitted,
	problemPackageIncomplete,
	problemRecordAlreadyIngested,
	problemLengthRequired,
	problemRateLimited,
	problemUnavailable,
	problemInternal,
	manifest.ProblemPackageIDMismatch,
	manifest.ProblemManifestInvalid,
	manifest.ProblemUnsupportedVersion,
	manifest.ProblemServerMismatch,
	manifest.ProblemFileHashMismatch,
	manifest.ProblemNotOptedIn,
	manifest.ProblemPackageTooLarge,
	"other",
}

// countRequests counts each authenticated request by BBB server (the
// credential's client ID) and outcome. It runs inside the authentication,
// so unauthenticated requests are not counted.
func countRequests(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &outcomeRecorder{ResponseWriter: w}
		next(rec, r)
		client, _ := httpapi.ClientFrom(r.Context())
		metrics.IngestRequest(client.ID, rec.outcome())
	}
}

// outcomeRecorder remembers the status and, for an error, the problem slug
// from the first write of the problem body (httpapi.WriteProblem writes it
// in one piece).
type outcomeRecorder struct {
	http.ResponseWriter
	status int
	slug   string
}

func (o *outcomeRecorder) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *outcomeRecorder) Write(b []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	if o.status >= 400 && o.slug == "" &&
		strings.HasPrefix(o.Header().Get("Content-Type"), "application/problem+json") {
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &p) == nil {
			o.slug = strings.TrimPrefix(p.Type, httpapi.ProblemTypePrefix)
		}
	}
	return o.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (o *outcomeRecorder) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *outcomeRecorder) outcome() string {
	if o.status < 400 { // 0: nothing written, net/http answers 200
		return metrics.OK
	}
	if o.slug != metrics.OK && slices.Contains(Outcomes, o.slug) {
		return o.slug
	}
	return "other"
}
