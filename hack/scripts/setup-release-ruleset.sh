#!/usr/bin/env bash
# Create (or re-sync) the branch ruleset that protects release-X.Y maintenance
# branches, mirroring whatever protects the default branch.
#
# Release branches carry cherry-picked fixes to shipped versions, including
# security fixes, so they need the same protections main has. This script does
# not invent those protections: it reads the default branch's ruleset and reuses
# its pull-request, status-check and code-scanning rules verbatim, so the two
# cannot drift. Re-run with --update after main's checks change.
#
# Four deliberate differences from the source, each explained where it is
# applied below: the ref pattern, do_not_enforce_on_create, no merge queue, and
# no bypass actors.
#
# Usage:
#   hack/scripts/setup-release-ruleset.sh [--repo Azure/unbounded] \
#     [--from RULESET_ID] [--update] [--dry-run] [--yes]

set -euo pipefail
IFS=$'\n\t'

# Never drop the caller into a pager; this is meant to be readable in one go.
export GH_PAGER=cat

REPO="Azure/unbounded"
FROM_ID=""
DO_UPDATE="false"
DRY_RUN="false"
ASSUME_YES="false"

# The ref pattern the new ruleset protects, and the branch shape the rest of the
# release tooling validates against.
RELEASE_REF_PATTERN="refs/heads/release-*"
RULESET_NAME="release branches"

usage() {
    cat <<'EOF'
Create or re-sync the ruleset protecting release-X.Y branches.

Protections are copied from the default branch's ruleset rather than written
here, so the required status checks and reviewers cannot drift from main's.

Optional:
  --repo OWNER/NAME     Target repository. Default: Azure/unbounded
  --from RULESET_ID     Source ruleset to mirror. Default: the active
                        default-branch ruleset that requires status checks
  --update              Re-sync an existing release ruleset instead of refusing
  --dry-run             Print what would be sent and change nothing
  --yes                 Skip the confirmation prompt
  --help                Show this help

Exit codes:
  0 success
  1 usage, validation, or an unmet prerequisite
  2 gh not authenticated
  3 GitHub API call failed
EOF
}

die() {
    echo "error: $*" >&2
    exit 1
}

api_die() {
    echo "error: $*" >&2
    exit 3
}

require_value() {
    if [[ -z "${2:-}" || "${2:0:2}" == "--" ]]; then
        die "flag $1 requires a value"
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --repo)    require_value "$1" "${2:-}"; REPO="$2"; shift 2 ;;
        --from)    require_value "$1" "${2:-}"; FROM_ID="$2"; shift 2 ;;
        --update)  DO_UPDATE="true"; shift ;;
        --dry-run) DRY_RUN="true"; shift ;;
        --yes)     ASSUME_YES="true"; shift ;;
        --help|-h) usage; exit 0 ;;
        *)         usage >&2; die "unknown argument: $1" ;;
    esac
done

command -v gh >/dev/null 2>&1 || die "'gh' CLI not found on PATH; install from https://cli.github.com/"
command -v jq >/dev/null 2>&1 || die "'jq' not found on PATH"

if ! gh auth status >/dev/null 2>&1; then
    echo "error: 'gh' is not authenticated; run 'gh auth login' first" >&2
    exit 2
fi

# --- Prerequisite: CI must already run on release branches -------------------
#
# This is the guard worth having. Every required status check comes from
# ci.yaml, whose pull_request branch filter is matched against the BASE branch
# of the pull request. A ruleset requiring those checks on a branch where
# ci.yaml never runs blocks every pull request to it permanently, with nothing
# an operator can do except remove the ruleset again.
#
# The check below reads the default branch's ci.yaml, because that is where the
# trigger has to land first. A release branch carries its own copy of the
# workflow, so a branch cut from a tag that predates that change needs release-*
# added to its ci.yaml as the branch's first commit.

echo "Checking that CI runs on release branches..."

CI_WORKFLOW="$(gh api "repos/${REPO}/contents/.github/workflows/ci.yaml" \
    -H "Accept: application/vnd.github.raw" 2>/dev/null)" ||
    die "could not read .github/workflows/ci.yaml from ${REPO}"

if ! grep -q 'release-\*' <<<"$CI_WORKFLOW"; then
    cat >&2 <<EOF
error: ci.yaml on the default branch does not trigger on release-* branches.

  Creating this ruleset now would require status checks that can never run on a
  release branch, blocking every pull request to it permanently.

  Land the change that adds release-* to ci.yaml's push and pull_request branch
  filters first, then re-run this script.
EOF
    exit 1
fi

echo "  ci.yaml triggers on release-* branches."

# --- Scan the repository's rulesets ------------------------------------------
#
# The list endpoint returns neither conditions nor rules, so every ruleset has
# to be fetched to classify it. One pass answers both questions that matter:
# which ruleset to mirror, and whether a release ruleset already exists.
#
# More than one ruleset can target the default branch - this repository has a
# disabled Copilot-review one alongside the real protections - so the
# discriminator for the source is requiring status checks, which is the thing
# being mirrored.

echo "Scanning rulesets in ${REPO}..."

RULESET_LIST="$(gh api "repos/${REPO}/rulesets" --jq '.[].id')" ||
    api_die "could not list rulesets for ${REPO} (admin access is required)"

RULESET_IDS=()
if [[ -n "$RULESET_LIST" ]]; then
    mapfile -t RULESET_IDS <<<"$RULESET_LIST"
fi

# Candidate sources to mirror, and the release ruleset if one already exists.
MATCHES=()
EXISTING_ID=""

for id in "${RULESET_IDS[@]}"; do
    body="$(gh api "repos/${REPO}/rulesets/${id}" 2>/dev/null)" || continue

    if jq -e '
        .enforcement == "active"
        and (.conditions.ref_name.include // [] | index("~DEFAULT_BRANCH"))
        and ([.rules[].type] | index("required_status_checks"))
    ' >/dev/null <<<"$body"; then
        MATCHES+=("$id")
    fi

    if [[ -z "$EXISTING_ID" ]] && jq -e --arg pattern "$RELEASE_REF_PATTERN" \
        '(.conditions.ref_name.include // []) | index($pattern)' >/dev/null <<<"$body"; then
        EXISTING_ID="$id"
    fi
done

if [[ -z "$FROM_ID" ]]; then
    case ${#MATCHES[@]} in
        0) die "no active default-branch ruleset with required status checks found in ${REPO}; pass --from RULESET_ID" ;;
        1) FROM_ID="${MATCHES[0]}" ;;
        *)
            # Joined by hand: "${MATCHES[*]}" would separate on IFS, which is a
            # newline here and would break the message across lines.
            MATCH_LIST="$(printf '%s, ' "${MATCHES[@]}")"
            die "several default-branch rulesets require status checks (${MATCH_LIST%, }); pass --from RULESET_ID to choose"
            ;;
    esac
fi

SOURCE="$(gh api "repos/${REPO}/rulesets/${FROM_ID}" 2>/dev/null)" ||
    api_die "could not read ruleset ${FROM_ID} from ${REPO}"

SOURCE_NAME="$(jq -r '.name' <<<"$SOURCE")"
echo "  mirroring ruleset ${FROM_ID} (\"${SOURCE_NAME}\")."

# A `creation` rule on the source would be inherited and would stop
# create-release-branch.yaml from creating a branch at all. Refuse rather than
# produce a ruleset that quietly breaks that workflow.
if jq -e '[.rules[].type] | index("creation")' >/dev/null <<<"$SOURCE"; then
    die "source ruleset ${FROM_ID} contains a 'creation' rule; inheriting it would prevent release branches from being created at all"
fi

# --- Refuse to clobber an existing release ruleset ---------------------------

if [[ -n "$EXISTING_ID" && "$DO_UPDATE" != "true" ]]; then
    die "ruleset ${EXISTING_ID} already protects ${RELEASE_REF_PATTERN}; re-run with --update to re-sync it from ${FROM_ID}"
fi

if [[ -z "$EXISTING_ID" && "$DO_UPDATE" == "true" ]]; then
    die "--update given but no ruleset protects ${RELEASE_REF_PATTERN} yet; re-run without it to create one"
fi

# --- Build the payload -------------------------------------------------------
#
# Four deliberate differences from the source:
#
#   ref pattern     release-* instead of the default branch. The point.
#
#   do_not_enforce_on_create: true
#                   The source has false, which is inert there because a default
#                   branch is never created. Here it is not: with false, creating
#                   release-0.3 at v0.3.0's commit would be gated on those checks
#                   being recorded against that exact SHA. They usually are, but
#                   relying on it makes branch creation fail in a way that is
#                   confusing and hard to fix from under a ruleset. Creation is
#                   safe ungated because the commit is an already-released tag
#                   that passed CI on the default branch.
#
#   no merge queue  A release branch takes occasional cherry-picks. A queue adds
#                   latency and machinery for no benefit; easy to add later.
#
#   no bypass actors
#                   Set explicitly rather than inherited, in both directions.
#                   The source has none today, so nothing is lost now; if one is
#                   added there later, mirroring it onto release branches should
#                   be a decision rather than a side effect. Equally, --update
#                   sends a full PUT, so a bypass added to the release ruleset by
#                   hand is removed on the next re-sync. Grant bypasses here, in
#                   the payload below, or not at all.

PAYLOAD="$(jq \
    --arg name "$RULESET_NAME" \
    --arg pattern "$RELEASE_REF_PATTERN" \
    '{
        name: $name,
        target: "branch",
        enforcement: "active",
        bypass_actors: [],
        conditions: { ref_name: { include: [$pattern], exclude: [] } },
        rules: [
            .rules[]
            | select(.type != "merge_queue")
            | if .type == "required_status_checks"
              then .parameters.do_not_enforce_on_create = true
              else . end
        ]
     }' <<<"$SOURCE")"

CHECKS="$(jq -r '
    [.rules[] | select(.type == "required_status_checks")
     | .parameters.required_status_checks[].context] | join(", ")
' <<<"$PAYLOAD")"

RULE_TYPES="$(jq -r '[.rules[].type] | join(", ")' <<<"$PAYLOAD")"

cat <<EOF

About to $( [[ -n "$EXISTING_ID" ]] && echo "UPDATE ruleset ${EXISTING_ID}" || echo "CREATE a ruleset" ) in ${REPO}:

  Name:            ${RULESET_NAME}
  Protects:        ${RELEASE_REF_PATTERN}
  Mirroring:       ruleset ${FROM_ID} ("${SOURCE_NAME}")
  Rules:           ${RULE_TYPES}
  Required checks: ${CHECKS}

  Differences from the source, all deliberate:
    - protects release-* rather than the default branch
    - do_not_enforce_on_create: true, so a branch can be created
    - no merge queue
    - no bypass actors

EOF

if [[ "$DRY_RUN" == "true" ]]; then
    echo "Dry run. Payload that would be sent:"
    echo
    jq . <<<"$PAYLOAD"
    echo
    echo "Nothing was changed."
    exit 0
fi

if [[ "$ASSUME_YES" != "true" ]]; then
    read -r -p "Proceed? [y/N] " reply
    [[ "$reply" == "y" || "$reply" == "Y" ]] || die "aborted"
fi

if [[ -n "$EXISTING_ID" ]]; then
    jq . <<<"$PAYLOAD" |
        gh api --method PUT "repos/${REPO}/rulesets/${EXISTING_ID}" --input - >/dev/null ||
        api_die "failed to update ruleset ${EXISTING_ID}"
    RESULT_ID="$EXISTING_ID"
    echo "Updated ruleset ${RESULT_ID}."
else
    RESULT_ID="$(jq . <<<"$PAYLOAD" |
        gh api --method POST "repos/${REPO}/rulesets" --input - --jq '.id')" ||
        api_die "failed to create the ruleset"
    echo "Created ruleset ${RESULT_ID}."
fi

# --- Verify ------------------------------------------------------------------

echo
echo "Verifying:"
gh api "repos/${REPO}/rulesets/${RESULT_ID}" --jq '
    "  id:          \(.id)",
    "  name:        \(.name)",
    "  enforcement: \(.enforcement)",
    "  protects:    \(.conditions.ref_name.include | join(", "))",
    "  rules:       \([.rules[].type] | join(", "))"
' || api_die "created ruleset ${RESULT_ID} but could not read it back"

cat <<EOF

Done. Release branches are now protected.

Next: create one with

  gh workflow run create-release-branch.yaml --repo ${REPO} -f series=X.Y

The first branch created is also the real test of the creation settings above.
Do not rehearse with a throwaway branch: deletion protection is one of the rules
just applied, so a test branch could not be removed without admin.
EOF

# Strictness inherited from the source that lands differently on a release
# branch, where every change arrives as a cherry-pick. Read back from the
# payload rather than hard-coded, so this can only describe rules that are
# actually there.
STRICTNESS="$(jq -r '
    [ (.rules[] | select(.type == "required_status_checks")
        | select(.parameters.strict_required_status_checks_policy == true)
        | "  - a branch must be up to date with the base before it can merge"),
      (.rules[] | select(.type == "pull_request")
        | select(.parameters.require_last_push_approval == true)
        | "  - the most recent push must be approved by someone other than whoever pushed it"),
      (.rules[] | select(.type == "pull_request")
        | select(.parameters.require_extra_approval_for_unattributed_changes == true)
        | "  - unattributed changes need an additional approval")
    ] | join("\n")
' <<<"$PAYLOAD")"

if [[ -n "$STRICTNESS" ]]; then
    cat <<EOF

Inherited from "${SOURCE_NAME}", and worth knowing before the first cherry-pick:

${STRICTNESS}

A cherry-pick normally records a committer different from the original author,
which counts as unattributed, so an urgent fix can need more approvals than
expected. Plan for that rather than discovering it mid-incident.
EOF
fi
