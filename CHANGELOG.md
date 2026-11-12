# Changelog

All notable changes to this project are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

First release.

### Added

- BBB side: `post_archive` hook and `recap-exporter` daemon that package a recorded, opted-in meeting (events, one
  audio file per speaker, notes, slides) and push it to the service over HTTPS with resumable, verified uploads;
  optional deletion of the raw recording after acknowledgement. Runs as a container on packaged and docker-based BBB.
- Service (`recap api`, `recap worker`, `recap migrate`): ingest API, registration API for front ends, Postgres job
  queue with steps `validate`, `normalize`, `transcribe`, `assemble`, `summarize`, `render`, `deliver`, `cleanup`,
  retries, holds and leases.
- Providers: ASR dialects `whisperx-family` (whisperx-api-server) and `transkribierbar`; OpenAI-compatible chat
  completions for the language model, with structured output and chunking for long meetings.
- Built-in output format `meeting-summary` (Markdown summary, WebVTT transcript), output in the meeting's language.
- Sinks: La Suite Docs (`create-for-owner`) and `frontend` (the front end fetches the Markdown); result record for
  front ends for `RECAP_RESULT_TTL`.
- Admin API and admin web pages with OIDC login; actions `retry`, `release`, `rerun`, `purge`, `erase`; audit log.
- Privacy defaults: content deleted right after delivery, configurable retention, no content in logs, optional
  provider data declarations.
- Prometheus metrics and health endpoints; docker compose deployment with optional Caddy TLS; filesystem and S3 blob
  backends; images on ghcr.io.
