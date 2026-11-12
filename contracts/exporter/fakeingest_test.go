package exporter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJobID is the job_id the fake service puts in every ack (UUIDv7 shape).
const fakeJobID = "019a3000-0000-7000-8000-000000000001"

// Request kinds of the ingest protocol (docs/reference/bbb-exporter.md §6.2).
const (
	kindManifest = "manifest" // PUT /api/v1/ingest/packages/{package_id}
	kindFile     = "file"     // PUT …/{package_id}/files/{sha256}
	kindCommit   = "commit"   // POST …/{package_id}/commit
	kindStatus   = "status"   // GET …/{package_id}
)

// fault tells the fake service how to answer one request instead of the
// normal behaviour.
type fault struct {
	status     int    // reply with this problem status (0 = handle normally)
	slug       string // problem type slug for status
	retryAfter string // Retry-After header value
	hold       bool   // block until released or until the client goes away
	badAck     string // commit: answer an ack whose package_id, record_id, file_count, size or job_id is wrong
}

// rule returns the fault for the n-th request (1-based) of a kind, or nil.
type rule func(kind string, n int) *fault

type logEntry struct {
	Method, Path string // Path with the package_id replaced by {package_id}
	Status       int
}

type fakePackage struct {
	id       string
	recordID string
	files    map[string]int64 // sha256 -> size
	received map[string]bool
	total    int64 // bytes per distinct hash (IngestStatus sizes)
	ackCount int   // manifest totals per path (IngestAck, §6.2)
	ackSize  int64
	commit   bool
}

// fakeIngest is a minimal TLS ingest service for the exporter tests. It
// implements the wire protocol of docs/reference/bbb-exporter.md §6.2 and the
// checks V-I01, V-I03, V-I05, V-I06 (meeting-package.md §7). It is not the
// service; the ingest contract tests (§6.6) pin the full exchanges.
type fakeIngest struct {
	t       *testing.T
	srv     *httptest.Server
	token   string
	rule    rule
	arrived chan string   // kind of every held request, on arrival
	release chan struct{} // sent to, to let a held request continue
	done    chan struct{} // closed at test end, so held requests return

	mu       sync.Mutex
	counts   map[string]int
	log      []logEntry
	packages map[string]*fakePackage
	byRecord map[string]string
}

var ingestPath = regexp.MustCompile(`^/api/v1/ingest/packages/([0-9a-f]{64})(/files/([0-9a-f]{64})|/commit)?$`)

func newFakeIngest(t *testing.T, token string, r rule) *fakeIngest {
	t.Helper()
	f := &fakeIngest{
		t:        t,
		token:    token,
		rule:     r,
		arrived:  make(chan string, 16),
		release:  make(chan struct{}),
		done:     make(chan struct{}),
		counts:   map[string]int{},
		packages: map[string]*fakePackage{},
		byRecord: map[string]string{},
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.done) }) // runs before srv.Close
	return f
}

// URL is the service base URL for RECAP_EXPORTER_SERVICE_URL.
func (f *fakeIngest) URL() string { return f.srv.URL }

// writeCert writes the server certificate as PEM, for SSL_CERT_FILE
// (docs/reference/deployment.md §1: Go reads it on Linux).
func (f *fakeIngest) writeCert(path string) {
	f.t.Helper()
	if err := os.WriteFile(path, pemCert(f.srv.Certificate().Raw), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// waitArrived waits until a held request of kind arrives. It fails early
// when alive reports that the client is gone.
func (f *fakeIngest) waitArrived(kind string, timeout time.Duration, alive func() bool) {
	f.t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case k := <-f.arrived:
			if k == kind {
				return
			}
		case <-tick.C:
			if !alive() {
				f.t.Fatalf("client exited before a %s request; requests: %v", kind, f.requests())
			}
		case <-deadline:
			f.t.Fatalf("no held %s request within %s; requests: %v", kind, timeout, f.requests())
		}
	}
}

// releaseOne lets one held request continue.
func (f *fakeIngest) releaseOne() {
	select {
	case f.release <- struct{}{}:
	case <-time.After(10 * time.Second):
		f.t.Fatal("no held request to release")
	}
}

func (f *fakeIngest) requests() []logEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logEntry(nil), f.log...)
}

func (f *fakeIngest) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[kind]
}

// committed returns the package IDs the fake service holds for a record and
// whether the current one is committed.
func (f *fakeIngest) committed(recordID string) (packageID string, done bool, all []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, p := range f.packages {
		if p.recordID == recordID {
			all = append(all, id)
		}
	}
	sort.Strings(all)
	packageID = f.byRecord[recordID]
	if p := f.packages[packageID]; p != nil {
		done = p.commit
	}
	return packageID, done, all
}

func (f *fakeIngest) serve(w http.ResponseWriter, r *http.Request) {
	m := ingestPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		f.reply(w, r, "", http.StatusNotFound, problem("not-found"))
		return
	}
	packageID, sha := m[1], m[3]
	kind := kindStatus
	switch {
	case r.Method == http.MethodPut && sha != "":
		kind = kindFile
	case r.Method == http.MethodPut:
		kind = kindManifest
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commit"):
		kind = kindCommit
	case r.Method != http.MethodGet || m[2] != "":
		f.reply(w, r, packageID, http.StatusMethodNotAllowed, problem("method-not-allowed"))
		return
	}

	// Read the body first: only then does net/http notice a client that goes away.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.reply(w, r, packageID, http.StatusUnauthorized, problem("unauthorized"))
		return
	}

	f.mu.Lock()
	f.counts[kind]++
	n := f.counts[kind]
	f.mu.Unlock()

	var flt *fault
	if f.rule != nil {
		flt = f.rule(kind, n)
	}
	if flt != nil && flt.hold {
		f.arrived <- kind
		select {
		case <-f.release:
		case <-r.Context().Done():
			f.logRequest(r, packageID, 0) // client killed while held
			return
		case <-f.done:
			return
		}
	}
	if flt != nil && flt.status != 0 {
		if flt.retryAfter != "" {
			w.Header().Set("Retry-After", flt.retryAfter)
		}
		f.reply(w, r, packageID, flt.status, problem(flt.slug))
		return
	}

	switch kind {
	case kindManifest:
		f.putManifest(w, r, packageID, body)
	case kindFile:
		f.putFile(w, r, packageID, sha, body)
	case kindCommit:
		badAck := ""
		if flt != nil {
			badAck = flt.badAck
		}
		f.commit(w, r, packageID, badAck)
	default:
		f.status(w, r, packageID)
	}
}

func (f *fakeIngest) putManifest(w http.ResponseWriter, r *http.Request, packageID string, body []byte) {
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != packageID {
		f.reply(w, r, packageID, http.StatusBadRequest, problem("package-id-mismatch"))
		return
	}
	var man struct {
		BBBServerID string `json:"bbb_server_id"`
		RecordID    string `json:"record_id"`
		Files       []struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &man); err != nil || man.RecordID == "" || len(man.Files) == 0 {
		f.reply(w, r, packageID, http.StatusUnprocessableEntity, problem("manifest-invalid"))
		return
	}
	if man.BBBServerID != fixtureServerID {
		f.reply(w, r, packageID, http.StatusForbidden, problem("server-mismatch"))
		return
	}

	f.mu.Lock()
	if other, ok := f.byRecord[man.RecordID]; ok && other != packageID {
		f.mu.Unlock()
		f.reply(w, r, packageID, http.StatusConflict, problem("record-already-ingested"))
		return
	}
	status := http.StatusOK
	p := f.packages[packageID]
	if p == nil {
		status = http.StatusCreated
		p = &fakePackage{id: packageID, recordID: man.RecordID, files: map[string]int64{}, received: map[string]bool{}}
		for _, file := range man.Files {
			if _, dup := p.files[file.SHA256]; !dup {
				p.total += file.Size
			}
			p.files[file.SHA256] = file.Size
			p.ackCount++
			p.ackSize += file.Size
		}
		f.packages[packageID] = p
		f.byRecord[man.RecordID] = packageID
	}
	st := p.statusLocked()
	f.mu.Unlock()
	f.reply(w, r, packageID, status, st)
}

func (f *fakeIngest) putFile(w http.ResponseWriter, r *http.Request, packageID, sha string, body []byte) {
	if r.ContentLength < 0 {
		f.reply(w, r, packageID, http.StatusLengthRequired, problem("length-required"))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.packages[packageID]
	switch {
	case p == nil:
		f.replyLocked(w, r, packageID, http.StatusNotFound, problem("unknown-package"))
		return
	case p.commit:
		f.replyLocked(w, r, packageID, http.StatusConflict, problem("package-committed"))
		return
	}
	size, ok := p.files[sha]
	if !ok {
		f.replyLocked(w, r, packageID, http.StatusNotFound, problem("file-not-in-manifest"))
		return
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != sha || int64(len(body)) != size {
		f.replyLocked(w, r, packageID, http.StatusUnprocessableEntity, problem("file-hash-mismatch"))
		return
	}
	status := http.StatusNoContent
	if p.received[sha] {
		status = http.StatusOK
	}
	p.received[sha] = true
	f.replyLocked(w, r, packageID, status, nil)
}

func (f *fakeIngest) commit(w http.ResponseWriter, r *http.Request, packageID string, badAck string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.packages[packageID]
	if p == nil {
		f.replyLocked(w, r, packageID, http.StatusNotFound, problem("unknown-package"))
		return
	}
	st := p.statusLocked()
	if missing := st["missing"].([]string); len(missing) > 0 {
		body := problem("package-incomplete")
		body["missing"] = missing
		f.replyLocked(w, r, packageID, http.StatusConflict, body)
		return
	}
	p.commit = true
	ack := map[string]any{
		"package_id":   packageID,
		"state":        "committed",
		"job_id":       fakeJobID,
		"record_id":    p.recordID,
		"file_count":   p.ackCount,
		"size":         p.ackSize,
		"committed_at": "2025-10-29T10:11:00Z",
	}
	switch badAck {
	case "":
	case "package_id":
		ack["package_id"] = strings.Repeat("0", 64)
	case "record_id":
		ack["record_id"] = "0000000000000000000000000000000000000000-1761732000000"
	case "file_count":
		ack["file_count"] = p.ackCount + 1
	case "size":
		ack["size"] = p.ackSize + 1
	case "job_id":
		ack["job_id"] = "not-a-uuid"
	default:
		f.t.Errorf("unknown badAck %q", badAck)
	}
	f.replyLocked(w, r, packageID, http.StatusOK, ack)
}

func (f *fakeIngest) status(w http.ResponseWriter, r *http.Request, packageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.packages[packageID]
	if p == nil {
		f.replyLocked(w, r, packageID, http.StatusNotFound, problem("unknown-package"))
		return
	}
	f.replyLocked(w, r, packageID, http.StatusOK, p.statusLocked())
}

// statusLocked builds an IngestStatus (§6.2). Ack fields once committed are
// left out: the exporter verifies the ack from the commit response.
func (p *fakePackage) statusLocked() map[string]any {
	missing := []string{}
	var got int64
	for sha, size := range p.files {
		if p.received[sha] {
			got += size
		} else {
			missing = append(missing, sha)
		}
	}
	sort.Strings(missing)
	state := "receiving"
	switch {
	case p.commit:
		state = "committed"
	case len(missing) == 0:
		state = "complete"
	}
	return map[string]any{
		"package_id":    p.id,
		"state":         state,
		"missing":       missing,
		"received_size": got,
		"expected_size": p.total,
	}
}

func problem(slug string) map[string]any {
	return map[string]any{"type": "urn:recap:problem:" + slug, "title": slug}
}

func (f *fakeIngest) reply(w http.ResponseWriter, r *http.Request, packageID string, status int, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replyLocked(w, r, packageID, status, body)
}

func (f *fakeIngest) replyLocked(w http.ResponseWriter, r *http.Request, packageID string, status int, body map[string]any) {
	f.log = append(f.log, logEntry{Method: r.Method, Path: templatePath(r.URL.Path, packageID), Status: status})
	if body == nil {
		w.WriteHeader(status)
		return
	}
	ct := "application/json"
	if status >= 400 {
		ct = "application/problem+json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeIngest) logRequest(r *http.Request, packageID string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, logEntry{Method: r.Method, Path: templatePath(r.URL.Path, packageID), Status: status})
}

func templatePath(path, packageID string) string {
	if packageID == "" {
		return path
	}
	return strings.Replace(path, packageID, "{package_id}", 1)
}

func (e logEntry) String() string { return fmt.Sprintf("%s %s %d", e.Method, e.Path, e.Status) }

// distinctRequests is the set of requests (method and path) in a log, sorted.
func distinctRequests(log []logEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range log {
		k := e.Method + " " + e.Path
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
