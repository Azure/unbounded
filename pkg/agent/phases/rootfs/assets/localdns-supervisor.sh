#!/usr/bin/env bash
set -euo pipefail

corefile=/etc/unbounded/localdns/Corefile
node_listener=$(sed -n 's/^NODE_LISTENER=//p' /etc/unbounded/localdns/environment)
cluster_listener=$(sed -n 's/^CLUSTER_LISTENER=//p' /etc/unbounded/localdns/environment)
coredns_pid=

shutdown() {
    if [[ -n "${coredns_pid}" ]] && kill -0 "${coredns_pid}" 2>/dev/null; then
        kill -INT "${coredns_pid}" 2>/dev/null || true
        wait "${coredns_pid}" || true
    fi
}
trap shutdown EXIT INT TERM

/usr/local/bin/coredns -conf "${corefile}" &
coredns_pid=$!

dns_health() {
    local address=$1
    timeout 3 bash -c '
        exec 3<>/dev/tcp/$1/53
        printf "\\x00\\x2d\\x12\\x34\\x01\\x00\\x00\\x01\\x00\\x00\\x00\\x00\\x00\\x00\\x0chealth-check\\x08localdns\\x05local\\x00\\x00\\x01\\x00\\x01" >&3
        read -r -a response < <(dd bs=1 count=14 <&3 2>/dev/null | od -An -tu1)
        ((${#response[@]} == 14))
        (( (response[5] & 15) == 0 ))
    ' _ "${address}"
}

ready() {
    curl --silent --fail --noproxy '*' --connect-timeout 2 --max-time 3 \
        "http://${node_listener}:8181/ready" >/dev/null &&
    curl --silent --fail --noproxy '*' --connect-timeout 2 --max-time 3 \
        "http://${cluster_listener}:8181/ready" >/dev/null
}

healthy() {
    ready && dns_health "${node_listener}" && dns_health "${cluster_listener}"
}

for _ in $(seq 1 60); do
    if healthy; then
        systemd-notify --ready
        break
    fi
    if ! kill -0 "${coredns_pid}" 2>/dev/null; then
        wait "${coredns_pid}"
        exit $?
    fi
    sleep 1
done

if ! healthy; then
    echo "LocalDNS did not become ready and answer health queries" >&2
    exit 1
fi

watchdog_usec=${WATCHDOG_USEC:-60000000}
interval=$((watchdog_usec / 5 / 1000000))
((interval > 0)) || interval=1
failures=0
window_start=0

while kill -0 "${coredns_pid}" 2>/dev/null; do
    if healthy; then
        systemd-notify WATCHDOG=1
    else
        now=$(date +%s)
        if ((window_start == 0 || now - window_start > 600)); then
            window_start=${now}
            failures=1
        else
            failures=$((failures + 1))
        fi
        if ((failures >= 10)); then
            echo "LocalDNS failed 10 health checks within 10 minutes" >&2
            systemd-notify WATCHDOG=trigger || true
            exit 1
        fi
    fi
    sleep "${interval}"
done

wait "${coredns_pid}"
