// Command backfill imports historical data into the events table.
//
// Webhooks only fire forward from when a subscription is created, so history —
// including everything predating this service — can only come from elsewhere.
// This is a separate ingestion path from the webhook handler, but it writes the
// SAME rows with origin='backfill' and converges on the same reconciliation.
// Dedup is on (source, resource_id), so a transaction backfilled today and
// delivered by webhook tomorrow collapses to one row rather than being
// processed twice.
//
// Two sources:
//
//	--from-snapshot DIR   replay a captured snapshot. No network, no credentials.
//	--live                reach the real provider APIs.
//
// The snapshot path is what the parity-through-Postgres run uses, which
// proves the engine end to end without a single live call.
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
		// Reaching a live provider account must be a deliberate act, never a
		// default.
		return errors.New("nothing to do: pass --from-snapshot DIR, or --live to " +
			"reach real provider APIs")
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
// Ticket Tailor is always a FULL RESYNC — see docs/ARCHITECTURE.md, "Backfill
// is always a full resync" for why a created_at watermark isn't safe here.
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

	start, end, err := payPalWindow(time.Now().UTC(), from, to)
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
// now is injected rather than read internally so the default-range
// calculation is testable without depending on the wall clock.
func payPalWindow(now time.Time, from, to string) (time.Time, time.Time, error) {
	end := now
	if to != "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--to: %w", err)
		}
		end = t
	}

	// Transaction Search only retains about three years. fetchPage sends
	// this truncated to midnight UTC on the calendar date (start of day,
	// not the exact instant) — so "exactly 3 years before now" would
	// truncate to a time PayPal considers slightly MORE than 3 years back
	// whenever the request runs after midnight UTC, and gets rejected
	// ("Start Date provided is before the allowed range of 3 years").
	// Padding by a day guarantees the truncated value always lands after
	// PayPal's true boundary, regardless of what time of day this runs.
	start := end.AddDate(-3, 0, 1)
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
