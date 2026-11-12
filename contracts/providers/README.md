# Contract tests: provider adapters

Black-box tests for `docs/reference/providers.md` §6 (provider rows). They drive `recap tool asr` and `recap tool llm`
from `RECAP_BIN` against fake provider servers that the tests start on `127.0.0.1` (HTTPS with a test certificate).
Without `RECAP_BIN` every test skips and names it.

Tests whose name ends in `(provisional)` depend on the whisperx-api-server `verbose_json` shape, which has not been
captured yet from a real server. Their fixtures are spec-derived (`testdata/providers/README.md`). They are reviewed
when a recorded response replaces the fixture.

## Environment of the tool

Both tools are dev/test only (`docs/reference/README.md` "Configuration, processes, metrics") and read only:

| Variable               | Use                                                                                        |
| ---------------------- | ------------------------------------------------------------------------------------------ |
| `RECAP_PROVIDERS_FILE` | provider profiles (`providers.md` §4.1). Required                                          |
| `RECAP_ASR_LANGUAGE`   | `language_hint` of every ASR request (§1.2). Empty or unset: no `language` field           |
| `RECAP_LOG_LEVEL`      | log level; the tests set `debug`                                                           |
| `SSL_CERT_FILE`        | CA bundle for HTTPS (`deployment.md`); the tests point it at the fake server's certificate |

The tests start the tool with no other `RECAP_*` variable, with every `*_PROXY` / `*_proxy` variable removed and
`NO_PROXY=*` / `no_proxy=*` set, so the local fakes are always reached directly.

**Start.** The tool first loads and validates the whole providers file with the service's startup rules (§4.1: unknown
keys, unknown dialects, incomplete `declaration` (when present), unreadable `credentials_file`, bad `defaults` entry,
bad name, missing required key). On any error it exits 2, writes nothing to stdout, and names each offending key on
stderr. Keys the tests treat as required: top level `schema_version`, `defaults`, `profiles`; every profile `name`,
`kind`, `dialect`, `base_url`, `model`; LLM profiles also `context_tokens`, `max_output_tokens`; a `declaration` when
present (all nine fields of `privacy.md` §7). Optional: `declaration` (absent: the file loads and stderr has exactly
one WARN line naming the profile), `credentials_file`, `extra_body`, `structured_output` (default `json_object`),
`max_inflight` (default `1`) (`providers.md` §3.3), and `max_file_bytes` on ASR profiles (integer; overrides the
dialect's capability, null for whisperx-family).

**Logs** go to stderr as JSON lines with a `level` field. Never at any level: provider error bodies, credentials,
`Authorization` headers (`privacy.md` §6). Never at INFO or above: segment text, prompt text or reply text
(`README` "Logging", `providers.md` §6). The tests scan the tool's stderr directly (not `RECAP_CONTRACT_LOG_FILE`): a
line counts as INFO or above unless it is a JSON object whose `level` is `debug` (any case).

**Exit codes.** 0 when the operations ran, including adapter errors (they are in the output). 1 on a runtime error
outside the adapter (for example the audio file is unreadable). 2 on a usage or configuration error. On 1 and 2
stdout is empty and stderr says why.

Readers ignore output fields they do not know.

## `recap tool asr`

One JSON object on stdin:

```json
{
  "profile": "whisperx-lab",
  "request": {
    "job_id": "0190f5a0-0000-7000-8000-000000000001",
    "track_ref": "trk-1",
    "idempotency_key": "0190f5a0-0000-7000-8000-000000000001:trk-1:1",
    "audio": { "path": "/abs/path/file.webm", "media_type": "audio/webm", "size": 93076, "sha256": "<64 hex>" },
    "duration_ms": 9000
  },
  "operations": ["submit", "status", "result", "cleanup"],
  "attempt_timeout_ms": 1000
}
```

| Field                | Meaning                                                                                                                                                                                                                                                  |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `profile`            | ASR profile name; absent: `defaults.asr`                                                                                                                                                                                                                 |
| `request`            | `AsrRequest` (§1.2). `audio.path` is a local file in place of the blob key. `language_hint` comes from `RECAP_ASR_LANGUAGE`. May be absent when `operations` is empty                                                                                    |
| `operations`         | adapter operations (§1) run in this order on one handle: `submit`, `status`, `result`, `cleanup`. Every listed operation runs, also after an earlier one failed. Empty list: only load the profiles (start check above), then print `{"operations": []}` |
| `attempt_timeout_ms` | dev/test only: replaces the ASR attempt timeout `max(600 s, 0.5 × duration)` of §1.5, so a timeout can be tested quickly. Absent: the §1.5 value                                                                                                         |

Output, one JSON object on stdout, one entry per requested operation:

```json
{
  "operations": [
    { "op": "submit", "error": null },
    { "op": "status", "status": "succeeded", "error": null },
    {
      "op": "result",
      "result": {
        "language": "en",
        "model": "large-v3",
        "segments": [{ "start_ms": 1000, "end_ms": 5500, "text": "…" }]
      },
      "error": null
    },
    { "op": "cleanup", "error": null }
  ]
}
```

- `status`: `queued`, `running`, `succeeded` or `failed` (only for `status`).
- `result`: the `AsrResult` of §1.3 (only for `result`, `null` on error). Integer ms; no `speaker`.
- `error`: `null` or `{"class": "retryable" | "permanent_input" | "permanent_config" | "invalid_response",
"code": "<allow-listed code>", "retry_after_ms": <integer> | null}`. `code` is not pinned by the tests.
  `retry_after_ms` is the parsed `Retry-After` header (delta-seconds form) when the reply had one, capped at
  30 min (1 800 000) by the adapter (§1.4).
- A file above the profile's `max_file_bytes` fails `submit` with `permanent_input` without a request.
- For the synchronous whisperx-family dialect a failure may show up at `submit`, `status` or `result`; the tests
  read the first error in operation order.

## `recap tool llm complete`

One JSON object on stdin:

```json
{
  "profile": "vllm-lab",
  "request": {
    "messages": [
      { "role": "system", "content": "…" },
      { "role": "user", "content": "…" }
    ],
    "output": { "type": "json", "schema": { "type": "object" } },
    "max_output_tokens": 1024,
    "call_ref": "0190f5a0-0000-7000-8000-000000000001:summarize:summarize:0"
  }
}
```

`request` is the `LlmRequest` of §3.1; `output` is `{"type": "text"}` or `{"type": "json", "schema": <draft 2020-12
schema>}`. `profile` absent: `defaults.llm`. The structured-output mode comes from the profile (§3.3). In
`json_object` and `prompt` modes the adapter adds the schema to the system message; in `json_schema` mode it sends it in
`response_format`. JSON repair
calls (§3.4) happen inside one run; there are no other retries.

Output: `{"response": <LlmResponse> | null, "error": <error as above> | null}`, exactly one of them non-null.
`LlmResponse` = `{"text": <raw content>, "json": <parsed value or null>, "finish_reason": "stop", "usage": {…},
"model": "<reported>"}`; `model` is the model the provider reports in its reply, not the profile's. Error classes: `retryable`, `permanent_config`, `input_too_large`, `output_truncated`,
`invalid_response`.

## `recap tool llm chunk`

The map cut of §3.5 step 2 and the re-split of step 4. No provider call. Stdin:

```json
{ "lines": ["…", "…"], "budget_tokens": 50, "resplit": [1] }
{ "lines": ["…", "…"], "profile": "vllm-lab", "prompt_chars": 3000 }
```

- `lines`: transcript lines in order; one line is one segment. Opaque strings.
- `budget_tokens`: the budget of §3.5 step 1, already computed. Tokens of a chunk = `ceil(chars / 3.0)` of its lines
  joined with `\n`.
- Or, instead of `budget_tokens`: `profile` (LLM profile; absent: `defaults.llm`) and `prompt_chars` (characters of the
  prompt around the transcript). The tool then computes `budget_tokens = context_tokens − max_output_tokens −
ceil(prompt_chars / 3.0) − 10 % of context_tokens` with the profile's values (§3.5 step 1).
- `resplit` (optional): indexes into the chunk list the same input produces without `resplit`; each named chunk is
  split in half once (step 4).

Output: `{"budget_tokens": <used budget>, "chunks": [{"lines": ["…"]}, …]}`. A line that does not fit the budget is cut into pieces (at the last
sentence end, else the last space, within the budget); a piece is an element of `lines`.

## Fixtures

Only `testdata/` (`testdata/providers/README.md`): profiles, spec-derived whisperx replies and their `AsrResult`, a
chat completion template, an output schema, F-01's first microphone file as audio, and the fixture keys
`testdata/secrets/asr-fixture.key` and `llm-fixture.key`. The tests write the profiles file (with fake server URLs
and absolute key paths) and the test certificate to temporary directories.

## Run

```sh
make contracts                                       # all skip
make build && RECAP_BIN=./bin/recap make contracts
```
