#!/usr/bin/env python3
"""Offline structural audit of tokens.sqlite against the Fielin mint formula.

deviceToken = base64(tF # Q # w # tC # p)
  Q  = uuid1 + "-h-" + nowMillis + "-" + uuid2   (32-hex uuids, no dashes)
  p  = md5("tF#Q#w#tC#daye,raolewoba!")          (hex, lowercase)

The formula was verified byte-exact against harvested tokens on 2026-09-02
(mint-replay-full probe PASS live). This script checks every row WITHOUT
consuming anything: a token whose rebuilt bytes differ from the stored bytes
is malformed/corrupted (or was harvested with a different secret revision).
"""
import base64
import hashlib
import re
import sqlite3
import sys

SECRET = "daye,raolewoba!"


def parse_token(raw: str):
    """Return (ok, reason, fields) for a single base64 device token."""
    try:
        dec = base64.b64decode(raw)
    except Exception as e:
        return False, f"b64decode: {e}", None
    parts = dec.split(b"#")
    if len(parts) != 5:
        return False, f"expected 5 #-fields, got {len(parts)}", None
    tf, q, w, tc, p = (x.decode("utf-8", "replace") for x in parts)
    if tf != "SG_WEB":
        return False, f"tF={tf!r} != 'SG_WEB'", None
    m = re.match(r"^([0-9a-f]{32})-h-(\d{13})-([0-9a-f]{32})$", q)
    if not m:
        return False, f"Q malformed: {q[:40]!r}...", None
    uuid1, ts, uuid2 = m.group(1), m.group(2), m.group(3)
    if not re.match(r"^\d+$", tc):
        return False, f"tC non-numeric: {tc[:20]!r}", None
    expect = hashlib.md5(
        "#".join([tf, q, w, tc, SECRET]).encode()
    ).hexdigest()
    if p != expect:
        return False, f"MD5 mismatch (stored={p[:12]}… recalced={expect[:12]}…)", None
    return True, "ok", {
        "uuid1": uuid1, "ts": ts, "uuid2": uuid2,
        "w_len": len(w), "tC": tc, "p": p,
    }


def main():
    db = sys.argv[1] if len(sys.argv) > 1 else "tokens.sqlite"
    con = sqlite3.connect(db)
    rows = con.execute("SELECT id, token FROM tokens ORDER BY id").fetchall()
    total = len(rows)
    if total == 0:
        print(f"{db}: empty")
        return

    bad = []
    ok_count = 0
    tC_values = {}
    w_lens = {}
    uuid1_seen = {}
    ts_min, ts_max = None, None
    w_empty = 0

    for tid, raw in rows:
        ok, reason, f = parse_token(raw)
        if not ok:
            bad.append((tid, reason))
            continue
        ok_count += 1
        tC_values[f["tC"]] = tC_values.get(f["tC"], 0) + 1
        w_lens[f["w_len"]] = w_lens.get(f["w_len"], 0) + 1
        if f["w_len"] == 0:
            w_empty += 1
        uuid1_seen[f["uuid1"]] = uuid1_seen.get(f["uuid1"], 0) + 1
        ts = int(f["ts"])
        if ts_min is None or ts < ts_min:
            ts_min = ts
        if ts_max is None or ts > ts_max:
            ts_max = ts

    print(f"DB: {db}  rows={total}")
    print(f"  structural PASS: {ok_count}  FAIL: {len(bad)}")
    if ts_min:
        import datetime
        fmt = lambda ms: datetime.datetime.fromtimestamp(
            ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%SZ")
        print(f"  token ts range : {fmt(ts_min)} … {fmt(ts_max)}")
    print(f"  tC distribution: {dict(sorted(tC_values.items(), key=lambda kv: -kv[1])[:8])}")
    top_w = dict(sorted(w_lens.items(), key=lambda kv: -kv[1])[:6])
    print(f"  w-blob length distribution (top): {top_w}")
    print(f"  w-blob empty: {w_empty}")
    dup = sum(1 for v in uuid1_seen.values() if v > 1)
    print(f"  distinct uuid1 (devices): {len(uuid1_seen)}  reused: {dup}")
    if bad:
        print(f"  -- first 10 bad rows --")
        for tid, reason in bad[:10]:
            print(f"    id={tid}: {reason}")
        with open("bad_tokens.txt", "w") as fh:
            for tid, reason in bad:
                fh.write(f"{tid}\t{reason}\n")
        print(f"  full list: bad_tokens.txt")


if __name__ == "__main__":
    main()
