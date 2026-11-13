# Fixtures: synthetic meetings, fixture secrets, golden files

Tests, examples and development setups use only these fixtures: no real meeting data, voices or secrets.

## 1. Principles

- Synthetic only: fictional names ("Ada Testperson"), `@example.org`, `*.recap.test` (RFC 2606). IDs `F-NN`, `M-NN`.
- No real voices (§4). Deterministic: fixed IDs, timestamps and clock; generated binaries committed with the generator.
- Shaped like BBB ([`upstream-facts.md`](upstream-facts.md)). Parts whose real shape was not checked carry an XML
  comment starting with `SYNTHETIC: shape` (§8). A structure-only diff against a real recording decides whether
  fixtures change.

## 2. Layout

```
testdata/
  meetings/<fixture-id>/
    README.md, registration.json (if any), raw/<record_id>/… (as BBB's archive.rb leaves it)
    expected/                hook.json, manifest.json, package_id.txt, exporter.json, service.json
                             (formats defined in testdata/README.md)
  manifests/invalid/*.json (one rule each), ingest/exchanges/*.json (golden HTTP, bbb-exporter.md)
  secrets/ (§5), generator/ (scripts that produced the audio)
```

`record_id` = `<sha1(fixture ID)>-1761732000000` (2025-10-29T10:00:00Z), shaped like BBB's fixture
`183f0bf3a0982a127bdb8161e0c44eb696b3e75c-1630430006889` (bigbluebutton/bigbluebutton
`record-and-playback/core/resources/raw/…/events.xml @ 7d27e7e`); F-01: `536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000`. `meeting_id` = `fixture-meeting-<NN>`.

## 3. Catalogue

Common: BBB 4.0 with LiveKit, `bbb-fixture-a`, `pilos-fixture`, `meta_bbb-origin=PILOS` ([`upstream-facts.md`](upstream-facts.md)). Opted-in
fixtures carry `recap-v=1`, `recap-enabled=true`, `recap-type=meeting-summary`, `recap-sink=docs-main`. Service config:
type `meeting-summary`, sink profile `docs-main` (`https://docs.recap.test`), providers `whisperx-lab`, `vllm-lab`
(`providers.md` §4.1), `RECAP_DEFAULT_LANGUAGE=en`, `RECAP_DISPLAY_TZ=UTC`.

| ID   | Name                  | Purpose                                                                          | Expected outcome                                                                                |
| ---- | --------------------- | -------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| F-01 | `two-speakers-pause`  | happy path, all content types                                                    | spooled; golden manifest; acked; job `delivered` with mocked providers and sink                 |
| F-02 | `legacy-freeswitch`   | non-LiveKit (ADR-001)                                                            | exporter `failed/unsupported_audio_bridge`; fed directly: job `failed/unsupported_audio_bridge` |
| F-03 | `no-optin`            | no `recap-*` keys                                                                | no spool entry, `not_opted_in`                                                                  |
| F-04 | `invalid-type`        | `recap-type=no-such-type`                                                        | acked; job `held/unknown_type`                                                                  |
| F-05 | `optin-invalid-value` | `recap-enabled=yes`                                                              | no spool entry, `not_opted_in` (also F-11)                                                      |
| F-06 | `invalid-metadata`    | `recap-sink` missing                                                             | job `held/invalid_metadata`                                                                     |
| F-07 | `no-registration`     | F-01 raw dir, no registration                                                    | job `held/no_registration`                                                                      |
| F-08 | `breakout`            | breakout room inheriting the opt-in                                              | no spool entry, `breakout_skipped`                                                              |
| F-09 | `oversize`            | F-01 plus a sparse 3 GiB `presentation/` file made at test time                  | exporter `failed/limit_exceeded`                                                                |
| F-10 | `livekit-no-mic`      | opted in, no published microphone (no `audio/` dir: git cannot store empty dirs) | exporter `failed/no_microphone_tracks`                                                          |
| F-11 | `optin-false`         | `recap-enabled=false`                                                            | as F-05                                                                                         |
| F-12 | `no-marks`            | opted in, `record=true`, no `RecordStatusEvent`                                  | hook WARN `no_marks`, no spool entry (`bbb-exporter.md` §3.3)                                   |
| F-13 | `slides`              | F-01's periods and audio plus three decks with `textfiles/` (§3.2)               | `normalize` gives the golden `slides[]` (`templates.md` §3.1); job `delivered`                  |

### 3.1 F-01 `two-speakers-pause`

Offsets in ms from the epoch base; `timestamp` uses a monotonic base 5 000 000.

| Offset                  | Event                                                                                                                                     | Notes                                                 |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------- |
| 0 / 1 000               | `ParticipantJoinEvent` Ada (`w_fixada0001`, `u1`, `Ada Testperson`, `MODERATOR`) / Ben (`w_fixben0002`, `u2`, `Ben Testperson`, `VIEWER`) | `userId`, `externalUserId`, `name`, `role`            |
| 5 000                   | `RecordStatusEvent` start                                                                                                                 | period 1                                              |
| 5 200 / 5 300           | `AudioTrackPublishedEvent` Ada / Ben, `source=microphone`, `filename=…/microphone-<userId>-<trackId>-<suffix>.webm`                       | file naming: [`upstream-facts.md`](upstream-facts.md) |
| 6 000–60 000            | `PublicChatEvent` ×2, poll started, 2 responses, stopped, published                                                                       | poll fields synthetic (§8)                            |
| 60 000                  | `RecordStatusEvent` stop; unpublish Ada, Ben                                                                                              | **pause**                                             |
| 90 000; 90 200 / 90 300 | `RecordStatusEvent` start; new `AudioTrackPublishedEvent` Ada / Ben, new `trackId`/`suffix`                                               | period 2; several files per user                      |
| 150 000; 155 000        | `RecordStatusEvent` stop, unpublish; `ParticipantLeftEvent` ×2                                                                            |                                                       |

Also: `PadCreatedEvent`; `SharePresentationEvent`/`GotoSlideEvent` for a 2-page deck; `/recording/metadata` with the
opt-in keys and `bbb-origin=PILOS`; `<meeting name="Fixture Widget Meeting">`.

Raw files **in** the package: `events.xml`; `audio/microphone-w_fixada0001-TR_fixA1-<s>.webm`, `…TR_fixA2…` (Ada),
`audio/microphone-w_fixben0002-TR_fixB1-<s>.webm`, `…TR_fixB2…` (Ben); `notes/notes.html`;
`presentation/<pres-id>/<pres-id>.pdf` + 2 slide images (layout synthetic, §8). **Not** in it: a 5 s tone
`audio/screen_share_audio-…`, `notes/notes.pdf`, 2 s black VP8 in `video/` and `deskshare/`,
`learning-dashboard/data.json`, and a symlink `audio/link-to-elsewhere.webm` (counted as skipped). `TR_fix…` track IDs
are synthetic (the real format was not checked; the exporter does not parse names).

Dialogue: the five cue texts of §7.1 in that order, pause after the second (a decision, an action item, a question).
Chat: Ben "Link to the widget spec will follow." · Ada "Thanks." Poll "Blue or green?": both blue. Golden transcript
segments (set by mock ASR replies; axis = offset − 5 000; periods `{0, 55 000, 0}`, `{85 000, 145 000, 55 000}`):

| Segment    | Track      | Speaker   | Axis ms       | Recorded ms   |
| ---------- | ---------- | --------- | ------------- | ------------- |
| seg-000001 | trk-1 (A1) | spk-1 Ada | 1 000–5 500   | 1 000–5 500   |
| seg-000002 | trk-2 (B1) | spk-2 Ben | 6 000–10 000  | 6 000–10 000  |
| seg-000003 | trk-3 (A2) | spk-1 Ada | 86 000–91 000 | 56 000–61 000 |
| seg-000004 | trk-4 (B2) | spk-2 Ben | 92 000–96 500 | 62 000–66 500 |
| seg-000005 | trk-3 (A2) | spk-1 Ada | 97 000–99 000 | 67 000–69 000 |

Deltas from F-01:

- **F-02:** VOICE `StartRecordingEvent`/`StopRecordingEvent`, mixed `audio/<record_id>-<ts>.opus`, no
  publish events. **F-10:** no publish events, empty `audio/`. **F-03:** only `bbb-origin`. **F-07:** F-01 by reference.
- **F-08:** `<meeting … breakout="true">` plus `<breakout>` (encoding synthetic, §8). **F-09:** never committed.

### 3.2 F-13 `slides`

F-01's participants, recording periods (5 000–60 000, 90 000–150 000) and microphone files (byte-identical copies),
without chat, poll and notes, plus three decks in the default pod. Offsets in ms from the epoch base:

| Offset            | Event                                                                        | Expected (`templates.md` §3.1)          |
| ----------------- | ---------------------------------------------------------------------------- | --------------------------------------- |
| 300–302           | default deck (`originalFilename` `default.pdf`) converted, shared, page 1    | shown 5 000–12 000 (7 s), but skipped   |
| 2 000 / 2 500     | `ConversionCompletedEvent` for a 4-page deck and an image deck (not current) | original file names never leave         |
| 12 000            | image deck shared (2 s)                                                      | no slide (placeholder text)             |
| 14 000            | 4-page deck shared, page 1 until 30 000                                      | slide `d-1`/1, `t_ms` 9 000             |
| 30 000            | page 2 until 45 000                                                          | slide `d-1`/2, `t_ms` 25 000            |
| 45 000 / 46 000   | page 3 for 1 s, back to page 2                                               | page 3 under the 5 s minimum            |
| 70 000 / 80 000   | page 3 during the pause, back to page 2                                      | page 3 still not shown                  |
| 100 000           | image deck shared again (20 s)                                               | no slide (placeholder text)             |
| 120 000           | 4-page deck shared again: back on page 2 for exactly 5 s                     | already listed                          |
| 125 000 / 135 000 | page 4 (placeholder text) for 10 s, then page 1 again                        | no slide for page 4; page 1 listed once |

Page texts are synthetic, shaped like `pdftotext -layout` output; page 2 also has CRLF, a tab and layout spacing for
the cleaning rules. Golden: `testdata/meetings/F-13/expected/slides.json`, written by hand in the generator from this
table. F-13 has no package golden (`manifest.json`); its slide behaviour is tested at raw-directory level with
`recap tool normalize`.

## 4. Synthetic audio

- **Speech: eSpeak NG**: formant synthesis from rules, no recordings of people, offline, deterministic; two built-in
  English voice variants.
- **Tones:** ffmpeg `sine` for content that does not matter. **Encoding:** Opus in WebM, 48 kHz, mono, like the LiveKit
  recorder (bigbluebutton/bbb-webrtc-recorder `internal/webrtc/recorder/webm.go:24,136 @ 3d581ad`; channel count
  synthetic, §8); F-02 Ogg/Opus like BBB's FreeSWITCH archive. Each file holds its speaker's lines with silence
  elsewhere (how real files handle muted stretches: §8).
- **Use:** CI mocks ASR and LLM (mock ASR replies keyed by file SHA-256). The audio serves manual tests against a real BBB
  and realistic sizes. `testdata/generator/manifest.txt` lists tool, version and command per file; regenerating is a fixture change.

## 5. Fixture secrets and identities

Dev-compose Postgres and MinIO credentials are not committed: `tools/dev/dev-secrets.sh` generates them with the
`FIXTURE-NOT-A-SECRET-` prefix into an ignored directory.

In `testdata/secrets/`, mode 0644, values start with `FIXTURE-NOT-A-SECRET-`. `clients/` mounts as `RECAP_CLIENT_SECRETS_DIR`.

| File                                                                                                                  | Used as                                                                           | Value                                              |
| --------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- | -------------------------------------------------- |
| `clients/bbb_server.bbb-fixture-a`, `….bbb-fixture-b`                                                                 | ingest credentials (a is also the exporter's token file; b for `server-mismatch`) | `FIXTURE-NOT-A-SECRET-bbb-a-<32 hex>`, `…-bbb-b-…` |
| `clients/frontend.pilos-fixture`, `clients/frontend.other-fixture` (foreign front end), `clients/admin.fixture-admin` | registration and admin credentials                                                | `FIXTURE-NOT-A-SECRET-fe-<32 hex>`, `…-admin-…`    |
| `asr-fixture.key`, `llm-fixture.key`                                                                                  | provider credentials for mocks                                                    | `FIXTURE-NOT-A-SECRET-asr-…`, `…-llm-…`            |
| `docs-s2s.key`                                                                                                        | Docs server-to-server key for `create-for-owner` (mock Docs, ADR-007)             | `FIXTURE-NOT-A-SECRET-docs-s2s-…`                  |

Owner of F-01/F-04/F-06: `sub=fixture-sub-ada-0001`, `ada.testperson@example.org`. Mock Docs users (`sinks.md` §4): Ada;
`fixture-sub-dup-0007` and `-0008` sharing `dup.testperson@example.org` (CT-SINK-08). No Docs account (invitation):
`fixture-sub-unknown-0009`, `nobody.testperson@example.org`. Admin of the admin UI (mock IdP default, role `recap-admin`): `fixture-sub-admin-0010`; it is never an
owner, so pages and audit rows written by it cannot be confused with owner data. Hosts `*.recap.test` are local fakes, never resolved.

## 6. Mutations (from F-01, generated at test time)

| ID          | Mutation                                                            | Expected                                                                 |
| ----------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| M-01        | flip a byte in a microphone file after the manifest is built        | exporter `failed/source_changed`; direct upload 422 `file-hash-mismatch` |
| M-02        | delete a microphone file after build                                | exporter `failed/source_missing`                                         |
| M-03 / M-04 | path `../events.xml` / duplicate path, unsorted files, wrong totals | 422 `manifest-invalid`                                                   |
| M-05        | `schema_version` `2.0.0` / `1.9.0`                                  | 422 `unsupported-schema-version`                                         |
| M-06        | non-canonical manifest bytes                                        | 400 `package-id-mismatch`                                                |
| M-07        | upload with the `bbb-fixture-b` credential                          | 403 `server-mismatch`                                                    |
| M-08        | second package for an ingested `record_id`                          | 409 `record-already-ingested`                                            |
| M-09        | manifest `metadata` ≠ `events.xml`                                  | job `failed/manifest_metadata_mismatch`                                  |
| M-10        | `externalId` ≠ manifest `meeting_id`                                | job `failed/events_mismatch`                                             |
| M-11        | rename Ada's A1 file so no publish event matches                    | job continues; gap for trk-1 (V-S05); gap NOTE in `transcript.vtt`       |
| M-12 / M-13 | `recap-enabled=yes` sent to ingest / `breakout: true`               | 422 `not-opted-in` / 422 `manifest-invalid`                              |

M-03…M-06 are also committed as `testdata/manifests/invalid/*.json`.

## 7. Golden files and protection

| Golden                                                                                                          | Pinned by                          |
| --------------------------------------------------------------------------------------------------------------- | ---------------------------------- |
| `raw/**`, `hook.json`, `exporter.json`, `service.json`                                                          | all CTs; CT-EXP-01…05; CT-PKG-06   |
| `manifest.json`, `package_id.txt`, `manifests/invalid/*.json`                                                   | CT-PKG-01…03, CT-PKG-05, CT-EXP-02 |
| `testdata/transcripts/F-01/…` (`transcript.json` etc.)                                                          | CT-TR-01, CT-TR-02                 |
| `testdata/render/F-01/…` (`summary.json` mock LLM reply, `summary.md`, `transcript.vtt`, `de` and gap variants) | CT-TPL-02, CT-TPL-08…10            |
| `ingest/exchanges/*.json`                                                                                       | CT-EXP-04                          |
| `meetings/F-13/expected/slides.json`                                                                            | CT-TPL-12                          |

Notes: F-02 has no manifest (no microphone WebM); it is tested at raw-directory level only
(`recap tool validate-package`). Golden manifests assume exporter version `0.0.0-fixture` and `created_at`
`2025-10-29T10:10:00Z`; implementations inject both in tests. Regeneration is byte-identical only with the tool
versions recorded in `testdata/generator/manifest.txt`, and only with eSpeak voice variants without breath noise
(f2, f3, f5 vary per run).

Goldens are protected: they change only in a dedicated fixture change that says why, never together with the code
they test. They are made by hand or by a script independent of the code under test.

### 7.1 `transcript.vtt` golden for F-01 (by hand from §3.1 and `templates.md` §6; ends with one LF)

```
WEBVTT

NOTE Automatic transcript (speech recognition). Times are recorded time; pauses are removed.

seg-000001
00:00:01.000 --> 00:00:05.500
<v Ada Testperson>Welcome to the fixture meeting. Today we decide the colour of the test widget.</v>

seg-000002
00:00:06.000 --> 00:00:10.000
<v Ben Testperson>I propose blue. Blue widgets passed every test last week.</v>

NOTE Recording paused here.

seg-000003
00:00:56.000 --> 00:01:01.000
<v Ada Testperson>Agreed, blue it is. Ben, please update the widget specification by Friday.</v>

seg-000004
00:01:02.000 --> 00:01:06.500
<v Ben Testperson>I will update the specification by Friday. Do we need a review?</v>

seg-000005
00:01:07.000 --> 00:01:09.000
<v Ada Testperson>Yes, I will review it on Monday.</v>
```

## 8. Privacy and known limitations

No personal data (synthetic), kept in git for the repository's life. INFO logs stay content-free, checked with dialogue
phrases as canaries (CT-EXP-08, CT-TR-03).

Not checked against a real BBB 4.0 recording, so synthetic in the fixtures:

- the exact rendering of `AudioTrackPublishedEvent` (child order, `<date>`), poll event fields and the breakout
  encoding in `events.xml`;
- the internal layout of `presentation/`, including `textfiles/` (F-13 follows BBB's code, `upstream-facts.md`
  "BBB presentations"), and the child order of the presentation events;
- the Opus channel count of LiveKit WebM files, and whether muted stretches stay as silence or are collapsed in the
  file timeline;
- the format of LiveKit track IDs.
