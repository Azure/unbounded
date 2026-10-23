//! Policy-driven admission, charged storage, and completion-owned flow control.
//!
//! Implement [`Class`] and [`Policy`] to supply resource limits and keyed fairness.
//! [`Quotas`] is a worker-local authority; [`SharedQuotas`] admits unkeyed work
//! across threads. Keep each [`Charge`] with its resource until completion. A
//! completion reservation bypasses stop and fair-share checks, not aggregate or
//! key-record limits. Policy can also allow selected classes after stop.
//!
//! Recycling retains at most eight buffers and 128 MiB, grouped by resource and
//! size class. Each class also retains at most half its policy limit. Within
//! that bound, a [`PoolDemand`] controller sizes the pool from observed misses
//! and idle windows; drive it with [`Quotas::maintain_buffers`]. Pressure retries admission after applicable reclamation; stop
//! releases retained buffers. Shared handles never retain the recycler.
//!
//! [`ChargedBuffer`] pairs fixed initialized backing with admission. Low-level
//! raw buffer users must themselves retain sufficient charge for every allocation
//! and validate class, key, and provenance. Application authentication, error
//! classification, request deadlines, and cancellation policy stay with callers.
//!
//! Endpoint circuits, adaptive admission, handoffs, and hedge alarms use distinct
//! ownership rules. Pipes retain their charges while idle; socket-retained bytes
//! are outside the pipe capacity budget. The `simulation` feature forwards the
//! runtime's simulated descriptors without changing admission policy.
//!
//! [`coalesce`] provides worker-local keyed cohorts, shared results, and flight
//! lifecycle tracking. Callers retain execution, admission, and result policy;
//! cancellation never substitutes for real operation completion.
#![deny(unsafe_op_in_unsafe_fn)]

/// Adaptive admission and completion-owned handoffs.
mod admission;

/// Keyed cohorts and completion-owned flight tracking.
pub mod coalesce;

/// Progress prediction and bounded synchronous reclamation.
pub mod progress;

/// Deadline-aware FIFO admission bookkeeping.
pub mod fifo;

/// Compact interval selection and bounded subscription fanout.
pub mod subscriptions;

/// Charged kernel pipes and worker-local reuse.
mod pipe;

pub use admission::{
    Adaptive, Admitted, Circuits, Clock as AdaptiveClock, Config as AdaptiveConfig,
    Event as AdaptiveEvent, Handoff, HandoffAdmission, HedgePermit, Hedges,
    Observer as AdaptiveObserver, Offer, Outcome as AdaptiveOutcome, Permit as AdaptivePermit,
    Probe,
};
pub use page_alloc::{PoolDemand, PoolStats};
pub use pipe::{MAX_PIPE_BYTES, PipeLease, PipePool, splice_unsupported};

use std::{
    cell::RefCell,
    collections::{HashMap, VecDeque},
    hash::Hash,
    marker::PhantomData,
    rc::Rc,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    },
};

/// Admission and pipe-creation failures, independent of application error policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The requested geometry or state transition is invalid.
    InvalidInput,

    /// A resource or keyed fairness limit is exhausted.
    Overloaded,

    /// Admission has stopped or its shared state is unavailable.
    Unavailable,

    /// A kernel pipe operation failed during creation.
    Io,
}

impl std::fmt::Display for Error {
    /// Describe the failure without application resource names.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::InvalidInput => "invalid flow-control input",
            Self::Overloaded => "flow-control quota exhausted",
            Self::Unavailable => "flow control unavailable",
            Self::Io => "flow-control I/O failed",
        })
    }
}

impl std::error::Error for Error {}

/// A flow-control operation's result.
pub type Result<T> = std::result::Result<T, Error>;

/// A dense, stable index in `0..COUNT`, unique to each resource class.
pub trait Class: Copy + Send + Sync + 'static {
    /// Number of distinct resource classes.
    const COUNT: usize;

    /// Return this class's unique index, strictly below `COUNT`.
    fn index(self) -> usize;

    /// Default to secure erasure unless the class contains only public bytes.
    fn requires_wipe(self) -> bool {
        true
    }
}

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

/// Worker-local keyed admission and recycler authority; never moves across workers.
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

    buffers: Arc<Buffers<P>>,

    local: PhantomData<Rc<()>>,

    rejection: RefCell<Option<Option<Rejection<P::Class>>>>,
}

/// Owns a live charge independently of the local quota authority and policy.
pub struct Charge<P: Policy> {
    class: P::Class,

    amount: usize,

    key: Option<P::Key>,

    totals: Arc<Counters<P::Key>>,

    local: Option<Arc<Counters<P::Key>>>,

    buffers: Weak<Buffers<P>>,

    stopped: Arc<AtomicBool>,

    // Backing adopted from the recycler when this charge's admission needed the
    // bytes that held it. Reusing it avoids unmapping and remapping a page.
    // Boxed so charges without adopted backing grow by one pointer only.
    spare: Option<Box<Mutex<Vec<u8>>>>,
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

/// An unkeyed admission and usage handle. It retains no recycler buffers.
/// It is Send + Sync when the policy is Send + Sync.
pub struct SharedQuotas<P: Policy> {
    policy: P,

    totals: Arc<Counters<P::Key>>,

    stopped: Arc<AtomicBool>,

    // Weak: telemetry may read pool statistics without keeping buffers alive.
    buffers: Weak<Buffers<P>>,
}

impl<P: Policy + Clone> Clone for SharedQuotas<P> {
    /// Share aggregate counters and stop state without retaining recycled backing.
    fn clone(&self) -> Self {
        Self {
            policy: self.policy.clone(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
            buffers: self.buffers.clone(),
        }
    }
}

impl<P: Policy> SharedQuotas<P> {
    /// Read the recycler's size, target, and counters; empty after the authority drops.
    pub fn buffer_pool_stats(&self) -> PoolStats {
        self.buffers
            .upgrade()
            .map(|buffers| buffers.stats())
            .unwrap_or_default()
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

    /// Admit a nonzero unkeyed amount without retaining the local recycler.
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
            buffers: Weak::new(),
            stopped: self.stopped.clone(),
            spare: None,
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
        Ok(self.transfer(amount, self.buffers.clone(), key))
    }

    /// Caller must retain at least the capacity of every live backing allocation.
    pub fn shrink(&mut self, amount: usize) -> Result<()> {
        if amount == 0 || amount > self.amount {
            return Err(Error::InvalidInput);
        }
        self.release_to(amount);
        Ok(())
    }

    /// Obtain zeroed backing without transferring or subdividing this charge.
    ///
    /// This low-level API does not track other allocations made with the same
    /// charge. The caller must retain sufficient admission for all live backing,
    /// validate its class/key/provenance, and never shrink below live capacity.
    pub fn buffer(&self, length: usize) -> Result<Vec<u8>> {
        if length > self.amount {
            return Err(Error::InvalidInput);
        }
        if let Some(mut bytes) = self.take_spare(length) {
            if let Some(pool) = self.buffers.upgrade() {
                let mut pool = pool.lock().unwrap_or_else(|e| e.into_inner());
                let left = pool.entries.len();
                pool.demand.record_hit(left);
            }
            if !self.class.requires_wipe() {
                // Same zeroed-checkout contract as a pooled entry.
                bytes.resize(bytes.capacity(), 0);
                bytes.fill(0);
            }
            bytes.truncate(length);
            return Ok(bytes);
        }
        if let Some(pool) = self.buffers.upgrade() {
            let mut pool = pool.lock().unwrap_or_else(|e| e.into_inner());
            let index = [true, false].into_iter().find_map(|same_class| {
                pool.entries.iter().position(|(bytes, old)| {
                    (!same_class || old.class.index() == self.class.index())
                        && bytes.capacity() >= length
                        && bytes.capacity() <= self.amount
                        // A tag-sized tail can straddle a class boundary. Only
                        // borrow that neighbor when all capacity is admitted.
                        && (page_alloc::buffer_capacity(bytes.capacity())
                            == page_alloc::buffer_capacity(length)
                            || bytes.capacity() - length < 64 * 1024)
                })
            });
            if let Some(index) = index {
                let (mut bytes, old) = pool.entries.remove(index).expect("pooled entry");
                let left = pool.entries.len();
                pool.demand.record_hit(left);
                drop(pool);
                if !old.class.requires_wipe() {
                    // Public ciphertext was not securely erased at release.
                    // Preserve the zeroed-checkout contract, including spare bytes.
                    bytes.resize(bytes.capacity(), 0);
                    bytes.fill(0);
                }
                drop(old);
                bytes.truncate(length);
                return Ok(bytes);
            }
            if length >= 1024 * 1024 {
                pool.demand.record_miss();
            }
        }
        let capacity = page_alloc::buffer_capacity(length)
            .filter(|&n| length >= 1024 * 1024 && n <= self.amount)
            .unwrap_or(length);
        let mut bytes = zeroed_payload(capacity)?;
        bytes.truncate(length);
        Ok(bytes)
    }

    /// Take adopted backing that fits `length` under the same rules as a pooled entry.
    fn take_spare(&self, length: usize) -> Option<Vec<u8>> {
        let mut spare = self
            .spare
            .as_ref()?
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let capacity = spare.capacity();
        let fits = capacity >= length
            && capacity <= self.amount
            && (page_alloc::buffer_capacity(capacity) == page_alloc::buffer_capacity(length)
                || capacity - length < 64 * 1024);
        // An empty Vec left behind never fits again because length is nonzero.
        if fits && length > 0 {
            Some(std::mem::take(&mut *spare))
        } else {
            None
        }
    }

    /// Drop adopted backing that this charge no longer admits in full.
    fn trim_spare(&mut self) {
        if let Some(spare) = &mut self.spare
            && spare
                .get_mut()
                .unwrap_or_else(|e| e.into_inner())
                .capacity()
                > self.amount
        {
            self.spare = None;
        }
    }

    /// At most eight >=1MiB buffers, fewer while demand is low, within the
    /// global and per-class byte budgets retain live
    /// charges; only the final exclusive payload owner may return an allocation.
    pub fn recycle(&mut self, bytes: Vec<u8>) {
        let mut pending = RecycleBytes {
            bytes,
            secret: self.class.requires_wipe(),
        };
        self.try_recycle(&mut pending.bytes);
    }

    fn try_recycle(&mut self, bytes: &mut Vec<u8>) {
        if self.stopped.load(Ordering::Acquire)
            || bytes.capacity() < 1024 * 1024
            || bytes.capacity() > self.amount
        {
            return;
        }
        let Some(pool) = self.buffers.upgrade() else {
            return;
        };
        // Application cloning may reenter stop or fill the pool. Clone unlocked,
        // then check retention again before transferring any admission.
        let key = self.key.clone();
        let class_limit = pool.limits[self.class.index()].load(Ordering::Relaxed)
            / pool.class_divisor.load(Ordering::Relaxed);
        let Ok(mut pool) = pool.try_lock() else {
            return;
        };
        if self.stopped.load(Ordering::Acquire) {
            return;
        }
        let class_bytes = pool
            .entries
            .iter()
            .filter(|(_, charge)| charge.class.index() == self.class.index())
            .map(|(_, charge)| charge.amount)
            .sum::<usize>();
        if self.amount > class_limit.saturating_sub(class_bytes) {
            pool.demand.record_reject();
            return;
        }
        let bytes_now = pool
            .entries
            .iter()
            .map(|(b, _)| b.capacity())
            .sum::<usize>();
        let len = pool.entries.len();
        if !pool
            .demand
            .admit(len, bytes_now, bytes.capacity(), BUFFER_BUDGET)
        {
            return;
        }
        if self.class.requires_wipe() {
            wipe_payload(bytes);
            // SAFETY: wipe_payload initialized the entire capacity above.
            unsafe { bytes.set_len(bytes.capacity()) };
        }
        let charge = self.transfer(self.amount, Weak::new(), key);
        pool.entries.push_back((std::mem::take(bytes), charge));
    }

    /// Move admission with a precloned key without touching either usage counter.
    fn transfer(&mut self, amount: usize, buffers: Weak<Buffers<P>>, key: Option<P::Key>) -> Self {
        self.amount -= amount;
        self.trim_spare();
        Self {
            class: self.class,
            amount,
            key,
            totals: self.totals.clone(),
            local: self.local.clone(),
            buffers,
            stopped: self.stopped.clone(),
            spare: None,
        }
    }

    /// Return released admission without notifying the shared waiter.
    fn release_to(&mut self, amount: usize) {
        let released = self.amount - amount;
        self.amount = amount;
        // Unadmitted backing must not outlive the admission that covered it.
        self.trim_spare();
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
            buffers: Arc::new(Buffers::new(PoolDemand::new().with_bounds(0, BUFFER_DEPTH))),
            local: PhantomData,
            rejection: RefCell::new(None),
        }
    }

    /// Replace the recycler's demand controller; its maximum is capped at eight.
    #[must_use]
    pub fn with_buffer_demand(self, demand: PoolDemand) -> Self {
        let max = demand.max_depth().min(BUFFER_DEPTH);
        let target = demand.target();
        let demand = demand.with_bounds(0, max).with_target(target);
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .demand = demand;
        self
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

    /// Create a transferable unkeyed handle without retaining recycler storage.
    pub fn shared(&self) -> SharedQuotas<P>
    where
        P: Clone,
    {
        SharedQuotas {
            policy: self.policy.clone(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
            buffers: Arc::downgrade(&self.buffers),
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
        self.reclaim_buffers();
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
        self.reserve_reclaiming(key, class, amount, AdmissionMode::Ordinary)
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
        self.reserve_reclaiming(key, class, amount, AdmissionMode::Completion)
    }

    /// Sum admission retained by idle recycler allocations.
    pub fn retained_buffer_bytes(&self) -> usize {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .entries
            .iter()
            .map(|(_, charge)| charge.amount())
            .sum()
    }

    /// Release every idle recycler allocation and its charge.
    pub fn reclaim_buffers(&self) {
        let mut pool = self.buffers.lock().unwrap_or_else(|e| e.into_inner());
        let removed = std::mem::take(&mut pool.entries);
        pool.demand.record_trim(removed.len(), 0);
        drop(pool);
        drop(removed);
    }

    /// Run one demand window at `now`, releasing idle entries oldest first.
    ///
    /// Returns the admission bytes released. Skips silently when the recycler
    /// is contended; between windows this is O(1). Entries are freed after the
    /// lock is released; secret entries were already erased when recycled.
    pub fn maintain_buffers(&self, now: std::time::Instant) -> usize {
        let Ok(mut pool) = self.buffers.try_lock() else {
            return 0;
        };
        let len = pool.entries.len();
        let release = pool.demand.tick(now, len);
        if release == 0 {
            return 0;
        }
        let removed: Vec<_> = pool.entries.drain(..release).collect();
        drop(pool);
        let bytes = removed.iter().map(|(_, charge)| charge.amount).sum();
        drop(removed);
        bytes
    }

    /// Hold the recycler empty and refuse returns while constrained.
    ///
    /// Constraining releases retained entries on the next maintenance pass.
    pub fn constrain_buffers(&self, constrained: bool) {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .demand
            .set_constrained(constrained);
    }

    /// Read the recycler's size, target, and cumulative counters.
    pub fn buffer_pool_stats(&self) -> PoolStats {
        self.buffers.stats()
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

    /// Retry overload once after reclaiming relevant retained backing.
    fn reserve_reclaiming(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        mode: AdmissionMode,
    ) -> std::result::Result<Charge<P>, ReserveError<P::Class>> {
        let result = self.reserve_inner(key, class, amount, mode);
        if matches!(
            result,
            Err(ReserveError {
                error: Error::Overloaded,
                ..
            })
        ) {
            let adopted = self.reclaim_buffers_for(
                key,
                class,
                amount,
                result.as_ref().err().and_then(|e| e.rejection),
            );
            // Keep one reclaimed page-sized allocation for the new charge rather
            // than unmapping it and mapping a fresh one for the same bytes.
            let mut charge = self.reserve_inner(key, class, amount, mode)?;
            if let Some(bytes) = adopted.filter(|bytes| bytes.capacity() <= charge.amount) {
                charge.spare = Some(Box::new(Mutex::new(bytes)));
            }
            return Ok(charge);
        }
        result
    }

    /// Reclaim only the class causing pressure, stopping once its deficit is met.
    /// Key-table exhaustion is separate: release one idle key's entries, not all.
    /// Returns one reclaimed allocation of the requested size class, if any, with
    /// its admission released, so the caller can reuse it once admission succeeds.
    fn reclaim_buffers_for(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        rejection: Option<Rejection<P::Class>>,
    ) -> Option<Vec<u8>> {
        let mut guard = self.buffers.lock().unwrap_or_else(|e| e.into_inner());
        let buffers = &mut guard.entries;
        let mut removed = Vec::new();
        if matches!(rejection, Some(Rejection::Keys { .. })) {
            // Arc cloning cannot invoke application key callbacks under the lock.
            let victim = buffers.iter().find_map(|(_, charge)| charge.local.clone());
            if let Some(victim) = victim {
                let mut i = 0;
                while i < buffers.len() {
                    if buffers[i]
                        .1
                        .local
                        .as_ref()
                        .is_some_and(|local| Arc::ptr_eq(local, &victim))
                    {
                        removed.push(buffers.remove(i).expect("pooled entry"));
                    } else {
                        i += 1;
                    }
                }
            }
        } else if let Some(Rejection::Resource {
            used,
            limit,
            key_used,
            key_limit,
            ..
        }) = rejection
        {
            let keyed = key_used.zip(key_limit);
            let (used, limit) = keyed.unwrap_or((used, limit));
            let mut deficit = used.saturating_add(amount).saturating_sub(limit);
            // Prefer the requested size class, then other sizes in this resource
            // class: quota pressure is measured in bytes, not allocation sizes.
            for exact in [true, false] {
                let mut i = 0;
                while i < buffers.len() && deficit != 0 {
                    let (bytes, charge) = &buffers[i];
                    if charge.class.index() == class.index()
                        && (keyed.is_none() || charge.key.as_ref() == key)
                        && (!exact
                            || page_alloc::buffer_capacity(bytes.capacity())
                                == page_alloc::buffer_capacity(amount))
                    {
                        deficit = deficit.saturating_sub(charge.amount);
                        removed.push(buffers.remove(i).expect("pooled entry"));
                    } else {
                        i += 1;
                    }
                }
            }
        }
        let left = buffers.len();
        guard.demand.record_trim(removed.len(), left);
        drop(guard);
        let adopted = removed
            .iter()
            .position(|(bytes, charge)| {
                charge.class.index() == class.index()
                    && bytes.capacity() <= amount
                    && page_alloc::buffer_capacity(bytes.capacity())
                        == page_alloc::buffer_capacity(amount)
            })
            .map(|i| std::mem::take(&mut removed[i].0));
        drop(removed);
        adopted
    }

    /// Perform one admission attempt and report its rejection before retrying.
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
        self.buffers.limits[class.index()].store(limit, Ordering::Relaxed);
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
            buffers: Arc::downgrade(&self.buffers),
            stopped: self.stopped.clone(),
            spare: None,
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
        self.policy.rejected(rejection);
        ReserveError {
            error: Error::Overloaded,
            rejection: Some(rejection),
        }
    }
}

/// Recyclable vector ownership for caller-validated payloads. This is not an I/O
/// buffer: callers must keep allocation geometry stable while kernel pointers live.
/// Class, key, capacity and authority validation remain the caller's responsibility.
pub struct ChargedBytes<P: Policy> {
    /// Payload allocation, recycled through its original authority on final drop.
    pub bytes: Vec<u8>,

    /// The charge retained with the complete allocation.
    pub reservation: Charge<P>,
}

impl<P: Policy> Drop for ChargedBytes<P> {
    /// Wipe and recycle the allocation before releasing its accounting owner.
    fn drop(&mut self) {
        self.reservation.recycle(std::mem::take(&mut self.bytes));
    }
}

/// Fixed initialized backing paired with its live quota charge.
pub struct ChargedBuffer<P: Policy> {
    bytes: Vec<u8>,

    charge: Option<Charge<P>>,

    recycle: bool,
}

impl<P: Policy> ChargedBuffer<P> {
    /// Allocate admitted backing; the caller validates class, key, and provenance.
    pub fn new(charge: Charge<P>, length: usize) -> Result<Self> {
        if length == 0 {
            return Err(Error::InvalidInput);
        }
        let mut buffer = Self::unpooled(charge, length)?;
        buffer
            .charge
            .as_mut()
            .expect("owned charge")
            .shrink(buffer.bytes.capacity())?;
        buffer.recycle = true;
        Ok(buffer)
    }

    /// Preserve the complete reservation and release backing on drop. Transport
    /// staging may borrow recycler storage without retaining it after abandonment.
    /// An empty view is allowed while its nonzero reservation remains owned.
    pub fn unpooled(charge: Charge<P>, length: usize) -> Result<Self> {
        let bytes = charge.buffer(length)?;
        // Do not box or resize again while I/O pointers are live.
        Ok(Self {
            bytes,
            charge: Some(charge),
            recycle: false,
        })
    }

    /// Borrow the charge retained for the fixed allocation.
    pub fn charge(&self) -> &Charge<P> {
        self.charge.as_ref().expect("owned charge")
    }

    /// Borrow initialized bytes without changing allocation geometry.
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }

    /// Mutate initialized bytes without changing allocation geometry.
    pub fn bytes_mut(&mut self) -> &mut [u8] {
        &mut self.bytes
    }

    /// Transfer backing and admission together to a final completion owner.
    pub fn into_parts(mut self) -> (Box<[u8]>, Charge<P>) {
        (
            std::mem::take(&mut self.bytes).into_boxed_slice(),
            self.charge.take().expect("owned charge"),
        )
    }
}

impl<P: Policy> Drop for ChargedBuffer<P> {
    /// Recycle ordinary backing; unpooled staging drops before releasing its charge.
    fn drop(&mut self) {
        if self.recycle
            && let Some(charge) = &mut self.charge
        {
            charge.recycle(std::mem::take(&mut self.bytes));
        }
    }
}

// SAFETY: private fixed backing and charge remain exclusively owned.
unsafe impl<P: Policy> uring_runtime::reactor::IoBuffer for ChargedBuffer<P> {
    /// Buffer access uses the crate's application-independent error type.
    type Error = Error;

    /// Borrow stable initialized backing while the runtime owns this buffer.
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes)
    }

    /// Borrow stable mutable backing while the runtime owns this buffer.
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes)
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

/// Bounded free lists keyed by class and capacity class. Secret backing is zeroed;
/// public backing is cleared on checkout, including cross-class reuse.
///
/// Entries are kept oldest first so demand maintenance trims the longest idle.
struct Buffers<P: Policy> {
    pool: Mutex<Recycler<P>>,

    /// Last policy limit observed at admission per class; retention keeps at
    /// most half of it idle. Unknown classes start at zero and retain nothing.
    limits: Box<[AtomicUsize]>,

    /// Divisor applied to each class limit; tests of pre-demand pressure
    /// behavior lift it to retain a whole class.
    class_divisor: AtomicUsize,
}

/// Idle entries and the demand controller that sizes them, under one lock.
struct Recycler<P: Policy> {
    entries: VecDeque<(Vec<u8>, Charge<P>)>,

    demand: PoolDemand,
}

impl<P: Policy> Buffers<P> {
    fn new(demand: PoolDemand) -> Self {
        Self {
            pool: Mutex::new(Recycler {
                entries: VecDeque::new(),
                demand,
            }),
            limits: (0..P::Class::COUNT).map(|_| AtomicUsize::new(0)).collect(),
            class_divisor: AtomicUsize::new(2),
        }
    }

    fn lock(&self) -> std::sync::LockResult<std::sync::MutexGuard<'_, Recycler<P>>> {
        self.pool.lock()
    }

    fn try_lock(&self) -> std::sync::TryLockResult<std::sync::MutexGuard<'_, Recycler<P>>> {
        self.pool.try_lock()
    }

    fn stats(&self) -> PoolStats {
        let pool = self.lock().unwrap_or_else(|e| e.into_inner());
        let bytes = pool.entries.iter().map(|(_, charge)| charge.amount).sum();
        pool.demand.stats(pool.entries.len(), bytes)
    }
}

#[cfg(test)]
impl<P: Policy> std::ops::Deref for Recycler<P> {
    type Target = VecDeque<(Vec<u8>, Charge<P>)>;

    fn deref(&self) -> &Self::Target {
        &self.entries
    }
}

/// Maximum recycler depth; demand chooses the target within it.
const BUFFER_DEPTH: usize = page_alloc::DEFAULT_MAX_DEPTH;
const BUFFER_BUDGET: usize = 128 * 1024 * 1024;

/// Preserve erase-before-free on rejection and on a panicking key callback.
struct RecycleBytes {
    bytes: Vec<u8>,
    secret: bool,
}

impl Drop for RecycleBytes {
    fn drop(&mut self) {
        if self.secret {
            wipe_payload(&mut self.bytes);
        }
    }
}

/// Fallible initialized byte storage using the same layout as Vec<u8>.
fn zeroed_payload(length: usize) -> Result<Vec<u8>> {
    // SAFETY: this callback supplies the exact zeroed global allocation requested.
    unsafe {
        zeroed_payload_with(length, |layout| {
            // SAFETY: checked nonzero layout; the returned Vec owns deallocation.
            std::alloc::alloc_zeroed(layout)
        })
    }
}

/// Injectable allocator keeps null-failure tests independent of host overcommit.
/// Safety: callback returns null or a zeroed global allocation with this layout.
unsafe fn zeroed_payload_with(
    length: usize,
    allocate: impl FnOnce(std::alloc::Layout) -> *mut u8,
) -> Result<Vec<u8>> {
    if length == 0 {
        return Ok(Vec::new());
    }
    let layout = std::alloc::Layout::from_size_align(length, 1).map_err(|_| Error::Overloaded)?;
    let pointer = std::ptr::NonNull::new(allocate(layout)).ok_or(Error::Overloaded)?;
    // SAFETY: alloc_zeroed uses the global allocator with nonzero size and align
    // 1, exactly Vec<u8>'s layout. All length bytes are initialized to zero; length
    // equals capacity, and this Vec takes sole ownership with matching deallocation.
    // Null and layout overflow return Overloaded before constructing a Vec.
    Ok(unsafe { Vec::from_raw_parts(pointer.as_ptr(), length, length) })
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

/// Initialize and wipe the full allocation, including truncated and spare bytes.
fn wipe_payload(bytes: &mut Vec<u8>) {
    #[cfg(test)]
    WIPED_BYTES.with(|total| total.set(total.get() + bytes.capacity()));
    bytes.clear();
    #[cfg(all(target_os = "linux", any(target_env = "gnu", target_env = "musl")))]
    if bytes.capacity() != 0 {
        // SAFETY: the exclusive Vec owns capacity writable bytes. explicit_bzero
        // initializes spare capacity and cannot be removed as a dead store.
        unsafe { libc::explicit_bzero(bytes.as_mut_ptr().cast(), bytes.capacity()) };
    }
    #[cfg(not(all(target_os = "linux", any(target_env = "gnu", target_env = "musl"))))]
    {
        use zeroize::Zeroize;
        bytes.zeroize();
    }
}

#[cfg(test)]
thread_local! {
    static WIPED_BYTES: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
}

/// Accounting, retirement, reclamation, and cross-thread release contracts.
#[cfg(test)]
mod quota_tests {
    use super::*;

    /// Pre-demand fixture: full depth and whole-class retention, so pressure
    /// tests still exercise a recycler that can hold an entire class.
    fn full_depth<P: Policy>(quotas: Quotas<P>) -> Quotas<P> {
        quotas.buffers.class_divisor.store(1, Ordering::Relaxed);
        quotas.with_buffer_demand(PoolDemand::new().with_target(BUFFER_DEPTH))
    }

    /// Reserve, check out, and return `count` concurrent 1 MiB buffers.
    fn churn(quotas: &Quotas<TestPolicy>, class: Resource, count: usize) {
        let size = 1 << 20;
        let held: Vec<_> = (0..count)
            .map(|_| {
                let charge = quotas.reserve(None, class, size).unwrap();
                let mut bytes = charge.buffer(size).unwrap();
                bytes.fill(0x5c);
                (charge, bytes)
            })
            .collect();
        for (mut charge, bytes) in held {
            charge.recycle(bytes);
        }
    }

    #[test]
    fn recycler_target_grows_under_repeated_miss_and_reject() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(64 * size, 1));
        let window = page_alloc::DEFAULT_WINDOW;
        let start = std::time::Instant::now();
        assert_eq!(quotas.maintain_buffers(start), 0);
        churn(&quotas, Resource::Payload, 6);
        let stats = quotas.buffer_pool_stats();
        assert_eq!((stats.entries, stats.target), (2, 2));
        assert_eq!((stats.misses, stats.rejects), (6, 4));
        assert_eq!(quotas.maintain_buffers(start + window), 0);
        assert_eq!(quotas.buffer_pool_stats().target, 5);
        churn(&quotas, Resource::Payload, 6);
        assert_eq!(quotas.buffer_pool_stats().entries, 5);
        for n in 2..10 {
            churn(&quotas, Resource::Payload, 12);
            quotas.maintain_buffers(start + window * n);
            assert!(quotas.buffer_pool_stats().target <= BUFFER_DEPTH);
        }
        assert_eq!(quotas.buffer_pool_stats().target, BUFFER_DEPTH);
        assert!(quotas.buffer_pool_stats().hits > 0);
    }

    #[test]
    fn recycler_trims_idle_entries_and_returns_admission() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(64 * size, 1))
            .with_buffer_demand(PoolDemand::new().with_target(4));
        let window = page_alloc::DEFAULT_WINDOW;
        let start = std::time::Instant::now();
        churn(&quotas, Resource::Payload, 4);
        quotas.maintain_buffers(start);
        assert_eq!(quotas.used(Resource::Payload), 4 * size);
        let oldest = quotas.buffers.lock().unwrap()[2].0.as_ptr();
        assert_eq!(quotas.maintain_buffers(start + window), 2 * size);
        assert_eq!(quotas.used(Resource::Payload), 2 * size);
        assert_eq!(quotas.buffers.lock().unwrap()[0].0.as_ptr(), oldest);
        let mut now = start + window;
        for _ in 0..40 {
            now += window;
            quotas.maintain_buffers(now);
        }
        assert_eq!(quotas.used(Resource::Payload), 0);
        let stats = quotas.buffer_pool_stats();
        assert_eq!((stats.entries, stats.bytes, stats.trimmed), (0, 0, 4));
        assert!(stats.target < 4);
    }

    #[test]
    fn recycler_trim_frees_only_erased_secret_bytes() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(64 * size, 1));
        churn(&quotas, Resource::Payload, 2);
        assert!(
            quotas
                .buffers
                .lock()
                .unwrap()
                .iter()
                .all(|(bytes, _)| bytes.len() == size && bytes.iter().all(|&b| b == 0))
        );
        let start = std::time::Instant::now();
        quotas.maintain_buffers(start);
        quotas.maintain_buffers(start + page_alloc::DEFAULT_WINDOW);
        quotas.maintain_buffers(start + page_alloc::DEFAULT_WINDOW * 2);
        assert_eq!(quotas.retained_buffer_bytes(), 0);
        assert_eq!(quotas.used(Resource::Payload), 0);
    }

    #[test]
    fn constrained_recycler_releases_and_refuses_returns() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(64 * size, 1));
        churn(&quotas, Resource::Payload, 2);
        quotas.constrain_buffers(true);
        let mut charge = quotas.reserve(None, Resource::Progress, size).unwrap();
        charge.recycle(vec![1; size]);
        drop(charge);
        assert_eq!(quotas.used(Resource::Progress), 0);
        assert_eq!(quotas.maintain_buffers(std::time::Instant::now()), 2 * size);
        assert_eq!(quotas.used(Resource::Payload), 0);
        assert!(quotas.buffer_pool_stats().constrained);
        quotas.constrain_buffers(false);
        churn(&quotas, Resource::Payload, 1);
        assert_eq!(quotas.retained_buffer_bytes(), size);
    }

    #[test]
    fn contended_recycler_maintenance_is_skipped() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(64 * size, 1));
        churn(&quotas, Resource::Payload, 2);
        let start = std::time::Instant::now();
        quotas.maintain_buffers(start);
        let guard = quotas.buffers.lock().unwrap();
        assert_eq!(
            quotas.maintain_buffers(start + page_alloc::DEFAULT_WINDOW),
            0
        );
        drop(guard);
        assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
        assert_eq!(
            quotas.maintain_buffers(start + page_alloc::DEFAULT_WINDOW),
            size
        );
    }

    #[test]
    fn recycler_retains_at_most_half_of_a_class_limit() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(5 * size, 1))
            .with_buffer_demand(PoolDemand::new().with_target(BUFFER_DEPTH));
        churn(&quotas, Resource::Payload, 4);
        assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
        for _ in 0..3 {
            let mut charge = quotas.reserve(None, Resource::Progress, size).unwrap();
            charge.recycle(vec![1; size]);
        }
        assert_eq!(quotas.retained_buffer_bytes(), 4 * size);
        assert!(quotas.buffer_pool_stats().rejects >= 2);
    }

    #[test]
    fn shared_handles_report_pool_stats_without_retaining_buffers() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(64 * size, 1));
        let shared = quotas.shared();
        churn(&quotas, Resource::Payload, 1);
        let stats = shared.buffer_pool_stats();
        assert_eq!((stats.entries, stats.bytes), (1, size));
        assert_eq!(stats, quotas.buffer_pool_stats());
        drop(quotas);
        assert_eq!(shared.buffer_pool_stats(), PoolStats::default());
        assert_eq!(shared.used(Resource::Payload), 0);
    }

    #[test]
    fn rejected_recycle_erases_secrets_but_public_bytes_take_fast_path() {
        let size = 1 << 20;
        for stopped in [false, true] {
            for class in [Resource::Payload, Resource::Progress] {
                let quotas = Quotas::new(TestPolicy::new(32 * size, 1));
                if stopped {
                    quotas.stop();
                } else {
                    for _ in 0..BUFFER_DEPTH {
                        let mut charge = quotas.reserve(None, class, size).unwrap();
                        charge.recycle(vec![0xa5; size]);
                    }
                }
                let mut charge = quotas.reserve_completion(None, class, size).unwrap();
                let before = WIPED_BYTES.with(|total| total.get());
                charge.recycle(vec![0xa5; size]);
                let erased = WIPED_BYTES.with(|total| total.get()) - before;
                assert_eq!(erased, if class.requires_wipe() { size } else { 0 });
                assert_eq!(charge.amount(), size);
            }
        }
    }

    #[test]
    fn small_payloads_stay_exact_and_cross_class_reuse_is_zeroed() {
        let quotas = Quotas::new(TestPolicy::new(16 << 20, 2));
        let charge = quotas.reserve(None, Resource::Payload, 1 << 20).unwrap();
        let small = charge.buffer(3).unwrap();
        assert_eq!(small.len(), 3);
        assert_eq!(small.capacity(), 3);
        drop(charge);
        let size = (1 << 20) + 16;
        let mut charge = quotas.reserve(None, Resource::Progress, size).unwrap();
        let mut bytes = charge.buffer(size).unwrap();
        let pointer = bytes.as_ptr();
        bytes.fill(0xa5);
        charge.recycle(bytes);
        let charge = quotas.reserve(None, Resource::Payload, size).unwrap();
        let bytes = charge.buffer(size - 16).unwrap();
        assert_eq!(bytes.as_ptr(), pointer);
        assert_eq!(bytes.len(), size - 16);
        assert!(bytes.iter().all(|&b| b == 0));
        assert_eq!(quotas.used(Resource::Progress), 0);
        assert_eq!(quotas.used(Resource::Payload), size);
    }

    #[test]
    fn zeroed_payload_handles_empty_overflow_and_allocator_failure() {
        assert!(zeroed_payload(0).unwrap().is_empty());
        assert_eq!(zeroed_payload(usize::MAX), Err(Error::Overloaded));
        assert_eq!(
            unsafe { zeroed_payload_with(16, |_| std::ptr::null_mut()) },
            Err(Error::Overloaded)
        );
        let bytes = zeroed_payload(2 * 1024 * 1024 + 1).unwrap();
        assert_eq!(bytes.len(), bytes.capacity());
        assert!(bytes.iter().all(|&b| b == 0));
        let quotas = Quotas::new(TestPolicy::new(usize::MAX, 1));
        let charge = quotas.reserve(None, Resource::Payload, usize::MAX).unwrap();
        assert_eq!(charge.buffer(usize::MAX), Err(Error::Overloaded));
    }

    #[test]
    fn recycler_reuses_size_class_without_destroying_mismatches() {
        let size = (1 << 20) + 123;
        let capacity = page_alloc::buffer_capacity(size).unwrap();
        let quotas = Quotas::new(TestPolicy::new(16 << 20, 4));
        let mut charge = quotas.reserve(None, Resource::Payload, capacity).unwrap();
        let mut bytes = charge.buffer(size).unwrap();
        let pointer = bytes.as_ptr();
        bytes.fill(0xa5);
        charge.recycle(bytes);
        let other = quotas.reserve(None, Resource::Payload, 2 << 20).unwrap();
        let mismatch = other.buffer(2 << 20).unwrap();
        assert_eq!(quotas.retained_buffer_bytes(), capacity);
        let charge = quotas.reserve(None, Resource::Payload, capacity).unwrap();
        let bytes = charge.buffer(size + 17).unwrap();
        assert_eq!(bytes.as_ptr(), pointer);
        assert_eq!(bytes.len(), size + 17);
        assert!(bytes.iter().all(|&b| b == 0));
        assert_eq!(quotas.retained_buffer_bytes(), 0);
        drop(mismatch);
    }

    #[test]
    fn recycler_depth_and_byte_budget_bound_differing_sizes() {
        for step in [64 * 1024, 8 * 1024 * 1024] {
            let quotas = full_depth(Quotas::new(TestPolicy::new(1024 << 20, 1)));
            for n in 1..=16 {
                let size = (1 << 20) + n * step;
                let mut charge = quotas.reserve(None, Resource::Payload, size).unwrap();
                charge.recycle(zeroed_payload(size).unwrap());
                assert!(quotas.retained_buffer_bytes() <= BUFFER_BUDGET);
                assert!(quotas.buffers.lock().unwrap().len() <= BUFFER_DEPTH);
            }
            if step == 64 * 1024 {
                assert_eq!(quotas.buffers.lock().unwrap().len(), BUFFER_DEPTH);
            }
        }
    }

    #[test]
    fn pressure_reclaims_only_contended_class_key_and_needed_size() {
        let size = 1 << 20;
        let quotas = full_depth(Quotas::new(TestPolicy::new(8 * size, 4)));
        let a = "a".to_owned();
        let b = "b".to_owned();
        for (key, class, length) in [
            (&a, Resource::Payload, size),
            (&a, Resource::Payload, 2 * size),
            (&b, Resource::Payload, size),
            (&a, Resource::Other, size),
        ] {
            let mut charge = quotas.reserve(Some(key), class, length).unwrap();
            charge.recycle(zeroed_payload(length).unwrap());
        }
        let held = quotas
            .reserve(Some(&a), Resource::Payload, 2 * size)
            .unwrap();
        assert_eq!(quotas.retained_buffer_bytes(), 3 * size);
        assert_eq!(quotas.used(Resource::Other), size);
        let charge = quotas.reserve(Some(&b), Resource::Payload, size).unwrap();
        let bytes = charge.buffer(size).unwrap();
        assert!(bytes.iter().all(|&b| b == 0));
        assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
        drop(held);
    }

    #[test]
    fn public_recycle_skips_wipe_but_secret_recycle_erases_full_capacity() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(8 * size, 2));
        for class in [Resource::Payload, Resource::Progress] {
            let mut charge = quotas.reserve(None, class, size).unwrap();
            let mut bytes = charge.buffer(size).unwrap();
            bytes.fill(0xa5);
            charge.recycle(bytes);
        }
        {
            let pool = quotas.buffers.lock().unwrap();
            for (bytes, charge) in pool.iter() {
                let expected = if charge.class.requires_wipe() {
                    0
                } else {
                    0xa5
                };
                assert!(bytes.iter().all(|&b| b == expected));
            }
        }
        let charge = quotas.reserve(None, Resource::Progress, size).unwrap();
        assert!(charge.buffer(size).unwrap().iter().all(|&b| b == 0));
        quotas.stop();
        let mut charge = quotas
            .reserve_completion(None, Resource::Progress, size)
            .unwrap();
        let mut bytes = vec![0xa5; size];
        charge.try_recycle(&mut bytes);
        assert!(bytes.iter().all(|&b| b == 0xa5));
        assert_eq!(charge.amount(), size);
        assert_eq!(quotas.retained_buffer_bytes(), 0);
    }

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

    /// Wiping initializes every allocated byte without moving or resizing backing.
    #[test]
    fn secure_payload_wipe_initializes_spare_capacity_and_preserves_geometry() {
        for capacity in [0, 1, 15, 16, 17, 63, 64, 65, 4095, 4096, 4097] {
            for initialized in [false, true] {
                let mut bytes = Vec::with_capacity(capacity);
                if initialized {
                    bytes.resize(bytes.capacity(), 0xa7);
                    bytes.truncate(capacity / 2);
                }
                let pointer = bytes.as_ptr();
                let allocated = bytes.capacity();
                wipe_payload(&mut bytes);
                assert!(bytes.is_empty());
                assert_eq!(bytes.capacity(), allocated);
                assert_eq!(bytes.as_ptr(), pointer);
                // SAFETY: wipe_payload initializes every byte of the allocation.
                unsafe { bytes.set_len(allocated) };
                assert!(bytes.iter().all(|byte| *byte == 0));
                wipe_payload(&mut bytes);
                assert!(bytes.is_empty());
            }
        }
    }

    /// Compare full-capacity secure wiping on the same alternating workload.
    #[test]
    #[ignore = "release-only alternating full-capacity secure wipe comparison"]
    #[allow(clippy::assertions_on_constants)]
    fn secure_payload_wipe_benchmark() {
        use std::{hint::black_box, time::Instant};
        use zeroize::Zeroize;
        assert!(!cfg!(debug_assertions), "run with --release");
        const ITERATIONS: usize = 128;
        for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
            let mut bytes = vec![0u8; length];
            for sample in 0..6 {
                for optimized in if sample % 2 == 0 {
                    [false, true]
                } else {
                    [true, false]
                } {
                    let start = Instant::now();
                    for _ in 0..ITERATIONS {
                        bytes.fill(black_box(0xa7));
                        black_box(&bytes);
                        if optimized {
                            wipe_payload(&mut bytes);
                        } else {
                            bytes.clear();
                            bytes.zeroize();
                        }
                        // SAFETY: both primitives initialize the full capacity.
                        unsafe { bytes.set_len(length) };
                        black_box(&bytes);
                    }
                    let elapsed = start.elapsed();
                    assert!(bytes.iter().all(|byte| *byte == 0));
                    if sample != 0 {
                        println!(
                            "secure_wipe length={length} optimized={optimized} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                            elapsed.as_nanos() as f64 / ITERATIONS as f64
                        );
                    }
                }
            }
        }
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
            assert_eq!(
                rejected.len(),
                7,
                "local retries once; shared does not retry"
            );
            for (index, rejection) in rejected.iter().enumerate() {
                let (key_used, key_limit) = if matches!(index, 3 | 4) {
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
        assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 7);
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
                    assert!(matches!(
                        quotas.policy.rejected.borrow().as_slice(),
                        [
                            Rejection::Keys { used: 2, limit: 2 },
                            Rejection::Keys { used: 2, limit: 2 }
                        ]
                    ));
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

    /// Idle backing retains concurrent charges and reports pressure before retry.
    #[test]
    fn recycler_retains_two_live_charges_and_reports_pressure_before_retry() {
        let size = 1 << 20;
        let quotas = full_depth(Quotas::new(TestPolicy::new(3 * size, 4)));
        let mut buffers: Vec<_> = (0..3)
            .map(|_| {
                let charge = quotas.reserve(None, Resource::Payload, size).unwrap();
                let bytes = charge.buffer(size).unwrap();
                (charge, bytes)
            })
            .collect();
        for (mut charge, mut bytes) in buffers.drain(..) {
            bytes.fill(0xa7);
            bytes.truncate(1);
            charge.recycle(bytes);
        }
        assert_eq!(quotas.buffers.lock().unwrap().len(), 3);
        assert_eq!(quotas.retained_buffer_bytes(), 3 * size);
        assert_eq!(quotas.used(Resource::Payload), 3 * size);
        assert!(
            quotas
                .buffers
                .lock()
                .unwrap()
                .iter()
                .all(|(b, _)| b.len() == size && b.iter().all(|v| *v == 0))
        );
        let shared = quotas.shared();
        let other = quotas.reserve(None, Resource::Other, 3 * size).unwrap();
        assert!(matches!(
            quotas.reserve(None, Resource::Other, 1),
            Err(Error::Overloaded)
        ));
        assert_eq!(quotas.retained_buffer_bytes(), 3 * size);
        assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 2);
        let charge = quotas.reserve(None, Resource::Payload, 3 * size).unwrap();
        assert_eq!(quotas.retained_buffer_bytes(), 0);
        assert_eq!(quotas.used(Resource::Payload), 3 * size);
        assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 3);
        assert!(
            matches!(quotas.policy.rejected.lock().unwrap()[2], Rejection::Resource { used, requested, .. } if used == 3 * size && requested == 3 * size)
        );
        let mut charge = charge;
        charge.recycle(vec![0xa7; 3 * size]);
        drop((charge, other, quotas));
        assert_eq!(
            shared.used(Resource::Payload),
            0,
            "usage handles must not retain recycled buffers"
        );
    }

    /// Key cloning runs unlocked, and callback changes are checked before retention.
    #[test]
    fn recycler_key_clone_runs_before_retention_checks() {
        thread_local! {
            static CLONE_HOOK: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        }

        #[derive(Eq, PartialEq, Hash)]
        struct Key;

        impl Clone for Key {
            fn clone(&self) -> Self {
                let hook = CLONE_HOOK.with(|slot| slot.borrow_mut().take());
                if let Some(hook) = hook {
                    hook();
                }
                Self
            }
        }

        struct ClonePolicy;

        impl Policy for ClonePolicy {
            type Class = Resource;
            type Key = Key;

            fn limit(&self, _: Resource) -> usize {
                10 << 20
            }

            fn max_keys(&self) -> usize {
                1
            }

            fn wakes(_: Resource) -> bool {
                false
            }

            fn covers(_: Resource) -> bool {
                false
            }

            fn rejected(&self, _: Rejection<Resource>) {
                panic!("unexpected rejection");
            }
        }

        let size = 1 << 20;
        for action in ["retain", "stop", "fill"] {
            let quotas = Rc::new(full_depth(Quotas::new(ClonePolicy)));
            let mut idle = quotas.reserve(None, Resource::Payload, size).unwrap();
            idle.recycle(vec![0xa7; size]);
            let mut donor = quotas
                .reserve(Some(&Key), Resource::Payload, 2 * size)
                .unwrap();
            let nested = quotas.clone();
            CLONE_HOOK.with(|slot| {
                *slot.borrow_mut() = Some(Box::new(move || {
                    // Fail before reentry can block on the same mutex.
                    assert!(
                        nested.buffers.try_lock().is_ok(),
                        "key cloned under recycler lock"
                    );
                    match action {
                        "stop" => nested.stop(),
                        "fill" => {
                            for _ in 1..BUFFER_DEPTH {
                                let mut extra =
                                    nested.reserve(None, Resource::Payload, size).unwrap();
                                extra.recycle(vec![0xa7; size]);
                            }
                        }
                        _ => {}
                    }
                }));
            });

            donor.recycle(vec![0xa7; size]);
            assert!(CLONE_HOOK.with(|slot| slot.borrow().is_none()));
            assert_eq!(quotas.is_stopped(), action == "stop");
            let retained = match action {
                "retain" => 3 * size,
                "fill" => BUFFER_DEPTH * size,
                _ => 0,
            };
            let owned = if action == "retain" { 0 } else { 2 * size };
            assert_eq!(donor.amount(), owned);
            assert!(donor.key().is_some());
            assert_eq!(
                donor
                    .local
                    .as_ref()
                    .unwrap()
                    .counter(Resource::Payload)
                    .used(),
                2 * size
            );
            assert_eq!(quotas.retained_buffer_bytes(), retained);
            assert_eq!(quotas.used(Resource::Payload), retained + owned);
            drop(donor);
            quotas.reclaim_buffers();
            assert_eq!(quotas.used(Resource::Payload), 0);
            assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
        }
    }

    /// Only the final shared owner recycles, wiping spare capacity and retaining its charge.
    #[test]
    fn charged_bytes_recycles_once_after_last_owner_and_preserves_small_release() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(2 * size, 4));
        let reservation = quotas.reserve(None, Resource::Payload, size).unwrap();
        let mut bytes = reservation.buffer(size).unwrap();
        bytes.fill(0xa7);
        bytes.truncate(1);
        let owner = Arc::new(ChargedBytes { bytes, reservation });
        let retained = owner.clone();
        drop(owner);
        assert_eq!(quotas.retained_buffer_bytes(), 0);
        assert_eq!(quotas.used(Resource::Payload), size);
        std::thread::spawn(move || drop(retained)).join().unwrap();
        assert_eq!(quotas.retained_buffer_bytes(), size);
        assert_eq!(quotas.used(Resource::Payload), size);
        assert!(
            quotas.buffers.lock().unwrap()[0]
                .0
                .iter()
                .all(|byte| *byte == 0)
        );
        quotas.reclaim_buffers();
        assert_eq!(quotas.used(Resource::Payload), 0);
        let small = ChargedBytes {
            bytes: vec![7; 3],
            reservation: quotas.reserve(None, Resource::Payload, 3).unwrap(),
        };
        drop(small);
        assert_eq!(quotas.used(Resource::Payload), 0);
        assert_eq!(quotas.retained_buffer_bytes(), 0);
    }

    /// Key-table pressure reports original facts before successful reclamation.
    #[test]
    fn key_record_rejection_is_reported_before_reclaim_retry() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(2 * size, 1));
        let (a, b) = ("a".to_owned(), "b".to_owned());
        let mut old = quotas.reserve(Some(&a), Resource::Payload, size).unwrap();
        old.recycle(vec![0xa7; size]);
        drop(old);
        let charge = quotas.reserve(Some(&b), Resource::Other, 1).unwrap();
        assert_eq!(quotas.retained_buffer_bytes(), 0);
        assert!(!quotas.keys.borrow().contains_key(&a));
        assert!(matches!(
            &quotas.policy.rejected.lock().unwrap()[..],
            [Rejection::Keys { used: 1, limit: 1 }]
        ));
        drop(charge);
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
