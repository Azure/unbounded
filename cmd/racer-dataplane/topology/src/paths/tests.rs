use super::*;
use futures::{executor::block_on, task::noop_waker_ref};
use std::{future::Future, num::NonZeroU32, task::Context};

#[test]
fn cache_duplicate_store_and_lru_match_deterministic_model() {
    let members = membership(32);
    let keys: Vec<_> = (1..32)
        .map(|to| PathKey::new(&members, &query(0, to, 4)).unwrap())
        .collect();
    let mut cache = PathCache::default();
    let mut model = VecDeque::new();
    let alternatives = Rc::new(vec![vec![0, 1]]);
    let charge = PathCache::entry_bytes(&keys[0], &alternatives, alternatives.capacity());
    let mut state = 0xa712_81d3u64;
    for turn in 0..2048 {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        let index = usize::try_from(state % 31).unwrap();
        let key = &keys[index];
        if turn % 3 == 0 {
            let found = model.iter().position(|v| *v == index);
            assert_eq!(cache.get(key).is_some(), found.is_some());
            if let Some(position) = found {
                model.remove(position);
                model.push_back(index);
            }
        } else {
            let before = cache.bytes;
            cache.store(key.clone(), Rc::clone(&alternatives), 7, charge * 5);
            if model.contains(&index) {
                // Duplicate store is idempotent, including recency and accounting.
                assert_eq!(cache.bytes, before);
            } else {
                if model.len() == 5 {
                    model.pop_front();
                }
                model.push_back(index);
            }
        }
        assert_eq!(cache.entries.len(), model.len());
        assert_eq!(cache.bytes, charge * model.len());
        assert_eq!(
            cache.bytes,
            cache
                .entries
                .values()
                .map(|entry| entry.bytes)
                .sum::<usize>()
        );
        let mut current = cache.oldest.clone();
        let mut previous: Option<Rc<PathKey>> = None;
        for &expected in &model {
            let owned = current.take().unwrap();
            assert!(owned.as_ref() == &keys[expected]);
            let entry = cache.entries.get(&owned).unwrap();
            assert!(entry.older == previous);
            current.clone_from(&entry.newer);
            previous = Some(owned);
        }
        assert!(current.is_none());
        assert!(previous == cache.newest);
    }
    let saved = cache.bytes;
    // Oversized unique result bypasses without changing existing entries.
    let mut key = keys[0].clone();
    key.links = 5;
    cache.store(key, Rc::new(vec![vec![0; 10_000]]), 7, charge * 5);
    assert_eq!(cache.bytes, saved);
    while !cache.entries.is_empty() {
        cache.evict();
    }
    assert_eq!(cache.bytes, 0);
    assert!(cache.oldest.is_none() && cache.newest.is_none());
    cache.store(keys[0].clone(), Rc::clone(&alternatives), 1, charge);
    let owned = Rc::clone(cache.entries.first_key_value().unwrap().0);
    for _ in 0..100 {
        assert!(Rc::ptr_eq(&cache.get(&keys[0]).unwrap(), &alternatives));
        assert!(Rc::ptr_eq(
            cache.entries.first_key_value().unwrap().0,
            &owned
        ));
        assert_eq!(cache.bytes, charge);
    }
    cache.store(keys[0].clone(), Rc::new(vec![vec![9; 100]]), 1, charge);
    assert!(Rc::ptr_eq(&cache.get(&keys[0]).unwrap(), &alternatives));
    assert_eq!(cache.bytes, charge);
}

#[derive(Clone)]
struct TestMember(Vec<u8>, NonZeroU32);
impl Member for TestMember {
    const DOMAIN: &'static str = "racer";
    fn id(&self) -> &[u8] {
        &self.0
    }
    fn weight(&self) -> NonZeroU32 {
        self.1
    }
}
fn membership(n: usize) -> Membership<TestMember> {
    Membership::new(
        (0..n)
            .map(|i| {
                TestMember(
                    format!("node-{i:06}").into_bytes(),
                    NonZeroU32::new(4).unwrap(),
                )
            })
            .collect(),
    )
    .unwrap()
}
fn query(from: usize, to: usize, links: u8) -> PathQuery<'static> {
    PathQuery {
        from,
        to,
        links,
        visited: &[],
        blocked: &[],
        seed: &[],
    }
}
// Independent ring definition, using explicit schema bytes and sorted tuples.
fn graph(n: usize) -> Vec<Vec<usize>> {
    let mut edges = vec![vec![]; n];
    if n < 2 {
        return edges;
    }
    for ring in 0u32..32 {
        let mut order: Vec<_> = (0..n)
            .map(|position| {
                let id = format!("node-{position:06}");
                let mut bytes = b"racer/overlay/sha256-rings-32/v1\0".to_vec();
                bytes.extend_from_slice(&ring.to_be_bytes());
                bytes.extend_from_slice(&(id.len() as u64).to_be_bytes());
                bytes.extend_from_slice(id.as_bytes());
                (<[u8; 32]>::from(sha2::Sha256::digest(bytes)), position)
            })
            .collect();
        order.sort_unstable();
        for index in 0..n {
            let a = order[index].1;
            let b = order[(index + 1) % n].1;
            edges[a].push(b);
            edges[b].push(a);
        }
    }
    for neighbors in &mut edges {
        neighbors.sort_unstable();
        neighbors.dedup();
    }
    edges
}
fn distances(edges: &[Vec<usize>], key: &PathKey) -> Vec<usize> {
    let mut distances = vec![usize::MAX; edges.len()];
    distances[key.to] = 0;
    let mut queue = VecDeque::from([key.to]);
    while let Some(node) = queue.pop_front() {
        for &next in &edges[node] {
            if key.visited.contains(&next)
                || (node == key.from && key.blocked.contains(&next))
                || (next == key.from && key.blocked.contains(&node))
                || distances[next] != usize::MAX
            {
                continue;
            }
            distances[next] = distances[node] + 1;
            queue.push_back(next);
        }
    }
    distances
}
fn search(members: &Membership<TestMember>, key: &PathKey) -> Result<Vec<Vec<usize>>> {
    let mut search = EqualCostSearch::new(members.graph(), key);
    while !search.step(1).done {}
    search.finish()
}
#[test]
fn all_equal_next_hops_match_independent_oracle() {
    for n in [37, 401, 1500] {
        let edges = graph(n);
        let members = membership(n);
        for source in [0, 19, n - 1] {
            for to in (0..n).step_by(17).filter(|&to| to != source) {
                for filtered in [false, true] {
                    let mut key = PathKey::new(&members, &query(source, to, 4)).unwrap();
                    if filtered {
                        key.visited = [7, 33]
                            .into_iter()
                            .filter(|v| *v != source && *v != to)
                            .collect();
                        key.blocked = edges[source].iter().copied().step_by(2).collect();
                    }
                    let distance = distances(&edges, &key);
                    for links in [1, 2, 4, 255] {
                        key.links = links;
                        let result = search(&members, &key);
                        if distance[source] > links as usize {
                            assert_eq!(result, Err(Error::Unreachable));
                            continue;
                        }
                        let alternatives = result.unwrap();
                        let expected: Vec<_> = edges[source]
                            .iter()
                            .copied()
                            .filter(|v| {
                                !key.blocked.contains(v)
                                    && !key.visited.contains(v)
                                    && distance[*v].checked_add(1) == Some(distance[source])
                            })
                            .collect();
                        assert_eq!(
                            alternatives.iter().map(|p| p[1]).collect::<Vec<_>>(),
                            expected
                        );
                        for path in alternatives {
                            assert_eq!(path.first(), Some(&source));
                            assert_eq!(path.last(), Some(&to));
                            assert_eq!(path.len(), distance[source] + 1);
                            for pair in path.windows(2) {
                                assert!(edges[pair[0]].contains(&pair[1]));
                                assert!(!key.visited.contains(&pair[1]));
                            }
                        }
                    }
                }
            }
        }
    }
}
#[test]
fn independent_hash_vectors_cache_reselection_and_snapshot_weights() {
    let mut input = membership(1500).members().to_vec();
    for (i, member) in input.iter_mut().enumerate() {
        member.1 = NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
    }
    let members = Membership::new(input).unwrap();
    let cached = Paths::new(1);
    let cold = Paths::new(0);
    let alternatives = search(
        &members,
        &PathKey::new(&members, &query(0, 1499, 4)).unwrap(),
    )
    .unwrap();
    assert!(alternatives.len() > 1);
    // Independent hash byte construction, separate from production helpers.
    for attempt in [0u128, 1, 2, 127] {
        let mut seed = vec![1; 16];
        seed.extend_from_slice(&attempt.to_be_bytes());
        let query = PathQuery {
            seed: &seed,
            ..query(0, 1499, 4)
        };
        let mut bytes = b"racer/next-hop/v5\0".to_vec();
        for field in [seed.as_slice(), members.id(0), members.id(1499)] {
            bytes.extend_from_slice(&u32::try_from(field.len()).unwrap().to_be_bytes());
            bytes.extend_from_slice(field);
        }
        bytes.extend_from_slice(&0u32.to_be_bytes());
        let digest = sha2::Sha256::digest(bytes);
        let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
        let weights: Vec<_> = alternatives
            .iter()
            .map(|p| u64::from(members.weight(p[1]).get()))
            .collect();
        let total: u64 = weights.iter().sum();
        assert!(sample >= total.wrapping_neg() % total);
        let mut ticket = sample % total;
        let expected = alternatives
            .iter()
            .zip(weights)
            .find_map(|(path, weight)| {
                if ticket < weight {
                    Some(path[1])
                } else {
                    ticket -= weight;
                    None
                }
            })
            .unwrap();
        let actual = block_on(cached.route(&members, query)).unwrap();
        assert_eq!(actual[1], expected);
        assert_eq!(actual, block_on(cold.route(&members, query)).unwrap());
    }
    let mut changed = members.members().to_vec();
    let favored = alternatives[0][1];
    changed[favored].1 = NonZeroU32::new(u32::MAX).unwrap();
    let changed = Membership::new(changed).unwrap();
    let mut seed = vec![1; 16];
    seed.extend_from_slice(&[2; 16]);
    let query = PathQuery {
        seed: &seed,
        ..query(0, 1499, 4)
    };
    let previous = Rc::clone(
        &cached
            .cache
            .borrow()
            .entries
            .first_key_value()
            .unwrap()
            .1
            .alternatives,
    );
    assert_eq!(block_on(cached.route(&changed, query)).unwrap()[1], favored);
    assert!(Rc::ptr_eq(
        &previous,
        &cached
            .cache
            .borrow()
            .entries
            .first_key_value()
            .unwrap()
            .1
            .alternatives
    ));
    assert_eq!(cached.cache.borrow().entries.len(), 1);
    assert_eq!(cold.cache.borrow().entries.len(), 0);
}
#[test]
fn last_first_hop_bit_survives_search_and_reconstruction() {
    let n = 100_000;
    let source = 19;
    let members = membership(n);
    let neighbors = members.neighbors(source);
    assert_eq!(neighbors.len(), 64);
    let last = neighbors[63];
    let mut key = PathKey::new(&members, &query(source, last, 4)).unwrap();
    key.blocked = neighbors[..63].to_vec();
    let next = members
        .neighbors(last)
        .into_iter()
        .find(|v| *v != source && !neighbors.contains(v))
        .unwrap();
    for to in [last, next] {
        key.to = to;
        let alternatives = search(&members, &key).unwrap();
        assert_eq!(alternatives.len(), 1);
        assert_eq!(alternatives[0][1], last);
        assert_eq!(alternatives[0].last(), Some(&to));
    }
    key.blocked = neighbors;
    assert_eq!(search(&members, &key), Err(Error::Unreachable));
}
#[test]
fn weighted_integer_mapping_and_probabilities() {
    assert_eq!(weighted_draw(0, 5, &[4, 1]), None);
    let mut counts = [0; 2];
    for sample in 1..=10_000 {
        counts[weighted_draw(sample, 5, &[4, 1]).unwrap()] += 1;
    }
    assert_eq!(counts, [8000, 2000]);
    let max = u64::from(u32::MAX);
    assert_eq!(weighted_draw(u64::MAX, 64 * max, &[max; 64]), Some(1));
}
#[test]
fn search_work_is_bounded_per_turn() {
    let members = membership(100_000);
    let key = PathKey::new(&members, &query(0, 80_003, 4)).unwrap();
    let mut search = EqualCostSearch::new(members.graph(), &key);
    assert!(Arc::ptr_eq(&search.graph, &members.graph()));
    loop {
        let step = search.step(7);
        assert!(step.expansions <= 7);
        assert!(step.edges <= step.expansions * MAX_DEGREE);
        assert!(
            search
                .waves
                .iter()
                .map(|wave| wave.visits.len())
                .sum::<usize>()
                <= 2 * 100_000
        );
        if step.done {
            break;
        }
    }
    let alternatives = search.finish().unwrap();
    assert!(alternatives.len() <= MAX_DEGREE);
    assert!(alternatives.iter().all(|p| p.len() <= 5));
}
#[test]
fn exhaustive_inverse_matches_definition() {
    // Retain the historical test name, now checking the ID-ring definition.
    for n in 1..=160 {
        let members = membership(n);
        let expected = graph(n);
        for (i, row) in expected.iter().enumerate() {
            assert_eq!(members.neighbor_slice(i), row, "N={n}, i={i}");
        }
    }
    assert!(membership(0).neighbors(0).is_empty());
    assert!(membership(1).neighbors(usize::MAX).is_empty());
}
#[test]
fn hundred_thousand_nodes_bounded_symmetric_and_four_link_reachable() {
    let n = 100_000;
    let members = membership(n);
    for i in 0..n {
        let neighbors = members.neighbor_slice(i);
        assert!(neighbors.len() <= MAX_DEGREE);
        for &other in neighbors {
            assert!(members.neighbor_slice(other).binary_search(&i).is_ok());
        }
    }
    for source in [0, 1, 17, 49_999, 99_999] {
        let mut distance = vec![u8::MAX; n];
        distance[source] = 0;
        let mut queue = VecDeque::from([source]);
        while let Some(i) = queue.pop_front() {
            if distance[i] == 4 {
                continue;
            }
            for &j in members.neighbor_slice(i) {
                if distance[j] == u8::MAX {
                    distance[j] = distance[i] + 1;
                    queue.push_back(j);
                }
            }
        }
        assert!(distance.iter().all(|d| *d <= 4));
    }
}
#[test]
fn arbitrary_binary_members_invalid_queries_and_first_hop_only_blocking() {
    struct Binary([u8; 2]);
    impl Member for Binary {
        const DOMAIN: &'static str = "binary";
        fn id(&self) -> &[u8] {
            &self.0
        }
        fn weight(&self) -> NonZeroU32 {
            NonZeroU32::new(1).unwrap()
        }
    }
    let members = Membership::new((0u16..1500).map(|i| Binary(i.to_be_bytes())).collect()).unwrap();
    let paths = Paths::new(2);
    let basic = query(0, 1499, 255);
    let route = block_on(paths.route(&members, basic)).unwrap();
    assert_eq!(route.first(), Some(&0));
    assert_eq!(route.last(), Some(&1499));
    assert!(route.len() > 2);
    // Blocking the destination's first-hop edge does not ban reaching it later.
    assert_eq!(
        block_on(paths.route(
            &members,
            PathQuery {
                blocked: &[1499],
                ..basic
            }
        ))
        .unwrap(),
        route
    );
    for invalid in [
        PathQuery {
            from: usize::MAX,
            ..basic
        },
        PathQuery { to: 1500, ..basic },
        PathQuery {
            visited: &[1500],
            links: 4,
            ..basic
        },
        PathQuery {
            blocked: &[usize::MAX],
            ..basic
        },
        PathQuery {
            visited: &[0],
            links: 4,
            ..basic
        },
        PathQuery {
            visited: &[1499],
            links: 4,
            ..basic
        },
        PathQuery {
            visited: &[1, 1],
            links: 4,
            ..basic
        },
        PathQuery {
            visited: &[1],
            ..basic
        },
    ] {
        assert_eq!(
            block_on(paths.route(&members, invalid)),
            Err(Error::InvalidQuery)
        );
    }
    assert_eq!(
        block_on(paths.route(&members, query(0, 1499, 0))),
        Err(Error::Unreachable)
    );
    assert_eq!(block_on(paths.route(&members, query(0, 0, 0))), Ok(vec![0]));
    let empty = Membership::<Binary>::new(vec![]).unwrap();
    assert!(empty.neighbors(0).is_empty());
    assert!(members.neighbors(1500).is_empty());
    assert_eq!(
        block_on(paths.route(&empty, query(0, 0, 0))),
        Err(Error::InvalidQuery)
    );
}
#[test]
fn canonical_cache_identity_includes_domain_and_weights() {
    struct Other(TestMember);
    impl Member for Other {
        const DOMAIN: &'static str = "other";
        fn id(&self) -> &[u8] {
            self.0.id()
        }
        fn weight(&self) -> NonZeroU32 {
            self.0.weight()
        }
    }
    let members = membership(1500);
    let other = Membership::new(members.members().iter().cloned().map(Other).collect()).unwrap();
    let paths = Paths::new(4);
    let a = PathQuery {
        visited: &[7, 33],
        blocked: &[1, 2],
        ..query(0, 1499, 4)
    };
    let b = PathQuery {
        visited: &[33, 7],
        blocked: &[2, 1, 2],
        ..a
    };
    assert_eq!(
        block_on(paths.route(&members, a)),
        block_on(paths.route(&members, b))
    );
    assert_eq!(paths.cache.borrow().entries.len(), 1);
    let identical = membership(1500);
    assert_eq!(
        block_on(paths.route(&members, a)),
        block_on(paths.route(&identical, a))
    );
    assert_eq!(paths.cache.borrow().entries.len(), 1);
    assert_eq!(
        block_on(paths.route(&other, a)).unwrap(),
        block_on(Paths::new(0).route(&other, a)).unwrap()
    );
    assert_eq!(paths.cache.borrow().entries.len(), 2);
    let mut changed = members.members().to_vec();
    changed[0].1 = NonZeroU32::new(1).unwrap();
    block_on(paths.route(&Membership::new(changed).unwrap(), a)).unwrap();
    // Historical name retained: weights affect selection, not cache identity.
    assert_eq!(paths.cache.borrow().entries.len(), 2);
}
#[test]
fn cooperative_admission_cancellation_and_cache_hits() {
    let members = membership(100_000);
    let base = query(0, 80_003, 4);
    let mut cx = Context::from_waker(noop_waker_ref());
    for capacity in [0, 1, 32] {
        let paths = Paths::with_limits(capacity, usize::MAX, 2);
        let mut pending = Vec::new();
        for to in [80_003, 80_004] {
            let mut future = Box::pin(paths.route(&members, PathQuery { to, ..base }));
            assert!(future.as_mut().poll(&mut cx).is_pending());
            pending.push(future);
        }
        assert_eq!(
            block_on(paths.route(&members, PathQuery { to: 80_005, ..base })),
            Err(Error::Overloaded)
        );
        drop(pending);
        assert_eq!(paths.active_searches(), 0);
        let expected = block_on(paths.route(&members, base)).unwrap();
        assert_eq!(paths.active_searches(), 0);
        let mut future = Box::pin(paths.route(&members, base));
        let first = future.as_mut().poll(&mut cx);
        if capacity == 0 {
            assert!(first.is_pending());
        } else {
            assert_eq!(first, Poll::Ready(Ok(expected)));
        }
    }
}

#[test]
fn self_routes_validate_before_bypassing_admission_and_cache() {
    let members = membership(1500);
    let paths = Paths::with_limits(0, 0, 0);
    assert_eq!(block_on(paths.route(&members, query(0, 0, 0))), Ok(vec![0]));
    assert_eq!(
        block_on(paths.route(&members, query(0, 1, 4))),
        Err(Error::Overloaded)
    );
    for invalid in [
        PathQuery {
            blocked: &[usize::MAX],
            ..query(0, 0, 0)
        },
        PathQuery {
            visited: &[0],
            ..query(0, 0, 0)
        },
        query(1500, 1500, 0),
    ] {
        assert_eq!(
            block_on(paths.route(&members, invalid)),
            Err(Error::InvalidQuery)
        );
    }
    assert_eq!(paths.cached_entries(), 0);
    assert_eq!(paths.active_searches(), 0);
}

#[test]
fn memory_budget_lru_and_same_count_id_invalidation() {
    let members = membership(1500);
    let paths = Paths::with_limits(2, usize::MAX, 1);
    let a = query(0, 1499, 4);
    let b = query(1, 1499, 4);
    let c = query(2, 1499, 4);
    for q in [a, b, a, c] {
        block_on(paths.route(&members, q)).unwrap();
    }
    assert_eq!(paths.cached_entries(), 2);
    let cache = paths.cache.borrow();
    assert_eq!(cache.oldest.as_ref().unwrap().from, 0);
    assert_eq!(cache.newest.as_ref().unwrap().from, 2);
    drop(cache);
    let single = Paths::new(1);
    block_on(single.route(&members, a)).unwrap();
    let bytes = single.cached_bytes();
    assert!(bytes > 0);
    for budget in [0, 1, bytes - 1, bytes, bytes * 2] {
        let bounded = Paths::with_limits(1000, budget, 1);
        block_on(bounded.route(&members, a)).unwrap();
        assert_eq!(bounded.cached_entries(), usize::from(budget >= bytes));
        for q in [b, c, a, b] {
            block_on(bounded.route(&members, q)).unwrap();
            assert!(bounded.cached_bytes() <= budget);
        }
    }
    let mut values = members.members().to_vec();
    values[1499].0 = b"zz-replacement".to_vec();
    let replaced = Membership::new(values).unwrap();
    let paths = Paths::new(8);
    block_on(paths.route(&members, a)).unwrap();
    block_on(paths.route(&replaced, a)).unwrap();
    assert_eq!(paths.cached_entries(), 2);
    assert_ne!(
        paths.cache.borrow().oldest.as_ref().unwrap().membership,
        paths.cache.borrow().newest.as_ref().unwrap().membership
    );
}

#[test]
fn duplicate_searches_share_progress_survive_cancellation_and_reselect() {
    let members = membership(10_000);
    let base = query(0, 9999, 4);
    let paths = Paths::with_limits(0, 0, 1);
    let mut cx = Context::from_waker(noop_waker_ref());
    let mut first = Box::pin(paths.route(&members, base));
    assert!(first.as_mut().poll(&mut cx).is_pending());
    let shared = Rc::clone(paths.inflight.borrow().values().next().unwrap());
    let mut second = Box::pin(paths.route(
        &members,
        PathQuery {
            seed: b"different",
            ..base
        },
    ));
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert!(Rc::ptr_eq(
        &shared,
        paths.inflight.borrow().values().next().unwrap()
    ));
    drop(shared);
    assert_eq!(paths.active_searches(), 1);
    assert_eq!(block_on(paths.route(&members, query(0, 0, 0))), Ok(vec![0]));
    assert_eq!(
        block_on(paths.route(&members, query(1, 9999, 4))),
        Err(Error::Overloaded)
    );
    drop(first);
    assert_eq!(paths.active_searches(), 1);
    let actual = block_on(second).unwrap();
    assert_eq!(paths.active_searches(), 0);
    assert_eq!(
        actual,
        block_on(Paths::new(0).route(
            &members,
            PathQuery {
                seed: b"different",
                ..base
            }
        ))
        .unwrap()
    );
    let mut first = Box::pin(paths.route(&members, base));
    let mut second = Box::pin(paths.route(&members, base));
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    drop(second);
    assert_eq!(paths.active_searches(), 1);
    drop(first);
    assert_eq!(paths.active_searches(), 0);
    // Completed but unpolled followers must not remove a new generation.
    let mut first = Box::pin(paths.route(&members, base));
    let mut follower = Box::pin(paths.route(&members, base));
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(follower.as_mut().poll(&mut cx).is_pending());
    block_on(first).unwrap();
    let mut next = Box::pin(paths.route(&members, base));
    assert!(next.as_mut().poll(&mut cx).is_pending());
    drop(follower);
    assert_eq!(paths.active_searches(), 1);
    drop(next);
    assert_eq!(paths.active_searches(), 0);
}

#[test]
fn cache_size_and_admission_are_independent() {
    let members = membership(10_000);
    let mut cx = Context::from_waker(noop_waker_ref());
    for entries in [0, 1, 1000] {
        for limit in [0, 1, 3] {
            let paths = Paths::with_limits(entries, 4096, limit);
            let mut waiters = Vec::new();
            for index in 0..limit {
                let mut future = Box::pin(
                    paths.route(&members, query(0, 9999, 4 + u8::try_from(index).unwrap())),
                );
                assert!(future.as_mut().poll(&mut cx).is_pending());
                waiters.push(future);
            }
            assert_eq!(paths.active_searches(), limit);
            assert_eq!(
                block_on(paths.route(&members, query(0, 9999, 7))),
                Err(Error::Overloaded)
            );
            drop(waiters);
            assert_eq!(paths.active_searches(), 0);
        }
    }
    assert_eq!(Paths::new(0).search_limit, Paths::new(1000).search_limit);
}

#[test]
fn validation_limits_and_deterministic_fuzz_cases() {
    let members = membership(401);
    let paths = Paths::new(4);
    let visited: Vec<_> = (1..=255).collect();
    assert!(
        PathKey::new(
            &members,
            &PathQuery {
                visited: &visited,
                ..query(0, 400, 0)
            }
        )
        .is_ok()
    );
    for links in [1, 4, 255] {
        assert_eq!(
            block_on(paths.route(
                &members,
                PathQuery {
                    visited: &visited,
                    ..query(0, 400, links)
                }
            )),
            Err(Error::InvalidQuery)
        );
    }
    let blocked = vec![1; 65];
    let seed = vec![0; MAX_SEED_BYTES + 1];
    for invalid in [
        PathQuery {
            blocked: &blocked,
            ..query(0, 400, 4)
        },
        PathQuery {
            seed: &seed,
            ..query(0, 400, 4)
        },
    ] {
        assert_eq!(
            block_on(paths.route(&members, invalid)),
            Err(Error::InvalidQuery)
        );
    }
    assert!(
        PathKey::new(
            &members,
            &PathQuery {
                blocked: &blocked[..64],
                seed: &seed[..MAX_SEED_BYTES],
                ..query(0, 400, 4)
            }
        )
        .is_ok()
    );
    let non_neighbor = (1..401)
        .find(|v| !members.neighbor_slice(0).contains(v))
        .unwrap();
    let canonical = PathKey::new(
        &members,
        &PathQuery {
            blocked: &[non_neighbor, non_neighbor, 0],
            ..query(0, 400, 4)
        },
    )
    .unwrap();
    assert!(canonical.blocked.is_empty());
    check_deterministic_fuzz_cases(&members, &paths);
}

fn check_deterministic_fuzz_cases(members: &Membership<TestMember>, paths: &Paths) {
    let edges = members.graph();
    let mut state = 0xa127_3921u64;
    for iteration in 0..128 {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        let from = usize::try_from(state % 401).unwrap();
        let to = usize::try_from((state >> 32) % 401).unwrap();
        let visited: Vec<_> = (0..iteration % 9)
            .map(|i| (i * 37 + iteration) % 401)
            .filter(|v| *v != from && *v != to)
            .collect();
        let blocked: Vec<_> = members
            .neighbor_slice(from)
            .iter()
            .copied()
            .step_by(3)
            .collect();
        let q = PathQuery {
            visited: &visited,
            blocked: &blocked,
            ..query(from, to, u8::try_from(iteration % 5).unwrap())
        };
        let key = PathKey::new(members, &q).unwrap();
        let distance = distances(&edges, &key)[from];
        let actual = block_on(paths.route(members, q));
        if distance > usize::from(q.links) {
            assert_eq!(actual, Err(Error::Unreachable));
        } else {
            let route = actual.unwrap();
            assert_eq!(route.len(), distance + 1);
            assert_eq!(route.first(), Some(&from));
            assert_eq!(route.last(), Some(&to));
            for pair in route.windows(2) {
                assert!(edges[pair[0]].contains(&pair[1]));
                assert!(!visited.contains(&pair[1]));
            }
            if route.len() > 1 {
                assert!(!blocked.contains(&route[1]));
            }
        }
    }
}

#[test]
fn rejection_retry_is_bounded_and_distinct_from_unreachable() {
    let mut calls = 0;
    assert_eq!(
        sample_index(&[4, 1], 5, |counter| {
            assert_eq!(counter, calls);
            calls += 1;
            0
        }),
        Err(Error::SamplingExhausted)
    );
    assert_eq!(calls, 64);
    assert_eq!(
        sample_index(&[4, 1], 5, |counter| if counter == 63 { 4 } else { 0 }),
        Ok(1)
    );
}

#[test]
fn selection_reads_frozen_ids_and_weights_after_interior_mutation() {
    use std::cell::Cell;
    struct Mutable {
        id: Vec<u8>,
        changed: Cell<bool>,
        weight: Cell<NonZeroU32>,
    }
    impl Member for Mutable {
        const DOMAIN: &'static str = "racer";
        fn id(&self) -> &[u8] {
            if self.changed.get() {
                b"changed"
            } else {
                &self.id
            }
        }
        fn weight(&self) -> NonZeroU32 {
            self.weight.get()
        }
    }
    let original = membership(1500);
    let values = original
        .members()
        .iter()
        .enumerate()
        .map(|(i, m)| Mutable {
            id: m.0.clone(),
            changed: Cell::new(false),
            weight: Cell::new(NonZeroU32::new(if i % 2 == 0 { 1 } else { 9 }).unwrap()),
        })
        .collect();
    let frozen = Membership::new(values).unwrap();
    let expected = Membership::new(
        frozen
            .members()
            .iter()
            .map(|m| TestMember(m.id.clone(), m.weight.get()))
            .collect(),
    )
    .unwrap();
    let paths = Paths::new(1);
    for m in frozen.members() {
        m.changed.set(true);
        m.weight.set(NonZeroU32::new(u32::MAX).unwrap());
    }
    for seed in [b"a".as_slice(), b"b", b"c"] {
        let q = PathQuery {
            seed,
            ..query(0, 1499, 4)
        };
        assert_eq!(
            block_on(paths.route(&frozen, q)),
            block_on(Paths::new(0).route(&expected, q))
        );
    }
    assert_eq!(paths.cached_entries(), 1);
}

#[test]
fn concurrent_weight_snapshots_share_search_but_not_selection() {
    let members = membership(10_000);
    let q = query(0, 9999, 4);
    let alternatives = search(&members, &PathKey::new(&members, &q).unwrap()).unwrap();
    assert!(alternatives.len() > 1);
    let mut values = members.members().to_vec();
    let favored = alternatives[0][1];
    values[favored].1 = NonZeroU32::new(u32::MAX).unwrap();
    let changed = Membership::new(values).unwrap();
    let paths = Paths::with_limits(0, 0, 1);
    let mut cx = Context::from_waker(noop_waker_ref());
    let mut old = Box::pin(paths.route(&members, q));
    let mut new = Box::pin(paths.route(&changed, q));
    assert!(old.as_mut().poll(&mut cx).is_pending());
    assert!(new.as_mut().poll(&mut cx).is_pending());
    assert_eq!(paths.active_searches(), 1);
    let expected_old = block_on(Paths::new(0).route(&members, q)).unwrap();
    assert_eq!(block_on(old).unwrap(), expected_old);
    assert_eq!(paths.active_searches(), 0);
    assert_eq!(block_on(new).unwrap()[1], favored);
}

#[test]
fn unreachable_releases_admission_and_cache_hits_ignore_overload() {
    let members = membership(10_000);
    let paths = Paths::with_limits(1, 1024 * 1024, 1);
    let cached = query(0, 9999, 4);
    let expected = block_on(paths.route(&members, cached)).unwrap();
    let mut pending = Box::pin(paths.route(&members, query(0, 9999, 5)));
    let mut cx = Context::from_waker(noop_waker_ref());
    assert!(pending.as_mut().poll(&mut cx).is_pending());
    let mut hit = Box::pin(paths.route(&members, cached));
    assert_eq!(hit.as_mut().poll(&mut cx), Poll::Ready(Ok(expected)));
    assert_eq!(paths.active_searches(), 1);
    drop(pending);
    assert_eq!(
        block_on(paths.route(&members, query(0, 9999, 1))),
        Err(Error::Unreachable)
    );
    assert_eq!(paths.active_searches(), 0);
    let blocked = members.neighbors(0);
    assert_eq!(
        block_on(paths.route(
            &members,
            PathQuery {
                blocked: &blocked,
                ..cached
            }
        )),
        Err(Error::Unreachable)
    );
    assert_eq!(paths.active_searches(), 0);
    assert_eq!(paths.cached_entries(), 1);
}

#[test]
fn full_u8_link_limit_reconstructs_without_overflow() {
    let members = membership(257);
    let mut graph = vec![vec![]; 257];
    for position in 1..257 {
        graph[position - 1].push(position);
        graph[position].push(position - 1);
    }
    let graph = Arc::new(graph);
    for (destination, reachable) in [(254, true), (255, true), (256, false)] {
        let key = PathKey::new(&members, &query(0, destination, 255)).unwrap();
        let mut search = EqualCostSearch::new(Arc::clone(&graph), &key);
        while !search.step(7).done {}
        if reachable {
            assert_eq!(
                search.finish().unwrap(),
                vec![(0..=destination).collect::<Vec<_>>()]
            );
        } else {
            assert_eq!(search.finish(), Err(Error::Unreachable));
        }
    }
}
