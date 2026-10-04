//! Racer policy adapters for the standalone fixed-length HTTP implementation.
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::VerifiedPage;
use crate::model::PageSlice;
use crate::admission::ResourceClass;
use crate::runtime::cooperative_turn as yield_once;

use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use flow_control::pipe::PipeLease;
use flow_control::pipe::PipePool;
use http1::MessageHead;
use std::io;
use std::path::PathBuf;
use std::rc::Rc;
use std::task::Waker;
use std::time::Duration;
use uring_runtime::reactor::Descriptor;
use uring_runtime::reactor::IoBuffer;
use uring_runtime::reactor::SendBuffer;
use uring_runtime::reactor::SocketAddress;

pub const MAX_HEAD_BYTES: usize = 32 * 1024;
pub fn new_pipe_pool(
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
) -> PipePool<AdmissionPolicy> {
    let waiter_limit = admission.policy().limits().queue_entries.get();
    PipePool::new(
        admission,
        ResourceClass::Pipe,
        ResourceClass::RequestContext,
        waiter_limit,
    )
}
pub(crate) fn acquire_wait<'a>(
    pool: &'a PipePool<AdmissionPolicy>,
    scope: &'a RequestScope,
) -> Operation<'a, PipeLease<AdmissionPolicy>> {
    Box::pin(pool.acquire_wait(
        || scope.check(),
        || {
            let cancellation = scope.cancellation.subscribe()?;
            Ok(move |waker: &Waker| cancellation.register(waker))
        },
    ))
}
pub struct RacerOpaque;
impl http1::Opaque for RacerOpaque {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}
pub type Codec = http1::Codec<RacerOpaque>;
pub type ConnectionLease = http1::connection::ConnectionLease<HttpContext>;
pub type OwnedBuffer = http1::connection::OwnedBuffer<HttpContext>;
pub type HttpIo = http1::connection::HttpIo<HttpContext>;
pub type HttpPool = http1::connection::HttpPool<HttpContext>;

/// HTTP's application context, separate from the generic quota authority.
pub struct HttpContext(pub(crate) Rc<flow_control::Quotas<AdmissionPolicy>>);
impl http1::connection::Context for HttpContext {
    type Error = Error;
    type Scope = RequestScope;
    type Budget = crate::runtime::AdmissionBudget;
    type Reactor = Reactor;
    type Charge = flow_control::Charge<AdmissionPolicy>;
    type Slot = ConnectionReservation;
    type Opaque = RacerOpaque;
    type State = State;
    type Endpoint = Endpoint;
    fn charge(&self, bytes: usize) -> Result<flow_control::Charge<AdmissionPolicy>> {
        self.0
            .reserve(None, ResourceClass::RequestContext, bytes)
            .map_err(Into::into)
    }
    fn outbound_slot(&self) -> Result<ConnectionReservation> {
        crate::admission::reserve_connection(&self.0, ResourceClass::OutboundConnection)
    }
    fn stopped(&self) -> bool {
        self.0.is_stopped()
    }
}
#[derive(Default)]
pub struct State {
    pub(crate) peer_admission: Option<std::sync::Arc<crate::peer::Permit>>,
    pub(crate) peer_response_verified: bool,
    pub(crate) connect_failure: Option<Rc<std::cell::Cell<bool>>>,
    #[cfg(test)]
    pub(crate) relay_fallback: bool,
    #[cfg(test)]
    pub(crate) relay_fallback_at: Option<usize>,
    pub(crate) relay_peer: Option<Box<ConnectionLease>>,
    pub(crate) relay_pipe: Option<flow_control::pipe::PipeLease<AdmissionPolicy>>,
    pub(crate) relay_context: Option<flow_control::Charge<AdmissionPolicy>>,
    pub(crate) relay_reservation: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
    pub(crate) session: Option<crate::peer::protocol::Session>,
    pub(crate) control_reservation: Option<flow_control::Charge<AdmissionPolicy>>,
}
impl http1::connection::State<Error> for State {
    fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
        match &mut self.session {
            Some(session) => session.admit(head),
            None => Ok(head),
        }
    }
    fn sign(&mut self, head: MessageHead) -> Result<MessageHead> {
        match &mut self.session {
            Some(session) => session.sign(head),
            None => Ok(head),
        }
    }
    fn finished(&mut self) {
        if self.peer_response_verified {
            if let Some(permit) = &self.peer_admission {
                permit.observe(crate::peer::Outcome::Verified);
            }
            self.peer_response_verified = false;
        }
    }
    fn connect_failed(&self, errno: Option<i32>) {
        if peer_connect_failure(errno) {
            if let Some(failure) = &self.connect_failure {
                failure.set(true);
            }
            if let Some(peer) = &self.peer_admission {
                peer.observe(crate::peer::Outcome::PeerFailure);
            }
        }
    }
    fn attach(&mut self, checkout: Self) {
        self.relay_reservation = checkout.relay_reservation;
        self.peer_admission = checkout.peer_admission;
        self.connect_failure = checkout.connect_failure;
    }
    fn idle(mut self) -> Self {
        Self {
            session: self.session.take(),
            ..Self::default()
        }
    }
}
pub fn from_accepted(
    fd: Descriptor,
    admission: &flow_control::Quotas<AdmissionPolicy>,
) -> Result<ConnectionLease> {
    from_reserved(
        fd,
        crate::admission::reserve_connection(admission, ResourceClass::IngressConnection)?,
    )
}
pub(crate) fn from_reserved(
    fd: Descriptor,
    reservation: ConnectionReservation,
) -> Result<ConnectionLease> {
    ConnectionLease::from_reserved(fd, reservation, State::default())
}
pub(crate) fn install_session(
    connection: &mut ConnectionLease,
    session: crate::peer::protocol::Session,
) -> Result<()> {
    if connection.state().session.is_some() || connection.closing() {
        return Err(Error::Unauthorized);
    }
    connection.state_mut().session = Some(session);
    connection.begin_io();
    Ok(())
}
pub fn new_io(
    reactor: Rc<Reactor>,
    codec: Codec,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    body_limit: u64,
) -> HttpIo {
    HttpIo::new(
        reactor,
        codec,
        Rc::new(HttpContext(admission)),
        body_limit,
        body_limit,
    )
}
pub fn client_io(
    reactor: Rc<Reactor>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
) -> HttpIo {
    HttpIo::new(
        reactor,
        Codec::new(
            admission
                .policy()
                .limits()
                .header_bytes
                .get()
                .min(MAX_HEAD_BYTES),
        ),
        Rc::new(HttpContext(admission)),
        crate::model::PAGE_BYTES + 16,
        i64::MAX as u64,
    )
}
#[derive(Clone, Debug, Eq, Hash, PartialEq, Ord, PartialOrd)]
pub enum Endpoint {
    Unix(PathBuf),
    Origin {
        cache: racer_control_wire::CacheId,
        path: PathBuf,
    },
    Peer(String),
}
impl http1::connection::Endpoint<Error> for Endpoint {
    fn address(&self) -> Result<SocketAddress> {
        Ok(match self {
            Self::Unix(path) | Self::Origin { path, .. } => SocketAddress::Unix(path.clone()),
            Self::Peer(value) => {
                SocketAddress::Inet(value.parse().map_err(|_| Error::InvalidConfiguration)?)
            }
        })
    }
    fn capacity(&self, config: &http1::connection::PoolConfig) -> usize {
        match self {
            Self::Peer(_) => config.per_endpoint,
            _ => config.secondary_cap,
        }
    }
    fn priority_headroom(&self) -> usize {
        usize::from(matches!(self, Self::Origin { .. }))
    }
    fn allocation(&self) -> usize {
        match self {
            Self::Unix(path) => path.as_os_str().len(),
            Self::Origin { cache, path } => cache.0.len() + path.as_os_str().len(),
            Self::Peer(value) => value.len(),
        }
    }
}
pub fn new_pool(
    reactor: Rc<Reactor>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    per_endpoint: usize,
) -> HttpPool {
    pool_with_limits(
        reactor,
        admission,
        per_endpoint,
        256,
        Duration::from_secs(30),
    )
}
pub fn pool_with_limits(
    reactor: Rc<Reactor>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    per_endpoint: usize,
    max_endpoints: usize,
    idle_timeout: Duration,
) -> HttpPool {
    HttpPool::new(
        reactor.clone(),
        Rc::new(HttpContext(admission.clone())),
        http1::connection::PoolConfig {
            per_endpoint,
            secondary_cap: per_endpoint,
            max_endpoints,
            idle_timeout,
            waiter_cap: admission.policy().limits().queue_entries.get(),
            tcp_nodelay: false,
        },
    )
}
pub(crate) fn checkout_peer<'a>(
    pool: &'a HttpPool,
    endpoint: &'a Endpoint,
    relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
    peer: Option<std::sync::Arc<crate::peer::Permit>>,
    failure: Option<Rc<std::cell::Cell<bool>>>,
    scope: &'a RequestScope,
) -> Operation<'a, ConnectionLease> {
    pool.checkout_with_state(
        endpoint,
        State {
            relay_reservation: relay,
            peer_admission: peer,
            connect_failure: failure,
            ..State::default()
        },
        scope,
    )
}
fn peer_connect_failure(errno: Option<i32>) -> bool {
    matches!(
        errno,
        Some(libc::ECONNREFUSED | libc::ECONNRESET | libc::EPIPE)
    )
}

// Limit both syscall size and work in one executor turn, even for a writable peer.
const SEND_CHUNK_BYTES: usize = 64 * 1024;
const SEND_BUDGET_BYTES: usize = 256 * 1024;
const SEND_BUDGET_CALLS: usize = 32;
/// One current final send, never an admission to release its CQE-owned leases.
#[derive(Default)]
pub(crate) struct FinalSend(std::cell::Cell<Option<bool>>);
impl FinalSend {
    pub(crate) fn provisional_release(&self) -> Result<()> {
        if self.0.get() != Some(false) {
            return Err(Error::InvalidRequest);
        }
        self.0.set(Some(true));
        Ok(())
    }
}
/// A fixed immutable view owns the full admitted page through the send fence.
/// It deliberately implements no receive or mutable-buffer capability.
struct PageSendRange {
    page: VerifiedPage,
    range: std::ops::Range<usize>,
}
impl PageSendRange {
    fn new(page: VerifiedPage, range: std::ops::Range<usize>) -> Result<Self> {
        if page.bytes().get(range.clone()).is_none() {
            return Err(Error::InvalidRange);
        }
        Ok(Self { page, range })
    }
}
// SAFETY: fixed immutable view retains the complete verified plaintext page owner.
unsafe impl SendBuffer for PageSendRange {
    type Error = Error;
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(&self.page.bytes()[self.range.clone()])
    }
}
enum DeliveryBuffer {
    Pipe(OwnedBuffer),
    Page(PageSendRange),
}
// SAFETY: both variants retain stable immutable backing through completion.
unsafe impl SendBuffer for DeliveryBuffer {
    type Error = Error;
    fn send_bytes(&self) -> Result<&[u8]> {
        match self {
            Self::Pipe(buffer) => buffer.send_bytes(),
            Self::Page(buffer) => buffer.send_bytes(),
        }
    }
}
pub struct Delivery {
    metrics: crate::telemetry::Metrics,
    pipes: Rc<PipePool<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    stall_timeout: Duration,
}
/// A reader pins an immutable page and a separately admitted pipe for its lifetime.
/// Each reader has its own staging pipe and socket-accepted cursor.
pub struct ReaderLease {
    _active: ::telemetry::Lease,
    page: VerifiedPage,
    pipe: PipeLease<AdmissionPolicy>,
    slice: PageSlice,
    sent: usize,
}
impl ReaderLease {
    pub fn slice(&self) -> PageSlice {
        self.slice
    }
    pub fn bytes_sent(&self) -> usize {
        self.sent
    }
    pub fn remaining(&self) -> usize {
        self.slice.length as usize - self.sent
    }
    fn try_send(&mut self, connection: &Descriptor, copying: bool) -> io::Result<usize> {
        let start = self.slice.offset as usize + self.sent;
        let count = self.remaining().min(SEND_CHUNK_BYTES);
        let bytes = &self.page.bytes()[start..start + count];
        if copying {
            return connection.try_send(bytes);
        }
        // Refill only an empty pipe: a partial splice leaves the exact suffix.
        if self.pipe.buffered() == 0 && self.pipe.try_write(bytes)? == 0 {
            return Err(io::ErrorKind::WriteZero.into());
        }
        self.pipe.try_splice_descriptor(connection, count)
    }
}
impl Delivery {
    pub fn new(
        pipes: Rc<PipePool<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        stall_timeout: Duration,
    ) -> Self {
        Self {
            metrics: crate::telemetry::Metrics::default(),
            pipes,
            reactor,
            stall_timeout,
        }
    }
    pub(crate) fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    pub fn attach(&self, page: VerifiedPage, slice: PageSlice) -> Result<ReaderLease> {
        validate_slice(&page, slice)?;
        self.attach_reserved(page, slice, self.pipes.acquire()?)
    }
    pub(crate) fn admit<'a>(
        &'a self,
        scope: &'a RequestScope,
    ) -> Operation<'a, PipeLease<AdmissionPolicy>> {
        acquire_wait(&self.pipes, scope)
    }
    pub(crate) fn attach_reserved(
        &self,
        page: VerifiedPage,
        slice: PageSlice,
        pipe: PipeLease<AdmissionPolicy>,
    ) -> Result<ReaderLease> {
        validate_slice(&page, slice)?;
        Ok(ReaderLease {
            _active: self
                .metrics
                .lease(crate::telemetry::Gauge::ActiveDeliveries)?,
            page,
            pipe,
            slice,
            sent: 0,
        })
    }
    /// Own the complete HTTP connection until the slice is sent, then return it
    /// for the next slice. Require a previously sent head with a known body length
    /// and update HTTP framing. Errors or abandonment cannot return it to a pool.
    pub fn finish_to<'a>(
        &'a self,
        reader: ReaderLease,
        connection: ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.finish_to_inner(reader, connection, scope, false, None)
    }
    /// Client body writes are bounded by lack of socket progress, not total
    /// object duration. Peer writes retain their absolute operation deadline.
    #[cfg(test)]
    pub(crate) fn finish_progressing<'a>(
        &'a self,
        reader: ReaderLease,
        connection: ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.finish_to_inner(reader, connection, scope, true, None)
    }
    pub(crate) fn finish_subscription<'a>(
        &'a self,
        reader: ReaderLease,
        connection: ConnectionLease,
        scope: &'a RequestScope,
        final_send: &'a FinalSend,
    ) -> Operation<'a, ConnectionLease> {
        self.finish_to_inner(reader, connection, scope, true, Some(final_send))
    }
    fn finish_to_inner<'a>(
        &'a self,
        mut reader: ReaderLease,
        mut connection: ConnectionLease,
        scope: &'a RequestScope,
        progressing: bool,
        final_send: Option<&'a FinalSend>,
    ) -> Operation<'a, ConnectionLease> {
        // Also protect abandonment before the returned future's first poll.
        connection.begin_io();
        Box::pin(async move {
            if !progressing {
                scope.check()?;
            }
            let remaining = connection.send_remaining().ok_or(Error::InvalidRequest)?;
            let length = reader.remaining() as u64;
            if length > remaining {
                return Err(Error::InvalidRequest);
            }
            let socket = connection.socket();
            socket.validate_socket()?;
            drop(socket);
            let mut stalled_at = uring_runtime::environment::now();
            let mut copying = false;
            let mut budget = 0;
            let mut calls = 0;
            while reader.remaining() != 0 {
                if !progressing {
                    scope.check()?;
                }
                let mut send_scope = scope.clone();
                let stall_deadline = stalled_at
                    .checked_add(self.stall_timeout)
                    .ok_or(Error::InvalidConfiguration)?;
                send_scope.deadline.0 = if progressing {
                    stall_deadline
                } else {
                    send_scope.deadline.0.min(stall_deadline)
                };
                send_scope.check()?;
                let sent = match reader.try_send(&connection.socket(), copying) {
                    Ok(sent) => {
                        if copying {
                            let _ = self
                                .metrics
                                .record(crate::telemetry::Event::DeliveryDirectBytes, sent as u64);
                        }
                        sent
                    }
                    Err(error) if !copying && splice_unsupported(&error) => {
                        copying = true;
                        continue;
                    }
                    Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                        yield_once().await;
                        continue;
                    }
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        // Readiness currently retains only an FD, not connection
                        // admission. Instead submit one owned send on backpressure.
                        // Drain the first copied pipe into accounted storage. Later
                        // sends own immutable page views, avoiding repeated staging
                        // allocation, copy, and wipe. The reactor also retains the
                        // entire reader/pipe/connection lease until its final fence.
                        let buffer = if copying {
                            let start = reader.slice.offset as usize + reader.sent;
                            let count = reader.remaining().min(SEND_CHUNK_BYTES);
                            DeliveryBuffer::Page(PageSendRange::new(
                                reader.page.clone(),
                                start..start + count,
                            )?)
                        } else {
                            let count = reader.pipe.buffered();
                            let mut buffer =
                                OwnedBuffer::new(&HttpContext(self.pipes.quotas().clone()), count)?;
                            match reader.pipe.try_read(buffer.bytes_mut()?) {
                                Ok(read) if read == count && count != 0 => {}
                                Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                                    yield_once().await;
                                    continue;
                                }
                                _ => return Err(Error::Io),
                            }
                            let _ = self
                                .metrics
                                .record(crate::telemetry::Event::DeliveryPipeDrain, 1);
                            DeliveryBuffer::Pipe(buffer)
                        };
                        let count = buffer.send_bytes()?.len();
                        if let Some(state) = final_send {
                            state.0.set((count == reader.remaining()).then_some(false));
                        }
                        let completion = self
                            .reactor
                            .send(
                                connection.socket(),
                                buffer,
                                (reader, connection),
                                &send_scope,
                            )
                            .await?;
                        (reader, connection) = completion.lease;
                        if let Some(state) = final_send {
                            let released = state.0.replace(None) == Some(true);
                            if released && completion.bytes != count {
                                return Err(Error::InvalidRequest);
                            }
                        }
                        if completion.bytes > count {
                            return Err(Error::Io);
                        }
                        // The pipe is empty and the immutable page reconstructs
                        // any unsent suffix. Avoid another write/splice/drain on
                        // the next backpressured chunk; keep the owned-send fence.
                        copying = true;
                        let _ = self.metrics.record(
                            crate::telemetry::Event::DeliveryDirectBytes,
                            completion.bytes as u64,
                        );
                        completion.bytes
                    }
                    Err(_) => return Err(Error::Io),
                };
                if sent == 0 || sent > reader.remaining() {
                    return Err(Error::Io);
                }
                reader.sent += sent;
                stalled_at = uring_runtime::environment::now();
                budget += sent;
                calls += 1;
                if (budget >= SEND_BUDGET_BYTES || calls >= SEND_BUDGET_CALLS)
                    && reader.remaining() != 0
                {
                    yield_once().await;
                    budget = 0;
                    calls = 0;
                }
            }
            connection.consume_sent(usize::try_from(length).map_err(|_| Error::InvalidRequest)?)?;
            Ok(connection)
        })
    }
}
fn validate_slice(page: &VerifiedPage, slice: PageSlice) -> Result<()> {
    let end = (slice.offset as usize)
        .checked_add(slice.length as usize)
        .ok_or(Error::InvalidRange)?;
    if slice.page != page.page().number || end > page.bytes().len() {
        return Err(Error::InvalidRange);
    }
    Ok(())
}
fn splice_unsupported(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::model::RequestId;
    use http1::Header;
    use http1::StartLine;
    use std::future::Future;
    use std::io::Read;
    use std::io::Write;
    use std::net::TcpListener;
    use std::os::unix::net::UnixStream;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;
    use uring_runtime::reactor::IoBuffer;

    mod delivery {
        use crate::http::*;
        use crate::memory::VerifiedBytes;
        use racer_control_wire::CacheId;
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::PageId;
        use crate::model::PageNumber;
        use crate::model::RequestId;
        use crate::admission::ResourceClass;
        use crate::model::StrongEtag;
        use crate::admission::AdmissionPolicy;
        use crate::runtime::Cancellation;
        use uring_runtime::deadline::Deadline;
        use crate::runtime::Reactor;
        use std::io::Read;
        use std::os::fd::AsRawFd;
        use std::os::unix::net::UnixStream;
        use std::sync::Arc;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Instant;
        fn admission(pipes: usize) -> Rc<flow_control::Quotas<AdmissionPolicy>> {
            let small = std::num::NonZeroUsize::new(8).unwrap();
            let bytes = std::num::NonZeroUsize::new(32 * 1024 * 1024).unwrap();
            Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::config::Limits {
                    plaintext_bytes: bytes,
                    ciphertext_bytes: bytes,
                    dirty_bytes: bytes,
                    registered_bytes: bytes,
                    request_context_bytes: bytes,
                    flights: small,
                    waiters_per_flight: small,
                    queue_entries: small,
                    connections_per_neighbor: small,
                    client_connections: small,
                    pipes: std::num::NonZeroUsize::new(pipes).unwrap(),
                    range_window_pages: small,
                    header_bytes: small,
                    cached_rankings: small,
                    cached_paths: small,
                    retained_snapshots: small,
                    metadata_entries: small,
                    relay_transfers: small,
                },
            )))
        }
        fn setup(
            pipes: usize,
            stall: Duration,
        ) -> (
            Rc<flow_control::Quotas<AdmissionPolicy>>,
            Rc<Reactor>,
            Delivery,
        ) {
            let admission = admission(pipes);
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let pool = Rc::new(crate::http::new_pipe_pool(admission.clone()));
            (
                admission,
                reactor.clone(),
                Delivery::new(pool, reactor, stall),
            )
        }
        fn scope() -> RequestScope {
            RequestScope {
                body_deadlines: None,
                request: RequestId([0; 16]),
                deadline: Deadline(Instant::now() + Duration::from_secs(5)),
                cancellation: Cancellation::new().unwrap(),
            }
        }
        fn page(admission: &flow_control::Quotas<AdmissionPolicy>, bytes: Vec<u8>) -> VerifiedPage {
            VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page: PageId {
                        version: ObjectVersion {
                            object: ObjectId {
                                cache: CacheId("delivery-test".into()),
                                key: CacheKey([7; 32]),
                            },
                            etag: StrongEtag::test_value("v1"),
                        },
                        number: PageNumber(0),
                    },
                    reservation: admission
                        .reserve(None, ResourceClass::Plaintext, bytes.len().max(1))
                        .unwrap(),
                    bytes: bytes.into(),
                }),
            }
        }
        fn slice(offset: u32, length: u32) -> PageSlice {
            PageSlice {
                page: PageNumber(0),
                offset,
                length,
            }
        }
        fn connection(
            socket: Descriptor,
            admission: &flow_control::Quotas<AdmissionPolicy>,
            length: u64,
        ) -> ConnectionLease {
            let mut connection = crate::http::from_accepted(socket, admission).unwrap();
            connection.set_framing(None, Some(length), false);
            connection
        }
        fn blocked_reader(
            admission: &flow_control::Quotas<AdmissionPolicy>,
            delivery: &Delivery,
        ) -> (
            ReaderLease,
            ConnectionLease,
            UnixStream,
            std::sync::Weak<VerifiedBytes>,
        ) {
            let page = page(admission, vec![0x5a; 512 * 1024]);
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
            let (socket, peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            (
                reader,
                connection(socket.into(), admission, 512 * 1024),
                peer,
                weak,
            )
        }
        fn assert_pending<T>(operation: &mut Operation<'_, T>) {
            assert!(
                operation
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                    .is_pending()
            );
        }
        fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
            let start = Instant::now();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            loop {
                if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                    return result;
                }
                assert!(
                    start.elapsed() < Duration::from_secs(5),
                    "delivery did not complete"
                );
                reactor.poll_budgeted(64).unwrap();
                std::thread::sleep(Duration::from_millis(1));
            }
        }
        fn small_send_buffer(socket: &UnixStream) {
            socket.set_nonblocking(true).unwrap();
            let size: libc::c_int = 4096;
            // SAFETY: the option pointer refers to a live, correctly sized integer.
            assert_eq!(
                unsafe {
                    libc::setsockopt(
                        socket.as_raw_fd(),
                        libc::SOL_SOCKET,
                        libc::SO_SNDBUF,
                        (&size as *const libc::c_int).cast(),
                        std::mem::size_of_val(&size) as libc::socklen_t,
                    )
                },
                0
            );
        }
        fn thread_cpu() -> Duration {
            let mut time = libc::timespec {
                tv_sec: 0,
                tv_nsec: 0,
            };
            // SAFETY: clock_gettime initializes the live timespec on success.
            assert_eq!(
                unsafe { libc::clock_gettime(libc::CLOCK_THREAD_CPUTIME_ID, &mut time) },
                0
            );
            Duration::new(time.tv_sec as u64, time.tv_nsec as u32)
        }
        #[test]
        fn page_send_range_is_a_stable_admitted_immutable_subrange() {
            let (admission, _, _) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"prefix-selected-suffix".to_vec());
            let weak = Arc::downgrade(&page.inner);
            let pointer = page.bytes()[7..].as_ptr();
            for range in [8..7, 0..23, usize::MAX..usize::MAX] {
                assert!(matches!(
                    PageSendRange::new(page.clone(), range),
                    Err(Error::InvalidRange)
                ));
            }
            let empty = PageSendRange::new(page.clone(), 22..22).unwrap();
            assert!(empty.send_bytes().unwrap().is_empty());
            drop(empty);
            let view = PageSendRange::new(page, 7..15).unwrap();
            assert_eq!(view.send_bytes().unwrap(), b"selected");
            assert_eq!(view.send_bytes().unwrap().as_ptr(), pointer);
            assert_eq!(admission.used(ResourceClass::Plaintext), 22);
            let moved = Box::new(view);
            assert_eq!(moved.send_bytes().unwrap().as_ptr(), pointer);
            drop(moved);
            assert!(weak.upgrade().is_none());
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        }
        #[test]
        fn copying_http_send_keeps_full_admission_until_failure_fence() {
            for failure in ["abandon", "cancel", "disconnect", "deadline", "drain"] {
                let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
                let page = page(&admission, vec![0x5a; 512 * 1024]);
                let weak = Arc::downgrade(&page.inner);
                let reader = delivery.attach(page, slice(7, 500 * 1024)).unwrap();
                let (socket, mut peer) = UnixStream::pair().unwrap();
                small_send_buffer(&socket);
                peer.set_nonblocking(true).unwrap();
                let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
                connection.set_framing(None, Some(500 * 1024), false);
                let mut scope = scope();
                if failure == "deadline" {
                    scope.deadline = Deadline(Instant::now() + Duration::from_millis(100));
                }
                let mut operation = delivery.finish_to(reader, connection, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                assert_eq!(weak.strong_count(), 1, "first send owns a pipe drain");
                let start = Instant::now();
                while weak.strong_count() == 1 {
                    assert!(start.elapsed() < Duration::from_secs(1));
                    loop {
                        match peer.read(&mut [0; 8192]) {
                            Ok(0) => panic!("unexpected EOF"),
                            Ok(_) => {}
                            Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                            Err(error) => panic!("{error}"),
                        }
                    }
                    reactor.poll_budgeted(64).unwrap();
                    assert!(operation.as_mut().poll(&mut cx).is_pending());
                }
                assert_eq!(weak.strong_count(), 2, "reader and immutable send view");
                assert_eq!(reactor.in_flight(), 1);
                assert_eq!(admission.used(ResourceClass::Plaintext), 512 * 1024);
                assert_eq!(admission.used(ResourceClass::Connection), 1);
                assert!(matches!(
                    delivery.pipes.acquire(),
                    Err(flow_control::Error::Overloaded)
                ));
                match failure {
                    "abandon" => {
                        drop(operation);
                        assert_eq!(weak.strong_count(), 2);
                        assert_eq!(admission.used(ResourceClass::Plaintext), 512 * 1024);
                        assert_eq!(admission.used(ResourceClass::Connection), 1);
                        assert!(matches!(
                            delivery.pipes.acquire(),
                            Err(flow_control::Error::Overloaded)
                        ));
                        drive(&reactor, reactor.drain()).unwrap();
                    }
                    "drain" => {
                        drive(&reactor, reactor.drain()).unwrap();
                        assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
                    }
                    _ => {
                        let expected = match failure {
                            "cancel" => {
                                scope.cancel().unwrap();
                                Error::Cancelled
                            }
                            "disconnect" => {
                                drop(peer);
                                Error::Io
                            }
                            _ => Error::DeadlineExceeded,
                        };
                        assert!(
                            matches!(drive(&reactor, operation), Err(error) if error == expected)
                        );
                    }
                }
                assert_eq!(reactor.in_flight(), 0);
                assert!(weak.upgrade().is_none());
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Connection), 0);
                assert!(delivery.pipes.acquire().is_ok());
            }
        }
        /// Real TCP, bounded send queue, and receiver pacing force repeated owned sends.
        /// Time only sender future/reactor turns, excluding receiver reads, validation,
        /// and pacing. This is thread CPU, not total kernel/worker CPU or throughput.
        /// Run with --ignored --nocapture --test-threads=1 before/after production edits.
        #[test]
        #[ignore = "local TCP delivery CPU benchmark"]
        fn loopback_backpressure_thread_cpu() {
            use std::net::TcpListener;
            use std::net::TcpStream;
            const LENGTH: usize = 512 * 1024;
            const PAGES: usize = 1024;
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(5));
            let bytes: Vec<u8> = (0..LENGTH).map(|i| (i % 251) as u8).collect();
            let page = page(&admission, bytes.clone());
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (socket, _) = listener.accept().unwrap();
            socket.set_nonblocking(true).unwrap();
            socket.set_nodelay(true).unwrap();
            peer.set_nonblocking(true).unwrap();
            let size: libc::c_int = 4096;
            // SAFETY: the option pointer refers to a live, correctly sized integer.
            assert_eq!(
                unsafe {
                    libc::setsockopt(
                        socket.as_raw_fd(),
                        libc::SOL_SOCKET,
                        libc::SO_SNDBUF,
                        (&size as *const libc::c_int).cast(),
                        std::mem::size_of_val(&size) as libc::socklen_t,
                    )
                },
                0
            );
            let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
            let mut scratch = [0; 64 * 1024];
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for sample in 0..4 {
                let wall = Instant::now();
                let mut cpu = Duration::ZERO;
                let mut pending_turns = 0;
                let drains = delivery
                    .metrics
                    .count(crate::telemetry::Event::DeliveryPipeDrain);
                for _ in 0..PAGES {
                    let scope = scope();
                    connection.set_framing(
                        connection.receive_remaining(),
                        Some(LENGTH as u64),
                        false,
                    );
                    let reader = delivery
                        .attach(page.clone(), slice(0, LENGTH as u32))
                        .unwrap();
                    let mut operation = delivery.finish_to(reader, connection, &scope);
                    let mut completed = None;
                    let mut received = 0;
                    while completed.is_none() || received != LENGTH {
                        assert!(wall.elapsed() < Duration::from_secs(60));
                        let start = thread_cpu();
                        reactor.poll_budgeted(64).unwrap();
                        if completed.is_none() {
                            if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                                completed = Some(result.unwrap());
                            } else if reactor.in_flight() != 0 {
                                pending_turns += 1;
                            }
                        }
                        cpu += thread_cpu() - start;
                        loop {
                            match peer.read(&mut scratch) {
                                Ok(0) => panic!("unexpected EOF"),
                                Ok(count) => {
                                    assert_eq!(
                                        &scratch[..count],
                                        &bytes[received..received + count]
                                    );
                                    received += count;
                                    let quick_ack: libc::c_int = 1;
                                    // SAFETY: live TCP socket and correctly sized option.
                                    assert_eq!(
                                        unsafe {
                                            libc::setsockopt(
                                                peer.as_raw_fd(),
                                                libc::IPPROTO_TCP,
                                                libc::TCP_QUICKACK,
                                                (&quick_ack as *const libc::c_int).cast(),
                                                std::mem::size_of_val(&quick_ack)
                                                    as libc::socklen_t,
                                            )
                                        },
                                        0
                                    );
                                }
                                Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                                Err(error) => panic!("{error}"),
                            }
                        }
                        // Let TCP and completion processing progress without charging
                        // busy polling or receiver pacing to the sender CPU sample.
                        std::thread::sleep(Duration::from_micros(50));
                    }
                    connection = completed.unwrap();
                    assert_eq!(connection.send_remaining(), Some(0));
                    assert_eq!(reactor.in_flight(), 0);
                }
                let drains = delivery
                    .metrics
                    .count(crate::telemetry::Event::DeliveryPipeDrain)
                    - drains;
                assert!(drains > 0 && pending_turns > PAGES);
                eprintln!(
                    "delivery_tcp sample={sample} warmup={} bytes={} sender_cpu_ms={:.3} cpu_ns_per_byte={:.4} wall_ms={} pipe_drains={drains} pending_turns={pending_turns}",
                    sample == 0,
                    LENGTH * PAGES,
                    cpu.as_secs_f64() * 1000.0,
                    cpu.as_nanos() as f64 / (LENGTH * PAGES) as f64,
                    wall.elapsed().as_millis()
                );
            }
        }
        #[test]
        fn independent_slices_share_page_until_last_reader_and_return_socket() {
            let (admission, reactor, delivery) = setup(2, Duration::from_secs(1));
            let page = page(&admission, b"0123456789".to_vec());
            let weak = Arc::downgrade(&page.inner);
            let first = delivery.attach(page.clone(), slice(1, 3)).unwrap();
            let second = delivery.attach(page.clone(), slice(6, 4)).unwrap();
            assert_eq!(first.bytes_sent(), 0);
            assert_eq!(first.remaining(), 3);
            assert_eq!(second.slice(), slice(6, 4));
            drop(page);
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let scope = scope();
            let socket = drive(
                &reactor,
                delivery.finish_to(first, connection(socket.into(), &admission, 7), &scope),
            )
            .unwrap();
            assert!(weak.upgrade().is_some());
            let socket = drive(&reactor, delivery.finish_to(second, socket, &scope)).unwrap();
            assert!(weak.upgrade().is_none());
            let mut bytes = [0; 7];
            peer.read_exact(&mut bytes).unwrap();
            assert_eq!(&bytes, b"1236789");
            drop(socket);
            assert!(delivery.pipes.acquire().is_ok());
        }
        #[test]
        fn fallback_after_partial_splice_restarts_at_accepted_cursor() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"0123456789".to_vec());
            let mut reader = delivery.attach(page, slice(1, 8)).unwrap();
            let (socket, mut peer) = UnixStream::pair().unwrap();
            socket.set_nonblocking(true).unwrap();
            reader.pipe.try_write(b"12345678").unwrap();
            reader.sent = reader.pipe.try_splice_to(&socket, 3).unwrap();
            assert_eq!(reader.sent, 3);
            assert_eq!(reader.pipe.buffered(), 5);
            // A blocking FD is deliberately unsupported by the splice API, but the
            // send fallback still cannot block and must not resend the accepted prefix.
            socket.set_nonblocking(false).unwrap();
            let socket = drive(
                &reactor,
                delivery.finish_to(reader, connection(socket.into(), &admission, 5), &scope()),
            )
            .unwrap();
            drop(socket);
            let mut bytes = Vec::new();
            peer.read_to_end(&mut bytes).unwrap();
            assert_eq!(bytes, b"12345678");
        }
        #[test]
        fn tcp_splice_delivers_selected_slice_and_returns_connection() {
            use std::net::TcpListener;
            use std::net::TcpStream;
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (socket, _) = listener.accept().unwrap();
            socket.set_nonblocking(true).unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
            let page = page(&admission, b"prefix-selected-suffix".to_vec());
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(7, 8)).unwrap();
            let connection = connection(socket.into(), &admission, 8);
            drop(drive(&reactor, delivery.finish_to(reader, connection, &scope())).unwrap());
            assert!(weak.upgrade().is_none());
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            assert!(delivery.pipes.acquire().is_ok());
            let mut bytes = Vec::new();
            peer.read_to_end(&mut bytes).unwrap();
            assert_eq!(bytes, b"selected");
        }
        #[test]
        fn invalid_ranges_and_missing_framing_fail_without_sending() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"abc".to_vec());
            for invalid in [
                slice(4, 0),
                slice(2, 2),
                slice(u32::MAX, u32::MAX),
                PageSlice {
                    page: PageNumber(1),
                    offset: 0,
                    length: 1,
                },
            ] {
                assert!(matches!(
                    delivery.attach(page.clone(), invalid),
                    Err(Error::InvalidRange)
                ));
            }
            let scope = scope();
            let reader = delivery.attach(page.clone(), slice(0, 3)).unwrap();
            let (socket, _peer) = UnixStream::pair().unwrap();
            let unframed = crate::http::from_accepted(socket.into(), &admission).unwrap();
            assert!(matches!(
                drive(&reactor, delivery.finish_to(reader, unframed, &scope)),
                Err(Error::InvalidRequest)
            ));
            let reader = delivery.attach(page, slice(3, 0)).unwrap();
            let (socket, _peer) = UnixStream::pair().unwrap();
            let unframed = crate::http::from_accepted(socket.into(), &admission).unwrap();
            assert!(matches!(
                drive(&reactor, delivery.finish_to(reader, unframed, &scope)),
                Err(Error::InvalidRequest)
            ));
        }
        #[test]
        fn framed_delivery_and_empty_slices_work() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"abc".to_vec());
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let reader = delivery.attach(page.clone(), slice(1, 2)).unwrap();
            drop(
                drive(
                    &reactor,
                    delivery.finish_to(reader, connection(socket.into(), &admission, 2), &scope()),
                )
                .unwrap(),
            );
            let mut bytes = Vec::new();
            peer.read_to_end(&mut bytes).unwrap();
            assert_eq!(bytes, b"bc");
            let (socket, _peer) = UnixStream::pair().unwrap();
            let reader = delivery.attach(page, slice(3, 0)).unwrap();
            assert!(
                drive(
                    &reactor,
                    delivery.finish_to(reader, connection(socket.into(), &admission, 0), &scope())
                )
                .is_ok()
            );
        }
        #[test]
        fn canceled_expired_and_disconnected_readers_release_resources() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            for failure in [Error::Cancelled, Error::DeadlineExceeded, Error::Io] {
                let mut scope = scope();
                let (socket, peer) = UnixStream::pair().unwrap();
                let page = page(&admission, b"abc".to_vec());
                let weak = Arc::downgrade(&page.inner);
                let reader = delivery.attach(page, slice(0, 3)).unwrap();
                match failure {
                    Error::Cancelled => scope.cancel().unwrap(),
                    Error::DeadlineExceeded => scope.deadline = Deadline(Instant::now()),
                    Error::Io => drop(peer),
                    _ => unreachable!(),
                }
                assert!(
                    matches!(drive(&reactor, delivery.finish_to(reader, connection(socket.into(), &admission, 3), &scope)), Err(error) if error == failure)
                );
                assert!(weak.upgrade().is_none());
                assert!(delivery.pipes.acquire().is_ok());
            }
        }
        #[test]
        fn rejects_nonsockets_and_datagrams() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"abc".to_vec());
            let reader = delivery.attach(page.clone(), slice(0, 3)).unwrap();
            let file = std::fs::File::open("/dev/null").unwrap();
            assert!(matches!(
                drive(
                    &reactor,
                    delivery.finish_to(reader, connection(file.into(), &admission, 3), &scope())
                ),
                Err(Error::InvalidRequest)
            ));
            let reader = delivery.attach(page, slice(0, 3)).unwrap();
            let (datagram, _peer) = std::os::unix::net::UnixDatagram::pair().unwrap();
            assert!(matches!(
                drive(
                    &reactor,
                    delivery.finish_to(
                        reader,
                        connection(datagram.into(), &admission, 3),
                        &scope()
                    )
                ),
                Err(Error::InvalidRequest)
            ));
        }
        #[test]
        fn partial_sends_wait_and_deliver_exactly_once_without_blocking() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
            let bytes: Vec<u8> = (0..512 * 1024).map(|i| (i % 251) as u8).collect();
            let page = page(&admission, bytes.clone());
            let reader = delivery.attach(page, slice(0, bytes.len() as u32)).unwrap();
            let (socket, mut peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            peer.set_nonblocking(true).unwrap();
            let scope = scope();
            let mut operation = delivery.finish_to(
                reader,
                connection(socket.into(), &admission, bytes.len() as u64),
                &scope,
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            let mut received = Vec::new();
            let mut scratch = [0; 8192];
            let start = Instant::now();
            let mut complete = false;
            while received.len() != bytes.len() || !complete {
                assert!(start.elapsed() < Duration::from_secs(5));
                loop {
                    match peer.read(&mut scratch) {
                        Ok(0) => break,
                        Ok(count) => received.extend_from_slice(&scratch[..count]),
                        Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                        Err(error) => panic!("{error}"),
                    }
                }
                reactor.poll_budgeted(64).unwrap();
                if !complete {
                    if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                        result.unwrap();
                        complete = true;
                    }
                }
            }
            assert_eq!(received, bytes);
        }
        #[test]
        fn stalled_reader_times_out_without_canceling_another_reader() {
            let (admission, reactor, delivery) = setup(2, Duration::from_millis(25));
            let page = page(&admission, vec![0x5a; 512 * 1024]);
            let weak = Arc::downgrade(&page.inner);
            let slow = delivery.attach(page.clone(), slice(0, 512 * 1024)).unwrap();
            let fast = delivery.attach(page, slice(5, 3)).unwrap();
            let (socket, _peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            let scope = scope();
            assert!(matches!(
                drive(
                    &reactor,
                    delivery.finish_to(
                        slow,
                        connection(socket.into(), &admission, 512 * 1024),
                        &scope
                    )
                ),
                Err(Error::DeadlineExceeded)
            ));
            assert_eq!(scope.check(), Ok(()));
            assert!(weak.upgrade().is_some());
            let (socket, mut peer) = UnixStream::pair().unwrap();
            drive(
                &reactor,
                delivery.finish_to(fast, connection(socket.into(), &admission, 3), &scope),
            )
            .unwrap();
            let mut bytes = [0; 3];
            peer.read_exact(&mut bytes).unwrap();
            assert_eq!(bytes, [0x5a; 3]);
            assert!(weak.upgrade().is_none());
        }
        #[test]
        fn abandonment_and_cancellation_of_pending_send_release_copied_page() {
            for cancel in [false, true] {
                let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
                let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
                let scope = scope();
                let mut operation = delivery.finish_to(reader, connection, &scope);
                assert_pending(&mut operation);
                assert!(weak.upgrade().is_some());
                assert!(matches!(
                    delivery.pipes.acquire(),
                    Err(flow_control::Error::Overloaded)
                ));
                if cancel {
                    scope.cancel().unwrap();
                    assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
                } else {
                    drop(operation);
                    assert!(weak.upgrade().is_some());
                    drive(&reactor, reactor.drain()).unwrap();
                }
                assert!(weak.upgrade().is_none());
                assert!(delivery.pipes.acquire().is_ok());
                assert_eq!(reactor.in_flight(), 0);
            }
        }
        #[test]
        fn http_delivery_preserves_framing_and_owned_connection() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"abc".to_vec());
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
            // Equivalent to successful head I/O: there are no request body bytes and
            // the response head promised exactly three bytes.
            connection.set_framing(Some(0), Some(3), false);
            let reader = delivery.attach(page.clone(), slice(0, 2)).unwrap();
            let scope = scope();
            // A writable socket completes through the pipe before the reactor is driven.
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let Poll::Ready(Ok(connection)) = operation.as_mut().poll(&mut cx) else {
                panic!("writable HTTP socket did not complete synchronously");
            };
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(connection.send_remaining(), Some(1));
            assert!(!connection.is_reusable());
            let reader = delivery.attach(page, slice(2, 1)).unwrap();
            let mut connection =
                drive(&reactor, delivery.finish_to(reader, connection, &scope)).unwrap();
            assert_eq!(connection.send_remaining(), Some(0));
            connection.finish_exchange().unwrap();
            assert!(connection.is_reusable());
            let mut bytes = [0; 3];
            peer.read_exact(&mut bytes).unwrap();
            assert_eq!(&bytes, b"abc");
        }
        #[test]
        fn http_delivery_rejects_missing_or_insufficient_body_framing() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            for remaining in [None, Some(2)] {
                let page = page(&admission, b"abc".to_vec());
                let (socket, mut peer) = UnixStream::pair().unwrap();
                let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
                connection.set_framing(None, remaining, false);
                let reader = delivery.attach(page, slice(0, 3)).unwrap();
                assert!(matches!(
                    drive(&reactor, delivery.finish_to(reader, connection, &scope())),
                    Err(Error::InvalidRequest)
                ));
                let mut bytes = [0; 1];
                assert_eq!(peer.read(&mut bytes).unwrap(), 0);
            }
        }
        #[test]
        fn unpolled_future_releases_page_pipe_and_owned_connection() {
            let (admission, _reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"abc".to_vec());
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, 3)).unwrap();
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let scope = scope();
            let operation =
                delivery.finish_to(reader, connection(socket.into(), &admission, 3), &scope);
            assert!(weak.upgrade().is_some());
            assert!(matches!(
                delivery.pipes.acquire(),
                Err(flow_control::Error::Overloaded)
            ));
            drop(operation);
            assert!(weak.upgrade().is_none());
            assert!(delivery.pipes.acquire().is_ok());
            let mut bytes = [0; 1];
            assert_eq!(peer.read(&mut bytes).unwrap(), 0);
        }
        #[test]
        fn http_connection_and_reader_admission_survive_readiness_wait() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
            let scope = scope();
            let mut operation = delivery.finish_to(reader, connection, &scope);
            assert_pending(&mut operation);
            assert!(reactor.in_flight() > 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(admission.used(ResourceClass::Pipe), 1);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            scope.cancel().unwrap();
            assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            assert!(delivery.pipes.acquire().is_ok());
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        #[test]
        fn original_deadline_caps_a_longer_stall_timeout() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(30));
            let (reader, connection, _peer, _) = blocked_reader(&admission, &delivery);
            let mut scope = scope();
            scope.deadline = Deadline(Instant::now() + Duration::from_millis(25));
            let original = scope.deadline.0;
            assert!(matches!(
                drive(&reactor, delivery.finish_to(reader, connection, &scope)),
                Err(Error::DeadlineExceeded)
            ));
            assert_eq!(scope.deadline.0, original);
        }
        #[test]
        fn abandoned_http_send_does_not_cycle_through_reactor_owner() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let weak_reactor = Rc::downgrade(&reactor);
            let (reader, connection, _peer, weak_page) = blocked_reader(&admission, &delivery);
            let scope = scope();
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert!(reactor.in_flight() > 0);
            drop(operation);
            drop(delivery);
            // The pending lease must not own an Rc back to its containing reactor.
            assert_eq!(Rc::strong_count(&reactor), 1);
            drop(reactor);
            assert!(weak_reactor.upgrade().is_none());
            assert!(weak_page.upgrade().is_none());
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        #[test]
        fn abandoned_http_send_retains_all_leases_until_completion_fence() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
            let scope = scope();
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            drop(operation);
            assert!(weak.upgrade().is_some());
            assert!(matches!(
                delivery.pipes.acquire(),
                Err(flow_control::Error::Overloaded)
            ));
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            let start = Instant::now();
            while reactor.in_flight() != 0 {
                assert!(start.elapsed() < Duration::from_secs(5));
                reactor.poll_budgeted(64).unwrap();
                std::thread::sleep(Duration::from_millis(1));
            }
            assert!(weak.upgrade().is_none());
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert!(delivery.pipes.acquire().is_ok());
        }
        #[test]
        fn reactor_drain_fences_pending_http_delivery_and_releases_all_leases() {
            for abandon in [false, true] {
                let (admission, reactor, delivery) = setup(1, Duration::from_secs(30));
                let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
                let scope = scope();
                let mut operation = delivery.finish_to(reader, connection, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                assert_eq!(reactor.in_flight(), 1);
                let mut operation = if abandon {
                    drop(operation);
                    None
                } else {
                    Some(operation)
                };
                assert!(weak.upgrade().is_some());
                assert_eq!(admission.used(ResourceClass::Pipe), 1);
                assert_eq!(admission.used(ResourceClass::Connection), 1);
                // drain is a completion fence, not a blocking shutdown call. The
                // driver must keep polling CQEs until the outstanding send is fenced.
                drive(&reactor, reactor.drain()).unwrap();
                if let Some(operation) = operation.take() {
                    assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
                }
                drop(operation);
                assert_eq!(reactor.in_flight(), 0);
                assert!(weak.upgrade().is_none());
                assert_eq!(
                    admission.used(ResourceClass::Pipe),
                    1,
                    "idle pipe remains admitted"
                );
                assert_eq!(admission.used(ResourceClass::Connection), 0);
                // The reactor's own ring allocation remains admitted until drop.
                drop(delivery);
                assert_eq!(admission.used(ResourceClass::Pipe), 0);
                drop(reactor);
                assert_eq!(admission.used(ResourceClass::RequestContext), 0);
                assert_eq!(scope.check(), Ok(()));
            }
        }
        #[test]
        fn http_splice_and_backpressure_send_deliver_exactly_once() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
            let bytes: Vec<u8> = (0..512 * 1024).map(|i| (i % 251) as u8).collect();
            let mut backing = b"prefix!".to_vec();
            backing.extend_from_slice(&bytes);
            backing.extend_from_slice(b"suffix!");
            let page = page(&admission, backing);
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(7, bytes.len() as u32)).unwrap();
            let (socket, mut peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            peer.set_nonblocking(true).unwrap();
            let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
            connection.set_framing(None, Some(bytes.len() as u64), false);
            let scope = scope();
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            let mut received = Vec::new();
            let mut scratch = [0; 8192];
            let start = Instant::now();
            let mut completed = None;
            let mut page_sends = 0;
            while received.len() != bytes.len() || completed.is_none() {
                assert!(start.elapsed() < Duration::from_secs(5));
                loop {
                    match peer.read(&mut scratch) {
                        Ok(0) => break,
                        Ok(count) => received.extend_from_slice(&scratch[..count]),
                        Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                        Err(error) => panic!("{error}"),
                    }
                }
                reactor.poll_budgeted(64).unwrap();
                if completed.is_none() {
                    if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                        completed = Some(result.unwrap());
                    }
                    if weak.strong_count() == 2 {
                        page_sends += 1;
                    }
                }
            }
            assert!(
                page_sends > 1,
                "exercise repeated immutable page sends and short writes"
            );
            assert_eq!(received, bytes);
            assert_eq!(completed.as_ref().unwrap().send_remaining(), Some(0));
            assert_eq!(
                delivery
                    .metrics
                    .count(crate::telemetry::Event::DeliveryPipeDrain),
                1,
                "a backpressured page drains its staging pipe only once"
            );
            assert!(
                delivery
                    .metrics
                    .count(crate::telemetry::Event::DeliveryDirectBytes)
                    > 0
            );
            assert!(weak.upgrade().is_none());
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            assert!(delivery.pipes.acquire().is_ok());
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            drop(completed);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        #[test]
        fn pending_http_send_cancellation_disconnect_and_stall_release_all_leases() {
            let (admission, reactor, delivery) = setup(1, Duration::from_millis(25));
            for failure in [Error::Cancelled, Error::Io, Error::DeadlineExceeded] {
                let (reader, connection, peer, weak) = blocked_reader(&admission, &delivery);
                let scope = scope();
                let original = scope.deadline.0;
                let mut operation = delivery.finish_progressing(reader, connection, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                assert_eq!(reactor.in_flight(), 1);
                match failure {
                    Error::Cancelled => scope.cancel().unwrap(),
                    Error::Io => drop(peer),
                    _ => {}
                }
                assert!(matches!(drive(&reactor, operation), Err(error) if error == failure));
                assert_eq!(scope.deadline.0, original);
                if failure != Error::Cancelled {
                    assert_eq!(scope.check(), Ok(()));
                }
                assert_eq!(reactor.in_flight(), 0);
                assert!(weak.upgrade().is_none());
                assert_eq!(
                    admission.used(ResourceClass::Pipe),
                    1,
                    "idle pipe remains admitted"
                );
                assert!(delivery.pipes.acquire().is_ok());
                assert_eq!(admission.used(ResourceClass::Connection), 0);
            }
        }
        #[test]
        fn unpolled_http_delivery_closes_connection_and_releases_admission() {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let page = page(&admission, b"abc".to_vec());
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, 3)).unwrap();
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
            connection.set_framing(None, Some(3), false);
            let scope = scope();
            let operation = delivery.finish_to(reader, connection, &scope);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            drop(operation);
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            assert!(delivery.pipes.acquire().is_ok());
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert_eq!(peer.read(&mut [0; 1]).unwrap(), 0);
        }
        #[test]
        fn tcp_http_delivery_survives_page_and_pipe_release_before_peer_reads() {
            use std::net::TcpListener;
            use std::net::TcpStream;
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            let (socket, _) = listener.accept().unwrap();
            let bytes: Vec<u8> = (0..4096).map(|i| (i % 251) as u8).collect();
            let page = page(&admission, bytes.clone());
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, bytes.len() as u32)).unwrap();
            let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
            connection.set_framing(None, Some(bytes.len() as u64), false);
            let connection =
                drive(&reactor, delivery.finish_to(reader, connection, &scope())).unwrap();
            assert_eq!(connection.send_remaining(), Some(0));
            assert!(weak.upgrade().is_none());
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            let mut replacement = delivery.pipes.acquire().unwrap();
            replacement.try_write(&[0xff; 4096]).unwrap();
            let mut received = vec![0; bytes.len()];
            peer.read_exact(&mut received).unwrap();
            assert_eq!(received, bytes);
            drop(connection);
            assert_eq!(peer.read(&mut [0; 1]).unwrap(), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }

    mod pool {
        use super::*;
        use crate::test_support::WakeCounter;
        use std::sync::Arc;
        use std::task::Waker;
        use uring_runtime::reactor::simulation::Fault;
        use uring_runtime::reactor::simulation::Simulation;
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
                static NEXT: std::sync::atomic::AtomicUsize =
                    std::sync::atomic::AtomicUsize::new(0);
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
        fn scope() -> RequestScope {
            RequestScope::new(
                RequestId([7; 16]),
                uring_runtime::environment::now() + Duration::from_secs(5),
            )
            .unwrap()
        }
        fn setup() -> (
            Rc<flow_control::Quotas<AdmissionPolicy>>,
            Rc<Reactor>,
            HttpPool,
        ) {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.queue_entries = std::num::NonZeroUsize::new(2).unwrap();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            reactor.init().unwrap();
            let mut pool = new_pool(reactor.clone(), admission.clone(), 1);
            pool.config_mut().secondary_cap = 2;
            (admission, reactor, pool)
        }
        // Hold a real checked-out connection only after a complete head/body exchange.
        fn held(
            pool: &HttpPool,
            reactor: &Rc<Reactor>,
            admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
            endpoint: &Endpoint,
            listener: &Listener,
        ) -> (ConnectionLease, ConnectionLease) {
            let scope = scope();
            let io = new_io(reactor.clone(), Codec::new(4096), admission.clone(), 16);
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
                let connection = from_accepted(socket, admission)?;
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
            drive(reactor, async { futures::try_join!(client, server) }).unwrap()
        }
        #[test]
        fn peer_tcp_nodelay_does_not_touch_unix_or_origin_sockets() {
            let (admission, reactor, mut pool) = setup();
            pool.config_mut().tcp_nodelay = true;
            let (listener, endpoint) = Listener::origin();
            let Endpoint::Unix(path) = endpoint else {
                panic!()
            };
            for endpoint in [
                Endpoint::Unix(path.clone()),
                Endpoint::Origin {
                    path,
                    cache: racer_control_wire::CacheId("nodelay-test".into()),
                },
            ] {
                let scope = scope();
                let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
                let accepted = listener.accept().unwrap();
                drop(connection);
                drop(accepted);
                assert_eq!(admission.used(ResourceClass::Connection), 0);
            }
        }
        #[test]
        fn idle_expiration_runs_without_checkout_or_waiters_and_is_budgeted() {
            let clock = uring_runtime::environment::SimulationClock::new(91);
            let _environment = clock.environment(0).enter();
            let (admission, reactor, mut pool) = setup();
            pool.config_mut().idle_timeout = Duration::ZERO;
            let mut peers = Vec::new();
            for _ in 1..=3 {
                let (listener, endpoint) = Listener::peer();
                let (lease, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
                peers.push(peer);
                drop(lease);
            }
            for remaining in (0..3).rev() {
                clock.advance(Duration::from_secs(1));
                pool.poll_waiters(1);
                assert_eq!(admission.used(ResourceClass::OutboundConnection), remaining);
            }
        }
        #[test]
        fn origin_wait_is_bounded_fifo_without_blocking_peers_or_other_caches() {
            let clock = uring_runtime::environment::SimulationClock::new(92);
            let _environment = clock.environment(0).enter();
            let (admission, reactor, pool) = setup();
            let (listener, endpoint) = Listener::origin();
            let (first, a) = held(&pool, &reactor, &admission, &endpoint, &listener);
            let (second, b) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
            pool.poll_waiters(2);
            assert_eq!(count.count(), 1, "early worker repolls do not busy-wake");
            assert!(matches!(
                pool.checkout_wait(&endpoint, &scope).as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
            assert!(admission.used(ResourceClass::RequestContext) > 0);
            assert_eq!(reactor.in_flight(), 0);
            // A full origin queue does not gate peers or unrelated caches; peer saturation is immediate.
            for (listener, other) in [Listener::peer(), Listener::origin()] {
                let (lease, _peer) = held(&pool, &reactor, &admission, &other, &listener);
                if matches!(other, Endpoint::Peer(_)) {
                    assert!(matches!(
                        pool.checkout(&other, &scope).as_mut().poll(&mut cx),
                        Poll::Ready(Err(Error::Overloaded))
                    ));
                }
                drop(lease);
                let Poll::Ready(Ok(lease)) =
                    pool.checkout_wait(&other, &scope).as_mut().poll(&mut cx)
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
                let (held, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
                assert_eq!(pool.snapshot().waiting, 0);
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
            use futures::Stream;
            use futures::stream::FuturesUnordered;
            let (admission, reactor, pool) = setup();
            let (listener, endpoint) = Listener::peer();
            let (_held, _peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
            assert_eq!(pool.snapshot().waiting, 0);
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
                pool.snapshot().entries.is_empty(),
                "no slot held waiting for global quota"
            );
            drop(quota);
            pool.poll_waiters(1);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert_eq!(pool.snapshot().waiting, 0);
            assert_eq!(reactor.in_flight(), 1);
            drop(wait);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert_eq!(pool.snapshot().entries[&endpoint].0, 1);
            assert!(matches!(
                pool.checkout(&endpoint, &scope).as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
            while reactor.in_flight() != 0 {
                scope.check().unwrap();
                reactor.poll_budgeted(32).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            assert!(pool.snapshot().entries.is_empty());
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        }
        #[test]
        fn waiting_endpoint_table_and_context_pressure_remain_bounded() {
            let (admission, reactor, mut pool) = setup();
            pool.config_mut().max_endpoints = 1;
            let (listener, first) = Listener::peer();
            let (second_listener, second) = Listener::peer();
            let (held, peer) = held(&pool, &reactor, &admission, &first, &listener);
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
            assert_eq!(pool.snapshot().waiting, 0);
            assert_eq!(pool.snapshot().entries.len(), 1);
            drop(context);
            let mut wait = pool.checkout_wait(&second, &scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert_eq!(pool.snapshot().entries.len(), 1);
            drop(held);
            // Real checkout evicts the idle-only endpoint before its idle timeout.
            let connection = drive(&reactor, wait.as_mut()).unwrap();
            assert!(second_listener.accept().is_some());
            assert_eq!(pool.snapshot().entries.len(), 1);
            assert!(!pool.snapshot().entries.contains_key(&first));
            drop((connection, wait, peer));
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        #[test]
        fn origin_uid_churn_preserves_endpoint_and_connection_bounds() {
            let (admission, reactor, mut pool) = setup();
            pool.config_mut().max_endpoints = 1;
            let (listener, endpoint) = Listener::origin();
            let Endpoint::Unix(path) = endpoint else {
                unreachable!()
            };
            let origin = |uid: usize| Endpoint::Origin {
                cache: racer_control_wire::CacheId(format!("cache-{uid}")),
                path: path.clone(),
            };
            let (first, peer) = held(&pool, &reactor, &admission, &origin(0), &listener);
            assert!(matches!(
                pool.prepare_connection(&origin(1)),
                Err(Error::Overloaded)
            ));
            assert_eq!(pool.snapshot().entries.len(), 1);
            drop((first, peer));
            for uid in 1..32 {
                let (connection, peer) = held(&pool, &reactor, &admission, &origin(uid), &listener);
                assert_eq!(pool.snapshot().entries.len(), 1);
                assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
                drop((connection, peer));
            }
            pool.close();
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        #[test]
        fn origin_cap_is_independent_of_peer_cap_and_idle_quota_is_reclaimed() {
            let (admission, reactor, pool) = setup();
            let (listener, endpoint) = Listener::origin();
            let Endpoint::Unix(path) = endpoint else {
                unreachable!()
            };
            let origin = Endpoint::Origin {
                cache: racer_control_wire::CacheId("cache".into()),
                path,
            };
            let (first, a) = held(&pool, &reactor, &admission, &origin, &listener);
            let (second, b) = held(&pool, &reactor, &admission, &origin, &listener);
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
            assert_eq!(pool.snapshot().entries[&origin].1, 0);
            drop((lease, quota));
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        #[test]
        fn metadata_bypasses_queued_pages_within_existing_origin_cap() {
            let (admission, reactor, pool) = setup();
            let (listener, endpoint) = Listener::origin();
            let Endpoint::Unix(path) = endpoint else {
                unreachable!()
            };
            let endpoint = Endpoint::Origin {
                cache: racer_control_wire::CacheId("cache".into()),
                path,
            };
            let (first, a) = held(&pool, &reactor, &admission, &endpoint, &listener);
            let (idle, b) = held(&pool, &reactor, &admission, &endpoint, &listener);
            let baseline = admission.used(ResourceClass::RequestContext);
            drop(idle);
            let scope = scope();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut page = pool.checkout_wait(&endpoint, &scope);
            assert!(page.as_mut().poll(&mut cx).is_pending());
            let mut metadata = pool.checkout_metadata(&endpoint, &scope);
            let Poll::Ready(Ok(metadata)) = metadata.as_mut().poll(&mut cx) else {
                panic!("metadata queued behind page")
            };
            assert_eq!(pool.snapshot().entries[&endpoint].0, 2);
            assert!(page.as_mut().poll(&mut cx).is_pending());
            drop((metadata, first, page, a, b));
            pool.close();
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        }
        #[test]
        fn invalidated_active_generation_cannot_reenter_idle_and_expiry_releases_quota() {
            let (admission, reactor, mut pool) = setup();
            pool.config_mut().idle_timeout = Duration::ZERO;
            let (listener, endpoint) = Listener::peer();
            for invalidate in [true, false] {
                let (connection, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
                assert!(pool.snapshot().entries.is_empty());
            }
        }
        #[test]
        fn opaque_staging_and_connection_reservations_survive_reactor_abandonment() {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            reactor.init().unwrap();
            let baseline = admission.used(ResourceClass::RequestContext);
            let pool = new_pool(reactor.clone(), admission.clone(), 1);
            let (listener, endpoint) = Listener::peer();
            let (connection, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
            let scope =
                RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(5))
                    .unwrap();
            let staging = OwnedBuffer::new(&HttpContext(admission.clone()), 4096).unwrap();
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
        #[test]
        fn adaptive_checkout_attributes_actual_connect_completion_errno() {
            use crate::telemetry::Event;
            use crate::telemetry::Gauge;
            use crate::telemetry::Metrics;
            for (errno, blame) in [
                (libc::ENOBUFS, false),
                (libc::ENOMEM, false),
                (libc::EADDRNOTAVAIL, false),
                (libc::ECONNREFUSED, true),
            ] {
                let simulation = Simulation::new();
                let _env = simulation.enter();
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let reactor = Rc::new(Reactor::new(admission.clone()));
                let pool = new_pool(reactor.clone(), admission, 1);
                let metrics = Metrics::default();
                let peers = crate::peer::AdaptivePeers::new(
                    crate::peer::Config {
                        total: 1,
                        per_peer: 1,
                    },
                    metrics.clone(),
                )
                .unwrap();
                let node = racer_control_wire::NodeId("peer".into());
                let permit = peers.acquire(&node).unwrap();
                let failure = Rc::new(std::cell::Cell::new(false));
                let scope = RequestScope::new(
                    crate::model::RequestId([88; 16]),
                    uring_runtime::environment::now() + Duration::from_secs(5),
                )
                .unwrap();
                let endpoint = Endpoint::Peer("127.0.0.1:9999".into());
                simulation.inject("connect", Fault::Errno(errno));
                let mut checkout = checkout_peer(
                    &pool,
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
    }

    mod relay_tests {
        use super::*;
        use crate::peer::transport::relay_body;
        use flow_control::pipe::MAX_PIPE_BYTES;
        use std::net::TcpStream;
        fn pair() -> (TcpStream, TcpStream) {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (socket, _) = listener.accept().unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
            peer.set_write_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            (socket, peer)
        }
        fn poll<F: Future + ?Sized>(future: std::pin::Pin<&mut F>) -> Poll<F::Output> {
            future.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        }
        // Match the production service's wake-driven FuturesUnordered and reactor wait,
        // rather than repeatedly polling a blocked future with a noop waker.
        fn drive_worker<T>(reactor: &Reactor, work: impl Future<Output = T>) -> T {
            use futures::Stream;
            use futures::stream::FuturesUnordered;
            use std::sync::Arc;
            use std::task::Wake;
            struct WakeReactor(uring_runtime::reactor::ReactorWake);
            impl Wake for WakeReactor {
                fn wake(self: Arc<Self>) {
                    self.0.wake().unwrap();
                }
            }
            let waker = std::task::Waker::from(Arc::new(WakeReactor(reactor.waker().unwrap())));
            let mut active = FuturesUnordered::new();
            active.push(work);
            let until = Instant::now() + Duration::from_secs(8);
            loop {
                reactor.poll_budgeted(64).unwrap();
                if let Poll::Ready(Some(result)) =
                    std::pin::Pin::new(&mut active).poll_next(&mut Context::from_waker(&waker))
                {
                    return result;
                }
                assert!(Instant::now() < until, "wake-driven relay watchdog");
                reactor.wait(Duration::from_millis(10)).unwrap();
            }
        }
        struct Fixture {
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            reactor: Rc<Reactor>,
            io: HttpIo,
            pipes: PipePool<AdmissionPolicy>,
            scope: RequestScope,
        }
        impl Fixture {
            fn new() -> Self {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.pipes = std::num::NonZeroUsize::new(1).unwrap();
                limits.relay_transfers = std::num::NonZeroUsize::new(1).unwrap();
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
                let reactor = Rc::new(Reactor::new(admission.clone()));
                reactor.init().unwrap();
                Self {
                    io: new_io(
                        reactor.clone(),
                        Codec::new(4096),
                        admission.clone(),
                        crate::model::PAGE_BYTES + 16,
                    ),
                    pipes: new_pipe_pool(admission.clone()),
                    admission,
                    reactor,
                    scope: RequestScope::new(
                        RequestId([3; 16]),
                        Instant::now() + Duration::from_secs(8),
                    )
                    .unwrap(),
                }
            }
            fn connections(
                &self,
                length: usize,
            ) -> (ConnectionLease, ConnectionLease, TcpStream, TcpStream) {
                let (source, writer) = pair();
                let (destination, reader) = pair();
                let mut source = from_accepted(source.into(), &self.admission).unwrap();
                let mut destination = from_accepted(destination.into(), &self.admission).unwrap();
                let reservation = Rc::new(
                    self.admission
                        .reserve(None, ResourceClass::Relay, 1)
                        .unwrap(),
                );
                source.state_mut().relay_reservation = Some(reservation.clone());
                destination.state_mut().relay_reservation = Some(reservation);
                source.set_framing(Some(length as u64), Some(0), false);
                destination.set_framing(Some(0), Some(length as u64), false);
                (source, destination, writer, reader)
            }
            fn drain(&self) {
                drive(&self.reactor, self.reactor.drain()).unwrap();
            }
        }
        #[test]
        fn truncation_after_success_closes_both_without_error_suffix_or_pool_reuse() {
            let f = Fixture::new();
            let (source, mut destination, mut writer, mut reader) = f.connections(1024);
            destination.set_framing(destination.receive_remaining(), None, false);
            destination = drive(
                &f.reactor,
                f.io.send_head(
                    destination,
                    MessageHead {
                        start: StartLine::Response { status: 200 },
                        headers: vec![Header {
                            name: "Content-Length".into(),
                            value: b"1024".to_vec(),
                        }],
                    },
                    &f.scope,
                ),
            )
            .unwrap()
            .connection;
            writer.write_all(b"short").unwrap();
            writer.shutdown(std::net::Shutdown::Write).unwrap();
            let pipe = f.pipes.acquire().unwrap();
            assert!(matches!(
                drive(
                    &f.reactor,
                    relay_body(&f.io, source, destination, Some(pipe), &f.scope)
                ),
                Err(Error::Io)
            ));
            let mut bytes = Vec::new();
            reader.read_to_end(&mut bytes).unwrap();
            assert_eq!(bytes, b"HTTP/1.1 200 \r\nContent-Length: 1024\r\n\r\nshort");
            assert_eq!(writer.read(&mut [0]).unwrap(), 0);
            f.drain();
            assert_eq!(f.admission.used(ResourceClass::Connection), 0);
            assert_eq!(f.admission.used(ResourceClass::Relay), 0);
        }
        #[test]
        fn stalled_transit_retains_both_connections_pipe_and_relay_until_cancel_fence() {
            for writing in [false, true] {
                for end in ["drop", "cancel", "deadline", "disconnect"] {
                    let mut f = Fixture::new();
                    if end == "deadline" {
                        f.scope.deadline.0 = Instant::now() + Duration::from_millis(40);
                    }
                    let (source, destination, writer, reader) =
                        f.connections(16 * 1024 * 1024 + 16);
                    let pipe = f.pipes.acquire().unwrap();
                    let mut held_writer = Some(writer);
                    let producer = if writing {
                        let mut writer = held_writer.take().unwrap();
                        Some(std::thread::spawn(move || {
                            let _ = writer.write_all(&vec![91; 16 * 1024 * 1024 + 16]);
                        }))
                    } else {
                        None
                    };
                    let mut work =
                        Box::pin(relay_body(&f.io, source, destination, Some(pipe), &f.scope));
                    assert!(poll(work.as_mut()).is_pending());
                    if writing {
                        let until = Instant::now() + Duration::from_millis(15);
                        while Instant::now() < until {
                            f.reactor.poll_budgeted(128).unwrap();
                            assert!(poll(work.as_mut()).is_pending());
                        }
                    }
                    assert_eq!(f.admission.used(ResourceClass::Connection), 2);
                    assert_eq!(f.admission.used(ResourceClass::Relay), 1);
                    assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
                    assert!(matches!(
                        f.pipes.acquire(),
                        Err(flow_control::Error::Overloaded)
                    ));
                    assert!(matches!(
                        f.admission.reserve(None, ResourceClass::Relay, 1),
                        Err(flow_control::Error::Overloaded)
                    ));
                    let mut reader = Some(reader);
                    if end == "disconnect" {
                        reader.take();
                    }
                    if end == "cancel" {
                        f.scope.cancel().unwrap();
                    }
                    if end != "drop" {
                        let result = drive(&f.reactor, work.as_mut());
                        let expected = match end {
                            "cancel" => Error::Cancelled,
                            "deadline" => Error::DeadlineExceeded,
                            _ => Error::Io,
                        };
                        assert!(
                            matches!(result, Err(error) if error == expected),
                            "{writing}/{end}"
                        );
                    }
                    drop(work);
                    if end == "drop" && !writing {
                        assert_eq!(f.reactor.in_flight(), 1);
                        assert_eq!(f.admission.used(ResourceClass::Connection), 2);
                        assert_eq!(f.admission.used(ResourceClass::Relay), 1);
                        assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
                    }
                    f.drain();
                    assert_eq!(f.admission.used(ResourceClass::Connection), 0);
                    assert_eq!(f.admission.used(ResourceClass::Relay), 0);
                    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
                    assert!(f.admission.used(ResourceClass::Pipe) <= 1);
                    if let Some(producer) = producer {
                        producer.join().unwrap();
                    }
                }
            }
        }
        #[test]
        fn unsupported_splice_with_buffered_pipe_drains_suffix_and_keeps_exact_frame() {
            let f = Fixture::new();
            let length = 1024 * 1024 + 16;
            let (source, mut destination, mut writer, mut reader) = f.connections(length);
            destination.state_mut().relay_fallback_at = Some(length / 2);
            let producer = std::thread::spawn(move || {
                writer.write_all(&vec![83; length]).unwrap();
                writer
            });
            let consumer = std::thread::spawn(move || {
                let mut body = vec![0; length];
                reader.read_exact(&mut body).unwrap();
                assert!(body.iter().all(|b| *b == 83));
                reader
            });
            let pipe = f.pipes.acquire().unwrap();
            let result = drive(
                &f.reactor,
                relay_body(&f.io, source, destination, Some(pipe), &f.scope),
            )
            .unwrap();
            assert!(result.is_reusable());
            drop(result);
            drop(producer.join().unwrap());
            drop(consumer.join().unwrap());
            f.drain();
            assert_eq!(f.admission.used(ResourceClass::Relay), 0);
            assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
            assert!(f.io.retained_buffer_bytes() <= MAX_PIPE_BYTES);
        }
        #[test]
        fn tcp_backpressure_recovers_and_wakes_queued_pipe_owner_without_losing_frame() {
            use std::os::fd::AsRawFd;
            let f = Fixture::new();
            let length = 16 * 1024 * 1024 + 16;
            let (source, destination, mut writer, mut reader) = f.connections(length);
            let size: libc::c_int = 64 * 1024;
            // SAFETY: live TCP descriptor and correctly sized option storage.
            assert_eq!(
                unsafe {
                    libc::setsockopt(
                        destination.socket().as_raw_fd(),
                        libc::SOL_SOCKET,
                        libc::SO_SNDBUF,
                        (&size as *const libc::c_int).cast(),
                        std::mem::size_of_val(&size) as libc::socklen_t,
                    )
                },
                0
            );
            let producer = std::thread::spawn(move || {
                writer.write_all(&vec![83; length]).unwrap();
                writer
            });
            let mut pipe = f.pipes.acquire().unwrap();
            pipe.prepare_transit();
            let mut relay = Box::pin(relay_body(&f.io, source, destination, Some(pipe), &f.scope));
            let mut waiting = acquire_wait(&f.pipes, &f.scope);
            assert!(poll(waiting.as_mut()).is_pending());
            // The reader is deliberately not running. A page cannot fit in the bounded
            // send/receive buffers, so transit must retain its owners under backpressure.
            let pause = Instant::now() + Duration::from_millis(40);
            while Instant::now() < pause {
                f.reactor.poll_budgeted(64).unwrap();
                assert!(poll(relay.as_mut()).is_pending());
                std::thread::yield_now();
            }
            assert!(poll(waiting.as_mut()).is_pending());
            assert_eq!(f.admission.used(ResourceClass::Relay), 1);
            assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
            assert_eq!(f.admission.used(ResourceClass::Connection), 2);
            let consumer = std::thread::spawn(move || {
                let mut body = vec![0; length];
                reader.read_exact(&mut body).unwrap();
                assert!(body.iter().all(|b| *b == 83));
                reader
            });
            let (connection, pipe) =
                drive_worker(&f.reactor, async { futures::join!(relay, waiting) });
            let mut connection = connection.unwrap();
            let mut pipe = pipe.unwrap();
            assert!(connection.is_reusable());
            assert_eq!(f.admission.used(ResourceClass::Relay), 0);
            assert_eq!(pipe.buffered(), 0);
            assert_eq!(f.admission.used(ResourceClass::Connection), 1);
            // Reusing the pipe must not corrupt bytes already accepted by the socket.
            pipe.try_write(b"replacement").unwrap();
            let mut bytes = [0; 11];
            assert_eq!(pipe.try_read(&mut bytes).unwrap(), 11);
            assert_eq!(&bytes, b"replacement");
            drop(pipe);
            drop(producer.join().unwrap());
            let mut reader = consumer.join().unwrap();
            connection.set_framing(Some(0), Some(4), false);
            let mut suffix = f.io.buffer(4).unwrap();
            suffix.bytes_mut().unwrap().copy_from_slice(b"next");
            drop(drive_worker(&f.reactor, f.io.write_body(connection, suffix, &f.scope)).unwrap());
            let mut next = [0; 4];
            reader.read_exact(&mut next).unwrap();
            assert_eq!(&next, b"next");
            assert_eq!(reader.read(&mut next).unwrap(), 0);
            f.drain();
            assert_eq!(f.admission.used(ResourceClass::Connection), 0);
            assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
            // HttpIo retains its bounded reusable read/write buffers after completion.
            assert!(f.io.retained_buffer_bytes() <= MAX_PIPE_BYTES);
            assert_eq!(f.pipes.idle_count(), 1);
        }
    }

    fn setup() -> (
        Rc<flow_control::Quotas<AdmissionPolicy>>,
        Rc<Reactor>,
        HttpIo,
        RequestScope,
    ) {
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = new_io(
            reactor.clone(),
            Codec::new(4096),
            admission.clone(),
            8 * 1024 * 1024,
        );
        let scope = RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10))
            .unwrap();
        (admission, reactor, io, scope)
    }
    pub(crate) fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let deadline = Instant::now() + Duration::from_secs(12);
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(
                Instant::now() < deadline,
                "HTTP operation failed to make progress"
            );
            // Repoll completed work before sleeping: a drained CQ has no event left.
            if reactor.poll_budgeted(128).unwrap() == 0 {
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        }
    }
    fn request(method: &str) -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: "/test".into(),
            },
            headers: vec![Header {
                name: "Host".into(),
                value: b"localhost".to_vec(),
            }],
        }
    }
    fn response(length: usize) -> MessageHead {
        MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header {
                name: "Content-Length".into(),
                value: length.to_string().into_bytes(),
            }],
        }
    }
    fn drain(reactor: &Reactor) {
        drive(reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.in_flight(), 0);
    }
    #[test]
    fn small_peer_send_stages_actual_head_under_context_pressure() {
        let (admission, reactor, _, scope) = setup();
        reactor.init().unwrap();
        let limit = crate::peer::protocol::MAX_ENVELOPE_HEAD;
        let io = new_io(reactor.clone(), Codec::new(limit), admission.clone(), 16);
        let baseline = admission.used(ResourceClass::RequestContext);
        let held = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limit(ResourceClass::RequestContext) - baseline - limit - 4096,
            )
            .unwrap();
        let head = response(0);
        let expected = Codec::new(limit).encode_head(&head).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let result = drive(&reactor, io.send_head(connection, head, &scope)).unwrap();
        let mut received = vec![0; expected.len()];
        peer.read_exact(&mut received).unwrap();
        assert_eq!(received, expected);
        drop(result);
        drop(held);
        io.reclaim_buffer();
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn staging_recycles_zeroed_backing_with_retained_admission() {
        let (admission, _, io, _) = setup();
        let baseline = admission.used(ResourceClass::RequestContext);
        let mut buffer = io.buffer(4096).unwrap();
        let pointer = buffer.bytes().unwrap().as_ptr();
        buffer.bytes_mut().unwrap().fill(91);
        drop(buffer);
        assert_eq!(io.retained_buffer_bytes(), 4096);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + 4096
        );
        let buffer = io.buffer(4096).unwrap();
        assert_eq!(buffer.bytes().unwrap().as_ptr(), pointer);
        assert!(buffer.bytes().unwrap().iter().all(|b| *b == 0));
        drop(buffer);
        io.reclaim_buffer();
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }
    #[test]
    fn decoded_field_storage_is_admitted_before_parser_allocations() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let held = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limit(ResourceClass::RequestContext) - baseline - 8192,
            )
            .unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let mut head = b"GET / HTTP/1.1\r\n".to_vec();
        for _ in 0..500 {
            head.extend_from_slice(b"X:\r\n");
        }
        head.extend_from_slice(b"\r\n");
        peer.write_all(&head).unwrap();
        assert!(matches!(
            drive(&reactor, io.receive_head(connection, &scope)),
            Err(Error::Overloaded)
        ));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        drop(held);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
    }
    #[test]
    fn client_constructor_streams_beyond_page_cap_with_bounded_staging() {
        let (admission, reactor, _, scope) = setup();
        let io = client_io(reactor.clone(), admission.clone()).capped(4096);
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(b"GET /test HTTP/1.1\r\nHost: localhost\r\n\r\n")
            .unwrap();
        let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        let length = 2 * crate::model::PAGE_BYTES as usize + 113;
        let thread = std::thread::spawn(move || {
            peer.set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let mut head = Vec::new();
            while !head.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                peer.read_exact(&mut byte).unwrap();
                head.push(byte[0]);
            }
            assert_eq!(
                head,
                format!("HTTP/1.1 200 \r\nContent-Length: {length}\r\n\r\n").as_bytes()
            );
            let mut chunk = [0; 8192];
            let mut remaining = length;
            while remaining != 0 {
                let count = remaining.min(chunk.len());
                peer.read_exact(&mut chunk[..count]).unwrap();
                assert!(chunk[..count].iter().all(|byte| *byte == 91));
                remaining -= count;
            }
        });
        let mut connection = drive(
            &reactor,
            io.send_head(received.connection, response(length), &scope),
        )
        .unwrap()
        .connection;
        drop(received.value);
        drop(received._decoded);
        assert_eq!(connection.finish_exchange(), Err(Error::InvalidRequest));
        let mut buffer = io.buffer(8192).unwrap();
        buffer.bytes_mut().unwrap().fill(91);
        let mut remaining = length;
        while remaining != 0 {
            let count = remaining.min(8192);
            let completed = drive(
                &reactor,
                io.write_body_range(connection, buffer, 0..count, &scope),
            )
            .unwrap();
            assert_eq!(completed.bytes, count);
            buffer = completed.buffer;
            connection = completed.lease;
            remaining -= count;
            assert_eq!(
                admission.used(ResourceClass::RequestContext),
                baseline + 8192
            );
        }
        connection.finish_exchange().unwrap();
        assert!(connection.is_reusable());
        assert!(matches!(
            drive(
                &reactor,
                io.write_body_range(connection, buffer, 0..1, &scope)
            ),
            Err(Error::InvalidRequest)
        ));
        thread.join().unwrap();
        drain(&reactor);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn client_send_limit_does_not_relax_receive_or_page_transport_limits() {
        let (admission, reactor, _, scope) = setup();
        let page_limit = crate::model::PAGE_BYTES + 16;
        let page_io = new_io(
            reactor.clone(),
            Codec::new(4096),
            admission.clone(),
            page_limit,
        );
        let client_io = client_io(reactor.clone(), admission.clone()).capped(4096);
        for io in [&page_io, &client_io] {
            assert_eq!(
                io.framing(&response(page_limit as usize), false),
                Ok(page_limit)
            );
            for start in ["HTTP/1.1 200 OK", "GET /test HTTP/1.1"] {
                let (socket, mut peer) = UnixStream::pair().unwrap();
                let connection = from_accepted(socket.into(), &admission).unwrap();
                write!(
                    peer,
                    "{start}\r\nContent-Length: {}\r\n\r\n",
                    page_limit + 1
                )
                .unwrap();
                assert!(matches!(
                    drive(&reactor, io.receive_head(connection, &scope)),
                    Err(Error::InvalidRequest)
                ));
            }
        }
        for (io, length) in [
            (&page_io, page_limit + 1),
            (&client_io, i64::MAX as u64 + 1),
        ] {
            let (socket, _peer) = UnixStream::pair().unwrap();
            let connection = from_accepted(socket.into(), &admission).unwrap();
            assert!(matches!(
                drive(
                    &reactor,
                    io.send_head(connection, response(length as usize), &scope)
                ),
                Err(Error::InvalidRequest)
            ));
        }
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn endpoint_caps_accept_exact_boundary_and_reject_one_extra_byte() {
        let (admission, reactor, _, scope) = setup();
        let peer = new_io(
            reactor.clone(),
            Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            16,
        );
        let client = client_io(reactor.clone(), admission.clone());
        let origin = peer.capped(crate::http::MAX_HEAD_BYTES);
        for (io, limit) in [
            (&client, admission.policy().limits().header_bytes.get()),
            (&origin, crate::http::MAX_HEAD_BYTES),
            (&peer, crate::peer::protocol::MAX_ENVELOPE_HEAD),
        ] {
            for extra in [0, 1] {
                let mut head = request("GET");
                let overhead = Codec::new(usize::MAX).encode_head(&head).unwrap().len();
                let value_length = head.headers[0].value.len() + limit - overhead + extra;
                head.headers[0].value.resize(value_length, b'x');
                let raw = Codec::new(limit + 1).encode_head(&head).unwrap();
                assert_eq!(raw.len(), limit + extra);
                let (socket, mut other) = UnixStream::pair().unwrap();
                let connection = from_accepted(socket.into(), &admission).unwrap();
                let writer = std::thread::spawn(move || {
                    let _ = other.write_all(&raw);
                });
                let result = drive(&reactor, io.receive_head(connection, &scope));
                if extra == 0 {
                    assert!(result.is_ok());
                } else {
                    assert!(matches!(result, Err(Error::HeaderTooLarge)));
                }
                drop(result);
                writer.join().unwrap();
                let (socket, mut other) = UnixStream::pair().unwrap();
                let connection = from_accepted(socket.into(), &admission).unwrap();
                let reader = std::thread::spawn(move || {
                    let mut received = Vec::new();
                    other.read_to_end(&mut received).unwrap();
                    received.len()
                });
                let result = drive(&reactor, io.send_head(connection, head, &scope));
                if extra == 0 {
                    assert!(result.is_ok());
                } else {
                    assert!(matches!(result, Err(Error::HeaderTooLarge)));
                }
                drop(result);
                assert_eq!(reader.join().unwrap(), if extra == 0 { limit } else { 0 });
            }
        }
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn real_socket_fragmentation_read_ahead_and_owned_ranges() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let thread = std::thread::spawn(move || {
            peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Len").unwrap();
            std::thread::sleep(Duration::from_millis(5));
            peer.write_all(b"gth: 5\r\n\r\nhello").unwrap();
        });
        let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        let mut bytes = io.buffer(9).unwrap();
        bytes.bytes_mut().unwrap().fill(b'_');
        let mut connection = received.connection;
        let mut offset = 2;
        while offset < 7 {
            let completed = drive(
                &reactor,
                io.read_body_range(connection, bytes, offset..7, &scope),
            )
            .unwrap();
            offset += completed.bytes;
            bytes = completed.buffer;
            connection = completed.lease;
        }
        assert_eq!(bytes.bytes().unwrap(), b"__hello__");
        assert_eq!(connection.remaining_body(), Some(0));
        drop(connection);
        drop(bytes);
        drop(received.value);
        drop(received._decoded);
        thread.join().unwrap();
        drain(&reactor);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn real_partial_sends_preserve_owned_subrange() {
        use std::os::fd::AsRawFd;
        let (admission, reactor, io, scope) = setup();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let size: libc::c_int = 4096;
        // SAFETY: setsockopt synchronously reads this correctly sized integer.
        assert_eq!(
            unsafe {
                libc::setsockopt(
                    socket.as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_SNDBUF,
                    (&size as *const libc::c_int).cast(),
                    std::mem::size_of_val(&size) as _,
                )
            },
            0
        );
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let length = 1024 * 1024;
        let thread = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(10));
            let mut received = Vec::new();
            peer.read_to_end(&mut received).unwrap();
            let start = received.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
            assert_eq!(received.len() - start, length);
            assert!(received[start..].iter().all(|b| *b == 91));
        });
        let sent = drive(&reactor, io.send_head(connection, response(length), &scope)).unwrap();
        let mut bytes = io.buffer(length + 4).unwrap();
        bytes.bytes_mut().unwrap()[2..length + 2].fill(91);
        let completed = drive(
            &reactor,
            io.write_body_range(sent.connection, bytes, 2..length + 2, &scope),
        )
        .unwrap();
        assert_eq!(completed.bytes, length);
        drop(completed);
        drain(&reactor);
        thread.join().unwrap();
    }
    #[test]
    fn tcp_pool_reuses_only_finished_exchanges_and_enforces_capacity() {
        let (admission, reactor, io, scope) = setup();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let pool = new_pool(reactor.clone(), admission.clone(), 1);
        let thread = std::thread::spawn(move || {
            let (mut socket, _) = listener.accept().unwrap();
            for _ in 0..2 {
                let mut bytes = Vec::new();
                while !bytes.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    socket.read_exact(&mut byte).unwrap();
                    bytes.push(byte[0]);
                }
                socket
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                    .unwrap();
            }
        });
        for _ in 0..2 {
            let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
            assert!(matches!(
                drive(&reactor, pool.checkout(&endpoint, &scope)),
                Err(Error::Overloaded)
            ));
            let received = drive(
                &reactor,
                io.exchange_head(connection, request("GET"), &scope),
            )
            .unwrap();
            let mut completed =
                drive(&reactor, io.collect_body(received.connection, 2, &scope)).unwrap();
            assert_eq!(completed.buffer.bytes().unwrap(), b"ok");
            completed.lease.finish_exchange().unwrap();
            assert!(completed.lease.is_reusable());
            drop(completed);
        }
        thread.join().unwrap();
        pool.close();
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn head_has_no_body_even_with_large_representation_length() {
        let (admission, reactor, io, scope) = setup();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let thread = std::thread::spawn(move || {
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                peer.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 999999999\r\n\r\n")
                .unwrap();
        });
        let mut received = drive(
            &reactor,
            io.exchange_head(connection, request("HEAD"), &scope),
        )
        .unwrap();
        assert_eq!(received.connection.remaining_body(), Some(0));
        received.connection.finish_exchange().unwrap();
        thread.join().unwrap();
    }
    #[test]
    fn truncation_and_future_drop_do_not_recycle_live_buffers() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort")
            .unwrap();
        drop(peer);
        let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        assert!(matches!(
            drive(&reactor, io.collect_body(received.connection, 9, &scope)),
            Err(Error::Io)
        ));
        drop(received.value);
        drop(received._decoded);
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let mut future = io.receive_head(connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        drop(future);
        scope.cancel().unwrap();
        drain(&reactor);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn deadline_expires_without_peer_traffic() {
        let (admission, reactor, io, _) = setup();
        let scope = RequestScope::new(
            RequestId([2; 16]),
            Instant::now() + Duration::from_millis(30),
        )
        .unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        assert!(matches!(
            drive(&reactor, io.receive_head(connection, &scope)),
            Err(Error::DeadlineExceeded)
        ));
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn unix_pool_reconnects_after_unread_response_and_bounds_endpoints() {
        use std::os::unix::net::UnixListener;
        let (admission, reactor, io, scope) = setup();
        let mut nonce = [0; 8];
        getrandom::getrandom(&mut nonce).unwrap();
        let path = std::path::PathBuf::from(format!(
            "racer-http-{}-{}.sock",
            std::process::id(),
            u64::from_ne_bytes(nonce)
        ));
        let listener = UnixListener::bind(&path).unwrap();
        let endpoint = Endpoint::Unix(path.clone());
        let pool = pool_with_limits(reactor.clone(), admission.clone(), 1, 1, Duration::ZERO);
        let thread = std::thread::spawn(move || {
            for _ in 0..2 {
                let (mut socket, _) = listener.accept().unwrap();
                let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata");
                let mut sink = Vec::new();
                let _ = socket.read_to_end(&mut sink);
            }
        });
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        assert_eq!(
            received.connection.finish_exchange(),
            Err(Error::InvalidRequest)
        );
        drop(received);
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        let other = Endpoint::Peer("127.0.0.1:1".into());
        assert!(matches!(
            drive(&reactor, pool.checkout(&other, &scope)),
            Err(Error::Overloaded)
        ));
        pool.invalidate(&endpoint);
        drop(connection);
        pool.expire_idle();
        pool.close();
        thread.join().unwrap();
        std::fs::remove_file(path).unwrap();
        assert!(matches!(
            drive(&reactor, pool.checkout(&endpoint, &scope)),
            Err(Error::Unavailable)
        ));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn read_ahead_erases_credentials_and_endpoint_limit_is_enforced() {
        let (admission, reactor, io, scope) = setup();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(
            b"POST / HTTP/1.1\r\nAuthorization: secret\r\nContent-Length: 4\r\n\r\nbody",
        )
        .unwrap();
        let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        let (ahead, range) = received
            .connection
            .take_read_ahead()
            .expect("socket supplied head and body together");
        assert!(
            ahead.bytes().unwrap()[..range.start]
                .iter()
                .all(|b| *b == 0)
        );
        assert_eq!(&ahead.bytes().unwrap()[range.clone()], b"body");
        drop(received);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(b"GET / HTTP/1.1\r\nAuthorization: too-large\r\n\r\n")
            .unwrap();
        assert!(matches!(
            drive(&reactor, io.receive_head_limited(connection, &scope, 20)),
            Err(Error::HeaderTooLarge)
        ));
    }
    #[test]
    fn bodyless_and_malformed_response_framing_is_explicit() {
        let (_, _, io, _) = setup();
        assert_eq!(io.framing(&response(usize::MAX), true), Ok(0));
        assert!(io.framing(&response(usize::MAX), false).is_err());
        assert_eq!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status: 204 },
                    headers: vec![]
                },
                false
            ),
            Ok(0)
        );
        assert!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status: 200 },
                    headers: vec![]
                },
                false
            )
            .is_err()
        );
        assert!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status: 101 },
                    headers: vec![]
                },
                false
            )
            .is_err()
        );
    }
    #[test]
    fn dropped_connect_retains_slot_until_fd_fence_then_releases_quota() {
        let (admission, reactor, _, scope) = setup();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let pool = new_pool(reactor.clone(), admission.clone(), 1);
        let mut future = pool.checkout(&endpoint, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        drop(future);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert!(matches!(
            drive(&reactor, pool.checkout(&endpoint, &scope)),
            Err(Error::Overloaded)
        ));
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn dropped_checkout_and_destroyed_pool_release_quota_only_after_connect_fence() {
        for (cancel_before_drop, submit_before_drop) in
            [(false, false), (true, false), (true, true)]
        {
            let (admission, reactor, _, scope) = setup();
            reactor.init().unwrap();
            let baseline = admission.used(ResourceClass::RequestContext);
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
            let pool = new_pool(reactor.clone(), admission.clone(), 1);
            let mut future = pool.checkout(&endpoint, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            if submit_before_drop {
                reactor.poll_budgeted(32).unwrap();
            }
            if cancel_before_drop {
                scope.cancel().unwrap();
            }
            drop(future);
            drop(pool);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert_eq!(reactor.in_flight(), 1);
            drain(&reactor);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
            // No pool sweep exists: prove admission regained the charge rather than leaking it.
            let charge = admission
                .reserve(
                    None,
                    ResourceClass::Connection,
                    admission.policy().limits().client_connections.get(),
                )
                .unwrap();
            drop(charge);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
    #[test]
    fn canceled_receive_retains_resources_until_completion_and_reports_canceled() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let mut future = io.receive_head(connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert!(matches!(drive(&reactor, future), Err(Error::Cancelled)));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
    }
    #[test]
    fn raw_request_head_errors_return_fenced_socket_for_empty_400_and_431() {
        for oversized in [false, true] {
            let (admission, reactor, _, scope) = setup();
            let io = new_io(
                reactor.clone(),
                Codec::new(32 * 1024),
                admission.clone(),
                1024,
            );
            reactor.init().unwrap();
            let baseline = admission.used(ResourceClass::RequestContext);
            let (socket, mut peer) = UnixStream::pair().unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            let connection = from_accepted(socket.into(), &admission).unwrap();
            let raw = if oversized {
                let mut raw = b"GET / HTTP/1.1\r\nAuthorization: ".to_vec();
                raw.resize(crate::http::MAX_HEAD_BYTES, b'x');
                raw
            } else {
                b"GET / HTTP/1.1\r\nAuthorization:secret\r\nContent-Length: 4\r\n\r\nbody".to_vec()
            };
            peer.write_all(&raw).unwrap();
            let expected = if oversized {
                Error::HeaderTooLarge
            } else {
                Error::InvalidRequest
            };
            let mut outcome = drive(
                &reactor,
                io.receive_request_head_limited(connection, &scope, 32 * 1024),
            )
            .unwrap();
            assert!(matches!(outcome.value, Err(error) if error == expected));
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(
                admission.used(ResourceClass::RequestContext),
                baseline + io.retained_buffer_bytes()
            );
            assert!(outcome.connection.take_read_ahead().is_none());
            assert!(!outcome.connection.is_reusable());
            assert_eq!(
                outcome.connection.finish_exchange(),
                Err(Error::InvalidRequest)
            );
            let status = if oversized { 431 } else { 400 };
            let mut head = response(0);
            head.start = StartLine::Response { status };
            head.headers.push(Header {
                name: "Connection".into(),
                value: b"close".to_vec(),
            });
            let mut sent = drive(&reactor, io.send_head(outcome.connection, head, &scope)).unwrap();
            assert!(!sent.connection.is_reusable());
            assert_eq!(
                sent.connection.finish_exchange(),
                Err(Error::InvalidRequest)
            );
            let expected =
                format!("HTTP/1.1 {status} \r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
            let mut wire = vec![0; expected.len()];
            peer.read_exact(&mut wire).unwrap();
            assert_eq!(wire, expected.as_bytes());
            drop(sent);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
}
