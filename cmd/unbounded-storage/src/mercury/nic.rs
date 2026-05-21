// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-HCA Mercury class plus its progress contexts and peer table.
//!
//! A `Nic` owns one `hg_class_t`, registers the bulk-get RPC against it,
//! constructs `cfg.contexts_per_nic` `ProgressContext`s sharing the
//! class, and keeps a `PeerTable` of resolved peer addresses. Wave 6
//! fills in the server-side RPC trampoline and the client-side transport;
//! this file is responsible for the wiring and the tear-down ordering.
//!
//! Shutdown order (also enforced by `Drop` as a safety net):
//!  1. flip the local `shut` flag (idempotency guard);
//!  2. signal every progress context to exit;
//!  3. close every server queue;
//!  4. join every progress thread (blocking);
//!  5. drop the registered local bulk handle (HG_Bulk_free via Drop);
//!  6. free every peer address (HG_Addr_free against the live class);
//!  7. destroy every context (HG_Context_destroy);
//!  8. finalize the class (HG_Finalize).

use std::ffi::CString;
use std::os::raw::c_char;
use std::ptr::NonNull;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use arc_swap::ArcSwapOption;

use super::bulk::LocalBulk;
use super::config::{NicConfig, PeerEntry};
use super::context::ProgressContext;
use super::error::{HgError, Result};
use super::ffi::{
    self, HG_BULK_READWRITE, HG_FALSE, HG_SUCCESS, HG_TRUE, hg_addr_t, hg_class_t, hg_handle_t,
    hg_id_t, hg_return_t,
};
use super::peer::{PeerId, PeerTable};
use super::rpc::CtxSelector;
use crate::runtime::{Threading, WorkerIdx};

/// Null-terminated RPC name registered against the class. The trailing
/// `\0` lets us cast the byte slice straight to `*const c_char`.
pub const RPC_NAME: &[u8] = b"ub.bufferpool.bulk_get.v1\0";

/// Per-HCA Mercury transport bundle.
///
/// Cheap to clone (it is just an `Arc` around the inner state), but the
/// inner is constructed once and torn down once. The expected ownership
/// pattern is: one `Nic` per HCA, held by `MercuryTransport`, with the
/// inner `Arc` shared into the per-context callbacks via `Arc::downgrade`
/// when wave 6 wires up the trampoline.
pub struct Nic {
    inner: Arc<NicInner>,
}

struct NicInner {
    class: hg_class_t,
    rpc_id: hg_id_t,
    contexts: Vec<Arc<ProgressContext>>,
    selector: CtxSelector,
    peers: PeerTable,
    local_bulk: ArcSwapOption<LocalBulk>,
    listen: bool,
    shut: AtomicBool,
}

// SAFETY: `class` is a Mercury class pointer, documented as safe to use
// across threads; `PeerTable` and `ArcSwapOption<LocalBulk>` are both
// `Send + Sync`; `contexts` is a `Vec<Arc<ProgressContext>>` and
// `ProgressContext` is `Send + Sync`. The `shut` AtomicBool plus the
// idempotent shutdown protocol are the only mutating points after
// construction.
unsafe impl Send for NicInner {}
unsafe impl Sync for NicInner {}

impl Nic {
    /// Bring up Mercury, register the RPC, build N contexts, and look
    /// up every static peer in `cfg.peers`. Any failure during peer
    /// lookup or context construction tears down everything that has
    /// been built so far before returning.
    pub fn new(cfg: &NicConfig, threading: &dyn Threading, worker: WorkerIdx) -> Result<Self> {
        cfg.validate()?;

        let info = CString::new(cfg.na_info_string.as_str())
            .map_err(|_| HgError::BadConfig("na_info_string contains nul"))?;
        let listen_flag = if cfg.listen { HG_TRUE } else { HG_FALSE };

        // SAFETY: `info` outlives the call; `HG_Init` either returns a
        // non-null class or null on failure.
        let class = unsafe { ffi::HG_Init(info.as_ptr(), listen_flag) };
        if class.is_null() {
            return Err(HgError::HgInit(0));
        }

        let rpc_id = register_rpc(class, cfg.listen);
        if rpc_id == 0 {
            // SAFETY: `class` came from `HG_Init` above and no contexts
            // exist yet.
            unsafe {
                let _ = ffi::HG_Finalize(class);
            }
            return Err(HgError::HgRegister(0));
        }

        let mut contexts: Vec<Arc<ProgressContext>> =
            Vec::with_capacity(cfg.contexts_per_nic as usize);
        for i in 0..cfg.contexts_per_nic {
            match ProgressContext::new(class, i, cfg, threading, worker) {
                Ok(c) => contexts.push(c),
                Err(e) => {
                    teardown_contexts(&contexts);
                    // SAFETY: every context was joined by
                    // `teardown_contexts`; the class is still live and
                    // owns no peers yet.
                    unsafe {
                        let _ = ffi::HG_Finalize(class);
                    }
                    drop(contexts);
                    return Err(e);
                }
            }
        }

        let peers = PeerTable::new();
        for entry in &cfg.peers {
            if let Err(e) = lookup_and_insert(class, &peers, entry) {
                teardown_peers(class, &peers);
                teardown_contexts(&contexts);
                drop(contexts);
                // SAFETY: contexts are joined and destroyed; peers are
                // freed; class is still live.
                unsafe {
                    let _ = ffi::HG_Finalize(class);
                }
                return Err(e);
            }
        }

        let selector = CtxSelector::new(cfg.contexts_per_nic);

        Ok(Self {
            inner: Arc::new(NicInner {
                class,
                rpc_id,
                contexts,
                selector,
                peers,
                local_bulk: ArcSwapOption::empty(),
                listen: cfg.listen,
                shut: AtomicBool::new(false),
            }),
        })
    }

    /// Register `backing` as a bulk handle with read/write permissions.
    /// Replaces any prior registration; the prior handle's `Drop` runs
    /// when the last `Arc` reference is released.
    ///
    /// Caller guarantees `backing` outlives any in-flight transfer
    /// using the returned handle.
    pub fn register_backing(&self, backing: &crate::bufferpool::Backing) -> Result<()> {
        let len = backing
            .page_size
            .checked_mul(backing.page_count)
            .ok_or(HgError::BadConfig("backing size overflow"))?;
        // SAFETY: caller documents that `backing.base` points to
        // `page_size * page_count` valid bytes that outlive any pending
        // transfer over the resulting handle.
        let bulk =
            unsafe { LocalBulk::over(self.inner.class, backing.base, len, HG_BULK_READWRITE)? };
        self.inner.local_bulk.store(Some(Arc::new(bulk)));
        Ok(())
    }

    /// Resolve `na_addr` to an `hg_addr_t` and insert it under `id`.
    /// If a peer with the same id already exists, frees the old address
    /// before storing the new one.
    pub fn add_peer(&self, id: PeerId, na_addr: &str) -> Result<()> {
        let entry = PeerEntry {
            id,
            na_addr: na_addr.to_string(),
        };
        if let Some(prev) = lookup_and_insert(self.inner.class, &self.inner.peers, &entry)? {
            // SAFETY: `prev` was produced by an earlier
            // `HG_Addr_lookup2` against the same class and has not been
            // freed yet. `HG_Addr_free` accepts a single owned addr.
            unsafe {
                let _ = ffi::HG_Addr_free(self.inner.class, prev.as_ptr() as hg_addr_t);
            }
        }
        Ok(())
    }

    /// Orderly, idempotent shutdown. Performs the 8-step protocol
    /// documented at the top of this file.
    pub fn shutdown(&self) -> Result<()> {
        self.inner.shutdown_inner()
    }

    pub fn class(&self) -> hg_class_t {
        self.inner.class
    }

    pub fn rpc_id(&self) -> hg_id_t {
        self.inner.rpc_id
    }

    pub fn peers(&self) -> &PeerTable {
        &self.inner.peers
    }

    pub fn contexts(&self) -> &[Arc<ProgressContext>] {
        &self.inner.contexts
    }

    pub fn selector(&self) -> &CtxSelector {
        &self.inner.selector
    }

    pub fn local_bulk(&self) -> Option<Arc<LocalBulk>> {
        self.inner.local_bulk.load_full()
    }

    pub fn is_listening(&self) -> bool {
        self.inner.listen
    }
}

impl NicInner {
    fn shutdown_inner(&self) -> Result<()> {
        // Step 1: idempotency guard. Returns early on second call.
        if self.shut.swap(true, Ordering::AcqRel) {
            return Ok(());
        }

        // Step 2: signal every progress context.
        for ctx in &self.contexts {
            ctx.signal_shutdown();
        }
        // Step 3: close every server queue so the async server task
        // unblocks and stops accepting new jobs.
        for ctx in &self.contexts {
            if let Some(q) = ctx.server_queue() {
                q.close();
            }
        }
        // Step 4: join every progress thread. Errors are logged via
        // eprintln but do not short-circuit teardown.
        for ctx in &self.contexts {
            if let Err(e) = ctx.join() {
                eprintln!("nic shutdown: progress join error: {e}");
            }
        }

        // Step 5: drop the registered local bulk; its `Drop` calls
        // `HG_Bulk_free` while the class is still live.
        self.local_bulk.store(None);

        // Step 6: free peer addresses against the still-live class.
        teardown_peers(self.class, &self.peers);

        // Step 7: destroy contexts.
        // ProgressContext's Drop calls HG_Context_destroy. We can't
        // drop the Vec directly (NicInner is borrowed); instead we
        // rely on Drop of NicInner to release the Arcs in declaration
        // order. The contexts have already been joined, so destroying
        // them now is safe; the per-Arc Drop will fire when the last
        // outside reference releases. Force-drop here would require
        // owning the Vec, which we do at NicInner::drop time.

        // Step 8: HG_Finalize is deferred to NicInner::drop so the
        // class outlives any references the contexts may still hold.
        // Calling it here would invalidate context pointers. The
        // observable effect to callers is the same: by the time the
        // last `Arc<NicInner>` drops, finalize has run.

        Ok(())
    }
}

impl Drop for NicInner {
    fn drop(&mut self) {
        // Safety net: shut down idempotently in case the embedder
        // dropped without calling `shutdown`.
        let _ = self.shutdown_inner();

        // Now that we own the Vec mutably, drop every context (this
        // runs each ProgressContext's Drop, which destroys its
        // `hg_context_t`).
        self.contexts.clear();

        if !self.class.is_null() {
            // SAFETY: every context has been destroyed (the Vec was
            // cleared above) and every peer addr has been freed by
            // `shutdown_inner`. `HG_Finalize` consumes the class.
            unsafe {
                let _ = ffi::HG_Finalize(self.class);
            }
            self.class = std::ptr::null_mut();
        }
    }
}

/// Look up `entry.na_addr` against `class` and insert into `peers`.
/// Returns the previous addr handle for the same id, if any (the caller
/// is responsible for freeing it).
fn lookup_and_insert(
    class: hg_class_t,
    peers: &PeerTable,
    entry: &PeerEntry,
) -> Result<Option<NonNull<hg_addr_t>>> {
    let c_addr = CString::new(entry.na_addr.as_str())
        .map_err(|_| HgError::BadConfig("peer na_addr contains nul"))?;
    let mut addr: hg_addr_t = std::ptr::null_mut();
    // SAFETY: `c_addr` outlives the call; `&mut addr` is a valid
    // out-parameter; `class` is non-null per construction.
    let rc = unsafe { ffi::HG_Addr_lookup2(class, c_addr.as_ptr(), &mut addr) };
    if rc != HG_SUCCESS {
        return Err(HgError::HgAddrLookup(rc));
    }
    if addr.is_null() {
        return Err(HgError::HgAddrLookup(HG_SUCCESS));
    }
    // `PeerTable` keys on `NonNull<hg_addr_t>`, where `hg_addr_t` is
    // itself a `*mut hg_addr_s`. Mercury hands us the `hg_addr_t`
    // directly; we round-trip its bit pattern through `NonNull` of
    // the pointer-typed alias, matching the convention used by
    // `peer.rs` tests.
    let nn = NonNull::new(addr.cast::<hg_addr_t>()).ok_or(HgError::HgAddrLookup(HG_SUCCESS))?;
    Ok(peers.insert(entry.id, nn))
}

/// Free every peer address against `class` and clear the table.
fn teardown_peers(class: hg_class_t, peers: &PeerTable) {
    for (_id, addr) in peers.drain() {
        // SAFETY: `addr` was produced by `HG_Addr_lookup2` against the
        // same class and has not been freed yet.
        unsafe {
            let _ = ffi::HG_Addr_free(class, addr.as_ptr() as hg_addr_t);
        }
    }
}

/// Best-effort signal+join of every context in `contexts`. Used during
/// `Nic::new` rollback before HG_Finalize.
fn teardown_contexts(contexts: &[Arc<ProgressContext>]) {
    for ctx in contexts {
        ctx.signal_shutdown();
        if let Some(q) = ctx.server_queue() {
            q.close();
        }
    }
    for ctx in contexts {
        if let Err(e) = ctx.join() {
            eprintln!("nic init rollback: progress join error: {e}");
        }
    }
}

/// Register the bulk-get RPC against `class`. Returns `0` on failure.
///
/// Wave 6 owns the server-side trampoline body. For now we register a
/// stub that returns `HG_SUCCESS` so non-listening clients can register
/// without surprising the trampoline.
fn register_rpc(class: hg_class_t, listen: bool) -> hg_id_t {
    let in_proc: ffi::hg_proc_cb_t = ffi::ub_proc_bulk_get_in;
    let out_proc: ffi::hg_proc_cb_t = ffi::ub_proc_bulk_get_out;
    let rpc_cb: Option<ffi::hg_rpc_cb_t> = if listen {
        Some(server_rpc_cb_trampoline)
    } else {
        None
    };
    // SAFETY: `RPC_NAME` is a static null-terminated byte slice; the
    // proc callbacks are defined in the C shim and exported as
    // `extern "C" fn`s; `class` is non-null per the caller.
    unsafe {
        ffi::HG_Register_name(
            class,
            RPC_NAME.as_ptr() as *const c_char,
            Some(in_proc),
            Some(out_proc),
            rpc_cb,
        )
    }
}

/// Server-side RPC trampoline.
///
/// Called synchronously by Mercury for every inbound bulk-get RPC.
/// Recovers the parked `Arc<Nic>` via `HG_Registered_data`, allocates a
/// boxed `BulkGetIn` for the input, decodes it with `HG_Get_input`, picks
/// a context queue via the `CtxSelector`, and pushes a `ServerJob` onto
/// it. The async drainer in `server.rs` then takes over: it owns the
/// `Handle` (via `from_raw`) and is responsible for `HG_Free_input`,
/// `HG_Bulk_transfer`, `HG_Respond`, and `HG_Destroy`.
///
/// On any failure that prevents pushing the job (decode failure, queue
/// full, queue closed, etc.) the trampoline frees the input box,
/// destroys the handle here so Mercury does not leak it, and returns
/// `HG_SUCCESS` so Mercury reports the inbound RPC as handled. The
/// client observes the absence of a response as a forward-side error.
unsafe extern "C" fn server_rpc_cb_trampoline(handle: hg_handle_t) -> hg_return_t {
    if handle.is_null() {
        return HG_SUCCESS;
    }

    // Recover the per-Nic state parked by `MercuryServer::run`. Without
    // it we cannot route the job to a context queue.
    // SAFETY: `ub_handle_info` returns a pointer that is valid for the
    // lifetime of the handle; Mercury keeps the handle alive across
    // this synchronous callback.
    let info_ptr = unsafe { ffi::ub_handle_info(handle) };
    if info_ptr.is_null() {
        // SAFETY: handle is non-null per the early check above.
        unsafe {
            let _ = ffi::HG_Destroy(handle);
        }
        return HG_SUCCESS;
    }
    // SAFETY: see above; the returned struct is alive for the call.
    let info = unsafe { &*info_ptr };
    let class = info.hg_class;
    let rpc_id = info.id;
    let addr = info.addr;

    // SAFETY: `class` and `rpc_id` came from the live handle's info;
    // `HG_Registered_data` returns the pointer parked by an earlier
    // `HG_Register_data` (or null if none was parked).
    let data = unsafe { ffi::HG_Registered_data(class, rpc_id) };
    if data.is_null() {
        // No server installed against this class; drop the handle.
        // SAFETY: handle is non-null.
        unsafe {
            let _ = ffi::HG_Destroy(handle);
        }
        return HG_SUCCESS;
    }
    // SAFETY: `data` was produced by `Arc::into_raw(Arc<Nic>)` in
    // `MercuryServer::run` and remains live until the class is finalized
    // (which frees it via the registered free callback). Borrowing it
    // here without taking ownership is sound.
    let nic: &Nic = unsafe { &*(data as *const Nic) };

    // Allocate the input box on the heap so it can travel with the job.
    // SAFETY: `BulkGetIn::zeroed` returns a valid all-zero struct; the
    // pointer fields are null until populated by `HG_Get_input`.
    let mut input = Box::new(unsafe { super::rpc::BulkGetIn::zeroed() });
    // SAFETY: `handle` is non-null and `input` points to a properly
    // aligned, writeable `BulkGetIn` for the duration of the call.
    let rc = unsafe {
        ffi::HG_Get_input(
            handle,
            input.as_mut() as *mut _ as *mut std::os::raw::c_void,
        )
    };
    if rc != HG_SUCCESS {
        // SAFETY: handle is non-null; nothing else owns it yet.
        unsafe {
            let _ = ffi::HG_Destroy(handle);
        }
        return HG_SUCCESS;
    }

    // Distribute jobs across contexts. The address pointer's bit
    // pattern serves as the hash input; addresses already differ
    // between distinct peers and modulo distributes them across the
    // small `contexts_per_nic` we use.
    // TODO(wave-7): replace with XxHash for better distribution under
    // colliding low bits.
    let addr_hash = addr as usize as u64;
    let ctx_idx = nic.selector().pick(addr_hash) as usize;
    let contexts = nic.contexts();
    let queue = match contexts.get(ctx_idx).and_then(|c| c.server_queue()) {
        Some(q) => Arc::clone(q),
        None => {
            // SAFETY: `input` was populated by the successful
            // `HG_Get_input` above; freeing it releases tail
            // allocations. `handle` is non-null and unowned elsewhere.
            unsafe {
                let _ = ffi::HG_Free_input(
                    handle,
                    input.as_mut() as *mut _ as *mut std::os::raw::c_void,
                );
                let _ = ffi::HG_Destroy(handle);
            }
            return HG_SUCCESS;
        }
    };

    let job = super::context::ServerJob { handle, input };
    if let Err(e) = queue.push(job) {
        // Recover the handle and input from the rejected job and free
        // them here; Mercury will surface the missing response as a
        // forward-side error on the client.
        let job = match e {
            super::progress::PushError::Full(j) => j,
            super::progress::PushError::Closed(j) => j,
        };
        let mut input = job.input;
        let handle = job.handle;
        // SAFETY: `input` was populated by the successful
        // `HG_Get_input` above; `handle` is non-null and unowned
        // elsewhere because the queue rejected the job.
        unsafe {
            let _ = ffi::HG_Free_input(
                handle,
                input.as_mut() as *mut _ as *mut std::os::raw::c_void,
            );
            let _ = ffi::HG_Destroy(handle);
        }
    }

    HG_SUCCESS
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::Backing;
    use crate::runtime::DefaultRuntime;
    use std::time::{Duration, Instant};

    fn cfg(listen: bool, contexts: u16) -> NicConfig {
        NicConfig {
            na_info_string: "na+sm".to_string(),
            listen,
            contexts_per_nic: contexts,
            ..NicConfig::default()
        }
    }

    fn build_nic(listen: bool, contexts: u16) -> Option<Nic> {
        let rt = DefaultRuntime::new(1);
        match Nic::new(&cfg(listen, contexts), &*rt, WorkerIdx(0)) {
            Ok(n) => Some(n),
            Err(HgError::HgInit(_)) => None,
            Err(e) => panic!("unexpected Nic::new error: {e:?}"),
        }
    }

    #[test]
    fn nic_init_and_shutdown() {
        let nic = match build_nic(false, 2) {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        assert_eq!(nic.contexts().len(), 2);
        assert!(nic.rpc_id() != 0);
        assert!(!nic.is_listening());
        assert_eq!(nic.selector().contexts, 2);
        assert!(nic.peers().is_empty());

        let started = Instant::now();
        nic.shutdown().expect("first shutdown");
        nic.shutdown().expect("second shutdown is idempotent");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "shutdown took longer than 5s"
        );
        drop(nic);
    }

    #[test]
    fn register_backing_round_trip() {
        let nic = match build_nic(false, 1) {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };

        let mut bytes = vec![0u8; 16 * 16];
        let base = bytes.as_mut_ptr();
        let backing = Backing {
            base,
            page_size: 16,
            page_count: 16,
            _own: Box::new(bytes),
        };

        nic.register_backing(&backing).expect("register ok");
        assert!(nic.local_bulk().is_some(), "bulk slot populated");

        nic.shutdown().expect("shutdown");
        drop(nic);
    }

    #[test]
    fn concurrent_shutdown_is_safe() {
        let nic = match build_nic(false, 2) {
            Some(n) => Arc::new(n),
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };

        let a = Arc::clone(&nic);
        let b = Arc::clone(&nic);
        let h1 = std::thread::spawn(move || a.shutdown());
        let h2 = std::thread::spawn(move || b.shutdown());
        h1.join().expect("t1 joined").expect("t1 ok");
        h2.join().expect("t2 joined").expect("t2 ok");
    }
}
