// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Restart-time recovery: LBA-order leaf scan.
//!
//! If both meta pages fail to decode we fall back to scanning
//! every disk LBA looking for leaf pages. Among those, we pick
//! the highest `txn_id` group and synthesize a fresh tree from
//! their entries. This is the design's "LBA-order restart scan"
//! and is intentionally tolerant of arbitrary garbage on disk -
//! anything that doesn't parse as a leaf, or whose checksum
//! mismatches, is silently skipped.
//!
//! Internal pages are *not* trusted during rebuild: torn writes
//! that took out the meta slots may also have corrupted the
//! interior of the tree, so we ignore them and re-derive
//! structure from the (key, value) pairs we can verify.

use std::collections::BTreeMap;

use std::cmp::Ordering;

use crate::storage::blockdev::BlockDevice;
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

/// Scan the disk for the highest-txn-id leaf cohort. In practice
/// the per-disk capacity is a few hundred thousand 4 KiB pages and
/// we expect to scan all of them.
pub async fn scan_for_leaves<B: BlockDevice>(
    device: &B,
) -> Result<Option<RebuildResult>, Error> {
    let ps = device.page_size();
    let cap = device.capacity_pages();
    let mut buf = vec![0u8; ps];
    let mut best_txn: u64 = 0;
    let mut best: BTreeMap<PageKey, LeafEntry> = BTreeMap::new();

    for lba in 0..cap {
        if lba == META_SLOT_A.0 || lba == META_SLOT_B.0 {
            continue;
        }
        if device.read(Lba(lba), &mut buf).await.is_err() {
            continue;
        }
        if let Decoded::Leaf { txn_id, entries } = page::decode(&buf) {
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
