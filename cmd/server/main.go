// Command server runs the reconciliation worker and the read-only query API.
//
// Webhook ingestion is P4 — CLAUDE.md guardrail 1 forbids pointing this service
// at live webhook subscriptions until its output has been diffed against the
// dashboard, which `make parity-db` does.
//
// # Two database connections, on purpose
//
// The worker writes; the API must not. They therefore use separate pools with
// separate roles, and the API's is verified read-only at startup rather than
// trusted. See deploy/readonly-role.sql.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/api"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/config"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/importer"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/metrics"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/webhook"
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
		once         = flag.Bool("once", false, "run a single worker tick and exit")
		noAPI        = flag.Bool("no-api", false, "run only the worker, without the HTTP listener")
		insecureAPI  = flag.Bool("insecure-allow-writable-api", false,
			"skip the read-only proof for the API's database role (development only)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("starting", "version", version)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	url := *dsn
	if url == "" {
		url = cfg.DatabaseURL
	}
	if url == "" {
		return errors.New("no database: pass --database-url or set DATABASE_URL")
	}

	// SIGTERM is how Compose stops a container. Cancelling lets the worker
	// finish its tick and the listener drain, rather than being killed
	// mid-transaction.
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

	shutdownMetrics, err := metrics.Init(ctx)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownMetrics(sctx); err != nil {
			log.Error("metrics shutdown", "err", err)
		}
	}()
	if err := metrics.RegisterHealthGauges(st); err != nil {
		return fmt.Errorf("metrics: health gauges: %w", err)
	}

	workerCfg := worker.DefaultConfig()
	if *pollInterval > 0 {
		workerCfg.PollInterval = *pollInterval
	}
	w := worker.New(st, workerCfg, log)

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

	if *noAPI {
		return w.Run(ctx)
	}

	srv, apiStore, err := buildAPI(ctx, cfg, url, log, *insecureAPI)
	if err != nil {
		return err
	}
	if apiStore != nil {
		defer apiStore.Close()
	}

	mux := srv.Routes()

	// The webhook route writes through st (the worker's role), never through
	// the API's read-only pool — guardrail 3 only constrains the query API.
	// Optional for now: TT_WEBHOOK_SECRET is still being pinned against real
	// captured deliveries (see CLAUDE.md, P4), so a missing secret disables the
	// route rather than failing startup.
	if cfg.TicketTailor.WebhookSecret == "" {
		log.Warn("TT_WEBHOOK_SECRET unset: /webhooks/tickettailor is disabled")
	} else {
		ttHandler, err := webhook.NewTicketTailorHandler(cfg.TicketTailor.WebhookSecret, st, log)
		if err != nil {
			return fmt.Errorf("ticket tailor webhook handler: %w", err)
		}
		mux.Handle("POST /webhooks/tickettailor", ttHandler)
		log.Info("ticket tailor webhook route enabled", "path", "/webhooks/tickettailor")
	}

	// Same optional-gate pattern as Ticket Tailor above: all three of
	// PAYPAL_CLIENT_ID/PAYPAL_SECRET/PAYPAL_WEBHOOK_ID are needed before the
	// route can verify anything, and PAYPAL_WEBHOOK_ID in particular does not
	// exist until a webhook subscription has been created against this
	// server's own URL — a chicken-and-egg the optional gate resolves by
	// just starting without the route rather than refusing to boot.
	switch {
	case cfg.PayPal.ClientID == "" || cfg.PayPal.Secret == "":
		log.Warn("PAYPAL_CLIENT_ID/PAYPAL_SECRET unset: /webhooks/paypal is disabled")
	case cfg.PayPal.WebhookID == "":
		log.Warn("PAYPAL_WEBHOOK_ID unset: /webhooks/paypal is disabled")
	default:
		ppClient := importer.NewPayPalClient(cfg.PayPal.BaseURL(), cfg.PayPal.ClientID, cfg.PayPal.Secret)
		ppHandler, err := webhook.NewPayPalHandler(ppClient, cfg.PayPal.WebhookID, st, log)
		if err != nil {
			return fmt.Errorf("paypal webhook handler: %w", err)
		}
		mux.Handle("POST /webhooks/paypal", ppHandler)
		log.Info("paypal webhook route enabled", "path", "/webhooks/paypal", "sandbox", cfg.PayPal.Sandbox)
	}

	httpSrv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		// otelhttp wraps the WHOLE mux, not individual routes, so it reads
		// Go 1.22+ ServeMux's r.Pattern (e.g. "/events/{id}") for the
		// http.route label rather than the raw request path — one label per
		// route, not one per UUID.
		Handler:           otelhttp.NewHandler(mux, "api"),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Deliberately a SEPARATE server on its own, unpublished port rather than
	// a route on mux: the Caddyfile's site block proxies everything on
	// cfg.Port to the internet with no path matching, so /metrics would be
	// public if it lived there. Isolated the same way postgres is isolated —
	// reachable only from the prometheus sibling container, over the Compose
	// network (see compose.yml).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.MetricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		log.Info("metrics listening", "addr", metricsSrv.Addr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdown); err != nil {
		log.Error("http shutdown", "err", err)
	}
	if err := metricsSrv.Shutdown(shutdown); err != nil {
		log.Error("metrics http shutdown", "err", err)
	}
	log.Info("stopped")
	return nil
}

// buildAPI wires the query API on its own read-only pool.
//
// Returns the store so the caller can close it.
func buildAPI(
	ctx context.Context, cfg *config.Config, writeURL string, log *slog.Logger, insecure bool,
) (*api.Server, *store.Store, error) {
	consumers, err := api.ParseConsumers(cfg.APIKeys)
	if err != nil {
		return nil, nil, err
	}
	if len(consumers) == 0 {
		// Reconciliation data is buyer-payment mapping. A service that serves it
		// to anyone because its configuration is missing is the failure worth
		// engineering against, so this is fatal rather than a warning.
		return nil, nil, errors.New(
			"API_KEYS is empty: the query API would have no way to authenticate " +
				"anyone. Set API_KEYS, or pass --no-api to run the worker alone")
	}

	// A dedicated read-only role is the enforcement guardrail 3 asks for.
	// Sharing the worker's writable role is a development convenience and has to
	// be asked for explicitly.
	readOnly := cfg.APIDatabaseURL != ""
	target := cfg.APIDatabaseURL

	if !readOnly {
		if !insecure {
			return nil, nil, errors.New(
				"API_DATABASE_URL is unset. Guardrail 3 requires the query API to " +
					"use a SELECT-only role (see deploy/readonly-role.sql). Pass " +
					"--insecure-allow-writable-api to override in development")
		}
		log.Warn("API is sharing the worker's writable database role; " +
			"guardrail 3 is NOT enforced in this process")
		target = writeURL
	}

	st, err := store.New(ctx, target)
	if err != nil {
		return nil, nil, fmt.Errorf("api database: %w", err)
	}

	srv := api.New(st, consumers, log)

	if readOnly {
		// Prove it rather than trust it. A role that can write is a boot failure.
		if err := srv.VerifyReadOnly(ctx); err != nil {
			st.Close()
			return nil, nil, err
		}
	}

	log.Info("query API configured", "consumers", len(consumers), "read_only", readOnly)
	return srv, st, nil
}
