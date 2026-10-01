//! Current routing contract, independent BFS oracle, and resource bounds.
use super::*;
use crate::topology::{fixtures::membership, health::LinkOutcome};
use std::time::Duration;

fn budget(members: &MembershipLease, to: usize, links: u8) -> RouteBudget {
    RouteBudget {
        membership: members.version,
        request: RequestId([1; 16]),
        attempt: AttemptId([2; 16]),
        destination: members.members()[to].node.clone(),
        visited: vec![],
        remaining_links: links,
        remaining_attempts: 3,
        deadline: Deadline(Instant::now() + Duration::from_secs(60)),
    }
}

// Build both edge directions from the definition, independently of the inverse
// neighbor implementation used by production search.
fn graph(n: usize) -> Vec<Vec<usize>> {
    let mut edges = vec![vec![]; n];
    for i in 0..n {
        for digit in 0..32 {
            let j = (32 * i + digit) % n;
            if i != j {
                edges[i].push(j);
                edges[j].push(i);
            }
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
                || (node == key.from && key.failed.contains(&next))
                || (next == key.from && key.failed.contains(&node))
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

#[test]
fn all_equal_next_hops_match_independent_oracle() {
    for n in [37, 401, 1500] {
        let edges = graph(n);
        let members = membership(n);
        let paths = Paths::new(Rc::new(LinkHealth), 4);
        for source in [0, 19, n - 1] {
            for to in (0..n).step_by(17).filter(|&to| to != source) {
                let request = budget(&members, to, NORMAL_LINKS);
                for filtered in [false, true] {
                    let mut key = paths
                        .key(&members, &members.members()[source].node, &request)
                        .unwrap();
                    if filtered {
                        key.visited = [7, 33]
                            .into_iter()
                            .filter(|v| *v != source && *v != to)
                            .collect();
                        key.failed = edges[source].iter().copied().step_by(2).collect();
                    }
                    let distance = distances(&edges, &key);
                    for links in [1, 2, 4] {
                        key.links = links;
                        let mut search = EqualCostSearch::new(n, &key);
                        while !search.step(1, request.deadline).unwrap() {}
                        let result = search.finish();
                        if distance[source] > links as usize {
                            assert_eq!(result, Err(Error::Unavailable));
                            continue;
                        }
                        let alternatives = result.unwrap();
                        let expected: Vec<_> = edges[source]
                            .iter()
                            .copied()
                            .filter(|v| {
                                !key.failed.contains(v)
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
        member.shares = std::num::NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
    }
    let members =
        Arc::new(Membership::validate(crate::model::MembershipVersion(1), input).unwrap());
    let cached = Paths::new(Rc::new(LinkHealth), 1);
    let cold = Paths::new(Rc::new(LinkHealth), 0);
    for (attempt, expected) in super::super::fixtures::V5_NEXT_HOPS {
        let mut request = budget(&members, 1499, NORMAL_LINKS);
        request.attempt = AttemptId(attempt.to_be_bytes());
        let from = &members.members()[0].node;
        let actual = cached.shortest(members.clone(), from, &request).unwrap();
        assert_eq!(actual.nodes[1], members.members()[expected].node);
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        assert_eq!(
            actual.nodes,
            futures::executor::block_on(cold.shortest_async(
                members.clone(),
                from,
                &request,
                &scope
            ))
            .unwrap()
            .nodes
        );
    }
    let mut changed = members.members().to_vec();
    changed[1312].shares = std::num::NonZeroU32::new(u32::MAX).unwrap();
    let changed =
        Arc::new(Membership::validate(crate::model::MembershipVersion(2), changed).unwrap());
    let route = cached
        .shortest(
            changed.clone(),
            &changed.members()[0].node,
            &budget(&changed, 1499, 4),
        )
        .unwrap();
    assert_eq!(route.nodes[1], changed.members()[1312].node);
    assert_eq!(cached.cached_paths(), 1);
}

#[test]
fn last_first_hop_bit_survives_search_and_reconstruction() {
    let n = 100_000;
    let members = membership(n);
    let source = 19;
    let neighbors = neighbor_positions_for(n, source);
    assert_eq!(neighbors.len(), 64);
    let last = neighbors[63];
    let paths = Paths::new(Rc::new(LinkHealth), 0);
    let request = budget(&members, last, NORMAL_LINKS);
    let mut key = paths
        .key(&members, &members.members()[source].node, &request)
        .unwrap();
    key.failed = neighbors[..63].to_vec();
    let next = neighbor_positions_for(n, last)
        .into_iter()
        .find(|v| *v != source && !neighbors.contains(v))
        .unwrap();
    for to in [last, next] {
        key.to = to;
        let mut search = EqualCostSearch::new(n, &key);
        while !search.step(1, request.deadline).unwrap() {}
        let alternatives = search.finish().unwrap();
        assert_eq!(alternatives.len(), 1);
        assert_eq!(alternatives[0][1], last);
        assert_eq!(alternatives[0].last(), Some(&to));
    }
    key.failed = neighbors;
    let mut search = EqualCostSearch::new(n, &key);
    while !search.step(1, request.deadline).unwrap() {}
    assert_eq!(search.finish(), Err(Error::Unavailable));
}

#[test]
fn health_eviction_budget_deadline_and_cancellation() {
    let members = membership(1500);
    let source = &members.members()[0].node;
    let health = Rc::new(LinkHealth);
    let paths = Paths::new(health.clone(), 1);
    let request = budget(&members, 1499, NORMAL_LINKS);
    let original = paths.shortest(members.clone(), source, &request).unwrap();
    let mut blocked = request.clone();
    blocked.visited.push(original.nodes[1].clone());
    let alternate = paths.shortest(members.clone(), source, &blocked).unwrap();
    assert_eq!(alternate.nodes.len(), original.nodes.len());
    assert!(!alternate.nodes.contains(&original.nodes[1]));
    assert_eq!(paths.cached_paths(), 1);
    health
        .observe_at(
            &original.nodes[1],
            LinkOutcome::Timeout,
            Instant::now() + Duration::from_secs(60),
        )
        .unwrap();
    assert_ne!(
        paths
            .shortest(members.clone(), source, &request)
            .unwrap()
            .nodes[1],
        original.nodes[1]
    );
    for neighbor in Graph::new(members.clone()).neighbors(source).unwrap() {
        health
            .observe_at(
                &neighbor,
                LinkOutcome::Timeout,
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
    }
    assert_eq!(
        paths
            .shortest(members.clone(), source, &request)
            .unwrap_err(),
        Error::Unavailable
    );
    blocked = request.clone();
    blocked.remaining_links = 0;
    assert_eq!(
        paths
            .shortest(members.clone(), source, &blocked)
            .unwrap_err(),
        Error::HopBudgetExhausted
    );
    blocked = request.clone();
    blocked.visited.push(source.clone());
    assert_eq!(
        paths
            .shortest(members.clone(), source, &blocked)
            .unwrap_err(),
        Error::InvalidRequest
    );
    blocked = request;
    blocked.deadline = Deadline(Instant::now());
    assert_eq!(
        paths
            .shortest(members.clone(), source, &blocked)
            .unwrap_err(),
        Error::DeadlineExceeded
    );
    let cold = Paths::new(Rc::new(LinkHealth), 0);
    assert_eq!(
        cold.shortest(members.clone(), source, &budget(&members, 0, 0))
            .unwrap()
            .nodes,
        vec![source.clone()]
    );
    let request = budget(&members, 1499, 4);
    let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
    let mut operation = cold.shortest_async(members.clone(), source, &request, &scope);
    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
    let _ = operation.as_mut().poll(&mut cx);
    drop(operation);
    assert_eq!(cold.active_searches.get(), 0);
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
    let paths = Paths::new(Rc::new(LinkHealth), 0);
    let request = budget(&members, 80_003, 4);
    let key = paths
        .key(&members, &members.members()[0].node, &request)
        .unwrap();
    let mut search = EqualCostSearch::new(100_000, &key);
    loop {
        let before = search.expansions;
        let done = search.step(7, request.deadline).unwrap();
        assert!(search.expansions - before <= 7);
        assert!(search.edges <= search.expansions * MAX_DEGREE);
        assert!(search.visited_entries() <= 2 * 100_000);
        if done {
            break;
        }
    }
    let alternatives = search.finish().unwrap();
    assert!(alternatives.len() <= MAX_DEGREE);
    assert!(alternatives.iter().all(|p| p.len() <= 5));
}

#[test]
fn forwarded_budget_is_monotonic_and_rejects_cycles() {
    let members = membership(100);
    let request = budget(&members, 99, 4);
    let from = &members.members()[0].node;
    let next = &members.members()[1].node;
    let forwarded = request.forwarded(from, next).unwrap();
    request.validate_forwarded(&forwarded, from, next).unwrap();
    assert_eq!(forwarded.remaining_links, 3);
    assert_eq!(forwarded.remaining_attempts, request.remaining_attempts);
    assert!(forwarded.forwarded(next, from).is_err());
    let mut invalid = forwarded.clone();
    invalid.remaining_attempts += 1;
    assert!(request.validate_forwarded(&invalid, from, next).is_err());
    invalid = forwarded;
    invalid.deadline.0 += Duration::from_secs(1);
    assert!(request.validate_forwarded(&invalid, from, next).is_err());
}
