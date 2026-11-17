package render

import "testing"

// The document limit of a job is the smallest max_document_bytes of its
// sinks that carry the summary (sinks.md §2.4).
func TestLimit(t *testing.T) {
	d := Deps{MaxDocumentBytes: map[string]int64{"docs-main": 3 << 20, "frontend-main": 1 << 20}}
	list := func(s string) *string { return &s }
	for _, c := range []struct {
		sinks *string
		want  int64
	}{
		{nil, DefaultMaxDocumentBytes},
		{list("docs-main"), 3 << 20},
		{list("docs-main,frontend-main"), 1 << 20},
		{list("webhook-notice"), DefaultMaxDocumentBytes},
		{list("docs-main,webhook-notice"), 3 << 20},
	} {
		if got := d.limit(c.sinks); got != c.want {
			t.Errorf("limit(%v) = %d, want %d", c.sinks, got, c.want)
		}
	}
}
