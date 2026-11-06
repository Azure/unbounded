//! The node owner and per-worker clients for persistent RDMA links.
//!
//! See `designs/racer-verbs.md`, "Public API", and
//! `designs/racer-rdma-sessions.md`, "Link table and state machine".
//!
//! [`node`] returns one [`Client`] per I/O worker and one [`OwnerPort`].
//! Build the [`Owner`] from the port on the native thread and run it with
//! [`crate::OwnerRole`] (or poll it with [`Owner::poll_budgeted`]). Every
//! libibverbs call runs on that thread, except pool registrations, which run
//! on short-lived helper threads (`ffi::Registering`).
//! Clients queue commands and get results through [`Op`]s.

use std::{
    collections::BTreeMap,
    rc::Rc,
    sync::{
        Arc, Mutex, PoisonError, RwLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};

use futures::task::AtomicWaker;

use crate::{
    DeviceMemory, Endpoint, Error, Guard, GuardOwner, PortInfo, Result, Selection,
    channel::{self, Completer, Op, op},
    ffi::{self, Access, NativeDevice, NativeRegion, RegisterWake, Registering},
    link::{self, Break, Epoch, Granted, Limits, Link, Phase, Role, Source},
    pool::{BufferState, Pool, PoolShared},
};

/// Commands each worker may queue before [`Error::Overloaded`].
pub const COMMAND_QUEUE: usize = 64;

/// First backoff after a failed link.
pub const BACKOFF_MIN: Duration = Duration::from_secs(1);

/// Longest backoff. Short, because every page to a key in backoff goes over
/// HTTP; repeated failures only cost a cheap setup attempt this often.
pub const BACKOFF_MAX: Duration = Duration::from_secs(5);

/// A link ready this long resets its backoff.
pub const BACKOFF_RESET: Duration = Duration::from_secs(60);

pub use link::{EXPOSED_GRACE, SETUP_TIMEOUT};

/// Why the owner tore down (or started closing) a link. Counted per node,
/// see [`Client::link_breaks`].
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub enum BreakReason {
    /// Error completion, protocol violation, or a failed connect. Backs off.
    Error,

    /// Not ready within [`SETUP_TIMEOUT`]. Backs off.
    Timeout,

    /// The peer sent `Close`. No backoff.
    PeerClosed,

    /// Our own `Close` finished (idle close, retain, shutdown, or an
    /// expired grant). No backoff.
    Closed,

    /// The caller broke the link with [`Client::break_link`], typically a
    /// failed setup exchange. Backs off.
    Abandoned,

    /// The caller declined the link with [`Client::decline_link`]: the peer
    /// refused the setup for a benign reason (lost tie-break, rate limit,
    /// overload). Retries after [`BACKOFF_MIN`] without escalating.
    Declined,

    /// An exposed grant outlived its deadline plus [`EXPOSED_GRACE`], so the
    /// link started a graceful `Close`. Also counted as [`Self::Closed`]
    /// once the close finishes.
    GrantExpired,
}

impl BreakReason {
    /// Every reason, in counter index order.
    pub const ALL: [Self; 7] = [
        Self::Error,
        Self::Timeout,
        Self::PeerClosed,
        Self::Closed,
        Self::Abandoned,
        Self::Declined,
        Self::GrantExpired,
    ];

    fn of(reason: Break) -> Self {
        match reason {
            Break::Error => Self::Error,
            Break::Timeout => Self::Timeout,
            Break::PeerClosed => Self::PeerClosed,
            Break::Closed => Self::Closed,
        }
    }
}

/// Published links by state, from the latest snapshot. Idle keys are not
/// tracked.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct LinkCounts {
    /// Setup in progress.
    pub connecting: usize,

    /// Data may flow.
    pub ready: usize,

    /// Closing or being destroyed.
    pub breaking: usize,

    /// Refused until the backoff passes.
    pub backoff: usize,
}

/// Opaque caller identity for a link. Racer uses `SHA-256(NodeId)` and the
/// rail.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub struct LinkKey {
    /// Peer identity.
    pub peer: [u8; 32],

    /// Rail: the tag the selector gave the device.
    pub rail: u16,
}

/// A link as seen by workers. Snapshot; may lag the owner.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum LinkState {
    /// No link. [`Client::open`] may start one.
    Idle,

    /// Setup in progress.
    Connecting(Epoch),

    /// Data may flow. `link_id` is the id given to `accept` or `connect`.
    Ready {
        /// QP incarnation.
        epoch: Epoch,

        /// Handshake link id carried in every control message.
        link_id: u64,
    },

    /// Closing or being destroyed.
    Breaking,

    /// A recent failure; [`Client::open`] is refused until then.
    Backoff(Instant),
}

/// Local half of the setup, carried in the signed HTTP exchange.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Setup {
    /// This QP's address.
    pub endpoint: Endpoint,

    /// Receives the peer may consume before credits come back.
    pub recv_credits: u32,

    /// Max grants the sender of this setup accepts on the link. Each side
    /// holds at most `min(local, peer)` outstanding grants.
    pub grants_per_link: u32,
}

/// A new link's epoch and local setup.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Opened {
    /// The QP incarnation. Pass it to later calls.
    pub epoch: Epoch,

    /// Send this to the peer.
    pub setup: Setup,
}

/// Picks ports: takes the discovered port list, returns `(rail, index)`
/// pairs. Rails and indices must be unique.
pub type LinkSelector = dyn FnOnce(&[PortInfo]) -> Result<Vec<(u16, usize)>> + Send;

/// What [`Client::activate`] asks the owner to build.
pub struct NodeConfig {
    /// Runs on the owner thread with the discovered ports.
    pub selector: Box<LinkSelector>,

    /// Registered bytes per selected device, split into receive page
    /// buffers. A selected window-mode device must also support implicit
    /// on-demand paging, which writes read caller memory through;
    /// registration-mode devices register write sources per write instead.
    pub pool_bytes: usize,

    /// Page buffer length (Racer: `MAX_CIPHERTEXT` rounded up to 2 MiB). In
    /// registration mode it must be a multiple of 2 MiB, or activation fails
    /// with [`Error::InvalidConfiguration`].
    pub buffer_len: usize,

    /// Max links (QPs) per device.
    pub max_links: usize,

    /// Max concurrent grants either side may hold, per link.
    pub grants_per_link: u32,

    /// A ready link with no grant, write, or release this long closes.
    pub idle: Duration,

    /// Charged for every native object of this activation; leaks with any
    /// leaked object.
    pub guard: Guard,
}

/// How a command names its link.
#[derive(Clone, Copy)]
enum LinkMatch {
    /// The QP incarnation.
    Epoch(Epoch),

    /// The handshake link id.
    Id(u64),
}

/// Owner commands.
enum Command {
    Activate(NodeConfig, Completer<Vec<Selection>>),

    Open(LinkKey, Completer<Opened>),

    Accept {
        key: LinkKey,
        remote: Setup,
        link_id: u64,
        initiator: bool,
        done: Completer<Opened>,
    },

    Connect {
        key: LinkKey,
        epoch: Epoch,
        remote: Setup,
        link_id: u64,
        done: Completer<()>,
    },

    AttachDevice {
        rail: u16,
        id: u32,
        memory: DeviceMemory,
        done: Completer<usize>,
    },

    Grant {
        key: LinkKey,
        epoch: Epoch,
        device: Option<u32>,
        transfer: [u8; 16],
        length: u32,
        deadline: Instant,
        done: Completer<Granted>,
    },

    Write {
        key: LinkKey,
        epoch: Epoch,
        tag: u32,
        source: Source,
        deadline: Instant,
        done: Completer<()>,
    },

    Release {
        key: LinkKey,
        link: LinkMatch,
        tag: u32,
        done: Completer<()>,
    },

    /// Break the link; `true` declines it (benign, no escalating backoff).
    Break(LinkKey, Epoch, bool),
}

/// Published link states, keyed by link.
type LinkMap = BTreeMap<LinkKey, LinkState>;

/// The owner's published link table: an `Arc` swapped under a mutex that is
/// held only for the pointer swap or clone, plus a generation counter so
/// readers can reuse a cached `Arc` without locking.
///
/// Unlike a `try_read` on a `RwLock`, a read never fails under contention:
/// it waits at most for one pointer swap, and a writer never waits for
/// readers to finish with the old table.
struct Snapshot {
    current: Mutex<Arc<LinkMap>>,

    /// Bumped after every swap of `current`.
    generation: AtomicU64,
}

impl Snapshot {
    fn new() -> Self {
        Self {
            current: Mutex::new(Arc::new(LinkMap::new())),
            generation: AtomicU64::new(0),
        }
    }

    /// The latest table and its generation.
    fn load(&self) -> (u64, Arc<LinkMap>) {
        let current = self.current.lock().unwrap_or_else(PoisonError::into_inner);
        // Read under the lock: the generation matches the table.
        (self.generation.load(Ordering::Acquire), current.clone())
    }

    /// Replace the table. The old one drops outside the lock.
    fn store(&self, links: LinkMap) {
        let links = Arc::new(links);
        let old = {
            let mut current = self.current.lock().unwrap_or_else(PoisonError::into_inner);
            self.generation.fetch_add(1, Ordering::AcqRel);
            std::mem::replace(&mut *current, links)
        };
        drop(old);
    }
}

/// A worker's cached copy of the [`Snapshot`].
struct Cached {
    generation: u64,

    links: Arc<LinkMap>,
}

/// State shared by the owner and every client.
struct NodeShared {
    links: Snapshot,

    pools: RwLock<BTreeMap<u16, Arc<PoolShared>>>,

    /// Device pools by rail and pool id, once attached.
    device_pools: RwLock<BTreeMap<(u16, u32), Arc<PoolShared>>>,

    /// Write source prefetch by rail, for window-mode devices. The owner
    /// withdraws an entry under the write lock before dropping its implicit
    /// region, and [`Client::prefetch`] holds the read lock across the call.
    prefetch: RwLock<BTreeMap<u16, ffi::Prefetcher>>,

    /// Breaks that did not fit in a command queue; `true` declines.
    breaks: Mutex<Vec<(LinkKey, Epoch, bool)>>,

    /// Link breaks by [`BreakReason`] index, since the node started.
    break_counts: [AtomicU64; BreakReason::ALL.len()],

    /// The latest [`Client::retain`] request, not yet applied.
    retain: Mutex<Option<std::collections::BTreeSet<LinkKey>>>,

    closed: AtomicBool,

    drained: AtomicBool,

    /// False once the owner (or its port) is dropped.
    alive: AtomicBool,

    owner: AtomicWaker,
}

/// Create the owner port and `workers` clients (at least one).
pub fn node(workers: usize) -> Result<(Vec<Client>, OwnerPort)> {
    if workers == 0 {
        return Err(Error::InvalidConfiguration);
    }

    let (handles, channel) = channel::channels(workers, COMMAND_QUEUE)?;
    let shared = Arc::new(NodeShared {
        links: Snapshot::new(),
        pools: RwLock::new(BTreeMap::new()),
        device_pools: RwLock::new(BTreeMap::new()),
        prefetch: RwLock::new(BTreeMap::new()),
        breaks: Mutex::new(Vec::new()),
        break_counts: Default::default(),
        retain: Mutex::new(None),
        closed: AtomicBool::new(false),
        drained: AtomicBool::new(false),
        alive: AtomicBool::new(true),
        owner: AtomicWaker::new(),
    });
    let clients = handles
        .into_iter()
        .map(|worker| Client {
            worker,
            shared: shared.clone(),
            cached: Mutex::new(None),
        })
        .collect();

    Ok((clients, OwnerPort { channel, shared }))
}

/// The owner's end of [`node`]. `Send`; move it to the native thread and
/// pass it to [`Owner::new`].
pub struct OwnerPort {
    channel: channel::Owner<Command>,

    shared: Arc<NodeShared>,
}

impl Drop for OwnerPort {
    fn drop(&mut self) {
        self.shared.alive.store(false, Ordering::Release);
    }
}

/// One I/O worker's handle on the node. `Send`.
pub struct Client {
    worker: channel::Worker<Command>,

    shared: Arc<NodeShared>,

    /// The last snapshot this worker read. Uncontended: one worker owns it.
    cached: Mutex<Option<Cached>>,
}

/// An op that already failed with `error`.
fn failed<T>(error: Error) -> Op<T> {
    let (failed, done) = op();
    let _ = done.complete(Err(error));
    failed
}

impl Client {
    /// Queue a command built around a fresh op.
    fn submit<T>(&self, build: impl FnOnce(Completer<T>) -> Command) -> Op<T> {
        if self.shared.closed.load(Ordering::Acquire) || !self.shared.alive.load(Ordering::Acquire)
        {
            return failed(Error::Unavailable);
        }

        let (result, done) = op();

        match self.worker.try_send(build(done)) {
            Ok(()) => result,
            // The command (and its completer) is dropped; report why.
            Err((error, _)) => failed(error),
        }
    }

    /// Ready once this worker's command queue has room, after an
    /// `Overloaded` op. Fails `Unavailable` once the owner is gone.
    pub fn poll_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.worker.poll_ready(cx)
    }

    /// Discover ports, run the selector, and register one page pool per
    /// selected device. Once per round. The pool registrations run on a
    /// helper thread, so the owner keeps serving while they pin memory; the
    /// op resolves once every pool is registered.
    pub fn activate(&self, config: NodeConfig) -> Op<Vec<Selection>> {
        self.submit(|done| Command::Activate(config, done))
    }

    /// Snapshot of `key`'s link as last published by the owner. May lag the
    /// owner, but never reads as [`LinkState::Idle`] because of a concurrent
    /// publish: the read reuses this worker's cached table while the
    /// generation is unchanged and otherwise waits at most for one pointer
    /// swap.
    pub fn link(&self, key: LinkKey) -> LinkState {
        let generation = self.shared.links.generation.load(Ordering::Acquire);
        let mut cached = self.cached.lock().unwrap_or_else(PoisonError::into_inner);

        if cached.as_ref().is_none_or(|c| c.generation != generation) {
            let (generation, links) = self.shared.links.load();
            *cached = Some(Cached { generation, links });
        }

        cached
            .as_ref()
            .and_then(|c| c.links.get(&key).copied())
            .unwrap_or(LinkState::Idle)
    }

    /// Links by state in the latest published snapshot.
    pub fn link_counts(&self) -> LinkCounts {
        let (_, links) = self.shared.links.load();
        let mut counts = LinkCounts::default();

        for state in links.values() {
            match state {
                LinkState::Idle => {}
                LinkState::Connecting(_) => counts.connecting += 1,
                LinkState::Ready { .. } => counts.ready += 1,
                LinkState::Breaking => counts.breaking += 1,
                LinkState::Backoff(_) => counts.backoff += 1,
            }
        }

        counts
    }

    /// Links broken (or, for [`BreakReason::GrantExpired`], closed) for
    /// `reason` since the node started.
    pub fn link_breaks(&self, reason: BreakReason) -> u64 {
        self.shared.break_counts[reason as usize].load(Ordering::Relaxed)
    }

    /// Close every link whose key is not in `keep`: ready links send `Close`
    /// and drain, others break, and none backs off. Used when peers stop
    /// being RDMA edges. The latest request wins; never lost to a full queue.
    pub fn retain(&self, keep: std::collections::BTreeSet<LinkKey>) {
        *self
            .shared
            .retain
            .lock()
            .unwrap_or_else(PoisonError::into_inner) = Some(keep);
        self.shared.owner.wake();
    }

    /// Start a link as initiator. Returns the local setup to sign and send.
    ///
    /// Fails `Overloaded` if a link or setup for `key` exists or it is
    /// backing off, and `Unavailable` if the rail is not active.
    pub fn open(&self, key: LinkKey) -> Op<Opened> {
        self.submit(|done| Command::Open(key, done))
    }

    /// Responder side: replace any link for `key` with a new QP connected to
    /// `remote`. With `initiator` false the link is ready on the peer's
    /// first message; with `initiator` true it sends its own hello and is
    /// ready at once. The caller authenticates and tie-breaks first.
    pub fn accept(&self, key: LinkKey, remote: Setup, link_id: u64, initiator: bool) -> Op<Opened> {
        self.submit(|done| Command::Accept {
            key,
            remote,
            link_id,
            initiator,
            done,
        })
    }

    /// Initiator side: finish with the responder's setup. On failure the
    /// link breaks and backs off.
    pub fn connect(&self, key: LinkKey, epoch: Epoch, remote: Setup, link_id: u64) -> Op<()> {
        self.submit(|done| Command::Connect {
            key,
            epoch,
            remote,
            link_id,
            done,
        })
    }

    /// Receiver: bind a window of `length` bytes over a free page buffer and
    /// send `Grant`. Resolves once the `Grant` is posted.
    pub fn grant(
        &self,
        key: LinkKey,
        epoch: Epoch,
        transfer: [u8; 16],
        length: u32,
        deadline: Instant,
    ) -> Op<Granted> {
        self.submit(|done| Command::Grant {
            key,
            epoch,
            device: None,
            transfer,
            length,
            deadline,
            done,
        })
    }

    /// Receiver: like [`Self::grant`], but over a free buffer of the rail's
    /// device pool `id` (see [`Self::attach_device`]). The arrival is
    /// [`crate::Arrival::Device`]. The fence is the same: the page is handed
    /// out only after the peer's write completion and the window's signaled
    /// invalidation, so the peer can no longer write it. Fails with
    /// `Unavailable` if the rail has no device pool `id`, and `Overloaded`
    /// if no buffer of it is free. Nothing falls back to host memory.
    #[allow(clippy::too_many_arguments)]
    pub fn grant_device(
        &self,
        key: LinkKey,
        epoch: Epoch,
        id: u32,
        transfer: [u8; 16],
        length: u32,
        deadline: Instant,
    ) -> Op<Granted> {
        self.submit(|done| Command::Grant {
            key,
            epoch,
            device: Some(id),
            transfer,
            length,
            deadline,
            done,
        })
    }

    /// Register `memory` (a dma-buf, such as GPU memory) on `rail` as its
    /// device pool `id`, split into page buffers of the node's buffer
    /// length. A rail may hold several device pools with different ids (one
    /// per GPU, say); each id once per rail per round, after
    /// [`Self::activate`]. Resolves to the number of device buffers. The
    /// memory stays registered until the node drains, and the node holds
    /// `memory` until then; it is never read or written by the CPU. The
    /// registration runs on a helper thread; the owner keeps serving.
    pub fn attach_device(&self, rail: u16, id: u32, memory: DeviceMemory) -> Op<usize> {
        self.submit(|done| Command::AttachDevice {
            rail,
            id,
            memory,
            done,
        })
    }

    /// Sender: write `source` into the peer's grant `tag` with immediate,
    /// waiting for its `Grant` until `deadline`. Resolves on the send
    /// completion. Dropping the op does not cancel a posted write.
    pub fn write(
        &self,
        key: LinkKey,
        epoch: Epoch,
        tag: u32,
        source: Source,
        deadline: Instant,
    ) -> Op<()> {
        self.submit(|done| Command::Write {
            key,
            epoch,
            tag,
            source,
            deadline,
            done,
        })
    }

    /// Sender: map `bytes` into `rail`'s NIC page tables before writing
    /// them, so the write does not stall the queue pair on NIC page faults
    /// (window mode reads sources through implicit on-demand paging).
    ///
    /// Runs on the calling thread and blocks until the kernel has mapped
    /// the range, so the cost lands on the caller rather than the owner.
    /// Only advice: it never changes what a write reads.
    ///
    /// `Ok(false)` if `rail` has nothing to prefetch into (registration
    /// mode, or not activated); `Ok(true)` once mapped. Fails `Unavailable`
    /// if the provider cannot prefetch, and `Io` or `InvalidRange` if the
    /// kernel refused the range; the write may then take NIC page faults.
    pub fn prefetch(&self, rail: u16, bytes: &[u8]) -> Result<bool> {
        let prefetch = self
            .shared
            .prefetch
            .read()
            .unwrap_or_else(PoisonError::into_inner);
        let Some(prefetcher) = prefetch.get(&rail) else {
            return Ok(false);
        };
        // SAFETY: the owner withdraws the entry under the write lock before
        // it drops the region, and this read lock is held across the call.
        unsafe { prefetcher.run(bytes) }.map(|()| true)
    }

    /// Sender: decline the peer's grant `tag`. Resolves on the send
    /// completion.
    pub fn release(&self, key: LinkKey, epoch: Epoch, tag: u32) -> Op<()> {
        self.submit(|done| Command::Release {
            key,
            link: LinkMatch::Epoch(epoch),
            tag,
            done,
        })
    }

    /// Sender: decline the peer's grant `tag` on whichever current link of
    /// `key` carries handshake id `link_id`, without consulting the
    /// snapshot. The owner resolves the link, so a snapshot that lags a
    /// just-ready link cannot skip the release. Fails `Unavailable` if no
    /// ready link of `key` has that id.
    pub fn release_link(&self, key: LinkKey, link_id: u64, tag: u32) -> Op<()> {
        self.submit(|done| Command::Release {
            key,
            link: LinkMatch::Id(link_id),
            tag,
            done,
        })
    }

    /// Break the link now (QP to ERR and destroy) and back off. Outstanding
    /// ops fail with `Unavailable`. Never lost: a full queue falls back to a
    /// shared list.
    pub fn break_link(&self, key: LinkKey, epoch: Epoch) {
        self.submit_break(key, epoch, false);
    }

    /// Like [`Self::break_link`], for a setup the peer declined for a benign
    /// reason (lost tie-break, rate limit, overload): the key retries after
    /// [`BACKOFF_MIN`] and its backoff does not escalate.
    pub fn decline_link(&self, key: LinkKey, epoch: Epoch) {
        self.submit_break(key, epoch, true);
    }

    fn submit_break(&self, key: LinkKey, epoch: Epoch, declined: bool) {
        if let Err((_, Command::Break(key, epoch, declined))) =
            self.worker.try_send(Command::Break(key, epoch, declined))
        {
            self.shared
                .breaks
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .push((key, epoch, declined));
            self.shared.owner.wake();
        }
    }

    /// Page buffers of `rail` in `state`, for diagnostics.
    pub fn buffers(&self, rail: u16, state: BufferState) -> Option<usize> {
        let pools = self.shared.pools.try_read().ok()?;
        Some(pools.get(&rail)?.count_in(state))
    }

    /// Page buffers of `rail`'s device pool `id` in `state`, for
    /// diagnostics. `None` without that device pool.
    pub fn device_buffers(&self, rail: u16, id: u32, state: BufferState) -> Option<usize> {
        let pools = self.shared.device_pools.try_read().ok()?;
        Some(pools.get(&(rail, id))?.count_in(state))
    }

    /// Close the node: the owner sends `Close` on ready links, breaks every
    /// link, and frees everything once no page is held. Does not wait.
    pub fn close(&self) {
        self.shared.closed.store(true, Ordering::Release);
        self.shared.owner.wake();
    }

    /// True once closed.
    pub fn closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }

    /// True once closed and every native object is gone.
    pub fn drained(&self) -> bool {
        self.shared.drained.load(Ordering::Acquire)
    }

    /// Start a new round after [`Self::close`]. `Overloaded` until drained;
    /// `Unavailable` once the owner is gone.
    pub fn reopen(&self) -> Result<()> {
        if !self.shared.alive.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }

        if !self.shared.closed.load(Ordering::Acquire) {
            return Ok(());
        }

        if !self.shared.drained.load(Ordering::Acquire) {
            return Err(Error::Overloaded);
        }

        self.shared.drained.store(false, Ordering::Release);
        self.shared.closed.store(false, Ordering::Release);
        self.shared.owner.wake();
        Ok(())
    }
}

/// One selected device.
struct DeviceState {
    pool: Pool,

    // Dropped after the pool's region.
    device: Rc<NativeDevice>,
}

/// Activation parameters kept for link creation.
#[derive(Clone, Copy)]
struct Settings {
    limits: Limits,

    max_links: usize,
}

/// A selected device whose page pool is still registering.
struct PendingDevice {
    tag: u16,

    /// `None` once finished into `region`.
    registering: Option<Registering>,

    region: Option<Result<Rc<NativeRegion>>>,

    /// The implicit region being created off this thread (window mode);
    /// `None` once finished into `implicit` or when none is needed.
    implicit_registering: Option<Registering>,

    /// `None` until the implicit region exists (or failed).
    implicit: Option<Result<Rc<NativeRegion>>>,

    device: Rc<NativeDevice>,
}

/// An activation waiting for its page pool registrations.
struct Activation {
    selected: Vec<Selection>,

    devices: Vec<PendingDevice>,

    buffer_len: usize,

    settings: Settings,

    done: Completer<Vec<Selection>>,
}

/// A device pool registration in flight.
struct Attaching {
    rail: u16,

    id: u32,

    registering: Option<Registering>,

    done: Completer<usize>,
}

/// A link being destroyed.
struct Dying {
    key: LinkKey,

    link: Link,
}

/// Backoff memory for one key.
#[derive(Clone, Copy)]
struct Backoff {
    until: Instant,

    delay: Duration,
}

/// Retry delay for owner work that raises no wake event: failed destroys
/// and shutdown checks.
pub const RETRY_SOON: Duration = Duration::from_millis(1);

/// Runs on the native thread and owns every link, pool, and device.
///
/// Not `Send`. Poll it with [`Self::poll_budgeted`], or run it on its own
/// thread with [`crate::OwnerRole`].
pub struct Owner {
    ctx: racer_io::WorkerCtx,

    #[cfg(any(test, feature = "simulation"))]
    simulation: Option<crate::simulation::Simulation>,

    channel: channel::Owner<Command>,

    shared: Arc<NodeShared>,

    // Declared before `devices`: links drop before pools and devices.
    links: BTreeMap<LinkKey, Link>,

    dying: Vec<Dying>,

    devices: BTreeMap<u16, DeviceState>,

    quota: Option<Arc<GuardOwner>>,

    settings: Option<Settings>,

    /// An activation whose page pools are registering off this thread.
    activating: Option<Activation>,

    /// Device pools registering off this thread.
    attaching: Vec<Attaching>,

    backoff: BTreeMap<LinkKey, Backoff>,

    next_epoch: u64,

    /// Shutdown has started for this round.
    closing: bool,

    /// Snapshot needs publishing.
    dirty: bool,

    /// Earliest published backoff end; the snapshot changes then.
    expiry: Option<Instant>,

    /// A native object leaked against this round's charge: never drained.
    leaked: bool,

    /// The last poll left commands in the queue: poll again at once.
    backlog: bool,

    /// Scratch for the commands taken in one poll, reused across polls.
    commands: Vec<Command>,
}

impl Owner {
    /// Build the owner. Call on the native thread; `ctx` supplies its time
    /// and entropy (and it captures the thread's simulation, in tests).
    pub fn new(port: OwnerPort, ctx: racer_io::WorkerCtx) -> Self {
        // Move the fields out; `OwnerPort::drop` must not mark the node dead.
        let port = std::mem::ManuallyDrop::new(port);
        // SAFETY: each field is read once out of a `ManuallyDrop`.
        let (channel, shared) =
            unsafe { (std::ptr::read(&port.channel), std::ptr::read(&port.shared)) };

        Self {
            ctx,
            #[cfg(any(test, feature = "simulation"))]
            simulation: crate::simulation::current(),
            channel,
            shared,
            links: BTreeMap::new(),
            dying: Vec::new(),
            devices: BTreeMap::new(),
            quota: None,
            settings: None,
            activating: None,
            attaching: Vec::new(),
            backoff: BTreeMap::new(),
            next_epoch: 1,
            closing: false,
            dirty: false,
            expiry: None,
            leaked: false,
            backlog: false,
            commands: Vec::new(),
        }
    }

    /// Wake `waker` on commands, closes, and returned buffers. Completions
    /// and timers need periodic polls as well.
    pub fn register_driver(&self, waker: &Waker) {
        self.channel.register(waker);
        self.shared.owner.register(waker);

        for device in self.devices.values() {
            device.pool.register_owner(waker);
        }
    }

    /// Nonblocking file descriptors whose readiness means
    /// [`Self::poll_budgeted`] has work: each device's completion channel
    /// and, in registration mode, its connection manager channel. Changes
    /// with activation. Timers and commands still need the driver waker and
    /// periodic polls.
    pub fn poll_fds(&self) -> Vec<std::os::fd::RawFd> {
        let mut fds = Vec::new();
        self.poll_fds_into(&mut fds);
        fds
    }

    /// Append [`Self::poll_fds`] to `out`, so a caller can reuse its buffer.
    pub fn poll_fds_into(&self, out: &mut Vec<std::os::fd::RawFd>) {
        for device in self.devices.values() {
            device.device.fds_into(out);
        }
    }

    /// When the owner next needs a poll that no wake source announces:
    /// `None` means only a wake source (a command, a close, a returned
    /// buffer, or a [`Self::poll_fds`] descriptor) brings new work. A time
    /// at or before now means poll again at once (a full completion batch,
    /// queued commands, or synthetic completions).
    pub fn next_wake(&self) -> Option<Instant> {
        let now = self.ctx.now();
        let mut next: Option<Instant> = self.expiry;
        let mut at = |t: Instant| next = Some(next.map_or(t, |n| n.min(t)));
        if self.backlog {
            return Some(now);
        }
        // Destroys to retry and shutdown checks retry on a short timer. The
        // snapshot always publishes within the poll that dirtied it.
        if !self.dying.is_empty() || (self.closing && !self.leaked && !self.drained()) {
            at(now + RETRY_SOON);
        }
        for link in self.links.values() {
            if let Some(t) = link.next_deadline() {
                at(t);
            }
        }
        next
    }

    /// The owner's clock.
    pub fn ctx_now(&self) -> Instant {
        self.ctx.now()
    }

    /// Close from the owner side. Same as [`Client::close`].
    pub fn close(&self) {
        self.shared.closed.store(true, Ordering::Release);
    }

    /// True once closed and every native object is gone.
    pub fn drained(&self) -> bool {
        self.shared.drained.load(Ordering::Acquire)
    }

    /// Handle up to `budget` commands, then progress every link, run
    /// timers, and advance shutdown. `budget` 0 does nothing.
    pub fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        if budget == 0 {
            return Ok(());
        }

        let now = self.ctx.now();
        let closed = self.shared.closed.load(Ordering::Acquire);

        if !closed && self.closing {
            // Reopened: start a fresh round.
            self.closing = false;
            self.settings = None;
        }

        if closed && !self.closing {
            self.closing = true;
            self.begin_shutdown(now);
        }

        let mut commands = std::mem::take(&mut self.commands);
        self.channel
            .try_drain(budget, |_, command| commands.push(command));
        self.backlog = commands.len() >= budget;

        for command in commands.drain(..) {
            self.handle(command, now);
        }

        self.commands = commands;

        let breaks = std::mem::take(
            &mut *self
                .shared
                .breaks
                .lock()
                .unwrap_or_else(PoisonError::into_inner),
        );

        for (key, epoch, declined) in breaks {
            self.break_matching(key, epoch, declined, now);
        }

        let retain = self
            .shared
            .retain
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .take();

        if let Some(keep) = retain
            && !self.closing
        {
            self.retain_links(&keep, now);
        }

        self.advance_registrations();
        self.progress(now);
        self.reap();

        if self.closing {
            self.finish_shutdown();
        }

        self.publish(now);
        #[cfg(any(test, feature = "simulation"))]
        self.check_wake();
        Ok(())
    }

    /// Simulation-only invariant: an owner with work left must have armed a
    /// wake for it. Links, dying links, an undrained shutdown, and a pending
    /// backoff expiry need [`Self::next_wake`]; registrations in flight are
    /// woken by their helper (or held by the simulation). A violation means
    /// a parked owner would sleep forever: a missing timer.
    #[cfg(any(test, feature = "simulation"))]
    fn check_wake(&self) {
        if self.simulation.is_none() {
            return;
        }
        let pending = !self.links.is_empty()
            || !self.dying.is_empty()
            || (self.closing && !self.leaked && !self.drained())
            || self.expiry.is_some();
        let external = self.activating.is_some() || !self.attaching.is_empty();
        assert!(
            !missing_wake(pending, self.next_wake(), external),
            "rdma owner has pending work and no armed wake source"
        );
    }

    fn handle(&mut self, command: Command, now: Instant) {
        if self.closing {
            // Dropping the command fails its op with `Canceled`; say why.
            fail(command, Error::Unavailable);
            return;
        }

        match command {
            Command::Activate(config, done) => {
                // Completes once the page pools are registered.
                if self.activate(config, done).is_err() {
                    self.abandon_activation();
                    self.dirty = true;
                }
            }
            Command::Open(key, done) => {
                // The initiator went away: do not leave the link Connecting.
                if let Err(Ok(opened)) = done.complete(self.open(key, now)) {
                    self.break_matching(key, opened.epoch, false, now);
                }
            }
            Command::Accept {
                key,
                remote,
                link_id,
                initiator,
                done,
            } => {
                let _ = done.complete(self.accept(key, remote, link_id, initiator, now));
            }
            Command::Connect {
                key,
                epoch,
                remote,
                link_id,
                done,
            } => {
                let _ = done.complete(self.connect(key, epoch, remote, link_id, now));
            }
            Command::AttachDevice {
                rail,
                id,
                memory,
                done,
            } => {
                if let Err((error, done)) = self.attach_device(rail, id, memory, done) {
                    let _ = done.complete(Err(error));
                }
            }
            Command::Grant {
                key,
                epoch,
                device,
                transfer,
                length,
                deadline,
                done,
            } => match self.ready(key, epoch) {
                Some((link, pool)) => {
                    link.grant(pool, device, transfer, length, deadline, done, now)
                }
                None => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
            },
            Command::Write {
                key,
                epoch,
                tag,
                source,
                deadline,
                done,
            } => match self.ready(key, epoch) {
                Some((link, _)) => link.write(tag, source, deadline, done, now),
                None => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
            },
            Command::Release {
                key,
                link,
                tag,
                done,
            } => match self.matching(key, link) {
                Some((link, _)) => link.release(tag, done, now),
                None => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
            },
            Command::Break(key, epoch, declined) => {
                self.break_matching(key, epoch, declined, now);
            }
        }
    }

    /// The link for `key` named by `link`, and its pool.
    fn matching(&mut self, key: LinkKey, link: LinkMatch) -> Option<(&mut Link, &Pool)> {
        match link {
            LinkMatch::Epoch(epoch) => self.ready(key, epoch),
            LinkMatch::Id(id) => {
                let link = self
                    .links
                    .get_mut(&key)
                    .filter(|link| link.phase() == Phase::Ready && link.link_id() == id)?;
                let device = self.devices.get(&key.rail)?;
                Some((link, &device.pool))
            }
        }
    }

    /// The link for `key` at `epoch` and its pool.
    fn ready(&mut self, key: LinkKey, epoch: Epoch) -> Option<(&mut Link, &Pool)> {
        let link = self
            .links
            .get_mut(&key)
            .filter(|link| link.epoch() == epoch)?;
        let device = self.devices.get(&key.rail)?;
        Some((link, &device.pool))
    }

    /// Release what a failed [`Self::activate`] built. Its devices and
    /// regions are already dropped, so the charge returns unless a native
    /// object leaked against it.
    fn abandon_activation(&mut self) {
        if self.settings.is_some() || !self.devices.is_empty() || self.activating.is_some() {
            return;
        }

        if self
            .quota
            .take()
            .is_some_and(|q| q.quarantined() || Arc::strong_count(&q) != 1)
        {
            self.leaked = true;
        }
    }

    /// The owner's wake, for registrations finishing off this thread.
    fn register_wake(&self) -> RegisterWake {
        let shared = self.shared.clone();
        Arc::new(move || shared.owner.wake())
    }

    /// Start registering `memory` as `rail`'s device pool `id`; `done`
    /// completes once it is registered (see [`Self::advance_registrations`]).
    #[allow(clippy::type_complexity)]
    fn attach_device(
        &mut self,
        rail: u16,
        id: u32,
        memory: DeviceMemory,
        done: Completer<usize>,
    ) -> std::result::Result<(), (Error, Completer<usize>)> {
        #[cfg(any(test, feature = "simulation"))]
        let _simulation = self
            .simulation
            .as_ref()
            .map(crate::simulation::Simulation::enter);
        let Some(quota) = self.quota.clone() else {
            return Err((Error::InvalidRequest, done));
        };
        let wake = self.register_wake();
        let Some(state) = self.devices.get(&rail) else {
            return Err((Error::InvalidRequest, done));
        };

        if state.pool.device_shared(id).is_some()
            || self.attaching.iter().any(|a| a.rail == rail && a.id == id)
        {
            return Err((Error::InvalidRequest, done));
        }

        match Registering::dmabuf(state.device.clone(), memory, quota, wake) {
            Ok(registering) => {
                self.attaching.push(Attaching {
                    rail,
                    id,
                    registering: Some(registering),
                    done,
                });
                Ok(())
            }
            Err(error) => Err((error, done)),
        }
    }

    /// Finish registrations that completed off this thread: build the
    /// activation's pools once all of them are in, and attach device pools.
    fn advance_registrations(&mut self) {
        if self.activating.is_none() && self.attaching.is_empty() {
            return;
        }

        #[cfg(any(test, feature = "simulation"))]
        let _simulation = self
            .simulation
            .as_ref()
            .map(crate::simulation::Simulation::enter);
        self.advance_activation();

        let mut index = 0;

        while index < self.attaching.len() {
            if !self.attaching[index]
                .registering
                .as_ref()
                .is_some_and(Registering::ready)
            {
                index += 1;
                continue;
            }

            let mut attaching = self.attaching.swap_remove(index);
            let region = attaching.registering.take().expect("ready").finish();
            let result =
                region.and_then(|region| self.attach_region(attaching.rail, attaching.id, region));
            let _ = attaching.done.complete(result);
        }
    }

    /// Attach a registered dma-buf region as `rail`'s device pool `id`.
    fn attach_region(&mut self, rail: u16, id: u32, region: Rc<NativeRegion>) -> Result<usize> {
        if self.closing {
            return Err(Error::Unavailable);
        }

        let state = self.devices.get_mut(&rail).ok_or(Error::Unavailable)?;
        state.pool.attach_device(id, region)?;
        // The driver waker reaches the device pool on its next registration.
        let shared = state.pool.device_shared(id).expect("just attached").clone();
        let count = shared.count();
        self.shared
            .device_pools
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .insert((rail, id), shared);
        Ok(count)
    }

    /// Complete the activation once every page pool registration is done.
    fn advance_activation(&mut self) {
        let Some(activation) = &mut self.activating else {
            return;
        };
        let mut pending = false;

        for device in &mut activation.devices {
            match device.implicit_registering.as_ref().map(Registering::ready) {
                Some(true) => {
                    let registering = device.implicit_registering.take().expect("ready");
                    device.implicit = Some(registering.finish());
                }
                Some(false) => pending = true,
                None => {}
            }
            match device.registering.as_ref().map(Registering::ready) {
                Some(true) => {
                    device.region = Some(device.registering.take().expect("ready").finish());
                }
                Some(false) => pending = true,
                None => {}
            }
        }

        if pending {
            return;
        }

        let activation = self.activating.take().expect("checked");
        let result = self.install(
            activation.devices,
            activation.buffer_len,
            activation.settings,
        );
        self.dirty = true;

        match result {
            Ok(()) => {
                let _ = activation.done.complete(Ok(activation.selected));
            }
            Err(error) => {
                self.abandon_activation();
                let _ = activation.done.complete(Err(error));
            }
        }
    }

    /// Build the pools of a fully registered activation and serve them.
    fn install(
        &mut self,
        devices: Vec<PendingDevice>,
        buffer_len: usize,
        settings: Settings,
    ) -> Result<()> {
        if self.closing {
            return Err(Error::Unavailable);
        }

        let quota = self.quota.clone().ok_or(Error::Unavailable)?;
        let mut built = BTreeMap::new();

        for pending in devices {
            let region = pending.region.ok_or(Error::Unavailable)??;
            let implicit = pending.implicit.ok_or(Error::Unavailable)??;
            let pool = Pool::new(region, implicit, buffer_len, quota.clone())?;
            built.insert(
                pending.tag,
                DeviceState {
                    pool,
                    device: pending.device,
                },
            );
        }

        {
            let mut pools = self
                .shared
                .pools
                .write()
                .unwrap_or_else(PoisonError::into_inner);
            pools.clear();
            pools.extend(
                built
                    .iter()
                    .map(|(rail, d)| (*rail, d.pool.shared().clone())),
            );
        }

        {
            let mut prefetch = self
                .shared
                .prefetch
                .write()
                .unwrap_or_else(PoisonError::into_inner);
            prefetch.clear();
            prefetch.extend(
                built
                    .iter()
                    .filter(|(_, d)| d.device.mode == ffi::Mode::Window)
                    .filter_map(|(rail, d)| Some((*rail, d.pool.implicit().prefetcher()?))),
            );
        }

        self.devices = built;
        self.settings = Some(settings);
        Ok(())
    }

    /// Discover, select, and start registering one page pool per selected
    /// device off this thread. `done` completes once they are registered
    /// (see [`Self::advance_activation`]); on an early error it completes
    /// here and the error is returned.
    fn activate(&mut self, config: NodeConfig, done: Completer<Vec<Selection>>) -> Result<()> {
        match self.start_activation(config) {
            Ok((selected, devices, buffer_len, settings)) => {
                self.activating = Some(Activation {
                    selected,
                    devices,
                    buffer_len,
                    settings,
                    done,
                });
                Ok(())
            }
            Err(error) => {
                let _ = done.complete(Err(error));
                Err(error)
            }
        }
    }

    #[allow(clippy::type_complexity)]
    fn start_activation(
        &mut self,
        config: NodeConfig,
    ) -> Result<(Vec<Selection>, Vec<PendingDevice>, usize, Settings)> {
        if self.settings.is_some() || !self.devices.is_empty() || self.activating.is_some() {
            return Err(Error::InvalidRequest);
        }

        let buffer_len =
            u32::try_from(config.buffer_len).map_err(|_| Error::InvalidConfiguration)?;

        if buffer_len == 0
            || config.pool_bytes / config.buffer_len == 0
            || config.max_links == 0
            || link::depths(config.grants_per_link).is_none()
            || config.idle.is_zero()
        {
            return Err(Error::InvalidConfiguration);
        }

        #[cfg(any(test, feature = "simulation"))]
        let _simulation = self
            .simulation
            .as_ref()
            .map(crate::simulation::Simulation::enter);
        let quota = GuardOwner::new(config.guard);
        self.quota = Some(quota.clone());
        let discovered = ffi::discover_with_quota(|| Some(quota.clone()))?;
        let ports: Vec<PortInfo> = discovered
            .iter()
            .map(|d| PortInfo {
                device: d.name.clone(),
                port: d.endpoint.port,
                gid: d.endpoint.gid,
                numa_node: d.numa_node(),
            })
            .collect();
        let plan = (config.selector)(&ports)?;
        let mut selected = Vec::new();

        for (rail, index) in plan {
            if selected
                .iter()
                .any(|s: &Selection| s.tag == u32::from(rail) || s.index == index)
            {
                return Err(Error::InvalidConfiguration);
            }

            let port = ports.get(index).ok_or(Error::InvalidConfiguration)?.clone();
            selected.push(Selection {
                tag: rail.into(),
                index,
                port,
            });
        }

        let mut devices: Vec<Option<NativeDevice>> = discovered.into_iter().map(Some).collect();
        let count = config.pool_bytes / config.buffer_len;
        let mut pending = Vec::new();

        for selection in &selected {
            let device = Rc::new(
                devices[selection.index]
                    .take()
                    .ok_or(Error::InvalidConfiguration)?,
            );
            // Registration-mode grants round up to whole 2 MiB pages within
            // their buffer, so buffers must be whole pages.
            if device.mode == ffi::Mode::Registration
                && !(config.buffer_len as u64).is_multiple_of(ffi::HUGE)
            {
                return Err(Error::InvalidConfiguration);
            }
            // Writes read caller memory in place; there is no copy path.
            // Check implicit ODP support first (window mode) so an
            // unsupported device fails before anything slow starts.
            let implicit = match device.mode {
                ffi::Mode::Registration => {
                    Some(Ok(NativeRegion::implicit(device.clone(), quota.clone())
                        .ok_or(Error::Unavailable)?))
                }
                ffi::Mode::Window if !device.supports_implicit() => {
                    return Err(Error::Unavailable);
                }
                ffi::Mode::Window => None,
            };
            pending.push(PendingDevice {
                tag: selection.tag as u16,
                registering: None,
                region: None,
                implicit_registering: None,
                implicit,
                device,
            });
        }

        // Each link's QP and CQ must fit every selected device's limits, or
        // QP creation fails on every link. The setup exchange takes
        // min(local, peer), so capping the local value suffices.
        let mut grants = config.grants_per_link;
        for device in &pending {
            grants = grants.min(device.device.max_grants().ok_or(Error::Unavailable)?);
        }

        // Every check passed: start the slow registrations, each on its own
        // helper thread, so the devices proceed in parallel. Creating an
        // implicit ODP region alone takes over a second per mlx5 device.
        let wake = self.register_wake();

        for device in &mut pending {
            if device.implicit.is_none() {
                device.implicit_registering =
                    ffi::Registering::implicit(device.device.clone(), quota.clone(), wake.clone())?;
            }
            device.registering = Some(Registering::host(
                device.device.clone(),
                count * config.buffer_len,
                Access::PAGES,
                quota.clone(),
                wake.clone(),
            )?);
        }

        let settings = Settings {
            limits: Limits {
                grants,
                buffer_len,
                idle: config.idle,
            },
            max_links: config.max_links,
        };
        Ok((selected, pending, config.buffer_len, settings))
    }

    /// A new link on `key.rail`, if the rail exists and has room.
    ///
    /// Live and dying links are counted separately, each against
    /// `max_links`: a link for `key` that the new one replaces does not count
    /// as live, so a replacement on a full rail is admitted, and dying links
    /// (whose QPs still exist until destroyed) cannot crowd out live ones.
    fn create(&mut self, key: LinkKey, role: Role, now: Instant) -> Result<Link> {
        let settings = self.settings.ok_or(Error::Unavailable)?;
        let device = self.devices.get(&key.rail).ok_or(Error::Unavailable)?;
        let live = self
            .links
            .keys()
            .filter(|k| k.rail == key.rail && **k != key)
            .count();
        let dying = self.dying.iter().filter(|d| d.key.rail == key.rail).count();

        if live >= settings.max_links || dying >= settings.max_links {
            return Err(Error::Overloaded);
        }

        let epoch = Epoch(self.next_epoch);
        self.next_epoch += 1;
        let quota = self.quota.as_ref().ok_or(Error::Unavailable)?;

        Link::new(
            epoch,
            role,
            &device.device,
            quota,
            settings.limits,
            now,
            &self.ctx.rng,
        )
    }

    fn open(&mut self, key: LinkKey, now: Instant) -> Result<Opened> {
        if self.links.contains_key(&key)
            || self.dying.iter().any(|d| d.key == key)
            || self.backoff.get(&key).is_some_and(|b| now < b.until)
        {
            return Err(Error::Overloaded);
        }

        let link = match self.create(key, Role::Initiator, now) {
            Ok(link) => link,
            Err(error) => {
                // A missing rail is not a failure of this link.
                if error != Error::Unavailable {
                    self.back_off(key, false, now);
                }
                return Err(error);
            }
        };
        let opened = opened(&link);
        self.links.insert(key, link);
        self.dirty = true;
        Ok(opened)
    }

    fn accept(
        &mut self,
        key: LinkKey,
        remote: Setup,
        link_id: u64,
        initiator: bool,
        now: Instant,
    ) -> Result<Opened> {
        if remote.recv_credits < 2 || remote.grants_per_link == 0 {
            return Err(Error::InvalidRequest);
        }

        let role = if initiator {
            Role::Initiator
        } else {
            Role::Responder
        };
        // Build the replacement before retiring the old link, so a refused
        // or failed setup leaves the current link in place.
        let mut link = match self.create(key, role, now) {
            Ok(link) => link,
            Err(error) => {
                // A missing rail is not a failure of this link.
                if error != Error::Unavailable {
                    self.back_off(key, false, now);
                }
                return Err(error);
            }
        };
        if let Err(error) = link.connect(
            remote.endpoint,
            remote.recv_credits,
            remote.grants_per_link,
            link_id,
            now,
        ) {
            self.dying.push(Dying { key, link });
            self.back_off(key, false, now);
            self.dirty = true;
            return Err(error);
        }
        let opened = opened(&link);
        // An authenticated setup replaces whatever we had.
        if let Some(old) = self.links.insert(key, link) {
            self.dying.push(Dying { key, link: old });
        }
        self.dirty = true;
        Ok(opened)
    }

    fn connect(
        &mut self,
        key: LinkKey,
        epoch: Epoch,
        remote: Setup,
        link_id: u64,
        now: Instant,
    ) -> Result<()> {
        let link = self
            .links
            .get_mut(&key)
            .filter(|link| link.epoch() == epoch)
            .ok_or(Error::Unavailable)?;

        if link.role() != Role::Initiator || link.phase() != Phase::Created {
            return Err(Error::InvalidRequest);
        }

        let result = link.connect(
            remote.endpoint,
            remote.recv_credits,
            remote.grants_per_link,
            link_id,
            now,
        );

        if result.is_err() {
            self.break_link(key, Break::Error, now);
        }

        self.dirty = true;
        result
    }

    fn break_matching(&mut self, key: LinkKey, epoch: Epoch, declined: bool, now: Instant) {
        if self
            .links
            .get(&key)
            .is_some_and(|link| link.epoch() == epoch)
        {
            let reason = if declined {
                BreakReason::Declined
            } else {
                BreakReason::Abandoned
            };
            self.break_for(key, reason, now);
        }
    }

    /// Move `key`'s link to the dying list and record what comes next.
    fn break_link(&mut self, key: LinkKey, reason: Break, now: Instant) {
        self.break_for(key, BreakReason::of(reason), now);
    }

    /// Count `reason` in the shared break counters.
    fn count_break(&self, reason: BreakReason) {
        self.shared.break_counts[reason as usize].fetch_add(1, Ordering::Relaxed);
    }

    fn break_for(&mut self, key: LinkKey, reason: BreakReason, now: Instant) {
        let Some(link) = self.links.remove(&key) else {
            return;
        };

        self.count_break(reason);

        match reason {
            BreakReason::PeerClosed | BreakReason::Closed | BreakReason::GrantExpired => {
                self.backoff.remove(&key);
            }
            // Benign: retry soon, and forget any escalation.
            BreakReason::Declined => self.back_off(key, true, now),
            BreakReason::Error | BreakReason::Timeout | BreakReason::Abandoned => {
                let reset = link
                    .ready_since()
                    .is_some_and(|since| now.saturating_duration_since(since) >= BACKOFF_RESET);
                self.back_off(key, reset, now);
            }
        }

        self.dying.push(Dying { key, link });
        self.dirty = true;
    }

    /// Back `key` off after a failure: double the previous delay unless
    /// `reset`, starting from `BACKOFF_MIN`.
    fn back_off(&mut self, key: LinkKey, reset: bool, now: Instant) {
        let delay = match self.backoff.get(&key) {
            Some(previous) if !reset => (previous.delay * 2).min(BACKOFF_MAX),
            _ => BACKOFF_MIN,
        };
        self.backoff.insert(
            key,
            Backoff {
                until: now + jitter(delay, &self.ctx.rng),
                delay,
            },
        );
        self.dirty = true;
    }

    /// Close links whose keys are not in `keep`, without backoff.
    fn retain_links(&mut self, keep: &std::collections::BTreeSet<LinkKey>, now: Instant) {
        let gone: Vec<LinkKey> = self
            .links
            .keys()
            .filter(|key| !keep.contains(key))
            .copied()
            .collect();

        for key in gone {
            self.backoff.remove(&key);
            let link = self.links.get_mut(&key).unwrap();

            if link.phase() == Phase::Ready {
                link.close(now, link::CLOSE_GRACE);
            } else if !matches!(link.phase(), Phase::Closing { .. }) {
                self.break_link(key, Break::Closed, now);
            }
        }

        self.dirty = true;
    }

    /// Poll every live link and break the failed ones.
    fn progress(&mut self, now: Instant) {
        // Completion channel and connection manager events first: the
        // links below re-arm and poll their CQs.
        // Each device pass lets links skip CQs that raised no event.
        for device in self.devices.values() {
            device.device.begin_pass();
        }

        let mut broken = Vec::new();

        for (key, link) in &mut self.links {
            let Some(device) = self.devices.get(&key.rail) else {
                continue;
            };

            let phase = link.phase();

            let polled = link.poll(&device.pool, now);

            if link.take_grant_expired() {
                self.shared.break_counts[BreakReason::GrantExpired as usize]
                    .fetch_add(1, Ordering::Relaxed);
            }

            if let Err(reason) = polled {
                broken.push((*key, reason));
            } else if link.phase() != phase {
                self.dirty = true;
            }
        }

        for device in self.devices.values() {
            device.device.end_pass();
        }

        for (key, reason) in broken {
            self.break_link(key, reason, now);
        }
    }

    /// Destroy dying links; keep the ones whose destroy failed for a retry.
    fn reap(&mut self) {
        let devices = &self.devices;
        let before = self.dying.len();

        self.dying
            .retain_mut(|dying| match devices.get(&dying.key.rail) {
                Some(device) => !dying.link.destroy(&device.pool),
                None => false,
            });

        if self.dying.len() != before {
            self.dirty = true;
        }
    }

    fn begin_shutdown(&mut self, now: Instant) {
        let keys: Vec<LinkKey> = self.links.keys().copied().collect();

        for key in keys {
            let link = self.links.get_mut(&key).unwrap();

            // Ready links send `Close` and break once it completes or the
            // grace period ends (see `Link::poll`); others break now.
            if link.phase() == Phase::Ready {
                link.close(now, link::CLOSE_GRACE);
            } else {
                self.break_link(key, Break::Closed, now);
            }
        }

        self.dirty = true;
    }

    /// Free pools and devices once nothing uses them, then mark drained.
    fn finish_shutdown(&mut self) {
        if self.shared.drained.load(Ordering::Acquire)
            || self.activating.is_some()
            || !self.attaching.is_empty()
            || !self.links.is_empty()
            || !self.dying.is_empty()
        {
            return;
        }

        // Clients read receive-pool state through these; withdraw them before
        // checking that every device pool is idle.
        // Readers hold these locks only to count buffers.
        self.shared
            .pools
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        self.shared
            .device_pools
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        // Before any implicit region can drop.
        self.shared
            .prefetch
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();

        if self.devices.values().any(|d| !d.pool.idle()) {
            return;
        }

        self.devices.clear();
        self.backoff.clear();

        if self
            .quota
            .take()
            .is_some_and(|q| q.quarantined() || Arc::strong_count(&q) != 1)
        {
            // A native object leaked against the charge: never drained.
            self.leaked = true;
        }

        if self.leaked {
            return;
        }

        self.shared.drained.store(true, Ordering::Release);
    }

    /// Publish the link snapshot if anything changed or a backoff expired.
    fn publish(&mut self, now: Instant) {
        if self.expiry.is_some_and(|at| now >= at) {
            self.dirty = true;
        }

        if !self.dirty {
            return;
        }

        let mut links = LinkMap::new();
        self.expiry = None;

        for (key, backoff) in &self.backoff {
            if now < backoff.until {
                links.insert(*key, LinkState::Backoff(backoff.until));
                self.expiry = Some(
                    self.expiry
                        .map_or(backoff.until, |at| at.min(backoff.until)),
                );
            }
        }

        for dying in &self.dying {
            links.insert(dying.key, LinkState::Breaking);
        }

        for (key, link) in &self.links {
            let state = match link.phase() {
                Phase::Created | Phase::Connecting => LinkState::Connecting(link.epoch()),
                Phase::Ready => LinkState::Ready {
                    epoch: link.epoch(),
                    link_id: link.link_id(),
                },
                Phase::Closing { .. } => LinkState::Breaking,
            };
            links.insert(*key, state);
        }

        self.shared.links.store(links);
        self.dirty = false;
    }
}

impl Drop for Owner {
    /// Fail every op, destroy every link (leaking what cannot be
    /// destroyed), and leak pools that are still in use.
    fn drop(&mut self) {
        self.shared.alive.store(false, Ordering::Release);
        self.shared.closed.store(true, Ordering::Release);
        let keys: Vec<LinkKey> = self.links.keys().copied().collect();

        for key in keys {
            if let Some(link) = self.links.remove(&key) {
                self.dying.push(Dying { key, link });
            }
        }

        for mut dying in self.dying.drain(..) {
            if let Some(device) = self.devices.get(&dying.key.rail)
                && !dying.link.destroy(&device.pool)
            {
                dying.link.leak(&device.pool);
            }
        }

        self.shared.links.store(LinkMap::new());
        self.shared
            .pools
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        self.shared
            .device_pools
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        // Before any implicit region can drop.
        self.shared
            .prefetch
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        // Pools leak their regions if pages are still held (see `pool.rs`).
        self.devices.clear();
    }
}

/// True when an owner with `pending` work has neither a timer (`wake`) nor
/// an `external` wake source for it.
#[cfg(any(test, feature = "simulation"))]
fn missing_wake(pending: bool, wake: Option<Instant>, external: bool) -> bool {
    pending && wake.is_none() && !external
}

/// Fail `command`'s op with `error`.
fn fail(command: Command, error: Error) {
    match command {
        Command::Activate(_, done) => drop(done.complete(Err(error))),
        Command::Open(_, done) | Command::Accept { done, .. } => drop(done.complete(Err(error))),
        Command::Connect { done, .. }
        | Command::Write { done, .. }
        | Command::Release { done, .. } => drop(done.complete(Err(error))),
        Command::Grant { done, .. } => drop(done.complete(Err(error))),
        Command::AttachDevice { done, .. } => drop(done.complete(Err(error))),
        Command::Break(..) => {}
    }
}

fn opened(link: &Link) -> Opened {
    Opened {
        epoch: link.epoch(),
        setup: Setup {
            endpoint: link.endpoint(),
            recv_credits: link.recv_credits(),
            grants_per_link: link.grant_limit(),
        },
    }
}

/// `delay` plus or minus up to 20 %.
fn jitter(delay: Duration, rng: &racer_io::Rng) -> Duration {
    let mut bytes = [0u8; 2];

    if rng.fill(&mut bytes).is_err() {
        return delay;
    }

    // 0..=400 per mille, centered: 800..=1200.
    let per_mille = 800 + u64::from(u16::from_be_bytes(bytes)) % 401;
    delay * per_mille as u32 / 1000
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    /// Jitter stays within 20 % of the delay.
    fn jitter_is_bounded() {
        for _ in 0..100 {
            let d = jitter(Duration::from_secs(10), &racer_io::Rng::os());
            assert!(
                d >= Duration::from_secs(8) && d <= Duration::from_secs(12),
                "{d:?}"
            );
        }
    }

    #[test]
    /// The wake check flags only pending work with no timer and no external
    /// wake.
    fn missing_wake_needs_pending_work_and_no_source() {
        let now = Instant::now();
        assert!(missing_wake(true, None, false));
        assert!(!missing_wake(true, Some(now), false));
        assert!(!missing_wake(true, None, true));
        assert!(!missing_wake(false, None, false));
    }

    #[test]
    /// Clients and the port can cross threads; zero workers is refused.
    fn handles_are_send() {
        fn send<T: Send>() {}
        send::<Client>();
        send::<OwnerPort>();
        send::<Source>();
        assert_eq!(node(0).err(), Some(Error::InvalidConfiguration));
    }

    #[test]
    /// A reader racing a stream of publishes never sees a ready link as
    /// `Idle`: every published table holds the link, and a read either
    /// reuses its cached table or waits for the swap.
    fn contended_snapshot_never_reads_idle() {
        let (clients, port) = node(1).unwrap();
        let key = LinkKey {
            peer: [1; 32],
            rail: 0,
        };
        let client = clients.into_iter().next().unwrap();
        let ready = |n: u64| LinkState::Ready {
            epoch: Epoch(n),
            link_id: n,
        };
        port.shared.links.store(LinkMap::from([(key, ready(0))]));
        let stop = Arc::new(AtomicBool::new(false));
        let reader = {
            let stop = stop.clone();
            std::thread::spawn(move || {
                let mut reads = 0u64;
                while !stop.load(Ordering::Acquire) || reads < 1000 {
                    assert!(matches!(client.link(key), LinkState::Ready { .. }));
                    reads += 1;
                }
                // A later read sees the last publish.
                client.link(key)
            })
        };
        for n in 1..=20_000 {
            port.shared.links.store(LinkMap::from([(key, ready(n))]));
        }
        stop.store(true, Ordering::Release);
        assert_eq!(reader.join().unwrap(), ready(20_000));
    }

    #[test]
    /// A read reuses the cached table until the generation moves.
    fn snapshot_read_tracks_generation() {
        let (clients, port) = node(1).unwrap();
        let key = LinkKey {
            peer: [2; 32],
            rail: 1,
        };
        assert_eq!(clients[0].link(key), LinkState::Idle);
        port.shared
            .links
            .store(LinkMap::from([(key, LinkState::Breaking)]));
        assert_eq!(clients[0].link(key), LinkState::Breaking);
        port.shared.links.store(LinkMap::new());
        assert_eq!(clients[0].link(key), LinkState::Idle);
    }

    #[test]
    /// Busy or failed commands fail the op with the right error.
    fn failed_commands_report_their_error() {
        let (clients, port) = node(1).unwrap();
        let key = LinkKey {
            peer: [1; 32],
            rail: 0,
        };
        let mut ops: Vec<_> = (0..COMMAND_QUEUE).map(|_| clients[0].open(key)).collect();
        let overflow = clients[0].open(key);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert_eq!(
            overflow.poll(&mut cx).map(|r| r.err()),
            Poll::Ready(Some(Error::Overloaded))
        );
        // A break never gets lost even with a full queue.
        clients[0].break_link(key, Epoch(1));
        assert_eq!(port.shared.breaks.lock().unwrap().len(), 1);
        drop(port);
        for op in ops.drain(..) {
            assert_eq!(
                op.poll(&mut cx).map(|r| r.err()),
                Poll::Ready(Some(Error::Canceled))
            );
        }
        assert_eq!(
            clients[0].open(key).poll(&mut cx).map(|r| r.err()),
            Poll::Ready(Some(Error::Unavailable))
        );
        assert_eq!(clients[0].reopen(), Err(Error::Unavailable));
    }
}
