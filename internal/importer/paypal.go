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

// PayPalClient reads the Transaction Search API.
type PayPalClient struct {
	BaseURL  string
	ClientID string
	Secret   string
	HTTP     *http.Client
	Limiter  *Limiter

	// now is injectable so token-expiry logic is testable without waiting.
	now func() time.Time

	token       string
	tokenExpiry time.Time
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

// Token returns a cached access token, refreshing when it is close to expiring.
//
// The 60-second margin matters: a token that expires mid-window turns a long
// backfill into a spurious 401 halfway through.
func (c *PayPalClient) Token(ctx context.Context) (string, error) {
	if c.token != "" && c.now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {payPalScope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(strings.TrimSpace(c.ClientID), strings.TrimSpace(c.Secret))

	if err := c.Limiter.Wait(ctx); err != nil {
		return "", err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("paypal token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("paypal token: HTTP %d: %s", resp.StatusCode, truncate(body, 300))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("paypal token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("paypal token: response carried no access_token")
	}

	c.token = tr.AccessToken
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl > 60*time.Second {
		ttl -= 60 * time.Second
	}
	c.tokenExpiry = c.now().Add(ttl)

	return c.token, nil
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
