# Providers: ASR and LLM adapters, provider profiles, transcript schema

Decisions: [ADR-001](../decisions/ADR-001-scope-livekit-bbb4.md), [ADR-008](../decisions/ADR-008-provider-adapters-transcript.md),
[ADR-009](../decisions/ADR-009-privacy-retention.md), [ADR-010](../decisions/ADR-010-postgres-job-queue.md),
[ADR-011](../decisions/ADR-011-blob-storage.md), [ADR-012](../decisions/ADR-012-language-go.md). Names, states, error
classes and gap reasons are defined in [`README.md`](README.md). External facts are collected in
[`upstream-facts.md`](upstream-facts.md).

Scope: the step `transcribe` (ASR adapter), the step `assemble` (transcript) and the LLM calls of `summarize` (prompts
and rendering: [`templates.md`](templates.md)). No adapter imports a vendor SDK. Adapters speak HTTP and parse raw
JSON, because SDKs drop the extra fields that each server dialect uses (ADR-008). Operation names below are
descriptive, not Go identifiers.

Prior-art repositories cited below (read for design only; no code copied):

| Short name            | Repository                                             | Commit           |
| --------------------- | ------------------------------------------------------ | ---------------- |
| transkribierBAR       | https://github.com/virtUOS/transkribierBAR             | `f3f38f3`        |
| Meet                  | https://github.com/suitenumerique/meet                 | `815baa7`        |
| bbb-record-ai-summary | https://github.com/bigbluebutton/bbb-record-ai-summary | `fd1c2cd`        |
| BBB                   | https://github.com/bigbluebutton/bigbluebutton         | `7d27e7e` (v4.0) |

## 1. ASR adapter interface

The interface is job-shaped: `submit`, `status`, `result`, `cleanup`. The shape follows transkribierBAR
`backend/app/services/asr/base.py:26-47 @ f3f38f3` (Apache-2.0; shape only). A synchronous server is wrapped so that
`submit` does the whole request, as in transkribierBAR `backend/app/services/asr/whisperx.py:13-52 @ f3f38f3`.

| Operation         | Input → output                                  | Notes                                                                                                                                               |
| ----------------- | ----------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `capabilities()`  | → `Capabilities`                                | Static per dialect. Read at startup.                                                                                                                |
| `submit(req)`     | `AsrRequest` → `AsrHandle`                      | Synchronous dialect: blocks; the handle holds the result.                                                                                           |
| `status(handle)`  | → `queued` / `running` / `succeeded` / `failed` | Synchronous dialect: no network call.                                                                                                               |
| `result(handle)`  | → `AsrResult`                                   | Parse and normalize only.                                                                                                                           |
| `cleanup(handle)` | → ok / error                                    | Mandatory, idempotent, may be a no-op. Called after `result`, after a final failure, and by the job's `cleanup` step for any handle still recorded. |

`Capabilities`: `async`, `word_timestamps`, `word_confidence`, `diarization` (always requested **off**, ADR-001),
`max_file_bytes` (or null), `media_types`, `remote_retention` (`none` / `ttl` / `until_cleanup`).

### 1.2 `AsrRequest`

One call transcribes **one microphone file** (ADR-001, ADR-004). BBB writes one WebM/Opus file per published audio
track, so one user can have several files ([`upstream-facts.md`](upstream-facts.md), BBB raw recording). The adapter
never sees the meeting timeline; `assemble` applies the offsets (§4.3).

Fields: `job_id`, `track_ref` (correlation); `idempotency_key` = `<job_id>:<track_ref>:<attempt>`, sent as
`X-Request-ID`; `audio` (blob key, size, SHA-256, media type; streamed, never written to local disk); `duration_ms`
(sets the timeout, §1.5); **`language_hint`** = `RECAP_ASR_LANGUAGE` (ISO 639-1). **Default empty: no `language`
field is sent and the provider detects the language per track.** Whisper-family servers use `language` as the
decoding language, so a wrong value hurts more than no value.

### 1.3 `AsrResult`

Times are integer ms **relative to the start of the file**. Float seconds → `round(s × 1000)`, ties away from zero
(transkribierBAR also converts ×1000 to int: `backend/app/services/asr/whisperx.py:113-114 @ f3f38f3`).

Fields: `language` (detected or echoed, else `null`); `segments[]` = `{start_ms, end_ms, text, confidence?, words?}`
with text trimmed, empty segments dropped, `end_ms < start_ms` clamped; `model` (reported, else the profile's).
Provider speaker labels are ignored; the speaker comes from the track (ADR-001). Raw responses are not stored.

### 1.4 Errors

One class per failure; the step outcome per class is in the README table "Adapter error classes → step outcome".
Retries happen only in the job queue, never in an HTTP client (ADR-008; transkribierBAR sets SDK retries to 0 for the
same reason, `backend/app/config.py:33-42 @ f3f38f3`).

| Class              | Typical cause                                                                                                                                      |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `retryable`        | connect error, TLS failure, timeout, HTTP 408, 429, 500, 502, 503, 504                                                                             |
| `permanent_input`  | HTTP 400, 413, 415, 422; file above `max_file_bytes` (Meet treats 400 the same way, `src/summary/summary/core/celery_worker.py:154-160 @ 815baa7`) |
| `permanent_config` | HTTP 401, 403, 404, 3xx and any other unlisted 4xx; model rejected (other unlisted 5xx are `transient`)                                            |
| `invalid_response` | body not JSON, required fields missing, times not numbers                                                                                          |

`Retry-After` (429/503) is honoured, capped at 30 min (`architecture.md` §5.3); the adapter applies the cap and
reports the capped value. Error bodies only select an allow-listed code and are never logged.

### 1.5 Constants (fixed in code, not settings)

| Constant                        | Value                        | Source                                                                                                                                                                                                    |
| ------------------------------- | ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Connect timeout (ASR and LLM)   | 30 s                         | transkribierBAR `backend/app/services/asr/whisperx.py:9 @ f3f38f3`                                                                                                                                        |
| ASR attempt timeout             | `max(600 s, 0.5 × duration)` | pattern from transkribierBAR `backend/app/routers/transcription.py:86-95 @ f3f38f3` and bbb-record-ai-summary `src/scripts/post_archive/transcribe_audio.rb:235-245 @ fd1c2cd`; the values are our choice |
| First ASR retry after a timeout | ≥ 150 s                      | our choice: a timed-out request may still run on the GPU (transkribierBAR `backend/app/config.py:83-86 @ f3f38f3`)                                                                                        |
| LLM request timeout             | 600 s                        | transkribierBAR `LLM_TIMEOUT` default, `backend/app/config.py:32 @ f3f38f3`                                                                                                                               |
| JSON repair attempts            | 2                            | transkribierBAR `backend/app/services/llm/base.py:67-104 @ f3f38f3`                                                                                                                                       |
| Chars per token (estimate)      | 3.0                          | our choice, conservative; no tokenizer call                                                                                                                                                               |
| Budget margin                   | 10 %                         | our choice                                                                                                                                                                                                |
| Max reduce levels               | 3                            | our choice                                                                                                                                                                                                |
| Temperature                     | 0                            | reproducible re-runs                                                                                                                                                                                      |

### 1.6 Progress, idempotency, step outcome

- Each track's `AsrResult` is stored as a job blob as soon as it exists and recorded in `step_items`
  (`architecture.md`). A retried `transcribe` skips tracks that already have a result (ADR-010).
- A track whose attempts are exhausted, or that fails with `permanent_input`, becomes a **gap** (§4.2) and the step
  continues. The job fails with `all_tracks_failed` only when every track is a gap; `permanent_config` fails the job
  (README; ADR-008).
- Concurrency: `max_inflight` per profile, counted per worker process (`architecture.md` §7).

## 2. ASR dialect `whisperx-family` (whisperx-api-server)

The default ASR dialect talks to [whisperx-api-server](https://github.com/Nyralei/whisperx-api-server). Facts come
from its API description (https://raw.githubusercontent.com/Nyralei/whisperx-api-server/main/docs/api.md, read
2026-10-28) and from transkribierBAR's client `backend/app/services/asr/whisperx.py @ f3f38f3` (line numbers below
refer to this file). The server is synchronous: one `POST`, and the response body is the result
(`whisperx.py:126-140`). Its async mode is not used.

### 2.2 Request

`POST {base_url}/v1/audio/transcriptions`, `multipart/form-data` (`whisperx.py:135-136`).

| Part / header       | Value                                                        | Source                                                 |
| ------------------- | ------------------------------------------------------------ | ------------------------------------------------------ |
| `Authorization`     | `Bearer <token>` if the profile has `credentials_file`       | `whisperx.py:128-129`                                  |
| `X-Request-ID`      | `idempotency_key`                                            | `whisperx.py:130-131`                                  |
| `file`              | track audio, file name `<track_ref>.webm`, type `audio/webm` | `whisperx.py:137` (transkribierBAR sends `audio/mpeg`) |
| `model`             | profile `model`                                              | `whisperx.py:81-82`                                    |
| `language`          | `language_hint`; omitted when empty                          | `whisperx.py:83-84`                                    |
| `response_format`   | `verbose_json`                                               | `whisperx.py:89`                                       |
| `align`             | `true`                                                       | `whisperx.py:90`                                       |
| `suppress_numerals` | `false` (server default `true`, upstream API description)    | `whisperx.py:91`                                       |
| `diarize`           | `false`, always                                              | `whisperx.py:92`; ADR-001                              |

The file part's type is the request's media type, and the file name's extension matches it (`.webm` for
`audio/webm`). A media type outside `media_types` (§2.4) is `permanent_input` (code `unsupported_media_type`) and no
request is sent.

Never sent: `min_speakers`, `max_speakers`, `prompt`, `hotwords`, `audio_url`, `callback_url`, `async`
(`whisperx.py:85-97`).

### 2.3 Response mapping

The top level has `segments`, `text`, `language`. `segments` is a list, **or** an object `{segments, word_segments}`
when `align` or diarization is on. Segments have `start`, `end` (float seconds), `text`, `speaker`
(`whisperx.py:107-124`).

| Provider field                    | `AsrResult`          | Rule                                                                                                     |
| --------------------------------- | -------------------- | -------------------------------------------------------------------------------------------------------- |
| `segments` or `segments.segments` | `segments[]`         | Accept both forms; anything else → `invalid_response`.                                                   |
| `start`, `end`                    | `start_ms`, `end_ms` | Missing → `invalid_response` (transkribierBAR defaults to 0; we reject, because a 0 misplaces the text). |
| `text` / `speaker`                | `text` / —           | Trimmed, empty → dropped / ignored.                                                                      |
| `words`, `word_segments`          | —                    | Ignored (shape not documented upstream).                                                                 |
| `language`                        | `language`           | Kept if it matches `^[a-z]{2,3}$`, else `null`.                                                          |

Error bodies have `detail` as a string or a list of `{loc, msg}` (`whisperx.py:54-75`).

### 2.4 Remote retention

The server keeps results on disk, on by default with a 1 h TTL (`RESULT_STORE__ENABLED`, `RESULT_STORE__TTL_SECONDS`;
upstream API description). transkribierBAR recommends disabling it (`README.md:46 @ f3f38f3`). No delete endpoint is
known, so `cleanup` is a no-op and `remote_retention = ttl`. Operators should set `RESULT_STORE__ENABLED=false` on the
server; the profile `declaration` states the actual setting.

Capabilities: `async=false`, `word_timestamps=false`, `word_confidence=false`, `diarization=true` (unused),
`max_file_bytes=null`, `media_types=[audio/webm]`, `remote_retention=ttl`.

## 2a. ASR dialect `transkribierbar` (transkribierBAR job API)

For sites that reach their ASR server only through a transkribierBAR instance. transkribierBAR wraps
whisperx-api-server or MurmurAI behind its own job API (`README.md:46 @ f3f38f3`). Facts come from the transkribierBAR
source @ `f3f38f3` (paths below are under `backend/app/`) and one test against a deployed instance with fixture audio
(2026-11-06). The API is not OpenAI-compatible: there is no `/v1/audio/transcriptions` route; all routes are under
`/api/` (`backend/app/routers/`).

### 2a.1 Flow (inside `Submit`, synchronous to the caller: `async=false`)

All requests carry `Authorization: Bearer <token>` and `X-Request-ID`. The token is a transkribierBAR API token
(prefix `tw_`, issued per user with an expiry of 1–365 days; `README.md` "API access", `dependencies.py:94-111`).
`base_url` is the instance root (e.g. `https://transkribierbar.example.org`); the paths below are appended.

| #   | Request                                                                                                                                                                      | Response used                               | Source                                                |
| --- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------- | ----------------------------------------------------- |
| 1   | `POST /api/upload`, multipart part `file` = track audio, file name `<track_ref>.webm`                                                                                        | `id` (file ID)                              | `routers/upload.py:16,20`; `models.py:19-24`          |
| 2   | `POST /api/transcribe`, JSON `{"file_id", "model", "language"?}`; `language` only with a `language_hint`; never `min_speakers`, `max_speakers`, `initial_prompt`, `hotwords` | `id` (transcription ID), `status` `pending` | `routers/transcription.py:40-100`; `models.py:27-34`  |
| 3   | `GET /api/status/{id}` every 5 s until `completed` or `failed`                                                                                                               | `status`, `error_code`                      | `routers/transcription.py:279-291`; `models.py:45-52` |
| 4   | `GET /api/transcription/{id}`                                                                                                                                                | `utterances[]`, `language`                  | `routers/transcription.py:305-330`; `models.py:55-68` |

The poll interval of 5 s is our choice. The whole flow runs within the ASR attempt timeout (§1.5).

The uploaded file part's type is the request's media type, and the file name's extension matches it (`.webm` for
`audio/webm`); transkribierBAR accepts or rejects a file by its extension (`routers/upload.py:16,26-30`). A media type
outside `media_types` (§2a.4) is `permanent_input` (code `unsupported_media_type`) and no request is sent.

### 2a.2 Response mapping

`utterances[]` = `{start, end, text, speaker}` with `start`, `end` as integer **ms** (`models.py:55-59`; the test
instance returned 813–2340 for fixture speech at 800 ms) → `segments[]` `{start_ms, end_ms, text}`; text trimmed,
empty dropped, `speaker` ignored (ADR-001). Missing or non-integer times → `invalid_response`. `language` is kept if it
matches `^[a-z]{2,3}$`, else `null` (the test instance returned `null`). `model` = the profile's.

### 2a.3 Errors

HTTP status of any request as in §1.4, except: a `422` from step 2 whose `detail` is a string containing
`is not available on this instance` (`router_helpers.py:12`; matched on this wording) → `permanent_config` (code
`model_unavailable`); any other `422` → `permanent_input`. A `failed` status maps by `error_code`
(`routers/transcription.py:102-114`): `asr_unreachable`, `timeout`, `server_restarted` → `retryable`; `asr_failed` →
`permanent_input`; anything else → `retryable` (code `remote_failed`). An unknown `status` value → `invalid_response`.

### 2a.4 Cleanup and remote retention

`Cleanup` sends `DELETE /api/transcription/{id}`. It deletes the transcription and its uploaded file (on the test
instance, status and media returned 404 afterwards; `routers/transcription.py:438-490`). 404 counts as done.
`remote_retention = until_cleanup`.

transkribierBAR has no route to delete an uploaded file on its own (the file routes are upload, media, media fallback
and rename: `routers/upload.py:20,105,121,139`). A file whose transcription could not be started (step 2) is therefore
reused: when step 2 fails with `retryable` or `invalid_response`, the adapter keeps the file ID in memory for 24 h,
per job, track and audio SHA-256, and the track's next attempt in the same worker process starts the transcription on
that file instead of uploading it again. If the instance answers 404 for the kept file
(`routers/transcription.py:53`), the track is uploaded again. Once the transcription exists, `Cleanup` deletes it with
the file. Otherwise transkribierBAR's own expiry removes the file: `expires_at` is set at upload to
`DEFAULT_EXPIRY_HOURS` later (default 72 h; `routers/upload.py:79`, `config.py:102`), and an hourly task deletes
expired files that no pending or processing transcription uses (`main.py:150-163`). See Known limitations.

Capabilities: `async=false`, `word_timestamps=false`, `word_confidence=false`, `diarization=false`,
`max_file_bytes` 1 GiB (`routers/upload.py:17,49`), `media_types=[audio/webm]`, `remote_retention=until_cleanup`.

## 3. LLM adapter (OpenAI-compatible chat completions, vLLM)

### 3.1 Operation

`complete(LlmRequest) → LlmResponse | LlmError`.

`LlmRequest`: `messages[]` (`system`, `user`; `assistant` only for JSON repair), `output` (`text`, or `json` with a
draft 2020-12 schema owned by the format, `templates.md`), `max_output_tokens` (≤ profile value), `call_ref`
(`<job_id>:<step>:<call-name>:<chunk>`, logs only). `LlmResponse`: `text` (raw `choices[0].message.content`), `json`
(parsed, validated), `finish_reason`, `usage`, `model`.

### 3.2 Wire format

`POST {base_url}/chat/completions`, `Authorization: Bearer <token>`, body
`{model, messages, max_tokens, temperature: 0, stream: false, response_format?}` merged with the profile's
`extra_body`. transkribierBAR, Meet and bbb-record-ai-summary all use this shape. `base_url` includes `/v1` (Meet's
default is `https://api.openai.com/v1`). `extra_body` covers model quirks without code, e.g. vLLM
`chat_template_kwargs: {enable_thinking: false}` to turn off reasoning output (transkribierBAR
`backend/app/services/llm/openai.py:35-53 @ f3f38f3`).

### 3.3 Structured output (profile field `structured_output`)

| Mode          | Request                                                                             | Prior art                                                                                                    |
| ------------- | ----------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `json_schema` | `response_format: {type: "json_schema", json_schema: {name, schema, strict: true}}` | Meet `src/summary/summary/core/prompt.py:24-70`, `src/summary/summary/core/llm_service.py:129-130 @ 815baa7` |
| `json_object` | `response_format: {type: "json_object"}`; schema in the system message              | transkribierBAR `backend/app/services/llm/openai.py:65 @ f3f38f3`                                            |
| `prompt`      | no `response_format`; schema in the system message                                  | bbb-record-ai-summary `src/ai-summary/process/ai-summary.rb:1094-1110 @ fd1c2cd`                             |

The adapter (not the template) puts the schema into the system message. `structured_output` and `max_inflight` are
optional profile keys (defaults `json_object` and `1`). In every mode the adapter strips one surrounding Markdown code
fence, parses the content and validates it. It also rejects a **schema echo** (the top level has `$schema`, or `type`
plus `properties`), as transkribierBAR does (`reject_schema_echo`, `backend/app/services/llm/base.py:368-384 @ f3f38f3`).

### 3.4 Errors, truncation, repair

| Class              | Cause                                                                                                                             |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| `retryable`        | transport error, timeout, 408, 429, 500, 502, 503, 504                                                                            |
| `permanent_config` | 401, 403, 404, other 400                                                                                                          |
| `input_too_large`  | 400 on a call whose estimated input is > 80 % of `context_tokens` (the error body is not parsed)                                  |
| `output_truncated` | `finish_reason = "length"`; never parsed or repaired (as in transkribierBAR `backend/app/services/llm/openai.py:71-76 @ f3f38f3`) |
| `invalid_response` | no `choices`, a `finish_reason` other than `stop`, or JSON repair exhausted                                                       |

JSON repair: on invalid JSON, a schema violation or a schema echo, up to 2 more calls with the previous reply as
`assistant` and a fixed `user` message naming the validator error path (JSON pointer and rule, never content).
Repair calls are not queue retries. Outcomes per class: README table. `input_too_large` and `output_truncated` are
handled by one re-split (§3.5), then permanent.

INFO log per call: IDs, profile, model, status, class, latency, usage, `finish_reason`; never prompts or replies.

### 3.5 Long input: chunking

Prior art chunks by fixed character counts (transkribierBAR 100k, bbb-record-ai-summary 48k); Meet sends the whole
transcript on every call, which does not scale to long meetings. bbb-recap uses a token budget and map-reduce:

1. **Budget** = `context_tokens − max_output_tokens − prompt tokens − 10 % of context_tokens`; tokens ≈
   `ceil(chars / 3.0)`. `context_tokens` is declared in the profile, not asked from the provider. **Single call** if
   the input fits.
2. **Map:** otherwise cut the transcript lines into consecutive chunks ≤ budget, only between segments (a longer
   segment is cut at the last sentence end or space). No overlap. Chat, notes and polls go only to the reduce call.
3. **Reduce:** the map outputs, in chunk order, go into one reduce call; if they exceed the budget, reduce in groups,
   at most 3 levels, else the step fails with `input_too_large`.
4. **Re-split:** on `output_truncated` or `input_too_large` in a map call, split that chunk in half once. Map calls
   run concurrently up to `max_inflight`; the order of results is deterministic.

## 4. Provider profiles and transcript

### 4.1 Provider profiles (`RECAP_PROVIDERS_FILE`)

YAML, because profiles are nested. There is one default ASR and one default LLM profile; choosing a profile per
meeting type is not supported yet. Names follow the README grammar. Fixture values:

```yaml
schema_version: "1.0.0"
defaults: { asr: whisperx-lab, llm: vllm-lab }
profiles:
  - name: whisperx-lab
    kind: asr # asr | llm
    dialect: whisperx-family # asr: whisperx-family | transkribierbar; llm: openai-chat
    base_url: https://asr.recap.test
    model: large-v3
    credentials_file: /run/secrets/recap/providers/asr-fixture.key # optional
    max_inflight: 1
    max_file_bytes: null # optional; overrides the dialect capability (§1), null = no limit
    declaration: {
        data_categories: [voice],
        operator: "operator's computing centre",
        location: on_prem, # optional,
        retention: "No persistence; RESULT_STORE__ENABLED=false",
        deletion: manual, # see privacy.md
        used_for_training: "no",
        contract_ref: none,
        reviewed_by: fixture-reviewer,
        reviewed_at: "2026-10-29",
      }
  - name: vllm-lab
    kind: llm
    dialect: openai-chat
    base_url: https://llm.recap.test/v1
    model: example-model
    credentials_file: /run/secrets/recap/providers/llm-fixture.key
    context_tokens: 32768
    max_output_tokens: 4096
    structured_output: json_object # json_schema | json_object | prompt
    extra_body: { chat_template_kwargs: { enable_thinking: false } } # optional
    max_inflight: 4
    declaration:
      {
        data_categories: [transcript, chat, summary],
        operator: "operator's computing centre",
        location: on_prem,
        retention: "No request logging (operator to confirm)",
        deletion: none,
        used_for_training: "no",
        contract_ref: none,
        reviewed_by: fixture-reviewer,
        reviewed_at: "2026-10-29",
      }
```

Rules: the file is validated in one pass at startup (`check-config`). Unknown keys, unknown dialects, an incomplete
`declaration` (when present; a missing one only logs a WARN, `privacy.md` §7), an unreadable `credentials_file` or a
bad `defaults` entry stop the service. Extra CA certificates come from the standard `SSL_CERT_FILE` environment
variable, not from the profile. Credentials stay in memory and are never logged. Each provider call records profile,
dialect and reported model as provenance.

### 4.2 Transcript schema (step `assemble`)

[`schemas/transcript.schema.json`](schemas/transcript.schema.json), `$id` `urn:recap:schema:transcript:1`,
`schema_version` `1.0.0`.

| Field                              | Meaning                                                                                                                                                                                                                |
| ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `source`                           | `asr`. The value `live-captions` is reserved for BBB 4.1 `CaptionUpdatedEvent` ([`upstream-facts.md`](upstream-facts.md)); not produced yet.                                                                           |
| `language`                         | Language with the most transcribed speech ms over all tracks; `null` if no track reported one.                                                                                                                         |
| `time_base`, `recording_periods[]` | §4.3.                                                                                                                                                                                                                  |
| `speakers[]`                       | `{ref, display_name?, role?}`, one per BBB user with at least one microphone track, sorted by `ref`.                                                                                                                   |
| `tracks[]`                         | `{ref, speaker_ref, kind: microphone, blob_sha256, start_ms, end_ms, status, language?, provider?}`, sorted by `ref`; `status` `transcribed`, `empty`, `failed`. A `failed` track has no `language` and no `provider`. |
| `segments[]`                       | `{id, track_ref, speaker_ref, start_ms, end_ms, text, confidence?, words?}`, sorted by `start_ms`, then `track_ref`. Overlap between speakers is allowed.                                                              |
| `gaps[]`                           | `{track_ref, speaker_ref, start_ms, end_ms, reason}`, span = track span, `reason` = README gap reason; sorted by `start_ms`, then `track_ref`.                                                                         |

Refs: `spk-<n>`, `trk-<n>` (numbered by first appearance), `seg-<6+ digits>`. Rules beyond the schema (checked by
CT-TR-01): `end_ms ≥ start_ms`; every ref resolves; a segment's speaker equals its track's; each gap's track has
`status = failed`; periods are sorted and do not overlap. The schema file is strict (`additionalProperties: false`);
consumers ignore unknown fields so minor versions can add optional ones.

Speakers: `userId`s with a rejoin suffix `_N` are grouped after stripping `/_\d*$/`, as BBB does
(`record-and-playback/core/lib/recordandplayback/generators/events.rb:36-43 @ 7d27e7e`). `display_name` and `role`
come from `ParticipantJoinEvent`. BBB `userId` and `externalUserId` are not stored (data minimization). Without a
join event, renderers say "Speaker n".

### 4.3 Time base, marks and pauses

Facts (BBB @ `7d27e7e`): events carry a ms `timestamp` and a `timestampUTC`
(`akka-bbb-apps/src/main/scala/org/bigbluebutton/core/record/events/RecordEvent.scala:34-41,85-91`). BBB computes
offsets into a file as `event.timestamp − AudioTrackPublishedEvent.timestamp`
(`record-and-playback/core/lib/recordandplayback/generators/audio.rb:50-104`), which implies one shared clock.
`RecordStatusEvent` with `status = "true"` starts recording, anything else stops it; BBB's caption tool collapses
pauses into one playback timeline (`record-and-playback/core/scripts/utils/gen_webvtt:111-150,340-346`).

1. **Axis.** `*_ms` = `timestamp − origin_event_ts`; the origin is the first `RecordStatusEvent` with
   `status = "true"`; `origin_utc` = its `timestampUTC`.
2. **Periods.** Start/stop pairs give `{start_ms, end_ms, recorded_offset_ms}`; a start without a stop ends at the
   last event. `recorded_offset_ms` = sum of earlier period lengths. **Recorded time** = `t − period.start_ms +
period.recorded_offset_ms`. Pauses are not gaps.
3. **Track offset.** `track.start_ms` = matching `AudioTrackPublishedEvent.timestamp − origin` (matched by file
   basename, `record-and-playback/core/lib/recordandplayback/generators/events.rb:191-236 @ 7d27e7e`); `end_ms` from
   `AudioTrackUnpublishedEvent`, else start + decoded duration.
4. **Segment offset.** `track.start_ms + asr.start_ms` (same for end and words). Tests allow 1000 ms tolerance
   (see Known limitations).
5. **Clipping.** Segments whose midpoint lies outside every period are dropped. Without
   `recordFullDurationMedia=true` no segment should fall there, so this is a safety net. Audio is not cut to the
   recording periods before ASR.
6. A package without a recording start is rejected by `validate` (ADR-002).

Assembly is deterministic (byte-identical JSON; `created_at` = the job's `created_at`). A track with a result but no
segments gets `status = empty`.

Gaps follow the README exactly: a gap never fails the job on its own; the job fails with `all_tracks_failed` when every
track is a gap. The summary prompt is told which speakers have gaps; `summary.md` and `transcript.vtt` show them
(`templates.md` §5, §6).

## 5. Data flows

Each flow states the personal data, where it is stored, and who deletes it when.

| Flow          | Personal data                                                                                                                                                | Stored where                                                                        | Retention / deleter                                                                                                                                                                               |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Service → ASR | Voice of one participant per call; language hint. The upload name is `<track_ref>.webm`, so the BBB `userId` in the original file name stays in the service. | Audio and result blobs under `jobs/<job_id>/` (ADR-011); provider per `declaration` | Until the job's `cleanup` (default right after delivery; held or failed jobs until an admin acts, ADR-009). Remote: adapter `cleanup`, or operator config where the dialect cannot delete (§2.4). |
| Service → LLM | Transcript with speaker names, chat with sender names, notes, poll aggregates, meeting name (`templates.md` §3). No user IDs, email or owner identity.       | Map outputs and summary JSON as job blobs; provider per `declaration`               | Service `cleanup`; provider side: the operator.                                                                                                                                                   |
| Transcript    | Spoken words, display names, roles, speech timings                                                                                                           | Job blob; rendered to `transcript.vtt`, which is **not delivered** (ADR-007)        | Service `cleanup`.                                                                                                                                                                                |

## 6. Contract tests

Fixtures only; no real provider. A local fake replays fixture responses.

| ID              | Test                                                                                                                                                                                                                                                                          |
| --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-PROV-01      | whisperx request: method, path, multipart fields of §2.2, `Authorization`, `X-Request-ID`, file name `<track_ref>.webm`; no `language` field by default, `language=<code>` only when `RECAP_ASR_LANGUAGE` is set; never `diarize=true`, speaker counts, `prompt`, `hotwords`. |
| CT-PROV-02      | whisperx list-form response → expected `AsrResult` (rounding incl. ties, trim, empty drop, speaker ignored).                                                                                                                                                                  |
| CT-PROV-03      | whisperx object-form response → same `AsrResult` as CT-PROV-02.                                                                                                                                                                                                               |
| CT-PROV-04      | ASR status codes, connect refusal, timeout, non-JSON → class per §1.4; `Retry-After` parsed; error body never in logs or stored errors.                                                                                                                                       |
| CT-PROV-05 / 06 | Segment without `start` → `invalid_response` / `cleanup` callable twice in any state; `status`, `result` make no network call.                                                                                                                                                |
| CT-PROV-07      | LLM request body: `model`, `messages`, `max_tokens`, `temperature: 0`, `stream: false`, `response_format` per mode, `extra_body` merged.                                                                                                                                      |
| CT-PROV-08      | Structured output: valid → parsed; fenced → parsed; invalid then valid → 1 repair; always invalid → `invalid_response` after 2 repairs; schema echo → repair.                                                                                                                 |
| CT-PROV-09 / 10 | `finish_reason: "length"` → `output_truncated`, no repair call / LLM status codes → classes of §3.4.                                                                                                                                                                          |
| CT-PROV-11      | Chunker: boundaries between segments, no chunk over budget, deterministic, long-segment cut, one re-split.                                                                                                                                                                    |
| CT-PROV-12      | Profiles: valid fixture loads; each missing required key, unknown key, incomplete `declaration` (when present), bad `defaults`, bad name → startup error naming the key; a missing `declaration` loads with a WARN.                                                           |
| CT-TR-01        | Golden transcripts validate against the schema and the rules of §4.2; invalid fixtures (float or negative times, unknown `source`, missing `gaps`, extra field) fail.                                                                                                         |
| CT-TR-02        | Normalized events + fixture `AsrResult`s → golden transcript: offsets, `recorded_offset_ms`, `_N` grouping, clipping, gaps; byte-identical on re-run.                                                                                                                         |
| CT-TR-03        | Log capture over CT-TR-02: no segment text or display name at INFO or above. CT-PROV-02 and CT-PROV-08 run the same scan on their own runs (segment and prompt text).                                                                                                         |

CT-PROV-01..03, CT-PROV-09, CT-TR-01 and CT-TR-02 were written before the implementation and are not changed to fit it.

## 7. Known limitations

- **Microphone file without a publish event.** A microphone file without a matching `AudioTrackPublishedEvent`
  (validation rule V-S05, fixture M-11) is not transcribed. It becomes a gap `track_unreadable`; the speaker is the
  `userId` in its file name; the span runs from the file-name epoch suffix minus the origin to that plus the decoded
  duration. No CT-TR case covers it yet.
- **Long tracks.** whisperx-api-server's async mode is not used; a long track must finish within one request.
- **Media type.** WebM/Opus is sent as is. It worked with whisperx-api-server 1.4.1.0 in a test run; other versions
  and other servers are not checked. If a server refuses it, `normalize` would need to transcode.
- **Response shape.** The full whisperx-api-server `verbose_json` schema (words, `word_segments`) is not documented
  upstream; word-level data is ignored. The list of error body forms (§2.3) may be incomplete.
- **Remote retention.** whisperx-api-server has no known endpoint to delete a stored result; operators must turn the
  result store off (§2.4). transkribierBAR cannot delete an uploaded file on its own (§2a.4): if step 2 of §2a.1
  fails and the track is not retried in the same worker process (a permanent error, the track's last attempt, a
  worker restart, or the retry runs on another worker), the file stays on the instance until transkribierBAR's expiry
  removes it, by default 72 h after the upload (`DEFAULT_EXPIRY_HOURS`, checked hourly).
- **Clock and skew.** That BBB's event clock and the audio publish events share one clock is implied by BBB's code,
  not read in source. The skew between a WebM file's start and its publish event, and whether muted stretches are kept
  in the file, are not measured; tests allow 1000 ms.
- **Strict JSON schema.** Whether a given vLLM version accepts `json_schema` with `strict: true` is not checked; the
  default is `json_object`.
- **vLLM errors.** vLLM's error body for an over-long input is not parsed; `input_too_large` is inferred from the
  input estimate (§3.4).
- **Provider logging.** Whether an LLM server logs requests depends on its configuration; the operator states it in
  the profile `declaration`.
- **Cutting audio to recording periods** before ASR is not done; with `recordFullDurationMedia=true` audio outside the
  periods is transcribed and then dropped by clipping (§4.3).
