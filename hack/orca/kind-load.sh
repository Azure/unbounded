#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# kind-load.sh - sideload the Orca container image into the Kind nodes.
#
# Kind clusters can't pull from the local container engine's image
# store directly. This script saves the image to a tarball with the
# configured CONTAINER_ENGINE and feeds it to `kind load image-archive`.
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:?CLUSTER_NAME must be set}
ORCA_IMAGE=${ORCA_IMAGE:?ORCA_IMAGE must be set}
CONTAINER_ENGINE=${CONTAINER_ENGINE:-podman}

if ! command -v kind >/dev/null 2>&1; then
  echo "kind is not installed." >&2
  exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "${tmpdir}"' EXIT

archive="${tmpdir}/orca.tar"
echo "Saving ${ORCA_IMAGE} to ${archive} via ${CONTAINER_ENGINE} ..."
"${CONTAINER_ENGINE}" save -o "${archive}" "${ORCA_IMAGE}"

echo "Loading image into Kind cluster '${CLUSTER_NAME}' ..."
kind load image-archive "${archive}" --name "${CLUSTER_NAME}"

echo "Image loaded."
