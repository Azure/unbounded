// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Node-to-node transport: a stateless codec and nothing else.
//!
//! A logical operation becomes a plain NVMe read or write against a peer's namespace,
//! because read and write are the only verbs nvme-of gives us. No session, no sequence
//! number, no per-operation memory beyond the in-flight SQE: everything an operation
//! needs is in the LBA or in a 4 KiB trailer beside the payload. Retries, quorum,
//! caching and placement live above this file.
//!
//! The trick is that **the LBA is the RPC**. A read whose LBA encodes `(GET, addr)`
//! returns the page, DMA'd straight into the initiator's registered buffer; a write
//! encoding `(TRIM, addr)` returns only a status. The initiator drives everything, so
//! anything needing a rich reply has to be a read.
//!
//! # What the kernel forces
//!
//! 1. **Two address regions, not sub-frames.** `imm` sits inside the frame id, so
//!    `imm`-selected sub-frames of a 4 MiB page would land at unrelated LBAs. Each
//!    region has its own stride instead: one command goes out, the nvme layer splits it
//!    at the peer's MDTS, and the target reassembles by LBA offset.
//! 2. **48-bit frame ids.** A frame id times its stride is a byte offset, which must
//!    fit `loff_t`.
//! 3. **Four statuses.** The nvmet target erases the rest to `EIO`; see [`status`].
//! 4. **Relaying is a flag, not an opcode.** A read carries no data *to* the target, so
//!    an inner frame cannot ride an outer one's trailer — and the motivating case,
//!    relaying a metadata read, is a read. The outer frame *is* the inner frame; see
//!    [`HOPS`].
//! 5. **Gather copies the 4 KiB payload.** `ReadvFixed`/`WritevFixed` take a *single*
//!    `buf_index`, so a guest's ublk page and a pooled trailer cannot share one vectored
//!    fixed SQE; gather is instead one contiguous registered buffer of page plus
//!    trailer. Still one command and one round trip, and the volume path already stages
//!    every 4 KiB page through a pool buffer. 4 MiB pages never gather, and stay zero
//!    copy.
//! 6. **One entry point.** Direction follows the opcode and shape follows the length, so
//!    the client API is [`Link::send`] plus [`Frame::decode`]; what a frame *means* is
//!    decided above this file.

use std::io;
use std::path::Path;
use std::time::Duration;

use crate::config::Peer;
use crate::runtime::{Buf, Configurator, Disk, Durability, Errno};

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

/// The status alphabet, as it survives the wire.
///
/// The `ublk -> BLK_STS_* -> NVMe status -> nvme initiator -> errno` pipeline preserves
/// **four** values. The narrow point is nvmet's `blk_to_nvme_status()`, which has arms
/// for exactly `BLK_STS_{NOSPC,TARGET,NOTSUPP,MEDIUM}` and sends the rest as
/// `NVME_SC_INTERNAL`, which the initiator's `nvme_error_status()` turns back into
/// `EIO`. So `EBADE`, `EILSEQ`, `EAGAIN`, `ENOLINK`, `EINVAL` all arrive as a bare
/// `EIO`, which the rule "any unrecognized status is a transport failure" escalates
/// into a spurious path failover.
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
/// [`MISSING`] lacks it: safe, since every op is replay-safe, and useful for a briefly
/// quarantined page, but it is why a lost ballot is *not* reported as `MISSING` —
/// consensus would pay six round trips for it.
///
/// Two further outcomes need no code of their own:
///
/// - **Already written** (Immutable, `EEXIST`). A CORFU fill loser has to `GET` the
///   winner regardless, so the follow-up command recovers the distinction.
/// - **Overloaded** (`EAGAIN`). Backpressure *delays* the completion instead, as the
///   volume path does at `Pressure::Low`: holding the ublk tag makes queue depth bound
///   the kernel, which a status cannot.
///
/// Statuses classify, they do not explain: [`STALE`] says the caller's model of a page
/// is wrong, not what the right one is, and learning that costs a second command.
pub(crate) mod status {
    use crate::runtime::Errno;

    /// Your model of this page is stale: wrong ballot, version or placement. Both
    /// recoveries start by asking — `PREPARE` or `GETMETA` — and re-read the config if
    /// the page is not here at all.
    pub(crate) const STALE: Errno = Errno::EREMOTEIO;
    /// Page missing or quarantined. Heal from another group member.
    pub(crate) const MISSING: Errno = Errno::ENODATA;
    /// Malformed frame, unknown opcode, or an op this node does not implement.
    pub(crate) const BAD: Errno = Errno::EOPNOTSUPP;
    /// Out of space.
    pub(crate) const NOSPC: Errno = Errno::ENOSPC;
}

// ---------------------------------------------------------------------------
// opcodes
// ---------------------------------------------------------------------------

/// `GET` and `ACCEPT` are the hot path; everything else is rare by construction.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub(crate) enum Op {
    /// Read a page. Reply *is* the payload.
    Get = 0,
    /// Read the register: version, ballot, state, width hint. The hedged-read path.
    GetMeta = 1,
    /// Raise this group's promise and report `(version, ballot, term)`. A read, because
    /// the reply is rich.
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
    /// Both repair and the migration learn use this.
    Learn = 8,
    /// Freeze a shard at its source group.
    Seal = 9,
    /// Liveness and geometry.
    Ping = 10,
    /// A group's standing promise. Names a group, not a page.
    Term = 11,
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
            _ => return None,
        })
    }

    /// Whether the command is an NVMe read. Every interrogative op is a read, the only
    /// direction a rich reply can travel; every imperative op is a write with a small
    /// status alphabet.
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

    /// Whether the op moves no page. Its single block is the trailer.
    ///
    /// `LEARN` is here rather than with the page-moving ops: a 4 MiB frame has no room
    /// for a trailer (see the wire format below), but a repair must carry an exact
    /// `(version, ballot)`. So it names the value and the member holding it, and the
    /// receiver pulls the page with an ordinary `GET` — repair is the cold path, so the
    /// extra hop is free where it is spent.
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
        )
    }
}

/// Forwarding hops this frame may still take, in flag bits 0..1.
///
/// A budget, not an instruction: the *receiver* decides whether the frame is its to
/// serve — [`Frame::imm`] says who it is addressed to — and spends a hop when it is
/// not. It forwards the *same* frame with the budget one smaller and completes the
/// outer command when the inner one does. Because the outer frame is the inner frame,
/// a forward works in either direction and needs no trailer: a forwarded `GET` streams
/// the page back through the buffer it arrived in, a forwarded `ACCEPT` streams it
/// forward.
///
/// The field holds three; `paxos::Route` grants at most two, enough for a cross-zone
/// entry node forwarding to a group member whose shard is mid-migration and must
/// forward again. A node that must forward with no budget left answers
/// [`status::STALE`], which sends the originator back to its config.
const HOPS: u8 = 0b11;
/// Serve this `GET` from the cache region only, never from the allocator.
///
/// The one fabric change the cache requires, and a modifier on `GET` (and the `GETMETA`
/// paired with a 4 MiB one) alone: a reader that believes a cohort peer holds a replica
/// asks it directly, concurrently with the mandatory metadata round. A miss, or a
/// replica that is shedding, answers [`status::MISSING`] and the reader falls back to
/// the group, so this frame is never a correctness dependency and declining it is
/// always safe.
///
/// A 4 KiB reply is gather mode: the register the copy claims rides the trailer beside
/// the page. A 4 MiB reply has no trailer, so that register rides the paired `GETMETA`.
/// Neither carries a width hint — a replica does not own the sketch.
pub(crate) const CACHE_ONLY: u8 = 1 << 2;

// ---------------------------------------------------------------------------
// wire format
// ---------------------------------------------------------------------------

// Two regions, each with its own stride, so a 4 MiB payload is one contiguous run of
// blocks rather than a scatter of sub-frames:
//
//   small   lba = frame * 2                 frame < 2^48   block 0 payload, 1 trailer
//   huge    lba = HUGE_BASE + frame * 1024  frame < 2^39   blocks 0..1023 payload
//
// A frame id packs, from the low bit up:
//
//   | offset (32 small / 23 huge) | vol 6 | imm 2 | flags 3 | opcode 5 |
//
// `vol` is the volume's fabric slot from the config (config.rs), not its id: an id is
// 32 bits and six are all a frame can spare. The control plane assigns the slot and it
// is frozen for the volume's life, so it cannot be derived from position or id order —
// anything derived would shift the moment two nodes list their volumes differently or
// one volume is deleted, silently repointing every frame in flight. `offset` is the
// page index within that volume, so both classes share one space without either paying
// for the other's granularity.
const OP_BITS: u32 = 5;
const FLAG_BITS: u32 = 3;
const IMM_BITS: u32 = 2;
const VOL_BITS: u32 = 6;
const SMALL_OFF_BITS: u32 = 32;
const HUGE_OFF_BITS: u32 = 23;

const SMALL_SHIFT: u32 = 1;
const HUGE_SHIFT: u32 = 10;
/// Blocks in a 4 MiB page.
const HUGE_BLOCKS: u64 = 1 << HUGE_SHIFT;

/// First LBA of the 4 MiB region. Also the size of the 4 KiB region.
const HUGE_BASE_LBA: u64 =
    1 << (SMALL_OFF_BITS + VOL_BITS + IMM_BITS + FLAG_BITS + OP_BITS + SMALL_SHIFT);
const MAX_LBA: u64 = HUGE_BASE_LBA * 2;

/// Size the fabric device must be declared with: 4 EiB, entirely sparse. It is an
/// address space, not storage — nothing is ever laid out in it.
pub(crate) const DEVICE_SIZE: u64 = MAX_LBA * BLOCK as u64;

/// The logical block, and so the trailer size. Deliberately oversized: it never reaches
/// media, it exists only to spare a second round trip, and 4 KiB keeps every buffer
/// page-aligned and RDMA-friendly.
pub(crate) const BLOCK: usize = 4096;

/// Pages a 4 KiB volume may hold and still be reachable over the fabric.
pub(crate) const MAX_SMALL_PAGES: u64 = 1 << SMALL_OFF_BITS;
/// Pages a 4 MiB volume may hold and still be reachable over the fabric.
pub(crate) const MAX_HUGE_PAGES: u64 = 1 << HUGE_OFF_BITS;

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
    /// Bare mode: page bytes only, starting `off` into the page. `off` is always 0 for
    /// a 4 KiB page, and is how a 4 MiB transfer reassembles after the nvme layer has
    /// split it at the peer's MDTS.
    Payload { off: usize },
    /// Control mode: one trailer block, no page.
    Trailer,
    /// Gather mode: a page followed by its trailer, in one command.
    Both,
}

/// A decoded LBA. Pure data — construct one, hand it to [`Link::send`], and it is a
/// command; get one back from [`Frame::decode`], and it is a request.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) struct Frame {
    pub(crate) op: Op,
    /// [`HOPS`] | [`CACHE_ONLY`].
    pub(crate) flags: u8,
    /// Who this frame is addressed to, in two bits, uniformly across every page op.
    ///
    /// Zero means "you own this operation": resolve the address yourself and answer
    /// authoritatively rather than from your own copy. `k + 1` names member index `k`
    /// of the address's group — two bits hold both, because a group is three wide.
    ///
    /// This generalizes `ACCEPT`'s encoding, where zero means "you are the proposer,
    /// pick a ballot". On a `GET` it means "give me the linearizable value", which lets
    /// a node holding neither the slot table nor the catalog for a remote zone still
    /// take a confirmed read: it hands the whole round to the entry node, where the
    /// members are.
    ///
    /// A receiver that is not the addressee forwards, if [`HOPS`] allows.
    pub(crate) imm: u8,
    /// Whether `offset` counts 4 MiB pages rather than 4 KiB ones. Selects the region.
    pub(crate) huge: bool,
    /// The volume's fabric slot, assigned by the control plane. Not its id.
    pub(crate) vol: u8,
    /// Page index within the volume.
    pub(crate) offset: u32,
}

impl Frame {
    pub(crate) fn new(op: Op, huge: bool, vol: u8, offset: u32) -> Frame {
        Frame {
            op,
            flags: 0,
            imm: 0,
            huge,
            vol,
            offset,
        }
    }

    /// The frame's base LBA — block 0 of its footprint.
    ///
    /// Total: out-of-range fields are masked, not rejected. Every caller has already
    /// validated its address against the config, and a panicking encoder on the hot
    /// path would be worse than a frame the far end refuses — a masked frame decodes to
    /// a different address, which fails its own bounds check there.
    pub(crate) fn encode(&self) -> u64 {
        let (off_bits, base, shift) = if self.huge {
            (HUGE_OFF_BITS, HUGE_BASE_LBA, HUGE_SHIFT)
        } else {
            (SMALL_OFF_BITS, 0, SMALL_SHIFT)
        };
        let mut id = self.offset as u64 & mask(off_bits);
        id |= (self.vol as u64 & mask(VOL_BITS)) << off_bits;
        id |= (self.imm as u64 & mask(IMM_BITS)) << (off_bits + VOL_BITS);
        id |= (self.flags as u64 & mask(FLAG_BITS)) << (off_bits + VOL_BITS + IMM_BITS);
        id |= (self.op as u64) << (off_bits + VOL_BITS + IMM_BITS + FLAG_BITS);
        base + (id << shift)
    }

    /// The inverse, plus the frame shape implied by the transfer length.
    ///
    /// Pure and total: any `(lba, len)` at all either decodes or is [`status::BAD`],
    /// and none of them panics. This runs on the target before anything else, on bytes
    /// a peer chose, so being total is a safety property here rather than a nicety.
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
        let vol_sh = off_bits;
        let imm_sh = vol_sh + VOL_BITS;
        let flag_sh = imm_sh + IMM_BITS;
        let op_sh = flag_sh + FLAG_BITS;
        let op = Op::from_bits(((id >> op_sh) & mask(OP_BITS)) as u8).ok_or(status::BAD)?;
        let f = Frame {
            op,
            flags: ((id >> flag_sh) & mask(FLAG_BITS)) as u8,
            imm: ((id >> imm_sh) & mask(IMM_BITS)) as u8,
            huge,
            vol: ((id >> vol_sh) & mask(VOL_BITS)) as u8,
            offset: (id & mask(off_bits)) as u32,
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

    /// The frame as it leaves a site, with its budget restored. A crossing is bounded
    /// by the address rather than by the budget: past it the address is site-local, so
    /// a second crossing is unreachable and exactly one is possible.
    pub(crate) fn refreshed(&self) -> Frame {
        Frame {
            flags: self.flags | HOPS,
            ..*self
        }
    }
}

// ---------------------------------------------------------------------------
// trailers
// ---------------------------------------------------------------------------

// A trailer is one block of little-endian `u64` slots. There is deliberately no type
// per record: the records that exist today are two to four fields wide. The slot
// indices live in `paxos.rs` (`T_*`) and `heal.rs`; this is the per-op table.
//
//   PING     0 node id   1 config generation  2 topology epoch
//   GETMETA  0 version   1 ballot   2 state              3 cache width
//   GET      0 version   1 ballot   2 state              3 cache width (reply, gather)
//   PREPARE  0 version   1 ballot   2 promised term (after the bump)
//   ACCEPT   0 guard     1 ballot   2 topology epoch     (4 KiB only)
//   TRIM     0 guard     1 ballot   2 topology epoch
//   LEARN    0 version   1 ballot   2 member holding it  3 repair
//   SEAL                 1 term     2 extent
//   TERM                            2 promised term      (reply)
//   MERKLE   0..511 digests                              (reply, fills the block)
//   SNAPOPEN 0 cursor id                                 (reply)
//   SNAPNEXT 0 count     1 done     then 3 slots per page (reply)
//
// `LEARN`'s slot 3 marks a repair rather than a migration push, which also admits the
// equal-register case: our entry matches but our bytes fail their checksum. Slot 0 is
// empty on `SEAL`, whose volume comes from the frame, and on `TERM`, whose group does.
//
// `MERKLE`, `SNAPOPEN` and `SNAPNEXT` are the anti-entropy ops (`heal.rs`); they and
// `TERM` name a consensus group rather than a page, so `vol` and `offset` carry
// something else entirely there. A digest vector fills the block exactly, which is why
// it is 512 wide and one level deep instead of a tree. `SNAPNEXT` ships
// `(address, version, ballot)` and no page bytes; the reader pulls what it wants with
// an ordinary `GET`, so there is one data path and not two.
//
// A 4 MiB `ACCEPT` has no trailer, because a huge frame's whole stride is payload and
// a ublk request buffer has no address a vectored SQE could gather beside. Its guard
// and ballot are derived by the acceptor instead: see `paxos::accept`.
//
// The topology epoch rides the trailer of every routed write and of no bare read: a
// read served from a stale epoch is absorbed by the quorum, so gathering to carry it
// would tax the hot path to defend the path that needs no defending.

/// Write slot `i` of a trailer. Out of range is a no-op: a trailer arrives from a
/// peer, so nothing here may panic on its contents.
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

// ---------------------------------------------------------------------------
// links
// ---------------------------------------------------------------------------

/// How long a fabric command may take before it is a path failure.
///
/// The fabric never retries. Expiry surfaces as `ETIME`, consensus treats the replica
/// as non-responding, and the other two members carry the quorum.
const TIMEOUT: Duration = Duration::from_secs(2);

/// How long a 4 MiB payload may take before it is a path failure.
///
/// Shorter than [`TIMEOUT`] because the buffer such a command puts on the wire is the
/// guest's own, and the write cannot be answered until every replica leg is done with
/// it: this is what a peer that stops answering costs a 4 MiB write. A transfer that
/// size has either landed in a fraction of a second or is not going to.
const HUGE_TIMEOUT: Duration = Duration::from_millis(250);

/// A peer link. A handle and nothing more: an fd and the peer's id, no per-op state.
///
/// `!Send`, like the [`Disk`] it wraps. The runtime registers the file on every core,
/// so a submission never crosses cores even though one `Link` serves them all.
pub(crate) struct Link {
    disk: Disk,
    /// The same device on the shorter deadline a 4 MiB command is held to.
    huge: Disk,
    peer: u32,
}

impl Link {
    /// Open a link to `p`. The control plane has already attached the peer's fabric
    /// namespace locally, so this is an `open(2)` and nothing more.
    ///
    /// Links are opened when a configuration is built and closed when it retires;
    /// re-declaring the same path across a reload keeps the registration, so a live
    /// peer's fd is never disturbed.
    pub(crate) fn open(c: &Configurator, p: &Peer) -> io::Result<Link> {
        let disk = c.disk(Path::new(&p.device), Some(TIMEOUT), None)?;
        let huge = disk.by(HUGE_TIMEOUT);
        Ok(Link {
            disk,
            huge,
            peer: p.id,
        })
    }

    pub(crate) fn peer(&self) -> u32 {
        self.peer
    }

    /// Issue one frame. This is the whole client API.
    ///
    /// `buf` is the payload, the trailer, or both, and its length is what tells the
    /// target which; the shape is checked here so a malformed command is refused before
    /// it costs a round trip. Nothing is copied: `buf` is registered memory, which for
    /// a 4 MiB page is the guest's own pages.
    ///
    /// A 4 MiB transfer goes out as one command and the nvme layer splits it at the
    /// peer's MDTS; the target sees the pieces as separate requests at consecutive LBAs
    /// inside the frame's footprint and reassembles them by offset. There is no chunk
    /// index and no partial-failure case, because there is one completion.
    pub(crate) async fn send(&self, f: Frame, buf: Buf) -> Result<(), Errno> {
        let lba = f.encode();
        Frame::decode(lba, buf.len())?;
        let off = lba * BLOCK as u64;
        if f.op.is_read() {
            self.disk.read(off, buf).await
        } else {
            // Durable: a fabric write is only acked once the peer has it, and the
            // ublk device advertises no volatile cache, so there is no flush to pair.
            let d = if f.huge { &self.huge } else { &self.disk };
            d.write(off, buf, Durability::Durable).await
        }
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

/// The codec only: pure and total, so no device, no root and no peer. The paths that
/// need those are exercised end to end in `server`.
#[cfg(test)]
mod tests {
    use super::*;

    const ALL: [Op; 12] = [
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
    ];

    #[test]
    fn round_trips_every_field() {
        for op in ALL {
            for huge in [false, true] {
                for flags in 0..8u8 {
                    for imm in 0..4u8 {
                        // Alternating bits, so a field landing at the wrong shift shows.
                        let offset = if huge { 0x55_5555 } else { 0x5555_5555 };
                        let f = Frame {
                            op,
                            flags,
                            imm,
                            huge,
                            vol: 0b101010,
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
            vol: 63,
            offset: u32::MAX,
        };
        let huge = Frame {
            op: Op::Ping,
            flags: 7,
            imm: 3,
            huge: true,
            vol: 63,
            offset: (MAX_HUGE_PAGES - 1) as u32,
        };
        assert!(small.encode() < HUGE_BASE_LBA);
        assert!(huge.encode() >= HUGE_BASE_LBA);
        assert!(huge.encode() + HUGE_BLOCKS <= MAX_LBA);
        assert_eq!(DEVICE_SIZE, 4 << 60);
        // The last block's byte offset still fits `loff_t`.
        assert!(DEVICE_SIZE < i64::MAX as u64);
    }

    #[test]
    fn classifies_frame_shape() {
        let get = Frame::new(Op::Get, false, 1, 7);
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
        // Three blocks is not a shape a 4 KiB frame has.
        assert!(Frame::decode(get.encode(), 3 * BLOCK).is_err());

        let ping = Frame::new(Op::Ping, false, 0, 0);
        assert_eq!(
            Frame::decode(ping.encode(), BLOCK).unwrap().1,
            Part::Trailer
        );
        assert!(Frame::decode(ping.encode(), 2 * BLOCK).is_err());

        // A 4 MiB page arrives in whatever pieces the peer's MDTS produced.
        let huge = Frame::new(Op::Get, true, 2, 9);
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
        let trim = Frame::new(Op::Trim, true, 2, 9);
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
        // Opcodes 12..31 are unassigned; a peer running a newer build must not be able
        // to make us do something by accident.
        for raw in 12..32u64 {
            let sh = SMALL_OFF_BITS + VOL_BITS + IMM_BITS + FLAG_BITS;
            assert_eq!(
                Frame::decode((raw << sh) << SMALL_SHIFT, BLOCK),
                Err(status::BAD)
            );
        }
    }

    #[test]
    fn forwarding_preserves_the_request() {
        // A forward changes nothing but how far the frame may still go, which is what
        // makes it work in both directions and at either frame shape.
        let mut f = Frame::new(Op::GetMeta, false, 3, 9);
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
