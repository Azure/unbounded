// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};
use std::net::SocketAddr;

use crate::model::*;
use crate::product::{Product, choose};
use crate::{Error, Result, proto};

/// Preserve surviving roles while the profile remains useful. Assignments are
/// process-local, not restart authority: a cold leader deterministically rebuilds
/// from sorted physical identities. No assignment is an owner-hash input.
pub(crate) fn assign(
    members: &[String],
    previous: Option<&ProductPlacement>,
) -> Result<(Product, Vec<u32>)> {
    let count = members.len() as u32;
    let chosen = choose(count).map_err(Error)?;
    let retained = previous.and_then(|old| {
        let product = Product::new(old.left_factor, old.right_factor).ok()?;
        let order = product.order();
        let old_valid = old.members.len() == old.roles.len()
            && old.members.windows(2).all(|w| w[0] < w[1])
            && old.roles.iter().all(|&r| r < order);
        let degree = product.neighbors(0).len() as u32;
        let chosen_bound = count.div_ceil(chosen.order()) * chosen.neighbors(0).len() as u32;
        (old_valid
            && product.supports_members(count)
            && count <= 2 * order
            && count.div_ceil(order) * degree <= 2 * chosen_bound.max(1))
        .then_some((product, old))
    });
    let product = retained.map_or(chosen, |(p, _)| p);
    let order = product.order() as usize;
    let mut bundles = vec![BTreeSet::new(); order];
    let mut roles = vec![u32::MAX; members.len()];
    if let Some((_, old)) = retained {
        for (i, id) in members.iter().enumerate() {
            if let Ok(j) = old.members.binary_search(id) {
                let role = old.roles[j];
                roles[i] = role;
                bundles[role as usize].insert(i);
            }
        }
    }
    let mut loads: BTreeSet<_> = bundles
        .iter()
        .enumerate()
        .map(|(r, b)| (b.len(), r))
        .collect();
    for (i, role) in roles.iter_mut().enumerate() {
        if *role == u32::MAX {
            let (load, r) = loads.pop_first().unwrap();
            bundles[r].insert(i);
            *role = r as u32;
            loads.insert((load + 1, r));
        }
    }
    // Fill vacancies and rebalance only the excess from overloaded bundles.
    while let (Some(&(low, dest)), Some(&(high, source))) = (loads.first(), loads.last()) {
        if high <= low + 1 {
            break;
        }
        loads.remove(&(low, dest));
        loads.remove(&(high, source));
        let i = bundles[source].pop_last().unwrap();
        bundles[dest].insert(i);
        roles[i] = dest as u32;
        loads.insert((low + 1, dest));
        loads.insert((high - 1, source));
    }
    Ok((product, roles))
}

pub(crate) fn validate(g: &Generation) -> Result<()> {
    let has_product = g
        .volumes
        .iter()
        .any(|v| v.routing_algorithm == PRODUCT_ROUTING_ALGORITHM);
    let Some(p) = &g.product else {
        if has_product
            && (g.nodes.values().any(|m| m.ip.is_some())
                || g.volumes.iter().any(|v| {
                    v.routing_algorithm == PRODUCT_ROUTING_ALGORITHM && !v.owners.is_empty()
                }))
        {
            return Err(Error("product owners require product placement".into()));
        }
        return Ok(());
    };
    let graph = Product::new(p.left_factor, p.right_factor).map_err(Error)?;
    let n = p.members.len();
    let active: BTreeSet<_> = g
        .nodes
        .values()
        .filter(|m| m.ip.is_some())
        .map(|m| m.id.clone())
        .collect();
    if !has_product
        || n == 0
        || n > 100_000
        || n != p.roles.len()
        || !p.members.windows(2).all(|w| w[0] < w[1])
        || p.members.iter().cloned().collect::<BTreeSet<_>>() != active
        || !graph.supports_members(n as u32)
        || p.candidate_width == 0
        || p.candidate_width > 8
        || p.candidate_width as usize > n
    {
        return Err(Error("invalid product membership or geometry".into()));
    }
    let mut loads = vec![0usize; graph.order() as usize];
    for &role in &p.roles {
        let Some(load) = loads.get_mut(role as usize) else {
            return Err(Error("invalid product role".into()));
        };
        *load += 1;
    }
    if loads.contains(&0) || loads.iter().max().unwrap() - loads.iter().min().unwrap() > 1 {
        return Err(Error(
            "product roles must be nonempty balanced bundles".into(),
        ));
    }
    let volumes: Vec<_> = g
        .volumes
        .iter()
        .filter(|v| v.routing_algorithm == PRODUCT_ROUTING_ALGORITHM)
        .collect();
    let slots = volumes[0].slots as usize;
    let width = p.candidate_width as usize;
    let expected_width = volumes
        .iter()
        .map(|v| v.max_candidate_attempts)
        .max()
        .unwrap()
        .min(n as u32);
    if p.candidate_width != expected_width || p.candidates.len() != slots * width {
        return Err(Error("incomplete physical candidate table".into()));
    }
    for row in p.candidates.chunks_exact(width) {
        for (i, &candidate) in row.iter().enumerate() {
            if candidate as usize >= n || row[..i].contains(&candidate) {
                return Err(Error("invalid or repeated physical candidate".into()));
            }
        }
    }
    for v in volumes {
        if v.slots as usize != slots
            || v.owners.len() != slots
            || v.owners
                .iter()
                .enumerate()
                .any(|(s, name)| g.nodes[name].id != p.members[p.candidates[s * width] as usize])
        {
            return Err(Error("physical primary disagrees with ownership".into()));
        }
    }
    Ok(())
}

fn direct_indexes(p: &ProductPlacement, index: usize) -> Vec<usize> {
    let graph = Product::new(p.left_factor, p.right_factor).expect("validated product");
    let neighbors = graph.neighbors(p.roles[index]);
    p.roles
        .iter()
        .enumerate()
        .filter_map(|(i, role)| neighbors.binary_search(role).is_ok().then_some(i))
        .collect()
}

pub(crate) fn peer_counts(p: &ProductPlacement) -> Vec<u64> {
    let graph = Product::new(p.left_factor, p.right_factor).expect("validated product");
    let mut loads = vec![0u64; graph.order() as usize];
    for &role in &p.roles {
        loads[role as usize] += 1;
    }
    (0..graph.order())
        .map(|role| {
            graph
                .neighbors(role)
                .iter()
                .map(|&r| loads[r as usize])
                .sum()
        })
        .collect()
}

pub(crate) fn budget(p: &ProductPlacement, v: &Volume, direct: u64) -> Result<(u64, u64, u64)> {
    let members = p.members.len() as u64;
    let width = v.max_candidate_attempts.min(members as u32) as u64;
    let candidates = u64::from(v.slots) * width;
    // Conservative protobuf sizes: IDs plus framing, five-byte uint32 values,
    // three endpoint/peer references, and the legacy primary local-slot list.
    let records = 2 * members + 4 * direct + u64::from(v.slots);
    let wire = 80 * members
        + 5 * candidates
        + 2048 * direct
        + 5 * u64::from(v.slots)
        + (v.cache_socket.len() + v.origin_socket.len() + v.id.len()) as u64
        + 1024;
    // Top-k entries are bounded separately by wire/heap admission, rather than
    // charged as legacy per-slot routing search work.
    Ok((4 * candidates + 8 * members, records, wire))
}

#[cfg(test)]
#[path = "../tests/placement/product.rs"]
mod tests;

pub(crate) fn snapshot(
    g: &Generation,
    p: &ProductPlacement,
    v: &Volume,
    index: usize,
    local_slots: Vec<u32>,
    by_id: &BTreeMap<String, String>,
) -> (proto::Volume, BTreeMap<String, proto::Peer>) {
    let peers: BTreeMap<_, _> = direct_indexes(p, index)
        .into_iter()
        .map(|i| {
            let id = &p.members[i];
            let remote = &g.nodes[&by_id[id]];
            (
                id.clone(),
                proto::Peer {
                    id: id.clone(),
                    fabric: remote.fabric.clone(),
                    pod_uid: remote.pod_uid.clone(),
                    http_address: SocketAddr::new(remote.ip.unwrap(), 9443).to_string(),
                },
            )
        })
        .collect();
    let width = v.max_candidate_attempts.min(p.members.len() as u32);
    let candidates = p
        .candidates
        .chunks_exact(p.candidate_width as usize)
        .flat_map(|row| row[..width as usize].iter().copied())
        .collect();
    (
        proto::Volume {
            id: v.id.clone(),
            cache_generation: v.cache_generation,
            peers: peers.keys().cloned().collect(),
            cache_socket: v.cache_socket.clone(),
            origin_socket: v.origin_socket.clone(),
            max_candidate_attempts: Some(v.max_candidate_attempts),
            member_catalog: Some(0),
            peer_endpoints: Some(proto::VolumePeerEndpoints {
                peers: peers
                    .values()
                    .map(|peer| proto::VolumePeerEndpoint {
                        peer: peer.id.clone(),
                        http_address: peer.http_address.clone(),
                    })
                    .collect(),
            }),
            topology: Some(proto::Topology {
                epoch: g.revision,
                slot_count: v.slots,
                local_slots,
                neighbors: Vec::new(),
                routing_algorithm: Some(PRODUCT_ROUTING_ALGORITHM),
                product: Some(proto::ProductTopology {
                    left_factor: p.left_factor,
                    right_factor: p.right_factor,
                    members: p.members.clone(),
                    roles: p.roles.clone(),
                    local_member: index as u32,
                    candidate_width: width,
                    candidates,
                }),
            }),
        },
        peers,
    )
}
