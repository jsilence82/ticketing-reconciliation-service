// Package config loads service configuration from the environment.
//
// Deliberate design choice: secrets have NO defaults. A missing credential is
// a startup error, never a silently-substituted development value — that is
// precisely how a service ends up talking to production with test settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the full service configuration.
type Config struct {
	// Port is the HTTP listen port. Defaults to 8080.
	Port int

	// MetricsPort is the internal-only port serving Prometheus scrapes.
	// Deliberately NOT the same mux Port serves: the Caddyfile's site block
	// proxies everything on Port to the internet with no path matching, so
	// /metrics has to live elsewhere to stay internal-only. Not published by
	// compose.yml — only the prometheus sibling container reaches it, over
	// the Compose network. Defaults to 9464, the OTel/Prometheus convention.
	MetricsPort int

	// DatabaseURL is the Postgres connection string used by the worker, which
	// writes.
	DatabaseURL string

	// APIDatabaseURL is the connection string the read-only query API uses.
	//
	// Deliberately separate from DatabaseURL: no endpoint may mutate state, and
	// the way to guarantee that is a role with SELECT only (see
	// deploy/readonly-role.sql). The server proves the role really is
	// read-only at startup rather than assuming it.
	APIDatabaseURL string

	// APIKeys is "name:key,name:key" — one key per consumer, so a single
	// caller can be revoked without rotating everyone. Reconciliation data is
	// restricted to whoever handles SSG finances, which a shared secret cannot
	// express.
	APIKeys string

	// TicketTailor holds Ticket Tailor API credentials.
	TicketTailor TicketTailorConfig

	// PayPal holds PayPal API credentials.
	PayPal PayPalConfig

	// ParityDataDir points at the out-of-tree directory holding historical
	// data for the parity harness. Empty means the parity tests skip.
	//
	// It is intentionally NOT a path inside the repo: that data is live and
	// contains buyer PII, which must never exist in this tree in any form.
	ParityDataDir string
}

// TicketTailorConfig holds Ticket Tailor API access.
type TicketTailorConfig struct {
	APIKey string
	// WebhookSecret verifies inbound webhook signatures.
	WebhookSecret string
}

// PayPalConfig holds PayPal API access.
type PayPalConfig struct {
	ClientID string
	Secret   string
	// Sandbox selects the sandbox host. It defaults to TRUE: development must
	// opt IN to production, never out of it.
	Sandbox bool
	// WebhookID is required to verify inbound webhook signatures.
	WebhookID string
}

// BaseURL returns the PayPal API host for the configured environment.
func (p PayPalConfig) BaseURL() string {
	if p.Sandbox {
		return "https://api.sandbox.paypal.com"
	}
	return "https://api.paypal.com"
}

// MissingError reports required environment variables that were absent.
type MissingError struct {
	Vars []string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("missing required environment variable(s): %s",
		strings.Join(e.Vars, ", "))
}

// Load reads configuration from the environment.
//
// required names the variables that must be present. Callers pass only what
// they actually need, so cmd/parity does not demand PayPal credentials it will
// never use.
func Load(required ...string) (*Config, error) {
	cfg := &Config{
		Port:           8080,
		MetricsPort:    9464,
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		APIDatabaseURL: os.Getenv("API_DATABASE_URL"),
		APIKeys:        os.Getenv("API_KEYS"),
		ParityDataDir:  os.Getenv("SSG_PARITY_DATA"),
		TicketTailor: TicketTailorConfig{
			APIKey:        os.Getenv("TT_API_KEY"),
			WebhookSecret: os.Getenv("TT_WEBHOOK_SECRET"),
		},
		PayPal: PayPalConfig{
			ClientID:  os.Getenv("PAYPAL_CLIENT_ID"),
			Secret:    os.Getenv("PAYPAL_SECRET"),
			WebhookID: os.Getenv("PAYPAL_WEBHOOK_ID"),
			Sandbox:   true,
		},
	}

	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("PORT=%q is not a number: %w", v, err)
		}
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("PORT=%d out of range 1-65535", p)
		}
		cfg.Port = p
	}

	if v := os.Getenv("METRICS_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("METRICS_PORT=%q is not a number: %w", v, err)
		}
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("METRICS_PORT=%d out of range 1-65535", p)
		}
		cfg.MetricsPort = p
	}

	// Production must be selected explicitly. Any value other than a clear
	// "false" keeps sandbox on, so a typo cannot silently point development at
	// SSG's live PayPal account.
	if v := os.Getenv("PAYPAL_SANDBOX"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("PAYPAL_SANDBOX=%q is not a boolean: %w", v, err)
		}
		cfg.PayPal.Sandbox = b
	}

	if err := cfg.require(required); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) require(names []string) error {
	lookup := map[string]string{
		"DATABASE_URL":      c.DatabaseURL,
		"API_DATABASE_URL":  c.APIDatabaseURL,
		"API_KEYS":          c.APIKeys,
		"SSG_PARITY_DATA":   c.ParityDataDir,
		"TT_API_KEY":        c.TicketTailor.APIKey,
		"TT_WEBHOOK_SECRET": c.TicketTailor.WebhookSecret,
		"PAYPAL_CLIENT_ID":  c.PayPal.ClientID,
		"PAYPAL_SECRET":     c.PayPal.Secret,
		"PAYPAL_WEBHOOK_ID": c.PayPal.WebhookID,
	}

	var missing []string
	for _, n := range names {
		v, known := lookup[n]
		if !known {
			return fmt.Errorf("config: unknown required variable %q", n)
		}
		if strings.TrimSpace(v) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return &MissingError{Vars: missing}
	}
	return nil
}

// ErrLiveNotPermitted is returned when a command that touches live provider
// accounts is invoked without explicit opt-in.
var ErrLiveNotPermitted = errors.New(
	"refusing to use live provider credentials: pass --live to confirm")
