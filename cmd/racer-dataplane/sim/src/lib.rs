//! Discrete-event simulator for Racer.
//!
//! Runs the real decision logic from `racer-core` for many nodes in one
//! process and one thread. See `designs/racer-core-simulation.md`.
//! This is a skeleton: it parses options and returns a report stub.

use serde_json::{Value, json};

pub const USAGE: &str = "usage: racer-sim [--scenario NAME] [--nodes N] [--seed S] [--out PATH]";

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Options {
    pub scenario: String,
    pub nodes: u32,
    pub seed: u64,
    pub out: Option<String>,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            scenario: "smoke".to_owned(),
            nodes: 16,
            seed: 1,
            out: None,
        }
    }
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
                    options.nodes = raw.parse().map_err(|_| format!("bad --nodes {raw:?}"))?;
                    if options.nodes == 0 {
                        return Err("--nodes must be at least 1".to_owned());
                    }
                }
                "--seed" => {
                    let raw = value()?;
                    options.seed = raw.parse().map_err(|_| format!("bad --seed {raw:?}"))?;
                }
                "--out" => options.out = Some(value()?),
                _ => return Err(format!("unknown argument {arg:?}")),
            }
        }
        Ok(options)
    }
}

/// Run a scenario and return its JSON report.
pub fn run(options: &Options) -> Value {
    let mut rng = racer_core::time::SplitMix64::new(options.seed);
    json!({
        "scenario": options.scenario,
        "nodes": options.nodes,
        "seed": options.seed,
        "events": 0,
        "trace_hash": format!("{:016x}", rng.next_u64()),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| (*s).to_owned()).collect()
    }

    #[test]
    fn parses_all_flags() {
        let o = Options::parse(&args(&[
            "--scenario",
            "zipf",
            "--nodes=50000",
            "--seed",
            "7",
            "--out",
            "r.json",
        ]))
        .unwrap();
        assert_eq!(
            o,
            Options {
                scenario: "zipf".into(),
                nodes: 50_000,
                seed: 7,
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
        ] {
            assert!(Options::parse(&args(bad)).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn report_is_deterministic() {
        let o = Options::default();
        assert_eq!(run(&o), run(&o));
        assert_ne!(
            run(&o)["trace_hash"],
            run(&Options {
                seed: 2,
                ..o.clone()
            })["trace_hash"]
        );
        assert_eq!(run(&o)["nodes"], 16);
    }
}
