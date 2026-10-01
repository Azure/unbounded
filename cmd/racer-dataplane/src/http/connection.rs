//! Exclusive HTTP connections and bounded TCP/Unix reuse. Unfinished exchanges close.
use super::io::{HttpIo, OwnedBuffer};
use crate::runtime::reactor::Descriptor;
use crate::{
    error::{Error, Operation, Result},
    memory::pipe::{MAX_PIPE_BYTES, PipeLease},
    model::ResourceClass,
    runtime::{
        admission::{Admission, ConnectionReservation, Reservation},
        deadline::RequestScope,
        reactor::{IoBuffer, Reactor},
    },
};
use std::{
    cell::RefCell,
    collections::{BTreeMap, VecDeque},
    future::poll_fn,
    net::SocketAddr,
    path::PathBuf,
    rc::{Rc, Weak},
    task::{Poll, Waker},
    time::{Duration, Instant},
};

#[cfg(test)]
mod relay_tests;

/// A transit owns both connections, pipe, fallback storage, and relay admission.
struct Transit {
    source: ConnectionLease,
    destination: ConnectionLease,
    pipe: PipeLease,
    fallback: Option<OwnedBuffer>,
    pending: std::ops::Range<usize>,
    copied: bool,
    #[cfg(test)]
    fallback_at: Option<usize>,
}
fn unsupported(error: &std::io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}
impl HttpIo {
    pub(crate) async fn relay_body(
        &self,
        source: ConnectionLease,
        destination: ConnectionLease,
        pipe: Option<PipeLease>,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        if source.rx_remaining != destination.tx_remaining || source.rx_remaining.is_none() {
            return Err(Error::InvalidRequest);
        }
        if source.rx_remaining == Some(0) {
            let mut source = source;
            let mut destination = destination;
            finish(&mut source, &mut destination)?;
            destination.relay_reservation = None;
            return Ok(destination);
        }
        #[cfg(test)]
        let copied = destination.relay_fallback;
        #[cfg(test)]
        let fallback_at = destination.relay_fallback_at;
        #[cfg(not(test))]
        let copied = false;
        let state = Rc::new(RefCell::new(Transit {
            source,
            destination,
            pipe: pipe.ok_or(Error::InvalidRequest)?,
            fallback: None,
            pending: 0..0,
            copied,
            #[cfg(test)]
            fallback_at,
        }));
        loop {
            scope.check()?;
            let wait = {
                let mut state = state.borrow_mut();
                let s = &mut *state;
                if s.destination.fd.peer_read_closed() {
                    return Err(Error::Io);
                }
                let mut wait = None;
                // Bound synchronous work per poll so a hot link cannot monopolize
                // the worker. Drain each chunk before receiving the next one.
                for _ in 0..32 {
                    let remaining = s.source.rx_remaining.ok_or(Error::InvalidRequest)? as usize;
                    if s.pending.is_empty() && s.pipe.buffered() == 0 && remaining == 0 {
                        break;
                    }
                    let writing = !s.pending.is_empty() || s.pipe.buffered() != 0;
                    let result = if !s.pending.is_empty() {
                        s.destination
                            .fd
                            .try_send(&s.fallback.as_ref().unwrap().bytes()?[s.pending.clone()])
                    } else if s.pipe.buffered() != 0 {
                        #[cfg(test)]
                        if s.fallback_at
                            .is_some_and(|threshold| remaining <= threshold)
                            && !s.copied
                        {
                            s.copied =
                                unsupported(&std::io::Error::from_raw_os_error(libc::EOPNOTSUPP));
                        }
                        if s.copied {
                            if s.fallback.is_none() {
                                s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                            }
                            let n = s
                                .pipe
                                .try_read(s.fallback.as_mut().unwrap().bytes_mut()?)
                                .map_err(|_| Error::Io)?;
                            s.pending = 0..n;
                            continue;
                        }
                        s.pipe.try_splice_connection(&s.destination)
                    } else if let Some((ahead, range)) = s.source.read_ahead.take() {
                        let count = remaining.min(range.len());
                        if s.fallback.is_none() {
                            s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                        }
                        // Read-ahead may have a large head allocation but the tail
                        // is bounded by the receive growth step. Consume in chunks.
                        let count = count.min(MAX_PIPE_BYTES);
                        s.fallback.as_mut().unwrap().bytes_mut()?[..count]
                            .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                        if range.len() > count {
                            s.source.read_ahead = Some((ahead, range.start + count..range.end));
                        }
                        s.pending = 0..count;
                        s.source.rx_remaining = Some((remaining - count) as u64);
                        continue;
                    } else if s.copied {
                        if s.fallback.is_none() {
                            s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                        }
                        s.source.fd.try_recv(
                            &mut s.fallback.as_mut().unwrap().bytes_mut()?
                                [..remaining.min(MAX_PIPE_BYTES)],
                        )
                    } else {
                        s.pipe.try_splice_from(&s.source.fd, remaining)
                    };
                    match result {
                        Ok(0) => return Err(Error::Io),
                        Ok(n) => {
                            if writing {
                                if !s.pending.is_empty() {
                                    s.pending.start += n;
                                }
                                s.destination.tx_remaining = Some(
                                    s.destination.tx_remaining.ok_or(Error::InvalidRequest)?
                                        - n as u64,
                                );
                            } else {
                                s.source.rx_remaining = Some((remaining - n) as u64);
                                if s.copied {
                                    s.pending = 0..n;
                                }
                            }
                        }
                        Err(error) if unsupported(&error) => {
                            s.copied = true;
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::Interrupted => (),
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            wait = Some((
                                if writing {
                                    s.destination.socket()
                                } else {
                                    s.source.socket()
                                },
                                if writing { libc::POLLOUT } else { libc::POLLIN },
                            ));
                            break;
                        }
                        Err(_) => return Err(Error::Io),
                    }
                }
                if s.source.rx_remaining == Some(0) && s.destination.tx_remaining == Some(0) {
                    break;
                }
                wait
            };
            if let Some((fd, interest)) = wait {
                // A bounded poll interval also notices reverse disconnect while
                // the downstream source is silent. It never extends the deadline.
                let mut tick = scope.clone();
                tick.deadline.0 = tick
                    .deadline
                    .0
                    .min(crate::runtime::environment::now() + Duration::from_millis(10));
                match self
                    .reactor()
                    .readiness_with_lease(fd, interest as u32, state.clone(), &tick)
                    .await
                {
                    Ok(_) | Err(Error::DeadlineExceeded) => (),
                    Err(error) => return Err(error),
                }
            } else {
                let mut yielded = false;
                std::future::poll_fn(|cx| {
                    if yielded {
                        Poll::Ready(())
                    } else {
                        yielded = true;
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                })
                .await;
            }
        }
        scope.check()?;
        let mut state = Rc::try_unwrap(state)
            .map_err(|_| Error::Internal)?
            .into_inner();
        // Both finish checks must pass before either connection can be pooled.
        finish(&mut state.source, &mut state.destination)?;
        state.destination.relay_reservation = None;
        Ok(state.destination)
    }
}
fn finish(source: &mut ConnectionLease, destination: &mut ConnectionLease) -> Result<()> {
    if let Err(error) = source
        .finish_exchange()
        .and_then(|()| destination.finish_exchange())
    {
        source.poison();
        destination.poison();
        return Err(error);
    }
    Ok(())
}

#[derive(Clone, Debug, Eq, Hash, PartialEq, Ord, PartialOrd)]
pub enum Endpoint {
    Unix(PathBuf),
    /// Cache incarnation is pool identity; the name-derived path is only a dial address.
    Origin {
        cache: crate::model::CacheId,
        path: PathBuf,
    },
    /// Numeric IP:port only. DNS resolution is deliberately not performed on an
    /// I/O worker. Control-plane DNS must supply an already-resolved endpoint.
    Peer(String),
}
struct Idle {
    session: Option<crate::security::connection::Session>,
    fd: Rc<Descriptor>,
    reservation: ConnectionReservation,
    since: Instant,
}
#[derive(Default)]
struct Entry {
    active: usize,
    idle: Vec<Idle>,
    generation: u64,
}
struct PoolState {
    entries: BTreeMap<Endpoint, Entry>,
    expiry_cursor: Option<Endpoint>,
    next_expiry: Instant,
    next_generation: u64,
    closed: bool,
    waiting: VecDeque<Rc<WaitingEntry>>,
    poll_cursor: usize,
    next_waiter_poll: Instant,
}
struct WaitingEntry {
    endpoint: Endpoint,
    metadata: bool,
    waker: RefCell<Option<Waker>>,
}
struct Waiting {
    state: Rc<RefCell<PoolState>>,
    entry: Rc<WaitingEntry>,
    _reservation: Reservation,
}
impl Drop for Waiting {
    fn drop(&mut self) {
        let mut state = self.state.borrow_mut();
        state
            .waiting
            .retain(|entry| !Rc::ptr_eq(entry, &self.entry));
        if state.waiting.is_empty() {
            state.waiting = VecDeque::new();
        }
        state.wake_endpoint(&self.entry.endpoint);
    }
}
impl PoolState {
    fn wake_endpoint(&self, endpoint: &Endpoint) {
        if let Some(entry) = self
            .waiting
            .iter()
            .find(|entry| &entry.endpoint == endpoint)
        {
            if let Some(waker) = entry.waker.borrow().as_ref() {
                waker.wake_by_ref();
            }
        }
    }
}
struct ReturnToPool {
    state: Weak<RefCell<PoolState>>,
    endpoint: Endpoint,
    generation: u64,
}

/// Exclusive connection ownership follows every submitted operation into the
/// reactor. Drop is a pool return only after explicit successful finish_exchange.
pub struct ConnectionLease {
    // Follows submitted I/O and opaque bodies through the actual completion fence.
    pub(crate) peer_admission: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
    pub(crate) peer_response_verified: bool,
    #[cfg(test)]
    pub(crate) relay_fallback: bool,
    #[cfg(test)]
    pub(crate) relay_fallback_at: Option<usize>,
    // While a relay sends its verified head, retain the unfinished downstream
    // connection through that send's completion and cancellation fences too.
    pub(crate) relay_peer: Option<Box<ConnectionLease>>,
    pub(crate) relay_pipe: Option<crate::memory::pipe::PipeLease>,
    pub(crate) relay_context: Option<Reservation>,
    pub(crate) relay_reservation: Option<Rc<Reservation>>,
    pub(crate) session: Option<crate::security::connection::Session>,
    // Ingress handshake admission follows socket I/O through cancellation fences.
    pub(crate) control_reservation: Option<Reservation>,
    pub(crate) fd: Rc<Descriptor>,
    reusable: bool,
    pub(crate) reservation: Option<Rc<ConnectionReservation>>,
    pool: Option<ReturnToPool>,
    pub(crate) read_ahead: Option<(OwnedBuffer, std::ops::Range<usize>)>,
    pub(crate) rx_remaining: Option<u64>,
    pub(crate) tx_remaining: Option<u64>,
    pub(crate) request_is_head: bool,
    pub(crate) close: bool,
}
impl ConnectionLease {
    /// Takes ownership of an accepted socket, configures NONBLOCK/CLOEXEC, and
    /// reserves connection admission. No pool return is associated with this FD.
    pub fn from_accepted(fd: Descriptor, admission: &Admission) -> Result<Self> {
        let reservation = admission.reserve_connection(ResourceClass::IngressConnection)?;
        Self::from_reserved(fd, reservation)
    }
    pub(crate) fn from_reserved(
        fd: Descriptor,
        reservation: ConnectionReservation,
    ) -> Result<Self> {
        set_nonblocking(&fd)?;
        Ok(Self::new(Rc::new(fd), reservation, None))
    }
    fn new(
        fd: Rc<Descriptor>,
        reservation: ConnectionReservation,
        pool: Option<ReturnToPool>,
    ) -> Self {
        Self {
            fd,
            peer_admission: None,
            peer_response_verified: false,
            #[cfg(test)]
            relay_fallback: false,
            #[cfg(test)]
            relay_fallback_at: None,
            relay_peer: None,
            relay_pipe: None,
            relay_context: None,
            relay_reservation: None,
            session: None,
            control_reservation: None,
            reusable: false,
            reservation: Some(Rc::new(reservation)),
            pool,
            read_ahead: None,
            rx_remaining: None,
            tx_remaining: None,
            request_is_head: false,
            close: false,
        }
    }
    pub(crate) fn begin_io(&mut self) {
        self.reusable = false;
    }
    pub(crate) fn install_session(
        &mut self,
        session: crate::security::connection::Session,
    ) -> Result<()> {
        if self.session.is_some() || self.close {
            return Err(Error::Unauthorized);
        }
        self.session = Some(session);
        self.reusable = false;
        Ok(())
    }
    /// Explicitly mark the complete request/response exchange reusable. This is
    /// valid for client or server use and resets framing for the next exchange.
    /// Unexpected pipelined/read-ahead data prevents pool return.
    pub fn finish_exchange(&mut self) -> Result<()> {
        self.next_round()?;
        if self.peer_response_verified {
            if let Some(permit) = &self.peer_admission {
                permit.observe(crate::peer::adaptive::Outcome::Verified);
            }
            self.peer_response_verified = false;
        }
        self.reusable = !self.close;
        Ok(())
    }
    /// Reset framing inside an unfinished handshake/native transaction. Errors
    /// after this boundary still close the socket rather than returning it idle.
    pub(crate) fn next_round(&mut self) -> Result<()> {
        if self.rx_remaining != Some(0) || self.tx_remaining != Some(0) || self.read_ahead.is_some()
        {
            return Err(Error::InvalidRequest);
        }
        self.reusable = false;
        self.rx_remaining = None;
        self.tx_remaining = None;
        self.request_is_head = false;
        Ok(())
    }
    pub fn is_reusable(&self) -> bool {
        self.reusable
    }
    pub fn remaining_body(&self) -> Option<u64> {
        self.rx_remaining
    }
    /// Obtain the FD for an owned runtime operation. Pass this entire connection
    /// as the operation's lease; retaining just this FD does not retain admission.
    pub fn socket(&self) -> Rc<Descriptor> {
        self.fd.clone()
    }
    pub fn poison(&mut self) {
        self.close = true;
        self.reusable = false;
    }
}
impl Drop for ConnectionLease {
    fn drop(&mut self) {
        let Some(target) = &self.pool else {
            return;
        };
        let Some(state) = target.state.upgrade() else {
            return;
        };
        let mut state = state.borrow_mut();
        let closed = state.closed;
        if let Some(entry) = state.entries.get_mut(&target.endpoint) {
            entry.active = entry.active.saturating_sub(1);
            if !closed
                && entry.generation == target.generation
                && self.reusable
                && Rc::strong_count(&self.fd) == 1
            {
                if let Some(reservation) =
                    self.reservation.take().and_then(|r| Rc::try_unwrap(r).ok())
                {
                    entry.idle.push(Idle {
                        session: self.session.take(),
                        fd: self.fd.clone(),
                        reservation,
                        since: crate::runtime::environment::now(),
                    });
                }
            }
            if entry.active == 0 && entry.idle.is_empty() {
                state.entries.remove(&target.endpoint);
            }
        }
        state.wake_endpoint(&target.endpoint);
    }
}

pub struct HttpPool {
    reactor: Rc<Reactor>,
    admission: Rc<Admission>,
    per_endpoint: usize,
    per_origin: usize,
    max_endpoints: usize,
    idle_timeout: Duration,
    state: Rc<RefCell<PoolState>>,
}
impl HttpPool {
    pub fn new(reactor: Rc<Reactor>, admission: Rc<Admission>, per_endpoint: usize) -> Self {
        Self::with_limits(
            reactor,
            admission,
            per_endpoint,
            256,
            Duration::from_secs(30),
        )
    }
    pub fn with_limits(
        reactor: Rc<Reactor>,
        admission: Rc<Admission>,
        per_endpoint: usize,
        max_endpoints: usize,
        idle_timeout: Duration,
    ) -> Self {
        Self {
            reactor,
            admission,
            per_endpoint,
            per_origin: per_endpoint,
            max_endpoints,
            idle_timeout,
            state: Rc::new(RefCell::new(PoolState {
                entries: BTreeMap::new(),
                expiry_cursor: None,
                next_expiry: crate::runtime::environment::now(),
                next_generation: 0,
                closed: false,
                waiting: VecDeque::new(),
                poll_cursor: 0,
                next_waiter_poll: crate::runtime::environment::now(),
            })),
        }
    }
    /// Unix adapter endpoints have a separate cap from TCP peer endpoints. Both
    /// share the same accounted connection ceiling and bounded endpoint table.
    pub fn with_origin_limit(mut self, per_origin: usize) -> Self {
        self.per_origin = per_origin;
        self
    }
    /// Capacity exhaustion fails immediately with Overloaded: there is no hidden
    /// unbounded waiter queue. Callers schedule any retry under their own budget.
    pub fn checkout<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.checkout_relay(endpoint, None, scope)
    }
    pub(crate) fn checkout_relay<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        relay: Option<Rc<Reservation>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.checkout_peer(endpoint, relay, None, None, scope)
    }
    pub(crate) fn checkout_peer<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        relay: Option<Rc<Reservation>>,
        peer: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
        failure: Option<Rc<std::cell::Cell<bool>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            scope.check()?;
            let (mut connection, address) = self.prepare_connection(endpoint)?;
            connection.relay_reservation = relay;
            connection.peer_admission = peer.clone();
            let errno = Rc::new(std::cell::Cell::new(None));
            let result = if let Some(address) = address {
                self.reactor
                    .connect_with_observation(
                        connection.socket(),
                        address,
                        connection,
                        Some(errno.clone()),
                        scope,
                    )
                    .await
            } else {
                Ok(connection)
            };
            if peer_connect_failure(errno.get()) && scope.check().is_ok() {
                if let Some(failure) = failure {
                    failure.set(true);
                }
                if let Some(peer) = peer {
                    peer.observe(crate::peer::adaptive::Outcome::PeerFailure);
                }
            }
            result
        })
    }

    /// Bounded FIFO per endpoint, used by origin requests. Waiters hold context
    /// quota but no connection or reactor operation. The worker must call
    /// poll_waiters on its bounded tick, including while draining, for deadline
    /// checks and connection quota released by other pools/accepted sockets.
    pub fn checkout_wait<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.checkout_wait_class(endpoint, scope, false)
    }

    /// Metadata does not queue behind origin page transfers. Where the endpoint
    /// cap permits it, GETs leave one slot free for HEAD. Global outbound and
    /// context quotas remain authoritative; this is not an unaccounted socket.
    pub fn checkout_metadata<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.checkout_wait_class(endpoint, scope, true)
    }

    fn checkout_wait_class<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
        metadata: bool,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            scope.check()?;
            let mut waiting = None;
            let cancellation = scope.cancellation.subscribe()?;
            let (connection, address) = poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                if self.state.borrow().closed || self.admission.is_stopped() {
                    return Poll::Ready(Err(Error::Unavailable));
                }
                let first = self
                    .state
                    .borrow()
                    .waiting
                    .iter()
                    .find(|entry| &entry.endpoint == endpoint && entry.metadata == metadata)
                    .cloned();
                let turn = first.as_ref().is_none_or(|first| {
                    waiting
                        .as_ref()
                        .is_some_and(|waiting: &Waiting| Rc::ptr_eq(first, &waiting.entry))
                });
                if turn && self.class_available(endpoint, metadata) {
                    match self.prepare_connection(endpoint) {
                        Err(Error::Overloaded) => (),
                        result => return Poll::Ready(result),
                    }
                }
                if waiting.is_none() {
                    if self.state.borrow().waiting.len()
                        >= self.admission.limits().queue_entries.get()
                    {
                        return Poll::Ready(Err(Error::Overloaded));
                    }
                    let reservation = self.admission.reserve(
                        None,
                        ResourceClass::RequestContext,
                        std::mem::size_of::<Waiting>()
                            + std::mem::size_of::<WaitingEntry>()
                            + match endpoint {
                                Endpoint::Unix(path) => path.as_os_str().len(),
                                Endpoint::Origin { cache, path } => {
                                    cache.0.len() + path.as_os_str().len()
                                }
                                Endpoint::Peer(address) => address.len(),
                            }
                            + 128,
                    )?;
                    let entry = Rc::new(WaitingEntry {
                        endpoint: endpoint.clone(),
                        metadata,
                        waker: RefCell::new(None),
                    });
                    self.state.borrow_mut().waiting.push_back(entry.clone());
                    waiting = Some(Waiting {
                        state: self.state.clone(),
                        entry,
                        _reservation: reservation,
                    });
                }
                *waiting.as_ref().unwrap().entry.waker.borrow_mut() = Some(cx.waker().clone());
                Poll::Pending
            })
            .await?;
            drop(waiting);
            self.connect(connection, address, scope).await
        })
    }

    fn class_available(&self, endpoint: &Endpoint, metadata: bool) -> bool {
        if metadata || !matches!(endpoint, Endpoint::Origin { .. }) || self.per_origin <= 1 {
            return true;
        }
        self.state
            .borrow()
            .entries
            .get(endpoint)
            .is_none_or(|entry| entry.active < self.per_origin - 1)
    }

    /// Wake a bounded round-robin batch, including child futures whose executor
    /// does not repoll them merely because the outer worker received a timer tick.
    pub fn poll_waiters(&self, budget: usize) {
        self.expire_idle_budgeted(budget);
        let mut state = self.state.borrow_mut();
        let now = crate::runtime::environment::now();
        if budget == 0 || state.waiting.is_empty() || now < state.next_waiter_poll {
            return;
        }
        // Worker wakeups may be much more frequent than its fallback timer.
        // Do not let waking a queued child create an immediate busy-wake loop.
        state.next_waiter_poll = now + Duration::from_millis(1);
        for _ in 0..budget.min(state.waiting.len()) {
            state.poll_cursor %= state.waiting.len();
            if let Some(waker) = state.waiting[state.poll_cursor].waker.borrow().as_ref() {
                waker.wake_by_ref();
            }
            state.poll_cursor += 1;
        }
    }

    fn prepare_connection(
        &self,
        endpoint: &Endpoint,
    ) -> Result<(
        ConnectionLease,
        Option<crate::runtime::reactor::SocketAddress>,
    )> {
        if self.admission.is_stopped() {
            return Err(Error::Unavailable);
        }
        let (idle, generation) = {
            let mut state = self.state.borrow_mut();
            if state.closed {
                return Err(Error::Unavailable);
            }
            if !state.entries.contains_key(endpoint) {
                if state.entries.len() >= self.max_endpoints {
                    state.entries.retain(|_, entry| entry.active != 0);
                }
                if state.entries.len() >= self.max_endpoints {
                    return Err(Error::Overloaded);
                }
                state.next_generation = state
                    .next_generation
                    .checked_add(1)
                    .ok_or(Error::Unavailable)?;
                let generation = state.next_generation;
                state.entries.insert(
                    endpoint.clone(),
                    Entry {
                        generation,
                        ..Entry::default()
                    },
                );
            }
            let entry = state.entries.get_mut(endpoint).ok_or(Error::Unavailable)?;
            let now = crate::runtime::environment::now();
            entry
                .idle
                .retain(|idle| now.saturating_duration_since(idle.since) < self.idle_timeout);
            let limit = match endpoint {
                Endpoint::Unix(_) | Endpoint::Origin { .. } => self.per_origin,
                Endpoint::Peer(_) => self.per_endpoint,
            };
            if entry.active >= limit {
                return Err(Error::Overloaded);
            }
            entry.active += 1;
            (entry.idle.pop(), entry.generation)
        };
        // The slot guard closes a connecting slot if this future is dropped,
        // including before the connection lease has been constructed.
        let target = ReturnToPool {
            state: Rc::downgrade(&self.state),
            endpoint: endpoint.clone(),
            generation,
        };
        let mut slot = ConnectingSlot(Some(target));
        if let Some(idle) = idle {
            if idle_healthy(&idle.fd) {
                let mut connection = ConnectionLease::new(idle.fd, idle.reservation, slot.0.take());
                connection.session = idle.session;
                return Ok((connection, None));
            }
        }
        let reservation = match self
            .admission
            .reserve_connection(ResourceClass::OutboundConnection)
        {
            Err(Error::Overloaded) => {
                // Idle sockets must not strand this pool's connection quota.
                for entry in self.state.borrow_mut().entries.values_mut() {
                    entry.idle.clear();
                }
                self.admission
                    .reserve_connection(ResourceClass::OutboundConnection)?
            }
            result => result?,
        };
        let (fd, address) = create_socket(endpoint)?;
        let connection = ConnectionLease::new(Rc::new(fd), reservation, slot.0.take());
        Ok((connection, Some(address)))
    }

    async fn connect(
        &self,
        connection: ConnectionLease,
        address: Option<crate::runtime::reactor::SocketAddress>,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        let Some(address) = address else {
            return Ok(connection);
        };
        // Retain socket, admission and pool slot in the reactor through the
        // original/cancellation fences, even after both future and pool drop.
        self.reactor
            .connect_with_lease(connection.socket(), address, connection, scope)
            .await
    }
    pub fn expire_idle(&self) {
        let now = crate::runtime::environment::now();
        let mut state = self.state.borrow_mut();
        state.entries.retain(|_, entry| {
            entry
                .idle
                .retain(|idle| now.duration_since(idle.since) < self.idle_timeout);
            entry.active != 0 || !entry.idle.is_empty()
        });
    }
    /// Autonomous worker tick: visit at most `budget` endpoint buckets, resuming
    /// from an ordered cursor. No checkout or waiter is required for expiration.
    pub fn expire_idle_budgeted(&self, budget: usize) {
        use std::ops::Bound::{Excluded, Unbounded};
        let now = crate::runtime::environment::now();
        let mut state = self.state.borrow_mut();
        if budget == 0 || now < state.next_expiry {
            return;
        }
        state.next_expiry = now + Duration::from_millis(100);
        for _ in 0..budget.min(state.entries.len()) {
            let next = state
                .expiry_cursor
                .as_ref()
                .and_then(|cursor| state.entries.range((Excluded(cursor), Unbounded)).next())
                .or_else(|| state.entries.iter().next())
                .map(|(key, _)| key.clone());
            let Some(key) = next else {
                break;
            };
            let entry = state.entries.get_mut(&key).expect("selected endpoint");
            entry
                .idle
                .retain(|idle| now.saturating_duration_since(idle.since) < self.idle_timeout);
            if entry.active == 0 && entry.idle.is_empty() {
                state.entries.remove(&key);
            }
            state.expiry_cursor = Some(key);
        }
    }
    /// Invalidate an endpoint generation without closing active operations. Old
    /// leases finish normally but cannot enter the new generation's idle pool.
    pub fn invalidate(&self, endpoint: &Endpoint) {
        let mut state = self.state.borrow_mut();
        let Some(generation) = state.next_generation.checked_add(1) else {
            state.closed = true;
            return;
        };
        state.next_generation = generation;
        if let Some(entry) = state.entries.get_mut(endpoint) {
            entry.idle.clear();
            entry.generation = generation;
        }
    }
    pub fn close(&self) {
        let mut state = self.state.borrow_mut();
        state.closed = true;
        for entry in state.entries.values_mut() {
            entry.idle.clear();
        }
        for entry in &state.waiting {
            if let Some(waker) = entry.waker.borrow().as_ref() {
                waker.wake_by_ref();
            }
        }
    }
}

/// Resource exhaustion/address selection failures are local, not broken peers.
fn peer_connect_failure(errno: Option<i32>) -> bool {
    matches!(
        errno,
        Some(libc::ECONNREFUSED | libc::ECONNRESET | libc::EPIPE)
    )
}

#[cfg(test)]
#[test]
fn adaptive_connect_errno_preserves_local_exhaustion_as_neutral() {
    for errno in [
        None,
        Some(libc::ENOBUFS),
        Some(libc::ENOMEM),
        Some(libc::EADDRNOTAVAIL),
        Some(libc::ECANCELED),
        Some(libc::EIO),
    ] {
        assert!(!peer_connect_failure(errno));
    }
    for errno in [libc::ECONNREFUSED, libc::ECONNRESET, libc::EPIPE] {
        assert!(peer_connect_failure(Some(errno)));
    }
}

#[cfg(test)]
#[test]
fn adaptive_checkout_attributes_actual_connect_completion_errno() {
    use crate::{
        runtime::reactor::simulation::{Fault, Simulation},
        telemetry::metrics::{Event, Gauge, Metrics},
    };
    for (errno, blame) in [
        (libc::ENOBUFS, false),
        (libc::ENOMEM, false),
        (libc::EADDRNOTAVAIL, false),
        (libc::ECONNREFUSED, true),
    ] {
        let simulation = Simulation::new();
        let _env = simulation.enter();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor.clone(), admission, 1);
        let metrics = Metrics::default();
        let peers = crate::peer::adaptive::AdaptivePeers::new(
            crate::peer::adaptive::Config {
                total: 1,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let node = crate::model::NodeId("peer".into());
        let permit = peers.acquire(&node).unwrap();
        let failure = Rc::new(std::cell::Cell::new(false));
        let scope = RequestScope::new(
            crate::model::RequestId([88; 16]),
            crate::runtime::environment::now() + Duration::from_secs(5),
        )
        .unwrap();
        let endpoint = Endpoint::Peer("127.0.0.1:9999".into());
        simulation.inject("connect", Fault::Errno(errno));
        let mut checkout = pool.checkout_peer(
            &endpoint,
            None,
            Some(permit.clone()),
            Some(failure.clone()),
            &scope,
        );
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let mut result = None;
        for _ in 0..32 {
            if let std::task::Poll::Ready(done) = checkout.as_mut().poll(&mut cx) {
                result = Some(done);
                break;
            }
            reactor.poll_budgeted(32).unwrap();
        }
        assert!(matches!(result, Some(Err(Error::Io))));
        drop(checkout);
        drop(permit);
        assert_eq!(failure.get(), blame);
        assert_eq!(peers.available(&node), !blame);
        assert_eq!(metrics.count(Event::PeerLinkFailure), u64::from(blame));
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
    }
}
impl Drop for HttpPool {
    fn drop(&mut self) {
        self.close();
    }
}

fn idle_healthy(fd: &Descriptor) -> bool {
    fd.idle_healthy()
}
struct ConnectingSlot(Option<ReturnToPool>);
impl Drop for ConnectingSlot {
    fn drop(&mut self) {
        if let Some(target) = &self.0 {
            if let Some(state) = target.state.upgrade() {
                let mut state = state.borrow_mut();
                if let Some(entry) = state.entries.get_mut(&target.endpoint) {
                    entry.active = entry.active.saturating_sub(1);
                    if entry.active == 0 && entry.idle.is_empty() {
                        state.entries.remove(&target.endpoint);
                    }
                }
            }
        }
    }
}

fn set_nonblocking(fd: &Descriptor) -> Result<()> {
    fd.set_nonblocking()
}

// Runtime's owned address type is used so connect never borrows sockaddr bytes
// from a future that may disappear while its SQE is in flight.
fn create_socket(
    endpoint: &Endpoint,
) -> Result<(Descriptor, crate::runtime::reactor::SocketAddress)> {
    use crate::runtime::reactor::SocketAddress;
    let (domain, address) = match endpoint {
        Endpoint::Peer(value) => {
            let address: SocketAddr = value.parse().map_err(|_| Error::InvalidConfiguration)?;
            (
                if address.is_ipv4() {
                    libc::AF_INET
                } else {
                    libc::AF_INET6
                },
                SocketAddress::Inet(address),
            )
        }
        Endpoint::Unix(path) | Endpoint::Origin { path, .. } => {
            (libc::AF_UNIX, SocketAddress::Unix(path.clone()))
        }
    };
    Ok((Descriptor::socket(domain)?, address))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::{Codec, Header, MessageHead, StartLine};
    use crate::{model::RequestId, test_support::WakeCounter};
    use std::{sync::Arc, task::Context};

    enum Listener {
        Tcp(std::net::TcpListener),
        Unix(std::os::unix::net::UnixListener, PathBuf),
    }
    impl Listener {
        fn peer() -> (Self, Endpoint) {
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
            listener.set_nonblocking(true).unwrap();
            (Self::Tcp(listener), endpoint)
        }
        fn origin() -> (Self, Endpoint) {
            static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
            let path = PathBuf::from(format!(
                "pool-test-{}-{}.sock",
                std::process::id(),
                NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            ));
            let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
            listener.set_nonblocking(true).unwrap();
            (Self::Unix(listener, path.clone()), Endpoint::Unix(path))
        }
        fn accept(&self) -> Option<Descriptor> {
            let result = match self {
                Self::Tcp(listener) => listener.accept().map(|(socket, _)| socket.into()),
                Self::Unix(listener, path) => {
                    assert!(path.exists());
                    listener.accept().map(|(socket, _)| socket.into())
                }
            };
            match result {
                Ok(socket) => Some(socket),
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => None,
                Err(error) => panic!("accept: {error}"),
            }
        }
    }
    impl Drop for Listener {
        fn drop(&mut self) {
            if let Self::Unix(_, path) = self {
                std::fs::remove_file(path).unwrap();
            }
        }
    }
    fn drive<T>(reactor: &Reactor, future: impl std::future::Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(Instant::now() < deadline, "bounded pool exchange");
            reactor.poll_budgeted(128).unwrap();
        }
    }

    fn scope() -> RequestScope {
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(5)).unwrap()
    }

    fn setup() -> (Rc<Admission>, Rc<Reactor>, HttpPool) {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.queue_entries = std::num::NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1).with_origin_limit(2);
        (admission, reactor, pool)
    }

    // Hold a real checked-out connection only after a complete head/body exchange.
    fn held(
        pool: &HttpPool,
        endpoint: &Endpoint,
        listener: &Listener,
    ) -> (ConnectionLease, ConnectionLease) {
        let scope = scope();
        let io = HttpIo::with_admission(
            pool.reactor.clone(),
            Codec::new(4096, 16),
            pool.admission.clone(),
        );
        let client = async {
            let connection = pool.checkout_metadata(endpoint, &scope).await?;
            let request = MessageHead {
                start: StartLine::Request {
                    method: "GET".into(),
                    target: "/pool".into(),
                },
                headers: vec![Header {
                    name: "content-length".into(),
                    value: b"0".to_vec(),
                }],
            };
            let sent = io.send_head(connection, request, &scope).await?;
            let received = io.receive_head(sent.connection, &scope).await?;
            assert!(matches!(
                received.value.start,
                StartLine::Response { status: 200 }
            ));
            let read = io
                .read_body(received.connection, io.buffer(1)?, &scope)
                .await?;
            assert_eq!(read.bytes, 1);
            assert_eq!(read.buffer.bytes()?, b"x");
            let mut connection = read.lease;
            connection.finish_exchange()?;
            Ok::<_, Error>(connection)
        };
        let server = async {
            let socket =
                std::future::poll_fn(|_| listener.accept().map_or(Poll::Pending, Poll::Ready))
                    .await;
            let connection = ConnectionLease::from_accepted(socket, &pool.admission)?;
            let received = io.receive_head(connection, &scope).await?;
            assert!(
                matches!(received.value.start, StartLine::Request { ref target, .. } if target == "/pool")
            );
            let response = MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![Header {
                    name: "content-length".into(),
                    value: b"1".to_vec(),
                }],
            };
            let sent = io.send_head(received.connection, response, &scope).await?;
            let mut buffer = io.buffer(1)?;
            buffer.bytes_mut()?.copy_from_slice(b"x");
            let sent = io.write_body(sent.connection, buffer, &scope).await?;
            let mut connection = sent.lease;
            connection.finish_exchange()?;
            Ok::<_, Error>(connection)
        };
        drive(&pool.reactor, async { futures::try_join!(client, server) }).unwrap()
    }

    #[test]
    fn idle_expiration_runs_without_checkout_or_waiters_and_is_budgeted() {
        let (admission, _, mut pool) = setup();
        pool.idle_timeout = Duration::ZERO;
        let mut peers = Vec::new();
        for _ in 1..=3 {
            let (listener, endpoint) = Listener::peer();
            let (lease, peer) = held(&pool, &endpoint, &listener);
            peers.push(peer);
            drop(lease);
        }
        for remaining in (0..3).rev() {
            pool.state.borrow_mut().next_expiry = crate::runtime::environment::now();
            pool.poll_waiters(1);
            assert_eq!(pool.state.borrow().entries.len(), remaining);
            assert_eq!(admission.used(ResourceClass::OutboundConnection), remaining);
        }
    }
    #[test]
    fn origin_wait_is_bounded_fifo_without_blocking_peers_or_other_caches() {
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = Listener::origin();
        let (first, a) = held(&pool, &endpoint, &listener);
        let (second, b) = held(&pool, &endpoint, &listener);
        let baseline = admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut older = pool.checkout_wait(&endpoint, &scope);
        let mut newer = pool.checkout_wait(&endpoint, &scope);
        assert!(older.as_mut().poll(&mut cx).is_pending());
        assert!(newer.as_mut().poll(&mut cx).is_pending());
        assert_eq!(count.count(), 0, "no self-wake spin");
        pool.poll_waiters(1);
        assert_eq!(count.count(), 1, "tick wake is budgeted");
        pool.state.borrow_mut().next_waiter_poll = Instant::now() + Duration::from_secs(1);
        pool.poll_waiters(2);
        assert_eq!(count.count(), 1, "early worker repolls do not busy-wake");
        assert!(matches!(
            pool.checkout_wait(&endpoint, &scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(pool.state.borrow().waiting.len(), 2);
        assert!(admission.used(ResourceClass::RequestContext) > 0);
        assert_eq!(reactor.in_flight(), 0);

        // A full origin queue does not gate a peer or an unrelated cache. Peer
        // saturation remains immediate so routing can try another candidate.
        for (listener, other) in [Listener::peer(), Listener::origin()] {
            let (lease, _peer) = held(&pool, &other, &listener);
            if matches!(other, Endpoint::Peer(_)) {
                assert!(matches!(
                    pool.checkout(&other, &scope).as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Overloaded))
                ));
            }
            drop(lease);
            let Poll::Ready(Ok(lease)) = pool.checkout_wait(&other, &scope).as_mut().poll(&mut cx)
            else {
                panic!("unrelated endpoint blocked")
            };
            drop(lease);
        }
        let control = admission
            .reserve(None, ResourceClass::ControlProgress, 1)
            .unwrap();
        drop(control);
        drop(first);
        assert!(count.count() > 0);
        assert!(
            newer.as_mut().poll(&mut cx).is_pending(),
            "FIFO cannot be bypassed"
        );
        let Poll::Ready(Ok(lease)) = older.as_mut().poll(&mut cx) else {
            panic!("oldest must reuse released slot")
        };
        assert!(newer.as_mut().poll(&mut cx).is_pending());
        drop(second);
        let Poll::Ready(Ok(next)) = newer.as_mut().poll(&mut cx) else {
            panic!("second waiter must progress")
        };
        assert!(pool.state.borrow().waiting.is_empty());
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        drop((lease, next, a, b));
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn waiting_cancel_deadline_close_stop_and_drop_release_only_waiter_quota() {
        for case in ["cancel", "deadline", "close", "stop", "drop"] {
            let (admission, reactor, pool) = setup();
            let (listener, endpoint) = Listener::peer();
            let (held, peer) = held(&pool, &endpoint, &listener);
            let baseline = admission.used(ResourceClass::RequestContext);
            let mut scope = scope();
            if case == "deadline" {
                scope.deadline.0 = Instant::now() + Duration::from_millis(20);
            }
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut wait = pool.checkout_wait(&endpoint, &scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            let expected = match case {
                "cancel" => {
                    scope.cancel().unwrap();
                    Error::Cancelled
                }
                "deadline" => {
                    std::thread::sleep(Duration::from_millis(30));
                    pool.poll_waiters(1);
                    Error::DeadlineExceeded
                }
                "close" => {
                    pool.close();
                    Error::Unavailable
                }
                "stop" => {
                    admission.stop();
                    pool.poll_waiters(1);
                    Error::Unavailable
                }
                _ => Error::Internal,
            };
            if case != "drop" {
                assert!(count.count() > 0);
                assert!(
                    matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                );
            }
            drop(wait);
            assert!(pool.state.borrow().waiting.is_empty());
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
            assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
            assert_eq!(reactor.in_flight(), 0);
            drop((held, peer));
            pool.close();
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }

    #[test]
    fn worker_tick_wakes_bounded_round_robin_waiters_even_in_nested_executor() {
        use futures::{Stream, stream::FuturesUnordered};
        let (_, _, pool) = setup();
        let (listener, endpoint) = Listener::peer();
        let (_held, _peer) = held(&pool, &endpoint, &listener);
        let mut scope = scope();
        scope.deadline.0 = Instant::now() + Duration::from_millis(20);
        let mut futures = FuturesUnordered::new();
        futures.push(pool.checkout_wait(&endpoint, &scope));
        futures.push(pool.checkout_wait(&endpoint, &scope));
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(
            std::pin::Pin::new(&mut futures)
                .poll_next(&mut cx)
                .is_pending()
        );
        std::thread::sleep(Duration::from_millis(30));
        // Outer polling alone does not drive a sleeping child.
        assert!(
            std::pin::Pin::new(&mut futures)
                .poll_next(&mut cx)
                .is_pending()
        );
        pool.poll_waiters(0);
        assert!(
            std::pin::Pin::new(&mut futures)
                .poll_next(&mut cx)
                .is_pending()
        );
        for _ in 0..2 {
            pool.poll_waiters(1);
            assert!(matches!(
                std::pin::Pin::new(&mut futures).poll_next(&mut cx),
                Poll::Ready(Some(Err(Error::DeadlineExceeded)))
            ));
            std::thread::sleep(Duration::from_millis(2));
        }
        assert!(pool.state.borrow().waiting.is_empty());
    }

    #[test]
    fn waiting_connection_quota_releases_and_connect_abandonment_keeps_fence() {
        let (admission, reactor, pool) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let quota = admission
            .reserve(
                None,
                ResourceClass::Connection,
                admission.limit(ResourceClass::Connection),
            )
            .unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let scope = scope();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut wait = pool.checkout_wait(&endpoint, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 0);
        assert!(
            pool.state.borrow().entries.is_empty(),
            "no slot held waiting for global quota"
        );
        drop(quota);
        pool.poll_waiters(1);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        assert!(pool.state.borrow().waiting.is_empty());
        assert_eq!(reactor.in_flight(), 1);
        drop(wait);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(pool.state.borrow().entries[&endpoint].active, 1);
        assert!(matches!(
            pool.checkout(&endpoint, &scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        while reactor.in_flight() != 0 {
            scope.check().unwrap();
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert!(pool.state.borrow().entries.is_empty());
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }

    #[test]
    fn waiting_endpoint_table_and_context_pressure_remain_bounded() {
        let (admission, reactor, mut pool) = setup();
        pool.max_endpoints = 1;
        let (listener, first) = Listener::peer();
        let (second_listener, second) = Listener::peer();
        let (held, peer) = held(&pool, &first, &listener);
        let baseline = admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let context = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limit(ResourceClass::RequestContext) - baseline,
            )
            .unwrap();
        assert!(matches!(
            pool.checkout_wait(&second, &scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(pool.state.borrow().waiting.is_empty());
        assert_eq!(pool.state.borrow().entries.len(), 1);
        drop(context);
        let mut wait = pool.checkout_wait(&second, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        assert_eq!(pool.state.borrow().entries.len(), 1);
        drop(held);
        // An idle-only endpoint is evicted by a real checkout rather than waiting
        // for idle timeout to free the endpoint table.
        let connection = drive(&reactor, wait.as_mut()).unwrap();
        assert!(second_listener.accept().is_some());
        assert_eq!(pool.state.borrow().entries.len(), 1);
        assert!(!pool.state.borrow().entries.contains_key(&first));
        drop((connection, wait, peer));
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn origin_uid_churn_preserves_endpoint_and_connection_bounds() {
        let (admission, _, mut pool) = setup();
        pool.max_endpoints = 1;
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            unreachable!()
        };
        let origin = |uid: usize| Endpoint::Origin {
            cache: crate::model::CacheId(format!("cache-{uid}")),
            path: path.clone(),
        };
        let (first, peer) = held(&pool, &origin(0), &listener);
        assert!(matches!(
            pool.prepare_connection(&origin(1)),
            Err(Error::Overloaded)
        ));
        assert_eq!(pool.state.borrow().entries.len(), 1);
        drop((first, peer));
        for uid in 1..32 {
            let (connection, peer) = held(&pool, &origin(uid), &listener);
            assert_eq!(pool.state.borrow().entries.len(), 1);
            assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
            drop((connection, peer));
        }
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn origin_cap_is_independent_of_peer_cap_and_idle_quota_is_reclaimed() {
        let (admission, _, pool) = setup();
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            unreachable!()
        };
        let origin = Endpoint::Origin {
            cache: crate::model::CacheId("cache".into()),
            path,
        };
        let (first, a) = held(&pool, &origin, &listener);
        let (second, b) = held(&pool, &origin, &listener);
        drop(second);
        let (second, address) = pool.prepare_connection(&origin).unwrap();
        assert!(
            address.is_none(),
            "second origin slot available despite peer cap one"
        );
        assert!(matches!(
            pool.prepare_connection(&origin),
            Err(Error::Overloaded)
        ));
        drop((first, second, a, b));
        let quota = admission
            .reserve(
                None,
                ResourceClass::Connection,
                admission.limit(ResourceClass::Connection) - 1,
            )
            .unwrap();
        let other = Endpoint::Peer("127.0.0.1:9".into());
        let (lease, _) = pool.prepare_connection(&other).unwrap();
        assert_eq!(pool.state.borrow().entries[&origin].idle.len(), 0);
        drop((lease, quota));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn metadata_bypasses_queued_pages_within_existing_origin_cap() {
        let (admission, _, pool) = setup();
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            unreachable!()
        };
        let endpoint = Endpoint::Origin {
            cache: crate::model::CacheId("cache".into()),
            path,
        };
        let (first, a) = held(&pool, &endpoint, &listener);
        let (idle, b) = held(&pool, &endpoint, &listener);
        let baseline = admission.used(ResourceClass::RequestContext);
        drop(idle);
        let scope = scope();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut page = pool.checkout_wait(&endpoint, &scope);
        assert!(page.as_mut().poll(&mut cx).is_pending());
        let mut metadata = pool.checkout_metadata(&endpoint, &scope);
        let Poll::Ready(Ok(metadata)) = metadata.as_mut().poll(&mut cx) else {
            panic!("metadata queued behind page");
        };
        assert_eq!(pool.state.borrow().entries[&endpoint].active, 2);
        assert!(page.as_mut().poll(&mut cx).is_pending());
        drop((metadata, first, page, a, b));
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }

    #[test]
    fn invalidated_active_generation_cannot_reenter_idle_and_expiry_releases_quota() {
        let (admission, _, mut pool) = setup();
        pool.idle_timeout = Duration::ZERO;
        let (listener, endpoint) = Listener::peer();
        for invalidate in [true, false] {
            let (connection, peer) = held(&pool, &endpoint, &listener);
            if invalidate {
                pool.invalidate(&endpoint);
            }
            drop((connection, peer));
            assert_eq!(
                admission.used(ResourceClass::Connection),
                usize::from(!invalidate)
            );
            pool.expire_idle();
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert!(pool.state.borrow().entries.is_empty());
        }
    }
    #[test]
    fn opaque_staging_and_connection_reservations_survive_reactor_abandonment() {
        use super::super::io::OwnedBuffer;
        use crate::model::RequestId;
        use std::task::{Context, Poll};
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let (listener, endpoint) = Listener::peer();
        let (connection, peer) = held(&pool, &endpoint, &listener);
        let scope =
            RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let staging = OwnedBuffer::new(&admission, 4096).unwrap();
        let mut receive = reactor.recv(connection.socket(), staging, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(matches!(receive.as_mut().poll(&mut cx), Poll::Pending));
        drop(receive);
        drop(pool);
        assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
        assert!(admission.used(ResourceClass::RequestContext) >= baseline + 4096);
        let deadline = Instant::now() + Duration::from_secs(5);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        drop(peer);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }
}
