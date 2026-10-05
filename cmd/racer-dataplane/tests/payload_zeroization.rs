//! Observe the allocation at deallocation, before the system allocator can reuse it.
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;
use racer_dataplane::admission::AdmissionPolicy;
use racer_dataplane::admission::ResourceClass;
use racer_dataplane::config::Config;
use racer_dataplane::memory::BufferPool;
use racer_dataplane::model::CacheKey;
use racer_dataplane::model::Nonce;
use racer_dataplane::model::ObjectId;
use racer_dataplane::model::ObjectVersion;
use racer_dataplane::model::PageEnvelope;
use racer_dataplane::model::PageId;
use racer_dataplane::model::PageNumber;
use racer_dataplane::model::StrongEtag;
use std::alloc::GlobalAlloc;
use std::alloc::Layout;
use std::alloc::System;
use std::rc::Rc;
use std::sync::Mutex;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicPtr;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;

struct InspectAllocator;
static WATCH: AtomicPtr<u8> = AtomicPtr::new(std::ptr::null_mut());
static FREED: AtomicBool = AtomicBool::new(false);
static ZEROED: AtomicBool = AtomicBool::new(false);
static NEXT_SIZE: AtomicUsize = AtomicUsize::new(0);
static CANCEL_AT_ALLOCATION: Mutex<Option<racer_dataplane::runtime::RequestScope>> =
    Mutex::new(None);

// SAFETY: All allocation operations delegate to System with the original layout.
unsafe impl GlobalAlloc for InspectAllocator {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let pointer = unsafe { System.alloc(layout) };
        if !pointer.is_null()
            && NEXT_SIZE
                .compare_exchange(layout.size(), 0, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
        {
            WATCH.store(pointer, Ordering::SeqCst);
            // This scope has no registered waiters; cancellation allocates nothing.
            // Trigger after the engine's first scope check, before it transforms
            // the initialized output, to exercise the final publication check.
            if let Some(scope) = CANCEL_AT_ALLOCATION.lock().unwrap().take() {
                scope.cancel().unwrap();
            }
        }
        pointer
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
    let config = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("11111111-1111-4111-8111-111111111111".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://controller.invalid:443".into()),
            _ => None,
        })
    })
    .unwrap();
    let cache = CacheId("44444444-4444-4444-8444-444444444444".into());
    for ciphertext in [false, true] {
        for scenario in ["retained", "small", "full", "stopped", "destroyed"] {
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
            if scenario == "full" {
                // Different geometry prevents the watched owner from consuming a
                // slot, so its final release exercises full-pool rejection.
                let buffers: Vec<_> = (0..2)
                    .map(|_| {
                        pool.plaintext(
                            admission
                                .reserve(Some(&cache), ResourceClass::Plaintext, capacity + 1)
                                .unwrap(),
                            capacity + 1,
                        )
                        .unwrap()
                    })
                    .collect();
                drop(buffers);
            }
            let reservation = admission.reserve(Some(&cache), class, capacity).unwrap();
            let owner: Box<dyn Send> = if ciphertext {
                // A formerly initialized tail must be wiped even after truncation.
                let mut bytes = vec![0xa7; capacity];
                bytes.truncate(capacity - 31);
                let page = pool
                    .ciphertext(
                        reservation,
                        PageEnvelope {
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
            if scenario == "retained" {
                assert!(!FREED.load(Ordering::SeqCst));
                let admission = admission.as_ref().unwrap();
                assert_eq!(admission.used(class), capacity);
                admission.reclaim_buffers();
                assert_eq!(admission.used(class), 0);
            }
            assert!(FREED.load(Ordering::SeqCst), "{ciphertext} {scenario}");
            assert!(ZEROED.load(Ordering::SeqCst), "{ciphertext} {scenario}");
            if let Some(admission) = admission {
                admission.reclaim_buffers();
                assert_eq!(admission.used(class), 0);
            }
        }
    }
    failed_crypto_output_is_scrubbed(&config);
}

fn failed_crypto_output_is_scrubbed(config: &Config) {
    use base64::Engine;
    use racer_control_wire::ClusterId;
    use racer_control_wire::NodeId;
    use racer_crypto::identity::KeyEpochs;
    use racer_crypto::identity::KeyPurpose;
    use racer_crypto::identity::Keyring;
    use racer_crypto::{TAG_LEN, seal};
    use racer_dataplane::model::RequestId;
    use racer_dataplane::model::WorkerId;
    use racer_dataplane::runtime::RequestScope;
    use racer_dataplane::security::CryptoId;
    use racer_dataplane::security::CryptoInput;
    use racer_dataplane::security::PageCryptoEngine;
    use racer_dataplane::security::page_aad;
    use racer_dataplane::security::pair;
    use std::sync::Arc;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;

    let keys = Keyring::new(
        ClusterId("11111111-1111-4111-8111-111111111111".into()),
        NodeId("22222222-2222-4222-8222-222222222222".into()),
        Arc::new(KeyEpochs::default()),
    );
    let mut bundle: serde_json::Value = serde_json::from_slice(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../internal/racer/wire/testdata/bundle.json"
    )))
    .unwrap();
    for (i, key) in bundle["cache_keys"]
        .as_array_mut()
        .unwrap()
        .iter_mut()
        .enumerate()
    {
        key["material"] = base64::engine::general_purpose::STANDARD
            .encode([i as u8 + 7; 32])
            .into();
    }
    keys.install(racer_control_wire::decode_bundle(&serde_json::to_vec(&bundle).unwrap()).unwrap())
        .unwrap();
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
        let scope = RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10))
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
            CryptoInput::Encrypt {
                page: descriptor.page,
                plaintext,
                ciphertext: admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, length + 16)
                    .unwrap(),
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
            CryptoInput::Decrypt {
                ciphertext,
                plaintext: admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, length)
                    .unwrap(),
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
            *CANCEL_AT_ALLOCATION.lock().unwrap() = Some(scope.clone());
        }
        FREED.store(false, Ordering::SeqCst);
        ZEROED.store(false, Ordering::SeqCst);
        NEXT_SIZE.store(if encrypt { length + 16 } else { length }, Ordering::SeqCst);
        let completion = PageCryptoEngine::process(job);
        assert_eq!(
            NEXT_SIZE.load(Ordering::SeqCst),
            0,
            "{fault}: output allocated"
        );
        assert!(
            FREED.load(Ordering::SeqCst),
            "{fault}: output not published"
        );
        assert!(
            ZEROED.load(Ordering::SeqCst),
            "{fault}: entire output wiped before dealloc"
        );
        if fault.starts_with("cancel") {
            assert!(scope.check().is_err());
        }
        assert_eq!(admission.used(ResourceClass::Plaintext), length);
        assert_eq!(admission.used(ResourceClass::Ciphertext), cipher_charge);
        drop(completion);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
}
use uring_runtime::reactor::IoBuffer;
