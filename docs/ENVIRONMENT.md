# Environment

All configuration comes from the environment. **Secrets have no defaults** — a
missing credential is a startup error, never a silently-substituted development
value. That fail-closed rule is what keeps a misconfigured deployment from
silently reaching a live account instead of refusing to start.

Loaded by [internal/config](../internal/config/config.go). Each command declares
only the variables it actually needs, so `parity` never demands PayPal
credentials it will not use.

## Variables

| Variable | Required by | Default | Notes |
|---|---|---|---|
| `PORT` | — | `8080` | Rejected if not a number in 1–65535 |
| `DATABASE_URL` | `backfill`, `server`, integration tests | none | Postgres connection string for the **worker**, which writes. Unset makes DB-backed tests **skip**. |
| `API_DATABASE_URL` | `server` | none | Connection string for the **query API**. Must be a SELECT-only role; the server proves it at startup and refuses to serve otherwise. |
| `API_KEYS` | `server` | none | `name:key,name:key`. One key per consumer. Empty means the API cannot authenticate anyone, which is a startup error rather than open access. |
| `TT_API_KEY` | `backfill` (P2+) | none | Ticket Tailor API key; sent as HTTP Basic username with an empty password |
| `TT_WEBHOOK_SECRET` | `server` (P4+) | none | HMAC-SHA256 secret for `Tickettailor-Webhook-Signature`. Optional at startup: unset disables `POST /webhooks/tickettailor` with a warning log rather than failing boot, so the rest of the service can still run without it configured |
| `PAYPAL_CLIENT_ID` | `backfill` (P2+) | none | OAuth2 client id |
| `PAYPAL_SECRET` | `backfill` (P2+) | none | OAuth2 secret |
| `PAYPAL_WEBHOOK_ID` | `server` (P4+) | none | Required to verify webhook transmission signatures |
| `PAYPAL_SANDBOX` | — | **`true`** | See below |
| `SSG_PARITY_DATA` | `parity` (P1+) | none | Path to the out-of-tree historical dataset |
| `METRICS_PORT` | `server` (P5+) | `9464` | Internal-only Prometheus scrape port (see `internal/metrics`). Not published by compose.yml and not proxied by the Caddyfile's main site block — only the `prometheus` sibling container reaches it, over the Compose network. |
| `RESTIC_REPOSITORY` | `deploy/backup.sh`, `deploy/restore-check.sh` | none | The restic repository URL, e.g. `sftp:u123456@u123456.your-storagebox.de:/backups/reconciliation`. Consumed by the host-run backup scripts, not by the Go service. |
| `RESTIC_PASSWORD` | `deploy/backup.sh`, `deploy/restore-check.sh` | none | Encrypts the restic repository. Losing it means losing every backup — store it somewhere that survives losing the VPS, not only in `.env` on the VPS itself. |

## `PAYPAL_SANDBOX` defaults to true

Development must opt **in** to production, never out of it. Leaving the variable
unset, empty, or misspelled keeps sandbox on; only an explicitly parseable false
value (`false`, `0`) selects the live API. An unparseable value is a startup
error rather than a silent fallback — "silently defaulted to live" is exactly
the failure mode this is designed to prevent.

## `SSG_PARITY_DATA` is deliberately outside the repo

The parity harness needs real historical data to validate against, but that
data is a live production export and contains buyer PII, which must never
exist inside this tree in any form. It lives elsewhere on the machine and is
referenced by absolute path:

```bash
export SSG_PARITY_DATA="$HOME/ssg-parity-data"
```

When unset, parity tests **skip** rather than fail, so a clean checkout stays
green in CI.

Do not point this at a path inside the repository, and do not commit a `.env`
that does.

## Postgres for the integration tests

Tests that need a database skip when `DATABASE_URL` is unset, so a clean
checkout stays green. That is convenient and also a hazard: a test that
habitually skips is a test that rots. Two guards exist —
`make test-integration` **fails** rather than skips when the DSN is missing, and
the test helper hard-fails if `CI` is set but the DSN is not, so a broken CI
services block cannot green-light everything.

Local setup, no Docker required:

```bash
brew install postgresql@17
brew services start postgresql@17
export PATH="$(brew --prefix)/opt/postgresql@17/bin:$PATH"   # keg-only
createdb trs_test

export DATABASE_URL='postgres://localhost/trs_test?sslmode=disable'
make test-integration
```

Each test creates its own throwaway schema (`trs_test_<random>`) and drops it on
cleanup, so `go test -p N` works against this one database with no truncation
dance between packages.

Pinned to Postgres 17 — the same major CI runs and the VPS will run. The schema
relies on `gen_random_uuid()` being core, which needs PG13+; no `pgcrypto`
extension is required.

## Local development

```bash
cp .env.example .env    # then fill in sandbox credentials
make check              # fmt + vet + lint + test
make build              # binaries into bin/
```

`.env` is gitignored. There is no `.env` loading in the service itself — export
the variables in your shell, or let the container runtime read the file
(`docker run --env-file`, or compose's `env_file:`). That keeps the production
path identical to the local one: in both cases the process just reads its
environment.

## The query API's database role

No endpoint may mutate reconciliation state, and that is enforced by the
database, not by handler discipline: the API connects as a SELECT-only
role, and `cmd/server` **proves** it can't write by attempting an insert inside a
rolled-back transaction at startup. A role that succeeds is a boot failure.

Provision it once per environment:

```bash
psql -d reconciliation -v api_password="$(openssl rand -base64 32)" \
     -f deploy/readonly-role.sql
export API_DATABASE_URL='postgres://reconciliation_api:...@host/reconciliation'
```

The `ALTER DEFAULT PRIVILEGES` line in that script is the one most easily
forgotten: without it, the next migration hands the API write access to whatever
table it creates.

For local development only, `--insecure-allow-writable-api` lets the API share
the worker's role. It logs a warning every start, and it must never be used
anywhere real.

## API keys

`API_KEYS` is `name:key,name:key`. Per-consumer rather than one shared secret,
because reconciliation data is buyer-payment mapping restricted to whoever
handles the organization's finances — and a shared secret cannot be revoked
for one caller.

Keys must be at least 24 characters; shorter ones are refused at startup rather
than discovered in an access log. Generate with `openssl rand -base64 32`.

Requests carry the key as `Authorization: Bearer <key>` or `X-API-Key: <key>`.
`/healthz` is deliberately unauthenticated — the reverse proxy and uptime checks
need it, and it exposes counts rather than any buyer or payment data.

## Grafana admin password

`GF_SECURITY_ADMIN_PASSWORD` is consumed directly by the `grafana` container in
compose.yml, not by `internal/config` — the Go service never talks to Grafana.
It has the same "secrets have no defaults" treatment as `POSTGRES_PASSWORD`:
compose.yml's `${GF_SECURITY_ADMIN_PASSWORD:?...}` syntax fails `docker compose
up` outright rather than falling back to Grafana's own `admin`/`admin` default,
which would otherwise sit on a container reachable from the internet (see
Caddyfile). Generate it the same way `API_KEYS` recommends:

```bash
openssl rand -base64 32
```

## Postgres backups

Backing up Postgres on a schedule and verifying a restore at least once is
the one piece of operational rigour a Compose deployment with no
orchestrator genuinely needs, because the reconciliation history (and its
audit trail: delivery timing, `signature_verified_at`, retry/dead-letter
history) is not otherwise reconstructible.

`deploy/backup.sh` pipes `pg_dump` straight into
[restic](https://restic.net/) — no unencrypted dump ever touches disk — and
prunes to a `--keep-daily 14 --keep-weekly 8 --keep-monthly 12` retention
policy on every run. Deliberately a host-cron script, not a Compose service:
this only needs to run once a night, so a permanently-running container
would be new infrastructure for no benefit. See the script's own header for
the exact crontab line.

Target: a Hetzner Storage Box over restic's native SFTP backend
(`RESTIC_REPOSITORY=sftp:user@host:/path`), matching the same Hetzner family
the VPS itself runs on. Requires an SSH key configured for
non-interactive auth to the Storage Box — add a `Host` entry to the VPS's
`~/.ssh/config` with `IdentityFile` pointing at a dedicated key, rather than
relying on restic's default SSH lookup finding the right one incidentally.
Provision the repository once:

```bash
export RESTIC_REPOSITORY='sftp:u123456@u123456.your-storagebox.de:/backups/reconciliation'
export RESTIC_PASSWORD="$(openssl rand -base64 32)"   # save this — see the table above
restic init
```

`deploy/restore-check.sh` proves the "verify a restore at least once"
requirement is an actual fact, not an assumption: it restores the latest
snapshot into a throwaway `docker run` Postgres container (never the real
`postgres` compose service) and sanity-checks row counts and applied
migrations. It is meant to be run and its result recorded (see
`docs/BACKUP.md`'s template), then re-run after any change to `backup.sh` or
the schema — a deliberate, occasional act, not something wired into CI as a
continuous check.

## Production secrets

The service runs as a single Docker container on a VPS (see
`docs/DEPLOYMENT.md`). There is no orchestrator, so there is no ConfigMap, no
`envFrom`, and no secrets operator — secrets come from the container
environment:

- An `.env` file on the VPS, owned by the deploy user with `chmod 600`, passed via
  `--env-file` or compose's `env_file:`. It must sit outside any checkout.
- Or the VPS provider's own secret store, if it has one.

Rules that hold either way:

- **Never bake credentials into the image.** The `Dockerfile` takes no
  credential build args, and image layers are readable by anyone who can pull it.
- **Never commit a populated `.env`.** `.gitignore` covers `.env` and `.env.*`
  while allowing `.env.example`; keep it that way.
- **Rotate `PAYPAL_SECRET` and `TT_API_KEY` if they ever reach a shell history,
  a CI log, or a paste.** Treat exposure as compromise rather than judging
  likelihood.
- `PAYPAL_SANDBOX` must be explicitly `false` in production and nowhere else.
  Development uses sandbox and a personal Ticket Tailor account only —
  production credentials must never be used outside deployment.
