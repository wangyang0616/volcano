#!/usr/bin/env bash
#
# One-click test runner for the repack (defragmentation) package.
#
# Runs the package's unit tests fully offline using the repo's vendored deps
# (-mod=vendor), so it does NOT touch proxy.golang.org and works behind a
# restricted network. -count=1 disables the test cache (always a fresh run).
#
# Usage:
#   ./run_tests.sh              # quiet-ish, summary only
#   ./run_tests.sh -v           # verbose: per-test PASS/FAIL
#   ./run_tests.sh -run TestConsolidate_FreesTwoNodes   # single test (any `go test` flag)
#   COVER=1 ./run_tests.sh      # also print coverage and write coverage.out
#
set -uo pipefail

# Resolve the package dir (where this script lives) and the module root.
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$PKG_DIR" && git rev-parse --show-toplevel 2>/dev/null || echo "$PKG_DIR")"
PKG="./pkg/repackengine/..."

if ! command -v go >/dev/null 2>&1; then
  echo "error: 'go' not found on PATH. Install Go 1.25+ (matches go.mod) and retry." >&2
  exit 127
fi

# Colors (fall back to plain when not a TTY).
if [ -t 1 ]; then GREEN=$'\033[32m'; RED=$'\033[31m'; BOLD=$'\033[1m'; RST=$'\033[0m'; else GREEN=; RED=; BOLD=; RST=; fi

FLAGS=(-mod=vendor -count=1)
if [ "${COVER:-0}" = "1" ]; then
  FLAGS+=(-coverprofile="$PKG_DIR/coverage.out")
fi

echo "${BOLD}go version:${RST} $(go version)"
echo "${BOLD}running:${RST} go test ${FLAGS[*]} $* $PKG"
echo

cd "$ROOT"
# Stream output; tee so we can both show it and inspect the result.
set -o pipefail
go test "${FLAGS[@]}" "$@" "$PKG" 2>&1 | tee /tmp/repack_test.out
status=${PIPESTATUS[0]}

echo
if [ "$status" -eq 0 ]; then
  echo "${GREEN}${BOLD}PASS${RST} — all repack tests green."
  if [ "${COVER:-0}" = "1" ]; then
    echo "coverage profile: $PKG_DIR/coverage.out  (go tool cover -html=$PKG_DIR/coverage.out)"
    go test -mod=vendor -cover "$PKG" 2>/dev/null | grep -E 'coverage:' || true
  fi
else
  echo "${RED}${BOLD}FAIL${RST} — failing lines:"
  grep -nE '^(--- FAIL|FAIL|\s+.*_test\.go:[0-9]+:|panic:)' /tmp/repack_test.out | sed 's/^/  /' || true
fi
exit "$status"
