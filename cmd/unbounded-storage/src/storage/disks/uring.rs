// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Production [`DiskTarget`] backed by a pinned storage core.
//!
//! Each disk gets one dedicated OS thread - a "storage core" - pinned
//! to the CPU the topology plan picked for it. On that thread we:
//!
//! 1. open the disk via
//!    [`UringDevice::open`](crate::storage::blockdev::UringDevice::open),
//!    which owns the full block-device lifecycle (io_uring setup flags,
//!    `O_DIRECT`, the `BLKGETSIZE64` capacity probe, and file
//!    registration) and hands back the disk-side ring plus a
//!    [`CoreLocalDevice`] - a `Send + Sync` [`BlockDevice`] carrying the
//!    registered `Fixed` index and disk geometry that resolves the ring
//!    from the thread-local registry at call time,
//! 2. install the ring into the registry via
//!    [`set_current_storage_ring`] so the device (and the engine built
//!    on it) reach the ring without it being threaded through every
//!    signature,
//! 3. drive [`StorageEngine::open`] interleaved with ring progress so
//!    the engine's recovery I/O is served by the same loop,
//! 4. build a [`PageService`] over the engine plus the receiver half
//!    of a fresh [`PageChannel`], hand the [`PageChannel`] back to the
//!    caller via a oneshot, and
//! 5. drive [`StorageEngine::run_mutator`], [`PageService::poll_once`],
//!    and ring progress on the same loop until shutdown.
//!
//! Shutdown is initiated by dropping the [`UringDiskHandle`], which
//! sets the stop flag and joins. Once shutdown is observed the thread
//! stops admitting new page ops from the channel, calls
//! [`StorageEngine::close_mutator`], fails any in-flight or queued page
//! ops, waits for in-flight ring I/O to drain, clears the thread-local
//! ring, and drops the ring on its own stack. Not admitting new work
//! after the mutator is closing is what keeps the join from hanging
//! (see [`run_core_loop`]).
//!
//! Shard cores never touch the engine or its device directly: every
//! page read/write is shipped to this storage core over the
//! [`PageChannel`] and executed by the [`PageService`] against the
//! engine. That keeps the engine's device on the thread that built
//! its ring while letting the routing layer stay non-generic.
//!
//! [`PageChannel`]: crate::storage::PageChannel
//! [`PageService`]: crate::storage::PageService

use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::task::{Context, Poll};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use crate::config::schema::{DiskKind, DiskSpec};
use crate::ring::{StorageRingConfig, clear_current_storage_ring, set_current_storage_ring};
use crate::runtime::{PinnedRuntime, WorkerSpec, noop_waker};
use crate::storage::blockdev::{
    BlockDevice, CoreLocalDevice, OpenDisk, UringDevice, provision_file,
};
use crate::storage::types::Error;
use crate::storage::{EngineConfig, PageChannel, PageService, StorageEngine};
use crate::topology::DiskCpuSlot;

use super::{DiskError, DiskTarget};

/// Production [`DiskTarget`] that runs each disk on a pinned storage
/// core whose engine talks to a [`CoreLocalDevice`]. Thread spawning
/// and pinning are routed through the shared [`PinnedRuntime`] that
/// owns all placement; there is no per-disk throwaway runtime.
pub struct UringDiskTarget {
    runtime: std::sync::Arc<PinnedRuntime>,
}

impl UringDiskTarget {
    pub fn new(runtime: std::sync::Arc<PinnedRuntime>) -> Self {
        Self { runtime }
    }
}

/// Owns the storage-core thread spawned by [`UringDiskTarget::open`].
/// Dropping the handle sets the stop flag and joins the thread, which
/// is what drives shutdown of the underlying engine and ring.
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
    type Handle = UringDiskHandle;

    fn open(
        &self,
        spec: &DiskSpec,
        pin: Option<DiskCpuSlot>,
    ) -> Result<(UringDiskHandle, PageChannel), DiskError> {
        let engine_cfg = engine_config_from(spec);
        // The device page size must equal the engine's LBA unit. The
        // allocator counts btree pages (`alloc_contig` LBAs are btree-page
        // indices), and the device turns an LBA into a byte offset as
        // `lba * device_page_size` and rejects I/O whose length is not a
        // multiple of it. That unit is `btree_page_bytes`, NOT the cache
        // page size `page_size_bytes` (which can be far larger, e.g. 2 MiB
        // against a 4 KiB btree page): using the cache page size here makes
        // every 4 KiB btree/meta I/O fail EINVAL and skews all byte offsets.
        let page_size = device_page_size(&engine_cfg);
        let mut ring_cfg = StorageRingConfig::default();
        if let Some(qd) = spec.queue_depth {
            ring_cfg.queue_depth = qd;
        }

        // A file-backed disk runs buffered I/O on a regular file, so the
        // hardware-only ring flags (IOPOLL, and the O_DIRECT it implies,
        // plus SINGLE_ISSUER/DEFER_TASKRUN) must be stripped; this mirrors
        // StorageRingConfig::test_local. The backing file is created and
        // sized here, on the supervisor thread, before the storage core
        // opens it (capacity comes from the file length). Size validity is
        // guaranteed by config validation.
        if spec.kind == DiskKind::File {
            ring_cfg.iopoll = false;
            ring_cfg.single_issuer = false;
            ring_cfg.defer_taskrun = false;
            let size = spec
                .size
                .expect("file disk size is validated at config load")
                .bytes() as u64;
            if let Err(e) = provision_file(&spec.path, size) {
                return Err(DiskError::Open(format!(
                    "provision file {}: {e}",
                    spec.path.display()
                )));
            }
        }

        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let path = spec.path.clone();
        let name = format!("ub-disk-{}", path.display());
        let (ready_tx, ready_rx) = mpsc::sync_channel::<Result<PageChannel, String>>(1);

        let body = move || {
            run_storage_core(path, ring_cfg, page_size, engine_cfg, stop_thr, ready_tx);
        };

        // Route thread spawning + pinning through the shared runtime.
        // When the plan supplied a slot we pin the storage core to its
        // CPU and NUMA node; otherwise it runs unpinned.
        let spec = pin.map(|s| WorkerSpec::new(s.cpu, s.numa));
        let join: JoinHandle<()> = self.runtime.spawn_placed(spec, &name, Box::new(body));

        match ready_rx.recv() {
            Ok(Ok(channel)) => Ok((
                UringDiskHandle {
                    stop,
                    join: Some(join),
                },
                channel,
            )),
            Ok(Err(msg)) => {
                stop.store(true, Ordering::Release);
                let _ = join.join();
                Err(DiskError::Open(msg))
            }
            Err(_) => {
                stop.store(true, Ordering::Release);
                let _ = join.join();
                Err(DiskError::Open("storage core exited without status".into()))
            }
        }
    }
}

/// Body of the pinned storage-core thread. Opens the disk via the
/// blockdev layer, installs the returned ring into the thread-local
/// registry, opens the engine, then drives `run_mutator`, the
/// [`PageService`], and `ring.progress()` interleaved until shutdown.
fn run_storage_core(
    path: PathBuf,
    ring_cfg: StorageRingConfig,
    page_size: usize,
    engine_cfg: EngineConfig,
    stop: Arc<AtomicBool>,
    ready_tx: mpsc::SyncSender<Result<PageChannel, String>>,
) {
    // Phase 1: open the disk on this core. The blockdev layer owns the
    // full device lifecycle - io_uring setup flags, O_DIRECT, the
    // BLKGETSIZE64 capacity probe, and file registration - and hands
    // back the ring (to install + drive here) plus the Send + Sync
    // device the engine is built on.
    let OpenDisk {
        device,
        ring,
        // Held for the storage core's lifetime: the ring addresses this
        // fd by its registered Fixed index, so it must outlive the ring.
        file: _disk_file,
    } = match UringDevice::open(&path, ring_cfg, ring_cfg.iopoll, page_size) {
        Ok(d) => d,
        Err(e) => {
            let _ = ready_tx.send(Err(e.to_string()));
            return;
        }
    };
    let ring = Rc::new(ring);
    let device = Arc::new(device);

    // Phase 2: install the ring so the device and engine can reach it.
    // This MUST run on this pinned storage-core thread: the device
    // resolves its ring from the thread-local registry, so installing it
    // anywhere else would strand every off-thread I/O with ENXIO.
    set_current_storage_ring(ring.clone());

    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    // Phase 3: open the engine while serving its recovery I/O.
    let mut open_fut: Pin<Box<dyn Future<Output = Result<StorageEngine<CoreLocalDevice>, Error>>>> =
        Box::pin(StorageEngine::open(device.clone(), engine_cfg));
    let engine_arc: Arc<StorageEngine<CoreLocalDevice>> = loop {
        match open_fut.as_mut().poll(&mut cx) {
            Poll::Ready(Ok(eng)) => break Arc::new(eng),
            Poll::Ready(Err(e)) => {
                let _ = ready_tx.send(Err(format!("engine open: {e}")));
                clear_current_storage_ring();
                return;
            }
            Poll::Pending => {}
        }
        if let Err(e) = ring.progress() {
            let _ = ready_tx.send(Err(format!("ring progress during open: {e}")));
            clear_current_storage_ring();
            return;
        }
        if stop.load(Ordering::Acquire) {
            let _ = ready_tx.send(Err("shutdown requested during open".into()));
            clear_current_storage_ring();
            return;
        }
        thread::sleep(Duration::from_micros(100));
    };
    drop(open_fut);

    // Phase 4: build the page service over the engine and hand a
    // fresh channel back to the caller. From here, all shard-side page
    // ops arrive over `rx` and are executed by `service` against the
    // engine on this thread.
    let (channel, rx) = PageChannel::new();
    let mut service = PageService::new(engine_arc.clone(), rx);
    let _ = ready_tx.send(Ok(channel));

    // Phase 5: drive `run_mutator`, the page service, and ring
    // progress until shutdown is requested and the mutator, the
    // service, and the ring have all drained. The teardown invariant
    // (no new page ops admitted once shutdown is observed) lives in
    // `run_core_loop`.
    let mut mutator_fut: Pin<Box<dyn Future<Output = ()>>> =
        Box::pin(engine_arc.clone().run_mutator());
    run_core_loop(
        &engine_arc,
        &mut service,
        mutator_fut.as_mut(),
        &*ring,
        &stop,
        &mut cx,
        || thread::sleep(Duration::from_micros(100)),
    );

    // Phase 6: tear down. Drop the mutator future first so any borrow
    // of the engine is released, then clear the thread-local ring.
    drop(mutator_fut);
    drop(service);
    clear_current_storage_ring();
}

/// The progress surface of a storage ring the core loop drives: push
/// queued SQEs and reap completions, plus the count of ops still in
/// flight. Abstracted behind a trait so [`run_core_loop`] can be
/// exercised against an in-memory fake without a real io_uring.
trait RingDriver {
    fn progress(&self) -> Result<(), Error>;
    fn in_flight(&self) -> u32;
}

impl RingDriver for crate::ring::StorageRing {
    fn progress(&self) -> Result<(), Error> {
        crate::ring::StorageRing::progress(self)
    }

    fn in_flight(&self) -> u32 {
        crate::ring::StorageRing::in_flight(self)
    }
}

/// Drive the storage core's steady state and shutdown to completion.
///
/// In steady state the loop polls the mutator, admits and advances
/// page ops via [`PageService::poll_once`], and drives ring progress.
///
/// Shutdown (stop flag set, or every [`PageChannel`] dropped) flips the
/// loop into teardown. The teardown ordering is the crux of avoiding a
/// join-forever deadlock:
///
/// 1. Stop calling [`PageService::poll_once`]. `poll_once` is the only
///    thing that promotes a queued `ReadPage`/`WritePage` into the
///    in-flight set. A write promoted after the mutator is closing
///    parks on a closed mutator queue forever, so it would never
///    retire and `has_inflight()` would stay true, wedging the break
///    condition and hanging the thread join.
/// 2. [`StorageEngine::close_mutator`] lets `run_mutator` drain its
///    queue and return; because no new writes are admitted, the queue
///    is bounded and the mutator terminates.
/// 3. [`PageService::fail_all`] retires every op already in flight (and
///    drains the channel) with `EIO`, and [`PageService::mark_dead`]
///    flips `service_alive` so a client whose send races this point
///    resolves `EIO` via `AliveAwareWait` rather than parking forever.
/// 4. Each subsequent iteration only [`PageService::drain_pending`]s
///    late arrivals (failing them with `EIO`); it never admits them.
///
/// The loop terminates once the mutator has drained, the in-flight set
/// is empty (guaranteed, since teardown never admits), and the ring has
/// no outstanding I/O.
fn run_core_loop<B, R>(
    engine: &Arc<StorageEngine<B>>,
    service: &mut PageService<B>,
    mut mutator_fut: Pin<&mut (dyn Future<Output = ()> + '_)>,
    ring: &R,
    stop: &AtomicBool,
    cx: &mut Context<'_>,
    mut idle: impl FnMut(),
) where
    B: BlockDevice + 'static,
    R: RingDriver,
{
    let eio = crate::bufferpool::Error::Io(libc::EIO);
    let mut close_signaled = false;
    let mut mutator_done = false;
    loop {
        if !mutator_done {
            if let Poll::Ready(()) = mutator_fut.as_mut().poll(cx) {
                mutator_done = true;
            }
        }

        if close_signaled {
            // Teardown: never admit new work. Only fail late arrivals
            // so a send that raced shutdown unparks with `EIO` instead
            // of waiting on a reply slot nobody will set.
            service.drain_pending(eio.clone());
        } else {
            service.poll_once(cx);
        }

        if let Err(e) = ring.progress() {
            eprintln!("storage core: ring progress failed: {e}");
            // A progress error in steady state would otherwise skip the
            // teardown the shutdown path performs, stranding any client
            // parked in `AliveAwareWait`: its reply slot is never set and
            // `service_alive` stays true, so it parks forever. Run the
            // same teardown here (guarded so we never double-run it after
            // a shutdown-driven teardown) so every parked or late client
            // resolves with `EIO` before we break.
            if !close_signaled {
                engine.close_mutator();
                service.fail_all(eio.clone());
                service.mark_dead();
            }
            break;
        }

        // The channel disconnecting (all `PageChannel`s dropped) is an
        // implicit shutdown signal, same as the stop flag.
        let shutdown = stop.load(Ordering::Acquire) || service.channel_disconnected();
        if shutdown && !close_signaled {
            engine.close_mutator();
            service.fail_all(eio.clone());
            service.mark_dead();
            close_signaled = true;
        }

        // Quiescent: mutator drained, no page op in flight (guaranteed
        // once `close_signaled`, since teardown never admits), and no
        // ring I/O outstanding.
        if mutator_done && close_signaled && !service.has_inflight() && ring.in_flight() == 0 {
            break;
        }
        idle();
    }
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

/// Device page size to open the disk with: the engine's LBA unit.
///
/// The allocator counts btree pages and the device converts an LBA to a
/// byte offset as `lba * device_page_size`, so the device page size must
/// be `btree_page_bytes`, not the (potentially much larger) cache page
/// size `page_size_bytes`.
fn device_page_size(cfg: &EngineConfig) -> usize {
    cfg.btree_page_bytes
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::{Error as BpError, StripeKey};
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use std::cell::{Cell, RefCell};

    /// Spin a future to completion on the same noop-waker pattern the
    /// storage core uses. Bounded so a stuck future fails loudly.
    fn block_on<F: Future>(mut fut: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        // SAFETY: `fut` is owned here and never moved after pinning.
        let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
        for _ in 0..1_000_000 {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
            std::thread::yield_now();
        }
        panic!("block_on: future did not complete within spin budget");
    }

    /// In-memory [`RingDriver`] for the core-loop tests. `in_flight`
    /// reports whatever the test parks in the shared cell so the test
    /// can gate the loop's break condition. `progress` succeeds unless
    /// `fail` is set, in which case it returns an error so the test can
    /// simulate a steady-state ring failure.
    struct FakeRing {
        in_flight: Rc<Cell<u32>>,
        fail: Rc<Cell<bool>>,
    }

    impl RingDriver for FakeRing {
        fn progress(&self) -> Result<(), Error> {
            if self.fail.get() {
                return Err(Error::Io(libc::EIO));
            }
            Ok(())
        }

        fn in_flight(&self) -> u32 {
            self.in_flight.get()
        }
    }

    fn open_engine() -> Arc<StorageEngine<MockDevice>> {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 256,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096;
        cfg.btree_page_bytes = 4096;
        cfg.bypass_admission = true;
        Arc::new(block_on(StorageEngine::open(device, cfg)).expect("engine open"))
    }

    /// Regression for H3: a `WritePage` that arrives AFTER shutdown is
    /// signaled must be failed with `EIO` and never promoted into the
    /// in-flight set, and the run loop must terminate in bounded time.
    ///
    /// Before the fix, teardown kept calling `poll_once`, which pulled
    /// the late command into `in_flight` and polled its engine future
    /// against an already-closed mutator. That future could never
    /// complete, so `has_inflight()` stayed true forever and the loop
    /// (hence the thread join) hung.
    #[test]
    fn teardown_fails_racing_command_without_hanging() {
        let engine = open_engine();
        let (channel, rx) = PageChannel::new();
        let mut service = PageService::new(engine.clone(), rx);

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut mutator_fut: Pin<Box<dyn Future<Output = ()>>> =
            Box::pin(engine.clone().run_mutator());

        // Shutdown is already requested before the loop runs, so the
        // first iteration transitions straight into teardown.
        let stop = AtomicBool::new(true);
        let ring_in_flight = Rc::new(Cell::new(1u32));
        let ring = FakeRing {
            in_flight: ring_in_flight.clone(),
            fail: Rc::new(Cell::new(false)),
        };

        // The racing write. Polling it the first time enqueues the
        // command onto the channel and parks; we hold it in a cell so
        // the idle hook can inject it mid-teardown.
        let src_buf = vec![0u8; 4096];
        let src = std::ptr::slice_from_raw_parts(src_buf.as_ptr(), src_buf.len());
        let racing: Pin<Box<dyn Future<Output = Result<(), BpError>>>> =
            Box::pin(channel.write_page(StripeKey([7; 32]), 0, src));
        let racing = RefCell::new(racing);

        let injected = Cell::new(false);
        let iters = Cell::new(0u32);
        // Records the racing write's result the first time it resolves
        // so we never poll a completed future twice.
        let outcome: RefCell<Option<Result<(), BpError>>> = RefCell::new(None);
        run_core_loop(
            &engine,
            &mut service,
            mutator_fut.as_mut(),
            &ring,
            &stop,
            &mut cx,
            || {
                let n = iters.get();
                iters.set(n + 1);
                assert!(
                    n < 10_000,
                    "run_core_loop failed to terminate (teardown deadlock?)"
                );
                if !injected.get() {
                    injected.set(true);
                    // Enqueue the racing command now, AFTER teardown has
                    // already run `fail_all`/`mark_dead` on this same
                    // iteration. The fixed loop must NOT promote it.
                    let w = noop_waker();
                    let mut c = Context::from_waker(&w);
                    if let Poll::Ready(v) = racing.borrow_mut().as_mut().poll(&mut c) {
                        *outcome.borrow_mut() = Some(v);
                    }
                    // Let the loop reach its break condition.
                    ring_in_flight.set(0);
                } else {
                    let still_pending = outcome.borrow().is_none();
                    if still_pending {
                        // Re-poll so a reply set by `drain_pending` this
                        // iteration is observed.
                        let w = noop_waker();
                        let mut c = Context::from_waker(&w);
                        let polled = racing.borrow_mut().as_mut().poll(&mut c);
                        if let Poll::Ready(v) = polled {
                            *outcome.borrow_mut() = Some(v);
                        }
                    }
                }
            },
        );

        // The late command was failed, not promoted: nothing in flight.
        assert!(
            !service.has_inflight(),
            "racing command must not be promoted into in_flight during teardown",
        );

        // And the caller's future resolves with EIO rather than hanging.
        let result = {
            let mut o = outcome.borrow_mut();
            match o.take() {
                Some(v) => v,
                None => {
                    let w = noop_waker();
                    let mut c = Context::from_waker(&w);
                    match racing.borrow_mut().as_mut().poll(&mut c) {
                        Poll::Ready(v) => v,
                        Poll::Pending => panic!("racing write never resolved after teardown"),
                    }
                }
            }
        };
        assert!(
            matches!(result, Err(BpError::Io(n)) if n == libc::EIO),
            "expected EIO for racing write, got {result:?}",
        );

        drop(mutator_fut);
        drop(service);
    }

    /// A clean shutdown with no outstanding work terminates promptly.
    #[test]
    fn clean_shutdown_terminates() {
        let engine = open_engine();
        let (_channel, rx) = PageChannel::new();
        let mut service = PageService::new(engine.clone(), rx);

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut mutator_fut: Pin<Box<dyn Future<Output = ()>>> =
            Box::pin(engine.clone().run_mutator());

        let stop = AtomicBool::new(true);
        let ring = FakeRing {
            in_flight: Rc::new(Cell::new(0u32)),
            fail: Rc::new(Cell::new(false)),
        };

        let iters = Cell::new(0u32);
        run_core_loop(
            &engine,
            &mut service,
            mutator_fut.as_mut(),
            &ring,
            &stop,
            &mut cx,
            || {
                let n = iters.get();
                iters.set(n + 1);
                assert!(n < 10_000, "clean shutdown failed to terminate");
            },
        );

        assert!(!service.has_inflight());
        drop(mutator_fut);
        drop(service);
    }

    /// Regression for BUG 1: the device must be opened with the engine's
    /// LBA unit (`btree_page_bytes`), not the cache page size
    /// (`page_size_bytes`). With production defaults these differ
    /// (2 MiB vs 4 KiB); selecting the cache page size makes every btree
    /// or meta I/O fail EINVAL and skews all byte offsets by 512x.
    #[test]
    fn device_page_size_is_btree_page_unit_not_cache_page() {
        // Production defaults: page_size_bytes = 2 MiB, btree_page_bytes
        // = 4096. They must not be conflated.
        let cfg = EngineConfig::default();
        assert_ne!(
            cfg.page_size_bytes, cfg.btree_page_bytes,
            "test premise: defaults must differ to catch the bug",
        );
        assert_eq!(
            device_page_size(&cfg),
            cfg.btree_page_bytes,
            "device page size must equal the engine's LBA unit (btree_page_bytes)",
        );
        assert_ne!(
            device_page_size(&cfg),
            cfg.page_size_bytes,
            "device page size must NOT be the cache page size (page_size_bytes)",
        );

        // Independence from the cache page size: even with an oversized
        // cache page, the device unit tracks the btree page.
        let mut spec_cfg = EngineConfig::default();
        spec_cfg.page_size_bytes = 8 * 1024 * 1024;
        spec_cfg.btree_page_bytes = 4096;
        assert_eq!(device_page_size(&spec_cfg), 4096);
    }

    /// Regression for BUG 2: a `ring.progress()` error in STEADY STATE
    /// (before shutdown is signaled) must still run teardown so a client
    /// parked in `AliveAwareWait` resolves with `EIO` instead of parking
    /// forever.
    ///
    /// Before the fix, the loop logged and broke without calling
    /// `close_mutator`/`fail_all`/`mark_dead`, leaving `service_alive`
    /// true and the parked client's reply slot unset.
    #[test]
    fn steady_state_ring_error_fails_parked_client() {
        let engine = open_engine();
        let (channel, rx) = PageChannel::new();
        let mut service = PageService::new(engine.clone(), rx);

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut mutator_fut: Pin<Box<dyn Future<Output = ()>>> =
            Box::pin(engine.clone().run_mutator());

        // NOT shutting down: steady state. The teardown must be driven by
        // the ring error, not by the stop flag or a disconnected channel.
        let stop = AtomicBool::new(false);
        let ring = FakeRing {
            in_flight: Rc::new(Cell::new(0u32)),
            fail: Rc::new(Cell::new(true)),
        };

        // Park a client write. Polling it once enqueues onto the channel
        // and parks in `AliveAwareWait`; the channel is kept alive so the
        // loop does not observe a disconnect-driven shutdown.
        let src_buf = vec![0u8; 4096];
        let src = std::ptr::slice_from_raw_parts(src_buf.as_ptr(), src_buf.len());
        let mut parked: Pin<Box<dyn Future<Output = Result<(), BpError>>>> =
            Box::pin(channel.write_page(StripeKey([9; 32]), 0, src));
        {
            let w = noop_waker();
            let mut c = Context::from_waker(&w);
            assert!(
                matches!(parked.as_mut().poll(&mut c), Poll::Pending),
                "parked write should not resolve before teardown",
            );
        }

        run_core_loop(
            &engine,
            &mut service,
            mutator_fut.as_mut(),
            &ring,
            &stop,
            &mut cx,
            || panic!("loop must break on the ring error, not idle"),
        );

        // Teardown ran despite steady state: nothing left in flight and
        // the parked client resolves with EIO rather than hanging.
        assert!(
            !service.has_inflight(),
            "ring-error teardown must retire in-flight ops",
        );
        let result = {
            let w = noop_waker();
            let mut c = Context::from_waker(&w);
            match parked.as_mut().poll(&mut c) {
                Poll::Ready(v) => v,
                Poll::Pending => panic!("parked client never resolved after ring-error teardown"),
            }
        };
        assert!(
            matches!(result, Err(BpError::Io(n)) if n == libc::EIO),
            "expected EIO for parked client, got {result:?}",
        );

        drop(mutator_fut);
        drop(service);
    }
}
