# ADR-002 Trigger: post_archive hook, spool, push exporter

Status: accepted (2026-10-29)

## Context

BBB has no API to download raw recordings. A `post_archive` script runs after BBB archives a meeting, and only for
meetings with recording marks. The recording-ready callback has no retry
([upstream facts](../reference/upstream-facts.md#bbb-metadata-and-hooks)).

## Options

- A. post_archive hook, spool and exporter daemon (push).
- B. A custom BBB recording format.
- C. Pull after a callback or webhook.
- D. A plus webhooks.

## Decision

A. A small Ruby hook only checks the opt-in key and the recording marks and writes a spool entry: no network, no heavy
work. A Go exporter daemon builds the package and pushes it with retries. Users mark the parts to summarize with the
record button.

## Consequences

- Only outbound traffic from BBB. Uploads do not block the BBB recording pipeline (a known problem of hooks that
  upload in-line, see elan-ev/opencast-bigbluebutton-integration PR #72 @ `233179d`).
- The exporter owns deletion of `raw/<record_id>`: never (default), after a verified acknowledgement, or after N days
  of failure ([`bbb-exporter.md` §7](../reference/bbb-exporter.md)).
