//! Simulator configuration and JSON overrides.
//!
//! Every knob has a default suitable for a fast smoke run. `--config FILE`
//! takes a JSON object whose keys are field names of [`Config`] (nested
//! objects for `costs` and `origin_errors`, arrays for `faults` and `churn`).
//! Unknown keys are errors so typos do not silently fall back to defaults.

use serde_json::Value;

use crate::engine::{MICROS, MILLIS, Nanos, SECONDS};
use crate::faults::Fault;
use crate::node::NodeConfig;
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
    /// Stand-in memory tier capacity, pages per node.
    pub memory_pages: u32,
    /// Stand-in disk tier capacity, pages per node.
    pub disk_pages: u32,
    /// Hop budget for peer requests (links).
    pub hop_limit: u8,
    pub storms: Vec<Storm>,
    pub churn: Vec<Churn>,
    pub faults: Vec<Fault>,
    /// Real node logic knobs (`"node"` object in JSON).
    pub node: NodeConfig,
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
        }
    }
}

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
                "node" => self.node.apply_json(v)?,
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
        self.node.validate()
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
}
