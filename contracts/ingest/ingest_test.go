// Package ingest holds the black-box contract tests of the ingest protocol
// (docs/reference/bbb-exporter.md §6, docs/reference/architecture.md §4.1 and §14).
//
// The golden HTTP exchanges in testdata/ingest/exchanges are replayed against
// a running service (RECAP_CONTRACT_BASE_URL) and served by a fake ingest
// server to the exporter (RECAP_EXPORTER_BIN). Each side skips without its
// variable.
package ingest

import (
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

func TestIngestExchanges(t *testing.T) {
	t.Run("CT-EXP-04", func(t *testing.T) {
		t.Run("service", func(t *testing.T) {
			base := harness.BaseURL(t)
			runService(t, base, loadCases(t))
		})
		t.Run("exporter", func(t *testing.T) {
			bin := harness.ExporterBin(t)
			runExporter(t, bin, loadCases(t))
		})
	})
}
