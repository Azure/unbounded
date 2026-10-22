//! Production coordinator/range/selection/Fill graph across authenticated nodes.
use super::fill::*;
use crate::client::ClientRequest;
use crate::client::ReadKind;
use crate::control::Snapshot;
use crate::http::Codec;
use crate::http::Delivery;
use crate::model::ByteRange;
use crate::peer::PeerNetwork;
use crate::peer::forwarding::Forwarding;
use crate::peer::forwarding::VerifiedResponse;
use crate::peer::protocol::PeerRequest;
use crate::peer::protocol::Signatures;
use crate::peer::server::LocalPageService;
use crate::peer::server::PeerServer;
use crate::read::Coordinator;
use crate::test_support::security::network;
use crate::test_support::security::node;
use crate::topology::LinkHealth;
use crate::topology::Paths;
use controlplane::Published;
use racer_control_wire::MembershipVersion;

mod duplex_release {
    //! Exercise release credit through the real duplex response, not release_page.
    use super::*;
    use crate::client::Responses;
    use std::io::Read;
    use std::io::Write;

    #[test]
    fn duplex_exact_release_before_final_send_cqe_is_provisional() {
        use uring_runtime::reactor::simulation::Fault;
        use uring_runtime::reactor::simulation::Simulation;
        for mode in ["valid", "duplicate", "malformed", "short", "drop"] {
            let sim = Simulation::new();
            let _sim = sim.enter();
            let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
            let _owner = queue.enter();
            let signers = network(1);
            let mut f = fixture();
            f.reactor.init().unwrap();
            let membership = local_membership(&signers[0]);
            let (local, mut endpoint, _publication) =
                coordinator(&f, &signers[0], &membership, NoPeer::requester());
            let response = futures::executor::block_on(local.read(
                ClientRequest {
                    retain: None,
                    kind: ReadKind::Subscription {
                        pin: Some(f.page.version.etag.clone()),
                        range: None,
                        page_credits: 1,
                        byte_credits: PAGE_BYTES,
                    },
                    origin: OriginContext {
                        object: f.context.object.clone(),
                        metadata: None,
                        authorization: None,
                    },
                },
                &f.scope,
            ))
            .unwrap();
            let admission = f.fill.dependencies.admission.clone();
            let delivery = Rc::new(Delivery::new(f.reactor.clone(), Duration::from_secs(30)));
            let io = Rc::new(crate::http::new_io(
                f.reactor.clone(),
                Codec::new(32768),
                admission.clone(),
                i64::MAX as u64,
            ));
            let responses = Responses::new(io, delivery);
            let (socket, client) = sim.socket_pair();
            let connection = crate::http::from_accepted(socket, &admission).unwrap();
            let mut send = responses.send_subscription_unobserved(
                connection,
                response,
                &f.scope,
                Duration::from_secs(30),
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut bytes = Vec::new();
            let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
                endpoint.poll(&mut cx, 64).unwrap();
                uring_runtime::drivers::poll(&mut cx, 64);
                uring_runtime::group::Service::poll_budgeted(
                    &mut f.engine,
                    &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
                    64,
                )
                .unwrap();
                f.crypto.poll_budgeted(64).unwrap();
                f.reactor.poll_budgeted(64).unwrap();
            };
            let mut poll_cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut head_end = None;
            for _ in 0..1024 {
                assert!(send.as_mut().poll(&mut poll_cx).is_pending());
                pump(&mut endpoint);
                let mut scratch = [0; 32768];
                if let Ok(n) = client.try_recv(&mut scratch) {
                    bytes.extend_from_slice(&scratch[..n]);
                }
                head_end = bytes
                    .windows(4)
                    .position(|b| b == b"\r\n\r\n")
                    .map(|i| i + 4);
                if head_end.is_some_and(|n| bytes.len() == n + 21) {
                    break;
                }
            }
            let end = head_end.unwrap();
            assert_eq!(bytes.len(), end + 21);
            // Force the payload's final owned send, execute it, but withhold its CQE.
            sim.inject("splice", Fault::Errno(libc::EOPNOTSUPP))
                .unwrap();
            sim.inject("send", Fault::Errno(libc::EAGAIN)).unwrap();
            sim.inject("send", Fault::HoldCompletion(40)).unwrap();
            if mode == "short" {
                sim.set_max_chunk(2).unwrap();
            }
            let mut payload = [0; 3];
            let mut received = None;
            for _ in 0..8 {
                assert!(send.as_mut().poll(&mut poll_cx).is_pending());
                pump(&mut endpoint);
                if let Ok(n) = client.try_recv(&mut payload) {
                    received = Some(n);
                    break;
                }
            }
            let n = received.expect("final send executed before held completion");
            assert_eq!(n, if mode == "short" { 2 } else { 3 });
            assert_eq!(&payload[..n], &b"abc"[..n]);
            assert!(f.reactor.in_flight() > 0);
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
            assert!(admission.used(ResourceClass::Plaintext) >= 3);
            sim.set_max_chunk(usize::MAX).unwrap();
            let mut release = [0; 12];
            release[8..]
                .copy_from_slice(&(if mode == "malformed" { 2u32 } else { 3 }).to_be_bytes());
            assert_eq!(client.try_send(&release).unwrap(), 12);
            if mode == "duplicate" {
                assert_eq!(client.try_send(&release).unwrap(), 12);
            }
            let observed = send.as_mut().poll(&mut poll_cx);
            if matches!(mode, "duplicate" | "malformed") {
                assert!(matches!(observed, Poll::Ready(Err(Error::InvalidRequest))));
            } else {
                assert!(observed.is_pending());
            }
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                0,
                "direct sends do not own a pipe"
            );
            assert!(
                admission.used(ResourceClass::Plaintext) >= 3,
                "release cannot drop CQE owners"
            );
            if mode == "drop" || matches!(mode, "duplicate" | "malformed") {
                drop(send);
                for _ in 0..128 {
                    pump(&mut endpoint);
                }
            } else {
                let mut result = None;
                for _ in 0..128 {
                    pump(&mut endpoint);
                    if let Poll::Ready(value) = send.as_mut().poll(&mut poll_cx) {
                        result = Some(value);
                        break;
                    }
                }
                let result = result.expect("held CQE eventually observed");
                if mode == "short" {
                    assert!(matches!(result, Err(Error::InvalidRequest)));
                } else {
                    assert!(result.is_ok());
                }
                drop(send);
            }
            for _ in 0..128 {
                pump(&mut endpoint);
            }
            assert_eq!(f.reactor.in_flight(), 0);
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            endpoint.uninstall().unwrap();
        }
    }
    #[test]
    fn duplex_release_replenishes_acquisition_while_next_page_write_is_blocked() {
        run("valid");
    }
    #[test]
    fn duplex_release_malformed_length_and_incomplete_page_are_rejected_while_blocked() {
        run("malformed");
        run("incomplete");
    }
    #[test]
    fn duplex_release_eof_cancellation_and_drop_drain_owned_write_fences() {
        run("eof");
        run("cancel");
        run("drop");
    }
    #[test]
    fn duplex_release_stalled_writer_expires_but_progressing_writer_outlives_read_deadline() {
        run("deadline");
        run("progress");
    }
    #[test]
    fn duplex_release_partial_frame_survives_delivery_to_next_slice_transition() {
        run("transition");
    }

    fn run(mode: &str) {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _owner = queue.enter();
        let clock = uring_runtime::environment::SimulationClock::new(311);
        let environment = clock.environment(0);
        let _clock = environment.enter();
        let signers = network(1);
        let mut f = fixture_with(2 * PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        let membership = local_membership(&signers[0]);
        let (local, mut endpoint, _publication) =
            coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let scope = RequestScope::new(
            f.scope.request,
            uring_runtime::environment::now() + Duration::from_secs(60),
        )
        .unwrap();
        let request = ClientRequest {
            retain: None,
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::From(PAGE_BYTES - 1)),
                page_credits: 2,
                byte_credits: 2 * PAGE_BYTES,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        let response = futures::executor::block_on(local.read(request, &scope)).unwrap();
        let admission = f.fill.dependencies.admission.clone();
        let delivery = Rc::new(Delivery::new(f.reactor.clone(), Duration::from_secs(30)));
        let io = Rc::new(crate::http::new_io(
            f.reactor.clone(),
            Codec::new(32768),
            admission.clone(),
            i64::MAX as u64,
        ));
        let responses = Responses::new(io, delivery.clone());
        let (socket, mut client) = std::os::unix::net::UnixStream::pair().unwrap();
        client.set_nonblocking(true).unwrap();
        let connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
        let mut send = responses.send_subscription_unobserved(
            connection,
            response,
            &scope,
            Duration::from_secs(30),
        );
        #[derive(Default)]
        struct Wakes(std::sync::atomic::AtomicUsize);
        impl futures::task::ArcWake for Wakes {
            fn wake_by_ref(this: &Arc<Self>) {
                this.0.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            }
        }
        let wakes = Arc::new(Wakes::default());
        let waker = futures::task::waker(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
            pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
        };
        // Read just the HTTP head, complete one-byte page zero, and page-one frame.
        // Leaving the entire page-one payload unread forces actual socket backpressure.
        let mut prefix = Vec::new();
        let mut head_end = None;
        for _ in 0..8192 {
            if let Poll::Ready(result) = send.as_mut().poll(&mut cx) {
                panic!("response completed before prefix: {:?}", result.err());
            }
            pump(&mut endpoint);
            let mut byte = [0];
            match client.read(&mut byte) {
                Ok(1) => prefix.push(byte[0]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                other => panic!("prefix read: {other:?}"),
            }
            if head_end.is_none() && prefix.ends_with(b"\r\n\r\n") {
                head_end = Some(prefix.len());
            }
            if head_end.is_some_and(|end| prefix.len() == end + 43) {
                break;
            }
        }
        let end = head_end.expect("HTTP response head");
        assert_eq!(prefix.len(), end + 43);
        let page_frame = |number: u64, offset: u64, length: u32| {
            let mut frame = vec![1];
            frame.extend(number.to_be_bytes());
            frame.extend(offset.to_be_bytes());
            frame.extend(length.to_be_bytes());
            frame
        };
        assert_eq!(&prefix[end..end + 21], page_frame(0, PAGE_BYTES - 1, 1));
        assert_eq!(prefix[end + 21], 0);
        assert_eq!(
            &prefix[end + 22..],
            page_frame(1, PAGE_BYTES, PAGE_BYTES as u32)
        );
        for _ in 0..128 {
            assert!(send.as_mut().poll(&mut cx).is_pending());
            pump(&mut endpoint);
        }
        assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
        assert!(
            f.reactor.in_flight() > 0,
            "blocked send owns a completion fence"
        );
        // An independent reader retains the same immutable page with its own pipe.
        let other_scope = RequestScope::new(f.scope.request, scope.deadline.0).unwrap();
        let other = delivery
            .attach(
                f.fill
                    .dependencies
                    .memory
                    .get(&f.page)
                    .unwrap()
                    .unwrap()
                    .plaintext,
                crate::model::PageSlice {
                    page: PageNumber(0),
                    offset: (PAGE_BYTES - 1) as u32,
                    length: 1,
                },
            )
            .unwrap();
        let mut release = [0; 12];
        release[8..].copy_from_slice(&1u32.to_be_bytes());
        // Pump only the reactor after sending input. A blocked writer must be woken
        // by POLLIN, rather than relying on unrelated output/acquisition wakeups.
        let before = wakes.0.load(std::sync::atomic::Ordering::Relaxed);
        client.write_all(&release[..5]).unwrap();
        for _ in 0..128 {
            pump(&mut endpoint);
            if wakes.0.load(std::sync::atomic::Ordering::Relaxed) > before {
                break;
            }
        }
        assert!(
            wakes.0.load(std::sync::atomic::Ordering::Relaxed) > before,
            "release input wakes blocked delivery"
        );
        for _ in 0..128 {
            assert!(send.as_mut().poll(&mut cx).is_pending());
            pump(&mut endpoint);
        }
        assert_eq!(
            &*f.origin.started_pages.borrow(),
            &[0, 1],
            "partial release grants no credit"
        );
        let mut received = Vec::new();
        if matches!(
            mode,
            "malformed" | "incomplete" | "eof" | "cancel" | "drop" | "deadline"
        ) {
            let expected = match mode {
                "malformed" => {
                    release[8..].copy_from_slice(&2u32.to_be_bytes());
                    client.write_all(&release[5..]).unwrap();
                    Error::InvalidRequest
                }
                "incomplete" => {
                    release[5..8].copy_from_slice(&[0, 0, 1]);
                    release[8..].copy_from_slice(&(PAGE_BYTES as u32).to_be_bytes());
                    client.write_all(&release[5..]).unwrap();
                    Error::InvalidRequest
                }
                "eof" => {
                    client.shutdown(std::net::Shutdown::Write).unwrap();
                    Error::Io
                }
                "cancel" => {
                    scope.cancel().unwrap();
                    Error::Cancelled
                }
                "deadline" => {
                    clock.advance(Duration::from_secs(31));
                    Error::DeadlineExceeded
                }
                "drop" => Error::Cancelled,
                _ => unreachable!(),
            };
            if mode != "drop" {
                let mut error = None;
                for _ in 0..1024 {
                    if let Poll::Ready(result) = send.as_mut().poll(&mut cx) {
                        error = Some(result.err().expect("blocked response must fail"));
                        break;
                    }
                    pump(&mut endpoint);
                }
                assert_eq!(error, Some(expected), "{mode}");
            }
            drop(send);
            if matches!(mode, "malformed" | "incomplete" | "eof" | "drop") {
                assert!(
                    f.reactor.in_flight() > 0,
                    "abandonment does not drop accepted write ownership before its fence"
                );
            }
            assert_eq!(
                &*f.origin.started_pages.borrow(),
                &[0, 1],
                "invalid release never grants acquisition credit"
            );
        } else {
            if mode == "transition" {
                // Finish page one with the page-zero release still incomplete. The
                // next_slice credit wait must resume that parser, not start a new frame.
                for _ in 0..8192 {
                    assert!(send.as_mut().poll(&mut cx).is_pending());
                    pump(&mut endpoint);
                    let mut bytes = [0; 65536];
                    match client.read(&mut bytes) {
                        Ok(n) => received.extend_from_slice(&bytes[..n]),
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                        other => panic!("transition read: {other:?}"),
                    }
                    if received.len() == PAGE_BYTES as usize {
                        break;
                    }
                }
                assert_eq!(received.len(), PAGE_BYTES as usize);
                for _ in 0..128 {
                    assert!(send.as_mut().poll(&mut cx).is_pending());
                    pump(&mut endpoint);
                }
                assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
            }
            if mode == "progress" {
                // Free socket capacity and let delivery make progress before the read
                // alarm expires. The receive side must not impose a total page deadline.
                clock.advance(Duration::from_secs(20));
                for _ in 0..32 {
                    let mut bytes = [0; 65536];
                    match client.read(&mut bytes) {
                        Ok(n) if n > 0 => received.extend_from_slice(&bytes[..n]),
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                        other => panic!("progress drain: {other:?}"),
                    }
                }
                assert!(!received.is_empty());
                for _ in 0..128 {
                    assert!(send.as_mut().poll(&mut cx).is_pending());
                    pump(&mut endpoint);
                }
                let mut bytes = [0; 65536];
                let n = client
                    .read(&mut bytes)
                    .expect("writer refilled the drained socket");
                assert!(n > 0);
                received.extend_from_slice(&bytes[..n]);
                clock.advance(Duration::from_secs(11));
                for _ in 0..128 {
                    assert!(send.as_mut().poll(&mut cx).is_pending());
                    pump(&mut endpoint);
                }
            }
            client.write_all(&release[5..]).unwrap();
            let third = PageId {
                version: f.page.version.clone(),
                number: PageNumber(2),
            };
            for _ in 0..4096 {
                assert!(send.as_mut().poll(&mut cx).is_pending());
                pump(&mut endpoint);
                if f.fill
                    .dependencies
                    .memory
                    .ciphertext(&third)
                    .unwrap()
                    .is_some()
                {
                    break;
                }
            }
            assert_eq!(
                &*f.origin.started_pages.borrow(),
                &[0, 1, 2],
                "wire release must refill acquisition before page-one write completes"
            );
            assert!(
                f.fill
                    .dependencies
                    .memory
                    .ciphertext(&third)
                    .unwrap()
                    .is_some()
            );
            let mut done = None;
            for _ in 0..8192 {
                if done.is_none()
                    && let Poll::Ready(result) = send.as_mut().poll(&mut cx)
                {
                    done = Some(result.unwrap());
                }
                pump(&mut endpoint);
                let mut bytes = [0; 65536];
                match client.read(&mut bytes) {
                    Ok(n) => received.extend_from_slice(&bytes[..n]),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                    other => panic!("payload read: {other:?}"),
                }
                if done.is_some() && received.len() == PAGE_BYTES as usize + 21 + 7 + 21 {
                    break;
                }
            }
            assert!(
                done.is_some(),
                "subscription completed without final releases"
            );
            let n = PAGE_BYTES as usize;
            assert_eq!(received.len(), n + 49);
            assert!(received[..n].iter().all(|b| *b == 1));
            assert_eq!(&received[n..n + 21], page_frame(2, 2 * PAGE_BYTES, 7));
            assert_eq!(&received[n + 21..n + 28], &[2; 7]);
            let mut completion = vec![2];
            completion.extend(3u64.to_be_bytes());
            completion.extend((PAGE_BYTES + 8).to_be_bytes());
            completion.extend(0u32.to_be_bytes());
            assert_eq!(&received[n + 28..], completion);
            drop((send, done));
        }
        assert_eq!(other.bytes_sent(), 0);
        assert!(!other_scope.cancellation.is_cancelled());
        let (socket, mut independent_client) = std::os::unix::net::UnixStream::pair().unwrap();
        independent_client
            .set_read_timeout(Some(Duration::from_secs(1)))
            .unwrap();
        let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
        // Model a sent one-byte response head for this independent delivery fixture.
        connection.set_framing(None, Some(1), false);
        let mut finish = delivery.finish_to(other, connection, &other_scope);
        let mut finished = false;
        for _ in 0..1024 {
            if let Poll::Ready(result) = finish.as_mut().poll(&mut cx) {
                result.unwrap();
                finished = true;
                break;
            }
            pump(&mut endpoint);
        }
        assert!(finished, "independent reader completes");
        drop(finish);
        let mut byte = [255];
        independent_client.read_exact(&mut byte).unwrap();
        assert_eq!(byte, [0], "other reader retains its own page and cursor");
        for _ in 0..128 {
            pump(&mut endpoint);
        }
        // A fixed pump count does not bound kernel completion latency. In eof mode the
        // peer's SHUT_WR wakes the blocked send, and its EAGAIN retry can move to
        // io-wq, where ASYNC_CANCEL completes asynchronously. Wait on the ring under
        // a host watchdog until the abandoned write's fence drains.
        let until = Instant::now() + Duration::from_secs(5);
        while f.reactor.in_flight() != 0 && Instant::now() < until {
            pump(&mut endpoint);
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(
            f.reactor.in_flight(),
            0,
            "all socket completion fences drained"
        );
        endpoint.uninstall().unwrap();
    }
}

struct Link {
    client: Rc<Requester>,
    /// The only node with a reachable peer endpoint.
    provider: racer_control_wire::NodeId,
    demands: Rc<RefCell<Vec<u64>>>,
    stats: Rc<LinkStats>,
}
/// Subscribe traffic one requester node sent.
#[derive(Default)]
struct LinkStats {
    /// Drop multi-flight Subscribes without a response, as a provider
    /// predating `Subscription::flights` does.
    old_provider: bool,
    outstanding: Cell<usize>,
    max_outstanding: Cell<usize>,
    flights: RefCell<Vec<Option<u16>>>,
}
impl Link {
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &racer_control_wire::NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _: PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        panic!("hot-read demand fixture does not admit direct hedges")
    }
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            // Requester endpoints are unbound, so a request to one cannot
            // succeed. Refuse it here as the closed port would: otherwise the
            // health of the refused direct path decides whether the requester
            // retries through the provider's relay, whose outbound stack is a
            // stub, and the read then stalls on wall-clock timing.
            if request.route.destination != self.provider {
                return Err(Error::Os(libc::ECONNREFUSED));
            }
            // Direct window fills send page requests; selections send Subscribe.
            let PeerOperation::Subscribe { subscription, .. } = &request.operation else {
                return self.client.request(request, membership, scope).await;
            };
            self.demands
                .borrow_mut()
                .push(subscription.demand.page_count());
            self.stats.flights.borrow_mut().push(subscription.flights);
            if self.stats.old_provider && subscription.flights.is_some() {
                return Err(Error::Io);
            }
            struct Outstanding<'a>(&'a Cell<usize>);
            impl Drop for Outstanding<'_> {
                fn drop(&mut self) {
                    self.0.set(self.0.get() - 1);
                }
            }
            let outstanding = self.stats.outstanding.get() + 1;
            self.stats.outstanding.set(outstanding);
            let _outstanding = Outstanding(&self.stats.outstanding);
            self.stats
                .max_outstanding
                .set(self.stats.max_outstanding.get().max(outstanding));
            self.client.request(request, membership, scope).await
        })
    }
}
struct Gate {
    local: Rc<Coordinator>,
    blocked: Cell<bool>,
    calls: RefCell<Vec<u64>>,
}
impl LocalPageService for Gate {
    fn serve_peer<'a>(
        &'a self,
        request: crate::peer::forwarding::VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            let PeerOperation::Page { page, .. } = &request.request().operation else {
                panic!()
            };
            self.calls.borrow_mut().push(page.number.0);
            std::future::poll_fn(|cx| {
                cx.waker().wake_by_ref();
                if self.blocked.get() {
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
            Ok(self
                .local
                .serve_peer(request, membership, scope)
                .await
                .expect("provider Fill"))
        })
    }
}
fn local_membership(signer: &Signatures) -> std::sync::Arc<crate::topology::Membership> {
    Arc::new(
        Membership::validate(
            MembershipVersion(1),
            vec![Member {
                node: signer.node().clone(),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    )
}

pub(super) fn coordinator(
    f: &Fixture,
    signer: &Rc<Signatures>,
    membership: &std::sync::Arc<crate::topology::Membership>,
    peers: Rc<Requester>,
) -> (
    Rc<Coordinator>,
    crate::read::dispatch::WorkerEndpoint,
    Arc<Published<Snapshot>>,
) {
    coordinator_with_window(f, signer, membership, peers, 2)
}

/// A read graph whose range streams use a `window`-page fetch window.
fn coordinator_with_window(
    f: &Fixture,
    signer: &Rc<Signatures>,
    membership: &std::sync::Arc<crate::topology::Membership>,
    peers: Rc<Requester>,
    window: usize,
) -> (
    Rc<Coordinator>,
    crate::read::dispatch::WorkerEndpoint,
    Arc<Published<Snapshot>>,
) {
    let mut deps = f.fill.dependencies.clone();
    deps.candidates = Rc::new(CandidatePolicy::new(
        signer.node().clone(),
        Rc::new(Placement::new(64)),
        peers.clone(),
        deps.credentials.clone(),
        Arc::new(controlplane::Published::new(Snapshot::retention(2))),
    ));
    f.read_graph(
        Rc::new(Fill::new(deps)),
        membership,
        ReadGraphSettings {
            name: "hot-read",
            snapshots: 2,
            metadata: 32,
            window,
            stall: Duration::from_secs(30),
            seed: Some(f.origin.metadata.immutable()),
        },
    )
}

#[test]
fn concurrent_reads_share_page_work_and_bound_fetch_by_credit() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let signers = network(1);
    let mut f = fixture_with(3 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let open = |credits, start| {
        let request = ClientRequest {
            retain: None,
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::From(start)),
                page_credits: credits,
                byte_credits: PAGE_BYTES,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
        match futures::executor::block_on(local.read(request, &scope))
            .unwrap()
            .payload
        {
            crate::read::ReadPayload::Stream(stream) => stream,
            crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
        }
    };
    // A one-byte first slice plus a whole page exceeds the byte credit despite
    // two available page slots. No page-one work may start yet.
    let mut a = open(2, PAGE_BYTES - 1);
    let mut slow = open(1, PAGE_BYTES - 1);
    let mut third = open(1, PAGE_BYTES - 1);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
        pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
    };
    f.origin.blocked_pages.borrow_mut().insert(0);
    for _ in 0..32 {
        assert!(a.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(slow.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(third.next_slice().as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoint);
    }
    assert_eq!(
        &*f.origin.started_pages.borrow(),
        &[0],
        "same-page work singleflights across readers"
    );
    f.origin.blocked_pages.borrow_mut().clear();
    let mut first = None;
    let mut slow_first = None;
    let mut third_first = None;
    for _ in 0..1024 {
        if first.is_none()
            && let Poll::Ready(result) = a.next_slice().as_mut().poll(&mut cx)
        {
            first = result.unwrap();
        }
        if slow_first.is_none()
            && let Poll::Ready(result) = slow.next_slice().as_mut().poll(&mut cx)
        {
            slow_first = result.unwrap();
        }
        if third_first.is_none()
            && let Poll::Ready(result) = third.next_slice().as_mut().poll(&mut cx)
        {
            third_first = result.unwrap();
        }
        if first.is_some() && slow_first.is_some() && third_first.is_some() {
            break;
        }
        pump(&mut endpoint);
    }
    assert_eq!(first.as_ref().unwrap().slice().length, 1);
    assert!(slow_first.is_some() && third_first.is_some());
    assert_eq!(
        f.origin.calls.get(),
        1,
        "every reader reuses the verified page result"
    );
    drop(first);
    a.release_page(PageNumber(0), 1).unwrap();
    // Only prefetch, not next_slice, drives real new work while readers retain
    // their current delivery leases. The one-page byte cap stops page two.
    for _ in 0..1024 {
        a.poll_prefetch(&mut cx);
        pump(&mut endpoint);
        if f.fill
            .dependencies
            .memory
            .get(&PageId {
                version: f.page.version.clone(),
                number: PageNumber(1),
            })
            .unwrap()
            .is_some()
        {
            break;
        }
    }
    assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
    let second = futures::executor::block_on(a.next_slice())
        .unwrap()
        .unwrap();
    assert_eq!(second.slice().page, PageNumber(1));
    drop(second);
    a.release_page(PageNumber(1), PAGE_BYTES as u32).unwrap();
    drop((a, slow, third, slow_first, third_first));
    for _ in 0..32 {
        pump(&mut endpoint);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn fetch_window_is_not_an_unreleased_credit_ceiling() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    // Full-page batch, then a one-byte boundary with byte credit just below
    // and exactly at the threshold for two additional full pages.
    for (start, bytes, batch) in [
        (0, 4 * PAGE_BYTES, 4),
        (PAGE_BYTES - 1, 2 * PAGE_BYTES, 2),
        (PAGE_BYTES - 1, 2 * PAGE_BYTES + 1, 3),
    ] {
        let signers = network(1);
        let mut f = fixture_with(5 * PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        let membership = local_membership(&signers[0]);
        let (local, mut endpoint, _publication) =
            coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
        let mut stream = match futures::executor::block_on(local.read(
            ClientRequest {
                retain: None,
                kind: ReadKind::Subscription {
                    pin: Some(f.page.version.etag.clone()),
                    range: Some(ByteRange::From(start)),
                    page_credits: 4,
                    byte_credits: bytes,
                },
                origin: OriginContext {
                    object: f.context.object.clone(),
                    metadata: None,
                    authorization: None,
                },
            },
            &scope,
        ))
        .unwrap()
        .payload
        {
            crate::read::ReadPayload::Stream(stream) => stream,
            crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
            pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
        };
        // Complete acquisitions without delivery: even four credits must not
        // create more than the coordinator's two acquisition/ready slots.
        for _ in 0..1024 {
            stream.poll_prefetch(&mut cx);
            pump(&mut endpoint);
        }
        assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
        assert_eq!(stream.buffered_pages(), 2);
        let mut slices = Vec::new();
        for number in 0..batch {
            let mut reader = None;
            for _ in 0..2048 {
                if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                    reader = result.unwrap();
                    break;
                }
                assert!(stream.buffered_pages() <= 2);
                pump(&mut endpoint);
            }
            let reader = reader.expect("client credits must permit the entire unreleased batch");
            assert_eq!(reader.slice().page, PageNumber(number));
            slices.push(reader.slice());
            drop(reader); // Delivery completion does not release subscription credit.
        }
        for _ in 0..32 {
            assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
            pump(&mut endpoint);
        }
        assert_eq!(f.origin.started_pages.borrow().len(), batch as usize);
        // Partial first-page delivery still pins the entire admitted plaintext.
        assert!(
            f.fill
                .dependencies
                .admission
                .used(crate::admission::ResourceClass::Plaintext)
                >= batch as usize * PAGE_BYTES as usize
        );
        let first = slices[0];
        assert_eq!(
            stream.release_page(first.page, first.length + 1),
            Err(Error::InvalidRequest)
        );
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        for slice in slices {
            stream.release_page(slice.page, slice.length).unwrap();
        }
        let mut next = None;
        for _ in 0..2048 {
            if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                next = result.unwrap();
                break;
            }
            assert!(stream.buffered_pages() <= 2);
            pump(&mut endpoint);
        }
        assert_eq!(next.unwrap().slice().page, PageNumber(batch));
        futures::executor::block_on(stream.cancel()).unwrap();
        drop(stream);
        for _ in 0..128 {
            pump(&mut endpoint);
        }
        endpoint.uninstall().unwrap();
    }
}

#[test]
fn zero_priority_read_beside_active_cache_progresses_while_first_page_is_held() {
    use crate::admission::ResourceClass;
    use flow_control::Policy;
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let signers = network(1);
    // Production sizes stream and admission windows from the same setting.
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.range_window_pages = std::num::NonZeroUsize::new(2).unwrap();
    limits.plaintext_bytes = std::num::NonZeroUsize::new(8 * PAGE_BYTES as usize).unwrap();
    let mut f = fixture_with(3 * PAGE_BYTES + 7, Some(limits));
    f.reactor.init().unwrap();
    let admission = f.fill.dependencies.admission.clone();
    let cold = f.page.version.object.cache.clone();
    let hot = racer_control_wire::CacheId("hot".into());
    let definition =
        |id: &racer_control_wire::CacheId, priority| racer_control_wire::CacheDefinition {
            id: id.clone(),
            name: id.0.clone(),
            client_socket: "/c".into(),
            origin_socket: "/o".into(),
            priority,
        };
    admission
        .policy()
        .weights()
        .update(&[definition(&hot, 1000), definition(&cold, 0)]);
    admission.reweigh();
    // An active positive-priority cache leaves the read only its request-progress allowance.
    let _hot = admission
        .reserve(Some(&hot), ResourceClass::Plaintext, 1)
        .unwrap();
    assert_eq!(
        admission.fair_limit(&cold, ResourceClass::Plaintext),
        admission.policy().progress(ResourceClass::Plaintext)
    );
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
    let mut stream = match futures::executor::block_on(local.read(
        ClientRequest {
            retain: None,
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::From(0)),
                page_credits: 4,
                byte_credits: 4 * PAGE_BYTES,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        },
        &scope,
    ))
    .unwrap()
    .payload
    {
        crate::read::ReadPayload::Stream(stream) => stream,
        crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
    };
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
        pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
    };
    let mut held = Vec::new();
    // Keep one delivered page held while the next one arrives; each page must succeed.
    for number in 0..4 {
        let mut reader = None;
        for _ in 0..2048 {
            if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                reader = Some(result);
                break;
            }
            pump(&mut endpoint);
        }
        let reader = reader
            .expect("page delivery stalled")
            .unwrap_or_else(|error| {
                panic!(
                    "page {number} failed: {error:?} plaintext={} ciphertext={} cfair={} pfair={}",
                    admission.used(ResourceClass::Plaintext),
                    admission.used(ResourceClass::Ciphertext),
                    admission.fair_limit(&cold, ResourceClass::Ciphertext),
                    admission.fair_limit(&cold, ResourceClass::Plaintext),
                )
            })
            .expect("page before end of range");
        assert_eq!(reader.slice().page, PageNumber(number));
        held.push(reader.slice());
        drop(reader);
        // Release all but the most recent page, keeping one page held while
        // the next window is fetched.
        if held.len() > 1 {
            let slice = held.remove(0);
            stream.release_page(slice.page, slice.length).unwrap();
        }
    }
    assert!(
        admission.used(ResourceClass::Plaintext)
            <= admission.policy().progress(ResourceClass::Plaintext) + 1
    );
    futures::executor::block_on(stream.cancel()).unwrap();
    drop(stream);
    for _ in 0..128 {
        pump(&mut endpoint);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn later_page_completes_before_head_and_cancellation_keeps_completion_fence() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for mode in ["success", "cancel", "drop", "failure"] {
        let signers = network(1);
        let mut f = fixture_with(2 * PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        let membership = local_membership(&signers[0]);
        let (local, mut endpoint, _publication) =
            coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
        let request = ClientRequest {
            retain: None,
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: None,
                page_credits: 64,
                byte_credits: 64 * PAGE_BYTES,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        let mut stream = match futures::executor::block_on(local.read(request, &scope))
            .unwrap()
            .payload
        {
            crate::read::ReadPayload::Stream(stream) => stream,
            crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            endpoint.poll(&mut cx, 64).unwrap();
            uring_runtime::drivers::poll(&mut cx, 64);
            uring_runtime::group::Service::poll_budgeted(
                &mut f.engine,
                &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
            f.crypto.poll_budgeted(64).unwrap();
            f.reactor.poll_budgeted(128).unwrap();
        };
        f.origin.blocked_pages.borrow_mut().insert(0);
        for _ in 0..1024 {
            assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
            pump(&mut endpoint);
            if f.fill
                .dependencies
                .memory
                .get(&PageId {
                    version: f.page.version.clone(),
                    number: PageNumber(1),
                })
                .unwrap()
                .is_some()
            {
                break;
            }
        }
        assert!(
            f.fill
                .dependencies
                .memory
                .get(&PageId {
                    version: f.page.version.clone(),
                    number: PageNumber(1)
                })
                .unwrap()
                .is_some(),
            "later page really completed"
        );
        assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
        assert_eq!(stream.buffered_pages(), 2);
        if mode == "cancel" || mode == "drop" {
            if mode == "cancel" {
                futures::executor::block_on(stream.cancel()).unwrap();
            }
            drop(stream);
            for _ in 0..32 {
                pump(&mut endpoint);
            }
            assert!(
                queue.pending() > 0,
                "canceled waiter is not actual origin completion"
            );
            f.origin.blocked_pages.borrow_mut().clear();
            for _ in 0..1024 {
                pump(&mut endpoint);
            }
            assert_eq!(queue.pending(), 0, "accepted head work drains its fence");
            assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
        } else if mode == "failure" {
            f.origin.version_unavailable.set(true);
            f.origin.blocked_pages.borrow_mut().clear();
            let mut failure = None;
            for _ in 0..1024 {
                if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                    failure = result.err();
                    break;
                }
                pump(&mut endpoint);
            }
            assert_eq!(failure, Some(Error::VersionUnavailable));
            assert_eq!(stream.buffered_pages(), 0);
            assert!(
                futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .is_none()
            );
            assert_eq!(
                &*f.origin.started_pages.borrow(),
                &[0, 1],
                "failure never retries or starts the tail"
            );
            drop(stream);
        } else {
            f.origin.blocked_pages.borrow_mut().clear();
            let mut first = None;
            for _ in 0..1024 {
                if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                    first = result.unwrap();
                    break;
                }
                pump(&mut endpoint);
            }
            assert_eq!(first.as_ref().unwrap().slice().page, PageNumber(0));
            let second = futures::executor::block_on(stream.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(second.slice().page, PageNumber(1));
            drop(first);
            stream
                .release_page(PageNumber(0), PAGE_BYTES as u32)
                .unwrap();
            let delivery = Delivery::new(f.reactor.clone(), Duration::from_secs(30));
            let (socket, _stalled_client) = std::os::unix::net::UnixStream::pair().unwrap();
            let mut connection =
                crate::http::from_accepted(socket.into(), &f.fill.dependencies.admission).unwrap();
            connection.set_framing(None, Some(PAGE_BYTES), false);
            let mut write = delivery.finish_progressing(second, connection, &scope);
            // No call to next_slice: page two is acquired while the current
            // delivery lease (page one) is held, including its pipe and credit.
            for _ in 0..1024 {
                stream.poll_prefetch(&mut cx);
                assert!(
                    write.as_mut().poll(&mut cx).is_pending(),
                    "client has not read any payload"
                );
                pump(&mut endpoint);
                if f.fill
                    .dependencies
                    .memory
                    .get(&PageId {
                        version: f.page.version.clone(),
                        number: PageNumber(2),
                    })
                    .unwrap()
                    .is_some()
                {
                    break;
                }
            }
            assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1, 2]);
            let third = futures::executor::block_on(stream.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(third.slice().page, PageNumber(2));
            assert_eq!(third.slice().length, 7);
            assert!(
                futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .is_none()
            );
            drop((stream, write, third));
        }
        for _ in 0..32 {
            pump(&mut endpoint);
        }
        endpoint.uninstall().unwrap();
    }
}

#[test]
fn production_range_windows_select_from_provider_share_node_work_and_deliver_in_order() {
    let stats = production_range_windows(false);
    assert!(
        stats
            .iter()
            .flat_map(|stats| stats.flights.borrow().clone())
            .any(|flights| flights == Some(crate::read::range_stream::REQUESTED_FLIGHTS))
    );
}
#[test]
fn production_range_windows_fall_back_to_single_flight_for_old_providers() {
    let stats = production_range_windows(true);
    assert!(!stats[0].flights.borrow().is_empty());
    for stats in &stats {
        let flights = stats.flights.borrow();
        if flights.is_empty() {
            continue;
        }
        // The first Subscribe asks for several flights and is dropped; every
        // later one omits the field, and at most one is outstanding at a time.
        assert_eq!(
            flights[0],
            Some(crate::read::range_stream::REQUESTED_FLIGHTS)
        );
        assert!(flights[1..].iter().all(Option::is_none), "{flights:?}");
        assert!(stats.max_outstanding.get() <= 1);
    }
}
/// Run three range windows on two requester nodes against provider node 2.
/// The object and windows stay small: node 2's outbound stack is a stub, so
/// larger windows that overload it and fall back to relayed fetches stall.
/// Asserts in-order delivery and shared node work; returns each requester's
/// Subscribe traffic.
fn production_range_windows(old_provider: bool) -> Vec<Rc<LinkStats>> {
    const PAGES: u64 = 4;
    const WINDOW: usize = 4;
    let stats: Vec<Rc<LinkStats>> = (0..2)
        .map(|_| {
            Rc::new(LinkStats {
                old_provider,
                ..Default::default()
            })
        })
        .collect();
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let signers = network(3);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    // A heavily weighted primary keeps this small object's pages on node 2 while
    // preserving production HRW, membership checks and origin authority.
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            (0..3)
                .map(|i| Member {
                    node: node(i),
                    shares: NonZeroU32::new(if i == 2 { 1_000_000 } else { 1 }).unwrap(),
                    peer_endpoint: if i == 2 {
                        address.to_string()
                    } else {
                        format!("127.0.0.1:{}", 8100 + i)
                    },
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let mut fixtures: Vec<_> = (0..3)
        .map(|_| fixture_with(PAGES * PAGE_BYTES, None))
        .collect();
    for fixture in &fixtures {
        fixture.reactor.init().unwrap();
    }
    let demands = Rc::new(RefCell::new(Vec::new()));
    let mut locals = Vec::new();
    let mut endpoints = Vec::new();
    let mut publications = Vec::new();
    for i in 0..3 {
        let peers: Rc<Requester> = if i == 2 {
            NoPeer::requester()
        } else {
            let admission = fixtures[i].fill.dependencies.admission.clone();
            let reactor = fixtures[i].reactor.clone();
            let codec = Rc::new(crate::peer::protocol::SecurityCodec::new(
                admission.clone(),
                BufferPool::new(admission.clone()),
            ));
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                PAGE_BYTES + 16,
            ));
            let transfers = Rc::new(crate::peer::transport::Transfers::new(
                Rc::new(crate::http::new_pool(reactor, admission.clone(), 4)),
                io,
                admission,
                codec,
                signers[i].clone(),
            ));
            let requester = Rc::new(crate::peer::Requester::new(
                Rc::new(Paths::new(Rc::new(LinkHealth::default()), 4)),
                Rc::new(Forwarding::new(signers[i].clone())),
                transfers,
                Rc::new(
                    PeerNetwork::new(
                        node(i),
                        crate::test_support::published_membership(membership.clone()),
                    )
                    .unwrap(),
                ),
            ));
            Requester::scripted(
                Rc::new(Link {
                    client: requester,
                    provider: node(2),
                    demands: demands.clone(),
                    stats: stats[i].clone(),
                }),
                Link::direct_hedge_available,
                Link::request,
                Link::request_direct,
            )
        };
        let (local, endpoint, published) =
            coordinator_with_window(&fixtures[i], &signers[i], &membership, peers, WINDOW);
        locals.push(local);
        endpoints.push(endpoint);
        publications.push(published);
    }
    let gate = Rc::new(Gate {
        local: locals[2].clone(),
        blocked: Cell::new(false),
        calls: RefCell::new(Vec::new()),
    });
    let admission = fixtures[2].fill.dependencies.admission.clone();
    let auth = Rc::new(Forwarding::new(signers[2].clone()));
    let network = Rc::new(PeerNetwork::new(node(2), publications[2].clone()).unwrap());
    let outbound = crate::peer::tests::NoOutbound::new(signers[2].clone(), network.clone());
    let relay = Rc::new(crate::peer::Relay::new(
        Rc::new(Paths::new(Rc::new(LinkHealth::default()), 4)),
        auth.clone(),
        outbound.requester.clone(),
        admission.clone(),
        network.clone(),
    ));
    let codec = Rc::new(crate::peer::protocol::SecurityCodec::new(
        admission.clone(),
        BufferPool::new(admission.clone()),
    ));
    let server = PeerServer::for_test(
        Rc::new(crate::http::new_io(
            fixtures[2].reactor.clone(),
            Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            PAGE_BYTES + 16,
        )),
        auth,
        admission,
        gate.clone(),
        relay,
        codec,
        signers[2].clone(),
    );
    let serving_scope = RequestScope::new(
        RequestId([99; 16]),
        Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    let mut serving = server.listen(address, &serving_scope);
    let open = |index: usize, start: u64| {
        let f = &fixtures[index];
        let request = ClientRequest {
            retain: None,
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::From(start * PAGE_BYTES)),
                page_credits: WINDOW,
                byte_credits: WINDOW as u64 * PAGE_BYTES,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        match futures::executor::block_on(locals[index].read(request, &f.scope))
            .unwrap()
            .payload
        {
            crate::read::ReadPayload::Stream(stream) => stream,
            crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
        }
    };
    let mut a = open(0, 0);
    let mut a2 = open(0, 0);
    let mut b = open(1, 2);
    let warm_request = ClientRequest {
        retain: None,
        kind: ReadKind::Subscription {
            pin: Some(fixtures[0].page.version.etag.clone()),
            range: None,
            page_credits: 1,
            byte_credits: PAGE_BYTES,
        },
        origin: OriginContext {
            object: fixtures[0].context.object.clone(),
            metadata: None,
            authorization: None,
        },
    };
    let warm_scope = fixtures[0].scope.clone();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pump = |endpoints: &mut Vec<crate::read::dispatch::WorkerEndpoint>| {
        assert!(
            serving
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        for endpoint in endpoints {
            endpoint
                .poll(
                    &mut Context::from_waker(futures::task::noop_waker_ref()),
                    64,
                )
                .unwrap();
        }
        uring_runtime::drivers::poll(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            64,
        );
        for f in &mut fixtures {
            uring_runtime::group::Service::poll_budgeted(
                &mut f.engine,
                &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
            f.crypto.poll_budgeted(64).unwrap();
            f.reactor.poll_budgeted(128).unwrap();
        }
    };
    // Every reader drives its own window; pages may arrive in any order but
    // each reader receives its range strictly in order.
    let mut delivered: [Vec<u64>; 3] = Default::default();
    let ends = [PAGES; 3];
    // Bounded by time, not iterations: under load, retries after a refused
    // or failed exchange can wait on real timers.
    let until = Instant::now() + Duration::from_secs(10);
    while Instant::now() < until {
        for (index, stream) in [&mut a, &mut a2, &mut b].into_iter().enumerate() {
            let page = match stream.next_slice().as_mut().poll(&mut cx) {
                Poll::Ready(result) => result.unwrap().map(|reader| reader.slice().page),
                Poll::Pending => None,
            };
            if let Some(page) = page {
                stream.release_page(page, PAGE_BYTES as u32).unwrap();
                delivered[index].push(page.0);
            }
        }
        if delivered
            .iter()
            .zip([0, 0, 2])
            .zip(ends)
            .all(|((pages, start), end)| pages.len() as u64 == end - start)
        {
            break;
        }
        pump(&mut endpoints);
    }
    assert_eq!(delivered[0], (0..PAGES).collect::<Vec<_>>());
    assert_eq!(delivered[1], (0..PAGES).collect::<Vec<_>>());
    assert_eq!(delivered[2], (2..PAGES).collect::<Vec<_>>());
    assert!(
        demands.borrow().iter().any(|count| *count > 1),
        "the window selects holes from the provider, not fixed pages"
    );
    // The provider fills each page from origin once; receiving nodes and the
    // second local reader share its transfers rather than the origin.
    assert_eq!(
        gate.calls
            .borrow()
            .iter()
            .collect::<std::collections::BTreeSet<_>>()
            .len() as u64,
        PAGES
    );
    for stream in [&mut a, &mut a2, &mut b] {
        assert!(
            futures::executor::block_on(stream.next_slice())
                .unwrap()
                .is_none()
        );
    }
    // A selection the windows outran still completes on its worker.
    drop((a, a2, b));
    for _ in 0..4096 {
        if endpoints.iter().all(|endpoint| endpoint.is_drained()) {
            break;
        }
        pump(&mut endpoints);
    }
    // A later local subscriber must reuse already verified pages without asking
    // the provider to transfer pages excluded by its completed-page ledger.
    let transfers = demands.borrow().len();
    let mut warm = match futures::executor::block_on(locals[0].read(warm_request, &warm_scope))
        .unwrap()
        .payload
    {
        crate::read::ReadPayload::Stream(stream) => stream,
        crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
    };
    let cached = {
        let mut work = warm.next_slice();
        let mut result = None;
        for _ in 0..2048 {
            if let Poll::Ready(value) = work.as_mut().poll(&mut cx) {
                result = Some(value.unwrap().unwrap());
                break;
            }
            pump(&mut endpoints);
        }
        result.expect("warm local selection")
    };
    assert_eq!(cached.slice().page.0, 0);
    assert_eq!(
        demands.borrow().len(),
        transfers,
        "warm read sent a peer request"
    );
    drop((cached, warm));
    serving_scope.cancel().unwrap();
    drop(serving);
    for f in &fixtures {
        let mut drain = f.reactor.drain();
        let until = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            assert!(Instant::now() < until, "reactor drain watchdog");
            f.reactor.poll_budgeted(128).unwrap();
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    // Readers on one node share page work: no page starts twice per node.
    for f in &fixtures {
        let started = f.origin.started_pages.borrow();
        let unique = started.iter().collect::<std::collections::BTreeSet<_>>();
        assert_eq!(unique.len(), started.len(), "{started:?}");
    }
    for endpoint in &mut endpoints {
        endpoint.uninstall().unwrap();
    }
    stats
}
#[test]
fn origin_fill_preserves_ciphertext_for_memory_and_pending_candidate_copy() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(budget.remaining_attempts(), 7);
    let copy = futures::executor::block_on(f.fill.copy_only(&f.page, &f.scope))
        .unwrap()
        .unwrap();
    assert_eq!(copy.1.bytes(), result.ciphertext.bytes());
    assert_eq!(copy.1.envelope().nonce, result.ciphertext.envelope().nonce);
    let pending = f
        .fill
        .dependencies
        .writer
        .copy_only(&f.page)
        .unwrap()
        .unwrap();
    assert_eq!(pending.ciphertext.bytes(), result.ciphertext.bytes());
    let second = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(second.ciphertext.bytes(), result.ciphertext.bytes());
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
    assert_eq!(f.fill.metrics.count(Event::MemoryHit), 2);
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 0);
    assert_eq!(f.fill.metrics.count(Event::PeerHit), 0);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 0);
    f.reactor.init().unwrap();
    let _ = futures::executor::block_on(f.fill.dependencies.writer.open()).unwrap();
    drive_disk(&f, f.fill.dependencies.writer.progress(1, &f.scope)).unwrap();
    drop((result, second, copy, pending));
    assert!(f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap() > 0);
    let disk_copy = drive_io(
        f.fill.copy_only(&f.page, &f.scope),
        &f.reactor,
        &mut f.engine,
        &f.crypto,
    )
    .unwrap()
    .unwrap();
    assert_eq!(disk_copy.0.version, f.page.version);
    let disk_result = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(disk_result.plaintext.bytes(), b"abc");
    // CopyOnly retains the disk ciphertext, so plaintext acquisition promotes
    // that same allocation instead of reading the disk again.
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 1);
    assert!(Arc::ptr_eq(
        &disk_copy.1.inner,
        &disk_result.ciphertext.inner
    ));
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
    assert_eq!(f.fill.metrics.count(Event::MemoryHit), 2);
}

/// Acquire `f.page` twice concurrently on the same worker.
fn acquire_pair(f: &mut Fixture) -> (Result<PageResult>, Result<PageResult>) {
    let mut first_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let mut second_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    drive(
        futures::future::join(
            f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut first_budget,
            ),
            f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut second_budget,
            ),
        ),
        &mut f.engine,
        &f.crypto,
    )
}

/// Fill one page from origin, then release every plaintext owner.
fn fill_and_release(f: &mut Fixture) -> CiphertextPage {
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = acquire(f, &mut budget).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    let ciphertext = result.ciphertext.clone();
    drop(result);
    f.fill.dependencies.flights.poll_budgeted(64).unwrap();
    ciphertext
}

#[test]
fn memory_cache_retains_only_ciphertext_after_delivery() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let ciphertext = fill_and_release(&mut f);
    let deps = &f.fill.dependencies;
    deps.admission.reclaim_buffers();
    assert_eq!(deps.admission.used(ResourceClass::Plaintext), 0);
    assert!(deps.memory.get(&f.page).unwrap().is_none());
    let cached = deps.memory.ciphertext(&f.page).unwrap().unwrap();
    assert!(Arc::ptr_eq(&cached.ciphertext.inner, &ciphertext.inner));
    assert!(deps.admission.used(ResourceClass::Ciphertext) > 0);
}

#[test]
fn cached_ciphertext_hit_decrypts_without_refetching() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let ciphertext = fill_and_release(&mut f);
    let decrypts = f.fill.metrics.count(Event::FillDecryptCount);
    let hits = f.fill.metrics.count(Event::MemoryHit);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let again = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(again.plaintext.bytes(), b"abc");
    // The decrypt used the cached allocation; no origin, disk, or peer work.
    assert!(Arc::ptr_eq(&again.ciphertext.inner, &ciphertext.inner));
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::FillDecryptCount), decrypts + 1);
    assert_eq!(f.fill.metrics.count(Event::MemoryHit), hits + 1);
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 0);
    assert_eq!(f.fill.metrics.count(Event::PeerHit), 0);
    // The decrypted plaintext is shared while held, then released.
    let shared = f.fill.dependencies.memory.get(&f.page).unwrap().unwrap();
    assert!(Arc::ptr_eq(&shared.plaintext.inner, &again.plaintext.inner));
    drop((again, shared));
    f.fill.dependencies.flights.poll_budgeted(64).unwrap();
    f.fill.dependencies.admission.reclaim_buffers();
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
}

#[test]
fn concurrent_cached_ciphertext_hits_coalesce_into_one_decrypt() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    fill_and_release(&mut f);
    let decrypts = f.fill.metrics.count(Event::FillDecryptCount);
    let (first, second) = acquire_pair(&mut f);
    let (first, second) = (first.unwrap(), second.unwrap());
    assert_eq!(first.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(&first.plaintext.inner, &second.plaintext.inner));
    assert_eq!(f.fill.metrics.count(Event::FillDecryptCount), decrypts + 1);
    assert_eq!(f.origin.calls.get(), 1);
}

#[test]
fn live_plaintext_is_reused_without_another_decrypt() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let held = acquire(&mut f, &mut budget).unwrap();
    f.fill.dependencies.flights.poll_budgeted(64).unwrap();
    let decrypts = f.fill.metrics.count(Event::FillDecryptCount);
    for _ in 0..3 {
        let again = acquire(&mut f, &mut budget).unwrap();
        assert!(Arc::ptr_eq(&again.plaintext.inner, &held.plaintext.inner));
    }
    assert_eq!(f.fill.metrics.count(Event::FillDecryptCount), decrypts);
    assert_eq!(f.origin.calls.get(), 1);
    // The weak handle never keeps the plaintext alive by itself.
    let plaintext = Arc::downgrade(&held.plaintext.inner);
    drop(held);
    f.fill.dependencies.flights.poll_budgeted(64).unwrap();
    assert!(plaintext.upgrade().is_none());
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
}

#[test]
fn peers_receive_cached_ciphertext_without_plaintext() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let ciphertext = fill_and_release(&mut f);
    let decrypts = f.fill.metrics.count(Event::FillDecryptCount);
    let (metadata, copy) = futures::executor::block_on(f.fill.copy_only(&f.page, &f.scope))
        .unwrap()
        .unwrap();
    assert_eq!(metadata.version, f.page.version);
    assert!(Arc::ptr_eq(&copy.inner, &ciphertext.inner));
    assert_eq!(copy.envelope().nonce, ciphertext.envelope().nonce);
    // Serving ciphertext neither decrypts nor materializes plaintext.
    assert_eq!(f.fill.metrics.count(Event::FillDecryptCount), decrypts);
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
}

/// A fixture whose memory cache can hold `pages` ciphertext pages.
fn roomy_fixture(pages: u64) -> Fixture {
    let mut limits = crate::test_support::cluster::config(false).limits;
    let bytes =
        std::num::NonZeroUsize::new(2 * (pages as usize + 1) * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = bytes;
    limits.plaintext_bytes = bytes;
    fixture_with(pages * PAGE_BYTES, Some(limits))
}

/// Fill `pages` of `f` from origin through `membership`, then release every
/// plaintext so the memory cache holds only their ciphertext.
fn cache_ciphertext_only(
    f: &mut Fixture,
    membership: &std::sync::Arc<crate::topology::Membership>,
    pages: &[u64],
) {
    for &number in pages {
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        drop(
            drive(
                f.fill
                    .acquire(page, membership.clone(), &f.context, &f.scope, &mut budget),
                &mut f.engine,
                &f.crypto,
            )
            .unwrap(),
        );
    }
    f.fill.dependencies.flights.poll_budgeted(64).unwrap();
    f.fill.dependencies.admission.reclaim_buffers();
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
    for &number in pages {
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        assert!(f.fill.dependencies.memory.get(&page).unwrap().is_none());
        assert!(
            f.fill
                .dependencies
                .memory
                .ciphertext(&page)
                .unwrap()
                .is_some(),
            "page {number} cached"
        );
    }
}

/// Read the whole object as one range stream and return the delivered pages.
fn read_range(
    f: &mut Fixture,
    local: &Rc<Coordinator>,
    endpoint: &mut crate::read::dispatch::WorkerEndpoint,
    pages: u64,
) -> Vec<u64> {
    let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
    let request = ClientRequest {
        retain: None,
        kind: ReadKind::Subscription {
            pin: Some(f.page.version.etag.clone()),
            range: None,
            page_credits: 64,
            byte_credits: 64 * PAGE_BYTES,
        },
        origin: OriginContext {
            object: f.context.object.clone(),
            metadata: None,
            authorization: None,
        },
    };
    let mut stream = match futures::executor::block_on(local.read(request, &scope))
        .unwrap()
        .payload
    {
        crate::read::ReadPayload::Stream(stream) => stream,
        crate::read::ReadPayload::Empty => panic!("expected a nonempty read stream"),
    };
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut delivered = Vec::new();
    for _ in 0..8192 {
        let polled = stream.next_slice().as_mut().poll(&mut cx);
        if let Poll::Ready(result) = polled {
            let Some(reader) = result.unwrap() else {
                break;
            };
            let slice = reader.slice();
            delivered.push(slice.page.0);
            drop(reader);
            stream.release_page(slice.page, slice.length).unwrap();
            continue;
        }
        endpoint.poll(&mut cx, 64).unwrap();
        uring_runtime::drivers::poll(&mut cx, 64);
        uring_runtime::group::Service::poll_budgeted(&mut f.engine, &mut cx, 64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
        f.reactor.poll_budgeted(128).unwrap();
    }
    assert_eq!(delivered.len() as u64, pages, "range stream completed");
    drop(stream);
    for _ in 0..32 {
        endpoint.poll(&mut cx, 64).unwrap();
        uring_runtime::drivers::poll(&mut cx, 64);
    }
    delivered
}

#[test]
fn warm_range_reread_decrypts_cached_ciphertext_without_any_transfer() {
    const PAGES: u64 = 6;
    const WINDOW: usize = 4;
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let signers = network(2);
    let mut f = roomy_fixture(PAGES);
    f.reactor.init().unwrap();
    // A heavily weighted remote node owns every page, so a missed probe
    // selects the range from it. Any peer request panics.
    let remote = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            (0..2)
                .map(|i| Member {
                    node: node(i),
                    shares: NonZeroU32::new(if i == 1 { 1_000_000 } else { 1 }).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8100 + i),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let all: Vec<u64> = (0..PAGES).collect();
    let prefill = f.membership.clone();
    cache_ciphertext_only(&mut f, &prefill, &all);
    let origin = f.origin.calls.get();
    let (local, mut endpoint, _publication) =
        coordinator_with_window(&f, &signers[0], &remote, NoPeer::requester(), WINDOW);
    assert_eq!(read_range(&mut f, &local, &mut endpoint, PAGES), all);
    assert_eq!(f.origin.calls.get(), origin, "no origin requests");
    assert_eq!(
        local.fill.metrics.count(Event::FillDecryptCount),
        PAGES,
        "each page is decrypted exactly once"
    );
    endpoint.uninstall().unwrap();
}

#[test]
fn warm_range_reread_transfers_only_uncached_pages() {
    const PAGES: u64 = 6;
    const WINDOW: usize = 4;
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let signers = network(1);
    let mut f = roomy_fixture(PAGES);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let prefill = f.membership.clone();
    cache_ciphertext_only(&mut f, &prefill, &[0, 1, 2, 4, 5]);
    f.origin.started_pages.borrow_mut().clear();
    let (local, mut endpoint, _publication) =
        coordinator_with_window(&f, &signers[0], &membership, NoPeer::requester(), WINDOW);
    assert_eq!(
        read_range(&mut f, &local, &mut endpoint, PAGES),
        (0..PAGES).collect::<Vec<_>>()
    );
    assert_eq!(&*f.origin.started_pages.borrow(), &[3]);
    assert_eq!(
        local.fill.metrics.count(Event::FillDecryptCount),
        5,
        "cached pages are decrypted once each"
    );
    endpoint.uninstall().unwrap();
}
