package importer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/importer"
)

// These run entirely offline. The windowing and paging logic is the part that
// actually breaks, and it needs no credentials to exercise — so there is no
// reason for it to be untested until someone points the job at a live account.

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// payPalServer serves a token plus whatever the handler returns, and records
// every transaction query it saw.
type payPalServer struct {
	*httptest.Server
	queries []map[string]string
}

func newPayPalServer(t *testing.T, handler func(q map[string]string, w http.ResponseWriter)) *payPalServer {
	t.Helper()
	s := &payPalServer{}

	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":32400}`))
			return
		}

		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		s.queries = append(s.queries, q)

		w.Header().Set("Content-Type", "application/json")
		handler(q, w)
	}))
	t.Cleanup(s.Close)
	return s
}

func newClient(baseURL string) *importer.PayPalClient {
	c := importer.NewPayPalClient(baseURL, "id", "secret")
	c.Limiter = importer.NewLimiter(0) // no pacing in tests
	return c
}

func drain(t *testing.T, p *importer.PayPalPager) int {
	t.Helper()
	total := 0
	for i := 0; ; i++ {
		recs, more, err := p.Next(ctx(t))
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		total += len(recs)
		if !more {
			return total
		}
		if i > 200 {
			t.Fatal("pager did not terminate")
		}
	}
}

func TestPayPalPagesOnTotalPages(t *testing.T) {
	s := newPayPalServer(t, func(q map[string]string, w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, `{"transaction_details":[{"n":%s}],"total_pages":3}`, q["page"])
	})

	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := drain(t, newClient(s.URL).Transactions(day, day.AddDate(0, 0, 5)))

	if got != 3 {
		t.Errorf("collected %d records across the pages, want 3", got)
	}

	var pages []string
	for _, q := range s.queries {
		pages = append(pages, q["page"])
	}
	if strings.Join(pages, ",") != "1,2,3" {
		t.Errorf("requested pages %v, want 1,2,3", pages)
	}
}

// A missing total_pages means exactly one page, matching the reference's
// .get("total_pages", 1). Treating it as unbounded would loop forever.
func TestPayPalAbsentTotalPagesMeansOnePage(t *testing.T) {
	s := newPayPalServer(t, func(_ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"transaction_details":[{"a":1}]}`))
	})

	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	drain(t, newClient(s.URL).Transactions(day, day.AddDate(0, 0, 5)))

	if len(s.queries) != 1 {
		t.Errorf("made %d requests, want exactly 1", len(s.queries))
	}
}

// The windowing regression test, and the one most likely to rot: assert the
// exact date strings of every window.
func TestPayPalWindowing(t *testing.T) {
	s := newPayPalServer(t, func(_ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"transaction_details":[],"total_pages":1}`))
	})

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 70)
	drain(t, newClient(s.URL).Transactions(start, end))

	var windows [][2]string
	for _, q := range s.queries {
		windows = append(windows, [2]string{q["start_date"], q["end_date"]})
	}

	want := [][2]string{
		// cursor, then cursor+31d; next cursor is windowEnd+1d.
		{"2026-01-01T00:00:00+00:00", "2026-02-01T23:59:59+00:00"},
		{"2026-02-02T00:00:00+00:00", "2026-03-05T23:59:59+00:00"},
		// Final window clamps to end.
		{"2026-03-06T00:00:00+00:00", "2026-03-12T23:59:59+00:00"},
	}

	if len(windows) != len(want) {
		t.Fatalf("got %d windows, want %d: %v", len(windows), len(want), windows)
	}
	for i := range want {
		if windows[i] != want[i] {
			t.Errorf("window %d = %v, want %v", i, windows[i], want[i])
		}
	}
}

// Every request must carry the parameters the dashboard sends. Dropping
// balance_affecting_records_only would silently widen the result set.
func TestPayPalRequestParameters(t *testing.T) {
	s := newPayPalServer(t, func(_ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"transaction_details":[],"total_pages":1}`))
	})

	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	drain(t, newClient(s.URL).Transactions(day, day.AddDate(0, 0, 3)))

	q := s.queries[0]
	want := map[string]string{
		"fields":                         "transaction_info",
		"page_size":                      "500",
		"balance_affecting_records_only": "Y",
	}
	for k, v := range want {
		if q[k] != v {
			t.Errorf("%s = %q, want %q", k, q[k], v)
		}
	}
}

// The raw 401 says nothing useful. This is the most common setup failure, so it
// gets a diagnostic naming the exact fix.
func TestPayPalTransactionSearchDisabled(t *testing.T) {
	s := newPayPalServer(t, func(_ map[string]string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"name":"AUTHENTICATION_FAILURE"}`))
	})

	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _, err := newClient(s.URL).Transactions(day, day.AddDate(0, 0, 3)).Next(ctx(t))

	if !errors.Is(err, importer.ErrTransactionSearchDisabled) {
		t.Fatalf("err = %v, want ErrTransactionSearchDisabled", err)
	}
	if !strings.Contains(err.Error(), "Transaction Search") {
		t.Error("the error should name the feature that needs enabling")
	}
}

// The ported window spans 31 days AND 23:59:59, which exceeds PayPal's
// documented cap. Rather than abandoning a whole backfill if PayPal enforces it,
// retry the window once at 30 days.
func TestPayPalRetriesNarrowerWindowOnDateRejection(t *testing.T) {
	var rejected int
	s := newPayPalServer(t, func(q map[string]string, w http.ResponseWriter) {
		if strings.HasPrefix(q["start_date"], "2026-01-01") &&
			strings.HasPrefix(q["end_date"], "2026-02-01") {
			rejected++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"name":"INVALID_REQUEST","message":"date range exceeds 31 days"}`))
			return
		}
		_, _ = w.Write([]byte(`{"transaction_details":[{"a":1}],"total_pages":1}`))
	})

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := drain(t, newClient(s.URL).Transactions(start, start.AddDate(0, 0, 40))); got == 0 {
		t.Error("the backfill collected nothing; the narrower retry did not happen")
	}
	if rejected == 0 {
		t.Error("the 31-day window was never attempted")
	}

	var sawNarrow bool
	for _, q := range s.queries {
		if strings.HasPrefix(q["end_date"], "2026-01-31") {
			sawNarrow = true
		}
	}
	if !sawNarrow {
		t.Errorf("no 30-day retry was issued; queries were %v", s.queries)
	}
}

// A token is fetched once and reused. Re-authenticating per page would triple
// the request count on a large backfill.
func TestPayPalTokenIsCached(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			tokenCalls++
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":32400}`))
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_, _ = w.Write([]byte(`{"transaction_details":[],"total_pages":3}`))
	}))
	defer srv.Close()

	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	drain(t, newClient(srv.URL).Transactions(day, day.AddDate(0, 0, 3)))

	if tokenCalls != 1 {
		t.Errorf("fetched the token %d times, want 1", tokenCalls)
	}
}

// --- the sign convention ------------------------------------------------------

// The highest-risk conversion in the project. Transaction Search returns
// fee_amount already signed, so net is gross PLUS fee.
func TestPayPalSearchSignConvention(t *testing.T) {
	tests := []struct {
		name                    string
		gross, fee              string
		wantGross, wantFee, net float64
	}{
		{
			name: "charge: fee arrives negative",
			// A EUR 23.00 sale costing EUR 1.08 in fees nets EUR 21.92.
			gross: "23.00", fee: "-1.08",
			wantGross: 23.00, wantFee: -1.08, net: 21.92,
		},
		{
			name: "refund: gross negative, fee returned positive",
			// Refunding that sale returns EUR 0.39 of fee, netting -22.61.
			gross: "-23.00", fee: "0.39",
			wantGross: -23.00, wantFee: 0.39, net: -22.61,
		},
		{
			name:  "missing fee object is zero, not an error",
			gross: "10.00", fee: "",
			wantGross: 10.00, wantFee: 0, net: 10.00,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feeJSON := "null"
			if tc.fee != "" {
				feeJSON = fmt.Sprintf(`{"value":%q,"currency_code":"EUR"}`, tc.fee)
			}
			raw := fmt.Sprintf(`{"transaction_info":{
				"transaction_id":"TX1",
				"transaction_initiation_date":"2026-03-24T10:11:12+0100",
				"transaction_status":"S",
				"transaction_amount":{"value":%q,"currency_code":"EUR"},
				"fee_amount":%s}}`, tc.gross, feeJSON)

			rec, err := ingestFromSearch(t, raw)
			if err != nil {
				t.Fatalf("FromPayPalSearch: %v", err)
			}

			var got struct {
				Gross float64 `json:"gross"`
				Fee   float64 `json:"fee"`
				Net   float64 `json:"net"`
				Date  string  `json:"date"`
			}
			if err := json.Unmarshal(rec.Payload, &got); err != nil {
				t.Fatal(err)
			}

			if got.Gross != tc.wantGross {
				t.Errorf("gross = %v, want %v", got.Gross, tc.wantGross)
			}
			if got.Fee != tc.wantFee {
				t.Errorf("fee = %v, want %v — Transaction Search signs it already, "+
					"so it must not be negated again", got.Fee, tc.wantFee)
			}
			if d := got.Net - tc.net; d > 0.0001 || d < -0.0001 {
				t.Errorf("net = %v, want %v (gross PLUS fee, never minus)", got.Net, tc.net)
			}
			// The date is a raw ten-character slice with no timezone conversion.
			if got.Date != "2026-03-24" {
				t.Errorf("date = %q, want 2026-03-24", got.Date)
			}
		})
	}
}
