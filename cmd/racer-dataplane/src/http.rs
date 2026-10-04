//! Racer policy adapters for the standalone fixed-length HTTP implementation.
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::ResourceClass;

use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::runtime::RequestScope;
use crate::runtime::Reactor;
use flow_control::pipe::MAX_PIPE_BYTES;
use flow_control::pipe::PipeLease;
use http1::MessageHead;
use std::cell::RefCell;
use std::ops::Deref;
use std::path::PathBuf;
use std::rc::Rc;
use std::task::Poll;
use std::time::Duration;
use uring_runtime::Descriptor;
use uring_runtime::IoBuffer;
use uring_runtime::SocketAddress;

pub const MAX_HEAD_BYTES: usize = 32 * 1024;
pub struct RacerOpaque;
impl http1::Opaque for RacerOpaque {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}
pub type Codec = http1::Codec<RacerOpaque>;
pub type ConnectionLease = http1::connection::ConnectionLease<HttpContext>;
pub type OwnedBuffer = http1::connection::OwnedBuffer<HttpContext>;
pub type HeadCompletion<T> = http1::connection::HeadCompletion<HttpContext, T>;

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
        reserve_connection(&self.0, ResourceClass::OutboundConnection)
    }
    fn stopped(&self) -> bool {
        self.0.is_stopped()
    }
}
#[derive(Default)]
pub struct State {
    pub(crate) peer_admission: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
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
    pub(crate) session: Option<crate::security::connection::Session>,
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
                permit.observe(crate::peer::adaptive::Outcome::Verified);
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
                peer.observe(crate::peer::adaptive::Outcome::PeerFailure);
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
        reserve_connection(&admission, ResourceClass::IngressConnection)?,
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
    session: crate::security::connection::Session,
) -> Result<()> {
    if connection.state().session.is_some() || connection.closing() {
        return Err(Error::Unauthorized);
    }
    connection.state_mut().session = Some(session);
    connection.begin_io();
    Ok(())
}
pub struct HttpIo(http1::connection::HttpIo<HttpContext>);
impl Deref for HttpIo {
    type Target = http1::connection::HttpIo<HttpContext>;
    fn deref(&self) -> &Self::Target {
        &self.0
    }
}
impl HttpIo {
    pub fn with_admission(
        reactor: Rc<Reactor>,
        codec: Codec,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        body_limit: u64,
    ) -> Self {
        Self(http1::connection::HttpIo::new(
            reactor,
            codec,
            Rc::new(HttpContext(admission)),
            body_limit,
            body_limit,
        ))
    }
    pub fn for_clients(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Self {
        Self(http1::connection::HttpIo::new(
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
        ))
    }
    pub fn capped(&self, limit: usize) -> Self {
        Self(self.0.capped(limit))
    }
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
pub struct HttpPool {
    core: http1::connection::HttpPool<HttpContext>,
    #[cfg(test)]
    reactor: Rc<Reactor>,
    #[cfg(test)]
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}
impl Deref for HttpPool {
    type Target = http1::connection::HttpPool<HttpContext>;
    fn deref(&self) -> &Self::Target {
        &self.core
    }
}
impl HttpPool {
    pub fn new(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        per_endpoint: usize,
    ) -> Self {
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
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        per_endpoint: usize,
        max_endpoints: usize,
        idle_timeout: Duration,
    ) -> Self {
        Self {
            core: http1::connection::HttpPool::new(
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
            ),
            #[cfg(test)]
            reactor,
            #[cfg(test)]
            admission,
        }
    }
    pub fn with_origin_limit(mut self, limit: usize) -> Self {
        self.core.config_mut().secondary_cap = limit;
        self
    }
    pub fn with_peer_tcp_nodelay(mut self, enabled: bool) -> Self {
        self.core.config_mut().tcp_nodelay = enabled;
        self
    }
    pub(crate) fn checkout_peer<'a>(
        &'a self,
        endpoint: &'a Endpoint,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        peer: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
        failure: Option<Rc<std::cell::Cell<bool>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.core.checkout_with_state(
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
}
fn peer_connect_failure(errno: Option<i32>) -> bool {
    matches!(
        errno,
        Some(libc::ECONNREFUSED | libc::ECONNRESET | libc::EPIPE)
    )
}
/// Racer-specific opaque relay. Both connections remain completion-owned.
struct Transit {
    source: ConnectionLease,
    destination: ConnectionLease,
    pipe: PipeLease<AdmissionPolicy>,
    fallback: Option<OwnedBuffer>,
    pending: std::ops::Range<usize>,
    copied: bool,
    #[cfg(test)]
    fallback_at: Option<usize>,
}
fn unsupported(error: &std::io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}
impl HttpIo {
    pub(crate) async fn relay_body(
        &self,
        source: ConnectionLease,
        destination: ConnectionLease,
        pipe: Option<PipeLease<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        if source.receive_remaining() != destination.send_remaining()
            || source.receive_remaining().is_none()
        {
            return Err(Error::InvalidRequest);
        }
        if source.receive_remaining() == Some(0) {
            let mut source = source;
            let mut destination = destination;
            finish(&mut source, &mut destination)?;
            destination.state_mut().relay_reservation = None;
            return Ok(destination);
        }
        #[cfg(test)]
        let copied = destination.state().relay_fallback;
        #[cfg(test)]
        let fallback_at = destination.state().relay_fallback_at;
        #[cfg(not(test))]
        let copied = false;
        let state = Rc::new(RefCell::new(Transit {
            source,
            destination,
            pipe: pipe.ok_or(Error::InvalidRequest)?,
            fallback: None,
            pending: 0..0,
            copied,
            #[cfg(test)]
            fallback_at,
        }));
        loop {
            scope.check()?;
            let wait = self.relay_chunk(&state)?;
            {
                let s = state.borrow();
                if s.source.receive_remaining() == Some(0)
                    && s.destination.send_remaining() == Some(0)
                {
                    break;
                }
            }
            self.wait_relay_progress(state.clone(), wait, scope).await?;
        }
        scope.check()?;
        let mut state = Rc::try_unwrap(state)
            .map_err(|_| Error::Internal)?
            .into_inner();
        finish(&mut state.source, &mut state.destination)?;
        state.destination.state_mut().relay_reservation = None;
        Ok(state.destination)
    }
    fn relay_chunk(&self, state: &RefCell<Transit>) -> Result<Option<(Rc<Descriptor>, i16)>> {
        let mut state = state.borrow_mut();
        let s = &mut *state;
        if s.destination.socket().peer_read_closed() {
            return Err(Error::Io);
        }
        let mut wait = None;
        for _ in 0..32 {
            let remaining = s.source.receive_remaining().ok_or(Error::InvalidRequest)? as usize;
            if s.pending.is_empty() && s.pipe.buffered() == 0 && remaining == 0 {
                break;
            }
            let writing = !s.pending.is_empty() || s.pipe.buffered() != 0;
            let result = if !s.pending.is_empty() {
                s.destination
                    .socket()
                    .try_send(&s.fallback.as_ref().unwrap().bytes()?[s.pending.clone()])
            } else if s.pipe.buffered() != 0 {
                #[cfg(test)]
                if s.fallback_at
                    .is_some_and(|threshold| remaining <= threshold)
                    && !s.copied
                {
                    s.copied = unsupported(&std::io::Error::from_raw_os_error(libc::EOPNOTSUPP));
                }
                if s.copied {
                    if s.fallback.is_none() {
                        s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                    }
                    let n = s
                        .pipe
                        .try_read(s.fallback.as_mut().unwrap().bytes_mut()?)
                        .map_err(|_| Error::Io)?;
                    s.pending = 0..n;
                    continue;
                }
                s.pipe.try_splice_connection(&s.destination.socket())
            } else if let Some((ahead, range)) = s.source.take_read_ahead() {
                let count = remaining.min(range.len()).min(MAX_PIPE_BYTES);
                if s.fallback.is_none() {
                    s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                }
                s.fallback.as_mut().unwrap().bytes_mut()?[..count]
                    .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                if range.len() > count {
                    // Preserve all excess so finish_exchange still rejects pipelining.
                    s.source
                        .restore_read_ahead(ahead, range.start + count..range.end)?;
                }
                s.pending = 0..count;
                s.source.consume_received(count)?;
                continue;
            } else if s.copied {
                if s.fallback.is_none() {
                    s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                }
                s.source.socket().try_recv(
                    &mut s.fallback.as_mut().unwrap().bytes_mut()?[..remaining.min(MAX_PIPE_BYTES)],
                )
            } else {
                s.pipe.try_splice_from(&s.source.socket(), remaining)
            };
            match result {
                Ok(0) => return Err(Error::Io),
                Ok(n) => {
                    if writing {
                        if !s.pending.is_empty() {
                            s.pending.start += n;
                        }
                        s.destination.consume_sent(n)?;
                    } else {
                        s.source.consume_received(n)?;
                        if s.copied {
                            s.pending = 0..n;
                        }
                    }
                }
                Err(error) if unsupported(&error) => s.copied = true,
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => (),
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    wait = Some((
                        if writing {
                            s.destination.socket()
                        } else {
                            s.source.socket()
                        },
                        if writing { libc::POLLOUT } else { libc::POLLIN },
                    ));
                    break;
                }
                Err(_) => return Err(Error::Io),
            }
        }
        Ok(wait)
    }
    async fn wait_relay_progress(
        &self,
        state: Rc<RefCell<Transit>>,
        wait: Option<(Rc<Descriptor>, i16)>,
        scope: &RequestScope,
    ) -> Result<()> {
        if let Some((fd, interest)) = wait {
            let mut tick = scope.clone();
            tick.deadline.0 = tick
                .deadline
                .0
                .min(uring_runtime::environment::now() + Duration::from_millis(10));
            match self
                .reactor()
                .readiness_with_lease(fd, interest as u32, state.clone(), &tick)
                .await
            {
                Ok(_) | Err(Error::DeadlineExceeded) => (),
                Err(error) => return Err(error),
            }
        } else {
            let mut yielded = false;
            std::future::poll_fn(|cx| {
                if yielded {
                    Poll::Ready(())
                } else {
                    yielded = true;
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await;
        }
        Ok(())
    }
}
fn finish(source: &mut ConnectionLease, destination: &mut ConnectionLease) -> Result<()> {
    if let Err(error) = source
        .finish_exchange()
        .and_then(|()| destination.finish_exchange())
    {
        source.poison();
        destination.poison();
        return Err(error);
    }
    Ok(())
}

#[cfg(test)]
pub(crate) mod tests;

#[cfg(test)]
mod relay_tests {
    //! Opaque transit, fallback, cancellation, and ownership-fence scenarios.
    use super::*;
    use crate::memory::acquire_wait;
    use crate::memory::new_pipe_pool;
    use crate::model::RequestId;
    use crate::model::ResourceClass;
    use crate::admission::AdmissionPolicy;
    use crate::runtime::Reactor;
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
        struct WakeReactor(uring_runtime::ReactorWake);
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
                io: HttpIo::with_admission(
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
                f.io.relay_body(source, destination, Some(pipe), &f.scope)
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
                let mut work = Box::pin(f.io.relay_body(source, destination, Some(pipe), &f.scope));
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
            f.io.relay_body(source, destination, Some(pipe), &f.scope),
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
        let mut relay = Box::pin(f.io.relay_body(source, destination, Some(pipe), &f.scope));
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
