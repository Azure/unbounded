// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! libfabric class lifecycle for the connection-managed `FI_EP_MSG`
//! transport: bring up a fabric and domain, a passive listening
//! endpoint (when configured to listen), a per-NUMA progress group,
//! and the inbound dispatch that routes received frames. Active
//! per-peer endpoints are created on demand by `add_connection`
//! (outbound dials) and by the accept loop (inbound), not here.
//!
//! Both providers (`verbs` and `tcp`) speak native `FI_EP_MSG`; there
//! is no address vector and no tagged/RDM path. Remote addressing is by
//! ordinary socket addresses through RDMA CM/libfabric, with a native
//! raw-address escape hatch for deployments that cannot use socket
//! addressing on their RDMA fabric.

use std::collections::{HashMap, HashSet};
use std::ffi::CString;
use std::net::SocketAddr;
use std::ptr;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::Duration;

use super::backing::LocalMrCtx;
use super::cm;
use super::completion::CompletionRegistry;
use super::config::{FabricConfig, Provider};
use super::connection::{ConnectionTable, Dialer, install_connection};
use super::dispatch::InboundDispatch;
use super::error::{FabricError, Result, check};
use super::ffi;
use super::progress::ProgressGroup;
use super::sendpool::SendPool;
use super::types::{ConnectionSpec, PeerId};

/// Live libfabric stack for one transport instance.
pub struct Fabric {
    inner: Arc<FabricInner>,
}

pub(crate) struct FabricInner {
    /// Primary `info` from `fi_getinfo` (with `FI_SOURCE` + the bound
    /// node/service when listening). Kept alive for the fabric's
    /// lifetime so the sub-attribute pointers handed to `fi_fabric` /
    /// `fi_domain` and the passive endpoint stay valid.
    info: *mut ffi::fi_info,
    /// Generic `info` (no `FI_SOURCE`, no node/service) used to build
    /// active endpoints on outbound dials. A distinct allocation from
    /// `info` so both can be freed independently in `Drop`.
    dial_info: *mut ffi::fi_info,
    fabric: *mut ffi::fid_fabric,
    domain: *mut ffi::fid_domain,
    /// Configuration captured at construction. `cfg.numa` drives the
    /// NUMA-mismatch check on `add_connection` / `register_backing`;
    /// `cfg.self_peer` is the identity sent on outbound dials.
    pub(crate) cfg: FabricConfig,
    /// Per-peer connected endpoints, keyed by `PeerId`. Published into
    /// by both the outbound dial (`add_connection`) and the inbound
    /// accept loop, which shares this `Arc`. A connection is reused for
    /// both send and receive regardless of which side dialed.
    pub(crate) connections: Arc<ConnectionTable>,
    /// MR handles registered against this fabric's domain. Held so the
    /// MRs are closed in `Drop` before the domain is torn down. The
    /// `MrHandle` value callers receive does not close the MR.
    pub(crate) mrs: RwLock<Vec<*mut ffi::fid_mr>>,
    /// Monotonic source of application-supplied MR keys (providers
    /// without `FI_MR_PROV_KEY` require a distinct key per `fi_mr_reg`).
    /// An `Arc` so the dialer and accept loop can register transient
    /// local control-buffer MRs from the same key space.
    pub(crate) next_mr_key: Arc<AtomicU64>,
    /// Whether the negotiated provider addresses remote RMA targets by
    /// virtual address (`FI_MR_VIRT_ADDR`) or by a 0-based MR offset.
    pub(crate) mr_virt_addr: bool,
    /// Whether the negotiated provider requires a registered local MR
    /// (`desc`) on every send/recv buffer (`FI_MR_LOCAL`; verbs). The tcp
    /// provider negotiates this off, so control buffers post `desc=NULL`.
    pub(crate) needs_local_mr: bool,
    /// Negotiated `domain_attr->threading` discriminant.
    threading: i32,
    /// Negotiated `domain_attr->mr_cnt` (0 = no fixed limit).
    domain_mr_cnt: usize,
    /// Per-NUMA progress group polling every connection's CQ. Shared
    /// with the accept loop so accepted connections can register their
    /// CQs. Stopped explicitly in `Drop` before the domain is closed.
    pub(crate) progress: Arc<ProgressGroup>,
    /// Inbound frame router: request frames to the RPC server, ack
    /// frames to the waiting client stream.
    pub(crate) dispatch: Arc<InboundDispatch>,
    /// Passive listening endpoint, present when `cfg.listen`.
    listener: Option<Arc<cm::Listener>>,
    /// Set to stop the accept loop at teardown.
    accept_shutdown: Arc<AtomicBool>,
    /// Accept-loop thread handle.
    accept_thread: Option<std::thread::JoinHandle<()>>,
    pub(crate) completions: Arc<CompletionRegistry>,
    /// Dial context shared with the background reconnect loop. Holds the
    /// raw handles needed to open active endpoints plus the per-fabric
    /// machinery to publish them.
    pub(crate) dialer: Arc<Dialer>,
    /// Peers this fabric should keep dialed, keyed by `PeerId`. Populated
    /// by `add_connection` / `set_desired_peers`; the reconnect loop
    /// re-dials any desired peer that has no live connection.
    pub(crate) desired: Arc<RwLock<HashMap<PeerId, ConnectionSpec>>>,
    /// Set to stop the reconnect loop at teardown.
    reconnect_shutdown: Arc<AtomicBool>,
    /// Reconnect-loop thread handle.
    reconnect_thread: Option<std::thread::JoinHandle<()>>,
    /// Pre-registered send slab shared by every connection's endpoint
    /// (local MRs are domain-scoped). Removes `fi_mr_reg` from the send
    /// hot path on verbs; its slab MR is closed via `mrs` in `Drop`.
    pub(crate) send_pool: Arc<SendPool>,
}

// SAFETY: every libfabric resource held here is documented thread-safe
// for the operations we perform; CQs are polled only by the progress
// group, and endpoints are created/closed under the fabric's lifecycle.
unsafe impl Send for FabricInner {}
unsafe impl Sync for FabricInner {}

/// Send wrapper for the raw domain pointer handed to the accept loop.
/// The domain outlives the loop (the loop is joined before the domain
/// closes) and is only used to create accepted endpoints.
struct DomainPtr(*mut ffi::fid_domain);
// SAFETY: see above; the domain is internally synchronized and stays
// alive for the loop's whole lifetime.
unsafe impl Send for DomainPtr {}

impl Fabric {
    /// Bring up a libfabric stack against `cfg`. On success the
    /// fabric/domain exist, the progress group is running, and (when
    /// `cfg.listen`) a passive endpoint is listening with its accept
    /// loop spawned.
    pub fn new(cfg: FabricConfig) -> Result<Self> {
        cfg.validate()?;

        let prov_name: &str = match cfg.provider {
            // Native verbs MSG: no `ofi_rxm` utility layer. The base
            // verbs provider's connection-oriented FI_EP_MSG is exactly
            // what this transport now uses.
            Provider::Verbs => "verbs",
            Provider::Tcp => "tcp",
        };
        let prov_cstr =
            CString::new(prov_name).map_err(|_| FabricError::BadConfig("provider name has NUL"))?;

        let hints = unsafe { ffi::ub_fi_build_msg_hints(prov_cstr.as_ptr()) };
        if hints.is_null() {
            return Err(FabricError::Pkg("ub_fi_build_msg_hints", 0));
        }
        let _hints_guard = FreeInfoOnDrop(hints);

        // Pin the verbs domain to the named HCA so fi_getinfo does not
        // collapse an 8-HCA host onto mlx5_0. The tcp provider has a
        // single domain, so this is verbs-only.
        if matches!(cfg.provider, Provider::Verbs) {
            let dom = CString::new(cfg.device_name.as_str())
                .map_err(|_| FabricError::BadConfig("device name has NUL"))?;
            let rc = unsafe { ffi::ub_fi_hints_set_domain(hints, dom.as_ptr()) };
            if rc != 0 {
                return Err(FabricError::Pkg("ub_fi_hints_set_domain", rc));
            }
        }

        // When listening, socket binds are passed to fi_getinfo as
        // node/service. Native binds seed hints->src_addr with raw
        // provider bytes and still use FI_SOURCE.
        let (node_cstr, service_cstr): (Option<CString>, Option<CString>) = if cfg.listen {
            match cfg.listen_addr.as_deref() {
                Some(addr) => {
                    if addr.starts_with("hex:") {
                        let native = cm::decode_native_addr(addr)?;
                        let rc = unsafe {
                            ffi::ub_fi_hints_set_src_addr(
                                hints,
                                native.as_ptr() as *const std::ffi::c_void,
                                native.len(),
                            )
                        };
                        if rc != 0 {
                            return Err(FabricError::Pkg("ub_fi_hints_set_src_addr", rc));
                        }
                        (None, None)
                    } else if matches!(cfg.provider, Provider::Verbs)
                        && is_unspecified_ephemeral_socket(addr)
                    {
                        // Auto-RDMA on InfiniBand HCAs has no netdev-backed
                        // IP address to bind. Passing NULL node/service lets
                        // verbs pick a native AF_IB source address, which is
                        // what peers can dial through RDMA-CM.
                        (None, None)
                    } else {
                        let (host, port) = addr.rsplit_once(':').ok_or(FabricError::BadConfig(
                            "listen_addr must be host:port or hex:<bytes>",
                        ))?;
                        let host = CString::new(host)
                            .map_err(|_| FabricError::BadConfig("listen_addr has NUL"))?;
                        let port = CString::new(port)
                            .map_err(|_| FabricError::BadConfig("listen_addr has NUL"))?;
                        (Some(host), Some(port))
                    }
                }
                None => (None, None),
            }
        } else {
            (None, None)
        };
        let node_ptr = node_cstr
            .as_ref()
            .map(|s| s.as_ptr())
            .unwrap_or(ptr::null());
        let service_ptr = service_cstr
            .as_ref()
            .map(|s| s.as_ptr())
            .unwrap_or(ptr::null());
        let flags = if cfg.listen { ffi::FI_SOURCE } else { 0 };

        // Primary info: source-bound when listening.
        let mut info: *mut ffi::fi_info = ptr::null_mut();
        let rc = unsafe {
            ffi::fi_getinfo(
                ffi::requested_version(),
                node_ptr,
                service_ptr,
                flags,
                hints,
                &mut info,
            )
        };
        check("fi_getinfo", rc)?;
        if info.is_null() {
            return Err(FabricError::NotFound("fi_getinfo returned no info"));
        }
        let info_guard = FreeInfoOnDrop(info);

        // Dial info: generic, no source bind. Built from the same hints
        // so it carries the same provider/domain/caps; used to create
        // active endpoints for outbound dials.
        let mut dial_info: *mut ffi::fi_info = ptr::null_mut();
        let rc = unsafe {
            ffi::fi_getinfo(
                ffi::requested_version(),
                ptr::null(),
                ptr::null(),
                0,
                hints,
                &mut dial_info,
            )
        };
        check("fi_getinfo(dial)", rc)?;
        if dial_info.is_null() {
            return Err(FabricError::NotFound("fi_getinfo(dial) returned no info"));
        }
        let dial_info_guard = FreeInfoOnDrop(dial_info);

        // Remote RMA addressing mode and domain attributes come from the
        // primary info.
        let mr_mode = unsafe { ffi::ub_fi_info_mr_mode(info) };
        let mr_virt_addr = (mr_mode & ffi::FI_MR_VIRT_ADDR) != 0;
        let needs_local_mr = (mr_mode & ffi::FI_MR_LOCAL) != 0;
        let threading = unsafe { ffi::ub_fi_info_threading(info) };
        let domain_mr_cnt = unsafe { ffi::ub_fi_info_mr_cnt(info) };
        let thread_safe = unsafe { ffi::ub_fi_thread_safe_value() };
        if threading != thread_safe {
            eprintln!(
                "warning: libfabric provider '{prov_name}' negotiated threading mode {threading} \
                 (FI_THREAD_SAFE is {thread_safe}); a shared per-HCA domain posted to from \
                 multiple shard cores requires FI_THREAD_SAFE."
            );
        }

        let result = (|| -> Result<Fabric> {
            // fi_fabric / fi_domain from the primary info.
            let mut fabric_p: *mut ffi::fid_fabric = ptr::null_mut();
            let attr = unsafe { ffi::ub_fi_info_fabric_attr(info) };
            let rc = unsafe { ffi::fi_fabric(attr, &mut fabric_p, ptr::null_mut()) };
            check("fi_fabric", rc)?;
            let fabric_guard = CloseFidOnDrop(ffi::as_fid_fabric(fabric_p));

            let mut domain_p: *mut ffi::fid_domain = ptr::null_mut();
            let rc = unsafe { ffi::ub_fi_domain(fabric_p, info, &mut domain_p, ptr::null_mut()) };
            check("fi_domain", rc)?;
            let domain_guard = CloseFidOnDrop(ffi::as_fid_domain(domain_p));

            // Each admitted request can hold a pipeline of reverse RMA writes,
            // and each connection arms one RecvPool per QP.
            let write_slots = cfg.max_inflight.saturating_mul(cfg.write_pipeline_depth);
            let registry_capacity = write_slots.saturating_add(
                cfg.max_connections
                    .saturating_mul(cfg.rpc_posted_recvs)
                    .saturating_mul(cfg.qps_per_connection),
            );
            let completions = CompletionRegistry::new(registry_capacity);
            let dispatch = InboundDispatch::new();
            let progress = Arc::new(ProgressGroup::new(&cfg)?);

            // Shared MR key counter and the local-MR context. The context
            // is cloned into the accept loop, the dialer, and (via
            // `FabricInner::local_mr_ctx`) the send paths so every posting
            // site can register a transient control-buffer MR on verbs
            // (`FI_MR_LOCAL`) and skip it on tcp.
            let next_mr_key = Arc::new(AtomicU64::new(0));
            let local_ctx = LocalMrCtx::new(domain_p, needs_local_mr, Arc::clone(&next_mr_key));

            // Per-fabric pre-registered send slab. On verbs this removes
            // `fi_mr_reg` from the send hot path (the slab MR is closed in
            // the fabric's ordered teardown, so it joins `mrs`); on tcp the
            // slab is unset and sends post `desc = NULL`.
            let send_pool = Arc::new(SendPool::new(
                domain_p,
                needs_local_mr,
                Arc::clone(&next_mr_key),
                Arc::clone(&completions),
            )?);
            let initial_mrs = match send_pool.slab_mr() {
                Some(mr) => vec![mr],
                None => Vec::new(),
            };

            // Listening endpoint, when configured. The accept loop is
            // spawned after `FabricInner` owns teardown for every raw
            // handle it can touch.
            let accept_shutdown = Arc::new(AtomicBool::new(false));
            let connections = Arc::new(ConnectionTable::new());
            let listener = if cfg.listen {
                Some(Arc::new(cm::Listener::new(fabric_p, info)?))
            } else {
                None
            };
            let accept_local_ctx = local_ctx.clone();
            let cfg_inner = cfg.clone();

            std::mem::forget(fabric_guard);
            std::mem::forget(domain_guard);
            let info_ptr = info_guard.0;
            std::mem::forget(info_guard);
            let dial_info_ptr = dial_info_guard.0;
            std::mem::forget(dial_info_guard);

            // Dial context, shared between `add_connection` and the
            // background reconnect loop. The raw handles outlive both
            // threads (joined in Drop before the domain/fabric close).
            let dialer = Arc::new(Dialer::new(
                fabric_p,
                domain_p,
                dial_info_ptr,
                cfg.self_peer,
                cfg.numa,
                cfg.rpc_posted_recvs,
                cfg.qps_per_connection,
                Arc::clone(&progress),
                Arc::clone(&completions),
                Arc::clone(&dispatch),
                Arc::clone(&connections),
                local_ctx,
            ));

            let desired = Arc::new(RwLock::new(HashMap::new()));
            let reconnect_shutdown = Arc::new(AtomicBool::new(false));

            let mut inner = FabricInner {
                info: info_ptr,
                dial_info: dial_info_ptr,
                fabric: fabric_p,
                domain: domain_p,
                cfg: cfg_inner,
                connections,
                mrs: RwLock::new(initial_mrs),
                next_mr_key,
                mr_virt_addr,
                needs_local_mr,
                threading,
                domain_mr_cnt,
                progress,
                dispatch,
                listener,
                accept_shutdown,
                accept_thread: None,
                completions,
                dialer,
                desired,
                reconnect_shutdown,
                reconnect_thread: None,
                send_pool,
            };

            if let Some(listener) = inner.listener.as_ref().map(Arc::clone) {
                inner.accept_thread = Some(spawn_accept_loop(
                    listener,
                    DomainPtr(inner.domain),
                    inner.cfg.numa,
                    inner.cfg.rpc_posted_recvs,
                    Arc::clone(&inner.completions),
                    Arc::clone(&inner.dispatch),
                    Arc::clone(&inner.progress),
                    Arc::clone(&inner.connections),
                    Arc::clone(&inner.accept_shutdown),
                    accept_local_ctx,
                ));
            }

            // Background reconnect loop: re-dials any desired peer with no
            // live connection, covering the startup race (both directed
            // dials lost) and peers that come up later.
            inner.reconnect_thread = Some(spawn_reconnect_loop(
                Arc::clone(&inner.dialer),
                Arc::clone(&inner.desired),
                Arc::clone(&inner.reconnect_shutdown),
            ));

            Ok(Fabric {
                inner: Arc::new(inner),
            })
        })();

        drop(_hints_guard);
        result
    }

    /// The bound listen address as a numeric socket address when
    /// libfabric reports one, otherwise as `hex:<fi_getname-bytes>`.
    /// Errors when this fabric was not configured to listen.
    pub fn self_address(&self) -> Result<String> {
        match self.inner.listener.as_ref() {
            Some(l) => l.local_addr(),
            None => Err(FabricError::BadConfig("fabric is not listening")),
        }
    }

    /// Shared completion registry for RPC submissions.
    pub(crate) fn completions(&self) -> &Arc<CompletionRegistry> {
        &self.inner.completions
    }

    pub(crate) fn inner(&self) -> &Arc<FabricInner> {
        &self.inner
    }

    pub(crate) fn inner_arc(&self) -> Arc<FabricInner> {
        self.inner.clone()
    }

    /// Inbound frame router; the RPC server installs its request sink
    /// here at startup.
    pub(crate) fn dispatch(&self) -> &Arc<InboundDispatch> {
        &self.inner.dispatch
    }

    /// True when the negotiated provider addresses remote RMA targets
    /// by virtual address; false when it uses 0-based MR offsets.
    pub(crate) fn mr_uses_virtual_addr(&self) -> bool {
        self.inner.mr_virt_addr
    }

    /// Negotiated `domain_attr->threading` discriminant.
    pub fn threading_mode(&self) -> i32 {
        self.inner.threading
    }

    /// Whether the negotiated domain threading mode is `FI_THREAD_SAFE`.
    pub fn is_thread_safe(&self) -> bool {
        self.inner.threading == unsafe { ffi::ub_fi_thread_safe_value() }
    }

    /// Negotiated maximum number of memory regions for this domain, or
    /// 0 when the provider advertises no fixed limit.
    pub fn domain_mr_limit(&self) -> usize {
        self.inner.domain_mr_cnt
    }

    /// Verify `expected_registrations` memory regions fit this
    /// (potentially shared) domain and log the negotiated threading
    /// mode and MR capacity. Returns true when the expected count
    /// exceeds a non-zero limit.
    pub fn check_shared_domain_capacity(&self, expected_registrations: usize) -> bool {
        let limit = self.inner.domain_mr_cnt;
        let safe = self.is_thread_safe();
        eprintln!(
            "fabric '{}': domain threading={} (thread_safe={}), mr_cnt limit={} (0=unlimited), \
             expected MR registrations={}",
            self.inner.cfg.device_name, self.inner.threading, safe, limit, expected_registrations,
        );
        let exceeded = mr_capacity_exceeds(limit, expected_registrations);
        if exceeded {
            eprintln!(
                "warning: expected {expected_registrations} MR registrations exceed the \
                 provider's per-domain limit of {limit}; reduce shards per HCA or split the \
                 shared domain."
            );
        }
        exceeded
    }
}

/// Spawn the accept loop. It accepts inbound connections and publishes
/// each into the shared connection table via `install_connection`,
/// which arms a recv pool and registers the CQ with the progress group.
/// Establishment is single-dialer: this side accepts connects from any
/// peer whose id is higher than ours (the lower-id node dials).
#[allow(clippy::too_many_arguments)]
fn spawn_accept_loop(
    listener: Arc<cm::Listener>,
    domain: DomainPtr,
    numa: Option<u16>,
    recv_depth: usize,
    completions: Arc<CompletionRegistry>,
    dispatch: Arc<InboundDispatch>,
    progress: Arc<ProgressGroup>,
    connections: Arc<ConnectionTable>,
    shutdown: Arc<AtomicBool>,
    local_ctx: LocalMrCtx,
) -> std::thread::JoinHandle<()> {
    std::thread::Builder::new()
        .name("fabric-accept".to_string())
        .spawn(move || {
            let domain = domain;
            while !shutdown.load(Ordering::Relaxed) {
                match listener.accept_one_timeout(domain.0, numa, ACCEPT_POLL_MS) {
                    Ok(Some(conn)) => {
                        let peer = conn.peer();
                        if let Err(e) = install_connection(
                            &connections,
                            &progress,
                            &completions,
                            &dispatch,
                            recv_depth,
                            peer,
                            Arc::new(conn),
                            &local_ctx,
                        ) {
                            eprintln!("fabric accept: failed to install connection: {e:?}");
                            continue;
                        }
                    }
                    Ok(None) => continue,
                    Err(_) if shutdown.load(Ordering::Relaxed) => break,
                    Err(e) => {
                        eprintln!("fabric accept: accept_one failed: {e:?}");
                        // Brief backoff to avoid a hot error loop.
                        std::thread::sleep(std::time::Duration::from_millis(ACCEPT_POLL_MS as u64));
                    }
                }
            }
        })
        .expect("spawn fabric-accept thread")
}

/// Accept-loop poll interval in milliseconds; bounds shutdown latency.
const ACCEPT_POLL_MS: i32 = 200;

/// Reconnect-loop dial interval. Long enough not to hammer a genuinely
/// down peer, short enough that a startup race resolves quickly.
const RECONNECT_INTERVAL_MS: u64 = 1_000;

/// Reconnect-loop shutdown-poll granularity; bounds teardown latency
/// while the loop waits out `RECONNECT_INTERVAL_MS`.
const RECONNECT_POLL_MS: u64 = 100;

/// Background loop that re-dials every desired peer this node is the
/// dialer for (lower id) that lacks a live connection. This closes the
/// startup race (the peer's listener was not up when we first dialed)
/// and reconnects peers that come up after this fabric started. It does
/// not detect silently-dead established connections; those linger in the
/// connection table until a future disconnect-detection feature removes
/// them.
fn spawn_reconnect_loop(
    dialer: Arc<Dialer>,
    desired: Arc<RwLock<HashMap<PeerId, ConnectionSpec>>>,
    shutdown: Arc<AtomicBool>,
) -> std::thread::JoinHandle<()> {
    std::thread::Builder::new()
        .name("fabric-reconnect".to_string())
        .spawn(move || {
            while !shutdown.load(Ordering::Relaxed) {
                // Wait out the interval in small steps so teardown is
                // prompt.
                let mut waited = 0;
                while waited < RECONNECT_INTERVAL_MS && !shutdown.load(Ordering::Relaxed) {
                    std::thread::sleep(Duration::from_millis(RECONNECT_POLL_MS));
                    waited += RECONNECT_POLL_MS;
                }
                if shutdown.load(Ordering::Relaxed) {
                    break;
                }

                let connected: HashSet<PeerId> = dialer.connected_peers().into_iter().collect();
                let pending: Vec<ConnectionSpec> = match desired.read() {
                    Ok(map) => map
                        .values()
                        .filter(|spec| {
                            dialer.should_dial(spec.peer) && !connected.contains(&spec.peer)
                        })
                        .cloned()
                        .collect(),
                    Err(_) => break,
                };

                for spec in pending {
                    if let Err(e) = dialer.dial_and_install(&spec) {
                        eprintln!("fabric reconnect: dial peer {} failed: {e:?}", spec.peer.0);
                    }
                }
            }
        })
        .expect("spawn fabric-reconnect thread")
}

/// Whether `expected` MR registrations exceed a per-domain `limit`. A
/// `limit` of 0 means no fixed cap.
fn mr_capacity_exceeds(limit: usize, expected: usize) -> bool {
    limit != 0 && expected > limit
}

/// Probe whether `provider`'s libfabric provider can satisfy this
/// crate's `FI_EP_MSG` requirements in the current environment.
///
/// Returns `true` when libfabric returns at least one matching
/// `fi_info` for the provider's MSG endpoint, `false` otherwise
/// (including `-FI_ENODATA`). Issues a single non-binding `fi_getinfo`
/// and frees everything it allocates.
pub fn provider_available(provider: Provider) -> bool {
    let prov_name = match provider {
        Provider::Verbs => "verbs",
        Provider::Tcp => "tcp",
    };
    let Ok(prov_cstr) = CString::new(prov_name) else {
        return false;
    };

    let hints = unsafe { ffi::ub_fi_build_msg_hints(prov_cstr.as_ptr()) };
    if hints.is_null() {
        return false;
    }

    let mut info: *mut ffi::fi_info = ptr::null_mut();
    let rc = unsafe {
        ffi::fi_getinfo(
            ffi::requested_version(),
            ptr::null(),
            ptr::null(),
            0,
            hints,
            &mut info,
        )
    };
    let available = rc == 0 && !info.is_null();

    if !info.is_null() {
        unsafe { ffi::fi_freeinfo(info) };
    }
    unsafe { ffi::fi_freeinfo(hints) };

    available
}

impl FabricInner {
    pub(crate) fn domain(&self) -> *mut ffi::fid_domain {
        self.domain
    }

    /// The per-fabric pre-registered send pool used by both send paths.
    pub(crate) fn send_pool(&self) -> &Arc<SendPool> {
        &self.send_pool
    }
}

fn is_unspecified_ephemeral_socket(addr: &str) -> bool {
    addr.parse::<SocketAddr>()
        .is_ok_and(|addr| addr.ip().is_unspecified() && addr.port() == 0)
}

impl Drop for FabricInner {
    fn drop(&mut self) {
        // Stop accepting first so no new connection/CQ appears mid-teardown.
        self.accept_shutdown.store(true, Ordering::Relaxed);
        if let Some(handle) = self.accept_thread.take() {
            let _ = handle.join();
        }

        // Stop the reconnect loop too: like the accept loop it creates
        // endpoints, registers CQs, and arms recvs, so it must be fully
        // joined before any CQ/domain teardown below.
        self.reconnect_shutdown.store(true, Ordering::Relaxed);
        if let Some(handle) = self.reconnect_thread.take() {
            let _ = handle.join();
        }

        // Stop the progress threads BEFORE closing any CQ. The poll loop
        // dereferences raw CQ pointers from a lock-free snapshot, so a CQ
        // must not be closed while a thread might still poll it. Joining
        // the threads here guarantees no further polling; per-connection
        // unregister calls are then unnecessary.
        self.progress.stop();

        // Tear down every connection. The progress threads are already
        // joined, so dropping a connection (closes ep -> cancels its
        // recvs, then closes cq/eq) is race-free.
        for conn in self.connections.take_all() {
            drop(conn);
        }

        // Close the passive endpoint (depends on fabric + info) before
        // freeing either.
        self.listener = None;

        unsafe {
            if let Ok(mut mrs) = self.mrs.write() {
                for mr in mrs.drain(..) {
                    if !mr.is_null() {
                        let _ = ffi::ub_fi_close(ffi::as_fid_mr(mr));
                    }
                }
            }
            if !self.domain.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_domain(self.domain));
            }
            if !self.fabric.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_fabric(self.fabric));
            }
            if !self.info.is_null() {
                ffi::fi_freeinfo(self.info);
            }
            if !self.dial_info.is_null() {
                ffi::fi_freeinfo(self.dial_info);
            }
        }
    }
}

/// Drop guard that closes a libfabric resource if still alive at scope
/// exit. Keeps the construction path in `Fabric::new` linear.
struct CloseFidOnDrop(*mut ffi::fid);
impl Drop for CloseFidOnDrop {
    fn drop(&mut self) {
        if !self.0.is_null() {
            unsafe {
                let _ = ffi::ub_fi_close(self.0);
            }
        }
    }
}

struct FreeInfoOnDrop(*mut ffi::fi_info);
impl Drop for FreeInfoOnDrop {
    fn drop(&mut self) {
        if !self.0.is_null() {
            unsafe {
                ffi::fi_freeinfo(self.0);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fabric::config::defaults_for;
    use crate::runtime::{DefaultRuntime, WorkerIdx};

    /// Probe whether the tcp MSG provider can be brought up here.
    fn tcp_provider_available() -> bool {
        provider_available(Provider::Tcp)
    }

    #[test]
    fn fabric_new_tcp_loopback_or_skip() {
        if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
            eprintln!("FABRIC_SKIP_FFI set; skipping libfabric tcp loopback test");
            return;
        }
        if !tcp_provider_available() {
            eprintln!("libfabric tcp provider unavailable; skipping loopback test");
            return;
        }

        let rt: std::sync::Arc<dyn crate::runtime::Threading> = DefaultRuntime::new(1);
        let mut cfg = defaults_for("lo", rt, WorkerIdx(0));
        cfg.provider = Provider::Tcp;
        cfg.progress_threads = 1;
        cfg.max_inflight = 16;
        cfg.listen = true;
        cfg.listen_addr = Some("127.0.0.1:0".to_string());

        let fabric = Fabric::new(cfg)
            .expect("Fabric::new should succeed after tcp provider availability is established");

        // The bound address must resolve to a concrete loopback port.
        let addr = fabric.self_address().expect("self_address");
        assert!(addr.starts_with("127.0.0.1:"), "unexpected addr {addr}");

        drop(fabric);
    }

    #[test]
    fn provider_available_matches_probe_for_tcp() {
        if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
            eprintln!("FABRIC_SKIP_FFI set; skipping libfabric provider probe test");
            return;
        }
        assert_eq!(
            provider_available(Provider::Tcp),
            tcp_provider_available(),
            "provider_available(Tcp) must match the local tcp probe",
        );
    }

    #[test]
    fn thread_safe_value_is_fi_thread_safe() {
        assert_eq!(unsafe { ffi::ub_fi_thread_safe_value() }, 1);
    }

    #[test]
    fn unspecified_ephemeral_socket_detects_auto_rdma_bind() {
        assert!(is_unspecified_ephemeral_socket("0.0.0.0:0"));
        assert!(is_unspecified_ephemeral_socket("[::]:0"));
        assert!(!is_unspecified_ephemeral_socket("0.0.0.0:1"));
        assert!(!is_unspecified_ephemeral_socket("127.0.0.1:0"));
        assert!(!is_unspecified_ephemeral_socket("hex:001122"));
    }

    #[test]
    fn mr_capacity_exceeds_respects_zero_and_limit() {
        assert!(!mr_capacity_exceeds(0, 0));
        assert!(!mr_capacity_exceeds(0, 1_000_000));
        assert!(!mr_capacity_exceeds(8, 0));
        assert!(!mr_capacity_exceeds(8, 8));
        assert!(mr_capacity_exceeds(8, 9));
    }

    #[test]
    fn fabric_reports_thread_safe_and_capacity_or_skip() {
        if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
            eprintln!("FABRIC_SKIP_FFI set; skipping libfabric threading/capacity test");
            return;
        }
        if !tcp_provider_available() {
            eprintln!("libfabric tcp provider unavailable; skipping threading/capacity test");
            return;
        }

        let rt: std::sync::Arc<dyn crate::runtime::Threading> = DefaultRuntime::new(1);
        let mut cfg = defaults_for("lo", rt, WorkerIdx(0));
        cfg.provider = Provider::Tcp;
        cfg.progress_threads = 1;
        cfg.max_inflight = 16;
        cfg.listen = true;
        cfg.listen_addr = Some("127.0.0.1:0".to_string());

        let fabric = Fabric::new(cfg).expect("Fabric::new should succeed for tcp loopback");

        assert!(
            fabric.is_thread_safe(),
            "tcp provider negotiated threading mode {} but FI_THREAD_SAFE is required",
            fabric.threading_mode(),
        );

        let limit = fabric.domain_mr_limit();
        assert!(
            !fabric.check_shared_domain_capacity(2),
            "two registrations must fit the domain mr_cnt limit of {limit}",
        );

        drop(fabric);
    }
}
