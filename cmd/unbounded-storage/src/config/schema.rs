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
    pub p2p: P2pCfg,
    pub peers: Vec<PeerSpec>,
    pub disks: Vec<DiskSpec>,
    pub backends: Vec<BackendSpec>,
    pub frontends: Vec<FrontendSpec>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            fabric: FabricCfg::default(),
            storage: StorageCfg::default(),
            topology: TopologyCfg::default(),
            p2p: P2pCfg::default(),
            peers: Vec::new(),
            disks: Vec::new(),
            backends: Vec::new(),
            frontends: Vec::new(),
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
    /// Force the libfabric `tcp` provider even when an HCA is present.
    /// Drops all discovered HCAs during planning so the daemon takes
    /// the TCP fallback path. Useful on hosts whose verbs provider is
    /// unusable despite RDMA hardware being visible in sysfs.
    pub disable_rdma: bool,
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
            disable_rdma: false,
        }
    }
}

/// Peer-to-peer / DHT knobs.
///
/// `fingers_per_node` and `local_node_id` are validated at load time
/// (see `config::load::validate`) but are not yet consumed by a
/// runtime FingerTable: that wiring lands with the stripe-DHT
/// subsystem in a later phase. `local_labels` is similarly validated
/// for shape (present, possibly empty) but not yet consumed.
#[derive(Clone, Debug, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct P2pCfg {
    pub fingers_per_node: u32,
    /// Stable identifier for this daemon in the p2p ring. `None`
    /// means "unset"; load-time validation rejects unset ids when
    /// peers are configured, since the silent NodeId(0) collision
    /// would otherwise lurk until the DHT runtime tried to use it.
    pub local_node_id: Option<u64>,
    pub local_labels: Vec<String>,
}

impl Default for P2pCfg {
    fn default() -> Self {
        Self {
            fingers_per_node: 100,
            local_node_id: None,
            local_labels: Vec::new(),
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
    /// Topology labels for this peer. Propagated into
    /// `ConnectionSpec.labels`; consumed by the p2p FingerTable's
    /// topology-distance heuristic when peers are added to the local
    /// routing table.
    #[serde(default)]
    pub labels: Vec<String>,
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
    /// Size of the backing file for `kind = "file"` disks. The file is
    /// created (or grown/shrunk) to exactly this size on open. Ignored
    /// for `nvme`/`block` kinds, where capacity comes from the device.
    #[serde(default)]
    pub size: Option<ByteSize>,
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
    /// Regular file on a normal filesystem (ext4/tmpfs), sized by [`DiskSpec::size`]. Intended for testing where no dedicated device is available.
    File,
}

impl Default for DiskKind {
    fn default() -> Self {
        DiskKind::Nvme
    }
}

/// An origin tier the daemon fetches stripes from on a P2P cache
/// miss. Keyed by `id`; the future `backend::http` runtime is
/// constructed from one of these. Config surface only: no runtime is
/// wired to it yet.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct BackendSpec {
    /// Unique identifier; referenced by `FrontendSpec.backend`.
    pub id: String,
    /// Backend implementation selector.
    pub kind: BackendKind,
    /// Origin endpoint as `host:port`, resolved via DNS at startup
    /// (IPv4-only in v1). Examples: `origin.example.com:443`,
    /// `127.0.0.1:9000`. Must not include a URL scheme or path.
    pub endpoint: String,
    /// Stripe granularity for deterministic StripeKey derivation.
    #[serde(default = "default_stripe_size_bytes")]
    pub stripe_size_bytes: u64,
    /// Max concurrent in-flight origin HTTP requests per shard.
    #[serde(default = "default_http_concurrency")]
    pub http_concurrency: u32,
    /// Reserved: S3 bucket name. Not yet wired. S3 addressing is
    /// currently path-style: the bucket is carried in the client's
    /// request path (`/bucket/key`) and forwarded verbatim to the
    /// origin, so this field has no effect today. Accepted for
    /// forward-compatibility.
    #[serde(default)]
    pub bucket: Option<String>,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum BackendKind {
    Http,
    S3,
}

fn default_stripe_size_bytes() -> u64 {
    4 * 1024 * 1024
}

fn default_http_concurrency() -> u32 {
    64
}

/// A workload-facing listener the daemon serves cached objects from.
/// Keyed by `id`; `backend` must reference an existing
/// [`BackendSpec::id`] (enforced by load-time validation). Config
/// surface only.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct FrontendSpec {
    pub id: String,
    /// Frontend protocol selector.
    pub kind: FrontendKind,
    /// Listen address, e.g. `0.0.0.0:9000`.
    pub bind: String,
    /// Id of the [`BackendSpec`] this frontend serves from.
    pub backend: String,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum FrontendKind {
    Http,
    S3,
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
        assert_eq!(c.p2p.fingers_per_node, 100);
        assert!(c.p2p.local_node_id.is_none());
        assert!(c.p2p.local_labels.is_empty());
        assert!(c.peers.is_empty());
        assert!(c.disks.is_empty());
        assert!(c.backends.is_empty());
        assert!(c.frontends.is_empty());
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

    #[test]
    fn p2p_defaults_are_populated() {
        let c: Config = toml::from_str("").unwrap();
        assert_eq!(c.p2p.fingers_per_node, 100);
        assert!(c.p2p.local_node_id.is_none());
        assert!(c.p2p.local_labels.is_empty());
    }

    #[test]
    fn p2p_round_trips() {
        let s = r#"
[p2p]
fingers_per_node = 128
local_node_id = 42
local_labels = ["us-west", "az1", "row3", "rack7"]
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(c.p2p.fingers_per_node, 128);
        assert_eq!(c.p2p.local_node_id, Some(42));
        assert_eq!(
            c.p2p.local_labels,
            vec![
                "us-west".to_string(),
                "az1".to_string(),
                "row3".to_string(),
                "rack7".to_string(),
            ]
        );
    }

    #[test]
    fn unknown_fields_rejected_in_p2p() {
        let s = "[p2p]\nbogus = 1\n";
        assert!(toml::from_str::<Config>(s).is_err());
    }

    #[test]
    fn peer_labels_default_to_empty() {
        let s = r#"
[[peers]]
id = 1
transport = "tcp"
address = "127.0.0.1:9000"
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert!(c.peers[0].labels.is_empty());
    }

    #[test]
    fn peer_labels_round_trip() {
        let s = r#"
[[peers]]
id = 1
transport = "tcp"
address = "127.0.0.1:9000"
labels = ["us-west", "az1", "row3", "rack7"]
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(
            c.peers[0].labels,
            vec![
                "us-west".to_string(),
                "az1".to_string(),
                "row3".to_string(),
                "rack7".to_string(),
            ]
        );
    }

    #[test]
    fn backend_and_frontend_round_trip() {
        let s = r#"
[[backends]]
id = "primary-http"
kind = "http"
endpoint = "https://origin.example.com"
stripe_size_bytes = 8388608
http_concurrency = 32

[[frontends]]
id = "workload-http"
kind = "http"
bind = "0.0.0.0:9000"
backend = "primary-http"
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(c.backends.len(), 1);
        let b = &c.backends[0];
        assert_eq!(b.id, "primary-http");
        assert_eq!(b.kind, BackendKind::Http);
        assert_eq!(b.endpoint, "https://origin.example.com");
        assert_eq!(b.stripe_size_bytes, 8 * 1024 * 1024);
        assert_eq!(b.http_concurrency, 32);

        assert_eq!(c.frontends.len(), 1);
        let f = &c.frontends[0];
        assert_eq!(f.id, "workload-http");
        assert_eq!(f.kind, FrontendKind::Http);
        assert_eq!(f.bind, "0.0.0.0:9000");
        assert_eq!(f.backend, "primary-http");
    }

    #[test]
    fn backend_optional_fields_default() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://example.com"
"#;
        let c: Config = toml::from_str(s).unwrap();
        let b = &c.backends[0];
        assert_eq!(b.stripe_size_bytes, 4 * 1024 * 1024);
        assert_eq!(b.http_concurrency, 64);
    }

    #[test]
    fn unknown_fields_rejected_in_backend() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://example.com"
extra = "nope"
"#;
        assert!(toml::from_str::<Config>(s).is_err());
    }

    #[test]
    fn unknown_fields_rejected_in_frontend() {
        let s = r#"
[[frontends]]
id = "f"
kind = "http"
bind = "0.0.0.0:9000"
backend = "b"
extra = "nope"
"#;
        assert!(toml::from_str::<Config>(s).is_err());
    }

    #[test]
    fn backend_and_frontend_kind_case_sensitive() {
        let ok = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://example.com"
"#;
        assert!(toml::from_str::<Config>(ok).is_ok());

        let bad = r#"
[[backends]]
id = "b"
kind = "Http"
endpoint = "https://example.com"
"#;
        assert!(toml::from_str::<Config>(bad).is_err());
    }

    #[test]
    fn s3_backend_round_trips_with_bucket() {
        let s = r#"
[[backends]]
id = "primary-s3"
kind = "s3"
endpoint = "s3.us-east-1.amazonaws.com:443"
bucket = "my-bucket"
"#;
        let c: Config = toml::from_str(s).unwrap();
        let b = &c.backends[0];
        assert_eq!(b.kind, BackendKind::S3);
        assert_eq!(b.bucket.as_deref(), Some("my-bucket"));
    }

    #[test]
    fn s3_backend_optional_fields_default_to_none() {
        let s = r#"
[[backends]]
id = "b"
kind = "s3"
endpoint = "s3.example.com:443"
"#;
        let c: Config = toml::from_str(s).unwrap();
        let b = &c.backends[0];
        assert_eq!(b.kind, BackendKind::S3);
        assert!(b.bucket.is_none());
    }

    #[test]
    fn s3_frontend_kind_round_trips() {
        let s = r#"
[[frontends]]
id = "workload-s3"
kind = "s3"
bind = "0.0.0.0:9000"
backend = "b"
"#;
        let c: Config = toml::from_str(s).unwrap();
        assert_eq!(c.frontends[0].kind, FrontendKind::S3);
    }

    #[test]
    fn s3_kind_is_case_sensitive() {
        let bad_backend = r#"
[[backends]]
id = "b"
kind = "S3"
endpoint = "s3.example.com:443"
"#;
        assert!(toml::from_str::<Config>(bad_backend).is_err());

        let bad_frontend = r#"
[[frontends]]
id = "f"
kind = "S3"
bind = "0.0.0.0:9000"
backend = "b"
"#;
        assert!(toml::from_str::<Config>(bad_frontend).is_err());
    }
}
