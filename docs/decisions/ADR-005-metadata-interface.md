# ADR-005 Metadata interface: meta_ keys plus a registration API

Status: accepted (2026-10-29), amended 2026-11-13 by [ADR-016](ADR-016-multiple-sinks-webhook-matrix.md)

## Context

BBB `meta_*` keys must match `[a-zA-Z][a-zA-Z0-9-]*`, are lowercased, are written to `events.xml` and may be visible
to participants ([upstream facts](../reference/upstream-facts.md#bbb-metadata-and-hooks)). PILOS supports only static
`create` parameters per room type ([upstream facts](../reference/upstream-facts.md#front-ends)).

## Options

- a. Plain meta keys.
- b. A signed JWT in one meta key.
- c. A registration API on the service only.
- a+c.

## Decision

a+c. Non-personal meta keys `<p>-v`, `<p>-enabled`, `<p>-type`, `<p>-sink` (prefix configurable, default `recap`).
Personal data (the owner) goes through a registration API on the service. No opt-in key means no processing. Opt-in
present but other keys missing or invalid, an unknown type, or no registration: the job is held and an admin can act
([`metadata-interface.md`](../reference/metadata-interface.md)).

## Consequences

- Front ends (PILOS, others) need a small change to set the keys and call the registration API.
- `<p>-sink` names a sink profile configured in the service, not a raw target ID.

## Amendment 2026-11-13

`<p>-sink` is a list of 1–4 profile names, and the optional key `<p>-matrix-room` names the Matrix room for a
`matrix` profile ([ADR-016](ADR-016-multiple-sinks-webhook-matrix.md)). The room is a target chosen by the room
owner; it carries no credential, and the bot posts only where it was invited. It is not an
authorisation (ADR-016, Consequences).
