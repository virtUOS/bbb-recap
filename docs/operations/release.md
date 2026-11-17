# Releases, published images and the plugin

For maintainers. Image rules: [deployment.md](../reference/deployment.md) §1; upgrade order: §8.

## What CI publishes

| Workflow                         | Trigger                                | Publishes                                                                     |
| -------------------------------- | -------------------------------------- | ----------------------------------------------------------------------------- |
| `.github/workflows/ci.yaml`      | every push and pull request            | nothing (`make lint`, `make test`, `make images`, hook tests)                 |
| `.github/workflows/image.yaml`   | push to `main`, a `v*` tag, manual run | `ghcr.io/virtuos/bbb-recap/recap`, `ghcr.io/virtuos/bbb-recap/recap-exporter` |
| `.github/workflows/release.yaml` | a `v*` tag                             | the GitHub release of the tag with the BBB plugin                             |

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

## The plugin release

For a tag `vX.Y.Z`, `release.yaml` builds `plugin/` (`npm ci`, `npm run build`, the Node.js line of the ci `plugin`
job) and attaches to the GitHub release of the tag:

| Asset                     | Content                                                   |
| ------------------------- | --------------------------------------------------------- |
| `RecapPlugin.js`          | the plugin script                                         |
| `manifest.json`           | the manifest, with the version from `plugin/package.json` |
| `recap-plugin-vX.Y.Z.zip` | both files, no directory                                  |
| `SHA256SUMS`              | SHA-256 of the three files above, for `sha256sum -c`      |

- It creates the release with the assets if there is none, with notes generated from the merged pull requests
  (`gh release create --generate-notes`). Edit the notes afterwards on the release page or with
  `gh release edit "$VERSION" --notes-file notes.md`; a re-run does not overwrite them.
- A tag with a pre-release suffix (`vX.Y.Z-<pre>`) creates a pre-release.
- Re-running the workflow for the same tag replaces the assets (`gh release upload --clobber`).
- **Set `version` in `plugin/package.json` to the release before tagging.** The manifest and the banner of
  `RecapPlugin.js` carry it, and BBB appends it to the script URL to bust caches. The workflow fails for `vX.Y.Z` if
  it differs and only warns for a pre-release.

Admins download the assets as in [install-plugin.md](install-plugin.md) step 1.

## Make the package pullable (once)

A new ghcr.io package is private. After the first image run, either make `bbb-recap/recap` and
`bbb-recap/recap-exporter` public (organization → Packages → package settings → change visibility), or log in on each
host with a `read:packages` token:

```sh
GHCR_USER=your-github-login
read -rs GHCR_TOKEN   # paste the token, then Enter
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io --username "$GHCR_USER" --password-stdin; unset GHCR_TOKEN
```

## Dependabot (once)

`.github/dependabot.yml` proposes weekly updates for Go modules, the base images, the compose images and the
actions. `.github/workflows/dependabot-auto-merge.yml` turns on auto-merge for minor and patch updates; majors wait for
a maintainer. Auto-merge is only safe with two repository settings:

- Settings → General → Pull Requests: **Allow auto-merge**. Without it the workflow fails and nothing merges.
- A branch protection rule (or ruleset) on `main` with **Require status checks to pass** and the `ci` jobs (`lint`,
  `fmt`, `test`, `build`, both `hook-test` entries) as required checks. Without required checks a queued pull request
  merges at once, before CI has run.
  The rule must not require approving reviews: the workflow does not approve, so such a pull request would wait
  forever.

A merge done by auto-merge may not start the `image` workflow on `main` (GitHub does not start workflows for events
of its own token). If `edge` should carry the update, run the `image` workflow by hand.

## Cut a release

With CI green on the commit, from a checkout with push rights:

```sh
VERSION=v0.2.0
git switch main && git pull --ff-only
if [ "v$(jq -r .version plugin/package.json)" = "$VERSION" ]; then
  git tag -a "$VERSION" -m "recap $VERSION" && git push origin "$VERSION"
else
  echo "plugin/package.json is not ${VERSION#v}: bump it in a pull request first"
fi
sleep 10
gh run watch "$(gh run list --workflow image.yaml --branch "$VERSION" --limit 1 --json databaseId --jq '.[0].databaseId')" --exit-status
gh run watch "$(gh run list --workflow release.yaml --branch "$VERSION" --limit 1 --json databaseId --jq '.[0].databaseId')" --exit-status
gh release view "$VERSION"   # expect the four assets of "The plugin release"
```

If `release.yaml` fails on the version check anyway, the tag and the images are already public: do not move the
tag; set `plugin/package.json` to the next patch version and release that.

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
