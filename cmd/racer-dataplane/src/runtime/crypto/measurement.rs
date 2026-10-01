//! Measurement scenarios and opt-in benchmarks using the real page engine.
use super::tests::{input, keyring};
use super::*;
use crate::telemetry::metrics::{Event, Event::*, Metrics};
use crate::{
    memory::pool::BufferPool,
    model::{KeyId, Nonce, PageEnvelope, ResourceClass, *},
    runtime::{admission::Admission, reactor::IoBuffer},
    security::{
        aead::{PageCryptoEngine, page_aad},
        crc64,
    },
};
use chacha20poly1305::{
    KeyInit, XChaCha20Poly1305,
    aead::{AeadInOut, inout::InOutBuf},
};
use std::{
    hint::black_box,
    rc::Rc,
    time::{Duration, Instant},
};

fn command(program: &str, args: &[&str]) -> String {
    let output = std::process::Command::new("timeout")
        .args(["--signal=TERM", "--kill-after=10s", "10s", program])
        .args(args)
        .output()
        .unwrap();
    assert!(output.status.success());
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}

const ROTATING: usize = 128 * 1024 * 1024;
fn cpu_ns() -> u64 {
    let mut time = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // Process CPU includes both real handoff threads, not just the caller.
    assert_eq!(
        unsafe { libc::clock_gettime(libc::CLOCK_PROCESS_CPUTIME_ID, &mut time) },
        0
    );
    time.tv_sec as u64 * 1_000_000_000 + time.tv_nsec as u64
}
fn report(
    layer: &str,
    operation: &str,
    size: usize,
    rotating: bool,
    iterations: usize,
    start: Instant,
    cpu: u64,
    failures: usize,
    concurrency: usize,
) {
    println!(
        "CRYPTO_MEASURE {}",
        serde_json::json!({
            "layer": layer, "operation": operation, "bytes": size,
            "input": if rotating { "rotating" } else { "reused" },
        "source_span_bytes": if rotating { ROTATING } else { size },
            "iterations": iterations, "failures": failures, "concurrency": concurrency,
            "elapsed_ns": start.elapsed().as_nanos(), "process_cpu_ns": cpu_ns() - cpu,
        "cleanup_inclusive": layer != "primitive",
        "success_bytes": (iterations - failures) as u64 * size as u64
        })
    );
}
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
        key_id: KeyId::from_generation(1, 1).unwrap(),
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
fn primitive(size: usize, rotating: bool, out_of_place: bool) {
    let slots = if rotating { ROTATING.div_ceil(size) } else { 1 };
    let iterations = if rotating {
        slots
    } else {
        (16 * 1024 * 1024 / size).max(8)
    };
    let mut buffers: Vec<u8> = vec![7; slots * size];
    let cipher = XChaCha20Poly1305::new((&[7; 32]).into());
    let aad = page_aad(&envelope(size)).unwrap();
    let nonce = (&[2; 24]).into();
    let mut scratch = vec![0; size];
    for operation in ["crc", "encrypt", "decrypt", "bad_tag"] {
        if out_of_place && operation == "crc" {
            continue;
        }
        let mut tags = Vec::new();
        if operation == "decrypt" || operation == "bad_tag" {
            for bytes in buffers.chunks_exact_mut(size) {
                let mut tag = cipher
                    .encrypt_inout_detached(nonce, &aad, bytes.into())
                    .unwrap();
                if operation == "bad_tag" {
                    tag[0] ^= 1;
                }
                tags.push(tag);
            }
        }
        let start = Instant::now();
        let cpu = cpu_ns();
        let mut failures = 0;
        for i in 0..iterations {
            let index = i % slots;
            let bytes = &mut buffers[index * size..(index + 1) * size];
            match operation {
                "crc" => {
                    black_box(crc64::checksum(black_box(bytes)));
                }
                "encrypt" => {
                    let buffer = if out_of_place {
                        InOutBuf::new(bytes, &mut scratch).unwrap()
                    } else {
                        bytes.into()
                    };
                    let tag = black_box(
                        cipher
                            .encrypt_inout_detached(nonce, &aad, black_box(buffer))
                            .unwrap(),
                    );
                    // Validation outside timing is handled by the engine cases;
                    // keep the complete detached tag observable here.
                    black_box(tag);
                }
                _ => {
                    // Preserve the baseline copy-inclusive case separately from
                    // the detached immutable-input path used by the page engine.
                    let buffer = if out_of_place {
                        InOutBuf::new(bytes, &mut scratch).unwrap()
                    } else {
                        scratch.copy_from_slice(bytes);
                        scratch.as_mut_slice().into()
                    };
                    failures += usize::from(
                        cipher
                            .decrypt_inout_detached(nonce, &aad, black_box(buffer), &tags[index])
                            .is_err(),
                    );
                }
            }
        }
        report(
            "primitive",
            if out_of_place {
                match operation {
                    "encrypt" => "encrypt_inout",
                    "decrypt" => "decrypt_inout",
                    _ => "bad_tag_inout",
                }
            } else if operation == "decrypt" {
                "decrypt_copy"
            } else {
                operation
            },
            size,
            rotating,
            iterations,
            start,
            cpu,
            failures,
            1,
        );
        assert_eq!(
            failures,
            if operation == "bad_tag" {
                iterations
            } else {
                0
            }
        );
    }
}

fn lifecycle(size: usize, rotating: bool, paired: bool, operation: &str) {
    let inputs = MeasurementInputs::new(size, rotating, operation);
    let admission = &inputs.admission;
    let cache = &inputs.page.version.object.cache;
    let keys = tests::keyring();
    let iterations = if rotating {
        ROTATING.div_ceil(size).min(4096)
    } else {
        (4 * 1024 * 1024 / size).clamp(8, 4096)
    };
    let concurrency = if paired { 8 } else { 1 };
    let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(concurrency).unwrap());
    let thread = if paired {
        Some(std::thread::spawn(move || {
            loop {
                let job =
                    futures::executor::block_on(futures::future::poll_fn(|cx| engine.poll_job(cx)))
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
    let start = Instant::now();
    let cpu = cpu_ns();
    let mut failures = 0;
    let mut completed = 0;
    for i in 0..iterations {
        let input = inputs.input(i, operation);
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
            keys.active(cache, crate::security::identity::KeyPurpose::Page)
                .unwrap(),
            scope.clone(),
        );
        if paired {
            assert!(io.try_submit(job).is_ok());
        } else {
            let result = PageCryptoEngine::process(job);
            validate(&result, size);
            failures += usize::from(matches!(result.outcome, CryptoOutcome::Failed { .. }));
            black_box(&result);
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
                black_box(&result);
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
    report(
        if paired { "paired" } else { "engine" },
        operation,
        size,
        rotating,
        iterations,
        start,
        cpu,
        failures,
        concurrency,
    );
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
            lifecycle(4095, false, paired, operation);
        }
    }
}

#[test]
#[ignore = "release-only bounded crypto measurement"]
fn crypto_measurement() {
    assert!(!cfg!(debug_assertions), "run with --release");
    provenance();
    for size in [63, 4095, 1024 * 1024, 16 * 1024 * 1024] {
        for rotating in [false, true] {
            primitive(size, rotating, false);
            for paired in [false, true] {
                for operation in ["encrypt", "decrypt", "bad_tag"] {
                    lifecycle(size, rotating, paired, operation);
                }
            }
        }
    }
}

fn provenance() {
    println!(
        "CRYPTO_PROVENANCE {}",
        serde_json::json!({
            "arch": std::env::consts::ARCH, "os": std::env::consts::OS,
            "profile": "release", "rotating_bytes": ROTATING,
        "revision": command("git", &["rev-parse", "HEAD"]),
        "dirty": !command("git", &["status", "--porcelain"]).is_empty(),
        "compiler": command("rustc", &["-Vv"]),
        "lockfile_sha256": command("sha256sum", &["Cargo.lock"]),
        "affinity": std::fs::read_to_string("/proc/self/status").unwrap().lines().filter(|l| l.starts_with("Cpus_allowed_list") || l.starts_with("Mems_allowed_list")).collect::<Vec<_>>(),
            "rustflags": std::env::var("RUSTFLAGS").unwrap_or_default(),
            "cpuinfo": std::fs::read_to_string("/proc/cpuinfo").unwrap().lines().filter(|l| l.starts_with("model name") || l.starts_with("flags")).take(2).collect::<Vec<_>>(),
        })
    );
}

#[test]
#[ignore = "release-only detached immutable-input supplement to the 80-case baseline matrix"]
fn crypto_measurement_inout() {
    assert!(!cfg!(debug_assertions), "run with --release");
    for size in [63, 4095, 1024 * 1024, 16 * 1024 * 1024] {
        for rotating in [false, true] {
            primitive(size, rotating, true);
        }
    }
}

/// Full client submission, waiter registration, real paired execution, I/O reap,
/// result delivery, and cleanup. Only attribution differs between the two modes.
fn accounting_sample(size: usize, decrypt: bool, enabled: bool, iterations: usize) -> (u64, u64) {
    let operation = if decrypt { "decrypt" } else { "encrypt" };
    let inputs = MeasurementInputs::new(size, false, operation);
    let admission = &inputs.admission;
    let keys = tests::keyring();
    let cache = &inputs.page.version.object.cache;
    let metrics = Metrics::default();
    let (mut io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(8).unwrap());
    io.attribution_disabled = !enabled;
    let client = CryptoClient::new(io);
    client.set_metrics(metrics.clone());
    let environment = crate::runtime::environment::Environment::current();
    let thread = std::thread::spawn(move || {
        let _env = environment.enter();
        while let Some(job) =
            futures::executor::block_on(futures::future::poll_fn(|cx| engine.poll_job(cx))).unwrap()
        {
            assert!(engine.complete(PageCryptoEngine::process(job)).is_ok());
        }
    });
    let scope = RequestScope::new(
        RequestId([0; 16]),
        crate::runtime::environment::now() + Duration::from_secs(240),
    )
    .unwrap();
    let cpu = cpu_ns();
    let start = Instant::now();
    for batch in (0..iterations).step_by(8) {
        let operations = (batch..(batch + 8).min(iterations)).map(|_| async {
            let input = inputs.input(0, operation);
            let output = client
                .execute(
                    input,
                    keys.active(cache, crate::security::identity::KeyPurpose::Page)
                        .unwrap(),
                    &scope,
                )
                .await
                .unwrap();
            let (CryptoOutput::Encrypted(plain, cipher) | CryptoOutput::Decrypted(plain, cipher)) =
                &output
            else {
                panic!("AEAD measurement received checksum-only output");
            };
            assert_eq!(plain.bytes().len(), size);
            assert_eq!((plain.bytes()[0], plain.bytes()[size - 1]), (7, 7));
            assert_eq!(cipher.bytes().len(), size + 16);
            black_box(&output);
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
    let elapsed = start.elapsed().as_nanos() as u64;
    let cpu = cpu_ns() - cpu;
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
        assert_eq!(metrics.count(event), if enabled { expected } else { 0 });
    }
    if enabled && crate::runtime::environment::simulation_seed().is_none() {
        assert!(metrics.count(events[5]) > 0);
        assert!(metrics.count(events[7]) > 0);
    }
    (elapsed, cpu)
}

#[test]
fn attribution_toggle_preserves_client_cleanup_and_dst() {
    use crate::runtime::environment::{self, SimulationClock};
    let clock = SimulationClock::new(73);
    let env = clock.environment(0);
    let _env = env.enter();
    let _strict = environment::require_simulated();
    for enabled in [false, true] {
        for decrypt in [false, true] {
            accounting_sample(63, decrypt, enabled, 16);
        }
    }
}

#[test]
#[ignore = "release-only full-client attribution on/off comparison, eight alternating pairs"]
fn crypto_attribution_overhead() {
    assert!(!cfg!(debug_assertions), "run with --release");
    provenance();
    for size in [63, 16 * 1024 * 1024] {
        let iterations = if size == 63 { 32768 } else { 16 };
        for decrypt in [false, true] {
            // Warm both paths before collecting samples; order alternates below.
            for enabled in [false, true] {
                accounting_sample(size, decrypt, enabled, if size == 63 { 512 } else { 8 });
            }
            let mut ratios = Vec::new();
            for pair in 0..8 {
                let mut samples = [(0, 0); 2];
                for enabled in if pair % 2 == 0 {
                    [false, true]
                } else {
                    [true, false]
                } {
                    let sample = accounting_sample(size, decrypt, enabled, iterations);
                    samples[usize::from(enabled)] = sample;
                    println!(
                        "CRYPTO_ATTRIBUTION {}",
                        serde_json::json!({
                            "layer": "full_client_paired", "input": "reused", "bytes": size,
                            "operation": if decrypt { "decrypt" } else { "encrypt" },
                            "pair": pair, "instrumented_first": pair % 2 != 0,
                            "instrumented": enabled, "iterations": iterations, "concurrency": 8,
                            "cleanup_inclusive": true, "elapsed_ns": sample.0, "process_cpu_ns": sample.1,
                            "success_bytes": iterations * size, "failures": 0
                        })
                    );
                }
                ratios.push((
                    samples[1].0 as f64 / samples[0].0 as f64,
                    samples[1].1 as f64 / samples[0].1 as f64,
                ));
            }
            println!(
                "CRYPTO_ATTRIBUTION_RATIOS {}",
                serde_json::json!({
                    "bytes": size, "operation": if decrypt { "decrypt" } else { "encrypt" },
                    "on_over_off_elapsed_cpu": ratios
                })
            );
        }
    }
}

#[test]
fn measurements_account_once_at_reap_even_for_cancel_and_abandon() {
    use crate::runtime::environment::{self, SimulationClock};
    use std::time::Duration;
    let clock = SimulationClock::new(42);
    let env = clock.environment(0);
    let _env = env.enter();
    let _strict = environment::require_simulated();
    let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let keys = keyring();
    for decrypt in [false, true] {
        for mode in [
            "success",
            "failure",
            "cancel",
            "abandon",
            "aead",
            "malformed",
            "abandon_crc",
            "abandon_aead",
        ] {
            if !decrypt && matches!(mode, "aead" | "malformed" | "abandon_crc" | "abandon_aead") {
                continue;
            }
            let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
            let client = CryptoClient::new(io);
            let metrics = Metrics::default();
            client.set_metrics(metrics.clone());
            let failures = crate::telemetry::failures::Failures::default();
            client.set_failure_observer(failures.observer(WorkerId(0)));
            let scope = RequestScope::new(
                crate::model::RequestId([0; 16]),
                environment::now() + Duration::from_secs(10),
            )
            .unwrap();
            let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
            let lease = || {
                keys.active(&cache, crate::security::identity::KeyPurpose::Page)
                    .unwrap()
            };
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut data = input(&admission);
            if decrypt {
                let (setup, _setup_engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                let Poll::Ready(Ok(permit)) = setup.poll_reserve(
                    &mut cx,
                    CryptoId {
                        worker: WorkerId(0),
                        generation: 0,
                        sequence: 0,
                    },
                ) else {
                    panic!("permit")
                };
                let result = crate::security::aead::PageCryptoEngine::process(permit.job(
                    data,
                    lease(),
                    scope.clone(),
                ));
                let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
                    result.outcome
                else {
                    panic!("encrypted")
                };
                drop(plain);
                if matches!(mode, "failure" | "aead" | "abandon_crc" | "abandon_aead") {
                    let inner = Arc::get_mut(&mut ciphertext.inner).unwrap();
                    inner.bytes[0] ^= 1;
                    if matches!(mode, "aead" | "abandon_aead") {
                        // Peer bytes without a persisted CRC must still fail AEAD.
                        inner.checksum = std::sync::OnceLock::new();
                    }
                }
                if mode == "malformed" {
                    Arc::get_mut(&mut ciphertext.inner)
                        .unwrap()
                        .envelope
                        .ciphertext_length += 1;
                }
                data = CryptoInput::Decrypt {
                    ciphertext,
                    plaintext: admission
                        .reserve(Some(&cache), crate::model::ResourceClass::Plaintext, 1)
                        .unwrap(),
                };
            } else if mode == "failure" {
                if let CryptoInput::Encrypt { page, .. } = &mut data {
                    page.version.object.cache.0 = "wrong".into();
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
            let mut completion = crate::security::aead::PageCryptoEngine::process(job);
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
    use crate::runtime::environment;
    use crate::{
        security::aead::PageCryptoEngine,
        telemetry::send_crc::{Pair, Samples},
    };
    let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let keys = keyring();
    let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
    let lease = || {
        keys.active(&cache, crate::security::identity::KeyPurpose::Page)
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
    let expected = crate::security::crc64::checksum(ciphertext.bytes());
    let retained = ciphertext.clone();
    let samples = Samples::default();
    let filter =
        Pair::parse("8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111")
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
    use crate::{
        security::aead::{PageCrypto, PageCryptoEngine},
        telemetry::send_crc::{Pair, Samples},
    };
    let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let keys = std::rc::Rc::new(keyring());
    let filter =
        Pair::parse("8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111")
            .unwrap();
    for mode in ["cached", "cancel", "missing", "capacity"] {
        let mut page = crate::memory::pool::tests::bundle_for(
            &admission,
            crate::model::VersionMetadata {
                content_type: None,
                version: crate::model::ObjectVersion {
                    object: crate::model::ObjectId {
                        cache: crate::model::CacheId("00000000-0000-4000-8000-000000000003".into()),
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
            .active(&cache, crate::security::identity::KeyPurpose::Page)
            .unwrap();
        let scope = RequestScope::new(
            crate::model::RequestId([7; 16]),
            crate::runtime::environment::now() + Duration::from_secs(10),
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
    use crate::{
        runtime::environment::{self, SimulationClock},
        security::aead::PageCryptoEngine,
    };
    use std::time::Duration;

    let clock = SimulationClock::new(43);
    let env = clock.environment(0);
    let _env = env.enter();
    let _strict = environment::require_simulated();
    let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let keys = keyring();
    let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
    let lease = || {
        keys.active(&cache, crate::security::identity::KeyPurpose::Page)
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
        let (waiting_io, mut waiting_engine) = pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
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
    use crate::runtime::environment::{self, SimulationClock};
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
    admission: &Rc<Admission>,
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
    admission: Rc<Admission>,
    pool: BufferPool,
    page: PageId,
    source: Vec<u8>,
    descriptor: PageEnvelope,
    encrypted: Vec<Vec<u8>>,
    size: usize,
    rotating: bool,
}

impl MeasurementInputs {
    fn new(size: usize, rotating: bool, operation: &str) -> Self {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = NonZeroUsize::new(512 * 1024 * 1024).unwrap();
        limits.ciphertext_bytes = limits.plaintext_bytes;
        let admission = Rc::new(Admission::new(limits));
        let pool = BufferPool::new(admission.clone());
        let page = page();
        let source = vec![7u8; if rotating { ROTATING + size } else { size }];
        let cipher = XChaCha20Poly1305::new((&[7; 32]).into());
        let descriptor = envelope(size);
        let mut encrypted = vec![7; size];
        cipher
            .encrypt_in_place(
                (&descriptor.nonce.0).into(),
                &page_aad(&descriptor).unwrap(),
                &mut encrypted,
            )
            .unwrap();
        if operation == "bad_tag" {
            encrypted[size] ^= 1;
        }
        let encrypted: Vec<_> = (0..if rotating { ROTATING.div_ceil(size) } else { 1 })
            .map(|_| encrypted.clone())
            .collect();
        Self {
            admission,
            pool,
            page,
            source,
            descriptor,
            encrypted,
            size,
            rotating,
        }
    }

    fn input(&self, i: usize, operation: &str) -> CryptoInput {
        let Self {
            admission,
            pool,
            page,
            source,
            descriptor,
            encrypted,
            size,
            rotating,
        } = self;
        let (size, rotating) = (*size, *rotating);
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
            let offset = if rotating {
                (i * size.max(32768)) % ROTATING
            } else {
                0
            };
            plain
                .bytes_mut()
                .unwrap()
                .copy_from_slice(&source[offset..offset + size]);
            CryptoInput::Encrypt {
                page: page.clone(),
                plaintext: plain,
                ciphertext: admission
                    .reserve(Some(cache), ResourceClass::Ciphertext, size + 16)
                    .unwrap(),
            }
        } else {
            // Fresh received allocation/checksum state, as on disk/peer ingress.
            let index = if rotating {
                i * (32768 / size).max(1) % encrypted.len()
            } else {
                0
            };
            let bytes = &encrypted[index];
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
