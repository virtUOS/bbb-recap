#!/usr/bin/env bash
# Tests deploy/setup.sh without starting the service: a non-interactive run
# with dummy values in a temp dir must pass check-config with a local image,
# leave the modes of install-compose.md, print no provider or sink key, and a
# second run must change no file. A missing required value must fail.
#
# Usage: tools/e2e/setup-test.sh   (after make images)
#   IMAGE    the recap image (default localhost/recap:dev)
#   COMPOSE  the compose command (default podman compose)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE=${IMAGE:-localhost/recap:dev}
COMPOSE=${COMPOSE:-podman compose}
PROJECT=recap-setup-test
WORK=$(mktemp -d)
read -r -a compose <<<"$COMPOSE"
# Only the answers given below count.
unset "${!RECAP_SETUP_@}"

cleanup() {
	for d in "$WORK"/*/; do
		[[ -f $d/compose.yaml && -f $d/.env ]] || continue
		(cd "$d" && "${compose[@]}" -p "$PROJECT" down -v >/dev/null 2>&1) || true
	done
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
	echo "FAIL $*"
	exit 1
}
ok() {
	echo "ok   $*"
}

# Dummy answers for every question (no real host, provider or key).
dummy_env() {
	env RECAP_SETUP_COMPOSE="$COMPOSE -p $PROJECT" \
		RECAP_SETUP_IMAGE="$IMAGE" \
		RECAP_SETUP_HOST=recap.example.org \
		RECAP_SETUP_ASR_URL=https://asr.example.org \
		RECAP_SETUP_ASR_KEY=dummy-asr-key-not-a-secret \
		RECAP_SETUP_LLM_URL=https://llm.example.org/v1 \
		RECAP_SETUP_LLM_MODEL=example-model \
		RECAP_SETUP_LLM_KEY=none \
		RECAP_SETUP_OPERATOR="Example operator" \
		RECAP_SETUP_DOCS_URL=https://docs.example.org \
		RECAP_SETUP_DOCS_KEY=dummy-docs-key-not-a-secret \
		RECAP_SETUP_BBB_SERVERS=bbb-1,bbb-2 \
		RECAP_SETUP_FRONTENDS=frontend-1 \
		RECAP_SETUP_OIDC_ISSUER=https://idp.example.org/realms/example \
		RECAP_SETUP_OIDC_CLIENT_SECRET=dummy-oidc-secret-not-a-secret \
		"$@"
}

# state DIR: path, mode, mtime and SHA-256 of everything below DIR.
state() {
	(cd "$1" && find . -printf '%p %m %T@\n' | sort && find . -type f -exec sha256sum {} + | sort -k2)
}

podman image exists "$IMAGE" 2>/dev/null || docker image inspect "$IMAGE" >/dev/null 2>&1 ||
	fail "image $IMAGE not found; run make images first"

# 1. A missing required value fails before anything is written.
dir=$WORK/missing
if dummy_env RECAP_SETUP_LLM_MODEL= "$ROOT/deploy/setup.sh" --dir "$dir" --non-interactive --no-start \
	>"$WORK/missing.out" 2>&1; then
	fail "missing RECAP_SETUP_LLM_MODEL: exit 0"
fi
grep -q 'missing value: .*RECAP_SETUP_LLM_MODEL' "$WORK/missing.out" ||
	fail "missing value: no clear message: $(cat "$WORK/missing.out")"
[[ -z $(ls -A "$dir") ]] || fail "missing value: files written"
ok "missing required value: exit non-zero, names RECAP_SETUP_LLM_MODEL, writes nothing"

# 2. First run: the files pass check-config.
dir=$WORK/docs
dummy_env "$ROOT/deploy/setup.sh" --dir "$dir" --non-interactive --no-start >"$WORK/run1.out" 2>&1 ||
	fail "first run: $(cat "$WORK/run1.out")"
grep -q 'configuration is valid for \[api worker\]' "$WORK/run1.out" || fail "first run: check-config did not pass"
ok "first run: check-config passes"
if grep -q -e dummy-asr-key -e dummy-docs-key -e dummy-oidc-secret "$WORK/run1.out"; then
	fail "first run: a key was printed"
fi
grep -q "$(cat "$dir/secrets/clients/bbb_server.bbb-1")" "$WORK/run1.out" || fail "first run: new token not shown"
if grep -q "$(cat "$dir/secrets/db-password")" "$WORK/run1.out"; then
	fail "first run: db-password was printed"
fi
ok "first run: shows the new client tokens, no other secret"
if grep -v '^ *#' "$dir"/config/*.yaml | grep -q REPLACE; then
	fail "first run: REPLACE left in config"
fi
ok "first run: no REPLACE value left"

# 3. Modes as in install-compose.md: install dir 0700, .env 0600, everything
# the containers read world-readable, nothing group- or world-writable.
check_mode() {
	local got
	got=$(stat -c %a "$dir/$1")
	[[ $got == "$2" ]] || fail "mode of $1: want $2, got $got"
}
check_mode . 700
check_mode .env 600
for f in compose.yaml config/providers.yaml config/sinks.yaml config/meeting-types.yaml secrets/db-password \
	secrets/session-key secrets/oidc-client-secret secrets/clients/bbb_server.bbb-1 secrets/clients/frontend.frontend-1 \
	secrets/clients/admin.ops secrets/providers/asr.key secrets/sinks/docs.key; do
	check_mode "$f" 644
done
for d in config secrets secrets/clients secrets/providers secrets/sinks; do
	check_mode "$d" 755
done
[[ ! -e $dir/secrets/providers/llm.key ]] || fail "llm.key written for key none"
[[ -z $(find "$dir" -perm /022) ]] || fail "group- or world-writable files: $(find "$dir" -perm /022)"
ok "modes: dir 0700, .env 0600, config and secrets 0644 in 0755 dirs"

# 4. Second run, without any answers: nothing changes.
before=$(state "$dir")
env RECAP_SETUP_COMPOSE="$COMPOSE -p $PROJECT" \
	"$ROOT/deploy/setup.sh" --dir "$dir" --non-interactive --no-start >"$WORK/run2.out" 2>&1 ||
	fail "second run: $(cat "$WORK/run2.out")"
[[ $(state "$dir") == "$before" ]] || fail "second run changed files: $(diff <(echo "$before") <(state "$dir"))"
grep -q 'Nothing to change' "$WORK/run2.out" || fail "second run: no 'Nothing to change'"
if grep -q "$(cat "$dir/secrets/clients/bbb_server.bbb-1")" "$WORK/run2.out"; then
	fail "second run: token shown again"
fi
ok "second run: no file changed (paths, modes, mtimes, checksums), no token shown"

# 5. --force rewrites config (with .bak) but keeps every secret.
secrets_before=$(state "$dir/secrets")
dummy_env RECAP_SETUP_LLM_MODEL=other-model "$ROOT/deploy/setup.sh" --dir "$dir" --non-interactive --no-start --force \
	>"$WORK/run3.out" 2>&1 || fail "--force: $(cat "$WORK/run3.out")"
grep -q 'model: other-model' "$dir/config/providers.yaml" || fail "--force: providers.yaml not rewritten"
grep -q 'model: example-model' "$dir/config/providers.yaml.bak" || fail "--force: no providers.yaml.bak"
[[ $(state "$dir/secrets") == "$secrets_before" ]] || fail "--force changed secrets"
ok "--force: config rewritten with .bak, secrets unchanged"

# 6. Front end only: no docs profile, check-config passes.
dir=$WORK/frontend
dummy_env RECAP_SETUP_SINK=frontend RECAP_SETUP_DOCS_URL= RECAP_SETUP_DOCS_KEY= RECAP_SETUP_OIDC_ISSUER= \
	"$ROOT/deploy/setup.sh" --dir "$dir" --non-interactive --no-start >"$WORK/run4.out" 2>&1 ||
	fail "front end only: $(cat "$WORK/run4.out")"
grep -q 'configuration is valid' "$WORK/run4.out" || fail "front end only: check-config did not pass"
if grep -q 'type: docs' "$dir/config/sinks.yaml"; then
	fail "front end only: docs profile left"
fi
[[ ! -e $dir/secrets/sinks/docs.key && ! -e $dir/secrets/session-key ]] ||
	fail "front end only: docs key or session key written"
ok "front end only: no docs profile, check-config passes"
