use std::collections::HashMap;

use crate::layout;
use crate::paxos::Register;
use crate::runtime::{self, CoreId};

use super::Cache;

/// One cache slot. Subtractive next to an allocator entry: no CRC, no generation, no A/B copy,
/// no group commit. The register is here only so a hit can be confirmed against the quorum;
/// nothing here survives a restart.
#[derive(Clone, Copy, Default)]
pub(super) struct Slot {
    pub(super) addr: u64,
    pub(super) reg: Register,
    /// CLOCK reference bit.
    pub(super) used: bool,
    pub(super) live: bool,
    /// A write is in flight into this slot. CLOCK steps over it rather than handing the same
    /// media out twice.
    pub(super) busy: bool,
    /// Reads in flight out of this slot. Same purpose as `busy` and the same treatment: an IO
    /// is aimed at these exact bytes, so nothing may take them until it lands. Reads share,
    /// which is why this counts and `busy` does not.
    pub(super) readers: u16,
}

impl Slot {
    /// Whether an IO is aimed at this slot's media.
    fn pinned(&self) -> bool {
        self.busy || self.readers > 0
    }
}

/// Why a claim found no slot. The metrics keep them apart: `Colder` is the cache working as
/// intended under pressure, `Busy` is transient and self-clearing.
#[derive(Clone, Copy, Debug, PartialEq)]
pub(super) enum Decline {
    Busy,
    Colder,
}

/// One 4 MiB piece of media the cache holds, carved from the store's tail.
pub(super) struct Chunk {
    pub(super) off: u64,
}

/// Slots one 4 MiB chunk holds. Everything the cache holds is an immutable 4 KiB block, so
/// there is one number and it is 1024.
pub(super) const fn chunk_slots() -> usize {
    (layout::CHUNK_BYTES / layout::SMALL_PAGE) as usize
}

/// This core's share of the cache, as a set of 4 MiB chunks of 4 KiB slots.
pub(super) struct Store {
    pub(super) chunks: Vec<Chunk>,
    pub(super) slots: Vec<Slot>,
    pub(super) map: HashMap<u64, u32>,
    pub(super) hand: u32,
    pub(super) evicted: u64,
    pub(super) dropped: u64,
}

impl Store {
    pub(super) fn new() -> Store {
        Store {
            chunks: Vec::new(),
            slots: Vec::new(),
            map: HashMap::new(),
            hand: 0,
            evicted: 0,
            dropped: 0,
        }
    }

    /// Byte offset of a local slot.
    pub(super) fn off(&self, i: u32) -> u64 {
        let cs = chunk_slots();
        let (c, k) = (i as usize / cs, i as usize % cs);
        self.chunks[c].off + k as u64 * layout::SMALL_PAGE
    }

    pub(super) fn push_chunk(&mut self, c: Chunk) {
        let cs = chunk_slots();
        self.chunks.push(c);
        self.slots.resize(self.slots.len() + cs, Slot::default());
        self.map.reserve(cs);
    }

    pub(super) fn current(&self, addr: u64, reg: Register) -> bool {
        self.map.get(&addr).is_some_and(|&i| {
            let s = &self.slots[i as usize];
            s.live && s.reg == reg
        })
    }

    pub(super) fn find(&mut self, addr: u64) -> Option<(u32, Register)> {
        let i = *self.map.get(&addr)?;
        let s = &mut self.slots[i as usize];
        if !s.live {
            return None;
        }
        s.used = true;
        s.readers = s.readers.saturating_add(1);
        Some((i, s.reg))
    }

    pub(super) fn release(&mut self, i: u32) {
        let s = &mut self.slots[i as usize];
        debug_assert!(s.readers > 0, "released a slot nothing was reading");
        s.readers = s.readers.saturating_sub(1);
    }

    pub(super) fn forget(&mut self, addr: u64) {
        if let Some(i) = self.map.remove(&addr) {
            self.slots[i as usize].live = false;
        }
    }

    pub(super) fn forget_at(&mut self, addr: u64, reg: Register) {
        if self
            .map
            .get(&addr)
            .is_some_and(|&i| self.slots[i as usize].reg == reg)
        {
            self.forget(addr);
        }
    }

    pub(super) fn claim(
        &mut self,
        addr: u64,
        reg: Register,
        hotter: impl Fn(u64) -> bool,
    ) -> Result<u32, Decline> {
        if self.slots.is_empty() {
            return Err(Decline::Busy);
        }
        let i = match self.map.get(&addr) {
            Some(&i) if !self.slots[i as usize].pinned() => i,
            Some(_) => return Err(Decline::Busy),
            None => self.evict(hotter)?,
        };
        self.map.insert(addr, i);
        self.slots[i as usize] = Slot {
            addr,
            reg,
            used: false,
            live: false,
            busy: true,
            readers: 0,
        };
        Ok(i)
    }

    fn evict(&mut self, hotter: impl Fn(u64) -> bool) -> Result<u32, Decline> {
        let n = self.slots.len() as u32;
        for _ in 0..2 * n {
            let i = self.hand;
            self.hand = (self.hand + 1) % n;
            let s = &mut self.slots[i as usize];
            if s.pinned() {
                continue;
            }
            if !s.live {
                return Ok(i);
            }
            if s.used {
                s.used = false;
                continue;
            }
            let old = s.addr;
            if hotter(old) {
                return Err(Decline::Colder);
            }
            self.slots[i as usize].live = false;
            self.map.remove(&old);
            self.evicted += 1;
            return Ok(i);
        }
        Err(Decline::Busy)
    }

    pub(super) fn finish(&mut self, i: u32, ok: bool) {
        let s = &mut self.slots[i as usize];
        s.busy = false;
        s.live = ok;
        if !ok {
            self.map.remove(&s.addr);
        }
    }
}

/// A cache entry's media, held still for the length of one read.
#[must_use = "a leased slot cannot be reused until the read is done with it"]
pub(super) struct Lease {
    pub(super) cache: &'static Cache,
    pub(super) owner: CoreId,
    pub(super) slot: u32,
}

impl Lease {
    pub(super) async fn release(self) {
        let me = std::mem::ManuallyDrop::new(self);
        me.cache.unpin(me.owner, me.slot).await;
    }
}

impl Drop for Lease {
    fn drop(&mut self) {
        let (cache, owner, slot) = (self.cache, self.owner, self.slot);
        let _ = runtime::spawn_local(async move { cache.unpin(owner, slot).await });
    }
}

/// A slot claimed for an admission, carrying the obligation to settle its busy state.
#[must_use = "a claimed slot is being written into until the admission reports back"]
pub(super) struct Admit {
    pub(super) cache: &'static Cache,
    pub(super) owner: CoreId,
    pub(super) slot: u32,
}

impl Admit {
    pub(super) async fn settle(self, ok: bool) {
        let me = std::mem::ManuallyDrop::new(self);
        me.cache.report(me.owner, me.slot, ok).await;
    }
}

impl Drop for Admit {
    fn drop(&mut self) {
        let (cache, owner, slot) = (self.cache, self.owner, self.slot);
        let _ = runtime::spawn_local(async move { cache.report(owner, slot, false).await });
    }
}
