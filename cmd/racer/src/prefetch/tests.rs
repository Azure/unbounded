//! Registry and owner-budget rules without I/O.

use super::Priority;
use super::api::{self, Reason, RefError, RefOutcome, State};
use super::registry::{PutCheck, Registry, Resolution, RetryKind, UnitKey, UnitOutcome};
use super::{Config, metrics::Counter, metrics::Outcome};
use crate::model::{CacheKey, ObjectId, ObjectVersion, PAGE_BYTES, StrongEtag, VersionMetadata};
use racer_wire::control::{CacheId, NodeId};
use racer_wire::object::prefetch::{ErrorCode, RefsFilter};
use std::time::{Duration, Instant};

const KEY_A: &str = "aa00000000000000000000000000000000000000000000000000000000000000";
const KEY_B: &str = "bb00000000000000000000000000000000000000000000000000000000000000";

fn cache() -> CacheId {
    CacheId("5b0c7a5e-2f7d-4b8e-9d0e-6f6f2f3b1a10".into())
}

fn version(key: &str, etag: &str) -> VersionMetadata {
    VersionMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: cache(),
                key: CacheKey::parse_hex(key.as_bytes()).unwrap(),
            },
            etag: StrongEtag::test_value(etag),
        },
        length: 4 * PAGE_BYTES,
    }
}

fn body(extra: &str, refs: &[&str]) -> String {
    let refs = refs
        .iter()
        .map(|r| format!("{{{r}}}"))
        .collect::<Vec<_>>()
        .join(",");
    format!("{{{extra}\"refs\":[{refs}]}}")
}

fn spec(extra: &str, refs: &[&str]) -> api::Spec {
    api::parse_spec(body(extra, refs).as_bytes(), false, &Config::default()).unwrap()
}

fn whole(key: &str) -> String {
    format!("\"key\":\"{key}\"")
}

fn pages(key: &str, first: u64, count: u64) -> String {
    format!(
        "\"key\":\"{key}\",\"offset\":{},\"length\":{}",
        first * PAGE_BYTES,
        count * PAGE_BYTES
    )
}

struct Rig {
    registry: Registry,
    now: Instant,
    a: VersionMetadata,
    b: VersionMetadata,
}

#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
fn rig(config: Config) -> Rig {
    Rig {
        registry: Registry::new(config, 7, racer_io::Clock::host()),
        now: Instant::now(),
        a: version(KEY_A, "a1"),
        b: version(KEY_B, "b1"),
    }
}

impl Rig {
    fn resolve(&self, spec: &api::Spec) -> Vec<Resolution> {
        spec.refs
            .iter()
            .map(|r| {
                let hex = r.key.to_hex();
                Resolution::Version(if hex == KEY_A {
                    self.a.clone()
                } else {
                    self.b.clone()
                })
            })
            .collect()
    }

    fn put(&mut self, id: &str, spec: api::Spec) -> Result<(u16, api::Status), ErrorCode> {
        match self
            .registry
            .check_put(&cache(), id, &spec, &None, self.now)
        {
            Err(e) => return Err(e.code),
            Ok(PutCheck::Replay(status)) => return Ok((200, *status)),
            Ok(PutCheck::Proceed) => {}
        }
        let resolved = self.resolve(&spec);
        self.registry
            .apply_put(&cache(), id, spec, resolved, None, self.now)
            .map_err(|e| e.code)
    }

    fn status(&self, id: &str) -> api::Status {
        self.registry.status(&cache(), id, None, None).unwrap()
    }

    /// Dispatch everything ready and answer each page with `answer`.
    fn drain(&mut self, mut answer: impl FnMut(&UnitKey) -> UnitOutcome) -> Vec<UnitKey> {
        let mut seen = Vec::new();
        while let Some(batch) = self.registry.next_batch(self.now, 256) {
            for page in batch.pages {
                let unit = UnitKey {
                    version: batch.metadata.version.clone(),
                    page,
                };
                let outcome = answer(&unit);
                self.registry
                    .complete(&unit, outcome, Some(node("owner")), self.now);
                seen.push(unit);
            }
        }
        seen
    }
}

fn node(name: &str) -> NodeId {
    NodeId(name.into())
}

#[test]
fn idempotency_and_ordering_table() {
    let mut r = rig(Config::default());
    let first = spec("\"generation\":1,\"sequence\":1,", &[&whole(KEY_A)]);
    assert_eq!(r.put("t", first.clone()).unwrap().0, 201);
    // Same (g, s) and payload: the current status, 200.
    assert_eq!(r.put("t", first.clone()).unwrap().0, 200);
    // Same (g, s), another payload.
    let other = spec("\"generation\":1,\"sequence\":1,", &[&whole(KEY_B)]);
    assert_eq!(r.put("t", other).unwrap_err(), ErrorCode::PayloadMismatch);
    // Older sequence and generation.
    let older = spec("\"generation\":1,\"sequence\":0,", &[&whole(KEY_A)]);
    assert_eq!(r.put("t", older).unwrap_err(), ErrorCode::StaleSequence);
    let older = spec("\"generation\":0,\"sequence\":9,", &[&whole(KEY_A)]);
    assert_eq!(r.put("t", older).unwrap_err(), ErrorCode::StaleGeneration);
    // Newer sequence replaces; newer generation supersedes.
    let newer = spec("\"generation\":1,\"sequence\":2,", &[&whole(KEY_B)]);
    let (code, status) = r.put("t", newer).unwrap();
    assert_eq!((code, status.sequence), (200, 2));
    let reset = spec("\"generation\":2,\"sequence\":0,", &[&whole(KEY_A)]);
    let (code, status) = r.put("t", reset).unwrap();
    assert_eq!((code, status.generation, status.sequence), (200, 2, 0));
    // A mismatched cache UID is refused before anything else.
    let wrong = spec("\"cache_uid\":\"other\",", &[&whole(KEY_A)]);
    assert_eq!(r.put("u", wrong).unwrap_err(), ErrorCode::CacheUidMismatch);
    let (events, _) = r.registry.take_events();
    assert!(
        events
            .iter()
            .any(|(c, _, n)| *c == Counter::WindowUpdates && *n == 1)
    );
}

/// Refs are cut at the cluster page size, so a 16 MiB range of a
/// 64 KiB-page cluster is 256 units and every byte count uses 64 KiB.
#[test]
fn refs_are_cut_at_the_cluster_page_size() {
    let small = crate::model::PageSize::new(64 << 10).unwrap();
    let mut r = rig(Config::default());
    r.registry.set_page_size(small);
    let offset = small.get() - 1;
    let (code, status) = r
        .put(
            "t",
            spec(
                "",
                &[&format!(
                    "\"key\":\"{KEY_A}\",\"offset\":{offset},\"length\":{}",
                    small.get() + 2
                )],
            ),
        )
        .unwrap();
    assert_eq!(code, 201);
    // Bytes small-1 ..= 2*small cover pages 0, 1, and 2.
    assert_eq!(status.bytes.aligned_requested, 3 * small.get());
    assert_eq!(r.registry.pending_bytes(), 3 * small.get());
    let units = r.drain(|_| UnitOutcome::Admitted);
    assert_eq!(
        units.iter().map(|u| u.page).collect::<Vec<_>>(),
        vec![0, 1, 2]
    );
    let status = r.status("t");
    assert_eq!(status.state, State::Succeeded);
    assert_eq!(status.bytes.admitted, 3 * small.get());
    assert_eq!(r.registry.pending_bytes(), 0);
}

#[test]
fn batch_succeeds_and_reports_bytes() {
    let mut r = rig(Config::default());
    let (code, status) = r
        .put("t", spec("", &[&pages(KEY_A, 0, 2), &pages(KEY_A, 1, 2)]))
        .unwrap();
    assert_eq!(code, 201);
    assert_eq!(status.state, State::Queued);
    assert!(!status.settled);
    assert_eq!(status.bytes.logical_requested, 4 * PAGE_BYTES);
    // Duplicate pages are deduplicated.
    assert_eq!(status.bytes.aligned_requested, 3 * PAGE_BYTES);
    assert_eq!(status.bytes.queued, 3 * PAGE_BYTES);
    let mut first = true;
    r.drain(|_| {
        let resident = first;
        first = false;
        if resident {
            UnitOutcome::Resident
        } else {
            UnitOutcome::Admitted
        }
    });
    let status = r.status("t");
    assert_eq!(status.state, State::Succeeded);
    assert!(status.settled);
    assert_eq!(status.refs_succeeded, 2);
    assert_eq!(status.bytes.already_resident, PAGE_BYTES);
    assert_eq!(status.bytes.fetched_from_origin, 2 * PAGE_BYTES);
    assert_eq!(status.bytes.admitted, 3 * PAGE_BYTES);
    assert_eq!(status.bytes.queued + status.bytes.in_flight, 0);
    assert_eq!(r.registry.pending_bytes(), 0);
    let (_, finished) = r.registry.take_events();
    assert_eq!(finished, vec![(Priority::Normal, Outcome::Succeeded)]);
}

#[test]
fn version_change_fails_only_that_ref_and_gives_partial() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 1), &pages(KEY_B, 0, 1)]))
        .unwrap();
    let b = r.b.version.clone();
    r.drain(|unit| {
        if unit.version == b {
            UnitOutcome::Failed(RefError::VersionChanged)
        } else {
            UnitOutcome::Admitted
        }
    });
    let status = r.status("t");
    assert_eq!(status.state, State::Partial);
    assert_eq!((status.refs_succeeded, status.refs_failed), (1, 1));
    assert_eq!(status.bytes.failed, PAGE_BYTES);
    let listed = r
        .registry
        .status(&cache(), "t", Some(RefsFilter::Failed), None)
        .unwrap();
    let refs = listed.refs.unwrap();
    assert_eq!(refs.len(), 1);
    assert_eq!(refs[0].index, 1);
    assert_eq!(refs[0].outcome, RefOutcome::Failed);
    assert_eq!(refs[0].error, Some(RefError::VersionChanged));
    assert_eq!(listed.next_cursor, Some(None));
    // Every ref failing is FAILED.
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    r.drain(|_| UnitOutcome::Failed(RefError::NotFound));
    assert_eq!(r.status("t").state, State::Failed);
}

#[test]
fn resolution_failures_and_invalid_ranges_fail_refs_without_failing_the_put() {
    let mut r = rig(Config::default());
    let spec = spec(
        "",
        &[
            &whole(KEY_A),
            &format!("\"key\":\"{KEY_A}\",\"offset\":{}", 4 * PAGE_BYTES),
        ],
    );
    let resolved = vec![
        Resolution::Failed(RefError::Forbidden),
        Resolution::Version(r.a.clone()),
    ];
    let (code, status) = r
        .registry
        .apply_put(&cache(), "t", spec, resolved, None, r.now)
        .unwrap();
    assert_eq!(code, 201);
    assert_eq!(status.state, State::Failed);
    assert!(status.settled);
    let refs = r
        .registry
        .status(&cache(), "t", Some(RefsFilter::All), None)
        .unwrap()
        .refs
        .unwrap();
    assert_eq!(refs[0].error, Some(RefError::Forbidden));
    assert_eq!(refs[0].etag, None);
    assert_eq!(refs[1].error, Some(RefError::InvalidRange));
    assert_eq!(refs[1].etag.as_deref(), Some("\"a1\""));
}

#[test]
fn units_are_shared_across_tickets_and_joins_are_counted() {
    let mut r = rig(Config::default());
    r.put("one", spec("", &[&pages(KEY_A, 0, 2)])).unwrap();
    r.put("two", spec("", &[&pages(KEY_A, 1, 2)])).unwrap();
    let shared = UnitKey {
        version: r.a.version.clone(),
        page: 1,
    };
    assert_eq!(r.registry.interest(&shared), Some(2));
    assert_eq!(r.registry.pending_bytes(), 3 * PAGE_BYTES);
    let dispatched = r.drain(|_| UnitOutcome::Admitted);
    assert_eq!(dispatched.len(), 3, "each page is dispatched once");
    let one = r.status("one");
    let two = r.status("two");
    assert_eq!(one.bytes.admitted + two.bytes.admitted, 4 * PAGE_BYTES);
    assert_eq!(
        one.bytes.joined + two.bytes.joined,
        PAGE_BYTES,
        "the shared page joined the other ticket's fill"
    );
}

#[test]
fn strict_priority_then_round_robin_then_ref_order() {
    let mut r = rig(Config::default());
    r.put("low", spec("\"priority\":\"low\",", &[&pages(KEY_A, 0, 1)]))
        .unwrap();
    r.put("n1", spec("", &[&pages(KEY_A, 1, 1), &pages(KEY_A, 2, 1)]))
        .unwrap();
    r.put("n2", spec("", &[&pages(KEY_B, 0, 1), &pages(KEY_B, 1, 1)]))
        .unwrap();
    r.put(
        "high",
        spec("\"priority\":\"high\",", &[&pages(KEY_A, 3, 1)]),
    )
    .unwrap();
    let mut order = Vec::new();
    while let Some(batch) = r.registry.next_batch(r.now, 1) {
        order.push((batch.dispatcher.1.clone(), batch.pages[0]));
    }
    assert_eq!(
        order,
        vec![
            ("high".into(), 3),
            ("n1".into(), 1),
            ("n2".into(), 0),
            ("n1".into(), 2),
            ("n2".into(), 1),
            ("low".into(), 0),
        ]
    );
}

#[test]
fn per_ticket_pending_bound_limits_in_flight_bytes() {
    let mut r = rig(Config::default());
    let bound = 2 * PAGE_BYTES;
    r.put(
        "t",
        spec(
            &format!("\"max_pending_bytes\":{bound},"),
            &[&pages(KEY_A, 0, 4)],
        ),
    )
    .unwrap();
    let first = r.registry.next_batch(r.now, 256).unwrap();
    assert_eq!(first.pages, vec![0, 1]);
    assert!(r.registry.next_batch(r.now, 256).is_none());
    assert_eq!(r.status("t").bytes.in_flight, bound);
    let unit = UnitKey {
        version: r.a.version.clone(),
        page: 0,
    };
    r.registry
        .complete(&unit, UnitOutcome::Admitted, None, r.now);
    let next = r.registry.next_batch(r.now, 256).unwrap();
    assert_eq!(next.pages, vec![2]);
}

#[test]
fn window_replace_drops_queued_interest_but_not_in_flight_fills() {
    let mut r = rig(Config::default());
    r.put(
        "w",
        spec(
            "\"sequence\":0,\"max_pending_bytes\":16777216,",
            &[&pages(KEY_A, 0, 3)],
        ),
    )
    .unwrap();
    let batch = r.registry.next_batch(r.now, 256).unwrap();
    assert_eq!(batch.pages, vec![0]);
    // Slide the window: page 0 (in flight) and 1 (queued) leave; 3 joins.
    r.put(
        "w",
        spec(
            "\"sequence\":1,\"max_pending_bytes\":16777216,",
            &[&pages(KEY_A, 2, 2)],
        ),
    )
    .unwrap();
    let key = |page| UnitKey {
        version: r.a.version.clone(),
        page,
    };
    assert_eq!(r.registry.interest(&key(1)), None, "queued unit dropped");
    assert_eq!(r.registry.interest(&key(0)), Some(0), "in-flight fill kept");
    let (events, _) = r.registry.take_events();
    assert!(events.contains(&(Counter::WindowDroppedBytes, Priority::Normal, PAGE_BYTES)));
    let status = r.status("w");
    assert_eq!(status.state, State::Queued);
    assert_eq!(status.bytes.aligned_requested, 2 * PAGE_BYTES);
    // The in-flight fill completes normally and is then forgotten.
    r.registry
        .complete(&key(0), UnitOutcome::Admitted, None, r.now);
    assert_eq!(r.registry.interest(&key(0)), None);
    r.drain(|_| UnitOutcome::Admitted);
    let status = r.status("w");
    assert_eq!(status.state, State::Succeeded);
    // A settled window returns to running on its next update.
    r.put("w", spec("\"sequence\":2,", &[&pages(KEY_B, 0, 1)]))
        .unwrap();
    assert!(!r.status("w").settled);
}

#[test]
fn ttl_expiry_cancels_unsettled_tickets_and_retention_removes_them() {
    let config = Config {
        ticket_retention_secs: 10,
        ..Config::default()
    };
    let mut r = rig(config);
    r.put("t", spec("\"ttl_seconds\":5,", &[&pages(KEY_A, 0, 1)]))
        .unwrap();
    r.put("done", spec("\"ttl_seconds\":5,", &[&pages(KEY_B, 0, 1)]))
        .unwrap();
    let b = r.b.version.clone();
    while let Some(batch) = r.registry.next_batch(r.now, 256) {
        for page in batch.pages {
            let unit = UnitKey {
                version: batch.metadata.version.clone(),
                page,
            };
            if unit.version == b {
                r.registry
                    .complete(&unit, UnitOutcome::Admitted, None, r.now);
            }
        }
    }
    assert!(r.registry.next_deadline().is_some());
    r.registry.expire(r.now + Duration::from_secs(6));
    let status = r.status("t");
    assert_eq!(status.state, State::Canceled);
    assert_eq!(status.reason, Some(Reason::Expired));
    // A settled ticket keeps its outcome at expiry.
    assert_eq!(r.status("done").state, State::Succeeded);
    // Equal (g, s) and hash on a terminal ticket returns the retained status;
    // a higher (g, s) restarts it.
    let replay = spec("\"ttl_seconds\":5,", &[&pages(KEY_A, 0, 1)]);
    assert_eq!(r.put("t", replay).unwrap().1.state, State::Canceled);
    r.registry.expire(r.now + Duration::from_secs(17));
    assert_eq!(r.registry.ticket_count(), 0);
    let restart = spec("\"sequence\":1,", &[&pages(KEY_A, 0, 1)]);
    assert_eq!(r.put("t", restart).unwrap().0, 201);
}

#[test]
fn delete_cancels_and_restart_needs_higher_sequence() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 2)])).unwrap();
    let batch = r.registry.next_batch(r.now, 1).unwrap();
    let status = r.registry.delete(&cache(), "t", r.now).unwrap();
    assert_eq!(status.state, State::Canceled);
    assert_eq!(status.reason, Some(Reason::Client));
    assert!(status.settled);
    // The queued page is dropped; the in-flight fill completes normally.
    assert_eq!(r.registry.pending_bytes(), PAGE_BYTES);
    let unit = UnitKey {
        version: batch.metadata.version.clone(),
        page: batch.pages[0],
    };
    r.registry
        .complete(&unit, UnitOutcome::Admitted, None, r.now);
    assert_eq!(r.registry.pending_bytes(), 0);
    assert!(r.registry.next_batch(r.now, 1).is_none());
    assert_eq!(
        r.registry
            .delete(&cache(), "missing", r.now)
            .unwrap_err()
            .code,
        ErrorCode::NotFound
    );
    let (code, status) = r
        .put("t", spec("\"sequence\":1,", &[&pages(KEY_A, 0, 1)]))
        .unwrap();
    assert_eq!((code, status.state), (200, State::Queued));
    let (_, finished) = r.registry.take_events();
    assert!(finished.contains(&(Priority::Normal, Outcome::Canceled)));
}

#[test]
fn retries_back_off_then_fail_with_the_mapped_error() {
    for (kind, error) in [
        (RetryKind::Deferred, RefError::BudgetTimeout),
        (RetryKind::Overloaded, RefError::BudgetTimeout),
        (RetryKind::Unavailable, RefError::OriginError),
        (RetryKind::Unsupported, RefError::PeerUnsupported),
    ] {
        let config = Config {
            max_attempts: 3,
            ..Config::default()
        };
        let mut r = rig(config);
        r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
        let mut attempts = 0;
        for _ in 0..10 {
            if r.status("t").settled {
                break;
            }
            let Some(batch) = r.registry.next_batch(r.now, 256) else {
                // Backing off: nothing is ready until the deadline passes.
                let at = r.registry.next_deadline().unwrap();
                assert!(at > r.now);
                assert!(at <= r.now + Duration::from_secs(10));
                r.now = at;
                continue;
            };
            attempts += 1;
            let unit = UnitKey {
                version: batch.metadata.version.clone(),
                page: batch.pages[0],
            };
            r.registry.complete(
                &unit,
                UnitOutcome::Retry(kind, Some(Duration::from_millis(250))),
                None,
                r.now,
            );
        }
        assert_eq!(attempts, 3, "{kind:?}");
        let refs = r
            .registry
            .status(&cache(), "t", Some(RefsFilter::Failed), None)
            .unwrap()
            .refs
            .unwrap();
        assert_eq!(refs[0].error, Some(error), "{kind:?}");
        let (events, _) = r.registry.take_events();
        let retries: u64 = events
            .iter()
            .filter(|(c, ..)| *c == Counter::Retries)
            .map(|(.., n)| n)
            .sum();
        assert_eq!(retries, 2);
        let deferrals = events.iter().any(|(c, ..)| *c == Counter::Deferrals);
        assert_eq!(deferrals, kind == RetryKind::Deferred);
    }
}

#[test]
fn deferral_honors_owner_retry_after() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    let batch = r.registry.next_batch(r.now, 256).unwrap();
    let unit = UnitKey {
        version: batch.metadata.version.clone(),
        page: 0,
    };
    r.registry.complete(
        &unit,
        UnitOutcome::Retry(RetryKind::Deferred, Some(Duration::from_secs(3))),
        None,
        r.now,
    );
    assert!(
        r.registry
            .next_batch(r.now + Duration::from_millis(2999), 256)
            .is_none()
    );
    assert!(
        r.registry
            .next_batch(r.now + Duration::from_secs(3), 256)
            .is_some()
    );
}

#[test]
fn membership_change_requeues_admitted_pages_of_departed_owners() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 2)])).unwrap();
    while let Some(batch) = r.registry.next_batch(r.now, 256) {
        for page in batch.pages {
            let unit = UnitKey {
                version: batch.metadata.version.clone(),
                page,
            };
            let owner = if page == 0 { "gone" } else { "stays" };
            r.registry
                .complete(&unit, UnitOutcome::Admitted, Some(node(owner)), r.now);
        }
    }
    assert_eq!(r.status("t").state, State::Succeeded);
    r.registry.requeue_departed(|_, n| n.0 == "stays");
    let status = r.status("t");
    assert!(!status.settled);
    assert_eq!(status.bytes.queued, PAGE_BYTES);
    let batch = r.registry.next_batch(r.now, 256).unwrap();
    assert_eq!(batch.pages, vec![0]);
}

#[test]
fn owners_that_leave_the_cache_pool_are_requeued() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 2)])).unwrap();
    while let Some(batch) = r.registry.next_batch(r.now, 256) {
        for page in batch.pages {
            let unit = UnitKey {
                version: batch.metadata.version.clone(),
                page,
            };
            r.registry
                .complete(&unit, UnitOutcome::Admitted, Some(node("owner")), r.now);
        }
    }
    assert_eq!(r.status("t").state, State::Succeeded);
    // Still a cluster member, still serving the cache: nothing moves.
    r.registry.requeue_departed(|_, _| true);
    assert!(r.status("t").settled);
    // Still a cluster member but no longer in this cache's pool.
    r.registry
        .requeue_departed(|cache_id, n| !(cache_id == &cache() && n.0 == "owner"));
    let status = r.status("t");
    assert!(!status.settled);
    assert_eq!(status.bytes.queued, 2 * PAGE_BYTES);
    let batch = r.registry.next_batch(r.now, 256).unwrap();
    assert_eq!(batch.pages, vec![0, 1]);
}

#[test]
fn fills_that_finish_after_their_owner_left_the_pool_are_requeued() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    let batch = r.registry.next_batch(r.now, 256).unwrap();
    let units: Vec<UnitKey> = batch
        .pages
        .iter()
        .map(|&page| UnitKey {
            version: batch.metadata.version.clone(),
            page,
        })
        .collect();
    let departed = |cache_id: &CacheId, n: &NodeId| !(cache_id == &cache() && n.0 == "owner");
    // The pool changes while the fill is in flight: the full scan skips it.
    r.registry.requeue_departed(departed);
    for unit in &units {
        r.registry
            .complete(unit, UnitOutcome::Admitted, Some(node("owner")), r.now);
    }
    assert!(r.status("t").settled);
    // The pump rechecks just-completed units against current membership.
    r.registry.requeue_departed_among(&units, departed);
    let status = r.status("t");
    assert!(!status.settled);
    assert_eq!(status.bytes.queued, PAGE_BYTES);
    assert_eq!(r.registry.next_batch(r.now, 256).unwrap().pages, vec![0]);
}

#[test]
fn cache_removal_and_passthrough_cancel_tickets() {
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    r.registry.cancel_caches(
        &std::collections::HashSet::from([cache()]),
        Reason::Passthrough,
        r.now,
    );
    assert_eq!(r.status("t").reason, Some(Reason::Passthrough));
    assert_eq!(r.registry.pending_bytes(), 0);
    let mut r = rig(Config::default());
    r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    r.registry.retain_caches(|_| true, r.now);
    assert_eq!(r.status("t").state, State::Queued);
    r.registry.retain_caches(|_| false, r.now);
    let status = r.status("t");
    assert_eq!(
        (status.state, status.reason),
        (State::Canceled, Some(Reason::CacheRemoved))
    );
}

#[test]
fn node_limits_answer_429_with_retry_after() {
    let config = Config {
        max_tickets: 1,
        node_pending_bytes: 16 * 1024 * 1024,
        ..Config::default()
    };
    let mut r = rig(config);
    r.put("t", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    let s = spec("", &[&pages(KEY_B, 0, 1)]);
    let resolved = r.resolve(&s);
    let error = r
        .registry
        .apply_put(&cache(), "u", s, resolved, None, r.now)
        .unwrap_err();
    assert_eq!(error.code, ErrorCode::PendingLimit);
    assert_eq!(error.retry_after, Some(5));
    r.drain(|_| UnitOutcome::Admitted);
    let s = spec("", &[&pages(KEY_B, 0, 1)]);
    assert_eq!(r.put("u", s).unwrap_err(), ErrorCode::TicketLimit);
    // A retained terminal ticket makes room.
    r.registry.delete(&cache(), "t", r.now).unwrap();
    let s = spec("", &[&pages(KEY_B, 0, 1)]);
    assert_eq!(r.put("u", s).unwrap().0, 201);
}

#[test]
fn refs_paging_uses_cursors_bound_to_the_window() {
    let mut r = rig(Config::default());
    let refs: Vec<String> = (0..300).map(|_| pages(KEY_A, 0, 1)).collect();
    let refs: Vec<&str> = refs.iter().map(String::as_str).collect();
    r.put("t", spec("\"sequence\":4,", &refs)).unwrap();
    let first = r
        .registry
        .status(&cache(), "t", Some(RefsFilter::All), None)
        .unwrap();
    assert_eq!(first.refs.as_ref().unwrap().len(), 256);
    let cursor = first.next_cursor.unwrap().unwrap();
    let second = r
        .registry
        .status(&cache(), "t", Some(RefsFilter::All), Some(&cursor))
        .unwrap();
    assert_eq!(second.refs.as_ref().unwrap().len(), 44);
    assert_eq!(second.refs.unwrap()[0].index, 256);
    assert_eq!(second.next_cursor, Some(None));
    r.put("t", spec("\"sequence\":5,", &refs)).unwrap();
    assert_eq!(
        r.registry
            .status(&cache(), "t", Some(RefsFilter::All), Some(&cursor))
            .unwrap_err()
            .code,
        ErrorCode::InvalidRequest
    );
}

#[test]
fn list_is_sorted_by_id_and_scoped_to_the_cache() {
    let mut r = rig(Config::default());
    r.put("b", spec("", &[&pages(KEY_A, 0, 1)])).unwrap();
    r.put("a", spec("", &[&pages(KEY_B, 0, 1)])).unwrap();
    let list = r.registry.list(&cache());
    assert_eq!(
        list.iter().map(|s| s.id.as_str()).collect::<Vec<_>>(),
        ["a", "b"]
    );
    assert!(r.registry.list(&CacheId("other".into())).is_empty());
    let json = serde_json::to_string(&api::List { prefetches: list }).unwrap();
    assert!(json.starts_with("{\"prefetches\":[{\"id\":\"a\""));
    assert!(!json.contains("coverage"));
}

#[test]
fn gauges_count_queued_bytes_and_live_tickets_per_priority() {
    let mut r = rig(Config::default());
    r.put("h", spec("\"priority\":\"high\",", &[&pages(KEY_A, 0, 2)]))
        .unwrap();
    r.put("l", spec("\"priority\":\"low\",", &[&pages(KEY_A, 1, 2)]))
        .unwrap();
    let (queued, tickets) = r.registry.gauges();
    // The shared page counts once, under the higher priority.
    assert_eq!(queued, [2 * PAGE_BYTES, 0, PAGE_BYTES]);
    assert_eq!(tickets, [1, 0, 1]);
}

#[test]
fn owner_promotion_releases_the_prefetch_slot() {
    let prefetch = super::service::Prefetch::new(racer_io::WorkerCtx::host());
    prefetch
        .configure(Config {
            concurrent_fills: 1,
            ..Config::default()
        })
        .unwrap();
    let page = crate::model::PageId {
        version: version(KEY_A, "a1").version,
        number: crate::model::PageNumber(0),
    };
    let other = crate::model::PageId {
        number: crate::model::PageNumber(1),
        ..page.clone()
    };
    let held = prefetch.owner_admit_for_test(&page, Priority::High);
    assert!(held.is_ok());
    assert_eq!(prefetch.active_fills(), 1);
    // A second fill is deferred while the slot is held.
    assert!(
        prefetch
            .owner_admit_for_test(&other, Priority::High)
            .is_err()
    );
    // Demand joins the prefetch flight and promotes it out of the budget.
    assert!(prefetch.promote(&page));
    assert!(!prefetch.promote(&page));
    assert_eq!(prefetch.active_fills(), 0);
    let other_fill = prefetch.owner_admit_for_test(&other, Priority::High);
    assert!(other_fill.is_ok());
    drop((held, other_fill));
    assert_eq!(prefetch.active_fills(), 0);
    assert_eq!(prefetch.filling_for_test(), 0);
}

/// Dropping an owner fill (for example a canceled `serve_fill` future)
/// releases its slot and bookkeeping; a joined admission of the same page
/// keeps the slot until it is dropped too.
#[test]
fn owner_fill_slot_is_released_on_drop_and_refcounted() {
    let prefetch = super::service::Prefetch::new(racer_io::WorkerCtx::host());
    prefetch
        .configure(Config {
            concurrent_fills: 1,
            ..Config::default()
        })
        .unwrap();
    let page = crate::model::PageId {
        version: version(KEY_A, "a1").version,
        number: crate::model::PageNumber(0),
    };
    let other = crate::model::PageId {
        number: crate::model::PageNumber(1),
        ..page.clone()
    };
    let first = prefetch
        .owner_admit_for_test(&page, Priority::High)
        .ok()
        .unwrap();
    let second = prefetch
        .owner_admit_for_test(&page, Priority::Low)
        .ok()
        .unwrap();
    drop(first);
    // The joined admission still holds the only slot.
    assert_eq!(prefetch.active_fills(), 1);
    assert!(
        prefetch
            .owner_admit_for_test(&other, Priority::High)
            .is_err()
    );
    drop(second);
    assert_eq!(prefetch.active_fills(), 0);
    assert_eq!(prefetch.filling_for_test(), 0);
    // A fill future dropped mid-flight releases its slot.
    let fill = async {
        let _fill = prefetch.owner_admit_for_test(&other, Priority::High);
        std::future::pending::<()>().await;
    };
    let mut fill = Box::pin(fill);
    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
    assert!(fill.as_mut().poll(&mut cx).is_pending());
    assert_eq!(prefetch.active_fills(), 1);
    drop(fill);
    assert_eq!(prefetch.active_fills(), 0);
    assert_eq!(prefetch.filling_for_test(), 0);
}

/// A long-poll keeps one waker however often it is polled, and removes it
/// when it ends.
#[test]
fn status_waiters_are_deduplicated_and_removed() {
    let prefetch = super::service::Prefetch::new(racer_io::WorkerCtx::host());
    let waker = futures::task::noop_waker();
    assert_eq!(prefetch.wait_for_test(&waker, 100), 1);
}

#[test]
fn owner_and_peer_records_map_to_unit_outcomes() {
    use super::service::{classify, outcome_of};
    use crate::error::Error;
    use racer_core::prefetch::{FailCode, PageResult};
    assert_eq!(outcome_of(PageResult::Admitted), UnitOutcome::Admitted);
    assert_eq!(outcome_of(PageResult::Resident), UnitOutcome::Resident);
    assert_eq!(
        outcome_of(PageResult::Deferred { retry_after_ms: 40 }),
        UnitOutcome::Retry(RetryKind::Deferred, Some(Duration::from_millis(40)))
    );
    assert_eq!(
        outcome_of(PageResult::Failed(FailCode::VersionChanged)),
        UnitOutcome::Failed(RefError::VersionChanged)
    );
    assert_eq!(
        outcome_of(PageResult::Failed(FailCode::Unavailable)),
        UnitOutcome::Retry(RetryKind::Unavailable, None)
    );
    // An old owner that cannot decode `prefetch` is retried, then reported
    // as peer_unsupported.
    for error in [
        Error::InvalidRequest,
        Error::CorruptRecord,
        Error::Unauthorized,
    ] {
        assert_eq!(
            classify(error),
            UnitOutcome::Retry(RetryKind::Unsupported, None)
        );
    }
    assert_eq!(
        classify(Error::Overloaded),
        UnitOutcome::Retry(RetryKind::Overloaded, None)
    );
    assert_eq!(
        classify(Error::OriginForbidden),
        UnitOutcome::Failed(RefError::Forbidden)
    );
}

#[test]
fn coverage_deadline_reserves_part_of_the_request_budget() {
    use super::service::coverage_deadline;
    let at = Instant::now();
    // A short request budget leaves time to send the status response.
    let deadline = coverage_deadline(at, at + Duration::from_secs(4)).unwrap();
    assert_eq!(deadline, at + Duration::from_secs(3));
    // A long-poll budget is still capped by the dispatch timeout.
    let deadline = coverage_deadline(at, at + Duration::from_secs(3600)).unwrap();
    assert_eq!(deadline, at + Duration::from_secs(120));
    // No remaining budget means no probing.
    assert!(coverage_deadline(at, at).is_none());
    assert!(coverage_deadline(at + Duration::from_secs(1), at).is_none());
}

fn auth_spec(extra: &str, refs: &[&str]) -> api::Spec {
    api::parse_spec(body(extra, refs).as_bytes(), true, &Config::default()).unwrap()
}

fn token(value: &str) -> super::registry::Credential {
    Some(std::sync::Arc::new(zeroize::Zeroizing::new(
        value.as_bytes().to_vec(),
    )))
}

impl Rig {
    fn put_with(
        &mut self,
        id: &str,
        spec: api::Spec,
        authorization: super::registry::Credential,
    ) -> Result<(u16, api::Status), ErrorCode> {
        match self
            .registry
            .check_put(&cache(), id, &spec, &authorization, self.now)
        {
            Err(e) => return Err(e.code),
            Ok(PutCheck::Replay(status)) => return Ok((200, *status)),
            Ok(PutCheck::Proceed) => {}
        }
        let resolved = self.resolve(&spec);
        self.registry
            .apply_put(&cache(), id, spec, resolved, authorization, self.now)
            .map_err(|e| e.code)
    }

    /// The next batch's credential, completing each page with `outcome`.
    fn fill_once(&mut self, outcome: UnitOutcome) -> Option<Option<Vec<u8>>> {
        let batch = self.registry.next_batch(self.now, 256)?;
        let credential = batch
            .context
            .authorization
            .as_ref()
            .map(|a| a.as_slice().to_vec());
        for page in batch.pages {
            let unit = UnitKey {
                version: batch.metadata.version.clone(),
                page,
            };
            self.registry
                .complete(&unit, outcome, Some(node("owner")), self.now);
        }
        Some(credential)
    }

    fn outcome(&self, id: &str) -> (RefOutcome, Option<RefError>) {
        let status = self
            .registry
            .status(&cache(), id, Some(RefsFilter::All), None)
            .unwrap();
        let entry = &status.refs.unwrap()[0];
        (entry.outcome, entry.error)
    }
}

/// A unit created by a ticket with a credential is filled for a
/// higher-priority ticket without one using no credential: a ticket's fill
/// never borrows another ticket's token.
#[test]
fn fills_use_the_highest_priority_ticket_credential() {
    let mut r = rig(Config::default());
    let page = pages(KEY_A, 0, 1);
    r.put_with(
        "low",
        auth_spec("\"priority\":\"low\",", &[&page]),
        token("Bearer low"),
    )
    .unwrap();
    r.put_with("high", spec("\"priority\":\"high\",", &[&page]), None)
        .unwrap();
    assert_eq!(r.fill_once(UnitOutcome::Admitted), Some(None));
    assert_eq!(r.outcome("high").0, RefOutcome::Succeeded);
    assert_eq!(r.outcome("low").0, RefOutcome::Succeeded);
}

/// An origin refusal fails only the tickets holding the refused credential;
/// the unit is retried once with each next distinct credential.
#[test]
fn rejected_credentials_fail_only_their_tickets() {
    let mut r = rig(Config::default());
    let page = pages(KEY_A, 0, 1);
    r.put_with(
        "a",
        auth_spec("\"priority\":\"high\",", &[&page]),
        token("Bearer a"),
    )
    .unwrap();
    r.put_with(
        "a2",
        auth_spec("\"priority\":\"high\",", &[&page]),
        token("Bearer a"),
    )
    .unwrap();
    r.put_with(
        "b",
        auth_spec("\"priority\":\"normal\",", &[&page]),
        token("Bearer b"),
    )
    .unwrap();
    r.put_with("c", spec("\"priority\":\"low\",", &[&page]), None)
        .unwrap();
    let forbidden = UnitOutcome::Failed(RefError::Forbidden);
    assert_eq!(r.fill_once(forbidden), Some(Some(b"Bearer a".to_vec())));
    for id in ["a", "a2"] {
        assert_eq!(
            r.outcome(id),
            (RefOutcome::Failed, Some(RefError::Forbidden))
        );
        assert!(r.status(id).settled);
    }
    assert_eq!(r.outcome("b").0, RefOutcome::Pending);
    assert_eq!(r.fill_once(forbidden), Some(Some(b"Bearer b".to_vec())));
    assert_eq!(r.outcome("b").1, Some(RefError::Forbidden));
    // The ticket without a credential is filled without one.
    assert_eq!(r.fill_once(UnitOutcome::Admitted), Some(None));
    assert_eq!(r.outcome("c").0, RefOutcome::Succeeded);
    assert_eq!(r.outcome("a").1, Some(RefError::Forbidden));
    assert!(r.fill_once(UnitOutcome::Admitted).is_none());
    // The last remaining credential's refusal fails the unit as before.
    let mut r = rig(Config::default());
    r.put_with("only", auth_spec("", &[&page]), token("Bearer x"))
        .unwrap();
    assert_eq!(r.fill_once(forbidden), Some(Some(b"Bearer x".to_vec())));
    assert_eq!(
        r.outcome("only"),
        (RefOutcome::Failed, Some(RefError::Forbidden))
    );
    assert!(r.fill_once(UnitOutcome::Admitted).is_none());
}

/// Every accepted PUT replaces the credential, including a same-body replay
/// with a rotated token, and a refused ticket is filled again with its new one.
#[test]
fn put_replaces_the_ticket_credential() {
    let mut r = rig(Config::default());
    let page = pages(KEY_A, 0, 1);
    let body = auth_spec("\"generation\":1,\"sequence\":1,", &[&page]);
    r.put_with("t", body.clone(), token("Bearer old")).unwrap();
    // Same body, same token: a replay.
    assert_eq!(
        r.put_with("t", body.clone(), token("Bearer old"))
            .unwrap()
            .0,
        200
    );
    // Same body, rotated token: accepted and used.
    assert_eq!(
        r.put_with("t", body.clone(), token("Bearer new"))
            .unwrap()
            .0,
        200
    );
    assert_eq!(
        r.fill_once(UnitOutcome::Failed(RefError::Forbidden)),
        Some(Some(b"Bearer new".to_vec()))
    );
    assert_eq!(r.outcome("t").1, Some(RefError::Forbidden));
    // Another holder keeps the unit queued; a refreshed token re-joins it.
    let mut r = rig(Config::default());
    r.put_with(
        "other",
        auth_spec("\"priority\":\"low\",", &[&page]),
        token("Bearer other"),
    )
    .unwrap();
    r.put_with(
        "t",
        auth_spec("\"priority\":\"high\",", &[&page]),
        token("Bearer old"),
    )
    .unwrap();
    assert_eq!(
        r.fill_once(UnitOutcome::Failed(RefError::Forbidden)),
        Some(Some(b"Bearer old".to_vec()))
    );
    r.put_with(
        "t",
        auth_spec("\"priority\":\"high\",\"sequence\":1,", &[&page]),
        token("Bearer new"),
    )
    .unwrap();
    assert_eq!(r.outcome("t").0, RefOutcome::Pending);
    assert_eq!(
        r.fill_once(UnitOutcome::Admitted),
        Some(Some(b"Bearer new".to_vec()))
    );
    assert_eq!(r.outcome("t").0, RefOutcome::Succeeded);
    assert_eq!(r.outcome("other").0, RefOutcome::Succeeded);
}

/// A status-wait wake survives registry changes and is reported to the
/// owning lane's park deadline.
#[test]
fn status_wait_wake_survives_registry_changes() {
    let prefetch = super::service::Prefetch::new(racer_io::WorkerCtx::host());
    assert_eq!(prefetch.next_wake(), None);
    let until = Instant::now() + Duration::from_secs(60);
    prefetch.wake_at(until);
    assert_eq!(prefetch.next_wake(), Some(until));
    prefetch.passthrough_caches(&std::collections::HashSet::from([cache()]));
    assert_eq!(prefetch.next_wake(), Some(until));
    prefetch.tick();
    assert_eq!(prefetch.next_wake(), Some(until));
}
