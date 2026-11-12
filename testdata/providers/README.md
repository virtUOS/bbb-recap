# testdata/providers

Mock provider responses and profiles for the provider contract tests (`contracts/providers/`).
All values are synthetic. Nothing here was recorded from a real provider.

**Spec-derived, not recorded.** Files named `*.spec-derived.json` are built only from the fields that
`docs/reference/providers.md` states. The real whisperx-api-server `verbose_json` response has not been captured yet
from a real server. When it is, a recorded response replaces the whisperx files, and the tests marked
`provisional` are reviewed.

| File | Content | Source |
|---|---|---|
| `profiles/providers.yaml` | provider profiles: the fixture values of §4.1 in block style (one key per line) | `providers.md` §4.1, `privacy.md` §7 |
| `whisperx/list-form.spec-derived.json` | `verbose_json` reply with `segments` as a list | `providers.md` §2.3 (top level `segments`, `text`, `language`; segments `start`, `end`, `text`, `speaker`) |
| `whisperx/object-form.spec-derived.json` | the same segments in the object form `{segments, word_segments}` | `providers.md` §2.3; `word_segments` is empty because its shape has not been checked against a real server |
| `whisperx/result.json` | the `AsrResult` both replies map to | `providers.md` §1.3, §2.3 |
| `llm/chat-completion.spec-derived.json` | chat completion reply; tests fill `content` and `finish_reason` | `providers.md` §3.1, §3.2 (`choices[0].message.content`, `finish_reason`, `usage`, `model`). `usage` fields follow the OpenAI shape that transkribierBAR reads (`backend/app/metrics.py:247-248 @ f3f38f3`); vLLM's fields were not checked and no test pins them |
| `llm/schema.json` | small draft 2020-12 output schema for the structured-output tests | test-only; not the `summary.v1` schema of `templates.md` |

Values chosen on purpose in the whisperx replies:

- Times are dyadic fractions (`6.0625`, `7.5625`, `10.0078125`, `12.4921875`), exact in binary floating point, so
  `round(s × 1000)` has one right answer: `6062.5` and `7562.5` are ties (away from zero: `6063`, `7563`).
- Segment texts have surrounding spaces (trimmed), one segment is only spaces (dropped), and one has `end < start`
  (`9.5` / `9.0`, clamped to `end_ms = start_ms`).
- Every segment carries a `speaker`, which the result must not contain.
- Texts are lines of the F-01 dialogue (`docs/reference/fixtures.md` §7.1).
