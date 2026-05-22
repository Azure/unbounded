#!/usr/bin/env sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# orcadev-flags.sh - shell helper for hack/orca/Makefile targets that
# invoke orcadev. Translates the harness .env settings into the
# corresponding orcadev CLI flags, accounting for the fact that
# rendered Orca manifests use in-cluster service DNS while orcadev
# runs on the host and needs the matching NodePort URLs.
#
# Sourced (not executed) from Makefile recipes. After sourcing, "$@"
# holds the flags. Each recipe then runs:
#
#   . hack/orca/orcadev-flags.sh
#   go run hack/cmd/orcadev "$@" <subcommand> <args>
#
# Environment inputs (set by the Makefile):
#
#   ENV_FILE    path to the harness .env (sourced when present)
#   NAMESPACE   the harness namespace; defaults to unbounded-kube.
#               The in-cluster service URLs we translate include
#               this value, so it MUST match what `render` produced.
#
# Credential flags are forwarded only when .env supplies them.
# Otherwise orcadev's built-in defaults take over (see
# defaultGlobalFlags in hack/cmd/orcadev/orcadev/config.go).

ENV_FILE="${ENV_FILE:-hack/orca/.env}"
NAMESPACE="${NAMESPACE:-unbounded-kube}"

if [ -f "${ENV_FILE}" ]; then
	set -a
	# shellcheck disable=SC1090
	. "${ENV_FILE}"
	set +a
fi

set --

driver="${ORIGIN_DRIVER:-azureblob}"
set -- "$@" --origin-driver "${driver}"

if [ -n "${ORIGIN_ID:-}" ]; then
	set -- "$@" --origin-id "${ORIGIN_ID}"
fi

case "${driver}" in
	azureblob)
		azure_account="${AZURE_STORAGE_ACCOUNT:-devstoreaccount1}"
		azure_container="${AZURE_CONTAINER:-${AZURITE_CONTAINER:-orca-test}}"
		azure_endpoint="${AZUREBLOB_ENDPOINT:-}"
		azure_key="${AZURE_STORAGE_KEY:-}"

		if [ "${azure_endpoint}" = "http://azurite.${NAMESPACE}.svc.cluster.local:10000/devstoreaccount1/" ]; then
			azure_endpoint="http://localhost:30100/devstoreaccount1/"
		fi

		set -- "$@" --origin-account "${azure_account}" --origin-bucket "${azure_container}"

		if [ -n "${azure_endpoint}" ] || [ "${azure_account}" != "devstoreaccount1" ]; then
			set -- "$@" --origin-endpoint "${azure_endpoint}"
		fi

		if [ -n "${azure_key}" ]; then
			set -- "$@" --origin-account-key "${azure_key}"
		elif [ "${azure_account}" != "devstoreaccount1" ]; then
			echo "ERROR: AZURE_STORAGE_KEY is required for AZURE_STORAGE_ACCOUNT=${azure_account}" >&2
			exit 1
		fi
		;;
	awss3)
		origin_endpoint="${ORIGIN_AWSS3_ENDPOINT:-http://localstack.${NAMESPACE}.svc.cluster.local:4566}"
		if [ "${origin_endpoint}" = "http://localstack.${NAMESPACE}.svc.cluster.local:4566" ]; then
			origin_endpoint="http://localhost:30200"
		fi

		set -- "$@" \
			--origin-bucket "${ORIGIN_AWSS3_BUCKET:-orca-origin}" \
			--origin-endpoint "${origin_endpoint}" \
			--origin-region "${ORIGIN_AWSS3_REGION:-us-east-1}" \
			--origin-use-path-style=true

		if [ -n "${ORCA_AWSS3_ACCESS_KEY:-}" ]; then
			set -- "$@" --origin-access-key "${ORCA_AWSS3_ACCESS_KEY}"
		fi

		if [ -n "${ORCA_AWSS3_SECRET_KEY:-}" ]; then
			set -- "$@" --origin-secret-key "${ORCA_AWSS3_SECRET_KEY}"
		fi
		;;
	*)
		echo "ERROR: unknown ORIGIN_DRIVER=${driver}" >&2
		exit 1
		;;
esac

cachestore_endpoint="${CACHESTORE_ENDPOINT:-http://localstack.${NAMESPACE}.svc.cluster.local:4566}"
if [ "${cachestore_endpoint}" = "http://localstack.${NAMESPACE}.svc.cluster.local:4566" ]; then
	cachestore_endpoint="http://localhost:30200"
fi

set -- "$@" \
	--cachestore-bucket "${CACHESTORE_BUCKET:-orca-cache}" \
	--cachestore-endpoint "${cachestore_endpoint}" \
	--cachestore-region "${CACHESTORE_REGION:-us-east-1}" \
	--cachestore-use-path-style=true

if [ -n "${ORCA_CACHESTORE_S3_ACCESS_KEY:-}" ]; then
	set -- "$@" --cachestore-access-key "${ORCA_CACHESTORE_S3_ACCESS_KEY}"
fi

if [ -n "${ORCA_CACHESTORE_S3_SECRET_KEY:-}" ]; then
	set -- "$@" --cachestore-secret-key "${ORCA_CACHESTORE_S3_SECRET_KEY}"
fi
