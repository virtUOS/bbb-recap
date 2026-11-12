# recap-mock: mock ASR, LLM and Docs for dev and CI, never in production
# (docs/reference/deployment.md §1). Build with `make images` from the repo root.
# It carries the fixture data the mock answers from, but no keys: they come
# from the dev secrets mount (-asr-key, -llm-key, -docs-key).

# Only provides the CA bundle.
FROM docker.io/library/alpine:3.24 AS certs

FROM scratch
ARG VERSION=unknown
ARG REVISION=unknown
LABEL org.opencontainers.image.title="recap-mock" \
      org.opencontainers.image.source="https://github.com/virtUOS/bbb-recap" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY bin/recap-mock /usr/local/bin/recap-mock
COPY testdata/providers/ /fixtures/providers/
COPY testdata/render/ /fixtures/render/
COPY testdata/sink/ /fixtures/sink/
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/recap-mock"]
