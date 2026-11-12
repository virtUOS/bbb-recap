#!/bin/sh
# Creates tools/dev/secrets/ (gitignored) for the dev compose stack
# (docs/reference/fixtures.md §5). Fixture values only, never real secrets:
#   clients/*, sinks/docs-s2s.key, providers/{asr,llm}-fixture.key
#                      copied from testdata/secrets/
#   db-password, s3-access-key-id, s3-secret-access-key,
#   oidc-client-secret (admin UI client at the mock OIDC provider),
#   session-key (admin UI session cookies, >= 32 bytes)
#                      generated: FIXTURE-NOT-A-SECRET-<name>-<32 hex>
# Files are mode 0644 so uid 10001 in the containers can read them.
#
#   dev-secrets.sh [-f]   -f replaces an existing secrets/
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
SRC=$HERE/../../testdata/secrets
DIR=$HERE/secrets

die() {
	echo "dev-secrets: $*" >&2
	exit 1
}

force=no
case ${1:-} in
'') ;;
-f) force=yes ;;
*)
	echo "usage: $0 [-f]" >&2
	exit 2
	;;
esac
[ $# -le 1 ] || {
	echo "usage: $0 [-f]" >&2
	exit 2
}

if [ -e "$DIR" ]; then
	[ "$force" = yes ] || die "$DIR exists: use -f to replace it"
	rm -rf "$DIR"
fi
[ -d "$SRC/clients" ] || die "$SRC/clients missing"

# fixture NAME prints a generated fixture value (no trailing newline).
fixture() {
	hex=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
	printf 'FIXTURE-NOT-A-SECRET-%s-%s' "$1" "$hex"
}

umask 022
mkdir -p "$DIR/clients" "$DIR/sinks" "$DIR/providers"
cp "$SRC"/clients/* "$DIR/clients/"
cp "$SRC/docs-s2s.key" "$DIR/sinks/docs-s2s.key"
cp "$SRC/asr-fixture.key" "$SRC/llm-fixture.key" "$DIR/providers/"
fixture db >"$DIR/db-password"
fixture s3-access >"$DIR/s3-access-key-id"
fixture s3-secret >"$DIR/s3-secret-access-key"
fixture oidc-client >"$DIR/oidc-client-secret"
fixture session >"$DIR/session-key"
find "$DIR" -type d -exec chmod 0755 {} +
find "$DIR" -type f -exec chmod 0644 {} +
echo "dev-secrets: created $DIR"
