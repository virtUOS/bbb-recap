#!/bin/sh
# E2E M1: the F-01 fixture package and its late registration
# go through the dev compose stack (all mocks) to held/no_registration,
# cleaned/delivered, one document at the mock Docs, and nothing personal left.
#
# The script plays the BBB server (ingest protocol, docs/reference/bbb-exporter.md
# §6.2, §6.3), the front end (registration, metadata-interface.md §2) and the
# admin (GET /api/v1/admin/jobs/{job_id}) with curl on 127.0.0.1.
#
#   tools/e2e/m1.sh [-p PROJECT] [-P HOSTPORT]   defaults recap-e2e-m1, 18025
#
# Environment: TIMEOUT (seconds per wait, default 180), KEEP=1 (leave the
# stack and the scratch dir), LOG_FILE (keep the scanned service logs there), STOP_WORKER=1
# (negative control: stop the worker after the commit; the run must fail).
# Exit 0 on success, 1 on a failed check, 2 on usage or missing tools or
# images. Needs `make images` first; the script never pulls. Expects
# podman-compose container names (PROJECT_<service>_1, see tools/e2e/lib.sh).
set -eu

PROJECT=recap-e2e-m1
HOSTPORT=18025
while getopts p:P: opt; do
	case $opt in
	p) PROJECT=$OPTARG ;;
	P) HOSTPORT=$OPTARG ;;
	*) echo "usage: $0 [-p PROJECT] [-P HOSTPORT]" >&2 && exit 2 ;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || {
	echo "usage: $0 [-p PROJECT] [-P HOSTPORT]" >&2
	exit 2
}

# shellcheck source=tools/e2e/lib.sh
. "$(dirname "$0")/lib.sh"

case $PROJECT in '' | *[!a-z0-9-]*) e2e_usage_error "PROJECT must use only a-z, 0-9 and -" ;; esac
case $HOSTPORT in '' | *[!0-9]*) e2e_usage_error "HOSTPORT must be a number" ;; esac
e2e_require_tools podman curl jq sha256sum

F01=$ROOT/testdata/meetings/F-01
MANIFEST=$F01/expected/manifest.json
REGISTRATION=$F01/registration.json
# RECAP_LOG_LEVEL is read by compose for api, worker, migrate and mock.
export RECAP_LOG_LEVEL=debug
# The compose default: the fs backend on the volume blobdata, no MinIO. It
# overrides the S3 of dev.env (the shell wins over --env-file).
export RECAP_BLOB_BACKEND=fs

trap e2e_cleanup EXIT
trap 'exit 1' INT TERM
e2e_init

# 1. The fixture: canonical manifest bytes hash to the golden package ID.
PACKAGE_ID=$(cat "$F01/expected/package_id.txt")
got=$(sha256sum "$MANIFEST" | cut -d' ' -f1)
[ "$got" = "$PACKAGE_ID" ] || e2e_fail "package id: sha256 of manifest.json is $got, want $PACKAGE_ID"
e2e_ok "package id $PACKAGE_ID matches the manifest"
RECORD_ID=$(jq -r .record_id "$MANIFEST")
FILE_COUNT=$(jq -r .totals.file_count "$MANIFEST")
SIZE=$(jq -r .totals.size "$MANIFEST")
RAW=$F01/raw/$RECORD_ID

# 2. A fresh stack on the fs backend. tls is left out: this scenario talks
# plain HTTP to api. Alpine lists the blobdata volume (e2e_fs_blob_count).
E2E_IMAGES=$ALPINE_IMAGE
e2e_up postgres migrate mock api worker
e2e_auth bbb bbb_server.bbb-fixture-a
e2e_auth frontend frontend.pilos-fixture
e2e_auth admin admin.fixture-admin
INGEST=/api/v1/ingest/packages/$PACKAGE_ID

# 3. Manifest PUT: a new package, every distinct hash missing.
e2e_http "manifest PUT" bbb PUT "$INGEST" "$MANIFEST"
e2e_expect_status 201 "$HTTP_CODE" "manifest PUT"
jq -r '.missing[]' "$WORK/body" >"$WORK/missing"
e2e_ok "manifest PUT 201 (state $(jq -r .state "$WORK/body"), $(wc -l <"$WORK/missing") hashes missing)"

# 4. Upload each missing hash once, in the order of missing (§6.3).
uploads=0
while read -r hash; do
	path=$(jq -r --arg h "$hash" 'first(.files[] | select(.sha256 == $h) | .path)' "$MANIFEST")
	[ -n "$path" ] && [ -f "$RAW/$path" ] || e2e_fail "upload: no fixture file for hash $hash"
	e2e_http "upload of $hash" bbb PUT "$INGEST/files/$hash" "$RAW/$path" application/octet-stream
	case $HTTP_CODE in 204 | 200) ;; *) e2e_expect_status 204 "$HTTP_CODE" "upload of $hash" ;; esac
	uploads=$((uploads + 1))
done <"$WORK/missing"
e2e_ok "uploaded $uploads files ($FILE_COUNT paths in the manifest)"

# 5. Manifest PUT again: nothing missing.
e2e_http "second manifest PUT" bbb PUT "$INGEST" "$MANIFEST"
e2e_expect_status 200 "$HTTP_CODE" "second manifest PUT"
left=$(jq -r '.missing | length' "$WORK/body")
[ "$left" -eq 0 ] || e2e_fail "second manifest PUT: $left hashes still missing"
e2e_ok "second manifest PUT 200: missing is empty (state $(jq -r .state "$WORK/body"))"

# 6. Commit and verify the ack like the exporter (§6.3 step 2).
echo '{}' >"$WORK/empty.json"
e2e_http commit bbb POST "$INGEST/commit" "$WORK/empty.json"
e2e_expect_status 200 "$HTTP_CODE" "commit"
JOB_ID=$(jq -r .job_id "$WORK/body")
jq -e --arg p "$PACKAGE_ID" --arg r "$RECORD_ID" --argjson n "$FILE_COUNT" --argjson s "$SIZE" '
	.state == "committed" and .package_id == $p and .record_id == $r and .file_count == $n and .size == $s
	and (.job_id | test("^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
' "$WORK/body" >/dev/null || {
	e2e_info "ack: $(cat "$WORK/body")"
	e2e_fail "commit: ack does not match the manifest or job_id is not a UUIDv7"
}
e2e_ok "commit 200: ack verified (package_id, record_id, $FILE_COUNT files, $SIZE bytes), job_id $JOB_ID is a UUIDv7"

if [ "${STOP_WORKER:-0}" = 1 ]; then
	podman stop -t 30 "$(e2e_container worker)" >/dev/null
	e2e_info "STOP_WORKER=1: worker stopped (negative control, this run must fail)"
fi

# 7. No registration yet: validate holds the job.
e2e_wait_job "$JOB_ID" held/no_registration
e2e_ok "job held/no_registration"
# The package is in the fs store (the volume blobdata), so the empty check
# in step 11 is not vacuous.
n=$(e2e_fs_blob_count "jobs/$JOB_ID/") || e2e_fail "blobs: find failed: $(cat "$WORK/find.err")"
[ "$n" -gt 0 ] || e2e_fail "blobs: no files under jobs/$JOB_ID/ in the volume while the job is held"
e2e_ok "blobs: $n files under jobs/$JOB_ID/ in the volume ${PROJECT}_blobdata"

# 8. The front end registers late; the job resumes.
e2e_http "registration PUT" frontend PUT "/api/v1/registrations/$RECORD_ID" "$REGISTRATION"
e2e_expect_status 201 "$HTTP_CODE" "registration PUT"
e2e_ok "registration PUT 201"

# 9. Through the pipeline to cleaned/delivered with a sent delivery.
e2e_wait_job "$JOB_ID" cleaned/delivered
jq -e '.delivery.state == "sent" and (.delivery.document_id | type == "string" and length > 0)' \
	"$WORK/job" >/dev/null || {
	e2e_info "job: $(cat "$WORK/job")"
	e2e_fail "job cleaned/delivered but delivery is not sent with a document_id"
}
DOC_ID=$(jq -r .delivery.document_id "$WORK/job")
e2e_ok "job cleaned/delivered, delivery.state sent, document_id $DOC_ID"

# 10. The mock Docs answered exactly one create-for-owner: 201, for a known
# owner, with the job's document ID.
e2e_check_docs_once "$DOC_ID"

# 11. Nothing left in the blob volume: neither the job prefix nor anything else.
e2e_check_blobs_empty "$JOB_ID"

# 12. Nothing personal left in Postgres: the registration is only the result
# record (or gone with RECAP_RESULT_TTL=0), and no owner data, content or BBB
# user ID (DB_CANARIES) is anywhere in a data-only dump.
# cleanup deletes the package_files rows, whose paths hold BBB user IDs
# (privacy.md §5.2).
e2e_check_result_record "$RECORD_ID"
n=$(e2e_db_canaries) || e2e_fail "postgres: pg_dump failed"
[ "$n" -eq 0 ] || {
	grep -iE "$DB_CANARIES" "$WORK/db.sql" | cut -c1-200 | sed 's/^/     db: /'
	e2e_fail "postgres: $n dump lines contain owner data, content or BBB user IDs"
}
e2e_ok "postgres: no owner data, content or BBB user IDs in the dump"
n=$(e2e_psql "SELECT count(*) FROM package_files")
[ "$n" -eq 0 ] || e2e_fail "postgres: $n package_files rows left"
n=$(e2e_psql "SELECT file_count FROM packages WHERE package_id = '$PACKAGE_ID'")
e2e_ok "postgres: no package_files rows left; the package keeps file_count $n"

# 13. Logs of api, worker and mock: JSON, no fixture content, no secrets.
e2e_scan_logs api worker mock

e2e_ok "M1 passed in $(e2e_elapsed)s"
