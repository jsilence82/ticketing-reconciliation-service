package importer_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/importer"
)

type ttServer struct {
	*httptest.Server
	queries []map[string]string
	auth    []string
}

func newTTServer(t *testing.T, handler func(q map[string]string, w http.ResponseWriter)) *ttServer {
	t.Helper()
	s := &ttServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		s.queries = append(s.queries, q)
		s.auth = append(s.auth, r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		handler(q, w)
	}))
	t.Cleanup(s.Close)
	return s
}

func newTTClient(baseURL string) *importer.TicketTailorClient {
	c := importer.NewTicketTailorClient(baseURL, "sk_test_key")
	c.Limiter = importer.NewLimiter(0)
	return c
}

// fullPage renders n records with sequential ids.
func fullPage(prefix string, from, n int) string {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"%s%d","object":"order"}`, prefix, from+i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func drainTT(t *testing.T, p *importer.TicketTailorPager) ([]json.RawMessage, error) {
	t.Helper()
	var all []json.RawMessage
	for i := 0; ; i++ {
		recs, more, err := p.Next(ctx(t))
		if err != nil {
			return all, err
		}
		all = append(all, recs...)
		if !more {
			return all, nil
		}
		if i > 500 {
			t.Fatal("pager did not terminate")
		}
	}
}

// The cursor must advance to the LAST record's id, or the second page repeats
// the first forever.
func TestTicketTailorCursorAdvances(t *testing.T) {
	s := newTTServer(t, func(q map[string]string, w http.ResponseWriter) {
		switch q["starting_after"] {
		case "":
			_, _ = w.Write([]byte(fullPage("or_", 1, 100)))
		case "or_100":
			_, _ = w.Write([]byte(fullPage("or_", 101, 37))) // short page ends it
		default:
			t.Errorf("unexpected cursor %q", q["starting_after"])
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	})

	got, err := drainTT(t, newTTClient(s.URL).Page("orders", nil))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 137 {
		t.Errorf("collected %d records, want 137", len(got))
	}
	if len(s.queries) != 2 {
		t.Fatalf("made %d requests, want 2", len(s.queries))
	}
	if s.queries[1]["starting_after"] != "or_100" {
		t.Errorf("second request cursor = %q, want or_100 (the last id of page 1)",
			s.queries[1]["starting_after"])
	}
}

// A page shorter than the limit is the end. Requesting another would be a
// wasted round trip on every single import.
func TestTicketTailorShortPageEnds(t *testing.T) {
	s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(fullPage("or_", 1, 5)))
	})

	if _, err := drainTT(t, newTTClient(s.URL).Page("orders", nil)); err != nil {
		t.Fatal(err)
	}
	if len(s.queries) != 1 {
		t.Errorf("made %d requests for a short page, want 1", len(s.queries))
	}
}

// Without an id on the last record the cursor cannot advance, so continuing
// would loop on the same page indefinitely.
func TestTicketTailorStopsWhenCursorCannotAdvance(t *testing.T) {
	s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := 0; i < 100; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			if i == 99 {
				b.WriteString(`{"object":"order"}`) // no id
			} else {
				fmt.Fprintf(&b, `{"id":"or_%d","object":"order"}`, i)
			}
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	})

	got, err := drainTT(t, newTTClient(s.URL).Page("orders", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 {
		t.Errorf("collected %d, want the one page of 100", len(got))
	}
	if len(s.queries) != 1 {
		t.Errorf("made %d requests, want 1 — an id-less last record must stop paging",
			len(s.queries))
	}
}

// The dashboard silently truncates at 200 pages. Here it must be an error: a
// silent truncation in a backfill produces a reconciliation that looks complete
// and is not.
func TestTicketTailorPageCapIsAnError(t *testing.T) {
	var n int
	s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
		n++
		_, _ = w.Write([]byte(fullPage("or_", n*100, 100)))
	})

	_, err := drainTT(t, newTTClient(s.URL).Page("orders", nil))
	if err == nil {
		t.Fatal("hitting the page cap was not reported as an error")
	}
	if !strings.Contains(err.Error(), "truncate") {
		t.Errorf("err = %v; it should say the import refuses to truncate silently", err)
	}
}

// HTTP Basic with the key as username and an EMPTY password. The trailing colon
// is load-bearing; without it every request 401s.
func TestTicketTailorAuth(t *testing.T) {
	s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	if _, err := drainTT(t, newTTClient(s.URL).Page("orders", nil)); err != nil {
		t.Fatal(err)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("sk_test_key:"))
	if s.auth[0] != want {
		t.Errorf("Authorization = %q, want %q", s.auth[0], want)
	}
}

func TestTicketTailorSendsLimit(t *testing.T) {
	s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	if _, err := drainTT(t, newTTClient(s.URL).Page("orders", nil)); err != nil {
		t.Fatal(err)
	}
	if s.queries[0]["limit"] != "100" {
		t.Errorf("limit = %q, want 100", s.queries[0]["limit"])
	}
}

// Ticket Tailor is not perfectly consistent about the envelope, so the reference
// falls back through several shapes.
func TestTicketTailorEnvelopeShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"data key", `{"data":[{"id":"a"},{"id":"b"}]}`, 2},
		{"bare list", `[{"id":"a"}]`, 1},
		{"first list-valued key", `{"records":[{"id":"a"},{"id":"b"},{"id":"c"}]}`, 3},
		{"empty", `{"data":[]}`, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := drainTT(t, newTTClient(s.URL).Page("orders", nil))
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("got %d records, want %d", len(got), tc.want)
			}
		})
	}
}

func TestTicketTailorReportsHTTPErrors(t *testing.T) {
	s := newTTServer(t, func(_ map[string]string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
	})

	_, err := drainTT(t, newTTClient(s.URL).Page("orders", nil))
	if err == nil {
		t.Fatal("a 401 was not reported")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v; it should name the status", err)
	}
}
