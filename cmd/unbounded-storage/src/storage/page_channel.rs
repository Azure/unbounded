// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-core, page-granularity bridge to a [`StorageEngine`] pinned
//! to a single "storage core" thread.
//!
//! A [`StorageEngine<CoreLocalDevice>`] can only be driven from the
//! storage core that built its [`StorageRing`]: the device resolves
//! the ring out of a thread-local registry and returns `ENXIO` off
//! that thread. Shard cores, however, need to read and write pages
//! through that engine. [`PageChannel`] closes that gap by shipping
//! each page operation - read, write, or buffer registration - over
//! an `mpsc` channel to the storage core, which runs a
//! [`PageService`] that executes the op against the engine and
//! completes the caller's [`ReplySlot`].
//!
//! The design mirrors the old `BlockDeviceProxy`, but the handoff is
//! at *page* granularity rather than [`BlockDevice`] granularity, and
//! the channel is concrete (it carries a raw pointer plus a
//! [`StripeKey`]/offset, not a generic device type). That lets the
//! disk-supervisor routing layer collapse to non-generic types while
//! the engine stays generic over its device.
//!
//! Buffer-lifetime contract: dropping a read/write future does not
//! cancel the in-flight op. The service-side future continues to run
//! and the kernel may still touch the caller's buffer, so callers
//! must keep the buffer alive until the matching [`ReplySlot`] is
//! set. This is the same contract callers already have with the
//! bufferpool [`Backing`]: I/O buffers live inside the backing for
//! the lifetime of the program.
//!
//! [`StorageEngine`]: crate::storage::StorageEngine
//! [`StorageRing`]: crate::ring::StorageRing
//! [`BlockDevice`]: crate::storage::blockdev::BlockDevice
//! [`StorageEngine<CoreLocalDevice>`]: crate::storage::StorageEngine
//! [`Backing`]: crate::memory::Backing

use std::future::Future;
use std::pin::Pin;
use std::ptr::NonNull;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Receiver, Sender, TryRecvError, channel};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use crate::bufferpool::{Error, StripeKey};
use crate::runtime::noop_waker;
use crate::storage::blockdev::BlockDevice;
use crate::storage::engine::StorageEngine;

/// `Send + Sync` clonable handle to a storage core's
/// [`PageService`]. Every clone targets the same service; when the
/// last handle is dropped the channel disconnects and the service
/// exits after draining in-flight work.
///
/// See the module docs for the buffer-lifetime contract.
pub struct PageChannel {
    cmd_tx: Sender<PageCommand>,
    /// Flipped to `false` by the service during shutdown so a send
    /// that raced the service's exit resolves with `EIO` instead of
    /// parking on a reply slot that nobody will set. Same mechanism
    /// the old proxy used.
    service_alive: Arc<AtomicBool>,
}

impl PageChannel {
    /// Build a channel and its single-consumer receiver. The
    /// receiver is consumed by a [`PageService`] on the storage
    /// core.
    pub fn new() -> (PageChannel, PageChannelReceiver) {
        let (tx, rx) = channel();
        let service_alive = Arc::new(AtomicBool::new(true));
        (
            PageChannel {
                cmd_tx: tx,
                service_alive: service_alive.clone(),
            },
            PageChannelReceiver { rx, service_alive },
        )
    }

    /// Read `(key, stripe_off)` into `dst`. Resolves `true` on a
    /// cache hit, `false` on a miss; `Err` only on transport
    /// failure (channel closed or service gone).
    ///
    /// SAFETY: `dst` must point to a writable region that lives
    /// until the returned future resolves and is pinned for DMA
    /// (the buffer-lifetime contract in the module docs).
    pub async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: *mut [u8],
    ) -> Result<bool, Error> {
        let len = dst.len();
        let ptr = NonNull::new(dst.cast::<u8>()).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(PageCommand::ReadPage {
                key,
                stripe_off,
                dst: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        AliveAwareWait::new(reply, self.service_alive.clone()).await
    }

    /// Write `src` to `(key, stripe_off)`.
    ///
    /// SAFETY: `src` must point to a readable region that lives
    /// until the returned future resolves and is pinned for DMA.
    pub async fn write_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        src: *const [u8],
    ) -> Result<(), Error> {
        let len = src.len();
        let ptr = NonNull::new(src.cast::<u8>().cast_mut()).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(PageCommand::WritePage {
                key,
                stripe_off,
                src: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        AliveAwareWait::new(reply, self.service_alive.clone()).await
    }

    /// Register a pinned region with the storage core's engine so
    /// it is eligible for fixed-buffer DMA. Synchronous: setup
    /// paths already block their thread, so a brief alive-aware
    /// spin on the reply slot is acceptable.
    pub fn register_buffer(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        let ptr = NonNull::new(base).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(PageCommand::RegisterBuffer {
                base: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        spin_block_on_with_alive(reply.wait(), &self.service_alive)
    }

    /// Stable identity of the storage-core service this channel
    /// targets. All clones of one channel share it; a channel built by
    /// a distinct [`Self::new`] has a distinct identity. Used by the
    /// channel directory to skip republishing an unchanged channel set
    /// (the `service_alive` allocation is kept alive by the holder for
    /// as long as the channel is published, so the pointer is a sound
    /// identity for live channels).
    pub(crate) fn service_id(&self) -> usize {
        Arc::as_ptr(&self.service_alive) as usize
    }
}

impl Clone for PageChannel {
    fn clone(&self) -> Self {
        Self {
            cmd_tx: self.cmd_tx.clone(),
            service_alive: self.service_alive.clone(),
        }
    }
}

/// Single-consumer receiver half of a [`PageChannel`]. Not `Clone`:
/// exactly one [`PageService`] may drain a given channel. Carries the
/// shared `service_alive` flag forward so the service can flip it
/// during shutdown.
pub struct PageChannelReceiver {
    rx: Receiver<PageCommand>,
    service_alive: Arc<AtomicBool>,
}

/// One page operation in transit to the storage core.
pub enum PageCommand {
    ReadPage {
        key: StripeKey,
        stripe_off: u64,
        dst: SendPtr,
        len: usize,
        reply: Arc<ReplySlot<bool>>,
    },
    WritePage {
        key: StripeKey,
        stripe_off: u64,
        src: SendPtr,
        len: usize,
        reply: Arc<ReplySlot<()>>,
    },
    RegisterBuffer {
        base: SendPtr,
        len: usize,
        reply: Arc<ReplySlot<()>>,
    },
}

/// Drives a storage core's [`StorageEngine`] on behalf of remote
/// [`PageChannel`] callers.
///
/// The service owns the engine `Arc` and the channel receiver and
/// exposes [`Self::poll_once`] so the storage core can interleave it
/// with `engine.run_mutator()` and `ring.progress()` on the same
/// thread. It never calls `device.progress()`: the storage core's
/// main loop already drives the ring.
pub struct PageService<B: BlockDevice + 'static> {
    engine: Arc<StorageEngine<B>>,
    rx: Receiver<PageCommand>,
    in_flight: Vec<Inflight>,
    disconnected: bool,
    service_alive: Arc<AtomicBool>,
}

impl<B: BlockDevice + 'static> PageService<B> {
    /// Wrap an engine and its companion receiver.
    pub fn new(engine: Arc<StorageEngine<B>>, rx: PageChannelReceiver) -> Self {
        Self {
            engine,
            rx: rx.rx,
            in_flight: Vec::new(),
            disconnected: false,
            service_alive: rx.service_alive,
        }
    }

    /// Flip the shared `service_alive` flag to `false`. Callers must
    /// invoke this once they have committed to never polling again,
    /// so racing channel sends resolve with `EIO` rather than
    /// parking forever.
    pub fn mark_dead(&self) {
        self.service_alive.store(false, Ordering::Release);
    }

    /// Drain queued commands into the in-flight set and poll each
    /// in-flight op once. Reads/writes are spawned as engine
    /// futures; `RegisterBuffer` runs synchronously and completes
    /// immediately.
    ///
    /// Does not drive the device; the storage core owns ring
    /// progress.
    pub fn poll_once(&mut self, cx: &mut Context<'_>) {
        loop {
            match self.rx.try_recv() {
                Ok(PageCommand::ReadPage {
                    key,
                    stripe_off,
                    dst,
                    len,
                    reply,
                }) => {
                    let engine = self.engine.clone();
                    let fut: Pin<Box<dyn Future<Output = Result<bool, Error>>>> =
                        Box::pin(async move {
                            // SAFETY: caller keeps the buffer at
                            // `dst` valid until `reply` is set (see
                            // module docs).
                            let slice = std::ptr::slice_from_raw_parts_mut(dst.0.as_ptr(), len);
                            unsafe { engine.read_page_into(key, stripe_off, slice).await }
                        });
                    self.in_flight.push(Inflight::Read { fut, reply });
                }
                Ok(PageCommand::WritePage {
                    key,
                    stripe_off,
                    src,
                    len,
                    reply,
                }) => {
                    let engine = self.engine.clone();
                    let fut: Pin<Box<dyn Future<Output = Result<(), Error>>>> =
                        Box::pin(async move {
                            // SAFETY: same contract as ReadPage; the
                            // service treats this region as immutable.
                            let slice =
                                std::ptr::slice_from_raw_parts(src.0.as_ptr().cast_const(), len);
                            unsafe { engine.write_page_from(key, stripe_off, slice).await }
                        });
                    self.in_flight.push(Inflight::Write { fut, reply });
                }
                Ok(PageCommand::RegisterBuffer { base, len, reply }) => {
                    let res = self.engine.register_extra_buffer(base.0.as_ptr(), len);
                    reply.set(res);
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    self.disconnected = true;
                    break;
                }
            }
        }

        let mut i = 0;
        while i < self.in_flight.len() {
            let done = match &mut self.in_flight[i] {
                Inflight::Read { fut, reply } => match fut.as_mut().poll(cx) {
                    Poll::Ready(r) => {
                        reply.set(r);
                        true
                    }
                    Poll::Pending => false,
                },
                Inflight::Write { fut, reply } => match fut.as_mut().poll(cx) {
                    Poll::Ready(r) => {
                        reply.set(r);
                        true
                    }
                    Poll::Pending => false,
                },
            };
            if done {
                self.in_flight.swap_remove(i);
            } else {
                i += 1;
            }
        }
    }

    /// Whether the command channel has disconnected (all
    /// [`PageChannel`]s dropped).
    pub fn channel_disconnected(&self) -> bool {
        self.disconnected
    }

    /// Whether any in-flight ops are still being polled.
    pub fn has_inflight(&self) -> bool {
        !self.in_flight.is_empty()
    }

    /// Fail every in-flight op and every still-queued command with
    /// `err`. Used on shutdown; see the old proxy's `fail_all` for
    /// the race-window discussion.
    pub fn fail_all(&mut self, err: Error) {
        for op in self.in_flight.drain(..) {
            match op {
                Inflight::Read { reply, .. } => reply.set(Err(err.clone())),
                Inflight::Write { reply, .. } => reply.set(Err(err.clone())),
            }
        }
        self.drain_pending(err);
    }

    /// Drain any commands sitting in the channel, failing each one
    /// with `err`. Cheap to call repeatedly; closes the race window
    /// between [`Self::fail_all`] and the eventual drop of the
    /// receiver.
    pub fn drain_pending(&mut self, err: Error) {
        loop {
            match self.rx.try_recv() {
                Ok(cmd) => fail_command(cmd, err.clone()),
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    self.disconnected = true;
                    break;
                }
            }
        }
    }
}

// ---- internals ---------------------------------------------------

/// Raw pointer used under the buffer-lifetime contract (see module
/// docs). Wrapping it keeps the `unsafe impl Send`/`Sync` localized.
#[derive(Copy, Clone)]
pub struct SendPtr(pub NonNull<u8>);

// SAFETY: a `SendPtr` is only constructed from a caller-owned buffer
// the caller is required to keep alive (and not concurrently mutate,
// for reads) until the matching reply slot is set.
unsafe impl Send for SendPtr {}
unsafe impl Sync for SendPtr {}

enum Inflight {
    Read {
        fut: Pin<Box<dyn Future<Output = Result<bool, Error>>>>,
        reply: Arc<ReplySlot<bool>>,
    },
    Write {
        fut: Pin<Box<dyn Future<Output = Result<(), Error>>>>,
        reply: Arc<ReplySlot<()>>,
    },
}

/// One-shot completion slot generic over the success type. The
/// producer (service) stores a result and wakes any parked consumer;
/// the consumer polls and returns the stored result if present.
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

/// `Future` adapter for the async paths (`read_page` / `write_page`).
/// Resolves with `Err(Io(EIO))` if the slot is still pending once the
/// service flips `service_alive` to false; the extra poll absorbs the
/// race where the service set the slot and then flipped the flag.
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

fn fail_command(cmd: PageCommand, err: Error) {
    match cmd {
        PageCommand::ReadPage { reply, .. } => reply.set(Err(err)),
        PageCommand::WritePage { reply, .. } => reply.set(Err(err)),
        PageCommand::RegisterBuffer { reply, .. } => reply.set(Err(err)),
    }
}

/// Spin on a reply slot until it resolves, bailing with `EIO` once
/// the service flips `service_alive` to false. The trailing extra
/// poll guards the race where the service filled the slot and then
/// flipped the flag before our last poll observed the result.
fn spin_block_on_with_alive<T>(fut: ReplyWait<T>, service_alive: &AtomicBool) -> Result<T, Error> {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = Box::pin(fut);
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                if !service_alive.load(Ordering::Acquire) {
                    return match fut.as_mut().poll(&mut cx) {
                        Poll::Ready(v) => v,
                        Poll::Pending => Err(Error::Io(libc::EIO)),
                    };
                }
                std::thread::yield_now();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use crate::storage::engine::EngineConfig;
    use std::sync::mpsc::channel as std_channel;
    use std::task::{RawWaker, RawWakerVTable};
    use std::time::Duration;

    fn block_on<F: Future>(mut fut: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        // SAFETY: the future is owned on the stack and never moved
        // after pinning.
        let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
        for _ in 0..1_000_000 {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
            std::thread::yield_now();
        }
        panic!("block_on: future did not complete within spin budget");
    }

    /// Build a waker whose `wake` flips a shared `Arc<AtomicBool>`,
    /// mirroring the shard loop's `flag_waker`. The shard polls
    /// page-channel reply futures with exactly this kind of waker, so
    /// the `ReplySlot` stashes it and a cross-thread `set` must flip
    /// the flag. This local copy lets the page-channel tests pin that
    /// contract from this side of the boundary.
    fn flag_waker() -> (Waker, Arc<AtomicBool>) {
        let flag = Arc::new(AtomicBool::new(false));
        let data = Arc::into_raw(flag.clone()) as *const ();
        // SAFETY: `data` is a freshly leaked `Arc<AtomicBool>` and the
        // vtable upholds the matching clone/wake/drop refcounting.
        let waker = unsafe { Waker::from_raw(RawWaker::new(data, &FLAG_VTABLE)) };
        (waker, flag)
    }

    static FLAG_VTABLE: RawWakerVTable =
        RawWakerVTable::new(flag_clone, flag_wake, flag_wake_by_ref, flag_drop);

    unsafe fn flag_clone(data: *const ()) -> RawWaker {
        // SAFETY: `data` points at a live `Arc<AtomicBool>`.
        unsafe { Arc::increment_strong_count(data as *const AtomicBool) };
        RawWaker::new(data, &FLAG_VTABLE)
    }

    unsafe fn flag_wake(data: *const ()) {
        // SAFETY: consumes the one owned ref this waker held.
        let arc = unsafe { Arc::from_raw(data as *const AtomicBool) };
        arc.store(true, Ordering::Release);
    }

    unsafe fn flag_wake_by_ref(data: *const ()) {
        // SAFETY: borrows without consuming; the ref is handed back.
        let arc = unsafe { Arc::from_raw(data as *const AtomicBool) };
        arc.store(true, Ordering::Release);
        let _ = Arc::into_raw(arc);
    }

    unsafe fn flag_drop(data: *const ()) {
        // SAFETY: balances one clone/into_raw strong ref.
        unsafe { Arc::decrement_strong_count(data as *const AtomicBool) };
    }

    /// Run a storage-core-like loop on the current thread: build a
    /// MockDevice engine, hand a [`PageChannel`] back, then drive
    /// [`PageService::poll_once`] and the engine mutator until the
    /// channel disconnects and all work drains.
    fn run_service_core(channel_tx: std::sync::mpsc::Sender<PageChannel>, stop: Arc<AtomicBool>) {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 256,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096;
        cfg.btree_page_bytes = 4096;
        let engine = Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap());

        let (channel, rx) = PageChannel::new();
        channel_tx.send(channel).expect("send channel back");
        let mut service = PageService::new(engine.clone(), rx);

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut mutator = Box::pin(engine.clone().run_mutator());
        let mut close_signaled = false;
        let mut mutator_done = false;
        loop {
            if !mutator_done {
                if let Poll::Ready(()) = mutator.as_mut().poll(&mut cx) {
                    mutator_done = true;
                }
            }
            service.poll_once(&mut cx);

            let shutdown = stop.load(Ordering::Acquire) || service.channel_disconnected();
            if shutdown && !close_signaled {
                engine.close_mutator();
                close_signaled = true;
            }
            if close_signaled && mutator_done && !service.has_inflight() {
                service.fail_all(Error::Io(libc::EIO));
                service.drain_pending(Error::Io(libc::EIO));
                service.mark_dead();
                return;
            }
            std::thread::sleep(Duration::from_micros(50));
        }
    }

    #[test]
    fn page_channel_is_send_and_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<PageChannel>();
    }

    #[test]
    fn reply_slot_completes_set_then_wait() {
        let slot: Arc<ReplySlot<bool>> = ReplySlot::new();
        slot.set(Ok(true));
        assert!(matches!(block_on(slot.wait()), Ok(true)));
    }

    #[test]
    fn reply_slot_wait_then_set_wakes_waker() {
        let slot: Arc<ReplySlot<()>> = ReplySlot::new();
        let setter = slot.clone();
        let h = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(5));
            setter.set(Err(Error::Io(libc::EIO)));
        });
        let result = block_on(slot.wait());
        h.join().unwrap();
        assert!(matches!(result, Err(Error::Io(n)) if n == libc::EIO));
    }

    /// The shard drives its page-channel reply futures with a
    /// flag-flipping waker so a cross-thread completion is observable
    /// without a real reactor. This pins that contract: a `ReplyWait`
    /// polled with such a waker stashes it, and a later `set` from a
    /// "remote" storage core wakes it, flipping the shared flag the
    /// shard's idle-park logic checks. If `ReplySlot::set` ever stopped
    /// waking the stored waker, the shard would silently fall back to
    /// the 100ms park this fix removed.
    #[test]
    fn reply_set_flips_stored_flag_waker() {
        let slot: Arc<ReplySlot<bool>> = ReplySlot::new();
        let (waker, flag) = flag_waker();
        let mut cx = Context::from_waker(&waker);

        // First poll: pending, so the slot stashes our flag waker.
        let mut fut = Box::pin(slot.clone().wait());
        assert!(matches!(fut.as_mut().poll(&mut cx), Poll::Pending));
        assert!(!flag.load(Ordering::Acquire), "flag clear before set");

        // A storage core completes the op on another logical thread.
        slot.set(Ok(true));

        // The reply path must have woken the stashed waker.
        assert!(
            flag.load(Ordering::Acquire),
            "ReplySlot::set must wake the stored waker so the shard re-polls",
        );

        // And the result is now observable on the next poll.
        assert!(matches!(fut.as_mut().poll(&mut cx), Poll::Ready(Ok(true))));
    }

    /// Write then read a page across the channel. The admission
    /// filter rejects the first write, so we write twice before the
    /// read can hit (mirrors `shard_local_store_roundtrip_across_disks`).
    #[test]
    fn page_round_trip_via_os_thread() {
        let (chan_tx, chan_rx) = std_channel::<PageChannel>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let service = std::thread::spawn(move || run_service_core(chan_tx, stop_thr));
        let channel = chan_rx.recv().expect("receive channel");

        // Register a pinned backing first so the engine can DMA.
        let mut buf = vec![0u8; 4096 * 2].into_boxed_slice();
        channel
            .register_buffer(buf.as_mut_ptr(), buf.len())
            .expect("register");

        // Stage a pattern in page 0 and write it twice.
        for i in 0..4096usize {
            buf[i] = ((i * 13) & 0xff) as u8;
        }
        let key = StripeKey([0x42; 32]);
        let src = std::ptr::slice_from_raw_parts(buf.as_ptr(), 4096);
        block_on(channel.write_page(key, 0, src)).expect("write 1");
        block_on(channel.write_page(key, 0, src)).expect("write 2");

        // Read into page 1 and confirm the bytes match.
        let dst = std::ptr::slice_from_raw_parts_mut(unsafe { buf.as_mut_ptr().add(4096) }, 4096);
        let hit = block_on(channel.read_page(key, 0, dst)).expect("read");
        assert!(hit, "expected cache hit through the channel");
        for i in 0..4096usize {
            assert_eq!(buf[4096 + i], ((i * 13) & 0xff) as u8, "byte {i} mismatch");
        }

        drop(channel);
        stop.store(true, Ordering::Release);
        service.join().expect("service thread");
    }

    #[test]
    fn register_buffer_round_trip() {
        let (chan_tx, chan_rx) = std_channel::<PageChannel>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let service = std::thread::spawn(move || run_service_core(chan_tx, stop_thr));
        let channel = chan_rx.recv().expect("receive channel");

        let mut buf = vec![0u8; 4096];
        channel
            .register_buffer(buf.as_mut_ptr(), buf.len())
            .expect("register_buffer");

        drop(channel);
        stop.store(true, Ordering::Release);
        service.join().expect("service thread");
    }

    /// Dropping every channel disconnects the service, which must
    /// treat that as an implicit shutdown and exit cleanly.
    #[test]
    fn channel_drop_disconnects_service() {
        let (chan_tx, chan_rx) = std_channel::<PageChannel>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let service = std::thread::spawn(move || run_service_core(chan_tx, stop_thr));
        let channel = chan_rx.recv().expect("receive channel");
        drop(channel);
        service.join().expect("service thread");
        assert!(!stop.load(Ordering::Acquire));
    }

    /// Once the service has exited, a fresh write must fail promptly
    /// with `EPIPE` instead of parking on a reply slot.
    #[test]
    fn send_after_shutdown_returns_epipe() {
        let (chan_tx, chan_rx) = std_channel::<PageChannel>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let service = std::thread::spawn(move || run_service_core(chan_tx, stop_thr));
        let channel = chan_rx.recv().expect("receive channel");
        stop.store(true, Ordering::Release);
        service.join().expect("service thread");

        let mut buf = vec![0u8; 4096];
        let src = std::ptr::slice_from_raw_parts(buf.as_mut_ptr().cast_const(), 4096);
        let result = block_on(channel.write_page(StripeKey([0; 32]), 0, src));
        assert!(
            matches!(result, Err(Error::Io(n)) if n == libc::EPIPE),
            "expected EPIPE after service exit, got {result:?}",
        );
    }
}
