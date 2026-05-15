#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# install-mercury.sh - Idempotently build and install Mercury (mercury-hpc)
# into a local prefix for use by the unbounded-storage Rust crate.
#
# The unbounded-storage crate links against Mercury via pkg-config. Rather
# than depending on a system package (Mercury is not packaged on most
# distros, and the version matters for ABI), we build a pinned release from
# source and install it under a project-local prefix.
#
# Behaviour:
#   - If $MERCURY_PREFIX/lib/pkgconfig/mercury.pc already advertises the
#     requested $MERCURY_VERSION, exit 0 immediately. This makes the script
#     safe to call from Make targets and CI caches.
#   - Otherwise, clone Mercury at the pinned tag into a scratch source tree
#     ($MERCURY_SRC), configure with CMake (libfabric NA + SM transport
#     enabled), build, and install into $MERCURY_PREFIX.
#
# Required system packages (install separately, e.g. via apt):
#   - cmake, build-essential (gcc/g++/make), pkg-config, git
#   - libfabric-dev (provides the libfabric NA backend)
#
# Configuration (override via environment):
#   MERCURY_VERSION  Mercury release tag to build. Default: v2.3.1.
#   MERCURY_PREFIX   Install prefix. Default: $REPO_ROOT/tmp/mercury-prefix.
#   MERCURY_SRC      Scratch source tree. Default: $REPO_ROOT/tmp/mercury-src.
#   MERCURY_REPO     Upstream repo. Default: https://github.com/mercury-hpc/mercury.git
#   JOBS             Parallel build jobs. Default: nproc.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

MERCURY_VERSION="${MERCURY_VERSION:-v2.3.1}"
MERCURY_PREFIX="${MERCURY_PREFIX:-${REPO_ROOT}/tmp/mercury-prefix}"
MERCURY_SRC="${MERCURY_SRC:-${REPO_ROOT}/tmp/mercury-src}"
MERCURY_REPO="${MERCURY_REPO:-https://github.com/mercury-hpc/mercury.git}"
JOBS="${JOBS:-$(nproc 2>/dev/null || echo 4)}"

# Strip leading 'v' from tag for pkg-config comparison (Mercury's mercury.pc
# advertises "2.3.1", the git tag is "v2.3.1").
expected_pc_version="${MERCURY_VERSION#v}"

pc_file="${MERCURY_PREFIX}/lib/pkgconfig/mercury.pc"
if [[ -f "${pc_file}" ]]; then
  installed_version="$(PKG_CONFIG_PATH="${MERCURY_PREFIX}/lib/pkgconfig" \
    pkg-config --modversion mercury 2>/dev/null || true)"
  if [[ "${installed_version}" == "${expected_pc_version}" ]]; then
    echo "Mercury ${installed_version} already installed at ${MERCURY_PREFIX}; skipping build."
    exit 0
  fi
  echo "Mercury at ${MERCURY_PREFIX} reports version '${installed_version}', need '${expected_pc_version}'; rebuilding."
fi

echo "Building Mercury ${MERCURY_VERSION} -> ${MERCURY_PREFIX}"

# Verify required tools are present early with a clear message.
for tool in git cmake make pkg-config; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "error: required tool '${tool}' not found in PATH" >&2
    exit 1
  fi
done

# Verify libfabric is available (needed for the OFI NA backend).
if ! pkg-config --exists libfabric; then
  echo "error: libfabric not found via pkg-config. Install libfabric-dev." >&2
  exit 1
fi

# Fetch / refresh the source tree at the requested tag.
if [[ ! -d "${MERCURY_SRC}/.git" ]]; then
  echo "Cloning ${MERCURY_REPO} -> ${MERCURY_SRC}"
  rm -rf "${MERCURY_SRC}"
  mkdir -p "$(dirname "${MERCURY_SRC}")"
  git clone --recurse-submodules --depth 1 --branch "${MERCURY_VERSION}" \
    "${MERCURY_REPO}" "${MERCURY_SRC}"
else
  echo "Updating existing source tree ${MERCURY_SRC} to ${MERCURY_VERSION}"
  git -C "${MERCURY_SRC}" fetch --depth 1 origin "refs/tags/${MERCURY_VERSION}:refs/tags/${MERCURY_VERSION}"
  git -C "${MERCURY_SRC}" checkout --force "${MERCURY_VERSION}"
  git -C "${MERCURY_SRC}" submodule update --init --recursive --depth 1
fi

build_dir="${MERCURY_SRC}/build"
rm -rf "${build_dir}"
mkdir -p "${build_dir}"

cmake -S "${MERCURY_SRC}" -B "${build_dir}" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_INSTALL_PREFIX="${MERCURY_PREFIX}" \
  -DBUILD_SHARED_LIBS=ON \
  -DBUILD_TESTING=OFF \
  -DBUILD_EXAMPLES=OFF \
  -DMERCURY_USE_BOOST_PP=OFF \
  -DMERCURY_USE_CHECKSUMS=OFF \
  -DNA_USE_SM=ON \
  -DNA_USE_OFI=ON

cmake --build "${build_dir}" --parallel "${JOBS}"
cmake --install "${build_dir}"

# Sanity check: pkg-config should now agree.
installed_version="$(PKG_CONFIG_PATH="${MERCURY_PREFIX}/lib/pkgconfig" \
  pkg-config --modversion mercury)"
if [[ "${installed_version}" != "${expected_pc_version}" ]]; then
  echo "error: post-install mercury.pc reports '${installed_version}', expected '${expected_pc_version}'" >&2
  exit 1
fi

echo "Mercury ${installed_version} installed at ${MERCURY_PREFIX}"
