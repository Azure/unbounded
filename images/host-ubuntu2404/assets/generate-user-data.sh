#!/bin/sh
# Generates cloud-init user-data by injecting unbounded-metal-attest.py
# into the user-data template.
#
# Usage: generate-user-data.sh <attest-script> <template> > user-data
set -eu

ATTEST_SCRIPT="$1"
TEMPLATE="$2"

# Indent the attest script content by 4 spaces for YAML block scalar.
INDENTED=$(sed 's/^/    /' "$ATTEST_SCRIPT")

# Replace the @@ATTEST_SCRIPT@@ placeholder (which sits at 4-space indent
# under "content: |") with the properly indented script content.
awk -v script="$INDENTED" '{
    if ($0 ~ /@@ATTEST_SCRIPT@@/) {
        print script
    } else {
        print
    }
}' "$TEMPLATE"
