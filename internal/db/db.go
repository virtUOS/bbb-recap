// Package db opens the Postgres pool and applies the embedded, forward-only
// migrations (docs/reference/deployment.md §6).
package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// lockKey is the advisory lock key held while migrating ("recap" in ASCII).
const lockKey = 0x7265636170

// LockWait is how long Migrate waits for another migrating process.
const LockWait = 5 * time.Minute

// ErrChecksumMismatch means an applied migration differs from the embedded one.
var ErrChecksumMismatch = errors.New("applied migration was changed")

// Open returns a connection pool for dsn. password comes from
// RECAP_DB_PASSWORD_FILE; an empty password keeps whatever dsn says.
func Open(ctx context.Context, dsn, password string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	if password != "" {
		cfg.ConnConfig.Password = password
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

type migration struct {
	version int
	name    string
	sql     string
	sha256  string
}

// migrations returns the embedded migrations ordered by version.
func migrations() ([]migration, error) {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	return parseMigrations(sub)
}

// parseMigrations reads the *.sql files of fsys. File names are
// NNNN_description.sql; two files with the same version are an error.
func parseMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var list []migration
	seen := map[int]string{}
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 || path.Ext(e.Name()) != ".sql" {
			return nil, fmt.Errorf("bad migration file name %q", e.Name())
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s have the same version %d", other, e.Name(), v)
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		list = append(list, migration{v, e.Name(), string(b), hex.EncodeToString(sum[:])})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	return list, nil
}

// LatestVersion is the highest embedded migration version: the schema this
// binary needs.
func LatestVersion() int {
	list, err := migrations()
	if err != nil || len(list) == 0 {
		return 0
	}
	return list[len(list)-1].version
}

// Migrate applies every embedded migration that is not in the ledger yet,
// each in its own transaction. It holds an advisory lock, so parallel runs
// are safe. A ledger entry whose sha256 differs from the embedded file
// returns ErrChecksumMismatch. Versions the binary does not know (a newer
// database) are left alone.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	list, err := migrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	lockCtx, cancel := context.WithTimeout(ctx, LockWait)
	defer cancel()
	if _, err := conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return fmt.Errorf("wait for migration lock: %w", err)
	}
	defer func() {
		// Unlock even when ctx was cancelled; closing the connection would also release it.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		sha256     text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}
	applied, err := ledger(ctx, conn)
	if err != nil {
		return err
	}
	for _, m := range list {
		if sum, ok := applied[m.version]; ok {
			if sum != m.sha256 {
				return fmt.Errorf("%w: %s", ErrChecksumMismatch, m.name)
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, sha256) VALUES ($1, $2)", m.version, m.sha256)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		log.Info("migration applied", "version", m.version, "name", m.name)
	}
	return nil
}

func ledger(ctx context.Context, conn *pgxpool.Conn) (map[int]string, error) {
	rows, err := conn.Query(ctx, "SELECT version, sha256 FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	applied := map[int]string{}
	var v int
	var sum string
	_, err = pgx.ForEachRow(rows, []any{&v, &sum}, func() error {
		applied[v] = sum
		return nil
	})
	return applied, err
}

// SchemaCompatible reports whether the database has at least the schema
// this binary needs. A newer database is fine (expand-only, deployment.md §6).
func SchemaCompatible(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var v int
	err := pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&v)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table: never migrated
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v >= LatestVersion(), nil
}
