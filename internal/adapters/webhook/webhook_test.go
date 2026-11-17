package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/sink"
)

// Synthetic values (fixtures.md); canaries for the log.
const (
	title   = "Fixture Widget Meeting 2025-10-29"
	summary = "## Overview\n\nThe team chose blue for the test widget.\n"
	jobID   = "019a2f8c-3b40-7000-8000-000000000009"
	query   = "channel=fixture-query-canary"
)

var keys = [][]byte{[]byte("FIXTURE-NOT-A-SECRET-webhook-unit-00001"), bytes.Repeat([]byte{9}, 24)}

func request() sink.Request {
	sum := sha256.Sum256([]byte(summary))
	return sink.Request{JobID: jobID, SinkProfile: "webhook-main", Title: title, SummaryMD: summary,
		SummarySHA256: hexString(sum[:]), Owner: sink.Owner{Sub: "fixture-sub-ada-0001", Email: "ada.testperson@example.org"},
		Links:      []sink.Link{{SinkProfile: "docs-main", URL: "https://docs.recap.test/docs/d1/"}},
		RenderedAt: "2025-10-29T10:19:00Z"}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

type received struct {
	header http.Header
	body   []byte
}

func server(t *testing.T, status int, got *[]received) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = append(*got, received{r.Header.Clone(), b})
		if status == http.StatusMovedPermanently {
			w.Header().Set("Location", "/elsewhere")
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "fixture-receiver-body")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSendSignedNotice(t *testing.T) {
	var got []received
	srv := server(t, http.StatusNoContent, &got)
	var logs bytes.Buffer
	c := New("webhook-main", srv.URL+"/hook?"+query, keys, ContentNotice, time.Second,
		slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.now = func() time.Time { return time.Unix(1761733200, 0) }
	id, err := c.Send(context.Background(), request())
	if err != nil || id != "" || len(got) != 1 {
		t.Fatalf("Send = %q, %v; %d requests", id, err, len(got))
	}
	h := got[0].header
	if h.Get("webhook-id") != jobID+":webhook-main" || h.Get("webhook-timestamp") != "1761733200" ||
		h.Get("Authorization") != "" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", h)
	}
	sigs := strings.Split(h.Get("webhook-signature"), " ")
	if len(sigs) != 2 {
		t.Fatalf("signatures %v, want one per key", sigs)
	}
	for i, k := range keys {
		mac := hmac.New(sha256.New, k)
		mac.Write([]byte(jobID + ":webhook-main.1761733200."))
		mac.Write(got[0].body)
		if sigs[i] != "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Errorf("signature %d does not verify", i)
		}
	}
	want := `{"type":"recap.summary.delivered","timestamp":"2025-10-29T10:19:00Z","data":{"job_id":"` + jobID +
		`","sink_profile":"webhook-main","title":"` + title + `","links":[{"sink_profile":"docs-main",` +
		`"url":"https://docs.recap.test/docs/d1/"}],"content":"notice","summary_md":null,"summary_sha256":null}}`
	if string(got[0].body) != want {
		t.Errorf("body\n%s\nwant\n%s", got[0].body, want)
	}
	for _, v := range []string{query, title, summary[:10], "docs.recap.test", "fixture-receiver-body", "FIXTURE-NOT-A-SECRET"} {
		if strings.Contains(logs.String(), v) {
			t.Errorf("log contains %q", v)
		}
	}
}

func TestBodySummaryAndNoLinks(t *testing.T) {
	c := New("webhook-main", "https://hooks.recap.test/x", keys[:1], ContentSummary, 0, slog.New(slog.DiscardHandler))
	req := request()
	req.Links = nil
	req.Title = "A & B <c>"
	b, err := c.Body(req)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Data struct {
			Title         string          `json:"title"`
			Links         json.RawMessage `json:"links"`
			Content       string          `json:"content"`
			SummaryMD     string          `json:"summary_md"`
			SummarySHA256 string          `json:"summary_sha256"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if string(p.Data.Links) != "[]" || p.Data.Content != "summary" || p.Data.SummaryMD != summary ||
		p.Data.SummarySHA256 != req.SummarySHA256 || !bytes.Contains(b, []byte(`"A & B <c>"`)) {
		t.Errorf("body %s", b)
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		t.Error("body ends with a newline")
	}
}

// Error classes of sinks.md §4b.3; a redirect is not followed.
func TestStatusClasses(t *testing.T) {
	for _, c := range []struct {
		status int
		class  sink.Class
	}{
		{200, ""}, {202, ""}, {429, sink.NotSent}, {500, sink.Uncertain}, {503, sink.Uncertain},
		{301, sink.Config}, {307, sink.Config}, {401, sink.Config}, {403, sink.Config}, {404, sink.Config},
		{410, sink.Config}, {400, sink.Rejected}, {422, sink.Rejected},
	} {
		var got []received
		srv := server(t, c.status, &got)
		cl := New("webhook-main", srv.URL+"/hook", keys, ContentNotice, time.Second, slog.New(slog.DiscardHandler))
		_, err := cl.Send(context.Background(), request())
		var serr *sink.Error
		switch {
		case c.class == "" && err != nil:
			t.Errorf("%d: %v, want success", c.status, err)
		case c.class != "" && (!errorsAs(err, &serr) || serr.Class != c.class):
			t.Errorf("%d: %v, want %s", c.status, err, c.class)
		case c.status == 429 && serr.RetryAfter != 7*time.Second:
			t.Errorf("429: Retry-After %v, want 7s", serr.RetryAfter)
		}
		if len(got) != 1 {
			t.Errorf("%d: %d requests, want 1", c.status, len(got))
		}
	}
}

func errorsAs(err error, target **sink.Error) bool {
	e, ok := err.(*sink.Error)
	if ok {
		*target = e
	}
	return ok
}

func TestTransportClasses(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	u := closed.URL
	closed.Close()
	c := New("webhook-main", u, keys, ContentNotice, time.Second, slog.New(slog.DiscardHandler))
	if _, err := c.Send(context.Background(), request()); err.(*sink.Error).Class != sink.NotSent {
		t.Errorf("refused: %v, want not_sent", err)
	}
	// The handler reads the body first: only then does the server notice
	// that the client went away and cancel the request's context.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	c = New("webhook-main", hang.URL, keys, ContentNotice, 200*time.Millisecond, slog.New(slog.DiscardHandler))
	if _, err := c.Send(context.Background(), request()); err.(*sink.Error).Class != sink.Uncertain {
		t.Errorf("hang: %v, want uncertain", err)
	}
}

// Precheck: the summary only with content summary; the title and
// rendered_at always.
func TestPrecheck(t *testing.T) {
	notice := New("w", "https://hooks.recap.test/x", keys, ContentNotice, 0, slog.New(slog.DiscardHandler))
	full := New("w", "https://hooks.recap.test/x", keys, ContentSummary, 0, slog.New(slog.DiscardHandler))
	bad := request()
	bad.SummaryMD += "<div>x</div>\n"
	if err := notice.Precheck(bad, 0); err != nil {
		t.Errorf("notice with a summary it does not send: %v", err)
	}
	if err := full.Precheck(bad, 0); err == nil || err.Class != sink.Rejected {
		t.Errorf("summary with a forbidden construct: %v, want rejected", err)
	}
	long := request()
	long.Title = strings.Repeat("t", 201)
	if err := notice.Precheck(long, 0); err == nil || err.Class != sink.Rejected {
		t.Errorf("long title: %v, want rejected", err)
	}
	noTime := request()
	noTime.RenderedAt = ""
	if err := notice.Precheck(noTime, 0); err == nil || err.Code != "rendered_at_invalid" {
		t.Errorf("no rendered_at: %v", err)
	}
}
