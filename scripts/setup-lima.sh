#!/usr/bin/env bash
# Creates (or starts) the fc-lab Lima VM with nested virtualization and
# installs the packages branchvm needs inside it. Runs on the macOS host.
# Safe to re-run.
set -euo pipefail

VM=fc-lab
ASSETS_DIR=/var/lib/branchvm/assets

if ! command -v limactl >/dev/null 2>&1; then
  echo "error: limactl not found; install Lima first (brew install lima)" >&2
  exit 1
fi

if limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$VM"; then
  status=$(limactl list --format '{{.Status}}' "$VM" 2>/dev/null || true)
  echo "VM $VM exists (status: ${status:-unknown})"
  if [ "$status" = "Stopped" ]; then
    limactl start "$VM"
  fi
else
  limactl start --name "$VM" --vm-type vz --nested-virt \
    --cpus 4 --memory 6 --disk 40 --mount-writable --tty=false \
    template:ubuntu
fi

limactl shell "$VM" sudo bash -c '
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y xfsprogs nftables iproute2 curl jq git golang-go debootstrap docker.io
'

if ! limactl shell "$VM" test -e /dev/kvm; then
  cat >&2 <<EOF
error: /dev/kvm is missing inside $VM.
Firecracker needs KVM. Nested virtualization in Lima (vz) requires
Apple M3 or newer and macOS 15 or newer, and the VM must be created
with --nested-virt. Delete and recreate the VM on supported hardware:
  limactl delete -f $VM && $0
EOF
  exit 1
fi

limactl shell "$VM" sudo mkdir -p "$ASSETS_DIR"

echo
echo "fc-lab ready:"
echo "  /dev/kvm:   present"
echo "  assets dir: $ASSETS_DIR (inside VM)"
echo "  next:       limactl shell $VM sudo bash $(cd "$(dirname "$0")" && pwd)/fetch-assets.sh"
