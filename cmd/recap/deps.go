package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/virtUOS/bbb-recap/internal/adapters/s3blob"
	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/ops"
)

// deps are what api and worker share: configuration, logger, ops listener
// and its metrics registry, database pool and blob store, each opened once.
// The route and step hook files get them.
type deps struct {
	cfg     *config.Config
	log     *slog.Logger
	ops     *ops.Server
	metrics *prometheus.Registry
	pool    *pgxpool.Pool
	blobs   blob.Store
	results blob.Results         // nil when the blob backend keeps no results (blob.Results)
	creds   *httpapi.Credentials // api only
}

// waitRetry is how long api and worker wait between tries while the
// database is unreachable or not migrated yet.
const waitRetry = 2 * time.Second

// openDeps loads the configuration for role, starts the ops listener and
// opens everything else. While the database is unreachable or its schema is
// older than this binary, it logs a warning and keeps waiting (not ready);
// it does not exit. It returns nil and an exit code on a configuration
// error or when the ops port cannot be opened, or exitOK when ctx ends while
// waiting. The ops listener answers 503 on /readyz once ctx ends.
func openDeps(ctx context.Context, role config.Role, environ []string, stderr io.Writer) (*deps, int) {
	prog := "recap " + string(role)
	cfg, err := config.Load(environ, role)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid configuration:\n%v\n", prog, err)
		return nil, exitUsage
	}
	log := logging.New(stderr, cfg.LogLevel)
	cfg.LogBoot(log)
	d := &deps{cfg: cfg, log: log}

	if role == config.RoleAPI {
		if d.creds, err = httpapi.LoadCredentials(cfg.ClientSecretsDir); err != nil {
			log.Error("cannot load client credentials", "err", err)
			return nil, exitUsage
		}
	}
	if d.blobs, err = openBlobs(cfg); err != nil {
		log.Error("cannot open the blob store", "err", err)
		return nil, exitUsage
	}
	d.results, _ = d.blobs.(blob.Results)
	if err := d.startOps(ctx, role); err != nil {
		log.Error("cannot open the ops listener", "err", err)
		return nil, exitFailure
	}

	for d.pool == nil {
		pool, err := db.Open(ctx, cfg.DBDSN, cfg.DBPassword)
		if err == nil {
			d.pool = pool
			break
		}
		log.Warn("database unreachable; retrying", "err", err)
		if !sleep(ctx, waitRetry) {
			d.close()
			return nil, exitOK
		}
	}
	for {
		ok, err := db.SchemaCompatible(ctx, d.pool)
		if ok {
			d.opsReady(role)
			return d, exitOK
		}
		if err != nil {
			log.Warn("cannot read the schema version; retrying", "err", err)
		} else {
			log.Warn("database schema is older than this binary; waiting for recap migrate",
				"want_version", db.LatestVersion())
		}
		if !sleep(ctx, waitRetry) {
			d.close()
			return nil, exitOK
		}
	}
}

func openBlobs(cfg *config.Config) (blob.Store, error) {
	if cfg.BlobBackend == "fs" {
		return blob.NewFS(cfg.FSRoot)
	}
	return s3blob.New(s3blob.Config{
		Endpoint:        cfg.S3Endpoint,
		Region:          cfg.S3Region,
		Bucket:          cfg.S3Bucket,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
	})
}

// close closes the pool, then the ops listener. It waits at most
// ops.PoolCloseTime for connections still in use, so a shutdown stays
// within its budget (architecture.md §9); the process exits right after
// anyway.
func (d *deps) close() {
	defer d.ops.Close()
	if d.pool == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		d.pool.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(ops.PoolCloseTime):
		d.log.Warn("database connections still in use at exit")
	}
}

// sleep waits for d and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
