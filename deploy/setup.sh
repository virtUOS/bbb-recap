#!/usr/bin/env bash
# Sets up the recap service with docker compose (or podman compose) on this
# host: .env, config/*.yaml and secrets/ in the install directory from the
# examples next to this script, then check-config and compose up.
# Guide: docs/operations/install-compose.md.
#
# Usage: deploy/setup.sh [--dir DIR] [--non-interactive] [--no-start] [--force]
#
# Each question can be answered in advance with the environment variable shown
# in brackets (for Ansible or CI). With --non-interactive a missing required
# value is an error. Existing files are kept: a second run changes nothing.
# --force rewrites the config files (compose.yaml, .env, config/*.yaml) from
# the answers, which discards hand edits in them; each old file is kept as
# <file>.bak.<time>. It never touches a secret. Secrets are never printed,
# except the client tokens created in this run, once, at the end.
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: deploy/setup.sh [--dir DIR] [--non-interactive] [--no-start] [--force]

  --dir DIR          install directory (RECAP_SETUP_DIR, default /opt/recap)
  --non-interactive  ask nothing; fail on a missing required value
  --no-start         write the files and run check-config, do not start
  --force            rewrite compose.yaml, .env and config/*.yaml from the answers (hand edits
                     in them are lost; each old file is kept as <file>.bak.<time>)

Answers from the environment (questions not needed for an existing file are skipped):
  RECAP_SETUP_VERSION       release tag, e.g. v0.3.0 (default: the tag of this checkout)
  RECAP_SETUP_IMAGE         image (default ghcr.io/virtuos/bbb-recap/recap:<version>)
  RECAP_SETUP_HOST          public host name of the service, e.g. recap.example.org
  RECAP_SETUP_ASR_DIALECT   whisperx-family (default) or transkribierbar
  RECAP_SETUP_ASR_URL       ASR server root, e.g. https://asr.example.org
  RECAP_SETUP_ASR_MODEL     ASR model (default large-v3)
  RECAP_SETUP_ASR_KEY       ASR key, or none; or RECAP_SETUP_ASR_KEY_FILE with the key
  RECAP_SETUP_LLM_URL       LLM endpoint including /v1, e.g. https://llm.example.org/v1
  RECAP_SETUP_LLM_MODEL     served model name
  RECAP_SETUP_LLM_CONTEXT   context length in tokens (default 32768)
  RECAP_SETUP_LLM_KEY       LLM key, or none; or RECAP_SETUP_LLM_KEY_FILE
  RECAP_SETUP_OPERATOR      who runs ASR, LLM and Docs (privacy declarations)
  RECAP_SETUP_SINK          docs (La Suite Docs and the front end, default) or frontend (front end only)
  RECAP_SETUP_DOCS_URL      La Suite Docs URL, e.g. https://docs.example.org
  RECAP_SETUP_DOCS_KEY      Docs server-to-server key; or RECAP_SETUP_DOCS_KEY_FILE
  RECAP_SETUP_BBB_SERVERS   BBB server IDs, comma-separated, e.g. bbb-1,bbb-2
  RECAP_SETUP_FRONTENDS     front-end IDs, comma-separated, e.g. pilos-1
  RECAP_SETUP_OIDC_ISSUER   OIDC issuer URL for the admin UI (empty: no admin UI)
  RECAP_SETUP_OIDC_CLIENT_ID      OIDC client ID (default recap-admin-ui)
  RECAP_SETUP_OIDC_CLIENT_SECRET  OIDC client secret; or RECAP_SETUP_OIDC_CLIENT_SECRET_FILE
  RECAP_SETUP_COMPOSE       compose command (default: docker compose, else podman compose)
EOF
}

die() {
	echo "setup.sh: $*" >&2
	exit 1
}

# --- Options

src=$(cd "$(dirname "$0")" && pwd) # deploy/ of the release checkout
dir=${RECAP_SETUP_DIR:-/opt/recap}
interactive=1
start=1
force=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--dir)
		[[ $# -ge 2 ]] || die "--dir needs a value"
		dir=$2
		shift
		;;
	--non-interactive) interactive=0 ;;
	--no-start) start=0 ;;
	--force) force=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown option $1 (see --help)" ;;
	esac
	shift
done
if [[ $interactive -eq 1 && ! -t 0 ]]; then
	die "stdin is not a terminal; use --non-interactive and the RECAP_SETUP_* variables"
fi
for f in compose.yaml .env.example config/providers.yaml config/sinks.yaml config/meeting-types.yaml; do
	[[ -f $src/$f ]] || die "$src/$f is missing; run this script from a bbb-recap checkout"
done

# --- Questions

# ask VAR QUESTION [DEFAULT]: VAR keeps its value from the environment; else
# the answer, or DEFAULT. Without --non-interactive it asks until it gets a
# value; with it, a missing value without default is an error.
ask() {
	local var=$1 question=$2 default=${3-} answer=
	[[ -n ${!var-} ]] && return
	if [[ $interactive -eq 1 ]]; then
		while [[ -z $answer ]]; do
			read -r -p "$question${default:+ [$default]}: " answer
			answer=${answer:-$default}
		done
	else
		answer=$default
		[[ -n $answer ]] || die "missing value: $question (set $var)"
	fi
	printf -v "$var" '%s' "$answer"
}

# ask_optional VAR QUESTION: like ask, but an empty answer is allowed.
ask_optional() {
	local var=$1 question=$2 answer=
	[[ -n ${!var-} ]] && return
	if [[ $interactive -eq 1 ]]; then
		read -r -p "$question (empty: none): " answer
	fi
	printf -v "$var" '%s' "$answer"
}

# ask_secret VAR QUESTION: from VAR, from the file named in VAR_FILE, or asked
# without echo. Never printed.
ask_secret() {
	local var=$1 question=$2 file_var=${1}_FILE answer=
	[[ -n ${!var-} ]] && return
	if [[ -n ${!file_var-} ]]; then
		[[ -r ${!file_var} ]] || die "$file_var: cannot read ${!file_var}"
		answer=$(cat -- "${!file_var}")
	elif [[ $interactive -eq 1 ]]; then
		while [[ -z $answer ]]; do
			read -r -s -p "$question (not shown): " answer
			echo
		done
	fi
	[[ -n $answer ]] || die "missing value: $question (set $var or $file_var)"
	printf -v "$var" '%s' "$answer"
}

# check VALUE REGEX WHAT: fails unless VALUE matches the extended REGEX.
check() {
	[[ $1 =~ $2 ]] || die "$3: invalid value '$1'"
}

url_re='^https?://[^[:space:]"#]+$'
model_re='^[A-Za-z0-9._:/@-]+$'
id_re='^[a-z0-9][a-z0-9-]{0,62}$'

# new FILE: true if FILE is to be written in this run (missing, or a config
# file with --force).
new() {
	[[ ! -e $dir/$1 || $force -eq 1 ]]
}

secret_missing() {
	[[ ! -e $dir/secrets/$1 ]]
}

[[ -d $dir ]] || mkdir -m 0700 "$dir" 2>/dev/null ||
	die "cannot create $dir; create it first: sudo install -d -m 0700 -o \"\$(id -u)\" -g \"\$(id -g)\" $dir"
[[ -w $dir ]] || die "$dir is not writable for $(id -un)"
# The secrets inside are readable for every user (the containers need that),
# so only the install dir protects them.
[[ $(stat -c %a "$dir") == 700 ]] ||
	die "$dir has mode $(stat -c %a "$dir"); it must be 0700, because the secrets in it are world-readable:" \
		"chmod 0700 $dir"
fresh=1
[[ -e $dir/.env ]] && fresh=0
# The *.new files below are temporary.
trap 'rm -f "$dir/.env.new" "$dir/config/providers.yaml.new" "$dir/config/sinks.yaml.new"' EXIT

# ignored FILE VAR...: warns about each VAR that is set although FILE is kept.
ignored() {
	local file=$1 var
	shift
	for var; do
		[[ -z ${!var-} ]] || echo "setup.sh: $file exists, $var is ignored (--force rewrites $file)" >&2
	done
}

# Release and image: from the tag of this checkout unless given.
if new .env; then
	if [[ -z ${RECAP_SETUP_IMAGE-} ]]; then
		[[ -n ${RECAP_SETUP_VERSION-} ]] ||
			RECAP_SETUP_VERSION=$(git -C "$src" describe --tags --exact-match 2>/dev/null || true)
		ask RECAP_SETUP_VERSION "Release tag (e.g. v0.3.0)"
		check "$RECAP_SETUP_VERSION" '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' "release tag"
		RECAP_SETUP_IMAGE=ghcr.io/virtuos/bbb-recap/recap:$RECAP_SETUP_VERSION
	fi
	check "$RECAP_SETUP_IMAGE" '^[A-Za-z0-9./:@_-]+$' "image"
	ask RECAP_SETUP_HOST "Public host name of this service (e.g. recap.example.org)"
	RECAP_SETUP_HOST=${RECAP_SETUP_HOST#https://}
	RECAP_SETUP_HOST=${RECAP_SETUP_HOST%/}
	check "$RECAP_SETUP_HOST" '^[A-Za-z0-9.-]+(:[0-9]+)?$' "host name"
	ask_optional RECAP_SETUP_OIDC_ISSUER \
		"OIDC issuer URL for the admin UI, e.g. https://keycloak.example.org/realms/example"
	if [[ -n $RECAP_SETUP_OIDC_ISSUER ]]; then
		check "$RECAP_SETUP_OIDC_ISSUER" "$url_re" "OIDC issuer"
		ask RECAP_SETUP_OIDC_CLIENT_ID "OIDC client ID" recap-admin-ui
		check "$RECAP_SETUP_OIDC_CLIENT_ID" '^[A-Za-z0-9._:-]+$' "OIDC client ID"
		if secret_missing oidc-client-secret; then
			ask_secret RECAP_SETUP_OIDC_CLIENT_SECRET "OIDC client secret"
		fi
	fi
else
	ignored .env RECAP_SETUP_VERSION RECAP_SETUP_IMAGE RECAP_SETUP_HOST RECAP_SETUP_OIDC_ISSUER RECAP_SETUP_OIDC_CLIENT_ID
	# For the next steps below: the values of the existing .env.
	RECAP_SETUP_VERSION=
	RECAP_SETUP_IMAGE=$(sed -n 's/^RECAP_IMAGE=//p' "$dir/.env")
	base_url=$(sed -n 's/^RECAP_PUBLIC_BASE_URL=//p' "$dir/.env")
	RECAP_SETUP_HOST=${base_url#https://}
	RECAP_SETUP_OIDC_ISSUER=$(sed -n 's/^RECAP_OIDC_ISSUER=//p' "$dir/.env")
fi

if new config/providers.yaml || new config/sinks.yaml; then
	ask RECAP_SETUP_OPERATOR "Who runs the ASR server, the LLM endpoint and Docs (for the privacy declarations)"
	[[ $RECAP_SETUP_OPERATOR != *[\"\\$'\n']* ]] || die "operator: no \", \\ or line break please"
fi
new config/providers.yaml || ignored config/providers.yaml RECAP_SETUP_ASR_DIALECT RECAP_SETUP_ASR_URL \
	RECAP_SETUP_ASR_MODEL RECAP_SETUP_LLM_URL RECAP_SETUP_LLM_MODEL RECAP_SETUP_LLM_CONTEXT
new config/sinks.yaml || ignored config/sinks.yaml RECAP_SETUP_SINK RECAP_SETUP_DOCS_URL
if new config/providers.yaml; then
	ask RECAP_SETUP_ASR_DIALECT "ASR dialect: whisperx-family or transkribierbar" whisperx-family
	check "$RECAP_SETUP_ASR_DIALECT" '^(whisperx-family|transkribierbar)$' "ASR dialect"
	ask RECAP_SETUP_ASR_URL "ASR server root URL, no path (e.g. https://asr.example.org)"
	check "$RECAP_SETUP_ASR_URL" "$url_re" "ASR URL"
	ask RECAP_SETUP_ASR_MODEL "ASR model" large-v3
	check "$RECAP_SETUP_ASR_MODEL" "$model_re" "ASR model"
	if secret_missing providers/asr.key; then
		ask_secret RECAP_SETUP_ASR_KEY "ASR API key, or none"
	fi
	ask RECAP_SETUP_LLM_URL "LLM endpoint including /v1 (e.g. https://llm.example.org/v1)"
	check "$RECAP_SETUP_LLM_URL" "$url_re" "LLM URL"
	ask RECAP_SETUP_LLM_MODEL "LLM model name as served"
	check "$RECAP_SETUP_LLM_MODEL" "$model_re" "LLM model"
	ask RECAP_SETUP_LLM_CONTEXT "LLM context length in tokens" 32768
	check "$RECAP_SETUP_LLM_CONTEXT" '^[0-9]+$' "LLM context length"
	((RECAP_SETUP_LLM_CONTEXT > 4096)) || die "LLM context length: must be above max_output_tokens (4096)"
	if secret_missing providers/llm.key; then
		ask_secret RECAP_SETUP_LLM_KEY "LLM API key, or none"
	fi
fi
if new config/sinks.yaml; then
	ask RECAP_SETUP_SINK \
		"Where summaries go: docs (La Suite Docs; the front end may fetch them too) or frontend (front end only)" docs
	check "$RECAP_SETUP_SINK" '^(docs|frontend)$' "sink"
	if [[ $RECAP_SETUP_SINK == docs ]]; then
		ask RECAP_SETUP_DOCS_URL "La Suite Docs URL (e.g. https://docs.example.org)"
		check "$RECAP_SETUP_DOCS_URL" "$url_re" "Docs URL"
		if secret_missing sinks/docs.key; then
			ask_secret RECAP_SETUP_DOCS_KEY "Docs server-to-server key (one of DJANGO_SERVER_TO_SERVER_API_TOKENS)"
		fi
	fi
fi

# existing KIND: the IDs of the existing secrets/clients/KIND.<id> files, comma-separated.
existing() {
	local f ids=
	for f in "$dir"/secrets/clients/"$1".*; do
		[[ -e $f && $f != *.next ]] || continue
		ids=${ids:+$ids,}${f##*/"$1".}
	done
	printf '%s' "$ids"
}
ask RECAP_SETUP_BBB_SERVERS "BBB server IDs, comma-separated (e.g. bbb-1; one ID for a whole BBB cluster)" \
	"$(existing bbb_server)"
ask RECAP_SETUP_FRONTENDS "Front-end IDs, comma-separated (e.g. pilos-1)" "$(existing frontend)"
IFS=, read -r -a bbb_servers <<<"$RECAP_SETUP_BBB_SERVERS"
IFS=, read -r -a frontends <<<"$RECAP_SETUP_FRONTENDS"
for id in "${bbb_servers[@]}" "${frontends[@]}"; do
	check "$id" "$id_re" "client ID (a-z, 0-9 and -)"
done

# --- Files

umask 077
created=()

# put FILE MODE: writes stdin to FILE (relative to the install dir) with MODE
# unless the file exists; with --force an existing config file is kept as .bak.
put() {
	local path=$dir/$1 tmp
	tmp=$(mktemp "$path.XXXXXX")
	cat >"$tmp"
	chmod "$2" "$tmp"
	if [[ -e $path ]]; then
		if cmp -s "$tmp" "$path"; then
			rm -f "$tmp"
			return
		fi
		cp -p "$path" "$path.bak.$(date +%Y%m%d-%H%M%S)"
	fi
	mv "$tmp" "$path"
	created+=("$1")
}

# put_secret FILE VALUE: writes a secret file once; never overwrites one.
put_secret() {
	[[ -e $dir/secrets/$1 ]] && return
	put "secrets/$1" 0644 <<<"$2"
}

token() {
	od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
}

# sed_value VALUE: VALUE escaped for the replacement part of s|...|...|.
sed_value() {
	printf '%s' "$1" | sed 's/[\\&|]/\\&/g'
}

# The containers run as UID 10001 and read config and secrets through the
# "other" bits; the install dir itself stays 0700 (install-compose.md).
# Only missing directories: an admin may have tightened existing ones.
for d in config secrets secrets/clients secrets/providers secrets/sinks; do
	[[ -d $dir/$d ]] || install -d -m 0755 "$dir/$d"
done

if new compose.yaml; then
	put compose.yaml 0644 <"$src/compose.yaml"
fi

if new .env; then
	oidc_client_id=
	[[ -n $RECAP_SETUP_OIDC_ISSUER ]] && oidc_client_id=$RECAP_SETUP_OIDC_CLIENT_ID
	sed -e "s|^RECAP_IMAGE=.*|RECAP_IMAGE=$(sed_value "$RECAP_SETUP_IMAGE")|" \
		-e "s|^RECAP_PUBLIC_BASE_URL=.*|RECAP_PUBLIC_BASE_URL=https://$(sed_value "$RECAP_SETUP_HOST")|" \
		-e "s|^RECAP_OIDC_ISSUER=.*|RECAP_OIDC_ISSUER=$(sed_value "$RECAP_SETUP_OIDC_ISSUER")|" \
		-e "s|^RECAP_OIDC_CLIENT_ID=.*|RECAP_OIDC_CLIENT_ID=$(sed_value "$oidc_client_id")|" \
		"$src/.env.example" >"$dir/.env.new"
	put .env 0600 <"$dir/.env.new"
	rm -f "$dir/.env.new"
fi

if new config/providers.yaml; then
	# Each sed line replaces one REPLACE value of the example, comment included.
	sed -e "s|^\( *dialect:\) whisperx-family$|\1 $RECAP_SETUP_ASR_DIALECT|" \
		-e "s|^\( *base_url:\) https://asr.example.org .*|\1 $(sed_value "$RECAP_SETUP_ASR_URL")|" \
		-e "s|^\( *model:\) large-v3 .*|\1 $(sed_value "$RECAP_SETUP_ASR_MODEL")|" \
		-e "s|^\( *base_url:\) https://llm.example.org/v1 .*|\1 $(sed_value "$RECAP_SETUP_LLM_URL")|" \
		-e "s|^\( *model:\) example-model .*|\1 $(sed_value "$RECAP_SETUP_LLM_MODEL")|" \
		-e "s|^\( *context_tokens:\) 32768 .*|\1 $RECAP_SETUP_LLM_CONTEXT|" \
		-e "s|^\( *operator:\) \"REPLACE: .*\"|\1 \"$(sed_value "$RECAP_SETUP_OPERATOR")\"|" \
		"$src/config/providers.yaml" >"$dir/config/providers.yaml.new"
	# transkribierBAR keeps a transcription until the adapter's cleanup, and an
	# upload whose transcription never started until its own expiry, 72 h by
	# default (docs/reference/providers.md §2a.4, Known limitations).
	if [[ $RECAP_SETUP_ASR_DIALECT == transkribierbar ]]; then
		retention="Until the cleanup of each job deletes the transcription; an upload whose transcription"
		retention+=" never started until transkribierBAR deletes it (DEFAULT_EXPIRY_HOURS, default 72 h)"
		sed -i -e "s|^\( *retention:\) \"No persistence (RESULT_STORE__ENABLED=false)\"\$|\1 \"$retention\"|" \
			-e 's|^\( *deletion:\) none # adapter_cleanup.*|\1 adapter_cleanup|' "$dir/config/providers.yaml.new"
	fi
	# "none": a server without key; the profile then has no credentials_file.
	for k in asr llm; do
		var=RECAP_SETUP_${k^^}_KEY
		if secret_missing "providers/$k.key" && [[ ${!var} == none ]]; then
			sed -i "\|credentials_file: /run/secrets/recap/providers/$k.key|d" "$dir/config/providers.yaml.new"
		fi
	done
	put config/providers.yaml 0644 <"$dir/config/providers.yaml.new"
	rm -f "$dir/config/providers.yaml.new"
fi

if new config/sinks.yaml; then
	if [[ $RECAP_SETUP_SINK == docs ]]; then
		sed -e "s|^\( *docs_base_url:\) https://docs.example.org .*|\1 $(sed_value "$RECAP_SETUP_DOCS_URL")|" \
			-e "s|^\( *operator:\) \"REPLACE: .*\"|\1 \"$(sed_value "$RECAP_SETUP_OPERATOR")\"|" \
			"$src/config/sinks.yaml" >"$dir/config/sinks.yaml.new"
	else
		# Front end only: drop the docs-main profile (from its comment to the next profile's).
		awk '/^  # La Suite Docs/ { skip = 1 } /^  # The front end that registered/ { skip = 0 } !skip' \
			"$src/config/sinks.yaml" >"$dir/config/sinks.yaml.new"
	fi
	put config/sinks.yaml 0644 <"$dir/config/sinks.yaml.new"
	rm -f "$dir/config/sinks.yaml.new"
fi

if new config/meeting-types.yaml; then
	put config/meeting-types.yaml 0644 <"$src/config/meeting-types.yaml"
fi

if grep -v '^ *#' "$dir"/config/*.yaml | grep -q REPLACE; then
	echo "setup.sh: warning: a REPLACE value is left in config/; check: grep -n REPLACE $dir/config/*.yaml" >&2
fi

put_secret db-password "$(token)"
[[ -n $(existing admin) ]] || put_secret clients/admin.ops "$(token)"
new_tokens=()
for id in "${bbb_servers[@]}"; do
	secret_missing "clients/bbb_server.$id" && new_tokens+=("bbb_server.$id")
	put_secret "clients/bbb_server.$id" "$(token)"
done
for id in "${frontends[@]}"; do
	secret_missing "clients/frontend.$id" && new_tokens+=("frontend.$id")
	put_secret "clients/frontend.$id" "$(token)"
done
[[ ${RECAP_SETUP_ASR_KEY:-none} == none ]] || put_secret providers/asr.key "$RECAP_SETUP_ASR_KEY"
[[ ${RECAP_SETUP_LLM_KEY:-none} == none ]] || put_secret providers/llm.key "$RECAP_SETUP_LLM_KEY"
if [[ -n ${RECAP_SETUP_DOCS_KEY-} ]] && grep -q '^ *type: docs$' "$dir/config/sinks.yaml"; then
	put_secret sinks/docs.key "$RECAP_SETUP_DOCS_KEY"
fi
if [[ -n $RECAP_SETUP_OIDC_ISSUER ]]; then
	[[ -z ${RECAP_SETUP_OIDC_CLIENT_SECRET-} ]] || put_secret oidc-client-secret "$RECAP_SETUP_OIDC_CLIENT_SECRET"
	put_secret session-key "$(token)"
fi
# The keys are on disk now; compose and the containers do not need them.
unset RECAP_SETUP_ASR_KEY RECAP_SETUP_LLM_KEY RECAP_SETUP_DOCS_KEY RECAP_SETUP_OIDC_CLIENT_SECRET

if [[ ${#created[@]} -gt 0 ]]; then
	echo "Written in $dir:"
	printf '  %s\n' "${created[@]}"
else
	echo "Nothing to change in $dir."
fi

# --- Check and start

if [[ -n ${RECAP_SETUP_COMPOSE-} ]]; then
	read -r -a compose <<<"$RECAP_SETUP_COMPOSE"
elif docker compose version >/dev/null 2>&1; then
	compose=(docker compose)
elif podman compose version >/dev/null 2>&1; then
	compose=(podman compose)
else
	die "neither docker compose nor podman compose works; install one, or set RECAP_SETUP_COMPOSE"
fi
cd "$dir"
echo "Checking the configuration (${compose[*]} run --rm --no-deps api check-config):"
"${compose[@]}" run --rm --no-deps api check-config ||
	die "check-config failed; fix the files named above, then run this script again"
if [[ $start -eq 1 ]]; then
	"${compose[@]}" up -d
	# api and worker read config and secrets at start: after a change to an
	# existing installation (e.g. a new token) they must be recreated.
	if [[ ${#created[@]} -gt 0 && $fresh -eq 0 ]]; then
		"${compose[@]}" up -d --force-recreate api worker
	fi
	"${compose[@]}" ps
fi

# --- Next steps

service_url=https://${RECAP_SETUP_HOST:-<this host>}
# The release for the exporter: given, or the tag of a release image.
version=${RECAP_SETUP_VERSION:-${RECAP_SETUP_IMAGE##*:}}
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || version='<release>'
port=$(sed -n 's/^RECAP_HTTP_PORT=//p' .env)
port=${port:-8080}
sink_names=$(sed -n 's/^  - name: //p' config/sinks.yaml | paste -sd, -)
cat <<EOF

Next steps (docs/operations/integration.md has the whole picture):

1. Reverse proxy: TLS for $service_url to 127.0.0.1:$port (deploy/Caddyfile.example).
   Check: curl -sS -o /dev/null -w '%{http_code}\n' $service_url/   # expect 404

2. On each BBB server, as root (docs/operations/install-exporter.md); the script asks for the server's token.
   Docker BBB: add --bbb-dir with the directory of BBB's docker-compose.yml.
     curl -fsSLO https://raw.githubusercontent.com/virtUOS/bbb-recap/$version/deploy/exporter/setup.sh
EOF
for id in "${bbb_servers[@]}"; do
	echo "     bash setup.sh --version $version --service-url $service_url --server-id $id   # on $id"
done
cat <<EOF

3. In each front end (e.g. PILOS with a recap integration): service URL $service_url, its token below,
   meeting type meeting-summary (config/meeting-types.yaml) and a sink profile from config/sinks.yaml
   ($sink_names).
EOF
if [[ -n $RECAP_SETUP_OIDC_ISSUER ]]; then
	cat <<EOF

4. Admin UI: in the OIDC provider, the client's redirect URI is $service_url/admin/callback and admins
   need the realm role recap-admin (install-compose.md, "Admin UI with Keycloak"). Then open $service_url/admin/.
EOF
fi
if [[ ${#new_tokens[@]} -gt 0 ]]; then
	echo
	echo "Tokens created now, shown only this once (also in $dir/secrets/clients/). Hand each to its admin:"
	for name in "${new_tokens[@]}"; do
		printf '  %-32s %s\n' "$name" "$(cat "$dir/secrets/clients/$name")"
	done
else
	echo
	echo "No new tokens; the existing ones are in $dir/secrets/clients/."
fi
