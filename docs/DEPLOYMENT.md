# Deployment

The Compose stack in `compose.yml` is the production deployment target for a
small VPS — not a local dev tool. Local development runs `go run
./cmd/server` directly against a local Postgres (see `docs/ENVIRONMENT.md`);
nothing in `compose.yml` is needed for that.

## Why Compose, not Kubernetes

This service reconciles ticket sales for one amateur theatre group and will
be used by a handful of people. Kubernetes and Helm are out of scope — they
were an early assumption and are overkill for the load and the operational
budget. The target is Docker Compose on a small Hetzner VPS.

## Why Caddy, not nginx

Caddy issues and renews the TLS certificate automatically via Let's Encrypt
once DNS points at the host — no manual cert management. Both providers
(PayPal, Ticket Tailor) require HTTPS webhook endpoints, so TLS is not
optional, but it belongs to the proxy, not this service. That near-zero-config
automatic TLS is why Caddy was picked over nginx for a low-ops project.

## Image build and deploy

The app image is pulled, never built on the VPS — building on the box
couples deploys to it having a toolchain and a clean checkout.
`.github/workflows/ci.yml` builds and pushes
`ghcr.io/<owner>/ticketing-reconciliation-service` on every merge to `main`.
Routine deploy, once set up: `docker compose pull && docker compose up -d`.

CI stays deliberately simple: one job on `ubuntu-latest`, push+PR on `main`,
toolchain caching via the setup action, no matrix, no release job. The
image-push step is gated to `push` on `main` only, so a PR branch — which
has not been reviewed — never publishes an image anyone could pull.

## Observability topology

OpenTelemetry runs as an in-process SDK, not a separate container — only
Prometheus and Grafana are actual sibling containers. The app exposes
Prometheus-formatted metrics on its own internal-only port (`METRICS_PORT`,
default 9464 — see `internal/metrics` and `cmd/server/main.go`), which is
**not** the port `proxy` reaches: the Caddyfile's site block proxies
everything on the app's main port to the internet with no path matching, so
`/metrics` has to live somewhere Caddy never sees. `prometheus` scrapes that
port over the internal Compose network; `grafana` queries `prometheus` the
same way and is the only one of the two actually reachable externally (via
its own Caddy site block), since it's the human-facing UI.

## One-time setup on the VPS, before the first `up`

1. `cp .env.example .env`, then fill in production secrets. `chmod 600` it.
   Never commit this file — see `docs/ENVIRONMENT.md`.
2. Point DNS at this host (both the main domain and `grafana.<domain>`),
   then edit the Caddyfile's placeholder domains.
3. After the first `up` brings Postgres online, provision the read-only API
   role once (not automated — see `docs/ENVIRONMENT.md`, "The query API's
   database role"):
   ```bash
   docker compose exec -T postgres psql -U reconciler -d reconciliation \
     -v api_password="$(openssl rand -base64 32)" -v DBNAME=reconciliation \
     -f /dev/stdin < deploy/readonly-role.sql
   ```
   (`-T` disables TTY allocation so piped stdin actually reaches `psql`.) Put
   the resulting connection string in `.env` as `API_DATABASE_URL`, then
   `docker compose up -d` again so the app picks it up.
4. Backups (see `docs/ENVIRONMENT.md`, "Postgres backups", and
   `docs/BACKUP.md` for the verification record). Not a service in
   `compose.yml` — `deploy/backup.sh` is a host-cron script, not a
   container, because it only needs to run once a night. Provision the
   restic repository once (`restic init`), install the crontab line from
   that script's header, then run `make restore-check` and record the
   result.

## Postgres and network isolation

`postgres` gets a named volume, not a bind mount into the repo, and is not
published on a host port — it holds buyer payment data and must not be
reachable from the internet at all, only from other Compose services. The
same isolation applies to `prometheus`: nothing needs to reach it directly,
Grafana queries it over the internal network. `grafana` is the one exception
— its UI is meant to be used from outside the VPS, gated by its own login
rather than network isolation.
