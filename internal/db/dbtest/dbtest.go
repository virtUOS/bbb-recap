// Package dbtest gives tests a fresh Postgres database. It reads
// TEST_DATABASE_URL (deliberately not a RECAP_* name); without it the test
// is skipped.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/db"
)

// EnvVar names the variable with the URL of a Postgres server the tests may
// create and drop databases on.
const EnvVar = "TEST_DATABASE_URL"

// Empty creates an empty database, drops it when the test ends and returns
// its URL.
func Empty(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skipf("%s not set: skipping database test", EnvVar)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s: %v", EnvVar, err)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect %s: %v", EnvVar, err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	name := "recap_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		_ = admin.Close(ctx)
	})
	u.Path = "/" + name
	return u.String()
}

// Open opens a pool on dsn that is closed when the test ends.
func Open(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), dsn, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// New returns a pool on a fresh, migrated database that is dropped when the
// test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := Open(t, Empty(t))
	if err := db.Migrate(context.Background(), pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
