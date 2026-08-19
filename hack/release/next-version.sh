#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# next-version: work out which tag a release-prepare run should mint, and which
# commit to mint it at.
#
# Called by .github/workflows/release-prepare.yaml, which owns pushing the tag.
# This script only decides what to tag and where, which is the part that used to
# be inline in the workflow where the only way to test a change was to push a
# tag and see what happened. Three real problems came out of that:
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
#   stdout  `key=value` lines, ready to append to $GITHUB_OUTPUT:
#             tag=vX.Y.Z[-rc.N]
#             base=<commit the tag should be created at>
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
# A LIVE train, which everything below turns on: a core vX.Y.Z with prerelease
# tags, no final tag of its own, AND newer than the latest final. That last
# clause makes an abandoned train invisible - v0.1.24 has twelve candidates and
# no final, but v0.2.4 has since shipped, so it is stale rather than in flight.
#
# Why `base` exists: promote finalises a candidate that was already built,
# deployed and smoke-tested, so tagging HEAD would ship a DIFFERENT tree under a
# version whose only claim to being trustworthy is that soak. promote resolves
# the candidate's commit; release and prerelease resolve HEAD as always.

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

# What a version looks like, in one place. Every pattern below is built from
# SEMVER_COMPONENT, because three of them used to differ: discovery bounded the
# digits, the promote input did not, and the final check on the computed tag did
# not either - so a bump could mint a ten-digit version that passed on the way
# out and was invisible to discovery on the way back in.
#
# Nine digits: bash arithmetic is signed 64-bit and wraps in silence
# (9999999999999999999 + 1 is negative), and below a billion nothing can.
#
# No leading zeros: v01.2.3 is not a version, and `sort -V` does not order it
# where a reader would expect.
SEMVER_COMPONENT='(0|[1-9][0-9]{0,8})'
RC_NUMBER='[1-9][0-9]{0,8}'
SEMVER_TAG="^v${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}\$"
SEMVER_RC_TAG="^v${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}-rc\.${RC_NUMBER}\$"
SEMVER_ANY_TAG="^v${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}(-rc\.${RC_NUMBER})?\$"

# reachable_tags prints the tags that are ancestors of HEAD, which is the branch
# this run is releasing from.
#
# Tags anywhere else in the repository must not influence the numbering: a
# `v9.0.0` cut on someone's feature branch would otherwise become the latest
# final and make the next release from main v9.0.1. The workflow checks out the
# default branch, so "reachable from HEAD" is "released from this line".
#
# The cost is that a tag whose commit later leaves the branch's history stops
# being seen. That fails safe - the resolver would recompute a version whose tag
# already exists, and refuse it - and is the correct reading anyway: a tag on a
# commit that is no longer on the branch is not part of its history.
reachable_tags() {
  git tag --merged HEAD --list "$1" 2>/dev/null || true
}

# reachable_rc_tags prints the canonical candidates for one core. Release train
# discovery, numbering and promotion must agree on this definition: a stray
# alpha, malformed rc or bare suffix is repository metadata, not a live train.
reachable_rc_tags() {
  local core="$1"

  reachable_tags "${core}-rc.*" | grep -E -- "^${core}-rc\.${RC_NUMBER}\$" || true
}

# latest_final prints the highest final (non-prerelease) tag, or v0.0.0 when the
# repository has none yet.
latest_final() {
  local tag

  tag="$(reachable_tags 'v[0-9]*' | grep -E -- "$SEMVER_TAG" | sort -V | tail -n1 || true)"

  echo "${tag:-v0.0.0}"
}

# train_cores prints every core version that has canonical rc tags.
train_cores() {
  reachable_tags 'v[0-9]*-rc.*' | grep -E -- "$SEMVER_RC_TAG" | sed 's/-rc\.[0-9]*$//' | sort -uV || true
}

# has_final reports whether a core has been released. Deliberately global, not
# reachability-scoped: the question is whether the name is already taken, and it
# is taken wherever the tag exists.
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

  # Said here rather than left to the pattern check at the end, which would
  # report "not vX.Y.Z" for a version that is plainly vX.Y.Z and merely too big
  # for the arithmetic that produced it.
  if (( ${#major} > 9 || ${#minor} > 9 || ${#patch} > 9 )); then
    fail "bumping ${base} by ${level} overflows a version component; nine digits is the limit"
  fi

  echo "v${major}.${minor}.${patch}"
}

# max_rc prints the highest rc number already cut for a core, or 0. Compared
# numerically on purpose: v0.1.24 ran to rc.18, and a lexical maximum reports
# rc.9, which would hand out rc.10 a second time.
#
# 10# forces base ten. Bash reads a leading zero as octal, so a stray rc.08 tag
# makes `(( n > max ))` an arithmetic error - and because that sits in an `if`
# condition, which set -e exempts, the comparison silently evaluates false and
# the tag is skipped rather than the run failing.
max_rc() {
  local core="$1" max=0 n

  while read -r n; do
    [[ -n "$n" ]] || continue

    if (( 10#$n > 10#$max )); then
      max="$n"
    fi
  done < <(reachable_rc_tags "$core" | sed "s|^${core}-rc\.||")

  echo "$((10#$max))"
}

# candidate_commit prints the commit of the highest rc tag for a core: the tree
# that was actually built, deployed and smoke-tested. Reuses max_rc rather than
# git's version sort, because rc.18 must beat rc.9.
candidate_commit() {
  local core="$1" highest

  highest="$(max_rc "$core")"

  (( highest > 0 )) || fail "no rc tag found for ${core} (tags: $(git tag --list "${core}-*" | tr '\n' ' ')); nothing to promote, use mode=release to cut it directly"

  git rev-list -n1 "${core}-rc.${highest}" 2>/dev/null \
    || fail "could not resolve the commit of ${core}-rc.${highest}"
}

FINAL="$(latest_final)"
mapfile -t LIVE < <(live_trains "$FINAL")
mapfile -t STALE < <(stale_trains "$FINAL")

# Every mode but promote cuts from wherever the workflow checked out, which it
# pins to the default branch.
BASE="$(git rev-parse HEAD)"

note "Latest final: ${FINAL}"
note "Live trains:  $( ((${#LIVE[@]})) && echo "${LIVE[*]}" || echo "(none)" )"

if ((${#STALE[@]})); then
  note "Stale trains: ${STALE[*]} (superseded by ${FINAL}; their tags can be deleted)"
fi

case "$MODE" in
  release)
    [[ -z "$PRE" ]] || fail "pre is only valid with mode=prerelease"

    TAG="$(bump_version "$FINAL" "$BUMP")"

    # Cutting the version a live train is heading for is not a fork, it is that
    # train being finalised the long way round, so only warn when they differ.
    if ((${#LIVE[@]})) && [[ " ${LIVE[*]} " != *" ${TAG} "* ]]; then
      note "::warning::cutting ${TAG} while ${LIVE[*]} is still in flight; that train will be stranded"
    elif ((${#LIVE[@]})); then
      note "::warning::${TAG} is the version its candidates were building toward, but mode=release cuts it from HEAD; use mode=promote to ship the tree that was soaked"
    fi
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

    # has_final is repository-wide on purpose, and that has a consequence here:
    # a core whose final exists somewhere off this branch is excluded from LIVE,
    # so nothing stopped a new train being started on it. Every run then
    # reported "Starting a new train" and minted another rc, and promote could
    # never finish any of them, because the tag it would create is taken.
    if has_final "$CORE"; then
      fail "${CORE} already exists as a tag, though not on this branch; a train for it could never be promoted, so pick another bump or delete that tag"
    fi

    if [[ -n "$PRE" ]]; then
      # rc is the only suffix in use; see RELEASING.md. alpha and beta were
      # previously used interchangeably with no defined meaning.
      #
      # Leading zeros are rejected rather than normalised. rc.08 is an octal
      # literal to bash, so the "is it ahead" test below became an arithmetic
      # error - silently false inside an `if`, which set -e exempts - and the
      # guard did not fire. The tag it then minted poisoned max_rc the same way.
      [[ "$PRE" =~ ^rc\.[1-9][0-9]{0,8}$ ]] || fail "pre must look like rc.N with no leading zeros and at most nine digits (got '${PRE}'); rc is the only prerelease suffix"

      want="${PRE#rc.}"
      have="$(max_rc "$CORE")"

      if (( 10#$want <= 10#$have )); then
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

      [[ "$NORM" =~ $SEMVER_TAG ]] || fail "version must be a final vX.Y.Z with no suffix, no leading zeros and at most nine digits per component, got: ${VERSION}"

      if [[ -z "$(reachable_rc_tags "$NORM")" ]]; then
        fail "no prerelease train found for ${NORM}; use mode=release to cut it directly"
      fi

      # An explicit version must not be a way back to a train the resolver has
      # just reported as stale. Promoting v0.1.24 today would mint a final
      # release below v0.2.4 out of a candidate abandoned months ago.
      if ! version_gt "$NORM" "$FINAL"; then
        fail "${NORM} is older than the latest final ${FINAL}; its candidates were abandoned, delete them rather than promoting them"
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

    # Ship the tree that was soaked. The explicit `|| exit` matters: fail's exit
    # only leaves the command substitution's subshell.
    BASE="$(candidate_commit "$TAG")" || exit 1
    ;;

  *)
    fail "unexpected mode: ${MODE}"
    ;;
esac

[[ "$TAG" =~ $SEMVER_ANY_TAG ]] || fail "computed tag is not vX.Y.Z[-suffix]: ${TAG}"

if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1; then
  fail "tag ${TAG} already exists"
fi

# A candidate cut from an unmerged branch, or orphaned by a force-push, would
# ship a tree nobody reviewed on the default branch.
if ! git merge-base --is-ancestor "$BASE" HEAD 2>/dev/null; then
  fail "${BASE} is not an ancestor of HEAD; refusing to tag a commit that is not on the branch being released from"
fi

if [[ "$BASE" != "$(git rev-parse HEAD)" ]]; then
  note "Base commit: ${BASE} ($(git log -1 --format=%s "$BASE"))"
  note "Excluding $(git rev-list --count "${BASE}..HEAD") commit(s) merged since that candidate was cut"
else
  note "Base commit: ${BASE} (HEAD)"
fi

note "Computed tag: ${TAG}"

echo "tag=${TAG}"
echo "base=${BASE}"
