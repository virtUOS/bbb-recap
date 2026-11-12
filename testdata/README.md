# testdata

Synthetic fixtures and goldens from `docs/reference/fixtures.md`. No real people, voices, secrets or meetings.
Contract tests compare against these files; change them only together with the contract they pin.
The F-01 `notes.html` is 350 bytes on purpose: `contracts/package/package_test.go` rewrites its `"size":350` manifest
entry, so keep that size when changing the file.

Regenerate with `make fixtures` (runs `tools/fixturegen/generate.sh`): `tools/fixturegen/fixturegen.py` writes
the raw dirs, outcome files, secrets and `generator/manifest.txt`; the independent reference scripts in
`tools/ref/` write the package goldens and invalid manifests, then check them. Output is byte-identical
across runs with the tool versions recorded in `generator/manifest.txt`.

## Layout

| Path | Content |
|---|---|
| `meetings/F-NN/README.md` | purpose, IDs, metadata and expected outcomes of the fixture |
| `meetings/F-NN/registration.json` | registration request body (F-01, F-04, F-06 only) |
| `meetings/F-NN/raw/<record_id>/` | raw recording dir as `archive.rb` leaves it (F-07 and F-09 use F-01's by reference) |
| `meetings/F-NN/expected/hook.json` | post_archive hook decision (below) |
| `meetings/F-NN/expected/exporter.json` | exporter end state (below) |
| `meetings/F-NN/expected/service.json` | service job outcome (below) |
| `meetings/F-NN/expected/manifest.json` | canonical (JCS) manifest, only where the exporter builds a package (F-01, F-04, F-06, F-07) |
| `meetings/F-NN/expected/package_id.txt` | SHA-256 of `manifest.json` bytes, hex, one line |
| `manifests/invalid/<M-ID>.<rule>.json` | manifests from F-01 that violate exactly `<rule>` (`tools/ref/manifest_rules.py`) |
| `secrets/` | fixture credentials, every value starts with `FIXTURE-NOT-A-SECRET-` |
| `generator/manifest.txt` | tool, version, license and command for every audio and video file |

Injected exporter inputs for the manifests: `bbb_server_id=bbb-fixture-a`, `created_at=2025-10-29T10:10:00Z`,
`exporter.version=0.0.0-fixture`, `meta_prefix=recap` (`tools/ref/make_goldens.py`).

## Synthetic values

All IDs and values are synthetic. They include `bbb_version="4.0.0"` (in `events.xml` and the golden manifests;
not checked against a packaged BBB 4.0, since `bigbluebutton.yml:1 @ 7d27e7e` still says `3.0.17`), `padId`
`fixturepad0001`, chat `messageId` `fixmsg-000N`, poll ID `fixpoll-0001`, the presentation ID
`<sha1("F-01-deck")>-1761732001000`, the `TR_fix…` track IDs (the real format was not researched), the user IDs
`w_fix…`, VOICE `bridge`/`recordingTimestamp` (F-02), and the record IDs `<sha1(fixture ID)>-1761732000000`.
Event shapes not checked against a real `events.xml` carry a generated `<!-- SYNTHETIC: shape … -->` comment in
`events.xml` (`docs/reference/fixtures.md` §8).

## Outcome files

`hook.json` (`bbb-exporter.md` §3.3): `decision` is `spool`, `not_opted_in`, `breakout_skipped` or `no_marks`;
`exit_code`; `no_marks` is logged at WARN, the others at INFO; `spool_entry` is `null` or the pinned fields of the spool entry (`spool_version`, `record_id`).
`hooked_at` and `hook_version` vary and are not pinned.

`exporter.json` (`bbb-exporter.md` §5): `spooled` (bool); `state` is the final exporter state (`acked`, `failed`)
or `null` when nothing was spooled; `reason` is the failure reason or `null`.

`service.json`: `input` is `exporter` (package pushed by the exporter), `direct` (package fed to the service
without the exporter) or `none`; `job` is `null` or `{state, reason}` with a README job state and a held or
failure reason, or `null`.

## Rules in invalid manifest names

`V-I01-non-canonical`, `V-I02-schema`, `V-I02-unsupported-schema-version`, `V-I04-paths-duplicate`,
`V-I04-paths-unsorted`, `V-I04-totals`, `V-I08-breakout` (rule IDs from `meeting-package.md` §7).

## Other directories

Not written by `make fixtures`; each has its own README or generator: `ingest/exchanges/`
(`make_exchanges.py`), `transcripts/` (`tools/ref/make_transcripts.py`), `providers/`, `render/` with the
`summary.md` and `transcript.vtt` goldens (`render/make_goldens.py`), `sink/`.
