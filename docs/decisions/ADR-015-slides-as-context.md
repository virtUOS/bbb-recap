# ADR-015 Slide text as context for the summary

Status: accepted (2026-11-13)

## Context

Slides often carry the agenda, the structure and the exact terms of a meeting (project names, figures, the decisions
to take). The summary sees only speech (with recognition errors), chat, shared notes and polls. The package already
contains `presentation/` (ADR-004), and BBB writes the text of every converted page to
`presentation/<pres-id>/textfiles/slide-<n>.txt`; `events.xml` says which deck and page were shown when
([upstream facts](../reference/upstream-facts.md#bbb-presentations)). The pipeline ignores both today.

Slides are prepared material: a deck may hold pages the meeting never reached, the default welcome deck, personal
data on title pages, and text written as instructions. Slide text competes with the transcript for the LLM budget
([`providers.md` §3.5](../reference/providers.md)).

## Options

- a. **BBB text files of the slides that were shown**, as a capped data section, switched on per meeting type. Pure
  Go, no new dependency; depends on BBB's text extraction; image-only slides give nothing.
- b. Text extraction from the PDF in the service. Works without `textfiles/`, but parses untrusted PDFs in the worker
  and needs a PDF library or `pdftotext`, which does not fit a `scratch` image.
- c. OCR or a vision model on slide images. Covers image-only slides, but adds a provider type, cost and more data to
  a third party.
- d. Slide titles only, as markers in the transcript. Small, but loses agenda and term detail.

## Decision

a, with d's titles as the fallback when the size cap is hit.

- **Source:** only BBB's `textfiles/slide-<n>.txt`. A page whose file is missing, empty or holds BBB's placeholder
  text gives no slide.
- **Which slides:** only pages shown inside a recording period for at least 5 s without a change. The default deck
  (`default.pdf`) is skipped. Unshown pages are never used.
- **Boundary (ADR-013):** `normalize` turns them into a source-neutral field `slides[]` of the normalized meeting
  (`{t_ms, deck, page, text}`, `deck` an opaque ref `d-<n>`), which bumps its version to 4. A version 3 blob is read
  as "no slides". No presentation ID and no original file name leave `normalize`.
- **Opt-in:** slides are an input the format declares and a meeting type switches on (`inputs: {slides: true}` in
  the meeting-types file); default off. With it off, nothing about slides reaches the LLM, not even the prompt
  text that explains them.
- **Calls:** a `SLIDES` data section in the single `summarize` call and in `consolidate`, never in `extract` or
  `merge` calls, like chat and notes.
- **Size:** at most 1 000 characters per slide; the section at most 15 % of the call budget (at least 2 000
  characters); over that, titles only, then the latest slides are dropped. Constants, not settings.
- **Prompt:** when the section is sent, the system message names slides as prepared material that the meeting may not have covered: useful for terms,
  spelling and structure, never evidence that something was discussed or decided.
- The format `meeting-summary` becomes version 1.1.0.
- A run on a real BBB 4.0 recording confirms the `textfiles/` layout before the feature is relied on; until then the
  layout is known from BBB's code only.

## Consequences

- Slide text of shown slides reaches the LLM provider for meeting types that switch it on. The provider's
  declaration lists the data category `slides` ([`privacy.md`](../reference/privacy.md) §2, §7).
- Slide text is stored in the normalized meeting blob and deleted with it at `cleanup` (ADR-009); no new store.
- Slide text is presenter-written and may be instruction-shaped. It gets the same fencing as chat and notes; the
  residual risk that the LLM follows it is the same class as for chat and is accepted.
- Image-only slides and scanned decks contribute nothing. Service-side extraction (b) stays an option if real
  recordings show missing or poor `textfiles/`.
- The format version in the summary footer changes, so the summary goldens change with the implementation.

## Sources

ADR-004, ADR-008, ADR-009, ADR-013; [`templates.md` §3.1](../reference/templates.md),
[`privacy.md`](../reference/privacy.md), [`upstream-facts.md`](../reference/upstream-facts.md#bbb-presentations).
