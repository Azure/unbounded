// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn specified_xxh64_scores_match_independent_libxxhash_vectors() {
    // Generated independently using Python hashlib and system libxxhash XXH64,
    // with the complete 36-byte canonical message (not the optimized prefix).
    let prefix = super::score_prefix(
        &crate::model::identity_bytes("universe", "site-a"),
        &crate::model::identity_bytes("node", "node-uid"),
    );
    for (slot, expected) in [
        (0, 0x51bbdc24387aa2c8),
        (1, 0xe14b4688f1764d77),
        (262143, 0x17eaad839e375d92),
        (u32::MAX, 0x54adf5c7edba562d),
    ] {
        assert_eq!(super::score(prefix, slot), expected);
    }
}
