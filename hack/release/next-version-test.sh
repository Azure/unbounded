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
fixture() {
  local dir
  dir="$(mktemp -d)"

  git -C "$dir" init -q
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name test
  git -C "$dir" commit -q --allow-empty -m base

  local tag
  for tag in "$@"; do
    git -C "$dir" tag "$tag"
  done

  echo "$dir"
}

# expect_tag <name> <expected> <tags...> -- <env assignments...>
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
    got=$([[ $rc -ne 0 ]] && echo ERROR || echo "$out")
  else
    got="$out"
  fi

  if [[ "$got" == "$expected" ]]; then
    printf 'PASS  %-46s %s\n' "$name" "$got"
    PASS=$((PASS + 1))
  else
    printf 'FAIL  %-46s got=%q want=%q\n' "$name" "$got" "$expected"
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
