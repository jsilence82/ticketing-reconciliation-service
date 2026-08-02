// Package snapshot loads captured dashboard data into the domain model.
//
// It exists so the parity harness can feed internal/recon exactly what the
// Python reference is fed. It reads the dashboard's own cache files rather than
// a bespoke format, which removes a whole class of "the harness transformed it
// differently" doubt from a parity failure.
//
// The files it reads hold live production data with buyer PII and must live
// OUTSIDE this repository. Nothing here writes.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// canonicalPayload mirrors data/ssg_cache.json.
type canonicalPayload struct {
	IsCanonical bool              `json:"is_canonical"`
	SavedAt     string            `json:"saved_at"`
	Records     []canonicalRecord `json:"records"`
}

// canonicalRecord mirrors one row of the dashboard's canonical frame.
//
// Buyer fields (email, buyer_name, buyer_id) are present in the file but
// deliberately NOT decoded: reconciliation never reads them, and not loading
// them keeps PII out of this service's memory entirely.
type canonicalRecord struct {
	Show              string   `json:"show"`
	Category          string   `json:"category"`
	Quantity          float64  `json:"quantity"`
	Revenue           float64  `json:"revenue"`
	Date              string   `json:"date"`
	Status            string   `json:"status"`
	Occurrence        string   `json:"occurrence"`
	PerformanceDate   string   `json:"performance_date"`
	PayPalTxnID       string   `json:"paypal_txn_id"`
	OrderPaymentType  string   `json:"_order_payment_type"`
	OrderRefundAmount float64  `json:"_order_refund_amount"`
	OrderTotalPaid    *float64 `json:"_order_total_paid"`
}

// paypalPayload mirrors data/paypal_cache.json.
type paypalPayload struct {
	SavedAt      string         `json:"saved_at"`
	Transactions []paypalRecord `json:"transactions"`
}

type paypalRecord struct {
	TxnID             string  `json:"txn_id"`
	Date              string  `json:"date"`
	Gross             float64 `json:"gross"`
	Fee               float64 `json:"fee"`
	Net               float64 `json:"net"`
	Status            string  `json:"status"`
	PayPalReferenceID string  `json:"paypal_reference_id"`
}

// Snapshot is a loaded dataset ready to hand to internal/recon.
type Snapshot struct {
	Tickets []model.CanonicalTicket
	Txns    []model.PayPalTxn
}

// Shows returns the distinct show names, in first-seen order.
func (s Snapshot) Shows() []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range s.Tickets {
		if !seen[t.Show] {
			seen[t.Show] = true
			out = append(out, t.Show)
		}
	}
	return out
}

// Load reads ssg_cache.json and paypal_cache.json from dir.
func Load(dir string) (Snapshot, error) {
	tickets, err := loadCanonical(filepath.Join(dir, "ssg_cache.json"))
	if err != nil {
		return Snapshot{}, err
	}

	txns, err := loadPayPal(filepath.Join(dir, "paypal_cache.json"))
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{Tickets: tickets, Txns: txns}, nil
}

func loadCanonical(path string) ([]model.CanonicalTicket, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, read-only
	if err != nil {
		return nil, fmt.Errorf("read canonical cache: %w", err)
	}

	var payload canonicalPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse canonical cache: %w", err)
	}

	out := make([]model.CanonicalTicket, 0, len(payload.Records))
	for i, r := range payload.Records {
		saleDate, err := parseCacheTime(r.Date)
		if err != nil {
			return nil, fmt.Errorf("record %d: date: %w", i, err)
		}
		perfDate, err := parseCacheTime(r.PerformanceDate)
		if err != nil {
			return nil, fmt.Errorf("record %d: performance_date: %w", i, err)
		}

		var totalPaid float64
		if r.OrderTotalPaid != nil {
			totalPaid = *r.OrderTotalPaid
		}

		out = append(out, model.CanonicalTicket{
			Show:              r.Show,
			Category:          r.Category,
			Quantity:          r.Quantity,
			Revenue:           r.Revenue,
			Date:              saleDate,
			Status:            r.Status,
			Occurrence:        r.Occurrence,
			PerformanceDate:   perfDate,
			PayPalTxnID:       r.PayPalTxnID,
			OrderPaymentType:  r.OrderPaymentType,
			OrderRefundAmount: r.OrderRefundAmount,
			OrderTotalPaid:    totalPaid,
		})
	}
	return out, nil
}

func loadPayPal(path string) ([]model.PayPalTxn, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, read-only
	if err != nil {
		return nil, fmt.Errorf("read paypal cache: %w", err)
	}

	var payload paypalPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse paypal cache: %w", err)
	}

	out := make([]model.PayPalTxn, 0, len(payload.Transactions))
	for _, t := range payload.Transactions {
		out = append(out, model.PayPalTxn{
			TxnID: t.TxnID,
			// Already a bare YYYY-MM-DD in the cache, produced by the
			// reference's raw string slice. Kept verbatim.
			DateKey:           t.Date,
			Gross:             t.Gross,
			Fee:               t.Fee,
			Net:               t.Net,
			Status:            t.Status,
			PayPalReferenceID: t.PayPalReferenceID,
		})
	}
	return out, nil
}

// cacheTimeLayouts covers how pandas stringifies a UTC timestamp when the cache
// is written with json.dumps(default=str).
var cacheTimeLayouts = []string{
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.999999-07:00",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
}

// parseCacheTime parses a stringified pandas timestamp.
//
// A missing or NaT value yields the zero time, which is how this port
// represents pandas' NaT — and rows carrying it are dropped by the performance
// date groupby, per ledger entry 9.
func parseCacheTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "NaT", "None", "nan", "null":
		return time.Time{}, nil
	}

	for _, layout := range cacheTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}
