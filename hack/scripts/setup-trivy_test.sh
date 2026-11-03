#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Tests for setup-trivy.sh. They run offline: the "release tarball" is a
# local file:// URL wrapping a fake trivy that can be told to fail its DB
# download a set number of times.
#
# Usage: ./hack/scripts/setup-trivy_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP="${SCRIPT_DIR}/setup-trivy.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
failures=0

pass() {
    echo "ok   $1"
}

fail() {
    echo "FAIL $1: $2"
    failures=$((failures + 1))
}

# Build a tarball holding a fake trivy. `trivy --version` reports 0.70.0.
# `trivy image --download-db-only` fails while ${FAKE_TRIVY_FAILS_FILE}
# holds a positive count, decrementing it each time, then succeeds and
# writes a metadata.json into --cache-dir.
mkdir -p "${TMP}/pkg"
cat >"${TMP}/pkg/trivy" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "--version" ]]; then
    echo "Version: 0.70.0"
    exit 0
fi
echo "$*" >>"${FAKE_TRIVY_CALLS_FILE}"
if [[ -n "${TRIVY_SKIP_DB_UPDATE:-}" ]]; then
    echo "fake trivy: --skip-db-update and --download-db-only options can not be specified both" >&2
    exit 1
fi
remaining="$(cat "${FAKE_TRIVY_FAILS_FILE}" 2>/dev/null || echo 0)"
if [[ "${remaining}" -gt 0 ]]; then
    echo $((remaining - 1)) >"${FAKE_TRIVY_FAILS_FILE}"
    echo "fake trivy: simulated DB download failure" >&2
    exit 1
fi
cache_dir=""
while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--cache-dir" ]]; then
        cache_dir="$2"
        shift
    fi
    shift
done
mkdir -p "${cache_dir}/db"
echo '{}' >"${cache_dir}/db/metadata.json"
EOF
chmod +x "${TMP}/pkg/trivy"
tar -czf "${TMP}/trivy.tar.gz" -C "${TMP}/pkg" trivy
TARBALL_URL="file://${TMP}/trivy.tar.gz"
TARBALL_SHA="$(sha256sum "${TMP}/trivy.tar.gz" | cut -d' ' -f1)"

# run_setup <case> [VAR=value...]
#
# Runs setup-trivy.sh in a fresh per-case directory with fast backoff and
# GitHub Actions output files. Sets $rc, $out, and $case_dir.
run_setup() {
    local name="$1"
    shift

    case_dir="${TMP}/${name}"
    mkdir -p "${case_dir}"
    : >"${case_dir}/github_env"
    : >"${case_dir}/github_path"
    : >"${case_dir}/calls"

    set +e
    out="$(
        env -u RUNNER_TEMP \
            TRIVY_INSTALL_DIR="${case_dir}/bin" \
            TRIVY_CACHE_DIR="${case_dir}/cache" \
            GITHUB_ENV="${case_dir}/github_env" \
            GITHUB_PATH="${case_dir}/github_path" \
            SETUP_TRIVY_DOWNLOAD_URL="${TARBALL_URL}" \
            SETUP_TRIVY_SHA256="${TARBALL_SHA}" \
            SETUP_TRIVY_ATTEMPTS=4 \
            SETUP_TRIVY_BACKOFF=0 \
            SETUP_TRIVY_CURL_RETRIES=0 \
            FAKE_TRIVY_CALLS_FILE="${case_dir}/calls" \
            FAKE_TRIVY_FAILS_FILE="${case_dir}/fails" \
            "$@" \
            timeout --kill-after=5s 60s "${SETUP}" 2>&1
    )"
    rc=$?
    set -e
}

count() {
    grep -c -- "$1" <<<"${out}" || true
}

# Happy path: installs, downloads the DB once, and exports the job env.
run_setup happy
if [[ "${rc}" -eq 0 && -x "${case_dir}/bin/trivy" && -f "${case_dir}/cache/db/metadata.json" ]]; then
    pass "happy path installs trivy and DB"
else
    fail "happy path installs trivy and DB" "rc=${rc} output: ${out}"
fi

if [[ "$(cat "${case_dir}/github_path")" == "${case_dir}/bin" ]]; then
    pass "happy path appends install dir to GITHUB_PATH"
else
    fail "happy path appends install dir to GITHUB_PATH" "got '$(cat "${case_dir}/github_path")'"
fi

want_env="TRIVY_CACHE_DIR=${case_dir}/cache
TRIVY_SKIP_VERSION_CHECK=true
TRIVY_SKIP_DB_UPDATE=true"
if [[ "$(cat "${case_dir}/github_env")" == "${want_env}" ]]; then
    pass "happy path exports TRIVY_* to GITHUB_ENV"
else
    fail "happy path exports TRIVY_* to GITHUB_ENV" "got '$(cat "${case_dir}/github_env")'"
fi

if grep -q -- "--download-db-only" "${case_dir}/calls" && grep -q -- "--cache-dir ${case_dir}/cache" "${case_dir}/calls"; then
    pass "happy path passes --download-db-only and --cache-dir"
else
    fail "happy path passes --download-db-only and --cache-dir" "calls: $(cat "${case_dir}/calls")"
fi

# Transient DB failures are retried until the download succeeds.
mkdir -p "${TMP}/db-retry"
echo 2 >"${TMP}/db-retry/fails"
run_setup db-retry
if [[ "${rc}" -eq 0 && "$(wc -l <"${case_dir}/calls")" -eq 3 && "$(count 'retrying in')" -eq 2 ]]; then
    pass "DB download retries transient failures"
else
    fail "DB download retries transient failures" "rc=${rc} calls=$(wc -l <"${case_dir}/calls") output: ${out}"
fi

# Persistent DB failures give up after SETUP_TRIVY_ATTEMPTS and fail.
mkdir -p "${TMP}/db-exhausted"
echo 99 >"${TMP}/db-exhausted/fails"
run_setup db-exhausted
if [[ "${rc}" -ne 0 && "$(wc -l <"${case_dir}/calls")" -eq 4 ]] &&
    grep -q "vulnerability DB download failed after 4 attempts" <<<"${out}"; then
    pass "DB download fails after exhausting attempts"
else
    fail "DB download fails after exhausting attempts" "rc=${rc} calls=$(wc -l <"${case_dir}/calls") output: ${out}"
fi

if [[ ! -s "${case_dir}/github_env" && ! -s "${case_dir}/github_path" ]]; then
    pass "failed setup exports nothing"
else
    fail "failed setup exports nothing" "env='$(cat "${case_dir}/github_env")' path='$(cat "${case_dir}/github_path")'"
fi

# Persistent download failures give up after SETUP_TRIVY_ATTEMPTS and fail,
# doubling the delay between attempts up to SETUP_TRIVY_MAX_BACKOFF.
run_setup download-exhausted SETUP_TRIVY_DOWNLOAD_URL="file://${TMP}/missing.tar.gz" \
    SETUP_TRIVY_BACKOFF=1 SETUP_TRIVY_MAX_BACKOFF=2
delays="$(grep -o 'retrying in [0-9]*s' <<<"${out}" | tr '\n' ' ')"
if [[ "${rc}" -ne 0 && "${delays}" == "retrying in 1s retrying in 2s retrying in 2s " && ! -e "${case_dir}/bin/trivy" ]] &&
    grep -q "trivy download failed after 4 attempts" <<<"${out}"; then
    pass "binary download fails after exhausting attempts"
else
    fail "binary download fails after exhausting attempts" "rc=${rc} output: ${out}"
fi

# An initial backoff above SETUP_TRIVY_MAX_BACKOFF is clamped, so no delay,
# including the first, exceeds the max.
run_setup backoff-clamped SETUP_TRIVY_DOWNLOAD_URL="file://${TMP}/missing.tar.gz" \
    SETUP_TRIVY_BACKOFF=10 SETUP_TRIVY_MAX_BACKOFF=1
delays="$(grep -o 'retrying in [0-9]*s' <<<"${out}" | tr '\n' ' ')"
if [[ "${rc}" -ne 0 && "${delays}" == "retrying in 1s retrying in 1s retrying in 1s " ]]; then
    pass "initial backoff is clamped to the max backoff"
else
    fail "initial backoff is clamped to the max backoff" "rc=${rc} output: ${out}"
fi

# A tarball that does not match the pinned checksum is never installed.
run_setup bad-checksum SETUP_TRIVY_SHA256="0000000000000000000000000000000000000000000000000000000000000000"
if [[ "${rc}" -ne 0 && ! -e "${case_dir}/bin/trivy" ]] && grep -q "checksum mismatch" <<<"${out}"; then
    pass "checksum mismatch is rejected"
else
    fail "checksum mismatch is rejected" "rc=${rc} output: ${out}"
fi

# A version without a pinned checksum is rejected unless one is supplied.
run_setup unpinned-version TRIVY_VERSION=0.0.1 SETUP_TRIVY_SHA256=
if [[ "${rc}" -ne 0 ]] && grep -q "no pinned checksum for trivy 0.0.1" <<<"${out}"; then
    pass "unpinned version is rejected"
else
    fail "unpinned version is rejected" "rc=${rc} output: ${out}"
fi

# An already-installed matching version is reused without downloading.
mkdir -p "${TMP}/reuse/bin"
cp "${TMP}/pkg/trivy" "${TMP}/reuse/bin/trivy"
run_setup reuse SETUP_TRIVY_DOWNLOAD_URL="file://${TMP}/missing.tar.gz" SETUP_TRIVY_SKIP_DB=true
if [[ "${rc}" -eq 0 ]] && grep -q "already installed" <<<"${out}" &&
    ! grep -q "TRIVY_SKIP_DB_UPDATE" "${case_dir}/github_env"; then
    pass "installed version is reused and SKIP_DB skips the DB"
else
    fail "installed version is reused and SKIP_DB skips the DB" "rc=${rc} output: ${out}"
fi

# A second run in the same job inherits TRIVY_SKIP_DB_UPDATE=true from the
# first run's GITHUB_ENV; the DB refresh must still work.
run_setup rerun TRIVY_SKIP_DB_UPDATE=true
if [[ "${rc}" -eq 0 && -f "${case_dir}/cache/db/metadata.json" ]]; then
    pass "rerun with TRIVY_SKIP_DB_UPDATE set still refreshes the DB"
else
    fail "rerun with TRIVY_SKIP_DB_UPDATE set still refreshes the DB" "rc=${rc} output: ${out}"
fi

# Invalid knobs are rejected before any download.
run_setup bad-attempts SETUP_TRIVY_ATTEMPTS=0
if [[ "${rc}" -ne 0 ]] && grep -q "SETUP_TRIVY_ATTEMPTS must be at least 1" <<<"${out}"; then
    pass "zero attempts is rejected"
else
    fail "zero attempts is rejected" "rc=${rc} output: ${out}"
fi

run_setup bad-backoff SETUP_TRIVY_BACKOFF=soon
if [[ "${rc}" -ne 0 ]] && grep -q "SETUP_TRIVY_BACKOFF must be a non-negative integer" <<<"${out}"; then
    pass "non-numeric backoff is rejected"
else
    fail "non-numeric backoff is rejected" "rc=${rc} output: ${out}"
fi

if [[ "${failures}" -ne 0 ]]; then
    echo "${failures} test(s) failed"
    exit 1
fi

echo "all tests passed"
