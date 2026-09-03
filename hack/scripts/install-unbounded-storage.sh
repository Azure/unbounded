#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

REPO="${REPO:-Azure/unbounded-kube}"
VERSION="${VERSION:-latest}"
PREFIX="${PREFIX:-/opt/unbounded-storage}"
SERVICE_NAME="${SERVICE_NAME:-unbounded-storage}"
CONFIG_PATH="${CONFIG_PATH:-/etc/unbounded-storage/config.toml}"
STORAGE_ARGS="${STORAGE_ARGS:-}"
NO_ENABLE="${NO_ENABLE:-0}"
LOCAL_TARBALL="${LOCAL_TARBALL:-}"
HUGEPAGES="${HUGEPAGES:-}"
# Buffer-pool size (bytes) the hugepage reservation is sized for. Matches the
# daemon's default 128 MiB per-shard backing so a fresh install reserves enough
# 2MiB hugepages for the buffer pool. Override with POOL_BYTES=<bytes> (keep it
# in sync with bytes_per_shard in the daemon config). Ignored when HUGEPAGES is
# set to an explicit count.
POOL_BYTES="${POOL_BYTES:-134217728}"

# Optional first positional argument selects where the release-layout tarball
# comes from:
#   (omitted)            download the latest (or VERSION) GitHub release
#   http(s)://...        curl the artifact from the given URL
#   /path or ./path      use the local tarball at that filesystem path
# For backward compatibility, LOCAL_TARBALL=<path> is honored when no argument
# is given.
SOURCE="${1:-${LOCAL_TARBALL}}"

log() { printf '%s\n' "$*"; }
err() { printf 'error: %s\n' "$*" >&2; }

# Classify SOURCE into one of: release | url | file.
if [[ -z "${SOURCE}" ]]; then
	SOURCE_MODE="release"
elif [[ "${SOURCE}" =~ ^https?:// ]]; then
	SOURCE_MODE="url"
else
	SOURCE_MODE="file"
fi

# --- Preconditions ----------------------------------------------------------

if [[ "${EUID}" -ne 0 ]]; then
	err "this installer must run as root (it writes to ${PREFIX} and /etc/systemd/system)."
	err "re-run with sudo."
	exit 1
fi

for tool in systemctl install mktemp uname tar; do
	if ! command -v "${tool}" >/dev/null 2>&1; then
		err "required tool '${tool}' not found in PATH."
		exit 1
	fi
done

# curl and sha256sum are both needed whenever we fetch over the network
# (release or url modes): every published tarball ships a companion .sha256
# checksum that we always verify before installing. A local file is extracted
# as-is.
if [[ "${SOURCE_MODE}" != "file" ]]; then
	if ! command -v curl >/dev/null 2>&1; then
		err "required tool 'curl' not found in PATH."
		exit 1
	fi

	if ! command -v sha256sum >/dev/null 2>&1; then
		err "required tool 'sha256sum' not found in PATH."
		exit 1
	fi
fi

# --- Resolve target arch ----------------------------------------------------

if [[ -z "${ARCH:-}" ]]; then
	machine="$(uname -m)"
	case "${machine}" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*)
		err "unsupported machine architecture '${machine}'. Set ARCH=amd64|arm64 explicitly."
		exit 1
		;;
	esac
fi

if [[ "${ARCH}" != "amd64" && "${ARCH}" != "arm64" ]]; then
	err "ARCH must be 'amd64' or 'arm64', got '${ARCH}'."
	exit 1
fi

if [[ ! "${POOL_BYTES}" =~ ^[0-9]+$ || "${POOL_BYTES}" -le 0 ]]; then
	err "POOL_BYTES must be a positive integer (bytes), got '${POOL_BYTES}'."
	exit 1
fi

NAME="unbounded-storage-linux-${ARCH}"

workdir="$(mktemp -d)"
cleanup() { rm -rf "${workdir}"; }
trap cleanup EXIT

# Both code paths below resolve ${archive} to a release-layout tarball on disk
# (downloaded or supplied via LOCAL_TARBALL). It is then extracted into
# ${extracted} so the install steps that follow are identical regardless of
# where the bits came from.
extracted="${workdir}/extracted"
mkdir -p "${extracted}"

curl_fetch() {
	# -f: fail on HTTP errors, -L: follow redirects (required for the
	# latest-release and asset CDN redirects), -S/-s: quiet but show errors.
	curl -fLsS -o "$2" "$1"
}

if [[ "${SOURCE_MODE}" == "file" ]]; then
	# --- Use a release-layout tarball from disk (no download) -----------------
	log "Installing unbounded-storage (${ARCH}, ${VERSION}) from local file ${SOURCE}"

	if [[ ! -f "${SOURCE}" ]]; then
		err "source '${SOURCE}' does not exist or is not a file."
		exit 1
	fi
	archive="${SOURCE}"
elif [[ "${SOURCE_MODE}" == "url" ]]; then
	# --- Download a release-layout tarball from an explicit URL ----------------
	log "Installing unbounded-storage (${ARCH}, ${VERSION}) from ${SOURCE}"

	ARCHIVE="${NAME}.tar.gz"
	log "Downloading ${SOURCE} ..."
	if ! curl_fetch "${SOURCE}" "${workdir}/${ARCHIVE}"; then
		err "failed to download ${SOURCE}"
		exit 1
	fi

	# Always verify against the sibling .sha256 published alongside the
	# tarball. Refuse to install if the checksum cannot be fetched.
	log "Downloading ${SOURCE}.sha256 ..."
	if ! curl_fetch "${SOURCE}.sha256" "${workdir}/${ARCHIVE}.sha256"; then
		err "failed to download checksum ${SOURCE}.sha256; refusing to install."
		exit 1
	fi

	log "Verifying SHA-256 checksum ..."
	# Rewrite the checksum file to reference the local archive name, since
	# the published .sha256 may point at a differently named artifact.
	expected="$(awk '{print $1; exit}' "${workdir}/${ARCHIVE}.sha256")"
	printf '%s  %s\n' "${expected}" "${ARCHIVE}" >"${workdir}/${ARCHIVE}.sha256"
	(
		cd "${workdir}"
		sha256sum -c "${ARCHIVE}.sha256"
	) || {
		err "checksum verification failed; refusing to install."
		exit 1
	}
	log "Checksum OK."

	archive="${workdir}/${ARCHIVE}"
else
	# --- Download + verify a release tarball from GitHub ----------------------
	ARCHIVE="${NAME}.tar.gz"

	# Use the latest-release redirect when VERSION=latest so this script keeps
	# working across future releases without edits; otherwise pin to the tag.
	if [[ "${VERSION}" == "latest" ]]; then
		base_url="https://github.com/${REPO}/releases/latest/download"
	else
		base_url="https://github.com/${REPO}/releases/download/${VERSION}"
	fi

	log "Installing unbounded-storage (${ARCH}, ${VERSION}) from ${REPO}"

	log "Downloading ${ARCHIVE} ..."
	if ! curl_fetch "${base_url}/${ARCHIVE}" "${workdir}/${ARCHIVE}"; then
		err "failed to download ${base_url}/${ARCHIVE}"
		err "if the next release has not published storage artifacts yet, this 404 is expected."
		exit 1
	fi

	log "Downloading ${ARCHIVE}.sha256 ..."
	if ! curl_fetch "${base_url}/${ARCHIVE}.sha256" "${workdir}/${ARCHIVE}.sha256"; then
		err "failed to download checksum ${base_url}/${ARCHIVE}.sha256"
		exit 1
	fi

	log "Verifying SHA-256 checksum ..."
	# The .sha256 file is produced by `sha256sum NAME.tar.gz` and references the
	# bare archive name, so verify from within the download dir.
	(
		cd "${workdir}"
		sha256sum -c "${ARCHIVE}.sha256"
	) || {
		err "checksum verification failed; refusing to install."
		exit 1
	}
	log "Checksum OK."

	archive="${workdir}/${ARCHIVE}"
fi

log "Extracting ..."
# Strip the top-level unbounded-storage-linux-<arch>/ directory so bin/ and
# lib/ land directly under ${extracted}, independent of the tarball's top-level
# directory name.
tar -xzf "${archive}" -C "${extracted}" --strip-components=1

# --- Validate + install -----------------------------------------------------

if [[ ! -x "${extracted}/bin/unbounded-storage" ]]; then
	err "staged payload does not contain bin/unbounded-storage; layout changed?"
	exit 1
fi

# Install into a versioned directory and flip a 'current' symlink so upgrades
# are atomic and rollbacks are trivial.
release_dir="${PREFIX}/releases/${VERSION}-${ARCH}"
log "Installing to ${release_dir} ..."
rm -rf "${release_dir}"
mkdir -p "${release_dir}"
cp -a "${extracted}/." "${release_dir}/"

ln -sfn "${release_dir}" "${PREFIX}/current"
log "Linked ${PREFIX}/current -> ${release_dir}"

# --- systemd unit -----------------------------------------------------------

unit_path="/etc/systemd/system/${SERVICE_NAME}.service"
log "Writing systemd unit ${unit_path} ..."

reserve_cmd='hp=/sys/kernel/mm/hugepages/hugepages-2048kB; [ -d "$hp" ] || { echo "unbounded-storage: kernel exposes no 2MiB hugepage pool; hugepages are required" >&2; exit 1; }; want='"${HUGEPAGES:-0}"'; if [ "$want" -le 0 ]; then pool=$(( ('"${POOL_BYTES}"' + 2097151) / 2097152 )); n=$(nproc 2>/dev/null || echo 1); [ "$n" -gt 8 ] && n=8; need=$(( pool + 8 * n )); want=$(( need + need / 2 )); else need=$want; fi; cur=$(cat "$hp/nr_hugepages" 2>/dev/null || echo 0); [ "$cur" -lt "$want" ] && echo "$want" > "$hp/nr_hugepages" 2>/dev/null || true; free=$(cat "$hp/free_hugepages" 2>/dev/null || echo 0); if [ "$free" -lt "$need" ] && [ -w /proc/sys/vm/compact_memory ]; then echo 1 > /proc/sys/vm/compact_memory 2>/dev/null || true; [ "$cur" -lt "$want" ] && echo "$want" > "$hp/nr_hugepages" 2>/dev/null || true; free=$(cat "$hp/free_hugepages" 2>/dev/null || echo 0); fi; nr=$(cat "$hp/nr_hugepages" 2>/dev/null || echo 0); echo "unbounded-storage: 2MiB hugepages nr=$nr free=$free (need $need target $want)" >&2; [ "$free" -ge "$need" ] || { echo "unbounded-storage: could not reserve $need free 2MiB hugepages (have $free); free host memory or reserve hugepages at boot" >&2; exit 1; }'
HUGEPAGE_PRE="ExecStartPre=+/bin/sh -c '${reserve_cmd}'"

config_ensure_cmd='d=$(dirname "'"${CONFIG_PATH}"'"); mkdir -p "$d"; [ -f "'"${CONFIG_PATH}"'" ] || : > "'"${CONFIG_PATH}"'"'
CONFIG_PRE="ExecStartPre=+/bin/sh -c '${config_ensure_cmd}'"

cat >"${unit_path}" <<EOF
[Unit]
Description=unbounded-storage daemon
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
Environment=LD_LIBRARY_PATH=${PREFIX}/current/lib
${HUGEPAGE_PRE}
${CONFIG_PRE}
ExecStart=${PREFIX}/current/bin/unbounded-storage --config ${CONFIG_PATH} ${STORAGE_ARGS}
Restart=always
RestartSec=2s

LimitNOFILE=infinity
LimitNPROC=infinity
LimitMEMLOCK=infinity
LimitCORE=infinity
LimitFSIZE=infinity
LimitAS=infinity
LimitDATA=infinity
LimitSTACK=infinity
LimitCPU=infinity
TasksMax=infinity

[Install]
WantedBy=multi-user.target
EOF

chmod 0644 "${unit_path}"

log "Reloading systemd ..."
systemctl daemon-reload

if [[ "${NO_ENABLE}" == "1" ]]; then
	log "NO_ENABLE=1 set; unit installed but not enabled/started."
	log "Start it with: systemctl enable --now ${SERVICE_NAME}"
else
	log "Enabling and (re)starting ${SERVICE_NAME} ..."
	systemctl enable "${SERVICE_NAME}"
	systemctl restart "${SERVICE_NAME}"
	log "Service status:"
	systemctl --no-pager --full status "${SERVICE_NAME}" || true
fi

log ""
log "Done. unbounded-storage ${VERSION} (${ARCH}) installed."
log "  binary : ${PREFIX}/current/bin/unbounded-storage"
log "  config : ${CONFIG_PATH}"
log "  unit   : ${unit_path}"
log "  logs   : journalctl -u ${SERVICE_NAME} -f"
