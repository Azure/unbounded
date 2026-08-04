#!/usr/bin/env bash
set -euo pipefail

interface=localdns
node_listener={{.NodeListenerIP}}
cluster_listener={{.ClusterListenerIP}}
comment='unbounded-localdns: skip conntrack'

if ip link show "${interface}" >/dev/null 2>&1; then
    if ! ip -d -o link show dev "${interface}" | grep -Eq '(^|[[:space:]])dummy([[:space:]]|$)'; then
        echo "existing ${interface} interface is not a dummy interface" >&2
        exit 1
    fi
else
    ip link add name "${interface}" type dummy
fi
ip link set up dev "${interface}"
ip address flush dev "${interface}"
ip address replace "${node_listener}/32" dev "${interface}"
ip address replace "${cluster_listener}/32" dev "${interface}"

for chain in OUTPUT PREROUTING; do
    while rule_number=$(iptables -w -t raw -L "${chain}" --line-numbers -n | awk -v marker="${comment}" 'index($0, marker) {print $1; exit}') && [[ -n "${rule_number}" ]]; do
        iptables -w -t raw -D "${chain}" "${rule_number}"
    done
done

for chain in OUTPUT PREROUTING; do
    for address in "${node_listener}" "${cluster_listener}"; do
        for protocol in tcp udp; do
            rule=(-m comment --comment "${comment}" -p "${protocol}" -d "${address}" --dport 53 -j NOTRACK)
            if ! iptables -w -t raw -C "${chain}" "${rule[@]}" >/dev/null 2>&1; then
                iptables -w -t raw -A "${chain}" "${rule[@]}"
            fi
        done
    done
done
