//! Observe the allocation at deallocation, before the system allocator can reuse it.
/// Shared integration configuration and enrollment helpers.
#[path = "support/enrollment.rs"]
#[allow(dead_code)]
mod fixture_io;

use racer as dataplane;
use racer::admission::AdmissionPolicy;
use racer::admission::ResourceClass;
use racer::config::Config;
use racer::memory::BufferPool;
use racer::model::CacheKey;
use racer::model::Nonce;
use racer::model::ObjectId;
use racer::model::ObjectVersion;
use racer::model::PageEnvelope;
use racer::model::PageId;
use racer::model::PageNumber;
use racer::model::StrongEtag;
use racer_wire::control::CacheId;
use racer_wire::control::KeyId;
use std::alloc::GlobalAlloc;
use std::alloc::Layout;
use std::alloc::System;
use std::rc::Rc;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicPtr;
use std::sync::atomic::Ordering;

struct InspectAllocator;
static WATCH: AtomicPtr<u8> = AtomicPtr::new(std::ptr::null_mut());
static FREED: AtomicBool = AtomicBool::new(false);
static ZEROED: AtomicBool = AtomicBool::new(false);

// SAFETY: All allocation operations delegate to System with the original layout.
unsafe impl GlobalAlloc for InspectAllocator {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        unsafe { System.alloc(layout) }
    }
    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        if WATCH.load(Ordering::SeqCst) == ptr {
            // SAFETY: The watched u8 allocation was initialized through its full
            // capacity before publication and is still live until System.dealloc.
            let bytes = unsafe { std::slice::from_raw_parts(ptr, layout.size()) };
            ZEROED.store(bytes.iter().all(|byte| *byte == 0), Ordering::SeqCst);
            WATCH.store(std::ptr::null_mut(), Ordering::SeqCst);
            FREED.store(true, Ordering::SeqCst);
        }
        unsafe { System.dealloc(ptr, layout) }
    }
}

#[global_allocator]
static ALLOCATOR: InspectAllocator = InspectAllocator;

fn watch(pointer: *const u8) {
    FREED.store(false, Ordering::SeqCst);
    ZEROED.store(false, Ordering::SeqCst);
    WATCH.store(pointer.cast_mut(), Ordering::SeqCst);
}

#[test]
fn final_payload_owner_scrubs_full_allocation_on_reclaim_and_rejection() {
    let config = fixture_io::default_config("11111111-1111-4111-8111-111111111111");
    let cache = CacheId("44444444-4444-4444-8444-444444444444".into());
    for ciphertext in [false, true] {
        for scenario in [
            "retained",
            "trimmed",
            "small",
            "full",
            "stopped",
            "destroyed",
        ] {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                config.limits.clone(),
            )));
            let pool = BufferPool::new(admission.clone());
            let capacity = if scenario == "small" { 4096 } else { 1 << 20 };
            let class = if ciphertext {
                ResourceClass::Ciphertext
            } else {
                ResourceClass::Plaintext
            };
            let reservation = admission.reserve(Some(&cache), class, capacity).unwrap();
            let owner: Box<dyn Send> = if ciphertext {
                // Ciphertext needs no secure wipe, including its truncated tail.
                let mut bytes = vec![0xa7; capacity];
                bytes.truncate(capacity - 31);
                let page = pool
                    .ciphertext(
                        reservation,
                        PageEnvelope {
                            cipher: racer::model::PageCipher::Xaes256Gcm,
                            page: PageId {
                                version: ObjectVersion {
                                    object: ObjectId {
                                        cache: cache.clone(),
                                        key: CacheKey([9; 32]),
                                    },
                                    etag: StrongEtag::parse(b"\"zeroization\"").unwrap(),
                                },
                                number: PageNumber(0),
                            },
                            key_id: KeyId([1; 16]),
                            nonce: Nonce([2; 24]),
                            plaintext_length: (bytes.len() - 16) as u32,
                            ciphertext_length: bytes.len() as u32,
                        },
                        bytes,
                    )
                    .unwrap();
                watch(page.bytes().as_ptr());
                let last = page.clone();
                drop(page);
                assert!(!FREED.load(Ordering::SeqCst));
                assert_eq!(last.bytes()[0], 0xa7);
                Box::new(last)
            } else {
                let mut buffer = pool.plaintext(reservation, capacity).unwrap();
                buffer.bytes_mut().unwrap().fill(0xa7);
                watch(buffer.bytes().unwrap().as_ptr());
                Box::new(buffer)
            };
            if scenario == "stopped" {
                admission.stop();
            }
            drop(pool);
            let admission = if scenario == "destroyed" {
                drop(admission);
                None
            } else {
                Some(admission)
            };
            std::thread::spawn(move || drop(owner)).join().unwrap();
            // Both payloads live in the worker's pooled storage, which the
            // dropped pool has closed: the last owner frees it at once, and
            // wipes it first only for plaintext.
            assert!(FREED.load(Ordering::SeqCst), "{ciphertext} {scenario}");
            assert_eq!(
                ZEROED.load(Ordering::SeqCst),
                !ciphertext,
                "{ciphertext} {scenario}"
            );
            if let Some(admission) = admission {
                assert_eq!(admission.used(class), 0);
            }
        }
    }
    failed_crypto_output_is_scrubbed(&config);
}

fn failed_crypto_output_is_scrubbed(config: &Config) {
    use racer::model::RequestId;
    use racer::model::WorkerId;
    use racer::runtime::RequestScope;
    use racer::security::CryptoId;
    use racer::security::CryptoInput;
    use racer::security::PageCryptoEngine;
    use racer::security::page_aad;
    use racer::security::pair;
    use racer_crypto::identity::KeyEpochs;
    use racer_crypto::identity::KeyPurpose;
    use racer_crypto::identity::Keyring;
    use racer_crypto::{TAG_LEN, seal};
    use racer_wire::control::ClusterId;
    use racer_wire::control::NodeId;
    use std::sync::Arc;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;

    let keys = Keyring::new(
        ClusterId("11111111-1111-4111-8111-111111111111".into()),
        NodeId("22222222-2222-4222-8222-222222222222".into()),
        Arc::new(KeyEpochs::with_clock(racer::enrollment::wall_clock(
            &racer_io::Clock::host(),
        ))),
    );
    let mut bundle = racer_wire::control::decode_bundle(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/wire/testdata/bundle.json"
    )))
    .unwrap();
    bundle.cache_keys = std::mem::take(&mut bundle.cache_keys)
        .into_iter()
        .enumerate()
        .map(|(i, key)| {
            let (key, state, _) = key.into_installation();
            racer_wire::control::CacheEncryptionKey::new(
                key,
                state,
                zeroize::Zeroizing::new([i as u8 + 7; 32]),
            )
        })
        .collect();
    keys.install(bundle).unwrap();
    let cache = CacheId("44444444-4444-4444-8444-444444444444".into());
    // Unusual sizes isolate the admitted payload allocation from engine metadata.
    let length = 4093;
    for fault in [
        "nonce",
        "aad",
        "ciphertext",
        "tag",
        "cancel-decrypt",
        "cancel-encrypt",
    ] {
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            config.limits.clone(),
        )));
        let pool = BufferPool::new(admission.clone());
        let key = keys.active(&cache, KeyPurpose::Page).unwrap();
        let mut descriptor = PageEnvelope {
            cipher: racer::model::PageCipher::Xaes256Gcm,
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: cache.clone(),
                        key: CacheKey([3; 32]),
                    },
                    etag: StrongEtag::parse(b"\"scrub\"").unwrap(),
                },
                number: PageNumber(0),
            },
            key_id: key.id(),
            nonce: Nonce([2; 24]),
            plaintext_length: length as u32,
            ciphertext_length: length as u32 + 16,
        };
        let encrypt = fault == "cancel-encrypt";
        let scope = RequestScope::with_clock(
            RequestId([1; 16]),
            Instant::now() + Duration::from_secs(10),
            racer_io::Clock::host(),
        )
        .unwrap();
        let input = if encrypt {
            let mut plaintext = pool
                .plaintext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Plaintext, length)
                        .unwrap(),
                    length,
                )
                .unwrap();
            plaintext.bytes_mut().unwrap().fill(0xa7);
            watch(plaintext.bytes().unwrap().as_ptr());
            CryptoInput::Encrypt {
                page: descriptor.page,
                plaintext,
                ciphertext: Box::new(
                    pool.encryption_output(
                        admission
                            .reserve(Some(&cache), ResourceClass::Ciphertext, length + 16)
                            .unwrap(),
                        length + 16,
                    )
                    .unwrap(),
                ),
            }
        } else {
            let plaintext = vec![0xa7; length];
            let mut bytes = vec![0; length + TAG_LEN];
            seal(
                &[7; 32],
                &descriptor.nonce.0,
                &page_aad(&descriptor).unwrap(),
                &plaintext,
                &mut bytes,
            )
            .unwrap();
            match fault {
                "nonce" => descriptor.nonce.0[0] ^= 1,
                "aad" => descriptor.page.number.0 += 1,
                "ciphertext" => bytes[0] ^= 1,
                "tag" => bytes[length] ^= 1,
                _ => {}
            }
            // Charge the complete fixture allocation, including the tag.
            let ciphertext = pool
                .ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, bytes.capacity())
                        .unwrap(),
                    descriptor,
                    bytes,
                )
                .unwrap();
            // Decrypt opens into pooled storage checked out before submission.
            // Its pool is gone, so the last owner wipes and frees it.
            let plaintext = racer::memory::BufferPool::new(admission.clone())
                .plaintext_storage(
                    admission
                        .reserve(Some(&cache), ResourceClass::Plaintext, length)
                        .unwrap(),
                    length,
                )
                .unwrap();
            watch(plaintext.as_ptr());
            CryptoInput::Decrypt {
                ciphertext,
                plaintext,
            }
        };
        let cipher_charge = admission.used(ResourceClass::Ciphertext);
        let (io, _port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(permit)) = io.poll_reserve(
            &mut cx,
            CryptoId {
                worker: WorkerId(0),
                generation: 1,
                sequence: 1,
            },
        ) else {
            panic!("reserve")
        };
        let job = permit.job(input, key, scope.clone());
        if fault.starts_with("cancel") {
            // Outputs are checked out before submission, so cancel up front.
            scope.cancel().unwrap();
        }
        let completion = PageCryptoEngine::process(job, &racer_io::WorkerCtx::host());
        assert!(
            !FREED.load(Ordering::SeqCst),
            "{fault}: failures never consume the input storage"
        );
        if fault.starts_with("cancel") {
            assert!(scope.check().is_err());
        }
        // Pooled plaintext is charged at its 4 KiB granule.
        assert_eq!(
            admission.used(ResourceClass::Plaintext),
            length.next_multiple_of(4096)
        );
        assert_eq!(admission.used(ResourceClass::Ciphertext), cipher_charge);
        drop(completion);
        if !encrypt {
            assert!(FREED.load(Ordering::SeqCst), "{fault}: storage released");
            assert!(
                ZEROED.load(Ordering::SeqCst),
                "{fault}: entire storage wiped before dealloc"
            );
        }
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
}
use racer_io::ring::IoBuffer;
