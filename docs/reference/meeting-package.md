# Meeting package v1

What leaves a BBB server for one recording. Decisions: [ADR-001](../decisions/ADR-001-scope-livekit-bbb4.md),
[ADR-003](../decisions/ADR-003-exporter-contract-transport-neutral.md),
[ADR-004](../decisions/ADR-004-meeting-package.md), [ADR-011](../decisions/ADR-011-blob-storage.md). Names come from
[`README.md`](README.md). Wire transfer: [`bbb-exporter.md`](bbb-exporter.md). Schema:
[`schemas/manifest.schema.json`](schemas/manifest.schema.json) (`$id` `urn:recap:schema:manifest:1`, `schema_version`
1.0.x). BBB sources are `bigbluebutton/bigbluebutton` at `7d27e7e` unless noted; see also
[`upstream-facts.md`](upstream-facts.md).

## 1. What a package is

The unit of data that leaves a BBB server for one recording: **allow-listed raw files, copied byte for byte** from
`raw/<record_id>/`, plus a **manifest** with role, size and SHA-256 per file, the meeting IDs and the opt-in metadata.
The exporter transforms nothing; the service parses and normalizes. The format does not depend on the transport: the
exporter pushes it today; a pull API could serve it later.

## 2. Layout

One package per `record_id`, which names `raw/<record_id>` (`archive.rb:210-211,239 @ 7d27e7e`). Paths are relative
to the record directory.

```
manifest.json                                      the manifest; not listed in itself
events.xml                                         role events            (exactly one)
audio/microphone-<userId>-<trackId>-<suffix>.webm  role audio-microphone  (one or more)
notes/<file>                                       role notes             (zero or more)
presentation/<sub/path>                            role presentation      (zero or more)
```

Sources:

- `events.xml`, `audio/`, `notes/`, `presentation/` are written by `archive.rb`
  (`archive.rb:34-46,48-83,116-150,247 @ 7d27e7e`).
- LiveKit microphone files: `microphone-<userId>-<trackId>-<suffix>.webm`, `suffix = Date.now()`
  (`bigbluebutton/bbb-webrtc-sfu`, `lib/livekit/livekit-recorder-manager.js:264-285,400,419-441 @ c25fec6`). Archive
  keeps the name and remuxes with `-c copy` (`archive.rb:116-150`). Opus in WebM, 48 kHz
  (`bigbluebutton/bbb-webrtc-recorder`, `internal/webrtc/recorder/webm.go:24,136 @ 3d581ad`).
- Notes: `notes/notes.<fmt>`, default `html` and `pdf`; `notes/export_failed` marks a failed export
  (`archive.rb:48-83`, `bigbluebutton.yml:10-15 @ 7d27e7e`).
- `presentation/` is an rsync of `/var/bigbluebutton/<id>/<id>` (`archive.rb:247`). It is treated as an opaque
  subtree.

## 3. Tar (not supported yet)

The push transport uploads files one by one ([`bbb-exporter.md`](bbb-exporter.md) §6); no tar is built. A later pull
API or file export may serialize a package as a tar laid out `raw/<record_id>/manifest.json`, then the files in
manifest order, following [BBB issue #24294](https://github.com/bigbluebutton/bigbluebutton/issues/24294). Its byte
format will be specified with that feature. Package identity never depends on tar bytes: it is `package_id` plus the
per-file SHA-256 in the manifest.

## 4. Manifest (`schema_version` 1.0.0)

| Field            | Req. | Meaning                                                                                                                                                                                                                              |
| ---------------- | ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `schema_version` | yes  | `1.0.0`.                                                                                                                                                                                                                             |
| `created_at`     | yes  | RFC 3339 UTC, when the exporter built it.                                                                                                                                                                                            |
| `exporter`       | yes  | `{name, version}`; ours is `recap-exporter`.                                                                                                                                                                                         |
| `bbb_server_id`  | yes  | Must equal the server bound to the upload credential (V-I03).                                                                                                                                                                        |
| `record_id`      | yes  | BBB internal meeting ID.                                                                                                                                                                                                             |
| `meeting_id`     | yes  | `events.xml` `<meeting externalId>` (`events_archiver.rb:260-317 @ 7d27e7e`).                                                                                                                                                        |
| `bbb_version`    | no   | `<recording bbb_version>` if present. Informational.                                                                                                                                                                                 |
| `breakout`       | yes  | `<meeting breakout>`. Our exporter always writes `false`; ingest rejects `true` (V-I08).                                                                                                                                             |
| `meta_prefix`    | yes  | Configured prefix, default `recap`.                                                                                                                                                                                                  |
| `metadata`       | yes  | From `/recording/metadata`: keys starting with `<meta_prefix>-`, plus `bbb-origin` and `bbb-origin-server-name` if present. Lowercase, without `meta_` (`ParamsProcessorUtil.java:300-303`, `events_archiver.rb:286-292 @ 7d27e7e`). |
| `files`          | yes  | `{path, role, size, sha256}` for every file except `manifest.json`, sorted by `path` (bytewise UTF-8).                                                                                                                               |
| `totals`         | yes  | `{file_count, size}`.                                                                                                                                                                                                                |

The manifest has no names, emails or chat; those stay in `events.xml`, which is shipped **as is**
([ADR-004](../decisions/ADR-004-meeting-package.md)). The service derives speakers from
`AudioTrackPublishedEvent.filename` → `userId` → `ParticipantJoinEvent`.

**`package_id`** = lowercase hex SHA-256 of the RFC 8785 (JCS) canonical manifest ([`README.md`](README.md)). The
exporter writes the file in canonical form, so the file bytes hash to `package_id`. A rebuild gets a new `created_at`
and so a new ID; the exporter therefore builds the manifest once and keeps it ([`bbb-exporter.md`](bbb-exporter.md)).

**Paths:** relative, `/`-separated, no empty, `.` or `..` segment, no backslash, no control characters, ≤ 1024 bytes,
unique, matching the role pattern. Regular files only; symlinks and other types are skipped and counted.

## 5. File roles

| Role               | Selected from `raw/<record_id>/`                                                           | Count | Personal data                                                                                                                          |
| ------------------ | ------------------------------------------------------------------------------------------ | ----- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `events`           | `events.xml`                                                                               | 1     | Display names, `externalUserId`, `userdata`, roles, public chat, poll answers (with `userId` unless secret), whiteboard, all `meta_*`. |
| `audio-microphone` | files in `audio/` named `microphone-*.webm`                                                | ≥ 1   | Voice of one participant per file.                                                                                                     |
| `notes`            | `notes/notes.<fmt>` for `<fmt>` in `html`, `md`, `txt` (fixed), plus `notes/export_failed` | 0..n  | Notes text.                                                                                                                            |
| `presentation`     | every regular file below `presentation/`                                                   | 0..n  | Slide content, uploader file names.                                                                                                    |

**Excluded:** `video/`, `deskshare/`, `learning-dashboard/`, everything else. In `audio/`, only microphone files:
`screen_share_audio-*.webm` and FreeSWITCH files are excluded. The fixed notes formats skip the redundant PDF;
`md`/`txt` exist only if the BBB admin enables them (`new-features.md:280 @ 7d27e7e`).

**Screen-share audio** (`screen_share_audio-<userId>-<trackId>-<suffix>.webm`,
`livekit-recorder-manager.js:264-285 @ c25fec6`) is not a speaker track and may contain third-party media, so v1
excludes it.

Data flow:

| Data                            | Stored where                                                      | Retention                                                                                                                              | Deleted by                    |
| ------------------------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------- |
| Raw directory on BBB            | BBB disk                                                          | BBB cron or exporter policy ([`bbb-exporter.md`](bbb-exporter.md) §7); never automatically if all formats are disabled                 | BBB cron, exporter, BBB admin |
| Manifest copy in exporter state | exporter state dir                                                | until the state record is pruned                                                                                                       | exporter                      |
| Package in transit              | TLS only                                                          | —                                                                                                                                      | —                             |
| Package in the service          | blob store `jobs/<job_id>/sha256/<hex>`, manifest row in Postgres | until `cleanup` (default right after delivery); held/failed until an admin acts ([ADR-009](../decisions/ADR-009-privacy-retention.md)) | service `cleanup`             |

## 6. LiveKit-only rule

Only recordings with one file per microphone track are accepted
([ADR-001](../decisions/ADR-001-scope-livekit-bbb4.md)). Legacy bridges produce one mixed FreeSWITCH file, archived as
`.opus`, bracketed by `StartRecordingEvent`/`StopRecordingEvent` of module VOICE (`archive.rb:25-32,85-107`;
`RedisRecorderActor.scala:515-530 @ 7d27e7e`).

- **Exporter** (file names only): no microphone file ⇒ nothing is uploaded; reason `unsupported_audio_bridge` if
  `audio/` has other files, else `no_microphone_tracks`. A LiveKit meeting where nobody published a microphone while
  recording also has none (`livekit-recorder-manager.js:30-32,195-246 @ c25fec6`).
- **Service** (`validate`, authoritative, because other producers may exist): V-S02, V-S03.

## 7. Validation rules

**E** = exporter (state `failed`, nothing uploaded), **I** = ingest (HTTP problem, nothing kept beyond staging),
**S** = `validate` step (job `failed` or `held`).
Ingest order: parse and `schema_version` (V-I02 version part), then the §8 size limits (413), then the full schema
and the remaining rules. Files the schema allows are accepted even if the exporter's allow-list would not select them.

| ID    | Rule                                                                                                                                                                                                                                          | Where | Outcome                                                                               |
| ----- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----- | ------------------------------------------------------------------------------------- |
| V-E01 | `events.xml` exists, regular file                                                                                                                                                                                                             | E     | `no_events`                                                                           |
| V-E02 | ≥ 1 microphone file                                                                                                                                                                                                                           | E     | `no_microphone_tracks` / `unsupported_audio_bridge`                                   |
| V-E03 | file, package and count limits (§8)                                                                                                                                                                                                           | E, I  | `limit_exceeded` / 413 `package-too-large`                                            |
| V-E04 | file bytes still match the manifest during upload                                                                                                                                                                                             | E     | `source_changed`                                                                      |
| V-I01 | SHA-256 of the manifest bytes = `package_id`; bytes are JCS-canonical                                                                                                                                                                         | I     | 400 `package-id-mismatch`                                                             |
| V-I02 | manifest valid for its `schema_version`; major 1; minor not newer than supported                                                                                                                                                              | I     | 422 `manifest-invalid` / `unsupported-schema-version`                                 |
| V-I03 | `bbb_server_id` = server of the credential                                                                                                                                                                                                    | I     | 403 `server-mismatch`                                                                 |
| V-I04 | paths unique and sorted, totals right, role patterns hold                                                                                                                                                                                     | I     | 422 `manifest-invalid`                                                                |
| V-I05 | each upload matches its SHA-256 and size                                                                                                                                                                                                      | I     | 422 `file-hash-mismatch`                                                              |
| V-I06 | no other `package_id` for the same (`bbb_server_id`, `record_id`)                                                                                                                                                                             | I     | 409 `record-already-ingested`                                                         |
| V-I07 | `metadata["<prefix>-enabled"]` = `true`                                                                                                                                                                                                       | I     | 422 `not-opted-in`, nothing stored                                                    |
| V-I08 | `breakout` = `false`                                                                                                                                                                                                                          | I     | 422 `manifest-invalid`                                                                |
| V-S01 | `events.xml` well-formed; `/recording/@meeting_id` = `record_id` and, if present, `<meeting id>` = `record_id`; `externalId` = `meeting_id`; a recording start with `timestampUTC` exists ([`providers.md`](providers.md) §4.3 items 1 and 6) | S     | failed `events_mismatch`                                                              |
| V-S02 | ≥ 1 `AudioTrackPublishedEvent` (module `bbb-webrtc-sfu`) with `source=microphone`                                                                                                                                                             | S     | failed `unsupported_audio_bridge`                                                     |
| V-S03 | no `StartRecordingEvent` of module VOICE                                                                                                                                                                                                      | S     | failed `unsupported_audio_bridge`                                                     |
| V-S04 | manifest `metadata` = `/recording/metadata` in `events.xml`                                                                                                                                                                                   | S     | failed `manifest_metadata_mismatch`                                                   |
| V-S05 | each microphone file has a matching `AudioTrackPublishedEvent` by basename (`generators/audio.rb:137-145 @ 7d27e7e`)                                                                                                                          | S     | failed `all_tracks_failed` if **none** matches; a single unmatched file becomes a gap |
| V-S06 | version, type, sink checks                                                                                                                                                                                                                    | S     | held per [`metadata-interface.md`](metadata-interface.md) §3                          |
| V-S07 | registration exists, may claim the server, `meeting_id` matches                                                                                                                                                                               | S     | held `no_registration`                                                                |

A re-export of an ingested record is refused (V-I06); new output comes from an admin `rerun`.

## 8. Size, integrity, versioning

- Integrity: per-file SHA-256, `package_id` over the manifest, checked on every hop (exporter while uploading, ingest
  while receiving, blob store on write, [ADR-011](../decisions/ADR-011-blob-storage.md)). Authenticity: per-server
  bearer credential over TLS; no signature in v1.
- Limits are fixed constants, the same in exporter and service: 2 GiB per file, 8 GiB per package, 10 000 files.
- Versioning: semver. Patch = text only; minor = new optional fields or roles; major = breaking. Readers accept major 1
  up to the highest minor they know, so the service is upgraded before the exporter. Schema files per minor live side
  by side (`manifest.schema.json` = 1.0.x).

## 9. Contract tests

| ID        | Test                                                                                                                                                                 |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CT-PKG-01 | Golden manifests validate against the schema; each `testdata/manifests/invalid/*.json` fails for its documented reason.                                              |
| CT-PKG-02 | `package_id` of each golden manifest = golden `package_id.txt`; the manifest bytes are already canonical.                                                            |
| CT-PKG-03 | Allow-list on F-01 selects exactly the golden file list; `video/`, `deskshare/`, `learning-dashboard/`, `screen_share_audio-*`, `notes.pdf`, the symlink are absent. |
| CT-PKG-04 | Reserved (was the tar format, which is not built).                                                                                                                   |
| CT-PKG-05 | V-I01…V-I08, one fixture or mutation each.                                                                                                                           |
| CT-PKG-06 | V-S01…V-S05 on F-01 (pass), F-02 (`unsupported_audio_bridge`) and the mutation set.                                                                                  |
| CT-PKG-07 | JCS output matches the RFC 8785 test vectors.                                                                                                                        |

## 10. Known limitations

- Screen-share audio is not exported (§5). A role for it may be added later.
- If a LiveKit server can also write a VOICE `StartRecordingEvent`, V-S03 would reject a valid recording. Not
  observed so far.
- Not checked against the BBB source: the channel count of the microphone WebM files, the exact rendering of
  `AudioTrackPublishedEvent`, the encoding of `<meeting breakout>`, the layout of `presentation/`, and the
  `events.xml` attributes used by V-S01.
- The size limits (§8) are assumptions; realistic package and `presentation/` sizes were not measured.
- Whether `notes/` attributes text to authors was not checked.
- The package has no signature. A pull API would need one.
