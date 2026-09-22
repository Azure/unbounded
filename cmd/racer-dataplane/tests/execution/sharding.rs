// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod tests {
    use super::*;
    use crate::{
        allocator::Slab,
        buffers::{self, Key},
        cache::{Cache, Namespace},
    };
    use std::sync::atomic::{AtomicUsize, Ordering};
    fn slab(count: usize) -> Slab {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let path = std::env::temp_dir().join(format!(
            "racer-sharding-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let slab = Slab::create(&path, count as u64 * 32 * 1024 * 1024, count).unwrap();
        std::fs::remove_file(path).unwrap();
        slab
    }
    #[test]
    fn assignments_are_once_only_and_bound_to_plan_and_worker() {
        let plan: Arc<[Placement]> = placements(
            vec![(CpuId(0), NumaNodeId(0)), (CpuId(1), NumaNodeId(0))],
            5,
        )
        .unwrap()
        .into();
        let a = WorkerContext::pinned(plan.clone(), 0);
        let b = WorkerContext::pinned(plan, 1);
        let foreign = WorkerContext::test(5);
        let assignments = a.take_assignments().unwrap();
        assert!(a.take_assignments().is_err());
        assert!(a.clone().take_assignments().is_err());
        for assignment in &assignments {
            assert!(a.check(assignment).is_ok());
            assert!(b.check(assignment).is_err());
            assert!(foreign.check(assignment).is_err());
        }
        assert_eq!(
            a.shard_ids().iter().map(|s| s.index()).collect::<Vec<_>>(),
            [0, 2, 4]
        );
        assert_eq!(a.shard_id(4).unwrap().index(), 4);
        assert!(a.shard_id(5).is_err());
    }
    #[test]
    fn activation_checks_geometry_pool_and_assignment() {
        let c = WorkerContext::test(2);
        let pool = buffers::io_test_pool(2);
        let mut slab = slab(2);
        let mut assignments = c.take_assignments().unwrap().into_iter();
        let a = assignments.next().unwrap();
        assert!(
            ShardState::activate(
                &c,
                a,
                slab.take_shard(ShardId::at(1)).unwrap(),
                &pool,
                allocator::Config::default()
            )
            .is_err()
        );
        let other = WorkerContext::test(2);
        let a = other.take_assignments().unwrap().remove(0);
        assert!(
            ShardState::activate(
                &c,
                a,
                slab.take_shard(ShardId::at(0)).unwrap(),
                &pool,
                allocator::Config::default()
            )
            .is_err()
        );
        c.bind_pool(&pool).unwrap();
        assert!(c.bind_pool(&buffers::io_test_pool(1)).is_err());
        assert!(c.clone().bind_pool(&buffers::io_test_pool(1)).is_err());
        let mut wrong_geometry = super::tests::slab(1);
        let a = WorkerContext::test(2);
        let assignment = a.take_assignments().unwrap().remove(0);
        assert!(
            ShardState::activate(
                &a,
                assignment,
                wrong_geometry.take_shard(ShardId::at(0)).unwrap(),
                &pool,
                allocator::Config::default()
            )
            .is_err()
        );
    }
    #[test]
    fn cache_validates_collection_and_views_share_pool_capacity() {
        let c = WorkerContext::test(2);
        let pool = buffers::io_test_pool(2);
        let mut slab = slab(2);
        let states: Vec<_> = c
            .take_assignments()
            .unwrap()
            .into_iter()
            .map(|a| {
                let storage = slab.take_shard(a.id()).unwrap();
                ShardState::activate(&c, a, storage, &pool, allocator::Config::default()).unwrap()
            })
            .collect();
        let key = Key::new([7; 32]);
        let first = states[0].buffers.as_ref().unwrap().pool();
        let second = states[1].buffers.as_ref().unwrap().pool();
        assert!(first.same_pool(second));
        let mut fill = first.stage(key).unwrap();
        let independent = second.stage(key).unwrap();
        assert_ne!(fill.region().index, independent.region().index);
        assert!(first.private_fill().is_err());
        fill.as_mut_slice()[0] = 42;
        let buffer = fill.publish(1).unwrap();
        drop(independent);
        let reused = first.stage(key).unwrap();
        assert_ne!(reused.region().index, buffer.region().index);
        let namespace = Namespace::new("test:1").unwrap();
        let other = WorkerContext::test(2);
        assert!(Cache::new(&other, namespace, states).is_err());
        // Missing assignments cannot be disguised as a smaller local shard set.
        assert!(Cache::new(&c, namespace, Vec::new()).is_err());
    }
    #[test]
    fn activated_cache_owns_storage_and_releases_it_on_drop() {
        let context = WorkerContext::test(2);
        let pool = buffers::io_test_pool(1);
        let mut slab = slab(2);
        let mut file = None;
        let states = context
            .take_assignments()
            .unwrap()
            .into_iter()
            .map(|assignment| {
                let storage = slab.take_shard(assignment.id()).unwrap();
                file = Some(Arc::downgrade(&storage.file_identity()));
                ShardState::activate(
                    &context,
                    assignment,
                    storage,
                    &pool,
                    allocator::Config::default(),
                )
                .unwrap()
            })
            .collect();
        let cache = Cache::new(&context, Namespace::new("test:1").unwrap(), states).unwrap();
        drop(slab);
        let file = file.unwrap();
        assert!(file.upgrade().is_some());
        drop(cache);
        assert!(file.upgrade().is_none());
    }
    #[test]
    fn cache_rejects_mixed_slabs_and_reordered_shards() {
        for mixed in [false, true] {
            let c = WorkerContext::test(2);
            let pool = buffers::io_test_pool(1);
            let mut first = slab(2);
            let mut second = slab(2);
            let mut states: Vec<_> = c
                .take_assignments()
                .unwrap()
                .into_iter()
                .map(|a| {
                    let slab = if mixed && a.id().index() == 1 {
                        &mut second
                    } else {
                        &mut first
                    };
                    let storage = slab.take_shard(a.id()).unwrap();
                    ShardState::activate(&c, a, storage, &pool, allocator::Config::default())
                        .unwrap()
                })
                .collect();
            if !mixed {
                states.reverse();
            }
            assert!(Cache::new(&c, Namespace::new("test:1").unwrap(), states).is_err());
        }
    }

    fn generation_states(
        context: &WorkerContext,
        generation: &StorageGeneration,
        pool: &buffers::WorkerPool,
        slab: &mut Slab,
    ) -> Vec<ShardState> {
        generation
            .take_assignments(context)
            .unwrap()
            .into_iter()
            .map(|a| {
                let storage = slab.take_shard(a.id()).unwrap();
                ShardState::activate(context, a, storage, pool, allocator::Config::default())
                    .unwrap()
            })
            .collect()
    }

    #[test]
    fn existing_large_layout_has_complete_unique_authority_but_planning_stays_bounded() {
        let context = WorkerContext::test(1);
        let generation = context.storage_generation(2048).unwrap();
        let other = context.storage_generation(2048).unwrap();
        assert!(!Arc::ptr_eq(&generation.identity, &other.identity));
        assert_eq!(generation.worker_count(), 1);
        assert_eq!(generation.shard_count(), 2048);
        assert!(
            generation
                .take_assignments(&WorkerContext::test(1))
                .is_err()
        );
        let mut slab = slab(2048);
        let pool = buffers::io_test_pool(1);
        let states = generation_states(&context, &generation, &pool, &mut slab);
        assert_eq!(states.len(), 2048);
        assert!(generation.take_assignments(&context).is_err());
        assert!(
            Cache::for_generation(
                &context,
                &other,
                Namespace::new("test").unwrap(),
                Vec::new()
            )
            .is_err()
        );
        let _cache = Cache::for_generation(
            &context,
            &generation,
            Namespace::new("test").unwrap(),
            states,
        )
        .unwrap();
        let plan = allocator::LayoutPlan::new(64 << 30, 1).unwrap();
        assert_eq!(plan.shard_count(), 4);
        assert_eq!(plan.authorize(&context).unwrap().shard_count(), 4);
        assert!(allocator::LayoutPlan::new(64 << 30, 2048).is_err());
        assert!(allocator::LayoutPlan::new(allocator::MAX_CAPACITY + (4 << 20), 1).is_err());
        let max =
            allocator::LayoutPlan::new(allocator::MAX_CAPACITY, allocator::MAX_PLANNED_SHARDS)
                .unwrap();
        assert_eq!(max.shard_count(), allocator::MAX_PLANNED_SHARDS);
    }

    #[test]
    fn replacement_generations_grow_and_shrink_on_same_execution_and_pool() {
        let context = WorkerContext::test(1);
        let pool = buffers::io_test_pool(1);
        context.bind_pool(&pool).unwrap();
        let mut ring =
            crate::uring::Ring::http_test_ring(pool.clone(), crate::uring::Config::default())
                .unwrap();
        let namespace = Namespace::new("replacement").unwrap();
        for count in [1, 128, 256, 1] {
            let generation = context.storage_generation(count).unwrap();
            let mut slab = slab(count);
            let states = generation_states(&context, &generation, &pool, &mut slab);
            assert_eq!(states.len(), count);
            assert!(
                states
                    .iter()
                    .all(|s| s.buffers.as_ref().unwrap().pool().same_pool(&pool))
            );
            assert!(generation.take_assignments(&context).is_err());
            let mut cache =
                Cache::for_generation(&context, &generation, namespace, states).unwrap();
            assert!(cache.poll_shutdown(&mut ring).unwrap().0);
            assert_eq!(context.shard_count(), 1);
            assert_eq!(context.shard_ids(), &[ShardId::at(0)]);
            assert!(context.bind_pool(&buffers::io_test_pool(1)).is_err());
        }
    }

    #[test]
    fn replacement_authority_rejects_foreign_worker_plan_and_generation() {
        let plan: Arc<[Placement]> = placements(
            vec![(CpuId(0), NumaNodeId(0)), (CpuId(1), NumaNodeId(0))],
            2,
        )
        .unwrap()
        .into();
        let a = WorkerContext::pinned(plan.clone(), 0);
        let b = WorkerContext::pinned(plan, 1);
        assert!(a.storage_generation(1).is_err());
        let generation = a.storage_generation(5).unwrap();
        assert!(
            generation
                .take_assignments(&WorkerContext::test(2))
                .is_err()
        );
        let assignments = generation.take_assignments(&a).unwrap();
        assert_eq!(
            assignments
                .iter()
                .map(|a| a.id().index())
                .collect::<Vec<_>>(),
            [0, 2, 4]
        );
        for assignment in assignments {
            assert!(b.check(&assignment).is_err());
        }
        assert_eq!(generation.take_assignments(&b).unwrap().len(), 2);
        let c = WorkerContext::test(1);
        let pool = buffers::io_test_pool(1);
        for mixed in [false, true] {
            let g1 = c.storage_generation(2).unwrap();
            let g2 = c.storage_generation(2).unwrap();
            let mut slab1 = slab(2);
            let mut slab2 = slab(2);
            let mut s1 = generation_states(&c, &g1, &pool, &mut slab1);
            let mut s2 = generation_states(&c, &g2, &pool, &mut slab2);
            if mixed {
                std::mem::swap(&mut s1[1], &mut s2[1]);
            }
            assert!(Cache::for_generation(&c, &g2, Namespace::new("test").unwrap(), s1).is_err());
        }
        assert!(
            allocator::LayoutPlan::new(64 * 1024 * 1024, 2)
                .unwrap()
                .authorize(&c)
                .is_err()
        );
    }

    #[test]
    fn new_generation_rejects_geometry_order_missing_and_legacy_open() {
        let c = WorkerContext::test(1);
        let pool = buffers::io_test_pool(1);
        for reverse in [false, true] {
            let generation = c.storage_generation(3).unwrap();
            let mut slab = slab(3);
            let mut states = generation_states(&c, &generation, &pool, &mut slab);
            if reverse {
                states.reverse();
            } else {
                states.pop();
            }
            assert!(
                Cache::for_generation(&c, &generation, Namespace::new("test").unwrap(), states)
                    .is_err()
            );
        }
        let generation = c.storage_generation(2).unwrap();
        let mut assignments = generation.take_assignments(&c).unwrap();
        let mut wrong = slab(1);
        assert!(
            ShardState::activate(
                &c,
                assignments.remove(0),
                wrong.take_shard(ShardId::at(0)).unwrap(),
                &pool,
                allocator::Config::default()
            )
            .is_err()
        );
        let mut expanded = slab(2);
        assert!(
            Allocator::open(
                &c,
                expanded.take_shard(ShardId::at(1)).unwrap(),
                allocator::Config::default()
            )
            .is_err()
        );
        let foreign_pool = buffers::io_test_pool(1);
        let generation = c.storage_generation(2).unwrap();
        let assignment = generation.take_assignments(&c).unwrap().remove(0);
        assert!(
            ShardState::activate(
                &c,
                assignment,
                expanded.take_shard(ShardId::at(0)).unwrap(),
                &foreign_pool,
                allocator::Config::default()
            )
            .is_err()
        );
    }
}
