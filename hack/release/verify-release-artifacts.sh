#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# verify-release-artifacts: prove that a downloaded release was built by this
# repository's release workflow, at this tag, from this commit.
#
# Shared by the deploy and forced-publish jobs in release-upgrade.yaml, the only
# two paths that can publish a release. The rollout gate next door already
# showed where two copies of one check leads: they drift, and the weaker one
# lets a broken release through.
#
# It needs no cluster access, which is the point. Forcing a publish exists for
# when the soak cluster is unreachable, and the soak is the only thing it may
# skip: forcing means "we accept an unsoaked release", never "an unverified
# one".
#
# Usage:
#   hack/release/verify-release-artifacts.sh <tag> <expected-commit> [dist-dir]
#
# Contract (provided by the calling workflow):
#   GITHUB_REPOSITORY   owner/repo, used to build the certificate identity.
#
# Optional:
#   CHECKSUM_FILES      space-separated artifact names the caller is about to
#                       USE, which must be covered by the signed checksums.txt.
#                       GoReleaser signs that file, not the individual binaries,
#                       so this is how a binary gets verified before it runs.
#   RELEASE_ASSETS_FILE a file listing the release's asset names, one per line
#                       (`gh release view --json assets`). Required whenever the
#                       BOM declares artifacts, because that is the only way to
#                       tell whether the release still carries them.
#
# What is checked:
#   1. Each signed blob (manifest tarball, operator manifest, release BOM)
#      carries a Sigstore bundle whose certificate identity is this repo's
#      release.yaml AT THIS TAG. That binding is what makes the signature mean
#      anything: one from @refs/heads/main, or from a fork, is rejected.
#   2. The BOM's recorded tag and commit match what the caller expected.
#   3. Every image the BOM pins is itself signed by the same identity.
#   4. Every CHECKSUM_FILES entry is listed in checksums.txt, that file carries
#      the same signed identity, and the bytes on disk match it.
#   5. The release still carries every artifact the BOM declares, and each
#      declared signature bundle. A deploy consumes three of the six; the rest
#      are what users download, and nothing else would notice their absence.
#
# The expected commit is a parameter, not a 'git rev-parse HEAD': the check is
# about the release, not about wherever this runs from, and an implicit HEAD
# quietly becomes a tautology if a caller's checkout changes.

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

# Escaped: unescaped, the dots in v0.2.5 are wildcards, so the identity would
# also match a signature made at v0X2Y5. Not reachable while the tag shape is
# validated upstream, but this check should not depend on something two
# workflows away for its correctness.
TAG_PATTERN="${TAG//./\\.}"

IDENTITY="^https://github.com/${GITHUB_REPOSITORY}/\.github/workflows/release\.yaml@refs/tags/${TAG_PATTERN}$"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

ARCHIVE="${DIST}/unbounded-manifests-${TAG}.tar.gz"
OPERATOR_MANIFEST="${DIST}/unbounded-operator-${TAG}.yaml"
BOM="${DIST}/unbounded-release-bom-${TAG}.json"
CHECKSUMS="${DIST}/checksums.txt"

# Word-split on purpose: one entry per artifact name.
# shellcheck disable=SC2206
CHECKSUM_TARGETS=( ${CHECKSUM_FILES:-} )

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

# Captured, not piped: `mapfile < <(jq ...)` reports mapfile's exit status, not
# jq's, so a BOM that emitted two images and then failed on a third left the
# array non-empty and this script verified the prefix and passed. A command
# substitution propagates the failure.
if ! image_refs="$(jq -r '.images[] | (.reference | sub(":[^/]+$"; "")) + "@" + .digest' "${BOM}")"; then
  echo "::error::could not read the image list from ${BOM}; refusing to treat it as verified"
  exit 1
fi

if ! bom_image_count="$(jq -r '.images | length' "${BOM}")"; then
  echo "::error::could not count the images in ${BOM}"
  exit 1
fi

mapfile -t images <<<"$image_refs"

# Drop the single empty element `<<<` produces for empty input.
if (( ${#images[@]} == 1 )) && [[ -z "${images[0]}" ]]; then
  images=()
fi

if (( ${#images[@]} == 0 )); then
  echo "::error::release BOM lists no images; refusing to treat it as verified"
  exit 1
fi

# Cross-check against the BOM's own count, so an emission that stops early
# without failing cannot pass either.
if (( ${#images[@]} != bom_image_count )); then
  echo "::error::release BOM lists ${bom_image_count} images but only ${#images[@]} resolved; refusing to treat it as verified"
  exit 1
fi

# The BOM is the release's own account of what it shipped, so it is what
# "complete" is measured against. Checked by NAME against the release's asset
# list rather than by downloading: the storage tarballs are large, a deploy
# never consumes them, and the failure being caught here is a draft that lost
# assets - to a partial upload, or to a hand-run `gh release delete-asset` -
# rather than one whose bytes were altered.
declared="$(jq -r '.artifacts // [] | .[] | .name, (.signatureBundle // empty)' "${BOM}")" || {
  echo "::error::could not read the artifact list from ${BOM}"
  exit 1
}

if [[ -z "$declared" ]]; then
  if jq -e 'has("artifacts")' "${BOM}" >/dev/null 2>&1; then
    echo "::error::release BOM declares an empty artifact list; refusing to treat it as complete"
    exit 1
  fi

  # Older releases predate the field. Backfilling one is a documented path, so
  # this reports rather than fails.
  echo "::notice::release BOM has no artifact list; skipping the completeness check"
else
  if [[ -z "${RELEASE_ASSETS_FILE:-}" ]]; then
    echo "::error::the BOM declares artifacts but RELEASE_ASSETS_FILE was not provided; completeness cannot be checked"
    exit 2
  fi

  if [[ ! -f "$RELEASE_ASSETS_FILE" ]]; then
    echo "::error::no asset list at ${RELEASE_ASSETS_FILE}"
    exit 2
  fi

  # Collected and reported together: a half-uploaded draft is usually missing
  # several things, and one name per run is a poor way to find that out.
  missing=()

  while read -r name; do
    [[ -n "$name" ]] || continue

    grep -qxF -- "$name" "$RELEASE_ASSETS_FILE" || missing+=("$name")
  done <<<"$declared"

  if (( ${#missing[@]} > 0 )); then
    echo "::error::the release is missing $(printf '%s ' "${missing[@]}")- its BOM declares them, so it is incomplete"
    exit 1
  fi

  echo "Release carries all $(wc -l <<<"$declared") declared artifact(s)"
fi

for image in "${images[@]}"; do
  cosign verify \
    --certificate-identity-regexp "${IDENTITY}" \
    --certificate-oidc-issuer "${OIDC_ISSUER}" \
    "${image}" >/dev/null
done

# Binaries are covered by checksums.txt rather than individually: GoReleaser
# signs that one file (.goreleaser.yml), which is what makes verifying it
# equivalent to verifying everything it lists.
if (( ${#CHECKSUM_TARGETS[@]} > 0 )); then
  for artifact in "$CHECKSUMS" "${CHECKSUMS}.bundle.json"; do
    if [[ ! -f "$artifact" ]]; then
      echo "::error::missing ${artifact}; cannot verify $(printf '%s ' "${CHECKSUM_TARGETS[@]}")"
      exit 1
    fi
  done

  cosign verify-blob \
    --bundle "${CHECKSUMS}.bundle.json" \
    --certificate-identity-regexp "${IDENTITY}" \
    --certificate-oidc-issuer "${OIDC_ISSUER}" \
    "${CHECKSUMS}"

  for name in "${CHECKSUM_TARGETS[@]}"; do
    if [[ ! -f "${DIST}/${name}" ]]; then
      echo "::error::${name} was not downloaded, so it cannot be verified"
      exit 1
    fi

    # An artifact absent from the signed list is unverifiable, and
    # --ignore-missing would pass it over in silence.
    if ! grep -qF -- "  ${name}" "$CHECKSUMS"; then
      echo "::error::${name} is not listed in checksums.txt; refusing to treat it as verified"
      exit 1
    fi
  done

  # --ignore-missing because the release lists every platform's artifacts and a
  # caller downloads only what it needs; each target's presence is asserted
  # above, and this fails outright when nothing at all was verified.
  ( cd "$DIST" && sha256sum --ignore-missing -c checksums.txt )
fi

echo "OK: ${#images[@]} image(s), 3 blob(s) and ${#CHECKSUM_TARGETS[@]} checksummed artifact(s) verified against ${TAG}@${EXPECTED_COMMIT}"
