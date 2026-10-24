//! Per-link (QP) owner state: credits, tags, grants, and the fence.
//!
//! See `designs/racer-verbs.md`, "Links" and "Memory safety rules". A
//! [`Link`] is one RC QP used in both directions. It lives on the owner
//! thread and is driven by [`Link::poll`]. Any anomaly returns a [`Break`];
//! the owner then calls [`Link::destroy`] (QP to ERR and destroy) until it
//! succeeds, and only then are the link's buffers free again.
//!
//! Work request ids: bits 63..56 kind, 31..0 receive slot, send slot, or tag.

use std::{
    collections::{BTreeMap, VecDeque},
    rc::Rc,
    sync::{Arc, atomic::AtomicBool},
    time::{Duration, Instant},
};

use crate::{
    Endpoint, Error, GuardOwner, Result,
    channel::{Completer, Op, op},
    control::{CONTROL_LEN, ControlKind, ControlMessage},
    ffi::{
        Access, MemoryWindow, NativeDevice, NativeQueuePair, NativeRegion, WorkCompletion, opcode,
    },
    pool::{Buffer, BufferState, DevicePage, Pool, RegisteredPage},
};

/// A link not `Ready` this long after creation breaks.
pub const SETUP_TIMEOUT: Duration = Duration::from_secs(5);

/// After sending `Close`, break at the latest this long later.
pub(crate) const CLOSE_GRACE: Duration = Duration::from_secs(1);

const SEND_CTL: u8 = 1;
const RECV_CTL: u8 = 2;
const WRITE: u8 = 3;
const BIND: u8 = 4;
const INVALIDATE: u8 = 5;

/// Work request id for `kind` and `index`.
fn wr(kind: u8, index: u32) -> u64 {
    (u64::from(kind) << 56) | u64::from(index)
}

/// One QP incarnation. Never reused within a process.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub struct Epoch(pub u64);

/// A grant the peer may write into.
#[derive(Debug)]
pub struct Granted {
    /// Per-link tag. The peer's write carries it as the immediate.
    pub tag: u32,

    /// Resolves once the peer wrote or released, after the fence.
    pub arrival: Op<Arrival>,
}

/// How a grant ended.
#[derive(Debug)]
pub enum Arrival {
    /// Write with immediate observed and the window invalidated. Exclusive
    /// CPU access.
    Page(RegisteredPage),

    /// Like [`Self::Page`], for a grant made with [`crate::Client::grant_device`]:
    /// the bytes are in device memory and the CPU never touched them.
    Device(DevicePage),

    /// The peer sent `Release`. The buffer is already back in the pool.
    Released,
}

/// Caller memory that a NIC may read in place for a [`Source`] write.
///
/// # Safety
///
/// While the value is only shared (accessed through `&`), every call to
/// [`bytes`](DirectBytes::bytes) must return the same initialized slice, the
/// same pointer and length, and those bytes must stay allocated and unchanged
/// until the value is dropped or mutably borrowed. The owner reads the slice
/// once when it accepts a write and lets the NIC read that address range
/// after the call returns, holding the `Arc` until the write completes or the
/// queue pair is destroyed.
///
/// Arbitrary [`AsRef`] types cannot be written directly, because nothing
/// stops a safe `AsRef` implementation from returning a different slice on
/// each call:
///
/// ```compile_fail
/// struct Shifting;
///
/// impl AsRef<[u8]> for Shifting {
///     fn as_ref(&self) -> &[u8] {
///         b"bytes"
///     }
/// }
///
/// let _ = rdma_verbs::Source::new(std::sync::Arc::new(Shifting));
/// ```
pub unsafe trait DirectBytes: Send + Sync + 'static {
    /// The bytes to write.
    fn bytes(&self) -> &[u8];
}

// SAFETY: a shared `Vec` cannot reallocate or change its contents.
unsafe impl DirectBytes for Vec<u8> {
    fn bytes(&self) -> &[u8] {
        self
    }
}

// SAFETY: a shared boxed slice cannot move or change its contents.
unsafe impl DirectBytes for Box<[u8]> {
    fn bytes(&self) -> &[u8] {
        self
    }
}

// SAFETY: a `'static` shared slice lives forever and never changes.
unsafe impl DirectBytes for &'static [u8] {
    fn bytes(&self) -> &[u8] {
        self
    }
}

/// Bytes to write into a peer's grant.
///
/// The NIC reads the caller's memory in place through the device's implicit
/// on-demand paging region. The owner holds the `Arc` until the write
/// completes or the QP is destroyed.
#[derive(Clone)]
pub struct Source(Arc<dyn DirectBytes>);

impl Source {
    /// Write `bytes` in place.
    pub fn new(bytes: Arc<dyn DirectBytes>) -> Self {
        Self(bytes)
    }

    /// Length in bytes.
    pub fn len(&self) -> usize {
        self.0.bytes().len()
    }

    /// True if empty. Empty sources cannot be written.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

impl std::fmt::Debug for Source {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Source({})", self.len())
    }
}

/// `(send_depth, recv_depth)` for `grants` grants per link.
pub(crate) fn depths(grants: u32) -> Option<(u32, u32)> {
    let recv = grants.checked_mul(2)?.checked_add(4)?;
    let send = recv.checked_add(grants.checked_mul(2)?.checked_add(4)?)?;

    (grants > 0).then_some((send, recv))
}

/// Who started the link.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Role {
    /// Ready once connected and its hello is queued.
    Initiator,

    /// Ready on its first receive completion.
    Responder,
}

/// Link lifecycle as seen by the owner.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Phase {
    /// QP created and receives posted, not connected.
    Created,

    /// Connected, waiting for the initiator's first message.
    Connecting,

    /// Data may flow.
    Ready,

    /// `Close` queued; break once sent or at `until`.
    Closing { until: Instant },
}

/// Why a link must be broken.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Break {
    /// Error completion, protocol violation, or exposed grant deadline.
    Error,

    /// The peer sent `Close`.
    PeerClosed,

    /// Our own `Close` finished (idle close or shutdown).
    Closed,

    /// Not ready within [`SETUP_TIMEOUT`].
    Timeout,
}

/// Fixed per-link parameters.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Limits {
    /// Max concurrent grants either side may hold against the other.
    pub(crate) grants: u32,

    /// Page buffer length; bounds grant and write lengths.
    pub(crate) buffer_len: u32,

    /// Ready with no traffic this long sends `Close`.
    pub(crate) idle: Duration,
}

enum GrantState {
    /// `BIND_MW` posted.
    Binding,

    /// Bound, `Grant` waiting in the outbox.
    Bound,

    /// `Grant` posted: the peer may write.
    Exposed,

    /// Fenced `LOCAL_INV` queued or posted.
    Invalidating(Outcome),
}

enum Outcome {
    Written(u32),

    Released,

    /// Never exposed to the peer; the grant op fails with this.
    Cancelled(Error),
}

/// A grant we issued (receiver side).
struct GrantEntry {
    transfer: [u8; 16],

    length: u32,

    buffer: Option<Buffer>,

    window: usize,

    key: u32,

    state: GrantState,

    deadline: Instant,

    granted: Option<Completer<Granted>>,

    arrival: Option<Completer<Arrival>>,
}

/// A grant the peer issued (sender side).
#[derive(Clone, Copy, Debug)]
struct RemoteGrant {
    rkey: u32,

    addr: u64,

    length: u32,

    transfer: [u8; 16],
}

/// A write or release waiting for its `Grant`.
enum Waiting {
    Write {
        source: Source,

        deadline: Instant,

        done: Completer<()>,
    },

    Release {
        done: Completer<()>,
    },
}

/// Work waiting for credits, send queue space, or a control slot.
enum Outgoing {
    Grant(u32),

    Release {
        tag: u32,

        transfer: [u8; 16],

        done: Completer<()>,
    },

    Credit,

    Close,

    Write {
        tag: u32,

        source: Payload,

        len: u32,

        remote: RemoteGrant,

        deadline: Instant,

        done: Completer<()>,
    },

    Bind(u32),

    Invalidate(u32),

    /// Tests only: an arbitrary control message.
    #[cfg(test)]
    Raw(ControlMessage),
}

impl Outgoing {
    /// Credits needed to post. One is reserved for `Credit` and `Close`.
    fn credits(&self) -> u32 {
        match self {
            Self::Grant(_) | Self::Release { .. } | Self::Write { .. } => 2,
            Self::Credit | Self::Close => 1,
            #[cfg(test)]
            Self::Raw(_) => 1,
            Self::Bind(_) | Self::Invalidate(_) => 0,
        }
    }

    fn control(&self) -> bool {
        !matches!(
            self,
            Self::Write { .. } | Self::Bind(_) | Self::Invalidate(_)
        )
    }
}

/// Caller memory a queued or posted write reads through the device's
/// implicit region. Held until the write completes or the QP is destroyed.
/// `addr` is the start of the slice captured when the write was accepted;
/// the posted length comes from the same slice.
struct Payload {
    bytes: Arc<dyn DirectBytes>,

    addr: u64,
}

/// Posted send-queue work.
enum InFlight {
    Control {
        slot: u32,

        done: Option<Completer<()>>,

        close: bool,
    },

    Write {
        source: Payload,

        done: Completer<()>,
    },

    Bind(u32),

    Invalidate(u32),
}

/// One RC QP and its owner-side state.
pub(crate) struct Link {
    // Declared first: dropped (stopped and freed) before the windows and
    // regions it may use.
    qp: Rc<NativeQueuePair>,

    windows: Vec<Rc<MemoryWindow>>,

    /// The device, for windows over a device pool, made on first use.
    device: Rc<NativeDevice>,

    control: Rc<NativeRegion>,

    epoch: Epoch,

    role: Role,

    limits: Limits,

    phase: Phase,

    link_id: u64,

    send_depth: u32,

    recv_depth: u32,

    /// Peer receives we may still consume.
    credits: u32,

    /// Peer receives we consumed and the peer has not returned yet.
    unreturned: u32,

    /// Receives we reposted but have not reported.
    owed: u32,

    sq_used: u32,

    next_tag: u32,

    free_windows: Vec<usize>,

    /// Free windows tied to the device pool. A window stays tied to the
    /// first region it binds, so device grants never reuse host windows.
    free_device_windows: Vec<usize>,

    free_slots: Vec<u32>,

    grants: BTreeMap<u32, GrantEntry>,

    /// Grants we may hold: `min(local, peer)` once connected.
    grant_cap: u32,

    remote: BTreeMap<u32, RemoteGrant>,

    waiting: BTreeMap<u32, Waiting>,

    outbox: VecDeque<Outgoing>,

    inflight: BTreeMap<u64, InFlight>,

    /// Buffers held until the QP is destroyed.
    quarantine: Vec<Buffer>,

    /// Sources of failed posted writes, held until the QP is
    /// destroyed: the NIC may still read them.
    held: Vec<Arc<dyn DirectBytes>>,

    created: Instant,

    ready_since: Option<Instant>,

    last_used: Instant,

    close_sent: bool,

    /// Ops were failed by `destroy`.
    failed: bool,
}

impl Link {
    /// Create the QP, its control region and windows, and post every
    /// receive. The link is [`Phase::Created`].
    pub(crate) fn new(
        epoch: Epoch,
        role: Role,
        device: &Rc<NativeDevice>,
        quota: &Arc<GuardOwner>,
        limits: Limits,
        now: Instant,
    ) -> Result<Self> {
        let (send_depth, recv_depth) = depths(limits.grants).ok_or(Error::InvalidConfiguration)?;
        let send_slots = recv_depth;
        let qp = NativeQueuePair::new_with_depth(
            device.clone(),
            quota.clone(),
            Arc::new(AtomicBool::new(false)),
            send_depth,
            recv_depth,
        )?;
        let control = NativeRegion::with_access(
            device.clone(),
            CONTROL_LEN * (recv_depth + send_slots) as usize,
            Access::CONTROL,
            quota.clone(),
        )?;
        let windows = (0..limits.grants)
            .map(|_| MemoryWindow::new(device))
            .collect::<Result<Vec<_>>>()?;

        for slot in 0..recv_depth {
            qp.post_recv(
                wr(RECV_CTL, slot),
                &control,
                u64::from(slot) * CONTROL_LEN as u64,
                CONTROL_LEN as u32,
            )?;
        }

        Ok(Self {
            qp,
            free_windows: (0..windows.len()).rev().collect(),
            windows,
            device: device.clone(),
            free_device_windows: Vec::new(),
            control,
            epoch,
            role,
            limits,
            phase: Phase::Created,
            link_id: 0,
            send_depth,
            recv_depth,
            credits: 0,
            unreturned: 0,
            owed: 0,
            sq_used: 0,
            next_tag: 0,
            free_slots: (recv_depth..recv_depth + send_slots).rev().collect(),
            grants: BTreeMap::new(),
            grant_cap: limits.grants,
            remote: BTreeMap::new(),
            waiting: BTreeMap::new(),
            outbox: VecDeque::new(),
            inflight: BTreeMap::new(),
            quarantine: Vec::new(),
            held: Vec::new(),
            created: now,
            ready_since: None,
            last_used: now,
            close_sent: false,
            failed: false,
        })
    }

    /// This QP's address for the peer.
    pub(crate) fn endpoint(&self) -> Endpoint {
        self.qp.endpoint
    }

    /// Receives we posted for the peer.
    pub(crate) fn recv_credits(&self) -> u32 {
        self.recv_depth
    }

    /// Our local grant limit, advertised to the peer.
    pub(crate) fn grant_limit(&self) -> u32 {
        self.limits.grants
    }

    pub(crate) fn epoch(&self) -> Epoch {
        self.epoch
    }

    pub(crate) fn role(&self) -> Role {
        self.role
    }

    pub(crate) fn phase(&self) -> Phase {
        self.phase
    }

    pub(crate) fn link_id(&self) -> u64 {
        self.link_id
    }

    /// When the link became `Ready`, if it did.
    pub(crate) fn ready_since(&self) -> Option<Instant> {
        self.ready_since
    }

    /// The first completion that broke the QP, if any.
    #[cfg(test)]
    pub(crate) fn failure(&self) -> Option<crate::ffi::Failure> {
        self.qp.failure()
    }

    /// Connect to the peer's QP. `credits` is the peer's advertised receive
    /// count and must be at least 2. `peer_grants` is the peer's grant
    /// limit; we hold at most `min(local, peer_grants)` grants so the peer
    /// never sees more than it accepts. The initiator queues its hello and is
    /// `Ready`; the responder waits for it.
    pub(crate) fn connect(
        &mut self,
        remote: Endpoint,
        credits: u32,
        peer_grants: u32,
        link_id: u64,
        now: Instant,
    ) -> Result<()> {
        if self.phase != Phase::Created || credits < 2 || peer_grants == 0 {
            return Err(Error::InvalidRequest);
        }

        self.qp.connect(remote)?;
        self.credits = credits;
        self.grant_cap = self.limits.grants.min(peer_grants);
        self.link_id = link_id;

        match self.role {
            Role::Initiator => {
                self.outbox.push_back(Outgoing::Credit);
                self.ready(now);
            }
            Role::Responder => self.phase = Phase::Connecting,
        }

        Ok(())
    }

    fn ready(&mut self, now: Instant) {
        self.phase = Phase::Ready;
        self.ready_since = Some(now);
        self.last_used = now;
    }

    /// Bind a window over a free buffer and queue a `Grant`. `done` gets the
    /// tag and arrival op once the `Grant` is posted. With `device`, the
    /// buffer comes from the device pool and the arrival is
    /// [`Arrival::Device`]: fails with [`Error::Unavailable`] if the rail has
    /// no device pool and [`Error::Overloaded`] if none is free.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn grant(
        &mut self,
        pool: &Pool,
        device: bool,
        transfer: [u8; 16],
        length: u32,
        deadline: Instant,
        done: Completer<Granted>,
        now: Instant,
    ) {
        if self.phase != Phase::Ready {
            let _ = done.complete(Err(Error::Unavailable));
            return;
        }

        if length == 0 || length > self.limits.buffer_len {
            let _ = done.complete(Err(Error::InvalidRange));
            return;
        }

        if self.grants.len() >= self.grant_cap as usize {
            let _ = done.complete(Err(Error::Overloaded));
            return;
        }

        if device && pool.device_shared().is_none() {
            let _ = done.complete(Err(Error::Unavailable));
            return;
        }

        let window = if device {
            match self.free_device_windows.pop() {
                Some(window) => window,
                None => match MemoryWindow::new(&self.device) {
                    // At most one device window per concurrent grant, so
                    // device windows never exceed the grant cap.
                    Ok(window) => {
                        self.windows.push(window);
                        self.windows.len() - 1
                    }
                    Err(error) => {
                        let _ = done.complete(Err(error));
                        return;
                    }
                },
            }
        } else {
            self.free_windows.pop().expect("a window per grant")
        };

        let taken = if device {
            pool.take_device(BufferState::Binding)
        } else {
            pool.take(BufferState::Binding)
        };
        let Some(buffer) = taken else {
            self.free_window(device, window);
            let _ = done.complete(Err(Error::Overloaded));
            return;
        };

        let tag = self.allocate_tag();
        self.last_used = now;
        self.grants.insert(
            tag,
            GrantEntry {
                transfer,
                length,
                buffer: Some(buffer),
                window,
                key: 0,
                state: GrantState::Binding,
                deadline,
                granted: Some(done),
                arrival: None,
            },
        );
        self.outbox.push_back(Outgoing::Bind(tag));
    }

    /// Return `window` to its free list.
    fn free_window(&mut self, device: bool, window: usize) {
        if device {
            self.free_device_windows.push(window);
        } else {
            self.free_windows.push(window);
        }
    }

    /// Next nonzero tag not in use.
    fn allocate_tag(&mut self) -> u32 {
        loop {
            self.next_tag = self.next_tag.wrapping_add(1);

            if self.next_tag != 0 && !self.grants.contains_key(&self.next_tag) {
                return self.next_tag;
            }
        }
    }

    /// Write `source` into the peer's grant `tag`, waiting for the `Grant`
    /// until `deadline`. `done` resolves on the send completion.
    pub(crate) fn write(
        &mut self,
        tag: u32,
        source: Source,
        deadline: Instant,
        done: Completer<()>,
        now: Instant,
    ) {
        if self.phase != Phase::Ready {
            let _ = done.complete(Err(Error::Unavailable));
            return;
        }

        if source.is_empty() || source.len() > self.limits.buffer_len as usize {
            let _ = done.complete(Err(Error::InvalidRange));
            return;
        }

        self.last_used = now;

        if self.remote.contains_key(&tag) {
            self.start_write(tag, source, deadline, done);
        } else if self.waiting.contains_key(&tag) {
            let _ = done.complete(Err(Error::InvalidRequest));
        } else if self.waiting.len() >= 2 * self.limits.grants as usize {
            let _ = done.complete(Err(Error::Overloaded));
        } else {
            self.waiting.insert(
                tag,
                Waiting::Write {
                    source,
                    deadline,
                    done,
                },
            );
        }
    }

    /// Queue the write of `source` for the peer's grant `tag`.
    fn start_write(&mut self, tag: u32, source: Source, deadline: Instant, done: Completer<()>) {
        let remote = self.remote[&tag];
        // Capture the caller's slice once: the NIC reads exactly this range,
        // so the length and the address must come from the same borrow.
        let slice = source.0.bytes();
        let (len, addr) = (slice.len(), slice.as_ptr() as u64);

        if len > remote.length as usize {
            let _ = done.complete(Err(Error::InvalidRange));
            return;
        }

        self.remote.remove(&tag);
        self.outbox.push_back(Outgoing::Write {
            tag,
            source: Payload {
                bytes: source.0,
                addr,
            },
            len: len as u32,
            remote,
            deadline,
            done,
        });
    }

    /// Decline the peer's grant `tag`, now or when its `Grant` arrives.
    /// `done` resolves on the send completion.
    pub(crate) fn release(&mut self, tag: u32, done: Completer<()>, now: Instant) {
        if !matches!(self.phase, Phase::Ready | Phase::Closing { .. }) {
            let _ = done.complete(Err(Error::Unavailable));
            return;
        }

        self.last_used = now;

        if let Some(remote) = self.remote.remove(&tag) {
            self.outbox.push_back(Outgoing::Release {
                tag,
                transfer: remote.transfer,
                done,
            });
            return;
        }

        match self.waiting.remove(&tag) {
            Some(Waiting::Write { done: write, .. }) => {
                let _ = write.complete(Err(Error::Cancelled));
                self.waiting.insert(tag, Waiting::Release { done });
            }
            Some(waiting @ Waiting::Release { .. }) => {
                self.waiting.insert(tag, waiting);
                let _ = done.complete(Err(Error::InvalidRequest));
            }
            None if self.waiting.len() >= 2 * self.limits.grants as usize => {
                let _ = done.complete(Err(Error::Overloaded));
            }
            None => {
                self.waiting.insert(tag, Waiting::Release { done });
            }
        }
    }

    /// Queue `Close` and break once it is sent or after `grace`.
    pub(crate) fn close(&mut self, now: Instant, grace: Duration) {
        if self.phase == Phase::Ready {
            self.outbox.push_back(Outgoing::Close);
            self.phase = Phase::Closing { until: now + grace };
        }
    }

    /// Reap completions, run timers, and post queued work.
    pub(crate) fn poll(&mut self, pool: &Pool, now: Instant) -> std::result::Result<(), Break> {
        if matches!(self.phase, Phase::Created | Phase::Connecting)
            && now >= self.created + SETUP_TIMEOUT
        {
            return Err(Break::Timeout);
        }

        let mut completions = Vec::new();

        let progressed = if self.phase == Phase::Created {
            Ok(0)
        } else {
            self.qp.progress_with(|c| completions.push(c))
        };

        // Completions before a failed one are still valid. A `Close` among
        // them means the peer closed first; anything else is an error.
        for completion in completions {
            match self.complete(pool, completion, now) {
                Ok(()) => {}
                Err(Break::PeerClosed) => return Err(Break::PeerClosed),
                Err(error) if progressed.is_ok() => return Err(error),
                Err(_) => return Err(Break::Error),
            }
        }

        progressed.map_err(|_| Break::Error)?;

        self.expire(now)?;

        if self.phase == Phase::Ready && now >= self.last_used + self.limits.idle {
            self.close(now, CLOSE_GRACE);
        }

        // A queued control carries `owed` anyway, unless it is starved of
        // credits; then a standalone `Credit` must go out on the reserve.
        if self.owed >= self.recv_depth / 2
            && !self
                .outbox
                .iter()
                .any(|o| matches!(o, Outgoing::Credit | Outgoing::Close))
            && (self.credits < 2 || !self.outbox.iter().any(Outgoing::control))
        {
            self.outbox.push_back(Outgoing::Credit);
        }

        self.flush(pool)?;

        if let Phase::Closing { until } = self.phase
            && (self.close_sent || now >= until)
        {
            return Err(Break::Closed);
        }

        Ok(())
    }

    /// Handle one successful completion.
    fn complete(
        &mut self,
        pool: &Pool,
        c: WorkCompletion,
        now: Instant,
    ) -> std::result::Result<(), Break> {
        if (c.id >> 56) as u8 == RECV_CTL {
            return self.received(pool, c.id as u32, c, now);
        }

        let inflight = self.inflight.remove(&c.id).ok_or(Break::Error)?;
        self.sq_used -= 1;

        match inflight {
            InFlight::Control { slot, done, close } => {
                self.free_slots.push(slot);
                self.close_sent |= close;

                if let Some(done) = done {
                    let _ = done.complete(Ok(()));
                }
            }
            InFlight::Write { source: _, done } => {
                // Completed: the NIC no longer reads the source.
                let _ = done.complete(Ok(()));
            }
            InFlight::Bind(tag) => {
                let entry = self.grants.get_mut(&tag).ok_or(Break::Error)?;
                let abandoned = entry.granted.as_ref().is_none_or(Completer::is_abandoned);

                if abandoned || now >= entry.deadline {
                    let error = if abandoned {
                        Error::Cancelled
                    } else {
                        Error::DeadlineExceeded
                    };
                    entry.state = GrantState::Invalidating(Outcome::Cancelled(error));
                    self.outbox.push_back(Outgoing::Invalidate(tag));
                } else {
                    entry.state = GrantState::Bound;
                    self.outbox.push_back(Outgoing::Grant(tag));
                }
            }
            InFlight::Invalidate(tag) => {
                let mut entry = self.grants.remove(&tag).ok_or(Break::Error)?;
                let buffer = entry.buffer.take().ok_or(Break::Error)?;
                self.free_window(buffer.is_device(), entry.window);
                let GrantState::Invalidating(outcome) = entry.state else {
                    return Err(Break::Error);
                };

                match outcome {
                    Outcome::Written(len) => {
                        let page = if buffer.is_device() {
                            pool.own_device(buffer, len as usize).map(Arrival::Device)
                        } else {
                            pool.own(buffer, len as usize).map(Arrival::Page)
                        }
                        .map_err(|buffer| {
                            self.quarantine.push(buffer);
                            Break::Error
                        })?;

                        if let Some(arrival) = entry.arrival.take() {
                            // A dropped op hands the page back; dropping it frees the buffer.
                            let _ = arrival.complete(Ok(page));
                        }
                    }
                    Outcome::Released => {
                        pool.release(buffer);

                        if let Some(arrival) = entry.arrival.take() {
                            let _ = arrival.complete(Ok(Arrival::Released));
                        }
                    }
                    Outcome::Cancelled(error) => {
                        pool.release(buffer);

                        if let Some(granted) = entry.granted.take() {
                            let _ = granted.complete(Err(error));
                        }
                    }
                }
            }
        }

        Ok(())
    }

    /// Handle a receive completion in control slot `slot`.
    fn received(
        &mut self,
        pool: &Pool,
        slot: u32,
        c: WorkCompletion,
        now: Instant,
    ) -> std::result::Result<(), Break> {
        if slot >= self.recv_depth || c.id != wr(RECV_CTL, slot) {
            return Err(Break::Error);
        }

        if self.phase == Phase::Connecting {
            self.ready(now);
        }

        let message = match c.opcode {
            opcode::RECV => {
                let len = (c.byte_len as usize).min(CONTROL_LEN);
                let mut bytes = [0u8; CONTROL_LEN];
                // SAFETY: the receive completed, so the NIC is done with this
                // slot until it is reposted below.
                unsafe {
                    std::ptr::copy_nonoverlapping(
                        self.control.as_ptr().add(slot as usize * CONTROL_LEN),
                        bytes.as_mut_ptr(),
                        len,
                    );
                }

                let bytes = if c.byte_len as usize == len {
                    &bytes[..len]
                } else {
                    // Longer than a slot cannot happen; reject it as a bad length.
                    &[][..]
                };

                let message = ControlMessage::decode(bytes, self.link_id, self.limits.buffer_len)
                    .map_err(|_| Break::Error)?;

                if message.kind == ControlKind::Close {
                    return Err(Break::PeerClosed);
                }

                Some(message)
            }
            opcode::RECV_RDMA_WITH_IMM => None,
            _ => return Err(Break::Error),
        };

        self.qp
            .post_recv(
                c.id,
                &self.control,
                u64::from(slot) * CONTROL_LEN as u64,
                CONTROL_LEN as u32,
            )
            .map_err(|_| Break::Error)?;
        self.owed += 1;

        let Some(message) = message else {
            let tag = c.imm.ok_or(Break::Error)?;
            let entry = self.grants.get_mut(&tag).ok_or(Break::Error)?;

            if !matches!(entry.state, GrantState::Exposed)
                || c.byte_len == 0
                || c.byte_len > entry.length
            {
                return Err(Break::Error);
            }

            entry.state = GrantState::Invalidating(Outcome::Written(c.byte_len));
            pool.set(
                entry.buffer.as_ref().ok_or(Break::Error)?,
                BufferState::Invalidating,
            );
            self.outbox.push_back(Outgoing::Invalidate(tag));
            self.last_used = now;
            return Ok(());
        };

        self.unreturned = self
            .unreturned
            .checked_sub(message.credits)
            .ok_or(Break::Error)?;
        self.credits += message.credits;

        match message.kind {
            ControlKind::Grant {
                tag,
                rkey,
                addr,
                length,
                transfer_id,
            } => {
                if self.remote.contains_key(&tag)
                    || self.remote.len() >= self.limits.grants as usize
                {
                    return Err(Break::Error);
                }

                self.last_used = now;
                self.remote.insert(
                    tag,
                    RemoteGrant {
                        rkey,
                        addr,
                        length,
                        transfer: transfer_id,
                    },
                );

                match self.waiting.remove(&tag) {
                    Some(Waiting::Write {
                        source,
                        deadline,
                        done,
                    }) => self.start_write(tag, source, deadline, done),
                    Some(Waiting::Release { done }) => {
                        self.remote.remove(&tag);
                        self.outbox.push_back(Outgoing::Release {
                            tag,
                            transfer: transfer_id,
                            done,
                        });
                    }
                    None => {}
                }
            }
            ControlKind::Release { tag, transfer_id } => {
                let entry = self.grants.get_mut(&tag).ok_or(Break::Error)?;

                if !matches!(entry.state, GrantState::Exposed) || entry.transfer != transfer_id {
                    return Err(Break::Error);
                }

                entry.state = GrantState::Invalidating(Outcome::Released);
                pool.set(
                    entry.buffer.as_ref().ok_or(Break::Error)?,
                    BufferState::Invalidating,
                );
                self.outbox.push_back(Outgoing::Invalidate(tag));
                self.last_used = now;
            }
            ControlKind::Credit | ControlKind::Close => {}
        }

        Ok(())
    }

    /// Fail or break on passed deadlines.
    fn expire(&mut self, now: Instant) -> std::result::Result<(), Break> {
        let mut cancelled = Vec::new();

        for (&tag, entry) in &mut self.grants {
            if now < entry.deadline {
                continue;
            }

            match entry.state {
                // An exposed window can only be fenced by the peer or a break.
                GrantState::Exposed => return Err(Break::Error),
                GrantState::Bound => {
                    entry.state =
                        GrantState::Invalidating(Outcome::Cancelled(Error::DeadlineExceeded));
                    cancelled.push(tag);
                }
                GrantState::Binding | GrantState::Invalidating(_) => {}
            }
        }

        for tag in cancelled {
            self.outbox
                .retain(|o| !matches!(o, Outgoing::Grant(t) if *t == tag));
            self.outbox.push_back(Outgoing::Invalidate(tag));
        }

        let expired: Vec<u32> = self
            .waiting
            .iter()
            .filter(|(_, w)| matches!(w, Waiting::Write { deadline, .. } if now >= *deadline))
            .map(|(tag, _)| *tag)
            .collect();

        for tag in expired {
            if let Some(Waiting::Write { done, .. }) = self.waiting.remove(&tag) {
                let _ = done.complete(Err(Error::DeadlineExceeded));
            }
        }

        let mut kept = VecDeque::with_capacity(self.outbox.len());

        for outgoing in self.outbox.drain(..) {
            match outgoing {
                Outgoing::Write {
                    tag,
                    remote,
                    deadline,
                    done,
                    ..
                } if now >= deadline => {
                    self.remote.insert(tag, remote);
                    let _ = done.complete(Err(Error::DeadlineExceeded));
                }
                other => kept.push_back(other),
            }
        }

        self.outbox = kept;
        Ok(())
    }

    /// Post queued work that has credits, send queue space, and a slot.
    /// Credit-consuming work keeps its order; binds and invalidates pass.
    fn flush(&mut self, pool: &Pool) -> std::result::Result<(), Break> {
        let mut blocked = false;
        let mut i = 0;

        while i < self.outbox.len() && self.sq_used < self.send_depth {
            let item = &self.outbox[i];
            let need = item.credits();

            let starved = self.credits < need || (item.control() && self.free_slots.is_empty());

            // Data keeps its order; `Credit` and `Close` use the reserved credit
            // and may pass blocked data, or both sides could wait forever.
            if need == 2 && (blocked || starved) {
                blocked = true;
                i += 1;
                continue;
            }

            if need == 1 && starved {
                i += 1;
                continue;
            }

            let item = self.outbox.remove(i).unwrap();
            self.post(pool, item).map_err(|_| Break::Error)?;
        }

        Ok(())
    }

    /// Post one item. Callers checked credits, queue space, and slots.
    fn post(&mut self, pool: &Pool, item: Outgoing) -> Result<()> {
        match item {
            Outgoing::Bind(tag) => {
                let entry = self.grants.get_mut(&tag).ok_or(Error::Io)?;
                let buffer = entry.buffer.as_ref().ok_or(Error::Io)?;
                let key = self.qp.bind_window(
                    wr(BIND, tag),
                    &self.windows[entry.window],
                    pool.region_of(buffer),
                    pool.offset(buffer),
                    entry.length,
                )?;
                entry.key = key;
                self.track(wr(BIND, tag), InFlight::Bind(tag));
            }
            Outgoing::Invalidate(tag) => {
                let entry = self.grants.get(&tag).ok_or(Error::Io)?;
                self.qp.invalidate_window(
                    wr(INVALIDATE, tag),
                    &self.windows[entry.window],
                    entry.key,
                )?;
                self.track(wr(INVALIDATE, tag), InFlight::Invalidate(tag));
            }
            Outgoing::Write {
                tag,
                source,
                len,
                remote,
                done,
                ..
            } => {
                // The implicit region's offset is the virtual address.
                let posted = self.qp.write_imm(
                    wr(WRITE, tag),
                    pool.implicit(),
                    source.addr,
                    len,
                    remote.addr,
                    remote.rkey,
                    tag,
                );

                if let Err(error) = posted {
                    // Nothing reached the NIC: dropping the source is safe.
                    let _ = done.complete(Err(error));
                    return Err(error);
                }

                self.consume();
                self.track(wr(WRITE, tag), InFlight::Write { source, done });
            }
            Outgoing::Grant(tag) => {
                let entry = self.grants.get_mut(&tag).ok_or(Error::Io)?;

                if entry.granted.as_ref().is_none_or(Completer::is_abandoned) {
                    // Never exposed: fence the bound window and free it.
                    entry.state = GrantState::Invalidating(Outcome::Cancelled(Error::Cancelled));
                    return self.post(pool, Outgoing::Invalidate(tag));
                }

                let buffer = entry.buffer.as_ref().ok_or(Error::Io)?;
                let message = ControlKind::Grant {
                    tag,
                    rkey: entry.key,
                    addr: pool.region_of(buffer).address() + pool.offset(buffer),
                    length: entry.length,
                    transfer_id: entry.transfer,
                };
                self.send_control(message, None, false)?;

                let entry = self.grants.get_mut(&tag).ok_or(Error::Io)?;
                entry.state = GrantState::Exposed;
                pool.set(
                    entry.buffer.as_ref().ok_or(Error::Io)?,
                    BufferState::Exposed,
                );
                let (arrival, completer) = op();
                entry.arrival = Some(completer);

                if let Some(granted) = entry.granted.take() {
                    // A dropped op drops `arrival`: the grant is abandoned and
                    // ends at the peer's write or release, or its deadline.
                    let _ = granted.complete(Ok(Granted { tag, arrival }));
                }
            }
            Outgoing::Release {
                tag,
                transfer,
                done,
            } => {
                self.send_control(
                    ControlKind::Release {
                        tag,
                        transfer_id: transfer,
                    },
                    Some(done),
                    false,
                )?;
            }
            Outgoing::Credit => self.send_control(ControlKind::Credit, None, false)?,
            Outgoing::Close => self.send_control(ControlKind::Close, None, true)?,
            #[cfg(test)]
            Outgoing::Raw(message) => self.send_raw(message, None, false)?,
        }

        Ok(())
    }

    /// Send a control message carrying our owed credits.
    fn send_control(
        &mut self,
        kind: ControlKind,
        done: Option<Completer<()>>,
        close: bool,
    ) -> Result<()> {
        let message = ControlMessage {
            credits: self.owed,
            link_id: self.link_id,
            kind,
        };
        self.send_raw(message, done, close)?;
        self.owed = 0;
        Ok(())
    }

    fn send_raw(
        &mut self,
        message: ControlMessage,
        done: Option<Completer<()>>,
        close: bool,
    ) -> Result<()> {
        let slot = self.free_slots.pop().ok_or(Error::Overloaded)?;
        let offset = slot as usize * CONTROL_LEN;
        let bytes = message.encode();
        // SAFETY: a free send slot has no posted work request.
        unsafe {
            std::ptr::copy_nonoverlapping(
                bytes.as_ptr(),
                self.control.as_ptr().add(offset),
                CONTROL_LEN,
            );
        }

        if let Err(error) = self.qp.send(
            wr(SEND_CTL, slot),
            &self.control,
            offset as u64,
            CONTROL_LEN as u32,
        ) {
            self.free_slots.push(slot);
            if let Some(done) = done {
                let _ = done.complete(Err(error));
            }
            return Err(error);
        }

        self.consume();
        self.track(wr(SEND_CTL, slot), InFlight::Control { slot, done, close });
        Ok(())
    }

    /// One peer receive used.
    fn consume(&mut self) {
        self.credits -= 1;
        self.unreturned += 1;
    }

    fn track(&mut self, id: u64, inflight: InFlight) {
        self.sq_used += 1;
        self.inflight.insert(id, inflight);
    }

    /// Fail every op with [`Error::Unavailable`], move every buffer to
    /// `Quarantined`, and destroy the QP. Returns `true` once the QP is
    /// destroyed and the buffers are `Free`; otherwise call again later.
    pub(crate) fn destroy(&mut self, pool: &Pool) -> bool {
        if !self.failed {
            self.failed = true;
            self.fail_all(pool);
        }

        if self.qp.stop().is_err() {
            return false;
        }

        for buffer in self.quarantine.drain(..) {
            pool.release(buffer);
        }

        self.held.clear();
        true
    }

    /// Owner shutdown after a failed destroy: the buffers are never reused.
    pub(crate) fn leak(&mut self, pool: &Pool) {
        if !self.failed {
            self.failed = true;
            self.fail_all(pool);
        }

        for buffer in self.quarantine.drain(..) {
            pool.leak(buffer);
        }

        // The NIC may still read these: never free them.
        std::mem::forget(std::mem::take(&mut self.held));
    }

    fn fail_all(&mut self, pool: &Pool) {
        let mut quarantine = Vec::new();

        for (_, mut entry) in std::mem::take(&mut self.grants) {
            if let Some(buffer) = entry.buffer.take() {
                quarantine.push(buffer);
            }

            if let Some(granted) = entry.granted.take() {
                let _ = granted.complete(Err(Error::Unavailable));
            }

            if let Some(arrival) = entry.arrival.take() {
                let _ = arrival.complete(Err(Error::Unavailable));
            }
        }

        for (_, waiting) in std::mem::take(&mut self.waiting) {
            let (Waiting::Write { done, .. } | Waiting::Release { done }) = waiting;
            let _ = done.complete(Err(Error::Unavailable));
        }

        for outgoing in self.outbox.drain(..) {
            match outgoing {
                Outgoing::Write { done, .. } => {
                    // Not posted: no NIC access, so the source drops now.
                    let _ = done.complete(Err(Error::Unavailable));
                }
                Outgoing::Release { done, .. } => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
                _ => {}
            }
        }

        for (_, inflight) in std::mem::take(&mut self.inflight) {
            match inflight {
                InFlight::Write { source, done } => {
                    self.held.push(source.bytes);
                    let _ = done.complete(Err(Error::Unavailable));
                }
                InFlight::Control {
                    done: Some(done), ..
                } => {
                    let _ = done.complete(Err(Error::Unavailable));
                }
                _ => {}
            }
        }

        for buffer in &quarantine {
            pool.set(buffer, BufferState::Quarantined);
        }

        self.quarantine.extend(quarantine);
        self.remote.clear();
    }
}

#[cfg(test)]
mod tests;
