# Contract tests: transcript

Black-box tests for the transcript (`docs/reference/providers.md` §4.2, §4.3; §6 lists them). They drive
`recap tool normalize` and `recap tool assemble` from `RECAP_BIN`. Without it the tests skip and name it.

## `recap tool normalize`

Dev/test only (`docs/reference/README.md` "Configuration, processes, metrics"). Needs no configuration.

```
recap tool normalize DIR
```

- `DIR`: a raw recording directory `raw/<record_id>/` as `archive.rb` leaves it, or a package directory (files at
  their manifest paths). Files are selected by the allow-list of `meeting-package.md` §5. The tool only reads below
  `DIR` and writes nothing.
- Output: the normalized meeting as **one JSON object** on stdout, exit 0. Its fields are not pinned here: the tests
  pass it unchanged to `assemble`. It must hold everything `assemble` needs (events, track hashes, decode results),
  because `assemble` reads nothing but stdin.
- A microphone file that cannot be decoded is not an error: it becomes a gap `track_unreadable`
  (`architecture.md` §5.2), which shows up in the transcript.
- Deterministic: the same `DIR` gives byte-identical stdout.
- Exit 1 on a runtime error, 2 on a usage error; then stdout is empty and stderr says why.

## `recap tool assemble`

```
recap tool assemble            < input.json      # transcript JSON on stdout
recap tool assemble --check    < transcript.json # verdict on stdout
```

### Assemble form

Input on stdin, one JSON object:

```json
{
  "job_id": "019a2f8c-3b40-7000-8000-000000000006",
  "created_at": "2025-10-29T10:10:00Z",
  "normalized": { "...": "stdout of recap tool normalize, unchanged" },
  "asr": [
    {
      "path": "audio/microphone-w_fixada0001-TR_fixA1-1761732005200.webm",
      "profile": "whisperx-lab",
      "dialect": "whisperx-family",
      "result": {
        "language": "en",
        "model": "large-v3",
        "segments": [{ "start_ms": 800, "end_ms": 5300, "text": "..." }]
      }
    },
    { "path": "audio/microphone-w_fixben0002-TR_fixB1-1761732005300.webm", "gap": "asr_rejected_input" }
  ]
}
```

| Field                  | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `job_id`, `created_at` | copied into the transcript (`created_at` = the job's `created_at` (providers.md §4.3), §4.3)                                                                                                                                                                                                                                                                                                                                                 |
| `normalized`           | the normalize output                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `asr[]`                | the outcome of the `transcribe` step per microphone file, keyed by its package `path`. Either `result`, an `AsrResult` (§1.3: times in integer ms relative to the start of the file), with the `profile` and `dialect` that produced it; or `gap` with the README gap reason of a track that has no result (`asr_failed`, `asr_rejected_input`, `asr_invalid_response`). Tracks that normalize made a gap (`track_unreadable`) have no entry |

Output: the transcript (`schemas/transcript.schema.json`) on stdout, exit 0. Deterministic: the same input gives
byte-identical stdout. Exit 1 on a runtime error, 2 on a usage error; then stdout is empty and stderr says why.

### Check form

`--check` reads one transcript and checks it against the schema and the rules of §4.2: the listed rules (end not
before start, refs resolve, a segment's or gap's speaker is its track's, gaps only on `failed` tracks, periods sorted
and not overlapping) and the rules of the §4.2 table (`speakers` and `tracks` sorted by ref, each speaker has a
microphone track, `segments` and `gaps` sorted by `start_ms` then `track_ref`, a `failed` track has no `language` and
no `provider`), and the README rule that `created_at` and `time_base.origin_utc` are RFC 3339 UTC. Output on stdout, exit 0
whenever a verdict was produced:

```json
{ "valid": false }
```

Other fields (for example an error path) are allowed and not pinned.

### Logs

Both tools write their log to stderr (JSON lines, `RECAP_LOG_LEVEL`, `deployment.md`). README "Logging" and
`privacy.md` §6 apply.

## What the tests pin

| Test                   | Pins                                                                                                                                                                   |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| schema and rules       | every golden passes the test's own schema and rule checker, every file in `invalid/` fails it with exactly one defect; then `assemble --check` gives the same verdicts |
| normalize and assemble | per case: both tools exit 0 and are byte-identical on a re-run; the output passes schema and rules and equals the golden                                               |
| no content in logs     | stderr of both tools for every case, at `RECAP_LOG_LEVEL=info`, contains no segment text or display name of the goldens (the provider tests scan their own runs)       |

Comparison with the golden: exact, including the order of every array, except

- `created_at` and `time_base.origin_utc` must be RFC 3339 UTC (offset `Z` or `+00:00`, `docs/reference/README.md` "Units
  and time") and are compared as instants (their spelling, e.g. `…05Z` or `…05.000Z`, is not pinned);
- segment `start_ms`/`end_ms` are compared within 1000 ms (§4.3 item 4: the skew between file start and publish event is not
  measured yet; `providers.md` §7).

Not covered: a microphone file without a matching publish event (fixture M-11; `providers.md` §7).

## Cases (`testdata/transcripts/`)

All built from fixture F-01 by `tools/ref/make_transcripts.py`. The F-01 audio files are the inputs; ASR results are
derived from the segment table in `fixtures.md` §3.1 and the dialogue in §7.1.

| Case          | Input change                                                                                                                                | What it shows                                                                                                                                                                            |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `F-01`        | none                                                                                                                                        | offsets, two periods, `recorded_offset_ms`, speakers from `ParticipantJoinEvent`                                                                                                         |
| `F-01-rejoin` | Ada leaves and rejoins as `w_fixada0001_1` before period 2; her second file is renamed to match                                             | `_N` grouping: the transcript equals F-01's                                                                                                                                              |
| `F-01-gaps`   | B2 replaced with bytes that are not WebM; A1's result has no segments (language `de`); B1 rejected by the ASR                               | statuses `empty`, `failed`, `transcribed`; gaps `asr_rejected_input` and `track_unreadable`; language by speech ms                                                                       |
| `F-01-clip`   | B1 stays published into the pause; B1 has a segment whose midpoint is inside period 1 and one inside the pause; no stop mark after period 2 | clipping by midpoint, which always applies (`providers.md` §4.3 item 5), even though the package does not mark `recordFullDurationMedia`; a period without a stop ends at the last event |

`invalid/<kind>-<defect>.json`: F-01's golden with one defect each. `schema-*` break the schema (float time,
negative time, unknown `source`, missing `gaps`, extra field); `rule-*` break a §4.2 rule (end before start,
unresolved speaker or track ref, segment speaker differs from its track's, gap on a track that is not `failed`,
overlapping or unsorted periods, unsorted speakers, tracks, segments or gaps, a speaker without a track, a `failed`
track with `language` or with `provider`, a `created_at` with offset `+02:00`; the failed-track and gap-order files derive from the `F-01-gaps` golden).

Regenerate (deterministic; checks every golden with `jsonschema` and the §4.2 rules):

```sh
python3 -B -P tools/ref/make_transcripts.py
```

## Run

```sh
make contracts                                       # all skip
make build && RECAP_BIN=./bin/recap make contracts
```
