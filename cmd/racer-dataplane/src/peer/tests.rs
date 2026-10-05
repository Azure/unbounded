//! Shared signed peer fixtures and scenario suites.
use super::*;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::memory::BufferPool;
use crate::model::MetadataSelector;
use crate::model::Nonce;
use crate::model::*;
use crate::peer::forwarding::Forwarding;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation;
use crate::peer::protocol::PeerRequest;
use crate::peer::protocol::PeerResponse;
use crate::peer::protocol::SecurityCodec;
use crate::peer::protocol::Signatures;
use crate::peer::protocol::decode_envelope;
use crate::peer::protocol::encode_envelope;
use crate::runtime::RequestScope;
use crate::security::EncryptedAuthorization;
use crate::security::PeerOriginContext;
use crate::topology::RouteBudget;
use racer_control_wire::CacheId;
use racer_control_wire::ClusterId;
use racer_control_wire::KeyId;
use racer_identity::Certificates;
use racer_identity::Keyring;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::deadline::Deadline;
use uring_runtime::environment::SimulationClock;
#[cfg(test)]
use uring_runtime::group::Service;
use uring_runtime::reactor::IoBuffer;

mod body_progress {
    use super::*;
    use crate::http::Codec;
    use crate::http::Endpoint;
    use crate::http::HttpIo;
    use crate::http::HttpPool;
    use crate::model::ExpiresAt;
    use crate::model::ObjectMetadata;
    use crate::model::PageEnvelope;
    use crate::runtime::Reactor;
    use crate::telemetry::Telemetry;
    use std::cell::Cell;
    use std::net::TcpListener;
    use std::task::Context;
    use std::task::Poll;

    #[test]
    fn progressing_body_diagnostics_success_share_expiry_cancel_and_eof() {
        body_cases(&["success", "share", "cancel", "eof"]);
    }

    #[test]
    fn progressing_body_completes_past_share_stall_and_trickle_are_bounded() {
        body_cases(&["progress", "stall", "hard", "idle_cancel"]);
    }

    #[test]
    fn known_body_reserve_keeps_healthy_progress_but_bounds_slow_progress() {
        body_cases(&["reserved_progress", "reserved_slow"]);
    }

    #[test]
    fn local_total_cap_bounds_progress_and_pending_body_without_shortening_wire_authority() {
        body_cases(&["capped_progress", "capped_trickle", "capped_stall"]);
    }

    fn body_cases(cases: &[&str]) {
        for &case in cases {
            BodyFixture::new(case).run(case);
        }
    }

    struct BodyFixture {
        idle: bool,
        telemetry: Telemetry,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        io: Rc<HttpIo>,
        pool: Rc<HttpPool>,
        signers: Vec<Rc<Signatures>>,
        transfers: transport::Transfers,
        listener: TcpListener,
        start: Instant,
        original: Instant,
        share: Instant,
        signed_deadline: Instant,
        scope: RequestScope,
        server_scope: RequestScope,
        auth: Forwarding,
        signed: protocol::SignedRequest,
        binding: crate::peer::forwarding::RequestBinding,
    }

    impl BodyFixture {
        fn new(case: &str) -> Self {
            let idle = matches!(
                case,
                "progress"
                    | "stall"
                    | "hard"
                    | "idle_cancel"
                    | "reserved_progress"
                    | "reserved_slow"
                    | "capped_progress"
                    | "capped_trickle"
                    | "capped_stall"
            );
            let telemetry = Telemetry::default();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            admission
                .policy()
                .set_observer(telemetry.failures.observer(WorkerId(2)));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            let pool = Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 1));
            let signers = signers();
            let transfers = transport::Transfers::new(
                pool.clone(),
                io.clone(),
                None,
                admission.clone(),
                Rc::new(codec(&admission)),
                signers[0].clone(),
            );
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let start = Instant::now();
            let original = start
                + if case == "hard" {
                    Duration::from_millis(500)
                } else {
                    Duration::from_secs(3)
                };
            let share = start + Duration::from_millis(250);
            let mut local = request(&admission, 9);
            let signed_deadline = if idle { original } else { share };
            local.route.deadline = Deadline(signed_deadline);
            local.origin.scope.deadline = Deadline(signed_deadline);
            local.operation = Operation::Page {
                page: PageId {
                    version: ObjectVersion {
                        object: local.origin.object.clone(),
                        etag: StrongEtag::test_value("secret-etag"),
                    },
                    number: PageNumber(0),
                },
                mode: FetchMode::CopyOnly,
            };
            let mut scope = local.origin.scope().clone();
            scope.body_deadlines = Some((original, share));
            if idle {
                scope.set_candidate_idle(share - start).unwrap();
            }
            if matches!(case, "reserved_progress" | "reserved_slow") {
                scope
                    .set_candidate_body_budget(share - start, start + Duration::from_millis(700))
                    .unwrap();
            }
            if matches!(case, "capped_progress" | "capped_trickle" | "capped_stall") {
                scope
                    .set_candidate_total(start + Duration::from_millis(500))
                    .unwrap();
                // The capped stall starts near the cap, before its idle alarm expires.
                // Other cases retain their original idle/projection policy.
            }
            let auth = Forwarding::new(signers[0].clone());
            let (signed, binding) = auth.sign_request(local).unwrap();
            let server_scope = RequestScope::new(scope.request, original).unwrap();
            Self {
                idle,
                telemetry,
                admission,
                reactor,
                io,
                pool,
                signers,
                transfers,
                listener,
                start,
                original,
                share,
                signed_deadline,
                scope,
                server_scope,
                auth,
                signed,
                binding,
            }
        }

        fn run(self, case: &str) {
            let Self {
                idle,
                telemetry,
                admission,
                reactor,
                io,
                pool,
                signers,
                transfers,
                listener,
                start,
                original,
                share,
                signed_deadline,
                scope,
                server_scope,
                auth,
                signed,
                binding,
            } = self;
            let mut timing = super::super::PageTiming::new(&telemetry.metrics);
            let address = listener.local_addr().unwrap();
            let fixture_end = start + Duration::from_secs(5);
            let sent = Cell::new(0usize);
            let server = async {
                let fd = reactor
                    .accept(Rc::new(listener.into()), &server_scope)
                    .await?;
                let conn = crate::http::from_accepted(fd, &admission)?;
                let conn =
                    crate::peer::protocol::accept(&io, conn, signers[2].clone(), &server_scope)
                        .await?;
                let received = io.receive_head(conn, &server_scope).await?;
                let (head, _) = decode_envelope(received.value, false)?;
                let remote_auth = Forwarding::new(signers[2].clone());
                let req =
                    remote_auth.verify_request(codec(&admission).request(head, &server_scope)?)?;
                // Wire rounding is sub-millisecond; remote authority is the original
                // ceiling, established before signing, never a locally extended share.
                let remote_deadline = req.request().route.deadline.0;
                assert!(
                    signed_deadline.saturating_duration_since(remote_deadline)
                        < Duration::from_millis(1)
                );
                assert!(remote_deadline <= signed_deadline);
                assert!(
                    req.request()
                        .origin
                        .scope()
                        .deadline
                        .0
                        .saturating_duration_since(remote_deadline)
                        < Duration::from_millis(1)
                );
                let Operation::Page { page, .. } = &req.request().operation else {
                    panic!()
                };
                let length = 8192usize;
                let bytes = vec![0x9a; length + 16];
                let ciphertext = BufferPool::new(admission.clone()).ciphertext(
                    admission.reserve(
                        Some(&page.version.object.cache),
                        ResourceClass::Ciphertext,
                        bytes.len(),
                    )?,
                    PageEnvelope {
                        page: page.clone(),
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: bytes.len() as u32,
                    },
                    bytes,
                )?;
                let response = remote_auth.sign_response(
                    req.binding(),
                    PeerResponse::Page {
                        metadata: ObjectMetadata {
                            content_type: None,
                            version: page.version.clone(),
                            length: length as u64,
                            expires_at: ExpiresAt::test_time(
                                std::time::SystemTime::now() + Duration::from_secs(60),
                            ),
                        },
                        ciphertext: ciphertext.clone(),
                    },
                )?;
                let mut conn = io
                    .send_head(
                        received.connection,
                        encode_envelope(&response.authentication, true, length + 16)?,
                        &server_scope,
                    )
                    .await?
                    .connection;
                let mut next = Instant::now();
                while sent.get() < length + 16 {
                    if case != "success" {
                        futures::future::poll_fn(|_| {
                            if Instant::now() >= next {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        })
                        .await;
                    }
                    let offset = sent.get();
                    let end = (offset + 256).min(length + 16);
                    let done = io
                        .write_body_range(conn, ciphertext.clone(), offset..end, &server_scope)
                        .await?;
                    conn = done.lease;
                    sent.set(end);
                    if end == 4096 && matches!(case, "cancel" | "idle_cancel") {
                        scope.cancel()?;
                    }
                    if end == 4096 && case == "eof" {
                        return Ok::<_, Error>(());
                    }
                    if (end == 4096 && case == "stall") || (end == 8192 && case == "capped_stall") {
                        futures::future::pending::<()>().await;
                    }
                    next = Instant::now()
                        + Duration::from_millis(
                            if matches!(case, "hard" | "reserved_slow" | "capped_trickle") {
                                30
                            } else {
                                10
                            },
                        );
                }
                Ok::<_, Error>(())
            };
            let mut server: crate::error::Operation<'_, ()> = Box::pin(server);
            let mut client = transfers.exchange_timed(
                Endpoint::Peer(address.to_string()),
                signed,
                crate::rdma::TransportPlan::Http,
                None,
                None,
                None,
                None,
                Rc::new(Cell::new(false)),
                Some(&mut timing),
                &scope,
            );
            let result = loop {
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                if let Poll::Ready(result) = client.as_mut().poll(&mut cx) {
                    break result;
                }
                if let Poll::Ready(result) = server.as_mut().poll(&mut cx) {
                    assert!(result.is_ok(), "{case}: server {result:?}");
                    // Completed futures must not be polled again.
                    server = Box::pin(futures::future::pending());
                }
                reactor.poll_budgeted(128).unwrap();
                assert!(Instant::now() < fixture_end, "bounded body fixture");
                std::thread::sleep(Duration::from_micros(100));
            };
            drop(client);
            if matches!(
                case,
                "success" | "progress" | "reserved_progress" | "capped_progress"
            ) {
                let transport::RelayResponse::Complete(response) = result.unwrap() else {
                    panic!("requester must receive a complete HTTP body")
                };
                let response = auth.verify_response(response, &binding).unwrap();
                timing.success(&response);
                assert!(
                    matches!(response.response(), PeerResponse::Page { ciphertext, .. } if ciphertext.bytes().len() == 8208)
                );
                if matches!(case, "progress" | "reserved_progress" | "capped_progress") {
                    assert!(Instant::now() > share && Instant::now() < original);
                }
            } else {
                assert!(
                    matches!(result, Err(e) if e == match case { "share" | "stall" | "hard" | "reserved_slow" | "capped_trickle" | "capped_stall" => Error::DeadlineExceeded, "cancel" | "idle_cancel" => Error::Cancelled, _ => Error::Io }),
                    "{case}"
                );
            }
            drop(timing);
            drop(server);
            let mut text = String::new();
            telemetry.failures.write(&mut text).unwrap();
            if matches!(
                case,
                "success" | "progress" | "reserved_progress" | "capped_progress"
            ) {
                assert_eq!(text, "total=0 retained=0 capacity=128\n");
            } else {
                assert!(
                    text.starts_with("total=1 retained=1 capacity=128\n"),
                    "{text}"
                );
                assert!(text.contains("stage=PeerReceiveBody"));
                assert!(text.contains("attempt=09090909090909090909090909090909"));
                assert!(text.contains(&format!("remote={C}")));
                assert!(text.contains(&format!(">{address}")));
                let field = |name: &str| {
                    u64::from_str_radix(
                        text.split_whitespace()
                            .find_map(|s| s.strip_prefix(name))
                            .unwrap(),
                        if name == "n=" { 10 } else { 16 },
                    )
                    .unwrap()
                };
                let rx = text
                    .split("rx=")
                    .nth(1)
                    .unwrap()
                    .split('/')
                    .next()
                    .unwrap()
                    .parse::<usize>()
                    .unwrap();
                assert!(rx > 0 && rx < 8208, "{text}");
                assert!(field("n=") >= 3);
                assert!(field("f=") < field("l="));
                assert!(field("l=") <= field("now="));
                if case == "hard" {
                    assert!(field("now=") >= field("orig="));
                    assert!(field("now=") - field("l=") < 100);
                } else {
                    assert!(field("now=") < field("orig="));
                }
                assert_eq!(field(if idle { "orig=" } else { "share=" }), field("sig="));
                if case == "stall" {
                    assert!(field("now=") - field("l=") >= 250);
                }
                if matches!(case, "capped_trickle" | "capped_stall") {
                    assert!(Instant::now() >= start + Duration::from_millis(500));
                    assert!(Instant::now() < original);
                    // The stalled body's last chunk precedes expiry by less than
                    // the idle allowance, proving total-cap rather than idle expiry.
                    assert!(field("now=") - field("l=") < 250, "{text}");
                }
                if case == "share" {
                    assert!(field("now=") >= field("share="));
                    assert!(field("now=") - field("l=") < 50, "{text}");
                }
                for secret in [
                    "secret-etag",
                    "authorization",
                    "racer-signature",
                    "ciphertext",
                    "nonce",
                ] {
                    assert!(!text.contains(secret));
                }
            }
            drop(transfers);
            drop(pool);
            let mut drain = reactor.drain();
            while drain
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
            {
                reactor.poll_budgeted(128).unwrap();
                assert!(Instant::now() < fixture_end);
            }
            drop(drain);
            drop(io);
            drop(binding);
            admission.reclaim_buffers();
            assert_eq!(reactor.in_flight(), 0);
            drop(reactor);
            for class in [
                ResourceClass::Connection,
                ResourceClass::Ciphertext,
                ResourceClass::RequestContext,
                ResourceClass::ControlProgress,
            ] {
                assert_eq!(admission.used(class), 0, "{case} {class:?}");
            }
            let success = matches!(
                case,
                "success" | "progress" | "reserved_progress" | "capped_progress"
            );
            for (count, sum) in super::super::STAGES {
                assert_eq!(telemetry.metrics.count(count), u64::from(success), "{case}");
                if !success {
                    assert_eq!(telemetry.metrics.count(sum), 0, "{case}");
                }
            }
            assert_eq!(
                telemetry
                    .metrics
                    .count(crate::telemetry::Event::PeerPageCensored),
                u64::from(!success),
                "{case}"
            );
        }
    }
}
mod destination_disconnect {
    //! Real destination HTTP ingress with an independently owned, pending page flight.
    use super::*;
    use crate::http::Codec;
    use crate::http::HttpIo;
    use crate::read::flight::AcquisitionBudget;
    use crate::read::flight::AcquisitionEvent;
    use crate::read::flight::AcquisitionFailure;
    use crate::read::flight::Flights;
    use crate::read::flight::JoinedCopy;
    use crate::read::flight::JoinedFlight;
    use crate::runtime::Reactor;
    use crate::security::OriginContext;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use std::cell::Cell;
    use std::future::Future;
    use std::net::Shutdown;
    use std::net::TcpListener;
    use std::net::TcpStream;
    use std::task::Context;
    use std::task::Poll;

    struct PendingPage {
        flights: Rc<Flights>,
        page: PageId,
        entered: Cell<usize>,
    }
    impl server::LocalPageService for PendingPage {
        fn serve_peer<'a>(
            &'a self,
            request: crate::peer::forwarding::VerifiedRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            scope: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                assert_eq!(scope.deadline.0, request.request().route.deadline.0);
                let JoinedCopy::Waiter(mut waiter) = self.flights.join_copy(&self.page, scope)?
                else {
                    panic!("must join the pending shared acquisition")
                };
                self.entered.set(self.entered.get() + 1);
                let copy = waiter.wait().await?.copy();
                Ok(PeerResponse::Page {
                    metadata: copy.metadata,
                    ciphertext: copy.ciphertext,
                })
            })
        }
    }
    fn poll<F: Future + ?Sized>(work: std::pin::Pin<&mut F>) -> Poll<F::Output> {
        work.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    fn drive<T>(reactor: &Reactor, work: impl Future<Output = T>) -> T {
        let mut work = std::pin::pin!(work);
        let end = Instant::now() + Duration::from_secs(2);
        loop {
            if let Poll::Ready(result) = poll(work.as_mut()) {
                return result;
            }
            assert!(
                Instant::now() < end,
                "destination waiter did not detach promptly"
            );
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    #[test]
    fn destination_fin_detaches_only_its_waiter_from_pending_acquisition() {
        destination_exchange(CompletionOrder::FailureAfterFin);
    }

    #[test]
    fn destination_success_fences_watch_and_delivers_shared_page() {
        destination_exchange(CompletionOrder::Success);
    }

    #[test]
    fn destination_completion_fin_race_preserves_independent_callers() {
        for order in [
            CompletionOrder::FinThenComplete,
            CompletionOrder::CompleteThenFin,
            CompletionOrder::FinWhileCompletionFences,
            CompletionOrder::SuccessAfterFin,
        ] {
            destination_exchange(order);
        }
    }

    #[derive(Clone, Copy)]
    enum CompletionOrder {
        FailureAfterFin,
        Success,
        FinThenComplete,
        CompleteThenFin,
        FinWhileCompletionFences,
        SuccessAfterFin,
    }

    fn page_result(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        page: &PageId,
    ) -> crate::memory::PageResult {
        use crate::memory::CiphertextBytes;
        use crate::memory::CiphertextPage;
        use crate::memory::VerifiedBytes;
        use crate::memory::VerifiedPage;
        use crate::model::ExpiresAt;
        use crate::model::ObjectMetadata;
        use crate::model::PageEnvelope;
        crate::memory::PageResult {
            metadata: ObjectMetadata {
                content_type: None,
                version: page.version.clone(),
                length: 3,
                expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
            },
            plaintext: VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page: page.clone(),
                    bytes: vec![1, 2, 3],
                    reservation: admission
                        .reserve(None, ResourceClass::Plaintext, 3)
                        .unwrap(),
                }),
            },
            ciphertext: CiphertextPage {
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: std::sync::OnceLock::new(),
                    envelope: PageEnvelope {
                        page: page.clone(),
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([0; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    bytes: vec![7; 19],
                    reservation: admission
                        .reserve(None, ResourceClass::Ciphertext, 19)
                        .unwrap(),
                }),
            },
        }
    }

    fn destination_exchange(order: CompletionOrder) {
        DestinationFixture::new().run(order);
    }

    struct DestinationFixture {
        outbound: NoOutbound,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        io: Rc<HttpIo>,
        signers: Vec<Rc<Signatures>>,
        membership: std::sync::Arc<crate::topology::Membership>,
        page: PageId,
        flights: Rc<Flights>,
        service: Rc<PendingPage>,
        server: server::PeerServer,
        parent: RequestScope,
    }

    impl DestinationFixture {
        fn new() -> Self {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            reactor.init().unwrap();
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            let signers = signers();
            let forwarding = Rc::new(Forwarding::new(signers[2].clone()));
            let membership = Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [A, C]
                        .iter()
                        .enumerate()
                        .map(|(i, n)| Member {
                            node: NodeId((*n).into()),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                            rails: vec![],
                            site: String::new(),
                        })
                        .collect(),
                )
                .unwrap(),
            );
            let network = Rc::new(
                PeerNetwork::new(
                    NodeId(C.into()),
                    crate::control::PublishedState::for_membership(membership.clone()),
                )
                .unwrap(),
            );
            let page = PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(CACHE.into()),
                        key: CacheKey([3; 32]),
                    },
                    etag: StrongEtag::test_value("pending"),
                },
                number: PageNumber(0),
            };
            let flights = Rc::new(Flights::new(
                admission.clone(),
                crate::test_support::availability_for(vec![CacheId(CACHE.into())]),
            ));
            let service = Rc::new(PendingPage {
                flights: flights.clone(),
                page: page.clone(),
                entered: Cell::new(0),
            });
            let outbound = NoOutbound::new(signers[2].clone(), network.clone());
            let relay = Rc::new(Relay::new(
                Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
                forwarding.clone(),
                outbound.requester.clone(),
                admission.clone(),
                network.clone(),
            ));
            let server = server::PeerServer::for_test(
                io.clone(),
                forwarding,
                admission.clone(),
                service.clone(),
                relay,
                Rc::new(codec(&admission)),
                signers[2].clone(),
            );
            let parent =
                RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            Self {
                outbound,
                admission,
                reactor,
                io,
                signers,
                membership,
                page,
                flights,
                service,
                server,
                parent,
            }
        }

        fn run(self, order: CompletionOrder) {
            let Self {
                outbound: _outbound,
                admission,
                reactor,
                io,
                signers,
                membership,
                page,
                flights,
                service,
                server,
                parent,
            } = self;
            let independent = RequestScope::new(RequestId([8; 16]), parent.deadline.0).unwrap();
            let origin = OriginContext {
                object: page.version.object.clone(),
                metadata: None,
                authorization: None,
            };
            let mut budget = AcquisitionBudget::new(independent.deadline.0, 3, 8);
            let JoinedFlight::Waiter(mut supplier) = flights
                .join(page.clone(), membership, &origin, &independent, &mut budget)
                .unwrap()
            else {
                panic!("new flight")
            };
            let leader = match poll(supplier.wait().as_mut()) {
                Poll::Ready(Ok(AcquisitionEvent::Lead(leader))) => leader,
                _ => panic!("leader"),
            };
            let accepted = flights
                .retain_operation(
                    &leader,
                    crate::telemetry::Metrics::default()
                        .lease(crate::telemetry::Gauge::ActiveFills)
                        .unwrap(),
                )
                .unwrap();
            let JoinedCopy::Waiter(mut other) = flights.join_copy(&page, &independent).unwrap()
            else {
                panic!("independent waiter")
            };

            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let upstream = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (socket, _) = listener.accept().unwrap();
            let client =
                crate::http::from_accepted(upstream.try_clone().unwrap().into(), &admission)
                    .unwrap();
            let connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
            let mut work = server.serve_connection(connection, &parent);
            let sender = Forwarding::new(signers[0].clone());
            let mut request = request(&admission, 41);
            request.operation = Operation::Page {
                page: page.clone(),
                mode: FetchMode::CopyOnly,
            };
            let (signed, binding) = sender.sign_request(request).unwrap();
            let head = encode_envelope(&signed.authentication, false, 0).unwrap();
            let client = drive(&reactor, async {
                let sending = async {
                    let client = crate::peer::protocol::connect(
                        &io,
                        client,
                        signers[0].clone(),
                        signers[2].node(),
                        &parent,
                    )
                    .await?;
                    io.send_head(client, head, &parent).await
                };
                let mut sending = std::pin::pin!(sending);
                std::future::poll_fn(|cx| {
                    assert!(poll(work.as_mut()).is_pending());
                    sending.as_mut().poll(cx)
                })
                .await
            })
            .unwrap();
            drive(
                &reactor,
                std::future::poll_fn(|_| {
                    assert!(poll(work.as_mut()).is_pending());
                    if service.entered.get() == 1 {
                        Poll::Ready(())
                    } else {
                        Poll::Pending
                    }
                }),
            );
            assert_eq!(admission.used(ResourceClass::Waiter), 4);
            if !matches!(order, CompletionOrder::FailureAfterFin) {
                let result = page_result(&admission, &page);
                flights.publish(leader, result.clone()).unwrap();
                assert!(poll(other.wait().as_mut()).is_pending());
                assert!(poll(work.as_mut()).is_pending());
                assert_eq!(admission.used(ResourceClass::Flight), 1);

                match order {
                    CompletionOrder::Success => accepted.complete().unwrap(),
                    CompletionOrder::FinThenComplete => {
                        upstream.shutdown(Shutdown::Write).unwrap();
                        accepted.complete().unwrap();
                    }
                    CompletionOrder::CompleteThenFin => {
                        accepted.complete().unwrap();
                        upstream.shutdown(Shutdown::Write).unwrap();
                    }
                    CompletionOrder::FinWhileCompletionFences => {
                        accepted.complete().unwrap();
                        // Let dispatch consume the page before FIN, while the watch's
                        // cancellation completion still prevents returning the connection.
                        assert!(poll(work.as_mut()).is_pending());
                        assert_eq!(admission.used(ResourceClass::Waiter), 2);
                        assert!(reactor.in_flight() > 0);
                        upstream.shutdown(Shutdown::Write).unwrap();
                    }
                    CompletionOrder::SuccessAfterFin => {
                        upstream.shutdown(Shutdown::Write).unwrap();
                        assert!(matches!(
                            drive(&reactor, work.as_mut()),
                            Err(Error::Cancelled)
                        ));
                        assert_eq!(admission.used(ResourceClass::Waiter), 2);
                        assert_eq!(admission.used(ResourceClass::Flight), 1);
                        assert_eq!(reactor.in_flight(), 0);
                        assert!(poll(other.wait().as_mut()).is_pending());
                        accepted.complete().unwrap();
                    }
                    CompletionOrder::FailureAfterFin => unreachable!(),
                }
                if matches!(
                    order,
                    CompletionOrder::Success | CompletionOrder::FinWhileCompletionFences
                ) {
                    let returned = drive(&reactor, work.as_mut()).unwrap();
                    assert_eq!(reactor.in_flight(), 0, "success must fence the FIN watch");
                    let received =
                        drive(&reactor, io.receive_head(client.connection, &parent)).unwrap();
                    let (head, length) = decode_envelope(received.value, true).unwrap();
                    assert_eq!(length, result.ciphertext.bytes().len());
                    let mut connection = received.connection;
                    let mut bytes = Vec::new();
                    while bytes.len() < length {
                        let done = drive(
                            &reactor,
                            io.read_body(
                                connection,
                                io.buffer(length - bytes.len()).unwrap(),
                                &parent,
                            ),
                        )
                        .unwrap();
                        assert!(done.bytes > 0);
                        bytes.extend_from_slice(&done.buffer.bytes().unwrap()[..done.bytes]);
                        connection = done.lease;
                    }
                    assert_eq!(bytes, result.ciphertext.bytes());
                    let decoded = codec(&admission).response(head, bytes, &parent).unwrap();
                    let verified = sender.verify_response(decoded, &binding).unwrap();
                    let PeerResponse::Page {
                        metadata,
                        ciphertext,
                    } = verified.response()
                    else {
                        panic!("expected a successful page response")
                    };
                    assert_eq!(metadata.version, page.version);
                    assert_eq!(ciphertext.bytes(), result.ciphertext.bytes());
                    drop(connection);
                    drop(returned);
                } else {
                    if !matches!(order, CompletionOrder::SuccessAfterFin) {
                        assert!(matches!(
                            drive(&reactor, work.as_mut()),
                            Err(Error::Cancelled)
                        ));
                    }
                    drop(client);
                }
                drop(work);
                assert_eq!(parent.check(), Ok(()));
                assert_eq!(independent.check(), Ok(()));
                assert_eq!(reactor.in_flight(), 0);
                let shared = match poll(other.wait().as_mut()) {
                    Poll::Ready(Ok(shared)) => shared.copy(),
                    _ => panic!("independent copy waiter must receive successful publication"),
                };
                assert!(Arc::ptr_eq(
                    &shared.ciphertext.inner,
                    &result.ciphertext.inner
                ));
                let supplied = match poll(supplier.wait().as_mut()) {
                    Poll::Ready(Ok(AcquisitionEvent::Complete(page))) => page,
                    _ => panic!("independent supplier must receive successful publication"),
                };
                assert!(Arc::ptr_eq(
                    &supplied.plaintext.inner,
                    &result.plaintext.inner
                ));
                drop(shared);
                drop(supplied);
                drop(result);
                drop(other);
                drop(supplier);
                assert_eq!(admission.used(ResourceClass::Waiter), 0);
                assert_eq!(admission.used(ResourceClass::Flight), 0);
                assert_eq!(admission.used(ResourceClass::Connection), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                return;
            }
            upstream.shutdown(Shutdown::Write).unwrap();
            assert!(matches!(
                drive(&reactor, work.as_mut()),
                Err(Error::Cancelled)
            ));
            drop(work);
            assert_eq!(parent.check(), Ok(()));
            assert_eq!(independent.check(), Ok(()));
            assert_eq!(admission.used(ResourceClass::Waiter), 2);
            assert_eq!(admission.used(ResourceClass::Flight), 1);
            assert_eq!(reactor.in_flight(), 0, "FIN watch must be fenced");
            assert!(poll(other.wait().as_mut()).is_pending());
            // Accepted work remains owned until actual completion, not socket cancellation.
            flights
                .fail(leader, AcquisitionFailure::Terminal(Error::NotFound))
                .unwrap();
            assert!(poll(other.wait().as_mut()).is_pending());
            assert_eq!(admission.used(ResourceClass::Flight), 1);
            accepted.complete().unwrap();
            assert!(matches!(
                poll(other.wait().as_mut()),
                Poll::Ready(Err(Error::NotFound))
            ));
            drop(other);
            drop(supplier);
            drop(client);
            assert_eq!(admission.used(ResourceClass::Waiter), 0);
            assert_eq!(admission.used(ResourceClass::Flight), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
}
mod encrypted_http {
    //! Production page AEAD across independent keyrings and the signed HTTP transport.
    use super::*;
    use crate::http::Codec;
    use crate::http::Endpoint;
    use crate::memory::CiphertextPage;
    use crate::runtime::Reactor;
    use crate::security;
    use crate::security::CryptoClient;
    use crate::worker::CryptoRuntime;

    use crate::rdma::TransportPlan;
    use crate::security::PageCrypto;
    use crate::security::PageCryptoEngine;
    use crate::telemetry::Event;
    use crate::telemetry::Metrics;
    use racer_control_wire::CacheEncryptionKey;
    use racer_control_wire::CacheKeyPurpose;
    use racer_control_wire::CacheKeyRef;
    use racer_control_wire::CacheKeyState;
    use std::cell::Cell;
    use std::future::Future;
    use std::net::TcpListener;
    use std::task::Context;
    use std::task::Poll;
    use std::time::SystemTime;

    fn drive<T>(
        future: impl Future<Output = T>,
        reactor: &Reactor,
        engines: &mut [PageCryptoEngine],
        clients: &[Rc<CryptoClient>],
    ) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let until = Instant::now() + Duration::from_secs(20);
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(Instant::now() < until, "encrypted HTTP regression watchdog");
            for engine in engines.iter_mut() {
                engine.poll_budgeted(&mut cx, 8).unwrap();
            }
            for client in clients {
                client.poll_budgeted(8).unwrap();
            }
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    #[test]
    fn production_aad_http_roundtrip_cancel_reuse_and_mismatched_body() {
        // Each identity owns a different KeyEpochs store, populated from equal bundles.
        let identities = crate::test_support::security::identities(
            ClusterId(CLUSTER.into()),
            &[NodeId(A.into()), NodeId(C.into())],
            || {
                let mut keys = crate::test_support::security::mac_test_key(CACHE);
                keys.push(CacheEncryptionKey::new(
                    CacheKeyRef {
                        cache: CacheId(CACHE.into()),
                        id: crate::model::key_id_from_generation(1, 7).unwrap(),
                        purpose: CacheKeyPurpose::Page,
                    },
                    CacheKeyState::Active,
                    zeroize::Zeroizing::new([19; 32]),
                ));
                keys
            },
        );
        assert!(!Rc::ptr_eq(&identities[0].keys, &identities[1].keys));
        let admissions: Vec<_> = (0..2)
            .map(|_| {
                Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )))
            })
            .collect();
        let reactor = Rc::new(Reactor::new(admissions[0].clone()));
        let mut engines = Vec::new();
        let mut clients = Vec::new();
        let mut cryptos = Vec::new();
        let metrics = Metrics::default();
        for (i, identity) in identities.iter().enumerate() {
            let (io, port) = security::pair(
                WorkerId(i as u16),
                1,
                std::num::NonZeroUsize::new(4).unwrap(),
            );
            let client = Rc::new(CryptoClient::new(io));
            if i == 0 {
                client.set_metrics(metrics.clone());
            }
            cryptos.push(PageCrypto::new(identity.keys.clone(), client.clone()));
            clients.push(client);
            engines.push(PageCryptoEngine::new(CryptoRuntime { port }));
        }
        let ios: Vec<_> = admissions
            .iter()
            .map(|admission| {
                Rc::new(crate::http::new_io(
                    reactor.clone(),
                    Codec::new(protocol::MAX_ENVELOPE_HEAD),
                    admission.clone(),
                    PAGE_BYTES + 16,
                ))
            })
            .collect();
        let pool = Rc::new(crate::http::new_pool(
            reactor.clone(),
            admissions[0].clone(),
            1,
        ));
        let transfers = transport::Transfers::new(
            pool.clone(),
            ios[0].clone(),
            None,
            admissions[0].clone(),
            Rc::new(codec(&admissions[0])),
            identities[0].signatures.clone(),
        );
        let scope = RequestScope::new(RequestId([8; 16]), Instant::now() + Duration::from_secs(20))
            .unwrap();
        let length = 1 << 20; // Enter the production recycled-payload pool.
        let mut pages: Vec<CiphertextPage> = Vec::new();
        let mut expected = Vec::new();
        for i in 0..2u8 {
            let page = PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(CACHE.into()),
                        key: CacheKey([3 + i; 32]),
                    },
                    etag: StrongEtag::parse(if i == 0 {
                        b"\"a,\\literal\""
                    } else {
                        b"\"b,\\literal\""
                    })
                    .unwrap(),
                },
                number: PageNumber(0),
            };
            let bytes: Vec<_> = (0..length)
                .map(|offset| ((offset + usize::from(i) * 31) % 251) as u8)
                .collect();
            let cache = &page.version.object.cache;
            let mut plaintext = BufferPool::new(admissions[1].clone())
                .plaintext(
                    admissions[1]
                        .reserve(Some(cache), ResourceClass::Plaintext, length)
                        .unwrap(),
                    length,
                )
                .unwrap();
            plaintext.bytes_mut().unwrap().copy_from_slice(&bytes);
            let output = admissions[1]
                .reserve(Some(cache), ResourceClass::Ciphertext, length + 16)
                .unwrap();
            let (verified, ciphertext) = drive(
                cryptos[1].encrypt(page, plaintext, output, &scope),
                &reactor,
                &mut engines,
                &clients,
            )
            .unwrap();
            assert_eq!(verified.bytes(), bytes);
            drop(verified);
            pages.push(ciphertext);
            expected.push(bytes);
        }
        assert_ne!(pages[0].envelope().nonce, pages[1].envelope().nonce);
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let listener = Rc::new(uring_runtime::reactor::Descriptor::from(listener));
        let canceled = RequestScope::new(RequestId([9; 16]), scope.deadline.0).unwrap();
        let accepts = Cell::new(0);
        let prefix_sent = Cell::new(false);
        let server = async {
            let auth = Forwarding::new(identities[1].signatures.clone());
            let mut connection = None;
            for attempt in 0..5 {
                let conn = match connection.take() {
                    Some(conn) => conn,
                    None => {
                        accepts.set(accepts.get() + 1);
                        let fd = reactor.accept(listener.clone(), &scope).await?;
                        let conn = crate::http::from_accepted(fd, &admissions[1])?;
                        crate::peer::protocol::accept(
                            &ios[1],
                            conn,
                            identities[1].signatures.clone(),
                            &scope,
                        )
                        .await?
                    }
                };
                let received = ios[1].receive_head(conn, &scope).await?;
                let (head, _) = decode_envelope(received.value, false)?;
                let request = auth.verify_request(codec(&admissions[1]).request(head, &scope)?)?;
                let index = usize::from(attempt == 2 || attempt == 4);
                let ciphertext = &pages[index];
                let response = auth.sign_response(
                    request.binding(),
                    PeerResponse::Page {
                        metadata: ObjectMetadata {
                            content_type: None,
                            version: ciphertext.envelope().page.version.clone(),
                            length: length as u64,
                            expires_at: ExpiresAt::test_time(
                                SystemTime::now() + Duration::from_secs(60),
                            ),
                        },
                        ciphertext: ciphertext.clone(),
                    },
                )?;
                let mut conn = ios[1]
                    .send_head(
                        received.connection,
                        encode_envelope(&response.authentication, true, length + 16)?,
                        &scope,
                    )
                    .await?
                    .connection;
                // Negative control: preserve A's valid signed descriptor, send B's
                // independently valid equal-length body. Neither body is bit-flipped.
                let body = if attempt == 3 { &pages[1] } else { ciphertext };
                for offset in (0..length + 16).step_by(16381) {
                    let end = (offset + 16381).min(length + 16);
                    conn = ios[1]
                        .write_body_range(conn, body.clone(), offset..end, &scope)
                        .await?
                        .lease;
                    if attempt == 0 {
                        prefix_sent.set(true);
                        // Do not let cancellation race ahead of receive admission:
                        // the partial exchange must own a full ciphertext allocation.
                        futures::future::poll_fn(|_| {
                            if admissions[0].used(ResourceClass::Ciphertext) == length + 16 {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        })
                        .await;
                        canceled.cancel()?;
                        break;
                    }
                }
                if attempt != 0 {
                    conn.finish_exchange()?;
                    connection = Some(conn);
                } // A canceled partial exchange must close, never return to the pool.
            }
            Ok::<(), Error>(())
        };
        let client = async {
            let auth = Forwarding::new(identities[0].signatures.clone());
            let mut reused = None;
            for attempt in 0..5u8 {
                let index = usize::from(attempt == 2 || attempt == 4);
                let page = &pages[index];
                let mut request = request(&admissions[0], attempt);
                request.origin.authorization = None;
                request.origin.object = page.envelope().page.version.object.clone();
                request.operation = Operation::Page {
                    page: page.envelope().page.clone(),
                    mode: FetchMode::CopyOnly,
                };
                let turn = if attempt == 0 { &canceled } else { &scope };
                request.route.request = turn.request;
                request.route.deadline = turn.deadline;
                request.origin.request = turn.request;
                request.origin.scope = turn.clone();
                let (signed, binding) = auth.sign_request(request)?;
                let response = transfers
                    .exchange_timed(
                        endpoint.clone(),
                        signed,
                        TransportPlan::Http,
                        None,
                        None,
                        None,
                        None,
                        Rc::new(Cell::new(false)),
                        None,
                        turn,
                    )
                    .await;
                if attempt == 0 {
                    assert!(prefix_sent.get());
                    assert!(matches!(response, Err(Error::Cancelled)));
                    assert_eq!(metrics.count(Event::CryptoDecryptStarted), 0);
                    continue;
                }
                let transport::RelayResponse::Complete(response) = response? else {
                    panic!("HTTP requester must materialize ciphertext")
                };
                let response = auth.verify_response(response, &binding)?;
                let PeerResponse::Page { ciphertext, .. } = response.response() else {
                    panic!("page required")
                };
                assert_eq!(ciphertext.envelope(), page.envelope());
                assert_eq!(
                    ciphertext.bytes(),
                    pages[if attempt == 3 { 1 } else { index }].bytes()
                );
                let pointer = ciphertext.bytes().as_ptr();
                if let Some(previous) = reused {
                    assert_eq!(
                        pointer, previous,
                        "receive allocation must actually be reused"
                    );
                }
                reused = Some(pointer);
                // A receiver-computed CRC succeeds even for the wrong valid body.
                assert_eq!(ciphertext.verify_checksum(), Ok(()));
                let successes = metrics.count(Event::CryptoDecryptSuccess);
                let clear = cryptos[0]
                    .decrypt(
                        ciphertext.clone(),
                        admissions[0].reserve(
                            Some(&CacheId(CACHE.into())),
                            ResourceClass::Plaintext,
                            length,
                        )?,
                        turn,
                    )
                    .await;
                if attempt == 3 {
                    assert!(matches!(clear, Err(Error::CorruptRecord)));
                    assert_eq!(
                        metrics.count(Event::CryptoDecryptSuccess),
                        successes,
                        "mismatch cannot produce publishable plaintext"
                    );
                    assert_eq!(metrics.count(Event::CryptoDecryptAeadRejected), 1);
                    assert_eq!(metrics.count(Event::CryptoDecryptCrcRejected), 0);
                } else {
                    let clear = clear?;
                    assert_eq!(clear.page(), &page.envelope().page);
                    assert_eq!(clear.bytes(), expected[index]);
                    drop(clear);
                }
                drop(response);
            }
            Ok::<(), Error>(())
        };
        drive(
            async { futures::try_join!(server, client) },
            &reactor,
            &mut engines,
            &clients,
        )
        .unwrap();
        assert_eq!(
            accepts.get(),
            2,
            "cancel closes the first socket; later exchanges reuse one socket"
        );
        assert_eq!(metrics.count(Event::CryptoDecryptSuccess), 3);
        assert_eq!(metrics.count(Event::CryptoDecryptAeadRejected), 1);
        assert_eq!(metrics.count(Event::CryptoDecryptCrcRejected), 0);
        assert!(clients.iter().all(|client| client.outstanding() == 0));
        drive(reactor.drain(), &reactor, &mut engines, &clients).unwrap();
        assert_eq!(reactor.in_flight(), 0);
    }
}
mod opaque;
mod protocol_socket;
mod requester_safety {
    //! Real socket exchange with the adaptive controller attached to routing/requester.
    use crate::admission::AdmissionPolicy;
    use crate::admission::ResourceClass;
    use crate::http::Codec;
    use crate::peer::AdaptivePeers;
    use crate::peer::Outcome;
    use crate::peer::protocol as connection;
    use crate::peer::protocol::PeerResponse;
    use crate::peer::protocol::decode_envelope;
    use crate::peer::protocol::encode_envelope;
    use crate::peer::transport::RelayResponse;
    use crate::peer::*;
    use crate::runtime::Reactor;
    use crate::telemetry::Event;
    use crate::telemetry::Gauge;
    use crate::telemetry::Metrics;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use racer_control_wire::MembershipVersion;
    use std::sync::Arc;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;

    #[test]
    fn attached_requester_verification_and_opaque_outcome_gate_real_probe_recovery() {
        for opaque in [false, true] {
            for case in ["overloaded", "corrupt", "miss"] {
                probe_exchange(opaque, case);
            }
        }
    }

    #[test]
    fn direct_page_hedge_transport_pins_receiver_and_uses_shared_admission() {
        probe_exchange(false, "direct");
    }

    #[test]
    fn page_hedge_does_not_treat_multihop_destination_as_independent_first_hop() {
        let members = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..1500)
                    .map(|i| Member {
                        node: racer_control_wire::NodeId(format!(
                            "{i:08x}-1111-4111-8111-111111111111"
                        )),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let local = members.members()[0].node.clone();
        let network = Rc::new(
            crate::peer::PeerNetwork::new(
                local.clone(),
                crate::control::PublishedState::for_membership(members.clone()),
            )
            .unwrap(),
        );
        let nonneighbor = members
            .members()
            .iter()
            .find(|m| m.node != local && network.endpoint(&members, &m.node).is_err())
            .unwrap();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(crate::http::new_io(
            reactor.clone(),
            Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            0,
        ));
        let pool = Rc::new(crate::http::new_pool(reactor, admission.clone(), 2));
        let adaptive = AdaptivePeers::new(Default::default(), Metrics::default()).unwrap();
        let signers = crate::peer::tests::signers();
        let requester = Requester::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4).with_peer_admission(adaptive)),
            Rc::new(Forwarding::new(signers[0].clone())),
            Rc::new(Transfers::new(
                pool,
                io,
                None,
                admission.clone(),
                Rc::new(crate::peer::tests::codec(&admission)),
                signers[0].clone(),
            )),
            network,
        );
        assert!(!requester.direct_hedge_available(&members, &nonneighbor.node));
    }

    fn probe_exchange(opaque: bool, case: &str) {
        let signers = crate::peer::tests::signers();
        let fixture = super::SocketFixture::with_body_limit(2, 0);
        let receive_metrics = Metrics::default();
        let receive_gate = crate::peer::receive::Gate::with_metrics(
            crate::peer::receive::Config {
                active: 1,
                ..Default::default()
            },
            receive_metrics.clone(),
        )
        .unwrap();
        let transfers = Rc::new(
            Rc::try_unwrap(fixture.transfers(signers[0].clone()))
                .ok()
                .unwrap()
                .with_receive_gate(receive_gate),
        );
        let super::SocketFixture {
            admission,
            reactor,
            io,
            codec,
            ..
        } = fixture;
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap().to_string();
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [0, 2]
                    .iter()
                    .map(|i| Member {
                        node: signers[*i].node().clone(),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: address.clone(),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let metrics = Metrics::default();
        let adaptive = AdaptivePeers::new(
            crate::peer::Config {
                total: 4,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let node = signers[2].node().clone();
        if case != "direct" {
            let failed = adaptive.acquire(&node).unwrap();
            failed.observe(Outcome::PeerFailure);
            drop(failed);
            std::thread::sleep(Duration::from_millis(260));
        }
        let paths =
            Rc::new(Paths::new(Rc::new(LinkHealth), 4).with_peer_admission(adaptive.clone()));
        let forwarding = Rc::new(Forwarding::new(signers[0].clone()));
        let requester = Requester::new(
            paths,
            forwarding.clone(),
            transfers,
            Rc::new(
                crate::peer::PeerNetwork::new(
                    signers[0].node().clone(),
                    crate::control::PublishedState::for_membership(membership.clone()),
                )
                .unwrap(),
            ),
        );
        let mut request = crate::peer::tests::request(&admission, 91);
        let scope = RequestScope::new(
            request.route.request,
            Instant::now() + Duration::from_secs(3),
        )
        .unwrap();
        request.route.deadline = scope.deadline;
        request.origin.scope = scope.clone();
        request.operation = crate::peer::protocol::Operation::Bootstrap {
            object: request.origin.object.clone(),
            mode: crate::peer::protocol::FetchMode::CopyOnly,
        };
        if case == "direct" {
            let held = adaptive.acquire(&node).unwrap();
            assert!(!requester.direct_hedge_available(&membership, &node));
            drop(held);
            request.operation = crate::peer::protocol::Operation::Page {
                page: crate::model::PageId {
                    version: crate::model::ObjectVersion {
                        object: request.origin.object.clone(),
                        etag: crate::model::StrongEtag::test_value("hedge"),
                    },
                    number: crate::model::PageNumber(0),
                },
                mode: crate::peer::protocol::FetchMode::CopyOnly,
            };
            assert!(requester.direct_hedge_available(&membership, &node));
        }
        let (direct, signed) = if case == "direct" {
            (Some(request), None)
        } else {
            (None, Some(forwarding.sign_request(request).unwrap().0))
        };
        let server = async {
            let fd = reactor
                .accept(
                    Rc::new(uring_runtime::reactor::Descriptor::from(listener)),
                    &scope,
                )
                .await?;
            let conn = crate::http::from_accepted(fd, &admission)?;
            let conn = connection::accept(&io, conn, signers[2].clone(), &scope).await?;
            let received = io.receive_head(conn, &scope).await?;
            let (head, length) = decode_envelope(received.value, false)?;
            assert_eq!(length, 0);
            let auth = Forwarding::new(signers[2].clone());
            let request = auth.verify_request(codec.request(head, &scope)?)?;
            let outcome = if case == "overloaded" {
                PeerResponse::Overloaded
            } else {
                PeerResponse::Miss
            };
            let mut response = auth.sign_response(request.binding(), outcome)?;
            if case == "corrupt" {
                // The session authenticates the outer frame, but the original proof
                // no longer agrees with its signed application outcome.
                let original = Arc::get_mut(&mut response.authentication.original).unwrap();
                original
                    .head
                    .headers
                    .iter_mut()
                    .find(|h| h.name == "racer-outcome")
                    .unwrap()
                    .value = b"overloaded".to_vec();
            }
            let head = encode_envelope(&response.authentication, true, 0)?;
            let sent = io.send_head(received.connection, head, &scope).await?;
            drop(sent);
            Ok::<_, Error>(())
        };
        let client = async {
            if let Some(request) = direct {
                requester
                    .request_direct(request, membership, &scope)
                    .await
                    .map(|r| RelayResponse::Complete(r.into_signed()))
            } else if opaque {
                requester
                    .exchange_relay(
                        signed.unwrap(),
                        membership,
                        Rc::new(admission.reserve(None, ResourceClass::Relay, 1)?),
                        &scope,
                    )
                    .await
            } else {
                requester
                    .exchange(signed.unwrap(), membership, &scope)
                    .await
                    .map(RelayResponse::Complete)
            }
        };
        let mut work = Box::pin(async { futures::join!(client, server) });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let (result, served) = loop {
            assert!(
                Instant::now() < scope.deadline.0,
                "real peer safety watchdog"
            );
            if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                break result;
            }
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };
        served.unwrap();
        drop(work);
        if case == "corrupt" {
            assert!(result.is_err());
        } else {
            match result.unwrap() {
                RelayResponse::Http {
                    mut connection,
                    length,
                    ..
                } => {
                    assert_eq!(length, 0);
                    assert_eq!(
                        metrics.count(Event::PeerVerified),
                        0,
                        "headers cannot recover probe"
                    );
                    assert_eq!(metrics.gauge(Gauge::PeerExchanges), 1);
                    assert_eq!(
                        receive_metrics.gauge(Gauge::PeerReceiveActive),
                        1,
                        "opaque head is not body completion"
                    );
                    connection.finish_exchange().unwrap();
                    drop(connection);
                }
                RelayResponse::Complete(response) => assert_eq!(
                    matches!(response.response, PeerResponse::Overloaded),
                    case == "overloaded"
                ),
            }
        }
        assert_eq!(metrics.count(Event::PeerProbe), u64::from(case != "direct"));
        assert_eq!(
            metrics.count(Event::PeerVerified),
            u64::from(case == "miss" || case == "direct")
        );
        assert_eq!(
            adaptive.available(&node),
            case == "miss" || case == "direct"
        );
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
        assert_eq!(receive_metrics.gauge(Gauge::PeerReceiveActive), 0);
        assert_eq!(receive_metrics.count(Event::PeerReceiveAdmitted), 1);
    }
}
mod subscriptions {
    use super::*;
    use crate::http::Codec;
    use crate::memory::CiphertextCopy;
    use crate::model::ExpiresAt;
    use crate::model::ObjectMetadata;
    use crate::model::PageEnvelope;
    use crate::peer::subscriptions::Demand;
    use crate::peer::subscriptions::PageInterval;
    use crate::peer::subscriptions::Subscription;
    use crate::peer::subscriptions::TransferGrant;
    use crate::runtime::Reactor;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use racer_control_wire::BundleGeneration;
    use racer_control_wire::KeyringBundle;
    use racer_control_wire::SCHEMA_VERSION;
    use std::cell::Cell;
    use std::task::Context;
    use std::task::Poll;
    use std::time::UNIX_EPOCH;
    fn subscribe(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        sender: &str,
        id: u8,
        start: u64,
        mode: FetchMode,
    ) -> PeerRequest {
        let mut request = request(admission, id);
        request.origin.authorization = None;
        request.route.visited = vec![NodeId(sender.into())];
        request.route.remaining_attempts = if matches!(mode, FetchMode::Acquire) {
            3
        } else {
            0
        };
        request.operation = Operation::Subscribe {
            subscription: Subscription {
                id: [id; 16],
                sequence: 0,
                page_budget: 2,
                byte_budget: 2 * crate::model::PAGE_BYTES + 32,
                version: ObjectVersion {
                    object: request.origin.object.clone(),
                    etag: StrongEtag::test_value("v1"),
                },
                demand: Demand::new(vec![PageInterval { start, end: 20 }]).unwrap(),
            },
            mode,
        };
        request
    }
    fn copy(admission: &Rc<flow_control::Quotas<AdmissionPolicy>>, page: PageId) -> CiphertextCopy {
        let length = (page.number.0 + 1) * crate::model::PAGE_BYTES - crate::model::PAGE_BYTES + 3;
        let ciphertext = BufferPool::new(admission.clone())
            .ciphertext(
                admission
                    .reserve(
                        Some(&page.version.object.cache),
                        ResourceClass::Ciphertext,
                        19,
                    )
                    .unwrap(),
                PageEnvelope {
                    page: page.clone(),
                    key_id: KeyId([1; 16]),
                    nonce: Nonce([2; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                vec![3; 19],
            )
            .unwrap();
        CiphertextCopy {
            metadata: ObjectMetadata {
                version: page.version,
                length,
                content_type: None,
                expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
            },
            ciphertext,
        }
    }
    #[test]
    fn signed_subscription_selects_hot_page_fans_out_and_isolates_credential_failures() {
        struct Service {
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            ready: Cell<bool>,
            calls: Cell<usize>,
            reject: bool,
        }
        impl server::LocalPageService for Service {
            fn serve_peer<'a>(
                &'a self,
                request: crate::peer::forwarding::VerifiedRequest,
                _: std::sync::Arc<crate::topology::Membership>,
                _: &'a RequestScope,
            ) -> crate::error::Operation<'a, PeerResponse> {
                Box::pin(async move {
                    self.calls.set(self.calls.get() + 1);
                    std::future::poll_fn(|_| {
                        if self.ready.get() {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    })
                    .await;
                    let Operation::Page { page, mode } = &request.request().operation else {
                        panic!("must project demand")
                    };
                    assert!(matches!(mode, FetchMode::Acquire));
                    assert_eq!(request.request().route.remaining_attempts, 3);
                    assert_eq!(page.number.0, 8);
                    if self.reject && request.origin().node().0 == A {
                        return Ok(PeerResponse::OriginForbidden);
                    }
                    let copy = copy(&self.admission, page.clone());
                    Ok(PeerResponse::Page {
                        metadata: copy.metadata,
                        ciphertext: copy.ciphertext,
                    })
                })
            }
        }
        for reject in [false, true] {
            let signers = signers();
            let senders: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let membership = Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [A, B, C]
                        .iter()
                        .enumerate()
                        .map(|(i, n)| Member {
                            node: NodeId((*n).into()),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                            rails: vec![],
                            site: String::new(),
                        })
                        .collect(),
                )
                .unwrap(),
            );
            let network = Rc::new(
                PeerNetwork::new(
                    NodeId(C.into()),
                    crate::control::PublishedState::for_membership(membership),
                )
                .unwrap(),
            );
            let destination = Rc::new(Forwarding::new(signers[2].clone()));
            let outbound = NoOutbound::new(signers[2].clone(), network.clone());
            let relay = Rc::new(Relay::new(
                Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
                destination.clone(),
                outbound.requester.clone(),
                admission.clone(),
                network.clone(),
            ));
            let service = Rc::new(Service {
                admission: admission.clone(),
                ready: Cell::new(false),
                calls: Cell::new(0),
                reject,
            });
            let server = server::PeerServer::for_test(
                Rc::new(crate::http::new_io(
                    Rc::new(Reactor::new(admission.clone())),
                    Codec::new(protocol::MAX_ENVELOPE_HEAD),
                    admission.clone(),
                    crate::model::PAGE_BYTES + 16,
                )),
                destination,
                admission.clone(),
                service.clone(),
                relay,
                Rc::new(codec(&admission)),
                signers[2].clone(),
            );
            let codec = codec(&admission);
            let first = subscribe(&admission, A, 1, 8, FetchMode::Acquire);
            let first_scope = first.origin.scope().clone();
            let (first, first_binding) = senders[0].sign_request(first).unwrap();
            // Exercise the actual versioned wire and canonical logical decoding.
            let (first, _) = decode_envelope(
                encode_envelope(&first.authentication, false, 0).unwrap(),
                false,
            )
            .unwrap();
            let mut first =
                server.dispatch(codec.request(first, &first_scope).unwrap(), &first_scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(first.as_mut().poll(&mut cx).is_pending());
            assert!(first.as_mut().poll(&mut cx).is_pending());
            let second = subscribe(&admission, B, 2, 0, FetchMode::Acquire);
            let second_scope = second.origin.scope().clone();
            let (second, second_binding) = senders[1].sign_request(second).unwrap();
            let mut second = server.dispatch(second, &second_scope);
            // Both compact endpoints may need independent cooperative eligibility
            // checks before the follower joins the held leader.
            for _ in 0..4 {
                assert!(second.as_mut().poll(&mut cx).is_pending());
            }
            assert_eq!(service.calls.get(), 1);
            service.ready.set(true);
            let Poll::Ready(Ok(first)) = first.as_mut().poll(&mut cx) else {
                panic!("leader completes")
            };
            let Poll::Ready(Ok(second)) = second.as_mut().poll(&mut cx) else {
                panic!("follower completes")
            };
            assert_eq!(service.calls.get(), if reject { 2 } else { 1 });
            let first = senders[0].verify_response(first, &first_binding).unwrap();
            if reject {
                assert!(matches!(first.response(), PeerResponse::OriginForbidden));
            }
            let PeerResponse::Selected {
                ciphertext, grant, ..
            } = &second.response
            else {
                panic!("selected result")
            };
            assert_eq!(grant.page.number.0, 8);
            assert_eq!(grant.receiver.0, B);
            let body = ciphertext.bytes().to_vec();
            let (encoded, length) = decode_envelope(
                encode_envelope(&second.authentication, true, body.len()).unwrap(),
                true,
            )
            .unwrap();
            assert_eq!(length, 19);
            let decoded = codec.response(encoded, body, &second_scope).unwrap();
            let verified = senders[1]
                .verify_response(decoded, &second_binding)
                .unwrap();
            assert!(matches!(verified.response(), PeerResponse::Selected { .. }));
            let PeerResponse::Selected { ciphertext, .. } = verified.response() else {
                unreachable!()
            };
            let provenance = ciphertext.provenance.unwrap();
            assert_eq!(&provenance.supplier, signers[2].node().0.as_bytes());
            assert_eq!(provenance.remote, provenance.supplier);
            let retained = ciphertext.clone();
            drop(verified);
            assert_eq!(retained.provenance, Some(provenance));
        }
    }
    #[test]
    fn signed_ingress_cold_selection_is_bounded_cancellable_and_does_not_block_other_workers() {
        use crate::peer::subscriptions::Selection;
        use crate::topology::scored_members;
        struct Never;
        impl server::LocalPageService for Never {
            fn serve_peer<'a>(
                &'a self,
                _: crate::peer::forwarding::VerifiedRequest,
                _: std::sync::Arc<crate::topology::Membership>,
                _: &'a RequestScope,
            ) -> crate::error::Operation<'a, PeerResponse> {
                Box::pin(async { panic!("all demand endpoints must be ineligible") })
            }
        }
        let signers = signers();
        let sender = Forwarding::new(signers[0].clone());
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        // Full protocol membership; the local provider has negligible weight. The
        // signed unavailable response below proves all 64 endpoints were ineligible.
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..100_000)
                    .map(|i| Member {
                        node: NodeId(match i {
                            0 => A.into(),
                            1 => C.into(),
                            _ => format!("member-{i:06}"),
                        }),
                        shares: std::num::NonZeroU32::new(if i == 1 { 1 } else { u32::MAX })
                            .unwrap(),
                        peer_endpoint: "127.0.0.1:8000".into(),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let destination = Rc::new(Forwarding::new(signers[2].clone()));
        let network = Rc::new(
            PeerNetwork::new(
                NodeId(C.into()),
                crate::control::PublishedState::for_membership(membership),
            )
            .unwrap(),
        );
        let outbound = NoOutbound::new(signers[2].clone(), network.clone());
        let relay = Rc::new(Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
            destination.clone(),
            outbound.requester.clone(),
            admission.clone(),
            network,
        ));
        let server = server::PeerServer::for_test(
            Rc::new(crate::http::new_io(
                Rc::new(Reactor::new(admission.clone())),
                Codec::new(protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            )),
            destination,
            admission.clone(),
            Rc::new(Never),
            relay,
            Rc::new(codec(&admission)),
            signers[2].clone(),
        );
        let mut request = subscribe(&admission, A, 70, 0, FetchMode::Acquire);
        let scope = RequestScope::new(
            RequestId([70; 16]),
            Instant::now() + Duration::from_secs(240),
        )
        .unwrap();
        request.origin.scope = scope.clone();
        request.origin.request = scope.request;
        request.route.request = scope.request;
        request.route.deadline = scope.deadline;
        let Operation::Subscribe { subscription, .. } = &mut request.operation else {
            panic!()
        };
        subscription.demand = Demand::new(
            (0..64)
                .map(|i| PageInterval {
                    start: i * 2,
                    end: i * 2 + 1,
                })
                .collect(),
        )
        .unwrap();
        let contract = subscription.clone();
        let (signed, _) = sender.sign_request(request).unwrap();
        let (wire, _) = decode_envelope(
            encode_envelope(&signed.authentication, false, 0).unwrap(),
            false,
        )
        .unwrap();
        let mut work = server.dispatch(codec(&admission).request(wire, &scope).unwrap(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert_eq!(scored_members() - before, 256);
        // Another OS worker must acquire the same node-wide scheduler while ingress
        // ranking is suspended. A bounded channel receive diagnoses lock retention.
        let scheduler = server.subscription_owner().clone();
        let mut other = contract.clone();
        other.id = [71; 16];
        let (send, receive) = std::sync::mpsc::channel();
        let worker = std::thread::spawn(move || {
            let now = crate::peer::protocol::millis(std::time::SystemTime::now()).unwrap();
            let Selection::Leader { work, mut waiter } = scheduler
                .schedule(
                    other,
                    MembershipVersion(1),
                    NodeId(B.into()),
                    now + 30_000,
                    now,
                )
                .unwrap()
            else {
                panic!("independent flight")
            };
            work.fail(Error::Unavailable);
            assert!(matches!(waiter.try_result(now), Err(Error::Unavailable)));
            send.send(()).unwrap();
        });
        receive.recv_timeout(Duration::from_secs(2)).unwrap();
        worker.join().unwrap();
        scope.cancel().unwrap();
        let before = scored_members();
        assert!(matches!(
            work.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(
            scored_members(),
            before,
            "cancellation must hash no more members"
        );
        drop(work);
        assert_eq!(admission.used(ResourceClass::Waiter), 0);
        // Resume the accepted contract with a new signed sequence and the original
        // deadline, then traverse every ineligible interval. Every poll is measured,
        // including rank completion and transition to the next endpoint.
        let mut request = subscribe(&admission, A, 72, 0, FetchMode::Acquire);
        let resume_scope = RequestScope::new(scope.request, scope.deadline.0).unwrap();
        request.origin.scope = resume_scope.clone();
        request.origin.request = resume_scope.request;
        request.route.request = resume_scope.request;
        request.route.deadline = resume_scope.deadline;
        let mut resume = contract.clone();
        resume.sequence = 1;
        request.operation = Operation::Subscribe {
            subscription: resume,
            mode: FetchMode::Acquire,
        };
        let (signed, binding) = sender.sign_request(request).unwrap();
        let (wire, _) = decode_envelope(
            encode_envelope(&signed.authentication, false, 0).unwrap(),
            false,
        )
        .unwrap();
        let mut work = server.dispatch(
            codec(&admission).request(wire, &resume_scope).unwrap(),
            &resume_scope,
        );
        let start = scored_members();
        let mut polls = 0;
        let response = loop {
            let before = scored_members();
            let result = work.as_mut().poll(&mut cx);
            assert!(
                scored_members() - before <= 256,
                "poll {polls} exceeded rank quantum"
            );
            polls += 1;
            assert!(polls < 30_000, "bounded interval set must finish");
            if let Poll::Ready(result) = result {
                break result.unwrap();
            }
        };
        assert!(
            polls > 20_000,
            "must actually traverse large cold membership"
        );
        assert_eq!(
            scored_members() - start,
            64 * 100_000 - 256,
            "canceled partial ranking is reused"
        );
        assert!(matches!(
            sender
                .verify_response(response, &binding)
                .unwrap()
                .response(),
            PeerResponse::Unavailable
        ));
        // Persistent cache avoids another full membership traversal for later updates.
        drop(work);
        let mut request = subscribe(&admission, A, 73, 0, FetchMode::Acquire);
        request.origin.scope = resume_scope.clone();
        request.origin.request = resume_scope.request;
        request.route.request = resume_scope.request;
        request.route.deadline = resume_scope.deadline;
        let mut resume = contract;
        resume.sequence = 2;
        request.operation = Operation::Subscribe {
            subscription: resume,
            mode: FetchMode::Acquire,
        };
        let (signed, binding) = sender.sign_request(request).unwrap();
        let mut work = server.dispatch(signed, &resume_scope);
        let before = scored_members();
        for _ in 0..64 {
            assert!(
                work.as_mut().poll(&mut cx).is_pending(),
                "even hot endpoints yield"
            );
        }
        let Poll::Ready(Ok(response)) = work.as_mut().poll(&mut cx) else {
            panic!("hot sweep finishes")
        };
        assert_eq!(scored_members(), before);
        assert!(matches!(
            sender
                .verify_response(response, &binding)
                .unwrap()
                .response(),
            PeerResponse::Unavailable
        ));
    }
    #[test]
    fn subscription_selection_is_canonical_signed_and_bound_to_exact_grant() {
        let signers = signers();
        let sender = Forwarding::new(signers[0].clone());
        let destination = Forwarding::new(signers[2].clone());
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let local = subscribe(&admission, A, 1, 0, FetchMode::CopyOnly);
        let (signed, binding) = sender.sign_request(local).unwrap();
        let admitted = destination.verify_request(signed).unwrap();
        let Operation::Subscribe { subscription, .. } = &admitted.request().operation else {
            panic!()
        };
        let page = PageId {
            version: subscription.version.clone(),
            number: PageNumber(0),
        };
        let good = TransferGrant {
            subscription_id: subscription.id,
            sequence: 0,
            page: page.clone(),
            membership: MembershipVersion(1),
            receiver: NodeId(A.into()),
            deadline: crate::peer::protocol::encode_deadline(admitted.request().route.deadline)
                .unwrap(),
            remaining_page_budget: 1,
            remaining_byte_budget: subscription.byte_budget - 19,
        };
        for case in 0..10 {
            let mut grant = good.clone();
            match case {
                1 => grant.subscription_id[0] ^= 1,
                2 => grant.sequence += 1,
                3 => grant.receiver = NodeId(B.into()),
                4 => grant.membership.0 += 1,
                5 => grant.deadline += 1,
                6 => grant.remaining_page_budget += 1,
                7 => grant.remaining_byte_budget += 1,
                8 => grant.page.number.0 = 20,
                9 => grant.page.version.etag = StrongEtag::test_value("changed"),
                _ => (),
            }
            let copy = copy(&admission, grant.page.clone());
            let response = PeerResponse::Selected {
                metadata: copy.metadata,
                ciphertext: copy.ciphertext,
                grant,
            };
            // Sign directly to test receiver validation independently of sign_response.
            let mut head = crate::peer::protocol::response_head(
                &response,
                &crate::peer::protocol::signed_digest(&admitted.signed().authentication.original)
                    .unwrap(),
                &[NodeId(A.into()), NodeId(C.into())],
            )
            .unwrap();
            crate::peer::protocol::push(&mut head, "racer-receiver", A);
            let signed = protocol::SignedResponse {
                authentication: crate::peer::forwarding::ForwardedHead {
                    original: Arc::new(signers[2].sign(head).unwrap()),
                    hops: vec![],
                },
                response,
            };
            assert_eq!(
                sender.verify_response(signed, &binding).is_ok(),
                case == 0,
                "case {case}"
            );
        }
        let mut bad = subscribe(&admission, A, 3, 0, FetchMode::CopyOnly);
        bad.origin.authorization = Some(EncryptedAuthorization {
            key_id: KeyId([1; 16]),
            nonce: Nonce([1; 24]),
            ciphertext: vec![0; 16],
        });
        assert!(sender.sign_request(bad).is_err());
        let mut signed = sender
            .sign_request(subscribe(&admission, A, 4, 0, FetchMode::CopyOnly))
            .unwrap()
            .0;
        let Operation::Subscribe { subscription, .. } = &mut signed.request.operation else {
            panic!()
        };
        subscription.sequence += 1;
        assert!(
            destination.verify_request(signed).is_err(),
            "logical sequence must not be elided as a session auth field"
        );
    }
    #[test]
    fn subscription_runs_through_real_tcp_requester_session_and_provider() {
        struct Local(Rc<flow_control::Quotas<AdmissionPolicy>>);
        impl server::LocalPageService for Local {
            fn serve_peer<'a>(
                &'a self,
                request: crate::peer::forwarding::VerifiedRequest,
                _: std::sync::Arc<crate::topology::Membership>,
                _: &'a RequestScope,
            ) -> crate::error::Operation<'a, PeerResponse> {
                Box::pin(async move {
                    let Operation::Page {
                        page,
                        mode: FetchMode::CopyOnly,
                    } = &request.request().operation
                    else {
                        panic!("copy-only projection required")
                    };
                    assert!(request.request().origin.authorization.is_none());
                    assert_eq!(request.request().route.remaining_attempts, 0);
                    let copy = copy(&self.0, page.clone());
                    Ok(PeerResponse::Page {
                        metadata: copy.metadata,
                        ciphertext: copy.ciphertext,
                    })
                })
            }
        }
        let signers = signers();
        let fixture = SocketFixture::new(2);
        let transfers = fixture.transfers(signers[0].clone());
        let SocketFixture {
            admission,
            reactor,
            io,
            codec,
            ..
        } = fixture;
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap();
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [A, C]
                    .iter()
                    .enumerate()
                    .map(|(i, n)| Member {
                        node: NodeId((*n).into()),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: if i == 1 {
                            address.to_string()
                        } else {
                            "127.0.0.1:9000".into()
                        },
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let network = |node: &str| {
            Rc::new(
                PeerNetwork::new(
                    NodeId(node.into()),
                    crate::control::PublishedState::for_membership(membership.clone()),
                )
                .unwrap(),
            )
        };
        let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
        let destination = Rc::new(Forwarding::new(signers[2].clone()));
        let local = Rc::new(Local(admission.clone()));
        let outbound = NoOutbound::new(signers[2].clone(), network(C));
        let relay = Rc::new(Relay::new(
            paths.clone(),
            destination.clone(),
            outbound.requester.clone(),
            admission.clone(),
            network(C),
        ));
        let server = server::PeerServer::for_test(
            io.clone(),
            destination,
            admission.clone(),
            local,
            relay,
            codec.clone(),
            signers[2].clone(),
        );
        let requester = Requester::new(
            paths,
            Rc::new(Forwarding::new(signers[0].clone())),
            transfers,
            network(A),
        );
        let request = subscribe(&admission, A, 1, 8, FetchMode::CopyOnly);
        let scope = request.origin.scope().clone();
        let server_work = async {
            let fd = reactor
                .accept(
                    Rc::new(uring_runtime::reactor::Descriptor::from(listener)),
                    &scope,
                )
                .await?;
            server
                .serve_connection(crate::http::from_accepted(fd, &admission)?, &scope)
                .await?;
            Ok::<_, Error>(())
        };
        let exchange = async {
            futures::try_join!(requester.request(request, membership, &scope), server_work)
        };
        let mut exchange = std::pin::pin!(exchange);
        let watchdog = Instant::now() + Duration::from_secs(10);
        let (response, ()) = loop {
            if let Poll::Ready(result) = exchange
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            {
                break result.unwrap();
            }
            assert!(Instant::now() < watchdog);
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };
        let PeerResponse::Selected {
            grant, ciphertext, ..
        } = response.response()
        else {
            panic!("selected response")
        };
        assert_eq!(grant.page.number.0, 8);
        assert_eq!(ciphertext.bytes(), &[3; 19]);
        assert_eq!(grant.remaining_page_budget, 1);
    }
    #[test]
    fn retained_subscription_cannot_complete_after_request_mac_key_retirement() {
        let (signers, discovery) = identities();
        let sender = Forwarding::new(signers[0].clone());
        let destination = Forwarding::new(signers[2].clone());
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let (request, _) = sender
            .sign_request(subscribe(&admission, A, 1, 0, FetchMode::CopyOnly))
            .unwrap();
        let admitted = destination.verify_request(request).unwrap();
        let keys = &discovery[2].0;
        let mut replacement = crate::test_support::security::mac_test_key(CACHE);
        for key in &mut replacement {
            key.key.id.0[4..12].copy_from_slice(&2u64.to_be_bytes());
            let (reference, state, mut material) = key.clone().into_installation();
            material[0] ^= 1;
            *key = racer_control_wire::CacheEncryptionKey::new(reference, state, material);
        }
        keys.install(KeyringBundle {
            schema_version: SCHEMA_VERSION,
            cluster: ClusterId(CLUSTER.into()),
            generation: BundleGeneration(2),
            peer_trust_roots: (*keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: replacement,
        })
        .unwrap();
        assert!(
            destination
                .sign_response(admitted.binding(), PeerResponse::Miss)
                .is_err()
        );
    }
}
mod timing {
    use super::*;
    use crate::peer::PageTiming;
    use crate::peer::STAGES;
    use crate::rdma::TransportPlan;
    use crate::telemetry::Event;
    use crate::telemetry::Metrics;
    use racer_control_wire::RailId;

    fn page_request(admission: &flow_control::Quotas<AdmissionPolicy>, attempt: u8) -> PeerRequest {
        let mut local = request(admission, attempt);
        local.operation = Operation::Page {
            page: PageId {
                version: ObjectVersion {
                    object: local.origin.object.clone(),
                    etag: StrongEtag::test_value("timing"),
                },
                number: PageNumber(0),
            },
            mode: FetchMode::CopyOnly,
        };
        local
    }

    fn page_response(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        request: &PeerRequest,
    ) -> PeerResponse {
        let Operation::Page { page, .. } = &request.operation else {
            panic!()
        };
        PeerResponse::Page {
            metadata: crate::model::ObjectMetadata {
                content_type: None,
                version: page.version.clone(),
                length: 3,
                expires_at: crate::model::ExpiresAt::test_time(
                    uring_runtime::environment::wall_now() + Duration::from_secs(60),
                ),
            },
            // Intentionally not AEAD-valid: forwarding verification does not decrypt.
            ciphertext: BufferPool::new(admission.clone())
                .ciphertext(
                    admission
                        .reserve(
                            Some(&page.version.object.cache),
                            ResourceClass::Ciphertext,
                            19,
                        )
                        .unwrap(),
                    crate::model::PageEnvelope {
                        page: page.clone(),
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([2; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    vec![9; 19],
                )
                .unwrap(),
        }
    }

    #[test]
    fn page_timing_deterministic_delays_publish_once_only_after_verified_page() {
        let signers = signers();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let sender = Forwarding::new(signers[0].clone());
        let receiver = Forwarding::new(signers[2].clone());
        let local = page_request(&admission, 1);
        let metrics = Metrics::default();
        let clock = SimulationClock::new_at(7, Instant::now(), std::time::SystemTime::now());
        let _environment = clock.environment(1).enter();
        let mut timing = PageTiming::new(&metrics);
        timing.enable(&local, TransportPlan::Http, false);
        for stage in 0..4 {
            if stage > 0 {
                timing.begin();
            }
            clock.advance(Duration::from_nanos((stage + 1) as u64 * 11));
            timing.end(stage);
            for (count, sum) in STAGES {
                assert_eq!(metrics.count(count), 0);
                assert_eq!(metrics.count(sum), 0);
            }
        }
        let (signed, binding) = sender.sign_request(local).unwrap();
        let admitted = receiver.verify_request(signed).unwrap();
        let response = receiver
            .sign_response(
                admitted.binding(),
                page_response(&admission, admitted.request()),
            )
            .unwrap();
        let verified = sender.verify_response(response, &binding).unwrap();
        timing.success(&verified);
        timing.success(&verified);
        drop(timing);
        for (stage, (count, sum)) in STAGES.into_iter().enumerate() {
            assert_eq!(metrics.count(count), 1);
            assert_eq!(metrics.count(sum), (stage + 1) as u64 * 11);
        }
        assert_eq!(metrics.count(Event::PeerPageCensored), 0);
    }

    #[test]
    fn page_timing_excludes_metadata_transit_native_and_opaque() {
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let metrics = Metrics::default();
        for case in ["metadata", "bootstrap", "transit", "native", "opaque"] {
            let mut local = page_request(&admission, 1);
            if case == "metadata" {
                local = request(&admission, 1);
            }
            if case == "bootstrap" {
                local.operation = Operation::Bootstrap {
                    object: local.origin.object.clone(),
                    mode: FetchMode::CopyOnly,
                };
            }
            if case == "transit" {
                local.route.visited.push(NodeId(B.into()));
            }
            let plan = if case == "native" {
                TransportPlan::Rdma { rail: RailId(0) }
            } else {
                TransportPlan::Http
            };
            let mut timing = PageTiming::new(&metrics);
            timing.enable(&local, plan, case == "opaque");
            for stage in 0..4 {
                timing.begin();
                timing.end(stage);
            }
            drop(timing);
        }
        assert_eq!(metrics.count(Event::PeerPageCensored), 0);
        for (count, sum) in STAGES {
            assert_eq!(metrics.count(count), 0);
            assert_eq!(metrics.count(sum), 0);
        }
    }

    #[test]
    fn page_timing_pending_future_drop_counts_once_without_partial_samples() {
        use std::task::Context;
        use std::task::Poll;
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let metrics = Metrics::default();
        let local = page_request(&admission, 1);
        let clock = SimulationClock::new_at(8, Instant::now(), std::time::SystemTime::now());
        let _environment = clock.environment(1).enter();
        let mut work = Box::pin(async {
            let mut timing = PageTiming::new(&metrics);
            timing.enable(&local, TransportPlan::Http, false);
            clock.advance(Duration::from_nanos(17));
            timing.end(0);
            futures::future::pending::<()>().await;
            drop(timing);
        });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..3 {
            assert!(matches!(work.as_mut().poll(&mut cx), Poll::Pending));
            assert_eq!(metrics.count(Event::PeerPageCensored), 0);
        }
        drop(work);
        assert_eq!(metrics.count(Event::PeerPageCensored), 1);
        for (count, sum) in STAGES {
            assert_eq!(metrics.count(count), 0);
            assert_eq!(metrics.count(sum), 0);
        }
    }

    #[test]
    fn page_timing_invalid_signature_miss_and_drop_censor_without_partial_success() {
        let signers = signers();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let sender = Forwarding::new(signers[0].clone());
        let receiver = Forwarding::new(signers[2].clone());
        let metrics = Metrics::default();
        for (attempt, case) in ["invalid", "miss", "drop"].into_iter().enumerate() {
            let local = page_request(&admission, attempt as u8);
            let mut timing = PageTiming::new(&metrics);
            timing.enable(&local, TransportPlan::Http, false);
            for stage in 0..4 {
                timing.begin();
                timing.end(stage);
            }
            let (signed, binding) = sender.sign_request(local).unwrap();
            let admitted = receiver.verify_request(signed).unwrap();
            let response = if case == "miss" {
                PeerResponse::Miss
            } else {
                page_response(&admission, admitted.request())
            };
            let mut response = receiver
                .sign_response(admitted.binding(), response)
                .unwrap();
            if case == "invalid" {
                Arc::get_mut(&mut response.authentication.original)
                    .unwrap()
                    .signature[0] ^= 1;
                assert!(sender.verify_response(response, &binding).is_err());
            } else if case == "miss" {
                timing.success(&sender.verify_response(response, &binding).unwrap());
            }
            drop(timing);
            assert_eq!(metrics.count(Event::PeerPageCensored), attempt as u64 + 1);
            for (count, sum) in STAGES {
                assert_eq!(metrics.count(count), 0);
                assert_eq!(metrics.count(sum), 0);
            }
        }
    }

    #[test]
    fn page_timing_requester_reuses_authenticated_session_and_rejects_bad_signature() {
        use crate::topology::LinkHealth;
        use crate::topology::Member;
        use crate::topology::Membership;
        use crate::topology::Paths;
        use std::net::TcpListener;
        use std::task::Context;
        use std::task::Poll;
        let signers = signers();
        let fixture = SocketFixture::new(1);
        let receive_metrics = Metrics::default();
        let gate = crate::peer::receive::Gate::with_metrics(
            crate::peer::receive::Config {
                active: 1,
                ..Default::default()
            },
            receive_metrics.clone(),
        )
        .unwrap();
        let transfers = Rc::new(
            Rc::try_unwrap(fixture.transfers(signers[0].clone()))
                .ok()
                .unwrap()
                .with_receive_gate(gate),
        );
        let SocketFixture {
            admission,
            reactor,
            io,
            pool,
            ..
        } = fixture;
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let members = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [A, C]
                    .into_iter()
                    .map(|name| Member {
                        node: NodeId(name.into()),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: address.to_string(),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let metrics = Metrics::default();
        let requester = Requester::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4).with_peer_admission(
                crate::peer::AdaptivePeers::new(Default::default(), metrics.clone()).unwrap(),
            )),
            Rc::new(Forwarding::new(signers[0].clone())),
            transfers,
            Rc::new(
                PeerNetwork::new(
                    NodeId(A.into()),
                    crate::control::PublishedState::for_membership(members.clone()),
                )
                .unwrap(),
            ),
        )
        .with_metrics(metrics.clone());
        let scope =
            RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let client = async {
            for attempt in 0..3 {
                let response = if attempt == 1 {
                    requester
                        .request_direct(page_request(&admission, attempt), members.clone(), &scope)
                        .await
                } else {
                    requester
                        .request(page_request(&admission, attempt), members.clone(), &scope)
                        .await
                };
                assert_eq!(
                    receive_metrics.gauge(crate::telemetry::Gauge::PeerReceiveActive),
                    0,
                    "completed socket returns idle without permit"
                );
                assert_eq!(
                    receive_metrics.count(Event::PeerReceiveAdmitted),
                    attempt as u64 + 1
                );
                if attempt < 2 {
                    assert!(matches!(
                        response.unwrap().response(),
                        PeerResponse::Page { .. }
                    ));
                } else {
                    assert!(matches!(response, Err(Error::Unauthorized)));
                }
                for (count, _) in STAGES {
                    assert_eq!(metrics.count(count), (attempt as u64 + 1).min(2));
                }
            }
            assert_eq!(metrics.count(Event::PeerPageCensored), 1);
            Ok::<_, Error>(())
        };
        let server = async {
            // Only one accept and handshake: all three exchanges must reuse this session.
            let fd = reactor.accept(Rc::new(listener.into()), &scope).await?;
            let connection = crate::http::from_accepted(fd, &admission)?;
            let mut connection =
                crate::peer::protocol::accept(&io, connection, signers[2].clone(), &scope).await?;
            let auth = Forwarding::new(signers[2].clone());
            for attempt in 0..3 {
                let received = io.receive_head(connection, &scope).await?;
                let (head, _) = decode_envelope(received.value, false)?;
                let request = auth.verify_request(codec(&admission).request(head, &scope)?)?;
                let mut response = auth.sign_response(
                    request.binding(),
                    page_response(&admission, request.request()),
                )?;
                if attempt == 2 {
                    Arc::get_mut(&mut response.authentication.original)
                        .unwrap()
                        .signature[0] ^= 1;
                }
                let PeerResponse::Page { ciphertext, .. } = response.response else {
                    panic!()
                };
                let sent = io
                    .send_head(
                        received.connection,
                        encode_envelope(&response.authentication, true, 19)?,
                        &scope,
                    )
                    .await?;
                let sent = io
                    .write_body_range(sent.connection, ciphertext, 0..19, &scope)
                    .await?;
                connection = sent.lease;
                connection.finish_exchange()?;
            }
            Ok::<_, Error>(())
        };
        let mut work = Box::pin(async { futures::try_join!(client, server) });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            scope.check().unwrap();
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        drop(work);
        drop(requester);
        drop(pool);
        let mut drain = reactor.drain();
        while drain.as_mut().poll(&mut cx).is_pending() {
            scope.check().unwrap();
            reactor.poll_budgeted(128).unwrap();
        }
        drop(drain);
        assert_eq!(reactor.in_flight(), 0);
        drop(io);
        drop(reactor);
        admission.reclaim_buffers();
        for class in [
            ResourceClass::Connection,
            ResourceClass::IngressConnection,
            ResourceClass::Ciphertext,
            ResourceClass::RequestContext,
            ResourceClass::ControlProgress,
        ] {
            assert_eq!(admission.used(class), 0, "{class:?}");
        }
    }
}

const A: &str = "00000001-1111-4111-8111-111111111111";
const B: &str = "00000002-1111-4111-8111-111111111111";
const C: &str = "00000003-1111-4111-8111-111111111111";
const CACHE: &str = "cccccccc-1111-4111-8111-111111111111";
const CLUSTER: &str = "dddddddd-1111-4111-8111-111111111111";
type Discovery = (Rc<Keyring>, Rc<Certificates>);
fn identities() -> (Vec<Rc<Signatures>>, Vec<Discovery>) {
    crate::test_support::security::identities(
        ClusterId(CLUSTER.into()),
        &[A, B, C].map(|name| NodeId(name.into())),
        || crate::test_support::security::mac_test_key(CACHE),
    )
    .into_iter()
    .map(|identity| (identity.signatures, (identity.keys, identity.certificates)))
    .unzip()
}
pub(super) fn signers() -> Vec<Rc<Signatures>> {
    let (signers, _) = identities();
    signers
}
pub(super) fn request(
    admission: &flow_control::Quotas<AdmissionPolicy>,
    attempt: u8,
) -> PeerRequest {
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let object = ObjectId {
        cache: CacheId(CACHE.into()),
        key: CacheKey([3; 32]),
    };
    let route = RouteBudget {
        membership: MembershipVersion(1),
        request: scope.request,
        attempt: AttemptId([attempt; 16]),
        destination: NodeId(C.into()),
        visited: vec![NodeId(A.into())],
        remaining_links: 4,
        remaining_attempts: 0,
        deadline: scope.deadline,
    };
    let origin = PeerOriginContext {
        object: object.clone(),
        request: scope.request,
        attempt: route.attempt,
        metadata: None,
        authorization: Some(EncryptedAuthorization {
            key_id: KeyId([2; 16]),
            nonce: Nonce([4; 24]),
            ciphertext: vec![5; 32],
        }),
        reservation: admission
            .reserve(None, ResourceClass::RequestContext, 4096)
            .unwrap(),
        scope,
    };
    PeerRequest {
        operation: Operation::Metadata {
            object,
            selector: MetadataSelector::Fresh,
            mode: FetchMode::CopyOnly,
        },
        origin,
        route,
    }
}
pub(super) fn codec(admission: &Rc<flow_control::Quotas<AdmissionPolicy>>) -> SecurityCodec {
    SecurityCodec::new(admission.clone(), BufferPool::new(admission.clone()))
}

#[test]
fn receive_gate_operation_bypass_and_chained_small_caps_expire_and_recover() {
    use crate::telemetry::{Gauge, Metrics};
    let clock = uring_runtime::environment::SimulationClock::new(920);
    let _env = clock.environment(0).enter();
    let fixtures = [SocketFixture::new(1), SocketFixture::new(1)];
    let metrics = [Metrics::default(), Metrics::default()];
    let gates: Vec<_> = metrics
        .iter()
        .map(|m| {
            crate::peer::receive::Gate::with_metrics(
                crate::peer::receive::Config {
                    active: 1,
                    wait: Duration::from_millis(10),
                    ..Default::default()
                },
                m.clone(),
            )
            .unwrap()
        })
        .collect();
    let signers = signers();
    let transfers: Vec<_> = fixtures
        .iter()
        .zip(&gates)
        .map(|(f, g)| {
            Rc::try_unwrap(f.transfers(signers[0].clone()))
                .ok()
                .unwrap()
                .with_receive_gate(g.clone())
        })
        .collect();
    let scope = RequestScope::new(
        RequestId([20; 16]),
        uring_runtime::environment::now() + Duration::from_secs(1),
    )
    .unwrap();
    let mut requests: Vec<_> = fixtures.iter().map(|f| request(&f.admission, 20)).collect();
    let original = (
        requests[0].route.remaining_links,
        requests[0].route.remaining_attempts,
        requests[0].route.deadline.0,
    );
    let mut held = Vec::new();
    for (r, t) in requests.iter_mut().zip(&transfers) {
        r.operation = Operation::Bootstrap {
            object: r.origin.object.clone(),
            mode: FetchMode::CopyOnly,
        };
        held.push(futures::executor::block_on(t.admit_receive(r, &scope)).unwrap());
    }
    // Opposing chains already own their first-hop receive slots. Their next
    // exchanges queue at saturated receivers, not recursively acquiring a slot.
    requests[0].operation = Operation::Page {
        page: PageId {
            version: ObjectVersion {
                object: requests[0].origin.object.clone(),
                etag: StrongEtag::test_value("incast"),
            },
            number: PageNumber(0),
        },
        mode: FetchMode::CopyOnly,
    };
    requests[1].operation = Operation::Subscribe {
        subscription: super::subscriptions::Subscription {
            id: [20; 16],
            sequence: 0,
            page_budget: 1,
            byte_budget: PAGE_BYTES + 16,
            version: ObjectVersion {
                object: requests[1].origin.object.clone(),
                etag: StrongEtag::test_value("incast"),
            },
            demand: super::subscriptions::Demand::new(vec![super::subscriptions::PageInterval {
                start: 0,
                end: 1,
            }])
            .unwrap(),
        },
        mode: FetchMode::CopyOnly,
    };
    let mut pending: Vec<_> = transfers
        .iter()
        .zip(&requests)
        .map(|(t, r)| Box::pin(t.admit_receive(r, &scope)))
        .collect();
    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
    for p in &mut pending {
        assert!(p.as_mut().poll(&mut cx).is_pending());
    }
    for (f, t) in fixtures.iter().zip(&transfers) {
        let metadata = request(&f.admission, 21);
        assert!(
            futures::executor::block_on(t.admit_receive(&metadata, &scope))
                .unwrap()
                .is_none(),
            "metadata bypasses saturated body gate"
        );
    }
    clock.advance(Duration::from_millis(11));
    for g in &gates {
        g.poll_deadlines(64);
    }
    for p in &mut pending {
        assert!(matches!(
            p.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Err(Error::DeadlineExceeded))
        ));
    }
    drop(pending);
    drop(held);
    for (t, r) in transfers.iter().zip(&requests) {
        assert!(
            futures::executor::block_on(t.admit_receive(r, &scope))
                .unwrap()
                .is_some()
        );
    }
    assert_eq!(
        (
            requests[0].route.remaining_links,
            requests[0].route.remaining_attempts,
            requests[0].route.deadline.0
        ),
        original
    );
    for m in metrics {
        assert_eq!(m.gauge(Gauge::PeerReceiveActive), 0);
        assert_eq!(m.gauge(Gauge::PeerReceiveQueued), 0);
    }
}

/// Common signed HTTP plumbing. Scenarios retain their own membership and service.
pub(crate) struct SocketFixture {
    pub admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    pub reactor: Rc<crate::runtime::Reactor>,
    pub io: Rc<crate::http::HttpIo>,
    pub codec: Rc<SecurityCodec>,
    pub pool: Rc<crate::http::HttpPool>,
}

impl SocketFixture {
    pub fn new(pool_limit: usize) -> Self {
        Self::with_body_limit(pool_limit, PAGE_BYTES + 16)
    }
    pub fn with_body_limit(pool_limit: usize, body_limit: u64) -> Self {
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(crate::runtime::Reactor::new(admission.clone()));
        let io = Rc::new(crate::http::new_io(
            reactor.clone(),
            crate::http::Codec::new(protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            body_limit,
        ));
        Self {
            codec: Rc::new(codec(&admission)),
            pool: Rc::new(crate::http::new_pool(
                reactor.clone(),
                admission.clone(),
                pool_limit,
            )),
            admission,
            reactor,
            io,
        }
    }

    pub fn transfers(&self, signer: Rc<Signatures>) -> Rc<transport::Transfers> {
        Rc::new(transport::Transfers::new(
            self.pool.clone(),
            self.io.clone(),
            None,
            self.admission.clone(),
            self.codec.clone(),
            signer,
        ))
    }
}

/// Real outbound stack for destination-only tests. Count attempts even when a
/// route would fail before opening a socket, rather than relying on bad endpoints.
pub(crate) struct NoOutbound {
    pub requester: Rc<Requester>,
    _socket: SocketFixture,
}
pub(crate) fn signed_fixture_response() -> protocol::SignedResponse {
    let admission = flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let signers = signers();
    let sender = Forwarding::new(signers[0].clone());
    let receiver = Forwarding::new(signers[2].clone());
    let (signed, _) = sender.sign_request(request(&admission, 42)).unwrap();
    let admitted = receiver.verify_request(signed).unwrap();
    receiver
        .sign_response(admitted.binding(), PeerResponse::Miss)
        .unwrap()
}
impl NoOutbound {
    pub fn new(signer: Rc<Signatures>, network: Rc<PeerNetwork>) -> Self {
        let socket = SocketFixture::new(2);
        let requester = Rc::new(Requester::new(
            Rc::new(crate::topology::Paths::new(
                Rc::new(crate::topology::LinkHealth),
                4,
            )),
            Rc::new(Forwarding::new(signer.clone())),
            socket.transfers(signer),
            network,
        ));
        Self {
            requester,
            _socket: socket,
        }
    }
}
impl Drop for NoOutbound {
    fn drop(&mut self) {
        assert_eq!(
            self.requester.outbound_requests(),
            0,
            "destination must issue zero outbound requests"
        );
        assert_eq!(
            self._socket.reactor.in_flight(),
            0,
            "destination outbound stack must have no pending submissions"
        );
    }
}
#[test]
fn page_timing_duration_conversion_saturates_and_reversed_clock_is_zero() {
    use crate::peer::Event;
    use crate::peer::Metrics;
    use crate::peer::PageTiming;
    use crate::peer::now;
    use std::time::Duration;
    use uring_runtime::environment::SimulationClock;
    let metrics = Metrics::default();
    let clock = SimulationClock::new(9);
    let _environment = clock.environment(1).enter();
    let mut timing = PageTiming::new(&metrics);
    timing.active = true;
    timing.begin();
    clock.advance(Duration::from_secs(u64::MAX / 1_000_000_000 + 1));
    timing.end(0);
    assert_eq!(timing.durations[0], u64::MAX);
    timing.start = Some(now() + Duration::from_secs(1));
    timing.end(1);
    assert_eq!(timing.durations[1], 0);
    drop(timing);
    assert_eq!(metrics.count(Event::PeerPageCensored), 1);
    assert_eq!(metrics.count(Event::PeerPageCheckoutNs), 0);
}
mod adaptive {
    use crate::peer::*;
    #[test]
    fn hedges_require_spare_capacity_and_no_local_or_peer_pressure() {
        let clock = uring_runtime::environment::SimulationClock::new(905);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 4,
                per_peer: 2,
            },
            Metrics::default(),
        )
        .unwrap();
        let node = NodeId("a".into());
        assert!(owner.hedge_available(&node));
        let permit = owner.acquire(&node).unwrap();
        clock.advance(BACKOFF);
        permit.observe(Outcome::LocalPressure);
        assert!(!owner.hedge_available(&node));
        drop(permit);
        let owner = AdaptivePeers::new(
            Config {
                total: 4,
                per_peer: 2,
            },
            Metrics::default(),
        )
        .unwrap();
        let permit = owner.acquire(&node).unwrap();
        permit.observe(Outcome::PeerFailure);
        assert!(!owner.hedge_available(&node));
    }
    #[test]
    fn churn_cannot_evict_neutral_probe_backoff_with_old_update_time() {
        let clock = uring_runtime::environment::SimulationClock::new(783);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 512,
                per_peer: 1,
            },
            Metrics::default(),
        )
        .unwrap();
        let node = NodeId("000-circuit".into());
        let permit = owner.acquire(&node).unwrap();
        permit.observe(Outcome::PeerFailure);
        drop(permit);
        let held: Vec<_> = (1..CAPACITY)
            .map(|i| owner.acquire(&NodeId(i.to_string())).unwrap())
            .collect();
        clock.advance(Duration::from_secs(61));
        let probe = owner.acquire(&node).unwrap();
        drop(probe);
        assert!(!owner.available(&node));
        assert!(matches!(
            owner.acquire(&NodeId("new".into())),
            Err(Error::Overloaded)
        ));
        // Stale internal timestamps are covered by flow-control's generic test.
        // Service-level churn must preserve this circuit until its retry boundary.
        assert!(matches!(
            owner.acquire(&NodeId("new".into())),
            Err(Error::Overloaded)
        ));
        assert!(!owner.available(&node));
        drop(held);
    }
    #[test]
    fn shared_caps_local_pressure_and_completion_fences() {
        let metrics = Metrics::default();
        let owner = AdaptivePeers::new(
            Config {
                total: 2,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let worker = owner.clone();
        let a = NodeId("a".into());
        let b = NodeId("b".into());
        let permit = owner.acquire(&a).unwrap();
        assert!(matches!(worker.acquire(&a), Err(Error::Overloaded)));
        let io = permit.clone();
        permit.observe(Outcome::LocalPressure);
        drop(permit);
        assert!(matches!(worker.acquire(&a), Err(Error::Overloaded)));
        let second = worker.acquire(&b).unwrap();
        assert!(matches!(
            owner.acquire(&NodeId("c".into())),
            Err(Error::Overloaded)
        ));
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 2);
        drop((io, second));
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
        assert!(owner.available(&a));
        assert_eq!(metrics.count(Event::PeerLocalPressure), 1);
        assert_eq!(metrics.count(Event::PeerLinkFailure), 0);
    }
    #[test]
    fn recovery_requires_exclusive_verified_probe_and_old_success_cannot_clear_failure() {
        let clock = uring_runtime::environment::SimulationClock::new(779);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 4,
                per_peer: 4,
            },
            Metrics::default(),
        )
        .unwrap();
        let node = NodeId("a".into());
        let old = owner.acquire(&node).unwrap();
        let failed = owner.acquire(&node).unwrap();
        failed.observe(Outcome::PeerFailure);
        old.observe(Outcome::Verified);
        assert!(!owner.available(&node));
        drop((old, failed));
        clock.advance(BACKOFF);
        let probe = owner.acquire(&node).unwrap();
        assert!(matches!(owner.acquire(&node), Err(Error::Unavailable)));
        assert!(!owner.available(&node));
        drop(probe); // Cancellation/unverified completion cannot recover.
        assert!(!owner.available(&node));
        clock.advance(RECOVERY);
        let probe = owner.acquire(&node).unwrap();
        probe.observe(Outcome::Verified);
        assert!(!owner.available(&node)); // Still exclusive until its I/O fence.
        drop(probe);
        assert!(owner.available(&node));
        let held: Vec<_> = (0..3).map(|_| owner.acquire(&node).unwrap()).collect();
        assert!(matches!(owner.acquire(&node), Err(Error::Overloaded)));
        drop(held);
    }
    #[test]
    fn state_capacity_never_evicts_live_permits_and_ages_retired_entries() {
        let clock = uring_runtime::environment::SimulationClock::new(780);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 512,
                per_peer: 1,
            },
            Metrics::default(),
        )
        .unwrap();
        let permits: Vec<_> = (0..CAPACITY)
            .map(|i| owner.acquire(&NodeId(i.to_string())).unwrap())
            .collect();
        clock.advance(Duration::from_secs(61));
        assert!(matches!(
            owner.acquire(&NodeId("new".into())),
            Err(Error::Overloaded)
        ));
        drop(permits);
        let _permit = owner.acquire(&NodeId("new".into())).unwrap();
        // The generic test asserts the internal table length. Here verify the
        // configured capacity accepts exactly that many simultaneously live keys.
        let held: Vec<_> = (1..CAPACITY)
            .map(|i| owner.acquire(&NodeId(i.to_string())).unwrap())
            .collect();
        assert!(matches!(
            owner.acquire(&NodeId("overflow".into())),
            Err(Error::Overloaded)
        ));
        drop(held);
    }
    #[test]
    fn local_pressure_shrinks_node_limit_without_revoking_work_or_blame() {
        let clock = uring_runtime::environment::SimulationClock::new(781);
        let _env = clock.environment(0).enter();
        let metrics = Metrics::default();
        let owner = AdaptivePeers::new(
            Config {
                total: 4,
                per_peer: 4,
            },
            metrics.clone(),
        )
        .unwrap();
        let node = NodeId("a".into());
        let permits: Vec<_> = (0..4).map(|_| owner.acquire(&node).unwrap()).collect();
        clock.advance(BACKOFF);
        for permit in &permits {
            permit.observe(Outcome::LocalPressure);
        }
        assert_eq!(metrics.gauge(Gauge::PeerAdmissionLimit), 2);
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 4);
        assert!(owner.available(&node));
        assert_eq!(metrics.count(Event::PeerLinkFailure), 0);
        assert!(matches!(
            owner.acquire(&NodeId("other".into())),
            Err(Error::Overloaded)
        ));
        clock.advance(RECOVERY);
        permits[0].observe(Outcome::Verified);
        assert_eq!(metrics.gauge(Gauge::PeerAdmissionLimit), 3);
        drop(permits);
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
    }
}
