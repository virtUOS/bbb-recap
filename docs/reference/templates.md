# Templates: built-in output format and later admin templates

Decisions: [ADR-005](../decisions/ADR-005-metadata-interface.md), [ADR-007](../decisions/ADR-007-docs-sink-bot-user.md),
[ADR-008](../decisions/ADR-008-provider-adapters-transcript.md), [ADR-009](../decisions/ADR-009-privacy-retention.md),
[ADR-012](../decisions/ADR-012-language-go.md). Names come from [`README.md`](README.md). LLM calls go through
[`providers.md`](providers.md) §3. External facts are collected in [`upstream-facts.md`](upstream-facts.md); prior-art
repositories are listed at the top of `providers.md`.

Scope: the steps `summarize` (LLM) and `render`. `render` writes two blobs: **`summary.md`** (delivered to the sink)
and **`transcript.vtt`** (WebVTT, not delivered; README "Outputs"). Delivery is described in [`sinks.md`](sinks.md).

## 1. Selection by meeting type

- The meeting type is the value of `meta_recap-type` (ADR-005). Names follow the README name grammar.
- Formats are data files under `RECAP_FORMATS_DIR` (ADR-012). One format ships: `meeting-summary`. In the repository
  formats live under `formats/`; the image copies them to the default `RECAP_FORMATS_DIR`.
- The type → format mapping is the meeting-types file `RECAP_MEETING_TYPES_FILE` (YAML, a list). Default and fixture
  file:

  ```yaml
  schema_version: "1.0.0"
  meeting_types:
    - name: meeting-summary # value of meta_recap-type
      format: meeting-summary # format ID under RECAP_FORMATS_DIR
  ```

- A type not in the file, or a type whose format is missing → job `held/unknown_type` in `validate`, before any
  provider call (ADR-005). After the operator fixes the file, an admin `release`s the job.
- Each job records `format_id`, `format_version` and the SHA-256 of the format files, so a result can be traced to
  the exact format.

## 2. Format `meeting-summary` v1.0.0

Inputs (§3) → the LLM returns JSON (§4) → a deterministic renderer writes `summary.md` (§5) and `transcript.vtt` from
the transcript (§6). JSON plus a renderer keeps action items structured and lets the renderer escape all content.
(bbb-record-ai-summary, which asks for Markdown directly, handles action items on a separate JSON path:
`src/ai-summary/process/ai-summary.rb:1097-1107 @ fd1c2cd`.)

## 3. Inputs

Only declared inputs reach the LLM. A missing optional input means its section is left out.

| Input        | Source                                                                         | Sent to LLM                         | In `summary.md`  | Notes                                                                                                                                                                                                                            |
| ------------ | ------------------------------------------------------------------------------ | ----------------------------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Transcript   | `providers.md` §4.2                                                            | yes, lines `[hh:mm:ss] Name: text`  | no               | Recorded time (§5.3). Same line form as transkribierBAR `backend/app/services/llm/prompt.py:74-92 @ f3f38f3`. Name = `display_name` or "Speaker n".                                                                              |
| Gaps         | transcript `gaps[]`                                                            | yes, speaker and span               | yes, notice      |                                                                                                                                                                                                                                  |
| Public chat  | `PublicChatEvent` (HTML); edits applied, deleted messages dropped, HTML → text | yes, `[hh:mm:ss] Name: text`        | no               | Only messages inside recording periods; chat during pauses is left out. A public chat clear removes all earlier public messages from the input (the moderator's intent); a deleted message is dropped together with its replies. |
| Polls        | `Poll*RecordEvent`s                                                            | question, options, aggregate counts | yes, table       | Never per-user answers. Only published polls inside a recording period; a changed answer counts once, as the user's last answer.                                                                                                 |
| Shared notes | `notes/notes.md`, else `notes/notes.html` as text; `export_failed` = absent    | yes                                 | no               | BBB exports notes as html and pdf by default ([`upstream-facts.md`](upstream-facts.md)). `notes.txt` is not an input.                                                                                                            |
| Meeting name | `events.xml` `<meeting name>`                                                  | yes                                 | title and header | May contain personal data.                                                                                                                                                                                                       |
| Times        | `time_base.origin_utc`, `recording_periods[]`                                  | no                                  | header           | Shown in `RECAP_DISPLAY_TZ` (IANA name, default `UTC`).                                                                                                                                                                          |

Not used: slide titles, the attendee list.

Content fencing: meeting content goes into the `user` message inside delimited data sections; the `system` message
says these sections are data, not instructions (the technique is also used by bbb-record-ai-summary
`src/ai-summary/process/ai-summary.rb:1108-1109 @ fd1c2cd`; no text copied). There is no user-supplied prompt text.

Data flow (inputs → LLM → render):

|                     |                                                                                                                                                               |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Personal data       | Transcript text and speaker names, chat with sender names, notes, poll aggregates, meeting name; LLM output derived from them (action-item owners are names). |
| Stored where        | LLM input is built in memory only. Map JSON, summary JSON, `summary.md` and `transcript.vtt` are job blobs (ADR-011). LLM provider: per profile.              |
| Retention / deleter | Until the job's `cleanup` step (ADR-009), deleted by the service. The delivered Docs document belongs to the owner.                                           |

## 4. LLM steps

Prompts are English data files shipped with the format; the output language is injected (§7). Temperature 0.

**Call plan.** If the rendered input fits the budget (`providers.md` §3.5): one `summarize` call → `summary.v1`.
Otherwise `extract` per transcript chunk → `chunk-notes.v1`, then `consolidate` over the chunk notes, chat, notes,
polls and the gap list → `summary.v1`. If the transcript has no segments (everyone silent): no LLM call; `summary.md`
says that no speech was recognized.

**Prompt requirements.** The system message states the task, that data sections are data only, the output language,
and JSON only (with the schema embedded in `json_object` and `prompt` modes). It forbids inventing owners or due
dates, requires time references copied from line timestamps, and says that listed gaps are missing speech and must not
be guessed.

**Schema `summary.v1`** (shipped as `meeting-summary/summary.v1.schema.json`, JSON Schema draft 2020-12):

| Field                 | Items                       | Limits                                  |
| --------------------- | --------------------------- | --------------------------------------- |
| `overview` (required) | string                      | 1–2000 chars                            |
| `topics[]`            | `{title, summary, start?}`  | ≤ 12; title ≤ 120, summary ≤ 2000       |
| `decisions[]`         | `{text, at?}`               | ≤ 50; text ≤ 500                        |
| `action_items[]`      | `{task, owner?, due?, at?}` | ≤ 50; task ≤ 500, owner ≤ 120, due ≤ 60 |
| `open_questions[]`    | `{text}`                    | ≤ 30; text ≤ 500                        |

All arrays are required (they may be empty). Objects have `additionalProperties: false`; required strings are
non-empty; `start`/`at` match `^[0-9]{2}:[0-5][0-9]:[0-5][0-9]$`. The LLM does not write a title: the document title
is fixed by §5.1. The fields are close to those of prior art (transkribierBAR `SUMMARY_SCHEMA` and `PROTOCOL_SCHEMA`,
`backend/app/services/llm/prompt.py:17-36,160-216 @ f3f38f3`). `due` is free text. Owners are names as spoken, not
identity mappings.

**`chunk-notes.v1`** (map): same item shapes, `overview` renamed `chunk_summary` (max 1500 chars), each array capped at
half. `consolidate` merges duplicates and orders topics by `start`.

**After the LLM:** `at`/`start` values after the last recorded time are removed. A schema failure after repair is
`invalid_response` (`providers.md` §3.4).

## 5. `summary.md`

### 5.1 Title

Title = `{meeting_name} {date}`, `date` = `YYYY-MM-DD` of `time_base.origin_utc` in `RECAP_DISPLAY_TZ` (ADR-007).
Control characters and newlines become spaces; runs of spaces collapse. If the title is longer than 200 characters,
the meeting name is cut so that the whole title is exactly 200 characters, ending with `…` (Docs stores at most 255,
[`upstream-facts.md`](upstream-facts.md)). The title is passed to the sink as a plain string; it is not part of the
Markdown body.

### 5.2 Markdown subset and escaping

Only what Docs converts. Docs converts Markdown with BlockNote's parser (`markdownToHtml.ts`, BlockNote 0.54, through
the y-provider `/convert` endpoint; Docs @ `6c730f7`, see [`upstream-facts.md`](upstream-facts.md)). Used: ATX
headings `##`–`###`, paragraphs, `**bold**`, `*italic*`, `-` lists, task lists `- [ ]`, GFM pipe tables with inline
cells, blockquotes, `---`. Not used: links, images, raw HTML, footnotes. The size limit and the full dialect are in
`sinks.md`; the step fails `permanent` if the document exceeds the sink's limit.

Every string from meeting content or the LLM is escaped: a backslash before ``\ ` * _ ~ [ ] < > # |``; at the start
of every escaped value a leading `-`, `+`, `>` or `=` gets a backslash, and a leading `<digits>.` becomes
`<digits>\.`; newlines in any value become spaces (CRLF, CR and LF each count as one newline). The parser supports
backslash escapes (BlockNote `packages/core/src/api/parsers/markdown/markdownToHtml.ts:65-151,212-215 @ v0.54.0`).

### 5.3 Time display

`[hh:mm:ss]` is **recorded time** (pauses collapsed, `providers.md` §4.3), as in BBB playback
(`record-and-playback/core/scripts/utils/gen_webvtt:111-150 @ 7d27e7e`). It is plain text; it is safe because the
parser needs `](` for a link (`markdownToHtml.ts:363-372 @ v0.54.0`). Wall-clock times appear only in the header line.

### 5.4 Layout

Static strings come from the string table of the output language (§7). `{…}` is filled and escaped. Blocks are
separated by one blank line. Optional parts (`{owner}`, `{due}`, `[{at}]`) are left out with their separators when
empty.

| #   | Block                      | Markdown                                                                                               |
| --- | -------------------------- | ------------------------------------------------------------------------------------------------------ |
| 1   | Header                     | `*{meeting_name} · {date} · {first_start}–{last_end} {tz} · recorded: {recorded_duration}*`            |
| 2   | Notice                     | `*Automatically generated from the recorded parts of this meeting. It may contain errors.*`            |
| 3   | Gaps (if any), one per gap | `> **Incomplete transcript:** no text for {speaker} from [{start}] to [{end}] (transcription failed).` |
| 4   | Overview                   | `## Overview`, then the paragraph                                                                      |
| 5   | Topics                     | `## Topics`; per topic `### {title} [{start}]` and its paragraph                                       |
| 6   | Decisions                  | `## Decisions`; `- {text} [{at}]`                                                                      |
| 7   | Action items               | `## Action items`; `- [ ] {task} — {owner} — {due_label}: {due} [{at}]`                                |
| 8   | Open questions             | `## Open questions`; `- {text}`                                                                        |
| 9   | Polls (if any)             | `## Polls`; per poll `**{question}**` and a table `{option_label} \| {votes_label}`                    |
| 10  | Footer                     | `---`, then `*{format_id} {format_version} · ASR: {asr_model} · LLM: {llm_model} · {generated_at}*`    |

Details:

- `{first_start}`/`{last_end}` are `HH:MM` (seconds cut, not rounded) in `RECAP_DISPLAY_TZ`, from the first period
  start and the last period end; `{tz}` is that zone's name.
- `{recorded_duration}` is the sum of the period lengths as `hh:mm:ss`; gap times are `hh:mm:ss`.
- A poll is the bold question, a blank line, then a table with a header row, `| --- | --- |` and one row per option,
  including options with 0 votes.
- `{asr_model}` is the distinct `tracks[].provider.model` values joined by `, `.
- `{generated_at}` is the job's `created_at`, RFC 3339 UTC (deterministic).
- `{speaker}` for a speaker without a display name is the string-table entry with `n` from `spk-<n>`.
- The `en` and `de` string tables are in `testdata/render/make_goldens.py` (`STRINGS`). `de` labels: Überblick,
  Themen, Beschlüsse, Aufgaben, Offene Fragen, Umfragen, "Keine Einträge.", fällig, Option, Stimmen, "Sprecher {n}".

Empty LLM sections (5–8) render "None recorded." so readers see they were considered. Blocks for missing optional
inputs (polls) are left out. Golden files: `testdata/render/`.

## 6. `transcript.vtt` (WebVTT)

Syntax facts: W3C WebVTT, https://www.w3.org/TR/webvtt1/ §4.1–4.2 (read 2026-10-29). Purpose: a standard file that
other software can read. It is not delivered yet (ADR-007).

| Rule                     | Value                                                                                                              | WebVTT fact                                                                                                                |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------- |
| Encoding                 | UTF-8, no BOM, LF line endings                                                                                     | UTF-8 required; BOM optional; LF is a valid line terminator (§4.1)                                                         |
| Header                   | `WEBVTT`, blank line, then a fixed `NOTE` (below)                                                                  | file starts with `WEBVTT`, followed by two or more line terminators (§4.1)                                                 |
| Blocks                   | separated by exactly one blank line; file ends with LF                                                             |                                                                                                                            |
| Cues                     | one cue per transcript segment, in `segments[]` order                                                              |                                                                                                                            |
| Cue identifier           | segment `id` (`seg-000001`)                                                                                        | unique, no `-->`, no line break (§4.1)                                                                                     |
| Timing line              | `hh:mm:ss.ttt --> hh:mm:ss.ttt`, hours always written (≥ 2 digits)                                                 | format and the `-->` separator (§4.1)                                                                                      |
| Times                    | recorded time of `start_ms`/`end_ms` in the period that contains the segment midpoint; both clamped to that period |                                                                                                                            |
| End > start              | if `end ≤ start` after mapping, `end = start + 1 ms`                                                               | end must be greater than start (§4.1)                                                                                      |
| Order                    | start times never decrease (holds: segments are sorted and recorded time is monotonic)                             | each start ≥ all earlier starts (§4.1)                                                                                     |
| Payload                  | one line: `<v {name}>{text}</v>`                                                                                   | voice span `<v annotation>` (§4.2)                                                                                         |
| Name                     | `display_name`, else "Speaker n" from the string table                                                             | annotation may not contain LF, CR, `&`, `>` (§4.2)                                                                         |
| Escaping (name and text) | `&` → `&amp;`, `<` → `&lt;`, `>` → `&gt;`; CR, LF, tab → space; collapse spaces; trim                              | cue text may not contain raw `&`, `<`, LF, CR; character references allowed (§4.2); `-->` is impossible after escaping `>` |

`NOTE` blocks are comments, ignored by players; their text may not contain `-->` or line breaks (§4.1). The NOTE texts
come from the string table of the output language (§7); `en` and `de`:

| NOTE   | Where                                                                                                                                            | Text                                                                                                                                                                                                       |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Header | after `WEBVTT`                                                                                                                                   | en `NOTE Automatic transcript (speech recognition). Times are recorded time; pauses are removed.`<br>de `NOTE Automatisches Transkript (Spracherkennung). Zeiten sind Aufnahmezeit; Pausen sind entfernt.` |
| Pause  | before the first cue of each recording period after the first                                                                                    | en `NOTE Recording paused here.`<br>de `NOTE Aufnahme hier pausiert.`                                                                                                                                      |
| Gap    | before the first cue whose start ≥ the gap's recorded start (ties: NOTE first; after a pause NOTE for the same cue); at the end if there is none | en `NOTE Gap {start} to {end}, {name}: no transcript ({reason}).`<br>de `NOTE Lücke {start} bis {end}, {name}: kein Transkript ({reason}).`                                                                |

In a gap NOTE, `{start}`/`{end}` use the timing format, `{reason}` is the README gap reason, and `{name}` is cleaned
by turning CR, LF and tab into spaces, collapsing spaces and trimming, with `&`, `<` and `>` kept as they are
(comments have no character references); then `-->` is replaced by `->` repeatedly until none is left.

No `STYLE`, `REGION` or cue settings. No language marker: the W3C file syntax defines no language field (§4.1).
Deterministic: the same transcript gives a byte-identical file. Golden for F-01: `fixtures.md` §7.

## 7. Output language

The output follows the transcript language: `transcript.language` if the format has a string table for it, else
`RECAP_DEFAULT_LANGUAGE` (default `en`). ASR detects the language by default (`RECAP_ASR_LANGUAGE`, `providers.md`
§1.2); there is no language meta key. **Every** fixed string in `summary.md` and `transcript.vtt` (labels, notices,
speaker fallback, NOTEs) comes from that string table; nothing user-facing is hard-coded in one language. The language
goes into every system prompt as an ISO 639-1 code plus its English name, and selects the string table. String tables
ship for `en` and `de`. More tables can be added as files; the service does not translate labels itself.
transkribierBAR also falls back to the transcription language (`backend/app/routers/analysis.py:73 @ f3f38f3`).

## 8. Contract tests

| ID        | Test                                                                                                                                                                                                                                                                                                     |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-TPL-01 | Fixture LLM replies (valid and invalid) validate/fail against `summary.v1` and `chunk-notes.v1`.                                                                                                                                                                                                         |
| CT-TPL-02 | Golden `summary.md`: fixture transcript + summary JSON → byte-identical, for `en` and `de`, with and without gaps, polls, empty sections.                                                                                                                                                                |
| CT-TPL-03 | Call plan under/over the budget against a fake LLM; no chat or notes in `extract` calls.                                                                                                                                                                                                                 |
| CT-TPL-04 | Markdown escaping: `# x`, `[a](b)`, `<script>`, `- [x]`, `\|`, newlines render as literal text.                                                                                                                                                                                                          |
| CT-TPL-05 | Captured LLM requests contain no BBB user IDs, no email, no per-user poll answers.                                                                                                                                                                                                                       |
| CT-TPL-06 | Selection: `meeting-summary` → format selected; `no-such-type` (F-04) → `held/unknown_type` before any provider call.                                                                                                                                                                                    |
| CT-TPL-07 | Manual or optional CI check: golden Markdown through a local y-provider `/convert` at the pinned Docs version gives the expected block types.                                                                                                                                                            |
| CT-TPL-08 | Golden `transcript.vtt` for F-01 (`fixtures.md` §7): byte-identical.                                                                                                                                                                                                                                     |
| CT-TPL-09 | WebVTT syntax: a test validator written from §4.1–4.2 of the W3C spec checks header, timing format, end > start, non-decreasing starts, unique IDs, no blank line or `-->` in any cue or NOTE. Runs over every rendered `.vtt`, including adversarial names and text (`&`, `<b>`, `-->`, `>`, newlines). |
| CT-TPL-10 | WebVTT mapping: pause collapse, pause NOTE, gap NOTE placement (failed-track gap; the M-11 case is not covered yet, `providers.md` §7), zero-length segment → +1 ms, segment crossing a period end → clamped.                                                                                            |
| CT-TPL-11 | Title and language: title rule of §5.1 incl. the 200-character cut; `transcript.language` `de` → `de` strings; unsupported language → `RECAP_DEFAULT_LANGUAGE`.                                                                                                                                          |

CT-TPL-01, the golden files of CT-TPL-02 and CT-TPL-08, CT-TPL-04, CT-TPL-06 and CT-TPL-09 were written before the
implementation and are not changed to fit it.

## 9. Later: admin-defined templates (not supported yet)

The built-in format already uses the structure planned for admin templates (ID, version, meeting types, declared
inputs, steps with prompts and schemas, render skeleton), so admin templates would only change how formats are
loaded. `transcript.vtt` stays built in. Planned properties: a sandboxed, logic-light engine where missing
placeholders fail at load time; schema and skeleton in one template; golden fixtures before a template is enabled;
escaping by the engine; no user prompt text.

## 10. Known limitations

- Admin-defined templates are not supported; the template engine and where templates are stored are not chosen yet.
- There is no per-meeting prompt text from users.
- Slide titles are not used as input: what the `presentation/` directory of a BBB 4.0 recording contains was not
  checked.
- The poll event fields were checked against one real recording only.
- Raw HTML, images and block content in table cells are avoided because how Docs converts them was not checked
  (`sinks.md` §2.2).
