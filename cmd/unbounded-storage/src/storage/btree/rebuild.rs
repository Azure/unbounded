// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Restart-time recovery: LBA-order leaf scan.
//!
//! If both meta pages fail to decode we fall back to scanning
//! disk LBAs looking for leaf pages. Among those, we pick the
//! highest `txn_id` group and synthesize a fresh tree from their
//! entries. This is the design's "LBA-order restart scan" and is
//! intentionally tolerant of arbitrary garbage on disk - anything
//! that doesn't parse as a leaf, or whose checksum mismatches, is
//! silently skipped.
//!
//! The scan is bounded by the persisted high-water mark from the
//! active meta slot: if a meta page survives we never need to
//! look past its recorded HWM, since the allocator never handed
//! out a higher LBA. If neither meta slot decodes we degrade to a
//! full-capacity scan.
//!
//! Internal pages are *not* trusted during rebuild: torn writes
//! that took out the meta slots may also have corrupted the
//! interior of the tree, so we ignore them and re-derive
//! structure from the (key, value) pairs we can verify.

use std::collections::BTreeMap;

use std::cmp::Ordering;
use std::rc::Rc;

use crate::storage::blockdev::{BlockDevice, ScratchPool};
use crate::storage::btree::page::{self, Decoded, LeafEntry, META_SLOT_A, META_SLOT_B};
use crate::storage::types::{Error, Lba, PageKey};

/// Result of a rebuild scan: the entries to seed the in-memory
/// mirror with, plus the `txn_id` they came from. `None` means
/// the scan found no valid leaves anywhere - treat the disk as
/// empty.
pub struct RebuildResult {
    pub txn_id: u64,
    pub entries: BTreeMap<PageKey, LeafEntry>,
}

/// Scan the disk for the highest-txn-id leaf cohort.
///
/// `upper_bound` bounds the LBA range scanned:
/// - `None` means scan the full device (legacy / no meta survived).
/// - `Some(0)` means a legacy meta page with no HWM info; also
///   degrades to a full-capacity scan.
/// - `Some(hwm)` scans `0..=hwm` clamped to the device, since the
///   allocator never handed out a higher LBA.
pub async fn scan_for_leaves<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    upper_bound: Option<u64>,
) -> Result<Option<RebuildResult>, Error> {
    let cap = device.capacity_pages();
    let end = match upper_bound {
        None | Some(0) => cap,
        Some(hwm) => hwm.min(cap.saturating_sub(1)).saturating_add(1),
    };
    let mut buf = scratch.acquire().await;
    let mut best_txn: u64 = 0;
    let mut best: BTreeMap<PageKey, LeafEntry> = BTreeMap::new();

    for lba in 0..end {
        if lba == META_SLOT_A.0 || lba == META_SLOT_B.0 {
            continue;
        }
        if device.read(Lba(lba), buf.as_mut_slice()).await.is_err() {
            continue;
        }
        if let Decoded::Leaf { txn_id, entries } = page::decode(buf.as_mut_slice()) {
            match txn_id.cmp(&best_txn) {
                Ordering::Greater => {
                    best_txn = txn_id;
                    best.clear();
                    for (k, v) in entries {
                        best.insert(k, v);
                    }
                }
                Ordering::Equal if best_txn > 0 => {
                    // Same epoch: merge. Distinct leaves of the same
                    // CoW generation should never share a txn id, but
                    // a torn neighbor write can leave two leaves that
                    // both claim a key; the higher LBA wins arbitrarily.
                    for (k, v) in entries {
                        best.insert(k, v);
                    }
                }
                _ => {}
            }
        }
    }

    if best_txn == 0 {
        Ok(None)
    } else {
        Ok(Some(RebuildResult {
            txn_id: best_txn,
            entries: best,
        }))
    }
}
