# Golden ingest exchanges

Golden HTTP exchanges of the push ingest protocol (`docs/reference/bbb-exporter.md` §6.6), used by
`contracts/ingest`. Written by `make_exchanges.py` from the fixture manifests (run it from the repo root with
`python3 -B -I testdata/ingest/exchanges/make_exchanges.py`; output is deterministic).

## Files

| File | Case | Package | Sides |
|---|---|---|---|
| `01-not-opted-in.json` | `recap-enabled=yes` → 422, nothing stored (M-12); the opted-in package for the record is then new (201) | F-01 record, `bodies/M-12…` | service |
| `02-unauthorized-401.json` | wrong bearer token → 401 | F-04 | both |
| `03-server-mismatch-403.json` | `bbb-fixture-b` credential → 403 `server-mismatch` (M-07) | F-04 | both |
| `04-happy-path.json` | manifest, all files, re-check, commit, status | F-01 | both |
| `05-lost-commit-response.json` | commit response lost (exporter's fake drops it); re-check says `committed`; same ack again | F-01 | both |
| `06-record-already-ingested-409.json` | other `package_id` for an ingested record (M-08) | F-01 record, `bodies/M-08…` | both (first F-01 PUT service only) |
| `07-hash-mismatch.json` | upload with one flipped byte → 422 `file-hash-mismatch` (M-01) | F-04 | both |
| `08-resume.json` | upload cut mid-body; re-check lists it again; finish | F-04 | both |
| `09-duplicate-file.json` | the same upload twice → `204`, then `200` | F-06 | both (repeat service only) |
| `10-retry-after-429.json` | 429 with `Retry-After: 40`, then success | F-01 | exporter |
| `11-unavailable-503.json` | 503 on an upload; after backoff the re-check (200 `receiving`) and success | F-01 | exporter |
| `12-duplicate-hash.json` | one hash under two paths: listed and uploaded once, `file_count` counts both (§6.3 step 5) | F-01 + copy, `bodies/F-01.duplicate-hash…` | both |

`bodies/` holds request bodies that are not fixture files: `{}` for commit, and the two manifests derived from
F-01's golden manifest by one value each (M-12: `recap-enabled` `yes`; M-08: `created_at` one hour later), and
F-01's manifest plus `svgs/slide1-copy.svg`, a byte copy of `slide1.svg` (case 12). All are JCS-canonical; their
SHA-256 is their `package_id`. Case 12 does not pin `received_size`/`expected_size`: whether a shared blob counts
once or per path is not specified.

## Format

```
case, pins (spec sections), fixture (F-NN raw dir for the exporter side), record_id,
raw_copies: optional [{from, to}], files the exporter side copies inside its temp raw dir (paths in the record dir),
exporter: {state, reason} expected in the exporter's state.json, or null (service side only),
steps: [ {
  sides: ["service"|"exporter"]     optional, default both
  request: { method, path, credential, headers, body, fault }
    credential   file in testdata/secrets/clients/, or "invalid"; the token itself is never recorded
    body         null, or {source (relative to testdata/), flip_byte (optional), sha256, size}
                 sha256 and size are those of the bytes sent (after flip_byte)
    fault        "abort_body": the upload is cut after about half of the body (no response)
  response: { status, headers, body, fault } or null when the request is aborted
    body         expected JSON, null = not checked (service) / empty (exporter's fake server)
    fault        "drop": the exporter's fake server closes the connection instead of answering
  min_delay_s: exporter side: least time since the previous response
} ]
```

Matching rules (service side): status exact; listed headers exact, `Content-Type` by media type; body objects
match when every listed key matches (extra keys are allowed); arrays match in any order. `{{uuid:NAME}}` matches
a lowercase UUIDv7, `{{time:NAME}}` an RFC 3339 UTC timestamp; every later use of the same NAME within a case must
be equal (so a repeated commit returns the identical ack).

## Running

- **Service side** (`RECAP_CONTRACT_BASE_URL`): each case runs on its own, against any service with the fixture
  clients (`testdata/secrets/clients`) and meta prefix `recap`; it may already hold data, and re-runs are fine
  (contracts/README.md). Per case, every golden manifest is first rebuilt unchanged and checked
  against its golden `package_id`, then rebuilt with a fresh random `record_id` (same `<40 hex>-<ms>` shape; one
  fresh ID per fixture `record_id`, so M-08 and F-01 still share a record within case 06). The golden
  `package_id`s and `record_id`s in paths and expected bodies are replaced by the derived ones. Placeholder bindings
  are per case.
- **Exporter side** (`RECAP_EXPORTER_BIN`): each case runs `recap-exporter run` with a copy of the fixture raw dir,
  a spool entry and a TLS fake server that answers with the golden responses. The fake replaces the golden
  `package_id` with the one the exporter computed (its manifest has its own `created_at` and version) and
  placeholders with fixed values. It accepts `GET` for a manifest re-check (§6.3).
