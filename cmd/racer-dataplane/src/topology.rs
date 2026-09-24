// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Logical key-slot reduction, independent of physical product routing.
use std::{fmt, num::NonZeroU32};

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Slot(u32);
impl Slot {
    pub const fn get(self) -> u32 {
        self.0
    }
}
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Epoch(u64);
impl Epoch {
    pub const fn new(value: u64) -> Self {
        Self(value)
    }
    pub const fn get(self) -> u64 {
        self.0
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    ZeroSlots,
    SlotOutOfRange { slot: u32, slot_count: u32 },
}
impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ZeroSlots => f.write_str("topology requires at least one slot"),
            Self::SlotOutOfRange { slot, slot_count } => {
                write!(f, "slot {slot} is outside 0..{slot_count}")
            }
        }
    }
}
impl std::error::Error for Error {}
#[derive(Debug)]
pub struct Topology {
    epoch: Epoch,
    slots: NonZeroU32,
}
impl Topology {
    pub fn new(slot_count: u32, epoch: Epoch) -> Result<Self, Error> {
        Ok(Self {
            epoch,
            slots: NonZeroU32::new(slot_count).ok_or(Error::ZeroSlots)?,
        })
    }
    pub const fn epoch(&self) -> Epoch {
        self.epoch
    }
    pub const fn slot_count(&self) -> u32 {
        self.slots.get()
    }
    pub fn slot(&self, value: u32) -> Result<Slot, Error> {
        if value < self.slot_count() {
            Ok(Slot(value))
        } else {
            Err(Error::SlotOutOfRange {
                slot: value,
                slot_count: self.slot_count(),
            })
        }
    }
    /// First eight BLAKE3 digest bytes, little-endian, modulo slot count.
    pub fn owner(&self, digest: &[u8; 32]) -> Slot {
        Slot(
            (u64::from_le_bytes(digest[..8].try_into().unwrap()) % u64::from(self.slot_count()))
                as u32,
        )
    }
}
#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/control/topology.rs"
));
