#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# verify-release-artifacts: prove that a downloaded release was built by this
# repository's release workflow, at this tag, from this commit.
#
# Shared by the deploy job and the forced-publish job in
# .github/workflows/release-upgrade.yaml. Those are the only two paths that can
# publish a release, and they previously would have carried separate copies of
# this logic. The rollout gate next door already demonstrated where that leads:
# two copies of one check drifted until the weaker one let a broken release
# through.
#
# This needs no cluster access, which is the point. A forced publish exists
# precisely for the case where the soak cluster is unreachable, and the soak is
# the only thing it is allowed to skip. Provenance is not negotiable: forcing
# means "we accept an unsoaked release", never "we accept an unverified one".
#
# Usage:
#   hack/release/verify-release-artifacts.sh <tag> <expected-commit> [dist-dir]
#
# Contract (provided by the calling workflow):
#   GITHUB_REPOSITORY   owner/repo, used to build the certificate identity.
#
# What is checked:
#   1. Each signed blob (manifest tarball, operator manifest, release BOM)
#      carries a Sigstore bundle whose certificate identity is this repo's
#      release.yaml AT THIS TAG. That binding is what makes the signature mean
#      anything: a signature from release.yaml@refs/heads/main, or from a fork,
#      is rejected.
#   2. The BOM's recorded tag and commit match what the caller expected.
#   3. Every image the BOM pins is itself signed by the same identity.
#
# The expected commit is passed in rather than read from the working tree. The
# check is about the release, not about wherever this script happens to be run
# from, and an implicit 'git rev-parse HEAD' silently becomes a tautology if the
# caller's checkout ever changes.

set -euo pipefail

TAG="${1:?usage: verify-release-artifacts.sh <tag> <expected-commit> [dist-dir]}"
EXPECTED_COMMIT="${2:?expected commit is required}"
DIST="${3:-dist}"

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"

for tool in cosign jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "::error::verify-release-artifacts.sh requires ${tool} on PATH"
    exit 2
  fi
done

# The tag is escaped before it goes into the certificate identity pattern.
# Unescaped, the dots in v0.2.5 are regex wildcards, so the identity would also
# match a signature made at a tag like v0X2Y5. The tag shape is validated
# upstream so this is not reachable today, but this is the check that decides
# whether a release was built by this repository, and it should not depend on
# something two workflows away for its correctness.
TAG_PATTERN="${TAG//./\\.}"

IDENTITY="^https://github.com/${GITHUB_REPOSITORY}/\.github/workflows/release\.yaml@refs/tags/${TAG_PATTERN}$"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

ARCHIVE="${DIST}/unbounded-manifests-${TAG}.tar.gz"
OPERATOR_MANIFEST="${DIST}/unbounded-operator-${TAG}.yaml"
BOM="${DIST}/unbounded-release-bom-${TAG}.json"

echo "Verifying release artifacts for ${TAG} (expected commit ${EXPECTED_COMMIT})"

for artifact in "${ARCHIVE}" "${OPERATOR_MANIFEST}" "${BOM}"; do
  if [[ ! -f "$artifact" ]]; then
    echo "::error::missing release artifact ${artifact}; the release is incomplete"
    exit 1
  fi

  if [[ ! -f "${artifact}.bundle.json" ]]; then
    echo "::error::missing signature bundle for ${artifact}"
    exit 1
  fi

  cosign verify-blob \
    --bundle "${artifact}.bundle.json" \
    --certificate-identity-regexp "${IDENTITY}" \
    --certificate-oidc-issuer "${OIDC_ISSUER}" \
    "${artifact}"
done

bom_tag="$(jq -r '.release.tag' "${BOM}")"
if [[ "$bom_tag" != "$TAG" ]]; then
  echo "::error::release BOM records tag ${bom_tag}, expected ${TAG}"
  exit 1
fi

bom_commit="$(jq -r '.release.gitCommit' "${BOM}")"
if [[ "$bom_commit" != "$EXPECTED_COMMIT" ]]; then
  echo "::error::release BOM records commit ${bom_commit}, expected ${EXPECTED_COMMIT}"
  exit 1
fi

# Piping into a while loop would run the body in a subshell, so a cosign failure
# there could not fail this script. The digests are read into an array first so
# the verification runs in this shell and 'set -e' still applies.
mapfile -t images < <(jq -r '.images[] | (.reference | sub(":[^/]+$"; "")) + "@" + .digest' "${BOM}")

if (( ${#images[@]} == 0 )); then
  echo "::error::release BOM lists no images; refusing to treat it as verified"
  exit 1
fi

for image in "${images[@]}"; do
  cosign verify \
    --certificate-identity-regexp "${IDENTITY}" \
    --certificate-oidc-issuer "${OIDC_ISSUER}" \
    "${image}" >/dev/null
done

echo "OK: ${#images[@]} image(s) and 3 blob(s) verified against ${TAG}@${EXPECTED_COMMIT}"
