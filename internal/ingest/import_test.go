package ingest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/snapshot"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// --- normalizers (no database) ----------------------------------------------

func TestFromTicketTailor(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantType   model.ResourceType
		wantID     string
		wantStatus string
		// wantOccurred is the unix second the record should be versioned at.
		wantOccurred int64
	}{
		{
			name: "order versions on created_at",
			// Orders carry no updated_at — 0 of 1,532 in the real snapshot — so
			// occurred_at is frozen at creation and the payload hash is what
			// detects a later refund.
			payload:      `{"object":"order","id":"or_1","created_at":1710000000,"txn_id":"TX1"}`,
			wantType:     model.ResourceOrder,
			wantID:       "or_1",
			wantStatus:   "received",
			wantOccurred: 1710000000,
		},
		{
			name: "issued ticket prefers updated_at",
			// Tickets DO carry updated_at, and it moves when one is voided, so it
			// is the meaningful version.
			payload: `{"object":"issued_ticket","id":"it_1","created_at":1710000000,
			           "updated_at":1710009999,"status":"valid"}`,
			wantType:     model.ResourceIssuedTicket,
			wantID:       "it_1",
			wantStatus:   "received",
			wantOccurred: 1710009999,
		},
		{
			name:         "event",
			payload:      `{"object":"event","id":"ev_1","created_at":1710000000,"name":"Carmilla"}`,
			wantType:     model.ResourceEvent,
			wantID:       "ev_1",
			wantStatus:   "received",
			wantOccurred: 1710000000,
		},
		{
			name: "unknown object is stored but never reconciled",
			// waitlist_signup is in the ingest list but is not reconcilable.
			// Without the ignored status it would sit at received forever and
			// pollute dead-letter metrics.
			payload:      `{"object":"waitlist_signup","id":"wl_1","created_at":1710000000}`,
			wantType:     model.ResourceOther,
			wantID:       "wl_1",
			wantStatus:   "ignored",
			wantOccurred: 1710000000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ingest.FromTicketTailor([]byte(tc.payload), model.OriginBackfill)
			if err != nil {
				t.Fatalf("FromTicketTailor: %v", err)
			}
			if got.ResourceType != tc.wantType {
				t.Errorf("ResourceType = %q, want %q", got.ResourceType, tc.wantType)
			}
			if got.ResourceID != tc.wantID {
				t.Errorf("ResourceID = %q, want %q", got.ResourceID, tc.wantID)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.OccurredAt.Unix() != tc.wantOccurred {
				t.Errorf("OccurredAt = %d, want %d", got.OccurredAt.Unix(), tc.wantOccurred)
			}
			if got.Source != model.SourceTicketTailor {
				t.Errorf("Source = %q", got.Source)
			}
			if got.Origin != model.OriginBackfill {
				t.Errorf("Origin = %q", got.Origin)
			}
		})
	}
}

// An unidentifiable resource cannot be deduped, so it must be refused rather
// than stored under an empty key.
func TestFromTicketTailorRequiresAnID(t *testing.T) {
	if _, err := ingest.FromTicketTailor(
		[]byte(`{"object":"order","created_at":1}`), model.OriginBackfill); err == nil {
		t.Error("a resource with no id was accepted")
	}
}

func TestFromPayPalCache(t *testing.T) {
	raw := `{"txn_id":"TX1","date":"2026-03-24","gross":23.0,"fee":-1.08,
	         "net":21.92,"status":"S","paypal_reference_id":"REF1",
	         "subject":"buyer note","invoice_id":"INV1"}`

	got, err := ingest.FromPayPalCache([]byte(raw), model.OriginBackfill)
	if err != nil {
		t.Fatalf("FromPayPalCache: %v", err)
	}

	if got.ResourceID != "TX1" {
		t.Errorf("ResourceID = %q, want TX1", got.ResourceID)
	}
	if got.ResourceType != model.ResourcePayPalTransaction {
		t.Errorf("ResourceType = %q", got.ResourceType)
	}
	// The cache date is day-granular, which is why the version guard cannot rely
	// on timestamps alone.
	if got.OccurredAt.Format("2006-01-02") != "2026-03-24" {
		t.Errorf("OccurredAt = %v, want 2026-03-24", got.OccurredAt)
	}

	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"txn_id", "gross", "fee", "net", "paypal_reference_id"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("dropped %q, which the engine reads", k)
		}
	}
	// Both can carry buyer-supplied text and neither is read.
	for _, k := range []string{"subject", "invoice_id"} {
		if _, ok := payload[k]; ok {
			t.Errorf("kept %q, which should be dropped", k)
		}
	}
}

func TestFromPayPalCacheRequiresATxnID(t *testing.T) {
	if _, err := ingest.FromPayPalCache([]byte(`{"date":"2026-01-01"}`),
		model.OriginBackfill); err == nil {
		t.Error("a transaction with no txn_id was accepted")
	}
}

// --- import into Postgres ----------------------------------------------------

func scratchStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL is unset in CI; the postgres service block is not wired up")
		}
		t.Skip("DATABASE_URL unset; see docs/ENVIRONMENT.md")
	}

	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}

	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, cleanup, err := store.NewScratch(c, dsn, "trs_test_"+hex.EncodeToString(buf[:]))
	if err != nil {
		t.Fatalf("scratch store: %v", err)
	}
	t.Cleanup(cleanup)
	return s
}

func realSnapshot(t *testing.T) snapshot.RawJSON {
	t.Helper()

	dir := os.Getenv("SSG_PARITY_DATA")
	if dir == "" {
		t.Skip("SSG_PARITY_DATA unset; see docs/ENVIRONMENT.md")
	}
	raw, err := snapshot.LoadRawJSON(dir)
	if err != nil {
		t.Fatalf("LoadRawJSON: %v", err)
	}
	return raw
}

func TestImportSnapshot(t *testing.T) {
	s := scratchStore(t)
	raw := realSnapshot(t)

	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	report, err := ingest.ImportSnapshot(c, s, raw)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	t.Logf("%s", report)

	if report.Total() != raw.Total() {
		t.Errorf("stored %d of %d resources", report.Total(), raw.Total())
	}

	// Re-importing must change nothing. Backfill runs repeatedly, and a
	// resource backfilled today and delivered by webhook tomorrow must
	// collapse to one row rather than being processed twice.
	second, err := ingest.ImportSnapshot(c, s, raw)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Total() != 0 {
		t.Errorf("re-import wrote %d rows; a repeat import must be a no-op", second.Total())
	}
	if second.Outcomes[store.Rejected] != raw.Total() {
		t.Errorf("re-import rejected %d of %d; every row should be an unchanged duplicate",
			second.Outcomes[store.Rejected], raw.Total())
	}
}

// The end-to-end PII assertion: not "the sanitizer works" but "nothing personal
// is in the database after a real import".
func TestImportStoresNoPII(t *testing.T) {
	s := scratchStore(t)
	raw := realSnapshot(t)

	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := ingest.ImportSnapshot(c, s, raw); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	var emailRows, keyRows, total int

	if err := s.Pool().QueryRow(c, `
		SELECT count(*) FROM events
		 WHERE payload::text ~ '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+[.][A-Za-z]{2,}'`,
	).Scan(&emailRows); err != nil {
		t.Fatalf("email sweep: %v", err)
	}

	if err := s.Pool().QueryRow(c, `
		SELECT count(*) FROM events
		 WHERE payload ?| ARRAY['email','buyer_details','first_name','last_name',
		                        'full_name','phone','barcode','status_message']`,
	).Scan(&keyRows); err != nil {
		t.Fatalf("key sweep: %v", err)
	}

	if err := s.Pool().QueryRow(c, `SELECT count(*) FROM events`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}

	if emailRows != 0 {
		t.Errorf("%d stored payloads contain an email-shaped value", emailRows)
	}
	if keyRows != 0 {
		t.Errorf("%d stored payloads carry a forbidden key", keyRows)
	}
	t.Logf("%d rows stored, none carrying PII", total)
}

// Ingest order must survive the import, or the reconcile read cannot reproduce
// the provider's array order and parity stops being bit-exact.
func TestImportPreservesProviderOrder(t *testing.T) {
	s := scratchStore(t)
	raw := realSnapshot(t)

	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := ingest.ImportSnapshot(c, s, raw); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// Expected order, straight from the snapshot arrays.
	wantTickets := make([]string, 0, len(raw.Tickets))
	for _, r := range raw.Tickets {
		var e struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r, &e); err != nil {
			t.Fatal(err)
		}
		wantTickets = append(wantTickets, e.ID)
	}

	got, err := s.LoadResources(c)
	if err != nil {
		t.Fatalf("LoadResources: %v", err)
	}

	var gotTickets []string
	for _, r := range got {
		if r.ResourceType == model.ResourceIssuedTicket {
			gotTickets = append(gotTickets, r.ResourceID)
		}
	}

	if len(gotTickets) != len(wantTickets) {
		t.Fatalf("read back %d tickets, imported %d", len(gotTickets), len(wantTickets))
	}
	for i := range wantTickets {
		if gotTickets[i] != wantTickets[i] {
			t.Fatalf("position %d: got %q, want %q — provider order was not preserved "+
				"through storage, so parity cannot be bit-exact",
				i, gotTickets[i], wantTickets[i])
		}
	}
	t.Logf("all %d tickets read back in provider order", len(gotTickets))
}

// A filter that dropped the match key would pass every leak check perfectly.
func TestImportKeepsTheMatchKey(t *testing.T) {
	s := scratchStore(t)
	raw := realSnapshot(t)

	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := ingest.ImportSnapshot(c, s, raw); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	var withTxn int
	if err := s.Pool().QueryRow(c, `
		SELECT count(*) FROM events
		 WHERE resource_type = 'order'
		   AND payload->>'txn_id' IS NOT NULL
		   AND payload->>'txn_id' <> ''`).Scan(&withTxn); err != nil {
		t.Fatalf("count txn_id: %v", err)
	}

	if withTxn == 0 {
		t.Fatal("not one stored order kept a txn_id; the match key is being stripped " +
			"and reconciliation would match nothing")
	}
	t.Logf("%d stored orders carry a txn_id", withTxn)
}
