//! Discrete-event simulator for Racer.
//!
//! Runs Racer decision logic for many nodes in one process and one thread.
//! See `designs/racer-core-simulation.md`.
//!
//! Layout:
//! - [`engine`]: deterministic event queue (u64 ns, `(time, seq)` order),
//!   per-role seeded RNG streams, order-sensitive trace hash.
//! - [`resources`]: FIFO servers for NIC tx/rx (cut-through per chunk), disk,
//!   CPU workers and crypto helper, site RTT matrix, origin.
//! - [`workload`] and [`faults`]: Poisson, Zipf, ranges, storms, churn,
//!   partitions, crashes, slow nodes, origin errors.
//! - [`report`]: histograms, counters, invariants, memory accounting.
//! - [`logic`]: the [`logic::NodeLogic`] plug-in trait and stand-ins; the
//!   module docs list where real `racer-core` logic plugs in.
//! - [`scenario`]: the world and the placeholder caches-only read flow.

pub mod config;
pub mod engine;
pub mod faults;
pub mod logic;
pub mod report;
pub mod resources;
pub mod scenario;
pub mod workload;

use serde_json::Value;

use crate::config::Config;
use crate::engine::{MILLIS, Nanos, Role, SECONDS, Streams};
use crate::faults::Fault;
use crate::logic::StandIn;
use crate::scenario::World;
use crate::workload::{Churn, Storm};

pub const USAGE: &str = "usage: racer-sim [--scenario NAME] [--nodes N] [--seed S] \
[--duration D] [--config FILE.json] [--out PATH]\n\
scenarios: smoke, caches-only, zipf, storm, churn, faults, empty\n\
durations: integer with ns, us, ms or s suffix (plain numbers are seconds)";

pub const SCENARIOS: &[&str] = &[
    "smoke",
    "caches-only",
    "zipf",
    "storm",
    "churn",
    "faults",
    "empty",
];

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Options {
    pub scenario: String,
    pub nodes: Option<u32>,
    pub seed: u64,
    pub duration_ns: Option<Nanos>,
    pub config: Option<String>,
    pub out: Option<String>,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            scenario: "smoke".to_owned(),
            nodes: None,
            seed: 1,
            duration_ns: None,
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
                "--scenario" => {
                    let s = value()?;
                    if !SCENARIOS.contains(&s.as_str()) {
                        return Err(format!("unknown scenario {s:?}"));
                    }
                    options.scenario = s;
                }
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
                "--duration" => options.duration_ns = Some(parse_duration(&value()?)?),
                "--config" => options.config = Some(value()?),
                "--out" => options.out = Some(value()?),
                _ => return Err(format!("unknown argument {arg:?}")),
            }
        }
        Ok(options)
    }

    /// Build the configuration: scenario defaults, then `--config` JSON,
    /// then `--nodes` and `--duration`, then scenario-generated schedules
    /// (only when the config left them empty).
    pub fn build_config(&self) -> Result<Config, String> {
        let text = match &self.config {
            Some(path) => {
                Some(std::fs::read_to_string(path).map_err(|e| format!("read {path}: {e}"))?)
            }
            None => None,
        };
        self.build_config_with(text.as_deref())
    }

    /// [`Self::build_config`] with the `--config` file contents given.
    pub fn build_config_with(&self, json: Option<&str>) -> Result<Config, String> {
        let mut config = Config::default();
        if self.scenario == "zipf" {
            config.objects = 4096;
            config.zipf_s = 1.0;
        }
        if let Some(text) = json {
            let value: Value =
                serde_json::from_str(text).map_err(|e| format!("parse config: {e}"))?;
            config.apply_json(&value)?;
        }
        if let Some(n) = self.nodes {
            config.nodes = n;
        }
        if let Some(d) = self.duration_ns {
            config.duration_ns = d;
        }
        config.validate()?;
        scenario_schedules(&self.scenario, &mut config, self.seed);
        Ok(config)
    }
}

fn scenario_schedules(scenario: &str, config: &mut Config, seed: u64) {
    let streams = Streams::new(seed);
    let d = config.duration_ns;
    match scenario {
        "storm" if config.storms.is_empty() => {
            config.storms.push(Storm {
                at_ns: d / 4,
                object: 0,
                nodes: config.nodes,
                window_ns: 10 * MILLIS,
            });
        }
        "churn" if config.churn.is_empty() => {
            config.churn = Churn::random(
                config.nodes,
                (config.nodes / 10).max(1),
                d,
                d / 4,
                &mut streams.stream(Role::Churn, 0),
            );
        }
        "faults" if config.faults.is_empty() => {
            let n = config.nodes;
            config.faults = vec![
                Fault::Partition {
                    from_ns: d / 4,
                    until_ns: d / 2,
                    site_a: 0,
                    site_b: 1,
                },
                Fault::Crash {
                    node: 0,
                    at_ns: d / 3,
                    restart_ns: 2 * d / 3,
                },
                Fault::Slow {
                    node: 1 % n,
                    factor: 8,
                    from_ns: 0,
                    until_ns: d,
                },
                Fault::OriginErrors {
                    from_ns: d / 2,
                    until_ns: 3 * d / 4,
                    errors: resources::OriginErrors {
                        p412: 0.05,
                        p403: 0.01,
                        p5xx: 0.2,
                    },
                },
            ];
        }
        "empty" => config.request_rate = 0.0,
        _ => {}
    }
}

/// Build the world for `options` without running it.
pub fn build(options: &Options) -> Result<World<StandIn>, String> {
    let config = options.build_config()?;
    let logic = StandIn::new(config.nodes, config.memory_pages, config.disk_pages);
    Ok(World::new(config, options.seed, logic))
}

/// Run a scenario and return its JSON report.
pub fn run(options: &Options) -> Result<Value, String> {
    let mut world = build(options)?;
    let end = world.config.duration_ns;
    world.run(end);
    Ok(world.report(&options.scenario, options.seed))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| (*s).to_owned()).collect()
    }

    fn opts(scenario: &str, nodes: u32, seed: u64) -> Options {
        Options {
            scenario: scenario.into(),
            nodes: Some(nodes),
            seed,
            duration_ns: Some(SECONDS),
            ..Options::default()
        }
    }

    #[test]
    fn parses_all_flags() {
        let o = Options::parse(&args(&[
            "--scenario",
            "zipf",
            "--nodes=50000",
            "--seed",
            "7",
            "--duration",
            "250ms",
            "--config=c.json",
            "--out",
            "r.json",
        ]))
        .unwrap();
        assert_eq!(
            o,
            Options {
                scenario: "zipf".into(),
                nodes: Some(50_000),
                seed: 7,
                duration_ns: Some(250 * MILLIS),
                config: Some("c.json".into()),
                out: Some("r.json".into())
            }
        );
    }

    #[test]
    fn defaults_when_empty() {
        assert_eq!(Options::parse(&[]).unwrap(), Options::default());
    }

    #[test]
    fn rejects_bad_input() {
        for bad in [
            &["--nodes"][..],
            &["--nodes", "x"],
            &["--nodes", "0"],
            &["--seed", "-1"],
            &["--bogus"],
            &["--scenario", "nope"],
            &["--duration", "5h"],
            &["--duration", "x"],
        ] {
            assert!(Options::parse(&args(bad)).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn durations() {
        assert_eq!(parse_duration("2").unwrap(), 2 * SECONDS);
        assert_eq!(parse_duration("7ns").unwrap(), 7);
        assert_eq!(parse_duration("3us").unwrap(), 3000);
        assert!(parse_duration("99999999999999999999s").is_err());
        assert!(parse_duration("").is_err());
    }

    #[test]
    fn config_file_and_flag_precedence() {
        let mut o = Options::default();
        let json = r#"{"nodes": 5, "sites": 2}"#;
        let c = o.build_config_with(Some(json)).unwrap();
        assert_eq!((c.nodes, c.sites), (5, 2));
        o.nodes = Some(9);
        assert_eq!(o.build_config_with(Some(json)).unwrap().nodes, 9);
        assert!(o.build_config_with(Some("{")).is_err());
        assert!(o.build_config_with(Some(r#"{"nodes": "x"}"#)).is_err());
        o.config = Some("/nonexistent/racer-sim/config.json".into());
        assert!(o.build_config().is_err());
    }

    #[test]
    fn replay_is_deterministic() {
        for scenario in SCENARIOS {
            let o = opts(scenario, 40, 3);
            let (a, b) = (run(&o).unwrap(), run(&o).unwrap());
            assert_eq!(a["trace_hash"], b["trace_hash"], "{scenario}");
            assert_eq!(a["metrics"], b["metrics"], "{scenario}");
        }
        let a = run(&opts("smoke", 40, 3)).unwrap();
        let b = run(&opts("smoke", 40, 4)).unwrap();
        assert_ne!(a["trace_hash"], b["trace_hash"]);
    }

    /// CI smoke: ~200 nodes, every scenario, no hard invariant failures.
    #[test]
    fn smoke_scenarios_hold_invariants() {
        for scenario in SCENARIOS {
            let r = run(&opts(scenario, 200, 1)).unwrap();
            assert_eq!(
                r["invariants"]["hard_failures"], 0,
                "{scenario}: {}",
                r["invariants"]
            );
            let counters = &r["metrics"]["counters"];
            if *scenario == "empty" {
                assert_eq!(r["events"], 0);
                continue;
            }
            assert!(
                counters["reads_completed"].as_u64().unwrap() > 0,
                "{scenario}"
            );
            assert!(
                counters["origin_requests"].as_u64().unwrap() > 0,
                "{scenario}"
            );
            assert!(r["memory"]["accounted_bytes"].as_u64().unwrap() > 0);
        }
    }

    #[test]
    fn scenarios_exercise_their_features() {
        let storm = run(&opts("storm", 200, 1)).unwrap();
        assert!(
            storm["metrics"]["counters"]["flight_joins"]
                .as_u64()
                .unwrap()
                > 0
        );
        // Singleflight: without errors each page hits the origin once per owner.
        assert_eq!(storm["origin_per_page"]["max_requests_per_page"], 1);

        let churn = run(&opts("churn", 200, 1)).unwrap();
        assert!(
            churn["metrics"]["counters"]["membership_changes"]
                .as_u64()
                .unwrap()
                > 0
        );

        let faults = run(&opts("faults", 200, 1)).unwrap();
        let c = &faults["metrics"]["counters"];
        assert_eq!(c["crashes"], 1);
        assert_eq!(c["restarts"], 1);
        assert!(c["origin_5xx"].as_u64().unwrap() > 0, "{c}");

        let smoke = run(&opts("smoke", 200, 1)).unwrap();
        let c = &smoke["metrics"]["counters"];
        assert!(c["range_reads"].as_u64().unwrap() > 0);
        assert!(c["peer_bytes"].as_u64().unwrap() > 0);
        assert!(
            smoke["metrics"]["histograms"]["read_ns"]["count"]
                .as_u64()
                .unwrap()
                > 0
        );
    }

    /// 50k nodes x 2 workers, substrate only, must fit far below 32 GiB.
    /// Run with `cargo test --release -p racer-sim -- --ignored`.
    #[test]
    #[ignore]
    fn empty_substrate_50k_rss() {
        let o = Options {
            scenario: "empty".into(),
            nodes: Some(50_000),
            ..Options::default()
        };
        let r = run(&o).unwrap();
        let rss = r["memory"]["peak_rss_bytes"].as_u64().unwrap();
        let per_node = r["memory"]["accounted_bytes_per_node"].as_u64().unwrap();
        eprintln!("50k empty substrate: peak RSS {rss} bytes, accounted {per_node} B/node");
        assert!(rss < 32 << 30);
        assert!(per_node < 640 << 10);
    }
}
