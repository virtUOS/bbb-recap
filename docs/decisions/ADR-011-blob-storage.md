# ADR-011 Blob storage: job-scoped hashed keys, S3 and filesystem backends

Status: accepted (2026-10-29), amended 2026-10-30

## Context

Global content addressing needs reference counting or garbage collection, which delays deletion for privacy; audio
is unique per meeting anyway. Several workers need shared storage.

## Options

- a. Job-scoped hashed keys `jobs/<job_id>/sha256/<hex>`.
- b. Global content-addressed storage with deduplication.

## Decision

a. One blob-store interface with two backends and the same layout: **filesystem** (a volume shared by api and worker;
the compose default) and **S3** (an operator's existing S3; required on Kubernetes, where shared read-write volumes
are not assumed). Every blob is checked against the manifest SHA-256. Deleting a job deletes its prefix
([`architecture.md` §8](../reference/architecture.md)).

## Consequences

- No deduplication across jobs.

## Amendment 2026-10-30

The compose stack ships no object store. MinIO is used only as a development and test S3 server in the `dev`
profile (its community edition is archived since 2026-04, https://github.com/minio/minio). This replaces "S3 is the
normal backend (MinIO under compose)".
