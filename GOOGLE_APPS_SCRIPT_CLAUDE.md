# CLAUDE.md (draft — basis for a Google Apps Script reconciliation-report project)

> Generated from inside the `ticketing-reconciliation-service` Go repo, by
> reading its `CLAUDE.md`/`docs/` in full and the actual Go source
> (`internal/api`, `internal/model`, `internal/ingest`, `internal/recon`).
> This is a sibling document to `FRONTEND_CLAUDE.md` (the React+Vite option)
> in the same repo — both are drafts for **alternative or parallel**
> consumers of the same read-only recon API, not drafts of the same
> project. Move this into its own Apps Script project once scaffolded; treat
> it as a strong first draft, not a finished document. Sections marked
> **OPEN QUESTION** were deliberately left undecided rather than guessed.

## What this project is

A Google Apps Script project — living inside Google Workspace, likely as
its own standalone Sheet-bound (or Web App) project rather than attached to
an existing document — that consumes the read-only query API of the
`ticketing-reconciliation-service` Go project (**"the recon API"** /
**"the Go service"**) and produces the same reconciliation report described
in `FRONTEND_CLAUDE.md`: the report that used to live in the SSG Ticket
Dashboard's Reconciliation tab (`sections/reconciliation.py`) and its PDF
export (`sections/pdf_export.py`).

This exists because a stakeholder asked whether the report could instead
live natively inside Google Workspace — as its own app/sheet SSG staff
already know how to open — rather than (or alongside) a standalone
React web app. **Feasibility was assessed and confirmed** (see "Why Apps
Script is workable" below) before this document was written; what follows
assumes that direction is being pursued, not that it's still an open
question.

**Relationship to `FRONTEND_CLAUDE.md`'s React+Vite project**: these are
two independent consumers of the same API, evaluated as alternatives.
**OPEN QUESTION:** whether both actually get built (e.g. Apps Script for
treasury's day-to-day view, React for something else), or this replaces
that direction entirely, hasn't been decided. If both end up existing, see
"Duplicated aggregation logic" under Guardrails — that is the one real risk
of running both in parallel, and it should be resolved explicitly rather
than discovered later as two reports quietly disagreeing.

## Relationship to the recon API — source of truth, not to be re-derived

Same rule as `FRONTEND_CLAUDE.md`, repeated here because this document may
be read on its own once moved out of this repo:

- Read `ticketing-reconciliation-service/CLAUDE.md` in full — especially
  "Business logic contract," "Column mapping," "Query API," and "Persistence
  & read model." It is the frozen, published contract this project consumes.
- Read `internal/api/dto.go` and `internal/api/server.go` in that repo for
  the literal wire format. If this document's tables ever disagree with
  that source, the Go source wins.
- **Never reimplement the PayPal ↔ Ticket Tailor matching rule.** The Go
  service already decided, for every transaction and ticket, whether it's
  matched/unmatched/transferred/pending — that verdict is `recon_status` on
  every event `GET /events` returns. This project's job starts *after* that
  verdict exists.

## Why Apps Script is workable — and its real constraints

Apps Script can call the recon API and do the join/aggregation logic
described in `FRONTEND_CLAUDE.md`. It is not a toy environment for this —
`UrlFetchApp` is a real HTTPS client with custom headers, and the V8 runtime
supports modern JS (`Map`, `Set`, arrow functions, destructuring, template
literals). But three things about Apps Script's execution model shape the
design in ways a normal Node/browser app wouldn't need to worry about:

1. **6-minute execution cap, no memory between runs.** A single script
   execution (a menu click, a trigger firing) cannot run indefinitely and
   starts cold every time — there is no long-lived process to cache a
   join result in RAM between report views. Paginating through thousands of
   `GET /events` rows, joining them, and rendering a report **cannot** all
   happen as one live pass per report view once the dataset is more than a
   few hundred rows.
2. **No persistent database — a Sheet has to be the database.** This
   project doesn't get Postgres. The practical design is: a **time-driven
   trigger** periodically pages through `GET /events` (using `since`/
   `until` and a stored watermark) and writes/updates rows into a hidden
   "raw events" sheet tab, incrementally, across as many trigger firings as
   it takes. Totals/Statistics are then computed **from that sheet**, not
   from a live API call — closer to a small scheduled ETL job than a
   request-response app. This also means the report a user sees is only as
   fresh as the last successful sync, not live — that staleness window
   should be surfaced in the UI (e.g. a "last synced at" timestamp), not
   hidden.
3. **`UrlFetchApp` quota.** Google Workspace accounts get a daily URL Fetch
   quota (in the tens of thousands of calls); at SSG's data volume
   (thousands of rows total, paginated in batches of e.g. 100–500 per
   `GET /events` call) this is not a real constraint, but a sync design that
   makes one HTTP call per row would be a design bug, not a scale problem to
   solve later.

None of this makes the aggregation logic itself harder — it's the same
plain-JS grouping/summing work described in `FRONTEND_CLAUDE.md`. It changes
*where* that logic runs (against a Sheet, not against a live in-memory join)
and *when* (on a schedule, not on page load).

## Architecture

```
Time-driven trigger (e.g. every 15–30 min)
   │
   ▼
Apps Script sync function
   │  UrlFetchApp, Authorization: Bearer <token from Script Properties>
   ▼
Go recon API — GET /events?resource_type=...&since=<watermark>&cursor=...
   │
   ▼
"Raw events" sheet tab(s)  ←── one row per event, keyed on (source, resource_id)
   │  (same idempotency key the Go service itself uses — see below)
   ▼
On-demand or scheduled aggregation pass (join + group + sum, in Apps Script)
   │
   ▼
"Totals" / "Statistics" sheet tabs  →  charts  →  PDF export (Sheet.getAs / Doc template)
```

**Auth**: the recon API's bearer token is stored in **Script Properties**
(`PropertiesService.getScriptProperties()`), read only by the sync function,
never written into a cell, a log, or the script's source. This is
functionally equivalent to the "thin proxy" pattern in `FRONTEND_CLAUDE.md`
— the token lives server-side (on Google's infrastructure, under this
script's execution identity) and is never sent to an end user's browser.

**The equivalent of "restrict to whoever handles SSG's finances"** here is
controlling **who has Edit access to the Apps Script project itself** —
anyone with Editor access can read Script Properties via the Apps Script
editor UI, so that access list *is* the access-control boundary, the same
restriction the Go service's own "Access control" section calls for.
Sharing the resulting Sheet/report more broadly (e.g. read-only to a wider
group) is fine and separate — the sensitive boundary is who can open the
**script editor**, not who can view the finished report tab, provided the
report tab itself doesn't leak raw PayPal/Ticket Tailor identifiers to
someone who shouldn't have them (see PII note below).

**OPEN QUESTION — how the report surfaces**: a few options, not yet
decided:
- A **container-bound script** on a Sheet, with the Sheet itself as the
  "app" (Totals/Statistics as tabs, a menu item to trigger sync/PDF export).
  Closest to what treasury already knows how to use.
- A **standalone Web App** (`doGet`) rendering an HTML report, deployed
  with its own access control (`Execute as: Me`, `Who has access:` scoped to
  domain or specific users).
- Both — a Sheet as the data/computation layer, a thin Web App front end for
  a nicer read-only view.

## Data contract: identical to `FRONTEND_CLAUDE.md`

The wire format doesn't change based on who's consuming it. Rather than
duplicate it, this section only calls out what differs for an Apps Script
consumer specifically — see `FRONTEND_CLAUDE.md`'s "Data contract" section
for the full `eventDTO` shape, the per-`resource_type` payload field
tables, and the join logic (`internal/model.Assemble`'s LEFT JOIN over
order → ticket → event) this project has to replicate the same way.

- `GET /events?resource_type=&since=&until=&limit=&cursor=` — same
  endpoint, same keyset pagination via `next_cursor`. In Apps Script, drive
  the paging loop explicitly: fetch a page, write its rows to the sheet,
  follow `next_cursor` until absent, then persist the **new** watermark
  (the max `updated_at`/`occurred_at` seen) for next run — store it in
  Script Properties or a dedicated single-cell "sync state" area, not
  inferred from sheet contents each time.
- **Idempotency key for the sheet**: mirror the Go service's own
  `(source, resource_id)` uniqueness (see its `CLAUDE.md`, "Data model") —
  key each row in the raw-events sheet the same way (e.g. a helper column
  concatenating `source` + `resource_id`) so a resynced/updated event
  **overwrites** its existing row rather than appending a duplicate.
  Getting this wrong double-counts revenue the moment any event is ever
  updated after its first sync — which is the normal case, not an edge
  case (an order's `refund_amount` changing, a ticket voided after sale).
- **Money**: same rule as `FRONTEND_CLAUDE.md` — sum in integer minor units
  (`amounts.gross.minor` etc., or `payload.listed_price` for tickets),
  never `parseFloat` a decimal string and sum floats. Apps Script's numbers
  are the same IEEE-754 doubles JavaScript always uses, so the same
  float-accumulation risk applies identically here.

## Aggregation logic — same decision as `FRONTEND_CLAUDE.md`: correct, not bug-for-bug

This carries over directly, not a separate decision: this project's
Totals/Statistics should compute **corrected** figures using `recon_status`
as-is, not reproduce the old dashboard's documented bugs (cross-night
double-counting, the always-empty unmatched-transaction filter, the
CPython/numpy dual-rounding split). See `FRONTEND_CLAUDE.md`'s "Aggregation
fidelity" section for the full reasoning — it applies verbatim here; there
is no reason for the Apps Script report and the React report (if both end
up existing) to disagree about *what correct looks like*, even though they
may be implemented independently.

Concretely, port (not literally copy — different runtime) the same rules:

- Group by `occurrence` (`issued_ticket.payload.event_id`), label by the
  joined event's `start.unix`.
- Attribute each PayPal transaction to a night once, via its matched
  ticket(s) — never once per night it happens to touch.
- Unmatched/pending transactions surfaced explicitly (their own sheet tab
  or clearly marked rows), never dropped or guessed into Gross/Net.
- Transferred tickets keep their revenue in Totals — a `transferred`
  status is not a cancellation for reporting purposes.
- A night with no matched PayPal transactions falls back to summing
  ticket revenue (`issued_ticket.payload.listed_price`) directly, same as
  the React project's fallback.

## PDF export — native, and simpler here than in the React project

This is a genuine advantage of the Apps Script route: PDF and chart
rendering are **built into** Sheets/Docs, with no equivalent of the
"jsPDF vs. server-side Puppeteer" decision still open in
`FRONTEND_CLAUDE.md`.

- `Sheet.getAs(MimeType.PDF)` (or `DriveApp`/`Drive API` export) turns a
  formatted Sheet tab directly into a PDF — page size, margins, and print
  area are configured on the Sheet itself (`File > Print settings`,
  scriptable via `SpreadsheetApp`'s page-setup methods).
- Native Sheets charts (pie, stacked column) can be embedded on a "report"
  tab and will render into the PDF export as-is — no chart-to-image step
  needed.
- For a closer match to the dashboard's specific multi-section landscape
  layout (metrics strip, refund-impact table, yield/capacity block), a
  **Google Docs template** populated via `DocumentApp` and then exported to
  PDF gives more layout control than a Sheet export, at the cost of
  building/maintaining that template. **OPEN QUESTION**: Sheet-export vs.
  Doc-template — not yet decided; either is a reasonable starting point,
  and a Sheet export is the faster path to a first working version.

## Guardrails — do not skip these

1. **Never reimplement the PayPal ↔ Ticket Tailor matching rule.** Same as
   `FRONTEND_CLAUDE.md`, guardrail 1 — `recon_status`/`recon_counterpart_id`
   are already correct. This project only groups and sums.
2. **Never let the recon API token leave Script Properties.** Not into a
   sheet cell (even a "config" tab), not into `Logger.log`, not into a
   Web App response body, not into the sync watermark storage.
3. **Restrict Editor access on the Apps Script project itself** to whoever
   is authorized to handle SSG's reconciliation data — that access list is
   the real security boundary here, not the visibility of the finished
   report tab. Review it the same way the Go service's `API_KEYS` are
   meant to be issued/revoked per person, not shared as one login.
4. **Never build a feature that needs buyer PII.** The Go service strips
   it before storage; there is no path to recover it from `GET /events`,
   and nothing here should assume otherwise.
5. **Sum money in integer minor units, never in parsed decimal floats** —
   identical risk and identical rule to `FRONTEND_CLAUDE.md`.
6. **Get the sheet's idempotency key right before trusting any total.**
   A raw-events sheet keyed incorrectly (e.g. appending instead of
   overwriting on `(source, resource_id)`) will silently double-count the
   moment any single event is ever updated — which happens routinely
   (a ticket voided after sale, a refund posted against an old order), not
   as a rare edge case.
7. **Duplicated aggregation logic is a real, ongoing risk if both this
   project and a React frontend exist.** The matching rule isn't
   duplicated (it's server-side and shared), but the join/grouping/summing
   logic described above would be written twice, in two different
   runtimes, by construction. If both projects end up shipping, either
   (a) periodically cross-check their Totals/Statistics output against
   each other for the same show and treat any disagreement as a bug in
   one of them, or (b) factor the aggregation logic into a small
   dependency-free JS module written in a subset both a Node/browser build
   and Apps Script's V8 runtime can run, and share it via `clasp` rather
   than hand-porting it twice. Neither is set up yet — decide before both
   projects have diverging aggregation logic in production, not after.

## Open questions / decisions still needed

- Does this project ship alongside `FRONTEND_CLAUDE.md`'s React app, or
  instead of it? (See "Relationship to `FRONTEND_CLAUDE.md`" above.)
- Report surface: container-bound Sheet, standalone Web App, or both?
- PDF path: Sheet export or a Docs template?
- Sync cadence: how often should the time-driven trigger run? Balance
  report freshness against `UrlFetchApp`/execution quota and how quickly
  SSG treasury actually needs a new transaction to show up.
- Where does the sync watermark/cursor live — Script Properties, or a
  dedicated cell/tab in the sheet itself (more visible/debuggable, but
  editable by anyone with sheet-edit access, which may be broader than
  script-edit access)?
- Should the raw-events sheet be visible/auditable by report viewers, or
  hidden and only the computed Totals/Statistics tabs shown?
- Everything else not addressed above (naming, folder/Drive organization,
  who owns the Apps Script project in Workspace admin terms, error
  alerting if a sync run fails) is undecided and should be resolved as this
  project is actually scaffolded, not assumed from this draft.
