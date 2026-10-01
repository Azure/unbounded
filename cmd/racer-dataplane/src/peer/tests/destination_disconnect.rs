//! Real destination HTTP ingress with an independently owned, pending page flight.
use super::*;
use crate::{
    http::{
        Codec,
        connection::{ConnectionLease, HttpIo},
    },
    model::OriginContext,
    read::flight::{
        AcquisitionBudget, AcquisitionEvent, AcquisitionFailure, Flights, JoinedCopy, JoinedFlight,
    },
    runtime::reactor::{IoBuffer, Reactor},
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        paths::Paths,
    },
};
use std::{
    cell::Cell,
    future::Future,
    net::{Shutdown, TcpListener, TcpStream},
    task::{Context, Poll},
};

struct PendingPage {
    flights: Rc<Flights>,
    page: PageId,
    entered: Cell<usize>,
}
impl server::LocalPageService for PendingPage {
    fn serve_peer<'a>(
        &'a self,
        request: protocol::VerifiedRequest,
        _: crate::topology::membership::MembershipLease,
        scope: &'a RequestScope,
    ) -> crate::error::Operation<'a, PeerResponse> {
        Box::pin(async move {
            assert_eq!(scope.deadline.0, request.request().route.deadline.0);
            let JoinedCopy::Waiter(mut waiter) = self.flights.join_copy(&self.page, scope)? else {
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
impl PeerTransport for PendingPage {
    fn exchange<'a>(
        &'a self,
        _: protocol::SignedRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> crate::error::Operation<'a, protocol::SignedResponse> {
        Box::pin(async { panic!("final destination must not relay") })
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

fn page_result(admission: &Admission, page: &PageId) -> crate::memory::page::PageResult {
    use crate::{
        memory::pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
        model::{ExpiresAt, ObjectMetadata, PageEnvelope},
    };
    crate::memory::page::PageResult {
        metadata: ObjectMetadata {
            content_type: None,
            version: page.version.clone(),
            length: 3,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
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
            inner: Arc::new(CiphertextBytes {
                checksum: std::sync::OnceLock::new(),
                envelope: PageEnvelope {
                    page: page.clone(),
                    key_id: KeyId([0; 16]),
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
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    reactor.init().unwrap();
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(protocol::MAX_ENVELOPE_HEAD, crate::model::PAGE_BYTES + 16),
        admission.clone(),
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
                    alignment_enabled: false,
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::snapshot::PublishedState::for_membership(membership.clone()),
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
        crate::control::availability::Availability::permissive_for_tests(),
    ));
    let service = Rc::new(PendingPage {
        flights: flights.clone(),
        page: page.clone(),
        entered: Cell::new(0),
    });
    let relay = Rc::new(Relay::new(
        Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
        forwarding.clone(),
        service.clone(),
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
        RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(60)).unwrap();
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
    let accepted = flights.retain_operation(&leader, ()).unwrap();
    let JoinedCopy::Waiter(mut other) = flights.join_copy(&page, &independent).unwrap() else {
        panic!("independent waiter")
    };

    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let upstream = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (socket, _) = listener.accept().unwrap();
    let client =
        ConnectionLease::from_accepted(upstream.try_clone().unwrap().into(), &admission).unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let mut work = server.serve_connection(connection, &parent);
    let sender = Forwarding::new(signers[0].clone());
    let mut request = request(&admission, 41);
    request.operation = Operation::Page {
        page: page.clone(),
        mode: FetchMode::CopyOnly,
    };
    let (signed, binding) = sender.sign_request(request).unwrap();
    let head = WireCodec::encode(&signed.authentication, false, 0).unwrap();
    let client = drive(&reactor, async {
        let sending = async {
            let client = crate::security::connection::connect(
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
            let received = drive(&reactor, io.receive_head(client.connection, &parent)).unwrap();
            let (head, length) = WireCodec::decode(received.value, true).unwrap();
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
