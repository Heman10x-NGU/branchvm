#!/bin/sh
# Installs branchvm and static execd on Linux (amd64 or arm64).
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Heman10x-NGU/branchvm/main/scripts/install.sh | sudo sh
set -eu

REPO="${BRANCHVM_REPO:-Heman10x-NGU/branchvm}"
VERSION="${VERSION:-latest}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
ASSETS_DIR="${ASSETS_DIR:-/var/lib/branchvm/assets}"
SHARE_DIR="${SHARE_DIR:-/usr/local/share/branchvm}"

die() { echo "branchvm install: $*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || die "requires Linux with KVM (on macOS, use scripts/setup-lima.sh first)"
[ "$(id -u)" -eq 0 ] || die "must run as root (pipe to sudo sh)"

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

for tool in curl tar sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

if [ "$VERSION" = "latest" ]; then
  BASE_URL="https://github.com/${REPO}/releases/latest/download"
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$TAG" ] || die "could not resolve latest release tag for ${REPO}"
else
  TAG="$VERSION"
  BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
fi

TARBALL="branchvm_${TAG}_linux_${ARCH}.tar.gz"
SUMFILE="${TARBALL}.sha256"

TMP="$(mktemp -d /tmp/branchvm-install.XXXXXX)"
trap '[ -n "${TMP:-}" ] && rm -rf "$TMP"' EXIT

echo "downloading ${TARBALL} from ${REPO} (${TAG})..."
curl -fsSL -o "${TMP}/${TARBALL}" "${BASE_URL}/${TARBALL}"
curl -fsSL -o "${TMP}/${SUMFILE}" "${BASE_URL}/${SUMFILE}"

(cd "$TMP" && sha256sum -c "$SUMFILE")
tar -xzf "${TMP}/${TARBALL}" -C "$TMP"

mkdir -p "$BIN_DIR" "$ASSETS_DIR" "$SHARE_DIR"
install -m 0755 "${TMP}/branchvm" "${BIN_DIR}/branchvm"
install -m 0755 "${TMP}/execd" "${ASSETS_DIR}/execd"
install -m 0755 "${TMP}/scripts/fetch-assets.sh" "${SHARE_DIR}/fetch-assets.sh"

echo "installed:"
echo "  ${BIN_DIR}/branchvm"
echo "  ${ASSETS_DIR}/execd"
echo "  ${SHARE_DIR}/fetch-assets.sh"
echo
echo "next step (if Firecracker >= 1.17.0 and Linux >= 6.10 kernel are not yet installed):"
echo "  sudo bash ${SHARE_DIR}/fetch-assets.sh"
