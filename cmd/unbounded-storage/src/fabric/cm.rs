// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Connection manager for the `FI_EP_MSG` transport.
//!
//! The native verbs and tcp MSG endpoint types are connection
//! oriented: a passive endpoint ([`Listener`]) listens on a bound
//! address, active endpoints dial it with [`connect`], and the
//! resulting [`Connection`] owns one active `fid_ep` plus its
//! completion queue. Connection-state transitions (`FI_CONNREQ`,
//! `FI_CONNECTED`, `FI_SHUTDOWN`) are delivered on an event queue
//! (`fid_eq`) rather than a CQ.
//!
//! Two EQ-ownership rules follow from how libfabric routes these
//! events:
//!
//! - A [`Listener`] owns its EQ. The passive endpoint's `FI_CONNREQ`
//!   events and every accepted active endpoint's `FI_CONNECTED` event
//!   are delivered on that one shared EQ, so an accepted [`Connection`]
//!   borrows the listener's EQ (`owns_eq == false`) and must not close
//!   it. The listener therefore has to outlive every connection it
//!   accepts.
//! - A dialing [`Connection`] (from [`connect`]) opens and owns its own
//!   EQ (`owns_eq == true`); it closes it on drop.
//!
//! Peer identity is carried in the connection handshake's private data:
//! the dialer sends its own 8-byte [`PeerId`] as the `fi_connect`
//! parameter, and the accepting side reads it back out of the
//! `FI_CONNREQ` event's trailing private-data bytes.
//!
//! This module is intentionally low level: it operates on raw
//! `fid_fabric` / `fid_domain` / `fi_info` pointers owned by the
//! caller and never creates or frees them (with the sole exception of
//! the `fi_info` libfabric hands back inside a `FI_CONNREQ`, which the
//! acceptor must consume). The higher-level fabric bring-up wires these
//! primitives to a real domain.

// The connection-manager surface is intentionally complete: it exposes
// the full set of low-level primitives (single-shot accept, event
// waits, ...) even though production bring-up only drives a subset.
#![allow(dead_code)]

use std::collections::HashMap;
use std::ptr;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use super::error::{FabricError, Result, check};
use super::ffi;
use super::types::PeerId;

/// Bounded wait for a single connection-manager event, in
/// milliseconds. Generous: bring-up handshakes complete in well under a
/// second on both loopback tcp and IPoIB verbs, so this only bounds the
/// pathological "peer never answers" case rather than steady state.
const CM_EVENT_TIMEOUT_MS: i32 = 10_000;

/// Size of the connect private-data payload: an 8-byte [`PeerId`]
/// (`to_ne_bytes`, same host on both ends in tests) followed by the QP
/// index and QP total as little-endian `u16`s. Multi-QP connections
/// dial `qp_total` endpoints, each tagged with its `qp_index`, so the
/// acceptor can group the `qp_total` inbound requests from one peer into
/// a single logical [`Connection`].
const CONNECT_PRIVATE_LEN: usize = 12;

/// Passive (listening) endpoint plus its connection-event queue. Owns
/// both; closes them on drop. Must outlive every [`Connection`] it
/// accepts, since those share this EQ.
pub(crate) struct Listener {
    pep: *mut ffi::fid_pep,
    eq: *mut ffi::fid_eq,
    /// Per-peer accumulation of a multi-QP connection in progress. The
    /// dialer issues `qp_total` separate `fi_connect`s; their `CONNREQ`
    /// and `CONNECTED` events arrive interleaved on the shared listener
    /// EQ (mixed with other peers' events), so each peer's accepted
    /// endpoints are staged here keyed by [`PeerId`] until all
    /// `qp_total` have reached `FI_CONNECTED`, at which point they are
    /// handed back as one [`Connection`].
    staging: Mutex<HashMap<u64, Partial>>,
    /// Maps an accepted endpoint's `fid` pointer (as `usize`) back to the
    /// dialing [`PeerId`], so a `FI_CONNECTED` event (which identifies
    /// only the endpoint) can be attributed to the right staged peer.
    ep_to_peer: Mutex<HashMap<usize, u64>>,
}

/// A multi-QP connection being assembled on the acceptor side: the
/// endpoints/CQs accepted so far for one peer, plus the expected total.
struct Partial {
    total: usize,
    numa: Option<u16>,
    eps: Vec<*mut ffi::fid_ep>,
    cqs: Vec<*mut ffi::fid_cq>,
    accepted: usize,
}

/// Outcome of processing a single connection-manager event.
enum AcceptProgress {
    /// A peer's full set of `qp_total` endpoints is now connected.
    Completed(Connection),
    /// An event was processed (a CONNREQ accepted, or a partial
    /// CONNECTED) but no connection is complete yet.
    Progressed,
    /// No event was available within the timeout window.
    Idle,
}

impl Listener {
    /// Open an EQ on `fabric`, create a passive endpoint from `info`
    /// (which must carry the desired source address, e.g. from
    /// `fi_getinfo` with `FI_SOURCE`), bind the EQ, and start listening.
    ///
    /// `info` is borrowed: it is not consumed or freed here. The caller
    /// retains ownership and must keep it alive at least until this call
    /// returns.
    pub(crate) fn new(fabric: *mut ffi::fid_fabric, info: *mut ffi::fi_info) -> Result<Self> {
        let eq = open_eq(fabric)?;
        let eq_guard = FidGuard(ffi::as_fid_eq(eq));

        let mut pep: *mut ffi::fid_pep = ptr::null_mut();
        let rc = unsafe { ffi::ub_fi_passive_ep(fabric, info, &mut pep, ptr::null_mut()) };
        check("fi_passive_ep", rc)?;
        let pep_guard = FidGuard(ffi::as_fid_pep(pep));

        let rc = unsafe { ffi::ub_fi_pep_bind(pep, ffi::as_fid_eq(eq), 0) };
        check("fi_pep_bind(eq)", rc)?;

        let rc = unsafe { ffi::ub_fi_listen(pep) };
        check("fi_listen", rc)?;

        std::mem::forget(eq_guard);
        std::mem::forget(pep_guard);
        Ok(Listener {
            pep,
            eq,
            staging: Mutex::new(HashMap::new()),
            ep_to_peer: Mutex::new(HashMap::new()),
        })
    }

    /// The local address the passive endpoint is bound to, rendered as
    /// numeric "ip:port" (or "[ip]:port" for IPv6). Resolves the actual
    /// port chosen by the provider when the source was bound with port
    /// 0.
    pub(crate) fn local_addr(&self) -> Result<String> {
        getname_string(ffi::as_fid_pep(self.pep))
    }

    /// Block until one peer's full multi-QP connection is established on
    /// `domain` and return it. The returned connection borrows this
    /// listener's EQ, so it must be dropped before the listener.
    ///
    /// `numa` is attached to the connection as locality metadata; it is
    /// not interpreted here.
    pub(crate) fn accept_one(
        &self,
        domain: *mut ffi::fid_domain,
        numa: Option<u16>,
    ) -> Result<Connection> {
        loop {
            match self.poll_accept_event(domain, numa, CM_EVENT_TIMEOUT_MS)? {
                AcceptProgress::Completed(conn) => return Ok(conn),
                AcceptProgress::Progressed => continue,
                AcceptProgress::Idle => return Err(FabricError::Timeout),
            }
        }
    }

    /// Process at most one connection-manager event, waiting up to
    /// `timeout_ms` for it. Returns `Ok(Some(conn))` only when an event
    /// completes a peer's full `qp_total` endpoint set; otherwise
    /// `Ok(None)` (a clean timeout, or an intermediate CONNREQ/CONNECTED
    /// that did not yet complete a group). An accept loop calls this
    /// repeatedly, polling a shutdown flag between calls; intermediate
    /// progress and idle both collapse to `None` because the loop simply
    /// calls again.
    pub(crate) fn accept_one_timeout(
        &self,
        domain: *mut ffi::fid_domain,
        numa: Option<u16>,
        timeout_ms: i32,
    ) -> Result<Option<Connection>> {
        match self.poll_accept_event(domain, numa, timeout_ms)? {
            AcceptProgress::Completed(conn) => Ok(Some(conn)),
            AcceptProgress::Progressed | AcceptProgress::Idle => Ok(None),
        }
    }

    /// Read and act on a single EQ event:
    ///
    /// - `FI_CONNREQ`: build and enable the active endpoint, `fi_accept`
    ///   it, and stage it under the dialer's [`PeerId`]. Yields
    ///   `Progressed`.
    /// - `FI_CONNECTED`: attribute the now-connected endpoint to its
    ///   staged peer; if that completes the peer's `qp_total` set, return
    ///   the assembled `Completed(Connection)`, else `Progressed`.
    /// - `FI_SHUTDOWN`: best-effort teardown of any partial for that
    ///   endpoint's peer. Yields `Progressed`.
    /// - clean timeout: `Idle`.
    fn poll_accept_event(
        &self,
        domain: *mut ffi::fid_domain,
        numa: Option<u16>,
        timeout_ms: i32,
    ) -> Result<AcceptProgress> {
        let mut buf = [0u8; 256];
        let (event, n) = match eq_read_timeout(self.eq, &mut buf, timeout_ms)? {
            Some(v) => v,
            None => return Ok(AcceptProgress::Idle),
        };
        let entry = buf.as_ptr() as *const ffi::fi_eq_cm_entry;

        if event == unsafe { ffi::ub_fi_connreq() } {
            let req_info = unsafe { (*entry).info };
            if req_info.is_null() {
                return Err(FabricError::NotFound("CONNREQ carried no fi_info"));
            }
            // From here the CONNREQ info must be freed on every path.
            let info_guard = FreeInfoGuard(req_info);

            let (peer, _qp_index, qp_total) = read_peer_private(&buf, n)?;

            // Build and enable the active endpoint against the shared EQ.
            let (ep, cq) = bring_up_active(domain, req_info, self.eq)?;
            let ep_guard = FidGuard(ffi::as_fid_ep(ep));
            let cq_guard = FidGuard(ffi::as_fid_cq(cq));

            let rc = unsafe { ffi::ub_fi_accept(ep, ptr::null(), 0) };
            check("fi_accept", rc)?;

            // Stage the accepted endpoint; do NOT inline-wait CONNECTED
            // (it arrives later on the shared EQ, possibly interleaved
            // with other peers' events).
            {
                let mut staging = self.staging.lock().unwrap();
                let partial = staging.entry(peer.0).or_insert_with(|| Partial {
                    total: qp_total as usize,
                    numa,
                    eps: Vec::with_capacity(qp_total as usize),
                    cqs: Vec::with_capacity(qp_total as usize),
                    accepted: 0,
                });
                partial.total = qp_total as usize;
                partial.eps.push(ep);
                partial.cqs.push(cq);
            }
            self.ep_to_peer
                .lock()
                .unwrap()
                .insert(ffi::as_fid_ep(ep) as usize, peer.0);

            std::mem::forget(ep_guard);
            std::mem::forget(cq_guard);
            drop(info_guard);
            return Ok(AcceptProgress::Progressed);
        }

        if event == unsafe { ffi::ub_fi_connected() } {
            let key = unsafe { (*entry).fid } as usize;
            let peer0 = match self.ep_to_peer.lock().unwrap().remove(&key) {
                Some(p) => p,
                // A CONNECTED for an endpoint we did not stage (already
                // torn down, or a duplicate); ignore it.
                None => return Ok(AcceptProgress::Progressed),
            };
            let mut staging = self.staging.lock().unwrap();
            let complete = match staging.get_mut(&peer0) {
                Some(partial) => {
                    partial.accepted += 1;
                    partial.accepted >= partial.total
                }
                None => return Ok(AcceptProgress::Progressed),
            };
            if complete {
                let partial = staging.remove(&peer0).unwrap();
                return Ok(AcceptProgress::Completed(Connection {
                    peer: PeerId(peer0),
                    eps: partial.eps,
                    cqs: partial.cqs,
                    eq: self.eq,
                    owns_eq: false,
                    numa: partial.numa,
                    next: AtomicUsize::new(0),
                }));
            }
            return Ok(AcceptProgress::Progressed);
        }

        if event == unsafe { ffi::ub_fi_shutdown() } {
            let key = unsafe { (*entry).fid } as usize;
            if let Some(peer0) = self.ep_to_peer.lock().unwrap().remove(&key) {
                if let Some(partial) = self.staging.lock().unwrap().remove(&peer0) {
                    close_partial(partial);
                }
            }
            return Ok(AcceptProgress::Progressed);
        }

        Err(FabricError::Pkg(
            "fi_eq_sread(unexpected event)",
            event as i32,
        ))
    }
}

impl Drop for Listener {
    fn drop(&mut self) {
        // Close any half-assembled connections' endpoints/CQs before the
        // shared EQ they are bound to (and before the passive endpoint).
        if let Ok(mut staging) = self.staging.lock() {
            for (_, partial) in staging.drain() {
                close_partial(partial);
            }
        }
        // Close the passive endpoint before its EQ.
        unsafe {
            if !self.pep.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_pep(self.pep));
            }
            if !self.eq.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_eq(self.eq));
            }
        }
    }
}

/// Close every endpoint then every CQ held by a still-partial accepted
/// connection. Endpoints are closed before CQs (matching
/// [`Connection`]'s drop order); the shared listener EQ is not touched.
fn close_partial(partial: Partial) {
    unsafe {
        for ep in partial.eps {
            if !ep.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_ep(ep));
            }
        }
        for cq in partial.cqs {
            if !cq.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_cq(cq));
            }
        }
    }
}

// SAFETY: the contained pointers are libfabric resources whose
// documented operations are internally synchronized. The listener is
// shared across the accepting thread and the owner via an `Arc`; only
// `accept_one` (which takes `&self`) touches the EQ, and libfabric
// permits concurrent EQ readers.
unsafe impl Send for Listener {}
unsafe impl Sync for Listener {}

/// One established connection: a bundle of one or more active
/// `FI_EP_MSG` endpoints (QPs) and their completion queues, tagged with
/// the remote [`PeerId`] and NUMA locality. Outbound requests
/// round-robin across the endpoints via [`Connection::next_ep`]. Owns
/// the endpoints and CQs; owns the EQ only when it dialed out (see
/// [`connect`]).
pub(crate) struct Connection {
    peer: PeerId,
    eps: Vec<*mut ffi::fid_ep>,
    cqs: Vec<*mut ffi::fid_cq>,
    eq: *mut ffi::fid_eq,
    owns_eq: bool,
    numa: Option<u16>,
    /// Round-robin cursor over `eps`, advanced by [`Self::next_ep`].
    next: AtomicUsize,
}

impl Connection {
    /// The remote peer's identity. For an accepted connection this is
    /// the dialer's id read from the handshake private data; for a
    /// dialed connection it is the caller-supplied `remote` id.
    pub(crate) fn peer(&self) -> PeerId {
        self.peer
    }

    /// The endpoints (QPs) bundled in this connection.
    pub(crate) fn eps(&self) -> &[*mut ffi::fid_ep] {
        &self.eps
    }

    /// The completion queues, one per endpoint, in `eps` order.
    pub(crate) fn cqs(&self) -> &[*mut ffi::fid_cq] {
        &self.cqs
    }

    /// Number of endpoints (QPs) in this connection.
    pub(crate) fn ep_count(&self) -> usize {
        self.eps.len()
    }

    /// Pick the next endpoint for an outbound request, round-robin
    /// across the bundled QPs. With a single QP this always returns the
    /// only endpoint.
    pub(crate) fn next_ep(&self) -> *mut ffi::fid_ep {
        let n = self.eps.len();
        debug_assert!(n >= 1);
        let i = self.next.fetch_add(1, Ordering::Relaxed) % n;
        self.eps[i]
    }

    /// NUMA locality attached at construction, if any.
    pub(crate) fn numa(&self) -> Option<u16> {
        self.numa
    }
}

impl Drop for Connection {
    fn drop(&mut self) {
        // Close every endpoint first, then every CQ, then the EQ but
        // only if this connection owns it (a dialed connection). An
        // accepted connection borrows the listener's EQ and must leave it
        // for the listener to close. Endpoints-before-CQs preserves the
        // invariant that a CQ is never freed while an endpoint still
        // bound to it can be polled.
        unsafe {
            for &ep in &self.eps {
                if !ep.is_null() {
                    let _ = ffi::ub_fi_close(ffi::as_fid_ep(ep));
                }
            }
            for &cq in &self.cqs {
                if !cq.is_null() {
                    let _ = ffi::ub_fi_close(ffi::as_fid_cq(cq));
                }
            }
            if self.owns_eq && !self.eq.is_null() {
                let _ = ffi::ub_fi_close(ffi::as_fid_eq(self.eq));
            }
        }
    }
}

// SAFETY: the contained pointers are libfabric resources with
// internally-synchronized operations; the connection is moved between
// the accepting thread and its owner but never mutated concurrently.
unsafe impl Send for Connection {}

// SAFETY: a connection is shared by `&` across threads (the progress
// thread polls its `cq` while client/server threads post sends and RMA
// on its `ep`). The underlying libfabric endpoint and completion queue
// are opened on a domain configured for thread-safe access, so these
// concurrent `&self` operations are sound.
unsafe impl Sync for Connection {}

/// Dial `dest` ("ip:port") and return an established [`Connection`]
/// bundling `qps` endpoints (QPs).
///
/// Opens a private EQ on `fabric`, builds `qps` active endpoints from
/// `info` on `domain`, and issues one `fi_connect` per endpoint, each
/// carrying `local` plus its QP index/total as private data so the
/// remote side learns who dialed and can group the requests. After all
/// dials are issued it drains `qps` `FI_CONNECTED` events. The returned
/// connection's [`Connection::peer`] is set to `remote`, the identity
/// the caller expects to reach. `info` is borrowed (not freed). `numa`
/// is attached as locality metadata.
pub(crate) fn connect(
    fabric: *mut ffi::fid_fabric,
    domain: *mut ffi::fid_domain,
    info: *mut ffi::fi_info,
    dest: &str,
    local: PeerId,
    remote: PeerId,
    numa: Option<u16>,
    qps: usize,
) -> Result<Connection> {
    let qps = qps.max(1);

    let mut sockaddr = [0u8; 128];
    let dest_c =
        std::ffi::CString::new(dest).map_err(|_| FabricError::BadConfig("dest has NUL"))?;
    let len = unsafe {
        ffi::ub_fi_parse_sockaddr(dest_c.as_ptr(), sockaddr.as_mut_ptr(), sockaddr.len())
    };
    if len < 0 {
        return Err(FabricError::Pkg("ub_fi_parse_sockaddr", len as i32));
    }

    let eq = open_eq(fabric)?;
    let eq_guard = FidGuard(ffi::as_fid_eq(eq));

    // All endpoints' guards live here until the whole bundle is built;
    // an early return drops them and closes every endpoint/CQ created so
    // far. The `eps`/`cqs` vecs hold the same raw pointers but do not own
    // them (no Drop), so there is no double close.
    let mut guards: Vec<FidGuard> = Vec::with_capacity(qps * 2);
    let mut eps: Vec<*mut ffi::fid_ep> = Vec::with_capacity(qps);
    let mut cqs: Vec<*mut ffi::fid_cq> = Vec::with_capacity(qps);

    for i in 0..qps {
        let (ep, cq) = bring_up_active(domain, info, eq)?;
        guards.push(FidGuard(ffi::as_fid_ep(ep)));
        guards.push(FidGuard(ffi::as_fid_cq(cq)));

        let private = connect_private(local, i as u16, qps as u16);
        let rc = unsafe {
            ffi::ub_fi_connect(
                ep,
                sockaddr.as_ptr() as *const std::ffi::c_void,
                private.as_ptr() as *const std::ffi::c_void,
                CONNECT_PRIVATE_LEN,
            )
        };
        check("fi_connect", rc)?;
        eps.push(ep);
        cqs.push(cq);
    }

    // Drain one FI_CONNECTED per dialed endpoint. They arrive on this
    // private EQ (only this dialer's events land here), order-independent.
    for _ in 0..qps {
        let mut buf = [0u8; 256];
        let (event, _) = eq_read_blocking(eq, &mut buf)?;
        if event != unsafe { ffi::ub_fi_connected() } {
            return Err(FabricError::Pkg(
                "fi_eq_sread(expected CONNECTED)",
                event as i32,
            ));
        }
    }

    std::mem::forget(eq_guard);
    for guard in guards {
        std::mem::forget(guard);
    }
    Ok(Connection {
        peer: remote,
        eps,
        cqs,
        eq,
        owns_eq: true,
        numa,
        next: AtomicUsize::new(0),
    })
}

/// Open a connection-event queue on `fabric` with a blocking wait
/// object so `fi_eq_sread` can sleep until an event arrives.
fn open_eq(fabric: *mut ffi::fid_fabric) -> Result<*mut ffi::fid_eq> {
    let mut attr: ffi::fi_eq_attr = unsafe { std::mem::zeroed() };
    attr.wait_obj = ffi::FI_WAIT_UNSPEC;
    let mut eq: *mut ffi::fid_eq = ptr::null_mut();
    let rc = unsafe { ffi::ub_fi_eq_open(fabric, &mut attr, &mut eq, ptr::null_mut()) };
    check("fi_eq_open", rc)?;
    Ok(eq)
}

/// Create an active endpoint from `info` on `domain`, bind it to the
/// connection event queue `eq` and a fresh data-format CQ, and enable
/// it. Returns the endpoint and its CQ. On any failure both are closed
/// before returning.
fn bring_up_active(
    domain: *mut ffi::fid_domain,
    info: *mut ffi::fi_info,
    eq: *mut ffi::fid_eq,
) -> Result<(*mut ffi::fid_ep, *mut ffi::fid_cq)> {
    let mut ep: *mut ffi::fid_ep = ptr::null_mut();
    let rc = unsafe { ffi::ub_fi_endpoint(domain, info, &mut ep, ptr::null_mut()) };
    check("fi_endpoint", rc)?;
    let ep_guard = FidGuard(ffi::as_fid_ep(ep));

    let rc = unsafe { ffi::ub_fi_ep_bind_eq(ep, eq, 0) };
    check("fi_ep_bind(eq)", rc)?;

    let mut cq_attr: ffi::fi_cq_attr = unsafe { std::mem::zeroed() };
    cq_attr.format = ffi::FI_CQ_FORMAT_DATA;
    cq_attr.wait_obj = ffi::FI_WAIT_NONE;
    let mut cq: *mut ffi::fid_cq = ptr::null_mut();
    let rc = unsafe { ffi::ub_fi_cq_open(domain, &mut cq_attr, &mut cq, ptr::null_mut()) };
    check("fi_cq_open", rc)?;
    let cq_guard = FidGuard(ffi::as_fid_cq(cq));

    let rc = unsafe { ffi::ub_fi_ep_bind(ep, ffi::as_fid_cq(cq), ffi::FI_TRANSMIT | ffi::FI_RECV) };
    check("fi_ep_bind(cq)", rc)?;

    let rc = unsafe { ffi::ub_fi_enable(ep) };
    check("fi_enable", rc)?;

    std::mem::forget(ep_guard);
    std::mem::forget(cq_guard);
    Ok((ep, cq))
}

/// Block until one event is readable on `eq`, returning its event
/// discriminant and the number of bytes written into `buf`. Translates
/// the `-FI_EAVAIL` "error available" path into a populated
/// [`FabricError`].
fn eq_read_blocking(eq: *mut ffi::fid_eq, buf: &mut [u8]) -> Result<(u32, usize)> {
    match eq_read_timeout(eq, buf, CM_EVENT_TIMEOUT_MS)? {
        Some(v) => Ok(v),
        None => Err(FabricError::Timeout),
    }
}

/// Wait up to `timeout_ms` for one event on `eq`. Returns `Ok(None)` on
/// a clean timeout (`-FI_EAGAIN`) so a caller can poll between waits;
/// returns `Ok(Some((event, n)))` on success, and surfaces the
/// `-FI_EAVAIL` error-entry path as a populated [`FabricError`].
fn eq_read_timeout(
    eq: *mut ffi::fid_eq,
    buf: &mut [u8],
    timeout_ms: i32,
) -> Result<Option<(u32, usize)>> {
    let mut event: u32 = 0;
    let n = unsafe {
        ffi::ub_fi_eq_sread(
            eq,
            &mut event,
            buf.as_mut_ptr() as *mut std::ffi::c_void,
            buf.len(),
            timeout_ms,
            0,
        )
    };
    if n < 0 {
        let rc = n as i32;
        if rc == -unsafe { ffi::ub_fi_eagain() } {
            return Ok(None);
        }
        if rc == -unsafe { ffi::ub_fi_eavail() } {
            return Err(eq_read_error(eq));
        }
        return Err(FabricError::Pkg("fi_eq_sread", rc));
    }
    Ok(Some((event, n as usize)))
}

/// Drain the error entry behind a `-FI_EAVAIL` from the EQ.
fn eq_read_error(eq: *mut ffi::fid_eq) -> FabricError {
    let mut err: ffi::fi_eq_err_entry = unsafe { std::mem::zeroed() };
    let rc = unsafe { ffi::ub_fi_eq_readerr(eq, &mut err, 0) };
    if rc < 0 {
        return FabricError::Pkg("fi_eq_readerr", rc as i32);
    }
    FabricError::Cq {
        prov_errno: err.prov_errno,
        err: err.err,
    }
}

/// Read the dialer's [`PeerId`] and QP index/total from the private
/// data trailing a `FI_CONNREQ` event. The private data begins
/// immediately after the fixed `fi_eq_cm_entry` header and is laid out
/// by [`connect_private`].
fn read_peer_private(buf: &[u8], n: usize) -> Result<(PeerId, u16, u16)> {
    let off = std::mem::size_of::<ffi::fi_eq_cm_entry>();
    if n < off + CONNECT_PRIVATE_LEN {
        return Err(FabricError::NotFound("CONNREQ private data too short"));
    }
    let mut id = [0u8; 8];
    id.copy_from_slice(&buf[off..off + 8]);
    let mut qi = [0u8; 2];
    qi.copy_from_slice(&buf[off + 8..off + 10]);
    let mut qt = [0u8; 2];
    qt.copy_from_slice(&buf[off + 10..off + 12]);
    Ok((
        PeerId(u64::from_ne_bytes(id)),
        u16::from_le_bytes(qi),
        u16::from_le_bytes(qt),
    ))
}

/// Encode the connect private data: 8-byte [`PeerId`] (`to_ne_bytes`)
/// followed by the QP index and QP total as little-endian `u16`s.
fn connect_private(local: PeerId, qp_index: u16, qp_total: u16) -> [u8; CONNECT_PRIVATE_LEN] {
    let mut p = [0u8; CONNECT_PRIVATE_LEN];
    p[0..8].copy_from_slice(&local.0.to_ne_bytes());
    p[8..10].copy_from_slice(&qp_index.to_le_bytes());
    p[10..12].copy_from_slice(&qp_total.to_le_bytes());
    p
}

/// `fi_getname` on `fid`, formatted as numeric "ip:port". Probes the
/// length first (libfabric returns `-FI_ETOOSMALL` with `len` set), then
/// renders the sockaddr via the shim.
fn getname_string(fid: *mut ffi::fid) -> Result<String> {
    let mut len: usize = 0;
    let rc = unsafe { ffi::ub_fi_getname(fid, ptr::null_mut(), &mut len) };
    if rc != 0 && len == 0 {
        return Err(FabricError::Pkg("fi_getname (size probe)", rc));
    }
    let mut addr = vec![0u8; len];
    let rc =
        unsafe { ffi::ub_fi_getname(fid, addr.as_mut_ptr() as *mut std::ffi::c_void, &mut len) };
    check("fi_getname", rc)?;

    let mut out = [0i8; 128];
    let written = unsafe {
        ffi::ub_fi_format_sockaddr(
            addr.as_ptr() as *const std::ffi::c_void,
            len,
            out.as_mut_ptr(),
            out.len(),
        )
    };
    if written < 0 {
        return Err(FabricError::Pkg("ub_fi_format_sockaddr", written as i32));
    }
    let bytes: Vec<u8> = out[..written as usize].iter().map(|&b| b as u8).collect();
    String::from_utf8(bytes).map_err(|_| FabricError::NotFound("sockaddr not utf8"))
}

/// Close-on-drop guard for a libfabric resource, used to keep the
/// construction paths above linear. Dismissed with `std::mem::forget`
/// once ownership transfers to a [`Listener`] / [`Connection`].
struct FidGuard(*mut ffi::fid);
impl Drop for FidGuard {
    fn drop(&mut self) {
        if !self.0.is_null() {
            unsafe {
                let _ = ffi::ub_fi_close(self.0);
            }
        }
    }
}

/// Free-on-drop guard for an `fi_info` the acceptor must consume.
struct FreeInfoGuard(*mut ffi::fi_info);
impl Drop for FreeInfoGuard {
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
    use std::ffi::CString;
    use std::sync::Arc;

    /// Raw pointer wrapper so a `fid_domain` can be moved into the
    /// accept thread. The domain outlives the thread (the test holds it)
    /// and is only used to create the accepted endpoint.
    struct SendPtr<T>(*mut T);
    unsafe impl<T> Send for SendPtr<T> {}

    fn tcp_msg_available() -> bool {
        let prov = CString::new("tcp").unwrap();
        let hints = unsafe { ffi::ub_fi_build_msg_hints(prov.as_ptr()) };
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

    /// `fi_getinfo` for the tcp MSG transport. `node`/`service` are the
    /// optional source (server, with `FI_SOURCE`) or destination
    /// (client) address; pass `None` for an unbound generic info.
    fn getinfo_tcp_msg(
        node: Option<&str>,
        service: Option<&str>,
        source: bool,
    ) -> (
        *mut ffi::fi_info,
        *mut ffi::fid_fabric,
        *mut ffi::fid_domain,
    ) {
        let prov = CString::new("tcp").unwrap();
        let hints = unsafe { ffi::ub_fi_build_msg_hints(prov.as_ptr()) };
        assert!(!hints.is_null());

        let node_c = node.map(|s| CString::new(s).unwrap());
        let service_c = service.map(|s| CString::new(s).unwrap());
        let node_ptr = node_c.as_ref().map(|s| s.as_ptr()).unwrap_or(ptr::null());
        let service_ptr = service_c
            .as_ref()
            .map(|s| s.as_ptr())
            .unwrap_or(ptr::null());
        let flags = if source { ffi::FI_SOURCE } else { 0 };

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
        unsafe { ffi::fi_freeinfo(hints) };
        assert_eq!(rc, 0, "fi_getinfo for tcp msg failed");
        assert!(!info.is_null());

        let attr = unsafe { ffi::ub_fi_info_fabric_attr(info) };
        let mut fabric: *mut ffi::fid_fabric = ptr::null_mut();
        let rc = unsafe { ffi::fi_fabric(attr, &mut fabric, ptr::null_mut()) };
        assert_eq!(rc, 0, "fi_fabric failed");

        let mut domain: *mut ffi::fid_domain = ptr::null_mut();
        let rc = unsafe { ffi::ub_fi_domain(fabric, info, &mut domain, ptr::null_mut()) };
        assert_eq!(rc, 0, "fi_domain failed");

        (info, fabric, domain)
    }

    #[test]
    fn cm_loopback_connect_accept_round_trips_peer() {
        if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
            eprintln!("FABRIC_SKIP_FFI set; skipping cm loopback test");
            return;
        }
        if !tcp_msg_available() {
            eprintln!("libfabric tcp MSG provider unavailable; skipping cm loopback test");
            return;
        }

        let client_peer = PeerId(0xC11E_0000_0000_0001);
        let server_peer = PeerId(0x5E11_0000_0000_0002);

        // Server: listen on an ephemeral loopback port.
        let (srv_info, srv_fabric, srv_domain) =
            getinfo_tcp_msg(Some("127.0.0.1"), Some("0"), true);
        let listener = Arc::new(Listener::new(srv_fabric, srv_info).expect("listener"));
        let addr = listener.local_addr().expect("local_addr");
        assert!(addr.starts_with("127.0.0.1:"), "unexpected addr {addr}");

        // Accept on a worker thread so the client can dial concurrently.
        let accept_listener = Arc::clone(&listener);
        let domain_ptr = SendPtr(srv_domain);
        let accept = std::thread::spawn(move || {
            let domain_ptr = domain_ptr;
            accept_listener.accept_one(domain_ptr.0, Some(3))
        });

        // Client: generic info, dial the server's bound address.
        let (cli_info, cli_fabric, cli_domain) = getinfo_tcp_msg(None, None, false);
        let client_conn = connect(
            cli_fabric,
            cli_domain,
            cli_info,
            &addr,
            client_peer,
            server_peer,
            Some(7),
            1,
        )
        .expect("client connect");

        let server_conn = accept.join().expect("accept thread").expect("accept");

        // The accepting side must have learned the dialer's identity from
        // the handshake private data; the dialing side keeps the remote
        // identity it was asked to reach.
        assert_eq!(server_conn.peer(), client_peer);
        assert_eq!(client_conn.peer(), server_peer);
        assert_eq!(server_conn.numa(), Some(3));
        assert_eq!(client_conn.numa(), Some(7));
        assert_eq!(server_conn.ep_count(), 1);
        assert_eq!(client_conn.ep_count(), 1);
        assert!(!server_conn.eps()[0].is_null());
        assert!(!client_conn.cqs()[0].is_null());

        // Drop connections before the listener (which owns the shared
        // server EQ) and before tearing down domains/fabrics/info.
        drop(client_conn);
        drop(server_conn);
        drop(listener);

        unsafe {
            let _ = ffi::ub_fi_close(ffi::as_fid_domain(cli_domain));
            let _ = ffi::ub_fi_close(ffi::as_fid_fabric(cli_fabric));
            ffi::fi_freeinfo(cli_info);
            let _ = ffi::ub_fi_close(ffi::as_fid_domain(srv_domain));
            let _ = ffi::ub_fi_close(ffi::as_fid_fabric(srv_fabric));
            ffi::fi_freeinfo(srv_info);
        }
    }

    #[test]
    fn cm_loopback_groups_multiple_qps_into_one_connection() {
        if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
            eprintln!("FABRIC_SKIP_FFI set; skipping cm multi-qp test");
            return;
        }
        if !tcp_msg_available() {
            eprintln!("libfabric tcp MSG provider unavailable; skipping cm multi-qp test");
            return;
        }

        let client_peer = PeerId(0xC11E_0000_0000_0004);
        let server_peer = PeerId(0x5E11_0000_0000_0008);
        let qps = 4usize;

        // Server: listen on an ephemeral loopback port.
        let (srv_info, srv_fabric, srv_domain) =
            getinfo_tcp_msg(Some("127.0.0.1"), Some("0"), true);
        let listener = Arc::new(Listener::new(srv_fabric, srv_info).expect("listener"));
        let addr = listener.local_addr().expect("local_addr");

        // Accept on a worker thread; the acceptor must group all `qps`
        // inbound CONNREQ/CONNECTED events from the same dialer into one
        // logical connection before returning.
        let accept_listener = Arc::clone(&listener);
        let domain_ptr = SendPtr(srv_domain);
        let accept = std::thread::spawn(move || {
            let domain_ptr = domain_ptr;
            accept_listener.accept_one(domain_ptr.0, Some(5))
        });

        // Client: dial the server's bound address with `qps` endpoints.
        let (cli_info, cli_fabric, cli_domain) = getinfo_tcp_msg(None, None, false);
        let client_conn = connect(
            cli_fabric,
            cli_domain,
            cli_info,
            &addr,
            client_peer,
            server_peer,
            Some(2),
            qps,
        )
        .expect("client connect");

        let server_conn = accept.join().expect("accept thread").expect("accept");

        // Identities survive the multi-endpoint handshake, and both sides
        // end up with exactly `qps` distinct, non-null endpoints/CQs
        // bundled into the single returned connection.
        assert_eq!(server_conn.peer(), client_peer);
        assert_eq!(client_conn.peer(), server_peer);
        assert_eq!(server_conn.ep_count(), qps);
        assert_eq!(client_conn.ep_count(), qps);
        for i in 0..qps {
            assert!(!server_conn.eps()[i].is_null());
            assert!(!server_conn.cqs()[i].is_null());
            assert!(!client_conn.eps()[i].is_null());
            assert!(!client_conn.cqs()[i].is_null());
        }

        // next_ep round-robins across all endpoints before repeating.
        let mut seen = std::collections::HashSet::new();
        for _ in 0..qps {
            seen.insert(client_conn.next_ep() as usize);
        }
        assert_eq!(seen.len(), qps, "next_ep must cover every endpoint");

        drop(client_conn);
        drop(server_conn);
        drop(listener);

        unsafe {
            let _ = ffi::ub_fi_close(ffi::as_fid_domain(cli_domain));
            let _ = ffi::ub_fi_close(ffi::as_fid_fabric(cli_fabric));
            ffi::fi_freeinfo(cli_info);
            let _ = ffi::ub_fi_close(ffi::as_fid_domain(srv_domain));
            let _ = ffi::ub_fi_close(ffi::as_fid_fabric(srv_fabric));
            ffi::fi_freeinfo(srv_info);
        }
    }
}
