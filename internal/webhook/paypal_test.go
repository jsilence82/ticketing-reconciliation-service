package webhook_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/importer"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/webhook"
)

// fakeVerifier substitutes for a real call to PayPal's
// verify-webhook-signature endpoint, and records the request it was asked to
// verify so tests can assert the handler filled it in from the right headers.
type fakeVerifier struct {
	ok       bool
	err      error
	lastReq  importer.VerifyWebhookSignatureRequest
	callSeen bool
}

func (f *fakeVerifier) VerifyWebhookSignature(
	_ context.Context, req importer.VerifyWebhookSignatureRequest,
) (bool, error) {
	f.callSeen = true
	f.lastReq = req
	return f.ok, f.err
}

const paypalCaptureCompletedBody = `{
	"id": "WH-2WR32451HC0233532-67976317FL4543714",
	"event_type": "PAYMENT.CAPTURE.COMPLETED",
	"resource_type": "capture",
	"create_time": "2026-08-01T12:00:00.000Z",
	"resource": {
		"id": "3C679366HD394342E",
		"status": "COMPLETED",
		"amount": {"value": "10.00", "currency_code": "USD"},
		"seller_receivable_breakdown": {
			"paypal_fee": {"value": "0.59", "currency_code": "USD"}
		},
		"create_time": "2026-08-01T11:59:50.000Z",
		"links": []
	}
}`

func paypalRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/paypal", bytes.NewReader([]byte(body)))
	req.Header.Set("Paypal-Transmission-Id", "tx-1")
	req.Header.Set("Paypal-Transmission-Time", "2026-08-01T12:00:00Z")
	req.Header.Set("Paypal-Cert-Url", "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-1")
	req.Header.Set("Paypal-Auth-Algo", "SHA256withRSA")
	req.Header.Set("Paypal-Transmission-Sig", "deadbeef")
	return req
}

func TestPayPalHandler_ValidCaptureCompleted(t *testing.T) {
	v := &fakeVerifier{ok: true}
	st := &recordingStore{}
	h, err := webhook.NewPayPalHandler(v, "WH-ID-123", st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paypalRequest(paypalCaptureCompletedBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !v.callSeen {
		t.Fatal("verifier was never called")
	}
	if v.lastReq.WebhookID != "WH-ID-123" {
		t.Errorf("verify request WebhookID = %q, want WH-ID-123", v.lastReq.WebhookID)
	}
	if v.lastReq.TransmissionID != "tx-1" {
		t.Errorf("verify request TransmissionID = %q, want tx-1", v.lastReq.TransmissionID)
	}
	if v.lastReq.CertURL != "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-1" {
		t.Errorf("verify request CertURL = %q", v.lastReq.CertURL)
	}

	if len(st.got) != 1 {
		t.Fatalf("got %d upserts, want 1", len(st.got))
	}
	got := st.got[0]
	if got.ResourceID != "3C679366HD394342E" {
		t.Errorf("ResourceID = %q", got.ResourceID)
	}
	if got.Source != model.SourcePayPal {
		t.Errorf("Source = %q, want paypal", got.Source)
	}
	if got.Origin != model.OriginWebhook {
		t.Errorf("Origin = %q, want webhook", got.Origin)
	}
	if got.SignatureVerifiedAt == nil {
		t.Error("SignatureVerifiedAt is nil, want set")
	}
}

func TestPayPalHandler_InvalidSignatureRejected(t *testing.T) {
	v := &fakeVerifier{ok: false}
	st := &recordingStore{}
	h, err := webhook.NewPayPalHandler(v, "WH-ID-123", st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paypalRequest(paypalCaptureCompletedBody))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(st.got) != 0 {
		t.Fatalf("got %d upserts, want 0 — a rejected signature must not reach storage", len(st.got))
	}
}

func TestPayPalHandler_VerificationCallFailureReturns500(t *testing.T) {
	// The verification call itself erroring (network blip, PayPal outage) is
	// not the delivery's fault — must be a 5xx so PayPal retries, not a 4xx.
	v := &fakeVerifier{err: errors.New("connection reset")}
	st := &recordingStore{}
	h, err := webhook.NewPayPalHandler(v, "WH-ID-123", st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paypalRequest(paypalCaptureCompletedBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(st.got) != 0 {
		t.Fatalf("got %d upserts, want 0", len(st.got))
	}
}

func TestPayPalHandler_StorageErrorReturns500(t *testing.T) {
	v := &fakeVerifier{ok: true}
	st := &recordingStore{err: errors.New("connection reset")}
	h, err := webhook.NewPayPalHandler(v, "WH-ID-123", st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paypalRequest(paypalCaptureCompletedBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestPayPalHandler_MalformedDeliveryReturns400(t *testing.T) {
	v := &fakeVerifier{ok: true}
	st := &recordingStore{}
	h, err := webhook.NewPayPalHandler(v, "WH-ID-123", st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	// Verification passes (PayPal confirms it's a genuine delivery) but the
	// body has no envelope id, so ingest.FromPayPalWebhook must reject it.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paypalRequest(`{"event_type": "PAYMENT.CAPTURE.COMPLETED", "resource": {}}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(st.got) != 0 {
		t.Fatalf("got %d upserts, want 0", len(st.got))
	}
}

func TestPayPalHandler_NonWhitelistedTopicStillStoredAsIgnored(t *testing.T) {
	v := &fakeVerifier{ok: true}
	st := &recordingStore{}
	h, err := webhook.NewPayPalHandler(v, "WH-ID-123", st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	body := `{
		"id": "WH-dispute-1",
		"event_type": "CUSTOMER.DISPUTE.CREATED",
		"resource": {"dispute_id": "PP-D-1"}
	}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paypalRequest(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(st.got) != 1 || st.got[0].Status != "ignored" {
		t.Fatalf("got %+v, want one row with Status=ignored", st.got)
	}
}

func TestNewPayPalHandler_EmptyWebhookIDRejected(t *testing.T) {
	if _, err := webhook.NewPayPalHandler(&fakeVerifier{ok: true}, "", &recordingStore{}, slog.Default()); err == nil {
		t.Fatal("want error for empty webhook id, got nil")
	}
}
