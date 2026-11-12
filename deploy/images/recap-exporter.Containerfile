# recap-exporter: the exporter daemon for BBB hosts on scratch
# (docs/reference/deployment.md §1, §7; docs/operations/install-exporter.md).
# Build with `make images` from the repo root: it builds bin/recap-exporter on
# the host (static, CGO_ENABLED=0) and passes VERSION and REVISION from
# `go version -m bin/recap-exporter`. No RUN in the final stage, no shell.
# The post_archive hook is not in the image: it runs in BBB's recording
# workers, not here (bbb-exporter.md §3).

# Only provides the CA bundle (same base as recap.Containerfile).
FROM docker.io/library/alpine:3.24 AS certs

FROM scratch
ARG VERSION=unknown
ARG REVISION=unknown
LABEL org.opencontainers.image.title="recap-exporter" \
      org.opencontainers.image.source="https://github.com/virtUOS/bbb-recap" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY bin/recap-exporter /usr/local/bin/recap-exporter
# Overridden in deploy/exporter/compose.yaml with the host user that owns the
# spool, state and token file.
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/recap-exporter"]
CMD ["run"]
