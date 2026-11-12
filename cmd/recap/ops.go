package main

import (
	"context"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/ops"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// opsAddr is the ops listener address (ops.Addr).
var opsAddr = ops.Addr

// startOps starts the ops listener with the registry of the role, right
// after the configuration is loaded and before the database is open, so
// /healthz answers while /readyz says starting. /readyz answers 503 as soon
// as ctx ends (architecture.md §9).
func (d *deps) startOps(ctx context.Context, role config.Role) error {
	d.metrics = metrics.NewWorkerRegistry()
	if role == config.RoleAPI {
		d.metrics = metrics.NewAPIRegistry()
	}
	d.ops = ops.New(d.log, d.metrics, ctx.Done())
	addr, err := d.ops.Start(opsAddr)
	if err != nil {
		return err
	}
	d.log.Info("ops listening", "addr", addr.String())
	return nil
}

// opsReady turns on the readiness check and, for api, the DB gauges, once
// the database is open.
func (d *deps) opsReady(role config.Role) {
	d.ops.SetCheck(ops.Dependencies(d.pool, d.blobs))
	if role == config.RoleAPI {
		d.metrics.MustRegister(metrics.NewJobsCollector(d.pool, queue.Steps, d.log))
	}
}
