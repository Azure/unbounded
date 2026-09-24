// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Validated physical catalogs and deterministic, prefix-preserving local repair.
use super::{
    proto,
    routing::{Cursor, Routing, invalid},
};
use crate::{product::Product, topology::Topology};
use std::{collections::BTreeSet, io};

pub(super) struct Physical {
    pub(super) config: proto::ProductTopology,
    graph: Product,
    bundles: Vec<Vec<u32>>,
}

impl Physical {
    pub(super) fn route(&self, source: u32, target: u32) -> io::Result<Vec<u32>> {
        if source == target {
            return Ok(vec![source]);
        }
        let (s, t) = (
            self.config.roles[source as usize],
            self.config.roles[target as usize],
        );
        if s == t {
            let role = self
                .graph
                .neighbors(s)
                .into_iter()
                .next()
                .ok_or_else(invalid)?;
            return Ok(vec![source, self.bundles[role as usize][0], target]);
        }
        let roles = self.graph.route(s, t).map_err(|_| invalid())?;
        self.lift(&roles, source, target, u32::MAX)
    }

    fn lift(&self, roles: &[u32], source: u32, target: u32, failed: u32) -> io::Result<Vec<u32>> {
        let mut path = vec![source];
        for &role in roles.iter().skip(1).take(roles.len().saturating_sub(2)) {
            path.push(
                *self.bundles[role as usize]
                    .iter()
                    .find(|&&m| m != failed)
                    .ok_or_else(invalid)?,
            );
        }
        path.push(target);
        Ok(path)
    }

    fn repaired(&self, healthy: &[u32], at: usize, failed: u32) -> io::Result<Vec<u32>> {
        if at + 2 >= healthy.len() || healthy[at + 1] != failed {
            return Err(invalid());
        }
        let mut path = healthy.to_vec();
        let role = self.config.roles[failed as usize];
        if let Some(&twin) = self.bundles[role as usize].iter().find(|&&m| m != failed) {
            path[at + 1] = twin;
        } else {
            let source = healthy[at];
            let target = *healthy.last().unwrap();
            let (s, t) = (
                self.config.roles[source as usize],
                self.config.roles[target as usize],
            );
            let suffix = if s == t {
                let role = self
                    .graph
                    .neighbors(s)
                    .into_iter()
                    .find(|&r| r != role)
                    .ok_or_else(invalid)?;
                vec![source, self.bundles[role as usize][0], target]
            } else {
                self.lift(
                    &self.graph.repair(s, t, role).map_err(|_| invalid())?,
                    source,
                    target,
                    failed,
                )?
            };
            path.truncate(at);
            path.extend(suffix);
        }
        if path.len() > 5
            || path.contains(&failed)
            || path.iter().collect::<BTreeSet<_>>().len() != path.len()
        {
            return Err(invalid());
        }
        Ok(path)
    }
}

impl Routing {
    pub(super) fn new_product(
        universe: &[u8],
        volume: &proto::Volume,
        topology: proto::Topology,
        geometry: Topology,
    ) -> io::Result<Self> {
        let config = topology.product.ok_or_else(invalid)?;
        let graph = Product::new(config.left_factor, config.right_factor).map_err(|_| invalid())?;
        let count = config.members.len();
        if count == 0
            || count > 100_000
            || !graph.supports_members(count as u32)
            || config.roles.len() != count
            || config.local_member as usize >= count
            || !(1..=8).contains(&config.candidate_width)
            || config.candidate_width as usize > count
            || config.candidates.len()
                != geometry.slot_count() as usize * config.candidate_width as usize
            || config.members.windows(2).any(|w| w[0] >= w[1])
            || config
                .members
                .iter()
                .any(|id| crate::http_auth::identity_bytes(id).is_err())
        {
            return Err(invalid());
        }
        let mut bundles = vec![Vec::new(); graph.order() as usize];
        for (member, &role) in config.roles.iter().enumerate() {
            bundles
                .get_mut(role as usize)
                .ok_or_else(invalid)?
                .push(member as u32);
        }
        if bundles.iter().any(Vec::is_empty)
            || bundles.iter().map(Vec::len).max().unwrap()
                - bundles.iter().map(Vec::len).min().unwrap()
                > 1
        {
            return Err(invalid());
        }
        for row in config
            .candidates
            .chunks_exact(config.candidate_width as usize)
        {
            if row
                .iter()
                .enumerate()
                .any(|(i, &m)| m as usize >= count || row[..i].contains(&m))
            {
                return Err(invalid());
            }
        }
        let local: BTreeSet<_> = topology.local_slots.iter().copied().collect();
        let expected_local: BTreeSet<_> = config
            .candidates
            .chunks_exact(config.candidate_width as usize)
            .enumerate()
            .filter_map(|(slot, row)| (row[0] == config.local_member).then_some(slot as u32))
            .collect();
        if local.len() != topology.local_slots.len() || local != expected_local {
            return Err(invalid());
        }
        let required: BTreeSet<_> = graph
            .neighbors(config.roles[config.local_member as usize])
            .into_iter()
            .flat_map(|r| {
                bundles[r as usize]
                    .iter()
                    .map(|&m| config.members[m as usize].clone())
            })
            .collect();
        let peers: BTreeSet<_> = volume.peers.iter().cloned().collect();
        if peers != required || peers.len() != volume.peers.len() {
            return Err(invalid());
        }
        let mut hash = blake3::Hasher::new();
        hash.update(b"racer/product-routing/v1");
        for bytes in [universe, volume.id.as_bytes()] {
            hash.update(&(bytes.len() as u64).to_le_bytes());
            hash.update(bytes);
        }
        hash.update(&volume.cache_generation.to_le_bytes());
        hash.update(&topology.epoch.to_le_bytes());
        for value in [
            geometry.slot_count(),
            config.left_factor,
            config.right_factor,
            config.candidate_width,
            count as u32,
        ] {
            hash.update(&value.to_le_bytes());
        }
        for (id, role) in config.members.iter().zip(&config.roles) {
            hash.update(id.as_bytes());
            hash.update(&role.to_le_bytes());
        }
        for candidate in &config.candidates {
            hash.update(&candidate.to_le_bytes());
        }
        Ok(Self {
            geometry,
            local,
            identity: *hash.finalize().as_bytes(),
            product: Physical {
                config,
                graph,
                bundles,
            },
            #[cfg(test)]
            namespace: crate::cache::Namespace::volume(
                universe,
                &volume.id,
                volume.cache_generation,
                crate::cache::Namespace::new(&volume.id).map_err(crate::cache::Error::into_io)?,
            ),
        })
    }

    pub(super) fn validate_product(&self, c: &Cursor) -> io::Result<()> {
        let p = &self.product;
        if c.identity != self.identity
            || c.owner >= self.geometry.slot_count()
            || c.attempt >= p.config.candidate_width
            || c.source as usize >= p.config.members.len()
            || c.position as usize >= c.path.len()
            || c.path.len() > 5
            || c.repair_position > c.position
        {
            return Err(invalid());
        }
        let healthy = p.route(c.source, self.destination(c))?;
        let expected = if c.failed == u32::MAX {
            if c.repair_position != 0 {
                return Err(invalid());
            }
            healthy
        } else {
            if c.failed as usize >= p.config.members.len() {
                return Err(invalid());
            }
            p.repaired(&healthy, c.repair_position as usize, c.failed)?
        };
        if c.path != expected {
            return Err(invalid());
        }
        Ok(())
    }

    /// Only the caller holding evidence about this immediate exchange may invoke repair.
    pub fn repair(&self, c: &Cursor) -> io::Result<Cursor> {
        self.validate_product(c)?;
        let p = &self.product;
        if c.failed != u32::MAX || c.path[c.position as usize] != p.config.local_member {
            return Err(invalid());
        }
        let failed = *c.path.get(c.position as usize + 1).ok_or_else(invalid)?;
        let mut repaired = c.clone();
        repaired.failed = failed;
        repaired.repair_position = c.position;
        repaired.path = p.repaired(&c.path, c.position as usize, failed)?;
        self.validate_product(&repaired)?;
        Ok(repaired)
    }
}

#[cfg(test)]
#[path = "../../tests/control/product.rs"]
pub(crate) mod tests;
