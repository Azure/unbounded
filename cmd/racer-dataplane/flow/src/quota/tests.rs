use super::*;

#[derive(Clone, Copy, Debug)]
enum Resource {
    Payload,
    Other,
    Progress,
}
impl Class for Resource {
    const COUNT: usize = 3;
    fn index(self) -> usize {
        self as usize
    }
}
#[derive(Clone)]
struct TestPolicy {
    limit: usize,
    max_keys: usize,
    rejected: Arc<Mutex<Vec<Rejection<Resource>>>>,
}
impl TestPolicy {
    fn new(limit: usize, max_keys: usize) -> Self {
        Self {
            limit,
            max_keys,
            rejected: Arc::default(),
        }
    }
}
impl Policy for TestPolicy {
    type Class = Resource;
    type Key = String;
    fn limit(&self, _: Resource) -> usize {
        self.limit
    }
    fn max_keys(&self) -> usize {
        self.max_keys
    }
    fn wakes(class: Resource) -> bool {
        matches!(class, Resource::Other)
    }
    fn covers(class: Resource) -> bool {
        matches!(class, Resource::Payload)
    }
    fn allows_stopped(class: Resource) -> bool {
        matches!(class, Resource::Progress)
    }
    fn rejected(&self, rejection: Rejection<Resource>) {
        self.rejected.lock().unwrap().push(rejection);
    }
}

#[test]
fn secure_payload_wipe_initializes_spare_capacity_and_preserves_geometry() {
    for capacity in [0, 1, 15, 16, 17, 63, 64, 65, 4095, 4096, 4097] {
        for initialized in [false, true] {
            let mut bytes = Vec::with_capacity(capacity);
            if initialized {
                bytes.resize(bytes.capacity(), 0xa7);
                bytes.truncate(capacity / 2);
            }
            let pointer = bytes.as_ptr();
            let allocated = bytes.capacity();
            wipe_payload(&mut bytes);
            assert!(bytes.is_empty());
            assert_eq!(bytes.capacity(), allocated);
            assert_eq!(bytes.as_ptr(), pointer);
            // SAFETY: wipe_payload initializes every byte of the allocation.
            unsafe { bytes.set_len(allocated) };
            assert!(bytes.iter().all(|byte| *byte == 0));
            wipe_payload(&mut bytes);
            assert!(bytes.is_empty());
        }
    }
}

#[test]
#[ignore = "release-only alternating full-capacity secure wipe comparison"]
#[allow(clippy::assertions_on_constants)] // Fail only when this ignored benchmark is run.
fn secure_payload_wipe_benchmark() {
    use std::{hint::black_box, time::Instant};
    use zeroize::Zeroize;
    assert!(!cfg!(debug_assertions), "run with --release");
    const ITERATIONS: usize = 128;
    for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
        let mut bytes = vec![0u8; length];
        for sample in 0..6 {
            for optimized in if sample % 2 == 0 {
                [false, true]
            } else {
                [true, false]
            } {
                let start = Instant::now();
                for _ in 0..ITERATIONS {
                    bytes.fill(black_box(0xa7));
                    black_box(&bytes);
                    if optimized {
                        wipe_payload(&mut bytes);
                    } else {
                        bytes.clear();
                        bytes.zeroize();
                    }
                    // SAFETY: both primitives initialize the full capacity.
                    unsafe { bytes.set_len(length) };
                    black_box(&bytes);
                }
                let elapsed = start.elapsed();
                assert!(bytes.iter().all(|byte| *byte == 0));
                if sample != 0 {
                    println!(
                        "secure_wipe length={length} optimized={optimized} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                        elapsed.as_nanos() as f64 / ITERATIONS as f64
                    );
                }
            }
        }
    }
}

#[test]
fn fairness_retirement_reclamation_and_completion() {
    let quotas = Quotas::new(TestPolicy::new(100, 2));
    let (a, b, c) = ("a".to_owned(), "b".to_owned(), "c".to_owned());
    let first = quotas.reserve(Some(&a), Resource::Payload, 40).unwrap();
    let second = quotas.reserve(Some(&b), Resource::Payload, 40).unwrap();
    assert!(matches!(
        quotas.reserve(Some(&a), Resource::Payload, 11),
        Err(Error::Overloaded)
    ));
    assert_eq!(
        quotas.reclamation(&a, Resource::Payload, 11),
        Some((Some(a.clone()), 1))
    );
    assert_eq!(quotas.reclamation(&a, Resource::Payload, 51), None);
    assert!(matches!(
        quotas.reserve(Some(&c), Resource::Other, 1),
        Err(Error::Overloaded)
    ));
    drop(second);
    let third = quotas.reserve(Some(&a), Resource::Payload, 60).unwrap();
    assert_eq!(quotas.active_keys.load(Ordering::Acquire), 1);
    assert!(!quotas.keys.borrow().contains_key(&b));
    assert_eq!(
        quotas.reclamation(&a, Resource::Payload, 1),
        Some((Some(a.clone()), 1))
    );
    let unkeyed = quotas.reserve(None, Resource::Other, 100).unwrap();
    assert_eq!(quotas.reclamation(&a, Resource::Other, 1), Some((None, 1)));
    drop((unkeyed, first, third));
    let first = quotas.reserve(Some(&a), Resource::Payload, 60).unwrap();
    let second = quotas.reserve(Some(&b), Resource::Other, 1).unwrap();
    quotas.stop();
    assert!(matches!(
        quotas.reserve(None, Resource::Payload, 1),
        Err(Error::Unavailable)
    ));
    assert!(quotas.reserve(None, Resource::Progress, 1).is_ok());
    let completion = quotas
        .reserve_completion(Some(&a), Resource::Payload, 40)
        .unwrap();
    assert_eq!(quotas.used(Resource::Payload), 100);
    assert!(matches!(
        quotas.reserve_completion(None, Resource::Payload, 1),
        Err(Error::Overloaded)
    ));
    drop((completion, first, second));
    assert_eq!(quotas.used(Resource::Payload), 0);
}

#[test]
fn recycler_retains_two_live_charges_and_reports_pressure_before_retry() {
    let size = 1 << 20;
    let quotas = Quotas::new(TestPolicy::new(3 * size, 4));
    let mut buffers: Vec<_> = (0..3)
        .map(|_| {
            let charge = quotas.reserve(None, Resource::Payload, size).unwrap();
            let bytes = charge.buffer(size).unwrap();
            (charge, bytes)
        })
        .collect();
    for (mut charge, mut bytes) in buffers.drain(..) {
        bytes.fill(0xa7);
        bytes.truncate(1);
        charge.recycle(bytes);
    }
    assert_eq!(quotas.buffers.lock().unwrap().len(), 2);
    assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
    assert!(
        quotas
            .buffers
            .lock()
            .unwrap()
            .iter()
            .all(|(b, _)| b.len() == size && b.iter().all(|v| *v == 0))
    );
    let shared = quotas.shared();
    let other = quotas.reserve(None, Resource::Other, 3 * size).unwrap();
    assert!(matches!(
        quotas.reserve(None, Resource::Other, 1),
        Err(Error::Overloaded)
    ));
    assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
    assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 2);
    let charge = quotas.reserve(None, Resource::Payload, 3 * size).unwrap();
    assert_eq!(quotas.retained_buffer_bytes(), 0);
    assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 3);
    assert!(
        matches!(quotas.policy.rejected.lock().unwrap()[2], Rejection::Resource { used, requested, .. } if used == 2 * size && requested == 3 * size)
    );
    let mut charge = charge;
    charge.recycle(vec![0xa7; 3 * size]);
    drop((charge, other, quotas));
    assert_eq!(
        shared.used(Resource::Payload),
        0,
        "usage handles must not retain recycled buffers"
    );
}

#[test]
fn key_record_rejection_is_reported_before_reclaim_retry() {
    let size = 1 << 20;
    let quotas = Quotas::new(TestPolicy::new(2 * size, 1));
    let (a, b) = ("a".to_owned(), "b".to_owned());
    let mut old = quotas.reserve(Some(&a), Resource::Payload, size).unwrap();
    old.recycle(vec![0xa7; size]);
    drop(old);
    let charge = quotas.reserve(Some(&b), Resource::Other, 1).unwrap();
    assert_eq!(quotas.retained_buffer_bytes(), 0);
    assert!(!quotas.keys.borrow().contains_key(&a));
    assert!(matches!(
        &quotas.policy.rejected.lock().unwrap()[..],
        [Rejection::Keys { used: 1, limit: 1 }]
    ));
    drop(charge);
}

#[test]
fn charge_validation_and_thread_safe_shared_usage() {
    fn send_sync<T: Send + Sync>() {}
    send_sync::<Charge<TestPolicy>>();
    send_sync::<SharedQuotas<TestPolicy>>();
    let quotas = Quotas::new(TestPolicy::new(100, 2));
    assert!(matches!(
        quotas.reserve(None, Resource::Payload, 0),
        Err(Error::InvalidInput)
    ));
    assert!(matches!(
        quotas.reserve(None, Resource::Payload, usize::MAX),
        Err(Error::Overloaded)
    ));
    let shared = quotas.shared();
    assert!(matches!(
        shared.reserve(Resource::Payload, 0),
        Err(Error::InvalidInput)
    ));
    let mut charge = shared.reserve(Resource::Payload, 100).unwrap();
    assert!(quotas.owns(&charge));
    assert!(!Quotas::new(TestPolicy::new(100, 2)).owns(&charge));
    assert!(page_alloc::Charge::covers(&charge, 100));
    assert!(!page_alloc::Charge::covers(&charge, 101));
    assert!(charge.validate(Resource::Payload, 100).is_ok());
    assert!(charge.validate(Resource::Other, 100).is_err());
    assert!(charge.split(100).is_err());
    assert!(charge.split(0).is_err());
    assert!(charge.shrink(0).is_err());
    assert!(charge.shrink(101).is_err());
    let split = charge.split(60).unwrap();
    charge.shrink(19).unwrap();
    assert_eq!(quotas.used(Resource::Payload), 79);
    std::thread::spawn(move || drop(split)).join().unwrap();
    assert_eq!(shared.used(Resource::Payload), 19);
    drop(charge);
    let other = shared.reserve(Resource::Other, 1).unwrap();
    assert!(!page_alloc::Charge::covers(&other, 1));
    drop(other);
    quotas.stop();
    assert!(shared.is_stopped());
    assert!(matches!(
        shared.reserve(Resource::Payload, 1),
        Err(Error::Unavailable)
    ));
}

#[test]
fn release_and_stop_wake_shared_waiters() {
    struct WakeCount(AtomicUsize);
    impl std::task::Wake for WakeCount {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    let count = Arc::new(WakeCount(AtomicUsize::new(0)));
    let waker = std::task::Waker::from(count.clone());
    let quotas = Quotas::new(TestPolicy::new(1, 1));
    let shared = quotas.shared();
    shared.register(&waker);
    let charge = shared.reserve(Resource::Other, 1).unwrap();
    assert!(matches!(
        shared.reserve(Resource::Other, 1),
        Err(Error::Overloaded)
    ));
    std::thread::spawn(move || drop(charge)).join().unwrap();
    assert_eq!(count.0.load(Ordering::Relaxed), 1);
    shared.register(&waker);
    quotas.stop();
    assert_eq!(count.0.load(Ordering::Relaxed), 2);
}
