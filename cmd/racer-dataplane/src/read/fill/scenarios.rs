use super::*;
use crate::peer::PeerClient;
mod hot_reads;
mod metadata {
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
            test_support::origin::{AdapterOrigin, RequestKind},
        };
        for failure in [None, Some(502)] {
            let clock = uring_runtime::environment::SimulationClock::new_at(
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
            let service =
                f.metadata_service_with_index(adapter_client(&f, &adapter), 1, f.metadata_index(8));
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
        let mut f = fixture();
        use crate::test_support::origin::{AdapterOrigin, RequestKind};
        let adapter = AdapterOrigin::new("fixture", f.origin.metadata.clone());
        adapter.set_body(vec![1; 3]);
        adapter.reject_next(RequestKind::InitialGet, 403);
        let origin = adapter_client(&f, &adapter);
        let service = f.metadata_service_with_index(origin, 8, f.metadata_index(8));
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
                        &mut first_budget
                    ),
                    service.bootstrap_peer(
                        f.membership.clone(),
                        &second_context,
                        &second_scope,
                        &mut second_budget
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
            reservation: flow_control::Charge<AdmissionPolicy>,
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
            _: flow_control::Charge<AdmissionPolicy>,
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
        use crate::model::ContentType;
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
                let mut cached = acquire(&mut f, &mut budget).unwrap();
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
                let index = f.metadata_index(4);
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
                let service = f.metadata_service_with_index(origin.clone(), 4, index.clone());
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
        use crate::{model::MetadataSelector, test_support::WakeCounter};
        use std::task::Waker;
        for cancel_leader in [false, true] {
            let f = fixture();
            let (send, receive) = futures::channel::oneshot::channel();
            let service = f.metadata_service(
                Rc::new(GatedMetadataOrigin {
                    buffers: f.origin.buffers.clone(),
                    receive: RefCell::new(Some(receive)),
                    calls: Cell::new(0),
                }),
                4,
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
            model::MetadataSelector, read::metadata::tests::assert_ingress_counts,
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
            let service = f.metadata_service(origin.clone(), 1);
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
                assert!(
                    matches!(Pin::new(&mut stream).poll_next(&mut cx), Poll::Ready(Some((true, Ok(metadata)))) if metadata == f.origin.metadata)
                );
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
}
mod peer_copies;
mod pressure {
    //! Reclamation scenarios share the acquisition fixture and real crypto pipeline.
    use super::*;

    #[test]
    fn sequential_full_pages_reclaim_idle_bytes_and_preserve_busy_reader_leases() {
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _owner = queue.enter();
        for (pages, ciphertext_pages, dirty_pages, hold_bundle) in
            [(8, 4, 2, false), (8, 3, 1, false), (12, 3, 2, true)]
        {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.plaintext_bytes = std::num::NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
            limits.ciphertext_bytes =
                std::num::NonZeroUsize::new(ciphertext_pages * (PAGE_BYTES as usize + 16)).unwrap();
            limits.dirty_bytes =
                std::num::NonZeroUsize::new(dirty_pages * (PAGE_BYTES as usize + 16)).unwrap();
            // Byte pressure must occur before entry-count eviction in every case.
            limits.metadata_entries = std::num::NonZeroUsize::new(128).unwrap();
            let mut f = fixture_with(pages * PAGE_BYTES, Some(limits.clone()));
            let mut pinned = None;
            let mut bundle = None;
            for number in 0..pages {
                let page = PageId {
                    version: f.page.version.clone(),
                    number: PageNumber(number),
                };
                let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
                let result = drive(
                    f.fill.acquire(
                        page,
                        f.membership.clone(),
                        &f.context,
                        &f.scope,
                        &mut budget,
                    ),
                    &mut f.engine,
                    &f.crypto,
                )
                .unwrap();
                assert_eq!(result.plaintext.bytes().len(), PAGE_BYTES as usize);
                assert!(
                    result
                        .plaintext
                        .bytes()
                        .iter()
                        .all(|byte| *byte == number as u8)
                );
                if number == 0 {
                    pinned = Some(result.plaintext.clone());
                    if hold_bundle {
                        bundle = Some(result.clone());
                    }
                }
                assert_eq!(pinned.as_ref().unwrap().bytes()[0], 0);
                drop(result);
                f.fill.dependencies.flights.poll_budgeted(64).unwrap();
                let admission = &f.fill.dependencies.admission;
                for class in [
                    ResourceClass::Plaintext,
                    ResourceClass::Ciphertext,
                    ResourceClass::DirtyCiphertext,
                ] {
                    assert!(admission.used(class) <= admission.limit(class));
                }
            }
            assert_eq!(
                f.origin.calls.get(),
                pages as usize,
                "byte capacity must not permanently block idle-cache misses"
            );
            assert!(
                f.fill.dependencies.memory.get(&f.page).unwrap().is_some(),
                "independent reader protected its cached page"
            );
            drop((pinned, bundle));
            f.fill.dependencies.writer.discard_unsubmitted();
            f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
            for class in [
                ResourceClass::Plaintext,
                ResourceClass::Ciphertext,
                ResourceClass::DirtyCiphertext,
            ] {
                assert_eq!(f.fill.dependencies.admission.used(class), 0);
            }
        }
    }

    #[test]
    fn bootstrap_admission_discards_queued_copy_before_evicting_idle_bundle() {
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _owner = queue.enter();
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = std::num::NonZeroUsize::new(PAGE_BYTES as usize).unwrap();
        let mut f = fixture_with(3, Some(limits));
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let result = drive(
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
        drop(result);
        let dependencies = &f.fill.dependencies;
        assert_eq!(dependencies.writer.queued_count(), 1);
        assert_eq!(dependencies.memory.evict_idle(usize::MAX), Ok(0));
        use crate::test_support::origin::{AdapterOrigin, RequestKind};
        let mut metadata = f.origin.metadata.clone();
        metadata.version.etag = StrongEtag::test_value("empty");
        metadata.length = 0;
        let adapter = AdapterOrigin::new("fixture", metadata.clone());
        let service =
            f.metadata_service_with_index(adapter_client(&f, &adapter), 8, f.metadata_index(8));
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let response = drive_io(
            service.bootstrap_peer(f.membership.clone(), &f.context, &f.scope, &mut budget),
            &f.reactor,
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        assert!(
            matches!(response, PeerResponse::Bootstrap { metadata: actual, page_zero: None } if actual == metadata)
        );
        assert_eq!(adapter.count(RequestKind::InitialGet), 1);
        assert_eq!(adapter.count(RequestKind::Head), 0);
        assert_eq!(dependencies.writer.discarded_count(), 1);
        assert_eq!(dependencies.writer.pending_count(), 0);
        assert!(dependencies.memory.get(&f.page).unwrap().is_none());
        assert_eq!(dependencies.admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(
            dependencies.admission.used(ResourceClass::DirtyCiphertext),
            0
        );
        // The empty reply releases the bootstrap plaintext reservation after I/O.
        assert_eq!(dependencies.admission.used(ResourceClass::Plaintext), 0);
    }

    fn retained_page(
        f: &Fixture,
        cache: &CacheId,
        version: &str,
        class: ResourceClass,
        amount: usize,
        queued: bool,
    ) -> PageId {
        let admission = &f.fill.dependencies.admission;
        let mut descriptor = f.origin.metadata.immutable();
        descriptor.version.object.cache = cache.clone();
        descriptor.version.etag = StrongEtag::test_value(version);
        let id = PageId {
            version: descriptor.version.clone(),
            number: PageNumber(0),
        };
        let mut page = PageResult {
            metadata: descriptor.for_pin(),
            plaintext: crate::memory::pool::VerifiedPage {
                inner: Arc::new(crate::memory::pool::VerifiedBytes {
                    page: id.clone(),
                    bytes: vec![1; 3],
                    reservation: admission
                        .reserve(Some(cache), ResourceClass::Plaintext, 3)
                        .unwrap(),
                }),
            },
            ciphertext: f
                .fill
                .dependencies
                .buffers
                .ciphertext(
                    admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, 19)
                        .unwrap(),
                    crate::model::PageEnvelope {
                        page: id,
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        nonce: crate::model::Nonce([2; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    vec![2; 19],
                )
                .unwrap(),
        };
        // Small final pages may retain full-page admission slack.
        let reservation = admission.reserve(Some(cache), class, amount).unwrap();
        match class {
            ResourceClass::Plaintext => {
                Arc::get_mut(&mut page.plaintext.inner).unwrap().reservation = reservation
            }
            ResourceClass::Ciphertext => {
                Arc::get_mut(&mut page.ciphertext.inner)
                    .unwrap()
                    .reservation = reservation
            }
            _ => panic!("page class required"),
        }
        let id = page.plaintext.page().clone();
        if queued {
            let dirty = admission
                .reserve(Some(cache), ResourceClass::DirtyCiphertext, 19)
                .unwrap();
            f.fill
                .dependencies
                .writer
                .enqueue(page.copy(), dirty)
                .unwrap();
        }
        f.fill.dependencies.memory.publish(page).unwrap();
        id
    }

    #[test]
    fn reclamation_uses_second_bounded_scan_without_revoking_readers_or_spinning() {
        for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
            for (busy_count, idle_tail) in [(256, true), (256, false), (512, true)] {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.metadata_entries = std::num::NonZeroUsize::new(1024).unwrap();
                let f = fixture_with(3, Some(limits));
                let cache = &f.context.object.cache;
                let deps = &f.fill.dependencies;
                let amount = if matches!(class, ResourceClass::Plaintext) {
                    3
                } else {
                    19
                };
                let mut readers = Vec::new();
                for n in 0..busy_count {
                    let id = retained_page(&f, cache, &format!("busy-{n}"), class, amount, false);
                    readers.push(deps.memory.get(&id).unwrap().unwrap());
                }
                let tail =
                    idle_tail.then(|| retained_page(&f, cache, "idle-tail", class, amount, false));
                let used = deps.admission.used(class);
                let _pressure = deps
                    .admission
                    .reserve(None, class, deps.admission.limit(class) - used)
                    .unwrap();
                let attempts = Cell::new(0);
                let result = f.fill.reserve_reclaiming(cache, class, amount, || {
                    attempts.set(attempts.get() + 1);
                    deps.admission
                        .reserve(Some(cache), class, amount)
                        .map_err(Into::into)
                });
                if busy_count == 256 && idle_tail {
                    assert!(
                        result.is_ok(),
                        "idle tail beyond first scan must admit: {class:?}, {:?}",
                        result.as_ref().err()
                    );
                    assert!(deps.memory.get(tail.as_ref().unwrap()).unwrap().is_none());
                } else {
                    assert!(matches!(result, Err(Error::Overloaded)));
                    if let Some(tail) = &tail {
                        assert!(
                            deps.memory.get(tail).unwrap().is_some(),
                            "must not scan beyond existing two-pass bound"
                        );
                    }
                }
                assert_eq!(
                    attempts.get(),
                    3,
                    "initial admission plus two bounded scans"
                );
                for reader in &readers {
                    assert_eq!(reader.plaintext.bytes(), &[1; 3]);
                    assert_eq!(reader.ciphertext.bytes(), &[2; 19]);
                    assert!(deps.memory.get(reader.plaintext.page()).unwrap().is_some());
                }
                assert_eq!(deps.writer.discarded_count(), 0);
            }
        }
    }

    #[test]
    fn fair_share_one_page_deficit_preserves_other_caches_and_remaining_working_set() {
        for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
            let amount = PAGE_BYTES as usize + 16;
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.plaintext_bytes = std::num::NonZeroUsize::new(6 * amount + 64).unwrap();
            limits.ciphertext_bytes = std::num::NonZeroUsize::new(6 * amount + 64).unwrap();
            let other_cache = CacheId("44444444-4444-4444-8444-444444444444".into());
            let f = fixture_with_caches(
                3,
                Some(limits),
                vec![
                    CacheId(crate::security::test_support::CACHE.into()),
                    other_cache.clone(),
                ],
            );
            let cache = &f.context.object.cache;
            let queued = matches!(class, ResourceClass::Plaintext);
            let other = retained_page(&f, &other_cache, "other", class, amount, queued);
            let first = retained_page(&f, cache, "first", class, amount, queued);
            let second = retained_page(&f, cache, "second", class, amount, queued);
            let third = retained_page(&f, cache, "third", class, amount, queued);
            let reservation = f
                .fill
                .reserve_with_reclamation(cache, class, amount)
                .unwrap();
            let deps = &f.fill.dependencies;
            assert!(deps.memory.get(&first).unwrap().is_none());
            for page in [&other, &second, &third] {
                assert!(deps.memory.get(page).unwrap().is_some());
                if queued {
                    assert!(deps.writer.copy_only(page).unwrap().is_some());
                }
            }
            assert_eq!(deps.writer.discarded_count(), u64::from(queued));
            drop(reservation);
        }
    }

    #[test]
    fn global_plaintext_deficit_counts_plaintext_not_combined_bundle_bytes() {
        let f = fixture();
        let cache = &f.context.object.cache;
        let first = retained_page(&f, cache, "first", ResourceClass::Plaintext, 3, false);
        let second = retained_page(&f, cache, "second", ResourceClass::Plaintext, 3, false);
        let third = retained_page(&f, cache, "third", ResourceClass::Plaintext, 3, false);
        let admission = &f.fill.dependencies.admission;
        let _pressure = admission
            .reserve(
                None,
                ResourceClass::Plaintext,
                admission.limit(ResourceClass::Plaintext) - 9,
            )
            .unwrap();
        let _reservation = f
            .fill
            .reserve_with_reclamation(cache, ResourceClass::Plaintext, 6)
            .unwrap();
        assert!(f.fill.dependencies.memory.get(&first).unwrap().is_none());
        assert!(f.fill.dependencies.memory.get(&second).unwrap().is_none());
        assert!(f.fill.dependencies.memory.get(&third).unwrap().is_some());
        // A full-page deficit cannot be remedied by the remaining short idle page.
        assert!(matches!(
            f.fill.reserve_bootstrap(cache),
            Err(Error::Overloaded)
        ));
        for page in [&first, &second, &third] {
            assert!(f.fill.dependencies.memory.get(page).unwrap().is_none());
        }
    }

    #[test]
    fn dirty_only_pressure_skips_persistence_without_flushing_memory_or_queue() {
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _owner = queue.enter();
        let mut f = fixture();
        let cache = &f.context.object.cache;
        let page = retained_page(&f, cache, "queued", ResourceClass::Plaintext, 3, true);
        let deps = &f.fill.dependencies;
        let _pressure = deps
            .admission
            .reserve(
                None,
                ResourceClass::DirtyCiphertext,
                deps.admission.limit(ResourceClass::DirtyCiphertext) - 19,
            )
            .unwrap();
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let result = drive(
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
        assert_eq!(result.plaintext.bytes(), b"abc");
        assert_eq!(f.origin.calls.get(), 1);
        assert!(deps.writer.copy_only(&f.page).unwrap().is_none());
        assert!(deps.memory.get(&page).unwrap().is_some());
        assert!(deps.writer.copy_only(&page).unwrap().is_some());
        assert_eq!(deps.writer.discarded_count(), 0);
    }

    #[test]
    fn busy_leases_and_impossible_allocations_do_not_discard_queued_work() {
        let f = fixture();
        let cache = &f.context.object.cache;
        let first = retained_page(&f, cache, "first", ResourceClass::Plaintext, 3, true);
        let second = retained_page(&f, cache, "second", ResourceClass::Plaintext, 3, true);
        let deps = &f.fill.dependencies;
        let plaintext = deps.memory.get(&first).unwrap().unwrap().plaintext;
        let ciphertext = deps.memory.ciphertext(&second).unwrap().unwrap();
        let _pressure = deps
            .admission
            .reserve(
                None,
                ResourceClass::Plaintext,
                deps.admission.limit(ResourceClass::Plaintext) - 6,
            )
            .unwrap();
        assert!(matches!(
            f.fill.reserve_bootstrap(cache),
            Err(Error::Overloaded)
        ));
        assert_eq!(deps.writer.discarded_count(), 0);
        assert_eq!(deps.writer.pending_count(), 2);
        assert_eq!(plaintext.bytes(), &[1; 3]);
        assert_eq!(ciphertext.ciphertext.bytes(), &[2; 19]);
        drop((plaintext, ciphertext));
        assert!(matches!(
            f.fill
                .reserve_with_reclamation(cache, ResourceClass::Plaintext, usize::MAX),
            Err(Error::Overloaded)
        ));
        assert!(deps.memory.get(&first).unwrap().is_some());
        assert!(deps.memory.get(&second).unwrap().is_some());
        assert_eq!(deps.writer.discarded_count(), 0);
    }

    #[test]
    fn ciphertext_staging_release_stops_before_evicting_unpinned_memory_bundle() {
        let f = fixture();
        let cache = &f.context.object.cache;
        let first = retained_page(&f, cache, "first", ResourceClass::Plaintext, 3, true);
        let second = retained_page(&f, cache, "second", ResourceClass::Plaintext, 3, true);
        let deps = &f.fill.dependencies;
        let _pressure = deps
            .admission
            .reserve(
                None,
                ResourceClass::Ciphertext,
                deps.admission.limit(ResourceClass::Ciphertext)
                    - deps.admission.used(ResourceClass::Ciphertext),
            )
            .unwrap();
        let _reservation = f
            .fill
            .reserve_with_reclamation(cache, ResourceClass::Ciphertext, 1)
            .unwrap();
        assert_eq!(deps.writer.discarded_count(), 1);
        assert!(deps.writer.copy_only(&first).unwrap().is_none());
        assert!(deps.writer.copy_only(&second).unwrap().is_some());
        assert!(deps.memory.get(&first).unwrap().is_some());
        assert!(deps.memory.get(&second).unwrap().is_some());
    }
}
use crate::{
    model::{
        CacheId, CacheKey, ExpiresAt, ObjectId, ObjectVersion, PageNumber, RequestId,
        ResourceClass, StrongEtag, WorkerId,
    },
    origin::{MetadataReply, OriginPage},
    read::dispatch::WorkerDirectory,
    runtime::{
        crypto::{self, CryptoClient},
        reactor::Reactor,
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::aead::PageCryptoEngine,
    store::catalog::{Index, SegmentClock},
    topology::{
        membership::{Member, Membership},
        placement::Placement,
    },
};
use std::{
    cell::{Cell, RefCell},
    future::Future,
    num::NonZeroU32,
    pin::Pin,
    task::{Context, Poll},
    time::{Duration, Instant},
};

use crate::test_support::NoPeers as NoPeer;
struct TestOrigin {
    buffers: BufferPool,
    calls: Cell<usize>,
    metadata: ObjectMetadata,
    reject_once: Cell<bool>,
    version_unavailable: Cell<bool>,
    blocked_pages: RefCell<std::collections::BTreeSet<u64>>,
    started_pages: RefCell<Vec<u64>>,
}
impl Origin for TestOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: flow_control::Charge<AdmissionPolicy>,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async { panic!("pinned page fill must not bootstrap metadata") })
    }
    fn metadata<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: crate::model::MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async { panic!("pinned page fill must not refresh metadata") })
    }
    fn page_reserved<'a>(
        &'a self,
        authority: &'a super::super::candidates::OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, page.number)?;
            self.calls.set(self.calls.get() + 1);
            self.started_pages.borrow_mut().push(page.number.0);
            std::future::poll_fn(|cx| {
                if self.blocked_pages.borrow().contains(&page.number.0) {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
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
            if self.reject_once.replace(false) {
                return Err(Error::OriginForbidden);
            }
            if self.version_unavailable.get() {
                return Err(Error::VersionUnavailable);
            }
            let length = self.metadata.immutable().page_length(page)? as usize;
            let mut plaintext = self.buffers.plaintext(reservation, length)?;
            if length == 3 {
                plaintext.bytes_mut()?.copy_from_slice(b"abc");
            } else {
                plaintext.bytes_mut()?.fill(page.number.0 as u8);
            }
            Ok(OriginPage {
                metadata: self.metadata.clone(),
                plaintext,
            })
        })
    }
}

struct Fixture {
    fill: Fill,
    reactor: Rc<Reactor>,
    keys: Rc<racer_identity::Keyring>,
    origin: Rc<TestOrigin>,
    crypto: Rc<CryptoClient>,
    engine: PageCryptoEngine,
    context: OriginContext,
    membership: MembershipLease,
    page: PageId,
    scope: RequestScope,
    directory: std::path::PathBuf,
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}
impl Fixture {
    fn metadata_index(&self, capacity: usize) -> Rc<Index> {
        Rc::new(Index::new(
            WorkerId(0),
            capacity,
            crate::control::state::for_caches(
                self.keys.clone(),
                vec![self.context.object.cache.clone()],
            ),
        ))
    }

    fn metadata_service(
        &self,
        origin: Rc<dyn Origin>,
        capacity: usize,
    ) -> super::super::metadata::MetadataService {
        self.metadata_service_with_index(origin, capacity, self.metadata_index(4))
    }

    fn metadata_service_with_index(
        &self,
        origin: Rc<dyn Origin>,
        capacity: usize,
        index: Rc<Index>,
    ) -> super::super::metadata::MetadataService {
        use super::super::metadata::{MetadataDependencies, MetadataService};
        MetadataService::new(
            self.fill.dependencies.candidates.clone(),
            origin,
            self.fill.dependencies.credentials.clone(),
            capacity,
            MetadataDependencies {
                index,
                owners: self.fill.dependencies.metadata_owner.clone(),
                fill: Rc::new(self.fill.clone()),
            },
        )
    }

    // Keep graph sizing explicit: cold-copy and hot-range scenarios deliberately
    // use different catalog, delivery, and acquisition bounds.
    fn read_graph(
        &self,
        fill: Rc<Fill>,
        membership: &MembershipLease,
        settings: ReadGraphSettings,
    ) -> (
        Rc<crate::read::Coordinator>,
        crate::read::dispatch::WorkerEndpoint,
        Arc<crate::control::state::PublishedState>,
    ) {
        use crate::{
            control::{
                state::{Availability, CacheDefinition, PublishedState, SnapshotStore},
                wire::*,
            },
            memory::{delivery::Delivery, pipe::new_pipe_pool},
            read::{
                Coordinator,
                metadata::{MetadataDependencies, MetadataService},
                range_stream::RangeStreams,
            },
        };
        let published = Arc::new(PublishedState::default());
        let availability = Rc::new(Availability::new(published.clone(), self.keys.clone()));
        let snapshots = Rc::new(SnapshotStore::new(
            self.keys.cluster().clone(),
            published.clone(),
            settings.snapshots,
        ));
        let (client_socket, origin_socket) =
            crate::control::state::canonical_socket_paths(settings.name).unwrap();
        snapshots
            .publish(Publication {
                schema_version: SCHEMA_VERSION,
                cluster: self.keys.cluster().clone(),
                sequence: PublicationSequence(1),
                membership_version: membership.version,
                members: membership.members().to_vec(),
                caches: vec![CacheDefinition {
                    id: self.context.object.cache.clone(),
                    name: settings.name.into(),
                    client_socket,
                    origin_socket,
                }],
            })
            .unwrap();
        let owners = fill.dependencies.metadata_owner.clone();
        let metadata = Rc::new(MetadataService::new(
            fill.dependencies.candidates.clone(),
            self.origin.clone(),
            fill.dependencies.credentials.clone(),
            settings.metadata,
            MetadataDependencies {
                index: Rc::new(Index::new(
                    WorkerId(0),
                    settings.metadata,
                    availability.clone(),
                )),
                owners: owners.clone(),
                fill: fill.clone(),
            },
        ));
        if let Some(version) = settings.seed {
            metadata.publish_version(version).unwrap();
        }
        let delivery = Rc::new(Delivery::new(
            Rc::new(new_pipe_pool(fill.dependencies.admission.clone())),
            self.reactor.clone(),
            settings.stall,
        ));
        let streams = Rc::new(RangeStreams::new(owners.clone(), delivery, settings.window));
        let local = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill.clone(),
            streams,
            fill.dependencies.credentials.clone(),
            availability,
        ));
        let endpoint = owners.install(WorkerId(0), local.clone()).unwrap();
        (local, endpoint, published)
    }
}

struct ReadGraphSettings {
    name: &'static str,
    snapshots: usize,
    metadata: usize,
    window: usize,
    stall: Duration,
    seed: Option<crate::model::VersionMetadata>,
}
fn pump_worker(
    endpoint: &mut crate::read::dispatch::WorkerEndpoint,
    engine: &mut PageCryptoEngine,
    crypto: &CryptoClient,
    reactor: &Reactor,
) {
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    endpoint.poll(&mut cx, 64).unwrap();
    crate::read::drivers::poll(&mut cx, 64);
    engine.poll_budgeted(64).unwrap();
    crypto.poll_budgeted(64).unwrap();
    reactor.poll_budgeted(128).unwrap();
}
fn fixture() -> Fixture {
    fixture_with(3, None)
}

fn acquire(f: &mut Fixture, budget: &mut AcquisitionBudget) -> Result<PageResult> {
    drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
}
fn fixture_with(length: u64, limits: Option<crate::model::Limits>) -> Fixture {
    fixture_with_caches(
        length,
        limits,
        vec![CacheId(crate::security::test_support::CACHE.into())],
    )
}
fn fixture_with_caches(
    length: u64,
    limits: Option<crate::model::Limits>,
    caches: Vec<CacheId>,
) -> Fixture {
    let mut config = crate::test_support::cluster::config(false);
    if let Some(limits) = limits {
        config.limits = limits;
    }
    let worker = WorkerId(0);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let keys = Rc::new(crate::security::test_support::keys_for(&caches));
    let availability = crate::control::state::for_caches(keys.clone(), caches);
    let buffers = BufferPool::new(admission.clone());
    let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let index = Rc::new(Index::new(worker, 16, availability.clone()));
    let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let directory = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("target")
        .join(format!(
            "read-fill-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
        ));
    let slabs = Rc::new(page_alloc::Slab::new(
        directory.join(format!("worker-{}-slab-0.dat", worker.0)),
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
        crate::model::PAGE_BYTES as usize + crate::store::format::MAX_HEADER_BYTES + 16,
    ));
    slabs
        .open_now()
        .expect("read fixture filesystem supports direct slab alignment");
    let clock = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
    let metrics = Metrics::default();
    let disk = Rc::new(
        StoreReader::new(
            clock,
            index.clone(),
            segments.clone(),
            slabs.clone(),
            admission.clone(),
            reactor.clone(),
            buffers.clone(),
        )
        .with_metrics(metrics.clone()),
    );
    let writer = Rc::new(StoreWriter::new(
        index,
        segments,
        slabs,
        admission.clone(),
        reactor.clone(),
        availability.clone(),
    ));
    let context = OriginContext {
        object: ObjectId {
            cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
            key: CacheKey([0; 32]),
        },
        metadata: None,
        authorization: None,
    };
    let node = crate::model::NodeId("22222222-2222-4222-8222-222222222222".into());
    let membership = Arc::new(
        Membership::validate(
            crate::model::MembershipVersion(1),
            vec![Member {
                node: node.clone(),
                shares: NonZeroU32::new(4).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let metadata = ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
        expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
    };
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    let origin = Rc::new(TestOrigin {
        buffers: buffers.clone(),
        calls: Cell::new(0),
        metadata,
        reject_once: Cell::new(false),
        version_unavailable: Cell::new(false),
        blocked_pages: RefCell::new(Default::default()),
        started_pages: RefCell::new(Vec::new()),
    });
    let (port, engine) = crypto::pair(worker, 0, config.limits.queue_entries);
    let crypto = Rc::new(CryptoClient::new(port));
    let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
    let peers = Rc::new(NoPeer);
    let candidates = Rc::new(CandidatePolicy::new(
        node,
        Rc::new(Placement::new(16)),
        peers.clone(),
        credentials.clone(),
        Arc::new(Default::default()),
    ));
    // Deliberately uninstalled catalog owner: publication must retain page-local
    // metadata and still complete when the optional catalog is unavailable.
    let metadata_owner = Arc::new(
        WorkerDirectory::new(
            Arc::new(WorkerMap::new(vec![worker]).unwrap()),
            vec![worker],
            16,
        )
        .unwrap(),
    );
    let flights = Rc::new(Flights::new(admission.clone(), availability));
    let fill = Fill::new(FillDependencies {
        memory,
        buffers,
        disk,
        writer,
        origin: origin.clone(),
        candidates,
        flights,
        crypto: Rc::new(PageCrypto::new(keys.clone(), crypto.clone())),
        credentials,
        admission,
        metadata_owner,
    })
    .with_metrics(metrics);
    Fixture {
        keys,
        reactor,
        directory,
        fill,
        origin,
        crypto,
        engine: PageCryptoEngine::new(CryptoRuntime { port: engine }),
        context,
        membership,
        page,
        scope: RequestScope::new(
            RequestId([1; 16]),
            Instant::now() + Duration::from_secs(600),
        )
        .unwrap(),
    }
}
fn drive<T>(
    future: impl Future<Output = T>,
    engine: &mut PageCryptoEngine,
    crypto: &CryptoClient,
) -> T {
    let mut future = Box::pin(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..256 {
        super::super::drivers::poll(&mut cx, 64);
        engine.poll_budgeted(64).unwrap();
        crypto.poll_budgeted(64).unwrap();
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
    }
    panic!("bounded test executor did not complete");
}

fn adapter_client(
    f: &Fixture,
    adapter: &crate::test_support::origin::AdapterOrigin,
) -> Rc<crate::origin::OriginClient> {
    use crate::control::{
        state::{PublishedState, SnapshotStore},
        wire::{Publication, PublicationSequence, SCHEMA_VERSION},
    };
    let snapshots = Rc::new(SnapshotStore::new(
        f.keys.cluster().clone(),
        Arc::new(PublishedState::default()),
        2,
    ));
    let (client_socket, origin_socket) =
        crate::control::state::canonical_socket_paths("fixture").unwrap();
    snapshots
        .publish(Publication {
            schema_version: SCHEMA_VERSION,
            cluster: f.keys.cluster().clone(),
            sequence: PublicationSequence(1),
            membership_version: f.membership.version,
            members: f.membership.members().to_vec(),
            caches: vec![crate::control::state::CacheDefinition {
                id: f.context.object.cache.clone(),
                name: "fixture".into(),
                client_socket,
                origin_socket,
            }],
        })
        .unwrap();
    adapter.client(
        snapshots,
        f.fill.dependencies.admission.clone(),
        f.reactor.clone(),
        f.fill.dependencies.buffers.clone(),
    )
}

fn drive_io<T>(
    future: impl Future<Output = T>,
    reactor: &Reactor,
    engine: &mut PageCryptoEngine,
    crypto: &CryptoClient,
) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        super::super::drivers::poll(&mut cx, 64);
        engine.poll_budgeted(64).unwrap();
        crypto.poll_budgeted(64).unwrap();
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < deadline, "read fixture made no progress");
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

#[test]
fn abandoned_acquisition_does_not_cancel_shared_peer_scope() {
    abandoned_acquisition_preserves_peer_scope(false);
}

#[test]
fn abandoned_metadata_does_not_cancel_shared_peer_scope() {
    abandoned_acquisition_preserves_peer_scope(true);
}

fn abandoned_acquisition_preserves_peer_scope(metadata: bool) {
    use crate::model::MetadataSelector;
    {
        let mut f = fixture();
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _queue = queue.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
        if metadata {
            let (send, receive) = futures::channel::oneshot::channel();
            let service = f.metadata_service(
                Rc::new(metadata::GatedMetadataOrigin {
                    buffers: f.origin.buffers.clone(),
                    receive: RefCell::new(Some(receive)),
                    calls: Cell::new(0),
                }),
                4,
            );
            let mut read = service.resolve(
                MetadataSelector::Fresh,
                f.membership.clone(),
                &f.context,
                &f.scope,
            );
            assert!(read.as_mut().poll(&mut cx).is_pending());
            drop(read);
            send.send(MetadataReply {
                metadata: f.origin.metadata.clone(),
                page_zero: None,
            })
            .ok()
            .unwrap();
            queue.poll(&mut cx, 64);
        } else {
            let mut read = f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            );
            assert!(read.as_mut().poll(&mut cx).is_pending());
            drop(read);
            queue.poll(&mut cx, 64);
        }
        assert_eq!(
            f.scope.check(),
            Ok(()),
            "abandoning metadata={metadata} must not poison worker peer ingress"
        );
        assert_eq!(queue.pending(), 0);
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
        let result = acquire(&mut f, &mut budget).unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc");
    }
}

#[test]
fn completed_fill_waits_release_shared_cancellation_capacity() {
    use futures::{StreamExt, stream::FuturesUnordered};
    let mut f = fixture();
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _queue = queue.enter();
    // Force a terminal fill failure without any accepted I/O. Every cohort must
    // release its waiter and driver before the same long-lived scope is reused.
    let admission = &f.fill.dependencies.admission;
    let pressure = admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            admission.limit(ResourceClass::Plaintext),
        )
        .unwrap();
    for round in 0..1100 {
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
        let mut pending = FuturesUnordered::new();
        pending.push(f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ));
        assert!(
            matches!(
                drive(pending.next(), &mut f.engine, &f.crypto),
                Some(Err(Error::Overloaded))
            ),
            "pressure round {round}"
        );
        drop(pending);
        assert_eq!(queue.pending(), 0);
        assert_eq!(admission.used(ResourceClass::Flight), 0);
        assert_eq!(admission.used(ResourceClass::Waiter), 0);
    }
    drop(pressure);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let mut pending = FuturesUnordered::new();
    pending.push(f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut budget,
    ));
    let result = drive(pending.next(), &mut f.engine, &f.crypto).unwrap();
    assert!(
        result.is_ok(),
        "released pressure must recover without replacing scope: {:?}",
        result.as_ref().err()
    );
    assert_eq!(result.unwrap().plaintext.bytes(), b"abc");
}

#[test]
fn selected_owner_reclaims_foreign_receive_charges_across_full_pages() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::model::PAGE_BYTES;
    use std::num::NonZeroUsize;
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(3 * (PAGE_BYTES as usize + 16)).unwrap();
    let mut source = fixture_with(8 * PAGE_BYTES, Some(limits.clone()));
    let mut target = fixture_with(8 * PAGE_BYTES, Some(limits));
    for number in 0..8 {
        let page = PageId {
            version: source.page.version.clone(),
            number: PageNumber(number),
        };
        let mut budget = AcquisitionBudget::new(source.scope.deadline.0, 8, 16);
        let received = drive(
            source.fill.acquire(
                page.clone(),
                source.membership.clone(),
                &source.context,
                &source.scope,
                &mut budget,
            ),
            &mut source.engine,
            &source.crypto,
        )
        .unwrap();
        // Hold an alias to model a transport completion owner. Rehoming must not
        // revoke its bytes/charge, even when a copy is necessary.
        let canceled = RequestScope::new(target.scope.request, target.scope.deadline.0).unwrap();
        canceled.cancel().unwrap();
        assert!(matches!(
            drive(
                target.fill.accept_selected(received.copy(), &canceled),
                &mut target.engine,
                &target.crypto
            ),
            Err(Error::Cancelled)
        ));
        assert_eq!(target.fill.metrics.count(Event::PeerHit), number);
        let copy = received.copy();
        let result = drive(
            target.fill.accept_selected(copy, &target.scope),
            &mut target.engine,
            &target.crypto,
        )
        .unwrap();
        assert!(
            target
                .fill
                .dependencies
                .admission
                .owns(&result.plaintext.inner.reservation)
        );
        assert!(
            target
                .fill
                .dependencies
                .admission
                .owns(&result.ciphertext.inner.reservation)
        );
        assert_eq!(result.ciphertext.bytes(), received.ciphertext.bytes());
        assert_eq!(result.plaintext.bytes()[0], number as u8);
        assert_eq!(target.fill.metrics.count(Event::PeerHit), number + 1);
        assert_eq!(target.fill.metrics.count(Event::OriginFill), 0);
        drop((received, result));
        source.fill.dependencies.flights.poll_budgeted(128).unwrap();
        source.fill.dependencies.writer.discard_unsubmitted();
        source
            .fill
            .dependencies
            .memory
            .remove_cache(&source.context.object.cache)
            .unwrap();
        source.fill.dependencies.admission.reclaim_buffers();
        assert_eq!(
            source
                .fill
                .dependencies
                .admission
                .used(ResourceClass::Ciphertext),
            0
        );
        // Previously this source reservation failed while the remote owner held
        // an idle page. No target eviction should be required to free the source.
        assert!(
            source
                .fill
                .reserve_bootstrap(&source.context.object.cache)
                .is_ok()
        );
        assert!(
            target
                .fill
                .dependencies
                .memory
                .get(&page)
                .unwrap()
                .is_some()
        );
    }
    assert_eq!(source.origin.calls.get(), 8);
    assert_eq!(target.origin.calls.get(), 0);
}

#[test]
fn ordinary_publication_rejects_foreign_worker_charges() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut source = fixture();
    let target = fixture();
    let mut budget = AcquisitionBudget::new(source.scope.deadline.0, 8, 16);
    let page = acquire(&mut source, &mut budget).unwrap();
    assert!(matches!(
        target.fill.dependencies.memory.publish(page.clone()),
        Err(Error::InvalidConfiguration)
    ));
    assert!(
        target
            .fill
            .dependencies
            .memory
            .get(&source.page)
            .unwrap()
            .is_none()
    );
    assert_eq!(
        target
            .fill
            .dependencies
            .admission
            .used(ResourceClass::Plaintext),
        0
    );
    assert!(
        source
            .fill
            .dependencies
            .admission
            .used(ResourceClass::Plaintext)
            >= 3
    );
}

#[test]
fn cached_corrupt_ciphertext_falls_back_without_exposing_plaintext() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = acquire(&mut f, &mut budget).unwrap();
    let mut bytes = result.ciphertext.bytes().to_vec();
    bytes[0] ^= 1;
    let copy = crate::memory::page::CiphertextCopy {
        metadata: result.metadata.clone(),
        ciphertext: f
            .fill
            .dependencies
            .buffers
            .ciphertext(
                f.fill
                    .dependencies
                    .admission
                    .reserve(
                        Some(&f.context.object.cache),
                        ResourceClass::Ciphertext,
                        bytes.len(),
                    )
                    .unwrap(),
                result.ciphertext.envelope().clone(),
                bytes,
            )
            .unwrap(),
    };
    f.fill
        .dependencies
        .memory
        .remove_cache(&f.context.object.cache)
        .unwrap();
    drop(result);
    f.fill
        .dependencies
        .memory
        .publish_ciphertext(UnverifiedPage {
            copy,
            disk_token: None,
        })
        .unwrap();
    let metrics = Metrics::default();
    f.fill.metrics = metrics.clone();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(
        metrics.count(Event::PageDecrypt),
        2,
        "bad cached copy then retained original"
    );
    assert_eq!(metrics.count(Event::FillDecryptRetainedCorrupt), 1);
    assert_eq!(metrics.count(Event::FillDecryptDiskCorrupt), 0);
    assert_eq!(metrics.count(Event::FillDecryptPeerCorrupt), 0);
    assert_eq!(f.origin.calls.get(), 1);
}

#[test]
fn ciphertext_ready_promotes_once_for_concurrent_plaintext_readers() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    let metrics = Metrics::default();
    f.fill.metrics = metrics.clone();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let original = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(f.origin.calls.get(), 1);
    let copy = original.copy();
    drop(original);
    // Pending original ciphertext is retained by the writer, without plaintext.
    f.fill
        .dependencies
        .memory
        .remove_cache(&f.context.object.cache)
        .unwrap();
    let flights = f.fill.dependencies.flights.clone();
    let mut holder_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let JoinedFlight::Waiter(holder) = flights
        .join_for(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut holder_budget,
            false,
        )
        .unwrap()
    else {
        panic!("registration")
    };
    let mut cipher_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = drive(
        f.fill.acquire_ciphertext(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut cipher_budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert_eq!(result.ciphertext.bytes(), copy.ciphertext.bytes());
    assert_eq!(metrics.count(Event::PageDecrypt), 0);
    let mut first_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let mut second_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let (a, b) = drive(
        async {
            futures::join!(
                f.fill.acquire(
                    f.page.clone(),
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                    &mut first_budget
                ),
                f.fill.acquire(
                    f.page.clone(),
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                    &mut second_budget
                )
            )
        },
        &mut f.engine,
        &f.crypto,
    );
    let (a, b) = (a.unwrap(), b.unwrap());
    assert_eq!(a.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(&a.plaintext.inner, &b.plaintext.inner));
    assert_eq!(metrics.count(Event::PageDecrypt), 1);
    assert_eq!(f.origin.calls.get(), 1);
    // The elected promotion returns its transferred budget intact. Neither a
    // ciphertext-only reader nor a coalesced plaintext reader starts a new route.
    for budget in [&cipher_budget, &first_budget, &second_budget] {
        assert_eq!(budget.remaining_attempts(), 8);
        assert_eq!(budget.remaining_links(), 8);
        assert_eq!(budget.deadline(), f.scope.deadline.0);
    }
    assert_eq!(metrics.gauge(Gauge::ActiveFills), 0);
    assert_eq!(super::super::drivers::pending(), 0);
    drop(holder);
}

#[test]
fn retired_completed_flight_misses_new_callers_but_admitted_waiters_finish() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::security::test_support::rotation_bundle;
    use racer_identity::KeyPurpose;
    let mut f = fixture_with(3, None);
    let flights = f.fill.dependencies.flights.clone();
    let mut held_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let JoinedFlight::Waiter(mut held) = flights
        .join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut held_budget,
        )
        .unwrap()
    else {
        panic!("expected registration")
    };
    let JoinedCopy::Waiter(mut old_copy) = flights.join_copy(&f.page, &f.scope).unwrap() else {
        panic!("expected copy registration")
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    // The retained waiter borrows context/scope, so drive only the engine mutably.
    let original = drive(
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
    let old_key = original.ciphertext.envelope().key_id;
    // Exercise the completed fast paths before retirement, with registrations
    // deliberately retaining the real encrypted fill after its driver finishes.
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Complete(_)
    ));
    assert!(matches!(
        flights
            .join(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget
            )
            .unwrap(),
        JoinedFlight::Complete(_)
    ));
    let roots = (*f.keys.peer_trust_roots().unwrap()).clone();
    f.keys.install(rotation_bundle(2, roots)).unwrap();
    let current_key = f
        .keys
        .active(&f.context.object.cache, KeyPurpose::Page)
        .unwrap()
        .id();
    assert_ne!(current_key, old_key);
    assert!(
        f.keys
            .lease(Some(&f.context.object.cache), old_key, KeyPurpose::Page)
            .is_err()
    );
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(matches!(
        flights.join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget
        ),
        Err(Error::MissingKey)
    ));
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Miss
    ));
    assert!(matches!(
        drive(
            f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget
            ),
            &mut f.engine,
            &f.crypto,
        ),
        Err(Error::MissingKey)
    ));
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_none()
    );
    assert_eq!(f.origin.calls.get(), 1);
    // The public pinned coordinator may still know the immutable descriptor.
    // Its range body must nevertheless miss the retired completed page flight.
    {
        use crate::{
            client::{ClientRequest, ReadKind},
            model::ByteRange,
            read::ReadService,
        };
        let (coordinator, mut endpoint, _) = f.read_graph(
            Rc::new(Fill::new(f.fill.dependencies.clone())),
            &f.membership,
            ReadGraphSettings {
                name: "rotation",
                snapshots: 2,
                metadata: 16,
                window: 1,
                stall: Duration::from_secs(10),
                seed: Some(original.metadata.immutable()),
            },
        );
        for ordered in [false, true] {
            let kind = ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::Closed { first: 0, last: 2 }),
                page_credits: 1,
                byte_credits: crate::model::PAGE_BYTES,
                ordered,
            };
            let mut response = drive(
                coordinator.read(
                    ClientRequest {
                        kind,
                        origin: OriginContext {
                            object: f.context.object.clone(),
                            metadata: None,
                            authorization: None,
                        },
                    },
                    &f.scope,
                ),
                &mut f.engine,
                &f.crypto,
            )
            .unwrap();
            let mut slice = response.body.as_mut().unwrap().next_slice();
            assert!(matches!(
                drive(
                    std::future::poll_fn(|cx| {
                        endpoint.poll(cx, 64).unwrap();
                        slice.as_mut().poll(cx)
                    }),
                    &mut f.engine,
                    &f.crypto
                ),
                Err(Error::MissingKey)
            ));
        }
    }
    let AcquisitionEvent::Complete(admitted) = futures::executor::block_on(held.wait()).unwrap()
    else {
        panic!("original waiter must finish")
    };
    let admitted_copy = futures::executor::block_on(old_copy.wait()).unwrap();
    assert_eq!(admitted.plaintext.bytes(), b"abc");
    assert_eq!(
        admitted_copy.copy().ciphertext.bytes(),
        original.ciphertext.bytes()
    );
    assert_eq!(admitted.ciphertext.envelope().key_id, old_key);
    drop((held, old_copy));
    let replacement = drive(
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
    assert_eq!(replacement.ciphertext.envelope().key_id, current_key);
    assert_eq!(replacement.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(original.plaintext.bytes(), b"abc");

    // Publication can also retain a completed bundle behind an outstanding
    // completion fence. New joins must not sneak into that draining cohort.
    let JoinedFlight::Waiter(mut draining) = flights
        .join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut held_budget,
        )
        .unwrap()
    else {
        panic!("expected new flight")
    };
    let AcquisitionEvent::Lead(leader) = futures::executor::block_on(draining.wait()).unwrap()
    else {
        panic!("expected election")
    };
    let operation = flights.retain_operation(&leader, ()).unwrap();
    flights.publish(leader, replacement.clone()).unwrap();
    let roots = (*f.keys.peer_trust_roots().unwrap()).clone();
    f.keys.install(rotation_bundle(3, roots)).unwrap();
    assert!(matches!(
        flights.join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget
        ),
        Err(Error::MissingKey)
    ));
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Miss
    ));
    operation.complete().unwrap();
    let AcquisitionEvent::Complete(result) = futures::executor::block_on(draining.wait()).unwrap()
    else {
        panic!("admitted draining waiter must finish")
    };
    assert_eq!(result.ciphertext.envelope().key_id, current_key);
}

#[test]
fn concurrent_readers_share_origin_encryption_and_pending_original_ciphertext() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    let mut a = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let mut b = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let reads = futures::future::join(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut a,
        ),
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut b,
        ),
    );
    let (a, b) = drive(reads, &mut f.engine, &f.crypto);
    let a = a.unwrap();
    let b = b.unwrap();
    assert_eq!(f.origin.calls.get(), 1);
    assert!(Arc::ptr_eq(&a.plaintext.inner, &b.plaintext.inner));
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
    assert_eq!(f.fill.metrics.count(Event::MemoryHit), 0);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 0);
    assert!(Arc::ptr_eq(&a.ciphertext.inner, &b.ciphertext.inner));
    assert_eq!(a.plaintext.bytes(), b"abc");
    let copy = drive(
        f.fill.copy_only(&f.page, &f.scope),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap()
    .unwrap();
    assert_eq!(copy.1.bytes(), a.ciphertext.bytes());
    assert_eq!(copy.1.envelope().nonce, a.ciphertext.envelope().nonce);
    assert_eq!(f.origin.calls.get(), 1);
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_some()
    );
    assert!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext)
            > 0
    );
}

#[test]
fn ciphertext_origin_fill_retains_verified_publication_without_a_plaintext_waiter() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    f.context.authorization =
        Some(crate::model::Authorization::from_header(b"test-supplier-credential").unwrap());
    let flights = f.fill.dependencies.flights.clone();
    let mut holder_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let JoinedFlight::Waiter(holder) = flights
        .join_for(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut holder_budget,
            false,
        )
        .unwrap()
    else {
        panic!("ciphertext registration")
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let copy = drive(
        f.fill.acquire_ciphertext(
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
    let retained = f.fill.dependencies.memory.get(&f.page).unwrap().unwrap();
    assert_eq!(retained.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(
        &retained.ciphertext.inner,
        &copy.ciphertext.inner
    ));
    assert!(
        f.fill
            .dependencies
            .memory
            .unverified(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(f.fill.local_copies.borrow().is_empty());
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Complete(_)
    ));
    drop(holder);
    // A different credential context can consume verified bytes, never the
    // supplier's authorization. No additional origin call or decrypt is needed.
    let context = OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 0, 0);
    let result = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert!(Arc::ptr_eq(
        &retained.plaintext.inner,
        &result.plaintext.inner
    ));
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 0);
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::RequestContext),
        0
    );
}

fn drive_disk<T>(f: &Fixture, future: impl Future<Output = T>) -> T {
    let mut future = Box::pin(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    loop {
        f.scope.check().unwrap();
        super::super::drivers::poll(&mut cx, 64);
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        f.reactor.poll_budgeted(64).unwrap();
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

fn cold_disk_fixture() -> Fixture {
    let mut f = fixture();
    f.scope.deadline.0 = Instant::now() + Duration::from_secs(30);
    f.reactor.init().unwrap();
    futures::executor::block_on(f.fill.dependencies.writer.open()).unwrap();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let original = acquire(&mut f, &mut budget).unwrap();
    assert_eq!(
        drive_disk(&f, f.fill.dependencies.writer.progress(1, &f.scope)).unwrap(),
        1
    );
    drop(original);
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    assert!(
        f.fill
            .dependencies
            .memory
            .ciphertext(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_none()
    );
    f.origin.calls.set(0);
    f
}

#[test]
fn copy_only_rejects_disk_payload_and_tag_corruption_before_retention() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    for acquire in [false, true] {
        for corrupt_tag in [false, true] {
            let mut f = cold_disk_fixture();
            let writer = &f.fill.dependencies.writer;
            let location = writer.index().lookup(&f.page).unwrap().unwrap().location;
            let staging = writer
                .slabs()
                .allocate(
                    location.extent.length(),
                    f.fill
                        .dependencies
                        .admission
                        .reserve(None, ResourceClass::Ciphertext, location.extent.length())
                        .unwrap(),
                )
                .unwrap();
            let lease = writer.lease(&location).unwrap();
            let mut stored = drive_disk(
                &f,
                writer
                    .slabs()
                    .read(&f.reactor, location.extent, staging, lease, &f.scope),
            )
            .unwrap();
            let parsed = crate::store::format::parse(&stored, location.extent).unwrap();
            let offset = if corrupt_tag {
                parsed.ciphertext.end - 1
            } else {
                parsed.ciphertext.start
            };
            stored.bytes_mut().unwrap()[offset] ^= 1;
            let lease = writer.lease(&location).unwrap();
            drive_disk(
                &f,
                writer
                    .slabs()
                    .write(&f.reactor, location.extent, stored, lease, &f.scope),
            )
            .unwrap();
            if acquire {
                assert!(
                    drive_io(
                        f.fill.acquire_local_copy(&f.page, &f.scope, false),
                        &f.reactor,
                        &mut f.engine,
                        &f.crypto
                    )
                    .unwrap()
                    .is_none()
                );
            } else {
                assert!(
                    drive_io(
                        f.fill.copy_only(&f.page, &f.scope),
                        &f.reactor,
                        &mut f.engine,
                        &f.crypto
                    )
                    .unwrap()
                    .is_none()
                );
            }
            assert!(
                drive_io(
                    f.fill.copy_only(&f.page, &f.scope),
                    &f.reactor,
                    &mut f.engine,
                    &f.crypto
                )
                .unwrap()
                .is_none()
            );
            assert!(writer.index().lookup(&f.page).unwrap().is_none());
            assert!(
                f.fill
                    .dependencies
                    .memory
                    .unverified(&f.page)
                    .unwrap()
                    .is_none()
            );
            assert_eq!(f.fill.metrics.count(Event::CorruptMiss), 1);
            assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 0);
            assert_eq!(f.origin.calls.get(), 0);
            assert_eq!(f.reactor.in_flight(), 0);
            assert!(f.fill.local_copies.borrow().is_empty());
            assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
            assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
            if acquire {
                // A full ciphertext-only Acquire must fall through to origin, not
                // publish the rejected disk copy through its flight.
                f.origin.version_unavailable.set(true);
                let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
                assert!(matches!(
                    drive_io(
                        f.fill.acquire_ciphertext(
                            f.page.clone(),
                            f.membership.clone(),
                            &f.context,
                            &f.scope,
                            &mut budget
                        ),
                        &f.reactor,
                        &mut f.engine,
                        &f.crypto
                    ),
                    Err(Error::VersionUnavailable)
                ));
                assert_eq!(f.origin.calls.get(), 1);
                assert!(
                    f.fill
                        .dependencies
                        .memory
                        .unverified(&f.page)
                        .unwrap()
                        .is_none()
                );
            }
        }
    }
}

#[test]
fn disk_crc_validation_preserves_replacement_and_intact_acquire() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = cold_disk_fixture();
    let (mut stale, token) = drive_disk(
        &f,
        f.fill.dependencies.disk.read_with_token(&f.page, &f.scope),
    )
    .unwrap()
    .unwrap();
    let (replacement, _) = drive_disk(
        &f,
        f.fill.dependencies.disk.read_with_token(&f.page, &f.scope),
    )
    .unwrap()
    .unwrap();
    // Hold an old read across a same-page replacement publication.
    Arc::get_mut(&mut stale.ciphertext.inner).unwrap().bytes[0] ^= 1;
    let writer = &f.fill.dependencies.writer;
    let old = writer.index().lookup(&f.page).unwrap().unwrap().location;
    let dirty = f
        .fill
        .dependencies
        .admission
        .reserve(
            Some(&f.page.version.object.cache),
            ResourceClass::DirtyCiphertext,
            replacement.ciphertext.bytes().len(),
        )
        .unwrap();
    writer.enqueue(replacement, dirty).unwrap();
    drive_disk(&f, writer.progress(1, &f.scope)).unwrap();
    let new = writer.index().lookup(&f.page).unwrap().unwrap().location;
    assert_ne!(old, new);
    assert_eq!(
        drive(
            f.fill.validate_disk_copy(&stale, &f.page, &token, &f.scope),
            &mut f.engine,
            &f.crypto
        ),
        Err(Error::CorruptRecord)
    );
    assert_eq!(
        writer.index().lookup(&f.page).unwrap().unwrap().location,
        new
    );
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let intact = drive_io(
        f.fill.acquire_ciphertext(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &f.reactor,
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert_eq!(intact.ciphertext.verify_checksum(), Ok(()));
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 0);
    assert_eq!(f.origin.calls.get(), 0);
    assert!(
        f.fill
            .dependencies
            .memory
            .unverified(&f.page)
            .unwrap()
            .is_some()
    );
}

#[test]
fn concurrent_cold_disk_copy_only_shares_io_and_retains_original_ciphertext() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = cold_disk_fixture();
    let disk_hits = f.fill.metrics.count(Event::DiskIndexLookupHit);
    let cipher_misses = f.fill.metrics.count(Event::CiphertextLookupMiss);
    let pending_misses = f.fill.metrics.count(Event::PendingLookupMiss);
    let context_baseline = f
        .fill
        .dependencies
        .admission
        .used(ResourceClass::RequestContext);
    let mut first = f.fill.copy_only(&f.page, &f.scope);
    let mut second = f.fill.copy_only(&f.page, &f.scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.reactor.in_flight(), 1, "one cold disk submission");
    assert_eq!(f.fill.local_copies.borrow().len(), 1);
    let (first, second) = drive_io(
        futures::future::join(first, second),
        &f.reactor,
        &mut f.engine,
        &f.crypto,
    );
    let (first, second) = (first.unwrap().unwrap(), second.unwrap().unwrap());
    assert!(Arc::ptr_eq(&first.1.inner, &second.1.inner));
    let retained = f
        .fill
        .dependencies
        .memory
        .unverified(&f.page)
        .unwrap()
        .unwrap();
    assert!(retained.disk_token.is_some());
    assert!(Arc::ptr_eq(&first.1.inner, &retained.copy.ciphertext.inner));
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
    let hot = drive_disk(&f, f.fill.copy_only(&f.page, &f.scope))
        .unwrap()
        .unwrap();
    assert!(Arc::ptr_eq(&first.1.inner, &hot.1.inner));
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 1);
    assert_eq!(
        f.fill.metrics.count(Event::DiskIndexLookupHit),
        disk_hits + 1
    );
    assert_eq!(
        f.fill.metrics.count(Event::CiphertextLookupMiss),
        cipher_misses + 2
    );
    assert_eq!(
        f.fill.metrics.count(Event::PendingLookupMiss),
        pending_misses + 2
    );
    assert_eq!(f.fill.metrics.count(Event::CiphertextLookupHit), 1);
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 0);
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert!(f.fill.local_copies.borrow().is_empty());
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::RequestContext),
        context_baseline
    );
    // The retained disk token and original ciphertext also support a single
    // verified promotion, rather than another disk read or origin acquisition.
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let promoted = drive(
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
    assert_eq!(promoted.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(&first.1.inner, &promoted.ciphertext.inner));
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 1);
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 1);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn detached_copy_only_keeps_disk_fence_and_independent_waiters() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = cold_disk_fixture();
    let caller = RequestScope::new(RequestId([9; 16]), f.scope.deadline.0).unwrap();
    let mut first = f.fill.copy_only(&f.page, &caller);
    let mut second = f.fill.copy_only(&f.page, &f.scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    caller.cancel().unwrap();
    assert!(matches!(
        first.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    drop((first, second));
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 1);
    assert_eq!(f.fill.local_copies.borrow().len(), 1);
    assert_eq!(f.reactor.in_flight(), 1);
    // A replacement joins the still-owned read, not a second disk submission.
    let mut replacement = f.fill.copy_only(&f.page, &f.scope);
    assert!(replacement.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.reactor.in_flight(), 1);
    assert!(
        drive_io(replacement, &f.reactor, &mut f.engine, &f.crypto)
            .unwrap()
            .is_some()
    );
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
    assert!(f.fill.local_copies.borrow().is_empty());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn copy_only_miss_releases_shared_scope_subscriptions_across_cohorts() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    use futures::{StreamExt, stream::FuturesUnordered};
    let mut f = fixture();
    for _ in 0..1100 {
        let mut requests = FuturesUnordered::new();
        requests.push(f.fill.copy_only(&f.page, &f.scope));
        assert!(
            drive(requests.next(), &mut f.engine, &f.crypto)
                .unwrap()
                .unwrap()
                .is_none()
        );
        drop(requests);
        assert!(f.fill.local_copies.borrow().is_empty());
        assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
        assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
        assert_eq!(super::super::drivers::pending(), 0);
    }
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.fill.metrics.count(Event::CiphertextLookupMiss), 1100);
    assert_eq!(f.fill.metrics.count(Event::PendingLookupMiss), 1100);
    assert_eq!(f.fill.metrics.count(Event::DiskIndexLookupMiss), 1100);
    assert_eq!(f.fill.metrics.count(Event::DiskIndexLookupHit), 0);
}

#[test]
fn canceled_before_lookup_has_no_outcome() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let f = fixture();
    f.scope.cancel().unwrap();
    assert!(matches!(
        f.fill.cached_page(&f.page, &f.scope),
        Err(Error::Cancelled)
    ));
    assert!(matches!(
        futures::executor::block_on(f.fill.copy_only(&f.page, &f.scope)),
        Err(Error::Cancelled)
    ));
    for event in [
        Event::PlaintextLookupHit,
        Event::PlaintextLookupMiss,
        Event::PlaintextLookupError,
        Event::CiphertextLookupHit,
        Event::CiphertextLookupMiss,
        Event::CiphertextLookupError,
        Event::PendingLookupMiss,
        Event::DiskIndexLookupMiss,
    ] {
        assert_eq!(f.fill.metrics.count(event), 0);
    }
}

#[test]
fn lookup_plaintext_and_pending_hits_do_not_probe_disk() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    assert!(f.fill.cached_page(&f.page, &f.scope).unwrap().is_none());
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let page = acquire(&mut f, &mut budget).unwrap();
    assert!(f.fill.cached_page(&f.page, &f.scope).unwrap().is_some());
    assert_eq!(f.fill.metrics.count(Event::PlaintextLookupHit), 1);
    assert_eq!(f.fill.metrics.count(Event::PlaintextLookupMiss), 2);
    drop(page);
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    // The pending writer protects its shared ciphertext from idle eviction.
    // Remove only memory lookup references to exercise the pending boundary.
    f.fill
        .dependencies
        .memory
        .remove_cache(&f.context.object.cache)
        .unwrap();
    assert!(
        f.fill
            .dependencies
            .memory
            .ciphertext(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_some()
    );
    let disk_misses = f.fill.metrics.count(Event::DiskIndexLookupMiss);
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_some()
    );
    assert_eq!(f.fill.metrics.count(Event::PendingLookupHit), 1);
    assert_eq!(
        f.fill.metrics.count(Event::DiskIndexLookupMiss),
        disk_misses
    );
    assert_eq!(f.fill.metrics.count(Event::DiskIndexLookupHit), 0);
}

#[test]
fn copy_only_local_state_admission_failure_releases_all_reservations() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    let admission = &f.fill.dependencies.admission;
    let pressure = admission
        .reserve(
            None,
            ResourceClass::Flight,
            admission.limit(ResourceClass::Flight),
        )
        .unwrap();
    assert!(matches!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        ),
        Err(Error::Overloaded)
    ));
    assert!(f.fill.local_copies.borrow().is_empty());
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    assert_eq!(super::super::drivers::pending(), 0);
    drop(pressure);
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_none()
    );
    assert_eq!(admission.used(ResourceClass::Flight), 0);
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn copy_only_disk_read_reclaims_idle_ciphertext_without_revoking_live_copies() {
    disk_copy_reclaims_idle_ciphertext(false);
}

#[test]
fn peer_bootstrap_disk_copy_reclaims_idle_ciphertext_before_fresh_acquisition() {
    disk_copy_reclaims_idle_ciphertext(true);
}

fn disk_copy_reclaims_idle_ciphertext(bootstrap: bool) {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = std::num::NonZeroUsize::new(512 * 1024 * 1024).unwrap();
    limits.ciphertext_bytes = limits.plaintext_bytes;
    limits.metadata_entries = std::num::NonZeroUsize::new(64).unwrap();
    let mut f = fixture_with(PAGE_BYTES, Some(limits));
    f.scope.deadline.0 = Instant::now() + Duration::from_secs(30);
    f.reactor.init().unwrap();
    futures::executor::block_on(f.fill.dependencies.writer.open()).unwrap();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let original = acquire(&mut f, &mut budget).unwrap();
    let expected = original.ciphertext.bytes().to_vec();
    let envelope = original.ciphertext.envelope().clone();
    let metadata = original.metadata.clone();
    let run_io = |mut work: crate::error::Operation<'_, usize>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            f.scope.check().unwrap();
            if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                return result.unwrap();
            }
            f.reactor.poll_budgeted(64).unwrap();
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    assert_eq!(run_io(f.fill.dependencies.writer.progress(1, &f.scope)), 1);
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .lookup(&f.page)
            .unwrap()
            .is_some()
    );
    drop(original);
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    let deps = &f.fill.dependencies;
    assert!(
        deps.memory.ciphertext(&f.page).unwrap().is_none(),
        "target must be disk-only"
    );
    let failures = crate::telemetry::failures::Failures::default();
    deps.admission.set_observer(failures.observer(WorkerId(0)));
    let index = f.metadata_index(64);
    let mut fresh = metadata.clone();
    fresh.expires_at = ExpiresAt::from_unix_millis(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_millis() as u64
            + 60_000,
    )
    .unwrap();
    index.publish_current(fresh).unwrap();
    let service = f.metadata_service_with_index(deps.origin.clone(), 64, index);
    let read_copy = || -> crate::error::Operation<'_, (ObjectMetadata, CiphertextPage)> {
        Box::pin(async {
            if bootstrap {
                let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
                let response = service
                    .bootstrap_peer(f.membership.clone(), &f.context, &f.scope, &mut budget)
                    .await?;
                let PeerResponse::Bootstrap {
                    metadata,
                    page_zero: Some(ciphertext),
                } = response
                else {
                    panic!("expected retained bootstrap page")
                };
                assert_eq!(
                    budget.remaining_attempts(),
                    4,
                    "copy needs no acquisition attempt"
                );
                Ok((metadata, ciphertext))
            } else {
                f.fill
                    .copy_only(&f.page, &f.scope)
                    .await
                    .map(|copy| copy.expect("persisted page"))
            }
        })
    };
    let mut retained = Vec::new();
    for number in 1..=30 {
        let mut metadata = metadata.clone();
        metadata.version.object.key = CacheKey([number; 32]);
        let mut envelope = envelope.clone();
        envelope.page.version = metadata.version.clone();
        let ciphertext = deps
            .buffers
            .ciphertext(
                deps.admission
                    .reserve(
                        Some(&f.context.object.cache),
                        ResourceClass::Ciphertext,
                        expected.len(),
                    )
                    .unwrap(),
                envelope,
                expected.clone(),
            )
            .unwrap();
        let page = crate::memory::page::UnverifiedPage {
            copy: crate::memory::page::CiphertextCopy {
                metadata,
                ciphertext,
            },
            disk_token: None,
        };
        deps.memory.publish_ciphertext(page.clone()).unwrap();
        retained.push(page);
    }
    // All copies are live: fail without revoking a reader or starting origin work.
    let mut blocked = read_copy();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    match blocked.as_mut().poll(&mut cx) {
        Poll::Ready(Err(Error::Overloaded)) => {}
        Poll::Ready(Err(error)) => panic!("unexpected blocked error {error:?}"),
        Poll::Ready(Ok(_)) => panic!("unexpected blocked success"),
        Poll::Pending => panic!("unexpected blocked pending"),
    }
    drop(blocked);
    assert_eq!(f.origin.calls.get(), 1);
    // Keep one copy pinned, but make the remaining working set reclaimable.
    let busy = retained.pop().unwrap();
    drop(retained);
    let mut read = read_copy();
    let copy = loop {
        f.scope.check().unwrap();
        f.engine.poll_budgeted(64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
        if let Poll::Ready(result) = read.as_mut().poll(&mut cx) {
            break result.expect("idle cached ciphertext must not reject a disk copy");
        }
        f.reactor.poll_budgeted(64).unwrap();
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    };
    assert_eq!(copy.1.bytes(), expected);
    assert_eq!(copy.1.envelope(), &envelope);
    assert_eq!(busy.copy.ciphertext.bytes(), expected);
    assert!(
        deps.memory
            .ciphertext(&busy.copy.ciphertext.envelope().page)
            .unwrap()
            .is_some()
    );
    assert_eq!(f.origin.calls.get(), 1);
    assert!(
        deps.admission.used(ResourceClass::Ciphertext)
            <= deps.admission.limit(ResourceClass::Ciphertext)
    );
    let mut diagnostics = String::new();
    failures.write(&mut diagnostics).unwrap();
    assert!(diagnostics.contains("requested: 33554960"), "{diagnostics}");
    drop((read, copy, busy));
    deps.memory.evict_idle(usize::MAX).unwrap();
    deps.writer.slabs().reclaim_idle();
    deps.admission.reclaim_buffers();
    assert_eq!(deps.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.reactor.in_flight(), 0);
}

#[test]
fn copy_only_miss_has_no_origin_side_effect_and_wrong_context_never_joins() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_none()
    );
    assert_eq!(f.origin.calls.get(), 0);
    f.context.object.key = CacheKey([1; 32]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::InvalidRequest)
    ));
    assert_eq!(budget.remaining_attempts(), 4);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn rejected_origin_supplier_does_not_fail_an_independent_coalesced_reader() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    f.origin.reject_once.set(true);
    let mut a = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let mut b = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let reads = futures::future::join(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut a,
        ),
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut b,
        ),
    );
    let (first, second) = drive(reads, &mut f.engine, &f.crypto);
    assert!(matches!(first, Err(Error::OriginForbidden)));
    assert_eq!(second.unwrap().plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(a.remaining_attempts(), 3);
    assert_eq!(b.remaining_attempts(), 3);
}

#[test]
fn canceled_supplier_retains_crypto_fence_before_replacement_origin_work() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    let mut a = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let second_scope = RequestScope::new(RequestId([2; 16]), f.scope.deadline.0).unwrap();
    let mut b = AcquisitionBudget::new(second_scope.deadline.0, 4, 8);
    let mut first = f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut a,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.origin.calls.get(), 1);
    super::super::drivers::poll(&mut cx, 64);
    assert_eq!(
        f.crypto.outstanding(),
        1,
        "origin bytes accepted for crypto before cancellation"
    );
    drop(first);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 1);
    let mut second = f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &second_scope,
        &mut b,
    );
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        f.origin.calls.get(),
        1,
        "retry cannot overlap retained crypto completion"
    );
    // Reap no crypto here: a cancellation notification must not stand in for its
    // accepted completion even when the detached read driver is polled again.
    super::super::drivers::poll(&mut cx, 64);
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        f.origin.calls.get(),
        1,
        "accepted crypto must fence replacement election"
    );
    let result = drive(second, &mut f.engine, &f.crypto).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 0);
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
}
use uring_runtime::reactor::IoBuffer;
