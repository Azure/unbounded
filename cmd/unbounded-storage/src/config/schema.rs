// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Serde schema for the daemon's TOML configuration.

use std::path::PathBuf;

use serde::{Deserialize, Deserializer};

#[derive(Clone, Debug, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Config {
    pub fabric: FabricCfg,
    pub storage: StorageCfg,
    pub topology: TopologyCfg,
    pub peers: Vec<PeerSpec>,
    pub disks: Vec<DiskSpec>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            fabric: FabricCfg::default(),
            storage: StorageCfg::default(),
            topology: TopologyCfg::default(),
            peers: Vec::new(),
            disks: Vec::new(),
        }
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct FabricCfg {
    pub listen_addr: String,
    pub max_inflight: usize,
    pub progress_threads: u8,
    pub progress_poll_us: u32,
}

impl Default for FabricCfg {
    fn default() -> Self {
        Self {
            listen_addr: "0.0.0.0:0".to_string(),
            max_inflight: 1024,
            progress_threads: 2,
            progress_poll_us: 10,
        }
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct StorageCfg {
    pub bytes_per_shard: ByteSize,
    pub backing_kind: BackingKindCfg,
}

impl Default for StorageCfg {
    fn default() -> Self {
        Self {
            bytes_per_shard: ByteSize(128 * 1024 * 1024),
            backing_kind: BackingKindCfg::default(),
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum BackingKindCfg {
    Hugepage2Mb,
    Heap,
}

impl Default for BackingKindCfg {
    fn default() -> Self {
        BackingKindCfg::Hugepage2Mb
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct TopologyCfg {
    pub rdma_progress_per_hca: usize,
    pub rdma_handlers_per_hca: usize,
    pub use_smt_siblings: bool,
    pub respect_isolated: bool,
    pub exclude_node_cpu0: bool,
    pub require_active_port: bool,
    pub tcp_fallback_threads: usize,
}

impl Default for TopologyCfg {
    fn default() -> Self {
        Self {
            rdma_progress_per_hca: 1,
            rdma_handlers_per_hca: 4,
            use_smt_siblings: false,
            respect_isolated: true,
            exclude_node_cpu0: true,
            require_active_port: true,
            tcp_fallback_threads: 1,
        }
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerSpec {
    pub id: u64,
    pub transport: PeerTransport,
    pub address: String,
    #[serde(default)]
    pub hca_numa: Option<u16>,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum PeerTransport {
    Tcp,
    Rdma,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DiskSpec {
    pub path: PathBuf,
    #[serde(default)]
    pub kind: DiskKind,
    #[serde(default)]
    pub numa: Option<u16>,
    #[serde(default)]
    pub queue_depth: Option<u32>,
    /// Engine page size in bytes. When unset, the disk supervisor
    /// applies its default (4 KiB) for both the storage engine and
    /// the underlying io_uring device.
    #[serde(default)]
    pub page_size_bytes: Option<usize>,
    /// Skip the engine's admission filter on the write path. Useful
    /// for benchmarking and for tests that want every write to
    /// commit unconditionally; should be left `false` in production.
    #[serde(default)]
    pub bypass_admission: bool,
    /// Skip the recovery scan when the disk has no engine metadata.
    /// Set on fresh disks at bring-up to avoid the no-op scan; safe
    /// to leave `false` otherwise.
    #[serde(default)]
    pub skip_recovery_scan_if_no_meta: bool,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum DiskKind {
    Nvme,
    Block,
}

impl Default for DiskKind {
    fn default() -> Self {
        DiskKind::Nvme
    }
}

/// A byte count accepted as either an integer (bytes) or a string with
/// an optional `K`/`M`/`G` suffix interpreted as powers of 1024.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ByteSize(pub usize);

impl ByteSize {
    pub fn bytes(self) -> usize {
        self.0
    }
}

impl<'de> Deserialize<'de> for ByteSize {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        use serde::de::{self, Visitor};
        use std::fmt;

        struct V;

        impl<'de> Visitor<'de> for V {
            type Value = ByteSize;

            fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
                f.write_str("a byte count as integer or string with K/M/G suffix")
            }

            fn visit_u64<E: de::Error>(self, v: u64) -> Result<ByteSize, E> {
                usize::try_from(v)
                    .map(ByteSize)
                    .map_err(|_| de::Error::custom("byte size overflows usize"))
            }

            fn visit_i64<E: de::Error>(self, v: i64) -> Result<ByteSize, E> {
                if v < 0 {
                    return Err(de::Error::custom("byte size must be non-negative"));
                }
                self.visit_u64(v as u64)
            }

            fn visit_str<E: de::Error>(self, s: &str) -> Result<ByteSize, E> {
                parse_byte_str(s).map(ByteSize).map_err(de::Error::custom)
            }
        }

        deserializer.deserialize_any(V)
    }
}

fn parse_byte_str(s: &str) -> Result<usize, String> {
    let s = s.trim();
    if s.is_empty() {
        return Err("empty byte size".to_string());
    }
    let bytes = s.as_bytes();
    let (num_part, mult) = match bytes[bytes.len() - 1] {
        b'K' | b'k' => (&s[..s.len() - 1], 1024usize),
        b'M' | b'm' => (&s[..s.len() - 1], 1024 * 1024),
        b'G' | b'g' => (&s[..s.len() - 1], 1024 * 1024 * 1024),
        b'0'..=b'9' => (s, 1usize),
        c => return Err(format!("invalid byte size suffix: {}", c as char)),
    };
    let n: usize = num_part
        .trim()
        .parse()
        .map_err(|e| format!("invalid byte size number {num_part:?}: {e}"))?;
    n.checked_mul(mult)
        .ok_or_else(|| "byte size overflows usize".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_are_populated() {
        let c: Config = toml::from_str("").unwrap();
        assert_eq!(c.fabric.listen_addr, "0.0.0.0:0");
        assert_eq!(c.fabric.max_inflight, 1024);
        assert_eq!(c.fabric.progress_threads, 2);
        assert_eq!(c.fabric.progress_poll_us, 10);
        assert_eq!(c.storage.bytes_per_shard.bytes(), 128 * 1024 * 1024);
        assert_eq!(c.storage.backing_kind, BackingKindCfg::Hugepage2Mb);
        assert_eq!(c.topology.rdma_progress_per_hca, 1);
        assert_eq!(c.topology.rdma_handlers_per_hca, 4);
        assert!(!c.topology.use_smt_siblings);
        assert!(c.topology.respect_isolated);
        assert!(c.topology.exclude_node_cpu0);
        assert!(c.topology.require_active_port);
        assert_eq!(c.topology.tcp_fallback_threads, 1);
        assert!(c.peers.is_empty());
        assert!(c.disks.is_empty());
    }

    #[test]
    fn bytesize_parses_integer_and_suffixes() {
        #[derive(Deserialize)]
        struct W {
            x: ByteSize,
        }
        assert_eq!(toml::from_str::<W>("x = 4096").unwrap().x.bytes(), 4096);
        assert_eq!(
            toml::from_str::<W>("x = \"128M\"").unwrap().x.bytes(),
            134_217_728
        );
        assert_eq!(toml::from_str::<W>("x = \"4K\"").unwrap().x.bytes(), 4096);
        assert_eq!(
            toml::from_str::<W>("x = \"2G\"").unwrap().x.bytes(),
            2 * 1024 * 1024 * 1024
        );
        assert_eq!(toml::from_str::<W>("x = \"1024\"").unwrap().x.bytes(), 1024);
        assert!(toml::from_str::<W>("x = \"bogus\"").is_err());
        assert!(toml::from_str::<W>("x = \"12X\"").is_err());
    }

    #[test]
    fn unknown_fields_rejected_top_level() {
        assert!(toml::from_str::<Config>("nonsense = 1\n").is_err());
    }

    #[test]
    fn unknown_fields_rejected_in_fabric() {
        let s = "[fabric]\nbogus = 1\n";
        assert!(toml::from_str::<Config>(s).is_err());
    }

    #[test]
    fn unknown_fields_rejected_in_peer() {
        let s = r#"
[[peers]]
id = 1
transport = "tcp"
address = "127.0.0.1:9000"
extra = "nope"
"#;
        assert!(toml::from_str::<Config>(s).is_err());
    }

    #[test]
    fn unknown_fields_rejected_in_disk() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"
extra = "nope"
"#;
        assert!(toml::from_str::<Config>(s).is_err());
    }

    #[test]
    fn enums_are_case_sensitive() {
        let ok = r#"
[storage]
backing_kind = "heap"
"#;
        let parsed: Config = toml::from_str(ok).unwrap();
        assert_eq!(parsed.storage.backing_kind, BackingKindCfg::Heap);

        let bad = r#"
[storage]
backing_kind = "Heap"
"#;
        assert!(toml::from_str::<Config>(bad).is_err());

        let bad2 = r#"
[[peers]]
id = 1
transport = "TCP"
address = "127.0.0.1:9000"
"#;
        assert!(toml::from_str::<Config>(bad2).is_err());
    }

    #[test]
    fn disk_kind_defaults_to_nvme() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(c.disks[0].kind, DiskKind::Nvme);
    }

    #[test]
    fn disk_engine_fields_default_to_unset_and_off() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(c.disks[0].page_size_bytes, None);
        assert!(!c.disks[0].bypass_admission);
        assert!(!c.disks[0].skip_recovery_scan_if_no_meta);
    }

    #[test]
    fn disk_engine_fields_round_trip() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"
page_size_bytes = 4096
bypass_admission = true
skip_recovery_scan_if_no_meta = true
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(c.disks[0].page_size_bytes, Some(4096));
        assert!(c.disks[0].bypass_admission);
        assert!(c.disks[0].skip_recovery_scan_if_no_meta);
    }
}
