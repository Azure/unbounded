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
//! The owner replies with the in-backing byte locations of those
//! pages; the coordinator then issues zero-copy `SEND_ZC` directly
//! from the owner's backing and, once the NIC has consumed the data,
//! tells the owner to release the pin.
//!
//! This module is the Send-capable channel that carries those requests
//! across the per-core executors. It mirrors the cross-core pattern in
//! [`crate::storage::page_channel`] but carries its own message and
//! reply shapes and, crucially, never copies page bytes: the reply is
//! only a description of where the pinned pages live.

use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
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

/// Successful result of a [`FetchCommand::Fetch`].
///
/// The owner has fetched and pinned every page covering the requested
/// stripe slice and will keep them pinned until it receives a matching
/// [`FetchCommand::Release`] carrying `pin_token`. `pages` lists those
/// pages in ascending stripe order; the coordinator must `SEND_ZC`
/// them in exactly that order to preserve byte-stream ordering.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FetchReply {
    pub pin_token: u64,
    pub pages: Vec<PageLoc>,
}

/// Commands carried from a coordinator shard to an owner shard.
pub enum FetchCommand {
    /// Fetch and pin the pages covering `[intra_offset, intra_offset +
    /// intra_len)` of the stripe named by `req`. The owner derives
    /// nothing: `req` already carries the content-addressed key (and
    /// the origin mapping needed only on a full miss). On success the
    /// owner sets `reply` to a [`FetchReply`] describing the pinned
    /// pages; on failure it sets the slot's `Err`.
    Fetch {
        req: StripeReq,
        intra_offset: u64,
        intra_len: u64,
        reply: Arc<ReplySlot<FetchReply>>,
    },
    /// Release the pin established by a prior `Fetch` whose reply
    /// carried `pin_token`. Fire-and-forget: the coordinator issues
    /// this only after the NIC has signalled `SEND_ZC` completion for
    /// every page of the pin, so the owner may drop the page guards
    /// immediately. Unknown tokens are ignored.
    Release { pin_token: u64 },
}

/// Send half of a cross-shard fetch channel. Cloneable and `Send`, so
/// every coordinator shard can hold a handle to every owner shard.
pub struct FetchChannel {
    cmd_tx: Sender<FetchCommand>,
    service_alive: Arc<AtomicBool>,
}

impl Clone for FetchChannel {
    fn clone(&self) -> Self {
        Self {
            cmd_tx: self.cmd_tx.clone(),
            service_alive: self.service_alive.clone(),
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
        (
            FetchChannel {
                cmd_tx,
                service_alive: service_alive.clone(),
            },
            FetchChannelReceiver { rx, service_alive },
        )
    }

    /// Stable identity of the owning service, usable as a map key so a
    /// coordinator can detect when an owner is itself (and take the
    /// local fast path) or dedup handles.
    pub fn service_id(&self) -> usize {
        Arc::as_ptr(&self.service_alive) as usize
    }

    /// Request the owner fetch and pin the pages covering the given
    /// slice of `req`'s stripe. Resolves with the pinned-page locations
    /// or an error (including `EIO` if the owner service has died).
    pub async fn fetch(
        &self,
        req: StripeReq,
        intra_offset: u64,
        intra_len: u64,
    ) -> Result<FetchReply, Error> {
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(FetchCommand::Fetch {
                req,
                intra_offset,
                intra_len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;

        AliveAwareWait::new(reply, self.service_alive.clone()).await
    }

    /// Release a pin previously established by [`fetch`](Self::fetch).
    /// Fire-and-forget; a dead service or full disconnect is ignored
    /// because the owner dropping the channel already frees every pin.
    pub fn release(&self, pin_token: u64) {
        let _ = self.cmd_tx.send(FetchCommand::Release { pin_token });
    }
}

/// Receive half of a cross-shard fetch channel. Not `Clone`: a single
/// owner-side service drains it.
pub struct FetchChannelReceiver {
    rx: Receiver<FetchCommand>,
    service_alive: Arc<AtomicBool>,
}

impl FetchChannelReceiver {
    /// Hand the raw receiver and liveness flag to the owner-side
    /// service. This is the *only* way the flag's "alive" state is
    /// preserved past the receiver's lifetime: ownership of
    /// `service_alive` transfers to the caller (a [`FetchService`]),
    /// which flips it to false on its own `Drop`. Self is forgotten so
    /// the receiver's own `Drop` (which flips the flag) does not run.
    pub(crate) fn into_parts(self) -> (Receiver<FetchCommand>, Arc<AtomicBool>) {
        let this = std::mem::ManuallyDrop::new(self);
        // SAFETY: each field is read exactly once out of a
        // `ManuallyDrop`, so there is no double-drop, and `this` is
        // never dropped, so the flag-flipping `Drop` is intentionally
        // skipped (the service now owns that transition).
        let rx = unsafe { std::ptr::read(&this.rx) };
        let service_alive = unsafe { std::ptr::read(&this.service_alive) };
        (rx, service_alive)
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

/// One-shot completion slot generic over the success type. The owner
/// service stores a result and wakes any parked coordinator; the
/// coordinator polls and returns the stored result if present.
///
/// This mirrors the slot in [`crate::storage::page_channel`], which is
/// not reusable here because its wait machinery is private to that
/// module.
pub struct ReplySlot<T> {
    inner: Mutex<ReplyInner<T>>,
}

struct ReplyInner<T> {
    result: Option<Result<T, Error>>,
    waker: Option<Waker>,
}

impl<T> ReplySlot<T> {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(ReplyInner {
                result: None,
                waker: None,
            }),
        })
    }

    /// Store the result and wake the parked waiter. The waker is woken
    /// outside the lock. This MUST wake any stored waker, otherwise the
    /// coordinator shard only re-checks on its fallback park interval.
    pub fn set(&self, result: Result<T, Error>) {
        let waker = {
            let mut g = self.inner.lock().unwrap();
            g.result = Some(result);
            g.waker.take()
        };

        if let Some(w) = waker {
            w.wake();
        }
    }

    fn wait(self: Arc<Self>) -> ReplyWait<T> {
        ReplyWait { inner: self }
    }
}

struct ReplyWait<T> {
    inner: Arc<ReplySlot<T>>,
}

impl<T> Future for ReplyWait<T> {
    type Output = Result<T, Error>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut g = self.inner.inner.lock().unwrap();
        if let Some(r) = g.result.take() {
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
struct AliveAwareWait<T> {
    reply: ReplyWait<T>,
    service_alive: Arc<AtomicBool>,
}

impl<T> AliveAwareWait<T> {
    fn new(reply: Arc<ReplySlot<T>>, service_alive: Arc<AtomicBool>) -> Self {
        Self {
            reply: ReplyWait { inner: reply },
            service_alive,
        }
    }
}

impl<T> Future for AliveAwareWait<T> {
    type Output = Result<T, Error>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // SAFETY: `ReplyWait` holds only an `Arc` and is trivially
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
    // reply slot actually wakes a stored waker.
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
        static VTABLE: RawWakerVTable =
            RawWakerVTable::new(clone, wake, wake_by_ref, drop_fn);
        let raw = RawWaker::new(Arc::into_raw(flag) as *const (), &VTABLE);
        unsafe { Waker::from_raw(raw) }
    }

    #[test]
    fn fetch_round_trip_resolves_with_reply() {
        let (tx, rx) = FetchChannel::new();
        let (rx, _alive) = rx.into_parts();

        let req = StripeReq::new(crate::bufferpool::StripeKey([7u8; 32]));
        let mut fut = Box::pin(tx.fetch(req, 0, 4096));

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(fut.as_mut().poll(&mut cx).is_pending());

        // The owner drains the command and answers.
        match rx.try_recv().expect("command queued") {
            FetchCommand::Fetch {
                intra_offset,
                intra_len,
                reply,
                ..
            } => {
                assert_eq!(intra_offset, 0);
                assert_eq!(intra_len, 4096);
                reply.set(Ok(FetchReply {
                    pin_token: 42,
                    pages: vec![PageLoc {
                        page_byte_offset: 8192,
                        len: 4096,
                    }],
                }));
            }
            _ => panic!("expected Fetch"),
        }

        let got = block_on(fut);
        let reply = got.expect("ok reply");
        assert_eq!(reply.pin_token, 42);
        assert_eq!(reply.pages.len(), 1);
        assert_eq!(reply.pages[0].page_byte_offset, 8192);
        assert_eq!(reply.pages[0].len, 4096);
    }

    #[test]
    fn set_wakes_stored_waker() {
        let slot = ReplySlot::<FetchReply>::new();
        let flag = Arc::new(AtomicBool::new(false));
        let waker = flag_waker(flag.clone());
        let mut cx = Context::from_waker(&waker);

        let mut wait = slot.clone().wait();
        // SAFETY: `wait` is pinned to this stack frame and not moved.
        let pinned = unsafe { Pin::new_unchecked(&mut wait) };
        assert!(pinned.poll(&mut cx).is_pending());
        assert!(!flag.load(Ordering::SeqCst));

        slot.set(Ok(FetchReply {
            pin_token: 1,
            pages: Vec::new(),
        }));
        assert!(flag.load(Ordering::SeqCst), "set must wake stored waker");
    }

    #[test]
    fn fetch_after_service_death_resolves_eio() {
        let (tx, rx) = FetchChannel::new();
        let (_rx, alive) = rx.into_parts();

        let req = StripeReq::new(crate::bufferpool::StripeKey([0u8; 32]));
        let mut fut = Box::pin(tx.fetch(req, 0, 1));

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(fut.as_mut().poll(&mut cx).is_pending());

        // Owner dies without answering: the wait must resolve EIO.
        alive.store(false, Ordering::Release);
        match block_on(fut) {
            Err(Error::Io(e)) => assert_eq!(e, libc::EIO),
            other => panic!("expected EIO, got {other:?}"),
        }
    }

    #[test]
    fn dropping_receiver_before_service_resolves_eio() {
        let (tx, rx) = FetchChannel::new();

        let req = StripeReq::new(crate::bufferpool::StripeKey([3u8; 32]));
        let mut fut = Box::pin(tx.fetch(req, 0, 1));

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
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
        let (rx, _alive) = rx.into_parts();
        tx.release(99);
        match rx.try_recv().expect("release queued") {
            FetchCommand::Release { pin_token } => assert_eq!(pin_token, 99),
            _ => panic!("expected Release"),
        }
    }
}
