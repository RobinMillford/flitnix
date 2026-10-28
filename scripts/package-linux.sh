#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:-${VERSION:-0.1.0}}"
ARCH="${2:-${ARCH:-amd64}}"
DIST_DIR="${3:-dist}"

# Ensure build binary exists
if [ ! -f "build/bin/flitnix" ]; then
    echo "Error: build/bin/flitnix does not exist. Run wails build first." >&2
    exit 1
fi

mkdir -p "${DIST_DIR}"

export VERSION ARCH

echo "Packaging Linux releases for Flitnix v${VERSION} (${ARCH})..."

# 1. DEB package
echo "-> Building DEB package..."
nfpm package --config nfpm.yaml --packager deb --target "${DIST_DIR}/flitnix_${VERSION}_linux_${ARCH}.deb"

# 2. RPM package
echo "-> Building RPM package..."
nfpm package --config nfpm.yaml --packager rpm --target "${DIST_DIR}/flitnix_${VERSION}_linux_${ARCH}.rpm"

# 3. Manual tar.gz archive
echo "-> Building manual tar.gz archive..."
TAR_STAGE="$(mktemp -d)"
trap 'rm -rf "${TAR_STAGE}"' EXIT

mkdir -p "${TAR_STAGE}/flitnix/bin" "${TAR_STAGE}/flitnix/share/applications" "${TAR_STAGE}/flitnix/share/icons"
cp build/bin/flitnix "${TAR_STAGE}/flitnix/bin/"
chmod 0755 "${TAR_STAGE}/flitnix/bin/flitnix"
cp build/linux/flitnix.desktop "${TAR_STAGE}/flitnix/share/applications/"
cp -r build/linux/icons/hicolor "${TAR_STAGE}/flitnix/share/icons/"
if [ -f LICENSE ]; then
    cp LICENSE "${TAR_STAGE}/flitnix/"
fi
if [ -f NOTICE ]; then
    cp NOTICE "${TAR_STAGE}/flitnix/"
fi
if [ -f README.md ]; then
    cp README.md "${TAR_STAGE}/flitnix/"
fi

cat << 'INSTALL_HELPER' > "${TAR_STAGE}/flitnix/install.sh"
#!/usr/bin/env sh
set -eu

PREFIX="${PREFIX:-/usr/local}"
if [ "$(id -u)" -ne 0 ] && [ "$PREFIX" = "/usr/local" ]; then
    PREFIX="${HOME}/.local"
fi

echo "Installing Flitnix to ${PREFIX}..."
mkdir -p "${PREFIX}/bin" "${PREFIX}/share/applications" "${PREFIX}/share/icons"
cp bin/flitnix "${PREFIX}/bin/flitnix"
chmod 0755 "${PREFIX}/bin/flitnix"
cp share/applications/flitnix.desktop "${PREFIX}/share/applications/flitnix.desktop"
cp -r share/icons/hicolor "${PREFIX}/share/icons/"

if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "${PREFIX}/share/applications" || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache -f -t "${PREFIX}/share/icons/hicolor" || true
fi

echo "Flitnix installed successfully to ${PREFIX}/bin/flitnix"
INSTALL_HELPER
chmod 0755 "${TAR_STAGE}/flitnix/install.sh"

tar -czf "${DIST_DIR}/flitnix_${VERSION}_linux_${ARCH}.tar.gz" -C "${TAR_STAGE}" flitnix

echo "-> All Linux packages created in ${DIST_DIR}/:"
ls -lh "${DIST_DIR}"/flitnix_"${VERSION}"_linux_"${ARCH}".*
