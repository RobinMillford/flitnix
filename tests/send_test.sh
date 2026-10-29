#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_PATH="${REPO_ROOT}/build/bin/flitnix"

echo "==> Running flitnix send CLI test suite..."

# Ensure binary is built
if [ ! -f "${BIN_PATH}" ]; then
    echo "Building flitnix binary for tests..."
    cd "${REPO_ROOT}"
    go build -tags webkit2_41 -o "${BIN_PATH}" .
fi

pass_count=0
fail_count=0

run_test() {
    local desc="$1"
    shift
    echo -n "  Testing: ${desc}... "
    if "$@"; then
        echo "[PASS]"
        pass_count=$((pass_count + 1))
    else
        echo "[FAIL]"
        fail_count=$((fail_count + 1))
    fi
}

# 1. flitnix send --help returns 0 and displays usage
test_send_help() {
    output=$("${BIN_PATH}" send --help 2>&1)
    echo "${output}" | grep -q "Flitnix CLI Sender" || return 1
    echo "${output}" | grep -q -- "--to" || return 1
    echo "${output}" | grep -q "Exit Codes:" || return 1
}
run_test "flitnix send --help returns usage and exit codes" test_send_help

# 2. flitnix send without arguments exits with code 1 (ExitUsageError)
test_send_missing_args() {
    set +e
    output=$("${BIN_PATH}" send 2>&1)
    status=$?
    set -e
    [ ${status} -eq 1 ] || return 1
    echo "${output}" | grep -q "no files or directories specified" || return 1
}
run_test "flitnix send without arguments exits with code 1" test_send_missing_args

# 3. flitnix send with link-local IPv6 URL exits with code 1 and explanatory message
test_send_link_local() {
    set +e
    output=$("${BIN_PATH}" send --to "http://[fe80::1]:43821/#dummy-token" /etc/hosts 2>&1)
    status=$?
    set -e
    [ ${status} -eq 1 ] || return 1
    echo "${output}" | grep -q "link-local" || return 1
}
run_test "flitnix send with link-local IPv6 rejects with code 1" test_send_link_local

# 4. flitnix send with missing file exits with code 2 (ExitLocalPathError)
test_send_missing_file() {
    set +e
    output=$("${BIN_PATH}" send --to "http://127.0.0.1:43821/#dummy-token" /path/to/missing/file_xyz.txt 2>&1)
    status=$?
    set -e
    [ ${status} -eq 2 ] || return 1
    echo "${output}" | grep -q "cannot access" || return 1
}
run_test "flitnix send with nonexistent file exits with code 2" test_send_missing_file

# 5. flitnix send with empty directory exits with code 2 (ExitLocalPathError)
test_send_empty_dir() {
    tmp_empty=$(mktemp -d)
    trap "rm -rf ${tmp_empty}" RETURN
    set +e
    output=$("${BIN_PATH}" send --to "http://127.0.0.1:43821/#dummy-token" "${tmp_empty}" 2>&1)
    status=$?
    set -e
    [ ${status} -eq 2 ] || return 1
    echo "${output}" | grep -q "contains no readable files" || return 1
}
run_test "flitnix send with empty directory exits with code 2" test_send_empty_dir

# 6. flitnix send with unreachable recipient exits with code 3 (ExitConnectionError)
test_send_unreachable() {
    tmp_file=$(mktemp)
    echo "test" > "${tmp_file}"
    trap "rm -f ${tmp_file}" RETURN
    set +e
    # Choose a high port where nothing is listening
    output=$("${BIN_PATH}" send --to "http://127.0.0.1:49999/#dummy-token" "${tmp_file}" 2>&1)
    status=$?
    set -e
    [ ${status} -eq 3 ] || return 1
    echo "${output}" | grep -q "cannot reach recipient" || return 1
}
run_test "flitnix send to unreachable host exits with code 3" test_send_unreachable

# 7. Security notice emitted when token is in CLI arguments
test_send_security_notice() {
    tmp_file=$(mktemp)
    echo "test" > "${tmp_file}"
    trap "rm -f ${tmp_file}" RETURN
    set +e
    output=$("${BIN_PATH}" send --to "http://127.0.0.1:49999/#dummy-token" "${tmp_file}" 2>&1)
    set -e
    echo "${output}" | grep -q "Pairing token passed via command-line arguments" || return 1
}
run_test "flitnix send emits security notice for token in CLI arguments" test_send_security_notice

# 8. flitnix send --token - accepts token via standard input
test_send_token_stdin() {
    tmp_file=$(mktemp)
    echo "test" > "${tmp_file}"
    trap "rm -f ${tmp_file}" RETURN
    set +e
    output=$(echo "secret-token-from-stdin" | "${BIN_PATH}" send --to "http://127.0.0.1:49999" --token - "${tmp_file}" 2>&1)
    status=$?
    set -e
    [ ${status} -eq 3 ] || return 1
    # Notice should NOT be emitted when token was read from stdin
    if echo "${output}" | grep -q "Pairing token passed via command-line arguments"; then
        return 1
    fi
    echo "${output}" | grep -q "cannot reach recipient" || return 1
}
run_test "flitnix send --token - reads token from stdin without warning" test_send_token_stdin

# 9. Existing commands still work
test_existing_commands() {
    "${BIN_PATH}" version > /dev/null || return 1
    "${BIN_PATH}" --version > /dev/null || return 1
    "${BIN_PATH}" help > /dev/null || return 1
}
run_test "existing commands (version, help) preserved" test_existing_commands

echo ""
echo "==> flitnix send CLI test suite results: ${pass_count} passed, ${fail_count} failed."
if [ ${fail_count} -gt 0 ]; then
    exit 1
fi
