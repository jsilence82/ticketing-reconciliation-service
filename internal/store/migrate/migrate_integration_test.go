package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These tests need a real Postgres and skip without DATABASE_URL, matching the
// SSG_PARITY_DATA idiom already used in internal/snapshot. One convention, not
// two.
//
//	brew services start postgresql@17
//	createdb trs_test
//	DATABASE_URL='postgres://localhost/trs_test?sslmode=disable' make test
//
// Each test gets its OWN schema rather than sharing one and truncating, so
// `go test -p N` works against a single database and the migration test gets a
// genuinely empty namespace.

func testConn(t *testing.T) (*pgx.Conn, string) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// A skip-based strategy is only trustworthy if it cannot skip silently
		// where it matters. In CI an unset DSN means the services block broke,
		// not that integration testing is optional.
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL is unset in CI; the postgres service block is not wired up")
		}
		t.Skip("DATABASE_URL unset; see docs/ENVIRONMENT.md")
	}

	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}

	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "trs_test_" + hex.EncodeToString(buf[:])

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(c, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = conn.Close(c)
	})

	return conn, schema
}

func TestApply(t *testing.T) {
	conn, _ := testConn(t)
	ctx := context.Background()

	applied, err := Apply(ctx, conn)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("Apply reported no migrations applied to an empty schema")
	}
	t.Logf("applied %v", applied)

	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != len(applied) {
		t.Errorf("schema_migrations has %d rows, applied %d", n, len(applied))
	}
}

// Apply runs on every start, so a second run must be a cheap no-op rather than
// an error or a duplicate.
func TestApplyIsIdempotent(t *testing.T) {
	conn, _ := testConn(t)
	ctx := context.Background()

	first, err := Apply(ctx, conn)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	second, err := Apply(ctx, conn)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second Apply re-applied %v; it must be a no-op", second)
	}
	if len(first) == 0 {
		t.Error("first Apply applied nothing")
	}
}

// An edited migration means the repo and the database disagree about what the
// schema is. Refusing is the only safe answer.
func TestApplyRejectsTamperedChecksum(t *testing.T) {
	conn, _ := testConn(t)
	ctx := context.Background()

	if _, err := Apply(ctx, conn); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, err := conn.Exec(ctx,
		`UPDATE schema_migrations SET checksum = '\x00'::bytea WHERE version = 1`,
	); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := Apply(ctx, conn)
	if err == nil {
		t.Fatal("Apply accepted a migration whose recorded checksum no longer matches")
	}
	t.Logf("correctly refused: %v", err)
}

// V1 encodes several decisions with written rationale. Assert the shape that
// actually reaches Postgres, not just the text of the file.
func TestV1ProducesExpectedSchema(t *testing.T) {
	conn, schema := testConn(t)
	ctx := context.Background()

	if _, err := Apply(ctx, conn); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	t.Run("columns", func(t *testing.T) {
		want := map[string]string{
			"id":                      "uuid",
			"seq":                     "bigint",
			"source":                  "text",
			"resource_type":           "text",
			"resource_id":             "text",
			"webhook_notification_id": "text",
			"origin":                  "text",
			"topic":                   "text",
			"status":                  "text",
			"payload":                 "jsonb",
			"payload_hash":            "bytea",
			"occurred_at":             "timestamp with time zone",
			"recon_status":            "text",
			"recon_counterpart_id":    "text",
			"recon_run_id":            "uuid",
			"retry_count":             "integer",
			"next_attempt_at":         "timestamp with time zone",
			"last_error":              "text",
			"signature_verified_at":   "timestamp with time zone",
			"created_at":              "timestamp with time zone",
			"updated_at":              "timestamp with time zone",
		}

		rows, err := conn.Query(ctx, `
			SELECT column_name, data_type
			  FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 'events'`, schema)
		if err != nil {
			t.Fatalf("query columns: %v", err)
		}
		defer rows.Close()

		got := map[string]string{}
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[name] = typ
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}

		for name, typ := range want {
			actual, ok := got[name]
			if !ok {
				t.Errorf("column %q is missing", name)
				continue
			}
			if actual != typ {
				t.Errorf("column %q is %s, want %s", name, actual, typ)
			}
		}
		for name := range got {
			if _, ok := want[name]; !ok {
				t.Errorf("unexpected column %q — update this test if it is intentional", name)
			}
		}
	})

	t.Run("seq is an identity column", func(t *testing.T) {
		// seq is the ingest ordinal the reconcile read sorts by. If it stops
		// being generated, ordering silently falls back to whatever the planner
		// returns and parity-db loses bit-exactness.
		var identity string
		if err := conn.QueryRow(ctx, `
			SELECT is_identity FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 'events' AND column_name = 'seq'`,
			schema).Scan(&identity); err != nil {
			t.Fatalf("query: %v", err)
		}
		if identity != "YES" {
			t.Errorf("seq is_identity = %q, want YES", identity)
		}
	})

	t.Run("dedup key is unique", func(t *testing.T) {
		if _, err := conn.Exec(ctx, insertMinimal, "paypal", "paypal_transaction", "TX1"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		_, err := conn.Exec(ctx, insertMinimal, "paypal", "paypal_transaction", "TX1")
		if err == nil {
			t.Error("duplicate (source, resource_id) was accepted; the dedup key is not enforced")
		}
	})

	t.Run("check constraints reject bad enums", func(t *testing.T) {
		cases := []struct{ name, source, rtype string }{
			{"bad source", "stripe", "paypal_transaction"},
			{"bad resource_type", "paypal", "invoice"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := conn.Exec(ctx, insertMinimal, tc.source, tc.rtype, "X-"+tc.name)
				if err == nil {
					t.Errorf("accepted %s=%q/%q; the CHECK constraint is missing",
						tc.name, tc.source, tc.rtype)
				}
			})
		}
	})

	t.Run("recon_status permits pending", func(t *testing.T) {
		// An unresolved refund reference is ReconPending, not unmatched — the
		// CHECK constraint must allow the value the classifier actually emits.
		if _, err := conn.Exec(ctx, `
			INSERT INTO events (source, resource_type, resource_id, origin, status,
			                    payload, payload_hash, occurred_at, recon_status)
			VALUES ('paypal','paypal_transaction','R-pending','backfill','received',
			        '{}'::jsonb, '\x00'::bytea, now(), 'pending')`); err != nil {
			t.Errorf("recon_status='pending' rejected: %v", err)
		}
	})

	t.Run("indexes exist", func(t *testing.T) {
		want := []string{
			"events_created_at_id_idx",
			"events_source_ck_idx",
			"events_status_ck_idx",
			"events_recon_ck_idx",
			"events_occurred_at_idx",
			"events_claim_idx",
			"events_read_idx",
		}
		rows, err := conn.Query(ctx,
			`SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = 'events'`, schema)
		if err != nil {
			t.Fatalf("query indexes: %v", err)
		}
		defer rows.Close()

		got := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[n] = true
		}
		for _, n := range want {
			if !got[n] {
				t.Errorf("index %q is missing", n)
			}
		}
	})
}

const insertMinimal = `
INSERT INTO events (source, resource_type, resource_id, origin, status,
                    payload, payload_hash, occurred_at)
VALUES ($1, $2, $3, 'backfill', 'received', '{}'::jsonb, '\x00'::bytea, now())`
