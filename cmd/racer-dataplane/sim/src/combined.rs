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
use crate::faults::Fault;
use crate::node::NodeWorld;
use crate::node::world::Event;
use crate::resources::OriginErrors;
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

/// Data-plane faults layered on the control plans (`"combined_faults"` in
/// `--config`). Used only when `--config` gives no `faults` list.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct DataFaults {
    pub enabled: bool,
    /// Slow nodes per thousand (`full-lagging`, `full-cold-start`).
    pub slow_permille: u32,
    /// Slowdown factor of a slow node.
    pub slow_factor: u8,
    /// Origin 5xx probability during a burst (`full-failover`).
    pub p5xx: f64,
    /// Origin 412 probability during a burst (`full-churn`).
    pub p412: f64,
    /// Length of each origin error burst.
    pub burst_ns: Nanos,
    /// Site 0 and site 1 are cut apart (`full-failover`).
    pub partition: bool,
}

impl Default for DataFaults {
    fn default() -> Self {
        Self {
            enabled: true,
            slow_permille: 10,
            slow_factor: 8,
            p5xx: 0.3,
            p412: 0.1,
            burst_ns: 30 * SECONDS,
            partition: true,
        }
    }
}

impl DataFaults {
    pub fn apply_json(&mut self, value: &Value) -> Result<(), String> {
        let obj = value
            .as_object()
            .ok_or_else(|| format!("combined_faults: expected an object, got {value}"))?;
        for (key, v) in obj {
            let bad = || format!("combined_faults.{key}: invalid value {v}");
            match key.as_str() {
                "enabled" => self.enabled = v.as_bool().ok_or_else(bad)?,
                "partition" => self.partition = v.as_bool().ok_or_else(bad)?,
                "slow_permille" => {
                    self.slow_permille = v.as_u64().filter(|n| *n <= 1000).ok_or_else(bad)? as u32;
                }
                "slow_factor" => {
                    self.slow_factor = v
                        .as_u64()
                        .and_then(|n| u8::try_from(n).ok())
                        .filter(|n| *n >= 1)
                        .ok_or_else(bad)?;
                }
                "p5xx" => {
                    self.p5xx = v
                        .as_f64()
                        .filter(|p| (0.0..=1.0).contains(p))
                        .ok_or_else(bad)?
                }
                "p412" => {
                    self.p412 = v
                        .as_f64()
                        .filter(|p| (0.0..=1.0).contains(p))
                        .ok_or_else(bad)?
                }
                "burst_ns" => self.burst_ns = v.as_u64().ok_or_else(bad)?,
                _ => return Err(format!("unknown key combined_faults.{key}")),
            }
        }
        Ok(())
    }

    /// Every `1000 / slow_permille`-th node, slowed during `[from, until)`.
    fn slow(&self, nodes: u32, from_ns: Nanos, until_ns: Nanos, out: &mut Vec<Fault>) {
        if self.slow_permille == 0 || self.slow_factor <= 1 {
            return;
        }
        let stride = (1000 / self.slow_permille).max(1);
        for node in (stride / 2..nodes).step_by(stride as usize) {
            out.push(Fault::Slow {
                node,
                factor: self.slow_factor,
                from_ns,
                until_ns,
            });
        }
    }

    fn origin(&self, at: &[Nanos], errors: OriginErrors, out: &mut Vec<Fault>) {
        if errors.p412 + errors.p5xx <= 0.0 || self.burst_ns == 0 {
            return;
        }
        for from_ns in at {
            out.push(Fault::OriginErrors {
                from_ns: *from_ns,
                until_ns: from_ns + self.burst_ns,
                errors,
            });
        }
    }

    /// The default data-plane faults of `scenario` over `d` nanoseconds.
    pub fn plan(&self, scenario: &str, nodes: u32, d: Nanos) -> Vec<Fault> {
        let mut out = Vec::new();
        if !self.enabled {
            return out;
        }
        match scenario {
            "full-cold-start" => self.slow(nodes, d * 3 / 4 - d / 40, d * 3 / 4 + d / 20, &mut out),
            "full-failover" => {
                if self.partition {
                    out.push(Fault::Partition {
                        from_ns: d * 2 / 5,
                        until_ns: d / 2,
                        site_a: 0,
                        site_b: 1,
                    });
                }
                let errors = OriginErrors {
                    p5xx: self.p5xx,
                    ..OriginErrors::default()
                };
                self.origin(&[d / 5, d * 3 / 5], errors, &mut out);
            }
            "full-churn" => {
                let errors = OriginErrors {
                    p412: self.p412,
                    ..OriginErrors::default()
                };
                self.origin(&[d / 2], errors, &mut out);
            }
            _ => self.slow(nodes, d / 10, d * 9 / 10, &mut out),
        }
        out
    }
}

/// Data-plane schedules generated after `--config` (only when empty).
pub fn data_schedules(scenario: &str, config: &mut Config) {
    if config.faults.is_empty() {
        config.faults = config
            .combined_faults
            .plan(scenario, config.nodes, config.duration_ns);
    }
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

    /// Known issue 7: the Pod crashed for good at `d / 2 + 5 s` leaves the
    /// membership once its Node has been not ready for the grace, so peers
    /// stop choosing it. Before round 4 they chose it until the end.
    #[test]
    fn failover_drops_the_dead_member() {
        let r = quick("full-failover", 200, 300, 1, 0.2);
        assert_clean(&r);
        let age = &r["metrics"]["histograms"]["dead_member_link_age_s"];
        assert!(counter(&r, "dead_member_links") > 0, "{}", r["metrics"]);
        // Node monitor grace 40 s, readiness grace 30 s, one 10 s pass,
        // plus client propagation.
        assert!(age["max"].as_u64().unwrap() <= 90, "{age}");
        assert!(
            ctl_counter(&r, "ctl_commits") > 0 && counter(&r, "membership_publications") >= 2,
            "{}",
            r["metrics"]["counters"]
        );
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

    /// The combined plans carry data-plane faults by default; the knobs
    /// turn them off, and an explicit `faults` list replaces them.
    #[test]
    fn data_faults_join_the_control_plans() {
        let d = 300 * SECONDS;
        let faults = DataFaults::default();
        let failover = faults.plan("full-failover", 200, d);
        assert!(
            failover
                .iter()
                .any(|f| matches!(f, Fault::Partition { .. }))
        );
        assert_eq!(
            failover
                .iter()
                .filter(|f| matches!(f, Fault::OriginErrors { .. }))
                .count(),
            2
        );
        let lagging = faults.plan("full-lagging", 1000, d);
        assert_eq!(lagging.len(), 10);
        assert!(
            lagging
                .iter()
                .all(|f| matches!(f, Fault::Slow { factor: 8, .. }))
        );
        let mut off = faults;
        off.apply_json(&json!({"enabled": false})).unwrap();
        assert!(off.plan("full-failover", 200, d).is_empty());
        assert!(off.apply_json(&json!({"slow_factor": 0})).is_err());
        assert!(
            DataFaults::default()
                .apply_json(&json!({"nope": 1}))
                .is_err()
        );

        let r = quick("full-failover", 200, 300, 1, 0.2);
        assert_clean(&r);
        assert!(
            counter(&r, "origin_5xx") > 0,
            "{}",
            r["metrics"]["counters"]
        );
        assert!(
            counter(&r, "link_failures") > 0,
            "{}",
            r["metrics"]["counters"]
        );

        let mut config = Config {
            nodes: 200,
            ..Config::default()
        };
        data_defaults(&mut config);
        config.request_rate = 0.2;
        config.combined_faults.enabled = false;
        let r = run(
            "full-failover",
            config.clone(),
            1,
            Some(300 * SECONDS),
            None,
        )
        .unwrap();
        assert_clean(&r);
        assert_eq!(counter(&r, "origin_5xx"), 0);
        // An explicit fault list replaces the defaults.
        config.combined_faults.enabled = true;
        config.faults = vec![Fault::Slow {
            node: 1,
            factor: 2,
            from_ns: 0,
            until_ns: SECONDS,
        }];
        let r = run("full-failover", config, 1, Some(300 * SECONDS), None).unwrap();
        assert_eq!(counter(&r, "origin_5xx"), 0);
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
