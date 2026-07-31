package importer_test

import (
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// ingestFromSearch is a thin wrapper so the sign-convention table reads
// cleanly. The normalizer lives in internal/ingest because both the importer
// and, later, the webhook handler must produce identical records.
func ingestFromSearch(t *testing.T, raw string) (model.EventRecord, error) {
	t.Helper()
	return ingest.FromPayPalSearch([]byte(raw), model.OriginBackfill)
}
