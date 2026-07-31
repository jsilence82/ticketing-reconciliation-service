// Command parity runs the reconciliation engine over a captured snapshot and
// emits the result as JSON for cell-by-cell diffing against the Python
// reference.
//
// This is the gate described in CLAUDE.md guardrail 1: no live webhook
// subscription until this output matches the dashboard across real history.
//
// Pair it with tools/parity/driver.py, which produces the same JSON shape from
// the unmodified reference implementation, then diff the two files.
//
//	python tools/parity/driver.py "$SSG_PARITY_DATA" python.json
//	./bin/parity -out go.json
//	python tools/parity/compare.py python.json go.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/config"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/recon"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/recon/oracle"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/snapshot"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

var version = "dev"

// Floats are emitted as hex literals so the comparison is over exact IEEE-754
// bits rather than a decimal rendering that could mask a one-ULP difference.
type totalsRowJSON struct {
	PerformanceDate string `json:"performance_date"`
	Transactions    int    `json:"transactions"`
	Gross           string `json:"gross"`
	Fees            string `json:"fees"`
	Net             string `json:"net"`
}

type totalsJSON struct {
	Rows  []totalsRowJSON `json:"rows"`
	Total *totalsRowJSON  `json:"total"`
}

type statsRowJSON struct {
	PerformanceDate string         `json:"performance_date"`
	TotalTickets    int            `json:"total_tickets"`
	ByCategory      map[string]int `json:"by_category"`
}

type statsJSON struct {
	Rows       []statsRowJSON `json:"rows"`
	Total      *statsRowJSON  `json:"total"`
	Categories []string       `json:"categories"`
}

type showJSON struct {
	Totals         totalsJSON `json:"totals"`
	Statistics     statsJSON  `json:"statistics"`
	MatchedTxnIDs  []string   `json:"matched_txn_ids"`
	UnmatchedCount int        `json:"unmatched_count"`
}

type outputJSON struct {
	Env         map[string]string   `json:"env"`
	RecordCount int                 `json:"record_count"`
	TxnCount    int                 `json:"txn_count"`
	Shows       []string            `json:"shows"`
	Results     map[string]showJSON `json:"results"`

	// Classification is the service's ACTUAL product, reported here for
	// visibility. It is show-agnostic and has no Python counterpart, so
	// compare.py ignores it — the reference has no per-resource verdict to diff
	// against.
	Classification *classificationJSON `json:"classification,omitempty"`
}

type classificationJSON struct {
	Matched       int `json:"matched"`
	Unmatched     int `json:"unmatched"`
	Transferred   int `json:"transferred"`
	Pending       int `json:"pending"`
	NotApplicable int `json:"not_applicable"`
}

// hexf renders a float the way Python's float.hex() does.
func hexf(v float64) string {
	return strconv.FormatFloat(v, 'x', -1, 64)
}

func main() {
	var (
		out     = flag.String("out", "", "write JSON here (default: stdout)")
		dataDir = flag.String("data", "", "snapshot directory (default: $SSG_PARITY_DATA)")
		fixAll  = flag.Bool("fix-all", false,
			"enable every fix flag; output will NOT match the reference (see docs/PARITY.md)")
		fromDB = flag.Bool("from-db", false,
			"read from Postgres instead of the snapshot files, to prove the storage "+
				"round trip is lossless")
		dsn = flag.String("database-url", "", "Postgres connection string (default: $DATABASE_URL)")
	)
	flag.Parse()

	// Default: reproduce the reference exactly, bug for bug. -fix-all exists to
	// quantify how much the known defects actually move the numbers, and to
	// prove the comparison harness can detect a difference at all — a parity
	// check that cannot fail is worthless.
	flags := recon.Flags{}
	if *fixAll {
		flags = recon.Flags{
			FixUnmatchedDetection:    true,
			FixCrossNightDoubleCount: true,
			FixMultiRefund:           true,
			FixRetainedFeeRatio:      true,
			FixStatusReadmit:         true,
			FixStatusTrim:            true,
			FixNaTPerformanceDate:    true,
			FixTransactionUnits:      true,
		}
	}

	// The snapshot directory is only needed by the file path; -from-db reads
	// everything from Postgres.
	dir := *dataDir
	if !*fromDB {
		if dir == "" {
			cfg, err := config.Load()
			if err != nil {
				fmt.Fprintf(os.Stderr, "config: %v\n", err)
				os.Exit(2)
			}
			dir = cfg.ParityDataDir
		}
		if dir == "" {
			fmt.Fprintln(os.Stderr, "no snapshot directory: pass -data or set SSG_PARITY_DATA")
			fmt.Fprintln(os.Stderr, "It must point OUTSIDE this repository (see CLAUDE.md).")
			os.Exit(2)
		}
	}

	var (
		tickets []model.CanonicalTicket
		txns    []model.PayPalTxn
		err     error
	)
	if *fromDB {
		// The whole point of this path: everything downstream of here —
		// model.Assemble, internal/recon, the oracle, the hex-float encoder —
		// is the same code the file path runs. Only the source differs, so a
		// difference in output can only have come from storage.
		tickets, txns, err = loadFromDB(*dsn)
	} else {
		tickets, txns, err = loadFromFiles(dir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}

	shows := distinctShows(tickets)
	sort.Strings(shows)

	results := make(map[string]showJSON, len(shows))
	for _, show := range shows {
		res, err := oracle.Build(tickets, txns, show, flags)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile %q: %v\n", show, err)
			os.Exit(1)
		}
		results[show] = toJSON(res)
	}

	// Classify runs once over everything, not per show: a resource's verdict
	// cannot depend on which show a caller happened to ask about.
	counts := recon.Summarise(recon.Classify(tickets, txns, flags))

	payload := outputJSON{
		Env:         map[string]string{"go": version, "impl": "go"},
		RecordCount: len(tickets),
		TxnCount:    len(txns),
		Shows:       shows,
		Results:     results,
		Classification: &classificationJSON{
			Matched:       counts.Matched,
			Unmatched:     counts.Unmatched,
			Transferred:   counts.Transferred,
			Pending:       counts.Pending,
			NotApplicable: counts.NotApplicable,
		},
	}

	enc, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
	enc = append(enc, '\n')

	if *out == "" {
		_, _ = os.Stdout.Write(enc)
	} else if err := os.WriteFile(*out, enc, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}

	source := "files"
	if *fromDB {
		source = "postgres"
	}
	fmt.Fprintf(os.Stderr, "[%s] %d records, %d transactions, %d shows\n",
		source, len(tickets), len(txns), len(shows))
	fmt.Fprintf(os.Stderr,
		"classification: %d matched, %d unmatched, %d transferred, %d pending\n",
		counts.Matched, counts.Unmatched, counts.Transferred, counts.Pending)
}

func toJSON(res oracle.Result) showJSON {
	out := showJSON{
		Totals:     totalsJSON{Rows: []totalsRowJSON{}},
		Statistics: statsJSON{Rows: []statsRowJSON{}, Categories: []string{}},
		// The reference reports the matched list in attribution order.
		MatchedTxnIDs:  []string{},
		UnmatchedCount: len(res.Unmatched),
	}

	for _, r := range res.Totals.Rows {
		out.Totals.Rows = append(out.Totals.Rows, totalsRowJSON{
			PerformanceDate: r.PerformanceDate,
			Transactions:    r.Transactions,
			Gross:           hexf(r.Gross),
			Fees:            hexf(r.Fees),
			Net:             hexf(r.Net),
		})
	}
	if len(res.Totals.Rows) > 0 {
		t := res.Totals.Total
		out.Totals.Total = &totalsRowJSON{
			PerformanceDate: t.PerformanceDate,
			Transactions:    t.Transactions,
			Gross:           hexf(t.Gross),
			Fees:            hexf(t.Fees),
			Net:             hexf(t.Net),
		}
	}

	if res.Statistics.Categories != nil {
		out.Statistics.Categories = res.Statistics.Categories
	}
	for _, r := range res.Statistics.Rows {
		out.Statistics.Rows = append(out.Statistics.Rows, statsRowJSON{
			PerformanceDate: r.PerformanceDate,
			TotalTickets:    r.TotalTickets,
			ByCategory:      r.ByCategory,
		})
	}
	if len(res.Statistics.Rows) > 0 {
		t := res.Statistics.Total
		out.Statistics.Total = &statsRowJSON{
			PerformanceDate: t.PerformanceDate,
			TotalTickets:    t.TotalTickets,
			ByCategory:      t.ByCategory,
		}
	}

	for _, tx := range res.MatchedTxns {
		out.MatchedTxnIDs = append(out.MatchedTxnIDs, tx.TxnID)
	}

	return out
}

// loadFromFiles is the P1 path: the dashboard's canonical cache, straight off
// disk.
func loadFromFiles(dir string) ([]model.CanonicalTicket, []model.PayPalTxn, error) {
	snap, err := snapshot.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	return snap.Tickets, snap.Txns, nil
}

// loadFromDB reads the same data back out of Postgres and re-assembles the
// canonical frame at read time, as the service does in production.
//
// If this returns values equal to loadFromFiles, the round trip through jsonb
// preserved everything — including float bits and provider array order.
func loadFromDB(dsn string) ([]model.CanonicalTicket, []model.PayPalTxn, error) {
	if dsn == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, nil, err
		}
		dsn = cfg.DatabaseURL
	}
	if dsn == "" {
		return nil, nil, fmt.Errorf("no database: pass -database-url or set DATABASE_URL")
	}

	ctx := context.Background()
	st, err := store.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	defer st.Close()

	rows, err := st.LoadResources(ctx)
	if err != nil {
		return nil, nil, err
	}

	inputs, err := ingest.DecodeResources(rows)
	if err != nil {
		return nil, nil, err
	}

	return model.Assemble(inputs.Orders, inputs.Tickets, inputs.Events), inputs.Txns, nil
}

// distinctShows lists the shows present, in first-seen order.
func distinctShows(tickets []model.CanonicalTicket) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range tickets {
		if !seen[t.Show] {
			seen[t.Show] = true
			out = append(out, t.Show)
		}
	}
	return out
}
