//! End-to-end tests that use only the public API.

use futures::{executor::block_on, task::noop_waker_ref};

use std::{future::Future, num::NonZeroU32, task::Context};

use topology::{Error, Maintenance, Member, Membership, PathQuery, Paths, Placement};

/// Test member with a 2-byte ID.
#[derive(Clone, Debug)]
struct Node([u8; 2]);

impl Member for Node {
    const DOMAIN: &'static str = "workflow-store";

    /// Return the ID.
    fn id(&self) -> &[u8] {
        &self.0
    }

    /// Every member has weight 1.
    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::new(1).unwrap()
    }
}

/// Build a membership, passing members in reverse order.
fn members(ids: std::ops::Range<u16>) -> Membership<Node> {
    // Discovery order is intentionally different from membership position order.
    Membership::new(ids.rev().map(|id| Node(id.to_be_bytes())).collect()).unwrap()
}

/// Check that a route is valid: right ends, within limits, no loops,
/// respects filters, and follows real graph edges.
fn assert_route<M: Member>(members: &Membership<M>, query: PathQuery<'_>, route: &[usize]) {
    assert_eq!(route.first(), Some(&query.from));
    assert_eq!(route.last(), Some(&query.to));
    assert!(route.len() <= usize::from(query.links) + 1);
    for (index, &node) in route.iter().enumerate() {
        assert!(!query.visited.contains(&node));
        assert!(!route[..index].contains(&node), "route must not loop");
    }
    for edge in route.windows(2) {
        assert!(members.neighbors(edge[0]).contains(&edge[1]));
    }
    if route.len() > 1 {
        assert!(!query.blocked.contains(&route[1]));
    }
}

/// Route to a key's owners and retry around failures at each hop.
#[test]
fn route_to_a_replica_forward_without_loops_and_recover_from_blocked_links() {
    let members = members(0..1000);
    let placement = Placement::new(1);
    let key = b"objects/hello\0page-0";
    let replicas = block_on(placement.rank_async(&members, key)).unwrap();
    assert_eq!(replicas.len(), 3);
    for (index, &replica) in replicas.iter().enumerate() {
        assert!(!replicas[..index].contains(&replica));
        assert_eq!(
            members.position(members.members()[replica].id()),
            Some(replica)
        );
    }
    assert_eq!(placement.rank(&members, key).unwrap(), replicas);

    let to = replicas[0];
    let from = (0..members.members().len())
        .find(|&node| node != to && !members.neighbors(node).contains(&to))
        .unwrap();
    let paths = Paths::new(2);
    let query = PathQuery {
        from,
        to,
        links: 4,
        visited: &[],
        blocked: &[],
        seed: key,
    };
    let original = block_on(paths.route(&members, query)).unwrap();
    assert_route(&members, query, &original);
    assert!(original.len() > 2, "exercise forwarding, not a direct edge");

    // A failed first link requires a new route, not a change in object ownership.
    let failed = [original[1]];
    let retry = PathQuery {
        blocked: &failed,
        ..query
    };
    let alternate = block_on(paths.route(&members, retry)).unwrap();
    assert_route(&members, retry, &alternate);
    assert_ne!(alternate[1], original[1]);
    assert_eq!(placement.rank(&members, key).unwrap(), replicas);

    // Each relay recomputes using its own source and the remaining hop budget.
    let mut visited = vec![from];
    let mut current = alternate[1];
    let mut links = query.links - 1;
    while current != to {
        assert!(
            links > 0,
            "forwarding must finish within the original budget"
        );
        let forwarded = PathQuery {
            from: current,
            links,
            visited: &visited,
            ..query
        };
        let route = block_on(paths.route(&members, forwarded)).unwrap();
        assert_route(&members, forwarded, &route);
        visited.push(current);
        current = route[1];
        links -= 1;
    }
    assert_eq!(
        block_on(paths.route(
            &members,
            PathQuery {
                from: to,
                links: 0,
                ..query
            }
        )),
        Ok(vec![to])
    );

    let blocked = members.neighbors(from);
    assert_eq!(
        block_on(paths.route(
            &members,
            PathQuery {
                blocked: &blocked,
                ..query
            }
        )),
        Err(Error::Unreachable)
    );
    assert_eq!(
        block_on(paths.route(&members, PathQuery { links: 1, ..query })),
        Err(Error::Unreachable)
    );
    // Bad forwarded state is rejected even after successful queries warmed caches.
    assert_eq!(
        block_on(paths.route(
            &members,
            PathQuery {
                visited: &[from],
                ..query
            }
        )),
        Err(Error::InvalidQuery)
    );
    assert_eq!(block_on(paths.route(&members, query)).unwrap(), original);
}

/// Swap memberships during async placement and recover after cancellation.
#[test]
fn replace_membership_during_pending_placement_then_fall_back_for_large_changes() {
    let old = members(0..600);
    let key = b"objects/snapshot";
    let oracle = Placement::new(0);
    let expected_old = oracle.rank(&old, key).unwrap();
    let removed_id = old.members()[expected_old[0]].0;
    let mut replacement = old.members().to_vec();
    replacement.remove(expected_old[0]);
    replacement.push(Node(600u16.to_be_bytes()));
    let next = Membership::new(replacement).unwrap().with_predecessor(&old);
    assert_ne!(old.identity(), next.identity());
    assert_eq!(next.position(&removed_id), None);

    let placement = Placement::new(1);
    let mut cx = Context::from_waker(noop_waker_ref());
    let mut pending = Box::pin(placement.rank_async(&old, key));
    assert!(pending.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        block_on(placement.rank_async(&next, key)),
        Err(Error::Overloaded)
    );
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));

    // Dropping the old request frees admission. Retry the interrupted maintenance
    // before requesting a rank; an incomplete predecessor must never be reused.
    drop(pending);
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    let ranked = block_on(placement.rank_async(&next, key)).unwrap();
    assert_eq!(ranked, oracle.rank(&next, key).unwrap());
    assert_eq!(ranked.len(), 3);
    assert!(
        ranked
            .iter()
            .all(|&index| next.members()[index].0 != removed_id)
    );
    assert_eq!(placement.rank(&old, key).unwrap(), expected_old);
    assert_eq!(placement.rank(&next, key).unwrap(), ranked);

    // Replacing every member exceeds the bounded predecessor hint. The public
    // workflow must still yield cooperatively and produce an exact cold ranking.
    let fresh = members(1000..1600).with_predecessor(&next);
    placement.maintain(&fresh).unwrap();
    let mut pending = Box::pin(placement.rank_async(&fresh, key));
    assert!(pending.as_mut().poll(&mut cx).is_pending());
    let fresh_ranked = block_on(pending).unwrap();
    assert_eq!(fresh_ranked, oracle.rank(&fresh, key).unwrap());
    assert!(
        fresh_ranked
            .iter()
            .all(|&index| { next.position(fresh.members()[index].id()).is_none() })
    );
    // Returning to a retained snapshot after eviction is still safe.
    assert_eq!(placement.rank(&old, key).unwrap(), expected_old);
}

/// Test member whose weight can differ between memberships.
#[derive(Clone)]
struct WeightedNode {
    id: Vec<u8>,

    weight: NonZeroU32,
}

impl Member for WeightedNode {
    const DOMAIN: &'static str = "racer";

    /// Return the ID.
    fn id(&self) -> &[u8] {
        &self.id
    }

    /// Return the weight.
    fn weight(&self) -> NonZeroU32 {
        self.weight
    }
}

/// Memberships that differ only in weight share searches; canceled callers
/// free their slot; cache hits pick again.
#[test]
fn shared_routes_survive_cancellation_and_weight_only_successors() {
    let old = Membership::new(
        (0..10_000)
            .map(|i| WeightedNode {
                id: format!("node-{i:06}").into_bytes(),
                weight: NonZeroU32::new(4).unwrap(),
            })
            .collect(),
    )
    .unwrap();
    let query = PathQuery {
        from: 0,
        to: 9999,
        links: 4,
        visited: &[],
        blocked: &[],
        seed: b"successor-workflow",
    };
    let oracle = Paths::new(0);
    let expected_old = block_on(oracle.route(&old, query)).unwrap();
    let alternate = block_on(oracle.route(
        &old,
        PathQuery {
            blocked: &[expected_old[1]],
            ..query
        },
    ))
    .unwrap();
    assert_eq!(alternate.len(), expected_old.len());
    let favored = alternate[1];
    let mut records = old.members().to_vec();
    records[favored].weight = NonZeroU32::new(u32::MAX).unwrap();
    let next = Membership::new_with_predecessor(records, &old).unwrap();
    assert_ne!(next.identity(), old.identity());
    let next_query = PathQuery {
        seed: b"successor-reselection",
        ..query
    };
    let expected_next = block_on(oracle.route(&next, next_query)).unwrap();
    assert_eq!(expected_next[1], favored);
    assert_ne!(expected_next[1], expected_old[1]);

    let paths = Paths::with_limits(1, 1024 * 1024, 1);
    let mut cx = Context::from_waker(noop_waker_ref());
    let mut canceled = Box::pin(paths.route(&old, query));
    let mut successor = Box::pin(paths.route(&next, next_query));
    assert!(canceled.as_mut().poll(&mut cx).is_pending());
    assert!(successor.as_mut().poll(&mut cx).is_pending());
    assert_eq!(paths.active_searches(), 1);
    assert_eq!(
        block_on(paths.route(&old, PathQuery { links: 5, ..query })),
        Err(Error::Overloaded)
    );
    drop(canceled);
    assert_eq!(paths.active_searches(), 1);
    let route = block_on(successor).unwrap();
    assert_eq!(route, expected_next);
    assert_route(&next, next_query, &route);
    assert_eq!(paths.active_searches(), 0);
    assert_eq!(paths.cached_entries(), 1);
    let bytes = paths.cached_bytes();
    assert!(bytes > 0);

    // The same retained alternatives serve the old weights without another poll.
    let mut old_hit = Box::pin(paths.route(&old, query));
    assert_eq!(
        old_hit.as_mut().poll(&mut cx),
        std::task::Poll::Ready(Ok(expected_old))
    );
    assert_eq!(paths.cached_entries(), 1);
    assert_eq!(paths.cached_bytes(), bytes);
    assert_eq!(paths.active_searches(), 0);

    // Canceling every waiter frees admission without disturbing the warm entry.
    let cold = PathQuery { links: 5, ..query };
    let mut first = Box::pin(paths.route(&old, cold));
    let mut second = Box::pin(paths.route(&next, cold));
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    drop(second);
    assert_eq!(paths.active_searches(), 1);
    drop(first);
    assert_eq!(paths.active_searches(), 0);
    assert_eq!(paths.cached_bytes(), bytes);
    assert_eq!(
        block_on(paths.route(&next, next_query)).unwrap(),
        expected_next
    );
}
