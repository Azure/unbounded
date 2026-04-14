#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# Downloads pinned versions of protoc, protoc-gen-go, and protoc-gen-go-grpc
# into hack/bin/ so that proto code generation is reproducible and does not
# depend on system-wide package managers.

set -euo pipefail

# --- Pinned versions --------------------------------------------------------
PROTOC_VERSION="29.3"
PROTOC_GEN_GO_VERSION="v1.36.6"
PROTOC_GEN_GO_GRPC_VERSION="v1.5.1"

# --- Paths ------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HACK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${HACK_DIR}/bin"
INCLUDE_DIR="${BIN_DIR}/include"

mkdir -p "${BIN_DIR}"

# --- Helpers ----------------------------------------------------------------
need_download() {
  local bin="$1" want_version="$2"
  if [[ ! -x "${bin}" ]]; then
    return 0
  fi
  # Strip any leading "v" so that "v1.5.1" and "1.5.1" both match the
  # version string regardless of how the tool formats its output.
  local bare_version="${want_version#v}"
  local got
  got=$("${bin}" --version 2>&1 || true)
  if echo "${got}" | grep -qF "${bare_version}"; then
    return 1
  fi
  return 0
}

detect_os() {
  local os
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "${os}" in
    linux)  echo "linux" ;;
    darwin) echo "osx" ;;
    *)      echo >&2 "unsupported OS: ${os}"; exit 1 ;;
  esac
}

detect_arch() {
  local arch
  arch="$(uname -m)"
  case "${arch}" in
    x86_64)  echo "x86_64" ;;
    aarch64|arm64) echo "aarch_64" ;;
    *)       echo >&2 "unsupported arch: ${arch}"; exit 1 ;;
  esac
}

# --- protoc -----------------------------------------------------------------
install_protoc() {
  if ! need_download "${BIN_DIR}/protoc" "${PROTOC_VERSION}"; then
    echo "protoc ${PROTOC_VERSION} already installed"
    return
  fi

  local os arch zip_name url tmp_dir
  os="$(detect_os)"
  arch="$(detect_arch)"
  zip_name="protoc-${PROTOC_VERSION}-${os}-${arch}.zip"
  url="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/${zip_name}"
  tmp_dir="$(mktemp -d)"

  echo "downloading protoc ${PROTOC_VERSION}..."
  curl -sSfL -o "${tmp_dir}/${zip_name}" "${url}"
  python3 -c "
import zipfile, sys
with zipfile.ZipFile(sys.argv[1]) as z:
    z.extractall(sys.argv[2])
" "${tmp_dir}/${zip_name}" "${tmp_dir}/protoc"

  cp "${tmp_dir}/protoc/bin/protoc" "${BIN_DIR}/protoc"
  chmod +x "${BIN_DIR}/protoc"

  # Copy well-known types (google/protobuf/*.proto) so protoc can find them.
  rm -rf "${INCLUDE_DIR}"
  cp -r "${tmp_dir}/protoc/include" "${INCLUDE_DIR}"

  rm -rf "${tmp_dir}"
  echo "installed protoc ${PROTOC_VERSION} -> ${BIN_DIR}/protoc"
}

# --- protoc-gen-go ----------------------------------------------------------
install_protoc_gen_go() {
  if ! need_download "${BIN_DIR}/protoc-gen-go" "${PROTOC_GEN_GO_VERSION}"; then
    echo "protoc-gen-go ${PROTOC_GEN_GO_VERSION} already installed"
    return
  fi

  echo "installing protoc-gen-go ${PROTOC_GEN_GO_VERSION}..."
  GOBIN="${BIN_DIR}" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
  echo "installed protoc-gen-go ${PROTOC_GEN_GO_VERSION} -> ${BIN_DIR}/protoc-gen-go"
}

# --- protoc-gen-go-grpc -----------------------------------------------------
install_protoc_gen_go_grpc() {
  if ! need_download "${BIN_DIR}/protoc-gen-go-grpc" "${PROTOC_GEN_GO_GRPC_VERSION}"; then
    echo "protoc-gen-go-grpc ${PROTOC_GEN_GO_GRPC_VERSION} already installed"
    return
  fi

  echo "installing protoc-gen-go-grpc ${PROTOC_GEN_GO_GRPC_VERSION}..."
  GOBIN="${BIN_DIR}" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
  echo "installed protoc-gen-go-grpc ${PROTOC_GEN_GO_GRPC_VERSION} -> ${BIN_DIR}/protoc-gen-go-grpc"
}

# --- Main -------------------------------------------------------------------
install_protoc
install_protoc_gen_go
install_protoc_gen_go_grpc

echo "all proto tools ready in ${BIN_DIR}"
