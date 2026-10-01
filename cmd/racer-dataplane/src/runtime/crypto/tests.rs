use super::*;
use crate::test_support::WakeCounter;

struct Fixture {
    admission: std::rc::Rc<crate::runtime::admission::Admission>,
    client: CryptoClient,
    engine: CryptoPort,
    scope: RequestScope,
}

impl Fixture {
    fn new() -> Self {
        let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        Self {
            admission,
            client: CryptoClient::new(io),
            engine,
            scope: RequestScope::new(
                crate::model::RequestId([0; 16]),
                crate::runtime::environment::now() + std::time::Duration::from_secs(5),
            )
            .unwrap(),
        }
    }
}

pub(super) fn input(admission: &std::rc::Rc<crate::runtime::admission::Admission>) -> CryptoInput {
    use crate::{
        memory::pool::BufferPool,
        model::{ResourceClass, *},
    };
    let cache = CacheId("00000000-0000-4000-8000-000000000003".into());
    CryptoInput::Encrypt {
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        },
        plaintext: BufferPool::new(admission.clone())
            .plaintext(
                admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                    .unwrap(),
                1,
            )
            .unwrap(),
        ciphertext: admission
            .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
            .unwrap(),
    }
}

#[test]
fn engine_loss_reclaims_queued_owners_and_unblocks_drain() {
    use crate::model::ResourceClass;
    let Fixture {
        admission,
        client,
        engine,
        scope,
    } = Fixture::new();
    let mut future = client.execute(input(&admission), key(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(engine);
    client.poll_budgeted(1).unwrap();
    assert_eq!(client.outstanding(), 0);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert!(matches!(future.as_mut().poll(&mut cx), Poll::Ready(Err(_))));
    assert!(client.drain(&scope).as_mut().poll(&mut cx).is_ready());
}

#[test]
fn accepted_cancellation_cannot_return_before_completion_consumption() {
    use crate::model::ResourceClass;
    let Fixture {
        admission,
        client,
        mut engine,
        scope,
    } = Fixture::new();
    let mut future = client.execute(input(&admission), key(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    scope.cancel().unwrap();
    for _ in 0..4 {
        client.poll_budgeted(1).unwrap();
        assert!(future.as_mut().poll(&mut cx).is_pending());
    }
    assert_eq!(client.outstanding(), 1);
    assert_eq!(admission.used(ResourceClass::Plaintext), 1);
    let job = match engine.poll_job(&mut cx) {
        Poll::Ready(Ok(Some(job))) => job,
        _ => panic!("job"),
    };
    let completion = crate::security::aead::PageCryptoEngine::process(job);
    assert!(engine.complete(completion).is_ok());
    assert!(
        future.as_mut().poll(&mut cx).is_pending(),
        "queued completion is not consumed"
    );
    client.poll_budgeted(1).unwrap();
    assert!(matches!(
        future.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    assert_eq!(client.outstanding(), 0);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
}

#[test]
fn accepted_deadline_expiry_waits_for_engine_completion() {
    let clock = crate::runtime::environment::SimulationClock::new(91);
    let _environment = clock.environment(0).enter();
    let Fixture {
        admission,
        client,
        mut engine,
        scope,
    } = Fixture::new();
    let key = key();
    let mut future = client.execute(input(&admission), key, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    clock.advance(std::time::Duration::from_secs(5));
    assert!(future.as_mut().poll(&mut cx).is_pending());
    let job = match engine.poll_job(&mut cx) {
        Poll::Ready(Ok(Some(job))) => job,
        _ => panic!("job"),
    };
    let completion = crate::security::aead::PageCryptoEngine::process(job);
    assert!(engine.complete(completion).is_ok());
    client.poll_budgeted(1).unwrap();
    assert!(matches!(
        future.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::DeadlineExceeded))
    ));
    assert_eq!(client.outstanding(), 0);
}

#[test]
fn abandoned_task_cannot_replace_worker_completion_wake() {
    let Fixture {
        admission,
        client,
        mut engine,
        scope,
    } = Fixture::new();
    let driver = Arc::new(WakeCounter::default());
    client.register_driver(&Waker::from(driver.clone()));
    let mut future = client.execute(input(&admission), key(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    let job = match engine.poll_job(&mut cx) {
        Poll::Ready(Ok(Some(job))) => job,
        _ => panic!("job"),
    };
    let CryptoJob {
        permit,
        input,
        key,
        scope: _,
    } = job;
    assert!(
        engine
            .complete(CryptoCompletion {
                permit,
                outcome: CryptoOutcome::Failed {
                    input,
                    error: Error::Cancelled
                },
                _key: key,
            })
            .is_ok()
    );
    assert_eq!(driver.count(), 1);
    client.poll_budgeted(1).unwrap();
    assert_eq!(client.outstanding(), 0);
}

#[test]
fn drain_scope_cancellation_wakes_even_with_unconsumed_result() {
    use crate::model::RequestId;
    let Fixture {
        admission,
        client,
        mut engine,
        scope,
    } = Fixture::new();
    let mut future = client.execute(input(&admission), key(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    let job = match engine.poll_job(&mut cx) {
        Poll::Ready(Ok(Some(job))) => job,
        _ => panic!("job"),
    };
    let CryptoJob {
        permit,
        input,
        key,
        scope: _,
    } = job;
    assert!(
        engine
            .complete(CryptoCompletion {
                permit,
                outcome: CryptoOutcome::Failed {
                    input,
                    error: Error::Cancelled
                },
                _key: key,
            })
            .is_ok()
    );
    client.poll_budgeted(1).unwrap();
    let drain_scope = RequestScope::new(RequestId([1; 16]), scope.deadline.0).unwrap();
    let count = Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut drain_cx = Context::from_waker(&waker);
    let mut drain = client.drain(&drain_scope);
    assert!(drain.as_mut().poll(&mut drain_cx).is_pending());
    drain_scope.cancel().unwrap();
    assert!(count.count() > 0);
    assert!(matches!(
        drain.as_mut().poll(&mut drain_cx),
        Poll::Ready(Ok(()))
    ));
    assert!(matches!(
        future.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
}

#[test]
fn capacity_tracks_reserved_owners_and_rejects_reused_identity() {
    let (io, _engine) = pair(WorkerId(1), 4, NonZeroUsize::new(1).unwrap());
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let first = CryptoId {
        worker: WorkerId(1),
        generation: 4,
        sequence: 1,
    };
    let permit = match io.poll_reserve(&mut cx, first) {
        Poll::Ready(Ok(permit)) => permit,
        _ => panic!("first permit"),
    };
    let second = CryptoId {
        sequence: 2,
        ..first
    };
    assert!(io.poll_reserve(&mut cx, second).is_pending());
    assert!(matches!(
        io.poll_reserve(&mut cx, first),
        Poll::Ready(Err(Error::StaleFlight))
    ));
    drop(permit);
    assert!(matches!(
        io.poll_reserve(&mut cx, second),
        Poll::Ready(Ok(_))
    ));
    io.close_submissions().unwrap();
    assert!(matches!(
        io.poll_reserve(
            &mut cx,
            CryptoId {
                sequence: 3,
                ..first
            }
        ),
        Poll::Ready(Err(Error::Unavailable))
    ));
}

#[test]
fn wrong_pair_generation_never_debits_capacity() {
    let (io, _engine) = pair(WorkerId(1), 4, NonZeroUsize::new(1).unwrap());
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for (worker, generation) in [(WorkerId(2), 4), (WorkerId(1), 3)] {
        assert!(matches!(
            io.poll_reserve(
                &mut cx,
                CryptoId {
                    worker,
                    generation,
                    sequence: 1
                }
            ),
            Poll::Ready(Err(Error::StaleFlight))
        ));
    }
    assert_eq!(io.handoff.outstanding.load(Ordering::Acquire), 0);
}

#[test]
fn invalid_queue_capacity_is_a_startup_error_not_a_panic() {
    assert!(matches!(
        try_pair(WorkerId(0), 0, NonZeroUsize::new(usize::MAX).unwrap()),
        Err(Error::InvalidConfiguration)
    ));
}

pub(super) fn key() -> KeyLease {
    keyring()
        .active(
            &crate::model::CacheId("00000000-0000-4000-8000-000000000003".into()),
            crate::security::identity::KeyPurpose::Page,
        )
        .unwrap()
}

pub(super) fn keyring() -> crate::security::identity::Keyring {
    use crate::{
        control::wire::*,
        model::KeyId,
        model::{CacheId, ClusterId, NodeId},
        security::identity::{KeyEpochs, Keyring},
    };
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let mut params = rcgen::CertificateParams::default();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    let ca = params.self_signed(&ca_key).unwrap();
    let keys = Keyring::new(
        ClusterId("00000000-0000-4000-8000-000000000001".into()),
        NodeId("00000000-0000-4000-8000-000000000002".into()),
        Arc::new(KeyEpochs::default()),
    );
    let cache = CacheId("00000000-0000-4000-8000-000000000003".into());
    keys.install(KeyringBundle {
        schema_version: 1,
        cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
        generation: BundleGeneration(1),
        peer_trust_roots: vec![ca.der().to_vec()],
        cache_keys: vec![CacheEncryptionKey {
            key: CacheKeyRef {
                cache: cache.clone(),
                id: KeyId::from_generation(1, 1).unwrap(),
                purpose: CacheKeyPurpose::Page,
            },
            state: CacheKeyState::Active,
            material: [7; 32],
        }],
    })
    .unwrap();
    keys
}

#[test]
fn abandoned_future_retains_buffers_key_and_permit_until_reaped() {
    use crate::{
        memory::pool::BufferPool,
        model::{ResourceClass, *},
        runtime::admission::Admission,
    };
    use std::{
        rc::Rc,
        time::{Duration, Instant},
    };
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let cache = CacheId("cache".into());
    let pool = BufferPool::new(admission.clone());
    let plaintext = pool
        .plaintext(
            admission
                .reserve(Some(&cache), ResourceClass::Plaintext, 3)
                .unwrap(),
            3,
        )
        .unwrap();
    let input = CryptoInput::Encrypt {
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        },
        plaintext,
        ciphertext: admission
            .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
            .unwrap(),
    };
    let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
    let client = CryptoClient::new(io);
    let scope =
        RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
    let mut operation = client.execute(input, key(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    struct Count(std::sync::atomic::AtomicUsize);
    impl std::task::Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    let count = Arc::new(Count(std::sync::atomic::AtomicUsize::new(0)));
    let driver = Waker::from(count.clone());
    client.register_driver(&driver);
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    drop(operation);
    assert_eq!(client.outstanding(), 1);
    assert_eq!(admission.used(ResourceClass::Plaintext), 3);
    let job = match engine.poll_job(&mut cx) {
        Poll::Ready(Ok(Some(job))) => job,
        _ => panic!("accepted job"),
    };
    let CryptoJob {
        permit,
        input,
        key,
        scope: _,
    } = job;
    assert!(
        engine
            .complete(CryptoCompletion {
                permit,
                outcome: CryptoOutcome::Failed {
                    input,
                    error: Error::Cancelled
                },
                _key: key,
            })
            .is_ok()
    );
    assert_eq!(client.outstanding(), 1);
    assert_eq!(admission.used(ResourceClass::Plaintext), 3);
    client.poll_budgeted(1).unwrap();
    assert!(
        count.0.load(Ordering::Relaxed) > 0,
        "abandoned operation must not steal driver wake"
    );
    assert_eq!(client.outstanding(), 0);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn engine_drop_reclaims_queued_jobs_and_drain_finishes() {
    use crate::{
        memory::pool::BufferPool,
        model::{ResourceClass, *},
        runtime::admission::Admission,
    };
    use std::{
        rc::Rc,
        time::{Duration, Instant},
    };
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let cache = CacheId("cache".into());
    let plaintext = BufferPool::new(admission.clone())
        .plaintext(
            admission
                .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                .unwrap(),
            1,
        )
        .unwrap();
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: cache.clone(),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        },
        number: PageNumber(0),
    };
    let ciphertext = admission
        .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
        .unwrap();
    let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
    let client = CryptoClient::new(io);
    let scope =
        RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
    let mut operation = client.execute(
        CryptoInput::Encrypt {
            page,
            plaintext,
            ciphertext,
        },
        key(),
        &scope,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    drop(engine);
    client.poll_budgeted(1).unwrap();
    assert!(matches!(
        operation.as_mut().poll(&mut cx),
        Poll::Ready(Err(_))
    ));
    drop(operation);
    assert_eq!(client.outstanding(), 0);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert!(client.drain(&scope).as_mut().poll(&mut cx).is_ready());
}

#[test]
fn original_scope_failure_is_returned_without_submission() {
    use crate::{
        memory::pool::BufferPool,
        model::{ResourceClass, *},
        runtime::admission::Admission,
    };
    use std::{rc::Rc, time::Instant};
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let cache = CacheId("cache".into());
    let plaintext = BufferPool::new(admission.clone())
        .plaintext(
            admission
                .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                .unwrap(),
            1,
        )
        .unwrap();
    let ciphertext = admission
        .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
        .unwrap();
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache,
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        },
        number: PageNumber(0),
    };
    let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
    let client = CryptoClient::new(io);
    let scope = RequestScope::new(RequestId([0; 16]), Instant::now()).unwrap();
    let result = futures::executor::block_on(client.execute(
        CryptoInput::Encrypt {
            page,
            plaintext,
            ciphertext,
        },
        key(),
        &scope,
    ));
    assert!(matches!(result, Err(Error::DeadlineExceeded)));
    assert_eq!(client.outstanding(), 0);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert!(
        engine
            .poll_job(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
}

#[test]
fn only_owned_messages_and_endpoints_cross_threads() {
    fn send<T: Send + 'static>() {}
    send::<CryptoInput>();
    send::<CryptoOutput>();
    send::<CryptoJob>();
    send::<CryptoCompletion>();
    send::<CryptoPermit>();
    send::<IoCryptoPort>();
    send::<CryptoPort>();
}

#[test]
fn endpoints_enforce_pair_capacity_generation_and_close() {
    let (io, engine) = pair(WorkerId(7), 12, NonZeroUsize::new(3).unwrap());
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for (worker, generation) in [(WorkerId(8), 12), (WorkerId(7), 13)] {
        assert!(matches!(
            io.poll_reserve(
                &mut cx,
                CryptoId {
                    worker,
                    generation,
                    sequence: 0
                }
            ),
            Poll::Ready(Err(Error::StaleFlight))
        ));
    }
    let id = |sequence| CryptoId {
        worker: WorkerId(7),
        generation: 12,
        sequence,
    };
    let mut permits = Vec::new();
    for sequence in 0..3 {
        let Poll::Ready(Ok(permit)) = io.poll_reserve(&mut cx, id(sequence)) else {
            panic!("available permit")
        };
        permits.push(permit);
    }
    assert!(io.poll_reserve(&mut cx, id(3)).is_pending());
    drop(permits.pop());
    assert!(matches!(
        io.poll_reserve(&mut cx, id(3)),
        Poll::Ready(Ok(_))
    ));
    drop(engine);
    assert!(matches!(
        io.poll_reserve(&mut cx, id(4)),
        Poll::Ready(Err(Error::Unavailable))
    ));
}
