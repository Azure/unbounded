//! Combined mode: the control plane ([`ControlSim`]) and the data plane
//! ([`NodeWorld`]) on one deterministic timeline.
//!
//! Pods map 1:1 to data-plane nodes (Pod `i` is node `i`). The two queues are
//! co-stepped: before the data plane processes an event at time `t`, every
//! control event at or before `t` runs (control first on ties). After each
//! control step the recorded [`Changes`] are applied to the data plane:
//!
//! - a committed membership version becomes a shared `topology::Membership`
//!   built from the published members (cached by version, with the previous
//!   version as predecessor, see [`NodeWorld::commit_membership`]);
//! - each node adopts exactly the version its control client applied, so
//!   lagging or partitioned clients produce real data-plane membership skew;
//!   a node whose client has applied nothing (not yet enrolled, or restarted)
//!   neither reads nor accepts peer links;
//! - a Pod crash crashes the node (its disk survives), a restart restarts it,
//!   and a removed Pod stays down.
//!
//! The trace hash mixes both timelines' hashes.

use serde_json::{Value, json};

use crate::config::Config;
use crate::control::{self, Changes, ControlSim, CtlFault, Life};
use crate::engine::{Nanos, SECONDS, mix};
use crate::node::NodeWorld;
use crate::node::world::Event;
use crate::workload::Storm;

/// Combined scenario names accepted by the CLI.
pub const SCENARIOS: &[&str] = &[
    "full-cold-start",
    "full-failover",
    "full-churn",
    "full-lagging",
];

/// The control scenario whose plan (settings and faults) a combined
/// scenario uses.
pub fn control_scenario(scenario: &str) -> &'static str {
    match scenario {
        "full-cold-start" => "enrollment-storm",
        "full-failover" => "failover",
        "full-churn" => "ctl-churn",
        _ => "lagging",
    }
}

/// Default simulated duration of a combined scenario.
pub fn default_duration(scenario: &str) -> Nanos {
    match scenario {
        "full-cold-start" => 400 * SECONDS,
        "full-failover" => 900 * SECONDS,
        "full-churn" => 1200 * SECONDS,
        _ => 900 * SECONDS,
    }
}

/// Data-plane defaults for a combined scenario (before `--config`): a light
/// Zipf read load so 50,000 nodes stay tractable.
pub fn data_defaults(config: &mut Config) {
    config.objects = 4096;
    config.zipf_s = 1.0;
    config.request_rate = 0.02;
}

/// Data-plane schedules generated after `--config` (only when empty).
pub fn data_schedules(scenario: &str, config: &mut Config) {
    if scenario == "full-cold-start" && config.storms.is_empty() {
        // Once everyone has enrolled (about 280 s at 50,000 Pods), every
        // node reads object 0 within one second.
        config.storms.push(Storm {
            at_ns: config.duration_ns * 3 / 4,
            object: 0,
            nodes: config.nodes,
            window_ns: SECONDS,
        });
    }
}

pub struct Combined {
    pub node: NodeWorld,
    pub ctl: ControlSim,
    pruned_ns: Nanos,
}

impl Combined {
    pub fn new(
        config: Config,
        settings: control::Settings,
        seed: u64,
        faults: Vec<CtlFault>,
    ) -> Self {
        let mut ctl = ControlSim::new(settings, seed, faults);
        ctl.record_changes();
        Self {
            node: NodeWorld::new_combined(config, seed),
            ctl,
            pruned_ns: 0,
        }
    }

    /// Run both timelines to `end`.
    pub fn run(&mut self, end: Nanos) {
        loop {
            let tn = self.node.engine.next_at().filter(|t| *t <= end);
            let tc = self.ctl.next_event_ns().filter(|t| *t <= end);
            match (tn, tc) {
                (None, None) => break,
                (tn, Some(c)) if tn.is_none_or(|n| c <= n) => {
                    self.ctl.advance_to(c);
                    self.sync(c);
                }
                _ => {
                    self.node.step(end);
                }
            }
        }
        self.ctl.advance_to(end);
        self.sync(end);
        self.node.run(end);
    }

    /// Apply control changes to the data plane at time `at`.
    fn sync(&mut self, at: Nanos) {
        if !self.ctl.has_changes() {
            return;
        }
        let Changes {
            memberships,
            applied,
            lives,
        } = self.ctl.drain();
        for (version, pods) in memberships {
            self.node.commit_membership(version, pods, at);
        }
        let changed = !applied.is_empty();
        for (pod, version) in applied {
            self.node.apply_membership(pod, version);
        }
        for (pod, life) in lives {
            let event = match life {
                Life::Crashed | Life::Removed => Event::Crash { node: pod },
                Life::Restarted => Event::Restart { node: pod },
            };
            self.node.engine.at(at, event);
        }
        // Clients catching up release old versions; prune at most once per
        // simulated second to keep the O(nodes) scan cheap.
        if changed && at >= self.pruned_ns + SECONDS {
            self.pruned_ns = at;
            self.node.prune_versions();
        }
    }

    pub fn trace_hash(&self) -> u64 {
        mix(self.node.engine.trace.value() ^ self.ctl.trace_hash().rotate_left(29))
    }

    pub fn report(&mut self, scenario: &str, seed: u64) -> Value {
        let mut report = self.node.report(scenario, seed);
        let ctl = self.ctl.report(scenario, seed);
        let ctl_failures = ctl["invariants"]["hard_failures"].as_u64().unwrap_or(0);
        let node_failures = report["invariants"]["hard_failures"].as_u64().unwrap_or(0);
        report["logic"] = json!("combined");
        report["trace_hash"] = json!(format!("{:016x}", self.trace_hash()));
        report["data_trace_hash"] = json!(format!("{:016x}", self.node.engine.trace.value()));
        report["events"] =
            json!(self.node.engine.processed() + ctl["events"].as_u64().unwrap_or(0));
        let mut extra = 0u64;
        for (name, bytes) in self.ctl.memory() {
            report["memory"]["components"][name] = json!(bytes);
            extra += bytes as u64;
        }
        let total = report["memory"]["accounted_bytes"].as_u64().unwrap_or(0) + extra;
        report["memory"]["accounted_bytes"] = json!(total);
        report["memory"]["accounted_bytes_per_node"] =
            json!(total / u64::from(self.node.config.nodes.max(1)));
        let skew = self.skew();
        report["combined"] = json!({
            "hard_failures": node_failures + ctl_failures,
            "control_invariants": ctl["invariants"],
            "control_metrics": ctl["metrics"],
            "control": ctl["control"],
            "membership_skew_at_end": skew,
        });
        report["invariants"]["hard_failures"] = json!(node_failures + ctl_failures);
        report
    }

    /// Nodes (not down) by how many committed versions they trail at the end.
    fn skew(&self) -> Value {
        let latest = self.ctl.committed_membership_version().unwrap_or(0);
        let mut current = 0u64;
        let mut behind = 0u64;
        let mut unenrolled = 0u64;
        for n in 0..self.node.config.nodes {
            if self.node.is_down(n) {
                continue;
            }
            match self.ctl.membership_version(n) {
                0 => unenrolled += 1,
                v if v == latest => current += 1,
                _ => behind += 1,
            }
        }
        json!({"current": current, "behind": behind, "unenrolled": unenrolled, "latest": latest})
    }
}

/// Build and run a combined scenario. `overrides` is the `"control"` object
/// from `--config`; `config` is the data-plane configuration.
pub fn run(
    scenario: &str,
    mut config: Config,
    seed: u64,
    duration: Option<Nanos>,
    overrides: Option<&Value>,
) -> Result<Value, String> {
    let end = duration.unwrap_or_else(|| default_duration(scenario));
    config.duration_ns = end;
    data_schedules(scenario, &mut config);
    let (mut settings, _, faults) =
        control::plan(control_scenario(scenario), config.nodes, Some(end));
    if let Some(o) = overrides {
        settings.apply_json(o)?;
        settings.initial_pods = settings.initial_pods.clamp(1, config.nodes);
    }
    let mut world = Combined::new(config, settings, seed, faults);
    world.run(end);
    let mut report = world.report(scenario, seed);
    report["duration_ns"] = json!(end);
    Ok(report)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn quick(scenario: &str, nodes: u32, secs: u64, seed: u64, rate: f64) -> Value {
        let mut config = Config {
            nodes,
            ..Config::default()
        };
        data_defaults(&mut config);
        config.request_rate = rate;
        run(scenario, config, seed, Some(secs * SECONDS), None).unwrap()
    }

    fn counter(r: &Value, k: &str) -> u64 {
        r["metrics"]["counters"][k].as_u64().unwrap_or(0)
    }

    fn ctl_counter(r: &Value, k: &str) -> u64 {
        r["combined"]["control_metrics"]["counters"][k]
            .as_u64()
            .unwrap_or(0)
    }

    fn assert_clean(r: &Value) {
        assert_eq!(
            r["invariants"]["hard_failures"], 0,
            "{}\n{}",
            r["invariants"], r["combined"]["control_invariants"]
        );
    }

    #[test]
    fn cold_start_reads_after_enrollment() {
        let r = quick("full-cold-start", 200, 120, 1, 0.05);
        assert_clean(&r);
        assert_eq!(r["combined"]["control"]["enrolled"], 200);
        assert!(
            counter(&r, "reads_skipped_not_enrolled") > 0,
            "{}",
            r["metrics"]
        );
        assert!(counter(&r, "reads_completed") > 0);
        assert!(counter(&r, "flight_joins") + counter(&r, "peer_requests_acquire") > 0);
        assert!(counter(&r, "membership_publications") >= 1);
        let max = r["origin_per_page"]["max_requests_per_page"]
            .as_u64()
            .unwrap();
        assert!(max <= topology::REPLICAS as u64 * 2, "{max}");
    }

    #[test]
    fn failover_crashes_data_nodes_too() {
        let r = quick("full-failover", 200, 300, 1, 0.2);
        assert_clean(&r);
        assert!(ctl_counter(&r, "ctl_primary_terms") >= 3);
        assert!(counter(&r, "crashes") >= 3, "{}", r["metrics"]["counters"]);
        assert!(counter(&r, "restarts") >= 1);
        assert!(counter(&r, "reads_completed") > 0);
    }

    #[test]
    fn churn_grows_the_data_plane() {
        let r = quick("full-churn", 320, 400, 1, 0.1);
        assert_clean(&r);
        assert!(
            counter(&r, "membership_publications") >= 4,
            "{}",
            r["metrics"]["counters"]
        );
        assert!(ctl_counter(&r, "ctl_pods_removed") > 0);
        assert!(counter(&r, "membership_adoptions") > 0);
        assert!(counter(&r, "reads_completed") > 0);
    }

    #[test]
    fn lagging_clients_skew_the_data_plane() {
        let r = quick("full-lagging", 300, 600, 1, 0.2);
        assert_clean(&r);
        assert!(ctl_counter(&r, "ctl_unreachable") > 0);
        assert!(counter(&r, "reads_completed") > 0);
        assert!(
            counter(&r, "stale_membership_replies") + counter(&r, "error_incompatible_membership")
                > 0,
            "{}",
            r["metrics"]["counters"]
        );
    }

    #[test]
    fn replays_exactly() {
        for s in SCENARIOS {
            let a = quick(s, 80, 120, 3, 0.1);
            let b = quick(s, 80, 120, 3, 0.1);
            assert_eq!(a["trace_hash"], b["trace_hash"], "{s}");
            assert_eq!(a["metrics"], b["metrics"], "{s}");
        }
        assert_ne!(
            quick("full-churn", 80, 120, 3, 0.1)["trace_hash"],
            quick("full-churn", 80, 120, 4, 0.1)["trace_hash"]
        );
    }

    #[test]
    fn crashed_pod_is_down_and_comes_back() {
        let mut config = Config {
            nodes: 40,
            request_rate: 0.0,
            ..Config::default()
        };
        config.duration_ns = 200 * SECONDS;
        let (settings, _, _) = control::plan("enrollment-storm", 40, None);
        let faults = vec![CtlFault::CrashSeats {
            at_ns: 100 * SECONDS,
            count: 1,
            restart_after_ns: Some(30 * SECONDS),
        }];
        let mut w = Combined::new(config, settings, 5, faults);
        w.run(90 * SECONDS);
        let latest = w.ctl.committed_membership_version().unwrap();
        assert!((0..40).all(|n| !w.node.is_down(n)));
        assert_eq!(w.node.versions.latest_number(), latest);
        w.run(101 * SECONDS);
        let down: Vec<u32> = (0..40).filter(|n| w.node.is_down(*n)).collect();
        assert_eq!(down.len(), 1);
        assert!(!w.node.versions.enrolled(down[0]), "client restarts empty");
        w.run(200 * SECONDS);
        assert!(!w.node.is_down(down[0]));
        assert!(w.node.versions.enrolled(down[0]));
    }
}
