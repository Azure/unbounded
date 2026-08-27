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
//! 1. One address region with a two-block stride, because `imm` sits inside the frame id
//!    and a frame is at most a 4 KiB block and its 4 KiB trailer.
//! 2. 48-bit frame ids: id times stride is a byte offset and must fit `loff_t`.
//! 3. Four statuses; nvmet erases the rest to `EIO`. See [`status`].
//! 4. Relaying is a flag ([`HOPS`]), not an opcode: a read carries no data to the target,
//!    so an inner frame cannot ride an outer one's trailer.
//! 5. Gather copies the 4 KiB payload: `ReadvFixed`/`WritevFixed` take a single
//!    `buf_index`, so a guest ublk page and a pooled trailer cannot share one vectored
//!    fixed SQE.
//! 6. Direction follows the opcode and shape follows the length, so the client API is
//!    [`Link::send`] plus [`Cmd::decode`]. Both speak [`Cmd`]: the bit packing is private,
//!    so no caller can name a field its opcode does not have.

pub(crate) use crate::layout::Class;
use crate::runtime::Errno;

mod footer;
mod link;

pub(crate) use footer::{
    AcceptReq, Footer, Guard, LearnReq, MerkleReply, MetaReply, PingReply, PrepareReply, Reg,
    SealReq, SnapNextReply, SnapOpenReply, SnapTuple, Stage, State, TermReply, TrimReq, WarmReq,
};
pub(crate) use link::Link;

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
/// | [`status::STALE`]   | `LBA_RANGE`      | `EREMOTEIO`   | yes |
/// | [`status::MISSING`] | `ACCESS_DENIED`  | `ENODATA`     | **no** |
/// | [`status::BAD`]     | `INVALID_OPCODE` | `EOPNOTSUPP`  | yes |
/// | [`status::NOSPC`]   | `CAP_EXCEEDED`   | `ENOSPC`      | yes |
///
/// A status without `NVME_STATUS_DNR` is retried by the initiator up to
/// `nvme_max_retries` (5) before delivery, per `nvme_decide_disposition()`. Only
/// [`status::MISSING`] lacks it, which is safe because every op is replay-safe, and is why a
/// lost ballot is not reported as [`status::MISSING`]: consensus would pay six round trips.
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
///
/// The numbers start at 16, not 0. The page model moved to one 4 KiB block per register
/// and placement changed with it, so a node of the previous protocol and a node of this
/// one must never agree on a frame. The old encoding used 0..12 and rejected everything
/// above; this one uses 16..28 and rejects everything below. A mixed deployment loses
/// availability instead of forming a quorum under two different placements.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub(crate) enum Op {
    /// Read a block. Reply *is* the payload.
    Get = 16,
    /// Read the register: version, ballot, state, width hint. The hedged-read path.
    GetMeta = 17,
    /// Raise this group's promise and report `(version, ballot, term)`. A read.
    Prepare = 18,
    /// Write a page under a ballot. Reply is a status.
    Accept = 19,
    /// Delete an immutable block.
    Trim = 20,
    /// Open a snapshot cursor, optionally filtered to one digest bucket.
    SnapOpen = 21,
    /// Advance a snapshot cursor. The cursor is explicit, so the stream is stateless.
    SnapNext = 22,
    /// Bucket digests for anti-entropy.
    Merkle = 23,
    /// Tell a peer its register is stale: it pulls the page and applies it if newer.
    /// Used by repair and by the migration learn.
    Learn = 24,
    /// Freeze a shard at its source group.
    Seal = 25,
    /// Liveness and geometry.
    Ping = 26,
    /// A group's standing promise. Names a group, not a page.
    Term = 27,
    /// Tell another zone that a page it keeps warm has a new value. Advisory both ways:
    /// the sender does not wait and the receiver may decline.
    Warm = 28,
}

impl Op {
    fn from_bits(b: u8) -> Option<Op> {
        Some(match b {
            16 => Op::Get,
            17 => Op::GetMeta,
            18 => Op::Prepare,
            19 => Op::Accept,
            20 => Op::Trim,
            21 => Op::SnapOpen,
            22 => Op::SnapNext,
            23 => Op::Merkle,
            24 => Op::Learn,
            25 => Op::Seal,
            26 => Op::Ping,
            27 => Op::Term,
            28 => Op::Warm,
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
    /// because a repair names a value and its holder rather than carrying it: the
    /// receiver pulls the block with an ordinary `GET`.
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
/// A modifier on `GET` and `GETMETA`: a reader that believes a cohort peer holds a
/// replica asks it directly, concurrently with the mandatory metadata round. A miss or a
/// shedding replica answers [`status::MISSING`] and the reader falls back to the group,
/// so declining is always safe. The reply puts the register the copy claims in the
/// trailer beside the block. It carries no width hint, because a replica does not own
/// the sketch.
const CACHE_ONLY: u8 = 1 << 2;

// --- wire format ---

// One region with a two-block stride, so a frame is a block and its optional trailer:
//
//   lba = frame * 2      frame < 2^48   block 0 payload, block 1 trailer
//
// A frame id packs, from the low bit up:
//
//   | offset 38 | imm 2 | flags 3 | opcode 5 |
//
// There is no address-space field: the namespace a frame arrived on is the universe. The
// control plane publishes one device per universe and attaches it only to that universe's
// members, so partitioning is enforced by the transport rather than by a number a peer
// could choose, which is what makes a universe a security boundary.
//
// `offset` is a block index in the universe's own flat LBA space. It addresses exactly
// `config::MAX_LBA` blocks, so any address the control plane may legally hand out is
// reachable, whatever kind of extent covers it.
const OP_BITS: u32 = 5;
const FLAG_BITS: u32 = 3;
const IMM_BITS: u32 = 2;
const OFF_BITS: u32 = 38;

/// A frame is two blocks wide: payload and trailer.
const FRAME_SHIFT: u32 = 1;

const MAX_LBA: u64 = 1 << (OFF_BITS + IMM_BITS + FLAG_BITS + OP_BITS + FRAME_SHIFT);

/// Size the fabric device must be declared with: sparse address space, never storage.
pub(crate) const DEVICE_SIZE: u64 = MAX_LBA * BLOCK as u64;

/// Tag bit separating the two kinds of device key the runtime sees.
///
/// A client device is keyed by its own 32-bit id and a fabric device by its universe, so
/// one bit above both keeps the two flat and disjoint. Private, like the rest of the bit
/// packing here: [`key`] and [`universe_of`] are the whole vocabulary, so no caller can
/// mistake one kind of key for the other.
const KEY_TAG: u64 = 1 << 32;

/// The runtime device key of a universe's fabric device.
pub(crate) fn key(universe: u32) -> u64 {
    KEY_TAG | universe as u64
}

/// The universe a request arrived for, or `None` when `key` names a client device.
pub(crate) fn universe_of(key: u64) -> Option<u32> {
    (key & KEY_TAG != 0).then_some(key as u32)
}

/// The logical block, and so the trailer size and the page size.
pub(crate) const BLOCK: usize = 4096;

/// Slots in one trailer.
const SLOTS: usize = BLOCK / 8;

/// Digest buckets a group's address space is cut into, and so the width of a `MERKLE`
/// reply. A digest vector fills the block exactly, which is why anti-entropy is 512 wide
/// and one level deep instead of a tree.
pub(crate) const BUCKETS: usize = SLOTS;

/// Pages one `SNAPNEXT` reply can carry: three slots each after a two-slot header.
const TUPLES: usize = (SLOTS - 2) / 3;

// The region covers a whole universe, so config validation's `base_lba + blocks <=
// MAX_LBA` is the only bound a page address needs; the fabric adds none of its own.
const _: () = assert!(1u64 << OFF_BITS == crate::config::MAX_LBA);

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
    /// Bare mode: page bytes only.
    Payload,
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
    /// three wide. The group-addressed ops borrow it for the slab class instead; see
    /// `heal.rs`. A receiver that is not the addressee forwards, if [`HOPS`] allows.
    imm: u8,
    /// Block index in the universe for a page op; op-specific for the group-addressed
    /// ops, which name no page at all.
    offset: u64,
}

impl Frame {
    /// The block this frame names, in the universe's address space. Meaningless on a
    /// group-addressed frame.
    fn lba(&self) -> u64 {
        self.offset
    }

    /// The frame's base LBA, block 0 of its footprint. Total: out-of-range fields are
    /// masked, not rejected, because every caller has already validated its address
    /// against the config and a masked frame just fails its bounds check at the far end.
    fn encode(&self) -> u64 {
        let mut id = self.offset & mask(OFF_BITS);
        id |= (self.imm as u64 & mask(IMM_BITS)) << OFF_BITS;
        id |= (self.flags as u64 & mask(FLAG_BITS)) << (OFF_BITS + IMM_BITS);
        id |= (self.op as u64) << (OFF_BITS + IMM_BITS + FLAG_BITS);
        id << FRAME_SHIFT
    }

    /// The inverse, plus the frame shape implied by the transfer length. Pure and total:
    /// any `(lba, len)` either decodes or is [`status::BAD`], and none panics. The target
    /// runs this on bytes a peer chose, so totality is a safety property here.
    fn decode(lba: u64, len: usize) -> Result<(Frame, Part), Errno> {
        if lba >= MAX_LBA || len == 0 || !len.is_multiple_of(BLOCK) {
            return Err(status::BAD);
        }
        let blocks = (len / BLOCK) as u64;
        let id = lba >> FRAME_SHIFT;
        let block = lba & 1;
        let flag_sh = OFF_BITS + IMM_BITS;
        let op_sh = flag_sh + FLAG_BITS;
        let op = Op::from_bits(((id >> op_sh) & mask(OP_BITS)) as u8).ok_or(status::BAD)?;
        let f = Frame {
            op,
            flags: ((id >> flag_sh) & mask(FLAG_BITS)) as u8,
            imm: ((id >> OFF_BITS) & mask(IMM_BITS)) as u8,
            offset: id & mask(OFF_BITS),
        };
        let part = if op.is_control() {
            if block != 0 || blocks != 1 {
                return Err(status::BAD);
            }
            Part::Trailer
        } else {
            match (block, blocks) {
                (0, 1) => Part::Payload,
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
    /// The addressee as it travels: zero for the owner, `k + 1` for member `k`.
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

/// A block named in the arriving universe's address space.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct PageRef {
    /// The block index in the universe, already range-checked.
    index: u64,
}

impl PageRef {
    /// `lba` counts 4 KiB blocks, the unit the control plane places extents in and the
    /// unit every register names. `None` when the address is outside the region.
    pub(crate) fn new(lba: u64) -> Option<PageRef> {
        (lba < 1 << OFF_BITS).then_some(PageRef { index: lba })
    }

    /// The block this reference names, the inverse of [`PageRef::new`].
    pub(crate) fn lba(self) -> u64 {
        self.index
    }
}

/// A consensus group index in the arriving universe's catalog.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct GroupIx(u32);

impl GroupIx {
    /// Bounded by the narrowest field any group op has for it, `SNAPOPEN`'s.
    pub(crate) const MAX: u32 = 1 << (OFF_BITS - 10);

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
    /// distinguishable from its neighbors.
    pub(crate) fn wrap(n: u8) -> Seq {
        Seq(n & Seq::MASK as u8)
    }

    pub(crate) fn get(self) -> u8 {
        self.0
    }
}

// --- commands ---

/// Which blocks of a frame a `GET` asks for.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Want {
    /// A bare block: payload only, no trailer.
    Page,
    /// A block and its register in one command.
    Gather,
}

/// One fabric operation, with exactly the fields its opcode may carry and no others.
///
/// This is the only way to phrase or read a frame. The bit packing below is private, so
/// an illegal combination of opcode, addressee, budget and transfer shape cannot be
/// built, sent, or survive [`Cmd::decode`].
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
    /// Write a page under a ballot. Always a gather: the page and its `AcceptReq`.
    Accept { page: PageRef, via: Via },
    /// Delete an immutable block.
    Trim { page: PageRef, via: Via },
    /// Tell a peer its register is stale.
    Learn { page: PageRef, via: Via },
    /// Freeze a shard at its source group. The frame names a page only so the request
    /// routes; the shard it seals is in the trailer.
    Seal { anchor: PageRef, via: Via },
    /// Tell another zone a page it keeps warm has a new value. Advisory, never relayed.
    Warm { page: PageRef },
    /// Bucket digests for anti-entropy. The class names a slab, not a page width.
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
    fn addressed(page: PageRef, via: Via) -> (u8, u8, u64) {
        (via.hops.get(), via.to.imm(), page.index)
    }

    /// The frame this command packs into, before its transfer shape is checked.
    fn frame(self) -> Frame {
        let (op, flags, imm, offset) = match self {
            Cmd::Get { page, from, .. } => {
                let (flags, imm) = match from {
                    Source::Group(via) => {
                        let (f, i, _) = Cmd::addressed(page, via);
                        (f, i)
                    }
                    Source::Cache => (CACHE_ONLY, 0),
                };
                (Op::Get, flags, imm, page.index)
            }
            Cmd::GetMeta { page, from } => {
                let (flags, imm) = match from {
                    Source::Group(via) => {
                        let (f, i, _) = Cmd::addressed(page, via);
                        (f, i)
                    }
                    Source::Cache => (CACHE_ONLY, 0),
                };
                (Op::GetMeta, flags, imm, page.index)
            }
            Cmd::Prepare { page, via } => {
                let (f, i, o) = Cmd::addressed(page, via);
                (Op::Prepare, f, i, o)
            }
            Cmd::Accept { page, via } => {
                let (f, i, o) = Cmd::addressed(page, via);
                (Op::Accept, f, i, o)
            }
            Cmd::Trim { page, via } => {
                let (f, i, o) = Cmd::addressed(page, via);
                (Op::Trim, f, i, o)
            }
            Cmd::Learn { page, via } => {
                let (f, i, o) = Cmd::addressed(page, via);
                (Op::Learn, f, i, o)
            }
            Cmd::Seal { anchor, via } => {
                let (f, i, o) = Cmd::addressed(anchor, via);
                (Op::Seal, f, i, o)
            }
            Cmd::Warm { page } => (Op::Warm, 0, 0, page.index),
            Cmd::Merkle { group, class } => (
                Op::Merkle,
                0,
                (class == Class::Immutable) as u8,
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
                (Op::SnapOpen, 0, (class == Class::Immutable) as u8, off)
            }
            Cmd::SnapNext { cursor, seq } => (
                Op::SnapNext,
                0,
                0,
                (cursor.get() as u64) << Seq::BITS | seq.get() as u64,
            ),
            Cmd::Term { group } => (Op::Term, 0, 0, group.get() as u64),
            Cmd::Ping => (Op::Ping, 0, 0, 0),
        };
        Frame {
            op,
            flags,
            imm,
            offset,
        }
    }

    /// Where in the frame's footprint a transfer of `len` bytes starts, and whether that
    /// shape is one this command is allowed to have.
    fn block(self, len: usize) -> Result<u64, Errno> {
        match self {
            Cmd::Get { want, .. } => match want {
                Want::Page if len == BLOCK => Ok(0),
                Want::Gather if len == 2 * BLOCK => Ok(0),
                _ => Err(status::BAD),
            },
            Cmd::Accept { .. } if len == 2 * BLOCK => Ok(0),
            Cmd::Accept { .. } => Err(status::BAD),
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
        let page = || PageRef::new(f.lba()).ok_or(status::BAD);
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
        // An advisory warm is never routed: it names a block and nothing else.
        let bare =
            || -> Result<(), Errno> { (f.flags == 0 && f.imm == 0).then_some(()).ok_or(status::BAD) };
        // The group-addressed ops carry no route and no page; they borrow `imm` for the
        // slab class or leave it clear.
        let plain = || -> Result<(), Errno> { (f.flags == 0 && f.imm == 0).then_some(()).ok_or(status::BAD) };
        let class = || -> Result<Class, Errno> {
            (f.flags == 0 && f.imm <= 1)
                .then(|| {
                    if f.imm == 1 {
                        Class::Immutable
                    } else {
                        Class::Mutable
                    }
                })
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
                    Part::Payload => Want::Page,
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
            Op::Accept => {
                if part != Part::Both {
                    return Err(status::BAD);
                }
                Cmd::Accept {
                    page: page()?,
                    via: via()?,
                }
            }
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

// --- Tests ---

/// The codec only: pure and total, so no device, no root and no peer. Those paths are
/// exercised end to end in `server`.
#[cfg(test)]
mod tests {
    use super::*;

    fn small(lba: u64) -> PageRef {
        PageRef::new(lba).unwrap()
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
        for hops in [Hops::NONE, Hops::ONE, Hops::TWO] {
            for to in [
                To::Owner,
                To::Member(Member::new(0).unwrap()),
                To::Member(Member::new(1).unwrap()),
                To::Member(Member::new(2).unwrap()),
            ] {
                let via = Via::new(to, hops);
                let page = s;
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
                // An accept always carries its guard, ballot and epoch in a trailer.
                v.push((Cmd::Accept { page: s, via }, 2 * BLOCK));
            }
        }
        v.push((
            Cmd::GetMeta {
                page: s,
                from: Source::Cache,
            },
            BLOCK,
        ));
        v.push((Cmd::Warm { page: s }, BLOCK));
        v.push((
            Cmd::Get {
                page: s,
                from: Source::Cache,
                want: Want::Gather,
            },
            2 * BLOCK,
        ));
        // The only place a class still travels: a group sweep names one slab.
        for class in [Class::Mutable, Class::Immutable] {
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
        // A gather is exactly two blocks, and an accept is never anything else.
        let gather = Cmd::Get {
            page: s,
            from: Source::Group(via),
            want: Want::Gather,
        };
        assert!(gather.encode(2 * BLOCK).is_ok());
        assert_eq!(gather.encode(BLOCK), Err(status::BAD));
        let accept = Cmd::Accept { page: s, via };
        assert!(accept.encode(2 * BLOCK).is_ok());
        assert_eq!(accept.encode(BLOCK), Err(status::BAD));
        assert_eq!(accept.encode(3 * BLOCK), Err(status::BAD));
        // A control op is exactly one trailer block.
        let trim = Cmd::Trim { page: s, via };
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
        for seq in [0u8, 1, 62, 63] {
            assert_eq!(Seq::wrap(seq).get(), seq);
        }
        assert_eq!(Seq::wrap(64).get(), 0);
        assert_eq!(Seq::wrap(65).get(), 1);
        assert_eq!(Seq::wrap(255).get(), 63);
        assert!(GroupIx::new(GroupIx::MAX).is_none());
        assert!(GroupIx::new(GroupIx::MAX - 1).is_some());
        assert!(State::new(3).is_none());
        // Every block is nameable; nothing past the universe's end is.
        assert!(PageRef::new(0).is_some());
        assert!(PageRef::new(1).is_some());
        assert!(PageRef::new(crate::config::MAX_LBA - 1).is_some());
        assert!(PageRef::new(crate::config::MAX_LBA).is_none());
    }

    /// Every address the control plane may hand out is nameable, which lets an extent sit
    /// anywhere in its universe's space.
    #[test]
    fn a_command_reaches_the_whole_universe() {
        let via = Via::direct(To::Owner);
        for last in [0, 1, crate::config::HUGE_BLOCKS, crate::config::MAX_LBA - 1] {
            let c = Cmd::Trim {
                page: small(last),
                via,
            };
            let d = Cmd::decode(c.encode(BLOCK).unwrap(), BLOCK, false).unwrap();
            assert_eq!(d.routing().unwrap().0.lba(), last);
        }
    }

    /// One region now, sized so the whole universe and its trailer fit the device.
    #[test]
    fn the_region_fits_the_device() {
        let via = Via::new(To::Member(Member::new(2).unwrap()), Hops::TWO);
        let s = Cmd::Trim {
            page: small(crate::config::MAX_LBA - 1),
            via,
        }
        .encode(BLOCK)
        .unwrap();
        assert!(s + 2 <= MAX_LBA);
        assert_eq!(DEVICE_SIZE, 4 << 60);
        // The last block's byte offset still fits `loff_t`.
        assert!(DEVICE_SIZE < i64::MAX as u64);
    }

    #[test]
    fn device_keys_are_disjoint_from_client_keys() {
        // A fabric key round trips to its universe.
        for u in [0, 1, 7, u32::MAX] {
            assert_eq!(universe_of(key(u)), Some(u));
        }
        // A client device is keyed by its bare 32-bit id, so no id can collide with a
        // universe: `configure` hands the runtime `dev.id as u64` directly.
        for id in [0u32, 1, 7, u32::MAX] {
            assert_eq!(universe_of(id as u64), None);
        }
        assert_ne!(key(0), 0);
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
        // Opcodes 0..15 belonged to the old wide-page protocol and 29..31 are unassigned.
        // A peer running either a newer or an older build must not be able to make us do
        // something by accident: the renumbering is what makes a mixed fleet fail closed.
        for raw in (0..16u64).chain(29..32) {
            let sh = OFF_BITS + IMM_BITS + FLAG_BITS;
            for read in [false, true] {
                assert_eq!(
                    Cmd::decode((raw << sh) << FRAME_SHIFT, BLOCK, read),
                    Err(status::BAD),
                    "opcode {raw}"
                );
            }
        }
    }

    /// The bits an op does not define must be clear. A peer that sets one is asking for
    /// something we do not implement, which is exactly [`status::BAD`].
    #[test]
    fn reserved_header_bits_are_refused() {
        let raw = |op: Op, flags: u8, imm: u8, offset: u64| {
            Cmd::decode(
                Frame {
                    op,
                    flags,
                    imm,
                    offset,
                }
                .encode(),
                BLOCK,
                op.is_read(),
            )
        };
        // Bit 2 outside a cache read is undefined; a cache read carries no route.
        assert_eq!(raw(Op::Trim, CACHE_ONLY, 0, 4), Err(status::BAD));
        assert_eq!(raw(Op::Prepare, CACHE_ONLY, 0, 4), Err(status::BAD));
        assert_eq!(
            raw(Op::GetMeta, CACHE_ONLY | 1, 0, 4),
            Err(status::BAD)
        );
        assert_eq!(raw(Op::GetMeta, CACHE_ONLY, 1, 4), Err(status::BAD));
        assert!(raw(Op::GetMeta, CACHE_ONLY, 0, 4).is_ok());
        // A warm and the group-addressed ops are never routed and never addressed.
        assert_eq!(raw(Op::Warm, 1, 0, 4), Err(status::BAD));
        assert_eq!(raw(Op::Warm, 0, 1, 4), Err(status::BAD));
        assert!(raw(Op::Warm, 0, 0, 4).is_ok());
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
        assert_eq!(raw(Op::Ping, 0, 0, 1), Err(status::BAD));
        assert_eq!(raw(Op::SnapOpen, 0, 0, 1), Err(status::BAD));
        assert!(raw(Op::SnapOpen, 0, 0, 1 << 9 | 1).is_ok());
        // A group index that would be truncated is refused, not narrowed.
        assert_eq!(raw(Op::Term, 0, 0, 1 << 32), Err(status::BAD));
        assert_eq!(
            raw(Op::Merkle, 0, 0, GroupIx::MAX as u64),
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
                class: Class::Mutable,
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
