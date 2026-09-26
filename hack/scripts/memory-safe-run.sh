#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Usage: bash hack/scripts/memory-safe-run.sh -- COMMAND [ARG...]
# Bounds the entire command subtree, including build tools, to 16 GiB and no swap.
# Requires Linux cgroup v2 at /sys/fs/cgroup and a working user systemd manager
# (the system manager when root), or an already bounded delegated cgroup.
# Limits are read back from the executing process's cgroup before COMMAND starts.
# No sudo, global swap changes, ulimit fallback, or unbounded retry is performed.
# Working directory, environment, argument boundaries, and exit status survive a
# systemd scope. Wrap make itself to include any prerequisite compilation:
#   bash hack/scripts/memory-safe-run.sh -- make racer-dataplane-test
set -euo pipefail

readonly memory_cap=17179869184
readonly cgroup_root=/sys/fs/cgroup

fail() {
    printf 'memory-safe-run: %s; command not started\n' "$*" >&2
    exit 125
}

usage() {
    printf 'Usage: bash %s -- COMMAND [ARG...]\n' "$0"
    printf 'Requires cgroup v2 memory.max <= %s and memory.swap.max = 0.\n' "$memory_cap"
}

# This mode is also safe to invoke directly: it verifies real kernel limits,
# never trusts an environment flag, and never tries to create another scope.
verify_only=false
if [[ ${1-} == --verify-exec ]]; then
    verify_only=true
    shift
fi
if [[ ${1-} == --help ]]; then
    usage
    exit 0
fi
[[ ${1-} == -- ]] || { usage >&2; exit 125; }
shift
(( $# > 0 )) || fail 'missing command'

[[ $(stat -f -c %T "$cgroup_root" 2>/dev/null) == cgroup2fs ]] ||
    fail 'cgroup v2 is unavailable at /sys/fs/cgroup'

# Reject non-root mounts: /proc/self/cgroup paths would otherwise be ambiguous
# when mapped to this mount. Unusual namespace layouts fail closed.
mount_ok=false
while IFS=' ' read -r _ _ _ root mountpoint rest; do
    if [[ $root == / && $mountpoint == "$cgroup_root" && $rest == *' - cgroup2 '* ]]; then
        mount_ok=true
        break
    fi
done < /proc/self/mountinfo
"$mount_ok" || fail 'cannot map cgroup membership to the cgroup v2 mount'

group=
while IFS=: read -r hierarchy controllers path; do
    if [[ $hierarchy == 0 && -z $controllers ]]; then
        group=$path
        break
    fi
done < /proc/self/cgroup
[[ $group == /* && $group != */../* && $group != */.. ]] ||
    fail 'cannot resolve current cgroup v2 membership'
readonly cgroup="$cgroup_root$group"

memory=unavailable
swap=unavailable
read -r memory 2>/dev/null < "$cgroup/memory.max" || memory=unavailable
read -r swap 2>/dev/null < "$cgroup/memory.swap.max" || swap=unavailable
if [[ $memory =~ ^[0-9]{1,11}$ && $swap == 0 ]] &&
    (( 10#$memory > 0 && 10#$memory <= memory_cap )); then
    printf 'memory-safe-run: cgroup=%s memory.max=%s memory.swap.max=%s\n' \
        "$cgroup" "$memory" "$swap" >&2
    exec "$@"
fi

"$verify_only" && fail "unsafe cgroup $cgroup (memory.max=$memory, memory.swap.max=$swap)"
command -v systemd-run >/dev/null 2>&1 ||
    fail 'systemd-run unavailable; use a delegated cgroup with verified memory and swap limits'
command -v systemctl >/dev/null 2>&1 || fail 'systemctl unavailable'
manager=(--user)
if (( EUID == 0 )); then
    manager=(--system)
fi
systemctl "${manager[@]}" show-environment >/dev/null 2>&1 ||
    fail 'systemd manager unavailable; use a delegated cgroup with verified memory and swap limits'

script=$(realpath -- "${BASH_SOURCE[0]}") || fail 'cannot resolve wrapper path'
readonly script
# Scope execution inherits cwd and environment, unlike a transient service.
# --expand-environment=no preserves literal dollar signs in arbitrary arguments.
# The gate runs inside the scope; accepted systemd properties alone are not proof
# of enforcement. A failed launch or command is never retried outside the scope.
exec systemd-run "${manager[@]}" --scope --quiet --collect \
    --expand-environment=no \
    --property="MemoryMax=$memory_cap" --property=MemorySwapMax=0 \
    -- "$BASH" "$script" --verify-exec -- "$@"
