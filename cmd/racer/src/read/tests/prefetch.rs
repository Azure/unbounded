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
    let env = racer_io::Env::host();
    let signers = network(&env, 1);
    let mut f = fixture_with(&env, 2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
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
    let env = racer_io::Env::host();
    for mode in [CacheMode::PassthroughPeers, CacheMode::PassthroughLocal] {
        let signers = network(&env, 1);
        let mut f = fixture_with(&env, PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        f.set_mode(&env, mode);
        let membership = local_membership(&signers[0]);
        let (local, mut endpoint, _publication) =
            coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
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
    let env = racer_io::Env::host();
    let signers = network(&env, 1);
    let mut f = fixture_with(&env, PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
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
    let env = racer_io::Env::host();
    let signers = network(&env, 2);
    let mut f = fixture_with(&env, PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let retained = pooled_membership(&mut f, &signers, &[0]);
    let current = pooled_membership(&mut f, &signers, &[1]);
    assert!(retained.serves(&f.context.object.cache, signers[0].node()));
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &current, NoPeer::requester(&env));
    let context = crate::security::OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    for action in [
        crate::peer::protocol::PrefetchAction::Probe { pages: vec![0] },
        crate::peer::protocol::PrefetchAction::Fill {
            page_size: crate::model::PageSize::DEFAULT,
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

/// An owner refuses a fill whose pages were cut at another page size
/// than the cluster's, before probing or filling anything.
#[test]
fn owner_refuses_a_fill_cut_at_another_page_size() {
    let env = racer_io::Env::host();
    let signers = network(&env, 2);
    let mut f = fixture_with(&env, PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = pooled_membership(&mut f, &signers, &[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
    assert_eq!(local.page_size(), Ok(crate::model::PageSize::DEFAULT));
    let context = crate::security::OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let result = futures::executor::block_on(crate::prefetch::service::serve(
        &local,
        f.page.version.clone(),
        crate::peer::protocol::PrefetchAction::Fill {
            page_size: crate::model::PageSize::new(64 << 10).unwrap(),
            pages: vec![0],
            length: PAGE_BYTES + 7,
            content_type: None,
            priority: racer_core::prefetch::Priority::default(),
        },
        membership.clone(),
        &context,
        &f.scope,
    ));
    assert!(matches!(result, Err(Error::InvalidRequest)));
    endpoint.uninstall().unwrap();
}

/// Pages are placed only over the cache's pool. With this node the only
/// pool member, it owns every page, so no peer is asked.
#[test]
#[allow(clippy::disallowed_methods)] // Real disk writes need wall time.
fn put_ranks_owners_only_within_the_cache_pool() {
    let env = racer_io::Env::host();
    let signers = network(&env, 2);
    let mut f = fixture_with(&env, 4 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = pooled_membership(&mut f, &signers, &[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
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
    let env = racer_io::Env::host();
    let signers = network(&env, 2);
    let mut f = fixture_with(&env, PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = pooled_membership(&mut f, &signers, &[1]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
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
            page_size: crate::model::PageSize::DEFAULT,
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

/// Fill every page of the fixture object onto this node through a prefetch
/// ticket, and wait for it to settle.
#[allow(clippy::disallowed_methods)] // Real disk writes need wall time.
fn fill_all(
    f: &mut Fixture,
    endpoint: &mut crate::read::dispatch::WorkerEndpoint,
    local: &crate::read::Coordinator,
) {
    let body = put_body(f);
    let put = PrefetchRequest::Put {
        id: "fill".into(),
        content_length: body.len() as u64,
        authorization: None,
    };
    let mut status = json(&run(f, endpoint, local, put, body.as_bytes()));
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while std::time::Instant::now() < deadline && status["settled"] != true {
        pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
        status = json(&run(f, endpoint, local, get("fill"), b""));
    }
    assert_eq!(status["state"], "SUCCEEDED", "{status}");
}

fn page(version: &crate::model::ObjectVersion, number: u64) -> PageId {
    PageId {
        version: version.clone(),
        number: PageNumber(number),
    }
}

/// Drive a coordinator future while pumping the worker.
fn drive<T>(
    f: &mut Fixture,
    endpoint: &mut crate::read::dispatch::WorkerEndpoint,
    future: impl std::future::Future<Output = T>,
) -> T {
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut future = std::pin::pin!(future);
    for _ in 0..20_000 {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        pump_worker(endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    panic!("operation did not complete");
}

/// A peer `invalidate` drops the named version's pages and metadata from the
/// receiver's memory and disk index, leaves other versions alone, is
/// idempotent, and is never sent on to another node.
#[test]
fn peer_invalidate_drops_local_pages_and_metadata_without_propagating() {
    use crate::peer::protocol::PrefetchAction;
    use crate::telemetry::Event;
    let env = racer_io::Env::host();
    let signers = network(&env, 1);
    let mut f = fixture_with(&env, 2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
    fill_all(&mut f, &mut endpoint, &local);
    let version = f.page.version.clone();
    let index = f.fill.dependencies.writer.index().clone();
    let resident = |index: &crate::store::catalog::Index| {
        (0..3)
            .filter(|&n| index.lookup(&page(&version, n)).unwrap().is_some())
            .count()
    };
    assert_eq!(resident(&index), 3);
    assert!(index.version(&version).unwrap().is_some());
    let context = crate::security::OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let serve = |f: &mut Fixture,
                 endpoint: &mut crate::read::dispatch::WorkerEndpoint,
                 version: crate::model::ObjectVersion,
                 pages: Vec<u64>| {
        let scope = f.scope.clone();
        let membership = membership.clone();
        let context = &context;
        let local = &local;
        drive(f, endpoint, async move {
            crate::prefetch::service::serve(
                local,
                version,
                PrefetchAction::Invalidate { pages },
                membership,
                context,
                &scope,
            )
            .await
        })
    };
    // Another ETag of the same key is a no-op for this version.
    let mut other = version.clone();
    other.etag = crate::model::StrongEtag::test_value("other");
    let response = serve(&mut f, &mut endpoint, other, vec![0, 1, 2]).unwrap();
    assert!(matches!(response, PeerResponse::Prefetch(ref r) if r.len() == 3));
    assert_eq!(resident(&index), 3);
    // A subset drops only the named pages.
    serve(&mut f, &mut endpoint, version.clone(), vec![1]).unwrap();
    assert_eq!(resident(&index), 2);
    assert!(index.lookup(&page(&version, 1)).unwrap().is_none());
    // The rest, twice: idempotent.
    for _ in 0..2 {
        let response = serve(&mut f, &mut endpoint, version.clone(), vec![0, 1, 2]).unwrap();
        match response {
            PeerResponse::Prefetch(results) => {
                assert_eq!(results, vec![racer_core::prefetch::PageResult::Admitted; 3])
            }
            _ => panic!("prefetch results expected"),
        }
    }
    assert_eq!(resident(&index), 0);
    assert!(index.version(&version).unwrap().is_none());
    for n in 0..3 {
        assert!(
            f.fill
                .dependencies
                .memory
                .ciphertext(&page(&version, n))
                .unwrap()
                .is_none()
        );
    }
    let metrics = &local.fill.metrics;
    assert_eq!(metrics.count(Event::InvalidatePeerServed), 4);
    assert_eq!(metrics.count(Event::InvalidatePeerSent), 0);
    endpoint.uninstall().unwrap();
}

/// An adapter-persistence cache keeps pages in memory only; invalidation drops
/// them from memory, never touches disk, and still answers admitted.
#[test]
fn peer_invalidate_drops_adapter_pages_from_memory() {
    use crate::peer::protocol::PrefetchAction;
    let env = racer_io::Env::host();
    let signers = network(&env, 1);
    let mut f = fixture_with(&env, 2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    f.set_mode(&env, CacheMode::Adapter);
    let membership = local_membership(&signers[0]);
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, NoPeer::requester(&env));
    fill_all(&mut f, &mut endpoint, &local);
    let version = f.page.version.clone();
    let in_memory = |f: &Fixture| {
        (0..3)
            .filter(|&n| {
                f.fill
                    .dependencies
                    .memory
                    .ciphertext(&page(&version, n))
                    .unwrap()
                    .is_some()
            })
            .count()
    };
    assert_eq!(in_memory(&f), 3);
    let index = f.fill.dependencies.writer.index().clone();
    assert_eq!(f.fill.dependencies.writer.pending_count(), 0);
    assert!(index.lookup(&page(&version, 0)).unwrap().is_none());
    let context = crate::security::OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let scope = f.scope.clone();
    let response = drive(&mut f, &mut endpoint, {
        let local = &local;
        let context = &context;
        let version = version.clone();
        async move {
            crate::prefetch::service::serve(
                local,
                version,
                PrefetchAction::Invalidate {
                    pages: vec![0, 1, 2],
                },
                membership,
                context,
                &scope,
            )
            .await
        }
    })
    .unwrap();
    match response {
        PeerResponse::Prefetch(results) => {
            assert_eq!(results, vec![racer_core::prefetch::PageResult::Admitted; 3])
        }
        _ => panic!("prefetch results expected"),
    }
    assert_eq!(in_memory(&f), 0);
    assert_eq!(f.fill.dependencies.writer.pending_count(), 0);
    assert!(index.version(&version).unwrap().is_none());
    endpoint.uninstall().unwrap();
}

/// Peers that record each invalidation sent to them and never answer.
struct Unreachable {
    calls: std::cell::RefCell<Vec<(racer_wire::control::NodeId, Vec<u64>)>>,
}

impl Unreachable {
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &racer_wire::control::NodeId,
    ) -> bool {
        false
    }
    fn request<'a>(
        &'a self,
        request: crate::peer::protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a crate::runtime::RequestScope,
    ) -> crate::error::Operation<'a, crate::peer::forwarding::VerifiedResponse> {
        if let crate::peer::protocol::Operation::Prefetch {
            action: crate::peer::protocol::PrefetchAction::Invalidate { pages },
            ..
        } = &request.operation
        {
            self.calls
                .borrow_mut()
                .push((request.route.destination.clone(), pages.clone()));
        }
        Box::pin(async { Err(Error::Unavailable) })
    }
}

/// A client invalidation drops local state first and succeeds even when the
/// other ranked owners cannot be reached; the failures are counted.
#[test]
fn client_invalidate_succeeds_locally_when_peers_fail() {
    use crate::telemetry::Event;
    let env = racer_io::Env::host();
    let signers = network(&env, 2);
    let mut f = fixture_with(&env, 2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = pooled_membership(&mut f, &signers, &[0, 1]);
    let peers = Rc::new(Unreachable {
        calls: Default::default(),
    });
    let requester = crate::peer::Requester::scripted(
        env.ctx.clock.clone(),
        peers.clone(),
        Unreachable::direct_hedge_available,
        Unreachable::request,
        Unreachable::request,
    );
    let (local, mut endpoint, _publication) =
        coordinator(&env, &f, &signers[0], &membership, requester);
    let version = f.page.version.clone();
    let index = f.fill.dependencies.writer.index().clone();
    index
        .publish_version(crate::model::VersionMetadata {
            content_type: None,
            version: version.clone(),
            length: 2 * PAGE_BYTES + 7,
        })
        .unwrap();
    let scope = f.scope.clone();
    let outcome = drive(
        &mut f,
        &mut endpoint,
        local.invalidate(version.clone(), &scope),
    )
    .unwrap();
    // Both nodes rank among the three replicas of every page, so the peer is
    // named for all three pages in one operation, which fails.
    assert_eq!(outcome.pages, 3);
    assert!(!outcome.truncated);
    assert_eq!(outcome.sent, 1);
    assert_eq!(outcome.failed, 1);
    assert!(index.version(&version).unwrap().is_none());
    assert_eq!(
        *peers.calls.borrow(),
        vec![(signers[1].node().clone(), vec![0, 1, 2])]
    );
    assert_eq!(local.fill.metrics.count(Event::InvalidateRequest), 1);
    assert_eq!(local.fill.metrics.count(Event::InvalidatePeerFailed), 1);
    endpoint.uninstall().unwrap();
}
