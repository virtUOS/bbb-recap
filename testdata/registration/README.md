# testdata/registration

Registration response fixtures for CT-REG-01 and CT-REG-10 (`docs/reference/metadata-interface.md` §2.3, §2.7, §7;
`docs/reference/schemas/registration.schema.json` `$defs/registration_response`). Synthetic only (F-01 IDs). Written by hand.

| File | Content |
|---|---|
| `responses/valid.<case>.json` | response bodies that the schema must accept: `registered` without job, a held job (result fields `null`/`false`), delivered to Docs (`document_url` set), delivered to the `frontend` sink (`document_url` `null`, `summary_available` `true`), delivered and then `cleaned`, released without a document ID, cancelled |
| `responses/invalid.<case>.json` | `valid.delivered-docs.json` with exactly one violation, named by `<case>`: a missing result field, a wrong type, an unknown `outcome`, a relative `document_url`, an extra `job` field, `schema_version` `2.0.0` |
| `responses/valid.<case>.json`, `schema_version` 1.3.0 (ADR-016) | `delivered-multi` (Docs, `frontend` and Matrix sinks, top-level fields derived from `deliveries`), `delivered-skipped` (one sink `skipped_by_admin`), `failed-partial` (one sink `sent`, one `failed`), `held-no-sinks-yet` (`deliveries` `[]`) |
| `responses/invalid.deliveries-<case>.json` | `valid.delivered-multi.json` with exactly one violation: `deliveries` missing at 1.3.0, an unknown `state`, an extra entry field, a missing `summary_available`, five entries |

| `responses/valid.registered-1.3.0.json`, `responses/invalid.result_url-<case>.json` | 1.3.0 top-level `result_url`: set on a registered response; missing at 1.3.0, `http://`, user info, whitespace, relative, over 2048 characters |
| `requests/valid.<case>.json`, `requests/invalid.<case>.json` | request 1.1.0 with `result_url` (CT-REG-10): valid with path, port, query, fragment; invalid `http://`, user info, whitespace, relative, over 2048 characters, a number, `null` |

The 1.0.0 request fixtures stay in `meetings/F-NN/registration.json`.
