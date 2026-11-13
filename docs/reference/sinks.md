# Sinks: interface, Markdown dialect, Docs, front-end, webhook and Matrix sinks

Decisions: [ADR-005](../decisions/ADR-005-metadata-interface.md), [ADR-006](../decisions/ADR-006-identity-owner-only.md),
[ADR-007](../decisions/ADR-007-docs-sink-bot-user.md), [ADR-009](../decisions/ADR-009-privacy-retention.md),
[ADR-010](../decisions/ADR-010-postgres-job-queue.md), [ADR-014](../decisions/ADR-014-results-in-frontend.md),
[ADR-016](../decisions/ADR-016-multiple-sinks-webhook-matrix.md). Names,
states, error classes and logging rules are defined in [`README.md`](README.md) and not repeated here.

Facts about La Suite Docs come from reading its source at https://github.com/suitenumerique/docs @ `6c730f7` (v6.0.x).
Below, `D` stands for `src/backend/` in that repository. The facts are collected in
[`upstream-facts.md`](upstream-facts.md). §5 lists what to check against a real Docs instance. Matrix facts come from the
client-server API v1.19 (https://spec.matrix.org/v1.19/client-server-api/), webhook facts from Standard Webhooks
(https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md @ `81ceb60`).

## 1. Generic sink interface

### 1.1 Position

The `deliver` step calls the sink profiles of the job, named by `meta_recap-sink` (ADR-005, a list of 1–4 names since
ADR-016, `metadata-interface.md` §1.2). A sink is an adapter that speaks HTTP directly; it uses no vendor SDK. It
delivers `summary.md` or a notice about it. `transcript.vtt` is rendered but not delivered (README "Outputs").

Delivery order is fixed by type, not by the list: group 1 is `frontend` and `docs`, group 2 is `webhook` and `matrix`;
within a group, list order. Group 2 starts only when every sink of group 1 is done (§1.8), so a notice can carry the
links of group 1. This order is the "delivery order" used everywhere below.

### 1.2 Input: delivery request

| Field                      | Notes                                                                                                                                                                                              |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `job_id`                   | UUIDv7 (README).                                                                                                                                                                                   |
| `sink_profile`             | Profile name.                                                                                                                                                                                      |
| `title`                    | Plain text, one line, rules in §2.3. Built by `render`.                                                                                                                                            |
| `summary_md`               | `summary.md` from `render`, UTF-8, dialect of §2.                                                                                                                                                  |
| `summary_sha256`           | Lowercase hex of the bytes of `summary_md`.                                                                                                                                                        |
| `owner.sub`, `owner.email` | From the registration (ADR-006, `identity-mapping.md`).                                                                                                                                            |
| `links`                    | Sinks of the same job that are done and have a `document_url` (`metadata-interface.md` §2.7), in delivery order: `[{sink_profile, document_url}]`; `[]` when none. Used by `webhook` and `matrix`. |
| `matrix_room`              | The job's Matrix room (`metadata-interface.md` §1.2), stored by `validate`; `null` unless the job lists a `matrix` profile.                                                                        |

`docs` uses only the first six fields; `webhook` and `matrix` never use `owner`.

The request lives only in the service (step input and blob store) and is deleted by `cleanup` (ADR-009). It is never
logged.

### 1.3 Output: delivery record

| Field                    | Notes                                                                                                                                                                                              |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `job_id`, `sink_profile` |                                                                                                                                                                                                    |
| `state`                  | `sending` (committed **before** the request, §1.4), `sent` (success received), `confirmed_by_admin` (`release` after `delivery_uncertain`), `skipped_by_admin` (`release` of a failed sink, §1.8). |
| `document_id`            | Docs document ID from the response (`{"id"}`), or the Matrix event ID (§4c). Empty for `frontend` and `webhook`, and if an admin released the sink without it.                                     |
| `sent_at`                | RFC 3339 UTC.                                                                                                                                                                                      |

This is the `deliveries` row (`architecture.md` §6.1), one per `(job_id, sink_profile)`. The link for humans is built from `document_url_template`
(§3.1) when shown; it is not stored. The record holds no content, title, email or `sub`. It is kept as job metadata
(`privacy.md` §3.2, J1).

### 1.4 Idempotency

`create-for-owner` takes no client-supplied ID, and a repeated request creates a second document
(`D/core/api/serializers.py:458-472,502-507`). So the sink never sends twice on its own:

Per sink of the job:

1. Commit `state = sending` in Postgres, then send.
2. Success (Docs: 201) → commit `sent` with `document_id`.
3. On re-entry with `state = sending` (worker crash, lease lost) the sink does not send. It returns `uncertain`.
   `not_sent` and any 4xx other than 429 (Docs answered, so no document was created) delete the record in the same
   transaction as the outcome; a later attempt (backoff, registration `PUT`, admin `release`/`retry`) sends again.
4. A new document (or post) is created only by an explicit admin action: `retry` on a `delivery_uncertain` job (for one
   sink with `sink`, §1.8), or `rerun` (new `job_id`, README "Identifiers").
5. A sink in `sent`, `confirmed_by_admin` or `skipped_by_admin` is never called again for the job.

### 1.5 Operations

| Operation                            | Contract                                                                                                                                                                                                                            |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `deliver(request) → record \| error` | At most one delivering HTTP request per call (before it the Matrix sink makes up to three lookups and a join, §4c.2). Safe to call again (§1.4).                                                                                    |
| `check_config()`                     | Static checks at start-up (`recap check-config`): URL, key file readable, declaration complete when present (`privacy.md` §7). No call to Docs: the key works only on the create endpoint, so a live check would create a document. |

### 1.6 Error classes

Per README "Adapter error classes → step outcome". Raw error bodies are read only to pick the class and are never
logged. The table is the Docs sink's; §4b and §4c have the tables of the webhook and Matrix sinks. With several sinks
the step outcome combines the sinks' outcomes (§1.8).

| Class                  | Docs responses                                                                                               | Outcome                                                                                 |
| ---------------------- | ------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| `not_sent`             | connection refused or DNS failure before sending; HTTP 429 (throttle, `D/core/api/viewsets.py:583`)          | transient: retry with backoff (`architecture.md` §5.3); exhausted → `retries_exhausted` |
| `uncertain`            | timeout, connection lost after sending, any 5xx, re-entry in `sending`                                       | hold `delivery_uncertain`; **never retried automatically**                              |
| `config`               | 401, 403 (key missing or wrong)                                                                              | permanent, job `failed` (`config_error`)                                                |
| `recipient_unresolved` | 400 with an `email` field error (two active Docs users share the email, `D/core/api/serializers.py:485-486`) | hold `owner_unresolved`                                                                 |
| `rejected`             | any other 4xx, e.g. 400 `{"content": ["Could not convert content"]}`                                         | permanent, job `failed` (`rejected_by_sink`)                                            |

A 400 means that no document exists: conversion runs before the document is created, and a storage failure rolls the
transaction back (`D/core/api/serializers.py:492-531`). So an admin `retry` after a `rejected` is safe. Both 401 and
403 map to `config`, because which of the two Docs returns for a wrong key was not checked.

### 1.7 Admin handling of `delivery_uncertain`

The admin checks in Docs (with the owner or the Docs operator) whether the document exists:

- found → `release`: the record becomes `confirmed_by_admin`, the job is marked `delivered` and continues to `cleanup`;
- not found → `retry`: the sink sends one new request.

The alert carries `job_id` and the time of the attempt, never the title or email. Each request counts in
`recap_external_requests_total{kind="sink"}` (`architecture.md` §12). With several sinks the admin names the sink
(§1.8); the same applies to a webhook receiver or a Matrix room.

### 1.8 Several sinks per job

ADR-016. `deliver` is one step with one item per sink (`step_items`, item = profile name, as `transcribe` has one per
track). Each sink has a **status**, shown in the admin job body (`architecture.md` §4) and the registration's
`job.deliveries` (`metadata-interface.md` §2.7):

| Status               | Meaning                                                                                                   |
| -------------------- | --------------------------------------------------------------------------------------------------------- |
| `pending`            | not sent yet: not attempted, waiting for a retry after `not_sent`, or after `recipient_unresolved`        |
| `sending`            | record in `sending` without a result: outcome unknown (`uncertain`)                                       |
| `sent`               | record `sent`                                                                                             |
| `confirmed_by_admin` | record `confirmed_by_admin`                                                                               |
| `skipped_by_admin`   | record `skipped_by_admin`: an admin gave up this sink; nothing was delivered there                        |
| `failed`             | the last attempt ended `config` or `rejected` (no record, §1.4 item 3); the item keeps the failure reason |

`sent`, `confirmed_by_admin` and `skipped_by_admin` are **done**.

**One attempt** of the step calls every `pending` sink of group 1 (§1.1), in delivery order, each at most once (one
`deliver` call per sink, §1.5) and each under its own `max_inflight` slot; when group 1 is then done, the same attempt
goes on with the `pending` sinks of group 2. A `failed` or `sending` sink is
not called again by an attempt; only an admin action resets it. Then the step outcome is, from the first rule that
applies:

1. a sink is `failed` → `permanent`, job `failed` with the failure reason of the first failed sink in delivery order
   (`config_error` or `rejected_by_sink`);
2. a sink is `sending` → hold `delivery_uncertain`;
3. a sink returned `recipient_unresolved` → hold `owner_unresolved` (a registration `PUT` resumes the job, as for one
   sink);
4. a sink is `pending` (a `not_sent` result; group 2 waits for group 1) → `transient`;
5. every sink done → `ok`, job `delivered`, `delivered_at` set, `cleanup` scheduled.

The attempts and backoff are those of the step (`architecture.md` §5.3: 10 for `deliver`); `retries_exhausted` fails
the job and leaves the sinks' statuses as they are.

**Admin actions** (`architecture.md` §5.4) take an optional `"sink": "<profile>"`. A name that is not a sink of the job
is `400 invalid-request`; a sink in a status the action does not accept is `409 invalid-job-state`.

| Action                | Job                                                 | Sink status           | Effect                                                                                                               |
| --------------------- | --------------------------------------------------- | --------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `retry` with `sink`   | `failed` in `deliver`, or held `delivery_uncertain` | `failed` or `sending` | the sink becomes `pending` (a `sending` record is deleted); the step runs again                                      |
| `retry`               | as above                                            | —                     | every `failed` and `sending` sink becomes `pending`; as before for one sink                                          |
| `release` with `sink` | held `delivery_uncertain`                           | `sending`             | `confirmed_by_admin`, optional `document_id`                                                                         |
| `release` with `sink` | `failed` in `deliver`                               | `failed`              | `skipped_by_admin`. Refused (`409`) when every other sink of the job is already skipped: use `purge`                 |
| `release`             | held `delivery_uncertain`                           | —                     | every `sending` sink becomes `confirmed_by_admin`; `document_id` only when exactly one sink is `sending`, else `400` |
| `release`             | `failed`                                            | —                     | `409 invalid-job-state`, as before                                                                                   |

After an action the job state follows from the statuses with the rules above, without calling a sink: every sink done
→ `delivered` (`cleanup` scheduled); a sink `failed` → `failed`; a sink `sending` → held `delivery_uncertain`; else the
step runs again (`running`). A job is `delivered` only when every sink is done; no outcome value is added.

## 2. Markdown dialect

### 2.1 Allowed

Docs converts Markdown with BlockNote 0.54's own parser in its y-provider service
(`src/frontend/servers/y-provider/src/handlers/convertHandler.ts:88-100 @ 6c730f7`; parser
https://github.com/TypeCellOS/BlockNote `packages/core/src/api/parsers/markdown/markdownToHtml.ts @ v0.54.0`).
Allowed is only what that parser handles: ATX headings `#`–`######` (templates use 1–3); paragraphs and hard breaks;
`**bold**`, `*italic*`, `~~strike~~`, `` `code` ``, backslash escapes; inline links `[text](https://…)`; bullet,
ordered and task lists, nested by indent; GFM pipe tables with inline content only; fenced code; blockquote `>`;
horizontal rule `---`; timestamps `[hh:mm:ss]` as plain text (no `](` follows, so no link).

### 2.2 Forbidden

Raw HTML (the result is unclear, and it is an injection surface for LLM output); reference links and autolinks `<…>`
(the parser has no handling for them); footnotes, definition lists, math (they become plain text); images (external
`src`); block content in table cells (probably lost); setext headings.

### 2.3 Title

- Title = meeting name + date (ADR-007), built by `render` (rule in `templates.md` §5.1).
- Plain text, one line, NFC, no control characters. At most 200 characters; `render` shortens the meeting name to fit.
  Docs stores at most 255 (`D/core/models.py:1026`); the server-to-server serializer does not check the length
  (`D/core/api/serializers.py:459`).
- The title appears in the emails that Docs sends to the owner. See `privacy.md` §9.

### 2.4 Size and checks

| Limit                          | Value                                             | Source                                                                             |
| ------------------------------ | ------------------------------------------------- | ---------------------------------------------------------------------------------- |
| Docs request body              | 20 MB (`DATA_UPLOAD_MAX_MEMORY_SIZE`)             | `D/impress/settings.py:221`                                                        |
| y-provider body                | 20 971 520 bytes (`CONVERSION_FILE_MAX_SIZE`)     | `src/frontend/servers/y-provider/src/servers/appServer.ts:24-29`, `src/env.ts:7-9` |
| Conversion timeout             | 30 s (`CONVERSION_API_TIMEOUT`)                   | `D/impress/settings.py:1072-1090`                                                  |
| bbb-recap `max_document_bytes` | 2 MiB default (sink profile), checked by `render` | this document                                                                      |

Before sending (before step 1 of §1.4), the sink checks UTF-8, the size and scans for §2.2 constructs. A violation is
`rejected` without a call, and no delivery record is written.

## 3. Docs sink (`type: docs`)

### 3.1 Sink profile (entry in `RECAP_SINKS_FILE`)

The file has `schema_version` (`1.x.y`) and a `sinks:` list of profiles. Relative file paths in a profile (e.g. the
key file) are resolved against the sinks file's directory.

| Setting                 | Meaning                                                                                                                            |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `name`                  | README name grammar; a name in `meta_recap-sink`. Fixture: `docs-main`.                                                            |
| `type`                  | `docs`.                                                                                                                            |
| `docs_base_url`         | Base URL of Docs.                                                                                                                  |
| `api_key_file`          | Mounted file with the server-to-server key.                                                                                        |
| `document_url_template` | Link for humans, e.g. `https://docs.recap.test/docs/{id}/`. The pattern `/docs/{id}/` opened the document in a test with Docs 6.0. |
| `request_timeout`       | Default 60 s (above Docs' 30 s conversion timeout). Expiry → `uncertain`.                                                          |
| `max_inflight`          | Concurrent requests per worker process (`architecture.md` §7). Default 2.                                                          |
| `max_document_bytes`    | §2.4.                                                                                                                              |
| `declaration`           | Privacy declaration (`privacy.md` §7). Optional; complete when present.                                                            |

### 3.2 Request

`POST <docs_base_url>/api/v1.0/documents/create-for-owner/` (`D/core/api/viewsets.py:972-995`).

- Header `Authorization: Bearer <key>`. Docs checks the key against `SERVER_TO_SERVER_API_TOKENS`, environment
  variable `DJANGO_SERVER_TO_SERVER_API_TOKENS` (`D/core/authentication/__init__.py:39-52`, `D/impress/settings.py:86`).
  The key is not scoped: it can create documents for any user. Treat it as a production secret.
- JSON body. Docs accepts these fields (`D/core/api/serializers.py:458-472`):

| Field                     | Sent | Note                                                                                  |
| ------------------------- | ---- | ------------------------------------------------------------------------------------- |
| `title`                   | yes  | §2.3                                                                                  |
| `content`                 | yes  | `summary.md`                                                                          |
| `sub`                     | yes  | owner `sub`; required by Docs, at most 255 characters                                 |
| `email`                   | yes  | owner email; required by Docs                                                         |
| `language`                | no   | Docs uses the stored language of a matched user (`D/core/api/serializers.py:488-490`) |
| `send_notification_email` | no   | Docs default `true`: Docs emails the owner                                            |
| `message`, `subject`      | no   | would override Docs' email text; not needed                                           |

- Response 201 `{"id": "<uuid>"}`. The document is a root document with link reach `restricted` and one `owner`
  access or one owner invitation (`D/core/models.py:1028-1032`, `D/core/api/serializers.py:502-522`).

### 3.3 How Docs finds the owner

From `D/core/models.py:118-143` and `D/core/api/serializers.py:485-522`:

1. Exact `sub` match.
2. Else, if `OIDC_FALLBACK_TO_EMAIL_FOR_IDENTIFICATION=true` (the default, `D/impress/settings.py:795-799`), a
   case-insensitive email match. Two active users with that email → 400 → `recipient_unresolved`.
3. Else Docs creates an owner **invitation** for the email and sends an invitation email. It is valid for 7 days
   (`INVITATION_VALIDITY_DURATION`, `D/impress/settings.py:548`). At first login the user becomes the owner. If nobody
   logs in in time, the document has no owner, and the server-to-server API cannot fix that
   (`D/core/models.py:399-425`).

The 201 response does not say which case happened. The service cannot tell "owned" from "invited", and it has no
rights on the document afterwards (ADR-007).

### 3.4 Operator prerequisites

- [ ] Docs: one key for this service in `DJANGO_SERVER_TO_SERVER_API_TOKENS`. The setting is a list, so a second key
      allows rotation.
- [ ] Docs: `OIDC_FALLBACK_TO_EMAIL_FOR_IDENTIFICATION` left at `true`, unless every owner `sub` is known to match.
- [ ] Identity provider: the front end and Docs use the same OIDC realm, so the registered `sub` is the one Docs
      stores (ADR-007).
- [ ] Network: the worker can reach `docs_base_url` over TLS.
- [ ] Service: sink profile set, key file mounted, `recap check-config` passes.

## 4. Contract tests (mock Docs)

The mock (`recap-mock`) serves only `create-for-owner` as described in §3: bearer key check, the eight body fields,
201 `{"id"}`, sub-then-email matching on fixture users, invitation for unknown emails, duplicate-email 400, 429, and
fault injection (drop the response after creating, 500, delay). Fixtures only (`fixtures.md`). These tests were
written before the implementation and are not changed to fit it.

| ID         | Scenario                                                                                                                                   | Expected                                                                                                                                    |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-SINK-01 | Known owner (sub match)                                                                                                                    | one POST, exact header and fields of §3.2, record `sent` with document ID                                                                   |
| CT-SINK-02 | Owner known only by email; unknown owner                                                                                                   | 201 both; mock shows owner access / invitation; record identical                                                                            |
| CT-SINK-03 | Connection refused, then 429, then 201                                                                                                     | `not_sent` twice, one document in the mock                                                                                                  |
| CT-SINK-04 | Timeout after create; 500; connection drop                                                                                                 | `uncertain`, job held `delivery_uncertain`, no second POST                                                                                  |
| CT-SINK-05 | Re-entry with `state = sending`                                                                                                            | no POST, `uncertain`                                                                                                                        |
| CT-SINK-06 | Admin `release` / `retry` on `delivery_uncertain`                                                                                          | record `confirmed_by_admin`, job `delivered` / exactly one new POST                                                                         |
| CT-SINK-07 | 401, 403, other 400                                                                                                                        | `config` / `config` / `rejected`, no retry                                                                                                  |
| CT-SINK-08 | Duplicate email in mock                                                                                                                    | held `owner_unresolved`; after a registration `PUT` the job resumes                                                                         |
| CT-SINK-09 | Oversized document; forbidden construct; title over 200 characters                                                                         | `rejected` before any call                                                                                                                  |
| CT-SINK-10 | Log capture of all tests at DEBUG                                                                                                          | no Markdown, title, email, `sub`, key or response body                                                                                      |
| CT-SINK-11 | Golden: the built-in format rendered with fixtures passes the §2 check, and a pinned y-provider `/convert` gives the stored BlockNote JSON | equal; drift → human review                                                                                                                 |
| CT-SINK-13 | Two sinks `docs-contract,frontend-contract`, both succeed                                                                                  | one POST; job `delivered`; two delivery records `sent`; registration `deliveries` in delivery order (§1.8)                                  |
| CT-SINK-14 | Docs answers 400 content, the frontend sink succeeds; then admin `release` without and with `sink`                                         | job `failed` `rejected_by_sink`, statuses `failed`/`sent`; `release` `{}` → 409; with `sink` → `skipped_by_admin`, `delivered`, no new POST |
| CT-SINK-15 | Docs answers 500 (uncertain), the frontend sink succeeds; admin `retry` with `sink`                                                        | held `delivery_uncertain`, statuses `sending`/`sent`; `retry` → exactly one new POST, `delivered`; the frontend sink is not delivered again |
| CT-SINK-16 | Docs answers 429, then 201                                                                                                                 | `not_sent` once, then `delivered`; one document; the `sent` frontend sink is not called again                                               |
| CT-SINK-17 | Admin actions with `sink` on a failed one-sink job: a name not in the job; `release` with `sink` of the only sink; `sink` on `purge`       | 400 / 409 / 400; the job stays `failed`                                                                                                     |
| CT-SINK-18 | `docs-contract,webhook-contract,matrix-contract` (needs the webhook and Matrix contract profiles)                                          | Docs first; the webhook body and the Matrix notice carry the Docs link; three records; order independent of the list order                  |

## 4a. Front-end sink (`type: frontend`, ADR-014)

The summary is kept for the front end that registered the meeting, which fetches and shows it
(`metadata-interface.md` §2.7). No outside system, no owner lookup.

- Profile fields: `name`, `type: frontend`, `max_document_bytes` (§2.4), `declaration`. No URL, key or timeout.
- `deliver`: runs the §2.4 checks, writes `summary.md` to the blob store under `results/<job_id>/summary.md` (outside
  the job prefix that `cleanup` deletes) and records `state = sent` with an empty `document_id`, in one transaction
  with the step outcome. Repeating it overwrites the same object: idempotent, no `uncertain` case.
- The owner `sub` and email are not used; the registration still needs them (`validate` and the join are unchanged).
- Retention: until the front end's `DELETE …/summary`, or `RECAP_RESULT_TTL` after delivery; admin `erase` deletes
  it. `recap check-config` fails a `frontend` profile when `RECAP_RESULT_TTL` is `0`.
- Contract tests: CT-SINK-12 (deliver stores the exact bytes, a second deliver overwrites, no network call, size
  limit → `rejected_by_sink`); CT-REG-09 for the API.

## 4b. Webhook sink (`type: webhook`, ADR-016)

One signed HTTP POST per job to a URL fixed in the profile, for programs that react to a finished summary (LMS,
automation). Signing follows Standard Webhooks (HMAC-SHA256, the `v1` scheme). No owner lookup and no owner data.

### 4b.1 Profile

| Setting               | Meaning                                                                                                                                                                 |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`, `type`        | README name grammar; `webhook`.                                                                                                                                         |
| `url`                 | Absolute URL of the receiver, fixed here; never taken from meta or the registration. Logs name the profile, never the URL.                                              |
| `signing_secret_file` | One or two lines `whsec_<base64>`, each 24–64 random bytes after decoding (Standard Webhooks "Signature scheme"). Two lines rotate the key: both sign, the first first. |
| `content`             | `notice` (default): title and links. `summary`: also `summary.md`.                                                                                                      |
| `request_timeout`     | Default 30 s (the spec recommends 15–30 s). Expiry → `uncertain`.                                                                                                       |
| `max_inflight`        | Default 2 (`architecture.md` §7).                                                                                                                                       |
| `max_document_bytes`  | §2.4, default 2 MiB; applies with `content: summary`.                                                                                                                   |
| `declaration`         | **Required** (`privacy.md` §7, §11).                                                                                                                                    |

### 4b.2 Request

`POST <url>`, `Content-Type: application/json`, compact UTF-8 JSON, no `Authorization` header, redirects not followed.

| Header              | Value                                                                                                                                                              |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `webhook-id`        | `<job_id>:<sink_profile>`. The same for every attempt of the job and sink, including an admin `retry`, so a receiver can drop repeats. Contains no `.`.            |
| `webhook-timestamp` | Unix time of this attempt in seconds.                                                                                                                              |
| `webhook-signature` | `v1,<base64 of HMAC-SHA256(key, "<webhook-id>.<webhook-timestamp>.<body>")>`, one per secret line, separated by a space; `key` = the decoded bytes after `whsec_`. |

Body:

```json
{
  "type": "recap.summary.delivered",
  "timestamp": "2025-10-29T10:20:00Z",
  "data": {
    "job_id": "019a2f8c-3b40-7000-8000-000000000009",
    "sink_profile": "webhook-main",
    "title": "Fixture Widget Meeting 2025-10-29",
    "links": [
      {
        "sink_profile": "docs-main",
        "document_url": "https://docs.recap.test/docs/0b8f2a3e-5c41-4d7a-9e26-3f1c7a9d2b10/"
      }
    ],
    "content": "notice",
    "summary_md": null,
    "summary_sha256": null
  }
}
```

`timestamp` is the time of the attempt (RFC 3339 UTC); `links` is the request's `links` (§1.2); `summary_md` and
`summary_sha256` are set only with `content: summary`. No owner `sub` or email, no `record_id` or `meeting_id`
(ADR-013): a front end matches the `job_id` of its registration (`metadata-interface.md` §2.3). Receivers ignore
fields they do not know; new fields are only added.

Any 2xx is success: record `sent` with an empty `document_id`.

### 4b.3 Error classes

| Class       | Webhook responses                                                                                     |
| ----------- | ----------------------------------------------------------------------------------------------------- |
| `not_sent`  | connection refused or DNS failure before sending; 429 (`Retry-After` honoured, at most 30 min)        |
| `uncertain` | timeout, connection lost after sending, any 5xx, re-entry in `sending`                                |
| `config`    | any 3xx (the URL must be fixed in the profile), 401, 403, 404, 410 (Standard Webhooks: endpoint gone) |
| `rejected`  | any other 4xx; a §2.4 violation with `content: summary` (no request)                                  |

No `recipient_unresolved`. Outcomes as in §1.6. `uncertain` holds the job as for Docs: the receiver may have acted.

### 4b.4 `check_config`

`url` absolute `https://` (or `http://`, README "HTTP"), without user info; every secret line valid; `content` known;
`declaration` present and complete. No call to the receiver.

### 4b.5 Contract tests (mock receiver)

The test serves a receiver over HTTPS that records each request and answers per test (any status, hang, drop). Fixture
secret `testdata/secrets/webhook-signing.key` (`fixtures.md` §5).

| ID         | Scenario                                                        | Expected                                                                                                                                          |
| ---------- | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-SINK-20 | `content: notice`, one link in the request                      | one POST to the profile URL; the three headers; signature valid; body fields exactly as §4b.2, no owner value; record `sent`, empty `document_id` |
| CT-SINK-21 | `content: summary`; two secret lines                            | `summary_md` byte for byte with its SHA-256; two signatures, each valid for its secret                                                            |
| CT-SINK-22 | refused, 429; 500, 503, hang; 301, 401, 403, 404, 410; 400, 422 | `not_sent`; `uncertain` (record `sending`); `config`; `rejected`; one request each, the redirect target never called                              |
| CT-SINK-23 | input record `sending`; a `not_sent` attempt then a 2xx         | no request, `uncertain`; the same `webhook-id` on both requests                                                                                   |
| CT-SINK-24 | profile without `declaration`; log scan of all runs             | tool exit 2; no URL, secret, title or summary text in stderr                                                                                      |

## 4c. Matrix sink (`type: matrix`, ADR-016)

Posts one message to a Matrix room as a bot user through the client-server API
(https://spec.matrix.org/v1.19/client-server-api/). The room is the job's `matrix_room` (§1.2), chosen by the room
owner (`metadata-interface.md` §1.2). No owner lookup and no owner data.

### 4c.1 Profile

| Setting              | Meaning                                                                                                                                                                              |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `name`, `type`       | README name grammar; `matrix`.                                                                                                                                                       |
| `homeserver_url`     | Base URL of the bot's homeserver for the client-server API, e.g. `https://matrix.example.org`.                                                                                       |
| `access_token_file`  | The bot account's access token, sent as `Authorization: Bearer` (spec "Using access tokens"). Keep it for the life of the profile: transaction IDs are scoped to its device (§4c.4). |
| `allowed_servers`    | Optional list of server names. When set, the room alias's server or the room ID's server must be listed (`metadata-interface.md` §3 row 6b; checked again before any call).          |
| `content`            | `notice` (default): title and links. `summary`: also `summary.md`.                                                                                                                   |
| `request_timeout`    | Default 30 s, per request. Expiry of the send request → `uncertain`.                                                                                                                 |
| `max_inflight`       | Default 2.                                                                                                                                                                           |
| `max_document_bytes` | Bytes of the event's `body`; default and maximum 30 000 (an event is at most 65 536 bytes including envelope and signatures, spec "Size limits").                                    |
| `declaration`        | **Required** (`privacy.md` §7, §11).                                                                                                                                                 |

### 4c.2 Requests

One `deliver` call makes up to five requests; only the last one posts. Path parameters are percent-encoded.

1. Pre-checks, no request: the room matches `room` and `allowed_servers` (§4c.1); the body fits `max_document_bytes`;
   with `content: summary` the §2.4 checks. A violation is `rejected`.
2. Alias only: `GET /_matrix/client/v3/directory/room/{roomAlias}` → `room_id`.
3. `GET /_matrix/client/v3/joined_rooms`. If `room_id` is listed, go to 5.
4. Pending invitation: `GET /_matrix/client/v3/sync?timeout=0&filter=<filter>`, with an inline filter that limits the
   response to this room (`room.rooms`) and asks for no presence or account data. If `rooms.invite` has `room_id`:
   `POST /_matrix/client/v3/rooms/{roomId}/join` with `{}`. Else `rejected` (code `matrix-not-invited`): the bot never
   joins a room it was not invited to, also not a public one. The sync response is discarded, never stored or logged.
5. Commit `sending` (§1.4), then `PUT /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}` with
   `txnId = recap-<job_id>-<sink_profile>`. 200 `{"event_id"}` → record `sent`, `document_id` = the event ID.

Event content (`m.notice`, the message type for automated clients, spec "m.notice"):

```json
{
  "msgtype": "m.notice",
  "body": "Fixture Widget Meeting 2025-10-29\nhttps://docs.recap.test/docs/0b8f2a3e-5c41-4d7a-9e26-3f1c7a9d2b10/"
}
```

`body` = the title, then each `document_url` of `links` on its own line, joined with LF and without a final LF; with
`content: summary` then two LF and `summary.md` unchanged. No `format` or `formatted_body`: the service generates no HTML from
meeting content; clients show the Markdown as text. The bot stays in the room; room moderators can remove it.

### 4c.3 Error classes

Requests 2–4 post nothing, so a failure there is never `uncertain`. Classified by HTTP status; `Retry-After` (or the
deprecated `retry_after_ms`) is honoured on 429.

| Class       | Requests 2–4                                                                | Request 5 (send)                                                                            |
| ----------- | --------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| `not_sent`  | connection refused, DNS failure, timeout, connection lost, 5xx, 429         | connection refused or DNS failure before sending, 429                                       |
| `uncertain` | —                                                                           | timeout, connection lost after sending, any 5xx, re-entry in `sending`                      |
| `config`    | 401 (`M_UNKNOWN_TOKEN`, `M_MISSING_TOKEN`)                                  | 401                                                                                         |
| `rejected`  | 404 on the alias (`M_NOT_FOUND`), 403 on join, no invitation, any other 4xx | 403 (`M_FORBIDDEN`: not a member, or no power to post), 413 or `M_TOO_LARGE`, any other 4xx |

No `recipient_unresolved`. A `rejected` room is the room owner's to fix (invite the bot, correct the room); then the
admin `retry`s the sink or skips it (§1.8).

### 4c.4 Idempotency

§1.4 applies: a timeout holds the job. The transaction ID is the same for every attempt of the job and sink. The spec
treats a request with the same transaction ID and path from the same device as a retransmission and returns the
original response (spec "Transaction identifiers"), so an admin `retry` after `uncertain` returns the original event
instead of posting twice, as long as the homeserver still remembers the transaction and the access token (device) is
unchanged. How long homeservers remember is not specified, so the hold stays.

### 4c.5 `check_config`

`homeserver_url` absolute; token file readable and not empty; `allowed_servers` entries match `server`
(`metadata-interface.md` §1.2); `max_document_bytes` at most 30 000; `content` known; `declaration` present and
complete. No call to the homeserver.

### 4c.6 Operator prerequisites

- [ ] A dedicated bot account on a homeserver the organisation runs or has a contract for; its access token in a
      mounted file. The display name tells room members what the bot is.
- [ ] Room owners invite the bot to their room; the room's power levels let it post `m.room.message`.
- [ ] Network: the worker reaches `homeserver_url`.

### 4c.7 Contract tests (mock homeserver)

The test serves the five endpoints of §4c.2 over HTTPS with fixture rooms (joined, invited, public but not invited,
an alias) and fault injection per request. Fixture token `testdata/secrets/matrix-bot.token` (`fixtures.md` §5).

| ID         | Scenario                                                                                                         | Expected                                                                                                                                                              |
| ---------- | ---------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-SINK-30 | joined room by ID, one link                                                                                      | `joined_rooms`, then one `PUT …/send/m.room.message/recap-<job_id>-<profile>`; bearer token; content exactly §4c.2; record `sent` with the event ID; no sync, no join |
| CT-SINK-31 | alias of a joined room                                                                                           | directory lookup, then the send goes to the resolved room ID                                                                                                          |
| CT-SINK-32 | invited room; public room without invitation                                                                     | sync, join, send / no join and no send, `rejected`                                                                                                                    |
| CT-SINK-33 | send 429; send 500, hang, drop; 401 on `joined_rooms`; 403 and 413 on send; unknown alias; 500 on `joined_rooms` | `not_sent`; `uncertain` (record `sending`); `config`; `rejected`; `rejected`; `not_sent` without a send                                                               |
| CT-SINK-34 | input record `sending`; a retry after `uncertain`                                                                | no request, `uncertain`; the retry uses the same transaction ID and the mock answers with the original event                                                          |
| CT-SINK-35 | `content: summary` over 30 000 bytes; bad room; server not in `allowed_servers`; no `declaration`; log scan      | `rejected` without a request (three cases); tool exit 2; no token, room, title or summary text in stderr                                                              |

## 5. Checks against a real Docs instance

Use synthetic content only. Check:

1. Creating a document for a known owner, for an owner known only by email, and for an unknown owner (invitation).
2. That the Markdown renders as expected, and how the title is shown.
3. Which status code a wrong key returns (401 or 403).
4. The front-end URL pattern for `document_url_template`.
5. The text of Docs' notification and invitation emails (is the title visible?).
6. Whether the throttle applies to `create-for-owner` (429), and how it is keyed.
7. The conversion time of the largest fixture summary against the 30 s timeout.

## 6. Later

- Deliver `transcript.vtt` through a future sink (ADR-007).
- Deliver to participants (ADR-006 option d). This needs Docs access management beyond `create-for-owner`.
- Chat webhook dialects (Mattermost, Slack-compatible `{"text"}`), and Matrix `formatted_body` from a fixed template
  (ADR-016).
- Check a live Matrix homeserver (Synapse) with synthetic content: invite handling through the sync filter, the
  transaction ID after a timeout, the size limit with `content: summary`.

## Known limitations

- The items of §5 are not checked against a live Docs instance.
- Not checked: how Docs answers a second user with the same email (the mock and the contract tests expect a 400 with
  an `email` error, `D/core/api/serializers.py:485-486`).
- Not checked: that the `sub` claim the service sends matches the `sub` Docs stores from its OIDC login, so that an
  existing Docs user is found by `sub` and not only by email (`identity-mapping.md`).
- An expired owner invitation leaves an ownerless document in Docs (§3.3). Cleaning it up is the Docs operator's
  task (`privacy.md`).
- Which status (401 or 403) Docs returns for a wrong key is not checked; both map to `config`.
- The throttle: `DocumentViewSet` has `throttle_scope = "document"` (`D/core/api/viewsets.py:583`, default
  `80/minute`, `D/impress/settings.py:468-471`). Whether `create-for-owner` inherits it, and how anonymous
  server-to-server calls are keyed, is not checked.
- `SERVER_TO_SERVER_API_TOKENS` is a django-configurations `ListValue` (`D/impress/settings.py:86`); the separator
  of its environment value is not checked.
- How Docs converts raw HTML, images with external URLs and block content in table cells is not checked; the dialect
  forbids them (§2.2).
- What Docs does with a title longer than 255 characters is not checked; titles are capped at 200.
- Webhook and Matrix sinks: built from the specifications cited in §4b and §4c, not checked against a live receiver or
  homeserver. Not checked: that `room.rooms` of a sync filter also limits `rooms.invite`, and how long homeservers
  keep transaction IDs. Room aliases outside the ASCII subset of `metadata-interface.md` §1.2 are not supported.

## Sources

- La Suite Docs: https://github.com/suitenumerique/docs @ `6c730f7` (files cited above).
- BlockNote parser: https://github.com/TypeCellOS/BlockNote @ `v0.54.0`.
- Matrix client-server API v1.19: https://spec.matrix.org/v1.19/client-server-api/ (sections "Transaction
  identifiers", "Standard error response", "Rate limiting", "Size limits", "m.notice", `GET /joined_rooms`,
  `GET /sync`, `POST /rooms/{roomId}/join`, `GET /directory/room/{roomAlias}`, `PUT /rooms/{roomId}/send/…`);
  identifiers: https://spec.matrix.org/v1.19/appendices/.
- Standard Webhooks: https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md @
  `81ceb60`.
- Decisions: ADR-005, ADR-006, ADR-007, ADR-009, ADR-010, ADR-014, ADR-016 in [`../decisions/`](../decisions/).
