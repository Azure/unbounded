// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};
use std::net::SocketAddr;
use std::sync::Arc;

use crate::model::*;
use crate::{Error, Result, proto};

pub fn degree(slots: u32) -> u32 {
    let mut d = 1u32;
    while u64::from(d).pow(3) < u64::from(slots) {
        d += 1;
    }
    d
}

/// Retention-first balanced placement, including Go-compatible pair phase and
/// bounded diversity repair. Input list order never affects the result.
pub fn place(slots: u32, names: &[String], previous: &[String]) -> Result<Vec<String>> {
    let p = slots as usize;
    let mut names = names.to_vec();
    names.sort();
    let n = names.len();
    if slots == 0
        || slots > SLOT_COUNT
        || n == 0
        || n > p
        || n > 100_000
        || names.iter().any(String::is_empty)
        || names.windows(2).any(|w| w[0] == w[1])
    {
        return Err(Error(
            "invalid placement capacity or duplicate participant".into(),
        ));
    }
    let indices: BTreeMap<_, _> = names
        .iter()
        .enumerate()
        .map(|(i, name)| (name, i))
        .collect();
    let prior: Vec<usize> = previous
        .iter()
        .take(p)
        .map(|s| indices.get(s).copied().unwrap_or(n))
        .collect();
    if n == 2 {
        let matches = |slot: usize, owner: usize| i64::from(prior.get(slot) == Some(&owner));
        let mut score: i64 = (0..p).map(|i| matches(i, i % 2)).sum();
        let mut best = score;
        let mut seam = 0;
        if p.is_multiple_of(2) {
            let other: i64 = (0..p).map(|i| matches(i, 1 - i % 2)).sum();
            if other > best {
                seam = 1;
            }
        } else {
            let mut k = 0;
            for _ in 1..p {
                let j = (k + 1) % p;
                score += matches(k, 1) + matches(j, 0) - matches(k, 0) - matches(j, 1);
                k = (k + 2) % p;
                if score > best {
                    best = score;
                    seam = k;
                }
            }
        }
        return Ok((0..p)
            .map(|i| names[(i + p - seam) % p % 2].clone())
            .collect());
    }
    let quota: Vec<usize> = (0..n).map(|i| p / n + usize::from(i < p % n)).collect();
    let mut owners = vec![n; p];
    let mut counts = vec![0usize; n];
    for (slot, owner) in prior.into_iter().enumerate() {
        if owner < n {
            owners[slot] = owner;
            counts[owner] += 1;
        }
    }
    let mut deficits: Vec<usize> = (0..n).filter(|&i| counts[i] < quota[i]).collect();
    let mut i = 0;
    for pass in 0..2 {
        for slot in 0..p {
            let old = owners[slot];
            if old < n && counts[old] <= quota[old] {
                continue;
            }
            while counts[deficits[i]] == quota[deficits[i]] {
                deficits.swap_remove(i);
                i %= deficits.len();
            }
            let mut owner = deficits[i];
            if pass == 0 && !safe(&owners, slot, owner) {
                let found = (1..deficits.len().min(8))
                    .map(|offset| (i + offset) % deficits.len())
                    .find(|&j| {
                        counts[deficits[j]] < quota[deficits[j]] && safe(&owners, slot, deficits[j])
                    });
                let Some(j) = found else {
                    continue;
                };
                i = j;
                owner = deficits[i];
            }
            if old < n {
                counts[old] -= 1;
            }
            owners[slot] = owner;
            counts[owner] += 1;
            i = (i + 1) % deficits.len();
        }
    }
    if n > 2 && !diversify(&mut owners) {
        for (slot, owner) in owners.iter_mut().enumerate() {
            *owner = slot % n;
        }
        if owners[0] == owners[p - 1] {
            owners.swap(p - 1, p - 2);
        }
    }
    Ok(owners.into_iter().map(|i| names[i].clone()).collect())
}

fn safe(owners: &[usize], slot: usize, owner: usize) -> bool {
    let p = owners.len();
    owners[(slot + p - 1) % p] != owner && owners[(slot + 1) % p] != owner
}

fn diversify(owners: &mut [usize]) -> bool {
    let p = owners.len();
    let mut donor = 0;
    let mut budget = 16 * p;
    for slot in 0..p {
        if owners[slot] != owners[(slot + p - 1) % p] {
            continue;
        }
        let mut found = false;
        for tried in 0..2 * p {
            if budget == 0 {
                break;
            }
            budget -= 1;
            let x = if tried < p { slot } else { (slot + p - 1) % p };
            let y = donor;
            donor = (donor + 1) % p;
            if owners[x] == owners[y] {
                continue;
            }
            owners.swap(x, y);
            if safe(owners, x, owners[x]) && safe(owners, y, owners[y]) {
                found = true;
                break;
            }
            owners.swap(x, y);
        }
        if !found {
            return false;
        }
    }
    true
}

/// Compile desired content at the previous revision. Publication assigns the
/// next revision only if content changed. Errors leave the caller's state intact.
pub fn compile(input: &Inventory, previous: Option<&Generation>) -> Result<Generation> {
    if input.universe.is_empty() {
        return Err(Error("universe must not be empty".into()));
    }
    if let Some(old) = previous {
        if old.universe != input.universe {
            return Err(Error("previous universe mismatch".into()));
        }
        Topology::new(old)?;
    }
    let mut g = previous
        .cloned()
        .unwrap_or_else(|| Generation::empty(&input.universe));
    let pod_keys: BTreeMap<_, _> = input
        .nodes
        .iter()
        .flat_map(|n| &n.pods)
        .filter(|p| !p.uid.is_empty())
        .map(|p| (p.uid.as_str(), p))
        .collect();
    for member in g.nodes.values_mut() {
        member.ip = None;
        if (member.pod_namespace.is_empty() || member.pod_name.is_empty())
            && let Some(pod) = pod_keys.get(member.pod_uid.as_str())
        {
            member.pod_namespace = pod.namespace.clone();
            member.pod_name = pod.name.clone();
        }
    }
    let pod_identities: BTreeMap<_, _> = g
        .nodes
        .values()
        .filter(|m| !m.pod_uid.is_empty())
        .map(|m| (m.pod_uid.clone(), m.id.clone()))
        .collect();
    let mut seen = BTreeSet::new();
    for node in &input.nodes {
        if !node.eligible || node.universe != input.universe {
            continue;
        }
        if node.name.is_empty()
            || node.name.starts_with("deleted/")
            || node.uid.is_empty()
            || !seen.insert(&node.name)
        {
            return Err(Error("empty or duplicate node identity".into()));
        }
        if node.fabric.len() > 256 || !node.fabric.bytes().all(|b| (0x21..=0x7e).contains(&b)) {
            return Err(Error(format!("node {} has invalid fabric", node.name)));
        }
        let id = identity("node", &node.uid);
        if g.nodes.get(&node.name).is_some_and(|old| old.id != id) {
            let old = g.nodes.remove(&node.name).unwrap();
            g.nodes.insert(format!("deleted/{}", old.id), old);
        }
        let member = g.nodes.entry(node.name.clone()).or_default();
        member.id = id;
        member.fabric = node.fabric.clone();
        let mut pods: Vec<_> = node
            .pods
            .iter()
            .filter(|p| {
                node.ready
                    && p.available
                    && !p.uid.is_empty()
                    && available_ip(&p.ip).is_some()
                    && pod_identities.get(&p.uid).is_none_or(|id| id == &member.id)
            })
            .collect();
        pods.sort_by(|a, b| {
            (!a.ready, a.created_at, &a.name, &a.uid).cmp(&(
                !b.ready,
                b.created_at,
                &b.name,
                &b.uid,
            ))
        });
        if let Some(pod) = pods.first() {
            member.ip = available_ip(&pod.ip);
            member.pod_uid = pod.uid.clone();
            member.pod_namespace = pod.namespace.clone();
            member.pod_name = pod.name.clone();
        }
    }
    let active: Vec<_> = g
        .nodes
        .iter()
        .filter(|(_, n)| n.ip.is_some())
        .map(|(name, _)| name.clone())
        .collect();
    if active.len() > 100_000 {
        return Err(Error("universe exceeds 100000 participants".into()));
    }
    let old_volumes = std::mem::take(&mut g.volumes);
    g.withdrawn.extend(old_volumes.iter().map(|v| v.id.clone()));
    let mut caches: Vec<_> = input.caches.iter().collect();
    caches.sort_by_key(|c| &c.name);
    let mut ids = BTreeSet::new();
    let mut names = BTreeSet::new();
    for cache in caches {
        if cache.uid.is_empty()
            || cache.cache_generation < 0
            || !(1..=8).contains(&cache.max_candidate_attempts)
            || !ids.insert(&cache.uid)
            || !names.insert(&cache.name)
        {
            return Err(Error(format!(
                "cache {} has invalid identity or configuration",
                cache.name
            )));
        }
        let (cache_socket, origin_socket) = cache_sockets(&input.socket_root, &cache.name)?;
        let prior = old_volumes
            .iter()
            .find(|v| v.id == cache.uid)
            .map(|v| v.owners.as_slice())
            .unwrap_or_default();
        let owners = if active.is_empty() {
            Vec::new()
        } else {
            place(SLOT_COUNT, &active, prior)?
        };
        g.withdrawn.remove(&cache.uid);
        g.slot_history.insert(cache.uid.clone(), SLOT_COUNT);
        g.volumes.push(Volume {
            id: cache.uid.clone(),
            name: cache.name.clone(),
            resource_generation: cache.resource_generation,
            cache_socket,
            origin_socket,
            slots: SLOT_COUNT,
            cache_generation: cache.cache_generation as u64,
            routing_algorithm: 2,
            max_candidate_attempts: cache.max_candidate_attempts,
            owners,
        });
    }
    Topology::new(&g)?;
    Ok(g)
}

/// An ephemeral O(total slots + members) index over immutable committed content.
pub struct Topology {
    generation: Arc<Generation>,
    by_id: BTreeMap<String, String>,
    local: Vec<BTreeMap<String, Vec<u32>>>,
    members: proto::MemberCatalog,
}

impl Topology {
    /// Validate persisted content and profile-1 admission before publishing it.
    pub fn new(g: &Generation) -> Result<Self> {
        Self::from_generation(Arc::new(g.clone()))
    }

    pub fn generation(&self) -> &Arc<Generation> {
        &self.generation
    }

    pub fn from_generation(g: Arc<Generation>) -> Result<Self> {
        if g.format != GENERATION_FORMAT || g.universe.is_empty() || g.volumes.len() > 64 {
            return Err(Error(
                "invalid generation format, universe, or volume count".into(),
            ));
        }
        let mut by_id = BTreeMap::new();
        for (name, member) in &g.nodes {
            if member.id.len() != 64
                || !member
                    .id
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                || by_id.insert(member.id.clone(), name.clone()).is_some()
                || member
                    .ip
                    .is_some_and(|ip| available_ip(&ip.to_string()).is_none())
                || (member.ip.is_some() && member.pod_uid.is_empty())
                || member.pod_uid.len() > 253
                || member.fabric.len() > 1024
            {
                return Err(Error("invalid or duplicate member".into()));
            }
        }
        let mut local = Vec::new();
        let mut ids = BTreeSet::new();
        for volume in &g.volumes {
            if volume.id.is_empty()
                || !ids.insert(&volume.id)
                || volume.slots == 0
                || volume.slots > SLOT_COUNT
                || volume.routing_algorithm != 2
                || !(1..=8).contains(&volume.max_candidate_attempts)
                || (!volume.owners.is_empty() && volume.owners.len() != volume.slots as usize)
                || [&volume.cache_socket, &volume.origin_socket]
                    .iter()
                    .any(|s| !s.starts_with('/') || s.len() > 107 || s.contains('\0'))
            {
                return Err(Error("invalid volume or incomplete ownership".into()));
            }
            let mut slots: BTreeMap<String, Vec<u32>> = BTreeMap::new();
            for (slot, owner) in volume.owners.iter().enumerate() {
                if g.nodes.get(owner).is_none_or(|m| m.ip.is_none()) {
                    return Err(Error(format!("slot {slot} has unavailable owner {owner}")));
                }
                slots.entry(owner.clone()).or_default().push(slot as u32);
            }
            local.push(slots);
        }
        let result = Self {
            members: proto::MemberCatalog {
                members: g
                    .nodes
                    .values()
                    .filter(|m| m.ip.is_some() && !m.pod_uid.is_empty())
                    .map(|m| proto::Member {
                        node: hex::decode(&m.id).expect("validated node"),
                        pod_uid: m.pod_uid.clone(),
                        fabric: m.fabric.clone(),
                    })
                    .collect(),
            },
            generation: g,
            by_id,
            local,
        };
        result.admit()?;
        Ok(result)
    }

    fn admit(&self) -> Result<()> {
        let membership_bytes: u64 = self
            .members
            .members
            .iter()
            .map(|m| 64 + m.pod_uid.len() as u64 + m.fabric.len() as u64)
            .sum();
        for name in self.generation.nodes.keys() {
            let (mut work, mut records, mut wire) =
                (0u64, self.members.members.len() as u64, membership_bytes);
            for (v, local) in self.generation.volumes.iter().zip(&self.local) {
                let l = local.get(name.as_str()).map_or(0, |s| s.len()) as u64;
                if l == 0 {
                    continue;
                }
                let p = u64::from(v.slots);
                let d = u64::from(degree(v.slots));
                let edges = (p - l).min(l * d);
                let direct = (local.len() as u64 - 1).min(2 * l * d);
                work += 64 * l;
                records += l + edges + 4 * direct;
                wire += 5 * l
                    + 80 * edges
                    + 2048 * direct
                    + (v.cache_socket.len() + v.origin_socket.len() + v.id.len()) as u64
                    + 1024;
            }
            if work > 64 * 1024 * 1024
                || records > 2 * 1024 * 1024
                || wire > 64 * 1024 * 1024 - 1024
            {
                return Err(Error(format!(
                    "node {name} exceeds profile-1 configuration budget"
                )));
            }
        }
        Ok(())
    }

    /// None means unknown identity; an empty nonidle snapshot is removal.
    pub fn snapshot(&self, id: &str) -> Option<proto::Snapshot> {
        let name = self.by_id.get(id)?;
        let g = &self.generation;
        let member = &g.nodes[name];
        let mut snapshot = proto::Snapshot {
            universe: identity_bytes("universe", &g.universe).to_vec(),
            node: hex::decode(id).ok()?,
            revision: g.revision,
            epoch: g.revision,
            fabric: member.fabric.clone(),
            idle: g.volumes.is_empty() && member.ip.is_some() && !member.pod_uid.is_empty(),
            ..Default::default()
        };
        let mut peers = BTreeMap::new();
        for (v, local) in g.volumes.iter().zip(&self.local) {
            let Some(slots) = local.get(name) else {
                continue;
            };
            let p = v.slots as usize;
            let d = degree(v.slots) as usize;
            let mut outgoing = BTreeSet::new();
            let mut direct_slots = BTreeSet::new();
            let mut dense_outgoing = (slots.len() * d > p).then(|| vec![false; p]);
            let mut dense_direct = dense_outgoing.as_ref().map(|_| vec![false; p]);
            for &slot in slots {
                for digit in 0..d {
                    let next = (slot as usize * d + digit) % p;
                    let incoming = (slot as usize + digit * p) / d;
                    if let (Some(out), Some(direct)) = (&mut dense_outgoing, &mut dense_direct) {
                        out[next] = true;
                        direct[next] = true;
                        direct[incoming] = true;
                    } else {
                        outgoing.insert(next);
                        direct_slots.insert(next);
                        direct_slots.insert(incoming);
                    }
                }
            }
            let mut neighbors = Vec::new();
            let mut out = BTreeSet::new();
            let mut direct = BTreeMap::new();
            let edge_slots: Box<dyn Iterator<Item = usize>> = if let Some(direct) = dense_direct {
                Box::new(
                    direct
                        .into_iter()
                        .enumerate()
                        .filter_map(|(slot, present)| present.then_some(slot)),
                )
            } else {
                Box::new(direct_slots.into_iter())
            };
            for slot in edge_slots {
                let owner = &v.owners[slot];
                if owner == name {
                    continue;
                }
                let remote = &g.nodes[owner];
                if dense_outgoing
                    .as_ref()
                    .map_or_else(|| outgoing.contains(&slot), |out| out[slot])
                {
                    neighbors.push(proto::SlotPeer {
                        slot: slot as u32,
                        peer: remote.id.clone(),
                    });
                    out.insert(remote.id.clone());
                }
                {
                    direct
                        .entry(remote.id.clone())
                        .or_insert_with(|| proto::Peer {
                            id: remote.id.clone(),
                            fabric: remote.fabric.clone(),
                            http_address: SocketAddr::new(remote.ip.unwrap(), 9443).to_string(),
                            pod_uid: remote.pod_uid.clone(),
                        });
                }
            }
            let endpoints = proto::VolumePeerEndpoints {
                peers: direct
                    .values()
                    .map(|p| proto::VolumePeerEndpoint {
                        peer: p.id.clone(),
                        http_address: p.http_address.clone(),
                    })
                    .collect(),
            };
            peers.extend(direct);
            snapshot.volumes.push(proto::Volume {
                id: v.id.clone(),
                cache_generation: v.cache_generation,
                peers: out.into_iter().collect(),
                cache_socket: v.cache_socket.clone(),
                origin_socket: v.origin_socket.clone(),
                max_candidate_attempts: Some(v.max_candidate_attempts),
                peer_endpoints: Some(endpoints),
                topology: Some(proto::Topology {
                    epoch: g.revision,
                    slot_count: v.slots,
                    local_slots: slots.clone(),
                    neighbors,
                    routing_algorithm: Some(v.routing_algorithm),
                }),
                member_catalog: Some(0),
            });
        }
        snapshot.peers = peers.into_values().collect();
        if !snapshot.volumes.is_empty() {
            snapshot.member_catalogs.push(self.members.clone());
        }
        Some(snapshot)
    }
}
