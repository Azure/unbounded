//! Bounded nonblocking TCP/Unix pools. Unfinished exchanges never return to idle.
use super::io::OwnedBuffer;
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
    collections::HashMap,
    net::SocketAddr,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    path::PathBuf,
    rc::{Rc, Weak},
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
    }
}

pub struct HttpPool {
    reactor: Rc<Reactor>,
    admission: Rc<Admission>,
    per_endpoint: usize,
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
            max_endpoints,
            idle_timeout,
            state: Rc::new(RefCell::new(PoolState {
                entries: HashMap::new(),
                next_generation: 0,
                closed: false,
            })),
        }
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
            let reservation = self.admission.reserve(None, ResourceClass::Connection, 1)?;
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
mod tests {
    use super::*;
    use std::os::unix::net::UnixStream;

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
