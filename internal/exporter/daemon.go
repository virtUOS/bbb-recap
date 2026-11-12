package exporter

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// Loop constants (§4.1, §4.3).
const (
	PollInterval  = 10 * time.Second
	ShutdownLimit = 20 * time.Second
)

// Daemon is one recap-exporter run, or the state access of an admin command.
type Daemon struct {
	cfg     *Config
	log     *slog.Logger
	store   *Store
	client  *client
	metrics *metrics
	now     func() time.Time
}

// NewDaemon prepares a daemon. token may be empty for admin commands, which
// never contact the service.
func NewDaemon(cfg *Config, token string, log *slog.Logger) *Daemon {
	d := &Daemon{
		cfg:     cfg,
		log:     log,
		store:   &Store{Dir: cfg.StateDir},
		client:  newClient(cfg.ServiceURL, token),
		metrics: newMetrics(),
		now:     time.Now,
	}
	d.client.now = func() time.Time { return d.now() }
	return d
}

// Run is `recap-exporter run`: it holds the state dir lock, serves metrics
// and health, and runs the loop until ctx is cancelled. After cancellation
// it returns within ShutdownLimit; every state change is already on disk,
// so an unfinished step is repeated by the next run (§4.3).
func (d *Daemon) Run(ctx context.Context) error {
	lock, err := Lock(d.cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	ln, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: d.metrics.handler(d.cfg.StateDir), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	d.log.Info("exporter started", "version", Version(), "delete_policy", d.cfg.DeletePolicy)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			d.loop(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(PollInterval):
			}
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(ShutdownLimit - 2*time.Second):
			d.log.Warn("shutdown limit reached, exiting with a step unfinished")
		}
	}
	d.log.Info("exporter stopped")
	return nil
}

// loop is one pass of §4.1: claim, build, upload and commit, delete, prune.
func (d *Daemon) loop(ctx context.Context) {
	d.claim()
	ids, err := d.store.RecordIDs()
	if err != nil {
		d.log.Error("state dir cannot be read")
		return
	}
	var infos []recordInfo
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		rec, err := d.store.Load(id)
		if err != nil {
			d.log.Error("state record cannot be read", "record_id", id)
			continue
		}
		if rec == nil {
			d.buildRecord(id)
			if rec, err = d.store.Load(id); err != nil || rec == nil {
				continue // dropped, or retried next loop
			}
		}
		if rec.State == StateReady || rec.State == StateDelivered {
			d.attempt(ctx, rec)
		}
		if ctx.Err() != nil {
			return
		}
		if reason, _ := d.deletion(rec, d.cfg.DeletePolicy, d.now()); reason != "" {
			if err := d.deleteRaw(rec, reason); err != nil {
				d.log.Error("raw dir cannot be deleted", "record_id", id, "state", rec.State)
				d.log.Debug("delete", "record_id", id, "err", err)
			}
		}
		if d.prune(rec, d.now()) {
			continue
		}
		infos = append(infos, d.info(rec))
	}
	d.metrics.set(infos, d.now())
}

func (d *Daemon) info(rec *Record) recordInfo {
	i := recordInfo{state: rec.State, since: rec.FirstAttemptAt}
	if b, err := d.store.Manifest(rec.RecordID); err == nil && b != nil {
		if m, err := parseManifest(b); err == nil {
			i.size = m.Totals.Size
		}
	}
	return i
}

// attempt pushes a ready or delivered record whose next_attempt_at has
// passed and records the outcome (§5.1, §6.5).
func (d *Daemon) attempt(ctx context.Context, rec *Record) {
	now := d.now()
	if !rec.NextAttemptAt.IsZero() && now.Before(rec.NextAttemptAt) {
		return
	}
	mb, err := d.store.Manifest(rec.RecordID)
	if err != nil || mb == nil || manifest.PackageID(mb) != rec.PackageID {
		d.log.Error("stored manifest missing or not matching package_id", "record_id", rec.RecordID,
			"package_id", rec.PackageID)
		return
	}
	start := now
	err = d.push(ctx, rec, mb)
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		return // shutting down: not an attempt, the next run repeats it
	}
	var pe *pushError
	if !errors.As(err, &pe) {
		pe = transient("push failed", err)
	}
	now = d.now()
	rec.LastAttemptAt = now.UTC()
	rec.LastError = &LastError{Type: pe.problem, Status: pe.status}
	if pe.reason != "" {
		rec.NextAttemptAt = time.Time{}
		rec.setState(StateFailed, pe.reason, now)
		if err := d.store.Save(rec); err != nil {
			d.log.Error("state cannot be saved", "record_id", rec.RecordID)
			return
		}
		d.log.Error("push failed permanently", "record_id", rec.RecordID, "package_id", rec.PackageID,
			"state", StateFailed, "reason", pe.reason, "status", pe.status, "problem", pe.problem)
		return
	}
	rec.Attempts++
	delay := retryDelay(rec.Attempts, pe.retryAfter, jitter())
	rec.NextAttemptAt = now.Add(delay).UTC()
	if err := d.store.Save(rec); err != nil {
		d.log.Error("state cannot be saved", "record_id", rec.RecordID)
		return
	}
	d.log.Warn("push failed, will retry", "record_id", rec.RecordID, "package_id", rec.PackageID,
		"state", rec.State, "attempts", rec.Attempts, "status", pe.status, "problem", pe.problem,
		"cause", pe.msg, "retry_in_s", int(delay.Seconds()), "duration_ms", now.Sub(start).Milliseconds())
	if pe.err != nil {
		d.log.Debug("push error", "record_id", rec.RecordID, "err", pe.err)
	}
}
