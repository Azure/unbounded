// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use sha2::{Digest, Sha256};

use crate::model::*;
use crate::{Error, Result, proto};

#[path = "placement_candidates.rs"]
mod placement_candidates;

/// Stateless highest-random-weight rendezvous. Each slot chooses the maximum
/// unsigned 64-bit score; ties choose the lexically smallest Node identity.
/// Balance is statistical. Adjacent slots may have the same owner, and live
/// participants may own zero slots, including when participants exceed slots.
pub fn place_in_universe(slots: u32, universe: &str, ids: &[String]) -> Result<Vec<String>> {
    PlacementCache::default().place(slots, universe, ids)
}

// XXH64 from https://github.com/Cyan4973/xxHash/blob/v0.8.3/doc/xxhash_spec.md,
// seed zero, specialized to a 36-byte message. The score is
// XXH64(SHA256(domain || universe_id[32] || node_id[32]) || LE32(slot), 0).
// domain is the exact bytes b"racer/placement/hrw/v1\0"; universe_id and node_id
// are the existing binary SHA-256 identities, not their hexadecimal text.
// This fixed-width encoding has no ambiguous concatenations. SHA-256 includes
// all identity bits; XXH64 cheaply avalanches each slot without new dependencies.
// Pod identity, IP, cache identity/generation, revision and fence are not inputs.
const P1: u64 = 11_400_714_785_074_694_791;
const P2: u64 = 14_029_467_366_897_019_727;
const P3: u64 = 1_609_587_929_392_839_161;
const P4: u64 = 9_650_029_242_287_828_579;

fn xxh_round(acc: u64, lane: u64) -> u64 {
    acc.wrapping_add(lane.wrapping_mul(P2))
        .rotate_left(31)
        .wrapping_mul(P1)
}

fn score_prefix(universe: &[u8; 32], node: &[u8; 32]) -> u64 {
    let mut hash = Sha256::new();
    hash.update(b"racer/placement/hrw/v1\0");
    hash.update(universe);
    hash.update(node);
    let digest = hash.finalize();
    let mut lanes = [P1.wrapping_add(P2), P2, 0, 0u64.wrapping_sub(P1)];
    for (acc, chunk) in lanes.iter_mut().zip(digest.chunks_exact(8)) {
        *acc = xxh_round(*acc, u64::from_le_bytes(chunk.try_into().unwrap()));
    }
    let mut h = lanes[0]
        .rotate_left(1)
        .wrapping_add(lanes[1].rotate_left(7))
        .wrapping_add(lanes[2].rotate_left(12))
        .wrapping_add(lanes[3].rotate_left(18));
    for lane in lanes {
        h = (h ^ xxh_round(0, lane)).wrapping_mul(P1).wrapping_add(P4);
    }
    h.wrapping_add(36)
}

#[inline]
fn score(prefix: u64, slot: u32) -> u64 {
    let mut h = (prefix ^ u64::from(slot).wrapping_mul(P1))
        .rotate_left(23)
        .wrapping_mul(P2)
        .wrapping_add(P3);
    h = (h ^ (h >> 33)).wrapping_mul(P2);
    h = (h ^ (h >> 29)).wrapping_mul(P3);
    h ^ (h >> 32)
}

#[cfg(test)]
#[path = "../tests/placement/hash.rs"]
mod placement_hash;

/// Disposable, single-universe acceleration, never persisted or trusted as
/// externally supplied ownership. Adding nodes compares only their scores;
/// removing nodes rescans only slots whose winner disappeared. Every result is
/// exactly the cold HRW result, including simultaneous additions and removals.
/// Space is O(slots + nodes); cold work is O(slots * nodes).
#[derive(Default)]
pub struct PlacementCache {
    universe: String,
    ids: Vec<String>,
    winners: Vec<(usize, u64)>,
    rankings: placement_candidates::Rankings,
}

impl PlacementCache {
    pub fn place(&mut self, slots: u32, universe: &str, ids: &[String]) -> Result<Vec<String>> {
        let mut ids = ids.to_vec();
        ids.sort();
        if slots == 0
            || slots > SLOT_COUNT
            || universe.is_empty()
            || ids.len() > 100_000
            || ids.windows(2).any(|w| w[0] == w[1])
        {
            return Err(Error(
                "invalid placement capacity, universe or duplicate identity".into(),
            ));
        }
        let universe_id = identity_bytes("universe", universe);
        let prefixes: Vec<_> = ids
            .iter()
            .map(|id| {
                if id.len() != 64
                    || !id
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                {
                    return Err(Error("invalid placement node identity".into()));
                }
                let mut node = [0; 32];
                hex::decode_to_slice(id, &mut node).map_err(|e| Error(e.to_string()))?;
                Ok(score_prefix(&universe_id, &node))
            })
            .collect::<Result<_>>()?;
        if self.universe != universe || self.winners.len() != slots as usize {
            self.ids.clear();
            self.winners = vec![(usize::MAX, 0); slots as usize];
        }
        let remap: Vec<_> = self
            .ids
            .iter()
            .map(|id| ids.binary_search(id).ok())
            .collect();
        let added: Vec<_> = ids
            .iter()
            .enumerate()
            .filter_map(|(i, id)| self.ids.binary_search(id).is_err().then_some(i))
            .collect();
        for (slot, winner) in self.winners.iter_mut().enumerate() {
            let retained = remap.get(winner.0).copied().flatten();
            let mut best = retained.map_or((usize::MAX, 0), |i| (i, winner.1));
            let mut consider = |i: usize| {
                let value = score(prefixes[i], slot as u32);
                if value > best.1 || (value == best.1 && i < best.0) {
                    best = (i, value);
                }
            };
            if retained.is_some() {
                for &i in &added {
                    consider(i);
                }
            } else {
                for i in 0..ids.len() {
                    consider(i);
                }
            }
            *winner = best;
        }
        self.universe = universe.into();
        self.ids = ids;
        Ok(if self.ids.is_empty() {
            Vec::new()
        } else {
            self.winners
                .iter()
                .map(|&(i, _)| self.ids[i].clone())
                .collect()
        })
    }
}

/// Compile desired content at the previous revision. Publication assigns the
/// next revision only if content changed. Errors leave the caller's state intact.
pub fn compile(input: &Inventory, previous: Option<&Generation>) -> Result<Generation> {
    compile_cached(input, previous, &mut PlacementCache::default())
}

/// Compile current ownership from inventory, retaining valid process-local roles.
/// A caller may retain one disposable placement cache per universe across polls.
pub fn compile_cached(
    input: &Inventory,
    previous: Option<&Generation>,
    placement: &mut PlacementCache,
) -> Result<Generation> {
    if input.universe.is_empty() {
        return Err(Error("universe must not be empty".into()));
    }
    if let Some(old) = previous {
        if old.universe != input.universe {
            return Err(Error("previous universe mismatch".into()));
        }
    }
    let mut g = Generation::empty(&input.universe);
    g.revision = previous.map_or(0, |old| old.revision);
    let mut seen = BTreeSet::new();
    for node in &input.nodes {
        if !node.eligible || node.universe != input.universe {
            continue;
        }
        if node.name.is_empty() || node.uid.is_empty() || !seen.insert(&node.name) {
            return Err(Error("empty or duplicate node identity".into()));
        }
        if node.fabric.len() > 256 || !node.fabric.bytes().all(|b| (0x21..=0x7e).contains(&b)) {
            return Err(Error(format!("node {} has invalid fabric", node.name)));
        }
        let id = identity("node", &node.uid);
        let member = g.nodes.entry(node.name.clone()).or_default();
        member.id = id;
        member.fabric = node.fabric.clone();
        let mut pods: Vec<_> = node
            .pods
            .iter()
            .filter(|p| {
                node.ready && p.available && !p.uid.is_empty() && available_ip(&p.ip).is_some()
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
    let active: BTreeMap<_, _> = g
        .nodes
        .iter()
        .filter(|(_, n)| n.ip.is_some())
        .map(|(name, member)| (member.id.clone(), name.clone()))
        .collect();
    if active.len() > 100_000 {
        return Err(Error("universe exceeds 100000 participants".into()));
    }
    let members: Vec<_> = active.keys().cloned().collect();
    let max_attempts = input
        .caches
        .iter()
        .map(|c| c.max_candidate_attempts)
        .max()
        .unwrap_or(1);
    if !(1..=8).contains(&max_attempts) {
        return Err(Error("invalid candidate attempt limit".into()));
    }
    let owners = if input.caches.is_empty() || active.is_empty() {
        Vec::new()
    } else {
        let width = max_attempts.min(members.len() as u32);
        let candidates = placement.candidates(SLOT_COUNT, &input.universe, &members, width)?;
        let (product, roles) =
            crate::product_topology::assign(&members, previous.and_then(|g| g.product.as_ref()))?;
        let owners = candidates
            .chunks_exact(width as usize)
            .map(|row| active[&members[row[0] as usize]].clone())
            .collect();
        g.product = Some(ProductPlacement {
            left_factor: product.left.order(),
            right_factor: product.right.order(),
            members,
            roles,
            candidate_width: width,
            candidates,
        });
        owners
    };
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
        let (client_socket, origin_socket) = cache_sockets(&input.socket_root, &cache.name)?;
        g.volumes.push(Volume {
            id: cache.uid.clone(),
            name: cache.name.clone(),
            resource_generation: cache.resource_generation,
            client_socket,
            origin_socket,
            slots: SLOT_COUNT,
            cache_generation: cache.cache_generation as u64,
            routing_algorithm: PRODUCT_ROUTING_ALGORITHM,
            max_candidate_attempts: cache.max_candidate_attempts,
            owners: owners.clone(),
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
    /// Validate generation content and profile-1 admission before publishing it.
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
                || volume.routing_algorithm != PRODUCT_ROUTING_ALGORITHM
                || !(1..=8).contains(&volume.max_candidate_attempts)
                || (!volume.owners.is_empty() && volume.owners.len() != volume.slots as usize)
                || [&volume.client_socket, &volume.origin_socket]
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
        crate::product_topology::validate(&g)?;
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
        let peer_counts = self
            .generation
            .product
            .as_ref()
            .map(crate::product_topology::peer_counts);
        let membership_bytes: u64 = self
            .members
            .members
            .iter()
            .map(|m| 64 + m.pod_uid.len() as u64 + m.fabric.len() as u64)
            .sum();
        for name in self.generation.nodes.keys() {
            let (mut work, mut records, mut wire) =
                (0u64, self.members.members.len() as u64, membership_bytes);
            for v in &self.generation.volumes {
                if let Some(product) = &self.generation.product {
                    if let Ok(index) = product
                        .members
                        .binary_search(&self.generation.nodes[name].id)
                    {
                        let direct = peer_counts.as_ref().unwrap()[product.roles[index] as usize];
                        let (w, r, b) = crate::product_topology::budget(product, v, direct)?;
                        work += w;
                        records += r;
                        wire += b;
                    }
                }
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
            idle: member.ip.is_some()
                && !member.pod_uid.is_empty()
                && g.product.is_none()
                && self.local.iter().all(|local| !local.contains_key(name)),
            ..Default::default()
        };
        let mut peers = BTreeMap::new();
        for (v, local) in g.volumes.iter().zip(&self.local) {
            if let Some(product) = &g.product {
                if let Ok(index) = product
                    .members
                    .binary_search_by(|member| member.as_str().cmp(id))
                {
                    let (volume, direct) = crate::product_topology::snapshot(
                        g,
                        product,
                        v,
                        index,
                        local.get(name).cloned().unwrap_or_default(),
                        &self.by_id,
                    );
                    snapshot.volumes.push(volume);
                    peers.extend(direct);
                }
            }
        }
        snapshot.peers = peers.into_values().collect();
        if !snapshot.volumes.is_empty() {
            snapshot.member_catalogs.push(self.members.clone());
        }
        Some(snapshot)
    }
}
