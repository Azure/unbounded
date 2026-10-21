//! Environment and deployment configuration, validated before startup.
//!
//! Default to all eligible physical cores, subject to CPU and resource budgets.
//! Shares and physical NIC bindings come from accepted controller membership.

use crate::error::Error;
use crate::error::Result;
use crate::model::PAGE_BYTES;
use crate::store::MAX_HEADER_BYTES;
use racer_control_wire::ClusterId;
use racer_control_wire::DEFAULT_SHARES;
use racer_control_wire::NodeId;
use std::net::IpAddr;
use std::net::SocketAddr;
use std::num::NonZeroUsize;
use std::path::Path;
use std::path::PathBuf;
use std::time::Duration;

pub const DEFAULT_MAX_THREADS: usize = usize::MAX;
/// Default per-cache, per-worker origin connection cap. Remote object stores
/// deliver roughly 30 MB/s per connection, so filling a fast NIC needs a few
/// hundred page fetches in flight across all workers.
pub const DEFAULT_ORIGIN_CONNECTIONS_PER_CACHE: usize = 32;
/// Not a Node UID. Only verified enrollment/local identity recovery may replace it.
pub const UNRESOLVED_NODE_ID: &str = "";
const MIB: u64 = 1024 * 1024;
const MAX_BYTES: u64 = 64 * 1024 * MIB;
const MAX_ENTRIES: usize = 1_048_576;

/// Disk retention policy only; never changes request admission or authorization.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionMode {
    Disabled,
    SecondSight,
}

/// Independent bounded-resource dimensions. Validation precedes resource creation.
#[derive(Clone, Debug)]
pub struct Limits {
    pub plaintext_bytes: NonZeroUsize,
    pub ciphertext_bytes: NonZeroUsize,
    pub dirty_bytes: NonZeroUsize,
    pub registered_bytes: NonZeroUsize,
    pub request_context_bytes: NonZeroUsize,
    pub flights: NonZeroUsize,
    pub waiters_per_flight: NonZeroUsize,
    pub queue_entries: NonZeroUsize,
    pub connections_per_neighbor: NonZeroUsize,
    pub client_connections: NonZeroUsize,
    pub pipes: NonZeroUsize,
    pub range_window_pages: NonZeroUsize,
    pub header_bytes: NonZeroUsize,
    pub placement_cache_bytes: NonZeroUsize,
    pub cached_paths: NonZeroUsize,
    pub path_cache_bytes: NonZeroUsize,
    /// Per-worker active searches, independent of cache retention and worker count.
    pub active_path_searches: NonZeroUsize,
    /// Old live membership generations in addition to current; cache-only
    /// publications reuse a generation and do not consume another slot.
    pub retained_snapshots: NonZeroUsize,
    pub metadata_entries: NonZeroUsize,
    pub relay_transfers: NonZeroUsize,
}

/// Which limit dimensions were left unset in the environment.
///
/// Unset dimensions carry per-worker defaults. Worker sizing divides every
/// node-wide dimension by the I/O worker count, so a fixed node-wide default
/// shrinks each lane's budget as cores are added and eventually falls under a
/// per-worker floor. Sizing then sheds workers until the partition fits, which
/// turns a larger host into a smaller dataplane. Scaling the defaults by the
/// planned worker count keeps each lane's budget constant instead.
///
/// A dimension set explicitly in the environment is node-wide by definition and
/// is never scaled.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct ScaledLimits {
    pub plaintext_bytes: bool,
    pub ciphertext_bytes: bool,
    pub dirty_bytes: bool,
    pub registered_bytes: bool,
    pub request_context_bytes: bool,
    pub flights: bool,
    pub queue_entries: bool,
    pub client_connections: bool,
    pub pipes: bool,
    pub placement_cache_bytes: bool,
    pub cached_paths: bool,
    pub path_cache_bytes: bool,
    pub metadata_entries: bool,
    pub relay_transfers: bool,
}

pub struct Config {
    pub warm: crate::read::warm::Config,
    pub send_crc_pair: Option<crate::telemetry::Pair>,
    pub page_hedge: crate::read::candidates::HedgeConfig,
    /// Order in which a non-owner requester visits a page's ranked owners.
    pub replica_selection: crate::read::candidates::ReplicaSelection,
    pub peer_admission: crate::peer::Config,
    pub peer_receive: crate::peer::receive::Config,
    pub shares: std::num::NonZeroU32,
    pub disk_page_entries: NonZeroUsize,
    pub admission_mode: AdmissionMode,
    /// Node-wide Bloom history, divided by final I/O worker count.
    pub admission_history_bytes: NonZeroUsize,
    pub admission_period: Duration,
    pub checkpoint_bytes: NonZeroUsize,
    pub cluster: ClusterId,
    /// Resolved by verified bootstrap/local identity recovery before workers start,
    /// not from a caller-provided UID or the Downward API's node name.
    /// Environment parsing leaves this unresolved; validation is not authentication.
    pub node: NodeId,
    /// Total thread cap, minimum two; usize::MAX means no configured ceiling.
    /// Control and diagnostics run on I/O threads within this budget.
    pub max_threads: usize,
    /// Paired exact counts; absent preserves automatic sizing and reduction.
    pub io_workers: Option<usize>,
    pub crypto_helpers: Option<usize>,
    /// Opt into logical-CPU sizing instead of one eligible CPU per physical core.
    pub allow_smt: bool,
    pub enable_rdma: bool,
    /// Opt into experimental opaque HTTP transit; materialized relay is the default.
    pub opaque_relay: bool,
    /// Peer-only TCP_NODELAY. Peer exchanges are strictly request/response with
    /// multi-segment signed heads, so Nagle stalls every round on a delayed ACK.
    /// Defaults on; set false only to reproduce the pre-nodelay socket behavior.
    pub peer_tcp_nodelay: bool,
    /// Send memory-resident pages over RDMA straight from their memory
    /// through an implicit on-demand paging MR, without a staging copy.
    /// Defaults on; devices without implicit ODP copy anyway. Set false to
    /// force the copy path, for example to compare benchmarks.
    pub rdma_zero_copy: bool,
    /// cgroup v2 memory-pressure monitoring and reclaim. Defaults on.
    pub memory_pressure: bool,
    pub pprof_enabled: bool,
    pub control_endpoint: String,
    pub peer_listen: std::net::SocketAddr,
    pub diagnostics_listen: std::net::SocketAddr,
    pub trust_bundle: PathBuf,
    pub service_account_token: PathBuf,
    /// Node-private persistent keys, separate from projected Secrets and slabs.
    pub identity_directory: PathBuf,
    pub slab_directory: PathBuf,
    pub device_directory: PathBuf,
    /// Startup bound for retrying selected devices whose exclusive open reports
    /// EBUSY, such as a predecessor process still releasing its claim. Zero
    /// disables waiting.
    pub device_busy_wait: Duration,
    pub slab_bytes: u64,
    pub segment_bytes: u64,
    pub free_segment_reserve: usize,
    pub limits: Limits,
    /// Limit dimensions still carrying per-worker defaults. See [`ScaledLimits`].
    pub scaled_limits: ScaledLimits,
    /// Per-cache, per-worker origin HTTP cap, separate from peer connections.
    pub origin_connections_per_cache: NonZeroUsize,
    pub request_timeout: Duration,
    /// Nonrenewable local peer-exchange cap; never serialized as authority.
    pub peer_attempt_timeout: Duration,
    pub reader_stall_timeout: Duration,
    pub shutdown_timeout: Duration,
}

impl Config {
    /// Parse and validate startup configuration from the process environment.
    pub fn from_env() -> Result<Self> {
        Self::from_lookup(env_value)
    }

    /// Read only exact supported names, without enumerating unrelated environment values.
    pub fn from_lookup(mut lookup: impl FnMut(&str) -> Result<Option<String>>) -> Result<Self> {
        let mut values = Vec::new();
        for name in ENV_NAMES {
            if let Some(value) = lookup(&format!("RACER_{name}"))? {
                if *name != "SEND_CRC_PAIR" {
                    validate_text(&value, 4096)?;
                }
                values.push(((*name).to_owned(), value));
            }
        }
        let raw: RawConfig = envy::from_iter(values).map_err(|_| Error::InvalidConfiguration)?;
        // Auto reserves native resources only with usable startup hardware.
        // Explicit true reserves them even when absent, enabling hotplug recovery.
        let enable_rdma = match raw.enable_rdma.as_deref().unwrap_or("auto") {
            "auto" => {
                cfg!(feature = "rdma")
                    && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
            }
            "true" => true,
            "false" => false,
            _ => return Err(Error::InvalidConfiguration),
        };
        let mut config = Self {
            warm: crate::read::warm::Config {
                max_ttl: Duration::from_secs(raw.warm_max_ttl_secs.unwrap_or(3600)),
                fill_timeout: Duration::from_secs(raw.warm_fill_timeout_secs.unwrap_or(600)),
                max_jobs: to_usize(raw.warm_max_jobs.unwrap_or(16))?,
                window: to_usize(raw.warm_window_pages.unwrap_or(2))?,
                retained_percent: to_usize(raw.retained_segment_percent.unwrap_or(50))?,
            },
            send_crc_pair: raw
                .send_crc_pair
                .map(|value| crate::telemetry::Pair::parse(&value))
                .transpose()?,
            page_hedge: crate::read::candidates::HedgeConfig {
                delay: Duration::from_millis(raw.page_hedge_delay_ms.unwrap_or(100)),
                slots: to_usize(raw.page_hedge_slots.unwrap_or(0))?,
                bytes: to_usize(
                    raw.page_hedge_bytes
                        .unwrap_or(crate::read::candidates::DUPLICATE_BYTES as u64),
                )?,
            },
            replica_selection: match raw.replica_selection.as_deref().unwrap_or("spread") {
                "primary" => crate::read::candidates::ReplicaSelection::Primary,
                "spread" => crate::read::candidates::ReplicaSelection::Spread,
                _ => return Err(Error::InvalidConfiguration),
            },
            peer_admission: crate::peer::Config {
                total: to_usize(raw.peer_inflight_max.unwrap_or(256))?,
                per_peer: to_usize(raw.peer_per_neighbor_max.unwrap_or(32))?,
            },
            peer_receive: crate::peer::receive::Config {
                active: to_usize(raw.peer_receive_max.unwrap_or(0))?,
                queued: to_usize(raw.peer_receive_queue_max.unwrap_or(256))?,
                wait: Duration::from_millis(raw.peer_receive_wait_ms.unwrap_or(1000)),
            },
            shares: nonzero_u32(raw.shares.unwrap_or(u64::from(DEFAULT_SHARES)))?,
            disk_page_entries: nonzero_usize(raw.disk_page_entries.unwrap_or(65536))?,
            admission_mode: match raw.admission_mode.as_deref().unwrap_or("second-sight") {
                "disabled" => AdmissionMode::Disabled,
                "second-sight" => AdmissionMode::SecondSight,
                _ => return Err(Error::InvalidConfiguration),
            },
            admission_history_bytes: nonzero_usize(raw.admission_history_bytes.unwrap_or(4 * MIB))?,
            admission_period: Duration::from_secs(raw.admission_period_secs.unwrap_or(60)),
            checkpoint_bytes: nonzero_usize(raw.checkpoint_bytes.unwrap_or(64 * MIB))?,
            cluster: ClusterId(raw.cluster_id),
            node: NodeId(UNRESOLVED_NODE_ID.into()),
            max_threads: to_usize(raw.max_threads.unwrap_or(DEFAULT_MAX_THREADS as u64))?,
            io_workers: raw.io_workers.map(to_usize).transpose()?,
            crypto_helpers: raw.crypto_helpers.map(to_usize).transpose()?,
            allow_smt: raw.allow_smt.unwrap_or(false),
            enable_rdma,
            opaque_relay: raw.opaque_relay.unwrap_or(false),
            peer_tcp_nodelay: raw.peer_tcp_nodelay.unwrap_or(true),
            rdma_zero_copy: raw.rdma_zero_copy.unwrap_or(true),
            memory_pressure: raw.memory_pressure.unwrap_or(true),
            pprof_enabled: raw.pprof_enabled.unwrap_or(false),
            control_endpoint: raw.control_endpoint,
            peer_listen: parse_listener_address(
                raw.peer_listen.as_deref().unwrap_or("0.0.0.0:7443"),
            )?,
            diagnostics_listen: parse_listener_address(
                raw.diagnostics_listen
                    .as_deref()
                    .unwrap_or("127.0.0.1:9090"),
            )?,
            trust_bundle: raw
                .trust_bundle
                .unwrap_or_else(|| "/etc/racer/trust/ca.crt".into())
                .into(),
            service_account_token: raw
                .service_account_token
                .unwrap_or_else(|| "/var/run/secrets/racer-control/token".into())
                .into(),
            identity_directory: raw
                .identity_directory
                .unwrap_or_else(|| "/var/lib/racer/identity".into())
                .into(),
            slab_directory: raw
                .slab_directory
                .unwrap_or_else(|| "/var/lib/racer/slabs".into())
                .into(),
            device_directory: raw
                .device_directory
                .unwrap_or_else(|| "/host/dev".into())
                .into(),
            device_busy_wait: Duration::from_secs(raw.device_busy_wait_secs.unwrap_or(120)),
            slab_bytes: raw.slab_bytes.unwrap_or(16 * 1024 * MIB),
            segment_bytes: raw.segment_bytes.unwrap_or(1024 * MIB),
            free_segment_reserve: to_usize(raw.free_segment_reserve.unwrap_or(2))?,
            limits: Limits {
                // Byte and count dimensions below are divided by the I/O worker
                // count during sizing, so their defaults are stated per worker
                // and multiplied back out by `scale_limits` once the planner
                // resolves that count. A page is 16 MiB.
                plaintext_bytes: nonzero_usize(raw.plaintext_bytes.unwrap_or(64 * MIB))?,
                ciphertext_bytes: nonzero_usize(raw.ciphertext_bytes.unwrap_or(128 * MIB))?,
                dirty_bytes: nonzero_usize(raw.dirty_bytes.unwrap_or(160 * MIB))?,
                registered_bytes: nonzero_usize(raw.registered_bytes.unwrap_or(96 * MIB))?,
                request_context_bytes: nonzero_usize(
                    raw.request_context_bytes.unwrap_or(32 * MIB),
                )?,
                flights: nonzero_usize(raw.flights.unwrap_or(64))?,
                waiters_per_flight: nonzero_usize(raw.waiters_per_flight.unwrap_or(64))?,
                queue_entries: nonzero_usize(raw.queue_entries.unwrap_or(256))?,
                connections_per_neighbor: nonzero_usize(raw.connections_per_neighbor.unwrap_or(2))?,
                client_connections: nonzero_usize(raw.client_connections.unwrap_or(64))?,
                pipes: nonzero_usize(raw.pipes.unwrap_or(16))?,
                range_window_pages: nonzero_usize(raw.range_window_pages.unwrap_or(2))?,
                header_bytes: nonzero_usize(raw.header_bytes.unwrap_or(32 * 1024))?,
                placement_cache_bytes: nonzero_usize(
                    raw.placement_cache_bytes.unwrap_or(16 * MIB),
                )?,
                cached_paths: nonzero_usize(raw.cached_paths.unwrap_or(128))?,
                path_cache_bytes: nonzero_usize(raw.path_cache_bytes.unwrap_or(8 * MIB))?,
                active_path_searches: nonzero_usize(raw.active_path_searches.unwrap_or(8))?,
                retained_snapshots: nonzero_usize(raw.retained_snapshots.unwrap_or(2))?,
                metadata_entries: nonzero_usize(raw.metadata_entries.unwrap_or(4096))?,
                relay_transfers: nonzero_usize(raw.relay_transfers.unwrap_or(16))?,
            },
            scaled_limits: ScaledLimits {
                plaintext_bytes: raw.plaintext_bytes.is_none(),
                ciphertext_bytes: raw.ciphertext_bytes.is_none(),
                dirty_bytes: raw.dirty_bytes.is_none(),
                registered_bytes: raw.registered_bytes.is_none(),
                request_context_bytes: raw.request_context_bytes.is_none(),
                flights: raw.flights.is_none(),
                queue_entries: raw.queue_entries.is_none(),
                client_connections: raw.client_connections.is_none(),
                pipes: raw.pipes.is_none(),
                placement_cache_bytes: raw.placement_cache_bytes.is_none(),
                cached_paths: raw.cached_paths.is_none(),
                path_cache_bytes: raw.path_cache_bytes.is_none(),
                metadata_entries: raw.metadata_entries.is_none(),
                relay_transfers: raw.relay_transfers.is_none(),
            },
            origin_connections_per_cache: nonzero_usize(
                raw.origin_connections_per_cache
                    .unwrap_or(DEFAULT_ORIGIN_CONNECTIONS_PER_CACHE as u64),
            )?,
            request_timeout: Duration::from_millis(raw.request_timeout_ms.unwrap_or(30_000)),
            peer_attempt_timeout: Duration::from_millis(
                raw.peer_attempt_timeout_ms.unwrap_or(30_000),
            ),
            reader_stall_timeout: Duration::from_millis(
                raw.reader_stall_timeout_ms.unwrap_or(10_000),
            ),
            shutdown_timeout: Duration::from_millis(raw.shutdown_timeout_ms.unwrap_or(30_000)),
        };
        if raw.origin_connections_per_cache.is_none() {
            // The default must not reject a smaller explicit client budget.
            config.origin_connections_per_cache = config
                .origin_connections_per_cache
                .min(config.limits.client_connections);
        }
        config.validate()?;
        Ok(config)
    }

    /// Multiply every defaulted limit dimension by the planned I/O worker count.
    ///
    /// Worker sizing partitions node-wide limits across lanes, so a default
    /// stated per worker has to be scaled back up before that division or each
    /// lane receives only a fraction of its intended budget. Explicitly
    /// configured dimensions are node-wide by definition and are left alone.
    ///
    /// A second call is a no-op: the scaling record is consumed, but only once
    /// every dimension has scaled successfully. Scaling is computed against a
    /// copy and committed whole, so a rejected worker count or an overflow in
    /// any one dimension leaves both the limits and the record untouched.
    pub fn scale_limits(&mut self, io_workers: usize) -> Result<()> {
        let scaled = self.scaled_limits;
        let workers = NonZeroUsize::new(io_workers).ok_or(Error::InvalidConfiguration)?;
        let scale = |value: &mut NonZeroUsize, apply: bool| -> Result<()> {
            if apply {
                *value = value
                    .checked_mul(workers)
                    .ok_or(Error::InvalidConfiguration)?;
            }
            Ok(())
        };
        let mut scratch = self.limits.clone();
        let limits = &mut scratch;
        scale(&mut limits.plaintext_bytes, scaled.plaintext_bytes)?;
        scale(&mut limits.ciphertext_bytes, scaled.ciphertext_bytes)?;
        scale(&mut limits.dirty_bytes, scaled.dirty_bytes)?;
        scale(&mut limits.registered_bytes, scaled.registered_bytes)?;
        scale(
            &mut limits.request_context_bytes,
            scaled.request_context_bytes,
        )?;
        scale(&mut limits.flights, scaled.flights)?;
        scale(&mut limits.queue_entries, scaled.queue_entries)?;
        scale(&mut limits.client_connections, scaled.client_connections)?;
        scale(&mut limits.pipes, scaled.pipes)?;
        scale(
            &mut limits.placement_cache_bytes,
            scaled.placement_cache_bytes,
        )?;
        scale(&mut limits.cached_paths, scaled.cached_paths)?;
        scale(&mut limits.path_cache_bytes, scaled.path_cache_bytes)?;
        scale(&mut limits.metadata_entries, scaled.metadata_entries)?;
        scale(&mut limits.relay_transfers, scaled.relay_transfers)?;
        self.limits = scratch;
        self.scaled_limits = ScaledLimits::default();
        Ok(())
    }

    /// Check arithmetic and progress reserves; filesystem alignment is additionally
    /// discovered and checked when opening the slabs, not guessed from this config.
    pub fn validate(&self) -> Result<()> {
        match (self.io_workers, self.crypto_helpers) {
            (None, None) => {}
            (Some(io), Some(helpers))
                if io > 0
                    && io <= usize::from(u16::MAX) + 1
                    && helpers > 0
                    && helpers <= io
                    && io
                        .checked_add(helpers)
                        .is_some_and(|n| n <= self.max_threads) => {}
            _ => return Err(Error::InvalidConfiguration),
        }
        self.warm.validate()?;
        if let Some(pair) = &self.send_crc_pair {
            pair.validate()?;
        }
        self.page_hedge.validate()?;
        self.peer_receive.validate()?;
        self.peer_admission.validate()?;
        if self.disk_page_entries.get() > MAX_ENTRIES
            || self.checkpoint_bytes.get() > 512 * MIB as usize
            || !(32..=512 * MIB as usize).contains(&self.admission_history_bytes.get())
            || self.admission_period < Duration::from_secs(1)
            || self.admission_period > Duration::from_secs(86_400)
            || self.device_busy_wait > Duration::from_secs(3_600)
        {
            return Err(Error::InvalidConfiguration);
        }
        if !valid_uuid(&self.cluster.0)
            || (self.node.0 != UNRESOLVED_NODE_ID && !valid_uuid(&self.node.0))
            || self.max_threads < 2
        {
            return Err(Error::InvalidConfiguration);
        }
        validate_endpoint(&self.control_endpoint)?;
        for address in [self.peer_listen, self.diagnostics_listen] {
            validate_socket(address)?;
        }
        if self.peer_listen.port() == self.diagnostics_listen.port()
            && (self.peer_listen.ip() == self.diagnostics_listen.ip()
                || self.peer_listen.ip().is_unspecified()
                || self.diagnostics_listen.ip().is_unspecified())
        {
            return Err(Error::InvalidConfiguration);
        }
        let paths = [
            &self.trust_bundle,
            &self.service_account_token,
            &self.identity_directory,
            &self.slab_directory,
            &self.device_directory,
        ];
        for path in paths {
            validate_path(path)?;
        }
        // Writable state must not contain or live inside projected credentials.
        // Lexical checks cannot prove mounts/symlinks are distinct; open-time checks
        // remain mandatory and must allow Kubernetes projection symlinks.
        for (index, left) in paths.iter().enumerate() {
            for right in &paths[index + 1..] {
                if left.starts_with(right) || right.starts_with(left) {
                    return Err(Error::InvalidConfiguration);
                }
            }
        }
        let record_bytes = PAGE_BYTES
            .checked_add(16)
            .and_then(|n| n.checked_add(MAX_HEADER_BYTES as u64))
            .ok_or(Error::InvalidConfiguration)?;
        if self.segment_bytes < record_bytes
            || self.slab_bytes > i64::MAX as u64
            || self.slab_bytes == 0
            || !self.slab_bytes.is_multiple_of(self.segment_bytes)
            || self.free_segment_reserve == 0
        {
            return Err(Error::InvalidConfiguration);
        }
        let segments = to_usize(self.slab_bytes / self.segment_bytes)?;
        if segments > MAX_ENTRIES || self.free_segment_reserve >= segments {
            return Err(Error::InvalidConfiguration);
        }
        let limits = &self.limits;
        if self.origin_connections_per_cache.get() > 1024
            || self.origin_connections_per_cache > limits.client_connections
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut bytes = 0u64;
        for limit in [
            limits.plaintext_bytes,
            limits.ciphertext_bytes,
            limits.dirty_bytes,
            limits.registered_bytes,
            limits.request_context_bytes,
        ] {
            let value = limit.get() as u64;
            if value > MAX_BYTES || limit.get() > isize::MAX as usize {
                return Err(Error::InvalidConfiguration);
            }
            bytes = bytes
                .checked_add(value)
                .ok_or(Error::InvalidConfiguration)?;
        }
        // Node-wide budgets are partitioned after affinity discovery. Integration
        // must recheck progress floors per I/O shard and reduce workers if necessary.
        if bytes > 256 * 1024 * MIB || bytes > isize::MAX as u64 {
            return Err(Error::InvalidConfiguration);
        }
        for (limit, maximum) in [
            (limits.flights, 65_536),
            (limits.waiters_per_flight, 4096),
            (limits.queue_entries, 65_536),
            (limits.connections_per_neighbor, 1024),
            (limits.client_connections, 65_536),
            (limits.pipes, 65_536),
            (limits.range_window_pages, 64),
            (limits.header_bytes, 32 * 1024),
            (limits.placement_cache_bytes, 512 * MIB as usize),
            (limits.cached_paths, MAX_ENTRIES),
            (limits.path_cache_bytes, 512 * MIB as usize),
            (limits.active_path_searches, 64),
            (limits.retained_snapshots, 64),
            (limits.metadata_entries, MAX_ENTRIES),
            (limits.relay_transfers, 65_536),
        ] {
            if limit.get() > maximum {
                return Err(Error::InvalidConfiguration);
            }
        }
        // Keep one page of acquisition progress beyond a full range window. The
        // ciphertext margin also fits one maximum unpadded storage record.
        let window = limits.range_window_pages.get() as u64;
        let plaintext = (window + 1)
            .checked_mul(PAGE_BYTES)
            .ok_or(Error::InvalidConfiguration)?;
        let ciphertext = window
            .checked_mul(PAGE_BYTES + 16)
            .and_then(|n| n.checked_add(record_bytes))
            .ok_or(Error::InvalidConfiguration)?;
        if (limits.plaintext_bytes.get() as u64) < plaintext
            || (limits.ciphertext_bytes.get() as u64) < ciphertext
            || (limits.dirty_bytes.get() as u64) < PAGE_BYTES + 16
            || (self.enable_rdma && (limits.registered_bytes.get() as u64) < PAGE_BYTES + 16)
            || limits.header_bytes.get() < 1024
            || limits.request_context_bytes.get() < 128 * 1024
            || limits.queue_entries.get() < 2
            || limits.retained_snapshots.get() < 2
            || limits.placement_cache_bytes.get() < crate::topology::RANKING_BYTES
            || limits.connections_per_neighbor > limits.client_connections
            || limits
                .flights
                .get()
                .checked_mul(limits.waiters_per_flight.get())
                .is_none_or(|n| n > MAX_ENTRIES)
        {
            return Err(Error::InvalidConfiguration);
        }
        for (timeout, maximum) in [
            (self.request_timeout, Duration::from_secs(86_400)),
            (self.peer_attempt_timeout, Duration::from_secs(86_400)),
            (self.reader_stall_timeout, self.request_timeout),
            (self.shutdown_timeout, Duration::from_secs(3600)),
        ] {
            if timeout < Duration::from_millis(1) || timeout > maximum {
                return Err(Error::InvalidConfiguration);
            }
        }
        Ok(())
    }

    /// Never round a small share up past the node budget. Retention rounds the
    /// allocation down to whole 32-byte Bloom units after this partition.
    pub(crate) fn admission_history_per_worker(&self, workers: usize) -> Result<usize> {
        self.admission_history_bytes
            .get()
            .checked_div(workers)
            .filter(|bytes| *bytes >= 32)
            .ok_or(Error::InvalidConfiguration)
    }
}

/// Environment names are exact and case sensitive, even though envy folds keys.
const ENV_NAMES: &[&str] = &[
    "CLUSTER_ID",
    "CONTROL_ENDPOINT",
    "ENABLE_RDMA",
    "ALLOW_SMT",
    "OPAQUE_RELAY",
    "PEER_TCP_NODELAY",
    "RDMA_ZERO_COPY",
    "MEMORY_PRESSURE",
    "PPROF_ENABLED",
    "PEER_LISTEN",
    "DIAGNOSTICS_LISTEN",
    "TRUST_BUNDLE",
    "SERVICE_ACCOUNT_TOKEN",
    "IDENTITY_DIRECTORY",
    "SLAB_DIRECTORY",
    "DEVICE_DIRECTORY",
    "DEVICE_BUSY_WAIT_SECS",
    "ADMISSION_MODE",
    "MAX_THREADS",
    "IO_WORKERS",
    "CRYPTO_HELPERS",
    "WARM_MAX_TTL_SECS",
    "WARM_FILL_TIMEOUT_SECS",
    "WARM_MAX_JOBS",
    "WARM_WINDOW_PAGES",
    "RETAINED_SEGMENT_PERCENT",
    "PAGE_HEDGE_DELAY_MS",
    "PAGE_HEDGE_SLOTS",
    "PAGE_HEDGE_BYTES",
    "REPLICA_SELECTION",
    "PEER_INFLIGHT_MAX",
    "PEER_PER_NEIGHBOR_MAX",
    "SHARES",
    "PEER_RECEIVE_MAX",
    "PEER_RECEIVE_QUEUE_MAX",
    "PEER_RECEIVE_WAIT_MS",
    "SLAB_BYTES",
    "SEGMENT_BYTES",
    "FREE_SEGMENT_RESERVE",
    "REQUEST_TIMEOUT_MS",
    "PEER_ATTEMPT_TIMEOUT_MS",
    "READER_STALL_TIMEOUT_MS",
    "SHUTDOWN_TIMEOUT_MS",
    "ADMISSION_PERIOD_SECS",
    "PLAINTEXT_BYTES",
    "CIPHERTEXT_BYTES",
    "DIRTY_BYTES",
    "REGISTERED_BYTES",
    "REQUEST_CONTEXT_BYTES",
    "FLIGHTS",
    "WAITERS_PER_FLIGHT",
    "QUEUE_ENTRIES",
    "CONNECTIONS_PER_NEIGHBOR",
    "CLIENT_CONNECTIONS",
    "PIPES",
    "RANGE_WINDOW_PAGES",
    "HEADER_BYTES",
    "PLACEMENT_CACHE_BYTES",
    "CACHED_PATHS",
    "PATH_CACHE_BYTES",
    "ACTIVE_PATH_SEARCHES",
    "RETAINED_SNAPSHOTS",
    "METADATA_ENTRIES",
    "RELAY_TRANSFERS",
    "ORIGIN_CONNECTIONS_PER_CACHE",
    "DISK_PAGE_ENTRIES",
    "ADMISSION_HISTORY_BYTES",
    "CHECKPOINT_BYTES",
    "SEND_CRC_PAIR",
];

/// Parsed values before application defaults, conversions, and cross-field policy.
#[derive(serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct RawConfig {
    cluster_id: String,
    control_endpoint: String,
    enable_rdma: Option<String>,
    allow_smt: Option<bool>,
    opaque_relay: Option<bool>,
    peer_tcp_nodelay: Option<bool>,
    rdma_zero_copy: Option<bool>,
    memory_pressure: Option<bool>,
    pprof_enabled: Option<bool>,
    peer_listen: Option<String>,
    diagnostics_listen: Option<String>,
    trust_bundle: Option<String>,
    service_account_token: Option<String>,
    identity_directory: Option<String>,
    slab_directory: Option<String>,
    device_directory: Option<String>,
    admission_mode: Option<String>,
    send_crc_pair: Option<String>,
    #[serde(default, deserialize_with = "decimal")]
    max_threads: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    io_workers: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    crypto_helpers: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    warm_max_ttl_secs: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    warm_fill_timeout_secs: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    warm_max_jobs: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    warm_window_pages: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    retained_segment_percent: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    page_hedge_delay_ms: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    page_hedge_slots: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    page_hedge_bytes: Option<u64>,
    replica_selection: Option<String>,
    #[serde(default, deserialize_with = "decimal")]
    peer_inflight_max: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    peer_per_neighbor_max: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    shares: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    peer_receive_max: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    peer_receive_queue_max: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    peer_receive_wait_ms: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    slab_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    segment_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    free_segment_reserve: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    request_timeout_ms: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    peer_attempt_timeout_ms: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    reader_stall_timeout_ms: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    shutdown_timeout_ms: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    admission_period_secs: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    device_busy_wait_secs: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    plaintext_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    ciphertext_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    dirty_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    registered_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    request_context_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    flights: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    waiters_per_flight: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    queue_entries: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    connections_per_neighbor: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    client_connections: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    pipes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    range_window_pages: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    header_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    placement_cache_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    cached_paths: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    path_cache_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    active_path_searches: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    retained_snapshots: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    metadata_entries: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    relay_transfers: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    origin_connections_per_cache: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    disk_page_entries: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    admission_history_bytes: Option<u64>,
    #[serde(default, deserialize_with = "decimal")]
    checkpoint_bytes: Option<u64>,
}

/// Keep decimal syntax stricter than Rust's unsigned FromStr, which accepts '+'.
fn decimal<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> std::result::Result<Option<u64>, D::Error> {
    let value = <String as serde::Deserialize>::deserialize(deserializer)?;
    unsigned(&value).map(Some).map_err(serde::de::Error::custom)
}

/// Read one Unicode variable, preserving the distinction between missing and invalid.
fn env_value(name: &str) -> Result<Option<String>> {
    lookup_result(std::env::var(name))
}

/// Missing values may use defaults; non-Unicode values must fail.
fn lookup_result(value: std::result::Result<String, std::env::VarError>) -> Result<Option<String>> {
    match value {
        Ok(value) => Ok(Some(value)),
        Err(std::env::VarError::NotPresent) => Ok(None),
        Err(std::env::VarError::NotUnicode(_)) => Err(Error::InvalidConfiguration),
    }
}

/// Reject empty, oversized, or control-bearing text without trimming it.
fn validate_text(value: &str, maximum: usize) -> Result<()> {
    if value.is_empty() || value.len() > maximum || value.chars().any(char::is_control) {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

/// Accept only ASCII decimal digits within the full u64 range.
fn unsigned(value: &str) -> Result<u64> {
    if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
        return Err(Error::InvalidConfiguration);
    }
    value.parse().map_err(|_| Error::InvalidConfiguration)
}

/// Convert a size without truncating it on smaller platforms.
fn to_usize(value: u64) -> Result<usize> {
    usize::try_from(value).map_err(|_| Error::InvalidConfiguration)
}

/// Convert a positive size without truncation.
fn nonzero_usize(value: u64) -> Result<NonZeroUsize> {
    NonZeroUsize::new(to_usize(value)?).ok_or(Error::InvalidConfiguration)
}

/// Convert a positive share count without truncation.
fn nonzero_u32(value: u64) -> Result<std::num::NonZeroU32> {
    std::num::NonZeroU32::new(u32::try_from(value).map_err(|_| Error::InvalidConfiguration)?)
        .ok_or(Error::InvalidConfiguration)
}

fn valid_uuid(value: &str) -> bool {
    racer_control_wire::valid_uuid(value) && value != "00000000-0000-0000-0000-000000000000"
}

// Kubernetes expands a single bracketed Pod IP template for both IP families.
// SocketAddr already accepts bracketed IPv6; normalize only bracketed IPv4 here.
fn parse_listener_address(value: &str) -> Result<SocketAddr> {
    if let Some((host, port)) = value.strip_prefix('[').and_then(|v| v.split_once("]:"))
        && let Ok(ip) = host.parse::<std::net::Ipv4Addr>()
    {
        return socket_address(&format!("{ip}:{port}"));
    }
    socket_address(value)
}

/// Parse a numeric socket address without DNS lookup.
fn socket_address(value: &str) -> Result<SocketAddr> {
    value.parse().map_err(|_| Error::InvalidConfiguration)
}

/// Require a usable port and reject multicast, broadcast, scoped, or mapped addresses.
fn validate_socket(address: SocketAddr) -> Result<()> {
    if address.port() == 0
        || address.ip().is_multicast()
        || matches!(address.ip(), IpAddr::V4(ip) if ip.is_broadcast())
        || matches!(address, SocketAddr::V6(ip) if ip.flowinfo() != 0 || ip.scope_id() != 0 || ip.ip().to_ipv4_mapped().is_some())
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

fn validate_endpoint(url: &str) -> Result<()> {
    let authority = url
        .strip_prefix("https://")
        .ok_or(Error::InvalidConfiguration)?;
    let authority = authority.strip_suffix('/').unwrap_or(authority);
    if authority.is_empty()
        || authority.len() > 320
        || authority
            .bytes()
            .any(|b| !b.is_ascii_graphic() || b"/?#@\\%".contains(&b))
    {
        return Err(Error::InvalidConfiguration);
    }
    let (host, port) = if let Some(rest) = authority.strip_prefix('[') {
        let (host, rest) = rest.split_once(']').ok_or(Error::InvalidConfiguration)?;
        host.parse::<std::net::Ipv6Addr>()
            .map_err(|_| Error::InvalidConfiguration)?;
        (
            host,
            if rest.is_empty() {
                None
            } else {
                Some(rest.strip_prefix(':').ok_or(Error::InvalidConfiguration)?)
            },
        )
    } else {
        let (host, port) = authority
            .split_once(':')
            .map_or((authority, None), |(h, p)| (h, Some(p)));
        if host.len() > 253
            || host.split('.').any(|label| {
                label.is_empty()
                    || label.len() > 63
                    || label.starts_with('-')
                    || label.ends_with('-')
                    || !label
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b == b'-')
            })
        {
            return Err(Error::InvalidConfiguration);
        }
        (host, port)
    };
    if let Some(port) = port
        && (port.is_empty()
            || !port.bytes().all(|b| b.is_ascii_digit())
            || port.parse::<u16>().ok().is_none_or(|p| p == 0))
    {
        return Err(Error::InvalidConfiguration);
    }
    rustls::pki_types::ServerName::try_from(host).map_err(|_| Error::InvalidConfiguration)?;
    if let Ok(ip) = host.parse::<IpAddr>() {
        validate_socket(SocketAddr::new(ip, 443))?;
        if ip.is_unspecified() {
            return Err(Error::InvalidConfiguration);
        }
    }
    Ok(())
}

fn validate_path(path: &Path) -> Result<()> {
    absolute_path(path, 4095, 255)
}

/// Check lexical path bounds without accessing mounts or resolving symlinks.
fn absolute_path(path: &Path, maximum: usize, component_maximum: usize) -> Result<()> {
    let text = path.to_str().ok_or(Error::InvalidConfiguration)?;
    if !path.is_absolute()
        || text.len() > maximum
        || text.chars().any(char::is_control)
        || text[1..].split('/').any(|part| {
            part.is_empty() || matches!(part, "." | "..") || part.len() > component_maximum
        })
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Read;

    #[test]
    fn every_allowlisted_name_is_a_raw_config_field() {
        let values: Vec<_> = ENV_NAMES
            .iter()
            .map(|name| {
                let value = match *name {
                    "ALLOW_SMT" | "OPAQUE_RELAY" | "PEER_TCP_NODELAY" | "RDMA_ZERO_COPY"
                    | "MEMORY_PRESSURE" | "PPROF_ENABLED" => "true",
                    _ => "1",
                };
                ((*name).to_owned(), value.to_owned())
            })
            .collect();
        // Values need only satisfy raw types; application policy is checked separately.
        assert!(envy::from_iter::<_, RawConfig>(values.clone()).is_ok());
        let mut typo = values;
        typo.push(("MAX_THREAD".into(), "1".into()));
        assert!(envy::from_iter::<_, RawConfig>(typo).is_err());
    }

    #[test]
    fn lookup_distinguishes_absence_empty_and_nonunicode() {
        assert_eq!(lookup_result(Ok(String::new())), Ok(Some(String::new())));
        assert_eq!(lookup_result(Ok("value".into())), Ok(Some("value".into())));
        assert_eq!(lookup_result(Err(std::env::VarError::NotPresent)), Ok(None));
        #[cfg(unix)]
        {
            use std::os::unix::ffi::OsStringExt;
            let value = std::ffi::OsString::from_vec(vec![0xff]);
            assert_eq!(
                lookup_result(Err(std::env::VarError::NotUnicode(value))),
                Err(Error::InvalidConfiguration)
            );
        }
    }

    #[test]
    fn exact_worker_counts_require_a_valid_pair_within_the_cap() {
        let defaults = parse(&[]).unwrap();
        assert_eq!((defaults.io_workers, defaults.crypto_helpers), (None, None));
        for (io, helpers, cap, valid) in [
            ("20", "10", "30", true),
            ("20", "20", "40", true),
            ("20", "20", "30", false),
            ("0", "1", "40", false),
            ("20", "0", "40", false),
            ("10", "20", "40", false),
            ("65537", "1", "99999", false),
            ("-1", "1", "40", false),
            ("18446744073709551615", "1", "40", false),
        ] {
            assert_eq!(
                parse(&[
                    ("RACER_IO_WORKERS", io),
                    ("RACER_CRYPTO_HELPERS", helpers),
                    ("RACER_MAX_THREADS", cap)
                ])
                .is_ok(),
                valid
            );
        }
        assert!(parse(&[("RACER_IO_WORKERS", "20")]).is_err());
        assert!(parse(&[("RACER_CRYPTO_HELPERS", "20")]).is_err());
    }

    #[test]
    fn text_defaults_and_strict_values() {
        assert!(validate_text("ok", 2).is_ok());
        assert!(validate_text("é", 2).is_ok());
        assert!(validate_text("é", 1).is_err());
        assert!(Config::from_lookup(|_| Ok(None)).is_err());
        assert_eq!(parse(&[]).unwrap().max_threads, DEFAULT_MAX_THREADS);
        for value in ["", "a\n", "\0", "\u{85}", "abc"] {
            assert!(validate_text(value, 2).is_err());
        }
        assert!(parse(&[("RACER_ALLOW_SMT", "true")]).unwrap().allow_smt);
        assert!(!parse(&[("RACER_ALLOW_SMT", "false")]).unwrap().allow_smt);
        for value in ["", "TRUE", "1", " false", "false\n"] {
            assert!(parse(&[("RACER_ALLOW_SMT", value)]).is_err());
        }
        assert_eq!(unsigned("000"), Ok(0));
        assert_eq!(unsigned("18446744073709551615"), Ok(u64::MAX));
        for value in [
            "",
            "+1",
            "-1",
            " 1",
            "1 ",
            "1\n",
            "１",
            "18446744073709551616",
        ] {
            assert!(unsigned(value).is_err());
        }
        assert!(nonzero_u32(0).is_err());
        assert!(nonzero_u32(u32::MAX as u64 + 1).is_err());
        assert_eq!(nonzero_u32(u32::MAX as u64).unwrap().get(), u32::MAX);
        assert!(nonzero_usize(0).is_err());
        assert_eq!(nonzero_usize(usize::MAX as u64).unwrap().get(), usize::MAX);
    }

    #[test]
    fn lexical_path_limits() {
        assert!(absolute_path(Path::new("/ab/c"), 5, 2).is_ok());
        assert!(absolute_path(Path::new("/ab/c"), 4, 2).is_err());
        assert!(absolute_path(Path::new("/ab/c"), 5, 1).is_err());
        for path in [
            "", "/", "relative", "/a/", "/a//b", "/a/./b", "/a/../b", "/a\0", "/a\n",
        ] {
            assert!(absolute_path(Path::new(path), 4095, 255).is_err());
        }
        #[cfg(unix)]
        {
            use std::os::unix::ffi::OsStrExt;
            assert!(
                absolute_path(Path::new(std::ffi::OsStr::from_bytes(b"/\xff")), 4095, 255).is_err()
            );
        }
    }

    #[test]
    fn numeric_socket_profile() {
        for value in ["0.0.0.0:1", "[::]:1", "127.0.0.1:65535", "[::1]:1"] {
            validate_socket(socket_address(value).unwrap()).unwrap();
        }
        for value in [
            "localhost:1",
            "[127.0.0.1]:1",
            "127.0.0.1:+1",
            "127.0.0.1:65536",
        ] {
            assert!(socket_address(value).is_err());
        }
        for value in [
            "127.0.0.1:0",
            "224.0.0.1:1",
            "255.255.255.255:1",
            "[ff02::1]:1",
            "[::ffff:127.0.0.1]:1",
            "[fe80::1%2]:1",
        ] {
            assert!(validate_socket(socket_address(value).unwrap()).is_err());
        }
        assert!(
            validate_socket(SocketAddr::V6(std::net::SocketAddrV6::new(
                std::net::Ipv6Addr::LOCALHOST,
                1,
                1,
                0
            )))
            .is_err()
        );
    }

    #[test]
    fn envy_preserves_full_decimal_range_and_text_bytes() {
        let raw: RawConfig = envy::from_iter([
            ("CLUSTER_ID".into(), "unused".into()),
            ("CONTROL_ENDPOINT".into(), "unused".into()),
            ("MAX_THREADS".into(), u64::MAX.to_string()),
        ])
        .unwrap();
        assert_eq!(raw.max_threads, Some(u64::MAX));
        assert_eq!(
            parse(&[("RACER_MAX_THREADS", "0004")]).unwrap().max_threads,
            4
        );
        assert_eq!(
            parse(&[("RACER_MAX_THREADS", &format!("{}4", "0".repeat(4095)))])
                .unwrap()
                .max_threads,
            4
        );
        assert!(parse(&[("RACER_MAX_THREADS", &format!("{}4", "0".repeat(4096)))]).is_err());
        assert_eq!(
            parse(&[("RACER_DEVICE_DIRECTORY", "/host/é space")])
                .unwrap()
                .device_directory,
            Path::new("/host/é space")
        );
    }

    #[cfg(unix)]
    #[test]
    fn process_environment_uses_only_exact_supported_unicode_names() {
        use std::os::unix::ffi::OsStringExt;
        const CHILD: &str = "RACER_CONFIG_TEST_CHILD";
        if let Ok(expected) = std::env::var(CHILD) {
            let result = Config::from_env();
            if expected == "valid" {
                let config = result.unwrap();
                assert_eq!(config.max_threads, 7);
                assert!(!config.allow_smt);
            } else {
                assert!(matches!(result, Err(Error::InvalidConfiguration)));
            }
            return;
        }
        // Child-only environment changes avoid races with other tests.
        for (name, value, expected) in [
            (
                "UNRELATED",
                std::ffi::OsString::from_vec(vec![0xff]),
                "valid",
            ),
            (
                "racer_allow_smt",
                std::ffi::OsString::from_vec(vec![0xff]),
                "valid",
            ),
            (
                "RACER_ALLOW_SMT",
                std::ffi::OsString::from_vec(vec![0xff]),
                "invalid",
            ),
            (
                "RACER_SHARES",
                std::ffi::OsString::from_vec(vec![0xff]),
                "invalid",
            ),
            ("RACER_SHARES", "".into(), "invalid"),
            ("RACER_ALLOW_SMT", "".into(), "invalid"),
        ] {
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .arg("--exact")
                .arg("config::tests::process_environment_uses_only_exact_supported_unicode_names")
                .env_clear()
                .env(CHILD, expected)
                .env("RACER_CLUSTER_ID", "00000000-0000-4000-8000-000000000001")
                .env("RACER_CONTROL_ENDPOINT", "https://control.example")
                .env("RACER_ENABLE_RDMA", "false")
                .env("RACER_MAX_THREADS", "7")
                .env("racer_max_threads", "9")
                .env("Racer_ALLOW_SMT", "true")
                .env("racer_shares", "ignored")
                .env(std::ffi::OsString::from_vec(vec![0xff]), "ignored")
                .env(name, value)
                .output()
                .unwrap();
            assert!(output.status.success(), "{name}: {output:?}");
        }
    }

    #[test]
    fn configured_ports_do_not_override_membership_or_discovered_hardware() {
        use crate::rdma::match_publication;
        use racer_control_wire::RailId;
        use racer_control_wire::RailMapping;
        let nic = RailMapping {
            device: "a".into(),
            port: 1,
            rail: RailId(7),
            gid: Some([1; 16]),
            numa_node: Some(9),
        };
        let live = rdma_verbs::PortInfo {
            device: "a".into(),
            port: 1,
            gid: [1; 16],
            numa_node: Some(0),
        };
        assert_eq!(
            match_publication(std::slice::from_ref(&nic), std::slice::from_ref(&live)).unwrap()[0]
                .0
                .numa_node,
            Some(9)
        );
        for bad in [
            rdma_verbs::PortInfo {
                device: "b".into(),
                ..live.clone()
            },
            rdma_verbs::PortInfo {
                port: 2,
                ..live.clone()
            },
            rdma_verbs::PortInfo {
                gid: [2; 16],
                ..live.clone()
            },
        ] {
            assert!(match_publication(std::slice::from_ref(&nic), &[bad]).is_err());
        }
        assert!(
            match_publication(std::slice::from_ref(&nic), &[live.clone(), live.clone()]).is_err()
        );
        assert!(match_publication(&[nic.clone(), nic.clone()], &[live]).is_err());
        assert!(match_publication(&[nic], &[]).is_err());
    }

    // RDMA NIC configuration arrives only through authenticated enrollment wire NICs.
    fn nic_request(nics: &str) -> Vec<u8> {
        String::from_utf8(
            include_bytes!("../../../internal/racer/wire/testdata/bootstrap-request.json").to_vec(),
        )
        .unwrap()
        .replace("\"rdma_nics\":[]", &format!("\"rdma_nics\":{nics}"))
        .into_bytes()
    }
    #[test]
    fn enrollment_nics_reject_malformed_ambiguous_or_unsafe_entries() {
        assert!(
            racer_control_wire::decode_enrollment_request(&nic_request(
                r#"[{"device":"a","port":1,"rail":0}]"#
            ))
            .is_ok()
        );
        for nics in [
            "null",
            "{}",
            "[null]",
            r#"[{"device":"a","port":0,"rail":0}]"#,
            r#"[{"device":"a","port":1,"rail":0,"gid":""}]"#,
            r#"[{"device":"a","port":1,"rail":0,"gid":null}]"#,
            r#"[{"device":"a","port":1,"port":2,"rail":0}]"#,
            r#"[{"device":"a","port":1,"rail":0,"unknown":true}]"#,
            r#"[{"device":"a","port":1,"rail":0},{"device":"a","port":1,"rail":1}]"#,
        ] {
            assert!(
                racer_control_wire::decode_enrollment_request(&nic_request(nics)).is_err(),
                "{nics}"
            );
        }
    }
    #[test]
    fn enrollment_nics_and_request_size_are_bounded() {
        let entries: Vec<_> = (0..65)
            .map(|i| format!(r#"{{"device":"d{i}","port":1,"rail":0}}"#))
            .collect();
        assert!(
            racer_control_wire::decode_enrollment_request(&nic_request(&format!(
                "[{}]",
                entries[..64].join(",")
            )))
            .is_ok()
        );
        assert!(
            racer_control_wire::decode_enrollment_request(&nic_request(&format!(
                "[{}]",
                entries.join(",")
            )))
            .is_err()
        );
        assert!(
            racer_control_wire::decode_enrollment_request(&vec![
                b' ';
                racer_control_wire::MAX_ENROLLMENT_BYTES
                    + 1
            ])
            .is_err()
        );
    }
    #[test]
    fn auto_without_ports_preserves_http_budget_explicit_true_reserves_hotplug() {
        let sim = rdma_verbs::simulation::Simulation::new()
            .with_devices(vec![])
            .unwrap();
        let _environment = sim.enter();
        assert!(
            !parse(&[("RACER_REGISTERED_BYTES", "1")])
                .unwrap()
                .enable_rdma
        );
        assert!(
            parse(&[
                ("RACER_ENABLE_RDMA", "true"),
                ("RACER_REGISTERED_BYTES", "1")
            ])
            .is_err()
        );
        assert!(parse(&[("RACER_ENABLE_RDMA", "true")]).unwrap().enable_rdma);
    }

    #[test]
    fn native_auto_requires_build_and_available_library() {
        for requested in [None, Some("auto"), Some("false")] {
            let config = Config::from_lookup(|name| {
                Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    "RACER_ENABLE_RDMA" => requested.map(str::to_owned),
                    _ => None,
                })
            })
            .unwrap();
            assert_eq!(
                config.enable_rdma,
                requested != Some("false")
                    && cfg!(feature = "rdma")
                    && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
            );
        }
    }

    fn parse(overrides: &[(&str, &str)]) -> Result<Config> {
        Config::from_lookup(|name| {
            Ok(overrides
                .iter()
                .find(|(key, _)| *key == name)
                .map(|(_, value)| (*value).to_owned())
                .or_else(|| match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
                    _ => None,
                }))
        })
    }
    #[test]
    fn warm_defaults_and_environment_limits_are_bounded() {
        let warm = parse(&[]).unwrap().warm;
        assert_eq!(warm.max_ttl, Duration::from_secs(3600));
        assert_eq!(warm.fill_timeout, Duration::from_secs(600));
        assert_eq!(
            (warm.max_jobs, warm.window, warm.retained_percent),
            (16, 2, 50)
        );
        let warm = parse(&[
            ("RACER_WARM_MAX_JOBS", "8"),
            ("RACER_WARM_WINDOW_PAGES", "4"),
        ])
        .unwrap()
        .warm;
        assert_eq!((warm.max_jobs, warm.window), (8, 4));
        for (name, value) in [
            ("RACER_WARM_MAX_TTL_SECS", "0"),
            ("RACER_WARM_FILL_TIMEOUT_SECS", "0"),
            ("RACER_WARM_MAX_JOBS", "0"),
            ("RACER_WARM_WINDOW_PAGES", "65"),
            ("RACER_RETAINED_SEGMENT_PERCENT", "101"),
        ] {
            assert!(parse(&[(name, value)]).is_err());
        }
    }

    #[test]
    fn send_crc_pair_is_opt_in_and_validated() {
        assert!(parse(&[]).unwrap().send_crc_pair.is_none());
        let valid = "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111";
        assert!(
            parse(&[("RACER_SEND_CRC_PAIR", valid)])
                .unwrap()
                .send_crc_pair
                .is_some()
        );
        for invalid in [
            "",
            "a,b",
            "a,b,c",
            "8816d91d-e896-49bf-ba8a-da97ede93818,8816d91d-e896-49bf-ba8a-da97ede93818",
        ] {
            assert!(parse(&[("RACER_SEND_CRC_PAIR", invalid)]).is_err());
        }
    }

    #[test]
    fn device_busy_wait_defaults_overrides_and_bounds() {
        assert_eq!(
            parse(&[]).unwrap().device_busy_wait,
            Duration::from_secs(120)
        );
        for (value, secs) in [("0", 0), ("1", 1), ("3600", 3_600)] {
            assert_eq!(
                parse(&[("RACER_DEVICE_BUSY_WAIT_SECS", value)])
                    .unwrap()
                    .device_busy_wait,
                Duration::from_secs(secs),
                "{value}"
            );
        }
        for value in [
            "",
            "3601",
            "-1",
            "1.5",
            "60s",
            " 60",
            "18446744073709551616",
        ] {
            assert!(
                parse(&[("RACER_DEVICE_BUSY_WAIT_SECS", value)]).is_err(),
                "{value}"
            );
        }
    }

    #[test]
    fn disk_admission_defaults_and_overrides_are_bounded() {
        let defaults = parse(&[]).unwrap();
        assert_eq!(defaults.admission_mode, AdmissionMode::SecondSight);
        assert_eq!(defaults.admission_history_bytes.get(), 4 * MIB as usize);
        assert_eq!(defaults.admission_period, Duration::from_secs(60));
        for (mode, expected) in [
            ("disabled", AdmissionMode::Disabled),
            ("second-sight", AdmissionMode::SecondSight),
        ] {
            let config = parse(&[
                ("RACER_ADMISSION_MODE", mode),
                ("RACER_ADMISSION_HISTORY_BYTES", "32"),
                ("RACER_ADMISSION_PERIOD_SECS", "1"),
            ])
            .unwrap();
            assert_eq!(config.admission_mode, expected);
            assert_eq!(config.admission_history_per_worker(1), Ok(32));
            assert!(config.admission_history_per_worker(2).is_err());
            assert_eq!(config.admission_period, Duration::from_secs(1));
        }
        assert!(
            parse(&[
                ("RACER_ADMISSION_HISTORY_BYTES", "536870912"),
                ("RACER_ADMISSION_PERIOD_SECS", "86400"),
            ])
            .is_ok()
        );
    }

    #[test]
    fn disk_admission_rejects_invalid_values_even_when_disabled() {
        for value in [
            "",
            "second_sight",
            "SECOND-SIGHT",
            "auto",
            " disabled",
            "disabled ",
        ] {
            assert!(
                parse(&[("RACER_ADMISSION_MODE", value)]).is_err(),
                "{value}"
            );
        }
        for (name, values) in [
            (
                "RACER_ADMISSION_HISTORY_BYTES",
                ["0", "31", "536870913", "4MiB", "-1", "18446744073709551616"],
            ),
            (
                "RACER_ADMISSION_PERIOD_SECS",
                ["0", "86401", "1.5", "60s", " 60", "18446744073709551616"],
            ),
        ] {
            for value in values {
                assert!(
                    parse(&[(name, value), ("RACER_ADMISSION_MODE", "disabled")]).is_err(),
                    "{name}={value}"
                );
            }
        }
        let mut config = parse(&[]).unwrap();
        config.admission_period = Duration::ZERO;
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.admission_period = Duration::from_secs(60);
        config.admission_history_bytes = NonZeroUsize::new(31).unwrap();
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
    }

    #[test]
    fn disk_admission_history_partition_does_not_multiply_node_budget() {
        let config = parse(&[("RACER_ADMISSION_HISTORY_BYTES", "97")]).unwrap();
        assert_eq!(config.admission_history_per_worker(3), Ok(32));
        assert!(config.admission_history_per_worker(4).is_err());
        assert!(config.admission_history_per_worker(0).is_err());
        let config = parse(&[]).unwrap();
        assert_eq!(config.admission_history_per_worker(8), Ok(512 * 1024));
        for workers in [1, 2, 3, 8, 64] {
            let share = config.admission_history_per_worker(workers).unwrap();
            let policy =
                crate::retention::Retention::configured(share, 1, config.admission_period, true)
                    .unwrap();
            let allocated = policy.snapshot().filter_bits / 8;
            assert_eq!(allocated, share / 32 * 32);
            assert!(allocated * workers <= config.admission_history_bytes.get());
        }
        let maximum_history_workers = config.admission_history_bytes.get() / 32;
        assert_eq!(
            config.admission_history_per_worker(maximum_history_workers),
            Ok(32)
        );
        assert!(
            config
                .admission_history_per_worker(maximum_history_workers + 1)
                .is_err()
        );
    }

    #[test]
    fn receive_gate_is_opt_in_and_bounded() {
        assert_eq!(parse(&[]).unwrap().peer_receive.active, 0);
        let c = parse(&[
            ("RACER_PEER_RECEIVE_MAX", "8"),
            ("RACER_PEER_RECEIVE_QUEUE_MAX", "32"),
            ("RACER_PEER_RECEIVE_WAIT_MS", "500"),
        ])
        .unwrap();
        assert_eq!(c.peer_receive.active, 8);
        assert_eq!(c.peer_receive.queued, 32);
        assert_eq!(c.peer_receive.wait, Duration::from_millis(500));
        for pair in [
            ("RACER_PEER_RECEIVE_MAX", "65537"),
            ("RACER_PEER_RECEIVE_QUEUE_MAX", "0"),
            ("RACER_PEER_RECEIVE_QUEUE_MAX", "65537"),
            ("RACER_PEER_RECEIVE_WAIT_MS", "0"),
            ("RACER_PEER_RECEIVE_WAIT_MS", "30001"),
        ] {
            assert!(parse(&[pair]).is_err());
        }
    }

    #[test]
    fn defaults_are_bounded_and_identity_is_unresolved() {
        let config = parse(&[]).unwrap();
        assert_eq!(config.max_threads, usize::MAX);
        assert!(!config.allow_smt);
        assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        assert_eq!(
            config.enable_rdma,
            cfg!(feature = "rdma") && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
        );
        assert_eq!(config.slab_bytes / config.segment_bytes, 16);
        assert!(!config.opaque_relay);
        assert_eq!(config.free_segment_reserve, 2);
        assert!(config.diagnostics_listen.ip().is_loopback());
        assert_eq!(config.request_timeout, Duration::from_secs(30));
        assert_eq!(config.peer_attempt_timeout, Duration::from_secs(30));
        assert_eq!(
            config.origin_connections_per_cache.get(),
            DEFAULT_ORIGIN_CONNECTIONS_PER_CACHE
        );
        assert_eq!(config.limits.connections_per_neighbor.get(), 2);
        assert_eq!(config.validate(), Ok(()));
        crate::app::Application::assemble(config).unwrap();
        assert!(parse(&[("RACER_ENABLE_RDMA", "true"), ("RACER_MAX_THREADS", "7")]).is_ok());
    }

    type LimitField = (&'static str, fn(&Limits) -> NonZeroUsize);

    const SCALED_FIELDS: [LimitField; 14] = [
        ("RACER_PLAINTEXT_BYTES", |l| l.plaintext_bytes),
        ("RACER_CIPHERTEXT_BYTES", |l| l.ciphertext_bytes),
        ("RACER_DIRTY_BYTES", |l| l.dirty_bytes),
        ("RACER_REGISTERED_BYTES", |l| l.registered_bytes),
        ("RACER_REQUEST_CONTEXT_BYTES", |l| l.request_context_bytes),
        ("RACER_FLIGHTS", |l| l.flights),
        ("RACER_QUEUE_ENTRIES", |l| l.queue_entries),
        ("RACER_CLIENT_CONNECTIONS", |l| l.client_connections),
        ("RACER_PIPES", |l| l.pipes),
        ("RACER_PLACEMENT_CACHE_BYTES", |l| l.placement_cache_bytes),
        ("RACER_CACHED_PATHS", |l| l.cached_paths),
        ("RACER_PATH_CACHE_BYTES", |l| l.path_cache_bytes),
        ("RACER_METADATA_ENTRIES", |l| l.metadata_entries),
        ("RACER_RELAY_TRANSFERS", |l| l.relay_transfers),
    ];

    #[test]
    fn scale_limits_multiplies_defaulted_dimensions_only() {
        for workers in [1, 6, 64] {
            let mut config = parse(&[]).unwrap();
            let before = config.limits.clone();
            config.scale_limits(workers).unwrap();
            for (name, field) in SCALED_FIELDS {
                assert_eq!(
                    field(&config.limits).get(),
                    field(&before).get() * workers,
                    "{name}"
                );
            }
            for field in [
                (|l: &Limits| l.waiters_per_flight) as fn(&Limits) -> NonZeroUsize,
                |l| l.connections_per_neighbor,
                |l| l.range_window_pages,
                |l| l.header_bytes,
                |l| l.active_path_searches,
                |l| l.retained_snapshots,
            ] {
                assert_eq!(field(&config.limits), field(&before));
            }
            assert_eq!(config.scaled_limits, ScaledLimits::default());
        }
    }

    #[test]
    fn scale_limits_preserves_explicit_environment_dimensions() {
        let defaults = parse(&[]).unwrap();
        for (explicit, field) in SCALED_FIELDS {
            let value = field(&defaults.limits).to_string();
            let mut config = parse(&[(explicit, &value)]).unwrap();
            config.scale_limits(6).unwrap();
            for (name, field) in SCALED_FIELDS {
                let factor = if name == explicit { 1 } else { 6 };
                assert_eq!(
                    field(&config.limits).get(),
                    field(&defaults.limits).get() * factor,
                    "explicit={explicit} field={name}"
                );
            }
        }
    }

    #[test]
    fn scale_limits_is_idempotent() {
        let mut config = parse(&[]).unwrap();
        config.scale_limits(6).unwrap();
        let once = config.limits.clone();
        config.scale_limits(9).unwrap();
        for (name, field) in SCALED_FIELDS {
            assert_eq!(field(&config.limits), field(&once), "{name}");
        }
    }

    #[test]
    fn scale_limits_rejects_zero_workers_and_overflow() {
        let mut config = parse(&[]).unwrap();
        assert_eq!(config.scale_limits(0), Err(Error::InvalidConfiguration));
        for (name, field) in SCALED_FIELDS {
            let mut config = parse(&[]).unwrap();
            config.scaled_limits = ScaledLimits::default();
            match name {
                "RACER_PLAINTEXT_BYTES" => config.scaled_limits.plaintext_bytes = true,
                "RACER_CIPHERTEXT_BYTES" => config.scaled_limits.ciphertext_bytes = true,
                "RACER_DIRTY_BYTES" => config.scaled_limits.dirty_bytes = true,
                "RACER_REGISTERED_BYTES" => config.scaled_limits.registered_bytes = true,
                "RACER_REQUEST_CONTEXT_BYTES" => config.scaled_limits.request_context_bytes = true,
                "RACER_FLIGHTS" => config.scaled_limits.flights = true,
                "RACER_QUEUE_ENTRIES" => config.scaled_limits.queue_entries = true,
                "RACER_CLIENT_CONNECTIONS" => config.scaled_limits.client_connections = true,
                "RACER_PIPES" => config.scaled_limits.pipes = true,
                "RACER_PLACEMENT_CACHE_BYTES" => config.scaled_limits.placement_cache_bytes = true,
                "RACER_CACHED_PATHS" => config.scaled_limits.cached_paths = true,
                "RACER_PATH_CACHE_BYTES" => config.scaled_limits.path_cache_bytes = true,
                "RACER_METADATA_ENTRIES" => config.scaled_limits.metadata_entries = true,
                "RACER_RELAY_TRANSFERS" => config.scaled_limits.relay_transfers = true,
                _ => unreachable!(),
            }
            let workers = usize::MAX / field(&config.limits).get() + 1;
            assert_eq!(
                config.scale_limits(workers),
                Err(Error::InvalidConfiguration),
                "{name}"
            );
        }
    }

    #[test]
    fn peer_admission_environment_is_bounded_and_node_scoped() {
        let config = parse(&[
            ("RACER_PEER_INFLIGHT_MAX", "8"),
            ("RACER_PEER_PER_NEIGHBOR_MAX", "2"),
        ])
        .unwrap();
        assert_eq!(config.peer_admission.total, 8);
        assert_eq!(config.peer_admission.per_peer, 2);
        for overrides in [
            vec![("RACER_PEER_INFLIGHT_MAX", "0")],
            vec![("RACER_PEER_INFLIGHT_MAX", "65537")],
            vec![("RACER_PEER_PER_NEIGHBOR_MAX", "0")],
            vec![("RACER_PEER_PER_NEIGHBOR_MAX", "257")],
        ] {
            assert!(parse(&overrides).is_err());
        }
    }
    #[test]
    fn page_hedges_are_off_by_default_and_require_bounded_capacity() {
        assert_eq!(parse(&[]).unwrap().page_hedge.slots, 0);
        assert!(parse(&[("RACER_PAGE_HEDGE_SLOTS", "1")]).is_ok());
        for pairs in [
            vec![("RACER_PAGE_HEDGE_SLOTS", "33")],
            vec![("RACER_PAGE_HEDGE_DELAY_MS", "0")],
            vec![
                ("RACER_PAGE_HEDGE_SLOTS", "1"),
                ("RACER_PAGE_HEDGE_BYTES", "1"),
            ],
        ] {
            assert!(parse(&pairs).is_err());
        }
    }

    #[test]
    fn replica_selection_defaults_to_spread_and_accepts_exact_values() {
        use crate::read::candidates::ReplicaSelection;
        assert_eq!(
            parse(&[]).unwrap().replica_selection,
            ReplicaSelection::Spread
        );
        assert_eq!(
            parse(&[("RACER_REPLICA_SELECTION", "spread")])
                .unwrap()
                .replica_selection,
            ReplicaSelection::Spread
        );
        assert_eq!(
            parse(&[("RACER_REPLICA_SELECTION", "primary")])
                .unwrap()
                .replica_selection,
            ReplicaSelection::Primary
        );
        for value in ["", "Primary", "SPREAD", "neighbor", "primary ", "random"] {
            assert!(
                parse(&[("RACER_REPLICA_SELECTION", value)]).is_err(),
                "{value:?}"
            );
        }
    }

    fn exact_boolean(name: &str, field: fn(&Config) -> bool) {
        exact_boolean_default(name, field, false);
    }

    fn exact_boolean_default(name: &str, field: fn(&Config) -> bool, default: bool) {
        assert_eq!(field(&parse(&[]).unwrap()), default);
        for (value, expected) in [("true", true), ("false", false)] {
            assert_eq!(field(&parse(&[(name, value)]).unwrap()), expected);
        }
        for value in [
            "", "1", "0", "TRUE", "False", "auto", " true", "true ", "true\n",
        ] {
            assert!(matches!(
                parse(&[(name, value)]),
                Err(Error::InvalidConfiguration)
            ));
        }
        assert!(
            Config::from_lookup(|key| {
                if key == name {
                    Err(Error::InvalidConfiguration)
                } else {
                    Ok(match key {
                        "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                        "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
                        _ => None,
                    })
                }
            })
            .is_err()
        );
    }
    #[test]
    fn peer_tcp_nodelay_requires_exact_boolean_and_defaults_on() {
        exact_boolean_default("RACER_PEER_TCP_NODELAY", |c| c.peer_tcp_nodelay, true);
    }

    #[test]
    fn rdma_zero_copy_requires_exact_boolean_and_defaults_on() {
        exact_boolean_default("RACER_RDMA_ZERO_COPY", |c| c.rdma_zero_copy, true);
    }

    #[test]
    fn pprof_requires_exact_boolean_and_defaults_off() {
        exact_boolean("RACER_PPROF_ENABLED", |c| c.pprof_enabled);
    }

    #[test]
    fn memory_pressure_requires_exact_boolean_and_defaults_on() {
        exact_boolean_default("RACER_MEMORY_PRESSURE", |c| c.memory_pressure, true);
    }

    #[test]
    fn opaque_relay_requires_an_exact_explicit_boolean() {
        exact_boolean("RACER_OPAQUE_RELAY", |c| c.opaque_relay);
    }

    #[test]
    fn topology_cache_dimensions_are_independent_and_bounded() {
        let defaults = parse(&[]).unwrap();
        assert_eq!(
            defaults.limits.placement_cache_bytes.get(),
            16 * MIB as usize
        );
        assert_eq!(defaults.limits.cached_paths.get(), 128);
        assert_eq!(defaults.limits.path_cache_bytes.get(), 8 * MIB as usize);
        assert_eq!(defaults.limits.active_path_searches.get(), 8);
        let config = parse(&[
            ("RACER_PLACEMENT_CACHE_BYTES", "1025"),
            ("RACER_CACHED_PATHS", "1"),
            ("RACER_PATH_CACHE_BYTES", "7"),
            ("RACER_ACTIVE_PATH_SEARCHES", "2"),
        ])
        .unwrap();
        assert_eq!(config.limits.placement_cache_bytes.get(), 1025);
        assert_eq!(config.limits.cached_paths.get(), 1);
        assert_eq!(config.limits.path_cache_bytes.get(), 7);
        assert_eq!(config.limits.active_path_searches.get(), 2);
        for (name, value) in [
            ("RACER_PLACEMENT_CACHE_BYTES", "1023"),
            ("RACER_PLACEMENT_CACHE_BYTES", "536870913"),
            ("RACER_PATH_CACHE_BYTES", "0"),
            ("RACER_PATH_CACHE_BYTES", "536870913"),
            ("RACER_ACTIVE_PATH_SEARCHES", "0"),
            ("RACER_ACTIVE_PATH_SEARCHES", "65"),
            ("RACER_CACHED_PATHS", "0"),
            ("RACER_CACHED_PATHS", "1048577"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        let mut config = defaults;
        config.limits.placement_cache_bytes = NonZeroUsize::new(1023).unwrap();
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.limits.placement_cache_bytes = NonZeroUsize::new(1024).unwrap();
        config.limits.path_cache_bytes = NonZeroUsize::new(512 * MIB as usize + 1).unwrap();
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.limits.path_cache_bytes = NonZeroUsize::new(1).unwrap();
        config.limits.active_path_searches = NonZeroUsize::new(65).unwrap();
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.limits.active_path_searches = NonZeroUsize::new(64).unwrap();
        assert_eq!(config.validate(), Ok(()));
    }

    #[test]
    fn smt_requires_an_exact_explicit_boolean() {
        exact_boolean("RACER_ALLOW_SMT", |c| c.allow_smt);
    }

    #[test]
    fn origin_connection_cap_is_independent_and_validated() {
        let config = parse(&[
            ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "16"),
            ("RACER_CONNECTIONS_PER_NEIGHBOR", "1"),
        ])
        .unwrap();
        assert_eq!(config.origin_connections_per_cache.get(), 16);
        assert_eq!(config.limits.connections_per_neighbor.get(), 1);
        for value in ["", "0", "1025", "129", "-1", "1 ", "many"] {
            assert!(
                parse(&[("RACER_ORIGIN_CONNECTIONS_PER_CACHE", value)]).is_err(),
                "{value}"
            );
        }
        assert!(
            parse(&[
                ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "1024"),
                ("RACER_CLIENT_CONNECTIONS", "1024")
            ])
            .is_ok()
        );
        let small = parse(&[("RACER_CLIENT_CONNECTIONS", "12")]).unwrap();
        assert_eq!(small.origin_connections_per_cache.get(), 12);
        assert!(
            parse(&[
                ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "13"),
                ("RACER_CLIENT_CONNECTIONS", "12")
            ])
            .is_err()
        );
    }

    #[test]
    fn origin_pool_keeps_metadata_headroom_under_outbound_saturation() {
        use crate::admission::AdmissionPolicy;
        use crate::admission::ResourceClass;
        use flow_control::Policy;
        let effective = |config: &Config| {
            let outbound = AdmissionPolicy::new(config.limits.clone())
                .limit(ResourceClass::OutboundConnection);
            let cap = crate::app::origin_pool_capacity(
                config.origin_connections_per_cache.get(),
                outbound,
            );
            (cap, outbound)
        };
        // Stock budget: page GETs stop at cap - 1 active, before outbound
        // admission is exhausted, so a HEAD can still open a connection.
        let (cap, outbound) = effective(&parse(&[]).unwrap());
        assert!(cap <= outbound, "{cap} > {outbound}");
        assert!(cap - 1 < outbound);
        // An explicit cap above outbound admission is bounded the same way.
        let explicit = parse(&[("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "64")]).unwrap();
        assert_eq!(effective(&explicit), (outbound, outbound));
        // Large budgets keep the full default.
        let large = parse(&[("RACER_CLIENT_CONNECTIONS", "16384")]).unwrap();
        assert_eq!(effective(&large).0, DEFAULT_ORIGIN_CONNECTIONS_PER_CACHE);
        assert_eq!(crate::app::origin_pool_capacity(8, 0), 1);
    }

    #[test]
    fn diagnostics_accepts_expanded_pod_ip_of_either_family() {
        for (address, expected) in [
            ("192.0.2.1:9090", "192.0.2.1:9090"),
            ("[192.0.2.1]:9090", "192.0.2.1:9090"),
            ("[2001:db8::1]:9090", "[2001:db8::1]:9090"),
        ] {
            let config = parse(&[("RACER_DIAGNOSTICS_LISTEN", address)]).unwrap();
            assert_eq!(config.diagnostics_listen, expected.parse().unwrap());
            assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        }
        for address in [
            "[$(RACER_POD_IP)]:9090",
            "[]:9090",
            "[localhost]:9090",
            "[192.0.2.1]:0",
            "[192.0.2.1]:65536",
            "[192.0.2.1]:+9090",
            "[224.0.0.1]:9090",
            "[255.255.255.255]:9090",
            "[::ffff:192.0.2.1]:9090",
            "[fe80::1%eth0]:9090",
            "[ff02::1]:9090",
            "[192.0.2.1]:7443",
        ] {
            assert!(
                parse(&[("RACER_DIAGNOSTICS_LISTEN", address)]).is_err(),
                "{address}"
            );
        }
        assert!(
            parse(&[
                ("RACER_PEER_LISTEN", "0.0.0.0:9090"),
                ("RACER_DIAGNOSTICS_LISTEN", "[192.0.2.1]:9091"),
            ])
            .is_ok()
        );
    }

    #[test]
    fn peer_accepts_expanded_pod_ip_of_either_family() {
        for (address, expected) in [
            ("192.0.2.1:7443", "192.0.2.1:7443"),
            ("[192.0.2.1]:7443", "192.0.2.1:7443"),
            ("[2001:db8::1]:7443", "[2001:db8::1]:7443"),
            ("[192.0.2.1]:65535", "192.0.2.1:65535"),
        ] {
            let config = parse(&[("RACER_PEER_LISTEN", address)]).unwrap();
            assert_eq!(config.peer_listen, expected.parse().unwrap());
            assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        }
        assert_eq!(
            parse(&[]).unwrap().peer_listen,
            "0.0.0.0:7443".parse().unwrap()
        );
        for address in [
            "[$(RACER_POD_IP)]:7443",
            "[]:7443",
            "[localhost]:7443",
            "[192.0.2.1]:0",
            "[192.0.2.1]:65536",
            "[192.0.2.1]:+7443",
            "[192.0.2.1]:7443extra",
            "[[192.0.2.1]]:7443",
            "[224.0.0.1]:7443",
            "[255.255.255.255]:7443",
            "[::ffff:192.0.2.1]:7443",
            "[fe80::1%eth0]:7443",
            "[ff02::1]:7443",
            "2001:db8::1:7443",
        ] {
            assert!(
                parse(&[("RACER_PEER_LISTEN", address)]).is_err(),
                "{address}"
            );
        }
        for ip in ["192.0.2.1", "2001:db8::1"] {
            let peer = format!("[{ip}]:9090");
            let diagnostics = format!("[{ip}]:9091");
            assert!(
                parse(&[
                    ("RACER_PEER_LISTEN", &peer),
                    ("RACER_DIAGNOSTICS_LISTEN", &diagnostics),
                ])
                .is_ok()
            );
            assert!(
                parse(&[
                    ("RACER_PEER_LISTEN", &peer),
                    ("RACER_DIAGNOSTICS_LISTEN", &peer),
                ])
                .is_err()
            );
        }
    }

    fn peer_listener_round_trip(ip: &str) {
        use std::io::Write;
        use std::net::TcpListener;
        use std::net::TcpStream;

        let address = format!("[{ip}]:7443");
        let mut config = parse(&[("RACER_PEER_LISTEN", &address)]).unwrap();
        assert_eq!(config.peer_listen.ip(), ip.parse::<IpAddr>().unwrap());
        // Production rejects port zero. Allocate an ephemeral test port after
        // configuration validation, using the same exact bind as PeerServer.
        config.peer_listen.set_port(0);
        let listener = TcpListener::bind(config.peer_listen).unwrap();
        let bound = listener.local_addr().unwrap();
        assert_eq!(bound.ip(), config.peer_listen.ip());
        let mut client = TcpStream::connect_timeout(&bound, Duration::from_secs(2)).unwrap();
        client
            .set_write_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        client.write_all(b"peer").unwrap();
        listener.set_nonblocking(true).unwrap();
        let (mut accepted, _) = listener.accept().unwrap();
        assert_eq!(accepted.local_addr().unwrap(), bound);
        accepted
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let mut body = [0; 4];
        accepted.read_exact(&mut body).unwrap();
        assert_eq!(&body, b"peer");
    }

    #[test]
    fn peer_listener_binds_exact_ipv4() {
        peer_listener_round_trip("127.0.0.1");
    }

    #[test]
    fn peer_listener_binds_exact_ipv6() {
        peer_listener_round_trip("::1");
    }

    #[test]
    fn required_identity_and_lookup_errors_fail_closed() {
        assert!(matches!(
            Config::from_lookup(|_| Ok(None)),
            Err(Error::InvalidConfiguration)
        ));
        assert!(matches!(
            Config::from_lookup(|_| Err(Error::InvalidConfiguration)),
            Err(Error::InvalidConfiguration)
        ));
        for value in [
            "",
            "node-name",
            "00000000-0000-0000-0000-000000000000",
            "00000000-0000-4000-8000-00000000000A",
        ] {
            assert!(parse(&[("RACER_CLUSTER_ID", value)]).is_err(), "{value}");
        }
        assert!(parse(&[("RACER_SHARES", "")]).is_err());
        let mut config = parse(&[]).unwrap();
        config.node = NodeId("node-name".into());
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.node = NodeId("00000000-0000-4000-8000-000000000002".into());
        assert_eq!(config.validate(), Ok(()));
    }

    #[test]
    fn numeric_and_boolean_parsing_is_strict() {
        assert_eq!(parse(&[]).unwrap().shares.get(), 4);
        assert_eq!(parse(&[]).unwrap().shares.get(), DEFAULT_SHARES);
        assert_eq!(parse(&[("RACER_SHARES", "9")]).unwrap().shares.get(), 9);
        for value in ["0", "-1", "+4", "4294967296", "4.0"] {
            assert!(parse(&[("RACER_SHARES", value)]).is_err());
        }
        let config = parse(&[
            ("RACER_METADATA_ENTRIES", "32"),
            ("RACER_DISK_PAGE_ENTRIES", "8192"),
        ])
        .unwrap();
        assert_eq!(config.disk_page_entries.get(), 8192);
        assert_eq!(config.limits.metadata_entries.get(), 32);
        for value in [
            "",
            " 8",
            "8 ",
            "+8",
            "-8",
            "8MiB",
            "1.5",
            "18446744073709551616",
            "8\n",
        ] {
            assert!(parse(&[("RACER_MAX_THREADS", value)]).is_err(), "{value:?}");
        }
        for value in ["TRUE", "1", "yes", " false"] {
            assert!(parse(&[("RACER_ENABLE_RDMA", value)]).is_err());
        }
        for value in ["0", "1"] {
            assert!(parse(&[("RACER_MAX_THREADS", value)]).is_err());
        }
        for value in [2, 7, 257, usize::MAX] {
            assert_eq!(
                parse(&[("RACER_MAX_THREADS", &value.to_string())])
                    .unwrap()
                    .max_threads,
                value
            );
        }
    }

    #[test]
    fn endpoint_and_listener_validation_precedes_network_io() {
        for url in [
            "https://control.example",
            "https://localhost:443/",
            "https://127.0.0.1:7443",
            "https://[::1]:7443/",
        ] {
            assert!(parse(&[("RACER_CONTROL_ENDPOINT", url)]).is_ok(), "{url}");
        }
        for url in [
            "http://control.example",
            "https://",
            "https://user@host",
            "https://host/path",
            "https://host//",
            "https://host?x",
            "https://host#x",
            "https://host:0",
            "https://host:65536",
            "https://host:+443",
            "https://bad host",
            "https://host\r\nX: y",
            "https://host\\evil",
            "https://%68ost",
            "https://[::]",
            "https://[::1",
            "https://::1",
            "https://0.0.0.0",
            "https://-host",
            "https://host..example",
        ] {
            assert!(
                parse(&[("RACER_CONTROL_ENDPOINT", url)]).is_err(),
                "{url:?}"
            );
        }
        for address in [
            "localhost:7443",
            "127.0.0.1:0",
            "224.0.0.1:7443",
            "255.255.255.255:7443",
            "[ff02::1]:7443",
            "[::ffff:127.0.0.1]:7443",
        ] {
            assert!(
                parse(&[("RACER_PEER_LISTEN", address)]).is_err(),
                "{address}"
            );
        }
        assert!(parse(&[("RACER_DIAGNOSTICS_LISTEN", "127.0.0.1:7443")]).is_err());
        assert!(parse(&[("RACER_PEER_LISTEN", "[::]:7443")]).is_ok());
    }

    #[test]
    fn paths_are_absolute_canonical_and_separate_without_filesystem_access() {
        for name in [
            "RACER_TRUST_BUNDLE",
            "RACER_SERVICE_ACCOUNT_TOKEN",
            "RACER_IDENTITY_DIRECTORY",
            "RACER_SLAB_DIRECTORY",
        ] {
            for path in [
                "",
                "relative/path",
                "/",
                "/safe/../keys",
                "/safe/./keys",
                "/safe//keys",
                "/safe/keys/",
                "/safe/\0keys",
            ] {
                assert!(parse(&[(name, path)]).is_err(), "{name}: {path:?}");
            }
        }
        for path in [
            "/etc/racer/trust/ca.crt",
            "/etc/racer/trust",
            "/etc/racer",
            "/var/lib/racer/slabs",
            "/var/lib/racer",
            "/etc/racer/trust/ca.crt/private",
        ] {
            assert!(
                parse(&[("RACER_IDENTITY_DIRECTORY", path)]).is_err(),
                "{path}"
            );
        }
        assert!(
            parse(&[
                ("RACER_IDENTITY_DIRECTORY", "/absent-config-test/identity"),
                ("RACER_SLAB_DIRECTORY", "/absent-config-test/slabs")
            ])
            .is_ok()
        );
        assert!(parse(&[("RACER_TRUST_BUNDLE", &format!("/{}", "a".repeat(256)))]).is_err());
    }

    #[test]
    fn geometry_includes_aead_and_envelope_and_preserves_a_writable_segment() {
        let mut config = parse(&[]).unwrap();
        let minimum = PAGE_BYTES + 16 + MAX_HEADER_BYTES as u64;
        config.segment_bytes = minimum;
        config.slab_bytes = minimum * 3;
        assert_eq!(config.validate(), Ok(()));
        config.segment_bytes -= 1;
        config.slab_bytes = config.segment_bytes * 3;
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        for (name, value) in [
            ("RACER_SEGMENT_BYTES", "0"),
            ("RACER_SLAB_BYTES", "0"),
            ("RACER_SEGMENT_BYTES", "16777216"),
            ("RACER_SLAB_BYTES", "1073741825"),
            ("RACER_FREE_SEGMENT_RESERVE", "0"),
            ("RACER_FREE_SEGMENT_RESERVE", "16"),
            ("RACER_SLAB_BYTES", "18446744073709551615"),
            ("RACER_FREE_SEGMENT_RESERVE", "18446744073709551615"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
    }

    #[test]
    fn budgets_require_page_window_and_control_progress() {
        for (name, value) in [
            ("RACER_PLAINTEXT_BYTES", "16777216"),
            ("RACER_CIPHERTEXT_BYTES", "16777216"),
            ("RACER_DIRTY_BYTES", "16777216"),
            ("RACER_REQUEST_CONTEXT_BYTES", "32768"),
            ("RACER_QUEUE_ENTRIES", "1"),
            ("RACER_RETAINED_SNAPSHOTS", "1"),
            ("RACER_HEADER_BYTES", "32769"),
            ("RACER_HEADER_BYTES", "1023"),
            ("RACER_FLIGHTS", "0"),
            ("RACER_PIPES", "65537"),
            ("RACER_RANGE_WINDOW_PAGES", "16"),
            ("RACER_PLAINTEXT_BYTES", "18446744073709551615"),
            ("RACER_CONNECTIONS_PER_NEIGHBOR", "129"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        assert!(
            parse(&[
                ("RACER_FLIGHTS", "65536"),
                ("RACER_WAITERS_PER_FLIGHT", "4096")
            ])
            .is_err()
        );
        assert!(
            parse(&[
                ("RACER_ENABLE_RDMA", "true"),
                ("RACER_REGISTERED_BYTES", "16777216")
            ])
            .is_err()
        );
        assert!(parse(&[("RACER_REGISTERED_BYTES", "1")]).is_ok());
        assert!(
            parse(&[
                ("RACER_PLAINTEXT_BYTES", "68719476736"),
                ("RACER_CIPHERTEXT_BYTES", "68719476736"),
                ("RACER_DIRTY_BYTES", "68719476736"),
                ("RACER_REGISTERED_BYTES", "68719476736"),
            ])
            .is_err()
        );
        let mut config = parse(&[]).unwrap();
        let window = config.limits.range_window_pages.get();
        config.limits.plaintext_bytes =
            NonZeroUsize::new((window + 1) * PAGE_BYTES as usize).unwrap();
        config.limits.ciphertext_bytes =
            NonZeroUsize::new((window + 1) * (PAGE_BYTES as usize + 16) + MAX_HEADER_BYTES)
                .unwrap();
        assert_eq!(config.validate(), Ok(()));
        config.limits.ciphertext_bytes =
            NonZeroUsize::new(config.limits.ciphertext_bytes.get() - 1).unwrap();
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
    }

    #[test]
    fn timeouts_are_positive_bounded_and_stall_fits_request() {
        for (name, value) in [
            ("RACER_REQUEST_TIMEOUT_MS", "0"),
            ("RACER_REQUEST_TIMEOUT_MS", "86400001"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "0"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "86400001"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "18446744073709551615"),
            ("RACER_READER_STALL_TIMEOUT_MS", "0"),
            ("RACER_READER_STALL_TIMEOUT_MS", "30001"),
            ("RACER_SHUTDOWN_TIMEOUT_MS", "0"),
            ("RACER_SHUTDOWN_TIMEOUT_MS", "3600001"),
            ("RACER_REQUEST_TIMEOUT_MS", "18446744073709551615"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        assert!(
            parse(&[
                ("RACER_REQUEST_TIMEOUT_MS", "1"),
                ("RACER_READER_STALL_TIMEOUT_MS", "1"),
                ("RACER_SHUTDOWN_TIMEOUT_MS", "1")
            ])
            .is_ok()
        );
    }

    #[test]
    fn peer_attempt_timeout_is_independent_and_validates_programmatic_config() {
        let mut config = parse(&[
            ("RACER_REQUEST_TIMEOUT_MS", "10"),
            ("RACER_READER_STALL_TIMEOUT_MS", "1"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "6000"),
        ])
        .unwrap();
        assert_eq!(config.peer_attempt_timeout, Duration::from_secs(6));
        assert_eq!(config.validate(), Ok(()));
        config.peer_attempt_timeout = Duration::ZERO;
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.peer_attempt_timeout = Duration::from_millis(86_400_001);
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
    }
}
