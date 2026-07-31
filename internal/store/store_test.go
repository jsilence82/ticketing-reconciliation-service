package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// --- test harness ------------------------------------------------------------

// NewTestStore returns a migrated Store backed by a throwaway schema.
//
// Schema-per-test rather than a shared schema with truncation between cases:
// `go test -p N` then works against one database with no coordination, and each
// test starts genuinely empty.
//
// Skips when DATABASE_URL is unset so a clean checkout stays green — but hard
// fails when CI is set, because there an unset DSN means the postgres services
// block is broken, not that integration testing became optional. A skip-based
// strategy is only trustworthy if it cannot skip silently where it matters.
func NewTestStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL is unset in CI; the postgres service block is not wired up")
		}
		t.Skip("DATABASE_URL unset; see docs/ENVIRONMENT.md")
	}

	ctx := context.Background()

	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "trs_test_" + hex.EncodeToString(buf[:])

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}

	// Create the schema on a throwaway connection before the pool starts
	// handing out connections pinned to it.
	bootstrap, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("bootstrap pool: %v", err)
	}
	if _, err := bootstrap.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		bootstrap.Close()
		t.Fatalf("create schema: %v", err)
	}
	bootstrap.Close()

	// Every pooled connection must land in the test's schema, including ones
	// opened later as the pool grows — hence a connection-level setting rather
	// than a one-off SET.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}

	s := NewWithPool(pool)
	if _, err := s.Migrate(ctx); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()

		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		drop, err := pgxpool.New(c, dsn)
		if err != nil {
			return
		}
		defer drop.Close()
		_, _ = drop.Exec(c, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	return s
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func rec(resourceID string, opts ...func(*model.EventRecord)) model.EventRecord {
	r := model.EventRecord{
		Source:       model.SourcePayPal,
		ResourceType: model.ResourcePayPalTransaction,
		ResourceID:   resourceID,
		Origin:       model.OriginBackfill,
		Topic:        "PAYMENT.CAPTURE.COMPLETED",
		Status:       "received",
		Payload:      json.RawMessage(`{"id":"` + resourceID + `"}`),
		OccurredAt:   baseTime,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func withPayload(s string) func(*model.EventRecord) {
	return func(r *model.EventRecord) { r.Payload = json.RawMessage(s) }
}
func withOccurredAt(t time.Time) func(*model.EventRecord) {
	return func(r *model.EventRecord) { r.OccurredAt = t }
}
func withOrigin(o model.Origin) func(*model.EventRecord) {
	return func(r *model.EventRecord) { r.Origin = o }
}
func withType(rt model.ResourceType) func(*model.EventRecord) {
	return func(r *model.EventRecord) { r.ResourceType = rt }
}
func withSource(s model.Source) func(*model.EventRecord) {
	return func(r *model.EventRecord) { r.Source = s }
}
func withStatus(s string) func(*model.EventRecord) {
	return func(r *model.EventRecord) { r.Status = s }
}

// --- upsert / version ordering ----------------------------------------------

func TestUpsertInsertsThenUpdates(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if got, err := s.Upsert(c, rec("TX1")); err != nil || got != Inserted {
		t.Fatalf("first upsert = %v, %v; want inserted", got, err)
	}

	// Newer provider timestamp wins.
	newer := rec("TX1", withOccurredAt(baseTime.Add(time.Hour)), withPayload(`{"v":2}`))
	if got, err := s.Upsert(c, newer); err != nil || got != Updated {
		t.Fatalf("second upsert = %v, %v; want updated", got, err)
	}

	stored, err := s.ListEvents(c, ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored.Events) != 1 {
		t.Fatalf("got %d rows; the dedup key should collapse them to one", len(stored.Events))
	}
	if string(stored.Events[0].Payload) != `{"v": 2}` && string(stored.Events[0].Payload) != `{"v":2}` {
		t.Errorf("payload = %s; want the newer one", stored.Events[0].Payload)
	}
}

// The version guard, case for case against model.EventRecord.IsNewerThan.
// The Go predicate and the SQL predicate are two implementations of one rule;
// testing only the Go one proves nothing about what Postgres does.
func TestUpsertVersionOrdering(t *testing.T) {
	earlier := baseTime
	later := baseTime.Add(time.Hour)

	tests := []struct {
		name      string
		first     model.EventRecord
		second    model.EventRecord
		want      Outcome
		wantGo    bool // what IsNewerThan says, where it is comparable
		checkGo   bool
		rationale string
	}{
		{
			name:      "newer occurred_at wins",
			first:     rec("A", withOccurredAt(earlier)),
			second:    rec("A", withOccurredAt(later), withPayload(`{"v":2}`)),
			want:      Updated,
			wantGo:    true,
			checkGo:   true,
			rationale: "strictly newer by the provider's clock",
		},
		{
			name:      "older occurred_at is rejected",
			first:     rec("B", withOccurredAt(later)),
			second:    rec("B", withOccurredAt(earlier), withPayload(`{"v":2}`)),
			want:      Rejected,
			wantGo:    false,
			checkGo:   true,
			rationale: "a late delivery must not overwrite fresher state",
		},
		{
			name:      "webhook outranks backfill at equal time",
			first:     rec("C", withOrigin(model.OriginBackfill)),
			second:    rec("C", withOrigin(model.OriginWebhook), withPayload(`{"v":2}`)),
			want:      Updated,
			wantGo:    true,
			checkGo:   true,
			rationale: "a slow backfill page must not clobber a fresh webhook",
		},
		{
			name:      "backfill does not outrank webhook at equal time",
			first:     rec("D", withOrigin(model.OriginWebhook)),
			second:    rec("D", withOrigin(model.OriginBackfill), withPayload(`{"v":2}`)),
			want:      Updated, // via the content arm, not the rank arm
			wantGo:    false,
			checkGo:   true,
			rationale: "rank says no, but the content differs so the hash arm accepts it",
		},
		{
			name:      "identical rewrite is rejected",
			first:     rec("E"),
			second:    rec("E"),
			want:      Rejected,
			checkGo:   false,
			rationale: "a duplicate delivery is a dedup hit, not an update",
		},
		{
			name: "same version, different content is ACCEPTED",
			// The correctness case. Ticket Tailor orders carry no updated_at, so
			// a refund changes refund_amount while occurred_at stays put and both
			// writes are backfill. Without the payload-hash arm this update is
			// discarded and the refund is lost on every resync.
			first:  rec("F", withPayload(`{"refund_amount":0}`)),
			second: rec("F", withPayload(`{"refund_amount":1300}`)),
			want:   Updated,
			// IsNewerThan says false here — it only knows about time and origin.
			// That divergence is expected and is exactly why the SQL has a second
			// arm the Go predicate does not model.
			checkGo:   false,
			rationale: "a refund posted against an old order must land on resync",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewTestStore(t)
			c := ctx(t)

			if got, err := s.Upsert(c, tc.first); err != nil || got != Inserted {
				t.Fatalf("seed upsert = %v, %v; want inserted", got, err)
			}

			got, err := s.Upsert(c, tc.second)
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			if got != tc.want {
				t.Errorf("outcome = %q, want %q\n  because: %s", got, tc.want, tc.rationale)
			}

			if tc.checkGo {
				if gotGo := tc.second.IsNewerThan(tc.first); gotGo != tc.wantGo {
					t.Errorf("IsNewerThan = %v, want %v — the Go and SQL predicates "+
						"must agree wherever both apply", gotGo, tc.wantGo)
				}
			}
		})
	}
}

// The regression test for the bug the schema exists to prevent.
func TestUpsertLandsRefundOnResync(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	order := func(refund int) model.EventRecord {
		return rec("or_1",
			withSource(model.SourceTicketTailor),
			withType(model.ResourceOrder),
			withPayload(fmt.Sprintf(`{"id":"or_1","refund_amount":%d}`, refund)),
			withOccurredAt(baseTime), // created_at never moves: TT orders have no updated_at
		)
	}

	if _, err := s.Upsert(c, order(0)); err != nil {
		t.Fatalf("initial: %v", err)
	}

	// A later full resync fetches the same order, now refunded. occurred_at is
	// unchanged and both writes are backfill, so only the content arm can save
	// this.
	got, err := s.Upsert(c, order(1300))
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got != Updated {
		t.Fatalf("resync outcome = %q, want updated; the refund was silently dropped, "+
			"which is exactly the failure the payload-hash arm exists to prevent", got)
	}

	res, err := s.LoadResources(c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1", len(res))
	}
	var decoded struct {
		RefundAmount int `json:"refund_amount"`
	}
	if err := json.Unmarshal(res[0].Payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.RefundAmount != 1300 {
		t.Errorf("stored refund_amount = %d, want 1300", decoded.RefundAmount)
	}
}

// --- ingest ordering ---------------------------------------------------------

// The property that makes parity-db possible. If the read cannot reproduce the
// provider's array order, the pairwise and Neumaier summations produce different
// bits and matched_txn_ids comes back in the wrong order.
func TestLoadResourcesPreservesIngestOrder(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	// Every row shares an occurred_at, mirroring the real data where 1,026
	// tickets share a created_at and 387 PayPal rows share a date.
	const n = 200
	want := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("TX%03d", i)
		want[i] = id
		if _, err := s.Upsert(c, rec(id)); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	got, err := s.LoadResources(c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d resources, want %d", len(got), n)
	}
	for i := range want {
		if got[i].ResourceID != want[i] {
			t.Fatalf("position %d: got %q, want %q — ingest order was not preserved, "+
				"so parity cannot be bit-exact", i, got[i].ResourceID, want[i])
		}
	}
}

func TestLoadResourcesExcludesIgnoredAndDeadLettered(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.Upsert(c, rec("KEEP")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(c, rec("IGNORE", withStatus("ignored"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(c, rec("DEAD", withStatus("dead_lettered"))); err != nil {
		t.Fatal(err)
	}
	// 'received' must be visible: excluding it would make the first pass after a
	// backfill classify every transaction unmatched.
	if _, err := s.Upsert(c, rec("FRESH", withStatus("received"))); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadResources(c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range got {
		seen[r.ResourceID] = true
	}
	if !seen["KEEP"] || !seen["FRESH"] {
		t.Errorf("processed/received rows must be visible; got %v", seen)
	}
	if seen["IGNORE"] || seen["DEAD"] {
		t.Errorf("ignored/dead_lettered rows must be excluded; got %v", seen)
	}
}

// --- claim / concurrency -----------------------------------------------------

func TestClaim(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	for i := 0; i < 5; i++ {
		if _, err := s.Upsert(c, rec(fmt.Sprintf("TX%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := s.Claim(c, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claimed %d, want 3", len(claimed))
	}
	for _, r := range claimed {
		if r.Status != "processing" {
			t.Errorf("claimed row %s has status %q, want processing", r.ResourceID, r.Status)
		}
	}

	// Already-claimed rows must not come back.
	again, err := s.Claim(c, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 2 {
		t.Errorf("second claim returned %d, want the 2 remaining", len(again))
	}
}

// SKIP LOCKED is the whole reason a second worker is safe to add. Two
// concurrent claimers must partition the queue, never overlap.
func TestClaimSkipLockedPartitions(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	const total = 40
	for i := 0; i < total; i++ {
		if _, err := s.Upsert(c, rec(fmt.Sprintf("TX%02d", i))); err != nil {
			t.Fatal(err)
		}
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		allIDs   []string
		claimErr error
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Claim(c, 10)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				claimErr = err
				return
			}
			for _, r := range got {
				allIDs = append(allIDs, r.ResourceID)
			}
		}()
	}
	wg.Wait()

	if claimErr != nil {
		t.Fatalf("concurrent claim: %v", claimErr)
	}

	seen := map[string]bool{}
	for _, id := range allIDs {
		if seen[id] {
			t.Errorf("resource %s was claimed twice; SKIP LOCKED is not partitioning", id)
		}
		seen[id] = true
	}
	if len(allIDs) != total {
		t.Errorf("claimed %d of %d rows across 4 workers", len(allIDs), total)
	}
}

// A worker killed between claim and completion strands its rows. Without the
// reaper they are neither claimable nor finished, and nothing reports them.
func TestReleaseStale(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.Upsert(c, rec("TX1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(c, 1); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Not yet past the lease.
	if n, err := s.ReleaseStale(c, time.Hour); err != nil || n != 0 {
		t.Fatalf("premature release: n=%d err=%v", n, err)
	}

	// Past it.
	n, err := s.ReleaseStale(c, 0)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if n != 1 {
		t.Fatalf("released %d, want 1", n)
	}

	again, err := s.Claim(c, 1)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if len(again) != 1 {
		t.Error("released row was not claimable again")
	}
}

// --- transitions -------------------------------------------------------------

func TestMarkFailedBacksOffThenDeadLetters(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.Upsert(c, rec("TX1")); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(c, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v", err)
	}
	id := claimed[0].ID

	// First failure: backed off into the future, so it must not be re-claimable.
	if err := s.MarkFailed(c, id, errors.New("boom"), time.Hour, 3); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, err := s.GetEvent(c, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.RetryCount != 1 {
		t.Errorf("status=%q retry=%d, want failed/1", got.Status, got.RetryCount)
	}
	if got.LastError != "boom" {
		t.Errorf("last_error = %q, want boom", got.LastError)
	}
	if !got.NextAttemptAt.After(time.Now()) {
		t.Error("next_attempt_at is not in the future; backoff would be ignored and " +
			"the row re-claimed on the next tick")
	}
	if again, _ := s.Claim(c, 10); len(again) != 0 {
		t.Errorf("a backed-off row was re-claimed immediately (%d rows)", len(again))
	}

	// Exhausting the budget dead-letters.
	for i := 0; i < 2; i++ {
		if err := s.MarkFailed(c, id, errors.New("boom"), 0, 3); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
	}
	got, err = s.GetEvent(c, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "dead_lettered" {
		t.Errorf("status = %q after exhausting retries, want dead_lettered", got.Status)
	}
}

// --- verdicts ----------------------------------------------------------------

func TestWriteVerdicts(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	for _, id := range []string{"TX1", "TX2"} {
		if _, err := s.Upsert(c, rec(id)); err != nil {
			t.Fatal(err)
		}
	}

	run := "11111111-1111-1111-1111-111111111111"
	vs := []Verdict{
		{model.SourcePayPal, "TX1", model.ReconMatched, "or_1"},
		{model.SourcePayPal, "TX2", model.ReconUnmatched, ""},
	}

	n, err := s.WriteVerdicts(c, run, vs)
	if err != nil {
		t.Fatalf("write verdicts: %v", err)
	}
	if n != 2 {
		t.Fatalf("wrote %d verdicts, want 2", n)
	}

	page, err := s.ListEvents(c, ListFilter{ReconStatus: string(model.ReconMatched)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ResourceID != "TX1" {
		t.Errorf("recon_status filter returned %d rows; want just TX1", len(page.Events))
	}
	if page.Events[0].ReconCounterpartID != "or_1" {
		t.Errorf("counterpart = %q, want or_1", page.Events[0].ReconCounterpartID)
	}
}

// The anti-thrash guard. A pass that changes nothing must write nothing, or
// updated_at stops meaning "state changed" and the worker's dirty watermark
// oscillates forever.
func TestWriteVerdictsIsNoOpWhenUnchanged(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.Upsert(c, rec("TX1")); err != nil {
		t.Fatal(err)
	}

	run := "11111111-1111-1111-1111-111111111111"
	vs := []Verdict{{model.SourcePayPal, "TX1", model.ReconMatched, "or_1"}}

	if n, err := s.WriteVerdicts(c, run, vs); err != nil || n != 1 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}

	before, err := s.GetEvent(c, mustID(t, s, c, "TX1"))
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.WriteVerdicts(c, run, vs)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if n != 0 {
		t.Errorf("an unchanged verdict wrote %d rows; it must write 0 or the "+
			"reconcile pass re-triggers itself forever", n)
	}

	after, err := s.GetEvent(c, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("updated_at moved on a no-op verdict write")
	}
}

func mustID(t *testing.T, s *Store, c context.Context, resourceID string) string {
	t.Helper()
	page, err := s.ListEvents(c, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Events {
		if e.ResourceID == resourceID {
			return e.ID
		}
	}
	t.Fatalf("no row for %q", resourceID)
	return ""
}

// --- keyset pagination -------------------------------------------------------

// Aggregation is consumer-side, so a consumer summing a date range must see
// every row exactly once. That is the whole reason for keyset over OFFSET.
func TestListEventsKeysetSeesEveryRowExactlyOnce(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	const total = 250
	for i := 0; i < total; i++ {
		if _, err := s.Upsert(c, rec(fmt.Sprintf("TX%03d", i))); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0

	for {
		page, err := s.ListEvents(c, ListFilter{Limit: 37, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, e := range page.Events {
			seen[e.ResourceID]++
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next

		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct rows, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s was returned %d times; keyset paging must not repeat", id, n)
		}
	}
	t.Logf("%d rows across %d pages, each seen exactly once", len(seen), pages)
}

func TestListEventsFilters(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.Upsert(c, rec("PP1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(c, rec("TT1",
		withSource(model.SourceTicketTailor), withType(model.ResourceOrder))); err != nil {
		t.Fatal(err)
	}

	page, err := s.ListEvents(c, ListFilter{Source: string(model.SourceTicketTailor)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ResourceID != "TT1" {
		t.Errorf("source filter returned %d rows; want just TT1", len(page.Events))
	}

	// since filters on the PROVIDER's clock, not row-creation time.
	page, err = s.ListEvents(c, ListFilter{Since: baseTime.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 {
		t.Errorf("since filter returned %d rows for a future cutoff", len(page.Events))
	}
}

func TestListEventsRejectsBadCursor(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.ListEvents(c, ListFilter{Cursor: "not-base64!!"}); err == nil {
		t.Error("a malformed cursor was accepted")
	}
}

// --- health ------------------------------------------------------------------

func TestHealth(t *testing.T) {
	s := NewTestStore(t)
	c := ctx(t)

	if _, err := s.Upsert(c, rec("TX1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(c, rec("TX2", withStatus("ignored"))); err != nil {
		t.Fatal(err)
	}

	h, err := s.Health(c)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.ByStatus["received"] != 1 || h.ByStatus["ignored"] != 1 {
		t.Errorf("ByStatus = %v", h.ByStatus)
	}
	if h.ByReconStatus["not_applicable"] != 2 {
		t.Errorf("ByReconStatus = %v", h.ByReconStatus)
	}
}
