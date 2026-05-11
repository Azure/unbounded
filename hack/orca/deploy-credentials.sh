#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# deploy-credentials.sh - create the orca-credentials Secret holding
# Azure Blob and S3 cachestore credentials. Sourced from .env so secret
# values never land in YAML.
#
# The dev harness defaults to ORIGIN_DRIVER=awss3 (LocalStack as both
# origin and cachestore), in which case AZURE_STORAGE_KEY is optional
# and the Azure key is omitted from the Secret. If you switch to
# ORIGIN_DRIVER=azureblob, AZURE_STORAGE_KEY becomes required.
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:?CLUSTER_NAME must be set}
NAMESPACE=${NAMESPACE:?NAMESPACE must be set}
ENV_FILE=${ENV_FILE:?ENV_FILE must be set}

if [[ -f "${ENV_FILE}" ]]; then
  set -a
  # shellcheck disable=SC1090
  . "${ENV_FILE}"
  set +a
else
  echo "Note: ${ENV_FILE} not found; proceeding with default awss3 origin (LocalStack)."
fi

ORIGIN_DRIVER=${ORIGIN_DRIVER:-awss3}

# LocalStack accepts any non-empty creds; pin to test/test for parity
# with manual aws-cli calls in the init Job. Both the cachestore and
# (when the awss3 origin driver targets in-cluster LocalStack) the
# origin use the same creds.
ORCA_CACHESTORE_S3_ACCESS_KEY=${ORCA_CACHESTORE_S3_ACCESS_KEY:-test}
ORCA_CACHESTORE_S3_SECRET_KEY=${ORCA_CACHESTORE_S3_SECRET_KEY:-test}
ORCA_AWSS3_ACCESS_KEY=${ORCA_AWSS3_ACCESS_KEY:-test}
ORCA_AWSS3_SECRET_KEY=${ORCA_AWSS3_SECRET_KEY:-test}

# Build the kubectl literal flags conditionally so we don't ship empty
# strings as Azure keys in awss3 mode.
literals=(
  "--from-literal=ORCA_CACHESTORE_S3_ACCESS_KEY=${ORCA_CACHESTORE_S3_ACCESS_KEY}"
  "--from-literal=ORCA_CACHESTORE_S3_SECRET_KEY=${ORCA_CACHESTORE_S3_SECRET_KEY}"
  "--from-literal=ORCA_AWSS3_ACCESS_KEY=${ORCA_AWSS3_ACCESS_KEY}"
  "--from-literal=ORCA_AWSS3_SECRET_KEY=${ORCA_AWSS3_SECRET_KEY}"
)

case "${ORIGIN_DRIVER}" in
  azureblob)
    # In azureblob+Azurite mode (no real Azure account), fall back to
    # the well-known Azurite dev key. This is a public, documented
    # constant baked into Azurite -- not a secret.
    if [[ -z "${AZURE_STORAGE_KEY:-}" ]]; then
      AZURITE_DEV_KEY="Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
      echo "AZURE_STORAGE_KEY not set; using Azurite well-known dev key (account: devstoreaccount1)."
      AZURE_STORAGE_KEY="${AZURITE_DEV_KEY}"
    fi
    literals+=("--from-literal=ORCA_AZUREBLOB_ACCOUNT_KEY=${AZURE_STORAGE_KEY}")
    ;;
  awss3)
    if [[ -n "${AZURE_STORAGE_KEY:-}" ]]; then
      # Allow it to be present so reviewers can switch drivers without
      # editing secrets each time.
      literals+=("--from-literal=ORCA_AZUREBLOB_ACCOUNT_KEY=${AZURE_STORAGE_KEY}")
    fi
    ;;
  *)
    echo "ERROR: unknown ORIGIN_DRIVER=${ORIGIN_DRIVER}" >&2
    exit 1
    ;;
esac

echo "Creating/updating Secret orca-credentials in namespace ${NAMESPACE} (origin driver: ${ORIGIN_DRIVER}) ..."
kubectl --context "kind-${CLUSTER_NAME}" -n "${NAMESPACE}" create secret generic orca-credentials \
  "${literals[@]}" \
  --dry-run=client -o yaml | kubectl --context "kind-${CLUSTER_NAME}" apply -f -

echo "orca-credentials Secret applied."
