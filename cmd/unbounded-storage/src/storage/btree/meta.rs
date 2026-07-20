// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Dual meta-page bookkeeping.
//!
//! Slot A and slot B occupy fixed LBAs 0 and 1. Each holds a
//! transaction id and the root LBA of the tree as of that
//! transaction. On commit we always write to the *older* of the
//! two slots, then swap the in-memory active pointer; this means
//! the previously-current slot is still valid in case the new
//! write tears (and is then rejected at restart by its bad
//! checksum). On open we return both valid slots in descending
//! transaction order so tree recovery can fall back if the newest
//! meta points to damaged structural pages.

use std::rc::Rc;

use crate::storage::blockdev::{BlockDevice, ScratchPool};
use crate::storage::btree::page::{self, Decoded};
use crate::storage::types::{Error, Lba};

#[derive(Copy, Clone, Eq, PartialEq, Debug)]
pub enum MetaSlot {
    A,
    B,
}

impl MetaSlot {
    pub fn lba(self) -> Lba {
        match self {
            MetaSlot::A => page::META_SLOT_A,
            MetaSlot::B => page::META_SLOT_B,
        }
    }
    pub fn other(self) -> MetaSlot {
        match self {
            MetaSlot::A => MetaSlot::B,
            MetaSlot::B => MetaSlot::A,
        }
    }
}

#[derive(Copy, Clone, Debug)]
pub struct MetaState {
    pub active: MetaSlot,
    pub txn_id: u64,
    pub root_lba: Lba,
    pub hwm: u64,
}

/// Read both meta slots and return every checksum-valid candidate in
/// descending transaction order.
pub async fn load_meta_candidates<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
) -> Result<Vec<MetaState>, Error> {
    let mut buf_a = scratch.acquire().await;
    let mut buf_b = scratch.acquire().await;
    // We tolerate I/O errors here: the design wants us to treat a
    // partially-failing disk as "no meta", not propagate the error.
    let a = match device.read(page::META_SLOT_A, buf_a.as_mut_slice()).await {
        Ok(()) => page::decode(buf_a.as_mut_slice()),
        Err(_) => Decoded::Empty,
    };
    let b = match device.read(page::META_SLOT_B, buf_b.as_mut_slice()).await {
        Ok(()) => page::decode(buf_b.as_mut_slice()),
        Err(_) => Decoded::Empty,
    };
    let a_meta = as_meta(a, MetaSlot::A);
    let b_meta = as_meta(b, MetaSlot::B);
    let mut candidates: Vec<_> = [a_meta, b_meta].into_iter().flatten().collect();
    candidates.sort_by_key(|state| std::cmp::Reverse(state.txn_id));
    Ok(candidates)
}

fn as_meta(d: Decoded, slot: MetaSlot) -> Option<MetaState> {
    match d {
        Decoded::Meta {
            txn_id,
            root_lba,
            hwm,
        } => Some(MetaState {
            active: slot,
            txn_id,
            root_lba,
            hwm,
        }),
        _ => None,
    }
}

/// Write the new meta page into the *inactive* slot, leaving the
/// current active slot untouched until the caller swaps state.
pub async fn write_inactive<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    current_active: MetaSlot,
    new_txn_id: u64,
    new_root: Lba,
    new_hwm: u64,
) -> Result<MetaSlot, Error> {
    let target = current_active.other();
    let ps = device.page_size();
    let encoded = page::encode_meta(ps, new_txn_id, new_root, new_hwm);
    let mut page = scratch.acquire().await;
    let slice = page.as_mut_slice();
    debug_assert_eq!(slice.len(), encoded.len());
    slice.copy_from_slice(&encoded);
    device.write(target.lba(), slice).await?;
    Ok(target)
}
