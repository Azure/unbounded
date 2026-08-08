#!/usr/bin/env bash
set -euo pipefail

interface=localdns
node_listener={{.NodeListenerIP}}
cluster_listener={{.ClusterListenerIP}}
comment='unbounded-localdns: skip conntrack'
nft_table=unbounded_localdns

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

reconcile_nftables() {
    local table_exists=false
    if nft list table ip "${nft_table}" >/dev/null 2>&1; then
        table_exists=true
    fi

    {
        if [[ "${table_exists}" == true ]]; then
            printf 'delete table ip %s\n' "${nft_table}"
        fi
        printf 'add table ip %s\n' "${nft_table}"
        printf 'add chain ip %s output { type filter hook output priority raw; policy accept; }\n' "${nft_table}"
        printf 'add chain ip %s prerouting { type filter hook prerouting priority raw; policy accept; }\n' "${nft_table}"

        for chain in output prerouting; do
            for address in "${node_listener}" "${cluster_listener}"; do
                for protocol in tcp udp; do
                    printf 'add rule ip %s %s ip daddr %s %s dport 53 notrack comment "%s"\n' \
                        "${nft_table}" "${chain}" "${address}" "${protocol}" "${comment}"
                done
            done
        done
    } | nft -f -
}

reconcile_nftables
