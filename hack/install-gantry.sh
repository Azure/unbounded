#!/usr/bin/env sh
set -eu

repository=${GANTRY_REPOSITORY:-Azure/unbounded}
version=${GANTRY_VERSION:-}
: "${HOME:?HOME must be set}"
install_dir=${GANTRYCTL_INSTALL_DIR:-$HOME/.local/bin}
action=install

case ${1:-} in
  install)
    shift
    ;;
  uninstall)
    action=uninstall
    shift
    ;;
  ""|--*) ;;
  *)
    echo "usage: install-gantry.sh [install|uninstall] [gantryctl flags]" >&2
    exit 2
    ;;
esac

if [ "$action" = uninstall ] && [ -z "$version" ] && [ -x "${install_dir}/gantryctl" ]; then
  "${install_dir}/gantryctl" uninstall "$@"
  printf '\nStandalone Gantry uninstalled. gantryctl remains at %s/gantryctl.\n' "$install_dir"
  exit 0
fi

if [ -z "$version" ]; then
  latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${repository}/releases/latest")
  version=${latest_url##*/}
fi
case "$version" in
  v*) ;;
  *) version="v${version}" ;;
esac

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *)
    echo "unsupported operating system: $(uname -s)" >&2
    exit 1
    ;;
esac

case $(uname -m) in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *)
    echo "unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

archive="gantryctl-${os}-${arch}.tar.gz"
release_base="https://github.com/${repository}/releases/download/${version}"
temp_dir=$(mktemp -d)
trap 'rm -rf "$temp_dir"' EXIT HUP INT TERM

if ! command -v cosign >/dev/null 2>&1; then
  echo "cosign is required to verify the Gantry release signature" >&2
  exit 1
fi

curl -fsSL "${release_base}/${archive}" -o "${temp_dir}/${archive}"
curl -fsSL "${release_base}/checksums.txt" -o "${temp_dir}/checksums.txt"
curl -fsSL "${release_base}/checksums.txt.bundle.json" -o "${temp_dir}/checksums.txt.bundle.json"

identity="https://github.com/${repository}/.github/workflows/release.yaml@refs/tags/${version}"
cosign verify-blob \
  --bundle "${temp_dir}/checksums.txt.bundle.json" \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${temp_dir}/checksums.txt" >/dev/null

expected=$(awk -v name="$archive" '$2 == name || $2 == ("*" name) { print $1; exit }' "${temp_dir}/checksums.txt")
if [ -z "$expected" ]; then
  echo "release checksum for ${archive} was not found" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${temp_dir}/${archive}" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "${temp_dir}/${archive}" | awk '{print $1}')
else
  echo "sha256sum or shasum is required" >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "checksum verification failed for ${archive}" >&2
  exit 1
fi

tar -xzf "${temp_dir}/${archive}" -C "$temp_dir"
mkdir -p "$install_dir"
if command -v install >/dev/null 2>&1; then
  install -m 0755 "${temp_dir}/gantryctl" "${install_dir}/gantryctl"
else
  cp "${temp_dir}/gantryctl" "${install_dir}/gantryctl"
  chmod 0755 "${install_dir}/gantryctl"
fi

repository_owner=${repository%%/*}
registry_owner=$(printf '%s' "$repository_owner" | tr '[:upper:]' '[:lower:]')
if [ "$action" = uninstall ]; then
  "${install_dir}/gantryctl" uninstall "$@"
  printf '\nStandalone Gantry uninstalled. gantryctl remains at %s/gantryctl.\n' "$install_dir"
else
  image=${GANTRY_IMAGE:-ghcr.io/${registry_owner}/gantry:${version}}
  "${install_dir}/gantryctl" install --image "$image" "$@"

  printf '\ngantryctl installed at %s/gantryctl\n' "$install_dir"
  case ":${PATH}:" in
    *":${install_dir}:"*) ;;
    *) printf 'Add %s to PATH before running post-install registry commands.\n' "$install_dir" ;;
  esac
  printf 'Configure a registry: gantryctl registry add <registry-host> --auth delegated\n'
  printf 'Uninstall and restore host files: gantryctl uninstall\n'
fi
