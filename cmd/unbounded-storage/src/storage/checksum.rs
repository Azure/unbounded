// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use crate::storage::types::Checksum;

pub(crate) fn checksum(bytes: &[u8]) -> Checksum {
    Checksum(twox_hash::xxh3::hash64(bytes))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn checksum_is_deterministic() {
        assert_eq!(checksum(b"hello world"), checksum(b"hello world"));
    }

    #[test]
    fn checksum_differs_for_different_inputs() {
        assert_ne!(checksum(b"hello world"), checksum(b"hello worle"));
    }

    #[test]
    fn checksum_empty_is_well_defined() {
        assert_eq!(checksum(&[]), checksum(&[]));
    }
}
