//! Terminal registrations release waiter quota before the operation completes.
use super::*;
use crate::{
    error::Error,
    model::{
        context::OriginContext,
        identity::{
            CacheKey, MembershipVersion, ObjectId, ObjectVersion, PageId, PageNumber, RequestId,
            StrongEtag,
        },
    },
    read::flight::{
        AcquisitionBudget, AcquisitionEvent, AcquisitionFailure, Flights, JoinedFlight,
    },
    runtime::deadline::RequestScope,
    topology::membership::Membership,
};
use std::{
    rc::Rc,
    task::{Context, Poll},
    time::{Duration, Instant},
};

fn config() -> Config {
    Config {
        nodes: 2,
        workers: 1,
        pages_per_worker: 2,
        queue_entries: 2,
        pipes: 4,
        window: 2,
        ..Config::default()
    }
}

fn request() -> Request {
    Request {
        pages: 2,
        ..Request::new(0, 0)
    }
}

fn waiters(sim: &Simulator) -> usize {
    sim.workers[0].admission.used(ResourceClass::Waiter)
}

fn join<'a>(
    flights: &Rc<Flights>,
    page: &PageId,
    origin: &'a OriginContext,
    scope: &'a RequestScope,
    budget: &'a mut AcquisitionBudget,
) -> crate::error::Result<JoinedFlight<'a>> {
    let membership = Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap());
    flights.join(page.clone(), membership, origin, scope, budget)
}

#[test]
fn terminal_waiter_replacement_matches_production_before_completion() {
    for canceled in [true, false] {
        let mut sim = Simulator::new(config());
        sim.arrive(0, request());
        sim.pump(0);
        sim.arrive(1, request());
        sim.pump(1);

        let real = Rc::new(Admission::new(sim.workers[0].admission.limits().clone()));
        let flights = Rc::new(Flights::new(real.clone()));
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("0".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        };
        let origin = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let deadline = Instant::now() + Duration::from_secs(60);
        let scope = RequestScope::new(RequestId([0; 16]), deadline).unwrap();
        let terminal_scope = RequestScope::new(RequestId([1; 16]), deadline).unwrap();
        let mut budgets =
            std::array::from_fn::<_, 3, _>(|_| AcquisitionBudget::new(deadline, 3, 8));
        let [first_budget, terminal_budget, replacement_budget] = &mut budgets;
        let JoinedFlight::Waiter(mut first) =
            join(&flights, &page, &origin, &scope, first_budget).unwrap()
        else {
            panic!("expected first registration")
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(AcquisitionEvent::Lead(leader))) = first.wait().as_mut().poll(&mut cx)
        else {
            panic!("expected first acquisition")
        };
        let operation = flights
            .retain_operation(
                &leader,
                real.reserve_fill(&origin.object.cache, true).unwrap(),
            )
            .unwrap();
        let JoinedFlight::Waiter(mut terminal) =
            join(&flights, &page, &origin, &terminal_scope, terminal_budget).unwrap()
        else {
            panic!("expected second registration")
        };
        assert!(terminal.wait().as_mut().poll(&mut cx).is_pending());
        assert_eq!(real.used(ResourceClass::Waiter), 2);
        assert_eq!(waiters(&sim), 4, "two bounded registrations on each page");
        assert!(matches!(
            join(&flights, &page, &origin, &scope, replacement_budget),
            Err(Error::Overloaded)
        ));

        if canceled {
            terminal_scope.cancel().unwrap();
        }
        // Cancellation or a request failure drops its pending page registration.
        drop(terminal);
        sim.now = 1;
        sim.terminate(1, canceled);
        sim.sample();
        assert_eq!(real.used(ResourceClass::Waiter), 1);
        assert_eq!(waiters(&sim), 2);
        assert_eq!(sim.workers[0].sampled[ResourceClass::Waiter as usize], 2);
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::DirtyCiphertext,
        ] {
            let bytes = if matches!(class, ResourceClass::Plaintext) {
                PLAIN
            } else {
                CIPHER
            };
            assert_eq!(real.used(class), bytes);
            assert_eq!(sim.workers[0].admission.used(class), 2 * bytes);
        }
        assert_eq!(real.used(ResourceClass::ControlProgress), 1);
        assert_eq!(real.used(ResourceClass::Flight), 1);
        assert!(!operation.cancellation_requested());
        assert_eq!(sim.workers[0].admission.used(ResourceClass::Flight), 2);
        assert_eq!(
            sim.workers[0].admission.used(ResourceClass::RequestContext),
            2 * CONTEXT
        );

        let JoinedFlight::Waiter(mut replacement) =
            join(&flights, &page, &origin, &scope, replacement_budget).unwrap()
        else {
            panic!("replacement must join before completion")
        };
        assert!(replacement.wait().as_mut().poll(&mut cx).is_pending());
        sim.arrive(2, request());
        sim.pump(2);
        assert_eq!(real.used(ResourceClass::Waiter), 2);
        assert_eq!(waiters(&sim), 4);
        assert_eq!(sim.report.fills, 2);
        assert_eq!(sim.report.joined, 4);
        assert_eq!(sim.report.retries, 0);
        assert_eq!(sim.active[&2].outstanding, 2);
        assert!(sim.active[&2].ready.is_empty());
        for flight in sim.workers[0].flights.values() {
            assert_eq!(
                flight.waiters.iter().map(|w| w.0).collect::<Vec<_>>(),
                vec![0, 2]
            );
        }
        assert_eq!(
            sim.events
                .values()
                .filter(|event| matches!(event, Event::Fill(..)))
                .count(),
            2
        );

        operation.complete().unwrap();
        flights
            .fail(leader, AcquisitionFailure::Terminal(Error::Io))
            .unwrap();
        drop((first, replacement));
        assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
    }
}

#[test]
fn last_waiter_detach_keeps_peer_source_and_completion_owners() {
    let mut sim = Simulator::new(config());
    let key = (0, 0, 0);
    let source = Bundle {
        plain: Arc::new(sim.reserve(1, 0, ResourceClass::Plaintext, PLAIN).unwrap()),
        cipher: Arc::new(
            sim.reserve(1, 0, ResourceClass::Ciphertext, CIPHER)
                .unwrap(),
        ),
    };
    let pin = Arc::downgrade(&source.cipher);
    sim.workers[1].cache.insert(key, source);
    sim.directory.entry(key).or_default().insert(1);
    sim.arrive(
        0,
        Request {
            pages: 1,
            ..request()
        },
    );
    sim.pump(0);
    assert_eq!(pin.strong_count(), 2);
    sim.config.max_attempts = 2;
    let active = sim.active.remove(&0).unwrap();
    sim.retry(0, active);
    assert_eq!(waiters(&sim), 1, "a live retry keeps its registration");
    let active = sim.active.remove(&0).unwrap();
    sim.retry(0, active);
    assert_eq!(waiters(&sim), 0);
    assert!(sim.workers[0].flights[&key].waiters.is_empty());
    assert_eq!(pin.strong_count(), 2);
    assert_eq!(
        sim.workers[0].admission.used(ResourceClass::Plaintext),
        PLAIN
    );
    assert_eq!(
        sim.workers[0].admission.used(ResourceClass::Ciphertext),
        CIPHER
    );
    assert_eq!(
        sim.workers[0]
            .admission
            .used(ResourceClass::DirtyCiphertext),
        CIPHER
    );
    assert_eq!(sim.workers[0].admission.used(ResourceClass::Flight), 1);
    assert!(
        sim.events
            .values()
            .any(|event| matches!(event, Event::Fill(0, k) if *k == key))
    );
    assert!(
        sim.events
            .values()
            .any(|event| matches!(event, Event::Release(_)))
    );
    sim.detach_waiters(0, 0);
    assert_eq!(waiters(&sim), 0, "repeat detach is harmless");
}

#[test]
fn replacement_reads_finish_after_cancellation_or_deadline_detaches_waiters() {
    for canceled in [true, false] {
        let mut terminal = request();
        if canceled {
            terminal.cancel_after = Some(1);
        } else {
            terminal.deadline = 1;
        }
        let replacement = Request { at: 2, ..request() };
        let report = Simulator::new(config()).run(vec![request(), terminal, replacement]);
        assert_eq!(report.completed, 2, "{report:?}");
        assert_eq!(report.canceled, usize::from(canceled));
        assert_eq!(report.failed, usize::from(!canceled));
        assert_eq!(report.fills, 2);
        assert_eq!(report.joined, 4);
        assert_eq!(report.retries, 0);
        assert_eq!(report.final_used, [0; 11]);
    }
}
