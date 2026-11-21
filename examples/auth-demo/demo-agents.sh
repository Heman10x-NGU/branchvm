#!/usr/bin/env bash
# Deterministic offline 3-branch agent simulation for the auth demo.
# BRANCH_INDEX=1: no fix, the race test fails.
# BRANCH_INDEX=2: applies fixes/branch2.patch (ON CONFLICT DO NOTHING).
# BRANCH_INDEX=3: applies fixes/branch3.patch (map unique violation 23505).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
idx="${BRANCH_INDEX:-}"
case "$idx" in
  1|2|3) ;;
  *) echo "usage: BRANCH_INDEX=1|2|3 $0" >&2; exit 2 ;;
esac

if [[ "${BRANCHVM_USE_CLAUDE:-0}" == "1" ]] && command -v claude >/dev/null 2>&1; then
  echo "real agent run would use:"
  echo "  cd $here && claude -p 'Run go test ./... and fix the failing test TestSignupConcurrentDuplicate without editing tests.' --allowedTools 'Bash(go test:*),Edit,Read'"
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -R "$here/." "$work/"
cd "$work"

if [[ "$idx" != "1" ]]; then
  git apply "fixes/branch${idx}.patch"
  echo "branch $idx: applied fixes/branch${idx}.patch"
fi

set +e
out="$(go test -mod=vendor -count=1 -v ./... 2>&1)"
rc=$?
set -e
echo "$out" | grep -E '^(--- |FAIL|ok|PASS)' || true

pass="$(echo "$out" | grep -c '^--- PASS' || true)"
fail="$(echo "$out" | grep -c '^--- FAIL' || true)"
skip="$(echo "$out" | grep -c '^--- SKIP' || true)"
total=$((pass + fail + skip))

if [[ "$skip" -gt 0 ]]; then
  echo "VERDICT branch $idx: SKIPPED ($skip/$total skipped, Postgres unreachable at DATABASE_URL)"
  exit 3
fi
if [[ "$rc" -ne 0 ]]; then
  echo "VERDICT branch $idx: FAIL ($pass/$total passed, $fail failed)"
  exit 1
fi
echo "VERDICT branch $idx: PASS ($pass/$total passed)"
