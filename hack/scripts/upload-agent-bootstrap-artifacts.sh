#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

usage() {
  cat <<'EOF'
Upload rootfs OCI layout archives and bootstrap artifact archives to Azure Blob Storage.

Usage:
  upload-agent-bootstrap-artifacts.sh \
    --account-name <storage-account> \
    --container <container> \
    --version <version-prefix> \
    [--sas <sas-token>] \
    --rootfs-dir <directory> \
    --bootstrap-dir <directory> \
    [--container-path <path-prefix>] \
    [--dry-run]

Destination layout:
  <container-path>/<version>/rootfs/<rootfs archive>
  <container-path>/<version>/rootfs/<rootfs archive>.sha256
  <container-path>/<version>/bootstrap-artifacts/<bootstrap archive>
  <container-path>/<version>/bootstrap-artifacts/<bootstrap archive>.sha256

The SAS value may start with '?'. It must grant blob create/write permissions
and is required unless --dry-run is used.
Only .tar.gz archives and their adjacent .sha256 files are uploaded.
Use --dry-run to print source and destination mappings without uploading.
Uploads use AzCopy v10 for resumable, parallel blob transfers.
EOF
}

account_name=""
container=""
container_path=""
version=""
sas=""
rootfs_dir=""
bootstrap_dir=""
dry_run=false

while (($# > 0)); do
  case "$1" in
    --account-name)
      account_name=${2:-}
      shift 2
      ;;
    --container)
      container=${2:-}
      shift 2
      ;;
    --container-path)
      container_path=${2:-}
      shift 2
      ;;
    --version)
      version=${2:-}
      shift 2
      ;;
    --sas)
      sas=${2:-}
      shift 2
      ;;
    --rootfs-dir)
      rootfs_dir=${2:-}
      shift 2
      ;;
    --bootstrap-dir)
      bootstrap_dir=${2:-}
      shift 2
      ;;
    --dry-run)
      dry_run=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

for required in account_name container version rootfs_dir bootstrap_dir; do
  if [[ -z ${!required} ]]; then
    echo "missing required argument: ${required//_/-}" >&2
    usage >&2
    exit 2
  fi
done

if [[ $dry_run == false && -z $sas ]]; then
  echo "missing required argument: sas" >&2
  usage >&2
  exit 2
fi

if [[ $dry_run == false ]] && ! command -v azcopy >/dev/null 2>&1; then
  echo "azcopy is required" >&2
  exit 1
fi

if [[ ! -d $rootfs_dir ]]; then
  echo "rootfs directory does not exist: $rootfs_dir" >&2
  exit 1
fi

if [[ ! -d $bootstrap_dir ]]; then
  echo "bootstrap directory does not exist: $bootstrap_dir" >&2
  exit 1
fi

sas=${sas#\?}
container_path=${container_path#/}
container_path=${container_path%/}
version=${version#/}
version=${version%/}

if [[ $version == *'/'* ]]; then
  echo "version must be one path segment: $version" >&2
  exit 2
fi

join_blob_path() {
  local suffix=$1

  if [[ -n $container_path ]]; then
    printf '%s/%s/%s' "$container_path" "$version" "$suffix"
  else
    printf '%s/%s' "$version" "$suffix"
  fi
}

upload_file() {
  local file=$1
  local category=$2
  local filename blob_name content_type destination

  filename=$(basename "$file")
  blob_name=$(join_blob_path "$category/$filename")

  case "$filename" in
    *.sha256)
      content_type='text/plain'
      ;;
    *.tar.gz)
      content_type='application/gzip'
      ;;
    *)
      echo "refusing unsupported file: $file" >&2
      return 1
      ;;
  esac

  destination="https://${account_name}.blob.core.windows.net/${container}/${blob_name}"

  if [[ $dry_run == true ]]; then
    printf 'source:      %s\n' "$file"
    printf 'destination: %s\n' "$destination"
    printf 'content-type: %s\n\n' "$content_type"
    return
  fi

  azcopy copy \
    "$file" \
    "${destination}?${sas}" \
    --from-to=LocalBlob \
    --overwrite=true \
    --content-type="$content_type" \
    --log-level=ERROR \
    --output-type=text

  printf 'uploaded: %s\n' "$blob_name"
}

mapfile -d '' rootfs_files < <(
  find "$rootfs_dir" -type f \
    \( -name '*.oci.tar.gz' -o -name '*.oci.tar.gz.sha256' \) \
    -print0 | sort -z
)

mapfile -d '' bootstrap_files < <(
  find "$bootstrap_dir" -type f \
    \( -name 'bootstrap-artifacts-*.tar.gz' -o -name 'bootstrap-artifacts-*.tar.gz.sha256' \) \
    -print0 | sort -z
)

if ((${#rootfs_files[@]} == 0)); then
  echo "no rootfs OCI layout archives found under: $rootfs_dir" >&2
  exit 1
fi

if ((${#bootstrap_files[@]} == 0)); then
  echo "no bootstrap artifact archives found under: $bootstrap_dir" >&2
  exit 1
fi

for file in "${rootfs_files[@]}"; do
  upload_file "$file" rootfs
done

for file in "${bootstrap_files[@]}"; do
  upload_file "$file" bootstrap-artifacts
done

if [[ $dry_run == true ]]; then
  printf 'dry run: %d rootfs files and %d bootstrap artifact files\n' "${#rootfs_files[@]}" "${#bootstrap_files[@]}"
else
  printf 'uploaded %d rootfs files and %d bootstrap artifact files\n' "${#rootfs_files[@]}" "${#bootstrap_files[@]}"
fi
