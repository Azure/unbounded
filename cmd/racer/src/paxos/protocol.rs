use crate::alloc::Status;
use crate::config::Kind;

/// A CASPaxos ballot: `term` in the high 30 bits, proposer index in the low two.
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

    pub(super) fn term(self) -> u32 {
        self.0 >> 2
    }

    pub(super) fn member(self) -> u8 {
        (self.0 & 3) as u8
    }
}

/// What an acceptor holds for one page.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Debug, Default)]
pub struct Register {
    pub version: u64,
    pub ballot: Ballot,
}

impl Register {
    pub(crate) fn key(self) -> (u64, u32) {
        (self.version, self.ballot.raw())
    }

    pub(crate) fn accepted(guard: u64, ballot: Ballot) -> Register {
        Register {
            version: guard + 1,
            ballot,
        }
    }
}

/// The guarded acceptance rule, the whole of what an acceptor decides about an `ACCEPT`.
pub(crate) fn admits(current: u64, held: Option<Ballot>, guard: u64, ballot: Ballot) -> bool {
    current < guard || (current == guard && held.is_none_or(|h| ballot >= h))
}

/// Whether a ballot satisfies an acceptor's standing promise.
pub(super) fn promised(promise: u32, b: Ballot) -> bool {
    b.term() >= promise
}

/// The apply-if-newer rule behind repair, `LEARN` and migration.
pub(crate) fn supersedes(held: (u64, u32), r: Register, equal: bool) -> bool {
    held < r.key() || (held == r.key() && equal)
}

/// Whether a proposer's own leg plus `peers` peer accepts carry the round.
pub(super) fn carried(peers: usize, need: usize) -> bool {
    peers + 1 >= need
}

/// Recover the highest promise held by the other group members.
pub(super) fn recovered_term(peers: [u32; 2]) -> u32 {
    peers[0].max(peers[1])
}

/// The seal table's idempotent, monotone rule.
pub(super) fn sealed_at(held: Option<u32>, term: u32) -> u32 {
    held.map_or(term, |h| h.max(term))
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Choice {
    Chosen(Register),
    Free(Register),
    Ambiguous,
    Missing,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(super) enum Settled {
    Chosen(Register),
    Free(Register),
}

impl Settled {
    pub(super) fn register(self) -> Register {
        match self {
            Settled::Chosen(r) | Settled::Free(r) => r,
        }
    }

    pub(super) fn step_down(self) -> Option<Register> {
        match self {
            Settled::Free(r) => Some(r),
            Settled::Chosen(_) => None,
        }
    }
}

/// Decide which reported register was chosen.
pub(crate) fn choose(
    regs: &[Option<Register>; 3],
    answered: usize,
    silent: usize,
    need: usize,
    kind: Kind,
    below: Option<Register>,
) -> Choice {
    let unseen = 3 - answered;
    let mut vers: Vec<u64> = regs.iter().flatten().map(|r| r.version).collect();
    vers.sort_unstable_by(|a, b| b.cmp(a));
    vers.dedup();
    let mut chosen = None;
    let floor = vers
        .iter()
        .copied()
        .find(|v| regs.iter().flatten().filter(|r| r.version >= *v).count() + unseen >= need)
        .unwrap_or_default();
    for v in vers.iter().copied() {
        if v < floor {
            break;
        }
        if super::unwritten(kind, v) {
            continue;
        }
        let at: Vec<Register> = regs
            .iter()
            .flatten()
            .copied()
            .filter(|r| r.version == v)
            .collect();
        let above = regs.iter().flatten().filter(|r| r.version > v).count();
        let mut cands: Vec<Register> = Vec::new();
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
        if silent > 0 && need > 1 {
            return Choice::Ambiguous;
        }
        match cands.len() {
            1 => {
                chosen = Some(cands[0]);
                break;
            }
            0 => continue,
            _ if unseen == 0 => {
                chosen = regs.iter().flatten().copied().max_by_key(|r| r.key());
                break;
            }
            _ => return Choice::Ambiguous,
        }
    }
    match chosen {
        Some(r) => Choice::Chosen(r),
        // Getting here with a member still silent means no version a quorum could stand
        // behind carries a value at all: a written one would have been settled or refused
        // inside the loop. So the only members that could be holding a chosen value are the
        // ones that answered with no register, and a value is chosen only once `need`
        // acceptors hold it. A member that answered is proof it carries nothing, which is
        // why the count here is the members we never saw a register from rather than the
        // members we could not reach: silence hides a chosen value only when the unseen
        // could add up to a quorum between them. Refusing without that was refusing an
        // answer the round had already proved, and a page nobody ever wrote could then be
        // neither read nor written for as long as one member of its trio stayed down.
        None if need > 1 && silent > 0 && unseen >= need => Choice::Ambiguous,
        None => regs
            .iter()
            .flatten()
            .copied()
            .filter(|r| r.version >= floor && below.is_none_or(|b| r.key() < b.key()))
            .max_by_key(|r| r.key())
            .map_or(Choice::Missing, Choice::Free),
    }
}

/// What the write gate decided for an address.
pub(super) enum Gate {
    Serve { replaying: bool },
    Away(u32),
}

impl Gate {
    pub(super) fn decide(sealed: bool, replaying: bool, next: Option<u32>) -> Result<Gate, Status> {
        if !sealed {
            return Ok(Gate::Serve { replaying });
        }
        match next {
            Some(z) => Ok(Gate::Away(z)),
            None => Err(Status::Conflict { current: 0 }),
        }
    }

    pub(super) fn accepts(self) -> Result<(), Status> {
        match self {
            Gate::Serve { replaying: true } => Err(Status::Io),
            _ => Ok(()),
        }
    }
}

/// A group's standing promise and whether this node may issue at it.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub(super) enum Term {
    Unheld(u32),
    Held(u32),
}

impl Term {
    pub(super) fn new(value: u32) -> Term {
        Term::Unheld(value)
    }

    pub(super) fn promise(self) -> u32 {
        match self {
            Term::Unheld(v) | Term::Held(v) => v,
        }
    }

    pub(super) fn issuable(self) -> Option<u32> {
        match self {
            Term::Held(v) => Some(v),
            Term::Unheld(_) => None,
        }
    }

    pub(super) fn raise(&mut self) -> u32 {
        let v = self.promise().saturating_add(1) & 0x3fff_ffff;
        *self = Term::Held(v);
        v
    }

    pub(super) fn adopt(&mut self, term: u32) {
        if term > self.promise() {
            *self = Term::Unheld(term);
        }
    }

    pub(super) fn release(&mut self) {
        *self = Term::Unheld(self.promise());
    }

    pub(super) fn recover(&mut self, peers: [u32; 2]) {
        self.adopt(recovered_term(peers));
        self.release();
    }
}
