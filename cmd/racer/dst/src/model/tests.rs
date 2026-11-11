//! Unit tests for the Model interpreter.

use super::*;

#[test]
fn a_first_ranking_scores_every_member_and_a_cached_one_polls_once() {
    assert_eq!(ranking_polls(true, 0), 1);
    assert_eq!(ranking_polls(true, 256), 1);
    assert_eq!(ranking_polls(true, 257), 2);
    assert_eq!(ranking_polls(true, 50_000), 196);
    assert_eq!(ranking_polls(false, 50_000), 1);
}

fn page(number: u32) -> Page {
    Page { object: 0, number }
}

#[test]
fn a_rank0_timeout_pings_and_an_answer_clears_the_silence() {
    let scenario = crate::scenarios::cold_start(16);
    let spec = scenario.model.as_deref().expect("model spec");
    let mut model = Model::new(&scenario, spec, Choices::seeded(1));
    let sent = model.instant(0);
    model.engine.at(SECONDS, Ev::PollPings { from: 0 });
    model.run(SECONDS, u64::MAX);
    let at = model.instant(model.now());
    let needed = racer_core::liveness::RANK0_TIMEOUTS;
    for _ in 0..needed {
        model.nodes[0]
            .silence
            .timed_out(5, page(1), sent, at, needed);
    }
    assert!(model.nodes[0].silence.unanswered(&5, at));
    model.poll_pings(0);
    assert_eq!(model.report.counters.silence_pings, 1);
    assert_eq!(model.nodes[0].pings, 1);
    model.run(2 * SECONDS, u64::MAX);
    assert_eq!(model.nodes[0].pings, 0);
    let at = model.instant(model.now());
    assert!(!model.nodes[0].silence.unanswered(&5, at));
    assert!(!model.nodes[0].silence.silent(&5, at));
}

/// A request waiting for verification must not hold its destination worker's CPU: the
/// worker stays free for other work (signing, searches) until the verification ends.
#[test]
fn a_request_waiting_for_verification_leaves_its_worker_free() {
    let scenario = crate::scenarios::cold_start(16);
    let spec = scenario.model.as_deref().expect("model spec");
    let mut model = Model::new(&scenario, spec, Choices::seeded(1));
    let version = model.versions.borrow().adopted(0).number;
    let mid = 7;
    model.msgs.insert(
        mid,
        Msg {
            from: 0,
            worker: 0,
            read: ReadId(0),
            id: EffectId::default(),
            dest: 1,
            page: page(1),
            route: Route {
                version,
                copy_only: false,
                deadline: 10 * SECONDS,
                attempts: 1,
                links: 4,
                charged: 4,
            },
            path: vec![0],
            at: 1,
            abandon: 10 * SECONDS,
            ping: None,
        },
    );
    let conn = model.conn_worker(mid);
    // A deep crypto queue: the verification ends 10 ms from now.
    model.fabric.crypto(0, 1, conn, 10 * MILLIS);
    model.hop(mid, false);
    let worker = owner_worker(page(1), spec.workers.max(1)) as u8;
    assert_eq!(model.fabric.cpu(0, 1, worker, 1), 1);
}

#[test]
fn route_searches_queue_in_arrival_order_then_shed() {
    assert_eq!(search_start(&[], 5), Some(5));
    let running: Vec<Nanos> = (1..=ACTIVE_SEARCHES as Nanos).map(|i| 100 * i).collect();
    assert_eq!(search_start(&running[1..], 5), Some(5));
    // Every slot busy: the next search starts when the first one ends.
    assert_eq!(search_start(&running, 5), Some(100));
    // One waiting (ending at 150): the next one takes the second slot to free.
    let mut waiting = running.clone();
    waiting.push(150);
    assert_eq!(search_start(&waiting, 5), Some(150));
    // A full queue sheds.
    let full: Vec<Nanos> = (0..(ACTIVE_SEARCHES + SEARCH_QUEUE) as Nanos)
        .map(|i| 10 + i)
        .collect();
    assert_eq!(search_start(&full, 5), None);
    assert!(search_start(&full[1..], 5).is_some());
}

#[test]
fn store_evicts_the_least_recently_used_page() {
    let mut store = Store::new(2);
    assert_eq!(store.insert(page(1)), 0);
    assert_eq!(store.insert(page(2)), 0);
    assert!(store.touch(page(1)));
    assert_eq!(store.insert(page(3)), 1);
    assert!(store.contains(&page(1)) && store.contains(&page(3)));
    assert!(!store.contains(&page(2)) && !store.touch(page(2)));
    assert_eq!(store.insert(page(3)), 0, "a re-publish refreshes");
    assert_eq!(store.len(), 2);
}

#[test]
fn an_unbounded_store_never_evicts() {
    let mut store = Store::new(0);
    for n in 0..100 {
        assert_eq!(store.insert(page(n)), 0);
    }
    assert_eq!(store.len(), 100);
}
