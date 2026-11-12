# testdata/sink

Synthetic inputs for the sink and identity contract tests (`contracts/sink/`). No real people or secrets.
Written by hand.

| File | Content |
|---|---|
| `sinks.yaml` | sink profile `docs-main` with the settings of `docs/reference/sinks.md` §3.1. Tests replace `docs_base_url` and the key directory |
| `request.json` | delivery request of §1.2 without `summary_md` (F-01 job, owner Ada, title by `templates.md` §5.1) |
| `summary.md` | a hand-written `summary.md` in the layout of `templates.md` §5.4 for F-01. Sink input only, not a render golden |
| `allowed.md` | every construct of `sinks.md` §2.1 plus the escapes of `templates.md` §5.2; must be sent unchanged |
| `forbidden/*.md` | one construct of `sinks.md` §2.2 each; appended to `summary.md`, each must be rejected before any call |
| `docs-users.json` | mock Docs users of `fixtures.md` §5: Ada, and two users sharing `dup.testperson@example.org` |

The Docs key is `testdata/secrets/docs-s2s.key`.
