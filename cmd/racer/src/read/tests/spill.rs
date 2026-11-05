//! Read spill: pages past the window parked on local disk under reader holds.
use super::fill::*;
use super::hot_reads::coordinator;
use crate::client::{ClientRequest, ReadKind};
use crate::read::ReadPayload;
use crate::test_support::security::{network, node};

/// Open a single-node read graph with `range_spill_pages` enabled and a
/// one-page client credit, so every page past the head is a spill candidate.
fn spill_reader(
    env: &racer_io::Env,
    pages: u64,
    spill: usize,
) -> (
    Fixture,
    Rc<crate::read::Coordinator>,
    crate::read::dispatch::WorkerEndpoint,
    crate::read::range_stream::RangeStream,
) {
    let signers = network(env, 1);
    let f = fixture_with(env, pages * PAGE_BYTES, None);
    f.reactor.init().unwrap();
    let membership = Arc::new(
        Membership::validate(
            racer_wire::control::MembershipVersion(1),
            vec![Member {
                node: node(0),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8200".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let (local, endpoint, _) =
        coordinator(env, &f, &signers[0], &membership, NoPeer::requester(env));
    f.fill
        .dependencies
        .metadata_owner
        .configure_spill(crate::read::spill::Config {
            range_spill_pages: spill,
            ..Default::default()
        })
        .unwrap();
    let response = futures::executor::block_on(local.read(
        ClientRequest {
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: None,
                page_credits: 1,
                byte_credits: PAGE_BYTES,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        },
        &f.scope,
    ))
    .unwrap();
    let ReadPayload::Stream(stream) = response.payload else {
        panic!("body expected")
    };
    (f, local, endpoint, stream)
}

fn holders(f: &Fixture, number: u64) -> u32 {
    f.fill.dependencies.writer.index().hold_holders(&PageId {
        version: f.page.version.clone(),
        number: PageNumber(number),
    })
}

#[test]
fn read_spill_holds_pages_past_the_window_and_releases_each_after_delivery() {
    let env = racer_io::Env::host();
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let (mut f, _local, mut endpoint, mut stream) = spill_reader(&env, 4, 2);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    // Without delivering, the window holds page 0 and spill parks 1 and 2.
    for _ in 0..4096 {
        stream.poll_prefetch(&mut cx);
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        if holders(&f, 1) == 1 && holders(&f, 2) == 1 && f.origin.started_pages.borrow().len() == 3
        {
            break;
        }
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    for _ in 0..64 {
        stream.poll_prefetch(&mut cx);
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert_eq!((holders(&f, 1), holders(&f, 2), holders(&f, 3)), (1, 1, 0));
    assert_eq!(holders(&f, 0), 0, "the in-memory window takes no hold");
    let mut started = f.origin.started_pages.borrow().clone();
    started.sort_unstable();
    assert_eq!(started, vec![0, 1, 2]);
    // Deliver every page in order; each spilled page loses this read's holder
    // once delivered, and no page is fetched twice.
    for number in 0..4 {
        let mut reader = None;
        for _ in 0..4096 {
            if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                reader = result.unwrap();
                break;
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        let reader = reader.expect("page delivered");
        assert_eq!(reader.slice().page, PageNumber(number));
        assert!(reader.slice().length > 0);
        drop(reader);
        stream
            .release_page(PageNumber(number), PAGE_BYTES as u32)
            .unwrap();
        for _ in 0..8 {
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
        assert_eq!(holders(&f, number), 0, "page {number}");
    }
    assert!(
        futures::executor::block_on(stream.next_slice())
            .unwrap()
            .is_none()
    );
    let mut started = f.origin.started_pages.borrow().clone();
    started.sort_unstable();
    assert_eq!(started, vec![0, 1, 2, 3]);
    drop(stream);
    for _ in 0..64 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!((0..4).all(|number| holders(&f, number) == 0));
    endpoint.uninstall().unwrap();
}

#[test]
fn read_spill_cancel_releases_every_hold() {
    let env = racer_io::Env::host();
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let (mut f, _local, mut endpoint, mut stream) = spill_reader(&env, 4, 2);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..4096 {
        stream.poll_prefetch(&mut cx);
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        if holders(&f, 1) == 1 && holders(&f, 2) == 1 {
            break;
        }
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!((holders(&f, 1), holders(&f, 2)), (1, 1));
    futures::executor::block_on(stream.cancel()).unwrap();
    drop(stream);
    for _ in 0..256 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    assert!((0..4).all(|number| holders(&f, number) == 0));
    endpoint.uninstall().unwrap();
}

#[test]
fn read_spill_hold_refused_falls_back_to_memory_window() {
    let env = racer_io::Env::host();
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let (mut f, _local, mut endpoint, mut stream) = spill_reader(&env, 4, 2);
    // Another version takes every hold the node admits.
    let mut other = f.page.version.clone();
    other.etag = crate::model::StrongEtag::test_value("hold-capacity");
    let index = f.fill.dependencies.writer.index();
    let until = racer_io::Clock::host().wall_now() + Duration::from_secs(300);
    let mut capacity = 1 << 20;
    while capacity > 0 && index.reserve_hold(&other, capacity, until).is_err() {
        capacity /= 2;
    }
    while index.reserve_hold(&other, capacity + 1, until).is_ok() {
        capacity += 1;
    }
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut delivered = Vec::new();
    for _ in 0..8192 {
        let page = match stream.next_slice().as_mut().poll(&mut cx) {
            Poll::Ready(result) => match result.unwrap() {
                Some(reader) => Some(reader.slice().page),
                None => break,
            },
            Poll::Pending => None,
        };
        if let Some(page) = page {
            stream.release_page(page, PAGE_BYTES as u32).unwrap();
            delivered.push(page.0);
        }
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!(delivered, vec![0, 1, 2, 3]);
    assert!((0..4).all(|number| holders(&f, number) == 0));
    index.release_hold(&other).unwrap();
    drop(stream);
    for _ in 0..64 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}

#[test]
fn read_spill_fill_failure_stops_spill_without_retrying() {
    let env = racer_io::Env::host();
    let queue = Rc::new(crate::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let (mut f, _local, mut endpoint, mut stream) = spill_reader(&env, 3, 1);
    // The head stays blocked while every origin fill fails, so only the spill
    // path can reach page 1.
    f.origin.blocked_pages.borrow_mut().insert(0);
    f.origin.version_unavailable.set(true);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..256 {
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    let spilled = f
        .origin
        .started_pages
        .borrow()
        .iter()
        .filter(|&&number| number == 1)
        .count();
    assert_eq!(spilled, 1, "a failed spill fill is not retried");
    assert_eq!(
        holders(&f, 1),
        0,
        "the failed spill page's hold is released"
    );
    f.origin.blocked_pages.borrow_mut().clear();
    futures::executor::block_on(stream.cancel()).unwrap();
    drop(stream);
    for _ in 0..256 {
        pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
    }
    endpoint.uninstall().unwrap();
}
