#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tests for next-version.sh.
#
# Each case builds a throwaway git repository containing only tags, runs the
# resolver against it, and checks the computed tag or the error. Tags are the
# only input the resolver has, so a synthetic tag set is a complete fixture.
#
# This exists because the logic it covers mints version tags. Before it was
# extracted from the workflow the only way to test a change was to push a real
# tag to the real repository and see what happened.
#
# Usage: hack/release/next-version-test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESOLVER="${SCRIPT_DIR}/next-version.sh"

PASS=0
FAIL=0

# fixture creates a repository whose tags are exactly the arguments.
#
# A bare `<tag>` is placed on the current commit. `<tag>@new` creates a fresh
# commit first, so a fixture can express a train whose candidates are behind
# HEAD, which is the shape promote has to get right. `<tag>@off` places the tag
# on a commit that is NOT an ancestor of HEAD, as a tag cut on someone else's
# branch would be.
fixture() {
  local dir
  dir="$(mktemp -d)"

  git -C "$dir" init -q
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name test
  git -C "$dir" commit -q --allow-empty -m base

  local branch
  branch="$(git -C "$dir" rev-parse --abbrev-ref HEAD)"

  local tag
  for tag in "$@"; do
    case "$tag" in
      *@new)
        tag="${tag%@new}"
        git -C "$dir" commit -q --allow-empty -m "work before ${tag}"
        ;;
      *@off)
        tag="${tag%@off}"
        git -C "$dir" checkout -q -b "side-${tag}"
        git -C "$dir" commit -q --allow-empty -m "off-branch work for ${tag}"
        git -C "$dir" tag "$tag"
        git -C "$dir" checkout -q "$branch"

        continue
        ;;
    esac

    git -C "$dir" tag "$tag"
  done

  echo "$dir"
}

# field extracts one `key=value` line from the resolver's stdout.
field() {
  sed -n "s/^$1=//p" <<<"$2"
}

# expect <name> <mode> <expected-tag|ERROR> <tags...> -- <env assignments...>
expect() {
  local name="$1" mode="$2" expected="$3"
  shift 3

  local -a tags=() env=()
  local seen_sep=0 arg
  for arg in "$@"; do
    if [[ "$arg" == "--" ]]; then seen_sep=1; continue; fi
    if (( seen_sep )); then env+=("$arg"); else tags+=("$arg"); fi
  done

  local dir out rc=0
  dir="$(fixture ${tags[@]+"${tags[@]}"})"
  out="$(cd "$dir" && env MODE="$mode" ${env[@]+"${env[@]}"} "$RESOLVER" 2>/dev/null)" || rc=$?
  rm -rf "$dir"

  local got
  if [[ "$expected" == "ERROR" ]]; then
    got=$([[ $rc -ne 0 ]] && echo ERROR || field tag "$out")
  else
    got="$(field tag "$out")"
  fi

  if [[ "$got" == "$expected" ]]; then
    printf 'PASS  %-46s %s\n' "$name" "$got"
    PASS=$((PASS + 1))
  else
    printf 'FAIL  %-46s got=%q want=%q\n' "$name" "$got" "$expected"
    FAIL=$((FAIL + 1))
  fi
}

# expect_base <name> <mode> <expected-ref> <tags...> -- <env assignments...>
#
# expected-ref is HEAD, or the name of a tag whose commit the base must equal.
# This is what says the version being minted points at the tree that was soaked
# rather than at whatever has landed since.
expect_base() {
  local name="$1" mode="$2" expected="$3"
  shift 3

  local -a tags=() env=()
  local seen_sep=0 arg
  for arg in "$@"; do
    if [[ "$arg" == "--" ]]; then seen_sep=1; continue; fi
    if (( seen_sep )); then env+=("$arg"); else tags+=("$arg"); fi
  done

  local dir out rc=0
  dir="$(fixture ${tags[@]+"${tags[@]}"})"
  out="$(cd "$dir" && env MODE="$mode" ${env[@]+"${env[@]}"} "$RESOLVER" 2>/dev/null)" || rc=$?

  local got want
  got="$(field base "$out")"
  want="$(git -C "$dir" rev-parse "${expected}^{commit}" 2>/dev/null)"
  rm -rf "$dir"

  if (( rc != 0 )); then
    printf 'FAIL  %-46s resolver exited %d\n' "$name" "$rc"
    FAIL=$((FAIL + 1))

    return
  fi

  if [[ -n "$got" && "$got" == "$want" ]]; then
    printf 'PASS  %-46s base=%s (%s)\n' "$name" "${got:0:8}" "$expected"
    PASS=$((PASS + 1))
  else
    printf 'FAIL  %-46s base=%q want=%q (%s)\n' "$name" "$got" "$want" "$expected"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== bootstrap ==="
expect "no tags, release" release "v0.0.1" -- BUMP=patch
expect "no tags, prerelease" prerelease "v0.0.1-rc.1" -- BUMP=patch

echo
echo "=== starting a train ==="
expect "final only, prerelease patch" prerelease "v0.2.5-rc.1" v0.2.4 -- BUMP=patch
expect "final only, prerelease minor" prerelease "v0.3.0-rc.1" v0.2.4 -- BUMP=minor
expect "final only, prerelease major" prerelease "v1.0.0-rc.1" v0.2.4 -- BUMP=major
expect "final only, release patch" release "v0.2.5" v0.2.4 -- BUMP=patch

echo
echo "=== continuing a train ==="
expect "live train auto-increments" prerelease "v0.2.5-rc.2" \
  v0.2.4 v0.2.5-rc.1 -- BUMP=patch
expect "live train ignores conflicting bump" prerelease "v0.2.5-rc.2" \
  v0.2.4 v0.2.5-rc.1 -- BUMP=minor
# rc.18 must beat rc.9: a lexical max would hand out rc.10 a second time.
expect "rc numbering is numeric, not lexical" prerelease "v0.2.5-rc.19" \
  v0.2.4 v0.2.5-rc.7 v0.2.5-rc.8 v0.2.5-rc.9 v0.2.5-rc.10 v0.2.5-rc.18 -- BUMP=patch

echo
echo "=== stale trains are invisible ==="
# The real v0.1.24 shape: abandoned at rc.18, superseded by v0.2.4.
expect "stale train ignored when starting" prerelease "v0.2.5-rc.1" \
  v0.1.24-rc.17 v0.1.24-rc.18 v0.2.0 v0.2.4 -- BUMP=patch
expect "stale train not promotable" promote "ERROR" \
  v0.1.24-rc.17 v0.1.24-rc.18 v0.2.0 v0.2.4 --
expect "stale train ignored, release" release "v0.2.5" \
  v0.1.24-rc.18 v0.2.4 -- BUMP=patch

echo
echo "=== concurrent trains ==="
expect "second train refused by default" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.1 v0.3.0-rc.1 -- BUMP=patch
expect "second train allowed with opt-in" prerelease "v0.3.0-rc.1" \
  v0.2.4 v0.2.5-rc.1 -- BUMP=minor ALLOW_CONCURRENT_TRAINS=true
expect "opt-in continues matching train" prerelease "v0.2.5-rc.2" \
  v0.2.4 v0.2.5-rc.1 -- BUMP=patch ALLOW_CONCURRENT_TRAINS=true

echo
echo "=== promote ==="
expect "promote the only live train" promote "v0.2.5" \
  v0.2.4 v0.2.5-rc.1 v0.2.5-rc.2 --
expect "promote refuses to guess" promote "ERROR" \
  v0.2.4 v0.2.5-rc.1 v0.3.0-rc.1 --
expect "promote with explicit version" promote "v0.3.0" \
  v0.2.4 v0.2.5-rc.1 v0.3.0-rc.1 -- VERSION=v0.3.0
expect "promote accepts bare version" promote "v0.3.0" \
  v0.2.4 v0.3.0-rc.1 -- VERSION=0.3.0
expect "promote with no train at all" promote "ERROR" v0.2.4 --
expect "promote rejects a suffixed version" promote "ERROR" \
  v0.2.4 v0.2.5-rc.1 -- VERSION=v0.2.5-rc.1
expect "promote rejects a version with no train" promote "ERROR" \
  v0.2.4 -- VERSION=v0.9.9
# The abandoned v0.1.24 train is not a back door: naming it explicitly must not
# mint a final release below the latest final out of months-old candidates.
expect "promote rejects a stale train by name" promote "ERROR" \
  v0.1.24-rc.17 v0.1.24-rc.18 v0.2.4 -- VERSION=v0.1.24

echo
echo "=== which commit gets tagged ==="
# promote finalises a candidate that has already been built, deployed and
# smoke-tested. Tagging HEAD would ship every commit merged since, under a
# version whose only claim to being trustworthy is that soak.
expect_base "promote tags the last candidate" promote "v0.2.5-rc.2" \
  v0.2.4 v0.2.5-rc.1@new v0.2.5-rc.2@new --
expect_base "promote ignores commits merged since" promote "v0.2.5-rc.1" \
  v0.2.4 v0.2.5-rc.1@new v9.9.9-marker@new -- VERSION=v0.2.5
# rc.18 must beat rc.9 here too: a lexical maximum would tag the wrong tree,
# which is far harder to notice than a wrong version number.
expect_base "promote picks rc.18 over rc.9" promote "v0.2.5-rc.18" \
  v0.2.4 v0.2.5-rc.9@new v0.2.5-rc.18@new --
expect_base "prerelease tags HEAD" prerelease "HEAD" \
  v0.2.4 v0.2.5-rc.1@new -- BUMP=patch
expect_base "release tags HEAD" release "HEAD" \
  v0.2.4 -- BUMP=patch
# A train with prerelease tags but no rc has no candidate to point at, so
# there is nothing to promote rather than a HEAD to fall back on.
expect "promote refuses a train with no rc" promote "ERROR" \
  v0.2.4 v0.2.5-beta.1 --

echo
echo "=== suffix policy ==="
expect "explicit rc accepted" prerelease "v0.2.5-rc.4" \
  v0.2.4 v0.2.5-rc.1 -- BUMP=patch PRE=rc.4
expect "beta rejected" prerelease "ERROR" \
  v0.2.4 -- BUMP=patch PRE=beta.1
expect "alpha rejected" prerelease "ERROR" \
  v0.2.4 -- BUMP=patch PRE=alpha.0
expect "rc must move forward" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.5 -- BUMP=patch PRE=rc.2
expect "rc equal to current rejected" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.5 -- BUMP=patch PRE=rc.5

echo
echo "=== malformed input ==="
# rc.08 is an octal literal to bash. The "is it ahead" comparison used to be an
# arithmetic error inside an `if`, which set -e exempts, so the guard silently
# did not fire and rc.08 was accepted behind rc.5.
expect "leading zero rejected" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.5 -- BUMP=patch PRE=rc.08
expect "leading zero rejected even when ahead" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.1 -- BUMP=patch PRE=rc.09
# An rc.08 tag already in the repository must not poison the maximum: base-ten
# arithmetic keeps rc.9 the highest, so the next one is rc.10.
expect "existing leading-zero tag does not poison max_rc" prerelease "v0.2.5-rc.10" \
  v0.2.4 v0.2.5-rc.08 v0.2.5-rc.9 -- BUMP=patch
# A stray two-part tag used to be selected as the latest final and bumped to
# v1.2.1, which passes every later check because it looks well formed.
expect "two-part tag ignored" release "v0.2.5" \
  v0.2.4 v1.2 -- BUMP=patch
expect "date-shaped tag ignored" release "v0.2.5" \
  v0.2.4 v20260710 -- BUMP=patch
expect "four-part tag ignored" release "v0.2.5" \
  v0.2.4 v1.2.3.4 -- BUMP=patch
# A malformed prerelease tag must not invent a train.
expect "malformed prerelease tag ignored" prerelease "v0.2.5-rc.1" \
  v0.2.4 v1.2-rc.1 -- BUMP=patch

echo
echo "=== tags off the branch being released ==="
# A v9.0.0 cut on someone's feature branch used to become the latest final, so
# the next release from main was v9.0.1.
expect "off-branch final ignored" release "v0.2.5" \
  v0.2.4 v9.0.0@off -- BUMP=patch
expect "off-branch prerelease starts no train" prerelease "v0.2.5-rc.1" \
  v0.2.4 v9.0.0-rc.1@off -- BUMP=patch
expect "off-branch train not promotable" promote "ERROR" \
  v0.2.4 v9.0.0-rc.1@off --
# An rc cut on a branch that was never merged must not be handed out again
# either, since the tag name is taken wherever it lives.
expect "off-branch rc name still refused" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.1@off -- BUMP=patch PRE=rc.1

expect "off-branch final blocks a new train" prerelease "ERROR" \
  v0.2.4 v0.2.5@off -- BUMP=patch
expect "off-branch final blocks a second train" prerelease "ERROR" \
  v0.2.4 v0.2.5-rc.1 v0.3.0@off -- BUMP=minor ALLOW_CONCURRENT_TRAINS=true

echo
echo "=== absurd numbers ==="
expect "twenty-digit component ignored" release "v0.2.5" \
  v0.2.4 v99999999999999999999.0.0 -- BUMP=patch
expect "ten-digit rc rejected" prerelease "ERROR" \
  v0.2.4 -- BUMP=patch PRE=rc.9999999999
# The bump itself overflows rather than minting a ten-digit version that
# discovery would then ignore.
expect "bump that overflows refused" release "ERROR" \
  v999999999.0.0 -- BUMP=major
expect "leading zeros rejected in a version input" promote "ERROR" \
  v0.2.4 v0.2.5-rc.1 -- VERSION=v01.2.3
expect "unbounded version input rejected" promote "ERROR" \
  v0.2.4 v0.2.5-rc.1 -- VERSION=v99999999999.0.0

echo
echo "=== misuse ==="
expect "pre rejected with release" release "ERROR" v0.2.4 -- BUMP=patch PRE=rc.1
expect "pre rejected with promote" promote "ERROR" \
  v0.2.4 v0.2.5-rc.1 -- PRE=rc.1
# Promoting a train whose final already shipped. This is the shape the old
# "highest prerelease in the repository" heuristic would land on today.
expect "existing tag refused" promote "ERROR" \
  v0.2.4 v0.2.4-rc.1 -- VERSION=v0.2.4
expect "unknown mode" nonsense "ERROR" v0.2.4 --
expect "unknown bump" release "ERROR" v0.2.4 -- BUMP=sideways

echo
echo "passed=${PASS} failed=${FAIL}"
exit $(( FAIL > 0 ))
