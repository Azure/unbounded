// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! libfabric class lifecycle: bring up a fabric, domain, address
//! vector, one endpoint, and one CQ per progress thread, then drop
//! everything in the correct order. Phase 3 stops at "enabled
//! endpoint with progress threads alive"; no connections, MR
//! registration, or actual data traffic happens here.

use std::ffi::CString;
use std::ptr;
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

        let prov_cstr = match cfg.provider {
            Provider::Verbs => CString::new("verbs").expect("static literal"),
            Provider::Tcp => CString::new("tcp").expect("static literal"),
        };

        let hints = unsafe { ffi::ub_fi_build_hints(prov_cstr.as_ptr()) };
        if hints.is_null() {
            return Err(FabricError::Pkg("ub_fi_build_hints", 0));
        }
        let _hints_guard = FreeInfoOnDrop(hints);

        let service_cstr: Option<CString> = match cfg.provider {
            Provider::Tcp => cfg
                .listen_addr
                .as_deref()
                .map(|s| CString::new(s).map_err(|_| FabricError::BadConfig("listen_addr has NUL")))
                .transpose()?,
            Provider::Verbs => None,
        };
        let service_ptr = service_cstr
            .as_ref()
            .map(|s| s.as_ptr())
            .unwrap_or(ptr::null());
        let flags = if cfg.listen { ffi::FI_SOURCE } else { 0 };

        let mut info: *mut ffi::fi_info = ptr::null_mut();
        let rc = unsafe {
            ffi::fi_getinfo(
                ffi::FI_VERSION,
                ptr::null(),
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
                ffi::FI_VERSION,
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
}
