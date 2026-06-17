// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Fabric module configuration: provider selection heuristic and the
//! `FabricConfig` carried by later phases when constructing endpoints.

use std::sync::Arc;

use crate::runtime::{Threading, WorkerIdx};

use super::error::{FabricError, Result};
use super::types::PeerId;

#[derive(Copy, Clone, Eq, PartialEq, Debug)]
pub enum Provider {
    Verbs,
    Tcp,
}

impl Provider {
    /// Heuristic mapping from a Linux net/RDMA device name to a
    /// libfabric provider. Anything that looks RDMA-capable picks
    /// `verbs`; everything else falls back to the `tcp` provider.
    pub fn from_device_name(name: &str) -> Self {
        if name.starts_with("mlx") || name.starts_with("ib") || name.starts_with("rocep") {
            Provider::Verbs
        } else {
            Provider::Tcp
        }
    }

    /// Whether `fi_writedata` delivers a remote CQ-data completion to
    /// the target (RDMA Write With Immediate). `verbs` maps it to a
    /// Write-With-Immediate that consumes a target recv and surfaces the
    /// 32-bit immediate, so the server can land a page-ack with a single
    /// RMA op. The native `tcp` provider performs the RMA write but never
    /// delivers the immediate to the target, so the server must fall back
    /// to a framed `PageAck` send on that provider.
    pub fn supports_write_with_imm(self) -> bool {
        matches!(self, Provider::Verbs)
    }
}

#[derive(Clone)]
pub struct FabricConfig {
    pub device_name: String,
    pub provider: Provider,
    pub listen: bool,
    pub listen_addr: Option<String>,
    pub max_inflight: usize,
    /// Per-connection receive window: the number of receive buffers each
    /// connection's `RecvPool` keeps posted at all times. Under FI_EP_MSG
    /// every connection (inbound accepted and outbound dialed) self-arms
    /// this many recvs into the shared completion registry, so this is a
    /// modest per-connection sliding window, NOT a server-wide total. It
    /// bounds how many inbound messages one peer can have in flight before
    /// the progress thread re-arms. The shared registry is sized to
    /// `max_inflight + max_connections * rpc_posted_recvs` so outbound
    /// sends/RMA always have slots regardless of how many peers connect.
    pub rpc_posted_recvs: usize,
    /// Upper bound on the number of concurrent connections (inbound
    /// accepted plus outbound dialed) the completion registry budgets
    /// receive slots for. The registry is fixed-capacity at `Fabric::new`
    /// while connections are added dynamically, so this caps the recv-pool
    /// slot reservation: registry capacity is
    /// `max_inflight + max_connections * rpc_posted_recvs`. Must be `>= 1`.
    pub max_connections: usize,
    /// Number of long-lived worker threads the RPC server spawns to
    /// serve inbound requests. Each request is enqueued by the progress
    /// thread and picked up by one of these workers; the pool size
    /// bounds concurrent server-side request handling (and thus the
    /// thread count) regardless of arrival rate. Must be `>= 1`.
    pub rpc_worker_threads: usize,
    /// Number of libfabric endpoints (QPs) established per logical peer
    /// connection. All `qps_per_connection` endpoints are bundled inside a
    /// single `cm::Connection` (the connection table stays one entry per
    /// `PeerId`); outbound requests round-robin across them so a single
    /// peer's RMA writes spread over multiple QPs instead of serializing on
    /// one. The server side is automatic: each accepted endpoint gets its
    /// own `RecvPool` and replies on the endpoint the request arrived on.
    /// Must be `>= 1`; `1` reproduces the single-QP behavior exactly.
    pub qps_per_connection: usize,
    /// Maximum number of page writes the server keeps outstanding on the
    /// reverse RMA path before blocking for a completion. Each served
    /// page is posted as a single `fi_writedata` (RMA write carrying a
    /// 32-bit immediate that lands the page-ack on the client) and the
    /// worker pipelines up to this many before parking on the oldest
    /// completion. Deeper pipelines hide completion latency and lift the
    /// per-HCA serve throughput; `1` reproduces the original depth-1
    /// post-then-wait behavior exactly. Must be `>= 1`.
    pub write_pipeline_depth: usize,
    pub progress_threads: u8,
    /// Microseconds the progress thread sleeps when the CQ is empty,
    /// to bound idle CPU. Default is 10.
    pub progress_poll_us: u32,
    pub runtime: Arc<dyn Threading>,
    pub worker_idx: WorkerIdx,
    pub numa: Option<u16>,
    /// This node's own fabric identity, sent as the connection-manager
    /// private data on every outbound dial so the accepting peer learns
    /// who connected. The daemon assigns the real value; in-process
    /// tests and single-node setups can leave it at the default.
    pub self_peer: PeerId,
}

impl FabricConfig {
    pub fn validate(&self) -> Result<()> {
        if self.progress_threads < 1 {
            return Err(FabricError::BadConfig("progress_threads must be >= 1"));
        }
        if self.max_inflight == 0 {
            return Err(FabricError::BadConfig("max_inflight must be > 0"));
        }
        if self.rpc_posted_recvs == 0 {
            return Err(FabricError::BadConfig("rpc_posted_recvs must be >= 1"));
        }
        if self.max_connections == 0 {
            return Err(FabricError::BadConfig("max_connections must be >= 1"));
        }
        if self.rpc_worker_threads == 0 {
            return Err(FabricError::BadConfig("rpc_worker_threads must be >= 1"));
        }
        if self.qps_per_connection == 0 {
            return Err(FabricError::BadConfig("qps_per_connection must be >= 1"));
        }
        if self.write_pipeline_depth == 0 {
            return Err(FabricError::BadConfig("write_pipeline_depth must be >= 1"));
        }
        if self.listen && self.listen_addr.is_none() {
            return Err(FabricError::BadConfig(
                "listen=true requires listen_addr=Some(..)",
            ));
        }
        Ok(())
    }
}

/// Apply this crate's libfabric `tcp` provider tuning as process-wide
/// environment defaults. libfabric reads its tcp `fi_param`s lazily at
/// provider init (the first `fi_getinfo`, which in this crate happens on
/// spawned per-shard/per-worker threads), so these must be set while the
/// process is still single-threaded, before any [`Fabric`](super::Fabric)
/// is constructed or any shard/worker thread is spawned. Both knobs are
/// ignored by the `verbs` provider, so applying them unconditionally is
/// safe regardless of the device in use.
///
/// Call this once from `main` (or a benchmark entry point) at startup.
///
/// - `FI_TCP_ZEROCOPY_SIZE` (set to 16 KiB): enables the kernel
///   zero-copy send path (`send(MSG_ZEROCOPY)`) so the non-RDMA fallback
///   preserves the same zero-copy semantics as the verbs RMA path. 16 KiB
///   sits just above the tcp message-buffer size and well below the 2 MiB
///   page transfers, so every bulk `fi_write` qualifies. The provider
///   falls back to a copying send on kernels without TCP `MSG_ZEROCOPY`,
///   so it is always set (overriding any inherited value).
/// - `FI_TCP_MAX_SAVED` (set to 1048576): raises the cap on received
///   messages that arrive without a matching posted application buffer.
///   The provider queues such "saved" messages and its default cap is
///   only 64; once exceeded the provider stops making progress and the
///   next fabric request never completes, wedging the shard. A busy node
///   issues far more than 64 cross-node page fetches over the lifetime of
///   a long-lived fabric, so the default is unsafe for this workload. An
///   operator override (a value already present in the environment) is
///   respected.
///
/// SAFETY: the caller must invoke this while the process is still
/// single-threaded (no other threads spawned yet), so the `set_var`
/// calls cannot race a concurrent `getenv`/`setenv`.
pub fn apply_tcp_env_defaults() {
    // SAFETY: documented precondition - caller is single-threaded here.
    unsafe {
        std::env::set_var("FI_TCP_ZEROCOPY_SIZE", "16384");
    }
    if std::env::var_os("FI_TCP_MAX_SAVED").is_none() {
        // SAFETY: documented precondition - caller is single-threaded here.
        unsafe {
            std::env::set_var("FI_TCP_MAX_SAVED", "1048576");
        }
    }
}

pub fn defaults_for(
    device_name: impl Into<String>,
    runtime: Arc<dyn Threading>,
    worker_idx: WorkerIdx,
) -> FabricConfig {
    let device_name = device_name.into();
    let provider = Provider::from_device_name(&device_name);
    FabricConfig {
        device_name,
        provider,
        listen: false,
        listen_addr: None,
        max_inflight: 4096,
        rpc_posted_recvs: 32,
        max_connections: 256,
        rpc_worker_threads: 4,
        qps_per_connection: 1,
        write_pipeline_depth: 1,
        progress_threads: 2,
        progress_poll_us: 10,
        runtime,
        worker_idx,
        numa: None,
        self_peer: PeerId(0),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    struct DummyRuntime;
    impl Threading for DummyRuntime {
        fn worker_count(&self) -> usize {
            1
        }
        fn numa_of(&self, _idx: WorkerIdx) -> Option<u16> {
            None
        }
        fn spawn_pinned(
            &self,
            _idx: WorkerIdx,
            _name: &str,
            _f: Box<dyn FnOnce() + Send + 'static>,
        ) -> crate::runtime::JoinHandle {
            std::thread::spawn(|| {})
        }
    }

    fn rt() -> Arc<dyn Threading> {
        Arc::new(DummyRuntime)
    }

    #[test]
    fn provider_from_device_name_classifies_rdma_prefixes() {
        assert_eq!(Provider::from_device_name("mlx5_0"), Provider::Verbs);
        assert_eq!(Provider::from_device_name("ib0"), Provider::Verbs);
        assert_eq!(Provider::from_device_name("rocep1s0"), Provider::Verbs);
    }

    #[test]
    fn provider_from_device_name_defaults_to_tcp() {
        assert_eq!(Provider::from_device_name("eth0"), Provider::Tcp);
        assert_eq!(Provider::from_device_name("lo"), Provider::Tcp);
    }

    #[test]
    fn only_verbs_supports_write_with_imm() {
        assert!(Provider::Verbs.supports_write_with_imm());
        assert!(!Provider::Tcp.supports_write_with_imm());
    }

    #[test]
    fn validate_rejects_zero_progress_threads() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.progress_threads = 0;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_zero_max_inflight() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.max_inflight = 0;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_zero_rpc_posted_recvs() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.rpc_posted_recvs = 0;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_zero_rpc_worker_threads() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.rpc_worker_threads = 0;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_zero_qps_per_connection() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.qps_per_connection = 0;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_zero_write_pipeline_depth() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.write_pipeline_depth = 0;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_listen_without_addr() {
        let mut c = defaults_for("eth0", rt(), WorkerIdx(0));
        c.listen = true;
        c.listen_addr = None;
        match c.validate() {
            Err(FabricError::BadConfig(_)) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
    }

    #[test]
    fn validate_accepts_defaults() {
        let c = defaults_for("eth0", rt(), WorkerIdx(0));
        assert!(c.validate().is_ok());
    }
}
