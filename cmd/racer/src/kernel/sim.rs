//! The simulated kernel: an in-process model of what the kernel would have done.
//!
//! One `Sim` is shared by every fiber of a simulated cluster, which is why the interior is
//! `Cell` rather than atomics: there is only ever one thread. It is deliberately the same
//! handle a test holds, so a test can advance the clock, and the code under test cannot
//! tell the difference between that and time passing.

pub(crate) mod ring;
pub(crate) mod store;
pub(crate) mod ublk;

use std::cell::{Cell, RefCell};
use std::cmp::Reverse;
use std::collections::{BTreeMap, BTreeSet, BinaryHeap};
use std::io;
use std::path::Path;
use std::rc::{Rc, Weak};
use std::sync::mpsc::{Receiver, RecvError, RecvTimeoutError, TryRecvError};
use std::time::{Duration, Instant};

use store::Store;
use ublk::Ublk;

/// A thread of a simulated node: its name, what it does, and whether it has begun.
struct Fiber {
    /// Stable for the fiber's life, unlike its place in the table.
    ///
    /// A fiber that blocks pumps the others, and what it runs may retire a fiber ahead
    /// of it. Naming a fiber by where it sits would then name a different one, or none.
    id: u64,
    name: String,
    /// The node that started it. A thread belongs to whoever spawned it, and that is what
    /// decides whose counters it touches and whose tables it reports into when it runs.
    node: u32,
    /// Absent while this fiber is taking its turn.
    ///
    /// A fiber that blocks pumps the others, and the one thing it must not do is step
    /// itself: its turn is already on the stack. Lifting the task out for the duration of
    /// its turn is what makes that impossible rather than merely discouraged.
    task: Option<Box<dyn super::Task>>,
    started: bool,
}

/// How many rounds a blocking receive will pump before calling it a deadlock.
///
/// A simulated node is one thread, so a receive that nothing can satisfy would otherwise
/// spin for ever. This is not a timeout: it is a bug report with a bound on how long it
/// takes to write itself.
const PUMP_LIMIT: u32 = 1 << 20;

/// Something the kernel has undertaken to do at a virtual instant.
///
/// This is the whole of what the calendar carries. Anything richer - a frame crossing the
/// fabric, a guest copy - reaches the node as one of these in the end, because a
/// completion is the only thing a ring can be told.
pub(crate) enum Event {
    /// Answer a submission on the ring the simulator knows by `ring`.
    Cqe {
        ring: u32,
        user_data: u64,
        result: i32,
    },
    /// Move bytes between a device and a worker's buffer, then answer the submission that
    /// asked for it.
    ///
    /// The bytes move here, at the instant the transfer completes, rather than when it was
    /// submitted. That is what makes two overlapping transfers to one block interleave the
    /// way a device would let them, and it is the difference between a store that models a
    /// disk and one that merely remembers writes.
    Transfer {
        ring: u32,
        user_data: u64,
        dev: u32,
        read: bool,
        off: u64,
        addr: u64,
        len: u32,
    },
    /// A data-plane command for the ublk driver: a fetch, or a commit and fetch. What
    /// comes back is decided when this fires, not when it was submitted, because a fetch
    /// that finds nothing to do is answered by the next request to arrive.
    Ublk {
        ring: u32,
        user_data: u64,
        dev: u32,
        cmd_op: u32,
        cmd: [u8; 16],
        index: u16,
    },
    /// Carry a transfer to a peer's fabric device, which is what one node asking another
    /// for a page is. It arrives there as a request like any other, and the answer comes
    /// back to the submission that sent it.
    Frame {
        ring: u32,
        user_data: u64,
        /// The node that sent it. Carried rather than asked for, because by the time a
        /// frame is delivered the run has moved on and whoever is running is not it.
        from: u32,
        node: u32,
        read: bool,
        lba: u64,
        addr: u64,
        len: u32,
    },
    /// Move bytes between a request's guest pages and a worker's buffer, then answer the
    /// submission that asked for it.
    Guest {
        ring: u32,
        user_data: u64,
        dev: u32,
        read: bool,
        pos: u64,
        addr: u64,
        len: u32,
        /// Whether the submission named its buffer by registered index. The driver refuses
        /// one that did, so this decides whether the copy happens at all.
        fixed: bool,
    },
}

/// What a run is asked to go wrong with.
///
/// Every rate is per mille of the operations it applies to, so a run with none of these
/// set is a run on hardware that never fails. They are set from a test rather than drawn,
/// because a fault that only happens sometimes is a fault that is only tested sometimes.
#[derive(Clone, Debug, Default)]
pub struct Faults {
    /// Transfers against a store that come back `EIO`.
    pub io_error: u32,
    /// Reads that come back with a byte changed and nothing said, which is what a disk
    /// that has rotted does.
    pub corrupt: u32,
    /// Frames that never arrive, leaving the sender to find out by timing out.
    pub drop: u32,
    /// Pairs of nodes that cannot reach each other. Held both ways round, so a partition
    /// that only cuts one direction is expressible by holding only one.
    pub cut: BTreeSet<(u32, u32)>,
}

/// A path a run reached, counted so a test can insist it was reached.
///
/// A fault that is configured and never happens is a test that passes for the wrong
/// reason, so what a run went through is as much a result as what it answered.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Hit {
    /// A transfer against a store was refused.
    IoError,
    /// A read came back with a byte changed.
    Corrupt,
    /// A frame was dropped on the way out.
    Drop,
    /// A frame was refused because the two nodes cannot reach each other.
    Cut,
    /// A frame arrived at the peer it named.
    Frame,
}

impl Hit {
    /// Every one of them, which is also how many counters there are.
    pub const ALL: [Hit; 5] = [Hit::IoError, Hit::Corrupt, Hit::Drop, Hit::Cut, Hit::Frame];
}

/// An event and when it is due.
///
/// Ordered by `(at, seq)`, which is total, so two events scheduled for the same instant
/// happen in the order they were scheduled and the heap's tie-breaking cannot affect a
/// run.
struct Timed {
    at: u64,
    seq: u64,
    ev: Event,
}

impl PartialEq for Timed {
    fn eq(&self, o: &Timed) -> bool {
        (self.at, self.seq) == (o.at, o.seq)
    }
}
impl Eq for Timed {}
impl PartialOrd for Timed {
    fn partial_cmp(&self, o: &Timed) -> Option<std::cmp::Ordering> {
        Some(self.cmp(o))
    }
}
impl Ord for Timed {
    fn cmp(&self, o: &Timed) -> std::cmp::Ordering {
        (self.at, self.seq).cmp(&(o.at, o.seq))
    }
}

/// The page size every simulated node sees, whatever the host's is.
///
/// A run that reproduces on one machine has to reproduce on the next, and the ublk
/// descriptor stride is computed from this, so it cannot be asked of the host.
const PAGE: usize = 4096;

pub(crate) struct Sim {
    /// Virtual microseconds since the run began. Moves only when the simulator says so.
    now: Cell<u64>,
    /// The host instant the run is anchored to. Virtual time is reported as an offset from
    /// here because `Instant` is opaque and cannot be built from a number.
    base: Instant,
    /// How many logical CPUs the node being built believes it has. The simulator sets this
    /// before booting a node, and a node takes one worker per CPU.
    cpus: Cell<usize>,
    /// Which node is running. Every seam that asks "whose?" asks this.
    ///
    /// A real process is one node, so the question never comes up there. Here one process
    /// is a cluster, and the answer changes as the scheduler hands out turns.
    here: Cell<u32>,
    /// The last fiber id handed out. Ids start at one, so zero is before them all.
    next_fiber: Cell<u64>,
    /// The counters each node keeps while it runs, which a real node keeps in statics.
    ///
    /// Per node rather than per process, so two nodes in one address space neither sum
    /// their counters nor refuse to start because the other already has.
    counters: RefCell<Vec<[u64; super::COUNTERS]>>,
    /// Every block device in the run, and the paths they answer to.
    store: RefCell<Store>,
    /// What this run is asked to go wrong with.
    faults: RefCell<Faults>,
    /// What it has gone through, by [`Hit`].
    hits: Cell<[u64; Hit::ALL.len()]>,
    /// The whole of a run's nondeterminism. Nothing above the seam draws a number, so a
    /// run with the same seed and the same calls goes the same way every time.
    rng: Cell<u64>,
    /// Which minor each node exports its fabric device at.
    ///
    /// A peer is reached by writing to a device whose path names it, and this is the only
    /// place that path is turned back into the node on the other end. The driver's table
    /// is keyed by minor and knows nothing of nodes, so the cluster says.
    fabric: RefCell<BTreeMap<u32, u32>>,
    /// The ublk driver's device table: which minors are taken and who took them.
    ublk: RefCell<Ublk>,
    /// Anonymous mappings this kernel handed out, so that unmapping a window on a device
    /// does not free memory the device owns.
    anon: RefCell<BTreeSet<usize>>,

    /// Threads this node has started that the scheduler has not given a turn yet.
    ///
    /// A simulated thread is a fiber, and a fiber does not run because it was created: it
    /// runs when the simulation says so. Queuing them here is what makes a boot a sequence
    /// of steps rather than a race the host's scheduler arbitrates.
    fibers: RefCell<Vec<Fiber>>,
    /// Everything the kernel has undertaken to do, earliest first.
    ///
    /// Time does not pass here, it is spent: the clock stands still while any fiber has
    /// work, and jumps to the next entry when none has. So a run costs what the cluster
    /// waited for and nothing else, and it costs the same every time.
    calendar: RefCell<BinaryHeap<Reverse<Timed>>>,
    /// Scheduling order, which breaks ties in the calendar.
    seq: Cell<u64>,
    /// Every ring in the run. Weak, because the ring belongs to the worker that opened it:
    /// a node that crashed leaves entries here that answer nobody, which is exactly what
    /// a completion arriving for a dead worker should do.
    rings: RefCell<Vec<Weak<ring::Inner>>>,
}

impl Sim {
    pub(crate) fn new() -> Rc<Sim> {
        Rc::new(Sim {
            now: Cell::new(0),
            base: Instant::now(),
            cpus: Cell::new(1),
            here: Cell::new(0),
            next_fiber: Cell::new(0),
            counters: RefCell::new(Vec::new()),
            store: RefCell::new(Store::default()),
            faults: RefCell::new(Faults::default()),
            hits: Cell::new([0; Hit::ALL.len()]),
            rng: Cell::new(1),
            fabric: RefCell::new(BTreeMap::new()),
            ublk: RefCell::new(Ublk::default()),
            anon: RefCell::new(BTreeSet::new()),
            fibers: RefCell::new(Vec::new()),
            calendar: RefCell::new(BinaryHeap::new()),
            seq: Cell::new(0),
            rings: RefCell::new(Vec::new()),
        })
    }

    pub(crate) fn now_us(&self) -> u64 {
        self.now.get()
    }

    /// Advance to `us`. Time never goes backwards, so a caller that is already past it is
    /// left where it is rather than rewound.
    pub(crate) fn advance_to(&self, us: u64) {
        self.now.set(us.max(self.now.get()));
    }

    pub(crate) fn clock(&self) -> Instant {
        self.base + Duration::from_micros(self.now.get())
    }

    /// A thread that parks for `d`.
    ///
    /// A sleeping fiber is not the reason the simulation is standing still, so the rest of
    /// it runs while this one waits. What the sleeper is promised is only that it does not
    /// come back before its time, which is all a real sleep promises either.
    pub(crate) fn sleep_blocking(&self, d: Duration) {
        let until = self.now.get().saturating_add(d.as_micros() as u64);
        while self.now_us() < until {
            self.idle_until(until);
        }
    }

    /// How many CPUs the next node to boot will find.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn set_cpus(&self, n: usize) {
        assert!(n != 0, "a node needs at least one CPU");
        self.cpus.set(n);
    }

    pub(crate) fn page_size(&self) -> usize {
        PAGE
    }

    /// The CPUs the node may run on: `0..cpus`, with no SMT.
    ///
    /// A simulated worker is a fiber, so the topology is a description of how many of them
    /// to make rather than a description of any hardware. Reporting no siblings keeps the
    /// control thread from parking itself on a CPU that does not exist.
    pub(crate) fn affinity(&self) -> io::Result<Vec<usize>> {
        Ok((0..self.cpus.get()).collect())
    }

    pub(crate) fn siblings(&self, cpu: usize) -> Option<Vec<usize>> {
        (cpu < self.cpus.get()).then(|| vec![cpu])
    }

    /// Pinning a fiber to a CPU is a promise about a thread that does not exist.
    pub(crate) fn pin(&self, _cpu: usize) -> io::Result<()> {
        Ok(())
    }

    /// Anonymous memory, plainly.
    ///
    /// Memory is the one kernel resource worth taking for real: the runtime genuinely
    /// reads and writes these bytes, and a model of them would be the bytes themselves.
    /// What is dropped is the parts a simulation must not depend on - huge pages, which
    /// need a host pool the test machine may not have, and the first-touch pass, which
    /// fixes a NUMA node no fiber will ever migrate away from.
    pub(crate) fn map_anon(&self, len: usize) -> io::Result<*mut u8> {
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if ptr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }

        self.anon.borrow_mut().insert(ptr as usize);

        Ok(ptr as *mut u8)
    }

    /// A read-only window on a simulated file.
    ///
    /// The only caller is a ublk queue's descriptor array, so this hands back the driver's
    /// own memory for that queue. The node reads it and the driver writes it, which is
    /// exactly the arrangement the mapping stands for.
    pub(crate) fn map_read(&self, id: u32, offset: u64, len: usize) -> io::Result<*mut u8> {
        match cdev_minor(id) {
            Some(minor) => self.ublk.borrow_mut().descs(minor, offset, len),
            None => Err(io::Error::from_raw_os_error(libc::ENODEV)),
        }
    }

    /// # Safety
    ///
    /// `ptr` and `len` must name a mapping this module returned.
    ///
    /// A window on a device is not one of them: the driver's descriptor memory belongs to
    /// the driver, and the node letting go of its view of it is not a reason to free it.
    pub(crate) unsafe fn unmap(&self, ptr: *mut u8, len: usize) {
        if self.anon.borrow_mut().remove(&(ptr as usize)) {
            unsafe { libc::munmap(ptr as *mut libc::c_void, len) };
        }
    }

    /// Opens a simulated device by path, creating it the first time.
    ///
    /// There is no create flag and no mode: a simulated store exists because
    /// the topology says the node has one, and a path that names no device is
    /// `ENOENT` just as it would be on a host.
    pub(crate) fn open(&self, path: &Path) -> io::Result<u32> {
        if let Some(minor) = char_dev_minor(path) {
            if !self.ublk.borrow().holds(minor) {
                return Err(io::Error::from_raw_os_error(libc::ENOENT));
            }
            return Ok(CDEV_BASE + minor);
        }
        self.store.borrow_mut().open(path)
    }

    /// The size the node last asked for, which is what `stat` would report.
    pub(crate) fn file_len(&self, id: u32) -> u64 {
        self.store.borrow().len(id)
    }

    /// Sizes a store. Grow-only, exactly as `fallocate` on a real one is.
    pub(crate) fn resize(&self, id: u32, want: u64) -> io::Result<()> {
        self.store.borrow_mut().resize(id, want)
    }

    /// Reads a block-aligned byte range.
    pub(crate) fn read_at(&self, id: u32, off: u64, out: &mut [u8]) {
        self.store.borrow().read_at(id, off, out);
    }

    /// Writes a block-aligned byte range.
    pub(crate) fn write_at(&self, id: u32, off: u64, src: &[u8]) {
        self.store.borrow_mut().write_at(id, off, src);
    }

    /// The node a device belongs to.
    pub(crate) fn dev_node(&self, id: u32) -> u32 {
        self.store.borrow().dev(id).node
    }

    /// Whether a device is a handle on a peer rather than a store of our own.
    pub(crate) fn dev_fabric(&self, id: u32) -> bool {
        self.store.borrow().dev(id).fabric
    }

    /// The node whose turn it is.
    pub(crate) fn node(&self) -> u32 {
        self.here.get()
    }

    /// Makes `id` the node that is running, and answers with the one that was.
    ///
    /// The driver brackets a node's boot in this, so everything the boot reaches - the
    /// counters, the metrics tables, the fibers it starts - is attributed to it.
    pub(crate) fn enter_node(&self, id: u32) -> u32 {
        self.here.replace(id)
    }

    /// This node's counter row, grown to reach it.
    fn row<R>(&self, f: impl FnOnce(&mut [u64; super::COUNTERS]) -> R) -> R {
        let node = self.here.get() as usize;
        let mut rows = self.counters.borrow_mut();
        if rows.len() <= node {
            rows.resize(node + 1, [0; super::COUNTERS]);
        }

        f(&mut rows[node])
    }

    pub(crate) fn counter(&self, i: usize) -> u64 {
        self.row(|r| r[i])
    }

    pub(crate) fn set_counter(&self, i: usize, v: u64) {
        self.row(|r| r[i] = v);
    }

    pub(crate) fn add_counter(&self, i: usize, v: u64) {
        self.row(|r| r[i] = r[i].saturating_add(v));
    }

    pub(crate) fn swap_counter(&self, i: usize, v: u64) -> u64 {
        self.row(|r| std::mem::replace(&mut r[i], v))
    }

    /// Queues a fiber for the scheduler.
    ///
    /// Nothing runs here. A real `spawn` returns with the thread already racing the
    /// caller, which is exactly the nondeterminism a simulation exists to remove, so a
    /// simulated node's threads wait until the run gives them a turn.
    pub(crate) fn spawn(&self, name: String, task: Box<dyn super::Task>) -> io::Result<()> {
        let id = self.next_fiber.get() + 1;
        self.next_fiber.set(id);
        self.fibers.borrow_mut().push(Fiber {
            id,
            name,
            node: self.here.get(),
            task: Some(task),
            started: false,
        });
        Ok(())
    }

    /// How many fibers are waiting for their first turn.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn fibers(&self) -> usize {
        self.fibers.borrow().len()
    }

    /// The name of the fiber queued at `i`, for a test that wants to see the shape of a
    /// boot without running it.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn fiber_name(&self, i: usize) -> Option<String> {
        self.fibers.borrow().get(i).map(|f| f.name.clone())
    }

    /// Gives every fiber but the caller's own one turn, and retires the ones that finish.
    ///
    /// Answers how many turns were taken. Zero means there is nobody left to make
    /// progress, which is the only honest answer to give a caller that is waiting on one
    /// of them.
    ///
    /// A fiber started here rather than at spawn: a thread's first act runs on the thread
    /// itself, and for a worker that is where its ring, buffer pool and thread locals are
    /// built.
    /// Gives every fiber a turn, and if none of them could do anything, lets time pass to
    /// the next thing the kernel owes and does it.
    ///
    /// Zero means the simulation is over or stuck: nobody can run, and nothing is due.
    pub(crate) fn pump(&self) -> usize {
        self.pump_until(u64::MAX)
    }

    /// As [`Sim::pump`], but will not let the clock pass `limit`.
    ///
    /// A caller with a deadline of its own has to be able to stop time at it, or a
    /// timeout would report whatever the calendar happened to hold rather than what the
    /// caller asked about.
    pub(crate) fn pump_until(&self, limit: u64) -> usize {
        let turns = self.turn_fibers();
        if turns > 0 {
            return turns;
        }
        self.fire_due(limit)
    }

    /// One round of turns, in fiber order.
    fn turn_fibers(&self) -> usize {
        let mut turns = 0;
        // Ids already given a turn this round. Walking by id rather than by position is
        // what makes the walk survive a nested pump retiring or starting a fiber.
        let mut after = 0;
        // A fiber whose task is absent is one already taking its turn further up the
        // stack, and it is not this pump's to step.
        while let Some((id, node, started)) = self.next_fiber_after(after) {
            after = id;

            let Some(mut task) = self.detach(id) else {
                continue;
            };
            // A turn runs as the node that started the thread, and gives back whoever
            // was running: a fiber that blocks pumps from inside, so turns nest.
            let was = self.here.replace(node);
            if !started {
                task.start();
                self.with_fiber(id, |f| f.started = true);
            }
            let outcome = task.turn();
            // Only a turn that did something counts. A round in which every fiber went
            // idle is how the caller learns the run is waiting on the clock.
            if outcome == super::Turn::Ran {
                turns += 1;
            }
            if outcome != super::Turn::Done {
                self.with_fiber(id, |f| f.task = Some(task));
            } else {
                task.finish();
                drop(task);
                self.fibers.borrow_mut().retain(|f| f.id != id);
            }
            self.here.set(was);
        }
        turns
    }

    /// Lifts a fiber's task out, if it is there to be lifted.
    /// The next fiber a pump should step, by id rather than by position.
    fn next_fiber_after(&self, after: u64) -> Option<(u64, u32, bool)> {
        self.fibers
            .borrow()
            .iter()
            .find(|f| f.id > after && f.task.is_some())
            .map(|f| (f.id, f.node, f.started))
    }

    fn detach(&self, id: u64) -> Option<Box<dyn super::Task>> {
        let mut fibers = self.fibers.borrow_mut();
        fibers.iter_mut().find(|f| f.id == id)?.task.take()
    }

    /// Applies `f` to the fiber `id` names, if it is still there.
    fn with_fiber(&self, id: u64, f: impl FnOnce(&mut Fiber)) {
        let mut fibers = self.fibers.borrow_mut();
        if let Some(fiber) = fibers.iter_mut().find(|fiber| fiber.id == id) {
            f(fiber);
        }
    }

    /// Puts `ring` in the table, so a completion can find its way back to it.
    pub(crate) fn enroll_ring(&self, inner: Rc<ring::Inner>) -> Rc<ring::Inner> {
        let mut rings = self.rings.borrow_mut();
        inner.set_id(rings.len() as u32);
        rings.push(Rc::downgrade(&inner));
        inner
    }

    /// Undertakes to do `ev` once `delay_us` of virtual time has gone by.
    pub(crate) fn at(&self, delay_us: u64, ev: Event) {
        let seq = self.seq.get();
        self.seq.set(seq + 1);
        self.calendar.borrow_mut().push(Reverse(Timed {
            at: self.now_us().saturating_add(delay_us),
            seq,
            ev,
        }));
    }

    /// When the next thing the kernel owes is due.
    pub(crate) fn next_due(&self) -> Option<u64> {
        self.calendar.borrow().peek().map(|Reverse(t)| t.at)
    }

    /// Spends time up to the next due instant and does everything owed at it.
    ///
    /// Everything at one instant fires together, in scheduling order, because a virtual
    /// instant has no inside: two completions the kernel owes at the same microsecond are
    /// as simultaneous as two the real one posts in one batch.
    fn fire_due(&self, limit: u64) -> usize {
        let Some(at) = self.next_due().filter(|at| *at <= limit) else {
            return 0;
        };
        self.advance_to(at);
        let mut n = 0;
        loop {
            let ev = {
                let mut cal = self.calendar.borrow_mut();
                match cal.peek() {
                    Some(Reverse(t)) if t.at == at => cal.pop().map(|Reverse(t)| t.ev),
                    _ => None,
                }
            };
            let Some(ev) = ev else { return n };
            self.deliver(ev);
            n += 1;
        }
    }

    /// Carry out what the driver decided, on whichever ring each part of it names.
    fn apply(&self, actions: Vec<ublk::Action>) {
        for a in actions {
            let (ring, index, buf) = match a {
                ublk::Action::Register {
                    ring,
                    index,
                    addr,
                    len,
                } => (ring, index, Some((addr, len))),
                ublk::Action::Unregister { ring, index } => (ring, index, None),
                ublk::Action::Post {
                    ring,
                    user_data,
                    result,
                } => {
                    if let Some(r) = self.ring(ring) {
                        r.post(user_data, result);
                    }
                    continue;
                }
            };
            if let Some(r) = self.ring(ring) {
                r.set_buffer(index as u32, buf);
            }
        }
    }

    fn ring(&self, id: u32) -> Option<Rc<ring::Inner>> {
        self.rings.borrow().get(id as usize).and_then(Weak::upgrade)
    }

    fn deliver(&self, ev: Event) {
        let (ring, user_data) = match &ev {
            Event::Cqe {
                ring, user_data, ..
            } => (*ring, *user_data),
            Event::Transfer {
                ring, user_data, ..
            } => (*ring, *user_data),
            Event::Guest {
                ring, user_data, ..
            } => (*ring, *user_data),
            // A frame is answered by the peer that receives it, whenever that is, so
            // there is nothing to post now.
            Event::Frame {
                ring,
                user_data,
                from,
                node,
                read,
                lba,
                addr,
                len,
            } => {
                self.frame(*ring, *user_data, *from, *node, *read, *lba, *addr, *len);
                return;
            }
            // A ublk command answers on whichever ring parked the fetch, which need not be
            // this one, so it is applied rather than posted.
            Event::Ublk {
                dev,
                cmd_op,
                cmd,
                index,
                ring,
                user_data,
            } => {
                let Some(minor) = cdev_minor(*dev) else {
                    return;
                };
                let actions = self
                    .ublk
                    .borrow_mut()
                    .io(minor, *cmd_op, cmd, *index, *ring, *user_data);
                self.apply(actions);
                return;
            }
        };
        // A ring whose worker is gone is a node that crashed. The completion is dropped,
        // and so is the transfer: memory a dead worker owned is nobody's to touch.
        let Some(r) = self
            .rings
            .borrow()
            .get(ring as usize)
            .and_then(Weak::upgrade)
        else {
            return;
        };
        let result = match ev {
            Event::Cqe { result, .. } => result,
            Event::Transfer {
                dev,
                read,
                off,
                addr,
                len,
                ..
            } => {
                // SAFETY: the submission named this buffer, and the ring it arrived on is
                // still alive, so the memory belongs to a worker that has not gone away.
                let mem = unsafe { std::slice::from_raw_parts_mut(addr as *mut u8, len as usize) };
                let (bad, rot) = {
                    let f = self.faults.borrow();
                    (f.io_error, f.corrupt)
                };
                if self.chance(bad) {
                    self.hit(Hit::IoError);
                    -libc::EIO
                } else {
                    if read {
                        self.read_at(dev, off, mem);
                    } else {
                        self.write_at(dev, off, mem);
                    }
                    if read && !mem.is_empty() && self.chance(rot) {
                        self.hit(Hit::Corrupt);
                        // Silently, on the way back: caught by the small class's page
                        // checksum and, by design, not by the huge class's.
                        let at = (self.rand() as usize) % mem.len();
                        mem[at] ^= 0xff;
                    }
                    len as i32
                }
            }
            Event::Guest {
                dev,
                read,
                pos,
                addr,
                len,
                fixed,
                ..
            } => match cdev_minor(dev) {
                Some(minor) => self
                    .ublk
                    .borrow_mut()
                    .copy(minor, pos, addr, len, read, fixed),
                None => -libc::ENODEV,
            },
            Event::Ublk { .. } | Event::Frame { .. } => unreachable!("handled above"),
        };
        r.post(user_data, result);
    }

    /// What this run is asked to go wrong with, from here on.
    pub(crate) fn set_faults(&self, f: Faults) {
        *self.faults.borrow_mut() = f;
    }

    /// Where the run's nondeterminism starts. Odd, so the generator never sticks at zero.
    pub(crate) fn set_seed(&self, seed: u64) {
        self.rng.set(seed | 1);
    }

    /// How many times a run took a path.
    pub(crate) fn hits(&self, h: Hit) -> u64 {
        self.hits.get()[h as usize]
    }

    /// Counts a path taken.
    fn hit(&self, h: Hit) {
        let mut c = self.hits.get();
        c[h as usize] += 1;
        self.hits.set(c);
    }

    /// xorshift64*, which is small, fast and has a period no run will reach.
    fn rand(&self) -> u64 {
        let mut x = self.rng.get();
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.rng.set(x);
        x.wrapping_mul(0x2545_f491_4f6c_dd1d)
    }

    /// Whether something with this chance in a thousand happens now.
    fn chance(&self, per_mille: u32) -> bool {
        per_mille != 0 && (self.rand() % 1000) < per_mille as u64
    }

    /// Says where a node answers for itself, so a write to its fabric device reaches it.
    pub(crate) fn set_fabric(&self, node: u32, minor: u32) {
        self.fabric.borrow_mut().insert(node, minor);
    }

    /// Loses a node, the way a power cut does.
    ///
    /// Its threads stop where they stand, its exports go, and the counters it kept while
    /// it was up are forgotten so the same node can start again. Nothing it wrote is
    /// touched: what a crash costs is exactly what had not reached a device yet.
    pub(crate) fn crash_node(&self, id: u32, minors: &[u32]) {
        let gone: Vec<Fiber> = {
            let mut fibers = self.fibers.borrow_mut();
            let mut gone = Vec::new();
            let mut i = 0;
            while i < fibers.len() {
                if fibers[i].node == id {
                    gone.push(fibers.remove(i));
                } else {
                    i += 1;
                }
            }
            gone
        };
        // Forgotten, not dropped. A crashed thread's stack is never unwound, and here
        // that matters twice over: the futures on it hold buffers they would hand back
        // to a worker that is no longer bound to this thread.
        for f in gone {
            std::mem::forget(f);
        }
        {
            let mut u = self.ublk.borrow_mut();
            for &m in minors {
                u.forget(m);
            }
        }
        self.fabric.borrow_mut().remove(&id);
        let mut c = self.counters.borrow_mut();
        if let Some(row) = c.get_mut(id as usize) {
            *row = [0; super::COUNTERS];
        }
    }

    /// Hands a transfer to the peer it names, as a request on that peer's fabric device.
    ///
    /// The peer serves it with the same worker loop it serves a guest with, because to it
    /// there is no difference: a frame is a request that arrived on a device. The
    /// submission that sent it stays open until the peer answers, which is what makes a
    /// node that is slow to answer a node its peers are waiting on.
    #[allow(clippy::too_many_arguments)]
    fn frame(
        &self,
        ring: u32,
        user_data: u64,
        from: u32,
        node: u32,
        read: bool,
        lba: u64,
        addr: u64,
        len: u32,
    ) {
        let fail = |result| {
            if let Some(r) = self.ring(ring) {
                r.post(user_data, result);
            }
        };
        let Some(minor) = self.fabric.borrow().get(&node).copied() else {
            return fail(-libc::ENODEV);
        };
        let queues = self.ublk.borrow().queues(minor);
        if queues == 0 {
            return fail(-libc::ENODEV);
        }
        let (dropped, cut) = {
            let f = self.faults.borrow();
            (
                f.drop,
                f.cut.contains(&(from, node)) || f.cut.contains(&(node, from)),
            )
        };
        if cut {
            // Refused rather than dropped: the two ends of a cut still have a wire, and a
            // wire that is up and going nowhere is what the sender has to survive.
            self.hit(Hit::Cut);
            return fail(-libc::EIO);
        }
        if self.chance(dropped) {
            // A lost frame is one nobody answers, and what the sender eventually sees is
            // its own deadline. The ring decided that deadline when it read the
            // submission, so the timeout is spelled here rather than waited for: the
            // answer is the same one a link timeout would have posted.
            self.hit(Hit::Drop);
            return fail(-libc::ETIME);
        }
        let data = if read {
            vec![0u8; len as usize]
        } else {
            // The bytes are taken now, at the instant the transport would have taken
            // them, so what arrives is what was sent and not what the sender did next.
            // SAFETY: the submission is live, so `addr` names a buffer of the worker that
            // made it, and that worker is waiting on this.
            unsafe { std::slice::from_raw_parts(addr as *const u8, len as usize) }.to_vec()
        };
        let op = if read { ublk::OP_READ } else { ublk::OP_WRITE };
        let reply = ublk::Reply {
            ring,
            user_data,
            addr,
            len,
            read,
        };
        // A queue per page, the way the block layer spreads one guest over many.
        let q_id = (lba as usize) % queues;
        match self
            .ublk
            .borrow_mut()
            .submit(minor, q_id, op, lba, data, Some(reply))
        {
            Ok((_, actions)) => {
                self.hit(Hit::Frame);
                self.apply(actions);
            }
            Err(e) => fail(-e.raw_os_error().unwrap_or(libc::EIO)),
        }
    }

    /// Lets the simulation run while this fiber has nothing to do, for at most until `us`.
    ///
    /// This is what a park is: the caller is out of work, so it stops being the reason
    /// time is standing still.
    pub(crate) fn idle_until(&self, us: u64) {
        if self.pump_until(us) == 0 {
            self.advance_to(us);
        }
    }

    /// Waits for `rx`, running the other fibers until one of them sends.
    pub(crate) fn recv<T>(&self, rx: &Receiver<T>) -> Result<T, RecvError> {
        for _ in 0..PUMP_LIMIT {
            match rx.try_recv() {
                Ok(v) => return Ok(v),
                Err(TryRecvError::Disconnected) => return Err(RecvError),
                Err(TryRecvError::Empty) => {}
            }
            if self.pump() == 0 {
                // Nothing a turn could do. A fiber may still have finished and sent on
                // its way out, so ask once more before deciding nobody ever will.
                match rx.try_recv() {
                    Ok(v) => return Ok(v),
                    Err(TryRecvError::Disconnected) => return Err(RecvError),
                    Err(TryRecvError::Empty) => {}
                }
                // If the calendar still owes an answer, spend the time waiting for it;
                // if it does not, on a real node this would block for ever.
                match self.next_due() {
                    Some(at) => self.advance_to(at),
                    None => return Err(RecvError),
                }
            }
        }
        panic!("racer: a simulated receive made no progress in {PUMP_LIMIT} rounds: deadlock");
    }

    /// Waits for `rx`, running the other fibers, and gives up once `d` of virtual time
    /// has gone by.
    ///
    /// The clock only moves when the fibers stop having anything to do, so a timeout here
    /// means what it means on a real node: the work did not arrive in time, not that the
    /// simulation was slow.
    pub(crate) fn recv_timeout<T>(
        &self,
        rx: &Receiver<T>,
        d: Duration,
    ) -> Result<T, RecvTimeoutError> {
        let deadline = self.now_us().saturating_add(d.as_micros() as u64);
        loop {
            match rx.try_recv() {
                Ok(v) => return Ok(v),
                Err(TryRecvError::Disconnected) => return Err(RecvTimeoutError::Disconnected),
                Err(TryRecvError::Empty) => {}
            }
            if self.now_us() >= deadline {
                return Err(RecvTimeoutError::Timeout);
            }
            if self.pump_until(deadline) == 0 {
                self.advance_to(deadline);
            }
        }
    }

    pub(crate) fn ublk_exec(&self, op: u32, cmd: &[u8; 80]) -> io::Result<i32> {
        let mut u = self.ublk.borrow_mut();
        let r = u.exec(op, cmd, self.cpus.get());
        // Stopping a device is what completes the fetches parked against it, and those
        // completions are the only thing that ever drives a worker's tags back to idle.
        let actions = if r.is_ok() && op == ublk::STOP_DEV {
            u.abort(ublk::dev_id(cmd))
        } else {
            Vec::new()
        };
        drop(u);
        self.apply(actions);
        r
    }

    /// Whether a process the driver knows about is still serving. The real kernel answers
    /// this by signal; here the driver's own table is the whole of what exists.
    /// Hand a request to a device's queue, as a guest would, and say what to ask for the
    /// answer by.
    pub(crate) fn ublk_submit(
        &self,
        minor: u32,
        q_id: usize,
        op: u8,
        lba: u64,
        data: Vec<u8>,
    ) -> io::Result<u64> {
        let (id, actions) = self
            .ublk
            .borrow_mut()
            .submit(minor, q_id, op, lba, data, None)?;
        self.apply(actions);
        Ok(id)
    }

    /// The answer to a request, once the node has given one. Taken, not read: an answer is
    /// collected once.
    pub(crate) fn ublk_done(&self, id: u64) -> Option<(i32, Vec<u8>)> {
        self.ublk.borrow_mut().done(id)
    }

    pub(crate) fn process_alive(&self, pid: i32) -> bool {
        self.ublk.borrow().serving(pid)
    }

    /// Leave a device at `minor` owned by `pid`, as an instance of us that died would.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn preoccupy_minor(&self, minor: u32, pid: i32) {
        self.ublk.borrow_mut().preoccupy(minor, pid);
    }

    /// Whether `minor` is currently exported.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn holds_minor(&self, minor: u32) -> bool {
        self.ublk.borrow().holds(minor)
    }
}

/// Where the char device ids start, above every device the store can name.
const CDEV_BASE: u32 = 1 << 24;

/// The minor a file id names, if it names a ublk char device at all.
/// Whether a file id names a char device rather than a store.
pub(crate) fn is_char_dev(id: u32) -> bool {
    id >= CDEV_BASE
}

fn cdev_minor(id: u32) -> Option<u32> {
    (id >= CDEV_BASE).then(|| id - CDEV_BASE)
}

/// The minor a path names, if it is a ublk char device at all.
fn char_dev_minor(path: &Path) -> Option<u32> {
    path.to_str()?.strip_prefix("/dev/ublkc")?.parse().ok()
}
