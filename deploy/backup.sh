#!/usr/bin/env bash
# Nightly Postgres backup: pg_dump piped straight into restic, so an
# unencrypted dump never touches disk on the VPS. See docs/ENVIRONMENT.md,
# "Postgres backups" for why this matters — the reconciliation history and
# its audit trail are not otherwise reconstructible.
#
# Deliberately NOT a Compose service. This only needs to run once a night;
# a permanently-running container for that is new infrastructure for no
# benefit. Run this from the VPS host, e.g. via cron:
#
#   0 3 * * * cd /opt/reconciliation && set -a && . .env && set +a && \
#     ./deploy/backup.sh >> /var/log/reconciliation-backup.log 2>&1
#
# cron does not source .env itself, hence the `set -a && . .env` above —
# without it RESTIC_REPOSITORY/RESTIC_PASSWORD are unset and this script
# fails fast rather than silently backing up nothing.
#
# Requires: restic and the docker compose plugin on the VPS host, and an SSH
# key already configured (via ~/.ssh/config) for non-interactive auth to the
# Storage Box host named in RESTIC_REPOSITORY. See docs/ENVIRONMENT.md.
set -euo pipefail

: "${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY — see docs/ENVIRONMENT.md}"
: "${RESTIC_PASSWORD:?set RESTIC_PASSWORD — see docs/ENVIRONMENT.md}"

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# -Fc (custom format) rather than plain SQL: seekable, supports parallel
# pg_restore, and pg_restore can target a fresh empty database directly
# without a manual dropdb/createdb dance during a real recovery.
# --no-owner: the restore target's role name will not always be "reconciler"
# (e.g. a scratch database during deploy/restore-check.sh), so ownership
# statements in the dump would otherwise fail on restore.
docker compose exec -T postgres \
  pg_dump -U reconciler -Fc --no-owner reconciliation \
  | restic backup --stdin --stdin-filename reconciliation.dump --tag reconciliation

# Retention is restic's job, not ours to hand-roll. Applied every run so a
# missed prune on one night does not compound.
restic forget --keep-daily 14 --keep-weekly 8 --keep-monthly 12 \
  --tag reconciliation --prune
