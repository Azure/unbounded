//! One coherent publication cell with versioned, weakly indexed retained parts.

use crate::Error;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, Weak},
    time::{Duration, Instant},
};

/// An immutable publication with an independently versioned, expensive part.
/// Version and digest accessors must be stable for the allocation's lifetime.
pub trait Generation {
    /// Monotonic publication cursor.
    type Version: Copy + Ord;

    /// Monotonic retained-part identity, independent of the publication cursor.
    type PartVersion: Copy + Ord;

    /// Shared expensive state retained by operation leases.
    type Part;

    /// Content fingerprint used to distinguish exact replay from conflict.
    type Digest: Eq;

    /// Return the publication cursor.
    fn version(&self) -> Self::Version;

    /// Return the retained part's version.
    fn part_version(&self) -> Self::PartVersion;

    /// Borrow the retained allocation. Same-version updates must share this Arc.
    fn part(&self) -> &Arc<Self::Part>;

    /// Return the fingerprint of the complete publication.
    fn digest(&self) -> &Self::Digest;

    /// Return the fingerprint of the retained part alone.
    fn part_digest(&self) -> &Self::Digest;

    /// Estimate retained allocation bytes for grace-budget accounting.
    fn part_bytes(part: &Self::Part) -> usize;
}

/// Caller-selected retention bounds. External leases are never revoked.
/// Admission observes strong Arc pins, not externally created Weak leases. A Weak
/// upgrade can race admission and exceed this soft ceiling. Enforcing a strict
/// ceiling against those upgrades requires an opaque lease API rather than raw
/// Arcs. Existing leases and their upgrades are never invalidated or revoked.
#[derive(Clone, Copy, Debug)]
pub struct Retention {
    /// Maximum observed old live parts in addition to the current part.
    pub old_parts: usize,

    /// Maximum old parts retained solely to serve late incoming requests.
    pub grace_parts: usize,

    /// Maximum estimated bytes held in the grace list, including pinned parts.
    pub grace_bytes: usize,

    /// How long old parts remain eligible for grace retention.
    pub grace: Duration,
}

/// Node-wide immutable state. Readers and replacement admission share one lock.
pub struct Published<T: Generation> {
    retention: Retention,

    state: Mutex<State<T>>,
}

/// All mutable authority resides under the publication mutex.
struct State<T: Generation> {
    current: Option<Arc<T>>,

    parts: BTreeMap<T::PartVersion, Weak<T::Part>>,

    grace: Vec<(Instant, T::PartVersion, Arc<T::Part>)>,
}

impl<T: Generation> Published<T> {
    /// Start without an accepted generation.
    pub fn new(retention: Retention) -> Self {
        Self {
            retention,
            state: Mutex::new(State {
                current: None,
                parts: BTreeMap::new(),
                grace: Vec::new(),
            }),
        }
    }

    /// Lease the complete current publication, if one has been accepted.
    pub fn current(&self) -> Result<Option<Arc<T>>, Error> {
        Ok(self
            .state
            .lock()
            .map_err(|_| Error::Internal)?
            .current
            .clone())
    }

    /// Resolve a retained part without extending its grace lifetime.
    pub fn resolve(
        &self,
        version: T::PartVersion,
        now: Instant,
    ) -> Result<Option<Arc<T::Part>>, Error> {
        let mut retired = Vec::new();
        let mut state = self.state.lock().map_err(|_| Error::Internal)?;
        state.expire(now, &mut retired);
        // Release grace before resolving, so lookup cannot revive a part solely
        // because its expired structural owner is awaiting unlocked destruction.
        drop(state);
        drop(retired);
        let mut state = self.state.lock().map_err(|_| Error::Internal)?;
        state.parts.retain(|_, part| part.strong_count() != 0);
        Ok(state.parts.get(&version).and_then(Weak::upgrade))
    }

    /// Validate and atomically replace current state, committing staged resources
    /// under the same lock. Both callbacks must be short and must not reenter this
    /// cell. Commit must be infallible and must not panic. A replay never commits.
    /// Fallible validation or admission never changes the accepted cursor.
    pub fn publish<E: From<Error>>(
        &self,
        next: Arc<T>,
        now: Instant,
        validate: impl FnOnce(Option<&T>, &T) -> Result<(), E>,
        commit: impl FnOnce(),
    ) -> Result<Arc<T>, E> {
        // Declare destruction owners before the guard, including on error exits.
        let mut retired = Vec::new();
        let retired_snapshot;
        let mut state = self.state.lock().map_err(|_| Error::Internal)?;
        if let Some(old) = &state.current {
            if next.version() < old.version() || next.part_version() < old.part_version() {
                return Err(Error::Replay.into());
            }
            if next.version() == old.version()
                && (next.digest() != old.digest() || next.part_version() != old.part_version())
            {
                return Err(Error::Replay.into());
            }
            if next.part_version() == old.part_version() && next.part_digest() != old.part_digest()
            {
                return Err(Error::Conflict.into());
            }
        }
        validate(state.current.as_deref(), &next)?;
        if let Some(old) = &state.current {
            if next.version() == old.version() {
                return Ok(old.clone());
            }
            if next.part_version() == old.part_version() && !Arc::ptr_eq(next.part(), old.part()) {
                return Err(Error::Conflict.into());
            }
        }
        state.expire(now, &mut retired);
        let same_part = state
            .current
            .as_ref()
            .is_some_and(|old| old.part_version() == next.part_version());
        if !same_part {
            // Grace-only leases may be evicted. Pinned parts always count, even
            // when only a snapshot rather than a direct part lease pins them.
            while state.live_parts(&retired) > self.retention.old_parts {
                let Some(index) = state
                    .grace
                    .iter()
                    .position(|(_, _, p)| Arc::strong_count(p) == 1)
                else {
                    break;
                };
                state.retire(index, &mut retired);
            }
            state.parts.retain(|_, p| p.strong_count() != 0);
            let replaceable = state.current.as_ref().is_some_and(|old| {
                Arc::strong_count(old) == 1 && Arc::strong_count(old.part()) == 1
            });
            if state
                .live_parts(&retired)
                .saturating_sub(usize::from(replaceable))
                > self.retention.old_parts
            {
                return Err(Error::Capacity.into());
            }
        }
        // All fallible checks precede the resource commit and visibility change.
        commit();
        if !same_part
            && self.retention.grace_parts != 0
            && !self.retention.grace.is_zero()
            // Admission may rely on releasing the current structural lease.
            // Do not turn that replaceable lease into an extra grace owner when
            // externally pinned old parts already consume the entire budget.
            && state.live_parts(&retired) <= self.retention.old_parts
        {
            if let Some(old) = &state.current
                && T::part_bytes(old.part()) <= self.retention.grace_bytes
            {
                let entry = (
                    now.checked_add(self.retention.grace).unwrap_or(now),
                    old.part_version(),
                    old.part().clone(),
                );
                state.grace.push(entry);
            }
            while state.grace.len() > self.retention.grace_parts.min(self.retention.old_parts)
                || state
                    .grace
                    .iter()
                    .fold(0usize, |n, (_, _, p)| n.saturating_add(T::part_bytes(p)))
                    > self.retention.grace_bytes
            {
                state.retire(0, &mut retired);
            }
        }
        retired_snapshot = state.current.replace(next.clone());
        state.parts.retain(|_, p| p.strong_count() != 0);
        state
            .parts
            .insert(next.part_version(), Arc::downgrade(next.part()));
        drop(state);
        drop(retired_snapshot);
        drop(retired);
        Ok(next)
    }
}

impl<T: Generation> State<T> {
    /// Remove one grace owner, releasing the final allocation after unlock.
    fn retire(&mut self, index: usize, retired: &mut Vec<Arc<T::Part>>) {
        let (_, _, part) = self.grace.remove(index);
        // Never prune while this owner can still support an external Weak
        // upgrade. Registry pruning uses only an observed zero strong count.
        retired.push(part);
    }

    /// Count live allocations without owners already scheduled for destruction.
    /// This is admission accounting only, never authority to remove a weak entry.
    /// External Weak upgrades can still race this documented soft capacity bound.
    fn live_parts(&self, retired: &[Arc<T::Part>]) -> usize {
        self.parts
            .values()
            .filter(|part| {
                let structural = retired
                    .iter()
                    .filter(|owner| std::ptr::eq(Arc::as_ptr(owner), part.as_ptr()))
                    .count();
                part.strong_count() > structural
            })
            .count()
    }

    /// Retire expired owners and prune the weak registry.
    fn expire(&mut self, now: Instant, retired: &mut Vec<Arc<T::Part>>) {
        for index in (0..self.grace.len()).rev() {
            if self.grace[index].0 <= now {
                self.retire(index, retired);
            }
        }
        self.parts.retain(|_, p| p.strong_count() != 0);
    }
}

#[cfg(test)]
mod tests {
    //! Destructor reentrancy and retained-allocation lifetime checks.

    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Destructor observer proves allocation cleanup occurs after unlock.
    struct Part {
        owner: Weak<Published<Value>>,

        drops: Arc<AtomicUsize>,
    }

    impl Drop for Part {
        fn drop(&mut self) {
            if let Some(owner) = self.owner.upgrade() {
                assert!(
                    owner.state.try_lock().is_ok(),
                    "part destroyed under publication lock"
                );
            }
            self.drops.fetch_add(1, Ordering::SeqCst);
        }
    }

    /// Publication with separately observable snapshot and part destructors.
    struct Value {
        version: u64,

        part: Arc<Part>,

        rescue: Option<Arc<Mutex<Option<Arc<Part>>>>>,
    }

    impl Drop for Value {
        fn drop(&mut self) {
            if let Some(rescue) = &self.rescue {
                *rescue.lock().unwrap() = Arc::downgrade(&self.part).upgrade();
            }
            if let Some(owner) = self.part.owner.upgrade() {
                assert!(
                    owner.state.try_lock().is_ok(),
                    "snapshot destroyed under publication lock"
                );
            }
        }
    }

    impl Generation for Value {
        type Version = u64;

        type PartVersion = u64;

        type Part = Part;

        type Digest = u64;

        fn version(&self) -> u64 {
            self.version
        }

        fn part_version(&self) -> u64 {
            self.version
        }

        fn part(&self) -> &Arc<Part> {
            &self.part
        }

        fn digest(&self) -> &u64 {
            &self.version
        }

        fn part_digest(&self) -> &u64 {
            &self.version
        }

        fn part_bytes(_: &Part) -> usize {
            1
        }
    }

    /// Release final snapshot and part owners only after the publication lock.
    #[test]
    fn final_allocations_drop_outside_lock_on_success_error_and_expiration() {
        let cell = Arc::new(Published::new(Retention {
            old_parts: 1,
            grace_parts: 1,
            grace_bytes: 1,
            grace: Duration::from_secs(1),
        }));
        let drops = Arc::new(AtomicUsize::new(0));
        let next = |version| {
            Arc::new(Value {
                version,
                part: Arc::new(Part {
                    owner: Arc::downgrade(&cell),
                    drops: drops.clone(),
                }),
                rescue: None,
            })
        };
        let now = Instant::now();
        cell.publish(next(1), now, |_, _| Ok::<_, Error>(()), || ())
            .unwrap();
        cell.publish(next(2), now, |_, _| Ok::<_, Error>(()), || ())
            .unwrap();
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        assert!(
            cell.publish(next(3), now, |_, _| Err(Error::Conflict), || ())
                .is_err()
        );
        assert_eq!(drops.load(Ordering::SeqCst), 1);
        assert!(
            cell.resolve(1, now + Duration::from_secs(2))
                .unwrap()
                .is_none()
        );
        assert_eq!(drops.load(Ordering::SeqCst), 2);
        cell.publish(next(4), now, |_, _| Ok::<_, Error>(()), || ())
            .unwrap();
        cell.publish(next(5), now, |_, _| Ok::<_, Error>(()), || ())
            .unwrap();
        assert_eq!(drops.load(Ordering::SeqCst), 3);
    }

    /// Snapshot destruction can upgrade its part after replacement unlocks.
    #[test]
    fn snapshot_destructor_upgrade_preserves_registry_until_actual_death() {
        let cell = Arc::new(Published::new(Retention {
            old_parts: 0,
            grace_parts: 0,
            grace_bytes: 0,
            grace: Duration::ZERO,
        }));
        let rescue = Arc::new(Mutex::new(None));
        let drops = Arc::new(AtomicUsize::new(0));
        for version in 1..=2 {
            let value = Arc::new(Value {
                version,
                part: Arc::new(Part {
                    owner: Arc::downgrade(&cell),
                    drops: drops.clone(),
                }),
                rescue: (version == 1).then(|| rescue.clone()),
            });
            cell.publish(value, Instant::now(), |_, _| Ok::<_, Error>(()), || ())
                .unwrap();
        }
        let rescued = rescue.lock().unwrap().take().unwrap();
        assert!(Arc::ptr_eq(
            &rescued,
            &cell.resolve(1, Instant::now()).unwrap().unwrap()
        ));
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        drop(rescued);
        assert!(cell.resolve(1, Instant::now()).unwrap().is_none());
        assert_eq!(drops.load(Ordering::SeqCst), 1);
    }
}
