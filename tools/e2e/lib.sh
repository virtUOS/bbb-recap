# shellcheck shell=sh
# Shared helpers for the end-to-end scenarios in tools/e2e/ (sourced, not run).
#
# The scenarios run deploy/compose.yaml with the dev overlay tools/dev/
# (recap-mock for ASR, LLM and Docs, MinIO for S3) under their own project
# name, talk to it with curl on 127.0.0.1 and remove it with its volumes at
# the end. Run `make images` first: localhost/recap* exist in no registry.
# The scripts never pull; before compose up, e2e_up checks that the image of every service it starts and every
# image in E2E_IMAGES is present (e2e_require_images); compose up then has
# nothing to pull.
#
# Blobs: dev.env selects S3 on the dev MinIO. A scenario that exports
# RECAP_BLOB_BACKEND=fs runs api and worker on the compose default instead,
# the fs backend on the volume blobdata, and starts neither minio nor
# minio-init.
#
# Naming: the helpers expect podman-compose (1.x) names, containers
# PROJECT_<service>_1 and network PROJECT_default. e2e_up fails fast if the
# containers do not show up under these names (e.g. with docker compose v2,
# which names them PROJECT-<service>-1).
#
# A scenario sets these before calling the helpers:
#   PROJECT    compose project name (containers are PROJECT_<service>_1)
#   HOSTPORT   host port of api (127.0.0.1 only)
#   HTTPSPORT  host port of the dev TLS front tls (only if it starts tls)
#   MOCKPORT   host port of mock (its OIDC login), optional; default HOSTPORT+1
#   WORK       a private temp dir (e2e_init creates it), removed by e2e_cleanup
#   E2E_PIDS   background processes e2e_cleanup stops with SIGTERM (optional)
#   E2E_IMAGES images the scenario runs itself with podman run (optional)
# and may set KEEP=1 to leave the stack and WORK, TIMEOUT (seconds, default
# 180) for every wait, LOG_FILE to keep the service logs of e2e_scan_logs.
#
# Output: one line per check, "ok   <check>" or "FAIL <check>". e2e_fail
# exits 1; e2e_usage_error and missing tools exit 2. A scenario sets
# `trap e2e_cleanup EXIT` before e2e_init.

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
DEV_DIR=$ROOT/tools/dev
SECRETS_DIR=$DEV_DIR/secrets
CLIENTS_DIR=$ROOT/testdata/secrets/clients
MINIO_IMAGE=docker.io/minio/minio:RELEASE.2025-04-22T22-12-26Z
# Lists the files of the blobdata volume (fs backend).
ALPINE_IMAGE=docker.io/library/alpine:3.24
TIMEOUT=${TIMEOUT:-180}
KEEP=${KEEP:-0}

# Fixture strings that must never show up in logs (privacy.md §6): names,
# emails, subs, BBB user IDs, content, and the prefix of every fixture secret.
CANARIES='testperson|widget|fixture meeting|fixture-meeting-|fixture-sub-|@example.org|FIXTURE-NOT-A-SECRET|w_fix'
# What cleanup must remove from Postgres (privacy.md §5.1, §5.2): owner data
# (S2), content and BBB user IDs (package_files paths; cleanup deletes those
# rows).
DB_CANARIES='testperson|widget|fixture meeting|fixture-sub-|@example.org|w_fix'

e2e_ok() {
	echo "ok   $*"
}

e2e_info() {
	echo "     $*"
}

e2e_fail() {
	echo "FAIL $*"
	exit 1
}

e2e_usage_error() {
	echo "$*" >&2
	exit 2
}

# e2e_require_tools TOOL... exits 2 naming every missing tool.
e2e_require_tools() {
	missing=
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
	done
	[ -z "$missing" ] || e2e_usage_error "missing tools:$missing"
}

# e2e_require_images IMAGE... exits 2 naming every image that is not in
# local storage. It only looks (podman image exists); it never pulls.
e2e_require_images() {
	missing=
	for i in "$@"; do
		podman image exists "$i" || missing="$missing $i"
	done
	[ -z "$missing" ] || e2e_usage_error "missing images (the scripts never pull; make images builds localhost/recap*):$missing"
}

# e2e_service_images SERVICE... prints the image of each SERVICE, one per
# line, as `podman compose config` resolves it, so RECAP_IMAGE and
# RECAP_MOCK_IMAGE count. podman-compose prints the merged model as YAML:
# service names at two spaces under services:, their keys at four.
e2e_service_images() {
	e2e_compose config >"$WORK/config.yaml" 2>"$WORK/config.err" || return 1
	awk -v want=" $* " '
		/^[^ ]/ { in_services = ($0 == "services:"); next }
		in_services && /^  [^ ].*:$/ { svc = substr($0, 3, length($0) - 3); next }
		in_services && /^    image: / && index(want, " " svc " ") { print substr($0, 12) }
	' "$WORK/config.yaml"
}

# e2e_init creates WORK and the dev secrets (once; they are gitignored).
e2e_init() {
	E2E_START=$(date +%s)
	WORK=$(mktemp -d "${TMPDIR:-/tmp}/recap-e2e.XXXXXX")
	chmod 0700 "$WORK"
	if [ ! -d "$SECRETS_DIR" ]; then
		"$DEV_DIR/dev-secrets.sh" >/dev/null || e2e_fail "dev secrets: dev-secrets.sh failed"
	elif [ ! -f "$SECRETS_DIR/session-key" ]; then
		# Created before the admin UI secrets existed: regenerate (fixture values only).
		"$DEV_DIR/dev-secrets.sh" -f >/dev/null || e2e_fail "dev secrets: dev-secrets.sh -f failed"
	fi
}

# e2e_compose ARGS... runs podman compose on the dev stack of PROJECT. The
# admin UI URLs follow HOSTPORT and MOCKPORT, so parallel projects do not collide.
e2e_compose() {
	MOCKPORT=${MOCKPORT:-$((HOSTPORT + 1))}
	(
		cd "$ROOT" &&
			RECAP_HTTP_PORT=$HOSTPORT RECAP_HTTPS_PORT=${HTTPSPORT:-8443} RECAP_MOCK_PORT=$MOCKPORT \
				RECAP_PUBLIC_BASE_URL=http://127.0.0.1:$HOSTPORT RECAP_MOCK_OIDC_BROWSER_URL=http://127.0.0.1:$MOCKPORT/oidc \
				podman compose -p "$PROJECT" -f deploy/compose.yaml -f tools/dev/compose.yaml \
				--env-file tools/dev/dev.env "$@"
	)
}

# e2e_container SERVICE prints the container name of SERVICE.
e2e_container() {
	echo "${PROJECT}_$1_1"
}

# e2e_down removes the stack with its volumes and network.
e2e_down() {
	e2e_compose down -v -t 10 >/dev/null 2>&1 || true
}

# e2e_cleanup is the EXIT trap: E2E_PIDS stopped, stack down and WORK removed
# (both left with KEEP=1).
e2e_cleanup() {
	status=$?
	for pid in ${E2E_PIDS:-}; do
		kill -TERM "$pid" 2>/dev/null || continue
		i=0
		while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 20 ]; do
			sleep 1
			i=$((i + 1))
		done
		kill -KILL "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	if [ "$KEEP" = 1 ]; then
		e2e_info "KEEP=1: stack $PROJECT left up; remove it with: podman compose -p $PROJECT ... down -v"
		[ -z "${WORK:-}" ] || e2e_info "KEEP=1: scratch dir $WORK left"
	else
		e2e_down
		[ -z "${WORK:-}" ] || rm -rf "$WORK"
	fi
	exit "$status"
}

# e2e_wait_for DESCRIPTION SECONDS COMMAND... runs COMMAND every 2 s until it
# succeeds; fails after SECONDS.
e2e_wait_for() {
	what=$1 limit=$2
	shift 2
	waited=0
	until "$@"; do
		[ "$waited" -lt "$limit" ] || e2e_fail "timeout after ${limit}s: $what"
		sleep 2
		waited=$((waited + 2))
	done
}

# e2e_healthy SERVICE succeeds once the container reports healthy.
e2e_healthy() {
	[ "$(podman inspect --format '{{.State.Health.Status}}' "$(e2e_container "$1")" 2>/dev/null)" = healthy ]
}

# e2e_exited_ok SERVICE succeeds once a one-shot container exited 0.
e2e_exited_ok() {
	[ "$(podman inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$(e2e_container "$1")" 2>/dev/null)" = "exited 0" ]
}

# e2e_up SERVICE... checks that the images of SERVICE... and E2E_IMAGES are
# present (exit 2 otherwise), starts a fresh stack (down -v first) with
# SERVICE... and their dependencies, then waits until minio-init is done (if
# it is one of SERVICE...) and api and worker are healthy. SERVICE... names
# the dependencies too, so that their images are checked.
e2e_up() {
	e2e_service_images "$@" >"$WORK/images" || e2e_fail "compose config: $(tail -n 5 "$WORK/config.err")"
	[ "$(wc -l <"$WORK/images")" -eq $# ] || e2e_fail "compose config: not one image for each of: $*"
	# One image name per line, no spaces or glob characters.
	# shellcheck disable=SC2046,SC2086
	e2e_require_images $(sort -u "$WORK/images") ${E2E_IMAGES:-}
	e2e_down
	e2e_compose up -d "$@" >"$WORK/up.log" 2>&1 || {
		tail -n 20 "$WORK/up.log"
		e2e_fail "compose up"
	}
	for s in "$@"; do
		podman container exists "$(e2e_container "$s")" ||
			e2e_fail "compose up: no container $(e2e_container "$s") (podman-compose naming expected, see lib.sh)"
	done
	case " $* " in
	*" minio-init "*) e2e_wait_for "minio-init did not exit 0" "$TIMEOUT" e2e_exited_ok minio-init ;;
	esac
	e2e_wait_for "api not healthy" "$TIMEOUT" e2e_healthy api
	e2e_wait_for "worker not healthy" "$TIMEOUT" e2e_healthy worker
	e2e_ok "stack $PROJECT up (api on 127.0.0.1:$HOSTPORT)"
}

# e2e_auth NAME CLIENT writes the header file WORK/auth-NAME with the bearer
# token of testdata/secrets/clients/CLIENT. The token never goes on a command
# line; curl reads it with -H @file.
e2e_auth() {
	[ -r "$CLIENTS_DIR/$2" ] || e2e_usage_error "missing client token $CLIENTS_DIR/$2"
	tok=$(cat "$CLIENTS_DIR/$2")
	(
		umask 077
		printf 'Authorization: Bearer %s\n' "$tok" >"$WORK/auth-$1"
	)
	tok=
}

# e2e_http CHECK AUTH METHOD PATH [BODYFILE [CONTENT-TYPE]] sends a request
# to api with the header file WORK/auth-AUTH, writes the response body to
# WORK/body and sets HTTP_CODE. A transport error (no HTTP answer) fails
# CHECK. Call it directly, not in $(...), so that e2e_fail ends the script.
e2e_http() {
	check=$1 auth=$2 method=$3 path=$4 body=${5:-} ctype=${6:-application/json}
	set -- -sS -o "$WORK/body" -w '%{http_code}' -X "$method" -H "@$WORK/auth-$auth"
	if [ -n "$body" ]; then
		set -- "$@" -H "Content-Type: $ctype" --data-binary "@$body"
	fi
	HTTP_CODE=$(curl "$@" "http://127.0.0.1:$HOSTPORT$path" 2>"$WORK/curl.err") || {
		e2e_info "curl: $(cat "$WORK/curl.err")"
		e2e_fail "$check: no HTTP answer from api"
	}
}

# e2e_expect_status WANT GOT CHECK fails CHECK unless GOT is WANT.
e2e_expect_status() {
	[ "$2" = "$1" ] || {
		e2e_info "body: $(head -c 500 "$WORK/body" 2>/dev/null)"
		e2e_fail "$3: HTTP $2, want $1"
	}
}

# e2e_job_state JOB_ID writes the admin view of the job to WORK/job and sets
# JOB_STATE to "<state>/<held_reason or outcome>" (a bare state when neither
# is set; "http-<code>" when the API does not answer 200).
e2e_job_state() {
	e2e_http "job poll" admin GET "/api/v1/admin/jobs/$1"
	if [ "$HTTP_CODE" != 200 ]; then
		JOB_STATE=http-$HTTP_CODE
		return 0
	fi
	cp "$WORK/body" "$WORK/job"
	JOB_STATE=$(jq -r '.state + (if (.held_reason // .outcome) then "/" + (.held_reason // .outcome) else "" end)' \
		"$WORK/job")
}

# e2e_wait_job JOB_ID WANT polls the admin job API every 2 s until the job is
# in WANT ("state/reason"), prints every state it sees on the way, and fails
# after TIMEOUT naming the last state and printing the last job body.
e2e_wait_job() {
	job=$1 want=$2 waited=0 seen=
	while :; do
		e2e_job_state "$job"
		got=$JOB_STATE
		if [ "$got" != "$seen" ]; then
			[ "$got" = "$want" ] || e2e_info "job state: $got"
			seen=$got
		fi
		[ "$got" = "$want" ] && return 0
		if [ "$waited" -ge "$TIMEOUT" ]; then
			e2e_info "last job body: $(cat "$WORK/job" 2>/dev/null)"
			e2e_fail "timeout after ${TIMEOUT}s waiting for job $job to reach $want: job state is $got"
		fi
		sleep 2
		waited=$((waited + 2))
	done
}

# e2e_blob_count PREFIX prints the number of objects under PREFIX in the
# bucket (empty PREFIX: the whole bucket). mc runs in a throwaway container
# of the pinned MinIO image on the project network; the credentials are
# read inside the container from the mounted dev secrets.
e2e_blob_count() {
	podman run --rm --pull=never --security-opt label=disable --network "${PROJECT}_default" \
		--tmpfs /data --tmpfs /root/.mc \
		-v "$SECRETS_DIR/s3-access-key-id:/run/secrets/minio/s3-access-key-id:ro" \
		-v "$SECRETS_DIR/s3-secret-access-key:/run/secrets/minio/s3-secret-access-key:ro" \
		-e "PREFIX=$1" --entrypoint /bin/sh "$MINIO_IMAGE" -c '
			mc alias set local http://minio:9000 "$(cat /run/secrets/minio/s3-access-key-id)" \
				"$(cat /run/secrets/minio/s3-secret-access-key)" >/dev/null || exit 1
			mc ls --recursive "local/recap/$PREFIX" >/tmp/ls || exit 1
			wc -l </tmp/ls' 2>"$WORK/mc.err"
}

# e2e_check_docs_once DOC_ID fails unless the mock Docs logged exactly one
# create-for-owner request: 201, for a known owner (match "access" on the
# fixture users), with document DOC_ID.
e2e_check_docs_once() {
	podman logs "$(e2e_container mock)" 2>&1 | jq -c -R 'fromjson? | select(.msg == "mock docs request")' \
		>"$WORK/docs-requests"
	n=$(wc -l <"$WORK/docs-requests")
	[ "$n" -eq 1 ] || e2e_fail "mock Docs: $n create-for-owner requests, want 1"
	jq -e --arg d "$1" '.status == 201 and .owner == "access" and .document_id == $d' \
		"$WORK/docs-requests" >/dev/null || {
		e2e_info "request: $(cat "$WORK/docs-requests")"
		e2e_fail "mock Docs: the request is not 201/access for document $1"
	}
	e2e_ok "mock Docs: exactly 1 create-for-owner, 201, owner access, document $1"
}

# e2e_fs_blob_count PREFIX prints the number of files under PREFIX in the
# blobdata volume (empty PREFIX: the whole volume; a missing PREFIX counts
# 0). find runs in a throwaway Alpine container without network, the volume
# mounted read-only.
e2e_fs_blob_count() {
	podman volume exists "${PROJECT}_blobdata" || {
		echo "no volume ${PROJECT}_blobdata" >"$WORK/find.err"
		return 1
	}
	podman run --rm --pull=never --security-opt label=disable --network none \
		-v "${PROJECT}_blobdata:/blobs:ro" -e "PREFIX=$1" "$ALPINE_IMAGE" /bin/sh -c '
			[ -d "/blobs/$PREFIX" ] || { echo 0; exit 0; }
			find "/blobs/$PREFIX" -type f >/tmp/ls || exit 1
			wc -l </tmp/ls' 2>"$WORK/find.err"
}

# e2e_check_blobs_empty JOB_ID fails if any blob is left under jobs/JOB_ID/
# or anywhere else in the blob store: the bucket (S3), or the blobdata volume
# with RECAP_BLOB_BACKEND=fs.
e2e_check_blobs_empty() {
	if [ "${RECAP_BLOB_BACKEND:-}" = fs ]; then
		n=$(e2e_fs_blob_count "jobs/$1/") || e2e_fail "blobs: find failed: $(cat "$WORK/find.err")"
		[ "$n" -eq 0 ] || e2e_fail "blobs: $n files left under jobs/$1/ in the volume"
		n=$(e2e_fs_blob_count "") || e2e_fail "blobs: find failed: $(cat "$WORK/find.err")"
		[ "$n" -eq 0 ] || e2e_fail "blobs: $n files left in the volume"
		e2e_ok "blob prefix jobs/$1/ is empty (fs volume ${PROJECT}_blobdata has no files)"
		return 0
	fi
	n=$(e2e_blob_count "jobs/$1/") || e2e_fail "blobs: mc ls failed: $(cat "$WORK/mc.err")"
	[ "$n" -eq 0 ] || e2e_fail "blobs: $n objects left under jobs/$1/"
	n=$(e2e_blob_count "") || e2e_fail "blobs: mc ls failed: $(cat "$WORK/mc.err")"
	[ "$n" -eq 0 ] || e2e_fail "blobs: $n objects left in the bucket"
	e2e_ok "blob prefix jobs/$1/ is empty (bucket empty)"
}

# e2e_psql SQL runs SQL in the postgres container and prints the result
# (unaligned, tuples only).
e2e_psql() {
	podman exec "$(e2e_container postgres)" psql -X -q -At -U recap -d recap -c "$1"
}

# e2e_result_ttl prints RECAP_RESULT_TTL as the worker container has it, so
# the value compose resolved from the shell or dev.env, not only what the
# caller's shell exports. Empty in the container means the binary's default,
# 7d (deployment.md §5.2); it fails if the variable is missing altogether.
e2e_result_ttl() {
	env=$(podman inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(e2e_container worker)") || return 1
	printf '%s\n' "$env" | grep -q '^RECAP_RESULT_TTL=' || return 0
	ttl=$(printf '%s\n' "$env" | sed -n 's/^RECAP_RESULT_TTL=//p')
	echo "${ttl:-7d}"
}

# e2e_check_result_record RECORD_ID checks the registration after cleanup
# (metadata-interface.md §2.7, privacy.md §5.1): with RECAP_RESULT_TTL above
# 0 (default 7d) exactly one row stays as the result record, with owner_sub,
# owner_email and expires_at NULL and result_expires_at set; with
# RECAP_RESULT_TTL=0 (also 0s, 0d, ...) no row is left. The TTL is the
# worker's (e2e_result_ttl), which runs cleanup.
e2e_check_result_record() {
	ttl=$(e2e_result_ttl) || e2e_fail "podman inspect: cannot read the environment of $(e2e_container worker)"
	[ -n "$ttl" ] || e2e_fail "worker: RECAP_RESULT_TTL is not set in the container"
	if [ -z "$(printf %s "$ttl" | tr -d '0.dhmsun')" ]; then
		n=$(e2e_psql "SELECT count(*) FROM registrations WHERE record_id = '$1'")
		[ "$n" -eq 0 ] || e2e_fail "postgres: $n registration rows left for the record (RECAP_RESULT_TTL=$ttl)"
		e2e_ok "postgres: registration deleted (RECAP_RESULT_TTL=$ttl)"
		return
	fi
	n=$(e2e_psql "SELECT count(*) FROM registrations WHERE record_id = '$1'")
	[ "$n" -eq 1 ] || e2e_fail "postgres: $n registration rows for the record, want 1 result record"
	n=$(e2e_psql "SELECT count(*) FROM registrations WHERE record_id = '$1'
		AND owner_sub IS NULL AND owner_email IS NULL AND expires_at IS NULL AND result_expires_at IS NOT NULL")
	[ "$n" -eq 1 ] || {
		e2e_info "registration: $(e2e_psql "SELECT owner_sub IS NULL, owner_email IS NULL, expires_at IS NULL,
			result_expires_at IS NOT NULL FROM registrations WHERE record_id = '$1'")"
		e2e_fail "postgres: the registration is not a result record (owner NULL, result_expires_at set)"
	}
	e2e_ok "postgres: registration kept as result record (owner NULL, result_expires_at set, RECAP_RESULT_TTL=$ttl)"
}

# e2e_db_canaries prints how many lines of a data-only dump of the database
# (WORK/db.sql) contain a DB canary.
e2e_db_canaries() {
	podman exec "$(e2e_container postgres)" pg_dump -U recap -d recap --data-only \
		>"$WORK/db.sql" 2>"$WORK/pg_dump.err" || return 1
	grep -ciE "$DB_CANARIES" "$WORK/db.sql" || true
}

# e2e_scan_logs SERVICE... writes the logs of SERVICE... to LOG_FILE (default
# WORK/logs) and scans them with e2e_scan_file.
e2e_scan_logs() {
	log=${LOG_FILE:-$WORK/logs}
	: >"$log"
	for s in "$@"; do
		podman logs "$(e2e_container "$s")" >>"$log" 2>&1 || e2e_fail "log scan: cannot read logs of $s"
	done
	e2e_scan_file "$log" "$*"
}

# e2e_scan_file FILE WHAT checks that every line of FILE (the log of WHAT) is
# a JSON object with time, level and msg and that no line holds a canary.
e2e_scan_file() {
	lines=$(wc -l <"$1")
	bad=$(jq -R -r 'try (fromjson | if type == "object" and has("time") and has("level") and has("msg")
		then empty else "bad" end) catch "bad"' "$1" | wc -l)
	[ "$bad" -eq 0 ] || e2e_fail "log scan: $bad of $lines lines of $2 are not JSON with time, level and msg"
	hits=$(grep -ciE "$CANARIES" "$1" || true)
	[ "$hits" -eq 0 ] || e2e_fail "log scan: $hits lines of $2 contain fixture content, owner data or secrets"
	e2e_ok "log scan passed ($lines JSON lines of $2, no fixture content, owner data or secrets)"
}

# e2e_scan_canaries SERVICE checks only that no log line of SERVICE holds a
# canary, for logs not in our JSON format (Caddy writes "ts", not "time").
e2e_scan_canaries() {
	podman logs "$(e2e_container "$1")" >"$WORK/logs-$1" 2>&1 || e2e_fail "log scan: cannot read logs of $1"
	hits=$(grep -ciE "$CANARIES" "$WORK/logs-$1" || true)
	[ "$hits" -eq 0 ] || e2e_fail "log scan: $hits lines of $1 contain fixture content, owner data or secrets"
	e2e_ok "log scan passed ($(wc -l <"$WORK/logs-$1") lines of $1, canaries only: none found)"
}

# e2e_elapsed prints the seconds since e2e_init.
e2e_elapsed() {
	echo $(($(date +%s) - E2E_START))
}
