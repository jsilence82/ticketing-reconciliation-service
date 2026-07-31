// Command backfill imports historical data into the events table.
//
// Webhooks only fire forward from when a subscription is created, so history —
// including everything predating this service — can only come from elsewhere.
// This is a separate ingestion path from the webhook handler, but it writes the
// SAME rows with origin='backfill' and converges on the same reconciliation.
// Dedup is on (source, resource_id), so a transaction backfilled today and
// delivered by webhook tomorrow collapses to one row rather than being
// processed twice. See CLAUDE.md, "Historical data (backfill)".
//
// Two sources:
//
//	--from-snapshot DIR   replay a captured snapshot. No network, no credentials.
//	--live                permit real provider API calls (not yet implemented).
//
// The snapshot path is what the parity-through-Postgres run uses, which is how
// guardrail 1 is satisfied without a single live call.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/config"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/importer"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/snapshot"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/worker"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "backfill: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		fromSnapshot = flag.String("from-snapshot", "",
			"import a captured snapshot directory instead of calling any provider API")
		live = flag.Bool("live", false,
			"permit live provider API calls (required before any real credential is used)")
		dsn = flag.String("database-url", "",
			"Postgres connection string (default: $DATABASE_URL)")
		migrateFirst = flag.Bool("migrate", true, "apply pending migrations before importing")
		from         = flag.String("from", "",
			"PayPal window start, YYYY-MM-DD (--live only; defaults to 3 years back, "+
				"the Transaction Search limit)")
		to = flag.String("to", "",
			"PayPal window end, YYYY-MM-DD (--live only; defaults to today)")
		reconcile = flag.Bool("reconcile", true,
			"run a reconcile pass after importing, so a backfill is self-contained "+
				"and does not have to wait on a worker poll tick")
	)
	flag.Parse()

	fmt.Fprintf(os.Stderr, "ticketing-reconciliation-service backfill %s\n", version)

	if *fromSnapshot == "" && !*live {
		// Guardrail 4. Ticket Tailor has no sandbox, so a key in the environment
		// is a live key by construction — the gate cannot be delegated to
		// PAYPAL_SANDBOX.
		return errors.New("nothing to do: pass --from-snapshot DIR, or --live to " +
			"reach real provider APIs (see CLAUDE.md guardrail 4)")
	}

	url := *dsn
	if url == "" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		url = cfg.DatabaseURL
	}
	if url == "" {
		return errors.New("no database: pass --database-url or set DATABASE_URL")
	}

	// Ctrl-C should stop between batches rather than mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()

	if *migrateFirst {
		applied, err := st.Migrate(ctx)
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if len(applied) > 0 {
			fmt.Fprintf(os.Stderr, "applied migrations %v\n", applied)
		}
	}

	start := time.Now()

	var report ingest.Report
	if *fromSnapshot != "" {
		raw, err := snapshot.LoadRawJSON(*fromSnapshot)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "snapshot: %d resources from %s\n", raw.Total(), *fromSnapshot)

		report, err = ingest.ImportSnapshot(ctx, st, raw)
		if err != nil {
			return err
		}
	} else {
		var err error
		report, err = runLiveImport(ctx, st, *from, *to)
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "%s (%s)\n", report, time.Since(start).Round(time.Millisecond))

	if *reconcile {
		w := worker.New(st, worker.DefaultConfig(),
			slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

		processed, failed, err := w.DrainStageOne(ctx)
		if err != nil {
			return fmt.Errorf("validate: %w", err)
		}
		fmt.Fprintf(os.Stderr, "validated %d rows (%d failed)\n", processed, failed)

		res, err := w.Reconcile(ctx, time.Time{})
		if err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		fmt.Fprintf(os.Stderr, "reconciled %d resources, %d verdicts changed\n",
			res.Resources, res.Changed)
	}

	health, err := st.Health(ctx)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	fmt.Fprintf(os.Stderr, "stored by status: %v\n", health.ByStatus)
	fmt.Fprintf(os.Stderr, "verdicts: %v\n", health.ByReconStatus)

	return nil
}

// runLiveImport reaches the real provider APIs. Only called behind --live.
//
// Ticket Tailor is always a FULL RESYNC: CLAUDE.md is explicit that a
// created_at watermark misses refunds posted against old orders, and there is no
// way to detect that omission afterwards.
func runLiveImport(ctx context.Context, st *store.Store, from, to string) (ingest.Report, error) {
	var report ingest.Report

	cfg, err := config.Load("TT_API_KEY", "PAYPAL_CLIENT_ID", "PAYPAL_SECRET")
	if err != nil {
		return report, err
	}

	if !cfg.PayPal.Sandbox {
		fmt.Fprintln(os.Stderr,
			"WARNING: PAYPAL_SANDBOX is false — this will read SSG's live PayPal account")
	}
	// Ticket Tailor has no sandbox at all, so there is no equivalent warning to
	// suppress: any key reaching here is live.
	fmt.Fprintln(os.Stderr,
		"WARNING: Ticket Tailor has no sandbox; this reads a live account")

	ttStart := time.Now()

	ttClient := importer.NewTicketTailorClient(ticketTailorBaseURL, cfg.TicketTailor.APIKey)
	ttReport, err := importer.ImportTicketTailor(ctx, ttClient, st)
	if err != nil {
		return report, err
	}
	fmt.Fprintf(os.Stderr, "ticket tailor: %s (%s)\n",
		ttReport, time.Since(ttStart).Round(time.Millisecond))

	start, end, err := payPalWindow(from, to)
	if err != nil {
		return report, err
	}
	fmt.Fprintf(os.Stderr, "paypal window: %s to %s\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"))

	ppStart := time.Now()
	ppClient := importer.NewPayPalClient(cfg.PayPal.BaseURL(),
		cfg.PayPal.ClientID, cfg.PayPal.Secret)
	ppReport, err := importer.ImportPayPal(ctx, ppClient, st, start, end)
	if err != nil {
		return report, err
	}
	fmt.Fprintf(os.Stderr, "paypal: %s (%s)\n",
		ppReport, time.Since(ppStart).Round(time.Millisecond))

	report.Read = ttReport.Read + ppReport.Read
	report.Outcomes = map[store.Outcome]int{}
	for k, v := range ttReport.Outcomes {
		report.Outcomes[k] += v
	}
	for k, v := range ppReport.Outcomes {
		report.Outcomes[k] += v
	}
	return report, nil
}

const ticketTailorBaseURL = "https://api.tickettailor.com/v1"

// payPalWindow defaults to the widest range Transaction Search will serve.
func payPalWindow(from, to string) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	if to != "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--to: %w", err)
		}
		end = t
	}

	// Transaction Search only retains about three years.
	start := end.AddDate(-3, 0, 0)
	if from != "" {
		t, err := time.Parse("2006-01-02", from)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--from: %w", err)
		}
		start = t
	}

	if !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("--from must be before --to")
	}
	return start, end, nil
}
