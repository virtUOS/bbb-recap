# ADR-017 Slide images as context for the summary

Status: accepted (2026-11-17). Supersedes the rejection of option c in ADR-015; the rest of ADR-015 stays.

## Context

ADR-015 sends the text that BBB extracts from each shown slide (`textfiles/slide-<n>.txt`). That text is often poor:
image-only slides, scanned decks, charts, diagrams and screenshots give no text or BBB's placeholder, and layout text
loses tables and reading order. Vision-capable open models served with vLLM's OpenAI-compatible API (the service's
LLM dialect, ADR-008) accept images as `image_url` content parts in base64 `data:` URLs
([upstream facts](../reference/upstream-facts.md#providers)).

The raw recording keeps, per uploaded deck, the converted PDF `<pres-id>.pdf` (or the uploaded image), one SVG and one
text file per page and small thumbnails ([upstream facts](../reference/upstream-facts.md#bbb-presentations)). BBB's
SVG is a lossy derivative of the PDF page: for complex pages it wraps a raster image of at most 1440×1080, on a
failed conversion it is blank, and its text is drawn as glyph paths. A test with a synthetic deck found no maintained
pure-Go SVG rasterizer that draws BBB-style SVGs (embedded images, `use`, masks), while PDFium compiled to
WebAssembly, run by the pure-Go runtime wazero (`github.com/klippa-app/go-pdfium`, WebAssembly mode), rendered every
page correctly and with the same bytes on every run, in a static binary built with `CGO_ENABLED=0`.

Slide images can show more personal data than slide text: faces, handwriting, screenshots with names, e-mail or chat.
They go to a provider only if the operator switches them on.

## Options

- a. **Render the PDF page in the worker with go-pdfium in WebAssembly mode**, send the image to a vision model,
  use the description as slide text. One static binary, no new container, the PDF parser runs inside the WebAssembly
  sandbox. About +12 MB binary, about 2 s start-up per render run and about 300 MB peak memory while rendering
  (measured on a synthetic 3-page deck).
- b. Rasterize BBB's SVG in pure Go. No tested library draws the images and masks of BBB's SVGs.
- c. A separate renderer container with poppler (`pdftoppm`) behind an internal HTTP API. Same renderer as BBB, but a
  new service, image, network edge and manifests, and its image cannot follow the `scratch` convention.
- d. MuPDF (go-fitz, MuPDF.js). AGPL-3.0, not compatible with the project's licence choice.
- e. Send the PDF itself to the provider. Not part of the OpenAI-compatible chat API that the dialect speaks.

## Decision

a, with c as the fallback if go-pdfium fails on real decks. The step interface does not change with the renderer.

- **Renderer (D1):** go-pdfium in WebAssembly mode on `presentation/<pres-id>/<pres-id>.pdf`; an image upload
  (`png`, `jpg`, `jpeg`, `webp`) is decoded directly. No filesystem is mounted into the WebAssembly runtime; the PDF is
  passed as bytes, memory and time are capped. The version is the latest release when the code lands (go-pdfium
  v1.21.5 of 2026-11-17 at the time of writing, `proxy.golang.org`).
- **Switch (D2):** a new optional input `slide_images`, a boolean next to `slides` in the meeting type's `inputs`
  (the inputs map stays a map of booleans). It needs `slides: true` on the same type: images only replace or extend
  the text of the `SLIDES` section, whose selection, caps and prompt rules they reuse. Default off.
- **Vision profile (D3):** `defaults.vision` in the providers file names an `openai-chat` LLM profile, which may be
  the same as `defaults.llm` (one multimodal model) or another one. Schema version 1.1.0 of the providers file.
- **Merge (D4):** a slide with a description uses it in place of BBB's text; otherwise BBB's text is used as today.
  One source per slide, never both, because the description already contains the visible text.
- **Failure (D5):** a vision call that fails for one image, also after all queue retries, leaves that slide with
  BBB's text. The job neither fails nor holds for it; a metric counts the fallbacks. The delay is bounded: each
  vision call has its own 120 s timeout, and on the step's last attempt no new call starts after 3 `retryable`
  results in a row. Only `permanent_config` (wrong
  credentials, wrong URL) fails the job, as for every other provider.
- **One image per call (D6):** simple retries per image, small outputs, and it works with a server that allows one
  image per prompt.
- **Caps (D7):** long edge 1 280 px, PNG, at most 50 page renders per job, at most 1 000 characters per description
  (the existing per-slide cap), image uploads checked by their header against 50 megapixels before decoding. Constants, not settings, and **provisional** until the staging measurement (below).
- **Steps (D8):** `normalize` renders (it is the BBB-specific step that reads `presentation/`); a new source-neutral
  step `describe` between `assemble` and `summarize` calls the vision model, one image per call, with progress per
  image in `step_items`.
- **Consent (D9):** the front end's consent and recording notice names "the slides shown during the recording, as
  images" as data that goes to the AI provider when a room type uses such a meeting type.
- **Render only when switched on (D10):** `normalize` renders only for jobs whose meeting type has
  `slide_images: true`. Other jobs pay nothing and no image exists for them. This is an exception to the rule of
  ADR-015 that the switch does not change what `normalize` writes.
- **Staging first (D11):** before the feature is relied on, the staging deck's `<pres-id>.pdf` is rendered and
  compared with BBB's `svgs/`, and the latency and description quality of each candidate model are measured. The caps
  of D7 are fixed after that measurement.

The format `meeting-summary` becomes version 1.2.0; the normalized meeting becomes version 5.

## Consequences

- PNG images of the shown slides reach the vision provider for meeting types that switch them on. The provider's
  declaration lists the data category `slide_images` ([`privacy.md`](../reference/privacy.md) §12).
- Images and descriptions are job blobs and are deleted with the job at `cleanup` (ADR-009); nothing new is
  delivered.
- Text inside an image can be instruction-shaped. The describe call has no tools and a schema-checked output, and the
  description is fenced data in `summarize`, as slide text is; the residual risk is the one accepted for slide text.
- The worker binary grows by about 12 MB, and a render run needs about 2 s and up to about 300 MB more memory
  ([`deployment.md`](../reference/deployment.md) §4).
- PDFium in WebAssembly is a patched build maintained by one person and updated with go-pdfium releases. If it stalls
  or fails on real decks, option c replaces the renderer.
- A new step changes the step list and the database constraint on step names (a new migration).
- The format version in the summary footer changes, so the summary goldens change in a dedicated fixture change ahead
  of the implementation ([`templates.md` §8](../reference/templates.md)).

## Later

- Option c (poppler in its own container) if go-pdfium fails on real decks.
- Validation on staging (D11), then the caps of D7 lose their provisional status.
- SVG uploads and other sources of slide images (another meeting source, ADR-013) may fill the same field.

## Sources

ADR-004, ADR-008, ADR-009, ADR-011, ADR-013, ADR-015; [`templates.md` §3.2](../reference/templates.md),
[`providers.md` §3.6](../reference/providers.md), [`architecture.md` §5.2](../reference/architecture.md),
[`privacy.md` §12](../reference/privacy.md), [`upstream-facts.md`](../reference/upstream-facts.md#bbb-presentations).
go-pdfium: https://github.com/klippa-app/go-pdfium (README "WebAssembly", licence MIT; PDFium BSD-3-Clause style;
wazero Apache-2.0).
