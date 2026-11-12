package db

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseMigrations(t *testing.T) {
	list, err := parseMigrations(fstest.MapFS{
		"0002_b.sql": {Data: []byte("SELECT 2;")},
		"0001_a.sql": {Data: []byte("SELECT 1;")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].version != 1 || list[1].version != 2 {
		t.Fatalf("got %+v, want versions 1, 2", list)
	}
}

func TestParseMigrationsDuplicateVersion(t *testing.T) {
	_, err := parseMigrations(fstest.MapFS{
		"0001_init.sql":  {Data: []byte("SELECT 1;")},
		"0002_jobs.sql":  {Data: []byte("SELECT 2;")},
		"0002_audit.sql": {Data: []byte("SELECT 3;")},
	})
	if err == nil || !strings.Contains(err.Error(), "0002_audit.sql") || !strings.Contains(err.Error(), "0002_jobs.sql") {
		t.Fatalf("got %v, want an error naming both 0002 files", err)
	}
}

func TestParseMigrationsBadName(t *testing.T) {
	for _, name := range []string{"init.sql", "0000_zero.sql", "0003_x.txt"} {
		if _, err := parseMigrations(fstest.MapFS{name: {Data: []byte("x")}}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestEmbeddedMigrationsParse(t *testing.T) {
	if _, err := migrations(); err != nil {
		t.Fatal(err)
	}
}
