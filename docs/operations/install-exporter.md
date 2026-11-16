# Install the exporter on a BBB server

You get two parts on each BBB server: the `recap-exporter` daemon in a container, which uploads opted-in recordings
to the service, and the `post_archive` hook, a small Ruby script that runs inside BBB's recording worker and queues
each new recording for the daemon. You need BBB 4.0 or later (packaged or the docker compose setup), docker with the
compose plugin, the service URL (`https://`) and this server's token: the content of
`secrets/clients/bbb_server.<id>` on the service ([install-compose.md](install-compose.md)).

## Quick start

As root on the BBB server (`sudo -i`):

1. Set the variables. Keep the block that matches your BBB install.

   ```sh
   VERSION=v0.1.0                              # same release as the service
   SERVICE_URL=https://recap.example.org
   BBB_SERVER_ID=bbb-1
   SRC=https://raw.githubusercontent.com/virtUOS/bbb-recap/$VERSION
   # Packaged BBB:
   HOOK_GID=$(id -g bigbluebutton)
   BBB_RECORDING_DIR=/var/bigbluebutton/recording
   # Docker BBB instead (BBB_DIR holds BBB's docker-compose.yml):
   # BBB_DIR=/srv/bbb; HOOK_GID=998; BBB_RECORDING_DIR=$BBB_DIR/data/bigbluebutton/recording
   ```

2. Create the user, the directories, the config and the token:

   ```sh
   id recap-exporter >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin recap-exporter
   install -d -m 0755 /etc/recap-exporter /var/lib/recap-exporter /opt/recap-exporter
   install -d -m 2770 -o recap-exporter -g "$HOOK_GID" /var/lib/recap-exporter/spool
   install -d -m 0700 -o recap-exporter -g recap-exporter /var/lib/recap-exporter/state /etc/recap-exporter/secrets
   printf 'RECAP_EXPORTER_SERVICE_URL=%s\nRECAP_EXPORTER_BBB_SERVER_ID=%s\n' "$SERVICE_URL" "$BBB_SERVER_ID" \
     > /etc/recap-exporter/exporter.env
   read -rs TOKEN                              # paste the token, then Enter
   printf '%s' "$TOKEN" > /etc/recap-exporter/secrets/ingest-token; unset TOKEN
   chown recap-exporter:recap-exporter /etc/recap-exporter/secrets/ingest-token
   chmod 0400 /etc/recap-exporter/secrets/ingest-token
   ```

3. Start the daemon:

   ```sh
   cd /opt/recap-exporter
   curl -fsSL -o compose.yaml "$SRC/deploy/exporter/compose.yaml"
   cat > .env <<EOF
   EXPORTER_IMAGE=ghcr.io/virtuos/bbb-recap/recap-exporter:$VERSION
   EXPORTER_UID=$(id -u recap-exporter)
   EXPORTER_GID=$(id -g recap-exporter)
   HOOK_GID=$HOOK_GID
   BBB_RECORDING_DIR=$BBB_RECORDING_DIR
   RAW_MOUNT=ro
   EOF
   docker compose run --rm exporter check-config     # expect: config OK
   docker compose up -d
   curl -s http://127.0.0.1:9466/readyz              # expect: ok
   ```

4. Install the hook. **Packaged BBB:**

   ```sh
   curl -fsSL -o /usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb "$SRC/hook/post_archive/recap_export.rb"
   chmod 0644 /usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb
   sudo -u bigbluebutton test -w /var/lib/recap-exporter/spool && echo spool writable
   ```

   **Docker BBB:** mount the hook, the config and the spool into `recording-workers`. If a
   `docker-compose.override.yml` exists already, add the three volumes to it instead.

   ```sh
   curl -fsSL -o /etc/recap-exporter/recap_export.rb "$SRC/hook/post_archive/recap_export.rb"
   chmod 0644 /etc/recap-exporter/recap_export.rb
   cat > "$BBB_DIR/docker-compose.override.yml" <<'EOF'
   services:
     recording-workers:
       volumes:
         - /etc/recap-exporter/recap_export.rb:/usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb:ro,z
         - /etc/recap-exporter/exporter.env:/etc/recap-exporter/exporter.env:ro,z
         - /var/lib/recap-exporter/spool:/var/lib/recap-exporter/spool:z
   EOF
   cd "$BBB_DIR" && docker compose up -d recording-workers
   ```

5. Test with a synthetic meeting that a front end registered for recap: record, stop, end. About a minute after BBB
   archived it, `cd /opt/recap-exporter && docker compose exec exporter /usr/local/bin/recap-exporter status` shows
   the recording as `acked`.

Optional: the BBB plugin adds an "AI summary" button that starts and pauses the recording
([install-plugin.md](install-plugin.md)).

## Reference

| Path on the host                           | Owner, mode                         | Content                                                                                        |
| ------------------------------------------ | ----------------------------------- | ---------------------------------------------------------------------------------------------- |
| `/opt/recap-exporter/compose.yaml`, `.env` | root                                | the container ([`.env.example`](../../deploy/exporter/.env.example))                           |
| `/etc/recap-exporter/exporter.env`         | root, `0644`                        | settings of daemon and hook, no secrets ([`exporter.env`](../../deploy/exporter/exporter.env)) |
| `/etc/recap-exporter/secrets/ingest-token` | `recap-exporter`, `0400`            | the token                                                                                      |
| `/var/lib/recap-exporter/spool`            | `recap-exporter`:hook group, `2770` | written by the hook, read by the daemon                                                        |
| `/var/lib/recap-exporter/state`            | `recap-exporter`, `0700`            | the daemon's state per recording                                                               |

| BBB install type                       | Hook runs in                      | Hook user       | `raw/` on the host                          |
| -------------------------------------- | --------------------------------- | --------------- | ------------------------------------------- |
| packaged (Debian packages)             | BBB's resque worker on the host   | `bigbluebutton` | `/var/bigbluebutton/recording/raw`          |
| docker (bigbluebutton PR #26186 setup) | the `recording-workers` container | UID/GID `998`   | `$BBB_DIR/data/bigbluebutton/recording/raw` |

| Task              | Command (in `/opt/recap-exporter`)                                                                                       |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Status            | `docker compose exec exporter /usr/local/bin/recap-exporter status [RECORD_ID]`                                          |
| Retry a recording | `docker compose stop exporter`, `docker compose run --rm exporter requeue "$RECORD_ID"`, `docker compose start exporter` |
| Logs              | `docker compose logs --tail 50 exporter`                                                                                 |
| Metrics, health   | `http://127.0.0.1:9466/metrics`, `/healthz`, `/readyz` (host only; do not expose publicly)                               |
| All settings      | [`exporter.env`](../../deploy/exporter/exporter.env), [deployment.md](../reference/deployment.md) §5.4                   |

### Upgrade and rollback

Upgrade the service first. Daemon and hook come from the same release:

```sh
VERSION=v0.2.0
SRC=https://raw.githubusercontent.com/virtUOS/bbb-recap/$VERSION
cd /opt/recap-exporter
sed -i "s|^EXPORTER_IMAGE=.*|EXPORTER_IMAGE=ghcr.io/virtuos/bbb-recap/recap-exporter:$VERSION|" .env
curl -fsSL -o compose.yaml "$SRC/deploy/exporter/compose.yaml"
docker compose pull && docker compose run --rm exporter check-config && docker compose up -d
```

Then download the hook again as in step 4. To roll back, do the same with the previous version. Check that the hook
is still there after each BBB package upgrade (not verified whether upgrades keep it).

### BBB cluster or Scalelite

Behind a load balancer such as Scalelite, treat the cluster as one BBB server for recap:

- On the recap host create one token for the whole cluster, e.g. `secrets/clients/bbb_server.bbb-cluster`.
- Install the exporter on every BBB node with the same token and `BBB_SERVER_ID=bbb-cluster`. Nodes can be added
  and removed without changes on recap.
- In Scalelite, do not list any `meta_recap-*` parameter in `CREATE_EXCLUDE_PARAMS`. Scalelite passes the other
  `create` parameters on to the BBB node and returns the node's response, so the front end gets the right recording ID
  ([metadata-interface.md](../reference/metadata-interface.md) §4).

Trade-offs:

- One leaked node token means a new token for all nodes. Rotate it with the `.next` file on recap
  ([install-compose.md](install-compose.md#bbb-servers-and-front-ends)), write the new token to
  `/etc/recap-exporter/secrets/ingest-token` on every node and run `docker compose restart exporter` there; the
  exporter reads the token at start.
- Logs, metrics and the admin UI show the cluster ID, not the node. The exporter on each node still shows its own
  recordings (`status`, its own metrics).

### Read access to `raw/`

The daemon runs as `recap-exporter` with the hook's group and must read everything below `raw/`. On docker BBB `raw/`
was world-readable on a test install. On packaged BBB this is not checked. This lists files the daemon cannot read
(expect no output):

```sh
find "$BBB_RECORDING_DIR/raw" -mindepth 1 \( -type d ! -perm -0005 ! -perm -0050 \) -o \( -type f ! -perm -0004 ! -perm -0040 \) | head
```

### Delete raw data after upload

By default the exporter never deletes anything. To delete `raw/<record_id>` once the service has the package and BBB
has published the recording, add to `exporter.env` and allow writes to `raw/`:

```sh
printf 'RECAP_EXPORTER_DELETE_POLICY=after_ack\nRECAP_EXPORTER_RECORDING_FORMATS=presentation\n' >> /etc/recap-exporter/exporter.env
cd /opt/recap-exporter && sed -i 's/^RAW_MOUNT=.*/RAW_MOUNT=rw/' .env && docker compose up -d
```

Independent of this: on a BBB server that also runs the Opencast `post_archive` integration, set its
`deleteIfSuccessful = false`, or it can delete `raw/` before the exporter reads it.

### Private CA

If the service uses a certificate from a private CA, mount a bundle of the public CAs plus yours (it replaces the
system store):

```sh
cat /etc/ssl/certs/ca-certificates.crt /path/to/private-ca.crt > /etc/recap-exporter/ca-bundle.pem
chmod 0644 /etc/recap-exporter/ca-bundle.pem
cat > /opt/recap-exporter/compose.override.yaml <<'EOF'
services:
  exporter:
    environment:
      SSL_CERT_FILE: /etc/recap-exporter/ca-bundle.pem
    volumes:
      - /etc/recap-exporter/ca-bundle.pem:/etc/recap-exporter/ca-bundle.pem:ro,z
EOF
cd /opt/recap-exporter && docker compose up -d
```

### Sources

- Packaged BBB: worker user `bigbluebutton`: `record-and-playback/core/systemd/bbb-rap-resque-worker.service:14 @ 7d27e7e`;
  recording dir: `record-and-playback/core/scripts/bigbluebutton.yml:66 @ 7d27e7e`.
- Docker BBB: services, mounts, UID/GID 998: `docker-compose.yml` and `docker/README.md` of bigbluebutton/bigbluebutton
  PR #26186 @ `c3d6b3e` ([bbb-exporter.md](../reference/bbb-exporter.md) §8.6).
- More: [upstream-facts.md](../reference/upstream-facts.md).
