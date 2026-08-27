//! Read-only API between the Racer dataplane and control plane implementations.

use std::ffi::OsString;
use std::io;
use std::path::{Path, PathBuf};

use prost::Message as _;

use crate::cache::Roster;
use crate::fabric::Link;

mod text;
mod validate;
mod watch;

#[cfg(any(test, doc))]
use watch::Watch;
pub use watch::watch;

pub mod pb {
    include!(concat!(env!("OUT_DIR"), "/racer.config.rs"));
}

pub(crate) use pb::{Extent, Peer, Trio, Universe};

const SLOTS: usize = 16384;
const SMALL_PAGE: u64 = 4096;
const HUGE_PAGE: u64 = 4 << 20;

pub(crate) const CACHE_MAX_ADMIT: u32 = 15;

const MAX_GATEWAYS: usize = 64;
const MAX_WARM_ZONES: usize = 16;

pub fn rejected() -> u64 {
    crate::kernel::counter(crate::kernel::Counter::ConfigRejected)
}

pub const STORE_PATH_ENV: &str = "RACER_STORE";
pub const DEFAULT_STORE_PATH: &str = "/var/lib/racer/store.img";

pub fn store_path() -> PathBuf {
    path_from(std::env::var_os(STORE_PATH_ENV))
}

fn path_from(v: Option<OsString>) -> PathBuf {
    match v {
        Some(s) if !s.is_empty() => PathBuf::from(s),
        _ => PathBuf::from(DEFAULT_STORE_PATH),
    }
}

fn bad(msg: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, msg.into())
}

pub struct Compiled {
    config: Config,
    roster: Roster,
    links: Box<[Link]>,
}

impl Compiled {
    pub(crate) fn with_links(config: Config, links: Vec<Link>) -> Compiled {
        Compiled {
            roster: Roster::of(&config),
            config,
            links: links.into_boxed_slice(),
        }
    }

    pub(crate) fn config(&self) -> &Config {
        &self.config
    }

    pub(crate) fn roster(&self) -> &Roster {
        &self.roster
    }

    pub(crate) fn link(&self, universe: u32, node: u32) -> Option<&Link> {
        self.links
            .iter()
            .find(|l| l.universe() == universe && l.peer() == node)
    }

    pub(crate) fn has_links(&self, universe: u32) -> bool {
        self.links.iter().any(|l| l.universe() == universe)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Mutable,
    Immutable,
}

pub const UNIVERSE_BITS: u32 = 26;
pub const LBA_BITS: u32 = 38;
pub const MAX_LBA: u64 = 1 << LBA_BITS;
pub const MAX_UNIVERSE: u32 = 1 << UNIVERSE_BITS;
pub const HUGE_BLOCKS: u64 = HUGE_PAGE / SMALL_PAGE;
pub(crate) const MAX_EXPORTS: usize = 256;
pub(crate) const MAX_EXTENTS: usize = 1024;

/// The universe a page address belongs to.
pub fn universe_of(addr: u64) -> u32 {
    (addr >> LBA_BITS) as u32
}

/// The 4 KiB block a page address names inside its universe.
pub fn lba_of(addr: u64) -> u64 {
    addr & (MAX_LBA - 1)
}

/// The two halves joined. Out-of-range inputs are masked; every caller is post-validation.
pub fn addr_of(universe: u32, lba: u64) -> u64 {
    ((universe as u64 & (MAX_UNIVERSE as u64 - 1)) << LBA_BITS) | (lba & (MAX_LBA - 1))
}

/// A consensus group: a universe and an index into that universe's catalog. Catalogs are
/// per universe, so the same index means unrelated node sets elsewhere; a bare index may
/// never cross a universe boundary, and frames carrying one name a universe's namespace.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct GroupId(u64);

impl GroupId {
    pub fn new(universe: u32, index: u32) -> GroupId {
        GroupId((universe as u64) << 32 | index as u64)
    }

    pub fn universe(self) -> u32 {
        (self.0 >> 32) as u32
    }

    pub fn index(self) -> u32 {
        self.0 as u32
    }
}

/// A contiguous run of pages at a fixed offset in its universe's address space: the unit
/// of page kind and size, zone affinity, tombstone epoch, sealing, migration, census and
/// device composition. Position is explicit, not list order, so hosts may differ on it.
impl Extent {
    /// The wire kind. Total because `from_pb` refuses an id no version of the schema
    /// spells, so nothing downstream has to carry an "unknown kind" case.
    fn wire_kind(&self) -> pb::Kind {
        pb::Kind::try_from(self.kind).unwrap_or(pb::Kind::Mutable)
    }

    /// What guard a write must present, and with it whether the block is checksummed
    /// and whether it may be cached. Frozen.
    pub(crate) fn guard(&self) -> Kind {
        match self.wire_kind() {
            pb::Kind::Mutable => Kind::Mutable,
            pb::Kind::Immutable => Kind::Immutable,
        }
    }

    /// Length in blocks, which is what the extent reserves in the universe. Both kinds
    /// address 4 KiB blocks, so this is the schema's count unchanged.
    pub(crate) fn blocks(&self) -> u64 {
        self.blocks
    }

    /// One past the last block. Extents in a universe are disjoint over `base..end`.
    pub(crate) fn end_lba(&self) -> u64 {
        self.base_lba + self.blocks()
    }

    fn contains(&self, lba: u64) -> bool {
        lba >= self.base_lba && lba < self.end_lba()
    }
}

/// A catalog group: three node ids in paxos member order, which is also the cohort column.
/// Named fields on the wire, so "not three" cannot occur.
impl Trio {
    pub(crate) fn nodes(&self) -> [u32; 3] {
        [self.cohort_0, self.cohort_1, self.cohort_2]
    }

    /// The node holding cohort `c`, or `None` past the third: there is no fourth cohort.
    pub(crate) fn cohort(&self, c: usize) -> Option<u32> {
        self.nodes().get(c).copied()
    }
}

impl From<[u32; 3]> for Trio {
    fn from(n: [u32; 3]) -> Trio {
        Trio {
            cohort_0: n[0],
            cohort_1: n[1],
            cohort_2: n[2],
        }
    }
}

/// A shared LBA space spanning a set of nodes: an address space, a transport, a consensus
/// domain and a security boundary, all the same object. Each universe has its own fabric
/// namespace, so nothing on the wire names a universe and a node holds a link only where
/// the control plane published one. Epoch, catalog, zones and peers live here, not on the
/// node, so two universes on one node stay independent.
impl Universe {
    /// The extent covering `lba`, if any block of this universe's space is mapped there.
    /// `from_pb` sorted the list by `base_lba` and made it disjoint, so this is a search.
    pub(crate) fn extent_at(&self, lba: u64) -> Option<&Extent> {
        let i = self.extents.partition_point(|e| e.base_lba <= lba);
        let e = self.extents.get(i.checked_sub(1)?)?;
        e.contains(lba).then_some(e)
    }

    /// Every node the catalog names, sorted and deduplicated.
    pub(crate) fn zone_nodes(&self) -> Vec<u32> {
        let mut v: Vec<u32> = self.catalog.iter().flat_map(|g| g.nodes()).collect();
        v.sort_unstable();
        v.dedup();
        v
    }

    /// How many of the catalog's group slots one node holds. This is that node's share of
    /// the zone, and it is not the same for every node: a zone growing, shrinking or
    /// levelling out moves one group at a time.
    pub(crate) fn slots_held(&self, node: u32) -> u64 {
        if node == 0 {
            return 0;
        }
        self.catalog
            .iter()
            .filter(|g| g.nodes().contains(&node))
            .count() as u64
    }

    fn known_zone(&self, zone: u32, ours: u32) -> bool {
        zone == ours || self.zones.iter().any(|z| z.id == zone)
    }

    /// The nodes of `zone` taking traffic from outside. Empty for a zone we were not told
    /// of, which reads the same as having nowhere to send.
    pub(crate) fn gateways_of(&self, zone: u32) -> &[u32] {
        match self.zones.iter().find(|z| z.id == zone) {
            Some(z) => &z.gateways,
            None => &[],
        }
    }

    /// `zone`'s gateways in the order to try them for `addr`: rendezvous on the address,
    /// so addresses spread over the set and a sender with no link to one falls through.
    /// Promotion is safe here but not for the cache's ring (cache.rs): any gateway resolves
    /// any address in its zone, so no second party has to agree.
    pub(crate) fn gateways_for(&self, zone: u32, addr: u64) -> impl Iterator<Item = u32> {
        ranked(self.gateways_of(zone), addr)
    }

    /// The node of cohort `c` a warm copy of `addr` belongs on: the top of this zone's
    /// cohort `c` under the rendezvous ranking the cache uses. The catalog column is the
    /// cohort, so *every* cohort is computable from one config, unlike `cache::Roster`,
    /// which projects only its own column. Hence the two-stage warm push: the source zone
    /// has no catalog for the destination, whose gateway has all three columns.
    pub(crate) fn cohort_winner(&self, addr: u64, c: usize) -> Option<u32> {
        let mut best: Option<(u64, u32)> = None;
        for g in &self.catalog {
            let n = g.cohort(c)?;
            let k = (rank(addr, n), n);
            if best.is_none_or(|b| k > b) {
                best = Some(k);
            }
        }
        best.map(|(_, n)| n)
    }

    /// Blocks of one kind this universe places in `ours`, counting an extent on its way
    /// in as well as one on its way out: both zones hold it while it moves.
    fn zone_blocks(&self, kind: Kind, ours: u32) -> u64 {
        self.extents
            .iter()
            .filter(|e| e.guard() == kind && (e.zone == ours || e.next_zone == ours))
            .map(|e| e.blocks())
            .sum()
    }
}

/// One extent of a local block device, resolved against the config that named it.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Span {
    pub(crate) universe: u32,
    pub(crate) extent: u32,
    base_lba: u64,
    blocks: u64,
}

/// A local ublk block device: an ordered list of whole extents, concatenated. Nothing is
/// shared: hosts may build different devices from the same extents in different orders,
/// mount one twice, or not at all. A device may span universes; each page is still reached
/// over its own universe's fabric.
///
/// Everything counts in 4 KiB blocks, which is both the unit an extent is composed of
/// and the logical block size the device is exported with.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Device {
    pb: pb::Device,
    /// One per entry of `pb.extents`, resolved against the config that named it.
    spans: Vec<Span>,
    /// Prefix sums in blocks, `spans.len() + 1` long.
    starts: Vec<u64>,
}

impl std::ops::Deref for Device {
    type Target = pb::Device;

    fn deref(&self) -> &pb::Device {
        &self.pb
    }
}

impl Device {
    fn new(pb: pb::Device, spans: Vec<Span>) -> io::Result<Device> {
        let id = pb.id;
        let mut starts = Vec::with_capacity(spans.len() + 1);
        let mut at = 0u64;
        starts.push(0);
        for s in &spans {
            at = at
                .checked_add(s.blocks)
                .ok_or_else(|| bad(format!("device {id} has too many blocks to address")))?;
            starts.push(at);
        }
        Ok(Device { pb, spans, starts })
    }

    /// Length in 4 KiB blocks.
    pub(crate) fn blocks(&self) -> u64 {
        self.starts.last().copied().unwrap_or(0)
    }

    pub(crate) fn bytes(&self) -> u64 {
        self.blocks() * SMALL_PAGE
    }

    /// The global address block `lba` of this device lands on, or `None` past the end.
    /// One device block is exactly one addressable block, so a request is cut per block.
    pub(crate) fn map(&self, lba: u64) -> Option<u64> {
        if lba >= self.blocks() {
            return None;
        }
        let i = self.starts.partition_point(|&s| s <= lba) - 1;
        let s = &self.spans[i];
        Some(addr_of(s.universe, s.base_lba + (lba - self.starts[i])))
    }
}

/// This node, as the control plane named it, plus the one fact the file does not carry.
///
/// Derefs to the wire message, so `node.id` and `node.zone` read straight off it. `store`
/// shadows the wire's `store` submessage deliberately: the path is a deployment fact from
/// `RACER_STORE` that `load`/`parse` fill in, and the size and rates the wire does carry
/// are read through the accessors below.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Node {
    pb: pb::Node,
    /// The store file this node owns.
    pub store: PathBuf,
}

impl std::ops::Deref for Node {
    type Target = pb::Node;

    fn deref(&self) -> &pb::Node {
        &self.pb
    }
}

impl Node {
    /// 0..2, our cache cohort (cache.rs): the same catalog column in every universe.
    /// `from_pb` refuses a config that names none, so the default here is never taken.
    pub(crate) fn cohort(&self) -> u8 {
        self.pb.cohort.unwrap_or(0) as u8
    }

    /// The length the store file is held at. Grown to on start, never shrunk.
    pub(crate) fn store_bytes(&self) -> u64 {
        self.pb.store.as_ref().map_or(0, |s| s.size_bytes)
    }

    /// The rate we drive the store at, zero for unmetered. Absent and zero say the same
    /// thing, so the wire spells unmetered by leaving the field out. Read once per IO.
    pub(crate) fn max_iops(&self) -> u64 {
        self.pb
            .store
            .as_ref()
            .map_or(0, |s| s.max_iops.unwrap_or(0))
    }

    pub(crate) fn max_bytes_per_sec(&self) -> u64 {
        self.pb
            .store
            .as_ref()
            .map_or(0, |s| s.max_bytes_per_sec.unwrap_or(0))
    }

    /// Resize the store. For tests and harnesses that size a store to the fixture they
    /// built rather than to the one the control plane published.
    #[cfg(test)]
    pub(crate) fn set_store_bytes(&mut self, bytes: u64) {
        self.pb.store.get_or_insert_default().size_bytes = bytes;
    }
}

/// DRAM ceilings and sweep rates, all optional on the wire. Absent means the default
/// below, applied where it is read rather than stored, so a config round-trips unchanged
/// and adding a knob is one accessor.
impl Config {
    /// DRAM ceiling for the 4 KiB index. A config needing more is refused, not left to OOM.
    pub(crate) fn max_index_bytes(&self) -> u64 {
        self.policy.max_index_bytes.unwrap_or(8 << 30)
    }

    /// DRAM ceiling for the OCC read pool across the whole node. Evicting a read record
    /// can only turn a success into a conflict, so this bounds memory, not correctness.
    pub(crate) fn occ_bytes(&self) -> u64 {
        self.policy.occ_bytes.unwrap_or(256 << 20)
    }

    /// DRAM ceiling for the read cache index across all cores and classes; its media is
    /// whatever the slabs left over. Not an admission check: the cache holds fewer chunks.
    pub(crate) fn cache_index_bytes(&self) -> u64 {
        self.policy.cache_index_bytes.unwrap_or(1 << 30)
    }

    /// Registers one anti-entropy sweep pulls per group replay, and pushes per extent while
    /// handing one over: the rate of member replacement and handover (heal.rs).
    pub(crate) fn repairs_per_replay(&self) -> u32 {
        self.policy.repairs_per_replay.unwrap_or(4096)
    }
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct Config {
    /// Strictly increasing; non-advancing is rejected. Echoed in this node's PING
    /// (server.rs), so a node redirected on a stale epoch can tell if it holds the file it
    /// was told to fetch.
    pub(crate) generation: u64,
    pub node: Node,
    /// Sorted by id, so `universe` is a binary search.
    universes: Vec<Universe>,
    /// Sorted by id.
    devices: Vec<Device>,
    policy: pb::Policy,
    /// `(extent id, universe index, extent index)`, sorted by extent id. Ids are unique
    /// file-wide, so one flat index serves the wire (a SEAL carries a bare id) and devices.
    index: Vec<(u32, u32, u32)>,
}

impl Config {
    /// Read and decode the file the control plane renamed into place. See `validate`.
    pub fn load(path: &Path) -> io::Result<Config> {
        Config::decode(&std::fs::read(path)?)
    }

    /// The wire carries the store's size but not its path, so the path comes from the env.
    fn decode(bytes: &[u8]) -> io::Result<Config> {
        let pb = pb::NodeConfig::decode(bytes).map_err(|e| bad(format!("protobuf: {e}")))?;
        let mut cfg = Config::from_pb(pb)?;
        cfg.node.store = store_path();
        Ok(cfg)
    }

    pub fn encode(&self) -> Vec<u8> {
        self.to_pb().encode_to_vec()
    }

    pub(crate) fn universes(&self) -> &[Universe] {
        &self.universes
    }

    pub(crate) fn universe(&self, id: u32) -> Option<&Universe> {
        let i = self.universes.binary_search_by_key(&id, |u| u.id).ok()?;
        self.universes.get(i)
    }

    /// The universe an address names.
    pub(crate) fn universe_at(&self, addr: u64) -> Option<&Universe> {
        self.universe(universe_of(addr))
    }

    pub(crate) fn devices(&self) -> &[Device] {
        &self.devices
    }

    pub(crate) fn device(&self, id: u32) -> Option<&Device> {
        let i = self.devices.binary_search_by_key(&id, |d| d.id).ok()?;
        self.devices.get(i)
    }

    /// The extent covering `addr`; `None` is ordinary for an address we hold no extent of.
    pub(crate) fn extent_at(&self, addr: u64) -> Option<&Extent> {
        self.universe_at(addr)?.extent_at(lba_of(addr))
    }

    /// An extent by the id the wire uses, with the universe it belongs to.
    pub(crate) fn extent_by_id(&self, id: u32) -> Option<(&Universe, &Extent)> {
        let i = self.index.binary_search_by_key(&id, |&(k, _, _)| k).ok()?;
        let (_, u, e) = self.index[i];
        let u = self.universes.get(u as usize)?;
        Some((u, u.extents.get(e as usize)?))
    }

    /// Every link this node holds, as `(universe, peer)`.
    pub(crate) fn peers(&self) -> impl Iterator<Item = (u32, &Peer)> {
        self.universes
            .iter()
            .flat_map(|u| u.peers.iter().map(move |p| (u.id, p)))
    }

    pub(crate) fn peer_count(&self) -> usize {
        self.universes.iter().map(|u| u.peers.len()).sum()
    }

    pub(crate) fn extent_count(&self) -> usize {
        self.index.len()
    }

    /// Every extent, with the universe it belongs to.
    pub(crate) fn extents(&self) -> impl Iterator<Item = (&Universe, &Extent)> {
        self.universes
            .iter()
            .flat_map(|u| u.extents.iter().map(move |e| (u, e)))
    }

    /// Mutable blocks this node must have room for.
    pub(crate) fn mutable_blocks(&self) -> u64 {
        self.count_blocks(Kind::Mutable)
    }

    /// Immutable blocks this node must have room for.
    pub(crate) fn immutable_blocks(&self) -> u64 {
        self.count_blocks(Kind::Immutable)
    }

    /// This node's share of one kind over all its universes. A zone's blocks are spread
    /// over its catalog's groups and each group is stored three times, so a node's share is
    /// exactly the group slots it holds; shares add up. A node the catalog does not name is
    /// sized as though it held an even share, because it is either about to be named or
    /// draining out and still holding what it has not handed over.
    fn count_blocks(&self, kind: Kind) -> u64 {
        self.universes
            .iter()
            .map(|u| {
                let groups = u.catalog.len() as u64;
                if groups == 0 {
                    return 0;
                }
                let held = u.slots_held(self.node.id);
                let share = if held == 0 {
                    (groups * 3).div_ceil(u.zone_nodes().len().max(1) as u64)
                } else {
                    held
                };
                (u.zone_blocks(kind, self.node.zone) * share).div_ceil(groups)
            })
            .sum()
    }

    /// The consensus group an address belongs to, given the kind of the block that lives
    /// there. An address in a universe we hold no catalog for still yields a well-formed
    /// id; it is `members` that refuses it.
    ///
    /// The kind is a parameter rather than a lookup because a row whose extent has been
    /// taken out of the config still has to be routed to the group that holds it, so that
    /// shedding and anti-entropy can find its peers. The allocator keeps the kind next to
    /// the row for exactly that.
    pub fn group_of(&self, addr: u64, kind: Kind) -> GroupId {
        let n = self.universe_at(addr).map_or(0, |u| u.catalog.len()) as u32;
        let placed = placement_of(addr, kind);
        let index = if n == 0 { 0 } else { slot_of(placed) as u32 % n };
        GroupId::new(universe_of(addr), index)
    }

    /// The group for an address the config still maps. `None` when nothing is mapped
    /// there, because there is then no kind to place it by.
    pub fn group(&self, addr: u64) -> Option<GroupId> {
        self.extent_at(addr).map(|e| self.group_of(addr, e.guard()))
    }

    /// The zone answering for `addr`, `None` when nothing is mapped there.
    pub(crate) fn zone_of(&self, addr: u64) -> Option<u32> {
        self.extent_at(addr).map(|e| e.zone)
    }

    /// The zone taking `addr` over, if its extent is migrating.
    pub(crate) fn next_zone_of(&self, addr: u64) -> Option<u32> {
        self.extent_at(addr)
            .map(|e| e.next_zone)
            .filter(|&z| z != 0)
    }

    /// `zone`'s gateways in `addr`'s universe, in the order to try them for `addr`.
    pub(crate) fn gateways_for(&self, zone: u32, addr: u64) -> impl Iterator<Item = u32> {
        self.universe_at(addr)
            .map(|u| u.gateways_for(zone, addr))
            .into_iter()
            .flatten()
    }

    /// The topology epoch of `addr`'s universe, zero for an address in no universe of ours.
    pub(crate) fn epoch_of(&self, addr: u64) -> u32 {
        self.universe_at(addr).map_or(0, |u| u.epoch)
    }

    /// The tombstone epoch of `addr`'s extent.
    pub(crate) fn tombstone_epoch_of(&self, addr: u64) -> u64 {
        self.extent_at(addr).map_or(0, |e| e.tombstone_epoch as u64)
    }

    /// The cache admission threshold of `addr`'s extent (cache.rs): 0 never admits, 1 on
    /// first sight, `n` once the demand estimate reaches `n`. An address in no extent of
    /// ours is never cached, so an extent leaving this config stops admission at once.
    pub(crate) fn cache_admit_of(&self, addr: u64) -> u8 {
        self.extent_at(addr).map_or(0, |e| e.cache_admit as u8)
    }

    /// The zones `addr`'s extent asks to have warmed as it commits (paxos.rs). Empty for
    /// an unmapped address, and on every node outside the home zone, which alone commits.
    pub(crate) fn warm_zones_of(&self, addr: u64) -> &[u32] {
        self.extent_at(addr).map_or(&[][..], |e| &e.warm_zones)
    }

    /// Whether *our* zone is a warm destination for `addr`: a page of this extent may
    /// already be in this zone's cohort caches, so a cross-zone read looks there first.
    pub(crate) fn warmed_here(&self, addr: u64) -> bool {
        self.warm_zones_of(addr).contains(&self.node.zone)
    }

    /// The id of `addr`'s extent, which is what the census is keyed by.
    pub(crate) fn extent_id_of(&self, addr: u64) -> Option<u32> {
        self.extent_at(addr).map(|e| e.id)
    }

    /// Whether this configuration can retire anything the store still holds; if not, a
    /// sweep is pointless.
    ///
    /// Two things retire a row. An extent that has collected at least once leaves rows
    /// behind its own epoch. And an extent that has gone from the configuration outright,
    /// because its volume was deleted or it finished moving to another zone, leaves rows
    /// no extent covers at all - which is why an empty extent list is not the same as
    /// nothing to do. That reading is only safe on a full publication, and a
    /// configuration that names peers is one: the bootstrap shape carries neither peers
    /// nor extents.
    pub(crate) fn reclaimable(&self) -> bool {
        self.extents().any(|(_, e)| e.tombstone_epoch != 0) || self.peer_count() > 0
    }

    /// The structural checks: what a `Config` cannot represent at all, unlike `validate`.
    /// Everything the wire says is kept verbatim; what is built here is the ordering and
    /// the indexes the query methods above rely on.
    fn from_pb(p: pb::NodeConfig) -> io::Result<Config> {
        let pb_node = p.node.unwrap_or_default();
        if pb_node.cohort.is_none() {
            return Err(bad("node names no cohort"));
        }
        let node = Node {
            pb: pb_node,
            store: PathBuf::new(),
        };
        let mut universes = p.universes;
        for u in &mut universes {
            for e in &u.extents {
                if pb::Kind::try_from(e.kind).is_err() {
                    return Err(bad(format!("extent {} has unknown kind {}", e.id, e.kind)));
                }
                if e.cache_admit > CACHE_MAX_ADMIT {
                    return Err(bad(format!(
                        "extent {} asks for cache_admit {}, above the {CACHE_MAX_ADMIT} the cache can observe",
                        e.id, e.cache_admit
                    )));
                }
            }
            u.extents.sort_by_key(|e| e.base_lba);
        }
        universes.sort_by_key(|u| u.id);
        // The flat extent index, and the uniqueness the wire cannot express: one id names
        // one extent in one universe, everywhere on this node.
        let mut index: Vec<(u32, u32, u32)> = universes
            .iter()
            .enumerate()
            .flat_map(|(ui, u)| {
                u.extents
                    .iter()
                    .enumerate()
                    .map(move |(ei, e)| (e.id, ui as u32, ei as u32))
            })
            .collect();
        index.sort_unstable();
        if let Some(w) = index.windows(2).find(|w| w[0].0 == w[1].0) {
            return Err(bad(format!("extent id {} is used twice", w[0].0)));
        }
        let find = |id: u32| -> Option<(&Universe, &Extent)> {
            let i = index.binary_search_by_key(&id, |&(k, _, _)| k).ok()?;
            let (_, ui, ei) = index[i];
            let u = universes.get(ui as usize)?;
            Some((u, u.extents.get(ei as usize)?))
        };
        let mut devices = Vec::with_capacity(p.devices.len());
        for d in p.devices {
            let mut spans = Vec::with_capacity(d.extents.len());
            for &id in &d.extents {
                let (u, e) = find(id)
                    .ok_or_else(|| bad(format!("device {} maps unknown extent {id}", d.id)))?;
                spans.push(Span {
                    universe: u.id,
                    extent: e.id,
                    base_lba: e.base_lba,
                    blocks: e.blocks(),
                });
            }
            devices.push(Device::new(d, spans)?);
        }
        devices.sort_by_key(|d| d.id);
        Ok(Config {
            generation: p.generation,
            node,
            universes,
            devices,
            policy: p.policy.unwrap_or_default(),
            index,
        })
    }

    fn to_pb(&self) -> pb::NodeConfig {
        pb::NodeConfig {
            generation: self.generation,
            node: Some(self.node.pb),
            universes: self.universes.clone(),
            devices: self.devices.iter().map(|d| d.pb.clone()).collect(),
            policy: Some(self.policy),
        }
    }
}

/// The address a block is placed by, which is not always the address it is named by.
///
/// A mutable block is placed on its own, so every 4 KiB block of a mutable extent is
/// routed independently. An immutable block is placed by the absolute 4 MiB stripe it
/// falls in, so the 1024 blocks of a stripe share a consensus group and a peer link
/// while keeping their own register, slot, tombstone and cache entry. Extents are
/// allocated stripe-aligned, so two extents never share a stripe.
pub fn placement_of(addr: u64, kind: Kind) -> u64 {
    match kind {
        Kind::Mutable => addr,
        Kind::Immutable => addr & !(HUGE_BLOCKS - 1),
    }
}

/// The group slot an address hashes into. A pure function of the address, so two zones
/// agree on it without sharing a slot table.
fn slot_of(addr: u64) -> u16 {
    (mix(addr) % SLOTS as u64) as u16
}

/// A cheap avalanche so adjacent addresses land in unrelated slots. Any fixed permutation
/// works, but every node must agree and cache.rs and heal.rs derive from it: never change.
pub(crate) fn mix(mut x: u64) -> u64 {
    x ^= x >> 33;
    x = x.wrapping_mul(0xff51_afd7_ed55_8ccd);
    x ^= x >> 33;
    x = x.wrapping_mul(0xc4ce_b9fe_1a85_ec53);
    x ^ (x >> 33)
}

/// The rendezvous score of `node` for `addr`: the one ranking function this crate has.
/// Independent of how many nodes the caller takes, so rings built on it nest (a wider
/// selection appends, never reorders) and two nodes agree without exchanging anything.
/// Shared by the cache's cohort ring (cache.rs) and a zone's gateway ring.
pub(crate) fn rank(addr: u64, node: u32) -> u64 {
    mix(addr ^ mix(node as u64))
}

/// `nodes` in descending rank order for `addr`, lazily. Successive maximum rather than a
/// sort: the lists are tens of entries and callers stop at the first, trading an allocation
/// per cross-zone operation for a linear scan per item taken. The node id breaks a score
/// tie, so the order is total and the same everywhere.
pub(crate) fn ranked(nodes: &[u32], addr: u64) -> impl Iterator<Item = u32> {
    let mut last: Option<(u64, u32)> = None;
    std::iter::from_fn(move || {
        let mut best: Option<(u64, u32)> = None;
        for &n in nodes {
            let k = (rank(addr, n), n);
            if last.is_some_and(|l| k >= l) {
                continue;
            }
            if best.is_none_or(|b| k > b) {
                best = Some(k);
            }
        }
        last = best;
        best.map(|(_, n)| n)
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One node in two universes: the second proves they share nothing but this node.
    const SAMPLE: &str = "\
# the node itself
generation 7
node id=1 zone=1 cohort=0 store=/var/lib/racer/store.img size=68719476736

universe 1 epoch=3 fabric_device_id=9
  peer id=2 device=/dev/nvme1n1
  peer id=7 device=/dev/nvme3n1
  group 1 2 3
  group 4 5 6
  zone id=2 gateways=7,8,9,10
  extent id=10 base=0     blocks=100 kind=mutable           zone=1 cache_admit=3
  extent id=11 base=100   blocks=50  kind=mutable           zone=1
  extent id=12 base=1024  blocks=8192 kind=immutable  zone=1 cache_admit=1 warm_zones=2
  extent id=13 base=16384 blocks=4   kind=mutable           zone=2

device 1 extents=10,11
device 2 extents=12
";

    fn sample() -> Config {
        Config::parse(SAMPLE).unwrap()
    }

    /// A page address in universe `u` at block `lba`.
    fn at(u: u32, lba: u64) -> u64 {
        addr_of(u, lba)
    }

    #[test]
    fn parses_and_validates() {
        let c = sample();
        c.validate().unwrap();
        assert_eq!(c.generation, 7);
        assert_eq!(c.node.id, 1);
        assert_eq!(c.node.store, PathBuf::from("/var/lib/racer/store.img"));
        assert_eq!(c.universes.len(), 1);
        assert_eq!(c.peer_count(), 2);
        assert_eq!(c.extent_count(), 4);
        // 150 mutable and 8192 immutable blocks in our zone, three replicas over six nodes.
        assert_eq!(c.mutable_blocks(), 75);
        assert_eq!(c.immutable_blocks(), 4096);
        assert_eq!(c.extent_at(at(1, 0)).unwrap().id, 10);
        assert_eq!(c.extent_at(at(1, 99)).unwrap().id, 10);
        assert_eq!(c.extent_at(at(1, 100)).unwrap().guard(), Kind::Mutable);
        assert!(c.extent_at(at(1, 150)).is_none(), "the gap is unmapped");
        assert_eq!(c.extent_at(at(1, 1024)).unwrap().guard(), Kind::Immutable);
        assert_eq!(c.extent_at(at(1, 1024 + 1023)).unwrap().id, 12);
        assert!(c.extent_at(at(2, 0)).is_none(), "universe 2 does not exist");
        assert_eq!(c.extent_by_id(12).unwrap().0.id, 1);
        assert!(c.extent_by_id(99).is_none());
    }

    /// Both kinds are addressed the same way: one device block is one 4 KiB block, and the
    /// kind changes nothing about the mapping. It decides placement and durability, not
    /// geometry.
    #[test]
    fn a_device_concatenates_whole_extents() {
        let c = sample();
        let d = c.devices.iter().find(|d| d.id == 1).unwrap();
        assert_eq!(d.blocks(), 150);
        assert_eq!(d.bytes(), 150 * 4096);
        assert_eq!(d.map(0), Some(at(1, 0)));
        assert_eq!(d.map(99), Some(at(1, 99)));
        assert_eq!(d.map(100), Some(at(1, 100)), "the second extent starts here");
        assert_eq!(d.map(149), Some(at(1, 149)));
        assert_eq!(d.map(150), None);

        let h = c.devices.iter().find(|d| d.id == 2).unwrap();
        assert_eq!(h.blocks(), 8 * 1024);
        assert_eq!(h.bytes(), 8 * (4 << 20));
        assert_eq!(h.map(0), Some(at(1, 1024)));
        assert_eq!(h.map(1), Some(at(1, 1025)), "the next block, not the next stripe");
        assert_eq!(h.map(1024), Some(at(1, 2048)));
        assert_eq!(h.map(8 * 1024), None);
    }

    /// The same extents may compose into different devices, in any order, and repeat.
    #[test]
    fn extents_compose_in_any_order_and_combination() {
        let c = Config::parse(&format!("{SAMPLE}device 3 extents=11,10\n")).unwrap();
        c.validate().unwrap();
        let d = c.devices.iter().find(|d| d.id == 3).unwrap();
        assert_eq!(d.blocks(), 150);
        assert_eq!(d.map(0), Some(at(1, 100)), "extent 11 is mounted first");
        assert_eq!(d.map(50), Some(at(1, 0)));
        assert_eq!(d.map(149), Some(at(1, 99)));
    }

    /// A device may hold both kinds. Nothing about the export changes: there is one block
    /// size, and a request is cut the same way over either extent.
    #[test]
    fn a_device_may_mix_kinds() {
        let c = Config::parse(&format!("{SAMPLE}device 3 extents=10,12,11\n")).unwrap();
        c.validate().unwrap();
        let d = c.devices.iter().find(|d| d.id == 3).unwrap();
        assert_eq!(d.blocks(), 100 + 8 * 1024 + 50);
        assert_eq!(d.bytes(), (100 + 8 * 1024 + 50) * 4096);

        assert_eq!(d.map(99), Some(at(1, 99)), "the last mutable block");
        assert_eq!(d.map(100), Some(at(1, 1024)), "the immutable extent starts");
        assert_eq!(d.map(100 + 1023), Some(at(1, 1024 + 1023)));
        assert_eq!(d.map(100 + 1024), Some(at(1, 2048)));
        assert_eq!(
            d.map(100 + 8 * 1024),
            Some(at(1, 100)),
            "back to the mutable extent"
        );
        assert_eq!(d.map(100 + 8 * 1024 + 49), Some(at(1, 149)));
        assert_eq!(d.map(100 + 8 * 1024 + 50), None);
    }

    /// A mutable block is placed on itself; an immutable one is placed on the 4 MiB stripe
    /// that contains it. The 1024 blocks of a stripe therefore share a group, and adjacent
    /// stripes need not.
    #[test]
    fn immutable_blocks_are_placed_by_stripe() {
        let c = sample();
        let base = at(1, 1024);
        assert_eq!(placement_of(base, Kind::Immutable), base);
        assert_eq!(placement_of(at(1, 1024 + 1), Kind::Immutable), base);
        assert_eq!(placement_of(at(1, 1024 + 1023), Kind::Immutable), base);
        assert_eq!(placement_of(at(1, 2048), Kind::Immutable), at(1, 2048));
        // A mutable block is its own placement, so neighbors are placed independently.
        assert_eq!(placement_of(at(1, 7), Kind::Mutable), at(1, 7));

        let g = c.group_of(base, Kind::Immutable);
        for i in [1, 5, 1023] {
            assert_eq!(
                c.group_of(at(1, 1024 + i), Kind::Immutable),
                g,
                "block {i} of the stripe shares the stripe's group"
            );
        }
        // The kind is a parameter, not a lookup: a row that outlives its extent still
        // resolves to the group its class puts it in.
        assert_eq!(c.group(base), Some(g));
    }

    #[test]
    fn a_device_maps_an_extent_once() {
        let c = Config::parse(&format!("{SAMPLE}device 3 extents=10,10\n")).unwrap();
        assert!(c.validate().is_err(), "a device may not repeat an extent");
    }

    #[test]
    fn a_device_maps_extents_that_exist() {
        let e = Config::parse(&format!("{SAMPLE}device 3 extents=10,77\n")).unwrap_err();
        assert!(format!("{e}").contains("unknown extent"), "{e}");
    }

    /// Every export is a ublk device the node asks the kernel for by minor, so the fabric
    /// namespace needs one as much as a local device does.
    #[test]
    fn a_universe_names_the_device_its_fabric_is_exported_as() {
        let c = sample();
        assert_eq!(c.universes[0].fabric_device_id, 9);

        let mut c = sample();
        c.universes[0].fabric_device_id = 0;
        let e = c.validate().unwrap_err();
        assert!(format!("{e}").contains("no device"), "{e}");
    }

    /// Minors are unique per node, whoever is asking: two fabrics, or a fabric and a local
    /// device, wanting one is a config the kernel would only refuse half way through.
    #[test]
    fn two_exports_may_not_ask_for_one_device() {
        let mut text = SAMPLE.to_string();
        text.push_str(
            "universe 2 epoch=1 fabric_device_id=9
  group 1 2 3
  extent id=20 base=0 blocks=8 kind=mutable zone=1
",
        );
        let e = Config::parse(&text).unwrap().validate().unwrap_err();
        assert!(
            format!("{e}").contains("universe 1 fabric and universe 2 fabric"),
            "{e}"
        );

        let mut c = sample();
        c.universes[0].fabric_device_id = 2;
        let e = c.validate().unwrap_err();
        assert!(
            format!("{e}").contains("device 2 and universe 1 fabric"),
            "{e}"
        );
    }

    /// Peers hold the path of our fabric device, so where it is exported is frozen for as
    /// long as the universe is here.
    #[test]
    fn a_fabric_device_does_not_move() {
        let b = sample();
        let mut c = sample();
        c.generation = 8;
        c.universes[0].fabric_device_id = 30;
        let e = c.validate_against(&b).unwrap_err();
        assert!(format!("{e}").contains("moves its fabric"), "{e}");

        // A universe that is gone takes its fabric with it, and a new one may have any.
        let mut c = sample();
        c.generation = 8;
        c.universes.clear();
        c.validate_against(&b).unwrap();
    }

    /// Fabric devices come out of the same budget as local ones: both are ublk devices.
    #[test]
    fn exports_are_counted_together() {
        let mut c = sample();
        let one = c.devices[0].clone();
        for id in 100..100 + (MAX_EXPORTS as u32 - 3) {
            let mut d = one.clone();
            d.pb.id = id;
            c.devices.push(d);
        }
        c.devices.sort_by_key(|d| d.id);
        c.validate().unwrap();

        let mut d = one.clone();
        d.pb.id = 1000;
        c.devices.push(d);
        let e = c.validate().unwrap_err();
        assert!(format!("{e}").contains("more than the 256"), "{e}");
    }

    /// Extents share one flat space per universe, so overlap is a fatal placement mistake.
    #[test]
    fn extents_may_not_overlap() {
        let mut c = sample();
        c.universes[0].extents[1].base_lba = 99;
        assert!(c.validate().is_err(), "99 is still inside extent 10");
        let mut c = sample();
        c.universes[0].extents[2].base_lba = 100;
        assert!(c.validate().is_err(), "a 4 MiB extent must be aligned");
    }

    #[test]
    fn extents_must_fit_in_the_universe() {
        for (base, pages) in [(MAX_LBA + 1, 1), (0, 1u64 << 54)] {
            let text = SAMPLE.replace(
                "extent id=12 base=1024  blocks=8192 kind=immutable  zone=1 cache_admit=1 warm_zones=2",
                &format!(
                    "extent id=12 base={base} blocks={pages} kind=immutable zone=1 cache_admit=1 warm_zones=2"
                ),
            );
            let result = Config::parse(&text).and_then(|c| c.validate());
            assert!(result.is_err(), "base={base} blocks={pages}");
        }
    }

    #[test]
    fn an_extent_id_names_one_extent() {
        let mut text = SAMPLE.to_string();
        text.push_str("universe 2 epoch=1 fabric_device_id=10\n  group 1 2 3\n");
        text.push_str("  extent id=10 base=0 blocks=8 kind=mutable zone=1\n");
        let e = Config::parse(&text).unwrap_err();
        assert!(format!("{e}").contains("used twice"), "{e}");
    }

    /// Two universes share nothing but the node: separate address spaces, catalogs, links.
    #[test]
    fn universes_partition_everything() {
        let mut text = SAMPLE.to_string();
        text.push_str(
            "universe 2 epoch=1 fabric_device_id=10
  peer id=9 device=/dev/nvme2n1
  group 1 8 9
  extent id=20 base=0 blocks=100 kind=mutable zone=1
device 3 extents=20
",
        );
        let c = Config::parse(&text).unwrap();
        c.validate().unwrap();

        // The same block in two universes is two different pages.
        assert_ne!(at(1, 0), at(2, 0));
        assert_eq!(c.extent_at(at(1, 0)).unwrap().id, 10);
        assert_eq!(c.extent_at(at(2, 0)).unwrap().id, 20);
        // And two different groups, even when the block hashes to the same slot.
        let g1 = c.group(at(1, 0)).unwrap();
        let g2 = c.group(at(2, 0)).unwrap();
        assert_eq!(g1.universe(), 1);
        assert_eq!(g2.universe(), 2);
        assert_ne!(g1, g2);
        // Catalogs are per universe, so a member of one is not a member of the other.
        assert_eq!(c.universe(1).unwrap().zone_nodes(), vec![1, 2, 3, 4, 5, 6]);
        assert_eq!(c.universe(2).unwrap().zone_nodes(), vec![1, 8, 9]);
        assert_eq!(c.peer_count(), 3);
        assert_eq!(
            c.peers().map(|(u, p)| (u, p.id)).collect::<Vec<_>>(),
            vec![(1, 2), (1, 7), (2, 9)]
        );
        // Storage is the sum of our share of each: 75 from universe 1, 100*3/3 from 2.
        assert_eq!(c.mutable_blocks(), 75 + 100);
    }

    /// A namespace belongs to one universe; the same path in two would breach the boundary.
    #[test]
    fn a_namespace_belongs_to_one_universe() {
        let mut text = SAMPLE.to_string();
        text.push_str(
            "universe 2 epoch=1 fabric_device_id=10
  peer id=2 device=/dev/nvme1n1
  group 1 2 3
  extent id=20 base=0 blocks=8 kind=mutable zone=1
",
        );
        let c = Config::parse(&text).unwrap();
        let e = c.validate().unwrap_err();
        assert!(format!("{e}").contains("two peers"), "{e}");
    }

    #[test]
    fn the_store_path_comes_from_the_environment() {
        assert_eq!(path_from(None), PathBuf::from(DEFAULT_STORE_PATH));
        assert_eq!(
            path_from(Some(OsString::from(""))),
            PathBuf::from(DEFAULT_STORE_PATH)
        );
        assert_eq!(
            path_from(Some(OsString::from("/mnt/x.img"))),
            PathBuf::from("/mnt/x.img")
        );

        // A config carries the store's size but not its path, so `store=` absent must fall
        // back to the environment rather than an empty path.
        let text = SAMPLE.replace(" store=/var/lib/racer/store.img", "");
        let c = Config::parse(&text).unwrap();
        assert_eq!(c.node.store, store_path());
        c.validate().unwrap();
    }

    #[test]
    fn a_store_needs_a_size_and_a_path() {
        let mut c = sample();
        c.node.set_store_bytes(0);
        assert!(c.validate().is_err());

        let mut c = sample();
        c.node.set_store_bytes(4097);
        assert!(c.validate().is_err(), "not a whole number of pages");

        let mut c = sample();
        c.node.store = PathBuf::new();
        assert!(c.validate().is_err());
    }

    #[test]
    fn a_store_may_grow_but_never_shrink() {
        let b = sample();
        let mut c = sample();
        c.generation = 8;
        c.node.set_store_bytes(b.node.store_bytes() * 2);
        c.validate_against(&b).unwrap();

        let mut c = sample();
        c.generation = 8;
        c.node.set_store_bytes(b.node.store_bytes() - 4096);
        assert!(c.validate_against(&b).is_err());
    }

    #[test]
    fn addresses_resolve_to_a_zone_and_a_gateway() {
        let c = sample();
        assert_eq!(c.zone_of(at(1, 0)), Some(1));
        assert_eq!(c.zone_of(at(1, 16384)), Some(2), "extent 13 is foreign");
        assert_eq!(c.zone_of(at(1, 150)), None);
        assert_eq!(c.epoch_of(at(1, 0)), 3);

        let ring: Vec<u32> = c.gateways_for(2, at(1, 16384)).collect();
        assert_eq!(ring.len(), 4, "every gateway is offered, in order");
        let mut sorted = ring.clone();
        sorted.sort_unstable();
        assert_eq!(sorted, vec![7, 8, 9, 10]);

        assert_eq!(
            c.gateways_for(3, at(1, 0)).count(),
            0,
            "zone 3 was never named"
        );
        // Our own zone has no gateways: we are already in it.
        assert_eq!(c.gateways_for(1, at(1, 0)).count(), 0);
    }

    /// Unlike the cache's ring this one promotes: senders fall through in a shared order.
    #[test]
    fn the_gateway_ring_spreads_and_falls_through() {
        let c = sample();
        let mut first = std::collections::BTreeSet::new();
        for lba in 16384..16388 {
            let ring: Vec<u32> = c.gateways_for(2, at(1, lba)).collect();
            assert_eq!(ring.len(), 4);
            // A total order: no repeats, and the same address always gives the same one.
            let uniq: std::collections::BTreeSet<u32> = ring.iter().copied().collect();
            assert_eq!(uniq.len(), 4);
            assert_eq!(ring, c.gateways_for(2, at(1, lba)).collect::<Vec<_>>());
            first.insert(ring[0]);
        }
        assert!(
            first.len() > 1,
            "consecutive addresses do not all pick one gateway"
        );
    }

    /// The order is a function of the address and ids alone, not of which links a node
    /// holds.
    #[test]
    fn the_gateway_order_is_stable_under_reordering() {
        let a = ranked(&[7, 8, 9, 10], 42).collect::<Vec<_>>();
        let b = ranked(&[10, 9, 8, 7], 42).collect::<Vec<_>>();
        assert_eq!(a, b);
        assert_eq!(a.len(), 4);
        // Dropping one leaves the rest in relative order, which makes fall-through cheap.
        let without = ranked(&[7, 9, 10], 42).collect::<Vec<_>>();
        let expect: Vec<u32> = a.iter().copied().filter(|&n| n != 8).collect();
        assert_eq!(without, expect);
    }

    #[test]
    fn a_zone_names_gateways_that_could_be_nodes() {
        let bad_zone = |line: &str| {
            let t = SAMPLE.replace("zone id=2 gateways=7,8,9,10", line);
            Config::parse(&t).and_then(|c| c.validate())
        };
        assert!(bad_zone("zone id=2 gateways=7").is_ok());
        assert!(
            bad_zone("zone id=2 gateways=8,9").is_ok(),
            "a gateway we hold no link to is a runtime answer, not a bad config"
        );
        assert!(
            bad_zone("zone id=2 gateways=0,7").is_err(),
            "node 0 is not a node"
        );
        assert!(bad_zone("zone id=2 gateways=7,7").is_err(), "named twice");
        assert!(bad_zone("zone id=2 gateways=1,7").is_err(), "that is us");
        let many: Vec<String> = (7..7 + MAX_GATEWAYS as u32 + 1)
            .map(|n| n.to_string())
            .collect();
        assert!(
            bad_zone(&format!("zone id=2 gateways={}", many.join(","))).is_err(),
            "above the ceiling"
        );
    }

    #[test]
    fn a_zone_with_no_gateways_is_refused() {
        // The parser will not accept an empty list, and neither will validation.
        let t = SAMPLE.replace("zone id=2 gateways=7,8,9,10", "zone id=2 gateways=");
        assert!(Config::parse(&t).and_then(|c| c.validate()).is_err());
    }

    #[test]
    fn warming_names_zones_that_do_not_already_hold_the_pages() {
        let warm = |line: &str| {
            let t = SAMPLE.replace(
                "extent id=12 base=1024  blocks=8192 kind=immutable  zone=1 cache_admit=1 warm_zones=2",
                line,
            );
            assert_ne!(t, SAMPLE, "the fixture line moved");
            Config::parse(&t).and_then(|c| c.validate())
        };
        let base = "extent id=12 base=1024 blocks=8192 kind=immutable zone=1 cache_admit=1";
        assert!(warm(&format!("{base} warm_zones=2")).is_ok());
        assert!(warm(base).is_ok(), "warming nobody is the default");
        assert!(
            warm(&format!("{base} warm_zones=3")).is_err(),
            "zone 3 is unknown"
        );
        assert!(
            warm(&format!("{base} warm_zones=1")).is_err(),
            "that is the home zone"
        );
        assert!(
            warm(&format!("{base} warm_zones=2,2")).is_err(),
            "named twice"
        );
        assert!(
            warm(&format!("{base} next_zone=2 warm_zones=2")).is_err(),
            "the destination will hold them outright"
        );
        let many: Vec<String> = (2..2 + MAX_WARM_ZONES as u32 + 1)
            .map(|n| n.to_string())
            .collect();
        assert!(
            warm(&format!("{base} warm_zones={}", many.join(","))).is_err(),
            "above the ceiling"
        );
    }

    /// A warm copy is believed on sight, which only an immutable version supports.
    #[test]
    fn only_an_immutable_extent_may_be_warmed() {
        let t = SAMPLE.replace(
            "extent id=10 base=0     blocks=100 kind=mutable           zone=1 cache_admit=3",
            "extent id=10 base=0 blocks=100 kind=mutable zone=1 cache_admit=3 warm_zones=2",
        );
        assert_ne!(t, SAMPLE, "the fixture line moved");
        assert!(
            Config::parse(&t).and_then(|c| c.validate()).is_err(),
            "OCC pages carry no version a remote reader could trust"
        );
    }

    #[test]
    fn warming_is_read_from_both_ends() {
        let c = sample();
        // Extent 12 is ours and asks for zone 2 to be warmed.
        assert_eq!(c.warm_zones_of(at(1, 1024)), &[2]);
        // We are zone 1, so nothing here is warmed *for us*.
        assert!(!c.warmed_here(at(1, 1024)));
        assert!(c.warm_zones_of(at(1, 0)).is_empty());
        assert!(c.warm_zones_of(at(1, 150)).is_empty(), "unmapped");

        // The same extent, in the file a node of the warmed zone runs: the extent is
        // still homed in zone 1, and this node's own zone is the one it names.
        let t = "\
generation 7
node id=20 zone=2 cohort=0 store=/var/lib/racer/store.img size=68719476736
universe 1 epoch=3 fabric_device_id=9
  peer id=21 device=/dev/nvme1n1
  group 20 21 22
  zone id=1 gateways=21,22
  extent id=12 base=1024 blocks=8192 kind=immutable zone=1 cache_admit=1 warm_zones=2
device 2 extents=12
";
        let c = Config::parse(t).unwrap();
        c.validate().unwrap();
        assert!(c.warmed_here(at(1, 1024)), "our zone is named");
        assert_eq!(c.zone_of(at(1, 1024)), Some(1), "still homed there");
    }

    /// One config names the rendezvous winner of every cohort of its own zone, so a
    /// gateway can fan a warm out across all three.
    #[test]
    fn a_catalog_names_a_winner_in_every_cohort() {
        let c = sample();
        let u = c.universe(1).unwrap();
        // The sample catalog is `1 2 3` and `4 5 6`, so column `c` is `{1+c, 4+c}`.
        for col in 0..3usize {
            let w = u.cohort_winner(at(1, 0), col).unwrap();
            assert!(
                w == 1 + col as u32 || w == 4 + col as u32,
                "cohort {col} winner {w} is not in that column"
            );
            assert_eq!(w, u.cohort_winner(at(1, 0), col).unwrap(), "stable");
        }
        assert_eq!(
            u.cohort_winner(at(1, 0), 3),
            None,
            "there is no fourth cohort"
        );
    }

    #[test]
    fn a_migrating_extent_names_its_destination() {
        let mut c = sample();
        assert_eq!(c.next_zone_of(at(1, 0)), None);
        c.universes[0].extents[0].next_zone = 2;
        assert_eq!(c.next_zone_of(at(1, 0)), Some(2));
        c.validate().unwrap();
        // Both zones hold it while it moves, so our share does not fall as it leaves.
        assert_eq!(c.mutable_blocks(), 75);

        let mut c = sample();
        c.universes[0].extents[0].next_zone = 1;
        assert!(c.validate().is_err(), "migrating to the zone it is in");

        let mut c = sample();
        c.universes[0].extents[0].next_zone = 9;
        assert!(c.validate().is_err(), "zone 9 was never named");
    }

    #[test]
    fn placement_names_a_known_zone() {
        let mut c = sample();
        c.universes[0].extents[0].zone = 9;
        assert!(c.validate().is_err());
        let mut c = sample();
        c.universes[0].extents[0].zone = 0;
        assert!(c.validate().is_err());
    }

    #[test]
    fn protobuf_round_trip() {
        let c = sample();
        let back = Config::from_pb(pb::NodeConfig::decode(&c.encode()[..]).unwrap()).unwrap();
        assert_eq!(back.universes, c.universes);
        assert_eq!(back.devices, c.devices);
        assert_eq!(back.index, c.index);
        assert_eq!(back.generation, c.generation);
        assert_eq!(back.policy, c.policy);
        back.validate().unwrap_err(); // no store path: `decode` fills that in, `from_pb` does not
    }

    /// The config is pushed whole on every change, so its size is control-plane write cost.
    #[test]
    fn stays_small() {
        let mut c = sample();
        c.universes[0].catalog = (0..300u32)
            .map(|i| [i * 3 + 1, i * 3 + 2, i * 3 + 3].into())
            .collect();
        assert!(c.encode().len() < 100 << 10, "{}", c.encode().len());
    }

    #[test]
    fn shape_is_frozen_across_generations() {
        let b = sample();
        for f in [
            (|c: &mut Config| c.universes[0].extents[0].blocks = 101) as fn(&mut Config),
            |c| c.universes[0].extents[0].base_lba = 4096,
            |c| c.universes[0].extents[0].kind = pb::Kind::Immutable as i32,
            |c| c.universes[0].extents[2].kind = pb::Kind::Immutable as i32,
            |c| c.universes[0].catalog.push([7, 8, 9].into()),
            |c| c.devices[0].pb.extents[0] = 11,
        ] {
            let mut c = sample();
            c.generation = 8;
            f(&mut c);
            assert!(c.validate_against(&b).is_err());
        }
    }

    /// An extent may be dropped from the new config; only a surviving id must keep shape.
    #[test]
    fn an_extent_may_be_unmapped() {
        let b = sample();
        let mut c = sample();
        c.generation = 8;
        c.universes[0].extents.remove(3);
        c.index.retain(|&(id, _, _)| id != 13);
        c.validate_against(&b).unwrap();
    }

    #[test]
    fn placement_moves_only_along_a_migration() {
        let mut b = sample();
        b.universes[0].extents[0].next_zone = 2;

        let mut c = sample();
        c.generation = 8;
        c.universes[0].extents[0].zone = 2;
        c.validate_against(&b).unwrap();

        let mut c = sample();
        c.generation = 8;
        c.universes[0].extents[0].zone = 2;
        assert!(
            c.validate_against(&sample()).is_err(),
            "nothing was migrating"
        );
    }

    #[test]
    fn catalog_moves_one_node_at_a_time() {
        let b = sample();

        let mut c = sample();
        c.generation = 8;
        c.universes[0].catalog[0] = [7, 2, 3].into();
        c.validate_against(&b).unwrap();

        let mut c = sample();
        c.generation = 8;
        c.universes[0].catalog[0] = [7, 8, 3].into();
        assert!(c.validate_against(&b).is_err());

        // Two generations on, the same jump is not something we can second-guess.
        let mut c = sample();
        c.generation = 9;
        c.universes[0].catalog[0] = [7, 8, 3].into();
        c.validate_against(&b).unwrap();

        let mut c = sample();
        c.generation = 8;
        c.universes[0].catalog.pop();
        assert!(c.validate_against(&b).is_err(), "the length is frozen");
    }

    #[test]
    fn capacity_is_an_equal_share_of_the_universe() {
        let c = Config::parse(
            "generation 1
node id=1 zone=1 cohort=0 store=/x size=1048576
universe 1 epoch=1 fabric_device_id=9
  group 1 2 3
  extent id=1 base=0 blocks=300 kind=mutable zone=1
",
        )
        .unwrap();
        c.validate().unwrap();
        assert_eq!(c.mutable_blocks(), 300, "three replicas over three nodes");
        assert_eq!(c.immutable_blocks(), 0);
    }

    #[test]
    fn capacity_follows_the_slots_a_node_holds() {
        // Two groups, and node 3 has handed one of them to node 4. Node 1 still holds both
        // of its column and sizes for the whole zone; node 3 holds one and sizes for half.
        let text = "generation 1
node id=%ID% zone=1 cohort=%COHORT% store=/x size=1048576
universe 1 epoch=1 fabric_device_id=9
  group 1 2 3
  group 1 2 4
  extent id=1 base=0 blocks=300 kind=mutable zone=1
";
        let held_both = Config::parse(&text.replace("%ID%", "1").replace("%COHORT%", "0")).unwrap();
        held_both.validate().unwrap();
        assert_eq!(held_both.mutable_blocks(), 300, "both groups of its column");

        let held_one = Config::parse(&text.replace("%ID%", "3").replace("%COHORT%", "2")).unwrap();
        held_one.validate().unwrap();
        assert_eq!(held_one.mutable_blocks(), 150, "one group of two");

        let held_none = Config::parse(&text.replace("%ID%", "9").replace("%COHORT%", "2")).unwrap();
        held_none.validate().unwrap();
        assert_eq!(
            held_none.mutable_blocks(),
            300,
            "six slots over four nodes, rounded up"
        );
    }

    #[test]
    fn an_uneven_catalog_is_accepted() {
        // Node 1 holds both groups of its column and node 4 holds one of node 3's. A zone
        // moving groups one at a time is uneven for as long as the move takes, and refusing
        // that would mean refusing every state between two even catalogs.
        let mut c = sample();
        c.universes[0].catalog = vec![[1, 2, 3].into(), [1, 2, 4].into()];
        c.validate().unwrap();
    }

    #[test]
    fn a_group_still_needs_three_distinct_nodes() {
        let mut c = sample();
        c.universes[0].catalog = vec![[1, 1, 2].into()];
        assert!(c.validate().is_err(), "a group needs three distinct nodes");
        let mut c = sample();
        c.universes[0].catalog = vec![[1, 0, 2].into()];
        assert!(c.validate().is_err(), "a group may not name node 0");
        let mut c = sample();
        c.universes[0].catalog.clear();
        assert!(c.validate().is_err());
    }

    /// The sweep runs on a full publication whether or not an extent is collecting: an
    /// extent that has left the configuration outright leaves rows behind too. It must not
    /// run on the bootstrap shape, which names no extents because it names nothing yet.
    #[test]
    fn only_a_full_publication_reclaims() {
        let mut b = sample();
        assert!(b.reclaimable(), "a config with peers can retire rows");

        b.universes[0].extents.clear();
        assert!(
            b.reclaimable(),
            "an emptied extent list is work, not silence"
        );

        for u in &mut b.universes {
            u.peers.clear();
        }
        assert!(!b.reclaimable(), "the bootstrap shape retires nothing");
    }

    #[test]
    fn tombstone_epoch_moves_forward_per_extent() {
        let mut b = sample();
        b.universes[0].extents[0].tombstone_epoch = 3;
        assert!(b.reclaimable());
        assert_eq!(b.tombstone_epoch_of(at(1, 0)), 3);
        assert_eq!(
            b.tombstone_epoch_of(at(1, 100)),
            0,
            "per extent, not global"
        );

        let mut c = b.clone();
        c.generation = 8;
        c.universes[0].extents[0].tombstone_epoch = 4;
        c.validate_against(&b).unwrap();

        let mut c = b.clone();
        c.generation = 9;
        c.universes[0].extents[0].tombstone_epoch = 2;
        assert!(
            c.validate_against(&b).is_err(),
            "a decrease strands every live page"
        );
    }

    /// Admission is per extent: two in one universe differ, and an unmapped address is 0.
    #[test]
    fn cache_admission_is_per_extent() {
        let c = sample();
        assert_eq!(c.cache_admit_of(at(1, 0)), 3);
        assert_eq!(c.cache_admit_of(at(1, 99)), 3);
        assert_eq!(c.cache_admit_of(at(1, 100)), 0, "the occ extent opts out");
        assert_eq!(
            c.cache_admit_of(at(1, 1024)),
            1,
            "the 4 MiB extent admits all"
        );
        assert_eq!(c.cache_admit_of(at(1, 150)), 0, "the gap is unmapped");
        assert_eq!(c.cache_admit_of(at(2, 0)), 0, "universe 2 does not exist");
    }

    /// The threshold is compared against a 4-bit sketch counter, so it has to fit in one.
    #[test]
    fn cache_admission_fits_the_counter() {
        for (n, ok) in [(0, true), (1, true), (7, true), (15, true), (16, false)] {
            let s = SAMPLE.replace("cache_admit=3", &format!("cache_admit={n}"));
            assert_eq!(Config::parse(&s).is_ok(), ok, "cache_admit={n}");
        }
    }

    /// Admission is a policy knob, not frozen shape: any reload may move it either way.
    #[test]
    fn cache_admission_may_change_on_a_reload() {
        let b = sample();
        for n in [0u32, 1, 15] {
            let mut c = sample();
            c.generation = 8;
            c.universes[0].extents[0].cache_admit = n;
            c.validate_against(&b).unwrap();
        }
    }

    #[test]
    fn a_universe_id_fits_an_address() {
        let mut c = sample();
        c.universes[0].id = MAX_UNIVERSE;
        assert!(c.validate().is_err());
        let mut c = sample();
        c.universes[0].id = 0;
        assert!(c.validate().is_err());
        assert_eq!(
            universe_of(at(MAX_UNIVERSE - 1, MAX_LBA - 1)),
            MAX_UNIVERSE - 1
        );
        assert_eq!(lba_of(at(MAX_UNIVERSE - 1, MAX_LBA - 1)), MAX_LBA - 1);
    }

    /// Delivery is a rename, so the watch must survive the inode changing underneath it.
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

    /// Delivery end to end: a good generation applies, a bad one is refused without
    /// disturbing what runs, and the next good one still lands.
    #[test]
    fn watch_applies_and_refuses() {
        let dir = std::env::temp_dir().join(format!("racer-apply-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("config.pb");
        std::fs::write(&path, sample().encode()).unwrap();

        let (tx, rx) = std::sync::mpsc::channel();
        let p = path.clone();
        let t = std::thread::spawn(move || {
            let mut current = sample();
            watch(&p, move |c| {
                if c.generation <= current.generation {
                    return Ok(false);
                }
                c.validate()
                    .and_then(|()| c.validate_against(&current))
                    .map_err(crate::runtime::UpdateError::Candidate)?;
                tx.send(c.generation).unwrap();
                current = c;
                Ok(true)
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
        // The watch starts on the other thread, so announce until it answers. inotify
        // reports that the file changed, not how often, so each step must be seen first.
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

        // Wait for the count to settle before taking the baseline.
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
        // Generation 8 again: not an advance, so the lifecycle ignores it.
        put(8, &|_| {});
        refused(0);
        // A shape change against the config now running.
        put(9, &|c| c.universes[0].extents[0].blocks = 999);
        refused(1);
        put(10, &|_| {});
        assert_eq!(rx.recv_timeout(took).unwrap(), 10);
        refused(1);

        drop(t);
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
