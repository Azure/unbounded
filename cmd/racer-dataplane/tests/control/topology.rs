// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;
    fn topology(p: u32) -> Topology { Topology::new(p, Epoch::new(17)).unwrap() }
    #[test]
    fn validates_slot_space_and_foreign_slots() {
        assert!(matches!(Topology::new(0, Epoch::new(0)), Err(Error::ZeroSlots)));
        let t = topology(7);
        assert_eq!(t.slot(6).unwrap().get(), 6);
        assert_eq!(t.slot(7), Err(Error::SlotOutOfRange { slot: 7, slot_count: 7 }));
        assert_eq!(t.slot(u32::MAX), Err(Error::SlotOutOfRange { slot: u32::MAX, slot_count: 7 }));
    }
    #[test]
    fn fixed_placement_vectors_and_epoch_independence() {
        let t = topology(1000);
        let mut digest = [0; 32];
        assert_eq!(t.owner(&digest).get(), 0);
        digest[0] = 1;
        digest[1] = 2;
        assert_eq!(t.owner(&digest).get(), 513);
        digest[8..].fill(255);
        assert_eq!(t.owner(&digest).get(), 513);
        digest[..8].fill(255);
        assert_eq!(t.owner(&digest).get(), 615);
        assert_eq!(Topology::new(1000, Epoch::new(18)).unwrap().owner(&digest), t.owner(&digest));
        assert_eq!(topology(4096).owner(&digest).get(), 4095);
        assert_eq!(topology(u32::MAX).owner(&digest).get(), 0);
    }
}
