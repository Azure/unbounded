#!/bin/bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Guest-side fault injection for the migration-recovery suite. The paths are
# supplied by the harness; the fixture never replaces the host's restorecon or
# changes labels on the installation being tested.
set -euo pipefail

action=$1
root=$2
legacy=$3
probe=$4
drop_in=$5
unit=unbounded-agent-daemon.service

require_enforcing() {
    if [ "$(getenforce)" != Enforcing ]; then
        echo "migration recovery requires SELinux enforcing mode" >&2
        exit 1
    fi
}

case "$action" in
    arm)
        require_enforcing
        test -L "$root"
        test "$(readlink "$root")" = "$legacy"
        test ! -e "$probe"
        test ! -e "$drop_in"
        real_restorecon=$(command -v restorecon)
        command -v chcon >/dev/null

        install -d -m 0755 "$probe/bin" "$(dirname "$drop_in")"
        printf '#!/bin/bash\nset -eu\nprobe=%q\nroot=%q\nreal_restorecon=%q\n' \
            "$probe" "$root" "$real_restorecon" > "$probe/bin/restorecon"
        cat >> "$probe/bin/restorecon" <<'WRAPPER'
if [ "$#" -eq 2 ] && [ "$1" = -R ] && [ "$2" = "$root" ] && [ -f "$root/.moving" ]; then
    # The real root has been installed but restorecon has not touched it yet.
    # Block in the same service cgroup until the harness kills the entire unit.
    : > "$probe/reached"
    exec sleep infinity
fi
exec "$real_restorecon" "$@"
WRAPPER
        chmod 0755 "$probe/bin/restorecon"
        # Only the test hook is labeled, so SELinux permits the daemon to run
        # it just as it permits the already-running legacy executable.
        chcon --reference="$legacy/bin" "$probe" "$probe/bin"
        chcon --reference="$(readlink -f "$legacy/bin/unbounded-agent-current")" "$probe/bin/restorecon"
        # Treat the injected SIGKILL as expected, so OnFailure does not roll
        # back the slots; Restart=no keeps the killed daemon stopped.
        cat > "$drop_in" <<DROP_IN
[Service]
Restart=no
SuccessExitStatus=SIGKILL
Environment="PATH=$probe/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
DROP_IN
        systemctl daemon-reload
        ;;
    checkpoint)
        require_enforcing
        test -f "$probe/reached"
        test ! -L "$root"
        test -f "$root/.moving"
        test ! -e "$root.staging"
        test -x "$legacy/bin/unbounded-agent-current"
        systemctl show "$unit" --property=ExecStart --value | grep -F "$legacy/bin/unbounded-agent-current"
        ls -ldZ "$root" "$root/bin" "$root/bin/"*
        # A no-op probe must not pass this test: at least one copied daemon
        # binary must still need relabeling. -n inspects without repairing it.
        restorecon -nvR "$root" > "$probe/labels-before.txt"
        cat "$probe/labels-before.txt"
        if ! grep -F "$root/bin/unbounded-agent-" "$probe/labels-before.txt"; then
            echo "checkpoint did not expose copied daemon binaries needing relabeling" >&2
            exit 1
        fi
        ;;
    interrupt)
        test -f "$probe/reached"
        systemctl kill --signal=SIGKILL --kill-who=all "$unit"
        systemctl stop "$unit"
        test "$(systemctl show "$unit" --property=MainPID --value)" = 0
        test -f "$root/.moving"
        ;;
    disarm)
        # Also used on failure. Stop any blocked child before removing its
        # executable, and keep automatic restart/recovery disabled until then.
        if [ -f "$drop_in" ]; then
            pid=$(systemctl show "$unit" --property=MainPID --value)
            if [ "${pid:-0}" -gt 0 ]; then
                systemctl kill --signal=SIGKILL --kill-who=all "$unit"
            fi
            systemctl stop "$unit"
            rm -f "$drop_in"
        fi
        rm -rf "$probe"
        systemctl daemon-reload
        ;;
    resume)
        require_enforcing
        test ! -e "$probe"
        test ! -e "$drop_in"
        test -f "$root/.moving"
        systemctl reset-failed "$unit"
        systemctl start "$unit"
        ;;
    inspect|diagnose)
        # Observations only. A label mismatch must not prevent the harness from
        # exercising the real services, and failed services must still have
        # their process contexts, journal and AVC evidence collected.
        echo '=== SELinux mode ==='
        getenforce || true
        echo '=== File contexts and proposed label changes (read-only) ==='
        ls -ldZ "$root" "$root/bin" "$root/bin/"* || true
        restorecon -nvR "$root" || true
        echo '=== Systemd execution results ==='
        systemctl show "$unit" unbounded-agent-daemon-recovery.service \
            unbounded-agent-regenerate-config@kube1.service unbounded-agent-regenerate-config@kube2.service \
            systemd-nspawn@kube1.service systemd-nspawn@kube2.service \
            --property=Id,ActiveState,SubState,Result,MainPID,ExecMainCode,ExecMainStatus,ExecStart,ExecStartPre,ExecStartPost,SELinuxContext || true
        echo '=== Running process contexts ==='
        ps -e -o label,pid,comm || true
        echo '=== Daemon, recovery and nspawn journal ==='
        journalctl -b --no-pager -n 150 -u "$unit" -u unbounded-agent-daemon-recovery.service \
            -u 'unbounded-agent-regenerate-config@*' -u 'systemd-nspawn@*' || true
        echo '=== SELinux AVCs from this boot ==='
        ausearch -m AVC,USER_AVC -ts boot || true
        ;;
    verify)
        require_enforcing
        test ! -e "$probe"
        test ! -e "$drop_in"
        test ! -L "$root"
        test ! -e "$root/.moving"
        ls -ldZ "$root" "$root/bin" "$root/bin/"*
        labels=$(restorecon -nvR "$root")
        if [ -n "$labels" ]; then
            echo "resumed migration left labels that differ from SELinux policy:"
            echo "$labels"
            exit 1
        fi
        ;;
    *)
        echo "unknown migration recovery action: $action" >&2
        exit 1
        ;;
esac
