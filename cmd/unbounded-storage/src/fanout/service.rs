// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Owner-side service for the cross-shard stripe-fetch channel.
//!
//! A [`FetchService`] runs as a tick hook on the shard that owns a set
//! of content-addressed stripes. It drains [`FetchCommand`]s sent by
//! coordinator shards, fetches the requested stripe slices through its
//! own local [`Pool`], and *pins* the resulting pages in its registered
//! backing by holding owned [`PageGuard`]s. It then replies with the
//! in-backing byte locations of those pages so the coordinator can issue
//! zero-copy `SEND_ZC` directly from this shard's backing.
//!
//! The pages stay pinned until the coordinator sends a matching
//! [`FetchCommand::Release`], which it does only after the NIC has
//! signalled `SEND_ZC` completion for every page of the pin. Releasing
//! before that completion would let the pool recycle the page while the
//! NIC is still reading it, so the release ordering is a hard
//! correctness requirement enforced by the coordinator.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Receiver, TryRecvError};
use std::task::{Context, Poll};

use super::channel::{FetchChannelReceiver, FetchCommand, FetchReply, PageLoc, ReplySlot};
use crate::bufferpool::{BlockStore, Error, PageGuard, Pool, Transport};
use crate::storage::StripeReq;

/// Pinned pages produced by one fetch, plus their in-backing locations
/// in ascending stripe order. The guards keep the pages pinned; the
/// locations are what the coordinator needs to `SEND_ZC`.
type FetchResult = Result<(Vec<PageGuard<'static>>, Vec<PageLoc>), Error>;

/// A fetch in progress on the owner's executor. `!Send` by construction
/// (it captures an `Rc<Pool>` and yields `PageGuard`s), which is fine:
/// the service is pinned to one core.
type FetchFuture = Pin<Box<dyn Future<Output = FetchResult>>>;

struct Inflight {
    reply: Arc<ReplySlot<FetchReply>>,
    fut: FetchFuture,
}

/// Owner-side tick hook draining a [`FetchChannelReceiver`].
///
/// Generic over the pool's transport and block store rather than the
/// [`BufferPool`](crate::bufferpool::BufferPool) trait because it relies
/// on the inherent `Pool::read_owned` owned-stream path, which yields
/// `PageGuard<'static>` guards the service can retain across the
/// cross-shard round-trip.
pub struct FetchService<T, S>
where
    T: Transport<StripeReq> + 'static,
    S: BlockStore + 'static,
{
    pool: Rc<Pool<T, S, StripeReq>>,
    rx: Receiver<FetchCommand>,
    service_alive: Arc<AtomicBool>,
    page_size: u64,
    pins: HashMap<u64, Vec<PageGuard<'static>>>,
    next_token: u64,
    in_flight: Vec<Inflight>,
    disconnected: bool,
}

impl<T, S> FetchService<T, S>
where
    T: Transport<StripeReq> + 'static,
    S: BlockStore + 'static,
{
    /// Build a service over `pool`, draining `rx`. `page_size` is the
    /// pool's page size in bytes, used to turn a page's `(page_idx,
    /// offset)` into an absolute byte offset within the backing.
    pub fn new(
        pool: Rc<Pool<T, S, StripeReq>>,
        rx: FetchChannelReceiver,
        page_size: usize,
    ) -> Self {
        let (rx, service_alive) = rx.into_parts();
        Self {
            pool,
            rx,
            service_alive,
            page_size: page_size as u64,
            pins: HashMap::new(),
            next_token: 0,
            in_flight: Vec::new(),
            disconnected: false,
        }
    }

    /// Advance the service once: admit any newly queued commands, then
    /// poll outstanding fetches and answer the ones that completed.
    pub fn poll_once(&mut self, cx: &mut Context<'_>) {
        self.drain_commands();
        self.poll_in_flight(cx);
    }

    /// Shard-loop tick hook: poll the service under a noop waker and
    /// report whether work remains. Returns `true` while any fetch is
    /// still resolving so the loop keeps spinning; newly arrived
    /// cross-shard commands are picked up by the next `try_recv` within
    /// the loop's idle interval.
    pub fn progress(&mut self) -> bool {
        let waker = crate::runtime::noop_waker();
        let mut cx = Context::from_waker(&waker);
        self.poll_once(&mut cx);
        self.has_inflight()
    }

    /// True while any fetch is still resolving. The shard loop uses this
    /// to decide whether it still needs to be polled.
    pub fn has_inflight(&self) -> bool {
        !self.in_flight.is_empty()
    }

    /// True once every coordinator sender has been dropped and no fetch
    /// remains in flight, i.e. the service can be torn down.
    pub fn is_finished(&self) -> bool {
        self.disconnected && self.in_flight.is_empty()
    }

    /// Mark the service dead so any coordinator parked in `fetch`
    /// resolves `EIO` instead of hanging. Idempotent.
    pub fn mark_dead(&self) {
        self.service_alive.store(false, Ordering::Release);
    }

    fn drain_commands(&mut self) {
        loop {
            match self.rx.try_recv() {
                Ok(FetchCommand::Fetch {
                    req,
                    intra_offset,
                    intra_len,
                    reply,
                }) => self.spawn_fetch(req, intra_offset, intra_len, reply),
                Ok(FetchCommand::Release { pin_token }) => {
                    // Dropping the guards unpins the pages; an unknown
                    // token (already released, or never issued) is a
                    // harmless no-op.
                    self.pins.remove(&pin_token);
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    self.disconnected = true;
                    break;
                }
            }
        }
    }

    fn spawn_fetch(
        &mut self,
        req: StripeReq,
        intra_offset: u64,
        intra_len: u64,
        reply: Arc<ReplySlot<FetchReply>>,
    ) {
        let pool = self.pool.clone();
        let page_size = self.page_size;
        let fut: FetchFuture = Box::pin(async move {
            let mut stream = pool.read_owned(&req, intra_offset, intra_len)?;
            let mut guards = Vec::new();
            let mut pages = Vec::new();
            while let Some(page) = stream.next_page_owned().await {
                let page = page?;
                let pr = page.page_ref();
                pages.push(PageLoc {
                    page_byte_offset: pr.page_idx as u64 * page_size + pr.offset as u64,
                    len: pr.len,
                });
                guards.push(page);
            }

            Ok((guards, pages))
        });
        self.in_flight.push(Inflight { reply, fut });
    }

    fn poll_in_flight(&mut self, cx: &mut Context<'_>) {
        let mut i = 0;
        while i < self.in_flight.len() {
            match self.in_flight[i].fut.as_mut().poll(cx) {
                Poll::Ready(result) => {
                    // swap_remove drops the borrow on slot `i` and moves
                    // the last entry into its place, so we re-poll the
                    // same index next iteration without advancing.
                    let done = self.in_flight.swap_remove(i);
                    self.complete(done.reply, result);
                }
                Poll::Pending => i += 1,
            }
        }
    }

    fn complete(&mut self, reply: Arc<ReplySlot<FetchReply>>, result: FetchResult) {
        match result {
            Ok((guards, pages)) => {
                let pin_token = self.next_token;
                self.next_token = self.next_token.wrapping_add(1);
                // Retain the guards under the token; the matching
                // Release (or service teardown) drops them.
                self.pins.insert(pin_token, guards);
                reply.set(Ok(FetchReply { pin_token, pages }));
            }
            Err(e) => reply.set(Err(e)),
        }
    }
}

impl<T, S> Drop for FetchService<T, S>
where
    T: Transport<StripeReq> + 'static,
    S: BlockStore + 'static,
{
    fn drop(&mut self) {
        // Any coordinator still parked in `fetch` must observe EIO
        // rather than hang once this service goes away.
        self.mark_dead();
    }
}
