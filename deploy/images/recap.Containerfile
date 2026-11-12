# recap: the service binary on scratch (docs/reference/deployment.md §1).
# Build with `make images` from the repo root: it builds bin/recap on the host
# (static, CGO_ENABLED=0) and passes VERSION and REVISION from
# `go version -m bin/recap`. No RUN in the final stage, no shell in the image.

# Only provides the CA bundle and an empty directory for the blob volume.
FROM docker.io/library/alpine:3.24 AS certs
RUN mkdir /blobs

FROM scratch
ARG VERSION=unknown
ARG REVISION=unknown
LABEL org.opencontainers.image.title="recap" \
      org.opencontainers.image.source="https://github.com/virtUOS/bbb-recap" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY bin/recap /usr/local/bin/recap
# The built-in formats at the default RECAP_FORMATS_DIR (templates.md §1).
COPY formats/ /etc/recap/formats/
# Mount point of the fs blob backend (RECAP_FS_ROOT in compose), owned by the
# service user so the read-only container can write to a named volume here.
# Podman chowns a new named volume to the container user on first mount
# (podman-run(1), "Chowning Volume Mounts"). Docker copies the image directory
# into a new, empty named volume
# (https://docs.docker.com/engine/storage/volumes/#populate-a-volume-using-a-container);
# that it also takes over this owner is not verified here.
COPY --from=certs --chown=10001:10001 /blobs /var/lib/recap/blobs
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/recap"]
