// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! The node configuration: the only input the control plane gives the dataplane, one
//! protobuf file per node, replaced whole and applied all or nothing.
//!
//! Three layers, in order: `pb` is the wire schema, tag for tag; [`Config`] is the
//! validated model, and building one from `pb` is where every structural check happens;
//! `Watch`/[`watch`] are delivery, since the control plane renames a new file over the
//! old one and inotify reports it.
//!
//! `Live` makes reload cheap: a pointer the control thread swaps and every worker reads
//! without a lock.

use std::ffi::CString;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::path::Path;
use std::sync::Mutex;
use std::sync::atomic::{AtomicPtr, AtomicU64, Ordering};

use prost::Message as _;

/// Consensus group slots; `group_slots` has exactly this many entries.
const SLOTS: usize = 16384;
/// Volumes this node may export at once, and the ceiling on a volume's fabric slot.
pub(crate) const MAX_VOLUMES: usize = 60;
const SMALL_PAGE: u64 = 4096;
const HUGE_PAGE: u64 = 4 << 20;

/// The largest `Policy::cache_target_rate` worth asking for: the cache's demand counter
/// is four bits wide (cache.rs), so it never observes a rate above this.
pub(crate) const CACHE_MAX_RATE: u32 = 15;

/// Configs the watcher refused. A rejection is not actionable, so it is counted and
/// dropped; it surfaces as `racer_config_rejected_total` (metrics.rs), an operator's
/// signal that the control plane is writing a config this node will not take.
static REJECTED: AtomicU64 = AtomicU64::new(0);

/// Configs refused since boot.
pub fn rejected() -> u64 {
    REJECTED.load(Ordering::Relaxed)
}

fn bad(msg: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, msg.into())
}

// ---------------------------------------------------------------------------
// Live: the reload primitive
// ---------------------------------------------------------------------------

/// A value the control thread replaces and every worker reads without a lock.
///
/// A read is a single acquire load, sound because the replaced value stays alive one
/// more generation: [`Runtime::reload`] blocks until every core has cut over and the
/// previous value has been retired, so no worker can still be reading what a second
/// install retires. Callers must not hold the returned reference across a reload.
///
/// [`Runtime::reload`]: crate::runtime::Runtime::reload
pub(crate) struct Live<T> {
    cur: AtomicPtr<T>,
    /// The generation before `cur`. Dropped when a third arrives.
    prev: Mutex<Option<Box<T>>>,
}

impl<T> Live<T> {
    pub(crate) fn new(v: T) -> Live<T> {
        Live {
            cur: AtomicPtr::new(Box::into_raw(Box::new(v))),
            prev: Mutex::new(None),
        }
    }

    pub(crate) fn get(&self) -> &T {
        // SAFETY: never null; `new` publishes one and `install` only swaps in another.
        unsafe { &*self.cur.load(Ordering::Acquire) }
    }

    /// Publish `v`. Control thread only.
    pub(crate) fn install(&self, v: T) {
        let old = self.cur.swap(Box::into_raw(Box::new(v)), Ordering::AcqRel);
        let mut prev = self.prev.lock().unwrap();
        // `old` may still be in the hands of a request in flight against the outgoing
        // runtime version; whatever `prev` held was retired by the previous reload, so
        // dropping it here is safe.
        *prev = Some(unsafe { Box::from_raw(old) });
    }
}

impl<T> Drop for Live<T> {
    fn drop(&mut self) {
        drop(unsafe { Box::from_raw(*self.cur.get_mut()) });
    }
}

// ---------------------------------------------------------------------------
// Wire schema
// ---------------------------------------------------------------------------

/// The file as it is written and read, generated from `proto/config.proto`: the schema
/// shared verbatim with the Go control plane, where the tags are the compatibility
/// contract.
///
/// Every field is optional and unvalidated on the wire, so nothing outside this module
/// reads `pb`: [`Config`] is the checked form.
mod pb {
    include!(concat!(env!("OUT_DIR"), "/racer.config.rs"));
}

/// What guard a write to an extent must present. Narrower than the wire enum, which
/// also spells the page size: `IMMUTABLE_4M` is the 4 MiB spelling of [`Kind::Immutable`]
/// and carries the same guard. Width is a property of the volume everywhere below this
/// module, so the two are split apart on the way in and rejoined on the way out.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Kind {
    Lww,
    Occ,
    Immutable,
}

/// The wire kind as (guard, 4 MiB).
fn split_kind(k: pb::Kind) -> (Kind, bool) {
    match k {
        pb::Kind::Lww => (Kind::Lww, false),
        pb::Kind::Occ => (Kind::Occ, false),
        pb::Kind::Immutable => (Kind::Immutable, false),
        pb::Kind::Immutable4m => (Kind::Immutable, true),
    }
}

/// The inverse of [`split_kind`]. Only Immutable has a 4 MiB spelling, so the other two
/// ignore the width their volume cannot have.
fn join_kind(k: Kind, huge: bool) -> pb::Kind {
    match k {
        Kind::Lww => pb::Kind::Lww,
        Kind::Occ => pb::Kind::Occ,
        Kind::Immutable if huge => pb::Kind::Immutable4m,
        Kind::Immutable => pb::Kind::Immutable,
    }
}

// ---------------------------------------------------------------------------
// The model
// ---------------------------------------------------------------------------

/// A contiguous run of pages within a volume. Length is in pages, not bytes, so a
/// misaligned extent is unrepresentable. An extent has no offset: its position in the
/// volume's list *is* its offset. Page size is a property of the volume, not of this.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Extent {
    pages: u64,
    pub(crate) kind: Kind,
    /// Home zone, within the volume's site. Never zero.
    pub(crate) zone: u32,
    /// 0 = not migrating; else the zone bytes are being pulled to.
    pub(crate) next_zone: u32,
}

#[derive(Clone, Debug)]
pub(crate) struct Volume {
    /// Globally unique, never reused; the top 8 bits are the site.
    pub(crate) id: u32,
    /// The volume's six-bit name on the fabric (fabric.rs): an id is 32 bits and a
    /// frame has no room for it. Not derived from position or id order — every node
    /// must decode a given LBA to the same page, and anything derived would shift when
    /// volume lists differ.
    pub(crate) slot: u8,
    /// Served in 4 MiB units rather than 4 KiB. Derived from the extents, which must
    /// agree, and held here rather than per extent so that the exported block device
    /// can advertise one page as both its max and its chunk size, making every request
    /// exactly one page.
    pub(crate) huge: bool,
    pub(crate) extents: Vec<Extent>,
    /// Scoped to this volume: page versions in it are `3*epoch + state`. Advancing it
    /// reclaims this volume's tombstones and leaves every other volume alone.
    pub(crate) tombstone_epoch: u64,
    /// Prefix sums of `extents[i].pages`, one longer than `extents`, so `extent_index`
    /// is a binary search instead of a walk.
    starts: Vec<u64>,
}

impl Volume {
    fn new(id: u32, slot: u8, huge: bool, extents: Vec<Extent>, tombstone_epoch: u64) -> Volume {
        let mut starts = Vec::with_capacity(extents.len() + 1);
        let mut acc = 0u64;
        starts.push(0);
        for e in &extents {
            acc += e.pages;
            starts.push(acc);
        }
        Volume {
            id,
            slot,
            huge,
            extents,
            tombstone_epoch,
            starts,
        }
    }

    /// The site this volume is homed in: the top 8 bits of its id.
    fn site(&self) -> u32 {
        self.id >> 24
    }

    /// Total length in pages.
    pub(crate) fn pages(&self) -> u64 {
        *self.starts.last().unwrap()
    }

    fn page_bytes(&self) -> u64 {
        if self.huge { HUGE_PAGE } else { SMALL_PAGE }
    }

    /// Size of the exported block device.
    pub(crate) fn bytes(&self) -> u64 {
        self.pages() * self.page_bytes()
    }

    /// The extent covering page `offset`.
    pub(crate) fn extent_at(&self, offset: u64) -> Option<&Extent> {
        Some(&self.extents[self.extent_index(offset)?])
    }

    /// The half-open page range extent `i` covers.
    pub(crate) fn extent_range(&self, i: usize) -> Option<(u64, u64)> {
        Some((*self.starts.get(i)?, *self.starts.get(i + 1)?))
    }

    /// The extent's index in the volume, which is also its name: an extent has no
    /// offset field because its index *is* its offset, and the consensus shard id
    /// (paxos.rs).
    pub(crate) fn extent_index(&self, offset: u64) -> Option<usize> {
        if offset >= self.pages() {
            return None;
        }
        // `starts` is sorted; partition_point gives the first start strictly above.
        Some(self.starts.partition_point(|&s| s <= offset) - 1)
    }
}

#[derive(Clone, Debug)]
pub struct Node {
    pub(crate) id: u32,
    pub(crate) site: u32,
    pub(crate) zone: u32,
    /// 0..2, the catalog column that is our cache cohort (cache.rs).
    pub(crate) cohort: u8,
    /// Backing NVMe namespace this node owns outright.
    pub device: String,
    pub(crate) cache_bytes_4k: u64,
    pub(crate) cache_bytes_4m: u64,
    /// The rate we are willing to drive the device at, zero for unmetered. Read once
    /// per IO, and only at startup can it change.
    pub(crate) device_max_iops: u64,
    pub(crate) device_max_bytes_per_sec: u64,
    /// The largest share of this zone this node may be given, out of `SLOTS`. The
    /// device is sized from this rather than from the live share, because slots are
    /// appended only at startup: a node whose slabs tracked its share would run short
    /// every time the control plane moved a group to it, until it was restarted.
    pub(crate) max_share_slots: u32,
    /// Nodes this one can reach directly, site crossings included.
    pub(crate) peers: Vec<Peer>,
}

impl Default for Node {
    fn default() -> Node {
        // An unset ceiling is the whole zone: a control plane that omits the field gets
        // a node sized for every page in it, which is wasteful but never short.
        Node {
            id: 0,
            site: 0,
            zone: 0,
            cohort: 0,
            device: String::new(),
            cache_bytes_4k: 0,
            cache_bytes_4m: 0,
            device_max_iops: 0,
            device_max_bytes_per_sec: 0,
            max_share_slots: SLOTS as u32,
            peers: Vec::new(),
        }
    }
}

/// One end of a fabric link. There is no address, port or NQN here on purpose: the
/// control plane owns the nvmet target and initiator configuration, so by the time we
/// see a peer it is already a local device path.
#[derive(Clone, Debug, Default)]
pub(crate) struct Peer {
    pub(crate) id: u32,
    /// Local path to the peer's fabric namespace, already attached.
    pub(crate) device: String,
    /// `Some(site)` iff this link is a site crossing. Any node may hold one; everything
    /// but routing treats it as an ordinary link.
    pub(crate) site: Option<u32>,
    /// Sites this peer carries traffic to on our behalf, because it holds a crossing to
    /// each. Empty on almost every peer, and the whole of what "gateway" now means.
    pub(crate) gateway_for: Vec<u32>,
}

/// Another zone in this site, and the entry node of each cohort in it.
#[derive(Clone, Debug, PartialEq)]
struct Zone {
    id: u32,
    /// Three entry nodes, one per cohort.
    entry: [u32; 3],
}

#[derive(Clone, Debug, Default)]
pub(crate) struct Topology {
    /// This zone's topology epoch: the term a shard is sealed with, and it rides the
    /// trailer of every routed write (paxos.rs).
    pub(crate) epoch: u32,
    /// Index is the group id. Each entry is exactly three distinct node ids, ordered by
    /// paxos member index, which is also the cohort column.
    pub(crate) catalog: Vec<[u32; 3]>,
    /// `SLOTS` entries, each an index into `catalog`.
    group_slots: Vec<u32>,
    /// The other zones in this site. Placement is intra-site, so this plus our own is
    /// the set an extent's `zone` and `next_zone` may name.
    zones: Vec<Zone>,
}

#[derive(Clone, Debug)]
pub(crate) struct Policy {
    /// DRAM ceiling for the 4 KiB index. A config whose small-page working set would
    /// exceed this is refused rather than allowed to OOM later.
    max_index_bytes: u64,
    /// DRAM ceiling for the OCC read pool across the whole node. Evicting a read
    /// record can only turn a would-be success into a conflict, so this bounds
    /// memory without bounding correctness.
    pub(crate) occ_bytes: u64,
    /// The cache's `τ`, in requests per decay interval. Zero disables the cache.
    pub(crate) cache_target_rate: u32,
    /// Registers one anti-entropy sweep pulls while replaying a group, and pushes per
    /// extent while handing one over. The rate a rebalance runs at (heal.rs).
    pub(crate) repairs_per_replay: u32,
}

impl Default for Policy {
    fn default() -> Policy {
        Policy {
            max_index_bytes: 8 << 30,
            occ_bytes: 256 << 20,
            cache_target_rate: 0,
            repairs_per_replay: 4096,
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct Config {
    /// Strictly increasing; a non-advancing value is rejected. Echoed in this node's
    /// PING (server.rs), so a node redirected on a stale topology epoch can tell
    /// whether it holds the file it was told to fetch.
    pub(crate) generation: u64,
    pub node: Node,
    pub(crate) topology: Topology,
    /// Sorted by fabric slot, so `volume_at` is a binary search.
    pub(crate) volumes: Vec<Volume>,
    pub(crate) policy: Policy,
}

impl Config {
    /// Read and decode the file the control plane renamed into place. Validation is
    /// separate; see `validate`.
    pub fn load(path: &Path) -> io::Result<Config> {
        Config::decode(&std::fs::read(path)?)
    }

    fn decode(bytes: &[u8]) -> io::Result<Config> {
        let pb = pb::NodeConfig::decode(bytes).map_err(|e| bad(format!("protobuf: {e}")))?;
        Config::from_pb(pb)
    }

    pub fn encode(&self) -> Vec<u8> {
        self.to_pb().encode_to_vec()
    }

    pub(crate) fn volume(&self, id: u32) -> Option<&Volume> {
        self.volumes.iter().find(|v| v.id == id)
    }

    /// The volume a frame's six-bit slot names.
    pub(crate) fn volume_at(&self, slot: u8) -> Option<&Volume> {
        let i = self.volumes.binary_search_by_key(&slot, |v| v.slot).ok()?;
        self.volumes.get(i)
    }

    /// 4 KiB page slots this node must be able to address.
    pub(crate) fn small_pages(&self) -> u64 {
        self.count_pages(false)
    }

    /// 4 MiB page slots this node must be able to address.
    pub(crate) fn huge_pages(&self) -> u64 {
        self.count_pages(true)
    }

    /// This node's share of its zone, by class. A page is stored by the three members
    /// of the group its slot points at, so what a node holds is the zone's pages scaled
    /// by the fraction of slots that land on a group it belongs to.
    ///
    /// Scaled by the ceiling rather than the live share: see `Node::max_share_slots`.
    /// The two agree on a node whose ceiling the control plane has filled.
    fn count_pages(&self, huge: bool) -> u64 {
        let share = self.node.max_share_slots.min(SLOTS as u32) as u64;
        self.zone_pages(huge)
            .saturating_mul(share)
            .div_ceil(SLOTS as u64)
    }

    /// Pages this zone is responsible for, by class: extents homed here, plus those
    /// being pulled in, so a migration cannot overflow its destination. Volumes homed
    /// in another site are excluded — their pages are routed away, never stored.
    ///
    /// The count is a whole zone's, not this node's: a slot table is per zone, and no
    /// node sees another zone's. Balance between zones is a separate question, settled
    /// by where the control plane homes an extent.
    fn zone_pages(&self, huge: bool) -> u64 {
        self.volumes
            .iter()
            .filter(|v| v.huge == huge && v.site() == self.node.site)
            .flat_map(|v| &v.extents)
            .filter(|e| e.zone == self.node.zone || e.next_zone == self.node.zone)
            .map(|e| e.pages)
            .sum()
    }

    /// Slots of this zone that hash into a catalog group this node belongs to: its
    /// share of the zone, out of `SLOTS`. Every slot is held by three nodes, so a
    /// zone's shares sum to `3 * SLOTS`.
    ///
    /// This is the whole of the capacity model. A node is given less of the zone by
    /// being put in fewer groups, which leaves `group_slots` alone and so leaves both
    /// the allocator's core mapping and the digest key alone: a group never moves, only
    /// its membership does.
    pub(crate) fn share_slots(&self) -> u32 {
        let t = &self.topology;
        let mine: Vec<bool> = t
            .catalog
            .iter()
            .map(|g| g.contains(&self.node.id))
            .collect();
        t.group_slots
            .iter()
            .filter(|&&g| *mine.get(g as usize).unwrap_or(&false))
            .count() as u32
    }

    /// The consensus group owning `addr`: the slot the address hashes into names it.
    /// The group also picks the core that serves the address (alloc.rs).
    pub fn group(&self, addr: u64) -> u32 {
        self.topology.group_slots[self.slot_of(addr) as usize]
    }

    fn slot_of(&self, addr: u64) -> u16 {
        slot_of(addr)
    }

    /// Whether `zone` is one this node may place pages in: our own, or another zone of
    /// this site. Placement is intra-site.
    fn known_zone(&self, zone: u32) -> bool {
        zone == self.node.zone || self.topology.zones.iter().any(|z| z.id == zone)
    }

    /// The zone `addr` is homed in: a lookup in its volume's extent list, not a hash.
    /// `None` for an address no volume covers.
    pub(crate) fn zone_of(&self, addr: u64) -> Option<u32> {
        let v = self.volume((addr >> 32) as u32)?;
        Some(v.extent_at(addr & 0xffff_ffff)?.zone)
    }

    /// The zone `addr`'s extent is being pulled to; `None` when it is not, so this is
    /// also the test for "is this page moving".
    pub(crate) fn next_zone_of(&self, addr: u64) -> Option<u32> {
        let v = self.volume((addr >> 32) as u32)?;
        let z = v.extent_at(addr & 0xffff_ffff)?.next_zone;
        (z != 0).then_some(z)
    }

    /// The node to send `addr` to when it is homed in another zone of this site: one
    /// of that zone's three entry nodes, picked by the address so cross-zone load
    /// spreads over all three cohorts. The entry node holds that zone's slot table and
    /// catalog and resolves the group itself, which keeps this node's configuration
    /// `O(zones)` rather than `O(cluster)`.
    pub(crate) fn entry_of(&self, zone: u32, addr: u64) -> Option<u32> {
        let z = self.topology.zones.iter().find(|z| z.id == zone)?;
        Some(z.entry[mix(addr) as usize % z.entry.len()])
    }

    /// The site `addr` is homed in. Not a lookup: the site is the top 8 bits of the
    /// volume id, so every node resolves it without holding any remote state.
    pub(crate) fn site_of(&self, addr: u64) -> Option<u32> {
        Some(self.volume((addr >> 32) as u32)?.site())
    }

    /// The tombstone epoch governing `addr`, taken from its volume. 0 for an address no
    /// volume covers, which is the same as a volume that has never collected: callers
    /// on a write path reject an unmapped address before they ever get here.
    pub(crate) fn tombstone_epoch_of(&self, addr: u64) -> u64 {
        self.volume((addr >> 32) as u32)
            .map_or(0, |v| v.tombstone_epoch)
    }

    /// Whether any volume has ever collected. The sweep is a no-op until one has.
    pub(crate) fn collecting(&self) -> bool {
        self.volumes.iter().any(|v| v.tombstone_epoch != 0)
    }

    /// Our own link into `site`, if we hold one — the crossing itself. Two crossings
    /// into one site are legal; this takes the first in config order.
    pub(crate) fn crossing_to(&self, site: u32) -> Option<u32> {
        self.node
            .peers
            .iter()
            .find(|p| p.site == Some(site))
            .map(|p| p.id)
    }

    /// A peer that will carry a page homed in `site` for us. Hashed over the peers named
    /// gateway for it, so cross-site load spreads. `None` if none were.
    ///
    /// Each node hashes over its own peer list, so two nodes need not pick the same
    /// gateway for the same page. Nothing reads that agreement — a gateway holds no
    /// per-page state — and the spread is what the hash is for.
    pub(crate) fn gateway_to(&self, site: u32, addr: u64) -> Option<u32> {
        let named = |p: &&Peer| p.gateway_for.contains(&site);
        let n = self.node.peers.iter().filter(named).count();
        if n == 0 {
            return None;
        }
        self.node
            .peers
            .iter()
            .filter(named)
            .nth(mix(addr) as usize % n)
            .map(|p| p.id)
    }

    // ---------------------------------------------------------------- validation

    /// The rules a config can be checked against on its own, without reference to what
    /// this node is already running.
    pub fn validate(&self) -> io::Result<()> {
        self.check_node()?;
        self.check_topology()?;
        self.check_volumes()?;
        // The share is what the device was sized for, so a config that hands this node
        // more of the zone than it declared room for is refused outright: taking it
        // would mean joining groups whose pages there are no slots to hold.
        let share = self.share_slots();
        if share > self.node.max_share_slots {
            return Err(bad(format!(
                "share of {share} slots is over max_share_slots {}",
                self.node.max_share_slots
            )));
        }
        // Refuse a working set we cannot index, rather than OOM. This and the volume
        // cap are the only checks that can fail on an internally consistent config, so
        // both name the limit they exceeded.
        let index_bytes = self.small_pages() * crate::alloc::INDEX_BYTES_PER_PAGE;
        if index_bytes > self.policy.max_index_bytes {
            return Err(bad(format!(
                "4 KiB working set needs {index_bytes} B of index, over max_index_bytes {}",
                self.policy.max_index_bytes
            )));
        }
        // The cache's demand counter saturates at `CACHE_MAX_RATE` per interval, so a
        // larger target is one the node could never meet. Refused rather than clamped:
        // a control plane that asked for it should hear so.
        if self.policy.cache_target_rate > CACHE_MAX_RATE {
            return Err(bad(format!(
                "cache_target_rate {} is above {CACHE_MAX_RATE}",
                self.policy.cache_target_rate
            )));
        }
        // A replay that pulls nothing per sweep never ends, and a group stuck replaying
        // is a group that neither accepts nor counts toward quorum.
        if self.policy.repairs_per_replay == 0 {
            return Err(bad("repairs_per_replay is zero"));
        }
        Ok(())
    }

    fn check_node(&self) -> io::Result<()> {
        if self.node.id == 0 {
            return Err(bad("node.id is required"));
        }
        if self.node.device.is_empty() {
            return Err(bad("node.device is required"));
        }
        // A volume's site is the top 8 bits of its id, so a wider site names no volume
        // and would make every volume here look foreign.
        if self.node.site > 255 {
            return Err(bad(format!("node.site {} is not 0..255", self.node.site)));
        }
        // Zero is how `next_zone` says "not migrating", so no zone may be named 0.
        if self.node.zone == 0 {
            return Err(bad("node.zone is required (0 means 'no zone')"));
        }
        // A link to ourselves, or twice to the same node, is one the fabric cannot make
        // sense of.
        let mut peers: Vec<u32> = self.node.peers.iter().map(|p| p.id).collect();
        peers.sort_unstable();
        if peers.windows(2).any(|w| w[0] == w[1]) {
            return Err(bad("duplicate peer id"));
        }
        for p in &self.node.peers {
            if p.id == 0 || p.id == self.node.id {
                return Err(bad(format!("bad peer id {}", p.id)));
            }
            if p.device.is_empty() {
                return Err(bad(format!("peer {} has no device", p.id)));
            }
            // A crossing to our own site names a link we would never take, and a peer
            // offered as our way into our own site is the same mistake said differently.
            if p.site == Some(self.node.site) {
                return Err(bad(format!("peer {} names this node's own site", p.id)));
            }
            if p.site.is_some_and(|s| s > 255) {
                return Err(bad(format!("peer {} site is not 0..255", p.id)));
            }
            for &s in &p.gateway_for {
                if s == self.node.site {
                    return Err(bad(format!(
                        "peer {} is gateway for this node's own site",
                        p.id
                    )));
                }
                if s > 255 {
                    return Err(bad(format!("peer {} gateway_for {s} is not 0..255", p.id)));
                }
            }
        }
        Ok(())
    }

    fn check_topology(&self) -> io::Result<()> {
        let t = &self.topology;
        if t.catalog.is_empty() {
            return Err(bad("topology catalog is empty"));
        }
        for (i, g) in t.catalog.iter().enumerate() {
            // Quorum is 2 of 3, so a group is exactly three distinct nodes.
            if g[0] == g[1] || g[1] == g[2] || g[0] == g[2] {
                return Err(bad(format!("group {i} members are not distinct")));
            }
            if g.contains(&0) {
                return Err(bad(format!("group {i} names node 0")));
            }
        }
        if t.group_slots.len() != SLOTS {
            return Err(bad(format!(
                "group_slots has {} entries, not {SLOTS}",
                t.group_slots.len()
            )));
        }
        if let Some(&s) = t
            .group_slots
            .iter()
            .find(|&&s| s as usize >= t.catalog.len())
        {
            return Err(bad(format!("group slot {s} is not in the catalog")));
        }
        for z in &t.zones {
            if z.id == 0 {
                return Err(bad(
                    "zone 0 is reserved (next_zone 0 means 'not migrating')",
                ));
            }
            if z.id == self.node.zone {
                return Err(bad(format!("zone {} is this node's own zone", z.id)));
            }
        }
        let mut ids: Vec<u32> = t.zones.iter().map(|z| z.id).collect();
        ids.sort_unstable();
        if ids.windows(2).any(|w| w[0] == w[1]) {
            return Err(bad("duplicate zone id"));
        }
        Ok(())
    }

    fn check_volumes(&self) -> io::Result<()> {
        if self.volumes.is_empty() {
            return Err(bad("no volumes"));
        }
        if self.volumes.len() > MAX_VOLUMES {
            return Err(bad(format!(
                "{} volumes, over the {MAX_VOLUMES} this node can export",
                self.volumes.len()
            )));
        }
        let mut ids: Vec<u32> = self.volumes.iter().map(|v| v.id).collect();
        ids.sort_unstable();
        if ids.windows(2).any(|w| w[0] == w[1]) {
            return Err(bad("duplicate volume id"));
        }
        // Slots name volumes on the wire, so a collision would decode one LBA to two
        // volumes. `volumes` is slot-sorted, so neighbors suffice.
        if self.volumes.windows(2).any(|w| w[0].slot == w[1].slot) {
            return Err(bad("duplicate volume slot"));
        }
        for v in &self.volumes {
            self.check_volume(v)?;
        }
        Ok(())
    }

    fn check_volume(&self, v: &Volume) -> io::Result<()> {
        if v.id == 0 {
            return Err(bad("volume id 0 is reserved (a zero address means free)"));
        }
        // An address is volume:32 | offset:32 with the site in the top 8 bits of the
        // volume id, so a volume's site is its id, not a field. A volume homed elsewhere
        // is legal, but only if some peer reaches that site: our own crossing into it,
        // or a peer named gateway for it.
        let s = v.site();
        let reachable = self.crossing_to(s).is_some()
            || self.node.peers.iter().any(|p| p.gateway_for.contains(&s));
        if s != self.node.site && !reachable {
            return Err(bad(format!(
                "volume {} is homed in site {s} and no peer reaches that site",
                v.id
            )));
        }
        if v.slot as usize >= MAX_VOLUMES {
            return Err(bad(format!(
                "volume {} slot {} is above {}",
                v.id,
                v.slot,
                MAX_VOLUMES - 1
            )));
        }
        if v.extents.is_empty() {
            return Err(bad(format!("volume {} has no extents", v.id)));
        }
        for (i, e) in v.extents.iter().enumerate() {
            if e.pages == 0 {
                return Err(bad(format!("volume {} extent {i} has no pages", v.id)));
            }
            if e.zone == 0 {
                return Err(bad(format!("volume {} extent {i} has no home zone", v.id)));
            }
            if e.next_zone == e.zone {
                return Err(bad(format!(
                    "volume {} extent {i} is migrating to the zone it is already in",
                    v.id
                )));
            }
            if !self.known_zone(e.zone) {
                return Err(bad(format!(
                    "volume {} extent {i} names zone {}, which is not in this site",
                    v.id, e.zone
                )));
            }
            if e.next_zone != 0 && !self.known_zone(e.next_zone) {
                return Err(bad(format!(
                    "volume {} extent {i} migrates to zone {}, which is not in this site",
                    v.id, e.next_zone
                )));
            }
        }
        // The address offset is 32 bits wide; each extent already fits, but their sum
        // need not. The fabric's tighter cap on a huge volume is enforced in server.rs.
        if v.pages() > u32::MAX as u64 {
            return Err(bad(format!("volume {} exceeds 2^32 pages", v.id)));
        }
        Ok(())
    }

    /// The rules that only make sense against the config this one replaces. A failure
    /// rejects the whole file: the node keeps running what it has, and nothing is
    /// partially applied.
    fn validate_against(&self, prev: &Config) -> io::Result<()> {
        if self.generation <= prev.generation {
            return Err(bad(format!(
                "generation {} does not advance on {}",
                self.generation, prev.generation
            )));
        }
        // The slot table is frozen for the life of the zone. A slot names the group that
        // owns its addresses, and that group is also the key the allocator shards on and
        // the key anti-entropy accumulates digests under: repointing one would strand
        // the pages in a core's stripe that no longer claims them and corrupt both
        // groups' digests, and there is no mover between groups to repair it with.
        // Capacity moves by changing who is *in* a group, which leaves all three alone.
        if self.topology.group_slots != prev.topology.group_slots {
            return Err(bad(
                "group slots changed; the slot table is fixed for the zone",
            ));
        }
        // The catalog moves one node at a time, so at most one group may differ; a
        // larger step would put two groups in flux at once.
        //
        // Only between adjacent generations. A node that was down for a rebalance
        // campaign is being handed the state that settled while it was away, not a
        // transient, and holding it to a one-group step would make it reject every
        // config from then on.
        let adjacent = self.generation == prev.generation + 1;
        let changed = self
            .topology
            .catalog
            .iter()
            .zip(&prev.topology.catalog)
            .filter(|(a, b)| a != b)
            .count()
            + self
                .topology
                .catalog
                .len()
                .abs_diff(prev.topology.catalog.len());
        if adjacent && changed > 1 {
            return Err(bad(format!(
                "{changed} catalog groups differ; at most one may change"
            )));
        }
        for old in &prev.volumes {
            let Some(new) = self.volume(old.id) else {
                continue;
            };
            self.check_replacement(old, new)?;
        }
        Ok(())
    }

    /// An existing volume's address space is frozen; only where its bytes live moves.
    fn check_replacement(&self, old: &Volume, new: &Volume) -> io::Result<()> {
        // Peers already have the slot on the wire; moving one would repoint every frame
        // in flight for it, so a volume keeps its slot for life. A deleted volume frees
        // its slot without renumbering any survivor.
        if old.slot != new.slot {
            return Err(bad(format!("volume {} changed slot", old.id)));
        }
        // Page size is baked into every entry the volume already has on the device.
        // Implied by the frozen extent kinds below, but checked first so the diagnostic
        // names what actually changed.
        if old.huge != new.huge {
            return Err(bad(format!("volume {} changed page size", old.id)));
        }
        // Page versions in this volume are `3*epoch + state`. A decrease would put the
        // fill point below entries the volume already holds, so every later write to
        // them would conflict forever. Any forward step is fine, including a jump:
        // nothing in the dataplane observes the size of one, so a node that missed
        // several generations adopts the current value instead of refusing the file.
        if new.tombstone_epoch < old.tombstone_epoch {
            return Err(bad(format!(
                "volume {} tombstone_epoch {} is below {}",
                old.id, new.tombstone_epoch, old.tombstone_epoch
            )));
        }
        if old.extents.len() != new.extents.len() {
            return Err(bad(format!(
                "volume {} has {} extents, was {}",
                old.id,
                new.extents.len(),
                old.extents.len()
            )));
        }
        for (i, (a, b)) in old.extents.iter().zip(&new.extents).enumerate() {
            if a.pages != b.pages || a.kind != b.kind {
                return Err(bad(format!("volume {} extent {i} changed shape", old.id)));
            }
            // A home zone moves only to the target a migration was already running
            // towards. This cannot verify the migration finished — the node may own
            // none of the extent's shards — only that the bookkeeping is possible.
            if a.zone != b.zone && b.zone != a.next_zone {
                return Err(bad(format!(
                    "volume {} extent {i} moved from zone {} to {} with no migration to it",
                    old.id, a.zone, b.zone
                )));
            }
        }
        Ok(())
    }

    // -------------------------------------------------------------- conversion

    fn from_pb(p: pb::NodeConfig) -> io::Result<Config> {
        let n = p.node.unwrap_or_default();
        let t = p.topology.unwrap_or_default();
        let catalog = t.catalog.iter().map(trio).collect();
        let mut volumes = Vec::with_capacity(p.volumes.len());
        for v in p.volumes {
            let id = v.id;
            let slot = u8::try_from(v.slot)
                .map_err(|_| bad(format!("volume {id} slot {} is out of range", v.slot)))?;
            let mut extents = Vec::with_capacity(v.extents.len());
            // The volume's page size is its extents', which must agree: a mixture is
            // not representable below this line, so it is refused here rather than in
            // `validate`.
            let mut huge: Option<bool> = None;
            for e in v.extents {
                let k = pb::Kind::try_from(e.kind)
                    .map_err(|_| bad(format!("volume {id}: unknown extent kind {}", e.kind)))?;
                let (kind, wide) = split_kind(k);
                if *huge.get_or_insert(wide) != wide {
                    return Err(bad(format!("volume {id} mixes 4 KiB and 4 MiB extents")));
                }
                extents.push(Extent {
                    pages: e.pages as u64,
                    kind,
                    zone: e.zone,
                    next_zone: e.next_zone,
                });
            }
            let huge = huge.unwrap_or(false);
            volumes.push(Volume::new(
                id,
                slot,
                huge,
                extents,
                v.tombstone_epoch as u64,
            ));
        }
        volumes.sort_by_key(|v| v.slot);

        let peers: Vec<Peer> = n
            .peers
            .iter()
            .map(|p| Peer {
                id: p.id,
                device: p.device.clone(),
                site: p.site,
                gateway_for: p.gateway_for.clone(),
            })
            .collect();
        // Absence is refused rather than defaulted, so a control plane that forgets the
        // field cannot silently ship cohort 0.
        let cohort = n.cohort.ok_or_else(|| bad("node names no cohort"))?;
        let cohort = pb::Cohort::try_from(cohort)
            .map_err(|_| bad(format!("unknown cohort {cohort}")))? as u8;
        let dev = n.device.unwrap_or_default();
        let policy = p.policy.map_or_else(Policy::default, |q| Policy {
            max_index_bytes: q
                .max_index_bytes
                .unwrap_or(Policy::default().max_index_bytes),
            occ_bytes: q.occ_bytes.unwrap_or(Policy::default().occ_bytes),
            cache_target_rate: q.cache_target_rate,
            repairs_per_replay: q
                .repairs_per_replay
                .unwrap_or(Policy::default().repairs_per_replay),
        });
        Ok(Config {
            generation: p.generation,
            node: Node {
                id: n.id,
                site: n.site,
                zone: n.zone,
                cohort,
                device: dev.path,
                cache_bytes_4k: dev.cache_bytes_4k,
                cache_bytes_4m: dev.cache_bytes_4m,
                device_max_iops: dev.max_iops,
                device_max_bytes_per_sec: dev.max_bytes_per_sec,
                // Unset is the whole zone: a forgotten field over-sizes the device,
                // where a literal zero would size it for nothing.
                max_share_slots: if dev.max_share_slots == 0 {
                    SLOTS as u32
                } else {
                    dev.max_share_slots
                },
                peers,
            },
            topology: Topology {
                epoch: t.epoch,
                catalog,
                group_slots: t.group_slots,
                zones: t
                    .zones
                    .into_iter()
                    .map(|z| Zone {
                        id: z.id,
                        entry: trio(&z.entry.unwrap_or_default()),
                    })
                    .collect(),
            },
            volumes,
            policy,
        })
    }

    fn to_pb(&self) -> pb::NodeConfig {
        pb::NodeConfig {
            generation: self.generation,
            node: Some(pb::Node {
                id: self.node.id,
                site: self.node.site,
                zone: self.node.zone,
                cohort: Some(self.node.cohort as i32),
                peers: self
                    .node
                    .peers
                    .iter()
                    .map(|p| pb::Peer {
                        id: p.id,
                        device: p.device.clone(),
                        site: p.site,
                        gateway_for: p.gateway_for.clone(),
                    })
                    .collect(),
                device: Some(pb::Device {
                    path: self.node.device.clone(),
                    cache_bytes_4k: self.node.cache_bytes_4k,
                    cache_bytes_4m: self.node.cache_bytes_4m,
                    max_iops: self.node.device_max_iops,
                    max_bytes_per_sec: self.node.device_max_bytes_per_sec,
                    max_share_slots: self.node.max_share_slots,
                }),
            }),
            topology: Some(pb::Topology {
                epoch: self.topology.epoch,
                catalog: self.topology.catalog.iter().map(pb_trio).collect(),
                group_slots: self.topology.group_slots.clone(),
                zones: self
                    .topology
                    .zones
                    .iter()
                    .map(|z| pb::Zone {
                        id: z.id,
                        entry: Some(pb_trio(&z.entry)),
                    })
                    .collect(),
            }),
            volumes: self
                .volumes
                .iter()
                .map(|v| pb::Volume {
                    id: v.id,
                    slot: v.slot as u32,
                    tombstone_epoch: v.tombstone_epoch as u32,
                    extents: v
                        .extents
                        .iter()
                        .map(|e| pb::Extent {
                            pages: e.pages as u32,
                            kind: join_kind(e.kind, v.huge) as i32,
                            zone: e.zone,
                            next_zone: e.next_zone,
                        })
                        .collect(),
                })
                .collect(),
            policy: Some(pb::Policy {
                max_index_bytes: Some(self.policy.max_index_bytes),
                occ_bytes: Some(self.policy.occ_bytes),
                cache_target_rate: self.policy.cache_target_rate,
                repairs_per_replay: Some(self.policy.repairs_per_replay),
            }),
        }
    }

    /// A human-writable rendering of the same schema, for tests. The control plane
    /// ships protobuf, not this.
    ///
    /// `#` starts a comment; blank lines are ignored; leading whitespace is not
    /// significant; a `key a=1 b=2` line sets the fields it names and defaults the rest.
    /// A key the line does not recognize is an error, not a default.
    ///
    /// ```text
    /// generation 7
    /// node id=1 site=0 zone=1 cohort=1 device=/dev/nvme1n1 cache_4k=0 cache_4m=0
    /// peer id=2 device=/dev/nvme2n1
    /// peer id=9 device=/dev/nvme9n1 gateway_for=2
    /// peer id=40 device=/dev/nvme4n1 site=3
    /// topology epoch=3
    /// group 1 2 3
    /// slots round_robin
    /// zone id=2 entry=4,5,6
    /// policy max_index_bytes=8589934592
    /// volume 1 slot=0 tombstone_epoch=0
    ///   extent pages=262144 kind=lww zone=1
    /// ```
    ///
    /// A peer with `site` is our own crossing into that site; a peer with `gateway_for`
    /// is one we hand foreign addresses to. Either is an ordinary peer otherwise.
    pub fn parse(text: &str) -> io::Result<Config> {
        let mut p = pb::NodeConfig::default();
        let mut node = pb::Node::default();
        let mut topo = pb::Topology::default();
        let mut policy = pb::Policy::default();
        let mut slots: Option<Vec<u32>> = None;
        for (n, raw) in text.lines().enumerate() {
            let line = raw.split('#').next().unwrap().trim();
            if line.is_empty() {
                continue;
            }
            let mut it = line.split_whitespace();
            let key = it.next().unwrap();
            let rest: Vec<&str> = it.collect();
            let f = fields(&rest);
            let at = |e: io::Error| bad(format!("line {}: {}", n + 1, e));
            match key {
                "generation" => p.generation = num(&rest, 0).map_err(at)?,
                "node" => {
                    let f = only(
                        &f,
                        &[
                            "id",
                            "site",
                            "zone",
                            "cohort",
                            "device",
                            "cache_4k",
                            "cache_4m",
                            "max_iops",
                            "max_bps",
                            "max_share",
                        ],
                    )
                    .map_err(at)?;
                    node.id = get(f, "id").map_err(at)? as u32;
                    node.site = get_or(f, "site", 0).map_err(at)? as u32;
                    node.zone = get_or(f, "zone", 0).map_err(at)? as u32;
                    node.cohort = Some(get_or(f, "cohort", 0).map_err(at)? as i32);
                    node.device = Some(pb::Device {
                        path: text_field(f, "device").map_err(at)?,
                        cache_bytes_4k: get_or(f, "cache_4k", 0).map_err(at)?,
                        cache_bytes_4m: get_or(f, "cache_4m", 0).map_err(at)?,
                        max_iops: get_or(f, "max_iops", 0).map_err(at)?,
                        max_bytes_per_sec: get_or(f, "max_bps", 0).map_err(at)?,
                        max_share_slots: get_or(f, "max_share", 0).map_err(at)? as u32,
                    });
                }
                "peer" => {
                    let f = only(&f, &["id", "device", "site", "gateway_for"]).map_err(at)?;
                    node.peers.push(pb::Peer {
                        id: get(f, "id").map_err(at)? as u32,
                        device: text_field(f, "device").map_err(at)?,
                        site: opt(f, "site").map_err(at)?.map(|s| s as u32),
                        gateway_for: opt_list(f, "gateway_for").map_err(at)?,
                    });
                }
                "topology" => {
                    let f = only(&f, &["epoch"]).map_err(at)?;
                    topo.epoch = get_or(f, "epoch", 0).map_err(at)? as u32;
                }
                "group" => topo.catalog.push(ids(&rest).and_then(as_trio).map_err(at)?),
                "zone" => {
                    let f = only(&f, &["id", "entry"]).map_err(at)?;
                    topo.zones.push(pb::Zone {
                        id: get(f, "id").map_err(at)? as u32,
                        entry: Some(list(f, "entry").and_then(as_trio).map_err(at)?),
                    });
                }
                "slots" => {
                    slots = Some(if rest.first() == Some(&"round_robin") {
                        Vec::new() // filled below, once the catalog is complete
                    } else {
                        ids(&rest).map_err(at)?
                    });
                }
                "policy" => {
                    let f = only(
                        &f,
                        &[
                            "max_index_bytes",
                            "occ_bytes",
                            "cache_target_rate",
                            "repairs_per_replay",
                        ],
                    )
                    .map_err(at)?;
                    // Absent means the default, which is not the same as zero.
                    policy.max_index_bytes = opt(f, "max_index_bytes").map_err(at)?;
                    policy.occ_bytes = opt(f, "occ_bytes").map_err(at)?;
                    policy.cache_target_rate =
                        get_or(f, "cache_target_rate", 0).map_err(at)? as u32;
                    policy.repairs_per_replay =
                        opt(f, "repairs_per_replay").map_err(at)?.map(|v| v as u32);
                }
                "volume" => {
                    let f = only(&f, &["slot", "tombstone_epoch"]).map_err(at)?;
                    p.volumes.push(pb::Volume {
                        id: num(&rest, 0).map_err(at)? as u32,
                        // Required, never defaulted: a slot derived from position would
                        // differ between two nodes whose volume lists differ, and the
                        // same LBA would then name two different pages.
                        slot: get(f, "slot").map_err(at)? as u32,
                        tombstone_epoch: get_or(f, "tombstone_epoch", 0).map_err(at)? as u32,
                        extents: Vec::new(),
                    });
                }
                "extent" => {
                    let f = only(&f, &["pages", "kind", "zone", "next_zone"]).map_err(at)?;
                    let e = pb::Extent {
                        pages: get(f, "pages").map_err(at)? as u32,
                        kind: named(f, "kind", "", pb::Kind::from_str_name).map_err(at)? as i32,
                        zone: get_or(f, "zone", 0).map_err(at)? as u32,
                        next_zone: get_or(f, "next_zone", 0).map_err(at)? as u32,
                    };
                    p.volumes
                        .last_mut()
                        .ok_or_else(|| bad(format!("line {}: extent before volume", n + 1)))?
                        .extents
                        .push(e);
                }
                other => return Err(bad(format!("line {}: unknown key {other}", n + 1))),
            }
        }
        let n_groups = topo.catalog.len().max(1);
        topo.group_slots = match slots {
            Some(s) if !s.is_empty() => s,
            _ => (0..SLOTS).map(|i| (i % n_groups) as u32).collect(),
        };
        p.node = Some(node);
        p.topology = Some(topo);
        p.policy = Some(policy);
        Config::from_pb(p)
    }
}

fn num(rest: &[&str], i: usize) -> io::Result<u64> {
    rest.get(i)
        .ok_or_else(|| bad("missing value"))?
        .parse()
        .map_err(|_| bad("expected a number"))
}

fn fields<'a>(rest: &[&'a str]) -> Vec<(&'a str, &'a str)> {
    rest.iter().filter_map(|s| s.split_once('=')).collect()
}

/// Reject a field this line has no use for, rather than ignoring it. A typo that
/// silently defaults is the worst failure this format can have: the node runs, and runs
/// on something other than what was written.
fn only<'a, 'b>(
    f: &'b [(&'a str, &'a str)],
    allowed: &[&str],
) -> io::Result<&'b [(&'a str, &'a str)]> {
    match f.iter().find(|(k, _)| !allowed.contains(k)) {
        Some((k, _)) => Err(bad(format!("unknown field {k}"))),
        None => Ok(f),
    }
}

fn ids(rest: &[&str]) -> io::Result<Vec<u32>> {
    rest.iter()
        .map(|s| s.parse::<u32>().map_err(|_| bad("expected a node id")))
        .collect()
}

fn as_trio(v: Vec<u32>) -> io::Result<pb::Trio> {
    let a: [u32; 3] = v
        .as_slice()
        .try_into()
        .map_err(|_| bad(format!("expected 3 node ids, got {}", v.len())))?;
    Ok(pb_trio(&a))
}

fn text_field(f: &[(&str, &str)], k: &str) -> io::Result<String> {
    f.iter()
        .find(|(a, _)| *a == k)
        .map(|(_, v)| v.to_string())
        .ok_or_else(|| bad(format!("missing field {k}")))
}

fn get(f: &[(&str, &str)], k: &str) -> io::Result<u64> {
    text_field(f, k)?
        .parse()
        .map_err(|_| bad(format!("field {k} is not a number")))
}

fn get_or(f: &[(&str, &str)], k: &str, d: u64) -> io::Result<u64> {
    Ok(opt(f, k)?.unwrap_or(d))
}

fn opt(f: &[(&str, &str)], k: &str) -> io::Result<Option<u64>> {
    match text_field(f, k) {
        Ok(_) => get(f, k).map(Some),
        Err(_) => Ok(None),
    }
}

/// A comma-separated id list, empty when the field is absent. A present-but-malformed
/// list is still an error, which is what separates this from a plain default.
fn opt_list(f: &[(&str, &str)], k: &str) -> io::Result<Vec<u32>> {
    match text_field(f, k) {
        Ok(_) => list(f, k),
        Err(_) => Ok(Vec::new()),
    }
}

/// An enum by name, case-insensitively; `from` is the lookup `prost` generated from
/// `config.proto`, so the two can never drift.
fn named<T>(
    f: &[(&str, &str)],
    k: &str,
    default: &str,
    from: fn(&str) -> Option<T>,
) -> io::Result<T> {
    let v = text_field(f, k).unwrap_or_else(|_| default.to_string());
    from(&v.to_uppercase()).ok_or_else(|| bad(format!("field {k}: unknown value {v:?}")))
}

fn list(f: &[(&str, &str)], k: &str) -> io::Result<Vec<u32>> {
    text_field(f, k)?
        .split(',')
        .map(|s| {
            s.parse::<u32>()
                .map_err(|_| bad(format!("field {k} is not a node id list")))
        })
        .collect()
}

/// The group slot an address hashes into. A pure function of the address, so a slot is
/// a name two zones agree on without either holding the other's slot table.
fn slot_of(addr: u64) -> u16 {
    (mix(addr) % SLOTS as u64) as u16
}

/// Three node ids in cohort order. Named fields on the wire, so "not three" is not a
/// state the model has to consider.
fn trio(t: &pb::Trio) -> [u32; 3] {
    [t.cohort_0, t.cohort_1, t.cohort_2]
}

fn pb_trio(t: &[u32; 3]) -> pb::Trio {
    pb::Trio {
        cohort_0: t[0],
        cohort_1: t[1],
        cohort_2: t[2],
    }
}

/// A cheap avalanche so that adjacent addresses land in unrelated slots. Any fixed
/// permutation works, but every node must agree and cache.rs and heal.rs derive from
/// it too, so it may never change.
pub(crate) fn mix(mut x: u64) -> u64 {
    x ^= x >> 33;
    x = x.wrapping_mul(0xff51_afd7_ed55_8ccd);
    x ^= x >> 33;
    x = x.wrapping_mul(0xc4ce_b9fe_1a85_ec53);
    x ^ (x >> 33)
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

/// An inotify watch on the *directory* holding the config file.
///
/// The directory, not the file, because delivery is a `rename(2)` over the path: a
/// watch on the file would hold the old inode and go deaf after the first push.
/// `IN_MOVED_TO` is the rename landing, `IN_CLOSE_WRITE` an operator editing in place.
struct Watch {
    fd: libc::c_int,
    name: Vec<u8>,
}

impl Watch {
    fn new(path: &Path) -> io::Result<Watch> {
        let dir = path
            .parent()
            .filter(|d| !d.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let name = path
            .file_name()
            .ok_or_else(|| bad("config path has no file name"))?
            .as_bytes()
            .to_vec();
        let fd = unsafe { libc::inotify_init1(libc::IN_CLOEXEC) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let w = Watch { fd, name };
        let c = CString::new(dir.as_os_str().as_bytes())?;
        let mask = libc::IN_MOVED_TO | libc::IN_CLOSE_WRITE;
        if unsafe { libc::inotify_add_watch(fd, c.as_ptr(), mask) } < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(w)
    }

    /// Consume whatever is already queued, without blocking.
    fn drain(&self) -> io::Result<()> {
        let mut buf = [0u64; 512];
        loop {
            let mut p = libc::pollfd {
                fd: self.fd,
                events: libc::POLLIN,
                revents: 0,
            };
            let ready = unsafe { libc::poll(&mut p, 1, 0) };
            let n = match ready {
                0 => return Ok(()),
                r if r > 0 => unsafe {
                    libc::read(
                        self.fd,
                        buf.as_mut_ptr().cast(),
                        std::mem::size_of_val(&buf),
                    )
                },
                _ => -1,
            };
            if n < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(e);
            }
        }
    }

    /// Block until the watched file may have changed. Events for other names in the
    /// directory are consumed and ignored.
    fn wait(&self) -> io::Result<()> {
        // u64-aligned so the event headers can be read in place.
        let mut buf = [0u64; 512];
        loop {
            let n = unsafe {
                libc::read(
                    self.fd,
                    buf.as_mut_ptr().cast(),
                    std::mem::size_of_val(&buf),
                )
            };
            if n < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(e);
            }
            let base = buf.as_ptr().cast::<u8>();
            let mut off = 0usize;
            let hdr = std::mem::size_of::<libc::inotify_event>();
            let mut hit = false;
            while off + hdr <= n as usize {
                let ev = unsafe { std::ptr::read(base.add(off).cast::<libc::inotify_event>()) };
                let name =
                    unsafe { std::slice::from_raw_parts(base.add(off + hdr), ev.len as usize) };
                // The name is NUL-padded to an alignment boundary.
                let name = &name[..name.iter().position(|&b| b == 0).unwrap_or(name.len())];
                hit |= name == self.name;
                off += hdr + ev.len as usize;
            }
            // The whole read is consumed before returning: an event left in the buffer
            // would never be reported again, and may be the only notice of a write that
            // lands after the caller has already reloaded the file.
            if hit {
                return Ok(());
            }
        }
    }
}

impl Drop for Watch {
    fn drop(&mut self) {
        unsafe { libc::close(self.fd) };
    }
}

/// Watch `path` and hand every accepted configuration to `apply`. Never returns while
/// the watch is healthy.
///
/// A config that fails validation is rejected wholesale and counted; the node keeps
/// running the one it has. `apply` failing is the same case — the runtime has already
/// rolled its own build back — so `current` only advances once the new config is live.
pub fn watch(
    path: &Path,
    mut current: Config,
    mut apply: impl FnMut(Config) -> io::Result<()>,
) -> io::Result<()> {
    let w = Watch::new(path)?;
    // inotify reports nothing that happened before the watch existed, and the caller
    // loaded `current` before this thread ran: a config published in that window would
    // be lost until someone published another one. So read the file here instead of
    // waiting for it. Draining before every read is what keeps the two in step — an
    // event dropped here can only announce a file this read is about to see, and a
    // publication this read misses is still queued for the loop below. Finding the
    // config already running is the ordinary case and is not a refusal.
    loop {
        w.drain()?;
        let Ok(next) = Config::load(path) else { break };
        if next.generation <= current.generation {
            break;
        }
        if let Err(e) = next
            .validate()
            .and_then(|()| next.validate_against(&current))
        {
            reject(path, e);
            break;
        }
        match apply(next.clone()) {
            Ok(()) => current = next,
            Err(e) => {
                reject(path, e);
                break;
            }
        }
    }
    loop {
        w.wait()?;
        let next = match Config::load(path) {
            Ok(c) => c,
            Err(e) => {
                reject(path, e);
                continue;
            }
        };
        if let Err(e) = next
            .validate()
            .and_then(|()| next.validate_against(&current))
        {
            reject(path, e);
            continue;
        }
        match apply(next.clone()) {
            Ok(()) => current = next,
            Err(e) => reject(path, e),
        }
    }
}

fn reject(path: &Path, e: io::Error) {
    REJECTED.fetch_add(1, Ordering::Relaxed);
    eprintln!("racer: rejected {}: {e}", path.display());
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = "
        generation 7
        node id=1 zone=1 device=/dev/nvme1n1 cache_4k=1048576
        peer id=2 device=/dev/nvme2n1
        group 1 2 3
        group 4 5 6
        slots round_robin
        zone id=2 entry=4,5,6
        volume 1 slot=0
          extent pages=100 kind=lww zone=1
          extent pages=50 kind=occ zone=1
        volume 2 slot=1
          extent pages=8 kind=immutable_4m zone=1
    ";

    fn sample() -> Config {
        let c = Config::parse(SAMPLE).unwrap();
        c.validate().unwrap();
        c
    }

    #[test]
    fn parses_and_validates() {
        let c = sample();
        assert_eq!(c.generation, 7);
        assert_eq!(c.node.device, "/dev/nvme1n1");
        assert_eq!(c.node.cohort, 0);
        assert_eq!(c.topology.group_slots.len(), SLOTS);
        assert_eq!(c.topology.zones[0].entry, [4, 5, 6]);
        assert_eq!(c.small_pages(), 150);
        assert_eq!(c.huge_pages(), 8);

        let v = c.volume(1).unwrap();
        assert_eq!(v.pages(), 150);
        assert!(!v.huge);
        assert_eq!(v.extent_at(0).unwrap().kind, Kind::Lww);
        assert_eq!(v.extent_at(99).unwrap().kind, Kind::Lww);
        assert_eq!(v.extent_at(100).unwrap().kind, Kind::Occ);
        assert_eq!(v.extent_at(149).unwrap().kind, Kind::Occ);
        assert!(v.extent_at(150).is_none());
        assert_eq!(c.volume(2).unwrap().bytes(), 8 * HUGE_PAGE);

        // Slots are what the fabric puts on the wire, so volumes must resolve by slot
        // and two volumes claiming one must be rejected.
        assert_eq!(c.volume_at(0).unwrap().id, 1);
        assert_eq!(c.volume_at(1).unwrap().id, 2);
        assert!(c.volume_at(2).is_none());
        let mut dup = c.clone();
        dup.volumes[1].slot = 0;
        assert!(dup.validate().is_err());
    }

    /// The page size rides on the extent kind, so a volume that names both spellings of
    /// Immutable has no page size at all. The model cannot hold the mixture, so it is
    /// refused on the way in rather than in `validate`.
    #[test]
    fn a_volume_is_all_one_page_size() {
        let mixed = "node id=1 zone=1 device=/dev/x
             group 1 2 3
             volume 1 slot=0
               extent pages=8 kind=immutable_4m zone=1
               extent pages=8 kind=immutable zone=1";
        assert!(Config::parse(mixed).is_err());
    }

    /// An address resolves to a zone by a lookup in its volume's extent list, not by a
    /// hash, and a zone to one of three entry nodes by the address, so cross-zone load
    /// spreads over all three cohorts.
    #[test]
    fn addresses_resolve_to_a_zone_and_an_entry_node() {
        let mut c = sample();
        c.volumes[0].extents[1].zone = 2;
        c.validate().unwrap();

        let at = |page: u64| (1u64 << 32) | page;
        assert_eq!(c.zone_of(at(0)), Some(1));
        assert_eq!(c.zone_of(at(99)), Some(1));
        assert_eq!(c.zone_of(at(100)), Some(2));
        assert_eq!(c.zone_of(at(149)), Some(2));
        // Past the end of the volume, and a volume that does not exist.
        assert_eq!(c.zone_of(at(150)), None);
        assert_eq!(c.zone_of(9u64 << 32), None);

        // Every entry node is reachable, and the choice is a pure function of the
        // address, so both ends of a link agree on it without saying so.
        let picked: Vec<u32> = (100..150).map(|p| c.entry_of(2, at(p)).unwrap()).collect();
        assert!(picked.iter().all(|n| [4, 5, 6].contains(n)));
        assert!([4, 5, 6].iter().all(|n| picked.contains(n)));
        assert_eq!(c.entry_of(2, at(100)), c.entry_of(2, at(100)));
        // Our own zone has no entry list: we resolve it ourselves.
        assert_eq!(c.entry_of(1, at(0)), None);
    }

    /// A site is the top bits of the volume id, so every node resolves one without
    /// holding anything of the far site. Reaching it takes a peer: our own crossing into
    /// it, or one named gateway for it. Nothing about the node itself is involved.
    #[test]
    fn addresses_resolve_to_a_site_through_a_peer() {
        let c = Config::parse(&format!(
            "
            generation 1
            node id=1 site=1 zone=1 device=/dev/nvme1n1
            peer id=2 device=/dev/nvme2n1
            peer id=9 device=/dev/nvme9n1 gateway_for=2
            peer id=10 device=/dev/nvme10n1 gateway_for=2
            group 2 3 4
            slots round_robin
            volume {} slot=0
              extent pages=64 kind=lww zone=1
            ",
            (1u32 << 24) | 5
        ))
        .unwrap();
        c.validate().unwrap();

        let home = ((1u64 << 24 | 5) << 32) | 7;
        let far = ((2u64 << 24 | 5) << 32) | 7;
        assert_eq!(c.site_of(home), Some(1), "the site is the id, not a lookup");
        assert_eq!(
            c.site_of(far),
            None,
            "a volume we do not carry has no site here"
        );

        // Cross-site load shards over the peers named for that site, deterministically.
        let picked: Vec<u32> = (0..64)
            .map(|p| c.gateway_to(2, home | p).unwrap())
            .collect();
        assert!([9, 10].iter().all(|g| picked.contains(g)));
        assert_eq!(c.gateway_to(2, home), c.gateway_to(2, home));
        assert_eq!(c.gateway_to(3, home), None, "no peer carries site 3");

        // A volume homed elsewhere is legal, but only with a peer that reaches it.
        let mut away = c.clone();
        away.volumes[0].id = (2 << 24) | 5;
        away.validate()
            .expect("a foreign volume routes through a peer named for its site");
        for p in &mut away.node.peers {
            p.gateway_for.clear();
        }
        assert!(away.validate().is_err(), "and is unreachable without one");

        // Any node may hold a crossing; it must name another site, and so must a
        // gateway_for. Either naming our own is a link we would never take.
        let wan = Config::parse(&format!(
            "
            generation 1
            node id=9 site=1 zone=1 device=/dev/nvme1n1
            peer id=40 device=/dev/nvme9n1 site=2
            group 2 3 4
            slots round_robin
            volume {} slot=0
              extent pages=64 kind=lww zone=1
            ",
            (1u32 << 24) | 5
        ))
        .unwrap();
        wan.validate().unwrap();
        assert_eq!(wan.crossing_to(2), Some(40));
        assert_eq!(
            wan.crossing_to(1),
            None,
            "our own site is not across anything"
        );
        let mut own = wan.clone();
        own.node.peers[0].site = Some(1);
        assert!(
            own.validate().is_err(),
            "a crossing that lands where it started"
        );
        let mut own = wan.clone();
        own.node.peers[0].gateway_for = vec![1];
        assert!(
            own.validate().is_err(),
            "nor is a peer our way into our own site"
        );
    }

    /// The point of dissolving the role: a node in the catalog may also hold a crossing.
    /// It serves its groups and has a cohort like any other, and routes across itself
    /// rather than handing off — which is the one case that must fund the far site's
    /// hops at origination, since it will never relay and so never refresh.
    #[test]
    fn a_catalog_member_may_hold_a_crossing() {
        let c = Config::parse(&format!(
            "
            generation 1
            node id=1 site=1 zone=1 cohort=2 device=/dev/nvme1n1
            peer id=2 device=/dev/nvme2n1 site=2
            peer id=3 device=/dev/nvme3n1 gateway_for=2
            group 1 4 5
            slots round_robin
            volume {} slot=0
              extent pages=64 kind=lww zone=1
            ",
            (1u32 << 24) | 5
        ))
        .unwrap();
        c.validate()
            .expect("a catalog member holding a crossing is an ordinary node");
        assert_eq!(c.node.cohort, 2, "and has a cohort, so a cache roster too");
        assert!(c.topology.catalog[0].contains(&c.node.id));
        // Our own crossing wins: we are already the hop that leaves the site.
        let addr = ((2u64 << 24 | 5) << 32) | 7;
        assert_eq!(c.crossing_to(2), Some(2));
        assert_eq!(
            c.gateway_to(2, addr),
            Some(3),
            "the hand-off is still there to take"
        );
    }

    /// A migration is named by an extent, and the dataplane must be able to say both
    /// "where is this page going" and "which pages am I handing over" from the config
    /// alone — the first per address, the second as the range a cursor walks.
    #[test]
    fn a_migrating_extent_names_its_destination_and_its_pages() {
        let mut c = sample();
        c.volumes[0].extents[1].next_zone = 2;
        c.validate().unwrap();

        let at = |page: u64| (1u64 << 32) | page;
        assert_eq!(
            c.next_zone_of(at(99)),
            None,
            "an extent staying put is not moving"
        );
        assert_eq!(c.next_zone_of(at(100)), Some(2));
        assert_eq!(c.next_zone_of(at(149)), Some(2));
        assert_eq!(c.next_zone_of(at(150)), None, "past the end of the volume");
        // The page range of the extent, half open, as the cursor's filter wants it.
        assert_eq!(c.volumes[0].extent_range(0), Some((0, 100)));
        assert_eq!(c.volumes[0].extent_range(1), Some((100, 150)));
        assert_eq!(c.volumes[0].extent_range(2), None);
    }

    /// The file the control plane actually ships. Everything the model holds has to
    /// survive a trip through it, or the text form and the wire form disagree.
    #[test]
    fn protobuf_round_trip() {
        let a = sample();
        let b = Config::decode(&a.encode()).unwrap();
        b.validate().unwrap();
        assert_eq!(format!("{:?}", a.node), format!("{:?}", b.node));
        assert_eq!(a.topology.group_slots, b.topology.group_slots);
        assert_eq!(a.topology.catalog, b.topology.catalog);
        assert_eq!(a.volumes[0].extents, b.volumes[0].extents);
        assert_eq!(a.policy.max_index_bytes, b.policy.max_index_bytes);
        assert_eq!(a.policy.occ_bytes, b.policy.occ_bytes);
        assert_eq!(a.generation, b.generation);
        // A truncated file is rejected, not guessed at.
        assert!(Config::decode(&a.encode()[..3]).is_err());
    }

    /// The file is sized to a node's neighborhood, not the cluster.
    #[test]
    fn stays_small() {
        let mut c = sample();
        c.topology.catalog = (0..300)
            .map(|i| [3 * i + 1, 3 * i + 2, 3 * i + 3])
            .collect();
        c.topology.group_slots = (0..SLOTS).map(|i| (i % 300) as u32).collect();
        assert!(c.encode().len() < 100 << 10, "{} B", c.encode().len());
    }

    #[test]
    fn placement_is_intra_site() {
        // Zone 5 is not this node's zone and not a listed zone.
        let c = Config::parse(
            "node id=1 zone=1 device=/dev/x
             group 1 2 3
             volume 1 slot=0
               extent pages=1 kind=lww zone=5",
        )
        .unwrap();
        assert!(c.validate().is_err());
        // A volume whose id does not name this node's site.
        let c = Config::parse(
            "node id=1 zone=1 site=2 device=/dev/x
             group 1 2 3
             volume 1 slot=0
               extent pages=1 kind=lww zone=1",
        )
        .unwrap();
        assert!(c.validate().is_err());
    }

    #[test]
    fn shape_is_frozen_across_generations() {
        let a = sample();
        let mut b = a.clone();
        b.generation = 8;
        b.validate_against(&a).unwrap();
        // Extent length may not change.
        let mut c = b.clone();
        c.volumes[0].extents[0].pages = 101;
        assert!(c.validate_against(&a).is_err());
        // Nor may a slot: peers already have it on the wire.
        let mut c = b.clone();
        c.volumes[0].slot = 7;
        assert!(c.validate_against(&a).is_err());
        // Nor the page size, which is baked into every entry on the device.
        let mut c = b.clone();
        c.volumes[0].huge = true;
        assert!(c.validate_against(&a).is_err());
        // Nor may the generation stand still.
        assert!(a.validate_against(&a).is_err());
    }

    #[test]
    fn placement_moves_only_along_a_migration() {
        let mut a = sample();
        let mut b = a.clone();
        b.generation = 8;
        // Starting a migration only names the destination.
        b.volumes[0].extents[0].next_zone = 2;
        b.validate_against(&a).unwrap();
        b.validate().unwrap();

        // Finishing it moves `zone` to where `next_zone` pointed.
        a = b.clone();
        let mut c = a.clone();
        c.generation = 9;
        c.volumes[0].extents[0].zone = 2;
        c.volumes[0].extents[0].next_zone = 0;
        c.validate_against(&a).unwrap();
        // A zone that no migration was running towards is not a legal transition.
        let mut d = a.clone();
        d.generation = 9;
        d.volumes[0].extents[0].zone = 3;
        assert!(d.validate_against(&a).is_err());
    }

    #[test]
    fn catalog_moves_one_group_at_a_time() {
        let a = sample();
        let mut b = a.clone();
        b.generation = 8;
        b.topology.catalog[0] = [1, 2, 7];
        b.validate_against(&a).unwrap();
        b.topology.catalog[1] = [4, 5, 8];
        assert!(b.validate_against(&a).is_err());

        // Between adjacent generations only. A node that was down for a campaign is
        // being handed a settled state, not a transient, and refusing every file after
        // the gap would strand it for good.
        b.generation = 12;
        b.validate_against(&a).unwrap();

        // The slot table is not one of the things that ever moves. A repointed slot
        // sends its addresses to a different allocator shard and a different digest,
        // with nothing to carry the registers across.
        let mut c = a.clone();
        c.generation = 8;
        c.topology.group_slots[0] = 1 - c.topology.group_slots[0];
        assert!(c.validate_against(&a).is_err());
        c.generation = 99;
        assert!(
            c.validate_against(&a).is_err(),
            "a gap is not a license to rehash"
        );
    }

    /// A node's capacity is the share of its zone's slots that point at a group it is a
    /// member of, and its device is sized from the ceiling that share may ever reach.
    #[test]
    fn capacity_is_a_share_of_the_zones_slots() {
        let mut c = sample();
        // Two groups, flat round robin, and this node is in one of them.
        assert_eq!(c.share_slots(), (SLOTS / 2) as u32);
        // An unset ceiling is the whole zone, so this node provisions for all of it.
        assert_eq!(c.node.max_share_slots, SLOTS as u32);
        assert_eq!(c.small_pages(), 150);

        // Halve the ceiling and it provisions for half.
        c.node.max_share_slots = (SLOTS / 2) as u32;
        c.validate().unwrap();
        assert_eq!(c.small_pages(), 75);
        assert_eq!(c.huge_pages(), 4);

        // A share above the ceiling is refused rather than served until the device
        // fills: the slots were never formatted for.
        c.topology.catalog[1] = [1, 5, 6];
        assert_eq!(c.share_slots(), SLOTS as u32);
        assert!(c.validate().is_err());

        // A group this node is not in contributes nothing to its share.
        let mut d = sample();
        d.topology.catalog[0] = [7, 2, 3];
        assert_eq!(d.share_slots(), 0);
    }

    #[test]
    fn tombstone_epoch_moves_forward_per_volume() {
        let a = sample();
        let mut b = a.clone();
        b.generation = 8;
        // Any forward step, not just one: a node that missed several generations must
        // adopt the current value rather than refuse the file.
        b.volumes[0].tombstone_epoch = 5;
        b.validate_against(&a).unwrap();
        // And one volume's epoch says nothing about another's.
        assert_eq!(b.volumes[1].tombstone_epoch, 0);
        assert_eq!(b.tombstone_epoch_of(1 << 32), 5);
        assert_eq!(b.tombstone_epoch_of(2 << 32), 0);

        let mut c = b.clone();
        c.generation = 9;
        c.volumes[0].tombstone_epoch = 4;
        assert!(
            c.validate_against(&b).is_err(),
            "a decrease strands every live page"
        );
    }

    #[test]
    fn live_swaps_without_disturbing_the_reader() {
        let live = Live::new(sample());
        assert_eq!(live.get().generation, 7);
        let mut next = sample();
        next.generation = 8;
        live.install(next);
        assert_eq!(live.get().generation, 8);
        let mut third = sample();
        third.generation = 9;
        live.install(third);
        assert_eq!(live.get().generation, 9);
    }

    /// Delivery is a rename over the path, so the watch has to survive the inode
    /// changing underneath it.
    #[test]
    fn watch_sees_a_rename() {
        let dir = std::env::temp_dir().join(format!("racer-cfg-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("config.pb");
        std::fs::write(&path, sample().encode()).unwrap();
        let w = Watch::new(&path).unwrap();

        let tmp = dir.join("config.pb.new");
        let mut next = sample();
        next.generation = 8;
        std::fs::write(&tmp, next.encode()).unwrap();
        std::fs::rename(&tmp, &path).unwrap();

        w.wait().unwrap();
        let seen = Config::load(&path).unwrap();
        assert_eq!(seen.generation, 8);
        seen.validate_against(&sample()).unwrap();
        std::fs::remove_dir_all(&dir).unwrap();
    }

    /// The whole of delivery on one path: a good generation is applied, a bad one is
    /// refused without disturbing what is running, and the next good one still lands.
    #[test]
    fn watch_applies_and_refuses() {
        let dir = std::env::temp_dir().join(format!("racer-apply-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("config.pb");
        std::fs::write(&path, sample().encode()).unwrap();

        let (tx, rx) = std::sync::mpsc::channel();
        let p = path.clone();
        let t = std::thread::spawn(move || {
            watch(&p, sample(), move |c| {
                tx.send(c.generation).unwrap();
                Ok(())
            })
        });

        let put = |g: u64, f: &dyn Fn(&mut Config)| {
            let mut c = sample();
            c.generation = g;
            f(&mut c);
            let tmp = dir.join("next");
            std::fs::write(&tmp, c.encode()).unwrap();
            std::fs::rename(&tmp, &path).unwrap();
        };
        // The watch is created on the other thread, so a rename can land before it
        // exists: announce until it answers. And inotify reports that the file changed,
        // not how often, so each step must be seen before the next is made.
        let took = std::time::Duration::from_secs(10);
        let beat = std::time::Duration::from_millis(20);
        let mut applied = None;
        for _ in 0..500 {
            put(8, &|_| {});
            if let Ok(g) = rx.recv_timeout(beat) {
                applied = Some(g);
                break;
            }
        }
        assert_eq!(applied, Some(8));

        // A duplicate announcement is refused, so wait for the count to stop moving
        // before taking the baseline this test measures against.
        let mut before = rejected();
        loop {
            std::thread::sleep(beat);
            let now = rejected();
            if now == before {
                break;
            }
            before = now;
        }
        let refused = |n: u64| {
            let end = std::time::Instant::now() + took;
            while rejected() - before < n && std::time::Instant::now() < end {
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            assert_eq!(rejected() - before, n);
        };
        // Generation 8 again: not an advance, so it never reaches `apply`.
        put(8, &|_| {});
        refused(1);
        // A shape change against the config now running.
        put(9, &|c| c.volumes[0].extents[0].pages = 999);
        refused(2);
        put(10, &|_| {});
        assert_eq!(rx.recv_timeout(took).unwrap(), 10);
        refused(2);

        drop(t);
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
