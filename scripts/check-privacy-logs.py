#!/usr/bin/env python3
"""check-privacy-logs: fail if non-test Go code logs a forbidden field.

Privacy gate for log calls (same pattern as kenaz's gate). A log call
is a line calling slog / a *slog.Logger method (Debug, Info, Warn, Error,
Log, With). A violation is a string-literal key on such a line that names a
secret or personal field. logx's redacting handler is defense in depth;
this gate keeps the call sites clean in the first place.

Usage: check-privacy-logs.py [ROOT]
"""
import pathlib
import re
import sys

FORBIDDEN = re.compile(
    r'"(?:[a-z_]*?)(token|secret|password|authorization|cookie|email|claims?|jwt|'
    r'hostname|prompt|content|arguments|tool_input|tool_output|cwd)(?:[a-z_]*)"',
    re.IGNORECASE,
)
LOG_CALL = re.compile(r'\b(?:slog|log|logger|l|d\.log|s\.log|link\.log)\.(?:Debug|Info|Warn|Error|Log|With)\(')


def scan(root: pathlib.Path) -> list[str]:
    bad = []
    for path in sorted(root.rglob("*.go")):
        if path.name.endswith("_test.go") or "/vendor/" in str(path) or "/testdata/" in str(path):
            continue
        for n, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if LOG_CALL.search(line) and FORBIDDEN.search(line):
                bad.append(f"{path.relative_to(root)}:{n}: {line.strip()}")
    return bad


def main() -> int:
    root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
    bad = scan(root)
    for b in bad:
        print("forbidden log field:", b)
    if bad:
        print(f"check-privacy-logs: {len(bad)} violation(s)")
        return 1
    print("check-privacy-logs: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
