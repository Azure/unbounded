//! Owned page-AEAD handoff between one I/O shard and its crypto engine endpoint.
//! Multiple endpoints can share a NUMA-local crypto execution thread.
//!
//! Queue ownership and completion admission are independent of waiting futures.
//! Admission stays on I/O. Reserve a job slot AND its eventual completion slot
//! before enqueueing. The permit follows the job through completion consumption,
//! so cancellation, an abandoned future, or shutdown cannot reclaim its capacity,
//! buffers, or key early. Completion publication never waits for new admission.
//!
//! All polling must register the waker and recheck state before returning Pending.
//! Enqueue wakes crypto; completion wakes I/O; consumption wakes capacity waiters;
//! close wakes both sides. Neither side spins or blocks awaiting the other. This
//! is required even when both OS threads share one allowed CPU.
//!
//! Queue timing measures accepted submission to crypto dequeue, excluding permit
//! waits and execution. I/O records each measured execution once on completion
//! reap, including failed, canceled, or abandoned work reaching that path. Queued
//! or in-flight work and engine loss without completion are not yet counted.
//! The racer_crypto_{encrypt,decrypt}_queue_nanoseconds_{sum,count} counters yield
//! mean wait in milliseconds as rate(sum) / rate(count) / 1e6. For a fleet mean,
//! sum each rate across instances before dividing. Zero count rate has no defined
//! mean; these counters provide neither percentiles nor current queue age or total
//! request latency.

#[cfg(test)]
mod measurement {
    //! Measurement correctness scenarios using the real page engine.
    use super::tests::input;
    use super::tests::keyring;
    use super::*;
    use crate::telemetry::Event;
    use crate::telemetry::Event::*;
    use crate::telemetry::Metrics;
    use crate::memory::BufferPool;
    use crate::model::Nonce;
    use crate::model::PageEnvelope;
    use crate::model::ResourceClass;
    use crate::model::*;
    use crate::security::PageCryptoEngine;
    use crate::security::page_aad;
    use racer_crypto::aead;
    use std::rc::Rc;
    use std::time::Duration;
    use std::time::Instant;
    use uring_runtime::reactor::IoBuffer;

    fn page() -> PageId {
        PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("00000000-0000-4000-8000-000000000003".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("measurement"),
            },
            number: PageNumber(0),
        }
    }
    fn envelope(size: usize) -> PageEnvelope {
        PageEnvelope {
            page: page(),
            key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
            nonce: Nonce([2; 24]),
            plaintext_length: size as u32,
            ciphertext_length: size as u32 + 16,
        }
    }

    fn validate(result: &CryptoCompletion, size: usize) {
        if let CryptoOutcome::Completed(output) = &result.outcome {
            let (CryptoOutput::Encrypted(plain, ciphertext)
            | CryptoOutput::Decrypted(plain, ciphertext)) = output
            else {
                panic!("AEAD measurement received checksum-only output");
            };
            assert_eq!(plain.bytes().len(), size);
            assert_eq!(ciphertext.bytes().len(), size + 16);
            assert_eq!(plain.bytes()[0], 7);
            assert_eq!(plain.bytes()[size - 1], 7);
        }
    }
    fn lifecycle(size: usize, paired: bool, operation: &str) {
        let inputs = MeasurementInputs::new(size, operation);
        let admission = &inputs.admission;
        let cache = &inputs.page.version.object.cache;
        let keys = tests::keyring();
        let iterations = 16;
        let concurrency = if paired { 8 } else { 1 };
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(concurrency).unwrap());
        let thread = if paired {
            Some(std::thread::spawn(move || {
                loop {
                    let job = futures::executor::block_on(futures::future::poll_fn(|cx| {
                        engine.poll_job(cx)
                    }))
                    .unwrap();
                    let Some(job) = job else { break };
                    assert!(engine.complete(PageCryptoEngine::process(job)).is_ok());
                }
            }))
        } else {
            None
        };
        let scope = RequestScope::new(
            RequestId([0; 16]),
            Instant::now() + Duration::from_secs(240),
        )
        .unwrap();
        let mut failures = 0;
        let mut completed = 0;
        for i in 0..iterations {
            let input = inputs.input(operation);
            let permit = futures::executor::block_on(futures::future::poll_fn(|cx| {
                io.poll_reserve(
                    cx,
                    CryptoId {
                        worker: WorkerId(0),
                        generation: 0,
                        sequence: i as u64,
                    },
                )
            }))
            .unwrap();
            let job = permit.job(
                input,
                keys.active(cache, racer_identity::KeyPurpose::Page)
                    .unwrap(),
                scope.clone(),
            );
            if paired {
                assert!(io.try_submit(job).is_ok());
            } else {
                let result = PageCryptoEngine::process(job);
                validate(&result, size);
                failures += usize::from(matches!(result.outcome, CryptoOutcome::Failed { .. }));
                drop(result);
                completed += 1;
            }
            if paired && (i + 1 - completed == concurrency || i + 1 == iterations) {
                while completed <= i {
                    let result = futures::executor::block_on(futures::future::poll_fn(|cx| {
                        io.poll_completion(cx)
                    }))
                    .unwrap()
                    .unwrap();
                    validate(&result, size);
                    failures += usize::from(matches!(result.outcome, CryptoOutcome::Failed { .. }));
                    drop(result);
                    completed += 1;
                }
            }
        }
        io.close_submissions().unwrap();
        if let Some(thread) = thread {
            thread.join().unwrap();
        }
        admission.reclaim_buffers();
        assert_eq!(
            failures,
            if operation == "bad_tag" {
                iterations
            } else {
                0
            }
        );
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn lifecycle_setup_preserves_success_failure_and_quota_cleanup() {
        for paired in [false, true] {
            for operation in ["encrypt", "decrypt", "bad_tag"] {
                lifecycle(4095, paired, operation);
            }
        }
    }

    /// Full client submission, waiter registration, real paired execution, I/O reap,
    /// result delivery, and cleanup for both encrypt and decrypt attribution.
    fn accounting_sample(size: usize, decrypt: bool, iterations: usize) {
        let operation = if decrypt { "decrypt" } else { "encrypt" };
        let inputs = MeasurementInputs::new(size, operation);
        let admission = &inputs.admission;
        let keys = tests::keyring();
        let cache = &inputs.page.version.object.cache;
        let metrics = Metrics::default();
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(8).unwrap());
        let client = CryptoClient::new(io);
        client.set_metrics(metrics.clone());
        let environment = uring_runtime::environment::Environment::current();
        let thread = std::thread::spawn(move || {
            let _env = environment.enter();
            while let Some(job) =
                futures::executor::block_on(futures::future::poll_fn(|cx| engine.poll_job(cx)))
                    .unwrap()
            {
                assert!(engine.complete(PageCryptoEngine::process(job)).is_ok());
            }
        });
        let scope = RequestScope::new(
            RequestId([0; 16]),
            uring_runtime::environment::now() + Duration::from_secs(240),
        )
        .unwrap();
        for batch in (0..iterations).step_by(8) {
            let operations = (batch..(batch + 8).min(iterations)).map(|_| async {
                let input = inputs.input(operation);
                let output = client
                    .execute(
                        input,
                        keys.active(cache, racer_identity::KeyPurpose::Page)
                            .unwrap(),
                        &scope,
                    )
                    .await
                    .unwrap();
                let (CryptoOutput::Encrypted(plain, cipher)
                | CryptoOutput::Decrypted(plain, cipher)) = &output
                else {
                    panic!("AEAD measurement received checksum-only output");
                };
                assert_eq!(plain.bytes().len(), size);
                assert_eq!((plain.bytes()[0], plain.bytes()[size - 1]), (7, 7));
                assert_eq!(cipher.bytes().len(), size + 16);
                drop(output);
            });
            let mut all = Box::pin(futures::future::join_all(operations));
            futures::executor::block_on(futures::future::poll_fn(|cx| {
                client.register_driver(cx.waker());
                client.poll_budgeted(8).unwrap();
                std::future::Future::poll(all.as_mut(), cx)
            }));
        }
        client.close_submissions().unwrap();
        thread.join().unwrap();
        admission.reclaim_buffers();
        assert_eq!(client.outstanding(), 0);
        assert!(client.waiters.borrow().is_empty());
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        let events = measurement_events(decrypt);
        for (event, expected) in events.into_iter().zip([
            iterations as u64,
            iterations as u64,
            0,
            (iterations * size) as u64,
            iterations as u64,
            metrics.count(events[5]),
            iterations as u64,
            metrics.count(events[7]),
        ]) {
            assert_eq!(metrics.count(event), expected);
        }
        if uring_runtime::environment::simulation_seed().is_none() {
            assert!(metrics.count(events[5]) > 0);
            assert!(metrics.count(events[7]) > 0);
        }
    }

    #[test]
    fn attribution_preserves_client_cleanup_and_dst() {
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;
        let clock = SimulationClock::new(73);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        for decrypt in [false, true] {
            accounting_sample(63, decrypt, 16);
        }
    }

    #[test]
    fn measurements_account_once_at_reap_even_for_cancel_and_abandon() {
        use std::time::Duration;
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;
        let clock = SimulationClock::new(42);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let keys = keyring();
        for decrypt in [false, true] {
            for mode in [
                "success",
                "failure",
                "cancel",
                "abandon",
                "aead",
                "malformed",
                "short_body",
                "abandon_crc",
                "abandon_aead",
            ] {
                if !decrypt
                    && matches!(
                        mode,
                        "aead" | "malformed" | "short_body" | "abandon_crc" | "abandon_aead"
                    )
                {
                    continue;
                }
                let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                let client = CryptoClient::new(io);
                let metrics = Metrics::default();
                client.set_metrics(metrics.clone());
                let failures = crate::telemetry::Failures::default();
                client.set_failure_observer(failures.observer(WorkerId(0)));
                let scope = RequestScope::new(
                    crate::model::RequestId([0; 16]),
                    environment::now() + Duration::from_secs(10),
                )
                .unwrap();
                let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
                let lease = || {
                    keys.active(&cache, racer_identity::KeyPurpose::Page)
                        .unwrap()
                };
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let mut data = measurement_input(
                    &admission,
                    &cache,
                    lease(),
                    &scope,
                    decrypt,
                    matches!(mode, "failure" | "aead" | "abandon_crc" | "abandon_aead"),
                );
                if let CryptoInput::Decrypt { ciphertext, .. } = &mut data {
                    if matches!(mode, "aead" | "abandon_aead") {
                        // Peer bytes without a persisted CRC must still fail AEAD.
                        Arc::get_mut(&mut ciphertext.inner).unwrap().checksum =
                            std::sync::OnceLock::new();
                    }
                    if mode == "malformed" {
                        Arc::get_mut(&mut ciphertext.inner)
                            .unwrap()
                            .envelope
                            .ciphertext_length += 1;
                    }
                    if mode == "short_body" {
                        // A valid descriptor with a mismatched body is malformed,
                        // not an authentication rejection from the primitive.
                        let inner = Arc::get_mut(&mut ciphertext.inner).unwrap();
                        inner.checksum = std::sync::OnceLock::new();
                        inner.bytes.pop();
                    }
                }
                // Admission delay must not leak into residence.
                clock.advance(Duration::from_millis(100));
                let mut future = client.execute(data, lease(), &scope);
                assert!(future.as_mut().poll(&mut cx).is_pending());
                if mode == "cancel" {
                    scope.cancel().unwrap();
                }
                clock.advance(Duration::from_millis(3));
                let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                    panic!("job")
                };
                let mut completion = crate::security::PageCryptoEngine::process(job);
                // The engine uses virtual time (zero cost in DST). Explicitly
                // advance a measured interval to exercise the exact sum separately.
                assert_eq!(completion.permit.measurement.execution_ns, Some(0));
                let start = environment::now();
                clock.advance(Duration::from_millis(2));
                completion.permit.executed(start);
                let (_, mut wrong) = pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
                completion = wrong.complete(completion).err().unwrap().command;
                assert!(engine.complete(completion).is_ok());
                let events = measurement_events(decrypt);
                assert_eq!(metrics.count(events[0]), 0);
                assert_eq!(metrics.count(CryptoDecryptCrcRejected), 0);
                assert_eq!(metrics.count(CryptoDecryptAeadRejected), 0);
                if matches!(mode, "abandon" | "abandon_crc" | "abandon_aead") {
                    drop(future);
                } else {
                    client.poll_budgeted(1).unwrap();
                    match future.as_mut().poll(&mut cx) {
                        Poll::Ready(Ok(_)) => assert_eq!(mode, "success"),
                        Poll::Ready(Err(error)) => assert_eq!(
                            error,
                            if mode == "cancel" {
                                Error::Cancelled
                            } else if !decrypt {
                                Error::MissingKey
                            } else {
                                Error::CorruptRecord
                            }
                        ),
                        Poll::Pending => panic!("completion not returned"),
                    }
                }
                client.poll_budgeted(1).unwrap();
                client.poll_budgeted(1).unwrap();
                let success = u64::from(mode == "success" || mode == "abandon");
                for (event, expected) in events.into_iter().zip([
                    1,
                    success,
                    1 - success,
                    success,
                    1,
                    2_000_000,
                    1,
                    3_000_000,
                ]) {
                    assert_eq!(metrics.count(event), expected, "{mode} {event:?}");
                }
                assert_eq!(client.outstanding(), 0);
                assert_eq!(
                    metrics.count(CryptoDecryptCrcRejected),
                    u64::from(decrypt && matches!(mode, "failure" | "abandon_crc")),
                    "{mode}"
                );
                assert_eq!(
                    metrics.count(CryptoDecryptAeadRejected),
                    u64::from(matches!(mode, "aead" | "abandon_aead")),
                    "{mode}"
                );
                let mut records = String::new();
                failures.write_aead(&mut records).unwrap();
                let rejected = matches!(mode, "aead" | "abandon_aead");
                assert_eq!(records.lines().count(), 1 + usize::from(rejected), "{mode}");
                assert!(records.starts_with(if rejected {
                    "total=1 retained=1"
                } else {
                    "total=0 retained=0"
                }));
                if rejected {
                    assert!(!records.contains("crc=none"));
                }
            }
        }
    }

    #[test]
    fn send_crc_computes_fresh_body_and_holds_owner_through_abandoned_reap() {
        use crate::security::PageCryptoEngine;
        use crate::telemetry::Pair;
        use crate::telemetry::Samples;
        use uring_runtime::environment;
        let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let keys = keyring();
        let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
        let lease = || {
            keys.active(&cache, racer_identity::KeyPurpose::Page)
                .unwrap()
        };
        let scope = RequestScope::new(
            crate::model::RequestId([7; 16]),
            environment::now() + Duration::from_secs(10),
        )
        .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let (setup, _engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let Poll::Ready(Ok(permit)) = setup.poll_reserve(
            &mut cx,
            CryptoId {
                worker: WorkerId(0),
                generation: 0,
                sequence: 1,
            },
        ) else {
            panic!("permit")
        };
        let completion =
            PageCryptoEngine::process(permit.job(input(&admission), lease(), scope.clone()));
        let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
            completion.outcome
        else {
            panic!("encrypted")
        };
        drop(plain);
        Arc::get_mut(&mut ciphertext.inner).unwrap().checksum = std::sync::OnceLock::new();
        let expected = racer_crypto::crc64(ciphertext.bytes());
        let retained = ciphertext.clone();
        let samples = Samples::default();
        let filter = Pair::parse(
            "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
        )
        .unwrap();
        let (ticket, sample) = samples
            .begin(&filter, &filter.sender, &filter.receiver)
            .unwrap();
        ticket.finish(true);
        drop(ticket);
        let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let mut future = client.execute_sample(
            CryptoInput::Checksum { ciphertext },
            lease(),
            &scope,
            Some(sample),
        );
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(retained.cached_checksum(), None);
        drop(future);
        let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
            panic!("job")
        };
        let completion = PageCryptoEngine::process(job);
        assert_eq!(retained.cached_checksum(), Some(expected));
        assert!(engine.complete(completion).is_ok());
        let mut text = String::new();
        samples.write(&mut text).unwrap();
        assert!(text.contains("busy=1"));
        assert!(text.contains("status=pending"));
        client.poll_budgeted(1).unwrap();
        client.poll_budgeted(1).unwrap();
        text.clear();
        samples.write(&mut text).unwrap();
        assert!(text.contains("busy=0"));
        assert!(text.contains("send=completed status=computed"));
        assert!(text.contains(&format!("crc={expected:016x}")));
        assert_eq!(text.lines().count(), 2);
        assert_eq!(client.outstanding(), 0);
    }

    #[test]
    fn send_crc_cached_cancel_missing_key_and_capacity_are_diagnostic_only() {
        use crate::security::PageCrypto;
        use crate::security::PageCryptoEngine;
        use crate::telemetry::Pair;
        use crate::telemetry::Samples;
        let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let keys = std::rc::Rc::new(keyring());
        let filter = Pair::parse(
            "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
        )
        .unwrap();
        for mode in ["cached", "cancel", "missing", "capacity"] {
            let mut page = crate::memory::tests::bundle_for(
                &admission,
                crate::model::VersionMetadata {
                    content_type: None,
                    version: crate::model::ObjectVersion {
                        object: crate::model::ObjectId {
                            cache: crate::model::CacheId(
                                "00000000-0000-4000-8000-000000000003".into(),
                            ),
                            key: crate::model::CacheKey([3; 32]),
                        },
                        etag: crate::model::StrongEtag::test_value("send-crc"),
                    },
                    length: 3,
                },
            )
            .ciphertext;
            let inner = Arc::get_mut(&mut page.inner).unwrap();
            inner.envelope.page.version.object.cache =
                crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
            if mode == "cached" {
                inner.checksum.set(42).unwrap();
            }
            if mode == "missing" {
                // The fixture keyring installs [1;16], also the bundle's default.
                inner.envelope.key_id = crate::model::KeyId([99; 16]);
            }
            let cache = page.envelope().page.version.object.cache.clone();
            let lease = keys
                .active(&cache, racer_identity::KeyPurpose::Page)
                .unwrap();
            let scope = RequestScope::new(
                crate::model::RequestId([7; 16]),
                uring_runtime::environment::now() + Duration::from_secs(10),
            )
            .unwrap();
            let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
            let client = std::rc::Rc::new(CryptoClient::new(io));
            let samples = Samples::default();
            let (ticket, sample) = samples
                .begin(&filter, &filter.sender, &filter.receiver)
                .unwrap();
            ticket.finish(true);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            if mode == "missing" {
                let crypto = PageCrypto::new(keys.clone(), client.clone());
                let mut future = Box::pin(crypto.sample_send(page, &scope, sample));
                assert!(
                    future.as_mut().poll(&mut cx).is_ready(),
                    "missing-key work must not enqueue"
                );
            } else {
                let held = if mode == "capacity" {
                    let Poll::Ready(Ok(permit)) = client.port.poll_reserve(
                        &mut cx,
                        CryptoId {
                            worker: WorkerId(0),
                            generation: 1,
                            sequence: 0,
                        },
                    ) else {
                        panic!("permit")
                    };
                    Some(permit)
                } else {
                    None
                };
                let mut future = client.execute_sample(
                    CryptoInput::Checksum { ciphertext: page },
                    lease,
                    &scope,
                    Some(sample),
                );
                assert!(future.as_mut().poll(&mut cx).is_pending());
                if mode == "capacity" {
                    scope.cancel().unwrap();
                    assert!(matches!(
                        future.as_mut().poll(&mut cx),
                        Poll::Ready(Err(Error::Cancelled))
                    ));
                    drop(future);
                    drop(held);
                } else {
                    if mode == "cancel" {
                        scope.cancel().unwrap();
                    }
                    let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                        panic!("job")
                    };
                    let completion = PageCryptoEngine::process(job);
                    assert!(engine.complete(completion).is_ok());
                    client.poll_budgeted(1).unwrap();
                    assert!(future.as_mut().poll(&mut cx).is_ready());
                    drop(future);
                }
            }
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("busy=0"), "{mode}: {text}");
            assert!(text.contains("send=completed"));
            assert!(
                text.contains(if mode == "cached" {
                    "status=cached"
                } else {
                    "status=unavailable"
                }),
                "{mode}: {text}"
            );
            if mode == "cached" {
                assert!(text.contains("crc=000000000000002a"));
            }
            if mode == "missing" {
                assert!(text.contains("MissingKey"));
            }
        }
    }

    #[test]
    fn shared_worker_encrypt_queue_measurements() {
        shared_worker_queue_measurements(false);
    }

    #[test]
    fn shared_worker_decrypt_queue_measurements() {
        shared_worker_queue_measurements(true);
    }

    fn shared_worker_queue_measurements(decrypt: bool) {
        use crate::security::PageCryptoEngine;
        use std::time::Duration;
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;

        let clock = SimulationClock::new(43);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let keys = keyring();
        let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
        let lease = || {
            keys.active(&cache, racer_identity::KeyPurpose::Page)
                .unwrap()
        };
        let events = measurement_events(decrypt);

        for mode in ["success", "failure", "cancel", "abandon"] {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let scopes = [0, 1].map(|id| {
                RequestScope::new(
                    crate::model::RequestId([id; 16]),
                    environment::now() + Duration::from_secs(10),
                )
                .unwrap()
            });

            let (other_io, mut other_engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
            let (waiting_io, mut waiting_engine) =
                pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
            // Occupy capacity without submitting: the target really parks before
            // admission, rather than merely being created after a clock advance.
            let Poll::Ready(Ok(blocker)) = waiting_io.poll_reserve(
                &mut cx,
                CryptoId {
                    worker: WorkerId(1),
                    generation: 0,
                    sequence: 0,
                },
            ) else {
                panic!("capacity blocker")
            };
            let other = CryptoClient::new(other_io);
            let waiting = CryptoClient::new(waiting_io);
            let other_metrics = Metrics::default();
            let waiting_metrics = Metrics::default();
            other.set_metrics(other_metrics.clone());
            waiting.set_metrics(waiting_metrics.clone());
            let mut future = Some(waiting.execute(
                measurement_input(
                    &admission,
                    &cache,
                    lease(),
                    &scopes[1],
                    decrypt,
                    mode == "failure",
                ),
                lease(),
                &scopes[1],
            ));
            assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            assert_eq!(waiting.pending.borrow().len(), 1);
            assert!(waiting.waiters.borrow().is_empty());
            clock.advance(Duration::from_millis(100));
            assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            assert!(waiting_engine.poll_job(&mut cx).is_pending());
            drop(blocker);
            assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            assert!(waiting.pending.borrow().is_empty());
            assert_eq!(waiting.waiters.borrow().len(), 1);

            let mut other_future = other.execute(
                measurement_input(&admission, &cache, lease(), &scopes[0], decrypt, false),
                lease(),
                &scopes[0],
            );
            assert!(other_future.as_mut().poll(&mut cx).is_pending());
            if mode == "cancel" {
                scopes[1].cancel().unwrap();
                assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            } else if mode == "abandon" {
                drop(future.take());
            }

            // One execution service thread, manually interleaved: service the
            // other shard for 7ms before dequeuing the already-submitted target.
            // Real AEAD has zero virtual cost; set its measured service interval
            // explicitly, following the single-pair measurement test above.
            for (engine, worker, service_ms) in [
                (&mut other_engine, WorkerId(0), 7),
                (&mut waiting_engine, WorkerId(1), 5),
            ] {
                let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                    panic!("submitted job")
                };
                assert_eq!(job.id().worker, worker);
                let mut completion = PageCryptoEngine::process(job);
                assert_eq!(completion.permit.measurement.execution_ns, Some(0));
                let start = environment::now();
                clock.advance(Duration::from_millis(service_ms));
                completion.permit.executed(start);
                assert!(engine.complete(completion).is_ok());
            }
            // Neither the target's own 5ms execution nor this 11ms delay before
            // I/O reaping belongs in its submit-to-dequeue queue measurement.
            clock.advance(Duration::from_millis(11));
            for event in events {
                assert_eq!(other_metrics.count(event), 0, "before reap {event:?}");
                assert_eq!(waiting_metrics.count(event), 0, "before reap {event:?}");
            }
            other.poll_budgeted(1).unwrap();
            for event in events {
                assert_eq!(
                    waiting_metrics.count(event),
                    0,
                    "other shard reaped {event:?}"
                );
            }
            waiting.poll_budgeted(1).unwrap();
            assert!(matches!(
                other_future.as_mut().poll(&mut cx),
                Poll::Ready(Ok(_))
            ));
            if let Some(mut future) = future {
                match (mode, future.as_mut().poll(&mut cx)) {
                    ("success", Poll::Ready(Ok(_)))
                    | ("failure", Poll::Ready(Err(_)))
                    | ("cancel", Poll::Ready(Err(Error::Cancelled))) => {}
                    _ => panic!("unexpected {mode} result"),
                }
            }
            assert_shared_measurements(
                &other,
                &waiting,
                &other_metrics,
                &waiting_metrics,
                decrypt,
                mode,
            );
        }
    }

    #[test]
    fn duration_saturates_without_host_time_in_dst() {
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;
        let clock = SimulationClock::new(19);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        let start = environment::now();
        clock.advance(std::time::Duration::from_secs(u64::MAX / 1_000_000_000 + 1));
        assert_eq!(elapsed_ns(start), u64::MAX);
    }

    fn measurement_events(decrypt: bool) -> [Event; 8] {
        if decrypt {
            [
                CryptoDecryptStarted,
                CryptoDecryptSuccess,
                CryptoDecryptFailure,
                CryptoDecryptBytes,
                CryptoDecryptExecutionCount,
                CryptoDecryptExecutionNs,
                CryptoDecryptQueueCount,
                CryptoDecryptQueueNs,
            ]
        } else {
            [
                CryptoEncryptStarted,
                CryptoEncryptSuccess,
                CryptoEncryptFailure,
                CryptoEncryptBytes,
                CryptoEncryptExecutionCount,
                CryptoEncryptExecutionNs,
                CryptoEncryptQueueCount,
                CryptoEncryptQueueNs,
            ]
        }
    }

    fn measurement_input(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        cache: &CacheId,
        key: KeyLease,
        scope: &RequestScope,
        decrypt: bool,
        corrupt: bool,
    ) -> CryptoInput {
        let mut data = input(admission);
        if decrypt {
            let (setup, _setup_engine) = pair(WorkerId(2), 0, NonZeroUsize::new(1).unwrap());
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let Poll::Ready(Ok(permit)) = setup.poll_reserve(
                &mut cx,
                CryptoId {
                    worker: WorkerId(2),
                    generation: 0,
                    sequence: 0,
                },
            ) else {
                panic!("setup permit")
            };
            let result = PageCryptoEngine::process(permit.job(data, key, scope.clone()));
            let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
                result.outcome
            else {
                panic!("setup encryption")
            };
            drop(plain);
            if corrupt {
                Arc::get_mut(&mut ciphertext.inner).unwrap().bytes[0] ^= 1;
            }
            CryptoInput::Decrypt {
                ciphertext,
                plaintext: admission
                    .reserve(Some(&cache), crate::model::ResourceClass::Plaintext, 1)
                    .unwrap(),
            }
        } else {
            if corrupt {
                if let CryptoInput::Encrypt { page, .. } = &mut data {
                    page.version.object.cache.0 = "wrong".into();
                }
            }
            data
        }
    }

    fn assert_shared_measurements(
        other: &CryptoClient,
        waiting: &CryptoClient,
        other_metrics: &Metrics,
        waiting_metrics: &Metrics,
        decrypt: bool,
        mode: &str,
    ) {
        let events = measurement_events(decrypt);
        let opposite_queue = if decrypt {
            [CryptoEncryptQueueCount, CryptoEncryptQueueNs]
        } else {
            [CryptoDecryptQueueCount, CryptoDecryptQueueNs]
        };
        let success = u64::from(mode == "success" || mode == "abandon");
        // Repeated drains must not duplicate either shard's observations,
        // even when delivery was canceled or abandoned before execution.
        for _ in 0..3 {
            other.poll_budgeted(1).unwrap();
            waiting.poll_budgeted(1).unwrap();
            for ((event, other_expected), waiting_expected) in events
                .into_iter()
                .zip([1, 1, 0, 1, 1, 7_000_000, 1, 0])
                .zip([1, success, 1 - success, success, 1, 5_000_000, 1, 7_000_000])
            {
                assert_eq!(
                    other_metrics.count(event),
                    other_expected,
                    "other {mode} {event:?}"
                );
                assert_eq!(
                    waiting_metrics.count(event),
                    waiting_expected,
                    "waiting {mode} {event:?}"
                );
            }
            for event in opposite_queue {
                assert_eq!(other_metrics.count(event), 0);
                assert_eq!(waiting_metrics.count(event), 0);
            }
            assert_eq!(other.outstanding(), 0);
            assert_eq!(waiting.outstanding(), 0);
        }
    }

    struct MeasurementInputs {
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        pool: BufferPool,
        page: PageId,
        source: Vec<u8>,
        descriptor: PageEnvelope,
        encrypted: Vec<u8>,
        size: usize,
    }

    impl MeasurementInputs {
        fn new(size: usize, operation: &str) -> Self {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.plaintext_bytes = NonZeroUsize::new(512 * 1024 * 1024).unwrap();
            limits.ciphertext_bytes = limits.plaintext_bytes;
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let pool = BufferPool::new(admission.clone());
            let page = page();
            let source = vec![7u8; size];
            let descriptor = envelope(size);
            let mut encrypted = vec![0; size + aead::TAG_LEN];
            aead::seal(
                &[7; 32],
                &descriptor.nonce.0,
                &page_aad(&descriptor).unwrap(),
                &source,
                &mut encrypted,
            )
            .unwrap();
            if operation == "bad_tag" {
                encrypted[size] ^= 1;
            }
            Self {
                admission,
                pool,
                page,
                source,
                descriptor,
                encrypted,
                size,
            }
        }

        fn input(&self, operation: &str) -> CryptoInput {
            let Self {
                admission,
                pool,
                page,
                source,
                descriptor,
                encrypted,
                size,
            } = self;
            let size = *size;
            let cache = &page.version.object.cache;
            if operation == "encrypt" {
                let mut plain = pool
                    .plaintext(
                        admission
                            .reserve(Some(cache), ResourceClass::Plaintext, size)
                            .unwrap(),
                        size,
                    )
                    .unwrap();
                plain.bytes_mut().unwrap().copy_from_slice(source);
                CryptoInput::Encrypt {
                    page: page.clone(),
                    plaintext: plain,
                    ciphertext: admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, size + 16)
                        .unwrap(),
                }
            } else {
                // Fresh received allocation/checksum state, as on disk/peer ingress.
                let bytes = encrypted;
                let ciphertext = pool
                    .ciphertext(
                        admission
                            .reserve(Some(cache), ResourceClass::Ciphertext, bytes.len())
                            .unwrap(),
                        descriptor.clone(),
                        bytes.clone(),
                    )
                    .unwrap();
                CryptoInput::Decrypt {
                    ciphertext,
                    plaintext: admission
                        .reserve(Some(cache), ResourceClass::Plaintext, size)
                        .unwrap(),
                }
            }
        }
    }
}

use crate::security::*;
#[cfg(test)]
mod channel_tests {
    #[cfg(test)]
    mod tests {
        use std::task::Context;
        use std::task::Poll;
        use uring_runtime::Error;
        use uring_runtime::channel::*;

        #[test]
        fn adapter_maps_errors_without_losing_command_ownership() {
            assert!(matches!(bounded::<u8>(0), Err(Error::InvalidConfiguration)));
            let (sender, mut receiver) = bounded(1).unwrap();
            assert!(sender.try_send(String::from("first")).is_ok());
            let failure = sender.try_send(String::from("second")).err().unwrap();
            assert_eq!(failure.error, Error::Overloaded);
            assert_eq!(failure.command, "second");
            assert_eq!(receiver.receive().unwrap().as_deref(), Some("first"));
            drop(receiver);
            let failure = sender.try_send(failure.command).err().unwrap();
            assert_eq!(failure.error, Error::Unavailable);
            assert_eq!(failure.command, "second");
            assert_eq!(
                sender.poll_ready(&mut Context::from_waker(futures::task::noop_waker_ref())),
                Poll::Ready(Err(Error::Unavailable))
            );
            assert!(sender.discard_closed());
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_support::WakeCounter;

    struct Fixture {
        admission: std::rc::Rc<flow_control::Quotas<AdmissionPolicy>>,
        client: CryptoClient,
        engine: CryptoPort,
        scope: RequestScope,
    }

    impl Fixture {
        fn new() -> Self {
            let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
            Self {
                admission,
                client: CryptoClient::new(io),
                engine,
                scope: RequestScope::new(
                    crate::model::RequestId([0; 16]),
                    uring_runtime::environment::now() + std::time::Duration::from_secs(5),
                )
                .unwrap(),
            }
        }
    }

    pub(super) fn input(
        admission: &std::rc::Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> CryptoInput {
        use crate::memory::BufferPool;
        use crate::model::ResourceClass;
        use crate::model::*;
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
        let completion = crate::security::PageCryptoEngine::process(job);
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
        let clock = uring_runtime::environment::SimulationClock::new(91);
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
        let completion = crate::security::PageCryptoEngine::process(job);
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
                racer_identity::KeyPurpose::Page,
            )
            .unwrap()
    }

    pub(super) fn keyring() -> racer_identity::Keyring {
        use crate::model::CacheId;
        use crate::model::ClusterId;
        use crate::model::NodeId;
        use racer_control_wire::*;
        use racer_identity::KeyEpochs;
        use racer_identity::Keyring;
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
            cache_keys: vec![CacheEncryptionKey::new(
                CacheKeyRef {
                    cache: cache.clone(),
                    id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    purpose: CacheKeyPurpose::Page,
                },
                CacheKeyState::Active,
                zeroize::Zeroizing::new([7; 32]),
            )],
        })
        .unwrap();
        keys
    }

    #[test]
    fn abandoned_future_retains_buffers_key_and_permit_until_reaped() {
        use crate::memory::BufferPool;
        use crate::model::ResourceClass;
        use crate::model::*;
        use std::rc::Rc;
        use std::time::Duration;
        use std::time::Instant;
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
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
        use crate::memory::BufferPool;
        use crate::model::ResourceClass;
        use crate::model::*;
        use std::rc::Rc;
        use std::time::Duration;
        use std::time::Instant;
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
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
        use crate::memory::BufferPool;
        use crate::model::ResourceClass;
        use crate::model::*;
        use std::rc::Rc;
        use std::time::Instant;
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
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
}
