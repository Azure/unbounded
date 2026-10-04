//! Racer policy adapters for the standalone fixed-length HTTP implementation.
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::ResourceClass;

use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::runtime::RequestScope;
use crate::runtime::Reactor;
use http1::MessageHead;
use std::path::PathBuf;
use std::rc::Rc;
use std::time::Duration;
use uring_runtime::reactor::Descriptor;
use uring_runtime::reactor::SocketAddress;

pub const MAX_HEAD_BYTES: usize = 32 * 1024;
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

#[cfg(test)]
pub(crate) mod tests;

#[cfg(test)]
mod relay_tests {
    //! Opaque transit, fallback, cancellation, and ownership-fence scenarios.
    use super::*;
    use crate::peer::transport::relay_body;
    use crate::http::acquire_wait;
    use crate::http::new_pipe_pool;
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
