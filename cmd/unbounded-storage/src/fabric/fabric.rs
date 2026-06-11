// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! libfabric class lifecycle: bring up a fabric, domain, address
//! vector, one endpoint, and one CQ per progress thread, then drop
//! everything in the correct order. Phase 3 stops at "enabled
//! endpoint with progress threads alive"; no connections, MR
//! registration, or actual data traffic happens here.

use std::ffi::CString;
use std::ptr;
use std::sync::atomic::AtomicU64;
use std::sync::{Arc, RwLock};

use super::completion::CompletionRegistry;
use super::config::{FabricConfig, Provider};
use super::connection::ConnectionTable;
use super::error::{FabricError, Result, check};
use super::ffi;
use super::progress::ProgressThread;

/// Live libfabric stack for one transport instance.
pub struct Fabric {
    inner: Arc<FabricInner>,
}

pub(crate) struct FabricInner {
    /// `info` returned by `fi_getinfo`; kept alive for the lifetime
    /// of the fabric so the sub-attribute pointers we passed to
    /// `fi_fabric` / `fi_domain` stay valid.
    info: *mut ffi::fi_info,
    fabric: *mut ffi::fid_fabric,
    domain: *mut ffi::fid_domain,
    av: *mut ffi::fid_av,
    ep: *mut ffi::fid_ep,
    /// Configuration captured at construction. Phase 4 reads
    /// `cfg.numa` for the NUMA-mismatch check on `add_connection`
    /// and `register_backing`.
    pub(crate) cfg: FabricConfig,
    /// Active connections (peer -> libfabric `fi_addr_t`).
    pub(crate) connections: ConnectionTable,
    /// MR handles registered against this fabric's domain. Held so
    /// the MRs are closed in `Drop` before the domain is torn down.
    /// Lifetime model: callers get a `MrHandle` value; the `fid_mr`
    /// is owned by this `Vec` and dropped on fabric shutdown. The
    /// `MrHandle` itself does not close the MR.
    pub(crate) mrs: RwLock<Vec<*mut ffi::fid_mr>>,
    /// Monotonic source of application-supplied MR keys. Providers that
    /// do not advertise `FI_MR_PROV_KEY` (for example the `tcp` RDM
    /// provider) require the caller to assign a distinct `requested_key`
    /// to every `fi_mr_reg`; reusing a key returns `FI_ENOKEY`. We hand
    /// out a fresh key per registration so multiple backings (the data
    /// region plus the RPC scratch region) can coexist. Providers that
    /// do assign their own keys simply ignore the requested value.
    pub(crate) next_mr_key: AtomicU64,
    /// Whether the negotiated provider addresses remote RMA targets by
    /// virtual address (`FI_MR_VIRT_ADDR` set in the domain `mr_mode`)
    /// or by a 0-based offset into the registered MR. The `tcp` RDM
    /// provider clears `mr_mode` to 0 here, so remote targets are
    /// offsets; verbs typically uses virtual addresses. The remote base
    /// advertised in an RPC request header is derived from this.
    pub(crate) mr_virt_addr: bool,
    /// The provider's negotiated `domain_attr->threading` mode (the raw
    /// `enum fi_threading` discriminant). A single shared per-HCA domain
    /// is posted to concurrently by multiple serving shards while
    /// NIC-worker threads progress completions, which is only sound
    /// without external serialization when the provider negotiated
    /// `FI_THREAD_SAFE`. Captured here so callers can verify the mode
    /// (see `is_thread_safe`) and so bring-up warns loudly on a weaker
    /// mode rather than racing silently.
    threading: i32,
    /// The provider's negotiated `domain_attr->mr_cnt`: the maximum
    /// number of memory regions registrable against this domain, or 0
    /// when the provider advertises no fixed limit. Used by
    /// `check_shared_domain_capacity` to verify the expected per-domain
    /// registrations fit before they are attempted.
    domain_mr_cnt: usize,
    /// One progress thread per CQ. Dropped before the CQ/EP/AV/domain/fabric
    /// teardown in `FabricInner::drop` so the threads join before we
    /// close their CQs.
    progress: Vec<ProgressThread>,
    /// Raw CQ pointers matched 1:1 with `progress`. We hold them
    /// separately so we can close them *after* `progress.clear()`
    /// has joined every poll thread.
    progress_cqs: Vec<*mut ffi::fid_cq>,
    pub(crate) completions: Arc<CompletionRegistry>,
}

// SAFETY: every libfabric resource we hold is documented as
// thread-safe; the only thread that actively polls the CQs is each
// progress thread, and the EP / AV are only mutated through
// reference-counted operations.
unsafe impl Send for FabricInner {}
unsafe impl Sync for FabricInner {}

impl Fabric {
    /// Bring up a libfabric stack against `cfg`. On success the
    /// endpoint is enabled and all progress threads are running.
    pub fn new(cfg: FabricConfig) -> Result<Self> {
        cfg.validate()?;

        let prov_name: &str = match cfg.provider {
            Provider::Verbs => "verbs",
            Provider::Tcp => "tcp",
        };
        let prov_cstr =
            CString::new(prov_name).map_err(|_| FabricError::BadConfig("provider name has NUL"))?;

        let hints = unsafe { ffi::ub_fi_build_hints(prov_cstr.as_ptr()) };
        if hints.is_null() {
            return Err(FabricError::Pkg("ub_fi_build_hints", 0));
        }
        let _hints_guard = FreeInfoOnDrop(hints);

        // libfabric expects the local address split into a `node`
        // (host) and a `service` (port); passing the combined
        // "host:port" string as `service` makes `fi_getinfo` fail
        // (getaddrinfo cannot parse it). Split on the final ':' so
        // IPv4 "127.0.0.1:9101" becomes node="127.0.0.1",
        // service="9101".
        let (node_cstr, service_cstr): (Option<CString>, Option<CString>) = match cfg.provider {
            Provider::Tcp => match cfg.listen_addr.as_deref() {
                Some(addr) => {
                    let (host, port) = addr
                        .rsplit_once(':')
                        .ok_or(FabricError::BadConfig("listen_addr must be host:port"))?;
                    let host = CString::new(host)
                        .map_err(|_| FabricError::BadConfig("listen_addr has NUL"))?;
                    let port = CString::new(port)
                        .map_err(|_| FabricError::BadConfig("listen_addr has NUL"))?;
                    (Some(host), Some(port))
                }
                None => (None, None),
            },
            Provider::Verbs => (None, None),
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

        // Remote RMA addressing mode is fixed by the provider's
        // negotiated mr_mode. With FI_MR_VIRT_ADDR the remote target is
        // the registered virtual address; without it (the tcp RDM provider)
        // it is a 0-based offset into the MR.
        let mr_mode = unsafe { ffi::ub_fi_info_mr_mode(info) };
        let mr_virt_addr = (mr_mode & ffi::FI_MR_VIRT_ADDR) != 0;

        // Negotiated domain threading model and MR capacity. A shared
        // per-HCA domain is driven concurrently by multiple shard cores,
        // so we require FI_THREAD_SAFE; warn (do not fail) on a weaker
        // mode so the fallback of gating posts through NIC-worker threads
        // can be applied without bricking otherwise-working providers.
        let threading = unsafe { ffi::ub_fi_info_threading(info) };
        let domain_mr_cnt = unsafe { ffi::ub_fi_info_mr_cnt(info) };
        let thread_safe = unsafe { ffi::ub_fi_thread_safe_value() };
        if threading != thread_safe {
            eprintln!(
                "warning: libfabric provider '{prov_name}' negotiated threading mode {threading} \
                 (FI_THREAD_SAFE is {thread_safe}); a shared per-HCA domain posted to from \
                 multiple shard cores requires FI_THREAD_SAFE. Outbound posting must be \
                 serialized through a NIC-worker thread until this is resolved."
            );
        }

        // From here on we accumulate libfabric resources and must
        // release them on any error path. We do that by building
        // `FabricInner` incrementally inside a closure and letting
        // its `Drop` clean up if construction fails partway.
        let result = (|| -> Result<Fabric> {
            // fi_fabric
            let mut fabric_p: *mut ffi::fid_fabric = ptr::null_mut();
            let attr = unsafe { ffi::ub_fi_info_fabric_attr(info) };
            let rc = unsafe { ffi::fi_fabric(attr, &mut fabric_p, ptr::null_mut()) };
            check("fi_fabric", rc)?;
            let fabric_guard = CloseFidOnDrop(ffi::as_fid_fabric(fabric_p));

            // fi_domain
            let mut domain_p: *mut ffi::fid_domain = ptr::null_mut();
            let rc = unsafe { ffi::ub_fi_domain(fabric_p, info, &mut domain_p, ptr::null_mut()) };
            check("fi_domain", rc)?;
            let domain_guard = CloseFidOnDrop(ffi::as_fid_domain(domain_p));

            // fi_av_open
            let mut av_attr: ffi::fi_av_attr = unsafe { std::mem::zeroed() };
            av_attr.type_ = ffi::FI_AV_TABLE;
            let mut av_p: *mut ffi::fid_av = ptr::null_mut();
            let rc =
                unsafe { ffi::ub_fi_av_open(domain_p, &mut av_attr, &mut av_p, ptr::null_mut()) };
            check("fi_av_open", rc)?;
            let av_guard = CloseFidOnDrop(ffi::as_fid_av(av_p));

            // fi_endpoint
            let mut ep_p: *mut ffi::fid_ep = ptr::null_mut();
            let rc = unsafe { ffi::ub_fi_endpoint(domain_p, info, &mut ep_p, ptr::null_mut()) };
            check("fi_endpoint", rc)?;
            let ep_guard = CloseFidOnDrop(ffi::as_fid_ep(ep_p));

            // Bind AV to EP.
            let rc = unsafe { ffi::ub_fi_ep_bind(ep_p, ffi::as_fid_av(av_p), 0) };
            check("fi_ep_bind(av)", rc)?;

            // Open one CQ per progress thread and spawn the threads.
            // Bind CQ #0 to the EP for both TX and RX. Later phases
            // can rebind or split tx/rx onto separate CQs.
            let mut cqs: Vec<CloseFidOnDrop> = Vec::with_capacity(cfg.progress_threads as usize);
            let mut cq_raw: Vec<*mut ffi::fid_cq> =
                Vec::with_capacity(cfg.progress_threads as usize);
            let mut progress: Vec<ProgressThread> =
                Vec::with_capacity(cfg.progress_threads as usize);
            for i in 0..cfg.progress_threads {
                let mut cq_attr: ffi::fi_cq_attr = unsafe { std::mem::zeroed() };
                cq_attr.format = ffi::FI_CQ_FORMAT_TAGGED;
                cq_attr.wait_obj = ffi::FI_WAIT_NONE;
                let mut cq_p: *mut ffi::fid_cq = ptr::null_mut();
                let rc = unsafe {
                    ffi::ub_fi_cq_open(domain_p, &mut cq_attr, &mut cq_p, ptr::null_mut())
                };
                check("fi_cq_open", rc)?;
                cqs.push(CloseFidOnDrop(ffi::as_fid_cq(cq_p)));
                cq_raw.push(cq_p);

                if i == 0 {
                    let rc = unsafe {
                        ffi::ub_fi_ep_bind(
                            ep_p,
                            ffi::as_fid_cq(cq_p),
                            ffi::FI_TRANSMIT | ffi::FI_RECV,
                        )
                    };
                    check("fi_ep_bind(cq)", rc)?;
                }

                let name = format!("fabric-progress-{i}");
                let pt = ProgressThread::spawn(&cfg, cq_p, &name)?;
                progress.push(pt);
            }

            // fi_enable.
            let rc = unsafe { ffi::ub_fi_enable(ep_p) };
            check("fi_enable", rc)?;

            // Transfer ownership: dismiss the guards, hand the raw
            // pointers to `FabricInner`, which will clean them up in
            // its own `Drop` in the documented order.
            std::mem::forget(fabric_guard);
            std::mem::forget(domain_guard);
            std::mem::forget(av_guard);
            std::mem::forget(ep_guard);
            for c in cqs {
                std::mem::forget(c);
            }
            // info is still owned by `info_guard`; we move that into
            // the FabricInner too.
            let info_ptr = info_guard.0;
            std::mem::forget(info_guard);

            let completions = CompletionRegistry::new(cfg.max_inflight);

            let fabric = Fabric {
                inner: Arc::new(FabricInner {
                    info: info_ptr,
                    fabric: fabric_p,
                    domain: domain_p,
                    av: av_p,
                    ep: ep_p,
                    cfg: cfg.clone(),
                    connections: ConnectionTable::new(),
                    mrs: RwLock::new(Vec::new()),
                    next_mr_key: AtomicU64::new(0),
                    mr_virt_addr,
                    threading,
                    domain_mr_cnt,
                    progress,
                    progress_cqs: cq_raw,
                    completions,
                }),
            };
            // Post the ping responder's initial recv so peers can
            // start sending pings as soon as the fabric is up.
            super::ping::install_ping_responder(&fabric)?;
            Ok(fabric)
        })();

        // _hints_guard drops here, freeing the hints (which fi_getinfo
        // does not consume - it returns a fresh info chain).
        drop(_hints_guard);
        result
    }

    /// Raw self-address bytes via `fi_getname`. Callers stringify
    /// per provider conventions (verbs uses an opaque blob; tcp
    /// returns a sockaddr).
    pub fn self_address(&self) -> Result<Vec<u8>> {
        // Probe length first.
        let mut len: usize = 0;
        let rc =
            unsafe { ffi::ub_fi_getname(ffi::as_fid_ep(self.inner.ep), ptr::null_mut(), &mut len) };
        // libfabric returns -FI_ETOOSMALL on the size probe; some
        // providers return 0 with `len` set. Either way, `len` is the
        // required buffer size.
        if rc != 0 {
            // Tolerate the documented "too small" return; fall through
            // and trust `len`.
            if len == 0 {
                return Err(FabricError::Pkg("fi_getname (size probe)", rc));
            }
        }
        let mut buf = vec![0u8; len];
        let rc = unsafe {
            ffi::ub_fi_getname(
                ffi::as_fid_ep(self.inner.ep),
                buf.as_mut_ptr() as *mut std::ffi::c_void,
                &mut len,
            )
        };
        check("fi_getname", rc)?;
        buf.truncate(len);
        Ok(buf)
    }

    /// Shared completion registry for later phases' RPC submissions.
    pub(crate) fn completions(&self) -> &Arc<CompletionRegistry> {
        &self.inner.completions
    }

    pub(crate) fn inner(&self) -> &Arc<FabricInner> {
        &self.inner
    }

    pub(crate) fn inner_arc(&self) -> Arc<FabricInner> {
        self.inner.clone()
    }

    /// True when the negotiated provider addresses remote RMA targets
    /// by virtual address; false when it uses 0-based MR offsets.
    pub(crate) fn mr_uses_virtual_addr(&self) -> bool {
        self.inner.mr_virt_addr
    }

    /// The provider's negotiated `domain_attr->threading` mode (raw
    /// `enum fi_threading` discriminant).
    pub fn threading_mode(&self) -> i32 {
        self.inner.threading
    }

    /// Whether the negotiated domain threading mode is `FI_THREAD_SAFE`,
    /// i.e. the domain may be posted to concurrently from multiple
    /// threads without external serialization. When false, a single
    /// shared per-HCA domain must funnel outbound posting through a
    /// single NIC-worker thread.
    pub fn is_thread_safe(&self) -> bool {
        self.inner.threading == unsafe { ffi::ub_fi_thread_safe_value() }
    }

    /// The provider's negotiated maximum number of memory regions for
    /// this domain, or 0 when the provider advertises no fixed limit.
    pub fn domain_mr_limit(&self) -> usize {
        self.inner.domain_mr_cnt
    }

    /// Verify that `expected_registrations` memory regions can be
    /// registered against this (potentially shared) domain and log the
    /// negotiated threading mode and MR capacity for diagnosis.
    ///
    /// When many serving shards share one per-HCA domain, each shard
    /// registers its pool backing against the domain and the domain
    /// holds a single shared RPC scratch region, so the per-domain MR
    /// count is `shards + 1` and grows with the shard count. Providers
    /// cap the number of MRs per domain (`domain_attr->mr_cnt`);
    /// exceeding it makes a later `fi_mr_reg` fail at bring-up. This
    /// surfaces the risk up front. A reported limit of 0 means "no fixed
    /// limit", in which case no warning is emitted. Returns true when the
    /// expected count exceeds a non-zero limit.
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

/// Whether `expected` memory-region registrations exceed a per-domain
/// `limit`. A `limit` of 0 means the provider advertises no fixed cap,
/// so nothing can exceed it.
fn mr_capacity_exceeds(limit: usize, expected: usize) -> bool {
    limit != 0 && expected > limit
}

/// Probe whether `provider`'s libfabric provider can satisfy this
/// crate's endpoint requirements in the current environment.
///
/// Some hosts expose an RDMA-capable device under
/// `/sys/class/infiniband` - so topology discovers an HCA and selects
/// the `verbs` provider - while libfabric has no working `verbs`
/// provider for it. This is a common shape on cloud VMs whose `mlx5`
/// device backs an accelerated-networking datapath rather than a usable
/// user-space verbs stack. On such hosts the first `fi_getinfo` for
/// `verbs` returns `-FI_ENODATA`, which otherwise crashes every shard at
/// bring-up. Call this before committing the topology to RDMA so the
/// daemon can fall back to the `tcp` provider instead.
///
/// Returns `true` when libfabric returns at least one matching `fi_info`
/// for the provider, and `false` otherwise (including `-FI_ENODATA` and
/// any other failure). The probe issues a single non-binding
/// `fi_getinfo` (no node/service, no `FI_SOURCE`) and frees everything it
/// allocates.
pub fn provider_available(provider: Provider) -> bool {
    let prov_name = match provider {
        Provider::Verbs => "verbs",
        Provider::Tcp => "tcp",
    };
    let Ok(prov_cstr) = CString::new(prov_name) else {
        return false;
    };

    let hints = unsafe { ffi::ub_fi_build_hints(prov_cstr.as_ptr()) };
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
        // SAFETY: `info` is a fresh chain returned by `fi_getinfo`.
        unsafe { ffi::fi_freeinfo(info) };
    }
    // SAFETY: `hints` was allocated by `ub_fi_build_hints`; `fi_getinfo`
    // does not consume it.
    unsafe { ffi::fi_freeinfo(hints) };

    available
}

impl FabricInner {
    pub(crate) fn ep(&self) -> *mut ffi::fid_ep {
        self.ep
    }
    pub(crate) fn domain(&self) -> *mut ffi::fid_domain {
        self.domain
    }
    pub(crate) fn av(&self) -> *mut ffi::fid_av {
        self.av
    }
}

impl Drop for FabricInner {
    fn drop(&mut self) {
        // Join progress threads first so no CQ poll races our close.
        self.progress.clear();
        // Close in libfabric's documented teardown order: MRs (which
        // depend on the domain), endpoint, AV, CQs (after the threads
        // polling them have joined), domain, fabric, then free the
        // info chain.
        unsafe {
            if let Ok(mut mrs) = self.mrs.write() {
                for mr in mrs.drain(..) {
                    if !mr.is_null() {
                        let _ = ffi::ub_fi_close(ffi::as_fid_mr(mr));
                    }
                }
            }
            if !self.ep.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_ep(self.ep));
            }
            if !self.av.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_av(self.av));
            }
            for cq in self.progress_cqs.drain(..) {
                if !cq.is_null() {
                    let _ = ffi::ub_fi_close(ffi::as_fid_cq(cq));
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
        }
    }
}

/// Drop guard that closes a libfabric resource if still alive at
/// scope exit. Used to keep the construction path in `Fabric::new`
/// linear.
struct CloseFidOnDrop(*mut ffi::fid);
impl Drop for CloseFidOnDrop {
    fn drop(&mut self) {
        if !self.0.is_null() {
            // SAFETY: each guard wraps a freshly-opened libfabric
            // resource; close exactly once on the early-return path.
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
            // SAFETY: each guard wraps an info chain returned by
            // libfabric (or `ub_fi_build_hints`); fi_freeinfo handles
            // both.
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

    /// Probe whether the tcp provider can be brought up in this
    /// environment. Returns `None` if libfabric reports no tcp
    /// provider; the caller then skips cleanly.
    fn tcp_provider_available() -> bool {
        let prov = CString::new("tcp").unwrap();
        let hints = unsafe { ffi::ub_fi_build_hints(prov.as_ptr()) };
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
        if !info.is_null() {
            unsafe { ffi::fi_freeinfo(info) };
        }
        unsafe { ffi::fi_freeinfo(hints) };
        rc == 0
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
        // Two progress threads is the default; reduce to one to keep
        // the test lean.
        cfg.progress_threads = 1;
        cfg.max_inflight = 16;
        // Pin the source bind to loopback so self_address() is
        // deterministic on multi-NIC hosts (see tcp_loopback_cfg in
        // fabric/tests.rs for the full rationale).
        cfg.listen = true;
        cfg.listen_addr = Some("127.0.0.1:0".to_string());

        let fabric = Fabric::new(cfg)
            .expect("Fabric::new should succeed after tcp provider availability is established");

        // self_address must round-trip without panic; we don't
        // care about the actual bytes, only that the call path
        // through `fi_getname` is wired up.
        let _ = fabric.self_address();
        // Explicit drop to exercise the teardown path here rather
        // than at process exit so any panic is attributed to this
        // test.
        drop(fabric);
    }

    #[test]
    fn provider_available_matches_probe_for_tcp() {
        if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
            eprintln!("FABRIC_SKIP_FFI set; skipping libfabric provider probe test");
            return;
        }
        // The production `provider_available` helper must agree with the
        // hand-rolled probe used to gate the loopback test above: both
        // ask libfabric the same question about the tcp provider.
        assert_eq!(
            provider_available(Provider::Tcp),
            tcp_provider_available(),
            "provider_available(Tcp) must match the local tcp probe",
        );
    }

    #[test]
    fn thread_safe_value_is_fi_thread_safe() {
        // FI_THREAD_SAFE is enum discriminant 1 in libfabric's
        // `enum fi_threading`. Sourcing it from C (rather than
        // hardcoding) keeps `is_thread_safe` correct against the
        // installed headers; assert the contract here. This is a pure
        // constant-returning shim call, so it needs no provider.
        assert_eq!(unsafe { ffi::ub_fi_thread_safe_value() }, 1);
    }

    #[test]
    fn mr_capacity_exceeds_respects_zero_and_limit() {
        // Zero means "no fixed limit": nothing exceeds it.
        assert!(!mr_capacity_exceeds(0, 0));
        assert!(!mr_capacity_exceeds(0, 1_000_000));
        // Under and at the limit are fine; over the limit trips.
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

        // The tcp RDM provider supports FI_THREAD_SAFE; we request it in
        // hints, so it must be negotiated for the shared-domain design to
        // hold. This is the early validation the plan flags as the
        // highest-risk unknown.
        assert!(
            fabric.is_thread_safe(),
            "tcp provider negotiated threading mode {} but FI_THREAD_SAFE is required",
            fabric.threading_mode(),
        );

        // The capacity check must not flag a tiny expected count. A
        // provider reporting no fixed limit (0) never flags; a provider
        // with a real limit must comfortably exceed two registrations.
        let limit = fabric.domain_mr_limit();
        assert!(
            !fabric.check_shared_domain_capacity(2),
            "two registrations must fit the domain mr_cnt limit of {limit}",
        );

        drop(fabric);
    }
}
