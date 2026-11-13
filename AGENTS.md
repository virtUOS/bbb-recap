# AGENTS.md

Guide for contributors, human or AI. Read [README.md](README.md) and [docs/overview.md](docs/overview.md) first.

## Commands

```sh
make build        # static binaries in bin/ (CGO_ENABLED=0)
make test         # unit tests; database tests need TEST_DATABASE_URL (a Postgres URL), else they skip
make lint         # gofmt, go vet, staticcheck, golangci-lint
make fmt-check    # prettier for Markdown, YAML, JSON (make fmt rewrites; needs Node.js for npx)
make contracts    # contract tests; skip unless RECAP_BIN etc. are set (contracts/README.md)
make hook-test    # Ruby hook tests (local ruby, else podman)
make images       # podman images localhost/recap:dev, recap-exporter, recap-mock
tools/e2e/m1.sh   # end-to-end with deploy/compose.yaml, the dev overlay tools/dev/ and mocks (after make images)
tools/e2e/m2.sh   # end-to-end including hook and exporter
cd plugin && npm ci && npm run lint && npm run typecheck && npm test && npm run build   # BBB plugin, dist/
```

CI (`.github/workflows/ci.yaml`) runs lint, the prettier check, test, images, the plugin checks and the hook tests.

## Layout

`cmd/` binaries, `internal/` code, `hook/` BBB hook (Ruby), `plugin/` BBB plugin (TypeScript, npm), `formats/` output formats, `contracts/` contract tests,
`testdata/` fixtures and goldens, `deploy/` images and the operator's compose files and config examples, `tools/` generators, the dev compose overlay
(`tools/dev/`) and test scripts, `docs/`.

## Conventions

- Go, formatted with `gofmt`; Markdown, YAML and JSON with prettier (`make fmt`); plain, readable code. Add machinery only when a documented requirement needs it.
- Images: static Go binaries on `scratch` plus CA certificates, non-root numeric UID.
- Configuration through `RECAP_*` environment variables; YAML only for nested config; secrets only from files.
- **No content in logs:** never log transcript, summary, titles, names or emails at INFO or above, and never URL
  query strings or raw provider error bodies.
- **Provider-agnostic core:** domain code never imports a vendor SDK; vendors live behind the adapter interfaces in
  `internal/adapters/`.
- **Contracts first:** interfaces are versioned schemas with fixtures and contract tests
  (`docs/reference/schemas/`, `contracts/`). Change the reference and the tests together with the code.
- **Migrations are checksummed:** never edit an applied migration in `internal/db/migrations/`, not even a
  comment; add a new one.
- Every job step must be safe to run twice. Fixtures are synthetic only (`docs/reference/fixtures.md`).
- Code cites the reference as `docs/reference/<file>.md §n`; keep section numbers stable.
- Commits: [Conventional Commits](https://www.conventionalcommits.org/) (`type(scope): description`).

## Where things are decided

- Exact rules: [docs/reference/](docs/reference/README.md). Upstream facts with sources:
  [docs/reference/upstream-facts.md](docs/reference/upstream-facts.md).
- Why: [docs/decisions/](docs/decisions/) (ADRs).
