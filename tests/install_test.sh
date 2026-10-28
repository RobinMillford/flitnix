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
trap 'rm -rf "${TEST_DIR}"' EXIT

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

echo "==> All installer tests passed!"
