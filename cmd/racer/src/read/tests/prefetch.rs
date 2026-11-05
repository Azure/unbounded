//! Prefetch tickets end to end on one node: the client handler, the dispatch
//! pump, the owner fill through the flight table, and normal persistence.
use super::fill::*;
use super::hot_reads::{coordinator, local_membership};
use crate::prefetch::service::{Response, handle};
use crate::test_support::security::network;
use racer_wire::control::CacheMode;
use racer_wire::object::prefetch::{PrefetchRequest, StatusQuery};

fn status_code(response: &Response) -> u16 {
    match response.head.start {
        http1::StartLine::Response { status } => status,
        _ => panic!("response expected"),
    }
}

fn json(response: &Response) -> serde_json::Value {
    serde_json::from_slice(&response.body).unwrap()
}

fn run(
    f: &mut Fixture,
    endpoint: &mut crate::read::dispatch::WorkerEndpoint,
    local: &crate::read::Coordinator,
    request: PrefetchRequest<'_>,
    body: &[u8],
) -> Response {
    let cache = f.context.object.cache.clone();
    let scope = f.scope.clone();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut future = Box::pin(handle(local, &cache, request, body, &scope));
    for _ in 0..20_000 {
        if let Poll::Ready(response) = future.as_mut().poll(&mut cx) {
            return response;
        }
        pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    panic!("prefetch request did not complete");
}

fn put_body(f: &Fixture) -> String {
    format!(
        r#"{{"refs":[{{"key":"{}","etag":{}}}]}}"#,
        f.context.object.key.to_hex(),
        serde_json::to_string(f.page.version.etag.as_str()).unwrap()
    )
}

fn get(id: &str) -> PrefetchRequest<'static> {
    PrefetchRequest::Get {
        id: id.into(),
        query: StatusQuery::default(),
    }
}

#[test]
#[allow(clippy::disallowed_methods)] // Real disk writes need wall time.
fn put_fills_every_page_through_the_owner_and_reports_success() {
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    let mut f = fixture_with(2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let body = put_body(&f);
    let put = |id: &str| PrefetchRequest::Put {
        id: id.into(),
        content_length: body.len() as u64,
        authorization: None,
    };
    let response = run(&mut f, &mut endpoint, &local, put("one"), body.as_bytes());
    assert_eq!(status_code(&response), 201, "{:?}", json(&response));
    let mut status = json(&response);
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while std::time::Instant::now() < deadline {
        if status["settled"] == true {
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        status = json(&run(&mut f, &mut endpoint, &local, get("one"), b""));
    }
    assert_eq!(status["state"], "SUCCEEDED", "{status}");
    assert_eq!(status["refs_succeeded"], 1);
    assert_eq!(status["bytes"]["admitted"], 3 * PAGE_BYTES);
    assert_eq!(status["bytes"]["fetched_from_origin"], 3 * PAGE_BYTES);
    // Pages are persisted normally: on disk and not held.
    let index = f.fill.dependencies.writer.index().clone();
    for number in 0..3 {
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        assert!(index.lookup(&page).unwrap().is_some(), "page {number}");
        assert!(!index.is_held(&page), "page {number}");
    }
    // A second ticket for the same pages finds them resident.
    let response = run(&mut f, &mut endpoint, &local, put("two"), body.as_bytes());
    assert_eq!(status_code(&response), 201);
    let mut status = json(&response);
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while std::time::Instant::now() < deadline {
        if status["settled"] == true {
            break;
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        status = json(&run(&mut f, &mut endpoint, &local, get("two"), b""));
    }
    assert_eq!(status["state"], "SUCCEEDED", "{status}");
    assert_eq!(status["bytes"]["already_resident"], 3 * PAGE_BYTES);
    // Coverage probes the owner.
    let response = run(
        &mut f,
        &mut endpoint,
        &local,
        PrefetchRequest::Get {
            id: "two".into(),
            query: StatusQuery {
                coverage: true,
                ..StatusQuery::default()
            },
        },
        b"",
    );
    assert_eq!(
        json(&response)["coverage"]["resident_bytes"],
        3 * PAGE_BYTES
    );
    // With no request budget left for probing, for example after a long-poll
    // wait or a slow owner, the status is still answered without probing.
    let request = f.scope.clone();
    f.scope =
        crate::runtime::RequestScope::host(f.scope.request, racer_io::Clock::host().now()).unwrap();
    let response = run(
        &mut f,
        &mut endpoint,
        &local,
        PrefetchRequest::Get {
            id: "two".into(),
            query: StatusQuery {
                coverage: true,
                ..StatusQuery::default()
            },
        },
        b"",
    );
    f.scope = request;
    assert_eq!(status_code(&response), 200);
    assert_eq!(json(&response)["state"], "SUCCEEDED");
    assert_eq!(json(&response)["coverage"]["resident_bytes"], 0);
    // List and delete.
    let list = json(&run(
        &mut f,
        &mut endpoint,
        &local,
        PrefetchRequest::List,
        b"",
    ));
    assert_eq!(list["prefetches"].as_array().unwrap().len(), 2);
    let deleted = run(
        &mut f,
        &mut endpoint,
        &local,
        PrefetchRequest::Delete { id: "one".into() },
        b"",
    );
    assert_eq!(status_code(&deleted), 200);
    assert_eq!(json(&deleted)["state"], "CANCELLED");
    assert_eq!(json(&deleted)["reason"], "client");
    let missing = run(&mut f, &mut endpoint, &local, get("nope"), b"");
    assert_eq!(status_code(&missing), 404);
    assert_eq!(json(&missing)["code"], "not_found");
    for _ in 0..256 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn put_to_a_passthrough_cache_is_409_passthrough() {
    for mode in [CacheMode::PassthroughPeers, CacheMode::PassthroughLocal] {
        let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let signers = network(1);
        let mut f = fixture_with(PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        f.set_mode(mode);
        let membership = local_membership(&signers[0]);
        let (local, mut endpoint, _publication) =
            coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let body = put_body(&f);
        let response = run(
            &mut f,
            &mut endpoint,
            &local,
            PrefetchRequest::Put {
                id: "t".into(),
                content_length: body.len() as u64,
                authorization: None,
            },
            body.as_bytes(),
        );
        assert_eq!(status_code(&response), 409, "{mode:?}");
        assert_eq!(json(&response)["code"], "passthrough");
        endpoint.uninstall().unwrap();
    }
}

#[test]
fn bad_bodies_and_draining_nodes_get_json_errors() {
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(1);
    let mut f = fixture_with(PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let response = run(
        &mut f,
        &mut endpoint,
        &local,
        PrefetchRequest::Put {
            id: "t".into(),
            content_length: 2,
            authorization: None,
        },
        b"{}",
    );
    assert_eq!(status_code(&response), 400);
    assert_eq!(json(&response)["code"], "invalid_request");
    local.prefetch_directory().prefetch.stop();
    let response = run(&mut f, &mut endpoint, &local, PrefetchRequest::List, b"");
    assert_eq!(status_code(&response), 503);
    assert_eq!(json(&response)["code"], "unavailable");
    endpoint.uninstall().unwrap();
}

/// Two members where the fixture cache's selector pool holds only `pool`.
/// Publishes the same pools from `f`.
fn pooled_membership(
    f: &mut Fixture,
    signers: &[Rc<crate::peer::protocol::Signatures>],
    pool: &[usize],
) -> std::sync::Arc<crate::topology::Membership> {
    use crate::topology::Member;
    use racer_wire::control::{MembershipVersion, Pool, PoolId};
    let mut nodes: Vec<_> = signers.iter().map(|s| s.node().clone()).collect();
    nodes.sort();
    let members = nodes
        .iter()
        .map(|node| Member {
            node: node.clone(),
            shares: std::num::NonZeroU32::new(1).unwrap(),
            peer_endpoint: "127.0.0.1:8000".into(),
            rails: vec![],
            site: String::new(),
        })
        .collect();
    let mut selected: Vec<u32> = pool
        .iter()
        .map(|&i| nodes.iter().position(|n| n == signers[i].node()).unwrap() as u32)
        .collect();
    selected.sort();
    let pools = vec![Pool {
        id: PoolId("sel-0123456789abcdef0123456789abcdef".into()),
        rank: 1 << 32,
        members: Some(selected),
        caches: vec![f.context.object.cache.clone()],
    }];
    f.pools = Some(pools.clone());
    std::sync::Arc::new(
        crate::topology::Membership::validate_with_pools(
            MembershipVersion(1),
            members,
            pools,
            None,
        )
        .unwrap(),
    )
}

/// A lagging sender that still carries a retained membership in which this
/// node was a pool member is refused once the current membership moved this
/// node out of the cache's pool.
#[test]
fn a_node_that_left_the_cache_pool_refuses_retained_membership_requests() {
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(2);
    let mut f = fixture_with(PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let retained = pooled_membership(&mut f, &signers, &[0]);
    let current = pooled_membership(&mut f, &signers, &[1]);
    assert!(retained.serves(&f.context.object.cache, signers[0].node()));
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &current, NoPeer::requester());
    let context = crate::security::OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    for action in [
        crate::peer::protocol::PrefetchAction::Probe { pages: vec![0] },
        crate::peer::protocol::PrefetchAction::Fill {
            pages: vec![0],
            length: PAGE_BYTES + 7,
            content_type: None,
            priority: racer_core::prefetch::Priority::default(),
        },
    ] {
        let response = futures::executor::block_on(crate::prefetch::service::serve(
            &local,
            f.page.version.clone(),
            action,
            retained.clone(),
            &context,
            &f.scope,
        ))
        .unwrap();
        assert!(matches!(response, PeerResponse::StaleMembership));
    }
    endpoint.uninstall().unwrap();
}

/// Pages are placed only over the cache's pool. With this node the only
/// pool member, it owns every page, so no peer is asked.
#[test]
#[allow(clippy::disallowed_methods)] // Real disk writes need wall time.
fn put_ranks_owners_only_within_the_cache_pool() {
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(2);
    let mut f = fixture_with(4 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = pooled_membership(&mut f, &signers, &[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let body = put_body(&f);
    let put = PrefetchRequest::Put {
        id: "t".into(),
        content_length: body.len() as u64,
        authorization: None,
    };
    let response = run(&mut f, &mut endpoint, &local, put, body.as_bytes());
    assert_eq!(status_code(&response), 201, "{:?}", json(&response));
    let mut status = json(&response);
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while std::time::Instant::now() < deadline && status["settled"] != true {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        status = json(&run(&mut f, &mut endpoint, &local, get("t"), b""));
    }
    assert_eq!(status["state"], "SUCCEEDED", "{status}");
    assert_eq!(status["bytes"]["fetched_from_origin"], 5 * PAGE_BYTES);
    for _ in 0..256 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}

/// A node outside the cache's pool neither accepts tickets nor fills pages
/// for peers: a peer that ranked it as owner used a stale membership.
#[test]
fn a_node_outside_the_cache_pool_refuses_tickets_and_fills() {
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let signers = network(2);
    let mut f = fixture_with(PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = pooled_membership(&mut f, &signers, &[1]);
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, NoPeer::requester());
    let body = put_body(&f);
    let put = PrefetchRequest::Put {
        id: "t".into(),
        content_length: body.len() as u64,
        authorization: None,
    };
    let response = run(&mut f, &mut endpoint, &local, put, body.as_bytes());
    assert_eq!(status_code(&response), 503);
    assert_eq!(json(&response)["code"], "unavailable");
    let context = crate::security::OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    for action in [
        crate::peer::protocol::PrefetchAction::Probe { pages: vec![0] },
        crate::peer::protocol::PrefetchAction::Fill {
            pages: vec![0],
            length: PAGE_BYTES + 7,
            content_type: None,
            priority: racer_core::prefetch::Priority::default(),
        },
    ] {
        let response = futures::executor::block_on(crate::prefetch::service::serve(
            &local,
            f.page.version.clone(),
            action,
            membership.clone(),
            &context,
            &f.scope,
        ))
        .unwrap();
        assert!(matches!(response, PeerResponse::StaleMembership));
    }
    endpoint.uninstall().unwrap();
}
