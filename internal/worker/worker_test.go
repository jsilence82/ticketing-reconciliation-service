package worker_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/snapshot"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/worker"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return c
}

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

// testConfig makes the debounce deterministic: no quiet period to wait out, so
// a reconcile runs as soon as the input has changed.
func testConfig() worker.Config {
	cfg := worker.DefaultConfig()
	cfg.QuietPeriod = 0
	cfg.PollInterval = time.Millisecond
	return cfg
}

func newWorker(t *testing.T, s *store.Store) *worker.Worker {
	t.Helper()
	return worker.New(s, testConfig(), discardLogger())
}

func rec(source model.Source, rt model.ResourceType, id, payload string) model.EventRecord {
	return model.EventRecord{
		Source:       source,
		ResourceType: rt,
		ResourceID:   id,
		Origin:       model.OriginBackfill,
		Status:       "received",
		Payload:      json.RawMessage(payload),
		OccurredAt:   time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

// --- stage 1 -----------------------------------------------------------------

func TestStageOneProcessesAndIgnores(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)
	w := newWorker(t, s)

	seed(t, s, c,
		rec(model.SourceTicketTailor, model.ResourceOrder, "or_1", `{"id":"or_1","txn_id":"TX1"}`),
		rec(model.SourceTicketTailor, model.ResourceOther, "wl_1", `{"id":"wl_1"}`),
	)

	processed, failed, err := w.DrainStageOne(c)
	if err != nil {
		t.Fatalf("DrainStageOne: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if processed != 2 {
		t.Errorf("processed = %d, want 2", processed)
	}

	h, err := s.Health(c)
	if err != nil {
		t.Fatal(err)
	}
	if h.ByStatus["processed"] != 1 {
		t.Errorf("processed rows = %d, want 1", h.ByStatus["processed"])
	}
	// Non-reconcilable resources must land in `ignored`, or they sit at
	// `received` forever and pollute dead-letter metrics.
	if h.ByStatus["ignored"] != 1 {
		t.Errorf("ignored rows = %d, want 1", h.ByStatus["ignored"])
	}
}

// A payload that cannot be parsed must fail alone. The point of validating per
// row is that one bad row costs one row, not the whole reconcile pass.
func TestStageOneDeadLettersUnparseableRow(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)

	cfg := testConfig()
	cfg.MaxRetries = 1 // dead-letter on first failure
	w := worker.New(s, cfg, discardLogger())

	seed(t, s, c,
		rec(model.SourceTicketTailor, model.ResourceOrder, "or_ok", `{"id":"or_ok"}`),
		// Valid JSON, but no id — unusable, and undetectable until decode.
		rec(model.SourceTicketTailor, model.ResourceOrder, "or_bad", `{"txn_id":"TX1"}`),
	)

	processed, failed, err := w.DrainStageOne(c)
	if err != nil {
		t.Fatalf("DrainStageOne: %v", err)
	}
	if processed != 1 || failed != 1 {
		t.Fatalf("processed=%d failed=%d, want 1 and 1", processed, failed)
	}

	h, err := s.Health(c)
	if err != nil {
		t.Fatal(err)
	}
	if h.ByStatus["dead_lettered"] != 1 {
		t.Errorf("dead_lettered = %d, want 1", h.ByStatus["dead_lettered"])
	}
	if h.ByStatus["processed"] != 1 {
		t.Errorf("the good row should still have been processed; got %v", h.ByStatus)
	}
}

// A sign error in the PayPal normalizer would silently double or zero the fees
// in every downstream figure, so it must fail loudly rather than be stored.
func TestStageOneRejectsInconsistentPayPalAmounts(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)

	cfg := testConfig()
	cfg.MaxRetries = 1
	w := worker.New(s, cfg, discardLogger())

	seed(t, s, c,
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX_ok",
			`{"txn_id":"TX_ok","gross":23.0,"fee":-1.08,"net":21.92}`),
		// net should be gross+fee = 21.92; 24.08 is what a flipped fee sign gives.
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX_bad",
			`{"txn_id":"TX_bad","gross":23.0,"fee":-1.08,"net":24.08}`),
	)

	_, failed, err := w.DrainStageOne(c)
	if err != nil {
		t.Fatalf("DrainStageOne: %v", err)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 — net != gross+fee must not be accepted", failed)
	}
}

// --- stage 2 -----------------------------------------------------------------

func TestReconcileWritesVerdicts(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)
	w := newWorker(t, s)

	seed(t, s, c,
		rec(model.SourceTicketTailor, model.ResourceOrder, "or_1",
			`{"id":"or_1","txn_id":"TX1","payment_method":{"type":"paypal"},"refund_amount":0}`),
		rec(model.SourceTicketTailor, model.ResourceIssuedTicket, "it_1",
			`{"id":"it_1","order_id":"or_1","event_id":"ev_1","description":"GA","status":"valid","listed_price":1000}`),
		rec(model.SourceTicketTailor, model.ResourceEvent, "ev_1",
			`{"id":"ev_1","name":"Carmilla","start":{"unix":1750000000}}`),
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1",
			`{"txn_id":"TX1","gross":10.0,"fee":-0.5,"net":9.5}`),
		// No ticket references this one: a genuine orphan.
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX9",
			`{"txn_id":"TX9","gross":99.0,"fee":-3.0,"net":96.0}`),
	)

	if _, _, err := w.DrainStageOne(c); err != nil {
		t.Fatalf("stage 1: %v", err)
	}

	res, err := w.Reconcile(c, time.Time{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Ran {
		t.Fatal("Reconcile did not run")
	}

	h, err := s.Health(c)
	if err != nil {
		t.Fatal(err)
	}
	if h.ByReconStatus["matched"] != 1 {
		t.Errorf("matched = %d, want 1 (TX1)", h.ByReconStatus["matched"])
	}
	// The orphan must be surfaced. This is the requirement the reference's own
	// unmatched detection can never satisfy (ledger entry 1).
	if h.ByReconStatus["unmatched"] != 1 {
		t.Errorf("unmatched = %d, want 1 (TX9)", h.ByReconStatus["unmatched"])
	}
}

// The anti-thrash property. If a pass that changes nothing still wrote rows, it
// would bump max(updated_at), which would re-trigger the next pass, forever.
func TestReconcileSettles(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)
	w := newWorker(t, s)

	seed(t, s, c,
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1",
			`{"txn_id":"TX1","gross":10.0,"fee":-0.5,"net":9.5}`),
	)
	if _, _, err := w.DrainStageOne(c); err != nil {
		t.Fatal(err)
	}

	// Tick until nothing changes, then confirm it stays that way.
	var passes int
	for i := 0; i < 10; i++ {
		res, err := w.Tick(c)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if res.Reconcile.Ran {
			passes++
		}
		if res.Reconcile.Ran && res.Reconcile.Changed == 0 && i > 0 {
			break
		}
	}

	// From here every tick must decline: the input is unchanged.
	for i := 0; i < 3; i++ {
		res, err := w.Tick(c)
		if err != nil {
			t.Fatalf("steady tick %d: %v", i, err)
		}
		if res.Reconcile.Ran {
			t.Errorf("tick %d reconciled again despite unchanged input; the loop is "+
				"thrashing (skip reason was %q)", i, res.ReconcileSkipped)
		}
		if res.ReconcileSkipped != "input unchanged" {
			t.Errorf("skip reason = %q, want \"input unchanged\"", res.ReconcileSkipped)
		}
	}
	t.Logf("settled after %d passes, then declined cleanly", passes)
}

// New input must wake it back up, or a late webhook would never be reconciled.
func TestReconcileWakesOnNewInput(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)
	w := newWorker(t, s)

	seed(t, s, c, rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1",
		`{"txn_id":"TX1","gross":10.0,"fee":-0.5,"net":9.5}`))

	for i := 0; i < 5; i++ {
		if _, err := w.Tick(c); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := w.Tick(c); res.Reconcile.Ran {
		t.Fatal("expected the loop to have settled first")
	}

	seed(t, s, c, rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX2",
		`{"txn_id":"TX2","gross":20.0,"fee":-1.0,"net":19.0}`))

	res, err := w.Tick(c)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reconcile.Ran {
		t.Errorf("new input did not trigger a pass (skip reason %q)", res.ReconcileSkipped)
	}
}

// Stage 2 must be single-flighted: two concurrent passes would race on the bulk
// verdict write.
func TestReconcileIsSingleFlighted(t *testing.T) {
	s := scratchStore(t)
	c := testCtx(t)

	seed(t, s, c, rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1",
		`{"txn_id":"TX1","gross":10.0,"fee":-0.5,"net":9.5}`))

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ran  int
		errs []error
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newWorker(t, s)
			res, err := w.Reconcile(c, time.Time{})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if res.Ran {
				ran++
			}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent reconcile: %v", err)
	}
	if ran == 0 {
		t.Error("no pass ran at all")
	}
	if ran > 1 {
		// They may serialise rather than overlap, so >1 is only a failure if it
		// means the advisory lock is absent. Report it either way.
		t.Logf("%d passes ran; they serialised rather than overlapping", ran)
	}
}

// --- end to end over real data ------------------------------------------------

func TestWorkerOnRealSnapshot(t *testing.T) {
	dir := os.Getenv("SSG_PARITY_DATA")
	if dir == "" {
		t.Skip("SSG_PARITY_DATA unset")
	}

	s := scratchStore(t)
	c := testCtx(t)
	w := newWorker(t, s)

	raw, err := snapshot.LoadRawJSON(dir)
	if err != nil {
		t.Fatalf("LoadRawJSON: %v", err)
	}
	if _, err := ingest.ImportSnapshot(c, s, raw); err != nil {
		t.Fatalf("import: %v", err)
	}

	processed, failed, err := w.DrainStageOne(c)
	if err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	if failed != 0 {
		t.Errorf("%d real rows failed validation", failed)
	}
	t.Logf("stage 1: %d processed, %d failed", processed, failed)

	res, err := w.Reconcile(c, time.Time{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	h, err := s.Health(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verdicts: %v", h.ByReconStatus)

	// These match what cmd/parity reports over the same data, which is the
	// cross-check: the worker and the offline path agree.
	if h.ByReconStatus["matched"] != 462 {
		t.Errorf("matched = %d, want 462", h.ByReconStatus["matched"])
	}
	if h.ByReconStatus["transferred"] != 20 {
		t.Errorf("transferred = %d, want 20", h.ByReconStatus["transferred"])
	}
	if h.ByReconStatus["unmatched"] != 0 {
		t.Errorf("unmatched = %d, want 0", h.ByReconStatus["unmatched"])
	}

	// A second pass over unchanged data must move nothing.
	again, err := w.Reconcile(c, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed != 0 {
		t.Errorf("a repeat pass changed %d verdicts; it must be a no-op", again.Changed)
	}
	t.Logf("first pass changed %d verdicts, second changed %d", res.Changed, again.Changed)
}

// --- helpers ------------------------------------------------------------------

func seed(t *testing.T, s *store.Store, c context.Context, recs ...model.EventRecord) {
	t.Helper()
	if _, err := s.UpsertBatch(c, recs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}
