//! Simulator configuration and JSON overrides.
//!
//! Every knob has a default suitable for a fast smoke run. `--config FILE`
//! takes a JSON object whose keys are field names of [`Config`] (nested
//! objects for `costs` and `origin_errors`, arrays for `faults` and `churn`).
//! Unknown keys are errors so typos do not silently fall back to defaults.

use serde_json::Value;

use crate::engine::{MICROS, MILLIS, Nanos, SECONDS};
use crate::faults::Fault;
use crate::node::world::DUPLICATE_BYTES;
use crate::resources::OriginErrors;
use crate::workload::{Churn, Storm};

/// Configurable CPU costs in nanoseconds.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Costs {
    /// AEAD seal/open cost per byte, in picoseconds.
    pub aead_ps_per_byte: u64,
    pub sign_ns: Nanos,
    pub verify_ns: Nanos,
    /// Cost to score one ranking chunk.
    pub ranking_chunk_ns: Nanos,
    /// Path expansion cost per membership node visited.
    pub path_expand_ns_per_node: Nanos,
    /// One control-plane handshake.
    pub control_handshake_ns: Nanos,
    /// Fixed per-request dispatch cost on a worker.
    pub request_ns: Nanos,
}

impl Default for Costs {
    fn default() -> Self {
        Self {
            aead_ps_per_byte: 250,
            sign_ns: 20 * MICROS,
            verify_ns: 40 * MICROS,
            ranking_chunk_ns: 2 * MICROS,
            path_expand_ns_per_node: 50,
            control_handshake_ns: 200 * MICROS,
            request_ns: 5 * MICROS,
        }
    }
}

impl Costs {
    pub fn aead(&self, bytes: u64) -> Nanos {
        (u128::from(bytes) * u128::from(self.aead_ps_per_byte)).div_ceil(1000) as Nanos
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct Config {
    pub nodes: u32,
    pub sites: u32,
    pub workers: u8,
    pub duration_ns: Nanos,
    pub local_rtt_ns: u32,
    pub remote_rtt_ns: u32,
    pub rtt_jitter_ns: u32,
    pub nic_bps: u64,
    pub chunk_bytes: u64,
    pub disk_read_bps: u64,
    pub disk_write_bps: u64,
    pub disk_latency_ns: Nanos,
    pub origin_caches: u32,
    pub origin_concurrency: u32,
    pub origin_bps: u64,
    pub origin_latency_ns: Nanos,
    pub origin_retry_ns: Nanos,
    pub origin_errors: OriginErrors,
    pub costs: Costs,
    pub page_bytes: u64,
    pub objects: u64,
    pub pages_per_object: u32,
    pub zipf_s: f64,
    /// Mean requests per node per second (Poisson).
    pub request_rate: f64,
    /// Fraction of requests that are range reads.
    pub range_fraction: f64,
    /// Pages per range read.
    pub range_pages: u32,
    /// Memory tier capacity, pages per node.
    pub memory_pages: u32,
    /// Disk tier capacity, pages per node.
    pub disk_pages: u32,
    /// Hop budget for peer requests (links).
    pub hop_limit: u8,
    pub storms: Vec<Storm>,
    pub churn: Vec<Churn>,
    pub faults: Vec<Fault>,
    /// Real node logic knobs (`"node"` object in JSON).
    pub node: NodeConfig,
    /// Data-plane faults the combined scenarios add (`"combined_faults"`).
    pub combined_faults: crate::combined::DataFaults,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            nodes: 16,
            sites: 4,
            workers: 2,
            duration_ns: 2 * SECONDS,
            local_rtt_ns: 200 * MICROS as u32,
            remote_rtt_ns: 20 * MILLIS as u32,
            rtt_jitter_ns: 5 * MILLIS as u32,
            nic_bps: 1_250_000_000, // 10 Gbit/s
            chunk_bytes: 1 << 20,
            disk_read_bps: 2_000_000_000,
            disk_write_bps: 1_000_000_000,
            disk_latency_ns: 100 * MICROS,
            origin_caches: 4,
            origin_concurrency: 64,
            origin_bps: 5_000_000_000,
            origin_latency_ns: 30 * MILLIS,
            origin_retry_ns: 50 * MILLIS,
            origin_errors: OriginErrors::default(),
            costs: Costs::default(),
            page_bytes: 1 << 20,
            objects: 64,
            pages_per_object: 16,
            zipf_s: 1.1,
            request_rate: 20.0,
            range_fraction: 0.1,
            range_pages: 8,
            memory_pages: 64,
            disk_pages: 256,
            hop_limit: 8,
            storms: Vec::new(),
            churn: Vec::new(),
            faults: Vec::new(),
            node: NodeConfig::default(),
            combined_faults: crate::combined::DataFaults::default(),
        }
    }
}

/// Node knobs: cache capacities, flight and relay limits, timeouts and the membership publication
/// model.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct NodeConfig {
    /// Phantom placement cache entries per worker (production: about 16,384 from a 16 MiB budget).
    pub placement_entries: u32,
    /// Route cache entries per node (production `cached_paths` 128).
    pub path_entries: u32,
    /// Route cache byte budget per node (production 8 MiB).
    pub path_bytes: u64,
    /// Concurrent route searches per node (production 8).
    pub active_searches: u32,
    /// Route searches that may wait for a slot per node (production
    /// `topology::DEFAULT_SEARCH_QUEUE`, 64). Zero rejects at once.
    pub search_queue: u32,
    pub flights: u32,
    pub waiters_per_flight: u32,
    pub relay_transfers: u32,
    pub plaintext_bytes: u64,
    pub ciphertext_bytes: u64,
    pub dirty_bytes: u64,
    /// Pages per disk segment.
    pub pages_per_segment: u32,
    /// Second-sight history bytes per node.
    pub history_bytes: u32,
    /// Membership changes are batched and published at this period.
    pub membership_batch_ns: Nanos,
    /// Each node adopts a publication after a per-node lag in `0..=membership_lag_ns` (must not
    /// exceed the batch period).
    pub membership_lag_ns: Nanos,
    /// Old membership versions a node can still resolve (production `retained_snapshots` 2).
    pub retained_versions: u64,
    /// Per-attempt peer timeout (`attempt_timeout`).
    pub attempt_timeout_ns: Nanos,
    /// Client read deadline per page.
    pub read_timeout_ns: Nanos,
    /// Time to detect a refused or partitioned link.
    pub connect_timeout_ns: Nanos,
    /// Request scope at a serving peer.
    pub serve_timeout_ns: Nanos,
    /// Concurrent page hedges per node (production `page_hedge_slots`, default 0: hedging off).
    pub hedge_slots: u32,
    /// Delay before the duplicate starts (production `page_hedge_delay_ms`, default 100 ms).
    pub hedge_delay_ns: Nanos,
    /// Speculative bytes per node (production `page_hedge_bytes`, default one `DUPLICATE_BYTES`).
    pub hedge_bytes: u64,
    // Ablation switches: production always has these on (except `lineage_slack`); set false to
    // measure without the fix.
    /// Overflow fan-out to page helpers.
    pub fanout: bool,
    /// Bodies of one page an owner streams before shedding its readers; 0 derives it from the page
    /// size like production.
    pub hot_page_bodies: u32,
    /// Fill handover to a live waiter when the leader expires.
    pub flight_handover: bool,
    /// Rank 0 asks a successor that recently missed it before origin.
    pub successor_hints: bool,
    /// Owners ask hinted later owners copy-only first. Needs `successor_hints`.
    pub successor_targets: bool,
    /// Non-owners walk the previous membership at a lagging destination.
    pub stale_fallback: bool,
    /// Lineage targets and lineage-first visit order.
    pub lineage: bool,
    /// A non-owner that cannot fall back waits once.
    pub stale_wait: bool,
    /// `StaleMembership` refunds the links past the answerer.
    pub stale_link_refund: bool,
    /// Owners also ask members just below past owners. Needs `lineage`.
    pub demoted_targets: bool,
    /// Lineage targets get their own grant.
    pub lineage_grants: bool,
    /// With `lineage_grants`, still fund lineage probes from slack (production off).
    pub lineage_slack: bool,
    /// Copy-only requests from earlier-ranked owners do not join.
    pub copy_join_rank: bool,
    /// Owners above rank 0 offer origin copies to rank 0. Needs `successor_hints`.
    pub orphan_notices: bool,
    /// Owners above rank 0 track rank 0 per page.
    pub rank0_watch: bool,
    /// Granted probes go out at once.
    pub parallel_granted: bool,
    /// Most lineage targets per owner walk (production `walk::MAX_LINEAGE`).
    pub lineage_cap: usize,
    /// Unknown-age demoted members are lineage targets too.
    pub unknown_age_targets: bool,
    /// Hold unanswering next hops silent.
    pub silence: bool,
    /// Visit owners that shed us last. Needs `silence`.
    pub shed_busy: bool,
    /// Leave the retry to rank 0 after its own failure. Needs `rank0_watch`.
    pub rank0_failure_hold: bool,
    /// Copy offers wait out a cut toward their target's site.
    pub offer_cut_groups: bool,
    /// Rank 2 holders also offer origin copies to rank 1.
    pub offer_predecessors: bool,
}

impl Default for NodeConfig {
    fn default() -> Self {
        Self {
            placement_entries: 256,
            path_entries: 128,
            path_bytes: 256 << 10,
            active_searches: 8,
            search_queue: topology::DEFAULT_SEARCH_QUEUE as u32,
            flights: 64,
            waiters_per_flight: 64,
            relay_transfers: 16,
            plaintext_bytes: 64 * MIB,
            ciphertext_bytes: 128 * MIB,
            dirty_bytes: 160 * MIB,
            pages_per_segment: 8,
            history_bytes: 2048,
            membership_batch_ns: 250 * MILLIS,
            membership_lag_ns: 100 * MILLIS,
            retained_versions: 2,
            attempt_timeout_ns: 2 * SECONDS,
            read_timeout_ns: 10 * SECONDS,
            connect_timeout_ns: 100 * MILLIS,
            serve_timeout_ns: 5 * SECONDS,
            hedge_slots: 0,
            hedge_delay_ns: 100 * MILLIS,
            hedge_bytes: DUPLICATE_BYTES as u64,
            fanout: true,
            hot_page_bodies: 0,
            flight_handover: true,
            successor_hints: true,
            successor_targets: true,
            stale_fallback: true,
            lineage: true,
            stale_wait: true,
            stale_link_refund: true,
            demoted_targets: true,
            lineage_grants: true,
            lineage_slack: true,
            copy_join_rank: true,
            orphan_notices: true,
            rank0_watch: true,
            parallel_granted: true,
            lineage_cap: racer_core::read::walk::MAX_LINEAGE,
            unknown_age_targets: true,
            silence: true,
            shed_busy: true,
            rank0_failure_hold: true,
            offer_cut_groups: true,
            offer_predecessors: true,
        }
    }
}

const MIB: u64 = 1 << 20;

fn u64_of(key: &str, v: &Value) -> Result<u64, String> {
    v.as_u64()
        .ok_or_else(|| format!("{key}: expected a non-negative integer, got {v}"))
}

fn f64_of(key: &str, v: &Value) -> Result<f64, String> {
    v.as_f64()
        .filter(|f| f.is_finite() && *f >= 0.0)
        .ok_or_else(|| format!("{key}: expected a non-negative number, got {v}"))
}

fn narrow<T: TryFrom<u64>>(key: &str, v: &Value) -> Result<T, String> {
    T::try_from(u64_of(key, v)?).map_err(|_| format!("{key}: out of range: {v}"))
}

fn object<'a>(key: &str, v: &'a Value) -> Result<&'a serde_json::Map<String, Value>, String> {
    v.as_object()
        .ok_or_else(|| format!("{key}: expected an object, got {v}"))
}

impl Config {
    /// Apply a JSON object of overrides.
    pub fn apply_json(&mut self, value: &Value) -> Result<(), String> {
        for (key, v) in object("config", value)? {
            let k = key.as_str();
            match k {
                "nodes" => self.nodes = narrow(k, v)?,
                "sites" => self.sites = narrow(k, v)?,
                "workers" => self.workers = narrow(k, v)?,
                "duration_ns" => self.duration_ns = u64_of(k, v)?,
                "local_rtt_ns" => self.local_rtt_ns = narrow(k, v)?,
                "remote_rtt_ns" => self.remote_rtt_ns = narrow(k, v)?,
                "rtt_jitter_ns" => self.rtt_jitter_ns = narrow(k, v)?,
                "nic_bps" => self.nic_bps = u64_of(k, v)?,
                "chunk_bytes" => self.chunk_bytes = u64_of(k, v)?,
                "disk_read_bps" => self.disk_read_bps = u64_of(k, v)?,
                "disk_write_bps" => self.disk_write_bps = u64_of(k, v)?,
                "disk_latency_ns" => self.disk_latency_ns = u64_of(k, v)?,
                "origin_caches" => self.origin_caches = narrow(k, v)?,
                "origin_concurrency" => self.origin_concurrency = narrow(k, v)?,
                "origin_bps" => self.origin_bps = u64_of(k, v)?,
                "origin_latency_ns" => self.origin_latency_ns = u64_of(k, v)?,
                "origin_retry_ns" => self.origin_retry_ns = u64_of(k, v)?,
                "page_bytes" => self.page_bytes = u64_of(k, v)?,
                "objects" => self.objects = u64_of(k, v)?,
                "pages_per_object" => self.pages_per_object = narrow(k, v)?,
                "zipf_s" => self.zipf_s = f64_of(k, v)?,
                "request_rate" => self.request_rate = f64_of(k, v)?,
                "range_fraction" => self.range_fraction = f64_of(k, v)?,
                "range_pages" => self.range_pages = narrow(k, v)?,
                "memory_pages" => self.memory_pages = narrow(k, v)?,
                "disk_pages" => self.disk_pages = narrow(k, v)?,
                "hop_limit" => self.hop_limit = narrow(k, v)?,
                "origin_errors" => {
                    for (ek, ev) in object(k, v)? {
                        let slot = match ek.as_str() {
                            "p412" => &mut self.origin_errors.p412,
                            "p403" => &mut self.origin_errors.p403,
                            "p5xx" => &mut self.origin_errors.p5xx,
                            _ => return Err(format!("unknown key origin_errors.{ek}")),
                        };
                        *slot = f64_of(ek, ev)?;
                    }
                }
                "costs" => {
                    for (ck, cv) in object(k, v)? {
                        let c = &mut self.costs;
                        let slot = match ck.as_str() {
                            "aead_ps_per_byte" => &mut c.aead_ps_per_byte,
                            "sign_ns" => &mut c.sign_ns,
                            "verify_ns" => &mut c.verify_ns,
                            "ranking_chunk_ns" => &mut c.ranking_chunk_ns,
                            "path_expand_ns_per_node" => &mut c.path_expand_ns_per_node,
                            "control_handshake_ns" => &mut c.control_handshake_ns,
                            "request_ns" => &mut c.request_ns,
                            _ => return Err(format!("unknown key costs.{ck}")),
                        };
                        *slot = u64_of(ck, cv)?;
                    }
                }
                "storms" => self.storms = list(k, v, Storm::from_json)?,
                "churn" => self.churn = list(k, v, Churn::from_json)?,
                "faults" => self.faults = list(k, v, Fault::from_json)?,
                "node" => {
                    let node = &mut self.node;
                    for (key, v) in object(k, v)? {
                        let n = v.as_u64().ok_or_else(|| {
                            format!("node.{key}: expected a non-negative integer, got {v}")
                        })?;
                        let small =
                            || u32::try_from(n).map_err(|_| format!("node.{key}: out of range"));
                        match key.as_str() {
                            "placement_entries" => node.placement_entries = small()?,
                            "path_entries" => node.path_entries = small()?,
                            "path_bytes" => node.path_bytes = n,
                            "active_searches" => node.active_searches = small()?,
                            "search_queue" => node.search_queue = small()?,
                            "flights" => node.flights = small()?,
                            "waiters_per_flight" => node.waiters_per_flight = small()?,
                            "relay_transfers" => node.relay_transfers = small()?,
                            "plaintext_bytes" => node.plaintext_bytes = n,
                            "ciphertext_bytes" => node.ciphertext_bytes = n,
                            "dirty_bytes" => node.dirty_bytes = n,
                            "pages_per_segment" => node.pages_per_segment = small()?,
                            "history_bytes" => node.history_bytes = small()?,
                            "membership_batch_ns" => node.membership_batch_ns = n,
                            "membership_lag_ns" => node.membership_lag_ns = n,
                            "retained_versions" => node.retained_versions = n,
                            "attempt_timeout_ns" => node.attempt_timeout_ns = n,
                            "read_timeout_ns" => node.read_timeout_ns = n,
                            "connect_timeout_ns" => node.connect_timeout_ns = n,
                            "serve_timeout_ns" => node.serve_timeout_ns = n,
                            "hedge_slots" => node.hedge_slots = small()?,
                            "hedge_delay_ns" => node.hedge_delay_ns = n,
                            "hedge_bytes" => node.hedge_bytes = n,
                            "fanout" if n <= 1 => node.fanout = n == 1,
                            "hot_page_bodies" => node.hot_page_bodies = small()?,
                            "flight_handover" if n <= 1 => node.flight_handover = n == 1,
                            "successor_hints" if n <= 1 => node.successor_hints = n == 1,
                            "successor_targets" if n <= 1 => node.successor_targets = n == 1,
                            "stale_fallback" if n <= 1 => node.stale_fallback = n == 1,
                            "lineage" if n <= 1 => node.lineage = n == 1,
                            "stale_wait" if n <= 1 => node.stale_wait = n == 1,
                            "stale_link_refund" if n <= 1 => node.stale_link_refund = n == 1,
                            "demoted_targets" if n <= 1 => node.demoted_targets = n == 1,
                            "lineage_grants" if n <= 1 => node.lineage_grants = n == 1,
                            "lineage_slack" if n <= 1 => node.lineage_slack = n == 1,
                            "copy_join_rank" if n <= 1 => node.copy_join_rank = n == 1,
                            "orphan_notices" if n <= 1 => node.orphan_notices = n == 1,
                            "rank0_watch" if n <= 1 => node.rank0_watch = n == 1,
                            "parallel_granted" if n <= 1 => node.parallel_granted = n == 1,
                            "unknown_age_targets" if n <= 1 => node.unknown_age_targets = n == 1,
                            "lineage_cap" if n <= racer_core::read::walk::MAX_LINEAGE as u64 => {
                                node.lineage_cap = n as usize;
                            }
                            "silence" if n <= 1 => node.silence = n == 1,
                            "shed_busy" if n <= 1 => node.shed_busy = n == 1,
                            "rank0_failure_hold" if n <= 1 => node.rank0_failure_hold = n == 1,
                            "offer_cut_groups" if n <= 1 => node.offer_cut_groups = n == 1,
                            "offer_predecessors" if n <= 1 => node.offer_predecessors = n == 1,
                            _ => return Err(format!("unknown key node.{key}")),
                        }
                    }
                }
                "combined_faults" => self.combined_faults.apply_json(v)?,
                _ => return Err(format!("unknown config key {key:?}")),
            }
        }
        self.validate()
    }

    pub fn validate(&self) -> Result<(), String> {
        if self.nodes == 0 {
            return Err("nodes must be at least 1".into());
        }
        if self.objects == 0 || self.pages_per_object == 0 {
            return Err("objects and pages_per_object must be at least 1".into());
        }
        if self.hop_limit == 0 {
            return Err("hop_limit must be at least 1".into());
        }
        let e = self.origin_errors;
        if e.p412 + e.p403 + e.p5xx > 1.0 {
            return Err("origin error probabilities sum above 1".into());
        }
        if self.range_fraction > 1.0 {
            return Err("range_fraction must be at most 1".into());
        }
        if self.node.flights == 0
            || self.node.waiters_per_flight == 0
            || self.node.relay_transfers == 0
        {
            return Err(
                "node: flights, waiters_per_flight and relay_transfers must be nonzero".into(),
            );
        }
        if self.node.pages_per_segment == 0 || self.node.history_bytes == 0 {
            return Err("node: pages_per_segment and history_bytes must be nonzero".into());
        }
        if self.node.membership_batch_ns == 0
            || self.node.membership_lag_ns > self.node.membership_batch_ns
        {
            return Err(
                "node: membership_lag_ns must not exceed a nonzero membership_batch_ns".into(),
            );
        }
        if self.node.attempt_timeout_ns == 0
            || self.node.read_timeout_ns == 0
            || self.node.serve_timeout_ns == 0
        {
            return Err("node: timeouts must be nonzero".into());
        }
        if self.node.hedge_slots > 0 && self.node.hedge_bytes < DUPLICATE_BYTES as u64 {
            return Err("node: hedge_bytes must cover one duplicate page".into());
        }
        Ok(())
    }
}

fn list<T>(key: &str, v: &Value, parse: fn(&Value) -> Result<T, String>) -> Result<Vec<T>, String> {
    v.as_array()
        .ok_or_else(|| format!("{key}: expected an array"))?
        .iter()
        .map(|item| parse(item).map_err(|e| format!("{key}: {e}")))
        .collect()
}

/// Read a field from a small JSON object used by workload and fault specs.
pub(crate) fn field(obj: &serde_json::Map<String, Value>, key: &str) -> Result<u64, String> {
    u64_of(
        key,
        obj.get(key)
            .ok_or_else(|| format!("missing field {key:?}"))?,
    )
}

pub(crate) fn spec<'a>(
    v: &'a Value,
    allowed: &[&str],
) -> Result<&'a serde_json::Map<String, Value>, String> {
    let obj = object("entry", v)?;
    for k in obj.keys() {
        if !allowed.contains(&k.as_str()) {
            return Err(format!("unknown field {k:?}"));
        }
    }
    Ok(obj)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn overrides_apply() {
        let mut c = Config::default();
        c.apply_json(&json!({
            "nodes": 200,
            "zipf_s": 0.9,
            "costs": {"sign_ns": 7},
            "origin_errors": {"p5xx": 0.1},
            "faults": [{"kind": "crash", "node": 3, "at_ns": 10, "restart_ns": 20}],
            "storms": [{"at_ns": 5, "object": 1, "nodes": 10, "window_ns": 100}],
            "churn": [{"at_ns": 9, "node": 2, "join": false}]
        }))
        .unwrap();
        assert_eq!(c.nodes, 200);
        assert_eq!(c.zipf_s, 0.9);
        assert_eq!(c.costs.sign_ns, 7);
        assert_eq!(c.origin_errors.p5xx, 0.1);
        assert_eq!(c.faults.len(), 1);
        assert_eq!(c.storms.len(), 1);
        assert_eq!(c.churn.len(), 1);
    }

    #[test]
    fn rejects_bad_overrides() {
        for bad in [
            json!([]),
            json!({"bogus": 1}),
            json!({"nodes": -1}),
            json!({"nodes": 0}),
            json!({"workers": 300}),
            json!({"zipf_s": "x"}),
            json!({"costs": {"nope": 1}}),
            json!({"origin_errors": {"p412": 0.6, "p403": 0.6}}),
            json!({"faults": [{"kind": "meteor"}]}),
            json!({"faults": [{"kind": "crash", "node": 1}]}),
        ] {
            assert!(Config::default().apply_json(&bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn aead_cost_rounds_up() {
        let c = Costs {
            aead_ps_per_byte: 3,
            ..Costs::default()
        };
        assert_eq!(c.aead(0), 0);
        assert_eq!(c.aead(1), 1);
        assert_eq!(c.aead(1000), 3);
    }

    #[test]
    fn overrides_and_validation() {
        let mut c = Config::default();
        c.validate().unwrap();
        c.apply_json(&json!({"node": {"placement_entries": 9, "membership_lag_ns": 5}}))
            .unwrap();
        assert_eq!((c.node.placement_entries, c.node.membership_lag_ns), (9, 5));
        assert!(c.node.flight_handover);
        c.apply_json(&json!({"node": {"flight_handover": 0}}))
            .unwrap();
        assert!(!c.node.flight_handover);
        for bad in [
            json!([]),
            json!({"nope": 1}),
            json!({"flights": -1}),
            json!({"flights": 1u64 << 40}),
            json!({"flight_handover": 2}),
        ] {
            assert!(
                Config::default().apply_json(&json!({"node": bad})).is_err(),
                "{bad}"
            );
        }
        let mut lagging = Config::default();
        lagging.node.membership_lag_ns = 2 * SECONDS;
        assert!(lagging.validate().is_err());
        let mut no_flights = Config::default();
        no_flights.node.flights = 0;
        assert!(no_flights.validate().is_err());
    }
}
