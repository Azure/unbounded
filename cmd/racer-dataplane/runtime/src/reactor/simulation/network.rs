//! Socket, datagram, pipe, and readiness semantics.
use super::*;
impl Simulation {
    /// Creates an unconnected stream socket labeled by the current endpoint scope.
    pub fn socket(&self, domain: i32) -> io::Result<Descriptor> {
        if ![libc::AF_INET, libc::AF_INET6, libc::AF_UNIX].contains(&domain) {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        let local = self.0.borrow().endpoint.clone();
        Ok(self.insert(Resource::Socket {
            domain,
            read_shutdown: false,
            write_shutdown: false,
            peer: None,
            bytes: VecDeque::new(),
            connected: false,
            local,
            remote: None,
        }))
    }
    /// Binds a datagram endpoint, choosing the lowest free port when given zero.
    pub fn bind_datagram(&self, mut address: std::net::SocketAddr) -> io::Result<Descriptor> {
        if address.port() == 0 {
            let w = self.0.borrow();
            let port = (20000..=65535)
                .find(|port| {
                    address.set_port(*port);
                    !w.datagrams.contains_key(&address)
                })
                .ok_or_else(|| errno(libc::EADDRINUSE))?;
            address.set_port(port);
        }
        if self.0.borrow().datagrams.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Datagram {
            address,
            peer: None,
            packets: VecDeque::new(),
        });
        let Some(h) = fd.as_sim() else { unreachable!() };
        self.0.borrow_mut().datagrams.insert(address, h.id);
        Ok(fd)
    }
    /// Creates a listener and, for Unix sockets, its volatile filesystem name.
    pub fn listen(&self, address: SocketAddress) -> io::Result<Descriptor> {
        validate_address(&address)?;
        if self.0.borrow().listeners.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Listener {
            pending: VecDeque::new(),
        });
        let Some(h) = fd.as_sim() else { unreachable!() };
        self.0.borrow_mut().listeners.insert(address.clone(), h.id);
        if let SocketAddress::Unix(path) = address {
            let mut w = self.0.borrow_mut();
            if w.paths.contains_key(&path) {
                drop(w);
                drop(fd);
                return Err(errno(libc::EADDRINUSE));
            }
            let node = w.node(libc::S_IFSOCK as u16 | 0o660);
            w.paths.insert(path, node);
        }
        Ok(fd)
    }
    /// Creates and connects a stream, failing immediately if it would block.
    pub fn connect(&self, address: SocketAddress) -> io::Result<Descriptor> {
        validate_address(&address)?;
        let fd = self.socket(address_family(&address))?;
        let Some(h) = fd.as_sim() else { unreachable!() };
        h.connect(&address)?;
        Ok(fd)
    }
    /// Creates two connected Unix stream endpoints in this world.
    pub fn socket_pair(&self) -> (Descriptor, Descriptor) {
        let a = self.socket(libc::AF_UNIX).unwrap();
        let b = self.socket(libc::AF_UNIX).unwrap();
        let (Some(ah), Some(bh)) = (a.as_sim(), b.as_sim()) else {
            unreachable!()
        };
        let mut w = self.0.borrow_mut();
        for (id, peer) in [(ah.id, bh.id), (bh.id, ah.id)] {
            if let Resource::Socket {
                peer: p, connected, ..
            } = w.resources.get_mut(&id).unwrap()
            {
                *p = Some(peer);
                *connected = true;
            }
        }
        (a, b)
    }
    /// Creates a fixture pipe after clamping its capacity to the supported range.
    pub fn pipe(&self, capacity: usize) -> (Descriptor, Descriptor) {
        // Compatibility fixture API; use try_pipe for untrusted capacities.
        self.try_pipe(capacity.clamp(1, MAX_ALLOCATION))
            .expect("bounded pipe")
    }
    /// Creates a pipe's read and write ends after validating its capacity.
    pub fn try_pipe(&self, capacity: usize) -> io::Result<(Descriptor, Descriptor)> {
        if capacity == 0 || capacity > MAX_ALLOCATION {
            return Err(errno(libc::EINVAL));
        }
        let bytes = Rc::new(RefCell::new(VecDeque::new()));
        Ok((
            self.insert(Resource::Pipe {
                bytes: bytes.clone(),
                write: false,
                capacity,
            }),
            self.insert(Resource::Pipe {
                bytes,
                write: true,
                capacity,
            }),
        ))
    }
}
impl Handle {
    /// Sets a datagram destination and filters incoming packets to that peer.
    pub fn connect_datagram(&self, peer: std::net::SocketAddr) -> io::Result<()> {
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Datagram {
            peer: target,
            address,
            ..
        }) = w.resources.get_mut(&self.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        if address.is_ipv4() != peer.is_ipv4() {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        *target = Some(peer);
        Ok(())
    }
    /// Enqueues one bounded packet, preserving its source address and boundaries.
    pub fn send_to(&self, bytes: &[u8], target: std::net::SocketAddr) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("send_datagram") {
            return Err(errno(n));
        }
        let Some(Resource::Datagram { address, .. }) = w.resources.get(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let address = *address;
        if address.is_ipv4() != target.is_ipv4() {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        if bytes.len() > 65507 {
            return Err(errno(libc::EMSGSIZE));
        }
        let target_id = *w
            .datagrams
            .get(&target)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        let Some(Resource::Datagram { packets, peer, .. }) = w.resources.get_mut(&target_id) else {
            unreachable!()
        };
        if peer.is_none_or(|peer| peer == address) {
            if packets.len() >= 64 {
                return Err(errno(libc::EAGAIN));
            }
            let mut packet = bounded_bytes(bytes.len())?;
            packet.copy_from_slice(bytes);
            packets.push_back((address, packet));
        }
        w.record("send_datagram", self.id, bytes.len() as i64);
        Ok(bytes.len())
    }
    /// Receives one packet and discards any tail beyond the supplied buffer.
    pub fn recv_from(&self, bytes: &mut [u8]) -> io::Result<(usize, std::net::SocketAddr)> {
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("recv_datagram") {
            return Err(errno(n));
        }
        let Some(Resource::Datagram { packets, .. }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let (address, packet) = packets.pop_front().ok_or_else(|| errno(libc::EAGAIN))?;
        let count = bytes.len().min(packet.len());
        bytes[..count].copy_from_slice(&packet[..count]);
        w.record("recv_datagram", self.id, count as i64);
        Ok((count, address))
    }
    /// Sends one packet to the previously connected datagram peer.
    pub fn send_datagram(&self, bytes: &[u8]) -> io::Result<usize> {
        let peer = match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::Datagram {
                peer: Some(peer), ..
            }) => *peer,
            _ => return Err(errno(libc::ENOTCONN)),
        };
        self.send_to(bytes, peer)
    }
    /// Rejects descriptors that are not stream sockets.
    pub fn validate_socket(&self) -> io::Result<()> {
        if matches!(
            self.sim.0.borrow().resources.get(&self.id),
            Some(Resource::Socket { .. })
        ) {
            Ok(())
        } else {
            Err(errno(libc::ENOTSOCK))
        }
    }
    /// Reports whether an empty stream and its peer are usable for reuse.
    pub fn idle_healthy(&self) -> bool {
        let w = self.sim.0.borrow();
        matches!(w.resources.get(&self.id), Some(Resource::Socket { peer: Some(peer), bytes, read_shutdown: false, write_shutdown: false, .. }) if bytes.is_empty() && matches!(w.resources.get(peer), Some(Resource::Socket { write_shutdown: false, .. })))
    }
    /// Reports a missing peer or a peer that shut down both directions.
    pub fn peer_disconnected(&self) -> bool {
        let w = self.sim.0.borrow();
        !matches!(w.resources.get(&self.id), Some(Resource::Socket { peer: Some(peer), .. }) if matches!(w.resources.get(peer), Some(Resource::Socket { read_shutdown, write_shutdown, .. }) if !(*read_shutdown && *write_shutdown)))
    }
    /// Reports whether reads will reach EOF after queued bytes are drained.
    pub fn peer_read_closed(&self) -> bool {
        let w = self.sim.0.borrow();
        !matches!(w.resources.get(&self.id), Some(Resource::Socket { peer: Some(peer), .. }) if matches!(w.resources.get(peer), Some(Resource::Socket { write_shutdown: false, .. })))
    }
    /// Kernel-style half-close. The peer can drain queued bytes before EOF.
    pub fn shutdown(&self, how: i32) -> io::Result<()> {
        if ![libc::SHUT_RD, libc::SHUT_WR, libc::SHUT_RDWR].contains(&how) {
            return Err(errno(libc::EINVAL));
        }
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Socket {
            connected,
            read_shutdown,
            write_shutdown,
            ..
        }) = w.resources.get_mut(&self.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        if !*connected {
            return Err(errno(libc::ENOTCONN));
        }
        if how != libc::SHUT_WR {
            *read_shutdown = true;
        }
        if how != libc::SHUT_RD {
            *write_shutdown = true;
        }
        Ok(())
    }
    /// Connects this socket and queues the server endpoint for acceptance.
    pub fn connect(&self, address: &SocketAddress) -> io::Result<()> {
        validate_address(address)?;
        let mut w = self.sim.0.borrow_mut();
        let local = match w.resources.get(&self.id) {
            Some(Resource::Socket {
                local,
                domain,
                connected,
                ..
            }) => {
                if *domain != address_family(address) {
                    return Err(errno(libc::EAFNOSUPPORT));
                }
                if *connected {
                    return Err(errno(libc::EISCONN));
                }
                local.clone()
            }
            _ => return Err(errno(libc::ENOTSOCK)),
        };
        if local.as_ref().is_some_and(|a| w.partitioned(a, address)) {
            w.record("blocked:connect", self.id, 0);
            return Err(errno(libc::EAGAIN));
        }
        let listener = *w
            .listeners
            .get(address)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        let peer = w.id();
        w.resources.insert(
            peer,
            Resource::Socket {
                domain: address_family(address),
                read_shutdown: false,
                write_shutdown: false,
                peer: Some(self.id),
                bytes: VecDeque::new(),
                connected: true,
                local: Some(address.clone()),
                remote: local,
            },
        );
        if let Resource::Socket {
            peer: p,
            connected,
            remote,
            ..
        } = w.resources.get_mut(&self.id).unwrap()
        {
            *p = Some(peer);
            *connected = true;
            *remote = Some(address.clone());
        }
        let Some(Resource::Listener { pending }) = w.resources.get_mut(&listener) else {
            return Err(errno(libc::ECONNREFUSED));
        };
        pending.push_back(peer);
        w.record("connect", self.id, 0);
        Ok(())
    }
    /// Takes ownership of the oldest connection waiting on this listener.
    pub fn accept(&self) -> io::Result<Descriptor> {
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Listener { pending }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::EINVAL));
        };
        let id = pending.pop_front().ok_or_else(|| errno(libc::EAGAIN))?;
        w.record("accept", self.id, id as i64);
        Ok(Descriptor::from(Handle {
            sim: self.sim.clone(),
            id,
        }))
    }
    /// Transfers a bounded stream prefix, honoring partitions and half-closes.
    pub fn send(&self, bytes: &[u8]) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        let limit = w.transfer_limit("send")?;
        let capacity = w.stream_capacity;
        let max_chunk = w.max_chunk;
        let Some(Resource::Socket {
            peer,
            connected,
            write_shutdown,
            ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::ENOTCONN));
        };
        if *write_shutdown {
            return Err(errno(libc::EPIPE));
        }
        let peer = peer.ok_or_else(|| {
            errno(if *connected {
                libc::EPIPE
            } else {
                libc::ENOTCONN
            })
        })?;
        if !bytes.is_empty() && w.stream_partitioned(self.id) {
            w.record("blocked:send", self.id, bytes.len() as i64);
            return Err(errno(libc::EAGAIN));
        }
        let Some(Resource::Socket {
            bytes: output,
            read_shutdown,
            ..
        }) = w.resources.get_mut(&peer)
        else {
            return Err(errno(libc::EPIPE));
        };
        if *read_shutdown {
            return Err(errno(libc::EPIPE));
        }
        let count = bytes
            .len()
            .min(capacity.saturating_sub(output.len()))
            .min(max_chunk)
            .min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output.try_reserve(count).map_err(|_| errno(libc::ENOMEM))?;
        output.extend(&bytes[..count]);
        w.record("send", self.id, count as i64);
        Ok(count)
    }
    /// Drains buffered stream bytes before reporting EOF from a closed peer.
    pub fn recv(&self, bytes: &mut [u8]) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        let limit = w.transfer_limit("recv")?;
        let max_chunk = w.max_chunk;
        let Some(Resource::Socket {
            peer, connected, ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        if !*connected {
            return Err(errno(libc::ENOTCONN));
        }
        let closed = peer.is_none_or(|id| {
            !matches!(
                w.resources.get(&id),
                Some(Resource::Socket {
                    write_shutdown: false,
                    ..
                })
            )
        });
        let closed = closed
            || matches!(
                w.resources.get(&self.id),
                Some(Resource::Socket {
                    read_shutdown: true,
                    ..
                })
            );
        let Some(Resource::Socket { bytes: input, .. }) = w.resources.get_mut(&self.id) else {
            unreachable!()
        };
        if input.is_empty() && !closed && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        let count = bytes.len().min(input.len()).min(max_chunk).min(limit);
        // VecDeque's reader copies contiguous slices, including a wrapped tail.
        std::io::Read::read_exact(input, &mut bytes[..count])?;
        w.record("recv", self.id, count as i64);
        Ok(count)
    }
    /// Computes Linux-style readiness without treating it as a liveness oracle.
    pub(super) fn ready(&self, interest: u32) -> io::Result<i32> {
        let w = self.sim.0.borrow();
        let flags = match w.resources.get(&self.id) {
            Some(Resource::File { flags, .. }) => {
                // O_PATH has no pollable file operations. io_uring reports EBADF
                // rather than poll(2)'s POLLNVAL for this descriptor.
                if flags & libc::O_PATH != 0 {
                    return Err(errno(libc::EBADF));
                }
                libc::POLLIN | libc::POLLOUT | libc::POLLRDNORM | libc::POLLWRNORM
            }
            Some(Resource::Datagram { packets, .. }) => {
                libc::POLLOUT | if packets.is_empty() { 0 } else { libc::POLLIN }
            }
            Some(Resource::Listener { pending }) => {
                if pending.is_empty() {
                    0
                } else {
                    libc::POLLIN
                }
            }
            Some(Resource::Socket {
                peer,
                bytes,
                read_shutdown,
                write_shutdown,
                ..
            }) => {
                let remote = peer.and_then(|id| w.resources.get(&id));
                let mut flags = if bytes.is_empty() { 0 } else { libc::POLLIN };
                if let Some(Resource::Socket {
                    bytes,
                    read_shutdown: remote_read_shutdown,
                    write_shutdown: remote_write_shutdown,
                    ..
                }) = remote
                {
                    if bytes.len() < w.stream_capacity && !w.stream_partitioned(self.id) {
                        flags |= libc::POLLOUT;
                    }
                    if *remote_read_shutdown && *remote_write_shutdown {
                        flags |= libc::POLLHUP;
                    }
                    // Linux can leave a full sender non-writable after peer
                    // SHUT_RD even though send would fail EPIPE. Readiness is not
                    // a terminal-state oracle; caller cancellation/deadlines must
                    // bound such waits. Do not invent POLLERR to wake the poll.
                } else {
                    flags |= libc::POLLHUP | libc::POLLIN | libc::POLLOUT;
                }
                if *read_shutdown
                    || !matches!(
                        remote,
                        Some(Resource::Socket {
                            write_shutdown: false,
                            ..
                        })
                    )
                {
                    flags |= libc::POLLIN | libc::POLLRDHUP;
                }
                if *read_shutdown && *write_shutdown {
                    flags |= libc::POLLHUP;
                }
                flags
            }
            _ => return Err(errno(libc::EINVAL)),
        };
        let flags = flags as u32 & (interest | (libc::POLLHUP | libc::POLLERR) as u32);
        if flags == 0 {
            Err(errno(libc::EAGAIN))
        } else {
            Ok(flags as i32)
        }
    }
    /// Writes a pipe prefix, preserving PIPE_BUF atomicity and reader-close errors.
    pub fn pipe_write(&self, bytes: &[u8]) -> io::Result<usize> {
        let limit = self.sim.0.borrow_mut().transfer_limit("pipe_write")?;
        let w = self.sim.0.borrow();
        let Some(Resource::Pipe {
            bytes: output,
            write: true,
            capacity,
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::EBADF));
        };
        let readers = w.pipe_endpoint_open(output, false);
        if !bytes.is_empty() && !readers {
            return Err(errno(libc::EPIPE));
        }
        let mut output = output.borrow_mut();
        if bytes.len() <= libc::PIPE_BUF && bytes.len() > capacity.saturating_sub(output.len()) {
            return Err(errno(libc::EAGAIN));
        }
        let count = bytes
            .len()
            .min(capacity.saturating_sub(output.len()))
            .min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output.try_reserve(count).map_err(|_| errno(libc::ENOMEM))?;
        output.extend(&bytes[..count]);
        Ok(count)
    }
    /// Drains a pipe and reports EOF only after its last writer closes.
    pub fn pipe_read(&self, bytes: &mut [u8]) -> io::Result<usize> {
        let limit = self.sim.0.borrow_mut().transfer_limit("pipe_read")?;
        let w = self.sim.0.borrow();
        let Some(Resource::Pipe {
            bytes: input,
            write: false,
            ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::EBADF));
        };
        let writers = w.pipe_endpoint_open(input, true);
        let mut input = input.borrow_mut();
        let count = bytes.len().min(input.len()).min(limit);
        if count == 0 && !bytes.is_empty() && writers {
            return Err(errno(libc::EAGAIN));
        }
        std::io::Read::read_exact(&mut *input, &mut bytes[..count])?;
        Ok(count)
    }
    /// Moves pipe bytes to a stream, consuming only the successfully sent prefix.
    pub fn splice(&self, socket: &Handle, count: usize) -> io::Result<usize> {
        let count = count.min(self.sim.0.borrow_mut().transfer_limit("splice")?);
        if !Rc::ptr_eq(&self.sim.0, &socket.sim.0) {
            return Err(errno(libc::EXDEV));
        }
        let bytes = {
            let w = self.sim.0.borrow();
            let Some(Resource::Pipe {
                bytes,
                write: false,
                ..
            }) = w.resources.get(&self.id)
            else {
                return Err(errno(libc::EBADF));
            };
            bytes.clone()
        };
        let mut input = bytes.borrow_mut();
        if count != 0 && input.is_empty() {
            let w = self.sim.0.borrow();
            if w.pipe_endpoint_open(&bytes, true) {
                return Err(errno(libc::EAGAIN));
            }
            return Ok(0);
        }
        let count = count.min(input.len());
        let sent = socket.send(&input.make_contiguous()[..count])?;
        input.drain(..sent);
        Ok(sent)
    }
}

impl World {
    /// Identify a live pipe end by shared queue identity, never by buffered bytes.
    fn pipe_endpoint_open(&self, queue: &Rc<RefCell<VecDeque<u8>>>, writing: bool) -> bool {
        self.resources.values().any(|resource| {
            matches!(resource, Resource::Pipe { bytes, write, .. }
                if *write == writing && Rc::ptr_eq(bytes, queue))
        })
    }
}

/// Maps the shared socket address representation to its Linux family.
fn address_family(address: &SocketAddress) -> i32 {
    match address {
        SocketAddress::Unix(_) => libc::AF_UNIX,
        SocketAddress::Inet(address) if address.is_ipv4() => libc::AF_INET,
        SocketAddress::Inet(_) => libc::AF_INET6,
    }
}

/// Applies the same pathname-only Unix address restrictions as the real kernel path.
fn validate_address(address: &SocketAddress) -> io::Result<()> {
    super::super::encode_address(address.clone())
        .map(|_| ())
        .map_err(|_| errno(libc::EINVAL))
}

/// End-to-end stream, packet, and completion ownership tests.
#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use crate::reactor::tests::fixtures::{
        Admission, Limits, Reactor, RequestScope, ResourceClass,
    };
    use crate::{Error, Operation, Result};
    use std::{
        cell::Cell,
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    /// Creates a production reactor with a small, fixed submission budget for tests.
    pub(in crate::reactor::simulation) fn reactor() -> Reactor {
        Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: std::num::NonZeroUsize::new(64).unwrap(),
        })))
    }
    /// Gives test operations a shared request scope with a bounded deadline.
    pub(in crate::reactor::simulation) fn scope() -> RequestScope {
        RequestScope::new((), Instant::now() + Duration::from_secs(30)).unwrap()
    }
    /// Polls an operation once without advancing the reactor or registering a wakeup.
    pub(in crate::reactor::simulation) fn poll<T>(op: &mut Operation<'_, T>) -> Poll<Result<T>> {
        op.as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    /// Drives bounded reactor turns until completion, failing if the fixture stalls.
    pub(in crate::reactor::simulation) fn drive<T>(
        r: &Reactor,
        mut op: Operation<'_, T>,
    ) -> Result<T> {
        for _ in 0..1000 {
            if let Poll::Ready(result) = poll(&mut op) {
                return result;
            }
            r.poll_budgeted(8)?;
            r.wait(Duration::ZERO)?;
        }
        panic!("simulation did not progress")
    }

    #[test]
    /// Drive real reactor ownership through stream backpressure, replies, EOF, and drain.
    fn real_reactor_stream_backpressure_eof_and_completion_fences() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        assert!(r.state.borrow().ring.is_none());
        assert!(r.state.borrow().wake.is_none());
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let address = SocketAddress::Unix("/stream".into());
        let listener = Rc::new(sim.listen(address.clone()).unwrap());
        let client = Rc::new(Descriptor::socket(libc::AF_UNIX).unwrap());
        drive(&r, r.connect(client.clone(), address, &scope)).unwrap();
        let server = Rc::new(drive(&r, r.accept(listener.clone(), &scope)).unwrap());
        sim.set_stream_capacity(3).unwrap();
        let mut sent = drive(
            &r,
            r.send(client.clone(), r.file_bytes(b"abcdef").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 3);
        sent.buffer.advance(sent.bytes).unwrap();
        assert_eq!(sent.buffer.remaining(), 3);
        let mut writable = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
        assert!(poll(&mut writable).is_pending());
        r.poll_budgeted(8).unwrap();
        assert!(poll(&mut writable).is_pending());
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 3);
        assert_eq!(read.buffer.prefix(3).unwrap(), b"abc");
        drop(read);
        assert_eq!(drive(&r, writable).unwrap(), libc::POLLOUT as u32);
        // Reuse the returned owner to finish the request, then send a reply.
        let mut sent = drive(&r, r.send(client.clone(), sent.buffer, (), &scope)).unwrap();
        assert_eq!(sent.bytes, 3);
        sent.buffer.advance(sent.bytes).unwrap();
        assert_eq!(sent.buffer.remaining(), 0);
        drop(sent);
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.buffer.prefix(read.bytes).unwrap(), b"def");
        drop(read);
        let sent = drive(
            &r,
            r.send(server.clone(), r.file_bytes(b"ok").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 2);
        drop(sent);
        let reply = drive(
            &r,
            r.recv(client.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(reply.buffer.prefix(reply.bytes).unwrap(), b"ok");
        drop(reply);
        drop(client);
        assert_eq!(
            drive(
                &r,
                r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        assert!(matches!(
            drive(
                &r,
                r.send(server.clone(), r.file_bytes(b"late").unwrap(), (), &scope)
            ),
            Err(Error::Os(libc::EPIPE))
        ));
        drive(&r, r.drain()).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.init(), Err(Error::Unavailable));
        drop((server, listener));
        assert_eq!(sim.live_handles(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    }

    #[test]
    /// Isolate nested worlds and close unaccepted server sockets with their listener.
    fn scoped_selection_and_listener_pending_close_are_isolated() {
        let sim = Simulation::new();
        let other = Simulation::new();
        assert!(Simulation::current().is_none());
        {
            let _scope = sim.enter();
            {
                let _nested = other.enter();
                assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &other.0));
            }
            assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &sim.0));
        }
        assert!(Simulation::current().is_none());
        let address = SocketAddress::Inet("127.0.0.1:1234".parse().unwrap());
        let listener = sim.listen(address.clone()).unwrap();
        let client = sim.connect(address).unwrap();
        assert_eq!(sim.live_handles(), 3);
        drop(listener);
        assert_eq!(sim.live_handles(), 1);
        let client = client.into_sim().unwrap();
        assert_eq!(
            client.send(b"x").unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        drop(client);
        assert_eq!(sim.live_handles(), 0);
    }

    #[test]
    /// Preserve wrapped queue prefixes across short reads, splice failures, and EOF.
    fn wrapped_stream_and_pipe_copies_preserve_short_io_and_errors() {
        let sim = Simulation::new();
        let (writer, reader) = sim.socket_pair();
        let (writer, reader) = (writer.into_sim().unwrap(), reader.into_sim().unwrap());
        let (pipe_reader, pipe_writer) = sim.pipe(16);
        let (pipe_reader, pipe_writer) = (
            pipe_reader.into_sim().unwrap(),
            pipe_writer.into_sim().unwrap(),
        );
        // Install wrapped queues explicitly so this does not depend on allocator growth.
        let wrapped = || {
            let mut queue = VecDeque::with_capacity(16);
            queue.extend(0..16);
            queue.drain(..12);
            queue.extend(16..24);
            assert!(!queue.as_slices().1.is_empty());
            queue
        };
        {
            let mut world = sim.0.borrow_mut();
            let Resource::Socket { bytes, .. } = world.resources.get_mut(&reader.id).unwrap()
            else {
                unreachable!()
            };
            *bytes = wrapped();
            let Resource::Pipe { bytes, .. } = world.resources.get(&pipe_reader.id).unwrap() else {
                unreachable!()
            };
            *bytes.borrow_mut() = wrapped();
        }
        let mut output = [0xcc; 16];
        sim.inject("recv", Fault::Short(7)).unwrap();
        assert_eq!(reader.recv(&mut output).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(&output[7..], &[0xcc; 9]);
        assert_eq!(reader.recv(&mut output).unwrap(), 5);
        assert_eq!(&output[..5], &[19, 20, 21, 22, 23]);
        sim.inject("pipe_read", Fault::Short(7)).unwrap();
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(pipe_writer.pipe_write(&[24, 25, 26, 27]).unwrap(), 4);
        sim.inject("send", Fault::Errno(libc::EPIPE)).unwrap();
        assert_eq!(
            pipe_reader.splice(&writer, 9).unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        sim.inject("splice", Fault::Short(6)).unwrap();
        assert_eq!(pipe_reader.splice(&writer, 9).unwrap(), 6);
        assert_eq!(reader.recv(&mut output).unwrap(), 6);
        assert_eq!(&output[..6], &[19, 20, 21, 22, 23, 24]);
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 3);
        assert_eq!(&output[..3], &[25, 26, 27]);
        assert_eq!(
            pipe_reader.pipe_read(&mut output).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(
            reader.recv(&mut output).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        drop(writer);
        assert_eq!(reader.recv(&mut output).unwrap(), 0);
    }

    #[test]
    /// Keep source addresses and packet boundaries through a datagram round trip.
    fn datagrams_preserve_packet_boundaries_and_source_addresses() {
        let sim = Simulation::new();
        let server_address = "127.0.0.1:53".parse().unwrap();
        let server = sim.bind_datagram(server_address).unwrap();
        let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
        let (server, client) = (server.into_sim().unwrap(), client.into_sim().unwrap());
        client.connect_datagram(server_address).unwrap();
        client.send_datagram(b"query").unwrap();
        let mut bytes = [0; 32];
        let (count, source) = server.recv_from(&mut bytes).unwrap();
        assert_eq!(&bytes[..count], b"query");
        server.send_to(b"reply", source).unwrap();
        let (count, source) = client.recv_from(&mut bytes).unwrap();
        assert_eq!(source, server_address);
        assert_eq!(&bytes[..count], b"reply");
    }

    /// Counts owner destruction to verify resources survive both completion fences.
    struct Probe(Rc<Cell<usize>>);
    impl Drop for Probe {
        /// Count one resource release after its final completion fence.
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }

    #[test]
    /// Retain abandoned descriptors, buffers, and leases through either CQE ordering.
    fn abandoned_resources_wait_for_both_fences_in_either_order() {
        for cancel_first in [false, true] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            r.init().unwrap();
            let baseline = r.admission.used(ResourceClass::RequestContext);
            let scope = scope();
            let (fd, peer) = sim.socket_pair();
            let fd = Rc::new(fd);
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let lease = r
                .admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap();
            let mut recv = r.recv(
                fd,
                r.file_buffer(8).unwrap(),
                (Probe(drops.clone()), lease),
                &scope,
            );
            assert!(poll(&mut recv).is_pending());
            let id = *r.state.borrow().entries.keys().next().unwrap();
            // An unsolicited cancellation CQE must not mutate the live entry.
            assert!(matches!(
                r.state.borrow_mut().complete(id.0 | CANCEL_BIT, 0),
                Err(Error::Io)
            ));
            drop(recv);
            assert_eq!(r.poll_budgeted(1), Ok(0));
            if !cancel_first {
                // Reorder the two actual driver CQEs, leaving production fence logic intact.
                r.state
                    .borrow_mut()
                    .simulation
                    .as_mut()
                    .unwrap()
                    .completed
                    .borrow_mut()
                    .swap(0, 1);
            }
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(r.in_flight(), 1);
            assert_eq!(r.admission.used(ResourceClass::Connection), 1);
            assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert_eq!(drops.get(), 1);
            assert!(weak.upgrade().is_none());
            assert_eq!(r.in_flight(), 0);
            assert_eq!(r.admission.used(ResourceClass::Connection), 0);
            assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
            assert!(matches!(
                r.state.borrow_mut().complete(id.0, 8),
                Err(Error::Io)
            ));
            drop(peer);
            assert_eq!(sim.live_handles(), 0);
        }
    }

    #[test]
    /// Preserve owners and monotonic operation IDs through delayed short I/O and failure.
    fn scheduled_short_io_disconnect_and_budget_preserve_resource_ownership() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let (fd, peer) = sim.socket_pair();
        let fd = Rc::new(fd);
        let drops = Rc::new(Cell::new(0));
        sim.inject("send", Fault::Delay(2)).unwrap();
        sim.set_max_chunk(3).unwrap();
        let mut first = r.send(
            fd.clone(),
            r.file_bytes(&[1; 8]).unwrap(),
            Probe(drops.clone()),
            &scope,
        );
        assert!(poll(&mut first).is_pending());
        let first_id = *r.state.borrow().entries.keys().next().unwrap();
        sim.inject("send", Fault::Errno(libc::ECONNRESET)).unwrap();
        let mut second = r.send(
            fd.clone(),
            r.file_bytes(&[2; 8]).unwrap(),
            Probe(drops.clone()),
            &scope,
        );
        assert!(poll(&mut second).is_pending());
        assert_eq!(r.poll_budgeted(0), Ok(0));
        assert_eq!(r.poll_budgeted(1), Ok(0));
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert!(poll(&mut first).is_pending());
        assert!(matches!(
            poll(&mut second),
            std::task::Poll::Ready(Err(Error::Os(libc::ECONNRESET)))
        ));
        assert_eq!(
            drops.get(),
            1,
            "failed I/O releases its owned resources after the CQE"
        );
        drop(second);
        assert_eq!(r.poll_budgeted(1), Ok(0));
        assert_eq!(r.poll_budgeted(0), Ok(0));
        assert!(poll(&mut first).is_pending());
        assert_eq!(r.poll_budgeted(1), Ok(1));
        let std::task::Poll::Ready(Ok(mut completed)) = poll(&mut first) else {
            panic!("short send did not complete")
        };
        drop(first);
        assert_eq!(completed.bytes, 3);
        assert_eq!(completed.buffer.prefix(8).unwrap(), &[1; 8]);
        assert_eq!(drops.get(), 1);
        assert_eq!(completed.buffer.advance(9), Err(Error::Io));
        assert_eq!(completed.buffer.remaining(), 8);
        completed.buffer.advance(3).unwrap();
        let mut remainder = r.send(fd.clone(), completed.buffer, completed.lease, &scope);
        assert!(poll(&mut remainder).is_pending());
        let next_id = *r.state.borrow().entries.keys().next().unwrap();
        assert!(next_id > first_id);
        assert!(matches!(
            r.state.borrow_mut().complete(first_id.0, 3),
            Err(Error::Io)
        ));
        assert_eq!(
            r.in_flight(),
            1,
            "stale CQE cannot retire the new submission"
        );
        sim.disconnect(&fd).unwrap();
        assert!(matches!(drive(&r, remainder), Err(Error::Os(libc::EPIPE))));
        assert_eq!(drops.get(), 2);
        let mut bytes = [0; 8];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 3);
        assert_eq!(&bytes[..3], &[1; 3]);
        let eof = drive(
            &r,
            r.recv(Rc::new(peer), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(eof.bytes, 0);
        drop((eof, fd));
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(sim.live_handles(), 0);
        assert!(
            sim.trace()
                .iter()
                .any(|e| e.operation == "submit:send" && e.resource == next_id.0)
        );
    }
}
