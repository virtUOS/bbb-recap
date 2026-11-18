# Install recap with docker compose

You get the recap service on one Linux host: `postgres`, a one-shot `migrate`, the `api` and the `worker`, all from
`deploy/compose.yaml`. You need docker with the compose plugin (podman with podman-compose works the same), `git`
(`openssl` only for the manual setup), a DNS name with a TLS reverse proxy, a speech recognition (ASR) server, an
OpenAI-compatible LLM endpoint, and La Suite Docs or a front end that fetches the summaries. BBB servers connect later
with the exporter ([install-exporter.md](install-exporter.md)).

Start with [integration.md](integration.md): the parts of an installation, which values connect them, where each is
set, and the order of the steps.

## Quick start

On the recap host, as a user who may run docker. [integration.md](integration.md) shows every value the setup asks
for and where else it must be set. Have them at hand: the public host name, the ASR and LLM URLs and keys, the Docs URL
and key (or none, if only the front end fetches the summaries), and the IDs of your BBB servers and front ends.

1. Get the files of a release ([release.md](release.md) lists the tags; `deploy/setup.sh` comes with the releases
   after v0.2.1, for older ones see [Manual setup](#manual-setup)) and create the install directory:

   ```sh
   RECAP_VERSION=v0.3.0
   git clone --depth 1 --branch "$RECAP_VERSION" https://github.com/virtUOS/bbb-recap.git /tmp/bbb-recap
   sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /opt/recap
   ```

2. Run the setup. It asks its questions, writes `.env`, `config/` and `secrets/` in `/opt/recap`, creates one token per
   BBB server and front end, runs `check-config` and starts the service:

   ```sh
   /tmp/bbb-recap/deploy/setup.sh --dir /opt/recap
   ```

   At the end it prints the next steps and the new tokens, once: give each to the admin of that BBB server or front
   end. They stay in `/opt/recap/secrets/clients/`.

3. Check:

   ```sh
   cd /opt/recap
   docker compose ps                                    # postgres, api, worker healthy; migrate exited (0)
   docker compose exec api /usr/local/bin/recap healthcheck && echo api ready
   ```

4. Put your TLS reverse proxy in front of `127.0.0.1:8080` ([`Caddyfile.example`](../../deploy/Caddyfile.example)
   shows Caddy). Check from outside: `curl -sS -o /dev/null -w '%{http_code}\n' https://recap.example.org/` prints
   `404` (the api answers; there is no page at `/`, the admin UI is at `/admin/`). `502` means the proxy cannot reach
   the api.

5. Connect each BBB server with the command the setup printed ([install-exporter.md](install-exporter.md)) and each
   front end ([below](#bbb-servers-and-front-ends)).

## Reference

All files live in `/opt/recap`.

| File                        | Content                                                                                                                                                |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `compose.yaml`              | the four services; take it unchanged from the release                                                                                                  |
| `.env`                      | every setting, one comment each; empty means the default                                                                                               |
| `config/providers.yaml`     | ASR and LLM endpoints ([providers.md](../reference/providers.md) §4.1)                                                                                 |
| `config/sinks.yaml`         | where summaries go: Docs or the front end ([sinks.md](../reference/sinks.md))                                                                          |
| `config/meeting-types.yaml` | meeting type to output format; slides go to the LLM only for a type with `inputs: {slides: true}` ([templates.md](../reference/templates.md) §1, §3.1) |
| `secrets/`                  | one file per secret, see below                                                                                                                         |

| Secret file                                | Content                                     | Needed                            |
| ------------------------------------------ | ------------------------------------------- | --------------------------------- |
| `db-password`                              | Postgres password (any random value)        | always                            |
| `clients/bbb_server.<id>`                  | token of a BBB server (its exporter)        | per BBB server                    |
| `clients/frontend.<id>`                    | token of a front end                        | per front end                     |
| `clients/admin.<name>`                     | token for the admin API                     | optional                          |
| `providers/asr.key`, `providers/llm.key`   | provider API keys                           | as named in `providers.yaml`      |
| `sinks/docs.key`                           | Docs server-to-server key                   | with the Docs sink                |
| `oidc-client-secret`, `session-key`        | admin UI client secret, cookie key (≥ 32 B) | with the admin UI                 |
| `s3-access-key-id`, `s3-secret-access-key` | S3 credentials                              | only with `RECAP_BLOB_BACKEND=s3` |

| Item           | Value                                                                                             |
| -------------- | ------------------------------------------------------------------------------------------------- |
| Port           | `127.0.0.1:8080` (api, `RECAP_HTTP_PORT`); metrics and health on `9090` inside the network only   |
| Volumes        | `pgdata` (database), `blobdata` (meeting files while a job runs, deleted after delivery)          |
| Logs           | `docker compose logs --tail 50 api worker`; JSON lines, never meeting content                     |
| Metrics        | `api:9090/metrics`, `worker:9090/metrics` on the network `recap_default`                          |
| After a change | `docker compose up -d --force-recreate api worker` (config, secrets and `.env` are read at start) |
| Backup         | `docker compose exec postgres pg_dump -U recap recap > recap.sql`; no backup of `blobdata`        |
| Stop           | `docker compose down`; `down -v` also deletes the database                                        |
| All settings   | [`.env.example`](../../deploy/.env.example), [deployment.md](../reference/deployment.md) §5       |

### setup.sh

`deploy/setup.sh [--dir DIR] [--non-interactive] [--no-start] [--force]`; `--help` lists every option and the
`RECAP_SETUP_*` variable that answers each question in advance (for Ansible or CI; with `--non-interactive` a missing
required value is an error). It works with `docker compose` and `podman compose` (`RECAP_SETUP_COMPOSE` picks one).

- It writes what the manual steps below write, from the files of the release next to it. Modes: `.env` `0600`,
  `compose.yaml`, `config/*.yaml` and the secret files `0644` in `0755` directories, so that the containers (user
  ID 10001) can read them. Only the install directory protects the secrets, so it must be `0700`; the script refuses
  to run otherwise.
- It never overwrites an existing file and never changes the mode of an existing directory. A second run changes
  nothing and asks only for what is missing; an answer for a file it keeps is reported as ignored. Run it again with
  a new BBB server or front-end ID to create that token; it then recreates `api` and `worker`.
- `--force` rewrites `compose.yaml`, `.env` and `config/*.yaml` from the answers, so hand edits in them are lost;
  each old file is kept as `<file>.bak.<time>`. It never touches a secret.
- It prints no secret except the client tokens created in that run. Provider and Docs keys are read without echo, or
  from `RECAP_SETUP_*_KEY_FILE`. With `--non-interactive` in Ansible or CI the tokens land in the job log: hide the
  output (Ansible `no_log: true`) and hand the tokens on from `secrets/clients/` instead.
- It configures one ASR and one LLM profile and the Docs and front-end sinks. Edit `config/*.yaml` for anything else
  (slides, webhook or Matrix sinks, other privacy declarations), then run `check-config` and recreate `api` and
  `worker` as in the table above.

### Manual setup

The same as `setup.sh`, by hand (the admin UI: [below](#admin-ui-with-keycloak)):

1. Get the files of a release ([release.md](release.md) lists the tags):

   ```sh
   RECAP_VERSION=v0.1.0
   git clone --depth 1 --branch "$RECAP_VERSION" https://github.com/virtUOS/bbb-recap.git /tmp/bbb-recap
   sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /opt/recap
   cp -r /tmp/bbb-recap/deploy/compose.yaml /tmp/bbb-recap/deploy/config /opt/recap/
   cp /tmp/bbb-recap/deploy/.env.example /opt/recap/.env
   cd /opt/recap
   sed -i "s|^RECAP_IMAGE=.*|RECAP_IMAGE=ghcr.io/virtuos/bbb-recap/recap:$RECAP_VERSION|" .env
   ```

2. Edit the settings. In `config/providers.yaml` and `config/sinks.yaml` replace every `REPLACE` value. Delete the
   sink you do not use. `.env` needs no change for a first start.

   ```sh
   ${EDITOR:-vi} .env config/providers.yaml config/sinks.yaml
   grep -v '^ *#' config/*.yaml | grep REPLACE   # expect no output
   ```

3. Create the secrets: one token per BBB server (here `bbb-1`; a BBB cluster shares one, see
   [install-exporter.md](install-exporter.md#bbb-cluster-or-scalelite)) and per front end (here `pilos-1`). Paste the
   keys you got from your ASR, LLM and Docs operators into the three key files.

   ```sh
   mkdir -p secrets/clients secrets/providers secrets/sinks
   openssl rand -hex 32 > secrets/db-password
   openssl rand -hex 32 > secrets/clients/bbb_server.bbb-1
   openssl rand -hex 32 > secrets/clients/frontend.pilos-1
   openssl rand -hex 32 > secrets/clients/admin.ops
   ${EDITOR:-vi} secrets/providers/asr.key secrets/providers/llm.key secrets/sinks/docs.key
   chmod -R a+rX secrets                # the containers read them; /opt/recap itself stays 0700
   ```

4. Check the config and start:

   ```sh
   docker compose run --rm --no-deps api check-config   # expect: configuration is valid for [api worker]
   docker compose up -d
   docker compose ps                                    # postgres, api, worker healthy; migrate exited (0)
   docker compose exec api /usr/local/bin/recap healthcheck && echo api ready
   docker compose exec worker /usr/local/bin/recap healthcheck && echo worker ready
   ```

### Upgrade and rollback

Read the release notes first. Migrations only add, so the previous release runs on the new schema.

```sh
cd /opt/recap
RECAP_VERSION=v0.2.0
rm -rf /tmp/bbb-recap && git clone --depth 1 --branch "$RECAP_VERSION" https://github.com/virtUOS/bbb-recap.git /tmp/bbb-recap
cp /tmp/bbb-recap/deploy/compose.yaml .
diff .env /tmp/bbb-recap/deploy/.env.example          # add new settings to .env
sed -i "s|^RECAP_IMAGE=.*|RECAP_IMAGE=ghcr.io/virtuos/bbb-recap/recap:$RECAP_VERSION|" .env
docker compose pull api
docker compose run --rm --no-deps api check-config
docker compose up -d
```

To roll back, set the previous tag the same way. Rolling
back past a release whose notes announce a "contract" migration needs a database restore.

### BBB servers and front ends

Each client is a token file in `secrets/clients/`. IDs use `a-z`, `0-9` and `-`. To add BBB server `bbb-2`: run
`deploy/setup.sh` again and give `bbb-2` (it creates the token and recreates `api` and `worker`), or create
`clients/bbb_server.bbb-2` as in [Manual setup](#manual-setup) step 3 and recreate `api`. A BBB cluster behind a load
balancer such as Scalelite needs only one token for all its servers
([install-exporter.md](install-exporter.md#bbb-cluster-or-scalelite)).

By default every front end may claim recordings of every BBB server. Set `RECAP_FRONTEND_SERVERS` in `.env` only when
several organisations share one recap, so that one front end cannot claim the meetings of another organisation's
servers: list each pair, e.g. `RECAP_FRONTEND_SERVERS=pilos-a:bbb-a,moodle-b:bbb-b`, and recreate `api` and `worker`.

- **BBB server:** give its admin the service URL and the content of `clients/bbb_server.bbb-2`. They install the
  exporter with [install-exporter.md](install-exporter.md).
- **Front end** (e.g. PILOS with a recap integration): give it the service URL and the content of
  `clients/frontend.pilos-1` as bearer token. It names a meeting type from `meeting-types.yaml` and a sink from
  `sinks.yaml` ([metadata-interface.md](../reference/metadata-interface.md)).
- **Rotate a token:** write the new value to `<file>.next`, recreate `api`, switch the client over, then rename
  `<file>.next` to `<file>` and recreate `api` again.

### Admin UI with Keycloak

The admin UI is off until `RECAP_OIDC_ISSUER` is set. The admin API (`clients/admin.<name>`) works either way.
`setup.sh` does step 2 on its first run when it gets the OIDC issuer.

1. In Keycloak, in your realm: create the realm role `recap-admin` and give it to the admins. Create the client
   `recap-admin-ui` with client authentication on, only the standard flow, PKCE method `S256`, and the redirect URI
   `https://recap.example.org/admin/callback`. Add a mapper to its dedicated scope: type "User Realm Role", token claim
   name `roles`, multivalued, added to the ID token. Copy the client secret.
2. On the recap host:

   ```sh
   cd /opt/recap
   ${EDITOR:-vi} secrets/oidc-client-secret         # paste the client secret
   openssl rand -hex 32 > secrets/session-key
   chmod -R a+rX secrets
   ${EDITOR:-vi} .env   # RECAP_PUBLIC_BASE_URL, RECAP_OIDC_ISSUER (realm URL), RECAP_OIDC_CLIENT_ID=recap-admin-ui
   docker compose up -d --force-recreate api
   ```

3. Open `https://recap.example.org/admin/` and log in. Users without `recap-admin` are refused.

### Private CA

If a provider, Docs, Keycloak or an external database uses a certificate from a private CA, build a bundle of the
public CAs plus yours (it replaces the system store) and mount it. Compose reads `compose.override.yaml` next to
`compose.yaml` on its own:

```sh
cd /opt/recap
cat /etc/ssl/certs/ca-certificates.crt /path/to/private-ca.crt > ca-bundle.pem   # Fedora/RHEL: /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem
chmod 0644 ca-bundle.pem
cat > compose.override.yaml <<'EOF'
services:
  api: &ca
    environment:
      SSL_CERT_FILE: /etc/recap/ca-bundle.pem
    volumes:
      - ./ca-bundle.pem:/etc/recap/ca-bundle.pem:ro,z
  worker: *ca
  migrate: *ca
EOF
docker compose up -d
```

### Existing Postgres or S3

- Postgres: set `RECAP_DB_DSN` without password (`sslmode=verify-full`); the password stays in `secrets/db-password`.
  The bundled `postgres` still starts but holds no data.
- S3 instead of the `blobdata` volume: set `RECAP_BLOB_BACKEND=s3`, `RECAP_S3_ENDPOINT` and `RECAP_S3_BUCKET`, put the
  keys into `secrets/s3-access-key-id` and `secrets/s3-secret-access-key`. Create the bucket once, with versioning off.

## Troubleshooting

`docker compose logs --tail 50 api worker` shows the reason; `docker compose run --rm --no-deps api check-config`
names configuration errors and warns about likely typos. Held and failed jobs are listed by the admin UI or the admin API, e.g.
`curl -sS -H "Authorization: Bearer $(cat secrets/clients/admin.ops)" "https://recap.example.org/api/v1/admin/jobs?state=held"`.

| Symptom                                                                                                   | Cause                                                                                                                                                                                      | Fix                                                                                                                                                                                                                                                |
| --------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `api` does not start: `client secrets: bbb_server.a and bbb_server.b hold the same token`                 | two credential files hold one token, typically after a client ID was renamed by copying its file                                                                                           | two clients cannot share a token: remove the file of the old ID and recreate `api`                                                                                                                                                                 |
| `check-config` or the `api` log warns `front end has no credential file; check the ID for a typo`         | an ID in `RECAP_FRONTEND_SERVERS`, `RECAP_FRONTEND_TYPES` or `RECAP_FRONTEND_SINKS` has no `clients/frontend.<id>`; without `RECAP_FRONTEND_SERVERS` the real front end stays unrestricted | correct the ID in `.env`; the INFO line `front end access` shows what each front end may claim and request                                                                                                                                         |
| Jobs held with `no_registration`; the front end logs failed registrations (e.g. HTTP 405 or an HTML page) | the front end's service URL points to another host or path, so a different web server answers its `PUT /api/v1/registrations/…`                                                            | set the front end's service URL to the recap base URL (`https://recap.example.org`, no path). A late registration releases the held job. PILOS: the failed registration is in its failed jobs table                                                |
| Summary fails with `output_truncated`                                                                     | the LLM hit `max_output_tokens` (`finish_reason` `length`); a reasoning model may spend all of it on thinking                                                                              | turn thinking off in the LLM profile, e.g. `extra_body: {chat_template_kwargs: {enable_thinking: false}}` for Qwen on vLLM, or raise `max_output_tokens` ([providers.md](../reference/providers.md) §3.2); recreate `worker`, then `retry` the job |
| The summary ignores the slides                                                                            | the meeting type does not switch slides on                                                                                                                                                 | add `inputs: {slides: true}` to the type in `config/meeting-types.yaml`, list `slides` in the LLM declaration's `data_categories`, recreate `api` and `worker`                                                                                     |
| A job that an admin retried after `failed` is delivered, but the front end never shows the summary        | the front end stopped polling when it saw `failed`                                                                                                                                         | make the front end fetch the result again. PILOS: dispatch its summary poll job for that meeting again                                                                                                                                             |
| `https://recap.example.org/` answers `404`                                                                | expected: there is no page at `/`                                                                                                                                                          | the admin UI is at `/admin/` (with `RECAP_OIDC_ISSUER` set)                                                                                                                                                                                        |
| `https://recap.example.org/…` answers `502`                                                               | the reverse proxy cannot reach the api                                                                                                                                                     | check `docker compose ps` (api healthy) and that the proxy forwards to `127.0.0.1:8080`                                                                                                                                                            |
