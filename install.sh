#!/usr/bin/env sh
# Flitnix Universal Installer
# https://github.com/RobinMillford/flitnix
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/RobinMillford/flitnix/main/install.sh | sh
#
# Inspect-first:
#   curl -fsSL https://raw.githubusercontent.com/RobinMillford/flitnix/main/install.sh -o install.sh
#   less install.sh
#   sh install.sh
#
# Flags:
#   --version <v>   Install specific version (e.g. 0.1.0 or v0.1.0)
#   --prefix <path> Custom install prefix for tar.gz (default: /usr/local or ~/.local)
#   --dry-run       Print detected environment and download URLs without installing
#   -h, --help      Show help and options

set -eu

REPO="RobinMillford/flitnix"
GITHUB_URL="${FLITNIX_BASE_URL:-https://github.com/${REPO}}"
DEFAULT_VERSION="0.1.0"

TARGET_VERSION=""
DRY_RUN=0
CUSTOM_PREFIX=""
TMP_DIR=""

cleanup() {
    if [ -n "${TMP_DIR}" ] && [ -d "${TMP_DIR}" ]; then
        rm -rf "${TMP_DIR}"
    fi
}
trap cleanup EXIT INT TERM

log_info() {
    printf "==> %s\n" "$*"
}

log_warn() {
    printf "\033[1;33m[WARNING]\033[0m %s\n" "$*" >&2
}

log_error() {
    printf "\033[1;31m[ERROR]\033[0m %s\n" "$*" >&2
}

show_help() {
    cat << EOF
Flitnix Installer

Usage:
  install.sh [options]

Options:
  --version <version>   Target version to install (e.g. 0.1.0 or v0.1.0)
  --prefix <path>       Target prefix for tarball install (default: /usr/local or ~/.local)
  --dry-run             Simulate detection and download without modifying system
  -h, --help            Show this help message

For more information, visit: ${GITHUB_URL}
EOF
}

# Parse command line flags
while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            if [ $# -lt 2 ]; then
                log_error "--version requires an argument"
                exit 1
            fi
            TARGET_VERSION="$2"
            shift 2
            ;;
        --prefix)
            if [ $# -lt 2 ]; then
                log_error "--prefix requires an argument"
                exit 1
            fi
            CUSTOM_PREFIX="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            show_help
            exit 1
            ;;
    esac
done

# Detect OS
OS_RAW="$(uname -s)"
case "${OS_RAW}" in
    Linux)
        OS="linux"
        ;;
    Darwin)
        OS="darwin"
        ;;
    *)
        log_error "Unsupported operating system: ${OS_RAW}. Flitnix supports Linux, macOS, and Windows."
        exit 1
        ;;
esac

# Detect Architecture
ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
    x86_64|amd64)
        ARCH="amd64"
        ;;
    arm64|aarch64)
        ARCH="arm64"
        ;;
    *)
        log_error "Unsupported architecture: ${ARCH_RAW}. Flitnix currently supports x86_64 / amd64 (and Apple Silicon on macOS)."
        exit 1
        ;;
esac

# Detect Linux Distribution / Package Manager
PKG_FORMAT="tar.gz"
if [ "${OS}" = "linux" ]; then
    if [ -f /etc/os-release ]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        DISTRO_ID="${ID:-}"
        DISTRO_LIKE="${ID_LIKE:-}"
    else
        DISTRO_ID=""
        DISTRO_LIKE=""
    fi

    if command -v dpkg >/dev/null 2>&1 && { [ "${DISTRO_ID}" = "ubuntu" ] || [ "${DISTRO_ID}" = "debian" ] || [ "${DISTRO_ID}" = "linuxmint" ] || [ "${DISTRO_ID}" = "pop" ] || echo "${DISTRO_LIKE}" | grep -q "debian"; }; then
        PKG_FORMAT="deb"
    elif command -v rpm >/dev/null 2>&1 && { [ "${DISTRO_ID}" = "fedora" ] || [ "${DISTRO_ID}" = "rhel" ] || [ "${DISTRO_ID}" = "centos" ] || [ "${DISTRO_ID}" = "rocky" ] || [ "${DISTRO_ID}" = "almalinux" ] || [ "${DISTRO_ID}" = "opensuse" ] || [ "${DISTRO_ID}" = "sles" ] || echo "${DISTRO_LIKE}" | grep -q "fedora\|rhel"; }; then
        PKG_FORMAT="rpm"
    fi
elif [ "${OS}" = "darwin" ]; then
    PKG_FORMAT="dmg"
fi

# Determine Version and Release Tag
if [ -n "${TARGET_VERSION}" ]; then
    VERSION="${TARGET_VERSION#v}"
    RELEASE_TAG="v${VERSION}"
else
    # Attempt to resolve latest release from GitHub API
    RESOLVED_TAG=""
    if command -v curl >/dev/null 2>&1; then
        RESOLVED_TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | head -n 1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' || true)"
    fi
    if [ -n "${RESOLVED_TAG}" ]; then
        RELEASE_TAG="${RESOLVED_TAG}"
        VERSION="${RELEASE_TAG#v}"
    else
        VERSION="${DEFAULT_VERSION}"
        RELEASE_TAG="v${VERSION}"
    fi
fi

# Determine Download Asset Name
if [ "${OS}" = "linux" ]; then
    case "${PKG_FORMAT}" in
        deb)
            ASSET_NAME="flitnix_${VERSION}_linux_${ARCH}.deb"
            ;;
        rpm)
            ASSET_NAME="flitnix_${VERSION}_linux_${ARCH}.rpm"
            ;;
        *)
            ASSET_NAME="flitnix_${VERSION}_linux_${ARCH}.tar.gz"
            ;;
    esac
elif [ "${OS}" = "darwin" ]; then
    ASSET_NAME="flitnix_${VERSION}_macOS_universal.dmg"
fi

DOWNLOAD_URL="${GITHUB_URL}/releases/download/${RELEASE_TAG}/${ASSET_NAME}"
CHECKSUM_URL="${GITHUB_URL}/releases/download/${RELEASE_TAG}/SHA256SUMS.txt"

log_info "Flitnix Installer"
log_info "Detected OS: ${OS} (${ARCH})"
log_info "Package type: ${PKG_FORMAT}"
log_info "Version: ${VERSION} (${RELEASE_TAG})"
log_info "Asset: ${ASSET_NAME}"

if [ "${DRY_RUN}" -eq 1 ]; then
    log_info "Dry-run mode enabled. Would download from:"
    log_info "  Package URL:  ${DOWNLOAD_URL}"
    log_info "  Checksum URL: ${CHECKSUM_URL}"
    exit 0
fi

# Verify required downloader
if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    log_error "Neither curl nor wget was found. Please install curl or wget to continue."
    exit 1
fi

download_file() {
    url="$1"
    output="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$output"
    else
        wget -qO "$output" "$url"
    fi
}

calculate_sha256() {
    file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$file" | awk '{print $NF}'
    else
        log_error "No SHA-256 utility found (sha256sum, shasum, openssl)."
        exit 1
    fi
}

# Create temporary working directory
TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'flitnix-install')"

log_info "Downloading ${ASSET_NAME}..."
if ! download_file "${DOWNLOAD_URL}" "${TMP_DIR}/${ASSET_NAME}"; then
    log_error "Failed to download asset from ${DOWNLOAD_URL}."
    log_error "Please check your network connection or verify that release ${RELEASE_TAG} exists."
    exit 1
fi

log_info "Downloading SHA256SUMS.txt..."
if ! download_file "${CHECKSUM_URL}" "${TMP_DIR}/SHA256SUMS.txt"; then
    log_error "Failed to download checksum file from ${CHECKSUM_URL}."
    exit 1
fi

# Verify Checksum
log_info "Verifying SHA-256 checksum..."
ACTUAL_HASH="$(calculate_sha256 "${TMP_DIR}/${ASSET_NAME}")"
EXPECTED_HASH="$(grep -F "${ASSET_NAME}" "${TMP_DIR}/SHA256SUMS.txt" 2>/dev/null | awk '{print $1}' || true)"

if [ -z "${EXPECTED_HASH}" ]; then
    log_error "Asset ${ASSET_NAME} not found in SHA256SUMS.txt"
    exit 1
fi

if [ "${ACTUAL_HASH}" != "${EXPECTED_HASH}" ]; then
    log_error "Checksum verification FAILED for ${ASSET_NAME}!"
    log_error "  Expected: ${EXPECTED_HASH}"
    log_error "  Actual:   ${ACTUAL_HASH}"
    log_error "The downloaded file may be corrupted or tampered with. Aborting."
    exit 1
fi
log_info "Checksum OK (${ACTUAL_HASH})"

# Helper for elevated commands
run_elevated() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif command -v sudo >/dev/null 2>&1; then
        log_info "Running with sudo: $*"
        sudo "$@"
    else
        log_error "Root permissions required to run: $*"
        exit 1
    fi
}

# Install based on package format
if [ "${OS}" = "linux" ]; then
    case "${PKG_FORMAT}" in
        deb)
            log_info "Installing Debian package..."
            if command -v apt-get >/dev/null 2>&1; then
                run_elevated apt-get install -y "${TMP_DIR}/${ASSET_NAME}"
            else
                run_elevated dpkg -i "${TMP_DIR}/${ASSET_NAME}"
            fi
            ;;
        rpm)
            log_info "Installing RPM package..."
            if command -v dnf >/dev/null 2>&1; then
                run_elevated dnf install -y "${TMP_DIR}/${ASSET_NAME}"
            elif command -v zypper >/dev/null 2>&1; then
                run_elevated zypper --non-interactive install "${TMP_DIR}/${ASSET_NAME}"
            else
                run_elevated rpm -Uvh --replacepkgs "${TMP_DIR}/${ASSET_NAME}"
            fi
            ;;
        *)
            log_info "Installing from tarball archive..."
            PREFIX="${CUSTOM_PREFIX:-/usr/local}"
            if [ "$(id -u)" -ne 0 ] && [ -z "${CUSTOM_PREFIX}" ]; then
                PREFIX="${HOME}/.local"
            fi

            mkdir -p "${TMP_DIR}/extracted"
            tar -xzf "${TMP_DIR}/${ASSET_NAME}" -C "${TMP_DIR}/extracted"

            TARGET_BIN_DIR="${PREFIX}/bin"
            TARGET_APP_DIR="${PREFIX}/share/applications"
            TARGET_ICON_DIR="${PREFIX}/share/icons"

            if [ "$(id -u)" -eq 0 ] || [ -w "${PREFIX}" ]; then
                mkdir -p "${TARGET_BIN_DIR}" "${TARGET_APP_DIR}" "${TARGET_ICON_DIR}"
                cp "${TMP_DIR}/extracted/flitnix/bin/flitnix" "${TARGET_BIN_DIR}/flitnix"
                chmod 0755 "${TARGET_BIN_DIR}/flitnix"
                cp "${TMP_DIR}/extracted/flitnix/share/applications/flitnix.desktop" "${TARGET_APP_DIR}/"
                cp -r "${TMP_DIR}/extracted/flitnix/share/icons/"* "${TARGET_ICON_DIR}/"
            else
                run_elevated mkdir -p "${TARGET_BIN_DIR}" "${TARGET_APP_DIR}" "${TARGET_ICON_DIR}"
                run_elevated cp "${TMP_DIR}/extracted/flitnix/bin/flitnix" "${TARGET_BIN_DIR}/flitnix"
                run_elevated chmod 0755 "${TARGET_BIN_DIR}/flitnix"
                run_elevated cp "${TMP_DIR}/extracted/flitnix/share/applications/flitnix.desktop" "${TARGET_APP_DIR}/"
                run_elevated cp -r "${TMP_DIR}/extracted/flitnix/share/icons/"* "${TARGET_ICON_DIR}/"
            fi

            if command -v update-desktop-database >/dev/null 2>&1; then
                update-desktop-database "${TARGET_APP_DIR}" || true
            fi
            if command -v gtk-update-icon-cache >/dev/null 2>&1; then
                gtk-update-icon-cache -f -t "${TARGET_ICON_DIR}/hicolor" || true
            fi
            ;;
    esac

    # Linux WebKitGTK dependency reminder check
    if ! ldconfig -p 2>/dev/null | grep -q "libwebkit2gtk-4\.[01]"; then
        log_warn "WebKitGTK (webkit2gtk-4.1 or webkit2gtk-4.0) was not detected on your system."
        log_warn "If Flitnix fails to launch, install WebKitGTK using your system package manager:"
        log_warn "  Fedora/RHEL:   sudo dnf install webkit2gtk4.1"
        log_warn "  Debian/Ubuntu: sudo apt install libwebkit2gtk-4.1-0"
        log_warn "  Arch Linux:    sudo pacman -S webkit2gtk-4.1"
    fi

elif [ "${OS}" = "darwin" ]; then
    log_info "Installing on macOS..."
    MOUNT_DIR="${TMP_DIR}/dmg_mount"
    mkdir -p "${MOUNT_DIR}"
    hdiutil attach "${TMP_DIR}/${ASSET_NAME}" -mountpoint "${MOUNT_DIR}" -nobrowse -quiet

    INSTALL_DEST="/Applications"
    if [ ! -w "${INSTALL_DEST}" ]; then
        INSTALL_DEST="${HOME}/Applications"
        mkdir -p "${INSTALL_DEST}"
    fi

    log_info "Copying Flitnix.app to ${INSTALL_DEST}..."
    cp -R "${MOUNT_DIR}/Flitnix.app" "${INSTALL_DEST}/"
    hdiutil detach "${MOUNT_DIR}" -quiet

    log_info "Flitnix installed to ${INSTALL_DEST}/Flitnix.app"
fi

log_info "Installation complete! You can now launch Flitnix."
