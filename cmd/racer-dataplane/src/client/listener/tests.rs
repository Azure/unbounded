//! Client socket scenarios: ownership, publication, HTTP delivery, and retirement.
use super::*;
use std::time::Instant;
mod recovery;

#[test]
fn local_and_distributed_installs_drain_responses_and_retire_idle_generations() {
    use crate::{model::WorkerId, runtime::ingress::Ingress, test_support::WakeCounter};
    use std::task::Waker;

    for distributed in [false, true] {
        for busy in [false, true] {
            let mut acceptor = Fixture::new();
            let mut worker = Fixture::new();
            let ingress = Arc::new(Ingress::new(&[WorkerId(1)]));
            if distributed {
                ingress
                    .install(WorkerId(1), &worker.listeners.admission)
                    .unwrap();
                acceptor.listeners = acceptor.listeners.with_ingress(ingress.clone());
            }
            let receiver = if distributed {
                &mut worker
            } else {
                &mut acceptor
            };
            let reads = Rc::new(GatedRead {
                inner: receiver.reads.clone(),
                scopes: RefCell::new(Vec::new()),
                release: Cell::new(false),
                wake: RefCell::new(None),
            });
            receiver.listeners.reads = reads.clone();
            acceptor.reconcile(&[definition()]).unwrap();
            let mut socket = acceptor.connect();
            let wakes = Arc::new(WakeCounter::default());
            let waker = Waker::from(wakes.clone());
            let mut cx = Context::from_waker(&waker);
            if distributed {
                // Register the receiving worker before the acceptor delivers a socket.
                assert!(ingress.pop_batch::<1>(WorkerId(1), &waker, 1).unwrap()[0].is_none());
            }
            assert_eq!(acceptor.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
            assert_eq!(wakes.count(), 1, "new work must wake its owning worker");
            let receiver = if distributed { &worker } else { &acceptor };
            if distributed {
                assert_eq!(acceptor.listeners.active_connections(), 0);
                assert_eq!(receiver.listeners.active_connections(), 0);
                receiver.install_handoff(&ingress);
            }
            assert_eq!(
                receiver.listeners.active_connections_for(&definition().id),
                1
            );
            if busy {
                socket.write_all(&request("HEAD", "")).unwrap();
            }
            for _ in 0..16 {
                receiver.pump(16);
            }
            assert_eq!(reads.scopes.borrow().len(), usize::from(busy));
            acceptor.reconcile(&[]).unwrap();
            // Reusing a UID creates a fresh generation, not a revival of the old one.
            acceptor.reconcile(&[definition()]).unwrap();
            for _ in 0..16 {
                receiver.pump(16);
            }
            if busy {
                assert!(reads.scopes.borrow()[0].check().is_ok());
                assert_eq!(receiver.listeners.active_connections(), 1);
                reads.release.set(true);
                reads.wake.borrow().as_ref().unwrap().wake_by_ref();
            }
            let response = receiver.receive(&mut socket, true);
            if busy {
                assert!(response.starts_with(b"HTTP/1.1 200"));
            } else {
                assert!(response.is_empty());
            }
            assert_eq!(receiver.reads.calls.get(), usize::from(busy));
            assert_no_body_leases(receiver);
        }
    }
}

#[test]
fn queued_handoffs_reject_retired_generations_and_stopped_receivers() {
    use crate::{
        model::{ResourceClass, WorkerId},
        runtime::ingress::Ingress,
    };

    for stop_receiver in [false, true] {
        let mut acceptor = Fixture::new();
        let worker = Fixture::new();
        let ingress = Arc::new(Ingress::new(&[WorkerId(1)]));
        ingress
            .install(WorkerId(1), &worker.listeners.admission)
            .unwrap();
        acceptor.listeners = acceptor.listeners.with_ingress(ingress.clone());
        acceptor.reconcile(&[definition()]).unwrap();
        let mut socket = acceptor.connect();
        acceptor.pump(1);
        assert_eq!(
            worker
                .listeners
                .admission
                .used(ResourceClass::IngressConnection),
            1
        );
        assert_eq!(worker.listeners.active_connections(), 0);
        if stop_receiver {
            worker.listeners.stop_admission();
        } else {
            acceptor.reconcile(&[]).unwrap();
            acceptor.reconcile(&[definition()]).unwrap();
        }
        worker.install_handoff(&ingress);
        assert_eq!(worker.listeners.active_connections(), 0);
        assert_eq!(
            worker
                .listeners
                .admission
                .used(ResourceClass::IngressConnection),
            0
        );
        // A concurrent fork can briefly retain the closed socket until exec.
        // Wait for EOF through the same bounded path as other real UDS tests.
        assert!(worker.receive(&mut socket, true).is_empty());
        assert_eq!(worker.reads.calls.get(), 0);
        assert_no_body_leases(&worker);
    }
}

#[test]
fn simulated_listener_preparation_rollback_and_real_http_exchange() {
    use crate::runtime::reactor::{Descriptor, SocketAddress, simulation::Simulation};
    let sim = Simulation::new();
    let _environment = sim.enter();
    let admission = Rc::new(Admission::new(limits()));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(32768, i64::MAX as u64),
        admission.clone(),
    ));
    let delivery = Rc::new(Delivery::new(
        Rc::new(PipePool::new(admission.clone(), reactor.clone())),
        Duration::from_secs(2),
    ));
    let reads = Rc::new(Heads {
        calls: Cell::new(0),
    });
    let listeners = ClientListeners::new(
        reads.clone(),
        RequestParser::new(32768),
        Rc::new(Responses::new(io.clone(), delivery)),
        io,
        admission,
    );
    let definition = definition();
    futures::executor::block_on(listeners.reconcile(std::slice::from_ref(&definition), &scope()))
        .unwrap();
    let path = PathBuf::from("/run/racer/example/client/socket");
    let inode = sim.metadata(&path).unwrap().0;
    assert_eq!(sim.metadata(&path).unwrap().1 & 0o777, 0o666);
    let mut changed = definition.clone();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let prepared = futures::executor::block_on(listeners.prepare(&[changed], &scope())).unwrap();
    assert_ne!(sim.metadata(&path).unwrap().0, inode);
    drop(prepared);
    assert_eq!(sim.metadata(&path).unwrap().0, inode);
    assert_eq!(sim.metadata(&path).unwrap().1 & 0o777, 0o666);
    let client = sim.connect(SocketAddress::Unix(path)).unwrap();
    let Descriptor::Sim(client) = client else {
        unreachable!()
    };
    client.send(&request("HEAD", "")).unwrap();
    let mut response = Vec::new();
    let mut bytes = [0; 4096];
    for _ in 0..1000 {
        listeners
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                16,
            )
            .unwrap();
        reactor.poll_budgeted(16).unwrap();
        match client.recv(&mut bytes) {
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
        if response.windows(4).any(|w| w == b"\r\n\r\n") {
            break;
        }
    }
    assert!(response.starts_with(b"HTTP/1.1 200"), "{response:?}");
    assert_eq!(reads.calls.get(), 1);
    drop(client);
    listeners.stop_admission();
    drop(listeners);
    drop(reactor);
    assert_eq!(sim.live_handles(), 0);
}
use crate::{
    client::ClientRequest,
    http::Codec,
    memory::{delivery::Delivery, pipe::PipePool},
    model::{ExpiresAt, Limits, ObjectMetadata, ObjectVersion, StrongEtag},
    read::ReadResponse,
    runtime::reactor::Reactor,
};
use std::{
    io::{Read, Write},
    num::NonZeroUsize,
    os::unix::net::UnixStream,
    sync::atomic::{AtomicUsize, Ordering},
    time::UNIX_EPOCH,
};

static NEXT_ROOT: AtomicUsize = AtomicUsize::new(0);
struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        // Test files stay inside the shared worktree, even on unprivileged runs.
        let path = Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
            ".client-test-{}-{}",
            std::process::id(),
            NEXT_ROOT.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        Self(path)
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn limits() -> Limits {
    let n = NonZeroUsize::new(64).unwrap();
    let bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
    Limits {
        plaintext_bytes: bytes,
        ciphertext_bytes: bytes,
        dirty_bytes: bytes,
        registered_bytes: bytes,
        request_context_bytes: bytes,
        flights: n,
        waiters_per_flight: n,
        queue_entries: n,
        connections_per_neighbor: n,
        client_connections: n,
        pipes: n,
        range_window_pages: n,
        header_bytes: NonZeroUsize::new(32768).unwrap(),
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    }
}
fn definition() -> CacheDefinition {
    CacheDefinition {
        id: CacheId("00000000-0000-4000-8000-000000000001".into()),
        name: "example".into(),
        client_socket: "/run/racer/example/client/socket".into(),
        origin_socket: "/run/racer/example/origin/socket".into(),
    }
}
fn scope() -> RequestScope {
    new_scope(Duration::from_secs(5), Cancellation::new().unwrap()).unwrap()
}
struct Heads {
    calls: Cell<usize>,
}
impl ReadService for Heads {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            scope.check()?;
            self.calls.set(self.calls.get() + 1);
            Ok(ReadResponse {
                metadata: ObjectMetadata {
                    content_type: None,
                    version: ObjectVersion {
                        object: request.origin.object,
                        etag: StrongEtag::parse(b"\"v1\"")?,
                    },
                    length: 17,
                    expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_millis(1234)),
                },
                range: None,
                body: None,
            })
        })
    }
}
struct Fixture {
    root: Root,
    listeners: ClientListeners,
    reactor: Rc<Reactor>,
    reads: Rc<Heads>,
}
impl Fixture {
    fn install_handoff(&self, ingress: &crate::runtime::ingress::Ingress) {
        use crate::{model::WorkerId, runtime::ingress::Kind};
        let [accepted] = ingress
            .pop_batch::<1>(WorkerId(1), futures::task::noop_waker_ref(), 1)
            .unwrap();
        let accepted = accepted.expect("accepted socket queued for receiving worker");
        let Kind::Client(cache, retired) = accepted.kind else {
            panic!("client endpoint delivered a peer socket");
        };
        let connection =
            ConnectionLease::from_reserved(accepted.fd.into(), accepted.reservation).unwrap();
        self.listeners
            .install_connection(connection, cache, retired)
            .unwrap();
    }

    fn new() -> Self {
        Self::with_limits(limits())
    }
    fn with_limits(limits: Limits) -> Self {
        let root = Root::new();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(32768, i64::MAX as u64),
            admission.clone(),
        ));
        let delivery = Rc::new(Delivery::new(
            Rc::new(PipePool::new(admission.clone(), reactor.clone())),
            Duration::from_secs(2),
        ));
        let reads = Rc::new(Heads {
            calls: Cell::new(0),
        });
        let responses = Rc::new(Responses::new(io.clone(), delivery));
        let mut listeners = ClientListeners::new(
            reads.clone(),
            RequestParser::new(32768),
            responses,
            io,
            admission,
        );
        listeners.root = root.0.clone();
        Self {
            root,
            listeners,
            reactor,
            reads,
        }
    }
    fn reconcile(&self, caches: &[CacheDefinition]) -> Result<()> {
        futures::executor::block_on(self.listeners.reconcile(caches, &scope()))
    }
    fn socket(&self) -> PathBuf {
        self.root.0.join("example/client/socket")
    }
    fn assert_metrics(&self, requests: u64, errors: u64, active: u64) {
        use crate::telemetry::metrics::{Event, Gauge};
        assert_eq!(self.listeners.metrics.count(Event::Request), requests);
        assert_eq!(self.listeners.metrics.count(Event::RequestError), errors);
        assert_eq!(self.listeners.metrics.gauge(Gauge::ActiveRequests), active);
    }
    fn connect(&self) -> UnixStream {
        // /proc keeps sun_path short even in deeply nested CI worktrees.
        let directory = File::open(self.socket().parent().unwrap()).unwrap();
        let stream = UnixStream::connect(file_path(&directory).join("socket")).unwrap();
        stream.set_nonblocking(true).unwrap();
        stream
    }
    fn pump(&self, budget: usize) {
        self.listeners
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                budget,
            )
            .unwrap();
        self.reactor.poll_budgeted(64).unwrap();
    }
    fn receive(&self, socket: &mut UnixStream, eof: bool) -> Vec<u8> {
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut output = Vec::new();
        loop {
            self.pump(16);
            let mut bytes = [0; 8192];
            match socket.read(&mut bytes) {
                Ok(0) => return output,
                Ok(n) => output.extend_from_slice(&bytes[..n]),
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) => panic!("client read failed: {error}"),
            }
            if !eof && output.windows(4).any(|part| part == b"\r\n\r\n") {
                return output;
            }
            assert!(
                Instant::now() < deadline,
                "client exchange did not complete"
            );
        }
    }
}
fn request(method: &str, fields: &str) -> Vec<u8> {
    let body = if method == "POST" {
        "Content-Length: 0\r\n"
    } else {
        ""
    };
    format!(
        "{method} /v2/objects/{} HTTP/1.1\r\nHost: racer\r\n{body}{fields}\r\n",
        "0".repeat(64)
    )
    .into_bytes()
}

#[test]
fn client_listener_readiness_recovers_from_queue_pressure() {
    let mut limits = limits();
    limits.queue_entries = NonZeroUsize::new(8).unwrap();
    let fixture = Fixture::with_limits(limits);
    fixture.reconcile(&[definition()]).unwrap();
    let scope = scope();
    let (reader, _writer) = UnixStream::pair().unwrap();
    let reader = Rc::new(crate::runtime::reactor::Descriptor::from(reader));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pressure = Vec::new();
    for _ in 0..8 {
        let mut wait = fixture
            .reactor
            .readiness(reader.clone(), libc::POLLIN as u32, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        pressure.push(wait);
    }
    for _ in 0..32 {
        fixture.listeners.poll_budgeted(&mut cx, 16).unwrap();
        assert_eq!(fixture.reactor.in_flight(), 8);
    }
    drop(pressure);
    let mut client = fixture.connect();
    client
        .write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(
        fixture
            .receive(&mut client, true)
            .starts_with(b"HTTP/1.1 200")
    );
}

struct GatedRead {
    wake: RefCell<Option<std::task::Waker>>,
    inner: Rc<dyn ReadService>,
    scopes: RefCell<Vec<RequestScope>>,
    release: Cell<bool>,
}
impl ReadService for GatedRead {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            self.scopes.borrow_mut().push(scope.clone());
            std::future::poll_fn(|cx| {
                *self.wake.borrow_mut() = Some(cx.waker().clone());
                if let Err(error) = scope.check() {
                    Poll::Ready(Err(error))
                } else if self.release.get() {
                    Poll::Ready(Ok(()))
                } else {
                    Poll::Pending
                }
            })
            .await?;
            self.inner.read(request, scope).await
        })
    }
}

fn sleep_until(deadline: Instant) {
    std::thread::sleep(deadline.saturating_duration_since(Instant::now()));
}

#[test]
fn configured_timeout_bounds_idle_partial_headers_and_keepalive() {
    for mode in ["idle", "partial", "keepalive"] {
        let mut fixture = Fixture::new();
        assert_eq!(fixture.listeners.request_timeout(), Duration::from_secs(30));
        let timeout = Duration::from_millis(200);
        fixture.listeners = fixture.listeners.with_request_timeout(timeout);
        fixture.reconcile(&[definition()]).unwrap();
        let mut socket = fixture.connect();
        if mode == "keepalive" {
            socket.write_all(&request("HEAD", "")).unwrap();
            assert!(
                fixture
                    .receive(&mut socket, false)
                    .starts_with(b"HTTP/1.1 200 ")
            );
        }
        for _ in 0..16 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 1);
        assert_no_head(&mut socket);
        // Make progress inside the header budget, then cross its original
        // deadline before a renewed budget could expire.
        let started = Instant::now();
        if mode == "partial" {
            sleep_until(started + timeout / 2);
            socket.write_all(b"HEAD /v1/objects/").unwrap();
            for _ in 0..16 {
                fixture.pump(16);
            }
            assert_no_head(&mut socket);
        }
        sleep_until(started + timeout + Duration::from_millis(20));
        for _ in 0..64 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 0, "{mode}");
        assert_eq!(socket.read(&mut [0; 1]).unwrap(), 0, "{mode}");
        assert_eq!(fixture.reads.calls.get(), usize::from(mode == "keepalive"));
        assert_no_body_leases(&fixture);
    }
}

#[test]
fn configured_timeout_starts_fresh_operations_after_headers_and_on_reuse() {
    let mut fixture = Fixture::new();
    let timeout = Duration::from_millis(500);
    fixture.listeners = fixture.listeners.with_request_timeout(timeout);
    let reads = Rc::new(GatedRead {
        inner: fixture.reads.clone(),
        scopes: RefCell::new(Vec::new()),
        release: Cell::new(false),
        wake: RefCell::new(None),
    });
    fixture.listeners.reads = reads.clone();
    fixture.reconcile(&[definition()]).unwrap();
    let mut socket = fixture.connect();
    let head = request("HEAD", "");
    socket.write_all(&head[..head.len() - 2]).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert!(reads.scopes.borrow().is_empty());
    std::thread::sleep(Duration::from_millis(100));
    let before = Instant::now();
    socket.write_all(b"\r\n").unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let first = reads.scopes.borrow()[0].clone();
    assert!(first.deadline.0 >= before + timeout);
    assert!(first.deadline.0 <= Instant::now() + timeout);
    reads.release.set(true);
    if let Some(waker) = reads.wake.borrow().as_ref() {
        waker.wake_by_ref();
    }
    assert!(
        fixture
            .receive(&mut socket, false)
            .starts_with(b"HTTP/1.1 200 ")
    );
    reads.release.set(false);
    socket.write_all(&head).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let second = reads.scopes.borrow()[1].clone();
    assert_ne!(first.request, second.request);
    assert!(second.deadline.0 > first.deadline.0);
    assert_no_head(&mut socket);
    sleep_until(second.deadline.0 + Duration::from_millis(20));
    let output = fixture.receive(&mut socket, true);
    assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
    assert!(output.ends_with(b"\r\n\r\n"));
    assert_eq!(fixture.reads.calls.get(), 1);
    assert_no_body_leases(&fixture);
}

#[test]
fn accepted_client_wakes_before_first_poll_and_blocked_clients_are_fair() {
    use crate::test_support::WakeCounter;
    use std::{sync::Arc, task::Waker};
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let _socket = fixture.connect();
    let count = Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 0).unwrap(), 0);
    assert_eq!(count.count(), 0);
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    assert_eq!(fixture.listeners.active_connections(), 1);
    assert_eq!(
        count.count(),
        1,
        "accepted task has no I/O registration yet"
    );
    // Replace the not-yet-polled socket operation with deterministic futures
    // at the actual listener driver seam, avoiding kernel timing dependencies.
    fixture.listeners.active.borrow_mut().clear();
    fixture.listeners.accepting.set(false);
    let order = Rc::new(RefCell::new(Vec::new()));
    let mut senders = Vec::new();
    for id in 0..3 {
        let (send, mut receive) = futures::channel::oneshot::channel::<()>();
        senders.push(send);
        let order = order.clone();
        fixture.listeners.active.borrow_mut().push_back(Active {
            runnable: crate::read::drivers::Runnable::new(),
            deadline: Rc::new(Cell::new(Instant::now() + Duration::from_secs(5))),
            expired: None,
            cache: definition().id,
            retired: Arc::new(crate::runtime::ingress::Retired::default()),
            idle: Rc::new(Cell::new(false)),
            cancellation: Cancellation::new().unwrap(),
            operation: Box::pin(std::future::poll_fn(move |cx| {
                order.borrow_mut().push(id);
                std::pin::Pin::new(&mut receive)
                    .poll(cx)
                    .map(|r| r.map_err(|_| Error::Unavailable))
            })),
        });
    }
    for _ in 0..6 {
        assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    }
    assert_eq!(&*order.borrow(), &[0, 1, 2]);
    assert_eq!(count.count(), 1, "blocked clients do not self-wake");
    for active in fixture.listeners.active.borrow().iter() {
        std::task::Wake::wake_by_ref(&active.runnable);
    }
    for _ in 0..3 {
        fixture.listeners.poll_budgeted(&mut cx, 1).unwrap();
    }
    assert_eq!(&*order.borrow(), &[0, 1, 2, 0, 1, 2]);
    assert_eq!(
        count.count(),
        4,
        "only the three explicit notifications wake the worker"
    );
    std::thread::spawn(move || {
        for send in senders {
            send.send(()).unwrap();
        }
    })
    .join()
    .unwrap();
    assert_eq!(
        count.count(),
        7,
        "listener forwards the real completion waker"
    );
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 2).unwrap(), 2);
    assert_eq!(fixture.listeners.active_connections(), 1);
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 2).unwrap(), 1);
    assert_eq!(fixture.listeners.active_connections(), 0);
    let mut yielded = false;
    fixture.listeners.active.borrow_mut().push_back(Active {
        runnable: crate::read::drivers::Runnable::new(),
        deadline: Rc::new(Cell::new(Instant::now() + Duration::from_secs(5))),
        expired: None,
        cache: definition().id,
        retired: Arc::new(crate::runtime::ingress::Retired::default()),
        idle: Rc::new(Cell::new(false)),
        cancellation: Cancellation::new().unwrap(),
        operation: Box::pin(std::future::poll_fn(move |cx| {
            if yielded {
                Poll::Ready(Ok(()))
            } else {
                yielded = true;
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })),
    });
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    assert_eq!(
        count.count(),
        8,
        "cooperative client continuation reaches driver"
    );
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    assert_eq!(fixture.listeners.active_connections(), 0);
}

struct Bodies {
    admission: Rc<Admission>,
    streams: crate::read::range_stream::RangeStreams,
    late_failure: bool,
    unseeded: bool,
}
impl ReadService for Bodies {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            use crate::{
                memory::{
                    page::PageResult,
                    pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
                },
                model::{
                    ByteRange, KeyId, MembershipVersion, Nonce, PAGE_BYTES, PageEnvelope, PageId,
                    PageNumber, ResourceClass,
                },
                topology::membership::Membership,
            };
            use std::sync::Arc;
            let length = if self.late_failure { PAGE_BYTES + 1 } else { 5 };
            let metadata = ObjectMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: request.origin.object.clone(),
                    etag: StrongEtag::parse(b"\"v1\"")?,
                },
                length,
                expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_millis(1234)),
            };
            let requested = match &request.kind {
                super::super::ReadKind::Subscription { range, .. } => {
                    range.unwrap_or(ByteRange::From(0))
                }
                _ => ByteRange::Closed {
                    first: 0,
                    last: PAGE_BYTES - 1,
                },
            };
            let range = requested.resolve(length)?;
            let page = PageId {
                version: metadata.version.clone(),
                number: PageNumber(0),
            };
            let bytes = if self.late_failure {
                vec![b'x'; PAGE_BYTES as usize]
            } else {
                b"hello".to_vec()
            };
            let envelope = PageEnvelope {
                page: page.clone(),
                key_id: KeyId([0; 16]),
                nonce: Nonce([0; 24]),
                plaintext_length: bytes.len() as u32,
                ciphertext_length: bytes.len() as u32 + 16,
            };
            let plaintext = VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page,
                    reservation: self.admission.reserve(
                        None,
                        ResourceClass::Plaintext,
                        bytes.len(),
                    )?,
                    bytes,
                }),
            };
            let ciphertext = CiphertextPage {
                inner: Arc::new(CiphertextBytes {
                    checksum: std::sync::OnceLock::new(),
                    reservation: self.admission.reserve(
                        None,
                        ResourceClass::Ciphertext,
                        envelope.ciphertext_length as usize,
                    )?,
                    bytes: vec![0; envelope.ciphertext_length as usize],
                    envelope,
                }),
            };
            let seed = PageResult {
                metadata: metadata.clone(),
                plaintext,
                ciphertext,
            };
            let mut stream = self.streams.open_with_budget(
                metadata.clone(),
                range,
                request.origin,
                Arc::new(Membership::validate(MembershipVersion(1), vec![])?),
                scope.clone(),
                crate::read::flight::AcquisitionBudget::new(scope.deadline.0, 4, 4),
                if self.unseeded { None } else { Some(seed) },
            )?;
            if let super::super::ReadKind::Subscription {
                page_credits,
                byte_credits,
                ordered,
                ..
            } = request.kind
            {
                stream.configure_subscription(page_credits, byte_credits, ordered)?;
            }
            Ok(ReadResponse {
                metadata,
                range: Some(range),
                body: Some(stream),
            })
        })
    }
}

#[test]
fn actual_uds_nonempty_range_and_late_failure_truncates() {
    use crate::{
        model::WorkerId,
        read::{dispatch::WorkerDirectory, range_stream::RangeStreams},
        runtime::worker::WorkerMap,
    };
    use std::sync::Arc;
    for (late_failure, fields, expected_range, expected_length, expected_body) in [
        (
            false,
            "Range: bytes=0-16777215\r\n",
            "bytes 0-4/5",
            5,
            b"hello".as_slice(),
        ),
        (
            false,
            "If-Match: \"v1\"\r\nRange: bytes=1-3\r\n",
            "bytes 1-3/5",
            3,
            b"ell".as_slice(),
        ),
        (
            false,
            "If-Match: \"v1\"\r\nRange: bytes=2-\r\n",
            "bytes 2-4/5",
            3,
            b"llo".as_slice(),
        ),
        (
            false,
            "If-Match: \"v1\"\r\nRange: bytes=-2\r\n",
            "bytes 3-4/5",
            2,
            b"lo".as_slice(),
        ),
        (
            true,
            "If-Match: \"v1\"\r\nRange: bytes=16777214-16777216\r\n",
            "bytes 16777214-16777216/16777217",
            3,
            b"xx".as_slice(),
        ),
    ] {
        let mut fixture = Fixture::new();
        let admission = fixture.listeners.admission.clone();
        let failures = crate::telemetry::failures::Failures::default();
        let observer = failures.observer(WorkerId(0));
        let delivery = Rc::new(Delivery::new(
            Rc::new(PipePool::new(admission.clone(), fixture.reactor.clone())),
            Duration::from_secs(2),
        ));
        fixture.listeners.responses = Rc::new(
            Responses::new(fixture.listeners.io.clone(), delivery.clone())
                .with_observer(observer.clone()),
        );
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                vec![WorkerId(0)],
                4,
            )
            .unwrap(),
        );
        // Seed the first authenticated page; an unavailable owner for page one
        // causes a real acquisition failure only after the first slice escaped.
        fixture.listeners.reads = Rc::new(Bodies {
            admission,
            streams: RangeStreams::new(directory, delivery, 1).with_observer(observer),
            late_failure,
            unseeded: false,
        });
        fixture.reconcile(&[definition()]).unwrap();
        let mut socket = fixture.connect();
        socket
            .write_all(&request("POST", &format!("{fields}Connection: close\r\n")))
            .unwrap();
        let output = fixture.receive(&mut socket, true);
        let end = output
            .windows(4)
            .position(|part| part == b"\r\n\r\n")
            .unwrap()
            + 4;
        let head = std::str::from_utf8(&output[..end])
            .unwrap()
            .to_ascii_lowercase();
        assert!(head.starts_with("http/1.1 200"), "{head}");
        assert!(head.contains("content-type: application/octet-stream\r\n"));
        let overhead = if late_failure { 63 } else { 42 };
        assert!(head.contains(&format!(
            "content-length: {}\r\n",
            expected_length + overhead
        )));
        let start: u64 = expected_range
            .strip_prefix("bytes ")
            .unwrap()
            .split('-')
            .next()
            .unwrap()
            .parse()
            .unwrap();
        assert!(head.contains(&format!("racer-range-start: {start}\r\n")));
        assert_eq!(output[end], 1);
        assert_eq!(
            u64::from_be_bytes(output[end + 9..end + 17].try_into().unwrap()),
            start
        );
        assert_eq!(
            &output[end + 21..end + 21 + expected_body.len()],
            expected_body
        );
        if late_failure {
            assert_eq!(output.len(), end + 21 + expected_body.len());
        } else {
            assert_eq!(output.len(), end + 42 + expected_body.len());
            assert_eq!(output[end + 21 + expected_body.len()], 2);
        }
        let mut diagnostics = String::new();
        failures.write(&mut diagnostics).unwrap();
        if late_failure {
            assert!(
                diagnostics.contains("stage=NextSlice error=Unavailable"),
                "{diagnostics}"
            );
            assert!(
                diagnostics.contains("sent: 2, expected: 3"),
                "{diagnostics}"
            );
            assert!(
                diagnostics.contains("stage=PageDispatch error=Unavailable"),
                "{diagnostics}"
            );
        } else {
            assert!(diagnostics.starts_with("total=0 "), "{diagnostics}");
        }
    }
}

#[test]
fn subscription_retains_delivered_page_until_release_and_rejects_invalid_releases() {
    use crate::model::ResourceClass;
    for release in [Some((0u64, 2u32)), Some((0, 1)), Some((1, 2)), None] {
        let (fixture, pipes) = body_fixture_with_large_page(4, false, true);
        let mut socket = fixture.connect();
        socket
            .write_all(&request(
                "POST",
                "Range: bytes=16777214-16777216\r\nRacer-Page-Credits: 1\r\n",
            ))
            .unwrap();
        let mut output = fixture.receive(&mut socket, false);
        let end = output.windows(4).position(|b| b == b"\r\n\r\n").unwrap() + 4;
        let deadline = Instant::now() + Duration::from_secs(5);
        while output.len() < end + 23 {
            fixture.pump(16);
            let mut bytes = [0; 4096];
            match socket.read(&mut bytes) {
                Ok(0) => panic!("subscription closed before release"),
                Ok(n) => output.extend_from_slice(&bytes[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(e) => panic!("{e}"),
            }
            assert!(Instant::now() < deadline);
        }
        assert_eq!(&output[end + 21..], b"xx");
        for _ in 0..32 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 1);
        assert_eq!(
            fixture.listeners.admission.used(ResourceClass::Plaintext),
            crate::model::PAGE_BYTES as usize
        );
        let Some(release) = release else {
            drop(socket);
            for _ in 0..64 {
                fixture.pump(16);
            }
            assert_only_idle_pipes(&fixture, &pipes);
            continue;
        };
        let mut bytes = [0; 12];
        bytes[..8].copy_from_slice(&release.0.to_be_bytes());
        bytes[8..].copy_from_slice(&release.1.to_be_bytes());
        // Exercise fragmented release reception and retention of its prefix.
        socket.write_all(&bytes[..5]).unwrap();
        for _ in 0..16 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 1);
        socket.write_all(&bytes[5..]).unwrap();
        assert!(fixture.receive(&mut socket, true).is_empty());
        // A valid release admits the unavailable next page; invalid releases
        // close immediately. Neither path can emit Complete or leak the lease.
        assert_only_idle_pipes(&fixture, &pipes);
    }
}

fn body_fixture(queue: usize, unseeded: bool) -> (Fixture, Rc<PipePool>) {
    body_fixture_with_large_page(queue, unseeded, false)
}

fn body_fixture_with_large_page(
    queue: usize,
    unseeded: bool,
    large_page: bool,
) -> (Fixture, Rc<PipePool>) {
    use crate::{
        model::WorkerId,
        read::{dispatch::WorkerDirectory, range_stream::RangeStreams},
        runtime::worker::WorkerMap,
    };
    use std::sync::Arc;
    let mut limits = limits();
    limits.pipes = NonZeroUsize::new(1).unwrap();
    limits.queue_entries = NonZeroUsize::new(queue).unwrap();
    let mut fixture = Fixture::with_limits(limits);
    let admission = fixture.listeners.admission.clone();
    let pipes = Rc::new(PipePool::new(admission.clone(), fixture.reactor.clone()));
    let delivery = Rc::new(Delivery::new(pipes.clone(), Duration::from_secs(2)));
    let directory = Arc::new(
        WorkerDirectory::new(
            Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
            vec![WorkerId(0)],
            4,
        )
        .unwrap(),
    );
    fixture.listeners.responses = Rc::new(Responses::new(
        fixture.listeners.io.clone(),
        delivery.clone(),
    ));
    fixture.listeners.reads = Rc::new(Bodies {
        admission,
        streams: RangeStreams::new(directory, delivery, 1),
        late_failure: large_page,
        unseeded,
    });
    fixture.reconcile(&[definition()]).unwrap();
    (fixture, pipes)
}

fn start_body(fixture: &Fixture) -> UnixStream {
    let mut socket = fixture.connect();
    socket
        .write_all(&request(
            "POST",
            "If-Match: \"v1\"\r\nRange: bytes=0-4\r\nConnection: close\r\n",
        ))
        .unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    socket
}

fn assert_no_head(socket: &mut UnixStream) {
    assert_eq!(
        socket.read(&mut [0; 1]).unwrap_err().kind(),
        std::io::ErrorKind::WouldBlock
    );
}

fn assert_no_body_leases(fixture: &Fixture) {
    use crate::model::ResourceClass;
    assert_eq!(fixture.listeners.active_connections(), 0);
    for class in [
        ResourceClass::Pipe,
        ResourceClass::Plaintext,
        ResourceClass::Ciphertext,
        ResourceClass::Connection,
    ] {
        assert_eq!(fixture.listeners.admission.used(class), 0, "{class:?}");
    }
}

fn assert_only_idle_pipes(fixture: &Fixture, pipes: &crate::memory::pipe::PipePool) {
    fixture.listeners.admission.reclaim_buffers();
    use crate::model::ResourceClass;
    assert_eq!(fixture.listeners.active_connections(), 0);
    assert_eq!(
        fixture.listeners.admission.used(ResourceClass::Pipe),
        pipes.idle_count()
    );
    for class in [
        ResourceClass::Plaintext,
        ResourceClass::Ciphertext,
        ResourceClass::Connection,
    ] {
        assert_eq!(fixture.listeners.admission.used(class), 0, "{class:?}");
    }
}

#[test]
fn configured_timeout_is_not_renewed_by_response_or_stream_progress() {
    let (mut fixture, _pipes) = body_fixture_with_large_page(2, false, true);
    let timeout = Duration::from_millis(800);
    fixture.listeners = fixture.listeners.with_request_timeout(timeout);
    let reads = Rc::new(GatedRead {
        inner: fixture.listeners.reads.clone(),
        scopes: RefCell::new(Vec::new()),
        release: Cell::new(false),
        wake: RefCell::new(None),
    });
    fixture.listeners.reads = reads.clone();
    let mut socket = fixture.connect();
    socket
        .write_all(&request(
            "POST",
            "If-Match: \"v1\"\r\nRange: bytes=0-16777215\r\nConnection: close\r\n",
        ))
        .unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let scope = reads.scopes.borrow()[0].clone();
    // Consume part of the total budget before committing response headers.
    sleep_until(scope.deadline.0 - timeout / 2);
    reads.release.set(true);
    if let Some(waker) = reads.wake.borrow().as_ref() {
        waker.wake_by_ref();
    }
    let mut output = fixture.receive(&mut socket, false);
    assert!(output.starts_with(b"HTTP/1.1 200 "));
    let head_end = output
        .windows(4)
        .position(|part| part == b"\r\n\r\n")
        .unwrap()
        + 4;
    let mut bytes = [0; 8192];
    // Keep making observable body progress without draining the whole page.
    for _ in 0..4 {
        loop {
            fixture.pump(16);
            match socket.read(&mut bytes) {
                Ok(n) if n != 0 => {
                    output.extend_from_slice(&bytes[..n]);
                    break;
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    assert!(Instant::now() < scope.deadline.0);
                }
                result => panic!("expected streaming progress: {result:?}"),
            }
        }
    }
    assert!(output.len() > head_end);
    assert_eq!(fixture.listeners.active_connections(), 1);
    sleep_until(scope.deadline.0 + Duration::from_millis(20));
    for _ in 0..64 {
        fixture.pump(16);
    }
    // The initial budget is not renewed: it is no longer the body lifetime.
    // Delivery remains live under its independent write-stall bound.
    assert_eq!(fixture.listeners.active_connections(), 1);
    output.extend(fixture.receive(&mut socket, true));
    assert_eq!(
        output.len() - head_end,
        crate::model::PAGE_BYTES as usize + 42
    );
    assert!(
        output[head_end + 21..output.len() - 21]
            .iter()
            .all(|byte| *byte == b'x')
    );
    assert_only_idle_pipes(&fixture, &_pipes);
}

#[test]
fn client_disconnect_cancels_pending_metadata_before_acquisition_deadline() {
    let mut fixture = Fixture::new();
    let reads = Rc::new(GatedRead {
        inner: fixture.reads.clone(),
        scopes: RefCell::new(Vec::new()),
        release: Cell::new(false),
        wake: RefCell::new(None),
    });
    fixture.listeners.reads = reads.clone();
    fixture.reconcile(&[definition()]).unwrap();
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let scope = reads.scopes.borrow()[0].clone();
    assert_eq!(scope.check(), Ok(()));
    drop(socket);
    for _ in 0..64 {
        fixture.pump(16);
    }
    assert!(scope.cancellation.is_cancelled());
    assert!(Instant::now() < scope.deadline.0);
    assert_no_body_leases(&fixture);
}

#[test]
fn actual_uds_pipe_waiters_progress_within_budget_and_overflow_before_206() {
    // Duplex completion frames and the next response head may be in flight
    // together, in addition to listener readiness. Keep reactor headroom
    // while independently exercising the bounded FIFO pipe queue.
    let (mut fixture, pipes) = body_fixture(4, false);
    let failures = crate::telemetry::failures::Failures::default();
    fixture.listeners.responses = Rc::new(
        Responses::new(
            fixture.listeners.io.clone(),
            Rc::new(Delivery::new(pipes.clone(), Duration::from_secs(2))),
        )
        .with_observer(failures.observer(crate::model::WorkerId(0))),
    );
    let held = pipes.acquire().unwrap();
    let mut first = start_body(&fixture);
    let mut second = start_body(&fixture);
    let mut third = start_body(&fixture);
    let mut fourth = start_body(&fixture);
    assert_no_head(&mut first);
    assert_no_head(&mut second);
    assert_no_head(&mut third);
    assert_no_head(&mut fourth);
    let mut overflow = start_body(&fixture);
    let output = fixture.receive(&mut overflow, true);
    assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
    assert!(output.ends_with(b"\r\n\r\n"));
    drop(held);
    for socket in [&mut first, &mut second, &mut third, &mut fourth] {
        let output = fixture.receive(socket, true);
        let mut diagnostics = String::new();
        failures.write(&mut diagnostics).unwrap();
        assert!(
            output.starts_with(b"HTTP/1.1 200 "),
            "{output:?} {diagnostics}"
        );
        assert_eq!(&output[output.len() - 26..output.len() - 21], b"hello");
    }
    assert_only_idle_pipes(&fixture, &pipes);
}

#[test]
fn actual_uds_first_page_failure_is_complete_503() {
    let (mut fixture, _pipes) = body_fixture(2, true);
    let failures = crate::telemetry::failures::Failures::default();
    let delivery = Rc::new(Delivery::new(_pipes.clone(), Duration::from_secs(2)));
    fixture.listeners.responses = Rc::new(
        Responses::new(fixture.listeners.io.clone(), delivery)
            .with_observer(failures.observer(crate::model::WorkerId(0))),
    );
    // No owner is installed for the unseeded first page.
    let mut socket = start_body(&fixture);
    let output = fixture.receive(&mut socket, true);
    assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
    assert!(output.ends_with(b"\r\n\r\n"));
    assert_only_idle_pipes(&fixture, &_pipes);
    fixture.assert_metrics(1, 1, 0);
    let mut diagnostics = String::new();
    failures.write(&mut diagnostics).unwrap();
    assert!(
        diagnostics.contains("stage=FirstSlice error=Unavailable"),
        "{diagnostics}"
    );
    assert!(!diagnostics.contains("stage=NextSlice"));
}

#[test]
fn actual_uds_waiting_deadline_and_cache_shutdown_release_all_leases() {
    for cancel in [false, true] {
        let (mut fixture, pipes) = body_fixture(2, false);
        fixture.listeners = fixture
            .listeners
            .with_request_timeout(Duration::from_millis(200));
        let held = pipes.acquire().unwrap();
        let mut socket = start_body(&fixture);
        assert_no_head(&mut socket);
        fixture.assert_metrics(1, 0, 1);
        if cancel {
            fixture.listeners.cancel_cache(&definition().id).unwrap();
        } else {
            std::thread::sleep(Duration::from_millis(220));
        }
        let output = fixture.receive(&mut socket, true);
        assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
        assert!(output.ends_with(b"\r\n\r\n"));
        drop(held);
        futures::executor::block_on(fixture.listeners.drain(&scope())).unwrap();
        assert_only_idle_pipes(&fixture, &pipes);
        fixture.assert_metrics(1, 1, 0);
    }
}

#[test]
fn actual_uds_empty_bootstrap_and_cancelled_success() {
    struct Empty(bool);
    impl ReadService for Empty {
        fn read<'a>(
            &'a self,
            request: ClientRequest,
            scope: &'a RequestScope,
        ) -> Operation<'a, ReadResponse> {
            Box::pin(async move {
                if self.0 {
                    scope.cancel()?;
                }
                Ok(ReadResponse {
                    metadata: ObjectMetadata {
                        content_type: None,
                        version: ObjectVersion {
                            object: request.origin.object,
                            etag: StrongEtag::parse(b"\"\"")?,
                        },
                        length: 0,
                        expires_at: ExpiresAt(UNIX_EPOCH),
                    },
                    range: None,
                    body: None,
                })
            })
        }
    }
    let mut fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    for cancelled in [false, true] {
        fixture.listeners.reads = Rc::new(Empty(cancelled));
        let mut socket = fixture.connect();
        socket
            .write_all(&request("POST", "Connection: close\r\n"))
            .unwrap();
        let output = fixture.receive(&mut socket, true);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        let status = if cancelled { 503 } else { 200 };
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains(if cancelled {
            "content-length: 0\r\n"
        } else {
            "content-length: 21\r\n"
        }));
        assert!(!text.contains("content-range:"));
        if cancelled {
            assert!(text.ends_with("\r\n\r\n"));
        } else {
            assert_eq!(
                &output[output.len() - 21..],
                &[
                    2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
                ]
            );
        }
        if !cancelled {
            assert!(text.contains("etag: \"\"\r\n"));
            assert!(text.contains("racer-expires-at: 0\r\n"));
            assert!(text.contains("content-type: application/octet-stream\r\n"));
        }
        fixture.assert_metrics(if cancelled { 2 } else { 1 }, u64::from(cancelled), 0);
    }
}

#[test]
fn actual_uds_head_keepalive_removal_and_accept_fairness() {
    let fixture = Fixture::new();
    assert!(!fixture.socket().exists());
    fixture.reconcile(&[definition()]).unwrap();
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    let mut idle = fixture.connect();
    fixture.pump(1);
    let mut socket = fixture.connect();
    socket
        .write_all(&request("HEAD", "If-Match: \"v1\"\r\n"))
        .unwrap();
    // A waiting idle connection must not monopolize a single-unit poll budget.
    for _ in 0..8 {
        fixture.pump(1);
    }
    assert_eq!(fixture.listeners.active_connections(), 2);
    let first = fixture.receive(&mut socket, false);
    let text = std::str::from_utf8(&first).unwrap().to_ascii_lowercase();
    assert!(text.starts_with("http/1.1 200"));
    assert!(text.contains("content-length: 17\r\n"));
    assert!(text.contains("etag: \"v1\"\r\n"));
    assert!(text.contains("racer-expires-at: 1234\r\n"));
    assert!(text.ends_with("\r\n\r\n"));
    socket.write_all(&request("HEAD", "")).unwrap();
    fixture.receive(&mut socket, false);
    assert_eq!(fixture.reads.calls.get(), 2);
    fixture.reconcile(&[]).unwrap();
    assert!(!fixture.socket().exists());
    assert!(fixture.receive(&mut socket, true).is_empty());
    assert!(fixture.receive(&mut idle, true).is_empty());
    assert_eq!(fixture.reads.calls.get(), 2);
}

#[test]
fn actual_uds_errors_and_idle_drain() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    for (method, fields, status, extra) in [
        ("GET", "", "405", "allow: head, post\r\n"),
        (
            "POST",
            "Range: bytes=2-1\r\n",
            "400",
            "content-length: 0\r\n",
        ),
        (
            "HEAD",
            "Authorization: \r\n",
            "400",
            "content-length: 0\r\n",
        ),
    ] {
        let mut socket = fixture.connect();
        socket.write_all(&request(method, fields)).unwrap();
        let output = fixture.receive(&mut socket, true);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains(extra), "{text}");
        assert!(text.ends_with("\r\n\r\n"));
    }
    assert_eq!(fixture.reads.calls.get(), 0);
    let mut idle = fixture.connect();
    fixture.pump(16);
    fixture.pump(16);
    fixture.listeners.stop_admission();
    assert!(fixture.receive(&mut idle, true).is_empty());
    futures::executor::block_on(fixture.listeners.drain(&scope())).unwrap();
    assert_eq!(fixture.listeners.active_connections(), 0);
}

#[test]
fn actual_uds_rejected_raw_heads_receive_sdk_errors() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let mut oversized = request("HEAD", &format!("X-Padding: {}\r\n", "x".repeat(32768)));
    // Hit the cap without sending beyond it: a full unterminated head is 431.
    oversized.truncate(32768);
    for (raw, status) in [
        (request("HEAD", "Authorization:secret\r\n"), 400),
        (request("HEAD", "Authorization:  secret\r\n"), 400),
        (
            request("HEAD", "Content-Length: 0\r\nContent-Length: 0\r\n"),
            400,
        ),
        (request("HEAD", "Transfer-Encoding: chunked\r\n"), 400),
        (oversized, 431),
        (
            request("HEAD", &format!("Racer-Metadata: {}\r\n", "x".repeat(8193))),
            431,
        ),
    ] {
        let mut socket = fixture.connect();
        socket.write_all(&raw).unwrap();
        let output = fixture.receive(&mut socket, true);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains("content-length: 0\r\n"));
        assert!(text.ends_with("\r\n\r\n"));
    }
    assert_eq!(fixture.reads.calls.get(), 0);
}

#[test]
fn actual_uds_configured_head_cap_counts_only_wire_bytes() {
    for limit in [512, super::super::MAX_HEAD_BYTES] {
        let mut fixture = Fixture::new();
        fixture.listeners.parser = RequestParser::new(limit);
        fixture.reconcile(&[definition()]).unwrap();
        for separator in ["", " ", "\t", "  "] {
            let fields = format!(
                "Connection: close\r\nX:{separator}{}\r\n",
                "x".repeat(
                    limit
                        - request("HEAD", &format!("Connection: close\r\nX:{separator}\r\n")).len()
                )
            );
            let raw = request("HEAD", &fields);
            assert_eq!(raw.len(), limit);
            let mut socket = fixture.connect();
            socket.write_all(&raw).unwrap();
            let output = fixture.receive(&mut socket, true);
            assert!(output.starts_with(b"HTTP/1.1 200 "));
        }
        let admitted = fixture.reads.calls.get();
        // Exhaust the raw cap on an unterminated head. Decoded value bytes
        // alone would fit; only framing can reject this before dispatch.
        let mut raw = request("HEAD", &format!("X:{}\r\n", "x".repeat(limit)));
        raw.truncate(limit);
        let mut socket = fixture.connect();
        socket.write_all(&raw).unwrap();
        let output = fixture.receive(&mut socket, true);
        assert!(output.starts_with(b"HTTP/1.1 431 "));
        assert_eq!(fixture.reads.calls.get(), admitted);
    }
}

#[test]
fn actual_uds_configured_head_limit_counts_received_bytes() {
    let mut fixture = Fixture::new();
    let limit = 512;
    fixture.listeners.parser = RequestParser::new(limit);
    fixture.reconcile(&[definition()]).unwrap();
    for separator in ["", " ", "\t", " \t"] {
        let prefix = request("HEAD", &format!("X:{separator}"));
        // Replace the request helper's final CRLF with field data and the
        // complete terminator. Unknown-field whitespace stays on the wire.
        let mut exact = prefix[..prefix.len() - 2].to_vec();
        exact.resize(limit - 4, b'x');
        exact.extend_from_slice(b"\r\n\r\n");
        let mut socket = fixture.connect();
        socket.write_all(&exact).unwrap();
        let reply = fixture.receive(&mut socket, false);
        assert!(reply.starts_with(b"HTTP/1.1 200 "));
        drop(socket);

        // This is the first limit bytes of a limit+1-byte head: the final
        // LF lies beyond the configured cap, so framing must reject it.
        exact.insert(exact.len() - 4, b'x');
        exact.truncate(limit);
        let mut socket = fixture.connect();
        socket.write_all(&exact).unwrap();
        let reply = fixture.receive(&mut socket, true);
        assert!(reply.starts_with(b"HTTP/1.1 431 "));
    }
    assert_eq!(fixture.reads.calls.get(), 4);
}

#[test]
fn actual_uds_read_failures_and_immutable_result_validation() {
    struct Failing(Error);
    impl ReadService for Failing {
        fn read<'a>(
            &'a self,
            _: ClientRequest,
            scope: &'a RequestScope,
        ) -> Operation<'a, ReadResponse> {
            Box::pin(async move {
                if self.0 == Error::Cancelled {
                    scope.cancel()?;
                }
                Err(self.0)
            })
        }
    }
    let mut fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    for (error, fields, status, range) in [
        (Error::NotFound, "", 404, None),
        (Error::NotFound, "If-Match: \"v1\"\r\n", 412, None),
        (
            Error::UnsatisfiableRangeWithLength(17),
            "",
            416,
            Some("content-range: bytes */17\r\n"),
        ),
        (Error::Cancelled, "", 503, None),
        (Error::OriginRejected, "", 401, None),
        (Error::OriginForbidden, "", 403, None),
        (Error::Overloaded, "", 503, None),
    ] {
        fixture.listeners.reads = Rc::new(Failing(error));
        let mut socket = fixture.connect();
        socket.write_all(&request("HEAD", fields)).unwrap();
        let output = fixture.receive(&mut socket, true);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains("content-length: 0\r\n"));
        assert!(!text.contains("etag:") && !text.contains("racer-expires-at:"));
        if let Some(range) = range {
            assert!(text.contains(range));
        }
        assert!(text.ends_with("\r\n\r\n"));
    }
    struct WrongIdentity(bool);
    fixture.assert_metrics(7, 7, 0);
    assert_eq!(
        fixture
            .listeners
            .metrics
            .count(crate::telemetry::metrics::Event::Overload),
        1
    );
    impl ReadService for WrongIdentity {
        fn read<'a>(
            &'a self,
            mut request: ClientRequest,
            _: &'a RequestScope,
        ) -> Operation<'a, ReadResponse> {
            Box::pin(async move {
                if self.0 {
                    request.origin.object.key.0[0] ^= 1;
                }
                Ok(ReadResponse {
                    metadata: ObjectMetadata {
                        content_type: None,
                        version: ObjectVersion {
                            object: request.origin.object,
                            etag: StrongEtag::parse(b"\"other\"")?,
                        },
                        length: 0,
                        expires_at: ExpiresAt(UNIX_EPOCH),
                    },
                    range: None,
                    body: None,
                })
            })
        }
    }
    for wrong_object in [true, false] {
        fixture.listeners.reads = Rc::new(WrongIdentity(wrong_object));
        let mut socket = fixture.connect();
        socket
            .write_all(&request(
                "HEAD",
                if wrong_object {
                    ""
                } else {
                    "If-Match: \"v1\"\r\n"
                },
            ))
            .unwrap();
        let output = fixture.receive(&mut socket, true);
        assert!(output.starts_with(b"HTTP/1.1 502"));
    }
}

#[test]
fn prepared_transition_rolls_back_bind_chmod_and_drop() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let mut added = definition();
    added.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    added.name = "blocked".into();
    added.client_socket = "/run/racer/blocked/client/socket".into();
    added.origin_socket = "/run/racer/blocked/origin/socket".into();
    fs::write(fixture.root.0.join("blocked"), b"foreign").unwrap();
    assert!(
        futures::executor::block_on(
            fixture
                .listeners
                .prepare(&[changed.clone(), added], &scope())
        )
        .is_err()
    );
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    FAIL_CHMOD.with(|fail| fail.set(true));
    assert!(
        futures::executor::block_on(fixture.listeners.prepare(&[changed.clone()], &scope()))
            .is_err()
    );
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[changed.clone()], &scope()))
            .unwrap();
    assert_ne!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    assert!(matches!(
        futures::executor::block_on(fixture.listeners.prepare(&[], &scope())),
        Err(Error::Overloaded)
    ));
    drop(prepared);
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        3
    );
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    assert!(
        fixture
            .receive(&mut socket, false)
            .starts_with(b"HTTP/1.1 200")
    );
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[changed], &scope())).unwrap();
    prepared.commit();
    fixture.pump(16);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    assert!(fixture.receive(&mut socket, true).is_empty());
}

#[test]
fn abandoned_prepare_future_removes_temporary_socket() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let definitions = [changed];
    let scope = scope();
    let mut future = fixture.listeners.prepare(&definitions, &scope);
    let waker = futures::task::noop_waker();
    assert!(
        future
            .as_mut()
            .poll(&mut Context::from_waker(&waker))
            .is_pending()
    );
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        5
    );
    drop(future);
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        3
    );
    assert!(!fixture.listeners.preparing.get());
}

#[test]
fn prepared_rename_failure_restores_already_exchanged_paths() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let mut added = definition();
    added.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    added.name = "added".into();
    added.client_socket = "/run/racer/added/client/socket".into();
    added.origin_socket = "/run/racer/added/origin/socket".into();
    FAIL_RENAME_AFTER.with(|count| count.set(Some(1)));
    assert!(
        futures::executor::block_on(fixture.listeners.prepare(&[changed, added], &scope()))
            .is_err()
    );
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        3
    );
    assert_eq!(
        fs::read_dir(fixture.root.0.join("added/client"))
            .unwrap()
            .count(),
        1
    );
    let mut socket = fixture.connect();
    socket
        .write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(
        fixture
            .receive(&mut socket, true)
            .starts_with(b"HTTP/1.1 200")
    );
}

#[test]
fn prepared_uid_reuse_and_foreign_replacement_are_inode_safe() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let origin = fixture.root.0.join("example/origin");
    fs::create_dir(&origin).unwrap();
    fs::write(origin.join("socket"), b"origin").unwrap();
    let mut replacement = definition();
    replacement.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[replacement.clone()], &scope()))
            .unwrap();
    prepared.commit();
    fixture.pump(16);
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    assert!(
        fixture
            .listeners
            .listeners
            .borrow()
            .contains_key(&replacement.id)
    );
    replacement.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[replacement], &scope())).unwrap();
    fs::remove_file(fixture.socket()).unwrap();
    fs::write(fixture.socket(), b"foreign").unwrap();
    drop(prepared);
    assert_eq!(fs::read(fixture.socket()).unwrap(), b"foreign");
    assert_eq!(fs::read(origin.join("socket")).unwrap(), b"origin");
    assert!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .any(|entry| entry.unwrap().metadata().unwrap().ino() == inode)
    );
}

#[test]
fn removal_commit_drains_active_response_and_reused_uid_does_not_revive_old_keepalive() {
    let mut fixture = Fixture::new();
    let gated = Rc::new(GatedRead {
        inner: fixture.reads.clone(),
        scopes: RefCell::new(vec![]),
        release: Cell::new(false),
        wake: RefCell::new(None),
    });
    fixture.listeners.reads = gated.clone();
    fixture.reconcile(&[definition()]).unwrap();
    // Model the descriptor reference inherited by a concurrent fork before exec.
    // Releasing the endpoint must not wait for that unrelated reference to close.
    let inherited_lock = fixture.listeners.listeners.borrow()[&definition().id]
        .owner
        .as_ref()
        .unwrap()
        .lock
        .try_clone()
        .unwrap();
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert_eq!(gated.scopes.borrow().len(), 1);
    let operation_scope = gated.scopes.borrow()[0].clone();
    fixture.reconcile(&[]).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert!(operation_scope.check().is_ok());
    assert_eq!(fixture.listeners.active_connections(), 1);
    fixture.reconcile(&[definition()]).unwrap();
    drop(inherited_lock);
    gated.release.set(true);
    if let Some(waker) = gated.wake.borrow().as_ref() {
        waker.wake_by_ref();
    }
    let response = fixture.receive(&mut socket, true);
    assert!(response.starts_with(b"HTTP/1.1 200"));
    assert_eq!(fixture.listeners.active_connections(), 0);
    let mut new = fixture.connect();
    new.write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(fixture.receive(&mut new, true).starts_with(b"HTTP/1.1 200"));
    assert_eq!(fixture.reads.calls.get(), 2);
}

#[test]
fn per_cache_cancellation_drains_active_read_and_keeps_other_cache() {
    struct Waiting(Rc<Cell<usize>>);
    impl ReadService for Waiting {
        fn read<'a>(
            &'a self,
            _: ClientRequest,
            scope: &'a RequestScope,
        ) -> Operation<'a, ReadResponse> {
            Box::pin(async move {
                self.0.set(self.0.get() + 1);
                std::future::poll_fn(|_| match scope.check() {
                    Ok(()) => Poll::Pending,
                    Err(error) => Poll::Ready(Err(error)),
                })
                .await
            })
        }
    }
    let mut fixture = Fixture::new();
    let calls = Rc::new(Cell::new(0));
    fixture.listeners.reads = Rc::new(Waiting(calls.clone()));
    let mut other = definition();
    other.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    other.name = "other".into();
    other.client_socket = "/run/racer/other/client/socket".into();
    other.origin_socket = "/run/racer/other/origin/socket".into();
    fixture.reconcile(&[definition(), other.clone()]).unwrap();
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert_eq!(calls.get(), 1);
    assert_eq!(
        fixture.listeners.active_connections_for(&definition().id),
        1
    );
    fixture.listeners.cancel_cache(&definition().id).unwrap();
    assert!(
        fixture
            .receive(&mut socket, true)
            .starts_with(b"HTTP/1.1 503")
    );
    futures::executor::block_on(fixture.listeners.drain_cache(&definition().id, &scope())).unwrap();
    assert_eq!(
        fixture.listeners.active_connections_for(&definition().id),
        0
    );
    assert!(fixture.listeners.listeners.borrow().contains_key(&other.id));
    assert!(fixture.root.0.join("other/client/socket").exists());
}

#[test]
fn socket_lifecycle_rejects_symlinks_and_preserves_unowned_paths() {
    let fixture = Fixture::new();
    let origin = fixture.root.0.join("example/origin");
    fs::create_dir_all(&origin).unwrap();
    fs::write(origin.join("socket"), b"adapter owned").unwrap();
    fixture.reconcile(&[definition()]).unwrap();
    let mut updated = definition();
    updated.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    fixture.reconcile(&[updated]).unwrap();
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    // Replacing the pathname does not give us ownership of its replacement.
    fs::remove_file(fixture.socket()).unwrap();
    fs::write(fixture.socket(), b"replacement").unwrap();
    fixture.reconcile(&[]).unwrap();
    assert_eq!(fs::read(fixture.socket()).unwrap(), b"replacement");
    assert_eq!(fs::read(origin.join("socket")).unwrap(), b"adapter owned");
    assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
    fs::remove_file(fixture.socket()).unwrap();
    // The persistent lock deliberately survives endpoint removal. Move the
    // whole directory aside to exercise replacement by a symlink.
    fs::rename(
        fixture.socket().parent().unwrap(),
        fixture.root.0.join("old-client"),
    )
    .unwrap();
    std::os::unix::fs::symlink(&origin, fixture.socket().parent().unwrap()).unwrap();
    assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
    assert_eq!(fs::read(origin.join("socket")).unwrap(), b"adapter owned");
    let mut invalid = definition();
    invalid.name = "../escape".into();
    assert_eq!(fixture.reconcile(&[invalid]), Err(Error::InvalidRequest));
}
