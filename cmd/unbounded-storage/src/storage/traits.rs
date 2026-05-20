// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-module trait surface.
//!
//! This file is intentionally small: the meaningful traits
//! ([`BlockDevice`](crate::storage::blockdev::BlockDevice)) live in
//! the module that owns their concrete implementations. What lives
//! here are tiny helper traits that more than one storage submodule
//! needs to agree on - currently just the checksum routine used by
//! both the btree and the engine.

use crate::storage::types::Checksum;

/// Single source of truth for "how do we compute a 64-bit checksum
/// over a byte slice". Implemented as a trait so the btree and the
/// engine can share it without anyone reaching into the other.
///
/// The default impl uses xxh3 with the storage subsystem's fixed
/// seed (`0`); callers should treat `Checksum::ZERO` as "no
/// checksum recorded yet" but verify-time `0` is a real value too.
pub trait PageChecksum {
    fn checksum_of(bytes: &[u8]) -> Checksum;
}

/// Default checksum impl over [xxh3](twox_hash::xxh3). Lives as a
/// zero-sized type so callers can write
/// `<Xxh3Checksum as PageChecksum>::checksum_of(...)` without
/// having to construct anything.
pub struct Xxh3Checksum;

impl PageChecksum for Xxh3Checksum {
    fn checksum_of(bytes: &[u8]) -> Checksum {
        Checksum(twox_hash::xxh3::hash64(bytes))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn checksum_is_deterministic() {
        let a = Xxh3Checksum::checksum_of(b"hello world");
        let b = Xxh3Checksum::checksum_of(b"hello world");
        assert_eq!(a, b);
    }

    #[test]
    fn checksum_differs_for_different_inputs() {
        let a = Xxh3Checksum::checksum_of(b"hello world");
        let b = Xxh3Checksum::checksum_of(b"hello worle");
        assert_ne!(a, b);
    }

    #[test]
    fn checksum_empty_is_well_defined() {
        let a = Xxh3Checksum::checksum_of(&[]);
        let b = Xxh3Checksum::checksum_of(&[]);
        assert_eq!(a, b);
    }
}
