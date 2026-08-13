//! Node-to-node transport: a stateless codec and nothing else.
//!
//! An operation is a plain NVMe read or write against a peer's namespace: read and write
//! are the only verbs nvme-of gives us. Nothing is kept per operation beyond the
//! in-flight SQE; everything is in the LBA or in a 4 KiB trailer beside the payload. The
//! LBA is the RPC, and only a read can carry a rich reply. Retries, quorum, caching and
//! placement live above this file.
//!
//! # What the kernel forces
//!
//! 1. Two address regions, not sub-frames, because `imm` sits inside the frame id. Each
//!    region has its own stride; a 4 MiB command is split at the peer's MDTS and
//!    reassembled by LBA offset.
//! 2. 48-bit frame ids: id times stride is a byte offset and must fit `loff_t`.
//! 3. Four statuses; nvmet erases the rest to `EIO`. See [`status`].
//! 4. Relaying is a flag ([`HOPS`]), not an opcode: a read carries no data to the target,
//!    so an inner frame cannot ride an outer one's trailer.
//! 5. Gather copies the 4 KiB payload: `ReadvFixed`/`WritevFixed` take a single
//!    `buf_index`, so a guest ublk page and a pooled trailer cannot share one vectored
//!    fixed SQE. 4 MiB pages never gather and stay zero copy.
//! 6. Direction follows the opcode and shape follows the length, so the client API is
//!    [`Link::send`] plus [`Cmd::decode`]. Both speak [`Cmd`]: the bit packing is private,
//!    so no caller can name a field its opcode does not have.

use std::io;
use std::path::Path;
use std::time::Duration;

use crate::config::Peer;
pub(crate) use crate::layout::Class;
use crate::runtime::{Buf, Configurator, Disk, Durability, Errno};

// --- status ---

/// The status alphabet, as it survives the wire.
///
/// The `ublk -> BLK_STS_* -> NVMe status -> nvme initiator -> errno` pipeline preserves
/// four values: nvmet's `blk_to_nvme_status()` has arms for exactly
/// `BLK_STS_{NOSPC,TARGET,NOTSUPP,MEDIUM}` and sends the rest as `NVME_SC_INTERNAL`,
/// which the initiator turns back into `EIO`. A bare `EIO` reads as a transport failure
/// and escalates into a path failover.
///
/// | here | NVMe status | initiator errno | `DNR` |
/// |------|-------------|-----------------|-------|
/// | [`STALE`]   | `LBA_RANGE`      | `EREMOTEIO`   | yes |
/// | [`MISSING`] | `ACCESS_DENIED`  | `ENODATA`     | **no** |
/// | [`BAD`]     | `INVALID_OPCODE` | `EOPNOTSUPP`  | yes |
/// | [`NOSPC`]   | `CAP_EXCEEDED`   | `ENOSPC`      | yes |
///
/// A status without `NVME_STATUS_DNR` is retried by the initiator up to
/// `nvme_max_retries` (5) before delivery, per `nvme_decide_disposition()`. Only
/// [`MISSING`] lacks it, which is safe because every op is replay-safe, and is why a
/// lost ballot is not reported as [`MISSING`]: consensus would pay six round trips.
/// `EEXIST` and `EAGAIN` need no code of their own; a CORFU fill loser's follow-up `GET`
/// recovers the first, and backpressure delays the completion rather than reporting the
/// second, so queue depth bounds the kernel.
pub(crate) mod status {
    use crate::runtime::Errno;

    /// Wrong ballot, version or placement: recover by asking (`PREPARE` or `GETMETA`),
    /// and re-read the config if the page is not here at all.
    pub(crate) const STALE: Errno = Errno::EREMOTEIO;
    /// Page missing or quarantined. Heal from another group member.
    pub(crate) const MISSING: Errno = Errno::ENODATA;
    /// Malformed frame, unknown opcode, or an op this node does not implement.
    pub(crate) const BAD: Errno = Errno::EOPNOTSUPP;
    /// Out of space.
    pub(crate) const NOSPC: Errno = Errno::ENOSPC;
}

// --- opcodes ---

/// `GET` and `ACCEPT` are the hot path; everything else is rare by construction.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub(crate) enum Op {
    /// Read a page. Reply *is* the payload.
    Get = 0,
    /// Read the register: version, ballot, state, width hint. The hedged-read path.
    GetMeta = 1,
    /// Raise this group's promise and report `(version, ballot, term)`. A read.
    Prepare = 2,
    /// Write a page under a ballot. Reply is a status.
    Accept = 3,
    /// Delete an Immutable page.
    Trim = 4,
    /// Open a snapshot cursor, optionally filtered to one digest bucket.
    SnapOpen = 5,
    /// Advance a snapshot cursor. The cursor is explicit, so the stream is stateless.
    SnapNext = 6,
    /// Bucket digests for anti-entropy.
    Merkle = 7,
    /// Tell a peer its register is stale: it pulls the page and applies it if newer.
    /// Used by repair and by the migration learn.
    Learn = 8,
    /// Freeze a shard at its source group.
    Seal = 9,
    /// Liveness and geometry.
    Ping = 10,
    /// A group's standing promise. Names a group, not a page.
    Term = 11,
    /// Tell another zone that a page it keeps warm has a new value. Advisory both ways:
    /// the sender does not wait and the receiver may decline.
    Warm = 12,
}

impl Op {
    fn from_bits(b: u8) -> Option<Op> {
        Some(match b {
            0 => Op::Get,
            1 => Op::GetMeta,
            2 => Op::Prepare,
            3 => Op::Accept,
            4 => Op::Trim,
            5 => Op::SnapOpen,
            6 => Op::SnapNext,
            7 => Op::Merkle,
            8 => Op::Learn,
            9 => Op::Seal,
            10 => Op::Ping,
            11 => Op::Term,
            12 => Op::Warm,
            _ => return None,
        })
    }

    /// Whether the command is an NVMe read. Interrogative ops are reads, the only
    /// direction a rich reply travels; imperative ops are writes returning a status.
    pub(crate) fn is_read(self) -> bool {
        matches!(
            self,
            Op::Get
                | Op::GetMeta
                | Op::Prepare
                | Op::SnapOpen
                | Op::SnapNext
                | Op::Merkle
                | Op::Ping
                | Op::Term
        )
    }

    /// Whether the op moves no page. Its single block is the trailer. `LEARN` is here
    /// because a 4 MiB frame has no room for a trailer but a repair must carry an exact
    /// `(version, ballot)`: it names the value and its holder, and the receiver pulls
    /// the page with an ordinary `GET`.
    fn is_control(self) -> bool {
        matches!(
            self,
            Op::GetMeta
                | Op::Prepare
                | Op::Trim
                | Op::SnapOpen
                | Op::SnapNext
                | Op::Merkle
                | Op::Learn
                | Op::Seal
                | Op::Ping
                | Op::Term
                | Op::Warm
        )
    }
}

/// Forwarding hops this frame may still take, in flag bits 0..1.
///
/// A budget, not an instruction: a receiver that is not the addressee ([`To`])
/// spends a hop, forwards the *same* frame with the budget one smaller, and completes
/// the outer command when the inner one does. The outer frame is the inner frame, so a
/// forward works in either direction and needs no trailer. The field holds three;
/// `paxos::Route` grants at most two, since routing is flat inside a universe and a
/// frame never leaves the universe it arrived on. Forwarding with no budget left answers
/// [`status::STALE`], which sends the originator back to its config.
const HOPS: u8 = 0b11;
/// Serve this `GET` from the cache region only, never from the allocator.
///
/// A modifier on `GET` and on the `GETMETA` paired with a 4 MiB one: a reader that
/// believes a cohort peer holds a replica asks it directly, concurrently with the
/// mandatory metadata round. A miss or a shedding replica answers [`status::MISSING`]
/// and the reader falls back to the group, so declining is always safe. A 4 KiB reply
/// puts the register the copy claims in the trailer beside the page; a 4 MiB reply has
/// no trailer, so that register rides the paired `GETMETA`. Neither carries a width
/// hint, because a replica does not own the sketch.
const CACHE_ONLY: u8 = 1 << 2;

// --- wire format ---

// Two regions, each with its own stride, so a 4 MiB payload is one contiguous run of
// blocks rather than a scatter of sub-frames:
//
//   small   lba = frame * 2                 frame < 2^48   block 0 payload, 1 trailer
//   huge    lba = HUGE_BASE + frame * 1024  frame < 2^38   blocks 0..1023 payload
//
// A frame id packs, from the low bit up:
//
//   | offset (38 small / 28 huge) | imm 2 | flags 3 | opcode 5 |
//
// There is no address-space field: the namespace a frame arrived on is the universe. The
// control plane publishes one device per universe and attaches it only to that universe's
// members, so partitioning is enforced by the transport rather than by a number a peer
// could choose, which is what makes a universe a security boundary.
//
// `offset` is a page index in the universe's own flat LBA space: 4 KiB pages count
// blocks directly, 4 MiB pages count 1024-block groups. Both regions address exactly
// `config::MAX_LBA` blocks, so any address the control plane may legally hand out is
// reachable.
const OP_BITS: u32 = 5;
const FLAG_BITS: u32 = 3;
const IMM_BITS: u32 = 2;
const SMALL_OFF_BITS: u32 = 38;
const HUGE_OFF_BITS: u32 = 28;

const SMALL_SHIFT: u32 = 1;
const HUGE_SHIFT: u32 = 10;
/// Blocks in a 4 MiB page.
const HUGE_BLOCKS: u64 = 1 << HUGE_SHIFT;

/// First LBA of the 4 MiB region. Also the size of the 4 KiB region.
const HUGE_BASE_LBA: u64 = 1 << (SMALL_OFF_BITS + IMM_BITS + FLAG_BITS + OP_BITS + SMALL_SHIFT);
const MAX_LBA: u64 = HUGE_BASE_LBA * 2;

/// Size the fabric device must be declared with: 4 EiB of sparse address space, never
/// storage.
pub(crate) const DEVICE_SIZE: u64 = MAX_LBA * BLOCK as u64;

/// The logical block, and so the trailer size. Oversized on purpose: it never reaches
/// media, it only spares a second round trip, and 4 KiB keeps every buffer page-aligned
/// and RDMA-friendly.
pub(crate) const BLOCK: usize = 4096;

/// Slots in one trailer.
const SLOTS: usize = BLOCK / 8;

/// Digest buckets a group's address space is cut into, and so the width of a `MERKLE`
/// reply. A digest vector fills the block exactly, which is why anti-entropy is 512 wide
/// and one level deep instead of a tree.
pub(crate) const BUCKETS: usize = SLOTS;

/// Pages one `SNAPNEXT` reply can carry: three slots each after a two-slot header.
const TUPLES: usize = (SLOTS - 2) / 3;

// Both regions cover a whole universe, so config validation's `base_lba + blocks <=
// MAX_LBA` is the only bound a page address needs; the fabric adds none of its own.
const _: () = assert!(1u64 << SMALL_OFF_BITS == crate::config::MAX_LBA);
const _: () = assert!((1u64 << HUGE_OFF_BITS) * HUGE_BLOCKS == crate::config::MAX_LBA);

const fn mask(bits: u32) -> u64 {
    (1u64 << bits) - 1
}

/// Forwarding hops `flags` still allows.
fn hops(flags: u8) -> u8 {
    flags & HOPS
}

/// Which blocks of a frame a request covers, and so what the payload means.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Part {
    /// Bare mode: page bytes only, starting `off` into the page. `off` is 0 for a 4 KiB
    /// page; on a 4 MiB transfer it is how the MDTS-split pieces reassemble.
    Payload { off: usize },
    /// Control mode: one trailer block, no page.
    Trailer,
    /// Gather mode: a page followed by its trailer, in one command.
    Both,
}

/// A decoded LBA. Pure data: hand one to [`Link::send`] and it is a command, get one
/// back from [`Frame::decode`] and it is a request.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
struct Frame {
    op: Op,
    /// [`HOPS`] | [`CACHE_ONLY`].
    flags: u8,
    /// Who this frame is addressed to, in two bits, uniformly across every page op.
    ///
    /// Zero means "you own this operation": resolve the address yourself and answer
    /// authoritatively rather than from your own copy, which is how a node holding
    /// neither the extent table nor the catalog for a remote zone still takes a confirmed
    /// read. On `ACCEPT` it means "you are the proposer, pick a ballot". `k + 1` names
    /// member index `k` of the address's group; two bits hold both, because a group is
    /// three wide. The group-addressed ops borrow it for the page class instead; see
    /// `heal.rs`. A receiver that is not the addressee forwards, if [`HOPS`] allows.
    imm: u8,
    /// Whether `offset` counts 4 MiB pages rather than 4 KiB ones. Selects the region.
    huge: bool,
    /// Page index in the universe for a page op; op-specific for the group-addressed
    /// ops, which name no page at all.
    offset: u64,
}

impl Frame {
    /// The block this frame's page starts at, in the universe's address space. The
    /// inverse of the division [`Cmd::frame`] does, and meaningless on a group-addressed
    /// frame.
    fn lba(&self) -> u64 {
        if self.huge {
            self.offset * HUGE_BLOCKS
        } else {
            self.offset
        }
    }

    /// The frame's base LBA, block 0 of its footprint. Total: out-of-range fields are
    /// masked, not rejected, because every caller has already validated its address
    /// against the config and a masked frame just fails its bounds check at the far end.
    fn encode(&self) -> u64 {
        let (off_bits, base, shift) = if self.huge {
            (HUGE_OFF_BITS, HUGE_BASE_LBA, HUGE_SHIFT)
        } else {
            (SMALL_OFF_BITS, 0, SMALL_SHIFT)
        };
        let mut id = self.offset & mask(off_bits);
        id |= (self.imm as u64 & mask(IMM_BITS)) << off_bits;
        id |= (self.flags as u64 & mask(FLAG_BITS)) << (off_bits + IMM_BITS);
        id |= (self.op as u64) << (off_bits + IMM_BITS + FLAG_BITS);
        base + (id << shift)
    }

    /// The inverse, plus the frame shape implied by the transfer length. Pure and total:
    /// any `(lba, len)` either decodes or is [`status::BAD`], and none panics. The target
    /// runs this on bytes a peer chose, so totality is a safety property here.
    fn decode(lba: u64, len: usize) -> Result<(Frame, Part), Errno> {
        if lba >= MAX_LBA || len == 0 || !len.is_multiple_of(BLOCK) {
            return Err(status::BAD);
        }
        let blocks = (len / BLOCK) as u64;
        let (huge, off_bits, id, block) = if lba >= HUGE_BASE_LBA {
            let r = lba - HUGE_BASE_LBA;
            (true, HUGE_OFF_BITS, r >> HUGE_SHIFT, r & (HUGE_BLOCKS - 1))
        } else {
            (false, SMALL_OFF_BITS, lba >> SMALL_SHIFT, lba & 1)
        };
        let imm_sh = off_bits;
        let flag_sh = imm_sh + IMM_BITS;
        let op_sh = flag_sh + FLAG_BITS;
        let op = Op::from_bits(((id >> op_sh) & mask(OP_BITS)) as u8).ok_or(status::BAD)?;
        let f = Frame {
            op,
            flags: ((id >> flag_sh) & mask(FLAG_BITS)) as u8,
            imm: ((id >> imm_sh) & mask(IMM_BITS)) as u8,
            huge,
            offset: id & mask(off_bits),
        };
        let part = if op.is_control() {
            if block != 0 || blocks != 1 {
                return Err(status::BAD);
            }
            Part::Trailer
        } else if huge {
            if block + blocks > HUGE_BLOCKS {
                return Err(status::BAD);
            }
            Part::Payload {
                off: block as usize * BLOCK,
            }
        } else {
            match (block, blocks) {
                (0, 1) => Part::Payload { off: 0 },
                (0, 2) => Part::Both,
                _ => return Err(status::BAD),
            }
        };
        Ok((f, part))
    }
}

// --- addressing ---

/// A member index inside a three-wide consensus group.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct Member(u8);

impl Member {
    /// `None` unless `k` names one of the three members.
    pub(crate) fn new(k: u8) -> Option<Member> {
        (k < 3).then_some(Member(k))
    }

    pub(crate) fn index(self) -> u8 {
        self.0
    }
}

/// A forwarding budget: how many more hops this frame may take. See [`HOPS`].
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub(crate) struct Hops(u8);

impl Hops {
    /// Direct: the addressee is the peer we hand the frame to.
    pub(crate) const NONE: Hops = Hops(0);
    /// One relay, which is all a member-addressed frame inside a zone ever needs.
    pub(crate) const ONE: Hops = Hops(1);
    /// Two relays: a gateway may cross a zone and then land on the member.
    pub(crate) const TWO: Hops = Hops(2);

    pub(crate) fn get(self) -> u8 {
        self.0
    }

    /// The budget after one relay, or `None` when there is none left to spend.
    fn spend(self) -> Option<Hops> {
        self.0.checked_sub(1).map(Hops)
    }
}

/// Who a page op is addressed to. The wire encoding is [`Frame::imm`].
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum To {
    /// "You own this operation": resolve the address yourself and answer
    /// authoritatively. On `ACCEPT` it means "you are the proposer, pick a ballot".
    Owner,
    /// Member `k` of the address's group, and nobody else.
    Member(Member),
}

impl To {
    /// The addressee as it travels: zero for the owner, `k + 1` for member `k`. Also the
    /// proposer token a split 4 MiB accept reassembles under, since pieces of two racing
    /// proposals must not be mixed.
    pub(crate) fn imm(self) -> u8 {
        match self {
            To::Owner => 0,
            To::Member(m) => m.index() + 1,
        }
    }

    fn from_imm(imm: u8) -> Result<To, Errno> {
        match imm {
            0 => Ok(To::Owner),
            k => Member::new(k - 1).map(To::Member).ok_or(status::BAD),
        }
    }
}

/// An addressee plus the budget to reach it: the routing half of every page op.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct Via {
    pub(crate) to: To,
    pub(crate) hops: Hops,
}

impl Via {
    pub(crate) fn new(to: To, hops: Hops) -> Via {
        Via { to, hops }
    }

    /// Straight at the addressee, no relay allowed.
    pub(crate) fn direct(to: To) -> Via {
        Via {
            to,
            hops: Hops::NONE,
        }
    }
}

/// Where a `GET` or `GETMETA` may be answered from.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Source {
    /// The allocator, through the group. Routable.
    Group(Via),
    /// The cache region of the peer we hand it to, and nowhere else. Never routed and
    /// never addressed to a member, because a cached copy belongs to no group role.
    Cache,
}

/// A page named in the arriving universe's address space, with its class.
///
/// The class is not a decoration: it picks the address region, and so the stride, so a
/// page reference is meaningless without it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct PageRef {
    class: Class,
    /// The page index in its region, already range-checked.
    index: u64,
}

impl PageRef {
    /// `lba` counts 4 KiB blocks whatever the class, because that is the unit the control
    /// plane places extents in; a 4 MiB page's `lba` is the first of its 1024. `None`
    /// when the address does not fit the class's region or, for a 4 MiB page, is not on
    /// a page boundary.
    pub(crate) fn new(class: Class, lba: u64) -> Option<PageRef> {
        let index = match class {
            Class::Small => lba,
            Class::Huge => {
                if !lba.is_multiple_of(HUGE_BLOCKS) {
                    return None;
                }
                lba / HUGE_BLOCKS
            }
        };
        let bits = match class {
            Class::Small => SMALL_OFF_BITS,
            Class::Huge => HUGE_OFF_BITS,
        };
        (index < 1 << bits).then_some(PageRef { class, index })
    }

    pub(crate) fn class(self) -> Class {
        self.class
    }

    /// The block this page starts at, the inverse of [`PageRef::new`].
    pub(crate) fn lba(self) -> u64 {
        match self.class {
            Class::Small => self.index,
            Class::Huge => self.index * HUGE_BLOCKS,
        }
    }
}

/// A consensus group index in the arriving universe's catalog.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct GroupIx(u32);

impl GroupIx {
    /// Bounded by the narrowest field any group op has for it, `SNAPOPEN`'s.
    pub(crate) const MAX: u32 = 1 << (SMALL_OFF_BITS - 10);

    pub(crate) fn new(i: u32) -> Option<GroupIx> {
        (i < GroupIx::MAX).then_some(GroupIx(i))
    }

    pub(crate) fn get(self) -> u32 {
        self.0
    }
}

/// A digest bucket, and so a slice of a group's address space. See `heal.rs`.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct Bucket(u16);

impl Bucket {
    pub(crate) fn new(b: u16) -> Option<Bucket> {
        ((b as usize) < BUCKETS).then_some(Bucket(b))
    }

    pub(crate) fn get(self) -> u16 {
        self.0
    }
}

/// A snapshot cursor handle, minted by the target in a `SNAPOPEN` reply.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct Cursor(u32);

impl Cursor {
    pub(crate) fn new(id: u32) -> Cursor {
        Cursor(id)
    }

    pub(crate) fn get(self) -> u32 {
        self.0
    }
}

/// A `SNAPNEXT` chunk sequence: six bits, enough to tell adjacent chunks and a retry of
/// one apart, which is all the target's cursor needs to be idempotent.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct Seq(u8);

impl Seq {
    const BITS: u32 = 6;
    const MASK: u64 = mask(Seq::BITS);

    pub(crate) fn new(n: u8) -> Option<Seq> {
        ((n as u64) <= Seq::MASK).then_some(Seq(n))
    }

    /// The sequence a caller's own attempt counter names. Cycling is intended: a reader
    /// walks a bucket in more chunks than six bits can count and only ever needs to be
    /// distinguishable from its neighbours.
    pub(crate) fn wrap(n: u8) -> Seq {
        Seq(n & Seq::MASK as u8)
    }

    pub(crate) fn get(self) -> u8 {
        self.0
    }
}

// --- commands ---

/// Which blocks of a page a `GET` asks for.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Want {
    /// A bare 4 KiB page: payload only, no trailer.
    Page,
    /// A 4 KiB page and its register in one command.
    Gather,
    /// Bytes `off..off + len` of a 4 MiB page. `off` is zero for a command a client
    /// phrases; the pieces a target sees come from the nvme layer splitting at MDTS.
    Piece { off: usize },
}

/// Which blocks of a page an `ACCEPT` carries.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Put {
    /// A 4 KiB page and its ballot in one command.
    Gather,
    /// Bytes `off..off + len` of a 4 MiB page. A 4 MiB `ACCEPT` has no trailer at all:
    /// the acceptor derives the guard and the ballot. See `paxos::accept`.
    Piece { off: usize },
}

/// One fabric operation, with exactly the fields its opcode may carry and no others.
///
/// This is the only way to phrase or read a frame. The bit packing below is private, so
/// an illegal combination of opcode, class, addressee, budget and transfer shape cannot
/// be built, sent, or survive [`Cmd::decode`].
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Cmd {
    /// Read a page. The reply *is* the payload.
    Get {
        page: PageRef,
        from: Source,
        want: Want,
    },
    /// Read a page's register. The hedged-read path.
    GetMeta { page: PageRef, from: Source },
    /// Raise this group's promise and report `(version, ballot, term)`.
    Prepare { page: PageRef, via: Via },
    /// Write a page under a ballot.
    Accept { page: PageRef, via: Via, put: Put },
    /// Delete an Immutable page.
    Trim { page: PageRef, via: Via },
    /// Tell a peer its register is stale.
    Learn { page: PageRef, via: Via },
    /// Freeze a shard at its source group. The frame names a page only so the request
    /// routes; the shard it seals is in the trailer.
    Seal { anchor: PageRef, via: Via },
    /// Tell another zone a page it keeps warm has a new value. Advisory, never relayed.
    Warm { page: PageRef },
    /// Bucket digests for anti-entropy.
    Merkle { group: GroupIx, class: Class },
    /// Open a snapshot cursor, optionally filtered to one digest bucket.
    SnapOpen {
        group: GroupIx,
        class: Class,
        bucket: Option<Bucket>,
    },
    /// Advance a snapshot cursor.
    SnapNext { cursor: Cursor, seq: Seq },
    /// A group's standing promise.
    Term { group: GroupIx },
    /// Liveness and geometry.
    Ping,
}

impl Cmd {
    pub(crate) fn op(self) -> Op {
        match self {
            Cmd::Get { .. } => Op::Get,
            Cmd::GetMeta { .. } => Op::GetMeta,
            Cmd::Prepare { .. } => Op::Prepare,
            Cmd::Accept { .. } => Op::Accept,
            Cmd::Trim { .. } => Op::Trim,
            Cmd::SnapOpen { .. } => Op::SnapOpen,
            Cmd::SnapNext { .. } => Op::SnapNext,
            Cmd::Merkle { .. } => Op::Merkle,
            Cmd::Learn { .. } => Op::Learn,
            Cmd::Seal { .. } => Op::Seal,
            Cmd::Ping => Op::Ping,
            Cmd::Term { .. } => Op::Term,
            Cmd::Warm { .. } => Op::Warm,
        }
    }

    /// The page this command routes on and the route it may take, for the ops that have
    /// one. `None` for a cache read, an advisory warm and every group-addressed op: none
    /// of those is ever relayed.
    pub(crate) fn routing(self) -> Option<(PageRef, Via)> {
        match self {
            Cmd::Get {
                page,
                from: Source::Group(via),
                ..
            }
            | Cmd::GetMeta {
                page,
                from: Source::Group(via),
            }
            | Cmd::Prepare { page, via }
            | Cmd::Accept { page, via, .. }
            | Cmd::Trim { page, via }
            | Cmd::Learn { page, via }
            | Cmd::Seal { anchor: page, via } => Some((page, via)),
            _ => None,
        }
    }

    /// The command as it leaves a forwarder: the same request, one hop poorer. `None`
    /// when the op is not routable or the budget is spent, which the target answers with
    /// [`status::STALE`] to send the originator back to its config.
    pub(crate) fn forwarded(self) -> Option<Cmd> {
        let mut c = self;
        let via = match &mut c {
            Cmd::Get {
                from: Source::Group(via),
                ..
            }
            | Cmd::GetMeta {
                from: Source::Group(via),
                ..
            }
            | Cmd::Prepare { via, .. }
            | Cmd::Accept { via, .. }
            | Cmd::Trim { via, .. }
            | Cmd::Learn { via, .. }
            | Cmd::Seal { via, .. } => via,
            _ => return None,
        };
        via.hops = via.hops.spend()?;
        Some(c)
    }

    /// The routing fields as they sit in a frame.
    fn addressed(page: PageRef, via: Via) -> (bool, u8, u8, u64) {
        (
            page.class == Class::Huge,
            via.hops.get(),
            via.to.imm(),
            page.index,
        )
    }

    /// The frame this command packs into, before its transfer shape is checked.
    fn frame(self) -> Frame {
        let (op, huge, flags, imm, offset) = match self {
            Cmd::Get { page, from, .. } => {
                let (huge, flags, imm) = match from {
                    Source::Group(via) => {
                        let (h, f, i, _) = Cmd::addressed(page, via);
                        (h, f, i)
                    }
                    Source::Cache => (page.class == Class::Huge, CACHE_ONLY, 0),
                };
                (Op::Get, huge, flags, imm, page.index)
            }
            Cmd::GetMeta { page, from } => {
                let (huge, flags, imm) = match from {
                    Source::Group(via) => {
                        let (h, f, i, _) = Cmd::addressed(page, via);
                        (h, f, i)
                    }
                    Source::Cache => (page.class == Class::Huge, CACHE_ONLY, 0),
                };
                (Op::GetMeta, huge, flags, imm, page.index)
            }
            Cmd::Prepare { page, via } => {
                let (h, f, i, o) = Cmd::addressed(page, via);
                (Op::Prepare, h, f, i, o)
            }
            Cmd::Accept { page, via, .. } => {
                let (h, f, i, o) = Cmd::addressed(page, via);
                (Op::Accept, h, f, i, o)
            }
            Cmd::Trim { page, via } => {
                let (h, f, i, o) = Cmd::addressed(page, via);
                (Op::Trim, h, f, i, o)
            }
            Cmd::Learn { page, via } => {
                let (h, f, i, o) = Cmd::addressed(page, via);
                (Op::Learn, h, f, i, o)
            }
            Cmd::Seal { anchor, via } => {
                let (h, f, i, o) = Cmd::addressed(anchor, via);
                (Op::Seal, h, f, i, o)
            }
            Cmd::Warm { page } => (Op::Warm, page.class == Class::Huge, 0, 0, page.index),
            Cmd::Merkle { group, class } => (
                Op::Merkle,
                false,
                0,
                (class == Class::Huge) as u8,
                group.get() as u64,
            ),
            Cmd::SnapOpen {
                group,
                class,
                bucket,
            } => {
                let off = (group.get() as u64) << 10
                    | (bucket.is_some() as u64) << 9
                    | bucket.map_or(0, |b| b.get() as u64);
                (Op::SnapOpen, false, 0, (class == Class::Huge) as u8, off)
            }
            Cmd::SnapNext { cursor, seq } => (
                Op::SnapNext,
                false,
                0,
                0,
                (cursor.get() as u64) << Seq::BITS | seq.get() as u64,
            ),
            Cmd::Term { group } => (Op::Term, false, 0, 0, group.get() as u64),
            Cmd::Ping => (Op::Ping, false, 0, 0, 0),
        };
        Frame {
            op,
            flags,
            imm,
            huge,
            offset,
        }
    }

    /// Where in the frame's footprint a transfer of `len` bytes starts, and whether that
    /// shape is one this command is allowed to have.
    fn block(self, len: usize) -> Result<u64, Errno> {
        let piece = |off: usize| -> Result<u64, Errno> {
            let end = off.checked_add(len).ok_or(status::BAD)?;
            if !off.is_multiple_of(BLOCK) || len == 0 || !len.is_multiple_of(BLOCK) {
                return Err(status::BAD);
            }
            if end > HUGE_BLOCKS as usize * BLOCK {
                return Err(status::BAD);
            }
            Ok((off / BLOCK) as u64)
        };
        match self {
            Cmd::Get { page, want, .. } => match (page.class, want) {
                (Class::Small, Want::Page) if len == BLOCK => Ok(0),
                (Class::Small, Want::Gather) if len == 2 * BLOCK => Ok(0),
                (Class::Huge, Want::Piece { off }) => piece(off),
                _ => Err(status::BAD),
            },
            Cmd::Accept { page, put, .. } => match (page.class, put) {
                (Class::Small, Put::Gather) if len == 2 * BLOCK => Ok(0),
                (Class::Huge, Put::Piece { off }) => piece(off),
                _ => Err(status::BAD),
            },
            // Everything else is a control op: one trailer block at the frame's base.
            _ if len == BLOCK => Ok(0),
            _ => Err(status::BAD),
        }
    }

    /// The LBA a transfer of `len` bytes must be issued at. Refuses any shape the
    /// command may not have, so a malformed command costs no round trip.
    pub(crate) fn encode(self, len: usize) -> Result<u64, Errno> {
        let block = self.block(len)?;
        Ok(self.frame().encode() + block)
    }

    /// The inverse. Pure and total: any `(lba, len, read)` either names one legal command
    /// or is [`status::BAD`], and none panics. The target runs this on bytes a peer chose,
    /// so totality is a safety property here.
    pub(crate) fn decode(lba: u64, len: usize, read: bool) -> Result<Cmd, Errno> {
        let (f, part) = Frame::decode(lba, len)?;
        if f.op.is_read() != read {
            return Err(status::BAD);
        }
        let page = || PageRef::new(Class::of(f.huge), f.lba()).ok_or(status::BAD);
        let via = || -> Result<Via, Errno> {
            if f.flags & !HOPS != 0 {
                return Err(status::BAD);
            }
            Ok(Via::new(To::from_imm(f.imm)?, Hops(hops(f.flags))))
        };
        let from = || -> Result<Source, Errno> {
            if f.flags & CACHE_ONLY == 0 {
                return via().map(Source::Group);
            }
            // A cached copy answers for itself: no group role to address and nowhere to
            // relay to.
            if f.flags != CACHE_ONLY || f.imm != 0 {
                return Err(status::BAD);
            }
            Ok(Source::Cache)
        };
        // An advisory warm is never routed: it names a page and a class and nothing else.
        let bare = || -> Result<(), Errno> {
            (f.flags == 0 && f.imm == 0)
                .then_some(())
                .ok_or(status::BAD)
        };
        // The group-addressed ops carry no route and no page, so they never set the huge
        // bit whatever class they ask about; they borrow `imm` for the class or leave it
        // clear.
        let plain = || -> Result<(), Errno> {
            (f.flags == 0 && f.imm == 0 && !f.huge)
                .then_some(())
                .ok_or(status::BAD)
        };
        let class = || -> Result<Class, Errno> {
            (f.flags == 0 && f.imm <= 1 && !f.huge)
                .then(|| Class::of(f.imm == 1))
                .ok_or(status::BAD)
        };
        let group = |i: u64| -> Result<GroupIx, Errno> {
            u32::try_from(i)
                .ok()
                .and_then(GroupIx::new)
                .ok_or(status::BAD)
        };
        Ok(match f.op {
            Op::Get => Cmd::Get {
                page: page()?,
                from: from()?,
                want: match part {
                    Part::Payload { off } if f.huge => Want::Piece { off },
                    Part::Payload { .. } => Want::Page,
                    Part::Both => Want::Gather,
                    Part::Trailer => return Err(status::BAD),
                },
            },
            Op::GetMeta => Cmd::GetMeta {
                page: page()?,
                from: from()?,
            },
            Op::Prepare => Cmd::Prepare {
                page: page()?,
                via: via()?,
            },
            Op::Accept => Cmd::Accept {
                page: page()?,
                via: via()?,
                put: match part {
                    Part::Payload { off } if f.huge => Put::Piece { off },
                    Part::Both => Put::Gather,
                    _ => return Err(status::BAD),
                },
            },
            Op::Trim => Cmd::Trim {
                page: page()?,
                via: via()?,
            },
            Op::Learn => Cmd::Learn {
                page: page()?,
                via: via()?,
            },
            Op::Seal => Cmd::Seal {
                anchor: page()?,
                via: via()?,
            },
            Op::Warm => {
                bare()?;
                Cmd::Warm { page: page()? }
            }
            Op::Merkle => Cmd::Merkle {
                group: group(f.offset)?,
                class: class()?,
            },
            Op::SnapOpen => {
                let class = class()?;
                let filtered = f.offset >> 9 & 1 == 1;
                let low = f.offset & mask(9);
                if !filtered && low != 0 {
                    return Err(status::BAD);
                }
                Cmd::SnapOpen {
                    group: group(f.offset >> 10)?,
                    class,
                    bucket: filtered
                        .then(|| Bucket::new(low as u16).ok_or(status::BAD))
                        .transpose()?,
                }
            }
            Op::SnapNext => {
                plain()?;
                Cmd::SnapNext {
                    cursor: Cursor::new(
                        u32::try_from(f.offset >> Seq::BITS).map_err(|_| status::BAD)?,
                    ),
                    seq: Seq::new((f.offset & Seq::MASK) as u8).ok_or(status::BAD)?,
                }
            }
            Op::Term => {
                plain()?;
                Cmd::Term {
                    group: group(f.offset)?,
                }
            }
            Op::Ping => {
                plain()?;
                if f.offset != 0 {
                    return Err(status::BAD);
                }
                Cmd::Ping
            }
        })
    }
}

// --- trailers ---

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

// --- links ---

/// How long a fabric command may take before it is a path failure. The fabric never
/// retries: expiry surfaces as `ETIME`, consensus treats the replica as non-responding,
/// and the other two members carry the quorum.
const TIMEOUT: Duration = Duration::from_secs(2);

/// How long a 4 MiB payload may take before it is a path failure. Shorter than
/// [`TIMEOUT`] because such a command puts the guest's own buffer on the wire and the
/// write cannot be answered until every replica leg is done with it.
const HUGE_TIMEOUT: Duration = Duration::from_millis(250);

/// A peer link: an fd and the peer's id, no per-op state.
///
/// A link is per `(universe, peer)`, not per peer: a peer we share two universes with
/// publishes two namespaces and we hold two links. That is the partitioning enforcement
/// on the client side, since there is no way to phrase a frame for a universe you hold
/// no namespace of. `!Send`, like the [`Disk`] it wraps: the runtime registers the file
/// on every core, so a submission never crosses cores even though one `Link` serves them
/// all.
pub(crate) struct Link {
    disk: Disk,
    /// The same device on the shorter deadline a 4 MiB command is held to.
    huge: Disk,
    universe: u32,
    peer: u32,
}

impl Link {
    /// Open a link to `p` in `universe`; the control plane has already attached the
    /// peer's fabric namespace locally, so this is just an `open(2)`. Links are opened
    /// when a configuration is built and closed when it retires; re-declaring the same
    /// path across a reload keeps the registration, so a live peer's fd is never
    /// disturbed.
    pub(crate) fn open(c: &Configurator, universe: u32, p: &Peer) -> io::Result<Link> {
        let disk = c.disk(Path::new(&p.device), Some(TIMEOUT), None)?;
        let huge = disk.by(HUGE_TIMEOUT);
        Ok(Link {
            disk,
            huge,
            universe,
            peer: p.id,
        })
    }

    pub(crate) fn peer(&self) -> u32 {
        self.peer
    }

    /// The universe this namespace belongs to. Every frame sent here is in its address
    /// space, and no other.
    pub(crate) fn universe(&self) -> u32 {
        self.universe
    }

    /// Issue one command. This is the whole client API.
    ///
    /// `buf` is the payload, the trailer, or both; its length tells the target which, and
    /// the shape is checked against the command here, so a shape the opcode may not have
    /// never reaches the wire. Nothing is copied: `buf` is registered memory, which for a
    /// 4 MiB page is the guest's own pages. A 4 MiB transfer goes out as one command that
    /// the nvme layer splits at the peer's MDTS; the target sees the pieces as separate
    /// requests at consecutive LBAs inside the frame's footprint. There is no chunk index
    /// and no partial-failure case, because there is one completion.
    pub(crate) async fn send(&self, cmd: Cmd, buf: Buf) -> Result<(), Errno> {
        let lba = cmd.encode(buf.len())?;
        let off = lba * BLOCK as u64;
        if cmd.op().is_read() {
            self.disk.read(off, buf).await
        } else {
            // Durable: a fabric write is only acked once the peer has it, and the ublk
            // device advertises no volatile cache, so there is no flush to pair.
            let d = if cmd.frame().huge {
                &self.huge
            } else {
                &self.disk
            };
            d.write(off, buf, Durability::Durable).await
        }
    }

    /// Reissue a command that arrived here for someone else, on our own link.
    ///
    /// The command keeps its shape, so the one registered buffer it arrived in serves
    /// both hops. Refused when the op is not routable or its budget is spent: the
    /// originator has our placement wrong and belongs back at its config.
    pub(crate) async fn relay(&self, cmd: Cmd, buf: Buf) -> Result<(), Errno> {
        self.send(cmd.forwarded().ok_or(status::STALE)?, buf).await
    }
}

// --- Tests ---

/// The codec only: pure and total, so no device, no root and no peer. Those paths are
/// exercised end to end in `server`.
#[cfg(test)]
mod tests {
    use super::*;

    fn small(lba: u64) -> PageRef {
        PageRef::new(Class::Small, lba).unwrap()
    }

    fn huge(lba: u64) -> PageRef {
        PageRef::new(Class::Huge, lba).unwrap()
    }

    fn group(i: u32) -> GroupIx {
        GroupIx::new(i).unwrap()
    }

    /// Every command, at every shape it is allowed to have, with the transfer length
    /// each one implies.
    fn corpus() -> Vec<(Cmd, usize)> {
        let mut v = Vec::new();
        // Alternating bits, so a field landing at the wrong shift shows.
        let s = small(0x15_5555_5555);
        let h = huge(0x5_5555 * HUGE_BLOCKS);
        for hops in [Hops::NONE, Hops::ONE, Hops::TWO] {
            for to in [
                To::Owner,
                To::Member(Member::new(0).unwrap()),
                To::Member(Member::new(1).unwrap()),
                To::Member(Member::new(2).unwrap()),
            ] {
                let via = Via::new(to, hops);
                for page in [s, h] {
                    v.push((
                        Cmd::GetMeta {
                            page,
                            from: Source::Group(via),
                        },
                        BLOCK,
                    ));
                    v.push((Cmd::Prepare { page, via }, BLOCK));
                    v.push((Cmd::Trim { page, via }, BLOCK));
                    v.push((Cmd::Learn { page, via }, BLOCK));
                    v.push((Cmd::Seal { anchor: page, via }, BLOCK));
                }
                for want in [Want::Page, Want::Gather] {
                    let len = if want == Want::Gather {
                        2 * BLOCK
                    } else {
                        BLOCK
                    };
                    v.push((
                        Cmd::Get {
                            page: s,
                            from: Source::Group(via),
                            want,
                        },
                        len,
                    ));
                }
                v.push((
                    Cmd::Accept {
                        page: s,
                        via,
                        put: Put::Gather,
                    },
                    2 * BLOCK,
                ));
                // A 4 MiB command arrives in whatever pieces the peer's MDTS produced.
                for (block, blocks) in [(0usize, 1024usize), (0, 256), (256, 256), (1023, 1)] {
                    let off = block * BLOCK;
                    let len = blocks * BLOCK;
                    v.push((
                        Cmd::Get {
                            page: h,
                            from: Source::Group(via),
                            want: Want::Piece { off },
                        },
                        len,
                    ));
                    v.push((
                        Cmd::Accept {
                            page: h,
                            via,
                            put: Put::Piece { off },
                        },
                        len,
                    ));
                }
            }
        }
        for page in [s, h] {
            v.push((
                Cmd::GetMeta {
                    page,
                    from: Source::Cache,
                },
                BLOCK,
            ));
            v.push((Cmd::Warm { page }, BLOCK));
        }
        v.push((
            Cmd::Get {
                page: s,
                from: Source::Cache,
                want: Want::Gather,
            },
            2 * BLOCK,
        ));
        v.push((
            Cmd::Get {
                page: h,
                from: Source::Cache,
                want: Want::Piece { off: 0 },
            },
            1024 * BLOCK,
        ));
        for class in [Class::Small, Class::Huge] {
            v.push((
                Cmd::Merkle {
                    group: group(0x555_5555),
                    class,
                },
                BLOCK,
            ));
            v.push((
                Cmd::SnapOpen {
                    group: group(0x155_5555),
                    class,
                    bucket: None,
                },
                BLOCK,
            ));
            for b in [0u16, 1, 511] {
                v.push((
                    Cmd::SnapOpen {
                        group: group(0x155_5555),
                        class,
                        bucket: Some(Bucket::new(b).unwrap()),
                    },
                    BLOCK,
                ));
            }
        }
        for seq in [0u8, 1, 63] {
            v.push((
                Cmd::SnapNext {
                    cursor: Cursor::new(0x5555_5555),
                    seq: Seq::new(seq).unwrap(),
                },
                BLOCK,
            ));
        }
        v.push((
            Cmd::Term {
                group: group(0x555_5555),
            },
            BLOCK,
        ));
        v.push((Cmd::Ping, BLOCK));
        v
    }

    #[test]
    fn round_trips_every_command() {
        for (c, len) in corpus() {
            let lba = c.encode(len).unwrap();
            let d = Cmd::decode(lba, len, c.op().is_read()).unwrap();
            assert_eq!(c, d, "{c:?} at {len}");
        }
    }

    /// Direction is the opcode's alone, and a frame issued the wrong way is refused
    /// before it can reach a handler.
    #[test]
    fn direction_follows_the_opcode() {
        for (c, len) in corpus() {
            let lba = c.encode(len).unwrap();
            assert_eq!(
                Cmd::decode(lba, len, !c.op().is_read()),
                Err(status::BAD),
                "{c:?}"
            );
        }
        assert!(Op::Get.is_read() && Op::Ping.is_read());
        assert!(!Op::Accept.is_read() && !Op::Trim.is_read() && !Op::Seal.is_read());
    }

    /// A shape an op may not have never leaves the client, and never survives decoding.
    #[test]
    fn refuses_shapes_an_op_may_not_have() {
        let s = small(7);
        let h = huge(9 * HUGE_BLOCKS);
        let via = Via::direct(To::Owner);
        let bare = Cmd::Get {
            page: s,
            from: Source::Group(via),
            want: Want::Page,
        };
        // A bare read is one block; the trailer half of a gather frame is not addressable.
        assert_eq!(bare.encode(2 * BLOCK), Err(status::BAD));
        assert_eq!(
            Cmd::decode(bare.encode(BLOCK).unwrap() + 1, BLOCK, true),
            Err(status::BAD)
        );
        assert_eq!(
            Cmd::decode(bare.encode(BLOCK).unwrap(), 3 * BLOCK, true),
            Err(status::BAD)
        );
        // Class and shape must agree in both directions.
        assert_eq!(
            Cmd::Get {
                page: h,
                from: Source::Group(via),
                want: Want::Gather,
            }
            .encode(2 * BLOCK),
            Err(status::BAD)
        );
        assert_eq!(
            Cmd::Get {
                page: s,
                from: Source::Group(via),
                want: Want::Piece { off: 0 },
            }
            .encode(BLOCK),
            Err(status::BAD)
        );
        assert_eq!(
            Cmd::Accept {
                page: h,
                via,
                put: Put::Gather,
            }
            .encode(2 * BLOCK),
            Err(status::BAD)
        );
        // A piece is block-aligned and never runs past the end of the page.
        let piece = |off, len| {
            Cmd::Get {
                page: h,
                from: Source::Group(via),
                want: Want::Piece { off },
            }
            .encode(len)
        };
        assert!(piece(BLOCK, 1023 * BLOCK).is_ok());
        assert_eq!(piece(BLOCK, 1024 * BLOCK), Err(status::BAD));
        assert_eq!(piece(1, BLOCK), Err(status::BAD));
        assert_eq!(piece(0, 0), Err(status::BAD));
        assert_eq!(piece(0, BLOCK - 1), Err(status::BAD));
        assert_eq!(piece(usize::MAX, BLOCK), Err(status::BAD));
        // A control op is exactly one trailer block, at either class.
        let trim = Cmd::Trim { page: h, via };
        assert!(trim.encode(BLOCK).is_ok());
        assert_eq!(trim.encode(2 * BLOCK), Err(status::BAD));
        assert_eq!(Cmd::Ping.encode(2 * BLOCK), Err(status::BAD));
        assert_eq!(
            Cmd::decode(Cmd::Ping.encode(BLOCK).unwrap(), 2 * BLOCK, true),
            Err(status::BAD)
        );
    }

    /// A field that cannot mean anything is not representable, so a peer cannot make one
    /// by hand either.
    #[test]
    fn bounds_are_types() {
        assert!(Member::new(3).is_none());
        assert!(Bucket::new(BUCKETS as u16).is_none());
        assert!(Seq::new(64).is_none());
        assert_eq!(Seq::wrap(64).get(), 0);
        assert_eq!(Seq::wrap(65).get(), 1);
        assert_eq!(Seq::wrap(255).get(), 63);
        assert!(GroupIx::new(GroupIx::MAX).is_none());
        assert!(GroupIx::new(GroupIx::MAX - 1).is_some());
        assert!(State::new(3).is_none());
        // A 4 MiB page names the first of its 1024 blocks and nothing between.
        assert!(PageRef::new(Class::Huge, HUGE_BLOCKS).is_some());
        assert!(PageRef::new(Class::Huge, 1).is_none());
        assert!(PageRef::new(Class::Small, crate::config::MAX_LBA).is_none());
        assert!(PageRef::new(Class::Huge, crate::config::MAX_LBA).is_none());
    }

    /// Every address the control plane may hand out is nameable in both classes, which
    /// lets an extent sit anywhere in its universe's space.
    #[test]
    fn a_command_reaches_the_whole_universe() {
        let via = Via::direct(To::Owner);
        let last = crate::config::MAX_LBA - 1;
        let c = Cmd::Trim {
            page: small(last),
            via,
        };
        let d = Cmd::decode(c.encode(BLOCK).unwrap(), BLOCK, false).unwrap();
        assert_eq!(d.routing().unwrap().0.lba(), last);

        let last_huge = crate::config::MAX_LBA - HUGE_BLOCKS;
        let g = Cmd::Trim {
            page: huge(last_huge),
            via,
        };
        let d = Cmd::decode(g.encode(BLOCK).unwrap(), BLOCK, false).unwrap();
        assert_eq!(d.routing().unwrap().0.lba(), last_huge);
    }

    #[test]
    fn regions_do_not_overlap() {
        let via = Via::new(To::Member(Member::new(2).unwrap()), Hops::TWO);
        let s = Cmd::Trim {
            page: small(crate::config::MAX_LBA - 1),
            via,
        }
        .encode(BLOCK)
        .unwrap();
        let h = Cmd::Trim {
            page: huge(crate::config::MAX_LBA - HUGE_BLOCKS),
            via,
        }
        .encode(BLOCK)
        .unwrap();
        assert!(s < HUGE_BASE_LBA);
        assert!(h >= HUGE_BASE_LBA);
        assert!(h + HUGE_BLOCKS <= MAX_LBA);
        assert_eq!(DEVICE_SIZE, 4 << 60);
        // The last block's byte offset still fits `loff_t`.
        assert!(DEVICE_SIZE < i64::MAX as u64);
    }

    #[test]
    fn decode_is_total() {
        // Anything at all is either a command or BAD, never a panic: the target runs this
        // on bytes a peer chose.
        let mut x = 0x9E37_79B9_7F4A_7C15u64;
        for i in 0..200_000u64 {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            let len = (i as usize % 2050) * BLOCK;
            for read in [false, true] {
                let _ = Cmd::decode(x, len, read);
                let _ = Cmd::decode(x >> (i % 64), len, read);
                let _ = Cmd::decode(i, len, read);
            }
        }
        assert!(Cmd::decode(u64::MAX, BLOCK, true).is_err());
        assert!(Cmd::decode(0, 0, true).is_err());
        assert!(Cmd::decode(0, 4095, true).is_err());
        assert!(Cmd::decode(0, usize::MAX, true).is_err());
    }

    #[test]
    fn unknown_opcode_is_refused() {
        // Opcodes 13..31 are unassigned; a peer running a newer build must not be able
        // to make us do something by accident.
        for raw in 13..32u64 {
            let sh = SMALL_OFF_BITS + IMM_BITS + FLAG_BITS;
            for read in [false, true] {
                assert_eq!(
                    Cmd::decode((raw << sh) << SMALL_SHIFT, BLOCK, read),
                    Err(status::BAD)
                );
            }
        }
    }

    /// The bits an op does not define must be clear. A peer that sets one is asking for
    /// something we do not implement, which is exactly [`status::BAD`].
    #[test]
    fn reserved_header_bits_are_refused() {
        let raw = |op: Op, huge: bool, flags: u8, imm: u8, offset: u64| {
            Cmd::decode(
                Frame {
                    op,
                    flags,
                    imm,
                    huge,
                    offset,
                }
                .encode(),
                BLOCK,
                op.is_read(),
            )
        };
        // Bit 2 outside a cache read is undefined; a cache read carries no route.
        assert_eq!(raw(Op::Trim, false, CACHE_ONLY, 0, 4), Err(status::BAD));
        assert_eq!(raw(Op::Prepare, false, CACHE_ONLY, 0, 4), Err(status::BAD));
        assert_eq!(
            raw(Op::GetMeta, false, CACHE_ONLY | 1, 0, 4),
            Err(status::BAD)
        );
        assert_eq!(raw(Op::GetMeta, false, CACHE_ONLY, 1, 4), Err(status::BAD));
        assert!(raw(Op::GetMeta, false, CACHE_ONLY, 0, 4).is_ok());
        // A warm and the group-addressed ops are never routed and never addressed.
        assert_eq!(raw(Op::Warm, false, 1, 0, 4), Err(status::BAD));
        assert_eq!(raw(Op::Warm, false, 0, 1, 4), Err(status::BAD));
        assert!(raw(Op::Warm, true, 0, 0, 4).is_ok());
        for op in [Op::Term, Op::SnapNext, Op::Ping] {
            assert_eq!(raw(op, false, 1, 0, 0), Err(status::BAD));
            assert_eq!(raw(op, false, 0, 1, 0), Err(status::BAD));
            assert_eq!(raw(op, true, 0, 0, 0), Err(status::BAD));
        }
        for op in [Op::Merkle, Op::SnapOpen] {
            assert_eq!(raw(op, false, 1, 0, 0), Err(status::BAD));
            assert_eq!(raw(op, false, 0, 2, 0), Err(status::BAD));
            assert_eq!(raw(op, true, 0, 0, 0), Err(status::BAD));
        }
        // A PING names nothing, and an unfiltered SNAPOPEN names no bucket.
        assert_eq!(raw(Op::Ping, false, 0, 0, 1), Err(status::BAD));
        assert_eq!(raw(Op::SnapOpen, false, 0, 0, 1), Err(status::BAD));
        assert!(raw(Op::SnapOpen, false, 0, 0, 1 << 9 | 1).is_ok());
        // A group index that would be truncated is refused, not narrowed.
        assert_eq!(raw(Op::Term, false, 0, 0, 1 << 32), Err(status::BAD));
        assert_eq!(
            raw(Op::Merkle, false, 0, 0, GroupIx::MAX as u64),
            Err(status::BAD)
        );
    }

    #[test]
    fn forwarding_preserves_the_request() {
        // A forward changes only how far the command may still go, so it works in both
        // directions and at either shape.
        let c = Cmd::GetMeta {
            page: small(9),
            from: Source::Group(Via::new(To::Member(Member::new(1).unwrap()), Hops::TWO)),
        };
        let once = c.forwarded().unwrap();
        let twice = once.forwarded().unwrap();
        assert_eq!(twice.forwarded(), None);
        for (d, hops) in [(once, Hops::ONE), (twice, Hops::NONE)] {
            let (page, via) = d.routing().unwrap();
            assert_eq!(page.lba(), 9);
            assert_eq!(via.to, To::Member(Member::new(1).unwrap()));
            assert_eq!(via.hops, hops);
            assert_eq!(
                Cmd::decode(d.encode(BLOCK).unwrap(), BLOCK, true).unwrap(),
                d
            );
        }
        // Nothing that is not routed can be forwarded at all.
        assert_eq!(Cmd::Ping.forwarded(), None);
        assert_eq!(Cmd::Warm { page: small(9) }.forwarded(), None);
        assert_eq!(
            Cmd::Get {
                page: small(9),
                from: Source::Cache,
                want: Want::Gather,
            }
            .forwarded(),
            None
        );
        assert_eq!(Cmd::Term { group: group(1) }.forwarded(), None);
    }

    /// Only the ops a relay may carry have a route, and it is the page they route on.
    #[test]
    fn routing_is_op_specific() {
        let page = small(11);
        let via = Via::direct(To::Owner);
        for c in [
            Cmd::Prepare { page, via },
            Cmd::Trim { page, via },
            Cmd::Learn { page, via },
            Cmd::Seal { anchor: page, via },
            Cmd::Get {
                page,
                from: Source::Group(via),
                want: Want::Page,
            },
        ] {
            assert_eq!(c.routing(), Some((page, via)), "{c:?}");
        }
        for c in [
            Cmd::Ping,
            Cmd::Warm { page },
            Cmd::Term { group: group(0) },
            Cmd::Merkle {
                group: group(0),
                class: Class::Small,
            },
            Cmd::GetMeta {
                page,
                from: Source::Cache,
            },
        ] {
            assert_eq!(c.routing(), None, "{c:?}");
        }
    }

    // --- trailers ---

    fn reg() -> Reg {
        Reg {
            version: 0x0123_4567_89ab_cdef,
            ballot: 0xdead_beef,
        }
    }

    /// A trailer is exactly one block, whichever record it holds.
    #[test]
    fn trailers_are_one_block() {
        let mut t = [0u8; BLOCK];
        assert!(TermReply { term: 1 }.encode(&mut t).is_ok());
        assert_eq!(TermReply { term: 1 }.encode(&mut t[..8]), Err(status::BAD));
        assert_eq!(TermReply::decode(&t[..8]), Err(status::BAD));
        assert_eq!(TermReply::decode(&[]), Err(status::BAD));
        let mut big = [0u8; 2 * BLOCK];
        assert_eq!(TermReply { term: 1 }.encode(&mut big), Err(status::BAD));
        assert_eq!(TermReply::decode(&big), Err(status::BAD));
    }

    /// Every record survives its own codec, and an encoder leaves nothing of what was
    /// in the buffer before.
    #[test]
    fn every_trailer_round_trips() {
        let mut t = [0u8; BLOCK];

        macro_rules! trip {
            ($v:expr, $ty:ty) => {{
                let v = $v;
                t.fill(0xa5);
                v.encode(&mut t).unwrap();
                assert_eq!(<$ty>::decode(&t).unwrap(), v);
            }};
        }

        trip!(
            MetaReply {
                reg: reg(),
                state: State::new(2).unwrap(),
                width: 3,
            },
            MetaReply
        );
        trip!(MetaReply::default(), MetaReply);
        trip!(
            PrepareReply {
                reg: reg(),
                term: 77,
            },
            PrepareReply
        );
        trip!(
            AcceptReq {
                guard: Guard::At(5),
                ballot: 9,
                epoch: 4,
            },
            AcceptReq
        );
        trip!(
            AcceptReq {
                guard: Guard::Derived,
                ballot: 9,
                epoch: 4,
            },
            AcceptReq
        );
        trip!(
            TrimReq {
                guard: 5,
                ballot: 9,
                epoch: 4,
            },
            TrimReq
        );
        trip!(
            LearnReq {
                reg: reg(),
                from: Member::new(2).unwrap(),
                repair: true,
            },
            LearnReq
        );
        trip!(
            LearnReq {
                reg: reg(),
                from: Member::new(0).unwrap(),
                repair: false,
            },
            LearnReq
        );
        trip!(
            WarmReq {
                version: 12,
                stage: Stage::Inbound,
            },
            WarmReq
        );
        trip!(
            WarmReq {
                version: 12,
                stage: Stage::Holder,
            },
            WarmReq
        );
        trip!(
            SealReq {
                term: 3,
                extent: 88,
            },
            SealReq
        );
        trip!(TermReply { term: 41 }, TermReply);
        trip!(
            PingReply {
                node: 6,
                generation: 7,
                epoch: 8,
            },
            PingReply
        );
        trip!(
            SnapOpenReply {
                cursor: Cursor::new(0xfeed)
            },
            SnapOpenReply
        );

        let mut digests = Box::new([0u64; BUCKETS]);
        for (i, d) in digests.iter_mut().enumerate() {
            *d = i as u64 * 0x9E37_79B9;
        }
        let m = MerkleReply(digests);
        t.fill(0xa5);
        m.encode(&mut t).unwrap();
        assert_eq!(MerkleReply::decode(&t).unwrap().0, m.0);

        for n in [0usize, 1, SnapNextReply::CAPACITY] {
            for done in [false, true] {
                let tuples: Vec<_> = (0..n)
                    .map(|i| SnapTuple {
                        addr: i as u64 * 4096,
                        reg: Reg {
                            version: i as u64,
                            ballot: i as u32,
                        },
                    })
                    .collect();
                let r = SnapNextReply::new(done, tuples).unwrap();
                t.fill(0xa5);
                r.encode(&mut t).unwrap();
                assert_eq!(SnapNextReply::decode(&t).unwrap(), r);
            }
        }
        assert!(
            SnapNextReply::new(
                false,
                vec![
                    SnapTuple {
                        addr: 0,
                        reg: reg()
                    };
                    TUPLES + 1
                ]
            )
            .is_none()
        );
    }

    /// A record decodes only what it defines: a slot past its end, a field that does not
    /// fit its type, and a flag that is neither zero nor one are all refused.
    #[test]
    fn trailers_refuse_malformed_records() {
        let put = |t: &mut [u8], i: usize, v: u64| {
            t[i * 8..i * 8 + 8].copy_from_slice(&v.to_le_bytes());
        };
        let mut t = [0u8; BLOCK];

        // Padding a record does not define.
        MetaReply::default().encode(&mut t).unwrap();
        assert!(MetaReply::decode(&t).is_ok());
        put(&mut t, 4, 1);
        assert_eq!(MetaReply::decode(&t), Err(status::BAD));
        put(&mut t, 4, 0);
        put(&mut t, BUCKETS - 1, 1);
        assert_eq!(MetaReply::decode(&t), Err(status::BAD));

        // A ballot is 32 bits and a width is 8; neither is narrowed silently.
        MetaReply::default().encode(&mut t).unwrap();
        put(&mut t, 1, 1 << 32);
        assert_eq!(MetaReply::decode(&t), Err(status::BAD));
        MetaReply::default().encode(&mut t).unwrap();
        put(&mut t, 3, 256);
        assert_eq!(MetaReply::decode(&t), Err(status::BAD));
        // And a state outside 0..3 means nothing.
        MetaReply::default().encode(&mut t).unwrap();
        put(&mut t, 2, 3);
        assert_eq!(MetaReply::decode(&t), Err(status::BAD));

        // A member index names one of three.
        LearnReq {
            reg: reg(),
            from: Member::new(0).unwrap(),
            repair: false,
        }
        .encode(&mut t)
        .unwrap();
        put(&mut t, 2, 3);
        assert_eq!(LearnReq::decode(&t), Err(status::BAD));
        // A flag is zero or one, not "nonzero".
        LearnReq {
            reg: reg(),
            from: Member::new(0).unwrap(),
            repair: false,
        }
        .encode(&mut t)
        .unwrap();
        put(&mut t, 3, 2);
        assert_eq!(LearnReq::decode(&t), Err(status::BAD));

        // A stage names one of two.
        WarmReq {
            version: 1,
            stage: Stage::Inbound,
        }
        .encode(&mut t)
        .unwrap();
        put(&mut t, 2, 2);
        assert_eq!(WarmReq::decode(&t), Err(status::BAD));
        // WARM and SEAL and TERM each leave a slot clear, and it must stay clear.
        WarmReq {
            version: 1,
            stage: Stage::Inbound,
        }
        .encode(&mut t)
        .unwrap();
        put(&mut t, 1, 1);
        assert_eq!(WarmReq::decode(&t), Err(status::BAD));
        SealReq { term: 1, extent: 2 }.encode(&mut t).unwrap();
        put(&mut t, 0, 1);
        assert_eq!(SealReq::decode(&t), Err(status::BAD));
        TermReply { term: 1 }.encode(&mut t).unwrap();
        put(&mut t, 0, 1);
        assert_eq!(TermReply::decode(&t), Err(status::BAD));

        // A snapshot reply never claims more pages than fit.
        SnapNextReply::new(true, vec![])
            .unwrap()
            .encode(&mut t)
            .unwrap();
        put(&mut t, 0, TUPLES as u64 + 1);
        assert_eq!(SnapNextReply::decode(&t), Err(status::BAD));
        SnapNextReply::new(true, vec![])
            .unwrap()
            .encode(&mut t)
            .unwrap();
        put(&mut t, 1, 2);
        assert_eq!(SnapNextReply::decode(&t), Err(status::BAD));

        // The guard sentinel is the only way to say "derive it", so it cannot also be a
        // version a proposer names.
        assert_eq!(
            AcceptReq {
                guard: Guard::At(u64::MAX),
                ballot: 0,
                epoch: 0,
            }
            .encode(&mut t),
            Err(status::BAD)
        );
    }

    /// Decoding a record never panics, whatever a peer put in the block.
    #[test]
    fn trailer_decode_is_total() {
        let mut x = 0x243F_6A88_85A3_08D3u64;
        let mut t = [0u8; BLOCK];
        for _ in 0..2_000 {
            for c in t.chunks_exact_mut(8) {
                x ^= x << 13;
                x ^= x >> 7;
                x ^= x << 17;
                c.copy_from_slice(&x.to_le_bytes());
            }
            let _ = MetaReply::decode(&t);
            let _ = PrepareReply::decode(&t);
            let _ = AcceptReq::decode(&t);
            let _ = TrimReq::decode(&t);
            let _ = LearnReq::decode(&t);
            let _ = WarmReq::decode(&t);
            let _ = SealReq::decode(&t);
            let _ = TermReply::decode(&t);
            let _ = PingReply::decode(&t);
            let _ = MerkleReply::decode(&t);
            let _ = SnapOpenReply::decode(&t);
            let _ = SnapNextReply::decode(&t);
        }
    }
}
