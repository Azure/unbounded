// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Fixed logical ownership geometry shared by control validation and transports.
use std::{fmt, iter::FusedIterator, num::NonZeroU32};

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

/// Immutable geometry. All clients use the same slot count and hash reduction;
/// endpoint assignments are resolved separately within the pinned epoch.
#[derive(Debug)]
pub struct Topology {
    epoch: Epoch,
    slots: NonZeroU32,
    degree: u32,
}
impl Topology {
    /// Derive ceil(cube_root(slot_count)) using exact integer arithmetic.
    pub fn new(slot_count: u32, epoch: Epoch) -> Result<Self, Error> {
        let slots = NonZeroU32::new(slot_count).ok_or(Error::ZeroSlots)?;
        let (mut low, mut high) = (1_u32, slot_count.min(1626));
        while low < high {
            let mid = low + (high - low) / 2;
            if u64::from(mid).pow(3) < u64::from(slot_count) {
                low = mid + 1;
            } else {
                high = mid;
            }
        }
        Ok(Self {
            epoch,
            slots,
            degree: low,
        })
    }
    pub const fn epoch(&self) -> Epoch {
        self.epoch
    }
    pub const fn slot_count(&self) -> u32 {
        self.slots.get()
    }
    pub const fn degree(&self) -> u32 {
        self.degree
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
    /// Placement is independent of epoch and physical membership.
    pub fn owner(&self, digest: &[u8; 32]) -> Slot {
        let hash = u64::from_le_bytes(digest[..8].try_into().unwrap());
        Slot((hash % u64::from(self.slot_count())) as u32)
    }
    pub fn candidates(&self, owner: Slot) -> Result<Candidates, Error> {
        self.slot(owner.0)?;
        Ok(Candidates {
            next: owner.0,
            remaining: self.slot_count(),
            slots: self.slots,
        })
    }
    pub fn route(&self, source: Slot, destination: Slot) -> Result<Route<'_>, Error> {
        self.slot(source.0)?;
        self.slot(destination.0)?;
        Ok(Route {
            topology: self,
            current: source,
            destination,
        })
    }
    // Minimal k in 0..=3 with (v - d^k*u) mod P < d^k. Taking the first
    // digit yields rank k-1 and identical destination-rooted suffixes.
    fn reach(&self, source: Slot, destination: Slot) -> (u8, u64, u64) {
        let p = u64::from(self.slot_count());
        let mut power = 1_u64;
        for k in 0..=3 {
            let start = (power % p) * u64::from(source.0) % p;
            let t = (u64::from(destination.0) + p - start) % p;
            if t < power {
                return (k, t, power);
            }
            power *= u64::from(self.degree);
        }
        unreachable!("d^3 >= P")
    }
    pub fn distance(&self, source: Slot, destination: Slot) -> Result<u8, Error> {
        self.slot(source.0)?;
        self.slot(destination.0)?;
        Ok(self.reach(source, destination).0)
    }
    fn neighbor(&self, source: Slot, digit: u32) -> Slot {
        Slot(
            ((u64::from(self.degree) * u64::from(source.0) + u64::from(digit))
                % u64::from(self.slot_count())) as u32,
        )
    }
}
/// Allocation-free owner/fallback traversal. Exhaustion is permanent.
#[derive(Debug)]
pub struct Candidates {
    next: u32,
    remaining: u32,
    slots: NonZeroU32,
}
impl Iterator for Candidates {
    type Item = Slot;
    fn next(&mut self) -> Option<Slot> {
        if self.remaining == 0 {
            return None;
        }
        let slot = Slot(self.next);
        self.remaining -= 1;
        self.next += 1;
        if self.next == self.slots.get() {
            self.next = 0;
        }
        Some(slot)
    }
    fn size_hint(&self) -> (usize, Option<usize>) {
        match usize::try_from(self.remaining) {
            Ok(n) => (n, Some(n)),
            Err(_) => (usize::MAX, None),
        }
    }
}
impl FusedIterator for Candidates {}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use]
pub enum Step {
    Arrived,
    Forward { next: Slot },
}
/// In-memory cursor pinned to geometry and algorithm; wire adapters validate
/// decoded identities and epoch before using it for network delivery.
#[derive(Debug)]
pub struct Route<'a> {
    topology: &'a Topology,
    current: Slot,
    destination: Slot,
}
impl Route<'_> {
    pub fn epoch(&self) -> Epoch {
        self.topology.epoch()
    }
    pub fn current(&self) -> Slot {
        self.current
    }
    pub fn destination(&self) -> Slot {
        self.destination
    }
    pub fn advance(&mut self) -> Step {
        if self.current == self.destination {
            return Step::Arrived;
        }
        let (_, t, power) = self.topology.reach(self.current, self.destination);
        let digit = (t / (power / u64::from(self.topology.degree))) as u32;
        self.current = self.topology.neighbor(self.current, digit);
        Step::Forward { next: self.current }
    }
}
#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/control/topology.rs"
));
