//! Bounded nonblocking TCP/Unix pools. Unfinished exchanges never return to idle.
use super::io::OwnedBuffer;
use crate::runtime::collections::HashMap;
use crate::runtime::reactor::Descriptor as OwnedFd;
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
        reactor::Reactor,
    },
};
use std::{
    cell::RefCell,
    collections::VecDeque,
    future::poll_fn,
    net::SocketAddr,
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
    session: Option<crate::security::connection::Session>,
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
    waiting: VecDeque<Rc<WaitingEntry>>,
    poll_cursor: usize,
    next_waiter_poll: Instant,
}
struct WaitingEntry {
    endpoint: Endpoint,
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
    pub(crate) session: Option<crate::security::connection::Session>,
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
            session: None,
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
                if let Some(reservation) = self.reservation.take() {
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
                entries: HashMap::default(),
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
        Box::pin(async move {
            scope.check()?;
            let (connection, address) = self.prepare_connection(endpoint)?;
            self.connect(connection, address, scope).await
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
                    .find(|entry| &entry.endpoint == endpoint)
                    .cloned();
                let turn = first.as_ref().is_none_or(|first| {
                    waiting
                        .as_ref()
                        .is_some_and(|waiting: &Waiting| Rc::ptr_eq(first, &waiting.entry))
                });
                if turn {
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
                                Endpoint::Peer(address) => address.len(),
                            }
                            + 128,
                    )?;
                    let entry = Rc::new(WaitingEntry {
                        endpoint: endpoint.clone(),
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

    /// Wake a bounded round-robin batch, including child futures whose executor
    /// does not repoll them merely because the outer worker received a timer tick.
    pub fn poll_waiters(&self, budget: usize) {
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
        self.expire_idle();
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
            let limit = match endpoint {
                Endpoint::Unix(_) => self.per_origin,
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
        let reservation = match self.admission.reserve(None, ResourceClass::Connection, 1) {
            Err(Error::Overloaded) => {
                // Idle sockets must not strand this pool's connection quota.
                for entry in self.state.borrow_mut().entries.values_mut() {
                    entry.idle.clear();
                }
                self.admission.reserve(None, ResourceClass::Connection, 1)?
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
impl Drop for HttpPool {
    fn drop(&mut self) {
        self.close();
    }
}

fn idle_healthy(fd: &OwnedFd) -> bool {
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

fn set_nonblocking(fd: &OwnedFd) -> Result<()> {
    fd.set_nonblocking()
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
    Ok((OwnedFd::socket(domain)?, address))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{model::identity::RequestId, test_support::WakeCounter};
    use std::{os::unix::net::UnixStream, sync::Arc, task::Context};

    fn scope() -> RequestScope {
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(5)).unwrap()
    }

    fn setup() -> (Rc<Admission>, Rc<Reactor>, HttpPool) {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.queue_entries = std::num::NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1).with_origin_limit(2);
        (admission, reactor, pool)
    }

    // A live, reusable exchange without submitting I/O, to isolate scheduling.
    fn held(pool: &HttpPool, endpoint: &Endpoint) -> (ConnectionLease, UnixStream) {
        let mut state = pool.state.borrow_mut();
        let entry = state.entries.entry(endpoint.clone()).or_default();
        entry.active += 1;
        let generation = entry.generation;
        let (socket, peer) = UnixStream::pair().unwrap();
        let mut connection = ConnectionLease::new(
            Rc::new(socket.into()),
            pool.admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap(),
            Some(ReturnToPool {
                state: Rc::downgrade(&pool.state),
                endpoint: endpoint.clone(),
                generation,
            }),
        );
        connection.rx_remaining = Some(0);
        connection.tx_remaining = Some(0);
        connection.finish_exchange().unwrap();
        (connection, peer)
    }

    #[test]
    fn origin_wait_is_bounded_fifo_without_blocking_peers_or_other_caches() {
        let (admission, reactor, pool) = setup();
        let endpoint = Endpoint::Unix("/unused/origin".into());
        let (first, _a) = held(&pool, &endpoint);
        let (second, _b) = held(&pool, &endpoint);
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
        for other in [
            Endpoint::Peer("127.0.0.1:9".into()),
            Endpoint::Unix("/unused/other".into()),
        ] {
            let (lease, _peer) = held(&pool, &other);
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
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        drop((lease, next));
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn waiting_cancel_deadline_close_stop_and_drop_release_only_waiter_quota() {
        for case in ["cancel", "deadline", "close", "stop", "drop"] {
            let (admission, reactor, pool) = setup();
            let endpoint = Endpoint::Peer("127.0.0.1:9".into());
            let (held, _peer) = held(&pool, &endpoint);
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
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert_eq!(reactor.in_flight(), 0);
            drop(held);
            pool.close();
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }

    #[test]
    fn worker_tick_wakes_bounded_round_robin_waiters_even_in_nested_executor() {
        use futures::{Stream, stream::FuturesUnordered};
        let (_, _, pool) = setup();
        let endpoint = Endpoint::Peer("127.0.0.1:9".into());
        let (_held, _peer) = held(&pool, &endpoint);
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
        let (admission, _, mut pool) = setup();
        pool.max_endpoints = 1;
        let first = Endpoint::Peer("127.0.0.1:9".into());
        let second = Endpoint::Peer("127.0.0.1:10".into());
        let (held, _peer) = held(&pool, &first);
        let scope = scope();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let context = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limit(ResourceClass::RequestContext),
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
        // An idle-only endpoint is evicted, rather than consuming the entire
        // table until its idle timeout. No actual connection is needed here.
        let (connection, _) = pool.prepare_connection(&second).unwrap();
        assert_eq!(pool.state.borrow().entries.len(), 1);
        assert!(!pool.state.borrow().entries.contains_key(&first));
        drop((connection, wait));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn origin_cap_is_independent_of_peer_cap_and_idle_quota_is_reclaimed() {
        let (admission, _, pool) = setup();
        let origin = Endpoint::Unix("/unused/origin".into());
        let (first, _a) = held(&pool, &origin);
        let (second, _b) = held(&pool, &origin);
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
        drop((first, second));
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
