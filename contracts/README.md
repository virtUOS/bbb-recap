# Contract tests

Black-box tests that pin the versioned contracts. They are language-neutral, so another implementation can be
tested the same way. They change only together with the contract they pin; an implementation makes them pass.

They find what they test through environment variables. Without a variable, a test skips and names it.

| Variable                  | Used for                                                                                                                                                                               |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `RECAP_BIN`               | pure functions via `recap tool <name>` (JSON in, JSON out)                                                                                                                             |
| `RECAP_CONTRACT_BASE_URL` | HTTP APIs of a running service                                                                                                                                                         |
| `RECAP_EXPORTER_BIN`      | the exporter                                                                                                                                                                           |
| (none)                    | the post_archive hook: `make hook-test` runs `hook/post_archive/test/*_test.rb` with `ruby` from `PATH`, else in podman with `docker.io/library/ruby:3.2-slim-bookworm` (never pulled) |

A relative `RECAP_BIN` or `RECAP_EXPORTER_BIN` is resolved against the module root (the directory with
`go.mod`), not against the test package directory.

Run:

```sh
make contracts                                    # everything skips that has no variable
make build && RECAP_BIN=./bin/recap make contracts
make hook-test                                    # hook tests on Ruby 3.2 and 4.0 (local ruby, else podman)
```

The harness's own unit tests also run under `make test`.

Against the contract environment (`tools/contract-env/run.sh`) on rootless podman (pasta). The address variables
(`RECAP_CONTRACT_DOCS_ADDR`, `RECAP_CONTRACT_HOOKS_ADDR`) are read twice: by `run.sh up`, as the address the containers
connect to, and by the tests, as the address they listen on. The two values differ, with the same port:

- at `up`: `host.containers.internal:<port>`, the containers' name for the host. It does not resolve on the host.
- for the tests: the host's LAN address and the port. A listener on `127.0.0.1` is not reachable from the containers,
  and the LAN address does not work at `up`: pasta gives the container namespace the host's LAN address, so a
  container connecting to it reaches itself.

```sh
make build
NAME=recap-ce-$$ API_PORT=18080 DOCS_PORT=18081
HOST_IP=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
RECAP_CONTRACT_DOCS_ADDR=host.containers.internal:$DOCS_PORT tools/contract-env/run.sh up "$NAME" "$API_PORT"
RECAP_BIN=./bin/recap RECAP_CONTRACT_BASE_URL=http://127.0.0.1:$API_PORT \
  RECAP_CONTRACT_DOCS_ADDR=$HOST_IP:$DOCS_PORT go test -count=1 ./contracts/sink/
tools/contract-env/run.sh down "$NAME"
```

`RECAP_CONTRACT_HOOKS_ADDR` follows the same split, on a port of its own. Where one address is reachable from both
sides, the same value serves both.

## Conventions

- Log scans read the log of the binary under test from `RECAP_CONTRACT_LOG_FILE`: JSON lines, all levels including
  DEBUG, written by whoever starts the binary. Unset: only the scan part skips; the rest of the test runs.
- The contract environment (`tools/contract-env/`) runs the service with `RECAP_RETENTION_AFTER_DELIVERY` above zero
  (e.g. `1h`), so tests can check behaviour between delivery and `cleanup`. A test that needs this fails with a hint
  when it is 0.
- HTTP tests that ingest derive fresh `record_id`s from a fixture (F-07 shares F-01's `record_id` on purpose). They
  first rebuild the unchanged fixture and check it against its golden `package_id`.
- Sink tests that drive the service serve their own mock Docs on `RECAP_CONTRACT_DOCS_ADDR` (`host:port`). The
  contract environment configures a sink profile `docs-contract` pointing at `http://<the address given at up>`;
  `docs-main` stays on `recap-mock`. The address at `up` and for the tests usually differs (see Run). Unset: those
  tests skip.
- Several sinks, webhook and Matrix (ADR-016): the multi-sink tests use `docs-contract` and `frontend-contract`. With
  `RECAP_CONTRACT_HOOKS_ADDR` (`host:port`, set at `up` and for the tests, split as in Run) the contract environment adds the profiles
  `webhook-contract` (`http://<addr>/webhook`) and `matrix-contract` (`http://<addr>`), and the tests serve a webhook receiver and a Matrix homeserver there. Unset: CT-SINK-18 and
  CT-META-08 skip. A service without the webhook and Matrix sink types does not start with these profiles.
- `RECAP_CONTRACT_SLOW` (any value) runs tests that wait for every attempt of a step, about two hours (CT-SINK-19);
  unset, they skip. Use `go test -timeout 4h`.
- Tests written before their implementation fail until it lands; their package README says which
  (`contracts/sink/README.md` "Not implemented yet").
- Tests that render pass `RECAP_FORMATS_DIR` through; unset, they set it to the repo's `formats/`.
- Results in the front end: the contract environment has the sink profile `frontend-contract` (`type: frontend`) and
  passes `RECAP_CONTRACT_RESULT_TTL` (default `7d`) to the service as `RECAP_RESULT_TTL`. The lifetime tests (record
  and summary after `cleanup`, expiry by the hourly cleanup loop, admin `erase`) run only when
  `RECAP_CONTRACT_RESULT_TTL` is also set for the tests; it must exceed `RECAP_RETENTION_AFTER_DELIVERY` (e.g. `90m`
  against `1h`), and the run takes up to about 2.5 h (`go test -timeout 4h`). Unset: those tests skip.
- JCS (RFC 8785) is pinned for integers, strings, booleans, null, arrays and objects only. Manifests carry no floats;
  a float is rejected, not formatted.
- Golden files under `testdata/` come from independent reference scripts (`tools/ref/`, `tools/fixturegen/`), not
  from the Go code under test.

Shared helpers live in `contracts/internal/harness` (`Bin`, `ExporterBin`, `BaseURL`).
