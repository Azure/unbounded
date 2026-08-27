use std::io;
use std::path::Path;
use std::time::Duration;

use crate::config::Peer;
use crate::runtime::{Buf, Disk, Durability, Errno, ResourceBuild};

use super::{BLOCK, Cmd, status};

/// How long a fabric command may take before it is a path failure. The fabric never
/// retries: expiry surfaces as `ETIME`, consensus treats the replica as non-responding,
/// and the other two members carry the quorum.
const TIMEOUT: Duration = Duration::from_secs(2);

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
    universe: u32,
    peer: u32,
}

impl Link {
    /// Open a link to `p` in `universe`; the control plane has already attached the
    /// peer's fabric namespace locally, so this is just an `open(2)`. Links are opened
    /// when a configuration is built and closed when it retires; re-declaring the same
    /// path across a reload keeps the registration, so a live peer's fd is never
    /// disturbed.
    pub(crate) fn open(c: &ResourceBuild, universe: u32, p: &Peer) -> io::Result<Link> {
        let disk = c.disk(Path::new(&p.device), Some(TIMEOUT), None)?;
        Ok(Link {
            disk,
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
    /// never reaches the wire. A frame is at most two blocks, well under any peer's MDTS,
    /// so one command is one request at the target and there is no partial-failure case.
    pub(crate) async fn send(&self, cmd: Cmd, buf: Buf) -> Result<(), Errno> {
        let lba = cmd.encode(buf.len())?;
        let off = lba * BLOCK as u64;
        if cmd.op().is_read() {
            self.disk.read(off, buf).await
        } else {
            // Durable: a fabric write is only acked once the peer has it, and the ublk
            // device advertises no volatile cache, so there is no flush to pair.
            self.disk.write(off, buf, Durability::Durable).await
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
