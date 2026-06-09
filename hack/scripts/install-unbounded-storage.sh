#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO="${REPO:-Azure/unbounded-kube}"
VERSION="${VERSION:-latest}"
PREFIX="${PREFIX:-/opt/unbounded-storage}"
SERVICE_NAME="${SERVICE_NAME:-unbounded-storage}"
STORAGE_ARGS="${STORAGE_ARGS:-}"
NO_ENABLE="${NO_ENABLE:-0}"
LOCAL_TARBALL="${LOCAL_TARBALL:-}"

log() { printf '%s\n' "$*"; }
err() { printf 'error: %s\n' "$*" >&2; }

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

# curl/sha256sum are only needed when downloading a release; in LOCAL_TARBALL
# mode we extract a tarball that already exists on disk.
if [[ -z "${LOCAL_TARBALL}" ]]; then
	for tool in curl sha256sum; do
		if ! command -v "${tool}" >/dev/null 2>&1; then
			err "required tool '${tool}' not found in PATH."
			exit 1
		fi
	done
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

if [[ -n "${LOCAL_TARBALL}" ]]; then
	# --- Use a release-layout tarball from disk (no download) -----------------
	log "Installing unbounded-storage (${ARCH}, ${VERSION}) from local tarball ${LOCAL_TARBALL}"

	if [[ ! -f "${LOCAL_TARBALL}" ]]; then
		err "LOCAL_TARBALL '${LOCAL_TARBALL}' does not exist or is not a file."
		exit 1
	fi
	archive="${LOCAL_TARBALL}"
else
	# --- Download + verify a release tarball ----------------------------------
	ARCHIVE="${NAME}.tar.gz"

	# Use the latest-release redirect when VERSION=latest so this script keeps
	# working across future releases without edits; otherwise pin to the tag.
	if [[ "${VERSION}" == "latest" ]]; then
		base_url="https://github.com/${REPO}/releases/latest/download"
	else
		base_url="https://github.com/${REPO}/releases/download/${VERSION}"
	fi

	log "Installing unbounded-storage (${ARCH}, ${VERSION}) from ${REPO}"

	curl_fetch() {
		# -f: fail on HTTP errors, -L: follow redirects (required for the
		# latest-release and asset CDN redirects), -S/-s: quiet but show errors.
		curl -fLsS -o "$2" "$1"
	}

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

# Put the bundled libfabric runtime (lib/libfabric.so*) on the loader search
# path so the daemon finds it without a wrapper script.
Environment=LD_LIBRARY_PATH=${PREFIX}/current/lib

ExecStart=${PREFIX}/current/bin/unbounded-storage ${STORAGE_ARGS}

# Restart whenever the process crashes or otherwise exits.
Restart=always
RestartSec=2s

# No resource restrictions: raise every rlimit to infinity and apply no
# CPU/memory/IO cgroup caps and no sandboxing.
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
	log "Enabling and starting ${SERVICE_NAME} ..."
	systemctl enable --now "${SERVICE_NAME}"
	log "Service status:"
	systemctl --no-pager --full status "${SERVICE_NAME}" || true
fi

log ""
log "Done. unbounded-storage ${VERSION} (${ARCH}) installed."
log "  binary : ${PREFIX}/current/bin/unbounded-storage"
log "  unit   : ${unit_path}"
log "  logs   : journalctl -u ${SERVICE_NAME} -f"
