# ADR-006 Identity: owner-only delivery via registration

Status: accepted (2026-10-29)

## Context

No front end sends a portable user identity to BBB on join
([upstream facts](../reference/upstream-facts.md#front-ends)). Docs identifies a user by OIDC `sub`, then email
([upstream facts](../reference/upstream-facts.md#la-suite-docs)). The front end and Docs are expected to share one OIDC
identity provider.

## Options

- a. Identity in meta keys.
- b. A resolver per front end.
- c. Owner only, via registration.
- d. c plus a participant map.

## Decision

c now, d possibly later. The front end registers the meeting with the owner's OIDC `sub` and email
([`identity-mapping.md`](../reference/identity-mapping.md)).

## Consequences

- The front end must store the owner's OIDC `sub` as the identity provider issues it to Docs.
- A participant map (d) needs access management in Docs (ADR-007) and is not built.
