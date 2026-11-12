# ADR-014 Summaries available in the front end (PILOS room)

Status: accepted (2026-11-10), amended 2026-11-12

## Context

- Users want the summaries of a room listed in the room, per processed meeting, like recordings: either a link to
  the document in the configured sink (Docs) or, without Docs, the summary itself.
- Without this, the front end never learns the result. `GET /api/v1/registrations/{record_id}` shows the job state,
  but `cleanup` deleted the registration right after delivery (`RECAP_RETENTION_AFTER_DELIVERY` default `0`), so a
  front end that polled saw 404 after success. The service keeps no content after cleanup (ADR-009).
- The Docs document is created for the owner only, with link reach `restricted`
  ([`sinks.md` §3.2](../reference/sinks.md), ADR-007). Other room members cannot open it unless the owner shares it.
- PILOS recordings have an access level per recording (`EVERYONE`, `PARTICIPANT`, `MODERATOR`, `OWNER`); new
  recordings start at `OWNER`, and a global retention period deletes them
  ([upstream facts](../reference/upstream-facts.md#front-ends)). Summaries follow the same model.

## Options

1. **Push:** after `deliver`, the service calls an endpoint of the front end with the result. Needs an inbound
   endpoint and credentials in every front end, and outbound calls and retries in the service.
2. **Pull:** the front end asks the service with the credentials it already uses for registrations. The service
   keeps a small result record, and for a new sink type the rendered Markdown, until the front end has fetched it or
   a time limit runs out.
3. **The service shows the summaries** in its own viewer. Needs user login and per-room access rules in the service
   and long-lived content there; duplicates what the front end already knows.

## Decision

2, pull.

**Service**

- After delivery the registration stays as a **result record** until `RECAP_RESULT_TTL` (default `7d`) expires. The
  record gains `outcome`, `delivered_at` and `document_url` (from the sink profile's `document_url_template`, `null`
  for sinks without one). The owner identity is deleted at `cleanup` as before; the record holds IDs, times and the
  URL only ([`metadata-interface.md` §2.7](../reference/metadata-interface.md)).
- New sink type **`frontend`**: `deliver` stores the rendered `summary.md` for the registering front end instead of
  sending it elsewhere. `GET /api/v1/registrations/{record_id}/summary` returns it (`text/markdown; charset=utf-8`,
  only to that front end); `DELETE` on the same path confirms the pickup, and the service deletes the content.
  Unfetched content is deleted with the registration after `RECAP_RESULT_TTL`. A meeting has one sink; a front end
  that wants both a Docs document and a copy in the room is out of scope for now.
- The job is `cleaned` as before; only the registration record and, for `frontend`, the summary text outlive it,
  both bounded by `RECAP_RESULT_TTL`.

**Front end (PILOS)**

- After a meeting with recap ends, a queued job polls the registration with back-off and gives up after the
  registration's expiry or a 404. On `delivered` it stores one **meeting summary** per meeting: room, meeting,
  start and end, `access` (default `OWNER`), and either the `document_url` or, for the `frontend` sink, the Markdown
  (then `DELETE …/summary`).
- The room lists the summaries in their own tab, with the same access rules and the same edit and delete actions as
  recordings for owners and co-owners; the recording retention period applies.

## Consequences

- Personal data in the front end: the summary text (names of speakers and action-item owners, meeting content) in
  its database, visible per `access`, deleted with the recording retention, the room, or by the owner. The consent
  text says that the summary may be available in the room.
- Personal data in the service: for `frontend`, the summary text up to `RECAP_RESULT_TTL` after delivery. Admin
  `erase` also deletes a waiting summary.
- In Docs mode, members other than the owner see that a summary exists but need the owner to share the document in
  Docs. The `frontend` sink avoids that.
- Poll cadence and the maximum summary size the front end accepts are front-end choices; the sink's
  `max_document_bytes` (default 2 MiB) bounds the summary size.

## Amendment 2026-11-12: download instead of display

A summary from the `frontend` sink is offered as a Markdown file download in the room, not rendered in the page. A
read-only view inside a room tab is too small for a full summary, and a good reading view would grow the front-end
change; a download needs no Markdown parser in the front end and turns no meeting content into HTML, so there is
nothing to sanitize. Users who want a formatted, editable document use the Docs sink.

- The tab lists each summary with date and access; `frontend` entries have a download action, Docs entries keep the
  link. Access rules, edit and delete actions and retention are unchanged.
- The front end serves the download from its own database with the same access check as the list, as
  `text/markdown; charset=utf-8`, `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`.
- The service side (sink type `frontend`, `GET`/`DELETE …/summary`, `RECAP_RESULT_TTL`) is unchanged.
