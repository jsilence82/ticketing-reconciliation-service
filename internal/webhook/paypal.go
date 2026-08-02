package webhook

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/importer"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/metrics"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
)

// maxPayPalBody mirrors maxTicketTailorBody's reasoning: PayPal payloads are a
// handful of KB, this only guards against an unbounded stream.
const maxPayPalBody = 5 << 20 // 5 MiB

// PayPalVerifier checks a webhook delivery's signature.
//
// CLAUDE.md, Architecture, records the choice behind this shape as deliberate
// (2026-08-01): it calls PayPal's own /v1/notifications/verify-webhook-signature
// endpoint rather than validating the X.509 cert chain offline, delegating the
// security-sensitive part to PayPal rather than reimplementing it.
// *importer.PayPalClient satisfies this interface structurally — no adapter
// needed — and tests substitute a fake.
type PayPalVerifier interface {
	VerifyWebhookSignature(ctx context.Context, req importer.VerifyWebhookSignatureRequest) (bool, error)
}

// PayPalHandler receives PayPal webhook deliveries.
type PayPalHandler struct {
	verifier  PayPalVerifier
	webhookID string
	store     Store
	log       *slog.Logger
}

// NewPayPalHandler builds a handler. webhookID is PAYPAL_WEBHOOK_ID — required,
// since verify-webhook-signature cannot check anything against an empty one.
func NewPayPalHandler(verifier PayPalVerifier, webhookID string, st Store, log *slog.Logger) (*PayPalHandler, error) {
	if webhookID == "" {
		return nil, errors.New("webhook: paypal webhook id is empty")
	}
	if log == nil {
		log = slog.Default()
	}
	return &PayPalHandler{verifier: verifier, webhookID: webhookID, store: st, log: log}, nil
}

func (h *PayPalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxPayPalBody))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	req := importer.VerifyWebhookSignatureRequest{
		TransmissionID:   r.Header.Get("Paypal-Transmission-Id"),
		TransmissionTime: r.Header.Get("Paypal-Transmission-Time"),
		CertURL:          r.Header.Get("Paypal-Cert-Url"),
		AuthAlgo:         r.Header.Get("Paypal-Auth-Algo"),
		TransmissionSig:  r.Header.Get("Paypal-Transmission-Sig"),
		WebhookID:        h.webhookID,
		WebhookEvent:     rawBody,
	}

	ok, err := h.verifier.VerifyWebhookSignature(r.Context(), req)
	if err != nil {
		// The verification call itself failed (network, PayPal outage, bad
		// OAuth token) — that is transient and not the delivery's fault, so a
		// 5xx lets PayPal's own retry-with-backoff do its job rather than
		// losing the delivery.
		h.log.Error("paypal webhook: verification call failed", "err", err)
		http.Error(w, "verification unavailable", http.StatusInternalServerError)
		return
	}
	if !ok {
		h.log.Warn("paypal webhook: signature rejected")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	rec, err := ingest.FromPayPalWebhook(rawBody, model.OriginWebhook)
	if err != nil {
		// The delivery itself is what's malformed — retrying will not fix it.
		h.log.Error("paypal webhook: unparseable delivery", "err", err)
		http.Error(w, "unparseable delivery", http.StatusBadRequest)
		return
	}

	verifiedAt := time.Now().UTC()
	rec.SignatureVerifiedAt = &verifiedAt

	outcome, err := h.store.Upsert(r.Context(), rec)
	if err != nil {
		h.log.Error("paypal webhook: upsert failed",
			"resource_type", rec.ResourceType, "resource_id", rec.ResourceID, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	metrics.EventsIngested.Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("source", "paypal"),
		attribute.String("resource_type", string(rec.ResourceType)),
		attribute.String("outcome", string(outcome))))

	h.log.Info("paypal webhook: received",
		"topic", rec.Topic, "resource_type", rec.ResourceType,
		"resource_id", rec.ResourceID, "outcome", outcome)
	w.WriteHeader(http.StatusOK)
}
