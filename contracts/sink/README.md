# Contract tests: sink and identity

Black-box tests for `docs/reference/sinks.md` §4 (CT-SINK) and `docs/reference/identity-mapping.md` §9 (CT-ID). The test IDs are
the subtest names.

| Level          | Variables                                                                                                                                                                                                                   | Tests                                                                                                                                                                   |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| tool           | `RECAP_BIN`                                                                                                                                                                                                                 | sinks.md rows 01–05 and 07–11, identity row 01, the tool half of identity row 04                                                                                        |
| tool + service | as the rows above                                                                                                                                                                                                           | CT-SINK-12 (frontend sink): `tool` with `RECAP_BIN`, `service` with the service variables; `TestResultDocumentURL` (CT-REG-08, release half) with the service variables |
| service        | `RECAP_CONTRACT_BASE_URL` and `RECAP_CONTRACT_DOCS_ADDR` (`host:port`; the contract environment's sink profile `docs-contract` points at `http://<that address>`, `docs-main` stays on `recap-mock`; `contracts/README.md`) | sinks.md row 06 (admin `release`/`retry`), identity rows 02 and 03, the log half of identity row 04                                                                     |

Without its variables a test skips and names them. The service-level log scans read `RECAP_CONTRACT_LOG_FILE`
when set (`contracts/README.md`); without it only the scan is skipped.

## Mock Docs

Built from the request and response shapes cited in `sinks.md` §3 (`suitenumerique/docs @ 6c730f7`); `mock_test.go`.

- Serves only `POST /api/v1.0/documents/create-for-owner/`; anything else is 404.
- `Authorization: Bearer <testdata/secrets/docs-s2s.key>`, else 401.
- JSON body; `sub` and `email` required, `sub` at most 255 characters, else 400 with a field error.
- Owner matching on the users of `testdata/sink/docs-users.json` (`fixtures.md` §5): exact `sub`, else
  case-insensitive email. One user → document with an owner access; none → document with an owner invitation; two
  → 400 `{"email": [...]}` and no document.
- 201 `{"id": "<uuid>"}`.
- Faults queued per test: any status with a body and no document (429, 401, 403, 400 content, 404, 500, 503);
  create, then hang until the client gives up; create, then close the connection without an answer.
- The error message texts are the mock's own (Docs' real texts were not checked) and serve as log canaries. The
  `{"detail": …}` shape of the 401/403/429/5xx/404 bodies was not checked either; tests classify by status only.
- The service-level mock on `RECAP_CONTRACT_DOCS_ADDR` is shared by the tests of the package and closed in `TestMain`.

## `recap tool sink`

Dev/test only (`docs/reference/README.md` "Configuration, processes, metrics"). One deliver call (`sinks.md` §1.5): at most
one HTTP request.

### Environment

| Variable                              | Use                                                                                                                                                                                                                 |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `RECAP_SINKS_FILE`                    | sink profiles (`sinks.md` §3.1). Required                                                                                                                                                                           |
| `RECAP_LOG_LEVEL`                     | the tests set `debug`                                                                                                                                                                                               |
| `SSL_CERT_FILE`                       | CA bundle; the tests point it at the mock's certificate                                                                                                                                                             |
| `RECAP_BLOB_BACKEND`, `RECAP_FS_ROOT` | only for a `frontend` profile (CT-SINK-12): `fs` and a temp dir; the tool writes the summary to key `results/<job_id>/summary.md` (`architecture.md` §8: the fs backend keeps the key layout under `RECAP_FS_ROOT`) |

The tests start the tool with no other `RECAP_*` variable. The sinks file is `testdata/sink/sinks.yaml` with
`docs_base_url` set to the mock and the key path set to `testdata/secrets/`. Its top level is `schema_version` and a
list `sinks`; each entry has the keys of §3.1.

### Input (stdin, one JSON object)

```json
{
  "request": {
    "job_id": "019a2f8c-3b40-7000-8000-000000000009",
    "sink_profile": "docs-main",
    "title": "Fixture Widget Meeting 2025-10-29",
    "summary_md": "…",
    "summary_sha256": "<64 hex>",
    "owner": { "sub": "fixture-sub-ada-0001", "email": "ada.testperson@example.org" }
  },
  "delivery": null,
  "request_timeout_ms": 1000
}
```

| Field                | Meaning                                                                                                                                                     |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `request`            | the delivery request of §1.2                                                                                                                                |
| `delivery`           | the job's delivery record before the call (§1.3): `null` (none) or a record. A record in `sending` is the re-entry of §1.4: no request, outcome `uncertain` |
| `request_timeout_ms` | optional, dev/test only: replaces the profile's `request_timeout`, so a timeout can be tested quickly                                                       |

### Output (stdout, one JSON object), exit 0

```json
{
  "outcome": "ok",
  "held_reason": null,
  "failure_reason": null,
  "error": null,
  "delivery": {
    "job_id": "…",
    "sink_profile": "docs-main",
    "state": "sent",
    "document_id": "<id from Docs>",
    "sent_at": "2026-10-29T10:00:00Z"
  }
}
```

`outcome` is the step outcome of README "Adapter error classes → step outcome", `delivery` the record the step
commits after the call. `error` is `null` or `{"class": "…", "code": "<allow-listed code>"}`; `code` is not pinned.

| Docs answer                                              | `error.class`          | `outcome`   | `held_reason` / `failure_reason` | `delivery`                                                      |
| -------------------------------------------------------- | ---------------------- | ----------- | -------------------------------- | --------------------------------------------------------------- |
| 201                                                      | —                      | `ok`        | —                                | `sent`, `document_id` from the response, `sent_at` RFC 3339 UTC |
| connection refused, 429                                  | `not_sent`             | `transient` | —                                | `null` (cleared, `architecture.md` §6.4)                        |
| timeout, connection lost, 5xx; input record in `sending` | `uncertain`            | `hold`      | `delivery_uncertain`             | `sending`, no `document_id`                                     |
| 401, 403                                                 | `config`               | `permanent` | `config_error`                   | `null`                                                          |
| 400 with an `email` field error                          | `recipient_unresolved` | `hold`      | `owner_unresolved`               | `null`                                                          |
| other 4xx                                                | `rejected`             | `permanent` | `rejected_by_sink`               | `null`                                                          |
| a §2.4 pre-check violation (no request)                  | `rejected`             | `permanent` | `rejected_by_sink`               | `null` (§2.4: no record is written)                             |

Frontend sink (`type: frontend`, `sinks.md` §4a): no request to anything. A delivery that passes §2.4 stores
`summary_md` byte for byte and answers `ok`, error `null`, `delivery` `{state: sent, document_id: null or ""}` with
`sent_at`, also when the input record is in `sending` (no `uncertain` case); a repeated call overwrites the object. A §2.4
violation is `rejected` as for Docs and stores nothing.

`sinks.md` §1.4 item 3: `not_sent` and any 4xx other than 429 delete the record in the same transaction as the
outcome, and a later attempt sends again; `uncertain` keeps it in `sending`. A pre-check rejection runs before step 1 of §1.4 and
writes no record (§2.4). The service-level tests check the same through the job body (`delivery: null` while
held `owner_unresolved`). The output never contains the title, the content, the `sub` or the email.
Readers ignore fields they do not know.

Exit 1 on a runtime error outside the adapter, 2 on a usage or configuration error; then stdout is empty and
stderr says why. Logs go to stderr in the format of README "Logging" (one JSON object per line with `time`,
`level`, `msg`).

## What the tests pin

| ID                       | Level                | Pins                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Reference                         | Fixtures                                                                               |
| ------------------------ | -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- | -------------------------------------------------------------------------------------- |
| CT-SINK-01               | tool                 | one POST, exact path, bearer key, JSON body with exactly `title`, `content`, `sub`, `email` (unchanged); record `sent` with the mock's ID                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | §3.2, §1.3                        | `request.json`, `summary.md`, `docs-users.json`, `sinks.yaml`, `docs-s2s.key`          |
| CT-SINK-02               | tool                 | `sub` unknown + Ada's email → owner access; unknown owner → invitation; both 201 and the same record                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | §3.3                              | as 01                                                                                  |
| CT-SINK-03               | tool                 | refused → `not_sent`, 429 → `not_sent`, then 201; two requests, one document                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | §1.6                              | as 01                                                                                  |
| CT-SINK-04               | tool                 | hang after create (timeout), 500, 503, connection drop after create → `uncertain`, hold `delivery_uncertain`, record `sending`, exactly one POST                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | §1.4, §1.6                        | as 01                                                                                  |
| CT-SINK-05               | tool                 | input record `sending` → no POST, `uncertain`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | §1.4                              | as 01                                                                                  |
| CT-SINK-06               | service              | connection dropped after create → held `delivery_uncertain`, one POST, job `delivery` `{sending, null}`; `release` with `{"document_id": <the mock's ID>}` → 200 job body, `delivered`, `delivery` `{confirmed_by_admin, <ID>}`, no POST; then `retry` → 409 `invalid-job-state`. 500 on a second job → held; `retry` with `{}` → 200, `delivered`, exactly one new POST, one document, `delivery` `{sent, <ID>}`                                                                                                                                                                                                                                                                                                                                                           | §1.7; `architecture.md` §5.4      | F-01 raw dir and golden manifest, client secrets                                       |
| CT-SINK-07               | tool                 | wrong key (401), 403 → `config`; 400 content, 404 → `rejected`; record `null` (§1.4 item 3); one request, no document                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       | §1.6                              | as 01                                                                                  |
| CT-SINK-08               | tool                 | duplicate email → `recipient_unresolved`, hold `owner_unresolved`, record `null` (§1.4 item 3), one request, no document (the job-level resume after a registration `PUT` is CT-ID-02)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | §1.6, §3.3                        | as 01                                                                                  |
| CT-SINK-09               | tool                 | 2 MiB + 1 byte, each file of `forbidden/` (raw HTML block and inline, image, autolink, reference link, footnote, setext heading), a 201-character title → `rejected`, no request. Controls: `allowed.md` with a 200-character title is sent unchanged. **Not covered** from §2.2: definition lists, math and block content in table cells (no unambiguous Markdown form to detect)                                                                                                                                                                                                                                                                                                                                                                                          | §2.1–§2.4                         | `forbidden/*.md`, `allowed.md`                                                         |
| CT-SINK-10               | tool (+ service log) | all scenarios again at DEBUG; stderr of every tool run in the package (and the service log when set) has README log lines and no content line, title, email, `sub`, key or mock response text                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | §4; `privacy.md` §6               | as 01                                                                                  |
| CT-SINK-11               | tool                 | every `testdata/render/**/summary*.md` golden passes the test's §2 scan and is sent byte for byte. The y-provider `/convert` comparison is not run (no pinned y-provider; `contracts/render/README.md`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | §2, §4                            | render goldens                                                                         |
| CT-SINK-12               | tool + service       | frontend sink. Tool (`frontend-main` of `sinks-frontend.yaml`): `summary.md` stored at `results/<job_id>/summary.md` byte for byte, record `sent` without document ID; a second call (input record `sending`) with `allowed.md` overwrites it; 2 MiB + 1 byte and a raw HTML block → `rejected`, nothing stored; the mock Docs of `docs-main` sees no request; stderr has no content, title or identity. Service (`frontend-contract`): F-01 delivered to `docs-contract` and to `frontend-contract` gives the same `summary.md` apart from the footer line (`{generated_at}` is the job's `created_at`); no Docs request for the frontend job; job `delivery` `{sent, empty}`; registration `document_url` null, `summary_available` true; `GET …/summary` `text/markdown` | §4a; `metadata-interface.md` §2.7 | `sinks-frontend.yaml`, `summary.md`, `allowed.md`, `forbidden/raw-html-block.md`, F-01 |
| CT-REG-08 (release half) | service              | `delivery_uncertain` released with `{}` → `document_url` null; released with the document ID → `docs-contract`'s link with that ID                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | `metadata-interface.md` §2.7      | F-01                                                                                   |
| CT-ID-01                 | tool                 | `sub` and `email` sent exactly as given: Ada, a mixed-case email, a 255-character `sub`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | `identity-mapping.md` §2          | as 01                                                                                  |
| CT-ID-02                 | service              | unresolvable owner → held `owner_unresolved` after one POST, job `delivery` `null`, no document; registration `PUT` with Ada resumes the job at once; delivered with a second POST carrying Ada's `sub` and email                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | §4, §5                            | F-01, client secrets                                                                   |
| CT-ID-03                 | service              | held job `purge`d (200 with the job body) → `cleaned`/`purged`; registration `GET` 404; admin job, job list and audit answers have no owner, `sub` or email field or value                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | §3; `privacy.md` §5.1             | F-01, client secrets                                                                   |
| CT-ID-04                 | tool + service log   | tool stderr of the owner scenarios and the service log after CT-ID-02/03 hold no fixture `sub`, email or F-01 `externalUserId` (as a JSON string)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | §3, §9                            | as 01                                                                                  |

Admin actions send `{}` (or `{"document_id": …}` for `release` of `delivery_uncertain`) and expect 200 with the job
body; the job body's `delivery` field shows the record (`architecture.md` §4).

Known gap: CT-ID-03 checks "no owner data after cleanup" through the API only; there is
no direct scan of Postgres (no database access in the contract tests).

## Run

```sh
make contracts                                       # all skip
make build && RECAP_BIN=./bin/recap make contracts
RECAP_CONTRACT_BASE_URL=… RECAP_CONTRACT_DOCS_ADDR=127.0.0.1:18080 RECAP_CONTRACT_LOG_FILE=… make contracts
```
