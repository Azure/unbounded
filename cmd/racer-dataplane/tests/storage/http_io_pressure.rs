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
        assert!((capacity as usize - 64..=capacity as usize - 32).contains(&a.len()));
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
        // Ordinary filling now leaves durable headroom. This finite burst
        // crosses the low watermark and must replenish without admission retries.
        for elapsed in 0..2000 {
            if elapsed % 10 == 0 {
                for n in 0..8 {
                    let Some(buffer) = pending[n].take() else {
                        continue;
                    };
                    match a.insert_payload(key(capacity + n as u64), buffer, None) {
                        Ok(()) => {
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
                    assert_eq!(admitted.iter().filter(|t| t.is_some()).count(), 8);
                    assert_eq!(first_rejected.iter().filter(|t| t.is_some()).count(), 0);
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
    run_full_page_burst(&[14, 25, 35], false, false);
}

// Finite success targets after ordinary fill has prepared durable headroom.
// They do not promise admission for arbitrary bursts or pinned capacity.
#[test]
fn full_slab_http_burst_25ms_requires_prepared_reclaim_headroom() {
    run_full_page_burst(&[25], true, false);
}

#[test]
fn full_slab_http_burst_35ms_requires_prepared_reclaim_headroom() {
    run_full_page_burst(&[35], true, false);
}

#[test]
fn full_slab_http_burst_35ms_succeeds_with_already_durable_headroom() {
    run_full_page_burst(&[35], true, true);
}

fn run_full_page_burst(latencies: &[u64], full: bool, prepared_headroom: bool) {
    for &latency in latencies {
        let world = World::new(424);
        let _scope = world.enter();
        let mut cluster = Cluster::with_pool(world.clone(), false, false, Some(2), 8);
        world.link_profile(65536, Some(1));
        for node in [2, 5] {
            let _node = world.scoped_node(Some(node));
            let size = 10 * 1024 * 1024 * 1024;
            let disk = crate::simulation::Disk::new(size);
            let mut slab = allocator::Slab::simulated(disk.clone(), size, 1, true).unwrap();
            if full {
                let context = crate::sharding::WorkerContext::test(1);
                let mut allocator = allocator::Allocator::open(
                    &context,
                    slab.take_shard(crate::workers::ShardId::at(0)).unwrap(),
                    allocator::Config::default(),
                )
                .unwrap();
                let mut seed = cluster.ring(node).pool().private_fill().unwrap();
                seed.as_mut_slice()[0] = 17;
                let seed = seed.publish(1).unwrap();
                let mut fill_storage = BTreeSet::new();
                for n in 0..2240u64 {
                    let mut key = [0; 32];
                    key[..8].copy_from_slice(&n.to_le_bytes());
                    allocator.insert_payload(key, seed.clone(), None).unwrap();
                    if n % 8 == 7 {
                        for _ in 0..2000 {
                            // Cross the production watermark with the same
                            // storage delay as the later HTTP burst, not an
                            // instantaneous/manual pre-reclaim preparation.
                            delay_storage(cluster.ring(node), latency, &mut fill_storage);
                            world.service_tick();
                            cluster.ring(node).progress().unwrap();
                            allocator.poll(cluster.ring(node), 1).unwrap();
                            if allocator.is_idle() {
                                break;
                            }
                        }
                        assert!(allocator.is_idle());
                    }
                }
                assert!(
                    (2176..=2208).contains(&allocator.len()),
                    "ordinary fill must replenish at the low watermark: {}",
                    allocator.pressure_snapshot()
                );
                eprintln!(
                    "full slab prepared node={node} {}",
                    allocator.pressure_snapshot()
                );
                if prepared_headroom {
                    // Controlled counterfactual, not a production reclaim policy:
                    // all seed values are durable and unpinned. Remove 64 via
                    // normal eviction and complete TWO real checkpoints before
                    // offering the identical measured HTTP burst.
                    let generation = allocator.generation();
                    let mut seen = BTreeSet::new();
                    for _ in 0..64 {
                        assert!(allocator.evict(allocator::Kind::Payload, 0).is_some());
                    }
                    for rotation in 1..=2 {
                        if rotation == 2 {
                            // A normal metadata mutation schedules the second
                            // root without private allocator state manipulation.
                            allocator
                                .insert_metadata(
                                    [255; 32],
                                    crate::metadata::Metadata {
                                        checksum: crate::metadata::Checksum([7; 32]),
                                        len: 1,
                                        expires: u64::MAX,
                                    },
                                    0,
                                )
                                .unwrap();
                        }
                        for _ in 0..2000 {
                            delay_storage(cluster.ring(node), latency, &mut seen);
                            world.service_tick();
                            cluster.ring(node).progress().unwrap();
                            allocator.poll(cluster.ring(node), 1).unwrap();
                            if allocator.is_idle() {
                                break;
                            }
                        }
                        assert!(allocator.is_idle());
                        assert_eq!(allocator.generation(), generation + rotation);
                    }
                    eprintln!(
                        "durable headroom node={node} {}",
                        allocator.pressure_snapshot()
                    );
                }
                drop((seed, allocator, slab));
                slab = allocator::Slab::simulated(disk, size, 1, false).unwrap();
            }
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
                    if response.status() != 206 {
                        eprintln!(
                            "full={full} latency={latency} node={node} index={index} status={} elapsed={}ms",
                            response.status(),
                            world.tick() - start
                        );
                        for event in world.events().iter().filter(|event| {
                            event.tick >= start && event.kind == "resource-exhausted"
                        }) {
                            eprintln!(
                                "t={} node={:?} target={} {}",
                                event.tick, event.node, event.target, event.detail
                            );
                        }
                    }
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
