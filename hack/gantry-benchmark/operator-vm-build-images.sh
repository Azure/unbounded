#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: operator-vm-build-images.sh <subscription> <baseline-acr> <gantry-acr> <source-revision> <source-short>" >&2
  exit 2
fi

subscription_id=$1
baseline_acr=$2
gantry_acr=$3
source_revision=$4
source_short=$5
baseline_login="${baseline_acr}.azurecr.io"
gantry_login="${gantry_acr}.azurecr.io"
log_file=/var/log/gantry-benchmark/deployment-images.log

. /etc/gantry-benchmark/env
export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
cd "$BENCHMARK_REPO_ROOT"

az login --identity --allow-no-subscriptions --output none >>"$log_file" 2>&1
az account set --subscription "$subscription_id"

image_state=/var/lib/gantry-benchmark/deployment-images.env
if [[ -f "$image_state" ]]; then
  recorded_revision=$(sed -n "s/^SOURCE_REVISION='\([^']*\)'$/\1/p" "$image_state")
  if [[ "$recorded_revision" == "$source_revision" ]]; then
    # shellcheck source=/dev/null
    . "$image_state"
    if [[ "$GANTRY_IMAGE" == "$gantry_login/gantry@sha256:"* &&
      "$BASELINE_PROBE_IMAGE" == "$baseline_login/gantry-deploy-probe@sha256:"* ]]; then
      jq -cn --arg gantry_image "$GANTRY_IMAGE" --arg baseline_probe_image "$BASELINE_PROBE_IMAGE" \
        '{gantry_image:$gantry_image,baseline_probe_image:$baseline_probe_image}' | \
        sed 's/^/DEPLOYMENT_IMAGES_JSON=/'
      exit 0
    fi
  fi
fi

registry_login() {
  local acr=$1
  local login=$2
  local token
  local attempt

  for attempt in $(seq 1 18); do
    if token=$(az acr login --name "$acr" --expose-token --query accessToken -o tsv 2>>"$log_file"); then
      printf '%s' "$token" | podman login "$login" \
        --username 00000000-0000-0000-0000-000000000000 \
        --password-stdin >>"$log_file" 2>&1
      unset token
      return
    fi
    sleep 10
  done

  echo "failed to authenticate to $login" >&2
  return 1
}

registry_login "$gantry_acr" "$gantry_login"
gantry_tag="$gantry_login/gantry:benchmark-$source_short"
podman build --isolation chroot --platform linux/amd64 \
  --build-arg "VERSION=benchmark-$source_short" \
  --build-arg "GIT_COMMIT=$source_revision" \
  --tag "$gantry_tag" --file images/gantry/Containerfile . >>"$log_file" 2>&1
gantry_digest_file=/var/lib/gantry-benchmark/gantry-deploy.digest
podman push --digestfile "$gantry_digest_file" "$gantry_tag" >>"$log_file" 2>&1
gantry_digest=$(tr -d '[:space:]' <"$gantry_digest_file")
podman logout "$gantry_login" >>"$log_file" 2>&1

registry_login "$baseline_acr" "$baseline_login"
podman pull mcr.microsoft.com/cbl-mariner/busybox:2.0 >>"$log_file" 2>&1
probe_tag="$baseline_login/gantry-deploy-probe:$source_revision"
podman tag mcr.microsoft.com/cbl-mariner/busybox:2.0 "$probe_tag"
probe_digest_file=/var/lib/gantry-benchmark/baseline-probe.digest
podman push --digestfile "$probe_digest_file" "$probe_tag" >>"$log_file" 2>&1
probe_digest=$(tr -d '[:space:]' <"$probe_digest_file")
podman logout "$baseline_login" >>"$log_file" 2>&1

GANTRY_IMAGE="$gantry_login/gantry@$gantry_digest"
BASELINE_PROBE_IMAGE="$baseline_login/gantry-deploy-probe@$probe_digest"
cat >"$image_state" <<IMAGES
SOURCE_REVISION='$source_revision'
GANTRY_IMAGE='$GANTRY_IMAGE'
BASELINE_PROBE_IMAGE='$BASELINE_PROBE_IMAGE'
IMAGES
chmod 0600 "$image_state"

jq -cn --arg gantry_image "$GANTRY_IMAGE" --arg baseline_probe_image "$BASELINE_PROBE_IMAGE" \
  '{gantry_image:$gantry_image,baseline_probe_image:$baseline_probe_image}' | \
  sed 's/^/DEPLOYMENT_IMAGES_JSON=/'
