#!/usr/bin/env bash
# Records the branchvm demo inside the fc-lab VM: init (only when no golden
# snapshot exists), a 3-branch run of examples/auth-demo, apply of branch 2,
# the resulting diffstat, and the isolation verifier.
#
# Usage, from the repo root inside the lab VM:
#   scripts/record-demo.sh [out.cast]
#
# With asciinema installed the run is recorded to out.cast (default
# demo.cast); without it the demo just runs in the terminal. The 60-second
# pacing assumes the golden snapshot already exists; a first init with the
# warm build takes several minutes.
#
# Environment: BRANCHVM (binary, default ./branchvm, built when missing),
# DEMO_WARM (init warm command), DEMO_PAUSE (seconds between steps).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEMO_DIR="$ROOT/examples/auth-demo"
BIN="${BRANCHVM:-$ROOT/branchvm}"
CAST="${1:-demo.cast}"
PAUSE="${DEMO_PAUSE:-2}"
WARM="${DEMO_WARM:-go test -run ^\$ ./...}"
GOLDEN=/var/lib/branchvm/pool/golden/snapshot

if [[ -z "${BVM_DEMO_INNER:-}" ]] && command -v asciinema >/dev/null 2>&1; then
  echo "recording to $CAST with asciinema"
  exec asciinema rec --overwrite --idle-time-limit 2 \
    --title "branchvm: 3 Firecracker branches of a Postgres app" \
    -c "BVM_DEMO_INNER=1 BRANCHVM='$BIN' DEMO_PAUSE='$PAUSE' DEMO_WARM='$WARM' '${BASH_SOURCE[0]}'" "$CAST"
fi

banner() {
  printf '\n\033[1m== %s\033[0m\n' "$1"
  sleep "$PAUSE"
}

step() {
  printf '\033[2m$ %s\033[0m\n' "$*"
  "$@"
}

if [[ ! -x "$BIN" ]]; then
  echo "building $BIN"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/branchvm)
fi
if ! git -C "$ROOT" diff --quiet -- examples/auth-demo; then
  echo "error: examples/auth-demo has local changes; reset it first:" >&2
  echo "  git -C $ROOT checkout -- examples/auth-demo" >&2
  exit 1
fi
sudo -v

clear
printf '\033[1mbranchvm\033[0m: one Postgres-backed guest, forked into 3 Firecracker branches\n'
sleep "$PAUSE"

cd "$DEMO_DIR"
if ! sudo test -f "$GOLDEN"; then
  banner "one-time init: boot the golden VM, warm the build cache, snapshot it"
  step sudo -E "$BIN" init --warm "$WARM"
fi

banner "restore 3 branches from one snapshot; each agent tries a different fix"
step sudo -E "$BIN" run --branches 3 -- ./demo-agents.sh

banner "promote branch 2 into the host repo; the run is torn down"
step sudo -E "$BIN" apply 2

banner "what landed"
step git -C "$ROOT" diff --stat

banner "fire 6 isolation attacks at 5 fresh branches"
step sudo "$BIN" verify --repo .

printf '\n\033[1mdone.\033[0m revert the demo change with: git checkout -- examples/auth-demo\n'
sleep "$PAUSE"
