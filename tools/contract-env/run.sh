#!/bin/sh
# Contract environment: runs the service (Postgres, migrate, api, worker) in
# podman for the HTTP contract tests, without host Postgres and without
# pulling images. Several environments can run side by side under different
# names: pick a unique NAME per run, e.g. recap-t021-$$, so that it clashes
# neither with another environment nor with a test database container.
#
#   run.sh up NAME HOSTPORT   start NAME-pg, NAME-migrate, NAME-mock, NAME-api, NAME-worker;
#                             the api (:8080) is published on 127.0.0.1:HOSTPORT
#   run.sh down NAME          remove the containers, volume, network and temp files
#   run.sh logs NAME FILE     write the api, worker and mock logs (JSON lines) to FILE
#
# The service binary is bin/recap, bind-mounted read-only into the Postgres
# image. It must be static: make build (CGO_ENABLED=0). up checks this with
# `go version -m`, so it needs the Go toolchain on the host. bin/recap-mock
# (mock ASR, LLM and Docs) runs the same way as NAME-mock, network alias
# mock, with testdata/ mounted read-only; config/providers.yaml points the
# provider profiles at http://mock:8080. up copies the repo's formats/ into
# the configuration, so the worker finds them at /etc/recap/formats.
#
# RECAP_CONTRACT_DOCS_ADDR (host:port, set at up): adds the sink profile
# docs-contract with docs_base_url http://RECAP_CONTRACT_DOCS_ADDR
# (contracts/README.md). The address must be reachable from the containers, e.g.
# host.containers.internal:PORT for a mock Docs server on the host.
# If that is refused (some rootless podman networks), use the host's LAN address.
#
# RECAP_CONTRACT_RESULT_TTL (duration, set at up and for the tests): the
# RECAP_RESULT_TTL of api and worker, default 7d. The slow lifetime tests of
# contracts/registration run only when it is set (contracts/README.md); use a
# value above RECAP_RETENTION_AFTER_DELIVERY (1h), e.g. 90m. The config has
# the sink profile frontend-contract (type frontend), so it must not be 0.
#
# Then, for the contract tests:
#   RECAP_CONTRACT_BASE_URL=http://127.0.0.1:HOSTPORT
#   RECAP_CONTRACT_LOG_FILE=FILE (after run.sh logs NAME FILE)
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE=docker.io/library/postgres:18-alpine
STATE_BASE=${TMPDIR:-/tmp}/recap-contract-env
RESULT_TTL=${RECAP_CONTRACT_RESULT_TTL:-7d}
DB_PASSWORD=FIXTURE-NOT-A-SECRET-db # fixture value (docs/reference/README.md "Fixtures")

die() {
	echo "contract-env: $*" >&2
	exit 1
}

usage() {
	echo "usage: $0 up NAME HOSTPORT | down NAME | logs NAME FILE" >&2
	exit 2
}

check_name() {
	case $1 in
	'' | *[!a-z0-9-]*) die "NAME must use only a-z, 0-9 and -: got '$1'" ;;
	esac
}

# recap_container CONTAINER COMMAND [PODMAN-RUN-OPTIONS...] runs bin/recap
# COMMAND as user 10001 in the present image, with the fixture configuration.
recap_container() {
	container=$1 command=$2
	shift 2
	podman run --pull=never --security-opt label=disable "$@" \
		--name "$container" --network "$NAME-net" --user 10001:10001 --read-only \
		--entrypoint /usr/local/bin/recap \
		-v "$ROOT/bin/recap:/usr/local/bin/recap:ro" \
		-v "$ROOT/testdata/secrets/clients:/run/secrets/recap/clients:ro" \
		-v "$ROOT/testdata/secrets/docs-s2s.key:/run/secrets/recap/sinks/docs-s2s.key:ro" \
		-v "$ROOT/testdata/secrets/asr-fixture.key:/run/secrets/recap/providers/asr-fixture.key:ro" \
		-v "$ROOT/testdata/secrets/llm-fixture.key:/run/secrets/recap/providers/llm-fixture.key:ro" \
		-v "$STATE/config:/etc/recap:ro" \
		-v "$STATE/db-password:/run/secrets/recap/db-password:ro" \
		-v "$NAME-blobs:/var/lib/recap/blobs:U" \
		-e RECAP_DB_DSN="postgres://recap@$NAME-pg:5432/recap?sslmode=disable" \
		-e RECAP_DB_PASSWORD_FILE=/run/secrets/recap/db-password \
		-e RECAP_BLOB_BACKEND=fs \
		-e RECAP_FS_ROOT=/var/lib/recap/blobs \
		-e RECAP_FRONTEND_SERVERS=pilos-fixture:bbb-fixture-a \
		-e RECAP_META_PREFIX=recap \
		-e RECAP_RETENTION_AFTER_DELIVERY=1h \
		-e RECAP_RESULT_TTL="$RESULT_TTL" \
		-e RECAP_LOG_LEVEL=debug \
		"$IMAGE" "$command"
}

# mock_container starts NAME-mock: bin/recap-mock as user 10001 in the
# present image, alias mock on the network, fixtures and keys read-only.
mock_container() {
	podman run -d --pull=never --security-opt label=disable \
		--name "$NAME-mock" --network "$NAME-net" --network-alias mock --user 10001:10001 --read-only \
		--entrypoint /usr/local/bin/recap-mock \
		-v "$ROOT/bin/recap-mock:/usr/local/bin/recap-mock:ro" \
		-v "$ROOT/testdata:/fixtures:ro" \
		"$IMAGE" -listen :8080 -fixtures /fixtures -log-level debug \
		-asr-key /fixtures/secrets/asr-fixture.key -llm-key /fixtures/secrets/llm-fixture.key
}

# add_docs_contract ADDR prints the docs-contract sink profile: docs-main
# (testdata/sink/sinks.yaml) with the base URL http://ADDR.
add_docs_contract() {
	case $1 in
	*[!A-Za-z0-9.:-]* | *:) die "RECAP_CONTRACT_DOCS_ADDR must be host:port: got '$1'" ;;
	*:*) ;;
	*) die "RECAP_CONTRACT_DOCS_ADDR must be host:port: got '$1'" ;;
	esac
	cat <<EOF
  -
    name: docs-contract
    type: docs
    docs_base_url: http://$1
    api_key_file: /run/secrets/recap/sinks/docs-s2s.key
    document_url_template: "http://$1/docs/{id}/"
    request_timeout: 60s
    max_inflight: 2
    max_document_bytes: 2097152
    declaration:
      data_categories: [summary, email]
      operator: "operator's Docs instance"
      location: on_prem
      retention: "Document owned by the user in Docs; Docs retention policy"
      deletion: manual
      used_for_training: "no"
      contract_ref: none
      reviewed_by: fixture-reviewer
      reviewed_at: "2026-10-29"
EOF
}

up() {
	case $PORT in
	'' | *[!0-9]*) die "HOSTPORT must be a number: got '$PORT'" ;;
	esac
	command -v podman >/dev/null 2>&1 || die "podman not found"
	podman image exists "$IMAGE" || die "image $IMAGE is not present (it is never pulled)"
	command -v go >/dev/null 2>&1 || die "go not found: needed to check that bin/recap is static"
	for bin in recap recap-mock; do
		[ -x "$ROOT/bin/$bin" ] || die "$ROOT/bin/$bin missing: run make build"
		go version -m "$ROOT/bin/$bin" | grep -q 'CGO_ENABLED=0' ||
			die "$ROOT/bin/$bin is not static: run make build (CGO_ENABLED=0)"
	done
	if podman container exists "$NAME-pg"; then
		die "$NAME is already up: run '$0 down $NAME' first"
	fi

	mkdir -p "$STATE"
	cp -R "$ROOT/tools/contract-env/config" "$STATE/config"
	# The repo's formats at the default RECAP_FORMATS_DIR (/etc/recap/formats),
	# as the image ships them (templates.md §1).
	mkdir -p "$STATE/config/formats"
	cp -R "$ROOT/formats/." "$STATE/config/formats/"
	if [ -n "${RECAP_CONTRACT_DOCS_ADDR:-}" ]; then
		add_docs_contract "$RECAP_CONTRACT_DOCS_ADDR" >>"$STATE/config/sinks.yaml"
	fi
	printf '%s\n' "$DB_PASSWORD" >"$STATE/db-password"
	chmod 0644 "$STATE/db-password" # readable by uid 10001 in the containers

	podman network create "$NAME-net" >/dev/null || die "podman network create $NAME-net failed"
	podman volume create "$NAME-blobs" >/dev/null || die "podman volume create $NAME-blobs failed"

	podman run -d --pull=never --security-opt label=disable --name "$NAME-pg" --network "$NAME-net" \
		-e POSTGRES_USER=recap -e POSTGRES_DB=recap \
		-e POSTGRES_PASSWORD_FILE=/run/secrets/db-password \
		-v "$STATE/db-password:/run/secrets/db-password:ro" \
		"$IMAGE" >/dev/null || die "cannot start $NAME-pg"
	# TCP on 127.0.0.1 answers only once the init scripts are done.
	i=0
	until podman exec "$NAME-pg" pg_isready -q -h 127.0.0.1 -U recap; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || die "$NAME-pg not ready after 60 s"
		sleep 1
	done

	recap_container "$NAME-migrate" migrate --rm || die "recap migrate failed"
	mock_container >/dev/null || die "cannot start $NAME-mock"
	recap_container "$NAME-api" api -d -p "127.0.0.1:$PORT:8080" >/dev/null || die "cannot start $NAME-api"
	recap_container "$NAME-worker" worker -d >/dev/null || die "cannot start $NAME-worker"

	i=0
	until curl -s -o /dev/null "http://127.0.0.1:$PORT/"; do
		i=$((i + 1))
		[ "$i" -lt 30 ] || die "$NAME-api does not answer on 127.0.0.1:$PORT after 30 s"
		sleep 1
	done
	echo "contract-env: $NAME is up: RECAP_CONTRACT_BASE_URL=http://127.0.0.1:$PORT"
}

down() {
	podman rm -f -v -t 0 "$NAME-api" "$NAME-worker" "$NAME-mock" "$NAME-migrate" "$NAME-pg" >/dev/null 2>&1 || true
	podman volume rm -f "$NAME-blobs" >/dev/null 2>&1 || true
	podman network rm -f "$NAME-net" >/dev/null 2>&1 || true
	rm -rf "$STATE"
	echo "contract-env: $NAME removed"
}

logs() {
	{
		podman logs "$NAME-api"
		podman logs "$NAME-worker"
		podman logs "$NAME-mock"
	} >"$FILE" 2>&1 || die "cannot read the logs of $NAME"
}

[ $# -ge 2 ] || usage
CMD=$1 NAME=$2
check_name "$NAME"
STATE=$STATE_BASE/$NAME
case $CMD in
up)
	[ $# -eq 3 ] || usage
	PORT=$3
	up
	;;
down)
	[ $# -eq 2 ] || usage
	down
	;;
logs)
	[ $# -eq 3 ] || usage
	FILE=$3
	logs
	;;
*) usage ;;
esac
