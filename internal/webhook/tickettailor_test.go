package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/webhook"
)

// --- signature verification (no database, no HTTP) --------------------------

// wantSig is computed independently in Python (not by calling the Go code
// under test), against secret/timestamp/body below:
//
//	python3 -c "
//	import hmac, hashlib
//	secret = 'whsec_test_shared_secret'
//	timestamp = '1735689600'
//	body = b'{\"id\":\"or_test123\",\"event\":\"ORDER.CREATED\"}'
//	print(hmac.new(secret.encode(), timestamp.encode()+body, hashlib.sha256).hexdigest())
//	"
//
// That is the same guardrail CLAUDE.md applies to money parity: a signature
// scheme validated only against itself proves nothing.
const (
	testSecret    = "whsec_test_shared_secret"
	testTimestamp = "1735689600"
	testBody      = `{"id":"or_test123","event":"ORDER.CREATED"}`
	wantSig       = "b34a9f4d3ec83ee8b2487eaafb8cad12378a71b16d0907638ae6e932fa293b60"
)

func TestVerifyTicketTailorSignature_KnownVector(t *testing.T) {
	header := fmt.Sprintf("t=%s,s=%s", testTimestamp, wantSig)

	ts, err := webhook.VerifyTicketTailorSignature(header, []byte(testBody), testSecret)
	if err != nil {
		t.Fatalf("VerifyTicketTailorSignature: %v", err)
	}
	if ts != testTimestamp {
		t.Errorf("timestamp = %q, want %q", ts, testTimestamp)
	}
}

func TestVerifyTicketTailorSignature_KeyLettersDoNotMatter(t *testing.T) {
	// Ticket Tailor's own sample parses positionally (split(',') then
	// split('=')[1]), not by key name. Any key letter in the first position
	// must work as the timestamp, and any in the second as the signature.
	header := fmt.Sprintf("whatever=%s,anything=%s", testTimestamp, wantSig)

	if _, err := webhook.VerifyTicketTailorSignature(header, []byte(testBody), testSecret); err != nil {
		t.Fatalf("VerifyTicketTailorSignature: %v", err)
	}
}

func TestVerifyTicketTailorSignature_Rejects(t *testing.T) {
	validHeader := fmt.Sprintf("t=%s,s=%s", testTimestamp, wantSig)

	tests := []struct {
		name   string
		header string
		body   string
		secret string
	}{
		{"wrong secret", validHeader, testBody, "not-the-real-secret"},
		{"tampered body", validHeader, testBody + " ", testSecret},
		{"tampered timestamp", fmt.Sprintf("t=%s,s=%s", "1735689601", wantSig), testBody, testSecret},
		{"malformed header, no comma", "t=" + testTimestamp, testBody, testSecret},
		{"malformed header, no equals", "t=" + testTimestamp + ",xyz", testBody, testSecret},
		{"signature not hex", fmt.Sprintf("t=%s,s=not-hex!!", testTimestamp), testBody, testSecret},
		{"empty header", "", testBody, testSecret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := webhook.VerifyTicketTailorSignature(tt.header, []byte(tt.body), tt.secret); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

// generateValidHeader is a self-contained helper for handler-level tests below,
// where the point is exercising the HTTP/store plumbing rather than the
// cryptography (which the tests above already check against an independent
// vector).
func generateValidHeader(t *testing.T, secret, timestamp string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(body)
	return fmt.Sprintf("t=%s,s=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

// --- handler (fake store, real HTTP plumbing) --------------------------------

type recordingStore struct {
	got []model.EventRecord
	err error
}

func (s *recordingStore) Upsert(_ context.Context, rec model.EventRecord) (store.Outcome, error) {
	if s.err != nil {
		return "", s.err
	}
	s.got = append(s.got, rec)
	return store.Inserted, nil
}

func envelope(t *testing.T, id, event string, payload map[string]any) []byte {
	t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(map[string]any{
		"id":           id,
		"created_at":   "2025-01-01 10:00:00",
		"event":        event,
		"resource_url": "https://api.tickettailor.com/v1/orders/or_737352",
		"payload":      json.RawMessage(p),
	})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestTicketTailorHandler_ValidOrderCreated(t *testing.T) {
	body := envelope(t, "wh_15", "ORDER.CREATED", map[string]any{
		"object":     "order",
		"id":         "or_737352",
		"created_at": 1710000000,
		"txn_id":     "TXNABC",
	})

	st := &recordingStore{}
	h, err := webhook.NewTicketTailorHandler(testSecret, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/tickettailor", bytes.NewReader(body))
	req.Header.Set("Tickettailor-Webhook-Signature", generateValidHeader(t, testSecret, "1735689600", body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(st.got) != 1 {
		t.Fatalf("got %d upserts, want 1", len(st.got))
	}

	got := st.got[0]
	if got.ResourceID != "or_737352" {
		t.Errorf("ResourceID = %q, want or_737352", got.ResourceID)
	}
	if got.ResourceType != model.ResourceOrder {
		t.Errorf("ResourceType = %q, want order", got.ResourceType)
	}
	if got.WebhookNotificationID != "wh_15" {
		t.Errorf("WebhookNotificationID = %q, want wh_15", got.WebhookNotificationID)
	}
	if got.Topic != "ORDER.CREATED" {
		t.Errorf("Topic = %q, want ORDER.CREATED", got.Topic)
	}
	if got.Origin != model.OriginWebhook {
		t.Errorf("Origin = %q, want webhook", got.Origin)
	}
	if got.SignatureVerifiedAt == nil {
		t.Error("SignatureVerifiedAt is nil, want set")
	}

	var rm metricdata.ResourceMetrics
	if err := testMetricsReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	attrs := attribute.NewSet(
		attribute.String("source", "tickettailor"),
		attribute.String("resource_type", string(model.ResourceOrder)),
		attribute.String("outcome", "inserted"))
	if got := findMetricSum(t, rm, "events.ingested", attrs); got < 1 {
		t.Errorf("events.ingested{source=tickettailor,resource_type=order,outcome=inserted} = %d, want >= 1", got)
	}
}

func TestTicketTailorHandler_InvalidSignatureRejected(t *testing.T) {
	body := envelope(t, "wh_16", "ORDER.CREATED", map[string]any{
		"object": "order", "id": "or_1", "created_at": 1710000000,
	})

	st := &recordingStore{}
	h, err := webhook.NewTicketTailorHandler(testSecret, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/tickettailor", bytes.NewReader(body))
	req.Header.Set("Tickettailor-Webhook-Signature", generateValidHeader(t, "wrong-secret", "1735689600", body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(st.got) != 0 {
		t.Fatalf("got %d upserts, want 0 — invalid signature must not reach storage", len(st.got))
	}
}

func TestTicketTailorHandler_UnknownResourceStoredAsIgnored(t *testing.T) {
	// A waitlist signup (or any future event type) has no "object" mapping in
	// ttObjectTypes, so it becomes ResourceOther/ignored rather than a hard
	// failure — CLAUDE.md: stored for the audit trail, never reconciled.
	body := envelope(t, "wh_17", "WAITLIST_SIGNUP.CREATED", map[string]any{
		"object": "waitlist_signup", "id": "wl_1", "created_at": 1710000000,
	})

	st := &recordingStore{}
	h, err := webhook.NewTicketTailorHandler(testSecret, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/tickettailor", bytes.NewReader(body))
	req.Header.Set("Tickettailor-Webhook-Signature", generateValidHeader(t, testSecret, "1735689600", body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(st.got) != 1 || st.got[0].Status != "ignored" {
		t.Fatalf("got %+v, want one row with Status=ignored", st.got)
	}
}

func TestTicketTailorHandler_StorageErrorReturns500(t *testing.T) {
	body := envelope(t, "wh_18", "ORDER.CREATED", map[string]any{
		"object": "order", "id": "or_1", "created_at": 1710000000,
	})

	st := &recordingStore{err: fmt.Errorf("connection reset")}
	h, err := webhook.NewTicketTailorHandler(testSecret, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/tickettailor", bytes.NewReader(body))
	req.Header.Set("Tickettailor-Webhook-Signature", generateValidHeader(t, testSecret, "1735689600", body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	// 5xx, not 4xx: this must make Ticket Tailor retry rather than give up,
	// since the delivery itself was valid.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestTicketTailorHandler_StaleTimestampStillAccepted(t *testing.T) {
	// CLAUDE.md is explicit: a blanket 5-minute reject would break Ticket
	// Tailor's own 72-hour retry policy. An old but validly-signed, novel
	// delivery must still be processed.
	body := envelope(t, "wh_19", "ORDER.CREATED", map[string]any{
		"object": "order", "id": "or_stale", "created_at": 1710000000,
	})

	oldTimestamp := fmt.Sprintf("%d", time.Now().Add(-72*time.Hour).Unix())

	st := &recordingStore{}
	h, err := webhook.NewTicketTailorHandler(testSecret, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/tickettailor", bytes.NewReader(body))
	req.Header.Set("Tickettailor-Webhook-Signature", generateValidHeader(t, testSecret, oldTimestamp, body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (old timestamp must not be a blanket reject)", rec.Code)
	}
	if len(st.got) != 1 {
		t.Fatalf("got %d upserts, want 1", len(st.got))
	}
}

func TestNewTicketTailorHandler_EmptySecretRejected(t *testing.T) {
	if _, err := webhook.NewTicketTailorHandler("", &recordingStore{}, slog.Default()); err == nil {
		t.Fatal("want error for empty secret, got nil")
	}
}
