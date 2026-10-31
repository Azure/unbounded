//! Generation-tagged barriers over an explicit, immutable worker registration set.

use crate::Error;
use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{Arc, Mutex},
};

/// Shared rollout authority. Required worker identities are fixed at creation.
pub struct Rollout<W, T> {
    required: BTreeMap<W, usize>,

    state: Mutex<State<T>>,
}

/// Worker preparation request; generation must accompany its acknowledgment.
pub struct Proposal<T> {
    /// Unique monotonically increasing proposal identity.
    pub generation: u64,

    /// Immutable state to prepare before acknowledging.
    pub value: Arc<T>,
}

/// Reservation of an acknowledged cut. Dropping it rolls back the cut's acks.
/// While reserved, supersession fails rather than invalidating an atomic commit.
pub struct Guard<'a, W: Ord, T> {
    rollout: &'a Rollout<W, T>,

    generation: u64,

    committed: bool,
}

/// Mutable proposal state. Worker resources remain with their local owners.
struct State<T> {
    generation: u64,

    value: Option<Arc<T>>,

    acknowledged: BTreeSet<usize>,

    reserved: bool,

    committed: bool,
}

impl<W: Ord + Clone, T> Rollout<W, T> {
    /// Register the exact workers required for every cut, including zero workers.
    pub fn new(required: impl IntoIterator<Item = W>) -> Self {
        Self {
            required: required
                .into_iter()
                .enumerate()
                .map(|(index, id)| (id, index))
                .collect(),
            state: Mutex::new(State {
                generation: 0,
                value: None,
                acknowledged: BTreeSet::new(),
                reserved: false,
                committed: false,
            }),
        }
    }

    /// Supersede pending work with a new generation. Old acknowledgments expire.
    /// Even equal values create a new generation; adapters may avoid reproposal.
    pub fn propose(&self, value: Arc<T>) -> Result<u64, Error> {
        let retired;
        let mut state = self.state.lock().map_err(|_| Error::Internal)?;
        if state.reserved {
            return Err(Error::Pending);
        }
        state.generation = state.generation.checked_add(1).ok_or(Error::Capacity)?;
        retired = state.value.replace(value);
        state.acknowledged.clear();
        state.committed = false;
        let generation = state.generation;
        drop(state);
        drop(retired);
        Ok(generation)
    }

    /// Return this registered worker's unacknowledged preparation request.
    pub fn pending(&self, worker: &W) -> Result<Option<Proposal<T>>, Error> {
        let index = self.required.get(worker).ok_or(Error::Stale)?;
        let state = self.state.lock().map_err(|_| Error::Internal)?;
        if state.committed || state.acknowledged.contains(index) {
            return Ok(None);
        }
        Ok(state.value.as_ref().map(|value| Proposal {
            generation: state.generation,
            value: value.clone(),
        }))
    }

    /// Acknowledge preparation only for the active generation and registered ID.
    pub fn ack(&self, worker: W, generation: u64) -> Result<(), Error> {
        let index = *self.required.get(&worker).ok_or(Error::Stale)?;
        let mut state = self.state.lock().map_err(|_| Error::Internal)?;
        if state.value.is_none() || state.generation != generation || state.committed {
            return Err(Error::Stale);
        }
        // Store only inert indices. User-owned IDs may run arbitrary destructors,
        // including reentrant queries, when duplicates or rolled-back acks drop.
        state.acknowledged.insert(index);
        Ok(())
    }

    /// Reserve a fully acknowledged cut for an infallible publication commit.
    pub fn stage(&self, generation: u64) -> Result<Guard<'_, W, T>, Error> {
        let mut state = self.state.lock().map_err(|_| Error::Internal)?;
        if state.value.is_none() || state.generation != generation || state.committed {
            return Err(Error::Stale);
        }
        if state.reserved || state.acknowledged.len() != self.required.len() {
            return Err(Error::Pending);
        }
        state.reserved = true;
        Ok(Guard {
            rollout: self,
            generation,
            committed: false,
        })
    }

    /// Return whether a particular cut has become committed.
    pub fn committed(&self, generation: u64) -> Result<bool, Error> {
        let state = self.state.lock().map_err(|_| Error::Internal)?;
        Ok(state.generation == generation && state.committed)
    }
}

impl<W: Ord, T> Guard<'_, W, T> {
    /// Mark this reserved cut committed. Call only from the publication commit.
    /// Resource installation belongs immediately before this call in that hook.
    pub fn commit(mut self) {
        let mut state = self.rollout.state.lock().unwrap_or_else(|e| e.into_inner());
        debug_assert_eq!(state.generation, self.generation);
        state.committed = true;
        state.reserved = false;
        self.committed = true;
    }
}

impl<W: Ord, T> Drop for Guard<'_, W, T> {
    /// Undo acknowledgments after failed admission, forcing local restaging.
    fn drop(&mut self) {
        if !self.committed {
            let mut state = self.rollout.state.lock().unwrap_or_else(|e| e.into_inner());
            if state.generation == self.generation {
                state.reserved = false;
                state.acknowledged.clear();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    //! Worker destructor reentrancy across acknowledgment and rollback.

    use super::*;
    use std::sync::Weak;

    /// Worker identity whose destructor may query its rollout owner.
    #[derive(Clone)]
    struct Worker {
        id: u64,

        owner: Weak<Rollout<Worker, ()>>,
    }

    impl PartialEq for Worker {
        fn eq(&self, other: &Self) -> bool {
            self.id == other.id
        }
    }

    impl Eq for Worker {}

    impl PartialOrd for Worker {
        fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
            Some(self.cmp(other))
        }
    }

    impl Ord for Worker {
        fn cmp(&self, other: &Self) -> std::cmp::Ordering {
            self.id.cmp(&other.id)
        }
    }

    impl Drop for Worker {
        fn drop(&mut self) {
            if let Some(owner) = self.owner.upgrade() {
                assert!(
                    owner.state.try_lock().is_ok(),
                    "worker dropped under barrier lock"
                );
            }
        }
    }

    /// Duplicate acknowledgments and guard rollback must not destroy user IDs
    /// while holding the barrier mutex, or reentrant worker cleanup deadlocks.
    #[test]
    fn worker_destruction_is_outside_barrier_lock() {
        let rollout = Arc::new_cyclic(|owner| {
            Rollout::new([
                Worker {
                    id: 1,
                    owner: owner.clone(),
                },
                Worker {
                    id: 1,
                    owner: owner.clone(),
                },
            ])
        });
        let worker = Worker {
            id: 1,
            owner: Arc::downgrade(&rollout),
        };
        let generation = rollout.propose(Arc::new(())).unwrap();
        rollout.ack(worker.clone(), generation).unwrap();
        rollout.ack(worker.clone(), generation).unwrap();
        drop(rollout.stage(generation).unwrap());
        assert!(rollout.pending(&worker).unwrap().is_some());
        rollout.ack(worker.clone(), generation).unwrap();
        rollout.stage(generation).unwrap().commit();
        assert!(rollout.committed(generation).unwrap());
    }
}
