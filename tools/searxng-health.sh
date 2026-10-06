#!/bin/sh
# searxng-health.sh — is the local SearXNG answering, and which engines are
# actually returning results right now?
#
# Usage: tools/searxng-health.sh [query]      (default: two canary queries)
# Exit:  0 healthy   1 degraded (up, but fewer than 2 engines returned
#        results)   2 down (no answer on the port)
# Env:   SEARXNG_URL (default http://127.0.0.1:8080)
#
# Each run sends one query per canary to every enabled general engine, so do
# not loop it: upstream engines rate-limit bursts, and SearXNG then suspends
# them for a few minutes.
exec python3 - "$@" <<'PY'
import json, os, sys, time, urllib.parse, urllib.request
from collections import Counter

base = os.environ.get("SEARXNG_URL", "http://127.0.0.1:8080").rstrip("/")
queries = sys.argv[1:] or ["python programming language", "github releases"]

try:
    urllib.request.urlopen(base + "/healthz", timeout=5).read()
except Exception as exc:
    print(f"DOWN  {base} does not answer ({type(exc).__name__}). "
          "Start it: launchctl kickstart -k gui/$(id -u)/com.example.local-rig.searxng")
    sys.exit(2)

engines, dead, totals, times = Counter(), {}, [], []
for q in queries:
    url = base + "/search?" + urllib.parse.urlencode({"q": q, "format": "json"})
    t0 = time.time()
    try:
        data = json.load(urllib.request.urlopen(url, timeout=30))
    except Exception as exc:
        print(f"DEGRADED  query {q!r} failed: {type(exc).__name__}: {exc}")
        sys.exit(1)
    times.append(time.time() - t0)
    results = data.get("results", [])
    totals.append(len(results))
    for r in results:
        for e in r.get("engines") or []:
            engines[e] += 1
    for name, reason in data.get("unresponsive_engines") or []:
        dead[name] = reason

working = sorted(engines)
state = "OK" if len(working) >= 2 else "DEGRADED"
print(f"{state}  {base}  results/query={totals}  slowest={max(times):.1f}s")
print(f"  answering: {', '.join(f'{e}({engines[e]})' for e in working) or 'none'}")
if dead:
    print(f"  not answering: {', '.join(f'{k} [{v}]' for k, v in sorted(dead.items()))}")
sys.exit(0 if state == "OK" else 1)
PY
