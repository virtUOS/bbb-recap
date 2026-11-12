package sink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testdata = "../../testdata"

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func request(md string) Request {
	return Request{JobID: "019a2f8c-3b40-7000-8000-000000000009", SinkProfile: "docs-main",
		Title: "Fixture Widget Meeting 2025-10-29", SummaryMD: md, SummarySHA256: hexSHA256(md),
		Owner: Owner{Sub: "fixture-sub-ada-0001", Email: "ada.testperson@example.org"}}
}

func TestCheckAllowed(t *testing.T) {
	files := []string{filepath.Join(testdata, "sink", "summary.md"), filepath.Join(testdata, "sink", "allowed.md")}
	goldens, _ := filepath.Glob(filepath.Join(testdata, "render", "*", "summary*.md"))
	if len(goldens) == 0 {
		t.Fatal("no render goldens")
	}
	for _, f := range append(files, goldens...) {
		if err := Check(request(read(t, f)), 0); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	req := request("Text\n")
	req.Title = strings.Repeat("ü", 200)
	if err := Check(req, 0); err != nil {
		t.Errorf("200-character title: %v", err)
	}
}

func TestCheckForbidden(t *testing.T) {
	summary := read(t, filepath.Join(testdata, "sink", "summary.md"))
	files, _ := filepath.Glob(filepath.Join(testdata, "sink", "forbidden", "*.md"))
	if len(files) != 7 {
		t.Fatalf("forbidden fixtures: %d", len(files))
	}
	for _, f := range files {
		if err := Check(request(summary+"\n"+read(t, f)), 0); err == nil || err.Class != Rejected {
			t.Errorf("%s: got %v, want rejected", filepath.Base(f), err)
		}
	}
	long := request(summary)
	long.Title = strings.Repeat("ü", 201)
	bad := request("x")
	bad.SummarySHA256 = strings.Repeat("0", 64)
	newline := request("x")
	newline.Title = "A\nB"
	for name, c := range map[string]struct {
		req  Request
		max  int64
		code string
	}{
		"too large":        {request(strings.Repeat("a", 11)), 10, "document_too_large"},
		"default max":      {request(strings.Repeat("a", DefaultMaxDocumentBytes+1)), 0, "document_too_large"},
		"invalid utf-8":    {request("a\xffb"), 0, "invalid_utf8"},
		"title too long":   {long, 0, "title_too_long"},
		"title newline":    {newline, 0, "title_invalid"},
		"sha256 mismatch":  {bad, 0, "summary_sha256_mismatch"},
		"setext h2":        {request("Para\n---\n"), 0, "setext_heading"},
		"html comment":     {request("a <!-- b -->\n"), 0, "raw_html"},
		"html in fence ok": {request("```\n<div>\n```\n"), 0, ""},
		"code span ok":     {request("`<div>` and `![x]`\n"), 0, ""},
		"escaped ok":       {request(`\<div\> \!\[x\]` + "\n"), 0, ""},
	} {
		err := Check(c.req, c.max)
		switch {
		case c.code == "" && err != nil:
			t.Errorf("%s: %v, want ok", name, err)
		case c.code != "" && (err == nil || err.Code != c.code || err.Class != Rejected):
			t.Errorf("%s: %v, want rejected %s", name, err, c.code)
		}
	}
}

type fakeSender struct {
	calls int
	id    string
	err   error
}

func (f *fakeSender) Send(context.Context, Request) (string, error) {
	f.calls++
	return f.id, f.err
}

func TestDeliver(t *testing.T) {
	req := request("# Summary\n")
	begin := func(n *int, err error) func(context.Context) error {
		return func(context.Context) error { *n++; return err }
	}
	t.Run("sent", func(t *testing.T) {
		s, n := &fakeSender{id: "doc-1"}, 0
		rec, err := Deliver(context.Background(), s, req, nil, 0, begin(&n, nil))
		if err != nil || rec.State != StateSent || *rec.DocumentID != "doc-1" || rec.SentAt == nil || s.calls != 1 || n != 1 {
			t.Fatalf("rec %+v err %v calls %d begins %d", rec, err, s.calls, n)
		}
	})
	for _, c := range []struct {
		class  Class
		record bool
	}{{NotSent, false}, {Uncertain, true}, {Config, false}, {RecipientUnresolved, false}, {Rejected, false}} {
		t.Run(string(c.class), func(t *testing.T) {
			s, n := &fakeSender{err: &Error{Class: c.class, Code: "x"}}, 0
			rec, err := Deliver(context.Background(), s, req, nil, 0, begin(&n, nil))
			var serr *Error
			if !errors.As(err, &serr) || serr.Class != c.class || s.calls != 1 {
				t.Fatalf("err %v calls %d", err, s.calls)
			}
			if c.record != (rec != nil) || (rec != nil && rec.State != StateSending) {
				t.Fatalf("record %+v", rec)
			}
		})
	}
	t.Run("re-entry in sending", func(t *testing.T) {
		s, n := &fakeSender{id: "doc"}, 0
		rec, err := Deliver(context.Background(), s, req, &Record{State: StateSending}, 0, begin(&n, nil))
		var serr *Error
		if !errors.As(err, &serr) || serr.Class != Uncertain || rec.State != StateSending || s.calls != 0 || n != 0 {
			t.Fatalf("rec %+v err %v calls %d", rec, err, s.calls)
		}
	})
	t.Run("record exists at begin", func(t *testing.T) {
		s, n := &fakeSender{id: "doc"}, 0
		_, err := Deliver(context.Background(), s, req, nil, 0, begin(&n, ErrRecordExists))
		var serr *Error
		if !errors.As(err, &serr) || serr.Class != Uncertain || s.calls != 0 {
			t.Fatalf("err %v calls %d", err, s.calls)
		}
	})
	t.Run("begin fails", func(t *testing.T) {
		s, n := &fakeSender{id: "doc"}, 0
		boom := errors.New("db down")
		rec, err := Deliver(context.Background(), s, req, nil, 0, begin(&n, boom))
		if !errors.Is(err, boom) || rec != nil || s.calls != 0 {
			t.Fatalf("rec %+v err %v calls %d", rec, err, s.calls)
		}
	})
	t.Run("pre-check before begin", func(t *testing.T) {
		s, n := &fakeSender{id: "doc"}, 0
		rec, err := Deliver(context.Background(), s, request("<div>x</div>\n"), nil, 0, begin(&n, nil))
		var serr *Error
		if !errors.As(err, &serr) || serr.Class != Rejected || rec != nil || s.calls != 0 || n != 0 {
			t.Fatalf("rec %+v err %v calls %d begins %d", rec, err, s.calls, n)
		}
	})
	t.Run("already sent", func(t *testing.T) {
		s, id := &fakeSender{id: "doc"}, "doc-0"
		prior := &Record{State: StateSent, DocumentID: &id}
		rec, err := Deliver(context.Background(), s, req, prior, 0, nil)
		if err != nil || rec != prior || s.calls != 0 {
			t.Fatalf("rec %+v err %v calls %d", rec, err, s.calls)
		}
	})
}
