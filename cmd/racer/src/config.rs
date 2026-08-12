//! Node configuration: the only input the control plane gives the dataplane, one protobuf
//! file per node, replaced whole and applied all or nothing.
//!
//! `pb` is the wire schema, tag for tag. [`Config`] is the validated model, and building
//! one from `pb` is where every structural check happens. `Watch`/[`watch`] are delivery,
//! driven by inotify because the control plane renames a new file over the old one.
//! `Live` makes reload cheap: a pointer the control thread swaps, read without a lock.

use std::ffi::{CString, OsString};
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicPtr, AtomicU64, Ordering};

use prost::Message as _;

/// Consensus slots an address may hash to. The owning group is `slot % catalog.len()`.
const SLOTS: usize = 16384;
const SMALL_PAGE: u64 = 4096;
const HUGE_PAGE: u64 = 4 << 20;

/// Largest useful `Extent::cache_admit`: the cache's demand counter is 4 bits (cache.rs).
pub(crate) const CACHE_MAX_ADMIT: u32 = 15;

/// Gateways one zone may name. Keeps this message `O(zones)` rather than `O(cluster)`.
const MAX_GATEWAYS: usize = 64;

/// Zones one extent may warm. Each costs a page transfer per cohort on every commit.
const MAX_WARM_ZONES: usize = 16;

/// Refused configs, counted and dropped: `racer_config_rejected_total` in metrics.rs.
static REJECTED: AtomicU64 = AtomicU64::new(0);

/// Configs refused since boot.
pub fn rejected() -> u64 {
    REJECTED.load(Ordering::Relaxed)
}

/// Where the backing store lives. Not in the config file: a deployment, not cluster, fact.
pub const STORE_PATH_ENV: &str = "RACER_STORE";
/// Used when `RACER_STORE` is unset or empty.
pub const DEFAULT_STORE_PATH: &str = "/var/lib/racer/store.img";

/// The backing store's path for this process.
pub fn store_path() -> PathBuf {
    path_from(std::env::var_os(STORE_PATH_ENV))
}

/// Split out from [`store_path`] so defaulting is testable without racing the process env.
fn path_from(v: Option<OsString>) -> PathBuf {
    match v {
        Some(s) if !s.is_empty() => PathBuf::from(s),
        _ => PathBuf::from(DEFAULT_STORE_PATH),
    }
}

fn bad(msg: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, msg.into())
}

// --- Live: the reload primitive ---

/// A value the control thread replaces and every worker reads without a lock. A read is a
/// single acquire load; the replaced value lives until its published configuration retires,
/// since [`Runtime::reload`] blocks until every core cuts over. Do not hold the reference
/// across reload.
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
        // `old` may still be held by an in-flight request; `prev` was retired last reload.
        *prev = Some(unsafe { Box::from_raw(old) });
    }

    /// Drop the replaced value after the runtime has drained its configuration guards.
    pub(crate) fn retire(&self) {
        self.prev.lock().unwrap().take();
    }
}

impl<T> Drop for Live<T> {
    fn drop(&mut self) {
        drop(unsafe { Box::from_raw(*self.cur.get_mut()) });
    }
}

// --- Wire schema ---

/// The file as written and read, generated from `proto/config.proto` and shared verbatim
/// with the Go control plane, where tags are the compatibility contract. Wire fields are
/// optional and unvalidated, so nothing outside reads `pb`; [`Config`] is the checked form.
mod pb {
    include!(concat!(env!("OUT_DIR"), "/racer.config.rs"));
}

/// What guard a write to an extent must present. Narrower than the wire enum, which also
/// spells the page size: `IMMUTABLE_4M` is [`Kind::Immutable`] at 4 MiB, same guard. Width
/// is a property of the extent below here, so the two split on the way in and rejoin out.
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

/// The inverse of [`split_kind`]. Only Immutable has a 4 MiB spelling.
fn join_kind(k: Kind, huge: bool) -> pb::Kind {
    match k {
        Kind::Lww => pb::Kind::Lww,
        Kind::Occ => pb::Kind::Occ,
        Kind::Immutable if huge => pb::Kind::Immutable4m,
        Kind::Immutable => pb::Kind::Immutable,
    }
}
// --- Addressing ---

/// Bits of a page address naming the universe the page lives in.
pub const UNIVERSE_BITS: u32 = 26;
/// Bits of a page address naming a 4 KiB block within that universe: 1 PiB each.
pub const LBA_BITS: u32 = 38;
/// One past the last addressable block in a universe.
pub const MAX_LBA: u64 = 1 << LBA_BITS;
/// One past the last universe id. Zero is reserved so that a zero address in an mblock
/// entry unambiguously means "free slot" (layout.rs), which leaves `1..MAX_UNIVERSE`.
pub const MAX_UNIVERSE: u32 = 1 << UNIVERSE_BITS;
/// Blocks one 4 MiB page spans; it is named by the first, so both sizes share one space.
pub const HUGE_BLOCKS: u64 = HUGE_PAGE / SMALL_PAGE;

/// Fabric namespaces plus local block devices this node may export at once. One ublk
/// device per universe and one per configured device; `runtime::MAX_DEVICES` caps the sum.
pub(crate) const MAX_EXPORTS: usize = 256;

/// Extents this node may be told about; the per-extent metrics table is sized to this.
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

// --- The model ---

/// A contiguous run of pages at a fixed offset in its universe's address space: the unit
/// of page kind and size, zone affinity, tombstone epoch, sealing, migration, census and
/// device composition. Position is explicit, not list order, so hosts may differ on it.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Extent {
    /// Unique node-wide and never reused. Names the extent in a SEAL, and labels metrics.
    pub(crate) id: u32,
    /// First block in its universe. Frozen, and a multiple of [`HUGE_BLOCKS`] for 4 MiB.
    pub(crate) base_lba: u64,
    /// Length in pages of this extent's own size, not in blocks. Frozen.
    pages: u64,
    /// What guard a write must present. Frozen.
    pub(crate) kind: Kind,
    /// 4 MiB pages rather than 4 KiB. Frozen, and derived from the wire kind.
    pub(crate) huge: bool,
    /// The zone answering for these pages. Never zero.
    pub(crate) zone: u32,
    /// Zero when the extent is where it belongs, else the zone taking it over.
    pub(crate) next_zone: u32,
    /// Page versions are `3*epoch + state`: `3e` empty, `3e+1` live, `3e+2` trimmed.
    /// Never decreases; advancing it destroys every page written under the old one.
    pub(crate) tombstone_epoch: u64,
    /// Cache admission threshold (cache.rs): 0 never admits, 1 admits on first sight, `n`
    /// admits once the demand estimate reaches `n`. Not frozen: a reload may change it.
    pub(crate) cache_admit: u8,
    /// Zones whose caches are filled with these pages as they commit, not on first demand
    /// (paxos.rs). Read from both ends: the home zone pushes to each, and a node seeing its
    /// own zone here is a warm destination. Sorted, never `zone`/`next_zone`. Not frozen.
    pub(crate) warm_zones: Box<[u32]>,
}

impl Extent {
    /// Blocks one page of this extent occupies.
    pub(crate) fn blocks_per_page(&self) -> u64 {
        if self.huge { HUGE_BLOCKS } else { 1 }
    }

    /// Length in blocks, which is what the extent reserves in the universe.
    pub(crate) fn blocks(&self) -> u64 {
        self.pages * self.blocks_per_page()
    }

    /// One past the last block. Extents in a universe are disjoint over `base..end`.
    pub(crate) fn end_lba(&self) -> u64 {
        self.base_lba + self.blocks()
    }

    fn contains(&self, lba: u64) -> bool {
        lba >= self.base_lba && lba < self.end_lba()
    }
}

/// A shared LBA space spanning a set of nodes: an address space, a transport, a consensus
/// domain and a security boundary, all the same object. Each universe has its own fabric
/// namespace, so nothing on the wire names a universe and a node holds a link only where
/// the control plane published one. Epoch, catalog, zones and peers live here, not on the
/// node, so two universes on one node stay independent.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Universe {
    /// Non-zero, below [`MAX_UNIVERSE`], never reused.
    pub(crate) id: u32,
    /// This universe's topology epoch: the term a shard is sealed with, and it rides the
    /// trailer of every routed write (paxos.rs).
    pub(crate) epoch: u32,
    /// The ublk minor this universe's fabric namespace is exported as, so peers find it at
    /// `/dev/ublkb<id>` without being told. Non-zero, distinct from every other export this
    /// node makes, and frozen for the life of the universe: the path is in peer configs.
    pub(crate) fabric_device_id: u32,
    /// Index is the group index; each entry is three distinct node ids in paxos member
    /// order, which is also the cohort column. Balanced: every node named holds the same
    /// number of groups.
    pub(crate) catalog: Vec<[u32; 3]>,
    /// The other zones of this universe, with their gateway nodes.
    zones: Vec<Zone>,
    /// Nodes we hold a link to in this universe. One namespace per entry.
    pub(crate) peers: Vec<Peer>,
    /// Sorted by `base_lba` and disjoint, which is what makes `extent_at` a search.
    pub(crate) extents: Vec<Extent>,
}

impl Universe {
    /// The extent covering `lba`, if any block of this universe's space is mapped there.
    pub(crate) fn extent_at(&self, lba: u64) -> Option<&Extent> {
        let i = self.extents.partition_point(|e| e.base_lba <= lba);
        let e = self.extents.get(i.checked_sub(1)?)?;
        e.contains(lba).then_some(e)
    }

    /// Every node the catalog names, sorted and deduplicated.
    pub(crate) fn zone_nodes(&self) -> Vec<u32> {
        let mut v: Vec<u32> = self.catalog.iter().flatten().copied().collect();
        v.sort_unstable();
        v.dedup();
        v
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
            let n = *g.get(c)?;
            let k = (rank(addr, n), n);
            if best.is_none_or(|b| k > b) {
                best = Some(k);
            }
        }
        best.map(|(_, n)| n)
    }

    /// Pages of one class this universe places in `ours`, counting an extent on its way
    /// in as well as one on its way out: both zones hold it while it moves.
    fn zone_pages(&self, huge: bool, ours: u32) -> u64 {
        self.extents
            .iter()
            .filter(|e| e.huge == huge && (e.zone == ours || e.next_zone == ours))
            .map(|e| e.pages)
            .sum()
    }
}

/// One extent of a local block device, resolved against the config that named it.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Span {
    pub(crate) universe: u32,
    pub(crate) extent: u32,
    base_lba: u64,
    pages: u64,
    /// 4 MiB pages, taken from the extent: page size is a property of the span, not of
    /// the device around it.
    huge: bool,
}

impl Span {
    fn blocks(&self) -> u64 {
        self.pages * if self.huge { HUGE_BLOCKS } else { 1 }
    }
}

/// What page sizes a device is built from, which is all the kernel needs to know about
/// its shape: it fixes the transfer and discard limits the block layer works to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Class {
    /// 4 KiB pages only, so every request is one page.
    Small,
    /// 4 MiB pages only, so the block layer can be told to align to them.
    Huge,
    /// Both, so no single alignment holds and the cuts are ours to make.
    Mixed,
}

/// Where one block of a device lands: the page holding it, that page's size, and how far
/// into the page it is. A request is served one page at a time, so this is also where it
/// gets cut.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Place {
    /// The page's global address, which is what consensus names.
    pub(crate) addr: u64,
    /// A 4 MiB page: whole-page writes only, and reads may take a piece of it.
    pub(crate) huge: bool,
    /// Blocks from the start of the page to this one.
    pub(crate) off: u64,
}

impl Place {
    /// Blocks from this one to the end of its page: the most of a request one operation
    /// may take.
    pub(crate) fn run(&self) -> u64 {
        (if self.huge { HUGE_BLOCKS } else { 1 }) - self.off
    }

    /// Bytes from the start of the page to this block.
    pub(crate) fn byte_off(&self) -> usize {
        (self.off * SMALL_PAGE) as usize
    }
}

/// A local ublk block device: an ordered list of whole extents, concatenated. Nothing is
/// shared: hosts may build different devices from the same extents in different orders,
/// mount one twice, or not at all. A device may span universes; each page is still reached
/// over its own universe's fabric.
///
/// Page size varies within a device, so everything here counts in 4 KiB blocks, which is
/// also the logical block size the device is exported with. A 4 MiB extent is 1024 blocks
/// of one page, not one block.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Device {
    pub(crate) id: u32,
    spans: Vec<Span>,
    /// Prefix sums in blocks, `spans.len() + 1` long.
    starts: Vec<u64>,
}

impl Device {
    fn new(id: u32, spans: Vec<Span>) -> Device {
        let mut starts = Vec::with_capacity(spans.len() + 1);
        let mut at = 0;
        starts.push(0);
        for s in &spans {
            at += s.blocks();
            starts.push(at);
        }
        Device { id, spans, starts }
    }

    /// Length in 4 KiB blocks.
    pub(crate) fn blocks(&self) -> u64 {
        self.starts.last().copied().unwrap_or(0)
    }

    pub(crate) fn bytes(&self) -> u64 {
        self.blocks() * SMALL_PAGE
    }

    /// The page sizes this device is built from.
    pub(crate) fn class(&self) -> Class {
        let huge = self.spans.iter().any(|s| s.huge);
        let small = self.spans.iter().any(|s| !s.huge);
        match (huge, small) {
            (true, true) => Class::Mixed,
            (true, false) => Class::Huge,
            // An empty device has no huge page to align to, so it is small by default.
            (false, _) => Class::Small,
        }
    }

    /// Where block `lba` of this device lands, or `None` past the end.
    pub(crate) fn map(&self, lba: u64) -> Option<Place> {
        if lba >= self.blocks() {
            return None;
        }
        let i = self.starts.partition_point(|&s| s <= lba) - 1;
        let s = &self.spans[i];
        let off = lba - self.starts[i];
        let bpp = if s.huge { HUGE_BLOCKS } else { 1 };
        Some(Place {
            addr: addr_of(s.universe, s.base_lba + off / bpp * bpp),
            huge: s.huge,
            off: off % bpp,
        })
    }
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct Node {
    pub(crate) id: u32,
    pub(crate) zone: u32,
    /// 0..2, our cache cohort (cache.rs): the same catalog column in every universe.
    pub(crate) cohort: u8,
    /// The store file this node owns. From `RACER_STORE`: `load`/`parse` fill it in, not
    /// `from_pb`.
    pub store: PathBuf,
    /// The length that file is held at. Grown to on start, never shrunk.
    pub(crate) store_bytes: u64,
    /// The rate we drive the store at, zero for unmetered. Read once per IO; fixed after
    /// start.
    pub(crate) store_max_iops: u64,
    pub(crate) store_max_bytes_per_sec: u64,
}

/// One end of a fabric link, inside one universe. No address, port or NQN: the control
/// plane owns the nvmet target and initiator config, so a peer is already a local path.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Peer {
    pub(crate) id: u32,
    /// Local path to the peer's fabric namespace for this universe, already attached.
    pub(crate) device: String,
}

/// Another zone of a universe, and the nodes of it that take traffic from outside.
#[derive(Clone, Debug, PartialEq)]
struct Zone {
    id: u32,
    /// Non-empty. Ranked per address, so the count is a capacity choice, not a protocol
    /// shape.
    gateways: Box<[u32]>,
}

#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Policy {
    /// DRAM ceiling for the 4 KiB index. A config needing more is refused, not left to OOM.
    max_index_bytes: u64,
    /// DRAM ceiling for the OCC read pool across the whole node. Evicting a read record
    /// can only turn a success into a conflict, so this bounds memory, not correctness.
    pub(crate) occ_bytes: u64,
    /// DRAM ceiling for the read cache index across all cores and classes; its media is
    /// whatever the slabs left over. Not an admission check: the cache holds fewer chunks.
    pub(crate) cache_index_bytes: u64,
    /// Registers one anti-entropy sweep pulls per group replay, and pushes per extent while
    /// handing one over: the rate of member replacement and handover (heal.rs).
    pub(crate) repairs_per_replay: u32,
}

impl Default for Policy {
    fn default() -> Policy {
        Policy {
            max_index_bytes: 8 << 30,
            occ_bytes: 256 << 20,
            cache_index_bytes: 1 << 30,
            repairs_per_replay: 4096,
        }
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
    pub(crate) policy: Policy,
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

    /// 4 KiB pages this node must have room for.
    pub(crate) fn small_pages(&self) -> u64 {
        self.count_pages(false)
    }

    /// 4 MiB pages this node must have room for.
    pub(crate) fn huge_pages(&self) -> u64 {
        self.count_pages(true)
    }

    /// This node's share of one page class over all its universes. The catalog is balanced,
    /// so each node of a zone holds an equal share of three replicas; shares add up.
    fn count_pages(&self, huge: bool) -> u64 {
        self.universes
            .iter()
            .map(|u| {
                let n = u.zone_nodes().len().max(1) as u64;
                (u.zone_pages(huge, self.node.zone) * 3).div_ceil(n)
            })
            .sum()
    }

    /// The consensus group an address belongs to. An address in a universe we hold no
    /// catalog for still yields a well-formed id; it is `members` that refuses it.
    pub fn group(&self, addr: u64) -> GroupId {
        let n = self.universe_at(addr).map_or(0, |u| u.catalog.len()) as u32;
        let index = if n == 0 { 0 } else { slot_of(addr) as u32 % n };
        GroupId::new(universe_of(addr), index)
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
        self.extent_at(addr).map_or(0, |e| e.tombstone_epoch)
    }

    /// The cache admission threshold of `addr`'s extent (cache.rs): 0 never admits, 1 on
    /// first sight, `n` once the demand estimate reaches `n`. An address in no extent of
    /// ours is never cached, so an extent leaving this config stops admission at once.
    pub(crate) fn cache_admit_of(&self, addr: u64) -> u8 {
        self.extent_at(addr).map_or(0, |e| e.cache_admit)
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

    /// Whether any extent has ever collected; if not, a tombstone sweep is pointless.
    pub(crate) fn collecting(&self) -> bool {
        self.extents().any(|(_, e)| e.tombstone_epoch != 0)
    }

    /// Every check that does not need the previous configuration.
    pub fn validate(&self) -> io::Result<()> {
        self.check_node()?;
        self.check_universes()?;
        self.check_devices()?;
        self.check_exports()?;
        if self.index.len() > MAX_EXTENTS {
            return Err(bad(format!(
                "{} extents, more than the {MAX_EXTENTS} this node can hold",
                self.index.len()
            )));
        }
        let index_bytes = self.small_pages() * crate::alloc::INDEX_BYTES_PER_PAGE;
        if index_bytes > self.policy.max_index_bytes {
            return Err(bad(format!(
                "the 4 KiB index would need {index_bytes} bytes, over the {} allowed",
                self.policy.max_index_bytes
            )));
        }
        if self.policy.repairs_per_replay == 0 {
            return Err(bad(
                "repairs_per_replay is zero, so a replay never finishes",
            ));
        }
        Ok(())
    }

    fn check_node(&self) -> io::Result<()> {
        if self.node.id == 0 {
            return Err(bad("node id is zero"));
        }
        if self.node.zone == 0 {
            return Err(bad("node zone is zero"));
        }
        if self.node.store.as_os_str().is_empty() {
            return Err(bad("node has no store path"));
        }
        if self.node.store_bytes == 0 {
            return Err(bad("node store size is zero"));
        }
        if !self.node.store_bytes.is_multiple_of(SMALL_PAGE) {
            return Err(bad(format!(
                "node store size {} is not a multiple of {SMALL_PAGE}",
                self.node.store_bytes
            )));
        }
        Ok(())
    }

    fn check_universes(&self) -> io::Result<()> {
        if self.universes.is_empty() {
            return Err(bad("node is in no universe"));
        }
        let mut paths: Vec<&str> = Vec::new();
        for (i, u) in self.universes.iter().enumerate() {
            if u.id == 0 {
                return Err(bad("universe id is zero"));
            }
            if u.id >= MAX_UNIVERSE {
                return Err(bad(format!(
                    "universe {} is at or above the {MAX_UNIVERSE} an address can name",
                    u.id
                )));
            }
            if i > 0 && self.universes[i - 1].id == u.id {
                return Err(bad(format!("universe {} appears twice", u.id)));
            }
            self.check_universe(u)?;
            for p in &u.peers {
                paths.push(&p.device);
            }
        }
        paths.sort_unstable();
        if let Some(w) = paths.windows(2).find(|w| w[0] == w[1]) {
            return Err(bad(format!(
                "namespace {} is named by two peers; a namespace belongs to one universe",
                w[0]
            )));
        }
        Ok(())
    }

    fn check_universe(&self, u: &Universe) -> io::Result<()> {
        let id = u.id;
        if u.catalog.is_empty() {
            return Err(bad(format!("universe {id} has an empty catalog")));
        }
        for (g, m) in u.catalog.iter().enumerate() {
            if m.contains(&0) {
                return Err(bad(format!("universe {id} group {g} names node 0")));
            }
            if m[0] == m[1] || m[0] == m[2] || m[1] == m[2] {
                return Err(bad(format!(
                    "universe {id} group {g} names {:?}, which is not three distinct nodes",
                    m
                )));
            }
        }
        // Homogeneity: each named node holds the same number of groups, so it can size its
        // store from the zone's total and the node count alone.
        let nodes = u.zone_nodes();
        let slots = 3 * u.catalog.len();
        if !slots.is_multiple_of(nodes.len()) {
            return Err(bad(format!(
                "universe {id} spreads {slots} group slots over {} nodes, which cannot be even",
                nodes.len()
            )));
        }
        let want = slots / nodes.len();
        for n in &nodes {
            let held = u.catalog.iter().flatten().filter(|&m| m == n).count();
            if held != want {
                return Err(bad(format!(
                    "universe {id} gives node {n} {held} groups, not the {want} every node holds"
                )));
            }
        }
        for (i, z) in u.zones.iter().enumerate() {
            if z.id == 0 {
                return Err(bad(format!("universe {id} names zone 0")));
            }
            if z.id == self.node.zone {
                return Err(bad(format!(
                    "universe {id} lists our own zone {} among the others",
                    z.id
                )));
            }
            if u.zones[..i].iter().any(|o| o.id == z.id) {
                return Err(bad(format!("universe {id} names zone {} twice", z.id)));
            }
            // Not checked: whether we hold a link to any of them. A node may hear of a zone
            // before its namespaces are attached, and a routing-only node never holds one;
            // both fail at runtime with `EIO`.
            if z.gateways.is_empty() {
                return Err(bad(format!(
                    "universe {id} zone {} names no gateways, so nothing can reach it",
                    z.id
                )));
            }
            if z.gateways.len() > MAX_GATEWAYS {
                return Err(bad(format!(
                    "universe {id} zone {} names {} gateways, above the {MAX_GATEWAYS} allowed",
                    z.id,
                    z.gateways.len()
                )));
            }
            for (j, &g) in z.gateways.iter().enumerate() {
                if g == 0 {
                    return Err(bad(format!("universe {id} zone {} names gateway 0", z.id)));
                }
                if g == self.node.id {
                    return Err(bad(format!(
                        "universe {id} zone {} names this node as one of its gateways",
                        z.id
                    )));
                }
                if z.gateways[..j].contains(&g) {
                    return Err(bad(format!(
                        "universe {id} zone {} names gateway {g} twice",
                        z.id
                    )));
                }
            }
        }
        for (i, p) in u.peers.iter().enumerate() {
            if p.id == 0 {
                return Err(bad(format!("universe {id} has a peer with id 0")));
            }
            if p.id == self.node.id {
                return Err(bad(format!("universe {id} names this node as a peer")));
            }
            if p.device.is_empty() {
                return Err(bad(format!("universe {id} peer {} has no device", p.id)));
            }
            if u.peers[..i].iter().any(|o| o.id == p.id) {
                return Err(bad(format!("universe {id} names peer {} twice", p.id)));
            }
        }
        let mut end = 0u64;
        for e in &u.extents {
            if e.id == 0 {
                return Err(bad(format!("universe {id} has an extent with id 0")));
            }
            if e.pages == 0 {
                return Err(bad(format!("extent {} is empty", e.id)));
            }
            if e.zone == 0 {
                return Err(bad(format!("extent {} is in zone 0", e.id)));
            }
            if e.next_zone == e.zone {
                return Err(bad(format!(
                    "extent {} is migrating to the zone it is already in",
                    e.id
                )));
            }
            if !u.known_zone(e.zone, self.node.zone) {
                return Err(bad(format!(
                    "extent {} is in zone {}, which universe {id} does not name",
                    e.id, e.zone
                )));
            }
            if e.next_zone != 0 && !u.known_zone(e.next_zone, self.node.zone) {
                return Err(bad(format!(
                    "extent {} is migrating to zone {}, which universe {id} does not name",
                    e.id, e.next_zone
                )));
            }
            // A warmed copy is read without a confirmation round, the round trip warming
            // exists to avoid. Only an immutable page can be believed on sight: its version
            // is a function of the tombstone epoch, so a copy is live or visibly not.
            if !e.warm_zones.is_empty() && e.kind != Kind::Immutable {
                return Err(bad(format!(
                    "extent {} asks to warm other zones, which only an immutable extent may: \
                     a {:?} page carries no version a remote reader could trust on sight",
                    e.id, e.kind
                )));
            }
            if e.warm_zones.len() > MAX_WARM_ZONES {
                return Err(bad(format!(
                    "extent {} warms {} zones, above the {MAX_WARM_ZONES} allowed",
                    e.id,
                    e.warm_zones.len()
                )));
            }
            for (j, &w) in e.warm_zones.iter().enumerate() {
                if !u.known_zone(w, self.node.zone) {
                    return Err(bad(format!(
                        "extent {} warms zone {w}, which universe {id} does not name",
                        e.id
                    )));
                }
                // The home zone holds the pages already, and a migration destination is
                // being sent every page and is about to become the home.
                if w == e.zone || w == e.next_zone {
                    return Err(bad(format!(
                        "extent {} warms zone {w}, which already holds its pages",
                        e.id
                    )));
                }
                if e.warm_zones[..j].contains(&w) {
                    return Err(bad(format!("extent {} warms zone {w} twice", e.id)));
                }
            }
            if e.huge && e.base_lba % HUGE_BLOCKS != 0 {
                return Err(bad(format!(
                    "extent {} starts at block {}, which is not a 4 MiB boundary",
                    e.id, e.base_lba
                )));
            }
            if e.base_lba < end {
                return Err(bad(format!(
                    "extent {} starts at block {}, inside the extent before it",
                    e.id, e.base_lba
                )));
            }
            if e.blocks() > MAX_LBA - e.base_lba {
                return Err(bad(format!(
                    "extent {} runs past the end of universe {id}",
                    e.id
                )));
            }
            end = e.end_lba();
        }
        Ok(())
    }

    fn check_devices(&self) -> io::Result<()> {
        for (i, d) in self.devices.iter().enumerate() {
            if d.id == 0 {
                return Err(bad("device id is zero"));
            }
            if i > 0 && self.devices[i - 1].id == d.id {
                return Err(bad(format!("device {} appears twice", d.id)));
            }
            if d.spans.is_empty() {
                return Err(bad(format!("device {} maps no extents", d.id)));
            }
            for (j, s) in d.spans.iter().enumerate() {
                if d.spans[..j].iter().any(|o| o.extent == s.extent) {
                    return Err(bad(format!(
                        "device {} maps extent {} twice",
                        d.id, s.extent
                    )));
                }
            }
        }
        Ok(())
    }

    /// The minors this node asks the kernel for. Every universe exports its fabric
    /// namespace and every device exports itself, each as the ublk device named by the id
    /// given here, so the paths follow from the config alone. Two exports asking for one
    /// minor is caught now rather than half way through attaching.
    fn check_exports(&self) -> io::Result<()> {
        let mut ids: Vec<(u32, String)> =
            Vec::with_capacity(self.universes.len() + self.devices.len());
        for u in &self.universes {
            if u.fabric_device_id == 0 {
                return Err(bad(format!(
                    "universe {} names no device for its fabric namespace",
                    u.id
                )));
            }
            ids.push((u.fabric_device_id, format!("universe {} fabric", u.id)));
        }
        for d in &self.devices {
            ids.push((d.id, format!("device {}", d.id)));
        }
        ids.sort();
        if let Some(w) = ids.windows(2).find(|w| w[0].0 == w[1].0) {
            return Err(bad(format!(
                "{} and {} both ask to be device {}",
                w[0].1, w[1].1, w[0].0
            )));
        }
        if ids.len() > MAX_EXPORTS {
            return Err(bad(format!(
                "{} universes plus {} devices is more than the {MAX_EXPORTS} this node can export",
                self.universes.len(),
                self.devices.len()
            )));
        }
        Ok(())
    }

    /// The checks that need the configuration this one replaces. Everything frozen here
    /// is frozen because the dataplane has already built something around it.
    fn validate_against(&self, prev: &Config) -> io::Result<()> {
        if self.generation <= prev.generation {
            return Err(bad(format!(
                "generation {} does not advance on {}",
                self.generation, prev.generation
            )));
        }
        if self.node.store_bytes < prev.node.store_bytes {
            return Err(bad(format!(
                "store size {} is below the {} already formatted",
                self.node.store_bytes, prev.node.store_bytes
            )));
        }
        for pu in &prev.universes {
            let Some(u) = self.universe(pu.id) else {
                continue;
            };
            if u.catalog.len() != pu.catalog.len() {
                return Err(bad(format!(
                    "universe {} changes from {} groups to {}",
                    u.id,
                    pu.catalog.len(),
                    u.catalog.len()
                )));
            }
            // The fabric namespace is already exported at this minor and peers hold the
            // path, so moving it would strand every link into this universe.
            if u.fabric_device_id != pu.fabric_device_id {
                return Err(bad(format!(
                    "universe {} moves its fabric from device {} to {}",
                    u.id, pu.fabric_device_id, u.fabric_device_id
                )));
            }
            // Only across one generation: a node that missed a push cannot tell how many
            // steps a catalog took, so it refuses to guess.
            if self.generation == prev.generation + 1 {
                let (was, now) = (pu.zone_nodes(), u.zone_nodes());
                let joined = now.iter().filter(|n| !was.contains(n)).count();
                let left = was.iter().filter(|n| !now.contains(n)).count();
                if joined > 1 || left > 1 {
                    return Err(bad(format!(
                        "universe {} moves {joined} nodes in and {left} out at once",
                        u.id
                    )));
                }
            }
        }
        for (pu, pe) in prev.extents() {
            let Some((u, e)) = self.extent_by_id(pe.id) else {
                continue;
            };
            if u.id != pu.id {
                return Err(bad(format!(
                    "extent {} moves from universe {} to {}",
                    pe.id, pu.id, u.id
                )));
            }
            self.check_replacement(pe, e)?;
        }
        for pd in &prev.devices {
            let Some(d) = self.devices.iter().find(|d| d.id == pd.id) else {
                continue;
            };
            let was: Vec<u32> = pd.spans.iter().map(|s| s.extent).collect();
            let now: Vec<u32> = d.spans.iter().map(|s| s.extent).collect();
            if was != now {
                return Err(bad(format!(
                    "device {} changes from extents {was:?} to {now:?}",
                    d.id
                )));
            }
        }
        Ok(())
    }

    /// What one extent may change into. Shape is frozen because the allocator placed pages
    /// by it; placement may move only along the migration the previous config declared.
    fn check_replacement(&self, old: &Extent, new: &Extent) -> io::Result<()> {
        if new.base_lba != old.base_lba {
            return Err(bad(format!(
                "extent {} moves from block {} to {}",
                old.id, old.base_lba, new.base_lba
            )));
        }
        if new.pages != old.pages {
            return Err(bad(format!(
                "extent {} changes from {} pages to {}",
                old.id, old.pages, new.pages
            )));
        }
        if new.kind != old.kind || new.huge != old.huge {
            return Err(bad(format!(
                "extent {} changes from {:?} to {:?}",
                old.id,
                (old.kind, old.huge),
                (new.kind, new.huge)
            )));
        }
        if new.tombstone_epoch < old.tombstone_epoch {
            return Err(bad(format!(
                "extent {} rewinds its tombstone epoch from {} to {}",
                old.id, old.tombstone_epoch, new.tombstone_epoch
            )));
        }
        if new.zone != old.zone && new.zone != old.next_zone {
            return Err(bad(format!(
                "extent {} moves from zone {} to {} without having been migrating there",
                old.id, old.zone, new.zone
            )));
        }
        Ok(())
    }

    /// The structural checks: what a `Config` cannot represent at all, unlike `validate`.
    fn from_pb(p: pb::NodeConfig) -> io::Result<Config> {
        let n = p.node.unwrap_or_default();
        let node = Node {
            id: n.id,
            zone: n.zone,
            cohort: n.cohort.ok_or_else(|| bad("node names no cohort"))? as u8,
            store: PathBuf::new(),
            store_bytes: n.store.as_ref().map_or(0, |s| s.size_bytes),
            store_max_iops: n.store.as_ref().map_or(0, |s| s.max_iops),
            store_max_bytes_per_sec: n.store.as_ref().map_or(0, |s| s.max_bytes_per_sec),
        };
        let mut universes = Vec::with_capacity(p.universes.len());
        for u in p.universes {
            let mut extents = Vec::with_capacity(u.extents.len());
            for e in u.extents {
                let k = pb::Kind::try_from(e.kind)
                    .map_err(|_| bad(format!("extent {} has unknown kind {}", e.id, e.kind)))?;
                let (kind, huge) = split_kind(k);
                if e.cache_admit > CACHE_MAX_ADMIT {
                    return Err(bad(format!(
                        "extent {} asks for cache_admit {}, above the {CACHE_MAX_ADMIT} the cache can observe",
                        e.id, e.cache_admit
                    )));
                }
                extents.push(Extent {
                    id: e.id,
                    base_lba: e.base_lba,
                    pages: e.pages,
                    kind,
                    huge,
                    zone: e.zone,
                    next_zone: e.next_zone,
                    tombstone_epoch: e.tombstone_epoch as u64,
                    cache_admit: e.cache_admit as u8,
                    warm_zones: e.warm_zones.into_boxed_slice(),
                });
            }
            extents.sort_by_key(|e| e.base_lba);
            universes.push(Universe {
                id: u.id,
                epoch: u.epoch,
                fabric_device_id: u.fabric_device_id,
                catalog: u.catalog.iter().map(trio).collect(),
                zones: u
                    .zones
                    .iter()
                    .map(|z| Zone {
                        id: z.id,
                        gateways: z.gateways.clone().into_boxed_slice(),
                    })
                    .collect(),
                peers: u
                    .peers
                    .into_iter()
                    .map(|p| Peer {
                        id: p.id,
                        device: p.device,
                    })
                    .collect(),
                extents,
            });
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
        for d in &p.devices {
            let mut spans = Vec::with_capacity(d.extents.len());
            for &id in &d.extents {
                let (u, e) = find(id)
                    .ok_or_else(|| bad(format!("device {} maps unknown extent {id}", d.id)))?;
                spans.push(Span {
                    universe: u.id,
                    extent: e.id,
                    base_lba: e.base_lba,
                    pages: e.pages,
                    huge: e.huge,
                });
            }
            devices.push(Device::new(d.id, spans));
        }
        devices.sort_by_key(|d| d.id);
        let policy = p.policy.map(|p| Policy {
            max_index_bytes: p
                .max_index_bytes
                .unwrap_or(Policy::default().max_index_bytes),
            occ_bytes: p.occ_bytes.unwrap_or(Policy::default().occ_bytes),
            cache_index_bytes: p
                .cache_index_bytes
                .unwrap_or(Policy::default().cache_index_bytes),
            repairs_per_replay: p
                .repairs_per_replay
                .unwrap_or(Policy::default().repairs_per_replay),
        });
        Ok(Config {
            generation: p.generation,
            node,
            universes,
            devices,
            policy: policy.unwrap_or_default(),
            index,
        })
    }

    fn to_pb(&self) -> pb::NodeConfig {
        pb::NodeConfig {
            generation: self.generation,
            node: Some(pb::Node {
                id: self.node.id,
                zone: self.node.zone,
                cohort: Some(self.node.cohort as i32),
                store: Some(pb::Store {
                    size_bytes: self.node.store_bytes,
                    max_iops: self.node.store_max_iops,
                    max_bytes_per_sec: self.node.store_max_bytes_per_sec,
                }),
            }),
            universes: self
                .universes
                .iter()
                .map(|u| pb::Universe {
                    id: u.id,
                    epoch: u.epoch,
                    fabric_device_id: u.fabric_device_id,
                    catalog: u.catalog.iter().map(pb_trio).collect(),
                    zones: u
                        .zones
                        .iter()
                        .map(|z| pb::Zone {
                            id: z.id,
                            gateways: z.gateways.to_vec(),
                        })
                        .collect(),
                    peers: u
                        .peers
                        .iter()
                        .map(|p| pb::Peer {
                            id: p.id,
                            device: p.device.clone(),
                        })
                        .collect(),
                    extents: u
                        .extents
                        .iter()
                        .map(|e| pb::Extent {
                            id: e.id,
                            base_lba: e.base_lba,
                            pages: e.pages,
                            kind: join_kind(e.kind, e.huge) as i32,
                            zone: e.zone,
                            next_zone: e.next_zone,
                            tombstone_epoch: e.tombstone_epoch as u32,
                            cache_admit: e.cache_admit as u32,
                            warm_zones: e.warm_zones.to_vec(),
                        })
                        .collect(),
                })
                .collect(),
            devices: self
                .devices
                .iter()
                .map(|d| pb::Device {
                    id: d.id,
                    extents: d.spans.iter().map(|s| s.extent).collect(),
                })
                .collect(),
            policy: Some(pb::Policy {
                max_index_bytes: Some(self.policy.max_index_bytes),
                occ_bytes: Some(self.policy.occ_bytes),
                cache_index_bytes: Some(self.policy.cache_index_bytes),
                repairs_per_replay: Some(self.policy.repairs_per_replay),
            }),
        }
    }

    /// A human-writable spelling of the same schema, for tests and reading by eye.
    /// Line-oriented; `peer`, `group`, `zone` and `extent` bind to the `universe` above.
    ///
    /// ```text
    /// generation 7
    /// node id=1 zone=1 cohort=0 store=/var/lib/racer/store.img size=68719476736
    /// universe 1 epoch=3 fabric_device_id=9
    ///   peer id=2 device=/dev/nvme1n1
    ///   group 1 2 3
    ///   zone id=2 gateways=4,5,6
    ///   extent id=10 base=0    pages=4096 kind=lww zone=1 cache_admit=2
    ///   extent id=11 base=4096 pages=512  kind=occ zone=1
    /// device 1 extents=10,11
    /// ```
    pub fn parse(text: &str) -> io::Result<Config> {
        let mut p = pb::NodeConfig::default();
        let mut store: Option<PathBuf> = None;
        for (n, line) in text.lines().enumerate() {
            let line = line.split('#').next().unwrap_or("").trim();
            if line.is_empty() {
                continue;
            }
            let parts: Vec<&str> = line.split_whitespace().collect();
            let (key, rest) = (parts[0], &parts[1..]);
            let f = fields(rest);
            let at = |e: io::Error| bad(format!("line {}: {e}", n + 1));
            match key {
                "generation" => p.generation = num(rest, 0).map_err(at)?,
                "node" => {
                    let f = only(
                        &f,
                        &[
                            "id", "zone", "cohort", "store", "size", "max_iops", "max_bps",
                        ],
                    )
                    .map_err(at)?;
                    // Absent is not empty: with no `store=` the path falls back to the env.
                    store = text_field(f, "store").ok().map(PathBuf::from);
                    p.node = Some(pb::Node {
                        id: get(f, "id").map_err(at)? as u32,
                        zone: get(f, "zone").map_err(at)? as u32,
                        cohort: Some(get_or(f, "cohort", 0).map_err(at)? as i32),
                        store: Some(pb::Store {
                            size_bytes: get(f, "size").map_err(at)?,
                            max_iops: get_or(f, "max_iops", 0).map_err(at)?,
                            max_bytes_per_sec: get_or(f, "max_bps", 0).map_err(at)?,
                        }),
                    });
                }
                "universe" => {
                    let f = only(&f, &["epoch", "fabric_device_id"]).map_err(at)?;
                    p.universes.push(pb::Universe {
                        id: num(rest, 0).map_err(at)? as u32,
                        epoch: get_or(f, "epoch", 0).map_err(at)? as u32,
                        fabric_device_id: get_or(f, "fabric_device_id", 0).map_err(at)? as u32,
                        ..Default::default()
                    });
                }
                "peer" => {
                    let f = only(&f, &["id", "device"]).map_err(at)?;
                    let peer = pb::Peer {
                        id: get(f, "id").map_err(at)? as u32,
                        device: text_field(f, "device").map_err(at)?,
                    };
                    last(&mut p, key).map_err(at)?.peers.push(peer);
                }
                "group" => {
                    let t = ids(rest).and_then(as_trio).map_err(at)?;
                    last(&mut p, key).map_err(at)?.catalog.push(t);
                }
                "zone" => {
                    let f = only(&f, &["id", "gateways"]).map_err(at)?;
                    let z = pb::Zone {
                        id: get(f, "id").map_err(at)? as u32,
                        gateways: list(f, "gateways").map_err(at)?,
                    };
                    last(&mut p, key).map_err(at)?.zones.push(z);
                }
                "extent" => {
                    let f = only(
                        &f,
                        &[
                            "id",
                            "base",
                            "pages",
                            "kind",
                            "zone",
                            "next_zone",
                            "tombstone_epoch",
                            "cache_admit",
                            "warm_zones",
                        ],
                    )
                    .map_err(at)?;
                    let e = pb::Extent {
                        id: get(f, "id").map_err(at)? as u32,
                        base_lba: get(f, "base").map_err(at)?,
                        pages: get(f, "pages").map_err(at)?,
                        kind: named(f, "kind", "LWW", pb::Kind::from_str_name).map_err(at)? as i32,
                        zone: get(f, "zone").map_err(at)? as u32,
                        next_zone: get_or(f, "next_zone", 0).map_err(at)? as u32,
                        tombstone_epoch: get_or(f, "tombstone_epoch", 0).map_err(at)? as u32,
                        cache_admit: get_or(f, "cache_admit", 0).map_err(at)? as u32,
                        warm_zones: list_or(f, "warm_zones").map_err(at)?,
                    };
                    last(&mut p, key).map_err(at)?.extents.push(e);
                }
                "device" => {
                    let f = only(&f, &["extents"]).map_err(at)?;
                    p.devices.push(pb::Device {
                        id: num(rest, 0).map_err(at)? as u32,
                        extents: list(f, "extents").map_err(at)?,
                    });
                }
                "policy" => {
                    let f = only(
                        &f,
                        &[
                            "max_index_bytes",
                            "occ_bytes",
                            "cache_index_bytes",
                            "repairs_per_replay",
                        ],
                    )
                    .map_err(at)?;
                    p.policy = Some(pb::Policy {
                        max_index_bytes: opt(f, "max_index_bytes").map_err(at)?,
                        occ_bytes: opt(f, "occ_bytes").map_err(at)?,
                        cache_index_bytes: opt(f, "cache_index_bytes").map_err(at)?,
                        repairs_per_replay: opt(f, "repairs_per_replay")
                            .map_err(at)?
                            .map(|v| v as u32),
                    });
                }
                other => return Err(bad(format!("line {}: unknown key {other}", n + 1))),
            }
        }
        Config::from_pb(p).map(|mut c| {
            c.node.store = store.unwrap_or_else(store_path);
            c
        })
    }
}

/// The universe the line being parsed belongs to: whichever `universe` line came last.
fn last<'a>(p: &'a mut pb::NodeConfig, key: &str) -> io::Result<&'a mut pb::Universe> {
    p.universes
        .last_mut()
        .ok_or_else(|| bad(format!("{key} before universe")))
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

/// Reject an unknown field rather than ignoring it: a silent default would mis-run the
/// node.
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

/// A catalog group, the one place three is still the shape: position is the paxos member
/// index and the cohort column, so "not three" is not a state the model considers.
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

/// An enum by name, case-insensitively; `from` is prost's lookup, so the two cannot drift.
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

/// [`list`], but an absent field is an empty list rather than an error.
fn list_or(f: &[(&str, &str)], k: &str) -> io::Result<Vec<u32>> {
    match f.iter().any(|(a, _)| *a == k) {
        true => list(f, k),
        false => Ok(Vec::new()),
    }
}

/// The group slot an address hashes into. A pure function of the address, so two zones
/// agree on it without sharing a slot table.
fn slot_of(addr: u64) -> u16 {
    (mix(addr) % SLOTS as u64) as u16
}

/// Three node ids in cohort order; named fields on the wire, so "not three" cannot occur.
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

// --- Delivery ---

/// An inotify watch on the *directory* holding the config file: delivery is a `rename(2)`
/// over the path, and a watch on the file would hold the old inode and go deaf after the
/// first push. `IN_MOVED_TO` is the rename landing, `IN_CLOSE_WRITE` an operator editing
/// in place.
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
            // The whole read is consumed before returning: a leftover event is never
            // reported again, and may be the only notice of a write that lands after the
            // caller reloaded.
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

/// Watch `path` and hand each accepted configuration to `apply`; never returns if healthy.
/// A config failing validation is rejected wholesale and counted; the node keeps the one it
/// has. `apply` failing is the same case, since the runtime rolled its own build back, so
/// `current` only advances once the new config is live.
pub fn watch(
    path: &Path,
    mut current: Config,
    mut apply: impl FnMut(Config) -> io::Result<()>,
) -> io::Result<()> {
    let w = Watch::new(path)?;
    // inotify reports nothing from before the watch existed, and the caller loaded
    // `current` before this thread ran, so a config published in that window would be lost.
    // Read the file here instead. Draining before every read keeps the two in step: a
    // dropped event can only announce a file this read is about to see, and one this read
    // misses is still queued for the loop below.
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
  extent id=10 base=0     pages=100 kind=lww           zone=1 cache_admit=3
  extent id=11 base=100   pages=50  kind=occ           zone=1
  extent id=12 base=1024  pages=8   kind=immutable_4m  zone=1 cache_admit=1 warm_zones=2
  extent id=13 base=16384 pages=4   kind=lww           zone=2

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

    /// The 4 KiB page at `lba`, landed on exactly.
    fn small(u: u32, lba: u64) -> Place {
        Place {
            addr: at(u, lba),
            huge: false,
            off: 0,
        }
    }

    /// Block `off` of the 4 MiB page based at `lba`.
    fn huge(u: u32, lba: u64, off: u64) -> Place {
        Place {
            addr: at(u, lba),
            huge: true,
            off,
        }
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
        // 150 small pages and 8 huge ones in our zone, three replicas over six nodes.
        assert_eq!(c.small_pages(), 75);
        assert_eq!(c.huge_pages(), 4);
        assert_eq!(c.extent_at(at(1, 0)).unwrap().id, 10);
        assert_eq!(c.extent_at(at(1, 99)).unwrap().id, 10);
        assert_eq!(c.extent_at(at(1, 100)).unwrap().kind, Kind::Occ);
        assert!(c.extent_at(at(1, 150)).is_none(), "the gap is unmapped");
        assert!(c.extent_at(at(1, 1024)).unwrap().huge);
        assert_eq!(c.extent_at(at(1, 1024 + 1023)).unwrap().id, 12);
        assert!(c.extent_at(at(2, 0)).is_none(), "universe 2 does not exist");
        assert_eq!(c.extent_by_id(12).unwrap().0.id, 1);
        assert!(c.extent_by_id(99).is_none());
    }

    /// A device concatenates whole extents; its block numbering is its own.
    #[test]
    fn a_device_concatenates_whole_extents() {
        let c = sample();
        let d = c.devices.iter().find(|d| d.id == 1).unwrap();
        assert_eq!(d.blocks(), 150);
        assert_eq!(d.bytes(), 150 * 4096);
        assert_eq!(d.class(), Class::Small);
        assert_eq!(d.map(0), Some(small(1, 0)));
        assert_eq!(d.map(99), Some(small(1, 99)));
        assert_eq!(
            d.map(100),
            Some(small(1, 100)),
            "the second extent starts here"
        );
        assert_eq!(d.map(149), Some(small(1, 149)));
        assert_eq!(d.map(150), None);

        let h = c.devices.iter().find(|d| d.id == 2).unwrap();
        assert_eq!(h.class(), Class::Huge);
        // Eight 4 MiB pages, addressed in 4 KiB blocks like every other device.
        assert_eq!(h.blocks(), 8 * 1024);
        assert_eq!(h.bytes(), 8 * (4 << 20));
        assert_eq!(h.map(0), Some(huge(1, 1024, 0)));
        assert_eq!(
            h.map(1),
            Some(huge(1, 1024, 1)),
            "still the first page, one block in"
        );
        assert_eq!(h.map(1024), Some(huge(1, 1024 + 1024, 0)));
        assert_eq!(h.map(8 * 1024), None);
    }

    /// The same extents may compose into different devices, in any order, and repeat.
    #[test]
    fn extents_compose_in_any_order_and_combination() {
        let c = Config::parse(&format!("{SAMPLE}device 3 extents=11,10\n")).unwrap();
        c.validate().unwrap();
        let d = c.devices.iter().find(|d| d.id == 3).unwrap();
        assert_eq!(d.blocks(), 150);
        assert_eq!(d.map(0), Some(small(1, 100)), "extent 11 is mounted first");
        assert_eq!(d.map(50), Some(small(1, 0)));
        assert_eq!(d.map(149), Some(small(1, 99)));
    }

    /// Page size belongs to the extent, so a device may hold both. The device is exported
    /// in 4 KiB blocks either way; what changes is where a request may be cut.
    #[test]
    fn a_device_may_mix_page_sizes() {
        let c = Config::parse(&format!("{SAMPLE}device 3 extents=10,12,11\n")).unwrap();
        c.validate().unwrap();
        let d = c.devices.iter().find(|d| d.id == 3).unwrap();
        assert_eq!(d.class(), Class::Mixed);
        assert_eq!(d.blocks(), 100 + 8 * 1024 + 50);
        assert_eq!(d.bytes(), (100 + 8 * 1024 + 50) * 4096);

        assert_eq!(d.map(99), Some(small(1, 99)), "the last small block");
        assert_eq!(d.map(100), Some(huge(1, 1024, 0)), "the huge extent starts");
        assert_eq!(d.map(100 + 1023), Some(huge(1, 1024, 1023)));
        assert_eq!(d.map(100 + 1024), Some(huge(1, 2048, 0)), "the next page");
        assert_eq!(
            d.map(100 + 8 * 1024),
            Some(small(1, 100)),
            "back to 4 KiB pages after the huge extent"
        );
        assert_eq!(d.map(100 + 8 * 1024 + 49), Some(small(1, 149)));
        assert_eq!(d.map(100 + 8 * 1024 + 50), None);
    }

    /// A request may only be served one page at a time, so the run left in the page is
    /// what tells the consumer path where to cut.
    #[test]
    fn a_place_bounds_what_one_operation_may_take() {
        let c = Config::parse(&format!("{SAMPLE}device 3 extents=10,12\n")).unwrap();
        let d = c.devices.iter().find(|d| d.id == 3).unwrap();
        assert_eq!(d.map(0).unwrap().run(), 1, "a 4 KiB page holds one block");
        assert_eq!(d.map(0).unwrap().byte_off(), 0);
        assert_eq!(d.map(100).unwrap().run(), 1024);
        assert_eq!(d.map(100 + 8).unwrap().run(), 1016);
        assert_eq!(d.map(100 + 8).unwrap().byte_off(), 8 * 4096);
        assert_eq!(d.map(100 + 1023).unwrap().run(), 1);
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
  extent id=20 base=0 pages=8 kind=lww zone=1
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
            c.devices.push(Device { id, ..one.clone() });
        }
        c.devices.sort_by_key(|d| d.id);
        c.validate().unwrap();

        c.devices.push(Device {
            id: 1000,
            ..one.clone()
        });
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
    fn an_extent_id_names_one_extent() {
        let mut text = SAMPLE.to_string();
        text.push_str("universe 2 epoch=1 fabric_device_id=10\n  group 1 2 3\n");
        text.push_str("  extent id=10 base=0 pages=8 kind=lww zone=1\n");
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
  extent id=20 base=0 pages=100 kind=lww zone=1
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
        let g1 = c.group(at(1, 0));
        let g2 = c.group(at(2, 0));
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
        assert_eq!(c.small_pages(), 75 + 100);
    }

    /// A namespace belongs to one universe; the same path in two would breach the boundary.
    #[test]
    fn a_namespace_belongs_to_one_universe() {
        let mut text = SAMPLE.to_string();
        text.push_str(
            "universe 2 epoch=1 fabric_device_id=10
  peer id=2 device=/dev/nvme1n1
  group 1 2 3
  extent id=20 base=0 pages=8 kind=lww zone=1
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
        c.node.store_bytes = 0;
        assert!(c.validate().is_err());

        let mut c = sample();
        c.node.store_bytes = 4097;
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
        c.node.store_bytes = b.node.store_bytes * 2;
        c.validate_against(&b).unwrap();

        let mut c = sample();
        c.generation = 8;
        c.node.store_bytes = b.node.store_bytes - 4096;
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
                "extent id=12 base=1024  pages=8   kind=immutable_4m  zone=1 cache_admit=1 warm_zones=2",
                line,
            );
            assert_ne!(t, SAMPLE, "the fixture line moved");
            Config::parse(&t).and_then(|c| c.validate())
        };
        let base = "extent id=12 base=1024 pages=8 kind=immutable_4m zone=1 cache_admit=1";
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
        for kind in ["lww", "occ"] {
            let t = SAMPLE.replace(
                "extent id=10 base=0     pages=100 kind=lww           zone=1 cache_admit=3",
                &format!(
                    "extent id=10 base=0 pages=100 kind={kind} zone=1 cache_admit=3 warm_zones=2"
                ),
            );
            assert_ne!(t, SAMPLE, "the fixture line moved");
            assert!(
                Config::parse(&t).and_then(|c| c.validate()).is_err(),
                "{kind} pages carry no version a remote reader could trust"
            );
        }
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
  extent id=12 base=1024 pages=8 kind=immutable_4m zone=1 cache_admit=1 warm_zones=2
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
        assert_eq!(c.small_pages(), 75);

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
            .map(|i| [i * 3 + 1, i * 3 + 2, i * 3 + 3])
            .collect();
        assert!(c.encode().len() < 100 << 10, "{}", c.encode().len());
    }

    #[test]
    fn shape_is_frozen_across_generations() {
        let b = sample();
        for f in [
            (|c: &mut Config| c.universes[0].extents[0].pages = 101) as fn(&mut Config),
            |c| c.universes[0].extents[0].base_lba = 4096,
            |c| c.universes[0].extents[0].kind = Kind::Occ,
            |c| c.universes[0].extents[2].huge = false,
            |c| c.universes[0].catalog.push([7, 8, 9]),
            |c| c.devices[0].spans[0].extent = 11,
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
        c.universes[0].catalog[0] = [7, 2, 3];
        c.validate_against(&b).unwrap();

        let mut c = sample();
        c.generation = 8;
        c.universes[0].catalog[0] = [7, 8, 3];
        assert!(c.validate_against(&b).is_err());

        // Two generations on, the same jump is not something we can second-guess.
        let mut c = sample();
        c.generation = 9;
        c.universes[0].catalog[0] = [7, 8, 3];
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
  extent id=1 base=0 pages=300 kind=lww zone=1
",
        )
        .unwrap();
        c.validate().unwrap();
        assert_eq!(c.small_pages(), 300, "three replicas over three nodes");
        assert_eq!(c.huge_pages(), 0);
    }

    #[test]
    fn an_unbalanced_catalog_is_refused() {
        let mut c = sample();
        c.universes[0].catalog = vec![[1, 2, 3], [1, 2, 4]];
        assert!(c.validate().is_err(), "node 3 holds fewer groups than 1");
        let mut c = sample();
        c.universes[0].catalog = vec![[1, 1, 2]];
        assert!(c.validate().is_err(), "a group needs three distinct nodes");
        let mut c = sample();
        c.universes[0].catalog.clear();
        assert!(c.validate().is_err());
    }

    #[test]
    fn tombstone_epoch_moves_forward_per_extent() {
        let mut b = sample();
        b.universes[0].extents[0].tombstone_epoch = 3;
        assert!(b.collecting());
        assert!(!sample().collecting());
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
        for n in [0u8, 1, 15] {
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

    #[test]
    fn live_swaps_without_disturbing_the_reader() {
        let live = Live::new(sample());
        assert_eq!(live.get().generation, 7);
        let mut next = sample();
        next.generation = 8;
        live.install(next);
        assert_eq!(live.get().generation, 8);
        assert!(live.prev.lock().unwrap().is_some());
        live.retire();
        assert!(live.prev.lock().unwrap().is_none());
        let mut third = sample();
        third.generation = 9;
        live.install(third);
        assert_eq!(live.get().generation, 9);
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

        // A duplicate announcement is refused, so wait for the count to settle before
        // taking the baseline.
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
        put(9, &|c| c.universes[0].extents[0].pages = 999);
        refused(2);
        put(10, &|_| {});
        assert_eq!(rx.recv_timeout(took).unwrap(), 10);
        refused(2);

        drop(t);
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
