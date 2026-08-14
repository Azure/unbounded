#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# next-version: work out which tag a release-prepare run should mint.
#
# Called by .github/workflows/release-prepare.yaml, which owns pushing the tag.
# This script only decides what the tag should be, which is the part that used
# to be inline in the workflow where the only way to test it was to push a tag
# and see what happened. Three real problems came out of that:
#
#   - `bump` was applied to the latest FINAL tag on every run, so it was never
#     remembered across a candidate train. Re-running with a different bump
#     silently forked the version.
#   - `pre` was hand-written and unvalidated, so nothing noticed rc.2 skipping
#     to rc.6, or an alpha appearing in an rc train.
#   - `promote` with no version finalised the highest prerelease in the whole
#     repository rather than the train being worked on. v0.1.24 reached rc.18
#     and was orphaned that way when a v0.2.0 train started beside it; there is
#     still no v0.1.24 tag.
#
# Output contract:
#   stdout  the computed tag, and nothing else, so the caller can capture it
#   stderr  the state report and any notices
#   exit    non-zero on anything ambiguous or invalid
#
# Inputs (environment, matching how the workflow binds its dispatch inputs):
#   MODE                     release | prerelease | promote
#   BUMP                     patch | minor | major (only used to START a train)
#   PRE                      optional explicit prerelease suffix, e.g. rc.3
#   VERSION                  optional explicit final version for promote
#   ALLOW_CONCURRENT_TRAINS  "true" to permit a second live train
#
# Definition of a LIVE train, which everything below turns on: a core version
# vX.Y.Z that has prerelease tags, has no final tag of its own, AND is newer
# than the latest final tag. That last clause is what makes an abandoned train
# invisible. v0.1.24 still has twelve candidates and no final, but the project
# has since shipped v0.2.4, so it is stale rather than in flight and must never
# be offered as something to continue or promote.

set -euo pipefail

MODE="${MODE:?MODE must be set}"
BUMP="${BUMP:-patch}"
PRE="${PRE:-}"
VERSION="${VERSION:-}"
ALLOW_CONCURRENT_TRAINS="${ALLOW_CONCURRENT_TRAINS:-false}"

# Everything the human needs to see goes to stderr so stdout stays parseable.
note() { echo "$*" >&2; }
fail() { echo "::error::$*" >&2; exit 1; }

# version_gt compares two vX.Y.Z strings. sort -V understands the leading v and
# orders 1.10 above 1.9, which a lexical comparison does not.
version_gt() {
  [[ "$1" != "$2" ]] || return 1

  [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1)" == "$1" ]]
}

# latest_final prints the highest final (non-prerelease) tag, or v0.0.0 when the
# repository has none yet.
latest_final() {
  local tag

  tag="$(git tag --list 'v[0-9]*' | grep -vE -- '-' | sort -V | tail -n1 || true)"

  echo "${tag:-v0.0.0}"
}

# train_cores prints every core version that has prerelease tags.
train_cores() {
  git tag --list 'v[0-9]*-*' | sed 's/-.*//' | sort -uV
}

# has_final reports whether a core has been released.
has_final() {
  git rev-parse -q --verify "refs/tags/$1" >/dev/null 2>&1
}

# live_trains prints cores that are still in flight: prereleases exist, no final
# was cut, and the core is newer than the latest final.
live_trains() {
  local final="$1" core

  while read -r core; do
    [[ -n "$core" ]] || continue

    if has_final "$core"; then
      continue
    fi

    if version_gt "$core" "$final"; then
      echo "$core"
    fi
  done < <(train_cores)
}

# stale_trains prints cores whose prereleases were abandoned. Reported so the
# leftover tags eventually get cleaned up rather than accumulating unexplained.
stale_trains() {
  local final="$1" core

  while read -r core; do
    [[ -n "$core" ]] || continue

    if has_final "$core"; then
      continue
    fi

    if ! version_gt "$core" "$final"; then
      echo "$core"
    fi
  done < <(train_cores)
}

bump_version() {
  local base="$1" level="$2" semver major minor patch

  semver="${base#v}"
  IFS='.' read -r major minor patch <<<"$semver"

  case "$level" in
    major) major=$((major + 1)); minor=0; patch=0 ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    patch) patch=$((patch + 1)) ;;
    *) fail "unexpected bump level: ${level}" ;;
  esac

  echo "v${major}.${minor}.${patch}"
}

# max_rc prints the highest rc number already cut for a core, or 0.
#
# The numbers are compared numerically on purpose. The v0.1.24 train ran to
# rc.18, and a lexical comparison reports rc.9 as its maximum, which would hand
# out rc.10 a second time.
max_rc() {
  local core="$1" max=0 n

  while read -r n; do
    [[ -n "$n" ]] || continue

    if (( n > max )); then
      max="$n"
    fi
  done < <(git tag --list "${core}-rc.*" | sed "s|^${core}-rc\.||" | grep -E '^[0-9]+$' || true)

  echo "$max"
}

FINAL="$(latest_final)"
mapfile -t LIVE < <(live_trains "$FINAL")
mapfile -t STALE < <(stale_trains "$FINAL")

note "Latest final: ${FINAL}"
note "Live trains:  $( ((${#LIVE[@]})) && echo "${LIVE[*]}" || echo "(none)" )"

if ((${#STALE[@]})); then
  note "Stale trains: ${STALE[*]} (superseded by ${FINAL}; their tags can be deleted)"
fi

case "$MODE" in
  release)
    [[ -z "$PRE" ]] || fail "pre is only valid with mode=prerelease"

    if ((${#LIVE[@]})); then
      note "::warning::cutting a final release while ${LIVE[*]} is still in flight; that train will be stranded"
    fi

    TAG="$(bump_version "$FINAL" "$BUMP")"
    ;;

  prerelease)
    if [[ "$ALLOW_CONCURRENT_TRAINS" == "true" ]]; then
      # Explicit intent wins: bump decides the target even if that means a
      # second train.
      CORE="$(bump_version "$FINAL" "$BUMP")"

      if ((${#LIVE[@]})) && [[ " ${LIVE[*]} " != *" ${CORE} "* ]]; then
        note "::warning::starting a SECOND live train ${CORE} alongside ${LIVE[*]}; promote will now require an explicit version"
      fi
    elif ((${#LIVE[@]} > 1)); then
      fail "multiple live trains (${LIVE[*]}); promote or delete one, or set allow_concurrent_trains to add another"
    elif ((${#LIVE[@]} == 1)); then
      # Continue the train in flight. bump is deliberately ignored rather than
      # validated: it is a required input with a default, so there is no way to
      # tell "the user chose patch" from "the user left the default", and
      # erroring would fire on the most ordinary invocation there is.
      CORE="${LIVE[0]}"
      note "Continuing live train ${CORE} (bump=${BUMP} ignored; it only applies when starting a train)"
    else
      CORE="$(bump_version "$FINAL" "$BUMP")"
      note "Starting a new train at ${CORE} (bump=${BUMP} from ${FINAL})"
    fi

    if [[ -n "$PRE" ]]; then
      # rc is the only suffix in use; see RELEASING.md. alpha and beta were
      # previously used interchangeably with no defined meaning.
      [[ "$PRE" =~ ^rc\.[0-9]+$ ]] || fail "pre must look like rc.N (got '${PRE}'); rc is the only prerelease suffix"

      want="${PRE#rc.}"
      have="$(max_rc "$CORE")"

      if (( want <= have )); then
        fail "pre=${PRE} is not ahead of ${CORE}-rc.${have}; leave pre blank to take the next one automatically"
      fi
    else
      PRE="rc.$(( $(max_rc "$CORE") + 1 ))"
      note "Auto-selected ${PRE} for ${CORE}"
    fi

    TAG="${CORE}-${PRE}"
    ;;

  promote)
    [[ -z "$PRE" ]] || fail "pre is only valid with mode=prerelease"

    if [[ -n "$VERSION" ]]; then
      NORM="v${VERSION#v}"

      [[ "$NORM" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "version must be a final vX.Y.Z with no suffix, got: ${VERSION}"

      if [[ -z "$(git tag --list "${NORM}-*")" ]]; then
        fail "no prerelease train found for ${NORM}; use mode=release to cut it directly"
      fi

      TAG="$NORM"
    elif ((${#LIVE[@]} == 0)); then
      fail "no live prerelease train to promote; use mode=release to cut a final version directly"
    elif ((${#LIVE[@]} > 1)); then
      # The v0.1.24 orphan came from guessing here. It no longer guesses.
      fail "multiple live trains (${LIVE[*]}); pass version to say which one to promote"
    else
      TAG="${LIVE[0]}"
      note "Promoting the only live train: ${TAG}"
    fi
    ;;

  *)
    fail "unexpected mode: ${MODE}"
    ;;
esac

[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail "computed tag is not vX.Y.Z[-suffix]: ${TAG}"

if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1; then
  fail "tag ${TAG} already exists"
fi

note "Computed tag: ${TAG}"

echo "$TAG"
