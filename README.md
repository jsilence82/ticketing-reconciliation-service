# Ticketing Reconciliation Service

A Go service that reconciles ticket sales against payment processor records —
built for the **Schauspielgruppe des Anglistischen Seminars (SSG)**, an
amateur theatre group that sells tickets through [Ticket Tailor](https://www.tickettailor.com/)
and takes payment through [PayPal](https://www.paypal.com/).

## The problem this solves

After every production run, someone has to answer a question that sounds
simple and isn't: **did the money PayPal actually paid out match the
tickets Ticket Tailor says were sold?**

That means, for each show:

- Matching every Ticket Tailor order against the PayPal transaction that
  paid for it.
- Catching **transferred tickets** — voided in Ticket Tailor, but the
  PayPal charge is still valid, so the money is real even though the ticket
  isn't.
- Catching **PayPal transactions with no matching order** — money that came
  in with no ticket to explain it.
- Getting the **fees** right. PayPal's reported fee isn't always final at
  the moment of charge (currency conversion, partial refunds, chargeback
  adjustments), and gross/fee/net has to reconcile exactly per production,
  not approximately.

This service ingests both providers' data continuously, does that matching
as new events arrive, and stores the verdict — matched, unmatched,
transferred, or pending — against every transaction and ticket. A
consumer (a report, a script, a person with a spreadsheet) then pulls the
reconciled data back out and builds the actual gross/fees/net figures per
production from it.

## What it does — and deliberately doesn't do

- **Ingests** Ticket Tailor and PayPal webhooks in real time, plus a
  REST-based backfill path for historical data predating the webhook
  subscriptions.
- **Deduplicates** on the underlying resource, not the delivery: a
  transaction backfilled today and delivered by webhook tomorrow collapses
  to one row, and retried webhook deliveries never double-count.
- **Reconciles**: matches every PayPal transaction against Ticket Tailor
  orders by transaction ID, flags transferred tickets, and surfaces
  unmatched PayPal transactions instead of silently dropping them.
- **Serves** the result through a read-only query API, filterable by
  source, status, reconciliation verdict, and date range.

What it does *not* do is generate the gross/fees/net report itself. That's
a deliberate line: this service is the source of truth for the underlying
facts, and shaping those facts into a per-production Totals report is a
consumer-side concern — computed on demand from raw reconciled data, not
cached or pre-aggregated here. That keeps there being exactly one source of
truth, with no report-shaped view that can silently drift from it.

## Architecture, at a glance

```
webhook  → verify signature → ┐
                              ├→ idempotency check → persist → worker → Postgres → read-only query API
backfill → REST paging ──────→┘
```

Both ingestion paths converge on the same schema and the same reconciliation
worker from the idempotency check onward — a transaction that arrives by
backfill and later by webhook is the same stored row either way. The worker
itself is two stages: a per-row validation pass, and a dataset-wide
reconcile pass that re-evaluates the matching rule any time new data
settles. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the design
decisions behind each piece — why there's no projection layer, why the API's
read-only guarantee is enforced structurally rather than by convention, how
each provider's webhook signature is verified, and how buyer PII is kept out
of storage entirely.

## Getting started

```bash
git clone <this repo>
cd ticketing-reconciliation-service
cp .env.example .env    # fill in PayPal sandbox + a personal Ticket Tailor account
make check               # fmt + vet + lint + test
make build               # binaries into bin/
```

Full environment variable reference, local Postgres setup, and the query
API's read-only database role are documented in
[`docs/ENVIRONMENT.md`](docs/ENVIRONMENT.md) — start there before running
the server for the first time.

Running the server locally, against a local Postgres, once `.env` is filled in:

```bash
set -a; source .env; set +a
go run ./cmd/server --insecure-allow-writable-api
```

There's no implicit `.env` loading in the service itself, in local
development or in production — `source`ing it into the shell (or letting the
container runtime read it via `env_file:`) is the actual mechanism, in both
places, by design. `--insecure-allow-writable-api` is a development-only
convenience that skips provisioning a separate read-only database role; see
`docs/ENVIRONMENT.md` for the role you'd provision for anything resembling
production.

## Query API

Read-only, authenticated with a per-consumer API key.

```
GET /events?source=paypal&recon_status=unmatched
GET /events/{id}
GET /healthz
```

Money crosses the API boundary as a decimal string, a minor-unit integer,
and a currency code — never a JSON float, so a consumer's own arithmetic
doesn't inherit a parser's rounding error:

```json
{
  "recon_status": "matched",
  "amounts": {
    "currency": "EUR",
    "gross": { "decimal": "23.00", "minor": 2300 },
    "fee":   { "decimal": "-1.08", "minor": -108 },
    "net":   { "decimal": "21.92", "minor": 2192 }
  }
}
```

A report generator (or a person) builds the per-production Totals — gross,
fees, net per night — by pulling every event for a show through this API and
summing client-side. That's the intended shape of the "generate the report"
step this project exists to support.

## Deployment

Docker Compose on a single small VPS — deliberately not Kubernetes, given
the scale of one amateur theatre group's ticket sales. The stack is the
app, Postgres, a Caddy reverse proxy (automatic TLS), and a
Prometheus/Grafana observability pair. See
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for the full setup runbook, and
[`docs/BACKUP.md`](docs/BACKUP.md) for the Postgres backup and
restore-verification process — the one piece of operational rigour this
setup genuinely needs, since reconciliation history isn't reconstructible
from webhooks alone.

## Tech stack

- **Go**, `net/http` with Go 1.22+ routing — no framework.
- **PostgreSQL**, accessed via `pgx` with hand-written SQL (no ORM).
- **Docker Compose** for deployment; **Caddy** for TLS termination.
- **OpenTelemetry → Prometheus → Grafana** for observability.
- **GitHub Actions** for CI: build, lint, test, and image publish on merge
  to `main`.

## Documentation

| Doc | Covers |
|---|---|
| [`docs/ENVIRONMENT.md`](docs/ENVIRONMENT.md) | Every environment variable, local Postgres setup, API keys, secrets handling |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Design rationale: webhook verification, PII handling, storage shape, why read-only is enforced structurally |
| [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) | The Compose stack, VPS setup runbook, why Compose/Caddy over the alternatives |
| [`docs/BACKUP.md`](docs/BACKUP.md) | Postgres backup mechanism and restore-verification record |

## Project status

Core reconciliation (webhook + backfill ingestion, the two-stage worker, the
matching rule, the read-only query API) is implemented and running. An
optional MCP server exposing the same read-only data to LLM tooling is
planned but not yet started. This service's output is validated against the
group's prior reconciliation process before it is trusted with a live
webhook subscription — see `docs/ARCHITECTURE.md` for how each provider's
webhook path was proven against real sandbox/live deliveries before being
turned on.
