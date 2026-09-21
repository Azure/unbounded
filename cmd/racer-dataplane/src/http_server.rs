//! Worker-local, plain TCP HTTP/1.1 GET/HEAD transport.
//!
//! [`Server`] drives a bounded set of [`Handler`] tasks from the worker's
//! [`crate::uring::Application`]. The same affine exchange types can be used with
//! a caller-owned scheduler. Polling never drives or sleeps the ring: merge every
//! pending [`Work`] into the driver's sleep decision. All polls are budgeted.
//!
//! Request targets and headers are opaque application inputs. Responses carry
//! caller-selected metadata and immutable pool buffers, like the RDMA transport;
//! lookup, routing, validators, authentication and page production belong above
//! this module. No path is reserved, and HTTP Upgrade is unsupported.
//! RDMA Offers can travel in ordinary HTTP headers: the application authenticates
//! and decodes them before passing them to [`crate::rdma`], and selects TCP fallback.
//!
//! Each connection reuses two 8 KiB allocations: request/read-ahead storage and
//! response headers. There are at most 64 fields per message. Requests have no
//! body; responses use Content-Length, never chunked framing. Pipelined requests
//! are processed sequentially. GET streams any u64 length in bounded pool chunks.
//! Payload sends use SEND_ZC; headers use ordinary SEND. The kernel may choose a
//! copying fallback. Pool ownership lasts through the actual ZC notification.
//!
//! Dropping an unfinished exchange shuts down its socket. Cancellation does not
//! recycle in-flight memory: the ring owns it until terminal completion. Forgetting
//! handles can exhaust capacity but cannot release kernel-accessible storage.
//!
//! Requests authorize exactly one response:
//! ```compile_fail
//! use racer_dataplane::http_server::*;
//! fn twice(r: GetRequest) {
//!     let a = r.respond(ResponseHead::new(200, Some(0), &[]).unwrap());
//!     let b = r.respond(ResponseHead::new(200, Some(0), &[]).unwrap());
//! }
//! ```
//! HEAD has no payload API:
//! ```compile_fail
//! use racer_dataplane::http_server::*;
//! fn payload(r: HeadRequest, chunk: BodyChunk) { r.send(chunk); }
//! ```
//! Only completion permits connection recycling:
//! ```compile_fail
//! use racer_dataplane::http_server::*;
//! fn early(w: BodyWriter) { w.recycle(); }
//! ```
//! Header views cannot survive response construction:
//! ```compile_fail
//! use racer_dataplane::http_server::*;
//! fn stale(r: GetRequest) {
//!     let headers = r.headers();
//!     let response = r.respond(ResponseHead::new(200, Some(0), &[]).unwrap());
//!     println!("{:?}", headers.get("etag"));
//! }
//! ```
//! Connections cannot move between worker threads:
//! ```compile_fail
//! use racer_dataplane::http_server::Connection;
//! fn transfer(c: Connection) { std::thread::spawn(move || drop(c)); }
//! ```

use crate::buffers::Buffer;
use crate::http::{
    Header, MAX_HEADERS, SCRATCH_SIZE, Span, authority, connection_close, decimal, field,
    header_end, invalid, line, protocol, target, token, transfer, trim, value,
};
pub use crate::http::{Headers, Progress};
use crate::uring::{
    Accept, BufferRange, Bytes, File, FixedFile, Identity, Rejected, Ring, SendZc, Ticket, Work,
};
use std::cell::Cell;
use std::collections::VecDeque;
use std::io::{self, Write};
use std::net::{SocketAddr, TcpListener};
use std::num::{NonZeroU32, NonZeroUsize};
use std::ops::Range;
use std::os::fd::{AsFd, AsRawFd, FromRawFd, OwnedFd};
use std::rc::Rc;
use std::time::{Duration, Instant};

// Cache-backed streaming belongs beside the transport's chunk/framing contracts.
// The handler kernel corpus supplies its original two-buffer ring and deadline.
#[cfg(test)]
pub(crate) use tests::cache_responses;

const ZC_WINDOW: usize = 4;
const ADMISSION_RETRY: Duration = Duration::from_millis(10);

/// Real HTTP fixture shared by runtime's deterministic origin/control scenarios.
#[cfg(test)]
pub(crate) use tests::scenario_origin;

fn pending<T>(runnable: bool, deadline: Option<Instant>) -> Progress<T> {
    Progress::Pending(Work { runnable, deadline })
}
fn check_deadline(deadline: Instant) -> io::Result<()> {
    if crate::environment::now() >= deadline {
        Err(io::Error::new(
            io::ErrorKind::TimedOut,
            "HTTP server deadline",
        ))
    } else {
        Ok(())
    }
}

/// Shared with the scheduler so even a handler that never polls its response is
/// bounded. Only successful socket sends renew this clock, not handler activity,
/// buffer acquisition, file-to-pipe copies, or zero-copy notifications.
#[derive(Clone)]
pub(crate) struct Deadline(Rc<DeadlineState>);
struct DeadlineState {
    end: Cell<Instant>,
    inactivity: Cell<Option<Duration>>,
    cap: Cell<Option<Instant>>,
}
impl Deadline {
    fn absolute(end: Instant) -> Self {
        Self(Rc::new(DeadlineState {
            end: Cell::new(end),
            inactivity: Cell::new(None),
            cap: Cell::new(None),
        }))
    }
    pub(crate) fn get(&self) -> Instant {
        self.0.end.get()
    }
    fn cap(&self, cap: Instant) {
        let cap = self.get().min(cap);
        self.0.cap.set(Some(cap));
        self.0.end.set(self.get().min(cap));
    }
    fn sent(&self) -> io::Result<()> {
        if let Some(timeout) = self.0.inactivity.get() {
            let end = crate::environment::now()
                .checked_add(timeout)
                .ok_or_else(|| invalid("deadline overflow"))?;
            self.0
                .end
                .set(self.0.cap.get().map_or(end, |cap| end.min(cap)));
        }
        Ok(())
    }
}
fn merge(work: &mut Work, other: Work) {
    work.runnable |= other.runnable;
    work.deadline = match (work.deadline, other.deadline) {
        (Some(a), Some(b)) => Some(a.min(b)),
        (a, b) => a.or(b),
    };
}
fn option(fd: &impl AsFd, level: i32, name: i32) -> io::Result<()> {
    #[cfg(test)]
    if crate::simulation::current().is_some() {
        return Ok(());
    }
    let one: libc::c_int = 1;
    // SAFETY: live descriptor, correctly sized and aligned option value.
    if unsafe {
        libc::setsockopt(
            fd.as_fd().as_raw_fd(),
            level,
            name,
            (&one as *const libc::c_int).cast(),
            size_of_val(&one) as libc::socklen_t,
        )
    } < 0
    {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

struct Control {
    file: File,
    closed: Cell<bool>,
}

/// Opaque worker-local TCP identity, stable across keep-alive requests. Holding
/// it prevents identity reuse but does not keep a dropped connection open.
#[derive(Clone)]
pub struct ConnectionId(Rc<Control>);
impl ConnectionId {
    pub fn is_closed(&self) -> bool {
        self.0.closed.get()
    }
}
impl PartialEq for ConnectionId {
    fn eq(&self, other: &Self) -> bool {
        Rc::ptr_eq(&self.0, &other.0)
    }
}
impl Eq for ConnectionId {}
impl std::hash::Hash for ConnectionId {
    fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
        std::hash::Hash::hash(&Rc::as_ptr(&self.0), state);
    }
}
impl Control {
    fn close(&self) {
        if !self.closed.replace(true) {
            self.file.shutdown_socket();
        }
    }
}

/// Worker-local listener. One outstanding accept; dropping cancels it even under
/// SQ pressure. Bind the same volume address on each worker using SO_REUSEPORT.
pub struct Listener {
    file: File,
    address: SocketAddr,
    ticket: Option<Ticket<Accept>>,
    ring: Option<Rc<Identity>>,
    retry_at: Option<Instant>,
    pressure_failures: u8,
    // One descriptor per listener may wait for the shared ring admission budget.
    // It owns no scratch or receive ticket until admitted.
    accepted: Option<File>,
}
impl Drop for Listener {
    fn drop(&mut self) {
        // A pending ACCEPT retains the descriptor through its terminal CQE.
        // Remove the kernel listening endpoint immediately when map ownership
        // ends, before a later prepare may bind an overlapping replacement.
        // This does not close accepted sockets or release ring-owned storage.
        self.file.shutdown_socket();
    }
}
impl Listener {
    pub fn bind(address: SocketAddr, backlog: NonZeroU32) -> io::Result<Self> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            return Ok(Self {
                file: File::simulated(world.listen(address)?),
                address,
                ticket: None,
                ring: None,
                retry_at: None,
                pressure_failures: 0,
                accepted: None,
            });
        }
        let backlog = i32::try_from(backlog.get()).map_err(|_| invalid("backlog exceeds i32"))?;
        // SAFETY: socket returns a fresh owned descriptor.
        let raw = unsafe {
            libc::socket(
                if address.is_ipv4() {
                    libc::AF_INET
                } else {
                    libc::AF_INET6
                },
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        if raw < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: successful socket is uniquely owned.
        let fd = unsafe { OwnedFd::from_raw_fd(raw) };
        option(&fd, libc::SOL_SOCKET, libc::SO_REUSEADDR)?;
        option(&fd, libc::SOL_SOCKET, libc::SO_REUSEPORT)?;
        // SAFETY: both sockaddr variants are initialized and passed with their exact size.
        let bound = unsafe {
            match address {
                SocketAddr::V4(a) => {
                    let addr = libc::sockaddr_in {
                        sin_family: libc::AF_INET as _,
                        sin_port: a.port().to_be(),
                        sin_addr: libc::in_addr {
                            s_addr: u32::from_ne_bytes(a.ip().octets()),
                        },
                        sin_zero: [0; 8],
                    };
                    libc::bind(
                        raw,
                        (&addr as *const libc::sockaddr_in).cast(),
                        size_of_val(&addr) as _,
                    )
                }
                SocketAddr::V6(a) => {
                    let addr = libc::sockaddr_in6 {
                        sin6_family: libc::AF_INET6 as _,
                        sin6_port: a.port().to_be(),
                        sin6_flowinfo: a.flowinfo().to_be(),
                        sin6_scope_id: a.scope_id(),
                        sin6_addr: libc::in6_addr {
                            s6_addr: a.ip().octets(),
                        },
                    };
                    libc::bind(
                        raw,
                        (&addr as *const libc::sockaddr_in6).cast(),
                        size_of_val(&addr) as _,
                    )
                }
            }
        };
        if bound < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: bound, owned stream socket and validated backlog.
        if unsafe { libc::listen(raw, backlog) } < 0 {
            return Err(io::Error::last_os_error());
        }
        let listener = TcpListener::from(fd);
        let address = listener.local_addr()?;
        Ok(Self {
            file: File::new(listener.into()),
            address,
            ticket: None,
            ring: None,
            retry_at: None,
            pressure_failures: 0,
            accepted: None,
        })
    }
    pub fn local_addr(&self) -> io::Result<SocketAddr> {
        Ok(self.address)
    }

    pub fn poll_accept(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<Connection>> {
        if let Some(id) = &self.ring {
            if !Rc::ptr_eq(id, ring.identity()) {
                return Err(invalid("listener belongs to another ring"));
            }
        } else {
            self.ring = Some(ring.identity().clone());
        }
        if let Some(after) = self.retry_at {
            if crate::environment::now() < after {
                return Ok(pending(false, Some(after)));
            }
            self.retry_at = None;
        }
        for _ in 0..budget {
            if self.accepted.is_some() {
                let Some(admission) = ring.admit_inbound() else {
                    let after = crate::environment::now() + ADMISSION_RETRY;
                    self.retry_at = Some(after);
                    return Ok(pending(false, Some(after)));
                };
                return Ok(Progress::Ready(Connection {
                    control: Rc::new(Control {
                        file: self.accepted.take().unwrap(),
                        closed: Cell::new(false),
                    }),
                    admission,
                    fixed: None,
                    ring: ring.identity().clone(),
                    input: Some(vec![0; SCRATCH_SIZE].into_boxed_slice()),
                    output: Some(vec![0; SCRATCH_SIZE].into_boxed_slice()),
                    used: 0,
                }));
            }
            if let Some(t) = &mut self.ticket {
                let Some(c) = ring.take_accept(t)? else {
                    return Ok(pending(false, None));
                };
                self.ticket = None;
                if let Err(e) = c.result {
                    if matches!(e.raw_os_error(), Some(libc::EMFILE | libc::ENFILE)) {
                        // Descriptor pressure is listener-local. Preserve accepted work
                        // and let the driver sleep rather than fail the worker group.
                        let delay = (10u64 << self.pressure_failures.min(7)).min(1000);
                        self.pressure_failures = self.pressure_failures.saturating_add(1);
                        let after = crate::environment::now() + Duration::from_millis(delay);
                        self.retry_at = Some(after);
                        return Ok(pending(false, Some(after)));
                    }
                    // Linux may report an individual incoming connection's error on accept.
                    if matches!(
                        e.raw_os_error(),
                        Some(
                            libc::ECONNABORTED
                                | libc::EPROTO
                                | libc::ENETDOWN
                                | libc::ENETUNREACH
                                | libc::EHOSTUNREACH
                        )
                    ) {
                        continue;
                    }
                    return Err(e);
                }
                let file = c
                    .resource
                    .ok_or_else(|| protocol("accept missing descriptor"))?;
                option(&file, libc::IPPROTO_TCP, libc::TCP_NODELAY)?;
                self.pressure_failures = 0;
                self.accepted = Some(file);
                continue;
            }
            match ring.accept(self.file.clone().into()) {
                Ok(t) => self.ticket = Some(t.cancel_on_drop()),
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    let after = crate::environment::now() + ADMISSION_RETRY;
                    self.retry_at = Some(after);
                    return Ok(pending(false, Some(after)));
                }
                Err(e) => return Err(e),
            }
        }
        Ok(pending(true, None))
    }
}

/// Affine idle connection. Its two scratch allocations are reused across requests.
#[must_use]
pub struct Connection {
    // The registration also retains this lease for ring-owned IO after drop.
    admission: Rc<crate::uring::Inbound>,
    control: Rc<Control>,
    fixed: Option<FixedFile>,
    ring: Rc<Identity>,
    input: Option<Box<[u8]>>,
    output: Option<Box<[u8]>>,
    used: usize,
}
impl Drop for Connection {
    fn drop(&mut self) {
        self.control.close();
    }
}
impl Connection {
    pub fn connection_id(&self) -> ConnectionId {
        ConnectionId(self.control.clone())
    }
    fn check(&self, ring: &Ring, deadline: Instant) -> io::Result<()> {
        check_deadline(deadline)?;
        if self.control.closed.get() {
            return Err(io::Error::new(
                io::ErrorKind::NotConnected,
                "HTTP connection closed",
            ));
        }
        if !Rc::ptr_eq(&self.ring, ring.identity()) {
            return Err(invalid("connection belongs to another ring"));
        }
        Ok(())
    }
    /// Receive with an absolute deadline for the entire exchange. `Server` adds
    /// its configured streaming inactivity policy after request admission.
    pub fn receive(self, deadline: Instant) -> ReceivingRequest {
        ReceivingRequest {
            connection: Some(self),
            ticket: None,
            scan: 0,
            deadline,
            first_byte_timeout: None,
            rejection: None,
            retry_at: None,
        }
    }
}

struct Metadata {
    head: bool,
    target: Span,
    headers: [Header; MAX_HEADERS],
    count: usize,
    close: bool,
}

// S3 SDKs can send absolute-form targets even to an origin server. The volume
// is selected by the listener, never by this authority. Preserve the exact
// path/query bytes for cache identity and peer authentication (no URL decoding).
fn target_offset(path: &[u8]) -> Option<usize> {
    if target(path) {
        return Some(0);
    }
    let scheme = path.windows(3).position(|w| w == b"://")?;
    if !path[..scheme].eq_ignore_ascii_case(b"http")
        && !path[..scheme].eq_ignore_ascii_case(b"https")
    {
        return None;
    }
    let start = scheme + 3;
    let offset = start + path[start..].iter().position(|&b| b == b'/')?;
    (authority(&path[start..offset]) && target(&path[offset..])).then_some(offset)
}

// Error values are final HTTP statuses, never application errors.
fn parse(bytes: &[u8], end: usize) -> Result<Metadata, u16> {
    let stop = line(bytes, 0, end).map_err(|_| 400u16)?;
    let mut parts = bytes[..stop].split(|&b| b == b' ');
    let method = parts.next().ok_or(400u16)?;
    let path = parts.next().ok_or(400u16)?;
    let version = parts.next().ok_or(400u16)?;
    let offset = target_offset(path).ok_or(400u16)?;
    if method.is_empty()
        || !method.iter().copied().all(token)
        || version != b"HTTP/1.1"
        || parts.next().is_some()
    {
        return Err(400);
    }
    let mut m = Metadata {
        head: method == b"HEAD",
        target: Span {
            start: (method.len() + 1 + offset) as u16,
            end: (method.len() + 1 + path.len()) as u16,
        },
        headers: [Header::default(); MAX_HEADERS],
        count: 0,
        close: false,
    };
    let mut host = false;
    let mut length = None;
    let mut pos = stop + 2;
    while pos < end - 2 {
        if m.count == MAX_HEADERS {
            return Err(431);
        }
        let stop = line(bytes, pos, end).map_err(|_| 400u16)?;
        let h = field(bytes, pos, stop).map_err(|_| 400u16)?;
        let name = h.name.slice(bytes);
        let v = h.value.slice(bytes);
        if name.eq_ignore_ascii_case(b"host") {
            if host || !authority(v) {
                return Err(400);
            }
            host = true;
        } else if name.eq_ignore_ascii_case(b"content-length") {
            if length.is_some() {
                return Err(400);
            }
            length = Some(decimal(v).map_err(|_| 400u16)?);
            if length != Some(0) {
                return Err(400);
            }
        } else if name.eq_ignore_ascii_case(b"connection") {
            m.close |= connection_close(v).map_err(|_| 400u16)?;
        } else if name.eq_ignore_ascii_case(b"expect") {
            return Err(417);
        } else if [
            b"transfer-encoding".as_slice(),
            b"upgrade",
            b"trailer",
            b"proxy-connection",
        ]
        .iter()
        .any(|n| name.eq_ignore_ascii_case(n))
        {
            return Err(400);
        }
        m.headers[m.count] = h;
        m.count += 1;
        pos = stop + 2;
    }
    if !host {
        return Err(400);
    }
    if method != b"GET" && method != b"HEAD" {
        return Err(405);
    }
    Ok(m)
}

/// Receives one request; malformed requests receive a bounded error then close.
/// EOF and completed protocol rejection are returned as connection-local errors.
#[must_use]
pub struct ReceivingRequest {
    connection: Option<Connection>,
    ticket: Option<Ticket<Bytes>>,
    scan: usize,
    deadline: Instant,
    first_byte_timeout: Option<Duration>,
    rejection: Option<SendingHeadHeaders>,
    retry_at: Option<Instant>,
}
impl ReceivingRequest {
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Request>> {
        let result = self.poll_inner(ring, budget);
        if result.is_err() {
            self.connection.take();
            self.ticket.take();
            self.rejection.take();
        }
        result
    }
    fn poll_inner(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Request>> {
        check_deadline(self.deadline)?;
        if let Some(rejection) = &mut self.rejection {
            return match rejection.poll(ring, budget)? {
                Progress::Pending(w) => Ok(Progress::Pending(w)),
                Progress::Ready(_) => Err(protocol("request rejected")),
            };
        }
        let c = self
            .connection
            .as_mut()
            .ok_or_else(|| invalid("receive already finished"))?;
        c.check(ring, self.deadline)?;
        if let Some(after) = self.retry_at {
            if crate::environment::now() < after {
                return Ok(pending(false, Some(after.min(self.deadline))));
            }
            self.retry_at = None;
        }
        for _ in 0..budget {
            if c.fixed.is_none() {
                match ring.register_inbound(c.control.file.clone(), c.admission.clone()) {
                    Ok(f) => c.fixed = Some(f),
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        let after = crate::environment::now() + ADMISSION_RETRY;
                        self.retry_at = Some(after);
                        return Ok(pending(false, Some(after.min(self.deadline))));
                    }
                    Err(e) => return Err(e),
                }
                continue;
            }
            if let Some(t) = &mut self.ticket {
                let Some(result) = ring.take_bytes(t)? else {
                    return Ok(pending(false, Some(self.deadline)));
                };
                self.ticket = None;
                let n = transfer(result.result?, SCRATCH_SIZE - c.used)?;
                if let Some(timeout) = self.first_byte_timeout.take() {
                    self.deadline = crate::environment::now()
                        .checked_add(timeout)
                        .ok_or_else(|| invalid("deadline overflow"))?;
                }
                c.used += n;
                c.input = Some(result.resource);
                continue;
            }
            let bytes = c.input.as_ref().unwrap();
            let end = header_end(&bytes[..c.used], &mut self.scan);
            let parsed = match end {
                Some(end) => Some(parse(bytes, end)),
                None if c.used == SCRATCH_SIZE => Some(Err(431)),
                None => None,
            };
            if let Some(parsed) = parsed {
                let mut connection = self.connection.take().unwrap();
                match parsed {
                    Ok(metadata) => {
                        let core = RequestCore {
                            metric_traffic: None,
                            connection,
                            metadata,
                            end: end.unwrap(),
                            deadline: Deadline::absolute(self.deadline),
                            identity: Rc::new(()),
                        };
                        return Ok(Progress::Ready(if core.metadata.head {
                            Request::Head(HeadRequest(core))
                        } else {
                            Request::Get(GetRequest(core))
                        }));
                    }
                    Err(status) => {
                        connection.used = 0;
                        let headers: &[(&str, &[u8])] = if status == 405 {
                            &[("Allow", b"GET, HEAD")]
                        } else {
                            &[]
                        };
                        let head = ResponseHead::new(status, Some(0), headers)?.close();
                        let response = Response::new(
                            connection,
                            Rc::new(()),
                            Deadline::absolute(self.deadline),
                            true,
                            head,
                        )?;
                        self.rejection = Some(SendingHeadHeaders(HeaderSend::new(response)));
                        return Ok(pending(true, Some(self.deadline)));
                    }
                }
            }
            let bytes = c.input.take().unwrap();
            match ring.recv_bytes_range(
                c.fixed.as_ref().unwrap().clone().into(),
                bytes,
                c.used..SCRATCH_SIZE,
            ) {
                Ok(t) => self.ticket = Some(t.cancel_on_drop()),
                Err(r) if r.error.kind() == io::ErrorKind::WouldBlock => {
                    c.input = Some(r.resource);
                    let after = crate::environment::now() + ADMISSION_RETRY;
                    self.retry_at = Some(after);
                    return Ok(pending(false, Some(after.min(self.deadline))));
                }
                Err(r) => return Err(r.error),
            }
        }
        Ok(pending(true, Some(self.deadline)))
    }
    /// Consuming cancellation also works by dropping this handle.
    pub fn cancel(self) {
        drop(self);
    }
}

struct RequestCore {
    metric_traffic: Option<crate::metrics::Traffic>,
    connection: Connection,
    metadata: Metadata,
    end: usize,
    deadline: Deadline,
    identity: Rc<()>,
}
impl RequestCore {
    fn respond(mut self, head: ResponseHead<'_>) -> io::Result<Response> {
        check_deadline(self.deadline.get())?;
        self.connection
            .input
            .as_mut()
            .unwrap()
            .copy_within(self.end..self.connection.used, 0);
        self.connection.used -= self.end;
        let mut head = head;
        head.close |= self.metadata.close;
        let mut response = Response::new(
            self.connection,
            self.identity,
            self.deadline,
            self.metadata.head,
            head,
        )?;
        response.metric_traffic = self.metric_traffic;
        Ok(response)
    }
}

/// Each variant owns the only response capability for its request.
#[must_use]
pub enum Request {
    Get(GetRequest),
    Head(HeadRequest),
}
impl Request {
    pub(crate) fn set_metric_traffic(&mut self, traffic: crate::metrics::Traffic) {
        match self {
            Self::Get(r) => r.0.metric_traffic = Some(traffic),
            Self::Head(r) => r.0.metric_traffic = Some(traffic),
        }
    }
    fn core(&self) -> &RequestCore {
        match self {
            Self::Get(r) => &r.0,
            Self::Head(r) => &r.0,
        }
    }
    pub fn target(&self) -> &str {
        self.core().target()
    }
    pub fn headers(&self) -> Headers<'_> {
        self.core().headers()
    }
    pub fn deadline(&self) -> Instant {
        self.core().deadline.get()
    }
    pub(crate) fn response_deadline(&self) -> Deadline {
        self.core().deadline.clone()
    }
    pub(crate) fn cap_deadline(&mut self, deadline: Instant) {
        let core = match self {
            Self::Get(r) => &mut r.0,
            Self::Head(r) => &mut r.0,
        };
        core.deadline.cap(deadline);
    }
    pub fn connection_id(&self) -> ConnectionId {
        self.core().connection.connection_id()
    }
}
#[must_use]
pub struct GetRequest(RequestCore);
#[must_use]
pub struct HeadRequest(RequestCore);
impl RequestCore {
    fn target(&self) -> &str {
        std::str::from_utf8(
            self.metadata
                .target
                .slice(self.connection.input.as_ref().unwrap()),
        )
        .unwrap()
    }
    fn headers(&self) -> Headers<'_> {
        Headers {
            bytes: self.connection.input.as_ref().unwrap(),
            headers: &self.metadata.headers[..self.metadata.count],
        }
    }
}
macro_rules! request_views {
    ($ty:ident) => {
        impl $ty {
            pub fn target(&self) -> &str {
                self.0.target()
            }
            pub fn headers(&self) -> Headers<'_> {
                self.0.headers()
            }
            pub fn deadline(&self) -> Instant {
                self.0.deadline.get()
            }
            pub fn connection_id(&self) -> ConnectionId {
                self.0.connection.connection_id()
            }
        }
    };
}
request_views!(GetRequest);
request_views!(HeadRequest);
impl GetRequest {
    pub fn respond(self, head: ResponseHead<'_>) -> io::Result<SendingGetHeaders> {
        Ok(SendingGetHeaders(HeaderSend::new(self.0.respond(head)?)))
    }
}
impl HeadRequest {
    pub fn respond(self, head: ResponseHead<'_>) -> io::Result<SendingHeadHeaders> {
        Ok(SendingHeadHeaders(HeaderSend::new(self.0.respond(head)?)))
    }
}

/// Validated final response metadata, borrowed only until `respond` returns.
/// Ordinary statuses require a length; 204 forbids it, 205 requires zero, and
/// 304 permits an optional representation length. HEAD suppresses all body bytes.
pub struct ResponseHead<'a> {
    status: u16,
    length: Option<u64>,
    headers: &'a [(&'a str, &'a [u8])],
    close: bool,
}
impl<'a> ResponseHead<'a> {
    pub fn new(
        status: u16,
        content_length: Option<u64>,
        headers: &'a [(&'a str, &'a [u8])],
    ) -> io::Result<Self> {
        if !(200..=599).contains(&status)
            || match status {
                204 => content_length.is_some(),
                205 => content_length != Some(0),
                304 => false,
                _ => content_length.is_none(),
            }
        {
            return Err(invalid("invalid response status or length"));
        }
        // Reserve fields for transport-generated Content-Length and Connection.
        if headers.len() > MAX_HEADERS - 2 {
            return Err(invalid("too many response headers"));
        }
        for &(name, v) in headers {
            if name.is_empty() || !name.bytes().all(token) || !value(v) {
                return Err(invalid("invalid response header"));
            }
            if [
                "content-length",
                "transfer-encoding",
                "connection",
                "proxy-connection",
                "keep-alive",
                "upgrade",
                "trailer",
                "te",
            ]
            .iter()
            .any(|n| name.eq_ignore_ascii_case(n))
            {
                return Err(invalid("transport-owned response header"));
            }
        }
        // Exact size bound, including a possible subsequent close() call.
        let digits = content_length.map_or(0, |n| if n == 0 { 1 } else { n.ilog10() as usize + 1 });
        let mut size = 17usize
            + 19
            + if content_length.is_some() {
                18 + digits
            } else {
                0
            };
        for &(name, v) in headers {
            size = size
                .checked_add(name.len())
                .and_then(|n| n.checked_add(v.len()))
                .and_then(|n| n.checked_add(4))
                .ok_or_else(|| invalid("response headers overflow"))?;
        }
        if size > SCRATCH_SIZE {
            return Err(invalid("response headers exceed 8 KiB"));
        }
        Ok(Self {
            status,
            length: content_length,
            headers,
            close: false,
        })
    }
    pub fn close(mut self) -> Self {
        self.close = true;
        self
    }
    fn encode(&self, mut out: &mut [u8]) -> io::Result<usize> {
        let capacity = out.len();
        // Empty reason phrase is legal and avoids a status-name table.
        write!(out, "HTTP/1.1 {} \r\n", self.status)?;
        if let Some(n) = self.length {
            write!(out, "Content-Length: {n}\r\n")?;
        }
        if self.close {
            out.write_all(b"Connection: close\r\n")?;
        }
        for &(name, value) in self.headers {
            out.write_all(name.as_bytes())?;
            out.write_all(b": ")?;
            out.write_all(value)?;
            out.write_all(b"\r\n")?;
        }
        out.write_all(b"\r\n")?;
        Ok(capacity - out.len())
    }
}

/// A nonempty published-buffer subrange. Construction returns ownership on error.
#[must_use]
pub struct BodyChunk {
    buffer: crate::cache::CachedValue,
    range: Range<usize>,
}
impl BodyChunk {
    pub fn new(buffer: Buffer, range: Range<usize>) -> Result<Self, Rejected<Buffer>> {
        if range.start >= range.end || range.end > buffer.as_slice().len() {
            return Err(Rejected {
                error: invalid("invalid body chunk range"),
                resource: buffer,
            });
        }
        Ok(Self {
            buffer: crate::cache::CachedValue::Buffer(buffer),
            range,
        })
    }
    pub fn value(
        buffer: crate::cache::CachedValue,
        range: Range<usize>,
    ) -> Result<Self, Rejected<crate::cache::CachedValue>> {
        if range.is_empty() || range.end > buffer.len() {
            return Err(Rejected {
                error: invalid("invalid file body range"),
                resource: buffer,
            });
        }
        Ok(Self { buffer, range })
    }
    pub fn into_parts(self) -> (crate::cache::CachedValue, Range<usize>) {
        (self.buffer, self.range)
    }
}

struct Response {
    metric_traffic: Option<crate::metrics::Traffic>,
    connection: Connection,
    identity: Rc<()>,
    deadline: Deadline,
    remaining: u64,
    close: bool,
    header_len: usize,
    // A fixed notification window: no allocation per body chunk.
    notifications: [Option<Ticket<SendZc>>; ZC_WINDOW],
    // Lazily allocated, response-scoped file streaming resources. A chunk only
    // finishes after its pipe is empty and its splice has completed.
    file: Option<FileSend>,
}
impl Response {
    fn new(
        mut connection: Connection,
        identity: Rc<()>,
        deadline: Deadline,
        head: bool,
        metadata: ResponseHead<'_>,
    ) -> io::Result<Self> {
        let header_len = metadata.encode(connection.output.as_mut().unwrap())?;
        Ok(Self {
            metric_traffic: None,
            connection,
            identity,
            deadline,
            remaining: if head || matches!(metadata.status, 204 | 304) {
                0
            } else {
                metadata.length.unwrap_or(0)
            },
            close: metadata.close,
            header_len,
            notifications: std::array::from_fn(|_| None),
            file: None,
        })
    }
    fn reap(&mut self, ring: &mut Ring) -> io::Result<()> {
        for t in &mut self.notifications {
            if let Some(ticket) = t
                && let Some(c) = ring.take_send_zc(ticket)?
            {
                c.result?;
                *t = None;
            }
        }
        Ok(())
    }
    fn has_capacity(&self) -> bool {
        self.notifications.iter().any(Option::is_none)
    }
    fn is_drained(&self) -> bool {
        self.notifications.iter().all(Option::is_none)
    }
    fn complete(self) -> Completed {
        debug_assert_eq!(self.remaining, 0);
        debug_assert!(self.is_drained());
        Completed {
            connection: if self.close {
                None
            } else {
                Some(self.connection)
            },
            identity: self.identity,
        }
    }
}

struct HeaderSend {
    response: Option<Response>,
    ticket: Option<Ticket<Bytes>>,
    sent: usize,
}
impl HeaderSend {
    fn new(response: Response) -> Self {
        Self {
            response: Some(response),
            ticket: None,
            sent: 0,
        }
    }
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Response>> {
        let result = self.poll_inner(ring, budget);
        if result.is_err() {
            self.response.take();
            self.ticket.take();
        }
        result
    }
    fn poll_inner(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Response>> {
        let r = self
            .response
            .as_mut()
            .ok_or_else(|| invalid("headers already finished"))?;
        r.connection.check(ring, r.deadline.get())?;
        for _ in 0..budget {
            if self.sent == r.header_len {
                return Ok(Progress::Ready(self.response.take().unwrap()));
            }
            if let Some(t) = &mut self.ticket {
                let Some(c) = ring.take_bytes(t)? else {
                    return Ok(pending(false, Some(r.deadline.get())));
                };
                self.ticket = None;
                self.sent += transfer(c.result?, r.header_len - self.sent)?;
                r.deadline.sent()?;
                r.connection.output = Some(c.resource);
            } else {
                let bytes = r.connection.output.take().unwrap();
                match ring.send_bytes_range(
                    r.connection.fixed.as_ref().unwrap().clone().into(),
                    bytes,
                    self.sent..r.header_len,
                ) {
                    Ok(t) => self.ticket = Some(t.cancel_on_drop()),
                    Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => {
                        r.connection.output = Some(e.resource);
                        break;
                    }
                    Err(e) => return Err(e.error),
                }
            }
        }
        Ok(pending(true, Some(r.deadline.get())))
    }
}
#[must_use]
pub struct SendingHeadHeaders(HeaderSend);
#[must_use]
pub struct SendingGetHeaders(HeaderSend);
impl SendingHeadHeaders {
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Completed>> {
        Ok(match self.0.poll(ring, budget)? {
            Progress::Pending(w) => Progress::Pending(w),
            Progress::Ready(r) => Progress::Ready(r.complete()),
        })
    }
    pub fn cancel(self) {
        drop(self);
    }
}
impl SendingGetHeaders {
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<BodyProgress>> {
        Ok(match self.0.poll(ring, budget)? {
            Progress::Pending(w) => Progress::Pending(w),
            Progress::Ready(r) => Progress::Ready(if r.remaining == 0 {
                BodyProgress::Done(r.complete())
            } else {
                BodyProgress::More(BodyWriter(r))
            }),
        })
    }
    pub fn cancel(self) {
        drop(self);
    }
}

#[must_use]
pub enum BodyProgress {
    More(BodyWriter),
    Done(Completed),
}
/// Capacity for one additional chunk. Dropping before the declared length closes.
#[must_use]
pub struct BodyWriter(Response);
impl BodyWriter {
    pub fn remaining(&self) -> u64 {
        self.0.remaining
    }
    // Return both affine capabilities without a heap allocation on rejection.
    #[allow(clippy::result_large_err)]
    pub fn send(self, chunk: BodyChunk) -> Result<SendingBody, Rejected<(Self, BodyChunk)>> {
        let validation = check_deadline(self.0.deadline.get()).and_then(|_| {
            if (chunk.range.len() as u64) > self.0.remaining {
                Err(invalid("chunk exceeds remaining body"))
            } else {
                Ok(())
            }
        });
        if let Err(error) = validation {
            return Err(Rejected {
                error,
                resource: (self, chunk),
            });
        }
        Ok(SendingBody {
            response: Some(self.0),
            chunk: Some(chunk),
            ticket: None,
            file_owner: None,
            small: None,
        })
    }
}

/// Sends a chunk, handles short transfers, and drains notifications as needed.
#[must_use]
pub struct SendingBody {
    small: Option<Ticket<Bytes>>,
    response: Option<Response>,
    chunk: Option<BodyChunk>,
    ticket: Option<Ticket<SendZc>>,
    file_owner: Option<Rc<FileChunk>>,
}
// One allocation per chunk, shared by every short fill/drain and the ring.
// Keep this outside the response's reusable FileSend: completed extents must
// be released even while the response keeps its pipes and slab descriptor.
struct FileChunk {
    _value: crate::allocator::FileValue,
    source: crate::uring::File,
    read: crate::uring::File,
    write: crate::uring::File,
}
struct FileSend {
    source: crate::allocator::FileSource,
    read: crate::uring::File,
    write: crate::uring::File,
    queued: usize,
    ticket: Option<Ticket<crate::uring::Splice>>,
    draining: bool,
    readiness: Option<Ticket<crate::uring::Control>>,
}
impl SendingBody {
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<BodyProgress>> {
        let result = self.poll_inner(ring, budget);
        if result.is_err() {
            self.response.take();
            self.chunk.take();
            self.ticket.take();
            self.file_owner.take();
            self.small.take();
        }
        result
    }
    fn poll_inner(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<BodyProgress>> {
        let r = self
            .response
            .as_mut()
            .ok_or_else(|| invalid("body send already finished"))?;
        r.connection.check(ring, r.deadline.get())?;
        for _ in 0..budget {
            r.reap(ring)?;
            if let Some(ticket) = &mut self.small {
                let Some(done) = ring.take_bytes(ticket)? else {
                    return Ok(pending(false, Some(r.deadline.get())));
                };
                self.small = None;
                let chunk = self.chunk.as_mut().unwrap();
                let n = transfer(done.result?, chunk.range.len())?;
                r.deadline.sent()?;
                chunk.range.start += n;
                r.remaining -= n as u64;
                if chunk.range.is_empty() {
                    self.chunk = None;
                }
                continue;
            }
            if let Some(t) = &self.ticket {
                let Some(result) = ring.send_result(t)? else {
                    return Ok(pending(false, Some(r.deadline.get())));
                };
                let chunk = self.chunk.as_mut().unwrap();
                let n = transfer(result?, chunk.range.len())?;
                r.deadline.sent()?;
                chunk.range.start += n;
                r.remaining -= n as u64;
                let slot = r.notifications.iter_mut().find(|t| t.is_none()).unwrap();
                *slot = self.ticket.take();
                if chunk.range.is_empty() {
                    self.chunk = None;
                }
                continue;
            }
            if self.chunk.is_none() {
                if r.remaining == 0 && r.is_drained() {
                    return Ok(Progress::Ready(BodyProgress::Done(
                        self.response.take().unwrap().complete(),
                    )));
                }
                if r.remaining != 0 && r.has_capacity() {
                    return Ok(Progress::Ready(BodyProgress::More(BodyWriter(
                        self.response.take().unwrap(),
                    ))));
                }
                return Ok(pending(false, Some(r.deadline.get())));
            }
            if !r.has_capacity() {
                return Ok(pending(false, Some(r.deadline.get())));
            }
            let chunk = self.chunk.as_ref().unwrap();
            if let crate::cache::CachedValue::Metadata(record) = &chunk.buffer {
                match ring.send_bytes_range(
                    r.connection.fixed.as_ref().unwrap().clone().into(),
                    Box::new(record.to_bytes()),
                    chunk.range.clone(),
                ) {
                    Ok(ticket) => {
                        ring.measure_send(&ticket, r.metric_traffic);
                        self.small = Some(ticket.cancel_on_drop());
                    }
                    Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => break,
                    Err(e) => return Err(e.error),
                }
                continue;
            }
            if let crate::cache::CachedValue::File(value) = &chunk.buffer {
                if r.file.is_none() {
                    let (read, write) = crate::uring::File::pipe()?;
                    r.file = Some(FileSend {
                        source: crate::allocator::FileSource::new(value)?,
                        read,
                        write,
                        queued: 0,
                        ticket: None,
                        draining: false,
                        readiness: None,
                    });
                }
                let file = r.file.as_mut().unwrap();
                if self.file_owner.is_none() {
                    self.file_owner = Some(Rc::new(FileChunk {
                        _value: value.clone(),
                        source: file.source.descriptor(value)?,
                        read: file.read.clone(),
                        write: file.write.clone(),
                    }));
                }
                if let Some(ticket) = &mut file.readiness {
                    let Some(completion) = ring.take_control(ticket)? else {
                        return Ok(pending(false, Some(r.deadline.get())));
                    };
                    completion.result?;
                    file.readiness = None;
                }
                if let Some(ticket) = &mut file.ticket {
                    let Some(result) = ring.take_splice(ticket)? else {
                        return Ok(pending(false, Some(r.deadline.get())));
                    };
                    file.ticket = None;
                    match result {
                        Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                            let descriptor = if file.draining {
                                r.connection.fixed.as_ref().unwrap().clone().into()
                            } else {
                                file.write.clone().into()
                            };
                            match ring.poll_fd(descriptor, crate::uring::Readiness::Writable) {
                                Ok(ticket) => file.readiness = Some(ticket.cancel_on_drop()),
                                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                                    return Ok(pending(true, Some(r.deadline.get())));
                                }
                                Err(error) => return Err(error),
                            }
                            return Ok(pending(false, Some(r.deadline.get())));
                        }
                        result => {
                            let n = transfer(
                                result?,
                                if file.draining {
                                    file.queued
                                } else {
                                    chunk.range.len()
                                },
                            )?;
                            if file.draining {
                                r.deadline.sent()?;
                                file.queued -= n;
                                let chunk = self.chunk.as_mut().unwrap();
                                chunk.range.start += n;
                                r.remaining -= n as u64;
                                if chunk.range.is_empty() {
                                    debug_assert_eq!(file.queued, 0);
                                    self.chunk = None;
                                    self.file_owner = None;
                                }
                            } else {
                                file.queued = n;
                            }
                        }
                    }
                    continue;
                }
                let owner = self.file_owner.as_ref().unwrap().clone();
                file.draining = file.queued != 0;
                let result = if file.draining {
                    ring.splice_owned(
                        owner,
                        |owner| &owner.read,
                        r.connection.fixed.as_ref().unwrap().clone().into(),
                        None,
                        file.queued,
                    )
                } else {
                    let output = owner.write.clone().into();
                    ring.splice_owned(
                        owner,
                        |owner| &owner.source,
                        output,
                        Some(crate::uring::FileOffset::new(
                            value.offset() + chunk.range.start as u64,
                        )?),
                        chunk.range.len(),
                    )
                };
                match result {
                    Ok(ticket) => {
                        if file.draining {
                            ring.measure_send(&ticket, r.metric_traffic);
                        }
                        file.ticket = Some(ticket.cancel_on_drop());
                    }
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                    Err(error) => return Err(error),
                }
                continue;
            }
            let crate::cache::CachedValue::Buffer(buffer) = &chunk.buffer else {
                unreachable!()
            };
            // Clone only the immutable handle. The ring owns its clone through
            // the notification; ours permits resubmission after a short send.
            match ring.send_zc(
                r.connection.fixed.as_ref().unwrap().clone().into(),
                buffer.clone(),
                BufferRange::new(chunk.range.clone())?,
            ) {
                Ok(t) => {
                    ring.measure_send(&t, r.metric_traffic);
                    self.ticket = Some(t.cancel_on_drop());
                }
                Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => break,
                Err(e) => return Err(e.error),
            }
        }
        Ok(pending(true, Some(r.deadline.get())))
    }
    pub fn cancel(self) {
        drop(self);
    }
}

/// A fully transmitted response, including terminal zero-copy notifications.
#[must_use]
pub struct Completed {
    connection: Option<Connection>,
    identity: Rc<()>,
}
impl Completed {
    pub fn recycle(self) -> Option<Connection> {
        self.connection
    }
}

/// Worker-local task factory and poller; no allocation or dynamic dispatch is
/// required. `start` constructs state; asynchronous/fallible work happens in poll.
/// Poll must make bounded progress even with budget 1. Pending work must include
/// application wakeups/deadlines as well as the HTTP exchange's work. Returning an
/// error aborts only this connection; expected HTTP errors are ordinary responses.
/// Dropped tasks must release their application resources without blocking.
pub trait Handler {
    type Task;
    fn start(&mut self, request: Request) -> Self::Task;
    fn poll(
        &mut self,
        task: &mut Self::Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<Completed>>;
}

#[derive(Clone, Debug)]
pub struct Config {
    pub max_connections: NonZeroUsize,
    /// Time to the first byte on a new or recycled connection.
    pub idle_timeout: Duration,
    /// Absolute time from observing the first request byte to the first successful
    /// response send, including headers and handler admission. Request trickle
    /// does not renew it. Read-ahead starts this clock on recycling.
    pub request_timeout: Duration,
    /// Maximum inactivity after response transmission starts. Successful socket
    /// sends renew this deadline; progressing responses have no total time cap.
    pub streaming_timeout: Duration,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            max_connections: NonZeroUsize::new(1024).unwrap(),
            idle_timeout: Duration::from_secs(30),
            request_timeout: Duration::from_secs(30),
            streaming_timeout: Duration::from_secs(30),
        }
    }
}

struct Slot<T> {
    control: Rc<Control>,
    receiving: Option<ReceivingRequest>,
    task: Option<T>,
    identity: Option<Rc<()>>,
    deadline: Instant,
    response_deadline: Option<Deadline>,
}
impl<T> Slot<T> {
    fn new(connection: Connection, config: &Config) -> io::Result<Self> {
        let control = connection.control.clone();
        let has_bytes = connection.used != 0;
        let deadline = crate::environment::now()
            .checked_add(if has_bytes {
                config.request_timeout
            } else {
                config.idle_timeout
            })
            .ok_or_else(|| invalid("deadline overflow"))?;
        let mut receiving = connection.receive(deadline);
        if !has_bytes {
            receiving.first_byte_timeout = Some(config.request_timeout);
        }
        Ok(Self {
            control,
            receiving: Some(receiving),
            task: None,
            identity: None,
            deadline,
            response_deadline: None,
        })
    }
}

// At most four budget-1 polls per visit, including receive-to-handler transitions.
// A runnable slot yields at this limit or caller budget exhaustion; a sleeper
// yields immediately. Keep admission and sweep accounting in units of visits.
const SLOT_QUANTUM: usize = 4;

/// Bounded round-robin transport scheduler. Call from Application::poll; merge
/// its Work with other services. Per-client errors are isolated. Errors returned
/// by this poll indicate listener/setup failures. A completion must match the
/// exact request being polled, not merely the same connection.
pub struct Server<H: Handler> {
    listener: Option<Listener>,
    handler: H,
    config: Config,
    slots: VecDeque<Slot<H::Task>>,
    ring: Option<Rc<Identity>>,
    until_accept: usize,
    // Work accumulated across a bounded sweep, so a small caller budget cannot
    // starve either admission or an older connection, or omit a sleeper deadline.
    sweep_left: usize,
    completion_epoch: u64,
    work: Work,
}
impl<H: Handler> Server<H> {
    pub fn new(listener: Listener, handler: H, config: Config) -> Self {
        Self {
            listener: Some(listener),
            handler,
            slots: VecDeque::new(),
            config,
            ring: None,
            until_accept: 0,
            sweep_left: 0,
            completion_epoch: 0,
            work: Work::default(),
        }
    }
    pub fn handler(&self) -> &H {
        &self.handler
    }
    pub fn handler_mut(&mut self) -> &mut H {
        &mut self.handler
    }
    pub fn connections(&self) -> usize {
        self.slots.len()
    }
    /// Stop accepting new sockets while allowing already accepted requests to
    /// finish. Idle keep-alive sockets are closed at their next completion.
    pub fn retire(&mut self) {
        self.listener.take();
    }

    /// Process shutdown admits no additional requests, even on an accepted idle
    /// or partially parsed keep-alive socket. Active tasks retain all IO leases.
    pub fn begin_drain(&mut self) {
        self.retire();
        self.slots.retain(|slot| {
            if slot.task.is_some() {
                true
            } else {
                slot.control.close();
                false
            }
        });
        self.sweep_left = 0;
    }

    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
        if let Some(id) = &self.ring {
            if !Rc::ptr_eq(id, ring.identity()) {
                return Err(invalid("server belongs to another ring"));
            }
        } else {
            self.ring = Some(ring.identity().clone());
        }
        if self.sweep_left == 0 {
            // Begin at admission then visit every existing connection once.
            self.until_accept = 0;
            self.sweep_left = self.slots.len() + 1;
            self.work = Work::default();
            self.completion_epoch = ring.completion_epoch();
        }
        let mut remaining = budget;
        while remaining != 0 {
            remaining -= 1;
            if self.until_accept == 0 {
                self.until_accept = self.slots.len() + 1;
                if self.listener.is_some() && self.slots.len() < self.config.max_connections.get() {
                    match self.listener.as_mut().unwrap().poll_accept(ring, 1)? {
                        Progress::Pending(w) => merge(&mut self.work, w),
                        Progress::Ready(c) => {
                            self.slots.push_back(Slot::new(c, &self.config)?);
                            self.sweep_left += 1;
                            self.work.runnable = true;
                        }
                    }
                }
            } else if let Some(mut slot) = self.slots.pop_front() {
                self.until_accept -= 1;
                let mut result = self.poll_slot(&mut slot, ring);
                for _ in 1..SLOT_QUANTUM {
                    if remaining == 0
                        || !matches!(&result, Ok(SlotProgress::Pending(w)) if w.runnable)
                    {
                        break;
                    }
                    remaining -= 1;
                    result = self.poll_slot(&mut slot, ring);
                }
                // Only the final poll describes this slot's outstanding work:
                // intermediate runnable transitions and renewed deadlines have
                // already been consumed. Other slots' sweep work stays merged.
                match result {
                    Ok(SlotProgress::Pending(w)) => {
                        merge(&mut self.work, w);
                        self.slots.push_back(slot);
                    }
                    Ok(SlotProgress::Complete(c)) => {
                        if let Some(connection) = c.recycle().filter(|_| self.listener.is_some()) {
                            self.slots.push_back(Slot::new(connection, &self.config)?);
                        }
                        // Revisit admission/newly recycled work before sleeping.
                        self.work.runnable = true;
                    }
                    Err(_) => {
                        slot.control.close();
                        self.work.runnable = true;
                    }
                }
            }
            self.sweep_left -= 1;
            if self.sweep_left == 0 {
                self.work.runnable |= self.completion_epoch != ring.completion_epoch();
                return Ok(self.work);
            }
        }
        Ok(Work {
            runnable: true,
            deadline: self.work.deadline,
        })
    }

    fn poll_slot(&mut self, slot: &mut Slot<H::Task>, ring: &mut Ring) -> io::Result<SlotProgress> {
        check_deadline(
            slot.response_deadline
                .as_ref()
                .map_or(slot.deadline, Deadline::get),
        )?;
        if let Some(receiving) = &mut slot.receiving {
            let progress = receiving.poll(ring, 1)?;
            slot.deadline = receiving.deadline;
            match progress {
                Progress::Pending(w) => return Ok(SlotProgress::Pending(w)),
                Progress::Ready(request) => {
                    let deadline = request.response_deadline();
                    deadline
                        .0
                        .inactivity
                        .set(Some(self.config.streaming_timeout));
                    slot.response_deadline = Some(deadline);
                    slot.identity = Some(request.core().identity.clone());
                    slot.task = Some(self.handler.start(request));
                    slot.receiving = None;
                    return Ok(SlotProgress::Pending(Work {
                        runnable: true,
                        deadline: Some(slot.response_deadline.as_ref().unwrap().get()),
                    }));
                }
            }
        }
        match self.handler.poll(slot.task.as_mut().unwrap(), ring, 1)? {
            Progress::Pending(mut work) => {
                merge(
                    &mut work,
                    Work {
                        runnable: false,
                        deadline: Some(slot.response_deadline.as_ref().unwrap().get()),
                    },
                );
                Ok(SlotProgress::Pending(work))
            }
            Progress::Ready(c) => {
                check_deadline(slot.response_deadline.as_ref().unwrap().get())?;
                if !Rc::ptr_eq(&c.identity, slot.identity.as_ref().unwrap()) {
                    return Err(invalid("handler returned another request's completion"));
                }
                Ok(SlotProgress::Complete(c))
            }
        }
    }

    /// Stops admission and closes all sockets before dropping handler tasks. The
    /// driver must subsequently drain the ring. Repeated calls are harmless.
    pub fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
        if self
            .ring
            .as_ref()
            .is_some_and(|id| !Rc::ptr_eq(id, ring.identity()))
        {
            return Err(invalid("server belongs to another ring"));
        }
        self.stop();
        Ok(())
    }
    fn stop(&mut self) {
        self.listener.take();
        for slot in &self.slots {
            slot.control.close();
        }
        self.slots.clear();
    }
}
impl<H: Handler> Drop for Server<H> {
    fn drop(&mut self) {
        self.stop();
    }
}
enum SlotProgress {
    Pending(Work),
    Complete(Completed),
}

/// Resolve only after the caller has evaluated validators such as If-Range, and
/// only for GET. Malformed, duplicate and multipart Range fields select Full.
#[derive(Debug, PartialEq, Eq)]
pub enum RangeSelection {
    Full,
    Partial(ResolvedRange),
    Unsatisfiable,
}

/// Nonempty half-open interval bounded by the representation length.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ResolvedRange {
    start: u64,
    end: u64,
    total: u64,
}
impl ResolvedRange {
    pub fn start(self) -> u64 {
        self.start
    }
    pub fn end(self) -> u64 {
        self.end
    }
    pub fn len(self) -> u64 {
        self.end - self.start
    }
    pub fn is_empty(self) -> bool {
        false
    }
    /// Allocation-free formatting of the Content-Range field value.
    pub fn content_range(self) -> impl std::fmt::Display {
        struct Display(ResolvedRange);
        impl std::fmt::Display for Display {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                write!(
                    f,
                    "bytes {}-{}/{}",
                    self.0.start,
                    self.0.end - 1,
                    self.0.total
                )
            }
        }
        Display(self)
    }
}
pub fn resolve_range(headers: Headers<'_>, representation_length: u64) -> RangeSelection {
    let mut fields = headers
        .iter()
        .filter(|(n, _)| n.eq_ignore_ascii_case("range"));
    let Some((_, bytes)) = fields.next() else {
        return RangeSelection::Full;
    };
    if fields.next().is_some() {
        return RangeSelection::Full;
    }
    range(trim(bytes), representation_length)
}
fn range(bytes: &[u8], total: u64) -> RangeSelection {
    use RangeSelection::*;
    let Some((unit, spec)) = split_byte(bytes, b'=') else {
        return Full;
    };
    if !unit.eq_ignore_ascii_case(b"bytes") {
        return Full;
    }
    let Some((first, last)) = split_byte(spec, b'-') else {
        return Full;
    };
    let interval = if first.is_empty() {
        let Ok(suffix) = decimal(last) else {
            return Full;
        };
        if suffix == 0 || total == 0 {
            return Unsatisfiable;
        }
        total.saturating_sub(suffix)..total
    } else {
        let Ok(start) = decimal(first) else {
            return Full;
        };
        let end = if last.is_empty() {
            total
        } else {
            let Ok(last) = decimal(last) else {
                return Full;
            };
            if last < start {
                return Full;
            }
            last.saturating_add(1).min(total)
        };
        if start >= total {
            return Unsatisfiable;
        }
        start..end
    };
    Partial(ResolvedRange {
        start: interval.start,
        end: interval.end,
        total,
    })
}

// Keep parsing on stable Rust without allocating strings.
fn split_byte(bytes: &[u8], byte: u8) -> Option<(&[u8], &[u8])> {
    let n = bytes.iter().position(|&b| b == byte)?;
    Some((&bytes[..n], &bytes[n + 1..]))
}

#[cfg(test)]
include!(concat!(env!("CARGO_MANIFEST_DIR"), "/tests/http/server.rs"));

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/http/server_scheduling.rs"
));
