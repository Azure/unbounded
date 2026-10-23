//! Run report: histograms, counters, memory accounting, invariants.

use std::collections::BTreeMap;

use serde_json::{Map, Value, json};

use crate::engine::Nanos;

/// Log-bucket histogram with fixed memory: 64 power-of-two octaves, each
/// split into `SUB` linear sub-buckets (relative error under 1/SUB).
#[derive(Clone, Debug)]
pub struct Histogram {
    buckets: Box<[u64; Self::LEN]>,
    count: u64,
    sum: u128,
    min: u64,
    max: u64,
}

impl Default for Histogram {
    fn default() -> Self {
        Self {
            buckets: Box::new([0; Self::LEN]),
            count: 0,
            sum: 0,
            min: u64::MAX,
            max: 0,
        }
    }
}

const SUB_BITS: u32 = 3;
const SUB: u64 = 1 << SUB_BITS;

impl Histogram {
    const LEN: usize = (64 * SUB) as usize;

    fn index(v: u64) -> usize {
        if v < SUB {
            return v as usize;
        }
        let octave = 63 - v.leading_zeros(); // >= SUB_BITS
        let sub = (v >> (octave - SUB_BITS)) & (SUB - 1);
        ((octave - SUB_BITS + 1) as u64 * SUB + sub) as usize
    }

    /// Lower bound of bucket `i`.
    fn lower(i: usize) -> u64 {
        let i = i as u64;
        if i < SUB {
            return i;
        }
        let octave = i / SUB - 1 + u64::from(SUB_BITS);
        if octave >= 64 {
            return u64::MAX;
        }
        (1 << octave) | ((i % SUB) << (octave - u64::from(SUB_BITS)))
    }

    pub fn record(&mut self, v: u64) {
        self.buckets[Self::index(v)] += 1;
        self.count += 1;
        self.sum += u128::from(v);
        self.min = self.min.min(v);
        self.max = self.max.max(v);
    }

    pub fn count(&self) -> u64 {
        self.count
    }

    /// Approximate quantile (bucket lower bound, clamped to [min, max]).
    pub fn quantile(&self, q: f64) -> u64 {
        if self.count == 0 {
            return 0;
        }
        let rank = ((q.clamp(0.0, 1.0) * self.count as f64).ceil() as u64).max(1);
        let mut seen = 0;
        for (i, &c) in self.buckets.iter().enumerate() {
            seen += c;
            if seen >= rank {
                return Self::lower(i).clamp(self.min, self.max);
            }
        }
        self.max
    }

    pub fn to_json(&self) -> Value {
        if self.count == 0 {
            return json!({"count": 0});
        }
        json!({
            "count": self.count,
            "min": self.min,
            "mean": (self.sum / u128::from(self.count)) as u64,
            "p50": self.quantile(0.5),
            "p90": self.quantile(0.9),
            "p99": self.quantile(0.99),
            "p999": self.quantile(0.999),
            "max": self.max,
        })
    }
}

/// Approximate heap and inline bytes owned by a simulated component.
pub trait MemoryUsage {
    fn approximate_bytes(&self) -> usize;
}

/// Peak resident set size of this process in bytes (VmHWM), if available.
pub fn peak_rss_bytes() -> Option<u64> {
    let status = std::fs::read_to_string("/proc/self/status").ok()?;
    let line = status.lines().find(|l| l.starts_with("VmHWM:"))?;
    let kib: u64 = line.split_whitespace().nth(1)?.parse().ok()?;
    Some(kib * 1024)
}

/// One invariant violation.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Violation {
    pub name: &'static str,
    pub time: Nanos,
    pub node: Option<u32>,
    pub detail: String,
}

/// Named invariants. Each check counts; failures are stored (capped) with
/// time, node and detail. An invariant may be marked relaxed with a reason
/// (a known issue): its failures are counted separately and do not fail the
/// run.
#[derive(Debug, Default)]
pub struct Invariants {
    checks: BTreeMap<&'static str, u64>,
    failures: BTreeMap<&'static str, u64>,
    relaxed: BTreeMap<&'static str, &'static str>,
    stored: Vec<Violation>,
}

impl Invariants {
    pub const MAX_STORED: usize = 100;

    pub fn relax(&mut self, name: &'static str, reason: &'static str) {
        self.relaxed.insert(name, reason);
    }

    /// Check `ok`; `detail` is only built on failure.
    pub fn check(
        &mut self,
        name: &'static str,
        ok: bool,
        time: Nanos,
        node: Option<u32>,
        detail: impl FnOnce() -> String,
    ) -> bool {
        *self.checks.entry(name).or_default() += 1;
        if !ok {
            *self.failures.entry(name).or_default() += 1;
            if self.stored.len() < Self::MAX_STORED {
                self.stored.push(Violation {
                    name,
                    time,
                    node,
                    detail: detail(),
                });
            }
        }
        ok
    }

    /// Failures of invariants that are not relaxed.
    pub fn hard_failures(&self) -> u64 {
        self.failures
            .iter()
            .filter(|(n, _)| !self.relaxed.contains_key(*n))
            .map(|(_, c)| c)
            .sum()
    }

    pub fn violations(&self) -> &[Violation] {
        &self.stored
    }

    pub fn to_json(&self) -> Value {
        let names: Vec<Value> = self
            .checks
            .iter()
            .map(|(name, checks)| {
                let mut o = Map::new();
                o.insert("name".into(), json!(name));
                o.insert("checks".into(), json!(checks));
                o.insert(
                    "failures".into(),
                    json!(self.failures.get(name).copied().unwrap_or(0)),
                );
                if let Some(reason) = self.relaxed.get(name) {
                    o.insert("relaxed".into(), json!(reason));
                }
                Value::Object(o)
            })
            .collect();
        let violations: Vec<Value> = self
            .stored
            .iter()
            .map(|v| json!({"name": v.name, "time_ns": v.time, "node": v.node, "detail": v.detail}))
            .collect();
        json!({
            "hard_failures": self.hard_failures(),
            "invariants": names,
            "violations": violations,
        })
    }
}

/// Counters and histograms collected during a run.
#[derive(Debug, Default)]
pub struct Metrics {
    pub counters: BTreeMap<&'static str, u64>,
    pub histograms: BTreeMap<&'static str, Histogram>,
}

impl Metrics {
    pub fn add(&mut self, name: &'static str, n: u64) {
        *self.counters.entry(name).or_default() += n;
    }

    pub fn incr(&mut self, name: &'static str) {
        self.add(name, 1);
    }

    pub fn get(&self, name: &str) -> u64 {
        self.counters.get(name).copied().unwrap_or(0)
    }

    pub fn observe(&mut self, name: &'static str, v: u64) {
        self.histograms.entry(name).or_default().record(v);
    }

    pub fn histogram(&self, name: &str) -> Option<&Histogram> {
        self.histograms.get(name)
    }

    pub fn to_json(&self) -> Value {
        json!({
            "counters": self.counters,
            "histograms": self
                .histograms
                .iter()
                .map(|(k, h)| (k.to_string(), h.to_json()))
                .collect::<Map<String, Value>>(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bucket_bounds_contain_values() {
        for v in (0..10_000u64).chain([u64::MAX, 1 << 40, (1 << 40) + 12345]) {
            let i = Histogram::index(v);
            assert!(i < Histogram::LEN, "{v}");
            assert!(Histogram::lower(i) <= v, "{v}");
            if i + 1 < Histogram::LEN && v != u64::MAX {
                assert!(Histogram::lower(i + 1) > v, "{v}");
            }
        }
    }

    #[test]
    fn quantiles_within_relative_error() {
        let mut h = Histogram::default();
        assert_eq!(h.quantile(0.5), 0);
        assert_eq!(h.to_json()["count"], 0);
        for v in 1..=1000u64 {
            h.record(v * 1000);
        }
        let p50 = h.quantile(0.5) as f64;
        assert!(
            (p50 - 500_000.0).abs() / 500_000.0 < 1.0 / SUB as f64,
            "{p50}"
        );
        assert!(h.quantile(1.0) <= 1_000_000);
        assert_eq!(h.quantile(0.0), 1000);
        assert_eq!(h.count(), 1000);
        assert_eq!(h.to_json()["max"], 1_000_000);
    }

    #[test]
    fn invariants_count_and_relax() {
        let mut inv = Invariants::default();
        assert!(inv.check("a", true, 1, None, || unreachable!()));
        assert!(!inv.check("a", false, 2, Some(3), || "bad".into()));
        assert!(!inv.check("b", false, 3, None, || "known".into()));
        inv.relax("b", "see Known issues");
        assert_eq!(inv.hard_failures(), 1);
        assert_eq!(inv.violations().len(), 2);
        assert_eq!(inv.violations()[0].node, Some(3));
        let j = inv.to_json();
        assert_eq!(j["hard_failures"], 1);
        assert_eq!(j["invariants"][1]["relaxed"], "see Known issues");
        for _ in 0..200 {
            inv.check("c", false, 0, None, String::new);
        }
        assert_eq!(inv.violations().len(), Invariants::MAX_STORED);
        assert_eq!(inv.hard_failures(), 201);
    }

    #[test]
    fn metrics_and_rss() {
        let mut m = Metrics::default();
        m.incr("x");
        m.add("x", 2);
        m.observe("lat", 5);
        assert_eq!(m.get("x"), 3);
        assert_eq!(m.get("missing"), 0);
        assert_eq!(m.to_json()["histograms"]["lat"]["count"], 1);
        if cfg!(target_os = "linux") {
            assert!(peak_rss_bytes().unwrap() > 0);
        }
    }
}
