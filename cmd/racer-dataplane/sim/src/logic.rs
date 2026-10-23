//! Stand-in node logic for the `standin` scenario.
//!
//! The real logic lives in [`crate::node`]; every other scenario runs it.
//! This module and [`crate::scenario::World`] stay as a cheap baseline for
//! the substrate. The original plug-in notes follow.
//!
//! The simulator world ([`crate::scenario::World`]) asks a [`NodeLogic`]
//! every decision it needs: which node owns a page, which links reach it,
//! and what the local cache tiers hold. The stand-ins here are deliberately
//! simple (hash ownership, direct or one-relay routes, direct-mapped caches)
//! so the substrate can be exercised before the extracted `racer-core` logic
//! is wired in. Plug-in points for the integrator:
//!
//! - [`NodeLogic::owner`]: replace with the phantom `Placement` ranking
//!   (topology `Placement<Phantom>`), returning the ranked owner.
//! - [`NodeLogic::route`]: replace with the phantom `Paths` key cache plus the
//!   shared route memo; the world enforces the hop limit invariant on the
//!   result.
//! - [`NodeLogic::lookup`]/[`NodeLogic::insert`]/[`NodeLogic::crash`]: replace
//!   with phantom memory LRU and phantom `SegmentClock` disk index.
//! - Read path decisions (candidate walk, flights, fill plan) currently live in
//!   `scenario.rs` as a fixed owner-fetch flow; they become calls into the
//!   `racer-core` state machines, each emitting effects the world schedules
//!   on the [`crate::resources::Fabric`].

use crate::engine::mix;
use crate::faults::Partitions;
use crate::report::MemoryUsage;
use crate::resources::Fabric;

/// Cluster membership: a live bitmap, a dense list of live ids and a
/// version that increases on every change.
#[derive(Clone, Debug)]
pub struct Membership {
    live: Vec<bool>,
    list: Vec<u32>,
    version: u64,
}

impl Membership {
    pub fn all(nodes: u32) -> Self {
        Self {
            live: vec![true; nodes as usize],
            list: (0..nodes).collect(),
            version: 1,
        }
    }

    pub fn version(&self) -> u64 {
        self.version
    }

    pub fn is_live(&self, node: u32) -> bool {
        self.live.get(node as usize).copied().unwrap_or(false)
    }

    pub fn live(&self) -> &[u32] {
        &self.list
    }

    /// Mark `node` live or not. Returns whether anything changed.
    pub fn set(&mut self, node: u32, live: bool) -> bool {
        let Some(slot) = self.live.get_mut(node as usize) else {
            return false;
        };
        if *slot == live {
            return false;
        }
        *slot = live;
        if live {
            let at = self.list.partition_point(|&n| n < node);
            self.list.insert(at, node);
        } else if let Ok(at) = self.list.binary_search(&node) {
            self.list.remove(at);
        }
        self.version += 1;
        true
    }
}

impl MemoryUsage for Membership {
    fn approximate_bytes(&self) -> usize {
        self.live.capacity() + self.list.capacity() * 4
    }
}

/// What a node can see when choosing a route.
pub struct View<'a> {
    pub fabric: &'a Fabric,
    pub partitions: &'a Partitions,
    pub now: u64,
    pub membership: &'a Membership,
}

/// Cache tier where a page was found.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tier {
    Memory,
    Disk,
    Miss,
}

/// Decisions the world delegates. See the module docs for plug-in points.
pub trait NodeLogic: MemoryUsage {
    /// Node that should fetch `page` from the origin, or `None` if no node
    /// is live.
    fn owner(&mut self, membership: &Membership, page: u64) -> Option<u32>;

    /// Write a route (node ids, `from` first, `to` last) into `out`. Return
    /// false when `to` is unreachable.
    fn route(&mut self, view: &View<'_>, from: u32, to: u32, out: &mut Vec<u32>) -> bool;

    fn lookup(&mut self, node: u32, page: u64) -> Tier;

    /// Store `page` in the given tier. Returns true when that evicted a page.
    fn insert(&mut self, node: u32, page: u64, tier: Tier) -> bool;

    /// The node crashed: volatile state is lost, disk survives.
    fn crash(&mut self, node: u32);
}

/// Stand-in logic: hash ownership over the live list, direct routes with a
/// single relay around partitions, direct-mapped memory and disk tiers.
pub struct StandIn {
    memory: Vec<u64>,
    disk: Vec<u64>,
    memory_pages: usize,
    disk_pages: usize,
}

impl StandIn {
    pub fn new(nodes: u32, memory_pages: u32, disk_pages: u32) -> Self {
        let (m, d) = (memory_pages.max(1) as usize, disk_pages.max(1) as usize);
        Self {
            memory: vec![0; nodes as usize * m],
            disk: vec![0; nodes as usize * d],
            memory_pages: m,
            disk_pages: d,
        }
    }

    fn slot(&self, node: u32, page: u64, tier: Tier) -> usize {
        let (width, salt) = match tier {
            Tier::Memory => (self.memory_pages, 0x51),
            _ => (self.disk_pages, 0xD1),
        };
        node as usize * width + (mix(page ^ salt) % width as u64) as usize
    }
}

impl MemoryUsage for StandIn {
    fn approximate_bytes(&self) -> usize {
        (self.memory.capacity() + self.disk.capacity()) * 8
    }
}

impl NodeLogic for StandIn {
    fn owner(&mut self, membership: &Membership, page: u64) -> Option<u32> {
        let live = membership.live();
        (!live.is_empty()).then(|| live[(mix(page) % live.len() as u64) as usize])
    }

    fn route(&mut self, view: &View<'_>, from: u32, to: u32, out: &mut Vec<u32>) -> bool {
        let View {
            fabric,
            partitions,
            now,
            membership,
        } = *view;
        out.clear();
        out.push(from);
        if from == to {
            return true;
        }
        let site = |n: u32| fabric.site[n as usize];
        if !partitions.blocked(now, site(from), site(to)) {
            out.push(to);
            return true;
        }
        // One relay: probe a few live nodes, deterministic in (from, to).
        let live = membership.live();
        for i in 0..8u64 {
            let relay = live
                [(mix((u64::from(from) << 32) | (u64::from(to) ^ i)) % live.len() as u64) as usize];
            if relay != from
                && relay != to
                && !partitions.blocked(now, site(from), site(relay))
                && !partitions.blocked(now, site(relay), site(to))
            {
                out.extend([relay, to]);
                return true;
            }
        }
        false
    }

    fn lookup(&mut self, node: u32, page: u64) -> Tier {
        if self.memory[self.slot(node, page, Tier::Memory)] == page + 1 {
            Tier::Memory
        } else if self.disk[self.slot(node, page, Tier::Disk)] == page + 1 {
            Tier::Disk
        } else {
            Tier::Miss
        }
    }

    fn insert(&mut self, node: u32, page: u64, tier: Tier) -> bool {
        let i = self.slot(node, page, tier);
        let table = match tier {
            Tier::Memory => &mut self.memory,
            Tier::Disk => &mut self.disk,
            Tier::Miss => return false,
        };
        let evicted = table[i] != 0 && table[i] != page + 1;
        table[i] = page + 1;
        evicted
    }

    fn crash(&mut self, node: u32) {
        let m = self.memory_pages;
        self.memory[node as usize * m..(node as usize + 1) * m].fill(0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use crate::engine::{Role, Streams};
    use crate::faults::Fault;

    #[test]
    fn membership_tracks_live_and_version() {
        let mut m = Membership::all(4);
        assert!(!m.set(1, true));
        assert!(m.set(1, false));
        assert_eq!(m.live(), &[0, 2, 3]);
        assert!(!m.is_live(1));
        assert!(m.set(1, true));
        assert_eq!(m.live(), &[0, 1, 2, 3]);
        assert_eq!(m.version(), 3);
        assert!(!m.set(99, false));
        assert!(!m.is_live(99));
    }

    #[test]
    fn tiers_and_crash() {
        let mut s = StandIn::new(2, 4, 8);
        assert_eq!(s.lookup(0, 7), Tier::Miss);
        assert!(!s.insert(0, 7, Tier::Memory));
        assert!(!s.insert(0, 7, Tier::Disk));
        assert!(!s.insert(0, 7, Tier::Miss));
        assert_eq!(s.lookup(0, 7), Tier::Memory);
        assert_eq!(s.lookup(1, 7), Tier::Miss);
        s.crash(0);
        assert_eq!(s.lookup(0, 7), Tier::Disk, "disk survives a crash");
        // A direct-mapped collision evicts.
        let other = (8..1000)
            .find(|p| s.slot(0, *p, Tier::Memory) == s.slot(0, 7, Tier::Memory))
            .unwrap();
        s.insert(0, 7, Tier::Memory);
        assert!(s.insert(0, other, Tier::Memory));
    }

    #[test]
    fn owner_and_routes() {
        let mut s = StandIn::new(8, 1, 1);
        let mut m = Membership::all(8);
        let o = s.owner(&m, 42).unwrap();
        assert!(o < 8);
        for n in 0..8 {
            m.set(n, false);
        }
        assert_eq!(s.owner(&m, 42), None);

        let config = Config {
            nodes: 8,
            sites: 4,
            ..Config::default()
        };
        let fabric = Fabric::new(&config, &mut Streams::new(1).stream(Role::Topology, 0));
        let m = Membership::all(8);
        let mut out = Vec::new();
        let none = Partitions::default();
        assert!(s.route(
            &View {
                fabric: &fabric,
                partitions: &none,
                now: 0,
                membership: &m
            },
            0,
            0,
            &mut out
        ));
        assert_eq!(out, [0]);
        assert!(s.route(
            &View {
                fabric: &fabric,
                partitions: &none,
                now: 0,
                membership: &m
            },
            0,
            1,
            &mut out
        ));
        assert_eq!(out, [0, 1]);
        let cut = Partitions::from_faults(&[Fault::Partition {
            from_ns: 0,
            until_ns: 10,
            site_a: 0,
            site_b: 1,
        }]);
        assert!(s.route(
            &View {
                fabric: &fabric,
                partitions: &cut,
                now: 5,
                membership: &m
            },
            0,
            1,
            &mut out
        ));
        assert_eq!(out.len(), 3);
        let relay = out[1];
        assert!(![0, 1].contains(&fabric.site[relay as usize]));
        // Fully isolated site: no relay works.
        let island = Partitions::from_faults(
            &(1..4)
                .map(|b| Fault::Partition {
                    from_ns: 0,
                    until_ns: 10,
                    site_a: 0,
                    site_b: b,
                })
                .collect::<Vec<_>>(),
        );
        assert!(!s.route(
            &View {
                fabric: &fabric,
                partitions: &island,
                now: 5,
                membership: &m
            },
            0,
            1,
            &mut out
        ));
    }
}
