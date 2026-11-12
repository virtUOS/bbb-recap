package ingest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// runService replays the golden exchanges against a running service. Each
// case gets fresh record_ids (contracts/README.md), so the
// cases are independent and can run against a non-empty service, repeatedly.
func runService(t *testing.T, baseURL string, cases []exchangeCase) {
	client := &http.Client{Timeout: 60 * time.Second}
	base := strings.TrimRight(baseURL, "/")
	for _, c := range cases {
		steps := c.stepsOn("service")
		if len(steps) == 0 {
			continue // exporter side only
		}
		t.Run(c.file, func(t *testing.T) {
			fr := freshRecords(t, steps)
			bd := bindings{}
			for i, s := range steps {
				if err := replay(t, client, base, s, bd, fr); err != nil {
					t.Fatalf("step %d (%s %s): %v", i+1, s.Request.Method, s.Request.Path, err)
				}
			}
		})
	}
}

// fresh holds one case's derived manifests: golden package_id → manifest
// bytes with a fresh record_id, and a replacer for golden IDs in paths and
// expected bodies.
type fresh struct {
	manifests map[string][]byte
	ids       *strings.Replacer
}

// freshRecords gives every record_id of a case a fresh one. Each golden
// manifest is first rebuilt unchanged and checked against its golden
// package_id, then rebuilt with the fresh record_id.
func freshRecords(t *testing.T, steps []exchangeStep) fresh {
	t.Helper()
	fr := fresh{manifests: map[string][]byte{}}
	records := map[string]string{}
	var pairs []string
	for _, s := range steps {
		m := packagePathRE.FindStringSubmatch(s.Request.Path)
		if s.Request.Method != http.MethodPut || m == nil || fr.manifests[m[1]] != nil {
			continue
		}
		golden := readBody(t, s.Request.Body)
		var v map[string]any
		dec := json.NewDecoder(bytes.NewReader(golden))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("golden manifest %s: %v", s.Request.Body.Source, err)
		}
		if again := canonical(t, v); !bytes.Equal(again, golden) || sha256hex(again) != m[1] {
			t.Fatalf("golden manifest %s: rebuilding it does not give package_id %s", s.Request.Body.Source, m[1])
		}
		old, _ := v["record_id"].(string)
		if records[old] == "" {
			records[old] = newRecordID(t, old)
			pairs = append(pairs, old, records[old])
		}
		v["record_id"] = records[old]
		derived := canonical(t, v)
		fr.manifests[m[1]] = derived
		pairs = append(pairs, m[1], sha256hex(derived))
	}
	fr.ids = strings.NewReplacer(pairs...)
	return fr
}

// canonical encodes a manifest as JCS (RFC 8785) for the value types a
// manifest has: ASCII-sorted keys, integers, strings without HTML escaping.
// freshRecords checks the result against the golden bytes first.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// newRecordID keeps the BBB shape <40 hex>-<ms timestamp> of the fixture's
// record_id with a random hex part.
func newRecordID(t *testing.T, old string) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	_, ts, _ := strings.Cut(old, "-")
	return hex.EncodeToString(b) + "-" + ts
}

func replay(t *testing.T, client *http.Client, base string, s exchangeStep, bd bindings, fr fresh) error {
	body := readBody(t, s.Request.Body)
	if m := packagePathRE.FindStringSubmatch(s.Request.Path); m != nil && s.Request.Method == http.MethodPut {
		body = fr.manifests[m[1]]
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	if s.Request.Fault == "abort_body" {
		// Send half of the body under the full Content-Length, then drop the connection.
		reader = &abortReader{data: body[:len(body)/2]}
	}
	req, err := http.NewRequest(s.Request.Method, base+fr.ids.Replace(s.Request.Path), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("Authorization", "Bearer "+token(t, s.Request.Credential))
	for k, v := range s.Request.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if s.Request.Fault == "abort_body" {
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				return fmt.Errorf("interrupted upload got %d, want no success", resp.StatusCode)
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	want := s.Response
	if resp.StatusCode != want.Status {
		return fmt.Errorf("status %d, want %d", resp.StatusCode, want.Status)
	}
	if err := checkHeaders(want.Headers, resp.Header.Get); err != nil {
		return err
	}
	if !want.hasBody() {
		return nil
	}
	var w, g any
	if err := json.Unmarshal(want.Body, &w); err != nil {
		return fmt.Errorf("golden body: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Errorf("response body is not JSON: %v", err)
	}
	w = mapStrings(w, fr.ids.Replace)
	return bd.match("body", w, g)
}

// abortReader yields its data and then fails, so the HTTP client closes the
// connection before the announced Content-Length is reached.
type abortReader struct{ data []byte }

func (r *abortReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("upload interrupted on purpose")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
