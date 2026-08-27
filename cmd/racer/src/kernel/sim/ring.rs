//! The simulated ring.
//!
//! A ring is a queue in both worlds; the difference is who empties it. The real one is
//! emptied by the kernel, on its own schedule, and answers whenever it likes. This one is
//! emptied by the simulator, at an instant it chose, and answers when the calendar says a
//! device or a peer would have. Everything above the seam submits and reaps exactly as it
//! does in production and cannot tell which it is talking to.
//!
//! The registration tables are modeled rather than ignored because they are not
//! bookkeeping: a submission names a file and a buffer by index, so a ring that did not
//! keep the tables could not say what a submission meant.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::io;
use std::os::fd::RawFd;
use std::rc::Rc;

use super::{Event, Sim};
use crate::kernel::FileRef;
use crate::kernel::ring::{Cqe, Op, Sqe, Timespec};

/// What a transfer against a simulated device costs.
///
/// One number, because a store here is not a disk with a queue and a head: it is the
/// promise that a read costs something, so a node that waits on one is seen to wait.
const IO_US: u64 = 50;

/// What a frame costs to reach a peer, one way. The fabric is a wire, and a wire is the
/// one part of a cluster whose cost is not the node's to spend.
const LINK_US: u64 = 50;

/// What a copy between a request's guest pages and a worker's buffer costs. Small, because
/// it is memory both ends of the way, but not nothing: two copies against one page still
/// have an inside.
const COPY_US: u64 = 1;

/// What a simulated ring answers when asked for a descriptor. A simulated ring has none,
/// and the doorbell knows not to post to it.
pub(crate) const NO_FD: RawFd = -1;

/// The ring itself, which the simulator reaches through a weak handle and the owner
/// reaches through [`Ring`]. Split in two so that a ring the simulator has a completion
/// for cannot outlive the worker it belongs to: the worker holds the only strong
/// reference, and a crashed node drops it.
pub(crate) struct Inner {
    /// Where the simulator finds this ring in its table, assigned when it enrols.
    id: Cell<u32>,
    /// How many submissions fit before pushes start failing, which is the whole of the
    /// backpressure the node feels from its ring.
    entries: u32,
    sq: RefCell<VecDeque<Sqe>>,
    cq: RefCell<VecDeque<Cqe>>,
    files: RefCell<Vec<crate::kernel::FileRef>>,
    bufs: RefCell<Vec<Option<(u64, usize)>>>,
    /// Set while the simulator has completions the owner has not collected, which is what
    /// the real ring's task-run flag reports.
    woken: Cell<bool>,
}

pub(crate) struct Ring {
    inner: Rc<Inner>,
    /// The model this ring answers from. Held rather than looked up, because a ring is
    /// reaped on the hot path and the thread it belongs to never changes.
    sim: Rc<Sim>,
}

pub(crate) fn open(s: &Rc<Sim>, entries: u32, _cq_entries: u32, _polled: bool) -> io::Result<Ring> {
    let inner = Rc::new(Inner {
        id: Cell::new(0),
        entries,
        sq: RefCell::new(VecDeque::new()),
        cq: RefCell::new(VecDeque::new()),
        files: RefCell::new(Vec::new()),
        bufs: RefCell::new(Vec::new()),
        woken: Cell::new(false),
    });
    let inner = s.enroll_ring(inner);
    Ok(Ring {
        inner,
        sim: s.clone(),
    })
}

impl Inner {
    pub(crate) fn id(&self) -> u32 {
        self.id.get()
    }

    pub(crate) fn set_id(&self, id: u32) {
        self.id.set(id);
    }

    /// Answers a submission. The owner sees it the next time it reaps.
    pub(crate) fn post(&self, user_data: u64, result: i32) {
        self.cq.borrow_mut().push_back(Cqe { user_data, result });
        self.woken.set(true);
    }

    /// The descriptor registered at `index`, which is what a submission naming a fixed
    /// file is asking for.
    pub(crate) fn file(&self, index: u32) -> Option<crate::kernel::FileRef> {
        self.files
            .borrow()
            .get(index as usize)
            .copied()
            .filter(|f| *f != crate::kernel::FileRef::NONE)
    }

    /// The address and length registered at `index`.
    /// Put a buffer into the table, or take one out. This is what automatic registration
    /// does when a fetch completes: the pages a request arrived with become the buffer the
    /// submission that serves it will name.
    pub(crate) fn set_buffer(&self, index: u32, b: Option<(u64, usize)>) {
        let mut table = self.bufs.borrow_mut();
        if let Some(slot) = table.get_mut(index as usize) {
            *slot = b;
        }
    }

    pub(crate) fn buffer(&self, index: u32) -> Option<(u64, usize)> {
        self.bufs.borrow().get(index as usize).copied().flatten()
    }
}

impl Ring {
    pub(crate) fn push(&self, e: &Sqe) -> bool {
        let mut sq = self.inner.sq.borrow_mut();
        if sq.len() >= self.inner.entries as usize {
            return false;
        }
        sq.push_back(*e);
        true
    }

    pub(crate) fn room(&self) -> usize {
        self.inner.entries as usize - self.inner.sq.borrow().len()
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.inner.sq.borrow().is_empty()
    }

    /// Hands everything queued to the simulator.
    ///
    /// Nothing has happened yet: each submission is read for what it asks, and what it
    /// asks for is put on the calendar at the instant it would have been answered. A
    /// submission takes effect when time reaches it, which is the point of the exercise.
    pub(crate) fn submit(&self) {
        let queued: Vec<Sqe> = self.inner.sq.borrow_mut().drain(..).collect();
        let mut i = 0;
        while i < queued.len() {
            let e = queued[i];
            // A linked submission is guarded by the timeout that follows it, and the two
            // are one decision: whichever is owed sooner happens, and the other is told it
            // was canceled. Deciding it here is exact, because a simulated device says
            // what it will cost before it starts.
            let guard = e
                .link
                .then(|| queued.get(i + 1))
                .flatten()
                .and_then(|g| match g.op {
                    Op::LinkTimeout { ts } => Some((*g, unsafe { (*ts).as_micros() })),
                    _ => None,
                });
            match guard {
                Some((g, deadline)) => {
                    i += 2;
                    match self.plan(&e) {
                        Some((at, ev)) if at <= deadline => {
                            self.sim.at(at, ev);
                            self.sim.at(at, self.cqe(g.user_data, -libc::ECANCELED));
                        }
                        Some(_) => {
                            self.sim.at(deadline, self.cqe(g.user_data, -libc::ETIME));
                            self.sim
                                .at(deadline, self.cqe(e.user_data, -libc::ECANCELED));
                        }
                        None => {
                            self.sim.at(0, self.cqe(e.user_data, -libc::EBADF));
                            self.sim.at(0, self.cqe(g.user_data, -libc::ECANCELED));
                        }
                    }
                }
                None => {
                    i += 1;
                    match self.plan(&e) {
                        Some((at, ev)) => self.sim.at(at, ev),
                        None => self.sim.at(0, self.cqe(e.user_data, -libc::EBADF)),
                    }
                }
            }
        }
    }

    fn cqe(&self, user_data: u64, result: i32) -> Event {
        Event::Cqe {
            ring: self.inner.id(),
            user_data,
            result,
        }
    }

    /// What a submission will do, and how long it takes to do it.
    ///
    /// `None` means the submission named something this ring does not have, which the real
    /// kernel reports as `EBADF` and so does this one.
    fn plan(&self, e: &Sqe) -> Option<(u64, Event)> {
        let ring = self.inner.id();
        match e.op {
            // A bare timeout is the runtime's only clock: it expires, and expiring is all
            // it ever does.
            Op::Timeout { ts } => Some((
                unsafe { (*ts).as_micros() },
                self.cqe(e.user_data, -libc::ETIME),
            )),
            // Only ever seen as a guard, which `submit` has already paired off. One that
            // arrives alone guards nothing and expires on its own.
            Op::LinkTimeout { ts } => Some((
                unsafe { (*ts).as_micros() },
                self.cqe(e.user_data, -libc::ETIME),
            )),
            Op::Read {
                file,
                buf,
                len,
                offset,
            } => self.transfer(ring, e.user_data, file, true, offset, buf as u64, len, None),
            Op::Write {
                file,
                buf,
                len,
                offset,
            } => self.transfer(
                ring,
                e.user_data,
                file,
                false,
                offset,
                buf as u64,
                len,
                None,
            ),
            Op::ReadFixed {
                file,
                buf,
                len,
                buf_index,
                offset,
            } => self.transfer(
                ring,
                e.user_data,
                file,
                true,
                offset,
                buf as u64,
                len,
                Some(buf_index),
            ),
            Op::WriteFixed {
                file,
                buf,
                len,
                buf_index,
                offset,
                ..
            } => self.transfer(
                ring,
                e.user_data,
                file,
                false,
                offset,
                buf as u64,
                len,
                Some(buf_index),
            ),
            // The ublk data plane. What comes back is the driver's business, decided when
            // the calendar reaches this, because a fetch may find nothing to do and wait.
            Op::UringCmd16 {
                file,
                cmd_op,
                cmd,
                addr,
            } => {
                let FileRef::Sim(dev) = self.inner.file(file)? else {
                    return None;
                };
                Some((
                    0,
                    Event::Ublk {
                        ring,
                        user_data: e.user_data,
                        dev,
                        cmd_op,
                        cmd,
                        // The buffer slot rides in the submission's address, packed the
                        // way `ublk_auto_buf_reg` packs it: index in the low sixteen bits.
                        index: addr.unwrap_or(0) as u16,
                    },
                ))
            }
            // A doorbell. A simulated ring has no descriptor to be rung through, so
            // nothing ever submits one; a worker that would have been woken finds its work
            // when its park expires instead.
            Op::MsgRingData { .. } => Some((0, self.cqe(e.user_data, 0))),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn transfer(
        &self,
        ring: u32,
        user_data: u64,
        file: u32,
        read: bool,
        off: u64,
        buf: u64,
        len: u32,
        buf_index: Option<u16>,
    ) -> Option<(u64, Event)> {
        let FileRef::Sim(dev) = self.inner.file(file)? else {
            return None;
        };
        // A submission carrying no address is naming its buffer by index alone, which is
        // what a registered buffer is for.
        let addr = if buf != 0 {
            buf
        } else {
            self.inner.buffer(buf_index? as u32)?.0
        };
        // A fabric handle is not a store either: it is a peer, and an offset in it names a
        // page on the other end. What the node wrote is a frame, not a block.
        if !crate::kernel::sim::is_char_dev(dev) && self.sim.dev_fabric(dev) {
            return Some((
                LINK_US,
                Event::Frame {
                    ring,
                    user_data,
                    from: self.sim.node(),
                    node: self.sim.dev_node(dev),
                    read,
                    lba: off / crate::kernel::sim::store::BLOCK as u64,
                    addr,
                    len,
                },
            ));
        }
        // A char device is not a store: an offset in it names a request's guest pages, and
        // reaching them is a copy, not a transfer.
        if crate::kernel::sim::is_char_dev(dev) {
            return Some((
                COPY_US,
                Event::Guest {
                    ring,
                    user_data,
                    dev,
                    read,
                    pos: off,
                    addr,
                    len,
                    // Which addressing mode the submission used is the driver's business,
                    // not the ring's: `ublk_drv` refuses a registered buffer outright.
                    fixed: buf_index.is_some(),
                },
            ));
        }
        Some((
            IO_US,
            Event::Transfer {
                ring,
                user_data,
                dev,
                read,
                off,
                addr,
                len,
            },
        ))
    }

    pub(crate) fn taskrun(&self) -> bool {
        self.inner.woken.get()
    }

    pub(crate) fn get_events(&self) {
        self.inner.woken.set(false);
    }

    pub(crate) fn drain(&self, out: &mut Vec<Cqe>) {
        self.inner.woken.set(false);
        out.extend(self.inner.cq.borrow_mut().drain(..));
    }

    /// Waits for a completion, for at most `ts`.
    ///
    /// Parking is where a worker stops being the reason the simulation is not moving, so
    /// this is the point the scheduler gets a turn. If nothing anywhere can run, time
    /// passes to the deadline and the worker comes round again having found nothing,
    /// which is exactly what the real one does.
    pub(crate) fn wait(&self, ts: &Timespec) {
        // Parking is a turn ending, not a turn blocking: the worker answers `Idle` and
        // the scheduler moves the clock once every fiber has said the same. Pumping from
        // in here would be a worker running its neighbors from inside its own turn.
        let _ = ts;
        self.inner.woken.set(false);
    }

    pub(crate) fn register_files_sparse(&self, n: u32) -> io::Result<()> {
        *self.inner.files.borrow_mut() = vec![crate::kernel::FileRef::NONE; n as usize];
        Ok(())
    }

    pub(crate) fn register_files_update(
        &self,
        off: u32,
        fds: &[crate::kernel::FileRef],
    ) -> io::Result<()> {
        let mut files = self.inner.files.borrow_mut();
        for (i, &fd) in fds.iter().enumerate() {
            let at = off as usize + i;
            if at >= files.len() {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            files[at] = fd;
        }
        Ok(())
    }

    pub(crate) fn register_buffers_sparse(&self, n: u32) -> io::Result<()> {
        *self.inner.bufs.borrow_mut() = vec![None; n as usize];
        Ok(())
    }

    pub(crate) fn register_buffers_update(&self, off: u32, bufs: &[libc::iovec]) -> io::Result<()> {
        let mut table = self.inner.bufs.borrow_mut();
        for (i, v) in bufs.iter().enumerate() {
            let at = off as usize + i;
            if at >= table.len() {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            table[at] = Some((v.iov_base as u64, v.iov_len));
        }
        Ok(())
    }

    pub(crate) fn as_raw_fd(&self) -> RawFd {
        NO_FD
    }

    // -- what the simulator drives ------------------------------------------

    /// Where the simulator knows this ring by.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn id(&self) -> u32 {
        self.inner.id.get()
    }

    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn post(&self, user_data: u64, result: i32) {
        self.inner.post(user_data, result);
    }

    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn file(&self, index: u32) -> Option<crate::kernel::FileRef> {
        self.inner.file(index)
    }

    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn buffer(&self, index: u32) -> Option<(u64, usize)> {
        self.inner.buffer(index)
    }
}

// SAFETY: a ring belongs to one thread at a time, exactly as the real one does. What
// makes this one look otherwise is that a submission carries the address of the buffer it
// names, and an address is not `Send`. Those addresses belong to the thread that pushed
// them and are read by nobody else; the only move that ever happens is handing a freshly
// built ring to the fiber about to own it.
unsafe impl Send for Ring {}
