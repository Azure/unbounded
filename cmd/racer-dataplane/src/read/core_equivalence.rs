//! Equivalence between production read-path state machines and the sans-IO
//! versions in `racer-core` that the simulator (`racer-sim`) drives.
//!
//! These replace the `fidelity` and `waiter_detach` oracles of the retired
//! `src/contention.rs` model: rather than comparing a payload-free model with
//! production, they check that the core used by the simulator makes the same
//! decisions as production on seeded traces.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::memory::PageResult;
use crate::model::CacheKey;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::RequestId;
use crate::model::StrongEtag;
use crate::read::flight::AcquisitionBudget;
use crate::read::flight::AcquisitionEvent;
use crate::read::flight::AcquisitionFailure;
use crate::read::flight::AcquisitionWaiter;
use crate::read::flight::FlightLeader;
use crate::read::flight::FlightLimits;
use crate::read::flight::FlightOperation;
use crate::read::flight::Flights;
use crate::read::flight::JoinedFlight;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use flow_control::coalesce::flight::state::Outcome;
use flow_control::coalesce::flight::state::Published;
use racer_control_wire::CacheId;
use racer_core::read::flight as core;
use racer_core::time::SplitMix64;
use std::rc::Rc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;

/// Observable outcome of one step, comparable across both tables.
#[derive(Clone, Debug, PartialEq, Eq)]
enum Obs {
    Waiter,
    Complete(u8),
    Partial,
    Lead,
    Pending,
    Failed(String),
    Err(String),
}

fn page_id() -> PageId {
    PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(crate::test_support::security::CACHE.into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        },
        number: PageNumber(0),
    }
}

/// A published production page whose plaintext is `[value; 3]`.
fn result(flights: &Flights, page: PageId, value: u8) -> PageResult {
    use crate::memory::CiphertextBytes;
    use crate::memory::CiphertextPage;
    use crate::memory::VerifiedBytes;
    use crate::memory::VerifiedPage;
    use crate::model::ExpiresAt;
    use crate::model::Nonce;
    use crate::model::ObjectMetadata;
    use crate::model::PageEnvelope;
    use std::sync::Arc;
    PageResult {
        metadata: ObjectMetadata {
            content_type: None,
            version: page.version.clone(),
            length: 3,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        },
        plaintext: VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: page.clone(),
                storage: flow_control::ChargedBytes {
                    bytes: vec![value; 3],
                    reservation: flights
                        .admission
                        .reserve(None, ResourceClass::Plaintext, 3)
                        .unwrap(),
                },
            }),
        },
        ciphertext: CiphertextPage {
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                checksum: std::sync::OnceLock::new(),
                envelope: PageEnvelope {
                    cipher: crate::model::PageCipher::Xaes256Gcm,
                    page,
                    key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([0; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                storage: flow_control::ChargedBytes {
                    bytes: vec![0; 19],
                    reservation: flights
                        .admission
                        .reserve(None, ResourceClass::Ciphertext, 19)
                        .unwrap(),
                }
                .into(),
            }),
        },
    }
}

/// Core admission that counts what production charges as `Waiter` and
/// `Flight` quota.
#[derive(Default)]
struct Count {
    waiters: usize,
    flights: usize,
}

impl core::FlightAdmission for Count {
    fn waiter(&mut self, _: &PageId) -> racer_core::read::Result<()> {
        self.waiters += 1;
        Ok(())
    }
    fn flight(&mut self, _: &PageId) -> racer_core::read::Result<()> {
        self.flights += 1;
        Ok(())
    }
    fn release_waiter(&mut self, _: &PageId) {
        self.waiters -= 1;
    }
    fn release_flight(&mut self, _: &PageId) {
        self.flights -= 1;
    }
}

type CoreTable = core::PageFlights<u8, (), u8>;

fn split(value: u8) -> Published<u8, ()> {
    Published::Complete(value)
}

/// One caller present on both sides.
struct Caller {
    prod: Option<AcquisitionWaiter<'static>>,
    leader: Option<FlightLeader>,
    scope: &'static RequestScope,
    core: u64,
}

struct Pair {
    flights: Rc<Flights>,
    quotas: Rc<flow_control::Quotas<AdmissionPolicy>>,
    table: CoreTable,
    count: Count,
    wakes: Vec<core::Token>,
    origin: &'static OriginContext,
    page: PageId,
    next_request: u8,
    /// Production operations retained by leaders and not yet completed; core
    /// counts the same operations per flight.
    operations: Vec<FlightOperation>,
}

fn poll_prod(waiter: &mut AcquisitionWaiter<'static>) -> (Obs, Option<FlightLeader>) {
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    match waiter.wait().as_mut().poll(&mut cx) {
        Poll::Pending => (Obs::Pending, None),
        Poll::Ready(Ok(AcquisitionEvent::Lead(leader))) => (Obs::Lead, Some(leader)),
        Poll::Ready(Ok(AcquisitionEvent::Complete(page))) => {
            (Obs::Complete(page.plaintext.inner.storage.bytes[0]), None)
        }
        Poll::Ready(Ok(AcquisitionEvent::Ciphertext(_))) => (Obs::Partial, None),
        Poll::Ready(Ok(AcquisitionEvent::Failed(e))) => (Obs::Failed(format!("{e:?}")), None),
        Poll::Ready(Err(e)) => (Obs::Err(format!("{e:?}")), None),
    }
}

/// Leader settlement must succeed or fail together. A revoked leader (its
/// waiter canceled or expired) is rejected by both, with different errors:
/// production returns `StaleFlight` from the fence check
/// (`src/read/flight.rs:1605`, `leader_entry`), core returns `Internal` from
/// `validate_leader` (`core/src/read/flight.rs:295`, `finish`). Neither reaches a
/// reader; both drivers drop the attempt.
fn settle(prod: crate::error::Result<()>, core: racer_core::read::Result<()>) {
    match (&prod, &core) {
        (Ok(()), Ok(())) => {}
        (Err(Error::StaleFlight), Err(racer_core::read::Error::Internal)) => {}
        _ => panic!("settle: production {prod:?}, core {core:?}"),
    }
}

impl Pair {
    fn new(limits: core::FlightLimits) -> Self {
        let quotas = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let flights = Rc::new(
            Flights::with_limits(
                quotas.clone(),
                crate::test_support::availability(),
                FlightLimits {
                    entries: limits.entries,
                    waiters_per_flight: limits.waiters_per_flight,
                    operations_per_flight: 64,
                    generations_per_flight: limits.generations_per_flight,
                },
            )
            .unwrap(),
        );
        let page = page_id();
        let origin = Box::leak(Box::new(OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        }));
        Self {
            flights,
            quotas,
            table: CoreTable::new(limits, split, uring_runtime::environment::now()),
            count: Count::default(),
            wakes: Vec::new(),
            origin,
            page,
            next_request: 0,
            operations: Vec::new(),
        }
    }

    fn now() -> Instant {
        uring_runtime::environment::now()
    }

    /// Join a new caller on both sides. Returns the outcome and the caller
    /// when both registered a waiter.
    fn join(&mut self, ttl: Duration, attempts: u32) -> (Obs, Option<Caller>) {
        let now = Self::now();
        let deadline = now + ttl;
        self.next_request = self.next_request.wrapping_add(1);
        let scope: &'static RequestScope = Box::leak(Box::new(
            RequestScope::new(RequestId([self.next_request; 16]), deadline).unwrap(),
        ));
        let budget: &'static mut AcquisitionBudget =
            Box::leak(Box::new(AcquisitionBudget::new(deadline, attempts, 8)));
        let membership = std::sync::Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        let prod = self
            .flights
            .join(self.page.clone(), membership, self.origin, scope, budget);
        let core = self.table.join(
            &self.page,
            now,
            deadline,
            attempts,
            true,
            &mut self.count,
            &mut self.wakes,
        );
        let prod_obs = match &prod {
            Ok(JoinedFlight::Waiter(_)) => Obs::Waiter,
            Ok(JoinedFlight::Complete(page)) => {
                Obs::Complete(page.plaintext.inner.storage.bytes[0])
            }
            Ok(JoinedFlight::Ciphertext(_)) => Obs::Partial,
            Err(e) => Obs::Err(format!("{e:?}")),
        };
        let core_obs = match &core {
            Ok(core::Joined::Waiter(_)) => Obs::Waiter,
            Ok(core::Joined::Complete(v)) => Obs::Complete(*v),
            Ok(core::Joined::Partial(())) => Obs::Partial,
            Err(e) => Obs::Err(format!("{e:?}")),
        };
        assert_eq!(prod_obs, core_obs, "join");
        let caller = match (prod, core) {
            (Ok(JoinedFlight::Waiter(waiter)), Ok(core::Joined::Waiter(id))) => Some(Caller {
                prod: Some(waiter),
                leader: None,
                scope,
                core: id,
            }),
            _ => None,
        };
        (prod_obs, caller)
    }

    fn poll(&mut self, caller: &mut Caller) -> Obs {
        let (prod, leader) = poll_prod(caller.prod.as_mut().unwrap());
        let core = match self.table.poll(
            &self.page,
            caller.core,
            Self::now(),
            caller.core,
            &mut self.wakes,
        ) {
            Ok(core::Event::Lead) => Obs::Lead,
            Ok(core::Event::Complete(v)) => Obs::Complete(v),
            Ok(core::Event::Failed(e)) => Obs::Failed(format!("{e:?}")),
            Ok(core::Event::Pending) => Obs::Pending,
            Err(e) => Obs::Err(format!("{e:?}")),
        };
        assert_eq!(prod, core, "poll");
        if let Some(leader) = leader {
            caller.leader = Some(leader);
        }
        prod
    }

    fn cancel(&mut self, caller: &mut Caller) {
        caller.scope.cancel().unwrap();
        self.table.cancel(&self.page, caller.core, &mut self.wakes);
    }

    /// Detach: production drops the waiter (and an unfinished leader, which
    /// revokes it); core cancels a leader and detaches.
    fn detach(&mut self, mut caller: Caller) {
        if caller.leader.is_some() {
            self.table.abandon(&self.page, caller.core, &mut self.wakes);
        }
        drop(caller.leader.take());
        drop(caller.prod.take());
        self.table
            .detach(&self.page, caller.core, &mut self.count, &mut self.wakes);
    }

    fn publish(&mut self, caller: &mut Caller, value: u8) {
        let leader = caller.leader.take().unwrap();
        let page = result(&self.flights, self.page.clone(), value);
        let prod = self.flights.publish(leader, page).map(|_| ());
        let core = self.table.finish(
            &self.page,
            caller.core,
            Outcome::Published(value),
            &mut self.wakes,
        );
        settle(prod, core);
    }

    fn fail(&mut self, caller: &mut Caller) {
        self.fail_with(caller, Error::Io, racer_core::read::Error::Io);
    }

    /// Fail with the elected caller's own scope error, which a handed-over
    /// acquisition turns into a retry for the adopter.
    fn fail_with(&mut self, caller: &mut Caller, prod: Error, core: racer_core::read::Error) {
        let leader = caller.leader.take().unwrap();
        let prod = self
            .flights
            .fail(leader, AcquisitionFailure::Terminal(prod))
            .map(|_| ());
        let core = self.table.finish(
            &self.page,
            caller.core,
            Outcome::Failed(core),
            &mut self.wakes,
        );
        settle(prod, core);
    }

    /// Allow `caller`'s running acquisition to be handed over on both sides.
    fn allow_handover(&mut self, caller: &Caller) {
        let leader = caller.leader.as_ref().unwrap();
        let prod = self.flights.allow_handover(leader);
        let core = self.table.allow_handover(&self.page, caller.core);
        settle(prod, core);
    }

    /// Whether both tables report the acquisition handed over.
    fn handed_over(&mut self) -> bool {
        let prod = self.flights.handed_over(&self.page);
        self.table.refresh(&self.page, Self::now(), &mut self.wakes);
        assert_eq!(prod, self.table.handed_over(&self.page), "handed over");
        prod
    }

    fn reject(&mut self, caller: &mut Caller) {
        let leader = caller.leader.take().unwrap();
        let prod = self
            .flights
            .fail(leader, AcquisitionFailure::OriginRejected)
            .map(|_| ());
        let core = self.table.reject(
            &self.page,
            caller.core,
            racer_core::read::Error::OriginRejected,
            &mut self.wakes,
        );
        settle(prod, core);
    }

    /// Retain one operation for `caller`'s leadership on both sides (the fill
    /// driver's `retain_operation` before submission). Both accept an active
    /// leader and reject a revoked one. Returns whether both retained.
    fn retain(&mut self, caller: &Caller) -> bool {
        let leader = caller.leader.as_ref().unwrap();
        let prod = self.flights.retain_operation(
            leader,
            crate::telemetry::Metrics::default()
                .lease(crate::telemetry::Gauge::ActiveFills)
                .unwrap(),
        );
        let core = self.table.retain_operation(&self.page, caller.core);
        assert_eq!(
            core.is_ok(),
            self.table.is_leader(&self.page, caller.core),
            "core retain"
        );
        match prod {
            Ok(operation) => {
                assert_eq!(core, Ok(()), "retain");
                self.operations.push(operation);
                true
            }
            Err(e) => {
                settle(Err(e), core);
                false
            }
        }
    }

    /// Complete the retained operation at `index` on both sides.
    fn complete(&mut self, index: usize) {
        let operation = self.operations.swap_remove(index);
        operation.complete().unwrap();
        self.table
            .complete_operation(&self.page, &mut self.count, &mut self.wakes)
            .unwrap();
    }

    fn refresh(&mut self) {
        self.table.refresh(&self.page, Self::now(), &mut self.wakes);
    }

    /// Admission and table occupancy agree.
    fn check(&self) {
        assert_eq!(
            self.quotas.used(ResourceClass::Waiter),
            self.count.waiters,
            "waiter quota"
        );
        assert_eq!(
            self.quotas.used(ResourceClass::Flight),
            self.count.flights,
            "flight quota"
        );
        assert_eq!(self.table.waiters(&self.page), self.count.waiters);
    }
}

/// Production `Error` and core `Error` print the same variant names.
#[test]
fn error_names_match() {
    assert_eq!(
        format!("{:?}", Error::Overloaded),
        format!("{:?}", racer_core::read::Error::Overloaded)
    );
}

/// Seeded join, poll, cancel, deadline, detach, publish, fail, and origin
/// rejection traces on one page: every step has the same observable outcome
/// and the same waiter and flight admission on both sides. Covers terminal
/// waiter replacement before completion (a waiter cap forces `Overloaded`
/// until a canceled or expired waiter detaches), last-waiter detach, and
/// leader re-election after cancellation and origin rejection.
#[test]
fn flights_match_page_flights_on_seeded_traces() {
    let mut steps = std::collections::BTreeMap::<&str, usize>::new();
    for seed in 0..48u64 {
        let clock = uring_runtime::environment::SimulationClock::new(900 + seed);
        let _environment = clock.environment(0).enter();
        let mut rng = SplitMix64::new(seed);
        let mut pair = Pair::new(core::FlightLimits {
            entries: 4,
            waiters_per_flight: 3,
            generations_per_flight: 1024,
        });
        let mut callers: Vec<Caller> = Vec::new();
        let mut value = 0u8;
        for _ in 0..60 {
            let leader = callers.iter().position(|c| c.leader.is_some());
            match rng.below(10) {
                0 | 1 => {
                    let ttl = if rng.below(3) == 0 {
                        Duration::from_secs(1)
                    } else {
                        Duration::from_secs(60)
                    };
                    let attempts = rng.below(3) as u32;
                    let (obs, caller) = pair.join(ttl, attempts);
                    *steps
                        .entry(match obs {
                            Obs::Err(_) => "join_rejected",
                            Obs::Complete(_) => "join_complete",
                            _ => "join",
                        })
                        .or_default() += 1;
                    callers.extend(caller);
                }
                2 | 3 if !callers.is_empty() => {
                    let i = rng.below(callers.len() as u64) as usize;
                    if callers[i].leader.is_none() {
                        let obs = pair.poll(&mut callers[i]);
                        *steps
                            .entry(match obs {
                                Obs::Lead => "lead",
                                Obs::Failed(_) => "failed",
                                Obs::Complete(_) => "complete",
                                _ => "poll",
                            })
                            .or_default() += 1;
                    }
                }
                4 if !callers.is_empty() => {
                    let i = rng.below(callers.len() as u64) as usize;
                    *steps.entry("cancel").or_default() += 1;
                    let mut caller = callers.swap_remove(i);
                    pair.cancel(&mut caller);
                    if caller.leader.is_none() {
                        pair.poll(&mut caller);
                    }
                    pair.detach(caller);
                }
                5 if !callers.is_empty() => {
                    let i = rng.below(callers.len() as u64) as usize;
                    *steps.entry("detach").or_default() += 1;
                    pair.detach(callers.swap_remove(i));
                }
                6 if leader.is_some() => {
                    let caller = &mut callers[leader.unwrap()];
                    match rng.below(4) {
                        0 => {
                            value += 1;
                            pair.publish(caller, value);
                            *steps.entry("publish").or_default() += 1;
                        }
                        1 => {
                            pair.fail(caller);
                            *steps.entry("fail").or_default() += 1;
                        }
                        2 => {
                            let handed = pair.handed_over();
                            pair.fail_with(
                                caller,
                                Error::DeadlineExceeded,
                                racer_core::read::Error::DeadlineExceeded,
                            );
                            *steps
                                .entry(if handed {
                                    "fail_handed_over"
                                } else {
                                    "fail_deadline"
                                })
                                .or_default() += 1;
                        }
                        _ => {
                            pair.reject(caller);
                            *steps.entry("reject").or_default() += 1;
                        }
                    }
                }
                7 => {
                    clock.advance(Duration::from_millis(600));
                    pair.refresh();
                    *steps.entry("advance").or_default() += 1;
                }
                8 if leader.is_some() && pair.operations.len() < 3 => {
                    let caller = &callers[leader.unwrap()];
                    let retained = pair.retain(caller);
                    *steps
                        .entry(if retained { "retain" } else { "retain_stale" })
                        .or_default() += 1;
                    if retained && rng.below(4) != 0 {
                        pair.allow_handover(caller);
                        *steps.entry("allow_handover").or_default() += 1;
                    }
                }
                9 if !pair.operations.is_empty() => {
                    let i = rng.below(pair.operations.len() as u64) as usize;
                    pair.complete(i);
                    *steps.entry("complete_operation").or_default() += 1;
                }
                _ => {}
            }
            if pair.handed_over() {
                *steps.entry("handed_over").or_default() += 1;
            }
            pair.check();
        }
        while !pair.operations.is_empty() {
            pair.complete(0);
        }
        for caller in callers.drain(..) {
            pair.detach(caller);
        }
        pair.check();
        assert_eq!(pair.count.waiters, 0);
        assert_eq!(pair.count.flights, 0, "seed {seed}");
        assert!(pair.table.is_empty());
    }
    for step in [
        "join",
        "join_rejected",
        "join_complete",
        "lead",
        "failed",
        "complete",
        "cancel",
        "detach",
        "publish",
        "fail",
        "reject",
        "advance",
        "retain",
        "complete_operation",
        "allow_handover",
        "handed_over",
    ] {
        assert!(
            steps.get(step).copied().unwrap_or(0) > 0,
            "{step}: {steps:?}"
        );
    }
}

/// Known issue 122: a leader whose deadline passes while its retained
/// operation runs is revoked; its detach keeps the entry, a new caller waits
/// rather than leading a concurrent acquisition, the stale publish is rejected
/// by both tables, and only completing the operation elects the new caller.
#[test]
fn expired_leader_with_retained_operation_drains_before_reelection() {
    let clock = uring_runtime::environment::SimulationClock::new(122);
    let _environment = clock.environment(0).enter();
    let mut pair = Pair::new(core::FlightLimits::default());
    let mut a = pair.join(Duration::from_secs(1), 2).1.unwrap();
    assert_eq!(pair.poll(&mut a), Obs::Lead);
    assert!(pair.retain(&a));
    clock.advance(Duration::from_secs(1));
    pair.refresh();
    assert_eq!(pair.poll(&mut a), Obs::Failed("DeadlineExceeded".into()));
    // The driver still holds the leader capability; only the waiter detaches.
    let leader = a.leader.take();
    let a_core = a.core;
    pair.detach(a);
    pair.check();
    assert_eq!(pair.count.flights, 1, "entry kept while the operation runs");
    let mut b = pair.join(Duration::from_secs(60), 2).1.unwrap();
    assert_eq!(pair.poll(&mut b), Obs::Pending);
    let mut stale = Caller {
        prod: None,
        leader,
        scope: b.scope,
        core: a_core,
    };
    assert!(!pair.retain(&stale), "a revoked leader cannot retain");
    pair.publish(&mut stale, 1);
    assert_eq!(pair.poll(&mut b), Obs::Pending);
    pair.complete(0);
    assert_eq!(pair.poll(&mut b), Obs::Lead);
    pair.publish(&mut b, 2);
    assert_eq!(pair.poll(&mut b), Obs::Complete(2));
    pair.detach(b);
    pair.check();
    assert!(pair.table.is_empty());
}

/// Known issue 161: an expired leader's running operation passes to the
/// live waiter with the latest deadline on both tables; the elected
/// capability's deadline failure then retries for the adopter, which leads
/// with its own budget; abandoning a handed-over acquisition revokes it and
/// fails the elected caller, not the adopter.
#[test]
fn expired_leader_hands_over_running_operation_on_both_tables() {
    let clock = uring_runtime::environment::SimulationClock::new(161);
    let _environment = clock.environment(0).enter();
    let mut pair = Pair::new(core::FlightLimits::default());
    let mut a = pair.join(Duration::from_secs(1), 2).1.unwrap();
    let mut b = pair.join(Duration::from_secs(60), 2).1.unwrap();
    let mut c = pair.join(Duration::from_secs(30), 2).1.unwrap();
    assert_eq!(pair.poll(&mut a), Obs::Lead);
    assert!(pair.retain(&a));
    pair.allow_handover(&a);
    assert!(!pair.handed_over());
    clock.advance(Duration::from_secs(1));
    pair.refresh();
    assert!(pair.handed_over());
    assert_eq!(pair.poll(&mut b), Obs::Pending);
    assert_eq!(pair.poll(&mut c), Obs::Pending);
    pair.complete(0);
    pair.fail_with(
        &mut a,
        Error::DeadlineExceeded,
        racer_core::read::Error::DeadlineExceeded,
    );
    assert_eq!(pair.poll(&mut a), Obs::Failed("DeadlineExceeded".into()));
    // Both remain eligible; c polls first and leads with its own budget.
    assert_eq!(pair.poll(&mut c), Obs::Lead);
    assert!(pair.retain(&c));
    pair.allow_handover(&c);
    pair.cancel(&mut c);
    assert!(pair.handed_over(), "b adopts c's operation");
    // Dropping the driver's capability abandons the work: it is revoked.
    pair.detach(c);
    assert!(!pair.handed_over());
    assert_eq!(pair.poll(&mut b), Obs::Pending, "b is still eligible");
    pair.complete(0);
    assert_eq!(pair.poll(&mut b), Obs::Lead);
    pair.publish(&mut b, 4);
    assert_eq!(pair.poll(&mut b), Obs::Complete(4));
    pair.detach(a);
    pair.detach(b);
    pair.check();
    assert!(pair.table.is_empty());
}

/// Known issue 8 (resolved): production `Flights::fail(OriginRejected)`
/// fails the supplying caller, which must not be served a page fetched with
/// another caller's credentials, and re-elects the next eligible waiter. Core
/// `PageFlights::reject` does the same; both keep failing the rejected caller
/// after another caller publishes.
#[test]
fn origin_rejection_fails_only_the_rejected_caller() {
    let clock = uring_runtime::environment::SimulationClock::new(77);
    let _environment = clock.environment(0).enter();
    let mut pair = Pair::new(core::FlightLimits::default());
    let mut a = pair.join(Duration::from_secs(60), 2).1.unwrap();
    let mut b = pair.join(Duration::from_secs(60), 2).1.unwrap();
    assert_eq!(pair.poll(&mut a), Obs::Lead);
    assert_eq!(pair.poll(&mut b), Obs::Pending);
    pair.reject(&mut a);
    assert_eq!(pair.poll(&mut b), Obs::Lead);
    // `Pair::poll` asserts production and core agree.
    assert_eq!(pair.poll(&mut a), Obs::Failed("OriginRejected".into()));
    pair.publish(&mut b, 9);
    assert_eq!(pair.poll(&mut a), Obs::Failed("OriginRejected".into()));
    assert_eq!(pair.poll(&mut b), Obs::Complete(9));
    pair.detach(a);
    pair.detach(b);
    pair.check();
    assert!(pair.table.is_empty());
}

/// The simulator's memory admission (`sim/src/node/world.rs`, `memory_insert`
/// and the memory-hit path of `serve_local`), restated over the same core
/// `TickLru` because those functions are private to the simulator.
struct SimMemory {
    lru: racer_core::cache::lru::TickLru<u32, u32>,
    cap: usize,
}

impl SimMemory {
    fn hit(&mut self, page: u32) -> bool {
        if self.lru.pages.contains_key(&page) {
            self.lru.touch(&page).unwrap();
            return true;
        }
        false
    }

    fn insert(&mut self, page: u32) {
        if self.lru.pages.contains_key(&page) {
            return;
        }
        while self.lru.len() >= self.cap {
            let Some(victim) = self.lru.victim(|_| true) else {
                break;
            };
            self.lru.remove(&victim);
        }
        self.lru.insert(page, 0).unwrap();
    }

    fn resident(&self) -> std::collections::BTreeSet<u32> {
        self.lru.pages.keys().copied().collect()
    }
}

struct ProdMemory {
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    cache: crate::memory::MemoryCache,
}

impl ProdMemory {
    fn new(cap: usize) -> Self {
        let admission = crate::memory::tests::admission(cap);
        let cache = crate::memory::MemoryCache::new(
            crate::memory::BufferPool::new(admission.clone()),
            crate::test_support::availability(),
        );
        Self { admission, cache }
    }

    fn id(page: u32) -> PageId {
        let mut id = page_id();
        id.version.etag = StrongEtag::test_value(&format!("v{page}"));
        id
    }

    fn hit(&self, page: u32) -> bool {
        // `get` touches a resident entry even when its plaintext is gone.
        let resident = self.cache.resident().contains(&Self::id(page));
        let _ = self.cache.get(&Self::id(page)).unwrap();
        resident
    }

    fn insert(&self, page: u32) {
        let bundle = crate::memory::tests::bundle(&self.admission, &format!("v{page}"));
        self.cache.publish(bundle).unwrap();
    }

    fn resident(&self, pages: u32) -> std::collections::BTreeSet<u32> {
        let resident = self.cache.resident();
        (0..pages)
            .filter(|p| resident.contains(&Self::id(*p)))
            .collect()
    }
}

/// Seeded hit and insert traces: production `MemoryCache` (entry cap, idle
/// victim from the reclamation cursor, touch on hit) keeps exactly the pages
/// the simulator's `TickLru` keeps, step by step. Replaces the old
/// `fair_reclamation_matches_metadata_trace_and_keeps_other_cache` oracle for
/// the single-cache order; quota-driven `evict_idle` and per-cache fair share
/// are not simulated (see the design doc).
#[test]
fn memory_cache_matches_sim_lru_on_seeded_traces() {
    const PAGES: u32 = 12;
    let mut evictions = 0;
    for seed in 0..32u64 {
        let mut rng = SplitMix64::new(seed);
        let cap = 2 + rng.below(4) as usize;
        let prod = ProdMemory::new(cap);
        let mut sim = SimMemory {
            lru: Default::default(),
            cap,
        };
        for step in 0..80 {
            let page = rng.below(u64::from(PAGES)) as u32;
            let before = sim.resident();
            if rng.below(2) == 0 {
                assert_eq!(prod.hit(page), sim.hit(page), "seed {seed} step {step}");
            } else {
                // Includes duplicate publishes of resident pages.
                prod.insert(page);
                sim.insert(page);
                evictions += usize::from(before.len() == cap && !before.contains(&page));
            }
            assert_eq!(
                prod.resident(PAGES),
                sim.resident(),
                "seed {seed} step {step}"
            );
        }
    }
    assert!(evictions > 100, "{evictions}");
}

/// Known issue 9 (resolved): a duplicate publish of a resident page does
/// not touch it in production (`MemoryCache::publish`) or in the simulator
/// (`memory_insert` in `sim/src/node/world.rs`), so both evict the oldest
/// insert.
#[test]
fn duplicate_publish_does_not_touch() {
    let prod = ProdMemory::new(2);
    let mut sim = SimMemory {
        lru: Default::default(),
        cap: 2,
    };
    for page in [0, 1, 0, 2] {
        prod.insert(page);
        sim.insert(page);
    }
    assert_eq!(prod.resident(3), [1, 2].into());
    assert_eq!(sim.resident(), [1, 2].into());
}
