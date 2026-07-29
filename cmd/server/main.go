// Command server runs the webhook ingestion pipeline and the read-only query
// API.
//
// Not implemented: per the phase order in docs/PARITY.md, webhook ingestion
// (P4) and the query API (P3) come after the reconciliation engine has been
// validated against real historical data. CLAUDE.md guardrail 1 forbids
// pointing this service at live subscriptions before then, so building the
// server earlier buys nothing.
package main

import (
	"fmt"
	"os"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "ticketing-reconciliation-service server %s\n", version)
	fmt.Fprintln(os.Stderr, "not implemented: scheduled for P3 (query API) and P4 (webhooks).")
	fmt.Fprintln(os.Stderr, "See docs/PARITY.md for the phase order.")
	os.Exit(1)
}
