# ADR-016 Several sinks per meeting, a signed webhook sink and a Matrix sink

Status: accepted (2026-11-13)

## Context

- A job delivers to exactly one sink profile, named by `meta_recap-sink` (ADR-005). A room cannot get a Docs document
  **and** a copy in the front end (left out of scope in ADR-014), nor a note in the room's chat.
- Rooms in PILOS often have a team chat room on Matrix. Room owners want a short note there when a summary is ready.
- Programs (LMS, automation) want a machine-readable notice with a signature they can check.
- The sink protocol "never send twice on its own" ([`sinks.md` §1.4](../reference/sinks.md)) and the owner-only reach
  of the Docs sink (ADR-006) must hold.
- Facts: Matrix client-server API v1.19 (https://spec.matrix.org/v1.19/client-server-api/), Standard Webhooks
  (https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md); collected in
  [upstream facts](../reference/upstream-facts.md#matrix-and-standard-webhooks).

## Options

1. **Bundle profile** in `sinks.yaml` (`type: bundle` listing other profiles); the meta key stays one name.
2. **List in `recap-sink`**: the front end sends up to four profile names; the room type offers a set, the room owner
   picks from it.
3. **Targets in the registration**, including chat URLs or rooms per meeting through the API.

For chat: a chat webhook dialect (Mattermost, Slack, matrix-hookshot `{"text"}`), or a native Matrix sink that posts
as a bot user.

## Decision

2, with a generic signed webhook and a native Matrix sink. No chat webhook dialect for now.

- **Meta key:** `recap-sink` is a comma-separated list of 1–4 distinct profile names. A single name stays valid, so
  `recap-v` stays `1`. Each name is checked against `RECAP_FRONTEND_SINKS`. At most one `frontend` and one `matrix`
  profile per list.
- **Delivery:** one `deliver` step with one item per sink. Fixed order, not list order: `frontend` and `docs` first,
  then `webhook` and `matrix`, each group in list order; the second group starts only when the first is done, so a
  notice can carry the Docs link. When the front end gave a `result_url` and the job has a `frontend` sink, the
  second group also waits until the front end has fetched the summary, so the link in a notice works. A sink that
  is `sent` is never sent again. Each sink keeps the rules of
  [`sinks.md` §1.4](../reference/sinks.md): a timeout holds the job as `delivery_uncertain`, for every sink type.
- **Job outcome:** `delivered` only when every sink is `sent`, `confirmed_by_admin` or `skipped_by_admin`. Admin
  `retry` and `release` take an optional `sink`; `release` of a failed sink marks it `skipped_by_admin`. A sink still
  `pending` when the step runs out of attempts becomes `failed` (`retries_exhausted`), so it can be skipped too. No new
  outcome value. Content is kept until then (ADR-009).
- **Result record:** the registration response lists `deliveries` per sink (response `schema_version` 1.3.0); the old
  top-level `document_url` and `summary_available` stay. The registration request may carry `result_url`, the place
  in the front end where people find the summary (request 1.1.0). It comes from the authenticated front end, not
  from meta.
- **Webhook sink (`type: webhook`):** one signed POST (Standard Webhooks, HMAC-SHA256) to a URL fixed in the profile.
  The body carries `job_id`, the title and the links of sinks delivered before it; the summary text only when the
  profile sets `content: summary`. Never owner data.
- **Matrix sink (`type: matrix`):** posts one short `m.notice` as a bot user through the client-server API: the title
  and the links where the summary can be read, first the front end's `result_url`, then a Docs link. Never the
  summary: a room reaches every member and federated servers keep copies, so the text stays behind the front end's
  or Docs' access control. Homeserver URL and access token file in the profile. The room comes from the meeting: new
  optional meta key `recap-matrix-room` (room ID or alias), set by the room owner in the front end. The bot posts
  only to a room it has joined, or accepts a pending invitation to exactly that room; it never joins a room it was
  not invited to. An alias is resolved, and the resolved room ID is the target.
- **No server allow-list for Matrix rooms.** A room ID's server part names only the server that created the room and
  implies nothing about where the room is now, and room IDs from room version 12 on (the default for new rooms) have
  no server part. A list of servers would not limit where a notice goes, so the profile has none.
- **Declarations:** a `webhook` or `matrix` profile must have a complete `declaration`
  ([`privacy.md` §7](../reference/privacy.md)); the service refuses to start without one.
- **Who chooses:** the front end's admin sets the profiles a room type offers and its default set; the room owner
  picks a non-empty subset and the Matrix room. Every target type was set up and declared by an admin.

Option 1 gives no per-room choice. Option 3 brings secrets and arbitrary URLs through the API (server-side request
forgery), breaks "type and sink come only from meta" (ADR-005) and has no declaration review. A chat webhook URL is a
secret per channel and the dialects render Markdown differently; Matrix covers the chat need with one API.

## Consequences

- A post in a Matrix room reaches every member of the room, and the room's homeserver and every federated server in
  the room keep a copy. The service cannot delete it (it does not redact). The room owner chooses the room; the
  front end's consent text must name each destination ([`privacy.md` §11](../reference/privacy.md)).
- `recap-matrix-room` is visible to anyone who can see the meeting's metadata (`metadata-interface.md` §1.1). Room
  IDs are opaque; aliases may reveal a room name.
- The room key is not an authorisation. Anyone who may set it in a front end and knows the ID or alias of a room the
  bot is in can direct the notices of their meetings there, without being a member of that room. What lands there
  is a notice (a title of at most 200 characters and links that open only for those the front end or Docs lets in).
  This is accepted for now; front ends let only the room owner set the key, and operators keep the bot out of rooms
  that must not receive such notices.
- With a `result_url`, the job is `delivered` only after the front end fetched the summary. A front end that never
  fetches leaves the notice sinks `failed` after the step's attempts; an admin skips or retries them.
- A failing sink delays cleanup of the whole job until an admin acts; the held and failed alerts cover it.
- A new migration changes the delivery record key to `(job_id, sink_profile)`.
- An older service holds a job with a list as `invalid_metadata`; front ends send lists only to services that
  support them.
- PILOS: the room type field becomes an offered set and a default; a room setting holds the owner's choice and the
  Matrix room.
- ADR-005 (`<p>-sink` names one profile) and ADR-014 ("a meeting has one sink") are amended by this decision.

## Later

- **Room binding confirmed by the Matrix room.** A moderator of the Matrix room confirms once that a front-end room
  may post there; the service keeps the binding as state and posts only to confirmed bindings. This closes the gap
  above. Not specified yet.

## Sources

ADR-005, ADR-006, ADR-007, ADR-009, ADR-014; [`metadata-interface.md`](../reference/metadata-interface.md) §1.2, §2.7,
§3; [`sinks.md`](../reference/sinks.md) §1, §4b, §4c; [`privacy.md`](../reference/privacy.md) §11.
