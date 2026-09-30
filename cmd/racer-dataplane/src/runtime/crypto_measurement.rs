//! Opt-in measurement only; no alternate CRC or page engine implementation.
use super::*;
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
        key_id: KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: size as u32,
        ciphertext_length: size as u32 + 16,
    }
}

fn validate(result: &CryptoCompletion, size: usize) {
    if let CryptoOutcome::Completed(output) = &result.outcome {
        let (CryptoOutput::Encrypted(plain, ciphertext)
        | CryptoOutput::Decrypted(plain, ciphertext)) = output;
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
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(512 * 1024 * 1024).unwrap();
    limits.ciphertext_bytes = limits.plaintext_bytes;
    let admission = Rc::new(Admission::new(limits));
    let pool = BufferPool::new(admission.clone());
    let keys = tests::keyring();
    let page = page();
    let cache = &page.version.object.cache;
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
        let input = if operation == "encrypt" {
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
        };
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
            keys.active(cache, crate::security::keyring::KeyPurpose::Page)
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
    use crate::telemetry::metrics::{Event::*, Metrics};
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(512 * 1024 * 1024).unwrap();
    limits.ciphertext_bytes = limits.plaintext_bytes;
    let admission = Rc::new(Admission::new(limits));
    let pool = BufferPool::new(admission.clone());
    let keys = tests::keyring();
    let page = page();
    let cache = &page.version.object.cache;
    let source = vec![7; size];
    let descriptor = envelope(size);
    let mut encrypted = source.clone();
    XChaCha20Poly1305::new((&[7; 32]).into())
        .encrypt_in_place(
            (&descriptor.nonce.0).into(),
            &page_aad(&descriptor).unwrap(),
            &mut encrypted,
        )
        .unwrap();
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
            let input = if decrypt {
                CryptoInput::Decrypt {
                    ciphertext: pool
                        .ciphertext(
                            admission
                                .reserve(Some(cache), ResourceClass::Ciphertext, encrypted.len())
                                .unwrap(),
                            descriptor.clone(),
                            encrypted.clone(),
                        )
                        .unwrap(),
                    plaintext: admission
                        .reserve(Some(cache), ResourceClass::Plaintext, size)
                        .unwrap(),
                }
            } else {
                let mut plaintext = pool
                    .plaintext(
                        admission
                            .reserve(Some(cache), ResourceClass::Plaintext, size)
                            .unwrap(),
                        size,
                    )
                    .unwrap();
                plaintext.bytes_mut().unwrap().copy_from_slice(&source);
                CryptoInput::Encrypt {
                    page: page.clone(),
                    plaintext,
                    ciphertext: admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, size + 16)
                        .unwrap(),
                }
            };
            let output = client
                .execute(
                    input,
                    keys.active(cache, crate::security::keyring::KeyPurpose::Page)
                        .unwrap(),
                    &scope,
                )
                .await
                .unwrap();
            let (CryptoOutput::Encrypted(plain, cipher) | CryptoOutput::Decrypted(plain, cipher)) =
                &output;
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
    let events = if decrypt {
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
    };
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
