//! Model checks for four state machines: the guarded one-shot accept with its prepare
//! and repair fallback, when the proposer's own copy of that accept may land, the shard
//! handover between disjoint memberships, and the anti-entropy sweep (`heal.rs`).
//!
//! Each mechanism is a parameter, so the checks come in pairs: the rule the
//! implementation uses must hold, the simpler rule it replaced must fail. Pure state
//! machines with no IO, so they need neither root nor a device.

use stateright::{Checker, HasDiscoveries, Model, Property};

// Full-space proofs use `spawn_dfs`; DFS holds one path at a time, BFS the whole
// frontier. Checks that assert a counterexample use `spawn_bfs`, the only shortest-path
// search, and assert on the path length.

/// Stop the search the moment `name` has a counterexample. The default runs until every
/// property has a discovery, enumerating the whole space when a `sometimes` never fires.
fn until(name: &'static str) -> HasDiscoveries {
    HasDiscoveries::AnyOf([name].into_iter().collect())
}

// --- The register ---

const N: usize = 3;
const NEED: usize = 2;

const MAX_PROPOSALS: u8 = 3;
const MAX_VERSION: u8 = 2;
const MAX_TERM: u8 = 2;
const MAX_REPAIRS: u8 = 1;
const MAX_WIPES: u8 = 1;

/// One acceptor's register. `value` is the page bytes; `(version, ballot)` orders it.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug, Default, PartialOrd, Ord)]
struct Reg {
    version: u8,
    ballot: u8,
    value: u8,
}

impl Reg {
    fn newer_than(&self, o: &Reg) -> bool {
        (self.version, self.ballot) > (o.version, o.ballot)
    }
}

/// `term << 2 | member`, the packing `paxos::Ballot` uses, so numeric comparison orders.
fn ballot(term: u8, member: u8) -> u8 {
    term << 2 | member
}

fn term_of(b: u8) -> u8 {
    b >> 2
}

/// What a prepare round concludes from its replies, walking versions from the top down.
/// A value is still a candidate at a version if the silent acceptors plus those past it
/// could carry it to a quorum; only an acceptor below is a denial. One candidate wins,
/// none continues the descent, two with a silent acceptor left retries. The descent stops
/// at the floor, the highest version a quorum could still hold; answering below it would
/// roll back what was chosen there.
fn choose(replies: &[Reg], floored: bool) -> Option<Reg> {
    let unseen = N - replies.len();
    let mut vers: Vec<u8> = replies.iter().map(|r| r.version).collect();
    vers.sort_unstable_by(|a, b| b.cmp(a));
    vers.dedup();
    let floor = vers
        .iter()
        .copied()
        .find(|v| replies.iter().filter(|r| r.version >= *v).count() + unseen >= NEED)
        .filter(|_| floored)
        .unwrap_or_default();
    for v in vers.iter().copied().take_while(|v| *v >= floor) {
        let at: Vec<Reg> = replies.iter().copied().filter(|r| r.version == v).collect();
        let above = replies.iter().filter(|r| r.version > v).count();
        let mut cands: Vec<Reg> = Vec::new();
        let mut sure = None;
        for r in at.iter().copied() {
            let exact = at.iter().filter(|x| **x == r).count();
            if exact >= NEED {
                sure = Some(r);
            }
            if !cands.contains(&r) && exact + unseen + above >= NEED {
                cands.push(r);
            }
        }
        if sure.is_some() {
            return sure;
        }
        match cands.len() {
            1 => return Some(cands[0]),
            0 => continue,
            // Whoever moved past this version built on one of them, so it supersedes both.
            _ if unseen == 0 => return replies.iter().copied().max_by_key(|r| key(*r)),
            _ => return None,
        }
    }
    // Nothing at or above the floor reached a quorum, so take the newest on offer there.
    replies
        .iter()
        .copied()
        .filter(|r| r.version >= floor)
        .max_by_key(|r| key(*r))
}

/// The apply-if-newer order: version first, ballot to break a tie within one.
fn key(r: Reg) -> (u8, u8) {
    (r.version, r.ballot)
}

/// A one-shot accept in flight; `left[i]` is unreached, so every delivery order is tried.
#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct Proposal {
    guard: u8,
    ballot: u8,
    value: u8,
    left: [bool; N],
}

/// The write phase of a repair in flight. One at a time, `MAX_REPAIRS` in all.
#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct Repair {
    pick: Reg,
    left: [bool; N],
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct RegisterState {
    reg: [Reg; N],
    /// Each acceptor's standing promise.
    term: [u8; N],
    proposals: Vec<Proposal>,
    repair: Option<Repair>,
    /// Every register a quorum of voting acceptors has held, and so was chosen.
    chosen: Vec<Reg>,
    made: u8,
    repairs: u8,
    /// Acceptors that lost what they had accepted and are refilling from the group.
    replaying: [bool; N],
    wipes: u8,
    /// A round answered with a register older than one already chosen.
    rolled: bool,
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
enum RegisterAction {
    Propose { member: u8, guard: u8 },
    /// The accept reaches one acceptor. Losing a leg is modelled by never taking this.
    Deliver { p: usize, a: u8 },
    Prepare { respond: [bool; N] },
    /// The write phase of a repair reaches one acceptor.
    Learn { a: u8 },
    /// An acceptor's device is reformatted: it keeps its identity, loses its state.
    Wipe { a: u8 },
    Rejoin { a: u8 },
}

/// Which rule `prepare_round` uses to decide what was chosen.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Rule {
    /// What the implementation does: walk versions down to the floor, keeping the value
    /// that could still have been chosen there.
    Majority,
    /// The simpler rule: highest version, ties on ballot.
    HighestBallot,
}

/// What a round does with an acceptor that is still replaying.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Replay {
    /// What the implementation does: a replaying node neither proposes nor accepts.
    Excluded,
    /// Treating it as an ordinary acceptor.
    Counted,
}

/// What a replaying acceptor carries back into the group with it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Recover {
    /// What the implementation does (`Paxos::rejoin`): adopt the peers' highest promise.
    FromPeers,
    /// Refilling the registers and nothing else.
    Nothing,
}

struct Register {
    rule: Rule,
    replay: Replay,
    /// Whether an acceptor may be wiped. Off for the `rule` checks, keeping their space.
    wipe: bool,
    /// How many one-shots may be issued. Wipe checks use one fewer; the second is undone.
    proposals: u8,
    /// What a wiped acceptor brings back with it, or `None` for one that never returns.
    recover: Option<Recover>,
}

impl Register {
    /// Fold the replica set into the chosen set. Only acceptors that take part count.
    fn observe(&self, s: &mut RegisterState) {
        for i in 0..N {
            if s.reg[i].version == 0 || !self.votes(s, i) {
                continue;
            }
            let held = (0..N)
                .filter(|j| self.votes(s, *j) && s.reg[*j] == s.reg[i])
                .count();
            if held >= NEED && !s.chosen.contains(&s.reg[i]) {
                s.chosen.push(s.reg[i]);
                s.chosen.sort();
            }
        }
    }

    /// The decision `prepare_round` reaches from prepare replies. `None` retries.
    fn pick(&self, replies: &[Reg]) -> Option<Reg> {
        if self.rule == Rule::HighestBallot {
            let top = replies.iter().map(|r| r.version).max()?;
            return replies
                .iter()
                .copied()
                .filter(|r| r.version == top)
                .max_by_key(|r| r.ballot);
        }
        choose(replies, true)
    }

    fn votes(&self, s: &RegisterState, a: usize) -> bool {
        self.replay == Replay::Counted || !s.replaying[a]
    }
}

impl Model for Register {
    type State = RegisterState;
    type Action = RegisterAction;

    fn init_states(&self) -> Vec<Self::State> {
        vec![RegisterState {
            reg: [Reg::default(); N],
            term: [0; N],
            proposals: Vec::new(),
            repair: None,
            chosen: Vec::new(),
            made: 0,
            repairs: 0,
            replaying: [false; N],
            wipes: 0,
            rolled: false,
        }]
    }

    fn actions(&self, s: &Self::State, out: &mut Vec<Self::Action>) {
        if s.made < self.proposals {
            for member in 0..N as u8 {
                for guard in 0..MAX_VERSION {
                    out.push(RegisterAction::Propose { member, guard });
                }
            }
        }
        for (p, prop) in s.proposals.iter().enumerate() {
            for a in 0..N as u8 {
                if prop.left[a as usize] {
                    out.push(RegisterAction::Deliver { p, a });
                }
            }
        }
        if s.repair.is_none() && s.repairs < MAX_REPAIRS {
            for mask in 1..8u8 {
                let respond = [mask & 1 != 0, mask & 2 != 0, mask & 4 != 0];
                // A replaying acceptor cannot promise, so the quorum comes from the rest.
                if respond
                    .iter()
                    .enumerate()
                    .any(|(i, r)| *r && !self.votes(s, i))
                {
                    continue;
                }
                if respond.iter().filter(|x| **x).count() >= NEED {
                    out.push(RegisterAction::Prepare { respond });
                }
            }
        }
        if let Some(r) = &s.repair {
            for a in 0..N as u8 {
                if r.left[a as usize] {
                    out.push(RegisterAction::Learn { a });
                }
            }
        }
        if self.wipe && s.wipes < MAX_WIPES {
            for a in 0..N as u8 {
                if !s.replaying[a as usize] {
                    out.push(RegisterAction::Wipe { a });
                }
            }
        }
        if self.recover.is_some() {
            for a in 0..N as u8 {
                let i = a as usize;
                // Refilled: no member holds a register we lack. Vacuously true of an empty
                // group, where only the promise speaks; that case is worth checking.
                if s.replaying[i] && (0..N).all(|j| !s.reg[j].newer_than(&s.reg[i])) {
                    out.push(RegisterAction::Rejoin { a });
                }
            }
        }
    }

    fn next_state(&self, last: &Self::State, action: Self::Action) -> Option<Self::State> {
        let mut s = last.clone();
        match action {
            RegisterAction::Propose { member, guard } => {
                s.made += 1;
                s.proposals.push(Proposal {
                    guard,
                    ballot: ballot(s.term[member as usize], member),
                    value: s.made,
                    left: [true; N],
                });
            }
            RegisterAction::Deliver { p, a } => {
                let prop = s.proposals.get(p)?.clone();
                s.proposals[p].left[a as usize] = false;
                let i = a as usize;
                // The guard, in order: refuse a version we are past, fill a gap below the
                // guard, refuse a regressing ballot at the guard.
                let behind = s.reg[i].version < prop.guard;
                let at = s.reg[i].version == prop.guard && prop.ballot >= s.reg[i].ballot;
                if self.votes(&s, i) && term_of(prop.ballot) >= s.term[i] {
                    s.term[i] = term_of(prop.ballot);
                    if behind || at {
                        s.reg[i] = Reg {
                            version: prop.guard + 1,
                            ballot: prop.ballot,
                            value: prop.value,
                        };
                    }
                }
                s.proposals.retain(|p| p.left.iter().any(|x| *x));
                self.observe(&mut s);
            }
            RegisterAction::Prepare { respond } => {
                // Every responder raises its promise. A round whose responders land on
                // different terms is dropped; the implementation proposes at the highest.
                let mut terms = Vec::new();
                for (i, r) in respond.iter().enumerate() {
                    if *r {
                        if s.term[i] + 1 > MAX_TERM {
                            return None;
                        }
                        s.term[i] += 1;
                        terms.push(s.term[i]);
                    }
                }
                s.repairs += 1;
                if terms.iter().any(|t| *t != terms[0]) {
                    return Some(s);
                }
                let replies: Vec<Reg> = (0..N).filter(|i| respond[*i]).map(|i| s.reg[i]).collect();
                if let Some(pick) = self.pick(&replies)
                    && pick.version > 0
                {
                    // A chosen value left a quorum standing at or above its version, so
                    // an older answer is a rollback. Only a wipe makes that legal.
                    s.rolled |= s.chosen.iter().any(|c| c.newer_than(&pick));
                    s.repair = Some(Repair {
                        pick,
                        left: [true; N],
                    });
                }
            }
            RegisterAction::Learn { a } => {
                let r = s.repair.clone()?;
                let i = a as usize;
                // The repair write is apply-if-newer, so it never regresses a version.
                if r.pick.newer_than(&s.reg[i]) {
                    s.reg[i] = r.pick;
                }
                let rep = s.repair.as_mut()?;
                rep.left[i] = false;
                if rep.left.iter().all(|x| !x) {
                    s.repair = None;
                }
                self.observe(&mut s);
            }
            RegisterAction::Wipe { a } => {
                // The reformat takes the term too: a member that answers knowing nothing.
                let i = a as usize;
                s.wipes += 1;
                s.replaying[i] = true;
                s.reg[i] = Reg::default();
                s.term[i] = 0;
            }
            RegisterAction::Rejoin { a } => {
                let i = a as usize;
                if self.recover == Some(Recover::FromPeers) {
                    let peers = (0..N).filter(|j| *j != i).map(|j| s.term[j]);
                    s.term[i] = peers.max().unwrap_or(0);
                }
                s.replaying[i] = false;
                self.observe(&mut s);
            }
        }
        Some(s)
    }

    fn properties(&self) -> Vec<Property<Self>> {
        vec![
            // Safety: a version is a decision point, and only one value is decided at it.
            Property::<Self>::always("one value per version", |_, s| {
                s.chosen.iter().all(|a| {
                    s.chosen
                        .iter()
                        .all(|b| a.version != b.version || a.value == b.value)
                })
            }),
            Property::<Self>::always("a repair never answers below a chosen version", |m, s| {
                // A wipe destroys the copy this rests on; only a wipe-free group owes it.
                m.wipe || !s.rolled
            }),
            Property::<Self>::sometimes("a write is chosen", |_, s| !s.chosen.is_empty()),
            Property::<Self>::sometimes("repair converges a split", |_, s| {
                s.repairs > 0
                    && s.repair.is_none()
                    && s.reg[0].version > 0
                    && s.reg[0] == s.reg[1]
                    && s.reg[1] == s.reg[2]
            }),
        ]
    }
}

#[test]
fn guarded_accepts_agree_on_one_value_per_version() {
    Register {
        rule: Rule::Majority,
        replay: Replay::Excluded,
        wipe: false,
        proposals: MAX_PROPOSALS,
        recover: None,
    }
    .checker()
    .threads(num_threads())
    .spawn_dfs()
    .join()
    .assert_properties();
}

#[test]
fn choosing_by_ballot_alone_resurrects_a_losing_proposal() {
    // The simpler rule picks the highest ballot at the top version, so a losing one-shot
    // at the chosen value's version with a higher ballot is promoted to a quorum.
    let path = Register {
        rule: Rule::HighestBallot,
        replay: Replay::Excluded,
        wipe: false,
        proposals: MAX_PROPOSALS,
        recover: None,
    }
    .checker()
    .threads(num_threads())
    .finish_when(until("one value per version"))
    .spawn_bfs()
    .join()
    .assert_any_discovery("one value per version");
    assert!(path.into_actions().len() >= 4);
}

#[test]
fn descending_past_the_floor_answers_with_a_rolled_back_write() {
    // Counterexample: a value chosen at version 2 by the two acceptors that have since
    // moved to version 3, each on a proposal nobody else saw, and the third never had it.
    // No reply names it, so an unfloored descent hands back a register from before it.
    let replies = [
        Reg {
            version: 1,
            ballot: ballot(0, 1),
            value: 1,
        },
        Reg {
            version: 3,
            ballot: ballot(1, 2),
            value: 2,
        },
        Reg {
            version: 3,
            ballot: ballot(1, 1),
            value: 3,
        },
    ];
    assert_eq!(choose(&replies, false).map(|r| r.version), Some(1));
    // The answer must come from the floor whether or not anyone can still name it.
    assert_eq!(choose(&replies, true), Some(replies[1]));
}

/// A wiped acceptor is not merely behind: it answers, and it answers with nothing. The
/// four checks below start from a wipe and let the group run around it. How a wipe comes
/// to be noticed stays outside the model and is not the property under test.
#[test]
fn a_replaying_acceptor_is_not_counted_toward_quorum() {
    Register {
        rule: Rule::Majority,
        replay: Replay::Excluded,
        wipe: true,
        proposals: 2,
        recover: None,
    }
    .checker()
    .threads(num_threads())
    .spawn_dfs()
    .join()
    .assert_properties();
}

#[test]
fn counting_a_replaying_acceptor_undoes_an_acknowledged_write() {
    // A wiped acceptor breaks quorum intersection while still answering: it guards at the
    // version it lost, so a second write takes that version and reaches its own quorum.
    let path = Register {
        rule: Rule::Majority,
        replay: Replay::Counted,
        wipe: true,
        proposals: 2,
        recover: None,
    }
    .checker()
    .threads(num_threads())
    .finish_when(until("one value per version"))
    .spawn_bfs()
    .join()
    .assert_any_discovery("one value per version");
    assert!(
        path.into_actions()
            .iter()
            .any(|a| matches!(a, RegisterAction::Wipe { .. }))
    );
}

#[test]
fn a_rejoining_acceptor_recovers_its_promise() {
    Register {
        rule: Rule::Majority,
        replay: Replay::Excluded,
        wipe: true,
        proposals: 2,
        recover: Some(Recover::FromPeers),
    }
    .checker()
    .threads(num_threads())
    .spawn_dfs()
    .join()
    .assert_properties();
}

#[test]
fn rejoining_without_the_promise_undoes_an_acknowledged_write() {
    // Replayed registers are a floor only where the group holds one; where it holds
    // nothing, a member back without its promise accepts a ballot it had refused.
    let path = Register {
        rule: Rule::Majority,
        replay: Replay::Excluded,
        wipe: true,
        proposals: 2,
        recover: Some(Recover::Nothing),
    }
    .checker()
    .threads(num_threads())
    .finish_when(until("one value per version"))
    .spawn_bfs()
    .join()
    .assert_any_discovery("one value per version");
    assert!(
        path.into_actions()
            .iter()
            .any(|a| matches!(a, RegisterAction::Rejoin { .. }))
    );
}

// --- When the proposer's own copy lands ---

/// One-shots this model may issue. Unlike the check above, the guard here is the
/// proposer's own register, as `alloc.guard` returns it; the question is when it may move.
const MAX_ORDER: u8 = 3;

/// When the proposer's own leg is installed.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Commit {
    /// What the implementation does: install the register only once peers make a quorum.
    Deferred,
    /// The simpler rule: the local accept runs beside the fan-out and stands regardless.
    Eager,
}

/// A one-shot in flight. Peers resolve one at a time; `Finish` settles the proposer's.
#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct Shot {
    by: u8,
    guard: u8,
    ballot: u8,
    value: u8,
    left: [bool; N],
    votes: u8,
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct CommitState {
    reg: [Reg; N],
    shots: Vec<Shot>,
    /// Values in the order they first reached a quorum: the client-visible history.
    chosen: Vec<u8>,
    made: u8,
    repairs: u8,
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
enum CommitAction {
    /// A member proposes at the version it holds. Ballots rise with the proposal order.
    Propose {
        by: u8,
    },
    Deliver {
        p: usize,
        a: u8,
    },
    Lose {
        p: usize,
        a: u8,
    },
    /// Every peer leg has resolved, so the proposer settles its own.
    Finish {
        p: usize,
    },
    /// A repair: prepare at `respond`, then learn the answer to `to`.
    Repair {
        respond: [bool; N],
        to: [bool; N],
    },
}

struct CommitModel {
    commit: Commit,
}

/// Fold the replica set into the chosen history: a register a quorum holds is decided.
fn settle(s: &mut CommitState) {
    for i in 0..N {
        let r = s.reg[i];
        if r.version == 0 || s.chosen.last() == Some(&r.value) {
            continue;
        }
        if (0..N).filter(|j| s.reg[*j] == r).count() >= NEED && !s.chosen.contains(&r.value) {
            s.chosen.push(r.value);
        }
    }
}

/// The acceptor rule for every leg: refuse a version we are past or a regressing ballot.
fn take(reg: &mut Reg, sh: &Shot) -> bool {
    if reg.version > sh.guard || (reg.version == sh.guard && sh.ballot < reg.ballot) {
        return false;
    }
    *reg = Reg {
        version: sh.guard + 1,
        ballot: sh.ballot,
        value: sh.value,
    };
    true
}

impl Model for CommitModel {
    type State = CommitState;
    type Action = CommitAction;

    fn init_states(&self) -> Vec<Self::State> {
        vec![CommitState {
            reg: [Reg::default(); N],
            shots: Vec::new(),
            chosen: Vec::new(),
            made: 0,
            repairs: 0,
        }]
    }

    fn actions(&self, s: &Self::State, out: &mut Vec<Self::Action>) {
        if s.made < MAX_ORDER && s.shots.is_empty() {
            for by in 0..N as u8 {
                out.push(CommitAction::Propose { by });
            }
        }
        for (p, sh) in s.shots.iter().enumerate() {
            for a in 0..N as u8 {
                if sh.left[a as usize] {
                    out.push(CommitAction::Deliver { p, a });
                    out.push(CommitAction::Lose { p, a });
                }
            }
            if sh.left.iter().all(|x| !x) {
                out.push(CommitAction::Finish { p });
            }
        }
        if s.repairs < MAX_REPAIRS {
            for r in 1..8u8 {
                let respond = [r & 1 != 0, r & 2 != 0, r & 4 != 0];
                if respond.iter().filter(|x| **x).count() < NEED {
                    continue;
                }
                for t in 1..8u8 {
                    out.push(CommitAction::Repair {
                        respond,
                        to: [t & 1 != 0, t & 2 != 0, t & 4 != 0],
                    });
                }
            }
        }
    }

    fn next_state(&self, last: &Self::State, action: Self::Action) -> Option<Self::State> {
        let mut s = last.clone();
        match action {
            CommitAction::Propose { by } => {
                s.made += 1;
                let i = by as usize;
                let mut left = [true; N];
                left[i] = false;
                let sh = Shot {
                    by,
                    guard: s.reg[i].version,
                    ballot: s.made,
                    value: s.made,
                    left,
                    votes: 0,
                };
                if self.commit == Commit::Eager {
                    take(&mut s.reg[i], &sh);
                }
                s.shots.push(sh);
            }
            CommitAction::Deliver { p, a } => {
                let sh = s.shots.get(p)?.clone();
                s.shots[p].left[a as usize] = false;
                if take(&mut s.reg[a as usize], &sh) {
                    s.shots[p].votes += 1;
                }
            }
            CommitAction::Lose { p, a } => {
                s.shots.get(p)?;
                s.shots[p].left[a as usize] = false;
            }
            CommitAction::Finish { p } => {
                let sh = s.shots.get(p)?.clone();
                // Without the peer accepts a quorum needs the staged page is dropped, so
                // no version exists that a quorum never stood behind.
                if self.commit == Commit::Deferred && sh.votes as usize >= NEED - 1 {
                    take(&mut s.reg[sh.by as usize], &sh);
                }
                s.shots.remove(p);
            }
            CommitAction::Repair { respond, to } => {
                s.repairs += 1;
                let replies: Vec<Reg> = (0..N).filter(|i| respond[*i]).map(|i| s.reg[i]).collect();
                if let Some(pick) = choose(&replies, true)
                    && pick.version > 0
                {
                    for (i, want) in to.iter().enumerate() {
                        if *want && pick.newer_than(&s.reg[i]) {
                            s.reg[i] = pick;
                        }
                    }
                }
            }
        }
        settle(&mut s);
        Some(s)
    }

    fn properties(&self) -> Vec<Property<Self>> {
        vec![
            // A later chosen value with an earlier proposal order is a write coming back.
            Property::<Self>::always("a write is not undone", |_, s: &CommitState| {
                s.chosen.windows(2).all(|w| w[0] < w[1])
            }),
            Property::<Self>::sometimes("a write is chosen", |_, s: &CommitState| {
                s.chosen.len() >= 2
            }),
            // The case the rule is about: a round no quorum agreed to.
            Property::<Self>::sometimes("a round is abandoned", |_, s: &CommitState| {
                s.made > 0 && s.shots.is_empty() && s.chosen.is_empty()
            }),
        ]
    }
}

#[test]
fn a_proposer_never_builds_on_a_version_of_its_own() {
    CommitModel {
        commit: Commit::Deferred,
    }
    .checker()
    .threads(num_threads())
    .spawn_dfs()
    .join()
    .assert_properties();
}

#[test]
fn committing_beside_the_fan_out_resurrects_a_failed_write() {
    // The local accept stands even when nothing agreed to it, so each retry leaves this
    // member a version ahead of a group that never saw those rounds. A later prepare
    // hearing from two of three finds that private version uncontested and spreads it.
    let path = CommitModel {
        commit: Commit::Eager,
    }
    .checker()
    .threads(num_threads())
    .finish_when(until("a write is not undone"))
    .spawn_bfs()
    .join()
    .assert_any_discovery("a write is not undone");
    assert!(
        path.into_actions()
            .iter()
            .any(|a| matches!(a, CommitAction::Repair { .. }))
    );
}

// --- Shard handover between disjoint memberships ---

const MAX_WRITES: u8 = 3;

/// One page, replicated in source group `a` and destination group `b`. The groups share
/// no member, so only the handover ordering keeps them from both accepting.
#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct HandoverState {
    a: [Reg; N],
    b: [Reg; N],
    /// Source members that have taken the seal; a quorum of them freezes the source.
    sealed: [bool; N],
    /// The destination has recorded the coming handover; nothing may be sealed first.
    pending: bool,
    /// Versions the source committed and may still forward. Fire-and-forget, may be lost.
    forwards: Vec<Reg>,
    live: bool,
    /// Every write acknowledged to a client, in the order it was acknowledged.
    history: Vec<Reg>,
    writes: u8,
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
enum HandoverAction {
    WriteA {
        set: [bool; N],
    },
    WriteB {
        set: [bool; N],
    },
    /// A live forward reaches one destination acceptor, apply-if-newer.
    Forward {
        f: usize,
        a: u8,
    },
    SealPending,
    Seal {
        a: u8,
    },
    /// The frozen source pushes what it holds; the destination applies if newer.
    Drain,
    GoLive,
}

/// Whether the destination drains before it installs.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Install {
    /// What the implementation requires: equality with the frozen source first.
    AfterDrain,
    /// The tempting shortcut: go live as soon as the seal is chosen.
    OnSeal,
}

struct Handover {
    install: Install,
}

fn quorum_reg(g: &[Reg; N]) -> Option<Reg> {
    (0..N)
        .filter(|i| g.iter().filter(|r| **r == g[*i]).count() >= NEED)
        .map(|i| g[i])
        .max()
}

impl Handover {
    fn seal_chosen(&self, s: &HandoverState) -> bool {
        s.sealed.iter().filter(|x| **x).count() >= NEED
    }

    fn drained(&self, s: &HandoverState) -> bool {
        match quorum_reg(&s.a) {
            None => true,
            Some(top) => s.b.iter().filter(|r| **r == top).count() >= NEED,
        }
    }
}

impl Model for Handover {
    type State = HandoverState;
    type Action = HandoverAction;

    fn init_states(&self) -> Vec<Self::State> {
        vec![HandoverState {
            a: [Reg::default(); N],
            b: [Reg::default(); N],
            sealed: [false; N],
            pending: false,
            forwards: Vec::new(),
            live: false,
            history: Vec::new(),
            writes: 0,
        }]
    }

    fn actions(&self, s: &Self::State, out: &mut Vec<Self::Action>) {
        let masks = (1..8u8)
            .map(|m| [m & 1 != 0, m & 2 != 0, m & 4 != 0])
            .filter(|set| set.iter().filter(|x| **x).count() >= NEED);
        if s.writes < MAX_WRITES {
            for set in masks.clone() {
                if !self.seal_chosen(s) {
                    out.push(HandoverAction::WriteA { set });
                }
                if s.live {
                    out.push(HandoverAction::WriteB { set });
                }
            }
        }
        for f in 0..s.forwards.len() {
            for a in 0..N as u8 {
                out.push(HandoverAction::Forward { f, a });
            }
        }
        if !s.pending {
            out.push(HandoverAction::SealPending);
        }
        if s.pending {
            for a in 0..N as u8 {
                if !s.sealed[a as usize] {
                    out.push(HandoverAction::Seal { a });
                }
            }
        }
        if self.seal_chosen(s) {
            if !s.live {
                out.push(HandoverAction::GoLive);
            }
            out.push(HandoverAction::Drain);
        }
    }

    fn next_state(&self, last: &Self::State, action: Self::Action) -> Option<Self::State> {
        let mut s = last.clone();
        match action {
            HandoverAction::WriteA { set } => {
                let prev = quorum_reg(&s.a).unwrap_or_default();
                if prev.version >= MAX_VERSION {
                    return None;
                }
                s.writes += 1;
                let r = Reg {
                    version: prev.version + 1,
                    ballot: 0,
                    value: s.writes,
                };
                for (i, on) in set.iter().enumerate() {
                    if *on {
                        s.a[i] = r;
                    }
                }
                s.history.push(r);
                s.forwards.push(r);
            }
            HandoverAction::WriteB { set } => {
                let prev = quorum_reg(&s.b).unwrap_or_default();
                if prev.version >= MAX_VERSION {
                    return None;
                }
                s.writes += 1;
                let r = Reg {
                    version: prev.version + 1,
                    ballot: 0,
                    value: 10 + s.writes,
                };
                for (i, on) in set.iter().enumerate() {
                    if *on {
                        s.b[i] = r;
                    }
                }
                s.history.push(r);
            }
            HandoverAction::Forward { f, a } => {
                let r = *s.forwards.get(f)?;
                let i = a as usize;
                if r.newer_than(&s.b[i]) {
                    s.b[i] = r;
                }
            }
            HandoverAction::SealPending => s.pending = true,
            HandoverAction::Seal { a } => s.sealed[a as usize] = true,
            HandoverAction::Drain => {
                // The source is frozen, so one pass closes the remaining difference.
                if let Some(top) = quorum_reg(&s.a) {
                    for i in 0..N {
                        if top.newer_than(&s.b[i]) {
                            s.b[i] = top;
                        }
                    }
                }
            }
            HandoverAction::GoLive => {
                if self.install == Install::AfterDrain && !self.drained(&s) {
                    return None;
                }
                s.live = true;
            }
        }
        Some(s)
    }

    fn properties(&self) -> Vec<Property<Self>> {
        vec![
            // Per-page version monotonicity survives the membership change only by drain.
            Property::<Self>::always("acknowledged versions increase", |_, s| {
                s.history.windows(2).all(|w| w[0].version < w[1].version)
            }),
            Property::<Self>::always("acknowledged writes are not clobbered", |_, s| {
                s.history.iter().all(|h| {
                    s.a.iter()
                        .chain(s.b.iter())
                        .all(|r| r.version != h.version || r.value == h.value)
                })
            }),
            Property::<Self>::sometimes("the destination takes over", |_, s| {
                s.live && !s.history.is_empty()
            }),
            Property::<Self>::sometimes("a forward is lost and the drain carries it", |_, s| {
                s.live && !s.forwards.is_empty() && quorum_reg(&s.b).is_some()
            }),
        ]
    }
}

#[test]
fn sealed_handover_preserves_acknowledged_writes() {
    Handover {
        install: Install::AfterDrain,
    }
    .checker()
    .threads(num_threads())
    .spawn_dfs()
    .join()
    .assert_properties();
}

#[test]
fn installing_on_the_seal_alone_rolls_a_write_back() {
    // At the seal the destination may be missing acknowledged pages, since live
    // replication is fire-and-forget. Going live without draining reuses a version.
    Handover {
        install: Install::OnSeal,
    }
    .checker()
    .threads(num_threads())
    .finish_when(until("acknowledged versions increase"))
    .spawn_bfs()
    .join()
    .assert_any_discovery("acknowledged versions increase");
}

// --- Healing: the anti-entropy sweep ---

const MAX_ACCEPTS: u8 = 2;
const MAX_SWEEPS: u8 = 2;

/// How the sweep closes a difference it found in the digests (`heal.rs`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Anti {
    /// The implementation: the comparison finds the address, a group repair closes it.
    Repair,
    /// The tempting shortcut: reconcile the two compared registers, skipping the round.
    Propagate,
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
struct SweepState {
    reg: [Reg; N],
    chosen: Vec<Reg>,
    accepts: u8,
    sweeps: u8,
    /// A comparison that began with two replicas disagreeing and ended with all agreeing.
    healed: bool,
}

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
enum SweepAction {
    /// A one-shot accept reaching `set` and no further; below a quorum it is divergence.
    Accept {
        member: u8,
        guard: u8,
        set: [bool; N],
    },
    /// `a` finds a bucket whose digest differs from `b`'s and reconciles it.
    Compare { a: u8, b: u8 },
}

struct Sweep {
    rule: Anti,
}

impl Sweep {
    fn observe(&self, s: &mut SweepState) {
        for i in 0..N {
            if s.reg[i].version == 0 {
                continue;
            }
            let held = s.reg.iter().filter(|r| **r == s.reg[i]).count();
            if held >= NEED && !s.chosen.contains(&s.reg[i]) {
                s.chosen.push(s.reg[i]);
                s.chosen.sort();
            }
        }
    }
}

impl Model for Sweep {
    type State = SweepState;
    type Action = SweepAction;

    fn init_states(&self) -> Vec<Self::State> {
        vec![SweepState {
            reg: [Reg::default(); N],
            chosen: Vec::new(),
            accepts: 0,
            sweeps: 0,
            healed: false,
        }]
    }

    fn actions(&self, s: &Self::State, out: &mut Vec<Self::Action>) {
        if s.accepts < MAX_ACCEPTS {
            for member in 0..N as u8 {
                for guard in 0..MAX_VERSION {
                    for mask in 1..(1u8 << N) {
                        let set = [mask & 1 != 0, mask & 2 != 0, mask & 4 != 0];
                        out.push(SweepAction::Accept { member, guard, set });
                    }
                }
            }
        }
        if s.sweeps < MAX_SWEEPS {
            for a in 0..N as u8 {
                for b in 0..N as u8 {
                    if a != b {
                        out.push(SweepAction::Compare { a, b });
                    }
                }
            }
        }
    }

    fn next_state(&self, last: &Self::State, action: Self::Action) -> Option<Self::State> {
        let mut s = last.clone();
        match action {
            SweepAction::Accept { member, guard, set } => {
                // One term for everyone: within a term the guard orders writers, and two
                // proposers on one version with different ballots is what the sweep faces.
                let b = ballot(1, member);
                let v = Reg {
                    version: guard + 1,
                    ballot: b,
                    value: s.accepts + 1,
                };
                s.accepts += 1;
                for (i, hit) in set.iter().enumerate() {
                    if *hit && s.reg[i].version == guard && b >= s.reg[i].ballot {
                        s.reg[i] = v;
                    }
                }
                self.observe(&mut s);
            }
            SweepAction::Compare { a, b } => {
                let (a, b) = (a as usize, b as usize);
                // Equal digests: the sweep does no work where replicas already agree.
                if s.reg[a] == s.reg[b] {
                    return None;
                }
                s.sweeps += 1;
                match self.rule {
                    Anti::Repair => {
                        if let Some(best) = choose(&s.reg, true) {
                            for r in s.reg.iter_mut() {
                                if best.newer_than(r) {
                                    *r = best;
                                }
                            }
                        }
                    }
                    Anti::Propagate => {
                        let best = if s.reg[b].newer_than(&s.reg[a]) {
                            s.reg[b]
                        } else {
                            s.reg[a]
                        };
                        for i in [a, b] {
                            if best.newer_than(&s.reg[i]) {
                                s.reg[i] = best;
                            }
                        }
                    }
                }
                s.healed |= s.reg[0].version > 0 && s.reg[0] == s.reg[1] && s.reg[1] == s.reg[2];
                self.observe(&mut s);
            }
        }
        Some(s)
    }

    fn properties(&self) -> Vec<Property<Self>> {
        vec![
            // The sweep touches registers nobody asked for, so it could undo a decision.
            Property::<Self>::always("one value per version", |_, s| {
                s.chosen.iter().all(|a| {
                    s.chosen
                        .iter()
                        .all(|b| a.version != b.version || a.value == b.value)
                })
            }),
            Property::<Self>::sometimes("a divergence nobody read is healed", |_, s| s.healed),
        ]
    }
}

#[test]
fn the_sweep_heals_a_gap_without_undoing_a_write() {
    Sweep { rule: Anti::Repair }
        .checker()
        .threads(num_threads())
        .spawn_dfs()
        .join()
        .assert_properties();
}

#[test]
fn reconciling_the_compared_pair_instead_of_the_group_loses_a_write() {
    // The newer of two is not the chosen of three: copying it onto the compared pair is
    // enough to give a losing proposal its second acceptor.
    Sweep {
        rule: Anti::Propagate,
    }
    .checker()
    .threads(num_threads())
    .finish_when(until("one value per version"))
    .spawn_bfs()
    .join()
    .assert_any_discovery("one value per version");
}

fn num_threads() -> usize {
    std::thread::available_parallelism()
        .map(|n| n.get())
        .unwrap_or(1)
}
