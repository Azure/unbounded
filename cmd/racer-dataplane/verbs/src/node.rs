//! The node owner and per-worker clients for persistent RDMA links.
//!
//! See `designs/racer-verbs.md`, "Public API", and
//! `designs/racer-rdma-sessions.md`, "Link table and state machine".
//!
//! [`node`] returns one [`Client`] per I/O worker and one [`OwnerPort`].
//! Build the [`Owner`] from the port on the native thread and host it with
//! [`crate::WithNative::with_host`] (or poll it with
//! [`Owner::poll_budgeted`]). Every libibverbs call runs on that thread.
//! Clients queue commands and get results through [`Op`]s.

use std::{
    collections::BTreeMap,
    rc::Rc,
    sync::{
        Arc, Mutex, PoisonError, RwLock,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};

use futures::task::AtomicWaker;

use crate::{
    Endpoint, Error, Guard, GuardOwner, PortInfo, Result, Selection,
    channel::{self, Completer, Op, op},
    ffi::{self, Access, NativeDevice, NativeRegion},
    link::{self, Break, Epoch, Granted, Limits, Link, Phase, Role, Source},
    pool::{BufferState, Pool, PoolShared, SendBuffer},
};

/// Commands each worker may queue before [`Error::Overloaded`].
pub const COMMAND_QUEUE: usize = 64;

/// First backoff after a failed link.
pub const BACKOFF_MIN: Duration = Duration::from_secs(1);

/// Longest backoff.
pub const BACKOFF_MAX: Duration = Duration::from_secs(60);

/// A link ready this long resets its backoff.
pub const BACKOFF_RESET: Duration = Duration::from_secs(60);

pub use link::SETUP_TIMEOUT;

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

    /// Registered bytes per selected device, split into page buffers.
    pub pool_bytes: usize,

    /// Page buffer length (Racer: `MAX_CIPHERTEXT` rounded up to 4 KiB).
    pub buffer_len: usize,

    /// Max links (QPs) per device.
    pub max_links: usize,

    /// Max concurrent grants either side may hold, per link.
    pub grants_per_link: u32,

    /// A ready link with no grant, write, or release this long closes.
    pub idle: Duration,

    /// Register page memory with remote write as well (provider escape
    /// hatch, see "Least access").
    pub mr_remote_write: bool,

    /// Charged for every native object of this activation; leaks with any
    /// leaked object.
    pub guard: Guard,
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

    Grant {
        key: LinkKey,
        epoch: Epoch,
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
        epoch: Epoch,
        tag: u32,
        done: Completer<()>,
    },

    Break(LinkKey, Epoch),
}

/// State shared by the owner and every client.
struct NodeShared {
    links: RwLock<BTreeMap<LinkKey, LinkState>>,

    pools: RwLock<BTreeMap<u16, Arc<PoolShared>>>,

    /// Breaks that did not fit in a command queue.
    breaks: Mutex<Vec<(LinkKey, Epoch)>>,

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
        links: RwLock::new(BTreeMap::new()),
        pools: RwLock::new(BTreeMap::new()),
        breaks: Mutex::new(Vec::new()),
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
    /// selected device. Once per round.
    pub fn activate(&self, config: NodeConfig) -> Op<Vec<Selection>> {
        self.submit(|done| Command::Activate(config, done))
    }

    /// Snapshot of `key`'s link. A busy snapshot reads as [`LinkState::Idle`]
    /// (callers then use HTTP or get `Overloaded` from `open`).
    pub fn link(&self, key: LinkKey) -> LinkState {
        match self.shared.links.try_read() {
            Ok(links) => links.get(&key).copied().unwrap_or(LinkState::Idle),
            Err(_) => LinkState::Idle,
        }
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
            transfer,
            length,
            deadline,
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

    /// Sender: decline the peer's grant `tag`. Resolves on the send
    /// completion.
    pub fn release(&self, key: LinkKey, epoch: Epoch, tag: u32) -> Op<()> {
        self.submit(|done| Command::Release {
            key,
            epoch,
            tag,
            done,
        })
    }

    /// Break the link now (QP to ERR and destroy) and back off. Outstanding
    /// ops fail with `Unavailable`. Never lost: a full queue falls back to a
    /// shared list.
    pub fn break_link(&self, key: LinkKey, epoch: Epoch) {
        if let Err((_, Command::Break(key, epoch))) =
            self.worker.try_send(Command::Break(key, epoch))
        {
            self.shared
                .breaks
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .push((key, epoch));
            self.shared.owner.wake();
        }
    }

    /// Take a free page buffer of `rail` to stage `len` bytes for
    /// [`Source::Staged`]. `Pending` until one returns.
    pub fn send_buffer(
        &self,
        rail: u16,
        len: usize,
        cx: &mut Context<'_>,
    ) -> Poll<Result<SendBuffer>> {
        let pool = match self.shared.pools.try_read() {
            Ok(pools) => pools.get(&rail).cloned(),
            Err(_) => return Poll::Pending,
        };
        let Some(pool) = pool else {
            return Poll::Ready(Err(Error::Unavailable));
        };

        match pool.poll_stage(len, cx.waker()) {
            Some(result) => Poll::Ready(result),
            None => Poll::Pending,
        }
    }

    /// Take a free page buffer of `rail` without waiting. `Ok(None)` when
    /// none is free right now or the pool table is busy; registers no waker.
    pub fn try_send_buffer(&self, rail: u16, len: usize) -> Result<Option<SendBuffer>> {
        let pool = match self.shared.pools.try_read() {
            Ok(pools) => pools.get(&rail).cloned(),
            Err(_) => return Ok(None),
        };
        let Some(pool) = pool else {
            return Err(Error::Unavailable);
        };

        pool.try_stage(len)
    }

    /// Page buffers of `rail` in `state`, for diagnostics.
    pub fn buffers(&self, rail: u16, state: BufferState) -> Option<usize> {
        let pools = self.shared.pools.try_read().ok()?;
        Some(pools.get(&rail)?.count_in(state))
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

/// Runs on the native thread and owns every link, pool, and device.
///
/// Not `Send`. Poll it with [`Self::poll_budgeted`], or host it with
/// [`crate::WithNative::with_host`].
pub struct Owner {
    environment: racer_platform::environment::Environment,

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
}

impl Owner {
    /// Build the owner. Call on the native thread; it captures that
    /// thread's environment (and simulation, in tests).
    pub fn new(port: OwnerPort) -> Self {
        // Move the fields out; `OwnerPort::drop` must not mark the node dead.
        let port = std::mem::ManuallyDrop::new(port);
        // SAFETY: each field is read once out of a `ManuallyDrop`.
        let (channel, shared) =
            unsafe { (std::ptr::read(&port.channel), std::ptr::read(&port.shared)) };

        Self {
            environment: racer_platform::environment::Environment::current(),
            #[cfg(any(test, feature = "simulation"))]
            simulation: crate::simulation::current(),
            channel,
            shared,
            links: BTreeMap::new(),
            dying: Vec::new(),
            devices: BTreeMap::new(),
            quota: None,
            settings: None,
            backoff: BTreeMap::new(),
            next_epoch: 1,
            closing: false,
            dirty: false,
            expiry: None,
            leaked: false,
        }
    }

    /// Wake `waker` on commands, closes, and returned buffers. Completions
    /// and timers need periodic polls as well.
    pub fn register_driver(&self, waker: &Waker) {
        self.channel.register(waker);
        self.shared.owner.register(waker);

        for device in self.devices.values() {
            device.pool.shared().register_owner(waker);
        }
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

        let _environment = self.environment.enter();
        let now = racer_platform::environment::now();
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

        let mut commands = Vec::new();
        self.channel
            .try_drain(budget, |_, command| commands.push(command));

        for command in commands {
            self.handle(command, now);
        }

        let breaks = std::mem::take(
            &mut *self
                .shared
                .breaks
                .lock()
                .unwrap_or_else(PoisonError::into_inner),
        );

        for (key, epoch) in breaks {
            self.break_matching(key, epoch, now);
        }

        self.progress(now);
        self.reap();

        if self.closing {
            self.finish_shutdown();
        }

        self.publish(now);
        Ok(())
    }

    fn handle(&mut self, command: Command, now: Instant) {
        if self.closing {
            // Dropping the command fails its op with `Cancelled`; say why.
            fail(command, Error::Unavailable);
            return;
        }

        match command {
            Command::Activate(config, done) => {
                let result = self.activate(config);
                self.dirty = true;
                let _ = done.complete(result);
            }
            Command::Open(key, done) => {
                let _ = done.complete(self.open(key, now));
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
            Command::Grant {
                key,
                epoch,
                transfer,
                length,
                deadline,
                done,
            } => match self.ready(key, epoch) {
                Some((link, pool)) => link.grant(pool, transfer, length, deadline, done, now),
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
                Some((link, pool)) => link.write(pool, tag, source, deadline, done, now),
                None => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
            },
            Command::Release {
                key,
                epoch,
                tag,
                done,
            } => match self.ready(key, epoch) {
                Some((link, _)) => link.release(tag, done, now),
                None => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
            },
            Command::Break(key, epoch) => self.break_matching(key, epoch, now),
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

    fn activate(&mut self, config: NodeConfig) -> Result<Vec<Selection>> {
        if self.settings.is_some() || !self.devices.is_empty() {
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
        let access = if config.mr_remote_write {
            Access::PAGES_REMOTE_WRITE
        } else {
            Access::PAGES
        };
        let count = config.pool_bytes / config.buffer_len;
        let mut built = BTreeMap::new();

        for selection in &selected {
            let device = Rc::new(
                devices[selection.index]
                    .take()
                    .ok_or(Error::InvalidConfiguration)?,
            );
            let region = NativeRegion::with_access(
                device.clone(),
                count * config.buffer_len,
                access,
                quota.clone(),
            )?;
            let pool = Pool::new(region, config.buffer_len, quota.clone())?;
            built.insert(selection.tag as u16, DeviceState { pool, device });
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

        self.devices = built;
        self.settings = Some(Settings {
            limits: Limits {
                grants: config.grants_per_link,
                buffer_len,
                idle: config.idle,
            },
            max_links: config.max_links,
        });
        Ok(selected)
    }

    /// A new link on `key.rail`, if the rail exists and has room.
    fn create(&mut self, key: LinkKey, role: Role, now: Instant) -> Result<Link> {
        let settings = self.settings.ok_or(Error::Unavailable)?;
        let device = self.devices.get(&key.rail).ok_or(Error::Unavailable)?;
        let used = self.links.keys().filter(|k| k.rail == key.rail).count()
            + self.dying.iter().filter(|d| d.key.rail == key.rail).count();

        if used >= settings.max_links {
            return Err(Error::Overloaded);
        }

        let epoch = Epoch(self.next_epoch);
        self.next_epoch += 1;
        let quota = self.quota.as_ref().ok_or(Error::Unavailable)?;

        Link::new(epoch, role, &device.device, quota, settings.limits, now)
    }

    fn open(&mut self, key: LinkKey, now: Instant) -> Result<Opened> {
        if self.links.contains_key(&key)
            || self.dying.iter().any(|d| d.key == key)
            || self.backoff.get(&key).is_some_and(|b| now < b.until)
        {
            return Err(Error::Overloaded);
        }

        let link = self.create(key, Role::Initiator, now)?;
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
        if remote.recv_credits < 2 {
            return Err(Error::InvalidRequest);
        }

        // An authenticated setup replaces whatever we had.
        if let Some(old) = self.links.remove(&key) {
            self.dying.push(Dying { key, link: old });
            self.dirty = true;
        }

        let role = if initiator {
            Role::Initiator
        } else {
            Role::Responder
        };
        let mut link = self.create(key, role, now)?;
        link.connect(remote.endpoint, remote.recv_credits, link_id, now)?;
        let opened = opened(&link);
        self.links.insert(key, link);
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

        let result = link.connect(remote.endpoint, remote.recv_credits, link_id, now);

        if result.is_err() {
            self.break_link(key, Break::Error, now);
        }

        self.dirty = true;
        result
    }

    fn break_matching(&mut self, key: LinkKey, epoch: Epoch, now: Instant) {
        if self
            .links
            .get(&key)
            .is_some_and(|link| link.epoch() == epoch)
        {
            self.break_link(key, Break::Error, now);
        }
    }

    /// Move `key`'s link to the dying list and record what comes next.
    fn break_link(&mut self, key: LinkKey, reason: Break, now: Instant) {
        let Some(link) = self.links.remove(&key) else {
            return;
        };

        match reason {
            Break::PeerClosed | Break::Closed => {
                self.backoff.remove(&key);
            }
            Break::Error | Break::Timeout => {
                let reset = link
                    .ready_since()
                    .is_some_and(|since| now.saturating_duration_since(since) >= BACKOFF_RESET);
                let delay = match self.backoff.get(&key) {
                    Some(previous) if !reset => (previous.delay * 2).min(BACKOFF_MAX),
                    _ => BACKOFF_MIN,
                };
                self.backoff.insert(
                    key,
                    Backoff {
                        until: now + jitter(delay),
                        delay,
                    },
                );
            }
        }

        self.dying.push(Dying { key, link });
        self.dirty = true;
    }

    /// Poll every live link and break the failed ones.
    fn progress(&mut self, now: Instant) {
        let mut broken = Vec::new();

        for (key, link) in &mut self.links {
            let Some(device) = self.devices.get(&key.rail) else {
                continue;
            };

            let phase = link.phase();

            if let Err(reason) = link.poll(&device.pool, now) {
                broken.push((*key, reason));
            } else if link.phase() != phase {
                self.dirty = true;
            }
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
            || !self.links.is_empty()
            || !self.dying.is_empty()
        {
            return;
        }

        // Clients stage from these; withdraw them before counting holders.
        match self.shared.pools.try_write() {
            Ok(mut pools) => pools.clear(),
            Err(_) => return,
        }

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

        let Ok(mut links) = self.shared.links.try_write() else {
            return;
        };

        links.clear();
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

        self.dirty = false;
    }
}

impl Drop for Owner {
    /// Fail every op, destroy every link (leaking what cannot be
    /// destroyed), and leak pools that are still in use.
    fn drop(&mut self) {
        let _environment = self.environment.enter();
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

        self.shared
            .links
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        self.shared
            .pools
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
        // Pools leak their regions if pages are still held (see `pool.rs`).
        self.devices.clear();
    }
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
        Command::Break(..) => {}
    }
}

fn opened(link: &Link) -> Opened {
    Opened {
        epoch: link.epoch(),
        setup: Setup {
            endpoint: link.endpoint(),
            recv_credits: link.recv_credits(),
        },
    }
}

/// `delay` plus or minus up to 20 %.
fn jitter(delay: Duration) -> Duration {
    let mut bytes = [0u8; 2];

    if racer_platform::environment::fill_random(&mut bytes).is_err() {
        return delay;
    }

    // 0..=400 per mille, centered: 800..=1200.
    let per_mille = 800 + u64::from(u16::from_be_bytes(bytes)) % 401;
    delay * per_mille as u32 / 1000
}

impl crate::NativeHost for Owner {
    fn register_driver(&self, waker: &Waker) {
        Owner::register_driver(self, waker);
    }

    fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        Owner::poll_budgeted(self, budget)
    }

    fn close(&self) {
        Owner::close(self);
    }

    fn drained(&self) -> bool {
        Owner::drained(self)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    /// Jitter stays within 20 % of the delay.
    fn jitter_is_bounded() {
        for _ in 0..100 {
            let d = jitter(Duration::from_secs(10));
            assert!(
                d >= Duration::from_secs(8) && d <= Duration::from_secs(12),
                "{d:?}"
            );
        }
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
                Poll::Ready(Some(Error::Cancelled))
            );
        }
        assert_eq!(
            clients[0].open(key).poll(&mut cx).map(|r| r.err()),
            Poll::Ready(Some(Error::Unavailable))
        );
        assert_eq!(clients[0].reopen(), Err(Error::Unavailable));
    }
}
