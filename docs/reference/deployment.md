# Deployment

Images, processes, compose and Kubernetes layout, every configuration variable, migrations, upgrades and backups.
Names follow [`README.md`](README.md); behaviour is described in [`architecture.md`](architecture.md). Decisions:
ADR-002, ADR-007, ADR-009 to ADR-012 in [`../decisions/`](../decisions/). Step-by-step guides are in
[`../operations/`](../operations/).

## 1. Images

| Image            | Contents                                                                                                                                                     | Used in                                     |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------- |
| `recap`          | one static Go binary (`CGO_ENABLED=0`) with embedded migrations and time zone data (`time/tzdata`, https://pkg.go.dev/time/tzdata), plus CA certificates     | compose, K8s                                |
| `recap-mock`     | static binary: mock ASR (WhisperX dialect), mock LLM, mock Docs `create-for-owner`, answering from `fixtures.md` data                                        | dev overlay and CI; **never in production** |
| `recap-exporter` | static binary of the exporter daemon and its admin CLI, plus CA certificates; no hook, no shell (`bbb-exporter.md` §8.5, §8.6). A Debian package comes later | BBB servers (compose, `deploy/exporter/`)   |

Rules for every image:

- `FROM scratch` plus copied CA certificates: a small attack surface. No shell, no package manager.
- Numeric non-root user `10001:10001` (`USER` in the image), so `runAsNonRoot` can verify it.
- Read-only root filesystem. Only writable path: `/tmp` as tmpfs (compose) or `emptyDir` (K8s); the exporter writes
  only to its state mount and needs no `/tmp`.
- Healthcheck via the binary: `recap healthcheck` calls `http://127.0.0.1:9090/readyz` and exits 0 or 1;
  `recap-exporter healthcheck` calls `/readyz` on the port of `RECAP_EXPORTER_LISTEN_ADDR` (loopback for a wildcard
  address) and exits 0 or 1.
- Extra CAs (private S3, internal providers): mount a bundle and set `SSL_CERT_FILE`, which Go reads on Linux
  (https://pkg.go.dev/crypto/x509#SystemCertPool). No custom CA variables.
- Labels carry version and commit; `recap_build_info` shows the same. Same image under compose and K8s.
- CA certificates are refreshed by rebuilding the image; a monthly rebuild is part of the release routine.

Exit codes: `0` stop, `1` runtime failure, `2` invalid config or usage error (unknown command), `3` migration failure or incompatible schema.

## 2. Processes and ports

| Process         | Ports                     | Replicas            | Needs                                          |
| --------------- | ------------------------- | ------------------- | ---------------------------------------------- |
| `recap api`     | public `8080`, ops `9090` | 2                   | Postgres, blob store, OIDC provider (admin UI) |
| `recap worker`  | ops `9090`                | 2                   | Postgres, blob store, ASR, LLM, Docs           |
| `recap migrate` | none; runs and exits      | one-shot per deploy | Postgres                                       |

Ports are fixed; remap them in compose or the Service object. TLS ends at the operator's proxy or ingress. Postgres
and S3 use TLS in production (`sslmode=verify-full`, `https://` endpoint).

## 3. Compose

`deploy/compose.yaml` (operator stack; settings in `.env` from `deploy/.env.example`, config examples in
`deploy/config/`):

| Service    | Image       | Depends on          | Storage                                                              | Notes                               |
| ---------- | ----------- | ------------------- | -------------------------------------------------------------------- | ----------------------------------- |
| `postgres` | Postgres 18 | —                   | volume `pgdata`                                                      | healthcheck `pg_isready`            |
| `migrate`  | `recap`     | `postgres` healthy  | secrets (ro) (DB credentials only)                                   | `recap migrate`, `restart: "no"`    |
| `api`      | `recap`     | `migrate` completed | config, secrets (ro), tmpfs `/tmp`, volume `blobdata` (`fs` backend) | only `8080` published, to the proxy |
| `worker`   | `recap`     | `migrate` completed | config, secrets (ro), tmpfs `/tmp`, volume `blobdata` (`fs` backend) | 1–2 replicas                        |

- `depends_on` with `service_completed_successfully` / `service_healthy`
  (https://docs.docker.com/reference/compose-file/services/#depends_on). `restart: unless-stopped` otherwise.
- Host paths: only the three config files (`meeting-types.yaml`, `providers.yaml`, `sinks.yaml`, mounted one by one
  into `/etc/recap/` so the image's `/etc/recap/formats` stays visible) and `./secrets` → `/run/secrets/recap`, all
  read-only. Data in volumes. With podman-compose, `migrate` may run again whenever a dependent service starts (safe:
  idempotent and locked), and a failed `migrate` does not stop `api`/`worker` from starting; they stay not ready
  (`/readyz` 503).
- Variables: every operator setting of §5 is in `deploy/.env.example` and passed by compose; an empty value means the
  binary's default. Compose-only (interpolation): `RECAP_IMAGE`, `RECAP_CONFIG_DIR`, `RECAP_SECRETS_DIR`,
  `RECAP_HTTP_PORT`. Leave unset (fixed by the compose file or the image): every `*_FILE`, `RECAP_FS_ROOT`,
  `RECAP_PROVIDERS_FILE`, `RECAP_SINKS_FILE`, `RECAP_MEETING_TYPES_FILE`, `RECAP_FORMATS_DIR`,
  `RECAP_CLIENT_SECRETS_DIR`.
- Blobs: the compose default is the `fs` backend on the volume `blobdata`, shared by `api` and `worker` (ADR-011
  amendment). An operator with an existing S3 sets `RECAP_BLOB_BACKEND=s3` and the `RECAP_S3_*` keys instead and
  creates the bucket once. The stack ships no object store and no TLS proxy; `deploy/Caddyfile.example` shows a
  reverse proxy on the host.
- Images: `RECAP_IMAGE` is required (a published image, `docs/operations/release.md`, or a local build). Postgres
  uses compose's default pull policy.
- Dev and tests: the overlay `tools/dev/compose.yaml` (`-f deploy/compose.yaml -f tools/dev/compose.yaml
--env-file tools/dev/dev.env`) adds `mock` (`recap-mock`: ASR, LLM, Docs and OIDC mocks; never published), `minio`
  and `minio-init` (a dev S3 to exercise the S3 backend) and `tls` (Caddy with `tls internal` in front of `api`, so
  the exporter can use `https://`; its CA reaches the exporter through `SSL_CERT_FILE`). Dev config and secrets live
  in `tools/dev/`. The repository's test scripts never pull; they check that every image is present first.
- Install guide: `docs/operations/install-compose.md`.

## 4. Kubernetes

Plain Deployments, a Job, Secrets and ConfigMaps; no Helm chart. The repository ships no Kubernetes manifests yet;
this section is the layout they follow.

- Same image and variables as compose. ConfigMap items mounted as single files (`subPath`) under `/etc/recap/`,
  Secrets as files under `/run/secrets/recap`. A custom formats directory is its own volume at `RECAP_FORMATS_DIR`.
- `api`, `worker`: Deployments. `migrate`: a Job before rollout. `api`/`worker` stay not-ready on an older schema.
- `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, all capabilities dropped, no `hostPath`.
  `/tmp` is an `emptyDir` with `sizeLimit`.
- No RWX volumes: shared bytes live in S3 (ADR-011). The filesystem blob backend is not for K8s.
- Probes: liveness `/healthz`, readiness `/readyz` on `9090`. `terminationGracePeriodSeconds: 30` (the binary uses
  25 s, `architecture.md` §9).
- NetworkPolicy: `api:8080` from the ingress only; `9090` from monitoring; `worker` egress to Postgres, S3, providers,
  Docs; `api` egress to Postgres, S3, IdP.
- Requests: `api` 0.25 CPU / 256 MiB, `worker` 0.5 CPU / 512 MiB, limits 2× (estimates, `architecture.md` §10).

## 5. Configuration

- **Environment variables first.** YAML files only for nested config: provider profiles, sink profiles, meeting types.
- **This section lists every variable.** Anything else is a constant documented in the reference document that uses it.
- Validated in one pass at start, before any connection; all errors are printed together; exit `2`.
  `recap check-config [--role api|worker]` runs the same check (default: both roles). Unknown `RECAP_*` variables are
  errors, except `RECAP_EXPORTER_*`, `RECAP_CONTRACT_*` and `RECAP_BIN` (other programs on the same host).
- **Secrets only from files** (`*_FILE`; content trimmed of a trailing newline). A password inside `RECAP_DB_DSN` is an
  error. A boot log at INFO lists effective values and file paths, never file contents, and warns on `http://` S3
  endpoints. The filesystem backend is supported for compose (one host, one shared volume); it is not for
  Kubernetes (§4), so the boot log notes the backend at INFO, without a warning.
- Types: `duration` = Go syntax plus `d` for days (`14d`); `bool` = `true`/`false`; `list` = comma-separated.
- Config is read at start; its hash is pinned per job (`architecture.md` §5.2). Changes need a restart.

### 5.1 Common

| Variable                                                         | Default                         | Required    | Meaning                                                                     |
| ---------------------------------------------------------------- | ------------------------------- | ----------- | --------------------------------------------------------------------------- |
| `RECAP_LOG_LEVEL`                                                | `info`                          | no          | `debug`, `info`, `warn`, `error`                                            |
| `RECAP_DB_DSN`                                                   | —                               | yes         | Postgres URL without password                                               |
| `RECAP_DB_PASSWORD_FILE`                                         | —                               | yes         | DB password                                                                 |
| `RECAP_BLOB_BACKEND`                                             | `s3`                            | no          | `s3` (Kubernetes, existing S3) or `fs` (compose default via `compose.yaml`) |
| `RECAP_S3_ENDPOINT`                                              | —                               | if `s3`     | `https://…`                                                                 |
| `RECAP_S3_REGION`                                                | `us-east-1`                     | no          |                                                                             |
| `RECAP_S3_BUCKET`                                                | —                               | if `s3`     | dedicated bucket                                                            |
| `RECAP_S3_ACCESS_KEY_ID_FILE`, `RECAP_S3_SECRET_ACCESS_KEY_FILE` | —                               | if `s3`     | S3 credentials                                                              |
| `RECAP_FS_ROOT`                                                  | —                               | if `fs`     |                                                                             |
| `RECAP_PROVIDERS_FILE`                                           | `/etc/recap/providers.yaml`     | worker      | provider profiles (`providers.md`)                                          |
| `RECAP_SINKS_FILE`                                               | `/etc/recap/sinks.yaml`         | api, worker | sink profiles (`sinks.md`)                                                  |
| `RECAP_MEETING_TYPES_FILE`                                       | `/etc/recap/meeting-types.yaml` | api, worker | meeting type → format (`templates.md`)                                      |
| `RECAP_FORMATS_DIR`                                              | `/etc/recap/formats`            | worker      | format and prompt files (`templates.md`)                                    |
| `RECAP_META_PREFIX`                                              | `recap`                         | no          | BBB source setting: must equal the exporter's `RECAP_EXPORTER_META_PREFIX`  |

### 5.2 `api`

| Variable                                                | Default                      | Required | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------------------------------------------------- | ---------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `RECAP_CLIENT_SECRETS_DIR`                              | `/run/secrets/recap/clients` | yes      | one file per client: `bbb_server.<id>`, `frontend.<id>`, `admin.<name>` (README "HTTP"). For rotation, `<name>.next` is accepted as well. Compared in constant time                                                                                                                                                                                                                                                                                                                                                        |
| `RECAP_FRONTEND_SERVERS`                                | — (all)                      | no       | BBB source setting: list of `<frontend_id>:<bbb_server_id>`: which servers' recordings a front end may claim (`metadata-interface.md` §2.1); read by api and worker (`validate` check 8). Unset or empty: every front end may claim every server. Set it when several organisations share one service; then the front ends in `RECAP_FRONTEND_TYPES` and `RECAP_FRONTEND_SINKS` must be listed here too. Without it, a mistyped front-end ID in those two variables is not detected and leaves that front end unrestricted |
| `RECAP_FRONTEND_TYPES`                                  | —                            | no       | list of `<frontend_id>:<meeting type>`; a front end listed here may request only these types; unlisted front ends may request every configured type (`validate` check 9a)                                                                                                                                                                                                                                                                                                                                                  |
| `RECAP_FRONTEND_SINKS`                                  | —                            | no       | list of `<frontend_id>:<sink profile>`; same rule for sink profiles (`validate` check 9a)                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `RECAP_REGISTRATION_TTL`                                | `14d`                        | no       | unmatched registrations are deleted this long after the latest `PUT` that changed them; read by api and worker (cleanup loop)                                                                                                                                                                                                                                                                                                                                                                                              |
| `RECAP_RESULT_TTL`                                      | `7d`                         | no       | how long the result record and a `frontend` sink summary wait for the front end after delivery (`metadata-interface.md` §2.7); `0` = delete at `cleanup`, not allowed with a `frontend` sink profile; read by api and worker                                                                                                                                                                                                                                                                                               |
| `RECAP_INGEST_STAGING_TTL`                              | `72h`                        | no       | uncommitted uploads are deleted after this; read by api and worker (cleanup loop)                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `RECAP_PUBLIC_BASE_URL`                                 | —                            | if UI    | external URL for OIDC redirects                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `RECAP_OIDC_ISSUER`                                     | —                            | no       | setting it enables the admin UI                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `RECAP_OIDC_CLIENT_ID`, `RECAP_OIDC_CLIENT_SECRET_FILE` | —                            | if UI    |                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `RECAP_OIDC_ROLE_CLAIM`                                 | `roles`                      | no       | claim (a string list) that must contain `recap-admin`                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `RECAP_SESSION_KEY_FILE`                                | —                            | if UI    | ≥ 32 bytes; same file on all `api` replicas                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |

### 5.3 `worker`

| Variable                         | Default | Required | Meaning                                                                    |
| -------------------------------- | ------- | -------- | -------------------------------------------------------------------------- |
| `RECAP_WORKER_SLOTS`             | `4`     | no       | steps at once per process                                                  |
| `RECAP_RETENTION_AFTER_DELIVERY` | `0`     | no       | delay before `cleanup` (ADR-009); read by worker and api (`release`)       |
| `RECAP_JOB_METADATA_RETENTION`   | `180d`  | no       | pseudonymous job rows                                                      |
| `RECAP_DISPLAY_TZ`               | `UTC`   | no       | IANA zone for dates in titles and documents (`templates.md`)               |
| `RECAP_DEFAULT_LANGUAGE`         | `en`    | no       | output language when the transcript gives none                             |
| `RECAP_ASR_LANGUAGE`             | empty   | no       | force the ASR decoding language (ISO 639-1); empty = auto-detect per track |

Provider and sink credentials are files named in the profile YAML (`providers.md`, `sinks.md`). `max_inflight` is a
profile field and counts per worker process (`architecture.md` §7). Retries, leases and intervals are constants
(`architecture.md` §5.3, §6.2).

### 5.4 Exporter (`/etc/recap-exporter/exporter.env`, `bbb-exporter.md` §8.1)

| Variable                                   | Default                                    | Required       | Meaning                                                                                                                  |
| ------------------------------------------ | ------------------------------------------ | -------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `RECAP_EXPORTER_SERVICE_URL`               | —                                          | yes            | service base URL, `https://…`                                                                                            |
| `RECAP_EXPORTER_BBB_SERVER_ID`             | —                                          | yes            | this server's `bbb_server_id`; all nodes of a BBB cluster may share one (`install-exporter.md`)                          |
| `RECAP_EXPORTER_TOKEN_FILE`                | `/etc/recap-exporter/secrets/ingest-token` | no             | ingest credential, mode `0400`                                                                                           |
| `RECAP_EXPORTER_META_PREFIX`               | `recap`                                    | no             | must equal `RECAP_META_PREFIX`                                                                                           |
| `RECAP_EXPORTER_RAW_DIR`                   | `/var/bigbluebutton/recording/raw`         | no             | BBB's raw recording directory ([`upstream-facts.md`](upstream-facts.md))                                                 |
| `RECAP_EXPORTER_RECORDING_STATUS_DIR`      | —                                          | if `after_ack` | BBB's `status` dir (parent of `published/`) for the completion check (`bbb-exporter.md` §7)                              |
| `RECAP_EXPORTER_RECORDING_FORMATS`         | —                                          | if `after_ack` | comma list of formats whose `status/published/<record_id>-<format>.done` must exist before deletion, e.g. `presentation` |
| `RECAP_EXPORTER_SPOOL_DIR`                 | `/var/lib/recap-exporter/spool`            | no             |                                                                                                                          |
| `RECAP_EXPORTER_STATE_DIR`                 | `/var/lib/recap-exporter/state`            | no             | same filesystem as the spool                                                                                             |
| `RECAP_EXPORTER_DELETE_POLICY`             | `never`                                    | no             | `never` or `after_ack`                                                                                                   |
| `RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS` | `0` (off)                                  | no             | failure expiry                                                                                                           |
| `RECAP_EXPORTER_LISTEN_ADDR`               | `127.0.0.1:9466`                           | no             | metrics and health                                                                                                       |
| `RECAP_EXPORTER_LOG_LEVEL`                 | `info`                                     | no             |                                                                                                                          |

## 6. Migrations

- SQL files embedded in the binary, numbered, forward-only. Only `recap migrate` applies them.
- `recap migrate` holds a Postgres advisory lock (waits up to 5 min), so parallel runs are safe
  (https://www.postgresql.org/docs/16/explicit-locking.html#ADVISORY-LOCKS).
- Ledger `schema_migrations(version, sha256, applied_at)`. A changed applied migration is fatal (exit `3`).
- **Expand, then contract:** a release only adds; removing a column happens one release after the code stopped using
  it, and the release notes say so.
- `api` and `worker` are not ready while the DB is older than they need. A newer DB is fine (expand-only).

## 7. Exporter installation

On every BBB server the exporter daemon runs as the `recap-exporter` image under docker compose
(`deploy/exporter/compose.yaml`), for both install types: packaged BBB (`bbb-exporter.md` §8.5) and the docker
compose setup (`bbb-exporter.md` §8.6). The post_archive hook is not in the image; it is installed into BBB's own worker.
Steps: `docs/operations/install-exporter.md`.

- One file `/etc/recap-exporter/exporter.env` serves hook and daemon with the paths the hook sees; the compose file
  sets the daemon's paths inside the container in its environment, which overrides the file (§8.1).
- Bind mounts only: the env file and the secrets dir (read-only), `/var/lib/recap-exporter` (spool and state in one
  mount, because claiming renames across them), BBB's `raw/` (read-only; read-write only for `after_ack` or failure
  expiry) and `status/` (read-only).
- Host user `recap-exporter` as the container user (`user:`), the hook's group added (`group_add:`), so the daemon
  reads the spool entries and `raw/` and owns spool, state and token like the systemd install.
- Host network (outbound HTTPS only; metrics stay on `127.0.0.1:9466`), read-only root filesystem, no capabilities,
  `no-new-privileges`, `restart: unless-stopped`, 30 s stop grace, healthcheck `recap-exporter healthcheck`.
- Later: a `.deb` with the systemd unit of `deploy/exporter/recap-exporter.service` (§8.5).

## 8. Upgrade and rollback

1. `recap check-config` with the new image.
2. `recap migrate` (expand-only, old binaries keep working).
3. Roll `worker`, then `api`. Old workers release their steps on SIGTERM; steps are idempotent.
4. A release must run steps of jobs created by the previous one; if it cannot, its notes say "drain first".

Commands for compose: `docs/operations/install-compose.md`, "Upgrade and rollback".

Rollback: deploy the previous image; no down-migrations. Rolling back past a contract migration needs a DB restore.
Exporter: deploy the previous `recap-exporter` image tag (`docs/operations/install-exporter.md`, "Upgrade and
rollback"); version N reads
N−1 state files. Upgrade the service before the
BBB servers (manifest minor versions, `meeting-package.md`).

## 9. Backups and restore

| Data       | Backup                               | Note                                                                                                                 |
| ---------- | ------------------------------------ | -------------------------------------------------------------------------------------------------------------------- |
| Postgres   | daily dump or WAL archiving, 14 days | contains personal data (registrations); retention listed in `privacy.md`                                             |
| Blob store | none                                 | content is transient (ADR-009); a backup would extend retention. Lost blobs → re-export while BBB still has raw data |
| Config     | version control                      | no secrets inside                                                                                                    |
| Secrets    | operator's secret store              |                                                                                                                      |

After a restore, unknown blob prefixes are removed by the cleanup loop (`architecture.md` §8). A restore can bring back
personal data deleted after the backup; this is accepted and documented in `privacy.md` §5.4. Test a restore with
fixture data before production use.

## 10. Operator prerequisites

- La Suite Docs: one server-to-server key in `DJANGO_SERVER_TO_SERVER_API_TOKENS`, the same Keycloak realm as PILOS
  so that `sub` matches (ADR-007; [`upstream-facts.md`](upstream-facts.md)). The key file is named in the sink profile.
- Keycloak (or another OIDC provider): client for the admin UI, confidential, standard flow with PKCE (S256), redirect URI
  `<RECAP_PUBLIC_BASE_URL>/admin/callback`. The role check reads a top-level string-list claim (`RECAP_OIDC_ROLE_CLAIM`,
  default `roles`); Keycloak puts realm roles under `realm_access.roles`, so the client needs a "User Realm Role"
  protocol mapper (`oidc-usermodel-realm-role-mapper`) with claim name `roles`, multivalued, added to the ID token
  (example: suitenumerique/meet `docs/examples/helm/keycloak.values.yaml:915-927 @ 815baa73`). Admins get the realm role
  `recap-admin`.
- Providers: whisperx-api-server and vLLM reachable from `worker`; `RESULT_STORE__ENABLED=false` on
  whisperx-api-server (`providers.md`; [`upstream-facts.md`](upstream-facts.md)).
- BBB servers that also run the Opencast post_archive integration: set its `deleteIfSuccessful = false`, otherwise it
  can delete `raw/<record_id>` before the exporter reads it (`bbb-exporter.md`).
- Prometheus scraping `9090`; alerts from `architecture.md` §12 and `bbb-exporter.md` §8.4.

## Known limitations

- Owner and mode of `raw/` and the BBB recording worker user on packaged BBB 4.0 are not checked; group membership or
  an ACL for the exporter may be needed (`bbb-exporter.md` §8.5).
- Not checked: whether BBB package upgrades keep files in `/usr/local/bigbluebutton/core/scripts/post_archive/`.
  Check the hook after each BBB upgrade.
- Kubernetes manifests are not shipped yet (§4).
