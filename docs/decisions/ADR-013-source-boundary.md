# ADR-013 Source boundary: the normalized meeting

Status: accepted (2026-11-03)

## Context

The service takes recordings from BBB only (ADR-001). Most of the pipeline does not depend on BBB: ASR and LLM
adapters (ADR-008), the transcript schema, formats and rendering, sinks, retention, `erase`, the admin UI and the job
queue. Other sources are plausible later: LiveKit or Jitsi egress, exports from other meeting platforms, single audio
files. They usually deliver one mixed audio track instead of one track per speaker, so speaker labels would come from
diarization rather than from the source. BBB with a non-LiveKit audio bridge is the same case (ADR-001).

Supporting another source later should not need a rewrite. It needs one clear place where BBB-specific data ends.

## Options

- a. Change nothing; decide when a second source is needed.
- b. Name the normalized meeting (output of `normalize`) as the source boundary now, keep it free of BBB concepts,
  and check the code for leaks. Build no second source.
- c. Build a generic source (upload API, manifest `source` field, diarization) now.

## Decision

b. **The normalized meeting is the boundary between a source and the pipeline.**

- **Source side** (one implementation per source): transport into the service (for BBB: the exporter and ingest),
  the package and its manifest ([`meeting-package.md`](../reference/meeting-package.md)), the `validate` step, how a
  meeting is registered and matched to its owner ([`metadata-interface.md`](../reference/metadata-interface.md)),
  and the `normalize` step that turns the package into a normalized meeting.
- **Pipeline side** (shared by all sources): `transcribe`, `assemble`, `summarize`, `render`, `deliver`, `cleanup`.
  These steps read only the normalized meeting, the files it references by path and hash, and the source-neutral
  job columns (`meeting_type`, `sink_profile`, owner from the registration). They never read the package manifest,
  `events.xml` or BBB identifiers.
- **Identity:** jobs are identified by `job_id` (UUIDv7), independent of the source. The BBB `record_id` is only the
  source-side join key between package and registration; where the pipeline side shows it (admin views, logs) it is
  an opaque string that no code parses. A second source brings its own join key.
- The normalized meeting gets a versioned JSON schema next to the transcript schema once its fields are shown to be
  source-neutral. A field that only BBB can fill is optional.
- No second source, no upload API and no diarization for now.

## Consequences

- A new source means a new transport, a new `validate`/`normalize` pair and a registration path, with no change to
  the pipeline steps, as long as the boundary holds.
- New pipeline code is checked for BBB-specific reads.
- Sources with one mixed track give lower speaker attribution quality (diarization labels, no names) than BBB's
  per-speaker tracks. The transcript schema already allows segments without a known participant name; whether it
  needs a diarization label field is decided with the first such source.
- Admin views and metrics may keep showing BBB fields (`bbb_server_id`, `record_id`) for operators; they are labelled
  as source details, not pipeline concepts.

## Sources

ADR-001, ADR-003, ADR-004, ADR-008; [`architecture.md` §5.2](../reference/architecture.md);
`internal/steps/normalize/normalize.go` (`Meeting` type).
