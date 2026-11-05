//! The cost table: how long every modeled operation takes.
//!
//! Values live in the checked-in `costs.toml` (crate root), so Full and Model
//! fidelity price the same operation the same way and recalibration is a data
//! change. The file documents the source of every constant. The format is a
//! strict TOML subset parsed here by hand (no TOML crate is in the lockfile):
//! `[section]` headers, `key = <unsigned integer>` lines with optional `_`
//! separators, and `#` comments. Unknown, duplicate or missing keys are errors.
use crate::engine::{Nanos, SECONDS};

/// The checked-in table.
pub const COSTS_TOML: &str = include_str!("../costs.toml");

/// Parsed cost table. Field names are `<section>_<key>` from `costs.toml`;
/// see that file for units and sources.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
#[allow(missing_docs)]
pub struct Costs {
    pub crypto_seal_bytes_per_sec: u64,
    pub crypto_open_bytes_per_sec: u64,
    pub crypto_tag_check_bytes_per_sec: u64,
    pub crypto_memcpy_bytes_per_sec: u64,
    pub crypto_sign_ns: Nanos,
    pub crypto_sign_hop_ns: Nanos,
    pub crypto_sign_response_ns: Nanos,
    pub crypto_verify_ns: Nanos,
    pub crypto_verify_hop_ns: Nanos,
    pub cpu_workers: u64,
    pub cpu_request_ns: Nanos,
    pub cpu_ranking_chunk_ns: Nanos,
    pub cpu_path_expand_ns_per_node: Nanos,
    pub cpu_control_handshake_ns: Nanos,
    pub nic_bytes_per_sec: u64,
    pub nic_chunk_bytes: u64,
    pub disk_read_bytes_per_sec: u64,
    pub disk_write_bytes_per_sec: u64,
    pub disk_latency_ns: Nanos,
    pub origin_caches: u64,
    pub origin_concurrency: u64,
    pub origin_bytes_per_sec: u64,
    pub origin_latency_ns: Nanos,
    pub origin_retry_ns: Nanos,
    pub rtt_local_ns: Nanos,
    pub rtt_remote_ns: Nanos,
    pub rtt_jitter_ns: Nanos,
}

impl Costs {
    /// Signing a peer request carrying `proofs` proofs (1 for the requester's own head),
    /// racer-sim `Costs::sign_request`.
    pub fn sign_request(&self, proofs: usize) -> Nanos {
        self.crypto_sign_ns + self.crypto_sign_hop_ns * proofs.saturating_sub(1) as Nanos
    }

    /// Verifying a peer head carrying `proofs` proofs, racer-sim `Costs::verify`.
    pub fn verify(&self, proofs: usize) -> Nanos {
        self.crypto_verify_ns + self.crypto_verify_hop_ns * proofs.saturating_sub(1) as Nanos
    }

    /// Every `(section, key)` the table must contain.
    pub const KEYS: &[(&str, &str)] = &[
        ("crypto", "seal_bytes_per_sec"),
        ("crypto", "open_bytes_per_sec"),
        ("crypto", "tag_check_bytes_per_sec"),
        ("crypto", "memcpy_bytes_per_sec"),
        ("crypto", "sign_ns"),
        ("crypto", "sign_hop_ns"),
        ("crypto", "sign_response_ns"),
        ("crypto", "verify_ns"),
        ("crypto", "verify_hop_ns"),
        ("cpu", "workers"),
        ("cpu", "request_ns"),
        ("cpu", "ranking_chunk_ns"),
        ("cpu", "path_expand_ns_per_node"),
        ("cpu", "control_handshake_ns"),
        ("nic", "bytes_per_sec"),
        ("nic", "chunk_bytes"),
        ("disk", "read_bytes_per_sec"),
        ("disk", "write_bytes_per_sec"),
        ("disk", "latency_ns"),
        ("origin", "caches"),
        ("origin", "concurrency"),
        ("origin", "bytes_per_sec"),
        ("origin", "latency_ns"),
        ("origin", "retry_ns"),
        ("rtt", "local_ns"),
        ("rtt", "remote_ns"),
        ("rtt", "jitter_ns"),
    ];

    fn slot(&mut self, section: &str, key: &str) -> Option<&mut u64> {
        Some(match (section, key) {
            ("crypto", "seal_bytes_per_sec") => &mut self.crypto_seal_bytes_per_sec,
            ("crypto", "open_bytes_per_sec") => &mut self.crypto_open_bytes_per_sec,
            ("crypto", "tag_check_bytes_per_sec") => &mut self.crypto_tag_check_bytes_per_sec,
            ("crypto", "memcpy_bytes_per_sec") => &mut self.crypto_memcpy_bytes_per_sec,
            ("crypto", "sign_ns") => &mut self.crypto_sign_ns,
            ("crypto", "sign_hop_ns") => &mut self.crypto_sign_hop_ns,
            ("crypto", "sign_response_ns") => &mut self.crypto_sign_response_ns,
            ("crypto", "verify_ns") => &mut self.crypto_verify_ns,
            ("crypto", "verify_hop_ns") => &mut self.crypto_verify_hop_ns,
            ("cpu", "workers") => &mut self.cpu_workers,
            ("cpu", "request_ns") => &mut self.cpu_request_ns,
            ("cpu", "ranking_chunk_ns") => &mut self.cpu_ranking_chunk_ns,
            ("cpu", "path_expand_ns_per_node") => &mut self.cpu_path_expand_ns_per_node,
            ("cpu", "control_handshake_ns") => &mut self.cpu_control_handshake_ns,
            ("nic", "bytes_per_sec") => &mut self.nic_bytes_per_sec,
            ("nic", "chunk_bytes") => &mut self.nic_chunk_bytes,
            ("disk", "read_bytes_per_sec") => &mut self.disk_read_bytes_per_sec,
            ("disk", "write_bytes_per_sec") => &mut self.disk_write_bytes_per_sec,
            ("disk", "latency_ns") => &mut self.disk_latency_ns,
            ("origin", "caches") => &mut self.origin_caches,
            ("origin", "concurrency") => &mut self.origin_concurrency,
            ("origin", "bytes_per_sec") => &mut self.origin_bytes_per_sec,
            ("origin", "latency_ns") => &mut self.origin_latency_ns,
            ("origin", "retry_ns") => &mut self.origin_retry_ns,
            ("rtt", "local_ns") => &mut self.rtt_local_ns,
            ("rtt", "remote_ns") => &mut self.rtt_remote_ns,
            ("rtt", "jitter_ns") => &mut self.rtt_jitter_ns,
            _ => return None,
        })
    }

    /// Parse a cost table. Errors name the 1-based line.
    pub fn parse(text: &str) -> Result<Self, String> {
        let mut costs = Self::default();
        let mut seen = vec![false; Self::KEYS.len()];
        let mut section = String::new();
        for (n, raw) in text.lines().enumerate() {
            let line = raw.split('#').next().unwrap_or("").trim();
            let err = |m: &str| format!("costs line {}: {m}: {raw:?}", n + 1);
            if line.is_empty() {
                continue;
            }
            if let Some(name) = line.strip_prefix('[') {
                section = name
                    .strip_suffix(']')
                    .ok_or_else(|| err("unterminated section"))?
                    .trim()
                    .to_owned();
                continue;
            }
            let (key, value) = line
                .split_once('=')
                .ok_or_else(|| err("expected key = value"))?;
            let key = key.trim();
            let value = value.trim().replace('_', "");
            let value: u64 = value
                .parse()
                .map_err(|_| err("expected unsigned integer"))?;
            let index = Self::KEYS
                .iter()
                .position(|k| *k == (section.as_str(), key))
                .ok_or_else(|| err("unknown key"))?;
            if std::mem::replace(&mut seen[index], true) {
                return Err(err("duplicate key"));
            }
            *costs.slot(&section, key).expect("KEYS and slot agree") = value;
        }
        if let Some(i) = seen.iter().position(|s| !s) {
            let (s, k) = Self::KEYS[i];
            return Err(format!("costs: missing [{s}] {k}"));
        }
        Ok(costs)
    }

    /// The checked-in table ([`COSTS_TOML`]).
    pub fn checked_in() -> Self {
        Self::parse(COSTS_TOML).expect("checked-in costs.toml parses")
    }

    /// AEAD seal time for `bytes` on one core.
    pub fn seal(&self, bytes: u64) -> Nanos {
        service_ns(bytes, self.crypto_seal_bytes_per_sec)
    }

    /// Simulated OS operation latency for `kind` (a racer-io operation name,
    /// see `racer_io::sim::OpDesc`) moving `bytes`. Disk operations pay the
    /// disk latency plus transfer time; sends pay NIC serialization; waits
    /// for a peer (receive, accept, poll) and closes are free here because
    /// their time comes from the peer.
    pub fn op(&self, kind: &str, bytes: u64) -> Nanos {
        let disk = self.disk_latency_ns;
        match kind {
            "read" | "readv" => disk + service_ns(bytes, self.disk_read_bytes_per_sec),
            "write" | "writev" => disk + service_ns(bytes, self.disk_write_bytes_per_sec),
            "fsync" | "open" | "stat" | "mkdir" | "rename" | "unlink" => disk,
            "send" | "sendmsg" | "splice" => service_ns(bytes, self.nic_bytes_per_sec),
            _ => 0,
        }
    }

    /// AEAD open time for `bytes` on one core.
    pub fn open(&self, bytes: u64) -> Nanos {
        service_ns(bytes, self.crypto_open_bytes_per_sec)
    }
}

/// Service time of `bytes` at `bytes_per_sec`, rounded up, at least 1 ns for
/// a nonzero transfer.
pub fn service_ns(bytes: u64, bytes_per_sec: u64) -> Nanos {
    if bytes == 0 {
        return 0;
    }
    let bps = u128::from(bytes_per_sec.max(1));
    (u128::from(bytes) * u128::from(SECONDS))
        .div_ceil(bps)
        .max(1)
        .min(u128::from(Nanos::MAX)) as Nanos
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn checked_in_table_parses_with_every_key() {
        let costs = Costs::checked_in();
        let mut probe = costs;
        for (s, k) in Costs::KEYS {
            assert!(*probe.slot(s, k).unwrap() > 0, "[{s}] {k} is zero");
        }
        // 16 MiB at the measured ~3.57 GiB/s is about 4.4 ms.
        let page = costs.seal(16 << 20);
        assert!((4_300_000..4_500_000).contains(&page), "{page}");
        assert!(costs.open(16 << 20) <= page);
        assert_eq!(costs.cpu_workers, 2);
    }

    #[test]
    fn peer_crypto_costs_match_racer_sim() {
        let costs = Costs::checked_in();
        assert_eq!(costs.sign_request(1), 64_000);
        assert_eq!(costs.sign_request(2), 76_000);
        assert_eq!(costs.verify(1), 73_000);
        assert_eq!(costs.verify(2), 160_000);
        assert_eq!(costs.crypto_sign_response_ns, 114_000);
    }

    #[test]
    fn parser_rejects_bad_tables() {
        let good = COSTS_TOML;
        let cases = [
            (good.replace("[nic]", "[nic"), "unterminated"),
            (good.replace("workers = 2", "workers = two"), "unsigned"),
            (good.replace("workers = 2", "workers 2"), "key = value"),
            (good.replace("workers = 2", "worker = 2"), "unknown"),
            (
                good.replace("workers = 2", "workers = 2\nworkers = 3"),
                "duplicate",
            ),
            (good.replace("workers = 2", ""), "missing [cpu] workers"),
        ];
        for (text, needle) in cases {
            let err = Costs::parse(&text).unwrap_err();
            assert!(err.contains(needle), "{err}");
        }
    }

    #[test]
    fn op_costs_follow_the_table() {
        let c = Costs::checked_in();
        assert_eq!(c.op("fsync", 0), c.disk_latency_ns);
        assert_eq!(
            c.op("write", 1 << 20),
            c.disk_latency_ns + service_ns(1 << 20, c.disk_write_bytes_per_sec)
        );
        assert_eq!(
            c.op("readv", 4096),
            c.disk_latency_ns + service_ns(4096, c.disk_read_bytes_per_sec)
        );
        assert_eq!(c.op("send", 1000), service_ns(1000, c.nic_bytes_per_sec));
        for free in ["recv", "accept", "poll", "close", "cancel"] {
            assert_eq!(c.op(free, 1 << 20), 0, "{free}");
        }
    }

    #[test]
    fn service_time_rounds_up() {
        assert_eq!(service_ns(0, 10), 0);
        assert_eq!(service_ns(1, SECONDS), 1);
        assert_eq!(service_ns(1, 3 * SECONDS), 1);
        assert_eq!(service_ns(1 << 20, 1 << 20), SECONDS);
        assert_eq!(service_ns(u64::MAX, 0), Nanos::MAX);
    }
}
