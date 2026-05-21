// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! In-memory [`BlockDevice`] used by unit and DST tests.
//!
//! `MockDevice` keeps a `RefCell<Vec<u8>>` of `capacity_pages *
//! page_size` bytes and serves reads and writes directly out of it.
//! It is intentionally `!Send + !Sync`: the DST executor is
//! single-threaded and production code never holds a `MockDevice`.
//! Fault injection knobs (latency, write errors, read corruption)
//! are configured up-front through [`MockDeviceConfig`].

use std::cell::{Cell, RefCell};

use crate::storage::blockdev::BlockDevice;
use crate::storage::types::{Error, Lba};

/// Where in an operation a fault should manifest.
#[derive(Copy, Clone, Eq, PartialEq, Debug, Default)]
pub enum MockFaultMode {
    #[default]
    None,
    /// Reads return `Err(Io)` with the configured errno.
    ReadIo,
    /// Reads succeed but flip a byte in the destination, simulating
    /// a silent torn write / bit rot.
    ReadCorrupt,
    /// Writes return `Err(Io)`.
    WriteIo,
}

/// Static configuration knobs for [`MockDevice`].
#[derive(Copy, Clone, Debug)]
pub struct MockDeviceConfig {
    pub page_size: usize,
    pub capacity_pages: u64,
    pub write_queue_depth: u32,
    pub fault_mode: MockFaultMode,
    pub fault_errno: i32,
}

impl Default for MockDeviceConfig {
    fn default() -> Self {
        Self {
            page_size: 4096,
            capacity_pages: 1024,
            write_queue_depth: 32,
            fault_mode: MockFaultMode::None,
            fault_errno: libc_eio(),
        }
    }
}

const fn libc_eio() -> i32 {
    // Same value across the platforms we care about; avoid pulling
    // libc into non-Linux builds just for this constant.
    5
}

pub struct MockDevice {
    cfg: Cell<MockDeviceConfig>,
    storage: RefCell<Vec<u8>>,
    read_count: Cell<u64>,
    write_count: Cell<u64>,
    registered_base: Cell<Option<*mut u8>>,
    registered_len: Cell<usize>,
}

impl MockDevice {
    pub fn new(cfg: MockDeviceConfig) -> Self {
        let bytes = cfg.capacity_pages as usize * cfg.page_size;
        Self {
            cfg: Cell::new(cfg),
            storage: RefCell::new(vec![0u8; bytes]),
            read_count: Cell::new(0),
            write_count: Cell::new(0),
            registered_base: Cell::new(None),
            registered_len: Cell::new(0),
        }
    }

    pub fn set_fault_mode(&self, mode: MockFaultMode) {
        let mut cfg = self.cfg.get();
        cfg.fault_mode = mode;
        self.cfg.set(cfg);
    }

    pub fn reads(&self) -> u64 {
        self.read_count.get()
    }

    pub fn writes(&self) -> u64 {
        self.write_count.get()
    }

    pub fn registered_base(&self) -> Option<*mut u8> {
        self.registered_base.get()
    }

    pub fn registered_len(&self) -> usize {
        self.registered_len.get()
    }

    /// Direct backdoor for tests: read raw bytes without going
    /// through the trait surface (and without faults).
    pub fn peek(&self, lba: Lba, dst: &mut [u8]) {
        let cfg = self.cfg.get();
        let start = lba.0 as usize * cfg.page_size;
        let bytes = self.storage.borrow();
        dst.copy_from_slice(&bytes[start..start + dst.len()]);
    }

    /// Direct backdoor for tests: poke raw bytes at an LBA. Used to
    /// stage corruption patterns the engine should subsequently
    /// detect.
    pub fn poke(&self, lba: Lba, src: &[u8]) {
        let cfg = self.cfg.get();
        let start = lba.0 as usize * cfg.page_size;
        let mut bytes = self.storage.borrow_mut();
        bytes[start..start + src.len()].copy_from_slice(src);
    }
}

impl BlockDevice for MockDevice {
    fn page_size(&self) -> usize {
        self.cfg.get().page_size
    }

    fn capacity_pages(&self) -> u64 {
        self.cfg.get().capacity_pages
    }

    fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        self.registered_base.set(Some(base));
        self.registered_len.set(len);
        Ok(())
    }

    fn write_queue_depth(&self) -> u32 {
        self.cfg.get().write_queue_depth
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        self.read_count.set(self.read_count.get() + 1);
        let cfg = self.cfg.get();
        if lba.0 >= cfg.capacity_pages {
            return Err(Error::OutOfRange);
        }
        if dst.len() > cfg.page_size {
            return Err(Error::OutOfRange);
        }
        if matches!(cfg.fault_mode, MockFaultMode::ReadIo) {
            return Err(Error::Io(cfg.fault_errno));
        }
        let start = lba.0 as usize * cfg.page_size;
        let bytes = self.storage.borrow();
        dst.copy_from_slice(&bytes[start..start + dst.len()]);
        if matches!(cfg.fault_mode, MockFaultMode::ReadCorrupt) && !dst.is_empty() {
            dst[0] ^= 0xff;
        }
        Ok(())
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error> {
        self.write_count.set(self.write_count.get() + 1);
        let cfg = self.cfg.get();
        if lba.0 >= cfg.capacity_pages {
            return Err(Error::OutOfRange);
        }
        if src.len() > cfg.page_size {
            return Err(Error::OutOfRange);
        }
        if matches!(cfg.fault_mode, MockFaultMode::WriteIo) {
            return Err(Error::Io(cfg.fault_errno));
        }
        let start = lba.0 as usize * cfg.page_size;
        let mut bytes = self.storage.borrow_mut();
        bytes[start..start + src.len()].copy_from_slice(src);
        Ok(())
    }
}
