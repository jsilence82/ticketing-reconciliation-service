// Command server runs the reconciliation worker, and eventually the read-only
// query API.
//
// The worker is live as of P2.5. The HTTP surface is P3, and webhook ingestion
// is P4 — CLAUDE.md guardrail 1 forbids pointing this service at live webhook
// subscriptions until its output has been diffed against the dashboard, which
// `make parity-db` now does.
//
// Running this against a database that a backfill has populated is enough to
// keep verdicts current: the worker validates new rows and re-reconciles when
// the input changes.
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

	"github.com/jsilence82/ticketing-reconciliation-service/internal/config"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/worker"
)

var version = "dev"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dsn          = flag.String("database-url", "", "Postgres connection string (default: $DATABASE_URL)")
		migrateFirst = flag.Bool("migrate", true, "apply pending migrations at startup")
		pollInterval = flag.Duration("poll", 0, "worker poll interval (default: 30s)")
		once         = flag.Bool("once", false, "run a single tick and exit, instead of looping")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("starting", "version", version)

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

	// SIGTERM is how Compose stops a container. Cancelling here lets the worker
	// finish its current tick rather than being killed mid-transaction; rows it
	// had claimed would otherwise wait for the reaper.
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
			log.Info("applied migrations", "versions", applied)
		}
	}

	cfg := worker.DefaultConfig()
	if *pollInterval > 0 {
		cfg.PollInterval = *pollInterval
	}
	w := worker.New(st, cfg, log)

	if *once {
		res, err := w.Tick(ctx)
		if err != nil {
			return err
		}
		log.Info("tick complete",
			"processed", res.Processed, "failed", res.Failed,
			"released", res.Released, "reconciled", res.Reconcile.Ran,
			"verdicts_changed", res.Reconcile.Changed, "skipped", res.ReconcileSkipped)
		return nil
	}

	// No HTTP listener yet, so nothing to shut down beyond the worker loop.
	// P3 adds the query API alongside this.
	return w.Run(ctx)
}
