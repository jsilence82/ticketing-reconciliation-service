package config

import (
	"errors"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("PAYPAL_SANDBOX", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if !cfg.PayPal.Sandbox {
		t.Error("PayPal.Sandbox = false; it must default to true so that " +
			"development opts IN to production, never out of it")
	}
}

func TestPayPalBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		sandbox bool
		want    string
	}{
		{"sandbox", true, "https://api.sandbox.paypal.com"},
		{"live", false, "https://api.paypal.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := PayPalConfig{Sandbox: tc.sandbox}
			if got := p.BaseURL(); got != tc.want {
				t.Errorf("BaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_SandboxParsing(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    bool
		wantErr bool
	}{
		{"unset stays sandbox", "", true, false},
		{"explicit false goes live", "false", false, false},
		{"explicit 0 goes live", "0", false, false},
		{"explicit true", "true", true, false},
		{"garbage is an error, not a silent live switch", "yes-please", false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAYPAL_SANDBOX", tc.env)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unparseable PAYPAL_SANDBOX")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.PayPal.Sandbox != tc.want {
				t.Errorf("Sandbox = %v, want %v", cfg.PayPal.Sandbox, tc.want)
			}
		})
	}
}

func TestLoad_PortValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    int
		wantErr bool
	}{
		{"default", "", 8080, false},
		{"valid", "9090", 9090, false},
		{"not a number", "http", 0, true},
		{"zero", "0", 0, true},
		{"too large", "70000", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PORT", tc.env)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for PORT=%q", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Port != tc.want {
				t.Errorf("Port = %d, want %d", cfg.Port, tc.want)
			}
		})
	}
}

func TestLoad_RequiredMissing(t *testing.T) {
	t.Setenv("TT_API_KEY", "")
	t.Setenv("PAYPAL_SECRET", "")

	_, err := Load("TT_API_KEY", "PAYPAL_SECRET")
	if err == nil {
		t.Fatal("expected an error when required variables are absent")
	}

	var missing *MissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %T, want *MissingError", err)
	}
	if len(missing.Vars) != 2 {
		t.Errorf("missing.Vars = %v, want both variables reported at once", missing.Vars)
	}
}

func TestLoad_RequiredPresent(t *testing.T) {
	t.Setenv("TT_API_KEY", "tt-key")

	if _, err := Load("TT_API_KEY"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

// A whitespace-only credential is a common .env accident and must be treated as
// absent rather than sent to a provider as a real key.
func TestLoad_WhitespaceCountsAsMissing(t *testing.T) {
	t.Setenv("TT_API_KEY", "   ")

	if _, err := Load("TT_API_KEY"); err == nil {
		t.Fatal("whitespace-only credential was accepted; it must count as missing")
	}
}

func TestLoad_UnknownRequiredName(t *testing.T) {
	if _, err := Load("NOT_A_REAL_VAR"); err == nil {
		t.Fatal("expected an error for an unknown required variable name")
	}
}
