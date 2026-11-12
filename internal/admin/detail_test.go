package admin

import (
	"errors"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/queue"
)

// JobDetail: steps in pipeline order with their attempts and error codes,
// rerun_of and the current step; ErrNotFound for an unknown, malformed or
// erased job (the web UI answers 404, not 500).
func TestJobDetail(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "failed")
	e.exec(t, `UPDATE steps SET last_error_class = 'permanent', last_error_code = 'events-mismatch'
		WHERE job_id = $1 AND name = 'normalize'`, id)

	d, err := e.api.JobDetail(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range d.Steps {
		names = append(names, s.Name)
	}
	if len(names) != len(queue.Steps) {
		t.Fatalf("steps %v, want %v", names, queue.Steps)
	}
	for i := range names {
		if names[i] != queue.Steps[i] {
			t.Fatalf("steps %v, want %v", names, queue.Steps)
		}
	}
	n := d.Steps[1]
	if d.JobID != id || d.State != "failed" || d.RerunOf != nil || d.CurrentStep == nil || *d.CurrentStep != "normalize" ||
		n.State != "failed" || n.Attempt != 1 || n.LastErrorCode == nil || *n.LastErrorCode != "events-mismatch" {
		t.Fatalf("detail %+v, normalize %+v", d, n)
	}

	rerun := decode(t, e.post(id, "rerun", "{}"))["job_id"].(string)
	if d, err := e.api.JobDetail(bg, rerun); err != nil || d.RerunOf == nil || *d.RerunOf != id {
		t.Fatalf("rerun detail %+v (%v), want rerun_of %s", d, err, id)
	}

	e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'purged', cleaned_at = now() WHERE job_id = $1`, id)
	if _, err := e.api.Erase(bg, queue.Actor{Kind: "admin", ID: "test-admin"}, id); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{id, queue.NewJobID(), "not-a-uuid"} {
		if _, err := e.api.JobDetail(bg, c); !errors.Is(err, ErrNotFound) {
			t.Errorf("JobDetail(%s) = %v, want ErrNotFound", c, err)
		}
	}
}
