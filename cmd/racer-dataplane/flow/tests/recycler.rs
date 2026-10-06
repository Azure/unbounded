//! Recycler and charge contracts relocated from the application admission tests.
use flow_control::{Error, Policy, Quotas, Rejection};

/// Two recyclable classes and one independent metadata class.
#[derive(Clone, Copy)]
enum Resource {
    Payload,
    Staging,
    Context,
}

impl flow_control::Class for Resource {
    const COUNT: usize = 3;

    fn index(self) -> usize {
        self as usize
    }
}

/// Explicit test limits, without Racer defaults or resource semantics.
struct Limits {
    bytes: usize,

    keys: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            bytes: 128 << 20,
            keys: 128,
        }
    }
}

impl Policy for Limits {
    type Class = Resource;

    type Key = String;

    fn limit(&self, _: Resource) -> usize {
        self.bytes
    }

    fn max_keys(&self) -> usize {
        self.keys
    }

    fn wakes(_: Resource) -> bool {
        false
    }

    fn covers(_: Resource) -> bool {
        false
    }

    fn rejected(&self, _: Rejection<Resource>) {}
}

#[test]
fn recycled_truncated_capacity_is_zero_before_cross_cache_and_class_reuse() {
    let admission = Quotas::new(Limits::default());
    let first = "first".to_owned();
    let second = "second".to_owned();
    let capacity = 1024 * 1024;
    for length in [0, 1, capacity - 16, capacity] {
        let mut reservation = admission
            .reserve(Some(&first), Resource::Staging, capacity)
            .unwrap();
        let mut bytes = reservation.buffer(capacity).unwrap();
        bytes.fill(0xa7);
        bytes.truncate(length);
        let pointer = bytes.as_ptr();
        reservation.recycle(bytes);
        drop(reservation);
        assert_eq!(admission.used(Resource::Staging), capacity);
        let mut reservation = admission
            .reserve(Some(&second), Resource::Payload, capacity)
            .unwrap();
        let bytes = reservation.buffer(capacity).unwrap();
        assert_eq!(bytes.len(), capacity);
        assert_eq!(bytes.as_ptr(), pointer);
        assert!(bytes.iter().all(|byte| *byte == 0));
        assert_eq!(admission.used(Resource::Staging), 0);
        assert_eq!(admission.used(Resource::Payload), capacity);
        reservation.recycle(bytes);
        drop(reservation);
        admission.reclaim_buffers();
        assert_eq!(admission.used(Resource::Payload), 0);
    }
}

#[test]
fn recycled_spare_capacity_is_initialized_and_checkout_is_exact_and_admitted() {
    let admission = Quotas::new(Limits::default());
    let mut bytes = Vec::with_capacity(1 << 20);
    bytes.push(0xa7);
    let capacity = bytes.capacity();
    let pointer = bytes.as_ptr();
    let mut reservation = admission
        .reserve(None, Resource::Staging, capacity)
        .unwrap();
    reservation.recycle(bytes);
    drop(reservation);
    let reservation = admission
        .reserve(None, Resource::Payload, capacity)
        .unwrap();
    assert!(matches!(
        reservation.buffer(capacity + 1),
        Err(Error::InvalidInput)
    ));
    let different = reservation.buffer(capacity - 1).unwrap();
    assert_ne!(different.as_ptr(), pointer);
    assert_eq!(different.len(), capacity - 1);
    assert!(different.iter().all(|byte| *byte == 0));
    drop(different);
    assert_eq!(admission.retained_buffer_bytes(), capacity);
    let bytes = reservation.buffer(capacity).unwrap();
    assert_eq!(bytes.as_ptr(), pointer);
    assert_eq!(bytes.len(), capacity);
    assert!(bytes.iter().all(|byte| *byte == 0));
    assert_eq!(admission.used(Resource::Staging), 0);
    assert_eq!(admission.used(Resource::Payload), capacity);
    drop((bytes, reservation));
    assert_eq!(admission.used(Resource::Payload), 0);
}

#[test]
fn recycled_pool_keeps_two_slots_and_pressure_releases_idle_charges() {
    let capacity = 1 << 20;
    let admission = Quotas::new(Limits {
        bytes: 3 * capacity,
        ..Default::default()
    });
    let buffers: Vec<_> = (0..3)
        .map(|_| {
            let reservation = admission
                .reserve(None, Resource::Staging, capacity)
                .unwrap();
            let mut bytes = reservation.buffer(capacity).unwrap();
            bytes.fill(0xa7);
            (bytes, reservation)
        })
        .collect();
    for (bytes, mut reservation) in buffers {
        reservation.recycle(bytes);
    }
    assert_eq!(admission.retained_buffer_bytes(), 2 * capacity);
    assert_eq!(admission.used(Resource::Staging), 2 * capacity);
    let reservation = admission
        .reserve(None, Resource::Staging, 3 * capacity)
        .unwrap();
    assert_eq!(admission.retained_buffer_bytes(), 0);
    assert_eq!(admission.used(Resource::Staging), 3 * capacity);
    drop(reservation);
    assert_eq!(admission.used(Resource::Staging), 0);
}

#[test]
fn unrelated_quota_failure_preserves_recycled_payload() {
    for completing in [false, true] {
        for class in [Resource::Payload, Resource::Staging] {
            let admission = Quotas::new(Limits::default());
            let length = (16 << 20) + usize::from(matches!(class, Resource::Staging)) * 16;
            let mut old = admission.reserve(None, class, length).unwrap();
            let mut bytes = old.buffer(length).unwrap();
            bytes.fill(0xa7);
            let pointer = bytes.as_ptr();
            old.recycle(bytes);
            drop(old);
            let limit = admission.limit(Resource::Context);
            let held = admission.reserve(None, Resource::Context, limit).unwrap();
            let result = if completing {
                admission.reserve_completion(None, Resource::Context, 1)
            } else {
                admission.reserve(None, Resource::Context, 1)
            };
            assert!(matches!(result, Err(Error::Overloaded)));
            assert_eq!(admission.used(Resource::Context), limit);
            assert_eq!(admission.retained_buffer_bytes(), length);
            assert_eq!(admission.used(class), length);
            drop(held);
            let next = admission.reserve(None, class, length).unwrap();
            let bytes = next.buffer(length).unwrap();
            assert_eq!(bytes.as_ptr(), pointer);
            assert!(bytes.iter().all(|byte| *byte == 0));
            assert_eq!(admission.retained_buffer_bytes(), 0);
            assert_eq!(admission.used(class), length);
            drop((bytes, next));
            assert_eq!(admission.used(class), 0);
        }
    }
}

#[test]
fn relevant_quota_failure_still_reclaims_recycled_payload() {
    for completing in [false, true] {
        for cache_records in [false, true] {
            let length = 1 << 20;
            let admission = Quotas::new(Limits {
                bytes: length,
                keys: 1,
            });
            let first = "first".to_owned();
            let second = "second".to_owned();
            let mut old = admission
                .reserve(Some(&first), Resource::Staging, length)
                .unwrap();
            let bytes = old.buffer(length).unwrap();
            old.recycle(bytes);
            drop(old);
            assert_eq!(admission.retained_buffer_bytes(), length);
            let (cache, class, amount) = if cache_records {
                (Some(&second), Resource::Context, 1)
            } else {
                (None, Resource::Staging, length)
            };
            let reservation = if completing {
                admission.reserve_completion(cache, class, amount)
            } else {
                admission.reserve(cache, class, amount)
            }
            .unwrap();
            assert_eq!(admission.retained_buffer_bytes(), 0);
            assert_eq!(admission.used(class), amount);
            if cache_records {
                assert_eq!(admission.used(Resource::Staging), 0);
            }
            drop(reservation);
            assert_eq!(admission.used(class), 0);
        }
    }
}

#[test]
fn recycled_payload_capacity_stays_admitted_zeroed_and_reclaimable() {
    let admission = Quotas::new(Limits::default());
    let cache = "pool".to_owned();
    let mut reservation = admission
        .reserve(Some(&cache), Resource::Payload, 1024 * 1024)
        .unwrap();
    let mut bytes = reservation.buffer(1024 * 1024).unwrap();
    let pointer = bytes.as_ptr();
    bytes.fill(87);
    reservation.recycle(bytes);
    drop(reservation);
    assert_eq!(admission.used(Resource::Payload), 1024 * 1024);
    let reservation = admission
        .reserve(Some(&cache), Resource::Payload, 1024 * 1024)
        .unwrap();
    let bytes = reservation.buffer(1024 * 1024).unwrap();
    assert_eq!(bytes.as_ptr(), pointer);
    assert!(bytes.iter().all(|b| *b == 0));
    assert_eq!(admission.used(Resource::Payload), 1024 * 1024);
    drop((bytes, reservation));
    assert_eq!(admission.retained_buffer_bytes(), 0);
    assert_eq!(admission.used(Resource::Payload), 0);
}

#[test]
fn split_and_shrink_preserve_live_ownership_and_reject_growth() {
    let admission = Quotas::new(Limits::default());
    let cache = "cache".to_owned();
    let mut bundle = admission
        .reserve(Some(&cache), Resource::Staging, 100)
        .unwrap();
    assert!(bundle.split(100).is_err());
    assert!(bundle.shrink(101).is_err());
    assert!(bundle.shrink(0).is_err());
    let staging = bundle.split(60).unwrap();
    assert_eq!(admission.used(Resource::Staging), 100);
    bundle.shrink(19).unwrap();
    assert_eq!(admission.used(Resource::Staging), 79);
    std::thread::spawn(move || drop(staging)).join().unwrap();
    assert_eq!(admission.used(Resource::Staging), 19);
    drop(bundle);
    assert_eq!(admission.used(Resource::Staging), 0);
}

/// Compare identical allocation work with and without retaining recycler backing.
#[test]
#[ignore = "opt-in same-workload payload recycle benchmark"]
fn payload_recycle_benchmark() {
    use std::hint::black_box;
    use std::time::Instant;
    const ITERATIONS: usize = 128;
    for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
        for retain in [true, false] {
            let admission = Quotas::new(Limits::default());
            if !retain {
                admission.stop();
            }
            for sample in 0..6 {
                let start = Instant::now();
                for _ in 0..ITERATIONS {
                    let mut reservation = admission
                        .reserve_completion(None, Resource::Staging, length)
                        .unwrap();
                    let mut bytes = reservation.buffer(length).unwrap();
                    bytes.fill(black_box(0xa7));
                    black_box(&bytes);
                    reservation.recycle(bytes);
                    drop(reservation);
                }
                let elapsed = start.elapsed();
                if sample != 0 {
                    println!(
                        "payload_recycle length={length} retain={retain} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                        elapsed.as_nanos() as f64 / ITERATIONS as f64
                    );
                }
            }
            admission.reclaim_buffers();
            assert_eq!(admission.used(Resource::Staging), 0);
        }
    }
}
