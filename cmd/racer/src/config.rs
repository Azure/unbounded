//! Environment and deployment configuration, validated once before startup.
//!
//! Every accepted variable is one entry in [`table::TABLE`]. Parsing,
//! defaults, single-variable bounds, the unknown-variable check, and the
//! generated reference documentation come from that table; cross-variable
//! rules live in [`Config::validate`] and name every variable they involve.
//!
//! Only `main` reads the process environment ([`Config::from_env`]). Tests and
//! simulations build a [`Config`] from an explicit map ([`Config::from_map`])
//! or lookup ([`Config::from_lookup`]), so many nodes in one process never see
//! each other's settings.
//!
//! Default to all eligible physical cores, subject to CPU and resource budgets.
//! Shares and physical NIC bindings come from accepted controller membership.

pub mod spec;
pub mod table;

use crate::error::Error;
use crate::error::Result;
use crate::model::PAGE_BYTES;
use racer_wire::control::ClusterId;
use racer_wire::control::NodeId;
pub use spec::ConfigError;
use spec::Values;
use std::collections::BTreeMap;
use std::net::IpAddr;
use std::net::SocketAddr;
use std::num::NonZeroUsize;
use std::path::Path;
use std::path::PathBuf;
use std::time::Duration;
use table::MAX_BYTES;
use table::MAX_ENTRIES;
use table::RECORD_BYTES;
use table::TABLE;

pub const DEFAULT_MAX_THREADS: usize = usize::MAX;
/// Not a Node UID. Only verified enrollment/local identity recovery may replace it.
pub const UNRESOLVED_NODE_ID: &str = "";
/// Not a cluster UID. Managed startup replaces it from the elected control bootstrap.
pub const UNRESOLVED_CLUSTER_ID: &str = "";
/// Projected control-audience token used by managed deployments.
pub const MANAGED_SERVICE_ACCOUNT_TOKEN: &str = "/var/run/racer-token/token";
/// Control-audience token default for a standalone controller.
pub const STANDALONE_SERVICE_ACCOUNT_TOKEN: &str = "/var/run/secrets/racer-control/token";
/// Smallest explicit thread cap in managed mode: I/O, crypto, and cluster control.
pub const MANAGED_MIN_THREADS: usize = 3;
const MIB: u64 = 1024 * 1024;

/// Disk retention policy only; never changes request admission or authorization.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionMode {
    Disabled,
    SecondSight,
}

pub use racer_core::admission::Limits;

/// Which shared limit dimensions hold per-worker amounts.
///
/// Worker sizing divides every shared dimension by the I/O worker count. A
/// `*_PER_WORKER` variable (or its default) is stated per worker, so it is
/// multiplied by the planned worker count first and each lane's budget stays
/// constant as cores are added. The plain-named variable is a node-wide total
/// and is never scaled.
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
    pub peer_connections: bool,
    pub outbound_connections: bool,
    pub pipes: bool,
    pub placement_cache_bytes: bool,
    pub cached_paths: bool,
    pub path_cache_bytes: bool,
    pub metadata_entries: bool,
    pub relay_transfers: bool,
    /// Neither registered-memory variable was set, so the scaled default is
    /// capped at [`DEFAULT_REGISTERED_BYTES_CAP`].
    pub registered_bytes_default_cap: bool,
}

/// Node-wide ceiling on the default RDMA registered pool. The per-worker
/// default scales the pool with node size, so small nodes pin little memory,
/// while large nodes stop at a pool that still holds 56 buffers per rail on
/// eight rails. An explicit setting is never capped.
pub const DEFAULT_REGISTERED_BYTES_CAP: u64 = 8 * 1024 * MIB;

/// Origin transport and Azure credential settings.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct OriginSettings {
    /// CA bundle for HTTPS origins and the Entra authority.
    pub trust_bundle: PathBuf,
    /// Entra authority base URL ending in `/`.
    pub azure_authority_host: String,
    /// Workload identity federated token file.
    pub azure_federated_token_file: Option<PathBuf>,
}

impl Default for OriginSettings {
    fn default() -> Self {
        Self {
            trust_bundle: http1::tls::DEFAULT_TRUST_BUNDLE.into(),
            azure_authority_host: crate::origin::azure_auth::DEFAULT_AUTHORITY_HOST.into(),
            azure_federated_token_file: None,
        }
    }
}

pub struct Config {
    pub spill: crate::read::spill::Config,
    pub prefetch: crate::prefetch::Config,
    pub send_tag_pair: Option<crate::telemetry::Pair>,
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
    /// Explicit standalone cluster, or unresolved until managed bootstrap adopts it.
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
    /// RDMA-to-HTTP circuit breaker and per-page RDMA timeout.
    pub rdma_breaker: crate::rdma::breaker::BreakerConfig,
    /// `gpu-read` support. Resolved against hardware at startup, on the GPU
    /// helper thread, because GPU handles cannot move between threads.
    pub enable_gpu: GpuMode,
    /// Per-GPU size of the dataplane-owned staging buffer that `gpu-read`
    /// decrypts sealed (ciphertext) pages through: two scratch slots plus
    /// slots for cuFile reads. Allocated on a GPU's first sealed page; pages
    /// that arrive as plaintext are copied straight to the client and never
    /// touch it. Below two slots, GPU decrypt is off and the CPU decrypts.
    pub gpu_staging_bytes: u64,
    /// Per-GPU GPU memory that peers' NICs write `gpu-read` ciphertext into
    /// over RDMA (GPUDirect RDMA landing). 0 turns landing off.
    pub gpu_landing_bytes: u64,
    /// Ask cuFile to read local NVMe into GPU memory over PCI P2PDMA
    /// (`use_pci_p2pdma`) instead of nvidia-fs. Defaults off.
    pub gpu_pci_p2pdma: bool,
    /// `RACER_GPU_PEER`: `auto` (default) lets one GPU copy another GPU's
    /// sealed ciphertext over NVLink or PCIe peer-to-peer when the driver
    /// allows it; `off` never enables peer access and moves such ciphertext
    /// through host memory instead.
    pub gpu_peer: bool,
    /// Opt into experimental opaque HTTP transit; materialized relay is the default.
    pub opaque_relay: bool,
    /// Peer-only TCP_NODELAY. Peer exchanges are strictly request/response with
    /// multi-segment signed heads, so Nagle stalls every round on a delayed ACK.
    /// Defaults on; set false only to reproduce the pre-nodelay socket behavior.
    pub peer_tcp_nodelay: bool,
    /// cgroup v2 memory-pressure monitoring and reclaim. Defaults on.
    pub memory_pressure: bool,
    pub pprof_enabled: bool,
    /// Static standalone controller URL. `None` selects managed mode, where
    /// elected endpoints and their pinned certificates come from cluster control.
    pub control_endpoint: Option<String>,
    /// Managed-mode endpoints and report sink, attached after control bootstrap.
    pub managed_control: Option<crate::control::Managed>,
    pub peer_listen: std::net::SocketAddr,
    pub diagnostics_listen: std::net::SocketAddr,
    pub trust_bundle: PathBuf,
    pub service_account_token: PathBuf,
    /// Node-private persistent keys, separate from projected Secrets and slabs.
    pub identity_directory: PathBuf,
    /// Enrollment identity location when it differs from `identity_directory`.
    /// Managed mode binds it to a cluster-scoped subdirectory, leaving any
    /// legacy standalone identity untouched. Rail journals stay in the root.
    pub enrollment_directory: Option<PathBuf>,
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
    /// Origin transport and Azure credential settings.
    pub origin: OriginSettings,
    /// Embedded cluster control settings, present when `RACER_NAMESPACE`
    /// selects managed mode. `main` takes them to start control.
    pub authority: Option<racer_authority::Config>,
}

/// Smallest ciphertext budget for a range window of `window` pages: one page
/// of acquisition progress beyond the window plus one maximum record header.
pub(crate) fn min_ciphertext_bytes(window: u64) -> u64 {
    window * (PAGE_BYTES + 16) + RECORD_BYTES
}

impl Config {
    /// Read the process environment and parse it. Only `main` calls this.
    pub fn from_env() -> std::result::Result<Self, ConfigError> {
        Self::from_map(&environment()?)
    }

    /// Parse an explicit environment. Any `RACER_*` name the table does not
    /// list is rejected.
    pub fn from_map(map: &BTreeMap<String, String>) -> std::result::Result<Self, ConfigError> {
        spec::reject_unknown(TABLE, map.keys().map(String::as_str))?;
        Self::parse(|name| Ok(map.get(name).cloned()))
    }

    /// Parse the table's names through `lookup`. Unlike [`Config::from_map`]
    /// this cannot see names outside the table, so it never rejects unknown
    /// variables; it suits tests that set a few values.
    pub fn from_lookup(
        mut lookup: impl FnMut(&str) -> Result<Option<String>>,
    ) -> std::result::Result<Self, ConfigError> {
        Self::parse(|name| {
            lookup(name).map_err(|error| ConfigError::new(name, format!("unreadable: {error}")))
        })
    }

    fn parse(
        lookup: impl FnMut(&str) -> std::result::Result<Option<String>, ConfigError>,
    ) -> std::result::Result<Self, ConfigError> {
        let v = Values::load(TABLE, lookup, &empty_is_unset)?;
        let u = |name: &str| v.unsigned(name).expect("defaulted");
        let size = |name: &str| to_usize(u(name)).map_err(|_| too_large(&v, name));
        let nonzero = |name: &str| {
            size(name).and_then(|n| {
                NonZeroUsize::new(n)
                    .ok_or_else(|| ConfigError::value(name, "0", "must be positive"))
            })
        };
        let flag = |name: &str| v.boolean(name).expect("defaulted");
        let word = |name: &str| v.text(name).expect("defaulted");
        let path = |name: &str| -> std::result::Result<PathBuf, ConfigError> {
            let text = word(name);
            validate_path(Path::new(text)).map_err(|_| {
                ConfigError::value(name, text, "must be an absolute, canonical path")
            })?;
            Ok(text.into())
        };
        let millis = |name: &str| Duration::from_millis(u(name));
        let secs = |name: &str| Duration::from_secs(u(name));

        // Auto reserves native resources only with usable startup hardware.
        // Explicit true reserves them even when absent, enabling hotplug recovery.
        let enable_rdma = match word("RACER_ENABLE_RDMA") {
            "auto" => {
                cfg!(feature = "rdma")
                    && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
            }
            value => value == "true",
        };
        let enable_gpu = match word("RACER_ENABLE_GPU") {
            "auto" => GpuMode::Auto,
            "true" => GpuMode::Required,
            _ => GpuMode::Disabled,
        };
        let cluster_id = v.text("RACER_CLUSTER_ID").map(str::to_owned);
        let control_endpoint = v.text("RACER_CONTROL_ENDPOINT").map(str::to_owned);
        // Standalone mode needs both static values; managed mode must set neither.
        if cluster_id.is_some() != control_endpoint.is_some() {
            return Err(ConfigError::new(
                "RACER_CLUSTER_ID, RACER_CONTROL_ENDPOINT",
                "set both for standalone control, or neither for managed control",
            ));
        }
        let service_account_token = match v.text("RACER_CONTROL_TOKEN") {
            Some(_) => path("RACER_CONTROL_TOKEN")?,
            None if control_endpoint.is_some() => STANDALONE_SERVICE_ACCOUNT_TOKEN.into(),
            None => MANAGED_SERVICE_ACCOUNT_TOKEN.into(),
        };
        let listener = |name: &str| {
            parse_listener_address(word(name)).map_err(|_| {
                ConfigError::value(name, word(name), "must be a numeric ip:port socket address")
            })
        };

        // Each shared budget comes from its node-wide name or, by default,
        // from its per-worker name, which is scaled later.
        let mut scaled = [false; 16];
        let mut shared = [NonZeroUsize::MIN; 16];
        for (index, (node, worker)) in table::SHARED_BUDGETS.iter().enumerate() {
            match (v.is_set(node), v.is_set(worker)) {
                (true, true) => {
                    return Err(ConfigError::new(
                        format!("{node}, {worker}"),
                        "set either the node-wide total or the per-worker amount, not both",
                    ));
                }
                (true, false) => shared[index] = nonzero(node)?,
                (false, _) => {
                    shared[index] = nonzero(worker)?;
                    scaled[index] = true;
                }
            }
        }
        let [
            plaintext_bytes,
            ciphertext_bytes,
            dirty_bytes,
            registered_bytes,
            request_context_bytes,
            flights,
            queue_entries,
            client_connections,
            peer_connections,
            outbound_connections,
            pipes,
            placement_cache_bytes,
            cached_paths,
            path_cache_bytes,
            metadata_entries,
            relay_transfers,
        ] = shared;
        let limits = Limits {
            plaintext_bytes,
            ciphertext_bytes,
            dirty_bytes,
            registered_bytes,
            request_context_bytes,
            flights,
            waiters_per_flight: nonzero("RACER_WAITERS_PER_FLIGHT")?,
            queue_entries,
            connections_per_neighbor: nonzero("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER")?,
            client_connections,
            peer_connections,
            outbound_connections,
            pipes,
            range_window_pages: nonzero("RACER_RANGE_WINDOW_PAGES")?,
            header_bytes: nonzero("RACER_HEADER_BYTES")?,
            placement_cache_bytes,
            cached_paths,
            path_cache_bytes,
            active_path_searches: nonzero("RACER_ACTIVE_PATH_SEARCHES_PER_WORKER")?,
            retained_snapshots: nonzero("RACER_RETAINED_SNAPSHOTS")?,
            metadata_entries,
            relay_transfers,
        };
        let [
            plaintext_bytes,
            ciphertext_bytes,
            dirty_bytes,
            registered_bytes,
            request_context_bytes,
            flights,
            queue_entries,
            client_connections,
            peer_connections,
            outbound_connections,
            pipes,
            placement_cache_bytes,
            cached_paths,
            path_cache_bytes,
            metadata_entries,
            relay_transfers,
        ] = scaled;
        let registered_bytes_default_cap =
            !v.is_set("RACER_REGISTERED_BYTES") && !v.is_set("RACER_REGISTERED_BYTES_PER_WORKER");
        let scaled_limits = ScaledLimits {
            registered_bytes_default_cap,
            plaintext_bytes,
            ciphertext_bytes,
            dirty_bytes,
            registered_bytes,
            request_context_bytes,
            flights,
            queue_entries,
            client_connections,
            peer_connections,
            outbound_connections,
            pipes,
            placement_cache_bytes,
            cached_paths,
            path_cache_bytes,
            metadata_entries,
            relay_transfers,
        };
        let mut origin_connections_per_cache =
            nonzero("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER")?;
        if !v.is_set("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER") {
            // The default must not reject a smaller explicit client budget.
            origin_connections_per_cache =
                origin_connections_per_cache.min(limits.client_connections);
        }
        let peer_listen = listener("RACER_PEER_LISTEN")?;
        let mut config = Self {
            spill: crate::read::spill::Config {
                retained_percent: size("RACER_RETAINED_SEGMENT_PERCENT")?,
                range_spill_pages: size("RACER_RANGE_SPILL_PAGES")?,
            },
            prefetch: crate::prefetch::Config {
                max_body_bytes: u("RACER_PREFETCH_MAX_BODY_BYTES"),
                max_refs: u("RACER_PREFETCH_MAX_REFS"),
                max_tickets: u("RACER_PREFETCH_MAX_TICKETS"),
                max_ttl_secs: u("RACER_PREFETCH_MAX_TTL_SECS"),
                ticket_retention_secs: u("RACER_PREFETCH_TICKET_RETENTION_SECS"),
                max_wait_secs: u("RACER_PREFETCH_MAX_WAIT_SECS"),
                default_pending_bytes: u("RACER_PREFETCH_DEFAULT_PENDING_BYTES"),
                max_pending_bytes: u("RACER_PREFETCH_MAX_PENDING_BYTES"),
                node_pending_bytes: u("RACER_PREFETCH_NODE_PENDING_BYTES"),
                concurrent_fills: u("RACER_PREFETCH_CONCURRENT_FILLS"),
                origin_bytes_per_sec: u("RACER_PREFETCH_ORIGIN_BYTES_PER_SEC"),
                max_attempts: u("RACER_PREFETCH_MAX_ATTEMPTS"),
            },
            send_tag_pair: v
                .text("RACER_SEND_TAG_PAIR")
                .map(|value| {
                    crate::telemetry::Pair::parse(value).map_err(|_| {
                        ConfigError::value(
                            "RACER_SEND_TAG_PAIR",
                            value,
                            "must be two different canonical node UUIDs, sender,receiver",
                        )
                    })
                })
                .transpose()?,
            page_hedge: crate::read::candidates::HedgeConfig {
                delay: millis("RACER_PAGE_HEDGE_DELAY_MS"),
                slots: size("RACER_PAGE_HEDGE_SLOTS")?,
                bytes: size("RACER_PAGE_HEDGE_BYTES")?,
            },
            replica_selection: match word("RACER_REPLICA_SELECTION") {
                "primary" => crate::read::candidates::ReplicaSelection::Primary,
                _ => crate::read::candidates::ReplicaSelection::Spread,
            },
            peer_admission: crate::peer::Config {
                total: size("RACER_PEER_INFLIGHT_MAX")?,
                per_peer: size("RACER_PEER_PER_NEIGHBOR_MAX")?,
                recovery: millis("RACER_PEER_ADMISSION_RECOVERY_MS"),
            },
            peer_receive: crate::peer::receive::Config {
                active: size("RACER_PEER_RECEIVE_MAX")?,
                queued: size("RACER_PEER_RECEIVE_QUEUE_MAX")?,
                wait: millis("RACER_PEER_RECEIVE_WAIT_MS"),
            },
            shares: std::num::NonZeroU32::new(
                u32::try_from(u("RACER_SHARES")).map_err(|_| too_large(&v, "RACER_SHARES"))?,
            )
            .ok_or_else(|| ConfigError::value("RACER_SHARES", "0", "must be positive"))?,
            disk_page_entries: nonzero("RACER_DISK_PAGE_ENTRIES")?,
            admission_mode: match word("RACER_ADMISSION_MODE") {
                "disabled" => AdmissionMode::Disabled,
                _ => AdmissionMode::SecondSight,
            },
            admission_history_bytes: nonzero("RACER_ADMISSION_HISTORY_BYTES")?,
            admission_period: secs("RACER_ADMISSION_PERIOD_SECS"),
            checkpoint_bytes: nonzero("RACER_CHECKPOINT_BYTES")?,
            cluster: ClusterId(
                cluster_id
                    .clone()
                    .unwrap_or_else(|| UNRESOLVED_CLUSTER_ID.into()),
            ),
            node: NodeId(UNRESOLVED_NODE_ID.into()),
            max_threads: match v.unsigned("RACER_MAX_THREADS") {
                Some(_) => size("RACER_MAX_THREADS")?,
                None => DEFAULT_MAX_THREADS,
            },
            io_workers: v
                .unsigned("RACER_IO_WORKERS")
                .map(|_| size("RACER_IO_WORKERS"))
                .transpose()?,
            crypto_helpers: v
                .unsigned("RACER_CRYPTO_HELPERS")
                .map(|_| size("RACER_CRYPTO_HELPERS"))
                .transpose()?,
            allow_smt: flag("RACER_ALLOW_SMT"),
            enable_rdma,
            rdma_breaker: crate::rdma::breaker::BreakerConfig {
                enabled: flag("RACER_RDMA_BREAKER"),
                base: millis("RACER_RDMA_BREAKER_COOLDOWN_MS"),
                max: millis("RACER_RDMA_BREAKER_MAX_COOLDOWN_MS"),
                page_timeout: millis("RACER_RDMA_PAGE_TIMEOUT_MS"),
            },
            enable_gpu,
            gpu_staging_bytes: u("RACER_GPU_STAGING_BYTES"),
            gpu_landing_bytes: u("RACER_GPU_LANDING_BYTES"),
            gpu_pci_p2pdma: flag("RACER_GPU_PCI_P2PDMA"),
            gpu_peer: word("RACER_GPU_PEER") == "auto",
            opaque_relay: flag("RACER_OPAQUE_RELAY"),
            peer_tcp_nodelay: flag("RACER_PEER_TCP_NODELAY"),
            memory_pressure: flag("RACER_MEMORY_PRESSURE"),
            pprof_enabled: flag("RACER_PPROF_ENABLED"),
            control_endpoint,
            managed_control: None,
            enrollment_directory: None,
            peer_listen,
            diagnostics_listen: listener("RACER_DIAGNOSTICS_LISTEN")?,
            trust_bundle: path("RACER_CONTROL_TRUST_BUNDLE")?,
            service_account_token,
            identity_directory: path("RACER_IDENTITY_DIRECTORY")?,
            slab_directory: path("RACER_SLAB_DIRECTORY")?,
            device_directory: path("RACER_DEVICE_DIRECTORY")?,
            device_busy_wait: secs("RACER_DEVICE_BUSY_WAIT_SECS"),
            slab_bytes: u("RACER_SLAB_BYTES_PER_WORKER"),
            segment_bytes: u("RACER_SEGMENT_BYTES"),
            free_segment_reserve: size("RACER_FREE_SEGMENT_RESERVE_PER_WORKER")?,
            limits,
            scaled_limits,
            origin_connections_per_cache,
            request_timeout: millis("RACER_REQUEST_TIMEOUT_MS"),
            peer_attempt_timeout: millis("RACER_PEER_ATTEMPT_TIMEOUT_MS"),
            reader_stall_timeout: millis("RACER_READER_STALL_TIMEOUT_MS"),
            shutdown_timeout: millis("RACER_SHUTDOWN_TIMEOUT_MS"),
            origin: OriginSettings {
                trust_bundle: path("RACER_ORIGIN_TRUST_BUNDLE")?,
                azure_authority_host: {
                    let host = word("RACER_AZURE_AUTHORITY_HOST");
                    if !host.starts_with("https://") && !host.starts_with("http://") {
                        return Err(ConfigError::value(
                            "RACER_AZURE_AUTHORITY_HOST",
                            host,
                            "must be an https:// URL",
                        ));
                    }
                    host.to_owned()
                },
                azure_federated_token_file: v
                    .text("RACER_AZURE_FEDERATED_TOKEN_FILE")
                    .map(|_| path("RACER_AZURE_FEDERATED_TOKEN_FILE"))
                    .transpose()?,
            },
            authority: None,
        };
        config.authority = authority(&v, &config)?;
        config.validate()?;
        Ok(config)
    }

    /// Multiply every per-worker shared limit by the planned I/O worker count.
    ///
    /// Worker sizing partitions shared limits across lanes, so a per-worker
    /// amount has to be scaled up before that division or each lane receives
    /// only a fraction of its intended budget. Node-wide totals are left alone.
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
        scale(&mut limits.peer_connections, scaled.peer_connections)?;
        scale(
            &mut limits.outbound_connections,
            scaled.outbound_connections,
        )?;
        scale(&mut limits.pipes, scaled.pipes)?;
        scale(
            &mut limits.placement_cache_bytes,
            scaled.placement_cache_bytes,
        )?;
        scale(&mut limits.cached_paths, scaled.cached_paths)?;
        scale(&mut limits.path_cache_bytes, scaled.path_cache_bytes)?;
        scale(&mut limits.metadata_entries, scaled.metadata_entries)?;
        scale(&mut limits.relay_transfers, scaled.relay_transfers)?;
        if scaled.registered_bytes_default_cap {
            let cap = usize::try_from(DEFAULT_REGISTERED_BYTES_CAP).unwrap_or(usize::MAX);
            if limits.registered_bytes.get() > cap {
                limits.registered_bytes =
                    NonZeroUsize::new(cap).ok_or(Error::InvalidConfiguration)?;
            }
        }
        self.limits = scratch;
        self.scaled_limits = ScaledLimits::default();
        Ok(())
    }

    /// Cross-variable rules. Single-variable bounds are already enforced by
    /// the table. Filesystem alignment is additionally discovered and checked
    /// when opening the slabs, not guessed from this config.
    ///
    /// Parsing runs this once; nothing else needs to.
    pub(crate) fn validate(&self) -> std::result::Result<(), ConfigError> {
        let rule = |names: &str, reason: &str| Err(ConfigError::new(names, reason));
        match (self.io_workers, self.crypto_helpers) {
            (None, None) => {}
            (Some(_), None) | (None, Some(_)) => {
                return rule(
                    "RACER_IO_WORKERS, RACER_CRYPTO_HELPERS",
                    "set both exact counts, or neither for automatic sizing",
                );
            }
            (Some(io), Some(helpers)) => {
                if helpers > io {
                    return rule(
                        "RACER_IO_WORKERS, RACER_CRYPTO_HELPERS",
                        "crypto helpers must not outnumber I/O workers",
                    );
                }
                // With RDMA on, one thread of the cap runs the RDMA owner.
                if io
                    .checked_add(helpers)
                    .and_then(|n| n.checked_add(usize::from(self.enable_rdma)))
                    .is_none_or(|n| n > self.max_threads)
                {
                    return rule(
                        "RACER_IO_WORKERS, RACER_CRYPTO_HELPERS, RACER_ENABLE_RDMA, RACER_MAX_THREADS",
                        "I/O workers plus crypto helpers, plus the RDMA owner thread when RDMA is enabled, must fit the thread cap",
                    );
                }
            }
        }
        // Managed mode withholds one thread for control; the rest must still
        // hold an I/O worker, a crypto helper, and the RDMA owner when enabled.
        if self.managed()
            && self.max_threads != DEFAULT_MAX_THREADS
            && self.max_threads < MANAGED_MIN_THREADS + usize::from(self.enable_rdma)
        {
            return rule(
                "RACER_MAX_THREADS",
                "managed deployments need at least 3 threads, or 4 with RDMA enabled",
            );
        }
        if self.prefetch.default_pending_bytes > self.prefetch.max_pending_bytes {
            return rule(
                "RACER_PREFETCH_DEFAULT_PENDING_BYTES, RACER_PREFETCH_MAX_PENDING_BYTES",
                "the default per-ticket bound must not exceed the maximum",
            );
        }
        if self.page_hedge.slots > 0
            && self.page_hedge.bytes < crate::read::candidates::DUPLICATE_BYTES
        {
            return rule(
                "RACER_PAGE_HEDGE_SLOTS, RACER_PAGE_HEDGE_BYTES",
                "hedging needs memory for at least one duplicate (two pages plus 16 bytes)",
            );
        }
        if self.peer_admission.per_peer > self.peer_admission.total {
            return rule(
                "RACER_PEER_PER_NEIGHBOR_MAX, RACER_PEER_INFLIGHT_MAX",
                "the per-neighbor cap must not exceed the in-flight cap",
            );
        }
        // Backstops: the table bounds make these unreachable, but the owning
        // modules keep their own invariants.
        let backstop = |result: Result<()>, names: &str| {
            result.map_err(|_| ConfigError::new(names, "rejected by its subsystem"))
        };
        backstop(
            self.spill.validate(),
            "RACER_RETAINED_SEGMENT_PERCENT, RACER_RANGE_SPILL_PAGES",
        )?;
        backstop(self.prefetch.validate(), "RACER_PREFETCH_*")?;
        backstop(self.page_hedge.validate(), "RACER_PAGE_HEDGE_*")?;
        backstop(self.peer_receive.validate(), "RACER_PEER_RECEIVE_*")?;
        backstop(
            self.peer_admission.validate(),
            "RACER_PEER_INFLIGHT_MAX, RACER_PEER_PER_NEIGHBOR_MAX, RACER_PEER_ADMISSION_RECOVERY_MS",
        )?;
        if self.node.0 != UNRESOLVED_NODE_ID && !valid_uuid(&self.node.0) {
            return Err(ConfigError::value(
                "node identity",
                &self.node.0,
                "must be a canonical, non-nil UUID",
            ));
        }
        if (self.control_endpoint.is_some() || !self.cluster.0.is_empty())
            && !valid_uuid(&self.cluster.0)
        {
            return Err(ConfigError::value(
                "RACER_CLUSTER_ID",
                &self.cluster.0,
                "must be a canonical, non-nil UUID",
            ));
        }
        if let Some(endpoint) = &self.control_endpoint {
            validate_endpoint(endpoint).map_err(|_| {
                ConfigError::value(
                    "RACER_CONTROL_ENDPOINT",
                    endpoint,
                    "must be https://host[:port] with no path, query, or user info",
                )
            })?;
        }
        for (name, address) in [
            ("RACER_PEER_LISTEN", self.peer_listen),
            ("RACER_DIAGNOSTICS_LISTEN", self.diagnostics_listen),
        ] {
            validate_socket(address).map_err(|_| {
                ConfigError::value(
                    name,
                    &address.to_string(),
                    "needs a nonzero port and a unicast address without scope, flow label, or IPv4 mapping",
                )
            })?;
        }
        if self.peer_listen.port() == self.diagnostics_listen.port()
            && (self.peer_listen.ip() == self.diagnostics_listen.ip()
                || self.peer_listen.ip().is_unspecified()
                || self.diagnostics_listen.ip().is_unspecified())
        {
            return rule(
                "RACER_PEER_LISTEN, RACER_DIAGNOSTICS_LISTEN",
                "the listeners overlap",
            );
        }
        let paths = [
            ("RACER_CONTROL_TRUST_BUNDLE", &self.trust_bundle),
            ("RACER_CONTROL_TOKEN", &self.service_account_token),
            ("RACER_IDENTITY_DIRECTORY", &self.identity_directory),
            ("RACER_SLAB_DIRECTORY", &self.slab_directory),
            ("RACER_DEVICE_DIRECTORY", &self.device_directory),
        ];
        // Writable state must not contain or live inside projected credentials.
        // Lexical checks cannot prove mounts/symlinks are distinct; open-time checks
        // remain mandatory and must allow Kubernetes projection symlinks.
        for (index, (left_name, left)) in paths.iter().enumerate() {
            for (right_name, right) in &paths[index + 1..] {
                if left.starts_with(right) || right.starts_with(left) {
                    return rule(
                        &format!("{left_name}, {right_name}"),
                        "paths must not contain each other",
                    );
                }
            }
        }
        if !self.slab_bytes.is_multiple_of(self.segment_bytes) {
            return rule(
                "RACER_SLAB_BYTES_PER_WORKER, RACER_SEGMENT_BYTES",
                "the slab must be a whole number of segments",
            );
        }
        let segments = self.slab_bytes / self.segment_bytes;
        if segments > MAX_ENTRIES || self.free_segment_reserve as u64 >= segments {
            return rule(
                "RACER_SLAB_BYTES_PER_WORKER, RACER_SEGMENT_BYTES, RACER_FREE_SEGMENT_RESERVE_PER_WORKER",
                "a slab needs more segments than the free reserve, and at most 1,048,576",
            );
        }
        let limits = &self.limits;
        if self.origin_connections_per_cache > limits.client_connections {
            return rule(
                "RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER, RACER_CLIENT_CONNECTIONS",
                "origin connections per cache must not exceed client connections",
            );
        }
        let mut bytes = 0u64;
        for limit in [
            limits.plaintext_bytes,
            limits.ciphertext_bytes,
            limits.dirty_bytes,
            limits.registered_bytes,
            limits.request_context_bytes,
        ] {
            bytes = bytes.saturating_add(limit.get() as u64);
        }
        // Node-wide budgets are partitioned after affinity discovery. Integration
        // must recheck progress floors per I/O shard and reduce workers if necessary.
        if bytes > 256 * 1024 * MIB || bytes > isize::MAX as u64 || bytes / 5 > MAX_BYTES {
            return rule(
                "RACER_PLAINTEXT_BYTES, RACER_CIPHERTEXT_BYTES, RACER_DIRTY_BYTES, RACER_REGISTERED_BYTES, RACER_REQUEST_CONTEXT_BYTES",
                "byte budgets must total at most 256 GiB",
            );
        }
        // Sized for the largest page: the cluster page size is published after
        // startup, so local limits must fit any page size it may choose.
        // Keep one page of acquisition progress beyond a full range window. The
        // ciphertext margin also fits one maximum unpadded storage record.
        // Plaintext covers only in-flight decrypts and pages held by range
        // streams; the memory cache retains ciphertext alone.
        let window = limits.range_window_pages.get() as u64;
        let plaintext = (window + 1) * PAGE_BYTES;
        let ciphertext = min_ciphertext_bytes(window);
        if (limits.plaintext_bytes.get() as u64) < plaintext {
            return rule(
                "RACER_PLAINTEXT_BYTES, RACER_RANGE_WINDOW_PAGES",
                "plaintext must fit one page more than the range window",
            );
        }
        if (limits.ciphertext_bytes.get() as u64) < ciphertext {
            return rule(
                "RACER_CIPHERTEXT_BYTES, RACER_RANGE_WINDOW_PAGES",
                "ciphertext must fit the range window plus one stored record",
            );
        }
        if self.enable_rdma && (limits.registered_bytes.get() as u64) < PAGE_BYTES + 16 {
            return rule(
                "RACER_REGISTERED_BYTES, RACER_ENABLE_RDMA",
                "RDMA needs registered memory for at least one page plus 16 bytes",
            );
        }
        if limits.connections_per_neighbor > limits.outbound_connections {
            return rule(
                "RACER_NEIGHBOR_CONNECTIONS_PER_WORKER, RACER_OUTBOUND_CONNECTIONS",
                "neighbor connections must not exceed outbound connections",
            );
        }
        if limits
            .flights
            .get()
            .checked_mul(limits.waiters_per_flight.get())
            .is_none_or(|n| n as u64 > MAX_ENTRIES)
        {
            return rule(
                "RACER_FLIGHTS, RACER_WAITERS_PER_FLIGHT",
                "flights times waiters must be at most 1,048,576",
            );
        }
        if self.rdma_breaker.base > self.rdma_breaker.max {
            return rule(
                "RACER_RDMA_BREAKER_COOLDOWN_MS, RACER_RDMA_BREAKER_MAX_COOLDOWN_MS",
                "the breaker cooldown must not exceed its maximum",
            );
        }
        if self.reader_stall_timeout > self.request_timeout {
            return rule(
                "RACER_READER_STALL_TIMEOUT_MS, RACER_REQUEST_TIMEOUT_MS",
                "the reader stall timeout must not exceed the request timeout",
            );
        }
        Ok(())
    }

    /// Whether elected cluster control, not a static endpoint, provides the controller.
    pub fn managed(&self) -> bool {
        self.control_endpoint.is_none()
    }

    /// Withhold one thread from an explicit cap for the authority thread.
    ///
    /// The control thread is unpinned and mostly blocked on network I/O. With
    /// no explicit cap, worker sizing is already bounded by eligible CPUs, so
    /// nothing is withheld. An explicit cap must leave at least one I/O and one
    /// crypto thread, plus the RDMA owner when RDMA is enabled, so managed mode
    /// requires a cap of at least three, or four with RDMA.
    pub fn reserve_control_thread(&mut self) -> Result<()> {
        if !self.managed() {
            return Err(Error::InvalidConfiguration);
        }
        if self.max_threads == DEFAULT_MAX_THREADS {
            return Ok(());
        }
        if self.max_threads < MANAGED_MIN_THREADS + usize::from(self.enable_rdma) {
            return Err(Error::InvalidConfiguration);
        }
        let reserved = self.max_threads - 1;
        // An exact worker pair must still fit beside the control thread.
        if let (Some(io), Some(helpers)) = (self.io_workers, self.crypto_helpers)
            && io
                .checked_add(helpers)
                .and_then(|n| n.checked_add(usize::from(self.enable_rdma)))
                .is_none_or(|n| n > reserved)
        {
            return Err(Error::InvalidConfiguration);
        }
        self.max_threads = reserved;
        Ok(())
    }

    /// Bind a managed configuration to the cluster returned by control bootstrap.
    ///
    /// The cluster is adopted exactly once; a different later value is refused.
    pub fn adopt_cluster(&mut self, cluster: ClusterId) -> Result<()> {
        if !self.managed() || !valid_uuid(&cluster.0) {
            return Err(Error::InvalidConfiguration);
        }
        if self.cluster.0 != UNRESOLVED_CLUSTER_ID && self.cluster != cluster {
            return Err(Error::Unauthorized);
        }
        self.cluster = cluster;
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

/// How `RACER_ENABLE_GPU` asks for `gpu-read`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GpuMode {
    /// Serve `gpu-read` when built with `cuda` and a usable GPU is found.
    Auto,
    /// Fail startup unless a usable GPU is found.
    Required,
    /// Never serve `gpu-read`; answer 501.
    Disabled,
}

/// Default `RACER_GPU_STAGING_BYTES`: 256 MiB per GPU.
pub const DEFAULT_GPU_STAGING_BYTES: u64 = 256 * 1024 * 1024;

/// Default `RACER_GPU_LANDING_BYTES`: 256 MiB per GPU.
pub const DEFAULT_GPU_LANDING_BYTES: u64 = 256 * 1024 * 1024;

/// GPU staging is allocated in 2 MiB device granules, so the cap must be a
/// whole number of them.
pub const GPU_STAGING_UNIT: u64 = 2 * 1024 * 1024;

/// Read every `RACER_*` variable and every other table name from the process
/// environment. Non-Unicode names are skipped, since no table name can match
/// one; a non-Unicode value of a candidate name is an error naming it.
pub fn environment() -> std::result::Result<BTreeMap<String, String>, ConfigError> {
    let mut map = BTreeMap::new();
    for (name, value) in std::env::vars_os() {
        let Some(name) = name.to_str() else {
            continue;
        };
        if !name.starts_with(spec::PREFIX) && !TABLE.iter().any(|var| var.name == name) {
            continue;
        }
        let value = value
            .into_string()
            .map_err(|_| ConfigError::new(name, "value is not valid UTF-8"))?;
        map.insert(name.to_owned(), value);
    }
    Ok(map)
}

/// Whether an empty value means unset. Kubernetes expands absent field
/// references and optional values to "", so Pod-derived and optional origin
/// settings treat "" as unset; every other variable rejects it.
pub fn empty_is_unset(name: &str) -> bool {
    TABLE.iter().any(|var| {
        var.name == name
            && (var.owner == spec::Owner::Authority
                || matches!(
                    name,
                    "RACER_ORIGIN_TRUST_BUNDLE"
                        | "RACER_AZURE_AUTHORITY_HOST"
                        | "RACER_AZURE_FEDERATED_TOKEN_FILE"
                ))
    })
}

fn too_large(v: &Values, name: &str) -> ConfigError {
    ConfigError::value(
        name,
        &v.shown(name).unwrap_or_default(),
        "does not fit this platform",
    )
}

/// Build embedded cluster control's configuration when `RACER_NAMESPACE`
/// selects managed mode.
fn authority(
    v: &Values,
    config: &Config,
) -> std::result::Result<Option<racer_authority::Config>, ConfigError> {
    use racer_authority::SeatPolicy;
    let Some(namespace) = v.text("RACER_NAMESPACE") else {
        return Ok(None);
    };
    let need = |name: &str| {
        v.text(name).map(str::to_owned).ok_or_else(|| {
            ConfigError::new(
                name,
                "required when RACER_NAMESPACE selects managed control",
            )
        })
    };
    let label = |name: &str, value: &str| {
        let ok = !value.is_empty()
            && value.len() <= 253
            && value
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-' || b == b'.');
        if ok {
            Ok(value.to_owned())
        } else {
            Err(ConfigError::value(
                name,
                value,
                "must be a lowercase DNS name of at most 253 characters",
            ))
        }
    };
    let ip = |name: &str, value: &str| {
        value
            .parse::<IpAddr>()
            .map_err(|_| ConfigError::value(name, value, "must be an IP address"))
    };
    let namespace = label("RACER_NAMESPACE", namespace)?;
    let pod_name = label("RACER_POD_NAME", &need("RACER_POD_NAME")?)?;
    let pod_uid = need("RACER_POD_UID")?;
    if !racer_wire::control::valid_uuid(&pod_uid) {
        return Err(ConfigError::value(
            "RACER_POD_UID",
            &pod_uid,
            "must be a canonical UUID",
        ));
    }
    let node_name = need("RACER_NODE_NAME")?;
    let pod_ip = ip("RACER_POD_IP", &need("RACER_POD_IP")?)?;
    let mut pod_ips = Vec::new();
    let listed = v.text("RACER_POD_IPS").unwrap_or_default();
    for value in listed.split(',').filter(|v| !v.is_empty()) {
        pod_ips.push(ip("RACER_POD_IPS", value)?);
    }
    // Kubernetes reports at most one Pod IP per family, primary first.
    if !pod_ips.is_empty()
        && (pod_ips[0] != pod_ip
            || pod_ips.len() > 2
            || (pod_ips.len() == 2 && pod_ips[0].is_ipv6() == pod_ips[1].is_ipv6()))
    {
        return Err(ConfigError::value(
            "RACER_POD_IPS",
            listed,
            "must start with RACER_POD_IP and hold at most one address per family",
        ));
    }
    let seats = match v.text("RACER_CONTROL_SEATS").unwrap_or("auto") {
        "auto" => SeatPolicy::Auto,
        value => match spec::unsigned(value) {
            Some(n)
                if (1..=racer_authority::annotations::MAX_SEATS as u64).contains(&n)
                    && value == n.to_string() =>
            {
                SeatPolicy::Fixed(n as usize)
            }
            _ => {
                return Err(ConfigError::value(
                    "RACER_CONTROL_SEATS",
                    value,
                    "must be auto or a decimal from 1 to 16 without leading zeros",
                ));
            }
        },
    };
    let address = |name: &str| -> std::result::Result<SocketAddr, ConfigError> {
        let raw = v.text(name).expect("defaulted");
        control_address(raw)
            .ok_or_else(|| ConfigError::value(name, raw, "must be ip:port, [ip]:port, or :port"))
    };
    let control_listen = address("RACER_CONTROL_LISTEN")?;
    let webhook_listen = address("RACER_WEBHOOK_LISTEN")?;
    if control_listen.port() != 0 && control_listen.port() == webhook_listen.port() {
        return Err(ConfigError::new(
            "RACER_CONTROL_LISTEN, RACER_WEBHOOK_LISTEN",
            "the listeners must use different ports",
        ));
    }
    let webhook_alternate = (webhook_listen.ip() == pod_ip)
        .then(|| {
            pod_ips
                .iter()
                .copied()
                .find(|ip| ip.is_ipv6() != pod_ip.is_ipv6())
        })
        .flatten();
    let host = need("KUBERNETES_SERVICE_HOST")?;
    let port = v.unsigned("KUBERNETES_SERVICE_PORT").expect("defaulted");
    let api_server = if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    };
    let control = racer_authority::Config {
        namespace,
        pod_name,
        pod_uid,
        node_name,
        pod_ip,
        seats,
        control_listen,
        webhook_listen,
        webhook_alternate,
        object_image: v.text("RACER_OBJECT_IMAGE").map(str::to_owned),
        api_server,
        api_ca: v.text("RACER_KUBE_API_CA").expect("defaulted").into(),
        api_token: v.text("RACER_KUBE_API_TOKEN").expect("defaulted").into(),
        control_token: config.service_account_token.clone(),
        daemonset: label(
            "RACER_DAEMONSET",
            v.text("RACER_DAEMONSET").expect("defaulted"),
        )?,
        service_account: label(
            "RACER_SERVICE_ACCOUNT",
            v.text("RACER_SERVICE_ACCOUNT").expect("defaulted"),
        )?,
        // Members reach this node's peer listener; publish its port.
        peer_port: config.peer_listen.port(),
        rotation: racer_authority::Rotation {
            retained_page_keys: v.unsigned("RACER_RETAINED_PAGE_KEYS").expect("defaulted") as usize,
            ..Default::default()
        },
        page_size: racer_wire::object::PageSize::new(
            v.unsigned("RACER_PAGE_BYTES").expect("defaulted"),
        )
        .map_err(|_| ConfigError::new("RACER_PAGE_BYTES", "must be a power of two"))?,
    };
    control.validate().map_err(|error| {
        ConfigError::new(
            "RACER_NAMESPACE",
            format!("cluster control rejected the Pod identity: {error}"),
        )
    })?;
    Ok(Some(control))
}

/// Control and webhook listeners also accept `:port` for every IPv4 address.
fn control_address(value: &str) -> Option<SocketAddr> {
    let value = if value.starts_with(':') {
        format!("0.0.0.0{value}")
    } else {
        value.to_owned()
    };
    if let Ok(address) = value.parse() {
        return Some(address);
    }
    let (host, port) = value.strip_prefix('[')?.split_once("]:")?;
    if port.is_empty() || !port.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    Some(SocketAddr::new(host.parse().ok()?, port.parse().ok()?))
}

/// Convert a size without truncating it on smaller platforms.
fn to_usize(value: u64) -> Result<usize> {
    usize::try_from(value).map_err(|_| Error::InvalidConfiguration)
}

fn valid_uuid(value: &str) -> bool {
    racer_wire::control::valid_uuid(value) && value != "00000000-0000-0000-0000-000000000000"
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
    use crate::store::MAX_HEADER_BYTES;
    use racer_wire::control::DEFAULT_SHARES;
    use std::io::Read;

    /// `RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER` default.
    const DEFAULT_ORIGIN_CONNECTIONS_PER_CACHE: usize = 32;

    #[test]
    fn every_table_default_parses_and_names_are_unique() {
        let mut names = std::collections::BTreeSet::new();
        for var in TABLE {
            assert!(names.insert(var.name), "{} listed twice", var.name);
            assert!(
                var.name.starts_with(spec::PREFIX) || var.name.starts_with("KUBERNETES_"),
                "{}",
                var.name
            );
            if let spec::Default::Value(text) = var.default {
                spec::parse(var, text).unwrap_or_else(|e| panic!("default of {}: {e}", var.name));
            }
            if let spec::Kind::Unsigned { min, max, step, .. } = var.kind {
                assert!(min <= max && step > 0, "{}", var.name);
            }
            assert!(
                !var.doc.is_empty() && !var.doc.contains('\u{2014}'),
                "{}",
                var.name
            );
        }
        // The bare defaults parse to a working standalone configuration.
        parse(&[]).unwrap();
    }

    #[test]
    fn defaults_match_the_owning_modules() {
        let config = parse(&[]).unwrap();
        let limits = Limits::default();
        assert_eq!(config.limits, limits);
        assert_eq!(config.prefetch, crate::prefetch::Config::default());
        assert_eq!(config.gpu_staging_bytes, DEFAULT_GPU_STAGING_BYTES);
        assert_eq!(config.gpu_landing_bytes, DEFAULT_GPU_LANDING_BYTES);
        assert_eq!(
            config.page_hedge.bytes,
            crate::read::candidates::DUPLICATE_BYTES
        );
        assert_eq!(
            config.shares.get(),
            racer_wire::control::DEFAULT_SHARES,
            "RACER_SHARES default"
        );
        assert_eq!(config.origin, OriginSettings::default());
    }

    #[test]
    fn unknown_racer_variables_are_rejected_by_name() {
        let error = parse(&[("RACER_MAX_THREAD", "4")]).err().unwrap();
        assert_eq!(error.name, "RACER_MAX_THREAD");
        assert!(error.reason.contains("unknown"), "{error}");
        // Retired and renamed names are unknown too.
        for name in [
            "RACER_WARM_MAX_TTL_SECS",
            "RACER_SLAB_BYTES",
            "RACER_TRUST_BUNDLE",
            "RACER_SERVICE_ACCOUNT_TOKEN",
            "RACER_PEER_PORT",
            "RACER_DST_SEEDS",
        ] {
            assert_eq!(parse(&[(name, "1")]).err().unwrap().name, name);
        }
        // Names without the prefix belong to someone else.
        parse(&[
            ("PATH", "/bin"),
            ("racer_max_threads", "x"),
            ("DST_SEEDS", "1"),
        ])
        .unwrap();
        // The operator's key reaches the Pod through envFrom and is accepted.
        parse(&[("RACER_NVIDIA", "cdi")]).unwrap();
    }

    #[test]
    fn bound_violations_name_the_variable_value_and_reason() {
        for var in TABLE {
            let spec::Kind::Unsigned { min, max, step, .. } = var.kind else {
                continue;
            };
            if max < u64::MAX {
                let value = (max + 1).to_string();
                let error = spec::parse(var, &value).err().unwrap();
                assert_eq!(error.name, var.name);
                assert_eq!(error.value.as_deref(), Some(value.as_str()));
                assert!(error.reason.contains("from"), "{error}");
            }
            if min > 0 {
                let value = (min - 1).to_string();
                assert_eq!(spec::parse(var, &value).err().unwrap().name, var.name);
            }
            if step > 1 {
                assert!(spec::parse(var, &(min + 1).to_string()).is_err());
            }
            assert!(spec::parse(var, "+1").is_err());
        }
        let error = parse(&[("RACER_RANGE_WINDOW_PAGES", "65")]).err().unwrap();
        assert_eq!(
            error.to_string(),
            "RACER_RANGE_WINDOW_PAGES=\"65\": must be from 1 to 64"
        );
        let error = parse(&[("RACER_ENABLE_GPU", "yes")]).err().unwrap();
        assert_eq!(error.name, "RACER_ENABLE_GPU");
        assert!(error.reason.contains("auto, true, false"), "{error}");
        // Cross-variable rules name every variable involved.
        let error = parse(&[("RACER_IO_WORKERS", "2")]).err().unwrap();
        assert_eq!(error.name, "RACER_IO_WORKERS, RACER_CRYPTO_HELPERS");
        // Long values are truncated in messages.
        let long = "a".repeat(4000);
        let error = parse(&[("RACER_SLAB_DIRECTORY", &long)]).err().unwrap();
        assert!(error.to_string().len() < 400, "{error}");
    }

    #[test]
    fn shared_budgets_are_per_worker_by_default_and_node_wide_when_named() {
        let defaults = parse(&[]).unwrap();
        assert_eq!(
            defaults.scaled_limits,
            ScaledLimits {
                plaintext_bytes: true,
                ciphertext_bytes: true,
                dirty_bytes: true,
                registered_bytes: true,
                request_context_bytes: true,
                flights: true,
                queue_entries: true,
                client_connections: true,
                peer_connections: true,
                outbound_connections: true,
                pipes: true,
                placement_cache_bytes: true,
                cached_paths: true,
                path_cache_bytes: true,
                metadata_entries: true,
                relay_transfers: true,
                registered_bytes_default_cap: true,
            }
        );
        // An explicit per-worker amount is still scaled.
        let mut worker = parse(&[("RACER_FLIGHTS_PER_WORKER", "10")]).unwrap();
        assert!(worker.scaled_limits.flights);
        worker.scale_limits(3).unwrap();
        assert_eq!(worker.limits.flights.get(), 30);
        // A node-wide total is never scaled.
        let mut node = parse(&[("RACER_FLIGHTS", "10")]).unwrap();
        assert!(!node.scaled_limits.flights);
        node.scale_limits(3).unwrap();
        assert_eq!(node.limits.flights.get(), 10);
        // The default registered pool scales with workers up to its cap.
        let per_worker = 384 * MIB as usize;
        let mut small = parse(&[]).unwrap();
        small.scale_limits(2).unwrap();
        assert_eq!(small.limits.registered_bytes.get(), 2 * per_worker);
        let mut large = parse(&[]).unwrap();
        large.scale_limits(86).unwrap();
        assert_eq!(
            large.limits.registered_bytes.get() as u64,
            DEFAULT_REGISTERED_BYTES_CAP
        );
        // An explicit per-worker amount or node-wide total is never capped.
        let mut explicit = parse(&[("RACER_REGISTERED_BYTES_PER_WORKER", "402653184")]).unwrap();
        assert!(!explicit.scaled_limits.registered_bytes_default_cap);
        explicit.scale_limits(86).unwrap();
        assert_eq!(explicit.limits.registered_bytes.get(), 86 * per_worker);
        let total = 16 * 1024 * MIB;
        let mut node = parse(&[("RACER_REGISTERED_BYTES", &total.to_string())]).unwrap();
        node.scale_limits(86).unwrap();
        assert_eq!(node.limits.registered_bytes.get() as u64, total);
        // Each pair is a node-wide name and its per-worker name.
        for (node_name, worker_name) in table::SHARED_BUDGETS {
            assert_eq!(format!("{node_name}_PER_WORKER"), worker_name);
            let node_var = TABLE.iter().find(|v| v.name == node_name).unwrap();
            let worker_var = TABLE.iter().find(|v| v.name == worker_name).unwrap();
            assert_eq!(node_var.scope, spec::Scope::Node);
            assert_eq!(worker_var.scope, spec::Scope::PerWorker);
            assert!(matches!(node_var.default, spec::Default::Unset(_)));
            assert!(matches!(worker_var.default, spec::Default::Value(_)));
            let spec::Default::Value(value) = worker_var.default else {
                unreachable!()
            };
            let error = parse(&[(node_name, value), (worker_name, value)])
                .err()
                .unwrap();
            assert_eq!(error.name, format!("{node_name}, {worker_name}"));
        }
        // Per-worker variables carry the suffix; node-wide ones do not.
        for var in TABLE {
            if var.scope == spec::Scope::PerWorker && var.name != "RACER_RETAINED_SEGMENT_PERCENT" {
                assert!(var.name.ends_with("_PER_WORKER"), "{}", var.name);
            }
            if var.name.ends_with("_PER_WORKER") {
                assert_eq!(var.scope, spec::Scope::PerWorker, "{}", var.name);
            }
        }
    }

    #[test]
    fn generated_reference_is_current() {
        // Regenerate with `BLESS=1 cargo test -p racer config::tests::generated_reference_is_current`.
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
        let bless = std::env::var_os("BLESS").is_some();
        let doc = root.join("docs/content/reference/racer.md");
        let begin = "<!-- BEGIN GENERATED: racer configuration table -->\n";
        let end = "<!-- END GENERATED: racer configuration table -->";
        let text = std::fs::read_to_string(&doc).unwrap();
        let start = text.find(begin).expect("begin marker") + begin.len();
        let stop = text.find(end).expect("end marker");
        let expected = format!("\n{}", spec::markdown(TABLE));
        if bless {
            std::fs::write(
                &doc,
                format!("{}{expected}{}", &text[..start], &text[stop..]),
            )
            .unwrap();
        } else {
            assert!(
                text[start..stop] == expected,
                "docs/content/reference/racer.md is stale; rerun with BLESS=1"
            );
        }
        let json = root.join("internal/operator/components/racer/dataplane-settings.json");
        let expected = spec::json(TABLE, &empty_is_unset);
        if bless {
            std::fs::write(&json, &expected).unwrap();
        } else {
            assert!(
                std::fs::read_to_string(&json).unwrap() == expected,
                "{} is stale; rerun with BLESS=1",
                json.display()
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
                    ("RACER_MAX_THREADS", cap),
                    ("RACER_ENABLE_RDMA", "false"),
                ])
                .is_ok(),
                valid
            );
        }
        // With RDMA on, the owner thread takes one thread of the cap.
        for (cap, valid) in [("30", false), ("31", true)] {
            assert_eq!(
                parse(&[
                    ("RACER_IO_WORKERS", "20"),
                    ("RACER_CRYPTO_HELPERS", "10"),
                    ("RACER_MAX_THREADS", cap),
                    ("RACER_ENABLE_RDMA", "true"),
                ])
                .is_ok(),
                valid,
                "{cap}"
            );
        }
        assert!(parse(&[("RACER_IO_WORKERS", "20")]).is_err());
        assert!(parse(&[("RACER_CRYPTO_HELPERS", "20")]).is_err());
    }

    #[test]
    fn text_defaults_and_strict_values() {
        // An empty environment selects managed mode with an unresolved cluster.
        assert!(Config::from_lookup(|_| Ok(None)).unwrap().managed());
        assert_eq!(parse(&[]).unwrap().max_threads, DEFAULT_MAX_THREADS);
        let var = TABLE
            .iter()
            .find(|v| v.name == "RACER_SLAB_DIRECTORY")
            .unwrap();
        assert!(spec::parse(var, "/é").is_ok());
        assert!(spec::parse(var, &format!("/{}", "a".repeat(4095))).is_ok());
        for value in [
            "",
            "/a\n",
            "/\0",
            "/\u{85}",
            &format!("/{}", "a".repeat(4096)),
        ] {
            assert!(spec::parse(var, value).is_err());
        }
        assert!(parse(&[("RACER_ALLOW_SMT", "true")]).unwrap().allow_smt);
        assert!(!parse(&[("RACER_ALLOW_SMT", "false")]).unwrap().allow_smt);
        for value in ["", "TRUE", "1", " false", "false\n"] {
            assert!(parse(&[("RACER_ALLOW_SMT", value)]).is_err());
        }
        assert_eq!(spec::unsigned("000"), Some(0));
        assert_eq!(spec::unsigned("18446744073709551615"), Some(u64::MAX));
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
            assert!(spec::unsigned(value).is_none());
        }
        assert!(parse(&[("RACER_SHARES", "0")]).is_err());
        assert!(parse(&[("RACER_SHARES", &(u32::MAX as u64 + 1).to_string())]).is_err());
        assert_eq!(
            parse(&[("RACER_SHARES", &u32::MAX.to_string())])
                .unwrap()
                .shares
                .get(),
            u32::MAX
        );
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
    fn decimal_range_and_text_bytes_are_preserved() {
        assert_eq!(
            parse(&[("RACER_MAX_THREADS", &u64::MAX.to_string())])
                .unwrap()
                .max_threads as u64,
            u64::MAX
        );
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
        const CHILD: &str = "CONFIG_TEST_CHILD";
        if let Ok(expected) = std::env::var(CHILD) {
            let result = Config::from_env();
            if expected == "valid" {
                let config = result.unwrap();
                assert_eq!(config.max_threads, 7);
                assert!(!config.allow_smt);
            } else {
                assert_eq!(result.err().unwrap().name, expected);
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
                "RACER_ALLOW_SMT",
            ),
            (
                "RACER_SHARES",
                std::ffi::OsString::from_vec(vec![0xff]),
                "RACER_SHARES",
            ),
            ("RACER_SHARES", "".into(), "RACER_SHARES"),
            ("RACER_ALLOW_SMT", "".into(), "RACER_ALLOW_SMT"),
            ("RACER_UNKNOWN", "1".into(), "RACER_UNKNOWN"),
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
        use racer_wire::control::RailId;
        use racer_wire::control::RailMapping;
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
        String::from_utf8(include_bytes!("../wire/testdata/bootstrap-request.json").to_vec())
            .unwrap()
            .replace("\"rdma_nics\":[]", &format!("\"rdma_nics\":{nics}"))
            .into_bytes()
    }
    #[test]
    fn enrollment_nics_reject_malformed_ambiguous_or_unsafe_entries() {
        assert!(
            racer_wire::control::decode_enrollment_request(&nic_request(
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
                racer_wire::control::decode_enrollment_request(&nic_request(nics)).is_err(),
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
            racer_wire::control::decode_enrollment_request(&nic_request(&format!(
                "[{}]",
                entries[..64].join(",")
            )))
            .is_ok()
        );
        assert!(
            racer_wire::control::decode_enrollment_request(&nic_request(&format!(
                "[{}]",
                entries.join(",")
            )))
            .is_err()
        );
        assert!(
            racer_wire::control::decode_enrollment_request(&vec![
                b' ';
                racer_wire::control::MAX_ENROLLMENT_BYTES
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
        let _devices = sim.enter();
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
    fn gpu_mode_and_staging_bytes_parse_and_validate() {
        let config = parse(&[]).unwrap();
        assert_eq!(config.enable_gpu, GpuMode::Auto);
        assert_eq!(config.gpu_staging_bytes, 256 << 20);
        for (value, mode) in [
            ("auto", GpuMode::Auto),
            ("true", GpuMode::Required),
            ("false", GpuMode::Disabled),
        ] {
            assert_eq!(
                parse(&[("RACER_ENABLE_GPU", value)]).unwrap().enable_gpu,
                mode
            );
        }
        for value in ["TRUE", "1", "yes", "", " auto"] {
            assert!(parse(&[("RACER_ENABLE_GPU", value)]).is_err(), "{value:?}");
        }
        assert_eq!(
            parse(&[("RACER_GPU_STAGING_BYTES", "4194304")])
                .unwrap()
                .gpu_staging_bytes,
            4 << 20
        );
        for value in [
            "0",
            "1048576",
            "3145728",
            "-2097152",
            "2MiB",
            "18446744073709551616",
        ] {
            assert!(
                parse(&[("RACER_GPU_STAGING_BYTES", value)]).is_err(),
                "{value:?}"
            );
        }
        assert_eq!(
            parse(&[]).unwrap().gpu_landing_bytes,
            DEFAULT_GPU_LANDING_BYTES
        );
        for (value, bytes) in [("0", 0), ("4194304", 4 << 20)] {
            assert_eq!(
                parse(&[("RACER_GPU_LANDING_BYTES", value)])
                    .unwrap()
                    .gpu_landing_bytes,
                bytes
            );
        }
        for value in ["1048576", "3145728", "-2097152", "2MiB"] {
            assert!(
                parse(&[("RACER_GPU_LANDING_BYTES", value)]).is_err(),
                "{value:?}"
            );
        }
    }

    #[test]
    fn gpu_pci_p2pdma_parses_and_validates() {
        assert!(!parse(&[]).unwrap().gpu_pci_p2pdma);
        for (value, enabled) in [("true", true), ("false", false)] {
            assert_eq!(
                parse(&[("RACER_GPU_PCI_P2PDMA", value)])
                    .unwrap()
                    .gpu_pci_p2pdma,
                enabled
            );
        }
        for value in ["TRUE", "1", "yes", "", " true"] {
            assert!(
                parse(&[("RACER_GPU_PCI_P2PDMA", value)]).is_err(),
                "{value:?}"
            );
        }
    }

    #[test]
    fn gpu_peer_parses_auto_and_off() {
        assert!(parse(&[]).unwrap().gpu_peer);
        assert!(parse(&[("RACER_GPU_PEER", "auto")]).unwrap().gpu_peer);
        assert!(!parse(&[("RACER_GPU_PEER", "off")]).unwrap().gpu_peer);
        for value in ["", "on", "true", "false", "AUTO", "Off"] {
            assert!(parse(&[("RACER_GPU_PEER", value)]).is_err(), "{value:?}");
        }
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

    type Parsed = std::result::Result<Config, ConfigError>;

    fn map(overrides: &[(&str, &str)]) -> BTreeMap<String, String> {
        overrides
            .iter()
            .map(|(key, value)| ((*key).to_owned(), (*value).to_owned()))
            .collect()
    }

    fn parse_managed(overrides: &[(&str, &str)]) -> Parsed {
        Config::from_map(&map(overrides))
    }

    #[test]
    fn managed_mode_omits_static_control_and_leaves_cluster_unresolved() {
        let config = parse_managed(&[]).unwrap();
        assert!(config.managed());
        assert_eq!(config.cluster.0, UNRESOLVED_CLUSTER_ID);
        assert!(config.managed_control.is_none());
        assert_eq!(
            config.service_account_token,
            PathBuf::from(MANAGED_SERVICE_ACCOUNT_TOKEN)
        );
        let explicit = parse(&[]).unwrap();
        assert!(!explicit.managed());
        assert_eq!(
            explicit.service_account_token,
            PathBuf::from("/var/run/secrets/racer-control/token")
        );
        // Half of the standalone pair is ambiguous and rejected.
        for partial in [
            ("RACER_CLUSTER_ID", "00000000-0000-4000-8000-000000000001"),
            ("RACER_CONTROL_ENDPOINT", "https://control.example"),
        ] {
            assert!(parse_managed(&[partial]).is_err(), "{partial:?}");
        }
        // Standalone mode still requires a resolved cluster.
        let mut unresolved = parse(&[]).unwrap();
        unresolved.cluster = ClusterId(UNRESOLVED_CLUSTER_ID.into());
        assert!(unresolved.validate().is_err());
    }

    #[test]
    fn managed_cluster_is_adopted_once() {
        let mut config = parse_managed(&[]).unwrap();
        assert!(
            config
                .adopt_cluster(ClusterId("not-a-uuid".into()))
                .is_err()
        );
        let first = ClusterId("11111111-1111-4111-8111-111111111111".into());
        config.adopt_cluster(first.clone()).unwrap();
        config.validate().unwrap();
        config.adopt_cluster(first.clone()).unwrap();
        assert_eq!(
            config.adopt_cluster(ClusterId("22222222-2222-4222-8222-222222222222".into())),
            Err(Error::Unauthorized)
        );
        assert_eq!(config.cluster, first);
        assert_eq!(
            parse(&[]).unwrap().adopt_cluster(first),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn managed_mode_reserves_one_thread_for_authority() {
        let mut unlimited = parse_managed(&[]).unwrap();
        unlimited.reserve_control_thread().unwrap();
        assert_eq!(unlimited.max_threads, DEFAULT_MAX_THREADS);
        let mut capped = parse_managed(&[("RACER_MAX_THREADS", "3")]).unwrap();
        capped.reserve_control_thread().unwrap();
        assert_eq!(capped.max_threads, 2);
        // Too small a managed cap is a configuration error before startup.
        assert!(parse_managed(&[("RACER_MAX_THREADS", "2")]).is_err());
        // An exact pair that no longer fits after reservation is rejected unchanged.
        let mut exact = parse_managed(&[
            ("RACER_MAX_THREADS", "4"),
            ("RACER_IO_WORKERS", "2"),
            ("RACER_CRYPTO_HELPERS", "2"),
        ])
        .unwrap();
        assert!(exact.reserve_control_thread().is_err());
        assert_eq!(exact.max_threads, 4);
        // The RDMA owner also needs its thread beside the control thread.
        let mut rdma = parse_managed(&[
            ("RACER_MAX_THREADS", "5"),
            ("RACER_IO_WORKERS", "2"),
            ("RACER_CRYPTO_HELPERS", "2"),
            ("RACER_ENABLE_RDMA", "true"),
        ])
        .unwrap();
        assert!(rdma.reserve_control_thread().is_err());
        let mut rdma = parse_managed(&[
            ("RACER_MAX_THREADS", "6"),
            ("RACER_IO_WORKERS", "2"),
            ("RACER_CRYPTO_HELPERS", "2"),
            ("RACER_ENABLE_RDMA", "true"),
        ])
        .unwrap();
        rdma.reserve_control_thread().unwrap();
        assert_eq!(rdma.max_threads, 5);
        assert!(parse(&[]).unwrap().reserve_control_thread().is_err());
    }

    /// The RDMA owner needs a thread beside I/O, crypto, and managed control.
    #[test]
    fn managed_rdma_needs_four_threads() {
        let Err(error) =
            parse_managed(&[("RACER_MAX_THREADS", "3"), ("RACER_ENABLE_RDMA", "true")])
        else {
            panic!("managed RDMA with three threads must be rejected");
        };
        assert_eq!(error.name, "RACER_MAX_THREADS");
        let mut rdma =
            parse_managed(&[("RACER_MAX_THREADS", "4"), ("RACER_ENABLE_RDMA", "true")]).unwrap();
        rdma.reserve_control_thread().unwrap();
        assert_eq!(rdma.max_threads, 3);
        let mut plain =
            parse_managed(&[("RACER_MAX_THREADS", "3"), ("RACER_ENABLE_RDMA", "false")]).unwrap();
        plain.reserve_control_thread().unwrap();
        assert_eq!(plain.max_threads, 2);
    }

    fn parse(overrides: &[(&str, &str)]) -> Parsed {
        let mut values = map(&[
            ("RACER_CLUSTER_ID", "00000000-0000-4000-8000-000000000001"),
            ("RACER_CONTROL_ENDPOINT", "https://control.example:7443"),
        ]);
        values.extend(map(overrides));
        Config::from_map(&values)
    }
    #[test]
    fn spill_defaults_and_environment_limits_are_bounded() {
        let spill = parse(&[]).unwrap().spill;
        assert_eq!((spill.retained_percent, spill.range_spill_pages), (50, 0));
        let spill = parse(&[
            ("RACER_RETAINED_SEGMENT_PERCENT", "100"),
            ("RACER_RANGE_SPILL_PAGES", "1024"),
        ])
        .unwrap()
        .spill;
        assert_eq!(
            (spill.retained_percent, spill.range_spill_pages),
            (100, 1024)
        );
        for (name, value) in [
            ("RACER_RETAINED_SEGMENT_PERCENT", "0"),
            ("RACER_RETAINED_SEGMENT_PERCENT", "101"),
            ("RACER_RANGE_SPILL_PAGES", "1025"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
    }

    #[test]
    fn prefetch_defaults_and_bounds() {
        let prefetch = parse(&[]).unwrap().prefetch;
        assert_eq!(prefetch, crate::prefetch::Config::default());
        assert_eq!(prefetch.max_body_bytes, 4194304);
        assert_eq!(prefetch.max_refs, 16384);
        assert_eq!(prefetch.max_tickets, 1024);
        assert_eq!(prefetch.max_ttl_secs, 3600);
        assert_eq!(prefetch.ticket_retention_secs, 300);
        assert_eq!(prefetch.max_wait_secs, 60);
        assert_eq!(prefetch.default_pending_bytes, 1 << 30);
        assert_eq!(prefetch.max_pending_bytes, 16 << 30);
        assert_eq!(prefetch.node_pending_bytes, 64 << 30);
        assert_eq!(prefetch.concurrent_fills, 16);
        assert_eq!(prefetch.origin_bytes_per_sec, 0);
        assert_eq!(prefetch.max_attempts, 5);
        // Inclusive bounds are accepted.
        for (name, value) in [
            ("RACER_PREFETCH_MAX_BODY_BYTES", "4096"),
            ("RACER_PREFETCH_MAX_BODY_BYTES", "67108864"),
            ("RACER_PREFETCH_MAX_REFS", "1"),
            ("RACER_PREFETCH_MAX_REFS", "1048576"),
            ("RACER_PREFETCH_MAX_TICKETS", "65536"),
            ("RACER_PREFETCH_MAX_TTL_SECS", "86400"),
            ("RACER_PREFETCH_TICKET_RETENTION_SECS", "0"),
            ("RACER_PREFETCH_MAX_WAIT_SECS", "600"),
            ("RACER_PREFETCH_DEFAULT_PENDING_BYTES", "16777216"),
            ("RACER_PREFETCH_MAX_PENDING_BYTES", "1125899906842624"),
            ("RACER_PREFETCH_NODE_PENDING_BYTES", "4503599627370496"),
            ("RACER_PREFETCH_CONCURRENT_FILLS", "1024"),
            ("RACER_PREFETCH_ORIGIN_BYTES_PER_SEC", "1099511627776"),
            ("RACER_PREFETCH_MAX_ATTEMPTS", "32"),
        ] {
            assert!(parse(&[(name, value)]).is_ok(), "{name}={value}");
        }
        for (name, value) in [
            ("RACER_PREFETCH_MAX_BODY_BYTES", "4095"),
            ("RACER_PREFETCH_MAX_BODY_BYTES", "67108865"),
            ("RACER_PREFETCH_MAX_REFS", "0"),
            ("RACER_PREFETCH_MAX_REFS", "1048577"),
            ("RACER_PREFETCH_MAX_TICKETS", "0"),
            ("RACER_PREFETCH_MAX_TICKETS", "65537"),
            ("RACER_PREFETCH_MAX_TTL_SECS", "0"),
            ("RACER_PREFETCH_MAX_TTL_SECS", "86401"),
            ("RACER_PREFETCH_TICKET_RETENTION_SECS", "86401"),
            ("RACER_PREFETCH_MAX_WAIT_SECS", "0"),
            ("RACER_PREFETCH_MAX_WAIT_SECS", "601"),
            ("RACER_PREFETCH_DEFAULT_PENDING_BYTES", "16777215"),
            ("RACER_PREFETCH_MAX_PENDING_BYTES", "16777215"),
            ("RACER_PREFETCH_MAX_PENDING_BYTES", "1125899906842625"),
            ("RACER_PREFETCH_NODE_PENDING_BYTES", "16777215"),
            ("RACER_PREFETCH_NODE_PENDING_BYTES", "4503599627370497"),
            ("RACER_PREFETCH_CONCURRENT_FILLS", "0"),
            ("RACER_PREFETCH_CONCURRENT_FILLS", "1025"),
            ("RACER_PREFETCH_ORIGIN_BYTES_PER_SEC", "1099511627777"),
            ("RACER_PREFETCH_MAX_ATTEMPTS", "0"),
            ("RACER_PREFETCH_MAX_ATTEMPTS", "33"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        // The default per-ticket bound may not exceed the per-ticket cap.
        assert!(
            parse(&[
                ("RACER_PREFETCH_MAX_PENDING_BYTES", "33554432"),
                ("RACER_PREFETCH_DEFAULT_PENDING_BYTES", "67108864"),
            ])
            .is_err()
        );
        let prefetch = parse(&[
            ("RACER_PREFETCH_MAX_PENDING_BYTES", "33554432"),
            ("RACER_PREFETCH_DEFAULT_PENDING_BYTES", "16777216"),
        ])
        .unwrap()
        .prefetch;
        assert_eq!(prefetch.max_pending_bytes, 33554432);
    }

    #[test]
    fn send_tag_pair_is_opt_in_and_validated() {
        assert!(parse(&[]).unwrap().send_tag_pair.is_none());
        let valid = "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111";
        assert!(
            parse(&[("RACER_SEND_TAG_PAIR", valid)])
                .unwrap()
                .send_tag_pair
                .is_some()
        );
        for invalid in [
            "",
            "a,b",
            "a,b,c",
            "8816d91d-e896-49bf-ba8a-da97ede93818,8816d91d-e896-49bf-ba8a-da97ede93818",
        ] {
            assert!(parse(&[("RACER_SEND_TAG_PAIR", invalid)]).is_err());
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
        assert!(parse(&[("RACER_ADMISSION_PERIOD_SECS", "0")]).is_err());
        assert!(parse(&[("RACER_ADMISSION_HISTORY_BYTES", "31")]).is_err());
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
            let policy = crate::retention::Retention::configured(
                share,
                config.admission_period,
                true,
                racer_io::Clock::host(),
            )
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
        assert_eq!(config.limits.connections_per_neighbor.get(), 8);
        config.validate().unwrap();
        crate::app::Application::assemble(config).unwrap();
        assert!(parse(&[("RACER_ENABLE_RDMA", "true"), ("RACER_MAX_THREADS", "7")]).is_ok());
    }

    type LimitField = (&'static str, fn(&Limits) -> NonZeroUsize);

    const SCALED_FIELDS: [LimitField; 16] = [
        ("RACER_PLAINTEXT_BYTES", |l| l.plaintext_bytes),
        ("RACER_CIPHERTEXT_BYTES", |l| l.ciphertext_bytes),
        ("RACER_DIRTY_BYTES", |l| l.dirty_bytes),
        ("RACER_REGISTERED_BYTES", |l| l.registered_bytes),
        ("RACER_REQUEST_CONTEXT_BYTES", |l| l.request_context_bytes),
        ("RACER_FLIGHTS", |l| l.flights),
        ("RACER_QUEUE_ENTRIES", |l| l.queue_entries),
        ("RACER_CLIENT_CONNECTIONS", |l| l.client_connections),
        ("RACER_PEER_CONNECTIONS", |l| l.peer_connections),
        ("RACER_OUTBOUND_CONNECTIONS", |l| l.outbound_connections),
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
                let mut expected = field(&before).get() * workers;
                if name == "RACER_REGISTERED_BYTES" {
                    expected = expected.min(DEFAULT_REGISTERED_BYTES_CAP as usize);
                }
                assert_eq!(field(&config.limits).get(), expected, "{name}");
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
                "RACER_PEER_CONNECTIONS" => config.scaled_limits.peer_connections = true,
                "RACER_OUTBOUND_CONNECTIONS" => {
                    config.scaled_limits.outbound_connections = true;
                }
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
            ("RACER_PEER_ADMISSION_RECOVERY_MS", "10"),
        ])
        .unwrap();
        assert_eq!(config.peer_admission.total, 8);
        assert_eq!(config.peer_admission.per_peer, 2);
        assert_eq!(config.peer_admission.recovery, Duration::from_millis(10));
        assert_eq!(
            parse(&[]).unwrap().peer_admission.recovery,
            Duration::from_secs(1)
        );
        for overrides in [
            vec![("RACER_PEER_INFLIGHT_MAX", "0")],
            vec![("RACER_PEER_INFLIGHT_MAX", "65537")],
            vec![("RACER_PEER_PER_NEIGHBOR_MAX", "0")],
            vec![("RACER_PEER_PER_NEIGHBOR_MAX", "4097")],
            vec![("RACER_PEER_ADMISSION_RECOVERY_MS", "0")],
            vec![("RACER_PEER_ADMISSION_RECOVERY_MS", "60001")],
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
            assert_eq!(parse(&[(name, value)]).err().unwrap().name, name);
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
            ("RACER_ACTIVE_PATH_SEARCHES_PER_WORKER", "2"),
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
            ("RACER_ACTIVE_PATH_SEARCHES_PER_WORKER", "0"),
            ("RACER_ACTIVE_PATH_SEARCHES_PER_WORKER", "65"),
            ("RACER_CACHED_PATHS", "0"),
            ("RACER_CACHED_PATHS", "1048577"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        assert!(parse(&[("RACER_PLACEMENT_CACHE_BYTES", "1023")]).is_err());
        parse(&[("RACER_PLACEMENT_CACHE_BYTES", "1024")]).unwrap();
        parse(&[
            ("RACER_PATH_CACHE_BYTES", "1"),
            ("RACER_ACTIVE_PATH_SEARCHES_PER_WORKER", "64"),
        ])
        .unwrap();
        drop(defaults);
    }

    #[test]
    fn smt_requires_an_exact_explicit_boolean() {
        exact_boolean("RACER_ALLOW_SMT", |c| c.allow_smt);
    }

    #[test]
    fn origin_connection_cap_is_independent_and_validated() {
        let config = parse(&[
            ("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER", "16"),
            ("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "1"),
        ])
        .unwrap();
        assert_eq!(config.origin_connections_per_cache.get(), 16);
        assert_eq!(config.limits.connections_per_neighbor.get(), 1);
        for value in ["", "0", "1025", "129", "-1", "1 ", "many"] {
            assert!(
                parse(&[("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER", value)]).is_err(),
                "{value}"
            );
        }
        assert!(
            parse(&[
                ("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER", "1024"),
                ("RACER_CLIENT_CONNECTIONS", "1024")
            ])
            .is_ok()
        );
        let small = parse(&[("RACER_CLIENT_CONNECTIONS", "12")]).unwrap();
        assert_eq!(small.origin_connections_per_cache.get(), 12);
        assert!(
            parse(&[
                ("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER", "13"),
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
        let explicit = parse(&[("RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER", "64")]).unwrap();
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
        // Without static control, the cluster stays unresolved until bootstrap.
        assert_eq!(
            Config::from_lookup(|_| Ok(None)).unwrap().cluster.0,
            UNRESOLVED_CLUSTER_ID
        );
        assert_eq!(
            Config::from_lookup(|_| Err(Error::InvalidConfiguration))
                .err()
                .unwrap()
                .name,
            "RACER_CLUSTER_ID"
        );
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
        assert!(config.validate().is_err());
        config.node = NodeId("00000000-0000-4000-8000-000000000002".into());
        config.validate().unwrap();
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
            "RACER_CONTROL_TRUST_BUNDLE",
            "RACER_CONTROL_TOKEN",
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
        assert!(
            parse(&[(
                "RACER_CONTROL_TRUST_BUNDLE",
                &format!("/{}", "a".repeat(256))
            )])
            .is_err()
        );
    }

    #[test]
    fn geometry_includes_aead_and_envelope_and_preserves_a_writable_segment() {
        let minimum = PAGE_BYTES + 16 + MAX_HEADER_BYTES as u64;
        let geometry = |segment: u64| {
            parse(&[
                ("RACER_SEGMENT_BYTES", &segment.to_string()),
                ("RACER_SLAB_BYTES_PER_WORKER", &(segment * 3).to_string()),
            ])
        };
        geometry(minimum).unwrap();
        assert_eq!(
            geometry(minimum - 1).err().unwrap().name,
            "RACER_SEGMENT_BYTES"
        );
        for (name, value) in [
            ("RACER_SEGMENT_BYTES", "0"),
            ("RACER_SLAB_BYTES_PER_WORKER", "0"),
            ("RACER_SEGMENT_BYTES", "16777216"),
            ("RACER_SLAB_BYTES_PER_WORKER", "1073741825"),
            ("RACER_FREE_SEGMENT_RESERVE_PER_WORKER", "0"),
            ("RACER_FREE_SEGMENT_RESERVE_PER_WORKER", "16"),
            ("RACER_SLAB_BYTES_PER_WORKER", "18446744073709551615"),
            (
                "RACER_FREE_SEGMENT_RESERVE_PER_WORKER",
                "18446744073709551615",
            ),
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
            ("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "129"),
            ("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "65"),
            ("RACER_PEER_CONNECTIONS_PER_WORKER", "0"),
            ("RACER_PEER_CONNECTIONS", "65537"),
            ("RACER_OUTBOUND_CONNECTIONS_PER_WORKER", "0"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        // Neighbor connections are bounded by outbound connections, not client connections.
        assert!(parse(&[("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "64")]).is_ok());
        let wide = parse(&[
            ("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "65"),
            ("RACER_OUTBOUND_CONNECTIONS_PER_WORKER", "128"),
            ("RACER_CLIENT_CONNECTIONS_PER_WORKER", "8"),
        ])
        .unwrap();
        assert_eq!(wide.limits.outbound_connections.get(), 128);
        assert_eq!(wide.limits.peer_connections.get(), 2048);
        assert!(
            parse(&[
                ("RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "9"),
                ("RACER_OUTBOUND_CONNECTIONS_PER_WORKER", "8"),
            ])
            .is_err()
        );
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
        config.validate().unwrap();
        config.limits.ciphertext_bytes =
            NonZeroUsize::new(config.limits.ciphertext_bytes.get() - 1).unwrap();
        assert!(config.validate().is_err());
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
    fn rdma_breaker_defaults_off_and_cooldowns_are_ordered() {
        let config = parse(&[]).unwrap();
        assert_eq!(
            config.rdma_breaker,
            crate::rdma::breaker::BreakerConfig {
                enabled: false,
                ..crate::rdma::breaker::BreakerConfig::default()
            }
        );
        let tuned = parse(&[
            ("RACER_RDMA_BREAKER", "true"),
            ("RACER_RDMA_BREAKER_COOLDOWN_MS", "500"),
            ("RACER_RDMA_BREAKER_MAX_COOLDOWN_MS", "500"),
            ("RACER_RDMA_PAGE_TIMEOUT_MS", "750"),
        ])
        .unwrap();
        assert!(tuned.rdma_breaker.enabled);
        assert_eq!(tuned.rdma_breaker.base, Duration::from_millis(500));
        assert_eq!(tuned.rdma_breaker.max, Duration::from_millis(500));
        assert_eq!(tuned.rdma_breaker.page_timeout, Duration::from_millis(750));
        for port in ["1", "18515", "65535"] {
            assert!(parse(&[("RACER_RDMA_CM_PORT", port)]).is_ok(), "{port}");
        }
        for (name, value) in [
            ("RACER_RDMA_PAGE_TIMEOUT_MS", "0"),
            ("RACER_RDMA_PAGE_TIMEOUT_MS", "60001"),
            ("RACER_RDMA_BREAKER_COOLDOWN_MS", "0"),
            ("RACER_RDMA_BREAKER_COOLDOWN_MS", "30001"),
            ("RACER_RDMA_CM_PORT", "0"),
            ("RACER_RDMA_CM_PORT", "65536"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
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
        config.validate().unwrap();
        for value in ["0", "86400001"] {
            assert_eq!(
                parse(&[("RACER_PEER_ATTEMPT_TIMEOUT_MS", value)])
                    .err()
                    .unwrap()
                    .name,
                "RACER_PEER_ATTEMPT_TIMEOUT_MS"
            );
        }
        config.reader_stall_timeout = Duration::from_millis(11);
        assert!(config.validate().is_err());
    }

    // Embedded cluster control settings (ported from the authority crate's own
    // environment parser, which this table replaced).

    fn managed_env(extra: &[(&str, &str)]) -> Parsed {
        let mut values = map(&[
            ("RACER_NAMESPACE", "racer-system"),
            ("RACER_POD_NAME", "racer-abc"),
            ("RACER_POD_UID", "11111111-1111-4111-8111-111111111111"),
            ("RACER_NODE_NAME", "node-a"),
            ("RACER_POD_IP", "10.0.0.5"),
            ("KUBERNETES_SERVICE_HOST", "10.96.0.1"),
        ]);
        for (key, value) in extra {
            values.insert((*key).into(), (*value).into());
        }
        Config::from_map(&values)
    }

    fn control(extra: &[(&str, &str)]) -> racer_authority::Config {
        managed_env(extra).unwrap().authority.unwrap()
    }

    #[test]
    fn authority_is_absent_without_a_namespace() {
        assert!(parse_managed(&[]).unwrap().authority.is_none());
        assert!(parse(&[]).unwrap().authority.is_none());
    }

    #[test]
    fn authority_defaults() {
        let config = managed_env(&[]).unwrap();
        let c = config.authority.as_ref().unwrap();
        assert_eq!(c.seats, racer_authority::SeatPolicy::Auto);
        assert_eq!(c.control_listen.port(), 8443);
        assert_eq!(c.webhook_listen.port(), 9443);
        assert_eq!(c.api_server, "10.96.0.1:443");
        assert!(c.object_image.is_none());
        assert_eq!(c.daemonset, "racer");
        // One control token serves the dataplane and control seats.
        assert_eq!(c.control_token, config.service_account_token);
        assert_eq!(
            c.control_token,
            PathBuf::from(MANAGED_SERVICE_ACCOUNT_TOKEN)
        );
        // The published peer port is the peer listener's port.
        assert_eq!(c.peer_port, config.peer_listen.port());
        let c = control(&[("RACER_PEER_LISTEN", "[10.0.0.5]:8082")]);
        assert_eq!(c.peer_port, 8082);
        // Kubernetes may expand absent fields to empty strings.
        let c = control(&[("RACER_OBJECT_IMAGE", ""), ("RACER_POD_IPS", "")]);
        assert!(c.object_image.is_none());
        assert!(c.webhook_alternate.is_none());
    }

    #[test]
    fn authority_retained_page_keys() {
        let c = control(&[]);
        assert_eq!(
            c.rotation.retained_page_keys,
            racer_authority::rules::keyring::DEFAULT_RETAINED_PAGE_KEYS
        );
        assert_eq!(c.rotation, racer_authority::Rotation::default());
        for (value, want) in [("1", 1), ("16", 16)] {
            let c = control(&[("RACER_RETAINED_PAGE_KEYS", value)]);
            assert_eq!(c.rotation.retained_page_keys, want);
        }
        for bad in ["0", "17", "x"] {
            assert_eq!(
                managed_env(&[("RACER_RETAINED_PAGE_KEYS", bad)])
                    .err()
                    .unwrap()
                    .name,
                "RACER_RETAINED_PAGE_KEYS"
            );
        }
    }

    #[test]
    fn authority_page_bytes() {
        assert_eq!(
            control(&[]).page_size,
            racer_wire::object::PageSize::DEFAULT
        );
        let c = control(&[("RACER_PAGE_BYTES", "16777216")]);
        assert_eq!(c.page_size.get(), 16 * 1024 * 1024);
        // The SDKs and disk index assume 16 MiB pages; other sizes are refused.
        for bad in ["4096", "65536", "1048576", "98304", "33554432", "x"] {
            assert_eq!(
                managed_env(&[("RACER_PAGE_BYTES", bad)])
                    .err()
                    .unwrap()
                    .name,
                "RACER_PAGE_BYTES"
            );
        }
    }

    #[test]
    fn authority_seats_and_ipv6_api() {
        let c = control(&[
            ("RACER_CONTROL_SEATS", "5"),
            ("KUBERNETES_SERVICE_HOST", "fd00::1"),
        ]);
        assert_eq!(c.seats, racer_authority::SeatPolicy::Fixed(5));
        assert_eq!(c.api_server, "[fd00::1]:443");
        for bad in ["0", "17", "05", "x"] {
            assert_eq!(
                managed_env(&[("RACER_CONTROL_SEATS", bad)])
                    .err()
                    .unwrap()
                    .name,
                "RACER_CONTROL_SEATS"
            );
        }
    }

    #[test]
    fn authority_listeners_accept_both_pod_ip_families() {
        for ip in ["10.244.2.3", "fd00:10:244::3"] {
            let control_listen = format!("[{ip}]:8443");
            let webhook = format!("[{ip}]:9443");
            let c = control(&[
                ("RACER_POD_IP", ip),
                ("RACER_CONTROL_LISTEN", &control_listen),
                ("RACER_WEBHOOK_LISTEN", &webhook),
            ]);
            let ip: IpAddr = ip.parse().unwrap();
            assert_eq!(c.control_listen, SocketAddr::new(ip, 8443));
            assert_eq!(c.webhook_listen, SocketAddr::new(ip, 9443));
        }
    }

    #[test]
    fn authority_webhook_also_serves_the_other_pod_ip_family() {
        let webhook = "[10.0.0.5]:9443";
        let c = |ips: &str, listen: &str| {
            managed_env(&[("RACER_POD_IPS", ips), ("RACER_WEBHOOK_LISTEN", listen)])
        };
        let alt = |ips, listen| c(ips, listen).unwrap().authority.unwrap().webhook_alternate;
        assert_eq!(
            alt("10.0.0.5,fd00::5", webhook),
            Some("fd00::5".parse().unwrap())
        );
        // Single stack, absent, or not listening on the Pod IP: none.
        assert_eq!(alt("10.0.0.5", webhook), None);
        assert_eq!(alt("10.0.0.5,fd00::5", "127.0.0.1:9443"), None);
        // Malformed, not led by status.podIP, duplicate, or same family: rejected.
        for bad in [
            "10.0.0.5,nope",
            "10.0.0.6,fd00::5",
            "fd00::5,10.0.0.5",
            "10.0.0.5,10.0.0.5",
            "10.0.0.5,10.0.0.6",
            "10.0.0.5,fd00::5,fd00::6",
        ] {
            assert_eq!(
                c(bad, webhook).err().unwrap().name,
                "RACER_POD_IPS",
                "{bad}"
            );
        }
    }

    #[test]
    fn authority_listener_address_validation() {
        for address in ["127.0.0.1:0", "[127.0.0.1]:0", "[::1]:0", ":0"] {
            let c = control(&[("RACER_CONTROL_LISTEN", address)]);
            assert_eq!(c.control_listen.port(), 0);
        }
        for bad in [
            "[10.244.2.3:8443",
            "10.244.2.3]:8443",
            "[10.244.2.3]:+8443",
            "[10.244.2.3]:8443extra",
            "[10.244.2.3]:65536",
            "[10.244.2.3]:",
            "[not-an-ip]:8443",
            "[]:8443",
            "fd00:10:244::3:8443",
        ] {
            for name in ["RACER_CONTROL_LISTEN", "RACER_WEBHOOK_LISTEN"] {
                assert_eq!(
                    managed_env(&[(name, bad)]).err().unwrap().name,
                    name,
                    "{name}={bad}"
                );
            }
        }
    }

    #[test]
    fn authority_missing_identity_fails_by_name() {
        let mut values = map(&[
            ("RACER_NAMESPACE", "racer-system"),
            ("RACER_POD_NAME", "racer-abc"),
            ("RACER_NODE_NAME", "node-a"),
            ("RACER_POD_IP", "10.0.0.5"),
            ("KUBERNETES_SERVICE_HOST", "10.96.0.1"),
        ]);
        assert_eq!(
            Config::from_map(&values).err().unwrap().name,
            "RACER_POD_UID"
        );
        values.insert("RACER_POD_UID".into(), "not-a-uid".into());
        assert_eq!(
            Config::from_map(&values).err().unwrap().name,
            "RACER_POD_UID"
        );
        assert_eq!(
            managed_env(&[("RACER_POD_IP", "nope")]).err().unwrap().name,
            "RACER_POD_IP"
        );
        assert_eq!(
            managed_env(&[("RACER_NAMESPACE", "Upper")])
                .err()
                .unwrap()
                .name,
            "RACER_NAMESPACE"
        );
        assert_eq!(
            managed_env(&[("KUBERNETES_SERVICE_PORT", "x")])
                .err()
                .unwrap()
                .name,
            "KUBERNETES_SERVICE_PORT"
        );
    }

    #[test]
    fn origin_settings_come_from_the_table() {
        let config = parse(&[
            ("RACER_ORIGIN_TRUST_BUNDLE", "/etc/origin.pem"),
            ("RACER_AZURE_AUTHORITY_HOST", "https://login.example/"),
            ("RACER_AZURE_FEDERATED_TOKEN_FILE", "/var/run/azure/token"),
        ])
        .unwrap();
        assert_eq!(
            config.origin,
            OriginSettings {
                trust_bundle: "/etc/origin.pem".into(),
                azure_authority_host: "https://login.example/".into(),
                azure_federated_token_file: Some("/var/run/azure/token".into()),
            }
        );
        // Empty selects the default, as Kubernetes may expand absent values.
        let config = parse(&[
            ("RACER_ORIGIN_TRUST_BUNDLE", ""),
            ("RACER_AZURE_AUTHORITY_HOST", ""),
            ("RACER_AZURE_FEDERATED_TOKEN_FILE", ""),
        ])
        .unwrap();
        assert_eq!(config.origin, OriginSettings::default());
        for (name, value) in [
            ("RACER_ORIGIN_TRUST_BUNDLE", "relative.pem"),
            ("RACER_AZURE_AUTHORITY_HOST", "login.example/"),
            ("RACER_AZURE_FEDERATED_TOKEN_FILE", "token"),
        ] {
            assert_eq!(parse(&[(name, value)]).err().unwrap().name, name);
        }
    }

    #[test]
    fn markdown_and_json_cover_every_variable() {
        let markdown = spec::markdown(TABLE);
        let json = spec::json(TABLE, &empty_is_unset);
        for var in TABLE {
            assert!(
                markdown.contains(&format!("| `{}` |", var.name)),
                "{}",
                var.name
            );
            assert!(
                json.contains(&format!("\"name\": \"{}\"", var.name)),
                "{}",
                var.name
            );
        }
        assert!(!markdown.contains('\u{2014}'));
    }
}
