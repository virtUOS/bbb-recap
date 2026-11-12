# ADR-001 Scope: BBB 4.0+ with LiveKit only

Status: accepted (2026-10-29)

## Context

Speaker attribution relies on one audio file per microphone track. Only the LiveKit audio bridge records that;
the other bridges (FreeSWITCH, SFU) record one mixed file ([upstream facts](../reference/upstream-facts.md#bbb-raw-recording)).

## Options

1. Support all audio bridges (needs diarization and speaker assignment).
2. LiveKit only.

## Decision

Option 2: BBB 4.0 or later with the LiveKit audio bridge. Recordings without per-track microphone files are rejected
at validation.

## Consequences

- No diarization. A room microphone shared by several people stays one "speaker".
- The validator detects non-LiveKit raw recordings and fails clearly (`unsupported_audio_bridge`,
  [`meeting-package.md` §6](../reference/meeting-package.md)).
