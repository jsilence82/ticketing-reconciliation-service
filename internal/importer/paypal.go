package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PayPal windowing and paging constants, taken from api/paypal.py rather than
// from the public docs — the dashboard's values are the ones known to work
// against this account.
const (
	payPalWindowDays = 31
	payPalPageSize   = 500

	// The scope Transaction Search requires. A REST app without the Transaction
	// Search feature enabled fails at the token step with an unhelpful 401,
	// which is why ErrTransactionSearchDisabled exists.
	payPalScope = "https://uri.paypal.com/services/reporting/search/read"
)

// ErrTransactionSearchDisabled is the single most common setup failure: the
// PayPal REST app exists and the credentials are right, but the Transaction
// Search feature was never switched on. The raw 401 does not say so.
var ErrTransactionSearchDisabled = errors.New(
	"PayPal returned AUTHENTICATION_FAILURE on Transaction Search. The REST app " +
		"most likely does not have the Transaction Search feature enabled: " +
		"developer.paypal.com -> My Apps -> select the app -> enable " +
		"'Transaction Search' under the Live (or Sandbox) features, then save")

// PayPalClient reads the Transaction Search API and, for the webhook path,
// calls PayPal's own signature verification endpoint.
type PayPalClient struct {
	BaseURL  string
	ClientID string
	Secret   string
	HTTP     *http.Client
	Limiter  *Limiter

	// now is injectable so token-expiry logic is testable without waiting.
	now func() time.Time

	// Two independent caches, deliberately not one. A token minted with
	// payPalScope (Transaction Search) may not carry the Webhooks scope
	// verify-webhook-signature needs, and vice versa — sharing one cache field
	// between them would intermittently authenticate one call with the wrong
	// scope's token depending on request order.
	searchToken       string
	searchTokenExpiry time.Time

	webhookToken       string
	webhookTokenExpiry time.Time
}

// NewPayPalClient builds a client. baseURL selects sandbox or live; the caller
// decides, because that choice is gated by guardrail 4.
func NewPayPalClient(baseURL, clientID, secret string) *PayPalClient {
	return &PayPalClient{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		ClientID: clientID,
		Secret:   secret,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Limiter:  NewLimiter(5),
		now:      time.Now,
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// Token returns a cached Transaction Search access token, refreshing when it
// is close to expiring.
//
// The 60-second margin matters: a token that expires mid-window turns a long
// backfill into a spurious 401 halfway through.
func (c *PayPalClient) Token(ctx context.Context) (string, error) {
	if c.searchToken != "" && c.now().Before(c.searchTokenExpiry) {
		return c.searchToken, nil
	}
	tok, expiry, err := c.fetchToken(ctx, payPalScope)
	if err != nil {
		return "", err
	}
	c.searchToken, c.searchTokenExpiry = tok, expiry
	return tok, nil
}

// webhookToken returns a cached DEFAULT-scope access token — no explicit
// scope parameter, so PayPal grants whatever the REST app itself is
// configured for. verify-webhook-signature needs the app's Webhooks scope,
// which the Transaction-Search-scoped token from Token() does not carry.
func (c *PayPalClient) webhookAccessToken(ctx context.Context) (string, error) {
	if c.webhookToken != "" && c.now().Before(c.webhookTokenExpiry) {
		return c.webhookToken, nil
	}
	tok, expiry, err := c.fetchToken(ctx, "")
	if err != nil {
		return "", err
	}
	c.webhookToken, c.webhookTokenExpiry = tok, expiry
	return tok, nil
}

// fetchToken does the client_credentials round trip. scope == "" omits the
// scope form field entirely, which is what asks PayPal for a token carrying
// the app's full default set of scopes rather than one narrowed to a single
// feature.
func (c *PayPalClient) fetchToken(ctx context.Context, scope string) (string, time.Time, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if scope != "" {
		form.Set("scope", scope)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(strings.TrimSpace(c.ClientID), strings.TrimSpace(c.Secret))

	if err := c.Limiter.Wait(ctx); err != nil {
		return "", time.Time{}, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("paypal token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("paypal token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("paypal token: HTTP %d: %s", resp.StatusCode, truncate(body, 300))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("paypal token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, errors.New("paypal token: response carried no access_token")
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl > 60*time.Second {
		ttl -= 60 * time.Second
	}
	return tr.AccessToken, c.now().Add(ttl), nil
}

// VerifyWebhookSignatureRequest is the body PayPal's
// /v1/notifications/verify-webhook-signature endpoint expects. Field names
// and shape are fixed by PayPal's API, not this service's convention.
type VerifyWebhookSignatureRequest struct {
	TransmissionID   string          `json:"transmission_id"`
	TransmissionTime string          `json:"transmission_time"`
	CertURL          string          `json:"cert_url"`
	AuthAlgo         string          `json:"auth_algo"`
	TransmissionSig  string          `json:"transmission_sig"`
	WebhookID        string          `json:"webhook_id"`
	WebhookEvent     json.RawMessage `json:"webhook_event"`
}

// VerifyWebhookSignature asks PayPal to verify a webhook delivery, rather than
// validating the X.509 cert chain offline.
//
// CLAUDE.md, Architecture, records this as a deliberate decision (2026-08-01):
// at this project's volume the extra round trip is cheap, and delegating
// verification to PayPal avoids re-implementing certificate chain validation,
// which is easy to get subtly wrong (e.g. an unrestricted cert_url fetch is a
// spoofing hole).
//
// WebhookEvent must be the EXACT raw body bytes PayPal sent — re-serializing
// would risk the same key-order/whitespace drift CLAUDE.md warns about for the
// offline CRC32 path, and PayPal's own docs pass the received body through
// unmodified.
func (c *PayPalClient) VerifyWebhookSignature(
	ctx context.Context, req VerifyWebhookSignatureRequest,
) (bool, error) {
	token, err := c.webhookAccessToken(ctx)
	if err != nil {
		return false, err
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("paypal verify webhook signature: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/notifications/verify-webhook-signature", strings.NewReader(string(payload)))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	if err := c.Limiter.Wait(ctx); err != nil {
		return false, err
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("paypal verify webhook signature: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("paypal verify webhook signature: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("paypal verify webhook signature: HTTP %d: %s",
			resp.StatusCode, truncate(body, 300))
	}

	var vr struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.Unmarshal(body, &vr); err != nil {
		return false, fmt.Errorf("paypal verify webhook signature: %w", err)
	}

	return vr.VerificationStatus == "SUCCESS", nil
}

// PayPalPager walks Transaction Search a window and a page at a time.
//
// Windowing reproduces api/paypal.py:45-98 exactly:
//
//	windowEnd = min(cursor+31d, end)
//	start_date = cursor    at 00:00:00+00:00
//	end_date   = windowEnd at 23:59:59+00:00
//	cursor     = windowEnd + 1d
//
// Note that spans 31 days AND 23:59:59, which exceeds PayPal's documented
// 31-day cap. The dashboard runs this in production, so either PayPal is lenient
// or every historical pull happened to hit the min() branch. Reproduced as-is
// rather than "corrected", per the instruction not to silently improve ported
// behaviour — but see the 400 handling in Next, which retries a narrower window
// once rather than failing the whole backfill.
type PayPalPager struct {
	client *PayPalClient
	end    time.Time

	cursor     time.Time
	page       int
	totalPages int
	done       bool
	// narrowed records that the current window was already retried smaller, so
	// a persistent 400 fails instead of looping.
	narrowed bool
}

// Transactions returns a pager over [start, end].
func (c *PayPalClient) Transactions(start, end time.Time) *PayPalPager {
	return &PayPalPager{
		client: c,
		cursor: start.UTC(),
		end:    end.UTC(),
		page:   1,
	}
}

// Next fetches the next page. more is false once the range is exhausted.
func (p *PayPalPager) Next(ctx context.Context) (records []json.RawMessage, more bool, err error) {
	if p.done || !p.cursor.Before(p.end) {
		return nil, false, nil
	}

	windowDays := payPalWindowDays
	if p.narrowed {
		windowDays = 30
	}
	windowEnd := p.cursor.AddDate(0, 0, windowDays)
	if windowEnd.After(p.end) {
		windowEnd = p.end
	}

	recs, totalPages, err := p.client.fetchPage(ctx, p.cursor, windowEnd, p.page)
	if err != nil {
		var de *dateRangeError
		if errors.As(err, &de) && !p.narrowed {
			// PayPal rejected the range. Retry this window once at 30 days
			// rather than abandoning the backfill, and say so loudly.
			p.narrowed = true
			return p.Next(ctx)
		}
		return nil, false, err
	}
	p.totalPages = totalPages

	if p.page >= totalPages {
		// Window exhausted; advance. The +1 day mirrors the reference.
		p.cursor = windowEnd.AddDate(0, 0, 1)
		p.page = 1
		p.narrowed = false
		if !p.cursor.Before(p.end) {
			p.done = true
		}
	} else {
		p.page++
	}

	return recs, !p.done, nil
}

// dateRangeError marks a 400 that names a date-range problem, so Next can
// distinguish it from any other bad request.
type dateRangeError struct{ body string }

func (e *dateRangeError) Error() string { return "paypal date range rejected: " + e.body }

type searchResponse struct {
	TransactionDetails []json.RawMessage `json:"transaction_details"`
	TotalPages         *int              `json:"total_pages"`
}

func (c *PayPalClient) fetchPage(
	ctx context.Context, start, end time.Time, page int,
) ([]json.RawMessage, int, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, 0, err
	}

	q := url.Values{
		"start_date":                     {start.Format("2006-01-02") + "T00:00:00+00:00"},
		"end_date":                       {end.Format("2006-01-02") + "T23:59:59+00:00"},
		"fields":                         {"transaction_info"},
		"page_size":                      {strconv.Itoa(payPalPageSize)},
		"page":                           {strconv.Itoa(page)},
		"balance_affecting_records_only": {"Y"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v1/reporting/transactions?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	if err := c.Limiter.Wait(ctx); err != nil {
		return nil, 0, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("paypal transactions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("paypal transactions: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		var e struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Name == "AUTHENTICATION_FAILURE" {
			return nil, 0, ErrTransactionSearchDisabled
		}
		return nil, 0, fmt.Errorf("paypal 401: %s", truncate(body, 300))

	case resp.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(string(body)), "date"):
		return nil, 0, &dateRangeError{body: truncate(body, 300)}

	case resp.StatusCode != http.StatusOK:
		return nil, 0, fmt.Errorf("paypal HTTP %d: %s", resp.StatusCode, truncate(body, 300))
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, 0, fmt.Errorf("paypal transactions: %w", err)
	}

	// A missing total_pages means exactly one page, matching the reference's
	// .get("total_pages", 1).
	total := 1
	if sr.TotalPages != nil {
		total = *sr.TotalPages
	}

	return sr.TransactionDetails, total, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
