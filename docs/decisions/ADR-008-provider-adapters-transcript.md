# ADR-008 Provider adapters and transcript schema

Status: accepted (2026-10-29), amended 2026-11-11

## Context

Speech-to-text (ASR) servers speak several dialects; language models are reached through OpenAI-style chat
completions ([upstream facts](../reference/upstream-facts.md#providers)). Typical self-hosted choices are
whisperx-api-server and vLLM.

## Options

- a. One OpenAI-compatible adapter each.
- b. A job-shaped ASR interface with one implementation per dialect, one OpenAI-compatible LLM adapter, named
  provider profiles.
- c. transkribierBAR as the only provider.

## Decision

b. Implementations: WhisperX-family ASR (whisperx-api-server), OpenAI-compatible LLM (for example vLLM). The
transcript schema is versioned JSON with integer milliseconds, segments per speaker track, a `source` field (`asr`,
later `live-captions`) and explicit gap markers. A failed track is delivered as a marked gap
([`providers.md`](../reference/providers.md)).

## Consequences

- No vendor SDKs in the core; adapters speak HTTP. SDK-level retries are off; retries live in the job queue.
- A provider profile may declare data location and retention for the privacy documentation.

## Amendment 2026-11-11

The declaration is optional; a profile without one logs a WARN ([`privacy.md` §7](../reference/privacy.md)).
