# Contributing

Thank you for your interest in bbb-recap.

- **Issues first.** Open an issue before larger changes, so that the approach can be agreed before you write code.
  Small fixes can go straight to a pull request.
- **Tests.** `make lint`, `make fmt-check` and `make test` must pass. Changes to an interface update the reference in
  `docs/reference/`, its schema and the contract tests in `contracts/` together. Use only synthetic test data; never
  add recordings or data of real people.
- **Style.** Plain, readable Go formatted with `gofmt`. Follow the conventions in [AGENTS.md](AGENTS.md): no meeting
  content in logs, no vendor SDKs in the core, never edit an applied database migration.
- **Commits.** [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description`, for example
  `fix(exporter): retry after connection reset`.
- **License.** Contributions are licensed under the [Apache License 2.0](LICENSE).

Security problems: see [SECURITY.md](SECURITY.md), not the issue tracker.
