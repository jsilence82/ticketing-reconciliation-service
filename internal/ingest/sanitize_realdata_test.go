package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// The golden tests use invented fixtures, which is right — a PII test
// containing real PII would defeat itself.
//
// But invented fixtures only ever contain the fields someone thought to invent.
// This sweep runs the sanitizer over the actual provider output, where the
// surprises live, and checks the result three ways: by key, by shape, and by
// value. The value check is the one that catches PII hiding under an innocuous
// key name.
//
// Gated on SSG_PARITY_DATA, which points outside the repository.

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

type rawCache struct {
	Orders  []json.RawMessage `json:"orders"`
	Tickets []json.RawMessage `json:"tickets"`
	Events  []json.RawMessage `json:"events"`
}

func loadRawCache(t *testing.T) rawCache {
	t.Helper()

	dir := os.Getenv("SSG_PARITY_DATA")
	if dir == "" {
		t.Skip("SSG_PARITY_DATA unset; see docs/ENVIRONMENT.md")
	}

	blob, err := os.ReadFile(filepath.Join(dir, "tt_raw_cache.json"))
	if err != nil {
		t.Fatalf("read raw cache: %v", err)
	}

	var c rawCache
	if err := json.Unmarshal(blob, &c); err != nil {
		t.Fatalf("parse raw cache: %v", err)
	}
	return c
}

func TestSanitizeRealData(t *testing.T) {
	cache := loadRawCache(t)

	groups := []struct {
		rt   model.ResourceType
		rows []json.RawMessage
	}{
		{model.ResourceOrder, cache.Orders},
		{model.ResourceIssuedTicket, cache.Tickets},
		{model.ResourceEvent, cache.Events},
	}

	for _, g := range groups {
		t.Run(string(g.rt), func(t *testing.T) {
			if len(g.rows) == 0 {
				t.Fatalf("no %s rows in the snapshot", g.rt)
			}

			exempt := PIIExemptions(g.rt)

			// Harvest every string the INPUT contains that identifies a person,
			// so the output can be checked against the actual values rather than
			// only against key names.
			secrets := map[string]bool{}
			for _, raw := range g.rows {
				collectSensitiveValues(t, raw, exempt, secrets)
			}
			t.Logf("harvested %d distinct sensitive values from %d input rows",
				len(secrets), len(g.rows))

			var failures int
			for i, raw := range g.rows {
				out, err := Sanitize(g.rt, raw)
				if err != nil {
					t.Fatalf("row %d: Sanitize: %v", i, err)
				}

				// 1. no forbidden key at any depth
				if err := ScanForPII(out, exempt); err != nil {
					failures++
					if failures <= 5 {
						t.Errorf("row %d: %v", i, err)
					}
					continue
				}

				text := string(out)

				// 2. nothing that looks like an email, whatever key it sits under
				if m := emailPattern.FindString(text); m != "" {
					failures++
					if failures <= 5 {
						t.Errorf("row %d: output contains an email-shaped value", i)
					}
					continue
				}

				// 3. no harvested value survives under any key. This is the check
				//    that catches a buyer name stored under something harmless.
				for s := range secrets {
					if strings.Contains(text, s) {
						failures++
						if failures <= 5 {
							t.Errorf("row %d: output still contains a value harvested "+
								"from the buyer fields of the input", i)
						}
						break
					}
				}
			}

			if failures > 5 {
				t.Errorf("... and %d further rows leaked (of %d)", failures-5, len(g.rows))
			}
			if failures == 0 {
				t.Logf("all %d rows sanitized clean", len(g.rows))
			}
		})
	}
}

// Sanitizing must not throw away what the engine reads. A filter that stripped
// everything would pass the leak checks perfectly.
func TestSanitizeRealDataKeepsWhatTheEngineReads(t *testing.T) {
	cache := loadRawCache(t)

	t.Run("order", func(t *testing.T) {
		var withTxn int
		for i, raw := range cache.Orders {
			out, err := Sanitize(model.ResourceOrder, raw)
			if err != nil {
				t.Fatalf("row %d: %v", i, err)
			}
			var got struct {
				ID            string `json:"id"`
				TxnID         string `json:"txn_id"`
				PaymentMethod struct {
					Type string `json:"type"`
				} `json:"payment_method"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("row %d: decode: %v", i, err)
			}
			if got.ID == "" {
				t.Fatalf("row %d: lost the order id", i)
			}
			if got.PaymentMethod.Type == "" {
				t.Fatalf("row %d: lost payment_method.type, which the matching rule reads", i)
			}
			if got.TxnID != "" {
				withTxn++
			}
		}
		// txn_id is THE match key. If sanitization dropped it, reconciliation
		// would silently match nothing.
		if withTxn == 0 {
			t.Fatal("not one order kept a txn_id; the match key is being stripped")
		}
		t.Logf("%d of %d orders retained a txn_id", withTxn, len(cache.Orders))
	})

	t.Run("issued_ticket", func(t *testing.T) {
		for i, raw := range cache.Tickets {
			out, err := Sanitize(model.ResourceIssuedTicket, raw)
			if err != nil {
				t.Fatalf("row %d: %v", i, err)
			}
			var got struct {
				ID          string `json:"id"`
				OrderID     string `json:"order_id"`
				EventID     string `json:"event_id"`
				Description string `json:"description"`
				Status      string `json:"status"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("row %d: decode: %v", i, err)
			}
			// description becomes the canonical `category`, status drives the
			// active and transferred masks, and the two ids drive the join.
			if got.ID == "" || got.OrderID == "" || got.EventID == "" ||
				got.Description == "" || got.Status == "" {
				t.Fatalf("row %d: lost a field the engine reads: %+v", i, got)
			}
		}
		t.Logf("all %d tickets retained their engine-visible fields", len(cache.Tickets))
	})
}

// collectSensitiveValues gathers the person-identifying strings present in a raw
// payload, so the sanitized output can be searched for them by value.
// collectSensitiveValues takes the same exemptions the scanner does. Without
// that, an event's `name` — the show title, legitimately kept — is harvested as
// sensitive and then found in the output, reporting a leak that is not one.
func collectSensitiveValues(t *testing.T, raw json.RawMessage, exempt, into map[string]bool) {
	t.Helper()

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	gather(v, false, exempt, into)
}

func gather(v any, sensitive bool, exempt, into map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			// Once inside a buyer subtree, everything below it counts.
			isSensitive := sensitive || (model.IsForbiddenKey(k) && !exempt[k])
			gather(child, isSensitive, exempt, into)
		}
	case []any:
		for _, child := range t {
			gather(child, sensitive, exempt, into)
		}
	case string:
		// Short strings produce false positives ("eur", "valid"), so only keep
		// values long enough to be distinctive.
		if sensitive && len(t) >= 6 {
			into[t] = true
		}
	}
}
