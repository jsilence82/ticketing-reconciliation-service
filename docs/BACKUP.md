# Postgres Backup — Verification Record

Status: **mechanism implemented and verified manually; the real Storage Box
path and the scripts themselves are PENDING their first live run.**

This is the record for backing up Postgres on a schedule and confirming a
restore actually works — the one piece of operational rigour a Compose
deployment with no orchestrator genuinely needs. It is a proof that gets run
and recorded, not a continuous CI check.

## What is proven so far (2026-08-02)

The backup/restore mechanics were validated by hand, directly against a
local Postgres and a local restic repository (`RESTIC_REPOSITORY=local:...`,
not SFTP), before `deploy/backup.sh` and `deploy/restore-check.sh` were
written:

```
pg_dump -Fc --no-owner <db> | restic backup --stdin --stdin-filename reconciliation.dump --tag reconciliation
restic dump latest reconciliation.dump --tag reconciliation > dump.file
pg_restore --no-owner -d <scratch db> dump.file
```

Round-tripped a two-row table through this exact sequence: both rows came
back byte-identical, `restic forget --keep-daily 14 --keep-weekly 8
--keep-monthly 12 --tag reconciliation --prune` correctly applied all three
retention reasons to the single snapshot. This confirms the *approach* is
sound — restic's stdin backup/dump primitives round-trip a `pg_dump -Fc`
correctly, and the retention policy behaves as documented.

## What is NOT yet proven

- **`deploy/backup.sh` and `deploy/restore-check.sh` as actual scripts,
  end-to-end.** The commands above were run by hand, not through the scripts
  themselves, and `restore-check.sh`'s `docker run` step has not been
  exercised at all — the environment used to build this had no Docker daemon
  available. `bash -n` and `shellcheck` both pass on both scripts, but that
  only proves they parse, not that they behave.
- **The real SFTP backend against an actual Hetzner Storage Box.** Only
  `local:` was tested. `RESTIC_REPOSITORY=sftp:...` needs a real Storage Box
  account, an SSH key installed for non-interactive auth (see
  `docs/ENVIRONMENT.md`, "Postgres backups"), and `restic init` run once
  against it before the first real backup.
- **The cron schedule.** The crontab line in `deploy/backup.sh`'s header is
  documented, not installed — nothing on this machine can touch the VPS's
  actual crontab.

## To close this out

Once real Storage Box credentials exist and the VPS is provisioned:

1. `restic init` against the real `RESTIC_REPOSITORY`.
2. Run `deploy/backup.sh` once by hand against the real `postgres` compose
   service (not a scratch database) and confirm `restic snapshots` shows it.
3. Run `deploy/restore-check.sh` and confirm it reports `restore check OK`
   with a plausible row count and the expected migration versions.
4. Install the crontab line from `deploy/backup.sh`'s header.
5. Update the Status line at the top of this file to record that date and
   the `restore check OK` output with exact figures, rather than leaving it
   at "should work."
