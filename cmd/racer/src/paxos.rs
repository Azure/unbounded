// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! CASPaxos over the page register.
//!
//! One register per page, three acceptors, quorum two. There is no log: the register
//! *is* the allocator's mblock entry, so this file owns only the decisions — ballots
//! and terms, who may write, when a read may be believed, and how a divergent register
//! is repaired. Durability is `alloc.rs`, framing `fabric.rs`, placement the config.
//!
//! A version guard replaces the prepare phase: a proposer sends one message, and an
//! acceptor applies it only if the register still holds the version guarded on. Two
//! proposals racing the same guard cannot both reach two of three, so one round trip
//! is safe without Fast Paxos's larger quorums and the loser is rejected.
//!
//! What drives a migration — sealing an extent, then pushing it at the zone taking it
//! over — is `heal.rs`.

use std::cell::RefCell;
use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::alloc::{Allocator, GlobalAddr, Pending, Status};
use crate::cache::Cache;
use crate::config::{Kind, Live};
use crate::fabric::{self, Frame, Link, Op, Part};
use crate::layout;
use crate::runtime::{self, Buf, PoolBuf};

/// Trailer slot indices; `fabric.rs` carries the per-op table.
const T_GUARD: usize = 0;
const T_VERSION: usize = 0;
const T_BALLOT: usize = 1;
const T_EPOCH: usize = 2;
const T_TERM: usize = 2;
const T_SOURCE: usize = 2;
const T_STATE: usize = 2;
const T_SEAL_TERM: usize = 1;
const T_EXTENT: usize = 2;
/// The cache's replication width. It rides a slot of the trailer the metadata round
/// already carries, so the hint costs no extra command.
const T_WIDTH: usize = 3;
/// `LEARN` must validate or replace an equal-register small page.
const T_REPAIR: usize = 3;

/// How many times a LWW proposal re-derives its guard before giving up. A mismatch is
/// retried here rather than reported, so the client only ever sees last-write-wins;
/// the bound keeps a pathologically hot page from spinning.
const LWW_RETRIES: u32 = 4;

/// The `T_GUARD` value that means "you derive it". Sent by a non-member forwarding a
/// LWW write, which has no observation of its own to guard on.
const DERIVED: u64 = u64::MAX;

/// How many times a prepare round is re-run when it cannot tell which of two values at
/// the top version was chosen. That is a race with a concurrent proposer, and the
/// member that would decide it is the one that stayed silent; another round usually
/// reaches it.
const PREPARE_RETRIES: u32 = 4;

// ---------------------------------------------------------------------------
// ballots and registers
// ---------------------------------------------------------------------------

/// A CASPaxos ballot: `term` in the high 30 bits, proposer index in the low two.
///
/// On the one-shot path a ballot only proves the proposer holds the group's standing
/// promise and separates two values at the same version; classical ordering is needed
/// only underneath a prepare. Two bits are enough for the proposer because they need
/// only separate three members at one term, and a membership change bumps the term.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Default)]
pub struct Ballot(u32);

impl std::fmt::Debug for Ballot {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}.{}", self.term(), self.member())
    }
}

impl Ballot {
    pub const ZERO: Ballot = Ballot(0);

    pub fn new(term: u32, member: u8) -> Ballot {
        Ballot((term & 0x3fff_ffff) << 2 | (member as u32 & 3))
    }

    pub fn from_raw(v: u32) -> Ballot {
        Ballot(v)
    }

    pub fn raw(self) -> u32 {
        self.0
    }

    fn term(self) -> u32 {
        self.0 >> 2
    }

    fn member(self) -> u8 {
        (self.0 & 3) as u8
    }
}

/// What an acceptor holds for one page. Both fields live in the mblock entry, and an
/// Immutable page's state is derived from the version, so this is the whole of the
/// durable consensus state.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Register {
    pub version: u64,
    pub ballot: Ballot,
}

impl Register {
    /// The apply-if-newer order. Ballot breaks ties at one version, which is what two
    /// racing one-shot proposals produce.
    fn key(self) -> (u64, u32) {
        (self.version, self.ballot.raw())
    }
}

/// The bytes a proposal carries. A 4 KiB page is staged through our own registered
/// memory because the allocator checksums it; a 4 MiB page is the caller's buffer and
/// is never copied.
#[derive(Clone, Copy)]
pub enum Page<'a> {
    Small(&'a PoolBuf),
    Huge(Buf),
}

/// Where a read delivers. The mirror of [`Page`], separate for the same reason: a
/// 4 KiB page has to land somewhere we can checksum it, a 4 MiB page is DMA'd straight
/// into the caller's registered buffer.
pub enum Sink<'a> {
    Small(&'a mut PoolBuf),
    Huge(Buf),
}

impl Sink<'_> {
    fn reborrow(&mut self) -> Sink<'_> {
        match self {
            Sink::Small(p) => Sink::Small(p),
            Sink::Huge(b) => Sink::Huge(*b),
        }
    }

    fn huge(&self) -> bool {
        matches!(self, Sink::Huge(_))
    }

    /// The registered memory behind the sink, so a page just read can be handed to the
    /// cache without a copy.
    fn buf(&self) -> Buf {
        match self {
            Sink::Small(p) => p.buf(),
            Sink::Huge(b) => *b,
        }
    }
}

/// What [`Paxos::gate`] decided about a write.
enum Gate {
    /// This group still owns the page. `replaying` says this node may take no part in a
    /// round for it, as either proposer or acceptor.
    Serve { replaying: bool },
    /// The shard has been sealed here; the write belongs to that zone.
    Away(u32),
}

/// A shard: the unit a migration hands between groups. An extent, not a slot within
/// one — the seal table is a bounded superblock record and one row per slot would not
/// fit, so a migration moves a whole extent at once.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
pub struct ShardId {
    pub volume: u32,
    pub extent: u32,
}

// ---------------------------------------------------------------------------
// per-core state
// ---------------------------------------------------------------------------

/// A group's standing promise. `held` is false for a term read back from the
/// superblock: a restarting node raises the term before proposing, because the
/// in-flight table that enforces one value per ballot is volatile.
#[derive(Clone, Copy)]
struct Term {
    value: u32,
    held: bool,
    /// A prepare for this group is already in flight from this node.
    preparing: bool,
}

impl Term {
    fn new(value: u32) -> Term {
        Term {
            value,
            held: false,
            preparing: false,
        }
    }
}

/// The consensus counters. One set per core; the exporter sums them.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub accept_ok: u64,
    pub accept_rejected: u64,
    pub one_shot: u64,
    pub guard_conflicts: u64,
    pub prepares: u64,
    pub term_bumps: u64,
    pub lww_retries: u64,
    pub repairs: u64,
    pub read_matched: u64,
    pub read_remote_match: u64,
    pub read_failed: u64,
    pub learn_stale: u64,
    pub seals: u64,
    pub groups_unavailable: u64,
}

/// Consensus state for the groups that hash to this core. No locks and nothing shared:
/// every field is reached through `on_core`.
#[derive(Default)]
struct Local {
    terms: BTreeMap<u32, Term>,
    seals: BTreeMap<ShardId, u32>,
    /// Addresses with a proposal in flight. The one-value-per-ballot rule, and the
    /// write path's per-key serialization, are the same table.
    inflight: BTreeSet<u64>,
    /// Groups we are still replaying. Set by the anti-entropy sweep when it finds our
    /// whole side of a group empty against a peer that has data, cleared when the
    /// digests agree.
    replaying: BTreeSet<u32>,
    stats: Stats,
}

// ---------------------------------------------------------------------------
// Paxos
// ---------------------------------------------------------------------------

/// The links this node holds, as one object so a reload can replace the set with a
/// single swap. Peers come and go with the configuration; a re-declared link keeps its
/// registration and is never disturbed.
struct Links(Box<[Link]>);

/// How to reach one member of a group: which peer to hand the frame to, who the frame
/// is for, and how many forwards it may take on the way.
///
/// `imm` is the address encoding, uniform across ops: zero means "you own this, resolve
/// it yourself", `k + 1` names member index `k`. A receiver that is not the addressee
/// forwards and spends a hop, which is how a leg reaches a member we hold no link to,
/// and how a node holding neither the slot table nor the catalog for a remote zone
/// routes by zone alone.
#[derive(Clone, Copy)]
struct Route<'a> {
    link: &'a Link,
    /// Forwarding budget: 0 to hand it straight to its addressee.
    hops: u8,
    imm: u8,
}

impl Route<'_> {
    fn stamp(&self, mut f: Frame) -> Frame {
        f.flags |= self.hops;
        f.imm = self.imm;
        f
    }

    async fn send(&self, f: Frame, buf: Buf) -> Result<(), Status> {
        self.link
            .send(self.stamp(f), buf)
            .await
            .map_err(Status::from_wire)
    }
}

pub struct Paxos {
    alloc: &'static Allocator,
    cache: &'static Cache,
    links: Live<Links>,
    state: Box<[RefCell<Local>]>,
}

// SAFETY: every `Local` is only ever borrowed from the worker that owns it, and never
// across an await. Same argument as `Allocator`.
unsafe impl Sync for Paxos {}

/// Build the consensus layer and leak it: hop closures must be `Send + 'static`, which
/// a borrow of the control thread's stack cannot be.
pub fn open(alloc: &'static Allocator, cache: &'static Cache, cores: usize) -> &'static Paxos {
    let mut state: Vec<RefCell<Local>> =
        (0..cores).map(|_| RefCell::new(Local::default())).collect();
    // A term is placed on the core that owns its group, which is where every later read
    // of it happens.
    let boot = alloc.boot_consensus();
    for &(group, value) in &boot.terms {
        let l = &mut state[group as usize % cores].get_mut().terms;
        l.insert(group, Term::new(value));
    }
    // A seal covers a whole extent, whose pages are spread over every core, so unlike
    // a term it is replicated rather than placed.
    for s in &boot.seals {
        let id = ShardId {
            volume: s.volume,
            extent: s.extent,
        };
        for l in state.iter_mut() {
            l.get_mut().seals.insert(id, s.term);
        }
    }
    Box::leak(Box::new(Paxos {
        alloc,
        cache,
        links: Live::new(Links(Box::new([]))),
        state: state.into(),
    }))
}

// --------------------------------------------------------------------- routing

impl Paxos {
    pub fn alloc(&self) -> &'static Allocator {
        self.alloc
    }

    pub fn cache(&self) -> &'static Cache {
        self.cache
    }

    pub(crate) fn group(&self, addr: GlobalAddr) -> u32 {
        self.alloc.config().group(addr.0)
    }

    /// The core holding a group's consensus state. In any real deployment this is the
    /// same core the allocator shards the page to: both are the group id modulo a shard
    /// count that saturates at the core count.
    fn core_of(&self, group: u32) -> usize {
        group as usize % self.state.len()
    }

    pub(crate) fn members(&self, group: u32) -> Option<[u32; 3]> {
        self.alloc
            .config()
            .topology
            .catalog
            .get(group as usize)
            .copied()
    }

    /// Our index in the group, if we are a member. Only a member may propose.
    pub(crate) fn self_index(&self, m: &[u32; 3]) -> Option<u8> {
        let me = self.alloc.config().node.id;
        m.iter().position(|&n| n == me).map(|i| i as u8)
    }

    /// Whether this node is one of the three acceptors for `addr`, and so is expected to
    /// hold the page at all.
    pub fn member_of(&self, addr: GlobalAddr) -> bool {
        !self.foreign(addr)
            && self
                .members(self.group(addr))
                .is_some_and(|m| self.self_index(&m).is_some())
    }

    /// Replace the link set. Control thread only, inside a reload's build step: the
    /// links it hands over were opened against the configuration being installed.
    pub fn install_links(&self, links: Vec<Link>) {
        self.links.install(Links(links.into_boxed_slice()));
    }

    pub(crate) fn link_of(&self, node: u32) -> Option<&Link> {
        self.links.get().0.iter().find(|l| l.peer() == node)
    }

    /// Where a frame we will not serve goes next. `imm` names a member of `addr`'s
    /// group rather than a peer, which is why a forwarded frame carries no node id and
    /// is passed on unchanged.
    ///
    /// Zero is the sender saying it could not name a member — it routed by zone alone —
    /// so we do the lookup it could not and pick a member we can reach; whoever lands on
    /// it owns the operation. The second value is whether the forward crosses a site
    /// boundary, the one hop that restores the budget instead of spending it.
    ///
    /// `None` for a foreign address we cannot route: nothing here describes it, and the
    /// originator is sent back to its config.
    pub fn forward_link(&self, op: Op, addr: GlobalAddr, imm: u8) -> Option<(&Link, bool)> {
        // Homed elsewhere: pass it toward that place, which resolves the group itself.
        // The frame is unchanged, so this is the same forward as any other.
        if !self.local_for(op, addr) {
            return self.away(addr).ok().flatten().map(|r| {
                // Whether this hop is a crossing is a fact about the link we chose, not
                // about us: it is one iff it is our own way into the address's site.
                let crossing = self
                    .offsite(addr)
                    .is_some_and(|s| self.alloc.config().crossing_to(s) == Some(r.link.peer()));
                (r.link, crossing)
            });
        }
        let m = self.members(self.group(addr))?;
        let link = match imm {
            0 => self.close(&m).map(|(l, _)| l),
            k => self.link_of(*m.get(k as usize - 1)?),
        };
        link.map(|l| (l, false))
    }

    /// Whether a frame addressed to `imm` is ours to answer or one we must pass on.
    /// Zero is "you own this", which only a member of the group can; `k + 1` names
    /// member `k` and nobody else. An address homed in another zone is never ours: our
    /// slot table does not describe it.
    pub fn serves(&self, op: Op, addr: GlobalAddr, imm: u8) -> bool {
        if !self.local_for(op, addr) {
            return false;
        }
        let me = self
            .members(self.group(addr))
            .and_then(|m| self.self_index(&m));
        match imm {
            0 => me.is_some(),
            k => me == Some(k - 1),
        }
    }

    /// Whether `addr` is homed somewhere this node does not describe: another zone of
    /// this site, or another site. Our slot table covers only our own zone, so a foreign
    /// address has no group here and nothing about it may be resolved locally.
    fn foreign(&self, addr: GlobalAddr) -> bool {
        let cfg = self.alloc.config();
        self.offsite(addr).is_some() || cfg.zone_of(addr.0).is_some_and(|z| z != cfg.node.zone)
    }

    /// The site `addr` is homed in, when it is not ours. Derived from the volume id, so
    /// it needs nothing of the far site.
    fn offsite(&self, addr: GlobalAddr) -> Option<u32> {
        let cfg = self.alloc.config();
        cfg.site_of(addr.0).filter(|&s| s != cfg.node.site)
    }

    /// The zone still answering for `addr` while its extent is being pulled into ours.
    /// `None` unless this node is the migration's destination.
    fn inbound(&self, addr: GlobalAddr) -> Option<u32> {
        let cfg = self.alloc.config();
        let here = cfg.next_zone_of(addr.0)? == cfg.node.zone;
        cfg.zone_of(addr.0).filter(|&z| here && z != cfg.node.zone)
    }

    /// Whether this zone is the one to answer `op` for `addr`. A migration destination
    /// takes the extent's bulk stream before it takes client traffic, so `LEARN` is the
    /// one op an inbound extent is already local for.
    fn local_for(&self, op: Op, addr: GlobalAddr) -> bool {
        !self.foreign(addr) || (op == Op::Learn && self.inbound(addr).is_some())
    }

    /// The route out of this zone, if `addr` is not homed here.
    ///
    /// We resolve only the zone and express it by which peer we send to; the entry node
    /// holds that zone's slot table and catalog and does the rest. `imm` is zero because
    /// we cannot name a member, and the budget is two: one hop to reach a member, one
    /// spare for a shard mid-migration at the far end.
    ///
    /// A foreign address we cannot route is an error rather than a fallback: resolving
    /// it against our own slot table would name a group in the wrong zone.
    fn away(&self, addr: GlobalAddr) -> Result<Option<Route<'_>>, Status> {
        if let Some(site) = self.offsite(addr) {
            return self.across(site, addr).map(Some);
        }
        if !self.foreign(addr) {
            return Ok(None);
        }
        let zone = self
            .alloc
            .config()
            .zone_of(addr.0)
            .ok_or(Status::Unmapped)?;
        self.toward(zone, addr).map(Some)
    }

    /// A route toward another site. We take our own crossing when we hold one; otherwise
    /// we hand the address to a peer that does, and know nothing further.
    fn across(&self, site: u32, addr: GlobalAddr) -> Result<Route<'_>, Status> {
        let cfg = self.alloc.config();
        let mine = cfg.crossing_to(site);
        let node = mine
            .or_else(|| cfg.gateway_to(site, addr.0))
            .ok_or(Status::Unmapped)?;
        let link = self.link_of(node).ok_or(Status::Io)?;
        // Crossing here means the far site funds its own hops, as a relayed crossing does
        // by refreshing; handing off locally leaves that to the peer we hand to.
        Ok(Route {
            link,
            hops: if mine.is_some() { 3 } else { 2 },
            imm: 0,
        })
    }

    /// A route into `zone`, through the entry node that zone's directory names for this
    /// address. Two hops: one to reach a member of the group, one spare for a shard the
    /// far side is in the middle of handing on.
    fn toward(&self, zone: u32, addr: GlobalAddr) -> Result<Route<'_>, Status> {
        let entry = self
            .alloc
            .config()
            .entry_of(zone, addr.0)
            .ok_or(Status::Unmapped)?;
        let link = self.link_of(entry).ok_or(Status::Io)?;
        Ok(Route {
            link,
            hops: 2,
            imm: 0,
        })
    }

    /// Whether this page has a peer that could heal it. Everything that escalates a miss
    /// to consensus checks this first, so a single-node configuration never pays for a
    /// round it has nobody to hold.
    pub fn healable(&self, addr: GlobalAddr) -> bool {
        let Some(m) = self.members(self.group(addr)) else {
            return false;
        };
        let me = self.self_index(&m);
        (0..3u8).any(|i| Some(i) != me && self.route(&m, i).is_some())
    }

    /// How many acceptors must apply a value for it to be chosen: a majority of the
    /// three, unconditionally. Reachability does not enter into it — a member we hold no
    /// link to is one we *route* to, and a member that is genuinely down is one the
    /// other two carry. A quorum that shrank to what we could see would let an isolated
    /// node decide alone.
    ///
    /// The exception is a configuration that names no peers at all: a single-node
    /// deployment rather than a degraded three, with nobody to route through and no
    /// second acceptor to wait for, so a local accept is a decision.
    fn quorum(&self) -> usize {
        if self.links.get().0.is_empty() { 1 } else { 2 }
    }

    fn frame(&self, op: Op, addr: GlobalAddr, huge: bool) -> Result<Frame, Status> {
        let v = self
            .alloc
            .config()
            .volume(addr.volume())
            .ok_or(Status::Unmapped)?;
        Ok(Frame::new(op, huge, v.slot, addr.offset()))
    }

    fn stat(&self, f: impl FnOnce(&mut Stats)) {
        f(&mut self.state[runtime::core()].borrow_mut().stats);
    }

    pub fn local_stats(&self) -> Stats {
        self.state[runtime::core()].borrow().stats
    }
}

// --------------------------------------------------------------------- client side

impl Paxos {
    /// The originating node's write path. `guard` is the version the caller expects to
    /// replace, and is every type check at once; `None` leaves it to be derived wherever
    /// the register lives.
    ///
    /// Returns the new version, always `guard + 1` for all three types — the reason an
    /// `ACCEPT` needs no reply body.
    async fn propose(
        &'static self,
        addr: GlobalAddr,
        guard: Option<u64>,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        // An address homed in another zone is not in our slot table, so there is no
        // group here to resolve. The entry node resolves it and the member it reaches
        // proposes, exactly as the close member does at home.
        if let Some(r) = self.away(addr)? {
            self.send_accept(r, addr, guard, Ballot::ZERO, page).await?;
            return Ok(guard.map_or(0, |g| g + 1));
        }
        let group = self.group(addr);
        let replaying = match self.gate(addr, group).await? {
            Gate::Serve { replaying } => replaying,
            // Sealed here and the config has not caught up: hand it to the destination.
            Gate::Away(z) => {
                let r = self.toward(z, addr)?;
                self.send_accept(r, addr, guard, Ballot::ZERO, page).await?;
                return Ok(guard.map_or(0, |g| g + 1));
            }
        };
        let m = self.members(group).ok_or(Status::Unmapped)?;
        match self.self_index(&m).filter(|_| !replaying) {
            // We hold the register ourselves, so we are the proposer and drive the
            // fan-out. A guard left to us is derived here, where the slab is
            // authoritative, and the derived value is what the peers are sent.
            Some(k) => {
                let g = match guard {
                    Some(g) => g,
                    None => self.alloc.guard(addr).await?,
                };
                self.drive(addr, group, m, k, g, page).await
            }
            // Otherwise the close member proposes, which `imm` zero says. The data
            // crosses the wire once and no reply body is needed.
            None => self.forward(addr, m, guard, page).await,
        }
    }

    /// `propose` with the guard derived here rather than supplied. What the ublk path
    /// uses: LWW takes the version it last observed and retries internally, OCC takes
    /// the version it read (and conflicts if it did not read), an Immutable fill takes
    /// `3 * epoch`.
    pub async fn write(&'static self, addr: GlobalAddr, page: Page<'_>) -> Result<u64, Status> {
        let (kind, _) = self.alloc.kind_of(addr)?;
        // "Version last observed" only means anything where the register lives: a
        // non-member's slab holds nothing for this key, so its LWW guard would be a
        // permanent zero and every write after the first would conflict. It leaves the
        // guard to the close member instead, whose `current` is what the retry loop
        // below converges on anyway. OCC cannot do this: its guard *is* the client's
        // read, and no acceptor knows it.
        let member = self.member_of(addr);
        let derive = kind == Kind::Lww && !member;
        let mut tries = 0;
        // Raised by a repair below to the version the group settled on.
        let mut floor = 0;
        loop {
            let guard = if derive {
                None
            } else {
                Some(self.alloc.guard(addr).await?.max(floor))
            };
            match self.propose(addr, guard, page).await {
                // A LWW mismatch is a lost race, not a client error. Re-read the
                // version and propose again; the client still sees last-write-wins.
                Err(Status::Conflict { .. }) if kind == Kind::Lww && tries < LWW_RETRIES => {
                    tries += 1;
                    self.stat(|s| s.lww_retries += 1);
                    // Our own version is only a guess at the group's, and a stale guess
                    // conflicts forever: the copy we hold may be behind, or the value we
                    // are behind may be unreadable everywhere, where no repair can move
                    // us. A blind write needs no old bytes, only the version to build
                    // on, so take that from the repair round directly.
                    if member && let Ok(best) = self.repair(addr).await {
                        floor = floor.max(best.version);
                    }
                }
                r => return r,
            }
        }
    }

    /// Delete an Immutable page: an ordinary guarded accept whose value is a
    /// tombstone. Idempotent, because the tombstone is a state and not an event.
    pub async fn trim(&'static self, addr: GlobalAddr) -> Result<(), Status> {
        let epoch = self.alloc.config().tombstone_epoch_of(addr.0);
        // A live page sits at `3e + 1`; that is what a trim guards on.
        let guard = 3 * epoch + 1;
        // Homed elsewhere: the entry node resolves the group.
        if let Some(r) = self.away(addr)? {
            return self.send_trim(r, addr, guard, Ballot::ZERO).await;
        }
        let group = self.group(addr);
        let replaying = match self.gate(addr, group).await? {
            Gate::Serve { replaying } => replaying,
            Gate::Away(z) => {
                return self
                    .send_trim(self.toward(z, addr)?, addr, guard, Ballot::ZERO)
                    .await;
            }
        };
        // Immutable cache entries are invalidated by the epoch bump, which a delete only
        // reaches later. Dropping our own entry here closes the window on the node doing
        // the trim; a cohort replica elsewhere still holds its copy until the epoch
        // advances.
        if let Ok((_, huge)) = self.alloc.kind_of(addr) {
            self.cache.forget(addr, huge).await;
        }
        let m = self.members(group).ok_or(Status::Unmapped)?;
        match self.self_index(&m).filter(|_| !replaying) {
            Some(k) => {
                let term = self.term_for(group, addr).await?;
                let b = Ballot::new(term, k);
                let need = self.quorum();
                let local = self.alloc.accept_trim(addr, guard, b);
                let mut peers = self.peers(&m, Some(k));
                self.fan_out(local, &mut peers, need, |r| {
                    self.send_trim(r, addr, guard, b)
                })
                .await?;
                self.stat(|s| s.one_shot += 1);
                Ok(())
            }
            None => {
                // `imm` zero: the close member picks the ballot and fans out.
                self.delegate(&m, |r| self.send_trim(r, addr, guard, Ballot::ZERO))
                    .await
            }
        }
    }

    /// Peers of ours in this group, as routes. A member we hold no direct link to is
    /// reached through one we do rather than dropped, so the quorum can stay a fixed
    /// 2 of 3 on a topology that is not a full mesh. The three members of a group sit
    /// in one zone and are mutually adjacent, so the forward is a fallback rather than
    /// the common case; [`Self::fan_out`] and [`Self::fan_peers`] refuse rather than
    /// ack short when even that is not available.
    fn peers(&self, m: &[u32; 3], me: Option<u8>) -> Vec<Route<'_>> {
        (0..3u8)
            .filter(|i| Some(*i) != me)
            .filter_map(|i| self.route(m, i))
            .collect()
    }

    /// Members we hold a direct link to, in member order. The first is the one we
    /// delegate to; the rest are the failover order for when it does not answer.
    fn candidates(&self, m: &[u32; 3]) -> impl Iterator<Item = (&Link, u8)> {
        let m = *m;
        (0..3u8).filter_map(move |i| self.link_of(m[i as usize]).map(|l| (l, i)))
    }

    /// The first member we hold a link to, plus its index.
    fn close(&self, m: &[u32; 3]) -> Option<(&Link, u8)> {
        self.candidates(m).next()
    }

    /// Member indices for the data leg, adjacent ones first. The value is the only part
    /// of a read big enough to care which way it travels: a `GET` routed through a
    /// neighbor crosses the wire twice and lands the page on a node with no use for it.
    /// Metadata legs are a trailer each and route freely.
    fn nearest_first(&self, m: &[u32; 3]) -> [u8; 3] {
        nearest_first(m, |n| self.link_of(n).is_some())
    }

    /// How to reach member `k`: directly if we hold a link, otherwise forwarded through
    /// a member we do.
    fn route(&self, m: &[u32; 3], k: u8) -> Option<Route<'_>> {
        match self.link_of(*m.get(k as usize)?) {
            Some(link) => Some(Route {
                link,
                hops: 0,
                imm: k + 1,
            }),
            None => self.close(m).map(|(link, _)| Route {
                link,
                hops: 1,
                imm: k + 1,
            }),
        }
    }

    /// We are member `k`: stage the page locally and fan out concurrently, acking as soon
    /// as a quorum is durable. Latency is one remote hop plus the slower of the local
    /// write and one peer accept; the third acceptor is in flight and nobody waits.
    async fn drive(
        &'static self,
        addr: GlobalAddr,
        group: u32,
        m: [u32; 3],
        k: u8,
        guard: u64,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        let term = self.term_for(group, addr).await?;
        let b = Ballot::new(term, k);
        let need = self.quorum();
        self.claim(addr, group).await?;
        let mut peers = self.peers(&m, Some(k));
        let r = self.round(addr, &mut peers, need, guard, b, page).await;
        self.release(addr, group).await;
        match r {
            Ok(()) => {
                self.stat(|s| {
                    s.one_shot += 1;
                    s.accept_ok += 1;
                });
                Ok(guard + 1)
            }
            Err(e) => {
                // Members refresh on a rejected ballot. A quorum of the other two can
                // raise the term without us, and the refusal carries no term back — an
                // `ACCEPT` reply has no trailer — so rejection is the only signal we
                // get. Dropping the held flag makes the next attempt prepare, which
                // re-establishes a term we may propose at. A guard conflict landing here
                // too costs one extra round trip, which contended LWW was going to cost
                // anyway.
                self.refresh(group).await;
                self.stat(|s| match e {
                    Status::Conflict { .. } => s.guard_conflicts += 1,
                    _ => s.accept_rejected += 1,
                });
                Err(e)
            }
        }
    }

    /// One accept round as the proposer. Our own leg is staged, not committed, until the
    /// peers have answered: a proposer that installed its value regardless would leave
    /// this node a version ahead of a group that never agreed to it, and every retry
    /// guards on that version, so the fork grows and no apply-if-newer repair can pull
    /// it back.
    async fn round(
        &'static self,
        addr: GlobalAddr,
        peers: &mut Vec<Route<'_>>,
        need: usize,
        guard: u64,
        b: Ballot,
        page: Page<'_>,
    ) -> Result<(), Status> {
        let staged = self.stage_local(addr, guard, b, page);
        // A 4 MiB page travels as the guest's own buffer, so every leg that was handed it
        // must be done with it before the round answers and the buffer is recycled.
        let settle = matches!(page, Page::Huge(_));
        let votes = self.fan_peers(peers, need, settle, |r| {
            self.send_accept(r, addr, Some(guard), b, page)
        });
        match join2(staged, votes).await {
            (Ok(p), Ok(())) => self.alloc.finish(addr, p).await.map(|_| ()),
            (Ok(p), Err(e)) => {
                self.alloc.abandon(addr, p).await;
                Err(e)
            }
            (Err(e), _) => Err(e),
        }
    }

    /// We are not a member: hand the page to the close member, which proposes. One
    /// fabric write, and the data crosses the wire once.
    async fn forward(
        &'static self,
        addr: GlobalAddr,
        m: [u32; 3],
        guard: Option<u64>,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        self.delegate(&m, |r| self.send_accept(r, addr, guard, Ballot::ZERO, page))
            .await?;
        // A guard left to the acceptor leaves us without the new version. Nothing on the
        // ublk path reads it, and an `ACCEPT` has no reply body to carry it back.
        Ok(guard.map_or(0, |g| g + 1))
    }

    /// Hand a proposal to a member and let it propose. A member that does not answer is
    /// not the group's answer: the next candidate is tried, and only a group with nobody
    /// home is unavailable. Anything other than a transport failure is the group's
    /// verdict and is returned as it stands.
    async fn delegate<'a, S, F>(&'a self, m: &[u32; 3], mut send: S) -> Result<(), Status>
    where
        S: FnMut(Route<'a>) -> F,
        F: Future<Output = Result<(), Status>>,
    {
        for (link, _) in self.candidates(m) {
            match send(Route {
                link,
                hops: 0,
                imm: 0,
            })
            .await
            {
                Err(Status::Io) => continue,
                r => return r,
            }
        }
        self.stat(|s| s.groups_unavailable += 1);
        Err(Status::Io)
    }

    async fn stage_local(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        b: Ballot,
        page: Page<'_>,
    ) -> Result<Pending, Status> {
        match page {
            Page::Small(p) => self.alloc.begin_small(addr, guard, b, p).await,
            Page::Huge(buf) => self.alloc.begin_huge(addr, guard, b, buf).await,
        }
    }

    /// The route's `imm` tells the target whether to apply or propose: it names a member
    /// on the proposer's own fan-out, and is zero on the leg from a non-member, which
    /// hands the proposal over entire.
    async fn send_accept(
        &self,
        r: Route<'_>,
        addr: GlobalAddr,
        guard: Option<u64>,
        b: Ballot,
        page: Page<'_>,
    ) -> Result<(), Status> {
        let huge = matches!(page, Page::Huge(_));
        let f = self.frame(Op::Accept, addr, huge)?;
        match page {
            // A 4 MiB frame is all payload, so its guard and ballot are derived by the
            // acceptor rather than sent. Sound only because the class is Immutable,
            // whose guard is a pure function of the epoch.
            Page::Huge(buf) => r.send(f, buf).await,
            // A 4 KiB page already pays a copy through registered memory, so gathering
            // its trailer beside it is free.
            Page::Small(p) => {
                let mut t = PoolBuf::alloc(2 * fabric::BLOCK).await;
                t[..fabric::BLOCK].copy_from_slice(p);
                t[fabric::BLOCK..].fill(0);
                let tr = &mut t[fabric::BLOCK..];
                fabric::put(tr, T_GUARD, guard.unwrap_or(DERIVED));
                fabric::put(tr, T_BALLOT, b.raw() as u64);
                fabric::put(tr, T_EPOCH, self.alloc.config().topology.epoch as u64);
                r.send(f, t.buf()).await
            }
        }
    }

    async fn send_trim(
        &self,
        r: Route<'_>,
        addr: GlobalAddr,
        guard: u64,
        b: Ballot,
    ) -> Result<(), Status> {
        let (_, huge) = self.alloc.kind_of(addr)?;
        let f = self.frame(Op::Trim, addr, huge)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        t.fill(0);
        fabric::put(&mut t, T_GUARD, guard);
        fabric::put(&mut t, T_BALLOT, b.raw() as u64);
        fabric::put(&mut t, T_EPOCH, self.alloc.config().topology.epoch as u64);
        r.send(f, t.buf()).await
    }

    /// However many peer accepts the quorum needs beside our own leg. The legs that lose
    /// are abandoned, not canceled: their futures are dropped and whatever they were
    /// doing completes unobserved. `settle` forbids that, for a payload the caller does
    /// not own: a 4 MiB accept puts the guest's own request buffer on the wire, and that
    /// buffer goes back to the kernel the moment the request is answered.
    async fn fan_peers<'p, S, F>(
        &self,
        peers: &mut Vec<Route<'p>>,
        need: usize,
        settle: bool,
        mut send: S,
    ) -> Result<(), Status>
    where
        S: FnMut(Route<'p>) -> F,
        F: Future<Output = Result<(), Status>>,
    {
        // A quorum we cannot reach means the group is unavailable, not smaller: acking
        // on the local write alone would let an isolated member decide.
        if peers.len() + 1 < need {
            self.stat(|s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }
        let want = need.saturating_sub(1);
        match (peers.pop(), peers.pop()) {
            _ if want == 0 => Ok(()),
            (None, _) => Ok(()),
            // Two members and a quorum of two: the one peer must land.
            (Some(a), None) => send(a).await,
            (Some(a), Some(b)) => {
                let q = if settle {
                    let (x, y) = join2(send(a), send(b)).await;
                    [Some(x), Some(y)]
                } else {
                    runtime::quorum([send(a), send(b)], want).await
                };
                let ok = q.iter().flatten().filter(|r| r.is_ok()).count();
                if ok >= want {
                    Ok(())
                } else {
                    // A refusal is the group's verdict and is worth another round; a
                    // peer we could not reach says only that we did not hear from it.
                    // Prefer the verdict, so one member behind on its term does not
                    // read to the client as a group that is gone.
                    let e = q
                        .into_iter()
                        .flatten()
                        .filter_map(|r| r.err())
                        .min_by_key(|e| matches!(e, Status::Io) as u8)
                        .unwrap_or(Status::Io);
                    Err(e)
                }
            }
        }
    }

    /// Await the local accept and however many peer accepts the quorum still needs.
    async fn fan_out<'p, L, S, F>(
        &self,
        local: L,
        peers: &mut Vec<Route<'p>>,
        need: usize,
        mut send: S,
    ) -> Result<(), Status>
    where
        L: Future<Output = Result<(), Status>>,
        S: FnMut(Route<'p>) -> F,
        F: Future<Output = Result<(), Status>>,
    {
        // A quorum we cannot reach means the group is unavailable, not smaller: acking
        // on the local write alone would let an isolated member decide.
        if peers.len() + 1 < need {
            self.stat(|s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }
        match (peers.pop(), peers.pop()) {
            (None, _) => local.await,
            (Some(a), None) => {
                let (l, r) = join2(local, send(a)).await;
                // Two members and a quorum of two: both must land.
                if need >= 2 { l.and(r) } else { l.or(r) }
            }
            (Some(a), Some(b)) => {
                let want = need.saturating_sub(1);
                let (l, q) = join2(local, runtime::quorum([send(a), send(b)], want)).await;
                let ok = q.iter().flatten().filter(|r| r.is_ok()).count();
                if ok >= want {
                    // The local write is what makes this node an acceptor of the value
                    // it chose, so its failure is still a failure.
                    l
                } else {
                    // A refusal is the group's verdict and is worth another round; a
                    // peer we could not reach says only that we did not hear from it.
                    // Prefer the verdict, so one member behind on its term does not
                    // read to the client as a group that is gone.
                    let e = q
                        .into_iter()
                        .flatten()
                        .filter_map(|r| r.err())
                        .min_by_key(|e| matches!(e, Status::Io) as u8)
                        .unwrap_or(Status::Io);
                    l.and(Err(e))
                }
            }
        }
    }

    /// The guard forbids pipelining two writes to one page, so the proposer serializes
    /// same-key proposals rather than letting them race and both lose. It is also the
    /// one-value-per-ballot rule: a second attempt at one version would have to reuse a
    /// ballot, which repair could then use to resurrect a value that was never chosen.
    async fn claim(&'static self, addr: GlobalAddr, group: u32) -> Result<(), Status> {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            if self.state[core].borrow_mut().inflight.insert(addr.0) {
                Ok(())
            } else {
                Err(Status::Conflict { current: 0 })
            }
        })
        .await
    }

    async fn release(&'static self, addr: GlobalAddr, group: u32) {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            self.state[core].borrow_mut().inflight.remove(&addr.0);
        })
        .await;
    }
}

// --------------------------------------------------------------------- read path

impl Paxos {
    /// Reads take no ballot and write nothing. A read is believed when two replicas
    /// report the same `(version, ballot)`: two conflicting one-shots at one version
    /// necessarily carry different ballots, so a matching pair implies matching bytes
    /// and no digest is needed.
    ///
    /// The only caller that records an OCC observation, because it is the only read a
    /// client of *this* node made. The observation ring must not see a peer's `GET`.
    pub async fn read(&'static self, addr: GlobalAddr, sink: Sink<'_>) -> Result<Register, Status> {
        let r = self.read_uncounted(addr, sink).await;
        match r {
            // A hole is an observation too, at version zero: without it the first
            // write to an OCC page could never pass its check.
            Ok(reg) => self.alloc.observed(addr, reg.version).await,
            Err(Status::Hole) => self.alloc.observed(addr, 0).await,
            Err(_) => {}
        }
        r
    }

    /// A peer's `GET` with `imm == 0`: it resolved our zone and stopped there, so it is
    /// asking for the group's answer rather than for this member's copy.
    ///
    /// Running the hedged round here makes a cross-zone read linearizable for the one
    /// round trip the reader paid: it cannot name the other two members, but we can and
    /// they are beside us. The observation ring must not see it — the read is a
    /// client's, but not ours.
    pub async fn read_for(
        &'static self,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        self.read_uncounted(addr, sink).await
    }

    async fn read_uncounted(
        &'static self,
        addr: GlobalAddr,
        mut sink: Sink<'_>,
    ) -> Result<Register, Status> {
        // Homed in another zone: the entry node resolves the group and the member it
        // reaches runs the round, so this costs one round trip rather than three and
        // the metadata legs stay inside the zone that owns the page.
        if let Some(r) = self.away(addr)? {
            return self.pull_from(r, addr, sink).await;
        }
        let group = self.group(addr);
        let m = self.members(group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&m);
        let need = self.quorum();
        let (kind, _) = self.alloc.kind_of(addr)?;

        // A group member holds the authoritative page, so it is never a cache client;
        // the 4 MiB class takes the round-free path in `server` instead.
        let client = me.is_none() && need > 1 && !sink.huge();
        // The width the last round advertised. It is only ever learned *from* a reply,
        // so the first read of a key is always uncached — which is also what the
        // admission filter wants.
        let w = if client { self.cache.hint(addr) } else { 0 };
        if w > 0
            && let Some(r) = self.hedged_cached(addr, &m, me, w, sink.reborrow()).await?
        {
            return Ok(r);
        }

        let (source, first) = match self.fetch(addr, &m, me, sink.reborrow()).await {
            Ok(v) => v,
            // The copy we are adjacent to has nothing. That is not an answer about the
            // register — `MISSING` is not a vote — so we heal from the other two and
            // ask again rather than reporting a hole.
            Err(e @ (Status::Hole | Status::Missing)) => {
                if need <= 1 {
                    return Err(e);
                }
                self.stat(|s| s.read_failed += 1);
                let best = self.repair(addr).await?;
                // The repair round is the first authoritative word on this key. If
                // nothing was ever chosen, the client is reading a hole — a distinction
                // the wire cannot carry, since `MISSING` is all a peer can say about a
                // page it does not have.
                if empty(kind, best.version) {
                    return Err(Status::Hole);
                }
                (
                    None,
                    self.pull_best(addr, &m, me, best, sink.reborrow()).await?,
                )
            }
            Err(e) => return Err(e),
        };
        if need <= 1 {
            self.stat(|s| s.read_matched += 1);
            return Ok(first);
        }
        let others = self.metas(addr, &m, me, source).await;
        if others.contains(&Some(first)) {
            self.stat(|s| s.read_matched += 1);
            if client {
                self.offer(addr, &sink, first).await;
            }
            return Ok(first);
        }
        // No pair including the copy we already hold. If two others agree, their value
        // is the chosen one and costs one more round trip; if nobody agrees, nothing was
        // chosen and we must repair before returning anything. Returning a value we
        // could not confirm is the one way this design could lose linearizability.
        match matching(&others) {
            Some((r, _)) => {
                self.stat(|s| s.read_remote_match += 1);
                self.pull_any(addr, &m, me, &others, r, sink.reborrow())
                    .await?;
                if client {
                    self.offer(addr, &sink, r).await;
                }
                Ok(r)
            }
            None => {
                self.stat(|s| s.read_failed += 1);
                let best = self.repair(addr).await?;
                if empty(kind, best.version) {
                    return Err(Status::Hole);
                }
                self.pull_best(addr, &m, me, best, sink).await
            }
        }
    }

    /// The cached leg and the metadata round, issued together.
    ///
    /// Both go out at once, so a hit is one round trip and at most two hops — what the
    /// uncached read costs. What changes is where the bytes come from: no media read at
    /// the owner and no page on the wire from it.
    ///
    /// `None` means the round agreed on nothing, which is the state the uncached path
    /// already knows how to repair. Everything else is answered here.
    async fn hedged_cached(
        &'static self,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        w: u8,
        mut sink: Sink<'_>,
    ) -> Result<Option<Register>, Status> {
        let (cached, others) = join2(
            self.cached_leg(addr, w, sink.reborrow()),
            self.metas(addr, m, me, None),
        )
        .await;
        let agreed = matching(&others);
        // Confirmation is on `(version, ballot)`, not the version alone, so a copy left
        // behind by a migrated extent fails it — the term bump changed the ballot — and
        // invalidation costs nothing.
        if let Some((r, _)) = agreed
            && cached == Some(r)
        {
            self.cache.served();
            self.stat(|s| s.read_matched += 1);
            return Ok(Some(r));
        }
        if cached.is_some() {
            self.cache.forget(addr, sink.huge()).await;
        }
        let Some((r, idx)) = agreed else {
            return Ok(None);
        };
        // The entry was stale or absent. The metadata round is already done, so only the
        // data leg is left: one extra round trip on a rare path.
        self.stat(|s| s.read_remote_match += 1);
        let route = self.route(m, idx).ok_or(Status::Io)?;
        self.pull_from(route, addr, sink.reborrow()).await?;
        self.offer(addr, &sink, r).await;
        Ok(Some(r))
    }

    /// The page from wherever the cohort keeps it: locally if we are one of the `w`
    /// replicas, otherwise a `CACHE_ONLY` `GET` at the highest-ranked live one. Either
    /// way it carries the register the copy claims, and nothing about it is believed
    /// until the metadata round confirms that register.
    async fn cached_leg(
        &'static self,
        addr: GlobalAddr,
        w: u8,
        sink: Sink<'_>,
    ) -> Option<Register> {
        if self.cache.holds(addr, w) {
            return self.cache.load(addr, false, 0, sink.buf()).await;
        }
        let node = self.cache.replica(addr, w, |n| self.link_of(n).is_some())?;
        let link = self.link_of(node)?;
        let Sink::Small(p) = sink else { return None };
        // Gather mode: the peer's page and the register it claims arrive in one command.
        // A miss, or a replica that is shedding, answers `MISSING` and we fall back to
        // the group.
        let mut f = self.frame(Op::Get, addr, false).ok()?;
        f.flags |= fabric::CACHE_ONLY;
        let t = PoolBuf::alloc(2 * fabric::BLOCK).await;
        link.send(f, t.buf()).await.ok()?;
        p[..fabric::BLOCK].copy_from_slice(&t[..fabric::BLOCK]);
        Some(read_register(&t[fabric::BLOCK..]))
    }

    /// The width the cache should use for `addr`, from this node's own read stream. The
    /// 4 MiB path has no metadata round to carry a hint on, so it asks the estimator
    /// directly.
    pub async fn cache_width(&'static self, addr: GlobalAddr) -> u8 {
        self.cache.observe(addr).await
    }

    /// A 4 MiB cache hit and its confirming round, issued together.
    ///
    /// The ordinary path for this class takes no round — a live immutable page is
    /// terminal within its epoch — but a *cached* copy is a weaker claim: some node held
    /// these bytes at some version, and only the group can say whether that version is
    /// still the chosen one. Both legs go out at once, so the hit still costs one round
    /// trip and saves the media read at the owner and the 4 MiB page on the wire.
    ///
    /// A version with no ballot beside it is a complete identity here: nothing but a
    /// quorum-confirmed value is ever admitted (see [`Self::offer_huge`]), and at one
    /// version there is exactly one such value.
    pub async fn cached_huge(&'static self, addr: GlobalAddr, off: usize, w: u8, buf: Buf) -> bool {
        let Some(m) = self.members(self.group(addr)) else {
            return false;
        };
        let me = self.self_index(&m);
        let (cached, others) = join2(
            self.cached_huge_leg(addr, off, w, buf),
            self.metas(addr, &m, me, None),
        )
        .await;
        let Some(r) = cached else { return false };
        if self.quorum() > 1 && !matches!(matching(&others), Some((q, _)) if q.version == r.version)
        {
            // Stale, or never chosen. Dropping it keeps a mis-cached page from costing
            // this round again on the next read.
            self.cache.forget(addr, true).await;
            return false;
        }
        self.cache.served();
        self.stat(|s| s.read_matched += 1);
        true
    }

    /// The 4 MiB page from wherever the cohort keeps it: our own region if we have it,
    /// otherwise a `CACHE_ONLY` pair at the highest-ranked live replica. The page has no
    /// trailer to gather a register into, so the register rides a concurrent
    /// `CACHE_ONLY` `GETMETA` — two commands, still one round trip. Both are filtered to
    /// the live version at the replica, so they cannot name different entries. A miss, or
    /// a replica that is shedding, answers `MISSING` and we fall back.
    async fn cached_huge_leg(
        &'static self,
        addr: GlobalAddr,
        off: usize,
        w: u8,
        buf: Buf,
    ) -> Option<Register> {
        if let Some(r) = self.cache.load_immutable(addr, true, off, buf).await {
            return Some(r);
        }
        // Only a whole page can come off a peer: a `GET` names a page and a 4 MiB frame
        // carries no offset. A partial read takes the ordinary path.
        if off != 0 || buf.len() as u64 != layout::HUGE_PAGE || w == 0 || self.cache.holds(addr, w)
        {
            return None;
        }
        let node = self.cache.replica(addr, w, |n| self.link_of(n).is_some())?;
        let link = self.link_of(node)?;
        let mut f = self.frame(Op::Get, addr, true).ok()?;
        f.flags |= fabric::CACHE_ONLY;
        let mut meta = self.frame(Op::GetMeta, addr, true).ok()?;
        meta.flags |= fabric::CACHE_ONLY;
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        let (d, r) = join2(link.send(f, buf), link.send(meta, t.buf())).await;
        d.ok()?;
        r.ok()?;
        Some(read_register(&t))
    }

    /// Admission for the 4 MiB class.
    ///
    /// The confirming round runs here rather than on the read that will use the entry.
    /// This class has no per-page checksum and its cached leg carries no ballot, so what
    /// makes a hit safe to serve on a version comparison alone is that only a value the
    /// group agreed on was ever admitted: two acceptors can hold different bytes at one
    /// version, and the loser's are exactly what a cache is liable to pick up. A miss
    /// has already moved 4 MiB, so two metadata commands are noise against it.
    ///
    /// The ballot is not stored: an immutable entry is validated against the version,
    /// leaving a ballot nothing to distinguish.
    pub async fn offer_huge(&'static self, addr: GlobalAddr, w: u8, buf: Buf, version: u64) {
        if w == 0 || !self.cache.holds(addr, w) {
            return;
        }
        let Some(m) = self.members(self.group(addr)) else {
            return;
        };
        if self.quorum() > 1 {
            let others = self.metas(addr, &m, self.self_index(&m), None).await;
            if !matches!(matching(&others), Some((q, _)) if q.version == version) {
                return;
            }
        }
        let r = Register {
            version,
            ballot: Ballot::ZERO,
        };
        self.cache.admit(addr, true, buf, r, w).await;
    }

    /// A reader that is one of the `w` replicas already has the page in flight from the
    /// group, so admission is a write of bytes it holds anyway. The cache decides for
    /// itself whether to take it.
    async fn offer(&'static self, addr: GlobalAddr, sink: &Sink<'_>, r: Register) {
        let w = self.cache.hint(addr);
        if w > 0 {
            self.cache.admit(addr, sink.huge(), sink.buf(), r, w).await;
        }
    }

    /// Read the page and its register from an adjacent member, or locally if we hold it.
    /// Returns the member index the bytes came from.
    ///
    /// Members we hold a link to are asked first, so the page comes off an adjacent node
    /// rather than being relayed through one. A member that does not answer is not the
    /// group's answer: the other two hold the same value and are tried in turn. `Hole`
    /// and `Missing` are answers, and the caller escalates them to repair rather than
    /// asking someone else.
    async fn fetch(
        &'static self,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        mut sink: Sink<'_>,
    ) -> Result<(Option<u8>, Register), Status> {
        if let Some(k) = me {
            let r = self.read_local(addr, sink).await?;
            return Ok((Some(k), r));
        }
        let mut last = Status::Io;
        for i in self.nearest_first(m) {
            let Some(route) = self.route(m, i) else {
                continue;
            };
            match self.pull_from(route, addr, sink.reborrow()).await {
                Ok(r) => return Ok((Some(i), r)),
                Err(Status::Io) => last = Status::Io,
                Err(e) => return Err(e),
            }
        }
        Err(last)
    }

    /// The whole 4 MiB page, from whichever member answers. This class takes no round on
    /// a hit, but a non-member never had a copy to hit: its local miss is not the group's
    /// answer, and repair heals acceptors rather than putting the page in front of a
    /// reader that is not one.
    pub async fn pull_huge(&'static self, addr: GlobalAddr, buf: Buf) -> Result<Register, Status> {
        if let Some(r) = self.away(addr)? {
            return self.pull_from(r, addr, Sink::Huge(buf)).await;
        }
        let m = self.members(self.group(addr)).ok_or(Status::Unmapped)?;
        let (_, r) = self.fetch(addr, &m, None, Sink::Huge(buf)).await?;
        Ok(r)
    }

    async fn read_local(
        &'static self,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        // The register comes back with the bytes rather than from a look of its own: an
        // accept landing between the two would pair a value with a version it was never
        // written at.
        match sink {
            Sink::Huge(b) => self.alloc.read_huge(addr, 0, b).await,
            Sink::Small(p) => self.alloc.read_small(addr, p).await,
        }
    }

    /// One `GET` at a peer. A small page gathers its register into the reply trailer; a
    /// 4 MiB page has no trailer, so its register rides a concurrent `GETMETA` — two
    /// commands issued together, still one round trip.
    async fn pull_from(
        &self,
        r: Route<'_>,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let f = self.frame(Op::Get, addr, sink.huge())?;
        match sink {
            Sink::Huge(b) => {
                let t = PoolBuf::alloc(fabric::BLOCK).await;
                let meta = self.frame(Op::GetMeta, addr, true)?;
                let (d, m) = join2(r.send(f, b), r.send(meta, t.buf())).await;
                d?;
                m?;
                Ok(read_register(&t))
            }
            Sink::Small(p) => {
                let t = PoolBuf::alloc(2 * fabric::BLOCK).await;
                r.send(f, t.buf()).await?;
                if p.len() < fabric::BLOCK {
                    return Err(Status::Unmapped);
                }
                p[..fabric::BLOCK].copy_from_slice(&t[..fabric::BLOCK]);
                self.note_width(addr, &t[fabric::BLOCK..]);
                Ok(read_register(&t[fabric::BLOCK..]))
            }
        }
    }

    /// The bytes belonging to the register a repair round settled on.
    ///
    /// Repair carries a decision, not a page: `learn` moves a replica's entry but only
    /// pulls data where its own copy is behind or unreadable. So the copy to answer from
    /// is whichever member's metadata equals what the round chose, and a remote one is
    /// preferred — ours is the copy that just failed, either because a proposal that
    /// lost sits at the same version or because the data write behind it never landed.
    async fn pull_best(
        &'static self,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        best: Register,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let after = self.metas(addr, m, me, None).await;
        self.pull_any(addr, m, me, &after, best, sink).await
    }

    /// The bytes for a register we already know which members hold. A member whose data
    /// write never landed reports `MISSING` for a register it still carries, and one copy
    /// failing is not the group's answer, so the others are asked before the read gives
    /// up. Remote first: ours is the copy that just failed.
    async fn pull_any(
        &'static self,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        regs: &[Option<Register>; 3],
        want: Register,
        mut sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let mut last = Status::Io;
        for i in 0..3u8 {
            if Some(i) == me || regs[i as usize] != Some(want) {
                continue;
            }
            let Some(route) = self.route(m, i) else {
                continue;
            };
            match self.pull_from(route, addr, sink.reborrow()).await {
                Ok(_) => return Ok(want),
                Err(e) => last = e,
            }
        }
        match me {
            Some(k) if regs[k as usize] == Some(want) => {
                self.read_local(addr, sink).await.map(|_| want)
            }
            _ => Err(last),
        }
    }

    /// `GETMETA` at every member but `skip`. Slot `i` is member `i`'s register, or
    /// `None` if it did not answer.
    async fn metas(
        &'static self,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        skip: Option<u8>,
    ) -> [Option<Register>; 3] {
        let mut out = [None; 3];
        let mut pending: Vec<(usize, Route<'_>)> = Vec::new();
        for i in 0..3u8 {
            if Some(i) == skip {
                continue;
            }
            if Some(i) == me {
                // Our own leg goes through the same call a peer's `GETMETA` would, so a
                // member's own client reads feed the sketch too: the cache rests on the
                // owner seeing the whole read stream, not just the fabric's share.
                out[i as usize] = self.register_and_width(addr).await.ok().map(|(r, _)| r);
            } else if let Some(r) = self.route(m, i) {
                pending.push((i as usize, r));
            }
        }
        // Two at a time. A member has a local leg and so at most two to send, which is
        // the pair this joins; a non-member asking all three pays one extra round trip
        // for the odd one out, and only the shed does that.
        while let Some((i, a)) = pending.pop() {
            match pending.pop() {
                Some((j, b)) => {
                    let (x, y) = join2(self.ask_meta(a, addr), self.ask_meta(b, addr)).await;
                    out[i] = x.ok();
                    out[j] = y.ok();
                }
                None => out[i] = self.ask_meta(a, addr).await.ok(),
            }
        }
        out
    }

    /// One `GETMETA` at a peer: the metadata half of the hedged read.
    async fn ask_meta(&self, r: Route<'_>, addr: GlobalAddr) -> Result<Register, Status> {
        let (_, huge) = self.alloc.kind_of(addr)?;
        let f = self.frame(Op::GetMeta, addr, huge)?;
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        r.send(f, t.buf()).await?;
        self.note_width(addr, &t);
        Ok(read_register(&t))
    }

    /// Lift the replication width out of a reply trailer. Every read reply carries it,
    /// so the reader's hint stays current for free; the damping that keeps a width from
    /// oscillating lives in the cache.
    fn note_width(&self, addr: GlobalAddr, t: &[u8]) {
        self.cache
            .note_hint(addr, fabric::get(t, T_WIDTH).min(u8::MAX as u64) as u8);
    }
}

/// Whether a register denotes no page at all. For `Lww`/`Occ` that is version zero; an
/// `Immutable` page is encoded as `3*epoch + ordinal`, where only ordinal 1 is a live
/// fill — `3*epoch` was never written and `3*epoch + 2` is a tombstone.
fn empty(kind: Kind, version: u64) -> bool {
    match kind {
        Kind::Immutable => version % 3 != 1,
        _ => version == 0,
    }
}

/// A version nothing has ever been accepted at — what an acceptor holding nothing for
/// the address reports. Narrower than [`empty`], which also covers a tombstone: that is
/// a value, and one a round may have to preserve.
fn unwritten(kind: Kind, version: u64) -> bool {
    match kind {
        // Ordinal 0 of the epoch: the fill point, before any fill or trim.
        Kind::Immutable => version.is_multiple_of(3),
        _ => version == 0,
    }
}

/// A `(version, ballot)` held by at least two responders, with the index of one of them.
/// The whole of the read's acceptance rule.
fn matching(rs: &[Option<Register>; 3]) -> Option<(Register, u8)> {
    for i in 0..3 {
        for j in i + 1..3 {
            if let (Some(a), Some(b)) = (rs[i], rs[j])
                && a == b
            {
                return Some((a, i as u8));
            }
        }
    }
    None
}

// --------------------------------------------------------------------- acceptor side

impl Paxos {
    /// The member side of an `ACCEPT`, reached from `server::dispatch` after the frame
    /// is decoded. `imm` zero means the sender is not a member and we are the proposer;
    /// otherwise we apply at the member index it names.
    pub async fn accept(
        &'static self,
        addr: GlobalAddr,
        imm: u8,
        trailer: Option<&[u8]>,
        page: Page<'_>,
    ) -> Result<(), Status> {
        let group = self.group(addr);
        if imm == 0 {
            // The originator sent us the page and the guard; we pick the ballot and
            // drive the round, which makes the close member a proposer rather than a
            // relay.
            let guard = match trailer {
                // `DERIVED` says the sender is not a member and had nothing to observe,
                // so the guard is ours to compute — see `write`. A real guard can never
                // reach it: versions count accepts.
                Some(t) => match fabric::get(t, T_GUARD) {
                    DERIVED => None,
                    g => Some(g),
                },
                // A 4 MiB frame carries no guard, so we derive it. Legal because the
                // huge class is Immutable-only, whose guard is `3 * epoch` on every
                // replica alike.
                None => None,
            };
            return self.propose(addr, guard, page).await.map(|_| ());
        }
        self.gate_accept(addr, group).await?;
        let k = imm - 1;
        let (guard, b) = match trailer {
            Some(t) => {
                let b = Ballot::from_raw(fabric::get(t, T_BALLOT) as u32);
                // The first conjunct of the acceptance rule. A ballot below our promise
                // is a proposer that has not noticed a term bump; it learns from the
                // rejection and refreshes. A ballot above it is a promise we adopt,
                // which lets one member's prepare grant the whole group its new term.
                if b.term() < self.held_term(group).await {
                    self.stat(|s| s.accept_rejected += 1);
                    return Err(Status::Conflict { current: 0 });
                }
                self.observe(group, b.term()).await;
                (fabric::get(t, T_GUARD), b)
            }
            None => {
                // Derived on both counts: the guard from the epoch, the term from the
                // promise we hold. The proposer's index is the only thing that had to
                // travel, and two bits of `imm` carry it.
                let term = self.held_term(group).await;
                (self.alloc.guard(addr).await?, Ballot::new(term, k))
            }
        };
        let r = match page {
            Page::Small(p) => self.alloc.accept_small(addr, guard, b, p).await.map(|_| ()),
            Page::Huge(buf) => self
                .alloc
                .accept_huge(addr, guard, b, buf)
                .await
                .map(|_| ()),
        };
        self.stat(|s| match r {
            Ok(()) => s.accept_ok += 1,
            Err(Status::Conflict { .. }) => s.guard_conflicts += 1,
            Err(_) => s.accept_rejected += 1,
        });
        r
    }

    /// One piece of a 4 MiB `ACCEPT` that a transport split on the way here. The frame
    /// identity rides the LBA, so every piece names the same address and differs only
    /// in its offset; the page reaches the register when the last piece is durable.
    ///
    /// Both ends derive the guard the same way — the class is Immutable — and the
    /// ballot names the proposer, which is what lets pieces arriving on different cores
    /// agree on the page they belong to.
    pub async fn accept_part(
        &'static self,
        addr: GlobalAddr,
        imm: u8,
        off: u32,
        buf: Buf,
    ) -> Result<(), Status> {
        let group = self.group(addr);
        if imm != 0 {
            self.gate_accept(addr, group).await?;
        }
        let guard = self.alloc.guard(addr).await?;
        let b = Ballot::new(self.held_term(group).await, imm.saturating_sub(1));
        let Some(p) = self
            .alloc
            .put_huge_part(addr, guard, b, imm, off, buf)
            .await?
        else {
            return Ok(());
        };
        if imm != 0 {
            let r = self.alloc.finish(addr, p).await.map(|_| ());
            self.stat(|s| match r {
                Ok(()) => s.accept_ok += 1,
                Err(Status::Conflict { .. }) => s.guard_conflicts += 1,
                Err(_) => s.accept_rejected += 1,
            });
            return r;
        }
        // The sender is not a member, so we propose. The page only exists as a slot,
        // and a proposal has to put it on the wire, so it comes back into our own
        // memory once and the staging slot goes straight back.
        let whole = PoolBuf::alloc(layout::HUGE_PAGE as usize).await;
        let r = self.alloc.read_pending(&p, whole.buf()).await;
        self.alloc.abandon(addr, p).await;
        r?;
        self.propose(addr, None, Page::Huge(whole.buf()))
            .await
            .map(|_| ())
    }

    /// The member side of a `TRIM`.
    pub async fn accept_trim(
        &'static self,
        addr: GlobalAddr,
        imm: u8,
        trailer: &[u8],
    ) -> Result<(), Status> {
        let group = self.group(addr);
        if imm == 0 {
            return self.trim(addr).await;
        }
        self.gate_accept(addr, group).await?;
        let guard = fabric::get(trailer, T_GUARD);
        let b = Ballot::from_raw(fabric::get(trailer, T_BALLOT) as u32);
        self.alloc.accept_trim(addr, guard, b).await
    }

    /// The member side of `GETMETA`: the whole of the hedged read at a replica, and the
    /// hop that feeds the cache's width estimator.
    pub async fn get_meta(&'static self, addr: GlobalAddr, out: &mut [u8]) -> Result<(), Status> {
        let (r, w) = self.register_and_width(addr).await?;
        self.trailer(addr, r, w, out);
        Ok(())
    }

    /// The trailer a gathered `GET` carries. The register is the one the page was read
    /// under, not a fresh look: read apart from the bytes it can name a version they
    /// were never written at, and a learner would then install the pair.
    pub async fn gathered(
        &'static self,
        addr: GlobalAddr,
        r: Register,
        out: &mut [u8],
    ) -> Result<(), Status> {
        let (_, w) = self.register_and_width(addr).await?;
        self.trailer(addr, r, w, out);
        Ok(())
    }

    fn trailer(&'static self, addr: GlobalAddr, r: Register, w: u8, out: &mut [u8]) {
        put_register(out, r, w);
        fabric::put(out, T_STATE, state_bits(self.alloc, addr, r.version));
    }

    /// One hop to the core owning the address's group, for both the register and the
    /// replication width. The sketch is updated on that same hop, so the owner sees
    /// every read of the page, including reads it no longer serves the bytes for.
    async fn register_and_width(&'static self, addr: GlobalAddr) -> Result<(Register, u8), Status> {
        let owner = self.alloc.owner_core(addr)?;
        let (alloc, cache) = (self.alloc, self.cache);
        runtime::on_core(owner, move || async move {
            let r = alloc.register_local(addr)?;
            Ok((r, cache.observe_local(addr)))
        })
        .await
    }

    /// The member side of `PREPARE`. A read carries no request body, so the requested
    /// term is not on the wire: the acceptor raises its own promise by one and reports
    /// the result, and the preparer takes the maximum it hears back.
    pub async fn prepare(&'static self, addr: GlobalAddr, out: &mut [u8]) -> Result<(), Status> {
        let group = self.group(addr);
        let term = self.bump(group).await?;
        let r = self.alloc.register(addr).await?;
        out.fill(0);
        fabric::put(out, T_VERSION, r.version);
        fabric::put(out, T_BALLOT, r.ballot.raw() as u64);
        fabric::put(out, T_TERM, term as u64);
        self.stat(|s| s.prepares += 1);
        Ok(())
    }

    /// The member side of `TERM`: the promise we hold for a group. Unlike `PREPARE` it
    /// raises nothing. Read by a member recovering a promise it lost, which needs the
    /// floor we already refuse below, not a new round.
    pub async fn term(&'static self, group: u32, out: &mut [u8]) -> Result<(), Status> {
        let term = self.held_term(group).await;
        out.fill(0);
        fabric::put(out, T_TERM, term as u64);
        Ok(())
    }

    /// The member side of `LEARN`: a value we may be behind on, and the member holding
    /// it. Apply-if-newer, so a repeated learn is free and the bulk and live streams of
    /// a migration commute.
    ///
    /// `repair` additionally admits the equal-register case for a small page: our entry
    /// matches but our bytes fail their checksum, which metadata alone cannot see, so
    /// the copy that reads back replaces ours at the same register.
    pub async fn learn(
        &'static self,
        addr: GlobalAddr,
        r: Register,
        from: u8,
        repair: bool,
    ) -> Result<(), Status> {
        let group = self.group(addr);
        let (kind, huge) = self.alloc.kind_of(addr)?;
        // A register we cannot read is not a reason to refuse the value: it says we hold
        // nothing for the address, which is what makes the learn apply.
        let held = match self.alloc.register(addr).await {
            Ok(h) => h,
            Err(Status::Missing) => Register::default(),
            Err(e) => return Err(e),
        };
        if held.key() > r.key()
            || (held.key() == r.key() && (!repair || huge || empty(kind, r.version)))
        {
            self.stat(|s| s.learn_stale += 1);
            return Ok(());
        }
        if held.key() == r.key() {
            let mut page = PoolBuf::alloc(fabric::BLOCK).await;
            match self.alloc.read_small(addr, &mut page).await {
                Ok(got) if got.key() >= r.key() => {
                    self.stat(|s| s.learn_stale += 1);
                    return Ok(());
                }
                Ok(_) | Err(Status::Missing) => {}
                Err(e) => return Err(e),
            }
        }
        // A tombstone is a value with nothing behind it: no page to pull, so the
        // register alone is what a replica takes.
        if kind == Kind::Immutable && r.version % 3 == 2 {
            self.alloc.learn_tombstone(addr, r).await?;
            self.stat(|s| s.repairs += 1);
            return Ok(());
        }
        let route = match self.inbound(addr) {
            // A migration's bulk stream: the value is in the zone handing the extent
            // over, and no member of our own group has a copy to name.
            Some(z) => self.toward(z, addr)?,
            None => {
                let m = self.members(group).ok_or(Status::Unmapped)?;
                self.route(&m, from).ok_or(Status::Io)?
            }
        };
        if huge {
            let buf = PoolBuf::alloc(layout::HUGE_PAGE as usize).await;
            let got = self.pull_from(route, addr, Sink::Huge(buf.buf())).await?;
            self.alloc.learn_huge(addr, got, buf.buf()).await?;
        } else {
            let mut buf = PoolBuf::alloc(fabric::BLOCK).await;
            let got = self.pull_from(route, addr, Sink::Small(&mut buf)).await?;
            self.alloc.learn_small(addr, got, &buf, repair).await?;
        }
        self.stat(|s| s.repairs += 1);
        Ok(())
    }

    /// Whether this extent has already been frozen here.
    pub async fn sealed(&'static self, id: ShardId) -> bool {
        runtime::on_core(0, move || async move {
            self.state[0].borrow().seals.contains_key(&id)
        })
        .await
    }

    /// Freeze an extent at this zone. Every group holding pages of it must refuse later
    /// accepts, so the seal goes to every node in the catalog rather than to one group's
    /// quorum. It is idempotent — a seal already held is re-observed — so each source
    /// node driving its own fan-out is merely redundant.
    ///
    /// The term is the topology epoch: a number every node in the zone already agrees
    /// on, and monotone, which is all the seal table asks of it.
    pub async fn seal_extent(&'static self, addr: GlobalAddr, id: ShardId) -> Result<(), Status> {
        let cfg = self.alloc.config();
        let term = cfg.topology.epoch;
        let mut nodes: Vec<u32> = cfg.topology.catalog.iter().flatten().copied().collect();
        nodes.sort_unstable();
        nodes.dedup();
        for n in nodes {
            if n == cfg.node.id {
                continue;
            }
            if let Some(link) = self.link_of(n) {
                let r = Route {
                    link,
                    hops: 0,
                    imm: 0,
                };
                let _ = self.send_seal(r, addr, id, term).await;
            }
        }
        // Ours last: while it is absent this node keeps driving the fan-out, so a partial
        // round is retried rather than forgotten.
        self.seal(id, term).await
    }

    async fn send_seal(
        &self,
        r: Route<'_>,
        addr: GlobalAddr,
        id: ShardId,
        term: u32,
    ) -> Result<(), Status> {
        let f = self.frame(Op::Seal, addr, false)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        t.fill(0);
        fabric::put(&mut t, T_EXTENT, id.extent as u64);
        fabric::put(&mut t, T_SEAL_TERM, term as u64);
        r.send(f, t.buf()).await
    }

    /// Hand one register to the zone taking an extent over. `LEARN` names the value;
    /// the destination pulls the bytes from here when it turns out to be behind.
    pub async fn push(
        &'static self,
        addr: GlobalAddr,
        r: Register,
        zone: u32,
    ) -> Result<(), Status> {
        self.send_learn(self.toward(zone, addr)?, addr, r, 0, false)
            .await
    }

    /// Whether all three members now named for `addr` hold it at `version` or later.
    ///
    /// The question a node shedding a group asks before it forgets a register. All
    /// three, not a quorum: a config rollout is not atomic, so a peer may still be
    /// running the generation that names us, and a quorum of the new membership can
    /// leave a quorum of the old one — us and the member that has not caught up — able
    /// to run a round that has not seen the value. Every quorum of either membership
    /// contains a member that answered here, so no round can regress.
    ///
    /// A member that is behind, or that we hold no link to, is a `false`: the drop waits
    /// for a later sweep.
    pub async fn confirmed(&'static self, addr: GlobalAddr, version: u64) -> bool {
        let Some(m) = self.members(self.group(addr)) else {
            return false;
        };
        let me = self.self_index(&m);
        let regs = self.metas(addr, &m, me, None).await;
        regs.iter().all(|r| r.is_some_and(|r| r.version >= version))
    }

    /// The member side of `SEAL`: this node refuses every later accept for any address
    /// in the shard. Idempotent, and monotone in `term`.
    pub async fn seal(&'static self, id: ShardId, term: u32) -> Result<(), Status> {
        // An extent's pages are spread over every core, so the refusal has to be too.
        for core in 0..self.state.len() {
            runtime::on_core(core, move || async move {
                let mut l = self.state[core].borrow_mut();
                l.stats.seals += 1;
                let e = l.seals.entry(id).or_insert(term);
                *e = (*e).max(term);
            })
            .await;
        }
        self.persist().await
    }

    /// The seal's refusal, and the replay flag, in one hop to the group's own core. Both
    /// tables are empty on the common path, so this is two predictable branches on a hot
    /// cache line.
    ///
    /// A sealed shard is `Away`: this group will accept nothing further for it, so the
    /// write belongs to the zone it is being handed to. Replaying is not an error to a
    /// proposer — it says only that this node must not run the round, so the caller
    /// hands it to a member that is caught up. To an acceptor it is one, and
    /// [`Self::gate_accept`] is where that decision lives.
    async fn gate(&'static self, addr: GlobalAddr, group: u32) -> Result<Gate, Status> {
        let cfg = self.alloc.config();
        let core = self.core_of(group);
        let id = self.shard_of(addr);
        let (sealed, replaying) = runtime::on_core(core, move || async move {
            let l = self.state[core].borrow();
            (
                id.is_some_and(|id| l.seals.contains_key(&id)),
                l.replaying.contains(&group),
            )
        })
        .await;
        if !sealed {
            return Ok(Gate::Serve { replaying });
        }
        // Sealed and still here: the config has not caught up, so forward to the
        // destination the extent names rather than refusing.
        match cfg.next_zone_of(addr.0) {
            Some(z) if z != cfg.node.zone => Ok(Gate::Away(z)),
            _ => Err(Status::Conflict { current: 0 }),
        }
    }

    /// The extent `addr` falls in, as the seal table names it.
    fn shard_of(&self, addr: GlobalAddr) -> Option<ShardId> {
        let v = self.alloc.config().volume(addr.volume())?;
        let ext = v.extent_index(addr.offset() as u64)?;
        Some(ShardId {
            volume: v.id,
            extent: ext as u32,
        })
    }

    /// [`Self::gate`] for the acceptor half of a round, which may not proceed while we
    /// are replaying.
    async fn gate_accept(&'static self, addr: GlobalAddr, group: u32) -> Result<(), Status> {
        match self.gate(addr, group).await? {
            Gate::Serve { replaying: true } => Err(Status::Io),
            _ => Ok(()),
        }
    }

    /// Whether this node is still replaying `group`, and so must not be counted toward a
    /// quorum for it.
    ///
    /// A replaying node lost values it had already accepted, which is different from
    /// being merely behind. Counting it breaks the quorum intersection everything else
    /// rests on: a write acked by it and one other member survives on that member alone,
    /// and a round reaching this node and the third finds nothing at that version and is
    /// free to decide something else. It refuses accepts; `LEARN` is untouched, since
    /// that is the traffic that ends the replay.
    ///
    /// `PREPARE` is untouched too, and must be: a register the node lost reports as
    /// missing and so is already counted among the members that did not answer, the
    /// conservative side. Refusing outright would discard the registers it does hold,
    /// and — since a replay ends only when a comparison finds nothing left to repair,
    /// and repairing is a prepare round — would leave the group with no way out of the
    /// replay at all.
    pub(crate) async fn replaying(&'static self, group: u32) -> bool {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            self.state[core].borrow().replaying.contains(&group)
        })
        .await
    }

    /// The groups this core is replaying. `core_of` is the group modulo the core count,
    /// so a core's own replay set is exactly the candidates the sweep picks from, and
    /// asking is a borrow rather than a hop per group.
    pub fn replaying_here(&self) -> Vec<u32> {
        self.state[runtime::core()]
            .borrow()
            .replaying
            .iter()
            .copied()
            .collect()
    }

    /// Recover this group's promise from its other members, then rejoin it.
    ///
    /// A reformat destroys the promise table along with the registers, so a node that
    /// replays back into a group has no memory of the terms it promised at. The
    /// registers it pulls back cover every address the group holds — a ballot below the
    /// one already on a register is refused as a regression — but not an address the
    /// group holds nothing for, where any guard matches and any ballot passes. There the
    /// promise is the only thing between a stale accept and a value some round had
    /// already fixed.
    ///
    /// The highest promise the others hold is enough, and exactly enough: every value
    /// ever chosen was accepted by two members, and an acceptor adopts the term of the
    /// ballot it accepts, so for each one at least one member that is not us still
    /// promises at that term or above. Both have to answer, because hearing from one
    /// could miss precisely that member — and losing a second member while a third
    /// replays is outside what a group of three survives anyway.
    pub async fn rejoin(&'static self, group: u32) -> Result<(), Status> {
        let m = self.members(group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&m).ok_or(Status::Unmapped)?;
        let mut term = 0;
        for i in 0..3u8 {
            if i == me {
                continue;
            }
            let link = self.link_of(m[i as usize]).ok_or(Status::Io)?;
            // Group-addressed, like the anti-entropy ops: `offset` is the group and the
            // reply is a trailer whose `T_TERM` slot is the promise.
            let f = Frame::new(Op::Term, false, 0, group);
            let t = PoolBuf::alloc(fabric::BLOCK).await;
            link.send(f, t.buf()).await.map_err(Status::from_wire)?;
            term = term.max(fabric::get(&t, T_TERM) as u32);
        }
        // Durable before it is used, as in `bump`: a promise lost to a restart was never
        // a promise.
        self.observe(group, term).await;
        self.persist().await?;
        self.set_replaying(group, false).await;
        Ok(())
    }

    /// Mark a group as still replaying, or caught up. Driven by the anti-entropy
    /// sweep.
    pub async fn set_replaying(&'static self, group: u32, on: bool) {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            let mut l = self.state[core].borrow_mut();
            if on {
                l.replaying.insert(group);
            } else {
                l.replaying.remove(&group);
            }
        })
        .await;
    }
}

/// Whether this task runs the group's prepare, waits for one, or already has a term.
enum Lead {
    Held(u32),
    Wait,
    Go,
}

/// How long a writer waits for another task's prepare round before rechecking.
const PREPARE_WAIT: std::time::Duration = std::time::Duration::from_micros(20);
/// How many times it rechecks before giving up and preparing itself.
const PREPARE_WAITS: usize = 256;

// --------------------------------------------------------------------- terms, repair

impl Paxos {
    /// The term this node may issue one-shot accepts at. A term read back from the
    /// superblock is not held until it has been raised once, so a restart costs one
    /// prepare per group and never a stale ballot.
    async fn term_for(&'static self, group: u32, addr: GlobalAddr) -> Result<u32, Status> {
        let core = self.core_of(group);
        // One prepare per group at a time. Every prepare raises both peers' promises by
        // one, so letting concurrent writes to the same group each run their own turns a
        // burst into a term escalation the accepts can never catch: each is refused as
        // stale, refreshes, and prepares again. Waiting out the leader's round costs the
        // loser a few microseconds and keeps all three members in step.
        for _ in 0..PREPARE_WAITS {
            let take = runtime::on_core(core, move || async move {
                let mut l = self.state[core].borrow_mut();
                let e = l.terms.entry(group).or_insert(Term::new(0));
                match (e.held, e.preparing) {
                    (true, _) => Lead::Held(e.value),
                    (false, true) => Lead::Wait,
                    (false, false) => {
                        e.preparing = true;
                        Lead::Go
                    }
                }
            })
            .await;
            match take {
                Lead::Held(t) => return Ok(t),
                Lead::Wait => {
                    runtime::sleep(PREPARE_WAIT).await;
                    continue;
                }
                Lead::Go => {
                    let r = self.prepare_round(addr, None).await;
                    runtime::on_core(core, move || async move {
                        if let Some(t) = self.state[core].borrow_mut().terms.get_mut(&group) {
                            t.preparing = false;
                        }
                    })
                    .await;
                    return r.map(|(t, ..)| t);
                }
            }
        }
        self.prepare_round(addr, None).await.map(|(t, ..)| t)
    }

    /// The term we currently promise, without raising it. Used where a ballot has to
    /// be derived rather than sent.
    async fn held_term(&'static self, group: u32) -> u32 {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            self.state[core]
                .borrow()
                .terms
                .get(&group)
                .map_or(0, |t| t.value)
        })
        .await
    }

    /// Give up the right to issue one-shot accepts at the term we hold, so the next
    /// `term_for` runs a prepare round. The promise itself is untouched: durable, and
    /// only ever rising.
    async fn refresh(&'static self, group: u32) {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            if let Some(t) = self.state[core].borrow_mut().terms.get_mut(&group) {
                t.held = false;
            }
        })
        .await;
    }

    /// Raise this group's promise by one and return it. Durable before it is used, so a
    /// promise never dies with the process.
    ///
    /// The bump also takes the right to issue one-shot accepts at the new term, on the
    /// acceptor side of someone else's `PREPARE` as much as on our own. Withholding it
    /// there measured worse: an acceptor that has just raised its promise is the
    /// cheapest proposer in the group, and making it prepare again to learn a term it
    /// already knows costs a round trip on every write.
    async fn bump(&'static self, group: u32) -> Result<u32, Status> {
        let core = self.core_of(group);
        let t = runtime::on_core(core, move || async move {
            let mut l = self.state[core].borrow_mut();
            l.stats.term_bumps += 1;
            let e = l.terms.entry(group).or_insert(Term::new(0));
            e.value = e.value.saturating_add(1) & 0x3fff_ffff;
            e.held = true;
            e.value
        })
        .await;
        self.persist().await?;
        Ok(t)
    }

    /// Record a term another member reported. Never marks it held: only our own `bump`
    /// grants the right to issue one-shot accepts at it, so a term that rises under us
    /// takes the right away again. Without that a proposer keeps issuing at the raised
    /// term, where another member's ballot is higher at the same term, and every accept
    /// is refused as a ballot regression forever. The prepare `term_for` then runs is
    /// how a member refreshes after a rejected accept.
    async fn observe(&'static self, group: u32, term: u32) {
        let core = self.core_of(group);
        runtime::on_core(core, move || async move {
            let mut l = self.state[core].borrow_mut();
            let e = l.terms.entry(group).or_insert(Term::new(term));
            if term > e.value {
                e.value = term;
                e.held = false;
            }
        })
        .await;
    }

    /// The prepare round: raise the term at a quorum, and decide which of the reported
    /// registers was chosen.
    ///
    /// The classical rule is "highest version, ties on ballot", which is not enough on
    /// its own: a losing one-shot can sit on a single acceptor at the same version as
    /// the chosen value and with a higher ballot, and picking it would resurrect a value
    /// that never reached a quorum. So a `(version, ballot)` held by a majority wins
    /// outright; a three-way split at one version means nothing was chosen and the
    /// highest ballot is a free choice; and two responses that disagree at the top
    /// version are unresolvable in this round, so we retry rather than guess.
    async fn prepare_round(
        &'static self,
        addr: GlobalAddr,
        below: Option<Register>,
    ) -> Result<(u32, Register, bool), Status> {
        // An unresolvable top version is a race, not an answer, and it is not the
        // client's `Conflict` either: retry it here so the only `Conflict` a caller
        // sees is a guard mismatch.
        let mut last = self.prepare_once(addr, below).await;
        for _ in 1..PREPARE_RETRIES {
            if !matches!(last, Err(Status::Conflict { .. })) {
                break;
            }
            last = self.prepare_once(addr, below).await;
        }
        last
    }

    async fn prepare_once(
        &'static self,
        addr: GlobalAddr,
        below: Option<Register>,
    ) -> Result<(u32, Register, bool), Status> {
        let group = self.group(addr);
        let m = self.members(group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&m);
        let need = self.quorum();

        let mut regs: [Option<Register>; 3] = [None; 3];
        let mut terms: Vec<u32> = Vec::new();
        let mut answered = 0;

        // Our own promise counts on the same terms a peer's does.
        if let Some(k) = me {
            let t = self.bump(group).await?;
            // A register we lost is not a vote for version zero. Leaving it out is the
            // same as not answering, the conservative side of the count.
            match self.alloc.register(addr).await {
                Ok(r) => {
                    regs[k as usize] = Some(r);
                    terms.push(t);
                    answered += 1;
                }
                Err(Status::Missing) => {}
                Err(e) => return Err(e),
            }
        }
        // Ask both peers at once. The count below is only as good as the answers it has,
        // so this waits for every one — but a member that is gone must cost one link
        // timeout for the round, not one apiece.
        let mut pending: Vec<(usize, Route<'_>)> = (0..3u8)
            .filter(|i| Some(*i) != me)
            .filter_map(|i| self.route(&m, i).map(|r| (i as usize, r)))
            .collect();
        let mut take = |i: usize, r: Result<(Register, u32), Status>| {
            if let Ok((reg, t)) = r {
                regs[i] = Some(reg);
                terms.push(t);
                answered += 1;
            }
        };
        match (pending.pop(), pending.pop()) {
            (None, _) => {}
            (Some((i, a)), None) => take(i, self.send_prepare(a, addr).await),
            (Some((i, a)), Some((j, b))) => {
                let (x, y) = join2(self.send_prepare(a, addr), self.send_prepare(b, addr)).await;
                take(i, x);
                take(j, y);
            }
        }
        if answered < need {
            self.stat(|s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }

        // The term to propose at is the highest any responder promised: an acceptor
        // rejects a ballot below its own promise and takes one at or above it. Members
        // left behind catch up from the ballot on the accept that follows, so nothing
        // here has to make them agree first — asking again would not, since each
        // acceptor raises only its own promise.
        let term = terms.iter().copied().max().unwrap_or(1);
        self.observe(group, term).await;

        // A value at a version can still be chosen only if the acceptors we did not hear
        // from could carry it to a quorum, so count each distinct value's votes plus the
        // silent members. Exactly one candidate must be preserved; two means the
        // acceptor that decides between them is the one that stayed silent.
        //
        // None at all means nothing was chosen *at that version*, which is not the same
        // as nothing having been chosen: a one-shot that reached a single acceptor
        // leaves it a version ahead of a value two others agreed on, and taking it
        // because it sits on top would drop a write that was acknowledged. So walk the
        // versions downwards and answer with the first one a quorum could stand behind.
        let unseen = 3 - answered;
        let mut vers: Vec<u64> = regs.iter().flatten().map(|r| r.version).collect();
        vers.sort_unstable_by(|a, b| b.cmp(a));
        vers.dedup();
        let mut chosen = None;
        let kind = self.alloc.kind_of(addr)?.0;
        // A value chosen at some version left a quorum standing at or above it, and
        // registers only advance, so the highest version a quorum could still be
        // standing at is a floor. Answering below it would replace whatever was chosen
        // there with something older, even when every copy of it has been overwritten
        // and no answer can name it any more.
        let floor = vers
            .iter()
            .copied()
            .find(|v| regs.iter().flatten().filter(|r| r.version >= *v).count() + unseen >= need)
            .unwrap_or_default();
        for v in vers.iter().copied() {
            if v < floor {
                break;
            }
            // A quorum holding nothing is not a decision to hold nothing: it says only
            // that no value has been chosen yet, which leaves the choice free. Stopping
            // here would answer with a register no member can supply the bytes for, and
            // a proposer already a version above it would be stuck guarding against one
            // nobody else will ever reach.
            if unwritten(kind, v) {
                continue;
            }
            let at: Vec<Register> = regs
                .iter()
                .flatten()
                .copied()
                .filter(|r| r.version == v)
                .collect();
            // A member that has moved past this version says nothing about it: it once
            // stood here and its answer has been overwritten, so it counts with the
            // silent rather than against. Only a member still below is a real denial.
            let above = regs.iter().flatten().filter(|r| r.version > v).count();
            let mut cands: Vec<Register> = Vec::new();
            // A quorum of answers holding the same value is proof rather than
            // possibility, and there can only be one such. Proof settles the version
            // outright: without it two values that merely *could* have been chosen are
            // indistinguishable, and with it they are not.
            let mut sure = None;
            for r in at.iter().copied() {
                let exact = at.iter().filter(|x| **x == r).count();
                if exact >= need {
                    sure = Some(r);
                }
                if !cands.contains(&r) && exact + unseen + above >= need {
                    cands.push(r);
                }
            }
            if sure.is_some() {
                chosen = sure;
                break;
            }
            match cands.len() {
                1 => {
                    chosen = Some(cands[0]);
                    break;
                }
                0 => continue,
                // Two values that could each have been chosen, and no silent member left
                // to tell them apart. Whoever moved past this version built on one of
                // them, so the register above supersedes both; waiting for an answer that
                // cannot come would only stall the page.
                _ if unseen == 0 => {
                    chosen = regs.iter().flatten().copied().max_by_key(|r| r.key());
                    break;
                }
                _ => return Err(Status::Conflict { current: 0 }),
            }
        }
        // Nothing at or above the floor could have reached a quorum, so every register
        // still on offer there is a free choice; take the newest, which is the one a
        // writer would build on. `below` narrows that choice when the caller has already
        // found the newer ones unusable, which is how a value whose only copy is
        // unreadable stops being an answer nobody can act on. It is deliberately ignored
        // once something *was* chosen: then the value is the group's, readable or not.
        let best = match chosen {
            Some(r) => r,
            None => regs
                .iter()
                .flatten()
                .copied()
                .filter(|r| r.version >= floor && below.is_none_or(|b| r.key() < b.key()))
                .max_by_key(|r| r.key())
                .ok_or(Status::Missing)?,
        };
        Ok((term, best, chosen.is_none()))
    }

    async fn send_prepare(
        &self,
        r: Route<'_>,
        addr: GlobalAddr,
    ) -> Result<(Register, u32), Status> {
        let (_, huge) = self.alloc.kind_of(addr)?;
        let f = self.frame(Op::Prepare, addr, huge)?;
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        r.send(f, t.buf()).await?;
        Ok((read_register(&t), fabric::get(&t, T_TERM) as u32))
    }

    /// Prepare, then copy the chosen value to a quorum.
    ///
    /// Classically the write-back is the one unguarded write in the system; here it is
    /// apply-if-newer, strictly weaker in what it will overwrite and so unable to
    /// regress a version. That is what makes the term unnecessary for safety, and what
    /// collapses repair and `learn` into one operation.
    ///
    /// Returns the register the round settled on, the only authoritative answer anyone
    /// has about a key whose nearest copy came back `MISSING`.
    pub async fn repair(&'static self, addr: GlobalAddr) -> Result<Register, Status> {
        // A free choice that nobody can serve is no answer: the copy holding it may have
        // lost its bytes. Nothing was chosen in that case, so stepping down to the next
        // register on offer is legal, and the only choice that converges.
        let mut below = None;
        let mut last = Err(Status::Io);
        for _ in 0..3 {
            let (_, best, free) = self.prepare_round(addr, below).await?;
            if below.is_some_and(|b: Register| best.key() >= b.key()) {
                break;
            }
            last = self.settle(addr, best).await;
            // Only a round that found nothing chosen may step down: `free` is that
            // certificate, and without it the value is the group's answer whether or not
            // a copy of it can still be read.
            if !free || !matches!(last, Err(Status::Missing | Status::Io)) {
                break;
            }
            below = Some(best);
        }
        last
    }

    /// Copy `best` to a quorum. Split out so a free choice nobody can serve can be
    /// retried one register down.
    async fn settle(&'static self, addr: GlobalAddr, best: Register) -> Result<Register, Status> {
        let group = self.group(addr);
        let m = self.members(group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&m);
        // Whoever holds readable bytes for the winning value is the source every laggard
        // pulls from; metadata alone cannot detect a damaged small page.
        let regs = self.metas(addr, &m, me, None).await;
        let src = self.repair_source(addr, best, &m, me, &regs).await?;
        // The prepare phase only *selects* a value; it becomes the group's answer once a
        // quorum holds it. The source does already, so count the learns that land and
        // refuse to call the round authoritative until they add up. Reporting a value
        // only one acceptor carries would let the next round — which need not reach that
        // acceptor — settle on a different one, and a client told the first would then
        // see the second. A member already past `best` counts: it built what it holds on
        // top, and the next round reads it that way too.
        let mut held = 1;
        let rest: Vec<u8> = (0..3u8).filter(|i| *i != src).collect();
        let (x, y) = join2(
            self.hand(addr, best, src, &m, me, rest[0]),
            self.hand(addr, best, src, &m, me, rest[1]),
        )
        .await;
        held += x as usize + y as usize;
        if held < self.quorum() {
            self.stat(|s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }
        self.stat(|s| s.repairs += 1);
        Ok(best)
    }

    /// Put one more copy of the chosen value where it belongs: our own slab if we are
    /// the target, otherwise a `LEARN` telling that member where to pull it from.
    async fn hand(
        &'static self,
        addr: GlobalAddr,
        best: Register,
        src: u8,
        m: &[u32; 3],
        me: Option<u8>,
        i: u8,
    ) -> bool {
        if Some(i) == me {
            self.learn(addr, best, src, true).await.is_ok()
        } else if let Some(r) = self.route(m, i) {
            self.send_learn(r, addr, best, src, true).await.is_ok()
        } else {
            false
        }
    }

    async fn send_learn(
        &self,
        route: Route<'_>,
        addr: GlobalAddr,
        r: Register,
        from: u8,
        repair: bool,
    ) -> Result<(), Status> {
        let (_, huge) = self.alloc.kind_of(addr)?;
        let f = self.frame(Op::Learn, addr, huge)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        t.fill(0);
        fabric::put(&mut t, T_VERSION, r.version);
        fabric::put(&mut t, T_BALLOT, r.ballot.raw() as u64);
        fabric::put(&mut t, T_SOURCE, from as u64);
        fabric::put(&mut t, T_REPAIR, repair as u64);
        route.send(f, t.buf()).await
    }

    async fn repair_source(
        &'static self,
        addr: GlobalAddr,
        best: Register,
        m: &[u32; 3],
        me: Option<u8>,
        regs: &[Option<Register>; 3],
    ) -> Result<u8, Status> {
        let (kind, huge) = self.alloc.kind_of(addr)?;
        if empty(kind, best.version) {
            return regs
                .iter()
                .position(|r| *r == Some(best))
                .map(|i| i as u8)
                .ok_or(Status::Missing);
        }

        let mut last = Status::Missing;
        if huge {
            let page = PoolBuf::alloc(layout::HUGE_PAGE as usize).await;
            for i in 0..3u8 {
                if regs[i as usize] != Some(best) {
                    continue;
                }
                let got = if Some(i) == me {
                    self.read_local(addr, Sink::Huge(page.buf())).await
                } else if let Some(route) = self.route(m, i) {
                    self.pull_from(route, addr, Sink::Huge(page.buf())).await
                } else {
                    Err(Status::Io)
                };
                match got {
                    Ok(r) if r == best => return Ok(i),
                    Ok(_) => last = Status::Missing,
                    Err(e) => last = e,
                }
            }
        } else {
            let mut page = PoolBuf::alloc(fabric::BLOCK).await;
            for i in 0..3u8 {
                if regs[i as usize] != Some(best) {
                    continue;
                }
                let got = if Some(i) == me {
                    self.read_local(addr, Sink::Small(&mut page)).await
                } else if let Some(route) = self.route(m, i) {
                    self.pull_from(route, addr, Sink::Small(&mut page)).await
                } else {
                    Err(Status::Io)
                };
                match got {
                    Ok(r) if r == best => return Ok(i),
                    Ok(_) => last = Status::Missing,
                    Err(e) => last = e,
                }
            }
        }
        Err(last)
    }

    /// Write the promise table and the seal table back to the superblock. Both are tiny
    /// and change rarely, so they can afford a full rewrite and the superblock's
    /// redundancy rather than the mblocks' delta scheme.
    async fn persist(&'static self) -> Result<(), Status> {
        let mut c = layout::Consensus::default();
        for i in 0..self.state.len() {
            let (terms, seals) = runtime::on_core(i, move || async move {
                let l = self.state[i].borrow();
                let t: Vec<(u32, u32)> = l.terms.iter().map(|(&g, x)| (g, x.value)).collect();
                let s: Vec<(ShardId, u32)> = if i == 0 {
                    l.seals.iter().map(|(&k, &v)| (k, v)).collect()
                } else {
                    Vec::new()
                };
                (t, s)
            })
            .await;
            c.terms.extend(terms);
            c.seals
                .extend(seals.into_iter().map(|(k, term)| layout::Seal {
                    volume: k.volume,
                    extent: k.extent,
                    term,
                }));
        }
        c.terms.sort_unstable();
        c.terms.truncate(layout::MAX_TERMS);
        c.seals.truncate(layout::MAX_SEALS);
        self.alloc.save_consensus(&c).await
    }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

/// Write a register, and the cache width beside it, into a reply trailer. Here rather
/// than in `server` because this is where the slot names live.
pub fn put_register(out: &mut [u8], r: Register, width: u8) {
    out.fill(0);
    fabric::put(out, T_VERSION, r.version);
    fabric::put(out, T_BALLOT, r.ballot.raw() as u64);
    fabric::put(out, T_WIDTH, width as u64);
}

fn read_register(t: &[u8]) -> Register {
    Register {
        version: fabric::get(t, T_VERSION),
        ballot: Ballot::from_raw(fabric::get(t, T_BALLOT) as u32),
    }
}

/// The `state` slot of a `GETMETA` reply: the Immutable state machine's ordinal, and
/// zero for the mutable types, which have no state outside the version.
fn state_bits(a: &'static Allocator, addr: GlobalAddr, version: u64) -> u64 {
    match a.kind_of(addr) {
        Ok((Kind::Immutable, _)) => version % 3,
        _ => 0,
    }
}

/// Member indices ordered so the ones `adjacent` accepts come first. A stable
/// partition: order within each half stays member order, so a full mesh and a group
/// with no adjacent member both read in plain member order.
fn nearest_first(m: &[u32; 3], adjacent: impl Fn(u32) -> bool) -> [u8; 3] {
    let mut out = [0u8; 3];
    let mut n = 0;
    for near in [true, false] {
        for i in 0..3u8 {
            if adjacent(m[i as usize]) == near {
                out[n] = i;
                n += 1;
            }
        }
    }
    out
}

/// Two futures, awaited together and held in place. `runtime::quorum` takes an array,
/// so it cannot combine two legs of different shape.
fn join2<A: Future, B: Future>(a: A, b: B) -> impl Future<Output = (A::Output, B::Output)> {
    Join2 {
        a,
        b,
        ra: None,
        rb: None,
    }
}

struct Join2<A: Future, B: Future> {
    a: A,
    b: B,
    ra: Option<A::Output>,
    rb: Option<B::Output>,
}

impl<A: Future, B: Future> Future for Join2<A, B> {
    type Output = (A::Output, B::Output);

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // SAFETY: neither future is ever moved — both are polled through a projection of
        // the pinned parent, which is what lets this hold a `!Unpin` cross-core hop.
        let s = unsafe { self.get_unchecked_mut() };
        if s.ra.is_none()
            && let Poll::Ready(v) = unsafe { Pin::new_unchecked(&mut s.a) }.poll(cx)
        {
            s.ra = Some(v);
        }
        if s.rb.is_none()
            && let Poll::Ready(v) = unsafe { Pin::new_unchecked(&mut s.b) }.poll(cx)
        {
            s.rb = Some(v);
        }
        if s.ra.is_some() && s.rb.is_some() {
            Poll::Ready((s.ra.take().unwrap(), s.rb.take().unwrap()))
        } else {
            Poll::Pending
        }
    }
}

/// A `LEARN` trailer: the value, its source, and whether equal bytes need validation.
pub fn learn_trailer(t: &[u8]) -> (Register, u8, bool) {
    let r = Register {
        version: fabric::get(t, T_VERSION),
        ballot: Ballot::from_raw(fabric::get(t, T_BALLOT) as u32),
    };
    (
        r,
        fabric::get(t, T_SOURCE) as u8,
        fabric::get(t, T_REPAIR) != 0,
    )
}

/// A `SEAL` trailer: which shard, and the term the source group sealed it at. The
/// volume comes from the frame, which names one already.
pub fn seal_trailer(volume: u32, t: &[u8]) -> (ShardId, u32) {
    let id = ShardId {
        volume,
        extent: fabric::get(t, T_EXTENT) as u32,
    };
    (id, fabric::get(t, T_SEAL_TERM) as u32)
}

/// Frame shapes the target accepts for an `ACCEPT`. Exposed so `server` and this file
/// cannot drift.
pub fn accept_parts(huge: bool, part: Part) -> bool {
    if huge {
        // Any piece of the page: one 4 MiB command is split at the target's MDTS and
        // arrives as consecutive requests the acceptor puts back together by offset.
        matches!(part, Part::Payload { .. })
    } else {
        part == Part::Both
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A ballot is one `u32` compared numerically, so the term must sit above the member
    /// index, and both must survive the round trip through the wire.
    #[test]
    fn ballots_order_by_term_then_member() {
        let b = Ballot::new(0x2fff_ffff, 3);
        assert_eq!(b.term(), 0x2fff_ffff);
        assert_eq!(b.member(), 3);
        assert_eq!(Ballot::from_raw(b.raw()), b);
        assert!(Ballot::new(5, 0) > Ballot::new(4, 3));
        assert!(Ballot::new(5, 2) > Ballot::new(5, 1));
        // A term wider than 30 bits is not representable and must not bleed into the
        // member index, which is what makes the packing safe to saturate against.
        assert_eq!(Ballot::new(u32::MAX, 1).member(), 1);
    }

    /// The data leg asks an adjacent member before a relayed one, because a forwarded
    /// `GET` puts the page on the wire twice. A stable partition, so a full mesh keeps
    /// reading in member order.
    #[test]
    fn data_leg_prefers_an_adjacent_member() {
        let m = [10, 11, 12];
        assert_eq!(
            nearest_first(&m, |_| true),
            [0, 1, 2],
            "a full mesh is unchanged"
        );
        assert_eq!(
            nearest_first(&m, |_| false),
            [0, 1, 2],
            "so is a group we cannot see"
        );
        // No link to member 0, so the adjacent member goes first.
        assert_eq!(nearest_first(&m, |n| n == 11), [1, 0, 2]);
        assert_eq!(nearest_first(&m, |n| n == 12), [2, 0, 1]);
        // Adjacent members keep their relative order, and every index appears once.
        let o = nearest_first(&m, |n| n != 11);
        assert_eq!(o, [0, 2, 1]);
        let mut seen = o;
        seen.sort_unstable();
        assert_eq!(seen, [0, 1, 2], "the failover order still covers the group");
    }
}
