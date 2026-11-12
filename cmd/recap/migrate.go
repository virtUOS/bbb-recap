package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/logging"
)

func runMigrate(args []string, stdout, stderr io.Writer) int {
	return migrate(args, os.Environ(), stderr)
}

// migrate applies the embedded migrations (deployment.md §6). Exit 2 on a
// config error, 1 when the database cannot be reached, 3 when a migration
// fails or an applied migration was changed.
func migrate(args, environ []string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "recap migrate: unexpected argument %q\n", args[0])
		return exitUsage
	}
	cfg, err := config.Load(environ, config.RoleMigrate)
	if err != nil {
		fmt.Fprintf(stderr, "recap migrate: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := logging.New(stderr, cfg.LogLevel)
	cfg.LogBoot(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, cfg.DBDSN, cfg.DBPassword)
	if err != nil {
		log.Error("cannot connect to the database", "err", err)
		return exitFailure
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, log); err != nil {
		log.Error("migration failed", "err", err)
		return exitMigrate
	}
	log.Info("database schema is up to date", "version", db.LatestVersion())
	return exitOK
}
