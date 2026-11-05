#![allow(clippy::disallowed_methods)] // Tests build instants to feed the API.

use super::*;
use crate::read::acquisition::{Input as AcqInput, RankAnswer};
use crate::read::hints::{HINT_CAPACITY, HINT_TTL};
use crate::read::walk::{Features, HedgeEnd, Ranked, Reply};
use crate::read::{OperationKind, PageBudget, ResponseKind, ServeFacts};
use std::collections::BTreeMap;
use std::time::Duration;

#[derive(Clone, Debug, Eq, PartialEq)]
struct Ty;

impl Types for Ty {
    type Rank = &'static str;
    type Membership = u64;
    type Answer = &'static str;
    type Page = u64;
    type Object = u64;
    type Metadata = u64;
}

type Core = WorkerCore<Ty>;

fn fill_config(now: Instant) -> FillConfig {
    FillConfig {
        operation: OperationKind::Page,
        hedge: false,
        walk_only: false,
        scope: now + Duration::from_secs(60),
        features: Features::PRODUCTION,
    }
}

/// Pages are owned by worker `page % workers`; owner 10's helper is 20, owner 11 has none.
fn core_of(me: Worker, workers: u32, now: Instant) -> Core {
    WorkerCore::new(
        Routing {
            me,
            owner_of: Box::new(move |page| (*page % u64::from(workers)) as u32),
            plan: Box::new(move |_, budget| (fill_config(budget.deadline), "r")),
            mesh: Box::new(|_, _| {
                Some(Mesh {
                    owners: vec![10, 11, 12],
                    start: 0,
                    reader: 1,
                })
            }),
            helper_of: Box::new(|owner| (owner == 10).then_some(20)),
        },
        FlightLimits::default(),
        16,
        ServeDecision::new(1 << 20),
        SuccessorHints::new(HINT_CAPACITY, HINT_TTL),
        OwnerBudget::new(4, 0),
        now,
    )
}

fn core(now: Instant) -> Core {
    core_of(0, 1, now)
}

fn spec(now: Instant) -> AcquisitionBudgetSpec {
    AcquisitionBudgetSpec {
        deadline: now + Duration::from_secs(30),
        attempts: 8,
        links: 16,
        route_failed: false,
    }
}

fn ranked() -> AcqInput<&'static str, u64, &'static str> {
    AcqInput::Ranked {
        id: EffectId(0),
        result: Ok(RankAnswer {
            ranked: Ranked {
                members: 8,
                ..Ranked::new(5, &[10, 11, 12], None)
            },
            ranking: Some("r"),
            recent: None,
        }),
    }
}

/// `input` with its id replaced by `id`.
fn with_id(
    input: AcqInput<&'static str, u64, &'static str>,
    id: EffectId,
) -> AcqInput<&'static str, u64, &'static str> {
    match input {
        AcqInput::Ranked { result, .. } => AcqInput::Ranked { id, result },
        other => other,
    }
}

fn acq(out: &[Effect<Ty>]) -> Option<(ReadId, EffectId, Class)> {
    out.iter().rev().find_map(|e| match e {
        Effect::Acquisition { read, .. } => Some((*read, e.id()?, e.class())),
        _ => None,
    })
}

/// Drive the leader acquisition from its first effect to a published peer copy.
fn run_to_publish(c: &mut Core, now: Instant, out: &mut Vec<Effect<Ty>>) {
    let (read, id, class) = acq(out).unwrap();
    assert_eq!(class, Class::WorkerMsg, "ranking");
    out.clear();
    c.on_acquisition(read, with_id(ranked(), id), now, out)
        .unwrap();
    let (read, id, class) = acq(out).unwrap();
    assert_eq!(class, Class::PeerSend);
    out.clear();
    let reply = AcqInput::Replied {
        id,
        reply: Reply::Answer {
            kind: ResponseKind::Page,
            signed: true,
        },
        answer: Some("a"),
    };
    c.on_acquisition(read, reply, now, out).unwrap();
    let (read, id, class) = acq(out).unwrap();
    assert_eq!(class, Class::Crypto);
    out.clear();
    let validated = AcqInput::Validated {
        id,
        result: Ok(None),
    };
    c.on_acquisition(read, validated, now, out).unwrap();
    let (read, id, class) = acq(out).unwrap();
    assert_eq!(class, Class::DiskWrite);
    out.clear();
    let published = AcqInput::Published { id, result: Ok(()) };
    c.on_acquisition(read, published, now, out).unwrap();
}

fn completed(out: &[Effect<Ty>]) -> Vec<(RequestId, Result<(), Error>)> {
    out.iter()
        .filter_map(|e| match e {
            Effect::Completed {
                request, result, ..
            } => Some((*request, *result)),
            _ => None,
        })
        .collect()
}

#[test]
fn a_read_leads_its_flight_to_completion_and_the_flight_is_removed() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let request = c.read_page(7, spec(now), now, &mut out);
    assert_eq!((c.flights(), c.reads()), (1, 1));
    run_to_publish(&mut c, now, &mut out);
    assert_eq!(completed(&out), vec![(request, Ok(()))]);
    assert_eq!((c.flights(), c.reads(), c.requests()), (0, 0, 0));
    assert!(
        c.observed().evidence > 0,
        "reports are consumed as counters"
    );
}

#[test]
fn stale_acquisition_inputs_are_refused() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    c.read_page(7, spec(now), now, &mut out);
    let (read, _, _) = acq(&out).unwrap();
    let bogus = EffectId(77);
    assert_eq!(
        c.on_acquisition(read, AcqInput::NoHelper { id: bogus }, now, &mut out),
        Err(Refused::Stale(bogus))
    );
    assert_eq!(
        c.on_acquisition(ReadId(999), AcqInput::NoHelper { id: bogus }, now, &mut out),
        Err(Refused::Unknown)
    );
}

#[test]
fn waiters_join_the_leader_and_complete_together() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let a = c.read_page(7, spec(now), now, &mut out);
    let b = c.read_page(7, spec(now), now, &mut out);
    assert_eq!((c.flights(), c.reads(), c.requests()), (1, 1, 2));
    run_to_publish(&mut c, now, &mut out);
    let mut done = completed(&out);
    done.sort_by_key(|(r, _)| *r);
    assert_eq!(done, vec![(a, Ok(())), (b, Ok(()))]);
    assert_eq!((c.flights(), c.reads(), c.requests()), (0, 0, 0));
}

#[test]
fn a_canceled_leader_hands_its_acquisition_over() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let a = c.read_page(7, spec(now), now, &mut out);
    let effects = out.clone();
    let b = c.read_page(7, spec(now), now, &mut out);
    out.clear();
    c.cancel(a, now, &mut out);
    assert_eq!(completed(&out), vec![(a, Err(Error::Cancelled))]);
    assert_eq!(c.reads(), 1, "the acquisition keeps running for b");
    out = effects;
    run_to_publish(&mut c, now, &mut out);
    assert_eq!(completed(&out), vec![(b, Ok(()))]);
    assert_eq!((c.flights(), c.reads(), c.requests()), (0, 0, 0));
}

#[test]
fn a_flight_nobody_waits_for_stops_its_acquisition() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let a = c.read_page(7, spec(now), now, &mut out);
    let (read, id, _) = acq(&out).unwrap();
    c.cancel(a, now, &mut out);
    assert_eq!((c.flights(), c.reads(), c.requests()), (0, 0, 0));
    assert_eq!(
        c.on_acquisition(read, with_id(ranked(), id), now, &mut out),
        Err(Refused::Unknown),
        "late completions of the stopped acquisition go nowhere"
    );
}

#[test]
fn expired_waiters_fail_on_tick() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let a = c.read_page(7, spec(now), now, &mut out);
    out.clear();
    c.tick(now + Duration::from_secs(31), &mut out);
    assert_eq!(completed(&out), vec![(a, Err(Error::DeadlineExceeded))]);
    assert_eq!((c.flights(), c.reads(), c.requests()), (0, 0, 0));
}

fn window_config(now: Instant, end: u64) -> WindowConfig {
    WindowConfig {
        start: 0,
        end,
        window_pages: end as usize,
        sealed: false,
        passthrough: false,
        spill: 0,
        budget: PageBudget::Client {
            deadline: now + Duration::from_secs(60),
        },
    }
}

fn delivered(out: &[Effect<Ty>]) -> Vec<Next> {
    out.iter()
        .filter_map(|e| match e {
            Effect::Deliver { next, .. } => Some(*next),
            _ => None,
        })
        .collect()
}

#[test]
fn a_window_delivers_through_the_core_and_ends() {
    let now = Instant::now();
    let mut c = core(now);
    let window = c.open(window_config(now, 2), Box::new(|p| p));
    let mut out = Vec::new();
    c.pump(window, now, &Facts::default(), |_| true, &mut out)
        .unwrap();
    let probes: Vec<_> = out.iter().filter_map(Effect::id).collect();
    assert_eq!(probes.len(), 2);
    assert!(out.iter().all(|e| e.class() == Class::DiskRead));
    out.clear();
    let probed = |id| window::Input::Probed { id, found: true };
    c.on_window(window, probed(probes[1]), true, now, &mut out)
        .unwrap();
    assert!(out.is_empty(), "page 1 waits for page 0");
    c.on_window(window, probed(probes[0]), true, now, &mut out)
        .unwrap();
    assert!(matches!(
        delivered(&out)[..],
        [
            Next::Page { page: 0, .. },
            Next::Page { page: 1, .. },
            Next::End
        ]
    ));
    c.close(window, now, &mut out);
    c.drop_window(window);
    assert_eq!(c.windows(), 0);
    assert_eq!(
        c.pump(window, now, &Facts::default(), |_| true, &mut out),
        Err(Refused::Unknown)
    );
}

/// Pump a one-page window and miss its probe, so it fetches.
fn window_fetch(c: &mut Core, now: Instant, out: &mut Vec<Effect<Ty>>) -> WindowId {
    let window = c.open(window_config(now, 1), Box::new(|p| p + 4));
    c.pump(window, now, &Facts::default(), |_| true, out)
        .unwrap();
    let probe = out[0].id().unwrap();
    out.clear();
    let missed = window::Input::Probed {
        id: probe,
        found: false,
    };
    c.on_window(window, missed, true, now, out).unwrap();
    c.pump(window, now, &Facts::default(), |_| true, out)
        .unwrap();
    window
}

#[test]
fn a_local_window_fetch_joins_the_local_flight() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let window = window_fetch(&mut c, now, &mut out);
    assert!(
        out.iter().all(|e| !matches!(
            e,
            Effect::Window {
                effect: window::Effect::Fetch { .. },
                ..
            } | Effect::Worker { .. }
        )),
        "{out:?}"
    );
    assert_eq!((c.flights(), c.reads()), (1, 1), "{out:?}");
    assert!(
        c.acquiring(&4),
        "the window's key_of maps page 0 to flight 4"
    );
    run_to_publish(&mut c, now, &mut out);
    assert!(matches!(
        delivered(&out)[..],
        [Next::Page { page: 0, .. }, Next::End]
    ));
    assert_eq!((c.flights(), c.reads(), c.requests()), (0, 0, 0));
    c.close(window, now, &mut out);
    c.drop_window(window);
}

#[test]
fn a_remote_window_fetch_is_a_typed_worker_message() {
    let now = Instant::now();
    let mut a = core_of(0, 2, now);
    let mut b = core_of(1, 2, now);
    let mut out = Vec::new();
    // Window page 1 maps to flight 5, owned by worker 1.
    let window = a.open(window_config(now, 2), Box::new(|p| p + 4));
    a.pump(window, now, &Facts::default(), |_| true, &mut out)
        .unwrap();
    let probes: Vec<_> = out.iter().filter_map(Effect::id).collect();
    out.clear();
    for id in probes {
        let missed = window::Input::Probed { id, found: false };
        a.on_window(window, missed, true, now, &mut out).unwrap();
    }
    a.pump(window, now, &Facts::default(), |_| true, &mut out)
        .unwrap();
    let msgs: Vec<_> = out
        .iter()
        .filter_map(|e| match e {
            Effect::Worker { to, msg } => Some((*to, msg.clone())),
            _ => None,
        })
        .collect();
    assert_eq!(msgs.len(), 1, "{out:?}");
    assert_eq!(
        out.iter()
            .find(|e| matches!(e, Effect::Worker { .. }))
            .unwrap()
            .class(),
        Class::WorkerMsg
    );
    let (to, msg) = msgs[0].clone();
    assert_eq!(to, 1);
    assert!(matches!(
        msg,
        WorkerMsg::Fetch {
            page: 5,
            from: 0,
            ..
        }
    ));
    assert_eq!(a.remote_fetches(), 1);
    // Worker 1 leads its own flight and answers worker 0.
    let mut remote = Vec::new();
    b.on_worker_msg(msg, now, &mut remote).unwrap();
    run_to_publish(&mut b, now, &mut remote);
    let answer = remote
        .iter()
        .find_map(|e| match e {
            Effect::Worker { to: 0, msg } => Some(msg.clone()),
            _ => None,
        })
        .unwrap();
    assert!(matches!(
        answer,
        WorkerMsg::Fetched {
            result: Ok(Fetched::Page),
            ..
        }
    ));
    assert_eq!((b.flights(), b.reads()), (0, 0));
    // Worker 0's local page 0 (flight 4) completes too.
    run_to_publish(&mut a, now, &mut out);
    a.on_worker_msg(answer.clone(), now, &mut out).unwrap();
    assert!(matches!(
        delivered(&out)[..],
        [
            Next::Page { page: 0, .. },
            Next::Page { page: 1, .. },
            Next::End
        ]
    ));
    assert_eq!(a.remote_fetches(), 0);
    assert_eq!(
        a.on_worker_msg(answer, now, &mut out),
        Err(Refused::Unknown),
        "an answer is consumed once"
    );
}

/// Drive a non-owner read until rank 0 sheds it.
fn shed(c: &mut Core, now: Instant, out: &mut Vec<Effect<Ty>>) {
    c.read_page(7, spec(now), now, out);
    let (read, id, _) = acq(out).unwrap();
    out.clear();
    c.on_acquisition(read, with_id(ranked(), id), now, out)
        .unwrap();
    let (read, id, _) = acq(out).unwrap();
    out.clear();
    let overloaded = AcqInput::Replied {
        id,
        reply: Reply::Answer {
            kind: ResponseKind::Overloaded,
            signed: true,
        },
        answer: None,
    };
    c.on_acquisition(read, overloaded, now, out).unwrap();
}

#[test]
fn a_shed_requester_asks_its_mesh_helper() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    shed(&mut c, now, &mut out);
    let dests: Vec<_> = out
        .iter()
        .filter_map(|e| match e {
            Effect::Acquisition {
                effect: acquisition::Io::Send { dest, .. },
                ..
            } => Some(*dest),
            _ => None,
        })
        .collect();
    assert_eq!(dests, vec![Some(20)], "{out:?}");
    assert_eq!(c.observed().helpers, 1);
}

#[test]
fn a_signed_shed_reports_the_owner_busy_and_a_helper_answer_reports_the_helper() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    c.read_page(7, spec(now), now, &mut out);
    let (read, id, _) = acq(&out).unwrap();
    out.clear();
    c.on_acquisition(read, with_id(ranked(), id), now, &mut out)
        .unwrap();
    let owner = out
        .iter()
        .find_map(|e| match e {
            Effect::Acquisition {
                effect: acquisition::Io::Send { dest, .. },
                ..
            } => *dest,
            _ => None,
        })
        .unwrap();
    let (read, id, _) = acq(&out).unwrap();
    out.clear();
    let overloaded = AcqInput::Replied {
        id,
        reply: Reply::Answer {
            kind: ResponseKind::Overloaded,
            signed: true,
        },
        answer: None,
    };
    c.on_acquisition(read, overloaded, now, &mut out).unwrap();
    let silence: Vec<_> = out
        .iter()
        .filter(|e| matches!(e, Effect::Silence { .. }))
        .collect();
    assert_eq!(
        silence,
        vec![&Effect::Silence {
            page: 7,
            dest: owner,
            sent: now,
            note: SilenceNote::Shed,
        }],
        "{out:?}"
    );
    assert_eq!(silence[0].id(), None, "a silence report expects no answer");
    // Production names the helper as the destination (`CandidatePolicy::apply`,
    // `ProbeDest::Helper`): its answer says it is not hung.
    let (read, id, _) = acq(&out).unwrap();
    out.clear();
    let miss = AcqInput::Replied {
        id,
        reply: Reply::Answer {
            kind: ResponseKind::Miss,
            signed: true,
        },
        answer: None,
    };
    c.on_acquisition(read, miss, now, &mut out).unwrap();
    let silence: Vec<_> = out
        .iter()
        .filter_map(|e| match e {
            Effect::Silence { dest, note, .. } => Some((*dest, *note)),
            _ => None,
        })
        .collect();
    assert_eq!(silence, vec![(20, SilenceNote::Answered)], "{out:?}");
}

#[test]
fn a_helper_probe_timeout_counts_against_the_helper() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    shed(&mut c, now, &mut out);
    let (read, id, _) = acq(&out).unwrap();
    out.clear();
    c.on_acquisition(
        read,
        AcqInput::Replied {
            id,
            reply: Reply::TimedOut,
            answer: None,
        },
        now,
        &mut out,
    )
    .unwrap();
    let silence: Vec<_> = out
        .iter()
        .filter_map(|e| match e {
            Effect::Silence { dest, note, .. } => Some((*dest, *note)),
            _ => None,
        })
        .collect();
    // Not rank 0: the timeout demotes the helper and never asks a ping by itself.
    assert_eq!(
        silence,
        vec![(
            20,
            SilenceNote::TimedOut {
                needed: crate::liveness::NO_PING
            }
        )],
        "{out:?}"
    );
    // The next probe goes to an owner; its evidence is not about the helper.
    assert!(acq(&out).is_some(), "{out:?}");
}

#[test]
fn a_shed_requester_without_a_helper_walks_on() {
    let now = Instant::now();
    let mut c = core(now);
    c.routing.helper_of = Box::new(|_| None);
    let mut out = Vec::new();
    shed(&mut c, now, &mut out);
    assert_eq!(c.observed().no_helper, 1);
    assert!(out.iter().all(|e| !matches!(
        e,
        Effect::Acquisition {
            effect: acquisition::Io::Send { dest: None, .. },
            ..
        }
    )));
}

#[test]
fn ids_are_unique_across_machines() {
    let now = Instant::now();
    let mut c = core(now);
    let window = c.open(window_config(now, 3), Box::new(|p| p));
    let mut out = Vec::new();
    c.pump(window, now, &Facts::default(), |_| true, &mut out)
        .unwrap();
    c.read_page(9, spec(now), now, &mut out);
    let ids: Vec<_> = out.iter().filter_map(Effect::id).collect();
    let unique: BTreeSet<_> = ids.iter().collect();
    assert_eq!(ids.len(), 4);
    assert_eq!(unique.len(), ids.len());
}

fn serve_request() -> ServeRequest {
    ServeRequest {
        facts: ServeFacts {
            operation: OperationKind::Page,
            copy_only: false,
            object_matches: true,
            cache_available: true,
        },
        body: true,
        local_flight: false,
    }
}

#[test]
fn serve_defers_to_a_hinted_successor_unless_acquiring() {
    let now = Instant::now();
    let mut c = core(now);
    let owners = [1, 2, 3];
    let view = ServeView {
        owners: &owners,
        local: 1,
        requester: 9,
        column_mate: false,
    };
    assert_eq!(
        c.serve(&7, serve_request(), &view, now),
        Served::Decided(Decision::Acquire)
    );
    c.successors.note(7, 2, now);
    assert_eq!(c.serve(&7, serve_request(), &view, now), Served::Defer(2));
    let owner = ServeView {
        requester: 3,
        ..view
    };
    assert_eq!(
        c.serve(&7, serve_request(), &owner, now),
        Served::Decided(Decision::Acquire),
        "owners are never deferred"
    );
    let mut out = Vec::new();
    c.read_page(7, spec(now), now, &mut out);
    assert_eq!(
        c.serve(&7, serve_request(), &view, now),
        Served::Decided(Decision::Acquire),
        "a running local acquisition is joined instead"
    );
}

#[test]
fn metadata_requests_share_one_refresh() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let deadline = now + Duration::from_secs(10);
    let a = c.refresh_metadata(1, RefreshPurpose::Current, deadline, 3, now, &mut out);
    let b = c.refresh_metadata(1, RefreshPurpose::Current, deadline, 3, now, &mut out);
    let fetches: Vec<_> = out
        .iter()
        .filter(|e| matches!(e, Effect::Metadata { .. }))
        .collect();
    assert_eq!(fetches.len(), 1);
    assert_eq!(fetches[0].class(), Class::OriginFetch);
    let id = fetches[0].id().unwrap();
    out.clear();
    c.on_metadata(id, Ok(42), now, &mut out).unwrap();
    let mut ready: Vec<_> = out
        .iter()
        .filter_map(|e| match e {
            Effect::MetadataReady { request, result } => Some((*request, *result)),
            _ => None,
        })
        .collect();
    ready.sort_by_key(|(r, _)| *r);
    assert_eq!(ready, vec![(a, Ok(42)), (b, Ok(42))]);
    assert_eq!(c.metadata(), (0, 0));
    assert_eq!(
        c.on_metadata(id, Ok(1), now, &mut out),
        Err(Refused::Stale(id))
    );
}

#[test]
fn a_failed_metadata_leader_passes_the_refresh_on() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    let deadline = now + Duration::from_secs(10);
    c.refresh_metadata(1, RefreshPurpose::Current, deadline, 3, now, &mut out);
    c.refresh_metadata(1, RefreshPurpose::Current, deadline, 3, now, &mut out);
    let mut fetches = 0;
    while let Some(id) = out.iter().find_map(|e| match e {
        Effect::Metadata { id, .. } => Some(*id),
        _ => None,
    }) {
        fetches += 1;
        assert!(fetches < 10);
        out.clear();
        c.on_metadata(id, Err(Error::OriginRejected), now, &mut out)
            .unwrap();
    }
    assert!(fetches >= 2, "a waiter retried after the leader failed");
    assert_eq!(c.metadata(), (0, 0));
}

/// A tiny deterministic generator for the randomized test.
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }

    fn below(&mut self, n: u64) -> u64 {
        self.next() % n
    }
}

/// A fake interpreter for two workers: answers acquisition effects with random outcomes,
/// delivers worker messages, and starts, cancels and expires client reads and windows.
fn randomized(seed: u64) -> (usize, usize) {
    let mut rng = Rng(seed | 1);
    let start = Instant::now();
    let mut now = start;
    let mut cores = [core_of(0, 2, now), core_of(1, 2, now)];
    let mut pending: Vec<(usize, Effect<Ty>)> = Vec::new();
    let mut answered: BTreeSet<(usize, EffectId)> = BTreeSet::new();
    let mut open: BTreeMap<(usize, RequestId), ()> = BTreeMap::new();
    let mut windows: Vec<(usize, WindowId)> = Vec::new();
    let mut ended = BTreeSet::new();
    let mut leases = Vec::new();
    let mut served = 0;
    for step in 0..400 {
        let mut out = Vec::new();
        let w = rng.below(2) as usize;
        match rng.below(10) {
            0..=2 if step < 300 => {
                let page = rng.below(6);
                let r = cores[w].read_page(page, spec(now), now, &mut out);
                open.insert((w, r), ());
            }
            3 if step < 300 => {
                let window = cores[w].open(window_config(now, 3), Box::new(|p| p + 2));
                cores[w]
                    .pump(window, now, &Facts::default(), |_| true, &mut out)
                    .unwrap();
                windows.push((w, window));
            }
            4 => {
                if let Some(&(cw, r)) = open
                    .keys()
                    .nth(rng.below(open.len().max(1) as u64) as usize)
                {
                    cores[cw].cancel(r, now, &mut out);
                    let mut o = Vec::new();
                    std::mem::swap(&mut o, &mut out);
                    pending.extend(o.into_iter().map(|e| (cw, e)));
                }
            }
            5 => {
                now += Duration::from_secs(rng.below(4));
                for (i, c) in cores.iter_mut().enumerate() {
                    let mut o = Vec::new();
                    c.tick(now, &mut o);
                    pending.extend(o.into_iter().map(|e| (i, e)));
                }
            }
            6 => {
                let key = rng.below(4);
                if let Ok(lease) = cores[w].prefetch.admit(key, 1, now) {
                    leases.push((w, key, lease));
                } else if let Some((lw, key, lease)) = leases.pop() {
                    cores[lw].prefetch.finish(&key, lease);
                }
            }
            7 => {
                for &(ww, window) in &windows {
                    let mut o = Vec::new();
                    let _ = cores[ww].pump(window, now, &Facts::default(), |_| true, &mut o);
                    pending.extend(o.into_iter().map(|e| (ww, e)));
                }
            }
            _ => {}
        }
        pending.extend(out.drain(..).map(|e| (w, e)));
        // Answer a few pending effects in random order.
        for _ in 0..3 {
            if pending.is_empty() {
                break;
            }
            let at = rng.below(pending.len() as u64) as usize;
            let (w, effect) = pending.swap_remove(at);
            if let Effect::Completed {
                request, result, ..
            } = &effect
            {
                assert!(
                    open.remove(&(w, *request)).is_some(),
                    "completion of an unknown read"
                );
                served += usize::from(result.is_ok());
                continue;
            }
            let mut out = Vec::new();
            if let Some(id) = effect.id()
                && !matches!(effect, Effect::Worker { .. })
            {
                assert!(answered.insert((w, id)), "effect {id:?} answered twice");
            }
            answer(&mut cores, w, effect, &mut rng, now, &mut out, &mut ended);
            pending.extend(out);
        }
        pending.retain(|(i, e)| {
            if let Effect::Completed { request, .. } = e {
                assert!(
                    open.remove(&(*i, *request)).is_some(),
                    "completion of an unknown read"
                );
                return false;
            }
            true
        });
    }
    // Drain: answer everything, then expire whatever is left.
    let mut rounds = 0;
    while !pending.is_empty() {
        rounds += 1;
        assert!(rounds < 100_000, "the fake interpreter does not converge");
        let (w, effect) = pending.remove(0);
        if let Effect::Completed { request, .. } = &effect {
            assert!(
                open.remove(&(w, *request)).is_some(),
                "completion of an unknown read"
            );
            continue;
        }
        if let Some(id) = effect.id()
            && !matches!(effect, Effect::Worker { .. })
        {
            assert!(answered.insert((w, id)), "effect {id:?} answered twice");
        }
        let mut out = Vec::new();
        answer(&mut cores, w, effect, &mut rng, now, &mut out, &mut ended);
        pending.extend(out);
        if pending.is_empty() {
            now += Duration::from_secs(120);
            for (i, c) in cores.iter_mut().enumerate() {
                let mut o = Vec::new();
                c.tick(now, &mut o);
                pending.extend(o.into_iter().map(|e| (i, e)));
            }
        }
    }
    assert!(
        open.is_empty(),
        "lost waiters: {open:?} core requests {} {} flights {} {}",
        cores[0].requests(),
        cores[1].requests(),
        cores[0].flights(),
        cores[1].flights()
    );
    for (w, window) in windows {
        let mut out = Vec::new();
        cores[w].close(window, now, &mut out);
        cores[w].drop_window(window);
    }
    for (w, key, lease) in leases {
        cores[w].prefetch.finish(&key, lease);
    }
    for c in &cores {
        assert_eq!(c.flights(), 0, "flights removed at quiescence");
        assert_eq!((c.reads(), c.requests(), c.remote_fetches()), (0, 0, 0));
        assert_eq!(c.prefetch.active(), 0, "prefetch budget conserved");
    }
    (served, ended.len())
}

#[allow(clippy::too_many_arguments)]
fn answer(
    cores: &mut [Core; 2],
    w: usize,
    effect: Effect<Ty>,
    rng: &mut Rng,
    now: Instant,
    out: &mut Vec<(usize, Effect<Ty>)>,
    ended: &mut BTreeSet<(usize, WindowId)>,
) {
    use acquisition::Io as Acq;
    let mut o = Vec::new();
    let c = &mut cores[w];
    let fail = rng.below(5) == 0;
    match effect {
        Effect::Acquisition { read, effect, .. } => {
            let input = match effect {
                Acq::Rank { id, .. } => with_id(ranked(), id),
                Acq::Hints { id, .. } => AcqInput::Hints {
                    id,
                    hints: Vec::new(),
                    rank0: Default::default(),
                },
                Acq::Hedge { id, .. } => AcqInput::Hedged {
                    id,
                    end: HedgeEnd::NotStarted,
                    copy: false,
                },
                Acq::Refresh { id, .. } => AcqInput::Membership {
                    id,
                    latest: Ok(None),
                    recent_previous: None,
                },
                Acq::Send { id, .. } => match rng.below(4) {
                    0 => AcqInput::Replied {
                        id,
                        reply: Reply::Failed {
                            error: Error::Unavailable,
                            sent: true,
                            scope_live: true,
                        },
                        answer: None,
                    },
                    1 => AcqInput::Replied {
                        id,
                        reply: Reply::Answer {
                            kind: ResponseKind::Overloaded,
                            signed: true,
                        },
                        answer: None,
                    },
                    _ => AcqInput::Replied {
                        id,
                        reply: Reply::Answer {
                            kind: ResponseKind::Page,
                            signed: true,
                        },
                        answer: Some("a"),
                    },
                },
                Acq::Validate { id, .. } => AcqInput::Validated {
                    id,
                    result: if fail {
                        Err(Error::Unavailable)
                    } else {
                        Ok(None)
                    },
                },
                Acq::Batch { id, .. } => AcqInput::Batched {
                    id,
                    result: Ok(false),
                },
                Acq::Sleep { id, .. } => AcqInput::Timer { id, result: Ok(()) },
                Acq::Origin { id, .. } => AcqInput::Origin {
                    id,
                    result: if fail {
                        Err(Error::Unavailable)
                    } else {
                        Ok(())
                    },
                },
                Acq::Publish { id, .. } => AcqInput::Published { id, result: Ok(()) },
            };
            let _ = c.on_acquisition(read, input, now, &mut o);
        }
        Effect::Window { window, effect } => {
            let input = match effect {
                window::Effect::Probe { id, .. } => Some(window::Input::Probed {
                    id,
                    found: rng.below(2) == 0,
                }),
                window::Effect::Select { id, .. } => Some(window::Input::Selected {
                    id,
                    ok: false,
                    started: false,
                }),
                window::Effect::Hold { id, .. } => Some(window::Input::Held {
                    id,
                    result: Err(Error::Unavailable),
                }),
                window::Effect::SpillFill { id, .. } => {
                    Some(window::Input::SpillFilled { id, ok: false })
                }
                window::Effect::Fetch { .. } => panic!("the core routes fetches"),
                window::Effect::Release(_) | window::Effect::Discard(_) => None,
            };
            if let Some(input) = input {
                let _ = c.on_window(window, input, true, now, &mut o);
            }
        }
        Effect::Deliver { window, next } => {
            if matches!(next, Next::End | Next::Fail(_)) {
                assert!(ended.insert((w, window)), "a window ended twice");
            }
        }
        Effect::Worker { to, msg } => {
            let to = to as usize;
            let _ = cores[to].on_worker_msg(msg, now, &mut o);
            out.extend(o.into_iter().map(|e| (to, e)));
            return;
        }
        Effect::Completed { .. } => panic!("completions are checked by the caller"),
        Effect::MetadataReady { .. } | Effect::Silence { .. } => {}
        Effect::Metadata { id, .. } => {
            let _ = c.on_metadata(id, Ok(1), now, &mut o);
        }
    }
    out.extend(o.into_iter().map(|e| (w, e)));
}

#[test]
fn randomized_worker_cores_keep_their_invariants() {
    let (mut served, mut ended) = (0, 0);
    for seed in 1..=40u64 {
        let (s, e) = randomized(seed.wrapping_mul(0x9e37_79b9_7f4a_7c15));
        served += s;
        ended += e;
    }
    assert!(served > 40 && ended > 40, "served {served} ended {ended}");
}

#[test]
fn request_budget_is_the_outstanding_sends_budget() {
    let now = Instant::now();
    let mut c = core(now);
    let mut out = Vec::new();
    shed(&mut c, now, &mut out);
    let (read, _, _) = acq(&out).unwrap();
    let budget = c.request_budget(read).expect("a running read has a budget");
    assert!(budget.remaining_attempts() > 0);
    assert!(
        budget.route_failed(),
        "a helper probe forwards every funded link"
    );
    assert!(c.request_budget(ReadId(999)).is_none());
}
