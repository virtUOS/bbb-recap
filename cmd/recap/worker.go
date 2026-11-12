package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/ops"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

func runWorker(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "recap worker: unexpected argument %q\n", args[0])
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return worker(ctx, os.Environ(), stderr)
}

// stepHandlers lists the step hook functions, one file each. A hook that
// returns a nil handler leaves its step unclaimed by this worker.
var stepHandlers = map[string]func(*deps) (queue.Handler, error){
	"validate":   validateHandler,
	"normalize":  normalizeHandler,
	"transcribe": transcribeHandler,
	"assemble":   assembleHandler,
	"summarize":  summarizeHandler,
	"render":     renderHandler,
	"deliver":    deliverHandler,
	"cleanup":    cleanupHandler,
}

// worker runs pipeline steps until ctx ends, then releases its claimed steps
// (architecture.md §9).
func worker(ctx context.Context, environ []string, stderr io.Writer) int {
	d, code := openDeps(ctx, config.RoleWorker, environ, stderr)
	if d == nil {
		return code
	}
	defer d.close()
	d.cfg.Providers.LogWarnings(d.log)

	handlers := map[string]queue.Handler{}
	for name, hook := range stepHandlers {
		h, err := hook(d)
		if err != nil {
			d.log.Error("cannot set up step", "step", name, "err", err)
			return exitFailure
		}
		handlers[name] = h
	}
	// The cleanup loop ends with ctx; the pool is closed only after it
	// has returned (deferred d.close above runs last).
	var loops sync.WaitGroup
	defer loops.Wait()
	if loop := cleanupLoop(d); loop != nil {
		loops.Go(func() { loop(ctx) })
	}

	owner := queue.NewOwnerID()
	d.log.Info("worker started", "owner", owner, "slots", d.cfg.WorkerSlots)
	queue.New(d.pool, queue.Config{
		Owner:                  owner,
		Slots:                  d.cfg.WorkerSlots,
		RetentionAfterDelivery: d.cfg.RetentionAfterDelivery,
		Handlers:               handlers,
		Log:                    d.log,
		// Run returns within Drain + queue.StopTime; PoolCloseTime is left
		// for closing the pool (architecture.md §9: 18 + 5 + 2 = 25 s).
		Drain: ops.ShutdownBudget - ops.PoolCloseTime - queue.StopTime,
	}).Run(ctx)
	return exitOK
}
