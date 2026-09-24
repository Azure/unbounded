// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

pub(crate) fn physical_core_count() -> io::Result<usize> {
    Ok(discover()?
        .iter()
        .map(|cpu| cpu.siblings[0])
        .collect::<BTreeSet<_>>()
        .len())
}

#[cfg(test)]
mod reentrant_tests {
    use super::*;
    use crate::buffers::{self, NetworkDependency, NetworkFlightKey, NetworkProgress};
    use std::mem::ManuallyDrop;
    use std::task::{RawWaker, RawWakerVTable, Waker};
    fn key() -> NetworkFlightKey {
        NetworkFlightKey {
            value: [7; 32],
            routing: [0; 32],
            version: 0,
            destination: 0,
            dependency: NetworkDependency::LocalShared,
        }
    }
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum Callback {
        Clone,
        Drop,
        Wake,
        WakeByRef,
    }
    struct Probe {
        pool: buffers::TestPoolLink,
        events: Mutex<Vec<Callback>>,
    }
    impl Probe {
        fn new(pool: &buffers::WorkerPool) -> Arc<Self> {
            Arc::new(Self {
                pool: pool.test_link(),
                events: Mutex::new(Vec::new()),
            })
        }
        fn callback(&self, callback: Callback) {
            let pool = self.pool.for_worker();
            // Reenter both allocation and the same network registry from every raw
            // callback. No callback may run with a registry or state lock held.
            drop(pool.private_fill().unwrap());
            drop(pool.network_flight(key()).unwrap());
            self.events.lock().unwrap().push(callback);
        }
        fn expect(&self, expected: &[Callback]) {
            let events = std::mem::take(&mut *self.events.lock().unwrap());
            assert_eq!(events, expected, "callback lock scope / reentry mismatch");
        }
    }
    fn raw_waker(probe: &Arc<Probe>) -> Waker {
        fn require_send_sync<T: Send + Sync>() {}
        require_send_sync::<Probe>();
        // SAFETY: the vtable preserves one Arc per owned raw pointer.
        unsafe { Waker::from_raw(raw(probe.clone())) }
    }
    fn raw(probe: Arc<Probe>) -> RawWaker {
        RawWaker::new(Arc::into_raw(probe).cast(), &VTABLE)
    }
    unsafe fn clone_waker(data: *const ()) -> RawWaker {
        // SAFETY: borrow the source reference, create an independently owned clone.
        let probe = ManuallyDrop::new(unsafe { Arc::from_raw(data.cast::<Probe>()) });
        probe.callback(Callback::Clone);
        raw(Arc::clone(&probe))
    }
    unsafe fn drop_waker(data: *const ()) {
        // SAFETY: consumes exactly this Waker's reference.
        let probe = unsafe { Arc::from_raw(data.cast::<Probe>()) };
        probe.callback(Callback::Drop);
    }
    unsafe fn wake_waker(data: *const ()) {
        // SAFETY: consumes exactly this Waker's reference.
        let probe = unsafe { Arc::from_raw(data.cast::<Probe>()) };
        probe.callback(Callback::Wake);
    }
    unsafe fn wake_waker_by_ref(data: *const ()) {
        // SAFETY: borrows the live reference without consuming it.
        let probe = ManuallyDrop::new(unsafe { Arc::from_raw(data.cast::<Probe>()) });
        probe.callback(Callback::WakeByRef);
    }
    static VTABLE: RawWakerVTable =
        RawWakerVTable::new(clone_waker, wake_waker, wake_waker_by_ref, drop_waker);
    #[test]
    fn poll_clone_replacement_and_waiter_unregistration_are_reentrant() {
        for cancel in [false, true] {
            for unregister in [false, true] {
                let pool = buffers::io_test_pool(2);
                let mut producer = pool.network_flight(key()).unwrap();
                assert!(matches!(
                    producer.poll(Waker::noop()),
                    NetworkProgress::Produce
                ));
                let mut pending = pool.network_flight(key()).unwrap();
                let old = Probe::new(&pool);
                let current = Probe::new(&pool);
                let old_waker = raw_waker(&old);
                let current_waker = raw_waker(&current);
                assert!(matches!(pending.poll(&old_waker), NetworkProgress::Pending));
                old.expect(&[Callback::Clone]);
                assert!(matches!(
                    pending.poll(&current_waker),
                    NetworkProgress::Pending
                ));
                current.expect(&[Callback::Clone]);
                old.expect(&[Callback::Drop]);
                let mut abandoned = pool.network_flight(key()).unwrap();
                assert!(matches!(
                    abandoned.poll(&old_waker),
                    NetworkProgress::Pending
                ));
                old.expect(&[Callback::Clone]);
                drop(abandoned);
                old.expect(&[Callback::Drop]);
                let mut pending = Some(pending);
                if unregister {
                    drop(pending.take());
                    current.expect(&[Callback::Drop]);
                }
                if cancel {
                    drop(producer);
                } else {
                    producer.finish(Err(Arc::new(crate::cache::Error::InvalidData("terminal"))));
                    drop(producer);
                }
                // Neither the replaced registration nor the abandoned waiter
                // may wake. The retained replacement must wake exactly once,
                // after all registrations are detached and locks released.
                old.expect(&[]);
                if unregister {
                    current.expect(&[]);
                } else {
                    current.expect(&[Callback::Wake]);
                }
                for pending in pending.iter_mut() {
                    match pending.poll(&current_waker) {
                        NetworkProgress::Produce if cancel => {}
                        NetworkProgress::Ready(Err(_)) if !cancel => {}
                        _ => panic!("replacement waiter observed wrong terminal result"),
                    }
                    current.expect(&[Callback::Clone, Callback::Drop]);
                }
                drop(pending);
                old.expect(&[]);
                current.expect(&[]);
                drop(old_waker);
                drop(current_waker);
                old.expect(&[Callback::Drop]);
                current.expect(&[Callback::Drop]);
                assert_eq!(Arc::strong_count(&old), 1);
                assert_eq!(Arc::strong_count(&current), 1);
                pool.assert_recovered();
            }
        }
    }
}

#[cfg(test)]
pub(crate) mod pool_tests {
    use super::*;
    use crate::buffers::{self, Exhausted, Key};
    use std::sync::{Barrier, atomic::AtomicUsize, mpsc};
    use std::task::Waker;
    use std::time::Duration;

    pub(crate) struct ThreadWake(thread::Thread);
    impl Wake for ThreadWake {
        fn wake(&self) {
            self.0.unpark();
        }
    }
    pub(crate) struct ParkDriver(Arc<ThreadWake>);
    impl ParkDriver {
        pub(crate) fn current() -> Self {
            Self(Arc::new(ThreadWake(thread::current())))
        }
    }
    impl Driver for ParkDriver {
        type Wake = ThreadWake;
        fn wake_handle(&self) -> Arc<ThreadWake> {
            self.0.clone()
        }
        fn turn(&mut self) -> io::Result<()> {
            thread::park();
            Ok(())
        }
        fn shutdown(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    pub(crate) fn on_pinned_worker(
        check: impl Fn(&WorkerContext) -> io::Result<()> + Send + Sync + 'static,
    ) {
        let config = Config {
            shard_count: NonZeroUsize::new(1).unwrap(),
        };
        let workers = Workers::start(config, move |placement| {
            check(placement)?;
            Ok(ParkDriver::current())
        })
        .unwrap();
        workers.stop_handle().request_stop();
        workers.join().unwrap();
    }
    #[test]
    fn pinned_workers_share_only_their_node_pool() {
        let registry = Arc::new(buffers::Pools::new(buffers::Config::new(
            NonZeroUsize::new(2).unwrap(),
        )));
        let (sender, receiver) = mpsc::channel();
        let workers = Workers::start(Config::default(), move |placement| {
            let pool = registry.test_for_worker(placement)?;
            let node = placement.numa_node_id();
            assert_eq!(pool.numa_node_id(), node);
            sender
                .send((node, pool.memory_lease().region().address as usize))
                .unwrap();
            Ok(ParkDriver::current())
        })
        .unwrap();
        let mut addresses = BTreeMap::new();
        for _ in workers.placements() {
            let (node, address) = receiver.recv().unwrap();
            assert_eq!(*addresses.entry(node).or_insert(address), address);
        }
        assert_eq!(
            addresses.values().collect::<BTreeSet<_>>().len(),
            addresses.len()
        );
        workers.stop_handle().request_stop();
        workers.join().unwrap();
    }
    fn key(value: u64) -> Key {
        let mut digest = [0; 32];
        digest[..8].copy_from_slice(&value.to_le_bytes());
        Key::new(digest)
    }
    #[test]
    fn demand_paged_machines_preserve_addresses_ownership_and_recycled_bytes() {
        let config = buffers::Config::new(NonZeroUsize::new(1).unwrap());
        // 4 GiB virtual, only two touched base pages per machine. No slab files.
        let pools: Vec<_> = (0..1024)
            .map(|_| buffers::test_pool(config, NumaNodeId(0), true))
            .collect();
        let mut ranges = Vec::new();
        for (machine, pool) in pools.iter().enumerate() {
            let lease = pool.memory_lease();
            lease.test_residency(false);
            let base = lease.region().address as usize;
            assert_eq!(lease.region().len, buffers::BUFFER_SIZE);
            ranges.push((base, base + lease.region().len));
            let shared = pool.test_other_worker();
            assert!(pool.same_pool(&shared));
            assert_eq!(shared.memory_lease().region().address as usize, base);
            let mut fill = pool.private_fill().unwrap();
            fill.as_mut_slice()[..8].copy_from_slice(&(machine as u64).to_le_bytes());
            fill.as_mut_slice()[buffers::BUFFER_SIZE - 1] = 37;
            let buffer = fill.publish(8).unwrap();
            assert_eq!(buffer.as_slice(), &(machine as u64).to_le_bytes());
            assert_eq!(pool.invariant_snapshot().refs, [1]);
            assert!(shared.private_fill().is_err());
            drop(buffer);
            let mut reused = shared.private_fill().unwrap();
            assert_eq!(reused.region().region.address as usize, base);
            assert_eq!(&reused.as_mut_slice()[..8], &(machine as u64).to_le_bytes());
            assert_eq!(reused.as_mut_slice()[buffers::BUFFER_SIZE - 1], 37);
            drop(reused);
            pool.assert_recovered();
        }
        ranges.sort_unstable();
        assert!(ranges.windows(2).all(|pair| pair[0].1 <= pair[1].0));
        let other = buffers::test_pool(config, NumaNodeId(1), true);
        assert!(!pools[0].same_pool(&other));
        assert_eq!(other.numa_node_id(), NumaNodeId(1));
        buffers::io_test_pool_config(config)
            .memory_lease()
            .test_residency(true);
    }
    #[test]
    fn network_two_consumer_cancel_and_cross_worker_takeover() {
        use buffers::{NetworkDependency, NetworkFlightKey, NetworkProgress};
        let key = NetworkFlightKey {
            value: [1; 32],
            routing: [9; 32],
            version: 2,
            destination: 3,
            dependency: NetworkDependency::LocalShared,
        };
        let pool = buffers::io_test_pool(3);
        let mut producer = pool.network_flight(key.clone()).unwrap();
        assert!(matches!(
            producer.poll(Waker::noop()),
            NetworkProgress::Produce
        ));
        let mut canceled = pool.network_flight(key.clone()).unwrap();
        assert!(matches!(
            canceled.poll(Waker::noop()),
            NetworkProgress::Pending
        ));
        drop(canceled);
        let mut joined = pool.network_flight(key.clone()).unwrap();
        assert!(matches!(
            producer.poll(Waker::noop()),
            NetworkProgress::Produce
        ));
        let link = pool.test_link();
        let (tx, rx) = mpsc::channel();
        let (release, released) = mpsc::channel();
        let worker = thread::spawn(move || {
            let pool = link.for_worker();
            let mut survivor = pool.network_flight(key.clone()).unwrap();
            assert!(matches!(
                survivor.poll(Waker::noop()),
                NetworkProgress::Pending
            ));
            tx.send(()).unwrap();
            released.recv_timeout(Duration::from_secs(5)).unwrap();
            assert!(matches!(
                survivor.poll(Waker::noop()),
                NetworkProgress::Produce
            ));
            let mut fill = pool.stage(Key::new(key.value)).unwrap();
            fill.as_mut_slice()[0] = 42;
            let buffer = fill.publish(1).unwrap();
            survivor.finish(Ok(&buffer));
        });
        rx.recv_timeout(Duration::from_secs(5)).unwrap();
        drop(producer);
        release.send(()).unwrap();
        worker.join().unwrap();
        let NetworkProgress::Ready(Ok(buffer)) = joined.poll(Waker::noop()) else {
            panic!("missing shared result")
        };
        assert_eq!(buffer.as_slice(), &[42]);
        drop((joined, buffer));
        pool.assert_recovered();
    }
    #[test]
    #[ignore = "requires mbind and get_mempolicy permissions"]
    fn real_numa_binding_and_prefault() {
        let registry = buffers::Pools::new(buffers::Config::new(NonZeroUsize::new(1).unwrap()));
        let workers = Workers::start(Config::default(), move |placement| {
            let pool = registry.for_worker(placement)?;
            let lease = pool.memory_lease();
            let mut node: libc::c_int = -1;
            // SAFETY: output is writable; address belongs to a populated mapping.
            let result = unsafe {
                libc::syscall(
                    libc::SYS_get_mempolicy,
                    &mut node as *mut libc::c_int,
                    std::ptr::null_mut::<libc::c_ulong>(),
                    0usize,
                    lease.region().address,
                    1u32 | 2u32, // MPOL_F_NODE | MPOL_F_ADDR
                )
            };
            if result != 0 {
                return Err(io::Error::last_os_error());
            }
            assert_eq!(node as usize, placement.numa_node_id().0);
            Ok(ParkDriver::current())
        })
        .unwrap();
        workers.stop_handle().request_stop();
        workers.join().unwrap();
    }
    #[test]
    #[ignore = "requires mbind and move_pages query permissions"]
    fn real_numa_whole_region_placement_and_prefault_across_three_buffers() {
        on_pinned_worker(|placement| {
            let registry = buffers::Pools::new(buffers::Config::new(NonZeroUsize::new(3).unwrap()));
            let pool = registry.for_worker(placement)?;
            let lease = pool.memory_lease();
            assert_eq!(lease.region().len, 3 * buffers::BUFFER_SIZE);
            assert_eq!(lease.buffers().len(), 3);
            lease.test_residency(true);
            // SAFETY: sysconf has no pointer arguments.
            let page_size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
            let pages: Vec<*mut libc::c_void> = (0..lease.region().len)
                .step_by(page_size)
                .map(|offset| {
                    // SAFETY: each offset is inside the still-leased mapping.
                    unsafe { lease.region().address.add(offset).cast() }
                })
                .collect();
            let mut status = vec![-1 as libc::c_int; pages.len()];
            // SAFETY: arrays contain count entries. Null nodes requests query
            // only; absent pages report ENOENT rather than being faulted in.
            let result = unsafe {
                libc::syscall(
                    libc::SYS_move_pages,
                    0 as libc::pid_t,
                    pages.len() as libc::c_ulong,
                    pages.as_ptr(),
                    std::ptr::null::<libc::c_int>(),
                    status.as_mut_ptr(),
                    0 as libc::c_int,
                )
            };
            if result < 0 {
                return Err(io::Error::last_os_error());
            }
            assert_eq!(result, 0);
            for (page, node) in status.into_iter().enumerate() {
                assert_eq!(
                    node,
                    placement.numa_node_id().0 as libc::c_int,
                    "wrong placement or absent page {page}"
                );
            }
            Ok(())
        });
    }
    struct ChannelWake(mpsc::Sender<()>);
    impl std::task::Wake for ChannelWake {
        fn wake(self: Arc<Self>) {
            self.0.send(()).unwrap();
        }
    }
    #[test]
    fn concurrent_single_flight_and_publication_poll_races() {
        use buffers::{NetworkDependency, NetworkFlightKey, NetworkProgress};
        let pool = buffers::io_test_pool(4);
        let barrier = Arc::new(Barrier::new(8));
        let fills = Arc::new(AtomicUsize::new(0));
        let threads: Vec<_> = (0..8)
            .map(|_| {
                let link = pool.test_link();
                let barrier = barrier.clone();
                let fills = fills.clone();
                thread::spawn(move || {
                    let pool = link.for_worker();
                    let (sender, notifications) = mpsc::channel();
                    let waker = Waker::from(Arc::new(ChannelWake(sender)));
                    for round in 0u64..200 {
                        barrier.wait();
                        let mut value = [0; 32];
                        value[..8].copy_from_slice(&round.to_le_bytes());
                        let mut request = pool
                            .network_flight(NetworkFlightKey {
                                value,
                                routing: [0; 32],
                                version: 0,
                                destination: 0,
                                dependency: NetworkDependency::LocalShared,
                            })
                            .unwrap();
                        barrier.wait(); // All requests join before publication.
                        let buffer = loop {
                            match request.poll(&waker) {
                                NetworkProgress::Produce => {
                                    fills.fetch_add(1, Ordering::Relaxed);
                                    let mut fill = pool.stage(key(round)).unwrap();
                                    fill.as_mut_slice()[..8].copy_from_slice(&round.to_le_bytes());
                                    let buffer = fill.publish(8).unwrap();
                                    request.finish(Ok(&buffer));
                                    break buffer;
                                }
                                NetworkProgress::Ready(result) => break result.unwrap(),
                                NetworkProgress::Pending => notifications
                                    .recv_timeout(Duration::from_secs(5))
                                    .expect("publication lost a wakeup"),
                                _ => panic!("unexpected result"),
                            }
                        };
                        assert_eq!(buffer.as_slice(), &round.to_le_bytes());
                        barrier.wait();
                        drop(buffer);
                        drop(request);
                        barrier.wait();
                    }
                })
            })
            .collect();
        for thread in threads {
            thread.join().unwrap();
        }
        assert_eq!(fills.load(Ordering::Relaxed), 200);
        pool.assert_recovered();
    }
    #[test]
    fn concurrent_allocation_never_recycles_live_payloads() {
        let pool = buffers::io_test_pool(4);
        let threads: Vec<_> = (0..8)
            .map(|worker| {
                let link = pool.test_link();
                thread::spawn(move || {
                    let pool = link.for_worker();
                    for round in 0..500 {
                        let value: u64 = worker * 500 + round;
                        let buffer = loop {
                            match pool.stage(key(value)) {
                                Ok(mut fill) => {
                                    fill.as_mut_slice()[..8].copy_from_slice(&value.to_le_bytes());
                                    break fill.publish(8).unwrap();
                                }
                                Err(Exhausted) => thread::yield_now(),
                            }
                        };
                        let clone = buffer.clone();
                        thread::yield_now();
                        drop(buffer);
                        assert_eq!(clone.as_slice(), &value.to_le_bytes());
                    }
                })
            })
            .collect();
        for thread in threads {
            thread.join().unwrap();
        }
        pool.assert_recovered();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::{Child, Command, ExitStatus, Stdio};
    use std::sync::mpsc::{self, Receiver, Sender};
    use std::time::{Duration, Instant};

    fn wait_with_deadline(child: &mut Child, timeout: Duration) -> io::Result<ExitStatus> {
        let deadline = Instant::now() + timeout;
        loop {
            if let Some(status) = child.try_wait()? {
                return Ok(status);
            }
            if Instant::now() >= deadline {
                child.kill()?;
                child.wait()?;
                return Err(io::Error::new(io::ErrorKind::TimedOut, "test child hung"));
            }
            thread::sleep(Duration::from_millis(10));
        }
    }

    // Joining and pool destruction can block even while unwinding a failed
    // assertion. Bound the entire lifecycle test, including its cleanup.
    fn in_subprocess(name: &str) -> bool {
        if std::env::var("RACER_WORKERS_TEST_CHILD").as_deref() == Ok(name) {
            return false;
        }
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", &format!("workers::tests::{name}")])
            .env("RACER_WORKERS_TEST_CHILD", name)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let status = wait_with_deadline(&mut child, Duration::from_secs(15));
        let output = child.wait_with_output().unwrap();
        assert!(
            status.as_ref().is_ok_and(|status| status.success()),
            "{name}: {status:?}\n{}\n{}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        true
    }

    #[test]
    fn subprocess_deadline_kills_blocked_child() {
        const FLAG: &str = "RACER_WORKERS_TEST_HANG";
        if std::env::var_os(FLAG).is_some() {
            thread::sleep(Duration::from_secs(60));
            return;
        }
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "workers::tests::subprocess_deadline_kills_blocked_child",
            ])
            .env(FLAG, "1")
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        assert_eq!(
            wait_with_deadline(&mut child, Duration::from_millis(100))
                .unwrap_err()
                .kind(),
            io::ErrorKind::TimedOut
        );
        assert!(child.try_wait().unwrap().is_some());
    }

    fn config(shards: usize) -> Config {
        Config {
            shard_count: NonZeroUsize::new(shards).unwrap(),
        }
    }

    fn cpu(id: usize, node: usize, siblings: &[usize]) -> Cpu {
        Cpu {
            id: CpuId(id),
            node: NumaNodeId(node),
            siblings: siblings.iter().copied().map(CpuId).collect(),
        }
    }

    fn topology() -> Vec<Cpu> {
        vec![
            cpu(4097, 9, &[4096, 4097]), // Only the secondary sibling is allowed.
            cpu(12, 2, &[12, 13]),
            cpu(3, 2, &[2, 3]),
            cpu(13, 2, &[12, 13]),
            cpu(20, 2, &[20]),
        ]
    }

    #[test]
    fn compute_plan_excludes_smt_siblings_and_reserves_each_participating_node() {
        let cpus = vec![
            cpu(0, 0, &[0, 8]),
            cpu(8, 0, &[0, 8]),
            cpu(1, 0, &[1, 9]),
            cpu(9, 0, &[1, 9]),
            cpu(2, 1, &[2, 10]),
            cpu(10, 1, &[2, 10]),
            cpu(3, 1, &[3, 11]),
            cpu(11, 1, &[3, 11]),
        ];
        let plan = CpuPlan::build(
            Config {
                shard_count: NonZeroUsize::new(8).unwrap(),
            },
            WorkerCounts {
                compute_per_node: NonZeroUsize::new(1),
                ..Default::default()
            },
            cpus.clone(),
        )
        .unwrap();
        assert_eq!(plan.compute.cpus.len(), 2);
        assert_eq!(plan.io.len(), 2);
        assert_eq!(plan.io.iter().map(|p| p.shards.len()).sum::<usize>(), 8);
        for (compute, node) in &plan.compute.cpus {
            let physical = cpus.iter().find(|c| c.id == *compute).unwrap();
            assert!(plan.io.iter().all(|p| !physical.siblings.contains(&p.cpu)));
            assert!(plan.io.iter().any(|p| p.node == *node));
        }
        assert!(
            CpuPlan::build(
                Config::default(),
                WorkerCounts {
                    compute_per_node: NonZeroUsize::new(2),
                    ..Default::default()
                },
                cpus
            )
            .is_err()
        );
    }

    #[test]
    fn worker_counts_balance_asymmetric_nodes_and_allow_independent_overrides() {
        // Five and eight physical cores, including SMT and a secondary-only CPU.
        let cpus: Vec<_> = (0..13)
            .flat_map(|core| {
                let node = usize::from(core >= 5);
                let siblings = [core * 2, core * 2 + 1];
                siblings
                    .into_iter()
                    .filter(move |&id| id != 0)
                    .map(move |id| cpu(id, node, &siblings))
            })
            .collect();
        for (io_count, crypto_count, expected) in [
            (None, None, [(3, 2), (4, 4)]),
            (Some(2), None, [(2, 3), (2, 6)]),
            (None, Some(2), [(3, 2), (6, 2)]),
            (Some(2), Some(1), [(2, 1), (2, 1)]),
        ] {
            let plan = CpuPlan::build(
                config(32),
                WorkerCounts {
                    io_per_node: io_count.and_then(NonZeroUsize::new),
                    compute_per_node: crypto_count.and_then(NonZeroUsize::new),
                },
                cpus.clone(),
            )
            .unwrap();
            for (node, (io, crypto)) in expected.into_iter().enumerate() {
                assert_eq!(plan.io.iter().filter(|p| p.node.0 == node).count(), io);
                assert_eq!(
                    plan.compute
                        .cpus
                        .iter()
                        .filter(|(_, n)| n.0 == node)
                        .count(),
                    crypto
                );
            }
            let physical: BTreeSet<_> = plan
                .io
                .iter()
                .map(|p| p.cpu.0 / 2)
                .chain(plan.compute.cpus.iter().map(|(cpu, _)| cpu.0 / 2))
                .collect();
            assert_eq!(physical.len(), plan.io.len() + plan.compute.cpus.len());
            assert_eq!(plan.io.iter().map(|p| p.shards.len()).sum::<usize>(), 32);
        }
        let plan = CpuPlan::build(config(1), WorkerCounts::default(), cpus.clone()).unwrap();
        assert_eq!(plan.io.len(), 1);
        assert_eq!(plan.compute.cpus.len(), 4);
        assert!(plan.compute.cpus.iter().all(|(_, node)| node.0 == 0));
        let plan = CpuPlan::build(config(3), WorkerCounts::default(), cpus.clone()).unwrap();
        assert_eq!(plan.io.len(), 3);
        assert_eq!(plan.compute.cpus.len(), 10);
        for (shards, io, crypto) in [
            (32, Some(5), None),
            (32, None, Some(5)),
            (32, Some(4), Some(2)),
            (32, Some(usize::MAX), Some(1)),
            (3, Some(2), Some(1)),
        ] {
            assert!(
                CpuPlan::build(
                    config(shards),
                    WorkerCounts {
                        io_per_node: io.and_then(NonZeroUsize::new),
                        compute_per_node: crypto.and_then(NonZeroUsize::new),
                    },
                    cpus.clone()
                )
                .is_err()
            );
        }
        assert!(
            CpuPlan::build(config(32), WorkerCounts::default(), vec![cpu(0, 0, &[0])]).is_err()
        );
    }

    #[test]
    fn placement_balances_nodes_and_shards_on_allowed_physical_cores() {
        for shards in 1..=35 {
            let placements = place(config(shards), topology()).unwrap();
            assert_eq!(placements.len(), shards.min(4));
            let expected = [CpuId(3), CpuId(4097), CpuId(12), CpuId(20)];
            let mut owned = BTreeSet::new();
            for (index, placement) in placements.iter().enumerate() {
                assert_eq!(placement.worker_id(), WorkerId(index));
                assert_eq!(placement.cpu_id(), expected[index]);
                assert_eq!(
                    placement.numa_node_id(),
                    NumaNodeId(if index == 1 { 9 } else { 2 })
                );
                for &shard in placement.shard_ids() {
                    assert!(owned.insert(shard));
                    assert_eq!(shard.index() % placements.len(), index);
                }
            }
            assert_eq!(owned, (0..shards).map(ShardId::at).collect());
            let sizes: Vec<_> = placements.iter().map(|p| p.shards.len()).collect();
            assert!(sizes.iter().max().unwrap() - sizes.iter().min().unwrap() <= 1);
        }
        assert_eq!(Config::default().shard_count.get(), 32);
    }

    #[test]
    fn rejects_inconsistent_topology() {
        for cpus in [
            vec![],
            vec![cpu(1, 0, &[])],
            vec![cpu(1, 0, &[2])],
            vec![cpu(1, 0, &[1, 0])],
            vec![cpu(1, 0, &[1, 1])],
            vec![cpu(1, 0, &[1]), cpu(1, 0, &[1])],
            vec![cpu(1, 0, &[1, 2]), cpu(2, 1, &[1, 2])],
            vec![cpu(1, 0, &[1, 2]), cpu(2, 0, &[2, 3])],
        ] {
            assert!(place(config(4), cpus).is_err());
        }
    }

    #[test]
    fn parses_sparse_cpu_lists() {
        assert_eq!(
            parse_cpu_list("0-2,9,4097\n").unwrap(),
            vec![CpuId(0), CpuId(1), CpuId(2), CpuId(9), CpuId(4097)]
        );
        for text in ["", "2-1", "1,1", "0-2,2", "1-2-3", "-1", "x", "1,"] {
            assert!(parse_cpu_list(text).is_err(), "{text}");
        }
    }

    #[test]
    fn affinity_masks_grow_and_preserve_sparse_high_bits() {
        let bits = libc::c_ulong::BITS as usize;
        let expected = [0, bits - 1, bits, 4097];
        let mut sizes = Vec::new();
        let cpus = read_affinity(|mask| {
            sizes.push(mask.len());
            if mask.len() * bits <= 4097 {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            for cpu in expected {
                mask[cpu / bits] |= 1 << (cpu % bits);
            }
            Ok(())
        })
        .unwrap();
        assert_eq!(cpus, expected.map(CpuId));
        assert_eq!(sizes[0], 16);
        assert!(sizes.len() > 1);
        assert!(sizes.windows(2).all(|pair| pair[1] == pair[0] * 2));
        for cpu in expected {
            let mask = single_cpu_mask(CpuId(cpu));
            assert_eq!(mask.len(), cpu / bits + 1);
            assert_eq!(mask.iter().map(|word| word.count_ones()).sum::<u32>(), 1);
            assert_eq!(mask[cpu / bits], 1 << (cpu % bits));
        }
        let mut calls = 0;
        let error = read_affinity(|_| {
            calls += 1;
            Err(io::Error::from_raw_os_error(libc::EPERM))
        })
        .unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::EPERM));
        assert_eq!(calls, 1);
    }

    #[test]
    fn discovery_validates_numa_sysfs_fixtures() {
        struct Fixture(std::path::PathBuf);
        impl Drop for Fixture {
            fn drop(&mut self) {
                let _ = fs::remove_dir_all(&self.0);
            }
        }
        let root =
            std::env::temp_dir().join(format!("racer-workers-topology-{}", std::process::id()));
        // Fail rather than overwrite an existing directory belonging to another test.
        fs::create_dir(&root).unwrap();
        let fixture = Fixture(root);
        let root = &fixture.0;
        let cpu = root.join("cpu/cpu4097");
        fs::create_dir_all(cpu.join("topology")).unwrap();
        let siblings = cpu.join("topology/thread_siblings_list");
        fs::write(&siblings, "4096-4097\n").unwrap();
        let discover = || discover_at(root, vec![CpuId(4097)]);
        let topology = discover().unwrap();
        assert_eq!(topology[0].node, NumaNodeId(0));
        assert_eq!(topology[0].siblings, vec![CpuId(4096), CpuId(4097)]);

        let assert_error = |expected: &str| {
            let error = discover().unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidData);
            assert!(error.to_string().contains(expected), "{error}");
        };
        fs::write(root.join("node"), "not a directory").unwrap();
        assert_error("not a directory");
        fs::remove_file(root.join("node")).unwrap();
        fs::create_dir(root.join("node")).unwrap();
        assert_error("missing NUMA node");
        fs::create_dir(cpu.join("node9")).unwrap();
        assert_eq!(discover().unwrap()[0].node, NumaNodeId(9));
        fs::create_dir(cpu.join("node2")).unwrap();
        assert_error("multiple NUMA nodes");
        fs::remove_dir(cpu.join("node2")).unwrap();
        fs::create_dir(cpu.join("nodebad")).unwrap();
        assert_error("invalid NUMA node");
        fs::remove_dir(cpu.join("nodebad")).unwrap();
        fs::write(&siblings, "1-2-3").unwrap();
        assert_error("invalid CPU list");
        fs::remove_file(siblings).unwrap();
        assert_eq!(discover().unwrap_err().kind(), io::ErrorKind::NotFound);
        // Metadata errors other than NotFound must not trigger the UMA fallback.
        fs::remove_dir(root.join("node")).unwrap();
        std::os::unix::fs::symlink("node", root.join("node")).unwrap();
        assert_eq!(discover().unwrap_err().raw_os_error(), Some(libc::ELOOP));
    }

    #[derive(Default)]
    struct Signal {
        pending: Mutex<bool>,
        changed: Condvar,
    }

    impl Signal {
        fn wait(&self) {
            let mut pending = self.pending.lock().unwrap();
            while !*pending {
                pending = self.changed.wait(pending).unwrap();
            }
            *pending = false;
        }
    }

    impl Wake for Signal {
        fn wake(&self) {
            *self.pending.lock().unwrap() = true;
            self.changed.notify_one();
        }
    }

    #[derive(Clone, Copy)]
    enum Fault {
        None,
        WakeHandle,
        TurnError,
        TurnPanic,
        ShutdownError,
        ShutdownPanic,
        DropPanic,
        ErrorDisplayPanic,
        ErrorDropPanic,
        PayloadDropPanic,
    }

    #[derive(Debug)]
    struct PanickingError {
        display: bool,
    }

    impl std::fmt::Display for PanickingError {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            if self.display {
                panic!("error display failure");
            }
            f.write_str("error with panicking destructor")
        }
    }

    impl std::error::Error for PanickingError {}

    impl Drop for PanickingError {
        fn drop(&mut self) {
            // Also exercise a secondary payload whose destructor would panic.
            std::panic::panic_any(PanickingPayload);
        }
    }

    struct PanickingPayload;

    impl Drop for PanickingPayload {
        fn drop(&mut self) {
            std::panic::panic_any(PanickingPayload);
        }
    }

    #[derive(Debug, Eq, PartialEq)]
    enum Event {
        Turn(WorkerId),
        Shutdown(WorkerId),
        Drop(WorkerId),
    }

    struct TestDriver {
        id: WorkerId,
        signal: Arc<Signal>,
        events: Sender<Event>,
        owner: Rc<thread::ThreadId>, // Deliberately !Send and !Sync.
        fault: Fault,
    }

    impl TestDriver {
        fn new(id: WorkerId, signal: Arc<Signal>, events: Sender<Event>, fault: Fault) -> Self {
            Self {
                id,
                signal,
                events,
                owner: Rc::new(thread::current().id()),
                fault,
            }
        }

        fn record(&self, event: Event) {
            assert_eq!(*self.owner, thread::current().id());
            self.events.send(event).unwrap();
        }
    }

    impl Driver for TestDriver {
        type Wake = Signal;

        fn wake_handle(&self) -> Arc<Signal> {
            if matches!(self.fault, Fault::WakeHandle) {
                panic!("wake handle failure");
            }
            self.signal.clone()
        }

        fn turn(&mut self) -> io::Result<()> {
            self.record(Event::Turn(self.id));
            self.signal.wait();
            match self.fault {
                Fault::TurnError => Err(io::Error::other("turn failure")),
                Fault::TurnPanic => panic!("turn failure"),
                Fault::ErrorDisplayPanic | Fault::ErrorDropPanic => {
                    Err(io::Error::other(PanickingError {
                        display: matches!(self.fault, Fault::ErrorDisplayPanic),
                    }))
                }
                Fault::PayloadDropPanic => std::panic::panic_any(PanickingPayload),
                _ => Ok(()),
            }
        }

        fn shutdown(&mut self) -> io::Result<()> {
            self.record(Event::Shutdown(self.id));
            match self.fault {
                Fault::ShutdownError => Err(io::Error::other("shutdown failure")),
                Fault::ShutdownPanic => panic!("shutdown failure"),
                _ => Ok(()),
            }
        }
    }

    impl Drop for TestDriver {
        fn drop(&mut self) {
            self.record(Event::Drop(self.id));
            if matches!(self.fault, Fault::DropPanic) {
                panic!("drop failure");
            }
        }
    }

    fn receive(events: &Receiver<Event>) -> Event {
        events.recv_timeout(Duration::from_secs(5)).unwrap()
    }

    fn test_placements() -> Vec<Placement> {
        place(config(2), vec![cpu(0, 0, &[0]), cpu(1, 0, &[1])]).unwrap()
    }

    fn test_workers(fault: Fault) -> (Workers, Vec<Arc<Signal>>, Receiver<Event>) {
        let (sender, events) = mpsc::channel();
        let signals: Vec<Arc<Signal>> = vec![Arc::default(), Arc::default()];
        let factory_signals = signals.clone();
        let workers = Workers::start_placed(
            test_placements(),
            move |p| {
                Ok(TestDriver::new(
                    p.worker,
                    factory_signals[p.worker.0].clone(),
                    sender.clone(),
                    if p.worker.0 == 0 { fault } else { Fault::None },
                ))
            },
            |_| Ok(()),
        )
        .unwrap();
        let turns = [receive(&events), receive(&events)];
        assert!(turns.contains(&Event::Turn(WorkerId(0))));
        assert!(turns.contains(&Event::Turn(WorkerId(1))));
        (workers, signals, events)
    }

    fn assert_cleanup(events: Receiver<Event>, count: usize) {
        let mut shutdown = BTreeSet::new();
        let mut dropped = BTreeSet::new();
        for event in events.try_iter() {
            match event {
                Event::Shutdown(id) => {
                    assert!(shutdown.insert(id));
                }
                Event::Drop(id) => {
                    assert!(shutdown.contains(&id));
                    assert!(dropped.insert(id));
                }
                Event::Turn(_) => panic!("unexpected turn"),
            }
        }
        assert_eq!(shutdown.len(), count);
        assert_eq!(dropped, shutdown);
    }

    #[test]
    fn hostile_errors_and_payloads_still_stop_and_cleanup() {
        if in_subprocess("hostile_errors_and_payloads_still_stop_and_cleanup") {
            return;
        }
        for (fault, message) in [
            (Fault::ErrorDisplayPanic, "error formatter panicked"),
            (Fault::ErrorDropPanic, "error with panicking destructor"),
            (Fault::PayloadDropPanic, "panicked: non-string payload"),
        ] {
            let (sender, events) = mpsc::channel();
            let trigger = Arc::new(Signal::default());
            let signal = trigger.clone();
            let workers = Workers::start_placed(
                test_placements(),
                move |p| {
                    Ok(TestDriver::new(
                        p.worker,
                        if p.worker.0 == 1 {
                            signal.clone()
                        } else {
                            Arc::default()
                        },
                        sender.clone(),
                        if p.worker.0 == 1 { fault } else { Fault::None },
                    ))
                },
                |_| Ok(()),
            )
            .unwrap();
            for _ in 0..2 {
                assert!(matches!(receive(&events), Event::Turn(_)));
            }
            trigger.wake();
            // Fail the later join target: the healthy first thread must wake.
            let error = workers.join().unwrap_err().to_string();
            assert!(error.contains("worker 1:"), "{error}");
            assert!(error.contains(message), "{error}");
            assert_cleanup(events, 2);
        }
    }

    #[test]
    fn startup_stop_before_and_after_registration_cleans_up() {
        if in_subprocess("startup_stop_before_and_after_registration_cleans_up") {
            return;
        }
        for stop_first in [false, true] {
            let shared = Arc::new(Shared::default());
            let (sender, events) = mpsc::channel();
            let (release_tx, release_rx) = mpsc::channel();
            let worker_shared = shared.clone();
            let worker = thread::spawn(move || {
                release_rx.recv().unwrap();
                Worker {
                    id: WorkerId(0),
                    driver: TestDriver::new(WorkerId(0), Arc::default(), sender, Fault::None),
                    shared: worker_shared,
                    _thread_local: PhantomData,
                }
                .run();
            });
            if stop_first {
                shared.fail(WorkerId(1), io::Error::other("startup failure"));
                release_tx.send(()).unwrap();
            } else {
                release_tx.send(()).unwrap();
                let mut state = shared.state.lock().unwrap();
                while state.wakes.is_empty() {
                    state = shared.changed.wait(state).unwrap();
                }
                // Acquiring this mutex after registration guarantees ready() has
                // entered its startup wait; release was never granted.
                assert!(!state.released);
                drop(state);
                shared.fail(WorkerId(1), io::Error::other("startup failure"));
            }
            worker.join().unwrap();
            assert_cleanup(events, 1);
        }
    }

    #[test]
    fn later_spawn_failure_rolls_back_started_threads() {
        if in_subprocess("later_spawn_failure_rolls_back_started_threads") {
            return;
        }
        let (sender, events) = mpsc::channel();
        let (entered_tx, entered_rx) = mpsc::channel();
        let mut spawned = 0;
        let result = Workers::start_with_spawner(
            test_placements(),
            move |p| {
                entered_tx.send(()).unwrap();
                Ok(TestDriver::new(
                    p.worker,
                    Arc::default(),
                    sender.clone(),
                    Fault::None,
                ))
            },
            |_| Ok(()),
            |builder, task| {
                spawned += 1;
                if spawned == 2 {
                    entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                    Err(io::Error::from_raw_os_error(libc::EAGAIN))
                } else {
                    builder.spawn(task)
                }
            },
        );
        let error = result.err().expect("spawn must fail");
        assert_eq!(error.kind(), io::ErrorKind::WouldBlock);
        assert!(error.to_string().contains("worker 1:"));
        assert_cleanup(events, 1);
    }

    #[test]
    fn startup_waits_for_wake_handle_before_releasing_turns() {
        if in_subprocess("startup_waits_for_wake_handle_before_releasing_turns") {
            return;
        }
        struct GatedDriver {
            inner: TestDriver,
            entered: Sender<()>,
            release: Option<Receiver<()>>,
        }
        impl Driver for GatedDriver {
            type Wake = Signal;
            fn wake_handle(&self) -> Arc<Signal> {
                self.entered.send(()).unwrap();
                if let Some(release) = &self.release {
                    release.recv().unwrap();
                }
                self.inner.wake_handle()
            }
            fn turn(&mut self) -> io::Result<()> {
                self.inner.turn()
            }
            fn shutdown(&mut self) -> io::Result<()> {
                self.inner.shutdown()
            }
        }
        let (entered_tx, entered_rx) = mpsc::channel();
        let (release_tx, release_rx) = mpsc::channel();
        let release = Mutex::new(Some(release_rx));
        let (sender, events) = mpsc::channel();
        let (started_tx, started_rx) = mpsc::channel();
        let starter = thread::spawn(move || {
            let workers = Workers::start_placed(
                test_placements(),
                move |p| {
                    Ok(GatedDriver {
                        inner: TestDriver::new(
                            p.worker,
                            Arc::default(),
                            sender.clone(),
                            Fault::None,
                        ),
                        entered: entered_tx.clone(),
                        release: if p.worker.0 == 1 {
                            release.lock().unwrap().take()
                        } else {
                            None
                        },
                    })
                },
                |_| Ok(()),
            )
            .unwrap();
            started_tx.send(workers).unwrap();
        });
        for _ in 0..2 {
            entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        }
        assert!(matches!(
            started_rx.try_recv(),
            Err(mpsc::TryRecvError::Empty)
        ));
        assert!(matches!(events.try_recv(), Err(mpsc::TryRecvError::Empty)));
        release_tx.send(()).unwrap();
        let workers = started_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        starter.join().unwrap();
        for _ in 0..2 {
            assert!(matches!(receive(&events), Event::Turn(_)));
        }
        drop(workers);
        assert_cleanup(events, 2);
    }

    #[test]
    fn idle_workers_stop_and_all_join() {
        if in_subprocess("idle_workers_stop_and_all_join") {
            return;
        }
        let (workers, signals, events) = test_workers(Fault::None);
        let stop = workers.stop_handle();
        stop.request_stop();
        stop.clone().request_stop();
        workers.join().unwrap();
        stop.request_stop();
        for signal in signals {
            signal.wake();
        } // Valid after driver destruction.
        assert_cleanup(events, 2);
    }

    #[test]
    fn dropping_pool_stops_and_joins() {
        if in_subprocess("dropping_pool_stops_and_joins") {
            return;
        }
        let (workers, _, events) = test_workers(Fault::None);
        drop(workers);
        assert_cleanup(events, 2);
    }

    #[test]
    fn wake_before_wait_and_late_registration_retain_stop() {
        if in_subprocess("wake_before_wait_and_late_registration_retain_stop") {
            return;
        }
        let shared = Shared::default();
        shared.request_stop();
        let signal = Arc::new(Signal::default());
        shared.ready(signal.clone());
        assert!(*signal.pending.lock().unwrap());
        signal.wait();
        assert!(!*signal.pending.lock().unwrap());
        signal.wake();
        signal.wake();
        signal.wait();
        assert!(!*signal.pending.lock().unwrap());
    }

    #[test]
    fn turn_failures_stop_peers_and_cleanup_before_join_returns() {
        if in_subprocess("turn_failures_stop_peers_and_cleanup_before_join_returns") {
            return;
        }
        for fault in [Fault::TurnError, Fault::TurnPanic] {
            let (workers, signals, events) = test_workers(fault);
            signals[0].wake();
            let error = workers.join().unwrap_err();
            assert!(error.to_string().contains("worker 0:"));
            assert!(error.to_string().contains("turn failure"));
            assert_cleanup(events, 2);
        }
    }

    #[test]
    fn shutdown_and_destructor_failures_still_join_peers() {
        if in_subprocess("shutdown_and_destructor_failures_still_join_peers") {
            return;
        }
        for fault in [Fault::ShutdownError, Fault::ShutdownPanic, Fault::DropPanic] {
            let (workers, _, events) = test_workers(fault);
            workers.stop_handle().request_stop();
            assert!(
                workers
                    .join()
                    .unwrap_err()
                    .to_string()
                    .contains("worker 0:")
            );
            assert_cleanup(events, 2);
        }
    }

    #[test]
    fn startup_failures_rollback_without_turning() {
        if in_subprocess("startup_failures_rollback_without_turning") {
            return;
        }
        // Fail pinning, factory, or wake registration. The successful factory can
        // register before or after stop; both must shut down without any turns.
        for stage in 0..4 {
            let (sender, events) = mpsc::channel();
            let (constructed, ready) = mpsc::channel();
            let ready = Mutex::new(ready);
            let result = Workers::start_placed(
                test_placements(),
                move |p| {
                    if p.worker.0 == 1 {
                        match stage {
                            1 => return Err(io::Error::other("factory failure")),
                            2 => panic!("factory failure"),
                            _ => {}
                        }
                    }
                    let driver = TestDriver::new(
                        p.worker,
                        Arc::default(),
                        sender.clone(),
                        if stage == 3 && p.worker.0 == 1 {
                            Fault::WakeHandle
                        } else {
                            Fault::None
                        },
                    );
                    if p.worker.0 == 0 {
                        constructed.send(()).unwrap();
                    }
                    Ok(driver)
                },
                move |cpu| {
                    // Startup cancellation can legitimately skip a factory that
                    // has not started. Ensure this test actually owns the healthy
                    // driver whose shutdown/drop it asserts, before injecting failure.
                    if cpu.0 == 1 {
                        ready
                            .lock()
                            .unwrap()
                            .recv_timeout(Duration::from_secs(5))
                            .unwrap();
                    }
                    if stage == 0 && cpu.0 == 1 {
                        Err(io::Error::other("pin failure"))
                    } else {
                        Ok(())
                    }
                },
            );
            let error = result.err().expect("startup must fail");
            assert!(error.to_string().contains("worker 1:"));
            assert_cleanup(events, if stage == 3 { 2 } else { 1 });
        }
    }

    #[test]
    fn startup_waits_for_every_factory_before_releasing_turns() {
        if in_subprocess("startup_waits_for_every_factory_before_releasing_turns") {
            return;
        }
        let (entered_tx, entered_rx) = mpsc::channel();
        let (release_tx, release_rx) = mpsc::channel();
        let release_rx = Mutex::new(release_rx);
        let (sender, events) = mpsc::channel();
        let (started_tx, started_rx) = mpsc::channel();
        let starter = thread::spawn(move || {
            let workers = Workers::start_placed(
                test_placements(),
                move |p| {
                    if p.worker.0 == 1 {
                        entered_tx.send(()).unwrap();
                        release_rx.lock().unwrap().recv().unwrap();
                    }
                    Ok(TestDriver::new(
                        p.worker,
                        Arc::default(),
                        sender.clone(),
                        Fault::None,
                    ))
                },
                |_| Ok(()),
            )
            .unwrap();
            started_tx.send(workers).unwrap();
        });
        entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(matches!(
            started_rx.try_recv(),
            Err(mpsc::TryRecvError::Empty)
        ));
        assert!(matches!(events.try_recv(), Err(mpsc::TryRecvError::Empty)));
        release_tx.send(()).unwrap();
        let workers = started_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        starter.join().unwrap();
        for _ in 0..2 {
            assert!(matches!(receive(&events), Event::Turn(_)));
        }
        drop(workers);
        assert_cleanup(events, 2);
    }

    #[test]
    fn panic_wakes_peers_before_local_shutdown_waits() {
        if in_subprocess("panic_wakes_peers_before_local_shutdown_waits") {
            return;
        }
        struct CoordinatedDriver {
            inner: TestDriver,
            peer_stopped: Option<Receiver<()>>,
            stopped: Sender<()>,
        }

        impl Driver for CoordinatedDriver {
            type Wake = Signal;

            fn wake_handle(&self) -> Arc<Signal> {
                self.inner.wake_handle()
            }

            fn turn(&mut self) -> io::Result<()> {
                self.inner.turn()
            }

            fn shutdown(&mut self) -> io::Result<()> {
                if let Some(peer) = &self.peer_stopped {
                    peer.recv_timeout(Duration::from_secs(5))
                        .expect("peer was not stopped before local cleanup");
                } else {
                    self.stopped.send(()).unwrap();
                }
                self.inner.shutdown()
            }
        }

        let (stopped, peer_stopped) = mpsc::channel();
        let peer_stopped = Mutex::new(Some(peer_stopped));
        let (sender, events) = mpsc::channel();
        let signal = Arc::new(Signal::default());
        let trigger = signal.clone();
        let workers = Workers::start_placed(
            test_placements(),
            move |p| {
                let failing = p.worker.0 == 0;
                Ok(CoordinatedDriver {
                    inner: TestDriver::new(
                        p.worker,
                        if failing {
                            signal.clone()
                        } else {
                            Arc::default()
                        },
                        sender.clone(),
                        if failing {
                            Fault::TurnPanic
                        } else {
                            Fault::None
                        },
                    ),
                    peer_stopped: if failing {
                        peer_stopped.lock().unwrap().take()
                    } else {
                        None
                    },
                    stopped: stopped.clone(),
                })
            },
            |_| Ok(()),
        )
        .unwrap();
        for _ in 0..2 {
            assert!(matches!(receive(&events), Event::Turn(_)));
        }
        trigger.wake();
        assert!(
            workers
                .join()
                .unwrap_err()
                .to_string()
                .contains("turn failure")
        );
        assert_cleanup(events, 2);
    }

    #[test]
    fn real_workers_initialize_after_pinning_with_non_send_drivers() {
        if in_subprocess("real_workers_initialize_after_pinning_with_non_send_drivers") {
            return;
        }
        let parent_affinity = allowed_cpus().unwrap();
        let (sender, events) = mpsc::channel();
        let workers = Workers::start(config(2), move |p| {
            assert_eq!(allowed_cpus()?, vec![p.cpu_id()]);
            Ok(TestDriver::new(
                p.worker_id(),
                Arc::default(),
                sender.clone(),
                Fault::None,
            ))
        })
        .unwrap();
        let count = workers.placements().len();
        for _ in 0..count {
            assert!(matches!(receive(&events), Event::Turn(_)));
        }
        assert_eq!(allowed_cpus().unwrap(), parent_affinity);
        workers.stop_handle().request_stop();
        workers.join().unwrap();
        assert_cleanup(events, count);
    }
}
