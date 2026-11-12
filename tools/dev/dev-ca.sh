#!/bin/sh
# Copies the root certificate of the dev TLS front (service tls, Caddy's local
# CA) out of the running container, for the exporter's SSL_CERT_FILE or
# curl --cacert. Caddy keeps it in its data volume at
# /data/caddy/pki/authorities/local/root.crt.
#
#   dev-ca.sh OUT [PROJECT]   PROJECT: the compose project name (default recap)
set -eu

CA=/data/caddy/pki/authorities/local/root.crt

[ $# -ge 1 ] && [ $# -le 2 ] || {
	echo "usage: $0 OUT [PROJECT]" >&2
	exit 2
}
OUT=$1
CONTAINER=${2:-recap}_tls_1

podman container exists "$CONTAINER" || {
	echo "dev-ca: container $CONTAINER not found: is the dev overlay (service tls) up?" >&2
	exit 1
}
# Caddy creates the CA right after start.
i=0
until podman cp "$CONTAINER:$CA" "$OUT" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 30 ] || {
		echo "dev-ca: $CA not in $CONTAINER after 30 s" >&2
		exit 1
	}
	sleep 1
done
echo "dev-ca: wrote $OUT"
