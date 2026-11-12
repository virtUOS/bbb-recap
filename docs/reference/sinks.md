# Sinks: interface, Markdown dialect, Docs sink

Decisions: [ADR-005](../decisions/ADR-005-metadata-interface.md), [ADR-006](../decisions/ADR-006-identity-owner-only.md),
[ADR-007](../decisions/ADR-007-docs-sink-bot-user.md), [ADR-009](../decisions/ADR-009-privacy-retention.md),
[ADR-010](../decisions/ADR-010-postgres-job-queue.md), [ADR-014](../decisions/ADR-014-results-in-frontend.md). Names,
states, error classes and logging rules are defined in [`README.md`](README.md) and not repeated here.

Facts about La Suite Docs come from reading its source at https://github.com/suitenumerique/docs @ `6c730f7` (v6.0.x).
Below, `D` stands for `src/backend/` in that repository. The facts are collected in
[`upstream-facts.md`](upstream-facts.md). §5 lists what to check against a real Docs instance.

## 1. Generic sink interface

### 1.1 Position

The `deliver` step calls one sink profile per job, named by `meta_recap-sink` (ADR-005). A sink is an adapter that
speaks HTTP directly; it uses no vendor SDK. It delivers `summary.md` only. `transcript.vtt` is rendered but not
delivered (README "Outputs").

### 1.2 Input: delivery request

| Field                      | Notes                                                   |
| -------------------------- | ------------------------------------------------------- |
| `job_id`                   | UUIDv7 (README).                                        |
| `sink_profile`             | Profile name.                                           |
| `title`                    | Plain text, one line, rules in §2.3. Built by `render`. |
| `summary_md`               | `summary.md` from `render`, UTF-8, dialect of §2.       |
| `summary_sha256`           | Lowercase hex of the bytes of `summary_md`.             |
| `owner.sub`, `owner.email` | From the registration (ADR-006, `identity-mapping.md`). |

The request lives only in the service (step input and blob store) and is deleted by `cleanup` (ADR-009). It is never
logged.

### 1.3 Output: delivery record

| Field                    | Notes                                                                                                                                   |
| ------------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| `job_id`, `sink_profile` |                                                                                                                                         |
| `state`                  | `sending` (committed **before** the request, §1.4), `sent` (201 received), `confirmed_by_admin` (`release` after `delivery_uncertain`). |
| `document_id`            | Docs document ID from the response (`{"id"}`). Empty if an admin released the job without it.                                           |
| `sent_at`                | RFC 3339 UTC.                                                                                                                           |

This is the `deliveries` row (`architecture.md` §6.1). The link for humans is built from `document_url_template`
(§3.1) when shown; it is not stored. The record holds no content, title, email or `sub`. It is kept as job metadata
(`privacy.md` §3.2, J1).

### 1.4 Idempotency

`create-for-owner` takes no client-supplied ID, and a repeated request creates a second document
(`D/core/api/serializers.py:458-472,502-507`). So the sink never sends twice on its own:

1. Commit `state = sending` in Postgres, then send.
2. 201 → commit `sent` with `document_id`.
3. On re-entry with `state = sending` (worker crash, lease lost) the sink does not send. It returns `uncertain`.
   `not_sent` and any 4xx other than 429 (Docs answered, so no document was created) delete the record in the same
   transaction as the outcome; a later attempt (backoff, registration `PUT`, admin `release`/`retry`) sends again.
4. A new document is created only by an explicit admin action: `retry` on a `delivery_uncertain` job, or `rerun`
   (new `job_id`, README "Identifiers").

### 1.5 Operations

| Operation                            | Contract                                                                                                                                                                                                                            |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `deliver(request) → record \| error` | At most one HTTP request per call. Safe to call again (§1.4).                                                                                                                                                                       |
| `check_config()`                     | Static checks at start-up (`recap check-config`): URL, key file readable, declaration complete when present (`privacy.md` §7). No call to Docs: the key works only on the create endpoint, so a live check would create a document. |

### 1.6 Error classes

Per README "Adapter error classes → step outcome". Raw error bodies are read only to pick the class and are never
logged.

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
`recap_external_requests_total{kind="sink"}` (`architecture.md` §12).

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
| `name`                  | README name grammar; value of `meta_recap-sink`. Fixture: `docs-main`.                                                             |
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

| ID         | Scenario                                                                                                                                   | Expected                                                                  |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------- |
| CT-SINK-01 | Known owner (sub match)                                                                                                                    | one POST, exact header and fields of §3.2, record `sent` with document ID |
| CT-SINK-02 | Owner known only by email; unknown owner                                                                                                   | 201 both; mock shows owner access / invitation; record identical          |
| CT-SINK-03 | Connection refused, then 429, then 201                                                                                                     | `not_sent` twice, one document in the mock                                |
| CT-SINK-04 | Timeout after create; 500; connection drop                                                                                                 | `uncertain`, job held `delivery_uncertain`, no second POST                |
| CT-SINK-05 | Re-entry with `state = sending`                                                                                                            | no POST, `uncertain`                                                      |
| CT-SINK-06 | Admin `release` / `retry` on `delivery_uncertain`                                                                                          | record `confirmed_by_admin`, job `delivered` / exactly one new POST       |
| CT-SINK-07 | 401, 403, other 400                                                                                                                        | `config` / `config` / `rejected`, no retry                                |
| CT-SINK-08 | Duplicate email in mock                                                                                                                    | held `owner_unresolved`; after a registration `PUT` the job resumes       |
| CT-SINK-09 | Oversized document; forbidden construct; title over 200 characters                                                                         | `rejected` before any call                                                |
| CT-SINK-10 | Log capture of all tests at DEBUG                                                                                                          | no Markdown, title, email, `sub`, key or response body                    |
| CT-SINK-11 | Golden: the built-in format rendered with fixtures passes the §2 check, and a pinned y-provider `/convert` gives the stored BlockNote JSON | equal; drift → human review                                               |

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

## Sources

- La Suite Docs: https://github.com/suitenumerique/docs @ `6c730f7` (files cited above).
- BlockNote parser: https://github.com/TypeCellOS/BlockNote @ `v0.54.0`.
- Decisions: ADR-005, ADR-006, ADR-007, ADR-009, ADR-010, ADR-014 in [`../decisions/`](../decisions/).
