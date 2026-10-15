# CLAUDE.md (draft — basis for the frontend project's own CLAUDE.md)

> This file was generated from inside the `ticketing-reconciliation-service`
> Go repo, by reading its `CLAUDE.md` and `docs/` in full, plus the actual
> Go source (`internal/api`, `internal/model`, `internal/ingest`,
> `internal/recon`) rather than inferring behavior from the docs alone. It
> is meant to be moved into the new frontend repo and edited there — treat
> it as a strong first draft, not a finished document. Sections marked
> **OPEN QUESTION** were deliberately left undecided rather than guessed;
> resolve them before or during initial implementation.
>
> Six architectural forks were decided with the project owner before this
> draft was written; they are recorded inline below rather than re-litigated.

## What this project is

A JavaScript frontend that consumes the read-only query API of the
`ticketing-reconciliation-service` Go project (referred to below as **"the
recon API"** or **"the Go service"**) and produces the reconciliation
report that used to live in the SSG Ticket Dashboard (Streamlit/Python),
specifically its Reconciliation Report tab (`sections/reconciliation.py`)
and its PDF export (`sections/pdf_export.py`).

The Go service deliberately does **not** produce this report itself — it
stores per-transaction/per-ticket facts (`matched` / `unmatched` /
`transferred` / `pending`) and serves them as raw, paginated events. Shaping
those into a report — grouping into nights, summing gross/fees/net, category
breakdowns, a PDF — is explicitly a **consumer-side concern** by the Go
service's own design (see its `CLAUDE.md`, "Persistence & read model" and
"Query API"). This project is that consumer. It is the report; the Go
service is only the source of truth for the facts the report is built from.

## Relationship to the recon API — source of truth, not to be re-derived

Do not guess the API's shape, the money-handling rules, or the column
mapping from the description in this file. Before implementing anything
that touches the API:

- Read `ticketing-reconciliation-service/CLAUDE.md` in full — especially
  "Business logic contract," "Column mapping," "Query API," and "Persistence
  & read model." It is the frozen, published contract this project consumes.
- Read `internal/api/dto.go` and `internal/api/server.go` in that repo for
  the literal wire format — the tables below were extracted from those
  files, but they can drift; the Go source is authoritative if the two ever
  disagree.
- If the Go service's API shape changes in a way that affects this project,
  that's a breaking change to a published interface (the Go docs say so
  explicitly) — it should be a deliberate, coordinated change, not something
  this project silently works around.

**This project must never reimplement the PayPal ↔ Ticket Tailor matching
rule.** The Go service already decided, for every transaction and ticket,
whether it's matched/unmatched/transferred/pending — that verdict is
`recon_status` on every event returned by `GET /events`. This project's job
starts *after* that verdict exists: grouping and summing already-classified
records, never re-deciding whether two records belong together. If a report
figure looks wrong, the fix is almost never "add a matching rule here" — see
Guardrails below.

## Relationship to the dashboard repo — layout reference only, not logic

`reference/ssg_dashboard/sections/pdf_export.py` (inside the Go repo, or a
fresh clone of the dashboard repo — see that project's own rules about never
copying a live working directory into a tree) is a useful reference for the
**visual shape** of the report SSG's treasurer is already used to reading:
a landscape A4 PDF with, in order —

1. Header: production name, period, generation timestamp.
2. A metrics strip: Gross / Fees / Net for the whole show.
3. **Totals table**: one row per performance night (`Performance Date`,
   `Gross`, `Fees`, `Net`), plus an appended TOTAL row.
4. **Refund impact table** (only if refunds exist): per refund, the fee
   PayPal returned vs. retained, and the transaction's payer-facing note.
5. **Statistics table**: one row per night (`Performance Date`,
   `Total Tickets`, then one column per ticket category), plus a stacked
   bar chart of the same data and an appended TOTAL row.
6. Category pie chart + yield/capacity metrics (sell-through %, revenue per
   seat) if a house capacity is configured.
7. Revenue trend (cumulative) and daily sales bar chart.
8. A PayPal-vs-Ticket-Tailor gross reconciliation footer (pass/fail banner).

Reproduce this **layout and section order** for treasurer familiarity. Do
**not** reproduce its arithmetic quirks — see "Aggregation fidelity" below,
where the project owner explicitly decided this project computes corrected
figures, not bug-for-bug parity with the old dashboard's known bugs.

## Architecture: browser SPA + thin backend proxy

**Decided:** this project is a React + Vite single-page app, plus a small
backend proxy component. The browser never talks to the recon API directly.

**Why a proxy, not a pure static SPA:** the recon API authenticates with a
long-lived per-consumer bearer token (`API_KEYS` in the Go service — see its
`docs/ENVIRONMENT.md`), and reconciliation data is buyer-payment mapping
that the Go service's own "Access control" section says must stay restricted
to whoever handles SSG's finances. A token embedded in browser JS is
readable by anyone who opens devtools on the page, which defeats that
restriction the moment the page is public even briefly. A thin proxy holds
the token server-side and is the only thing that ever presents it to the
Go API.

```
Browser (React/Vite SPA)
   │  same-origin fetch, no recon API token ever sent to the browser
   ▼
Thin backend proxy  ──(Authorization: Bearer <RECON_API_KEY>)──▶  Go recon API
   │
   └─ own access control (who may load the report at all)
```

Concretely, the proxy should:

- Hold `RECON_API_KEY` (or similarly named) as a server-side secret, never
  shipped to the client bundle.
- Expose only the routes this frontend actually needs (e.g. a handful of
  `/api/...` routes that forward to `GET /events` with server-validated query
  params), not a raw pass-through of arbitrary paths/headers — the browser
  should not be able to smuggle its own `Authorization` header through.
- Gate access to itself, independent of the recon API's own key — e.g. a
  login/session or an org SSO check — since the recon API's per-consumer
  keys are meant to be issued to *services*, not to every individual
  browser tab. **OPEN QUESTION:** what auth this proxy itself uses for
  end users (shared password, per-person login, IP allowlist, SSO) hasn't
  been decided; the Go service's own precedent is "per-consumer keys,
  revocable individually," which argues for per-person credentials here
  too rather than one shared login, but this needs an explicit decision
  before shipping to real users.

**OPEN QUESTION — proxy implementation:** not yet decided. A minimal
Node/Express (or Fastify) server matches the Go service's own
no-framework-where-possible philosophy and keeps this project's runtime
footprint small; that's the natural default given React+Vite was chosen
over a meta-framework like Next.js. Confirm before scaffolding.

**OPEN QUESTION — deployment target:** not yet decided with the project
owner. The natural fit, given the Go service already runs via Docker Compose
on a small Hetzner VPS behind Caddy (see its `docs/DEPLOYMENT.md`), would be
to add this project as a sibling Compose service on the same host,
reverse-proxied by the same Caddy instance on its own subdomain/path. That
is a reasonable default to propose, not a decision already made — confirm
before committing to it, and don't provision infrastructure on that
assumption alone.

## Data contract: what `GET /events` actually returns

Base behavior (see the Go repo's `internal/api/server.go`, `dto.go`):

- `GET /events?source=&status=&recon_status=&resource_type=&since=&until=&limit=&cursor=`
  — paginated, **keyset**-ordered (not offset). Response:
  `{ "events": [eventDTO...], "next_cursor": "opaque-string-or-absent" }`.
  Absent `next_cursor` means last page. Treat the cursor as opaque — the
  Go docs say the ordering columns behind it are not a published interface.
- `GET /events/{id}` — one event by its row `id` (a uuid), or the same
  `eventDTO` shape.
- `GET /healthz` — unauthenticated. Row counts by status, by recon verdict,
  and unresolved-parent counts (see "Cross-resource joins" below). Worth
  surfacing in this app's own ops view, since an unresolved-reference count
  climbing silently changes a night's numbers.
- Auth: `Authorization: Bearer <key>` or `X-API-Key: <key>`, checked by the
  proxy layer described above — the browser app itself never holds this.

`since`/`until` accept either RFC3339 or a bare `YYYY-MM-DD`; a bare date on
`until` rounds **up** to the end of that day (23:59:59.999999999), not its
start — pass a full RFC3339 timestamp if you need exact-instant precision.

### `eventDTO` shape

```jsonc
{
  "id": "uuid",
  "source": "paypal" | "tickettailor",
  "resource_type": "order" | "issued_ticket" | "event" | "event_series"
                  | "paypal_transaction" | "other",
  "resource_id": "string",
  "origin": "webhook" | "backfill",
  "topic": "string, omitempty",
  "status": "received" | "processing" | "processed" | "failed"
           | "dead_lettered" | "ignored",

  "recon_status": "matched" | "unmatched" | "transferred" | "pending"
                 | "not_applicable",
  "recon_counterpart_id": "string, omitempty",

  "occurred_at": "RFC3339 — the PROVIDER's timestamp",
  "created_at": "RFC3339",
  "updated_at": "RFC3339",

  // Present ONLY when resource_type is "paypal_transaction". Absent (not
  // zero) means the payload lacked one of gross/fee/net — an honest gap,
  // never an invented zero.
  "amounts": {
    "currency": "EUR",
    "gross": { "decimal": "10.00", "minor": 1000 },
    "fee":   { "decimal": "-0.49", "minor": -49 },
    "net":   { "decimal": "9.51",  "minor": 951 }
  },

  // The raw provider payload, byte-preserved (see Money handling below).
  // Shape depends on resource_type — see per-type payload shapes.
  "payload": { /* ... */ }
}
```

**`recon_status` means different things per `resource_type`** — this is
deliberate on the Go side, not an oversight: a `paypal_transaction` is
`matched`/`unmatched`; `transferred` only ever applies to a Ticket Tailor
`issued_ticket`. Filtering `?recon_status=matched` is asking about payments,
not tickets. Don't conflate the two when building UI filters.

### Per-`resource_type` payload shapes (from `internal/ingest/decode.go`)

These are the fields this project can actually rely on being present inside
`payload` for each `resource_type` — buyer PII (email, name, phone) is
stripped before storage (allowlist, not denylist — see the Go repo's
`docs/ARCHITECTURE.md`), so don't expect it and don't build a feature that
needs it.

| `resource_type` | Key `payload` fields |
|---|---|
| `order` | `id`, `txn_id` (the PayPal match key), `status`, `created_at` (unix seconds), `refund_amount`, `total_paid`, `currency.code`, `payment_method.type` |
| `issued_ticket` | `id`, `order_id`, `event_id`, `event_series_id`, `ticket_type_id`, `description` (→ canonical `category`), `status`, `listed_price` (minor units), `created_at`, `updated_at` (unix seconds — canonical `date` comes from this, **not** `created_at`), `listed_currency.code` |
| `event` | `id`, `event_series_id`, `name` (→ canonical `show`), `start.unix`, `end.unix` |
| `paypal_transaction` | `txn_id`, `date` (raw `YYYY-MM-DD` string — do not re-parse/reformat it, see below), `gross`, `fee`, `net`, `status`, `paypal_reference_id`, `currency` |

`refund_amount` on an `order` is **not** converted from minor units on the
Go side, because the reference implementation only ever compares it against
zero. If this project ever displays it as money rather than just testing
`=== 0`, convert it from cents first — don't copy the Go behavior blindly
into a context where it would be wrong.

### Money handling — decimal strings + minor units, never a bare float

The `amounts` block exists specifically so a consumer doesn't have to parse
a JSON float and do float arithmetic across hundreds of transactions — that
was the single highest-risk conversion in the Go project (see its
`CLAUDE.md`, "Data sources"). This project must not undo that:

- **Sum in minor units (integer cents), never in decimal floats.**
  `amounts.gross.minor` etc. are exact integers; sum those, then format the
  final total back to a decimal string for display. Summing
  `amounts.gross.decimal` after `parseFloat` reintroduces exactly the
  float-accumulation risk the API was designed to avoid.
- For `issued_ticket` rows there is no `amounts` block — revenue comes from
  `payload.listed_price`, which is already in minor units (cents) per the
  Go service's frozen column mapping (`prices_in_cents: true`). Treat it the
  same way: sum as integer cents.
- The raw `payload` still carries the same amounts as JSON numbers (for
  PayPal rows) — that's for anything that needs the untouched provider
  data; this project's own totals should come from `amounts`/minor-unit
  fields, not from re-parsing `payload`.
- `paypal_transaction.payload.date` is a bare `YYYY-MM-DD` string sliced
  from the provider's raw timestamp with no timezone conversion. Display it
  verbatim; do not run it through a date parser and reformat it, or a
  transaction near a timezone boundary can silently shift to the wrong
  calendar day (this is exactly the trap the Go service's own normalizer
  was written to avoid — see `model.PayPalTxn.DateKey`'s doc comment).

## Cross-resource joins are this project's job too

The Go service has **no projection/derived-table layer** by design (see its
`docs/ARCHITECTURE.md`, "No projection/derived-table layer") — `GET /events`
returns flat rows per resource, not a pre-joined "ticket with its order and
event attached" shape. To build one row per ticket with the show name,
category, revenue, night, and PayPal match key all attached (the shape the
old dashboard called its "canonical frame," and what Totals/Statistics group
over), this project has to do the same LEFT JOIN the Go service's own
`internal/model.Assemble` does at read time:

1. Fetch all `order`, `issued_ticket`, and `event` rows (paginate through
   `GET /events?resource_type=...` for each).
2. Index `order` by its `id`, `event` by its `id`.
3. For each `issued_ticket`, join in its order's `txn_id` /
   `payment_method.type` / `refund_amount`, and its event's `name` /
   `start.unix` — **a missing order or a tombstoned/missing event must
   produce an empty/zero value, not drop the ticket or throw.** A ticket
   legitimately arrives before its order under the Go service's own
   eventual-consistency model; treating that as an error would make report
   totals flicker based on webhook arrival order rather than actual state.
4. `occurrence` (the night-grouping key) is the ticket's `event_id` itself,
   not a parsed date — group by that, then use the joined event's
   `start.unix` (parsed as UTC, per the frozen column mapping) purely as the
   *label* for that group.

This mirrors `internal/model/assemble.go` in the Go repo closely enough that
reading it side-by-side while implementing this join is worth the ten
minutes — it documents exactly which edge case (tombstoned event, missing
order, multi-ticket order total-splitting) it exists to handle and why.

## This project's job: aggregation, not matching

### Aggregation fidelity — decided: correct numbers, not bug-for-bug parity

**Decided with the project owner:** unlike the Go service's matching rule
(which bug-for-bug reproduces `sections/reconciliation.py`, deviations
tracked in a ledger, because that parity is guardrail 1's evidence that the
Go service can safely replace the dashboard), this project's *Totals and
Statistics aggregation* is **not** required to reproduce the old dashboard's
known-broken behavior. The Go repo's own `internal/recon/oracle` package —
which does reproduce those bugs — is explicitly documented as "a
verification oracle, not a product surface... not shipped" and says
outright: *"For the truthful per-transaction answer, use recon.Classify
instead."* `recon_status` on every event already **is** that truthful
answer. Concretely, this means:

- **Do not** reproduce cross-night double-counting (a transaction covering
  tickets on two performance nights should be attributed once, not once per
  night it touches).
- **Do not** reproduce the old "unmatched PayPal transactions" detection,
  which was provably always empty in the reference implementation. Build
  the unmatched list straight from `?recon_status=unmatched` — that's
  already correct.
- **Do not** replicate the CPython-`round()`-vs-`numpy.round()` split the Go
  parity harness had to reproduce bit-for-bit. That split was a Python
  implementation-detail bug (guardrail 5/6 in the Go repo exist purely to
  catch *unintentional* divergence from it during the port, not to bless it
  as correct behavior going forward). This project should just do ordinary
  correct decimal rounding to 2 places, in integer cents, as described
  above.
- Historical reports generated by this project may therefore not match a
  PDF the treasurer generated from the old dashboard for the *same* show,
  cent-for-cent. That's expected and accepted — the old numbers were wrong
  in specific, documented ways; this project's numbers are the fix.

### Building Totals

One row per performance night (grouped by `occurrence`/`event_id`, labeled
by the joined event's start date):

- **Gross / Fees / Net**: sum the `amounts.gross.minor` / `.fee.minor` /
  `.net.minor` of every `paypal_transaction` event whose `recon_status` is
  `matched` (and, if you choose to surface them inline rather than in a
  separate warnings panel, `pending`) and which is attributable to that
  night via the joined ticket(s) it corresponds to (`recon_counterpart_id`
  / the match key, per ticket → order → PayPal transaction).
- A night with **no** matched PayPal transactions at all (e.g. cash/operator
  sales with no PayPal order) should fall back to summing ticket revenue
  (`issued_ticket.payload.listed_price`) directly — this mirrors the old
  dashboard's fallback path, minus its bugs, and keeps a night from showing
  a blank Gross when SSG did actually take money for it.
- An appended TOTAL row, summed the same way (integer cents, never
  re-deriving from already-rounded decimals).

### Building Statistics

One row per night: `Total Tickets` (count of `issued_ticket` rows for that
`occurrence`, excluding voided/cancelled/refunded per their `status`), and
one column per distinct `category` (`issued_ticket.payload.description`)
with the ticket count in that category for that night. An appended TOTAL
row summing all nights.

### Transferred and pending — surface, don't drop

Per the Go service's business contract (its `CLAUDE.md`, and its
plain-English restatement in `docs/HANDOVER.md`):

- **`transferred`** tickets (voided in Ticket Tailor, PayPal charge still
  stands, no refund issued) keep their revenue in the report — the group
  keeps the income. Don't let a `transferred` ticket's status make it look
  like a cancelled sale that should be excluded from Totals.
- **`unmatched`** PayPal transactions must be visibly surfaced somewhere in
  this project's UI (a warnings panel, a dedicated tab — not buried), not
  silently excluded from the report. They mean a payment PayPal took has no
  Ticket Tailor order pointing at it — worth a human's attention, per the
  Go service's own framing.
- **`pending`** refunds (a refund whose original charge couldn't be
  resolved) must also be surfaced, not guessed into either Gross or
  excluded silently. Same reasoning: a report that quietly drops a pending
  row understates net with no visible sign anything is wrong.

## PDF export — in scope now

**Decided:** PDF export is part of this project's initial scope, not
deferred work, matching the visual structure described under "Relationship
to the dashboard repo" above.

**OPEN QUESTION — rendering approach:** not yet decided. Two reasonable
directions, either is defensible:

- **Client-side** (e.g. `pdfmake`, `jspdf` + `jspdf-autotable`, or
  `@react-pdf/renderer`): no extra backend load, works from data already in
  the browser after the proxy fetch. Charts need a canvas-to-image step or a
  PDF-native charting approach.
- **Server-side in the proxy** (e.g. Puppeteer rendering an HTML report to
  PDF, or a Node PDF library run in the proxy process): closer to what
  `fpdf`-in-Python did, easier to keep pixel-perfect control over the
  landscape-A4 layout and charts, but adds real weight to what was scoped as
  a "thin" proxy.

Resolve this before scaffolding the PDF feature — it affects whether the
proxy stays thin or grows a rendering responsibility.

## Guardrails — do not skip these

1. **Never reimplement the PayPal ↔ Ticket Tailor matching rule in this
   project.** `recon_status` and `recon_counterpart_id` on each event are
   the Go service's product and are already correct (or, for `pending`,
   correctly flagged as unresolved rather than guessed). If a figure in this
   project's report looks wrong, the fix is in how this project aggregates
   already-classified events — grouping, join logic, rounding — almost
   never in re-deciding which transaction belongs to which ticket.
2. **Never hold the recon API's bearer token in browser-shipped code.** It
   must live only in the server-side proxy's environment/secrets, never in
   a Vite env var prefixed for client exposure (e.g. never `VITE_`-prefixed),
   never in a bundled config file, never logged.
3. **Never build a feature that needs buyer PII.** The Go service strips it
   before storage on purpose (email/name/phone are gone by the time this
   project ever sees a row) — there is no path to recover it, and no
   feature here should assume one exists.
4. **Sum money in integer minor units, never in parsed decimal floats** —
   see "Money handling" above. This is the single highest-risk conversion
   in the Go project and the same risk exists here the moment this project
   does its own arithmetic over `amounts`.
5. **Treat the Go service's `GET /events` DTO shape as a versioned,
   published interface**, not an implementation detail to poke around
   freely. If it changes, that's a coordinated change across both repos.
6. **Don't design around live SSG credentials in development.** Whatever
   this project's own dev/test setup looks like, it should point at the Go
   service's own sandbox/dev instance (per that repo's guardrail 4), never
   at a production `API_KEYS` value issued for the real deployment.

## Tech stack & conventions (partially decided, partially open)

- **Frontend**: React + Vite. — *decided.*
- **Backend proxy**: thin server holding the recon API token, gating its
  own access, forwarding only the routes this app needs. — *decided in
  shape; runtime (Node/Express suggested) and its own end-user auth are
  open questions above.*
- **PDF export**: in scope for v1. — *decided in scope; rendering approach
  open, see above.*
- **Deployment target**: likely a Compose sibling next to the Go service on
  the same VPS, behind the same Caddy instance — *proposed, not confirmed.*
- Everything else (test framework, linting, styling approach, state
  management, whether this becomes a monorepo with the proxy as a
  workspace or two separate deployables) is **undecided** and should be
  filled in as this project actually gets scaffolded, not assumed from this
  draft.
