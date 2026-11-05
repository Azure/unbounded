//! The Model run report: counters, read latency, origin traffic per page, invariants. Shaped
//! like racer-sim's `report.rs` so its scenario checks port over.

use std::collections::BTreeMap;

use serde_json::{Value, json};

use super::{ModelSpec, Page};
use crate::{engine::Nanos, scenario::Scenario};

/// Run counters (racer-sim names where one exists).
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Counters {
    /// Client reads that delivered the page.
    pub reads_completed: u64,
    /// Client reads that failed.
    pub reads_failed: u64,
    /// Client reads served from the node's own copy.
    pub local_hits: u64,
    /// Reads that joined a running local acquisition.
    pub flight_joins: u64,
    /// Origin page requests.
    pub origin_requests: u64,
    /// Origin bytes delivered.
    pub origin_bytes: u64,
    /// Peer probes sent.
    pub peer_probes: u64,
    /// Pages streamed between peers, and their bytes.
    pub peer_pages: u64,
    /// Peer bytes streamed.
    pub peer_bytes: u64,
    /// Probes shed by a hot-page owner.
    pub sheds: u64,
    /// Acquire probes deferred to a successor owner.
    pub deferrals: u64,
    /// Overflow helper probes, and shed requesters with no helper.
    pub helpers: u64,
    /// Shed requesters with no eligible helper.
    pub no_helper: u64,
    /// Pages published to disk.
    pub disk_writes: u64,
    /// Pages evicted from a full node store.
    pub evictions: u64,
    /// Peer pages served by a node that does not own the page (an overflow helper).
    pub helper_copies: u64,
    /// Injected origin precondition failures (412).
    pub origin_412: u64,
    /// Injected origin forbidden answers (403).
    pub origin_403: u64,
    /// Injected origin server errors (5xx).
    pub origin_5xx: u64,
    /// Window probes that found the page on disk.
    pub disk_reads: u64,
    /// Spill holds taken by range reads.
    pub spill_holds: u64,
    /// Metadata refreshes sent to origin.
    pub metadata_requests: u64,
    /// Stored pages lost to cache outages.
    pub outage_lost_pages: u64,
    /// Client read attempts retried after a recoverable failure.
    pub read_retries: u64,
    /// Flight waiters elected to lead after an earlier leader failed (racer-sim
    /// `flight_reelected`).
    pub flights_reelected: u64,
    /// Probes unanswered at their attempt bound (`AttemptPlan::attempt_end`).
    pub probe_timeouts: u64,
    /// Silence pings sent (production `Event::SilencePingSent`).
    pub silence_pings: u64,
    /// Page bodies dropped because their requester abandoned the exchange first.
    pub replies_abandoned: u64,
    /// Probe answers that arrived after their attempt timed out.
    pub late_answers: u64,
    /// Requests a relay forwarded (racer-sim `relays`).
    pub relays: u64,
    /// Relay failures answered by the relay, or dropped (racer-sim `relay_failures_dropped`).
    pub relay_failures: u64,
    /// Route searches that found no route (racer-sim `route_unreachable`).
    pub route_unreachable: u64,
    /// Last links unanswered when the requester gave up (racer-sim `silence_timeouts`).
    pub silence_timeouts: u64,
    /// Requests dropped at a relay or destination after the requester gave up (racer-sim
    /// `requests_abandoned`).
    pub requests_abandoned: u64,
    /// Node crashes.
    pub crashes: u64,
    /// Requests lost at a crashed or hung holder (racer-sim `messages_lost_to_crash`,
    /// `messages_lost_to_hang`).
    pub messages_lost: u64,
    /// Links that failed to connect (racer-sim `link_failures`).
    pub link_failures: u64,
    /// Replies lost on a cut route (racer-sim `replies_lost`).
    pub replies_lost: u64,
    /// Most pages any node stores at the end of the run.
    pub stored_pages_max: u64,
    /// Relays that found no free `Relay` transfer (racer-sim fails the hop `Overloaded`).
    pub relay_sheds: u64,
    /// Requests whose verification found the crypto queue full (racer-sim `verify_sheds`).
    pub verify_sheds: u64,
    /// Links that reused an idle connection (`racer_core::connections::Stats`).
    pub conn_reused: u64,
    /// Links whose new connection was accepted at once.
    pub conn_accepted: u64,
    /// Links whose new connection waited in the receiver's accept queue.
    pub conn_queued: u64,
    /// Links whose new connection a full accept queue dropped.
    pub conn_dropped: u64,
    /// Links refused `Overloaded` by the per-endpoint cap.
    pub conn_overloaded_endpoint: u64,
    /// Links refused `Overloaded` by a full outbound pool.
    pub conn_overloaded_pool: u64,
    /// Idle connections a full outbound pool closed.
    pub conn_idle_cleared: u64,
    /// Idle connections that timed out.
    pub conn_idle_expired: u64,
    /// The longest accept queue.
    pub conn_most_queued: u64,
    /// Idle inbound connections a full listener closed (not production).
    pub conn_idle_evicted: u64,
    /// Requests whose accept-queue wait outlived their requester's attempt.
    pub conn_queue_abandoned: u64,
    /// Route searches refused with every search slot busy (racer-sim `route_search_overloaded`).
    pub search_sheds: u64,
    /// Membership changes (racer-sim `membership_changes`).
    pub membership_changes: u64,
    /// Membership versions published (racer-sim `membership_publications`).
    pub membership_publications: u64,
    /// Requests answered `StaleMembership` (racer-sim `stale_membership_replies`).
    pub stale_membership_replies: u64,
    /// Stale-destination waits (racer-sim `stale_waits`).
    pub stale_waits: u64,
    /// Links returned after `StaleMembership` answers (racer-sim `stale_links_refunded`).
    pub stale_links_refunded: u64,
    /// Client reads not started on a node outside its own membership (racer-sim
    /// `reads_skipped_not_member`).
    pub reads_skipped_not_member: u64,
}

/// What a run observed.
#[derive(Debug, Default)]
pub struct Report {
    /// Counters.
    pub counters: Counters,
    /// Client read latencies (ns), completed reads only.
    pub latencies: Vec<Nanos>,
    /// Failed reads by error.
    pub errors: BTreeMap<String, u64>,
    /// Failed client read attempts (retried or final), by error and by attempt elapsed time
    /// rounded down to a second, as `"<error>@<seconds>s"`.
    pub attempt_errors: BTreeMap<String, u64>,
    /// Origin requests by page.
    pub origin_per_page: BTreeMap<Page, u64>,
    /// Core effect and serve outcome variants seen, by [`super::effect_name`].
    pub coverage: BTreeMap<&'static str, u64>,
    /// Hard failures (invariant violations), first few kept.
    pub violations: Vec<String>,
    /// Hard failure count.
    pub hard_failures: u64,
}

impl Report {
    /// Record one completed read's latency.
    pub fn latency(&mut self, ns: Nanos) {
        self.latencies.push(ns);
    }

    /// Record a hard failure.
    pub fn violation(&mut self, message: String) {
        self.hard_failures += 1;
        if self.violations.len() < 16 {
            self.violations.push(message);
        }
    }

    /// The JSON report.
    pub fn json(&self, scenario: &Scenario, spec: &ModelSpec, seed: u64, end: Nanos) -> Value {
        let c = &self.counters;
        let mut sorted = self.latencies.clone();
        sorted.sort_unstable();
        let quantile = |q: f64| -> u64 {
            if sorted.is_empty() {
                0
            } else {
                sorted[((sorted.len() - 1) as f64 * q) as usize]
            }
        };
        let max_per_page = self.origin_per_page.values().copied().max().unwrap_or(0);
        let pages = self.origin_per_page.len() as u64;
        json!({
            "scenario": scenario.name,
            "fidelity": "model",
            "seed": seed,
            "nodes": scenario.nodes,
            "sites": scenario.sites,
            "workers": spec.workers,
            "pages_per_object": spec.pages_per_object,
            "page_bytes": spec.page_bytes,
            "end_ns": end,
            "metrics": {
                "counters": {
                    "reads_completed": c.reads_completed,
                    "reads_failed": c.reads_failed,
                    "local_hits": c.local_hits,
                    "flight_joins": c.flight_joins,
                    "origin_requests": c.origin_requests,
                    "origin_bytes": c.origin_bytes,
                    "peer_probes": c.peer_probes,
                    "peer_pages": c.peer_pages,
                    "peer_bytes": c.peer_bytes,
                    "sheds": c.sheds,
                    "deferrals": c.deferrals,
                    "helper_probes": c.helpers,
                    "no_helper": c.no_helper,
                    "disk_writes": c.disk_writes,
                    "evictions": c.evictions,
                    "helper_copies": c.helper_copies,
                    "origin_412": c.origin_412,
                    "origin_403": c.origin_403,
                    "origin_5xx": c.origin_5xx,
                    "read_retries": c.read_retries,
                    "outage_lost_pages": c.outage_lost_pages,
                    "disk_reads": c.disk_reads,
                    "spill_holds": c.spill_holds,
                    "metadata_requests": c.metadata_requests,
                    "stored_pages_max": c.stored_pages_max,
                    "probe_timeouts": c.probe_timeouts,
                    "silence_pings": c.silence_pings,
                    "flights_reelected": c.flights_reelected,
                    "late_answers": c.late_answers,
                    "relays": c.relays,
                    "relay_sheds": c.relay_sheds,
                    "verify_sheds": c.verify_sheds,
                    "conn_reused": c.conn_reused,
                    "conn_accepted": c.conn_accepted,
                    "conn_queued": c.conn_queued,
                    "conn_dropped": c.conn_dropped,
                    "conn_overloaded_endpoint": c.conn_overloaded_endpoint,
                    "conn_overloaded_pool": c.conn_overloaded_pool,
                    "conn_idle_cleared": c.conn_idle_cleared,
                    "conn_idle_expired": c.conn_idle_expired,
                    "conn_most_queued": c.conn_most_queued,
                    "conn_idle_evicted": c.conn_idle_evicted,
                    "conn_queue_abandoned": c.conn_queue_abandoned,
                    "search_sheds": c.search_sheds,
                    "membership_changes": c.membership_changes,
                    "membership_publications": c.membership_publications,
                    "stale_membership_replies": c.stale_membership_replies,
                    "stale_waits": c.stale_waits,
                    "stale_links_refunded": c.stale_links_refunded,
                    "reads_skipped_not_member": c.reads_skipped_not_member,
                    "relay_failures": c.relay_failures,
                    "route_unreachable": c.route_unreachable,
                    "silence_timeouts": c.silence_timeouts,
                    "requests_abandoned": c.requests_abandoned,
                    "crashes": c.crashes,
                    "messages_lost": c.messages_lost,
                    "link_failures": c.link_failures,
                    "replies_lost": c.replies_lost,
                    "replies_abandoned": c.replies_abandoned,
                },
                "histograms": {
                    "read_latency_ns": {
                        "count": sorted.len(),
                        "min": sorted.first().copied().unwrap_or(0),
                        "p50": quantile(0.5),
                        "p99": quantile(0.99),
                        "max": sorted.last().copied().unwrap_or(0),
                    },
                },
                "errors": self.errors,
                "attempt_errors": self.attempt_errors,
            },
            "origin_per_page": {
                "pages": pages,
                "max_requests_per_page": max_per_page,
                "mean_requests_per_page": if pages == 0 { 0.0 } else { c.origin_requests as f64 / pages as f64 },
            },
            "coverage": self.coverage,
            "invariants": {
                "hard_failures": self.hard_failures,
                "violations": self.violations,
            },
        })
    }
}
