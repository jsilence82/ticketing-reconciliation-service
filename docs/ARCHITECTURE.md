# Architecture Notes

Design rationale for decisions that would otherwise only exist in commit
history or in someone's memory — collected here as the durable record. Code
comments carry the load-bearing fact itself; this doc carries the fuller
history and reasoning behind it.

## Webhook signature verification

### Ticket Tailor (`internal/webhook/tickettailor.go`)

Confirmed against Ticket Tailor's own webhook security documentation and
Python sample code (2026-08-01):

- The `Tickettailor-Webhook-Signature` header is two comma-separated
  `key=value` parts, timestamp first, signature second. Ticket Tailor's own
  sample code parses them **positionally** (`split(',')` then
  `split('=')[1]`) rather than by key name, so `VerifyTicketTailorSignature`
  does the same rather than assuming the literal key letters.
- The signed message is the timestamp **as a string**, concatenated directly
  with the raw request body bytes — no separator, no delimiter. This is not
  the Stripe-style `timestamp + "." + body` convention that seemed likely
  before the real docs were available; it was a wrong guess, corrected once
  the actual documentation was read.
- The envelope (`ttWebhookEnvelope`) wraps `id`/`created_at`/`event`/
  `resource_url`/`payload`. `id`/`created_at`/`event`/`resource_url` describe
  the *delivery*; `payload` is the changed resource in the same shape the
  REST API already returns it in, which is why it's handed to
  `ingest.FromTicketTailor` unchanged rather than needing a separate
  webhook-specific decoder. The envelope's own `id` (e.g. `wh_15`) is the
  delivery's identifier, not the resource's — it maps to
  `EventRecord.WebhookNotificationID`, kept only for debugging delivery-level
  duplication. The resource's own id inside `payload` is what `Upsert`
  actually dedupes on.

**Why the 5-minute staleness check never rejects.** Ticket Tailor's own docs
recommend rejecting deliveries with a timestamp older than 5 minutes. This
service deliberately does not: Ticket Tailor retries failed deliveries for up
to 72 hours, so a delivery that first succeeds on a later retry would have a
timestamp far older than 5 minutes and would be rejected forever, eventually
disabling the subscription. `staleAfter`/`logIfStale` exist only to annotate
age for observability. The real replay defense is
`internal/store.Upsert`'s version-ordered guard (identical `payload_hash`,
non-newer `occurred_at` is rejected as stale) — see `store.upsertSQL`.

### PayPal (`internal/webhook/paypal.go`, `internal/importer/paypal.go`)

**Decided 2026-08-01: verify via the `/v1/notifications/verify-webhook-signature`
REST round trip, not offline X.509 cert-chain validation**, even though the
offline path avoids a network call per event. Reasoning: at this project's
volume (a few thousand events, ever) the round-trip cost is close to
theoretical, while cert-chain validation is real security-sensitive code to
get right — a mishandled `PAYPAL-CERT-URL` fetch (e.g. not restricting it to
a `paypal.com` host before fetching) is a spoofing hole, not just a bug. The
REST round trip delegates that correctness to PayPal itself and reuses the
OAuth2 client `internal/importer/paypal.go` already has for Transaction
Search.

The offline signed-string format (`transmissionId|timeStamp|webhookId|
crc32(rawBody)`) is documented for reference in case the tradeoff is ever
revisited, but is not what this service implements. Either way, the raw body
bytes must be captured before JSON decoding — re-serializing loses key order
and whitespace and would break a CRC or HMAC check computed over the decoded
form.

**Normalizer validation status** (`internal/ingest.FromPayPalWebhook`).
`PAYMENT.CAPTURE.COMPLETED` and `PAYMENT.CAPTURE.REFUNDED` were proven
(2026-08-01) against a real sandbox transaction: an order captured then
refunded via the live Sandbox REST API, both deliveries genuinely signed by
PayPal, both verified, both normalized correctly — including the fee-sign
flip in both directions, and the refund's `paypal_reference_id` correctly
derived from its `up` link, with the classifier then pairing the two rows
correctly. `PAYMENT.CAPTURE.REVERSED` is implemented as if it shares the
refund shape (matching PayPal's public webhook reference), but unlike
COMPLETED/REFUNDED it has **not** been validated against a real delivery —
triggering a genuine reversal requires an actual bank-initiated chargeback,
which isn't practical to simulate on demand in a sandbox. Confirm against a
real delivery before trusting it in production.

## No projection/derived-table layer

`internal/store` deliberately holds one table (`events`) and nothing
resembling `tt_orders`/`tt_issued_tickets`/`paypal_transactions` mirror
tables. The canonical frame consumers need is assembled at *read* time by
`internal/model.Assemble` from the rows `LoadResources` returns, not
maintained as a standing projection. Trade-off, made deliberately: one source
of truth, no cached view that can drift from the raw events, and no
realistic performance reason at this project's data volume (thousands of
rows) to avoid computing it on demand. If a cross-resource join ever stops
being cheap enough to redo per request, the next step is a read-time SQL
view — not a materialized table, because a view cannot drift.

A consequence: a ticket can arrive before its order, or an order before its
event. This is not an error — a missing parent must read as empty/zero,
exactly like the reference implementation's
`tickets_df[order_id].map(order_txn_map).fillna("")`. `internal/model.Assemble`
implements this as a LEFT JOIN with COALESCE rather than an inner join, and
`/healthz` surfaces a count of unresolved references so an orphan doesn't
silently change a night's numbers without anyone noticing.

## Read-only is structural, not conventional

The query API's guarantee that no endpoint can mutate reconciliation state is
enforced by the database role, not by code review discipline. Documentation
saying "use a SELECT-only role" is not enforcement; a boot-time check that
fails loudly if the role can actually write is. `internal/api.Server`'s
startup path (`VerifyReadOnly`) attempts an insert inside a transaction it
then rolls back — a role that succeeds at that is treated as a boot failure,
not a warning.

## PII handling: allowlist, not denylist

`internal/ingest/sanitize.go` copies only an explicit allowlist of fields
into stored payloads, rather than trying to strip a denylist of known-PII
field names. A denylist fails open: the day a provider adds a field, or a
buyer types their address into a custom question, it lands in `jsonb` and
stays there silently. An allowlist fails closed — an unrecognized field is
dropped by default, on purpose.

This was validated against real captured data, not assumed. Two concrete
findings from that sweep:

- A Ticket Tailor **order** embeds a full copy of each of its tickets, so
  without allowlisting, all 2,562 ticket-level copies of `email` in one
  dataset would have been stored redundantly through the order object alone
  — the nesting itself is the argument for an allowlist over a denylist.
- `status_message` looked like a harmless status field by name, but in
  production data it is operator-authored free text and contained entries
  like `"Exchange other show (by <staff name> on <date>)"` — real PII that a
  key-name-based denylist would have missed entirely. It was only caught by
  checking sanitizer output *by value*, not by key name alone, which is why
  `internal/ingest/sanitize_realdata_test.go` exists as its own test file
  rather than folding into ordinary unit tests with synthetic fixtures.

Values are also copied as raw bytes rather than decoded and re-encoded:
re-encoding a JSON number can change its text (`"1e2"` becomes `"100"`,
trailing zeros move), and the parity harness depends on money values
surviving storage bit-for-bit.

## Migration tooling: hand-rolled runner, not goose/golang-migrate

Migrations are named `V{n}__snake_case.sql`, which matches neither `goose`
(`NNNNN_name.sql` with annotation comments) nor `golang-migrate`
(`{version}_{name}.{up|down}.sql`). Adopting either tool would mean either
renaming every migration or forking its filename parser, so a small embedded
runner was written instead. It advisory-locks (two Compose containers must
not race), checksums each file so an edited migration aborts the run, and
refuses a misnamed or non-contiguous file rather than silently skipping it —
a gap means a migration was deleted or never committed, and applying the
rest would produce a schema nobody has ever actually tested.

There are no down migrations; rollback for a single-table schema is a
Postgres restore (see `docs/BACKUP.md`), not a scripted down-migration.

## Backfill is always a full resync, never a `created_at` watermark

`cmd/backfill`'s live-import path does not support incremental sync by
`created_at`. Ticket Tailor orders carry no `updated_at` field at all, and a
`created_at` watermark would miss a refund posted against an old order — the
order's `created_at` never changes, so a watermark-based sync would never
revisit it. A full resync catches it; an incremental one silently wouldn't.
Ticket Tailor also has no sandbox at all, so any API key reaching the live
import path is a live key by construction — every live call site sits behind
an explicit `--live` flag for that reason, independent of the
`PAYPAL_SANDBOX` toggle which only covers PayPal.
