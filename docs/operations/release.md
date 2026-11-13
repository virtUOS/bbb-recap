# Releases and published images

For maintainers. Image rules: [deployment.md](../reference/deployment.md) §1; upgrade order: §8.

## What CI publishes

| Workflow                       | Trigger                                | Publishes                                                                     |
| ------------------------------ | -------------------------------------- | ----------------------------------------------------------------------------- |
| `.github/workflows/ci.yaml`    | every push and pull request            | nothing (`make lint`, `make test`, `make images`, hook tests)                 |
| `.github/workflows/image.yaml` | push to `main`, a `v*` tag, manual run | `ghcr.io/virtuos/bbb-recap/recap`, `ghcr.io/virtuos/bbb-recap/recap-exporter` |

| Git ref            | Image tags (`linux/amd64` only) |
| ------------------ | ------------------------------- |
| tag `vX.Y.Z`       | `vX.Y.Z`, `X.Y`, `sha-<7 hex>`  |
| tag `vX.Y.Z-<pre>` | `vX.Y.Z-<pre>`, `sha-<7 hex>`   |
| branch `main`      | `edge`, `sha-<7 hex>`           |

- `recap-mock` is never published. Other refs fail the workflow before anything is pushed.
- Builds use the latest Go 1.27 patch (`check-latest: true`), not the minimum in `go.mod`.
- **Label check:** `org.opencontainers.image.version` is the module version stamped into the binary and
  `org.opencontainers.image.revision` the full commit; `recap_build_info{version,commit}` shows the same. The workflow
  fails if the revision is not the built commit, the version is `+dirty`, unknown or `(devel)`, or, for a tag, not the
  tag.
- **Tags must be `v0.x.y` or `v1.x.y`.** The module path has no `/v2` suffix, so Go stamps a pseudo-version for `v2+`
  tags and the label check fails.
- Images built in a linked git worktree carry `(devel)`/`unknown` labels (no VCS stamping there); release images come
  from CI clones.

## Make the package pullable (once)

A new ghcr.io package is private. After the first image run, either make `bbb-recap/recap` and
`bbb-recap/recap-exporter` public (organization → Packages → package settings → change visibility), or log in on each
host with a `read:packages` token:

```sh
GHCR_USER=your-github-login
read -rs GHCR_TOKEN   # paste the token, then Enter
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io --username "$GHCR_USER" --password-stdin; unset GHCR_TOKEN
```

## Cut a release

With CI green on the commit, from a checkout with push rights:

```sh
VERSION=v0.1.0
git switch main && git pull --ff-only
git tag -a "$VERSION" -m "recap $VERSION" && git push origin "$VERSION"
sleep 10
gh run watch "$(gh run list --workflow image.yaml --branch "$VERSION" --limit 1 --json databaseId --jq '.[0].databaseId')" --exit-status
```

If the release cannot run steps of jobs the previous release created, its notes say "drain first".

Check the published image (expect the tag, the commit of the tag and `user=10001:10001`):

```sh
IMAGE_REF=ghcr.io/virtuos/bbb-recap/recap:$VERSION
podman pull "$IMAGE_REF"
podman image inspect --format '{{ index .Labels "org.opencontainers.image.version" }} {{ index .Labels "org.opencontainers.image.revision" }} user={{.Config.User}}' "$IMAGE_REF"
git rev-parse "$VERSION^{commit}"
```

## Monthly rebuild

The images carry the CA bundle of the Alpine stage in `deploy/images/*.Containerfile`, pulled fresh on every run.
Rebuild the current release and `main` once a month; the tags get a new digest, labels stay the same:

```sh
VERSION=v0.1.0
gh workflow run image.yaml --ref "$VERSION"
gh workflow run image.yaml --ref main
```
