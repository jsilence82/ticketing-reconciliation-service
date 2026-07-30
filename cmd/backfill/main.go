// Command backfill imports historical data from the Ticket Tailor and PayPal
// REST APIs.
//
// Webhooks only fire forward from when a subscription is created, so history —
// including everything predating this service — can only come from the providers'
// REST APIs. This is a separate ingestion path from the webhook handler, but it
// writes the SAME events rows with origin='backfill' and converges on the same
// reconciliation worker. Dedup is on (source, resource_id), so a transaction
// backfilled today and delivered by webhook tomorrow collapses to one row rather
// than being processed twice. See CLAUDE.md, "Historical data (backfill)".
//
// Being a batch job, it owns its own throttling: PayPal Transaction Search caps a
// query at 31 days and pages on total_pages; Ticket Tailor pages on a
// starting_after cursor. Both need explicit rate limiting, unlike the
// request-per-delivery webhook path.
//
// Not implemented: scheduled for P2. The --live flag below is defined now
// because it is a safety gate, not a feature — CLAUDE.md guardrail 4 requires
// that touching real provider accounts be an explicit, deliberate act rather
// than something a dev loop or CI run can fall into.
package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	var (
		live     = flag.Bool("live", false, "permit use of live (non-sandbox) provider credentials")
		snapshot = flag.String("snapshot", "", "write raw provider responses as JSONL to this directory")
	)
	flag.Parse()

	fmt.Fprintf(os.Stderr, "ticketing-reconciliation-service backfill %s\n", version)
	fmt.Fprintln(os.Stderr, "not implemented: scheduled for P2.")

	if *live {
		fmt.Fprintln(os.Stderr, "note: --live was passed; this will require real credentials once implemented.")
	}
	if *snapshot != "" {
		fmt.Fprintf(os.Stderr, "note: --snapshot=%s; snapshots contain buyer PII and must stay out of the repo.\n", *snapshot)
	}
	os.Exit(1)
}
