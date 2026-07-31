package migrate

import (
	"regexp"
	"strings"
	"testing"
)

// These tests take no database on purpose. Misnaming a migration or leaving a
// gap in the version sequence are the mistakes that happen on a clean checkout,
// and catching them should not depend on anyone having Postgres running.

func TestLoad(t *testing.T) {
	ms, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded; has the //go:embed pattern stopped matching?")
	}

	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("migration %d has version %d; versions must be contiguous from 1", i, m.Version)
		}
		if m.Name == "" {
			t.Errorf("V%d has an empty name", m.Version)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("V%d__%s is empty", m.Version, m.Name)
		}
		if len(m.Checksum) != 32 {
			t.Errorf("V%d__%s checksum is %d bytes, want 32", m.Version, m.Name, len(m.Checksum))
		}
	}

	t.Logf("%d migration(s) embedded", len(ms))
}

// The filename rule is the whole safety mechanism: anything that does not match
// is refused rather than skipped. A regression here would let a misnamed
// migration silently never run.
func TestFilePattern(t *testing.T) {
	tests := []struct {
		name  string
		match bool
	}{
		{"V1__create_events.sql", true},
		{"V12__add_recon_state.sql", true},
		{"V1__a.sql", true},

		{"V0001_create_events.sql", false}, // single underscore (goose style)
		{"1__create_events.sql", false},    // no V prefix
		{"V1__CreateEvents.sql", false},    // not snake_case
		{"V1__create-events.sql", false},   // hyphen
		{"V__create_events.sql", false},    // no version
		{"Vx__create_events.sql", false},   // non-numeric version
		{"V1__create_events.txt", false},   // wrong extension
		{"V1_create_events.sql", false},    // single underscore
		{"create_events.sql", false},       // no version at all
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filePattern.MatchString(tc.name); got != tc.match {
				t.Errorf("filePattern.MatchString(%q) = %v, want %v", tc.name, got, tc.match)
			}
		})
	}
}

// Migrations are immutable once applied, which only works if the checksum is
// over the file's exact bytes.
func TestLoadIsDeterministic(t *testing.T) {
	first, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	second, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("Load returned %d then %d migrations", len(first), len(second))
	}
	for i := range first {
		if !equalBytes(first[i].Checksum, second[i].Checksum) {
			t.Errorf("V%d checksum is not stable across calls", first[i].Version)
		}
	}
}

// V1 carries several decisions that are easy to "tidy" away without realising
// what they were for. Pin the ones with a written rationale.
func TestV1SchemaInvariants(t *testing.T) {
	ms, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v1 := ms[0].SQL

	required := []struct {
		fragment string
		why      string
	}{
		{"seq                     bigint GENERATED ALWAYS AS IDENTITY",
			"the ingest ordinal; the reconcile read orders by it because the engine is order-sensitive"},
		{"payload_hash            bytea NOT NULL",
			"required, not optional: TT orders have no updated_at, so the hash is the only discriminator on a refund resync"},
		{"next_attempt_at         timestamptz NOT NULL",
			"backoff is unimplementable with retry_count alone"},
		{"UNIQUE (source, resource_id)",
			"the dedup key that makes webhook and backfill converge"},
		{"'pending'",
			"recon_status must permit pending: an unresolved refund reference is not the same as unmatched"},
		{"events_claim_idx",
			"the worker claim index"},
		{"events_read_idx",
			"the reconcile read index, which must be able to return ingest order"},
	}

	for _, r := range required {
		if !strings.Contains(v1, r.fragment) {
			t.Errorf("V1 no longer contains %q\n  that fragment exists because: %s", r.fragment, r.why)
		}
	}

	// Down migrations are deliberately absent; rollback is a restore.
	if regexp.MustCompile(`(?i)drop\s+table`).MatchString(v1) {
		t.Error("V1 contains DROP TABLE; migrations are forward-only")
	}
}
