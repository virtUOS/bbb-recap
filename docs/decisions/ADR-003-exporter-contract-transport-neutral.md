# ADR-003 Exporter contract is transport-neutral; pull API is a separate project

Status: accepted (2026-10-29)

## Context

Several consumers (this service, Opencast, others) need BBB raw data. BBB issue
[#24294](https://github.com/bigbluebutton/bigbluebutton/issues/24294) proposes a download API but is stale and has no
ready signal, integrity check, acknowledgement or deletion
([upstream facts](../reference/upstream-facts.md#bbb-recording-download)).

## Options

1. Push only.
2. Pull API only.
3. One contract (package and states), two transports.

## Decision

3. The package format and the exporter states `ready → delivered → acked → deleted` do not depend on the transport.
   This service uses push. A pull-API exporter is a separate project; it must never hand out the BBB shared secret.

## Consequences

- [`bbb-exporter.md`](../reference/bbb-exporter.md) describes both transports against one state model.
- The package layout follows the tar layout proposed in #24294 (`<format>/<recordID>/…`, `raw` as format).
