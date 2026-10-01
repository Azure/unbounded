//! Metadata refresh, bootstrap publication, and independently timed callers.
use super::*;
use crate::read::{candidates::OriginAuthority, drivers};

pub(super) struct GatedMetadataOrigin {
    pub(super) buffers: BufferPool,
    pub(super) receive: RefCell<Option<futures::channel::oneshot::Receiver<MetadataReply>>>,
    pub(super) calls: Cell<usize>,
}

#[test]
fn metadata_cohorts_refresh_zero_ttl_and_do_not_negative_cache_adapter_failure() {
    use crate::{
        model::MetadataSelector,
        read::metadata::{MetadataDependencies, MetadataService},
        test_support::origin::{AdapterOrigin, RequestKind},
    };
    for failure in [None, Some(502)] {
        let clock = crate::runtime::environment::SimulationClock::new_at(
            71,
            Instant::now(),
            std::time::SystemTime::now(),
        );
        let environment = clock.environment(1);
        let _environment = environment.enter();
        let queue = Rc::new(drivers::DriverQueue::default());
        let _owner = queue.enter();
        let mut f = fixture();
        let adapter = AdapterOrigin::new("fixture", f.origin.metadata.clone());
        adapter.block(RequestKind::Head);
        if let Some(status) = failure {
            adapter.reject_next(RequestKind::Head, status);
        }
        let service = MetadataService::new(
            f.fill.dependencies.candidates.clone(),
            adapter_client(&f, &adapter),
            f.fill.dependencies.credentials.clone(),
            1,
            MetadataDependencies {
                index: Rc::new(Index::new(
                    WorkerId(0),
                    8,
                    crate::control::state::for_caches(
                        f.keys.clone(),
                        vec![f.context.object.cache.clone()],
                    ),
                )),
                fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
                owners: f.fill.dependencies.metadata_owner.clone(),
            },
        );
        let mut first = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let mut second = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(second.as_mut().poll(&mut cx).is_pending());
        adapter.release(RequestKind::Head);
        let (a, b) = drive_io(
            async { futures::join!(first.as_mut(), second.as_mut()) },
            &f.reactor,
            &mut f.engine,
            &f.crypto,
        );
        if failure.is_some() {
            assert_eq!(a, Err(Error::BadGateway));
            assert_eq!(b, Err(Error::BadGateway));
        } else {
            assert_eq!(a.unwrap(), f.origin.metadata);
            assert_eq!(b.unwrap(), f.origin.metadata);
        }
        assert_eq!(
            adapter.count(RequestKind::Head),
            1,
            "registered cohort shares one HEAD"
        );
        // Protocol failures back off the real client's transport circuit. Advancing
        // its clock lets this assertion distinguish backoff from negative caching.
        clock.advance(Duration::from_secs(2));
        // A new zero-TTL cohort must perform I/O even while completed futures live.
        adapter.block(RequestKind::Head);
        let mut next = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let pending = next.as_mut().poll(&mut cx);
        assert!(
            pending.is_pending(),
            "failure={failure:?}, result={pending:?}"
        );
        drop((first, second));
        let follower = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        adapter.release(RequestKind::Head);
        let (a, b) = drive_io(
            async { futures::join!(next, follower) },
            &f.reactor,
            &mut f.engine,
            &f.crypto,
        );
        assert_eq!(a.unwrap(), f.origin.metadata);
        assert_eq!(b.unwrap(), f.origin.metadata);
        assert_eq!(
            adapter.count(RequestKind::Head),
            2,
            "stale detach preserves new cohort; failures are not cached"
        );
    }
}

#[test]
fn bootstrap_rejection_re_elects_and_version_changes_never_mix_pages() {
    let queue = Rc::new(drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::read::metadata::{MetadataDependencies, MetadataService};
    let mut f = fixture();
    use crate::test_support::origin::{AdapterOrigin, RequestKind};
    let adapter = AdapterOrigin::new("fixture", f.origin.metadata.clone());
    adapter.set_body(vec![1; 3]);
    adapter.reject_next(RequestKind::InitialGet, 403);
    let origin = adapter_client(&f, &adapter);
    let service = MetadataService::new(
        f.fill.dependencies.candidates.clone(),
        origin.clone(),
        f.fill.dependencies.credentials.clone(),
        8,
        MetadataDependencies {
            index: Rc::new(Index::new(
                WorkerId(0),
                8,
                crate::control::state::for_caches(
                    f.keys.clone(),
                    vec![f.context.object.cache.clone()],
                ),
            )),
            fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
            owners: f.fill.dependencies.metadata_owner.clone(),
        },
    );
    let second_scope = RequestScope::new(RequestId([93; 16]), f.scope.deadline.0).unwrap();
    let second_context = OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let (a, b) = drive_io(
        async {
            let mut first_budget = AcquisitionBudget::new(f.scope.deadline.0, 32, 96);
            let mut second_budget = AcquisitionBudget::new(f.scope.deadline.0, 32, 96);
            futures::join!(
                service.bootstrap_peer(
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                    &mut first_budget,
                ),
                service.bootstrap_peer(
                    f.membership.clone(),
                    &second_context,
                    &second_scope,
                    &mut second_budget,
                )
            )
        },
        &f.reactor,
        &mut f.engine,
        &f.crypto,
    );
    assert!(matches!(a, Err(Error::OriginForbidden)));
    let PeerResponse::Bootstrap {
        metadata: first_metadata,
        page_zero: Some(first),
    } = b.unwrap()
    else {
        panic!("page")
    };
    assert_eq!(first.envelope().page.version, first_metadata.version);
    let first_bytes = f
        .fill
        .dependencies
        .memory
        .get(&first.envelope().page)
        .unwrap()
        .unwrap();
    assert_eq!(first_bytes.plaintext.bytes(), &[1; 3]);
    assert_eq!(adapter.count(RequestKind::InitialGet), 2);
    let mut metadata = f.origin.metadata.clone();
    metadata.version.etag = StrongEtag::test_value("v2");
    adapter.set_version(metadata);
    adapter.set_body(vec![2; 3]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 32, 96);
    let result = drive_io(
        service.bootstrap_peer(
            f.membership.clone(),
            &second_context,
            &second_scope,
            &mut budget,
        ),
        &f.reactor,
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    let PeerResponse::Bootstrap {
        metadata: second_metadata,
        page_zero: Some(second),
    } = result
    else {
        panic!("page")
    };
    assert_eq!(second.envelope().page.version, second_metadata.version);
    let second_bytes = f
        .fill
        .dependencies
        .memory
        .get(&second.envelope().page)
        .unwrap()
        .unwrap();
    assert_eq!(second_bytes.plaintext.bytes(), &[2; 3]);
    assert_eq!(second_metadata.version.etag, StrongEtag::test_value("v2"));
    assert_eq!(first_bytes.plaintext.bytes(), &[1; 3]);
    assert_eq!(first_metadata.version.etag, StrongEtag::test_value("v1"));
    assert_eq!(
        adapter.count(RequestKind::InitialGet),
        3,
        "rejection was not negative-cached"
    );
    assert_eq!(
        adapter.count(RequestKind::Head),
        0,
        "bootstrap must not HEAD"
    );
    assert_eq!(
        adapter.count(RequestKind::PinnedGet),
        0,
        "bootstrap must not refetch page"
    );
}
impl Origin for GatedMetadataOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async move {
            let mut reply = self
                .metadata(
                    authority,
                    context,
                    crate::model::MetadataSelector::Fresh,
                    scope,
                )
                .await?;
            if let Some(page) = reply.page_zero.take() {
                let bytes = page.plaintext.bytes()?.to_vec();
                drop(page.plaintext);
                let mut plaintext = self.buffers.plaintext(reservation, bytes.len())?;
                plaintext.bytes_mut()?.copy_from_slice(&bytes);
                reply.page_zero = Some(OriginPage {
                    metadata: page.metadata,
                    plaintext,
                });
            }
            Ok(reply)
        })
    }
    fn page_reserved<'a>(
        &'a self,
        _: &'a OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: Reservation,
        _: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async { panic!("metadata-only refresh") })
    }
    fn metadata<'a>(
        &'a self,
        _: &'a OriginAuthority,
        _: &'a OriginContext,
        _: crate::model::MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        self.calls.set(self.calls.get() + 1);
        let receive = self.receive.borrow_mut().take().unwrap();
        Box::pin(async move { receive.await.map_err(|_| Error::Unavailable) })
    }
}

#[test]
fn bootstrap_after_catalog_eviction_checks_cached_content_type_and_preserves_fresh_expiry() {
    let queue = Rc::new(drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::{
        model::ContentType,
        read::metadata::{MetadataDependencies, MetadataService},
    };
    for with_origin_page in [false, true] {
        for (cached_type, fresh_type, conflict) in [
            (Some("text/plain"), Some("text/html"), true),
            (Some("text/html"), Some("text/plain"), true),
            (Some("text/plain"), None, true),
            (None, Some("text/plain"), true),
            (Some("text/plain"), Some("text/plain"), false),
            (None, None, false),
        ] {
            let mut f = fixture();
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
            let mut cached = drive(
                f.fill.acquire(
                    f.page.clone(),
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                    &mut budget,
                ),
                &mut f.engine,
                &f.crypto,
            )
            .unwrap();
            // Retain real authenticated buffers with a historical descriptor in
            // memory, independently of the evictable page-zero catalog.
            cached.metadata.content_type =
                cached_type.map(|v| ContentType::parse(v.as_bytes()).unwrap());
            f.fill
                .dependencies
                .memory
                .remove_cache(&f.context.object.cache)
                .unwrap();
            f.fill.dependencies.memory.publish(cached.clone()).unwrap();
            let index = Rc::new(Index::new(
                WorkerId(0),
                4,
                crate::control::state::for_caches(
                    f.keys.clone(),
                    vec![f.context.object.cache.clone()],
                ),
            ));
            index.publish_version(cached.metadata.immutable()).unwrap();
            assert_eq!(index.evict_metadata(1).unwrap(), 1);
            assert!(index.version(&f.page.version).unwrap().is_none());
            assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_some());

            let mut fresh = cached.metadata.clone();
            fresh.content_type = fresh_type.map(|v| ContentType::parse(v.as_bytes()).unwrap());
            fresh.expires_at = ExpiresAt::from_unix_millis(1_900_000_000_000).unwrap();
            let page_zero = if with_origin_page {
                let reservation = f
                    .fill
                    .dependencies
                    .admission
                    .reserve(Some(&f.context.object.cache), ResourceClass::Plaintext, 3)
                    .unwrap();
                let mut plaintext = f.origin.buffers.plaintext(reservation, 3).unwrap();
                plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
                Some(OriginPage {
                    metadata: fresh.clone(),
                    plaintext,
                })
            } else {
                None
            };
            let (send, receive) = futures::channel::oneshot::channel();
            let origin = Rc::new(GatedMetadataOrigin {
                buffers: f.origin.buffers.clone(),
                receive: RefCell::new(Some(receive)),
                calls: Cell::new(0),
            });
            let service = MetadataService::new(
                f.fill.dependencies.candidates.clone(),
                origin.clone(),
                f.fill.dependencies.credentials.clone(),
                4,
                MetadataDependencies {
                    index: index.clone(),
                    owners: f.fill.dependencies.metadata_owner.clone(),
                    fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
                },
            );
            send.send(MetadataReply {
                metadata: fresh.clone(),
                page_zero,
            })
            .ok()
            .unwrap();
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 32, 96);
            let result = drive(
                service.bootstrap_peer(f.membership.clone(), &f.context, &f.scope, &mut budget),
                &mut f.engine,
                &f.crypto,
            );
            if conflict {
                assert!(
                    matches!(result, Err(Error::CorruptRecord)),
                    "{cached_type:?} -> {fresh_type:?}, origin page={with_origin_page}"
                );
                if with_origin_page {
                    assert!(
                        index.version(&f.page.version).unwrap().is_none(),
                        "conflicting bootstrap must fail before catalog publication"
                    );
                }
            } else {
                let PeerResponse::Bootstrap {
                    metadata,
                    page_zero: Some(ciphertext),
                } = result.unwrap()
                else {
                    panic!("nonempty bootstrap")
                };
                let expected = fresh.content_type.clone();
                assert_eq!(metadata.content_type, expected);
                assert_eq!(metadata.expires_at, fresh.expires_at);
                assert_eq!(ciphertext.envelope().page.version, metadata.version);
                assert_eq!(cached.plaintext.bytes(), b"abc");
                if with_origin_page {
                    assert_eq!(
                        index
                            .version(&f.page.version)
                            .unwrap()
                            .unwrap()
                            .content_type,
                        expected
                    );
                }
            }
            assert_eq!(origin.calls.get(), 1);
            assert_eq!(
                f.origin.calls.get(),
                1,
                "cached page must not be reacquired"
            );
            assert_eq!(
                f.fill
                    .dependencies
                    .memory
                    .get(&f.page)
                    .unwrap()
                    .unwrap()
                    .metadata,
                cached.metadata,
                "refresh must not rewrite historical page metadata"
            );
            assert_eq!(drivers::pending(), 0);
        }
    }
}

#[test]
fn blocked_metadata_leader_and_follower_notify_without_spinning() {
    let queue = Rc::new(drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::{
        model::MetadataSelector,
        read::metadata::{MetadataDependencies, MetadataService},
        test_support::WakeCounter,
    };
    use std::task::Waker;
    for cancel_leader in [false, true] {
        let f = fixture();
        let (send, receive) = futures::channel::oneshot::channel();
        let service = MetadataService::new(
            f.fill.dependencies.candidates.clone(),
            Rc::new(GatedMetadataOrigin {
                buffers: f.origin.buffers.clone(),
                receive: RefCell::new(Some(receive)),
                calls: Cell::new(0),
            }),
            f.fill.dependencies.credentials.clone(),
            4,
            MetadataDependencies {
                index: Rc::new(Index::new(
                    WorkerId(0),
                    4,
                    crate::control::state::for_caches(
                        f.keys.clone(),
                        vec![f.context.object.cache.clone()],
                    ),
                )),
                owners: f.fill.dependencies.metadata_owner.clone(),
                fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
            },
        );
        let follower_scope = RequestScope::new(RequestId([2; 16]), f.scope.deadline.0).unwrap();
        let mut leader = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let mut follower = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &follower_scope,
        );
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(leader.as_mut().poll(&mut cx).is_pending());
        assert!(follower.as_mut().poll(&mut cx).is_pending());
        let settled = count.count();
        for _ in 0..4 {
            assert!(leader.as_mut().poll(&mut cx).is_pending());
            assert!(follower.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(
            count.count(),
            settled,
            "external metadata waits must not self-wake"
        );
        let canceled = if cancel_leader {
            &f.scope
        } else {
            &follower_scope
        };
        canceled.cancel().unwrap();
        assert!(
            count.count() > settled,
            "cancellation registration survives polling"
        );
        if cancel_leader {
            assert!(matches!(
                leader.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
        } else {
            assert!(matches!(
                follower.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
        }
        // Parent cancellation now wakes the independent driver too. Consume
        // that wake before checking the separate origin-completion notification.
        drivers::poll(&mut cx, 64);
        let before = count.count();
        send.send(MetadataReply {
            metadata: f.origin.metadata.clone(),
            page_zero: None,
        })
        .ok()
        .unwrap();
        assert!(
            count.count() > before,
            "origin completion reaches registered driver"
        );
        if cancel_leader {
            // Finish the retained driver after caller cancellation without electing
            // a replacement supplier in this test.
            drop(follower);
            drivers::poll(&mut cx, 64);
        } else {
            assert!(matches!(leader.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        }
        assert_eq!(drivers::pending(), 0);
    }
}

#[test]
fn metadata_deadline_wakes_parked_follower_without_polling_gated_leader() {
    let queue = Rc::new(drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::{
        model::MetadataSelector,
        read::metadata::{MetadataDependencies, MetadataService, tests::assert_ingress_counts},
        test_support::WakeCounter,
    };
    use futures::{Stream, stream::FuturesUnordered};
    use std::task::Waker;

    for (budget_earlier, expire_leader) in [(false, false), (true, false), (false, true)] {
        let f = fixture();
        let admission = &f.fill.dependencies.admission;
        let baseline = admission.used(ResourceClass::RequestContext);
        let (send, receive) = futures::channel::oneshot::channel();
        let origin = Rc::new(GatedMetadataOrigin {
            buffers: f.origin.buffers.clone(),
            receive: RefCell::new(Some(receive)),
            calls: Cell::new(0),
        });
        let service = MetadataService::new(
            f.fill.dependencies.candidates.clone(),
            origin.clone(),
            f.fill.dependencies.credentials.clone(),
            1,
            MetadataDependencies {
                index: Rc::new(Index::new(
                    WorkerId(0),
                    4,
                    crate::control::state::for_caches(
                        f.keys.clone(),
                        vec![f.context.object.cache.clone()],
                    ),
                )),
                owners: f.fill.dependencies.metadata_owner.clone(),
                fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
            },
        );
        let due = Instant::now() + Duration::from_secs(60);
        let follower_scope = RequestScope::new(
            RequestId([2; 16]),
            if budget_earlier {
                f.scope.deadline.0
            } else {
                due
            },
        )
        .unwrap();
        let mut budget = AcquisitionBudget::new(
            if budget_earlier {
                due
            } else {
                f.scope.deadline.0
            },
            4,
            8,
        );
        let leader_polls = Cell::new(0);
        let follower_polls = Cell::new(0);
        let mut leader = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let mut follower = service.resolve_with_budget(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &follower_scope,
            &mut budget,
        );
        let mut stream = FuturesUnordered::<
            futures::future::LocalBoxFuture<'_, (bool, Result<ObjectMetadata>)>,
        >::new();
        stream.push(Box::pin(std::future::poll_fn(|cx| {
            leader_polls.set(leader_polls.get() + 1);
            leader.as_mut().poll(cx).map(|result| (true, result))
        })));
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        stream.push(Box::pin(std::future::poll_fn(|cx| {
            follower_polls.set(follower_polls.get() + 1);
            follower.as_mut().poll(cx).map(|result| (false, result))
        })));
        // Drain initial scheduling notifications, then only poll the parent stream.
        for _ in 0..4 {
            assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        }
        assert_eq!(origin.calls.get(), 1);
        assert_ingress_counts(&service, 2, 2);
        let retained = admission.used(ResourceClass::RequestContext);
        assert!(retained > baseline);
        let parked = (leader_polls.get(), follower_polls.get(), count.count());
        for _ in 0..4 {
            assert_eq!(service.poll_deadlines(due - Duration::from_nanos(1), 64), 0);
            assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        }
        assert_eq!(
            (leader_polls.get(), follower_polls.get(), count.count()),
            parked
        );
        assert_eq!(service.poll_deadlines(due, 0), 0);
        assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        assert_eq!(service.poll_deadlines(due, 1), 1);
        assert!(matches!(
            Pin::new(&mut stream).poll_next(&mut cx),
            Poll::Ready(Some((false, Err(Error::DeadlineExceeded))))
        ));
        assert_eq!(leader_polls.get(), parked.0);
        assert_eq!(follower_polls.get(), parked.1 + 1);
        assert_eq!(origin.calls.get(), 1);
        assert_eq!(drivers::pending(), 1);
        assert_ingress_counts(&service, 1, 1);
        assert_eq!(admission.used(ResourceClass::RequestContext), retained);
        let after_expiry = count.count();
        assert_eq!(service.poll_deadlines(due, 64), 0);
        assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        assert_eq!(count.count(), after_expiry);
        if expire_leader {
            assert_eq!(service.poll_deadlines(f.scope.deadline.0, 1), 1);
            assert!(matches!(
                Pin::new(&mut stream).poll_next(&mut cx),
                Poll::Ready(Some((true, Err(Error::DeadlineExceeded))))
            ));
            // Ingress timeout detaches only its guard. The driver still owns the
            // refresh registration, original budget, and charged origin context.
            assert_ingress_counts(&service, 0, 1);
            assert_eq!(drivers::pending(), 1);
            assert_eq!(admission.used(ResourceClass::RequestContext), retained);
            assert_eq!(origin.calls.get(), 1);
        }
        send.send(MetadataReply {
            metadata: f.origin.metadata.clone(),
            page_zero: None,
        })
        .ok()
        .unwrap();
        drivers::poll(&mut cx, 64);
        if !expire_leader {
            assert!(matches!(Pin::new(&mut stream).poll_next(&mut cx),
                Poll::Ready(Some((true, Ok(metadata)))) if metadata == f.origin.metadata));
        }
        assert!(matches!(
            Pin::new(&mut stream).poll_next(&mut cx),
            Poll::Ready(None)
        ));
        assert_eq!(service.poll_deadlines(f.scope.deadline.0, 64), 0);
        assert_eq!(drivers::pending(), 0);
        assert_ingress_counts(&service, 0, 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(origin.calls.get(), 1);
    }
}
