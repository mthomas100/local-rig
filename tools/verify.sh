#!/bin/sh
# verify.sh - the DETERMINISTIC gate of the plan -> code -> verify loop.
#
# Runs the project's own checks and writes their output VERBATIM to verify.log
# in the project root. Exit 0 = PASS, 1 = FAIL, 2 = no verifier detected.
# No model is involved, by design: self-review by a model rubber-stamps at a
# steady rate (arXiv 2606.28438), while fixed localize/repair/validate
# pipelines beat agentic loops at lower cost (Agentless, arXiv 2407.01489).
#
# Usage: verify.sh [project-dir]        (default: current directory)
#
# What runs, in this order; the first matching source of commands wins:
#   1. .rig-verify in the project root: one shell command per line, '#' comments.
#      This is the escape hatch for anything below that guesses wrong.
#   2. Makefile with a verify / check / test target  -> make <target>
#   3. package.json  -> npm run typecheck, lint, test, build (each only if defined)
#   4. pyproject.toml / setup.py / pytest.ini -> ruff check . (if installed),
#      mypy (only if [tool.mypy] is configured), pytest -q or unittest discover
#   5. Cargo.toml    -> cargo build, cargo clippy (if installed), cargo test
#   6. go.mod        -> go vet ./..., go build ./..., go test ./...
#   7. shell scripts at the root -> sh -n each, shellcheck if installed
# Exit 2 when nothing matched: an unverifiable change is NOT a passing one, and
# the coder is told to add a .rig-verify rather than to assume success.
set -u

DIR=${1:-.}
cd "$DIR" || { echo "verify.sh: cannot cd to $DIR" >&2; exit 2; }
LOG=verify.log
: > "$LOG"

have() { command -v "$1" >/dev/null 2>&1; }

CMDS=""
add() { CMDS="$CMDS$1
"; }

if [ -f .rig-verify ]; then
    while IFS= read -r line; do
        case "$line" in ''|'#'*) continue ;; esac
        add "$line"
    done < .rig-verify
elif [ -f Makefile ] && grep -qE '^(verify|check|test):' Makefile; then
    for t in verify check test; do
        grep -qE "^$t:" Makefile && { add "make $t"; break; }
    done
elif [ -f package.json ]; then
    for s in typecheck lint test build; do
        if python3 -c "import json,sys; sys.exit(0 if '$s' in json.load(open('package.json')).get('scripts',{}) else 1)"; then
            add "npm run --silent $s"
        fi
    done
elif [ -f pyproject.toml ] || [ -f setup.py ] || [ -f pytest.ini ]; then
    have ruff && add "ruff check ."
    [ -f pyproject.toml ] && grep -q '^\[tool.mypy\]' pyproject.toml && have mypy && add "mypy ."
    # pytest when importable, else the stdlib runner: a missing module must
    # not read as a failing test suite.
    if python3 -c "import pytest" >/dev/null 2>&1; then
        add "python3 -m pytest -q"
    else
        add "python3 -m unittest discover -v"
    fi
elif [ -f Cargo.toml ]; then
    add "cargo build"
    cargo clippy --version >/dev/null 2>&1 && add "cargo clippy -- -D warnings"
    add "cargo test"
elif [ -f go.mod ]; then
    add "go vet ./..."
    add "go build ./..."
    add "go test ./..."
else
    for f in ./*.sh; do
        [ -f "$f" ] || continue
        add "sh -n $f"
        have shellcheck && add "shellcheck $f"
    done
    for f in ./*; do
        [ -f "$f" ] && [ -x "$f" ] && head -1 "$f" | grep -qE '^#!.*\b(sh|bash|zsh)\b' && ! expr "$f" : '.*\.sh$' >/dev/null && add "sh -n $f"
    done
fi

if [ -z "$CMDS" ]; then
    echo "NO VERIFIER DETECTED in $(pwd): add a .rig-verify file (one command per line)" | tee "$LOG"
    exit 2
fi

FAIL=0
printf '%s' "$CMDS" | while IFS= read -r c; do
    [ -n "$c" ] || continue
    printf '\n$ %s\n' "$c" >> "$LOG"
    if sh -c "$c" >> "$LOG" 2>&1; then
        printf '=> ok\n' >> "$LOG"
    else
        rc=$?
        printf '=> FAILED (exit %s)\n' "$rc" >> "$LOG"
        echo "FAILMARK" >> "$LOG.rc"
    fi
done
if [ -f "$LOG.rc" ]; then FAIL=1; rm -f "$LOG.rc"; fi

if [ "$FAIL" = 0 ]; then
    echo "verify: PASS ($(printf '%s' "$CMDS" | grep -c .) command(s), log: $LOG)"
    exit 0
else
    echo "verify: FAIL - see $LOG"
    grep -n -B3 '=> FAILED' "$LOG" | head -40
    exit 1
fi
