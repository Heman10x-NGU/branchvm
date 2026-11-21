#!/usr/bin/env bash
# Runs INSIDE the Linux VM (fc-lab) as root. From the macOS host:
#   limactl shell fc-lab sudo bash /path/to/scripts/fetch-assets.sh
# Installs Firecracker >= v1.17.0 and a guest kernel into
# /var/lib/branchvm/assets. Safe to re-run.
set -euo pipefail

MIN_FC=1.17.0
MIN_KERNEL=6.10
ASSETS_DIR=/var/lib/branchvm/assets
CI_BUCKET=https://s3.amazonaws.com/spec.ccfc.min

ARCH=$(uname -m)
case "$ARCH" in
  aarch64) KERNEL_NAME=Image ;;
  x86_64) KERNEL_NAME=vmlinux ;;
  *) echo "error: unsupported arch $ARCH" >&2; exit 1 ;;
esac
KERNEL_PATH="$ASSETS_DIR/$KERNEL_NAME"

if [ "$(id -u)" -ne 0 ]; then
  echo "error: run as root (sudo)" >&2
  exit 1
fi

mkdir -p "$ASSETS_DIR"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# version_ge A B: true if A >= B
version_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

installed_fc_version() {
  command -v firecracker >/dev/null 2>&1 || return 0
  firecracker --version 2>/dev/null | head -n1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1 || true
}

install_firecracker() {
  local cur tag ver url dir
  cur=$(installed_fc_version)
  if [ -n "$cur" ] && version_ge "$cur" "$MIN_FC"; then
    echo "firecracker $cur already installed"
    return
  fi
  tag=$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | jq -r .tag_name)
  ver=${tag#v}
  if [ -z "$tag" ] || [ "$tag" = "null" ]; then
    echo "error: could not resolve latest Firecracker release" >&2
    exit 1
  fi
  if ! version_ge "$ver" "$MIN_FC"; then
    echo "error: latest Firecracker $tag is older than v$MIN_FC" >&2
    exit 1
  fi
  url="https://github.com/firecracker-microvm/firecracker/releases/download/${tag}/firecracker-${tag}-${ARCH}.tgz"
  echo "downloading $url"
  mkdir -p "$WORK/fc"
  curl -fsSL "$url" -o "$WORK/fc.tgz"
  tar -xzf "$WORK/fc.tgz" -C "$WORK/fc"
  dir="$WORK/fc/release-${tag}-${ARCH}"
  install -m 0755 "$dir/firecracker-${tag}-${ARCH}" /usr/local/bin/firecracker
  install -m 0755 "$dir/jailer-${tag}-${ARCH}" /usr/local/bin/jailer
}

# kernel_version FILE: prints x.y[.z] from the embedded banner or empty
kernel_version() {
  strings "$1" 2>/dev/null | grep -m1 -oE 'Linux version [0-9]+\.[0-9]+(\.[0-9]+)?' | awk '{print $3}' || true
}

kernel_valid() {
  local f=$1 v
  [ -s "$f" ] || return 1
  if [ "$ARCH" = "x86_64" ]; then
    file "$f" | grep -q ELF || return 1
  else
    file "$f" | grep -qiE 'arm64|aarch64|Linux kernel' || return 1
  fi
  v=$(kernel_version "$f")
  [ -n "$v" ] || return 1
  if [ "$ARCH" = "aarch64" ]; then
    version_ge "$v" "$MIN_KERNEL" || return 1
  fi
}

# Newest CI kernel key >= MIN_KERNEL for ARCH, searching the S3 listing.
find_ci_kernel() {
  local token="" url page keys=""
  while :; do
    url="$CI_BUCKET/?list-type=2&prefix=firecracker-ci/"
    [ -n "$token" ] && url="$url&continuation-token=$(printf '%s' "$token" | jq -sRr @uri)"
    page=$(curl -fsSL "$url") || return 0
    keys+=$(printf '%s' "$page" | grep -oE '<Key>[^<]+</Key>' | sed -e 's/<Key>//' -e 's/<\/Key>//' \
      | grep -E "/${ARCH}/vmlinux-[0-9]+\.[0-9]+(\.[0-9]+)?$" || true)
    keys+=$'\n'
    if printf '%s' "$page" | grep -q '<IsTruncated>true</IsTruncated>'; then
      token=$(printf '%s' "$page" | grep -oE '<NextContinuationToken>[^<]+' | sed 's/<NextContinuationToken>//')
    else
      break
    fi
  done
  printf '%s\n' "$keys" | grep -v '^$' | while read -r k; do
    v=${k##*vmlinux-}
    if version_ge "$v" "$MIN_KERNEL"; then printf '%s %s\n' "$v" "$k"; fi
  done | sort -V | tail -n1 | awk '{print $2}'
}

fetch_ubuntu_kernel() {
  echo "falling back to Ubuntu linux-image kernel"
  local deb_dir="$WORK/deb" pkg vmlinuz
  mkdir -p "$deb_dir"
  apt-get update >/dev/null
  pkg=$(apt-cache depends linux-image-virtual 2>/dev/null | awk '/Depends: linux-image-[0-9]/{print $2; exit}')
  if [ -z "$pkg" ]; then
    echo "error: could not resolve linux-image-virtual dependency" >&2
    exit 1
  fi
  (cd "$deb_dir" && apt-get download "$pkg")
  dpkg-deb -x "$deb_dir"/*.deb "$deb_dir/x"
  vmlinuz=$(ls "$deb_dir"/x/boot/vmlinuz-* | head -n1)
  if [ "$ARCH" = "aarch64" ]; then
    if file "$vmlinuz" | grep -q gzip; then
      gunzip -c "$vmlinuz" > "$WORK/kernel"
    else
      cp "$vmlinuz" "$WORK/kernel"
    fi
  else
    if [ -x /usr/src/linux-headers-"$(uname -r)"/scripts/extract-vmlinux ]; then
      /usr/src/linux-headers-"$(uname -r)"/scripts/extract-vmlinux "$vmlinuz" > "$WORK/kernel"
    else
      echo "error: x86_64 fallback needs extract-vmlinux to unpack bzImage" >&2
      exit 1
    fi
  fi
}

install_kernel() {
  if kernel_valid "$KERNEL_PATH"; then
    echo "kernel already present: $KERNEL_PATH ($(kernel_version "$KERNEL_PATH"))"
    return
  fi
  local key
  key=$(find_ci_kernel || true)
  if [ -n "$key" ]; then
    echo "downloading CI kernel $key"
    curl -fsSL "$CI_BUCKET/$key" -o "$WORK/kernel"
  else
    fetch_ubuntu_kernel
  fi
  file "$WORK/kernel"
  local v
  v=$(kernel_version "$WORK/kernel")
  echo "kernel version detected: ${v:-unknown}"
  if [ "$ARCH" = "aarch64" ] && { [ -z "$v" ] || ! version_ge "$v" "$MIN_KERNEL"; }; then
    echo "error: aarch64 needs Linux >= $MIN_KERNEL for DeviceTree VMGenID (got ${v:-unknown})" >&2
    exit 1
  fi
  install -m 0644 "$WORK/kernel" "$KERNEL_PATH"
}

install_firecracker
install_kernel

echo
echo "firecracker: $(firecracker --version 2>/dev/null | head -n1)"
echo "kernel:      $KERNEL_PATH ($(kernel_version "$KERNEL_PATH"))"
