// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Production [`DiskTarget`] backed by [`UringBlockDevice`].
//!
//! [`UringBlockDevice`] is `!Send` and `!Sync`: its `io_uring` handle
//! must stay on the thread that opened it. This module spawns one
//! dedicated thread per disk; on that thread we:
//!
//! 1. open the [`UringBlockDevice`],
//! 2. wrap it behind a [`BlockDeviceProxy`] + [`ProxyService`] pair,
//!    so a `Send + Sync` clone of the device can travel to other
//!    threads,
//! 3. drive [`StorageEngine::open`] interleaved with the proxy service
//!    so that the engine's own recovery I/O can be served by the same
//!    loop,
//! 4. hand the resulting `Arc<StorageEngine<BlockDeviceProxy>>` back
//!    to the caller via a oneshot, and
//! 5. drive [`StorageEngine::run_mutator`] and the proxy service
//!    together until shutdown is signalled.
//!
//! Shutdown is initiated by dropping the [`UringDiskHandle`], which
//! sets the stop flag and joins. The thread then calls
//! [`StorageEngine::close_mutator`], waits for in-flight I/O to
//! drain, fails any commands still in the proxy channel, and finally
//! drops the [`UringBlockDevice`] on its own stack.

use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use crate::config::schema::DiskSpec;
use crate::storage::blockdev::{
    BlockDevice, BlockDeviceProxy, ProxyMetadata, ProxyService, UringBlockDevice, UringConfig,
};
use crate::storage::types::Error;
use crate::storage::{EngineConfig, StorageEngine};

use super::{DiskError, DiskTarget};

/// Production [`DiskTarget`] that opens a real [`UringBlockDevice`]
/// on its own progress thread and exposes it through a
/// `Send + Sync` [`BlockDeviceProxy`].
pub struct UringDiskTarget;

/// Owns the disk thread spawned by [`UringDiskTarget::open`]. Dropping
/// the handle sets the stop flag and joins the thread, which is what
/// drives shutdown of the underlying engine and device.
pub struct UringDiskHandle {
    stop: Arc<AtomicBool>,
    join: Option<JoinHandle<()>>,
}

impl Drop for UringDiskHandle {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(j) = self.join.take() {
            let _ = j.join();
        }
    }
}

impl DiskTarget for UringDiskTarget {
    type Device = BlockDeviceProxy;
    type Handle = UringDiskHandle;

    fn open(
        &self,
        spec: &DiskSpec,
        _cpu_hint: Option<usize>,
    ) -> Result<(UringDiskHandle, Arc<StorageEngine<BlockDeviceProxy>>), DiskError> {
        // TODO(phase6): pin the disk thread to `_cpu_hint`. The
        // hint is now computed per-disk from the spec's `numa`
        // field via the registry's `CpuPlacer`; no pinning is
        // wired in yet.
        let engine_cfg = engine_config_from(spec);
        let mut uring_cfg = UringConfig::default();
        if let Some(qd) = spec.queue_depth {
            uring_cfg.queue_depth = qd;
        }
        // `UringConfig::page_size` must match the engine's
        // `page_size_bytes`; both feed the same LBA arithmetic.
        uring_cfg.page_size = engine_cfg.page_size_bytes;

        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let path = spec.path.clone();
        let (ready_tx, ready_rx) =
            mpsc::sync_channel::<Result<Arc<StorageEngine<BlockDeviceProxy>>, String>>(1);
        let join = thread::Builder::new()
            .name(format!("ub-disk-{}", path.display()))
            .spawn(move || run_disk_thread(path, uring_cfg, engine_cfg, stop_thr, ready_tx))
            .map_err(|e| DiskError::Open(format!("spawn disk thread: {e}")))?;
        match ready_rx.recv() {
            Ok(Ok(engine_arc)) => Ok((
                UringDiskHandle {
                    stop,
                    join: Some(join),
                },
                engine_arc,
            )),
            Ok(Err(msg)) => {
                stop.store(true, Ordering::Release);
                let _ = join.join();
                Err(DiskError::Open(msg))
            }
            Err(_) => {
                stop.store(true, Ordering::Release);
                let _ = join.join();
                Err(DiskError::Open("disk thread exited without status".into()))
            }
        }
    }
}

/// Mini-executor that runs on the device-owning thread. Drives the
/// proxy service alongside the engine's open / run_mutator futures
/// using a single noop waker; cooperative polling lets all three make
/// progress under one cargo of progress() calls into io_uring.
fn run_disk_thread(
    path: PathBuf,
    uring_cfg: UringConfig,
    engine_cfg: EngineConfig,
    stop: Arc<AtomicBool>,
    ready_tx: mpsc::SyncSender<Result<Arc<StorageEngine<BlockDeviceProxy>>, String>>,
) {
    // Phase 1: open the underlying device on this thread.
    let dev = match UringBlockDevice::open(&path, uring_cfg) {
        Ok(d) => d,
        Err(e) => {
            let _ = ready_tx.send(Err(format!("uring open: {e:?}")));
            return;
        }
    };
    let metadata = ProxyMetadata {
        page_size: dev.page_size(),
        capacity_pages: dev.capacity_pages(),
        write_queue_depth: dev.write_queue_depth(),
    };
    let (proxy, rx) = BlockDeviceProxy::new(metadata);
    let mut svc = ProxyService::new(dev, rx);

    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    // Phase 2: open the engine through the proxy while servicing
    // whatever recovery I/O it issues against itself.
    let mut engine_open_fut: Pin<
        Box<dyn Future<Output = Result<StorageEngine<BlockDeviceProxy>, Error>>>,
    > = Box::pin(StorageEngine::open(Arc::new(proxy.clone()), engine_cfg));
    let engine_arc: Arc<StorageEngine<BlockDeviceProxy>> = loop {
        if let Err(e) = svc.poll_once(&mut cx) {
            let _ = ready_tx.send(Err(format!("proxy progress during engine open: {e}")));
            svc.fail_all(libc::EIO);
            return;
        }
        match engine_open_fut.as_mut().poll(&mut cx) {
            Poll::Ready(Ok(eng)) => break Arc::new(eng),
            Poll::Ready(Err(e)) => {
                let _ = ready_tx.send(Err(format!("engine open: {e}")));
                svc.fail_all(libc::EIO);
                return;
            }
            Poll::Pending => {}
        }
        if stop.load(Ordering::Acquire) {
            let _ = ready_tx.send(Err("shutdown requested during open".into()));
            svc.fail_all(libc::EIO);
            return;
        }
        thread::sleep(Duration::from_micros(100));
    };

    // Phase 3: hand the engine back. Drop the local proxy clone so
    // that the engine's internal `Arc<BlockDeviceProxy>` (and any
    // clone the caller may make) are the only senders alive; that
    // way the channel disconnects naturally when the engine drops.
    let _ = ready_tx.send(Ok(engine_arc.clone()));
    drop(proxy);

    // Phase 4: drive `run_mutator` and the proxy service together
    // until shutdown is requested and the mutator has drained.
    let mut mutator_fut: Pin<Box<dyn Future<Output = ()>>> =
        Box::pin(engine_arc.clone().run_mutator());
    let mut close_signaled = false;
    let mut mutator_done = false;
    loop {
        if let Err(e) = svc.poll_once(&mut cx) {
            eprintln!("uring disk thread: proxy progress failed: {e}");
            break;
        }
        if !mutator_done {
            if let Poll::Ready(()) = mutator_fut.as_mut().poll(&mut cx) {
                mutator_done = true;
            }
        }
        if stop.load(Ordering::Acquire) && !close_signaled {
            engine_arc.close_mutator();
            close_signaled = true;
        }
        // Quiescent: mutator has drained and no proxy I/O is in
        // flight. Exit if shutdown was requested or the proxy channel
        // disconnected (no proxy clones left to send work).
        if mutator_done && !svc.has_inflight() && (close_signaled || svc.channel_disconnected()) {
            break;
        }
        thread::sleep(Duration::from_micros(100));
    }

    // Drain any commands that arrived after shutdown so producers get
    // a clean error instead of dropping their reply slots silently.
    svc.fail_all(libc::EIO);
}

/// Build an [`EngineConfig`] from a [`DiskSpec`]. Production defaults
/// come from [`EngineConfig::default`]; the spec only overrides what
/// the operator chose to expose: `page_size_bytes`, `bypass_admission`,
/// and `skip_recovery_scan_if_no_meta`.
fn engine_config_from(spec: &DiskSpec) -> EngineConfig {
    let mut cfg = EngineConfig::default();
    if let Some(p) = spec.page_size_bytes {
        cfg.page_size_bytes = p;
    }
    cfg.bypass_admission = spec.bypass_admission;
    cfg.skip_recovery_scan_if_no_meta = spec.skip_recovery_scan_if_no_meta;
    cfg
}

fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    unsafe { Waker::from_raw(raw()) }
}
