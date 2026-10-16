//! Real read, Fill, crypto, and disk paths with a gated authenticated peer.
use super::fill::*;
use super::hot_reads::coordinator;
use crate::client::{ClientRequest, ReadKind};
use crate::peer::forwarding::{Forwarding, VerifiedResponse};
use crate::peer::protocol::PeerRequest;
use crate::read::ReadPayload;
use crate::test_support::security::{network, node};

#[test]
fn completed_warm_two_owner_extension_crossing_expiry_revalidates_evicted_pages() {
    completed_extension(false);
    completed_extension(true);
}

fn completed_extension(refuse: bool) {
    let clock = uring_runtime::environment::SimulationClock::new(197);
    let _environment = clock.environment(0).enter();
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    let mut fixtures = [
        fixture_with(PAGE_BYTES + 7, None),
        fixture_with(PAGE_BYTES + 7, None),
    ];
    let membership = Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(1),
            vec![Member {
                node: node(0),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8200".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let mut locals = Vec::new();
    let mut endpoints = Vec::new();
    for f in &fixtures {
        f.reactor.init().unwrap();
        let (local, endpoint, _) = coordinator(f, &signers[0], &membership, NoPeer::requester());
        locals.push(local);
        endpoints.push(endpoint);
    }
    let workers = vec![WorkerId(0), WorkerId(1)];
    let directory = Arc::new(
        crate::read::dispatch::WorkerDirectory::new(
            Arc::new(crate::read::dispatch::WorkerMap::new(workers.clone()).unwrap()),
            workers,
            16,
        )
        .unwrap(),
    );
    directory.test_observation_mailbox();
    directory
        .warm
        .configure(crate::read::warm::Config {
            fill_timeout: Duration::from_secs(4),
            window: 1,
            ..Default::default()
        })
        .unwrap();
    let _local = directory.simulation_scope(Some((WorkerId(0), locals[0].clone())));
    let metadata = fixtures[0].origin.metadata.clone();
    let caller = RequestScope::new(
        RequestId([41; 16]),
        uring_runtime::environment::now() + Duration::from_secs(60),
    )
    .unwrap();
    let context = OriginContext {
        object: metadata.version.object.clone(),
        metadata: None,
        authorization: None,
    };
    let make_head = |ttl| {
        crate::read::warm::admit(
            directory.clone(),
            metadata.clone(),
            membership.clone(),
            locals[0]
                .credentials
                .local_context(&context, &caller)
                .unwrap(),
            ttl,
            &caller,
        )
    };
    let mut head = Box::pin(make_head(Duration::from_secs(5)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut commands: Vec<Operation<'static, ()>> = Vec::new();
    let mut pump = |blocked: Option<WorkerId>| {
        queue.poll(&mut cx, 64);
        for (i, local) in locals.iter().enumerate() {
            if blocked != Some(WorkerId(i as u16))
                && let Some(command) =
                    directory.test_warm_command(WorkerId(i as u16), local.clone())
            {
                commands.push(command);
            }
        }
        let mut i = 0;
        while i < commands.len() {
            if let Poll::Ready(result) = commands[i].as_mut().poll(&mut cx) {
                result.unwrap();
                drop(commands.remove(i));
            } else {
                i += 1;
            }
        }
        for f in &mut fixtures {
            uring_runtime::group::Service::poll_budgeted(&mut f.engine, &mut cx, 64).unwrap();
            f.crypto.poll_budgeted(64).unwrap();
            f.reactor.poll_budgeted(64).unwrap();
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    let mut accepted = None;
    for _ in 0..2048 {
        if accepted.is_none()
            && let Poll::Ready(result) = head
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            accepted = Some(result.unwrap());
        }
        pump(None);
        if accepted.is_some() && queue.pending() == 0 {
            break;
        }
    }
    assert!(accepted.is_some());
    drop(head);
    let page = (0..2)
        .map(|number| PageId {
            version: metadata.version.clone(),
            number: PageNumber(number),
        })
        .find(|page| directory.page_owner(page).unwrap() == WorkerId(1))
        .expect("fixture covers delayed owner");
    let delayed_index = locals[1].fill.dependencies.writer.index().clone();
    assert!(delayed_index.lookup(&page).unwrap().is_some());
    clock.advance(Duration::from_secs(3));
    let mut extension = Box::pin(make_head(Duration::from_secs(20)));
    for _ in 0..8 {
        assert!(
            extension
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        pump(Some(WorkerId(1)));
    }
    clock.advance(Duration::from_secs(3));
    let entry = delayed_index.lookup(&page).unwrap().unwrap();
    assert!(delayed_index.held_page_expiry(&page).is_none());
    delayed_index.remove_if_matches(&page, &entry.location);
    assert!(delayed_index.lookup(&page).unwrap().is_none());
    let mut pressure = metadata.version.clone();
    pressure.etag = crate::model::StrongEtag::test_value("pressure");
    if refuse {
        delayed_index
            .reserve_hold(
                &pressure,
                8,
                uring_runtime::environment::wall_now() + Duration::from_secs(30),
            )
            .unwrap();
    }
    let get_scope = RequestScope::new(
        RequestId([42; 16]),
        uring_runtime::environment::now() + Duration::from_secs(20),
    )
    .unwrap();
    let mut get = Box::pin(directory.warm.wait(&page, &get_scope));
    assert!(
        get.as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending(),
        "old frontier must be hidden"
    );
    let mut accepted = None;
    let mut delivered = false;
    for _ in 0..2048 {
        if accepted.is_none()
            && let Poll::Ready(result) = extension
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            accepted = Some(result);
        }
        if !delivered
            && let Poll::Ready(result) = get
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            assert_eq!(result.unwrap(), !refuse);
            delivered = true;
        }
        pump(None);
        if delivered && accepted.is_some() && queue.pending() == 0 {
            break;
        }
    }
    assert!(delivered && accepted.is_some());
    if refuse {
        assert_eq!(accepted.unwrap(), Err(Error::Overloaded));
        assert!(delayed_index.lookup(&page).unwrap().is_none());
        directory
            .warm
            .poll_releases(
                WorkerId(0),
                locals[0].fill.dependencies.writer.index(),
                &mut Context::from_waker(futures::task::noop_waker_ref()),
            )
            .unwrap();
        let first = (0..2)
            .map(|number| PageId {
                version: metadata.version.clone(),
                number: PageNumber(number),
            })
            .find(|page| directory.page_owner(page).unwrap() == WorkerId(0))
            .unwrap();
        assert!(
            locals[0]
                .fill
                .dependencies
                .writer
                .index()
                .held_page_expiry(&first)
                .unwrap()
                > uring_runtime::environment::wall_now(),
            "partial extension failure preserves already-promised pages"
        );
    } else {
        assert!(delayed_index.lookup(&page).unwrap().is_some());
        assert_eq!(
            delayed_index.held_page_expiry(&page),
            Some(accepted.unwrap().unwrap())
        );
    }
}

struct Peer {
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    sender: Forwarding,
    receiver: Forwarding,
    pages: Vec<PageResult>,
    blocked: Cell<bool>,
    wake: RefCell<Option<std::task::Waker>>,
    calls: RefCell<Vec<u64>>,
}
impl Peer {
    fn available(&self, _: &Arc<Membership>, _: &racer_control_wire::NodeId) -> bool {
        false
    }
    fn direct<'a>(
        &'a self,
        _: PeerRequest,
        _: Arc<Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async { panic!("unexpected hedge") })
    }
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        _: Arc<Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let PeerOperation::Page { page, .. } = &request.operation else {
                panic!("GET bypassed warm gate with provider selection");
            };
            let number = page.number.0;
            self.calls.borrow_mut().push(number);
            let cancellation = scope.cancellation.subscribe()?;
            std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                *self.wake.borrow_mut() = Some(cx.waker().clone());
                if self.blocked.get() {
                    Poll::Pending
                } else {
                    Poll::Ready(Ok::<(), Error>(()))
                }
            })
            .await?;
            let (signed, binding) = self.sender.sign_request(request)?;
            let verified = self.receiver.verify_request(signed)?;
            let page = &self.pages[number as usize];
            let ciphertext = page.ciphertext.clone().rehome(self.admission.reserve(
                Some(&page.metadata.version.object.cache),
                crate::admission::ResourceClass::Ciphertext,
                page.ciphertext.bytes().len(),
            )?)?;
            self.sender.verify_response(
                self.receiver.sign_response(
                    verified.binding(),
                    PeerResponse::Page {
                        metadata: page.metadata.clone(),
                        ciphertext,
                    },
                )?,
                &binding,
            )
        })
    }
    fn unblock(&self) {
        self.blocked.set(false);
        if let Some(wake) = self.wake.borrow_mut().take() {
            wake.wake();
        }
    }
}

#[test]
fn warm_head_detaches_and_real_gets_share_peer_pipeline_and_disk_frontier() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(2);
    let mut source = fixture_with(2 * PAGE_BYTES + 7, None);
    let mut pages = Vec::new();
    for number in 0..3 {
        let mut budget = AcquisitionBudget::new(source.scope.deadline.0, 8, 16);
        pages.push(
            drive(
                source.fill.acquire(
                    PageId {
                        version: source.page.version.clone(),
                        number: PageNumber(number),
                    },
                    source.membership.clone(),
                    &source.context,
                    &source.scope,
                    &mut budget,
                ),
                &mut source.engine,
                &source.crypto,
            )
            .unwrap(),
        );
    }
    let mut f = fixture_with(2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(1),
            (0..2)
                .map(|i| Member {
                    node: node(i),
                    shares: NonZeroU32::new(if i == 1 { 1_000_000 } else { 1 }).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8200 + i),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let peer = Rc::new(Peer {
        admission: f.fill.dependencies.admission.clone(),
        sender: Forwarding::new(signers[0].clone()),
        receiver: Forwarding::new(signers[1].clone()),
        pages,
        blocked: Cell::new(true),
        wake: RefCell::new(None),
        calls: RefCell::new(Vec::new()),
    });
    let requester = Requester::scripted(peer.clone(), Peer::available, Peer::request, Peer::direct);
    let (local, mut endpoint, _publication) = coordinator(&f, &signers[0], &membership, requester);
    let owners = f.fill.dependencies.metadata_owner.clone();
    owners
        .warm
        .configure(crate::read::warm::Config {
            window: 1,
            ..Default::default()
        })
        .unwrap();
    let origin = || OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let head_scope = RequestScope::new(RequestId([81; 16]), f.scope.deadline.0).unwrap();
    let mut head = local.read(
        ClientRequest {
            kind: ReadKind::HeadPinned {
                etag: f.page.version.etag.clone(),
            },
            retain: Some(Duration::from_secs(60)),
            origin: origin(),
        },
        &head_scope,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut response = None;
    for _ in 0..1024 {
        if let Poll::Ready(result) = head.as_mut().poll(&mut cx) {
            response = Some(result.unwrap());
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    let until = response
        .expect("HEAD must not wait for peer payload")
        .retain_until
        .expect("hold accepted");
    drop(head);
    head_scope.cancel().unwrap();
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .lookup(&f.page)
            .unwrap()
            .is_none()
    );
    let mut duplicate = local.read(
        ClientRequest {
            kind: ReadKind::HeadPinned {
                etag: f.page.version.etag.clone(),
            },
            retain: Some(Duration::from_secs(1)),
            origin: origin(),
        },
        &f.scope,
    );
    let mut repeated = None;
    for _ in 0..1024 {
        if let Poll::Ready(result) = duplicate.as_mut().poll(&mut cx) {
            repeated = Some(result.unwrap());
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert_eq!(repeated.unwrap().retain_until, Some(until));
    drop(duplicate);
    let mut streams = Vec::new();
    for ordered in [true, false] {
        let read = futures::executor::block_on(
            local.read(
                ClientRequest {
                    retain: None,
                    origin: origin(),
                    kind: ReadKind::Subscription {
                        pin: Some(f.page.version.etag.clone()),
                        range: Some(crate::model::ByteRange::From(2 * PAGE_BYTES)),
                        page_credits: 1,
                        byte_credits: PAGE_BYTES,
                        ordered,
                    },
                },
                &RequestScope::new(RequestId([82 + u8::from(ordered); 16]), f.scope.deadline.0)
                    .unwrap(),
            ),
        )
        .unwrap();
        let ReadPayload::Stream(stream) = read.payload else {
            panic!("body expected");
        };
        streams.push(stream);
    }
    for _ in 0..64 {
        for stream in &mut streams {
            assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert_eq!(
        &*peer.calls.borrow(),
        &[0],
        "neither GET requests its unstarted tail page"
    );
    peer.unblock();
    let mut delivered = [false; 2];
    for _ in 0..8192 {
        for (i, stream) in streams.iter_mut().enumerate() {
            if !delivered[i]
                && let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx)
            {
                let lease = result.unwrap().unwrap();
                assert_eq!(lease.slice().page, PageNumber(2));
                assert_eq!(lease.slice().length, 7);
                delivered[i] = true;
            }
        }
        if delivered == [true; 2] {
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!(
        delivered,
        [true; 2],
        "calls={:?}, warm={:?}, drivers={}, pending={}, io={}",
        peer.calls.borrow(),
        owners.warm.snapshot(),
        queue.pending(),
        f.fill.dependencies.writer.pending_count(),
        f.reactor.in_flight()
    );
    assert_eq!(
        &*peer.calls.borrow(),
        &[0, 1, 2],
        "one outbound acquisition per page across both GET modes"
    );
    assert_eq!(
        f.origin.calls.get(),
        0,
        "normal topology uses peer, not direct origin"
    );
    for number in 0..3 {
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        assert!(
            f.fill
                .dependencies
                .writer
                .index()
                .lookup(&page)
                .unwrap()
                .is_some()
        );
        assert_eq!(
            f.fill.dependencies.writer.index().held_page_expiry(&page),
            Some(until)
        );
    }
    drop(streams);
    for _ in 0..32 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn warm_partial_failure_releases_unused_hold_and_retries_without_refetching_disk() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    let mut f = fixture_with(PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(1),
            vec![Member {
                node: node(0),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8200".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let (local, mut endpoint, _) = coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let owners = f.fill.dependencies.metadata_owner.clone();
    owners
        .warm
        .configure(crate::read::warm::Config {
            window: 1,
            ..Default::default()
        })
        .unwrap();
    let request = || ClientRequest {
        retain: Some(Duration::from_secs(60)),
        kind: ReadKind::HeadPinned {
            etag: f.page.version.etag.clone(),
        },
        origin: OriginContext {
            object: f.context.object.clone(),
            metadata: None,
            authorization: None,
        },
    };
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    f.origin.blocked_pages.borrow_mut().insert(1);
    let mut head = local.read(request(), &f.scope);
    let mut accepted = None;
    for _ in 0..1024 {
        if let Poll::Ready(result) = head.as_mut().poll(&mut cx) {
            accepted = Some(result.unwrap());
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!(accepted.unwrap().retain_until.is_some());
    drop(head);
    for _ in 0..8192 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        if f.origin.started_pages.borrow().contains(&1) {
            break;
        }
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .held_page_expiry(&f.page)
            .is_some()
    );
    f.origin.version_unavailable.set(true);
    f.origin.blocked_pages.borrow_mut().clear();
    for _ in 0..1024 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        if owners.warm.snapshot().0 == 0 {
            break;
        }
    }
    assert_eq!(owners.warm.snapshot(), (0, 0));
    pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .held_page_expiry(&f.page)
            .is_some(),
        "partial failure preserves promised disk page"
    );
    // A new registry job expands this existing partial hold, then fails again.
    let mut failed_retry = local.read(request(), &f.scope);
    let mut admitted = false;
    for _ in 0..1024 {
        if let Poll::Ready(result) = failed_retry.as_mut().poll(&mut cx) {
            assert!(result.unwrap().retain_until.is_some());
            admitted = true;
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!(admitted);
    drop(failed_retry);
    for _ in 0..1024 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        if owners.warm.snapshot().0 == 0 {
            break;
        }
    }
    pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    assert_eq!(owners.warm.snapshot().0, 0);
    let mut other = f.page.version.clone();
    other.etag = crate::model::StrongEtag::test_value("other-hold");
    f.fill
        .dependencies
        .writer
        .index()
        .reserve_hold(
            &other,
            7,
            uring_runtime::environment::wall_now() + Duration::from_secs(30),
        )
        .expect("failed expansion released its unused second page");
    f.fill
        .dependencies
        .writer
        .index()
        .release_hold(&other)
        .unwrap();
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .held_page_expiry(&f.page)
            .is_some()
    );
    f.origin.version_unavailable.set(false);
    for ordered in [true, false] {
        let caller =
            RequestScope::new(RequestId([90 + u8::from(ordered); 16]), f.scope.deadline.0).unwrap();
        let response = futures::executor::block_on(local.read(
            ClientRequest {
                retain: None,
                kind: ReadKind::Subscription {
                    pin: Some(f.page.version.etag.clone()),
                    range: Some(crate::model::ByteRange::From(PAGE_BYTES)),
                    page_credits: 1,
                    byte_credits: PAGE_BYTES,
                    ordered,
                },
                origin: OriginContext {
                    object: f.context.object.clone(),
                    metadata: None,
                    authorization: None,
                },
            },
            &caller,
        ))
        .unwrap();
        let ReadPayload::Stream(mut stream) = response.payload else {
            panic!("body expected")
        };
        let mut delivered = false;
        for _ in 0..1024 {
            if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                assert_eq!(result.unwrap().unwrap().slice().page, PageNumber(1));
                delivered = true;
                break;
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
        assert!(delivered, "GET falls back only after failed warm finishes");
    }
    let mut retry = local.read(request(), &f.scope);
    let mut accepted = None;
    for _ in 0..1024 {
        if let Poll::Ready(result) = retry.as_mut().poll(&mut cx) {
            accepted = Some(result.unwrap());
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!(
        accepted.unwrap().retain_until.is_some(),
        "same-version retry admits after rollback"
    );
    drop(retry);
    let tail = PageId {
        version: f.page.version.clone(),
        number: PageNumber(1),
    };
    for _ in 0..8192 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        if f.fill
            .dependencies
            .writer
            .index()
            .held_page_expiry(&tail)
            .is_some()
            && queue.pending() == 0
        {
            break;
        }
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .held_page_expiry(&tail)
            .is_some()
    );
    assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1, 1, 1]);
    endpoint.uninstall().unwrap();
}

#[test]
fn warm_drain_cancels_detached_job_and_refused_head_keeps_metadata_success() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    for length in [3, 9 * PAGE_BYTES] {
        let mut f = fixture_with(length, None);
        f.reactor.init().unwrap();
        let membership = Arc::new(
            Membership::validate(
                racer_control_wire::MembershipVersion(1),
                vec![Member {
                    node: node(0),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:8200".into(),
                    rails: vec![],
                    site: String::new(),
                }],
            )
            .unwrap(),
        );
        let (local, mut endpoint, _) =
            coordinator(&f, &signers[0], &membership, NoPeer::requester());
        f.origin.blocked_pages.borrow_mut().insert(0);
        let mut head = local.read(
            ClientRequest {
                retain: Some(Duration::from_secs(60)),
                kind: ReadKind::HeadPinned {
                    etag: f.page.version.etag.clone(),
                },
                origin: OriginContext {
                    object: f.context.object.clone(),
                    metadata: None,
                    authorization: None,
                },
            },
            &f.scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut response = None;
        for _ in 0..1024 {
            if let Poll::Ready(result) = head.as_mut().poll(&mut cx) {
                response = Some(result.unwrap());
                break;
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
        let response = response.unwrap();
        assert_eq!(response.metadata.length, length);
        assert_eq!(response.retain_until.is_some(), length == 3);
        drop(head);
        endpoint.stop_admission();
        f.origin.blocked_pages.borrow_mut().clear();
        for _ in 0..1024 {
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
            if endpoint.is_drained() && queue.pending() == 0 {
                break;
            }
        }
        assert!(endpoint.is_drained());
        assert_eq!(queue.pending(), 0);
        assert_eq!(f.fill.dependencies.metadata_owner.warm.snapshot(), (0, 0));
        assert!(
            f.fill
                .dependencies
                .writer
                .index()
                .hold_expiry(&f.page.version)
                .is_none()
        );
        endpoint.uninstall().unwrap();
    }
}

#[test]
fn warm_cancel_keeps_get_gate_until_detached_origin_driver_fences() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    let mut f = fixture();
    f.reactor.init().unwrap();
    let membership = Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(1),
            vec![Member {
                node: node(0),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8200".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let (local, mut endpoint, _) = coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let owners = f.fill.dependencies.metadata_owner.clone();
    let origin = || OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    f.origin.blocked_pages.borrow_mut().insert(0);
    let mut head = local.read(
        ClientRequest {
            retain: Some(Duration::from_secs(60)),
            kind: ReadKind::HeadPinned {
                etag: f.page.version.etag.clone(),
            },
            origin: origin(),
        },
        &f.scope,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut accepted = false;
    for _ in 0..1024 {
        if let Poll::Ready(result) = head.as_mut().poll(&mut cx) {
            assert!(result.unwrap().retain_until.is_some());
            accepted = true;
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!(accepted);
    drop(head);
    for _ in 0..64 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert_eq!(f.origin.calls.get(), 1);
    let caller = RequestScope::new(RequestId([99; 16]), f.scope.deadline.0).unwrap();
    let response = futures::executor::block_on(local.read(
        ClientRequest {
            retain: None,
            origin: origin(),
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: None,
                page_credits: 1,
                byte_credits: PAGE_BYTES,
                ordered: false,
            },
        },
        &caller,
    ))
    .unwrap();
    let ReadPayload::Stream(mut stream) = response.payload else {
        panic!("stream");
    };
    owners.warm.stop();
    for _ in 0..64 {
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert_eq!(
        owners.warm.snapshot().0,
        1,
        "canceled driver still owns warm exclusion"
    );
    assert_eq!(f.origin.calls.get(), 1, "no unordered outbound overlap");
    f.origin.blocked_pages.borrow_mut().clear();
    let mut delivered = false;
    for _ in 0..1024 {
        if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
            assert!(result.unwrap().is_some());
            delivered = true;
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!(delivered);
    assert_eq!(owners.warm.snapshot().0, 0);
    drop(stream);
    for _ in 0..32 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn warm_follower_cancellation_waits_for_existing_foreground_supplier() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let mut f = fixture();
    f.origin.blocked_pages.borrow_mut().insert(0);
    let foreground_scope = RequestScope::new(RequestId([101; 16]), f.scope.deadline.0).unwrap();
    let warm_scope = RequestScope::new(RequestId([102; 16]), f.scope.deadline.0).unwrap();
    let mut foreground_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    let mut warm_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    let mut foreground = f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &foreground_scope,
        &mut foreground_budget,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..32 {
        assert!(foreground.as_mut().poll(&mut cx).is_pending());
        uring_runtime::drivers::poll(&mut cx, 64);
    }
    assert_eq!(f.origin.calls.get(), 1);
    let mut warm = Box::pin(f.fill.acquire_warm(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &warm_scope,
        &mut warm_budget,
    ));
    assert!(warm.as_mut().poll(&mut cx).is_pending());
    warm_scope.cancel().unwrap();
    foreground_scope.cancel().unwrap();
    assert!(matches!(
        foreground.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    drop(foreground);
    for _ in 0..32 {
        assert!(
            warm.as_mut().poll(&mut cx).is_pending(),
            "follower cancellation is not the supplier fence"
        );
        uring_runtime::drivers::poll(&mut cx, 64);
    }
    f.origin.blocked_pages.borrow_mut().clear();
    let mut done = false;
    for _ in 0..1024 {
        if let Poll::Ready(result) = warm.as_mut().poll(&mut cx) {
            assert!(matches!(result, Err(Error::Cancelled)));
            done = true;
            break;
        }
        uring_runtime::drivers::poll(&mut cx, 64);
        uring_runtime::group::Service::poll_budgeted(&mut f.engine, &mut cx, 64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
    }
    assert!(done);
    assert_eq!(f.origin.calls.get(), 1);
}
