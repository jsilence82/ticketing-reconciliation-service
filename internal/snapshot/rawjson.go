package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RawJSON is the snapshot as undecoded provider objects.
//
// LoadRaw decodes into typed structs, which is what the parity harness wants.
// The importer wants the opposite: the provider's own bytes, so the sanitizer
// can filter them and the survivors can be stored verbatim. Decoding and
// re-encoding would reformat JSON numbers, and the parity harness depends on
// money surviving storage bit-for-bit.
//
// Order within each slice is the provider's array order, and it is preserved
// deliberately — it becomes the ingest ordinal that makes reconciliation
// reproducible.
type RawJSON struct {
	Orders      []json.RawMessage
	Tickets     []json.RawMessage
	Events      []json.RawMessage
	EventSeries []json.RawMessage
	PayPalTxns  []json.RawMessage
}

// Total reports how many resources the snapshot holds.
func (r RawJSON) Total() int {
	return len(r.Orders) + len(r.Tickets) + len(r.Events) +
		len(r.EventSeries) + len(r.PayPalTxns)
}

type rawTTFile struct {
	Orders      []json.RawMessage `json:"orders"`
	Tickets     []json.RawMessage `json:"tickets"`
	Events      []json.RawMessage `json:"events"`
	EventSeries []json.RawMessage `json:"event_series"`
}

type rawPayPalFile struct {
	Transactions []json.RawMessage `json:"transactions"`
}

// LoadRawJSON reads the snapshot's provider objects without decoding them.
func LoadRawJSON(dir string) (RawJSON, error) {
	var out RawJSON

	ttBlob, err := os.ReadFile(filepath.Join(dir, "tt_raw_cache.json")) //nolint:gosec // operator-supplied path, read-only
	if err != nil {
		return out, fmt.Errorf("read raw TT cache: %w", err)
	}
	var tt rawTTFile
	if err := json.Unmarshal(ttBlob, &tt); err != nil {
		return out, fmt.Errorf("parse raw TT cache: %w", err)
	}

	ppBlob, err := os.ReadFile(filepath.Join(dir, "paypal_cache.json")) //nolint:gosec // operator-supplied path, read-only
	if err != nil {
		return out, fmt.Errorf("read paypal cache: %w", err)
	}
	var pp rawPayPalFile
	if err := json.Unmarshal(ppBlob, &pp); err != nil {
		return out, fmt.Errorf("parse paypal cache: %w", err)
	}

	out.Orders = tt.Orders
	out.Tickets = tt.Tickets
	out.Events = tt.Events
	out.EventSeries = tt.EventSeries
	out.PayPalTxns = pp.Transactions

	return out, nil
}
