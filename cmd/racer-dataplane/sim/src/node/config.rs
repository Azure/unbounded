//! Node knobs: cache capacities, flight and relay limits, timeouts and the
//! membership publication model.
//!
//! Capacities default far below production (see the design doc, section
//! "Node integration") so 50,000 nodes fit the memory budget; the decision
//! code is the same at any capacity. Flight, waiter and relay limits and the
//! fill quotas default to the production per-worker values in
//! `src/config.rs` (`flights` 64, `waiters_per_flight` 64,
//! `relay_transfers` 16, plaintext 64 MiB, ciphertext 128 MiB, dirty
//! 160 MiB).

use serde_json::Value;

use crate::engine::{MILLIS, Nanos, SECONDS};

const MIB: u64 = 1 << 20;

#[derive(Clone, Copy, Debug, PartialEq)]
pub struct NodeConfig {
    /// Phantom placement cache entries per worker (production: about
    /// 16,384 from a 16 MiB budget).
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
    /// Each node adopts a publication after a per-node lag in
    /// `0..=membership_lag_ns` (must not exceed the batch period).
    pub membership_lag_ns: Nanos,
    /// Old membership versions a node can still resolve (production
    /// `retained_snapshots` 2).
    pub retained_versions: u64,
    /// Per-attempt peer timeout (`attempt_timeout`).
    pub attempt_timeout_ns: Nanos,
    /// Client read deadline per page.
    pub read_timeout_ns: Nanos,
    /// Time to detect a refused or partitioned link.
    pub connect_timeout_ns: Nanos,
    /// Request scope at a serving peer.
    pub serve_timeout_ns: Nanos,
    /// Concurrent page hedges per node (production `page_hedge_slots`,
    /// default 0: hedging off).
    pub hedge_slots: u32,
    /// Delay before the duplicate starts (production `page_hedge_delay_ms`,
    /// default 100 ms).
    pub hedge_delay_ns: Nanos,
    /// Speculative bytes per node (production `page_hedge_bytes`, default
    /// one `DUPLICATE_BYTES`).
    pub hedge_bytes: u64,
    /// Overflow fan-out to page helpers (production: always on; set 0 to
    /// measure without it, Known issue 16).
    pub fanout: bool,
    /// Bodies of one page an owner streams before it sheds readers of the
    /// page; 0 (default) derives it from the page size like production
    /// (`racer_core::read::fanout::hot_page_bodies`).
    pub hot_page_bodies: u32,
    /// A running fill continues for another live waiter when its leader
    /// expires (production: on for every fill except warm gates; set 0 to
    /// measure without it, Known issue 30).
    pub flight_handover: bool,
    /// Rank 0 asks a successor that recently missed it before origin
    /// (production: always on; set 0 to measure without it, Known issue 21).
    pub successor_hints: bool,
    /// Owners ask hinted later owners copy-only before origin, each probe on
    /// its own grant (production: always on; set 0 to measure without it,
    /// Known issue 21, round 4 orphan2). Needs `successor_hints`.
    pub successor_targets: bool,
    /// Non-owners walk the recent previous membership at a destination that
    /// is behind (production: always on; set 0 to measure without it, Known
    /// issue 19).
    pub stale_fallback: bool,
    /// Owners ask, readers visit first, and walks fund later owners that
    /// arrived before an owner (production `lineage_targets`,
    /// `lineage_first`: always on; set 0 to measure without it, Known issue
    /// 25).
    pub lineage: bool,
    /// A non-owner that cannot walk the previous membership waits once for
    /// a destination that is behind (production `stale_wait`: always on;
    /// set 0 to measure without it, Known issue 26).
    pub stale_wait: bool,
    /// A verified `StaleMembership` also returns the links past the
    /// answering node (production: always on; Known issues 47 and 49).
    pub stale_link_refund: bool,
    /// Owners also ask, before origin, members ranked just below the owners
    /// that were owners when they arrived (production `demoted_targets`:
    /// always on; set 0 to measure without it, Known issue 34). Needs
    /// `lineage`.
    pub demoted_targets: bool,
    /// Lineage targets are asked on their own grant, like successor targets
    /// (production: always on; set 0 to fund them from the walk's slack
    /// only, Known issue 35).
    pub lineage_grants: bool,
    /// With `lineage_grants`, readers and delegations still fund lineage
    /// probes from slack (`WalkConfig::lineage`) (production: off; Known
    /// issue 35). Without `lineage_grants` slack funding is always on.
    pub lineage_slack: bool,
    /// A copy-only request from an owner ranked before the destination does
    /// not wait on the destination's acquisition (production
    /// `copy_join_allowed`: always on; set 0 to measure without it, Known
    /// issue 44).
    pub copy_join_rank: bool,
    /// An owner above rank 0 that fetched a page from origin offers it to
    /// rank 0 later (production `orphan_notices`: always on; set 0 to
    /// measure without it, Known issue 37). Needs `successor_hints`.
    pub orphan_notices: bool,
    /// Owners above rank 0 remember rank 0 found fetching or overdue per
    /// page (production `Rank0Watch`: always on; set 0 to measure without
    /// it, Known issue 39).
    pub rank0_watch: bool,
    /// Granted probes (successor and lineage targets) go out at once
    /// (production `WalkConfig::parallel_granted`: always on; set 0 to send
    /// them one after the other, Known issue 45).
    pub parallel_granted: bool,
    /// Most lineage targets per owner walk (production
    /// `walk::MAX_LINEAGE`; set lower to measure, 3 is the wave 5 cap, Known
    /// issue 46).
    pub lineage_cap: usize,
    /// Demoted members whose arrival is the chain's start (unknown age) are
    /// lineage targets too when the owner arrived later (production
    /// `demoted_targets_from` with `oldest` 0: always on; set 0 to measure
    /// without, Known issue 46).
    pub unknown_age_targets: bool,
    /// Senders hold next hops that leave exchanges unanswered silent and
    /// fail requests for them at once (Known issues 41 and 43).
    pub silence: bool,
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
            hedge_bytes: DUPLICATE_BYTES,
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
        }
    }
}

impl NodeConfig {
    pub fn apply_json(&mut self, value: &Value) -> Result<(), String> {
        let obj = value
            .as_object()
            .ok_or_else(|| format!("node: expected an object, got {value}"))?;
        for (key, v) in obj {
            let n = v
                .as_u64()
                .ok_or_else(|| format!("node.{key}: expected a non-negative integer, got {v}"))?;
            let small = || u32::try_from(n).map_err(|_| format!("node.{key}: out of range"));
            match key.as_str() {
                "placement_entries" => self.placement_entries = small()?,
                "path_entries" => self.path_entries = small()?,
                "path_bytes" => self.path_bytes = n,
                "active_searches" => self.active_searches = small()?,
                "search_queue" => self.search_queue = small()?,
                "flights" => self.flights = small()?,
                "waiters_per_flight" => self.waiters_per_flight = small()?,
                "relay_transfers" => self.relay_transfers = small()?,
                "plaintext_bytes" => self.plaintext_bytes = n,
                "ciphertext_bytes" => self.ciphertext_bytes = n,
                "dirty_bytes" => self.dirty_bytes = n,
                "pages_per_segment" => self.pages_per_segment = small()?,
                "history_bytes" => self.history_bytes = small()?,
                "membership_batch_ns" => self.membership_batch_ns = n,
                "membership_lag_ns" => self.membership_lag_ns = n,
                "retained_versions" => self.retained_versions = n,
                "attempt_timeout_ns" => self.attempt_timeout_ns = n,
                "read_timeout_ns" => self.read_timeout_ns = n,
                "connect_timeout_ns" => self.connect_timeout_ns = n,
                "serve_timeout_ns" => self.serve_timeout_ns = n,
                "hedge_slots" => self.hedge_slots = small()?,
                "hedge_delay_ns" => self.hedge_delay_ns = n,
                "hedge_bytes" => self.hedge_bytes = n,
                "fanout" if n <= 1 => self.fanout = n == 1,
                "hot_page_bodies" => self.hot_page_bodies = small()?,
                "flight_handover" if n <= 1 => self.flight_handover = n == 1,
                "successor_hints" if n <= 1 => self.successor_hints = n == 1,
                "successor_targets" if n <= 1 => self.successor_targets = n == 1,
                "stale_fallback" if n <= 1 => self.stale_fallback = n == 1,
                "lineage" if n <= 1 => self.lineage = n == 1,
                "stale_wait" if n <= 1 => self.stale_wait = n == 1,
                "stale_link_refund" if n <= 1 => self.stale_link_refund = n == 1,
                "demoted_targets" if n <= 1 => self.demoted_targets = n == 1,
                "lineage_grants" if n <= 1 => self.lineage_grants = n == 1,
                "lineage_slack" if n <= 1 => self.lineage_slack = n == 1,
                "copy_join_rank" if n <= 1 => self.copy_join_rank = n == 1,
                "orphan_notices" if n <= 1 => self.orphan_notices = n == 1,
                "rank0_watch" if n <= 1 => self.rank0_watch = n == 1,
                "parallel_granted" if n <= 1 => self.parallel_granted = n == 1,
                "unknown_age_targets" if n <= 1 => self.unknown_age_targets = n == 1,
                "lineage_cap" if n <= racer_core::read::walk::MAX_LINEAGE as u64 => {
                    self.lineage_cap = n as usize;
                }
                "silence" if n <= 1 => self.silence = n == 1,
                _ => return Err(format!("unknown key node.{key}")),
            }
        }
        Ok(())
    }

    pub fn validate(&self) -> Result<(), String> {
        if self.flights == 0 || self.waiters_per_flight == 0 || self.relay_transfers == 0 {
            return Err(
                "node: flights, waiters_per_flight and relay_transfers must be nonzero".into(),
            );
        }
        if self.pages_per_segment == 0 || self.history_bytes == 0 {
            return Err("node: pages_per_segment and history_bytes must be nonzero".into());
        }
        if self.membership_batch_ns == 0 || self.membership_lag_ns > self.membership_batch_ns {
            return Err(
                "node: membership_lag_ns must not exceed a nonzero membership_batch_ns".into(),
            );
        }
        if self.attempt_timeout_ns == 0 || self.read_timeout_ns == 0 || self.serve_timeout_ns == 0 {
            return Err("node: timeouts must be nonzero".into());
        }
        if self.hedge_slots > 0 && self.hedge_bytes < DUPLICATE_BYTES {
            return Err("node: hedge_bytes must cover one duplicate page".into());
        }
        Ok(())
    }
}

/// Production `DUPLICATE_BYTES`: two pages plus a tag.
const DUPLICATE_BYTES: u64 = 2 * MIB + 16;

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn overrides_and_validation() {
        let mut c = NodeConfig::default();
        c.validate().unwrap();
        c.apply_json(&json!({"placement_entries": 9, "membership_lag_ns": 5}))
            .unwrap();
        assert_eq!((c.placement_entries, c.membership_lag_ns), (9, 5));
        assert!(c.flight_handover);
        c.apply_json(&json!({"flight_handover": 0})).unwrap();
        assert!(!c.flight_handover);
        for bad in [
            json!([]),
            json!({"nope": 1}),
            json!({"flights": -1}),
            json!({"flights": 1u64 << 40}),
            json!({"flight_handover": 2}),
        ] {
            assert!(NodeConfig::default().apply_json(&bad).is_err(), "{bad}");
        }
        let lagging = NodeConfig {
            membership_lag_ns: 2 * SECONDS,
            ..NodeConfig::default()
        };
        assert!(lagging.validate().is_err());
        let no_flights = NodeConfig {
            flights: 0,
            ..NodeConfig::default()
        };
        assert!(no_flights.validate().is_err());
    }
}
