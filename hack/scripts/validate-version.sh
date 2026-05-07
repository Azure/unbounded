#!/usr/bin/env bash
# Validates that VERSION is a valid semantic version (with optional v-prefix
# and optional pre-release / build-metadata / -dirty suffix).
#
# Usage:
#   hack/scripts/validate-version.sh <version>
#
# Exits 0 on success, non-zero with a diagnostic message on failure.

set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <version>" >&2
    exit 2
fi

version="$1"

# Semver 2.0.0 regex with optional v-prefix and tolerant of git-describe
# suffixes like "-5-gabc1234" and "-dirty".
semver_re='^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'

if [[ ! "$version" =~ $semver_re ]]; then
    echo "error: '$version' is not a valid semantic version (expected vMAJOR.MINOR.PATCH[-pre][+build])" >&2
    exit 1
fi
