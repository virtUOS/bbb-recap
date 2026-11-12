// Package registration holds the black-box contract tests for the metadata
// interface and the registration API: docs/reference/metadata-interface.md §7.
//
// The tests drive a running service at RECAP_CONTRACT_BASE_URL and skip when
// it is unset. They use only HTTP: the registration API (§2), the ingest API
// (docs/reference/bbb-exporter.md §6) to push packages built from the F-01, F-04
// and F-06 fixtures, and the admin API (docs/reference/architecture.md §4) to read
// job states. Every test uses fresh record and meeting IDs, so the tests can
// run again against the same service.
//
// The service under test is configured as docs/reference/fixtures.md §3 and §5
// describe:
//   - RECAP_CLIENT_SECRETS_DIR holds testdata/secrets/clients.
//   - RECAP_FRONTEND_SERVERS is pilos-fixture:bbb-fixture-a only, so the
//     front end may not claim recordings of bbb-fixture-b.
//   - Meta prefix recap, meeting type meeting-summary, sink profile docs-main.
//   - ASR, LLM and Docs are the recap-mock servers, so a job runs through to
//     deliver. Mock Docs holds the fixture users of fixtures.md §5.
//   - RECAP_RETENTION_AFTER_DELIVERY is above zero (for example 1h), as
//     contracts/README.md requires: the rows "deliver claimed or delivered" read
//     the registration after delivery, and cleanup deletes it.
//
// For an HTTPS base URL with a test certificate, set SSL_CERT_FILE.
//
// Optional:
//   - RECAP_CONTRACT_LOG_FILE: the log of the service under test, JSON
//     lines, all levels including DEBUG (contracts/README.md). Without it only
//     the log scan of the logging test skips.
//
// A second front end (testdata/secrets/clients/frontend.other-fixture) is
// used for the foreign-registration checks.
//
// Results in the front end (§2.7, ADR-014; results_test.go): the service also
// has the sink profile frontend-contract (type frontend, sinks.md §4a) and
// docs-main's document_url_template is https://docs.recap.test/docs/{id}/,
// as tools/contract-env/config/sinks.yaml sets them. TestResponseSchema
// checks the schema against testdata/registration/ and needs no service.
// TestResults (CT-REG-08, CT-REG-09) runs in a few minutes. The lifetime
// half, TestResultLifetime (the result record and the summary after cleanup,
// their deletion after RECAP_RESULT_TTL by the hourly cleanup loop, admin
// erase), runs only with RECAP_CONTRACT_RESULT_TTL set to the service's
// RECAP_RESULT_TTL; it must exceed RECAP_RETENTION_AFTER_DELIVERY (e.g. 90m
// with the contract environment's 1h). Those subtests run in parallel and
// take up to about 2.5 hours, so go test needs -timeout 4h.
// RECAP_RESULT_TTL=0 needs another service configuration and is left to
// unit tests (the subtest says so when it skips).
package registration
