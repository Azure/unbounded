#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Installs a pinned Trivy release and pre-downloads its vulnerability DB,
# retrying transient network failures with exponential backoff.
#
# aquasecurity/trivy-action installs Trivy through setup-trivy, which runs
# Trivy's contrib/install.sh. That script fetches the tarball from
# get.trivy.dev with a single `curl` and no retries, so one DNS or connect
# failure (curl exit 6 or 7) fails the release job. This script replaces that
# step: it downloads from GitHub releases, verifies a pinned SHA-256, and
# retries both the binary download and the DB download. Pair it with
# `skip-setup-trivy: true`, `cache: false`, and TRIVY_SKIP_DB_UPDATE=true on
# trivy-action so the scan itself makes no further downloads.
#
# Usage:
#   hack/scripts/setup-trivy.sh
#
# Configuration (environment variables):
#   TRIVY_VERSION              Trivy version without the leading "v".
#                              Default: 0.70.0.
#   TRIVY_INSTALL_DIR          Directory to install the trivy binary into.
#                              Default: $RUNNER_TEMP/trivy-bin, or
#                              ./tmp/trivy-bin outside GitHub Actions.
#   TRIVY_CACHE_DIR            Trivy cache directory for the vulnerability DB.
#                              Default: $RUNNER_TEMP/trivy-cache, or
#                              ./tmp/trivy-cache outside GitHub Actions.
#   SETUP_TRIVY_SKIP_DB        Set to "true" to install the binary only.
#   SETUP_TRIVY_ATTEMPTS       Attempts per download. Default: 8.
#   SETUP_TRIVY_BACKOFF        Initial delay between attempts, in seconds.
#                              Doubles after each failure. Default: 10.
#   SETUP_TRIVY_MAX_BACKOFF    Upper bound on the delay, in seconds.
#                              Default: 120.
#   SETUP_TRIVY_CURL_RETRIES   curl --retry count within each download attempt,
#                              for short blips. Default: 3.
#   SETUP_TRIVY_DOWNLOAD_URL   Override the tarball URL (for tests and mirrors).
#   SETUP_TRIVY_SHA256         Override the expected tarball SHA-256. Required
#                              when TRIVY_VERSION has no pinned checksum below.
#
# When GITHUB_PATH and GITHUB_ENV are set, the install directory is added to
# PATH and TRIVY_CACHE_DIR, TRIVY_SKIP_DB_UPDATE, and TRIVY_SKIP_VERSION_CHECK
# are exported to later steps in the job.

set -euo pipefail

TRIVY_VERSION="${TRIVY_VERSION:-0.70.0}"
TRIVY_VERSION="${TRIVY_VERSION#v}"
default_root="${RUNNER_TEMP:-${PWD}/tmp}"
TRIVY_INSTALL_DIR="${TRIVY_INSTALL_DIR:-${default_root}/trivy-bin}"
TRIVY_CACHE_DIR="${TRIVY_CACHE_DIR:-${default_root}/trivy-cache}"
SETUP_TRIVY_SKIP_DB="${SETUP_TRIVY_SKIP_DB:-false}"
SETUP_TRIVY_ATTEMPTS="${SETUP_TRIVY_ATTEMPTS:-8}"
SETUP_TRIVY_BACKOFF="${SETUP_TRIVY_BACKOFF:-10}"
SETUP_TRIVY_MAX_BACKOFF="${SETUP_TRIVY_MAX_BACKOFF:-120}"
SETUP_TRIVY_CURL_RETRIES="${SETUP_TRIVY_CURL_RETRIES:-3}"

log() {
    echo "setup-trivy: $*" >&2
}

die() {
    log "error: $*"
    exit 1
}

for knob in SETUP_TRIVY_ATTEMPTS SETUP_TRIVY_BACKOFF SETUP_TRIVY_MAX_BACKOFF SETUP_TRIVY_CURL_RETRIES; do
    [[ "${!knob}" =~ ^[0-9]+$ ]] || die "${knob} must be a non-negative integer, got '${!knob}'"
done

[[ "${SETUP_TRIVY_ATTEMPTS}" -ge 1 ]] || die "SETUP_TRIVY_ATTEMPTS must be at least 1"

# retry <description> <command...>
#
# Runs the command until it succeeds or SETUP_TRIVY_ATTEMPTS is exhausted,
# sleeping SETUP_TRIVY_BACKOFF seconds after the first failure and doubling
# the delay after each subsequent failure, capped at SETUP_TRIVY_MAX_BACKOFF.
retry() {
    local description="$1"
    shift

    local attempt=1
    local delay="${SETUP_TRIVY_BACKOFF}"
    local rc

    while true; do
        "$@" && return 0
        rc=$?

        if [[ "${attempt}" -ge "${SETUP_TRIVY_ATTEMPTS}" ]]; then
            log "${description} failed after ${attempt} attempts (last exit code ${rc})"
            return "${rc}"
        fi

        log "${description} failed (attempt ${attempt}/${SETUP_TRIVY_ATTEMPTS}, exit code ${rc}); retrying in ${delay}s"
        sleep "${delay}"

        attempt=$((attempt + 1))
        delay=$((delay * 2))
        if [[ "${delay}" -gt "${SETUP_TRIVY_MAX_BACKOFF}" ]]; then
            delay="${SETUP_TRIVY_MAX_BACKOFF}"
        fi
    done
}

# Trivy publishes Linux tarballs as Linux-64bit and Linux-ARM64.
platform() {
    local os arch

    os="$(uname -s)"
    arch="$(uname -m)"

    [[ "${os}" == "Linux" ]] || die "unsupported OS '${os}'; only Linux is supported"

    case "${arch}" in
        x86_64 | amd64) echo "Linux-64bit" ;;
        aarch64 | arm64) echo "Linux-ARM64" ;;
        *) die "unsupported architecture '${arch}'" ;;
    esac
}

# SHA-256 checksums from trivy_<version>_checksums.txt on the GitHub release.
# Add an entry here when bumping TRIVY_VERSION.
pinned_sha256() {
    case "$1" in
        0.70.0/Linux-64bit) echo "8b4376d5d6befe5c24d503f10ff136d9e0c49f9127a4279fd110b727929a5aa9" ;;
        0.70.0/Linux-ARM64) echo "2f6bb988b553a1bbac6bdd1ce890f5e412439564e17522b88a4541b4f364fc8d" ;;
        *) return 1 ;;
    esac
}

# download_and_verify <url> <sha256> <dest>
#
# One download attempt. curl's own --retry handles quick blips inside an
# attempt; the outer retry() loop adds longer backoff for sustained outages
# and also re-downloads when the checksum does not match.
download_and_verify() {
    local url="$1" sha256="$2" dest="$3"

    rm -f "${dest}"

    curl --fail --silent --show-error --location \
        --connect-timeout 20 --max-time 300 \
        --retry "${SETUP_TRIVY_CURL_RETRIES}" --retry-delay 5 --retry-all-errors \
        --output "${dest}" "${url}" || return

    if ! echo "${sha256}  ${dest}" | sha256sum --check --status; then
        log "checksum mismatch for ${url}: expected ${sha256}, got $(sha256sum "${dest}" | cut -d' ' -f1)"
        return 1
    fi
}

install_trivy() {
    local installed plat tarball url sha256

    if [[ -x "${TRIVY_INSTALL_DIR}/trivy" ]]; then
        installed="$("${TRIVY_INSTALL_DIR}/trivy" --version 2>/dev/null || true)"
        if [[ $'\n'"${installed}"$'\n' == *$'\n'"Version: ${TRIVY_VERSION}"$'\n'* ]]; then
            log "trivy ${TRIVY_VERSION} already installed in ${TRIVY_INSTALL_DIR}"
            return 0
        fi
    fi

    plat="$(platform)"
    tarball="trivy_${TRIVY_VERSION}_${plat}.tar.gz"
    url="${SETUP_TRIVY_DOWNLOAD_URL:-https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/${tarball}}"
    sha256="${SETUP_TRIVY_SHA256:-$(pinned_sha256 "${TRIVY_VERSION}/${plat}" || true)}"

    [[ -n "${sha256}" ]] ||
        die "no pinned checksum for trivy ${TRIVY_VERSION} (${plat}); add it to pinned_sha256 or set SETUP_TRIVY_SHA256"

    workdir="$(mktemp -d)"
    trap 'rm -rf "${workdir}"' EXIT

    log "downloading ${url}"
    retry "trivy download" download_and_verify "${url}" "${sha256}" "${workdir}/${tarball}"

    tar -xzf "${workdir}/${tarball}" -C "${workdir}" trivy
    mkdir -p "${TRIVY_INSTALL_DIR}"
    install -m 0755 "${workdir}/trivy" "${TRIVY_INSTALL_DIR}/trivy"

    log "installed trivy ${TRIVY_VERSION} to ${TRIVY_INSTALL_DIR}"
}

# A previous run in the same job exported TRIVY_SKIP_DB_UPDATE=true, which
# trivy rejects in combination with --download-db-only, so drop it here.
download_db() {
    env -u TRIVY_SKIP_DB_UPDATE \
        timeout --kill-after=10s 600s \
        "${TRIVY_INSTALL_DIR}/trivy" image \
        --download-db-only \
        --no-progress \
        --skip-version-check \
        --cache-dir "${TRIVY_CACHE_DIR}"
}

workdir=""
install_trivy

if [[ "${SETUP_TRIVY_SKIP_DB}" != "true" ]]; then
    mkdir -p "${TRIVY_CACHE_DIR}"
    log "downloading vulnerability DB into ${TRIVY_CACHE_DIR}"
    retry "vulnerability DB download" download_db
fi

if [[ -n "${GITHUB_PATH:-}" ]]; then
    echo "${TRIVY_INSTALL_DIR}" >>"${GITHUB_PATH}"
fi

if [[ -n "${GITHUB_ENV:-}" ]]; then
    {
        echo "TRIVY_CACHE_DIR=${TRIVY_CACHE_DIR}"
        echo "TRIVY_SKIP_VERSION_CHECK=true"
        if [[ "${SETUP_TRIVY_SKIP_DB}" != "true" ]]; then
            echo "TRIVY_SKIP_DB_UPDATE=true"
        fi
    } >>"${GITHUB_ENV}"
fi
