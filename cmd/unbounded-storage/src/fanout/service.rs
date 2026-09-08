// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Owner-side service for the cross-shard stripe-fetch channel.
//!
//! A [`FetchService`] runs as a tick hook on the shard that owns a set
//! of content-addressed stripes. It drains [`FetchCommand`]s sent by
//! coordinator shards, fetches the requested stripe slices through its
//! own local [`Pool`], and *pins* each resulting page in its registered
//! backing by holding an owned [`PageGuard`]. It then emits the
//! in-backing byte location of that page so the coordinator can issue
//! zero-copy `SEND_ZC` directly from this shard's backing.
//!
//! Each page stays pinned until the coordinator sends a matching
//! [`FetchCommand::Release`], which it does only after the NIC has
//! signaled `SEND_ZC` completion for that page. Releasing before that
//! completion would let the pool recycle the page while the NIC is still
//! reading it, so the release ordering is a hard correctness requirement
//! enforced by the coordinator.

use std::collections::{HashMap, HashSet};
use std::pin::Pin;
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Receiver, TryRecvError};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use super::channel::{
    EventSlot, FetchChannelReceiver, FetchCommand, FetchEvent, FetchPage, PageLoc,
};
use crate::bufferpool::{
    BlockStore, Error, OwnedPageFuture, PageGuard, Pool, ReadStream, Transport,
};
use crate::storage::StripeReq;

const MAX_EMITTED_PINS_PER_FETCH: usize = 2;

struct Inflight {
    events: Arc<EventSlot>,
    task: FetchTask,
}

struct PinEntry {
    fetch_id: u64,
    sending: bool,
    #[allow(dead_code)]
    guard: PageGuard<'static>,
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
    pins: HashMap<u64, PinEntry>,
    fetch_pins: HashMap<u64, HashSet<u64>>,
    next_token: u64,
    in_flight: Vec<Inflight>,
    disconnected: bool,
    /// Waker the in-flight fetch futures are polled with. This is the
    /// owning shard's flag-flipping waker, so a cross-thread completion
    /// (a disk reply or fabric event) that wakes a fetch sets the
    /// shard's `wake_flag` and re-polls promptly, letting the shard park
    /// while merely waiting instead of busy-spinning.
    waker: Waker,
    command_waker: Arc<Mutex<Option<Waker>>>,
}

impl<T, S> FetchService<T, S>
where
    T: Transport<StripeReq> + 'static,
    S: BlockStore + 'static,
{
    /// Build a service over `pool`, draining `rx`. `page_size` is the
    /// pool's page size in bytes, used to turn a page's `(page_idx,
    /// offset)` into an absolute byte offset within the backing. `waker`
    /// is the owning shard's flag-flipping waker (see
    /// [`ShardLoop::waker`](crate::runtime::ShardLoop::waker)); in-flight
    /// fetch futures are polled with it so cross-thread completions wake
    /// the shard loop instead of relying on a busy-spin.
    pub fn new(
        pool: Rc<Pool<T, S, StripeReq>>,
        rx: FetchChannelReceiver,
        page_size: usize,
        waker: Waker,
    ) -> Self {
        let (rx, service_alive, command_waker) = rx.into_parts();
        *command_waker.lock().unwrap() = Some(waker.clone());
        Self {
            pool,
            rx,
            service_alive,
            page_size: page_size as u64,
            pins: HashMap::new(),
            fetch_pins: HashMap::new(),
            next_token: 0,
            in_flight: Vec::new(),
            disconnected: false,
            waker,
            command_waker,
        }
    }

    /// Advance the service once: admit any newly queued commands, then
    /// poll outstanding fetches and answer the ones that completed.
    /// Returns whether it made progress this tick (admitted a command or
    /// completed a fetch).
    pub fn poll_once(&mut self, cx: &mut Context<'_>) -> bool {
        let admitted = self.drain_commands();
        let completed = self.poll_in_flight(cx);
        admitted || completed
    }

    /// Shard-loop tick hook: poll the service under the shard's waker and
    /// report whether it did work this tick.
    ///
    /// Reports busy only when it actually made progress (a command was
    /// admitted or a fetch completed), not merely because a fetch is
    /// still resolving. While a fetch is in flight the shard loop is free
    /// to park its idle interval; the fetch future is polled with the
    /// shard's flag-flipping waker, so a cross-thread completion sets the
    /// shard's `wake_flag` and triggers a prompt re-poll. Reporting
    /// perpetual busyness instead would pin the shard thread at 100% CPU
    /// and starve co-located threads (the fabric progress thread, the
    /// origin) on a CPU-constrained host. Newly arrived cross-shard
    /// commands are picked up by the next `try_recv` within the loop's
    /// idle interval.
    pub fn progress(&mut self) -> bool {
        let waker = self.waker.clone();
        let mut cx = Context::from_waker(&waker);
        self.poll_once(&mut cx)
    }

    /// True while any fetch is still resolving or retained page pin is
    /// awaiting a coordinator release.
    pub fn has_inflight(&self) -> bool {
        !self.in_flight.is_empty() || !self.pins.is_empty()
    }

    /// True once every coordinator sender has been dropped and no fetch
    /// remains in flight, i.e. the service can be torn down.
    pub fn is_finished(&self) -> bool {
        self.disconnected && !self.has_inflight()
    }

    /// Mark the service dead so any coordinator parked in `fetch`
    /// resolves `EIO` instead of hanging. Idempotent.
    pub fn mark_dead(&self) {
        self.service_alive.store(false, Ordering::Release);
    }

    fn drain_commands(&mut self) -> bool {
        let mut admitted = false;
        loop {
            match self.rx.try_recv() {
                Ok(FetchCommand::Fetch {
                    fetch_id,
                    req,
                    intra_offset,
                    intra_len,
                    events,
                }) => {
                    self.spawn_fetch(fetch_id, req, intra_offset, intra_len, events);
                    admitted = true;
                }
                Ok(FetchCommand::Release { pin_token }) => {
                    // Dropping the guards unpins the pages; an unknown
                    // token (already released, or never issued) is a
                    // harmless no-op.
                    self.release_pin(pin_token);
                    self.waker.wake_by_ref();
                    admitted = true;
                }
                Ok(FetchCommand::Sending { pin_token }) => {
                    self.mark_sending(pin_token);
                    admitted = true;
                }
                Ok(FetchCommand::Cancel { fetch_id }) => {
                    self.cancel_fetch(fetch_id);
                    admitted = true;
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    self.disconnected = true;
                    break;
                }
            }
        }
        admitted
    }

    fn spawn_fetch(
        &mut self,
        fetch_id: u64,
        req: StripeReq,
        intra_offset: u64,
        intra_len: u64,
        events: Arc<EventSlot>,
    ) {
        match FetchTask::new(
            fetch_id,
            self.pool.clone(),
            &req,
            intra_offset,
            intra_len,
            self.page_size,
        ) {
            Ok(task) if task.is_done() => events.push(Ok(FetchEvent::Done)),
            Ok(task) => self.in_flight.push(Inflight { events, task }),
            Err(e) => events.push(Err(e)),
        }
    }

    fn poll_in_flight(&mut self, cx: &mut Context<'_>) -> bool {
        let mut completed = false;
        let mut i = 0;
        while i < self.in_flight.len() {
            if self.in_flight[i].task.emitted_pins >= MAX_EMITTED_PINS_PER_FETCH {
                i += 1;
                continue;
            }
            let allow_speculative =
                self.in_flight[i].task.emitted_pins + 1 < MAX_EMITTED_PINS_PER_FETCH;
            match self.in_flight[i].task.poll_next(cx, allow_speculative) {
                Poll::Ready(Ok(FetchTaskEvent::Page {
                    ordinal,
                    guard,
                    loc,
                })) => {
                    let pin_token = self.insert_pin(self.in_flight[i].task.fetch_id, guard);
                    self.in_flight[i].task.emitted_pins += 1;
                    self.in_flight[i]
                        .events
                        .push(Ok(FetchEvent::Page(FetchPage {
                            ordinal,
                            pin_token,
                            loc,
                        })));
                    if self.in_flight[i].task.is_done() {
                        let done = self.in_flight.swap_remove(i);
                        done.events.push(Ok(FetchEvent::Done));
                    } else {
                        i += 1;
                    }
                    completed = true;
                }
                Poll::Ready(Ok(FetchTaskEvent::Done)) => {
                    let done = self.in_flight.swap_remove(i);
                    done.events.push(Ok(FetchEvent::Done));
                    completed = true;
                }
                Poll::Ready(Err(e)) => {
                    let done = self.in_flight.swap_remove(i);
                    done.events.push(Err(e));
                    done.events.push(Ok(FetchEvent::Done));
                    completed = true;
                }
                Poll::Pending => i += 1,
            }
        }
        completed
    }

    fn insert_pin(&mut self, fetch_id: u64, guard: PageGuard<'static>) -> u64 {
        let pin_token = self.next_token;
        self.next_token = self.next_token.wrapping_add(1);
        self.pins.insert(
            pin_token,
            PinEntry {
                fetch_id,
                sending: false,
                guard,
            },
        );
        self.fetch_pins
            .entry(fetch_id)
            .or_default()
            .insert(pin_token);
        pin_token
    }

    fn release_pin(&mut self, pin_token: u64) {
        let Some(pin) = self.pins.remove(&pin_token) else {
            return;
        };
        if let Some(tokens) = self.fetch_pins.get_mut(&pin.fetch_id) {
            tokens.remove(&pin_token);
            if tokens.is_empty() {
                self.fetch_pins.remove(&pin.fetch_id);
            }
        }
        if let Some(inflight) = self
            .in_flight
            .iter_mut()
            .find(|inflight| inflight.task.fetch_id == pin.fetch_id)
        {
            inflight.task.emitted_pins = inflight.task.emitted_pins.saturating_sub(1);
        }
    }

    fn mark_sending(&mut self, pin_token: u64) {
        if let Some(pin) = self.pins.get_mut(&pin_token) {
            pin.sending = true;
        }
    }

    fn release_fetch_pins(&mut self, fetch_id: u64) {
        let Some(tokens) = self.fetch_pins.remove(&fetch_id) else {
            return;
        };
        let mut retained = HashSet::new();
        for token in tokens {
            let keep = self
                .pins
                .get(&token)
                .is_some_and(|pin| pin.fetch_id == fetch_id && pin.sending);
            if keep {
                retained.insert(token);
            } else {
                self.pins.remove(&token);
            }
        }
        if !retained.is_empty() {
            self.fetch_pins.insert(fetch_id, retained);
        }
    }

    fn cancel_fetch(&mut self, fetch_id: u64) {
        self.in_flight
            .retain(|inflight| inflight.task.fetch_id != fetch_id);
        self.release_fetch_pins(fetch_id);
    }
}

struct FetchTask {
    fetch_id: u64,
    pending: Vec<Option<PendingPage>>,
    stream: ReadStream<'static>,
    page_size: u64,
    emitted: usize,
    emitted_pins: usize,
    page_count: usize,
}

struct PendingPage {
    ordinal: usize,
    page_no: u64,
    fut: Option<OwnedPageFuture>,
    backed_off: bool,
}

enum FetchTaskEvent {
    Page {
        ordinal: usize,
        guard: PageGuard<'static>,
        loc: PageLoc,
    },
    Done,
}

impl FetchTask {
    fn new<T, S>(
        fetch_id: u64,
        pool: Rc<Pool<T, S, StripeReq>>,
        req: &StripeReq,
        intra_offset: u64,
        intra_len: u64,
        page_size: u64,
    ) -> Result<Self, Error>
    where
        T: Transport<StripeReq> + 'static,
        S: BlockStore + 'static,
    {
        let stream = pool.read_owned(req, intra_offset, intra_len)?;
        let page_range = stream.owned_page_range();
        let page_count = usize::try_from(page_range.end.saturating_sub(page_range.start))
            .map_err(|_| Error::OffsetOutOfRange)?;
        let mut pending = Vec::with_capacity(page_count);

        for (ordinal, page_no) in page_range.enumerate() {
            pending.push(Some(PendingPage {
                ordinal,
                page_no,
                fut: None,
                backed_off: false,
            }));
        }

        Ok(Self {
            fetch_id,
            stream,
            page_size,
            pending,
            emitted: 0,
            emitted_pins: 0,
            page_count,
        })
    }

    fn is_done(&self) -> bool {
        self.emitted == self.page_count
    }

    fn poll_next(
        &mut self,
        cx: &mut Context<'_>,
        allow_speculative: bool,
    ) -> Poll<Result<FetchTaskEvent, Error>> {
        if self.is_done() {
            return Poll::Ready(Ok(FetchTaskEvent::Done));
        }

        let Some(first_pending) = self.pending.iter().position(|slot| slot.is_some()) else {
            return Poll::Ready(Ok(FetchTaskEvent::Done));
        };

        if let Some(event) = self.try_emit_ready(first_pending)? {
            return Poll::Ready(Ok(event));
        }

        let slot = &mut self.pending[first_pending];
        let pending = slot.as_mut().expect("slot selected as pending");
        if pending.backed_off {
            pending.backed_off = false;
            return Poll::Pending;
        }
        if pending.fut.is_none() {
            pending.fut = Some(
                self.stream
                    .page_owned_future_at(pending.page_no)
                    .expect("pending page remains in stream range"),
            );
        }

        let fut = pending.fut.as_mut().expect("future initialized above");
        match Pin::new(fut).poll(cx) {
            Poll::Pending => {
                if allow_speculative {
                    for i in first_pending + 1..self.pending.len() {
                        if let Some(event) = self.try_emit_ready(i)? {
                            return Poll::Ready(Ok(event));
                        }
                    }
                }
            }
            Poll::Ready(Ok(page)) => {
                let ordinal = pending.ordinal;
                let pr = page.page_ref();
                let loc = page_loc(pr, self.page_size);
                *slot = None;
                self.emitted += 1;
                return Poll::Ready(Ok(FetchTaskEvent::Page {
                    ordinal,
                    guard: page,
                    loc,
                }));
            }
            Poll::Ready(Err(Error::Busy)) => {
                pending.fut = None;
                pending.backed_off = true;
            }
            Poll::Ready(Err(e)) => return Poll::Ready(Err(e)),
        }

        Poll::Pending
    }

    fn try_emit_ready(&mut self, index: usize) -> Result<Option<FetchTaskEvent>, Error> {
        let Some(pending) = self.pending[index].as_ref() else {
            return Ok(None);
        };
        let Some(page) = self.stream.try_ready_page_owned_at(pending.page_no) else {
            return Ok(None);
        };
        let ordinal = pending.ordinal;
        match page {
            Ok(page) => {
                let pr = page.page_ref();
                let loc = page_loc(pr, self.page_size);
                self.pending[index] = None;
                self.emitted += 1;
                Ok(Some(FetchTaskEvent::Page {
                    ordinal,
                    guard: page,
                    loc,
                }))
            }
            Err(Error::Busy) => Ok(None),
            Err(e) => Err(e),
        }
    }
}

impl Drop for FetchTask {
    fn drop(&mut self) {
        self.pending.clear();
    }
}

fn page_loc(pr: crate::bufferpool::PageRef, page_size: u64) -> PageLoc {
    PageLoc {
        page_byte_offset: pr.page_idx as u64 * page_size + pr.offset as u64,
        len: pr.len,
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
        self.command_waker.lock().unwrap().take();
    }
}
