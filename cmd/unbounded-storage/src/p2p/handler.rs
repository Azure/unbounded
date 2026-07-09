// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server-side recursive [`Handler`] for stripe reads.
//!
//! The handler resolves Chord-routed peer requests. When this node owns
//! the stripe, bytes are served from the shard-local bufferpool through
//! [`crate::fanout::FetchService`]: the owner shard keeps the actual
//! [`crate::bufferpool::PageGuard`] pinned, while the RPC worker receives
//! only a registered-memory location and a drop-owned release token. This
//! preserves zero-copy semantics and keeps the bufferpool cache as the
//! only owner-side page cache.
//!
//! Forwarding still uses a dedicated scratch backing: an intermediate hop
//! asks the next hop to RDMA-write into scratch, then relays that scratch
//! page upstream.

use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::bufferpool::{BulkRef, Error as PoolError, PageRef, PageStream, Req, StripeKey};
use crate::fabric::scratch::ScratchBacking;
use crate::fabric::{Fabric, FabricPage, FabricTransport, Handler, HandlerStream, MrHandle};
use crate::fanout::{FetchChannel, FetchEvent, FetchStream};
use crate::memory::Backing;
use crate::p2p::{ChainFingerRouter, FingerTable, RingId, RouteTableHandle, stripe_to_ring};
use crate::runtime::{block_on_cooperative, noop_waker};
use crate::storage::disks::CacheDirectorySet;
use crate::storage::{StripeReq, disk_for};

/// One local shard whose bufferpool backing may be used as an owner-side
/// source for recursive RPC responses.
#[derive(Clone)]
pub struct OwnerShardSource {
    pub shard_index: usize,
    pub channel: FetchChannel,
    pub mr: MrHandle,
    pub numa: Option<u16>,
}

/// Owner-shard selector for one fabric unit.
///
/// The table is intentionally scoped to the shards assigned to the same
/// fabric unit, so the RPC worker only sources memory regions registered
/// with its own libfabric domain.
#[derive(Clone)]
pub struct OwnerShardTable {
    entries: Arc<Vec<OwnerShardSource>>,
    cache_directories: Arc<CacheDirectorySet>,
    page_size: usize,
}

impl OwnerShardTable {
    pub fn new(
        entries: Vec<OwnerShardSource>,
        cache_directories: Arc<CacheDirectorySet>,
        page_size: usize,
    ) -> Self {
        Self {
            entries: Arc::new(entries),
            cache_directories,
            page_size,
        }
    }

    fn pick(&self, req: &StripeReq, src: BulkRef) -> Option<OwnerShardSource> {
        let entries = self.entries.as_slice();
        if entries.is_empty() {
            return None;
        }

        let disk_snapshot = self.cache_directories.snapshot(req.cache_id());
        let drive_numa = disk_snapshot
            .as_ref()
            .map(|snapshot| snapshot.drive_numa.as_slice())
            .unwrap_or_default();
        let target_numa = if drive_numa.is_empty() {
            None
        } else {
            drive_numa
                .get(disk_for(&req.key(), src.offset, drive_numa.len()))
                .copied()
                .flatten()
        };

        if let Some(numa) = target_numa {
            let matches = entries
                .iter()
                .filter(|entry| entry.numa == Some(numa))
                .count();
            if matches > 0 {
                let want = (shard_hash(req.key(), src.offset) as usize) % matches;
                return entries
                    .iter()
                    .filter(|entry| entry.numa == Some(numa))
                    .nth(want)
                    .cloned();
            }
        }

        let idx = (shard_hash(req.key(), src.offset) as usize) % entries.len();
        entries.get(idx).cloned()
    }

    fn page_size(&self) -> usize {
        self.page_size
    }
}

fn shard_hash(key: StripeKey, stripe_off: u64) -> u64 {
    let mut first = [0u8; 8];
    let mut second = [0u8; 8];
    first.copy_from_slice(&key.0[..8]);
    second.copy_from_slice(&key.0[8..16]);
    u64::from_le_bytes(first) ^ u64::from_le_bytes(second).rotate_left(17) ^ stripe_off
}

/// Error surfaced by [`RecursiveHandler`]'s response stream.
#[derive(Debug)]
pub enum RecursiveHandlerError {
    /// The handler's scratch backing is fully checked out. The peer
    /// should retry; this is a transient resource limit, not a data
    /// error.
    NoScratchPage,
    /// This node is an intermediate hop but the request's hop budget
    /// is exhausted. The recursion is cut short to bound forwarding.
    HopLimitExceeded,
    /// This fabric unit has no local shard from which it may source an
    /// owner response.
    NoOwnerShard,
    /// The owner shard's bufferpool/fetch service failed.
    OwnerFetch(PoolError),
    /// Forwarding to the next hop over the fabric failed.
    Forward(PoolError),
}

impl std::fmt::Display for RecursiveHandlerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RecursiveHandlerError::NoScratchPage => write!(f, "no scratch page available"),
            RecursiveHandlerError::HopLimitExceeded => {
                write!(f, "recursive forward hop limit exceeded")
            }
            RecursiveHandlerError::NoOwnerShard => write!(f, "no local owner shard available"),
            RecursiveHandlerError::OwnerFetch(e) => write!(f, "owner fetch: {e}"),
            RecursiveHandlerError::Forward(e) => write!(f, "forward to next hop: {e}"),
        }
    }
}

impl std::error::Error for RecursiveHandlerError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            RecursiveHandlerError::OwnerFetch(e) | RecursiveHandlerError::Forward(e) => Some(e),
            RecursiveHandlerError::NoScratchPage
            | RecursiveHandlerError::HopLimitExceeded
            | RecursiveHandlerError::NoOwnerShard => None,
        }
    }
}

/// Recursive handler resolving stripe reads by routing through the
/// finger table.
pub struct RecursiveHandler {
    scratch: Arc<ScratchBacking>,
    routes: RouteTableHandle,
    forward: FabricTransport<StripeReq, ChainFingerRouter>,
    owners: OwnerShardTable,
}

impl RecursiveHandler {
    /// Build a recursive handler over an existing route table.
    pub fn with_routes(
        scratch: Backing,
        scratch_pages: u32,
        routes: RouteTableHandle,
        fabric: Arc<Fabric>,
        scratch_mr: MrHandle,
        page_size: usize,
        owners: OwnerShardTable,
    ) -> crate::fabric::Result<Self> {
        let scratch = Arc::new(ScratchBacking::new(scratch, scratch_pages));
        let router = ChainFingerRouter::new(routes.clone());
        let forward = FabricTransport::new(fabric, scratch_mr, router, page_size)?;
        Ok(Self {
            scratch,
            routes,
            forward,
            owners,
        })
    }
}

impl Handler<StripeReq> for RecursiveHandler {
    type Error = RecursiveHandlerError;
    type Stream<'a>
        = RecursiveHandlerStream<'a>
    where
        Self: 'a;

    fn handle<'a>(
        &'a self,
        req: &'a StripeReq,
        src: BulkRef,
        hops_remaining: u32,
    ) -> Self::Stream<'a> {
        RecursiveHandlerStream {
            scratch: self.scratch.clone(),
            fingers: self.routes.route_for_req(req).map(|route| route.fingers),
            forward: &self.forward,
            owners: &self.owners,
            req,
            owned_req: req.clone(),
            key: req.key(),
            src,
            hops_remaining,
            state: StreamState::Pending,
            page_idx: None,
            owner_stream: None,
        }
    }
}

#[derive(Copy, Clone, PartialEq, Eq)]
enum StreamState {
    Pending,
    Done,
    Ended,
}

/// Where a request resolves on this node.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
enum Route {
    /// This node owns the stripe; serve from a local shard bufferpool.
    Owner,
    /// This node is an intermediate hop; forward downstream.
    Forward,
    /// Intermediate hop with no remaining hop budget.
    HopLimit,
}

/// Classify a request by finger-table ownership and hop budget. Pure
/// over `(fingers, target, hops_remaining)` so the routing decision is
/// unit-testable without a live fabric.
fn classify(fingers: &FingerTable, target: RingId, hops_remaining: u32) -> Route {
    match fingers.next_hop(target) {
        None => Route::Owner,
        Some(_) if hops_remaining == 0 => Route::HopLimit,
        Some(_) => Route::Forward,
    }
}

/// Response stream for one request. Resolves on first poll, yields
/// exactly one page on success then ends, or yields the error then ends.
pub struct RecursiveHandlerStream<'a> {
    scratch: Arc<ScratchBacking>,
    fingers: Option<Arc<FingerTable>>,
    forward: &'a FabricTransport<StripeReq, ChainFingerRouter>,
    owners: &'a OwnerShardTable,
    req: &'a StripeReq,
    owned_req: StripeReq,
    key: StripeKey,
    src: BulkRef,
    hops_remaining: u32,
    state: StreamState,
    page_idx: Option<u32>,
    owner_stream: Option<FetchStream>,
}

impl Unpin for RecursiveHandlerStream<'_> {}

impl HandlerStream for RecursiveHandlerStream<'_> {
    type Error = RecursiveHandlerError;

    fn poll_next(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<FabricPage, RecursiveHandlerError>>> {
        match self.state {
            StreamState::Done | StreamState::Ended => {
                self.state = StreamState::Ended;
                Poll::Ready(None)
            }
            StreamState::Pending => match self.serve() {
                Ok(page) => {
                    self.state = StreamState::Done;
                    Poll::Ready(Some(Ok(page)))
                }
                Err(e) => {
                    self.state = StreamState::Ended;
                    Poll::Ready(Some(Err(e)))
                }
            },
        }
    }
}

impl RecursiveHandlerStream<'_> {
    fn serve(&mut self) -> Result<FabricPage, RecursiveHandlerError> {
        let page_size = self.scratch.page_size();
        if self.req.fabric_only() {
            crate::metrics::p2p_request(crate::metrics::Disposition::Local);

            let (idx, page) = serve_synthetic_page(&self.scratch, self.src, page_size)?;
            self.page_idx = Some(idx);

            return Ok(page.into());
        }

        let target = stripe_to_ring(self.key);
        let route = self
            .fingers
            .as_deref()
            .map(|fingers| classify(fingers, target, self.hops_remaining))
            .unwrap_or(Route::Owner);
        match route {
            Route::HopLimit => {
                crate::metrics::p2p_hop_limit_exceeded();
                Err(RecursiveHandlerError::HopLimitExceeded)
            }
            Route::Owner => {
                crate::metrics::p2p_request(crate::metrics::Disposition::Local);
                self.serve_owner()
            }
            Route::Forward => {
                crate::metrics::p2p_request(crate::metrics::Disposition::Forward);
                let idx = self
                    .scratch
                    .take_zeroed()
                    .ok_or(RecursiveHandlerError::NoScratchPage)?;
                self.page_idx = Some(idx);
                let dst = scratch_page(idx, page_size);
                serve_forward(
                    self.forward,
                    self.req,
                    self.src,
                    self.hops_remaining - 1,
                    idx,
                    page_size,
                    dst,
                )?;
                Ok(self.clamped_page(idx, page_size).into())
            }
        }
    }

    fn serve_owner(&mut self) -> Result<FabricPage, RecursiveHandlerError> {
        let owner = self
            .owners
            .pick(self.req, self.src)
            .ok_or(RecursiveHandlerError::NoOwnerShard)?;
        let mut stream = owner
            .channel
            .fetch(self.owned_req.clone(), self.src.offset, self.src.len as u64)
            .map_err(RecursiveHandlerError::OwnerFetch)?;
        let event =
            block_on_local(stream.next_event()).map_err(RecursiveHandlerError::OwnerFetch)?;
        let page = match event {
            FetchEvent::Page(page) => page,
            FetchEvent::Done => {
                return Err(RecursiveHandlerError::OwnerFetch(PoolError::from(
                    "owner fetch ended without a page",
                )));
            }
        };

        let hold = stream.pin_release_hold(page.pin_token);
        let token = hold.token();
        hold.close();

        let byte_offset = page.loc.page_byte_offset as usize;
        let page_size = self.owners.page_size();
        let page_idx = u32::try_from(byte_offset / page_size)
            .map_err(|_| RecursiveHandlerError::OwnerFetch(PoolError::PageOutOfRange))?;
        let offset = u32::try_from(byte_offset % page_size)
            .map_err(|_| RecursiveHandlerError::OwnerFetch(PoolError::OffsetOutOfRange))?;
        let len = page.loc.len.min(self.src.len);

        let page = FabricPage::registered(
            PageRef {
                page_idx,
                offset,
                len,
            },
            owner.mr,
            Some(Box::new(token)),
        );

        self.owner_stream = Some(stream);

        Ok(page)
    }

    /// The yielded scratch page, clamped to the requested intra-stripe window.
    fn clamped_page(&self, idx: u32, page_size: usize) -> PageRef {
        let len = (self.src.len as usize).min(page_size) as u32;
        PageRef {
            page_idx: idx,
            offset: 0,
            len,
        }
    }
}

impl Drop for RecursiveHandlerStream<'_> {
    fn drop(&mut self) {
        if let Some(idx) = self.page_idx.take() {
            self.scratch.give(idx);
        }
    }
}

/// Full-page scratch destination for index `idx`.
fn scratch_page(idx: u32, page_size: usize) -> PageRef {
    PageRef {
        page_idx: idx,
        offset: 0,
        len: page_size as u32,
    }
}

/// Synthetic fabric benchmark path: reserve one zeroed scratch page and
/// return it as if it were a resolved stripe. The RPC layer still sends
/// the response by RDMA-writing this page to the requester.
fn serve_synthetic_page(
    scratch: &ScratchBacking,
    src: BulkRef,
    page_size: usize,
) -> Result<(u32, PageRef), RecursiveHandlerError> {
    let idx = scratch
        .take_zeroed()
        .ok_or(RecursiveHandlerError::NoScratchPage)?;
    let len = (src.len as usize).min(page_size) as u32;

    Ok((
        idx,
        PageRef {
            page_idx: idx,
            offset: 0,
            len,
        },
    ))
}

/// Forward path: hand the request to the next hop with TTL `ttl`,
/// landing the downstream page in scratch slot `dst_idx` via RDMA.
fn serve_forward<R>(
    forward: &FabricTransport<R, ChainFingerRouter>,
    req: &R,
    src: BulkRef,
    ttl: u32,
    dst_idx: u32,
    page_size: usize,
    dst: PageRef,
) -> Result<(), RecursiveHandlerError>
where
    R: Req + Serialize + DeserializeOwned + 'static,
{
    let dest_offset = (dst_idx as usize).saturating_mul(page_size);
    let stream = forward.bulk_get_forward(req, src, std::slice::from_ref(&dst), ttl, dest_offset);
    drive_page_stream(stream).map_err(RecursiveHandlerError::Forward)
}

/// Drive a [`PageStream`] to completion with a noop waker on the current
/// thread. Page payloads land in the caller-provided `dst` via stream
/// side effects, so yielded [`PageRef`]s are not inspected here.
fn drive_page_stream<P: PageStream>(stream: P) -> Result<(), PoolError> {
    let mut stream = std::pin::pin!(stream);
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    loop {
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(None) => return Ok(()),
            Poll::Ready(Some(Ok(_))) => {}
            Poll::Ready(Some(Err(e))) => return Err(e),
            Poll::Pending => std::thread::sleep(std::time::Duration::from_micros(50)),
        }
    }
}

fn block_on_local<F: std::future::Future>(fut: F) -> F::Output {
    block_on_cooperative(fut, || {
        std::thread::sleep(std::time::Duration::from_micros(50))
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::p2p::{FingerTableConfig, NodeId, PeerEntry, TopologyTags, node_to_ring};

    const PAGE: usize = 4096;
    const SCRATCH_PAGES: usize = 4;

    fn scratch_backing() -> (Backing, *mut u8) {
        let buf = vec![0u8; PAGE * SCRATCH_PAGES].into_boxed_slice();
        let base = Box::leak(buf).as_mut_ptr();
        let backing = Backing {
            base,
            page_size: PAGE,
            page_count: SCRATCH_PAGES,
            keepalive: std::sync::Arc::new(()),
        };
        (backing, base)
    }

    fn peer(node: u64) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: node_to_ring(NodeId(node)),
            tags: TopologyTags(vec!["r".to_string()]),
        }
    }

    fn key_for_ring(ring: u64) -> StripeKey {
        let mut k = [0u8; 32];
        k[..8].copy_from_slice(&ring.to_le_bytes());
        StripeKey(k)
    }

    fn src_for(key: StripeKey) -> BulkRef {
        BulkRef {
            stripe: key,
            offset: 0,
            len: PAGE as u32,
        }
    }

    #[test]
    fn classify_single_node_owns_everything() {
        let local = peer(1);
        let fingers = FingerTable::build(local.clone(), &[], FingerTableConfig::with_k(8));
        for target in [0u64, 1, 999, u64::MAX] {
            let r = classify(
                &fingers,
                stripe_to_ring(key_for_ring(target)),
                crate::fabric::MAX_HOPS,
            );
            assert_eq!(r, Route::Owner, "target {target}");
        }
    }

    #[test]
    fn classify_peer_owned_forwards_when_budget_remains() {
        let local = peer(1);
        let other = peer(2);
        let fingers = FingerTable::build(
            local,
            std::slice::from_ref(&other),
            FingerTableConfig::with_k(8),
        );
        let target = stripe_to_ring(key_for_ring(other.ring.0));
        assert_eq!(classify(&fingers, target, 5), Route::Forward);
    }

    #[test]
    fn classify_peer_owned_hits_hop_limit_at_zero() {
        let local = peer(1);
        let other = peer(2);
        let fingers = FingerTable::build(
            local,
            std::slice::from_ref(&other),
            FingerTableConfig::with_k(8),
        );
        let target = stripe_to_ring(key_for_ring(other.ring.0));
        assert_eq!(classify(&fingers, target, 0), Route::HopLimit);
    }

    #[test]
    fn owner_table_prefers_drive_numa_within_unit() {
        let (ch0, _rx0) = FetchChannel::new();
        let (ch1, _rx1) = FetchChannel::new();
        let mr = MrHandle {
            mr: std::ptr::null_mut(),
            remote_key: 0,
            base: 0,
            remote_base: 0,
            len: PAGE,
        };
        let directories = CacheDirectorySet::new();
        directories.apply_channels(
            "cache-a",
            vec![(
                std::path::PathBuf::from("/a"),
                dummy_page_channel(),
                Some(1),
                true,
            )],
        );
        let table = OwnerShardTable::new(
            vec![
                OwnerShardSource {
                    shard_index: 0,
                    channel: ch0,
                    mr,
                    numa: Some(0),
                },
                OwnerShardSource {
                    shard_index: 1,
                    channel: ch1,
                    mr,
                    numa: Some(1),
                },
            ],
            directories,
            PAGE,
        );
        let key = key_for_ring(7);
        let req = StripeReq::new(key).with_cache_id(Some("cache-a".to_string()));

        let picked = table.pick(&req, src_for(key)).expect("owner shard");

        assert_eq!(picked.shard_index, 1);
    }

    #[test]
    fn fabric_only_request_yields_zeroed_scratch() {
        let (backing, base) = scratch_backing();
        let key = key_for_ring(11);
        let scratch = ScratchBacking::new(backing, SCRATCH_PAGES as u32);

        let (_idx, page) = match serve_synthetic_page(&scratch, src_for(key), PAGE) {
            Ok(page) => page,
            Err(e) => panic!("fabric-only request failed: {e}"),
        };

        assert_eq!(page.page_idx, 0);
        assert_eq!(page.len, PAGE as u32);
        assert!(page_all(base, page.page_idx).iter().all(|&b| b == 0));
    }

    #[test]
    fn recycled_scratch_page_is_zeroed_on_checkout() {
        let (backing, base) = scratch_backing();
        let scratch = ScratchBacking::new(backing, 1);

        let idx = scratch.take_zeroed().expect("slot available");
        fill_page(base, idx, 0xEE);
        scratch.give(idx);

        let idx = scratch.take_zeroed().expect("slot available");
        assert!(
            page_all(base, idx).iter().all(|&b| b == 0),
            "recycled scratch page leaked prior request bytes"
        );
    }

    #[test]
    fn short_fill_does_not_relay_prior_bytes() {
        let (backing, base) = scratch_backing();
        let scratch = ScratchBacking::new(backing, 1);

        let idx = scratch.take_zeroed().expect("slot available");
        fill_page(base, idx, 0xEE);
        scratch.give(idx);

        let idx = scratch.take_zeroed().expect("slot available");
        fill_page_head(base, idx, 0x11, 64);

        let page = page_all(base, idx);
        assert!(page[..64].iter().all(|&b| b == 0x11), "head fill missing");
        assert!(
            page[64..].iter().all(|&b| b == 0),
            "tail leaked prior request bytes instead of zero"
        );
    }

    fn dummy_page_channel() -> crate::storage::PageChannel {
        let (tx, _rx) = crate::storage::PageChannel::new();
        tx
    }

    fn fill_page(base: *mut u8, idx: u32, byte: u8) {
        // SAFETY: idx is within the test backing.
        unsafe {
            std::ptr::write_bytes(base.add(idx as usize * PAGE), byte, PAGE);
        }
    }

    fn fill_page_head(base: *mut u8, idx: u32, byte: u8, n: usize) {
        // SAFETY: idx is within the test backing and n <= PAGE.
        unsafe {
            std::ptr::write_bytes(base.add(idx as usize * PAGE), byte, n);
        }
    }

    fn page_all(base: *mut u8, idx: u32) -> Vec<u8> {
        // SAFETY: idx is within the test backing.
        unsafe { std::slice::from_raw_parts(base.add(idx as usize * PAGE), PAGE).to_vec() }
    }
}
