#!/usr/bin/env bash
# Restores the latest backup.sh snapshot into a throwaway Postgres container
# and runs a sanity check, so "verify a restore at least once" (CLAUDE.md,
# Deployment) is a proven fact rather than an assumption. Modeled on
# docs/PARITY.md: this is meant to be run, its result recorded in
# docs/BACKUP.md, and re-run again after any change to backup.sh or the
# schema — not wired into CI as a continuous check, the same reasoning that
# keeps make parity a deliberate act rather than a pre-commit hook.
#
# Talks only to a disposable `docker run` container, never to the real
# `postgres` compose service — a restore check must not be able to touch
# production data even by accident.
set -euo pipefail

: "${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY — see docs/ENVIRONMENT.md}"
: "${RESTIC_PASSWORD:?set RESTIC_PASSWORD — see docs/ENVIRONMENT.md}"

TMP="$(mktemp -d)"
CONTAINER="reconciliation-restore-check-$$"

cleanup() {
	docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT

echo "==> dumping latest snapshot from $RESTIC_REPOSITORY"
restic dump latest reconciliation.dump --tag reconciliation > "$TMP/reconciliation.dump"

echo "==> starting a throwaway Postgres 17 (matches compose.yml's pinned version)"
docker run -d --name "$CONTAINER" \
	-e POSTGRES_PASSWORD=restore-check \
	-e POSTGRES_DB=reconciliation \
	postgres:17 >/dev/null

for _ in $(seq 1 30); do
	if docker exec "$CONTAINER" pg_isready -U postgres -d reconciliation >/dev/null 2>&1; then
		break
	fi
	sleep 1
done
if ! docker exec "$CONTAINER" pg_isready -U postgres -d reconciliation >/dev/null 2>&1; then
	echo "restore check FAILED: throwaway Postgres never became ready" >&2
	exit 1
fi

echo "==> restoring"
docker cp "$TMP/reconciliation.dump" "$CONTAINER:/tmp/reconciliation.dump"
docker exec "$CONTAINER" pg_restore -U postgres --no-owner -d reconciliation /tmp/reconciliation.dump

echo "==> sanity check"
COUNT="$(docker exec "$CONTAINER" psql -U postgres -d reconciliation -tAc 'SELECT count(*) FROM events;')"
VERSIONS="$(docker exec "$CONTAINER" psql -U postgres -d reconciliation -tAc 'SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations;' 2>/dev/null || echo "unknown")"

echo "restore check OK: events rows=$COUNT, migrations applied=[$VERSIONS]"
echo "record this result in docs/BACKUP.md — see that file's template"
