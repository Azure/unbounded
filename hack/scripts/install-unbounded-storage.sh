#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# install-unbounded-storage.sh - Download the latest unbounded-storage release
# artifact, verify it, install it under a local prefix, and register it as a
# systemd service.
#
# The release pipeline (.github/workflows/release.yaml) publishes, per arch:
#   - unbounded-storage-linux-<arch>.tar.gz
#   - unbounded-storage-linux-<arch>.tar.gz.sha256
#   - unbounded-storage-linux-<arch>.tar.gz.spdx.json   (SBOM)
#   - unbounded-storage-linux-<arch>.tar.gz.bundle.json (cosign signature)
#
# The tarball extracts to unbounded-storage-linux-<arch>/ containing:
#   bin/unbounded-storage   the release binary
#   lib/libfabric.so*       the bundled, version-pinned libfabric runtime
#
# The generated systemd unit puts lib/ on LD_LIBRARY_PATH via an Environment=
# line and execs bin/unbounded-storage directly, so the daemon finds the
# bundled libfabric without any wrapper script.
#
# This script downloads via GitHub's "latest release" redirect, so it does NOT
# need to be edited for each new tag: it will pick up the next release
# automatically. There are no storage release assets attached to the current
# tags yet, so running it before the next release will fail at the download
# step with a clear 404; that is expected.
#
# LOCAL TARBALL MODE: set LOCAL_TARBALL to the path of a release-layout
# tarball on disk to skip the download/verify entirely and install from that
# file instead. This is what CI uses so the smoke test runs the just-built
# binary under systemd without round-tripping through a real GitHub release.
# The tarball must follow the release layout above (bin/unbounded-storage and,
# optionally, lib/libfabric.so*).
#
# Configuration (override via environment):
#   REPO          GitHub repo. Default: Azure/unbounded-kube.
#   VERSION       Release tag to install (e.g. v0.2.0), or "latest".
#                 Default: latest. With LOCAL_TARBALL set this is only used to
#                 name the install directory; "local" is a sensible value.
#   ARCH          Target arch: amd64 or arm64. Default: auto-detected.
#   PREFIX        Install prefix. Default: /opt/unbounded-storage.
#   SERVICE_NAME  systemd unit name. Default: unbounded-storage.
#   STORAGE_ARGS  Extra args passed to the daemon by the unit. Default: empty.
#   NO_ENABLE     If set to 1, install the unit but do not enable/start it.
#   LOCAL_TARBALL Path to a release-layout tarball on disk. When set, the
#                 download/verify steps are skipped and this tarball is
#                 extracted and installed instead. Default: empty (download a
#                 release).
#
# SECURITY NOTE: As requested, the generated unit runs the daemon as root with
# auto-restart and with all sandboxing / resource limits removed. That is a
# deliberately permissive posture; see the "User=root" and "no restrictions"
# sections below before deploying to anything you care about.

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

# The unit execs the bare binary and puts the bundled libfabric on
# LD_LIBRARY_PATH via Environment=, so no wrapper script is needed.
# ${PREFIX}/current keeps the unit stable across upgrades.
#
# As requested:
#   - User=root / Group=root  -> runs with full privileges.
#   - Restart=always          -> restarts whenever it crashes (or exits).
#   - All resource limits set to infinity and every systemd sandbox/cgroup
#     restriction left off -> "no resource restrictions". LimitMEMLOCK in
#     particular is genuinely needed (the daemon pins io_uring buffers and
#     raises RLIMIT_MEMLOCK), so an unlimited memlock is also functional, not
#     just permissive.
cat > "${unit_path}" <<EOF
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
