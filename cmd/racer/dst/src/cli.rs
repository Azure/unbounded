//! One-scenario report mode of the `racer-dst` binary
//! (`racer-dst --scenario NAME --nodes N --seed S --duration D --config FILE --out PATH`) on
//! the Model fidelity. The report is the Model's JSON report (`model::run`) with top-level
//! `scenario`, `seed`, `duration_ns` and `wall_ms` fields.
//!
//! Scenarios: the control plans (`enrollment-storm`, `failover`, `ctl-churn`, `lagging`),
//! the combined scenarios (`full-cold-start`, `full-failover`, `full-churn`, `full-lagging`),
//! and every catalog scenario by name (`model-*`, `authority-*`, ...).
//!
//! `--config` takes the JSON keys the Model has a counterpart for (see [`KEYS`]); any other
//! key is an error rather than silently ignored.
use std::rc::Rc;

use serde_json::{Map, Value, json};

use crate::{
    Choices, Scenario,
    engine::{MILLIS, Nanos, SECONDS},
    model::{self, ModelSpec, PeerConnections},
    scenarios::{self, DataFaults},
};

/// Usage text.
pub const USAGE: &str = "usage: racer-dst [--scenario NAME] [--nodes N] [--seed S] \
[--duration D] [--config FILE.json] [--out PATH]\n\
scenarios: enrollment-storm, failover, ctl-churn, lagging,\n\
           full-cold-start, full-failover, full-churn, full-lagging,\n\
           aliases storm, churn, faults, smoke, caches-only, zipf and empty,\n\
           or any racer-dst catalog scenario name\n\
durations: integer with ns, us, ms or s suffix (plain numbers are seconds)\n\
without arguments racer-dst runs its seed set (DST_SEEDS, DST_STEPS, DST_FILTER)";

/// Control-plane scenario names: each runs a control plan ([`crate::model::control::plan`]).
pub const CONTROL: &[&str] = &["enrollment-storm", "failover", "ctl-churn", "lagging"];

/// Short scenario aliases and the catalog scenario each runs. `storm` is
/// [`scenarios::cold_start`] at `--nodes`; the steady-load aliases (`smoke`, `caches-only`, `zipf`) run the combined Zipf load, and `empty` runs it
/// with no reads.
pub const ALIASES: &[(&str, &str)] = &[
    ("storm", "storm"),
    ("churn", "model-membership-churn"),
    ("faults", "model-faults"),
    ("smoke", "full-cold-start"),
    ("caches-only", "full-cold-start"),
    ("zipf", "full-cold-start"),
    ("empty", "full-cold-start"),
];

/// Topology-only scenario names; they run as `topology/tests/mesh_scenarios.rs`, not here.
pub const MESH: &[&str] = &["mesh", "mesh-selector"];

/// Combined scenarios: a control plan co-stepped with the Model read path.
pub const COMBINED: &[&str] = &[
    "full-cold-start",
    "full-failover",
    "full-churn",
    "full-lagging",
];

/// Top-level `--config` keys the Model takes.
pub const KEYS: &[&str] = &[
    "nodes",
    "sites",
    "workers",
    "duration_ns",
    "request_rate",
    "objects",
    "zipf_s",
    "range_fraction",
    "range_pages",
    "page_bytes",
    "pages_per_object",
    "origin_errors",
    "control",
    "combined_faults",
    "node",
];

/// `"node"` keys the Model takes.
pub const NODE_KEYS: &[&str] = &[
    "crypto_queue",
    "relay_transfers",
    "waiters_per_flight",
    "connections",
    "circuits",
];

/// Parsed command line.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Options {
    /// Scenario name.
    pub scenario: String,
    /// `--nodes`, over the config's `nodes`.
    pub nodes: Option<u32>,
    /// Seed.
    pub seed: u64,
    /// `--duration`, over the config's `duration_ns`.
    pub duration: Option<Nanos>,
    /// `--config` file.
    pub config: Option<String>,
    /// `--out` file (stdout when unset).
    pub out: Option<String>,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            scenario: "full-cold-start".to_owned(),
            nodes: None,
            seed: 1,
            duration: None,
            config: None,
            out: None,
        }
    }
}

/// Parse `10s`, `250ms`, `5us`, `7ns` or a plain number of seconds.
pub fn parse_duration(raw: &str) -> Result<Nanos, String> {
    let (digits, unit) = match raw.find(|c: char| !c.is_ascii_digit()) {
        Some(i) => raw.split_at(i),
        None => (raw, "s"),
    };
    let n: u64 = digits
        .parse()
        .map_err(|_| format!("bad duration {raw:?}"))?;
    let scale = match unit {
        "ns" => 1,
        "us" => 1_000,
        "ms" => MILLIS,
        "s" => SECONDS,
        _ => return Err(format!("bad duration unit in {raw:?}")),
    };
    n.checked_mul(scale)
        .ok_or_else(|| format!("duration {raw:?} overflows"))
}

impl Options {
    /// Parse `--flag value` and `--flag=value` arguments.
    pub fn parse(args: &[String]) -> Result<Self, String> {
        let mut options = Self::default();
        let mut iter = args.iter();
        while let Some(arg) = iter.next() {
            let (flag, inline) = match arg.split_once('=') {
                Some((flag, value)) => (flag, Some(value.to_owned())),
                None => (arg.as_str(), None),
            };
            let mut value = || {
                inline
                    .clone()
                    .or_else(|| iter.next().cloned())
                    .ok_or(format!("{flag} needs a value"))
            };
            match flag {
                "--scenario" => options.scenario = value()?,
                "--nodes" => {
                    let raw = value()?;
                    let n: u32 = raw.parse().map_err(|_| format!("bad --nodes {raw:?}"))?;
                    if n == 0 {
                        return Err("--nodes must be at least 1".to_owned());
                    }
                    options.nodes = Some(n);
                }
                "--seed" => {
                    let raw = value()?;
                    options.seed = raw.parse().map_err(|_| format!("bad --seed {raw:?}"))?;
                }
                "--duration" => options.duration = Some(parse_duration(&value()?)?),
                "--config" => options.config = Some(value()?),
                "--out" => options.out = Some(value()?),
                _ => return Err(format!("unknown argument {arg:?}")),
            }
        }
        if MESH.contains(&options.scenario.as_str()) {
            return Err(format!(
                "scenario {:?} measures topology only; run `cargo test -p topology --test \
                 mesh_scenarios`",
                options.scenario
            ));
        }
        let known = CONTROL.contains(&options.scenario.as_str())
            || ALIASES.iter().any(|(n, _)| *n == options.scenario)
            || COMBINED.contains(&options.scenario.as_str())
            || scenarios::find(&options.scenario).is_some();
        if !known {
            return Err(format!("unknown scenario {:?}", options.scenario));
        }
        Ok(options)
    }
}

/// `--config` values, validated.
#[derive(Debug, Default)]
struct Config {
    nodes: Option<u32>,
    sites: Option<u32>,
    duration: Option<Nanos>,
    request_rate: Option<f64>,
    control: Option<Value>,
    faults: DataFaults,
    /// Settings applied to the scenario's [`ModelSpec`], in order.
    spec: Vec<(String, Value)>,
}

fn object<'a>(name: &str, value: &'a Value) -> Result<&'a Map<String, Value>, String> {
    value
        .as_object()
        .ok_or_else(|| format!("{name}: expected an object, got {value}"))
}

fn u64_of(key: &str, v: &Value) -> Result<u64, String> {
    v.as_u64()
        .ok_or_else(|| format!("{key}: expected an unsigned integer, got {v}"))
}

fn u32_of(key: &str, v: &Value) -> Result<u32, String> {
    u32::try_from(u64_of(key, v)?).map_err(|_| format!("{key}: {v} is too large"))
}

fn f64_of(key: &str, v: &Value) -> Result<f64, String> {
    v.as_f64()
        .ok_or_else(|| format!("{key}: expected a number, got {v}"))
}

fn bool_of(key: &str, v: &Value) -> Result<bool, String> {
    v.as_bool()
        .ok_or_else(|| format!("{key}: expected true or false, got {v}"))
}

impl Config {
    fn parse(value: &Value) -> Result<Self, String> {
        let mut config = Self::default();
        for (key, v) in object("config", value)? {
            match key.as_str() {
                "nodes" => {
                    let n = u32_of(key, v)?;
                    if n == 0 {
                        return Err("nodes must be at least 1".into());
                    }
                    config.nodes = Some(n);
                }
                "sites" => config.sites = Some(u32_of(key, v)?.max(1)),
                "duration_ns" => config.duration = Some(u64_of(key, v)?),
                "request_rate" => config.request_rate = Some(f64_of(key, v)?),
                "control" => {
                    object("control", v)?;
                    config.control = Some(v.clone());
                }
                "combined_faults" => config.faults.apply_json(v)?,
                "node" => {
                    for (k, nv) in object("node", v)? {
                        if !NODE_KEYS.contains(&k.as_str()) {
                            return Err(format!(
                                "node.{k} has no Model counterpart (supported: {NODE_KEYS:?})"
                            ));
                        }
                        config.spec.push((format!("node.{k}"), nv.clone()));
                    }
                }
                k if KEYS.contains(&k) => config.spec.push((key.clone(), v.clone())),
                _ => {
                    return Err(format!(
                        "{key} has no Model counterpart (supported: {KEYS:?})"
                    ));
                }
            }
        }
        Ok(config)
    }

    /// Apply the spec settings to `spec`.
    fn apply(&self, spec: &mut ModelSpec) -> Result<(), String> {
        for (key, v) in &self.spec {
            fn workload<'a>(
                key: &str,
                spec: &'a mut ModelSpec,
            ) -> Result<&'a mut model::Workload, String> {
                spec.workload
                    .as_mut()
                    .ok_or_else(|| format!("{key}: the scenario has no steady read load"))
            }
            match key.as_str() {
                "workers" => spec.workers = u32_of(key, v)?.max(1),
                "page_bytes" => spec.page_bytes = u64_of(key, v)?,
                "pages_per_object" => spec.pages_per_object = u32_of(key, v)?.max(1),
                "objects" => workload(key, spec)?.objects = u64_of(key, v)?.max(1),
                "zipf_s" => workload(key, spec)?.zipf_s = f64_of(key, v)?,
                "range_fraction" => workload(key, spec)?.range_fraction = f64_of(key, v)?,
                "range_pages" => workload(key, spec)?.range_pages = u32_of(key, v)?.max(1),
                "origin_errors" => {
                    for (k, p) in object("origin_errors", v)? {
                        let p = f64_of(k, p)?;
                        match k.as_str() {
                            "p412" => spec.origin_errors.p412 = p,
                            "p403" => spec.origin_errors.p403 = p,
                            "p5xx" => spec.origin_errors.p5xx = p,
                            _ => return Err(format!("unknown key origin_errors.{k}")),
                        }
                    }
                }
                "node.crypto_queue" => spec.crypto_queue = u64_of(key, v)? as usize,
                "node.relay_transfers" => spec.relay_transfers = u64_of(key, v)? as usize,
                "node.waiters_per_flight" => spec.waiters_per_flight = u64_of(key, v)? as usize,
                "node.connections" => {
                    spec.connections = if bool_of(key, v)? {
                        PeerConnections::Production
                    } else {
                        PeerConnections::Off
                    };
                }
                "node.circuits" => spec.circuits = bool_of(key, v)?,
                _ => unreachable!("validated in parse: {key}"),
            }
        }
        Ok(())
    }
}

/// Build the scenario `options` names with `config` applied.
fn build(options: &Options, config: &Config) -> Result<Scenario, String> {
    let sim_name = options.scenario.as_str();
    let name = ALIASES
        .iter()
        .find(|(n, _)| *n == sim_name)
        .map_or(sim_name, |(_, to)| to);
    let nodes = options.nodes.or(config.nodes);
    let duration = options.duration.or(config.duration);
    let rate = match sim_name {
        "empty" => Some(config.request_rate.unwrap_or(0.0)),
        _ => config.request_rate,
    };
    let mut scenario = if name == "storm" {
        let mut scenario = scenarios::cold_start(nodes.unwrap_or(16));
        if let Some(d) = duration {
            scenario = scenario.end(d);
        }
        scenario
    } else if COMBINED.contains(&name) {
        // Defaults: 0.02 reads per node per second on 16 nodes.
        let secs = duration.map(|d| d.div_ceil(SECONDS));
        let nodes = nodes.unwrap_or(16);
        scenarios::combined_with(name, nodes, rate.unwrap_or(0.02), secs, &config.faults)
    } else {
        if rate.is_some() {
            return Err(format!("request_rate: {name} has no steady read load"));
        }
        let mut scenario = if CONTROL.contains(&name) {
            let plan: &'static str = CONTROL.iter().find(|p| **p == name).expect("listed");
            scenarios::control(plan, nodes.unwrap_or(16))
        } else {
            if nodes.is_some() {
                return Err(format!("nodes: catalog scenario {name} has a fixed size"));
            }
            scenarios::find(name).expect("checked in parse")
        };
        if let Some(d) = duration {
            scenario = scenario.end(d);
        }
        scenario
    };
    if let Some(sites) = config.sites {
        scenario = scenario.sites(sites);
    }
    let spec = scenario
        .model
        .as_mut()
        .map(|s| Rc::get_mut(s).expect("a freshly built scenario owns its spec"));
    match spec {
        Some(spec) => {
            config.apply(spec)?;
            if let Some(o) = &config.control {
                let ctl = spec
                    .control
                    .as_mut()
                    .ok_or_else(|| format!("control: {name} has no control plane"))?;
                ctl.overrides = Some(o.clone());
            }
        }
        None if !config.spec.is_empty() || config.control.is_some() => {
            return Err(format!(
                "{name} is not a Model scenario; only --seed applies"
            ));
        }
        None => {}
    }
    Ok(scenario)
}

/// Run `options` and return the report and whether the run failed.
pub fn run(options: &Options) -> Result<(Value, bool), String> {
    let config = match &options.config {
        Some(path) => {
            let text = std::fs::read_to_string(path).map_err(|e| format!("read {path}: {e}"))?;
            let value: Value =
                serde_json::from_str(&text).map_err(|e| format!("parse config: {e}"))?;
            Config::parse(&value)?
        }
        None => Config::default(),
    };
    let scenario = build(options, &config)?;
    #[allow(clippy::disallowed_methods)] // Wall time is reported, never simulated.
    let started = std::time::Instant::now();
    let (outcome, mut report) = if scenario.model.is_some() {
        model::run(&scenario, Choices::seeded(options.seed))
    } else {
        (scenario.run(Choices::seeded(options.seed)), json!({}))
    };
    report["scenario"] = json!(options.scenario);
    report["dst_scenario"] = json!(scenario.name);
    report["seed"] = json!(options.seed);
    report["duration_ns"] = json!(outcome.end);
    report["trace_hash"] = json!(format!("{:016x}", outcome.trace));
    report["wall_ms"] = json!(started.elapsed().as_millis() as u64);
    report["failure"] = json!(outcome.failure.as_ref().map(ToString::to_string));
    Ok((report, outcome.failure.is_some()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| (*s).to_owned()).collect()
    }

    fn config(text: &str) -> Result<Config, String> {
        Config::parse(&serde_json::from_str(text).unwrap())
    }

    #[test]
    fn parses_options_and_durations() {
        let o = Options::parse(&args(&[
            "--scenario",
            "full-failover",
            "--nodes=200",
            "--seed",
            "7",
            "--duration",
            "300s",
            "--config=c.json",
            "--out",
            "r.json",
        ]))
        .unwrap();
        assert_eq!(
            o,
            Options {
                scenario: "full-failover".into(),
                nodes: Some(200),
                seed: 7,
                duration: Some(300 * SECONDS),
                config: Some("c.json".into()),
                out: Some("r.json".into()),
            }
        );
        for bad in [
            &["--nodes"][..],
            &["--nodes", "0"],
            &["--seed", "-1"],
            &["--bogus"],
            &["--scenario", "nope"],
            &["--duration", "5h"],
        ] {
            assert!(Options::parse(&args(bad)).is_err(), "{bad:?}");
        }
        assert_eq!(parse_duration("2").unwrap(), 2 * SECONDS);
        assert_eq!(parse_duration("250ms").unwrap(), 250 * MILLIS);
        assert!(parse_duration("99999999999999999999s").is_err());
        assert!(Options::parse(&args(&["--scenario", "model-hung-peers"])).is_ok());
    }

    #[test]
    fn config_overrides_reach_the_scenario() {
        let c = config(
            r#"{"nodes": 40, "sites": 2, "request_rate": 0.5, "zipf_s": 1.2,
                "combined_faults": {"partition": false, "p5xx": 0.0},
                "node": {"crypto_queue": 64, "circuits": false},
                "control": {"initial_pods": 4}}"#,
        )
        .unwrap();
        let o = Options {
            scenario: "full-failover".into(),
            duration: Some(60 * SECONDS),
            ..Options::default()
        };
        let s = build(&o, &c).unwrap();
        assert_eq!((s.nodes, s.sites, s.end), (40, 2, 60 * SECONDS));
        let spec = s.model.as_ref().unwrap();
        let w = spec.workload.unwrap();
        assert_eq!((w.rate, w.zipf_s), (0.5, 1.2));
        assert!(spec.site_cuts.is_empty() && spec.origin_bursts.is_empty());
        assert_eq!(spec.crypto_queue, 64);
        assert!(!spec.circuits);
        let ctl = spec.control.as_ref().unwrap();
        assert_eq!(ctl.overrides.as_ref().unwrap()["initial_pods"], 4);
        // `--nodes` wins over the config.
        let o = Options {
            nodes: Some(30),
            ..o
        };
        assert_eq!(build(&o, &c).unwrap().nodes, 30);
    }

    #[test]
    fn unsupported_config_is_an_error() {
        for bad in [
            r#"{"hop_limit": 3}"#,
            r#"{"node": {"hedge_slots": 1}}"#,
            r#"{"combined_faults": {"bogus": 1}}"#,
            r#"{"origin_errors": {"p999": 0.1}}"#,
            r#"{"nodes": 0}"#,
            r#"[]"#,
        ] {
            let parsed = config(bad);
            let built = parsed.and_then(|c| build(&Options::default(), &c));
            assert!(built.is_err(), "{bad}");
        }
        // A catalog scenario has a fixed size and no steady read load.
        let fixed = Options {
            scenario: "model-hung-peers".into(),
            nodes: Some(3),
            ..Options::default()
        };
        assert!(build(&fixed, &Config::default()).is_err());
        let rate = config(r#"{"request_rate": 1.0}"#).unwrap();
        let control = Options {
            scenario: "failover".into(),
            ..Options::default()
        };
        assert!(build(&control, &rate).is_err());
    }

    #[test]
    fn runs_a_combined_scenario_with_report_fields() {
        let o = Options {
            scenario: "full-failover".into(),
            nodes: Some(40),
            duration: Some(300 * SECONDS),
            ..Options::default()
        };
        let (r, failed) = run(&o).unwrap();
        assert!(!failed, "{}", r["failure"]);
        assert_eq!(r["scenario"], "full-failover");
        assert_eq!(r["seed"], 1);
        assert!(r["metrics"]["counters"]["reads_completed"].as_u64() > Some(0));
        assert!(r["origin_per_page"]["repeats"].is_object());
        assert!(r["trace_hash"].is_string() && r["wall_ms"].is_u64());
        let missing = Options {
            config: Some("/nonexistent/racer-dst/config.json".into()),
            ..o
        };
        assert!(run(&missing).is_err());
    }

    /// Every non-catalog `--scenario` name listed in [`USAGE`] runs and reports under its own
    /// name, except the topology-only mesh scenarios, which point at the topology tests.
    #[test]
    fn every_named_scenario_runs() {
        let names = CONTROL
            .iter()
            .chain(COMBINED)
            .chain(ALIASES.iter().map(|(n, _)| n));
        for name in names {
            let catalog = matches!(*name, "churn" | "faults");
            let o = Options {
                scenario: (*name).into(),
                nodes: (!catalog).then_some(8),
                duration: Some(20 * SECONDS),
                ..Options::default()
            };
            let parsed = Options::parse(&args(&["--scenario", name])).unwrap();
            assert_eq!(parsed.scenario, *name);
            let (r, _) = run(&o).unwrap_or_else(|e| panic!("{name}: {e}"));
            assert_eq!(r["scenario"], *name);
            assert!(r["dst_scenario"].is_string(), "{name}");
        }
        for name in MESH {
            let error = Options::parse(&args(&["--scenario", name])).unwrap_err();
            assert!(error.contains("mesh_scenarios"), "{error}");
        }
    }
}
