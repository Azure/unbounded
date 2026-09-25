//! Sparse slab crash images, torn/reordered writes, and direct-I/O alignment faults.
use crate::error::{Error, Result};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WriteId(pub u64);

#[derive(Clone)]
struct Write {
    id: WriteId,
    offset: u64,
    bytes: Vec<u8>,
}

#[derive(Clone, Default)]
struct State {
    durable: BTreeMap<u64, u8>,
    pending: Vec<Write>,
    sequence: u64,
}

/// A byte-level fault fixture, not a filesystem or a durability promise for slabs.
/// Submitted writes are visible until crash; only explicitly persisted bytes survive.
/// Sparse holes read as zero and all extents are bounded by the logical capacity.
#[derive(Clone)]
pub struct CrashDisk {
    capacity: u64,
    state: RefCell<State>,
}

impl Default for CrashDisk {
    fn default() -> Self {
        Self::new(1024 * 1024)
    }
}

impl CrashDisk {
    pub fn new(capacity: u64) -> Self {
        Self {
            capacity,
            state: RefCell::new(State::default()),
        }
    }

    fn extent(&self, offset: u64, length: usize) -> Result<()> {
        let length = u64::try_from(length).map_err(|_| Error::InvalidRange)?;
        if offset
            .checked_add(length)
            .is_none_or(|end| end > self.capacity)
        {
            return Err(Error::InvalidRange);
        }
        Ok(())
    }

    pub fn write(&self, offset: u64, bytes: &[u8]) -> Result<WriteId> {
        self.extent(offset, bytes.len())?;
        let mut state = self.state.borrow_mut();
        let next = state.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        let id = WriteId(state.sequence);
        state.pending.push(Write {
            id,
            offset,
            bytes: bytes.to_vec(),
        });
        state.sequence = next;
        Ok(id)
    }

    /// Exercise discovered alignment with an actual buffer address, offset, and length.
    /// This only checks constraints; it does not open or silently buffer O_DIRECT I/O.
    pub fn write_direct(
        &self,
        offset: u64,
        bytes: &[u8],
        memory_alignment: usize,
        offset_alignment: u64,
        length_alignment: usize,
    ) -> Result<WriteId> {
        if !memory_alignment.is_power_of_two() || offset_alignment == 0 || length_alignment == 0 {
            return Err(Error::DirectIoUnsupported);
        }
        if !(bytes.as_ptr() as usize).is_multiple_of(memory_alignment)
            || !offset.is_multiple_of(offset_alignment)
            || bytes.is_empty()
            || !bytes.len().is_multiple_of(length_alignment)
        {
            return Err(Error::InvalidRange);
        }
        self.write(offset, bytes)
    }

    pub fn read(&self, offset: u64, length: usize) -> Result<Vec<u8>> {
        self.extent(offset, length)?;
        let state = self.state.borrow();
        let mut result = vec![0; length];
        for (index, byte) in result.iter_mut().enumerate() {
            let address = offset + index as u64;
            *byte = *state.durable.get(&address).unwrap_or(&0);
            for write in &state.pending {
                if let Some(relative) = address.checked_sub(write.offset)
                    && let Ok(relative) = usize::try_from(relative)
                    && let Some(value) = write.bytes.get(relative)
                {
                    *byte = *value;
                }
            }
        }
        Ok(result)
    }

    /// Persist and retire exactly this write. A short prefix models a torn write;
    /// subsequent crash discards its unpersisted suffix. Order is caller-controlled.
    pub fn persist(&self, id: WriteId, prefix: usize) -> Result<()> {
        let mut state = self.state.borrow_mut();
        let index = state
            .pending
            .iter()
            .position(|write| write.id == id)
            .ok_or(Error::InvalidRequest)?;
        if prefix > state.pending[index].bytes.len() {
            return Err(Error::InvalidRange);
        }
        let write = state.pending.remove(index);
        for (index, byte) in write.bytes.into_iter().take(prefix).enumerate() {
            state.durable.insert(write.offset + index as u64, byte);
        }
        Ok(())
    }

    pub fn crash(&self) -> Result<()> {
        self.state.borrow_mut().pending.clear();
        Ok(())
    }

    /// Independent crash image after each prefix of an explicit persistence order.
    /// The first image loses every pending write; the source fixture is untouched.
    pub fn crash_prefixes(&self, order: &[(WriteId, usize)]) -> Result<Vec<Self>> {
        let mut seen = BTreeSet::new();
        let working = self.clone();
        let mut images = Vec::with_capacity(order.len() + 1);
        let image = working.clone();
        image.crash()?;
        images.push(image);
        for &(id, prefix) in order {
            if !seen.insert(id.0) {
                return Err(Error::InvalidRequest);
            }
            working.persist(id, prefix)?;
            let image = working.clone();
            image.crash()?;
            images.push(image);
        }
        Ok(images)
    }

    pub fn pending(&self) -> usize {
        self.state.borrow().pending.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crashprefix_covers_missing_torn_and_reordered_overwrites() {
        let disk = CrashDisk::new(1 << 40);
        let offset = (1 << 40) - 4;
        let initial = disk.write(offset, b"base").unwrap();
        disk.persist(initial, 4).unwrap();
        let first = disk.write(offset, b"AAAA").unwrap();
        let second = disk.write(offset + 1, b"BB").unwrap();
        assert_eq!(disk.read(offset, 4).unwrap(), b"ABBA");
        let images = disk.crash_prefixes(&[(second, 2), (first, 2)]).unwrap();
        for (image, expected) in images.iter().zip([b"base", b"bBBe", b"AABe"]) {
            assert_eq!(&image.read(offset, 4).unwrap(), expected);
            assert_eq!(image.pending(), 0);
        }
        assert_eq!(disk.pending(), 2);
        assert_eq!(disk.read(offset, 4).unwrap(), b"ABBA");
        disk.crash().unwrap();
        assert_eq!(disk.read(offset, 4).unwrap(), b"base");
        assert_eq!(disk.persist(first, 4), Err(Error::InvalidRequest));
    }

    #[test]
    fn invalid_fault_plans_and_extents_are_atomic_and_holes_are_zero() {
        let disk = CrashDisk::new(16);
        let id = disk.write(4, b"data").unwrap();
        assert_eq!(disk.persist(id, 5), Err(Error::InvalidRange));
        assert!(disk.crash_prefixes(&[(id, 4), (id, 4)]).is_err());
        assert_eq!(disk.pending(), 1);
        assert_eq!(disk.read(0, 4).unwrap(), [0; 4]);
        assert_eq!(disk.write(u64::MAX, &[1]), Err(Error::InvalidRange));
        assert_eq!(disk.read(15, 2), Err(Error::InvalidRange));
        disk.crash().unwrap();
        assert_ne!(disk.write(0, b"new").unwrap(), id);
    }

    #[test]
    fn direct_io_faults_check_address_offset_and_length_independently() {
        #[repr(align(16))]
        struct Aligned([u8; 32]);
        let bytes = Aligned([7; 32]);
        let disk = CrashDisk::new(64);
        assert_eq!(
            disk.write_direct(0, &bytes.0, 0, 16, 16),
            Err(Error::DirectIoUnsupported)
        );
        assert_eq!(
            disk.write_direct(1, &bytes.0, 16, 16, 16),
            Err(Error::InvalidRange)
        );
        assert_eq!(
            disk.write_direct(0, &bytes.0[1..17], 16, 16, 16),
            Err(Error::InvalidRange)
        );
        assert_eq!(
            disk.write_direct(0, &bytes.0[..15], 16, 16, 16),
            Err(Error::InvalidRange)
        );
        assert_eq!(disk.pending(), 0);
        let id = disk.write_direct(16, &bytes.0, 16, 16, 16).unwrap();
        disk.persist(id, 32).unwrap();
        disk.crash().unwrap();
        assert_eq!(disk.read(16, 32).unwrap(), bytes.0);
    }
}
