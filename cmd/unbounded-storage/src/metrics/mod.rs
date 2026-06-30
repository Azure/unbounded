// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Process-wide Prometheus metrics for the storage daemon.
//!
//! The crate is thread-per-core: every shard runs single-threaded on a
//! hard-pinned thread and its hot-path state (`Pool`, fabric RPC, the
//! storage engine) is `!Send` and never crosses a thread boundary. So
//! instead of threading a metrics handle through that `!Send` object
//! graph, this module follows the same pattern as [`crate::obs`]: a
//! single process-global registry installed once at startup
//! ([`init`]), incremented from any thread through the free functions
//! below.
//!
//! Counters and histograms are recorded at the event site. Gauges that
//! are naturally *per shard* (free pages, in-flight RPCs, open fabric
//! connections) are tracked as **deltas** - each shard `inc`/`dec`s the
//! single process-wide gauge as it allocates and frees - so the
//! exported value is the live sum across all shards without any shard
//! ever needing to know its own index or clobbering a sibling's value.
//! Gauges that are naturally *per disk* are keyed by the disk path.
//!
//! Pull-style series that can only be sampled at scrape time
//! (config versions, `process_*`) are refreshed inside [`render`],
//! which the exporter thread (see [`exporter`]) calls on each `GET
//! /metrics`. All metric handles are `Send + Sync`, so the exporter
//! thread and the shard threads share them freely.

mod exporter;

use std::sync::OnceLock;

use prometheus::{
    Encoder, Histogram, HistogramOpts, HistogramVec, IntCounter, IntCounterVec, IntGauge,
    IntGaugeVec, Opts, Registry, TextEncoder,
};

use crate::config::ConfigVersionStatus;

pub use exporter::{DeviceInventoryStatus, ExporterError, spawn};

/// Content type for the Prometheus text exposition format (v0.0.4),
/// sent in the `/metrics` response.
pub const TEXT_CONTENT_TYPE: &str = "text/plain; version=0.0.4; charset=utf-8";

static RENDER_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// Latency histogram buckets in seconds: 50us, then a factor of 3 per
/// bucket up to ~8.9s. Covers a fast local cache hit through a slow
/// cross-node fabric or origin fetch in one shared layout.
fn latency_buckets() -> Vec<f64> {
    prometheus::exponential_buckets(50e-6, 3.0, 12).expect("valid bucket params")
}

/// Outcome of an operation that can succeed or fail. Renders to the
/// `outcome` label value used across the request-path metrics.
#[derive(Clone, Copy)]
pub enum Outcome {
    Ok,
    Err,
}

impl Outcome {
    fn as_str(self) -> &'static str {
        match self {
            Outcome::Ok => "ok",
            Outcome::Err => "err",
        }
    }
}

/// Result of a bufferpool lookup or storage-engine lookup.
#[derive(Clone, Copy)]
pub enum Lookup {
    Hit,
    Miss,
}

impl Lookup {
    fn as_str(self) -> &'static str {
        match self {
            Lookup::Hit => "hit",
            Lookup::Miss => "miss",
        }
    }
}

/// Where a bufferpool miss was ultimately satisfied from.
#[derive(Clone, Copy)]
pub enum MissSource {
    Peer,
    Disk,
    Origin,
}

impl MissSource {
    fn as_str(self) -> &'static str {
        match self {
            MissSource::Peer => "peer",
            MissSource::Disk => "disk",
            MissSource::Origin => "origin",
        }
    }
}

/// How the recursive p2p router dispatched a request.
#[derive(Clone, Copy)]
pub enum Disposition {
    Local,
    Forward,
}

impl Disposition {
    fn as_str(self) -> &'static str {
        match self {
            Disposition::Local => "local",
            Disposition::Forward => "forward",
        }
    }
}

/// A physical disk I/O direction.
#[derive(Clone, Copy)]
pub enum DiskOp {
    Read,
    Write,
}

impl DiskOp {
    fn as_str(self) -> &'static str {
        match self {
            DiskOp::Read => "read",
            DiskOp::Write => "write",
        }
    }
}

/// All metric handles, registered into one private [`Registry`]. Cloned
/// handles are cheap (`Arc` inside) and `Send + Sync`; the struct is
/// stored once in [`METRICS`] and shared by every thread.
struct Metrics {
    registry: Registry,

    build_info: IntGaugeVec,
    config_version: IntGaugeVec,

    // Frontend (http/s3 serving).
    frontend_requests: IntCounterVec,
    frontend_response_bytes: IntCounterVec,
    frontend_request_duration: HistogramVec,
    frontend_active_connections: IntGauge,

    // Bufferpool.
    bufferpool_requests: IntCounterVec,
    bufferpool_page_cache_requests: IntCounterVec,
    bufferpool_page_cache_evictions: IntCounter,
    bufferpool_miss_source: IntCounterVec,
    bufferpool_inflight_coalesced: IntCounter,
    bufferpool_pages_total: IntGauge,
    bufferpool_pages_free: IntGauge,
    bufferpool_pages_cached: IntGauge,
    bufferpool_prefetch_inflight_pages: IntGauge,

    // p2p routing.
    p2p_requests: IntCounterVec,
    p2p_hop_limit_exceeded: IntCounter,

    // Fan-out routing.
    fanout_cross_numa_fetches: IntCounter,

    // Fabric RPC.
    fabric_rpc_served: IntCounterVec,
    fabric_rpc_duration: Histogram,
    fabric_rpc_inflight: IntGauge,
    fabric_connections: IntGauge,
    fabric_pages_written: IntCounter,
    fabric_bytes_written: IntCounter,

    // Backend (origin fetch).
    backend_fetches: IntCounterVec,
    backend_bytes: IntCounterVec,
    backend_fetch_duration: HistogramVec,

    // Storage engine.
    storage_lookups: IntCounterVec,
    storage_disk_ops: IntCounterVec,
    storage_evictions: IntCounterVec,
    storage_admission_rejected: IntCounterVec,
    storage_btree_commit_duration: Histogram,
    storage_used_bytes: IntGaugeVec,
    storage_capacity_bytes: IntGaugeVec,
    ring_backpressure: IntCounter,

    // Process / liveness.
    shards_up: IntGauge,
    process_resident_memory_bytes: IntGauge,
    process_open_fds: IntGauge,
    process_cpu_seconds_total: IntCounter,
}

static METRICS: OnceLock<Metrics> = OnceLock::new();

/// Build, register, and install the process-global metric registry.
/// Idempotent: a second call is a no-op so tests that touch the metrics
/// surface can call it freely. Records the immutable `build_info` series.
pub fn init() {
    let _ = METRICS.get_or_init(|| {
        let m = Metrics::new().expect("metric construction and registration succeed");
        m.build_info
            .with_label_values(&[env!("CARGO_PKG_VERSION")])
            .set(1);
        m
    });
}

fn metrics() -> Option<&'static Metrics> {
    METRICS.get()
}

impl Metrics {
    fn new() -> prometheus::Result<Metrics> {
        let registry = Registry::new();

        let build_info = IntGaugeVec::new(
            Opts::new(
                "unbounded_storage_build_info",
                "Build information; constant 1 with the version as a label.",
            ),
            &["version"],
        )?;
        let config_version = IntGaugeVec::new(
            Opts::new(
                "unbounded_storage_config_version",
                "Tracked configuration versions (known, applied, startup).",
            ),
            &["kind"],
        )?;

        let frontend_requests = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_frontend_requests_total",
                "Frontend client requests served, by frontend, method and status.",
            ),
            &["frontend", "method", "status"],
        )?;
        let frontend_response_bytes = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_frontend_response_bytes_total",
                "Response body bytes streamed to frontend clients.",
            ),
            &["frontend"],
        )?;
        let frontend_request_duration = HistogramVec::new(
            HistogramOpts::new(
                "unbounded_storage_frontend_request_duration_seconds",
                "Frontend request service latency in seconds.",
            )
            .buckets(latency_buckets()),
            &["frontend"],
        )?;
        let frontend_active_connections = IntGauge::new(
            "unbounded_storage_frontend_active_connections",
            "Currently open frontend client connections across all shards.",
        )?;

        let bufferpool_requests = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_bufferpool_requests_total",
                "Bufferpool page requests, by result (hit/miss).",
            ),
            &["result"],
        )?;
        let bufferpool_page_cache_requests = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_bufferpool_page_cache_requests_total",
                "Bufferpool in-memory page-cache lookups, by result (hit/miss).",
            ),
            &["result"],
        )?;
        let bufferpool_page_cache_evictions = IntCounter::new(
            "unbounded_storage_bufferpool_page_cache_evictions_total",
            "Idle in-memory bufferpool pages evicted to satisfy allocation pressure.",
        )?;
        let bufferpool_miss_source = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_bufferpool_miss_source_total",
                "Where bufferpool misses were satisfied (peer/disk/origin).",
            ),
            &["source"],
        )?;
        let bufferpool_inflight_coalesced = IntCounter::new(
            "unbounded_storage_bufferpool_inflight_coalesced_total",
            "Requests that joined an existing single-flight fetch instead of starting a new one.",
        )?;
        let bufferpool_pages_total = IntGauge::new(
            "unbounded_storage_bufferpool_pages_total",
            "Total bufferpool pages across all shards.",
        )?;
        let bufferpool_pages_free = IntGauge::new(
            "unbounded_storage_bufferpool_pages_free",
            "Free bufferpool pages across all shards.",
        )?;
        let bufferpool_pages_cached = IntGauge::new(
            "unbounded_storage_bufferpool_pages_cached",
            "Idle in-memory cached bufferpool pages across all shards.",
        )?;
        let bufferpool_prefetch_inflight_pages = IntGauge::new(
            "unbounded_storage_bufferpool_prefetch_inflight_pages",
            "Prefetch pages currently in flight across all shards.",
        )?;

        let p2p_requests = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_p2p_requests_total",
                "Recursive p2p routing decisions, by disposition (local/forward).",
            ),
            &["disposition"],
        )?;
        let p2p_hop_limit_exceeded = IntCounter::new(
            "unbounded_storage_p2p_hop_limit_exceeded_total",
            "Routed requests dropped because the hop limit was exceeded.",
        )?;

        let fanout_cross_numa_fetches = IntCounter::new(
            "unbounded_storage_fanout_cross_numa_fetches_total",
            "Fetches whose serving shard was not on the backing drive's NUMA node (the node has no serving shard, or the drive is unpinned / its node is unknown).",
        )?;

        let fabric_rpc_served = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_fabric_rpc_served_total",
                "Server-side fabric RPCs served, by outcome.",
            ),
            &["outcome"],
        )?;
        let fabric_rpc_duration = Histogram::with_opts(
            HistogramOpts::new(
                "unbounded_storage_fabric_rpc_duration_seconds",
                "Client-side fabric RPC round-trip latency in seconds.",
            )
            .buckets(latency_buckets()),
        )?;
        let fabric_rpc_inflight = IntGauge::new(
            "unbounded_storage_fabric_rpc_inflight",
            "In-flight server-side fabric RPCs across all shards.",
        )?;
        let fabric_connections = IntGauge::new(
            "unbounded_storage_fabric_connections",
            "Open fabric connections across all shards.",
        )?;
        let fabric_pages_written = IntCounter::new(
            "unbounded_storage_fabric_pages_written_total",
            "Pages written to peers via fabric RMA.",
        )?;
        let fabric_bytes_written = IntCounter::new(
            "unbounded_storage_fabric_bytes_written_total",
            "Bytes written to peers via fabric RMA.",
        )?;

        let backend_fetches = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_backend_fetches_total",
                "Origin backend fetches, by backend and outcome.",
            ),
            &["backend", "outcome"],
        )?;
        let backend_bytes = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_backend_bytes_total",
                "Bytes fetched from origin backends.",
            ),
            &["backend"],
        )?;
        let backend_fetch_duration = HistogramVec::new(
            HistogramOpts::new(
                "unbounded_storage_backend_fetch_duration_seconds",
                "Origin backend fetch latency in seconds.",
            )
            .buckets(latency_buckets()),
            &["backend"],
        )?;

        let storage_lookups = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_engine_lookups_total",
                "Storage-engine index lookups, by result (hit/miss).",
            ),
            &["result"],
        )?;
        let storage_disk_ops = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_disk_ops_total",
                "Physical disk operations, by disk, op (read/write) and outcome.",
            ),
            &["disk", "op", "outcome"],
        )?;
        let storage_evictions = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_evictions_total",
                "SIEVE evictions, by disk.",
            ),
            &["disk"],
        )?;
        let storage_admission_rejected = IntCounterVec::new(
            Opts::new(
                "unbounded_storage_admission_rejected_total",
                "Pages rejected by the admission filter, by disk.",
            ),
            &["disk"],
        )?;
        let storage_btree_commit_duration = Histogram::with_opts(
            HistogramOpts::new(
                "unbounded_storage_btree_commit_duration_seconds",
                "On-disk B-tree commit latency in seconds.",
            )
            .buckets(latency_buckets()),
        )?;
        let storage_used_bytes = IntGaugeVec::new(
            Opts::new(
                "unbounded_storage_used_bytes",
                "Bytes currently stored on each disk.",
            ),
            &["disk"],
        )?;
        let storage_capacity_bytes = IntGaugeVec::new(
            Opts::new(
                "unbounded_storage_capacity_bytes",
                "Usable capacity of each disk in bytes.",
            ),
            &["disk"],
        )?;
        let ring_backpressure = IntCounter::new(
            "unbounded_storage_ring_backpressure_total",
            "Times a submission was deferred because an io_uring was full.",
        )?;

        let shards_up = IntGauge::new(
            "unbounded_storage_shards_up",
            "Number of shard threads currently running.",
        )?;
        let process_resident_memory_bytes = IntGauge::new(
            "process_resident_memory_bytes",
            "Resident set size of the process in bytes.",
        )?;
        let process_open_fds =
            IntGauge::new("process_open_fds", "Number of open file descriptors.")?;
        let process_cpu_seconds_total = IntCounter::new(
            "process_cpu_seconds_total",
            "Total user and system CPU time spent in seconds.",
        )?;

        let m = Metrics {
            registry,
            build_info,
            config_version,
            frontend_requests,
            frontend_response_bytes,
            frontend_request_duration,
            frontend_active_connections,
            bufferpool_requests,
            bufferpool_page_cache_requests,
            bufferpool_page_cache_evictions,
            bufferpool_miss_source,
            bufferpool_inflight_coalesced,
            bufferpool_pages_total,
            bufferpool_pages_free,
            bufferpool_pages_cached,
            bufferpool_prefetch_inflight_pages,
            p2p_requests,
            p2p_hop_limit_exceeded,
            fanout_cross_numa_fetches,
            fabric_rpc_served,
            fabric_rpc_duration,
            fabric_rpc_inflight,
            fabric_connections,
            fabric_pages_written,
            fabric_bytes_written,
            backend_fetches,
            backend_bytes,
            backend_fetch_duration,
            storage_lookups,
            storage_disk_ops,
            storage_evictions,
            storage_admission_rejected,
            storage_btree_commit_duration,
            storage_used_bytes,
            storage_capacity_bytes,
            ring_backpressure,
            shards_up,
            process_resident_memory_bytes,
            process_open_fds,
            process_cpu_seconds_total,
        };
        m.register_all()?;
        Ok(m)
    }

    fn register_all(&self) -> prometheus::Result<()> {
        let collectors: Vec<Box<dyn prometheus::core::Collector>> = vec![
            Box::new(self.build_info.clone()),
            Box::new(self.config_version.clone()),
            Box::new(self.frontend_requests.clone()),
            Box::new(self.frontend_response_bytes.clone()),
            Box::new(self.frontend_request_duration.clone()),
            Box::new(self.frontend_active_connections.clone()),
            Box::new(self.bufferpool_requests.clone()),
            Box::new(self.bufferpool_page_cache_requests.clone()),
            Box::new(self.bufferpool_page_cache_evictions.clone()),
            Box::new(self.bufferpool_miss_source.clone()),
            Box::new(self.bufferpool_inflight_coalesced.clone()),
            Box::new(self.bufferpool_pages_total.clone()),
            Box::new(self.bufferpool_pages_free.clone()),
            Box::new(self.bufferpool_pages_cached.clone()),
            Box::new(self.bufferpool_prefetch_inflight_pages.clone()),
            Box::new(self.p2p_requests.clone()),
            Box::new(self.p2p_hop_limit_exceeded.clone()),
            Box::new(self.fanout_cross_numa_fetches.clone()),
            Box::new(self.fabric_rpc_served.clone()),
            Box::new(self.fabric_rpc_duration.clone()),
            Box::new(self.fabric_rpc_inflight.clone()),
            Box::new(self.fabric_connections.clone()),
            Box::new(self.fabric_pages_written.clone()),
            Box::new(self.fabric_bytes_written.clone()),
            Box::new(self.backend_fetches.clone()),
            Box::new(self.backend_bytes.clone()),
            Box::new(self.backend_fetch_duration.clone()),
            Box::new(self.storage_lookups.clone()),
            Box::new(self.storage_disk_ops.clone()),
            Box::new(self.storage_evictions.clone()),
            Box::new(self.storage_admission_rejected.clone()),
            Box::new(self.storage_btree_commit_duration.clone()),
            Box::new(self.storage_used_bytes.clone()),
            Box::new(self.storage_capacity_bytes.clone()),
            Box::new(self.ring_backpressure.clone()),
            Box::new(self.shards_up.clone()),
            Box::new(self.process_resident_memory_bytes.clone()),
            Box::new(self.process_open_fds.clone()),
            Box::new(self.process_cpu_seconds_total.clone()),
        ];
        for c in collectors {
            self.registry.register(c)?;
        }
        Ok(())
    }
}

// ---------------------------------------------------------------------
// Recording helpers. Each is a no-op until `init` has run, so library
// tests and non-daemon callers pay nothing and never panic.
// ---------------------------------------------------------------------

/// Record one served frontend request and its body size and latency.
pub fn frontend_request(frontend: &str, method: &str, status: u16, bytes: u64, duration_secs: f64) {
    if let Some(m) = metrics() {
        let status = status_str(status);
        m.frontend_requests
            .with_label_values(&[frontend, method, status])
            .inc();
        m.frontend_response_bytes
            .with_label_values(&[frontend])
            .inc_by(bytes);
        m.frontend_request_duration
            .with_label_values(&[frontend])
            .observe(duration_secs);
    }
}

/// Adjust the live frontend connection gauge by `delta` (use +1 on
/// accept, -1 on close).
pub fn frontend_connections_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.frontend_active_connections.add(delta);
    }
}

/// Record a bufferpool page request result (hit or miss).
pub fn bufferpool_request(result: Lookup) {
    if let Some(m) = metrics() {
        m.bufferpool_requests
            .with_label_values(&[result.as_str()])
            .inc();
    }
}

/// Record an in-memory bufferpool page-cache lookup result (hit or miss).
pub fn bufferpool_page_cache_request(result: Lookup) {
    if let Some(m) = metrics() {
        m.bufferpool_page_cache_requests
            .with_label_values(&[result.as_str()])
            .inc();
    }
}

/// Record eviction of one idle in-memory cached page.
pub fn bufferpool_page_cache_evicted() {
    if let Some(m) = metrics() {
        m.bufferpool_page_cache_evictions.inc();
    }
}

/// Adjust the live cached-page gauge by `delta`.
pub fn bufferpool_cached_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.bufferpool_pages_cached.add(delta);
    }
}

/// Record where a bufferpool miss was satisfied from.
pub fn bufferpool_miss_source(source: MissSource) {
    if let Some(m) = metrics() {
        m.bufferpool_miss_source
            .with_label_values(&[source.as_str()])
            .inc();
    }
}

/// Record a request that coalesced onto an in-flight single-flight fetch.
pub fn bufferpool_coalesced() {
    if let Some(m) = metrics() {
        m.bufferpool_inflight_coalesced.inc();
    }
}

/// Add `pages` to the process-wide total page count (call once per shard
/// as its pool is carved).
pub fn bufferpool_pages_added(pages: i64) {
    if let Some(m) = metrics() {
        m.bufferpool_pages_total.add(pages);
        m.bufferpool_pages_free.add(pages);
    }
}

/// Adjust the free-page gauge: negative when a page is pinned, positive
/// when it is returned.
pub fn bufferpool_free_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.bufferpool_pages_free.add(delta);
    }
}

/// Adjust the in-flight prefetch-page gauge.
pub fn bufferpool_prefetch_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.bufferpool_prefetch_inflight_pages.add(delta);
    }
}

/// Record a recursive routing decision.
pub fn p2p_request(disposition: Disposition) {
    if let Some(m) = metrics() {
        m.p2p_requests
            .with_label_values(&[disposition.as_str()])
            .inc();
    }
}

/// Record a routed request dropped for exceeding the hop limit.
pub fn p2p_hop_limit_exceeded() {
    if let Some(m) = metrics() {
        m.p2p_hop_limit_exceeded.inc();
    }
}

/// Record a fetch whose serving shard was not on the backing drive's
/// NUMA node (no co-located shard, or an unpinned / unknown-node drive).
pub fn fanout_cross_numa_fetch() {
    if let Some(m) = metrics() {
        m.fanout_cross_numa_fetches.inc();
    }
}

/// Record a server-side fabric RPC completion.
pub fn fabric_rpc_served(outcome: Outcome) {
    if let Some(m) = metrics() {
        m.fabric_rpc_served
            .with_label_values(&[outcome.as_str()])
            .inc();
    }
}

/// Record a client-side fabric RPC round-trip latency.
pub fn fabric_rpc_duration(duration_secs: f64) {
    if let Some(m) = metrics() {
        m.fabric_rpc_duration.observe(duration_secs);
    }
}

/// Adjust the in-flight server-side fabric RPC gauge.
pub fn fabric_inflight_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.fabric_rpc_inflight.add(delta);
    }
}

/// Adjust the open fabric connection gauge.
pub fn fabric_connections_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.fabric_connections.add(delta);
    }
}

/// Record an RMA write of `pages`/`bytes` to a peer.
pub fn fabric_written(pages: u64, bytes: u64) {
    if let Some(m) = metrics() {
        m.fabric_pages_written.inc_by(pages);
        m.fabric_bytes_written.inc_by(bytes);
    }
}

/// Record an origin backend fetch, its size and latency.
pub fn backend_fetch(backend: &str, outcome: Outcome, bytes: u64, duration_secs: f64) {
    if let Some(m) = metrics() {
        m.backend_fetches
            .with_label_values(&[backend, outcome.as_str()])
            .inc();
        m.backend_bytes.with_label_values(&[backend]).inc_by(bytes);
        m.backend_fetch_duration
            .with_label_values(&[backend])
            .observe(duration_secs);
    }
}

/// Drive a backend fetch future to completion, recording its outcome,
/// transferred byte count, and latency once it resolves.
///
/// Mirrors [`crate::obs::instrument`] so the two can be nested at a
/// backend's `fetch_stream` call site: the byte count is known up
/// front (the requested range length, or the metadata page size) and
/// is only attributed to `backend_bytes_total` on success.
pub async fn instrument_backend<T, E, F>(backend: String, bytes: u64, fut: F) -> Result<T, E>
where
    F: std::future::Future<Output = Result<T, E>>,
{
    let start = std::time::Instant::now();
    let result = fut.await;
    let elapsed = start.elapsed().as_secs_f64();

    match &result {
        Ok(_) => backend_fetch(&backend, Outcome::Ok, bytes, elapsed),
        Err(_) => backend_fetch(&backend, Outcome::Err, 0, elapsed),
    }

    result
}

/// Record a storage-engine index lookup result.
pub fn storage_lookup(result: Lookup) {
    if let Some(m) = metrics() {
        m.storage_lookups
            .with_label_values(&[result.as_str()])
            .inc();
    }
}

/// Record a physical disk operation.
pub fn storage_disk_op(disk: &str, op: DiskOp, outcome: Outcome) {
    if let Some(m) = metrics() {
        m.storage_disk_ops
            .with_label_values(&[disk, op.as_str(), outcome.as_str()])
            .inc();
    }
}

/// Record a SIEVE eviction on `disk`.
pub fn storage_eviction(disk: &str) {
    if let Some(m) = metrics() {
        m.storage_evictions.with_label_values(&[disk]).inc();
    }
}

/// Record an admission-filter rejection on `disk`.
pub fn storage_admission_rejected(disk: &str) {
    if let Some(m) = metrics() {
        m.storage_admission_rejected
            .with_label_values(&[disk])
            .inc();
    }
}

/// Record an on-disk B-tree commit latency.
pub fn storage_btree_commit_duration(duration_secs: f64) {
    if let Some(m) = metrics() {
        m.storage_btree_commit_duration.observe(duration_secs);
    }
}

/// Set the live used-bytes gauge for `disk`.
pub fn storage_used_bytes(disk: &str, bytes: i64) {
    if let Some(m) = metrics() {
        m.storage_used_bytes.with_label_values(&[disk]).set(bytes);
    }
}

/// Set the capacity gauge for `disk` (call once when the disk is opened).
pub fn storage_capacity_bytes(disk: &str, bytes: i64) {
    if let Some(m) = metrics() {
        m.storage_capacity_bytes
            .with_label_values(&[disk])
            .set(bytes);
    }
}

/// Record an io_uring submission deferred under backpressure.
pub fn ring_backpressure() {
    if let Some(m) = metrics() {
        m.ring_backpressure.inc();
    }
}

/// Adjust the running-shard gauge (use +1 when a shard starts, -1 when
/// it stops).
pub fn shards_delta(delta: i64) {
    if let Some(m) = metrics() {
        m.shards_up.add(delta);
    }
}

/// Render the current metric state in the Prometheus text exposition
/// format, refreshing the pull-style series (config versions and
/// `process_*`) at scrape time. Returns an empty vector before [`init`].
pub fn render(versions: &ConfigVersionStatus) -> Vec<u8> {
    let _guard = RENDER_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let m = match metrics() {
        Some(m) => m,
        None => return Vec::new(),
    };

    m.config_version
        .with_label_values(&["known"])
        .set(versions.known() as i64);
    m.config_version
        .with_label_values(&["applied"])
        .set(versions.applied() as i64);
    m.config_version
        .with_label_values(&["startup"])
        .set(versions.startup() as i64);

    refresh_process_metrics(m);

    let mut buf = Vec::with_capacity(8 * 1024);
    let encoder = TextEncoder::new();
    let families = m.registry.gather();
    if encoder.encode(&families, &mut buf).is_err() {
        return Vec::new();
    }
    buf
}

#[cfg(target_os = "linux")]
fn refresh_process_metrics(m: &Metrics) {
    // RSS pages and total CPU jiffies come from /proc/self; both are
    // best-effort and silently skipped if the format is unexpected.
    if let Ok(statm) = std::fs::read_to_string("/proc/self/statm") {
        if let Some(rss_pages) = statm.split_whitespace().nth(1) {
            if let Ok(pages) = rss_pages.parse::<i64>() {
                let page_size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
                if page_size > 0 {
                    m.process_resident_memory_bytes.set(pages * page_size);
                }
            }
        }
    }

    if let Ok(stat) = std::fs::read_to_string("/proc/self/stat") {
        // utime is field 14, stime field 15 (1-based), after the comm
        // field which may contain spaces inside parentheses.
        if let Some(after) = stat.rsplit_once(") ") {
            let fields: Vec<&str> = after.1.split_whitespace().collect();
            if let (Some(utime), Some(stime)) = (fields.get(11), fields.get(12)) {
                if let (Ok(u), Ok(s)) = (utime.parse::<u64>(), stime.parse::<u64>()) {
                    let hz = unsafe { libc::sysconf(libc::_SC_CLK_TCK) };
                    if hz > 0 {
                        let total = (u + s) / hz as u64;
                        let cur = m.process_cpu_seconds_total.get();
                        if total > cur {
                            m.process_cpu_seconds_total.inc_by(total - cur);
                        }
                    }
                }
            }
        }
    }

    if let Ok(entries) = std::fs::read_dir("/proc/self/fd") {
        m.process_open_fds.set(entries.count() as i64);
    }
}

#[cfg(not(target_os = "linux"))]
fn refresh_process_metrics(_m: &Metrics) {}

/// Map a numeric HTTP status to a small set of interned label strings,
/// keeping the `status` label cardinality bounded to the codes the
/// frontends actually emit.
fn status_str(status: u16) -> &'static str {
    match status {
        200 => "200",
        206 => "206",
        304 => "304",
        400 => "400",
        403 => "403",
        404 => "404",
        405 => "405",
        416 => "416",
        500 => "500",
        502 => "502",
        503 => "503",
        504 => "504",
        _ => "other",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn init_is_idempotent_and_render_contains_build_info() {
        init();
        init();
        let versions = ConfigVersionStatus::new(7);
        let text = String::from_utf8(render(&versions)).unwrap();
        assert!(
            text.contains("unbounded_storage_build_info"),
            "build_info series present: {text}"
        );
    }

    #[test]
    fn helpers_move_their_series() {
        init();
        frontend_request("fe", "GET", 200, 4096, 0.001);
        bufferpool_request(Lookup::Hit);
        bufferpool_page_cache_request(Lookup::Hit);
        bufferpool_page_cache_evicted();
        bufferpool_cached_delta(1);
        bufferpool_miss_source(MissSource::Disk);
        backend_fetch("b", Outcome::Ok, 100, 0.01);
        storage_disk_op("/dev/x", DiskOp::Read, Outcome::Ok);
        let versions = ConfigVersionStatus::new(1);
        let text = String::from_utf8(render(&versions)).unwrap();
        assert!(text.contains(
            "unbounded_storage_frontend_requests_total{frontend=\"fe\",method=\"GET\",status=\"200\"}"
        ));
        assert!(text.contains("unbounded_storage_bufferpool_requests_total{result=\"hit\"}"));
        assert!(
            text.contains("unbounded_storage_bufferpool_page_cache_requests_total{result=\"hit\"}")
        );
        assert!(text.contains("unbounded_storage_bufferpool_page_cache_evictions_total"));
        assert!(text.contains("unbounded_storage_bufferpool_pages_cached"));
        assert!(
            text.contains("unbounded_storage_backend_fetches_total{backend=\"b\",outcome=\"ok\"}")
        );
        assert!(text.contains(
            "unbounded_storage_disk_ops_total{disk=\"/dev/x\",op=\"read\",outcome=\"ok\"}"
        ));
    }

    #[test]
    fn config_version_series_track_the_handle() {
        init();
        let versions = ConfigVersionStatus::new(3);
        let text = String::from_utf8(render(&versions)).unwrap();
        assert!(text.contains("unbounded_storage_config_version{kind=\"startup\"} 3"));
    }

    #[test]
    fn status_str_buckets_unknown_codes() {
        assert_eq!(status_str(200), "200");
        assert_eq!(status_str(418), "other");
    }
}
