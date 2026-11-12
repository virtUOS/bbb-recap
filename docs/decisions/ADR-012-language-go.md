# ADR-012 Implementation language: Go

Status: accepted (2026-10-29)

## Context

The exporter runs on BBB servers; the service talks to providers over HTTP and JSON. Both should be small, static
and easy to deploy.

## Options

- a. Go for both.
- b. Python for both.
- c. Go exporter, Python service.

## Decision

a. Go for the exporter and the service. The post_archive hook stays a minimal Ruby script, because BBB runs its
recording scripts in Ruby. Prompts and output formats are data files, not code.

## Consequences

- Small static images and one toolchain. Needs that only Python can meet can be served by a separate worker on the
  queue contract.
- The template engine for later admin-defined templates is not chosen yet.
