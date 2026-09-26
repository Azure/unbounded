//! Bounded nonblocking TCP/Unix pools. Unfinished exchanges never return to idle.
use super::io::OwnedBuffer;
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::{Cancellation, RequestScope},
        reactor::Reactor,
    },
};
use std::{
    cell::RefCell,
    collections::{BTreeMap, HashMap},
    net::SocketAddr,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    path::PathBuf,
    rc::{Rc, Weak},
    task::{Poll, Waker},
    time::{Duration, Instant},
};

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub enum Endpoint {
    Unix(PathBuf),
    /// Numeric IP:port only. DNS resolution is deliberately not performed on an
    /// I/O worker. Control-plane DNS must supply an already-resolved endpoint.
    Peer(String),
}
struct Idle {
    fd: Rc<OwnedFd>,
    reservation: Reservation,
    since: Instant,
}
#[derive(Default)]
struct Entry {
    active: usize,
    idle: Vec<Idle>,
    generation: u64,
}
struct PoolState {
    entries: HashMap<Endpoint, Entry>,
    next_generation: u64,
    closed: bool,
    waiters: BTreeMap<u64, PeerWaiter>,
    next_waiter: u64,
    incoming_idle: Vec<Weak<IncomingIdle>>,
}
pub(crate) struct IncomingIdle {
    fd: Weak<OwnedFd>,
    pub(crate) scope: RequestScope,
    _context: Reservation,
}
struct PeerWaiter {
    global_capacity: bool,
    endpoint: Endpoint,
    scope: RequestScope,
    waker: Waker,
}
struct PeerWaitRegistration {
    state: Weak<RefCell<PoolState>>,
    id: u64,
    _reservation: Reservation,
    _context: Reservation,
}
impl Drop for PeerWaitRegistration {
    fn drop(&mut self) {
        if let Some(state) = self.state.upgrade() {
            state.borrow_mut().waiters.remove(&self.id);
        }
    }
}
fn wake_peer_waiters(state: &Rc<RefCell<PoolState>>, endpoint: &Endpoint) {
    let wakes: Vec<_> = state
        .borrow()
        .waiters
        .values()
        .filter(|waiter| &waiter.endpoint == endpoint)
        .map(|waiter| waiter.waker.clone())
        .collect();
    for wake in wakes {
        wake.wake();
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
    pub(crate) fd: Rc<OwnedFd>,
    reusable: bool,
    reservation: Option<Reservation>,
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
    pub fn from_accepted(fd: OwnedFd, admission: &Admission) -> Result<Self> {
        set_nonblocking(&fd)?;
        let reservation = admission.reserve(None, ResourceClass::Connection, 1)?;
        Ok(Self::new(Rc::new(fd), reservation, None))
    }
    fn new(fd: Rc<OwnedFd>, reservation: Reservation, pool: Option<ReturnToPool>) -> Self {
        Self {
            fd,
            reusable: false,
            reservation: Some(reservation),
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
    /// Explicitly mark the complete request/response exchange reusable. This is
    /// valid for client or server use and resets framing for the next exchange.
    /// Unexpected pipelined/read-ahead data prevents pool return.
    pub fn finish_exchange(&mut self) -> Result<()> {
        if self.rx_remaining != Some(0) || self.tx_remaining != Some(0) || self.read_ahead.is_some()
        {
            return Err(Error::InvalidRequest);
        }
        self.reusable = !self.close;
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
    pub fn socket(&self) -> Rc<OwnedFd> {
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
        let Some(pool_state) = target.state.upgrade() else {
            return;
        };
        let mut state = pool_state.borrow_mut();
        let closed = state.closed;
        if let Some(entry) = state.entries.get_mut(&target.endpoint) {
            entry.active = entry.active.saturating_sub(1);
            if !closed
                && entry.generation == target.generation
                && self.reusable
                && Rc::strong_count(&self.fd) == 1
            {
                if let Some(reservation) = self.reservation.take() {
                    entry.idle.push(Idle {
                        fd: self.fd.clone(),
                        reservation,
                        since: Instant::now(),
                    });
                }
            }
            if entry.active == 0 && entry.idle.is_empty() {
                state.entries.remove(&target.endpoint);
            }
        }
        drop(state);
        wake_peer_waiters(&pool_state, &target.endpoint);
    }
}

pub struct HttpPool {
    #[cfg(test)]
    pub(crate) peer_waits: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(crate) incoming_reclaims: std::cell::Cell<usize>,
    reactor: Rc<Reactor>,
    admission: Rc<Admission>,
    per_endpoint: usize,
    max_endpoints: usize,
    idle_timeout: Duration,
    state: Rc<RefCell<PoolState>>,
}
impl HttpPool {
    /// Only the peer server calls this after a complete successful exchange.
    pub(crate) fn register_incoming_idle(
        &self,
        connection: &ConnectionLease,
        scope: &RequestScope,
    ) -> Result<Rc<IncomingIdle>> {
        if !connection.is_reusable() || connection.pool.is_some() {
            return Err(Error::InvalidRequest);
        }
        let mut state = self.state.borrow_mut();
        state
            .incoming_idle
            .retain(|entry| entry.strong_count() != 0);
        if state.closed
            || state.incoming_idle.len() >= self.admission.limits().client_connections.get()
        {
            return Err(Error::Overloaded);
        }
        let mut idle_scope = scope.clone();
        idle_scope.cancellation = Cancellation::new()?;
        let idle = Rc::new(IncomingIdle {
            fd: Rc::downgrade(&connection.fd),
            scope: idle_scope,
            _context: self.admission.reserve(
                None,
                ResourceClass::RequestContext,
                std::mem::size_of::<IncomingIdle>(),
            )?,
        });
        state.incoming_idle.push(Rc::downgrade(&idle));
        Ok(idle)
    }
    fn reclaim_incoming_idle(&self) -> Result<bool> {
        let candidates: Vec<_> = self
            .state
            .borrow()
            .incoming_idle
            .iter()
            .filter_map(Weak::upgrade)
            .collect();
        for idle in candidates {
            if idle.scope.cancellation.is_cancelled() {
                continue;
            }
            let Some(fd) = idle.fd.upgrade() else {
                continue;
            };
            let mut byte = 0u8;
            // Readiness never consumes bytes. A partial/new head excludes this
            // connection from reclamation even before its task observes readiness.
            let count = unsafe {
                libc::recv(
                    fd.as_raw_fd(),
                    (&mut byte as *mut u8).cast(),
                    1,
                    libc::MSG_PEEK | libc::MSG_DONTWAIT,
                )
            };
            if count < 0 && std::io::Error::last_os_error().kind() == std::io::ErrorKind::WouldBlock
            {
                idle.scope.cancel()?;
                #[cfg(test)]
                self.incoming_reclaims.set(self.incoming_reclaims.get() + 1);
                return Ok(true);
            }
        }
        Ok(false)
    }
    /// Accept into the same worker quota as outbound traffic. A completed idle
    /// exchange may yield its slot before a new client or neighbor is rejected.
    /// The accepted FD is retained across the admission retry.
    pub fn accept(&self, fd: OwnedFd) -> Result<ConnectionLease> {
        if self.state.borrow().closed {
            return Err(Error::Unavailable);
        }
        set_nonblocking(&fd)?;
        let reservation = self.reserve_connection()?;
        Ok(ConnectionLease::new(Rc::new(fd), reservation, None))
    }
    fn reserve_connection(&self) -> Result<Reservation> {
        match self.admission.reserve(None, ResourceClass::Connection, 1) {
            Err(Error::Overloaded) if self.reclaim_idle_connection() => {
                self.admission.reserve(None, ResourceClass::Connection, 1)
            }
            result => result,
        }
    }
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
            #[cfg(test)]
            peer_waits: std::cell::Cell::new(0),
            #[cfg(test)]
            incoming_reclaims: std::cell::Cell::new(0),
            reactor,
            admission,
            per_endpoint,
            max_endpoints,
            idle_timeout,
            state: Rc::new(RefCell::new(PoolState {
                entries: HashMap::new(),
                next_generation: 0,
                closed: false,
                waiters: BTreeMap::new(),
                next_waiter: 0,
                incoming_idle: Vec::new(),
            })),
        }
    }
    /// Already-admitted peer work may wait for its selected neighbor's active
    /// slot. No route attempt is retried, and no connection or byte limit grows.
    /// The finite waiter table and original scope bound retention under pressure.
    pub fn checkout_peer<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            let mut registration: Option<PeerWaitRegistration> = None;
            std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                let mut state = self.state.borrow_mut();
                if state.closed {
                    return Poll::Ready(Err(Error::Unavailable));
                }
                if state
                    .entries
                    .get(endpoint)
                    .is_none_or(|entry| entry.active < self.per_endpoint)
                {
                    return Poll::Ready(Ok(()));
                }
                if let Some(registration) = &registration {
                    state
                        .waiters
                        .get_mut(&registration.id)
                        .ok_or(Error::Internal)?
                        .waker
                        .clone_from(cx.waker());
                } else {
                    if state.waiters.len() >= self.admission.limits().queue_entries.get() {
                        return Poll::Ready(Err(Error::Overloaded));
                    }
                    let reservation = self.admission.reserve(None, ResourceClass::Waiter, 1)?;
                    let context = self.admission.reserve(
                        None,
                        ResourceClass::RequestContext,
                        std::mem::size_of::<PeerWaiter>()
                            + std::mem::size_of::<PeerWaitRegistration>()
                            + match endpoint {
                                Endpoint::Peer(value) => value.len(),
                                Endpoint::Unix(value) => value.as_os_str().len(),
                            },
                    )?;
                    let id = state.next_waiter.checked_add(1).ok_or(Error::Overloaded)?;
                    #[cfg(test)]
                    self.peer_waits.set(self.peer_waits.get() + 1);
                    state.next_waiter = id;
                    state.waiters.insert(
                        id,
                        PeerWaiter {
                            global_capacity: false,
                            endpoint: endpoint.clone(),
                            scope: scope.clone(),
                            waker: cx.waker().clone(),
                        },
                    );
                    registration = Some(PeerWaitRegistration {
                        state: Rc::downgrade(&self.state),
                        id,
                        _reservation: reservation,
                        _context: context,
                    });
                }
                Poll::Pending
            })
            .await?;
            drop(registration);
            self.checkout_inner(endpoint, scope, true).await
        })
    }
    /// Worker timer tick wakes expired peer admission waiters, even if every
    /// active exchange is stalled. The table is capped by queue_entries.
    pub fn poll_peer_waiters(&self) {
        let wakes: Vec<_> = self
            .state
            .borrow()
            .waiters
            .values()
            .filter(|waiter| {
                waiter.scope.check().is_err()
                    || (waiter.global_capacity
                        && self.admission.used(ResourceClass::Connection)
                            < self.admission.limit(ResourceClass::Connection))
            })
            .map(|waiter| waiter.waker.clone())
            .collect();
        for wake in wakes {
            wake.wake();
        }
    }
    /// Capacity exhaustion fails immediately with Overloaded: there is no hidden
    /// unbounded waiter queue. Callers schedule any retry under their own budget.
    pub fn checkout<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.checkout_inner(endpoint, scope, false)
    }
    fn checkout_inner<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        scope: &'a RequestScope,
        reclaim_incoming: bool,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            scope.check()?;
            self.expire_idle();
            let (idle, generation) = {
                let mut state = self.state.borrow_mut();
                if state.closed {
                    return Err(Error::Unavailable);
                }
                if !state.entries.contains_key(endpoint) {
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
                if entry.active >= self.per_endpoint {
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
                    return Ok(ConnectionLease::new(
                        idle.fd,
                        idle.reservation,
                        slot.0.take(),
                    ));
                }
            }
            let reservation = match self.reserve_connection() {
                Err(Error::Overloaded) if reclaim_incoming && self.reclaim_incoming_idle()? => {
                    // Wait only for the selected idle owner's completion fence.
                    // No connection/request has been submitted or retried here.
                    let mut registration: Option<PeerWaitRegistration> = None;
                    let cancellation = scope.cancellation.subscribe()?;
                    std::future::poll_fn(|cx| {
                        cancellation.register(cx.waker());
                        scope.check()?;
                        if self.state.borrow().closed {
                            return Poll::Ready(Err(Error::Unavailable));
                        }
                        match self.reserve_connection() {
                            Err(Error::Overloaded) => {
                                let mut state = self.state.borrow_mut();
                                if let Some(registration) = &registration {
                                    state
                                        .waiters
                                        .get_mut(&registration.id)
                                        .ok_or(Error::Internal)?
                                        .waker
                                        .clone_from(cx.waker());
                                } else {
                                    if state.waiters.len()
                                        >= self.admission.limits().queue_entries.get()
                                    {
                                        return Poll::Ready(Err(Error::Overloaded));
                                    }
                                    let id = state
                                        .next_waiter
                                        .checked_add(1)
                                        .ok_or(Error::Overloaded)?;
                                    let permit =
                                        self.admission.reserve(None, ResourceClass::Waiter, 1)?;
                                    let context = self.admission.reserve(
                                        None,
                                        ResourceClass::RequestContext,
                                        std::mem::size_of::<PeerWaiter>()
                                            + std::mem::size_of::<PeerWaitRegistration>()
                                            + match endpoint {
                                                Endpoint::Peer(value) => value.len(),
                                                Endpoint::Unix(value) => value.as_os_str().len(),
                                            },
                                    )?;
                                    state.next_waiter = id;
                                    state.waiters.insert(
                                        id,
                                        PeerWaiter {
                                            global_capacity: true,
                                            endpoint: endpoint.clone(),
                                            scope: scope.clone(),
                                            waker: cx.waker().clone(),
                                        },
                                    );
                                    registration = Some(PeerWaitRegistration {
                                        state: Rc::downgrade(&self.state),
                                        id,
                                        _reservation: permit,
                                        _context: context,
                                    });
                                }
                                Poll::Pending
                            }
                            result => Poll::Ready(result),
                        }
                    })
                    .await?
                }
                result => result?,
            };
            let (fd, address) = create_socket(endpoint)?;
            let connection = ConnectionLease::new(Rc::new(fd), reservation, slot.0.take());
            // Retain socket, admission and pool slot in the reactor through the
            // original/cancellation fences, even after both future and pool drop.
            self.reactor
                .connect_with_lease(connection.socket(), address, connection, scope)
                .await
        })
    }
    pub fn expire_idle(&self) {
        let now = Instant::now();
        let mut state = self.state.borrow_mut();
        state.entries.retain(|_, entry| {
            entry
                .idle
                .retain(|idle| now.duration_since(idle.since) < self.idle_timeout);
            entry.active != 0 || !entry.idle.is_empty()
        });
    }
    /// An idle connection is an optimization, not a reason to reject work for
    /// another neighbor. Reclaim only one completed lease; active and connecting
    /// operations retain their permits through their existing completion fences.
    fn reclaim_idle_connection(&self) -> bool {
        let mut state = self.state.borrow_mut();
        let oldest = state
            .entries
            .iter()
            .flat_map(|(endpoint, entry)| {
                entry
                    .idle
                    .iter()
                    .enumerate()
                    .map(move |(index, idle)| (endpoint, index, idle.since))
            })
            .min_by_key(|(_, _, since)| *since);
        let Some((endpoint, index, _)) = oldest else {
            return false;
        };
        let endpoint = endpoint.clone();
        let entry = state
            .entries
            .get_mut(&endpoint)
            .expect("selected idle endpoint");
        entry.idle.swap_remove(index);
        if entry.active == 0 && entry.idle.is_empty() {
            state.entries.remove(&endpoint);
        }
        true
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
        let wakes: Vec<_> = state
            .waiters
            .values()
            .map(|waiter| waiter.waker.clone())
            .collect();
        drop(state);
        for wake in wakes {
            wake.wake();
        }
    }
}
impl Drop for HttpPool {
    fn drop(&mut self) {
        self.close();
    }
}

fn idle_healthy(fd: &OwnedFd) -> bool {
    let mut byte = 0u8;
    // SAFETY: a synchronous nonblocking peek uses a valid one-byte destination.
    // EOF or unsolicited data makes a pooled HTTP exchange unsafe to reuse.
    let result = unsafe {
        libc::recv(
            fd.as_raw_fd(),
            (&mut byte as *mut u8).cast(),
            1,
            libc::MSG_PEEK | libc::MSG_DONTWAIT,
        )
    };
    result < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EAGAIN)
}
struct ConnectingSlot(Option<ReturnToPool>);
impl Drop for ConnectingSlot {
    fn drop(&mut self) {
        if let Some(target) = &self.0 {
            if let Some(pool_state) = target.state.upgrade() {
                let mut state = pool_state.borrow_mut();
                if let Some(entry) = state.entries.get_mut(&target.endpoint) {
                    entry.active = entry.active.saturating_sub(1);
                    if entry.active == 0 && entry.idle.is_empty() {
                        state.entries.remove(&target.endpoint);
                    }
                }
                drop(state);
                wake_peer_waiters(&pool_state, &target.endpoint);
            }
        }
    }
}

fn set_nonblocking(fd: &OwnedFd) -> Result<()> {
    // SAFETY: fcntl accesses no caller memory; fd remains owned throughout.
    unsafe {
        let flags = libc::fcntl(fd.as_raw_fd(), libc::F_GETFL);
        if flags < 0
            || libc::fcntl(fd.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) < 0
            || libc::fcntl(fd.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) < 0
        {
            return Err(Error::Io);
        }
    }
    Ok(())
}

// Runtime's owned address type is used so connect never borrows sockaddr bytes
// from a future that may disappear while its SQE is in flight.
fn create_socket(endpoint: &Endpoint) -> Result<(OwnedFd, crate::runtime::reactor::SocketAddress)> {
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
        Endpoint::Unix(path) => (libc::AF_UNIX, SocketAddress::Unix(path.clone())),
    };
    // SAFETY: socket returns a new exclusively owned FD or -1.
    let raw = unsafe {
        libc::socket(
            domain,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if raw < 0 {
        return Err(Error::Io);
    }
    Ok((unsafe { OwnedFd::from_raw_fd(raw) }, address))
}

#[cfg(test)]
#[path = "pool_peer_tests.rs"]
mod peer_tests;
#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixStream;

    #[test]
    fn accepted_connection_reclaims_only_idle_capacity_and_retains_its_fd() {
        use std::io::{Read, Write};
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.client_connections = std::num::NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor, admission.clone(), 1);
        let (idle, mut idle_peer) = UnixStream::pair().unwrap();
        idle_peer.set_nonblocking(true).unwrap();
        pool.state.borrow_mut().entries.insert(
            Endpoint::Peer("127.0.0.1:1".into()),
            Entry {
                idle: vec![Idle {
                    fd: Rc::new(idle.into()),
                    reservation: admission
                        .reserve(None, ResourceClass::Connection, 1)
                        .unwrap(),
                    since: Instant::now(),
                }],
                ..Entry::default()
            },
        );
        let (active, _active_peer) = UnixStream::pair().unwrap();
        let active = pool.accept(active.into()).unwrap();
        let (incoming, mut incoming_peer) = UnixStream::pair().unwrap();
        let incoming = pool.accept(incoming.into()).unwrap();
        assert_eq!(admission.used(ResourceClass::Connection), 2);
        assert_eq!(idle_peer.read(&mut [0; 1]).unwrap(), 0);
        assert!(pool.state.borrow().entries.is_empty());
        incoming_peer.write_all(b"x").unwrap();
        let mut byte = [0];
        // The original accepted socket, rather than a replacement or closed FD,
        // remains owned after reclaiming the completed outbound lease.
        assert_eq!(
            unsafe { libc::recv(incoming.fd.as_raw_fd(), byte.as_mut_ptr().cast(), 1, 0) },
            1
        );
        assert_eq!(byte, *b"x");
        let (rejected, mut rejected_peer) = UnixStream::pair().unwrap();
        assert!(matches!(
            pool.accept(rejected.into()),
            Err(Error::Overloaded)
        ));
        assert_eq!(rejected_peer.read(&mut [0; 1]).unwrap(), 0);
        drop((incoming, active));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        pool.close();
        let (rejected, _peer) = UnixStream::pair().unwrap();
        assert!(matches!(
            pool.accept(rejected.into()),
            Err(Error::Unavailable)
        ));
    }

    #[test]
    fn checkout_reclaims_idle_connections_before_rejecting_a_new_neighbor() {
        use crate::model::identity::RequestId;
        use std::{num::NonZeroUsize, task::Context};
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.client_connections = NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let mut peers = Vec::new();
        for n in 0..2 {
            let (socket, peer) = UnixStream::pair().unwrap();
            peers.push(peer);
            pool.state.borrow_mut().entries.insert(
                Endpoint::Peer(format!("127.0.0.1:{}", n + 1)),
                Entry {
                    idle: vec![Idle {
                        fd: Rc::new(socket.into()),
                        reservation: admission
                            .reserve(None, ResourceClass::Connection, 1)
                            .unwrap(),
                        since: Instant::now(),
                    }],
                    ..Entry::default()
                },
            );
        }
        let scope =
            RequestScope::new(RequestId([8; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let mut work = pool.checkout(&endpoint, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let connection = loop {
            if let std::task::Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                break result.expect("idle peers must not prevent a new neighbor connection");
            }
            scope.check().unwrap();
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };
        assert_eq!(admission.used(ResourceClass::Connection), 2);
        assert_eq!(
            pool.state
                .borrow()
                .entries
                .values()
                .map(|e| e.idle.len())
                .sum::<usize>(),
            1
        );
        // Active operations cannot be reclaimed, and per-neighbor limits still apply.
        assert!(matches!(
            futures::executor::block_on(pool.checkout(&endpoint, &scope)),
            Err(Error::Overloaded)
        ));
        // Once every slot belongs to active work, another endpoint still fails.
        assert!(pool.reclaim_idle_connection());
        let active = admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let other = Endpoint::Peer("127.0.0.1:3".into());
        assert!(matches!(
            futures::executor::block_on(pool.checkout(&other, &scope)),
            Err(Error::Overloaded)
        ));
        assert_eq!(admission.used(ResourceClass::Connection), 2);
        drop(active);
        drop(connection);
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn invalidated_active_generation_cannot_reenter_idle_and_expiry_releases_quota() {
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::with_limits(reactor, admission.clone(), 1, 1, Duration::ZERO);
        let endpoint = Endpoint::Peer("127.0.0.1:1".into());
        for invalidate in [true, false] {
            pool.state.borrow_mut().entries.insert(
                endpoint.clone(),
                Entry {
                    active: 1,
                    generation: 1,
                    idle: vec![],
                },
            );
            pool.state.borrow_mut().next_generation = 1;
            let (socket, _peer) = UnixStream::pair().unwrap();
            let mut connection = ConnectionLease::new(
                Rc::new(socket.into()),
                admission
                    .reserve(None, ResourceClass::Connection, 1)
                    .unwrap(),
                Some(ReturnToPool {
                    state: Rc::downgrade(&pool.state),
                    endpoint: endpoint.clone(),
                    generation: 1,
                }),
            );
            connection.rx_remaining = Some(0);
            connection.tx_remaining = Some(0);
            connection.finish_exchange().unwrap();
            if invalidate {
                pool.invalidate(&endpoint);
            }
            drop(connection);
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
        use crate::model::identity::RequestId;
        use std::task::{Context, Poll};
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let endpoint = Endpoint::Peer("127.0.0.1:1".into());
        pool.state.borrow_mut().entries.insert(
            endpoint.clone(),
            Entry {
                active: 1,
                generation: 1,
                idle: vec![],
            },
        );
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::new(
            Rc::new(socket.into()),
            admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap(),
            Some(ReturnToPool {
                state: Rc::downgrade(&pool.state),
                endpoint,
                generation: 1,
            }),
        );
        let scope =
            RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let staging = OwnedBuffer::new(&admission, 4096).unwrap();
        let mut receive = reactor.recv(connection.socket(), staging, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(matches!(receive.as_mut().poll(&mut cx), Poll::Pending));
        drop(receive);
        drop(pool);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert!(admission.used(ResourceClass::RequestContext) >= baseline + 4096);
        let deadline = Instant::now() + Duration::from_secs(5);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }
}
