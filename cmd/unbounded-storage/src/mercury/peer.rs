// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Peer identifier and address table.
//!
//! `PeerId` is the wire-side identity used by the Mercury transport layer;
//! it is intentionally distinct from `bufferpool::types::PeerId`, which is
//! minted by the p2p layer and never crosses into transport internals.
//! `PeerTable` maps a `PeerId` to a Mercury-resolved `hg_addr_t`.

use std::collections::HashMap;
use std::ptr::NonNull;
use std::sync::RwLock;

use super::ffi::hg_addr_t;

/// Mercury peer identifier as understood by the transport layer.
/// Distinct from `bufferpool::types::PeerId`; this one keys the on-wire
/// address table.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct PeerId(pub u64);

/// In-memory map from `PeerId` to a resolved `hg_addr_t`.
///
/// `hg_addr_t` is an opaque pointer owned by Mercury; lifetime is bounded
/// by the owning `Nic` (entries are removed and freed during shutdown).
/// Multiple readers and a single writer are expected.
pub struct PeerTable {
    inner: RwLock<HashMap<PeerId, NonNull<hg_addr_t>>>,
}

// SAFETY: the wrapped `NonNull<hg_addr_t>` points into Mercury's internal
// state; Mercury documents that addr handles are safe to use from any
// thread for the lifetime of the owning class. The `RwLock` provides the
// only mutating access.
unsafe impl Send for PeerTable {}
unsafe impl Sync for PeerTable {}

impl PeerTable {
    pub fn new() -> Self {
        Self {
            inner: RwLock::new(HashMap::new()),
        }
    }

    /// Insert. Returns the previous handle if any (caller must free it
    /// using `HG_Addr_free` against the owning class).
    pub fn insert(&self, id: PeerId, addr: NonNull<hg_addr_t>) -> Option<NonNull<hg_addr_t>> {
        self.inner
            .write()
            .expect("peer table poisoned")
            .insert(id, addr)
    }

    pub fn get(&self, id: PeerId) -> Option<NonNull<hg_addr_t>> {
        self.inner
            .read()
            .expect("peer table poisoned")
            .get(&id)
            .copied()
    }

    pub fn remove(&self, id: PeerId) -> Option<NonNull<hg_addr_t>> {
        self.inner.write().expect("peer table poisoned").remove(&id)
    }

    /// Take all entries and clear the map. Caller frees each.
    pub fn drain(&self) -> Vec<(PeerId, NonNull<hg_addr_t>)> {
        let mut guard = self.inner.write().expect("peer table poisoned");
        guard.drain().collect()
    }

    pub fn len(&self) -> usize {
        self.inner.read().expect("peer table poisoned").len()
    }

    pub fn is_empty(&self) -> bool {
        self.inner.read().expect("peer table poisoned").is_empty()
    }
}

impl Default for PeerTable {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::thread;

    fn fake_addr(id: u64) -> NonNull<hg_addr_t> {
        // SAFETY: hg_addr_t is a zero-sized opaque enum; we never dereference
        // the pointer in tests, only round-trip it through the table.
        NonNull::new(id as *mut hg_addr_t).expect("non-zero id")
    }

    #[test]
    fn insert_get_remove_round_trip() {
        let table = PeerTable::new();
        let id = PeerId(42);
        let addr = fake_addr(0xdead_beef);

        assert!(table.insert(id, addr).is_none());
        assert_eq!(table.get(id), Some(addr));

        let removed = table.remove(id);
        assert_eq!(removed, Some(addr));
        assert_eq!(table.get(id), None);
    }

    #[test]
    fn drain_returns_all_and_empties() {
        let table = PeerTable::new();
        for i in 1..=5u64 {
            table.insert(PeerId(i), fake_addr(i * 0x100));
        }
        assert_eq!(table.len(), 5);

        let mut entries = table.drain();
        entries.sort_by_key(|(p, _)| p.0);
        assert_eq!(entries.len(), 5);
        for (i, (peer, addr)) in entries.iter().enumerate() {
            let expected = (i as u64) + 1;
            assert_eq!(peer.0, expected);
            assert_eq!(*addr, fake_addr(expected * 0x100));
        }
        assert!(table.is_empty());
        assert_eq!(table.len(), 0);
    }

    #[test]
    fn len_and_is_empty_track_inserts_and_removes() {
        let table = PeerTable::new();
        assert!(table.is_empty());
        assert_eq!(table.len(), 0);

        table.insert(PeerId(1), fake_addr(1));
        assert!(!table.is_empty());
        assert_eq!(table.len(), 1);

        table.insert(PeerId(2), fake_addr(2));
        assert_eq!(table.len(), 2);

        table.remove(PeerId(1));
        assert_eq!(table.len(), 1);

        table.remove(PeerId(2));
        assert!(table.is_empty());
    }

    #[test]
    fn concurrent_insert_and_get() {
        const THREADS: u64 = 8;
        const OPS: u64 = 1000;

        let table = Arc::new(PeerTable::new());
        let mut handles = Vec::new();
        for t in 0..THREADS {
            let table = Arc::clone(&table);
            handles.push(thread::spawn(move || {
                let base = t * OPS + 1;
                for i in 0..OPS {
                    let id = PeerId(base + i);
                    table.insert(id, fake_addr(base + i));
                    let _ = table.get(id);
                }
            }));
        }
        for h in handles {
            h.join().expect("worker thread panicked");
        }

        assert_eq!(table.len(), (THREADS * OPS) as usize);
    }
}
