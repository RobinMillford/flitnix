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

echo "==> All installer tests passed!"
