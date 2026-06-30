// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-shard stripe-fetch channel.
//!
//! A single inbound TCP connection is accepted and served by exactly
//! one shard (the SO_REUSEPORT 4-tuple hash pins it to one core). To
//! keep that coordinator core from serializing every stripe's work
//! (key derivation, pool orchestration, page fetch), the coordinator
//! routes each stripe to the shard that owns its content-addressed
//! [`StripeKey`](crate::bufferpool::StripeKey) and asks that owner to
//! fetch and *pin* the stripe's pages in its own registered backing.
//! The owner emits the in-backing byte location of each page as it is
//! pinned; the coordinator then issues zero-copy `SEND_ZC` directly from
//! the owner's backing and, once the NIC has consumed that page, tells
//! the owner to release the pin.
//!
//! This module is the Send-capable channel that carries those requests
//! across the per-core executors. It mirrors the cross-core pattern in
//! [`crate::storage::page_channel`] but carries its own message and
//! event shapes and, crucially, never copies page bytes: page events are
//! only descriptions of where the pinned pages live.

use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::mpsc::{Receiver, Sender, channel};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use crate::bufferpool::Error;
use crate::storage::StripeReq;

/// Location of one pinned page within the owner shard's registered
/// backing buffer. `page_byte_offset` is the absolute offset of the
/// page's data from the base of that backing (i.e.
/// `page_idx * page_size + intra_page_offset`); `len` is the number of
/// valid bytes at that offset that belong to the requested slice.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct PageLoc {
    pub page_byte_offset: u64,
    pub len: u32,
}

/// One page pinned by the owner for a streaming fetch.
///
/// `ordinal` is the page's position within the requested stripe slice,
/// not the absolute page number. The owner keeps the page pinned until
/// it receives a matching [`FetchCommand::Release`] carrying
/// `pin_token`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FetchPage {
    pub ordinal: usize,
    pub pin_token: u64,
    pub loc: PageLoc,
}

/// Event stream produced by a [`FetchCommand::Fetch`]. Page events may
/// arrive in any ordinal order; the coordinator buffers them and sends
/// only the contiguous prefix needed to preserve HTTP byte ordering.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FetchEvent {
    Page(FetchPage),
    Done,
}

/// Commands carried from a coordinator shard to an owner shard.
pub enum FetchCommand {
    /// Fetch and pin the pages covering `[intra_offset, intra_offset +
    /// intra_len)` of the stripe named by `req`. The owner derives
    /// nothing: `req` already carries the content-addressed key (and
    /// the origin mapping needed only on a full miss). On success the
    /// owner pushes [`FetchEvent::Page`] events as pages become ready
    /// and a final [`FetchEvent::Done`] once every page has been
    /// emitted. On failure it pushes `Err`.
    Fetch {
        fetch_id: u64,
        req: StripeReq,
        intra_offset: u64,
        intra_len: u64,
        events: Arc<EventSlot>,
    },
    /// Release one page pin established by a prior [`FetchEvent::Page`].
    /// Fire-and-forget: the coordinator issues this only after the NIC
    /// has signalled `SEND_ZC` completion for that page, so the owner
    /// may drop the page guard immediately. Unknown tokens are ignored.
    Release { pin_token: u64 },
    /// Mark an emitted page pin as handed to a zero-copy send. A later
    /// fetch cancel must not drop this pin; only [`Release`] may.
    Sending { pin_token: u64 },
    /// Drop any in-flight work and emitted pins for a fetch whose
    /// coordinator went away before consuming/sending every page. Pins
    /// already marked [`Sending`](Self::Sending) are retained until a
    /// matching [`Release`](Self::Release) arrives.
    Cancel { fetch_id: u64 },
}

/// Send half of a cross-shard fetch channel. Cloneable and `Send`, so
/// every coordinator shard can hold a handle to every owner shard.
pub struct FetchChannel {
    cmd_tx: Sender<FetchCommand>,
    service_alive: Arc<AtomicBool>,
    next_fetch_id: Arc<AtomicU64>,
    command_waker: Arc<Mutex<Option<Waker>>>,
}

impl Clone for FetchChannel {
    fn clone(&self) -> Self {
        Self {
            cmd_tx: self.cmd_tx.clone(),
            service_alive: self.service_alive.clone(),
            next_fetch_id: self.next_fetch_id.clone(),
            command_waker: self.command_waker.clone(),
        }
    }
}

impl FetchChannel {
    /// Build a connected `(sender, receiver)` pair. The receiver is
    /// consumed by exactly one owner-side service; the sender may be
    /// cloned freely across coordinator shards.
    pub fn new() -> (FetchChannel, FetchChannelReceiver) {
        let (cmd_tx, rx) = channel();
        let service_alive = Arc::new(AtomicBool::new(true));
        let next_fetch_id = Arc::new(AtomicU64::new(0));
        let command_waker = Arc::new(Mutex::new(None));
        (
            FetchChannel {
                cmd_tx,
                service_alive: service_alive.clone(),
                next_fetch_id,
                command_waker: command_waker.clone(),
            },
            FetchChannelReceiver {
                rx,
                service_alive,
                command_waker,
            },
        )
    }

    /// Stable identity of the owning service, usable as a map key so a
    /// coordinator can detect when an owner is itself (and take the
    /// local fast path) or dedup handles.
    pub fn service_id(&self) -> usize {
        Arc::as_ptr(&self.service_alive) as usize
    }

    /// Request the owner fetch and pin the pages covering the given
    /// slice of `req`'s stripe. Returns an event stream that yields
    /// pinned pages as soon as the owner has them ready, or an error
    /// (including `EIO` if the owner service has died).
    pub fn fetch(
        &self,
        req: StripeReq,
        intra_offset: u64,
        intra_len: u64,
    ) -> Result<FetchStream, Error> {
        let fetch_id = self.next_fetch_id.fetch_add(1, Ordering::Relaxed);
        let events = EventSlot::new();
        self.send_command(FetchCommand::Fetch {
            fetch_id,
            req,
            intra_offset,
            intra_len,
            events: events.clone(),
        })?;

        Ok(FetchStream {
            fetch_id,
            events,
            cmd_tx: self.cmd_tx.clone(),
            service_alive: self.service_alive.clone(),
            command_waker: self.command_waker.clone(),
            done: false,
        })
    }

    /// Release a pin previously established by [`fetch`](Self::fetch).
    /// Fire-and-forget; a dead service or full disconnect is ignored
    /// because the owner dropping the channel already frees every pin.
    pub fn release(&self, pin_token: u64) {
        let _ = self.send_command(FetchCommand::Release { pin_token });
    }

    /// Mark a pin as in use by a zero-copy send.
    pub fn sending(&self, pin_token: u64) {
        let _ = self.send_command(FetchCommand::Sending { pin_token });
    }

    fn send_command(&self, command: FetchCommand) -> Result<(), Error> {
        self.cmd_tx
            .send(command)
            .map_err(|_| Error::Io(libc::EPIPE))?;
        wake_command_service(&self.command_waker);
        Ok(())
    }
}

/// Coordinator-side stream of owner page completions for one fetch.
pub struct FetchStream {
    fetch_id: u64,
    events: Arc<EventSlot>,
    cmd_tx: Sender<FetchCommand>,
    service_alive: Arc<AtomicBool>,
    command_waker: Arc<Mutex<Option<Waker>>>,
    done: bool,
}

/// Deferred release for a page pin handed to one or more SEND_ZC ops.
///
/// The hold is closed when the coordinator is done issuing sends for the
/// page. Each send owns a token until its final SEND_ZC notification is
/// reaped. The owner pin is released exactly once after both are true.
pub struct PinReleaseHold {
    inner: Arc<PinReleaseInner>,
}

pub struct PinReleaseToken {
    inner: Arc<PinReleaseInner>,
}

struct PinReleaseInner {
    cmd_tx: Sender<FetchCommand>,
    command_waker: Arc<Mutex<Option<Waker>>>,
    pin_token: u64,
    in_flight: AtomicUsize,
    closed: AtomicBool,
    released: AtomicBool,
}

impl PinReleaseHold {
    fn new(
        cmd_tx: Sender<FetchCommand>,
        command_waker: Arc<Mutex<Option<Waker>>>,
        pin_token: u64,
    ) -> Self {
        Self {
            inner: Arc::new(PinReleaseInner {
                cmd_tx,
                command_waker,
                pin_token,
                in_flight: AtomicUsize::new(0),
                closed: AtomicBool::new(false),
                released: AtomicBool::new(false),
            }),
        }
    }

    pub fn token(&self) -> PinReleaseToken {
        self.inner.in_flight.fetch_add(1, Ordering::AcqRel);
        PinReleaseToken {
            inner: self.inner.clone(),
        }
    }

    pub fn close(&self) {
        self.inner.closed.store(true, Ordering::Release);
        self.inner.maybe_release();
    }
}

impl Drop for PinReleaseHold {
    fn drop(&mut self) {
        self.close();
    }
}

impl Drop for PinReleaseToken {
    fn drop(&mut self) {
        self.inner.in_flight.fetch_sub(1, Ordering::AcqRel);
        self.inner.maybe_release();
    }
}

impl PinReleaseInner {
    fn maybe_release(&self) {
        if !self.closed.load(Ordering::Acquire) || self.in_flight.load(Ordering::Acquire) != 0 {
            return;
        }
        if self.released.swap(true, Ordering::AcqRel) {
            return;
        }
        if self
            .cmd_tx
            .send(FetchCommand::Release {
                pin_token: self.pin_token,
            })
            .is_ok()
        {
            wake_command_service(&self.command_waker);
        }
    }
}

impl FetchStream {
    /// Fetch id used by owner-side cancellation. Exposed for tests and
    /// diagnostics; callers normally rely on `Drop` to cancel.
    pub fn fetch_id(&self) -> u64 {
        self.fetch_id
    }

    /// Wait for the next page-ready event or terminal event.
    pub async fn next_event(&mut self) -> Result<FetchEvent, Error> {
        if self.done {
            return Ok(FetchEvent::Done);
        }

        let event = match AliveAwareWait::new(self.events.clone(), self.service_alive.clone()).await
        {
            Ok(event) => event,
            Err(e) => {
                self.done = true;
                return Err(e);
            }
        };
        if matches!(event, FetchEvent::Done) {
            self.done = true;
        }
        Ok(event)
    }

    /// Release a page pin after its zero-copy send has completed.
    pub fn release(&self, pin_token: u64) {
        if self
            .cmd_tx
            .send(FetchCommand::Release { pin_token })
            .is_ok()
        {
            wake_command_service(&self.command_waker);
        }
    }

    /// Mark a pin as in use by a zero-copy send. After this, dropping
    /// the stream will not cancel that pin; the coordinator must still
    /// send a matching [`release`](Self::release) when the send completes.
    pub fn sending(&self, pin_token: u64) {
        if self
            .cmd_tx
            .send(FetchCommand::Sending { pin_token })
            .is_ok()
        {
            wake_command_service(&self.command_waker);
        }
    }

    /// Mark a pin as owned by zero-copy sends and return a deferred
    /// release guard. Tokens cloned from the guard are attached to each
    /// SEND_ZC op; dropping the stream will not cancel this pin, and the
    /// owner sees `Release` only after the guard is closed and every token
    /// has reached its final notification.
    pub fn pin_release_hold(&self, pin_token: u64) -> PinReleaseHold {
        self.sending(pin_token);
        PinReleaseHold::new(self.cmd_tx.clone(), self.command_waker.clone(), pin_token)
    }

    /// Explicitly cancel this fetch and release any owner-side pins that
    /// have not already been handed to zero-copy send ownership.
    pub fn cancel(&self) {
        if self
            .cmd_tx
            .send(FetchCommand::Cancel {
                fetch_id: self.fetch_id,
            })
            .is_ok()
        {
            wake_command_service(&self.command_waker);
        }
    }
}

impl Drop for FetchStream {
    fn drop(&mut self) {
        self.cancel();
    }
}

/// Receive half of a cross-shard fetch channel. Not `Clone`: a single
/// owner-side service drains it.
pub struct FetchChannelReceiver {
    rx: Receiver<FetchCommand>,
    service_alive: Arc<AtomicBool>,
    command_waker: Arc<Mutex<Option<Waker>>>,
}

impl FetchChannelReceiver {
    /// Hand the raw receiver and liveness flag to the owner-side
    /// service. This is the *only* way the flag's "alive" state is
    /// preserved past the receiver's lifetime: ownership of
    /// `service_alive` transfers to the caller (a [`FetchService`]),
    /// which flips it to false on its own `Drop`. Self is forgotten so
    /// the receiver's own `Drop` (which flips the flag) does not run.
    pub(crate) fn into_parts(
        self,
    ) -> (
        Receiver<FetchCommand>,
        Arc<AtomicBool>,
        Arc<Mutex<Option<Waker>>>,
    ) {
        let this = std::mem::ManuallyDrop::new(self);
        // SAFETY: each field is read exactly once out of a
        // `ManuallyDrop`, so there is no double-drop, and `this` is
        // never dropped, so the flag-flipping `Drop` is intentionally
        // skipped (the service now owns that transition).
        let rx = unsafe { std::ptr::read(&this.rx) };
        let service_alive = unsafe { std::ptr::read(&this.service_alive) };
        let command_waker = unsafe { std::ptr::read(&this.command_waker) };
        (rx, service_alive, command_waker)
    }
}

fn wake_command_service(command_waker: &Arc<Mutex<Option<Waker>>>) {
    let waker = command_waker.lock().unwrap().as_ref().map(Waker::clone);
    if let Some(w) = waker {
        w.wake();
    }
}

impl Drop for FetchChannelReceiver {
    /// If the receiver is dropped before it is ever turned into a
    /// running [`FetchService`] (for example, the owner shard publishes
    /// its channel in bring-up Phase A but then fails Phase B before the
    /// service is constructed), flip the liveness flag so any
    /// coordinator already parked in [`FetchChannel::fetch`] resolves
    /// with `EIO` instead of hanging on a service that will never run.
    /// The success path moves the fields out via [`into_parts`] and
    /// forgets self, so this never runs for a live service;
    /// [`FetchService::drop`](super::FetchService) owns that transition.
    fn drop(&mut self) {
        self.service_alive.store(false, Ordering::Release);
    }
}

/// FIFO event queue for one fetch stream. The owner service pushes page
/// events and a terminal event; the coordinator drains them one at a
/// time and parks on the stored waker when the queue is empty.
pub struct EventSlot {
    inner: Mutex<EventInner>,
}

struct EventInner {
    events: VecDeque<Result<FetchEvent, Error>>,
    waker: Option<Waker>,
}

impl EventSlot {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(EventInner {
                events: VecDeque::new(),
                waker: None,
            }),
        })
    }

    /// Store an event and wake the parked waiter. The waker is woken
    /// outside the lock. This MUST wake any stored waker, otherwise the
    /// coordinator shard only re-checks on its fallback park interval.
    pub fn push(&self, event: Result<FetchEvent, Error>) {
        let waker = {
            let mut g = self.inner.lock().unwrap();
            g.events.push_back(event);
            g.waker.take()
        };

        if let Some(w) = waker {
            w.wake();
        }
    }
}

struct EventWait {
    inner: Arc<EventSlot>,
}

impl Future for EventWait {
    type Output = Result<FetchEvent, Error>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut g = self.inner.inner.lock().unwrap();
        if let Some(r) = g.events.pop_front() {
            Poll::Ready(r)
        } else {
            if !g.waker.as_ref().is_some_and(|w| w.will_wake(cx.waker())) {
                g.waker = Some(cx.waker().clone());
            }

            Poll::Pending
        }
    }
}

/// `Future` adapter for the coordinator's `fetch` path. Resolves with
/// `Err(Io(EIO))` if the slot is still pending once the owner service
/// flips `service_alive` to false; the extra poll absorbs the race
/// where the service set the slot and then flipped the flag.
struct AliveAwareWait {
    reply: EventWait,
    service_alive: Arc<AtomicBool>,
}

impl AliveAwareWait {
    fn new(reply: Arc<EventSlot>, service_alive: Arc<AtomicBool>) -> Self {
        Self {
            reply: EventWait { inner: reply },
            service_alive,
        }
    }
}

impl Future for AliveAwareWait {
    type Output = Result<FetchEvent, Error>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // SAFETY: `EventWait` holds only an `Arc` and is trivially
        // `Unpin`; we never move out of the projected field and never
        // hand `&mut self` to user code.
        let this = unsafe { self.get_unchecked_mut() };
        match Pin::new(&mut this.reply).poll(cx) {
            Poll::Ready(v) => Poll::Ready(v),
            Poll::Pending => {
                if !this.service_alive.load(Ordering::Acquire) {
                    return match Pin::new(&mut this.reply).poll(cx) {
                        Poll::Ready(v) => Poll::Ready(v),
                        Poll::Pending => Poll::Ready(Err(Error::Io(libc::EIO))),
                    };
                }

                Poll::Pending
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::AtomicBool;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    use super::*;
    use crate::runtime::noop_waker;

    fn block_on<F: Future>(mut fut: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        // SAFETY: `fut` lives on this stack frame for the whole loop
        // and is never moved.
        let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
        let mut spins = 0u32;
        loop {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }

            spins += 1;
            assert!(spins < 1_000_000, "block_on made no progress");
        }
    }

    // A waker that flips an AtomicBool when woken, so we can assert the
    // event slot actually wakes a stored waker.
    fn flag_waker(flag: Arc<AtomicBool>) -> Waker {
        fn clone(p: *const ()) -> RawWaker {
            let arc = unsafe { Arc::from_raw(p as *const AtomicBool) };
            let cloned = arc.clone();
            std::mem::forget(arc);
            RawWaker::new(Arc::into_raw(cloned) as *const (), &VTABLE)
        }
        fn wake(p: *const ()) {
            let arc = unsafe { Arc::from_raw(p as *const AtomicBool) };
            arc.store(true, Ordering::SeqCst);
        }
        fn wake_by_ref(p: *const ()) {
            let arc = unsafe { Arc::from_raw(p as *const AtomicBool) };
            arc.store(true, Ordering::SeqCst);
            std::mem::forget(arc);
        }
        fn drop_fn(p: *const ()) {
            unsafe { drop(Arc::from_raw(p as *const AtomicBool)) };
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, wake, wake_by_ref, drop_fn);
        let raw = RawWaker::new(Arc::into_raw(flag) as *const (), &VTABLE);
        unsafe { Waker::from_raw(raw) }
    }

    #[test]
    fn fetch_round_trip_yields_page_events() {
        let (tx, rx) = FetchChannel::new();
        let (rx, _alive, _command_waker) = rx.into_parts();

        let req = StripeReq::new(crate::bufferpool::StripeKey([7u8; 32]));
        let mut stream = tx.fetch(req, 0, 4096).expect("fetch stream");

        // The owner drains the command and answers.
        match rx.try_recv().expect("command queued") {
            FetchCommand::Fetch {
                intra_offset,
                intra_len,
                events,
                ..
            } => {
                assert_eq!(intra_offset, 0);
                assert_eq!(intra_len, 4096);
                events.push(Ok(FetchEvent::Page(FetchPage {
                    ordinal: 0,
                    pin_token: 42,
                    loc: PageLoc {
                        page_byte_offset: 8192,
                        len: 4096,
                    },
                })));
                events.push(Ok(FetchEvent::Done));
            }
            _ => panic!("expected Fetch"),
        }

        let event = block_on(stream.next_event()).expect("page event");
        match event {
            FetchEvent::Page(page) => {
                assert_eq!(page.ordinal, 0);
                assert_eq!(page.pin_token, 42);
                assert_eq!(page.loc.page_byte_offset, 8192);
                assert_eq!(page.loc.len, 4096);
            }
            FetchEvent::Done => panic!("expected page"),
        }
        assert_eq!(
            block_on(stream.next_event()).expect("done"),
            FetchEvent::Done
        );
    }

    #[test]
    fn push_wakes_stored_waker() {
        let slot = EventSlot::new();
        let flag = Arc::new(AtomicBool::new(false));
        let waker = flag_waker(flag.clone());
        let mut cx = Context::from_waker(&waker);

        let mut wait = EventWait {
            inner: slot.clone(),
        };
        // SAFETY: `wait` is pinned to this stack frame and not moved.
        let pinned = unsafe { Pin::new_unchecked(&mut wait) };
        assert!(pinned.poll(&mut cx).is_pending());
        assert!(!flag.load(Ordering::SeqCst));

        slot.push(Ok(FetchEvent::Done));
        assert!(flag.load(Ordering::SeqCst), "push must wake stored waker");
    }

    #[test]
    fn fetch_after_service_death_resolves_eio() {
        let (tx, rx) = FetchChannel::new();
        let (_rx, alive, _command_waker) = rx.into_parts();

        let req = StripeReq::new(crate::bufferpool::StripeKey([0u8; 32]));
        let mut stream = tx.fetch(req, 0, 1).expect("fetch stream");

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = Box::pin(stream.next_event());
        assert!(fut.as_mut().poll(&mut cx).is_pending());

        // Owner dies without answering: the wait must resolve EIO.
        alive.store(false, Ordering::Release);
        match block_on(fut) {
            Err(Error::Io(e)) => assert_eq!(e, libc::EIO),
            other => panic!("expected EIO, got {other:?}"),
        }
    }

    #[test]
    fn fetch_error_marks_stream_done() {
        let (tx, rx) = FetchChannel::new();
        let (rx, _alive, _command_waker) = rx.into_parts();

        let req = StripeReq::new(crate::bufferpool::StripeKey([4u8; 32]));
        let mut stream = tx.fetch(req, 0, 1).expect("fetch stream");

        match rx.try_recv().expect("command queued") {
            FetchCommand::Fetch { events, .. } => events.push(Err(Error::Io(libc::EIO))),
            _ => panic!("expected Fetch"),
        }

        match block_on(stream.next_event()) {
            Err(Error::Io(e)) => assert_eq!(e, libc::EIO),
            other => panic!("expected EIO, got {other:?}"),
        }
        assert_eq!(block_on(stream.next_event()).unwrap(), FetchEvent::Done);
    }

    #[test]
    fn dropping_receiver_before_service_resolves_eio() {
        let (tx, rx) = FetchChannel::new();

        let req = StripeReq::new(crate::bufferpool::StripeKey([3u8; 32]));
        let mut stream = tx.fetch(req, 0, 1).expect("fetch stream");

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = Box::pin(stream.next_event());
        assert!(fut.as_mut().poll(&mut cx).is_pending());

        // Owner fails bring-up after publishing its channel but before
        // constructing the service: the receiver is dropped without ever
        // being converted via `into_parts`, so its `Drop` must flip the
        // liveness flag and unblock the parked coordinator with EIO.
        drop(rx);
        match block_on(fut) {
            Err(Error::Io(e)) => assert_eq!(e, libc::EIO),
            other => panic!("expected EIO, got {other:?}"),
        }
    }

    #[test]
    fn release_is_fire_and_forget() {
        let (tx, rx) = FetchChannel::new();
        let (rx, _alive, _command_waker) = rx.into_parts();
        let req = StripeReq::new(crate::bufferpool::StripeKey([5u8; 32]));
        let stream = tx.fetch(req, 0, 1).expect("fetch stream");
        let _ = rx.try_recv().expect("fetch queued");
        stream.release(99);
        match rx.try_recv().expect("release queued") {
            FetchCommand::Release { pin_token } => assert_eq!(pin_token, 99),
            _ => panic!("expected Release"),
        }
    }

    #[test]
    fn sending_is_fire_and_forget() {
        let (tx, rx) = FetchChannel::new();
        let (rx, _alive, _command_waker) = rx.into_parts();
        let req = StripeReq::new(crate::bufferpool::StripeKey([6u8; 32]));
        let stream = tx.fetch(req, 0, 1).expect("fetch stream");
        let _ = rx.try_recv().expect("fetch queued");
        stream.sending(99);
        match rx.try_recv().expect("sending queued") {
            FetchCommand::Sending { pin_token } => assert_eq!(pin_token, 99),
            _ => panic!("expected Sending"),
        }
    }

    #[test]
    fn dropping_stream_queues_cancel() {
        let (tx, rx) = FetchChannel::new();
        let (rx, _alive, _command_waker) = rx.into_parts();
        let req = StripeReq::new(crate::bufferpool::StripeKey([9u8; 32]));
        let stream = tx.fetch(req, 0, 1).expect("fetch stream");
        let fetch_id = stream.fetch_id();
        let _ = rx.try_recv().expect("fetch queued");

        drop(stream);
        match rx.try_recv().expect("cancel queued") {
            FetchCommand::Cancel { fetch_id: got } => assert_eq!(got, fetch_id),
            _ => panic!("expected Cancel"),
        }
    }
}
