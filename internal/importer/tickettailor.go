package importer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Ticket Tailor paging constants, from api/tickettailor.py.
const (
	ttPageLimit = 100
	ttMaxPages  = 200
)

// TicketTailorClient reads the Ticket Tailor REST API.
type TicketTailorClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Limiter *Limiter
}

// NewTicketTailorClient builds a client.
//
// Ticket Tailor has NO sandbox, so any key handed to this is a live key by
// construction. That is why every call site must be behind the --live gate:
// guardrail 4 cannot be delegated to PAYPAL_SANDBOX here.
func NewTicketTailorClient(baseURL, apiKey string) *TicketTailorClient {
	return &TicketTailorClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Limiter: NewLimiter(5),
	}
}

// authHeader is HTTP Basic with the API key as username and an EMPTY password —
// note the trailing colon, which is load-bearing.
func (c *TicketTailorClient) authHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.APIKey+":"))
}

// TicketTailorPager walks one endpoint using cursor pagination.
type TicketTailorPager struct {
	client   *TicketTailorClient
	endpoint string
	extra    url.Values

	after string
	pages int
	done  bool
}

// Page returns a pager over an endpoint such as "orders" or "issued_tickets".
//
// extra carries optional filters. Note CLAUDE.md's warning about
// `created_at.gte`: a watermark misses refunds posted against old orders, so it
// is an optimisation, never the default for a parity run.
func (c *TicketTailorClient) Page(endpoint string, extra url.Values) *TicketTailorPager {
	return &TicketTailorPager{
		client:   c,
		endpoint: strings.Trim(endpoint, "/"),
		extra:    extra,
	}
}

// Next fetches the next page.
//
// Three stop conditions, all reproduced from the reference:
//   - a short page means the end
//   - a last record with no id means the cursor cannot advance
//   - max_pages is a runaway guard
//
// The third differs deliberately: the dashboard silently truncates at 200
// pages, which would quietly lose data. Here it is an error, because a silent
// truncation in a backfill produces a reconciliation that looks complete and is
// not.
func (p *TicketTailorPager) Next(ctx context.Context) (records []json.RawMessage, more bool, err error) {
	if p.done {
		return nil, false, nil
	}

	if p.pages >= ttMaxPages {
		return nil, false, fmt.Errorf(
			"ticket tailor /%s: hit the %d-page cap; refusing to truncate silently",
			p.endpoint, ttMaxPages)
	}

	q := url.Values{}
	for k, vs := range p.extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("limit", strconv.Itoa(ttPageLimit))
	if p.after != "" {
		q.Set("starting_after", p.after)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.client.BaseURL+"/"+p.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", p.client.authHeader())

	if err := p.client.Limiter.Wait(ctx); err != nil {
		return nil, false, err
	}

	resp, err := p.client.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("ticket tailor /%s: %w", p.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, false, fmt.Errorf("ticket tailor /%s: %w", p.endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("ticket tailor /%s: HTTP %d: %s",
			p.endpoint, resp.StatusCode, truncate(body, 300))
	}

	recs, err := extractRecords(body)
	if err != nil {
		return nil, false, fmt.Errorf("ticket tailor /%s: %w", p.endpoint, err)
	}
	p.pages++

	if len(recs) < ttPageLimit {
		p.done = true
		return recs, false, nil
	}

	last := recs[len(recs)-1]
	var idOnly struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(last, &idOnly); err != nil || idOnly.ID == "" {
		// Without an id the cursor cannot advance, so continuing would loop on
		// the same page forever.
		p.done = true
		return recs, false, nil
	}
	p.after = idOnly.ID

	return recs, true, nil
}

// extractRecords unwraps the response envelope.
//
// Mirrors _extract_records: a bare list, else payload["data"], else the first
// list-valued key. The last fallback exists because Ticket Tailor is not
// perfectly consistent about the envelope.
func extractRecords(body []byte) ([]json.RawMessage, error) {
	var asList []json.RawMessage
	if err := json.Unmarshal(body, &asList); err == nil {
		return asList, nil
	}

	var asObj map[string]json.RawMessage
	if err := json.Unmarshal(body, &asObj); err != nil {
		return nil, fmt.Errorf("response is neither a list nor an object: %w", err)
	}

	if data, ok := asObj["data"]; ok {
		var recs []json.RawMessage
		if err := json.Unmarshal(data, &recs); err == nil {
			return recs, nil
		}
	}

	for _, v := range asObj {
		var recs []json.RawMessage
		if err := json.Unmarshal(v, &recs); err == nil {
			return recs, nil
		}
	}

	return nil, nil
}
