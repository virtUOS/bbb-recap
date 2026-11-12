# ADR-009 Privacy and retention defaults

Status: accepted (2026-10-29), amended 2026-11-11

## Context

The service does not provide participant notices, per-participant consent or a legal assessment.

## Decision

- Retention is configurable. Default: delete package, audio, transcript, summary and owner identity right after
  successful delivery; keep pseudonymous job metadata. Held or failed jobs keep content until an admin acts.
- No transcript or summary content in logs at INFO or above.
- [`privacy.md`](../reference/privacy.md) keeps a data inventory and data-flow description as input for a data
  protection impact assessment (DPIA).

## Consequences

- Whether and how the service may be used with real meetings is the deploying organisation's decision. Pilots with
  informed participants are the intended first use.

## Amendment 2026-11-11

The legal assessment (legal basis, DPIA, data protection officer, works council) is outside this project;
`privacy.md` provides the technical input for it.
