//! Socket, datagram, pipe, and readiness semantics.
use super::*;

impl SimOs {
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
        let address = normalize_address(&address)?;
        if self.0.borrow().listeners.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        if let SocketAddress::Unix(path) = &address
            && self.0.borrow().paths.contains_key(path)
        {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Listener {
            pending: VecDeque::new(),
        });
        let Some(h) = fd.as_sim() else { unreachable!() };
        self.0.borrow_mut().listeners.insert(address.clone(), h.id);
        if let SocketAddress::Unix(path) = address {
            let mut w = self.0.borrow_mut();
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
        // Convenience fixture API; use try_pipe for untrusted capacities.
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

    /// Whether this handle names a datagram socket.
    fn is_datagram(&self) -> bool {
        matches!(
            self.sim.0.borrow().resources.get(&self.id),
            Some(Resource::Datagram { .. })
        )
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
        let address = normalize_address(address)?;
        let mut w = self.sim.0.borrow_mut();
        let local = match w.resources.get(&self.id) {
            Some(Resource::Socket {
                local,
                domain,
                connected,
                ..
            }) => {
                if *domain != address_family(&address) {
                    return Err(errno(libc::EAFNOSUPPORT));
                }
                if *connected {
                    return Err(errno(libc::EISCONN));
                }
                local.clone()
            }
            _ => return Err(errno(libc::ENOTSOCK)),
        };
        if local.as_ref().is_some_and(|a| w.partitioned(a, &address)) {
            w.record("blocked:connect", self.id, 0);
            return Err(errno(libc::EAGAIN));
        }
        let listener = *w
            .listeners
            .get(&address)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        let peer = w.id();
        w.resources.insert(
            peer,
            Resource::Socket {
                domain: address_family(&address),
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
            *remote = Some(address);
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
        if self.is_datagram() {
            // Linux `send` on a connected UDP socket sends one packet.
            return self.send_datagram(bytes);
        }
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
        if self.is_datagram() {
            // Linux `recv` on a UDP socket takes one packet and drops its tail.
            return self.recv_from(bytes).map(|(n, _)| n);
        }
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
            Some(Resource::Pipe {
                bytes,
                write,
                capacity,
            }) => {
                let len = bytes.borrow().len();
                if *write {
                    let readers = w.pipe_endpoint_open(bytes, false);
                    (if len < *capacity { libc::POLLOUT } else { 0 })
                        | if readers { 0 } else { libc::POLLERR }
                } else {
                    let writers = w.pipe_endpoint_open(bytes, true);
                    (if len > 0 { libc::POLLIN } else { 0 })
                        | if writers { 0 } else { libc::POLLHUP }
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
    {
        // SAFETY: zeroed sockaddr storage is valid.
        let mut storage = unsafe { std::mem::zeroed() };
        crate::ring::encode_address(address, &mut storage)
    }
    .map(|_| ())
    .map_err(|_| errno(libc::EINVAL))
}

/// Use the fixture filesystem's path representation for Unix address keys.
fn normalize_address(address: &SocketAddress) -> io::Result<SocketAddress> {
    match address {
        SocketAddress::Unix(path) => disk::normalize(path).map(SocketAddress::Unix),
        _ => Ok(address.clone()),
    }
}
