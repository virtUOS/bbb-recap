# Architecture

How the service is put together: components, trust zones, HTTP surface, job lifecycle, queue, blob store, health
and metrics. Names, states, enums and the adapter error table are defined in [`README.md`](README.md) and not repeated
here. Decisions: ADR-002, ADR-007, ADR-009 to ADR-012 in [`../decisions/`](../decisions/).

Other reference files own the ingest wire protocol, package, registration, identity, adapters, format, sink, retention
and config (README "Files"). Scale target: about 10 BBB servers and 300 meetings per day (ADR-010); nothing here is
built for more.

## 1. Components

| Component             | Runs where            | Binary                                                                                  | Job                                                                                                |
| --------------------- | --------------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| post_archive hook     | BBB server            | Ruby script                                                                             | checks opt-in and marks, writes a spool entry; no network (`bbb-exporter.md` §3)                   |
| exporter              | BBB server            | `recap-exporter`                                                                        | builds the package, pushes it, deletes raw data per policy (`bbb-exporter.md`)                     |
| front-end integration | front end, e.g. PILOS | change in the front end (not in this repository)                                        | sets `meta_recap-*`, registers the owner (ADR-005, ADR-006)                                        |
| `api`                 | service               | `recap api`                                                                             | ingest, registration, admin API, admin UI. Never calls providers or sinks                          |
| `worker`              | service               | `recap worker`                                                                          | runs steps, reaps expired leases, runs the cleanup loop. Only component that calls ASR, LLM, Docs  |
| `migrate`             | service               | `recap migrate`                                                                         | one-shot schema migration (`deployment.md` §6)                                                     |
| Postgres, blob store  | service               | upstream; filesystem volume (compose) or an existing S3 (Kubernetes, operators with S3) | queue, state, registrations, audit (ADR-010); package files and step outputs (ADR-011)             |
| ASR, LLM, Docs        | external              | per provider or sink profile                                                            | whisperx-api-server, vLLM (ADR-008); Docs `create-for-owner` with a server-to-server key (ADR-007) |

One image, sub-commands per role (README "Configuration, processes, metrics"). `api` and `worker` scale independently.
All coordination goes through Postgres; replicas never talk to each other.

## 2. Trust zones and exposure

```
 Zone 1  BBB server               Zone 2  Front end (PILOS)       Zone 5  Admin (browser or curl)
   hook ─spool─> exporter            create(meta_recap-*)               │ (G) HTTPS
                    │ (A) HTTPS push    │ (B) HTTPS registration         │
                    ▼                   ▼                                ▼
 Zone 3  Service ───────────────────────────────────────────────────────────────────
   public :8080  /api/v1/… , /admin/              ops :9090  /healthz /readyz /metrics
   api ──┬── Postgres ──┬── worker ─(C) audio ─────────► ASR        Zone 4
         └── Blob store ┘          ─(D) transcript ────► LLM        external
                                   ─(E) summary, owner ► Docs
```

- BBB traffic is outbound only (ADR-002). Only the public port is reachable from outside, through the operator's TLS
  proxy or ingress; the ops port is cluster-internal.
- Postgres and the blob store are reachable only from `api`, `worker`, `migrate`. Only `worker` has egress to zone 4;
  `api` only to the OIDC provider (admin UI).
- Every client has its own bearer credential (README "HTTP"); the caller's ID comes from it, never from the body.

## 3. Data flows

Each flow states the personal data it carries, where it is stored, for how long and who deletes it. Defaults from
ADR-009; [`privacy.md`](privacy.md) owns the full inventory.

| #   | Flow                           | Personal data                                                                                              | Stored                                                         | Retention (default)                                                                                                                    | Deleted by                                     |
| --- | ------------------------------ | ---------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------- |
| A   | exporter → `api` ingest        | voices, names, user IDs, chat, polls, notes, slides (in `events.xml`, `audio/`, `notes/`, `presentation/`) | blob store `jobs/<job_id>/…`; DB holds IDs, hashes, sizes only | until the job's `cleanup` (right after delivery); held/failed jobs until an admin acts; uncommitted uploads `RECAP_INGEST_STAGING_TTL` | `cleanup` step, `purge`, cleanup loop          |
| B   | front end → `api` registration | owner `sub`, email                                                                                         | DB `registrations`                                             | until the job's `cleanup`; unmatched rows `RECAP_REGISTRATION_TTL`; cancel tombstones (IDs only) 30 days                               | `cleanup` step, cleanup loop                   |
| —   | step outputs                   | normalized events, ASR results, transcript, summary, `summary.md`, `transcript.vtt`                        | blob store, same job prefix                                    | same as A. `transcript.vtt` is not delivered by any sink yet (ADR-007)                                                                 | `cleanup` step                                 |
| C   | `worker` → ASR                 | audio per track                                                                                            | provider                                                       | as the profile declares (ADR-008)                                                                                                      | provider; adapter cleanup call where available |
| D   | `worker` → LLM                 | transcript, names, chat, polls, notes                                                                      | provider                                                       | as the profile declares                                                                                                                | provider                                       |
| E   | `worker` → Docs                | `summary.md`, title (meeting name + date), owner `sub` and email                                           | Docs                                                           | owner manages the document                                                                                                             | document owner                                 |
| G   | admin → `api`                  | admin name or `sub` in `audit`                                                                             | DB `audit`                                                     | as job metadata (`RECAP_JOB_METADATA_RETENTION`)                                                                                       | cleanup loop                                   |
| —   | job metadata after cleanup     | IDs, states, timestamps, error codes, Docs document ID                                                     | DB                                                             | `RECAP_JOB_METADATA_RETENTION` (180 d)                                                                                                 | cleanup loop                                   |
| —   | logs, metrics                  | none beyond IDs (README "Logging"); metric labels are config names only                                    | operator's systems                                             | operator                                                                                                                               | operator                                       |
| —   | DB backups                     | DB content at backup time                                                                                  | backup storage                                                 | `deployment.md` §9                                                                                                                     | operator                                       |

## 4. HTTP surface

Paths, auth and errors: README "HTTP". `worker` serves only the ops port. Admins use the API (e.g. with curl) or the admin UI (§11).

| Path                                                                                     | Port   | Caller                       | Reference                                                                                   |
| ---------------------------------------------------------------------------------------- | ------ | ---------------------------- | ------------------------------------------------------------------------------------------- |
| `PUT\|GET /api/v1/ingest/packages/{package_id}`, `PUT …/files/{sha256}`, `POST …/commit` | public | `bbb_server`                 | `bbb-exporter.md` §6; guarantees §4.1                                                       |
| `PUT\|GET\|DELETE /api/v1/registrations/{record_id}`                                     | public | `frontend`                   | `metadata-interface.md` §2                                                                  |
| `GET\|DELETE /api/v1/registrations/{record_id}/summary`                                  | public | `frontend`                   | `metadata-interface.md` §2.7 (ADR-014)                                                      |
| `GET /api/v1/admin/jobs[/{job_id}]`                                                      | public | admin                        | filters `state`, `held_reason`, `outcome`, `bbb_server_id`, time; cursor paging; body below |
| `POST /api/v1/admin/jobs/{job_id}/{action}`                                              | public | admin                        | §5.4                                                                                        |
| `GET /api/v1/admin/audit`                                                                | public | admin                        | filter by `job_id`                                                                          |
| `/admin/`                                                                                | public | admin (OIDC)                 | §11                                                                                         |
| `GET /healthz`, `/readyz`, `/metrics`                                                    | ops    | kubelet, compose, Prometheus | §9, §12                                                                                     |

Admin job list: filters `state`, `held_reason`, `outcome`, `bbb_server_id`, `created_after`, `created_before` (RFC 3339);
newest first; `limit` (default 50, max 200); opaque `cursor` / `next_cursor`. Audit list: `{"entries": [{at, actor_kind,
actor_id, action, job_id, detail}], "next_cursor"}`, same paging, filter `job_id`.

Admin job body (one job; the list returns `{"jobs": [...], "next_cursor": string|null}`): top-level `job_id`,
`record_id`, `meeting_id`, `bbb_server_id`, `state`, `held_reason`, `failure_reason`, `outcome`, `detail` (a code such
as `server-not-allowed` or `meeting-id-mismatch`), `delivery` (`{"state": "sending"|"sent"|"confirmed_by_admin",
"document_id": string|null}` or `null`), `created_at`, `updated_at`. Values that do not apply are `null`. `record_id`, `meeting_id` and `bbb_server_id` are
source details shown to operators (ADR-013); pipeline code does not use them.
No personal data (no owner, no meeting name). `POST …/{action}` takes `{}` (or `{"document_id": …}` for `release` of
`delivery_uncertain`) and answers `200` with the job body (for `rerun`: the new job). A `document_id` on any other action is 400
`invalid-request`. An admin action writes only its own audit row for the job it acts on, no extra `job_state` row; `rerun`'s new
job also gets its `job_state` `received` row.

### 4.1 Ingest: service-side guarantees

- **Manifest `PUT` checks:** `package_id` = SHA-256 of the canonical manifest; schema valid; caller's `bbb_server_id`
  = manifest's (else `403 server-mismatch`); no breakout; size within limits (`meeting-package.md` §8); opt-in is the
  literal `true`, else `422 not-opted-in` and **nothing is stored**.
- **Staging:** until commit, the package is a `packages` row (`receiving`/`complete`), its `package_files` rows and its
  blobs. The `job_id` is allocated at the first manifest `PUT`, so blobs land directly under `jobs/<job_id>/…`. **No
  `jobs` row exists yet.** The cleanup loop deletes staging idle longer than `RECAP_INGEST_STAGING_TTL`.
- **Atomic commit:** `POST …/commit` checks that every manifest hash is stored and verified, then in **one
  transaction** marks the package `committed`, inserts the job (`received`) and its steps, and writes an audit row.
  The `IngestAck` (with `job_id`) is sent only after that commit; only an ack lets the exporter delete raw data.
  Missing files → `409 package-incomplete`.
- **Not checked at ingest:** registration, meeting type, sink, LiveKit tracks. `validate` checks them, so a bad package
  becomes a visible held or failed job, not an endless exporter retry.
- **Idempotency** (key `package_id`):

| Situation                                                                  | Result                                                      |
| -------------------------------------------------------------------------- | ----------------------------------------------------------- |
| new `package_id`, no package for `(bbb_server_id, record_id)`              | `201`, `packages` row in `receiving`, list of missing files |
| same `package_id`, not committed                                           | resume: store only files not yet verified                   |
| commit or `GET`, already committed (job in any state, including `cleaned`) | the identical `IngestAck`; no new job, no reprocessing      |
| different `package_id`, same `(bbb_server_id, record_id)`                  | `409 record-already-ingested`                               |

Only `429` (ingest limit, `Retry-After`), `503` and network errors are retryable for the exporter.

## 5. Job lifecycle

### 5.1 Job states

States, outcomes, held and failure reasons: README "States and enums".

| From                                           | To                       | Trigger                                                                                                                                                                        |
| ---------------------------------------------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| —                                              | `received`               | ingest commit, or admin `rerun` (new job)                                                                                                                                      |
| `received`                                     | `running`                | first step claimed                                                                                                                                                             |
| `received`, `running`                          | `held`                   | a step returns `hold(reason)`                                                                                                                                                  |
| `held` (`no_registration`, `owner_unresolved`) | `running`                | automatic, in the same transaction as the front end's registration `PUT` that fixes it (`metadata-interface.md` §2)                                                            |
| `held`                                         | `running` or `delivered` | admin `release` (§5.4)                                                                                                                                                         |
| `held` (`delivery_uncertain`), `failed`        | `running`                | admin `retry`                                                                                                                                                                  |
| `running`                                      | `failed`                 | permanent outcome, or attempts exhausted (`retries_exhausted`)                                                                                                                 |
| `running`                                      | `delivered`              | `deliver` succeeded                                                                                                                                                            |
| `delivered`                                    | `cleaned` (`delivered`)  | `cleanup` succeeded                                                                                                                                                            |
| `delivered`                                    | `failed`                 | `cleanup` exhausted its attempts (`retries_exhausted`); an admin `retry`s or `purge`s                                                                                          |
| `received`, `running`, `held`, `failed`        | `cleaned` (`cancelled`)  | registration `DELETE` before `deliver` is claimed (so not for `delivery_uncertain`: `409 already-delivered`, `metadata-interface.md` §2.5), or a tombstone found by `validate` |
| `held`, `failed`                               | `cleaned` (`purged`)     | admin `purge`                                                                                                                                                                  |

`cleaned` is terminal. Job rows are removed only by the cleanup loop after `RECAP_JOB_METADATA_RETENTION`.

### 5.2 Steps

Linear and fixed (README "Steps"). The job pins `config_hash` at creation; config changes need a restart.
`config_hash` = SHA-256 over the raw bytes of the meeting-types, providers and sinks files (in that order, each
prefixed by its length as 8-byte big-endian), so `api` and `worker` compute the same value without parsing files they do not use.

| Step         | Output                                                                                                         | Hold / fail                                                                                                                                                                                                             |
| ------------ | -------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `validate`   | job columns `meeting_type`, `sink_profile`, `registration_id`                                                  | hold: `invalid_metadata`, `unknown_type`, `unknown_sink`, `no_registration`; fail: `events_mismatch` (incl. malformed XML), `unsupported_audio_bridge`, `manifest_metadata_mismatch`; tombstone → `cleaned`/`cancelled` |
| `normalize`  | normalized meeting JSON                                                                                        | an undecodable track becomes a gap (`track_unreadable`); unreadable optional inputs (notes, slides) are omitted                                                                                                         |
| `transcribe` | one ASR result per track (`step_items`)                                                                        | `all_tracks_failed` only if every track is a gap                                                                                                                                                                        |
| `assemble`   | transcript JSON                                                                                                | —                                                                                                                                                                                                                       |
| `summarize`  | summary JSON                                                                                                   | `rejected_by_provider` after re-split (`providers.md`)                                                                                                                                                                  |
| `render`     | `summary.md`, `transcript.vtt`                                                                                 | —                                                                                                                                                                                                                       |
| `deliver`    | `deliveries` row with Docs document ID                                                                         | hold: `owner_unresolved`, `delivery_uncertain`; fail: `rejected_by_sink`                                                                                                                                                |
| `cleanup`    | deletes blob prefix and owner data; the registration stays as result record until `RECAP_RESULT_TTL` (ADR-014) | —                                                                                                                                                                                                                       |

A step returns one outcome: `ok`, `transient`, `permanent` or `hold(reason)` (README table). `permanent_config` (ASR,
LLM) and sink `config` fail the job with `config_error`. `hold` puts the step back to `pending`. `cleanup` gets
`run_after = delivered_at + RECAP_RETENTION_AFTER_DELIVERY`.

### 5.3 Retries

Fixed constants, not config. Backoff `min(30s × 2^(n−1), 30m)`, ±20 % jitter, at least the provider's `Retry-After`
(capped at 30 m). Max attempts: 3 for `validate`, `normalize`, `assemble`, `render`, `cleanup`; 5 for `transcribe`,
`summarize`; 10 for `deliver` (only sink `not_sent` is retried). HTTP clients and SDKs do not retry on their own; the
queue is the only retry layer (ADR-008). The step stores the last error class and code, never a message with content.

### 5.4 Admin actions

README: `retry`, `release`, `rerun`, `purge`, `erase`. Each writes an audit row in the same transaction.
Wrong state → `409 invalid-job-state`.

| Action    | Allowed when                                      | Effect                                                                                                                                                                                                                                                                                             | Same job? |
| --------- | ------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------- |
| `retry`   | job `failed`, or `held` with `delivery_uncertain` | the failed or held step → `pending`, attempts reset, job `running`. Earlier outputs are kept. For `delivery_uncertain` the admin has checked Docs and found no document; `deliver` sends again                                                                                                     | yes       |
| `release` | job `held`                                        | `delivery_uncertain`: admin found the document in Docs; `deliver` is marked succeeded (`confirmed_by_admin`, optional Docs document ID), job `delivered`, `cleanup` scheduled. Any other reason: the held step runs again                                                                          | yes       |
| `rerun`   | job not `cleaned` (content still there)           | new job (`rerun_of`), package blobs copied server-side, starts at `validate`, takes over the registration (one registration has one active job). The old job then goes to `cleaned` (`purged`) without touching the registration. A tombstone still cancels the rerun. Creates a new Docs document | no        |
| `purge`   | job `held` or `failed`                            | runs `cleanup` only; job `cleaned`, outcome `purged`                                                                                                                                                                                                                                               | yes       |
| `erase`   | job `cleaned`                                     | data-subject erasure of the job's remaining rows; returns the Docs document ID (`privacy.md` §5)                                                                                                                                                                                                   | —         |

With the default retention 0 a delivered job is cleaned at once, so `rerun` works only for held and failed jobs, or
with `RECAP_RETENTION_AFTER_DELIVERY` > 0.

## 6. Queue (Postgres)

Claim, lease, reaper and transactional completion follow ADR-010. Lease arithmetic uses the database clock `now()`
only.

### 6.1 Tables (outline; `internal/db/migrations/` is authoritative)

- `packages`: `package_id` PK, `job_id`, `bbb_server_id`, `record_id`, `meeting_id`, `state`, `total_bytes`,
  `last_activity_at`, `committed_at`; unique `(bbb_server_id, record_id)`. `package_files`: `(package_id, path)` PK,
  `role`, `size`, `sha256`, `verified_at` (drives resume).
- `jobs`: `job_id` PK (UUIDv7), `package_id`, `rerun_of`, `state`, `held_reason`, `failure_reason`, `outcome`,
  `current_step`, `meeting_type`, `sink_profile`, `registration_id`, `config_hash`, timestamps.
- `steps`: `(job_id, name)` PK, `state`, `attempt`, `run_after`, `owner`, `lease_expires_at`, `last_error_class`,
  `last_error_code`, `output` (blob keys, small non-personal facts). `step_items`: per-track progress of `transcribe`.
- `registrations` (personal data, tombstones too; shape in `metadata-interface.md`). `deliveries`: `job_id` PK,
  `sink_profile`, `state` (`sending`|`sent`|`confirmed_by_admin`), `document_id`, `sent_at`.
- `audit`: `at`, `actor_kind` (`system`|`admin`|`client`), `actor_id`, `action`, `job_id`, `detail` (codes only).
  `action`: the admin action names (`retry`, `release`, `rerun`, `purge`, `erase`), `ingest_commit`, (`detail` `received`), and
  `job_state` for every other job state change (`detail` = `<new state>` or `<new state>/<reason>`).
  `schema_migrations`: `deployment.md` §6.

### 6.2 Claim, lease, reaper

- **Lock order:** a transaction that touches both locks the `registrations` row before any `jobs` row.
- **Claim:** one `UPDATE … WHERE (job_id, name) = (SELECT … FOR UPDATE SKIP LOCKED LIMIT 1)` picks a step in
  `pending`/`retrying`, `run_after <= now()`, equal to `jobs.current_step`, job `received`/`running` (or `delivered` for
  `cleanup`), oldest job first.
  It sets `claimed`, `owner`, `attempt + 1`, `lease_expires_at = now() + 60s`. The outer `WHERE` repeats the state
  predicate, so a lost race returns zero rows (https://www.postgresql.org/docs/16/sql-select.html#SQL-FOR-UPDATE-SHARE).
- **Poll** every 2 s when idle, at once after a step ends; no `LISTEN/NOTIFY`. **Heartbeat** every 20 s, guarded by
  `owner` and `state = 'claimed'`; zero rows → cancel the step, it must not commit.
- **Reaper:** in the poll loop, at most every 15 s. Expired `claimed` steps → `retrying` with backoff (the attempt
  counts) or `failed` when used up. Guarded by `state = 'claimed' AND lease_expires_at < now()`, so safe everywhere.

### 6.3 Transactional completion

External calls and blob writes happen first. Then one transaction: mark the step `succeeded` `WHERE owner = <me> AND
state = 'claimed'` (zero rows → roll back, drop the result); write step rows (`jobs` columns, `step_items`,
`deliveries`); advance `current_step` and set the next `run_after`; update the job state and write an audit row.
Retry, fail and hold outcomes use the same guard.

### 6.4 Idempotency per step

| Step                                                       | Why a repeat is safe                                                                                                                                                                                                                                                                                                       |
| ---------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `validate`, `normalize`, `assemble`, `render`, `summarize` | deterministic on pinned inputs, blobs keyed by hash; for `summarize` only the committed output counts                                                                                                                                                                                                                      |
| `transcribe`                                               | done tracks are skipped (`step_items`). A timed-out request may still run on the provider; the adapter's cleanup call is used where the dialect has one                                                                                                                                                                    |
| `deliver`                                                  | before the request, a `deliveries` row in `sending` is committed. A `not_sent` result clears it before the retry. A step that finds `sending` without a result does not send again: it holds the job (`delivery_uncertain`). A crash, timeout or lost response never creates a silent duplicate (ADR-007; `sinks.md` §1.4) |
| `cleanup`                                                  | deleting what is already gone is success                                                                                                                                                                                                                                                                                   |

## 7. Concurrency

No global slot table.

- **Per worker process:** `RECAP_WORKER_SLOTS` steps at once (`deployment.md` §5.3).
- **Per provider or sink profile:** `max_inflight` is a limit **per worker process** (in-process semaphore). Total
  requests to one provider = worker processes × `max_inflight`; e.g. 2 workers and whisperx `max_inflight: 1` → at most
  2 ASR requests. Operators size `max_inflight` with that product in mind.
- **Ingest:** at most 8 concurrent uploads per `api` process; `429` beyond.
- **Fairness:** FIFO by job creation.

## 8. Blob store

ADR-011. The interface lives in the domain package; vendor SDKs are imported only by the backend, never by domain
code.

- **Operations:** `Put(job_id, sha256, size, stream)` hashes while streaming (mismatch → delete, `ErrHashMismatch`;
  existing key → verify, succeed). A concurrent `Put` of the same key never removes or replaces an object another
  `Put` wrote correctly. `Get(key)` **verifies the hash on read**. `Copy` (server-side, for `rerun`),
  `DeletePrefix(job_id)` (missing = success), `ListJobs` (orphan search). Errors: not found, hash mismatch, transient.
- **Keys:** `jobs/<job_id>/sha256/<hex>`. A blob's role lives in the DB, never in the key. The `frontend` sink's summary uses `results/<job_id>/summary.md` (ADR-014); the orphan sweep (3) covers `jobs/` only.
- **S3:** one bucket, path-style addressing, multipart above 64 MiB. Read-side verification is mandatory (see Known
  limitations for `x-amz-checksum-sha256`). Writes are conditional: a new key with `If-None-Match: *`, a corrupt object
  is replaced with `If-Match` on its ETag
  ([AWS S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html); MinIO
  honours both, tested with the dev MinIO). If the condition fails, the object another `Put` wrote is verified instead.
  After a mismatch, `Put` removes the object only while it still has the ETag of its own upload. A server that ignores
  the conditional headers gets plain writes. Ceph RGW checks both headers on `PutObject` when it commits the object
  (ceph/ceph `src/rgw/rgw_rest_s3.cc:2796-2797`, `src/rgw/driver/rados/rgw_rados.cc:3296` @ v20.2.5; also read in
  v18.2.7 and v19.2.3), and on `CompleteMultipartUpload` only from v20 (`rgw_rest_s3.cc:4614-4615` @ v20.2.5): on
  older releases objects above 64 MiB are written unconditionally. **Filesystem:** same layout under `RECAP_FS_ROOT`;
  temp file, fsync, rename; the compose default (one host, one shared volume), not for Kubernetes (ADR-011).
- **Cleanup loop:** one loop in every worker, hourly, every delete guarded so parallel runs are safe. It removes
  (1) staging idle longer than `RECAP_INGEST_STAGING_TTL`; (2) unmatched registrations past `RECAP_REGISTRATION_TTL`
  and result records and `frontend` summaries past `RECAP_RESULT_TTL` (ADR-014)
  and tombstones older than 30 days; (3) blob prefixes older than one day whose job has no row or is `cleaned` (failed
  commits, DB restores); (4) `cleaned` jobs with their steps, deliveries and audit rows older than
  `RECAP_JOB_METADATA_RETENTION`.

## 9. Graceful shutdown and health

HTTP servers use `ReadHeaderTimeout` 10 s (slow-header protection).

On SIGTERM or SIGINT, with a fixed 25 s budget (below the 30 s grace period, `deployment.md` §4):

- `api`: `/readyz` returns 503 at once, stop accepting, finish in-flight requests, exit 0. Uploads resume later.
- `worker`: stop claiming; let running steps finish; then cancel them and release this instance's claimed steps in one
  statement (`pending`/`retrying`, `attempt − 1`, `owner = NULL`). A clean stop costs no attempt; a hard kill costs
  one. A `deliver` cancelled after its `sending` row was committed ends as `delivery_uncertain`. Exit 0.

`/healthz`: process alive. `/readyz`: DB ping, schema version compatible, blob store reachable (cached 5 s). A worker
that loses the DB keeps retrying and reports not ready; it does not exit.

## 10. Sizing

~300 meetings/day ≈ 12/h, with peaks when lectures end. Estimates, not measured (see Known limitations): peak
~50 packages in 30 min; package 20–200 MB, typical ~50 MB (Opus voice ~14 MB per track-hour, marked periods only);
ingest ~15 GB/day, peak ~11 Mbit/s; blob store ≥ 100 GB (in-flight plus held/failed); DB < 1 GB/year; `api` 2 × (0.25
CPU, 256 MiB), `worker` 2 × 4 slots × (0.5 CPU, 512 MiB). ASR capacity is the bottleneck, not the service;
`recap_step_oldest_ready_age_seconds{step="transcribe"}` shows whether the backlog drains between peaks.

## 11. Admin web UI

- Server-rendered HTML in `api` under `/admin/`, on top of the admin API. Views: job list, job detail (steps,
  attempts, error codes, held reason, Docs document ID, `rerun_of`), audit. Actions: §5.4, each confirmed.
- Never shows transcript, summary, audio, names or owner email.
- Login: OIDC code flow with PKCE against the organisation's OIDC provider (e.g. Keycloak); role value `recap-admin`
  in the claim named by `RECAP_OIDC_ROLE_CLAIM`. Enabled by setting `RECAP_OIDC_ISSUER` (`deployment.md` §5.2). Session cookie `Secure`, `HttpOnly`, `SameSite=Lax`; CSRF token on POST.

## 12. Metrics

Prefix `recap_`. No job, meeting, record, document or user IDs as labels. DB gauges are computed by `api` on scrape
(cached 15 s); alert rules use `max without(instance)`. Plus `recap_build_info{version,commit}`.

| Metric                                | Type      | Labels                                                                                               | Alert                                                      |
| ------------------------------------- | --------- | ---------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `recap_jobs`                          | gauge     | `state`                                                                                              | — (overview)                                               |
| `recap_jobs_held`                     | gauge     | `reason`                                                                                             | `> 0` for 15 min; `delivery_uncertain > 0` at once         |
| `recap_jobs_failed`                   | gauge     | `reason`                                                                                             | `> 0`                                                      |
| `recap_step_oldest_ready_age_seconds` | gauge     | `step`                                                                                               | `> 1800`                                                   |
| `recap_cleanup_overdue_jobs`          | gauge     | —                                                                                                    | `> 0` (retention not met)                                  |
| `recap_step_runs_total`               | counter   | `step`, `outcome` (`ok`, `transient`, `permanent`, `hold`)                                           | rising `permanent`                                         |
| `recap_step_duration_seconds`         | histogram | `step`                                                                                               | — (sizing)                                                 |
| `recap_lease_expired_total`           | counter   | `step`                                                                                               | increase (crashing workers)                                |
| `recap_external_requests_total`       | counter   | `profile`, `kind` (`asr`, `asr_cleanup`, `llm`, `sink`), `outcome`                                   | error ratio; any `asr_cleanup` failure (`privacy.md` §5.2) |
| `recap_ingest_requests_total`         | counter   | `bbb_server_id`, `outcome` (`ok`, an ingest or manifest problem slug, else `other`; 401 not counted) | error ratio per server                                     |
| `recap_dependency_up`                 | gauge     | `dependency` (`postgres`, `blobstore`)                                                               | `== 0`                                                     |

## 13. Logging

README "Logging" applies; `privacy.md` §6 has the full rules and tests. Request bodies of ingest, registration and
admin calls are never logged. Provider and sink error bodies are reduced to an allow-listed code before logging.

## 14. Contract tests

- Claim race: N workers, one step, one winner. A reaped step's late completion commits nothing. Clean shutdown costs
  no attempt; a hard kill does. Backoff and max attempts per step; every row of the README adapter table.
- Hold leaves the step `pending`; a late registration releases `no_registration` in the same transaction;
  registration `DELETE` before `deliver` → `cleaned`/`cancelled`; `purge` → `cleaned`/`purged`.
- `deliver`: timeout, lost response and crash after `sending` each end in `delivery_uncertain` with at most one request
  sent; `release` marks it delivered; `retry` sends once more; `rerun` creates a new job.
- Ingest idempotency table (§4.1); ack never before job commit (fault injection); not-opted-in stores nothing.
- Blob `Get` detects corruption; `DeletePrefix` removes exactly one job's objects (both backends). `cleanup` removes
  blobs and registration; job metadata keeps no personal columns.
- Two worker processes with `max_inflight: 1` never exceed 2 concurrent requests to one fake provider.
- SIGTERM: `/readyz` 503 at once; claimed steps released within 25 s.

## Known limitations

- S3 servers: support for `x-amz-checksum-sha256` (MinIO and others) is not checked. Reads are verified by the service
  either way. Server-side encryption and a bucket lifecycle rule as a backstop for `jobs/` are the operator's choice.
- Sizing (§10) and the request and limit values in `deployment.md` §4 are estimates, not measurements.
