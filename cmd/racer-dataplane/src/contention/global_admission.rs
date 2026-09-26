//! Global transport quota must not acquire cache-local fair-share ceilings.
use super::*;

fn config() -> Config {
    Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 16,
        dirty_pages: 16,
        pipes: 6,
        max_events: 1_000,
        ..Config::default()
    }
}

#[test]
fn two_live_caches_can_use_global_pipe_capacity_until_exhaustion() {
    assert_global_capacity(ResourceClass::Pipe);
}

#[test]
fn two_live_caches_can_use_global_connection_capacity_until_exhaustion() {
    assert_global_capacity(ResourceClass::Connection);
}

fn assert_global_capacity(class: ResourceClass) {
    // Production reserves None in http/pool.rs:109,411 and memory/pipe.rs:157.
    let mut sim = Simulator::new(config());
    let contexts = [0, 1].map(|cache| {
        sim.reserve(0, cache, ResourceClass::RequestContext, CONTEXT)
            .unwrap()
    });
    let limit = sim.workers[0].admission.limit(class);
    let mut leases = Vec::new();
    for slot in 0..limit {
        // Both caches own transport leases, but cache 0 exceeds half the
        // capacity while cache 1 remains live with ample context quota.
        let cache = usize::from(slot == 0);
        leases.push(sim.reserve(0, cache, class, 1).unwrap_or_else(|| {
            panic!("{class:?} falsely rejected at {slot}/{limit} global usage")
        }));
    }
    assert!(leases.iter().all(|lease| lease.cache().is_none()));
    assert_eq!(sim.report.rejections[class as usize], 0);
    assert_eq!(sim.workers[0].admission.used(class), limit);
    for cache in [0, 1] {
        assert!(sim.reserve(0, cache, class, 1).is_none());
    }
    assert_eq!(sim.workers[0].admission.used(class), limit);
    drop(leases.pop());
    let reused = sim.reserve(0, 1, class, 1).unwrap();
    assert_eq!(sim.workers[0].admission.used(class), limit);
    drop((leases, reused, contexts));
    assert!(
        CLASSES
            .iter()
            .all(|class| sim.workers[0].admission.used(*class) == 0)
    );
}

#[test]
fn two_caches_with_spare_pipes_do_not_park_readers() {
    let mut sim = Simulator::new(config());
    let requests = (0..5)
        .map(|object| Request {
            cache: usize::from(object == 0),
            pages: 1,
            ..Request::new(0, object)
        })
        .collect();
    let report = sim.run(requests);
    assert_eq!(report.completed, 5, "{report:?}");
    assert_eq!((report.failed, report.canceled, report.retries), (0, 0, 0));
    assert_eq!(report.rejections, [0; 11], "{report:?}");
    assert_eq!(report.peak_worker[ResourceClass::Pipe as usize], 5);
    assert_eq!(report.peak_worker[ResourceClass::Connection as usize], 5);
    assert_eq!(report.delivered_bytes, 5 * PAGE_BYTES);
    assert_eq!(report.final_used, [0; 11]);
}

#[test]
fn cache_scoped_resources_still_enforce_fairness_with_global_room() {
    for (class, amount) in [
        (ResourceClass::Plaintext, PLAIN),
        (ResourceClass::Ciphertext, CIPHER),
        (ResourceClass::DirtyCiphertext, CIPHER),
        (ResourceClass::Registered, CIPHER),
        (ResourceClass::RequestContext, CONTEXT),
        (ResourceClass::Flight, 1),
        (ResourceClass::Waiter, 1),
    ] {
        let mut sim = Simulator::new(config());
        let limit = sim.workers[0].admission.limit(class);
        let first = sim.reserve(0, 0, class, limit / 2).unwrap();
        let second = sim.reserve(0, 1, class, amount).unwrap();
        assert_eq!(first.cache(), Some(&CacheId("0".into())));
        assert_eq!(second.cache(), Some(&CacheId("1".into())));
        assert!(sim.workers[0].admission.used(class) + amount <= limit);
        assert!(sim.reserve(0, 0, class, amount).is_none(), "{class:?}");
        assert_eq!(sim.report.rejections[class as usize], 1);
        drop((first, second));
        assert_eq!(sim.workers[0].admission.used(class), 0);
    }
}
