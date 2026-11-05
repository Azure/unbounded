//! Epoch-published immutable publication cell with weakly indexed retained parts.
//!
//! Writers validate and commit under one mutex, store the new immutable
//! generation, then advance an atomic epoch with Release ordering. Readers on
//! the hot path keep a worker-local copy of what they need and reload it only
//! when an Acquire load of the epoch differs from the epoch they cached, so a
//! steady-state access takes no lock and touches no shared reference count.
//! Grace retention of superseded parts (`resolve`, `resolve_recent`) stays
//! behind the mutex because it is only used on cold paths.

use crate::error::{Error, Result};
use std::{
    collections::BTreeMap,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicU64, Ordering},
    },
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
/// upgrade can race admission and exceed this soft ceiling. Existing leases and
/// their upgrades are never invalidated or revoked.
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

/// How a proposed generation relates to the accepted one.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Accepted {
    /// The exact accepted generation was delivered again; nothing changes.
    Exact,

    /// A newer cursor that shares the accepted retained part.
    SamePart,

    /// A newer cursor with a newer retained part (or the first generation).
    NewPart,
}

/// The single replay and monotonicity check for publications.
///
/// Rejects cursor or part rollback, same-cursor content changes, and a reused
/// part version whose content or allocation differs.
pub fn accept<T: Generation>(prev: Option<&T>, next: &T) -> Result<Accepted> {
    let Some(old) = prev else {
        return Ok(Accepted::NewPart);
    };
    if next.version() < old.version() || next.part_version() < old.part_version() {
        return Err(Error::Replay);
    }
    if next.version() == old.version() {
        if next.digest() != old.digest() || next.part_version() != old.part_version() {
            return Err(Error::Replay);
        }
        return Ok(Accepted::Exact);
    }
    if next.part_version() == old.part_version() {
        if next.part_digest() != old.part_digest() || !Arc::ptr_eq(next.part(), old.part()) {
            return Err(Error::IncompatibleMembership);
        }
        return Ok(Accepted::SamePart);
    }
    Ok(Accepted::NewPart)
}

/// Node-wide immutable state. Replacement admission and cold reads share one
/// lock; hot readers follow `epoch` instead.
pub struct PublicationCell<T: Generation> {
    retention: Retention,

    /// Advanced with Release after every visible replacement of `current`.
    epoch: AtomicU64,

    state: Mutex<State<T>>,
}

/// All mutable authority resides under the publication mutex.
struct State<T: Generation> {
    current: Option<Arc<T>>,

    parts: BTreeMap<T::PartVersion, Weak<T::Part>>,

    grace: Vec<(Instant, T::PartVersion, Arc<T::Part>)>,
}

impl<T: Generation> PublicationCell<T> {
    /// Start without an accepted generation.
    pub fn new(retention: Retention) -> Self {
        Self {
            retention,
            epoch: AtomicU64::new(0),
            state: Mutex::new(State {
                current: None,
                parts: BTreeMap::new(),
                grace: Vec::new(),
            }),
        }
    }

    /// Current publication epoch. A worker whose cached copy carries this epoch
    /// is up to date. Pairs with the Release store in `publish`.
    #[inline]
    pub fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }

    /// Lease the complete current publication, if one has been accepted.
    pub fn current(&self) -> Result<Option<Arc<T>>> {
        Ok(self.lock()?.current.clone())
    }

    /// Lease the current publication together with the epoch that names it.
    pub fn current_with_epoch(&self) -> Result<(u64, Option<Arc<T>>)> {
        let state = self.lock()?;
        // The epoch only changes under this lock, so it names `current` exactly.
        Ok((self.epoch.load(Ordering::Relaxed), state.current.clone()))
    }

    /// Lock the state, mapping poisoning to an internal error.
    fn lock(&self) -> Result<std::sync::MutexGuard<'_, State<T>>> {
        self.state.lock().map_err(|_| Error::Internal)
    }

    /// Resolve a retained part without extending its grace lifetime.
    pub fn resolve(&self, version: T::PartVersion, now: Instant) -> Result<Option<Arc<T::Part>>> {
        let mut retired = Vec::new();
        let mut state = self.lock()?;
        state.expire(now, &mut retired);
        if retired.is_empty() {
            return Ok(state.parts.get(&version).and_then(Weak::upgrade));
        }
        // Release grace before resolving, so lookup cannot revive a part solely
        // because its expired structural owner is awaiting unlocked destruction.
        drop(state);
        drop(retired);
        let state = self.lock()?;
        Ok(state.parts.get(&version).and_then(Weak::upgrade))
    }

    /// Resolve a part held for grace retention that was superseded at most
    /// `within` before `now`, without extending its lifetime. Peers keep the
    /// same grace window, so a version this node replaced recently is one
    /// that a peer which adopted the replacement no earlier than this node
    /// minus (`grace` - `within`) still resolves. Parts retained only by
    /// other leases, or held longer than `within`, are not returned: their
    /// retention at a peer is unknown. `None` when grace retention is off.
    pub fn resolve_recent(
        &self,
        version: T::PartVersion,
        now: Instant,
        within: Duration,
    ) -> Result<Option<Arc<T::Part>>> {
        if within > self.retention.grace {
            return Ok(None);
        }
        let mut retired = Vec::new();
        let mut state = self.lock()?;
        state.expire(now, &mut retired);
        // A grace entry expires `grace` after its part was superseded.
        let recent = state
            .grace
            .iter()
            .find(|(expires, v, _)| {
                *v == version
                    && expires
                        .checked_sub(self.retention.grace)
                        .is_some_and(|superseded| {
                            now.saturating_duration_since(superseded) <= within
                        })
            })
            .map(|(_, _, part)| part.clone());
        drop(state);
        drop(retired);
        Ok(recent)
    }

    /// Validate and atomically replace current state, committing staged resources
    /// under the same lock. Both callbacks must be short and must not reenter this
    /// cell. Commit must be infallible and must not panic. A replay never commits.
    /// Fallible validation or admission never changes the accepted cursor.
    pub fn publish<E: From<Error>>(
        &self,
        next: Arc<T>,
        now: Instant,
        validate: impl FnOnce(Option<&T>, &T) -> std::result::Result<(), E>,
        commit: impl FnOnce(),
    ) -> std::result::Result<Arc<T>, E> {
        // Declare destruction owners before the guard, including on error exits.
        let mut retired = Vec::new();
        let retired_snapshot;
        let mut state = self.lock()?;
        let accepted = accept(state.current.as_deref(), &next)?;
        validate(state.current.as_deref(), &next)?;
        if accepted == Accepted::Exact
            && let Some(old) = &state.current
        {
            return Ok(old.clone());
        }
        state.expire(now, &mut retired);
        let same_part = accepted == Accepted::SamePart;
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
                return Err(Error::Overloaded.into());
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
        // Publish the new generation to epoch readers. Release pairs with the
        // Acquire load in `epoch`, so a reader that observes the new epoch and
        // then locks sees `current` (and everything committed above).
        self.epoch.fetch_add(1, Ordering::Release);
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

/// Worker-local copy of a value derived from the current publication.
///
/// `get` costs one Acquire load and a compare while the epoch is unchanged.
/// Only after a publication does it lock the cell and rebuild the copy. The
/// copy must not pin the retained part (for example by holding the whole
/// generation), or idle workers would consume grace capacity.
pub struct EpochCopy<V> {
    epoch: std::cell::Cell<u64>,

    value: std::cell::RefCell<Option<V>>,
}

impl<V> Default for EpochCopy<V> {
    fn default() -> Self {
        Self {
            epoch: std::cell::Cell::new(u64::MAX),
            value: std::cell::RefCell::new(None),
        }
    }
}

impl<V> EpochCopy<V> {
    /// Run `f` over the current derived value, refreshing it from `cell` with
    /// `derive` first when a newer publication exists.
    #[inline]
    pub fn with<T: Generation, R>(
        &self,
        cell: &PublicationCell<T>,
        derive: impl FnOnce(&T) -> V,
        f: impl FnOnce(Option<&V>) -> R,
    ) -> R {
        if cell.epoch() != self.epoch.get() {
            self.refresh(cell, derive);
        }
        f(self.value.borrow().as_ref())
    }

    /// Reload from the cell under its lock.
    #[cold]
    fn refresh<T: Generation>(&self, cell: &PublicationCell<T>, derive: impl FnOnce(&T) -> V) {
        let Ok((epoch, current)) = cell.current_with_epoch() else {
            // A poisoned cell admits nothing; retry on the next access.
            *self.value.borrow_mut() = None;
            return;
        };
        let next = current.as_deref().map(derive);
        let old = self.value.replace(next);
        self.epoch.set(epoch);
        drop(current);
        drop(old);
    }
}

#[cfg(test)]
mod tests;
