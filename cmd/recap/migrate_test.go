package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
)

// migrateEnv splits the test database URL into a DSN without password and a
// password file, as in production.
func migrateEnv(t *testing.T, dsn string) []string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	u.User = url.User(u.User.Username())
	file := filepath.Join(t.TempDir(), "db-password")
	if err := os.WriteFile(file, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"RECAP_DB_DSN=" + u.String(), "RECAP_DB_PASSWORD_FILE=" + file}
}

func TestMigrateCommand(t *testing.T) {
	dsn := dbtest.Empty(t)
	env := migrateEnv(t, dsn)
	for i := range 2 {
		var errOut bytes.Buffer
		if code := migrate(nil, env, &errOut); code != exitOK {
			t.Fatalf("run %d: exit code = %d, want 0; stderr:\n%s", i+1, code, errOut.String())
		}
	}

	pool := dbtest.Open(t, dsn)
	if _, err := pool.Exec(context.Background(), "UPDATE schema_migrations SET sha256 = repeat('0', 64)"); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	if code := migrate(nil, env, &errOut); code != exitMigrate {
		t.Fatalf("tampered ledger: exit code = %d, want %d; stderr:\n%s", code, exitMigrate, errOut.String())
	}
}

func TestMigrateCommandConfigError(t *testing.T) {
	var errOut bytes.Buffer
	env := []string{"RECAP_DB_DSN=postgres://recap:pw@127.0.0.1/recap"}
	if code := migrate(nil, env, &errOut); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}
