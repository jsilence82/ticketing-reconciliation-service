package main

import (
	"testing"
	"time"
)

// TestPayPalWindow_DefaultStartSurvivesMidnightTruncation reproduces the bug
// reported against a real backfill run: fetchPage sends start_date truncated
// to midnight UTC on the calendar date (start.Format("2006-01-02") +
// "T00:00:00+00:00"), not the exact instant payPalWindow computed. Computing
// "exactly 3 years before now" and then truncating to midnight moves the
// sent value further into the past by however many hours past midnight UTC
// it currently is — which PayPal's Transaction Search rejected as "before
// the allowed range of 3 years" for any run after 00:00:00 UTC.
func TestPayPalWindow_DefaultStartSurvivesMidnightTruncation(t *testing.T) {
	// A time deliberately far from midnight UTC, so the bug (had it still
	// been present) would reproduce regardless of when this test runs.
	now := time.Date(2026, 8, 2, 23, 59, 0, 0, time.UTC)
	trueBoundary := now.AddDate(-3, 0, 0)

	start, end, err := payPalWindow(now, "", "")
	if err != nil {
		t.Fatalf("payPalWindow: %v", err)
	}
	if !end.Equal(now) {
		t.Fatalf("end = %v, want %v", end, now)
	}

	// Reproduce fetchPage's exact formatting (internal/importer/paypal.go)
	// rather than asserting on the pre-truncation value, since the
	// pre-truncation value was never the actual bug.
	sentStart, err := time.Parse(time.RFC3339, start.Format("2006-01-02")+"T00:00:00+00:00")
	if err != nil {
		t.Fatalf("parse formatted start: %v", err)
	}

	if !sentStart.After(trueBoundary) {
		t.Errorf("start_date PayPal would receive (%s) is not after the true "+
			"3-year boundary (%s) — this is the exact rejection the backfill hit",
			sentStart.Format(time.RFC3339), trueBoundary.Format(time.RFC3339))
	}
}

func TestPayPalWindow_ExplicitFromAndTo(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	start, end, err := payPalWindow(now, "2026-01-01", "2026-06-01")
	if err != nil {
		t.Fatalf("payPalWindow: %v", err)
	}
	wantStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
}

func TestPayPalWindow_FromAfterToIsAnError(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	if _, _, err := payPalWindow(now, "2026-06-01", "2026-01-01"); err == nil {
		t.Fatal("want an error when --from is after --to, got nil")
	}
}
