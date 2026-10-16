//! The simulated network: streams, listeners, datagrams, pipes, readiness
//! and partitions.
//!
//! Streams connect through listeners registered by address (Internet or
//! filesystem Unix paths). Transfers move bytes into the peer's bounded
//! receive buffer ([`World::set_stream_capacity`]); a full buffer makes the
//! sender wait (`EAGAIN`), exactly like a nonblocking socket.
//!
//! # Partitions
//!
//! Every stream carries an optional *endpoint label*: the listening address
//! of the simulated node that owns it. A [`SimBackend`](crate::SimBackend)
//! labels sockets it creates with its
//! [`endpoint`](crate::SimBackend::set_endpoint); accepted streams are
//! labeled with their listener's label (or listening address). A
//! [`World::partition`] between two labels blackholes traffic between them in
//! both directions: sends and connects stall (`EAGAIN`, so asynchronous
//! operations keep waiting) until [`World::heal`]. Bytes already received
//! stay readable; a partition never produces end-of-file. Unlabeled streams
//! are never partitioned.
//!
//! # Readiness
//!
//! Readiness follows Linux `poll(2)` for streams (including half-close
//! `POLLRDHUP`/`POLLHUP`), listeners, datagrams, pipes (`POLLHUP` for a
//! closed writer, `POLLERR` for a closed reader) and regular files (always
//! readable and writable). Like Linux, readiness is not a liveness oracle: a
//! full sender whose peer shut down reading stays non-writable.

use std::cell::RefCell;
use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::io::Read as _;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, Shutdown, SocketAddr};
use std::path::{Path, PathBuf};
use std::rc::Rc;

use uring_runtime::{Result, SockAddr, SocketAddress};

use crate::fault::SyncOp;
use crate::fs::normalize;
use crate::trace::EventKind;
use crate::world::{
    AddressKey, MAX_ALLOCATION, Resource, SimFd, State, World, bounded_bytes, errno,
};

/// The default pipe capacity (64 KiB, as on Linux).
pub const DEFAULT_PIPE_CAPACITY: usize = 64 * 1024;

/// The largest datagram payload (as for UDP over IPv4).
pub const MAX_DATAGRAM: usize = 65507;

/// The most datagrams queued on one endpoint before senders get `EAGAIN`.
pub const DATAGRAM_QUEUE: usize = 64;

const EPHEMERAL_PORTS: std::ops::RangeInclusive<u16> = 49152..=65535;
const DATAGRAM_AUTO_PORTS: std::ops::RangeInclusive<u16> = 20000..=65535;

/// A connected or unconnected stream socket.
#[derive(Debug)]
pub(crate) struct Stream {
    domain: i32,
    local: Option<SocketAddress>,
    /// Whether this socket owns `local` in the bound-address table.
    owns_local: bool,
    remote: Option<SocketAddress>,
    label: Option<SocketAddress>,
    peer_label: Option<SocketAddress>,
    peer: Option<u64>,
    connected: bool,
    read_shutdown: bool,
    write_shutdown: bool,
    bytes: VecDeque<u8>,
}

/// A listening stream socket.
#[derive(Debug)]
pub(crate) struct Listener {
    local: SocketAddress,
    label: Option<SocketAddress>,
    pub(crate) pending: VecDeque<u64>,
    backlog: usize,
}

/// An Internet datagram socket.
#[derive(Debug)]
pub(crate) struct Datagram {
    domain: i32,
    local: Option<SocketAddr>,
    peer: Option<SocketAddr>,
    packets: VecDeque<(SocketAddr, Vec<u8>)>,
}

#[derive(Debug)]
struct PipeBuffer {
    bytes: VecDeque<u8>,
    capacity: usize,
    reader_open: bool,
    writer_open: bool,
}

/// One end of a pipe.
#[derive(Debug)]
pub(crate) struct PipeEnd {
    pipe: Rc<RefCell<PipeBuffer>>,
    write: bool,
}

impl PipeEnd {
    pub(crate) fn close(self) {
        let mut pipe = self.pipe.borrow_mut();
        if self.write {
            pipe.writer_open = false;
        } else {
            pipe.reader_open = false;
        }
    }
}

/// Address tables and partitions.
#[derive(Debug)]
pub(crate) struct NetTables {
    listeners: BTreeMap<AddressKey, u64>,
    bound: BTreeSet<AddressKey>,
    datagrams: BTreeMap<SocketAddr, u64>,
    partitions: BTreeSet<(AddressKey, AddressKey)>,
    next_port: u16,
}

impl Default for NetTables {
    fn default() -> Self {
        Self {
            listeners: BTreeMap::new(),
            bound: BTreeSet::new(),
            datagrams: BTreeMap::new(),
            partitions: BTreeSet::new(),
            next_port: *EPHEMERAL_PORTS.start(),
        }
    }
}

fn pair(a: &SocketAddress, b: &SocketAddress) -> (AddressKey, AddressKey) {
    let (a, b) = (AddressKey::new(a), AddressKey::new(b));
    if a <= b { (a, b) } else { (b, a) }
}

fn loopback(domain: i32) -> IpAddr {
    if domain == libc::AF_INET6 {
        Ipv6Addr::LOCALHOST.into()
    } else {
        Ipv4Addr::LOCALHOST.into()
    }
}

fn unspecified(domain: i32) -> IpAddr {
    if domain == libc::AF_INET6 {
        Ipv6Addr::UNSPECIFIED.into()
    } else {
        Ipv4Addr::UNSPECIFIED.into()
    }
}

fn unnamed() -> SocketAddress {
    SocketAddress::Unix(PathBuf::new())
}

/// Validate like the kernel encoder and normalize Unix paths.
fn normalize_address(address: &SocketAddress) -> Result<SocketAddress> {
    SockAddr::encode(address).map_err(|_| errno(libc::EINVAL))?;
    match address {
        SocketAddress::Unix(path) => normalize(path).map(SocketAddress::Unix),
        other => Ok(other.clone()),
    }
}

impl NetTables {
    pub(crate) fn unbind_listener(&mut self, id: u64, listener: &Listener) {
        self.listeners.retain(|_, owner| *owner != id);
        self.bound.remove(&AddressKey::new(&listener.local));
    }

    pub(crate) fn unbind_stream(&mut self, stream: &Stream) {
        if stream.owns_local
            && let Some(local) = &stream.local
        {
            self.bound.remove(&AddressKey::new(local));
        }
    }

    pub(crate) fn unbind_datagram(&mut self, id: u64) {
        self.datagrams.retain(|_, owner| *owner != id);
    }

    /// Forget a removed Unix socket path.
    pub(crate) fn forget_unix(&mut self, path: &Path) {
        let key = AddressKey::Unix(path.to_owned());
        self.listeners.remove(&key);
        self.bound.remove(&key);
    }

    /// Move Unix socket names along with a renamed subtree.
    pub(crate) fn rename_unix(&mut self, from: &Path, to: &Path, exchange: bool) {
        let rename = |key: AddressKey| -> Option<AddressKey> {
            match key {
                AddressKey::Unix(path) if path.starts_with(from) => Some(AddressKey::Unix(
                    to.join(path.strip_prefix(from).expect("prefix")),
                )),
                AddressKey::Unix(path) if path.starts_with(to) => exchange
                    .then(|| AddressKey::Unix(from.join(path.strip_prefix(to).expect("prefix")))),
                other => Some(other),
            }
        };
        self.listeners = std::mem::take(&mut self.listeners)
            .into_iter()
            .filter_map(|(key, id)| rename(key).map(|key| (key, id)))
            .collect();
        self.bound = std::mem::take(&mut self.bound)
            .into_iter()
            .filter_map(rename)
            .collect();
    }

    fn ephemeral(&mut self, ip: IpAddr) -> Result<SocketAddr> {
        let span = EPHEMERAL_PORTS.len();
        for _ in 0..span {
            let port = self.next_port;
            self.next_port = if port == *EPHEMERAL_PORTS.end() {
                *EPHEMERAL_PORTS.start()
            } else {
                port + 1
            };
            let address = SocketAddr::new(ip, port);
            if !self.bound.contains(&AddressKey::Inet(address)) {
                return Ok(address);
            }
        }
        Err(errno(libc::EADDRNOTAVAIL))
    }

    /// Find the listener for a connect to `address`, falling back to a
    /// wildcard listener on the same port.
    fn listener_for(&self, address: &SocketAddress) -> Option<u64> {
        let key = AddressKey::new(address);
        if let Some(id) = self.listeners.get(&key) {
            return Some(*id);
        }
        let inet = address.as_inet()?;
        let wildcard = SocketAddr::new(unspecified(address.family()), inet.port());
        self.listeners.get(&AddressKey::Inet(wildcard)).copied()
    }

    fn partitioned(&self, a: Option<&SocketAddress>, b: Option<&SocketAddress>) -> bool {
        match (a, b) {
            (Some(a), Some(b)) => self.partitions.contains(&pair(a, b)),
            _ => false,
        }
    }
}

impl State {
    fn stream(&self, id: u64) -> Result<&Stream> {
        match self.resources.get(&id) {
            Some(Resource::Stream(stream)) => Ok(stream),
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    fn stream_mut(&mut self, id: u64) -> Result<&mut Stream> {
        match self.resources.get_mut(&id) {
            Some(Resource::Stream(stream)) => Ok(stream),
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    fn stream_partitioned(&self, id: u64) -> bool {
        self.stream(id).is_ok_and(|s| {
            self.net
                .partitioned(s.label.as_ref(), s.peer_label.as_ref())
        })
    }

    /// `socket(domain, ty, 0)`.
    pub(crate) fn socket(
        &mut self,
        domain: i32,
        ty: i32,
        label: Option<&SocketAddress>,
    ) -> Result<u64> {
        if ![libc::AF_INET, libc::AF_INET6, libc::AF_UNIX].contains(&domain) {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        match ty & 0xf {
            libc::SOCK_STREAM => self.insert(Resource::Stream(Stream {
                domain,
                local: None,
                owns_local: false,
                remote: None,
                label: label.cloned(),
                peer_label: None,
                peer: None,
                connected: false,
                read_shutdown: false,
                write_shutdown: false,
                bytes: VecDeque::new(),
            })),
            libc::SOCK_DGRAM if domain != libc::AF_UNIX => {
                self.insert(Resource::Datagram(Datagram {
                    domain,
                    local: None,
                    peer: None,
                    packets: VecDeque::new(),
                }))
            }
            libc::SOCK_DGRAM => Err(errno(libc::EPROTONOSUPPORT)),
            _ => Err(errno(libc::ESOCKTNOSUPPORT)),
        }
    }

    /// `bind(id, address)`.
    pub(crate) fn bind(&mut self, id: u64, address: &SocketAddress) -> Result<()> {
        let address = normalize_address(address)?;
        match self.resources.get(&id) {
            Some(Resource::Datagram(datagram)) => {
                let Some(mut inet) = address.as_inet() else {
                    return Err(errno(libc::EINVAL));
                };
                if address.family() != datagram.domain || datagram.local.is_some() {
                    return Err(errno(libc::EINVAL));
                }
                if inet.port() == 0 {
                    let port = DATAGRAM_AUTO_PORTS
                        .clone()
                        .find(|port| {
                            !self
                                .net
                                .datagrams
                                .contains_key(&SocketAddr::new(inet.ip(), *port))
                        })
                        .ok_or_else(|| errno(libc::EADDRINUSE))?;
                    inet.set_port(port);
                }
                if self.net.datagrams.contains_key(&inet) {
                    return Err(errno(libc::EADDRINUSE));
                }
                self.net.datagrams.insert(inet, id);
                if let Some(Resource::Datagram(datagram)) = self.resources.get_mut(&id) {
                    datagram.local = Some(inet);
                }
                Ok(())
            }
            Some(Resource::Stream(stream)) => {
                if address.family() != stream.domain || stream.local.is_some() || stream.connected {
                    return Err(errno(libc::EINVAL));
                }
                let address = match &address {
                    SocketAddress::Inet(inet) if inet.port() == 0 => {
                        SocketAddress::Inet(self.net.ephemeral(inet.ip())?)
                    }
                    other => other.clone(),
                };
                let key = AddressKey::new(&address);
                if self.net.bound.contains(&key) {
                    return Err(errno(libc::EADDRINUSE));
                }
                if let SocketAddress::Unix(path) = &address {
                    if self.fs.paths.contains_key(path) {
                        return Err(errno(libc::EADDRINUSE));
                    }
                    let parent = path.parent().ok_or_else(|| errno(libc::ENOENT))?;
                    match self.fs.paths.get(parent).map(|node| node.borrow().kind()) {
                        None => return Err(errno(libc::ENOENT)),
                        Some(libc::S_IFDIR) => {}
                        Some(_) => return Err(errno(libc::ENOTDIR)),
                    }
                    let node = self.fs.node(libc::S_IFSOCK | 0o755);
                    self.fs.paths.insert(path.clone(), node);
                }
                self.net.bound.insert(key);
                let stream = self.stream_mut(id)?;
                stream.local = Some(address);
                stream.owns_local = true;
                Ok(())
            }
            Some(Resource::Listener(_)) => Err(errno(libc::EINVAL)),
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    /// `listen(id, backlog)`.
    pub(crate) fn listen(&mut self, id: u64, backlog: i32) -> Result<()> {
        if let Some(Resource::Listener(listener)) = self.resources.get_mut(&id) {
            listener.backlog = backlog.clamp(1, 4096) as usize;
            return Ok(());
        }
        let stream = match self.resources.get(&id) {
            Some(Resource::Stream(stream)) => stream,
            Some(Resource::Datagram(_)) => return Err(errno(libc::EOPNOTSUPP)),
            Some(_) => return Err(errno(libc::ENOTSOCK)),
            None => return Err(errno(libc::EBADF)),
        };
        if stream.connected {
            return Err(errno(libc::EINVAL));
        }
        if stream.local.is_none() {
            if stream.domain == libc::AF_UNIX {
                return Err(errno(libc::EINVAL));
            }
            let address = SocketAddr::new(unspecified(stream.domain), 0);
            self.bind(id, &address.into())?;
        }
        let Some(Resource::Stream(stream)) = self.resources.remove(&id) else {
            unreachable!("checked above");
        };
        let local = stream.local.expect("bound above");
        let label = stream.label.or_else(|| Some(local.clone()));
        self.net.listeners.insert(AddressKey::new(&local), id);
        self.resources.insert(
            id,
            Resource::Listener(Listener {
                local,
                label,
                pending: VecDeque::new(),
                backlog: backlog.clamp(1, 4096) as usize,
            }),
        );
        Ok(())
    }

    /// `connect(id, address)`. Returns `EAGAIN` while partitioned or while
    /// the listener's backlog is full.
    pub(crate) fn connect(&mut self, id: u64, address: &SocketAddress) -> Result<()> {
        let address = normalize_address(address)?;
        if let Some(Resource::Datagram(datagram)) = self.resources.get_mut(&id) {
            let inet = address.as_inet().ok_or_else(|| errno(libc::EAFNOSUPPORT))?;
            if address.family() != datagram.domain {
                return Err(errno(libc::EAFNOSUPPORT));
            }
            datagram.peer = Some(inet);
            return Ok(());
        }
        let stream = match self.resources.get(&id) {
            Some(Resource::Stream(stream)) => stream,
            Some(Resource::Listener(_)) => return Err(errno(libc::EISCONN)),
            Some(_) => return Err(errno(libc::ENOTSOCK)),
            None => return Err(errno(libc::EBADF)),
        };
        if stream.domain != address.family() {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        if stream.connected {
            return Err(errno(libc::EISCONN));
        }
        let listener_id = self.net.listener_for(&address);
        let listener = listener_id.and_then(|id| match self.resources.get(&id) {
            Some(Resource::Listener(listener)) => Some(listener),
            _ => None,
        });
        let server_label = listener.map_or(Some(&address), |l| l.label.as_ref());
        if self.net.partitioned(stream.label.as_ref(), server_label) {
            self.record(EventKind::Blocked(SyncOp::Connect), id, 0);
            return Err(errno(libc::EAGAIN));
        }
        let (Some(listener_id), Some(listener)) = (listener_id, listener) else {
            return Err(errno(libc::ECONNREFUSED));
        };
        if listener.pending.len() >= listener.backlog {
            return Err(errno(libc::EAGAIN));
        }
        let (server_local, server_label) = (listener.local.clone(), listener.label.clone());
        let domain = stream.domain;
        let client_label = stream.label.clone();
        let client_local = match &stream.local {
            Some(local) => local.clone(),
            None if domain == libc::AF_UNIX => unnamed(),
            None => {
                let local = SocketAddress::Inet(self.net.ephemeral(loopback(domain))?);
                self.net.bound.insert(AddressKey::new(&local));
                let stream = self.stream_mut(id)?;
                stream.local = Some(local.clone());
                stream.owns_local = true;
                local
            }
        };
        let server = self.insert(Resource::Stream(Stream {
            domain,
            local: Some(server_local),
            owns_local: false,
            remote: Some(client_local),
            label: server_label.clone(),
            peer_label: client_label,
            peer: Some(id),
            connected: true,
            read_shutdown: false,
            write_shutdown: false,
            bytes: VecDeque::new(),
        }))?;
        let stream = self.stream_mut(id)?;
        stream.peer = Some(server);
        stream.connected = true;
        stream.remote = Some(address);
        stream.peer_label = server_label;
        if let Some(Resource::Listener(listener)) = self.resources.get_mut(&listener_id) {
            listener.pending.push_back(server);
        }
        Ok(())
    }

    /// `accept(id)`: the id of the oldest pending connection.
    pub(crate) fn accept(&mut self, id: u64) -> Result<u64> {
        let listener = match self.resources.get_mut(&id) {
            Some(Resource::Listener(listener)) => listener,
            Some(_) => return Err(errno(libc::EINVAL)),
            None => return Err(errno(libc::EBADF)),
        };
        listener
            .pending
            .pop_front()
            .ok_or_else(|| errno(libc::EAGAIN))
    }

    /// The peer address of an accepted or connected stream.
    pub(crate) fn peer_addr(&self, id: u64) -> Result<SocketAddress> {
        match self.resources.get(&id) {
            Some(Resource::Stream(stream)) if stream.connected => {
                Ok(stream.remote.clone().unwrap_or_else(unnamed))
            }
            Some(Resource::Datagram(Datagram {
                peer: Some(peer), ..
            })) => Ok((*peer).into()),
            Some(Resource::Stream(_) | Resource::Datagram(_) | Resource::Listener(_)) => {
                Err(errno(libc::ENOTCONN))
            }
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    pub(crate) fn local_addr(&self, id: u64) -> Result<SocketAddress> {
        match self.resources.get(&id) {
            Some(Resource::Stream(stream)) => Ok(stream.local.clone().unwrap_or_else(|| {
                if stream.domain == libc::AF_UNIX {
                    unnamed()
                } else {
                    SocketAddr::new(unspecified(stream.domain), 0).into()
                }
            })),
            Some(Resource::Listener(listener)) => Ok(listener.local.clone()),
            Some(Resource::Datagram(datagram)) => Ok(datagram
                .local
                .unwrap_or_else(|| SocketAddr::new(unspecified(datagram.domain), 0))
                .into()),
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    pub(crate) fn set_nodelay(&self, id: u64) -> Result<()> {
        match self.resources.get(&id) {
            Some(Resource::Stream(Stream {
                domain: libc::AF_UNIX,
                ..
            })) => Err(errno(libc::EOPNOTSUPP)),
            Some(Resource::Stream(_) | Resource::Listener(_)) => Ok(()),
            Some(Resource::Datagram(_)) => Err(errno(libc::ENOPROTOOPT)),
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    /// Send on a stream (into the peer's buffer) or a connected datagram.
    pub(crate) fn send(&mut self, id: u64, buf: &[u8], limit: usize) -> Result<usize> {
        if let Some(Resource::Datagram(datagram)) = self.resources.get(&id) {
            let peer = datagram.peer.ok_or_else(|| errno(libc::EDESTADDRREQ))?;
            return self.send_to(id, buf, peer, limit);
        }
        let capacity = self.knobs.stream_capacity;
        let max_chunk = self.knobs.max_chunk;
        let stream = self.stream(id)?;
        if stream.write_shutdown {
            return Err(errno(libc::EPIPE));
        }
        let peer = stream.peer.ok_or_else(|| {
            errno(if stream.connected {
                libc::EPIPE
            } else {
                libc::ENOTCONN
            })
        })?;
        if !buf.is_empty() && self.stream_partitioned(id) {
            self.record(EventKind::Blocked(SyncOp::Send), id, buf.len() as i64);
            return Err(errno(libc::EAGAIN));
        }
        let Ok(output) = self.stream_mut(peer) else {
            return Err(errno(libc::EPIPE));
        };
        if output.read_shutdown {
            return Err(errno(libc::EPIPE));
        }
        let count = buf
            .len()
            .min(capacity.saturating_sub(output.bytes.len()))
            .min(max_chunk)
            .min(limit);
        if count == 0 && !buf.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output
            .bytes
            .try_reserve(count)
            .map_err(|_| errno(libc::ENOMEM))?;
        output.bytes.extend(&buf[..count]);
        Ok(count)
    }

    /// Receive from a stream, draining queued bytes before end-of-file, or
    /// one datagram.
    pub(crate) fn recv(
        &mut self,
        id: u64,
        buf: &mut [u8],
        limit: usize,
        peek: bool,
    ) -> Result<usize> {
        if let Some(Resource::Datagram(_)) = self.resources.get(&id) {
            return self.recv_from(id, buf, peek).map(|(n, _)| n);
        }
        let max_chunk = self.knobs.max_chunk;
        let stream = self.stream(id)?;
        if !stream.connected {
            return Err(errno(libc::ENOTCONN));
        }
        let peer_open = stream
            .peer
            .is_some_and(|peer| self.stream(peer).is_ok_and(|p| !p.write_shutdown));
        let closed = !peer_open || stream.read_shutdown;
        let input = &mut self.stream_mut(id)?.bytes;
        if input.is_empty() && !closed && !buf.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        let count = buf.len().min(input.len()).min(max_chunk).min(limit);
        if peek {
            for (slot, byte) in buf.iter_mut().zip(input.iter()).take(count) {
                *slot = *byte;
            }
        } else {
            input.read_exact(&mut buf[..count]).expect("count <= len");
        }
        Ok(count)
    }

    pub(crate) fn shutdown(&mut self, id: u64, how: Shutdown) -> Result<()> {
        let stream = match self.resources.get_mut(&id) {
            Some(Resource::Stream(stream)) => stream,
            Some(Resource::Listener(_) | Resource::Datagram(_)) => {
                return Err(errno(libc::ENOTCONN));
            }
            Some(_) => return Err(errno(libc::ENOTSOCK)),
            None => return Err(errno(libc::EBADF)),
        };
        if !stream.connected {
            return Err(errno(libc::ENOTCONN));
        }
        if how != Shutdown::Write {
            stream.read_shutdown = true;
        }
        if how != Shutdown::Read {
            stream.write_shutdown = true;
        }
        Ok(())
    }

    /// The ready `POLL*` events among `interest` (plus `POLLHUP` and
    /// `POLLERR`), or 0. `EBADF` for `O_PATH` files, as io_uring reports.
    pub(crate) fn ready(&self, id: u64, interest: u32) -> Result<u32> {
        let flags = match self.resources.get(&id) {
            Some(Resource::File(file)) => {
                if file.flags & libc::O_PATH != 0 {
                    return Err(errno(libc::EBADF));
                }
                libc::POLLIN | libc::POLLOUT | libc::POLLRDNORM | libc::POLLWRNORM
            }
            Some(Resource::Datagram(datagram)) => {
                libc::POLLOUT
                    | if datagram.packets.is_empty() {
                        0
                    } else {
                        libc::POLLIN
                    }
            }
            Some(Resource::Listener(listener)) => {
                if listener.pending.is_empty() {
                    0
                } else {
                    libc::POLLIN
                }
            }
            Some(Resource::Pipe(end)) => {
                let pipe = end.pipe.borrow();
                if end.write {
                    (if pipe.bytes.len() < pipe.capacity {
                        libc::POLLOUT
                    } else {
                        0
                    }) | if pipe.reader_open { 0 } else { libc::POLLERR }
                } else {
                    (if pipe.bytes.is_empty() {
                        0
                    } else {
                        libc::POLLIN
                    }) | if pipe.writer_open { 0 } else { libc::POLLHUP }
                }
            }
            Some(Resource::Stream(stream)) => {
                let remote = stream.peer.and_then(|peer| self.stream(peer).ok());
                let mut flags = if stream.bytes.is_empty() {
                    0
                } else {
                    libc::POLLIN
                };
                if let Some(remote) = remote {
                    if remote.bytes.len() < self.knobs.stream_capacity
                        && !self.stream_partitioned(id)
                    {
                        flags |= libc::POLLOUT;
                    }
                    if remote.read_shutdown && remote.write_shutdown {
                        flags |= libc::POLLHUP;
                    }
                } else {
                    flags |= libc::POLLHUP | libc::POLLIN | libc::POLLOUT;
                }
                if stream.read_shutdown || remote.is_none_or(|r| r.write_shutdown) {
                    flags |= libc::POLLIN | libc::POLLRDHUP;
                }
                if stream.read_shutdown && stream.write_shutdown {
                    flags |= libc::POLLHUP;
                }
                flags
            }
            None => return Err(errno(libc::EBADF)),
        };
        Ok(flags as u32 & (interest | (libc::POLLHUP | libc::POLLERR) as u32))
    }

    pub(crate) fn pipe(&mut self, capacity: usize) -> Result<(u64, u64)> {
        if capacity == 0 || capacity > MAX_ALLOCATION {
            return Err(errno(libc::EINVAL));
        }
        let pipe = Rc::new(RefCell::new(PipeBuffer {
            bytes: VecDeque::new(),
            capacity,
            reader_open: true,
            writer_open: true,
        }));
        let read = self.insert(Resource::Pipe(PipeEnd {
            pipe: pipe.clone(),
            write: false,
        }))?;
        let write = self.insert(Resource::Pipe(PipeEnd { pipe, write: true }))?;
        Ok((read, write))
    }

    fn pipe_end(&self, id: u64, write: bool) -> Result<Rc<RefCell<PipeBuffer>>> {
        match self.resources.get(&id) {
            Some(Resource::Pipe(end)) if end.write == write => Ok(end.pipe.clone()),
            _ => Err(errno(libc::EBADF)),
        }
    }

    pub(crate) fn is_pipe(&self, id: u64) -> bool {
        matches!(self.resources.get(&id), Some(Resource::Pipe(_)))
    }

    /// Write to a pipe; writes of at most `PIPE_BUF` bytes are atomic.
    pub(crate) fn pipe_write(&mut self, id: u64, buf: &[u8], limit: usize) -> Result<usize> {
        let pipe = self.pipe_end(id, true)?;
        let mut pipe = pipe.borrow_mut();
        if !buf.is_empty() && !pipe.reader_open {
            return Err(errno(libc::EPIPE));
        }
        let space = pipe.capacity.saturating_sub(pipe.bytes.len());
        if buf.len() <= libc::PIPE_BUF && buf.len() > space {
            return Err(errno(libc::EAGAIN));
        }
        let count = buf.len().min(space).min(limit);
        if count == 0 && !buf.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        pipe.bytes
            .try_reserve(count)
            .map_err(|_| errno(libc::ENOMEM))?;
        pipe.bytes.extend(&buf[..count]);
        Ok(count)
    }

    /// Read from a pipe; end-of-file only once the writer is closed.
    pub(crate) fn pipe_read(&mut self, id: u64, buf: &mut [u8], limit: usize) -> Result<usize> {
        let pipe = self.pipe_end(id, false)?;
        let mut pipe = pipe.borrow_mut();
        let count = buf.len().min(pipe.bytes.len()).min(limit);
        if count == 0 && !buf.is_empty() && pipe.writer_open {
            return Err(errno(libc::EAGAIN));
        }
        pipe.bytes
            .read_exact(&mut buf[..count])
            .expect("count <= len");
        Ok(count)
    }

    fn splice(&mut self, pipe_id: u64, socket: u64, count: usize, limit: usize) -> Result<usize> {
        let count = count.min(limit);
        let pipe = self.pipe_end(pipe_id, false)?;
        let mut input = pipe.borrow_mut();
        if count != 0 && input.bytes.is_empty() {
            return if input.writer_open {
                Err(errno(libc::EAGAIN))
            } else {
                Ok(0)
            };
        }
        let count = count.min(input.bytes.len());
        let sent = self.send(socket, &input.bytes.make_contiguous()[..count], usize::MAX)?;
        input.bytes.drain(..sent);
        Ok(sent)
    }

    fn datagram(&mut self, id: u64) -> Result<&mut Datagram> {
        match self.resources.get_mut(&id) {
            Some(Resource::Datagram(datagram)) => Ok(datagram),
            Some(_) => Err(errno(libc::ENOTSOCK)),
            None => Err(errno(libc::EBADF)),
        }
    }

    /// Send one datagram, auto-binding an unbound sender.
    pub(crate) fn send_to(
        &mut self,
        id: u64,
        buf: &[u8],
        target: SocketAddr,
        limit: usize,
    ) -> Result<usize> {
        let datagram = self.datagram(id)?;
        if (datagram.domain == libc::AF_INET) != target.is_ipv4() {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        if buf.len() > MAX_DATAGRAM {
            return Err(errno(libc::EMSGSIZE));
        }
        let source = match datagram.local {
            Some(local) => local,
            None => {
                let domain = datagram.domain;
                self.bind(id, &SocketAddr::new(loopback(domain), 0).into())?;
                self.datagram(id)?.local.expect("bound above")
            }
        };
        let target_id = *self
            .net
            .datagrams
            .get(&target)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        let receiver = self.datagram(target_id)?;
        let count = buf.len().min(limit);
        if receiver.peer.is_none_or(|peer| peer == source) {
            if receiver.packets.len() >= DATAGRAM_QUEUE {
                return Err(errno(libc::EAGAIN));
            }
            let mut packet = bounded_bytes(count)?;
            packet.copy_from_slice(&buf[..count]);
            receiver.packets.push_back((source, packet));
        }
        Ok(count)
    }

    /// Receive one datagram, discarding any part that does not fit.
    pub(crate) fn recv_from(
        &mut self,
        id: u64,
        buf: &mut [u8],
        peek: bool,
    ) -> Result<(usize, SocketAddr)> {
        let datagram = self.datagram(id)?;
        let (source, packet) = datagram
            .packets
            .front()
            .ok_or_else(|| errno(libc::EAGAIN))?;
        let source = *source;
        let count = buf.len().min(packet.len());
        buf[..count].copy_from_slice(&packet[..count]);
        if !peek {
            datagram.packets.pop_front();
        }
        Ok((count, source))
    }
}

/// Network fixtures and controls.
impl World {
    /// Create an unlabeled socket (`SOCK_STREAM`, or Internet `SOCK_DGRAM`).
    pub fn socket(&self, domain: i32, ty: i32) -> Result<SimFd> {
        self.labeled_socket(domain, ty, None)
    }

    pub(crate) fn labeled_socket(
        &self,
        domain: i32,
        ty: i32,
        label: Option<&SocketAddress>,
    ) -> Result<SimFd> {
        let mut state = self.state();
        state.sync_limit(SyncOp::Socket)?;
        let id = state.socket(domain, ty, label)?;
        drop(state);
        Ok(self.handle(id))
    }

    /// Create a stream listener bound to `address` (port 0 picks a free
    /// port; Unix addresses create a socket file).
    pub fn listen(&self, address: impl Into<SocketAddress>) -> Result<SimFd> {
        self.labeled_listen(&address.into(), None)
    }

    pub(crate) fn labeled_listen(
        &self,
        address: &SocketAddress,
        label: Option<&SocketAddress>,
    ) -> Result<SimFd> {
        let fd = self.labeled_socket(address.family(), libc::SOCK_STREAM, label)?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Bind)?;
        state.bind(fd.id(), address)?;
        state.sync_limit(SyncOp::Listen)?;
        state.listen(fd.id(), 4096)?;
        drop(state);
        Ok(fd)
    }

    /// Create an unlabeled stream connected to `address`. Fails instead of
    /// waiting (`EAGAIN` when partitioned, `ECONNREFUSED` with no listener).
    pub fn connect(&self, address: impl Into<SocketAddress>) -> Result<SimFd> {
        self.labeled_connect(&address.into(), None)
    }

    pub(crate) fn labeled_connect(
        &self,
        address: &SocketAddress,
        label: Option<&SocketAddress>,
    ) -> Result<SimFd> {
        let fd = self.labeled_socket(address.family(), libc::SOCK_STREAM, label)?;
        fd.connect(address)?;
        Ok(fd)
    }

    /// Create two connected, unlabeled Unix streams.
    pub fn socket_pair(&self) -> Result<(SimFd, SimFd)> {
        let a = self.socket(libc::AF_UNIX, libc::SOCK_STREAM)?;
        let b = self.socket(libc::AF_UNIX, libc::SOCK_STREAM)?;
        let mut state = self.state();
        for (id, peer) in [(a.id(), b.id()), (b.id(), a.id())] {
            let stream = state.stream_mut(id)?;
            stream.peer = Some(peer);
            stream.connected = true;
        }
        drop(state);
        Ok((a, b))
    }

    /// Create a pipe with `capacity` bytes of buffer, returning
    /// `(read end, write end)`. Fails with `EINVAL` for 0 or above
    /// [`MAX_ALLOCATION`].
    pub fn pipe(&self, capacity: usize) -> Result<(SimFd, SimFd)> {
        let (read, write) = self.state().pipe(capacity)?;
        Ok((self.handle(read), self.handle(write)))
    }

    /// Create an Internet datagram socket bound to `address` (port 0 picks
    /// the lowest free port from 20000).
    pub fn bind_datagram(&self, address: SocketAddr) -> Result<SimFd> {
        let fd = self.socket(SocketAddress::from(address).family(), libc::SOCK_DGRAM)?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Bind)?;
        state.bind(fd.id(), &address.into())?;
        drop(state);
        Ok(fd)
    }

    /// Blackhole traffic between endpoint labels `a` and `b`, in both
    /// directions, until [`heal`](Self::heal).
    pub fn partition(&self, a: &SocketAddress, b: &SocketAddress) {
        let mut state = self.state();
        state.net.partitions.insert(pair(a, b));
        state.record(EventKind::Partition, 0, 0);
    }

    /// Remove a partition between `a` and `b` (in either order). Queued
    /// bytes are kept.
    pub fn heal(&self, a: &SocketAddress, b: &SocketAddress) {
        let mut state = self.state();
        state.net.partitions.remove(&pair(a, b));
        state.record(EventKind::Heal, 0, 0);
    }

    /// Set the endpoint labels of an established stream and the opposite
    /// labels on its peer (for streams created outside a labeled backend).
    pub fn label_stream(
        &self,
        fd: &SimFd,
        local: SocketAddress,
        remote: SocketAddress,
    ) -> Result<()> {
        let id = self.id_of(fd)?;
        let mut state = self.state();
        let stream = state.stream_mut(id)?;
        stream.label = Some(local.clone());
        stream.peer_label = Some(remote.clone());
        if let Some(peer) = stream.peer
            && let Ok(peer) = state.stream_mut(peer)
        {
            peer.label = Some(remote);
            peer.peer_label = Some(local);
        }
        Ok(())
    }

    /// Break an established stream while both handles stay open. Each side
    /// can still read bytes it already received, then sees end-of-file;
    /// sends fail with `EPIPE`.
    pub fn disconnect(&self, fd: &SimFd) -> Result<()> {
        let id = self.id_of(fd)?;
        let mut state = self.state();
        let other = state.stream_mut(id)?.peer.take();
        if let Some(other) = other
            && let Ok(peer) = state.stream_mut(other)
        {
            peer.peer = None;
        }
        state.record(EventKind::Disconnect, id, 0);
        Ok(())
    }
}

/// Socket, pipe and datagram operations.
impl SimFd {
    /// Nonblocking `send`. Honors partitions, the peer's buffer capacity,
    /// [`World::set_max_chunk`] and synchronous [`SyncOp::Send`] faults.
    pub fn send(&self, buf: &[u8]) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::Send)?;
        let n = state.send(self.id(), buf, limit)?;
        state.record(EventKind::Call(SyncOp::Send), self.id(), n as i64);
        Ok(n)
    }

    /// Nonblocking `recv`: queued bytes, then 0 at end-of-file, else
    /// `EAGAIN`. With `peek`, bytes are not consumed (`MSG_PEEK`).
    pub fn recv(&self, buf: &mut [u8], peek: bool) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::Recv)?;
        let n = state.recv(self.id(), buf, limit, peek)?;
        state.record(EventKind::Call(SyncOp::Recv), self.id(), n as i64);
        Ok(n)
    }

    /// `shutdown(how)`; the peer can drain queued bytes before end-of-file.
    pub fn shutdown(&self, how: Shutdown) -> Result<()> {
        self.state().shutdown(self.id(), how)
    }

    /// Connect a stream (failing with `EAGAIN` instead of waiting) or set a
    /// datagram socket's default peer.
    pub fn connect(&self, address: &SocketAddress) -> Result<()> {
        let mut state = self.state();
        state.sync_limit(SyncOp::Connect)?;
        state.connect(self.id(), address)?;
        state.record(EventKind::Call(SyncOp::Connect), self.id(), 0);
        Ok(())
    }

    /// Accept the oldest pending connection (`EAGAIN` if none).
    pub fn accept(&self) -> Result<SimFd> {
        let mut state = self.state();
        state.sync_limit(SyncOp::Accept)?;
        let id = state.accept(self.id())?;
        state.record(EventKind::Call(SyncOp::Accept), self.id(), id as i64);
        drop(state);
        Ok(self.world().handle(id))
    }

    /// The ready `POLL*` events among `events` (always including `POLLHUP`
    /// and `POLLERR`), or 0, without waiting. `O_PATH` files report
    /// `EBADF`.
    pub fn poll(&self, events: u32) -> Result<u32> {
        self.state().ready(self.id(), events)
    }

    /// `getsockname`.
    pub fn local_addr(&self) -> Result<SocketAddress> {
        self.state().local_addr(self.id())
    }

    /// `getpeername`.
    pub fn peer_addr(&self) -> Result<SocketAddress> {
        self.state().peer_addr(self.id())
    }

    /// Write to a pipe (`EAGAIN` when full; writes up to `PIPE_BUF` bytes
    /// are all or nothing; `EPIPE` once the reader is closed).
    pub fn pipe_write(&self, buf: &[u8]) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::PipeWrite)?;
        state.pipe_write(self.id(), buf, limit)
    }

    /// Read from a pipe (`EAGAIN` when empty, 0 once the writer is closed).
    pub fn pipe_read(&self, buf: &mut [u8]) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::PipeRead)?;
        state.pipe_read(self.id(), buf, limit)
    }

    /// Move up to `count` bytes from this pipe's read end to a stream,
    /// consuming only what was sent.
    pub fn splice(&self, socket: &SimFd, count: usize) -> Result<usize> {
        let mut state = self.state();
        let socket = self.world().id_of(socket)?;
        let limit = state.sync_limit(SyncOp::Splice)?;
        let limit = limit.min(state.sync_limit(SyncOp::Send)?);
        state.splice(self.id(), socket, count, limit)
    }

    /// Send one datagram to `target`.
    pub fn send_to(&self, buf: &[u8], target: SocketAddr) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::SendTo)?;
        let n = state.send_to(self.id(), buf, target, limit)?;
        state.record(EventKind::Call(SyncOp::SendTo), self.id(), n as i64);
        Ok(n)
    }

    /// Receive one datagram and its source address.
    pub fn recv_from(&self, buf: &mut [u8]) -> Result<(usize, SocketAddr)> {
        let mut state = self.state();
        state.sync_limit(SyncOp::RecvFrom)?;
        let (n, source) = state.recv_from(self.id(), buf, false)?;
        state.record(EventKind::Call(SyncOp::RecvFrom), self.id(), n as i64);
        Ok((n, source))
    }

    /// Whether this is an established stream with an empty receive buffer
    /// whose peer is open in both directions: safe to reuse from a pool.
    pub fn idle_healthy(&self) -> bool {
        let state = self.state();
        state.stream(self.id()).is_ok_and(|s| {
            s.bytes.is_empty()
                && !s.read_shutdown
                && !s.write_shutdown
                && s.peer
                    .is_some_and(|peer| state.stream(peer).is_ok_and(|p| !p.write_shutdown))
        })
    }

    /// Whether the peer is gone or shut down in both directions.
    pub fn peer_disconnected(&self) -> bool {
        let state = self.state();
        !state.stream(self.id()).is_ok_and(|s| {
            s.peer.is_some_and(|peer| {
                state
                    .stream(peer)
                    .is_ok_and(|p| !(p.read_shutdown && p.write_shutdown))
            })
        })
    }

    /// Whether reads reach end-of-file once queued bytes are drained.
    pub fn peer_read_closed(&self) -> bool {
        let state = self.state();
        !state.stream(self.id()).is_ok_and(|s| {
            s.peer
                .is_some_and(|peer| state.stream(peer).is_ok_and(|p| !p.write_shutdown))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fault::{Fault, FaultTarget};

    fn code<T: std::fmt::Debug>(result: Result<T>) -> i32 {
        result.unwrap_err().raw_os_error().unwrap()
    }

    fn inet(text: &str) -> SocketAddress {
        SocketAddress::Inet(text.parse().unwrap())
    }

    const IN: u32 = libc::POLLIN as u32;
    const OUT: u32 = libc::POLLOUT as u32;
    const HUP: u32 = libc::POLLHUP as u32;
    const ERR: u32 = libc::POLLERR as u32;

    #[test]
    fn partition_stalls_and_heal_resumes_without_loss() {
        let world = World::new();
        let (a, b) = (inet("127.0.0.1:101"), inet("127.0.0.1:102"));
        let (local, peer) = world.socket_pair().unwrap();
        world.label_stream(&local, a.clone(), b.clone()).unwrap();
        local.send(b"before").unwrap();
        world.partition(&a, &b);
        assert_eq!(code(local.send(b"x")), libc::EAGAIN);
        assert_eq!(code(peer.send(b"x")), libc::EAGAIN);
        assert_eq!(local.poll(OUT).unwrap(), 0);
        let mut buf = [0; 16];
        assert_eq!(
            peer.recv(&mut buf, false).unwrap(),
            6,
            "received bytes stay readable"
        );
        let listener = world.listen(b.clone()).unwrap();
        let client = world
            .labeled_socket(libc::AF_INET, libc::SOCK_STREAM, Some(&a))
            .unwrap();
        assert_eq!(code(client.connect(&b)), libc::EAGAIN);
        world.heal(&b, &a);
        assert_eq!(local.send(b"x").unwrap(), 1);
        client.connect(&b).unwrap();
        let server = listener.accept().unwrap();
        assert_eq!(server.peer_addr().unwrap(), client.local_addr().unwrap());
        assert_eq!(client.peer_addr().unwrap(), b);
        world.disconnect(&local).unwrap();
        assert_eq!(peer.recv(&mut buf, false).unwrap(), 1);
        assert_eq!(peer.recv(&mut buf, false).unwrap(), 0);
        assert_eq!(code(local.send(b"x")), libc::EPIPE);
        let kinds: Vec<_> = world.trace().iter().map(|e| e.kind).collect();
        assert!(kinds.contains(&EventKind::Blocked(SyncOp::Connect)));
        assert!(kinds.contains(&EventKind::Blocked(SyncOp::Send)));
    }

    #[test]
    fn socket_families_half_close_and_renamed_listener_descendants() {
        let world = World::new();
        let address = inet("127.0.0.1:8080");
        let listener = world.listen(address.clone()).unwrap();
        let wrong = world.socket(libc::AF_UNIX, libc::SOCK_STREAM).unwrap();
        assert_eq!(code(wrong.connect(&address)), libc::EAFNOSUPPORT);
        let client = world.connect(address.clone()).unwrap();
        assert_eq!(code(client.connect(&address)), libc::EISCONN);
        let server = listener.accept().unwrap();
        assert_eq!(server.local_addr().unwrap(), address);
        client.send(b"hello").unwrap();
        client.shutdown(Shutdown::Write).unwrap();
        assert_eq!(code(client.send(b"x")), libc::EPIPE);
        assert_eq!(
            server.poll(libc::POLLRDHUP as u32).unwrap(),
            libc::POLLRDHUP as u32
        );
        let mut bytes = [0; 8];
        assert_eq!(server.recv(&mut bytes, true).unwrap(), 5);
        assert_eq!(server.recv(&mut bytes, false).unwrap(), 5);
        assert_eq!(server.recv(&mut bytes, false).unwrap(), 0);
        server.send(b"reply").unwrap();
        assert_eq!(client.recv(&mut bytes, false).unwrap(), 5);
        assert!(!server.idle_healthy());
        assert!(!server.peer_disconnected());
        assert!(server.peer_read_closed());
        world.create_dir_all("/old/child").unwrap();
        let unix = world
            .listen(SocketAddress::Unix("/old/child/socket".into()))
            .unwrap();
        world.rename("/old", "/new", 0).unwrap();
        assert!(
            world
                .connect(SocketAddress::Unix("/old/child/socket".into()))
                .is_err()
        );
        assert!(
            world
                .connect(SocketAddress::Unix("/new/child/socket".into()))
                .is_ok()
        );
        drop(unix);
        assert_eq!(
            code(world.connect(SocketAddress::Unix("/new/child/socket".into()))),
            libc::ECONNREFUSED
        );
    }

    #[test]
    fn relative_unix_listener_lookup_unlink_and_rebind() {
        let world = World::new();
        let relative = SocketAddress::Unix("sock".into());
        let absolute = SocketAddress::Unix("/sock".into());
        let listener = world.listen(relative.clone()).unwrap();
        assert_eq!(
            world.metadata("sock").unwrap(),
            world.metadata("/sock").unwrap()
        );
        assert_eq!(code(world.listen(absolute.clone())), libc::EADDRINUSE);
        let client = world.connect(absolute.clone()).unwrap();
        assert!(listener.accept().is_ok());
        drop(client);
        world.unlink("sock").unwrap();
        assert_eq!(code(world.connect(relative.clone())), libc::ECONNREFUSED);
        let rebound = world.listen(relative.clone()).unwrap();
        assert!(world.connect(relative).is_ok());
        drop((listener, rebound));
        assert_eq!(code(world.listen(absolute)), libc::EADDRINUSE);
    }

    #[test]
    fn unix_address_restrictions_have_no_side_effects() {
        let world = World::new();
        let fd = world.socket(libc::AF_UNIX, libc::SOCK_STREAM).unwrap();
        for path in ["", "/embedded\0nul"] {
            let address = SocketAddress::Unix(path.into());
            assert_eq!(code(world.listen(address.clone())), libc::EINVAL);
            assert_eq!(code(world.connect(address.clone())), libc::EINVAL);
            assert_eq!(code(fd.connect(&address)), libc::EINVAL);
        }
        let long = SocketAddress::Unix(PathBuf::from("x".repeat(108)));
        assert_eq!(code(fd.connect(&long)), libc::EINVAL);
        assert_eq!(world.live_handles(), 1);
        assert_eq!(world.state().net.listeners.len(), 0);
    }

    #[test]
    fn listener_close_closes_unaccepted_connections() {
        let world = World::new();
        let address = inet("127.0.0.1:1234");
        let listener = world.listen(address.clone()).unwrap();
        let client = world.connect(address).unwrap();
        assert_eq!(world.live_handles(), 3);
        drop(listener);
        assert_eq!(world.live_handles(), 1);
        assert_eq!(code(client.send(b"x")), libc::EPIPE);
        drop(client);
        assert_eq!(world.live_handles(), 0);
    }

    #[test]
    fn ephemeral_and_wildcard_binding() {
        let world = World::new();
        let wildcard = world.listen(inet("0.0.0.0:0")).unwrap();
        let port = wildcard.local_addr().unwrap().as_inet().unwrap().port();
        assert!(EPHEMERAL_PORTS.contains(&port));
        let client = world.connect(inet(&format!("127.0.0.1:{port}"))).unwrap();
        assert!(wildcard.accept().is_ok());
        let local = client.local_addr().unwrap().as_inet().unwrap();
        assert!(local.ip().is_loopback());
        let explicit = world.socket(libc::AF_INET, libc::SOCK_STREAM).unwrap();
        assert_eq!(
            code(world.state().bind(explicit.id(), &local.into())),
            libc::EADDRINUSE
        );
        assert_eq!(
            code(world.socket(libc::AF_PACKET, libc::SOCK_STREAM)),
            libc::EAFNOSUPPORT
        );
        assert_eq!(
            code(world.socket(libc::AF_UNIX, libc::SOCK_DGRAM)),
            libc::EPROTONOSUPPORT
        );
        assert_eq!(
            code(world.socket(libc::AF_INET, libc::SOCK_RAW)),
            libc::ESOCKTNOSUPPORT
        );
        let unbound = world.socket(libc::AF_INET6, libc::SOCK_STREAM).unwrap();
        assert_eq!(unbound.local_addr().unwrap(), inet("[::]:0"));
        assert_eq!(code(unbound.peer_addr()), libc::ENOTCONN);
        assert_eq!(code(unbound.shutdown(Shutdown::Both)), libc::ENOTCONN);
    }

    #[test]
    fn pipe_close_atomicity_and_empty_splice() {
        let world = World::new();
        let (read, write) = world.pipe(4096).unwrap();
        let (socket, _peer) = world.socket_pair().unwrap();
        assert_eq!(code(read.splice(&socket, 1)), libc::EAGAIN);
        assert_eq!(read.splice(&socket, 0).unwrap(), 0);
        write.pipe_write(&[1; 4095]).unwrap();
        assert_eq!(code(write.pipe_write(&[2; 2])), libc::EAGAIN);
        assert_eq!(write.pipe_write(&[2; 4097]).unwrap(), 1);
        let mut bytes = [0; 4096];
        assert_eq!(read.pipe_read(&mut bytes).unwrap(), 4096);
        assert_eq!(bytes[4095], 2);
        drop(write);
        assert_eq!(read.pipe_read(&mut bytes).unwrap(), 0);
        assert_eq!(read.splice(&socket, 1).unwrap(), 0);
        let (read, write) = world.pipe(4096).unwrap();
        drop(read);
        assert_eq!(code(write.pipe_write(b"x")), libc::EPIPE);
        assert_eq!(write.pipe_write(b"").unwrap(), 0);
        assert_eq!(code(world.pipe(0)), libc::EINVAL);
        assert_eq!(code(world.pipe(usize::MAX)), libc::EINVAL);
        assert_eq!(code(write.pipe_read(&mut bytes)), libc::EBADF);
    }

    #[test]
    fn pipe_readiness_separates_bytes_from_closed_ends() {
        for len in 0..=2 {
            let world = World::new();
            let (read, write) = world.pipe(2).unwrap();
            assert_eq!(write.pipe_write(&b"ab"[..len]).unwrap(), len);
            assert_eq!(read.poll(IN).unwrap(), if len == 0 { 0 } else { IN });
            assert_eq!(write.poll(OUT).unwrap(), if len == 2 { 0 } else { OUT });
            drop(write);
            let data = if len == 0 { 0 } else { IN };
            assert_eq!(read.poll(IN).unwrap(), data | HUP);
            assert_eq!(read.poll(0).unwrap(), HUP);
            let mut output = [0; 2];
            assert_eq!(read.pipe_read(&mut output).unwrap(), len);
            assert_eq!(read.poll(IN).unwrap(), HUP);
            assert_eq!(read.pipe_read(&mut output).unwrap(), 0);

            let (read, write) = world.pipe(2).unwrap();
            assert_eq!(write.pipe_write(&b"ab"[..len]).unwrap(), len);
            drop(read);
            let capacity = if len < 2 { OUT } else { 0 };
            assert_eq!(write.poll(OUT).unwrap(), capacity | ERR);
            assert_eq!(write.poll(0).unwrap(), ERR);
        }
    }

    #[test]
    fn wrapped_queues_preserve_short_io_and_errors() {
        let world = World::new();
        let (writer, reader) = world.socket_pair().unwrap();
        let (pipe_reader, pipe_writer) = world.pipe(16).unwrap();
        let wrapped = || {
            let mut queue = VecDeque::with_capacity(16);
            queue.extend(0..16);
            queue.drain(..12);
            queue.extend(16..24);
            assert!(!queue.as_slices().1.is_empty());
            queue
        };
        {
            let mut state = world.state();
            state.stream_mut(reader.id()).unwrap().bytes = wrapped();
            state
                .pipe_end(pipe_reader.id(), false)
                .unwrap()
                .borrow_mut()
                .bytes = wrapped();
        }
        let mut output = [0xcc; 16];
        world
            .inject(FaultTarget::Sync(SyncOp::Recv), Fault::Short(7))
            .unwrap();
        assert_eq!(reader.recv(&mut output, false).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(&output[7..], &[0xcc; 9]);
        assert_eq!(reader.recv(&mut output, false).unwrap(), 5);
        assert_eq!(&output[..5], &[19, 20, 21, 22, 23]);
        world
            .inject(FaultTarget::Sync(SyncOp::PipeRead), Fault::Short(7))
            .unwrap();
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 7);
        assert_eq!(pipe_writer.pipe_write(&[24, 25, 26, 27]).unwrap(), 4);
        world
            .inject(FaultTarget::Sync(SyncOp::Send), Fault::Errno(libc::EPIPE))
            .unwrap();
        assert_eq!(code(pipe_reader.splice(&writer, 9)), libc::EPIPE);
        world
            .inject(FaultTarget::Sync(SyncOp::Splice), Fault::Short(6))
            .unwrap();
        assert_eq!(pipe_reader.splice(&writer, 9).unwrap(), 6);
        assert_eq!(reader.recv(&mut output, false).unwrap(), 6);
        assert_eq!(&output[..6], &[19, 20, 21, 22, 23, 24]);
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 3);
        assert_eq!(&output[..3], &[25, 26, 27]);
        assert_eq!(code(pipe_reader.pipe_read(&mut output)), libc::EAGAIN);
        assert_eq!(code(reader.recv(&mut output, false)), libc::EAGAIN);
        drop(writer);
        assert_eq!(reader.recv(&mut output, false).unwrap(), 0);
    }

    #[test]
    fn datagrams_preserve_boundaries_and_sources() {
        let world = World::new();
        let server_address: SocketAddr = "127.0.0.1:53".parse().unwrap();
        let server = world.bind_datagram(server_address).unwrap();
        let client = world.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
        assert_eq!(client.local_addr().unwrap(), inet("127.0.0.1:20000"));
        assert_eq!(code(world.bind_datagram(server_address)), libc::EADDRINUSE);
        assert_eq!(code(client.send(b"x")), libc::EDESTADDRREQ);
        client.connect(&server_address.into()).unwrap();
        assert_eq!(client.peer_addr().unwrap(), server_address.into());
        client.send(b"query").unwrap();
        assert_eq!(server.poll(IN).unwrap(), IN);
        let mut bytes = [0; 32];
        let (count, source) = server.recv_from(&mut bytes).unwrap();
        assert_eq!(&bytes[..count], b"query");
        server.send_to(b"reply", source).unwrap();
        assert_eq!(client.recv(&mut bytes, false).unwrap(), 5);
        assert_eq!(&bytes[..5], b"reply");
        assert_eq!(code(client.recv_from(&mut bytes)), libc::EAGAIN);
        assert_eq!(
            code(server.send_to(&[0; MAX_DATAGRAM + 1], source)),
            libc::EMSGSIZE
        );
        assert_eq!(
            code(server.send_to(b"x", "[::1]:1".parse().unwrap())),
            libc::EAFNOSUPPORT
        );
        assert_eq!(
            code(server.send_to(b"x", "127.0.0.1:9".parse().unwrap())),
            libc::ECONNREFUSED
        );
        let unbound = world.socket(libc::AF_INET, libc::SOCK_DGRAM).unwrap();
        unbound.send_to(b"auto", server_address).unwrap();
        assert_eq!(server.recv_from(&mut bytes).unwrap().1.port(), 20001);
    }

    #[test]
    fn stream_backpressure_and_eof() {
        let world = World::new();
        world.set_stream_capacity(3).unwrap();
        let (client, server) = world.socket_pair().unwrap();
        assert_eq!(client.send(b"abcdef").unwrap(), 3);
        assert_eq!(client.poll(OUT).unwrap(), 0);
        assert_eq!(code(client.send(b"d")), libc::EAGAIN);
        let mut buf = [0; 8];
        assert_eq!(server.recv(&mut buf, false).unwrap(), 3);
        assert_eq!(client.poll(OUT).unwrap(), OUT);
        assert!(client.idle_healthy());
        drop(client);
        assert_eq!(server.recv(&mut buf, false).unwrap(), 0);
        assert_eq!(code(server.send(b"late")), libc::EPIPE);
        assert!(server.peer_disconnected());
    }
}
