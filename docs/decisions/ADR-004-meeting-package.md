# ADR-004 Meeting package: allow-listed raw files plus versioned manifest

Status: accepted (2026-10-29)

## Context

The BBB raw directory holds `events.xml`, audio, video, deskshare, presentation, notes and learning-dashboard data
([upstream facts](../reference/upstream-facts.md#bbb-raw-recording)).

## Options

- a. The whole raw directory plus a manifest.
- b. A normalized package built on the BBB server.
- c. Allow-listed raw files as they are plus a manifest; the service normalizes.

## Decision

c. Contents: `events.xml`, the microphone files in `audio/`, `notes/`, `presentation/`. Excluded: `video/`,
`deskshare/`, `learning-dashboard/`. The manifest is versioned JSON with SHA-256, size and role per file, the meeting
IDs and the opt-in metadata ([`meeting-package.md`](../reference/meeting-package.md)).

## Consequences

- The exporter parses `events.xml` only for the opt-in and recording-mark checks. BBB format changes are handled by
  the service's normalizer.
- `events.xml` is sent unfiltered.
