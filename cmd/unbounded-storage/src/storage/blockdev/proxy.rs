// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `Send + Sync` bridge for a [`BlockDevice`] whose real implementation
//! is pinned to one thread.
//!
//! Some block devices, most notably [`UringBlockDevice`], cannot leave
//! the thread that opened them: the underlying `io_uring` instance
//! holds kernel state that is keyed by submitter task, and the device
//! is marked `!Send` to make that non-negotiable. Higher layers
//! ([`StorageEngine`], [`LiveDiskTopology`], [`LocalStorage`]) require
//! `B: Send + Sync` so they can be shared across shard threads, which
//! means there has to be a `Send + Sync` shim sitting in front of the
//! pinned device.
//!
//! [`BlockDeviceProxy`] is that shim. It implements [`BlockDevice`]
//! by serializing each call into a [`Command`] and shipping it over an
//! `mpsc` channel to the thread that owns the real device. That
//! thread runs [`run_proxy_service`], which drains the command queue,
//! dispatches reads and writes against the underlying device,
//! repeatedly calls [`BlockDevice::progress`], and finishes each
//! request by completing the associated reply slot.
//!
//! The proxy is intentionally minimal: it does not buffer, batch, or
//! reorder; it does not enforce queue depth (the underlying device
//! already does); and dropping a read or write future does not cancel
//! the in-flight operation. The last point matters: the service-side
//! future continues to run and the kernel may still write into the
//! caller's buffer, so callers must keep the buffer alive until the
//! reply slot is set. That is the same lifetime contract callers
//! already have with [`UringBlockDevice`]: I/O buffers live inside
//! the bufferpool [`Backing`] for the lifetime of the program.
//!
//! [`UringBlockDevice`]: crate::storage::blockdev::UringBlockDevice
//! [`StorageEngine`]: crate::storage::StorageEngine
//! [`LiveDiskTopology`]: crate::disk_supervisor::LiveDiskTopology
//! [`LocalStorage`]: crate::storage::LocalStorage
//! [`Backing`]: crate::bufferpool::Backing

use std::future::Future;
use std::pin::Pin;
use std::ptr::NonNull;
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Receiver, Sender, TryRecvError, channel};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
use std::time::Duration;

use crate::storage::blockdev::BlockDevice;
use crate::storage::types::{Error, Lba};

/// Geometry of the device behind a proxy. Cached on the proxy side
/// so that calls to [`BlockDevice::page_size`] and friends do not
/// have to cross the channel.
#[derive(Copy, Clone, Debug)]
pub struct ProxyMetadata {
    pub page_size: usize,
    pub capacity_pages: u64,
    pub write_queue_depth: u32,
}

/// `Send + Sync` handle to a [`BlockDevice`] that lives on another
/// thread.
///
/// Clone freely; every clone points at the same service. The proxy
/// implements [`BlockDevice`] by enqueuing each call on its command
/// channel and awaiting the reply slot the service will fill in on
/// completion. When the last proxy is dropped, the channel closes
/// and the service loop exits after draining its in-flight work.
///
/// See the module docs for the buffer-lifetime contract.
pub struct BlockDeviceProxy {
    cmd_tx: Sender<Command>,
    metadata: ProxyMetadata,
    /// Set to `false` by [`run_proxy_service`] after its final
    /// receiver-drain and before the receiver is dropped. The
    /// proxy's synchronous waits check this flag so that a command
    /// whose `send` raced the service's shutdown can fail cleanly
    /// with `EIO` instead of parking on a reply slot that will
    /// never be set (the channel destructor silently discards
    /// in-flight commands without completing their replies).
    service_alive: Arc<AtomicBool>,
}

impl BlockDeviceProxy {
    /// Build a proxy and the receiver half of its command channel.
    /// The receiver is consumed by [`run_proxy_service`] on the
    /// thread that owns the underlying device.
    pub fn new(metadata: ProxyMetadata) -> (Self, ProxyReceiver) {
        let (tx, rx) = channel();
        let service_alive = Arc::new(AtomicBool::new(true));
        (
            Self {
                cmd_tx: tx,
                metadata,
                service_alive: service_alive.clone(),
            },
            ProxyReceiver { rx, service_alive },
        )
    }
}

impl Clone for BlockDeviceProxy {
    fn clone(&self) -> Self {
        Self {
            cmd_tx: self.cmd_tx.clone(),
            metadata: self.metadata,
            service_alive: self.service_alive.clone(),
        }
    }
}

impl BlockDevice for BlockDeviceProxy {
    fn page_size(&self) -> usize {
        self.metadata.page_size
    }

    fn capacity_pages(&self) -> u64 {
        self.metadata.capacity_pages
    }

    fn write_queue_depth(&self) -> u32 {
        self.metadata.write_queue_depth
    }

    fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        // The trait is synchronous; callers (Pool::new,
        // register_extra_buffer) are setup paths that already block
        // their thread, so a brief yield-loop on the reply slot is
        // acceptable here. The service-alive check guarantees the
        // wait cannot outlive the service: any send that races the
        // service's exit either lands in the final drain (reply set
        // to EIO) or sits in the channel until the receiver is
        // dropped (no completion); the latter case is caught by the
        // `service_alive == false` check below.
        let ptr = NonNull::new(base).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(Command::RegisterBuffers {
                base: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        spin_block_on_with_alive(reply.wait(), &self.service_alive)
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        let len = dst.len();
        let ptr = NonNull::new(dst.as_mut_ptr()).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(Command::Read {
                lba,
                ptr: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        AliveAwareWait::new(reply, self.service_alive.clone()).await
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error> {
        let len = src.len();
        // Transit as a single SendPtr; the service-side future
        // reconstructs the immutable slice for write.
        let ptr = NonNull::new(src.as_ptr() as *mut u8).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(Command::Write {
                lba,
                ptr: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        AliveAwareWait::new(reply, self.service_alive.clone()).await
    }

    fn progress(&self) -> Result<(), Error> {
        // The service loop on the owning thread drives the device's
        // progress; proxy callers have nothing useful to do here.
        Ok(())
    }
}

/// Single-consumer receiver half of the command channel.
///
/// Not `Clone`: only one [`run_proxy_service`] call may drain a
/// given proxy. Carries the shared `service_alive` flag forward so
/// the eventual [`ProxyService`] can flip it during shutdown.
pub struct ProxyReceiver {
    rx: Receiver<Command>,
    service_alive: Arc<AtomicBool>,
}

/// Single-iteration core of the proxy service loop.
///
/// `ProxyService` owns the device end of the channel - the `Rc<B>`
/// device, the receiver, and the in-flight queue - and exposes
/// [`Self::poll_once`] so callers can drive it alongside other
/// futures on the same thread. The disk supervisor uses this to
/// interleave the proxy with `StorageEngine::open` and
/// `StorageEngine::run_mutator` on the disk-owning thread.
///
/// Standalone callers usually want [`run_proxy_service`], which
/// wraps this in a stop-aware loop.
pub struct ProxyService<B: BlockDevice + 'static> {
    device: Rc<B>,
    rx: Receiver<Command>,
    in_flight: Vec<InflightOp>,
    disconnected: bool,
    service_alive: Arc<AtomicBool>,
}

impl<B: BlockDevice + 'static> ProxyService<B> {
    /// Wrap an owned device and its companion receiver.
    pub fn new(device: B, rx: ProxyReceiver) -> Self {
        Self {
            device: Rc::new(device),
            rx: rx.rx,
            in_flight: Vec::new(),
            disconnected: false,
            service_alive: rx.service_alive,
        }
    }

    /// Flip the shared `service_alive` flag to `false`. Callers
    /// driving [`Self::poll_once`] directly must call this once
    /// they have committed to never polling again, so that
    /// synchronous proxy waits ([`BlockDeviceProxy::register_buffers`])
    /// and async ones ([`BlockDeviceProxy::read`] /
    /// [`BlockDeviceProxy::write`]) can stop waiting for replies
    /// that will never arrive. [`run_proxy_service`] handles this
    /// automatically.
    pub fn mark_dead(&self) {
        self.service_alive.store(false, Ordering::Release);
    }

    /// The underlying device, shared via `Rc` so each in-flight
    /// future can keep it alive for the duration of its I/O.
    pub fn device(&self) -> &Rc<B> {
        &self.device
    }

    /// Whether the command channel has disconnected (all proxies
    /// dropped). Once true, no further commands will arrive.
    pub fn channel_disconnected(&self) -> bool {
        self.disconnected
    }

    /// Whether any in-flight ops are still being polled.
    pub fn has_inflight(&self) -> bool {
        !self.in_flight.is_empty()
    }

    /// Drain queued commands into the in-flight set, poll each
    /// in-flight future once, and drive [`BlockDevice::progress`].
    ///
    /// On a progress failure every in-flight op and every queued
    /// command is failed with the underlying errno and the error is
    /// returned; the caller is expected to tear down the device.
    /// `Disconnected` on the command channel is not fatal - it is
    /// surfaced via [`Self::channel_disconnected`] so the caller can
    /// decide when to exit.
    pub fn poll_once(&mut self, cx: &mut Context<'_>) -> Result<(), Error> {
        // 1. Drain pending commands into the in-flight set.
        loop {
            match self.rx.try_recv() {
                Ok(Command::Read {
                    lba,
                    ptr,
                    len,
                    reply,
                }) => {
                    let dev = Rc::clone(&self.device);
                    let fut: Pin<Box<dyn Future<Output = Result<(), Error>>>> =
                        Box::pin(async move {
                            // SAFETY: caller guarantees the buffer at
                            // `ptr` remains valid until `reply.set` is
                            // invoked (see module docs).
                            let slice =
                                unsafe { std::slice::from_raw_parts_mut(ptr.0.as_ptr(), len) };
                            dev.read(lba, slice).await
                        });
                    self.in_flight.push(InflightOp { fut, reply });
                }
                Ok(Command::Write {
                    lba,
                    ptr,
                    len,
                    reply,
                }) => {
                    let dev = Rc::clone(&self.device);
                    let fut: Pin<Box<dyn Future<Output = Result<(), Error>>>> =
                        Box::pin(async move {
                            // SAFETY: same contract as Read; the
                            // service treats this slice as immutable.
                            let slice = unsafe {
                                std::slice::from_raw_parts(ptr.0.as_ptr() as *const u8, len)
                            };
                            dev.write(lba, slice).await
                        });
                    self.in_flight.push(InflightOp { fut, reply });
                }
                Ok(Command::RegisterBuffers { base, len, reply }) => {
                    let res = self.device.register_buffers(base.0.as_ptr(), len);
                    reply.set(res);
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    self.disconnected = true;
                    break;
                }
            }
        }

        // 2. Poll in-flight ops; complete any that finished.
        let mut i = 0;
        while i < self.in_flight.len() {
            let outcome = self.in_flight[i].fut.as_mut().poll(cx);
            match outcome {
                Poll::Ready(result) => {
                    let op = self.in_flight.swap_remove(i);
                    op.reply.set(result);
                }
                Poll::Pending => i += 1,
            }
        }

        // 3. Drive the underlying device. A progress failure is
        //    treated as fatal: there is nothing useful we can do
        //    with in-flight requests except fail them and exit.
        if let Err(e) = self.device.progress() {
            let errno = errno_of(&e);
            self.fail_all(errno);
            return Err(e);
        }

        Ok(())
    }

    /// Fail every in-flight op and every still-queued command with
    /// the supplied errno. Used on shutdown and on a progress error.
    ///
    /// Invariant for shutdown callers: after this returns, any send
    /// that races with our final drain either (a) was caught by the
    /// drain and its reply slot was set with `Err(Io(errno))`, or
    /// (b) will fail at the producer with `SendError` once the
    /// receiver is dropped, which the proxy translates to `EPIPE`.
    /// Either way no caller hangs waiting on a reply slot that
    /// nobody will set. Callers that want to close the window
    /// further should call [`Self::drain_pending`] one more time
    /// before dropping the service.
    pub fn fail_all(&mut self, errno: i32) {
        for op in self.in_flight.drain(..) {
            op.reply.set(Err(Error::Io(errno)));
        }
        self.drain_pending(errno);
    }

    /// Drain any commands sitting in the channel, failing each one
    /// with `errno`. Cheap to call repeatedly; used by the shutdown
    /// path to close the race window between the drain inside
    /// [`Self::fail_all`] and the eventual drop of the receiver.
    pub fn drain_pending(&mut self, errno: i32) {
        loop {
            match self.rx.try_recv() {
                Ok(cmd) => fail_command(cmd, errno),
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    self.disconnected = true;
                    break;
                }
            }
        }
    }
}

/// Drive a proxy's underlying [`BlockDevice`].
///
/// Runs on the thread that owns the device and returns when the
/// command channel disconnects or the caller flips `stop`, provided
/// no in-flight ops remain. While stopping, queued commands are
/// failed with `EIO` so callers do not park forever waiting on a
/// reply that will never arrive.
///
/// On a [`BlockDevice::progress`] error every in-flight op and every
/// queued command is failed before the function returns; the error
/// is logged on stderr and the caller is expected to tear down the
/// device.
pub fn run_proxy_service<B: BlockDevice + 'static>(
    device: B,
    rx: ProxyReceiver,
    stop: Arc<AtomicBool>,
) {
    let mut svc = ProxyService::new(device, rx);
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    loop {
        if let Err(e) = svc.poll_once(&mut cx) {
            eprintln!("uring proxy service: progress failed: {e}");
            svc.mark_dead();
            return;
        }

        let shutdown_requested = stop.load(Ordering::Acquire) || svc.channel_disconnected();
        if shutdown_requested && !svc.has_inflight() {
            // Closing the shutdown race: `fail_all` drains both the
            // in-flight set and the receiver, but a producer thread
            // may sneak a `send` in between that drain and the
            // implicit drop of `svc.rx` at function exit. Drain one
            // more time so any such command is failed with EIO
            // instead of leaving its `ReplySlot` permanently unset.
            //
            // The drain alone still does not close the window: a
            // send that lands after the second drain but before
            // `svc` (and therefore `rx`) is dropped would sit in
            // the channel and be silently discarded by the channel
            // destructor, parking the producer's reply slot
            // forever. `mark_dead` flips `service_alive` to false
            // so the proxy's `spin_block_on_with_alive` and
            // `AliveAwareWait` see the service is gone and resolve
            // the wait with `EIO` rather than spin.
            svc.fail_all(libc::EIO);
            svc.drain_pending(libc::EIO);
            svc.mark_dead();
            return;
        }

        std::thread::sleep(Duration::from_micros(100));
    }
}

// ---- internals ---------------------------------------------------

enum Command {
    Read {
        lba: Lba,
        ptr: SendPtr,
        len: usize,
        reply: Arc<ReplySlot>,
    },
    Write {
        lba: Lba,
        ptr: SendPtr,
        len: usize,
        reply: Arc<ReplySlot>,
    },
    RegisterBuffers {
        base: SendPtr,
        len: usize,
        reply: Arc<ReplySlot>,
    },
}

/// Raw pointer that we promise to use under the proxy's buffer
/// lifetime contract (see module docs). Wrapping the pointer in a
/// dedicated type keeps the `unsafe impl Send` localized so the rest
/// of the file stays safe Rust.
#[derive(Copy, Clone)]
struct SendPtr(NonNull<u8>);

// SAFETY: a `SendPtr` is only constructed from a caller-owned buffer
// that the caller is required to keep alive (and not concurrently
// mutate, in the case of reads) until the matching reply slot is set.
unsafe impl Send for SendPtr {}
unsafe impl Sync for SendPtr {}

struct InflightOp {
    fut: Pin<Box<dyn Future<Output = Result<(), Error>>>>,
    reply: Arc<ReplySlot>,
}

/// One-shot completion slot. Mirrors the `MutatorReply` pattern used
/// inside the storage engine: the producer (service) stores a result
/// and wakes any parked consumer waker; the consumer polls and
/// returns the stored result if present.
struct ReplySlot {
    inner: Mutex<ReplyInner>,
}

struct ReplyInner {
    result: Option<Result<(), Error>>,
    waker: Option<Waker>,
}

impl ReplySlot {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(ReplyInner {
                result: None,
                waker: None,
            }),
        })
    }

    fn set(&self, result: Result<(), Error>) {
        let waker = {
            let mut g = self.inner.lock().unwrap();
            g.result = Some(result);
            g.waker.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    fn wait(self: Arc<Self>) -> ReplyWait {
        ReplyWait { inner: self }
    }
}

struct ReplyWait {
    inner: Arc<ReplySlot>,
}

impl Future for ReplyWait {
    type Output = Result<(), Error>;

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

fn fail_command(cmd: Command, errno: i32) {
    match cmd {
        Command::Read { reply, .. }
        | Command::Write { reply, .. }
        | Command::RegisterBuffers { reply, .. } => {
            reply.set(Err(Error::Io(errno)));
        }
    }
}

fn errno_of(err: &Error) -> i32 {
    match err {
        Error::Io(n) => *n,
        Error::Corrupt | Error::OutOfSpace | Error::Cancelled | Error::OutOfRange => libc::EIO,
    }
}

fn spin_block_on<F: Future>(fut: F) -> F::Output {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = Box::pin(fut);
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => std::thread::yield_now(),
        }
    }
}

/// Variant of [`spin_block_on`] specialized for proxy reply slots
/// that bails with `EIO` once the service flips `service_alive` to
/// false. The trailing extra poll guards the race where the
/// service drained the command (filling the slot) and then flipped
/// the flag before our last poll observed the result.
fn spin_block_on_with_alive(fut: ReplyWait, service_alive: &AtomicBool) -> Result<(), Error> {
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

/// `Future` adapter for the async proxy paths (`read` / `write`).
/// Polls the underlying [`ReplyWait`]; if the slot is still pending
/// and `service_alive` is false, polls once more to absorb the
/// race window described in [`spin_block_on_with_alive`] and then
/// resolves with `Err(Error::Io(EIO))`.
struct AliveAwareWait {
    reply: ReplyWait,
    service_alive: Arc<AtomicBool>,
}

impl AliveAwareWait {
    fn new(reply: Arc<ReplySlot>, service_alive: Arc<AtomicBool>) -> Self {
        Self {
            reply: ReplyWait { inner: reply },
            service_alive,
        }
    }
}

impl Future for AliveAwareWait {
    type Output = Result<(), Error>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // SAFETY: we project a pinned reference into a field that
        // is itself trivially `Unpin` (ReplyWait holds an Arc), so
        // moving the inner is sound. We never expose `&mut self`
        // to user code.
        let this = unsafe { self.get_unchecked_mut() };
        let reply_pin = Pin::new(&mut this.reply);
        match reply_pin.poll(cx) {
            Poll::Ready(v) => Poll::Ready(v),
            Poll::Pending => {
                if !this.service_alive.load(Ordering::Acquire) {
                    let reply_pin = Pin::new(&mut this.reply);
                    return match reply_pin.poll(cx) {
                        Poll::Ready(v) => Poll::Ready(v),
                        Poll::Pending => Poll::Ready(Err(Error::Io(libc::EIO))),
                    };
                }
                Poll::Pending
            }
        }
    }
}

fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: the vtable functions are all no-ops or returns of the
    // same vtable, so the resulting waker can be cloned and dropped
    // arbitrarily without UB.
    unsafe { Waker::from_raw(raw()) }
}

// ---- tests -------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use std::sync::atomic::AtomicBool;
    use std::sync::mpsc::channel as std_channel;

    fn block_on<F: Future>(mut fut: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        // SAFETY: the future is owned on the stack and never moved
        // after pinning; this mirrors the noop-waker block_on used
        // elsewhere in the crate.
        let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
        for _ in 0..1_000_000 {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
            std::thread::yield_now();
        }
        panic!("block_on: future did not complete within spin budget");
    }

    #[test]
    fn proxy_is_send_and_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<BlockDeviceProxy>();
    }

    #[test]
    fn reply_slot_completes_set_then_wait() {
        let slot = ReplySlot::new();
        slot.set(Ok(()));
        assert!(matches!(block_on(slot.wait()), Ok(())));
    }

    #[test]
    fn reply_slot_wait_then_set_wakes_waker() {
        let slot = ReplySlot::new();
        let slot_setter = slot.clone();
        // Spawn a thread that completes the slot while the main
        // thread is parked waiting.
        let h = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(5));
            slot_setter.set(Err(Error::Io(libc::EIO)));
        });
        let result = block_on(slot.wait());
        h.join().unwrap();
        assert!(matches!(result, Err(Error::Io(n)) if n == libc::EIO));
    }

    /// Spin up a service thread around a MockDevice and verify a
    /// write/read round-trip travels through the proxy unchanged.
    #[test]
    fn proxy_round_trip_via_os_thread() {
        let (proxy_tx, proxy_rx) = std_channel::<BlockDeviceProxy>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_clone = stop.clone();

        let service = std::thread::spawn(move || {
            let device = MockDevice::new(MockDeviceConfig::default());
            let metadata = ProxyMetadata {
                page_size: device.page_size(),
                capacity_pages: device.capacity_pages(),
                write_queue_depth: device.write_queue_depth(),
            };
            let (proxy, rx) = BlockDeviceProxy::new(metadata);
            proxy_tx.send(proxy).expect("send proxy back");
            run_proxy_service(device, rx, stop_clone);
        });

        let proxy = proxy_rx.recv().expect("receive proxy");
        assert_eq!(proxy.page_size(), 4096);
        assert_eq!(proxy.capacity_pages(), 1024);

        let payload = vec![0xa5u8; proxy.page_size()];
        block_on(proxy.write(Lba(7), &payload)).expect("write");

        let mut buf = vec![0u8; proxy.page_size()];
        block_on(proxy.read(Lba(7), &mut buf)).expect("read");
        assert_eq!(buf, payload);

        drop(proxy);
        stop.store(true, Ordering::Release);
        service.join().expect("service thread");
    }

    /// `register_buffers` is synchronous on the trait; verify the
    /// proxy's spin-block path completes against the service.
    #[test]
    fn proxy_register_buffers_round_trip() {
        let (proxy_tx, proxy_rx) = std_channel::<BlockDeviceProxy>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_clone = stop.clone();

        let service = std::thread::spawn(move || {
            let device = MockDevice::new(MockDeviceConfig::default());
            let metadata = ProxyMetadata {
                page_size: device.page_size(),
                capacity_pages: device.capacity_pages(),
                write_queue_depth: device.write_queue_depth(),
            };
            let (proxy, rx) = BlockDeviceProxy::new(metadata);
            proxy_tx.send(proxy).expect("send proxy back");
            run_proxy_service(device, rx, stop_clone);
        });

        let proxy = proxy_rx.recv().expect("receive proxy");
        let mut buf = vec![0u8; 4096];
        proxy
            .register_buffers(buf.as_mut_ptr(), buf.len())
            .expect("register_buffers");

        drop(proxy);
        stop.store(true, Ordering::Release);
        service.join().expect("service thread");
    }

    /// Dropping every proxy disconnects the channel, which the
    /// service must treat as an implicit shutdown and exit cleanly.
    #[test]
    fn proxy_drop_disconnects_service() {
        let (proxy_tx, proxy_rx) = std_channel::<BlockDeviceProxy>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_clone = stop.clone();

        let service = std::thread::spawn(move || {
            let device = MockDevice::new(MockDeviceConfig::default());
            let metadata = ProxyMetadata {
                page_size: device.page_size(),
                capacity_pages: device.capacity_pages(),
                write_queue_depth: device.write_queue_depth(),
            };
            let (proxy, rx) = BlockDeviceProxy::new(metadata);
            proxy_tx.send(proxy).expect("send proxy back");
            run_proxy_service(device, rx, stop_clone);
        });

        let proxy = proxy_rx.recv().expect("receive proxy");
        // Drop without flipping `stop`: the receiver-disconnect arm
        // should be sufficient to bring the service down.
        drop(proxy);
        service.join().expect("service thread");
        // `stop` was never set; the test passes by virtue of the
        // join completing.
        assert!(!stop.load(Ordering::Acquire));
    }

    /// Stop flag flipped while a write is in flight: the service
    /// must finish that write before exiting so the caller's reply
    /// slot completes.
    ///
    /// Synchronization: `BlockDeviceProxy::write` is an `async fn`
    /// and does not enqueue its `Command` until first polled. If we
    /// flipped `stop` from the test thread before polling the
    /// future even once, the service could observe `stop && rx
    /// empty && no in_flight` and exit before the send ever
    /// happened - the producer would then get `EPIPE`, not the
    /// "in-flight op completes despite stop" outcome this test is
    /// supposed to pin down. To eliminate that window we drive the
    /// write to completion on a worker thread and only flip `stop`
    /// after a sleep that comfortably exceeds the service's ~100us
    /// poll cadence; by then the worker has had dozens of polls
    /// worth of headroom to enqueue the command. If the send had
    /// not happened by then this test would fail loudly (the worker
    /// would return `EPIPE`) rather than flake silently.
    #[test]
    fn proxy_shutdown_after_inflight_completes() {
        let (proxy_tx, proxy_rx) = std_channel::<BlockDeviceProxy>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_clone = stop.clone();

        let service = std::thread::spawn(move || {
            let device = MockDevice::new(MockDeviceConfig::default());
            let metadata = ProxyMetadata {
                page_size: device.page_size(),
                capacity_pages: device.capacity_pages(),
                write_queue_depth: device.write_queue_depth(),
            };
            let (proxy, rx) = BlockDeviceProxy::new(metadata);
            proxy_tx.send(proxy).expect("send proxy back");
            run_proxy_service(device, rx, stop_clone);
        });

        let proxy = proxy_rx.recv().expect("receive proxy");
        let payload = vec![1u8; proxy.page_size()];

        let (result_tx, result_rx) = std_channel::<Result<(), Error>>();
        let writer_proxy = proxy.clone();
        let writer = std::thread::spawn(move || {
            let res = block_on(writer_proxy.write(Lba(0), &payload));
            result_tx.send(res).expect("send write result");
        });

        // 50ms >> the service's 100us poll cadence, so the worker's
        // first poll (which performs the send) has long happened by
        // the time we flip `stop`. If this assumption ever ceases
        // to hold the worker returns EPIPE and the assertion below
        // fails loudly instead of hanging.
        std::thread::sleep(Duration::from_millis(50));
        stop.store(true, Ordering::Release);

        let result = result_rx.recv().expect("write result");
        result.expect("write completes after stop");
        writer.join().expect("writer thread");

        drop(proxy);
        service.join().expect("service thread");
    }

    /// Once the service has exited, a fresh `write` must fail
    /// promptly with `EPIPE` instead of parking on a reply slot
    /// nobody will set.
    #[test]
    fn proxy_send_after_shutdown_returns_epipe() {
        let (proxy_tx, proxy_rx) = std_channel::<BlockDeviceProxy>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_clone = stop.clone();

        let service = std::thread::spawn(move || {
            let device = MockDevice::new(MockDeviceConfig::default());
            let metadata = ProxyMetadata {
                page_size: device.page_size(),
                capacity_pages: device.capacity_pages(),
                write_queue_depth: device.write_queue_depth(),
            };
            let (proxy, rx) = BlockDeviceProxy::new(metadata);
            proxy_tx.send(proxy).expect("send proxy back");
            run_proxy_service(device, rx, stop_clone);
        });

        let proxy = proxy_rx.recv().expect("receive proxy");
        stop.store(true, Ordering::Release);
        service.join().expect("service thread");

        let payload = vec![0u8; proxy.page_size()];
        let result = block_on(proxy.write(Lba(0), &payload));
        assert!(
            matches!(result, Err(Error::Io(n)) if n == libc::EPIPE),
            "expected EPIPE after service exit, got {result:?}",
        );
    }

    /// Synchronous `register_buffers` must not hang when the
    /// service shuts down concurrently. A watchdog thread bounds
    /// the test so a regression fails loudly instead of wedging
    /// CI; `std::thread::JoinHandle` has no timed `join`, so we
    /// fall back to "panic if the worker has not finished within
    /// the deadline".
    #[test]
    fn proxy_register_buffers_does_not_hang_on_shutdown_race() {
        let (proxy_tx, proxy_rx) = std_channel::<BlockDeviceProxy>();
        let stop = Arc::new(AtomicBool::new(false));
        let stop_clone = stop.clone();

        let service = std::thread::spawn(move || {
            let device = MockDevice::new(MockDeviceConfig::default());
            let metadata = ProxyMetadata {
                page_size: device.page_size(),
                capacity_pages: device.capacity_pages(),
                write_queue_depth: device.write_queue_depth(),
            };
            let (proxy, rx) = BlockDeviceProxy::new(metadata);
            proxy_tx.send(proxy).expect("send proxy back");
            run_proxy_service(device, rx, stop_clone);
        });

        let proxy = proxy_rx.recv().expect("receive proxy");

        let done = Arc::new(AtomicBool::new(false));
        let done_worker = done.clone();
        let worker_proxy = proxy.clone();
        let worker = std::thread::spawn(move || {
            let mut buf = vec![0u8; 4096];
            let r = worker_proxy.register_buffers(buf.as_mut_ptr(), buf.len());
            done_worker.store(true, Ordering::Release);
            r
        });

        // Flip stop concurrently to provoke the race.
        stop.store(true, Ordering::Release);

        // Watchdog: 5s is generous against a healthy build and
        // tight against an actual hang.
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !done.load(Ordering::Acquire) {
            if std::time::Instant::now() >= deadline {
                panic!("register_buffers hung past deadline");
            }
            std::thread::sleep(Duration::from_millis(10));
        }

        let result = worker.join().expect("worker thread");
        // Either outcome is acceptable: the contract is "do not
        // hang", not a specific status.
        match result {
            Ok(()) => {}
            Err(Error::Io(n)) if n == libc::EPIPE || n == libc::EIO => {}
            other => panic!("unexpected register_buffers result: {other:?}"),
        }

        drop(proxy);
        service.join().expect("service thread");
    }
}
