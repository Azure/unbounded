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

use std::cell::Cell;
use std::rc::Rc;

use rand::Rng;
use unbounded_storage::storage::blockdev::{BlockDevice, MockDevice, MockDeviceConfig};
use unbounded_storage::storage::types::{Error, Lba};

use crate::framework::executor::{with_sim, yield_n};

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
    io_errors: Cell<u64>,
}

impl SimBlockDevice {
    pub fn new(device_cfg: MockDeviceConfig, sim_cfg: Rc<MockSimConfig>) -> Self {
        Self {
            inner: MockDevice::new(device_cfg),
            cfg: sim_cfg,
            reads: Cell::new(0),
            writes: Cell::new(0),
            io_errors: Cell::new(0),
        }
    }

    pub fn reads(&self) -> u64 {
        self.reads.get()
    }
    pub fn writes(&self) -> u64 {
        self.writes.get()
    }
    pub fn io_errors(&self) -> u64 {
        self.io_errors.get()
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

    fn write_queue_depth(&self) -> u32 {
        self.inner.write_queue_depth()
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);
        yield_n(delay).await;
        self.reads.set(self.reads.get() + 1);
        if fault {
            self.io_errors.set(self.io_errors.get() + 1);
            return Err(Error::Io(5));
        }
        self.inner.read(lba, dst).await
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);
        yield_n(delay).await;
        self.writes.set(self.writes.get() + 1);
        if fault {
            self.io_errors.set(self.io_errors.get() + 1);
            return Err(Error::Io(5));
        }
        self.inner.write(lba, src).await
    }
}
