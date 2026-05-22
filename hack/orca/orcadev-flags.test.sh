#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# orcadev-flags.test.sh - smoke tests for hack/orca/orcadev-flags.sh.
# Sources the helper script with curated environments and asserts the
# resulting "$@" matches what the recipes expect to forward to
# `go run hack/cmd/orcadev`.
#
# Usage:
#   bash hack/orca/orcadev-flags.test.sh
#
# Designed to run unconditionally - no Kubernetes / kubectl required.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HELPER="${SCRIPT_DIR}/orcadev-flags.sh"

if [[ ! -f "${HELPER}" ]]; then
	echo "missing ${HELPER}" >&2
	exit 1
fi

fail=0

# expect compares the actual "$@" array to the expected one and
# reports a diff on mismatch.
expect() {
	local name="$1"
	shift
	local expected=("$@")

	if [[ ${#expected[@]} -ne ${#GOT[@]} ]]; then
		echo "FAIL ${name}: argc mismatch (want=${#expected[@]} got=${#GOT[@]})"
		echo "  want: ${expected[*]}"
		echo "  got : ${GOT[*]}"
		fail=$((fail + 1))
		return
	fi

	for i in "${!expected[@]}"; do
		if [[ "${expected[i]}" != "${GOT[i]}" ]]; then
			echo "FAIL ${name}: arg[${i}] mismatch (want=${expected[i]} got=${GOT[i]})"
			echo "  want: ${expected[*]}"
			echo "  got : ${GOT[*]}"
			fail=$((fail + 1))
			return
		fi
	done

	echo "PASS ${name}"
}

# Run the helper in a subshell with controlled env. The helper sources
# the .env file at ENV_FILE; we use /dev/null so no real .env leaks
# into the test.
run_helper() {
	# shellcheck disable=SC2034 # GOT is consumed by expect()
	mapfile -d '' GOT < <(
		ENV_FILE=/dev/null NAMESPACE="${NAMESPACE_OVERRIDE:-unbounded-kube}" \
			ORIGIN_DRIVER="${ORIGIN_DRIVER:-}" \
			ORIGIN_ID="${ORIGIN_ID:-}" \
			AZURE_STORAGE_ACCOUNT="${AZURE_STORAGE_ACCOUNT:-}" \
			AZURE_STORAGE_KEY="${AZURE_STORAGE_KEY:-}" \
			AZURE_CONTAINER="${AZURE_CONTAINER:-}" \
			AZUREBLOB_ENDPOINT="${AZUREBLOB_ENDPOINT:-}" \
			ORIGIN_AWSS3_ENDPOINT="${ORIGIN_AWSS3_ENDPOINT:-}" \
			ORIGIN_AWSS3_REGION="${ORIGIN_AWSS3_REGION:-}" \
			ORIGIN_AWSS3_BUCKET="${ORIGIN_AWSS3_BUCKET:-}" \
			ORCA_AWSS3_ACCESS_KEY="${ORCA_AWSS3_ACCESS_KEY:-}" \
			ORCA_AWSS3_SECRET_KEY="${ORCA_AWSS3_SECRET_KEY:-}" \
			CACHESTORE_ENDPOINT="${CACHESTORE_ENDPOINT:-}" \
			CACHESTORE_BUCKET="${CACHESTORE_BUCKET:-}" \
			CACHESTORE_REGION="${CACHESTORE_REGION:-}" \
			ORCA_CACHESTORE_S3_ACCESS_KEY="${ORCA_CACHESTORE_S3_ACCESS_KEY:-}" \
			ORCA_CACHESTORE_S3_SECRET_KEY="${ORCA_CACHESTORE_S3_SECRET_KEY:-}" \
			bash -c '. "$0" && printf "%s\0" "$@"' "${HELPER}"
	)
}

# Case 1: no .env, all defaults.
unset ORIGIN_DRIVER ORIGIN_ID AZURE_STORAGE_ACCOUNT AZURE_STORAGE_KEY AZURE_CONTAINER AZUREBLOB_ENDPOINT \
	ORIGIN_AWSS3_ENDPOINT ORIGIN_AWSS3_REGION ORIGIN_AWSS3_BUCKET ORCA_AWSS3_ACCESS_KEY ORCA_AWSS3_SECRET_KEY \
	CACHESTORE_ENDPOINT CACHESTORE_BUCKET CACHESTORE_REGION ORCA_CACHESTORE_S3_ACCESS_KEY ORCA_CACHESTORE_S3_SECRET_KEY \
	NAMESPACE_OVERRIDE
run_helper
expect "azureblob defaults" \
	--origin-driver azureblob \
	--origin-account devstoreaccount1 \
	--origin-bucket orca-test \
	--cachestore-bucket orca-cache \
	--cachestore-endpoint http://localhost:30200 \
	--cachestore-region us-east-1 \
	--cachestore-use-path-style=true

# Case 2: awss3 driver against in-cluster localstack, no creds set.
export ORIGIN_DRIVER=awss3
run_helper
expect "awss3 defaults" \
	--origin-driver awss3 \
	--origin-bucket orca-origin \
	--origin-endpoint http://localhost:30200 \
	--origin-region us-east-1 \
	--origin-use-path-style=true \
	--cachestore-bucket orca-cache \
	--cachestore-endpoint http://localhost:30200 \
	--cachestore-region us-east-1 \
	--cachestore-use-path-style=true

# Case 3: awss3 with credentials supplied.
export ORCA_AWSS3_ACCESS_KEY=mykey
export ORCA_AWSS3_SECRET_KEY=mysecret
export ORCA_CACHESTORE_S3_ACCESS_KEY=cachekey
export ORCA_CACHESTORE_S3_SECRET_KEY=cachesecret
run_helper
expect "awss3 with creds" \
	--origin-driver awss3 \
	--origin-bucket orca-origin \
	--origin-endpoint http://localhost:30200 \
	--origin-region us-east-1 \
	--origin-use-path-style=true \
	--origin-access-key mykey \
	--origin-secret-key mysecret \
	--cachestore-bucket orca-cache \
	--cachestore-endpoint http://localhost:30200 \
	--cachestore-region us-east-1 \
	--cachestore-use-path-style=true \
	--cachestore-access-key cachekey \
	--cachestore-secret-key cachesecret

# Case 4: custom NAMESPACE => in-cluster URL still maps to localhost.
unset ORIGIN_DRIVER ORCA_AWSS3_ACCESS_KEY ORCA_AWSS3_SECRET_KEY ORCA_CACHESTORE_S3_ACCESS_KEY ORCA_CACHESTORE_S3_SECRET_KEY
export NAMESPACE_OVERRIDE=custom-ns
export AZUREBLOB_ENDPOINT="http://azurite.custom-ns.svc.cluster.local:10000/devstoreaccount1/"
export CACHESTORE_ENDPOINT="http://localstack.custom-ns.svc.cluster.local:4566"
run_helper
expect "namespace override translates URLs" \
	--origin-driver azureblob \
	--origin-account devstoreaccount1 \
	--origin-bucket orca-test \
	--origin-endpoint http://localhost:30100/devstoreaccount1/ \
	--cachestore-bucket orca-cache \
	--cachestore-endpoint http://localhost:30200 \
	--cachestore-region us-east-1 \
	--cachestore-use-path-style=true

if [[ ${fail} -gt 0 ]]; then
	echo "${fail} test(s) failed" >&2
	exit 1
fi

echo "all tests passed"
