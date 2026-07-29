#!/usr/bin/env python3
"""Run the UNMODIFIED reference reconciliation and dump its output as JSON.

This is the generator half of the parity harness. It imports
reference/ssg_dashboard/core/reconciliation.py as-is — no edits, no
monkeypatching — loads the same canonical cache the dashboard loads, and emits
the Totals and Statistics sheets for every show.

Usage:
    python tools/parity/driver.py <data-dir> <output.json>

<data-dir> is the out-of-tree directory holding ssg_cache.json and
paypal_cache.json. It is NEVER inside this repository: that data is live and
carries buyer PII (CLAUDE.md).

Every float is emitted as a hex literal, so the comparison against Go is over
exact IEEE-754 bits rather than a decimal rendering of them.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pandas as pd

REPO = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(REPO / "reference" / "ssg_dashboard"))

# Imported unmodified. core/reconciliation.py depends only on pandas, so no
# streamlit stub is needed.
from core.reconciliation import build_reconciliation  # noqa: E402


def load_canonical(data_dir: Path) -> pd.DataFrame:
    """Reproduce persistence/canonical.py load_cache for the canonical frame."""
    payload = json.loads((data_dir / "ssg_cache.json").read_text(encoding="utf-8"))
    df = pd.DataFrame(payload.get("records", []))

    for col in ("date", "performance_date"):
        if col in df.columns:
            df[col] = pd.to_datetime(df[col], errors="coerce", utc=True)
    for col in ("quantity", "revenue"):
        if col in df.columns:
            df[col] = pd.to_numeric(df[col], errors="coerce").fillna(0)

    return df


def load_paypal(data_dir: Path) -> list[dict]:
    payload = json.loads((data_dir / "paypal_cache.json").read_text(encoding="utf-8"))
    return payload.get("transactions", [])


def hexf(v) -> str:
    return float(v).hex()


def totals_to_json(df: pd.DataFrame) -> dict:
    if df.empty:
        return {"rows": [], "total": None}

    body = df.drop(index="TOTAL", errors="ignore")
    rows = [
        {
            "performance_date": r["Performance Date"],
            "transactions": int(r["Transactions"]),
            "gross": hexf(r["Gross (€)"]),
            "fees": hexf(r["Fees (€)"]),
            "net": hexf(r["Net (€)"]),
        }
        for _, r in body.iterrows()
    ]

    total = None
    if "TOTAL" in df.index:
        t = df.loc["TOTAL"]
        total = {
            "performance_date": t["Performance Date"],
            "transactions": int(t["Transactions"]),
            "gross": hexf(t["Gross (€)"]),
            "fees": hexf(t["Fees (€)"]),
            "net": hexf(t["Net (€)"]),
        }

    return {"rows": rows, "total": total}


def stats_to_json(df: pd.DataFrame) -> dict:
    if df.empty:
        return {"rows": [], "total": None, "categories": []}

    categories = [c for c in df.columns if c not in ("Performance Date", "Total Tickets")]
    body = df.drop(index="TOTAL", errors="ignore")

    rows = [
        {
            "performance_date": r["Performance Date"],
            "total_tickets": int(r["Total Tickets"]),
            "by_category": {c: int(r[c]) for c in categories},
        }
        for _, r in body.iterrows()
    ]

    total = None
    if "TOTAL" in df.index:
        t = df.loc["TOTAL"]
        total = {
            "performance_date": t["Performance Date"],
            "total_tickets": int(t["Total Tickets"]),
            "by_category": {c: int(t[c]) for c in categories},
        }

    return {"rows": rows, "total": total, "categories": categories}


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2

    data_dir = Path(sys.argv[1]).expanduser()
    out_path = Path(sys.argv[2])

    df = load_canonical(data_dir)
    txns = load_paypal(data_dir)

    shows = sorted(df["show"].dropna().unique().tolist())

    results = {}
    for show in shows:
        totals_df, stats_df, unmatched_df, show_txns = build_reconciliation(df, txns, show_filter=show)
        results[show] = {
            "totals": totals_to_json(totals_df),
            "statistics": stats_to_json(stats_df),
            "matched_txn_ids": [t.get("txn_id", "") for t in show_txns],
            "unmatched_count": 0 if unmatched_df.empty else len(unmatched_df),
        }

    payload = {
        "env": {
            "python": sys.version.split()[0],
            "pandas": pd.__version__,
        },
        "record_count": int(len(df)),
        "txn_count": len(txns),
        "shows": shows,
        "results": results,
    }

    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w") as fh:
        json.dump(payload, fh, indent=1, sort_keys=True)
        fh.write("\n")

    print(f"wrote {out_path}")
    print(f"  {len(df)} records, {len(txns)} transactions, {len(shows)} shows")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
