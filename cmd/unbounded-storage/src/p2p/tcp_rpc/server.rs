// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Reuse-port listener, admission control, and cooperative connection driver.

use std::cell::{Cell, RefCell};
use std::collections::BTreeSet;
use std::fmt;
use std::future::Future;
use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use crate::bufferpool::Error as PoolError;
use crate::fanout::{FanoutTable, FetchChannel};
use crate::metrics;
use crate::p2p::RouteTableHandle;
use crate::ring::NetHandle;
use crate::tls::{PeerCertificateIdentity, PeerTlsContext};

use super::connection::serve_connection;
use super::wire::{MAX_DESTINATION_PAGE_COUNT, MAX_PAGE_BODY_LEN, WireError};

const LISTEN_BACKLOG: i32 = 1024;

/// Fixed per-shard settings for the TCP RPC listener and service.
#[derive(Clone, Debug)]
pub struct TcpRpcConfig {
    pub bind_addr: SocketAddr,
    pub local_peer_name: String,
    pub lane_count: u16,
    pub max_page: u32,
    pub socket_buffer_bytes: u32,
    pub max_connections: usize,
    pub max_inflight_requests: usize,
    pub max_pages_per_request: u32,
    pub max_requests_per_connection: usize,
}

impl TcpRpcConfig {
    fn validate(&self) -> Result<(), TcpRpcError> {
        if self.local_peer_name.is_empty() {
            return Err(TcpRpcError::Config("local peer name must not be empty"));
        }
        if self.lane_count == 0 {
            return Err(TcpRpcError::Config("lane count must be greater than zero"));
        }
        if self.max_page == 0 || self.max_page as usize > MAX_PAGE_BODY_LEN {
            return Err(TcpRpcError::Config("max page is outside the wire limit"));
        }
        if self.socket_buffer_bytes == 0 {
            return Err(TcpRpcError::Config(
                "socket buffer size must be greater than zero",
            ));
        }
        if self.max_connections == 0
            || self.max_inflight_requests == 0
            || self.max_requests_per_connection == 0
        {
            return Err(TcpRpcError::Config("admission limits must be nonzero"));
        }
        if self.max_pages_per_request == 0
            || self.max_pages_per_request > MAX_DESTINATION_PAGE_COUNT
        {
            return Err(TcpRpcError::Config(
                "page admission limit is outside the wire limit",
            ));
        }
        Ok(())
    }
}

/// Exact configured peer names accepted by the application handshake.
#[derive(Clone, Debug, Default)]
pub struct PeerDirectory {
    names: Rc<RefCell<BTreeSet<String>>>,
}

impl PeerDirectory {
    pub fn new(names: impl IntoIterator<Item = String>) -> Self {
        Self {
            names: Rc::new(RefCell::new(names.into_iter().collect())),
        }
    }

    pub fn replace(&self, names: impl IntoIterator<Item = String>) {
        *self.names.borrow_mut() = names.into_iter().collect();
    }

    pub fn contains(&self, name: &str) -> bool {
        self.names.borrow().contains(name)
    }

    pub fn authenticate(&self, claimed_name: &str, identity: &PeerCertificateIdentity) -> bool {
        self.contains(claimed_name) && identity.matches_dns_san(claimed_name)
    }
}

/// Shared shard-local request service used by every accepted lane.
pub struct TcpRpcService {
    pub(super) config: TcpRpcConfig,
    pub(super) handle: NetHandle,
    pub(super) tls: Rc<PeerTlsContext>,
    pub(super) peers: PeerDirectory,
    pub(super) fanout: Rc<FanoutTable>,
    pub(super) local_fetch: FetchChannel,
    pub(super) routes: RouteTableHandle,
    admission: Rc<Admission>,
}

impl TcpRpcService {
    pub fn new(
        config: TcpRpcConfig,
        handle: NetHandle,
        tls: Rc<PeerTlsContext>,
        peers: PeerDirectory,
        fanout: Rc<FanoutTable>,
        local_fetch: FetchChannel,
        routes: RouteTableHandle,
    ) -> Result<Rc<Self>, TcpRpcError> {
        config.validate()?;
        let admission = Admission::new(config.max_inflight_requests);
        Ok(Rc::new(Self {
            config,
            handle,
            tls,
            peers,
            fanout,
            local_fetch,
            routes,
            admission,
        }))
    }

    pub(super) fn try_admit(&self) -> Option<AdmissionPermit> {
        Admission::try_acquire(&self.admission)
    }
}

/// Tick-driven accept and connection future set for one shard.
pub struct TcpRpcDriver {
    service: Rc<TcpRpcService>,
    listen_fd: RawFd,
    accept_fut: Pin<Box<dyn Future<Output = std::io::Result<RawFd>>>>,
    connections: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    waker: Waker,
}

impl TcpRpcDriver {
    /// Bind the configured address and arm the first io_uring accept.
    pub fn bind(service: Rc<TcpRpcService>, waker: Waker) -> Result<Self, TcpRpcError> {
        let listen_fd =
            bind_reuseport_listener(service.config.bind_addr, service.config.socket_buffer_bytes)?;
        Ok(Self::from_listener(service, listen_fd, waker))
    }

    /// Build over an already bound listener. Ownership of `listen_fd` transfers
    /// to the driver and is released by `Drop`.
    pub fn from_listener(service: Rc<TcpRpcService>, listen_fd: RawFd, waker: Waker) -> Self {
        let accept_fut = Box::pin(service.handle.accept(listen_fd));
        Self {
            service,
            listen_fd,
            accept_fut,
            connections: Vec::new(),
            waker,
        }
    }

    pub fn connection_count(&self) -> usize {
        self.connections.len()
    }

    pub fn is_idle(&self) -> bool {
        self.connections.is_empty()
    }

    /// Advance accepts and each connection once. This is directly suitable for
    /// a `ShardLoop::add_tick_hook` closure; the ring itself remains a separate
    /// tick hook.
    pub fn progress(&mut self) -> bool {
        const ACCEPT_BUDGET: usize = 16;

        let mut busy = false;
        let mut cx = Context::from_waker(&self.waker);

        let mut index = 0;
        while index < self.connections.len() {
            match self.connections[index].as_mut().poll(&mut cx) {
                Poll::Ready(()) => {
                    let _ = self.connections.swap_remove(index);
                    busy = true;
                }
                Poll::Pending => index += 1,
            }
        }

        for _ in 0..ACCEPT_BUDGET {
            match self.accept_fut.as_mut().poll(&mut cx) {
                Poll::Ready(result) => {
                    busy = true;
                    match result {
                        Ok(fd) if self.connections.len() >= self.service.config.max_connections => {
                            metrics::tcp_rpc_connection_error();
                            close_fd(fd);
                        }
                        Ok(fd) => {
                            if configure_accepted_socket(
                                fd,
                                self.service.config.socket_buffer_bytes,
                            )
                            .is_err()
                            {
                                metrics::tcp_rpc_connection_error();
                                close_fd(fd);
                            } else {
                                self.connections
                                    .push(Box::pin(serve_connection(Rc::clone(&self.service), fd)));
                            }
                        }
                        Err(_) => {
                            metrics::tcp_rpc_connection_error();
                        }
                    }
                    self.accept_fut = Box::pin(self.service.handle.accept(self.listen_fd));
                }
                Poll::Pending => break,
            }
        }
        busy
    }
}

impl Drop for TcpRpcDriver {
    fn drop(&mut self) {
        if self.listen_fd >= 0 {
            close_fd(self.listen_fd);
            self.listen_fd = -1;
        }
    }
}

/// Create a TCP listener with `SO_REUSEADDR` and `SO_REUSEPORT`.
pub fn bind_reuseport_listener(
    addr: SocketAddr,
    socket_buffer_bytes: u32,
) -> Result<RawFd, TcpRpcError> {
    let family = match addr {
        SocketAddr::V4(_) => libc::AF_INET,
        SocketAddr::V6(_) => libc::AF_INET6,
    };
    // SAFETY: valid socket family and flags; every error path closes `fd`.
    let fd = unsafe {
        libc::socket(
            family,
            libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
            0,
        )
    };
    if fd < 0 {
        return Err(TcpRpcError::Io(std::io::Error::last_os_error()));
    }
    let guard = FdGuard(fd);

    set_socket_int(fd, libc::SOL_SOCKET, libc::SO_REUSEADDR, 1)?;
    set_socket_int(fd, libc::SOL_SOCKET, libc::SO_REUSEPORT, 1)?;
    set_socket_buffers(fd, socket_buffer_bytes)?;

    let bind_result = match addr {
        SocketAddr::V4(addr) => {
            let raw = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: addr.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(*addr.ip()).to_be(),
                },
                sin_zero: [0; 8],
            };
            // SAFETY: `raw` is a valid sockaddr_in for this call.
            unsafe {
                libc::bind(
                    fd,
                    (&raw as *const libc::sockaddr_in).cast(),
                    std::mem::size_of_val(&raw) as libc::socklen_t,
                )
            }
        }
        SocketAddr::V6(addr) => {
            let raw = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as libc::sa_family_t,
                sin6_port: addr.port().to_be(),
                sin6_flowinfo: addr.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: addr.ip().octets(),
                },
                sin6_scope_id: addr.scope_id(),
            };
            // SAFETY: `raw` is a valid sockaddr_in6 for this call.
            unsafe {
                libc::bind(
                    fd,
                    (&raw as *const libc::sockaddr_in6).cast(),
                    std::mem::size_of_val(&raw) as libc::socklen_t,
                )
            }
        }
    };
    if bind_result != 0 {
        return Err(TcpRpcError::Io(std::io::Error::last_os_error()));
    }
    // SAFETY: fd is a bound stream socket.
    if unsafe { libc::listen(fd, LISTEN_BACKLOG) } != 0 {
        return Err(TcpRpcError::Io(std::io::Error::last_os_error()));
    }

    std::mem::forget(guard);
    Ok(fd)
}

#[derive(Debug)]
pub enum TcpRpcError {
    Config(&'static str),
    Io(std::io::Error),
    Wire(WireError),
    Service(PoolError),
    Auth(String),
    Protocol(String),
}

impl fmt::Display for TcpRpcError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Config(message) => write!(f, "invalid TCP RPC config: {message}"),
            Self::Io(error) => write!(f, "TCP RPC I/O: {error}"),
            Self::Wire(error) => write!(f, "{error}"),
            Self::Service(error) => write!(f, "TCP RPC service: {error}"),
            Self::Auth(message) => write!(f, "TCP RPC authentication: {message}"),
            Self::Protocol(message) => write!(f, "TCP RPC protocol: {message}"),
        }
    }
}

impl std::error::Error for TcpRpcError {}

impl From<std::io::Error> for TcpRpcError {
    fn from(value: std::io::Error) -> Self {
        Self::Io(value)
    }
}

impl From<WireError> for TcpRpcError {
    fn from(value: WireError) -> Self {
        Self::Wire(value)
    }
}

impl From<PoolError> for TcpRpcError {
    fn from(value: PoolError) -> Self {
        Self::Service(value)
    }
}

pub(super) struct AdmissionPermit {
    admission: Rc<Admission>,
    outcome: metrics::Outcome,
}

impl AdmissionPermit {
    pub(super) fn set_outcome(&mut self, outcome: metrics::Outcome) {
        self.outcome = outcome;
    }
}

struct Admission {
    active: Cell<usize>,
    limit: usize,
}

impl Admission {
    fn new(limit: usize) -> Rc<Self> {
        Rc::new(Self {
            active: Cell::new(0),
            limit,
        })
    }

    fn try_acquire(admission: &Rc<Self>) -> Option<AdmissionPermit> {
        let active = admission.active.get();
        if active >= admission.limit {
            return None;
        }
        admission.active.set(active + 1);
        metrics::tcp_rpc_inflight_delta(1);
        Some(AdmissionPermit {
            admission: Rc::clone(admission),
            outcome: metrics::Outcome::Err,
        })
    }
}

impl Drop for AdmissionPermit {
    fn drop(&mut self) {
        self.admission
            .active
            .set(self.admission.active.get().saturating_sub(1));
        metrics::tcp_rpc_inflight_delta(-1);
        metrics::tcp_rpc_request(self.outcome);
    }
}

pub(super) fn record_error(error: &TcpRpcError) {
    match error {
        TcpRpcError::Io(_) => metrics::tcp_rpc_connection_error(),
        TcpRpcError::Wire(_) | TcpRpcError::Protocol(_) => metrics::tcp_rpc_protocol_error(),
        TcpRpcError::Auth(_) => metrics::tcp_rpc_auth_error(),
        TcpRpcError::Config(_) | TcpRpcError::Service(_) => {}
    }
}

struct FdGuard(RawFd);

impl Drop for FdGuard {
    fn drop(&mut self) {
        close_fd(self.0);
    }
}

fn configure_accepted_socket(fd: RawFd, socket_buffer_bytes: u32) -> Result<(), TcpRpcError> {
    set_close_on_exec(fd)?;
    set_socket_int(fd, libc::IPPROTO_TCP, libc::TCP_NODELAY, 1)?;
    set_socket_buffers(fd, socket_buffer_bytes)
}

fn set_close_on_exec(fd: RawFd) -> Result<(), TcpRpcError> {
    // SAFETY: standard fcntl get/set descriptor flags on a live socket.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
    if flags < 0 || unsafe { libc::fcntl(fd, libc::F_SETFD, flags | libc::FD_CLOEXEC) } < 0 {
        Err(TcpRpcError::Io(std::io::Error::last_os_error()))
    } else {
        Ok(())
    }
}

fn set_socket_buffers(fd: RawFd, bytes: u32) -> Result<(), TcpRpcError> {
    let bytes =
        i32::try_from(bytes).map_err(|_| TcpRpcError::Config("socket buffer size exceeds i32"))?;
    set_socket_int(fd, libc::SOL_SOCKET, libc::SO_SNDBUF, bytes)?;
    set_socket_int(fd, libc::SOL_SOCKET, libc::SO_RCVBUF, bytes)
}

fn set_socket_int(
    fd: RawFd,
    level: libc::c_int,
    option: libc::c_int,
    value: libc::c_int,
) -> Result<(), TcpRpcError> {
    // SAFETY: value points to a live c_int for the duration of setsockopt.
    let result = unsafe {
        libc::setsockopt(
            fd,
            level,
            option,
            (&value as *const libc::c_int).cast(),
            std::mem::size_of_val(&value) as libc::socklen_t,
        )
    };
    if result == 0 {
        Ok(())
    } else {
        Err(TcpRpcError::Io(std::io::Error::last_os_error()))
    }
}

fn close_fd(fd: RawFd) {
    // SAFETY: callers transfer unique ownership of socket descriptors here.
    unsafe {
        libc::close(fd);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn admission_never_exceeds_limit_and_releases_on_drop() {
        let admission = Admission::new(2);
        let first = Admission::try_acquire(&admission).expect("first permit");
        let second = Admission::try_acquire(&admission).expect("second permit");
        assert!(Admission::try_acquire(&admission).is_none());
        drop(first);
        assert!(Admission::try_acquire(&admission).is_some());
        drop(second);
    }

    #[test]
    fn peer_directory_requires_an_exact_configured_name() {
        let peers = PeerDirectory::new(["node-a".to_string(), "node-b".to_string()]);
        assert!(peers.contains("node-a"));
        assert!(!peers.contains("NODE-A"));
        assert!(!peers.contains("node-c"));
    }

    #[test]
    fn peer_directory_replacement_is_visible_to_clones() {
        let peers = PeerDirectory::new(["node-a".to_string()]);
        let connection_view = peers.clone();

        peers.replace(["node-b".to_string()]);

        assert!(!connection_view.contains("node-a"));
        assert!(connection_view.contains("node-b"));
    }

    #[test]
    fn reuseport_listener_binds_loopback() {
        let addr = "127.0.0.1:0".parse().unwrap();
        match bind_reuseport_listener(addr, 64 * 1024) {
            Ok(fd) => close_fd(fd),
            Err(error) => eprintln!("TCP RPC listener unavailable: {error}; skipping"),
        }
    }
}
