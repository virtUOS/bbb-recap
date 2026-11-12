# ADR-007 Docs sink: `create-for-owner`, summary only, WebVTT transcript kept back

Status: accepted (2026-10-29), revised the same day. The file name is kept for link stability; the bot-user design it
first described is superseded.

## Context

- Goal: the owner simply has the summary as a document in their own Docs account.
- `POST /api/v1.0/documents/create-for-owner/` with a server-to-server key creates a document owned by the user
  matched by `sub`, then email; for an unknown user Docs creates an owner invitation and emails it.
- That endpoint takes no client-supplied ID, so a blind retry after a timeout can create a duplicate.
- A bot user on the resource-server API would need an undocumented email-to-UUID lookup, a handover step, invitations
  that only work for new accounts, and child documents that inherit the parent's sharing.
- Sources: [upstream facts](../reference/upstream-facts.md#la-suite-docs).

## Options

1. Bot user on the resource-server API (first version of this ADR).
2. Token exchange to act as the owner (not relied on: not checked for the identity provider in use).
3. `create-for-owner`.

## Decision

3. Per job, `deliver` sends one `create-for-owner` request: title = meeting name and date, content = `summary.md`,
   `sub` and `email` from the registration. It retries only when the request provably was not processed (connection
   refused, HTTP 429). On an uncertain outcome (timeout, lost connection, 5xx) the job is held as `delivery_uncertain`;
   an admin checks Docs and then releases (mark delivered) or retries. The result is exactly one document or a visible
   hold, never a silent duplicate. The transcript is rendered as `transcript.vtt` (WebVTT) but not delivered; a later
   sink may deliver it. It is deleted at cleanup like all content ([`sinks.md` §3](../reference/sinks.md)).

## Consequences

- Operator prerequisites: one key in Docs' `DJANGO_SERVER_TO_SERVER_API_TOKENS`, and the same identity provider
  realm for the front end and Docs, so that `sub` matches.
- No bot user, no handover, no fallback email, no SMTP in the service.
- The service has no rights on the document afterwards. Re-delivery is an admin `rerun` (new document).
- Docs sends its own notification or invitation email.
