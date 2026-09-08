// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Deterministic synthetic object model used by load generation.

use crate::bufferpool::{Error, PageRef};

const OBJECT_PREFIX: &str = "/__unbounded/loadgen/v1/";
const SYNTHETIC_BLOCK_WORDS: usize = 512;
const SYNTHETIC_BLOCK_BYTES: usize = SYNTHETIC_BLOCK_WORDS * 8;
const INTRA_WORD_MUL_A: u64 = 0xD6E8_FEB8_6659_FD93;
const INTRA_WORD_MUL_B: u64 = 0xA076_1D64_78BD_642F;
const BLOCK_SALT_MUL: u64 = 0xE703_7ED1_A0B4_28DB;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SyntheticObjectId {
    pub seed: u64,
    pub ordinal: u64,
    pub hash: u64,
}

pub fn object_id(seed: u64, ordinal: u64) -> String {
    format!(
        "{OBJECT_PREFIX}{seed:016x}/{ordinal:016x}-{:016x}",
        object_hash(seed, ordinal),
    )
}

pub fn parse_object_id(raw: &str) -> Option<SyntheticObjectId> {
    let rest = raw.strip_prefix(OBJECT_PREFIX)?;
    let (seed_hex, object_hex) = rest.split_once('/')?;
    let (ordinal_hex, hash_hex) = object_hex.split_once('-')?;
    if seed_hex.len() != 16 || ordinal_hex.len() != 16 || hash_hex.len() != 16 {
        return None;
    }

    let seed = u64::from_str_radix(seed_hex, 16).ok()?;
    let ordinal = u64::from_str_radix(ordinal_hex, 16).ok()?;
    let hash = u64::from_str_radix(hash_hex, 16).ok()?;
    if hash != object_hash(seed, ordinal) {
        return None;
    }

    Some(SyntheticObjectId {
        seed,
        ordinal,
        hash,
    })
}

pub fn object_hash(seed: u64, ordinal: u64) -> u64 {
    splitmix64(seed ^ ordinal)
}

pub fn byte_at(seed: u64, ordinal: u64, offset: u64) -> u8 {
    word_bytes(seed, ordinal, offset / 8)[(offset % 8) as usize]
}

pub fn fill_pages(
    object: SyntheticObjectId,
    start_offset: u64,
    dsts: &[PageRef],
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let mut object_offset = start_offset;
    for page in dsts {
        let page_offset = page_byte_offset(page, page_size)?;
        for_pattern_ranges(
            object.seed,
            object.ordinal,
            object_offset,
            page.len as usize,
            |chunk, pattern| {
                // SAFETY: `page_offset + chunk.dst_offset` starts inside the registered backing owned by the shard.
                let dst = unsafe { backing_base.add(page_offset + chunk.dst_offset) };
                // SAFETY: `dst` points to `chunk.len` writable bytes and the source is an in-bounds subslice of `pattern`.
                unsafe {
                    std::ptr::copy_nonoverlapping(
                        pattern.as_ptr().add(chunk.pattern_offset),
                        dst,
                        chunk.len,
                    );
                }
                true
            },
        );
        object_offset = object_offset.wrapping_add(page.len as u64);
    }
    Ok(())
}

pub fn matches_bytes(object: SyntheticObjectId, start_offset: u64, body: &[u8]) -> bool {
    for_pattern_ranges(
        object.seed,
        object.ordinal,
        start_offset,
        body.len(),
        |chunk, pattern| {
            body[chunk.dst_offset..chunk.dst_offset + chunk.len]
                == pattern[chunk.pattern_offset..chunk.pattern_offset + chunk.len]
        },
    )
}

pub fn splitmix64(mut x: u64) -> u64 {
    x = x.wrapping_add(0x9E37_79B9_7F4A_7C15);
    x = (x ^ (x >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    x = (x ^ (x >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    x ^ (x >> 31)
}

fn page_byte_offset(page: &PageRef, page_size: usize) -> Result<usize, Error> {
    (page.page_idx as usize)
        .checked_mul(page_size)
        .and_then(|base| base.checked_add(page.offset as usize))
        .ok_or_else(|| Error::from("synthetic object: page byte offset overflow"))
}

fn word_bytes(seed: u64, ordinal: u64, word_idx: u64) -> [u8; 8] {
    let object_salt = synthetic_object_salt(seed, ordinal);
    let block_idx = word_idx / SYNTHETIC_BLOCK_WORDS as u64;
    let intra_word = word_idx % SYNTHETIC_BLOCK_WORDS as u64;
    let block_salt = synthetic_block_salt(object_salt, block_idx);
    synthetic_word(object_salt, block_salt, intra_word).to_le_bytes()
}

fn synthetic_object_salt(seed: u64, ordinal: u64) -> u64 {
    splitmix64(seed ^ ordinal.rotate_left(17))
}

fn synthetic_block_salt(object_salt: u64, block_idx: u64) -> u64 {
    splitmix64(object_salt ^ block_idx.wrapping_mul(BLOCK_SALT_MUL))
}

fn synthetic_word(object_salt: u64, block_salt: u64, intra_word: u64) -> u64 {
    let intra = intra_word
        .wrapping_mul(INTRA_WORD_MUL_A)
        .rotate_left(((intra_word as u32) & 31) + 1)
        ^ intra_word.wrapping_mul(INTRA_WORD_MUL_B).rotate_right(17);
    intra ^ object_salt ^ block_salt.rotate_left(((intra_word as u32).wrapping_mul(13)) & 63)
}

fn object_pattern(object_salt: u64, block_idx: u64) -> [u8; SYNTHETIC_BLOCK_BYTES] {
    let block_salt = synthetic_block_salt(object_salt, block_idx);
    let mut pattern = [0u8; SYNTHETIC_BLOCK_BYTES];
    for (word_idx, dst) in pattern.chunks_exact_mut(8).enumerate() {
        dst.copy_from_slice(
            &synthetic_word(object_salt, block_salt, word_idx as u64).to_le_bytes(),
        );
    }
    pattern
}

fn for_pattern_ranges<F>(seed: u64, ordinal: u64, start_offset: u64, len: usize, mut f: F) -> bool
where
    F: FnMut(PatternRange, &[u8; SYNTHETIC_BLOCK_BYTES]) -> bool,
{
    let object_salt = synthetic_object_salt(seed, ordinal);
    let mut next_object_offset = start_offset;
    let mut next_dst_offset = 0usize;
    let mut remaining = len;
    let mut block_idx_seen = None;
    let mut pattern = [0u8; SYNTHETIC_BLOCK_BYTES];

    while remaining > 0 {
        let block_idx = next_object_offset / SYNTHETIC_BLOCK_BYTES as u64;
        if block_idx_seen != Some(block_idx) {
            pattern = object_pattern(object_salt, block_idx);
            block_idx_seen = Some(block_idx);
        }

        let pattern_offset = (next_object_offset % SYNTHETIC_BLOCK_BYTES as u64) as usize;
        let len = remaining.min(SYNTHETIC_BLOCK_BYTES - pattern_offset);
        let chunk = PatternRange {
            pattern_offset,
            dst_offset: next_dst_offset,
            len,
        };

        if !f(chunk, &pattern) {
            return false;
        }

        next_object_offset = next_object_offset.wrapping_add(len as u64);
        next_dst_offset += len;
        remaining -= len;
    }

    true
}

struct PatternRange {
    pattern_offset: usize,
    dst_offset: usize,
    len: usize,
}

#[cfg(test)]
mod tests {
    use super::*;

    const PAGE_SIZE: usize = 16;

    #[test]
    fn object_id_round_trips_and_validates_hash() {
        let id = object_id(0x1234, 0x5678);
        assert_eq!(
            id,
            "/__unbounded/loadgen/v1/0000000000001234/0000000000005678-6a631dbadf9c8837"
        );

        let parsed = parse_object_id(&id).expect("parse object id");
        assert_eq!(parsed.seed, 0x1234);
        assert_eq!(parsed.ordinal, 0x5678);
        assert_eq!(parsed.hash, object_hash(0x1234, 0x5678));

        assert!(
            parse_object_id(
                "/__unbounded/loadgen/v1/0000000000001234/0000000000005678-0000000000000000"
            )
            .is_none()
        );
    }

    #[test]
    fn content_is_stable_by_absolute_offset() {
        let object = parse_object_id(&object_id(7, 11)).unwrap();
        let mut backing = vec![0u8; PAGE_SIZE * 2];
        let dsts = vec![
            PageRef {
                page_idx: 0,
                offset: 3,
                len: 5,
            },
            PageRef {
                page_idx: 1,
                offset: 2,
                len: 4,
            },
        ];

        fill_pages(object, 9, &dsts, backing.as_mut_ptr(), PAGE_SIZE).unwrap();

        assert!(matches_bytes(object, 9, &backing[3..8]));
        assert!(matches_bytes(
            object,
            14,
            &backing[PAGE_SIZE + 2..PAGE_SIZE + 6]
        ));
    }

    #[test]
    fn fill_matches_byte_contract_across_word_boundaries() {
        let object = parse_object_id(&object_id(13, 17)).unwrap();
        let mut backing = vec![0u8; PAGE_SIZE * 3];
        let dsts = vec![PageRef {
            page_idx: 1,
            offset: 1,
            len: 19,
        }];

        fill_pages(object, 5, &dsts, backing.as_mut_ptr(), PAGE_SIZE).unwrap();

        let filled = &backing[PAGE_SIZE + 1..PAGE_SIZE + 20];
        let expected: Vec<u8> = (0..19)
            .map(|idx| byte_at(object.seed, object.ordinal, 5 + idx))
            .collect();
        assert_eq!(filled, expected.as_slice());
        assert!(matches_bytes(object, 5, filled));
    }

    #[test]
    fn content_differs_by_absolute_synthetic_block() {
        let object = parse_object_id(&object_id(19, 23)).unwrap();
        let mut backing = vec![0u8; SYNTHETIC_BLOCK_BYTES + 32];
        let dsts = vec![PageRef {
            page_idx: 0,
            offset: 0,
            len: backing.len() as u32,
        }];

        fill_pages(object, 0, &dsts, backing.as_mut_ptr(), backing.len()).unwrap();

        assert_ne!(backing[0..32], backing[SYNTHETIC_BLOCK_BYTES..][..32]);
        assert!(matches_bytes(
            object,
            SYNTHETIC_BLOCK_BYTES as u64 - 8,
            &backing[SYNTHETIC_BLOCK_BYTES - 8..SYNTHETIC_BLOCK_BYTES + 24]
        ));
    }

    #[test]
    fn matches_bytes_rejects_block_shift() {
        let object = parse_object_id(&object_id(21, 25)).unwrap();
        let body: Vec<u8> = (0..64)
            .map(|idx| {
                byte_at(
                    object.seed,
                    object.ordinal,
                    SYNTHETIC_BLOCK_BYTES as u64 + idx,
                )
            })
            .collect();

        assert!(matches_bytes(object, SYNTHETIC_BLOCK_BYTES as u64, &body));
        assert!(!matches_bytes(object, 0, &body));
    }

    #[test]
    fn matches_bytes_rejects_corruption() {
        let object = parse_object_id(&object_id(23, 29)).unwrap();
        let mut body: Vec<u8> = (0..24)
            .map(|idx| byte_at(object.seed, object.ordinal, 3 + idx))
            .collect();

        assert!(matches_bytes(object, 3, &body));

        body[9] ^= 0xff;
        assert!(!matches_bytes(object, 3, &body));
    }

    #[test]
    fn chunked_matching_wraps_object_offset() {
        let object = parse_object_id(&object_id(31, 37)).unwrap();
        let body = [byte_at(object.seed, object.ordinal, u64::MAX)];

        assert!(matches_bytes(object, u64::MAX, &body));
    }
}
