// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::{
    allocator, cache, http_client as client, runtime::tests::dst::Cluster, simulation::World,
};
use std::collections::BTreeSet;
use std::num::NonZeroUsize;

#[test]
fn full_slab_finite_overlap_reclaims_after_inflight_writes_drain() {
    for latency in [14, 25, 35] {
        let world = World::new(425);
        let _scope = world.enter();
        world.enable_scheduler();
        let pool = buffers::io_test_pool(8);
        let mut ring = Ring::http_test_ring(pool.clone(), Config::default()).unwrap();
        let size = 10 * 1024 * 1024 * 1024;
        let disk = crate::simulation::Disk::new(size);
        let mut slab = allocator::Slab::simulated(disk.clone(), size, 1, true).unwrap();
        let context = crate::sharding::WorkerContext::test(1);
        let mut a = allocator::Allocator::open(
            &context,
            slab.take_shard(crate::workers::ShardId::at(0)).unwrap(),
            allocator::Config::default(),
        )
        .unwrap();
        let key = |n: u64| {
            let mut key = [0; 32];
            key[..8].copy_from_slice(&n.to_le_bytes());
            key
        };
        let tick = |a: &mut allocator::Allocator, ring: &mut Ring| {
            world.service_tick();
            ring.progress().unwrap();
            a.poll(ring, 1).unwrap();
        };
        // Every payload, even a clipped EOF page, occupies a 4 MiB extent.
        // Seed cheaply with clipped pages; the measured burst is full-size.
        let mut seed = pool.private_fill().unwrap();
        seed.as_mut_slice()[0] = 17;
        let seed = seed.publish(1).unwrap();
        let capacity = (size - size / 8) / buffers::BUFFER_SIZE as u64;
        assert_eq!(capacity, 2240);
        for n in 0..capacity - 4 {
            a.insert_payload(key(n), seed.clone(), None).unwrap();
            if n % 8 == 7 || n == capacity - 5 {
                for _ in 0..2000 {
                    tick(&mut a, &mut ring);
                    if a.is_idle() {
                        break;
                    }
                }
                assert!(a.is_idle());
            }
        }
        drop(seed);
        pool.assert_recovered();
        assert_eq!(a.len(), capacity as usize - 4);
        let generation = a.generation();
        let before = world.counts();
        let start = world.tick();
        let mut pending: Vec<_> = (0..8)
            .map(|n| {
                let mut fill = pool.private_fill().unwrap();
                fill.as_mut_slice().fill(n as u8 + 1);
                Some(fill.publish(buffers::BUFFER_SIZE).unwrap())
            })
            .collect();
        let mut admitted = [None; 8];
        let mut first_rejected = [None; 8];
        let mut seen = BTreeSet::new();
        let mut delayed = 0;
        let mut samples = Vec::new();
        let mut at_pressure_deadline = None;
        // Allocator-only probe: observe eventual reclaim beyond the cache's
        // unchanged 320 ms pressure allowance. This is not an HTTP success
        // assertion and must not be used to justify a larger request budget.
        for elapsed in 0..2000 {
            if elapsed % 10 == 0 {
                for n in 0..8 {
                    let Some(buffer) = pending[n].take() else {
                        continue;
                    };
                    match a.insert_payload(key(capacity + n as u64), buffer, None) {
                        Ok(()) => {
                            if n >= 4 {
                                assert!(
                                    a.generation() >= generation + 2,
                                    "both durable roots must rotate before reuse"
                                );
                            }
                            admitted[n] = Some(world.tick() - start);
                        }
                        Err(rejected) => {
                            assert_eq!(rejected.error.kind(), io::ErrorKind::WouldBlock);
                            first_rejected[n].get_or_insert(world.tick() - start);
                            pending[n] = Some(rejected.resource);
                        }
                    }
                }
                if elapsed == 0 {
                    assert_eq!(admitted.iter().filter(|t| t.is_some()).count(), 4);
                    assert_eq!(first_rejected.iter().filter(|t| t.is_some()).count(), 4);
                }
            }
            if elapsed % 50 == 0 {
                samples.push((elapsed, a.pressure_snapshot()));
            }
            if elapsed == 320 {
                at_pressure_deadline = Some((
                    admitted.iter().filter(|value| value.is_some()).count(),
                    a.pressure_snapshot(),
                ));
            }
            delayed += delay_storage(&mut ring, latency, &mut seen);
            tick(&mut a, &mut ring);
            assert!(!a.is_failed());
            if admitted.iter().all(Option::is_some) && a.is_idle() {
                break;
            }
        }
        eprintln!(
            "full-slab latency={latency}ms admissions={admitted:?} elapsed={} generations={} delayed_ops={delayed} syncs={} at_320ms={at_pressure_deadline:?} snapshots={samples:?}",
            world.tick() - start,
            a.generation() - generation,
            world.counts()[3] - before[3]
        );
        assert!(
            admitted.iter().all(Option::is_some),
            "finite in-flight writes must not indefinitely prevent reclaim"
        );
        assert!(
            a.is_idle(),
            "checkpoint rotations must finish without global HTTP idleness"
        );
        assert!(a.generation() >= generation + 2);
        assert!(delayed >= 16);
        for n in 0..8 {
            let lease = a.lookup(&key(capacity + n as u64), 0).unwrap();
            let file = lease.ready().unwrap();
            let mut bytes = vec![0; buffers::BUFFER_SIZE];
            disk.read_exact_at(&mut bytes, file.offset()).unwrap();
            assert!(bytes.iter().all(|b| *b == n as u8 + 1));
        }
        drop((pending, a, slab));
        ring.shutdown().unwrap();
        pool.assert_recovered();
        drop((ring, pool));
        world.assert_clean();
    }
}

// Owning Ring test scope grants private simulator access without a production
// hook. Delay each storage submission once; network operations use a separately
// specified 64 KiB / 1 ms profile. Storage CQ delivery remains seeded 1..3 ms,
// giving latency-1 .. latency+1 ms before any scheduler delay.
fn delay_storage(ring: &mut Ring, latency: u64, seen: &mut BTreeSet<u64>) -> usize {
    let RawRing::Sim(sim) = &mut ring.core.as_mut().unwrap().raw else {
        panic!("simulation required")
    };
    let mut delayed = 0;
    for (sqe, due) in sim.staged.iter_mut().chain(&mut sim.pending) {
        if matches!(sqe.opcode, 3 | 4 | 5 | 17 | 22 | 23) && seen.insert(sqe.user_data) {
            *due = sim.world.tick() + latency - 2;
            delayed += 1;
        }
    }
    delayed
}

#[test]
fn four_full_pages_plus_peer_work_progress_with_moderate_storage_latency() {
    for latency in [14, 25, 35] {
        let world = World::new(424);
        let _scope = world.enter();
        let mut cluster = Cluster::with_pool(world.clone(), false, false, Some(2), 8);
        world.link_profile(65536, Some(1));
        for node in [2, 5] {
            let _node = world.scoped_node(Some(node));
            let size = 10 * 1024 * 1024 * 1024;
            let mut slab =
                allocator::Slab::simulated(crate::simulation::Disk::new(size), size, 1, true)
                    .unwrap();
            let mut cache =
                cache::tests::cache_from_slab(&mut slab, 1, allocator::Config::default());
            cache.set_metrics(cluster.ring(node).metrics().clone());
            *cluster.cache(node) = cache;
        }
        let mut targets = Vec::new();
        for (source, owner) in [(2, 5), (5, 2)] {
            for page in 0..4 {
                let target = cluster.target(owner, &format!("wide-io-{source}-{page}"));
                crate::runtime::tests::dst::assert_route(
                    &cluster,
                    &target,
                    0,
                    &[source, owner as usize],
                );
                let mut head = crate::runtime::tests::dst::cold_head(&cluster, source, &target);
                assert_eq!(
                    crate::runtime::tests::dst::finish_head(&mut cluster, source, &mut head),
                    200
                );
                targets.push((source, target));
            }
        }
        cluster.turns(100);
        let mut origins: Vec<_> = [2, 5]
            .into_iter()
            .map(|node| {
                let server = cluster.separate_origin(node);
                let _node = world.scoped_node(Some(node));
                let pool = buffers::test_pool(
                    buffers::Config::new(NonZeroUsize::new(8).unwrap()),
                    crate::workers::NumaNodeId(100 + node),
                    true,
                );
                (
                    node,
                    Ring::http_test_ring(pool, Config::default()).unwrap(),
                    server,
                )
            })
            .collect();
        let mut clients: Vec<_> = [2, 5]
            .into_iter()
            .map(|node| {
                let _node = world.scoped_node(None);
                let pool = buffers::test_pool(
                    buffers::Config::new(NonZeroUsize::new(4).unwrap()),
                    crate::workers::NumaNodeId(200 + node),
                    true,
                );
                (node, Ring::http_test_ring(pool, Config::default()).unwrap())
            })
            .collect();
        let start = world.tick();
        let before = world.counts();
        let hits = cluster.hits.borrow().len();
        let mut requests: Vec<_> = targets
            .iter()
            .map(|(node, target)| {
                let ring = &clients.iter().find(|(n, _)| n == node).unwrap().1;
                client::Connection::new(
                    format!("127.0.0.1:{}", 10000 + node).parse().unwrap(),
                    "localhost",
                )
                .unwrap()
                .get(
                    client::Request::new(target, &[("Range", "bytes=0-4194303")]).unwrap(),
                    ring.pool().private_fill().unwrap(),
                    world.now() + Duration::from_secs(15),
                )
                .unwrap()
            })
            .collect();
        let mut done = vec![false; requests.len()];
        let mut seen = [BTreeSet::new(), BTreeSet::new()];
        let mut delayed = 0;
        let mut peak_loading = [0; 2];
        let expected: Vec<_> = (0..buffers::BUFFER_SIZE).map(|i| (i % 251) as u8).collect();
        for _ in 0..5000 {
            // Every newly queued storage SQE is delayed before the next driver
            // turn can submit/execute it. The production driver and cache poll
            // order are otherwise unchanged.
            for (index, node) in [2, 5].into_iter().enumerate() {
                delayed += delay_storage(cluster.ring(node), latency, &mut seen[index]);
                peak_loading[index] =
                    peak_loading[index].max(cluster.ring(node).pool().invariant_snapshot().loading);
            }
            cluster.turns(1);
            for (node, ring, server) in &mut origins {
                let _node = world.scoped_node(Some(*node));
                ring.progress().unwrap();
                server.poll(ring, 64).unwrap();
            }
            let _client = world.scoped_node(None);
            for (_, ring) in &mut clients {
                ring.progress().unwrap();
            }
            for (index, ((node, _), request)) in targets.iter().zip(&mut requests).enumerate() {
                if done[index] {
                    continue;
                }
                let ring = &mut clients.iter_mut().find(|(n, _)| n == node).unwrap().1;
                if let Progress::Ready(mut response) = request.poll(ring, 32).unwrap() {
                    assert_eq!(
                        response.status(),
                        206,
                        "latency={latency} index={index} tick={}",
                        world.tick()
                    );
                    assert_eq!(
                        response.body(),
                        expected,
                        "full exact page, not a clipped response"
                    );
                    done[index] = true;
                }
            }
            if done.iter().all(|value| *value) {
                break;
            }
        }
        assert!(done.iter().all(|value| *value), "finite burst stalled");
        assert!(
            delayed >= 32,
            "must delay real punch/write and checkpoint operations"
        );
        assert!(peak_loading.iter().all(|count| *count >= 4));
        assert_eq!(
            cluster.hits.borrow().len() - hits,
            8,
            "one origin fetch per page"
        );
        for node in [2, 5] {
            let cache = cluster.cache(node);
            let registry = crate::metrics::Registry::new(
                1,
                std::sync::Arc::new(crate::control::Updates::default()),
            );
            registry.register(0, cache.metrics());
            cache.metrics().publish();
            for line in registry.render().lines().filter(|line| {
                line.starts_with("racer_dataplane_cache_resource_exhaustions_total{")
            }) {
                assert!(
                    line.ends_with(" 0"),
                    "latency={latency} node={node}: {line}"
                );
            }
        }
        let after = world.counts();
        assert_eq!(
            after[5] - before[5],
            16,
            "each node caches its four local and four peer pages"
        );
        assert_eq!(after[17] - before[17], 16);
        assert!(
            !world.events().iter().any(|event| event.tick >= start
                && matches!(
                    event.kind,
                    "candidate" | "http-timeout" | "resource-exhausted"
                )),
            "no retry or timeout may mask the success oracle"
        );
        eprintln!(
            "full-page finite burst latency={latency}ms elapsed={}ms delayed_storage_ops={delayed} peak_loading={peak_loading:?} writes={} punches={} syncs={}",
            world.tick() - start,
            after[5] - before[5],
            after[17] - before[17],
            after[3] - before[3]
        );
        drop(requests);
        for (_, mut ring) in clients {
            ring.shutdown().unwrap();
            ring.pool().assert_recovered();
        }
        for (node, mut ring, mut server) in origins {
            let _node = world.scoped_node(Some(node));
            server.shutdown(&mut ring).unwrap();
            ring.shutdown().unwrap();
            ring.pool().assert_recovered();
        }
        crate::runtime::tests::dst::clean_repro(cluster, &world);
    }
}
