#!/bin/sh
# E2E M2: a copy of the F-01 raw dir stands in for BBB's
# recording/raw. The real post_archive hook writes the spool entry, the real
# recap-exporter daemon pushes the package over HTTPS (the dev TLS front) to
# the dev compose stack (all mocks), the job is delivered and cleaned, and the
# exporter goes acked → deleted under RECAP_EXPORTER_DELETE_POLICY=after_ack
# (docs/reference/bbb-exporter.md §3–§7).
#
# The script plays BBB (runs the hook, writes the published marker), the
# front end (registration) and the admin (GET /api/v1/admin/jobs/{job_id}).
#
#   tools/e2e/m2.sh [-p PROJECT] [-P HOSTPORT] [-S HTTPSPORT]
#                   defaults recap-e2e-m2, 18028, 18428
#
# Environment: TIMEOUT (seconds per pipeline wait, default 180), KEEP=1
# (leave the stack and the scratch dir), LOG_FILE (keep the scanned service
# logs there). Exit 0 on success, 1 on a failed check, 2 on usage or missing
# tools or images. Needs `make build` and `make images` first; the script
# never pulls.
# The hook runs with a local ruby, else in docker.io/library/ruby:3.2-slim-bookworm
# (present locally, like `make hook-test`). The exporter is bin/recap-exporter
# on the host; it trusts the dev CA through SSL_CERT_FILE only.
set -eu

PROJECT=recap-e2e-m2
HOSTPORT=18028
HTTPSPORT=18428
usage() {
	echo "usage: $0 [-p PROJECT] [-P HOSTPORT] [-S HTTPSPORT]" >&2
	exit 2
}
while getopts p:P:S: opt; do
	case $opt in
	p) PROJECT=$OPTARG ;;
	P) HOSTPORT=$OPTARG ;;
	S) HTTPSPORT=$OPTARG ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || usage

# shellcheck source=tools/e2e/lib.sh
. "$(dirname "$0")/lib.sh"

case $PROJECT in '' | *[!a-z0-9-]*) e2e_usage_error "PROJECT must use only a-z, 0-9 and -" ;; esac
case $HOSTPORT in '' | *[!0-9]*) e2e_usage_error "HOSTPORT must be a number" ;; esac
case $HTTPSPORT in '' | *[!0-9]*) e2e_usage_error "HTTPSPORT must be a number" ;; esac
e2e_require_tools podman curl jq sha256sum
EXPORTER=$ROOT/bin/recap-exporter
[ -x "$EXPORTER" ] || e2e_usage_error "missing $EXPORTER: run make build"
RUBY_IMAGE=docker.io/library/ruby:3.2-slim-bookworm
HOOK=$ROOT/hook/post_archive/recap_export.rb

F01=$ROOT/testdata/meetings/F-01
RECORD_ID=$(jq -r .record_id "$F01/expected/manifest.json")
# RECAP_LOG_LEVEL is read by compose for api, worker, migrate and mock.
export RECAP_LOG_LEVEL=debug
# S3 on the dev MinIO, as dev.env sets it; explicit so that a caller's
# RECAP_BLOB_BACKEND cannot switch this scenario to fs.
export RECAP_BLOB_BACKEND=s3
# The exporter's metrics and health listener: the first port after HTTPSPORT
# that nothing answers on.
port_free() {
	rc=0
	curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$1/" || rc=$?
	[ "$rc" -eq 7 ] # connection refused
}
METRICS_PORT=$((HTTPSPORT + 1))
until port_free "$METRICS_PORT"; do
	METRICS_PORT=$((METRICS_PORT + 1))
	[ "$METRICS_PORT" -le $((HTTPSPORT + 20)) ] ||
		e2e_usage_error "no free port for the exporter metrics in $((HTTPSPORT + 1))-$((HTTPSPORT + 20))"
done
METRICS=http://127.0.0.1:$METRICS_PORT

trap e2e_cleanup EXIT
trap 'exit 1' INT TERM
e2e_init

# hook runs the post_archive hook as BBB does (`ruby recap_export.rb -m ID`)
# and appends its output to WORK/hook.log. The container has no network and
# sees only the hook, the env file and raw/ read-only and spool/ writable,
# each at its host path, so the one env file serves hook and daemon.
hook() {
	if command -v ruby >/dev/null 2>&1; then
		RECAP_EXPORTER_ENV_FILE=$WORK/exporter.env ruby "$HOOK" -m "$RECORD_ID" >>"$WORK/hook.log" 2>&1
	else
		podman run --rm --pull=never --security-opt label=disable --network none \
			-v "$HOOK:/hook/recap_export.rb:ro" -v "$WORK/exporter.env:$WORK/exporter.env:ro" \
			-v "$WORK/raw:$WORK/raw:ro" -v "$WORK/spool:$WORK/spool" \
			-e "RECAP_EXPORTER_ENV_FILE=$WORK/exporter.env" \
			"$RUBY_IMAGE" ruby /hook/recap_export.rb -m "$RECORD_ID" >>"$WORK/hook.log" 2>&1
	fi
}

# exporter ARGS... runs bin/recap-exporter with only the env file and the
# dev CA, so no RECAP_EXPORTER_* of the caller leaks in.
exporter() {
	env -i "SSL_CERT_FILE=$WORK/ca.pem" "RECAP_EXPORTER_ENV_FILE=$WORK/exporter.env" "$EXPORTER" "$@"
}

# start_daemon starts `recap-exporter run` in the background (output to
# WORK/exporter.log) and waits until /readyz answers 200. A plain command,
# not the exporter function, so that $! is the daemon (env execs it) and
# SIGTERM reaches it.
start_daemon() {
	env -i "SSL_CERT_FILE=$WORK/ca.pem" "RECAP_EXPORTER_ENV_FILE=$WORK/exporter.env" "$EXPORTER" run \
		>>"$WORK/exporter.log" 2>&1 &
	DAEMON_PID=$!
	E2E_PIDS=$DAEMON_PID
	e2e_wait_for "exporter /readyz not 200" 30 daemon_ready
}

daemon_ready() {
	kill -0 "$DAEMON_PID" 2>/dev/null || {
		tail -n 5 "$WORK/exporter.log"
		e2e_fail "exporter exited at start"
	}
	[ "$(curl -s -o /dev/null -w '%{http_code}' "$METRICS/readyz")" = 200 ]
}

# stop_daemon sends SIGTERM and fails unless the daemon exits 0 within 20 s
# (bbb-exporter.md §4.3).
stop_daemon() {
	kill -TERM "$DAEMON_PID"
	waited=0
	while kill -0 "$DAEMON_PID" 2>/dev/null; do
		[ "$waited" -lt 20 ] || e2e_fail "exporter still running 20 s after SIGTERM"
		sleep 1
		waited=$((waited + 1))
	done
	rc=0
	wait "$DAEMON_PID" || rc=$?
	E2E_PIDS=
	[ "$rc" -eq 0 ] || e2e_fail "exporter exited $rc after SIGTERM, want 0"
	e2e_ok "SIGTERM: exporter exited 0 after ${waited}s"
}

# state prints the exporter state of the record ("none" without state.json).
state() {
	jq -r '.state' "$STATE_JSON" 2>/dev/null || echo none
}

# wait_state WANT SECONDS waits for the exporter state WANT; it fails early
# on failed and prints the state record.
wait_state() {
	waited=0
	until [ "$(state)" = "$1" ]; do
		if [ "$(state)" = failed ] || [ "$waited" -ge "$2" ]; then
			e2e_info "state.json: $(cat "$STATE_JSON" 2>/dev/null)"
			e2e_fail "exporter state is $(state), want $1 (waited ${waited}s)"
		fi
		sleep 2
		waited=$((waited + 2))
	done
}

# raw_others lists everything under raw/ except raw/RECORD_ID, with hashes.
raw_others() {
	(cd "$WORK/raw" && find . -path "./$RECORD_ID" -prune -o -type f -exec sha256sum {} + -o -print | sort)
}

# 1. Scratch layout: raw/<record_id> is a copy of the
# fixture, plus an unrelated recording the exporter must not touch.
mkdir -p "$WORK/raw" "$WORK/spool" "$WORK/state" "$WORK/status/published"
cp -a "$F01/raw/$RECORD_ID" "$WORK/raw/"
mkdir -p "$WORK/raw/other-0000000000-1"
echo '<recording/>' >"$WORK/raw/other-0000000000-1/events.xml"
raw_others >"$WORK/raw-others.before"
cp "$CLIENTS_DIR/bbb_server.bbb-fixture-a" "$WORK/token"
chmod 0400 "$WORK/token"
STATE_JSON=$WORK/state/$RECORD_ID/state.json
cat >"$WORK/exporter.env" <<EOF
RECAP_EXPORTER_SERVICE_URL=https://127.0.0.1:$HTTPSPORT
RECAP_EXPORTER_BBB_SERVER_ID=bbb-fixture-a
RECAP_EXPORTER_TOKEN_FILE=$WORK/token
RECAP_EXPORTER_META_PREFIX=recap
RECAP_EXPORTER_RAW_DIR=$WORK/raw
RECAP_EXPORTER_SPOOL_DIR=$WORK/spool
RECAP_EXPORTER_STATE_DIR=$WORK/state
RECAP_EXPORTER_DELETE_POLICY=after_ack
RECAP_EXPORTER_RECORDING_STATUS_DIR=$WORK/status
RECAP_EXPORTER_RECORDING_FORMATS=presentation
RECAP_EXPORTER_LISTEN_ADDR=127.0.0.1:$METRICS_PORT
RECAP_EXPORTER_LOG_LEVEL=info
EOF
e2e_ok "scratch dir: raw/$RECORD_ID copied from F-01, exporter.env, token 0400"

# 2. A fresh stack with the TLS front, and its root CA for SSL_CERT_FILE.
# MinIO's mc counts the bucket (e2e_blob_count); ruby runs the hook unless
# there is a local ruby.
E2E_IMAGES=$MINIO_IMAGE
command -v ruby >/dev/null 2>&1 || E2E_IMAGES="$E2E_IMAGES $RUBY_IMAGE"
e2e_up postgres minio migrate minio-init mock api worker tls
e2e_wait_for "tls not healthy" "$TIMEOUT" e2e_healthy tls
"$DEV_DIR/dev-ca.sh" "$WORK/ca.pem" "$PROJECT" >/dev/null || e2e_fail "dev-ca.sh could not copy the CA"
code=$(curl -sS -o /dev/null -w '%{http_code}' --cacert "$WORK/ca.pem" "https://127.0.0.1:$HTTPSPORT/" \
	2>"$WORK/curl.err") || e2e_fail "TLS front: $(cat "$WORK/curl.err")"
e2e_ok "TLS front https://127.0.0.1:$HTTPSPORT verified with the dev CA (HTTP $code)"
exporter check-config >"$WORK/check-config.out" 2>&1 || {
	cat "$WORK/check-config.out"
	e2e_fail "recap-exporter check-config"
}
e2e_ok "recap-exporter check-config: $(cat "$WORK/check-config.out")"

# 3. The front end registers when the meeting is created.
e2e_auth frontend frontend.pilos-fixture
e2e_auth admin admin.fixture-admin
e2e_http "registration PUT" frontend PUT "/api/v1/registrations/$RECORD_ID" "$F01/registration.json"
e2e_expect_status 201 "$HTTP_CODE" "registration PUT"
e2e_ok "registration PUT 201"

# 4. The hook writes one spool entry; a second run (bbb-record --rebuild)
# leaves it unchanged (§3.4).
hook || e2e_fail "hook exited non-zero: $(tail -n 1 "$WORK/hook.log")"
ENTRY=$WORK/spool/$RECORD_ID.json
[ -f "$ENTRY" ] || e2e_fail "hook: no spool entry $ENTRY ($(tail -n 1 "$WORK/hook.log"))"
jq -e --arg r "$RECORD_ID" '.spool_version == 1 and .record_id == $r and (keys | sort) ==
	["hook_version", "hooked_at", "record_id", "spool_version"]' "$ENTRY" >/dev/null ||
	e2e_fail "hook: unexpected spool entry $(cat "$ENTRY")"
[ "$(find "$WORK/spool" -type f | wc -l)" -eq 1 ] || e2e_fail "hook: more than one file in the spool"
e2e_ok "hook: spool entry $RECORD_ID.json (decision $(tail -n 1 "$WORK/hook.log" | jq -r .decision))"
before=$(sha256sum "$ENTRY")
hook || e2e_fail "second hook run exited non-zero: $(tail -n 1 "$WORK/hook.log")"
[ "$(sha256sum "$ENTRY")" = "$before" ] || e2e_fail "second hook run changed the spool entry"
decision=$(tail -n 1 "$WORK/hook.log" | jq -r .decision)
[ "$decision" = spool_exists ] || e2e_fail "second hook run: decision $decision, want spool_exists"
e2e_ok "hook again: entry unchanged (decision spool_exists)"

# 5. The daemon claims, builds, pushes over HTTPS, commits and verifies the ack.
start_daemon
e2e_ok "exporter daemon running (pid $DAEMON_PID, metrics on $METRICS)"
wait_state acked "$TIMEOUT"
JOB_ID=$(jq -r .job_id "$STATE_JSON")
echo "$JOB_ID" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' ||
	e2e_fail "acked without a UUIDv7 job_id: $JOB_ID"
[ ! -e "$ENTRY" ] || e2e_fail "spool entry still there after the claim"
e2e_ok "exporter state acked, package $(jq -r .package_id "$STATE_JSON"), job_id $JOB_ID"
ACKED_SEEN=$(date +%s)

# 6. The service runs the job to the mock Docs and cleans it.
e2e_wait_job "$JOB_ID" cleaned/delivered
jq -e '.delivery.state == "sent"' "$WORK/job" >/dev/null || {
	e2e_info "job: $(cat "$WORK/job")"
	e2e_fail "job cleaned/delivered but delivery.state is not sent"
}
DOC_ID=$(jq -r '.delivery.document_id // empty' "$WORK/job")
e2e_ok "job $JOB_ID cleaned/delivered, delivery.state sent, document_id $DOC_ID"
e2e_check_docs_once "$DOC_ID"
e2e_check_blobs_empty "$JOB_ID"
e2e_check_result_record "$RECORD_ID"
n=$(e2e_db_canaries) || e2e_fail "postgres: pg_dump failed"
[ "$n" -eq 0 ] || {
	grep -iE "$DB_CANARIES" "$WORK/db.sql" | cut -c1-200 | sed 's/^/     db: /'
	e2e_fail "postgres: $n dump lines contain owner data, content or BBB user IDs"
}
e2e_ok "postgres: no owner data, content or BBB user IDs in the dump"

# 7. No BBB marker yet and the grace has not passed: raw data stays. Wait
# until at least one daemon loop (10 s) has run since acked, so the check
# would see a premature deletion.
left=$((ACKED_SEEN + 12 - $(date +%s)))
[ "$left" -le 0 ] || sleep "$left"
[ "$(state)" = acked ] || e2e_fail "exporter state is $(state) before the BBB marker, want acked"
[ -f "$WORK/raw/$RECORD_ID/events.xml" ] || e2e_fail "raw/$RECORD_ID deleted before the BBB marker and grace"
e2e_ok "raw/$RECORD_ID still present (no published marker, grace not over)"

# 8. BBB publishes the presentation format; the daemon stops on SIGTERM.
: >"$WORK/status/published/$RECORD_ID-presentation.done"
e2e_ok "BBB marker status/published/$RECORD_ID-presentation.done written"
stop_daemon

# 9. 25 h pass: back-date the state record like the contract tests do.
jq -e '[.first_attempt_at, .last_attempt_at, .next_attempt_at, .history[].at] | map(select(. != null)) |
	all(test("^[0-9-]+T[0-9:]+(\\.[0-9]+)?Z$"))' "$STATE_JSON" >/dev/null ||
	e2e_fail "state.json timestamps are not RFC 3339 UTC: $(cat "$STATE_JSON")"
jq '
	def back: if type == "string" then sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 - 25 * 3600 | todate else . end;
	(.first_attempt_at, .last_attempt_at, .next_attempt_at) |= back | .history[].at |= back
' "$STATE_JSON" >"$STATE_JSON.backdate"
mv "$STATE_JSON.backdate" "$STATE_JSON"
e2e_ok "state record back-dated by 25 h (acked at $(jq -r '.history[] | select(.state == "acked") | .at' "$STATE_JSON"))"

# 10. Restart: the daemon deletes raw/<record_id> and nothing else.
start_daemon
wait_state deleted 60
[ ! -e "$WORK/raw/$RECORD_ID" ] || e2e_fail "state deleted but raw/$RECORD_ID still exists"
raw_others >"$WORK/raw-others.after"
cmp -s "$WORK/raw-others.before" "$WORK/raw-others.after" || {
	diff "$WORK/raw-others.before" "$WORK/raw-others.after" | sed 's/^/     /'
	e2e_fail "something else under raw/ changed"
}
e2e_ok "exporter state deleted, raw/$RECORD_ID gone, rest of raw/ untouched"
curl -fsS "$METRICS/metrics" >"$WORK/metrics" 2>"$WORK/curl.err" || e2e_fail "metrics: $(cat "$WORK/curl.err")"
grep -qx 'recap_exporter_deleted_total{reason="after_ack"} 1' "$WORK/metrics" || {
	grep '^recap_exporter_deleted_total' "$WORK/metrics" | sed 's/^/     /'
	e2e_fail 'metrics: recap_exporter_deleted_total{reason="after_ack"} is not 1'
}
e2e_ok 'metrics: recap_exporter_deleted_total{reason="after_ack"} 1'
exporter status >"$WORK/status.json" 2>"$WORK/status.err" || e2e_fail "recap-exporter status: $(cat "$WORK/status.err")"
jq -e --arg r "$RECORD_ID" '.records == [{"record_id": $r, "state": "deleted", "reason": "after_ack"}]' \
	"$WORK/status.json" >/dev/null || e2e_fail "recap-exporter status: $(cat "$WORK/status.json")"
e2e_ok "recap-exporter status: $RECORD_ID deleted (reason after_ack)"
stop_daemon

# 11. Logs: services, exporter and hook are JSON without fixture content;
# the Caddy front (its own JSON keys) has no fixture content.
e2e_scan_logs api worker mock
e2e_scan_file "$WORK/exporter.log" exporter
e2e_scan_file "$WORK/hook.log" hook
e2e_scan_canaries tls

e2e_ok "M2 passed in $(e2e_elapsed)s"
