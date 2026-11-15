#!/usr/bin/env bash
# Prints the installed-tier conformance evidence in test/conformance/evidence/installed
# as a Markdown summary (for a CI job summary): one row per cell this runner
# ran, then every step that failed or was not run. It reads evidence only and
# never fails the job; the run and verdict steps decide that.
set -uo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
dir="${1:-$root/test/conformance/evidence/installed}"
PYTHON="${PYTHON:-python3}"
command -v "$PYTHON" >/dev/null 2>&1 || PYTHON=python
"$PYTHON" - "$dir" <<'PY' || true
import glob, json, os, sys
d = sys.argv[1]
rows, notes, reduced = [], [], []
for f in sorted(glob.glob(os.path.join(d, "*__*.json"))):
    e = json.load(open(f))
    r = e.get("installed")
    if e.get("tier") != "installed" or r is None:
        continue
    steps = r["steps"]
    n = {s: sum(1 for x in steps if x["status"] == s) for s in ("pass", "fail", "not_run")}
    host = r["host_cli"]
    # A pass is a full installed-tier pass only when every step ran and the
    # runner is the cell's target OS. Anything else is "pass (reduced)".
    not_run = [x["id"] for x in steps if x["status"] == "not_run"]
    os_note = e.get("os_note", "") if e.get("os_match", "exact") != "exact" else ""
    verdict = e["verdict"]
    if verdict == "pass" and (not_run or os_note):
        verdict = "pass (reduced)"
        why = []
        if not_run:
            why.append("%d step(s) not run: %s" % (len(not_run), ", ".join(not_run)))
        if os_note:
            why.append(os_note)
        reduced.append("- `%s`: %s" % (e["cell"], "; ".join(why)))
    rows.append("| %s | %s | %d | %d | %d | %s | %s | %s | `%s` |" % (
        e["cell"], verdict, n["pass"], n["fail"], n["not_run"], e.get("os_observed", ""),
        host.get("version") or "absent", host["visibility"], r["build"]["tags"] or "default"))
    for x in steps:
        if x["status"] != "pass":
            notes.append("- `%s` **%s** %s: %s" % (e["cell"], x["id"], x["status"], x.get("detail", "")))
print("### Installed-tier conformance (credential-free; not a live pass, not a release verdict)\n")
if not rows:
    print("No installed-tier evidence was written on this runner.")
    sys.exit(0)
print("| cell | installed tier | passed | failed | not run | runner OS | host CLI | host visibility | build tags |")
print("|---|---|---|---|---|---|---|---|---|")
print("\n".join(rows))
full = sum(1 for r in rows if "| pass |" in r)
print("\n%d full pass, %d pass (reduced), %d other. A reduced pass is not a full installed-tier pass: every step that ran passed, but steps were not run or the runner is not the cell's target OS." % (
    full, len(reduced), len(rows) - full - len(reduced)))
if reduced:
    print("\nReduced passes:\n")
    print("\n".join(reduced))
if notes:
    print("\nSteps that failed or were not run:\n")
    print("\n".join(notes))
PY
