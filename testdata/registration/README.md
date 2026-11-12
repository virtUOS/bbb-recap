# testdata/registration

Registration response fixtures for CT-REG-01 (`docs/reference/metadata-interface.md` §2.3, §2.7, §7;
`docs/reference/schemas/registration.schema.json` `$defs/registration_response`). Synthetic only (F-01 IDs). Written by hand.

| File | Content |
|---|---|
| `responses/valid.<case>.json` | response bodies that the schema must accept: `registered` without job, a held job (result fields `null`/`false`), delivered to Docs (`document_url` set), delivered to the `frontend` sink (`document_url` `null`, `summary_available` `true`), delivered and then `cleaned`, released without a document ID, cancelled |
| `responses/invalid.<case>.json` | `valid.delivered-docs.json` with exactly one violation, named by `<case>`: a missing result field, a wrong type, an unknown `outcome`, a relative `document_url`, an extra `job` field, `schema_version` `2.0.0` |

The request fixtures stay in `meetings/F-NN/registration.json`.
