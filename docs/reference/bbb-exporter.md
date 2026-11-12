# BBB exporter: post_archive hook, exporter daemon, transports

The part of bbb-recap that runs on the BBB server: a small hook that decides whether a recording leaves the server,
and a daemon that builds the package and pushes it to the service. Decisions:
[ADR-001](../decisions/ADR-001-scope-livekit-bbb4.md) to [ADR-005](../decisions/ADR-005-metadata-interface.md),
[ADR-009](../decisions/ADR-009-privacy-retention.md), [ADR-012](../decisions/ADR-012-language-go.md). Package format:
[`meeting-package.md`](meeting-package.md). Names and enums: [`README.md`](README.md). BBB sources are
`bigbluebutton/bigbluebutton` at `7d27e7e` (v4.0.x-release) unless noted; see also
[`upstream-facts.md`](upstream-facts.md).

## 1. Overview

```
BBB sanity worker ─runs─▶ post_archive hook (Ruby, no network): reads events.xml, writes one spool entry
spool dir ─claimed by─▶ exporter daemon (Go)
                          build manifest (allow-list, SHA-256)  → ready
                          upload manifest + files               → delivered
                          commit; service verifies and acks     → acked
                          deletion policy                       → deleted
                          ▼ outbound HTTPS only: service ingest API
```

The package, the states (§5) and the deletion rules (§7) do not depend on the transport
([ADR-003](../decisions/ADR-003-exporter-contract-transport-neutral.md)). Transports: push (§6, implemented) and pull
(§9, not built).

## 2. Facts this design relies on

| Fact                                                                                                                                                                                                         | Source                                                                                                                                                                                                |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| post_archive scripts run in the **sanity** worker after a successful sanity check, every `*.rb` in sorted order, as `ruby <script> -m <meeting_id>`; a failing script only logs a warning                    | `workers/sanity_worker.rb:42-44,62`, `workers/base_worker.rb:70-89 @ 7d27e7e`                                                                                                                         |
| They run **only if the meeting has recording marks**; without marks archive writes `.norecord` and stops                                                                                                     | `workers/archive_worker.rb:45-58`, `archive.rb:285-301 @ 7d27e7e`                                                                                                                                     |
| Drop-in dir `/usr/local/bigbluebutton/core/scripts/post_archive`; raw dir `/var/bigbluebutton/recording/raw/<id>/`                                                                                           | `docs/docs/development/recording.md:479`, `archive.rb:210-211,239`, `bigbluebutton.yml:66 @ 7d27e7e`                                                                                                  |
| Meta values are attributes of `/recording/metadata` in `events.xml`, keys lowercased; read with `BigBlueButton::Events.get_meeting_metadata(events_xml)`                                                     | `events_archiver.rb:272-292`, `generators/events.rb:48-58`, `ParamsProcessorUtil.java:300-303 @ 7d27e7e`                                                                                              |
| Recording marks are `RecordStatusEvent` (module PARTICIPANT) with `userId`, `status`                                                                                                                         | `RecordStatusRecordEvent.scala:25-38 @ 7d27e7e`; the Opencast integration reads them the same way (`elan-ev/opencast-bigbluebutton-integration` PR #72, `post-archive/post_archive.rb:649 @ 233179d`) |
| Post scripts can run again (`bbb-record --rebuild`); breakouts inherit the parent's meta and get their own recording                                                                                         | `MeetingService.java:917-924 @ 7d27e7e`                                                                                                                                                               |
| The stock cron deletes `raw/<id>` 14 days after publish **only if** every format with a `process/*.rb` script has `status/published/<id>-<fmt>.done`; with all formats disabled the raw dir is never deleted | `bigbluebutton-config/cron.daily/bigbluebutton:29,94-123 @ 7d27e7e`                                                                                                                                   |
| `meta_bbb-disable-recording-formats` skips process/publish; sanity and post_archive still run                                                                                                                | `workers/base_worker.rb:121-166`, `customize.md:393 @ 7d27e7e`                                                                                                                                        |

## 3. post_archive hook

### 3.1 Inputs and outputs

- **Input:** `-m <record_id>`; reads `<RAW_DIR>/<record_id>/events.xml` and `/etc/recap-exporter/exporter.env`. It
  does not rely on the environment of BBB's recording worker.
- **Output:** at most one spool entry. Logs to stdout/stderr: `record_id` and the decision only, as JSON lines with
  the keys of [`README.md`](README.md) "Logging". Ruby libraries: `json` and `rexml`.
- **No network, no processes, no heavy work** (no hashing, copying or conversion;
  [ADR-002](../decisions/ADR-002-trigger-post-archive-push.md)). A static check rejects backticks, `%x`, `system`,
  `exec`, `spawn`, `fork`, `IO.popen`, `Open3` and `open("|…")` anywhere in the source, including strings and
  comments. (The Opencast integration, by contrast, converts and uploads inside its hook:
  `post-archive/post_archive.rb:524-531 @ 233179d`.)

Log decision codes besides those in §3.3: `invalid_record_id`, `config_unreadable`, `events_unreadable`,
`events_unparsable`, `error`, `spool`, `spool_exists`. `HOOK_VERSION` is set to the package version at build time; a
release build never ships `0.0.0-dev`.

### 3.2 What it reads from `events.xml`

Only `<prefix>-enabled`, the `<meeting>` attribute `breakout`, and whether any `RecordStatusEvent` exists (defence in
depth; `status` is not interpreted).

### 3.3 Decision table

| Condition (in order)                                                               | Hook action                                         |
| ---------------------------------------------------------------------------------- | --------------------------------------------------- |
| `/etc/recap-exporter/exporter.env` missing or unreadable                           | log ERROR, exit 1 (BBB logs a warning)              |
| `events.xml` missing or unparsable                                                 | log ERROR, exit 1 (BBB logs a warning)              |
| `<prefix>-enabled` is not the literal `true` ([`README.md`](README.md) "Grammars") | log INFO `not_opted_in`, exit 0, no spool entry     |
| breakout                                                                           | log INFO `breakout_skipped`, exit 0, no spool entry |
| no `RecordStatusEvent`                                                             | log WARN `no_marks`, exit 0, no spool entry         |
| otherwise                                                                          | write spool entry                                   |

The hook never changes `raw/`. Type, sink and version keys are checked by the service (held job,
[ADR-005](../decisions/ADR-005-metadata-interface.md)).

### 3.4 Spool entry

- `<SPOOL_DIR>/<record_id>.json`, written as `.tmp-<record_id>-<pid>`, fsynced, renamed. An existing entry for the same
  `record_id` is left alone and logged, so a `--rebuild` run does nothing.
- Content: `spool_version` (`1`), `record_id`, `hooked_at` (RFC 3339 UTC), `hook_version`. No metadata.
- Data flow: no personal data (`record_id` is an opaque BBB ID); BBB disk; seconds until claimed; deleted by the
  daemon.

## 4. Exporter daemon

### 4.1 Loop (every 10 s; one record and one file at a time)

1. **Claim:** rename each `<SPOOL_DIR>/*.json` to `<STATE_DIR>/<record_id>/spool.json` (same filesystem). If a state
   record exists already, delete the duplicate and log.
2. **Build:** re-check opt-in and breakout (no longer true → delete the record, log, no failure). Select files by
   allow-list ([`meeting-package.md`](meeting-package.md) §5), apply V-E01…V-E03, hash files in place (no copy),
   write the canonical `manifest.json` into the state record → `ready`.
3. **Upload and commit** (§6) records in `ready`/`delivered` whose `next_attempt_at` has passed.
4. **Delete** raw data per §7.
5. **Prune** old state records (§5.4).

One daemon per server (`flock` on `<STATE_DIR>/.lock`).

### 4.2 Files are read in place

Uploads stream from `raw/<record_id>/` and re-hash on the fly. Size or hash mismatch, or a missing file → `failed`
with `source_changed` or `source_missing`. BBB's cron can cause this 14 days after publish if uploads keep failing
(§2); `recap_exporter_oldest_unacked_seconds` warns early.

### 4.3 Graceful shutdown

On SIGTERM: stop claiming, abort requests, persist state, exit within 20 s. An interrupted file is sent again (§6.1).

## 5. State model (transport-neutral)

States and failure reasons: [`README.md`](README.md) "Exporter states per recording". `ready` = manifest built and
persisted; `delivered` = every file is on the receiver, not yet acknowledged; `acked` = verified ack (§6.3); `deleted`
= `raw/<record_id>/` removed by the exporter (§7); `failed` = needs an admin. "Spooled" is not a persisted state.

### 5.1 Transitions

| From                           | Event                                                                     | To                                                                                                                              |
| ------------------------------ | ------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| spooled                        | build OK                                                                  | `ready`                                                                                                                         |
| spooled                        | V-E01…V-E03 violated (V-E01 is checked first, before the opt-in re-check) | `failed` (`no_events`, `no_microphone_tracks`, `unsupported_audio_bridge`, `limit_exceeded`)                                    |
| `ready`                        | all files on receiver                                                     | `delivered`                                                                                                                     |
| `ready`, `delivered`           | transient error (§6.5)                                                    | same state, `attempts + 1`, new `next_attempt_at`                                                                               |
| `ready`, `delivered`           | permanent error                                                           | `failed` (`source_changed`, `source_missing`, `rejected`, `conflict` = `409 record-already-ingested`, `unauthorized` = 401/403) |
| `delivered`                    | verified ack                                                              | `acked`                                                                                                                         |
| `acked`                        | deletion policy satisfied                                                 | `deleted`                                                                                                                       |
| `ready`, `delivered`, `failed` | failure expiry                                                            | `deleted` (reason `failure_expiry`)                                                                                             |
| `failed`                       | admin `requeue` (§8.3)                                                    | spooled (manifest rebuilt)                                                                                                      |

Each transition is written to the state file (fsync, atomic rename) **before** the next side effect, so a crash
repeats at most the last idempotent step.

### 5.2 Failure reasons

The reasons of [`README.md`](README.md), set as in §5.1. A recording that is not opted in gets no spool entry and no
state, so it never fails.

### 5.3 State record

`<STATE_DIR>/<record_id>/state.json`: `record_id`, `state`, `reason`, `package_id`, `job_id` (after ack), `attempts`,
`first_attempt_at`, `last_attempt_at`, `next_attempt_at`, `last_error` (problem `type` and HTTP status only), `history`
(`{state, at}`). Next to it: `manifest.json`, `spool.json`.

### 5.4 Retention of exporter state

Personal data: `meeting_id` and BBB user IDs in file names inside `manifest.json`; on BBB disk. Records in `acked` or
`deleted` are pruned 30 days after entering that state; `failed` and in-progress records stay until an admin acts or
failure expiry. Deleter: the daemon. A recording hooked again after pruning gets `409 record-already-ingested` →
`failed/conflict`: visible and harmless.

## 6. Ingest protocol (push)

Service-side guarantees (staging, atomic commit, idempotency): [`architecture.md`](architecture.md) §4.1. Conventions:
[`README.md`](README.md) "HTTP". Bearer token from `RECAP_EXPORTER_TOKEN_FILE`; the service holds the same value as
`bbb_server.<bbb_server_id>`. HTTPS only; tests use a TLS test server.

### 6.1 Resume granularity: the file

The manifest goes first; the service answers with the files it lacks; the exporter uploads only those. An interrupted
file is sent again from byte 0. There is no partial-upload state on either side and no chunking within a file.

### 6.2 Endpoints

| #   | Request                                                                                                                                                                                           | Success                                                | Errors                                                                                                                                                                                                                                              |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | `PUT /api/v1/ingest/packages/{package_id}`, body = canonical `manifest.json`                                                                                                                      | `201` new / `200` known: `IngestStatus`                | 400 `package-id-mismatch` (V-I01); 422 `manifest-invalid`, `unsupported-schema-version` (V-I02, V-I04, V-I08), `not-opted-in` (V-I07); 403 `server-mismatch` (V-I03); 409 `record-already-ingested` (V-I06); 413 `package-too-large`; 401, 429, 503 |
| 2   | `PUT …/{package_id}/files/{sha256}`, body = file bytes (`Content-Type: application/octet-stream`; the service does not check it), `Content-Length` required, optional `Content-Digest` (RFC 9530) | `204` stored / `200` already there                     | 404 `unknown-package`, `file-not-in-manifest`; 409 `package-committed`; 411; 413; 422 `file-hash-mismatch` (V-I05); 401, 429, 503                                                                                                                   |
| 3   | `POST …/{package_id}/commit`, body `{}`                                                                                                                                                           | `200`: `IngestAck`                                     | 409 `package-incomplete` (lists `missing`); 404, 401, 429, 503                                                                                                                                                                                      |
| 4   | `GET …/{package_id}`                                                                                                                                                                              | `200`: `IngestStatus` (with ack fields once committed) | 404, 401                                                                                                                                                                                                                                            |

429 (`rate-limited`) and 503 (`unavailable`) carry `Retry-After: 30`. A manifest body over 1 MiB is 413
`package-too-large`. A `Content-Digest` with `sha-256` that differs from the path hash is 422 `file-hash-mismatch`;
other digest algorithms are ignored. A missing `Content-Length` is 411 `length-required`. An upload to a committed
package is 409 `package-committed` before any other file check. `missing` is sorted by hash. A package is visible only
to the `bbb_server` that created it; for any other server it is 404 `unknown-package`.

Files are addressed by hash: identical files go once, and URLs never contain file names (which hold BBB user IDs).

```json
IngestStatus: { "package_id": "<hex>", "state": "receiving|complete|committed", "missing": ["<sha256>"],
                "received_size": 123, "expected_size": 456 }
IngestAck:    { "package_id": "<hex>", "state": "committed", "job_id": "<uuidv7>", "record_id": "<id>",
                "file_count": 7, "size": 456, "committed_at": "<RFC 3339>" }
```

`received_size`/`expected_size` count each distinct hash once (bytes stored); the ack's `file_count` and `size` are the
manifest totals (per path).

**The ack means:** all files are verified and durably stored, and the job row is committed. A repeated commit returns
the same ack. Uncommitted uploads are deleted `RECAP_INGEST_STAGING_TTL` after the last activity; a slower exporter
uploads again.

### 6.3 Sequence and ack verification

1. `PUT` manifest; upload each hash in `missing`; `PUT` again (or `GET`) until `missing` is empty → `delivered`.
2. `POST` commit. Verify the ack: `package_id`, `record_id`, `file_count`, `size` match the manifest and `job_id` is a
   UUID. Mismatch → stay `delivered`, log ERROR, retry later; **never delete on an unverified ack**. Match → store
   `job_id`, `acked`.
3. Lost commit response: the next manifest `PUT` says `committed`; commit again, same ack.
4. Every retry, after any error, starts again at step 1 with a manifest `PUT` (or `GET`).
5. Files with the same hash under several paths are uploaded once (`missing` lists hashes).

### 6.4 Idempotency

Every request can be repeated. Keys: `package_id` (package), `sha256` (file), `(bbb_server_id, record_id)` (one
package per recording). No partial-file state.

### 6.5 Retries

Transient: connection and TLS errors, timeouts, HTTP 408, 429, 500, 502, 503, 504. Delay `min(1h, 30s ×
2^(attempts−1))`, ±20 % jitter, at least `Retry-After`. Transient errors retry **without limit**; only failure expiry
(§7) ends them. Request timeout per file: 60 s + size ÷ 256 KiB/s. `Retry-After` is honoured up to 1 h. A commit
answered `409 package-incomplete` is transient (restart at the manifest `PUT`). Anything else is permanent → `failed`.
`first_attempt_at` is set when the build succeeds.

### 6.6 Contract tests (ingest)

CT-EXP-04: golden HTTP exchanges in `testdata/ingest/exchanges/*.json` (request line, headers without the token,
body hash, expected status and body), replayed against the service and used as the exporter's fake server. Cases:
happy path, resume, duplicate file, hash mismatch, lost commit response, 409, 429 with `Retry-After`, 503, 401, 403.

## 7. Deletion of raw data (exporter-owned)

The exporter, not the service, owns deletion of raw data on the BBB server
([ADR-002](../decisions/ADR-002-trigger-post-archive-push.md)). `RECAP_EXPORTER_DELETE_POLICY`:

- `never` (**default**): the exporter never deletes; BBB's cron or a BBB admin does. With all playback formats
  disabled nothing deletes it (§2): choose `after_ack` or clean up by hand. `recap_exporter_raw_bytes` shows it.
- `after_ack`: delete `raw/<record_id>/` when
  1. the state is `acked` with a verified ack;
  2. BBB no longer needs the dir: every format in `RECAP_EXPORTER_RECORDING_FORMATS` has
     `<RECAP_EXPORTER_RECORDING_STATUS_DIR>/published/<record_id>-<format>.done`. The operator lists the formats that
     have a `process/*.rb` script and are enabled. This mirrors `cron.daily/bigbluebutton:94-123 @ 7d27e7e` and is
     needed because post_archive runs **before** process/publish (§2);
  3. 24 h have passed since `acked` (time to notice a service problem).
- Admin `requeue` and `delete-raw` need the daemon stopped (they take its lock); `status` works while it runs. Manual
  `delete-raw` ignores the delete policy but applies every other check of this section.
- **Failure expiry** (`RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS`, `0` = off, any policy; needs the status dir and
  formats like `after_ack`; applies only to records with a manifest, so build failures are never expired
  automatically): a record in `ready`, `delivered` or `failed` for N days since `first_attempt_at` → delete the raw
  dir (condition 2 still applies), state `deleted` with reason `failure_expiry`, metric for alerting. A crash after
  the raw dir is gone but before the state says `deleted` shows up once as `failed/source_missing`; that is expected.

Deletion removes only `raw/<record_id>/`, recursively, without following symlinks, and only if `events.xml` still
matches the manifest hash (else `failed/source_changed`). It does not call `bbb-record --delete`, which also touches
published playback (`bbb-record:209,290,434-450 @ 7d27e7e`).

Other post_archive scripts that delete raw data race the daemon (`failed/source_missing`). On BBB servers that run
bbb-recap, the Opencast integration must set `deleteIfSuccessful = false` (`post-archive/post_archive.rb:25,555 @
233179d`; [`deployment.md`](deployment.md) §10). Raw data is then deleted by BBB's cron or by `after_ack`.

Data flow: raw data holds everything in [`meeting-package.md`](meeting-package.md) §5 plus video, deskshare and the
learning dashboard; on BBB disk; deleted by BBB's cron, the exporter (per policy) or a BBB admin.

## 8. Configuration and operation

### 8.1 Config

One file `/etc/recap-exporter/exporter.env`, read by hook and daemon; the daemon's environment overrides it. The file
is root-owned, mode `0644` and holds no secrets (only the token file path), so the hook can read it as the recording
worker user.

File grammar: one `KEY=value` per line, surrounding whitespace of the line trimmed; empty lines and lines starting
with `#` ignored; split at the first `=`, no spaces around it, no quote processing, no shell expansion; an empty value
means the default. Any other line, and any key not starting with `RECAP_EXPORTER_`, is a config error for the daemon
and is ignored by the hook. `RECAP_EXPORTER_ENV_FILE` in the environment of hook or daemon names another path (tests,
packaging). Keys and defaults: [`deployment.md`](deployment.md) §5.4.

Fixed constants: poll 10 s and one upload at a time (§4.1), shutdown 20 s (§4.3), retries and timeouts (§6.5), delete
grace 24 h (§7), state retention 30 days (§5.4), notes formats and size limits
([`meeting-package.md`](meeting-package.md) §5, §8).

The daemon validates the whole config at start and exits non-zero listing all errors; unknown `RECAP_EXPORTER_*` keys
are errors. A missing file is a config error (exit 2) unless all required keys come from the environment.

TLS trusts the system CA store. For a private CA, Go's standard `SSL_CERT_FILE` names a bundle that **replaces** the
system store, so it must hold the system CAs plus the private CA (no extra setting).

Uploads follow the order of `missing`, but the service accepts any order.

### 8.2 Logging

Structured JSON to stdout. At INFO and above only `record_id`, `package_id`, `job_id`, states, reasons, sizes,
durations. `meeting_id` and file names only at DEBUG (stricter than [`README.md`](README.md) "Logging": external
meeting IDs and BBB user IDs can be personal data). Never: meta values, names, emails, the token, URL query strings,
service response bodies (only problem `type` and HTTP status).

### 8.3 Admin CLI

`recap-exporter run` is the daemon. Admin commands: `recap-exporter status [<record_id>]`, `requeue <record_id>` (from
`failed`: rebuild, retry), `delete-raw <record_id>` (manual, same checks as §7), `check-config`, `healthcheck` (calls
`/readyz` of the running daemon, exit 0/1; the container healthcheck). They work on the state dir under the same lock
and never contact the service.

### 8.4 Metrics and health (`RECAP_EXPORTER_LISTEN_ADDR`)

| Metric                                  | Type    | Labels                                             | Alert                     |
| --------------------------------------- | ------- | -------------------------------------------------- | ------------------------- |
| `recap_exporter_recordings`             | gauge   | `state`                                            | `state="failed"` > 0      |
| `recap_exporter_oldest_unacked_seconds` | gauge   | —                                                  | > 6 h                     |
| `recap_exporter_deleted_total`          | counter | `reason` (`after_ack`, `failure_expiry`, `manual`) | `failure_expiry` increase |
| `recap_exporter_raw_bytes`              | gauge   | `state`                                            | disk planning             |
| `recap_exporter_build_info`             | gauge   | `version`                                          | —                         |

`GET /healthz`: process alive. `GET /readyz`: config valid, state dir writable, lock held. No call to the service.

### 8.5 Installation on a Debian-package BBB (BBB 4.0)

**Supported: container.** The daemon runs from the `recap-exporter` image under docker compose
([`deployment.md`](deployment.md) §7, [`../operations/install-exporter.md`](../operations/install-exporter.md)); the
hook is copied to the post_archive dir below. Container user = the host user `recap-exporter`, extra group = the group
of the hook's user (`bigbluebutton`, the `User=` of BBB's resque worker:
`record-and-playback/core/systemd/bbb-rap-resque-worker.service:14 @ 7d27e7e`); paths, modes and the token as in the
list below. The systemd hardening maps to `read_only`, `cap_drop: [ALL]` and `no-new-privileges`.

**Not available yet: a `.deb` package.** The intended layout:

- The package installs the hook (repo file `hook/post_archive/recap_export.rb`) at
  `/usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb`, `/usr/bin/recap-exporter`,
  `/lib/systemd/system/recap-exporter.service` and `/etc/recap-exporter/exporter.env`. `postinst` creates the system
  user `recap-exporter` (no login shell) and `/var/lib/recap-exporter/{spool,state}`. The daemon does not start until
  the token file (mode `0400`, owner `recap-exporter`) exists.
- Permissions: the hook runs as the user of BBB's recording workers. The spool dir is
  `recap-exporter:<that group>`, mode `2770`. The daemon reads `raw/<record_id>/`; it writes to `raw/` only with
  `after_ack`.
- systemd: `User=recap-exporter`, `Restart=on-failure`, `NoNewPrivileges=yes`, `ProtectSystem=strict`,
  `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`, empty `CapabilityBoundingSet=`,
  `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, `ReadWritePaths=/var/lib/recap-exporter` (plus the raw dir only
  with `after_ack`), `TimeoutStopSec=30s`
  (https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html).
- Summary-only meetings: a front end may send `meta_bbb-disable-recording-formats=presentation` (§2); then the
  operator needs `after_ack` or manual cleanup (§7).

### 8.6 Docker compose BBB setup (upstream PR bigbluebutton/bigbluebutton#26186)

This section covers the docker compose setup of upstream PR
[bigbluebutton/bigbluebutton#26186](https://github.com/bigbluebutton/bigbluebutton/pull/26186) (branch `compose-4.1`,
base `v4.1.x-develop`, head `c3d6b3e`), not the community `bigbluebutton/docker` repo.

Four services share the `bbb-record-and-playback` image and the mounts `./data/bigbluebutton:/var/bigbluebutton` and
`./data/recordings:/var/lib/bbb-webrtc-recorder`: `recording-workers` (the resque workers, image default),
`recording-starter`, `recording-caption-inbox` and `recording-cleanup` (runs `/etc/cron.daily/bigbluebutton` daily)
(`docker-compose.yml:9-22,303-321 @ c3d6b3e`, `docker/README.md:68-70,78 @ c3d6b3e`). Processes run as
`bigbluebutton`, UID/GID 998 (`docker/README.md:57 @ c3d6b3e`).

Install:

- The hook is bind-mounted read-only into `recording-workers`, which runs post_archive as UID 998 with
  `-m <record_id>`, only for meetings with recording marks (observed on a test install of this setup).
- `/etc/recap-exporter/exporter.env` and the spool are bind-mounted at the same paths; spool group = GID 998.
- The daemon runs from the `recap-exporter` image under docker compose ([`deployment.md`](deployment.md) §7,
  [`../operations/install-exporter.md`](../operations/install-exporter.md)) with the host side of
  `./data/bigbluebutton/recording/raw` and `status/` mounted at the default paths, extra group 998. Running the
  daemon from systemd on the host also works, with `RECAP_EXPORTER_RAW_DIR` set to the host path.

Observed on a test install: `raw/<record_id>/` is complete (the audio is copied in; `./data/recordings` is `0700`
and empty after processing) and world-readable (`0755`/`0644`, UID/GID 998). UID/GID 998 may belong to another
account on the host (for example `systemd-coredump` on Fedora); a production install should give BBB its own UID/GID.

## 9. Pull-API transport (separate project, outline)

Not built. An outline, so the package and state model stay transport-neutral
([ADR-003](../decisions/ADR-003-exporter-contract-transport-neutral.md)):

- Same package and states; served by the BBB server (inbound access, own threat model), not by the service.
- Ready signal: the spool and state records, not `getRecordings`, which has no "raw ready" state
  (`docs/docs/data/getRecordings.tsx:17-20 @ 7d27e7e`).
- Auth: per-consumer credentials, **never the BBB shared secret**, which has no expiry and grants full API access
  (`docs/docs/development/api.md:237-256 @ 7d27e7e`).
- Sketch: list ready packages (paged), get manifest, get file by hash with `Range`, get deterministic tar
  ([`meeting-package.md`](meeting-package.md) §3), post an ack with the consumer's verification summary; `429`/`503`
  with `Retry-After`.
- Per consumer `ready` → `delivered` → `acked`; raw data is deleted only after **every** consumer acked and §7 holds.
- Open for that project: consumer registry, exposure behind BBB's nginx, and the upstream discussion in
  [BBB issue #24294](https://github.com/bigbluebutton/bigbluebutton/issues/24294).

## 10. Contract tests

| ID        | Test                                                                                                                                                                                                    | Pins |
| --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---- |
| CT-EXP-01 | Hook decision table on F-01, F-03, F-05, F-08, F-11 ([`fixtures.md`](fixtures.md)): golden spool entry or none; static check that the hook loads no network library and writes only below the spool dir | §3   |
| CT-EXP-02 | Build: F-01 → golden `manifest.json` and `package_id` (injected clock); F-02 → `failed/unsupported_audio_bridge`; F-10 → `failed/no_microphone_tracks`                                                  | §4   |
| CT-EXP-03 | State machine §5.1 against the CT-EXP-04 fake server, with a kill and restart at every persisted step: same final state and request log                                                                 | §5   |
| CT-EXP-04 | Ingest exchanges (§6.6), both sides                                                                                                                                                                     | §6   |
| CT-EXP-05 | Deletion: `never` deletes nothing; `after_ack` waits for BBB markers and grace; failure expiry; refusal when `events.xml` changed                                                                       | §7   |
| CT-EXP-06 | `source_changed` / `source_missing` when a file changes or disappears between build and upload                                                                                                          | §4.2 |
| CT-EXP-07 | Config: missing required keys, unknown keys, `http://` service URL → non-zero exit listing all errors                                                                                                   | §8.1 |
| CT-EXP-08 | Logging: F-01 end-to-end at INFO; no `meeting_id`, user ID, file name or transcript phrase in the output                                                                                                | §8.2 |

## 11. Known limitations

- The hook's log destination on a packaged BBB 4.0, and whether its Ruby ships `rexml`, were not checked. On the
  docker-based BBB 4.1 setup (§8.6) `json` and `rexml` load (Ruby 3.2.3).
- The recording worker user, and owner and mode of `raw/`, were not checked on a packaged BBB 4.0. If `raw/` is not
  readable for the exporter, it needs group membership or an ACL.
- Whether BBB package upgrades keep files in the post_archive directory was not checked.
- The exact effect of `bbb-record --delete` was not checked in detail; the exporter does not use it.
- `after_ack` mirrors the completion check of BBB's daily cron (§7). That a later cron run tolerates a raw dir already
  removed by the exporter was not checked.
- Whether `metadata.xml` of a published recording is publicly readable was not checked; treat meta values as visible
  ([`metadata-interface.md`](metadata-interface.md) §1.1).
