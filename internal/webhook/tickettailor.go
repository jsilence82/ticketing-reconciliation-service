// Package webhook receives provider webhook deliveries and converts them into
// the same model.EventRecord shape the backfill importer produces, so both
// ingestion paths converge on internal/store.Upsert and the shared worker.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/metrics"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// maxTicketTailorBody caps how much of a request body is read before
// verification. Ticket Tailor payloads are a handful of KB; this is only a
// guard against something feeding the endpoint an unbounded stream.
const maxTicketTailorBody = 5 << 20 // 5 MiB

// staleAfter is Ticket Tailor's own recommended replay window, used here only
// to annotate age for observability — it must NOT gate a reject, since Ticket
// Tailor retries failed deliveries for up to 72 hours and a delivery first
// retried after 5 minutes would then fail forever. internal/store.Upsert's
// version-ordered guard is what actually rejects a true replay. See
// docs/ARCHITECTURE.md, "Webhook signature verification".
const staleAfter = 5 * time.Minute

// VerifyTicketTailorSignature checks the HMAC-SHA256 signature Ticket Tailor
// sends in the Tickettailor-Webhook-Signature header, and returns the
// timestamp it was signed with.
//
// The header is two comma-separated key=value parts, timestamp first,
// signature second, parsed POSITIONALLY (Ticket Tailor's own sample code does
// `split(',')` then `split('=')[1]`) rather than by key name. The signed
// message is the timestamp AS A STRING concatenated directly with the raw
// request body bytes — no separator. See docs/ARCHITECTURE.md, "Webhook
// signature verification" for how this was confirmed.
//
// rawBody must be the exact bytes received, captured before any JSON
// decoding — re-serializing would change whitespace and key order and break
// the HMAC.
func VerifyTicketTailorSignature(header string, rawBody []byte, secret string) (timestamp string, err error) {
	parts := strings.Split(header, ",")
	if len(parts) != 2 {
		return "", fmt.Errorf("malformed signature header: want 2 comma-separated parts, got %d", len(parts))
	}

	ts, err := positionalValue(parts[0])
	if err != nil {
		return "", fmt.Errorf("timestamp part: %w", err)
	}
	sig, err := positionalValue(parts[1])
	if err != nil {
		return "", fmt.Errorf("signature part: %w", err)
	}

	got, err := hex.DecodeString(sig)
	if err != nil {
		return "", fmt.Errorf("signature is not hex: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write(rawBody)

	if !hmac.Equal(mac.Sum(nil), got) {
		return "", errors.New("signature mismatch")
	}

	return ts, nil
}

// positionalValue returns the part after "=" in a "key=value" fragment,
// without checking what the key actually is.
func positionalValue(part string) (string, error) {
	kv := strings.SplitN(part, "=", 2)
	if len(kv) != 2 {
		return "", fmt.Errorf("expected key=value, got %q", part)
	}
	return kv[1], nil
}

// ttWebhookEnvelope is Ticket Tailor's webhook delivery wrapper.
// id/created_at/event/resource_url describe the DELIVERY; payload is the
// changed resource in the same representation the REST API returns it in,
// which is why Payload is handed to ingest.FromTicketTailor unchanged rather
// than needing a separate webhook-shaped decoder.
//
// The envelope's own id (e.g. "wh_15") is the delivery's identifier, NOT the
// resource's — it maps to EventRecord.WebhookNotificationID, kept purely for
// debugging delivery-level duplication. The resource's own id inside payload
// is what Upsert dedupes on.
type ttWebhookEnvelope struct {
	ID          string          `json:"id"`
	CreatedAt   string          `json:"created_at"`
	Event       string          `json:"event"`
	ResourceURL string          `json:"resource_url"`
	Payload     json.RawMessage `json:"payload"`
}

// Store is the write-side dependency the handler needs, narrowed so tests can
// substitute a recorder — mirrors ingest.Writer's narrowing of the same store.
type Store interface {
	Upsert(ctx context.Context, rec model.EventRecord) (store.Outcome, error)
}

// TicketTailorHandler receives Ticket Tailor webhook deliveries.
type TicketTailorHandler struct {
	secret string
	store  Store
	log    *slog.Logger
}

// NewTicketTailorHandler builds a handler. secret is TT_WEBHOOK_SECRET; it
// must be non-empty, since an empty secret would make every signature check
// meaningless rather than merely absent.
func NewTicketTailorHandler(secret string, st Store, log *slog.Logger) (*TicketTailorHandler, error) {
	if secret == "" {
		return nil, errors.New("webhook: ticket tailor secret is empty")
	}
	if log == nil {
		log = slog.Default()
	}
	return &TicketTailorHandler{secret: secret, store: st, log: log}, nil
}

func (h *TicketTailorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxTicketTailorBody))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	ts, err := VerifyTicketTailorSignature(
		r.Header.Get("Tickettailor-Webhook-Signature"), rawBody, h.secret)
	if err != nil {
		h.log.Warn("ticket tailor webhook: signature rejected", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	h.logIfStale(ts)

	var env ttWebhookEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		h.log.Error("ticket tailor webhook: malformed envelope", "err", err)
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}

	rec, err := ingest.FromTicketTailor(env.Payload, model.OriginWebhook)
	if err != nil {
		// The resource itself is what's malformed, not something transient on
		// our end — retrying won't fix it, so this is a 400, not a 500. Logged
		// at Error rather than silently dropped, so every ingested payload is
		// still accounted for somewhere.
		h.log.Error("ticket tailor webhook: unparseable resource",
			"event", env.Event, "delivery_id", env.ID, "err", err)
		http.Error(w, "unparseable resource", http.StatusBadRequest)
		return
	}

	rec.WebhookNotificationID = env.ID
	rec.Topic = env.Event
	verifiedAt := time.Now().UTC()
	rec.SignatureVerifiedAt = &verifiedAt

	outcome, err := h.store.Upsert(r.Context(), rec)
	if err != nil {
		// A storage failure IS transient from Ticket Tailor's point of view —
		// returning 5xx lets its retry-with-backoff do its job rather than
		// losing the delivery.
		h.log.Error("ticket tailor webhook: upsert failed",
			"resource_type", rec.ResourceType, "resource_id", rec.ResourceID, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	metrics.EventsIngested.Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("source", "tickettailor"),
		attribute.String("resource_type", string(rec.ResourceType)),
		attribute.String("outcome", string(outcome))))

	h.log.Info("ticket tailor webhook: received",
		"event", env.Event, "resource_type", rec.ResourceType,
		"resource_id", rec.ResourceID, "outcome", outcome)
	w.WriteHeader(http.StatusOK)
}

// logIfStale is observability only — see the staleAfter doc comment for why
// this must never reject.
func (h *TicketTailorHandler) logIfStale(timestamp string) {
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		h.log.Warn("ticket tailor webhook: non-numeric timestamp, cannot check age", "timestamp", timestamp)
		return
	}
	if age := time.Since(time.Unix(sec, 0)); age > staleAfter {
		h.log.Info("ticket tailor webhook: signed timestamp older than 5m (not rejected)", "age", age)
	}
}
