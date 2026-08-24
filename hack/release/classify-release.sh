#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# classify-release: decide what happens to a release once it has been built.
#
# Called by .github/workflows/release-upgrade.yaml, which turns the answers into
# job conditions.
#
# There are two questions here and they are NOT the same question. An earlier
# design answered both with one version comparison and got the first one wrong.
#
#   from_main  Should this soak on unbounded-stable?
#
#              That cluster soaks main, and only main. A release cut from a
#              release-X.Y branch has its own soak story - eventually its own
#              cluster - and deploying it to stable would replace whatever main
#              last put there.
#
#              This is deliberately a question about PROVENANCE, not about
#              version ordering. Ordering cannot answer it: stable can be
#              running a candidate that is newer than the newest final, so
#              "is this version the highest" says deploy when the honest answer
#              is that this release has no business on that cluster at all.
#
#              Assumes release branches are never merged back into main. The
#              flow is one-way: fixes land on main and are cherry-picked down,
#              so a branch's commits stay off main and its tags stay
#              unreachable.
#
#   latest     Should this be marked "Latest" on the GitHub release?
#
#              This one IS about version ordering, because "Latest" is what
#              releases/latest/download resolves to, which is the install
#              command in README.md and every guide. Publishing v0.3.1 after
#              v0.5.0 must not repoint those at v0.3.1.
#
#              GitHub defaults make_latest to true on any newly published
#              release, so this must be passed explicitly on every publish.
#
# Contract:
#   $1      the tag being released, vX.Y.Z[-suffix]
#   SEMVER  optional override for the comparison command; defaults to running
#           hack/cmd/semver from the repository root. Word-split on purpose so
#           it can hold a command with arguments.
#
# Expects to run inside a checkout of the DEFAULT BRANCH with full history and
# tags, so HEAD is main. HEAD is used rather than origin/main because the
# checkout already puts us there and a remote-tracking ref may not exist.
#
# Output contract:
#   stdout  `from_main=` and `latest=` lines, in next-version.sh's key=value style
#   stderr  the reasoning, and errors
#   exit    0 classified, non-zero on a malformed or unknown tag

set -euo pipefail

note() { echo "$*" >&2; }
fail() {
  echo "::error::$*" >&2
  exit 1
}

TAG="${1:-}"
SEMVER="${SEMVER:-go run ./hack/cmd/semver}"

[[ -n "$TAG" ]] || fail "no tag given; usage: classify-release.sh vX.Y.Z[-suffix]"

# Same shape the resolver mints and release-upgrade already validates. Checked
# again here because this script is the thing deciding whether a cluster gets
# touched, and it should not depend on a caller two workflows away.
SEMVER_COMPONENT='(0|[1-9][0-9]{0,8})'
if [[ ! "$TAG" =~ ^v${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}\.${SEMVER_COMPONENT}(-[0-9A-Za-z.-]+)?$ ]]; then
  fail "not a release tag: ${TAG}"
fi

git rev-parse -q --verify "${TAG}^{commit}" >/dev/null 2>&1 ||
  fail "tag ${TAG} does not exist here; the checkout needs full history and tags"

# --- from_main -------------------------------------------------------------

if git merge-base --is-ancestor "${TAG}^{commit}" HEAD 2>/dev/null; then
  FROM_MAIN=true
  note "${TAG} is reachable from HEAD: cut from the default branch, so it soaks."
else
  FROM_MAIN=false
  note "${TAG} is NOT reachable from HEAD: cut from a release branch, so it does not soak."
fi

# --- latest ----------------------------------------------------------------

if [[ "$TAG" == *-* ]]; then
  # GitHub refuses to mark a prerelease as Latest, so asking for it is at best
  # ignored. Answer honestly rather than sending a flag that cannot apply.
  LATEST=false
  note "${TAG} is a prerelease; a prerelease can never be Latest."
else
  SERIES="$(sed -E 's/^v([0-9]+\.[0-9]+)\..*/\1/' <<<"$TAG")"

  # Everything on the trunk, plus everything in this tag's own line. That is
  # exactly the set of releases that could legitimately outrank it:
  #
  #   - Tags reachable from HEAD are main's releases. Scoping to them keeps a
  #     stray final cut on someone's feature branch from suppressing Latest
  #     forever, the same reason next-version.sh scopes discovery by
  #     reachability.
  #   - Tags in the same series are the branch's own, which reachability cannot
  #     see from main. Without them, republishing v0.3.1 while v0.3.2 exists
  #     would mark the older one Latest - flipping the marker backwards, which
  #     is the bug this whole check exists to prevent.
  #
  # shellcheck disable=SC2086 # SEMVER may carry arguments and must word-split
  SUPERSEDED="$(
    {
      git tag --merged HEAD --list 'v*'
      git tag --list "v${SERIES}.*"
    } | sort -u | $SEMVER is-maintenance "$TAG"
  )"

  case "$SUPERSEDED" in
    true) LATEST=false ;;
    false) LATEST=true ;;
    *) fail "unexpected verdict from semver: ${SUPERSEDED}" ;;
  esac
fi

echo "from_main=${FROM_MAIN}"
echo "latest=${LATEST}"
