// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pool client transport: routes each `bulk_get` to the first hop
//! only. The local node either owns the stripe (serve from the
//! topology-unaware origin `Backend`) or it does not (hand the request
//! to the fabric, which delivers it to the routed peer; that peer's
//! `RecursiveHandler` then recurses server-side toward the owner).
//!
//! This type replaces the old `backend::TieredTransport`. The crucial
//! difference is that the `Backend` here is purely an origin fetch:
//! it is consulted only when *this* node owns the stripe, and it has
//! no knowledge of topology. All routing lives in the p2p + fabric
//! layers.
//!
//! The first-hop decision is a single Chord `next_hop` lookup on the
//! stripe's ring position:
//!
//! * `next_hop(target) == None` - this node owns the stripe; serve
//!   from the local `Backend` (origin fetch into the pool's pages).
//! * `next_hop(target) == Some(peer)` - some other node is the next
//!   hop; hand the request to the wrapped `FabricTransport`, whose
//!   `FingerRouter` resolves the same peer. The request travels with
//!   the default `MAX_HOPS` TTL; recursion happens on the server.

use std::collections::HashMap;
use std::marker::PhantomData;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::backend::Backend;
use crate::bufferpool::{BulkRef, Error, PageRef, PageStream, Req, Transport};
use crate::fabric::Result as FabResult;
use crate::fabric::{Fabric, FabricTransport, MrHandle, PeerId};
use crate::p2p::{FingerRouter, FingerTable, NodeId, stripe_to_ring};

/// Transport that selects the first hop per request by finger-table
/// ownership: the local origin `Backend` when this node owns the
/// stripe, otherwise the fabric peer path.
pub struct RoutedTransport<R, B: Backend<Req = R>> {
    fabric_transport: FabricTransport<R, FingerRouter>,
    fingers: Arc<FingerTable>,
    /// Retained alongside `fingers` as the canonical record of the
    /// routing surface this transport was built from. The active
    /// `node -> peer` resolution happens inside the inner
    /// `FabricTransport`'s `FingerRouter` clone; this copy keeps the
    /// two views co-located for inspection and future re-routing.
    #[allow(dead_code)]
    node_to_peer: Arc<HashMap<NodeId, PeerId>>,
    backend: B,
    _marker: PhantomData<fn() -> R>,
}

impl<R, B: Backend<Req = R>> RoutedTransport<R, B> {
    /// Build a routed transport. Constructs the inner
    /// `FabricTransport` with a `FingerRouter` over the same
    /// `fingers` / `node_to_peer` used for the local-ownership
    /// pre-check.
    pub fn new(
        fabric: Arc<Fabric>,
        mr: MrHandle,
        page_size: usize,
        fingers: Arc<FingerTable>,
        node_to_peer: Arc<HashMap<NodeId, PeerId>>,
        backend: B,
    ) -> FabResult<Self> {
        let router = FingerRouter::new(fingers.clone(), node_to_peer.clone());
        let fabric_transport = FabricTransport::new(fabric, mr, router, page_size)?;
        Ok(Self {
            fabric_transport,
            fingers,
            node_to_peer,
            backend,
            _marker: PhantomData,
        })
    }

    /// Whether the local node owns `req`'s stripe (Chord `next_hop`
    /// returns `None`). When true, `bulk_get` routes to the backend;
    /// otherwise it routes to the fabric peer path. Factored out so
    /// the routing decision is unit-testable without a live fabric.
    pub fn owns_locally(&self, req: &R) -> bool
    where
        R: Req,
    {
        self.fingers.next_hop(stripe_to_ring(req.key())).is_none()
    }
}

impl<R, B> Transport<R> for RoutedTransport<R, B>
where
    R: Req + Serialize + DeserializeOwned + 'static,
    B: Backend<Req = R> + 'static,
{
    type Stream<'a>
        = RoutedStream<'a, R, B>
    where
        Self: 'a,
        R: 'a;

    fn bulk_get<'a>(&'a self, req: &'a R, src: BulkRef, dsts: &'a [PageRef]) -> Self::Stream<'a> {
        if self.owns_locally(req) {
            RoutedStream::Backend(self.backend.bulk_get(req, src, dsts))
        } else {
            RoutedStream::Fabric(self.fabric_transport.bulk_get(req, src, dsts))
        }
    }
}

/// Stream returned by [`RoutedTransport::bulk_get`], wrapping either
/// the fabric stream (first hop to a peer) or the local backend
/// stream (origin fetch). `poll_next` projects the pin into whichever
/// arm is active.
pub enum RoutedStream<'a, R, B>
where
    R: Req + Serialize + DeserializeOwned + 'static,
    B: Backend + 'a,
{
    Fabric(<FabricTransport<R, FingerRouter> as Transport<R>>::Stream<'a>),
    Backend(B::Stream<'a>),
}

impl<'a, R, B> PageStream for RoutedStream<'a, R, B>
where
    R: Req + Serialize + DeserializeOwned + 'static,
    B: Backend<Req = R> + 'a,
{
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        // SAFETY: standard enum pin projection. We never move the
        // wrapped stream out of the pinned `self`; each arm is
        // re-pinned in place via `Pin::new_unchecked`.
        unsafe {
            match self.get_unchecked_mut() {
                RoutedStream::Fabric(s) => Pin::new_unchecked(s).poll_next(cx),
                RoutedStream::Backend(s) => Pin::new_unchecked(s).poll_next(cx),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::pin::Pin;
    use std::sync::Arc;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    use crate::backend::Backend;
    use crate::bufferpool::{BulkRef, Error, PageRef, PageStream, Req, StripeKey};
    use crate::fabric::PeerId;
    use crate::p2p::{
        FingerTable, FingerTableConfig, NodeId, PeerEntry, RingId, TopologyLabels, node_to_ring,
        stripe_to_ring,
    };
    use serde::{Deserialize, Serialize};

    use super::RoutedStream;

    #[derive(Serialize, Deserialize)]
    struct TestReq(StripeKey);

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    /// Backend that echoes the requested `dsts` back as delivered
    /// pages, one `poll_next` at a time.
    struct EchoBackend;

    struct VecStream {
        pages: Vec<PageRef>,
        next: usize,
    }

    impl PageStream for VecStream {
        fn poll_next(
            mut self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, Error>>> {
            if self.next >= self.pages.len() {
                return Poll::Ready(None);
            }
            let page = self.pages[self.next];
            self.next += 1;
            Poll::Ready(Some(Ok(page)))
        }
    }

    impl Backend for EchoBackend {
        type Req = TestReq;
        type Stream<'a> = VecStream;

        fn bulk_get<'a>(
            &'a self,
            _req: &'a Self::Req,
            _src: BulkRef,
            dsts: &'a [PageRef],
        ) -> Self::Stream<'a> {
            VecStream {
                pages: dsts.to_vec(),
                next: 0,
            }
        }
    }

    fn peer(node: u64) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: node_to_ring(NodeId(node)),
            labels: TopologyLabels(vec!["r".to_string()]),
        }
    }

    fn key_for_ring(ring: u64) -> StripeKey {
        let mut k = [0u8; 32];
        k[..8].copy_from_slice(&ring.to_le_bytes());
        StripeKey(k)
    }

    fn node_to_peer_map(nodes: &[u64]) -> Arc<HashMap<NodeId, PeerId>> {
        Arc::new(nodes.iter().map(|&n| (NodeId(n), PeerId(n))).collect())
    }

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        // SAFETY: VTABLE clone/wake/drop are all no-ops on static data.
        unsafe { Waker::from_raw(raw()) }
    }

    /// Build a `RoutedTransport` whose `FabricTransport` is never
    /// driven. Constructing it without a live fabric is infeasible, so
    /// these tests exercise the routing decision (`owns_locally`) and
    /// the backend arm directly. The `owns_locally` predicate is the
    /// sole input to arm selection; the fabric arm itself is covered
    /// by the Phase 4 fabric loopback tests.
    ///
    /// We avoid constructing the full transport by testing
    /// `FingerTable::next_hop` (which `owns_locally` wraps) and the
    /// `RoutedStream::Backend` arm in isolation.
    #[test]
    fn single_node_owns_every_stripe() {
        // Single-node table: this node owns everything, so the
        // routing decision is always "Backend arm".
        let local = peer(1);
        let fingers = FingerTable::build(local.clone(), &[], FingerTableConfig { k: 8 });
        let _ = node_to_peer_map(&[1]);
        for target in [0u64, 1, 999, u64::MAX / 2, u64::MAX] {
            assert!(
                fingers
                    .next_hop(stripe_to_ring(key_for_ring(target)))
                    .is_none(),
                "single node must own target {target}",
            );
        }
    }

    #[test]
    fn peer_owned_stripe_is_not_local() {
        // Two-node ring: a stripe the other node owns must NOT be
        // owned locally, so `bulk_get` would select the Fabric arm.
        let local = peer(1);
        let other = peer(2);
        let fingers = FingerTable::build(
            local.clone(),
            std::slice::from_ref(&other),
            FingerTableConfig { k: 8 },
        );
        let req = TestReq(key_for_ring(other.ring.0));
        assert!(
            fingers.next_hop(stripe_to_ring(req.key())).is_some(),
            "peer-owned stripe must route off-node (Fabric arm)",
        );
    }

    #[test]
    fn backend_arm_yields_backend_pages() {
        // Drive the Backend arm directly: a `RoutedStream::Backend`
        // wrapping the EchoBackend must yield the requested pages.
        let backend = EchoBackend;
        let req = TestReq(StripeKey([7u8; 32]));
        let src = BulkRef {
            stripe: req.key(),
            offset: 0,
            len: 4096,
        };
        let dsts = [
            PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            },
        ];
        let stream: RoutedStream<'_, TestReq, EchoBackend> =
            RoutedStream::Backend(backend.bulk_get(&req, src, &dsts));
        let mut stream = stream;
        // SAFETY: pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut out = Vec::new();
        let mut spins = 0u64;
        loop {
            match stream.as_mut().poll_next(&mut cx) {
                Poll::Ready(None) => break,
                Poll::Ready(Some(Ok(p))) => out.push(p),
                Poll::Ready(Some(Err(e))) => panic!("unexpected error: {e:?}"),
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
        assert_eq!(out, dsts.to_vec());
    }

    /// `RingId` is re-exported for the doc-link above; reference it so
    /// the import is not flagged unused on toolchains that lint test
    /// imports.
    #[test]
    fn ring_id_is_in_scope() {
        let _ = RingId(0);
    }
}
