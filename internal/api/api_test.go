package api_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/api"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

const testKey = "test-key-long-enough-to-be-accepted"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

func newServer(t *testing.T, s *store.Store) http.Handler {
	t.Helper()
	consumers, err := api.ParseConsumers("dashboard:" + testKey)
	if err != nil {
		t.Fatalf("ParseConsumers: %v", err)
	}
	return api.New(s, consumers, discardLogger()).Routes()
}

func seed(t *testing.T, s *store.Store, recs ...model.EventRecord) {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.UpsertBatch(c, recs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func rec(source model.Source, rt model.ResourceType, id, payload string) model.EventRecord {
	return model.EventRecord{
		Source: source, ResourceType: rt, ResourceID: id,
		Origin: model.OriginBackfill, Status: "processed",
		Payload:    json.RawMessage(payload),
		OccurredAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func do(t *testing.T, h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// --- auth --------------------------------------------------------------------

// Reconciliation data is buyer-payment mapping, so every data endpoint is
// closed by default.
func TestAuthRequired(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	for _, path := range []string{"/events", "/events/" + someUUID} {
		t.Run(path, func(t *testing.T) {
			if got := do(t, h, "GET", path, "").Code; got != http.StatusUnauthorized {
				t.Errorf("without a key: status %d, want 401", got)
			}
			if got := do(t, h, "GET", path, "wrong-key-but-long-enough-here").Code; got != http.StatusUnauthorized {
				t.Errorf("with a wrong key: status %d, want 401", got)
			}
		})
	}
}

// Health is deliberately open: the reverse proxy and uptime checks need it, and
// it exposes counts rather than any buyer or payment data.
func TestHealthIsUnauthenticated(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	w := do(t, h, "GET", "/healthz", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
	for _, k := range []string{"events", "recon"} {
		if _, ok := body[k]; !ok {
			t.Errorf("health is missing %q, which is what makes drift visible", k)
		}
	}
}

func TestAPIKeyViaHeader(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	r := httptest.NewRequest("GET", "/events", nil)
	r.Header.Set("X-API-Key", testKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("X-API-Key was not accepted: status %d", w.Code)
	}
}

func TestParseConsumers(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		count   int
		wantErr bool
	}{
		{"empty is empty, not permissive", "", 0, false},
		{"one", "a:" + testKey, 1, false},
		{"two", "a:" + testKey + ",b:" + testKey + "2", 2, false},
		{"malformed entry", "nokey", 0, true},
		{"short key is rejected", "a:short", 0, true},
		{"duplicate key", "a:" + testKey + ",b:" + testKey, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := api.ParseConsumers(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConsumers: %v", err)
			}
			if len(got) != tc.count {
				t.Errorf("got %d consumers, want %d", len(got), tc.count)
			}
		})
	}
}

// An empty consumer set must deny, not allow. A service that becomes public
// when its configuration is missing is the failure worth engineering against.
func TestEmptyConsumersDenyEverything(t *testing.T) {
	s := scratchStore(t)
	h := api.New(s, api.Consumers{}, discardLogger()).Routes()

	if got := do(t, h, "GET", "/events", testKey).Code; got != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 — no configured keys must mean no access", got)
	}
}

// --- read-only enforcement ----------------------------------------------------

// Guardrail 3 is enforced by proving the role cannot write, not by trusting
// handlers. Against the ordinary test role — which CAN write — the check must
// fail, or it would rubber-stamp a misconfigured deployment.
func TestVerifyReadOnlyRejectsWritableRole(t *testing.T) {
	s := scratchStore(t)
	srv := api.New(s, api.Consumers{}, discardLogger())

	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := srv.VerifyReadOnly(c)
	if err == nil {
		t.Fatal("VerifyReadOnly accepted a role that can INSERT; it would " +
			"rubber-stamp a deployment where the API can mutate state")
	}
	if !strings.Contains(err.Error(), "SELECT-only") {
		t.Errorf("err = %v; it should name the fix", err)
	}
}

// The probe must never leave anything behind, even against a writable role.
func TestVerifyReadOnlyLeavesNoTrace(t *testing.T) {
	s := scratchStore(t)
	srv := api.New(s, api.Consumers{}, discardLogger())

	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = srv.VerifyReadOnly(c)

	var n int
	if err := s.Pool().QueryRow(c,
		`SELECT count(*) FROM events WHERE resource_id = '__readonly_probe__'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the read-only probe left %d row(s) behind; it must always roll back", n)
	}
}

// --- listing ------------------------------------------------------------------

func TestListEvents(t *testing.T) {
	s := scratchStore(t)
	seed(t, s,
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1",
			`{"txn_id":"TX1","gross":23.0,"fee":-1.08,"net":21.92,"currency":"EUR"}`),
		rec(model.SourceTicketTailor, model.ResourceOrder, "or_1", `{"id":"or_1"}`),
	)
	h := newServer(t, s)

	w := do(t, h, "GET", "/events", testKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	var body struct {
		Events []struct {
			ResourceID string          `json:"resource_id"`
			Payload    json.RawMessage `json:"payload"`
		} `json:"events"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(body.Events))
	}
	if body.NextCursor != "" {
		t.Error("a complete page should carry no next_cursor")
	}
}

func TestListEventsFilters(t *testing.T) {
	s := scratchStore(t)
	seed(t, s,
		rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1", `{"txn_id":"TX1"}`),
		rec(model.SourceTicketTailor, model.ResourceOrder, "or_1", `{"id":"or_1"}`),
	)
	h := newServer(t, s)

	w := do(t, h, "GET", "/events?source=tickettailor", testKey)
	var body struct {
		Events []struct {
			ResourceID string `json:"resource_id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].ResourceID != "or_1" {
		t.Errorf("source filter returned %+v, want just or_1", body.Events)
	}
}

// Aggregation is consumer-side, so a consumer summing a range must see every
// row exactly once. Paging is the mechanism that guarantees it.
func TestListEventsPagination(t *testing.T) {
	s := scratchStore(t)

	var recs []model.EventRecord
	for i := 0; i < 25; i++ {
		recs = append(recs, rec(model.SourcePayPal, model.ResourcePayPalTransaction,
			"TX"+string(rune('A'+i)), `{"txn_id":"x"}`))
	}
	seed(t, s, recs...)
	h := newServer(t, s)

	seen := map[string]int{}
	cursor := ""
	for i := 0; i < 20; i++ {
		path := "/events?limit=7"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		w := do(t, h, "GET", path, testKey)
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: status %d: %s", i, w.Code, w.Body)
		}

		var body struct {
			Events []struct {
				ResourceID string `json:"resource_id"`
			} `json:"events"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, e := range body.Events {
			seen[e.ResourceID]++
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
	}

	if len(seen) != 25 {
		t.Errorf("saw %d distinct rows, want 25", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s returned %d times; paging must not repeat", id, n)
		}
	}
}

func TestListEventsRejectsBadInput(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	tests := []struct{ name, path string }{
		{"bad cursor", "/events?cursor=not-base64!!"},
		{"bad since", "/events?since=last-tuesday"},
		{"bad limit", "/events?limit=-3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(t, h, "GET", tc.path, testKey).Code; got != http.StatusBadRequest {
				t.Errorf("status %d, want 400", got)
			}
		})
	}
}

// --- money representation ------------------------------------------------------

// CLAUDE.md: money crosses the boundary as decimal strings plus minor-unit
// integers plus a currency code, never a JSON float. Consumers do the
// arithmetic here, so this matters more than it would for a pre-aggregated
// response.
func TestMoneyIsNotServedAsAFloat(t *testing.T) {
	s := scratchStore(t)
	seed(t, s, rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1",
		`{"txn_id":"TX1","gross":23.0,"fee":-1.08,"net":21.92,"currency":"EUR"}`))
	h := newServer(t, s)

	w := do(t, h, "GET", "/events", testKey)

	var body struct {
		Events []struct {
			Amounts *struct {
				Currency string `json:"currency"`
				Gross    struct {
					Decimal string `json:"decimal"`
					Minor   int64  `json:"minor"`
				} `json:"gross"`
				Fee struct {
					Decimal string `json:"decimal"`
					Minor   int64  `json:"minor"`
				} `json:"fee"`
				Net struct {
					Decimal string `json:"decimal"`
					Minor   int64  `json:"minor"`
				} `json:"net"`
			} `json:"amounts"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].Amounts == nil {
		t.Fatal("no amounts block was served for a PayPal transaction")
	}

	a := body.Events[0].Amounts
	if a.Currency != "EUR" {
		t.Errorf("currency = %q, want EUR", a.Currency)
	}
	if a.Gross.Decimal != "23.00" || a.Gross.Minor != 2300 {
		t.Errorf("gross = %+v, want 23.00 / 2300", a.Gross)
	}
	// The fee sign must survive the boundary: negative on a charge.
	if a.Fee.Decimal != "-1.08" || a.Fee.Minor != -108 {
		t.Errorf("fee = %+v, want -1.08 / -108", a.Fee)
	}
	if a.Net.Minor != a.Gross.Minor+a.Fee.Minor {
		t.Errorf("net minor %d != gross %d + fee %d",
			a.Net.Minor, a.Gross.Minor, a.Fee.Minor)
	}

	// And the decimal really is a JSON string, not a bare number.
	if !strings.Contains(w.Body.String(), `"decimal":"23.00"`) {
		t.Error("decimal is not being serialised as a string")
	}
}

// A non-PayPal row has no amounts block. Inventing zeros would be worse than
// omitting it.
func TestNoAmountsForNonPayPalRows(t *testing.T) {
	s := scratchStore(t)
	seed(t, s, rec(model.SourceTicketTailor, model.ResourceOrder, "or_1", `{"id":"or_1"}`))
	h := newServer(t, s)

	if strings.Contains(do(t, h, "GET", "/events", testKey).Body.String(), `"amounts"`) {
		t.Error("an amounts block was served for a Ticket Tailor order")
	}
}

// --- single event ---------------------------------------------------------------

const someUUID = "11111111-1111-1111-1111-111111111111"

func TestGetEvent(t *testing.T) {
	s := scratchStore(t)
	seed(t, s, rec(model.SourcePayPal, model.ResourcePayPalTransaction, "TX1", `{"txn_id":"TX1"}`))
	h := newServer(t, s)

	list := do(t, h, "GET", "/events", testKey)
	var lb struct {
		Events []struct {
			ID string `json:"id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &lb); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/events/"+lb.Events[0].ID, testKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	var got struct {
		ResourceID string `json:"resource_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceID != "TX1" {
		t.Errorf("resource_id = %q, want TX1", got.ResourceID)
	}
}

func TestGetEventNotFoundAndBadID(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	if got := do(t, h, "GET", "/events/"+someUUID, testKey).Code; got != http.StatusNotFound {
		t.Errorf("missing event: status %d, want 404", got)
	}
	if got := do(t, h, "GET", "/events/not-a-uuid", testKey).Code; got != http.StatusBadRequest {
		t.Errorf("malformed id: status %d, want 400", got)
	}
}

// --- guardrail 3: no mutation surface -------------------------------------------

// The router must not answer a write verb on any route. This is the cheap check
// that complements the database-level one.
func TestNoMutationRoutes(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, path := range []string{"/events", "/events/" + someUUID, "/healthz"} {
			t.Run(method+" "+path, func(t *testing.T) {
				code := do(t, h, method, path, testKey).Code
				if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
					t.Errorf("%s %s returned %d; no write verb may be routed",
						method, path, code)
				}
			})
		}
	}
}

// No aggregation endpoints. CLAUDE.md is explicit that Totals and Statistics are
// consumer-side, and internal/recon/oracle must never be served.
func TestNoReconciliationEndpoints(t *testing.T) {
	s := scratchStore(t)
	h := newServer(t, s)

	for _, path := range []string{
		"/reconciliation/summary", "/reconciliation/categories", "/reconciliation",
	} {
		if got := do(t, h, "GET", path, testKey).Code; got != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404 — aggregation is a consumer concern",
				path, got)
		}
	}
}
