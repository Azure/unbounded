//! Racer policy adapters for the standalone fixed-length HTTP implementation.
use crate::runtime::cooperative_turn as yield_once;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::VerifiedPage;
use crate::model::PageSlice;
use crate::model::ResourceClass;

use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::runtime::RequestScope;
use crate::runtime::Reactor;
use flow_control::pipe::PipeLease;
use flow_control::pipe::PipePool;
use http1::MessageHead;
use std::task::Waker;
use std::io;
use std::path::PathBuf;
use std::rc::Rc;
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
        self.0.reserve_connection(ResourceClass::OutboundConnection)
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
        admission.reserve_connection(ResourceClass::IngressConnection)?,
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
        Codec::new(admission.policy().limits().header_bytes.get().min(MAX_HEAD_BYTES)),
        Rc::new(HttpContext(admission)),
        crate::model::PAGE_BYTES + 16,
        i64::MAX as u64,
    )
}
#[derive(Clone, Debug, Eq, Hash, PartialEq, Ord, PartialOrd)]
pub enum Endpoint {
    Unix(PathBuf),
    Origin {
        cache: crate::model::CacheId,
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
                            let _ = self.metrics.record(
                                crate::telemetry::Event::DeliveryDirectBytes,
                                sent as u64,
                            );
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
pub(crate) mod tests;
#[cfg(test)]
mod relay_tests {
    //! Opaque transit, fallback, cancellation, and ownership-fence scenarios.
    use super::*;
    use crate::peer::transport::relay_body;
    use crate::model::RequestId;
    use crate::model::ResourceClass;
    use crate::admission::AdmissionPolicy;
    use crate::runtime::Reactor;
    use flow_control::pipe::MAX_PIPE_BYTES;
    use flow_control::pipe::PipePool;
    use http1::Header;
    use http1::MessageHead;
    use http1::StartLine;
    use std::future::Future;
    use std::io::Read;
    use std::io::Write;
    use std::net::TcpListener;
    use std::net::TcpStream;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Instant;
    use uring_runtime::reactor::IoBuffer;

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
    use super::tests::drive;

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
                let (source, destination, writer, reader) = f.connections(16 * 1024 * 1024 + 16);
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
        let (connection, pipe) = drive_worker(&f.reactor, async { futures::join!(relay, waiting) });
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
