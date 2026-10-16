//! The in-flight table: a generation-tagged slab of [`Entry`] values and the
//! per-operation fence state machine.
//!
//! A key is the `user_data` value of an operation's original submission:
//! the low 32 bits are the slot index and bits 32..63 are the slot's
//! generation, so a stale or forged completion for a reused slot is detected
//! instead of being attributed to the new occupant. Bit 63 is never part of a
//! key; it marks the completion of the cancel request for that key.

use std::any::Any;
use std::ptr::NonNull;
use std::task::Waker;

use crate::{Backend, CancelRegistration, CancelToken};

use super::admission::Partition;

/// Set in the `user_data` of a cancel request.
pub(super) const CANCEL_BIT: u64 = 1 << 63;

/// Generations use 31 bits so that a key never has [`CANCEL_BIT`] set.
const GENERATION_MASK: u32 = (1 << 31) - 1;

/// Type-erased access to an operation's heap node.
///
/// The node owns everything the backend may touch (see the
/// [module docs](super)). It is allocated with `Box::into_raw` when the
/// operation is submitted and freed exactly once: by the operation's future
/// after the fence, or by the reactor if the future was dropped first. If
/// neither happens it is leaked, never freed early.
pub(super) trait Node<B: Backend> {
    /// Run the scope's `check`, returning `true` if it failed.
    fn scope_failed(&self) -> bool;

    /// The scope, for [`Reactor::fence_matching`](super::Reactor::fence_matching).
    fn scope_any(&self) -> &dyn Any;

    /// Record a successful original completion (for example, adopt an
    /// accepted descriptor so that it is closed if nobody collects it).
    fn deliver(&mut self, backend: &mut B, result: i32);
}

/// Progress of the cancel request for one operation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum CancelFence {
    /// No cancel request was pushed; none is expected.
    None,
    /// Cancellation was requested; the request waits in the cancel queue.
    Queued,
    /// The cancel request was pushed and its completion is outstanding.
    Pushed,
    /// The cancel request completed before the original.
    Completed,
}

/// Where an operation is in its lifecycle.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Phase {
    /// The original completion has not arrived.
    InFlight(CancelFence),
    /// The original completed with `result`, but a pushed cancel request has
    /// not; the node is still retained.
    AwaitingCancel(i32),
    /// Every completion arrived. The backend no longer references the node;
    /// the result waits for the future to collect it.
    Done(i32),
}

/// One submitted operation.
pub(super) struct Entry<B: Backend> {
    pub(super) node: NonNull<dyn Node<B>>,
    pub(super) phase: Phase,
    /// Cancellation was requested (scope failure, dropped future, drain or
    /// fence). Decides how the result is reported.
    pub(super) cancel_requested: bool,
    /// The future was dropped; the reactor frees the node at the fence.
    pub(super) abandoned: bool,
    pub(super) waker: Option<Waker>,
    pub(super) partition: Partition,
    pub(super) token: Option<CancelToken>,
    pub(super) registration: Option<CancelRegistration>,
}

impl<B: Backend> Entry<B> {
    /// Whether the backend may still reference the node.
    pub(super) fn in_flight(&self) -> bool {
        !matches!(self.phase, Phase::Done(_))
    }
}

/// How a completion changed an entry.
#[derive(Debug, Eq, PartialEq)]
pub(super) enum Transition {
    /// Accounted, but another completion is still required.
    Partial,
    /// Every required completion arrived; the entry is now [`Phase::Done`].
    Fenced,
}

/// A completion that does not match the state machine.
#[derive(Debug, Eq, PartialEq)]
pub(super) struct Unexpected;

impl Phase {
    /// Account one completion. `cancel` says whether it belongs to the
    /// cancel request rather than the original.
    pub(super) fn complete(&mut self, cancel: bool, result: i32) -> Result<Transition, Unexpected> {
        match (*self, cancel) {
            (Phase::InFlight(CancelFence::Pushed), true) => {
                // Even -ENOENT/-EALREADY prove that the request finished.
                *self = Phase::InFlight(CancelFence::Completed);
                Ok(Transition::Partial)
            }
            (Phase::AwaitingCancel(original), true) => {
                *self = Phase::Done(original);
                Ok(Transition::Fenced)
            }
            (Phase::InFlight(CancelFence::Pushed), false) => {
                *self = Phase::AwaitingCancel(result);
                Ok(Transition::Partial)
            }
            (Phase::InFlight(_), false) => {
                // A queued request is simply never pushed.
                *self = Phase::Done(result);
                Ok(Transition::Fenced)
            }
            _ => Err(Unexpected),
        }
    }
}

/// A slot: its current generation and occupant (or the next free slot).
struct Slot<T> {
    generation: u32,
    value: Option<T>,
    next_free: Option<u32>,
}

/// A generation-tagged slab.
pub(super) struct Slab<T> {
    slots: Vec<Slot<T>>,
    free: Option<u32>,
    len: usize,
}

impl<T> Default for Slab<T> {
    fn default() -> Self {
        Self {
            slots: Vec::new(),
            free: None,
            len: 0,
        }
    }
}

fn split(key: u64) -> (usize, u32) {
    ((key & 0xffff_ffff) as usize, (key >> 32) as u32)
}

fn join(index: usize, generation: u32) -> u64 {
    (u64::from(generation) << 32) | index as u64
}

impl<T> Slab<T> {
    /// The number of occupied slots.
    pub(super) fn len(&self) -> usize {
        self.len
    }

    /// The number of slots, occupied or not.
    pub(super) fn slot_count(&self) -> usize {
        self.slots.len()
    }

    /// The key the next [`insert`](Slab::insert) will return.
    ///
    /// Returns `None` if the slab cannot grow (more than `u32::MAX` slots).
    pub(super) fn vacant_key(&self) -> Option<u64> {
        match self.free {
            Some(index) => Some(join(index as usize, self.slots[index as usize].generation)),
            None => u32::try_from(self.slots.len())
                .ok()
                .map(|index| join(index as usize, 0)),
        }
    }

    /// Store `value`, returning its key (equal to the preceding
    /// [`vacant_key`](Slab::vacant_key)).
    pub(super) fn insert(&mut self, value: T) -> u64 {
        self.len += 1;
        match self.free {
            Some(index) => {
                let slot = &mut self.slots[index as usize];
                self.free = slot.next_free.take();
                slot.value = Some(value);
                join(index as usize, slot.generation)
            }
            None => {
                let index = self.slots.len();
                assert!(u32::try_from(index).is_ok(), "slab index overflow");
                self.slots.push(Slot {
                    generation: 0,
                    value: Some(value),
                    next_free: None,
                });
                join(index, 0)
            }
        }
    }

    pub(super) fn get(&self, key: u64) -> Option<&T> {
        let (index, generation) = split(key);
        let slot = self.slots.get(index)?;
        if slot.generation != generation {
            return None;
        }
        slot.value.as_ref()
    }

    pub(super) fn get_mut(&mut self, key: u64) -> Option<&mut T> {
        let (index, generation) = split(key);
        let slot = self.slots.get_mut(index)?;
        if slot.generation != generation {
            return None;
        }
        slot.value.as_mut()
    }

    /// Remove and return the value for `key`, retiring the key.
    pub(super) fn remove(&mut self, key: u64) -> Option<T> {
        let (index, generation) = split(key);
        let slot = self.slots.get_mut(index)?;
        if slot.generation != generation {
            return None;
        }
        let value = slot.value.take()?;
        slot.generation = slot.generation.wrapping_add(1) & GENERATION_MASK;
        slot.next_free = self.free;
        self.free = Some(index as u32);
        self.len -= 1;
        Some(value)
    }

    /// The occupant of slot `index`, with its key.
    pub(super) fn at(&self, index: usize) -> Option<(u64, &T)> {
        let slot = self.slots.get(index)?;
        slot.value
            .as_ref()
            .map(|value| (join(index, slot.generation), value))
    }

    /// Every occupant with its key.
    pub(super) fn iter(&self) -> impl Iterator<Item = (u64, &T)> + '_ {
        (0..self.slots.len()).filter_map(|index| self.at(index))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_are_generation_tagged() {
        let mut slab = Slab::default();
        let first = slab.vacant_key().unwrap();
        assert_eq!(slab.insert("a"), first);
        assert_eq!(slab.get(first), Some(&"a"));
        assert_eq!(slab.remove(first), Some("a"));
        assert_eq!(slab.get(first), None);
        assert_eq!(slab.remove(first), None);
        let second = slab.insert("b");
        assert_ne!(first, second, "a reused slot gets a new key");
        assert_eq!(second & 0xffff_ffff, first & 0xffff_ffff);
        assert_eq!(slab.get(first), None);
        assert_eq!(slab.len(), 1);
        assert_eq!(slab.iter().count(), 1);
        assert_eq!(slab.slot_count(), 1);
    }

    #[test]
    fn keys_never_use_the_cancel_bit() {
        let mut slab = Slab::default();
        slab.insert(());
        slab.slots[0].generation = GENERATION_MASK;
        let key = join(0, GENERATION_MASK);
        assert_eq!(key & CANCEL_BIT, 0);
        slab.remove(key).unwrap();
        assert_eq!(
            slab.slots[0].generation, 0,
            "generation wraps within 31 bits"
        );
    }

    #[test]
    fn free_list_is_lifo() {
        let mut slab = Slab::default();
        let a = slab.insert(1);
        let b = slab.insert(2);
        slab.remove(a);
        slab.remove(b);
        assert_eq!(slab.insert(3) & 0xffff_ffff, b & 0xffff_ffff);
        assert_eq!(slab.insert(4) & 0xffff_ffff, a & 0xffff_ffff);
        assert_eq!(slab.at(0).unwrap().1, &4);
    }

    #[test]
    fn fence_without_cancel() {
        let mut phase = Phase::InFlight(CancelFence::None);
        assert_eq!(phase.complete(false, 5), Ok(Transition::Fenced));
        assert_eq!(phase, Phase::Done(5));
        assert_eq!(
            phase.complete(false, 5),
            Err(Unexpected),
            "duplicate original"
        );
        assert_eq!(
            phase.complete(true, 0),
            Err(Unexpected),
            "cancel never pushed"
        );
    }

    #[test]
    fn queued_cancel_is_not_awaited() {
        let mut phase = Phase::InFlight(CancelFence::Queued);
        assert_eq!(phase.complete(true, 0), Err(Unexpected));
        assert_eq!(phase.complete(false, 1), Ok(Transition::Fenced));
    }

    #[test]
    fn original_then_cancel() {
        let mut phase = Phase::InFlight(CancelFence::Pushed);
        assert_eq!(phase.complete(false, 7), Ok(Transition::Partial));
        assert_eq!(phase, Phase::AwaitingCancel(7));
        assert_eq!(phase.complete(false, 7), Err(Unexpected));
        assert_eq!(
            phase.complete(true, -libc::EALREADY),
            Ok(Transition::Fenced)
        );
        assert_eq!(phase, Phase::Done(7), "the original result is kept");
    }

    #[test]
    fn cancel_then_original() {
        let mut phase = Phase::InFlight(CancelFence::Pushed);
        assert_eq!(phase.complete(true, 0), Ok(Transition::Partial));
        assert_eq!(phase.complete(true, 0), Err(Unexpected), "duplicate cancel");
        assert_eq!(
            phase.complete(false, -libc::ECANCELED),
            Ok(Transition::Fenced)
        );
        assert_eq!(phase, Phase::Done(-libc::ECANCELED));
    }
}
