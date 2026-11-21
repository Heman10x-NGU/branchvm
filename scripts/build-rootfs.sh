#!/usr/bin/env bash
# Builds /var/lib/branchvm/base/rootfs.ext4: Ubuntu 24.04 (noble) with
# Postgres 16, git, curl, dnsutils, Go, and the static execd as /sbin/init.
#
# Runs as root inside the fc-lab Linux VM. Usage:
#   sudo scripts/build-rootfs.sh [--force]
#
# Population uses debootstrap rather than exporting the ubuntu:24.04 docker
# image: it needs no container daemon, starts from a plain minbase system
# without docker's dpkg path excludes and policy tweaks, and only depends on
# the Ubuntu mirror.
#
# Environment overrides: ROOTFS_SIZE (default 1536M), OUT_DIR, UBUNTU_MIRROR.
set -euo pipefail

SUITE=noble
ROOTFS_SIZE="${ROOTFS_SIZE:-1536M}"
OUT_DIR="${OUT_DIR:-/var/lib/branchvm/base}"
ROOTFS="${OUT_DIR}/rootfs.ext4"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PACKAGES=(postgresql-16 git curl dnsutils golang-go ca-certificates iproute2)
FORCE=0

die() { echo "build-rootfs: $*" >&2; exit 1; }
log() { echo "build-rootfs: $*"; }

for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "must run as root"
[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux (inside fc-lab)"
for tool in debootstrap mkfs.ext4 e2fsck go mount umount truncate chroot file; do
  command -v "$tool" >/dev/null || die "missing required tool: $tool"
done

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64; DEFAULT_MIRROR=http://archive.ubuntu.com/ubuntu ;;
  aarch64|arm64) ARCH=arm64; DEFAULT_MIRROR=http://ports.ubuntu.com/ubuntu-ports ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
MIRROR="${UBUNTU_MIRROR:-$DEFAULT_MIRROR}"

if [[ -e "$ROOTFS" ]]; then
  [[ "$FORCE" -eq 1 ]] || die "$ROOTFS exists; pass --force to rebuild"
  log "removing existing $ROOTFS (--force)"
  rm -f "$ROOTFS"
fi
mkdir -p "$OUT_DIR"

WORK="$(mktemp -d /tmp/branchvm-rootfs.XXXXXX)"
MNT="$WORK/mnt"
IMG="$ROOTFS.partial"
mkdir -p "$MNT"

cleanup() {
  local rc=$?
  for sub in dev/pts dev sys proc; do
    if mountpoint -q "$MNT/$sub" 2>/dev/null; then
      umount "$MNT/$sub" || umount -l "$MNT/$sub" || true
    fi
  done
  if mountpoint -q "$MNT" 2>/dev/null; then
    umount "$MNT" || umount -l "$MNT" || true
  fi
  rm -rf "$WORK"
  if [[ $rc -ne 0 ]]; then
    rm -f "$IMG"
    echo "build-rootfs: failed (exit $rc); partial image removed" >&2
  fi
}
trap cleanup EXIT

log "building execd for linux/$ARCH"
(cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -ldflags '-s -w' -o "$WORK/execd" ./cmd/execd)
EXECD_INFO="$(file -b "$WORK/execd")"
[[ "$EXECD_INFO" == ELF* && "$EXECD_INFO" == *"statically linked"* ]] \
  || die "execd is not a static ELF: $EXECD_INFO"

log "creating sparse $ROOTFS_SIZE ext4 image"
rm -f "$IMG"
truncate -s "$ROOTFS_SIZE" "$IMG"
mkfs.ext4 -F -q -L branchvm-root "$IMG"
mount -o loop "$IMG" "$MNT"

log "debootstrap $SUITE ($ARCH) from $MIRROR"
debootstrap --variant=minbase --arch="$ARCH" --components=main,universe \
  --include=ca-certificates,iproute2 "$SUITE" "$MNT" "$MIRROR"

cat >"$MNT/etc/apt/sources.list" <<EOF
deb $MIRROR $SUITE main universe
deb $MIRROR $SUITE-updates main universe
deb $MIRROR $SUITE-security main universe
EOF

mount -t proc proc "$MNT/proc"
mount -t sysfs sysfs "$MNT/sys"
mount --bind /dev "$MNT/dev"
mount --bind /dev/pts "$MNT/dev/pts"
cp -L /etc/resolv.conf "$MNT/etc/resolv.conf"

# Package postinst scripts must not start services inside the chroot.
printf '#!/bin/sh\nexit 101\n' >"$MNT/usr/sbin/policy-rc.d"
chmod 0755 "$MNT/usr/sbin/policy-rc.d"

in_chroot() {
  chroot "$MNT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/root LANG=C.UTF-8 DEBIAN_FRONTEND=noninteractive "$@"
}

log "installing ${PACKAGES[*]}"
in_chroot apt-get update -q
in_chroot apt-get install -y -q --no-install-recommends "${PACKAGES[@]}"

if [[ ! -d "$MNT/etc/postgresql/16/main" ]]; then
  log "creating postgres cluster 16/main"
  in_chroot pg_createcluster --locale C.UTF-8 16 main
fi

PG_ETC="$MNT/etc/postgresql/16/main"
mkdir -p "$PG_ETC/conf.d"
cat >"$PG_ETC/conf.d/branchvm.conf" <<'EOF'
listen_addresses = '127.0.0.1'
port = 5432
ssl = off
EOF
cat >"$PG_ETC/pg_hba.conf" <<'EOF'
local   all   postgres                trust
local   all   all                     trust
host    all   all      127.0.0.1/32   trust
host    all   all      ::1/128        trust
EOF
in_chroot chown -R postgres:postgres /etc/postgresql/16/main /var/lib/postgresql
in_chroot chmod 0700 /var/lib/postgresql/16/main
rm -f "$MNT/var/lib/postgresql/16/main/postmaster.pid"

log "installing execd as /sbin/init"
mkdir -p "$MNT/workspace"
chmod 0755 "$MNT/workspace"
rm -f "$MNT/sbin/init"
install -m 0755 "$WORK/execd" "$MNT/sbin/init"

: >"$MNT/etc/machine-id"
chmod 0444 "$MNT/etc/machine-id"
rm -f "$MNT/var/lib/dbus/machine-id" "$MNT/var/lib/systemd/random-seed"
echo branchvm >"$MNT/etc/hostname"
cat >"$MNT/etc/hosts" <<'EOF'
127.0.0.1 localhost
127.0.1.1 branchvm
::1       localhost ip6-localhost ip6-loopback
EOF
: >"$MNT/etc/resolv.conf"

rm -f "$MNT/usr/sbin/policy-rc.d"
in_chroot apt-get clean
rm -rf "$MNT/var/lib/apt/lists/"* "$MNT/var/cache/apt/"*.bin "$MNT/tmp/"*

for sub in dev/pts dev sys proc; do umount "$MNT/$sub"; done
umount "$MNT"

log "checking filesystem"
fsck_rc=0
e2fsck -f -y "$IMG" || fsck_rc=$?
[[ $fsck_rc -le 1 ]] || die "e2fsck failed with exit $fsck_rc"

mv "$IMG" "$ROOTFS"
log "done: $ROOTFS"
log "apparent size: $(du -h --apparent-size "$ROOTFS" | cut -f1), on disk: $(du -h "$ROOTFS" | cut -f1)"
