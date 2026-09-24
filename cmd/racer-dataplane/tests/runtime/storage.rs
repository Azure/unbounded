// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::os::unix::fs::MetadataExt;

#[test]
fn production_worker_threads_keep_polling_through_resize_barriers() {
    use crate::workers;
    struct App(Volumes, Arc<std::sync::atomic::AtomicUsize>);
    impl uring::Application for App {
        fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
            self.1.fetch_add(1, Ordering::Relaxed);
            self.0.poll(ring, budget)
        }
        fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
            self.0.shutdown(ring)
        }
    }
    let plan = workers::CpuPlan::discover(
        workers::Config {
            shard_count: std::num::NonZeroUsize::new(2).unwrap(),
        },
        workers::WorkerCounts::default(),
    )
    .unwrap();
    assert_eq!(plan.io().len(), 2);
    let path = std::env::temp_dir().join(format!("racer-threaded-resize-{}", std::process::id()));
    let locked = StoragePath::lock(&path).unwrap();
    let budget = CheckpointBudget::default();
    let slab = LayoutPlan::new(1 << 30, 2)
        .unwrap()
        .create(locked.active(), budget.clone())
        .unwrap();
    let generation = plan.io()[0].storage_generation(slab.shard_count()).unwrap();
    let updates = Arc::new(Updates::default());
    let coordinator =
        StorageCoordinator::start(locked, &slab, &plan.io()[0], budget, updates.clone()).unwrap();
    let handle = coordinator.handle();
    let setup = Mutex::new((Some(slab), 2usize));
    let counts: Arc<Vec<_>> = Arc::new(
        (0..2)
            .map(|_| Arc::new(std::sync::atomic::AtomicUsize::new(0)))
            .collect(),
    );
    let observed = counts.clone();
    let worker_updates = updates.clone();
    let workers = workers::Workers::start_planned(plan, move |context| {
        let pool = crate::buffers::io_test_pool(4);
        let ring = uring::Ring::http_test_ring(pool.clone(), Default::default())?;
        let shards = {
            let mut setup = setup.lock().unwrap();
            let shards = generation
                .take_assignments(context)?
                .into_iter()
                .map(|assignment| {
                    let shard = setup.0.as_mut().unwrap().take_shard(assignment.id())?;
                    ShardState::activate(context, assignment, shard, &pool, Default::default())
                })
                .collect::<io::Result<Vec<_>>>()?;
            setup.1 -= 1;
            if setup.1 == 0 {
                setup.0.take();
            }
            shards
        };
        let cache = Cache::for_generation(
            context,
            &generation,
            Namespace::new("bootstrap").unwrap(),
            shards,
        )
        .map_err(io::Error::other)?;
        worker_updates.subscribe(ring.wake_handle());
        let app = Volumes::new(
            cache,
            worker_updates.clone(),
            Arc::new(crate::crypto::Pool::test_pool(&pool)),
            context.worker_id().0,
        )
        .with_storage(handle.clone(), context.clone());
        uring::Driver::new(ring, App(app, observed[context.worker_id().0].clone()), 128)
    })
    .unwrap();
    // Permanent post-rename sync failure keeps application work fenced, but
    // cannot block either worker's driver/heartbeat turns.
    coordinator.shared.faults.lock().unwrap().sync = true;
    updates.test_storage_policy(1, 48 << 30);
    let end = Instant::now() + Duration::from_secs(10);
    while !matches!(
        updates.storage_policy_status().result,
        Some(StorageResult::Failed(_))
    ) {
        assert!(Instant::now() < end);
        thread::sleep(TICK);
    }
    let before: Vec<_> = counts.iter().map(|n| n.load(Ordering::Relaxed)).collect();
    thread::sleep(Duration::from_millis(100));
    for (count, before) in counts.iter().zip(before) {
        assert!(count.load(Ordering::Relaxed) > before + 2);
    }
    coordinator.shared.faults.lock().unwrap().sync = false;
    while updates.storage_policy_status().result != Some(StorageResult::Applied) {
        assert!(Instant::now() < end);
        thread::sleep(TICK);
    }
    updates.test_storage_policy(2, 1 << 30);
    while updates.storage_policy_status().result != Some(StorageResult::Applied) {
        assert!(Instant::now() < end);
        thread::sleep(TICK);
    }
    workers.stop_handle().request_stop();
    workers.join().unwrap();
    drop(coordinator);
    let slab = Slab::open_existing_layout(&path, 2).unwrap();
    assert_eq!(slab.size(), 1 << 30);
    drop(slab);
    std::fs::remove_file(&path).unwrap();
    let mut lock = path.into_os_string();
    lock.push(".lock");
    std::fs::remove_file(lock).unwrap();
}

#[test]
#[ignore = "abrupt-exit helper for crash_publish_boundaries"]
fn crash_publish_child() {
    let path = std::env::var_os("RACER_RESIZE_CRASH_PATH").unwrap();
    let boundary = std::env::var("RACER_RESIZE_CRASH_BOUNDARY").unwrap();
    let locked = StoragePath::lock(path).unwrap();
    let old = LayoutPlan::new(512 << 20, 1)
        .unwrap()
        .create(locked.active(), CheckpointBudget::default())
        .unwrap();
    let _candidate = Slab::prepare_replacement(
        &locked.candidate,
        LayoutPlan::new(20 << 30, 1).unwrap(),
        CheckpointBudget::default(),
    )
    .unwrap();
    if boundary != "prepared" {
        locked.publish().unwrap();
    }
    if boundary == "synced" {
        locked.directory.sync_all().unwrap();
    }
    assert_eq!(old.size(), 512 << 20);
    // SAFETY: intentionally terminate this isolated subprocess without running
    // any coordinator, slab, or lock destructor, matching process death.
    unsafe {
        libc::_exit(73);
    }
}

#[test]
fn crash_publish_boundaries() {
    for boundary in ["prepared", "renamed", "synced"] {
        let path = std::env::temp_dir().join(format!(
            "racer-resize-crash-{}-{boundary}",
            std::process::id()
        ));
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "runtime::storage::tests::crash_publish_child",
                "--ignored",
                "--nocapture",
            ])
            .env("RACER_RESIZE_CRASH_PATH", &path)
            .env("RACER_RESIZE_CRASH_BOUNDARY", boundary)
            .output()
            .unwrap();
        assert_eq!(
            output.status.code(),
            Some(73),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert!(String::from_utf8_lossy(&output.stdout).contains("running 1 test"));
        let locked = StoragePath::lock(&path).unwrap();
        assert!(!locked.candidate.exists());
        let mut slab = Slab::open_existing_layout(locked.active(), 1).unwrap();
        assert_eq!(
            slab.size(),
            if boundary == "prepared" {
                512 << 20
            } else {
                20 << 30
            }
        );
        for id in 0..slab.shard_count() {
            let shard = slab.take_shard(crate::sharding::ShardId::at(id)).unwrap();
            let allocator =
                crate::allocator::Allocator::open_inner(shard, Default::default()).unwrap();
            assert!(allocator.is_empty());
        }
        drop(slab);
        drop(locked);
        std::fs::remove_file(&path).unwrap();
        let mut lock = path.into_os_string();
        lock.push(".lock");
        std::fs::remove_file(lock).unwrap();
    }
}

struct Fixture {
    path: PathBuf,
    coordinator: Option<StorageCoordinator>,
    nodes: Vec<(Volumes, uring::Ring)>,
    updates: Arc<Updates>,
}
impl Fixture {
    fn new(workers: usize) -> Self {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let path = std::env::temp_dir().join(format!(
            "racer-resize-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let locked = StoragePath::lock(&path).unwrap();
        let plan: Arc<[Placement]> = crate::workers::sharding::placements(
            (0..workers)
                .map(|i| (crate::workers::CpuId(i), crate::workers::NumaNodeId(0)))
                .collect(),
            workers,
        )
        .unwrap()
        .into();
        let budget = CheckpointBudget::default();
        let mut slab = LayoutPlan::new(workers as u64 * (512 << 20), workers)
            .unwrap()
            .create(locked.active(), budget.clone())
            .unwrap();
        let generation = plan[0].storage_generation(workers).unwrap();
        let updates = Arc::new(Updates::default());
        let coordinator =
            StorageCoordinator::start(locked, &slab, &plan[0], budget, updates.clone()).unwrap();
        let mut nodes = Vec::new();
        for i in 0..workers {
            let context = WorkerContext::pinned(plan.clone(), i);
            let pool = crate::buffers::io_test_pool(4);
            let ring = uring::Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
            let shards = generation
                .take_assignments(&context)
                .unwrap()
                .into_iter()
                .map(|assignment| {
                    let shard = slab.take_shard(assignment.id()).unwrap();
                    ShardState::activate(&context, assignment, shard, &pool, Default::default())
                        .unwrap()
                })
                .collect();
            let cache = Cache::for_generation(
                &context,
                &generation,
                Namespace::new("bootstrap").unwrap(),
                shards,
            )
            .unwrap();
            updates.subscribe(ring.wake_handle());
            let app = Volumes::new(
                cache,
                updates.clone(),
                Arc::new(crate::crypto::Pool::test_pool(&pool)),
                i,
            )
            .with_storage(coordinator.handle(), context);
            nodes.push((app, ring));
        }
        drop(slab);
        Self {
            path,
            coordinator: Some(coordinator),
            nodes,
            updates,
        }
    }
    fn shared(&self) -> Arc<Shared> {
        self.coordinator.as_ref().unwrap().shared.clone()
    }
    fn turn(&mut self) {
        for (app, ring) in &mut self.nodes {
            ring.progress().unwrap();
            app.poll(ring, 128).unwrap();
        }
        thread::sleep(Duration::from_millis(1));
    }
    fn until(&mut self, done: impl Fn(&Self) -> bool) {
        let end = Instant::now() + Duration::from_secs(10);
        while !done(self) {
            assert!(
                Instant::now() < end,
                "resize stalled: {:?}, phase={:?}",
                self.updates.storage_policy_status(),
                self.shared().transaction.lock().unwrap().phase
            );
            self.turn();
        }
    }
    fn applied(&mut self, version: u64, capacity: u64) {
        self.updates.test_storage_policy(version, capacity);
        self.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
        assert_eq!(self.updates.storage_policy_status().applied_bytes, capacity);
        let status = self.updates.status();
        assert_eq!(status["storage"]["appliedVersion"], version);
        assert_eq!(
            status["storage"]["shards"],
            LayoutPlan::new(capacity, self.nodes.len())
                .unwrap()
                .shard_count()
        );
        assert_eq!(std::fs::metadata(&self.path).unwrap().len(), capacity);
        assert!(self.nodes.iter().all(|(app, _)| !app.topology_fence.held()));
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        for (app, ring) in &mut self.nodes {
            app.shutdown(ring).unwrap();
            ring.shutdown().unwrap();
        }
        self.nodes.clear();
        self.coordinator.take();
        let _ = std::fs::remove_file(&self.path);
        for suffix in [".resize", ".lock"] {
            let mut path = self.path.as_os_str().to_owned();
            path.push(suffix);
            let _ = std::fs::remove_file(path);
        }
    }
}

#[test]
fn live_multiworker_grow_shrink_and_same_capacity_noop() {
    let mut f = Fixture::new(2);
    let original_inode = std::fs::metadata(&f.path).unwrap().ino();
    let original_rings: Vec<_> = f.nodes.iter().map(|(_, r)| r.identity().clone()).collect();
    f.applied(1, 1 << 30);
    assert_eq!(std::fs::metadata(&f.path).unwrap().ino(), original_inode);
    assert_eq!(f.shared().transaction.lock().unwrap().id, 0);
    f.applied(2, 50 << 30);
    assert_eq!(f.updates.status()["storage"]["shards"], 8);
    assert_ne!(std::fs::metadata(&f.path).unwrap().ino(), original_inode);
    f.applied(3, 7 * (512 << 20)); // seven shards, two workers, uneven local counts
    assert_eq!(f.updates.status()["storage"]["shards"], 7);
    f.applied(4, 1 << 30);
    for ((_, ring), original) in f.nodes.iter().zip(original_rings) {
        assert!(Rc::ptr_eq(ring.identity(), &original));
    }
    let inode = std::fs::metadata(&f.path).unwrap().ino();
    f.applied(5, 1 << 30);
    assert_eq!(std::fs::metadata(&f.path).unwrap().ino(), inode);
}

#[test]
fn failures_preserve_old_inode_then_retry_and_supersede() {
    let mut f = Fixture::new(2);
    let inode = std::fs::metadata(&f.path).unwrap().ino();
    for (index, faults) in [
        TestFaults {
            prepare: true,
            ..Default::default()
        },
        TestFaults {
            stage_worker: Some(1),
            ..Default::default()
        },
        TestFaults {
            drain_timeout: true,
            ..Default::default()
        },
        TestFaults {
            publish: true,
            ..Default::default()
        },
    ]
    .into_iter()
    .enumerate()
    {
        *f.shared().faults.lock().unwrap() = faults;
        f.updates.test_storage_policy(index as u64 + 1, 2 << 30);
        f.until(|f| {
            matches!(
                f.updates.storage_policy_status().result,
                Some(StorageResult::Failed(_))
            )
        });
        assert_eq!(std::fs::metadata(&f.path).unwrap().ino(), inode);
        assert!(f.nodes.iter().all(|(app, _)| !app.topology_fence.held()));
        let status = f.updates.status();
        assert_eq!(status["storage"]["phase"], "failed");
        assert_eq!(status["storage"]["appliedBytes"], 1 << 30);
        assert_eq!(status["storage"]["shards"], 2);
        assert_eq!(status["lastError"], serde_json::Value::Null);
        let id = f.shared().transaction.lock().unwrap().id;
        for _ in 0..20 {
            f.turn();
        }
        assert_eq!(
            f.shared().transaction.lock().unwrap().id,
            id,
            "retry must back off"
        );
    }
    *f.shared().faults.lock().unwrap() = TestFaults::default();
    // Same request retries without another control publication.
    f.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
    assert_eq!(std::fs::metadata(&f.path).unwrap().len(), 2 << 30);
    f.updates.test_storage_policy(5, 4 << 30);
    // Pin a real cache owner so the production fence cannot complete.
    let old_owner = f.nodes[0].0.cache.borrow().use_guard();
    f.until(|f| f.nodes.iter().all(|(app, _)| app.topology_fence.held()));
    f.updates.test_storage_policy(6, 3 << 30);
    drop(old_owner);
    f.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
    assert_eq!(std::fs::metadata(&f.path).unwrap().len(), 3 << 30);
}

#[test]
fn over_automatic_capacity_limit_reports_bounded_failure_and_keeps_serving() {
    let mut f = Fixture::new(1);
    let inode = std::fs::metadata(&f.path).unwrap().ino();
    f.updates.test_storage_policy(1, (4u64 << 40) + (64 << 20));
    f.until(|f| {
        matches!(
            f.updates.storage_policy_status().result,
            Some(StorageResult::Failed(_))
        )
    });
    let status = f.updates.storage_policy_status();
    let Some(StorageResult::Failed(error)) = status.result else {
        unreachable!()
    };
    assert!(error.contains("512 MiB..=4 TiB"), "{error}");
    assert!(error.len() < 1024);
    assert_eq!(std::fs::metadata(&f.path).unwrap().ino(), inode);
    assert!(f.nodes.iter().all(|(app, _)| !app.topology_fence.held()));
    f.updates.test_storage_policy(2, 1 << 30);
    f.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
    assert_eq!(std::fs::metadata(&f.path).unwrap().len(), 1 << 30);
}

#[test]
fn delayed_worker_ack_prevents_publish_and_partial_resume() {
    let mut f = Fixture::new(2);
    let inode = std::fs::metadata(&f.path).unwrap().ino();
    f.updates.test_storage_policy(1, 2 << 30);
    f.until(|f| f.shared().transaction.lock().unwrap().phase == Phase::Stage);
    for _ in 0..30 {
        let (app, ring) = &mut f.nodes[0];
        ring.progress().unwrap();
        app.poll(ring, 128).unwrap();
        thread::sleep(Duration::from_millis(1));
    }
    assert_eq!(std::fs::metadata(&f.path).unwrap().ino(), inode);
    f.until(|f| f.shared().transaction.lock().unwrap().phase == Phase::Install);
    for _ in 0..30 {
        let (app, ring) = &mut f.nodes[0];
        ring.progress().unwrap();
        app.poll(ring, 128).unwrap();
        thread::sleep(Duration::from_millis(1));
    }
    assert!(f.nodes.iter().all(|(app, _)| app.topology_fence.held()));
    assert_ne!(
        f.updates.storage_policy_status().result,
        Some(StorageResult::Applied)
    );
    f.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
}

#[test]
fn post_rename_sync_failure_stays_fenced_and_recovers_forward() {
    let mut f = Fixture::new(1);
    f.shared().faults.lock().unwrap().sync = true;
    f.updates.test_storage_policy(1, 1 << 30);
    f.until(|f| {
        matches!(
            f.updates.storage_policy_status().result,
            Some(StorageResult::Failed(_))
        )
    });
    assert_eq!(std::fs::metadata(&f.path).unwrap().len(), 1 << 30);
    assert!(f.nodes[0].0.topology_fence.held());
    assert!(StoragePath::lock(&f.path).is_err());
    f.shared().faults.lock().unwrap().sync = false;
    f.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
}

#[test]
fn restart_selects_complete_inode_on_both_sides_of_publish() {
    for publish in [false, true] {
        let mut f = Fixture::new(1);
        let path = f.path.clone();
        let shared = f.shared();
        shared.faults.lock().unwrap().sync = publish;
        let owner = f.nodes[0].0.cache.borrow().use_guard();
        f.updates.test_storage_policy(1, 1 << 30);
        f.until(|f| f.nodes[0].0.topology_fence.held());
        drop(owner);
        if publish {
            f.until(|f| {
                matches!(
                    f.updates.storage_policy_status().result,
                    Some(StorageResult::Failed(_))
                )
            });
        }
        // Abrupt coordinator stop at a known boundary, then release all file
        // owners. The durable namespace is the same one a new daemon opens.
        shared.stopping.store(true, Ordering::Release);
        for (app, ring) in &mut f.nodes {
            app.shutdown(ring).unwrap();
            ring.shutdown().unwrap();
        }
        f.nodes.clear();
        f.coordinator.take();
        let locked = StoragePath::lock(&path).unwrap();
        assert!(!locked.candidate.exists());
        let slab = Slab::open_existing_layout(locked.active(), 1).unwrap();
        assert_eq!(slab.size(), if publish { 1 << 30 } else { 512 << 20 });
        drop(slab);
        drop(locked);
        // Drop expects a coordinator only while polling; teardown is idempotent.
    }
}

#[test]
fn resource_validation_accepts_multi_tib_with_available_memory() {
    for (old_capacity, capacity) in [
        (512 << 20, 2 << 40),
        (2 << 40, 4 << 40),
        (4 << 40, 512 << 20),
    ] {
        let old = LayoutPlan::new(old_capacity, 1).unwrap().resources();
        let plan = LayoutPlan::new(capacity, 1).unwrap();
        let peak = plan.empty_preparation_bytes() + old.checkpoint_peak_bytes + (64 << 20);
        assert!(peak < 512 << 20);
        assert!(validate_resources(old, plan, peak).is_ok());
        assert!(validate_resources(old, plan, peak - 1).is_err());
        assert!(validate_resources(old, plan, 0).is_err());
        // Changing the diagnostic populated-tree bound must not change empty
        // replacement admission or count already-resident memory a second time.
        let diagnostic = ResourceEstimate {
            resident_index_bytes: u64::MAX / 2,
            ..old
        };
        assert!(validate_resources(diagnostic, plan, peak).is_ok());
    }
}

#[test]
fn startup_checks_incremental_bitmap_and_recovery_floor() {
    for workers in [1, 2, 32] {
        let resources = LayoutPlan::new(4 << 40, workers).unwrap().resources();
        let required = startup_allowance(resources, workers);
        assert!(required < 512 << 20);
        assert!(validate_headroom(required, required).is_ok());
        assert!(validate_headroom(required, required - 1).is_err());
        assert_eq!(
            required,
            startup_allowance(
                ResourceEstimate {
                    resident_index_bytes: u64::MAX / 2,
                    ..resources
                },
                workers
            )
        );
    }
}

#[test]
fn live_http_request_drains_busy_fence_and_refills_after_resize() {
    use crate::http_client as client;
    let mut f = Fixture::new(1);
    let stop = Arc::new(AtomicBool::new(false));
    let hits = Arc::new(Mutex::new(Vec::new()));
    let (origin, thread) = crate::conformance::origin(0, stop.clone(), hits.clone());
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    let (trust, mut config) = crate::control::tests::fixture();
    config.peers.clear();
    let volume = &mut config.volumes[0];
    volume.cache_socket = crate::control::tests::test_socket(address, "cache");
    volume.origin_socket = crate::control::tests::test_socket(origin, "origin");
    volume.peers.clear();
    let topology = volume.topology.as_mut().unwrap();
    topology.local_slots = vec![0, 1];
    topology.product = Some(crate::control::proto::ProductTopology {
        left_factor: 1,
        right_factor: 1,
        members: vec!["02".repeat(32)],
        roles: vec![0],
        local_member: 0,
        candidate_width: 1,
        candidates: vec![0; topology.slot_count as usize],
    });
    f.updates
        .publish(crate::control::tests::prepare_snapshot(
            &trust,
            config.clone(),
        ))
        .unwrap();
    f.until(|f| !f.nodes[0].0.servers.is_empty());
    let mut client_ring = crate::conformance::ring(4, Default::default());
    let request = |ring: &uring::Ring| {
        client::Connection::new_address(
            crate::socket::Address::unix(&crate::control::tests::test_socket(address, "cache"))
                .unwrap(),
            "localhost",
        )
        .unwrap()
        .get(
            client::Request::new("/resize", &[]).unwrap(),
            ring.pool().private_fill().unwrap(),
            Instant::now() + Duration::from_secs(5),
        )
        .unwrap()
    };
    let mut first = request(&client_ring);
    let end = Instant::now() + Duration::from_secs(5);
    loop {
        client_ring.progress().unwrap();
        assert!(matches!(
            first.poll(&mut client_ring, 1).unwrap(),
            Progress::Pending(_)
        ));
        f.turn();
        if !f.nodes[0].0.cache.borrow().maintenance_idle() {
            break;
        }
        assert!(Instant::now() < end);
    }
    let ring_identity = f.nodes[0].1.identity().clone();
    f.updates.test_storage_policy(1, 20 << 30);
    // Hold a cache owner past the first response to inspect the admission fence.
    let owner = f.nodes[0].0.cache.borrow().use_guard();
    f.until(|f| f.nodes[0].0.topology_fence.held());
    let inode = std::fs::metadata(&f.path).unwrap().ino();
    let mut busy = request(&client_ring);
    let mut first_done = false;
    let mut busy_done = false;
    while !first_done || !busy_done {
        client_ring.progress().unwrap();
        f.turn();
        if !first_done
            && let Progress::Ready(mut response) = first.poll(&mut client_ring, 32).unwrap()
        {
            assert_eq!(response.status(), 200);
            assert_eq!(response.body(), b"abc");
            first_done = true;
        }
        if !busy_done && let Progress::Ready(response) = busy.poll(&mut client_ring, 32).unwrap() {
            assert_eq!(response.status(), 503);
            busy_done = true;
        }
        assert!(Instant::now() < end);
    }
    assert_eq!(std::fs::metadata(&f.path).unwrap().ino(), inode);
    // Topology publication is accepted but worker activation waits for storage.
    config.revision += 1;
    config.volumes[0].topology.as_mut().unwrap().epoch += 1;
    f.updates
        .publish(crate::control::tests::prepare_snapshot(
            &trust,
            config.clone(),
        ))
        .unwrap();
    for _ in 0..10 {
        f.turn();
    }
    assert_ne!(f.nodes[0].0.revision, config.revision);
    drop(owner);
    f.until(|f| f.updates.storage_policy_status().result == Some(StorageResult::Applied));
    f.until(|f| f.nodes[0].0.revision == config.revision);
    assert!(Rc::ptr_eq(f.nodes[0].1.identity(), &ring_identity));
    let before = hits.lock().unwrap().len();
    let mut second = request(&client_ring);
    loop {
        client_ring.progress().unwrap();
        f.turn();
        if let Progress::Ready(mut response) = second.poll(&mut client_ring, 32).unwrap() {
            assert_eq!(response.status(), 200);
            assert_eq!(response.body(), b"abc");
            break;
        }
        assert!(Instant::now() < end);
    }
    assert!(
        hits.lock().unwrap().len() > before,
        "new inode refills from origin"
    );
    drop((first, busy, second));
    client_ring.shutdown().unwrap();
    stop.store(true, Ordering::Relaxed);
    thread.join().unwrap();
}

#[test]
fn retained_old_inode_prevents_a_third_generation_and_shutdown_never_resumes() {
    let mut f = Fixture::new(1);
    let old = {
        let (app, ring) = &mut f.nodes[0];
        crate::cache::tests::file_for_resize(&mut app.cache.borrow_mut(), ring)
    };
    f.applied(1, 1 << 30);
    let id = f.shared().transaction.lock().unwrap().id;
    f.updates.test_storage_policy(2, 1536 << 20);
    for _ in 0..40 {
        f.turn();
    }
    assert_eq!(f.shared().transaction.lock().unwrap().id, id);
    // FileValue still reads the exact old inode after the pathname changed.
    let ring = &mut f.nodes[0].1;
    let mut ticket = old.read(ring, ring.pool().private_fill().unwrap()).unwrap();
    let end = Instant::now() + Duration::from_secs(5);
    loop {
        ring.progress().unwrap();
        if let Some(result) = ring.take_read(&mut ticket).unwrap() {
            assert_eq!(result.result.unwrap(), 3);
            assert_eq!(result.resource.publish(3).unwrap().as_slice(), b"xxx");
            break;
        }
        assert!(Instant::now() < end);
    }
    drop(old);
    let owner = f.nodes[0].0.cache.borrow().use_guard();
    f.until(|f| f.nodes[0].0.topology_fence.held());
    f.nodes[0].0.begin_drain();
    drop(owner);
    for _ in 0..30 {
        f.turn();
    }
    assert!(f.nodes[0].0.topology_fence.held());
    assert_eq!(std::fs::metadata(&f.path).unwrap().len(), 1 << 30);
}
