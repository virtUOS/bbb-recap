#!/usr/bin/env bash
# Tests deploy/exporter/setup.sh in throw-away Ubuntu containers with a fake
# BBB layout (no docker inside, so --no-start): for packaged and docker BBB it
# must create the same user, directories, files, owners and modes as the
# manual steps 2 to 4 of docs/operations/install-exporter.md, and a second run
# must change nothing.
#
# Usage: tools/e2e/exporter-setup-test.sh
#   ENGINE  podman (default) or docker
#   BASE    the image (default docker.io/library/ubuntu:24.04, as BBB 4.0)
#   KEEP=1  keep the listings and logs in the temp dir
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
ENGINE=${ENGINE:-podman}
BASE=${BASE:-docker.io/library/ubuntu:24.04}
WORK=$(mktemp -d)
trap '[[ ${KEEP:-0} == 1 ]] && echo "kept $WORK" || rm -rf "$WORK"' EXIT

fail() {
	echo "FAIL $*"
	exit 1
}
ok() {
	echo "ok   $*"
}

# Runs inside the container: fake BBB, then the manual steps or the script,
# then a listing of everything the steps touch, to /out.
cat >"$WORK/inner.sh" <<'INNER'
set -euo pipefail
mode=$1 how=$2
VERSION=v0.0.0
SERVICE_URL=https://recap.example.org
BBB_SERVER_ID=bbb-1
TOKEN=dummy-token-not-a-secret

# Fake BBB: the hook's user and directories.
if [[ $mode == packaged ]]; then
	useradd --system --no-create-home --shell /usr/sbin/nologin bigbluebutton
	mkdir -p /var/bigbluebutton/recording/raw /var/bigbluebutton/recording/status \
		/usr/local/bigbluebutton/core/scripts/post_archive
	HOOK_GID=$(id -g bigbluebutton)
	BBB_RECORDING_DIR=/var/bigbluebutton/recording
	HOOK=/usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb
else
	BBB_DIR=/srv/bbb
	mkdir -p "$BBB_DIR/data/bigbluebutton/recording/raw" "$BBB_DIR/data/bigbluebutton/recording/status"
	echo 'services: {}' >"$BBB_DIR/docker-compose.yml"
	HOOK_GID=998
	BBB_RECORDING_DIR=$BBB_DIR/data/bigbluebutton/recording
	HOOK=/etc/recap-exporter/recap_export.rb
fi

list() {
	{
		getent passwd recap-exporter
		find /etc/recap-exporter /var/lib/recap-exporter /opt/recap-exporter "$HOOK" ${BBB_DIR:+"$BBB_DIR"} \
			-printf "%p %u %g %m${1-}\n" | sort
		find /etc/recap-exporter /var/lib/recap-exporter /opt/recap-exporter "$HOOK" ${BBB_DIR:+"$BBB_DIR"} \
			-type f -exec sha256sum {} + | sort -k2
	}
}

if [[ $how == manual ]]; then
	# install-exporter.md, "Manual installation", steps 2 to 4 (downloads replaced by copies from the checkout).
	id recap-exporter >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin recap-exporter
	install -d -m 0755 /etc/recap-exporter /var/lib/recap-exporter /opt/recap-exporter
	install -d -m 2770 -o recap-exporter -g "$HOOK_GID" /var/lib/recap-exporter/spool
	install -d -m 0700 -o recap-exporter -g recap-exporter /var/lib/recap-exporter/state /etc/recap-exporter/secrets
	printf 'RECAP_EXPORTER_SERVICE_URL=%s\nRECAP_EXPORTER_BBB_SERVER_ID=%s\n' "$SERVICE_URL" "$BBB_SERVER_ID" \
		>/etc/recap-exporter/exporter.env
	printf '%s' "$TOKEN" >/etc/recap-exporter/secrets/ingest-token
	chown recap-exporter:recap-exporter /etc/recap-exporter/secrets/ingest-token
	chmod 0400 /etc/recap-exporter/secrets/ingest-token
	cd /opt/recap-exporter
	cp /src/deploy/exporter/compose.yaml compose.yaml
	cat >.env <<EOF
EXPORTER_IMAGE=ghcr.io/virtuos/bbb-recap/recap-exporter:$VERSION
EXPORTER_UID=$(id -u recap-exporter)
EXPORTER_GID=$(id -g recap-exporter)
HOOK_GID=$HOOK_GID
BBB_RECORDING_DIR=$BBB_RECORDING_DIR
RAW_MOUNT=ro
EOF
	cd /
	cp /src/hook/post_archive/recap_export.rb "$HOOK"
	chmod 0644 "$HOOK"
	if [[ $mode == docker ]]; then
		cat >"$BBB_DIR/docker-compose.override.yml" <<'EOF'
services:
  recording-workers:
    volumes:
      - /etc/recap-exporter/recap_export.rb:/usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb:ro,z
      - /etc/recap-exporter/exporter.env:/etc/recap-exporter/exporter.env:ro,z
      - /var/lib/recap-exporter/spool:/var/lib/recap-exporter/spool:z
EOF
	fi
	list >/out/$mode-manual.txt
else
	args=(--version "$VERSION" --service-url "$SERVICE_URL" --server-id "$BBB_SERVER_ID" --no-start)
	[[ $mode == docker ]] && args+=(--bbb-dir "$BBB_DIR")
	printf '%s' "$TOKEN" | bash /src/deploy/exporter/setup.sh "${args[@]}" --token-file - >/out/$mode-run1.log
	list >/out/$mode-script.txt
	list ' %T@' >/out/$mode-before.txt
	bash /src/deploy/exporter/setup.sh "${args[@]}" </dev/null >/out/$mode-run2.log
	list ' %T@' >/out/$mode-after.txt
	# A rerun with another ID, token and hook writes the files in place: a
	# single-file bind mount (docker BBB) keeps seeing them.
	files=(/etc/recap-exporter/exporter.env /etc/recap-exporter/secrets/ingest-token "$HOOK")
	inodes=$(stat -c '%n %i' "${files[@]}")
	cp -r /src /tmp/src2
	echo '# changed' >>/tmp/src2/hook/post_archive/recap_export.rb
	args2=("${args[@]}" --src /tmp/src2)
	args2[5]=bbb-2
	printf 'new-dummy-token' | bash /src/deploy/exporter/setup.sh "${args2[@]}" --token-file - >/out/$mode-run3.log
	{
		[[ $(stat -c '%n %i' "${files[@]}") == "$inodes" ]] || echo "inode changed"
		grep -qx RECAP_EXPORTER_BBB_SERVER_ID=bbb-2 /etc/recap-exporter/exporter.env || echo "ID not updated"
		[[ $(cat /etc/recap-exporter/secrets/ingest-token) == new-dummy-token ]] || echo "token not updated"
		[[ $(stat -c '%U %a' /etc/recap-exporter/secrets/ingest-token) == "recap-exporter 400" ]] || echo "token mode"
		tail -1 "$HOOK" | grep -qx '# changed' || echo "hook not updated"
	} >/out/$mode-inode.txt
	# Without a token on the first run the script must refuse.
	userdel recap-exporter
	rm -rf /etc/recap-exporter /var/lib/recap-exporter /opt/recap-exporter
	if bash /src/deploy/exporter/setup.sh "${args[@]}" </dev/null >/out/$mode-notoken.log 2>&1; then
		echo "exit 0 without token" >>/out/$mode-notoken.log
		exit 1
	fi
fi
INNER

run() {
	timeout 300 "$ENGINE" run --rm --network none \
		-v "$ROOT:/src:ro,z" -v "$WORK:/out:z" -v "$WORK/inner.sh:/inner.sh:ro,z" \
		"$BASE" bash /inner.sh "$1" "$2"
}

for mode in packaged docker; do
	run "$mode" manual || fail "$mode: manual steps failed"
	run "$mode" script || fail "$mode: script failed: $(cat "$WORK/$mode"-*.log 2>/dev/null)"
	diff "$WORK/$mode-manual.txt" "$WORK/$mode-script.txt" ||
		fail "$mode: script and manual steps differ"
	ok "$mode BBB: same user, dirs, owners, modes and file contents as the manual steps"
	diff "$WORK/$mode-before.txt" "$WORK/$mode-after.txt" || fail "$mode: second run changed files"
	grep -q 'Nothing to change' "$WORK/$mode-run2.log" || fail "$mode: second run did not say Nothing to change"
	ok "$mode BBB: second run changes nothing"
	[[ ! -s $WORK/$mode-inode.txt ]] || fail "$mode: rerun with changes: $(cat "$WORK/$mode-inode.txt")"
	ok "$mode BBB: a rerun updates exporter.env, token and hook in place (same inode)"
	grep -q 'no token' "$WORK/$mode-notoken.log" || fail "$mode: no clear error without a token"
	ok "$mode BBB: refuses a first run without token"
	if grep -q dummy-token "$WORK/$mode"-run*.log; then
		fail "$mode: token printed"
	fi
done
