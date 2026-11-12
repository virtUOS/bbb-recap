# Reference: index and shared conventions

The reference documents state the exact rules of bbb-recap: interfaces, formats, configuration and privacy. Start
with the [overview](../overview.md) for the big picture. Every reference document uses the names below; where one
differs, this file wins. Decisions and their reasons are in [`../decisions/`](../decisions/); facts about BBB, Docs,
PILOS and providers, with sources, are in [`upstream-facts.md`](upstream-facts.md). Code cites these documents as
`docs/reference/<file>.md §n`.

## Files

| File                                                                                  | Content                                                                                                             | ADRs                    |
| ------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- | ----------------------- |
| [`architecture.md`](architecture.md)                                                  | components, trust zones, service HTTP surface, job lifecycle, step model, queue, blob store, admin actions, metrics | 002, 009, 010, 011, 012 |
| [`meeting-package.md`](meeting-package.md) + `schemas/manifest.schema.json`           | package layout, manifest, file roles, validation rules                                                              | 001, 004                |
| [`bbb-exporter.md`](bbb-exporter.md)                                                  | hook, spool, exporter daemon, exporter states, ingest protocol (wire), raw deletion, pull-API outline               | 002, 003                |
| [`metadata-interface.md`](metadata-interface.md) + `schemas/registration.schema.json` | `meta_recap-*` keys, registration API, held reasons                                                                 | 005                     |
| [`identity-mapping.md`](identity-mapping.md)                                          | owner identity, PILOS mapping, later participant map                                                                | 006, 007                |
| [`providers.md`](providers.md) + `schemas/transcript.schema.json`                     | ASR and LLM adapters, provider profiles, transcript schema                                                          | 008                     |
| [`templates.md`](templates.md)                                                        | built-in output format, selection by meeting type                                                                   | 005, 008                |
| [`sinks.md`](sinks.md)                                                                | sink interface, Markdown dialect, Docs sink (`create-for-owner`)                                                    | 006, 007, 009, 010      |
| [`deployment.md`](deployment.md)                                                      | images, compose, K8s, config contract, migrations, exporter installation                                            | 010, 011, 012           |
| [`privacy.md`](privacy.md)                                                            | data inventory, flows, retention, deletion, logging rules                                                           | 002, 004–009, 011       |
| [`fixtures.md`](fixtures.md)                                                          | synthetic meetings, mutation set, fixture secrets, golden files                                                     | all                     |
| [`upstream-facts.md`](upstream-facts.md)                                              | facts about BBB, La Suite Docs, front ends and providers, with sources                                              | —                       |

## Identifiers

| Name                           | Definition                                                                                                                                                     |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `meeting_id`                   | external meeting ID from the front end's `create`; consistency check only                                                                                      |
| `record_id`                    | BBB `internalMeetingID` from the `create` response = `raw/<record_id>` = `/recording/@meeting_id` in events.xml. **Join key** between package and registration |
| `bbb_server_id`, `frontend_id` | configured names, `^[a-z0-9][a-z0-9-]{0,62}$`, each bound to one credential                                                                                    |
| `package_id`                   | SHA-256 (lowercase hex) of the RFC 8785 (JCS) canonical `manifest.json`                                                                                        |
| `job_id`                       | UUIDv7. A `rerun` creates a new `job_id` (`rerun_of` links it); `retry` keeps it                                                                               |
| Docs document ID               | returned by Docs' `create-for-owner`; stored in the job's delivery record. New documents come only from `rerun`                                                |
| Outputs (`render`)             | `summary.md` (delivered to the sink), `transcript.vtt` (WebVTT; produced, **not delivered yet**, kept until cleanup)                                           |
| Format ID                      | `meeting-summary` (built in), selected through the meeting type named in `meta_recap-type`                                                                     |
| Transcript refs                | `spk-<n>`, `trk-<n>`, `seg-<nnnnnn>`                                                                                                                           |

## Grammars

- Meta prefix: `^[a-z][a-z0-9]{1,15}$`, default `recap`, configurable in exporter and service.
- Names (meeting types, sink profiles, provider profiles): `^[a-z][a-z0-9-]{0,62}$`.
- `record_id`: `^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$` (manifest schema). The hook rejects any other `-m` value as an
  error row; `hook_version` is the exporter package version.
- `meta_recap-enabled`: only the literal `true` opts in. Absent or any other value = not opted in, silently ignored (no failure).

## States and enums

- **Job states:** `received`, `running`, `held`, `failed`, `delivered`, `cleaned`. A `cleaned` job carries
  `outcome`: `delivered`, `cancelled` (registration cancelled), `purged` (admin).
- **Held reasons:** `invalid_metadata`, `unknown_type`, `unknown_sink`, `no_registration`, `owner_unresolved`,
  `delivery_uncertain` (sink outcome unknown, e.g. timeout; an admin checks Docs, then `release` or `retry`). A registration `PUT` releases `no_registration` and `owner_unresolved` automatically.
- **Job failure reasons:** `unsupported_audio_bridge`, `events_mismatch`, `manifest_metadata_mismatch` (these three come
  from the BBB source's `validate` step; another source adds its own, ADR-013),
  `retries_exhausted`, `all_tracks_failed`, `config_error` (adapter `permanent_config` / sink `config`),
  `rejected_by_provider` (LLM refusal after re-split), `rejected_by_sink`, `internal_error` (an earlier step's stored
  output is unreadable: a bug or corruption).
- **Steps (linear):** `validate`, `normalize`, `transcribe` (per-track results), `assemble` (transcript JSON),
  `summarize` (summary JSON), `render` (`summary.md`, `transcript.vtt`), `deliver`, `cleanup` (`run_after = delivered_at + retention`).
- **Step states:** `pending`, `claimed`, `succeeded`, `retrying`, `failed`.
- **Step outcomes:** `ok`, `transient` (retry with backoff), `permanent` (job `failed`), `hold` (job `held`).
- **Adapter error classes → step outcome:**

  | Adapter | Class              | Outcome                                                                                     |
  | ------- | ------------------ | ------------------------------------------------------------------------------------------- |
  | ASR     | `retryable`        | transient; when the track's attempts are exhausted → ok, track becomes a gap (`asr_failed`) |
  | ASR     | `permanent_input`  | ok, track becomes a gap (`asr_rejected_input`)                                              |
  | ASR     | `permanent_config` | permanent                                                                                   |
  | ASR     | `invalid_response` | transient (bounded); then ok, track becomes a gap (`asr_invalid_response`)                  |

  Per ADR-008, a failing track never fails the job on its own. `transcribe` fails the job only when
  every track is a gap (`all_tracks_failed`). Gap reasons: `asr_failed`, `asr_rejected_input`, `asr_invalid_response`,
  `track_unreadable`.

  | Adapter | Class                                                                                                   | Outcome                                                                               |
  | ------- | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
  | LLM     | `retryable` / `invalid_response`                                                                        | transient (bounded)                                                                   |
  | LLM     | `output_truncated` / `input_too_large`                                                                  | handled inside `summarize` by re-splitting, then permanent                            |
  | LLM     | `permanent_config`                                                                                      | permanent                                                                             |
  | Sink    | `not_sent` (request provably not processed: DNS failure or connection refused before sending, HTTP 429) | transient                                                                             |
  | Sink    | `uncertain` (timeout, connection lost after sending, HTTP 5xx)                                          | hold (`delivery_uncertain`); **never retried automatically**, so no silent duplicates |
  | Sink    | `config` (401/403) / `rejected` (other 4xx)                                                             | permanent                                                                             |
  | Sink    | `recipient_unresolved` (Docs cannot match the owner, e.g. two users with that email)                    | hold (`owner_unresolved`)                                                             |

- **Admin actions:** `retry`, `release`, `rerun`, `purge`, `erase` (data-subject erasure in the service;
  Docs documents are reported to the Docs operator, since the service has no rights on them).
  - `retry`: re-run the current step of a `failed` job, or of a `held` job with `delivery_uncertain` (re-sends). Same `job_id`.
    Other holds are lifted with `release`.
  - `release`: lift a hold and continue. On `delivery_uncertain` it **marks the job delivered** without re-sending;
    the admin may add the Docs document ID found by hand.
  - `rerun`: new job from the same package (new documents). `purge`: delete content, job → `cleaned/purged`.
- **Exporter states per recording:** `ready`, `delivered`, `acked`, `deleted`, `failed`. Failure reasons:
  `no_events`, `no_microphone_tracks`, `unsupported_audio_bridge`, `limit_exceeded`,
  `source_changed`, `source_missing`, `rejected`, `conflict`, `unauthorized`.
- **Exporter delete policy:** `never` (default), `after_ack`; optional `RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS`.
  Deletion reasons: `after_ack`, `failure_expiry`, `manual`.
- **Ingest package status:** `receiving`, `complete`, `committed`. Same `(bbb_server_id, record_id)` with a new
  `package_id` → `409 record-already-ingested`.
- **Package file roles:** `events`, `audio-microphone`, `notes`, `presentation`. Breakout recordings are not exported.

## Units and time

- Offsets: integer milliseconds. Timestamps: RFC 3339 UTC. Sizes: bytes. Hashes: lowercase hex SHA-256.
- Meeting axis: events.xml `timestamp` minus the first `RecordStatusEvent` start. Displayed `[hh:mm:ss]` = recorded
  time (pauses collapsed, as in BBB playback).

## HTTP

- Public listener: everything under `/api/v1/`. Ops listener (`:9090`, not public): `/healthz`, `/readyz`, `/metrics`.
- Paths: ingest `/api/v1/ingest/packages/{package_id}[/files/{sha256}|/commit]` (wire details in `bbb-exporter.md`);
  registration `PUT|GET|DELETE /api/v1/registrations/{record_id}`; admin `/api/v1/admin/jobs[/{job_id}[/{action}]]`,
  `/api/v1/admin/audit`; admin UI `/admin/`.
- JSON bodies except file uploads. Errors: RFC 9457 with `type` = `urn:recap:problem:<slug>`; any unexpected server
  error is 500 `internal` (no details).
- Failure reasons for size limits: a rendered document over the sink's `max_document_bytes` is `rejected_by_sink`;
  summarize needing more than 3 reduce levels is `rejected_by_provider`.
- Orphan blobs: age from the UUIDv7 time of the `job_id` prefix. A job is cleanup-overdue when its `cleanup` step is
  pending more than 1 h after `run_after`. Provider and sink `base_url`s may be `http://` (operator
  choice, e.g. the contract environment); `https://` is the default in examples.
- Every client authenticates with a per-client bearer credential from `RECAP_CLIENT_SECRETS_DIR`
  (`bbb_server.<id>`, `frontend.<id>`, `admin.<name>`). A missing, unknown or wrong-kind credential is `401`.

## Schemas

- Every schema has `schema_version` (semver) and `$id` = `urn:recap:schema:<name>:<major>`. Contract tests pin the major.

## Configuration, processes, metrics

- One image `recap`, sub-commands `api`, `worker`, `migrate`, `check-config`, `healthcheck`, and `tool` (dev/test only:
  `validate-package`, `normalize`, `assemble`, `asr`, `llm`, `render`, `sink`; JSON in, JSON out; used by black-box contract tests). Exit codes 0, 1, 2 (config or usage), 3 (migration).
- Env vars `RECAP_*` (exporter: `RECAP_EXPORTER_*`); secrets only via `*_FILE`. Files: `RECAP_PROVIDERS_FILE`,
  `RECAP_SINKS_FILE`, `RECAP_MEETING_TYPES_FILE`, `RECAP_FORMATS_DIR`. Defaults: `RECAP_REGISTRATION_TTL=14d`,
  `RECAP_RETENTION_AFTER_DELIVERY=0`, `RECAP_JOB_METADATA_RETENTION=180d` (provisional), `RECAP_INGEST_STAGING_TTL=72h` (provisional).
- Profile concurrency field: `max_inflight` (provider and sink profiles), counted **per worker process**; total load on a
  provider = worker processes × `max_inflight`.
- Ports are fixed: public `:8080`, ops `:9090`. Delivery record states: `sending`, `sent`, `confirmed_by_admin`.
- Metrics prefix `recap_` (exporter `recap_exporter_`); no job, meeting, record, document or user IDs as labels.

## Logging

Structured JSON, one object per line, with keys `time` (RFC 3339), `level` (`DEBUG`, `INFO`, `WARN`, `ERROR`) and `msg`
(Go `log/slog` JSON handler defaults; tests compare `level` case-insensitively). IDs only at INFO and above; never transcript, summary, titles, names or emails. Never log URL
query strings or raw error bodies from providers or sinks at any level.

## Fixtures

IDs `F-NN` (meetings) and `M-NN` (mutations) under `testdata/`. Fixture secrets start with `FIXTURE-NOT-A-SECRET-`.
Hosts `*.recap.test`, emails `@example.org`.

## Rules for reference authors

- Cite ADRs; external facts need `path:line @ sha` or a URL (collected in `upstream-facts.md`). What was not checked
  goes into the file's "Known limitations".
- Each data flow states personal data, storage, retention and who deletes it.
- Each interface lists inputs, outputs, errors, idempotency, retries, and the contract tests that pin it.
- Keep file names and section numbers stable; code cites them.
