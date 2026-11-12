# Render inputs and goldens

Inputs and golden outputs for `contracts/render` (`docs/reference/templates.md` §5, §6; `docs/reference/fixtures.md` §7).
Synthetic only (F-01 names and texts).

The transcript inputs here are independent; they do not depend on `testdata/transcripts/`.

## How they were made

- `*/context.json`, `*/transcript.json`, `*/summary.json`: written by hand.
  - `F-01/transcript.json` follows the F-01 segment table of fixtures.md §3.1 (axis ms, periods
    `{0, 55000, 0}` and `{85000, 145000, 55000}`) and the five dialogue lines of §7.1. Track spans come from the
    publish and unpublish events in F-01's `events.xml`, `blob_sha256` from the F-01 microphone files, origin
    `timestampUTC` 1761732005000 = `2025-10-29T10:00:05Z`.
  - `F-01/context.json`: meeting name, the two chat messages (axis 7000 and 9000 ms from `events.xml`), the shared
    notes as text, the poll "Blue or green?" (both answer Blue), the owner of fixtures.md §5,
    `llm_model` `example-model` (providers.md §4.1), `generated_at` `2025-10-29T10:20:00Z`.
  - `F-01/summary.json`: the mock LLM reply (`summary.v1`), written to match the dialogue.
  - `gap/`: F-01 with a failed-track gap: ASR failed on Ada's first track `trk-1` (`asr_failed`, span 200–55000 ms),
    its segment is gone and the others are renumbered; no polls; a summary with empty sections.
  - `mapping/transcript.json`: WebVTT mapping cases (two periods 0–30 s and 40–70 s): unnamed speaker, adversarial
    name and text, zero-length segment, a segment crossing the end of period 1, one starting in the pause, a gap
    whose start ties with a cue start, a gap whose NOTE follows the pause NOTE before the same cue, and a gap after
    the last cue. The gap speaker's name `  Gus <b>Gap</b> & Co\t--->\r\n---->  X  ` pins the §6 NOTE name rule
    (`Gus <b>Gap</b> & Co -> -> X`: whitespace cleaned, `&<>` literal, `-->` → `->` until none is left).
- Goldens and the LLM reply fixtures: written by `make_goldens.py`, an independent reference renderer written from
  the reference text (standard library plus `jsonschema` for input and fixture checks). Run from the repository root:
  `python3 -B -P testdata/render/make_goldens.py` (`-P`, not `-I`: `jsonschema` is a user package here). Output is
  deterministic. The script also checks that `F-01/transcript.vtt` equals fixtures.md §7.1 byte for byte, that the
  transcripts validate against `docs/reference/schemas/transcript.schema.json`, and that every `llm/` file is valid or
  invalid as its name says against reference schemas written from templates.md §4.
- Every golden was also checked by hand against templates.md §5.4 and §6.

## Files

| Path | Content |
|---|---|
| `F-01/summary.md`, `F-01/summary.de.md` | `summary.md` for F-01 in `en` and `de` (transcript language `de`) |
| `F-01/transcript.vtt` | = fixtures.md §7.1 |
| `F-01/transcript.de.vtt`, `gap/transcript.de.vtt` | the same with transcript language `de`: NOTE texts from the `de` string table (templates.md §6, §7) |
| `gap/summary.md`, `gap/summary.de.md`, `gap/transcript.vtt` | gap notice, "None recorded." sections, no polls; gap NOTE before the first cue |
| `mapping/transcript.vtt` | mapping cases above |
| `llm/summary.v1/`, `llm/chunk-notes.v1/` | `valid.<case>.json` / `invalid.<case>.json` LLM replies |

## Layout details

The details below are fixed in **templates.md §5.4** ("Fixed details"); escaping in §5.2, NOTE order in §6.

| # | Detail |
|---|---|
| C1 | `de` string table in `make_goldens.py` `STRINGS` (speaker label `Sprecher {n}`) and the WebVTT NOTE texts per language (§6, §7) |
| C2 | `en` labels: `{due_label}` = `due`, `{option_label}` = `Option`, `{votes_label}` = `Votes` |
| C3 | Header `{first_start}`/`{last_end}` = `HH:MM` (seconds cut, not rounded) in the display zone (first period start, last period end); `{tz}` = the zone name |
| C4 | `{recorded_duration}` = sum of period lengths as `hh:mm:ss`; gap times `hh:mm:ss` |
| C5 | Poll: bold question, blank line, table with header row, `\| --- \| --- \|`, one row per option incl. 0 votes |
| C6 | Footer `{asr_model}` = distinct `tracks[].provider.model` joined by `, `; `{generated_at}` RFC 3339 UTC |
| C7 | Leading `-`, `+`, `>`, `=` escaped at the start of every value; leading `<digits>.` → `<digits>\.`; newlines → spaces everywhere (CRLF, CR and LF each one newline) |
| C8 | "Speaker n" / "Sprecher n": `n` from `spk-<n>` |

Also fixed in the reference: the title is cut to exactly 200 characters ending in `…` (§5.1); in gap NOTE
names `-->` is replaced by `->` until none is left (§6).

M-11 (unmatched microphone file) is not covered here (providers.md §7); the gap fixtures use failed-track gaps.
