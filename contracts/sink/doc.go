// Package sink holds the black-box contract tests for the sink interface and
// the Docs sink (docs/reference/sinks.md §4) and for the owner identity
// (docs/reference/identity-mapping.md §9). The test IDs are the subtest names.
//
// Most tests drive `recap tool sink` from RECAP_BIN against a mock Docs
// (create-for-owner only) that the test starts on 127.0.0.1 over HTTPS. The
// tool's I/O is defined in README.md in this directory.
//
// The admin-action test and the owner resume and cleanup tests drive the
// running service at RECAP_CONTRACT_BASE_URL. The test serves mock Docs over
// HTTP on RECAP_CONTRACT_DOCS_ADDR; the service under test has a sink profile
// docs-contract pointing there, and the packages of these tests name it in
// meta recap-sink. Without either variable those tests skip and name it.
// RECAP_CONTRACT_LOG_FILE is scanned when set; RECAP_RETENTION_AFTER_DELIVERY
// is above zero as contracts/README.md requires.
//
// CT-SINK-12 (the frontend sink, sinks.md §4a) runs the tool with
// testdata/sink/sinks-frontend.yaml and an fs blob store in a temp dir, and
// the service with the contract environment's profile frontend-contract.
// TestResultDocumentURL is the admin-release half of CT-REG-08
// (metadata-interface.md §2.7); it lives here for the fault-injecting mock.
package sink
