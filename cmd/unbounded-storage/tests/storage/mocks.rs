// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Sim-aware [`BlockDevice`] mock for the storage DST.
//!
//! Wraps a production [`MockDevice`] (which is synchronous and
//! deliberately knowledge-free of the framework) with the same
//! `yield_n` + PRNG-driven fault injection pattern the bufferpool
//! DST uses for its transport and blockstore mocks. Per-area knobs
//! (delay bound, fault rate) live on a local `MockSimConfig` held
//! behind an `Rc`, never on the framework's [`SimState`].

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::rc::Rc;

use rand::Rng;
use unbounded_storage::storage::blockdev::{BlockDevice, MockDevice, MockDeviceConfig};
use unbounded_storage::storage::types::{Error, Lba};

use crate::framework::executor::{with_sim, yield_n, yield_once};

/// Storage-DST simulation knobs. Mirrors the bufferpool DST's
/// `MockSimConfig` so the two harnesses read the same way side by
/// side.
#[derive(Default)]
pub struct MockSimConfig {
    /// Upper bound on per-I/O pend count; the actual count per call
    /// is drawn from the framework PRNG.
    pub max_io_delay: Cell<u32>,
    /// Probability in `[0, 100]` that an I/O returns an `Io` error
    /// after its delay. `0` is the happy-path regime.
    pub io_fault_rate: Cell<u32>,
    /// Probability in `[0, 100]` that a successful read silently
    /// corrupts its first byte (bitrot simulation). The engine's
    /// per-page xxh3 checksum must catch this and convert the read
    /// into a miss; the bytes must never reach the caller.
    pub read_corrupt_rate: Cell<u32>,
}

impl MockSimConfig {
    pub fn new() -> Rc<Self> {
        Rc::new(Self::default())
    }
}

fn draw_delay(cfg: &MockSimConfig) -> u32 {
    let max = cfg.max_io_delay.get();
    if max == 0 {
        0
    } else {
        with_sim(|s| s.rng.gen_range(0..=max))
    }
}

fn draw_fault(cfg: &MockSimConfig) -> bool {
    let rate = cfg.io_fault_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100))
}

fn draw_corrupt(cfg: &MockSimConfig) -> bool {
    let rate = cfg.read_corrupt_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100))
}

/// Sim-aware wrapper over [`MockDevice`]. Every async method first
/// pulls a delay (and optional fault decision) from the framework
/// PRNG, then `yield_n`s before delegating to the inner mock. This
/// gives the executor permission to interleave concurrent I/Os while
/// keeping the deterministic `(seed, workload)` -> schedule guarantee.
pub struct SimBlockDevice {
    inner: MockDevice,
    cfg: Rc<MockSimConfig>,
    reads: Cell<u64>,
    writes: Cell<u64>,
    durable_writes: Cell<u64>,
    io_errors: Cell<u64>,
    corruptions_injected: Cell<u64>,
    inflight: Cell<u32>,
    max_inflight: Cell<u32>,
    read_pause: RefCell<Option<Rc<ReadPause>>>,
    /// Volatile write cache model, expressed as an undo log. A
    /// non-durable (best-effort) write lands in the device's volatile
    /// cache: before it overwrites a page's stable content we snapshot
    /// the prior *durable* bytes here (once per page, until the next
    /// durable write or crash). A durable (RWF_DSYNC/FUA) write writes
    /// through and clears the page's undo entry so it survives. A
    /// `crash()` restores every logged page to its snapshot, dropping
    /// all volatile writes exactly as a power loss would.
    volatile: RefCell<HashMap<u64, Vec<u8>>>,
    crashes: Cell<u64>,
}

impl SimBlockDevice {
    pub fn new(device_cfg: MockDeviceConfig, sim_cfg: Rc<MockSimConfig>) -> Self {
        Self {
            inner: MockDevice::new(device_cfg),
            cfg: sim_cfg,
            reads: Cell::new(0),
            writes: Cell::new(0),
            durable_writes: Cell::new(0),
            io_errors: Cell::new(0),
            corruptions_injected: Cell::new(0),
            inflight: Cell::new(0),
            max_inflight: Cell::new(0),
            read_pause: RefCell::new(None),
            volatile: RefCell::new(HashMap::new()),
            crashes: Cell::new(0),
        }
    }

    pub fn reads(&self) -> u64 {
        self.reads.get()
    }
    pub fn writes(&self) -> u64 {
        self.writes.get()
    }
    pub fn durable_writes(&self) -> u64 {
        self.durable_writes.get()
    }
    pub fn io_errors(&self) -> u64 {
        self.io_errors.get()
    }
    pub fn corruptions_injected(&self) -> u64 {
        self.corruptions_injected.get()
    }
    pub fn max_inflight(&self) -> u32 {
        self.max_inflight.get()
    }

    /// Number of volatile (non-durable) pages currently buffered in the
    /// device's write cache, i.e. pages that would be lost on a crash.
    pub fn volatile_pages(&self) -> usize {
        self.volatile.borrow().len()
    }

    pub fn crashes(&self) -> u64 {
        self.crashes.get()
    }

    /// Simulate a power loss: every page that was written
    /// non-durably since its last durable write reverts to that last
    /// durable content (or its initial zeroed state if it was never
    /// durably written). Durable (RWF_DSYNC/FUA) writes are unaffected
    /// because they never leave an undo entry behind. Call this after
    /// the engine has been dropped and before it is reopened to model a
    /// crash + recovery.
    pub fn crash(&self) {
        let undo = std::mem::take(&mut *self.volatile.borrow_mut());
        for (lba, snapshot) in undo {
            self.inner.poke(Lba(lba), &snapshot);
        }
        self.crashes.set(self.crashes.get() + 1);
    }

    /// Test-only backdoor: read raw bytes from the underlying
    /// [`MockDevice`] without going through the trait surface.
    /// Used by `recovery.rs` to locate structural pages on disk
    /// before staging corruption against them.
    pub fn peek(&self, lba: Lba, dst: &mut [u8]) {
        self.inner.peek(lba, dst);
    }

    /// Test-only backdoor: poke raw bytes at an LBA, simulating a
    /// torn / corrupted write that the engine must detect via
    /// checksum on subsequent reads.
    pub fn poke(&self, lba: Lba, src: &[u8]) {
        self.inner.poke(lba, src);
    }

    pub fn pause_next_read(&self, lba: Lba) -> Rc<ReadPause> {
        let pause = Rc::new(ReadPause::new(lba));
        *self.read_pause.borrow_mut() = Some(pause.clone());
        pause
    }
}

pub struct ReadPause {
    lba: Lba,
    armed: Cell<bool>,
    paused: Cell<bool>,
    released: Cell<bool>,
}

impl ReadPause {
    fn new(lba: Lba) -> Self {
        Self {
            lba,
            armed: Cell::new(true),
            paused: Cell::new(false),
            released: Cell::new(false),
        }
    }

    pub fn paused(&self) -> bool {
        self.paused.get()
    }

    pub fn release(&self) {
        self.released.set(true);
    }

    fn try_pause(&self, lba: Lba) -> bool {
        if self.lba != lba || !self.armed.replace(false) {
            return false;
        }
        self.paused.set(true);
        true
    }

    fn released(&self) -> bool {
        self.released.get()
    }
}

impl BlockDevice for SimBlockDevice {
    fn page_size(&self) -> usize {
        self.inner.page_size()
    }

    fn capacity_pages(&self) -> u64 {
        self.inner.capacity_pages()
    }

    fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        self.inner.register_buffers(base, len)
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);
        let _guard = InflightGuard::enter(self);
        yield_n(delay).await;
        let pause = self.read_pause.borrow().as_ref().cloned();
        if let Some(pause) = pause {
            if pause.try_pause(lba) {
                while !pause.released() {
                    yield_once().await;
                }
            }
        }
        self.reads.set(self.reads.get() + 1);
        if fault {
            self.io_errors.set(self.io_errors.get() + 1);
            return Err(Error::Io(5));
        }
        self.inner.read(lba, dst).await?;
        // Inject bitrot only on the success path: flipping the
        // first byte simulates the cheapest detectable corruption
        // the per-page xxh3 checksum should catch. The engine MUST
        // convert this into a miss; the bytes must never reach the
        // caller.
        if !dst.is_empty() && draw_corrupt(&self.cfg) {
            dst[0] ^= 0xff;
            self.corruptions_injected
                .set(self.corruptions_injected.get() + 1);
        }
        Ok(())
    }

    async fn write(&self, lba: Lba, src: &[u8], durable: bool) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);
        let _guard = InflightGuard::enter(self);
        yield_n(delay).await;
        self.writes.set(self.writes.get() + 1);
        if durable {
            self.durable_writes.set(self.durable_writes.get() + 1);
        }
        if fault {
            self.io_errors.set(self.io_errors.get() + 1);
            return Err(Error::Io(5));
        }
        // Maintain the volatile-cache undo log before the bytes hit
        // stable storage. We only touch well-formed, in-range writes;
        // malformed ones fall through to `inner.write` which rejects
        // them without persisting anything.
        let ps = self.inner.page_size();
        let capacity = self.inner.capacity_pages();
        if !src.is_empty() && src.len() % ps == 0 {
            let n_pages = (src.len() / ps) as u64;
            let mut undo = self.volatile.borrow_mut();
            for i in 0..n_pages {
                let page_lba = lba.0 + i;
                if page_lba >= capacity {
                    break;
                }
                if durable {
                    // Write-through: the page is now stable, so it must
                    // not be reverted by a later crash.
                    undo.remove(&page_lba);
                } else if !undo.contains_key(&page_lba) {
                    // First volatile write since the last durable write:
                    // remember the durable content to restore on crash.
                    let mut snapshot = vec![0u8; ps];
                    self.inner.peek(Lba(page_lba), &mut snapshot);
                    undo.insert(page_lba, snapshot);
                }
            }
        }
        self.inner.write(lba, src, durable).await
    }
}

/// RAII guard that bumps `inflight` (and `max_inflight`) on entry and
/// drops it on scope exit, including via early `return` on the fault
/// path. Single-threaded `Cell` arithmetic is enough; the DST executor
/// never preempts between yield points.
struct InflightGuard<'a> {
    dev: &'a SimBlockDevice,
}

impl<'a> InflightGuard<'a> {
    fn enter(dev: &'a SimBlockDevice) -> Self {
        let n = dev.inflight.get() + 1;
        dev.inflight.set(n);
        if n > dev.max_inflight.get() {
            dev.max_inflight.set(n);
        }
        Self { dev }
    }
}

impl Drop for InflightGuard<'_> {
    fn drop(&mut self) {
        self.dev.inflight.set(self.dev.inflight.get() - 1);
    }
}
