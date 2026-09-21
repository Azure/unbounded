// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Conservative intersection of TCP listener destination sets, independent of
//! the host's IPV6_V6ONLY default. This is admission policy, not bind probing:
//! even a successful SO_REUSEPORT bind can divert traffic before activation.
use std::net::{IpAddr, SocketAddr};

pub(crate) fn overlaps(a: SocketAddr, b: SocketAddr) -> bool {
    if a.port() != b.port() {
        return false;
    }
    // An IPv6 wildcard may receive IPv4 too. Keep dual-stack service supported
    // rather than changing socket options (and silently dropping IPv4 traffic).
    if [a.ip(), b.ip()]
        .iter()
        .any(|ip| matches!(ip, IpAddr::V6(ip) if ip.is_unspecified()))
    {
        return true;
    }
    let canonical = |ip| match ip {
        IpAddr::V6(ip) => ip.to_ipv4_mapped().map_or(IpAddr::V6(ip), IpAddr::V4),
        ip => ip,
    };
    match (canonical(a.ip()), canonical(b.ip())) {
        (IpAddr::V4(a), IpAddr::V4(b)) => a == b || a.is_unspecified() || b.is_unspecified(),
        // Scope IDs and flow labels do not prove disjoint destinations. Different
        // spellings of the same IPv6 IP must not create competing listeners.
        (IpAddr::V6(a), IpAddr::V6(b)) => a == b,
        _ => false,
    }
}
