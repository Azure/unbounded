#!/usr/bin/env bash
set -euo pipefail

interface=localdns
node_listener={{.NodeListenerIP}}
cluster_listener={{.ClusterListenerIP}}
comment='unbounded-localdns: skip conntrack'
nft_table=unbounded_localdns
backend_file=/etc/unbounded/kube/localdns-network-backend

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

cleanup_iptables() {
    if ! command -v iptables >/dev/null 2>&1; then
        return
    fi

    for chain in OUTPUT PREROUTING; do
        while rule_number=$(iptables -w -t raw -L "${chain}" --line-numbers -n 2>/dev/null | awk -v marker="${comment}" 'index($0, marker) {print $1; exit}') && [[ -n "${rule_number}" ]]; do
            iptables -w -t raw -D "${chain}" "${rule_number}"
        done
    done
}

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

reconcile_iptables() {
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
}

mkdir -p "$(dirname "${backend_file}")"
if command -v nft >/dev/null 2>&1 && reconcile_nftables; then
    cleanup_iptables
    printf 'nftables\n' >"${backend_file}.tmp"
else
    if command -v nft >/dev/null 2>&1; then
        nft delete table ip "${nft_table}" >/dev/null 2>&1 || true
    fi
    cleanup_iptables
    reconcile_iptables
    printf 'iptables\n' >"${backend_file}.tmp"
fi
mv "${backend_file}.tmp" "${backend_file}"
