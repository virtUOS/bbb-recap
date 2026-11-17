# Contract tests: templates and render

Black-box tests for `docs/reference/templates.md` (§8 lists them; the test IDs are the subtest names) and the Markdown
dialect of `docs/reference/sinks.md` §2. They drive `recap tool render` from `RECAP_BIN`. Without it every test skips and
names the variable. Inputs and goldens: `testdata/render/` (its README says how they were made).

## `recap tool render`

Dev/test only (`docs/reference/README.md` "Configuration, processes, metrics"). It runs format selection, optionally the
`summarize` step, and the `render` step for one job, without queue, database or blob store.

```
recap tool render --out DIR [--summarize [--images IMAGES]]   # stdin: render input (below)
recap tool render --check-schema FORMAT/SCHEMA  # stdin: one JSON value, e.g. an LLM reply
```

### Environment

| Variable                   | Set by the tests                                                                                                      | Use                    |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| `RECAP_MEETING_TYPES_FILE` | yes: the fixture file of templates.md §1 (variants map the type to a missing format, or set `inputs.slides`)          | format selection       |
| `RECAP_FORMATS_DIR`        | passed through; unset: the repo's `formats/`                                                                          | the shipped formats    |
| `RECAP_DISPLAY_TZ`         | yes: `UTC`, one title case `Europe/Berlin`                                                                            | dates and header times |
| `RECAP_DEFAULT_LANGUAGE`   | yes: `en`, language cases `de`                                                                                        | templates.md §7        |
| `RECAP_PROVIDERS_FILE`     | with `--summarize`: providers.md §4.1 fixture, `base_url` = the test's TLS fake LLM, `structured_output: json_schema` | LLM calls              |
| `SSL_CERT_FILE`            | with `--summarize`: the fake's certificate                                                                            | TLS to the fake        |

### Input (stdin)

One JSON object. Readers ignore fields they do not know.

| Field          | Content                                                                                                                                                                                                                                                                            |
| -------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `meeting_type` | value of `meta_recap-type`                                                                                                                                                                                                                                                         |
| `meeting_name` | `events.xml` `<meeting name>`                                                                                                                                                                                                                                                      |
| `transcript`   | transcript `urn:recap:schema:transcript:1` (providers.md §4.2)                                                                                                                                                                                                                     |
| `chat`         | public chat after `normalize`: `[{at_ms, sender_id, name, text}]`, `at_ms` on the meeting axis, `text` plain text (edits applied, deleted dropped). `sender_id` is the BBB `userId` and must never reach the LLM                                                                   |
| `notes`        | shared notes as plain text, or `null`                                                                                                                                                                                                                                              |
| `polls`        | `[{question, options: [text], responses: [{user_id, name, option}]}]`, `option` = index into `options`. Only aggregate counts may be used                                                                                                                                          |
| `slides`       | shown slides after `normalize` (templates.md §3.1): `[{at_ms, deck, page, text, image?}]`, `at_ms` = the normalized meeting's `t_ms`; `image` = `sha256:<hex>` of a file `IMAGES/<hex>.png` (templates.md §3.2). Used only when the meeting type switches slides on; absent = none |
| `owner`        | `{sub, email}` of the registration: job context, never sent to the LLM                                                                                                                                                                                                             |
| `llm_model`    | model shown in the footer when no LLM is called by the tool (with `--summarize`: the model the LLM reports)                                                                                                                                                                        |
| `generated_at` | RFC 3339 UTC time shown in the footer (injected for determinism)                                                                                                                                                                                                                   |
| `summary`      | `summary.v1` (templates.md §4). Required without `--summarize`; absent with it                                                                                                                                                                                                     |

### Output

stdout: one JSON object, exit code 0 for every outcome. Exit 1 on a runtime error, 2 on a usage error; then stdout
is empty and stderr says why.

| Outcome | stdout                                                                                                                                      | Files in `DIR`                 |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| ok      | `{"outcome":"ok","format_id","format_version","format_sha256","language","title"}`                                                          | `summary.md`, `transcript.vtt` |
| hold    | `{"outcome":"hold","reason":"unknown_type"}`: the meeting type is not in the meeting-types file, or its format is missing; no provider call | none                           |
| other   | `{"outcome":"transient","reason":…}` or `"permanent"` (not pinned here)                                                                     | not pinned                     |

`format_sha256` is the SHA-256 of the format files (templates.md §1); only its form (64 lowercase hex) is pinned.
`language` is the output language of §7; `title` the document title of §5.1 (not part of `summary.md`).

`--check-schema meeting-summary/summary.v1` or `meeting-summary/chunk-notes.v1` validates stdin against that schema of
the format and writes `{"valid": true|false}` (exit 0 for both).

### `--summarize`

Runs the `describe` step (templates.md §3.2) against the profile of `defaults.vision` when the meeting type switches
`slide_images` on, then the call plan of templates.md §4 against the default LLM profile, then renders. `--images
IMAGES` names the directory with the slide images that `slides[].image` refers to (a missing file is exit 1). The tool
runs `describe` as on the step's last attempt: an image whose call fails `retryable` falls back to its slide text, as
do `invalid_response`, `output_truncated`, `input_too_large` and an empty description; `permanent_config` gives
`{"outcome":"permanent","reason":"config_error"}` without a summarize call. A meeting-types or providers file that
breaks a rule of templates.md §1 or providers.md §4.1 (for example `slide_images` without `slides`, or without
`defaults.vision`) is exit 2 with the key named on stderr. The fake LLM answers
`chunk-notes.v1` requests (schema with `chunk_summary`) with `testdata/render/llm/chunk-notes.v1/valid.base.json`
and `summary.v1` requests (schema with `overview`) with `testdata/render/F-01/summary.json`.

## Tests

| Go test                      | Checks                                                                                                                                                                                                                                                                                                                |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestReplySchemas`           | `testdata/render/llm/<schema>/{valid,invalid}.*.json` and the goldens' `summary.json`                                                                                                                                                                                                                                 |
| `TestSummaryGolden`          | `summary.md` goldens: F-01 `en`/`de`, gap variant `en`/`de`                                                                                                                                                                                                                                                           |
| `TestCallPlan`               | one call under budget; extract per chunk + one consolidate over budget; no call without speech                                                                                                                                                                                                                        |
| `TestMarkdownEscaping`       | adversarial strings (incl. leading markers, `<digits>.`, CRLF/CR/LF) in every escaped field, each with its own suffix and expected exactly once: meeting name, overview, topic heading and paragraph, decision, action-item task/owner/due, open question, poll question and option, gap-notice speaker               |
| `TestLLMPrivacy`             | no user IDs, owner email/sub or per-user poll answers in LLM requests                                                                                                                                                                                                                                                 |
| `TestFormatSelection`        | `meeting-summary` selected; F-04 type and a missing format → hold `unknown_type`, no LLM call                                                                                                                                                                                                                         |
| `TestDocsDialect`            | rendered goldens use only the dialect; F-01 block types; then skips: the y-provider step is the manual check below                                                                                                                                                                                                    |
| `TestTranscriptGolden`       | `transcript.vtt` golden of fixtures.md §7.1, deterministic; language `de` → `transcript.de.vtt` (NOTE texts from the `de` string table)                                                                                                                                                                               |
| `TestVTTSyntax`              | WebVTT validator (written from the summary in templates.md §6, which cites W3C WebVTT §4.1–4.2; not from the W3C text itself) over goldens and adversarial renders; every rendered `.vtt` in all tests is also checked                                                                                                |
| `TestSlideSelection`         | CT-TPL-12: `recap tool normalize` of F-13 gives `testdata/meetings/F-13/expected/slides.json`; no presentation ID, original file name, default-deck, removed-deck, unshown or placeholder text in its output; F-01 gives `slides: []`                                                                                 |
| `TestSlideCallPlan`          | CT-TPL-13: slides on → exact `SLIDES` section in the single call and in consolidate, none in extract calls, a system message that names `SLIDES` and differs from the one with slides off, format version 1.1.0; slides off (no `inputs`, `slides: false`) → requests byte-identical to the same input without slides |
| `TestSlideCap`               | CT-TPL-14: per-slide cut at 1 000 characters; a section of about 6 000 characters (over the 2 000 minimum, under 15 % of the budget) arrives in full; titles only over the section limit; latest slides dropped when the titles alone are over it                                                                     |
| `TestSlideFencing`           | CT-TPL-15: fence markers, instructions, a heading and a link in slide text stay data inside the `SLIDES` section                                                                                                                                                                                                      |
| `TestSlideImageRendering`    | CT-TPL-16: `recap tool normalize --slide-images` of F-14 gives `expected/slide-images.json` `images_on`: images only where expected, PNG colour type 2, sizes, sample pixels (white where transparent), not blank, byte-identical re-run, no IDs or file names; without the flag `images_off` and no `image` field    |
| `TestSlideImageDedupe`       | CT-TPL-17: equal labels are equal bytes and one file, different labels differ, at most 50 images, the copy of a deck reuses its render                                                                                                                                                                                |
| `TestSlideDescribeRequests`  | CT-TPL-18: one vision call per distinct image to `vision-lab`, request shape (model, temperature 0, `max_tokens` 700, system string, text part plus the exact data URL, schema), no meeting content, all before summarize, no description in stderr; F-14's rendered images each described once                       |
| `TestSlideDescriptionSchema` | CT-TPL-19: `testdata/render/llm/slide-description.v1/{valid,invalid}.*.json`                                                                                                                                                                                                                                          |
| `TestSlideImageMerge`        | CT-TPL-20: exact `SLIDES` section with descriptions, text and a left-out image-only slide; format 1.2.0; system message changes only with a description; fence markers in a description stay data                                                                                                                     |
| `TestSlideImageFallback`     | CT-TPL-21: invalid reply (with two repair calls), 500, 400, `finish_reason: "length"`, empty description → text; five images failing `retryable` → at most 3 + (`max_inflight` − 1) calls, then text; all failing → requests equal to images off; 401 → `permanent`/`config_error` and no summarize call              |
| `TestSlideImagesOff`         | CT-TPL-22: `slide_images: false` or absent → no vision call, requests equal to the same slides without images                                                                                                                                                                                                         |
| `TestSlideImagesConfig`      | CT-TPL-23: images on with `defaults.vision` accepted; `slide_images` without `slides` names `slide_images`; without `defaults.vision` names `vision`                                                                                                                                                                  |
| `TestVTTMapping`             | gap variant (failed-track gap, `en` and `de`) and `mapping` golden: pause, gap NOTEs incl. tie, after a pause NOTE and at the end, +1 ms, clamping, "Speaker n", escaping                                                                                                                                             |

The `TestSlide*` tests (templates.md §3.1, §3.2) were written before the implementation and are not changed to fit it.
`TestSlideCallPlan/…/off-no-inputs` and `…/off-slides-false` guard that requests without slides stay unchanged.

## Not implemented yet

Written before the implementation of slide images (templates.md §3.2, ADR-017); they fail until it lands:
`TestSlideImageRendering`, `TestSlideImageDedupe`, `TestSlideDescribeRequests`, `TestSlideDescriptionSchema`,
`TestSlideImageMerge`, `TestSlideImageFallback`, `TestSlideImagesOff` and `TestSlideImagesConfig` (CT-TPL-16 to 23).
Against the current tool: `normalize` and `render` exit 2 on the unknown flags `--slide-images` and `--images`, the
meeting-types file is refused because `slide_images` is an unknown input, the providers file because `vision` is an
unknown key, and `--check-schema` because the format has no `slide-description.v1`. The configuration subtests
(`TestSlideImagesConfig/…/without-slides`, `…/without-defaults.vision`) fail too, because the tool stops at the
unknown `--images` flag before it reads the files. Only `TestSlideImageRendering/CT-TPL-16/images-off` passes already:
it guards that the text-only selection stays as it is. Format version 1.2.0 also changes the protected
goldens and the version `TestFormatSelection` and `TestSlideCallPlan` expect, in a dedicated fixture change ahead of
the implementation (templates.md §8).

## Manual check: goldens through y-provider `/convert` (optional, also in CI)

Needs a local Docs y-provider at the pinned Docs version (`suitenumerique/docs @ 6c730f7`). Its request
authentication (backend-signed JWT) and its output (`application/vnd.yjs.doc`) are only known from code reading
(`src/backend/core/services/converter_services.py:131-189` and
`src/frontend/servers/y-provider/src/handlers/convertHandler.ts:88-100,125-133` @ 6c730f7), so this is not automated
here.

1. Start the y-provider of the pinned Docs version locally (no network beyond localhost, fixture data only).
2. Convert `testdata/render/F-01/summary.md`, `F-01/summary.de.md` and `gap/summary.md` the way Docs' `YdocConverter`
   does and read the resulting BlockNote blocks.
3. Expect for F-01: paragraph, paragraph, heading 2, paragraph, heading 2, heading 3, paragraph (×3 topic pairs),
   heading 2, bullet list, heading 2, check list, heading 2, bullet list, heading 2, paragraph, table, divider,
   paragraph (the order `TestDocsDialect` checks). Escaped characters appear as literal text; `[hh:mm:ss]` is text,
   not a link. Record the output; drift goes to human review.

## Run

```sh
make contracts                                     # all skip
make build && RECAP_BIN=./bin/recap make contracts
```
