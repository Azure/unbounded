// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! On-disk page formats for the B+tree.
//!
//! Every btree page is the device's atomic write unit (default
//! 4 KiB) and starts with a fixed header:
//!
//! ```text
//!   offset  size  field
//!   ------  ----  ---------------------------------
//!   0       1     page_type (0 empty, 1 leaf, 2 internal, 3 meta)
//!   1       1     reserved
//!   2       2     nentries
//!   4       4     reserved
//!   8       8     txn_id
//!   16      8     checksum (xxh3 over the page with this field zeroed)
//!   24      8     reserved (padding so body is 32-aligned)
//!   32      ...   body
//! ```
//!
//! Body layout depends on `page_type`:
//!
//! - **leaf** entries (56 bytes each):
//!   `[ key:36 | lba:8 | data_checksum:8 | byte_len:4 ]`.
//! - **internal** entries (44 bytes each): `[ key:36 | child_lba:8 ]`.
//!   Each entry's `key` is the smallest [`PageKey`] reachable through
//!   `child_lba`; `nentries` keys map to `nentries` children. A search
//!   for `q` descends into the rightmost entry with `key <= q`.
//! - **meta** (one entry, 16 bytes): `[ root_lba:8 | hwm:8 ]`.
//!
//! Internal entries store, in their `key` slot, the *smallest* key
//! reachable through their child.

use crate::storage::traits::{PageChecksum, Xxh3Checksum};
use crate::storage::types::{Checksum, Error, Lba, PageKey};

pub const HEADER_LEN: usize = 32;
pub const KEY_LEN: usize = 36;
pub const LEAF_ENTRY_LEN: usize = KEY_LEN + 8 + 8 + 4; // 56
pub const INTERNAL_ENTRY_LEN: usize = KEY_LEN + 8; // 44

// Offsets within a leaf entry (relative to the entry base).
const LEAF_LBA_OFF: usize = KEY_LEN; // 36
const LEAF_CSUM_OFF: usize = LEAF_LBA_OFF + 8; // 44
const LEAF_LEN_OFF: usize = LEAF_CSUM_OFF + 8; // 52
const LEAF_LEN_END: usize = LEAF_LEN_OFF + 4; // 56

// Offsets within an internal entry.
const INT_CHILD_OFF: usize = KEY_LEN; // 36
const INT_CHILD_END: usize = INT_CHILD_OFF + 8; // 44

// Offsets of header fields where the layout matters in encode/decode.
const HDR_CSUM_OFF: usize = 16;
const HDR_CSUM_END: usize = 24;

pub const PAGE_TYPE_EMPTY: u8 = 0;
pub const PAGE_TYPE_LEAF: u8 = 1;
pub const PAGE_TYPE_INTERNAL: u8 = 2;
pub const PAGE_TYPE_META: u8 = 3;

/// Sentinel LBA used for the meta page slots. Allocator must
/// mark these as in-use at open time.
pub const META_SLOT_A: Lba = Lba(0);
pub const META_SLOT_B: Lba = Lba(1);

/// In-memory representation of a leaf value.
#[derive(Copy, Clone, Eq, PartialEq, Debug)]
pub struct LeafEntry {
    pub lba: Lba,
    pub data_checksum: Checksum,
    pub byte_len: u32,
}

/// What the parser found in a page. `Empty` is returned for
/// either a structurally invalid page or one whose checksum
/// failed; callers treat both as "this subtree is gone" per the
/// design's silent-miss policy.
#[derive(Clone, Debug)]
pub enum Decoded {
    Empty,
    Leaf {
        txn_id: u64,
        entries: Vec<(PageKey, LeafEntry)>,
    },
    Internal {
        txn_id: u64,
        keys: Vec<PageKey>,
        children: Vec<Lba>,
    },
    Meta {
        txn_id: u64,
        root_lba: Lba,
        hwm: u64,
    },
}

pub fn max_leaf_entries(page_size: usize) -> usize {
    (page_size - HEADER_LEN) / LEAF_ENTRY_LEN
}

pub fn max_internal_keys(page_size: usize) -> usize {
    // body holds N (key, child) pairs, 44 bytes each.
    let body = page_size.saturating_sub(HEADER_LEN);
    body / INTERNAL_ENTRY_LEN
}

/// Decode a page slice. The slice must be exactly `page_size`
/// bytes (the caller has already loaded it from disk).
pub fn decode(page: &mut [u8]) -> Decoded {
    if page.len() < HEADER_LEN {
        return Decoded::Empty;
    }
    let page_type = page[0];
    let nentries = u16::from_le_bytes([page[2], page[3]]) as usize;
    let txn_id = u64::from_le_bytes(page[8..16].try_into().unwrap());
    let stored_checksum = u64::from_le_bytes(page[HDR_CSUM_OFF..HDR_CSUM_END].try_into().unwrap());

    if !verify_checksum(page, stored_checksum) {
        return Decoded::Empty;
    }

    match page_type {
        PAGE_TYPE_LEAF => decode_leaf(page, nentries, txn_id),
        PAGE_TYPE_INTERNAL => decode_internal(page, nentries, txn_id),
        PAGE_TYPE_META => decode_meta(page, txn_id),
        _ => Decoded::Empty,
    }
}

fn verify_checksum(page: &mut [u8], stored: u64) -> bool {
    let saved: [u8; HDR_CSUM_END - HDR_CSUM_OFF] =
        page[HDR_CSUM_OFF..HDR_CSUM_END].try_into().unwrap();
    page[HDR_CSUM_OFF..HDR_CSUM_END].fill(0);
    let actual = Xxh3Checksum::checksum_of(page).0;
    page[HDR_CSUM_OFF..HDR_CSUM_END].copy_from_slice(&saved);
    actual == stored
}

fn decode_leaf(page: &[u8], nentries: usize, txn_id: u64) -> Decoded {
    let max = max_leaf_entries(page.len());
    if nentries > max {
        return Decoded::Empty;
    }
    let mut entries = Vec::with_capacity(nentries);
    for i in 0..nentries {
        let base = HEADER_LEN + i * LEAF_ENTRY_LEN;
        let Some(key) = PageKey::decode(&page[base..base + KEY_LEN]) else {
            return Decoded::Empty;
        };
        let lba = Lba(u64::from_le_bytes(
            page[base + LEAF_LBA_OFF..base + LEAF_CSUM_OFF]
                .try_into()
                .unwrap(),
        ));
        let data_checksum = Checksum(u64::from_le_bytes(
            page[base + LEAF_CSUM_OFF..base + LEAF_LEN_OFF]
                .try_into()
                .unwrap(),
        ));
        let byte_len = u32::from_le_bytes(
            page[base + LEAF_LEN_OFF..base + LEAF_LEN_END]
                .try_into()
                .unwrap(),
        );
        entries.push((
            key,
            LeafEntry {
                lba,
                data_checksum,
                byte_len,
            },
        ));
    }
    Decoded::Leaf { txn_id, entries }
}

fn decode_internal(page: &[u8], nentries: usize, txn_id: u64) -> Decoded {
    let body = page.len() - HEADER_LEN;
    if nentries * INTERNAL_ENTRY_LEN > body {
        return Decoded::Empty;
    }
    let mut keys = Vec::with_capacity(nentries);
    let mut children = Vec::with_capacity(nentries);
    for i in 0..nentries {
        let base = HEADER_LEN + i * INTERNAL_ENTRY_LEN;
        let Some(key) = PageKey::decode(&page[base..base + KEY_LEN]) else {
            return Decoded::Empty;
        };
        let child = Lba(u64::from_le_bytes(
            page[base + INT_CHILD_OFF..base + INT_CHILD_END]
                .try_into()
                .unwrap(),
        ));
        keys.push(key);
        children.push(child);
    }
    Decoded::Internal {
        txn_id,
        keys,
        children,
    }
}

fn decode_meta(page: &[u8], txn_id: u64) -> Decoded {
    if page.len() < HEADER_LEN + 8 {
        return Decoded::Empty;
    }
    let root_lba = Lba(u64::from_le_bytes(
        page[HEADER_LEN..HEADER_LEN + 8].try_into().unwrap(),
    ));
    // Pages predating the hwm field leave the trailing 8 bytes as
    // zero, which is the correct legacy value for hwm.
    let hwm = if page.len() >= HEADER_LEN + 16 {
        u64::from_le_bytes(page[HEADER_LEN + 8..HEADER_LEN + 16].try_into().unwrap())
    } else {
        0
    };
    Decoded::Meta {
        txn_id,
        root_lba,
        hwm,
    }
}

/// Build a leaf page. Returns `OutOfSpace` if `entries` doesn't fit.
pub fn encode_leaf(
    page_size: usize,
    txn_id: u64,
    entries: &[(PageKey, LeafEntry)],
) -> Result<Vec<u8>, Error> {
    if entries.len() > max_leaf_entries(page_size) {
        return Err(Error::OutOfSpace);
    }
    let mut page = vec![0u8; page_size];
    page[0] = PAGE_TYPE_LEAF;
    page[2..4].copy_from_slice(&(entries.len() as u16).to_le_bytes());
    page[8..16].copy_from_slice(&txn_id.to_le_bytes());
    for (i, (k, v)) in entries.iter().enumerate() {
        let base = HEADER_LEN + i * LEAF_ENTRY_LEN;
        page[base..base + KEY_LEN].copy_from_slice(&k.encode());
        page[base + LEAF_LBA_OFF..base + LEAF_CSUM_OFF].copy_from_slice(&v.lba.0.to_le_bytes());
        page[base + LEAF_CSUM_OFF..base + LEAF_LEN_OFF]
            .copy_from_slice(&v.data_checksum.0.to_le_bytes());
        page[base + LEAF_LEN_OFF..base + LEAF_LEN_END].copy_from_slice(&v.byte_len.to_le_bytes());
    }
    seal_checksum(&mut page);
    Ok(page)
}

pub fn encode_internal(
    page_size: usize,
    txn_id: u64,
    keys: &[PageKey],
    children: &[Lba],
) -> Result<Vec<u8>, Error> {
    debug_assert_eq!(keys.len(), children.len());
    if keys.len() != children.len() {
        return Err(Error::Corrupt);
    }
    if keys.len() > max_internal_keys(page_size) {
        return Err(Error::OutOfSpace);
    }
    let mut page = vec![0u8; page_size];
    page[0] = PAGE_TYPE_INTERNAL;
    page[2..4].copy_from_slice(&(keys.len() as u16).to_le_bytes());
    page[8..16].copy_from_slice(&txn_id.to_le_bytes());
    for (i, k) in keys.iter().enumerate() {
        let base = HEADER_LEN + i * INTERNAL_ENTRY_LEN;
        page[base..base + KEY_LEN].copy_from_slice(&k.encode());
        page[base + INT_CHILD_OFF..base + INT_CHILD_END]
            .copy_from_slice(&children[i].0.to_le_bytes());
    }
    seal_checksum(&mut page);
    Ok(page)
}

pub fn encode_meta(page_size: usize, txn_id: u64, root_lba: Lba, hwm: u64) -> Vec<u8> {
    let mut page = vec![0u8; page_size];
    page[0] = PAGE_TYPE_META;
    page[2..4].copy_from_slice(&1u16.to_le_bytes());
    page[8..16].copy_from_slice(&txn_id.to_le_bytes());
    page[HEADER_LEN..HEADER_LEN + 8].copy_from_slice(&root_lba.0.to_le_bytes());
    page[HEADER_LEN + 8..HEADER_LEN + 16].copy_from_slice(&hwm.to_le_bytes());
    seal_checksum(&mut page);
    page
}

pub fn encode_empty_leaf(page_size: usize, txn_id: u64) -> Vec<u8> {
    encode_leaf(page_size, txn_id, &[]).expect("empty leaf always fits")
}

fn seal_checksum(page: &mut [u8]) {
    for b in &mut page[HDR_CSUM_OFF..HDR_CSUM_END] {
        *b = 0;
    }
    let cs = Xxh3Checksum::checksum_of(page).0;
    page[HDR_CSUM_OFF..HDR_CSUM_END].copy_from_slice(&cs.to_le_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(i: u32) -> PageKey {
        PageKey::new([0u8; 32], i)
    }

    #[test]
    fn leaf_roundtrip() {
        let entries = vec![
            (
                key(1),
                LeafEntry {
                    lba: Lba(100),
                    data_checksum: Checksum(0xdead),
                    byte_len: 2048,
                },
            ),
            (
                key(2),
                LeafEntry {
                    lba: Lba(200),
                    data_checksum: Checksum(0xbeef),
                    byte_len: 1024,
                },
            ),
        ];
        let mut p = encode_leaf(4096, 42, &entries).unwrap();
        match decode(&mut p) {
            Decoded::Leaf {
                txn_id,
                entries: got,
            } => {
                assert_eq!(txn_id, 42);
                assert_eq!(got, entries);
            }
            other => panic!("expected leaf, got {other:?}"),
        }
    }

    #[test]
    fn internal_roundtrip() {
        let keys = vec![key(1), key(5), key(9)];
        let children = vec![Lba(10), Lba(20), Lba(30)];
        let mut p = encode_internal(4096, 7, &keys, &children).unwrap();
        match decode(&mut p) {
            Decoded::Internal {
                txn_id,
                keys: gk,
                children: gc,
            } => {
                assert_eq!(txn_id, 7);
                assert_eq!(gk, keys);
                assert_eq!(gc, children);
            }
            other => panic!("expected internal, got {other:?}"),
        }
    }

    #[test]
    fn meta_roundtrip() {
        let mut p = encode_meta(4096, 99, Lba(12345), 7777);
        match decode(&mut p) {
            Decoded::Meta {
                txn_id,
                root_lba,
                hwm,
            } => {
                assert_eq!(txn_id, 99);
                assert_eq!(root_lba, Lba(12345));
                assert_eq!(hwm, 7777);
            }
            other => panic!("expected meta, got {other:?}"),
        }
    }

    #[test]
    fn meta_legacy_zero_hwm() {
        // Documents the legacy-format compatibility behavior: a
        // meta page written with hwm = 0 decodes back as hwm = 0,
        // matching what an old encoder that left the trailing
        // reserved bytes zeroed would produce.
        let mut p = encode_meta(4096, 1, Lba(42), 0);
        match decode(&mut p) {
            Decoded::Meta { hwm, .. } => assert_eq!(hwm, 0),
            other => panic!("expected meta, got {other:?}"),
        }
    }

    #[test]
    fn checksum_mismatch_decodes_as_empty() {
        let p = encode_meta(4096, 1, Lba(1), 0);
        let mut bad = p.clone();
        bad[HEADER_LEN] ^= 0x01; // flip a byte in the body
        let before = bad.clone();
        assert!(matches!(decode(&mut bad), Decoded::Empty));
        assert_eq!(bad, before);
    }

    #[test]
    fn decode_restores_checksum_bytes() {
        let mut page = encode_meta(4096, 1, Lba(1), 0);
        let before = page.clone();

        assert!(matches!(decode(&mut page), Decoded::Meta { .. }));
        assert_eq!(page, before);
    }

    #[test]
    fn unknown_page_type_is_empty() {
        let mut p = encode_meta(4096, 1, Lba(1), 0);
        p[0] = 99;
        // reseal so checksum is valid but type is unknown
        seal_checksum(&mut p);
        assert!(matches!(decode(&mut p), Decoded::Empty));
    }

    #[test]
    fn leaf_fanout_sanity() {
        assert_eq!(max_leaf_entries(4096), 72);
        assert_eq!(max_internal_keys(4096), 92);
    }
}
