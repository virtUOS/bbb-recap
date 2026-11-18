#!/usr/bin/env bash
# Installs or updates the recap exporter and the post_archive hook on a BBB
# server: steps 2 to 4 of the manual installation in
# docs/operations/install-exporter.md. Run as root:
#
#   bash setup.sh --version v0.3.0 --service-url https://recap.example.org --server-id bbb-1
#
# Docker BBB: add --bbb-dir with the directory of BBB's docker-compose.yml.
# The token (secrets/clients/bbb_server.<id> on the recap host) comes from
# --token-file (- for stdin) or is asked for without echo in a terminal. It is
# never an argument. Without a new token an existing one is kept.
#
# Safe to run again: it changes only what differs (a new release, URL, ID or
# token) and keeps other lines of exporter.env and RAW_MOUNT in .env. Files
# are rewritten in place (same inode), because docker BBB bind-mounts
# exporter.env and the hook as single files. The daemon reads its settings and
# token at start, so it is recreated when they changed; on docker BBB so is
# recording-workers. compose.yaml and the hook come from the release on GitHub,
# or from the checkout this script is in (--src overrides both).
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: bash setup.sh --version TAG --service-url URL --server-id ID [options]

  --version TAG        release, the same as the service (e.g. v0.3.0)
  --service-url URL    the recap service, https://
  --server-id ID       this server's ID: bbb_server.<id> on the service
  --bbb-dir DIR        docker BBB: the directory of BBB's docker-compose.yml
  --hook-gid GID       group of the hook's user (default: packaged BBB bigbluebutton's group, docker BBB 998)
  --token-file FILE    read the token from FILE, or from stdin with -; without it the
                       token is asked for in a terminal (a first run needs one)
  --src DIR|URL        where compose.yaml and the hook come from (a checkout or a raw URL)
  --no-start           write the files only; do not start the exporter or recreate recording-workers
EOF
}

die() {
	echo "setup.sh: $*" >&2
	exit 1
}

version='' service_url='' server_id='' bbb_dir='' hook_gid='' token_file='' src='' start=1
while [[ $# -gt 0 ]]; do
	case $1 in
	--version | --service-url | --server-id | --bbb-dir | --hook-gid | --token-file | --src)
		[[ $# -ge 2 ]] || die "$1 needs a value"
		case $1 in
		--version) version=$2 ;;
		--service-url) service_url=${2%/} ;;
		--server-id) server_id=$2 ;;
		--bbb-dir) bbb_dir=${2%/} ;;
		--hook-gid) hook_gid=$2 ;;
		--token-file) token_file=$2 ;;
		--src) src=${2%/} ;;
		esac
		shift
		;;
	--no-start) start=0 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown option $1 (see --help)" ;;
	esac
	shift
done

[[ $(id -u) -eq 0 ]] || die "run as root"
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "--version: want a release tag such as v0.3.0"
[[ $service_url =~ ^https://[^[:space:]/]+$ ]] || die "--service-url: want https://<host>, no path"
[[ $server_id =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "--server-id: want a-z, 0-9 and -"

# Where compose.yaml and the hook come from: this checkout, if the script is
# in one, else the release on GitHub.
here=$(cd "$(dirname "$0")" && pwd)
if [[ -z $src ]]; then
	if [[ -f $here/../../hook/post_archive/recap_export.rb ]]; then
		src=$(cd "$here/../.." && pwd)
	else
		src=https://raw.githubusercontent.com/virtUOS/bbb-recap/$version
	fi
fi

# Install type (install-exporter.md, Reference).
if [[ -n $bbb_dir ]]; then
	[[ -f $bbb_dir/docker-compose.yml ]] || die "--bbb-dir: no docker-compose.yml in $bbb_dir"
	hook_gid=${hook_gid:-998}
	recording_dir=$bbb_dir/data/bigbluebutton/recording
else
	id bigbluebutton >/dev/null 2>&1 || die "no user bigbluebutton: not a packaged BBB? For docker BBB use --bbb-dir"
	hook_gid=${hook_gid:-$(id -g bigbluebutton)}
	recording_dir=/var/bigbluebutton/recording
	hook_dir=/usr/local/bigbluebutton/core/scripts/post_archive
	[[ -d $hook_dir ]] || die "$hook_dir is missing: not a packaged BBB? For docker BBB use --bbb-dir"
fi
[[ $hook_gid =~ ^[0-9]+$ ]] || die "--hook-gid: want a number"

token_path=/etc/recap-exporter/secrets/ingest-token
token=
if [[ $token_file == - ]]; then
	token=$(cat)
elif [[ -n $token_file ]]; then
	token=$(cat -- "$token_file")
elif [[ ! -s $token_path && -t 0 ]]; then
	read -r -s -p "Token of $server_id (secrets/clients/bbb_server.$server_id on the recap host, not shown): " token
	echo
fi
[[ -n $token || -s $token_path ]] ||
	die "no token: use --token-file FILE (- for stdin), or run in a terminal to type it"

changed=()

# set_key FILE KEY VALUE: sets KEY=VALUE in FILE, replacing the line of KEY or
# appending one; leaves the file alone when it already has that line. Writes
# in place, so a bind mount of the file sees the change.
set_key() {
	grep -qxF "$2=$3" "$1" && return
	local tmp
	tmp=$(mktemp)
	if grep -q "^$2=" "$1"; then
		awk -v k="$2" -v v="$3" 'index($0, k "=") == 1 { print k "=" v; next } { print }' "$1" >"$tmp"
	else
		cat "$1" >"$tmp"
		printf '%s=%s\n' "$2" "$3" >>"$tmp"
	fi
	cat "$tmp" >"$1"
	rm -f "$tmp"
	changed+=("$1")
}

# fetch PATH DEST MODE: writes PATH of the release (from $src) to DEST with
# MODE, unless DEST has the same content already; in place, as set_key.
fetch() {
	local tmp
	tmp=$(mktemp)
	if [[ $src == https://* ]]; then
		curl -fsSL -o "$tmp" "$src/$1" || die "cannot download $src/$1"
	else
		cp "$src/$1" "$tmp"
	fi
	if cmp -s "$tmp" "$2"; then
		rm -f "$tmp"
		return
	fi
	cat "$tmp" >"$2"
	chmod "$3" "$2"
	rm -f "$tmp"
	changed+=("$2")
}

# was_changed PATTERN: true if a changed path matches PATTERN.
was_changed() {
	local c
	for c in "${changed[@]}"; do
		# shellcheck disable=SC2053 # PATTERN is a glob on purpose
		[[ $c == $1 ]] && return 0
	done
	return 1
}

# 1. User, directories, settings and token (manual step 2).
if ! id recap-exporter >/dev/null 2>&1; then
	useradd --system --no-create-home --shell /usr/sbin/nologin recap-exporter
	changed+=("user recap-exporter")
fi
install -d -m 0755 /etc/recap-exporter /var/lib/recap-exporter /opt/recap-exporter
install -d -m 2770 -o recap-exporter -g "$hook_gid" /var/lib/recap-exporter/spool
install -d -m 0700 -o recap-exporter -g recap-exporter /var/lib/recap-exporter/state /etc/recap-exporter/secrets

env_file=/etc/recap-exporter/exporter.env
if [[ ! -e $env_file ]]; then
	printf 'RECAP_EXPORTER_SERVICE_URL=%s\nRECAP_EXPORTER_BBB_SERVER_ID=%s\n' "$service_url" "$server_id" >"$env_file"
	chmod 0644 "$env_file"
	changed+=("$env_file")
else
	set_key "$env_file" RECAP_EXPORTER_SERVICE_URL "$service_url"
	set_key "$env_file" RECAP_EXPORTER_BBB_SERVER_ID "$server_id"
fi

if [[ -n $token && (! -e $token_path || $(cat "$token_path") != "$token") ]]; then
	# In place, as set_key; a new file is created with mode 0600 at most.
	(
		umask 077
		printf '%s' "$token" >"$token_path"
	)
	changed+=("$token_path")
fi
unset token
chown recap-exporter:recap-exporter "$token_path"
chmod 0400 "$token_path"

# 2. The daemon's compose project (manual step 3).
fetch deploy/exporter/compose.yaml /opt/recap-exporter/compose.yaml 0644
compose_env=/opt/recap-exporter/.env
if [[ ! -e $compose_env ]]; then
	cat >"$compose_env" <<EOF
EXPORTER_IMAGE=ghcr.io/virtuos/bbb-recap/recap-exporter:$version
EXPORTER_UID=$(id -u recap-exporter)
EXPORTER_GID=$(id -g recap-exporter)
HOOK_GID=$hook_gid
BBB_RECORDING_DIR=$recording_dir
RAW_MOUNT=ro
EOF
	changed+=("$compose_env")
else
	# RAW_MOUNT stays as the admin set it (install-exporter.md, "Delete raw data after upload").
	set_key "$compose_env" EXPORTER_IMAGE "ghcr.io/virtuos/bbb-recap/recap-exporter:$version"
	set_key "$compose_env" EXPORTER_UID "$(id -u recap-exporter)"
	set_key "$compose_env" EXPORTER_GID "$(id -g recap-exporter)"
	set_key "$compose_env" HOOK_GID "$hook_gid"
	set_key "$compose_env" BBB_RECORDING_DIR "$recording_dir"
fi

# 3. The hook (manual step 4).
override_note=
if [[ -z $bbb_dir ]]; then
	fetch hook/post_archive/recap_export.rb "$hook_dir/recap_export.rb" 0644
	su -s /bin/sh -c 'test -w /var/lib/recap-exporter/spool' bigbluebutton ||
		die "user bigbluebutton cannot write /var/lib/recap-exporter/spool; check that its group is $hook_gid"
else
	fetch hook/post_archive/recap_export.rb /etc/recap-exporter/recap_export.rb 0644
	override=$bbb_dir/docker-compose.override.yml
	if [[ ! -e $override ]]; then
		cat >"$override" <<'EOF'
services:
  recording-workers:
    volumes:
      - /etc/recap-exporter/recap_export.rb:/usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb:ro,z
      - /etc/recap-exporter/exporter.env:/etc/recap-exporter/exporter.env:ro,z
      - /var/lib/recap-exporter/spool:/var/lib/recap-exporter/spool:z
EOF
		changed+=("$override")
	elif ! grep -q recap_export.rb "$override"; then
		# The script does not edit an override file it did not write.
		override_note="Add the three recording-workers volumes of install-exporter.md (Docker BBB, step 4) to
$override, then run: cd $bbb_dir && docker compose up -d recording-workers"
	fi
fi

if [[ ${#changed[@]} -gt 0 ]]; then
	echo "Changed:"
	printf '  %s\n' "${changed[@]}" | sort -u
else
	echo "Nothing to change."
fi

# 4. Start (or update) the daemon and the hook's container.
if [[ $start -eq 1 ]]; then
	cd /opt/recap-exporter
	docker compose pull --quiet
	docker compose run --rm exporter check-config
	# The daemon reads exporter.env and the token at start only.
	if was_changed "$env_file" || was_changed "$token_path"; then
		docker compose up -d --force-recreate exporter
	else
		docker compose up -d
	fi
	# Docker BBB: recording-workers runs the hook with the mounted files. On
	# packaged BBB each post_archive script runs as its own process
	# (upstream-facts.md), so the new hook needs no restart.
	if [[ -n $bbb_dir && -z $override_note ]]; then
		if was_changed "$env_file" || was_changed /etc/recap-exporter/recap_export.rb || was_changed "$bbb_dir/*"; then
			(cd "$bbb_dir" && docker compose up -d --force-recreate recording-workers)
		else
			(cd "$bbb_dir" && docker compose up -d recording-workers)
		fi
	fi
	ready=no
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		if [[ $(curl -fsS http://127.0.0.1:9466/readyz 2>/dev/null) == ok ]]; then
			ready=yes
			break
		fi
		sleep 3
	done
	[[ $ready == yes ]] ||
		die "exporter not ready after 30 s; see: cd /opt/recap-exporter && docker compose logs --tail 50 exporter"
	echo "exporter ready: yes"
fi

if [[ -n $override_note ]]; then
	echo "$override_note" >&2
	exit 1
fi
if [[ $start -eq 0 ]]; then
	echo "Not started (--no-start). To start: cd /opt/recap-exporter && docker compose up -d"
	[[ -z $bbb_dir ]] || echo "and: cd $bbb_dir && docker compose up -d recording-workers"
	exit 0
fi
cat <<EOF
Test: record a meeting that a front end registered for recap, stop and end it. About a minute after BBB archived it,
  cd /opt/recap-exporter && docker compose exec exporter /usr/local/bin/recap-exporter status
shows the recording as acked.
EOF
