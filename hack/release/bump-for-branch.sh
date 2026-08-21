#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# bump-for-branch: decide what a branch is allowed to release.
#
# Called by .github/workflows/release-prepare.yaml, which feeds the result to
# next-version.sh.
#
# The versioning rule (RELEASING.md, and #627):
#
#   main          cuts vX.Y.0  - a minor, or a major when explicitly asked
#   release-X.Y   cuts vX.Y.Z  - a patch, and nothing else
#
# Every minor's patch space therefore belongs to exactly one branch, and main
# never enters it. That is what makes a release branch possible at all: without
# it, main and release-0.2 would both compute v0.2.5 and compete for the same
# tag, which pre-1.0 they would do for months, since a minor series here runs
# twenty-odd releases.
#
# This lives outside next-version.sh on purpose. The resolver is a general
# version calculator with sixty cases, most of which exercise train mechanics
# through patch bumps that stay perfectly legal on a release branch. Enforcing
# the regime inside it would invalidate them to express a policy that is really
# about where you are cutting from, not about how versions are computed.
#
# Contract:
#   $1        branch name, `main` or `release-X.Y`
#   MAJOR     "true" to cut a major instead of a minor. Only valid on main, and
#             never derived: a major is always a deliberate human decision.
#
# Output contract:
#   stdout    `bump=` and `series=` lines, in next-version.sh's own key=value
#             style. series is empty for main.
#   stderr    errors
#   exit      0 resolved, 1 refused

set -euo pipefail

fail() {
  echo "::error::$*" >&2
  exit 1
}

BRANCH="${1:-}"
MAJOR="${MAJOR:-false}"

[[ -n "$BRANCH" ]] || fail "no branch given; expected main or release-X.Y"

case "$MAJOR" in
  true | false) ;;
  *) fail "MAJOR must be true or false, got: ${MAJOR}" ;;
esac

# Anchored, and no leading zeros, matching next-version.sh's SEMVER_COMPONENT.
# A branch named release-01.2 would otherwise imply a series that can never
# match a tag, since v01.2.3 is not a version.
RELEASE_BRANCH='^release-(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$'

if [[ "$BRANCH" == "main" ]]; then
  if [[ "$MAJOR" == "true" ]]; then
    BUMP="major"
  else
    BUMP="minor"
  fi

  SERIES=""
elif [[ "$BRANCH" =~ $RELEASE_BRANCH ]]; then
  # A release branch exists to patch a series that has already shipped. Letting
  # it cut a minor would escape the series it is named for and collide with
  # main, which is the exact failure this rule prevents.
  [[ "$MAJOR" != "true" ]] || fail "major is only valid on main; ${BRANCH} cuts patches"

  BUMP="patch"
  SERIES="${BRANCH#release-}"
else
  fail "refusing to release from ${BRANCH}; expected main or release-X.Y"
fi

echo "bump=${BUMP}"
echo "series=${SERIES}"
