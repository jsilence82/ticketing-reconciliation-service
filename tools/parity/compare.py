#!/usr/bin/env python3
"""Diff the Python reference output against the Go output, cell by cell.

Comparison is EXACT: hex float literals must match bit for bit, integers and
labels must match exactly, and row order must match. There is deliberately no
epsilon — an epsilon would hide precisely the rounding and summation bugs this
harness exists to catch (CLAUDE.md guardrail 6).

Usage:
    python tools/parity/compare.py <python.json> <go.json>

Exits non-zero if anything differs.
"""

from __future__ import annotations

import json
import struct
import sys
from pathlib import Path

MAX_SHOWN = 25


def bits(hexstr: str) -> bytes:
    """Return the exact IEEE-754 bit pattern of a hex float literal.

    Comparing the hex STRINGS directly does not work: Python's float.hex()
    emits a full 13-digit mantissa and an unpadded exponent (0x1.eb00000000000p+8)
    while Go's strconv emits the shortest mantissa and a zero-padded exponent
    (0x1.ebp+08). Those are the same number. Both languages parse either form,
    so decode to a float and compare bits — which also keeps +0.0 distinct
    from -0.0.
    """
    return struct.pack("<d", float.fromhex(hexstr))


def same(a: str, b: str) -> bool:
    return bits(a) == bits(b)


def fnum(hexstr: str) -> str:
    """Render a hex float as decimal for human-readable diff output."""
    try:
        return f"{float.fromhex(hexstr):.4f}"
    except (ValueError, AttributeError):
        return str(hexstr)


def compare(py: dict, go: dict) -> tuple[list[str], int]:
    diffs: list[str] = []
    cells = 0

    def note(msg: str) -> None:
        diffs.append(msg)

    def cmp_eq(a, b, msg: str) -> None:
        """Compare two exact values, counting the comparison."""
        nonlocal cells
        cells += 1
        if a != b:
            note(msg)

    def cmp_float(a: str, b: str, msg_fn) -> None:
        """Compare two hex floats bit-exactly, counting the comparison."""
        nonlocal cells
        cells += 1
        if not same(a, b):
            note(msg_fn())

    if py["record_count"] != go["record_count"]:
        note(f"record_count: python={py['record_count']} go={go['record_count']}")
    if py["txn_count"] != go["txn_count"]:
        note(f"txn_count: python={py['txn_count']} go={go['txn_count']}")
    if py["shows"] != go["shows"]:
        note(f"shows differ:\n  python={py['shows']}\n  go={go['shows']}")

    for show in py["shows"]:
        p = py["results"].get(show)
        g = go["results"].get(show)
        if g is None:
            note(f"[{show}] missing from go output")
            continue

        # --- Totals ---
        pr, gr = p["totals"]["rows"], g["totals"]["rows"]
        if len(pr) != len(gr):
            note(f"[{show}] totals row count: python={len(pr)} go={len(gr)}")
        for i, (a, b) in enumerate(zip(pr, gr)):
            cmp_eq(a["performance_date"], b["performance_date"],
                   f"[{show}] totals[{i}] label: "
                   f"python={a['performance_date']!r} go={b['performance_date']!r}")
            cmp_eq(a["transactions"], b["transactions"],
                   f"[{show}] totals[{i}] ({a['performance_date']}) transactions: "
                   f"python={a['transactions']} go={b['transactions']}")
            for field in ("gross", "fees", "net"):
                cmp_float(a[field], b[field],
                          lambda f=field, a=a, b=b, i=i:
                          f"[{show}] totals[{i}] ({a['performance_date']}) {f}: "
                          f"python={fnum(a[f])} go={fnum(b[f])}  [{a[f]} vs {b[f]}]")

        pt, gt = p["totals"]["total"], g["totals"]["total"]
        if (pt is None) != (gt is None):
            note(f"[{show}] totals TOTAL presence: python={pt is not None} go={gt is not None}")
        elif pt is not None:
            cmp_eq(pt["transactions"], gt["transactions"],
                   f"[{show}] TOTAL transactions: "
                   f"python={pt['transactions']} go={gt['transactions']}")
            for field in ("gross", "fees", "net"):
                cmp_float(pt[field], gt[field],
                          lambda f=field: f"[{show}] TOTAL {f}: "
                          f"python={fnum(pt[f])} go={fnum(gt[f])}  [{pt[f]} vs {gt[f]}]")

        # --- Statistics ---
        if p["statistics"]["categories"] != g["statistics"]["categories"]:
            note(f"[{show}] categories differ:\n"
                 f"  python={p['statistics']['categories']}\n"
                 f"  go={g['statistics']['categories']}")

        psr, gsr = p["statistics"]["rows"], g["statistics"]["rows"]
        if len(psr) != len(gsr):
            note(f"[{show}] statistics row count: python={len(psr)} go={len(gsr)}")
        for i, (a, b) in enumerate(zip(psr, gsr)):
            cmp_eq(a["performance_date"], b["performance_date"],
                   f"[{show}] stats[{i}] label: "
                   f"python={a['performance_date']!r} go={b['performance_date']!r}")
            cmp_eq(a["total_tickets"], b["total_tickets"],
                   f"[{show}] stats[{i}] ({a['performance_date']}) total_tickets: "
                   f"python={a['total_tickets']} go={b['total_tickets']}")
            for cat in sorted(set(a["by_category"]) | set(b["by_category"])):
                cmp_eq(a["by_category"].get(cat), b["by_category"].get(cat),
                       f"[{show}] stats[{i}] ({a['performance_date']}) "
                       f"category {cat!r}: python={a['by_category'].get(cat)} "
                       f"go={b['by_category'].get(cat)}")

        pst, gst = p["statistics"]["total"], g["statistics"]["total"]
        if (pst is None) != (gst is None):
            note(f"[{show}] stats TOTAL presence: python={pst is not None} go={gst is not None}")
        elif pst is not None:
            if pst["total_tickets"] != gst["total_tickets"]:
                note(f"[{show}] stats TOTAL total_tickets: "
                     f"python={pst['total_tickets']} go={gst['total_tickets']}")
            if pst["by_category"] != gst["by_category"]:
                note(f"[{show}] stats TOTAL by_category differs")

        # --- matched transactions ---
        if p["matched_txn_ids"] != g["matched_txn_ids"]:
            pset, gset = set(p["matched_txn_ids"]), set(g["matched_txn_ids"])
            if pset == gset:
                note(f"[{show}] matched transactions: same set, different ORDER "
                     f"({len(pset)} ids)")
            else:
                note(f"[{show}] matched transactions differ: "
                     f"python={len(pset)} go={len(gset)}, "
                     f"python-only={len(pset - gset)} go-only={len(gset - pset)}")

        if p["unmatched_count"] != g["unmatched_count"]:
            note(f"[{show}] unmatched_count: "
                 f"python={p['unmatched_count']} go={g['unmatched_count']}")

    return diffs, cells


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2

    py = json.loads(Path(sys.argv[1]).read_text())
    go = json.loads(Path(sys.argv[2]).read_text())

    diffs, cells = compare(py, go)

    if not diffs:
        print("PARITY PASS")
        print(f"  {len(py['shows'])} shows, {py['record_count']} records, "
              f"{py['txn_count']} transactions")
        print(f"  {cells} cells compared, exact bit equality, zero differences")
        return 0

    print(f"PARITY FAIL: {len(diffs)} of {cells} cells differ\n")
    for d in diffs[:MAX_SHOWN]:
        print(f"  {d}")
    if len(diffs) > MAX_SHOWN:
        print(f"  ... and {len(diffs) - MAX_SHOWN} more")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
