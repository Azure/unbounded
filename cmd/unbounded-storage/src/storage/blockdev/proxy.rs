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
}

impl BlockDeviceProxy {
    /// Build a proxy and the receiver half of its command channel.
    /// The receiver is consumed by [`run_proxy_service`] on the
    /// thread that owns the underlying device.
    pub fn new(metadata: ProxyMetadata) -> (Self, ProxyReceiver) {
        let (tx, rx) = channel();
        (
            Self {
                cmd_tx: tx,
                metadata,
            },
            ProxyReceiver(rx),
        )
    }
}

impl Clone for BlockDeviceProxy {
    fn clone(&self) -> Self {
        Self {
            cmd_tx: self.cmd_tx.clone(),
            metadata: self.metadata,
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
        // acceptable here.
        let ptr = NonNull::new(base).ok_or(Error::Io(libc::EINVAL))?;
        let reply = ReplySlot::new();
        self.cmd_tx
            .send(Command::RegisterBuffers {
                base: SendPtr(ptr),
                len,
                reply: reply.clone(),
            })
            .map_err(|_| Error::Io(libc::EPIPE))?;
        spin_block_on(reply.wait())
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
        reply.wait().await
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
        reply.wait().await
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
/// given proxy.
pub struct ProxyReceiver(Receiver<Command>);

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
    let device = Rc::new(device);
    let mut in_flight: Vec<InflightOp> = Vec::new();
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let rx = rx.0;

    loop {
        let mut disconnected = false;

        // 1. Drain pending commands into the in-flight set.
        loop {
            match rx.try_recv() {
                Ok(Command::Read {
                    lba,
                    ptr,
                    len,
                    reply,
                }) => {
                    let dev = Rc::clone(&device);
                    let fut: Pin<Box<dyn Future<Output = Result<(), Error>>>> =
                        Box::pin(async move {
                            // SAFETY: caller guarantees the buffer at
                            // `ptr` remains valid until `reply.set` is
                            // invoked (see module docs).
                            let slice = unsafe {
                                std::slice::from_raw_parts_mut(ptr.0.as_ptr(), len)
                            };
                            dev.read(lba, slice).await
                        });
                    in_flight.push(InflightOp { fut, reply });
                }
                Ok(Command::Write {
                    lba,
                    ptr,
                    len,
                    reply,
                }) => {
                    let dev = Rc::clone(&device);
                    let fut: Pin<Box<dyn Future<Output = Result<(), Error>>>> =
                        Box::pin(async move {
                            // SAFETY: same contract as Read; the
                            // service treats this slice as immutable.
                            let slice = unsafe {
                                std::slice::from_raw_parts(
                                    ptr.0.as_ptr() as *const u8,
                                    len,
                                )
                            };
                            dev.write(lba, slice).await
                        });
                    in_flight.push(InflightOp { fut, reply });
                }
                Ok(Command::RegisterBuffers { base, len, reply }) => {
                    let res = device.register_buffers(base.0.as_ptr(), len);
                    reply.set(res);
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    disconnected = true;
                    break;
                }
            }
        }

        // 2. Poll in-flight ops; complete any that finished.
        let mut i = 0;
        while i < in_flight.len() {
            let outcome = in_flight[i].fut.as_mut().poll(&mut cx);
            match outcome {
                Poll::Ready(result) => {
                    let op = in_flight.swap_remove(i);
                    op.reply.set(result);
                }
                Poll::Pending => i += 1,
            }
        }

        // 3. Drive the underlying device. A progress failure is
        //    treated as fatal: there is nothing useful we can do with
        //    in-flight requests except fail them and exit.
        if let Err(e) = device.progress() {
            let errno = errno_of(&e);
            for op in in_flight.drain(..) {
                op.reply.set(Err(Error::Io(errno)));
            }
            while let Ok(cmd) = rx.try_recv() {
                fail_command(cmd, errno);
            }
            eprintln!("uring proxy service: progress failed: {e}");
            return;
        }

        // 4. Exit conditions: shutdown requested AND nothing to do.
        let shutdown_requested = stop.load(Ordering::Acquire) || disconnected;
        if shutdown_requested && in_flight.is_empty() {
            while let Ok(cmd) = rx.try_recv() {
                fail_command(cmd, libc::EIO);
            }
            return;
        }

        // 5. Brief park so we do not pin a CPU when idle.
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

        // Submit the write, then immediately signal stop. The
        // service must drain the in-flight op before exiting.
        let write_fut = proxy.write(Lba(0), &payload);
        stop.store(true, Ordering::Release);
        block_on(write_fut).expect("write completes after stop");

        drop(proxy);
        service.join().expect("service thread");
    }
}
