# Metadata interface (front end → BBB → service, front end → service)

Interface version **1.1.0** (1.1.0: a list of sink profiles, the key `recap-matrix-room` and the registration field
`result_url`,
[ADR-016](../decisions/ADR-016-multiple-sinks-webhook-matrix.md)). For front-end developers (PILOS, Stud.IP, Moodle, Greenlight or others): a front end
that follows sections 1 and 2 is compatible. Decisions: [ADR-005](../decisions/ADR-005-metadata-interface.md),
[ADR-006](../decisions/ADR-006-identity-owner-only.md), [ADR-009](../decisions/ADR-009-privacy-retention.md),
[ADR-014](../decisions/ADR-014-results-in-frontend.md), [ADR-016](../decisions/ADR-016-multiple-sinks-webhook-matrix.md). IDs, held reasons and HTTP rules: [`README.md`](README.md);
identity: [`identity-mapping.md`](identity-mapping.md). BBB sources are `bigbluebutton/bigbluebutton` at `7d27e7e`
(v4.0.x-release) unless noted; see also [`upstream-facts.md`](upstream-facts.md).

## 0. Overview

A front end opts a meeting in through **two channels**:

| Channel                            | Carries                                                        | Personal data      | Why                                                                                                                                                                                                                               |
| ---------------------------------- | -------------------------------------------------------------- | ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A. `meta_recap-*` on BBB `create`  | version, opt-in, meeting type, sink profiles, Matrix room      | none               | The hook on the BBB server decides offline whether a recording leaves the server ([ADR-002](../decisions/ADR-002-trigger-post-archive-push.md)). Meta values land in the raw recording and may be visible to participants (§1.1). |
| B. Registration API on the service | `meeting_id`, `record_id`, owner `sub` and email, `result_url` | owner `sub`, email | Personal data must not travel through BBB metadata.                                                                                                                                                                               |

Sequence: `create` with meta keys → register `record_id` → recording → `post_archive` hook checks the opt-in → exporter
pushes the package → service validates and joins. Only **both** channels together lead to delivery.

## 1. `meta_recap-*` keys on BBB `create`

### 1.1 BBB rules

- A meta parameter is accepted only if its name matches `meta_[a-zA-Z][a-zA-Z0-9-]*`; others are **dropped silently**
  (`bbb-common-web/src/main/java/org/bigbluebutton/api/ParamsProcessorUtil.java:274-281,296-307 @ 7d27e7e`).
- BBB **lowercases keys**; values pass unchanged and unchecked (`ParamsProcessorUtil.java:300-303`).
- No control characters 0x00–0x1F in API values (`docs/docs/development/api.md:159`); the archiver strips them
  (`record-and-playback/core/lib/recordandplayback/events_archiver.rb:291`).
- Meta values go to `events.xml` `/recording/metadata` if `record=true` or `meetingKeepEvents=true`
  (`events_archiver.rb:286-292`), and to `getMeetingInfo`, `getRecordings`, `metadata.xml` and webhooks
  (`get-meeting-info.ftlx:54-60`, `RecordingServiceFileImpl.java:169-192`, `presentation.rb:129-140 @ 7d27e7e`;
  bbb-webhooks `src/process/event.js:268 @ v3.7.0`). They "may be exposed to users in the meeting" (`api.md:607-610`).
  The published `metadata.xml` lies below a path that nginx serves without authentication
  (`record-and-playback/presentation/scripts/presentation.nginx:28-31 @ 7d27e7e`).
- Breakout rooms inherit the parent's meta (`bbb-common-web/.../api/MeetingService.java:917-924`).
- A second `create` for a running meeting returns the existing meeting with `messageKey=duplicateWarning` and does
  **not** apply new parameters
  (`bigbluebutton-web/grails-app/controllers/org/bigbluebutton/web/controllers/ApiController.groovy:272-287`).
  So the keys must be on the **first** `create` of a session.

### 1.2 Key set (v1)

The prefix (default `recap`, grammar `^[a-z][a-z0-9]{1,15}$`) is set by `RECAP_EXPORTER_META_PREFIX` and
`RECAP_META_PREFIX`; both must match. This document writes `recap`.

| Parameter on `create`    | Key in `events.xml` | Required                        | Value  | Meaning                                                                                                  |
| ------------------------ | ------------------- | ------------------------------- | ------ | -------------------------------------------------------------------------------------------------------- |
| `meta_recap-v`           | `recap-v`           | if opted in                     | `1`    | Major version of this interface.                                                                         |
| `meta_recap-enabled`     | `recap-enabled`     | to opt in                       | `true` | Only the literal `true` opts in ([`README.md`](README.md) "Grammars").                                   |
| `meta_recap-type`        | `recap-type`        | if opted in                     | name   | Meeting type configured in the service ([`templates.md`](templates.md) §1). Built in: `meeting-summary`. |
| `meta_recap-sink`        | `recap-sink`        | if opted in                     | list   | 1–4 sink profiles configured in the service ([`sinks.md`](sinks.md)), never a raw target ID.             |
| `meta_recap-matrix-room` | `recap-matrix-room` | if a `matrix` profile is listed | room   | Matrix room for the `matrix` sink ([`sinks.md`](sinks.md) §4c), chosen by the room owner.                |

`name` = `^[a-z][a-z0-9-]{0,62}$` ([`README.md`](README.md)). Values are ASCII and lowercase; the service does not
fold their case. No whitespace, URLs, free text, **personal data or secrets** (names, emails, IDs, tokens, room or
course names): anyone who can see the metadata may read them. Send each key once, in lowercase. Other `recap-*` keys
are reserved and ignored by v1; new optional keys come in a minor version, breaking changes increment `recap-v`.

`list` = one to four distinct `name`s joined by `,` without spaces:
`^[a-z][a-z0-9-]{0,62}(,[a-z][a-z0-9-]{0,62}){0,3}$`. A single name is a list of one, so the 1.0.0 form stays valid.
The list order is kept within each delivery group; the groups are fixed ([`sinks.md`](sinks.md) §1.1).

The Matrix sink is not implemented yet ([`sinks.md`](sinks.md) §4c): the service refuses a `matrix` profile, so no
configured list can name one. `room` (interface 1.1.0) is the one value that is not a `name`. It is a Matrix room ID or room alias in an ASCII subset
of the Matrix grammar (https://spec.matrix.org/v1.19/appendices/#room-ids, `#room-aliases`, `#server-name`), at most
255 characters, case kept:

```
room       = room-id / room-alias
room-id    = "!" opaque [ ":" server ]          ; the server part is absent from room version 12 on
room-alias = "#" opaque ":" server
opaque     = 1*( ALPHA / DIGIT / "." / "_" / "~" / "=" / "+" / "/" / "-" )
server     = ( 1*( ALPHA / DIGIT / "." / "-" ) / "[" 1*( HEXDIG / ":" / "." ) "]" ) [ ":" 1*5DIGIT ]
```

The service checks the grammar, not whether the room exists. The key is read only when the list names a `matrix`
profile; otherwise it is ignored. The room is visible to anyone who can see the metadata (§1.1); prefer the opaque
room ID to an alias that names the room.

The key is not an authorisation. The bot posts to any room it is a member of or invited to, so anyone who may set the
key in a front end and knows the ID or alias of such a room (every member of that room knows its ID) can direct the
notices of their meetings there, without being a member. A notice holds the meeting title and links, never the
summary ([`sinks.md`](sinks.md) §4c). Front ends let only the room owner set the key, and operators keep the bot out
of rooms that must not receive such notices. A binding that the Matrix room confirms is planned
([ADR-016](../decisions/ADR-016-multiple-sinks-webhook-matrix.md)).

Example (F-01):

```
record=true&meta_recap-v=1&meta_recap-enabled=true&meta_recap-type=meeting-summary&meta_recap-sink=docs-main
```

Several sinks and a Matrix room (values URL-encoded on the wire: `,` as `%2C`, `!` as `%21`, `:` as `%3A`, `#` as
`%23`):

```
meta_recap-sink=docs-main,frontend-main,matrix-main&meta_recap-matrix-room=!fixtureroom01:matrix.recap.test
```

Nothing is exported without recording marks: `record=true` and someone presses record (or
`autoStartRecording=true`); without marks the `post_archive` hook does not run (`workers/archive_worker.rb:45-58 @
7d27e7e`; [ADR-002](../decisions/ADR-002-trigger-post-archive-push.md)).

### 1.3 Who sets the keys, and no opt-in

- Only the **room owner** opts in, per meeting; the registered owner is the room owner. Never set
  `recap-enabled=true` statically: every recorded meeting would be exported and held as `no_registration`.
- `recap-v`, `recap-type` may be static per room type. `recap-sink` is the room owner's choice from the set the room
  type offers (the room type's default when the owner did not choose), `recap-matrix-room` the owner's room setting
  ([ADR-016](../decisions/ADR-016-multiple-sinks-webhook-matrix.md)). In the PILOS integration they are two dedicated
  room type fields (type and sink), not free `create_parameters`; it rejects any `meta_recap-*` in a room type's
  `create_parameters`, so a room type cannot opt rooms in. Opt-in is a per-room toggle that only the room owner may
  change; a room type may default it and may force it off, never on.
- Absent, `false`, `True`, `yes`, `1`, ` true`: **not opted in, silently ignored**. The hook logs `not_opted_in` with
  `record_id` only; nothing leaves BBB. A package that reaches ingest anyway gets `422 not-opted-in` (V-I07).

## 2. Registration API

### 2.1 Conventions and authentication

[`README.md`](README.md) "HTTP" applies (JSON, RFC 9457, TLS outside local tests); body limit 8 KiB. Each front end
has a `frontend_id`, a credential `frontend.<frontend_id>` in `RECAP_CLIENT_SECRETS_DIR` (sent as
`Authorization: Bearer`, else 401). `RECAP_FRONTEND_SERVERS` optionally lists the `bbb_server_id`s each front end
may claim. Unset or empty, every front end may claim packages of every BBB server (the same default as
`RECAP_FRONTEND_TYPES` and `RECAP_FRONTEND_SINKS`); this suits one organisation with one or more front ends. Set it when
several organisations share one service: it is then the boundary between them, and a front end may claim only the
servers listed for it. Without it, a front end that knows another organisation's `record_id` and `meeting_id` could
register that meeting and receive its summary. Another front end's registration is 404 on `GET`/`DELETE` and 409 on `PUT`, without details.

### 2.2 When to call

1. Call BBB `create` with the meta keys; if it fails, do not register.
2. On `returncode=SUCCESS` (also with `duplicateWarning`), read `internalMeetingID`
   (`bigbluebutton-web/src/main/webapp/WEB-INF/freemarker/create-meeting.ftlx:6-7 @ 7d27e7e`; example
   `api.md:357-371 @ 7d27e7e`). This is `record_id`: it names `raw/<record_id>` and is `/recording/@meeting_id` in
   `events.xml` (`events_archiver.rb:267 @ 7d27e7e`).
3. `PUT /api/v1/registrations/{record_id}` before sending users into the meeting. Retry on network errors, 429 and
   5xx with backoff; `PUT` and `DELETE` are idempotent.

Why after `create`: external meeting IDs are reused across sessions (Moodle: `public/mod/bigbluebuttonbn/classes/instance.php:526-539 @
46c042e`); `record_id` is unique per session, so the join is exact. A late registration is fine: the job waits as
`no_registration` and resumes automatically ([`README.md`](README.md)).

### 2.3 Payloads

Schema: [`schemas/registration.schema.json`](schemas/registration.schema.json) (request at the root, response in
`$defs/registration_response`). Request (F-01):

```json
{
  "schema_version": "1.0.0",
  "meeting_id": "fixture-meeting-01",
  "record_id": "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000",
  "owner": {
    "sub": "fixture-sub-ada-0001",
    "email": "ada.testperson@example.org"
  }
}
```

With a link to the summary in the front end (request 1.1.0):

```json
{
  "schema_version": "1.1.0",
  "meeting_id": "fixture-meeting-01",
  "record_id": "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000",
  "owner": { "sub": "fixture-sub-ada-0001", "email": "ada.testperson@example.org" },
  "result_url": "https://pilos.recap.test/rooms/abc-def-123/summaries"
}
```

| Field            | Rule                                                                                                                                                                                                                                                                                                                                                                                               |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema_version` | semver, major `1`                                                                                                                                                                                                                                                                                                                                                                                  |
| `meeting_id`     | external `meetingID` from `create`; 1–256 characters, no control characters                                                                                                                                                                                                                                                                                                                        |
| `record_id`      | equals the path parameter; `^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$` as in the manifest. Observed form `<40 hex>-<epoch ms>` (`api.md:360 @ 7d27e7e`); kept loose on purpose                                                                                                                                                                                                                             |
| `owner.sub`      | OIDC `sub` as Docs knows it; 1–255 characters ([`identity-mapping.md`](identity-mapping.md) §2)                                                                                                                                                                                                                                                                                                    |
| `owner.email`    | 3–254 characters, `format: email`                                                                                                                                                                                                                                                                                                                                                                  |
| `result_url`     | optional (request 1.1.0; omit it when there is none, `null` is invalid): where people find the summary in the front end, e.g. the room's summaries tab behind the front end's login. Absolute `https://` URL without user info, at most 2048 characters, no whitespace or control characters. Sent in notices ([`sinks.md`](sinks.md) §1.2, §4b, §4c); it must not carry a secret or personal data |

No other fields (`additionalProperties: false`); type and sink come only from meta. `result_url` comes through the
registration, not meta: it is set by the authenticated front end, and the room ID it may contain is no business of
participants.
Response (201 or 200; also `GET`), never containing `owner`:

```json
{
  "schema_version": "1.3.0",
  "record_id": "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000",
  "meeting_id": "fixture-meeting-01",
  "frontend_id": "pilos-fixture",
  "status": "registered",
  "created_at": "2026-10-29T10:00:00Z",
  "updated_at": "2026-10-29T10:00:00Z",
  "expires_at": "2026-11-12T10:00:00Z",
  "result_url": "https://pilos.recap.test/rooms/abc-def-123/summaries",
  "job": null
}
```

`status`: `registered` (no package yet; `expires_at` set), `matched` (`job` = `{job_id, state, held_reason, outcome,
delivered_at, document_url, summary_available, deliveries}` with the states, held reasons and outcomes of
[`README.md`](README.md)), `cancelled`. `expires_at` is `null` unless `registered`. The result fields (`outcome`,
`delivered_at`, `document_url`, `summary_available`) are `null`/`false` until delivery (§2.7). Response
`schema_version` `1.3.0` (1.1.0 added the result fields; 1.2.0 the `record_id` pattern of the manifest, which allows up
to 255 characters and needs a letter or digit first; 1.3.0 `job.deliveries` and the top-level `result_url`, echoed
from the request or `null`, §2.7).

### 2.4 `PUT` matrix

| Existing registration                         | Request                 | Result                                                                                                                                                                                                                                                  |
| --------------------------------------------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| none                                          | valid                   | 201                                                                                                                                                                                                                                                     |
| same front end, identical body                | valid                   | 200, unchanged                                                                                                                                                                                                                                          |
| same front end, other `owner` or `result_url` | valid                   | 200, owner and `result_url` replaced (a request without `result_url` removes it) while no job exists or `deliver` is still `pending` (same transaction); else 409 `registration-locked`. Resumes a job held as `owner_unresolved` or `no_registration`. |
| same front end, other `meeting_id`            | —                       | 409 `immutable-field`                                                                                                                                                                                                                                   |
| `cancelled`, no job                           | valid                   | 200, `registered` again                                                                                                                                                                                                                                 |
| `cancelled`, job exists                       | —                       | 409 `registration-cancelled`                                                                                                                                                                                                                            |
| other front end                               | —                       | 409 `conflict`                                                                                                                                                                                                                                          |
| any                                           | path ≠ body `record_id` | 400 `invalid-request`                                                                                                                                                                                                                                   |

A `PUT` replaces the whole registration body: a later `PUT` without `result_url` removes it (the response then shows
`null`), under the same lock as `owner`.

Of the delivery records, only those of sinks that use the owner identity (`docs`) lock the owner; a `frontend` or
`webhook` record does not ([`sinks.md`](sinks.md) §1.8 rule 3). With two `docs` sinks, once one has delivered, the
owner stays locked, and the admin skips the held sink with `release`.

### 2.5 `DELETE` (cancel)

| State                                                                                    | Result                                                                                                                                           |
| ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `registered`                                                                             | 204. Owner identity deleted now; a tombstone (IDs and `cancelled_at`) is kept for 30 days (constant), so a later package is discarded, not held. |
| job exists, `deliver` not `claimed`                                                      | 202. Job stops at the next step boundary; content and owner identity deleted; job `cleaned`, outcome `cancelled`.                                |
| `deliver` claimed or delivered                                                           | 409 `already-delivered`. The Docs document belongs to the owner ([ADR-007](../decisions/ADR-007-docs-sink-bot-user.md)).                         |
| already `cancelled`                                                                      | 204                                                                                                                                              |
| job purged (no document was made) and its `cleanup` has not yet removed the registration | 204, registration deleted and tombstoned as for `registered`; after that `cleanup` the registration is gone: 404                                 |
| not found / other front end                                                              | 404                                                                                                                                              |

`GET` returns the response object; 404 if absent or foreign.

### 2.6 Errors (`urn:recap:problem:<slug>`; `detail` never contains personal data)

| HTTP | slug                                                                                                | When                                                    |
| ---- | --------------------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| 400  | `invalid-request`                                                                                   | not JSON, schema violation, path/body mismatch          |
| 401  | `unauthorized`                                                                                      | missing or unknown credential                           |
| 404  | `not-found`                                                                                         | no registration visible to this front end               |
| 404  | `summary-not-available`                                                                             | §2.7                                                    |
| 409  | `conflict`, `immutable-field`, `registration-locked`, `registration-cancelled`, `already-delivered` | see 2.4, 2.5                                            |
| 413  | `payload-too-large`                                                                                 | body > 8 KiB                                            |
| 422  | `unsupported-schema-version`                                                                        | major ≠ 1                                               |
| 429  | `rate-limited`                                                                                      | per-front-end limit, 600 requests per minute (constant) |

A registration that never succeeds leads to a held job and an alert, not to data loss.

### 2.7 Results

The front end learns the result by polling `GET /api/v1/registrations/{record_id}` (pull; no callback;
[ADR-014](../decisions/ADR-014-results-in-frontend.md)).

- **Result record.** `cleanup` deletes the owner identity (`sub`, email) but keeps the registration row with IDs,
  `delivered_at`, the outcome and `document_url` until `delivered_at + RECAP_RESULT_TTL` (default `7d`); then the
  cleanup loop deletes it and `GET` returns 404. With `RECAP_RESULT_TTL=0` the row goes at `cleanup`.
- **`document_url`**: the sink profile's `document_url_template` (default `<docs_base_url>/docs/{id}/`) with the
  delivery's document ID ([`sinks.md`](sinks.md) §3.1); `null` for sinks without one, for `release` without a
  document ID, and for the `frontend` sink.
- **`summary_available`**: `true` only for the `frontend` sink ([`sinks.md`](sinks.md) §4a) while the summary waits
  for pickup.
- **`deliveries`** (1.3.0): one entry per sink of the job, in delivery order ([`sinks.md`](sinks.md) §1.1):
  `{sink_profile, state, document_url, summary_available}`. `state` is the sink status of [`sinks.md`](sinks.md) §1.8
  (`pending`, `sending`, `sent`, `confirmed_by_admin`, `skipped_by_admin`, `failed`); `document_url` and
  `summary_available` as above, for that sink. The empty list `[]` until `validate` has read the sinks. The job's
  `document_url` is the first non-null `document_url` of the list, and its `summary_available` is `true` when any
  entry's is. A front end that sent a list reads `deliveries`; one that sends a single name may keep reading the
  top-level fields.
- `GET /api/v1/registrations/{record_id}/summary`: `200`, `text/markdown; charset=utf-8`, the delivered `summary.md`,
  as soon as the `frontend` sink is `sent`, also while the job's other sinks are still running;
  `404 summary-not-available` when there is none (not sent yet, other sink, already picked up, expired) or the
  registration is foreign. Only the registering front end.
- **`result_url`** (1.3.0, top level): as sent in the request, or `null`. With a `frontend` sink, notices of the
  `webhook` and `matrix` sinks carry it and wait until the front end has fetched the summary (`DELETE …/summary`), so
  the link works when the notice arrives ([`sinks.md`](sinks.md) §1.8).
- `DELETE /api/v1/registrations/{record_id}/summary`: confirms the pickup; the service deletes the text. `204`, also
  when nothing was there (idempotent). The result record stays until its TTL. A foreign registration → `404
summary-not-available`, nothing deleted. Every 404 on `…/summary` uses the slug `summary-not-available`, also when
  the registration no longer exists.
- `DELETE /api/v1/registrations/{record_id}` on a delivered registration stays `409 already-delivered` (§2.5).
- A `rerun` (admin) replaces `job` with the new job; the new delivery sets new result fields.
- Polling: a front end polls with back-off (suggested 2, 5, 10, 30 minutes, then hourly) and stops at `delivered`, a
  `failed` job, `cancelled`, or 404. It fetches and confirms the summary as soon as `summary_available` is `true`,
  without waiting for `delivered`: with a `result_url`, a `frontend` sink and a `webhook` or `matrix` sink, the job
  is `delivered` only after that pickup, and the notice follows at the next attempt (at most 30 minutes later). A
  front end that sends a `result_url` polls at least every few minutes while such a job is open, at least for the
  first two hours. A front end that never fetches leaves the notice sinks `failed` after the step's attempts (about
  two hours, [`sinks.md`](sinks.md) §1.8); a pickup after that does not resume the job, an admin `retry` does. Every
  poll counts against the rate limit of §2.6.

## 3. Validation and held reasons

Runs in `validate` on the manifest's `recap-*` values (equal to `events.xml`, V-S04). The first failing check sets the
outcome, and the admin job `detail` is that check's single code ([`architecture.md`](architecture.md) §4).

| #   | Check                                                                                                                                                                  | Outcome                                                                                     |
| --- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| 0   | `recap-enabled` = `true`                                                                                                                                               | else ingest `422 not-opted-in`, no job                                                      |
| 1   | no tombstone for `record_id`                                                                                                                                           | else job `cleaned`, outcome `cancelled`                                                     |
| 2   | `recap-v` = `1`                                                                                                                                                        | `invalid_metadata`                                                                          |
| 3   | `recap-type` present, matches `name`                                                                                                                                   | `invalid_metadata`                                                                          |
| 4   | `recap-type` configured                                                                                                                                                | `unknown_type`                                                                              |
| 5   | `recap-sink` present, matches `list` (§1.2: 1–4 names, distinct)                                                                                                       | `invalid_metadata`                                                                          |
| 6   | every name of `recap-sink` configured                                                                                                                                  | `unknown_sink`                                                                              |
| 6a  | at most one profile of type `frontend` and at most one of type `matrix` in the list (one summary for pickup, one room per meeting)                                     | `invalid_metadata` (detail `sink-list-invalid`)                                             |
| 6b  | if the list names a `matrix` profile: `recap-matrix-room` present and matches `room` (§1.2)                                                                            | `invalid_metadata` (detail `matrix-room-invalid`)                                           |
| 7   | active registration for `record_id`                                                                                                                                    | `no_registration`                                                                           |
| 8   | registration's front end may claim the package's `bbb_server_id` (`RECAP_FRONTEND_SERVERS`; skipped when unset)                                                        | `no_registration` (detail `server-not-allowed`)                                             |
| 9   | registration `meeting_id` = package `meeting_id`                                                                                                                       | `no_registration` (detail `meeting-id-mismatch`)                                            |
| 9a  | the registration's front end may request this type and every listed sink (`RECAP_FRONTEND_TYPES`, `RECAP_FRONTEND_SINKS`; a front end without entries may request all) | `unknown_type` (detail `type-not-allowed`), then `unknown_sink` (detail `sink-not-allowed`) |
| 10  | owner resolvable in Docs (at `deliver`, per `docs` sink; [`sinks.md`](sinks.md) §1.6, [`identity-mapping.md`](identity-mapping.md) §4)                                 | `owner_unresolved`                                                                          |

`no_registration` and `owner_unresolved` resume automatically on a matching registration `PUT`; other holds need an
admin (fix config, then `release` or `rerun`, or `purge`). Package meta is never edited. Held jobs keep content
([ADR-009](../decisions/ADR-009-privacy-retention.md)); a metric per reason alerts.

On success `validate` stores the meeting type, the sink list in delivery order and, for a `matrix` profile, the room
in job columns ([`architecture.md`](architecture.md) §5.2); the pipeline never reads the meta keys again (ADR-013).

Values the manifest schema already rejects (for example control characters) fail at ingest with
`422 manifest-invalid`; `invalid_metadata` covers schema-valid packages whose metadata breaks this table.

## 4. Join with the meeting package

- Manifest: `record_id` = `/recording/@meeting_id`, `meeting_id` = `/recording/meeting/@externalId`, breakout flag
  (`events_archiver.rb:267,281,283 @ 7d27e7e`); ingest adds `bbb_server_id` from the credential.
- **Join key:** `record_id`, exact; checks 8 and 9 guard against foreign or stale claims. Not used: `meta_bbb-origin`
  (unauthenticated), names, `userID`s. Breakouts are not exported and never registered (V-I08).
- Behind a load balancer such as Scalelite, every BBB server of the cluster may share one credential
  `bbb_server.<cluster-id>`, so all their packages carry that one `bbb_server_id`
  ([`operations/install-exporter.md`](../operations/install-exporter.md) "BBB cluster or Scalelite"). Scalelite passes
  every `create` parameter except those in `CREATE_EXCLUDE_PARAMS` on to the chosen BBB server and returns that
  server's response unchanged (`app/controllers/bigbluebutton_api_controller.rb:251-253,270-271 @ b99312f`,
  `config/application.rb:107 @ b99312f`, https://github.com/blindsidenetworks/scalelite). So `meta_recap-*` reach
  BBB, and the front end reads BBB's `internalMeetingID` from the response as without a load balancer, as long as
  `CREATE_EXCLUDE_PARAMS` does not list a `meta_recap-*` key.

## 5. Data flows

| Flow                                       | Personal data                                                        | Stored where                                                          | Retention                                                                                                                                            | Deleted by                                                                            |
| ------------------------------------------ | -------------------------------------------------------------------- | --------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `create` meta                              | none; reveals opt-in, type, sink profiles and the Matrix room (§1.2) | BBB memory, Redis, `events.xml`, `metadata.xml`, API, webhooks (§1.1) | with the BBB recording                                                                                                                               | BBB cleanup / exporter ([ADR-002](../decisions/ADR-002-trigger-post-archive-push.md)) |
| Manifest `recap-*` copy                    | none                                                                 | service blob store, job tables                                        | job content until `cleanup`; type and sink may stay in job metadata                                                                                  | `cleanup` step                                                                        |
| Registration                               | owner `sub`, email                                                   | service Postgres                                                      | unmatched: `RECAP_REGISTRATION_TTL` (`14d`); matched: until `cleanup` (default right after delivery); held: until an admin acts                      | cleanup loop, `cleanup`, admin `purge`                                                |
| Tombstone                                  | none (IDs)                                                           | service Postgres                                                      | 30 days (constant)                                                                                                                                   | cleanup loop                                                                          |
| Result record (§2.7)                       | none (IDs, times, `document_url`, `result_url`)                      | service Postgres                                                      | `RECAP_RESULT_TTL` (`7d`) after delivery                                                                                                             | cleanup loop, admin `erase`                                                           |
| Summary for pickup (§2.7, `frontend` sink) | summary text (names, meeting content)                                | service blob store                                                    | until the front end's `DELETE …/summary`, at most `RECAP_RESULT_TTL` after delivery; a job that is never delivered keeps it until `purge` or `erase` | front end, cleanup loop, admin `purge`, admin `erase`                                 |
| Summary in the front end                   | summary text or document link                                        | front-end database                                                    | the front end's policy (PILOS: recording retention, access per summary)                                                                              | front end                                                                             |

IDs may be logged at INFO; `owner.sub`, `owner.email` and request bodies never ([`README.md`](README.md) "Logging").

## 6. Security

- **Meta is unauthenticated**: anyone with the BBB secret sets it, and participants may see it (§1.1). It only
  _permits_ export; _delivery_ needs an authenticated registration. A forged opt-in is held as `no_registration`.
  Signed meta values would close this gap; they are not supported yet.
- **Registration hijack** is limited by checks 8 and 9, first front end wins, and no owner change after `deliver`
  starts.
- **Credentials** are per front end, from mounted files, never in meta, URLs or logs; treat them like the BBB secret.
- No tokens or emails in meta, unlike prior art: `bigbluebutton/bbb-record-ai-summary` reads
  `meta_la-suite-numerique-docs-access-token` (`src/scripts/post_publish/publish_to_docs.rb:121-135 @ fd1c2cd`);
  Moodle puts the creator's email into `meta_bn-recording-status` when that option is on
  (`public/mod/bigbluebuttonbn/classes/meeting.php:436-473 @ 46c042e`).

## 7. Contract tests

Fixtures only; tests pin `schema_version` major 1 and `recap-v=1`.

| ID              | Pins                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-META-01      | All key names are lowercase and valid after `meta_`: the four of 1.0.0 and the optional `recap-matrix-room`.                                                                                                                                                                                                                                                                                                                                                                                                                                |
| CT-META-02      | `name` grammar: empty, 64 chars, uppercase, `_`, `.`, leading digit, whitespace, control char → reject; valid → accept.                                                                                                                                                                                                                                                                                                                                                                                                                     |
| CT-META-03      | Opt-in: absent, `false`, `True`, `1`, `yes` → not opted in; `true` → opted in.                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| CT-META-04      | Order and outcome of every row of section 3 (one fixture per reason).                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| CT-META-05 / 06 | Unknown `recap-*` key ignored / manifest copy ≠ `events.xml` → `manifest_metadata_mismatch` (V-S04).                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| CT-REG-01       | Schema accepts valid fixtures, rejects invalid ones (missing or extra field, bad `record_id`, `sub` > 255, bad email, `schema_version` 2.0.0).                                                                                                                                                                                                                                                                                                                                                                                              |
| CT-REG-02 / 03  | `PUT` matrix (2.4) / `DELETE` matrix (2.5), incl. tombstone discarding a later package.                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| CT-REG-04       | Auth: no credential → 401; foreign registration → 404 / 409 without details.                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| CT-REG-05       | Errors are RFC 9457 with `urn:recap:problem:<slug>`; responses never contain `owner`.                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| CT-REG-06 / 07  | Late registration: held `no_registration`, then `PUT` → resumes / logs never contain `owner.sub` or `owner.email`.                                                                                                                                                                                                                                                                                                                                                                                                                          |
| CT-REG-08       | Result fields (§2.7): `null`/`false` before delivery; after delivery `outcome`, `delivered_at`, `document_url` set; owner data gone; record kept until `RECAP_RESULT_TTL`, then 404; `RECAP_RESULT_TTL=0` → 404 right after `cleanup`.                                                                                                                                                                                                                                                                                                      |
| CT-REG-09       | `…/summary` (§2.7): `frontend` sink → 200 `text/markdown` with the rendered bytes, `summary_available` true; `DELETE` → 204, then 404 and `summary_available` false; Docs sink, foreign front end, before delivery → 404 `summary-not-available`; TTL expiry deletes it; admin `erase` deletes it.                                                                                                                                                                                                                                          |
| CT-META-07      | `recap-sink` lists (§1.2, §3 rows 5, 6): two configured names (either order) pass to the next check; trailing or leading comma, empty element, space after a comma, a duplicate, five names, a bad name, `;` → `invalid_metadata`; a known and an unknown name, four valid names of which two are unknown → `unknown_sink`. Rows 6a and 9a per name are left to unit tests (the contract environment has one `frontend` profile and no `RECAP_FRONTEND_SINKS`).                                                                             |
| CT-META-08      | `recap-matrix-room` (§1.2, §3 row 6b, needs a `matrix` profile): missing, bad grammar (no sigil, alias without server, whitespace, empty opaque part, 256 characters, a user ID, a URL) → `invalid_metadata` `matrix-room-invalid`; room IDs with or without server part, on any server, and an alias pass; the key without a `matrix` profile is ignored.                                                                                                                                                                                  |
| CT-REG-10       | `deliveries` and `result_url` (§2.3, §2.7): one entry per sink in delivery order with per-sink `state`, `document_url`, `summary_available`; the top-level fields derived from it; `result_url` echoed (`null` without one); response `schema_version` 1.3.0 or later; a bad `result_url` (`http://`, user info, over 2048 characters, whitespace) → 400; schema fixtures for requests and 1.3.0 responses (missing `deliveries` or `result_url`, unknown state, extra field rejected; 1.1.0 and 1.2.0 responses without them still valid). |
| CT-JOIN-01      | Join by `record_id`; server not allowed → `no_registration`; `meeting_id` mismatch → `no_registration`.                                                                                                                                                                                                                                                                                                                                                                                                                                     |

## 8. Known limitations

- If the same key is sent twice in different case, it is not known which value BBB keeps. Send each key once, in
  lowercase.
- BBB's maximum `meetingID` length was not checked; the service accepts up to 256 characters.
- Whether the published `metadata.xml` is readable by anyone on a live server was not checked (§1.1). Treat meta
  values as public.
- There is no discovery endpoint for configured meeting types and sinks; front ends are configured with the names.
- `room` accepts an ASCII subset of the Matrix grammar. Aliases with other characters (the Matrix grammar allows any
  Unicode except `:` and NUL in the alias localpart) are not supported; use the room ID.
