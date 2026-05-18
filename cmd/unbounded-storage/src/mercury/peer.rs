// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Eager peer-address resolution. Mercury's `HG_Addr_lookup2` is
//! synchronous on libfabric NA backends; we call it once per peer
//! at class construction and stash the resulting `hg_addr_t` for
//! the lifetime of the class.

use std::ffi::CString;

use crate::bufferpool::PeerId;
use crate::mercury::config::PeerEntry;
use crate::mercury::error::{HgError, Result, check};
use crate::mercury::ffi;

/// Resolved peer table. Holds owning references to Mercury
/// addresses; freeing happens in `Drop`.
pub(crate) struct PeerTable {
    hg_class: ffi::hg_class_t,
    entries: Vec<(PeerId, ffi::hg_addr_t)>,
}

impl PeerTable {
    /// Look up every peer in `cfg`. On any failure the partial
    /// table is freed and the error is returned.
    pub(crate) fn new(hg_class: ffi::hg_class_t, peers: &[PeerEntry]) -> Result<Self> {
        let mut table = Self {
            hg_class,
            entries: Vec::with_capacity(peers.len()),
        };
        for entry in peers {
            let cname = CString::new(entry.addr.as_str())
                .map_err(|_| HgError::new(0, "peer addr contains NUL"))?;
            let mut addr: ffi::hg_addr_t = std::ptr::null_mut();
            // SAFETY: `hg_class` is the live class returned by HG_Init;
            // `cname` is a valid NUL-terminated string for the
            // duration of the call; `addr` is a stack out-pointer.
            let ret = unsafe { ffi::HG_Addr_lookup2(hg_class, cname.as_ptr(), &mut addr) };
            check(ret, "HG_Addr_lookup2")?;
            table.entries.push((entry.peer_id, addr));
        }
        Ok(table)
    }

    /// Resolve a `PeerId` to a Mercury address.
    pub(crate) fn lookup(&self, peer: PeerId) -> Result<ffi::hg_addr_t> {
        self.entries
            .iter()
            .find_map(|(id, addr)| if *id == peer { Some(*addr) } else { None })
            .ok_or(HgError::new(0, "unknown PeerId"))
    }
}

impl Drop for PeerTable {
    fn drop(&mut self) {
        for (_, addr) in self.entries.drain(..) {
            if !addr.is_null() {
                // SAFETY: addresses came from HG_Addr_lookup2 against
                // the same class; freeing them once on drop is the
                // documented contract. `ClassInner::drop` takes care
                // to drop the table before calling `HG_Finalize`.
                unsafe {
                    ffi::HG_Addr_free(self.hg_class, addr);
                }
            }
        }
    }
}
