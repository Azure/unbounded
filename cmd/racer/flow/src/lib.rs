//! Policy-driven admission, charged storage, and completion-owned flow control.
//!
//! Implement [`Class`] and [`Policy`] to supply resource limits and keyed fairness.
//! [`Quotas`] is a worker-local authority; [`SharedQuotas`] admits unkeyed work
//! across threads. Keep each [`Charge`] with its resource until completion. A
//! completion reservation bypasses stop and fair-share checks, not aggregate or
//! key-record limits. Policy can also allow selected classes after stop.
//!
//! Charges carry admission only; buffer backing and its idle reuse live in
//! `page_alloc::buf::Pool`. Callers validate class, key, and provenance.
//! Application authentication, error
//! classification, request deadlines, and cancellation policy stay with callers.
//!
//! Endpoint circuits, adaptive admission, handoffs, and hedge alarms use distinct
//! ownership rules. Pipes retain their charges while idle; socket-retained bytes
//! are outside the pipe capacity budget. [`pipe::PipePool::simulated`] creates
//! pipes in a simulated world without changing admission policy.
//!
//! [`coalesce`] provides worker-local flight state; the keyed table that drives it
//! lives in racer-core. Callers retain execution, admission, and result policy;
//! cancellation never substitutes for real operation completion.
#![deny(unsafe_op_in_unsafe_fn)]

pub use racer_flow_core::{Class, Error, Result, coalesce, fifo, progress, subscriptions};

/// Charged kernel pipes and worker-local reuse.
mod pipe;
mod pool_account;

pub use page_alloc::{PoolDemand, PoolStats};
pub use pipe::{MAX_PIPE_BYTES, PipeLease, PipePool, splice_unsupported};
pub use pool_account::QuotaPoolAccount;
pub use racer_flow_core::admission::{
    Adaptive, Admitted, Circuits, Config as AdaptiveConfig, Event as AdaptiveEvent, Handoff,
    HandoffAdmission, HedgePermit, Hedges, Observer as AdaptiveObserver, Offer,
    Outcome as AdaptiveOutcome, Permit as AdaptivePermit, Probe,
};

use std::{
    cell::{Cell, RefCell},
    collections::{HashMap, VecDeque},
    hash::Hash,
    marker::PhantomData,
    rc::Rc,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    },
};

/// Application policy. Charges do not retain this value, so suitability and
/// release notifications are static properties of the class.
pub trait Policy: 'static {
    /// Resource classes accounted by this policy.
    type Class: Class;

    /// Identity used to divide local fair shares.
    type Key: Clone + Eq + Hash + Send + Sync + 'static;

    /// Return the aggregate ceiling for a class.
    fn limit(&self, class: Self::Class) -> usize;

    /// Return the minimum keyed share, clipped to the aggregate ceiling.
    fn floor(&self, _class: Self::Class) -> usize {
        1
    }

    /// Return the per-key working-memory allowance of a weighted class.
    /// Weights never reduce a key's share below this allowance or the
    /// unweighted equal split, whichever is smaller. All concurrent work on
    /// the key shares the allowance, so it limits how far a low weight can
    /// shrink the share but does not guarantee that any request makes
    /// progress. Unweighted classes ignore it.
    fn progress(&self, _class: Self::Class) -> usize {
        0
    }

    /// Return the relative fair-share weight of a key. A key's share of a
    /// class is `limit * weight / sum(weights of active keys)`, still bounded
    /// below by `floor` and, for weighted classes, by `progress` capped at the
    /// equal split. Weight 0 grants no guaranteed share beyond those bounds.
    /// When every active weight is 0, active keys share equally. Values above
    /// `u32::MAX` are clamped. Weights are sampled when a key becomes active
    /// and again on each `Quotas::reweigh`.
    fn weight(&self, _key: &Self::Key) -> u64 {
        1
    }

    /// Whether `weight` shapes this class's keyed share. Unweighted classes
    /// split equally across active keys regardless of key weight, so
    /// request-serving resources stay independent of retention priority.
    fn weighted(_class: Self::Class) -> bool {
        true
    }

    /// Bound the number of retained key records.
    fn max_keys(&self) -> usize;

    /// Whether releasing this class wakes the shared admission waiter.
    fn wakes(class: Self::Class) -> bool;

    /// Whether ordinary admission of this class remains allowed after stop.
    fn allows_stopped(_class: Self::Class) -> bool {
        false
    }

    /// Whether this class can account for page allocator backing.
    fn covers(class: Self::Class) -> bool;

    /// Observe each failed attempt, including one recovered by reclamation.
    fn rejected(&self, rejection: Rejection<Self::Class>);
}

/// Facts observed at a failed admission attempt, before any reclamation retry.
#[derive(Clone, Copy, Debug)]
pub enum Rejection<C> {
    /// The bounded key table has no room for a new identity.
    Keys {
        /// Records still occupying the bounded key table.
        used: usize,

        /// Maximum retained records allowed by policy.
        limit: usize,
    },

    /// An aggregate or keyed share would be exceeded.
    Resource {
        class: C,

        used: usize,

        limit: usize,

        requested: usize,

        key_used: Option<usize>,

        key_limit: Option<usize>,
    },
}

/// One admission failure with the facts from that exact attempt, not a later
/// counter snapshot. Non-quota failures have no rejection detail.
#[derive(Clone, Copy, Debug)]
pub struct ReserveError<C> {
    /// The same failure returned by the compact admission API.
    pub error: Error,

    /// Exact quota facts, absent for invalid input or stopped admission.
    pub rejection: Option<Rejection<C>>,
}

impl<C> From<Error> for ReserveError<C> {
    /// Preserve failures that are not caused by resource pressure.
    fn from(error: Error) -> Self {
        Self {
            error,
            rejection: None,
        }
    }
}

/// Restores a worker-local synchronous observation sink, even during unwind.
struct RejectionGuard<'a, C: Copy> {
    sink: &'a RefCell<Option<Option<Rejection<C>>>>,

    previous: Option<Option<Rejection<C>>>,
}

impl<C: Copy> Drop for RejectionGuard<'_, C> {
    /// Nested operations cannot overwrite their parent's rejection facts.
    fn drop(&mut self) {
        *self.sink.borrow_mut() = self.previous.take();
    }
}

/// Worker-local keyed admission authority; never moves across workers.
///
/// Only shared handles and charges may cross threads:
///
/// ```compile_fail
/// use flow_control::{Policy, Quotas};
/// fn move_authority<P: Policy + Send>(authority: Quotas<P>) {
///     fn require_send<T: Send>(_: T) {}
///     require_send(authority);
/// }
/// ```
///
/// ```compile_fail
/// use flow_control::{Policy, Quotas};
/// fn share_authority<P: Policy + Sync>(authority: &Quotas<P>) {
///     fn require_sync<T: Sync>(_: &T) {}
///     require_sync(authority);
/// }
/// ```
pub struct Quotas<P: Policy> {
    policy: P,

    totals: Arc<Counters<P::Key>>,

    keys: Keys<P::Key>,

    active_keys: Arc<AtomicUsize>,

    active_weight: Arc<AtomicU64>,

    retired_keys: Arc<Mutex<VecDeque<P::Key>>>,

    stopped: Arc<AtomicBool>,

    local: PhantomData<Rc<()>>,

    rejection: RefCell<Option<Option<Rejection<P::Class>>>>,

    /// Depth of [`Quotas::retrying`] scopes; while nonzero, rejections are
    /// captured but not reported to policy.
    retrying: Cell<usize>,
}

/// Owns a live charge independently of the local quota authority and policy.
pub struct Charge<P: Policy> {
    class: P::Class,

    amount: usize,

    key: Option<P::Key>,

    totals: Arc<Counters<P::Key>>,

    local: Option<Arc<Counters<P::Key>>>,

    stopped: Arc<AtomicBool>,
}

impl<P: Policy> page_alloc::Charge for Charge<P> {
    /// Check page-backing suitability without transferring admission ownership.
    fn covers(&self, bytes: usize) -> bool {
        P::covers(self.class) && self.amount >= bytes
    }

    fn requires_wipe(&self) -> bool {
        self.class.requires_wipe()
    }
}

/// An unkeyed admission and usage handle.
/// It is Send + Sync when the policy is Send + Sync.
pub struct SharedQuotas<P: Policy> {
    policy: P,

    totals: Arc<Counters<P::Key>>,

    stopped: Arc<AtomicBool>,
}

impl<P: Policy + Clone> Clone for SharedQuotas<P> {
    /// Share aggregate counters and stop state.
    fn clone(&self) -> Self {
        Self {
            policy: self.policy.clone(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
        }
    }
}

impl<P: Policy> SharedQuotas<P> {
    /// Borrow the application policy this handle admits under.
    pub fn policy(&self) -> &P {
        &self.policy
    }

    /// Register the single shared admission waiter before checking capacity.
    pub fn register(&self, waker: &std::task::Waker) {
        self.totals.wake.register(waker);
    }

    /// Read current aggregate usage, including retained allocations.
    pub fn used(&self, class: P::Class) -> usize {
        self.totals.counter(class).used()
    }

    /// Return the policy's aggregate ceiling.
    pub fn limit(&self, class: P::Class) -> usize {
        self.policy.limit(class)
    }

    /// Whether the authority has requested admission shutdown.
    pub fn is_stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire)
    }

    /// Admit a nonzero unkeyed amount.
    pub fn reserve(&self, class: P::Class, amount: usize) -> Result<Charge<P>> {
        self.reserve_detailed(class, amount)
            .map_err(|failure| failure.error)
    }

    /// Return exact unkeyed rejection facts without retaining any observation sink.
    pub fn reserve_detailed(
        &self,
        class: P::Class,
        amount: usize,
    ) -> std::result::Result<Charge<P>, ReserveError<P::Class>> {
        if self.is_stopped() && !P::allows_stopped(class) {
            return Err(Error::Unavailable.into());
        }
        if amount == 0 {
            return Err(Error::InvalidInput.into());
        }
        let limit = self.limit(class);
        self.totals
            .counter(class)
            .reserve(amount, limit)
            .map_err(|_| {
                let rejection = Rejection::Resource {
                    class,
                    used: self.used(class),
                    limit,
                    requested: amount,
                    key_used: None,
                    key_limit: None,
                };
                self.policy.rejected(rejection);
                ReserveError {
                    error: Error::Overloaded,
                    rejection: Some(rejection),
                }
            })?;
        Ok(Charge {
            class,
            amount,
            key: None,
            totals: self.totals.clone(),
            local: None,
            stopped: self.stopped.clone(),
        })
    }
}

impl<P: Policy> Charge<P> {
    /// Return the amount still owned by this charge.
    pub fn amount(&self) -> usize {
        self.amount
    }

    /// Return the resource class selected at admission.
    pub fn class(&self) -> P::Class {
        self.class
    }

    /// Return the admitted key, or none for aggregate-only admission.
    pub fn key(&self) -> Option<&P::Key> {
        self.key.as_ref()
    }

    /// Check class and amount; this does not establish authority provenance.
    pub fn validate(&self, class: P::Class, amount: usize) -> Result<()> {
        if self.class.index() != class.index() || amount > self.amount {
            Err(Error::InvalidInput)
        } else {
            Ok(())
        }
    }

    /// Divide an admitted working set without changing its aggregate charge.
    pub fn split(&mut self, amount: usize) -> Result<Self> {
        if amount == 0 || amount >= self.amount {
            return Err(Error::InvalidInput);
        }
        let key = self.key.clone();
        Ok(self.transfer(amount, key))
    }

    /// Absorb `other` into this charge, the inverse of [`Self::split`].
    ///
    /// Both charges must come from the same authority, class, and key;
    /// otherwise `other` is handed back unchanged.
    pub fn merge(&mut self, mut other: Self) -> std::result::Result<(), Self> {
        let same_local = match (&self.local, &other.local) {
            (None, None) => true,
            (Some(a), Some(b)) => Arc::ptr_eq(a, b),
            _ => false,
        };
        if self.class.index() != other.class.index()
            || !Arc::ptr_eq(&self.totals, &other.totals)
            || !same_local
            || self.key != other.key
        {
            return Err(other);
        }
        self.amount += other.amount;
        other.amount = 0;
        Ok(())
    }

    /// Caller must retain at least the capacity of every live backing allocation.
    pub fn shrink(&mut self, amount: usize) -> Result<()> {
        if amount == 0 || amount > self.amount {
            return Err(Error::InvalidInput);
        }
        self.release_to(amount);
        Ok(())
    }

    /// Move admission with a precloned key without touching either usage counter.
    fn transfer(&mut self, amount: usize, key: Option<P::Key>) -> Self {
        self.amount -= amount;
        Self {
            class: self.class,
            amount,
            key,
            totals: self.totals.clone(),
            local: self.local.clone(),
            stopped: self.stopped.clone(),
        }
    }

    /// Return released admission without notifying the shared waiter.
    fn release_to(&mut self, amount: usize) {
        let released = self.amount - amount;
        self.amount = amount;
        self.totals.counter(self.class).release(released);
        if let Some(local) = &self.local {
            local.counter(self.class).release(released);
        }
    }
}

impl<P: Policy> Drop for Charge<P> {
    /// Release exactly this charge's remaining amount and apply wake policy.
    fn drop(&mut self) {
        self.release_to(0);
        // Retire the final key owner before a wake callback retries admission.
        drop(self.local.take());
        if P::wakes(self.class) {
            self.totals.wake.wake();
        }
    }
}

impl<P: Policy> Quotas<P> {
    /// Create a worker-local authority with no admitted work or retained backing.
    pub fn new(policy: P) -> Self {
        Self {
            policy,
            totals: Arc::new(Counters::new(P::Class::COUNT)),
            keys: RefCell::default(),
            active_keys: Arc::new(AtomicUsize::new(0)),
            active_weight: Arc::new(AtomicU64::new(0)),
            retired_keys: Arc::new(Mutex::new(VecDeque::new())),
            stopped: Arc::new(AtomicBool::new(false)),
            local: PhantomData,
            rejection: RefCell::new(None),
            retrying: Cell::new(0),
        }
    }

    /// Borrow application policy without transferring local authority.
    pub fn policy(&self) -> &P {
        &self.policy
    }

    /// Observe this synchronous compound operation only. The local authority
    /// cannot cross threads; no policy mutex, thread identity, or async guard is
    /// needed. Nested operations restore their parent's sink, including on panic.
    /// Callers decide whether a recovered rejection describes their final result.
    pub fn observe_rejections<T>(
        &self,
        operation: impl FnOnce() -> T,
    ) -> (T, Option<Rejection<P::Class>>) {
        let previous = self.rejection.replace(Some(None));
        let guard = RejectionGuard {
            sink: &self.rejection,
            previous,
        };
        let result = operation();
        let rejection = self.rejection.borrow().as_ref().copied().flatten();
        drop(guard);
        (result, rejection)
    }

    /// Run attempts the caller may retry: their rejections are captured as in
    /// [`Self::observe_rejections`] but not reported to policy. A caller that
    /// gives up reports the returned rejection once with [`Self::report`].
    /// Nesting is allowed; reports from inner scopes stay quiet too.
    pub fn retrying<T>(&self, operation: impl FnOnce() -> T) -> (T, Option<Rejection<P::Class>>) {
        struct Depth<'a>(&'a Cell<usize>);
        impl Drop for Depth<'_> {
            fn drop(&mut self) {
                self.0.set(self.0.get() - 1);
            }
        }
        self.retrying.set(self.retrying.get() + 1);
        let depth = Depth(&self.retrying);
        let result = self.observe_rejections(operation);
        drop(depth);
        result
    }

    /// Report a terminal rejection captured by [`Self::retrying`], subject to
    /// any enclosing retrying scope and rejection sink.
    pub fn report(&self, rejection: Rejection<P::Class>) {
        let _ = self.report_rejection(rejection);
    }

    /// Create a transferable unkeyed handle.
    pub fn shared(&self) -> SharedQuotas<P>
    where
        P: Clone,
    {
        SharedQuotas {
            policy: self.policy.clone(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
        }
    }

    /// Read aggregate usage, including idle resources and completion owners.
    pub fn used(&self, class: P::Class) -> usize {
        self.totals.counter(class).used()
    }

    /// Return the aggregate ceiling chosen by policy.
    pub fn limit(&self, class: P::Class) -> usize {
        self.policy.limit(class)
    }

    /// Check that a charge originated from this authority's counters.
    pub fn owns(&self, charge: &Charge<P>) -> bool {
        Arc::ptr_eq(&self.totals, &charge.totals)
    }

    /// Whether shutdown has been requested for this authority.
    pub fn is_stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire)
    }

    /// Stop ordinary admission, reclaim idle backing, and wake the shared waiter.
    pub fn stop(&self) {
        self.stopped.store(true, Ordering::Release);
        self.totals.wake.wake();
    }

    /// A keyed fair-share deficit must be reclaimed from that key. Global
    /// pressure returns no key; callers should first reclaim from keys with
    /// [`Self::excess`], then from any idle key. Impossible requests have no
    /// byte remedy.
    pub fn reclamation(
        &self,
        key: &P::Key,
        class: P::Class,
        amount: usize,
    ) -> Option<(Option<P::Key>, usize)> {
        // Keep the counters alive, but release the lookup borrow before callbacks.
        let local = self.keys.borrow().get(key).and_then(Weak::upgrade);
        let fair = match &local {
            Some(local) => self.fair_limit_for(class, local),
            None => self.weighted_limit(
                class,
                self.active_keys.load(Ordering::Acquire) + 1,
                self.key_weight(key),
                self.active_weight.load(Ordering::Acquire),
                true,
            ),
        };
        if amount > fair {
            return None;
        }
        let local_deficit = local
            .as_ref()
            .map_or(0, |local| local.counter(class).used())
            .saturating_sub(fair - amount);
        if local_deficit != 0 {
            return Some((Some(key.clone()), local_deficit));
        }
        let headroom = self.limit(class).checked_sub(amount)?;
        let deficit = self.used(class).saturating_sub(headroom);
        (deficit != 0).then_some((None, deficit))
    }

    /// Bytes `key` holds above its current weighted fair share, or zero when
    /// the key is inactive or within its share. One map lookup, so callers may
    /// ask about a few candidate keys on each reclaim without walking all keys.
    pub fn excess(&self, key: &P::Key, class: P::Class) -> usize {
        // Release the lookup borrow before policy callbacks, as in `reclamation`.
        let Some(local) = self.keys.borrow().get(key).and_then(Weak::upgrade) else {
            return 0;
        };
        local
            .counter(class)
            .used()
            .saturating_sub(self.fair_limit_for(class, &local))
    }

    /// Admit nonzero keyed or aggregate-only work under ordinary policy.
    pub fn reserve(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> Result<Charge<P>> {
        self.reserve_detailed(key, class, amount)
            .map_err(|failure| failure.error)
    }

    /// Admit ordinary work and return exact final-attempt quota rejection facts.
    /// Every failed attempt is still reported to policy, even if retry recovers.
    pub fn reserve_detailed(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> std::result::Result<Charge<P>, ReserveError<P::Class>> {
        self.reserve_inner(key, class, amount, AdmissionMode::Ordinary)
    }

    /// For already-admitted work during drain only. Aggregate limits still apply.
    pub fn reserve_completion(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> Result<Charge<P>> {
        self.reserve_completion_detailed(key, class, amount)
            .map_err(|failure| failure.error)
    }

    /// Return exact completion-admission rejection facts while preserving drain
    /// bypass policy and aggregate limits.
    pub fn reserve_completion_detailed(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> std::result::Result<Charge<P>, ReserveError<P::Class>> {
        self.reserve_inner(key, class, amount, AdmissionMode::Completion)
    }

    /// Sum of the weights recorded for currently active keys.
    pub fn active_weight(&self) -> u64 {
        self.active_weight.load(Ordering::Acquire)
    }

    /// Return the current keyed fair limit for `key` in `class`, counting the
    /// key as active (with its current policy weight) if it is not yet.
    pub fn fair_limit(&self, key: &P::Key, class: P::Class) -> usize {
        let local = self.keys.borrow().get(key).and_then(Weak::upgrade);
        match &local {
            Some(local) => self.fair_limit_for(class, local),
            None => self.weighted_limit(
                class,
                self.active_keys.load(Ordering::Acquire) + 1,
                self.key_weight(key),
                self.active_weight.load(Ordering::Acquire),
                true,
            ),
        }
    }

    /// Re-read `Policy::weight` for every active key and update the recorded
    /// weights and their total. Wakes the shared waiter so blocked admission
    /// can retry against changed shares. Safe to call at any time.
    pub fn reweigh(&self) {
        // Upgrade owners only: key clones and policy callbacks may reenter
        // the key map, so they must run after the borrow is released.
        let live: Vec<_> = self
            .keys
            .borrow()
            .values()
            .filter_map(Weak::upgrade)
            .collect();
        for counts in &live {
            let Some(retirement) = counts.retirement.as_ref() else {
                continue;
            };
            let next = self.key_weight(&retirement.key);
            // The upgraded owner prevents concurrent retirement of this key.
            let previous = retirement.weight.swap(next, Ordering::AcqRel);
            self.active_weight.fetch_add(next, Ordering::AcqRel);
            self.active_weight.fetch_sub(previous, Ordering::AcqRel);
        }
        drop(live);
        self.totals.wake.wake();
    }

    /// Clamp policy weights so the total over all keys cannot overflow.
    fn key_weight(&self, key: &P::Key) -> u64 {
        self.policy.weight(key).min(u64::from(u32::MAX))
    }

    /// Fair limit for an active key record using its recorded weight.
    fn fair_limit_for(&self, class: P::Class, local: &Counters<P::Key>) -> usize {
        let weight = local
            .retirement
            .as_ref()
            .map_or(1, |retirement| retirement.weight.load(Ordering::Acquire));
        self.weighted_limit(
            class,
            self.active_keys.load(Ordering::Acquire).max(1),
            weight,
            self.active_weight.load(Ordering::Acquire),
            false,
        )
    }

    /// Divide the aggregate ceiling by weight, honoring the clipped per-key
    /// floor. `include` adds `weight` to `total` for a not-yet-active key.
    fn weighted_limit(
        &self,
        class: P::Class,
        active: usize,
        weight: u64,
        total: u64,
        include: bool,
    ) -> usize {
        let limit = self.limit(class);
        let fair = if P::weighted(class) {
            // Low weight cannot cut a key below its shared progress allowance,
            // capped at the unweighted equal split.
            let equal = limit / active.max(1);
            share(limit, active, weight, total, include).max(self.policy.progress(class).min(equal))
        } else {
            limit / active.max(1)
        };
        fair.max(self.policy.floor(class)).min(limit)
    }

    /// Perform one admission attempt and report any rejection to policy.
    fn reserve_inner(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        mode: AdmissionMode,
    ) -> std::result::Result<Charge<P>, ReserveError<P::Class>> {
        if self.is_stopped() && mode == AdmissionMode::Ordinary && !P::allows_stopped(class) {
            return Err(Error::Unavailable.into());
        }
        if amount == 0 {
            return Err(Error::InvalidInput.into());
        }
        let local = key.map(|key| self.key_counters(key)).transpose()?;
        // Clone before checking limits: application code may admit more work,
        // change policy, stop admission, or panic without owning a charge yet.
        let key = key.cloned();
        let limit = self.limit(class);
        if let Some(local) = local.as_ref().filter(|_| mode == AdmissionMode::Ordinary) {
            let fair = self.fair_limit_for(class, local);
            let used = local.counter(class).used();
            if used.checked_add(amount).is_none_or(|next| next > fair) {
                return Err(self.rejected(class, amount, Some(used), Some(fair)));
            }
        }
        self.totals
            .counter(class)
            .reserve(amount, limit)
            .map_err(|_| self.rejected(class, amount, None, None))?;
        if let Some(local) = &local {
            local.counter(class).add(amount);
        }
        let charge = Charge {
            class,
            amount,
            key,
            totals: self.totals.clone(),
            local,
            stopped: self.stopped.clone(),
        };
        // Policy and key callbacks may stop admission. Drop the completed owner
        // to roll back both counters while preserving completion and drain work.
        if self.is_stopped() && mode == AdmissionMode::Ordinary && !P::allows_stopped(class) {
            return Err(Error::Unavailable.into());
        }
        Ok(charge)
    }

    /// Reuse a live key record or create one after bounded retirement cleanup.
    fn key_counters(
        &self,
        key: &P::Key,
    ) -> std::result::Result<Arc<Counters<P::Key>>, ReserveError<P::Class>> {
        let mut keys = self.keys.borrow_mut();
        let mut retired = self.retired_keys.lock().unwrap_or_else(|e| e.into_inner());
        for _ in 0..256 {
            let Some(id) = retired.pop_front() else {
                break;
            };
            if keys
                .get(&id)
                .is_some_and(|counts| counts.strong_count() == 0)
            {
                keys.remove(&id);
            }
        }
        drop(retired);
        if !keys.contains_key(key) && keys.len() >= self.policy.max_keys() {
            return Err(self.report_rejection(Rejection::Keys {
                used: keys.len(),
                limit: self.policy.max_keys(),
            }));
        }
        if let Some(counts) = keys.get(key).and_then(Weak::upgrade) {
            return Ok(counts);
        }
        drop(keys);
        let retirement_key = key.clone();
        let map_key = key.clone();
        let limit = self.policy.max_keys();
        // Sample the weight while unborrowed: the policy may reenter Quotas.
        let weight = self.key_weight(key);
        let mut keys = self.keys.borrow_mut();
        // Either clone or the weight callback may have installed this key or filled the table.
        if let Some(counts) = keys.get(key).and_then(Weak::upgrade) {
            return Ok(counts);
        }
        if !keys.contains_key(key) && keys.len() >= limit {
            let used = keys.len();
            drop(keys);
            return Err(self.report_rejection(Rejection::Keys { used, limit }));
        }
        let counts = Arc::new(Counters::keyed(
            P::Class::COUNT,
            retirement_key,
            weight,
            self.active_weight.clone(),
            self.active_keys.clone(),
            self.retired_keys.clone(),
        ));
        keys.insert(map_key, Arc::downgrade(&counts));
        Ok(counts)
    }

    /// Report the current aggregate and optional keyed rejection facts.
    fn rejected(
        &self,
        class: P::Class,
        requested: usize,
        key_used: Option<usize>,
        key_limit: Option<usize>,
    ) -> ReserveError<P::Class> {
        self.report_rejection(Rejection::Resource {
            class,
            used: self.used(class),
            limit: self.limit(class),
            requested,
            key_used,
            key_limit,
        })
    }

    /// Report one failed attempt and retain its facts in the active local sink.
    fn report_rejection(&self, rejection: Rejection<P::Class>) -> ReserveError<P::Class> {
        if let Some(sink) = self.rejection.borrow_mut().as_mut() {
            *sink = Some(rejection);
        }
        if self.retrying.get() == 0 {
            self.policy.rejected(rejection);
        }
        ReserveError {
            error: Error::Overloaded,
            rejection: Some(rejection),
        }
    }
}

/// Pending and issued reservations consume the same byte and slot budgets.
pub struct Window<K: Ord> {
    slots: usize,

    bytes: u64,

    max_item: u64,

    used: u64,

    outstanding: std::collections::BTreeMap<K, Credit>,
}

impl<K: Ord> Window<K> {
    /// Require positive limits; the item ceiling may exceed the byte budget.
    pub fn new(slots: usize, bytes: u64, max_item: u64) -> Result<Self> {
        if slots == 0 || bytes == 0 || max_item == 0 {
            return Err(Error::InvalidInput);
        }
        Ok(Self {
            slots,
            bytes,
            max_item,
            used: 0,
            outstanding: std::collections::BTreeMap::new(),
        })
    }

    /// Whether a valid length fits both limits, independent of item identity.
    pub fn can_reserve(&self, length: u64) -> bool {
        length != 0
            && length <= self.max_item
            && self.outstanding.len() < self.slots
            && length <= self.bytes - self.used
    }

    /// Reserve a unique item without returning capacity on later issuance.
    pub fn reserve(&mut self, key: K, length: u64) -> Result<()> {
        if length == 0 || length > self.max_item {
            return Err(Error::InvalidInput);
        }
        if !self.can_reserve(length) || self.outstanding.contains_key(&key) {
            return Err(Error::Overloaded);
        }
        self.outstanding.insert(key, Credit::Pending(length));
        self.used += length;
        Ok(())
    }

    /// Issue a pending item exactly once while retaining its full reservation.
    pub fn issued(&mut self, key: K) -> Result<()> {
        let entry = self.outstanding.get_mut(&key).ok_or(Error::InvalidInput)?;
        let Credit::Pending(length) = *entry else {
            return Err(Error::InvalidInput);
        };
        *entry = Credit::Issued(length);
        Ok(())
    }

    /// Release an issued item exactly once using its admitted length.
    pub fn release(&mut self, key: K, length: u64) -> Result<()> {
        if self.outstanding.get(&key) != Some(&Credit::Issued(length)) {
            return Err(Error::InvalidInput);
        }
        self.outstanding.remove(&key);
        self.used -= length;
        Ok(())
    }

    /// Whether neither pending nor issued items retain credit.
    pub fn is_empty(&self) -> bool {
        self.outstanding.is_empty()
    }
}

/// A credit's lifecycle; only an exact issued credit can be released.
#[derive(Eq, PartialEq)]
enum Credit {
    /// Reserved capacity whose item has not yet been issued.
    Pending(u64),

    /// Issued capacity awaiting an exact-length acknowledgment.
    Issued(u64),
}

/// Admission intent determines whether existing work may finish during drain.
#[derive(Clone, Copy, Eq, PartialEq)]
enum AdmissionMode {
    /// New work obeys stop state and keyed fair shares.
    Ordinary,

    /// Existing work bypasses stop and fairness, but not hard limits.
    Completion,
}

/// Worker-local key lookup; charges retain records independently.
type Keys<K> = RefCell<HashMap<K, Weak<Counters<K>>>>;

/// Isolate independently updated resource counters on separate cache lines.
#[repr(align(64))]
struct Counter(AtomicUsize);

impl Counter {
    /// Start one resource class with no admitted usage.
    fn new() -> Self {
        Self(AtomicUsize::new(0))
    }

    /// Read usage published by admission and cross-thread release.
    fn used(&self) -> usize {
        self.0.load(Ordering::Acquire)
    }

    /// Add usage only when both arithmetic and the aggregate ceiling allow it.
    fn reserve(&self, amount: usize, limit: usize) -> Result<()> {
        self.0
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(amount).filter(|next| *next <= limit)
            })
            .map(|_| ())
            .map_err(|_| Error::Overloaded)
    }

    /// Record keyed usage already covered by aggregate admission.
    fn add(&self, amount: usize) {
        self.0.fetch_add(amount, Ordering::AcqRel);
    }

    /// Return usage owned exclusively by the releasing charge.
    fn release(&self, amount: usize) {
        self.0.fetch_sub(amount, Ordering::AcqRel);
    }
}

/// Shared counters outliving the worker-local authority while charges remain.
struct Counters<K> {
    retirement: Option<Retirement<K>>,

    used: Box<[Counter]>,

    wake: futures::task::AtomicWaker,
}

impl<K> Counters<K> {
    /// Allocate zeroed, independently padded counters for all classes.
    fn new(classes: usize) -> Self {
        Self {
            retirement: None,
            used: (0..classes).map(|_| Counter::new()).collect(),
            wake: futures::task::AtomicWaker::new(),
        }
    }

    /// Couple one live key record to its eventual retirement notification.
    fn keyed(
        classes: usize,
        key: K,
        weight: u64,
        total_weight: Arc<AtomicU64>,
        active: Arc<AtomicUsize>,
        queue: Arc<Mutex<VecDeque<K>>>,
    ) -> Self {
        let mut counters = Self::new(classes);
        active.fetch_add(1, Ordering::AcqRel);
        total_weight.fetch_add(weight, Ordering::AcqRel);
        counters.retirement = Some(Retirement {
            key,
            weight: AtomicU64::new(weight),
            total_weight,
            active,
            queue,
        });
        counters
    }

    /// Select the padded counter using the policy's stable class index.
    fn counter<C: Class>(&self, class: C) -> &Counter {
        &self.used[class.index()]
    }
}

impl<K> Drop for Counters<K> {
    /// Retire the complete keyed state when its final charge leaves.
    fn drop(&mut self) {
        if let Some(retirement) = self.retirement.take() {
            retirement.retire();
        }
    }
}

/// One live key record owns both its active count and its cleanup notification.
struct Retirement<K> {
    key: K,

    weight: AtomicU64,

    total_weight: Arc<AtomicU64>,

    active: Arc<AtomicUsize>,

    queue: Arc<Mutex<VecDeque<K>>>,
}

impl<K> Retirement<K> {
    /// Publish retirement only after the last owner releases the key record.
    fn retire(self) {
        self.total_weight
            .fetch_sub(self.weight.load(Ordering::Acquire), Ordering::AcqRel);
        self.active.fetch_sub(1, Ordering::AcqRel);
        self.queue
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push_back(self.key);
    }
}

/// Weighted share of `limit` before floor clipping. Falls back to an equal
/// split across `active` keys when the total weight is zero.
fn share(limit: usize, active: usize, weight: u64, total: u64, include: bool) -> usize {
    let total = if include {
        total.saturating_add(weight)
    } else {
        total
    };
    if total == 0 {
        return limit / active.max(1);
    }
    let scaled = limit as u128 * u128::from(weight.min(total)) / u128::from(total);
    usize::try_from(scaled).unwrap_or(limit)
}

/// Accounting, retirement, reclamation, and cross-thread release contracts.
#[cfg(test)]
mod quota_tests {
    use super::*;

    /// Exact failures survive counter changes and never cross observation scopes.
    #[test]
    fn detailed_rejection_and_nested_unwind_observation() {
        let quotas = Quotas::new(TestPolicy::new(10, 1));
        let key = "a".to_owned();
        let held = quotas.reserve(Some(&key), Resource::Payload, 10).unwrap();
        let Err(failure) = quotas.reserve_detailed(Some(&key), Resource::Payload, 1) else {
            panic!("full quota")
        };
        assert_eq!(failure.error, Error::Overloaded);
        assert!(matches!(
            failure.rejection,
            Some(Rejection::Resource {
                used: 10,
                requested: 1,
                key_used: Some(10),
                key_limit: Some(10),
                ..
            })
        ));
        drop(held);
        assert_eq!(quotas.used(Resource::Payload), 0);
        assert!(matches!(
            failure.rejection,
            Some(Rejection::Resource { used: 10, .. })
        ));
        let (_, rejection) = quotas.observe_rejections(|| {
            let (_, inner) =
                quotas.observe_rejections(|| quotas.reserve(None, Resource::Payload, 11));
            assert!(inner.is_some());
        });
        assert!(rejection.is_none());
        let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            quotas.observe_rejections(|| {
                let _ = quotas.reserve(None, Resource::Payload, 11);
                panic!("fixture");
            });
        }));
        assert!(panic.is_err());
        assert!(quotas.rejection.borrow().is_none());
        let (_, rejection) = quotas.observe_rejections(|| {
            let shared = quotas.shared();
            std::thread::spawn(move || {
                assert!(shared.reserve_detailed(Resource::Payload, 11).is_err());
            })
            .join()
            .unwrap();
        });
        assert!(
            rejection.is_none(),
            "shared policy cannot pollute local facts"
        );
        let Err(invalid) = quotas.reserve_detailed(None, Resource::Payload, 0) else {
            panic!("zero request")
        };
        assert_eq!(invalid.error, Error::InvalidInput);
        assert!(invalid.rejection.is_none());
        quotas.stop();
        let Err(stopped) = quotas.reserve_detailed(None, Resource::Payload, 1) else {
            panic!("stopped")
        };
        assert_eq!(stopped.error, Error::Unavailable);
        assert!(stopped.rejection.is_none());
        assert!(
            quotas
                .reserve_completion_detailed(None, Resource::Payload, 1)
                .is_ok()
        );
    }

    /// Unavailability does not imply that flow control has stopped.
    #[test]
    fn unavailable_display_is_state_neutral() {
        assert_eq!(Error::Unavailable.to_string(), "flow control unavailable");
    }

    /// Retried attempts stay quiet; only an explicit terminal report reaches policy.
    #[test]
    fn retrying_captures_without_reporting_until_terminal_report() {
        let policy = TestPolicy::new(10, 4);
        let quotas = Quotas::new(policy.clone());
        let rejected = || policy.rejected.lock().unwrap().len();
        let (result, rejection) = quotas.retrying(|| {
            let first = quotas.reserve(None, Resource::Payload, 11);
            // A nested scope stays quiet and restores the outer depth.
            let ((), inner) = quotas.retrying(|| {
                assert!(quotas.reserve(None, Resource::Payload, 12).is_err());
            });
            assert!(matches!(
                inner,
                Some(Rejection::Resource { requested: 12, .. })
            ));
            // A report inside a retrying scope stays quiet as well.
            quotas.report(inner.unwrap());
            first.and_then(|_| quotas.reserve(None, Resource::Payload, 13))
        });
        assert!(result.is_err());
        assert!(matches!(
            rejection,
            Some(Rejection::Resource { requested: 12, .. })
        ));
        assert_eq!(rejected(), 0, "retried attempts are not reported");
        quotas.report(rejection.unwrap());
        assert_eq!(rejected(), 1);
        // A recovered retry reports nothing at all.
        let (result, _) = quotas.retrying(|| {
            let _ = quotas.reserve(None, Resource::Payload, 11);
            quotas.reserve(None, Resource::Payload, 1)
        });
        assert!(result.is_ok());
        assert_eq!(rejected(), 1);
        // Outside any scope every rejection is reported again.
        assert!(quotas.reserve(None, Resource::Payload, 11).is_err());
        assert_eq!(rejected(), 2);
        // A panicking attempt does not leave the authority quiet.
        let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            quotas.retrying(|| panic!("fixture"));
        }));
        assert!(panic.is_err());
        assert!(quotas.reserve(None, Resource::Payload, 11).is_err());
        assert_eq!(rejected(), 3);
    }

    /// Distinct payload, wake-enabled, and drain-progress fixture classes.
    #[derive(Clone, Copy, Debug)]
    enum Resource {
        Payload,

        Other,

        Progress,
    }

    impl Class for Resource {
        /// Number of fixture resource classes.
        const COUNT: usize = 3;

        /// Map each fixture class to its stable counter.
        fn index(self) -> usize {
            self as usize
        }

        fn requires_wipe(self) -> bool {
            !matches!(self, Self::Progress)
        }
    }

    /// Optional limit samples and a shared rejection log for assertions.
    #[derive(Clone)]
    struct TestPolicy {
        limit: usize,

        max_keys: usize,

        rejected: Arc<Mutex<Vec<Rejection<Resource>>>>,

        limit_samples: Arc<Mutex<VecDeque<usize>>>,

        weights: Arc<Mutex<HashMap<String, u64>>>,
    }

    impl TestPolicy {
        /// Construct fixed aggregate limits with an empty rejection log.
        fn new(limit: usize, max_keys: usize) -> Self {
            Self {
                limit,
                max_keys,
                rejected: Arc::default(),
                limit_samples: Arc::default(),
                weights: Arc::default(),
            }
        }
    }

    impl Policy for TestPolicy {
        /// Fixture resource classes with separate usage counters.
        type Class = Resource;

        /// Owned identities retained until all their charges leave.
        type Key = String;

        /// All fixture classes use the same aggregate ceiling.
        fn limit(&self, _: Resource) -> usize {
            self.limit_samples
                .lock()
                .unwrap()
                .pop_front()
                .unwrap_or(self.limit)
        }

        /// Configured key weights, defaulting to 1.
        fn weight(&self, key: &String) -> u64 {
            self.weights.lock().unwrap().get(key).copied().unwrap_or(1)
        }

        /// Progress stays an equal split regardless of key weight.
        fn weighted(class: Resource) -> bool {
            !matches!(class, Resource::Progress)
        }

        /// Return the fixture's key-record bound.
        fn max_keys(&self) -> usize {
            self.max_keys
        }

        /// Only the non-payload fixture class wakes shared waiters.
        fn wakes(class: Resource) -> bool {
            matches!(class, Resource::Other)
        }

        /// Only payload admission may cover page backing.
        fn covers(class: Resource) -> bool {
            matches!(class, Resource::Payload)
        }

        /// Progress reservations remain available during drain.
        fn allows_stopped(class: Resource) -> bool {
            matches!(class, Resource::Progress)
        }

        /// Preserve rejection order and facts for assertions.
        fn rejected(&self, rejection: Rejection<Resource>) {
            self.rejected.lock().unwrap().push(rejection);
        }
    }

    /// Build an authority whose keys carry the given weights.
    fn weighted(limit: usize, weights: &[(&str, u64)]) -> (Quotas<TestPolicy>, TestPolicy) {
        let policy = TestPolicy::new(limit, 16);
        policy.weights.lock().unwrap().extend(
            weights
                .iter()
                .map(|(key, weight)| ((*key).to_owned(), *weight)),
        );
        (Quotas::new(policy.clone()), policy)
    }

    /// Shares are proportional to weight and admission enforces them.
    #[test]
    fn weighted_limits_are_proportional() {
        let (quotas, _) = weighted(1000, &[("a", 900), ("b", 100)]);
        let a = quotas
            .reserve(Some(&"a".into()), Resource::Other, 1)
            .unwrap();
        let b = quotas
            .reserve(Some(&"b".into()), Resource::Other, 1)
            .unwrap();
        assert_eq!(quotas.active_weight(), 1000);
        assert_eq!(quotas.fair_limit(&"a".into(), Resource::Other), 900);
        assert_eq!(quotas.fair_limit(&"b".into(), Resource::Other), 100);
        let more = quotas
            .reserve(Some(&"b".into()), Resource::Other, 99)
            .unwrap();
        assert_eq!(
            quotas.reserve(Some(&"b".into()), Resource::Other, 1).err(),
            Some(Error::Overloaded)
        );
        // A new key's prospective limit includes its own weight.
        assert_eq!(quotas.fair_limit(&"c".into(), Resource::Other), 1);
        drop((a, b, more));
    }

    /// Unweighted classes split equally even when a key has weight 0.
    #[test]
    fn unweighted_classes_ignore_key_weight() {
        let (quotas, _) = weighted(1000, &[("a", 1), ("z", 0)]);
        let _a = quotas
            .reserve(Some(&"a".into()), Resource::Progress, 500)
            .unwrap();
        assert_eq!(quotas.fair_limit(&"z".into(), Resource::Progress), 500);
        let _z = quotas
            .reserve(Some(&"z".into()), Resource::Progress, 500)
            .unwrap();
        assert_eq!(quotas.fair_limit(&"a".into(), Resource::Progress), 500);
        // The weighted class still follows key weight.
        assert_eq!(quotas.fair_limit(&"z".into(), Resource::Other), 1);
        assert_eq!(quotas.fair_limit(&"a".into(), Resource::Other), 1000);
    }

    /// Weight 0 keeps only the floor; all-zero weights share equally.
    #[test]
    fn zero_weights_get_floor_or_equal_shares() {
        let (quotas, _) = weighted(1000, &[("a", 1), ("z", 0)]);
        let _a = quotas
            .reserve(Some(&"a".into()), Resource::Other, 1)
            .unwrap();
        let _z = quotas
            .reserve(Some(&"z".into()), Resource::Other, 1)
            .unwrap();
        assert_eq!(quotas.fair_limit(&"z".into(), Resource::Other), 1);
        assert_eq!(quotas.fair_limit(&"a".into(), Resource::Other), 1000);

        let (quotas, _) = weighted(1000, &[("x", 0), ("y", 0)]);
        assert_eq!(quotas.fair_limit(&"x".into(), Resource::Other), 1000);
        let _x = quotas
            .reserve(Some(&"x".into()), Resource::Other, 1)
            .unwrap();
        let _y = quotas
            .reserve(Some(&"y".into()), Resource::Other, 1)
            .unwrap();
        assert_eq!(quotas.active_weight(), 0);
        assert_eq!(quotas.fair_limit(&"x".into(), Resource::Other), 500);
    }

    /// Reclamation targets a key that exceeds its weighted share.
    #[test]
    fn weighted_reclamation_targets_over_share_key() {
        let (quotas, _) = weighted(1000, &[("a", 900), ("b", 100)]);
        let _b = quotas
            .reserve(Some(&"b".into()), Resource::Other, 500)
            .unwrap();
        let _a = quotas
            .reserve(Some(&"a".into()), Resource::Other, 100)
            .unwrap();
        assert_eq!(
            quotas.reclamation(&"b".into(), Resource::Other, 10),
            Some((Some("b".into()), 410))
        );
        assert_eq!(quotas.reclamation(&"a".into(), Resource::Other, 400), None);
        assert_eq!(
            quotas.reclamation(&"a".into(), Resource::Other, 500),
            Some((None, 100))
        );
    }

    /// Reweigh applies runtime changes; retirement subtracts the recorded weight.
    #[test]
    fn reweigh_updates_limits_and_retirement_uses_recorded_weight() {
        let (quotas, policy) = weighted(1000, &[("a", 1), ("b", 1)]);
        let a = quotas
            .reserve(Some(&"a".into()), Resource::Other, 1)
            .unwrap();
        let b = quotas
            .reserve(Some(&"b".into()), Resource::Other, 1)
            .unwrap();
        assert_eq!(quotas.fair_limit(&"a".into(), Resource::Other), 500);
        policy.weights.lock().unwrap().insert("a".into(), 3);
        // Not applied until reweigh.
        assert_eq!(quotas.active_weight(), 2);
        quotas.reweigh();
        assert_eq!(quotas.active_weight(), 4);
        assert_eq!(quotas.fair_limit(&"a".into(), Resource::Other), 750);
        assert_eq!(quotas.fair_limit(&"b".into(), Resource::Other), 250);
        // Changed but not reweighed: retirement subtracts the recorded 1.
        policy.weights.lock().unwrap().insert("b".into(), 7);
        drop(b);
        assert_eq!(quotas.active_weight(), 3);
        drop(a);
        assert_eq!(quotas.active_weight(), 0);
        quotas.reweigh();
        assert_eq!(quotas.active_weight(), 0);
    }

    /// Excess follows live reweigh and is zero for inactive or retired keys.
    #[test]
    fn excess_follows_reweigh_and_retirement() {
        let (quotas, policy) = weighted(100, &[("low", 1), ("high", 1)]);
        assert_eq!(quotas.excess(&"low".into(), Resource::Payload), 0);
        let low = quotas
            .reserve(Some(&"low".into()), Resource::Payload, 50)
            .unwrap();
        let high = quotas
            .reserve(Some(&"high".into()), Resource::Payload, 20)
            .unwrap();
        // Equal weights: both keys are within a share of 50.
        assert_eq!(quotas.excess(&"low".into(), Resource::Payload), 0);
        assert_eq!(quotas.excess(&"high".into(), Resource::Payload), 0);
        policy.weights.lock().unwrap().insert("low".into(), 100);
        policy.weights.lock().unwrap().insert("high".into(), 900);
        // Not applied until reweigh.
        assert_eq!(quotas.excess(&"low".into(), Resource::Payload), 0);
        quotas.reweigh();
        assert_eq!(quotas.excess(&"low".into(), Resource::Payload), 40);
        assert_eq!(quotas.excess(&"high".into(), Resource::Payload), 0);
        // Other classes keep their own counters.
        assert_eq!(quotas.excess(&"low".into(), Resource::Other), 0);
        drop(low);
        assert_eq!(quotas.excess(&"low".into(), Resource::Payload), 0);
        drop(high);
        assert_eq!(quotas.excess(&"high".into(), Resource::Payload), 0);
    }

    /// `Policy::weight` may reenter the authority while a new key activates.
    #[test]
    fn weight_callback_may_reenter_quotas() {
        use std::{
            cell::{Cell, RefCell},
            rc::Weak as RcWeak,
        };

        /// Reenters fair-share queries and nested keyed admission from `weight`.
        struct WeightPolicy {
            quotas: RefCell<RcWeak<Quotas<WeightPolicy>>>,

            nested: RefCell<Vec<Charge<WeightPolicy>>>,

            reentered: Cell<bool>,
        }

        impl Policy for WeightPolicy {
            type Class = Resource;

            type Key = String;

            /// Fixed aggregate ceiling.
            fn limit(&self, _: Resource) -> usize {
                100
            }

            /// Query and install other keys while the outer key activates.
            fn weight(&self, key: &String) -> u64 {
                if key == "outer" && !self.reentered.replace(true) {
                    let quotas = self.quotas.borrow().upgrade().unwrap();
                    let _ = quotas.fair_limit(&"outer".into(), Resource::Other);
                    let nested = quotas
                        .reserve(Some(&"nested".into()), Resource::Other, 1)
                        .unwrap();
                    self.nested.borrow_mut().push(nested);
                }
                if key == "outer" { 3 } else { 1 }
            }

            /// Room for both keys only.
            fn max_keys(&self) -> usize {
                2
            }

            /// No waiter is needed.
            fn wakes(_: Resource) -> bool {
                false
            }

            /// No page backing.
            fn covers(_: Resource) -> bool {
                false
            }

            /// Every admission must succeed.
            fn rejected(&self, _: Rejection<Resource>) {
                panic!("unexpected rejection");
            }
        }

        let quotas = Rc::new(Quotas::new(WeightPolicy {
            quotas: RefCell::default(),
            nested: RefCell::default(),
            reentered: Cell::default(),
        }));
        *quotas.policy.quotas.borrow_mut() = Rc::downgrade(&quotas);
        let outer = quotas
            .reserve(Some(&"outer".into()), Resource::Other, 1)
            .unwrap();
        assert_eq!(quotas.policy.nested.borrow().len(), 1);
        assert_eq!(quotas.active_weight(), 4);
        assert_eq!(quotas.used(Resource::Other), 2);
        assert_eq!(quotas.fair_limit(&"outer".into(), Resource::Other), 75);
        assert_eq!(quotas.fair_limit(&"nested".into(), Resource::Other), 25);
        drop(outer);
        quotas.policy.nested.borrow_mut().clear();
        assert_eq!(quotas.active_weight(), 0);
        assert_eq!(quotas.used(Resource::Other), 0);
    }

    /// Large limits and clamped weights do not overflow.
    #[test]
    fn weighted_share_is_overflow_safe() {
        assert_eq!(share(usize::MAX, 2, u64::MAX, u64::MAX, false), usize::MAX);
        assert_eq!(share(usize::MAX, 2, 1, 2, false), usize::MAX / 2);
        assert_eq!(share(10, 2, 0, 0, false), 5);
        assert_eq!(share(10, 2, 5, u64::MAX, true), 0);
        let (quotas, _) = weighted(usize::MAX, &[("a", u64::MAX), ("b", u64::MAX)]);
        let _a = quotas
            .reserve(Some(&"a".into()), Resource::Other, 1)
            .unwrap();
        let _b = quotas
            .reserve(Some(&"b".into()), Resource::Other, 1)
            .unwrap();
        assert_eq!(quotas.active_weight(), 2 * u64::from(u32::MAX));
        assert_eq!(
            quotas.fair_limit(&"a".into(), Resource::Other),
            usize::MAX / 2
        );
    }

    /// Fairness, retirement, and drain completion retain their separate limits.
    #[test]
    fn fairness_retirement_reclamation_and_completion() {
        let quotas = Quotas::new(TestPolicy::new(100, 2));
        let (a, b, c) = ("a".to_owned(), "b".to_owned(), "c".to_owned());
        let first = quotas.reserve(Some(&a), Resource::Payload, 40).unwrap();
        let second = quotas.reserve(Some(&b), Resource::Payload, 40).unwrap();
        assert!(matches!(
            quotas.reserve(Some(&a), Resource::Payload, 11),
            Err(Error::Overloaded)
        ));
        assert_eq!(
            quotas.reclamation(&a, Resource::Payload, 11),
            Some((Some(a.clone()), 1))
        );
        assert_eq!(quotas.reclamation(&a, Resource::Payload, 51), None);
        assert!(matches!(
            quotas.reserve(Some(&c), Resource::Other, 1),
            Err(Error::Overloaded)
        ));
        drop(second);
        let third = quotas.reserve(Some(&a), Resource::Payload, 60).unwrap();
        assert_eq!(quotas.active_keys.load(Ordering::Acquire), 1);
        assert!(!quotas.keys.borrow().contains_key(&b));
        assert_eq!(
            quotas.reclamation(&a, Resource::Payload, 1),
            Some((Some(a.clone()), 1))
        );
        let unkeyed = quotas.reserve(None, Resource::Other, 100).unwrap();
        assert_eq!(quotas.reclamation(&a, Resource::Other, 1), Some((None, 1)));
        drop((unkeyed, first, third));
        let first = quotas.reserve(Some(&a), Resource::Payload, 60).unwrap();
        let second = quotas.reserve(Some(&b), Resource::Other, 1).unwrap();
        quotas.stop();
        assert!(matches!(
            quotas.reserve(None, Resource::Payload, 1),
            Err(Error::Unavailable)
        ));
        assert!(quotas.reserve(None, Resource::Progress, 1).is_ok());
        let completion = quotas
            .reserve_completion(Some(&a), Resource::Payload, 40)
            .unwrap();
        assert_eq!(quotas.used(Resource::Payload), 100);
        assert!(matches!(
            quotas.reserve_completion(None, Resource::Payload, 1),
            Err(Error::Overloaded)
        ));
        drop((completion, first, second));
        assert_eq!(quotas.used(Resource::Payload), 0);
    }

    /// A lower second limit must not wrap or suggest an impossible byte remedy.
    #[test]
    fn dynamic_limits_reclamation_revalidates_aggregate_headroom() {
        for shared_usage in [false, true] {
            for existing_key in [false, true] {
                for (first, second, amount, expected) in [
                    (10, 0, 5, None),
                    (10, 4, 5, None),
                    (10, 5, 5, Some((None, 3))),
                    (10, 7, 5, Some((None, 1))),
                    (10, 8, 5, None),
                    (10, 20, 5, None),
                    (usize::MAX, usize::MAX, usize::MAX, Some((None, 3))),
                    (4, 0, 5, None),
                ] {
                    let quotas = Quotas::new(TestPolicy::new(100, 1));
                    let shared = quotas.shared();
                    let key = "a".to_owned();
                    let owner = existing_key
                        .then(|| quotas.reserve(Some(&key), Resource::Other, 1).unwrap());
                    let held = if shared_usage {
                        shared.reserve(Resource::Payload, 3).unwrap()
                    } else {
                        quotas.reserve(None, Resource::Payload, 3).unwrap()
                    };
                    quotas
                        .policy
                        .limit_samples
                        .lock()
                        .unwrap()
                        .extend([first, second]);
                    assert_eq!(
                        quotas.reclamation(&key, Resource::Payload, amount),
                        expected
                    );
                    assert_eq!(held.amount(), 3);
                    assert_eq!(quotas.used(Resource::Payload), 3);
                    assert_eq!(shared.used(Resource::Payload), 3);
                    assert_eq!(
                        quotas.active_keys.load(Ordering::Acquire),
                        usize::from(existing_key)
                    );
                    assert!(quotas.policy.rejected.lock().unwrap().is_empty());
                    drop((held, owner));
                    assert_eq!(shared.used(Resource::Payload), 0);
                    assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
                }
            }
        }
    }

    /// Reclamation callbacks may admit keyed work without borrowing the lookup table.
    mod reclamation_callbacks {
        use super::*;

        /// Application callback selected for one synchronous admission.
        #[derive(Clone, Copy, Eq, PartialEq)]
        enum Callback {
            Limit,

            Floor,

            Clone,
        }

        /// A one-shot callback, optionally delayed past the first limit sample.
        struct Hook {
            callback: Callback,

            skip: usize,

            action: Box<dyn FnOnce()>,
        }

        thread_local! {
            static HOOK: RefCell<Option<Hook>> = const { RefCell::new(None) };
        }

        /// Take the selected callback before running it so nested calls are inert.
        fn invoke(callback: Callback) {
            let hook = HOOK.with(|slot| {
                let mut slot = slot.borrow_mut();
                let hook = slot.as_mut()?;
                if hook.callback != callback {
                    return None;
                }
                if hook.skip != 0 {
                    hook.skip -= 1;
                    return None;
                }
                slot.take()
            });
            if let Some(hook) = hook {
                (hook.action)();
            }
        }

        /// A transferable identity with a worker-local clone callback.
        #[derive(Debug, Eq, PartialEq, Hash)]
        struct Key(u8);

        impl Clone for Key {
            /// Exercise application code when reclamation returns a keyed deficit.
            fn clone(&self) -> Self {
                invoke(Callback::Clone);
                Self(self.0)
            }
        }

        /// Fixed limits with one-shot application callbacks.
        struct ReentrantPolicy;

        impl Policy for ReentrantPolicy {
            type Class = Resource;

            type Key = Key;

            /// Permit both the initial charges and the nested admission.
            fn limit(&self, _: Resource) -> usize {
                invoke(Callback::Limit);
                100
            }

            /// Exercise callback reentry during fair-share calculation.
            fn floor(&self, _: Resource) -> usize {
                invoke(Callback::Floor);
                1
            }

            /// Leave room for nested creation as well as live-key reuse.
            fn max_keys(&self) -> usize {
                3
            }

            /// No waiter is needed for this synchronous regression.
            fn wakes(_: Resource) -> bool {
                false
            }

            /// The fixture does not allocate page backing.
            fn covers(_: Resource) -> bool {
                false
            }

            /// Every nested admission must succeed without a retry.
            fn rejected(&self, _: Rejection<Resource>) {
                panic!("unexpected rejection");
            }
        }

        /// Check deficit results and accounting after new-key and live-key reentry.
        fn check(callback: Callback, skip: usize) {
            for nested_key in [0, 2] {
                for (keyed, unkeyed, amount, expected) in [
                    (40, 0, 11, Some((Some(Key(0)), 1))),
                    (0, 90, 11, Some((None, 1))),
                    (0, 0, 11, None),
                    (0, 0, 51, None),
                ] {
                    let local_deficit = keyed != 0;
                    if (callback == Callback::Clone && !local_deficit)
                        || (skip != 0 && (local_deficit || amount > 50))
                    {
                        continue;
                    }
                    let quotas = Rc::new(Quotas::new(ReentrantPolicy));
                    let first = quotas.reserve(Some(&Key(0)), Resource::Other, 1).unwrap();
                    let second = quotas.reserve(Some(&Key(1)), Resource::Other, 1).unwrap();
                    let payload = (keyed + unkeyed != 0).then(|| {
                        quotas
                            .reserve(
                                (keyed != 0).then_some(&Key(0)),
                                Resource::Payload,
                                keyed + unkeyed,
                            )
                            .unwrap()
                    });
                    let nested = quotas.clone();
                    HOOK.with(|slot| {
                        *slot.borrow_mut() = Some(Hook {
                            callback,
                            skip,
                            action: Box::new(move || {
                                let charge = nested
                                    .reserve(Some(&Key(nested_key)), Resource::Progress, 1)
                                    .unwrap();
                                assert_eq!(nested.used(Resource::Progress), 1);
                                drop(charge);
                                assert_eq!(nested.used(Resource::Progress), 0);
                            }),
                        });
                    });
                    assert_eq!(
                        quotas.reclamation(&Key(0), Resource::Payload, amount),
                        expected
                    );
                    assert!(HOOK.with(|slot| slot.borrow().is_none()));
                    assert_eq!(quotas.used(Resource::Payload), keyed + unkeyed);
                    assert_eq!(quotas.active_keys.load(Ordering::Acquire), 2);
                    drop((payload, first, second));
                    assert_eq!(quotas.used(Resource::Payload), 0);
                    assert_eq!(quotas.used(Resource::Other), 0);
                    assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
                }
            }
        }

        /// The first aggregate sample runs without a key-table borrow.
        #[test]
        fn fair_limit_allows_keyed_reentry() {
            check(Callback::Limit, 0);
        }

        /// The later aggregate headroom sample also permits keyed reentry.
        #[test]
        fn aggregate_limit_allows_keyed_reentry() {
            check(Callback::Limit, 1);
        }

        /// The per-key floor callback may admit work synchronously.
        #[test]
        fn floor_allows_keyed_reentry() {
            check(Callback::Floor, 0);
        }

        /// Cloning the returned identity may admit work synchronously.
        #[test]
        fn key_clone_allows_keyed_reentry() {
            check(Callback::Clone, 0);
        }
    }

    /// Lower ceilings reject without losing old charges; higher ceilings admit exactly.
    #[test]
    fn dynamic_limits_admission_preserves_failure_accounting_and_success_edges() {
        let quotas = Quotas::new(TestPolicy::new(4, 1));
        let shared = quotas.shared();
        let key = "a".to_owned();
        quotas.policy.limit_samples.lock().unwrap().extend([10, 10]);
        let held = quotas.reserve(Some(&key), Resource::Payload, 6).unwrap();
        assert!(matches!(
            shared.reserve(Resource::Payload, 1),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            quotas.reserve(None, Resource::Payload, 1),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            quotas.reserve(Some(&key), Resource::Payload, 1),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            quotas.reserve_completion(Some(&key), Resource::Payload, 1),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            shared.reserve(Resource::Payload, 0),
            Err(Error::InvalidInput)
        ));
        assert!(matches!(
            quotas.reserve(Some(&key), Resource::Payload, 0),
            Err(Error::InvalidInput)
        ));
        assert_eq!(quotas.used(Resource::Payload), 6);
        assert_eq!(shared.used(Resource::Payload), 6);
        assert_eq!(
            held.local
                .as_ref()
                .unwrap()
                .counter(Resource::Payload)
                .used(),
            6
        );
        assert_eq!(quotas.active_keys.load(Ordering::Acquire), 1);
        {
            let rejected = quotas.policy.rejected.lock().unwrap();
            assert_eq!(rejected.len(), 4, "each failed attempt is reported once");
            for (index, rejection) in rejected.iter().enumerate() {
                let (key_used, key_limit) = if index == 2 {
                    (Some(6), Some(4))
                } else {
                    (None, None)
                };
                assert!(matches!(rejection, Rejection::Resource {
                    class: Resource::Payload, used: 6, limit: 4, requested: 1,
                    key_used: actual_used, key_limit: actual_limit,
                } if *actual_used == key_used && *actual_limit == key_limit));
            }
        }
        quotas.policy.limit_samples.lock().unwrap().extend([10, 10]);
        let refill = quotas.reserve(Some(&key), Resource::Payload, 4).unwrap();
        assert_eq!(shared.used(Resource::Payload), 10);
        assert_eq!(
            held.local
                .as_ref()
                .unwrap()
                .counter(Resource::Payload)
                .used(),
            10
        );
        drop((refill, held));
        assert_eq!(shared.used(Resource::Payload), 0);
        assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
        let exact = shared.reserve(Resource::Payload, 4).unwrap();
        assert_eq!(quotas.used(Resource::Payload), 4);
        drop(exact);
        assert_eq!(quotas.used(Resource::Payload), 0);
        assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 4);
    }

    /// Check nested admission at each key clone and preserve all owned counters.
    fn check_quota_key_clone(skip: usize) {
        use std::cell::Cell;

        /// Select one clone callback without affecting nested admission.
        struct Hook {
            skip: usize,

            action: Box<dyn FnOnce()>,
        }

        thread_local! {
            static HOOK: RefCell<Option<Hook>> = const { RefCell::new(None) };
        }

        /// A transferable key with a worker-local clone hook.
        #[derive(Debug, Eq, PartialEq, Hash)]
        struct Key(u8);

        impl Clone for Key {
            /// Release the hook borrow before running application code.
            fn clone(&self) -> Self {
                let hook = HOOK.with(|slot| {
                    let mut slot = slot.borrow_mut();
                    let hook = slot.as_mut()?;
                    if hook.skip != 0 {
                        hook.skip -= 1;
                        return None;
                    }
                    slot.take()
                });
                if let Some(hook) = hook {
                    (hook.action)();
                }
                Self(self.0)
            }
        }

        /// Mutable ceilings and observed rejection facts.
        struct ClonePolicy {
            limit: Cell<usize>,

            max_keys: Cell<usize>,

            rejected: RefCell<Vec<Rejection<Resource>>>,
        }

        impl Policy for ClonePolicy {
            type Class = Resource;

            type Key = Key;

            /// Read the current aggregate ceiling.
            fn limit(&self, _: Resource) -> usize {
                self.limit.get()
            }

            /// Read the current record ceiling.
            fn max_keys(&self) -> usize {
                self.max_keys.get()
            }

            /// This fixture has no admission waiter.
            fn wakes(_: Resource) -> bool {
                false
            }

            /// This fixture allocates no page backing.
            fn covers(_: Resource) -> bool {
                false
            }

            /// Retain the rejection facts for assertions.
            fn rejected(&self, rejection: Rejection<Resource>) {
                self.rejected.borrow_mut().push(rejection);
            }
        }

        for action in [
            "same",
            "keys",
            "fair",
            "global",
            "limit",
            "key_limit",
            "stop",
            "panic",
        ] {
            for completion in [false, true] {
                let quotas = Rc::new(Quotas::new(ClonePolicy {
                    limit: Cell::new(10),
                    max_keys: Cell::new(2),
                    rejected: RefCell::default(),
                }));
                let held = Rc::new(RefCell::new(Vec::new()));
                let nested = quotas.clone();
                let nested_held = held.clone();
                HOOK.with(|slot| {
                    *slot.borrow_mut() = Some(Hook {
                        skip,
                        action: Box::new(move || match action {
                            "same" => nested_held
                                .borrow_mut()
                                .push(nested.reserve(Some(&Key(0)), Resource::Payload, 3).unwrap()),
                            "keys" => {
                                for key in [1, 2] {
                                    let result =
                                        nested.reserve(Some(&Key(key)), Resource::Other, 1);
                                    if let Ok(charge) = result {
                                        nested_held.borrow_mut().push(charge);
                                    } else {
                                        assert_eq!(nested.keys.borrow().len(), 2);
                                    }
                                }
                            }
                            "fair" => nested_held
                                .borrow_mut()
                                .push(nested.reserve(Some(&Key(1)), Resource::Other, 1).unwrap()),
                            "global" => nested_held
                                .borrow_mut()
                                .push(nested.reserve(None, Resource::Payload, 4).unwrap()),
                            "limit" => nested.policy.limit.set(6),
                            "key_limit" => nested.policy.max_keys.set(0),
                            "stop" => nested.stop(),
                            "panic" => panic!("key clone failed"),
                            _ => unreachable!(),
                        }),
                    });
                });
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    if completion {
                        quotas.reserve_completion(Some(&Key(0)), Resource::Payload, 7)
                    } else {
                        quotas.reserve(Some(&Key(0)), Resource::Payload, 7)
                    }
                }));
                assert!(HOOK.with(|slot| slot.borrow().is_none()));
                if action == "panic" {
                    assert!(result.is_err());
                } else {
                    let result = result.unwrap();
                    let expected = match action {
                        "keys" if skip < 2 || !completion => Some(Error::Overloaded),
                        "key_limit" if skip < 2 => Some(Error::Overloaded),
                        "fair" if !completion => Some(Error::Overloaded),
                        "global" | "limit" => Some(Error::Overloaded),
                        "stop" if !completion => Some(Error::Unavailable),
                        _ => None,
                    };
                    if let Some(expected) = expected {
                        assert!(
                            matches!(result, Err(error) if error == expected),
                            "{action}"
                        );
                    } else {
                        let charge = result.unwrap();
                        assert_eq!(charge.key(), Some(&Key(0)));
                        if action == "same" {
                            assert!(Arc::ptr_eq(
                                charge.local.as_ref().unwrap(),
                                held.borrow()[0].local.as_ref().unwrap(),
                            ));
                        }
                        held.borrow_mut().push(charge);
                    }
                }
                if action == "keys" && skip < 2 {
                    let rejected = quotas.policy.rejected.borrow();
                    assert!(!rejected.is_empty());
                    assert!(
                        rejected
                            .iter()
                            .all(|r| matches!(r, Rejection::Keys { used: 2, limit: 2 }))
                    );
                }
                assert!(quotas.keys.borrow().len() <= 2);
                let owners = held.borrow();
                for class in [Resource::Payload, Resource::Other] {
                    let total: usize = owners
                        .iter()
                        .filter(|charge| charge.class.index() == class.index())
                        .map(Charge::amount)
                        .sum();
                    assert_eq!(quotas.used(class), total, "{action}");
                    assert!(total <= quotas.limit(class));
                    for charge in owners.iter().filter(|charge| charge.local.is_some()) {
                        let local = charge.local.as_ref().unwrap();
                        let keyed: usize = owners
                            .iter()
                            .filter(|other| {
                                other.key() == charge.key() && other.class.index() == class.index()
                            })
                            .map(Charge::amount)
                            .sum();
                        assert_eq!(local.counter(class).used(), keyed);
                    }
                }
                let active = owners
                    .iter()
                    .filter_map(Charge::key)
                    .collect::<std::collections::HashSet<_>>()
                    .len();
                assert_eq!(quotas.active_keys.load(Ordering::Acquire), active);
                drop(owners);
                held.borrow_mut().clear();
                assert_eq!(quotas.used(Resource::Payload), 0);
                assert_eq!(quotas.used(Resource::Other), 0);
                assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
            }
        }
    }

    /// The retirement-key clone may recursively reserve keyed work.
    #[test]
    fn quota_key_clone_first_reentry() {
        check_quota_key_clone(0);
    }

    /// The map-key clone must also run without holding the map borrow.
    #[test]
    fn quota_key_clone_second_reentry() {
        check_quota_key_clone(1);
    }

    /// The final charge-key clone must precede admission checks.
    #[test]
    fn quota_key_clone_charge_reentry() {
        check_quota_key_clone(2);
    }

    /// A key clone that reserves keyed work during reweigh does not panic.
    #[test]
    fn reweigh_allows_reentrant_key_clone() {
        thread_local! {
            static HOOK: RefCell<Option<Box<dyn FnOnce()>>> = const { RefCell::new(None) };
        }

        /// Run the one-shot hook after releasing its borrow.
        fn fire() {
            if let Some(action) = HOOK.with(|slot| slot.borrow_mut().take()) {
                action();
            }
        }

        /// A key whose clone runs a one-shot reentrant hook.
        #[derive(Debug, Eq, PartialEq, Hash)]
        struct Key(String);

        impl Clone for Key {
            /// Reenter admission from application clone code.
            fn clone(&self) -> Self {
                fire();
                Self(self.0.clone())
            }
        }

        /// Weighted policy over reentrant keys.
        struct KeyPolicy(TestPolicy);

        impl Policy for KeyPolicy {
            type Class = Resource;

            type Key = Key;

            /// Delegate the aggregate ceiling.
            fn limit(&self, class: Resource) -> usize {
                self.0.limit(class)
            }

            /// Delegate the record ceiling.
            fn max_keys(&self) -> usize {
                self.0.max_keys()
            }

            /// This fixture has no admission waiter.
            fn wakes(_: Resource) -> bool {
                false
            }

            /// This fixture allocates no page backing.
            fn covers(_: Resource) -> bool {
                false
            }

            /// This fixture does not observe rejections.
            fn rejected(&self, _: Rejection<Resource>) {}

            /// Reenter admission, then delegate the configured weight.
            fn weight(&self, key: &Key) -> u64 {
                fire();
                self.0.weight(&key.0)
            }
        }

        let policy = TestPolicy::new(1000, 16);
        policy
            .weights
            .lock()
            .unwrap()
            .extend([("a".to_owned(), 1), ("b".to_owned(), 1)]);
        let quotas = Rc::new(Quotas::new(KeyPolicy(policy.clone())));
        let a = quotas
            .reserve(Some(&Key("a".into())), Resource::Other, 1)
            .unwrap();
        policy.weights.lock().unwrap().insert("a".into(), 3);
        let held = Rc::new(RefCell::new(Vec::new()));
        let nested = quotas.clone();
        let nested_held = held.clone();
        HOOK.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                nested_held.borrow_mut().push(
                    nested
                        .reserve(Some(&Key("b".into())), Resource::Other, 1)
                        .unwrap(),
                );
            }));
        });
        quotas.reweigh();
        HOOK.with(|slot| slot.borrow_mut().take());
        // The hook fired inside reweigh and admitted "b" with weight 1.
        assert_eq!(held.borrow().len(), 1);
        assert_eq!(quotas.active_weight(), 4);
        assert_eq!(quotas.fair_limit(&Key("a".into()), Resource::Other), 750);
        held.borrow_mut().clear();
        drop(a);
        assert_eq!(quotas.active_weight(), 0);
    }

    /// A final keyed release frees its record before synchronous waiter reentry.
    #[test]
    fn keyed_release_retires_before_reentrant_wake() {
        thread_local! {
            static ON_WAKE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        }

        struct ReentrantWake(AtomicUsize);

        impl std::task::Wake for ReentrantWake {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
                let callback = ON_WAKE.with(|slot| slot.borrow_mut().take());
                if let Some(callback) = callback {
                    callback();
                }
            }
        }

        let quotas = Rc::new(Quotas::new(TestPolicy::new(10, 1)));
        let old = "old".to_owned();
        let mut charge = quotas.reserve(Some(&old), Resource::Other, 10).unwrap();
        let split = charge.split(4).unwrap();
        let wake = Arc::new(ReentrantWake(AtomicUsize::new(0)));
        let waker = std::task::Waker::from(wake.clone());

        let nested = quotas.clone();
        ON_WAKE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert_eq!(nested.used(Resource::Other), 6);
                assert_eq!(nested.active_keys.load(Ordering::Acquire), 1);
                let local = nested.keys.borrow()["old"].upgrade().unwrap();
                assert_eq!(local.counter(Resource::Other).used(), 6);
                assert!(nested.retired_keys.lock().unwrap().is_empty());
                assert!(matches!(
                    nested.reserve(Some(&"new".to_owned()), Resource::Other, 10),
                    Err(Error::Overloaded)
                ));
            }));
        });
        quotas.shared().register(&waker);
        drop(split);
        assert_eq!(wake.0.load(Ordering::Relaxed), 1);
        assert_eq!(charge.amount(), 6);
        assert_eq!(quotas.used(Resource::Other), 6);

        quotas.policy.rejected.lock().unwrap().clear();
        let nested = quotas.clone();
        ON_WAKE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert_eq!(nested.used(Resource::Other), 0);
                let replacement = nested
                    .reserve(Some(&"new".to_owned()), Resource::Other, 10)
                    .expect("the final release must free the key slot before waking");
                assert_eq!(nested.used(Resource::Other), 10);
                assert_eq!(nested.active_keys.load(Ordering::Acquire), 1);
                assert_eq!(
                    replacement
                        .local
                        .as_ref()
                        .unwrap()
                        .counter(Resource::Other)
                        .used(),
                    10
                );
                assert!(!nested.keys.borrow().contains_key("old"));
                assert_eq!(nested.keys.borrow().len(), 1);
                assert!(nested.retired_keys.lock().unwrap().is_empty());
                drop(replacement);
                assert_eq!(nested.used(Resource::Other), 0);
                assert_eq!(nested.active_keys.load(Ordering::Acquire), 0);
                assert_eq!(
                    nested
                        .retired_keys
                        .lock()
                        .unwrap()
                        .iter()
                        .collect::<Vec<_>>(),
                    vec!["new"]
                );
            }));
        });
        quotas.shared().register(&waker);
        drop(charge);
        assert_eq!(wake.0.load(Ordering::Relaxed), 2);
        assert_eq!(quotas.used(Resource::Other), 0);
        assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
        assert!(quotas.policy.rejected.lock().unwrap().is_empty());
        assert!(ON_WAKE.with(|slot| slot.borrow().is_none()));
    }

    /// Charges and shared handles retain exact accounting across threads.
    #[test]
    fn charge_validation_and_thread_safe_shared_usage() {
        /// Require transferable release and admission handles at compile time.
        fn send_sync<T: Send + Sync>() {}
        send_sync::<Charge<TestPolicy>>();
        send_sync::<SharedQuotas<TestPolicy>>();
        let quotas = Quotas::new(TestPolicy::new(100, 2));
        assert!(matches!(
            quotas.reserve(None, Resource::Payload, 0),
            Err(Error::InvalidInput)
        ));
        assert!(matches!(
            quotas.reserve(None, Resource::Payload, usize::MAX),
            Err(Error::Overloaded)
        ));
        let shared = quotas.shared();
        assert!(matches!(
            shared.reserve(Resource::Payload, 0),
            Err(Error::InvalidInput)
        ));
        let mut charge = shared.reserve(Resource::Payload, 100).unwrap();
        assert!(quotas.owns(&charge));
        assert!(!Quotas::new(TestPolicy::new(100, 2)).owns(&charge));
        assert!(page_alloc::Charge::covers(&charge, 100));
        assert!(!page_alloc::Charge::covers(&charge, 101));
        assert!(charge.validate(Resource::Payload, 100).is_ok());
        assert!(charge.validate(Resource::Other, 100).is_err());
        assert!(charge.split(100).is_err());
        assert!(charge.split(0).is_err());
        assert!(charge.shrink(0).is_err());
        assert!(charge.shrink(101).is_err());
        let split = charge.split(60).unwrap();
        charge.shrink(19).unwrap();
        assert_eq!(quotas.used(Resource::Payload), 79);
        std::thread::spawn(move || drop(split)).join().unwrap();
        assert_eq!(shared.used(Resource::Payload), 19);
        drop(charge);
        let other = shared.reserve(Resource::Other, 1).unwrap();
        assert!(!page_alloc::Charge::covers(&other, 1));
        drop(other);
        quotas.stop();
        assert!(shared.is_stopped());
        assert!(matches!(
            shared.reserve(Resource::Payload, 1),
            Err(Error::Unavailable)
        ));
    }

    /// Local callbacks cannot admit ordinary work after stopping the authority.
    #[test]
    fn local_reservation_rechecks_stop_after_callbacks() {
        use std::cell::Cell;

        thread_local! {
            static CLONE_HOOK: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        }

        /// A transferable key whose next clone can stop the local authority.
        #[derive(Eq, PartialEq, Hash)]
        struct Key(u8);

        impl Clone for Key {
            /// Run the one-shot callback without holding its borrow.
            fn clone(&self) -> Self {
                let hook = CLONE_HOOK.with(|hook| hook.borrow_mut().take());
                if let Some(hook) = hook {
                    hook();
                }
                Self(self.0)
            }
        }

        /// Select which application callback requests shutdown.
        #[derive(Clone, Copy, Debug, Eq, PartialEq)]
        enum Hook {
            Limit,

            Floor,

            Clone,
        }

        /// Stop through a weak authority link without retaining an ownership cycle.
        struct StopPolicy {
            quotas: RefCell<std::rc::Weak<Quotas<Self>>>,

            hook: Cell<Option<Hook>>,

            rejections: Cell<usize>,
        }

        impl StopPolicy {
            /// Stop only at the selected policy callback.
            fn stop_at(&self, hook: Hook) {
                if self.hook.get() == Some(hook) {
                    self.hook.set(None);
                    self.quotas.borrow().upgrade().unwrap().stop();
                }
            }
        }

        impl Policy for StopPolicy {
            type Class = Resource;

            type Key = Key;

            /// Leave enough capacity for both the existing and attempted charge.
            fn limit(&self, _: Resource) -> usize {
                self.stop_at(Hook::Limit);
                10
            }

            /// Exercise shutdown during keyed fair-share calculation.
            fn floor(&self, _: Resource) -> usize {
                self.stop_at(Hook::Floor);
                1
            }

            /// Permit one live identity and verify its eventual retirement.
            fn max_keys(&self) -> usize {
                1
            }

            /// Preserve the fixture's class-specific release wake policy.
            fn wakes(class: Resource) -> bool {
                TestPolicy::wakes(class)
            }

            /// Preserve the fixture's drain-progress exception.
            fn allows_stopped(class: Resource) -> bool {
                TestPolicy::allows_stopped(class)
            }

            /// No page backing is allocated by this test.
            fn covers(_: Resource) -> bool {
                false
            }

            /// Stop rejection must not be reported as quota pressure.
            fn rejected(&self, _: Rejection<Resource>) {
                self.rejections.set(self.rejections.get() + 1);
            }
        }

        for hook in [Hook::Limit, Hook::Floor, Hook::Clone] {
            // Unkeyed, new key, and existing key exercise distinct ownership paths.
            for (keyed, live_key) in [(false, false), (true, false), (true, true)] {
                if !keyed && hook != Hook::Limit {
                    continue;
                }
                for (class, completion) in [
                    (Resource::Payload, false),
                    (Resource::Other, false),
                    (Resource::Progress, false),
                    (Resource::Payload, true),
                ] {
                    if completion && hook == Hook::Floor {
                        continue;
                    }
                    let quotas = Rc::new(Quotas::new(StopPolicy {
                        quotas: RefCell::default(),
                        hook: Cell::new(None),
                        rejections: Cell::new(0),
                    }));
                    *quotas.policy.quotas.borrow_mut() = Rc::downgrade(&quotas);
                    let key = Key(1);
                    let existing = quotas.reserve(live_key.then_some(&key), class, 3).unwrap();
                    if hook == Hook::Clone {
                        let weak = Rc::downgrade(&quotas);
                        CLONE_HOOK.with(|hook| {
                            *hook.borrow_mut() =
                                Some(Box::new(move || weak.upgrade().unwrap().stop()));
                        });
                    } else {
                        quotas.policy.hook.set(Some(hook));
                    }
                    let result = if completion {
                        quotas.reserve_completion(keyed.then_some(&key), class, 7)
                    } else {
                        quotas.reserve(keyed.then_some(&key), class, 7)
                    };
                    assert!(quotas.is_stopped(), "callback {hook:?} must run");
                    if completion || StopPolicy::allows_stopped(class) {
                        let charge = result.unwrap();
                        assert_eq!(charge.amount(), 7);
                        assert_eq!(quotas.used(class), 10);
                        if let Some(local) = &charge.local {
                            assert_eq!(local.counter(class).used(), if live_key { 10 } else { 7 });
                        }
                        drop(charge);
                    } else {
                        assert!(matches!(result, Err(Error::Unavailable)), "hook {hook:?}");
                    }
                    assert_eq!(quotas.used(class), 3);
                    assert_eq!(
                        quotas.active_keys.load(Ordering::Acquire),
                        usize::from(live_key)
                    );
                    if let Some(local) = &existing.local {
                        assert_eq!(local.counter(class).used(), 3);
                    }
                    assert_eq!(quotas.policy.rejections.get(), 0);
                    drop(existing);
                    assert_eq!(quotas.used(class), 0);
                    assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
                    let replacement = quotas.reserve_completion(Some(&Key(2)), class, 10).unwrap();
                    assert!(!quotas.keys.borrow().contains_key(&key));
                    drop(replacement);
                    assert_eq!(quotas.used(class), 0);
                    assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
                }
            }
        }
    }

    /// Cross-thread release and local stop notify the registered shared waiter.
    #[test]
    fn release_and_stop_wake_shared_waiters() {
        /// Count notifications without accessing worker-local state.
        struct WakeCount(AtomicUsize);

        impl std::task::Wake for WakeCount {
            /// Record one notification.
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let count = Arc::new(WakeCount(AtomicUsize::new(0)));
        let waker = std::task::Waker::from(count.clone());
        let quotas = Quotas::new(TestPolicy::new(1, 1));
        let shared = quotas.shared();
        shared.register(&waker);
        let charge = shared.reserve(Resource::Other, 1).unwrap();
        assert!(matches!(
            shared.reserve(Resource::Other, 1),
            Err(Error::Overloaded)
        ));
        std::thread::spawn(move || drop(charge)).join().unwrap();
        assert_eq!(count.0.load(Ordering::Relaxed), 1);
        shared.register(&waker);
        quotas.stop();
        assert_eq!(count.0.load(Ordering::Relaxed), 2);
    }
}
