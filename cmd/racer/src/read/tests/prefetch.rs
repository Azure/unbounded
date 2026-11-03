//! Prefetch tickets end to end on one node: the client handler, the dispatch
//! pump, the owner fill through the flight table, and normal persistence.
use super::fill::*;
use super::hot_reads::{coordinator, local_membership};
use crate::prefetch::service::{Response, handle};
use crate::test_support::security::network;
use racer_control_wire::CacheMode;
use racer_object_wire::prefetch::{PrefetchRequest, StatusQuery};

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
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
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
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
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
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
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
