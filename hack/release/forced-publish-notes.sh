#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# forced-publish-notes: compose the release body for a forced publish.
#
# Called by the publish-forced job in .github/workflows/release-upgrade.yaml.
# It exists as a script, rather than inline in that job, because the bypass
# notice is the durable record of a release that skipped its soak: it is read
# long after the run log has expired, and it is the one artefact nobody
# exercises until an emergency. Two defects lived in it undetected precisely
# because it could not be tested - a skipped smoke matrix was described as a
# successful soak, and a re-run stacked a second notice onto the body.
#
# Usage:
#   forced-publish-notes.sh <body-file>
#
# The current release body is read from <body-file>; the new body is written to
# stdout. The caller then applies it and flips the draft in ONE `gh release
# edit`, so a failure cannot leave a published release with no record.
#
# Contract (provided by the calling workflow):
#   ACTOR            who dispatched the forced run
#   REASON           why, recorded verbatim on the release
#   DEPLOY_RESULT    needs.deploy.result
#   ORCA_RESULT      needs.deploy-orca.result
#   DISCOVER_RESULT  needs.smoke-discover.result
#   SMOKE_RESULT     needs.smoke-tests.result
#
# Exit codes:
#   0  a notice was appended; stdout is the new body
#   2  usage: a required input is missing
#   3  the body already carries a notice; stdout is the body unchanged

set -euo pipefail

BODY_FILE="${1:?usage: forced-publish-notes.sh <body-file>}"

: "${ACTOR:?ACTOR must be set}"
: "${DEPLOY_RESULT:?DEPLOY_RESULT must be set}"
: "${ORCA_RESULT:?ORCA_RESULT must be set}"
: "${DISCOVER_RESULT:?DISCOVER_RESULT must be set}"
: "${SMOKE_RESULT:?SMOKE_RESULT must be set}"

REASON="${REASON:-}"

# An HTML comment: invisible on the rendered release page, reliable to match on.
MARKER="<!-- unbounded:forced-publish -->"

if [[ -z "${REASON// /}" ]]; then
  echo "::error::force_publish requires a non-empty reason; it is recorded on the release" >&2
  exit 2
fi

if [[ ! -f "$BODY_FILE" ]]; then
  echo "::error::no release body at ${BODY_FILE}" >&2
  exit 2
fi

cat "$BODY_FILE"

# Guarantee the body ends with a newline so the notice is separated the same way
# every time, whether or not the generated changelog ended cleanly.
if [[ ! -s "$BODY_FILE" || -n "$(tail -c 1 "$BODY_FILE")" ]]; then
  echo
fi

if grep -qF "$MARKER" "$BODY_FILE"; then
  exit 3
fi

# A forced dispatch lands on this path even when every gate passed, so that
# forcing is always recorded as forcing. The headline therefore has to describe
# what actually happened rather than assume the worst.
#
# `skipped` is NOT a pass. Since smoke discovery fails rather than emitting an
# empty matrix, a skipped smoke job means something upstream failed and the
# tests never ran at all - which is exactly when this notice matters most.
if [[ "$DEPLOY_RESULT" == "success" && "$ORCA_RESULT" == "success" \
      && "$DISCOVER_RESULT" == "success" && "$SMOKE_RESULT" == "success" ]]; then
  headline="Published through the forced path, though the soak did pass."
  caveat="The gates this run bypasses all reported success; the bypass is recorded because it was requested."
else
  headline="Published without a successful soak."
  caveat="Release artifact signatures and the release BOM were verified; the deployment soak on unbounded-stable was not completed."
fi

printf '\n\n---\n\n%s\n> **%s**\n> Forced by @%s. Deploy: %s, Orca: %s, smoke discovery: %s, smoke: %s.\n> Reason: %s\n>\n> %s\n' \
  "$MARKER" "$headline" "$ACTOR" \
  "$DEPLOY_RESULT" "$ORCA_RESULT" "$DISCOVER_RESULT" "$SMOKE_RESULT" \
  "$REASON" "$caveat"
