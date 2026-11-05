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
    /// Most pages any node stores at the end of the run.
    pub stored_pages_max: u64,
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
                    "stored_pages_max": c.stored_pages_max,
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
