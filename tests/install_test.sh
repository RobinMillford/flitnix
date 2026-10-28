#!/usr/bin/env sh
set -eu

echo "==> Running installer test suite..."

# Test 1: install.sh syntax validation
sh -n install.sh
echo "  [PASS] sh -n install.sh"

# Test 2: --help output
HELP_OUTPUT="$(./install.sh --help)"
if ! echo "${HELP_OUTPUT}" | grep -q "Flitnix Installer"; then
    echo "  [FAIL] --help output missing title"
    exit 1
fi
echo "  [PASS] install.sh --help"

# Test 3: --dry-run output
DRY_OUTPUT="$(./install.sh --dry-run --version 0.1.0)"
if ! echo "${DRY_OUTPUT}" | grep -q "Dry-run mode enabled"; then
    echo "  [FAIL] --dry-run did not report dry run mode"
    exit 1
fi
if ! echo "${DRY_OUTPUT}" | grep -q "0.1.0"; then
    echo "  [FAIL] --dry-run did not show target version"
    exit 1
fi
echo "  [PASS] install.sh --dry-run with --version"

# Test 4: Checksum verification logic unit test
TEST_DIR="$(mktemp -d)"
cleanup() {
    rm -rf "${TEST_DIR}"
    if [ -n "${SERVER_PID:-}" ]; then
        kill "${SERVER_PID}" 2>/dev/null || true
    fi
    if [ -n "${SERVER_PID_V2:-}" ]; then
        kill "${SERVER_PID_V2}" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

FAKE_ASSET="${TEST_DIR}/flitnix_0.1.0_linux_amd64.tar.gz"
echo "hello-flitnix-content" > "${FAKE_ASSET}"

if command -v sha256sum >/dev/null 2>&1; then
    CORRECT_HASH="$(sha256sum "${FAKE_ASSET}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    CORRECT_HASH="$(shasum -a 256 "${FAKE_ASSET}" | awk '{print $1}')"
else
    CORRECT_HASH="$(openssl dgst -sha256 "${FAKE_ASSET}" | awk '{print $NF}')"
fi

# Valid checksum entry
echo "${CORRECT_HASH}  flitnix_0.1.0_linux_amd64.tar.gz" > "${TEST_DIR}/SHA256SUMS.txt"

MATCH_HASH="$(grep -F "flitnix_0.1.0_linux_amd64.tar.gz" "${TEST_DIR}/SHA256SUMS.txt" | awk '{print $1}')"
if [ "${MATCH_HASH}" != "${CORRECT_HASH}" ]; then
    echo "  [FAIL] Checksum match failed"
    exit 1
fi
echo "  [PASS] Checksum parsing & match verification"

# Invalid checksum detection
BAD_HASH="0000000000000000000000000000000000000000000000000000000000000000"
if [ "${BAD_HASH}" = "${CORRECT_HASH}" ]; then
    echo "  [FAIL] Bad hash unexpectedly matched"
    exit 1
fi
echo "  [PASS] Corrupted checksum rejection verification"

# Test 5: End-to-end mock server fixture tests
if command -v python3 >/dev/null 2>&1; then
    FIXTURE_ROOT="${TEST_DIR}/mock_repo"
    RELEASE_DIR="${FIXTURE_ROOT}/releases/download/v0.1.0"
    mkdir -p "${RELEASE_DIR}"

    # Determine asset name for this platform
    DETECT_RAW="$(./install.sh --dry-run --version 0.1.0)"
    TARGET_ASSET="$(echo "${DETECT_RAW}" | grep "Asset:" | awk '{print $NF}')"

    # Start mock HTTP server on an available port
    PORT_FILE="${TEST_DIR}/port.txt"
    python3 -c "
import http.server, socketserver, sys
class SilentHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, format, *args):
        pass
with socketserver.TCPServer(('127.0.0.1', 0), lambda *a: SilentHandler(*a, directory='${FIXTURE_ROOT}')) as httpd:
    with open('${PORT_FILE}', 'w') as f:
        f.write(str(httpd.server_address[1]))
    httpd.serve_forever()
" &
    SERVER_PID=$!

    # Wait for server to start
    for _ in $(seq 1 30); do
        if [ -f "${PORT_FILE}" ]; then
            break
        fi
        sleep 0.1
    done
    PORT="$(cat "${PORT_FILE}")"
    MOCK_URL="http://127.0.0.1:${PORT}"

    # Subtest 5a: Missing asset (404)
    # Asset does not exist yet in RELEASE_DIR
    echo "something" > "${RELEASE_DIR}/SHA256SUMS.txt"
    set +e
    MISSING_OUT="$(FLITNIX_BASE_URL="${MOCK_URL}" ./install.sh --version 0.1.0 2>&1)"
    STATUS=$?
    set -e
    if [ $STATUS -eq 0 ]; then
        echo "  [FAIL] install.sh succeeded on missing asset"
        exit 1
    fi
    if ! echo "${MISSING_OUT}" | grep -q "Failed to download asset"; then
        echo "  [FAIL] install.sh did not report download failure on missing asset: ${MISSING_OUT}"
        exit 1
    fi
    echo "  [PASS] install.sh fails cleanly when asset is missing (404)"

    # Subtest 5b: Asset with invalid checksum (tampered)
    echo "mock-tampered-content" > "${RELEASE_DIR}/${TARGET_ASSET}"
    echo "0000000000000000000000000000000000000000000000000000000000000000  ${TARGET_ASSET}" > "${RELEASE_DIR}/SHA256SUMS.txt"
    set +e
    TAMPER_OUT="$(FLITNIX_BASE_URL="${MOCK_URL}" ./install.sh --version 0.1.0 2>&1)"
    STATUS=$?
    set -e
    if [ $STATUS -eq 0 ]; then
        echo "  [FAIL] install.sh succeeded on tampered asset"
        exit 1
    fi
    if ! echo "${TAMPER_OUT}" | grep -q "Checksum verification FAILED"; then
        echo "  [FAIL] install.sh did not report checksum failure: ${TAMPER_OUT}"
        exit 1
    fi
    echo "  [PASS] install.sh rejects tampered asset with invalid checksum"

    # Subtest 5c: Asset missing from SHA256SUMS.txt
    echo "unlisted_asset.tar.gz" > "${RELEASE_DIR}/SHA256SUMS.txt"
    set +e
    UNLISTED_OUT="$(FLITNIX_BASE_URL="${MOCK_URL}" ./install.sh --version 0.1.0 2>&1)"
    STATUS=$?
    set -e
    if [ $STATUS -eq 0 ]; then
        echo "  [FAIL] install.sh succeeded on unlisted asset"
        exit 1
    fi
    if ! echo "${UNLISTED_OUT}" | grep -q "not found in SHA256SUMS.txt"; then
        echo "  [FAIL] install.sh did not report missing checksum entry: ${UNLISTED_OUT}"
        exit 1
    fi
    echo "  [PASS] install.sh fails cleanly when asset is unlisted in SHA256SUMS.txt"

    kill "${SERVER_PID}" 2>/dev/null || true
    unset SERVER_PID
fi

# Test 6: Focused package verification unit tests (DEB and RPM)
MOCK_TOOLS="${TEST_DIR}/mock_tools"
mkdir -p "${MOCK_TOOLS}"

cat << 'EOF' > "${MOCK_TOOLS}/rpm"
#!/usr/bin/env sh
if [ -z "${MOCK_RPM_VERSION:-}" ]; then
    echo "package flitnix is not installed" >&2
    exit 1
fi
case "$*" in
    *"%{VERSION}"*)
        echo "${MOCK_RPM_VERSION}"
        ;;
    *"%{NAME}"*)
        echo "flitnix-${MOCK_RPM_VERSION}-1.x86_64"
        ;;
    *)
        echo "flitnix-${MOCK_RPM_VERSION}-1.x86_64"
        ;;
esac
exit 0
EOF
chmod +x "${MOCK_TOOLS}/rpm"

cat << 'EOF' > "${MOCK_TOOLS}/dpkg-query"
#!/usr/bin/env sh
if [ -z "${MOCK_DEB_VERSION:-}" ]; then
    echo "dpkg-query: no packages found matching flitnix" >&2
    exit 1
fi
echo "${MOCK_DEB_VERSION}"
exit 0
EOF
chmod +x "${MOCK_TOOLS}/dpkg-query"

# Subtest 6a: RPM version mismatch (e.g. 0.0.0~rc0 installed, 0.1.1 expected)
set +e
RPM_NOOP_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_RPM_VERSION="0.0.0~rc0" FLITNIX_TEST_VERIFY_ONLY=1 ./install.sh rpm 0.1.1 2>&1)"
STATUS=$?
set -e
if [ $STATUS -eq 0 ]; then
    echo "  [FAIL] verify_installed_package unexpectedly succeeded on RPM version mismatch"
    exit 1
fi
if ! echo "${RPM_NOOP_OUT}" | grep -q "Installed package version mismatch"; then
    echo "  [FAIL] verify_installed_package missing mismatch error for RPM: ${RPM_NOOP_OUT}"
    exit 1
fi
echo "  [PASS] verify_installed_package rejects stale RPM version (no-op)"

# Subtest 6b: RPM version match (0.1.1 installed, 0.1.1 expected)
RPM_OK_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_RPM_VERSION="0.1.1" FLITNIX_TEST_VERIFY_ONLY=1 ./install.sh rpm 0.1.1 2>&1)"
if ! echo "${RPM_OK_OUT}" | grep -q "Verified installed package"; then
    echo "  [FAIL] verify_installed_package did not confirm matching RPM: ${RPM_OK_OUT}"
    exit 1
fi
echo "  [PASS] verify_installed_package accepts matching RPM version"

# Subtest 6c: DEB version mismatch (0.0.0~rc0-1 installed, 0.1.1 expected)
set +e
DEB_NOOP_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_DEB_VERSION="0.0.0~rc0-1" FLITNIX_TEST_VERIFY_ONLY=1 ./install.sh deb 0.1.1 2>&1)"
STATUS=$?
set -e
if [ $STATUS -eq 0 ]; then
    echo "  [FAIL] verify_installed_package unexpectedly succeeded on DEB version mismatch"
    exit 1
fi
if ! echo "${DEB_NOOP_OUT}" | grep -q "Installed package version mismatch"; then
    echo "  [FAIL] verify_installed_package missing mismatch error for DEB: ${DEB_NOOP_OUT}"
    exit 1
fi
echo "  [PASS] verify_installed_package rejects stale DEB version (no-op)"

# Subtest 6d: DEB version match with debian revision (0.1.1-1 installed, 0.1.1 expected)
DEB_OK_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_DEB_VERSION="0.1.1-1" FLITNIX_TEST_VERIFY_ONLY=1 ./install.sh deb 0.1.1 2>&1)"
if ! echo "${DEB_OK_OUT}" | grep -q "Verified installed package"; then
    echo "  [FAIL] verify_installed_package did not confirm matching DEB with revision: ${DEB_OK_OUT}"
    exit 1
fi
echo "  [PASS] verify_installed_package accepts matching DEB version with packaging revision"

# Subtest 6e: DEB version match with epoch (1:0.1.1-2 installed, 0.1.1 expected)
DEB_EPOCH_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_DEB_VERSION="1:0.1.1-2" FLITNIX_TEST_VERIFY_ONLY=1 ./install.sh deb 0.1.1 2>&1)"
if ! echo "${DEB_EPOCH_OUT}" | grep -q "Verified installed package"; then
    echo "  [FAIL] verify_installed_package did not confirm matching DEB with epoch: ${DEB_EPOCH_OUT}"
    exit 1
fi
echo "  [PASS] verify_installed_package accepts matching DEB version with epoch and revision"

# Test 7: End-to-end package installation test with mock package manager
if command -v python3 >/dev/null 2>&1; then
    MOCK_E2E_DIR="${TEST_DIR}/mock_e2e"
    mkdir -p "${MOCK_E2E_DIR}"

    # Mock sudo, dnf, apt-get
    cat << 'EOF' > "${MOCK_TOOLS}/sudo"
#!/usr/bin/env sh
"$@"
EOF
    chmod +x "${MOCK_TOOLS}/sudo"

    cat << 'EOF' > "${MOCK_TOOLS}/dnf"
#!/usr/bin/env sh
# Simulates successful dnf install
exit 0
EOF
    chmod +x "${MOCK_TOOLS}/dnf"

    cat << 'EOF' > "${MOCK_TOOLS}/apt-get"
#!/usr/bin/env sh
# Simulates successful apt-get install
exit 0
EOF
    chmod +x "${MOCK_TOOLS}/apt-get"

    # Start mock server serving v0.1.1 release files
    RELEASE_DIR_V2="${FIXTURE_ROOT}/releases/download/v0.1.1"
    mkdir -p "${RELEASE_DIR_V2}"

    DETECT_RAW_V2="$(./install.sh --dry-run --version 0.1.1)"
    TARGET_ASSET_V2="$(echo "${DETECT_RAW_V2}" | grep "Asset:" | awk '{print $NF}')"
    TARGET_PKG_FORMAT="$(echo "${DETECT_RAW_V2}" | grep "Package type:" | awk '{print $NF}')"

    echo "mock-package-binary-data" > "${RELEASE_DIR_V2}/${TARGET_ASSET_V2}"
    if command -v sha256sum >/dev/null 2>&1; then
        V2_HASH="$(sha256sum "${RELEASE_DIR_V2}/${TARGET_ASSET_V2}" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        V2_HASH="$(shasum -a 256 "${RELEASE_DIR_V2}/${TARGET_ASSET_V2}" | awk '{print $1}')"
    else
        V2_HASH="$(openssl dgst -sha256 "${RELEASE_DIR_V2}/${TARGET_ASSET_V2}" | awk '{print $NF}')"
    fi
    echo "${V2_HASH}  ${TARGET_ASSET_V2}" > "${RELEASE_DIR_V2}/SHA256SUMS.txt"

    PORT_FILE_V2="${TEST_DIR}/port_v2.txt"
    python3 -c "
import http.server, socketserver, sys
class SilentHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, format, *args):
        pass
with socketserver.TCPServer(('127.0.0.1', 0), lambda *a: SilentHandler(*a, directory='${FIXTURE_ROOT}')) as httpd:
    with open('${PORT_FILE_V2}', 'w') as f:
        f.write(str(httpd.server_address[1]))
    httpd.serve_forever()
" &
    SERVER_PID_V2=$!

    for _ in $(seq 1 30); do
        if [ -f "${PORT_FILE_V2}" ]; then
            break
        fi
        sleep 0.1
    done
    PORT_V2="$(cat "${PORT_FILE_V2}")"
    MOCK_URL_V2="http://127.0.0.1:${PORT_V2}"

    if [ "${TARGET_PKG_FORMAT}" = "rpm" ] || [ "${TARGET_PKG_FORMAT}" = "deb" ]; then
        # Subtest 7a: Package manager no-op (DNF/APT exits 0, but installed package remains old version)
        set +e
        E2E_NOOP_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_RPM_VERSION="0.0.0~rc0" MOCK_DEB_VERSION="0.0.0~rc0-1" FLITNIX_BASE_URL="${MOCK_URL_V2}" ./install.sh --version 0.1.1 2>&1)"
        STATUS=$?
        set -e
        if [ $STATUS -eq 0 ]; then
            echo "  [FAIL] install.sh succeeded despite package manager no-op"
            exit 1
        fi
        if echo "${E2E_NOOP_OUT}" | grep -q "Installation complete"; then
            echo "  [FAIL] install.sh falsely reported 'Installation complete' on package manager no-op"
            exit 1
        fi
        if ! echo "${E2E_NOOP_OUT}" | grep -q "Installed package version mismatch"; then
            echo "  [FAIL] install.sh did not report package version mismatch: ${E2E_NOOP_OUT}"
            exit 1
        fi
        echo "  [PASS] install.sh detects package manager no-op and aborts without reporting success"

        # Subtest 7b: Package manager successful upgrade
        E2E_OK_OUT="$(PATH="${MOCK_TOOLS}:${PATH}" MOCK_RPM_VERSION="0.1.1" MOCK_DEB_VERSION="0.1.1-1" FLITNIX_BASE_URL="${MOCK_URL_V2}" ./install.sh --version 0.1.1 2>&1)"
        if ! echo "${E2E_OK_OUT}" | grep -q "Installation complete"; then
            echo "  [FAIL] install.sh did not report installation complete on successful upgrade: ${E2E_OK_OUT}"
            exit 1
        fi
        echo "  [PASS] install.sh verifies successful package upgrade and reports completion"
    fi

    kill "${SERVER_PID_V2}" 2>/dev/null || true
    unset SERVER_PID_V2
fi

echo "==> All installer tests passed!"
