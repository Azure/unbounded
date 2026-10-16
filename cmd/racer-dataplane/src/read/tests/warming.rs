//! Real read, Fill, crypto, and disk paths with a gated authenticated peer.
use super::fill::*;
use super::hot_reads::coordinator;
use crate::client::{ClientRequest, ReadKind};
use crate::peer::forwarding::{Forwarding, VerifiedResponse};
use crate::peer::protocol::PeerRequest;
use crate::peer::protocol::WarmAction;
use crate::peer::server::LocalPageService;
use crate::read::ReadPayload;
use crate::test_support::security::{network, node};

#[test]
fn completed_warm_two_owner_extension_crossing_expiry_revalidates_evicted_pages() {
    completed_extension(false);
    completed_extension(true);
}

fn completed_extension(refuse: bool) {
    let clock = racer_platform::environment::SimulationClock::new(197);
    let _environment = clock.environment(0).enter();
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
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
        racer_platform::environment::now() + Duration::from_secs(60),
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
            racer_platform::group::Service::poll(&mut f.engine, &mut cx, 64).unwrap();
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
                racer_platform::environment::wall_now() + Duration::from_secs(30),
            )
            .unwrap();
    }
    let get_scope = RequestScope::new(
        RequestId([42; 16]),
        racer_platform::environment::now() + Duration::from_secs(20),
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
            assert_eq!(
                result.unwrap() == crate::read::warm::WarmWait::Local,
                !refuse
            );
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
                > racer_platform::environment::wall_now(),
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
    /// (warm fill, page) in request order.
    calls: RefCell<Vec<(bool, u64)>>,
    holds: RefCell<Vec<std::time::SystemTime>>,
    releases: Cell<usize>,
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
            // Remote warm holds and releases answer immediately; fills and page
            // reads share the gate so tests can observe the warm frontier.
            let (number, reply) = match &request.operation {
                PeerOperation::Page { page, .. } => (Some(page.number.0), None),
                PeerOperation::Warm { action, .. } => match action {
                    WarmAction::Hold { until, .. } => {
                        self.holds.borrow_mut().push(*until);
                        (None, Some(PeerResponse::Held { until: *until }))
                    }
                    WarmAction::Fill { page, .. } => (Some(page.0), Some(PeerResponse::Warmed)),
                    WarmAction::Release => {
                        self.releases.set(self.releases.get() + 1);
                        (None, Some(PeerResponse::Released))
                    }
                },
                _ => panic!("GET bypassed warm gate with provider selection"),
            };
            if let Some(number) = number {
                self.calls.borrow_mut().push((reply.is_some(), number));
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
            }
            let (signed, binding) = self.sender.sign_request(request)?;
            let verified = self.receiver.verify_request(signed)?;
            let response = match (reply, number) {
                (Some(reply), _) => reply,
                (None, Some(number)) => {
                    let page = &self.pages[number as usize];
                    let ciphertext = page.ciphertext.clone().rehome(self.admission.reserve(
                        Some(&page.metadata.version.object.cache),
                        crate::admission::ResourceClass::Ciphertext,
                        page.ciphertext.bytes().len(),
                    )?)?;
                    PeerResponse::Page {
                        metadata: page.metadata.clone(),
                        ciphertext,
                    }
                }
                (None, None) => unreachable!(),
            };
            self.sender.verify_response(
                self.receiver.sign_response(verified.binding(), response)?,
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
    // node(1) is the primary owner of every page: it holds and fills them, and
    // the requester keeps none of them, yet GETs still wait on the frontier.
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
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
        holds: RefCell::new(Vec::new()),
        releases: Cell::new(0),
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
    assert_eq!(peer.holds.borrow().len(), 2, "one remote hold per HEAD");
    assert_eq!(
        &*peer.calls.borrow(),
        &[(true, 0)],
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
        &[
            (true, 0),
            (true, 1),
            (true, 2),
            (true, 0),
            (true, 1),
            (true, 2),
            (false, 2)
        ],
        "the owner warms every page, the extending HEAD reverifies every page \
         at the owner, and both GET modes share one page read"
    );
    assert_eq!(peer.releases.get(), 0, "a completed warm keeps its holds");
    assert_eq!(
        f.origin.calls.get(),
        0,
        "normal topology uses peer, not direct origin"
    );
    assert!(until <= *peer.holds.borrow().first().unwrap());
    for number in 0..3 {
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        assert_eq!(
            f.fill.dependencies.writer.index().held_page_expiry(&page),
            None,
            "the requester holds none of the remote primary's pages"
        );
    }
    assert_eq!(
        f.fill
            .dependencies
            .writer
            .index()
            .hold_expiry(&f.page.version),
        None
    );
    drop(streams);
    for _ in 0..32 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn warm_partial_failure_releases_unused_hold_and_retries_without_refetching_disk() {
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
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
            racer_platform::environment::wall_now() + Duration::from_secs(30),
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
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
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
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
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
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
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
        racer_platform::drivers::poll(&mut cx, 64);
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
        racer_platform::drivers::poll(&mut cx, 64);
    }
    f.origin.blocked_pages.borrow_mut().clear();
    let mut done = false;
    for _ in 0..1024 {
        if let Poll::Ready(result) = warm.as_mut().poll(&mut cx) {
            assert!(matches!(result, Err(Error::Cancelled)));
            done = true;
            break;
        }
        racer_platform::drivers::poll(&mut cx, 64);
        racer_platform::group::Service::poll(&mut f.engine, &mut cx, 64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
    }
    assert!(done);
    assert_eq!(f.origin.calls.get(), 1);
}

/// Members with one dominant share, so placement is deterministic per test.
fn weighted(version: u64, shares: &[u32]) -> Arc<Membership> {
    Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(version),
            shares
                .iter()
                .enumerate()
                .map(|(i, shares)| Member {
                    node: node(i),
                    shares: NonZeroU32::new(*shares).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8200 + i),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    )
}

/// Remote warm owner double: scripted hold and fill outcomes per destination.
struct Owners {
    sender: Forwarding,
    receivers: Vec<(racer_control_wire::NodeId, Forwarding)>,
    refuse_hold: Option<racer_control_wire::NodeId>,
    fail_fill: bool,
    holds: RefCell<Vec<racer_control_wire::NodeId>>,
    fills: Cell<usize>,
    releases: RefCell<Vec<racer_control_wire::NodeId>>,
    /// Leases granted by Hold, validated on Fill as the real owner does.
    leases: RefCell<Vec<(racer_control_wire::NodeId, [u8; 16])>>,
}
impl Owners {
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
        _: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let destination = request.route.destination.clone();
            let PeerOperation::Warm { action, lease, .. } = &request.operation else {
                panic!("only warm requests expected");
            };
            let key = (destination.clone(), *lease);
            let response = match action {
                WarmAction::Hold { until, .. } => {
                    self.holds.borrow_mut().push(destination.clone());
                    if self.refuse_hold.as_ref() == Some(&destination) {
                        PeerResponse::Overloaded
                    } else {
                        if !self.leases.borrow().contains(&key) {
                            self.leases.borrow_mut().push(key);
                        }
                        PeerResponse::Held { until: *until }
                    }
                }
                WarmAction::Fill { .. } => {
                    self.fills.set(self.fills.get() + 1);
                    if !self.leases.borrow().contains(&key) {
                        PeerResponse::Unavailable
                    } else if self.fail_fill {
                        PeerResponse::Overloaded
                    } else {
                        PeerResponse::Warmed
                    }
                }
                WarmAction::Release => {
                    self.releases.borrow_mut().push(destination.clone());
                    self.leases.borrow_mut().retain(|held| held != &key);
                    PeerResponse::Released
                }
            };
            let (_, receiver) = self
                .receivers
                .iter()
                .find(|(node, _)| node == &destination)
                .unwrap();
            let (signed, binding) = self.sender.sign_request_to(request, &destination)?;
            let verified = receiver.verify_request(signed)?;
            self.sender.verify_response(
                receiver.sign_response(verified.binding(), response)?,
                &binding,
            )
        })
    }
}

/// Run a warm HEAD from node(0) against scripted remote owners.
fn remote_warm(
    shares: &[u32],
    refuse_hold: Option<usize>,
    fail_fill: bool,
) -> (
    Option<std::time::SystemTime>,
    Rc<Owners>,
    Arc<crate::read::dispatch::WorkerDirectory>,
    ObjectVersion,
) {
    remote_warms(shares, refuse_hold, fail_fill, 1)
}

/// Run `heads` sequential warm HEADs, each with a fresh request ID, from
/// node(0) against scripted remote owners. Returns the last HEAD's retention.
fn remote_warms(
    shares: &[u32],
    refuse_hold: Option<usize>,
    fail_fill: bool,
    heads: u8,
) -> (
    Option<std::time::SystemTime>,
    Rc<Owners>,
    Arc<crate::read::dispatch::WorkerDirectory>,
    ObjectVersion,
) {
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(shares.len());
    let mut f = fixture_with(8 * PAGE_BYTES, None);
    f.reactor.init().unwrap();
    let membership = weighted(1, shares);
    let owners = Rc::new(Owners {
        sender: Forwarding::new(signers[0].clone()),
        receivers: signers[1..]
            .iter()
            .map(|s| (s.node().clone(), Forwarding::new(s.clone())))
            .collect(),
        refuse_hold: refuse_hold.map(node),
        fail_fill,
        holds: RefCell::new(Vec::new()),
        fills: Cell::new(0),
        releases: RefCell::new(Vec::new()),
        leases: RefCell::new(Vec::new()),
    });
    let requester = Requester::scripted(
        owners.clone(),
        Owners::available,
        Owners::request,
        Owners::direct,
    );
    let (local, mut endpoint, _publication) = coordinator(&f, &signers[0], &membership, requester);
    let directory = f.fill.dependencies.metadata_owner.clone();
    let mut response = None;
    for n in 0..heads {
        let scope = RequestScope::new(RequestId([0xa0 + n; 16]), f.scope.deadline.0).unwrap();
        let mut head = local.read(
            ClientRequest {
                kind: ReadKind::HeadPinned {
                    etag: f.page.version.etag.clone(),
                },
                retain: Some(Duration::from_secs(60)),
                origin: OriginContext {
                    object: f.context.object.clone(),
                    metadata: None,
                    authorization: None,
                },
            },
            &scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        response = None;
        for _ in 0..4096 {
            if response.is_none()
                && let Poll::Ready(result) = head.as_mut().poll(&mut cx)
            {
                response = Some(result.unwrap());
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
            if response.is_some() && directory.warm.snapshot().0 == 0 && queue.pending() == 0 {
                break;
            }
            if response.is_some() && !fail_fill && refuse_hold.is_none() && queue.pending() == 0 {
                break;
            }
        }
        drop(head);
        for _ in 0..64 {
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
    }
    assert_eq!(
        f.origin.calls.get(),
        0,
        "the requester never fills remote pages"
    );
    let version = f.page.version.clone();
    (
        response.expect("HEAD completes").retain_until,
        owners,
        directory,
        version,
    )
}

#[test]
fn warm_holds_only_on_remote_primary_owners_and_requester_keeps_none() {
    let (until, owners, directory, version) = remote_warm(&[1, 1_000_000], None, false);
    assert!(until.is_some());
    assert_eq!(*owners.holds.borrow(), vec![node(1)]);
    assert_eq!(owners.fills.get(), 8, "the owner fills every page it holds");
    assert!(owners.releases.borrow().is_empty());
    assert_eq!(directory.warm.lease_count(&version), 1, "local lease only");
    for number in 0..8 {
        assert!(!directory.warm.is_local_page(&PageId {
            version: version.clone(),
            number: PageNumber(number),
        }));
    }
}

#[test]
fn rewarming_a_completed_remote_warm_fills_under_the_preserved_lease() {
    let (until, owners, directory, version) = remote_warms(&[1, 1_000_000], None, false, 2);
    assert!(until.is_some_and(|until| until > std::time::SystemTime::UNIX_EPOCH));
    assert_eq!(*owners.holds.borrow(), vec![node(1), node(1)]);
    assert_eq!(
        owners.fills.get(),
        16,
        "every fill passes the owner's lease check on the first attempt"
    );
    assert!(owners.releases.borrow().is_empty(), "no retry exhaustion");
    assert_eq!(
        owners.leases.borrow().len(),
        1,
        "both HEADs hold one owner lease"
    );
    assert_eq!(directory.warm.lease_count(&version), 1, "one renewed lease");
}

#[test]
fn warm_refused_by_any_primary_releases_every_owner_and_reports_no_retention() {
    let (until, owners, directory, version) =
        remote_warm(&[1, 1_000_000, 1_000_000], Some(2), false);
    assert_eq!(until, None, "one refusing owner refuses the whole HEAD");
    let mut holds = owners.holds.borrow().clone();
    holds.sort();
    assert_eq!(holds, vec![node(1), node(2)], "pages span both owners");
    let mut releases = owners.releases.borrow().clone();
    releases.sort();
    assert_eq!(releases, vec![node(1), node(2)]);
    assert_eq!(owners.fills.get(), 0);
    assert_eq!(directory.warm.lease_count(&version), 0);
    assert_eq!(directory.warm.snapshot().0, 0);
}

#[test]
fn warm_owner_fill_failure_fails_job_and_releases_remote_hold() {
    let (until, owners, directory, version) = remote_warm(&[1, 1_000_000], None, true);
    assert!(until.is_some(), "HEAD returns before any page is filled");
    assert_eq!(
        owners.fills.get(),
        3,
        "transient refusals retry within the attempt bound, then fail"
    );
    assert_eq!(*owners.releases.borrow(), vec![node(1)]);
    assert_eq!(directory.warm.lease_count(&version), 0);
    assert_eq!(directory.warm.snapshot().0, 0);
}

#[test]
fn owner_holds_only_primary_pages_and_shares_capacity_across_leases() {
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(2);
    let mut f = fixture_with(PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let stale = weighted(1, &[1, 1_000_000]);
    let primary = weighted(2, &[1_000_000, 1]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &primary, NoPeer::requester());
    let directory = f.fill.dependencies.metadata_owner.clone();
    let sender = Forwarding::new(signers[1].clone());
    let receiver = Forwarding::new(signers[0].clone());
    let context = OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let version = f.page.version.clone();
    let until = racer_platform::environment::wall_now() + Duration::from_secs(60);
    let mut id = 0u8;
    let mut serve = |lease: u8, action: WarmAction, membership: &Arc<Membership>| {
        id += 1;
        let scope = RequestScope::new(RequestId([id; 16]), f.scope.deadline.0).unwrap();
        let attempt = crate::model::AttemptId([id; 16]);
        let request = PeerRequest {
            operation: PeerOperation::Warm {
                version: version.clone(),
                lease: [lease; 16],
                action,
            },
            origin: local.credentials.seal(&context, attempt, &scope).unwrap(),
            route: crate::topology::RouteBudget {
                membership: membership.version,
                request: scope.request,
                attempt,
                destination: node(0),
                visited: vec![node(1)],
                remaining_links: 4,
                remaining_attempts: 4,
                deadline: scope.deadline,
            },
        };
        let (signed, _) = sender.sign_request(request).unwrap();
        let verified = receiver.verify_request(signed).unwrap();
        let mut response = local.serve_peer(verified, membership.clone(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..4096 {
            if let Poll::Ready(result) = response.as_mut().poll(&mut cx) {
                drop(response);
                for _ in 0..8 {
                    pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
                }
                return result.unwrap();
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        panic!("warm request did not complete");
    };
    let hold = WarmAction::Hold { pages: 2, until };
    let index = f.fill.dependencies.writer.index().clone();
    assert!(matches!(
        serve(1, hold.clone(), &stale),
        PeerResponse::StaleMembership
    ));
    assert_eq!(directory.warm.lease_count(&version), 0);
    assert_eq!(index.hold_expiry(&version), None);
    let PeerResponse::Held { until: held } = serve(1, hold.clone(), &primary) else {
        panic!("primary owner refused hold");
    };
    assert!(held <= until && held + Duration::from_millis(1) > until);
    assert!(matches!(
        serve(2, hold, &primary),
        PeerResponse::Held { .. }
    ));
    assert_eq!(directory.warm.lease_count(&version), 2);
    assert!(index.hold_expiry(&version).is_some());
    let fill = |length: u64, content_type: Option<crate::model::ContentType>| WarmAction::Fill {
        page: PageNumber(0),
        length,
        content_type,
    };
    assert!(
        matches!(
            serve(3, fill(PAGE_BYTES + 7, None), &primary),
            PeerResponse::Unavailable
        ),
        "a fill needs a live lease"
    );
    for mismatch in [
        fill(PAGE_BYTES + 8, None),
        fill(
            PAGE_BYTES + 7,
            Some(crate::model::ContentType::parse(b"text/plain").unwrap()),
        ),
    ] {
        assert!(
            matches!(
                serve(1, mismatch.clone(), &primary),
                PeerResponse::VersionUnavailable
            ),
            "acknowledged incompatible metadata {mismatch:?}"
        );
        assert_eq!(
            index.held_page_expiry(&f.page),
            None,
            "an incompatible copy is never published"
        );
    }
    let before = f.origin.calls.get();
    assert!(matches!(
        serve(1, fill(PAGE_BYTES + 7, None), &primary),
        PeerResponse::Warmed
    ));
    assert!(
        f.origin.calls.get() <= before + 1,
        "the owner fills from origin"
    );
    assert!(index.held_page_expiry(&f.page).is_some());
    assert!(matches!(
        serve(1, WarmAction::Release, &primary),
        PeerResponse::Released
    ));
    assert_eq!(directory.warm.lease_count(&version), 1);
    assert!(
        directory.warm.drained(WorkerId(0)),
        "another live lease keeps unused capacity"
    );
    assert!(index.hold_expiry(&version).is_some());
    assert!(matches!(
        serve(2, WarmAction::Release, &primary),
        PeerResponse::Released
    ));
    assert_eq!(directory.warm.lease_count(&version), 0);
    assert!(
        index.held_page_expiry(&f.page).is_some(),
        "release frees unused capacity but keeps published pages until expiry"
    );
    for _ in 0..8 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!(directory.warm.drained(WorkerId(0)));
    endpoint.uninstall().unwrap();
}

#[test]
fn worker_hold_covers_only_primary_pages_beside_cached_nonprimary_page() {
    let queue = Rc::new(racer_platform::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    let mut f = fixture_with(PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = weighted(1, &[1]);
    let (_local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let directory = f.fill.dependencies.metadata_owner.clone();
    let index = f.fill.dependencies.writer.index().clone();
    let version = f.page.version.clone();
    let page = |number| PageId {
        version: version.clone(),
        number: PageNumber(number),
    };
    let worker = directory.page_owner(&page(1)).unwrap();
    assert_eq!(directory.page_owner(&page(0)).unwrap(), worker);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    macro_rules! drive {
        ($future:expr) => {{
            let mut future = Box::pin($future);
            let mut output = None;
            for _ in 0..4096 {
                if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                    output = Some(result);
                    break;
                }
                pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
                f.reactor.wait(Duration::from_millis(1)).unwrap();
            }
            output.expect("request did not complete")
        }};
    }
    macro_rules! persist {
        ($page:expr) => {{
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
            let result = drive!(f.fill.acquire(
                $page,
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget
            ))
            .unwrap();
            let dirty = f
                .fill
                .dependencies
                .admission
                .reserve(
                    Some(&version.object.cache),
                    ResourceClass::DirtyCiphertext,
                    result.ciphertext.bytes().len(),
                )
                .unwrap();
            f.fill
                .dependencies
                .writer
                .enqueue(result.copy(), dirty)
                .unwrap();
            drive!(f.fill.dependencies.writer.progress(1, &f.scope)).unwrap();
        }};
    }
    // Page 0 is cached without any hold, as an ordinary read would leave it.
    persist!(page(0));
    assert!(index.lookup(&page(0)).unwrap().is_some());
    // Leave room for exactly one more page, so a hold consumed by page 0 would
    // leave no slot for page 1.
    index.set_page_capacity(2).unwrap();
    let until = racer_platform::environment::wall_now() + Duration::from_secs(60);
    let held = std::collections::BTreeMap::from([(
        worker.0,
        [PageNumber(1)]
            .into_iter()
            .collect::<crate::store::holds::HoldPages>(),
    )]);
    let mut fresh = Vec::new();
    drive!(directory.reserve_warm(&version, &held, until, &f.scope, &mut fresh)).unwrap();
    assert!(
        !index.is_held(&page(0)),
        "nonprimary cached page is not pinned"
    );
    assert!(index.held_page_expiry(&page(0)).is_none());
    assert!(index.is_held(&page(1)));
    assert!(index.held_page_expiry(&page(1)).is_none());
    persist!(page(1));
    assert!(
        index.held_page_expiry(&page(1)).is_some(),
        "the primary page publishes into its own held slot"
    );
    assert!(index.held_page_expiry(&page(0)).is_none());
    endpoint.uninstall().unwrap();
}
