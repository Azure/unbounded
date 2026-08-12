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
//!    [`Link::send`] plus [`Frame::decode`].

use std::io;
use std::path::Path;
use std::time::Duration;

use crate::config::Peer;
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
/// A budget, not an instruction: a receiver that is not the addressee ([`Frame::imm`])
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
pub(crate) const CACHE_ONLY: u8 = 1 << 2;

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

// Both regions cover a whole universe, so config validation's `base_lba + blocks <=
// MAX_LBA` is the only bound a page address needs; the fabric adds none of its own.
const _: () = assert!(1u64 << SMALL_OFF_BITS == crate::config::MAX_LBA);
const _: () = assert!((1u64 << HUGE_OFF_BITS) * HUGE_BLOCKS == crate::config::MAX_LBA);

const fn mask(bits: u32) -> u64 {
    (1u64 << bits) - 1
}

/// Forwarding hops `flags` still allows.
pub(crate) fn hops(flags: u8) -> u8 {
    flags & HOPS
}

/// Which blocks of a frame a request covers, and so what the payload means.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Part {
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
pub(crate) struct Frame {
    pub(crate) op: Op,
    /// [`HOPS`] | [`CACHE_ONLY`].
    pub(crate) flags: u8,
    /// Who this frame is addressed to, in two bits, uniformly across every page op.
    ///
    /// Zero means "you own this operation": resolve the address yourself and answer
    /// authoritatively rather than from your own copy, which is how a node holding
    /// neither the extent table nor the catalog for a remote zone still takes a confirmed
    /// read. On `ACCEPT` it means "you are the proposer, pick a ballot". `k + 1` names
    /// member index `k` of the address's group; two bits hold both, because a group is
    /// three wide. The group-addressed ops borrow it for the page class instead; see
    /// `heal.rs`. A receiver that is not the addressee forwards, if [`HOPS`] allows.
    pub(crate) imm: u8,
    /// Whether `offset` counts 4 MiB pages rather than 4 KiB ones. Selects the region.
    pub(crate) huge: bool,
    /// Page index in the universe for a page op; op-specific for the group-addressed
    /// ops, which name no page at all.
    pub(crate) offset: u64,
}

impl Frame {
    /// A frame naming the page at `lba` in the arriving universe's address space. `lba`
    /// counts 4 KiB blocks whatever the class, because that is the unit the control plane
    /// places extents in; a 4 MiB page's `lba` is the first of its 1024.
    pub(crate) fn page(op: Op, huge: bool, lba: u64) -> Frame {
        Frame::raw(op, huge, if huge { lba / HUGE_BLOCKS } else { lba })
    }

    /// A frame whose `offset` is not a page index: the anti-entropy and `TERM` ops,
    /// which name a consensus group. See `heal.rs` for the packing.
    pub(crate) fn raw(op: Op, huge: bool, offset: u64) -> Frame {
        Frame {
            op,
            flags: 0,
            imm: 0,
            huge,
            offset,
        }
    }

    /// The block this frame's page starts at, in the universe's address space. The
    /// inverse of [`Frame::page`], and meaningless on a group-addressed frame.
    pub(crate) fn lba(&self) -> u64 {
        if self.huge {
            self.offset * HUGE_BLOCKS
        } else {
            self.offset
        }
    }

    /// The frame's base LBA, block 0 of its footprint. Total: out-of-range fields are
    /// masked, not rejected, because every caller has already validated its address
    /// against the config and a masked frame just fails its bounds check at the far end.
    pub(crate) fn encode(&self) -> u64 {
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
    pub(crate) fn decode(lba: u64, len: usize) -> Result<(Frame, Part), Errno> {
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

    /// The frame as it leaves a forwarder: same request, one hop poorer.
    pub(crate) fn forwarded(&self) -> Frame {
        Frame {
            flags: (self.flags & !HOPS) | hops(self.flags).saturating_sub(1),
            ..*self
        }
    }
}

// --- trailers ---

// A trailer is one block of little-endian `u64` slots, with no type per record: the
// records today are two to four fields wide. Slot indices live in `paxos.rs` (`T_*`) and
// `heal.rs`; this is the per-op table.
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
//   SNAPOPEN 0 cursor id                                 (reply)
//   SNAPNEXT 0 count     1 done     then 3 slots per page (reply)
//
// `LEARN`'s slot 3 marks a repair rather than a migration push, which also admits the
// equal-register case: our entry matches but our bytes fail their checksum. `WARM` names
// no ballot and no holder, because the receiver reads the page through the ordinary
// cross-zone path; its stage (zero from the writing zone, one relayed by a gateway to the
// cohort member that will hold the copy) stops a warm being relayed forever. `SEAL` names
// an extent, so its address is in the trailer and not in the frame; `TERM` names a group,
// which is in the frame.
//
// A digest vector fills the block exactly, which is why `MERKLE` is 512 wide and one
// level deep instead of a tree. `SNAPNEXT` ships `(address, version, ballot)` and no page
// bytes; the reader pulls what it wants with an ordinary `GET`.
//
// A 4 MiB `ACCEPT` has no trailer, because a huge frame's whole stride is payload and a
// ublk request buffer has no address a vectored SQE could gather beside. Its guard and
// ballot are derived by the acceptor instead: see `paxos::accept`.
//
// The topology epoch rides the trailer of every routed write and of no bare read: a read
// served from a stale epoch is absorbed by the quorum.

/// Write slot `i` of a trailer. Out of range is a no-op: a trailer arrives from a peer,
/// so nothing here may panic on its contents.
pub(crate) fn put(dst: &mut [u8], i: usize, v: u64) {
    if let Some(c) = slot(dst.len(), i).and_then(|r| dst.get_mut(r)) {
        c.copy_from_slice(&v.to_le_bytes());
    }
}

/// Read slot `i` of a trailer. Out of range reads as zero.
pub(crate) fn get(src: &[u8], i: usize) -> u64 {
    slot(src.len(), i)
        .and_then(|r| src.get(r))
        .map_or(0, |c| u64::from_le_bytes(c.try_into().unwrap()))
}

fn slot(len: usize, i: usize) -> Option<std::ops::Range<usize>> {
    let a = i.checked_mul(8)?;
    let b = a.checked_add(8)?;
    (b <= len).then_some(a..b)
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

    /// Issue one frame. This is the whole client API.
    ///
    /// `buf` is the payload, the trailer, or both; its length tells the target which, and
    /// the shape is checked here so a malformed command costs no round trip. Nothing is
    /// copied: `buf` is registered memory, which for a 4 MiB page is the guest's own
    /// pages. A 4 MiB transfer goes out as one command that the nvme layer splits at the
    /// peer's MDTS; the target sees the pieces as separate requests at consecutive LBAs
    /// inside the frame's footprint. There is no chunk index and no partial-failure case,
    /// because there is one completion.
    pub(crate) async fn send(&self, f: Frame, buf: Buf) -> Result<(), Errno> {
        let lba = f.encode();
        Frame::decode(lba, buf.len())?;
        let off = lba * BLOCK as u64;
        if f.op.is_read() {
            self.disk.read(off, buf).await
        } else {
            // Durable: a fabric write is only acked once the peer has it, and the ublk
            // device advertises no volatile cache, so there is no flush to pair.
            let d = if f.huge { &self.huge } else { &self.disk };
            d.write(off, buf, Durability::Durable).await
        }
    }
}

// --- Tests ---

/// The codec only: pure and total, so no device, no root and no peer. Those paths are
/// exercised end to end in `server`.
#[cfg(test)]
mod tests {
    use super::*;

    const ALL: [Op; 13] = [
        Op::Get,
        Op::GetMeta,
        Op::Prepare,
        Op::Accept,
        Op::Trim,
        Op::SnapOpen,
        Op::SnapNext,
        Op::Merkle,
        Op::Learn,
        Op::Seal,
        Op::Ping,
        Op::Term,
        Op::Warm,
    ];

    #[test]
    fn round_trips_every_field() {
        for op in ALL {
            for huge in [false, true] {
                for flags in 0..8u8 {
                    for imm in 0..4u8 {
                        // Alternating bits, so a field landing at the wrong shift shows.
                        let offset = if huge { 0x555_5555 } else { 0x15_5555_5555 };
                        let f = Frame {
                            op,
                            flags,
                            imm,
                            huge,
                            offset,
                        };
                        let (g, _) = Frame::decode(f.encode(), BLOCK).unwrap();
                        assert_eq!(f, g, "{f:?}");
                    }
                }
            }
        }
    }

    #[test]
    fn regions_do_not_overlap() {
        let small = Frame {
            op: Op::Ping,
            flags: 7,
            imm: 3,
            huge: false,
            offset: mask(SMALL_OFF_BITS),
        };
        let huge = Frame {
            op: Op::Ping,
            flags: 7,
            imm: 3,
            huge: true,
            offset: mask(HUGE_OFF_BITS),
        };
        assert!(small.encode() < HUGE_BASE_LBA);
        assert!(huge.encode() >= HUGE_BASE_LBA);
        assert!(huge.encode() + HUGE_BLOCKS <= MAX_LBA);
        assert_eq!(DEVICE_SIZE, 4 << 60);
        // The last block's byte offset still fits `loff_t`.
        assert!(DEVICE_SIZE < i64::MAX as u64);
    }

    /// Every address the control plane may hand out is nameable in both classes, which
    /// lets an extent sit anywhere in its universe's space.
    #[test]
    fn a_frame_reaches_the_whole_universe() {
        let last = crate::config::MAX_LBA - 1;
        let f = Frame::page(Op::Get, false, last);
        assert_eq!(Frame::decode(f.encode(), BLOCK).unwrap().0.lba(), last);

        let last_huge = crate::config::MAX_LBA - HUGE_BLOCKS;
        let g = Frame::page(Op::Get, true, last_huge);
        assert_eq!(
            Frame::decode(g.encode(), BLOCK * 1024).unwrap().0.lba(),
            last_huge
        );
    }

    #[test]
    fn classifies_frame_shape() {
        let get = Frame::page(Op::Get, false, 7);
        assert_eq!(
            Frame::decode(get.encode(), BLOCK).unwrap().1,
            Part::Payload { off: 0 }
        );
        assert_eq!(
            Frame::decode(get.encode(), 2 * BLOCK).unwrap().1,
            Part::Both
        );
        // Block 1 alone is the trailer half of a gather frame; it is not addressable.
        assert!(Frame::decode(get.encode() + 1, BLOCK).is_err());
        assert!(Frame::decode(get.encode(), 3 * BLOCK).is_err());

        let ping = Frame::page(Op::Ping, false, 0);
        assert_eq!(
            Frame::decode(ping.encode(), BLOCK).unwrap().1,
            Part::Trailer
        );
        assert!(Frame::decode(ping.encode(), 2 * BLOCK).is_err());

        // A 4 MiB page arrives in whatever pieces the peer's MDTS produced.
        let huge = Frame::page(Op::Get, true, 9 * HUGE_BLOCKS);
        assert_eq!(huge.offset, 9);
        for (block, blocks) in [(0u64, 1024u64), (0, 256), (256, 256), (1023, 1)] {
            let (g, p) = Frame::decode(huge.encode() + block, blocks as usize * BLOCK).unwrap();
            assert_eq!(g, huge);
            assert_eq!(
                p,
                Part::Payload {
                    off: block as usize * BLOCK
                }
            );
        }
        // Never past the end of the page.
        assert!(Frame::decode(huge.encode() + 1, 1024 * BLOCK).is_err());
        // A 4 MiB TRIM is a control frame and stays one block.
        let trim = Frame::page(Op::Trim, true, 9 * HUGE_BLOCKS);
        assert_eq!(
            Frame::decode(trim.encode(), BLOCK).unwrap().1,
            Part::Trailer
        );
        assert!(Frame::decode(trim.encode(), 2 * BLOCK).is_err());
    }

    #[test]
    fn decode_is_total() {
        // Anything at all is either a frame or BAD, never a panic: the target runs this
        // on bytes a peer chose.
        let mut x = 0x9E37_79B9_7F4A_7C15u64;
        for i in 0..200_000u64 {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            let len = (i as usize % 2050) * BLOCK;
            let _ = Frame::decode(x, len);
            let _ = Frame::decode(x >> (i % 64), len);
            let _ = Frame::decode(i, len);
        }
        assert!(Frame::decode(u64::MAX, BLOCK).is_err());
        assert!(Frame::decode(0, 0).is_err());
        assert!(Frame::decode(0, 4095).is_err());
        assert!(Frame::decode(0, usize::MAX).is_err());
    }

    #[test]
    fn unknown_opcode_is_refused() {
        // Opcodes 13..31 are unassigned; a peer running a newer build must not be able
        // to make us do something by accident.
        for raw in 13..32u64 {
            let sh = SMALL_OFF_BITS + IMM_BITS + FLAG_BITS;
            assert_eq!(
                Frame::decode((raw << sh) << SMALL_SHIFT, BLOCK),
                Err(status::BAD)
            );
        }
    }

    #[test]
    fn forwarding_preserves_the_request() {
        // A forward changes only how far the frame may still go, so it works in both
        // directions and at either frame shape.
        let mut f = Frame::page(Op::GetMeta, false, 9);
        f.flags = 2 | CACHE_ONLY;
        f.imm = 2;
        let g = f.forwarded();
        assert_eq!(
            Frame {
                flags: 1 | CACHE_ONLY,
                ..f
            },
            g
        );
        assert_eq!(hops(g.flags), 1);
        assert_eq!(hops(g.forwarded().flags), 0);
        assert_eq!(g.imm, f.imm);
        assert_eq!(Frame::decode(g.encode(), BLOCK).unwrap().0, g);
        // And direction is the opcode's alone.
        assert!(Op::Get.is_read() && Op::Ping.is_read());
        assert!(!Op::Accept.is_read() && !Op::Trim.is_read() && !Op::Seal.is_read());
    }

    #[test]
    fn trailer_slots_are_bounded() {
        let mut t = [0u8; BLOCK];
        put(&mut t, 0, 0xdead_beef_cafe_f00d);
        put(&mut t, 511, 7);
        assert_eq!(get(&t, 0), 0xdead_beef_cafe_f00d);
        assert_eq!(get(&t, 511), 7);
        assert_eq!(get(&t, 1), 0);
        // Past the end: silently dropped, reads as zero, never panics.
        put(&mut t, 512, 1);
        put(&mut t, usize::MAX / 8, 1);
        assert_eq!(get(&t, 512), 0);
        assert_eq!(get(&t, usize::MAX), 0);
        assert_eq!(get(&[], 0), 0);
    }
}
