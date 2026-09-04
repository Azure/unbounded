#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# net-ebpf-verify.sh -- cheap CI check for the BPF CO-RE pin artifacts.
# Runs without bpftool / curl / dpkg-deb; just sha256sum and text diff.
#
# Validates that:
#  1. bpf/vmlinux.h's leading provenance comment embeds bpf/btf-kernel-pin
#     verbatim (lines stripped of "# " comments and empty lines).
#  2. The committed bpf/vmlinux.h hashes to vmlinux_h_sha256 in
#     bpf/btf-kernel-pin-hashes.
#
# A mismatch in (1) means someone edited bpf/btf-kernel-pin without running
# `make net-ebpf-generate`. A mismatch in (2) means bpf/vmlinux.h was edited
# without regenerating.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
pin_file="$repo_root/bpf/btf-kernel-pin"
hashes_file="$repo_root/bpf/btf-kernel-pin-hashes"
vmlinux_h="$repo_root/bpf/vmlinux.h"

fail() {
	echo "net-ebpf-verify: $1" >&2
	echo >&2
	echo "To regenerate the BPF CO-RE artifacts, run:" >&2
	echo "  make net-ebpf-generate" >&2
	exit 1
}

for f in "$pin_file" "$hashes_file" "$vmlinux_h"; do
	[[ -r "$f" ]] || fail "missing $f"
done

# --- Check 1: provenance block in bpf/vmlinux.h matches bpf/btf-kernel-pin

# Extract data lines from bpf/btf-kernel-pin (strip # comments and empties).
expected_block="$(grep -Ev '^\s*(#|$)' "$pin_file")"

# Extract the "// foo=bar" lines from the leading comment block of vmlinux.h.
# The marker for the start of the block is "// Generated from the kernel pin"
# and the end is the first "//" line that doesn't contain '=' (i.e. the
# "// SHA256 hashes ..." line). We just match lines of the form "// key=value".
got_block="$(awk '
	/^[^/]/ { exit }
	/^\/\/ [a-z_][a-zA-Z0-9_]*=/ {
		sub(/^\/\/ /, "");
		print;
	}
' "$vmlinux_h")"

if [[ "$expected_block" != "$got_block" ]]; then
	{
		echo "bpf/btf-kernel-pin and bpf/vmlinux.h provenance are out of sync."
		echo
		echo "expected (from bpf/btf-kernel-pin):"
		printf '%s\n' "$expected_block" | sed 's/^/  /'
		echo
		echo "got (from bpf/vmlinux.h leading comment):"
		printf '%s\n' "$got_block" | sed 's/^/  /'
	} >&2
	fail "pin provenance mismatch"
fi

# --- Check 2: bpf/vmlinux.h sha256 matches the value in btf-kernel-pin-hashes

expected_sha=""
while IFS= read -r line; do
	case "$line" in
		\#*|"") continue ;;
		vmlinux_h_sha256=*) expected_sha="${line#vmlinux_h_sha256=}" ;;
	esac
done < "$hashes_file"

if [[ -z "$expected_sha" ]]; then
	fail "$hashes_file is missing 'vmlinux_h_sha256=' line"
fi

actual_sha="$(sha256sum "$vmlinux_h" | awk '{print $1}')"
if [[ "$expected_sha" != "$actual_sha" ]]; then
	{
		echo "bpf/vmlinux.h sha256 does not match bpf/btf-kernel-pin-hashes."
		echo "  expected: $expected_sha"
		echo "  actual:   $actual_sha"
	} >&2
	fail "vmlinux.h hash mismatch"
fi

echo "BPF CO-RE pin artifacts are consistent."
