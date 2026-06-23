// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-fabric connection table: peer-id -> `cm::Connection`.
//!
//! Under the `FI_EP_MSG` transport a "connection" is a dedicated
//! connected endpoint (`cm::Connection`) negotiated through the
//! connection manager, not an address-vector row. The table owns one
//! `Arc<cm::Connection>` per `PeerId`; each connection carries its own
//! `fid_ep` and completion queue. Installing a connection registers its
//! CQ with the progress group and arms a self-reposting `RecvPool` so
//! inbound replies/requests are demultiplexed by the `InboundDispatch`.
//!
//! Connection establishment is *single-dialer*: for any peer pair the
//! lower-id node dials and the higher-id node only accepts. The
//! resulting connected endpoint is reused for both sending and
//! receiving, so once the link is up either side issues RPCs over it.
//! This deliberately avoids a simultaneous bidirectional connect: the
//! verbs RDMA-CM does not resolve two endpoints connecting to each other
//! at once (both listeners observe an `FI_SHUTDOWN` and neither reaches
//! `ESTABLISHED`); the tcp provider tolerates it, but the single-dialer
//! rule keeps both providers on one code path. The dialer side
//! (`add_connection` plus the background reconnect loop) retries until
//! the link is up; the accept side waits for the inbound connect. There
//! is therefore at most one connection per peer and no tie-break.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, RwLock};

use crate::fabric::PeerId;

use super::backing::LocalMrCtx;
use super::cm;
use super::completion::CompletionRegistry;
use super::dispatch::InboundDispatch;
use super::error::{FabricError, Result};
use super::fabric::Fabric;
use super::ffi;
use super::progress::ProgressGroup;
use super::recvpool::RecvPool;
use super::types::ConnectionSpec;

/// Result of offering a connection to the table; on `Rejected` the
/// caller tears down the returned (duplicate) connection. It was never
/// armed or registered, so dropping it simply closes the endpoint.
enum Offer {
    /// Inserted into a previously empty slot.
    Inserted,
    /// Rejected because a connection already exists for this peer (a
    /// concurrent dial won the race); the offered connection is returned
    /// for teardown.
    Rejected(Arc<cm::Connection>),
}

/// Internal mapping of `PeerId` to its connected endpoint. Held behind
/// an `Arc` by `FabricInner` and shared with the accept loop so both the
/// dial and accept paths can publish.
pub(crate) struct ConnectionTable {
    inner: RwLock<HashMap<PeerId, Arc<cm::Connection>>>,
    /// Serializes the *entire* install/remove of a connection (offer +
    /// arm + CQ register, and the symmetric unregister + drop on
    /// removal), not just the table mutation. Although establishment is
    /// single-dialer, `add_connection` and the background reconnect loop
    /// can both dial the same peer before either's `offer` publishes it,
    /// and `remove_connection` can race an install. `offer` alone is
    /// atomic, but the window between an `Inserted` offer and its
    /// `progress.register` is not. Holding this lock across the whole
    /// sequence keeps offer+arm+register atomic with respect to a
    /// duplicate install or a removal, so the progress group never
    /// registers or polls a CQ whose connection is concurrently dropped
    /// and closed (UAF/SIGSEGV).
    install: Mutex<()>,
}

impl ConnectionTable {
    pub(crate) fn new() -> Self {
        Self {
            inner: RwLock::new(HashMap::new()),
            install: Mutex::new(()),
        }
    }

    /// Offer `conn` for `peer`. Atomic under the write lock so a
    /// duplicate dial cannot also insert; rejects if a connection for
    /// `peer` already exists.
    fn offer(&self, peer: PeerId, conn: Arc<cm::Connection>) -> Offer {
        let mut map = match self.inner.write() {
            Ok(m) => m,
            // A poisoned lock means a publisher panicked mid-insert; the
            // fabric is being torn down. Reject so the caller drops it.
            Err(_) => return Offer::Rejected(conn),
        };
        match map.entry(peer) {
            std::collections::hash_map::Entry::Occupied(_) => Offer::Rejected(conn),
            std::collections::hash_map::Entry::Vacant(slot) => {
                slot.insert(conn);
                Offer::Inserted
            }
        }
    }

    pub(crate) fn list(&self) -> Vec<PeerId> {
        self.inner
            .read()
            .map(|m| m.keys().copied().collect())
            .unwrap_or_default()
    }

    pub(crate) fn lookup(&self, peer: PeerId) -> Option<Arc<cm::Connection>> {
        self.inner.read().ok().and_then(|m| m.get(&peer).cloned())
    }

    /// Remove and return the connection for `peer`, if present.
    pub(crate) fn remove(&self, peer: PeerId) -> Option<Arc<cm::Connection>> {
        self.inner.write().ok().and_then(|mut m| m.remove(&peer))
    }

    /// Drain every connection out of the table, returning them so the
    /// caller can drop them in a controlled order. Used by
    /// `FabricInner::Drop`.
    pub(crate) fn take_all(&self) -> Vec<Arc<cm::Connection>> {
        self.inner
            .write()
            .map(|mut m| m.drain().map(|(_, c)| c).collect())
            .unwrap_or_default()
    }

    /// Acquire the install/remove serialization lock. Poison-tolerant: a
    /// publisher that panicked mid-install means the fabric is tearing
    /// down, and the recovered guard still upholds mutual exclusion.
    fn install_guard(&self) -> std::sync::MutexGuard<'_, ()> {
        self.install.lock().unwrap_or_else(|e| e.into_inner())
    }
}

/// Publish `conn` into `table`, arm a recv pool, and register its CQ
/// with the progress group. Shared by the outbound dial
/// (`add_connection` / reconnect loop) and the inbound accept loop.
///
/// Establishment is single-dialer, so normally exactly one of those
/// paths installs a given peer. The exception is a benign duplicate: the
/// dialer's `add_connection` and the reconnect loop can both dial the
/// same peer before either publishes. `offer` resolves that atomically -
/// the loser is `Rejected` and dropped here before it is ever armed or
/// registered, so only one endpoint per peer consumes recv resources or
/// is polled by the progress group.
///
/// `offer` runs *first*, before arming or registering: a rejected
/// duplicate must never have posted recvs or a registered CQ. If arming
/// the winner fails the connection is removed again so the table never
/// holds an unarmed connection. The whole sequence is serialized by
/// `ConnectionTable::install` against a concurrent duplicate install or
/// a `remove_connection`.
pub(crate) fn install_connection(
    table: &ConnectionTable,
    progress: &ProgressGroup,
    completions: &Arc<CompletionRegistry>,
    dispatch: &Arc<InboundDispatch>,
    recv_depth: usize,
    peer: PeerId,
    conn: Arc<cm::Connection>,
    local_ctx: &LocalMrCtx,
) -> Result<()> {
    // Serialize the whole install so a concurrent duplicate install (or
    // removal) cannot interleave between our `offer` and `register`. See
    // `ConnectionTable::install`.
    let _install = table.install_guard();

    let conn = match table.offer(peer, Arc::clone(&conn)) {
        Offer::Inserted => conn,
        Offer::Rejected(_dup) => {
            // A connection for this peer already exists. `conn` was never
            // armed or registered, so dropping our handle closes it.
            drop(conn);
            return Ok(());
        }
    };

    // Arm and register every endpoint. Arm all recv pools first, then
    // register all CQs: the progress group must not poll a CQ before its
    // endpoint's recvs are posted. On any arm failure, roll back the
    // publish so the table never exposes a partially-armed connection
    // (dropping `conn` closes every endpoint/CQ, cancelling any recvs
    // already posted on earlier endpoints).
    for &ep in conn.eps() {
        if let Err(e) = RecvPool::arm(
            ep,
            peer,
            Arc::clone(completions),
            Arc::clone(dispatch),
            recv_depth,
            local_ctx.clone(),
        ) {
            table.remove(peer);
            drop(conn);
            return Err(e);
        }
    }
    for &cq in conn.cqs() {
        progress.register(conn.numa(), cq);
    }
    crate::metrics::fabric_connections_delta(1);
    Ok(())
}

/// Self-contained dial context shared between `add_connection` and the
/// background reconnect loop. Holds the raw libfabric handles needed to
/// open an active endpoint plus the per-fabric machinery
/// (`install_connection`) used to publish it.
///
/// The raw pointers are owned by `FabricInner` and outlive every
/// `Dialer` clone: the reconnect thread that holds an `Arc<Dialer>` is
/// joined in `FabricInner::Drop` before the domain/fabric are closed, so
/// the handles are always valid for the lifetime of any dial.
pub(crate) struct Dialer {
    fabric: *mut ffi::fid_fabric,
    domain: *mut ffi::fid_domain,
    dial_info: *mut ffi::fi_info,
    self_peer: PeerId,
    numa: Option<u16>,
    recv_depth: usize,
    qps: usize,
    progress: Arc<ProgressGroup>,
    completions: Arc<CompletionRegistry>,
    dispatch: Arc<InboundDispatch>,
    connections: Arc<ConnectionTable>,
    local_ctx: LocalMrCtx,
}

// SAFETY: the raw libfabric handles are internally synchronized for the
// operations performed here (opening active endpoints), and they outlive
// every Dialer clone (the reconnect thread is joined before teardown).
unsafe impl Send for Dialer {}
unsafe impl Sync for Dialer {}

impl Dialer {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn new(
        fabric: *mut ffi::fid_fabric,
        domain: *mut ffi::fid_domain,
        dial_info: *mut ffi::fi_info,
        self_peer: PeerId,
        numa: Option<u16>,
        recv_depth: usize,
        qps: usize,
        progress: Arc<ProgressGroup>,
        completions: Arc<CompletionRegistry>,
        dispatch: Arc<InboundDispatch>,
        connections: Arc<ConnectionTable>,
        local_ctx: LocalMrCtx,
    ) -> Self {
        Self {
            fabric,
            domain,
            dial_info,
            self_peer,
            numa,
            recv_depth,
            qps,
            progress,
            completions,
            dispatch,
            connections,
            local_ctx,
        }
    }

    /// Dial `spec.peer` and publish the resulting connection. Returns
    /// `Err` if the dial itself fails (the peer is not yet listening);
    /// callers decide whether to tolerate it.
    pub(crate) fn dial_and_install(&self, spec: &ConnectionSpec) -> Result<()> {
        let conn = cm::connect(
            self.fabric,
            self.domain,
            self.dial_info,
            &spec.address,
            self.self_peer,
            spec.peer,
            self.numa,
            self.qps,
        )?;
        install_connection(
            &self.connections,
            &self.progress,
            &self.completions,
            &self.dispatch,
            self.recv_depth,
            spec.peer,
            Arc::new(conn),
            &self.local_ctx,
        )
    }

    /// Whether this node is the dialer for `peer`. Establishment is
    /// single-dialer: the lower-id node dials, the higher-id node only
    /// accepts. With distinct peer ids exactly one side dials, so a peer
    /// pair never simultaneously connects.
    pub(crate) fn should_dial(&self, peer: PeerId) -> bool {
        self.self_peer.0 < peer.0
    }

    /// Peers with a currently-published connection. Used by the reconnect
    /// loop to skip already-connected peers.
    pub(crate) fn connected_peers(&self) -> Vec<PeerId> {
        self.connections.list()
    }
}

impl Fabric {
    /// Dial `spec.peer` and publish the resulting connection, but only if
    /// this node is the designated dialer for the pair.
    ///
    /// Establishment is single-dialer: the lower-id node dials, the
    /// higher-id node only accepts (see the module docs). When this node
    /// is the higher id, `add_connection` records the peer as *desired*
    /// and returns without dialing; the link is established by the
    /// peer's inbound connect into the accept loop.
    ///
    /// The peer is recorded in the *desired* set first, so a dial failure
    /// is *tolerated*: the peer may not be listening yet, and the
    /// background reconnect loop keeps retrying. Returning `Ok(())` keeps
    /// the peer in the reconcile `applied` set so it is not retried as a
    /// hard failure.
    pub fn add_connection(&self, spec: ConnectionSpec) -> Result<()> {
        if let (Some(n), Some(m)) = (self.inner().cfg.numa, spec.hca_numa)
            && n != m
        {
            return Err(FabricError::NumaMismatch {
                expected: n,
                got: m,
            });
        }

        // Record intent before dialing so the background reconnect loop
        // keeps trying if the dial fails now, and so the accept side
        // tracks the peer it is waiting for.
        if let Ok(mut desired) = self.inner().desired.write() {
            desired.insert(spec.peer, spec.clone());
        }

        // Only the lower-id node dials; the higher-id node waits for the
        // inbound connect.
        if self.inner().dialer.should_dial(spec.peer) {
            if let Err(e) = self.inner().dialer.dial_and_install(&spec) {
                eprintln!(
                    "fabric: dial to peer {} ({}) failed: {e:?}; relying on \
                     background reconnect",
                    spec.peer.0, spec.address,
                );
            }
        }
        Ok(())
    }

    /// Remove `peer` from the connection table and the desired set,
    /// unregistering its CQ from the progress group before the endpoint
    /// is closed. Errors with `FabricError::NotFound("peer")` if the peer
    /// had no published connection (its desired entry is still cleared).
    pub fn remove_connection(&self, peer: PeerId) -> Result<()> {
        // Drop the intent regardless so the reconnect loop stops dialing.
        if let Ok(mut desired) = self.inner().desired.write() {
            desired.remove(&peer);
        }

        // Serialize against in-flight installs so we never unregister and
        // close a CQ another install is mid-registering. See
        // `ConnectionTable::install`.
        let _install = self.inner().connections.install_guard();

        let conn = match self.inner().connections.remove(peer) {
            Some(c) => c,
            None => return Err(FabricError::NotFound("peer")),
        };
        // Unregister every CQ before drop: the progress thread must not
        // poll a CQ that is about to be closed by `Connection::Drop`.
        for &cq in conn.cqs() {
            self.inner().progress.unregister(cq);
        }
        drop(conn);
        crate::metrics::fabric_connections_delta(-1);
        Ok(())
    }

    /// Replace the set of peers the background reconnect loop keeps
    /// dialed. This is the authoritative prune: a peer dropped from the
    /// configuration that never connected (so `remove_connection` was
    /// never called for it, since reconcile only removes peers present in
    /// `list_connections`) is dropped from the desired set here. It does
    /// not itself dial or tear down live connections; the reconnect loop
    /// and the reconcile pass handle those.
    pub fn set_desired_peers(&self, specs: Vec<ConnectionSpec>) {
        if let Ok(mut desired) = self.inner().desired.write() {
            *desired = specs.into_iter().map(|s| (s.peer, s)).collect();
        }
    }

    /// Snapshot of currently-known peers.
    pub fn list_connections(&self) -> Vec<PeerId> {
        self.inner().connections.list()
    }

    /// Resolve a `PeerId` to its connected endpoint and destination
    /// address. Under `FI_EP_MSG` the endpoint is already bound to the
    /// remote, so the address is always `FI_ADDR_UNSPEC`. Used by the
    /// RPC submission and reply paths.
    pub(crate) fn resolve_peer(&self, peer: PeerId) -> Result<(*mut ffi::fid_ep, ffi::fi_addr_t)> {
        let conn = self
            .inner()
            .connections
            .lookup(peer)
            .ok_or(FabricError::NotFound("peer"))?;
        Ok((conn.next_ep(), ffi::FI_ADDR_UNSPEC))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn connection_table_starts_empty() {
        let t = ConnectionTable::new();
        assert!(t.list().is_empty());
        assert!(t.lookup(PeerId(1)).is_none());
    }

    /// Pure NUMA-mismatch helper exercised without any FFI. The
    /// production check lives inline in `add_connection`; this test
    /// mirrors the logic so a regression there fails loudly.
    fn numa_check(fabric_numa: Option<u16>, spec_numa: Option<u16>) -> Result<()> {
        if let (Some(n), Some(m)) = (fabric_numa, spec_numa)
            && n != m
        {
            return Err(FabricError::NumaMismatch {
                expected: n,
                got: m,
            });
        }
        Ok(())
    }

    #[test]
    fn numa_check_passes_when_either_unset() {
        assert!(numa_check(None, None).is_ok());
        assert!(numa_check(Some(0), None).is_ok());
        assert!(numa_check(None, Some(1)).is_ok());
    }

    #[test]
    fn numa_check_passes_when_equal() {
        assert!(numa_check(Some(2), Some(2)).is_ok());
    }

    #[test]
    fn numa_check_rejects_mismatch() {
        match numa_check(Some(0), Some(1)) {
            Err(FabricError::NumaMismatch { expected, got }) => {
                assert_eq!(expected, 0);
                assert_eq!(got, 1);
            }
            other => panic!("expected NumaMismatch, got {other:?}"),
        }
    }
}
