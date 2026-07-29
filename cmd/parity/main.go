// Command parity runs the reconciliation engine over a captured snapshot and
// emits the result as JSON for cell-by-cell diffing against the Python
// reference.
//
// Not implemented: scheduled for P1, once internal/recon has a body. This is
// the gate described in CLAUDE.md guardrail 1 — no live webhook subscription
// until its output matches the dashboard across real history.
package main

import (
	"fmt"
	"os"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/config"
)

var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "ticketing-reconciliation-service parity %s\n", version)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}

	if cfg.ParityDataDir == "" {
		fmt.Fprintln(os.Stderr, "SSG_PARITY_DATA is unset; nothing to reconcile against.")
		fmt.Fprintln(os.Stderr, "Point it at the out-of-tree historical data directory.")
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "parity data: %s\n", cfg.ParityDataDir)
	fmt.Fprintln(os.Stderr, "not implemented: scheduled for P1. See docs/PARITY.md.")
	os.Exit(1)
}
