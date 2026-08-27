//! CASPaxos over the page register.
//!
//! One register per page, three acceptors, quorum two. No log: the register *is* the
//! allocator's mblock entry. Durability is `alloc.rs`, framing `fabric.rs`, placement the
//! config, migration `heal.rs`.
//!
//! A version guard replaces the prepare phase: a proposer sends one message and an acceptor
//! applies it only if the register still holds the guarded version. Two proposals racing the
//! same guard cannot both reach two of three, so one round trip is safe without Fast Paxos's
//! larger quorums; the loser is rejected.

use std::cell::RefCell;
use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::alloc::{Allocator, GlobalAddr, Pending, Status};
use crate::cache::Cache;
use crate::config::{GroupId, Kind};
use crate::fabric::{
    self, Cmd, Footer, GroupIx, Hops, Link, Member, Op, PageRef, Source, To, Via, Want,
};
use crate::layout;
use crate::runtime::PoolBuf;
use crate::runtime::{self, Buf, CoreId};
use crate::server::{Server, Worker};

mod acceptor;
pub(crate) mod heal;
mod protocol;
mod read;
mod recovery;
mod routing;
mod warm;
mod write;

use heal::Heal;
pub use protocol::{Ballot, Register};
use protocol::{Choice, Gate, Settled, Term, carried, choose, promised, sealed_at};
pub(crate) use protocol::{admits, supersedes};

/// How many of a zone's gateways one operation tries before calling the zone unavailable.
/// Each try costs the fabric's full timeout.
const GATEWAY_TRIES: usize = 3;

/// How many times a prepare round is re-run when it cannot tell which of two values at the
/// top version was chosen. The member that would decide the race stayed silent, and another
/// round usually reaches it.
const PREPARE_RETRIES: u32 = 4;

/// The bytes a proposal carries: one 4 KiB block, staged through our own registered
/// memory because a mutable block is checksummed there.
#[derive(Clone, Copy)]
pub enum Page<'a> {
    Small(&'a PoolBuf),
}

/// Where a read delivers. The mirror of [`Page`].
pub enum Sink<'a> {
    Small(&'a mut PoolBuf),
}

impl<'a> Sink<'a> {
    fn reborrow(&mut self) -> Sink<'_> {
        match self {
            Sink::Small(p) => Sink::Small(p),
        }
    }

    /// The registered memory behind the sink, so a block just read reaches the cache
    /// without a copy.
    fn buf(&self) -> Buf {
        match self {
            Sink::Small(p) => p.buf(),
        }
    }
}

// --- per-core state ---

/// The consensus counters. One set per core; the exporter sums them.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub accept_ok: u64,
    pub accept_rejected: u64,
    pub one_shot: u64,
    pub guard_conflicts: u64,
    pub prepares: u64,
    pub term_bumps: u64,
    pub repairs: u64,
    pub read_matched: u64,
    pub read_remote_match: u64,
    pub read_failed: u64,
    pub learn_stale: u64,
    pub seals: u64,
    pub groups_unavailable: u64,
    /// Cross-zone operations that fell through to a lower-ranked gateway because the one the
    /// ring named did not answer. Retries, not failures.
    pub gateway_retries: u64,
    /// Cross-zone operations abandoned because no gateway of the target zone answered, or
    /// we hold a link to none of them.
    pub zones_unavailable: u64,
    /// `WARM` frames sent toward a zone an extent asks to keep warm.
    pub warms_sent: u64,
    /// `WARM` frames this node acted on: relayed to the cohort winners, or pulled into its
    /// own cache.
    pub warms_taken: u64,
    /// `WARM` frames dropped: no core capacity, allocator pressure, a cache that declined,
    /// or a pull that did not land.
    pub warms_dropped: u64,
}

/// Consensus state for the groups that hash to this core.
///
/// Lives in that core's [`crate::server::CoreState`], so there is no lock and nothing shared: the
/// only way to a row is a transaction the owning worker runs.
#[derive(Default)]
pub(crate) struct Local {
    terms: BTreeMap<GroupId, Term>,
    /// Extents sealed here, and the term the seal was taken at. Keyed by extent id alone:
    /// those are unique across every universe.
    seals: BTreeMap<u32, u32>,
    /// Addresses with a proposal in flight. The one-value-per-ballot rule, and the write
    /// path's per-key serialization, are the same table.
    inflight: BTreeSet<u64>,
    /// Groups with a prepare round in flight from this node. Held through a prepare-round
    /// lease, which is what puts a group in here and the only thing that takes it out.
    preparing: BTreeSet<GroupId>,
    /// Groups we are still replaying. Set by the anti-entropy sweep when it finds our whole
    /// side of a group empty against a peer that has data, cleared when the digests agree.
    replaying: BTreeSet<GroupId>,
    stats: Stats,
}

// --- Paxos ---

/// How to reach one member of a group: which peer to hand the frame to, who it is for, and
/// how many forwards it may take.
///
/// `imm` is the address encoding, uniform across ops: zero means "you own this, resolve it
/// yourself", `k + 1` names member index `k`. A receiver that is not the addressee forwards
/// and spends a hop, which is how a leg reaches a member we hold no link to and how a node
/// with no catalog for a remote zone routes by zone alone.
#[derive(Clone)]
struct Route {
    worker: Rc<Worker>,
    universe: u32,
    node: u32,
    /// Who the command is for and how many forwards it may take.
    via: Via,
}

impl Route {
    /// The addressing every command sent on this route carries.
    fn via(&self) -> Via {
        self.via
    }

    fn link(&self) -> &Link {
        self.worker
            .compiled()
            .link(self.universe, self.node)
            .expect("route link belongs to its worker generation")
    }

    async fn send(&self, cmd: Cmd, buf: Buf) -> Result<(), Status> {
        self.link().send(cmd, buf).await.map_err(Status::from_wire)
    }
}

pub struct Paxos {
    alloc: &'static Allocator,
    cache: &'static Cache,
    heal: std::sync::OnceLock<&'static Heal>,
}

/// One worker's share of the consensus state, living in that worker's [`crate::server::CoreState`].
///
/// A group's promise is not a copy of anything: it is held by exactly one core, and every
/// ballot issued under it comes from there. Putting the row where the core can reach it and
/// nowhere else is what makes that a fact rather than a convention.
pub(crate) struct Row(RefCell<Local>);

/// This core's share of the consensus state.
fn here<T>(worker: &Worker, f: impl FnOnce(&mut Local) -> T) -> T {
    f(&mut worker.core().paxos.0.borrow_mut())
}

/// `core`'s share, as a transaction that core runs.
///
/// Every one of these bodies is a table lookup or a small mutation, which is why they are
/// transactions and not hops: the owning worker settles each inside the drain that carried
/// it, rather than parking a future to be polled again.
fn at<T, F>(core: CoreId, f: F) -> impl Future<Output = T>
where
    F: FnOnce(&Worker, &mut Local) -> T + Send + 'static,
    T: Send + 'static,
{
    runtime::to::<Server, _, _>(core, move |_, worker| {
        f(worker, &mut worker.core().paxos.0.borrow_mut())
    })
}

/// Build the consensus layer and leak it, with one row per worker for the runtime to hand
/// out. Leaked because hop closures must be `Send + 'static`, which a borrow of the control
/// thread's stack cannot be.
pub fn open(
    alloc: &'static Allocator,
    cache: &'static Cache,
    cores: usize,
) -> (&'static Paxos, Vec<Row>) {
    let mut state: Vec<Local> = (0..cores).map(|_| Local::default()).collect();
    // A term lives on the core that owns its group, where every later read of it happens.
    let boot = alloc.boot_consensus();
    for &(group, value) in &boot.terms {
        let l = &mut state[group.index() as usize % cores].terms;
        l.insert(group, Term::new(value));
    }
    // A seal covers a whole extent, whose pages spread over every core, so unlike a term
    // it is replicated rather than placed.
    for s in &boot.seals {
        for l in state.iter_mut() {
            l.seals.insert(s.extent, s.term);
        }
    }
    let paxos = Box::leak(Box::new(Paxos {
        alloc,
        cache,
        heal: std::sync::OnceLock::new(),
    }));
    (
        paxos,
        state.into_iter().map(|l| Row(RefCell::new(l))).collect(),
    )
}

/// Whether a register denotes no page at all. For `Mutable` that is version zero; an
/// `Immutable` page is `3*epoch + ordinal`, where only ordinal 1 is a live fill: `3*epoch`
/// was never written and `3*epoch + 2` is a tombstone.
fn empty(kind: Kind, version: u64) -> bool {
    match kind {
        Kind::Immutable => version % 3 != 1,
        _ => version == 0,
    }
}

/// A version nothing has ever been accepted at, reported by an acceptor holding nothing for
/// the address. Narrower than [`empty`], which also covers a tombstone: that is a value, and
/// one a round may have to preserve.
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

// --- helpers ---

/// The register a metadata reply carries. The wire form holds the same two fields as
/// scalars, because `fabric` owns no consensus types.
impl From<Register> for fabric::Reg {
    fn from(r: Register) -> fabric::Reg {
        fabric::Reg {
            version: r.version,
            ballot: r.ballot.raw(),
        }
    }
}

impl From<fabric::Reg> for Register {
    fn from(r: fabric::Reg) -> Register {
        Register {
            version: r.version,
            ballot: Ballot::from_raw(r.ballot),
        }
    }
}

/// The register out of a metadata reply, discarding the width hint. Used where the reader
/// owns no sketch to feed: the cache legs and the 4 MiB metadata round.
fn read_register(t: &[u8]) -> Result<Register, Status> {
    Ok(read_meta(t)?.0)
}

/// A metadata reply: the register, and the cache width hint riding beside it.
fn read_meta(t: &[u8]) -> Result<(Register, u8), Status> {
    let m = fabric::MetaReply::decode(t).map_err(Status::from_wire)?;
    Ok((m.reg.into(), m.width))
}

/// The `state` field of a `GETMETA` reply: the Immutable state machine's ordinal, and zero
/// for the mutable types, which have no state outside the version.
fn state_of(
    a: &'static Allocator,
    worker: &Worker,
    addr: GlobalAddr,
    version: u64,
) -> fabric::State {
    match a.kind_of(worker, addr) {
        Ok(Kind::Immutable) => {
            fabric::State::new((version % 3) as u8).unwrap_or(fabric::State::ZERO)
        }
        _ => fabric::State::ZERO,
    }
}

/// Member indices ordered so the ones `adjacent` accepts come first. A stable partition:
/// order within each half stays member order, so a full mesh and a group with no adjacent
/// member both read in plain member order.
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

/// Two futures, awaited together and held in place. `runtime::quorum` takes an array, so it
/// cannot combine two legs of different shape.
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
        // SAFETY: neither future is ever moved. Both are polled through a projection of
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
        // A term wider than 30 bits is not representable and must not bleed into the member
        // index, which makes the packing safe to saturate against.
        assert_eq!(Ballot::new(u32::MAX, 1).member(), 1);
    }

    /// The data leg asks an adjacent member before a relayed one, because a forwarded `GET`
    /// puts the page on the wire twice. A stable partition, so a full mesh keeps member
    /// order.
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
        // Adjacent members keep their relative order.
        assert_eq!(nearest_first(&m, |n| n != 11), [0, 2, 1]);
    }

    fn reg(version: u64, term: u32, member: u8) -> Option<Register> {
        Some(Register {
            version,
            ballot: Ballot::new(term, member),
        })
    }

    /// A quorum of the answers naming the identical register is proof, and proof is all a
    /// round needs. The member it could not reach cannot be holding a value a quorum agreed
    /// on, because the two that answered are that quorum.
    #[test]
    fn a_quorum_of_answers_settles_without_the_third_member() {
        let r = reg(7, 1, 0);
        let regs = [r, r, None];
        assert_eq!(
            choose(&regs, 2, 1, 2, Kind::Occ, None),
            Choice::Chosen(r.unwrap())
        );
    }

    /// The shape a failed one-shot leaves behind: one member a version ahead of a value the
    /// other two agreed on. A round that reaches the pair keeps their value; a round that
    /// reaches the straggler and one of them cannot tell the two apart and must refuse,
    /// because taking the straggler would replace an answer a reader already saw.
    #[test]
    fn a_straggler_above_a_quorum_never_wins() {
        let (kept, orphan) = (reg(7, 1, 0), reg(8, 1, 2));
        assert_eq!(
            choose(&[kept, kept, orphan], 3, 0, 2, Kind::Occ, None),
            Choice::Chosen(kept.unwrap()),
            "a complete view can see the straggler is alone"
        );
        assert_eq!(
            choose(&[None, kept, orphan], 2, 1, 2, Kind::Occ, None),
            Choice::Ambiguous,
            "an incomplete one cannot, and guessing is how the value flips"
        );
    }

    /// The same rule with nothing on either side of it. Two members holding no register is
    /// not proof that nothing was chosen while a third is unreachable: the value can be
    /// sitting on the member that did not answer, and reporting a hole would be an answer a
    /// later round contradicts.
    #[test]
    fn nothing_chosen_needs_a_complete_view() {
        assert_eq!(
            choose(&[None, None, None], 0, 1, 2, Kind::Occ, None),
            Choice::Ambiguous
        );
        assert_eq!(
            choose(&[None, None, None], 0, 0, 2, Kind::Occ, None),
            Choice::Missing,
            "every member holding nothing is proof, and a cold page still reads"
        );
    }

    /// The other half of that rule: an answer is proof about the member that gave it. Two
    /// members reporting a version nothing was ever accepted at hold no value, and a value
    /// is chosen only once two acceptors hold it, so whatever the silent third has it has
    /// alone and nothing was chosen. The round can say so without it, and it has to: a page
    /// nobody wrote must be told apart from a page a round cannot see, or the first read and
    /// the first write both fail for as long as one member of the trio is down.
    #[test]
    fn a_quorum_holding_nothing_proves_the_page_is_free() {
        let empty = Some(Register::default());
        // Version zero for Mutable, and epoch zero's fill point for Immutable: both are a
        // version no value was ever accepted at.
        for kind in [Kind::Mutable, Kind::Immutable] {
            assert_eq!(
                choose(&[empty, empty, None], 2, 1, 2, kind, None),
                Choice::Free(Register::default()),
                "{kind:?} refused an answer the round had already proved"
            );
        }
    }

    /// One such answer is not that proof. Two members unaccounted for is a quorum's worth of
    /// members that could be carrying a value between them, and one of them may yet answer,
    /// so the round asks again rather than reporting a hole a later round contradicts.
    #[test]
    fn a_lone_answer_holding_nothing_proves_nothing() {
        let empty = Some(Register::default());
        assert_eq!(
            choose(&[empty, None, None], 1, 1, 2, Kind::Mutable, None),
            Choice::Ambiguous,
            "one member silent and one holding no index is still two that could carry it"
        );
        assert_eq!(
            choose(&[empty, None, None], 1, 2, 2, Kind::Mutable, None),
            Choice::Ambiguous,
            "and two silent members are the same two"
        );
    }

    /// A group of one has no quorum to be wrong about, so the rule must not strand it: with
    /// `need` at one the only answer there is is the one it holds.
    #[test]
    fn a_lone_member_still_answers() {
        let r = reg(4, 1, 0);
        assert_eq!(
            choose(&[r, None, None], 1, 2, 1, Kind::Occ, None),
            Choice::Chosen(r.unwrap())
        );
    }

    /// Two one-shots racing at one version, resolved by ballot once every member has been
    /// heard from. The pair that carried is unknowable from the registers alone, so the
    /// higher ballot wins and both are preserved as the same version.
    #[test]
    fn racing_proposals_at_one_version_resolve_by_ballot() {
        let (lo, hi) = (reg(5, 1, 0), reg(5, 1, 2));
        assert_eq!(
            choose(&[lo, hi, lo], 3, 0, 2, Kind::Occ, None),
            Choice::Chosen(lo.unwrap()),
            "a quorum at one of them is still proof"
        );
        assert_eq!(
            choose(&[lo, hi, None], 2, 1, 2, Kind::Occ, None),
            Choice::Ambiguous,
            "without it the missing member decides, and it is not here"
        );
    }
}

/// A model checker over the protocol itself.
///
/// Every decision below is the production one. [`admits`], [`promised`], [`supersedes`],
/// [`carried`], [`choose`], [`protocol::recovered_term`], [`sealed_at`], [`Register::accepted`],
/// [`Term::raise`], [`Term::adopt`] and [`Gate::decide`] are the same functions the dataplane
/// calls; `Paxos` is the IO that carries them and `Shard` the store they decide about. What
/// the model supplies is only the part that cannot be enumerated from inside a process: which
/// messages arrive, which members can hear each other, and when a disk is lost. So a
/// counterexample here is a counterexample in the shipped code.
///
/// The shape is the smallest that can still hold a disagreement: three members, a quorum of
/// two, one mutable address, two rounds, two terms. Two values are enough to disagree about, and
/// a value that survives one contested round survives any.
#[cfg(test)]
mod model {
    use super::*;
    use stateright::{Checker, Model, Property};
    use std::collections::BTreeSet;

    /// Members in a group and the quorum. Three and two everywhere in RACER.
    const N: usize = 3;
    const NEED: usize = 2;
    /// One register is the whole acceptor state for the modeled address.
    const KIND: Kind = Kind::Occ;
    const MAX_ROUNDS: u8 = 2;
    const MAX_TERM: u32 = 2;
    const GUARDS: [u64; 2] = [0, 1];

    /// The apply-if-newer key of a member that may be holding nothing.
    fn key(r: Option<Register>) -> (u64, u32) {
        r.map_or((0, 0), |r| r.key())
    }

    /// The version the modeled proposal guards on.
    fn current(r: Option<Register>) -> u64 {
        r.map_or(0, |r| r.version)
    }

    /// A one-shot accept on the wire.
    #[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
    struct Flight {
        /// The register it installs if the round carries.
        r: Register,
        /// Which payload it is carrying. Production keeps this in the page's bytes, not in
        /// the register, so the model has to keep it alongside to see two rounds landing
        /// under one name.
        val: u8,
        /// The version it guarded on. It rides on the wire, so an acceptor checks the
        /// proposer's guard rather than deriving one of its own.
        guard: u64,
        by: u8,
        /// Peers that took it.
        at: [bool; N],
        /// Peers that answered at all, a refusal included, or that we gave up waiting on.
        seen: [bool; N],
    }

    /// One consensus group over one address.
    #[derive(Clone, PartialEq, Eq, Hash, Debug)]
    struct Group {
        /// What each member durably holds. `None` is a member with no entry for the address
        /// at all, which every guard and every ballot passes.
        regs: [Option<Register>; N],
        /// The payload behind each member's register, where it holds one. Not part of the
        /// protocol: production carries it in the page and the checksum, and no decision
        /// below reads it. It is here so the model can tell two values apart when the
        /// protocol has given them the same name.
        vals: [u8; N],
        /// Each member's durable promise.
        terms: [Term; N],
        /// Members whose disk was lost and that have not caught up.
        replaying: [bool; N],
        flight: Option<Flight>,
        /// What a client was told had been written, as `(register, payload)`.
        acked: BTreeSet<(Register, u8)>,
        /// What a prepare round reported as the group's value, as `(register, payload)`.
        answered: BTreeSet<(Register, u8)>,
        rounds: u8,
        /// A round that failed to reach a quorum, and a member that lost its disk and came
        /// back: witnesses that the interesting paths are reached rather than pruned.
        lost: bool,
        rejoined: bool,
    }

    #[derive(Clone, PartialEq, Eq, Hash, Debug, PartialOrd, Ord)]
    enum Act {
        /// `by` runs a one-shot at `guard`, staging its own leg without committing it.
        Propose { by: u8, guard: u64 },
        /// The accept in flight reaches `to`, or does not: `up` false is a peer we cannot
        /// hear from, which a proposer must treat as a refusal.
        Deliver { to: u8, up: bool },
        /// The proposer counts what landed and either commits its staged leg or drops it.
        Settle,
        /// `by` prepares against the members in the `reach` mask and repairs to them. This
        /// is both the write path's fallback and the anti-entropy sweep, which repairs a
        /// discrepancy by running exactly this round.
        Repair { by: u8, reach: u8 },
        /// `at` loses its disk: registers and promise both.
        Wipe { at: u8 },
        /// `at` recovers its promise from the rest of the group and rejoins.
        Rejoin { at: u8 },
    }

    #[derive(Clone, Copy, PartialEq, Eq, Debug)]
    struct Consensus;

    impl Consensus {
        /// The members `reach` names, `by` always among them.
        fn reached(by: u8, reach: u8) -> Vec<u8> {
            (0..N as u8)
                .filter(|i| *i == by || reach & (1 << i) != 0)
                .collect()
        }
    }

    impl Model for Consensus {
        type State = Group;
        type Action = Act;

        fn init_states(&self) -> Vec<Group> {
            vec![Group {
                regs: [None; N],
                vals: [0; N],
                terms: [Term::new(0); N],
                replaying: [false; N],
                flight: None,
                acked: BTreeSet::new(),
                answered: BTreeSet::new(),
                rounds: 0,
                lost: false,
                rejoined: false,
            }]
        }

        fn actions(&self, s: &Group, out: &mut Vec<Act>) {
            match s.flight {
                Some(f) => {
                    for to in 0..N as u8 {
                        if to != f.by && !f.seen[to as usize] {
                            out.push(Act::Deliver { to, up: true });
                            out.push(Act::Deliver { to, up: false });
                        }
                    }
                    // Production awaits every leg it dispatched, so a round settles once each
                    // peer has answered or been given up on.
                    if (0..N).all(|i| i == f.by as usize || f.seen[i]) {
                        out.push(Act::Settle);
                    }
                }
                None if s.rounds < MAX_ROUNDS => {
                    for by in 0..N as u8 {
                        for guard in GUARDS {
                            out.push(Act::Propose { by, guard });
                        }
                    }
                }
                None => {}
            }
            for by in 0..N as u8 {
                for reach in (0..(1u8 << N)).filter(|&reach| reach & (1 << by) == 0) {
                    out.push(Act::Repair { by, reach });
                }
            }
            for at in 0..N as u8 {
                out.push(Act::Wipe { at });
                out.push(Act::Rejoin { at });
            }
        }

        fn next_state(&self, s: &Group, act: Act) -> Option<Group> {
            let mut n = s.clone();
            match act {
                Act::Propose { by, guard } => {
                    let i = by as usize;
                    // A replaying member runs no round of its own, and a one-shot needs a
                    // term this node raised itself: a term merely observed is one another
                    // proposer may already be issuing higher ballots at.
                    if s.flight.is_some() || s.replaying[i] || s.terms[i].issuable().is_none() {
                        return None;
                    }
                    let b = Ballot::new(s.terms[i].promise(), by);
                    let r = Register::accepted(guard, b);
                    // The proposer is an acceptor too, and `stage_local` runs the same guard
                    // check before anything goes on the wire.
                    if !admits(current(s.regs[i]), s.regs[i].map(|r| r.ballot), guard, b) {
                        return None;
                    }
                    n.rounds += 1;
                    n.flight = Some(Flight {
                        r,
                        val: s.rounds,
                        guard,
                        by,
                        at: [false; N],
                        seen: [false; N],
                    });
                }
                Act::Deliver { to, up } => {
                    let mut f = s.flight?;
                    let i = to as usize;
                    if to == f.by || f.seen[i] {
                        return None;
                    }
                    f.seen[i] = true;
                    n.flight = Some(f);
                    if !up {
                        return Some(n);
                    }
                    // The gate first: a replaying member takes no part in a round, as
                    // proposer or as acceptor.
                    if Gate::decide(false, s.replaying[i], None)
                        .and_then(Gate::accepts)
                        .is_err()
                    {
                        return Some(n);
                    }
                    if !promised(s.terms[i].promise(), f.r.ballot) {
                        return Some(n);
                    }
                    n.terms[i].adopt(f.r.ballot.term());
                    if admits(
                        current(s.regs[i]),
                        s.regs[i].map(|r| r.ballot),
                        f.guard,
                        f.r.ballot,
                    ) {
                        n.regs[i] = Some(f.r);
                        n.vals[i] = f.val;
                        f.at[i] = true;
                        n.flight = Some(f);
                    }
                }
                Act::Settle => {
                    let f = s.flight?;
                    if (0..N).any(|i| i != f.by as usize && !f.seen[i]) {
                        return None;
                    }
                    n.flight = None;
                    let peers = f.at.iter().filter(|x| **x).count();
                    if carried(peers, NEED) {
                        // Only now does the staged local leg commit. A proposer that
                        // installed regardless would sit a version ahead of a group that
                        // never agreed, and every retry would guard on that version.
                        n.regs[f.by as usize] = Some(f.r);
                        n.vals[f.by as usize] = f.val;
                        n.acked.insert((f.r, f.val));
                    } else {
                        // Production's `refresh`: a lost round gives up the right to issue
                        // one-shots, so the retry prepares instead of reusing the ballot.
                        n.terms[f.by as usize].release();
                        n.lost = true;
                    }
                }
                Act::Repair { by, reach } => {
                    let who = Consensus::reached(by, reach);
                    if s.replaying[by as usize] || who.len() < NEED {
                        return None;
                    }
                    // Bounded by the term ceiling: a prepare raises the promise of every
                    // member it reaches, so the search cannot run rounds forever.
                    if who
                        .iter()
                        .any(|i| s.terms[*i as usize].promise() >= MAX_TERM)
                    {
                        return None;
                    }
                    let mut regs = [None; N];
                    let mut term = 0;
                    for i in who.iter().map(|i| *i as usize) {
                        term = term.max(n.terms[i].raise());
                        regs[i] = s.regs[i];
                    }
                    n.terms[by as usize].adopt(term);
                    // A member that did not answer and a member holding nothing look the
                    // same in `regs`; the counts either side of it tell them apart. A
                    // member out of reach here answers nothing at all, so both are the
                    // ones it could not reach.
                    let best = match choose(&regs, who.len(), N - who.len(), NEED, KIND, None) {
                        Choice::Chosen(r) | Choice::Free(r) => r,
                        Choice::Ambiguous | Choice::Missing => return Some(n),
                    };
                    // Selecting a value is not deciding it. It becomes the group's answer
                    // once a quorum holds it, so count the members that end at or above it.
                    // The bytes travel with the register, so the copy carries the payload
                    // of whichever member the selection came from.
                    let val = match who
                        .iter()
                        .map(|i| *i as usize)
                        .find(|i| s.regs[*i] == Some(best))
                    {
                        Some(i) => s.vals[i],
                        None => return Some(n),
                    };
                    let mut held = 0;
                    for i in who.iter().map(|i| *i as usize) {
                        if supersedes(key(s.regs[i]), best, false) {
                            n.regs[i] = Some(best);
                            n.vals[i] = val;
                        }
                        held += (key(n.regs[i]) >= best.key()) as usize;
                    }
                    if carried(held.saturating_sub(1), NEED) {
                        n.answered.insert((best, val));
                    }
                }
                Act::Wipe { at } => {
                    let i = at as usize;
                    // One at a time, and never under a round. Losing a disk takes with it a
                    // vote the proposer has already counted, and a second loss before the
                    // sweep has put that value back is two thirds of the group gone, which
                    // no quorum protocol survives. RACER assumes the sweep runs between
                    // losses, and this is that assumption written down.
                    if s.flight.is_some() || s.replaying.iter().any(|x| *x) {
                        return None;
                    }
                    n.regs[i] = None;
                    n.vals[i] = 0;
                    n.terms[i] = Term::new(0);
                    n.replaying[i] = true;
                }
                Act::Rejoin { at } => {
                    let i = at as usize;
                    // The sweep repairs first and rejoins only once the comparison comes
                    // back clean, which for one address is this member holding the newest
                    // register in the group.
                    if !s.replaying[i] || (0..N).any(|j| key(s.regs[j]) > key(s.regs[i])) {
                        return None;
                    }
                    let mut peers = [0u32; 2];
                    for (k, j) in (0..N).filter(|j| *j != i).enumerate() {
                        peers[k] = s.terms[j].promise();
                    }
                    n.terms[i].recover(peers);
                    n.replaying[i] = false;
                    n.rejoined = true;
                }
            }
            (n != *s).then_some(n)
        }

        fn properties(&self) -> Vec<Property<Self>> {
            vec![
                // A register names exactly one value. Everything downstream of a write
                // reads the register and trusts it: apply-if-newer skips an equal one, a
                // prepare counts equal ones as agreement. Two payloads under one name and
                // that arithmetic is comparing things that are not the same.
                Property::<Self>::always("one register names one value", |_, s: &Group| {
                    let mut seen: BTreeSet<(Register, u8)> = (0..N)
                        .filter_map(|i| s.regs[i].map(|r| (r, s.vals[i])))
                        .collect();
                    seen.extend(s.acked.iter().chain(s.answered.iter()));
                    seen.iter()
                        .all(|(r, v)| !seen.iter().any(|(q, u)| q == r && u != v))
                }),
                // Consensus itself. Once a version has an acknowledged value, no round may
                // answer with a different one at that version: a client told the first would
                // otherwise read back the second.
                Property::<Self>::always("one value per version", |_, s: &Group| {
                    s.acked.iter().all(|(a, u)| {
                        s.acked
                            .iter()
                            .chain(s.answered.iter())
                            .all(|(r, v)| r.version != a.version || v == u)
                    })
                }),
                // An acknowledged value is never dropped: some member that is not replaying
                // still holds it or something built on top of it. That is what makes the next
                // round's prepare find it.
                Property::<Self>::always("an acknowledged value survives", |_, s: &Group| {
                    s.acked
                        .iter()
                        .all(|(a, _)| (0..N).any(|i| !s.replaying[i] && key(s.regs[i]) >= a.key()))
                }),
                Property::<Self>::sometimes("a value is acknowledged", |_, s: &Group| {
                    !s.acked.is_empty()
                }),
                Property::<Self>::sometimes("a round loses its quorum", |_, s: &Group| s.lost),
                Property::<Self>::sometimes("a repair answers a value", |_, s: &Group| {
                    !s.answered.is_empty()
                }),
                Property::<Self>::sometimes("a wiped member rejoins", |_, s: &Group| {
                    s.rejoined && !s.acked.is_empty()
                }),
            ]
        }
    }

    /// A shard moving from one group to another.
    ///
    /// The source seals, then streams what it holds to the destination, and a control plane
    /// outside RACER repoints the address once it is satisfied. There is no quorum barrier in
    /// the protocol proving every source acceptor sealed, and no durable gate on the
    /// destination proving it drained: [`MoveModel::drained`] is that precondition written
    /// down, and `Cut` is only offered where it holds. So what follows verifies that the seal
    /// and the apply-if-newer stream are enough *given* that precondition, not that RACER
    /// enforces it.
    #[derive(Clone, PartialEq, Eq, Hash, Debug)]
    struct Move {
        /// The source group's registers and its seal table, keyed by member.
        src: [Option<Register>; N],
        sealed: [u32; N],
        /// The destination group's registers.
        dst: [Option<Register>; N],
        /// Registers the source acknowledged, and every register it ever held.
        acked: BTreeSet<Register>,
        origin: BTreeSet<Register>,
        /// The control plane has repointed the address at the destination.
        cut: bool,
        /// A write that carried while a quorum of the source was already sealed.
        late: bool,
        writes: u8,
    }

    #[derive(Clone, PartialEq, Eq, Hash, Debug, PartialOrd, Ord)]
    enum MoveAct {
        /// A round on the source at `guard`, run by `by` against the members in `reach`.
        Write { by: u8, guard: u64, reach: u8 },
        /// The seal reaches source member `at`. Production sends it to every member of the
        /// destination zone's catalog and seals locally last, so the model lets them land in
        /// any order and lets the handover stall part way.
        Seal { at: u8 },
        /// Source member `from` streams its own snapshot to destination member `to`.
        Push { from: u8, to: u8 },
        /// The control plane repoints the address at the destination.
        Cut,
    }

    #[derive(Clone, Copy, PartialEq, Eq, Debug)]
    struct MoveModel;

    impl MoveModel {
        /// The precondition the control plane is trusted to wait for: every source member
        /// sealed, so the source can acknowledge nothing further, and every destination member
        /// holding what the source ended at.
        fn drained(s: &Move) -> bool {
            let top = s.src.iter().copied().map(key).max().unwrap_or_default();
            s.sealed.iter().all(|t| *t > 0) && s.dst.iter().copied().all(|r| key(r) >= top)
        }
    }

    impl Model for MoveModel {
        type State = Move;
        type Action = MoveAct;

        fn init_states(&self) -> Vec<Move> {
            vec![Move {
                src: [None; N],
                sealed: [0; N],
                dst: [None; N],
                acked: BTreeSet::new(),
                origin: BTreeSet::new(),
                cut: false,
                late: false,
                writes: 0,
            }]
        }

        fn actions(&self, s: &Move, out: &mut Vec<MoveAct>) {
            if s.writes < MAX_ROUNDS {
                for by in 0..N as u8 {
                    for guard in GUARDS {
                        for reach in (0..(1u8 << N)).filter(|&reach| reach & (1 << by) == 0) {
                            out.push(MoveAct::Write { by, guard, reach });
                        }
                    }
                }
            }
            for at in 0..N as u8 {
                out.push(MoveAct::Seal { at });
            }
            for from in 0..N as u8 {
                for to in 0..N as u8 {
                    out.push(MoveAct::Push { from, to });
                }
            }
            if !s.cut {
                out.push(MoveAct::Cut);
            }
        }

        fn next_state(&self, s: &Move, act: MoveAct) -> Option<Move> {
            let mut n = s.clone();
            match act {
                MoveAct::Write { by, guard, reach } => {
                    let who = Consensus::reached(by, reach);
                    // The proposer is the one whose gate decides the write: sealed here means
                    // the address belongs to the destination now, so the round never starts.
                    let next = (s.sealed[by as usize] > 0).then_some(1);
                    if !matches!(
                        Gate::decide(s.sealed[by as usize] > 0, false, next),
                        Ok(Gate::Serve { replaying: false })
                    ) {
                        return None;
                    }
                    let b = Ballot::new(1, by);
                    let r = Register::accepted(guard, b);
                    let mut took = Vec::new();
                    for i in who.iter().map(|i| *i as usize) {
                        // A sealed acceptor is `Away`, so it takes nothing further.
                        if !matches!(
                            Gate::decide(s.sealed[i] > 0, false, Some(1)),
                            Ok(Gate::Serve { replaying: false })
                        ) {
                            continue;
                        }
                        if admits(current(s.src[i]), s.src[i].map(|x| x.ballot), guard, b) {
                            took.push(i);
                        }
                    }
                    if !took.contains(&(by as usize))
                        || !carried(took.len().saturating_sub(1), NEED)
                    {
                        return None;
                    }
                    for i in took {
                        n.src[i] = Some(r);
                    }
                    n.acked.insert(r);
                    n.origin.insert(r);
                    n.writes += 1;
                    n.late |= s.sealed.iter().filter(|t| **t > 0).count() >= NEED;
                }
                MoveAct::Seal { at } => {
                    let i = at as usize;
                    let held = (s.sealed[i] > 0).then_some(s.sealed[i]);
                    n.sealed[i] = sealed_at(held, 1);
                }
                MoveAct::Push { from, to } => {
                    let (f, t) = (from as usize, to as usize);
                    // Production seals before it streams, so nothing the destination takes can
                    // be superseded on the source afterwards.
                    let r = s.src[f].filter(|_| s.sealed[f] > 0)?;
                    if !supersedes(key(s.dst[t]), r, false) {
                        return None;
                    }
                    n.dst[t] = Some(r);
                }
                MoveAct::Cut => {
                    if s.cut || !MoveModel::drained(s) {
                        return None;
                    }
                    n.cut = true;
                }
            }
            (n != *s).then_some(n)
        }

        fn properties(&self) -> Vec<Property<Self>> {
            vec![
                // The seal is the write path's stop: once a quorum of the source holds it, no
                // further value can carry, whatever order the seals landed in.
                Property::<Self>::always("a sealed quorum ends the writes", |_, s: &Move| !s.late),
                // Apply-if-newer, so the two streams commute and a repeated push is free. The
                // destination invents nothing.
                Property::<Self>::always(
                    "the destination holds only source values",
                    |_, s: &Move| s.dst.iter().flatten().all(|r| s.origin.contains(r)),
                ),
                // What the control plane's precondition buys: after the cutover every
                // destination member is at or above every value the source acknowledged, so no
                // reader at the destination can miss one.
                Property::<Self>::always(
                    "a cutover carries the acknowledged values",
                    |_, s: &Move| {
                        !s.cut
                            || s.acked
                                .iter()
                                .all(|a| (0..N).all(|i| key(s.dst[i]) >= a.key()))
                    },
                ),
                Property::<Self>::sometimes("the handover completes", |_, s: &Move| {
                    s.cut && !s.acked.is_empty()
                }),
            ]
        }
    }

    fn threads() -> usize {
        std::thread::available_parallelism()
            .map(|n| n.get())
            .unwrap_or(1)
    }

    /// Guarded accepts, prepare, repair, disk loss, and promise recovery, all driving the
    /// production rules.
    #[test]
    fn agreement_survives_disk_loss_and_promise_recovery() {
        Consensus
            .checker()
            .threads(threads())
            .spawn_dfs()
            .join()
            .assert_properties();
    }

    /// The seal and the snapshot stream, given the control plane's cutover precondition.
    #[test]
    fn a_handover_carries_every_acknowledged_value() {
        MoveModel
            .checker()
            .threads(threads())
            .spawn_dfs()
            .join()
            .assert_properties();
    }
}
