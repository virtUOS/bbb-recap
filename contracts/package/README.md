# Contract tests: meeting package

Black-box tests for `docs/reference/meeting-package.md` (§9 lists them). They drive `recap tool validate-package` from
`RECAP_BIN`; the one stateful ingest rule (V-I06) runs against `RECAP_CONTRACT_BASE_URL`. Without the variables the
tests skip and name them.

## `recap tool validate-package`

Dev/test only (`docs/reference/README.md` "Configuration, processes, metrics"). Pure function: it reads only the files
named on the command line and below `<dir>`, never follows a manifest path out of `<dir>`, and writes nothing.

```
recap tool validate-package [--manifest FILE] [--package-id HEX] [--bbb-server-id ID] DIR
recap tool validate-package --jcs
```

| Form    | Input                                                                                                                              | Checks                                                                                                                                                                                                                                                                                     |
| ------- | ---------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| raw     | `DIR` = a raw recording directory `raw/<record_id>/` as `archive.rb` leaves it; no `--manifest`                                    | allow-list (§5) selects the package files; then V-S01 (well-formed, `<meeting id>` = directory name), V-S02, V-S03, V-S05. V-S04 and the `meeting_id` part of V-S01 need a manifest and are not checked                                                                                    |
| package | `--manifest FILE` = the manifest bytes; `DIR` holds the files at their manifest paths (a raw directory or a package directory, §2) | V-I01, V-I02, V-I03 (only with `--bbb-server-id`), V-I04, V-I07, V-I08 on the manifest; then V-I05 on the files; then V-S01…V-S05                                                                                                                                                          |
| jcs     | JSON text on stdin                                                                                                                 | none: writes the RFC 8785 (JCS) canonical form of the input to stdout, nothing else (no trailing newline). Integers, strings, booleans, null, arrays and objects only (`contracts/README.md`): input with a float (fraction or exponent) is rejected with a non-zero exit and empty stdout |

- `--package-id HEX`: the `package_id` from the ingest URL. With it, V-I01 also requires SHA-256 of the manifest
  bytes = `HEX`. Without it, V-I01 checks only that the bytes are JCS-canonical.
- `--bbb-server-id ID`: the server bound to the upload credential (V-I03).
- Not checked by the tool: V-I06 (needs ingest state), V-S06 and V-S07 (need configuration and registrations), the
  exporter rules V-E01…V-E04. The tool may report V-E03 limits as `413 package-too-large`; no test pins that here.
- Checks run in the order above and stop at the first violated rule. A package with no violation is `valid`.

### Output (raw and package forms)

One JSON object on stdout. Exit code 0 whenever a verdict was produced, valid or not. Exit 1 on a runtime error
(for example `DIR` unreadable), exit 2 on a usage error; then stdout is empty and stderr says why. Readers ignore
fields they do not know.

```json
{
  "valid": false,
  "package_id": "<64 hex>",
  "files": [{ "path": "events.xml", "role": "events", "size": 13410, "sha256": "<64 hex>" }],
  "gaps": [],
  "violation": { "rule": "V-I04", "problem": "manifest-invalid", "status": 422 }
}
```

| Field        | Meaning                                                                                                                                                                                                                                                                                                                                 |
| ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `valid`      | `true` iff `violation` is `null`                                                                                                                                                                                                                                                                                                        |
| `package_id` | package form: lowercase hex SHA-256 of the manifest file bytes, even if the manifest is invalid. Raw form: `null`                                                                                                                                                                                                                       |
| `files`      | raw form: the allow-list selection as manifest `files` entries (`path`, `role`, `size`, `sha256`), sorted by `path` bytewise (§4). Package form: not pinned                                                                                                                                                                             |
| `gaps`       | paths of microphone files without a matching `AudioTrackPublishedEvent` (V-S05: a single unmatched file becomes a gap; if none matches, violation `failed` `all_tracks_failed`). Always an array when `valid`                                                                                                                           |
| `violation`  | `null`, or the first violated rule. Ingest rules (`V-I*`): `{rule, problem, status}` with the problem slug (the `<slug>` of `urn:recap:problem:<slug>`) and HTTP status from §7 / `bbb-exporter.md` §6.2. Validate rules (`V-S*`): `{rule, job_state, reason}` with job state `failed` or `held` and the failure or held reason from §7 |

Ingest outcomes used by the tests: V-I01 `package-id-mismatch` 400; V-I02 `manifest-invalid` 422 or
`unsupported-schema-version` 422; V-I03 `server-mismatch` 403; V-I04 and V-I08 `manifest-invalid` 422; V-I05
`file-hash-mismatch` 422; V-I07 `not-opted-in` 422.

## Fixtures and mutations

Only `testdata/` (`docs/reference/fixtures.md`). Mutations M-01, M-09…M-13 are built at test time in a temporary
directory from F-01; mutated manifests are re-canonicalized by a small JCS helper in the tests (integers, strings,
booleans, objects, arrays only), which is checked against the golden manifest bytes before use.

The V-I06 test first rebuilds the unchanged F-01 manifest and checks it against the golden `package_id`, then `PUT`s
two manifests for one fresh `record_id` (F-01 with a unique suffix) with the
`bbb_server.bbb-fixture-a` fixture credential. It leaves one uncommitted staging package in the service, which the
service removes after `RECAP_INGEST_STAGING_TTL`.

## Run

```sh
make contracts                                            # all skip
make build && RECAP_BIN=./bin/recap make contracts        # tool tests
RECAP_CONTRACT_BASE_URL=https://… make contracts          # V-I06 over HTTP
```
