//! Observe the allocation at deallocation, before the system allocator can reuse it.
use racer_dataplane::{
    config::Config,
    memory::pool::BufferPool,
    model::{
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::{CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag},
        limits::ResourceClass,
    },
    runtime::{admission::Admission, reactor::IoBuffer},
};
use std::{
    alloc::{GlobalAlloc, Layout, System},
    rc::Rc,
    sync::atomic::{AtomicBool, AtomicPtr, Ordering},
};

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
    let (config, _) = Config::from_lookup_with_fabric_ports(|name| {
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
            let admission = Rc::new(Admission::new(config.limits.clone()));
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
}
