//! The seam between racer and the kernel.
//!
//! Everything the node asks of the operating system - the clock, the io_uring rings, the
//! store's file descriptors, the ublk control device, threads and their channels - is
//! reached through this module and nowhere else. There are two implementations behind it.
//! `real` makes the syscall. `sim` answers from an in-process model of what the kernel
//! would have done, at a virtual instant of the simulation's choosing.
//!
//! The seam sits here, at the syscall boundary, rather than higher up on purpose: every
//! line above it is then literally the same code in both worlds. A simulated worker runs
//! the production `Worker::run`, drives the production `ublk::WorkerState`, and reconciles
//! through the production control thread. What a test exercises is what ships.
//!
//! Selection is dynamic, by the `Kernel` enum, and per-thread. That costs one predictable
//! branch at each call and lets a simulated cluster and a real one coexist in one process,
//! which is what makes `cargo test` able to run both. Hot paths capture their kernel once,
//! when the worker is built, so they never pay for the thread local.

pub(crate) mod real;
pub(crate) mod ring;
pub(crate) mod sim;

use std::cell::RefCell;
use std::io;
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, OwnedFd, RawFd};
use std::path::Path;
use std::rc::Rc;
use std::sync::mpsc::{Receiver, RecvError, RecvTimeoutError};
use std::time::{Duration, Instant};

/// Which kernel the current thread talks to.
///
/// `Real` carries no state: a real syscall needs nothing but its arguments. `Sim` carries
/// the model, shared by every fiber of the simulated node, which is why it is an `Rc` and
/// why a `Kernel` is not `Send`. Threads are a kernel resource too, so a simulated thread
/// is a fiber the simulator schedules, and it inherits the same handle.
#[derive(Clone)]
pub(crate) enum Kernel {
    Real,
    Sim(Rc<sim::Sim>),
}

thread_local! {
    static CURRENT: RefCell<Kernel> = const { RefCell::new(Kernel::Real) };
}

/// The kernel this thread talks to.
///
/// The clone is a refcount bump. Callers on a hot path hold their own handle instead; this
/// is for the cold ones, where the branch is lost in the work either way.
pub(crate) fn current() -> Kernel {
    CURRENT.with(|k| k.borrow().clone())
}

/// Point this thread at `k`, returning what it was pointed at before.
///
/// A test restores the previous kernel on the way out, so a simulation that panics does
/// not leave the thread talking to a world that has been torn down.
pub(crate) fn install(k: Kernel) -> Kernel {
    CURRENT.with(|c| std::mem::replace(&mut *c.borrow_mut(), k))
}

/// Now, as a host `Instant`, which is the only shape the runtime accepts.
///
/// Under the simulator this is virtual time: an offset from the instant the run was
/// anchored to. It moves only when the simulator advances it, so a handler that waits for
/// a deadline waits for the calendar to reach it and not for the wall clock.
pub(crate) fn now() -> Instant {
    match current() {
        Kernel::Real => real::now(),
        Kernel::Sim(s) => s.clock(),
    }
}

/// Nanoseconds since an arbitrary fixed point, shared by every caller on the node.
///
/// The rate limiter meters in nanoseconds against a virtual clock of its own, so it wants
/// a scalar rather than an `Instant`. The origin is arbitrary but must not move.
pub(crate) fn now_ns() -> u64 {
    match current() {
        Kernel::Real => real::now_ns(),
        Kernel::Sim(s) => s.now_us() * 1_000,
    }
}

/// Park this thread for `d`.
///
/// Only the control plane sleeps like this: the store's pacing during a format or a scan,
/// and ublk's wait for a dead server's minor to come back. A worker never does; it parks
/// in the ring so a doorbell can wake it.
pub(crate) fn sleep_blocking(d: Duration) {
    match current() {
        Kernel::Real => real::sleep_blocking(d),
        Kernel::Sim(s) => s.sleep_blocking(d),
    }
}

/// The size of a page, in bytes.
///
/// The ublk descriptor array's per-queue stride is derived from this, so it is part of the
/// ABI and not just an allocation hint. The simulator answers with a fixed size so a run
/// reproduces on a host whose pages are a different size.
pub(crate) fn page_size() -> usize {
    match current() {
        Kernel::Real => real::page_size(),
        Kernel::Sim(s) => s.page_size(),
    }
}

/// The logical CPUs this node may run on.
///
/// Never configured: narrow it with `taskset` or a cgroup cpuset. The simulator answers
/// with as many as the node being booted was asked for.
pub(crate) fn affinity() -> io::Result<Vec<usize>> {
    match current() {
        Kernel::Real => real::affinity(),
        Kernel::Sim(s) => s.affinity(),
    }
}

/// The logical CPUs sharing `cpu`'s physical core, including `cpu` itself.
pub(crate) fn siblings(cpu: usize) -> Option<Vec<usize>> {
    match current() {
        Kernel::Real => real::siblings(cpu),
        Kernel::Sim(s) => s.siblings(cpu),
    }
}

/// Pins the calling thread to `cpu`.
pub(crate) fn pin(cpu: usize) -> io::Result<()> {
    match current() {
        Kernel::Real => real::pin(cpu),
        Kernel::Sim(s) => s.pin(cpu),
    }
}

/// Maps `len` bytes of anonymous memory, zeroed.
///
/// `len` is taken as given: rounding is the caller's, because the caller is the one that
/// knows what it is going to lay out inside.
pub(crate) fn map_anon(len: usize) -> io::Result<*mut u8> {
    match current() {
        Kernel::Real => real::map_anon(len),
        Kernel::Sim(s) => s.map_anon(len),
    }
}

/// Maps `len` bytes at `offset` in `fd`, read only.
pub(crate) fn map_read(f: FileRef, offset: u64, len: usize) -> io::Result<*mut u8> {
    match f {
        FileRef::Real(fd) => real::map_read(fd, offset, len),
        FileRef::Sim(id) => match current() {
            Kernel::Real => unreachable!("racer: a simulated file mapped by a real kernel"),
            Kernel::Sim(s) => s.map_read(id, offset, len),
        },
    }
}

/// Releases a mapping.
///
/// The kernel that made a mapping is the one that must release it, so this reads the
/// thread local rather than taking a handle: a `Drop` runs wherever it runs.
///
/// # Safety
///
/// `ptr` and `len` must name a mapping this module returned, and nothing may still be
/// borrowing it.
pub(crate) unsafe fn unmap(ptr: *mut u8, len: usize) {
    match current() {
        Kernel::Real => unsafe { real::unmap(ptr, len) },
        Kernel::Sim(s) => unsafe { s.unmap(ptr, len) },
    }
}

/// An open file.
///
/// The real variant owns a descriptor, and hands it out: the runtime registers store
/// descriptors with io_uring, and a fixed-file table needs the real thing. The simulated
/// variant names an entry in the model's device table, which is the whole of what a
/// simulated store is.
pub(crate) enum File {
    Real(OwnedFd),
    Sim(u32),
}

impl File {
    /// The descriptor behind a real file.
    ///
    /// # Panics
    ///
    /// Panics on a simulated file. Only the paths that register a descriptor with the ring
    /// call this, and those do not run under the simulator, which models the ring instead.
    #[allow(dead_code)] // Used once the ring itself sits behind the seam.
    pub(crate) fn fd(&self) -> BorrowedFd<'_> {
        match self {
            File::Real(fd) => fd.as_fd(),
            File::Sim(_) => panic!("racer: a simulated file has no descriptor"),
        }
    }
}

/// A file named the way a submission names one: by fixed-file index, resolved through a
/// table the ring holds. Copyable and sendable because the table is filled from the control
/// thread and read on every worker, and because unregistering a slot has to name one too.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub(crate) enum FileRef {
    Real(RawFd),
    Sim(u32),
}

impl FileRef {
    /// The empty slot. This is the descriptor the kernel reads as "nothing here", and the
    /// simulated table spells it the same way so one table type serves both.
    pub(crate) const NONE: FileRef = FileRef::Real(-1);
}

impl File {
    /// How a submission will name this file.
    pub(crate) fn as_ref(&self) -> FileRef {
        match self {
            File::Real(fd) => FileRef::Real(fd.as_raw_fd()),
            File::Sim(id) => FileRef::Sim(*id),
        }
    }
}

/// Opens a path. `mode` applies only when `flags` carries `O_CREAT`.
///
/// The simulator has neither: a simulated device exists because the topology said the node
/// has one, so opening it either finds it or does not.
pub(crate) fn open(path: &Path, flags: i32, mode: u32) -> io::Result<File> {
    match current() {
        Kernel::Real => real::open(path, flags, mode).map(File::Real),
        Kernel::Sim(s) => s.open(path).map(File::Sim),
    }
}

/// Fills `buf` from `off`. A short transfer is an error, not a smaller answer.
pub(crate) fn pread(f: &File, buf: &mut [u8], off: u64) -> io::Result<()> {
    match f {
        File::Real(fd) => real::pread(fd.as_fd(), buf, off),
        File::Sim(id) => match current() {
            Kernel::Real => unreachable!("racer: a simulated file outside a simulated node"),
            Kernel::Sim(s) => {
                s.read_at(*id, off, buf);
                Ok(())
            }
        },
    }
}

/// Writes all of `buf` at `off`.
pub(crate) fn pwrite(f: &File, buf: &[u8], off: u64) -> io::Result<()> {
    match f {
        File::Real(fd) => real::pwrite(fd.as_fd(), buf, off),
        File::Sim(id) => match current() {
            Kernel::Real => unreachable!("racer: a simulated file outside a simulated node"),
            Kernel::Sim(s) => {
                s.write_at(*id, off, buf);
                Ok(())
            }
        },
    }
}

/// Makes everything written to `f` durable.
///
/// A simulated store is already the durable copy: the model is what survives a crash,
/// because a crash drops the node and leaves the device table standing.
pub(crate) fn fdatasync(f: &File) -> io::Result<()> {
    match f {
        File::Real(fd) => real::fdatasync(fd.as_fd()),
        File::Sim(_) => Ok(()),
    }
}

/// Reserves `len` bytes for `f`, refusing to shrink.
pub(crate) fn allocate(f: &File, len: u64) -> io::Result<()> {
    match f {
        File::Real(fd) => real::allocate(fd.as_fd(), len),
        File::Sim(id) => match current() {
            Kernel::Real => unreachable!("racer: a simulated file outside a simulated node"),
            Kernel::Sim(s) => s.resize(*id, len),
        },
    }
}

pub(crate) fn file_len(f: &File) -> io::Result<u64> {
    match f {
        File::Real(fd) => real::file_len(fd.as_fd()),
        File::Sim(id) => match current() {
            Kernel::Real => unreachable!("racer: a simulated file outside a simulated node"),
            Kernel::Sim(s) => Ok(s.file_len(*id)),
        },
    }
}

/// Creates `path` and every missing parent. A simulated device needs no directory.
pub(crate) fn create_dir_all(path: &Path) -> io::Result<()> {
    match current() {
        Kernel::Real => real::create_dir_all(path),
        Kernel::Sim(_) => Ok(()),
    }
}

/// A counter a node keeps for the life of its runtime.
///
/// Each of these was a process-wide `static`, which is exactly right while one process is
/// one node and exactly wrong the moment a test asks for two. The real kernel keeps them
/// process-wide still, because a real process is still one node. The simulator keeps a set
/// per node, so two nodes in one address space neither sum their counters nor fight over
/// one cache line.
#[derive(Copy, Clone)]
pub(crate) enum Counter {
    /// Set while a runtime is up, so a second `start` on the same node fails.
    Running,
    /// Control broadcasts that have outlasted their warning deadline.
    BroadcastStalls,
    /// How long the control thread has been stuck in its current broadcast.
    BroadcastWaitUs,
    /// Configurations the node read and refused.
    ConfigRejected,
}

/// How many counters there are. The enum is the index.
pub(crate) const COUNTERS: usize = 4;

/// Which node is running.
///
/// Always zero on a real kernel, because a real process is one node and the question has
/// only ever had one answer. Under simulation a process is a cluster, so anything a node
/// keeps for itself is indexed by this rather than held in a static.
pub(crate) fn node() -> usize {
    match current() {
        Kernel::Real => 0,
        Kernel::Sim(s) => s.node() as usize,
    }
}

pub(crate) fn counter(c: Counter) -> u64 {
    match current() {
        Kernel::Real => real::counter(c as usize),
        Kernel::Sim(s) => s.counter(c as usize),
    }
}

pub(crate) fn set_counter(c: Counter, v: u64) {
    match current() {
        Kernel::Real => real::set_counter(c as usize, v),
        Kernel::Sim(s) => s.set_counter(c as usize, v),
    }
}

pub(crate) fn add_counter(c: Counter, v: u64) {
    match current() {
        Kernel::Real => real::add_counter(c as usize, v),
        Kernel::Sim(s) => s.add_counter(c as usize, v),
    }
}

/// Stores `v` and answers with what was there, so a caller can claim a counter as a flag.
pub(crate) fn swap_counter(c: Counter, v: u64) -> u64 {
    match current() {
        Kernel::Real => real::swap_counter(c as usize, v),
        Kernel::Sim(s) => s.swap_counter(c as usize, v),
    }
}

/// What to do when a worker panics.
///
/// A panicking worker leaves the ring, op slab and ublk queue unrecoverable, so a real
/// node aborts: there is nothing left to serve with. A simulated node is one node of many
/// in a test process, so the panic is left to unwind and the test reports it.
pub(crate) fn on_worker_panic(core: usize) {
    match current() {
        Kernel::Real => real::on_worker_panic(core),
        Kernel::Sim(_) => eprintln!("racer: worker {core} panicked"),
    }
}

/// An open handle on the ublk control device.
///
/// Every control command is one `uring_cmd` on this, submitted and waited for, so the
/// handle is what serializes them and there is exactly one per node.
///
/// The simulated variant carries nothing: the driver's table belongs to the node, not to
/// the handle, and the handle is passed to the control thread, which a simulated node runs
/// as a fiber of the thread that installed the kernel. Holding the model here would make
/// this `!Send` and the production code that hands it over would stop compiling.
// A real handle owns a ring and a descriptor; a simulated one owns nothing at all,
// because the driver's table belongs to the node. Boxing to even them up would put an
// allocation on a path that opens one handle per node.
#[allow(clippy::large_enum_variant)]
pub(crate) enum UblkControl {
    Real(real::UblkControl),
    Sim,
}

pub(crate) fn ublk_control_open() -> io::Result<UblkControl> {
    match current() {
        Kernel::Real => real::ublk_control_open().map(UblkControl::Real),
        Kernel::Sim(_) => Ok(UblkControl::Sim),
    }
}

/// One ublk control command.
///
/// `cmd` is the 80-byte SQE payload: a `ublksrv_ctrl_cmd` in the first 32 bytes, and any
/// buffer it names is the caller's, read and written in place exactly as the driver does
/// it. The answer is the CQE result, so an error is `-errno` unpacked into an
/// [`io::Error`] and nothing above here can tell which driver replied.
pub(crate) fn ublk_exec(c: &mut UblkControl, op: u32, cmd: &[u8; 80]) -> io::Result<i32> {
    match c {
        UblkControl::Real(c) => real::ublk_exec(c, op, cmd),
        UblkControl::Sim => match current() {
            Kernel::Sim(s) => s.ublk_exec(op, cmd),
            Kernel::Real => unreachable!("racer: a simulated ublk handle outside a simulated node"),
        },
    }
}

/// Whether `pid` is still serving.
///
/// This decides whether a minor an earlier instance of us left behind may be reclaimed, so
/// it is the difference between taking back our own export and stealing someone else's.
pub(crate) fn process_alive(pid: i32) -> bool {
    match current() {
        Kernel::Real => real::process_alive(pid),
        Kernel::Sim(s) => s.process_alive(pid),
    }
}

/// A thread this node started.
///
/// A node's threads are part of its shape: one per worker core, one for the control
/// plane, one for metrics, one watching the configuration. A real node gets real threads.
/// A simulated node gets fibers the simulator schedules, so that a run is a sequence of
/// steps a seed reproduces rather than whatever the host's scheduler felt like.
pub(crate) enum Thread {
    Real(std::thread::JoinHandle<()>),
    Sim,
}

impl Thread {
    /// Waits for the thread to finish. A panic there is reported, not propagated.
    pub(crate) fn join(self) -> std::thread::Result<()> {
        match self {
            Thread::Real(h) => h.join(),
            Thread::Sim => Ok(()),
        }
    }
}

/// What a thread does, expressed so that someone else can decide when it does it.
///
/// A thread here is a loop, and the loop is split so a turn of it is a value the caller
/// can ask for. A real thread spins the turns out as fast as it can and nobody else is
/// involved. A simulated thread hands them back one at a time, which is the whole of
/// what makes a run reproducible: the interleaving is a decision the simulator makes and
/// a seed records, not one the host's scheduler makes and nobody records.
///
/// The three parts are the three parts a thread has. `start` runs once on whichever
/// thread or fiber will do the work, which matters because a worker's ring, buffer pool
/// and thread locals must be built where they will be used. `turn` is the loop body.
/// `finish` runs once afterwards, in time to tear down in an order the turns depended on.
pub(crate) trait Task {
    /// Runs once, on the thread that will take the turns, before the first of them.
    fn start(&mut self) {}

    /// One turn of the loop.
    fn turn(&mut self) -> Turn;

    /// Runs once, after the last turn.
    fn finish(&mut self) {}
}

/// What a turn came to.
///
/// A real thread only cares whether there is another turn to take. A simulated one cares
/// whether anything happened, because a round of turns in which nobody did anything is
/// how the simulator knows the run is waiting on the clock rather than on itself. A task
/// that would have blocked answers `Idle` and gets its turn back later.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub(crate) enum Turn {
    /// Something happened.
    Ran,
    /// Nothing to do. The task is still alive and wants another turn.
    Idle,
    /// Finished. There will be no more turns.
    Done,
}

/// What a receive that is only allowed one turn came to.
///
/// A real thread blocks until there is something or there is nobody left to send, so it
/// never sees `Idle`. A simulated one cannot block inside a turn - it would be blocking
/// the very fibers that would satisfy it - so an empty inbox ends the turn instead.
pub(crate) enum Wait<T> {
    Got(T),
    Idle,
    Closed,
}

/// Takes one message, or gives the turn back.
pub(crate) fn recv_turn<T>(rx: &Receiver<T>) -> Wait<T> {
    match current() {
        Kernel::Real => match rx.recv() {
            Ok(v) => Wait::Got(v),
            Err(_) => Wait::Closed,
        },
        Kernel::Sim(_) => match rx.try_recv() {
            Ok(v) => Wait::Got(v),
            Err(std::sync::mpsc::TryRecvError::Empty) => Wait::Idle,
            Err(std::sync::mpsc::TryRecvError::Disconnected) => Wait::Closed,
        },
    }
}

/// Something a task builds, uses and drops without ever letting it leave the thread.
///
/// A worker once it is built is full of `Rc`s, `Cell`s and raw pointers into its own
/// ring, so it is emphatically not `Send`. It does not need to be: `spawn` is handed an
/// empty task, and `start`, every `turn` and `finish` all run on the one thread that
/// filled it. The wrapper says that in the type system, which is the only place the
/// compiler can read it.
pub(crate) struct OnThread<T>(pub(crate) Option<T>);

// SAFETY: a task is handed to `spawn` with this empty, and whatever fills it is created,
// used and dropped by `start`, `turn` and `finish`, which the kernel runs on one thread.
unsafe impl<T> Send for OnThread<T> {}

/// Starts `t` on a thread named `name`.
///
/// The name is for `top` and for a core file, so it is worth keeping accurate. Pinning is
/// the thread's own business: a worker knows which CPU it wants and the control thread
/// wants a sibling of the first, and neither is decided here.
pub(crate) fn spawn(name: String, t: impl Task + Send + 'static) -> io::Result<Thread> {
    match current() {
        Kernel::Real => real::spawn(name, t).map(Thread::Real),
        Kernel::Sim(s) => s.spawn(name, Box::new(t)).map(|()| Thread::Sim),
    }
}

/// Waits for `rx` to have something, or for every sender to be gone.
///
/// Blocking is a scheduling decision, so it is the kernel's. A real thread blocks. A
/// simulated one cannot: it is the only thread there is, so whatever would satisfy the
/// wait has to be given a turn first. Under simulation this therefore runs the other
/// fibers until one of them sends, and reports a deadlock rather than hanging if none
/// ever will.
pub(crate) fn recv<T>(rx: &Receiver<T>) -> Result<T, RecvError> {
    match current() {
        Kernel::Real => rx.recv(),
        Kernel::Sim(s) => s.recv(rx),
    }
}

/// Waits for `rx` to have something, giving up after `d`.
pub(crate) fn recv_timeout<T>(rx: &Receiver<T>, d: Duration) -> Result<T, RecvTimeoutError> {
    match current() {
        Kernel::Real => rx.recv_timeout(d),
        Kernel::Sim(s) => s.recv_timeout(rx, d),
    }
}

/// Runs `f(0)` through `f(threads - 1)` and collects the answers in order.
///
/// This is the startup scan, and nothing else: a fixed amount of independent work, split
/// by index, that the node waits out before it serves. The real kernel runs the pieces at
/// once; the simulator runs them one after another. The split is the caller's either way,
/// so both do the same work and reach the same answer.
pub(crate) fn parallel<T, F>(threads: usize, f: F) -> Vec<T>
where
    F: Fn(usize) -> T + Sync,
    T: Send,
{
    match current() {
        Kernel::Real => real::parallel(threads, f),
        Kernel::Sim(_) => (0..threads).map(f).collect(),
    }
}

/// A submission and completion queue shared with the kernel.
///
/// Every worker owns one and never shares it, which is what lets the whole hot path run
/// without a lock. The control thread owns one too, used for nothing but posting a
/// completion onto a worker's ring to wake it.
// The real ring embeds io_uring's submitter and queues, and the simulated one is two
// pointers. Boxing to even them up would put an indirection on the reap path to save a
// few words on a per-worker structure that is already boxed.
#[allow(clippy::large_enum_variant)]
pub(crate) enum Ring {
    Real(real::ring::Ring),
    Sim(sim::ring::Ring),
}

/// Opens a ring holding `entries` submissions and `cq_entries` completions.
///
/// `polled` asks for the setup a worker needs: a single issuer, completions deferred to
/// the thread that owns the ring, a flag it can read without a syscall to learn there is
/// work, and submit-all so one full queue does not abandon the rest. The doorbell wants
/// none of that, because it only ever posts.
pub(crate) fn ring_open(entries: u32, cq_entries: u32, polled: bool) -> io::Result<Ring> {
    match current() {
        Kernel::Real => real::ring::open(entries, cq_entries, polled).map(Ring::Real),
        Kernel::Sim(s) => sim::ring::open(&s, entries, cq_entries, polled).map(Ring::Sim),
    }
}

macro_rules! on_ring {
    ($self:ident, $r:ident => $body:expr) => {
        match $self {
            Ring::Real($r) => $body,
            Ring::Sim($r) => $body,
        }
    };
}

impl Ring {
    /// Queues one submission, answering false if the queue is full. A refused submission
    /// is the caller's to hold on to and offer again.
    pub(crate) fn push(&self, e: &ring::Sqe) -> bool {
        on_ring!(self, r => r.push(e))
    }

    /// Room left in the submission queue, which a linked pair has to fit in whole.
    pub(crate) fn room(&self) -> usize {
        on_ring!(self, r => r.room())
    }

    pub(crate) fn is_empty(&self) -> bool {
        on_ring!(self, r => r.is_empty())
    }

    pub(crate) fn submit(&self) {
        on_ring!(self, r => r.submit())
    }

    /// Whether there are completions waiting that this thread has not been told about.
    pub(crate) fn taskrun(&self) -> bool {
        on_ring!(self, r => r.taskrun())
    }

    /// Collects waiting completions without submitting anything.
    pub(crate) fn get_events(&self) {
        on_ring!(self, r => r.get_events())
    }

    pub(crate) fn drain(&self, out: &mut Vec<ring::Cqe>) {
        on_ring!(self, r => r.drain(out))
    }

    /// Submits, then waits for a completion or for `ts` to run out.
    pub(crate) fn wait(&self, ts: &ring::Timespec) {
        on_ring!(self, r => r.wait(ts))
    }

    pub(crate) fn register_files_sparse(&self, n: u32) -> io::Result<()> {
        on_ring!(self, r => r.register_files_sparse(n))
    }

    pub(crate) fn register_files_update(&self, off: u32, fds: &[FileRef]) -> io::Result<()> {
        on_ring!(self, r => r.register_files_update(off, fds))
    }

    pub(crate) fn register_buffers_sparse(&self, n: u32) -> io::Result<()> {
        on_ring!(self, r => r.register_buffers_sparse(n))
    }

    pub(crate) fn register_buffers_update(&self, off: u32, bufs: &[libc::iovec]) -> io::Result<()> {
        on_ring!(self, r => r.register_buffers_update(off, bufs))
    }

    /// The descriptor another thread posts to in order to wake this ring's owner. A
    /// simulated ring has none: waking is the scheduler's business, not a descriptor's.
    pub(crate) fn as_raw_fd(&self) -> RawFd {
        on_ring!(self, r => r.as_raw_fd())
    }
}

/// Parses the kernel's "0-3,8" CPU list format.
///
/// Malformed pieces are dropped rather than refused: this reads sysfs, and a node that
/// cannot parse one line of topology should fall back to no siblings, not fail to boot.
pub(crate) fn parse_cpu_list(s: &str) -> Vec<usize> {
    let mut out = Vec::new();

    for part in s.split(',') {
        match part.split_once('-') {
            Some((a, b)) => {
                if let (Ok(a), Ok(b)) = (a.parse(), b.parse::<usize>()) {
                    out.extend(a..=b);
                }
            }
            None => {
                if let Ok(a) = part.parse() {
                    out.push(a);
                }
            }
        }
    }

    out
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Installing must be a swap, not a set: a test that leaves the thread pointed at a
    /// world it has torn down poisons every test that runs after it on that thread.
    #[test]
    fn install_returns_the_previous_kernel_and_restores_it() {
        assert!(matches!(current(), Kernel::Real), "threads start out real");

        let previous = install(Kernel::Real);
        assert!(matches!(previous, Kernel::Real));
        install(previous);

        assert!(matches!(current(), Kernel::Real));
    }

    #[test]
    fn the_real_clock_never_goes_backwards() {
        let a = now();
        let b = now();
        assert!(b >= a);
        assert!(
            now_ns() > 0,
            "the epoch is behind us, so something has elapsed"
        );
    }

    /// The whole point of the seam: the same `kernel::now` answers from the model, and
    /// only moves when the simulation says so.
    #[test]
    fn the_simulated_clock_moves_only_when_advanced() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));

        let start = now();
        assert_eq!(start, now(), "virtual time does not pass on its own");
        assert_eq!(now_ns(), 0, "the run begins at its own zero");

        s.advance_to(1_500);
        assert_eq!(now(), start + Duration::from_micros(1_500));
        assert_eq!(now_ns(), 1_500_000);

        // Time never runs backwards, however it is asked to.
        s.advance_to(500);
        assert_eq!(now(), start + Duration::from_micros(1_500));

        install(previous);
        assert!(matches!(current(), Kernel::Real));
    }

    /// A blocking sleep under the simulator costs virtual time, not wall time.
    #[test]
    fn a_simulated_sleep_spends_virtual_time() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));

        let start = now();
        sleep_blocking(Duration::from_millis(250));
        assert_eq!(now(), start + Duration::from_millis(250));

        install(previous);
    }

    #[test]
    fn a_counter_claimed_once_is_not_claimed_twice() {
        let was = swap_counter(Counter::Running, 1);
        let again = swap_counter(Counter::Running, 1);

        set_counter(Counter::Running, was);

        assert_eq!(again, 1, "the second claim must see the first");
    }

    #[test]
    fn two_simulated_nodes_do_not_share_a_counter() {
        let a = sim::Sim::new();
        let b = sim::Sim::new();

        let previous = install(Kernel::Sim(a.clone()));
        add_counter(Counter::ConfigRejected, 3);
        install(Kernel::Sim(b.clone()));
        add_counter(Counter::ConfigRejected, 1);
        let theirs = counter(Counter::ConfigRejected);
        install(Kernel::Sim(a));
        let ours = counter(Counter::ConfigRejected);
        install(previous);

        assert_eq!((ours, theirs), (3, 1), "each node keeps its own count");
    }

    #[test]
    fn a_cpu_list_drops_only_the_pieces_it_cannot_read() {
        assert_eq!(parse_cpu_list("0-2,7,9-10"), vec![0, 1, 2, 7, 9, 10]);
        assert_eq!(parse_cpu_list("bad,4-2,8-x,11"), vec![11]);
        assert_eq!(parse_cpu_list("3"), vec![3]);
        assert!(parse_cpu_list("").is_empty());
    }

    /// Anonymous memory is real either way, and has to read back what was written.
    #[test]
    fn anonymous_memory_is_zeroed_and_writable() {
        let len = 2 << 20;
        let p = map_anon(len).expect("map");

        // SAFETY: `len` bytes were just mapped read-write.
        unsafe {
            assert!(
                (0..len).all(|i| *p.add(i) == 0),
                "a fresh mapping is zeroed"
            );
            *p.add(len - 1) = 0xa5;
            assert_eq!(*p.add(len - 1), 0xa5);
            unmap(p, len);
        }
    }

    /// The host's topology is whatever it is; what must hold is that a node can be built
    /// from it. A CPU always shares a core with itself.
    #[test]
    fn the_real_topology_describes_at_least_one_usable_cpu() {
        let cpus = affinity().expect("affinity");
        assert!(!cpus.is_empty(), "the process runs somewhere");

        let first = cpus[0];
        if let Some(s) = siblings(first) {
            assert!(s.contains(&first), "a CPU is its own sibling");
        }
    }

    /// The simulator's topology is a statement of how many workers to make, and it must
    /// answer the same way every run whatever the host looks like.
    #[test]
    fn the_simulated_topology_is_the_one_the_run_asked_for() {
        let s = sim::Sim::new();
        s.set_cpus(4);
        let previous = install(Kernel::Sim(s.clone()));

        assert_eq!(affinity().unwrap(), vec![0, 1, 2, 3]);
        assert_eq!(siblings(2), Some(vec![2]), "no SMT: a fiber has no twin");
        assert_eq!(siblings(4), None, "and no CPU beyond the ones asked for");
        assert!(pin(3).is_ok());
        assert_eq!(page_size(), 4096, "fixed, so a run reproduces off the host");

        install(previous);
    }

    #[test]
    fn work_split_across_threads_comes_back_in_order() {
        // The answers are indexed by piece, not by whichever thread finished first.
        let got = parallel(8, |t| t * t);
        assert_eq!(got, vec![0, 1, 4, 9, 16, 25, 36, 49]);
    }

    /// A task that reports each part it runs and stops after `turns` turns.
    struct Counted {
        log: std::sync::mpsc::Sender<&'static str>,
        turns: u32,
    }

    impl Task for Counted {
        fn start(&mut self) {
            let _ = self.log.send("start");
        }

        fn turn(&mut self) -> Turn {
            if self.turns == 0 {
                return Turn::Done;
            }
            let _ = self.log.send("turn");
            self.turns -= 1;
            Turn::Ran
        }

        fn finish(&mut self) {
            let _ = self.log.send("finish");
        }
    }

    #[test]
    fn a_real_thread_runs_every_part_in_order_and_is_waited_for() {
        let (tx, rx) = std::sync::mpsc::channel();
        let h = spawn("racer-test".into(), Counted { log: tx, turns: 2 }).expect("spawn");
        h.join().expect("join");
        let seen: Vec<&str> = rx.into_iter().collect();
        assert_eq!(seen, ["start", "turn", "turn", "finish"]);
    }

    #[test]
    fn a_simulated_thread_waits_for_its_turn() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let (tx, rx) = std::sync::mpsc::channel();
        let h = spawn("racer-fiber".into(), Counted { log: tx, turns: 2 }).expect("spawn");
        assert_eq!(s.fibers(), 1);
        assert_eq!(s.fiber_name(0).as_deref(), Some("racer-fiber"));
        // Nothing has run: a fiber starts when the simulation says so and not before.
        assert!(rx.try_recv().is_err());

        assert_eq!(s.pump(), 1);
        assert_eq!(rx.try_recv(), Ok("start"));
        assert_eq!(rx.try_recv(), Ok("turn"));
        assert!(rx.try_recv().is_err());

        assert_eq!(s.pump(), 1);
        assert_eq!(rx.try_recv(), Ok("turn"));

        // A turn with nothing left to do is the fiber's last, and it is retired in the
        // same round. Retiring is not work, so the round counts nothing.
        assert_eq!(s.pump(), 0);
        assert_eq!(rx.try_recv(), Ok("finish"));
        assert_eq!(s.fibers(), 0);
        assert_eq!(s.pump(), 0);

        h.join().expect("join");
        install(previous);
    }

    /// A task that blocks on `rx` for its one turn, which only another fiber can satisfy.
    struct Waiter {
        rx: Receiver<u32>,
        got: std::sync::mpsc::Sender<u32>,
    }

    impl Task for Waiter {
        fn turn(&mut self) -> Turn {
            let v = recv(&self.rx).expect("the sender takes a turn of its own");
            let _ = self.got.send(v);
            Turn::Done
        }
    }

    struct Sender1(Option<std::sync::mpsc::Sender<u32>>);

    impl Task for Sender1 {
        fn turn(&mut self) -> Turn {
            if let Some(tx) = self.0.take() {
                let _ = tx.send(7);
            }
            Turn::Done
        }
    }

    #[test]
    fn a_blocked_fiber_runs_the_one_that_will_unblock_it() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let (tx, rx) = std::sync::mpsc::channel();
        let (got_tx, got_rx) = std::sync::mpsc::channel();
        spawn(
            "racer-waiter".into(),
            Waiter {
                rx,
                got: got_tx.clone(),
            },
        )
        .expect("spawn");
        spawn("racer-sender".into(), Sender1(Some(tx))).expect("spawn");
        // One pump: the waiter blocks inside its turn and pumps the sender from there,
        // which is the whole point. A pump never steps the fiber it was called from.
        s.pump();
        assert_eq!(got_rx.try_recv(), Ok(7));
        assert_eq!(s.fibers(), 0);
        install(previous);
    }

    #[test]
    fn a_receive_nobody_can_satisfy_is_reported_rather_than_waited_out() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s));
        let (tx, rx) = std::sync::mpsc::channel::<u32>();
        assert!(recv(&rx).is_err());
        drop(tx);
        install(previous);
    }

    #[test]
    fn a_receive_that_times_out_spends_virtual_time_and_no_more() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let (tx, rx) = std::sync::mpsc::channel::<u32>();
        let before = s.now_us();
        let r = recv_timeout(&rx, Duration::from_millis(50));
        assert!(matches!(r, Err(RecvTimeoutError::Timeout)));
        assert_eq!(s.now_us() - before, 50_000);
        drop(tx);
        install(previous);
    }

    #[test]
    fn simulated_work_is_split_the_same_way_and_run_in_order() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s));
        let seen = std::sync::Mutex::new(Vec::new());
        let got = parallel(4, |t| {
            seen.lock().unwrap().push(t);
            t + 1
        });
        install(previous);
        assert_eq!(got, vec![1, 2, 3, 4]);
        assert_eq!(*seen.lock().unwrap(), vec![0, 1, 2, 3]);
    }

    /// A timer whose deadline the caller owns, which is the shape every timed op has.
    fn timer(ts: &ring::Timespec, user_data: u64) -> ring::Sqe {
        ring::Sqe::new(ring::Op::Timeout { ts }, user_data)
    }

    #[test]
    fn a_full_submission_queue_refuses_rather_than_forgets() {
        let ring = ring_open(4, 8, false).expect("ring");
        let ts = ring::Timespec::from_duration(Duration::from_secs(1));
        assert_eq!(ring.room(), 4);
        assert!(ring.is_empty());
        for i in 0..4 {
            assert!(ring.push(&timer(&ts, i)), "submission {i} should fit");
        }
        assert_eq!(ring.room(), 0);
        assert!(!ring.is_empty());
        // The fifth is refused, so the caller still owns it and can offer it again.
        assert!(!ring.push(&timer(&ts, 4)));
    }

    #[test]
    fn a_deadline_survives_the_round_trip_through_the_wire_layout() {
        let ts = ring::Timespec::from_duration(Duration::from_millis(1_500));
        assert_eq!(ts.sec, 1);
        assert_eq!(ts.nsec, 500_000_000);
        assert_eq!(ts.as_micros(), 1_500_000);
    }

    #[test]
    fn a_submission_is_answered_when_the_calendar_reaches_it() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let ring = ring_open(8, 8, true).expect("ring");
        let ts = ring::Timespec::from_duration(Duration::from_micros(50));
        assert!(ring.push(&timer(&ts, 7)));
        // Pushing is not submitting: the kernel has not been told anything yet.
        assert_eq!(s.next_due(), None);
        ring.submit();
        assert!(ring.is_empty());
        assert!(!ring.taskrun());
        // Submitting puts it on the calendar, and it is answered at its own instant.
        assert_eq!(s.next_due(), Some(50));
        s.pump();
        assert_eq!(s.now_us(), 50);
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].user_data, 7);
        assert_eq!(out[0].result, -libc::ETIME);
        install(previous);
    }

    #[test]
    fn a_transfer_reaches_the_store_only_when_it_completes() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let dev = s.open(std::path::Path::new("/sim/n1/store")).expect("open");
        s.resize(dev, 1 << 20).expect("resize");
        let ring = ring_open(8, 8, true).expect("ring");
        ring.register_files_sparse(4).expect("files");
        ring.register_files_update(1, &[FileRef::Sim(dev)])
            .expect("update");

        let src = vec![0xabu8; 4096];
        assert!(ring.push(&ring::Sqe::new(
            ring::Op::WriteFixed {
                file: 1,
                buf: src.as_ptr(),
                len: 4096,
                buf_index: 0,
                offset: 8192,
                dsync: true,
            },
            21,
        )));
        ring.submit();
        // Submitted is not written: the block is still a hole until the transfer's instant.
        let mut check = vec![0u8; 4096];
        s.read_at(dev, 8192, &mut check);
        assert!(check.iter().all(|b| *b == 0));

        s.pump();
        s.read_at(dev, 8192, &mut check);
        assert!(check.iter().all(|b| *b == 0xab));
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert_eq!(out.len(), 1);
        assert_eq!((out[0].user_data, out[0].result), (21, 4096));

        // And it reads back through the same path.
        let mut dst = vec![0u8; 4096];
        assert!(ring.push(&ring::Sqe::new(
            ring::Op::ReadFixed {
                file: 1,
                buf: dst.as_mut_ptr(),
                len: 4096,
                buf_index: 0,
                offset: 8192,
            },
            22,
        )));
        ring.submit();
        s.pump();
        assert!(dst.iter().all(|b| *b == 0xab));
        install(previous);
    }

    #[test]
    fn a_submission_naming_a_file_the_ring_does_not_have_is_refused() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let ring = ring_open(8, 8, true).expect("ring");
        ring.register_files_sparse(4).expect("files");
        let mut dst = vec![0u8; 4096];
        assert!(ring.push(&ring::Sqe::new(
            ring::Op::ReadFixed {
                file: 1,
                buf: dst.as_mut_ptr(),
                len: 4096,
                buf_index: 0,
                offset: 0,
            },
            33,
        )));
        ring.submit();
        s.pump();
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert_eq!(out.len(), 1);
        assert_eq!((out[0].user_data, out[0].result), (33, -libc::EBADF));
        install(previous);
    }

    #[test]
    fn a_guarded_transfer_answers_whichever_was_owed_first() {
        // The transfer is quicker than its guard, so it lands and the guard is canceled.
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let dev = s.open(std::path::Path::new("/sim/n1/store")).expect("open");
        s.resize(dev, 1 << 20).expect("resize");
        let ring = ring_open(8, 8, true).expect("ring");
        ring.register_files_sparse(4).expect("files");
        ring.register_files_update(1, &[FileRef::Sim(dev)])
            .expect("update");
        let mut dst = vec![0u8; 4096];
        let read = ring::Sqe::new(
            ring::Op::ReadFixed {
                file: 1,
                buf: dst.as_mut_ptr(),
                len: 4096,
                buf_index: 0,
                offset: 0,
            },
            41,
        );
        let slow = ring::Timespec::from_duration(Duration::from_micros(5_000));
        assert!(ring.push(&read.linked()));
        assert!(ring.push(&ring::Sqe::new(ring::Op::LinkTimeout { ts: &slow }, 42)));
        ring.submit();
        s.pump();
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert_eq!(s.now_us(), 50);
        assert_eq!(out.len(), 2);
        assert_eq!((out[0].user_data, out[0].result), (41, 4096));
        assert_eq!((out[1].user_data, out[1].result), (42, -libc::ECANCELED));

        // A guard that expires first takes the transfer down with it, and nothing is read.
        let quick = ring::Timespec::from_duration(Duration::from_micros(10));
        assert!(ring.push(&read.linked()));
        assert!(ring.push(&ring::Sqe::new(ring::Op::LinkTimeout { ts: &quick }, 43)));
        ring.submit();
        s.pump();
        out.clear();
        ring.drain(&mut out);
        assert_eq!(s.now_us(), 60);
        assert_eq!(out.len(), 2);
        assert_eq!((out[0].user_data, out[0].result), (43, -libc::ETIME));
        assert_eq!((out[1].user_data, out[1].result), (41, -libc::ECANCELED));
        install(previous);
    }

    #[test]
    fn a_simulated_completion_is_reaped_exactly_once() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s));
        let ring = ring_open(8, 8, true).expect("ring");
        let Ring::Sim(r) = &ring else { unreachable!() };
        assert!(!ring.taskrun());
        r.post(9, -5);
        assert!(ring.taskrun());
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].user_data, 9);
        assert_eq!(out[0].result, -5);
        // Reaping empties the queue and clears the flag that said there was work.
        assert!(!ring.taskrun());
        out.clear();
        ring.drain(&mut out);
        assert!(out.is_empty());
        install(previous);
    }

    /// A control command's 80-byte payload.
    fn ctrl(dev_id: u32, len: u16, addr: u64, data: u64) -> [u8; 80] {
        let mut c = [0u8; 80];
        c[0..4].copy_from_slice(&dev_id.to_ne_bytes());
        c[4..6].copy_from_slice(&u16::MAX.to_ne_bytes());
        c[6..8].copy_from_slice(&len.to_ne_bytes());
        c[8..16].copy_from_slice(&addr.to_ne_bytes());
        c[16..24].copy_from_slice(&data.to_ne_bytes());
        c
    }

    /// A data-plane command's 16-byte payload.
    fn io_cmd(q_id: u16, tag: u16, result: i32) -> [u8; 16] {
        let mut c = [0u8; 16];
        c[0..2].copy_from_slice(&q_id.to_ne_bytes());
        c[2..4].copy_from_slice(&tag.to_ne_bytes());
        c[4..8].copy_from_slice(&result.to_ne_bytes());
        c
    }

    /// Export one queue of `depth` tags at `minor`, started and ready to serve.
    fn export(minor: u32, depth: u16) {
        let mut ctl = ublk_control_open().expect("a driver");
        let mut info = [0u8; 64];
        info[0..2].copy_from_slice(&1u16.to_ne_bytes());
        info[2..4].copy_from_slice(&depth.to_ne_bytes());
        let cmd = ctrl(minor, 64, info.as_mut_ptr() as u64, 0);
        ublk_exec(&mut ctl, sim::ublk::ADD_DEV, &cmd).expect("a minor");
        ublk_exec(&mut ctl, sim::ublk::SET_PARAMS, &ctrl(minor, 0, 0, 0)).expect("parameters");
        ublk_exec(&mut ctl, sim::ublk::START_DEV, &ctrl(minor, 0, 0, 7)).expect("a disk");
    }

    /// Where a request's guest pages sit in the char device's address space.
    fn buf_offset(q_id: u64, tag: u64, off: u64) -> u64 {
        0x8000_0000 + (q_id << 41) + (tag << 25) + off
    }

    /// A worker parks a fetch, a guest hands over a write, and the two meet: the
    /// descriptor says what was asked, the pages carry what it carried, and committing is
    /// what tells the guest it happened.
    #[test]
    fn a_request_reaches_a_parked_fetch_and_its_answer_reaches_the_guest() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));

        export(3, 2);
        let cdev = open(Path::new("/dev/ublkc3"), libc::O_RDWR, 0).expect("a char device");
        let ring = ring_open(16, 64, false).expect("a ring");
        ring.register_files_sparse(4).expect("a file table");
        ring.register_files_update(0, &[cdev.as_ref()])
            .expect("a slot");
        ring.register_buffers_sparse(8).expect("a buffer table");
        let Ring::Sim(r) = &ring else { unreachable!() };

        // Tag 1, parked with nothing to do.
        let tag = 1u16;
        ring.push(&ring::Sqe::new(
            ring::Op::UringCmd16 {
                file: 0,
                cmd_op: sim::ublk::IO_FETCH_REQ,
                cmd: io_cmd(0, tag, 0),
                addr: Some(tag as u64),
            },
            0x900,
        ));
        ring.submit();
        s.pump();
        let mut cqes = Vec::new();
        ring.drain(&mut cqes);
        assert!(cqes.is_empty(), "a fetch with nothing to do waits");
        assert_eq!(r.buffer(tag as u32), None);

        // A write arrives.
        let id = s
            .ublk_submit(3, 0, sim::ublk::OP_WRITE, 5, vec![0xcd; 4096])
            .expect("a request");
        s.pump();
        ring.drain(&mut cqes);
        assert_eq!(cqes.len(), 1);
        assert_eq!((cqes[0].user_data, cqes[0].result), (0x900, 0));
        let (_, len) = r.buffer(tag as u32).expect("the guest pages");
        assert_eq!(len, 4096);

        // The descriptor says what the guest asked for.
        let descs = map_read(cdev.as_ref(), 0, sim::ublk::CMD_BUF).expect("the queue");
        let at = tag as usize * sim::ublk::IO_DESC;
        // SAFETY: the queue's descriptor array, which the driver has just written.
        let d = unsafe { std::slice::from_raw_parts(descs.add(at), sim::ublk::IO_DESC) };
        assert_eq!(u32::from_ne_bytes(d[0..4].try_into().unwrap()) & 0xff, 1);
        assert_eq!(u32::from_ne_bytes(d[4..8].try_into().unwrap()), 8);
        assert_eq!(u64::from_ne_bytes(d[8..16].try_into().unwrap()), 40);

        // The payload is reachable at the position the ABI puts it, but only to a
        // submission that names its buffer by address. A registered one reaches the driver
        // as an `ITER_BVEC` iterator and `ublk_check_and_get_req()` refuses it, which is
        // what every request copy in the node used to be.
        let mut got = [0u8; 4096];
        ring.push(&ring::Sqe::new(
            ring::Op::ReadFixed {
                file: 0,
                buf: got.as_mut_ptr(),
                len: 4096,
                buf_index: 0,
                offset: buf_offset(0, tag as u64, 0),
            },
            0x9001,
        ));
        ring.submit();
        s.pump();
        ring.drain(&mut cqes);
        assert_eq!(
            (cqes[1].user_data, cqes[1].result),
            (0x9001, -libc::EACCES),
            "a registered buffer against the char device must be refused"
        );
        assert_eq!(got, [0u8; 4096], "and nothing may have been copied");

        ring.push(&ring::Sqe::new(
            ring::Op::Read {
                file: 0,
                buf: got.as_mut_ptr(),
                len: 4096,
                offset: buf_offset(0, tag as u64, 0),
            },
            0x901,
        ));
        ring.submit();
        s.pump();
        ring.drain(&mut cqes);
        assert_eq!((cqes[2].user_data, cqes[2].result), (0x901, 4096));
        assert_eq!(got, [0xcd; 4096]);

        // Committing answers the guest and asks for the next request in one submission.
        assert_eq!(
            s.ublk_done(id),
            None,
            "an answer arrives when it is committed"
        );
        ring.push(&ring::Sqe::new(
            ring::Op::UringCmd16 {
                file: 0,
                cmd_op: sim::ublk::IO_COMMIT_AND_FETCH_REQ,
                cmd: io_cmd(0, tag, 4096),
                addr: Some(tag as u64),
            },
            0x902,
        ));
        ring.submit();
        s.pump();
        let (res, data) = s.ublk_done(id).expect("an answer");
        assert_eq!(res, 4096);
        assert_eq!(data, vec![0xcd; 4096]);
        assert_eq!(r.buffer(tag as u32), None, "the pages go back");

        install(previous);
    }

    /// Stopping a device completes the fetches parked against it. Nothing else does, which
    /// is why a queue that is never stopped is never reaped.
    #[test]
    fn stopping_a_device_hands_back_every_fetch_parked_against_it() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));

        export(4, 2);
        let cdev = open(Path::new("/dev/ublkc4"), libc::O_RDWR, 0).expect("a char device");
        let ring = ring_open(16, 64, false).expect("a ring");
        ring.register_files_sparse(4).expect("a file table");
        ring.register_files_update(0, &[cdev.as_ref()])
            .expect("a slot");
        ring.register_buffers_sparse(8).expect("a buffer table");

        for tag in 0..2u16 {
            ring.push(&ring::Sqe::new(
                ring::Op::UringCmd16 {
                    file: 0,
                    cmd_op: sim::ublk::IO_FETCH_REQ,
                    cmd: io_cmd(0, tag, 0),
                    addr: Some(tag as u64),
                },
                0xa00 + tag as u64,
            ));
        }
        ring.submit();
        s.pump();
        let mut cqes = Vec::new();
        ring.drain(&mut cqes);
        assert!(cqes.is_empty());

        let mut ctl = ublk_control_open().expect("a driver");
        ublk_exec(&mut ctl, sim::ublk::STOP_DEV, &ctrl(4, 0, 0, 0)).expect("a stop");
        ring.drain(&mut cqes);
        assert_eq!(cqes.len(), 2);
        assert!(cqes.iter().all(|c| c.result == -libc::ENODEV));
        assert_eq!(
            s.ublk_submit(4, 0, sim::ublk::OP_READ, 0, vec![0; 4096])
                .err()
                .and_then(|e| e.raw_os_error()),
            Some(libc::ENODEV),
            "a stopped device serves nobody"
        );

        install(previous);
    }

    #[test]
    fn a_queue_is_read_through_memory_the_driver_writes() {
        use sim::ublk::{ADD_DEV, CMD_BUF, IO_DESC};

        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));

        // Two hardware queues, which is how much descriptor memory the driver reserves.
        let mut info = [0u8; 64];
        info[0..2].copy_from_slice(&2u16.to_ne_bytes());
        let mut cmd = [0u8; 80];
        cmd[0..4].copy_from_slice(&3u32.to_ne_bytes());
        cmd[6..8].copy_from_slice(&64u16.to_ne_bytes());
        cmd[8..16].copy_from_slice(&(info.as_mut_ptr() as u64).to_ne_bytes());
        let mut ctl = ublk_control_open().unwrap();
        ublk_exec(&mut ctl, ADD_DEV, &cmd).unwrap();

        let cdev = open(Path::new("/dev/ublkc3"), 0, 0).unwrap();
        let f = cdev.as_ref();
        let first = map_read(f, 0, IO_DESC).unwrap();
        let second = map_read(f, CMD_BUF as u64, IO_DESC).unwrap();

        // Distinct windows on one array, a queue apart, and the same window twice over.
        assert_eq!(second as usize - first as usize, CMD_BUF);
        assert_eq!(map_read(f, 0, IO_DESC).unwrap(), first);
        // Reading past the last queue is a mapping the device cannot answer for.
        assert!(map_read(f, 2 * CMD_BUF as u64, IO_DESC).is_err());
        // A window is the device's memory, so letting go of it does not free anything.
        unsafe { unmap(first, IO_DESC) };
        assert_eq!(map_read(f, 0, IO_DESC).unwrap(), first);

        // A minor nobody exported is a path that names nothing.
        assert_eq!(
            open(Path::new("/dev/ublkc4"), 0, 0)
                .err()
                .unwrap()
                .raw_os_error(),
            Some(libc::ENOENT)
        );

        install(previous);
    }

    #[test]
    fn a_simulated_ring_remembers_what_a_submission_names() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s));
        let ring = ring_open(8, 8, true).expect("ring");
        ring.register_files_sparse(4).expect("files");
        ring.register_buffers_sparse(4).expect("buffers");
        let Ring::Sim(r) = &ring else { unreachable!() };
        // Sparse means every slot exists and none of them names anything yet.
        assert!(r.file(2).is_none());
        assert!(r.buffer(2).is_none());
        ring.register_files_update(2, &[FileRef::Sim(11)])
            .expect("update");
        let iov = [libc::iovec {
            iov_base: 0x1000 as *mut libc::c_void,
            iov_len: 4096,
        }];
        ring.register_buffers_update(2, &iov).expect("update");
        assert_eq!(r.file(2), Some(FileRef::Sim(11)));
        assert_eq!(r.buffer(2), Some((0x1000, 4096)));
        // Past the end of the table is a refusal, not a silent write.
        assert!(ring.register_files_update(4, &[FileRef::Sim(12)]).is_err());
        install(previous);
    }

    #[test]
    fn a_simulated_ring_has_no_descriptor_to_be_woken_through() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s));
        let ring = ring_open(8, 8, true).expect("ring");
        // The doorbell reads this and declines to post: waking a fiber is the
        // scheduler's business, not a descriptor's.
        assert!(ring.as_raw_fd() < 0);
        install(previous);
    }

    #[test]
    fn a_scheduled_completion_arrives_when_its_time_comes_and_not_before() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let ring = ring_open(8, 8, true).expect("ring");
        let Ring::Sim(r) = &ring else { unreachable!() };
        s.at(
            50,
            sim::Event::Cqe {
                ring: r.id(),
                user_data: 7,
                result: 4096,
            },
        );

        // Nothing is due yet, so a pump that may not pass the instant does nothing and
        // the clock does not move.
        assert_eq!(s.pump_until(10), 0);
        assert_eq!(s.now_us(), 0);
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert!(out.is_empty());

        // Left to itself the calendar decides how far time goes.
        assert_eq!(s.pump(), 1);
        assert_eq!(s.now_us(), 50);
        assert!(ring.taskrun());
        ring.drain(&mut out);
        assert_eq!(out.len(), 1);
        assert_eq!((out[0].user_data, out[0].result), (7, 4096));
        assert!(!ring.taskrun());

        // And an empty calendar is the end of the run, not a wait.
        assert_eq!(s.pump(), 0);
        install(previous);
    }

    #[test]
    fn everything_owed_at_one_instant_happens_in_the_order_it_was_promised() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let ring = ring_open(8, 8, true).expect("ring");
        let Ring::Sim(r) = &ring else { unreachable!() };
        for i in 0..3u64 {
            s.at(
                20,
                sim::Event::Cqe {
                    ring: r.id(),
                    user_data: i,
                    result: 0,
                },
            );
        }
        s.at(
            30,
            sim::Event::Cqe {
                ring: r.id(),
                user_data: 9,
                result: 0,
            },
        );

        // One instant has no inside: the three land together, and the later one waits.
        assert_eq!(s.pump(), 3);
        assert_eq!(s.now_us(), 20);
        let mut out = Vec::new();
        ring.drain(&mut out);
        assert_eq!(
            out.iter().map(|c| c.user_data).collect::<Vec<_>>(),
            vec![0, 1, 2]
        );

        assert_eq!(s.pump(), 1);
        assert_eq!(s.now_us(), 30);
        install(previous);
    }

    #[test]
    fn a_completion_for_a_worker_that_is_gone_is_dropped() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let id = {
            let ring = ring_open(8, 8, true).expect("ring");
            let Ring::Sim(r) = &ring else { unreachable!() };
            r.id()
        };
        s.at(
            10,
            sim::Event::Cqe {
                ring: id,
                user_data: 1,
                result: 0,
            },
        );
        // The event is still owed and still spends its time; it simply reaches nobody,
        // exactly as a completion in flight when a node dies does.
        assert_eq!(s.pump(), 1);
        assert_eq!(s.now_us(), 10);
        install(previous);
    }

    #[test]
    fn a_parked_worker_ends_its_turn_rather_than_spending_the_time_itself() {
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let ring = ring_open(8, 8, true).expect("ring");
        let Ring::Sim(r) = &ring else { unreachable!() };
        s.at(
            100,
            sim::Event::Cqe {
                ring: r.id(),
                user_data: 3,
                result: 0,
            },
        );

        // Parking is a turn ending. It spends nothing on its own, because a worker that
        // ran the clock forward from inside its own turn would be running the fibers
        // that were about to answer it.
        ring.wait(&ring::Timespec::from_duration(Duration::from_micros(500)));
        assert_eq!(s.now_us(), 0);
        assert!(!ring.taskrun());

        // The scheduler is what moves the clock, and it moves it to what is owed.
        assert_eq!(s.pump(), 1);
        assert_eq!(s.now_us(), 100);
        assert!(ring.taskrun());
        install(previous);
    }

    #[test]
    fn a_sleeping_fiber_is_not_the_reason_the_run_is_still() {
        struct Ticks(std::rc::Rc<std::cell::Cell<u32>>);
        impl Task for Ticks {
            fn turn(&mut self) -> Turn {
                self.0.set(self.0.get() + 1);
                if self.0.get() < 3 {
                    Turn::Ran
                } else {
                    Turn::Done
                }
            }
        }
        let s = sim::Sim::new();
        let previous = install(Kernel::Sim(s.clone()));
        let seen = std::rc::Rc::new(std::cell::Cell::new(0));
        s.spawn("racer-ticks".into(), Box::new(Ticks(seen.clone())))
            .expect("spawn");

        sleep_blocking(Duration::from_micros(40));

        // The sleeper got its full 40 us, and the other fiber ran itself out inside them.
        assert_eq!(s.now_us(), 40);
        assert_eq!(seen.get(), 3);
        assert_eq!(s.fibers(), 0);
        install(previous);
    }
}
