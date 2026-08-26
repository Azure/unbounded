// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Concrete value types shared across the storage subsystem.
//!
//! Kept deliberately small: every field has a single, well-defined
//! meaning so that higher layers (btree, allocator, lru) can pass
//! them around without having to remember units or interpretations.

use std::fmt;

/// Identifies a single backing NVMe namespace in a storage shard.
/// One [`StorageEngine`](crate::storage::StorageEngine) drives one
/// disk, so the [`DiskId`] is mostly used at higher layers (hash
/// ring, telemetry).
#[derive(Copy, Clone, Eq, PartialEq, Hash, Debug)]
pub struct DiskId(pub u16);

/// Logical block address as exposed by a [`BlockDevice`](crate::storage::blockdev::BlockDevice).
///
/// Units are *block-device pages*, not bytes: callers multiply by
/// [`BlockDevice::page_size`](crate::storage::blockdev::BlockDevice::page_size)
/// when they need to talk to anything but the device.
#[derive(Copy, Clone, Eq, PartialEq, Ord, PartialOrd, Hash, Debug, Default)]
pub struct Lba(pub u64);

impl Lba {
    pub const INVALID: Lba = Lba(u64::MAX);
    pub fn is_valid(self) -> bool {
        self != Self::INVALID
    }
}

impl fmt::Display for Lba {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "lba:{}", self.0)
    }
}

/// Cache-side key. `value_hash` is the cryptographic content hash of
/// the *object* the page belongs to (32 bytes); `page_index` is the
/// page-within-object offset (a single object spans many cached
/// pages). The pair uniquely identifies a 2 MiB cache page on this
/// node.
#[derive(Copy, Clone, Eq, PartialEq, Ord, PartialOrd, Hash, Debug)]
pub struct PageKey {
    pub value_hash: [u8; 32],
    pub page_index: u32,
}

impl PageKey {
    pub fn new(value_hash: [u8; 32], page_index: u32) -> Self {
        Self {
            value_hash,
            page_index,
        }
    }

    /// Canonical byte encoding used as a btree key. Big-endian
    /// `page_index` so lexicographic order matches numeric order
    /// within a value.
    pub fn encode(&self) -> [u8; 36] {
        let mut out = [0u8; 36];
        out[..32].copy_from_slice(&self.value_hash);
        out[32..].copy_from_slice(&self.page_index.to_be_bytes());
        out
    }

    pub fn decode(bytes: &[u8]) -> Option<Self> {
        if bytes.len() != 36 {
            return None;
        }
        let mut vh = [0u8; 32];
        vh.copy_from_slice(&bytes[..32]);
        let mut pi = [0u8; 4];
        pi.copy_from_slice(&bytes[32..]);
        Some(Self {
            value_hash: vh,
            page_index: u32::from_be_bytes(pi),
        })
    }

    /// FNV-1a-style mixer over the key's bytes, tagged with a
    /// per-caller domain separator. Used by the admission doorkeeper
    /// and the singleflight shard selector. Not cryptographic;
    /// the `domain` tag is not a secret and only exists to give
    /// different callers near-independent 64-bit lanes.
    pub fn mix(&self, domain: u32) -> u64 {
        const FNV_OFFSET: u64 = 0xcbf29ce484222325;
        const FNV_PRIME: u64 = 0x100000001b3;
        let mut h: u64 = FNV_OFFSET ^ ((domain as u64).wrapping_mul(GOLDEN_RATIO_64));
        for b in self.value_hash {
            h ^= b as u64;
            h = h.wrapping_mul(FNV_PRIME);
        }
        h ^= self.page_index as u64;
        h.wrapping_mul(FNV_PRIME)
    }
}

/// 2^64 / phi, the standard 64-bit golden-ratio mixer constant
/// (Knuth, TAOCP 6.4). Used to decorrelate salt values and to
/// spread bloom-filter probe positions.
pub const GOLDEN_RATIO_64: u64 = 0x9e3779b97f4a7c15;

/// 64-bit page/data checksum. We use xxh3 (non-cryptographic but
/// fast and well-distributed) for both btree-page integrity and
/// payload integrity. The design only requires us to *detect*
/// corruption with high probability; we never use the checksum as
/// proof of authenticity.
#[derive(Copy, Clone, Eq, PartialEq, Hash, Debug, Default)]
pub struct Checksum(pub u64);

impl Checksum {
    pub const ZERO: Checksum = Checksum(0);
}

/// Failure modes surfaced by the storage subsystem. We deliberately
/// model very few variants: the design treats almost every failure
/// from the underlying disk as "this page is not in cache" (a miss),
/// so most call sites collapse `Err(Corrupt)` and `Ok(false)` into
/// the same path. The error type exists to let lower layers
/// distinguish I/O faults from logical misuse during testing.
#[derive(Debug, Clone)]
pub enum Error {
    /// Generic I/O error from the underlying device. The integer is
    /// `errno` when we get one from io_uring; `0` otherwise.
    Io(i32),
    /// Data read back failed checksum or had a structurally invalid
    /// layout. Higher layers convert this into a cache miss.
    Corrupt,
    /// Allocator is out of free pages on this disk.
    OutOfSpace,
    /// Operation was canceled because the engine is shutting down.
    Canceled,
    /// Caller passed an LBA / index outside the device's capacity.
    OutOfRange,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Io(e) => write!(f, "storage io error: errno={e}"),
            Error::Corrupt => write!(f, "storage corruption detected"),
            Error::OutOfSpace => write!(f, "storage allocator out of space"),
            Error::Canceled => write!(f, "storage operation canceled"),
            Error::OutOfRange => write!(f, "storage offset out of range"),
        }
    }
}

impl std::error::Error for Error {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn page_key_round_trips() {
        let k = PageKey::new([7u8; 32], 0xdead_beef);
        let bytes = k.encode();
        let back = PageKey::decode(&bytes).unwrap();
        assert_eq!(k, back);
    }

    #[test]
    fn page_key_orders_lexicographically_by_index_within_value() {
        let a = PageKey::new([1u8; 32], 1).encode();
        let b = PageKey::new([1u8; 32], 2).encode();
        let c = PageKey::new([2u8; 32], 0).encode();
        assert!(a < b);
        assert!(b < c);
    }

    #[test]
    fn page_key_decode_rejects_wrong_length() {
        assert!(PageKey::decode(&[0u8; 35]).is_none());
        assert!(PageKey::decode(&[0u8; 37]).is_none());
    }

    #[test]
    fn lba_invalid_is_max() {
        assert!(!Lba::INVALID.is_valid());
        assert!(Lba(0).is_valid());
    }
}
