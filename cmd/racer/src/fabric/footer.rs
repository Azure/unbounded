use crate::runtime::Errno;

use super::{BLOCK, BUCKETS, Cursor, Member, TUPLES, status};

// A trailer is one block of little-endian `u64` slots. There is no type tag on the wire:
// the opcode picks the record, so each record has a type here and no caller ever names a
// slot. Encoders zero-fill and decoders reject anything set past the record they know.
//
//   PING     0 node id   1 config generation  2 topology epoch
//   GETMETA  0 version   1 ballot   2 state              3 cache width
//   GET      0 version   1 ballot   2 state              3 cache width (reply, gather)
//   PREPARE  0 version   1 ballot   2 promised term (after the bump)
//   ACCEPT   0 guard     1 ballot   2 topology epoch     (4 KiB only)
//   TRIM     0 guard     1 ballot   2 topology epoch
//   LEARN    0 version   1 ballot   2 member holding it  3 repair
//   WARM     0 version              2 stage
//   SEAL                 1 term     2 extent
//   TERM                            2 promised term      (reply)
//   MERKLE   0..511 digests                              (reply, fills the block)
//   SNAPNEXT 0 count     1 done     then 3 slots per page (reply)
//   SNAPOPEN 0 cursor id                                 (reply)
//
// A 4 MiB `ACCEPT` has no trailer, because a huge frame's whole stride is payload and a
// ublk request buffer has no address a vectored SQE could gather beside. Its guard and
// ballot are derived by the acceptor instead: see `paxos::accept`.
//
// The topology epoch rides the trailer of every routed write and of no bare read: a read
// served from a stale epoch is absorbed by the quorum.

/// One trailer record. Uniform so the target can load, decode, encode and store a footer
/// without naming the record twice; every implementation is one of the types below.
pub(crate) trait Footer: Sized {
    /// Lay the record out in a whole block, zeroing everything it does not define.
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno>;
    /// Read the record back, rejecting anything malformed.
    fn decode(t: &[u8]) -> Result<Self, Errno>;
}

/// A trailer being read. Every accessor is strict: a field that does not fit its type,
/// a flag that is not zero or one, and a slot the record does not define but a peer set
/// anyway are all [`status::BAD`]. Nothing here panics on a peer's bytes.
struct Slots<'a>(&'a [u8]);

impl<'a> Slots<'a> {
    fn new(t: &'a [u8]) -> Result<Slots<'a>, Errno> {
        (t.len() == BLOCK).then_some(Slots(t)).ok_or(status::BAD)
    }

    fn raw(&self, i: usize) -> u64 {
        u64::from_le_bytes(self.0[i * 8..i * 8 + 8].try_into().unwrap())
    }

    fn u32(&self, i: usize) -> Result<u32, Errno> {
        u32::try_from(self.raw(i)).map_err(|_| status::BAD)
    }

    fn u8(&self, i: usize) -> Result<u8, Errno> {
        u8::try_from(self.raw(i)).map_err(|_| status::BAD)
    }

    fn flag(&self, i: usize) -> Result<bool, Errno> {
        match self.raw(i) {
            0 => Ok(false),
            1 => Ok(true),
            _ => Err(status::BAD),
        }
    }

    /// Every byte from slot `i` to the end of the block must be zero. Encoders always
    /// zero-fill, so a set bit past the record is a peer saying something we do not
    /// understand.
    fn tail(&self, i: usize) -> Result<(), Errno> {
        self.0[i * 8..]
            .iter()
            .all(|&b| b == 0)
            .then_some(())
            .ok_or(status::BAD)
    }

    fn reg(&self, version: usize, ballot: usize) -> Result<Reg, Errno> {
        Ok(Reg {
            version: self.raw(version),
            ballot: self.u32(ballot)?,
        })
    }
}

/// A trailer being written. Construction zero-fills, so a record never leaks whatever
/// the pooled buffer held and the reader's tail check means what it says.
struct Fill<'a>(&'a mut [u8]);

impl<'a> Fill<'a> {
    fn new(t: &'a mut [u8]) -> Result<Fill<'a>, Errno> {
        if t.len() != BLOCK {
            return Err(status::BAD);
        }
        t.fill(0);
        Ok(Fill(t))
    }

    fn put(&mut self, i: usize, v: u64) -> &mut Self {
        self.0[i * 8..i * 8 + 8].copy_from_slice(&v.to_le_bytes());
        self
    }

    fn reg(&mut self, version: usize, ballot: usize, r: Reg) -> &mut Self {
        self.put(version, r.version).put(ballot, r.ballot as u64)
    }
}

/// A page's consensus register as it travels: the wire's own `(version, ballot)`, with no
/// dependency on the consensus types that give the ballot meaning.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub(crate) struct Reg {
    pub(crate) version: u64,
    pub(crate) ballot: u32,
}

/// The immutable-state hint a `GETMETA` carries, in `0..3`. Advisory: a reader uses it to
/// date a copy, never to decide one.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub(crate) struct State(u8);

impl State {
    pub(crate) const ZERO: State = State(0);

    pub(crate) fn new(s: u8) -> Option<State> {
        (s < 3).then_some(State(s))
    }

    pub(crate) fn get(self) -> u8 {
        self.0
    }
}

/// `GET` (gathered) and `GETMETA` reply: what we hold and how widely it is cached.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub(crate) struct MetaReply {
    pub(crate) reg: Reg,
    pub(crate) state: State,
    /// Replicas the owner believes hold a copy. Zero from a cache or a confirmed read,
    /// which own no sketch.
    pub(crate) width: u8,
}

impl Footer for MetaReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?
            .reg(0, 1, self.reg)
            .put(2, self.state.get() as u64)
            .put(3, self.width as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<MetaReply, Errno> {
        let s = Slots::new(t)?;
        s.tail(4)?;
        Ok(MetaReply {
            reg: s.reg(0, 1)?,
            state: State::new(s.u8(2)?).ok_or(status::BAD)?,
            width: s.u8(3)?,
        })
    }
}

/// `PREPARE` reply: what we hold, and the promise we just raised.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct PrepareReply {
    pub(crate) reg: Reg,
    pub(crate) term: u32,
}

impl Footer for PrepareReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?.reg(0, 1, self.reg).put(2, self.term as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<PrepareReply, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        Ok(PrepareReply {
            reg: s.reg(0, 1)?,
            term: s.u32(2)?,
        })
    }
}

/// The version an `ACCEPT` overwrites.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Guard {
    /// The proposer did not name one: the acceptor derives it. A 4 MiB `ACCEPT` has no
    /// trailer at all and is always this.
    Derived,
    /// Overwrite exactly this version, or refuse.
    At(u64),
}

impl Guard {
    /// The sentinel is out of a version counter's reach, so no legal guard collides.
    const DERIVED: u64 = u64::MAX;

    fn raw(self) -> Result<u64, Errno> {
        match self {
            Guard::Derived => Ok(Guard::DERIVED),
            Guard::At(Guard::DERIVED) => Err(status::BAD),
            Guard::At(v) => Ok(v),
        }
    }

    fn from_raw(v: u64) -> Guard {
        match v {
            Guard::DERIVED => Guard::Derived,
            v => Guard::At(v),
        }
    }
}

/// 4 KiB `ACCEPT` request, beside the page.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct AcceptReq {
    pub(crate) guard: Guard,
    pub(crate) ballot: u32,
    /// The topology epoch the proposer placed this page under.
    pub(crate) epoch: u32,
}

impl Footer for AcceptReq {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        let guard = self.guard.raw()?;
        Fill::new(out)?
            .put(0, guard)
            .put(1, self.ballot as u64)
            .put(2, self.epoch as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<AcceptReq, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        Ok(AcceptReq {
            guard: Guard::from_raw(s.raw(0)),
            ballot: s.u32(1)?,
            epoch: s.u32(2)?,
        })
    }
}

/// `TRIM` request. Its guard is always a real version: a delete names what it deletes.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct TrimReq {
    pub(crate) guard: u64,
    pub(crate) ballot: u32,
    pub(crate) epoch: u32,
}

impl Footer for TrimReq {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?
            .put(0, self.guard)
            .put(1, self.ballot as u64)
            .put(2, self.epoch as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<TrimReq, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        Ok(TrimReq {
            guard: s.raw(0),
            ballot: s.u32(1)?,
            epoch: s.u32(2)?,
        })
    }
}

/// `LEARN` request: the value, who holds it, and why we are saying so.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct LearnReq {
    pub(crate) reg: Reg,
    /// The member whose copy the receiver should pull.
    pub(crate) from: Member,
    /// A repair rather than a migration push, which also admits the equal-register case:
    /// the entry matches but the bytes fail their checksum.
    pub(crate) repair: bool,
}

impl Footer for LearnReq {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?
            .reg(0, 1, self.reg)
            .put(2, self.from.index() as u64)
            .put(3, self.repair as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<LearnReq, Errno> {
        let s = Slots::new(t)?;
        s.tail(4)?;
        Ok(LearnReq {
            reg: s.reg(0, 1)?,
            from: Member::new(s.u8(2)?).ok_or(status::BAD)?,
            repair: s.flag(3)?,
        })
    }
}

/// How far a warm has travelled. Bounded so a warm cannot be relayed forever.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Stage {
    /// Straight from the writing zone, addressed at a gateway.
    Inbound = 0,
    /// Relayed by a gateway to the cohort member that will hold the copy.
    Holder = 1,
}

/// `WARM` request. Names no ballot and no holder: the receiver reads the page through the
/// ordinary cross-zone path if it wants it at all.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct WarmReq {
    pub(crate) version: u64,
    pub(crate) stage: Stage,
}

impl Footer for WarmReq {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?
            .put(0, self.version)
            .put(2, self.stage as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<WarmReq, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        if s.raw(1) != 0 {
            return Err(status::BAD);
        }
        Ok(WarmReq {
            version: s.raw(0),
            stage: match s.raw(2) {
                0 => Stage::Inbound,
                1 => Stage::Holder,
                _ => return Err(status::BAD),
            },
        })
    }
}

/// `SEAL` request. Names an extent, so its address is here and not in the frame.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct SealReq {
    pub(crate) term: u32,
    pub(crate) extent: u32,
}

impl Footer for SealReq {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?
            .put(1, self.term as u64)
            .put(2, self.extent as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<SealReq, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        if s.raw(0) != 0 {
            return Err(status::BAD);
        }
        Ok(SealReq {
            term: s.u32(1)?,
            extent: s.u32(2)?,
        })
    }
}

/// `TERM` reply: a group's standing promise. Shares the slot `PREPARE` reports its
/// promise in, so a reader of either finds the term in the same place.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct TermReply {
    pub(crate) term: u32,
}

impl Footer for TermReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?.put(2, self.term as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<TermReply, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        if s.raw(0) != 0 || s.raw(1) != 0 {
            return Err(status::BAD);
        }
        Ok(TermReply { term: s.u32(2)? })
    }
}

/// `PING` reply: liveness plus the geometry that dates an answer.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct PingReply {
    pub(crate) node: u32,
    pub(crate) generation: u64,
    /// The arriving universe's topology epoch; a caller has no business learning another's.
    pub(crate) epoch: u32,
}

impl Footer for PingReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?
            .put(0, self.node as u64)
            .put(1, self.generation)
            .put(2, self.epoch as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<PingReply, Errno> {
        let s = Slots::new(t)?;
        s.tail(3)?;
        Ok(PingReply {
            node: s.u32(0)?,
            generation: s.raw(1),
            epoch: s.u32(2)?,
        })
    }
}

/// `MERKLE` reply: one digest per bucket, filling the block exactly.
#[derive(Clone, PartialEq, Eq, Debug)]
pub(crate) struct MerkleReply(pub(crate) Box<[u64; BUCKETS]>);

impl Footer for MerkleReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        let mut f = Fill::new(out)?;
        for (i, d) in self.0.iter().enumerate() {
            f.put(i, *d);
        }
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<MerkleReply, Errno> {
        let s = Slots::new(t)?;
        let mut v = Box::new([0u64; BUCKETS]);
        for (i, d) in v.iter_mut().enumerate() {
            *d = s.raw(i);
        }
        Ok(MerkleReply(v))
    }
}

/// `SNAPOPEN` reply.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct SnapOpenReply {
    pub(crate) cursor: Cursor,
}

impl Footer for SnapOpenReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        Fill::new(out)?.put(0, self.cursor.get() as u64);
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<SnapOpenReply, Errno> {
        let s = Slots::new(t)?;
        s.tail(1)?;
        Ok(SnapOpenReply {
            cursor: Cursor::new(s.u32(0)?),
        })
    }
}

/// One page in a `SNAPNEXT` reply. No page bytes: the reader pulls what it wants with an
/// ordinary `GET`.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct SnapTuple {
    pub(crate) addr: u64,
    pub(crate) reg: Reg,
}

/// `SNAPNEXT` reply: a bounded run of pages and whether the cursor is spent.
#[derive(Clone, PartialEq, Eq, Debug)]
pub(crate) struct SnapNextReply {
    pub(crate) done: bool,
    tuples: Vec<SnapTuple>,
}

impl SnapNextReply {
    /// Pages one reply holds.
    pub(crate) const CAPACITY: usize = TUPLES;

    /// `None` if the run does not fit one block; a caller sizes its own batch.
    pub(crate) fn new(done: bool, tuples: Vec<SnapTuple>) -> Option<SnapNextReply> {
        (tuples.len() <= TUPLES).then_some(SnapNextReply { done, tuples })
    }

    pub(crate) fn tuples(&self) -> &[SnapTuple] {
        &self.tuples
    }
}

impl Footer for SnapNextReply {
    fn encode(&self, out: &mut [u8]) -> Result<(), Errno> {
        if self.tuples.len() > TUPLES {
            return Err(status::BAD);
        }
        let mut f = Fill::new(out)?;
        f.put(0, self.tuples.len() as u64).put(1, self.done as u64);
        for (i, t) in self.tuples.iter().enumerate() {
            f.put(2 + 3 * i, t.addr).reg(3 + 3 * i, 4 + 3 * i, t.reg);
        }
        Ok(())
    }

    fn decode(t: &[u8]) -> Result<SnapNextReply, Errno> {
        let s = Slots::new(t)?;
        let n = s.raw(0);
        if n > TUPLES as u64 {
            return Err(status::BAD);
        }
        let n = n as usize;
        s.tail(2 + 3 * n)?;
        let mut tuples = Vec::with_capacity(n);
        for i in 0..n {
            tuples.push(SnapTuple {
                addr: s.raw(2 + 3 * i),
                reg: s.reg(3 + 3 * i, 4 + 3 * i)?,
            });
        }
        Ok(SnapNextReply {
            done: s.flag(1)?,
            tuples,
        })
    }
}
