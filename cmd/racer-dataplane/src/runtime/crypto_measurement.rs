//! Opt-in measurement only; no alternate CRC or page engine implementation.
use super::*;
use crate::{
    memory::pool::BufferPool,
    model::{
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::*,
        limits::ResourceClass,
    },
    runtime::{admission::Admission, reactor::IoBuffer},
    security::{
        aead::{PageCryptoEngine, page_aad},
        crc64,
    },
};
use chacha20poly1305::{KeyInit, XChaCha20Poly1305, XNonce, aead::AeadInPlace};
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
fn primitive(size: usize, rotating: bool) {
    let slots = if rotating { ROTATING.div_ceil(size) } else { 1 };
    let iterations = if rotating {
        slots
    } else {
        (16 * 1024 * 1024 / size).max(8)
    };
    let mut buffers: Vec<u8> = vec![7; slots * size];
    let cipher = XChaCha20Poly1305::new((&[7; 32]).into());
    let aad = page_aad(&envelope(size)).unwrap();
    let nonce = XNonce::from_slice(&[2; 24]);
    let mut scratch = vec![0; size];
    for operation in ["crc", "encrypt", "decrypt", "bad_tag"] {
        let mut tags = Vec::new();
        if operation == "decrypt" || operation == "bad_tag" {
            for bytes in buffers.chunks_exact_mut(size) {
                let mut tag = cipher
                    .encrypt_in_place_detached(nonce, &aad, bytes)
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
                    let tag = black_box(
                        cipher
                            .encrypt_in_place_detached(nonce, &aad, black_box(bytes))
                            .unwrap(),
                    );
                    // Validation outside timing is handled by the engine cases;
                    // keep the complete detached tag observable here.
                    black_box(tag);
                }
                _ => {
                    scratch.copy_from_slice(bytes);
                    failures += usize::from(
                        cipher
                            .decrypt_in_place_detached(
                                nonce,
                                &aad,
                                black_box(&mut scratch),
                                &tags[index],
                            )
                            .is_err(),
                    );
                }
            }
        }
        report(
            "primitive",
            if operation == "decrypt" {
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
            XNonce::from_slice(&descriptor.nonce.0),
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
#[ignore = "release-only bounded crypto measurement; see designs/racer-crypto-performance.md"]
fn crypto_measurement() {
    assert!(!cfg!(debug_assertions), "run with --release");
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
    for size in [63, 4095, 1024 * 1024, 16 * 1024 * 1024] {
        for rotating in [false, true] {
            primitive(size, rotating);
            for paired in [false, true] {
                for operation in ["encrypt", "decrypt", "bad_tag"] {
                    lifecycle(size, rotating, paired, operation);
                }
            }
        }
    }
}
