# ADR-010 Postgres job queue

Status: accepted (2026-10-29)

## Context

Target size: about 10 BBB servers and 300 meetings a day. A Postgres task table with conditional-`UPDATE` claims,
leases and transactional completion is a common pattern; it needs no infrastructure beyond the
database the service needs anyway.

## Options

- a. Celery and a message broker.
- b. Postgres job and step tables.
- c. A workflow engine (Temporal).

## Decision

b. Jobs with linear steps; claims by conditional `UPDATE`, leases on the database clock, a reaper, per-step retry
with backoff, `held` and `failed` states, admin re-run, and step output plus completion in one transaction
([`architecture.md` §5–6](../reference/architecture.md)).

## Consequences

- The admin web UI and Prometheus `/metrics` read from these tables.
- A step can later be implemented by a worker in another language on the same table contract.
