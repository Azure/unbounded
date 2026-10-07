//! Policy-driven admission, charged storage, and completion-owned flow control.
//!
//! Implement [`Class`] and [`Policy`] to supply resource limits and keyed fairness.
//! [`Quotas`] is a worker-local authority; [`SharedQuotas`] admits unkeyed work
//! across threads. Keep each [`Charge`] with its resource until completion. A
//! completion reservation bypasses stop and fair-share checks, not aggregate or
//! key-record limits. Policy can also allow selected classes after stop.
//!
//! Recycling wipes the full allocation before retaining at most two buffers of
//! at least 1 MiB. Pressure retries admission after applicable reclamation; stop
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

/// Charged kernel pipes and worker-local reuse.
mod pipe;

pub use admission::{
    Adaptive, Admitted, Circuits, Config as AdaptiveConfig, Event as AdaptiveEvent, Handoff,
    HandoffAdmission, HedgePermit, Hedges, Observer as AdaptiveObserver, Offer,
    Outcome as AdaptiveOutcome, Permit as AdaptivePermit, Probe,
};
pub use pipe::{MAX_PIPE_BYTES, PipeLease, PipePool, splice_unsupported};

use std::{
    cell::RefCell,
    collections::{HashMap, VecDeque},
    hash::Hash,
    marker::PhantomData,
    rc::Rc,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, AtomicUsize, Ordering},
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

    retired_keys: Arc<Mutex<VecDeque<P::Key>>>,

    stopped: Arc<AtomicBool>,

    buffers: Arc<Buffers<P>>,

    local: PhantomData<Rc<()>>,
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
}

impl<P: Policy> page_alloc::Charge for Charge<P> {
    /// Check page-backing suitability without transferring admission ownership.
    fn covers(&self, bytes: usize) -> bool {
        P::covers(self.class) && self.amount >= bytes
    }
}

/// An unkeyed admission and usage handle. It retains no recycler buffers.
/// It is Send + Sync when the policy is Send + Sync.
pub struct SharedQuotas<P: Policy> {
    policy: P,

    totals: Arc<Counters<P::Key>>,

    stopped: Arc<AtomicBool>,
}

impl<P: Policy + Clone> Clone for SharedQuotas<P> {
    /// Share aggregate counters and stop state without retaining recycled backing.
    fn clone(&self) -> Self {
        Self {
            policy: self.policy.clone(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
        }
    }
}

impl<P: Policy> SharedQuotas<P> {
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
        if self.is_stopped() && !P::allows_stopped(class) {
            return Err(Error::Unavailable);
        }
        if amount == 0 {
            return Err(Error::InvalidInput);
        }
        let limit = self.limit(class);
        self.totals
            .counter(class)
            .reserve(amount, limit)
            .map_err(|_| {
                self.policy.rejected(Rejection::Resource {
                    class,
                    used: self.used(class),
                    limit,
                    requested: amount,
                    key_used: None,
                    key_limit: None,
                });
                Error::Overloaded
            })?;
        let charge = Charge {
            class,
            amount,
            key: None,
            totals: self.totals.clone(),
            local: None,
            buffers: Weak::new(),
            stopped: self.stopped.clone(),
        };
        // Stop may race the policy callback or counter reservation. Dropping the
        // charge rolls back usage and applies the usual release wake policy.
        if self.is_stopped() && !P::allows_stopped(class) {
            return Err(Error::Unavailable);
        }
        Ok(charge)
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
        if let Some(pool) = self.buffers.upgrade() {
            let recycled = {
                let mut pool = pool.lock().unwrap_or_else(|e| e.into_inner());
                pool.iter()
                    .position(|(bytes, _)| bytes.capacity() == length)
                    .map(|index| pool.swap_remove(index))
            };
            if let Some((bytes, old)) = recycled {
                drop(old);
                debug_assert_eq!(bytes.len(), length);
                return Ok(bytes);
            }
        }
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(length)
            .map_err(|_| Error::Overloaded)?;
        bytes.resize(length, 0);
        Ok(bytes)
    }

    /// Wipe before every retention check. At most two >=1MiB buffers retain live
    /// charges; only the final exclusive payload owner may return an allocation.
    pub fn recycle(&mut self, mut bytes: Vec<u8>) {
        wipe_payload(&mut bytes);
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
        let mut pool = match pool.try_lock() {
            Ok(pool) => pool,
            Err(std::sync::TryLockError::WouldBlock) => return,
            Err(std::sync::TryLockError::Poisoned(error)) => error.into_inner(),
        };
        if self.stopped.load(Ordering::Acquire) || pool.len() >= 2 {
            return;
        }
        // SAFETY: wipe_payload initialized the entire capacity above.
        unsafe { bytes.set_len(bytes.capacity()) };
        let charge = self.transfer(self.amount, Weak::new(), key);
        pool.push((bytes, charge));
    }

    /// Move admission with a precloned key without touching either usage counter.
    fn transfer(&mut self, amount: usize, buffers: Weak<Buffers<P>>, key: Option<P::Key>) -> Self {
        self.amount -= amount;
        Self {
            class: self.class,
            amount,
            key,
            totals: self.totals.clone(),
            local: self.local.clone(),
            buffers,
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
            retired_keys: Arc::new(Mutex::new(VecDeque::new())),
            stopped: Arc::new(AtomicBool::new(false)),
            buffers: Arc::new(Mutex::new(Vec::new())),
            local: PhantomData,
        }
    }

    /// Borrow application policy without transferring local authority.
    pub fn policy(&self) -> &P {
        &self.policy
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
    /// pressure can use any idle key; impossible requests have no byte remedy.
    pub fn reclamation(
        &self,
        key: &P::Key,
        class: P::Class,
        amount: usize,
    ) -> Option<(Option<P::Key>, usize)> {
        // Keep the counters alive, but release the lookup borrow before callbacks.
        let local = self.keys.borrow().get(key).and_then(Weak::upgrade);
        let fair = self.fair_limit(
            class,
            self.active_keys.load(Ordering::Acquire) + usize::from(local.is_none()),
        );
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

    /// Admit nonzero keyed or aggregate-only work under ordinary policy.
    pub fn reserve(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> Result<Charge<P>> {
        self.reserve_reclaiming(key, class, amount, AdmissionMode::Ordinary)
    }

    /// For already-admitted work during drain only. Aggregate limits still apply.
    pub fn reserve_completion(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> Result<Charge<P>> {
        self.reserve_reclaiming(key, class, amount, AdmissionMode::Completion)
    }

    /// Sum admission retained by idle recycler allocations.
    pub fn retained_buffer_bytes(&self) -> usize {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .map(|(_, charge)| charge.amount())
            .sum()
    }

    /// Release every idle recycler allocation and its charge.
    pub fn reclaim_buffers(&self) {
        let reclaimed = {
            let mut buffers = self.buffers.lock().unwrap_or_else(|e| e.into_inner());
            std::mem::take(&mut *buffers)
        };
        drop(reclaimed);
    }

    /// Divide the aggregate ceiling, honoring the clipped per-key floor.
    fn fair_limit(&self, class: P::Class, active: usize) -> usize {
        let limit = self.limit(class);
        (limit / active.max(1))
            .max(self.policy.floor(class))
            .min(limit)
    }

    /// Retry overload once after reclaiming relevant retained backing.
    fn reserve_reclaiming(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        mode: AdmissionMode,
    ) -> Result<Charge<P>> {
        let result = self.reserve_inner(key, class, amount, mode);
        if matches!(result, Err(Error::Overloaded)) {
            self.reclaim_buffers_for(key, class);
            return self.reserve_inner(key, class, amount, mode);
        }
        result
    }

    /// Reclaim only when key records, fairness, or this class can benefit.
    fn reclaim_buffers_for(&self, key: Option<&P::Key>, class: P::Class) {
        let reclaimed = {
            let mut buffers = self.buffers.lock().unwrap_or_else(|e| e.into_inner());
            // Keyed admission can exhaust records or fair shares too. Unkeyed
            // admission only reclaims when this class has an idle charge.
            if key.is_some()
                || buffers
                    .iter()
                    .any(|(_, charge)| charge.class.index() == class.index())
            {
                std::mem::take(&mut *buffers)
            } else {
                Vec::new()
            }
        };
        drop(reclaimed);
    }

    /// Perform one admission attempt and report its rejection before retrying.
    fn reserve_inner(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        mode: AdmissionMode,
    ) -> Result<Charge<P>> {
        if self.is_stopped() && mode == AdmissionMode::Ordinary && !P::allows_stopped(class) {
            return Err(Error::Unavailable);
        }
        if amount == 0 {
            return Err(Error::InvalidInput);
        }
        let limit = self.limit(class);
        let local = key.map(|key| self.key_counters(key)).transpose()?;
        if let Some(local) = local.as_ref().filter(|_| mode == AdmissionMode::Ordinary) {
            let fair = self.fair_limit(class, self.active_keys.load(Ordering::Acquire).max(1));
            let used = local.counter(class).used();
            if used.checked_add(amount).is_none_or(|next| next > fair) {
                self.rejected(class, amount, Some(used), Some(fair));
                return Err(Error::Overloaded);
            }
        }
        // Application cloning may panic. Finish it before committing admission
        // so every counter increment is paired with a fully constructed owner.
        let key = key.cloned();
        self.totals
            .counter(class)
            .reserve(amount, limit)
            .map_err(|_| {
                self.rejected(class, amount, None, None);
                Error::Overloaded
            })?;
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
        };
        // Policy and key callbacks may stop admission. Drop the completed owner
        // to roll back both counters while preserving completion and drain work.
        if self.is_stopped() && mode == AdmissionMode::Ordinary && !P::allows_stopped(class) {
            return Err(Error::Unavailable);
        }
        Ok(charge)
    }

    /// Reuse a live key record or create one after bounded retirement cleanup.
    fn key_counters(&self, key: &P::Key) -> Result<Arc<Counters<P::Key>>> {
        let limit = self.policy.max_keys();
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
        if !keys.contains_key(key) && keys.len() >= limit {
            let used = keys.len();
            drop(keys);
            self.policy.rejected(Rejection::Keys { used, limit });
            return Err(Error::Overloaded);
        }
        if let Some(counts) = keys.get(key).and_then(Weak::upgrade) {
            return Ok(counts);
        }
        let counts = Arc::new(Counters::keyed(
            P::Class::COUNT,
            key.clone(),
            self.active_keys.clone(),
            self.retired_keys.clone(),
        ));
        keys.insert(key.clone(), Arc::downgrade(&counts));
        Ok(counts)
    }

    /// Report the current aggregate and optional keyed rejection facts.
    fn rejected(
        &self,
        class: P::Class,
        requested: usize,
        key_used: Option<usize>,
        key_limit: Option<usize>,
    ) {
        self.policy.rejected(Rejection::Resource {
            class,
            used: self.used(class),
            limit: self.limit(class),
            requested,
            key_used,
            key_limit,
        });
    }
}

/// Fixed initialized backing paired with its live quota charge.
pub struct ChargedBuffer<P: Policy> {
    bytes: Vec<u8>,

    charge: Option<Charge<P>>,
}

impl<P: Policy> ChargedBuffer<P> {
    /// Allocate admitted backing; the caller validates class, key, and provenance.
    pub fn new(mut charge: Charge<P>, length: usize) -> Result<Self> {
        if length == 0 {
            return Err(Error::InvalidInput);
        }
        let bytes = charge.buffer(length)?;
        // Do not box or resize again while I/O pointers are live.
        let bytes = bytes.into_boxed_slice().into_vec();
        charge.shrink(bytes.len())?;
        Ok(Self {
            bytes,
            charge: Some(charge),
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
    /// Wipe and optionally retain backing before its admission is released.
    fn drop(&mut self) {
        if let Some(charge) = &mut self.charge {
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

/// Retained zeroed backing together with the charge that still accounts for it.
type Buffers<P> = Mutex<Vec<(Vec<u8>, Charge<P>)>>;

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
        active: Arc<AtomicUsize>,
        queue: Arc<Mutex<VecDeque<K>>>,
    ) -> Self {
        let mut counters = Self::new(classes);
        active.fetch_add(1, Ordering::AcqRel);
        counters.retirement = Some(Retirement { key, active, queue });
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

    active: Arc<AtomicUsize>,

    queue: Arc<Mutex<VecDeque<K>>>,
}

impl<K> Retirement<K> {
    /// Publish retirement only after the last owner releases the key record.
    fn retire(self) {
        self.active.fetch_sub(1, Ordering::AcqRel);
        self.queue
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push_back(self.key);
    }
}

/// Initialize and wipe the full allocation, including truncated and spare bytes.
fn wipe_payload(bytes: &mut Vec<u8>) {
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

/// Accounting, retirement, reclamation, and cross-thread release contracts.
#[cfg(test)]
mod quota_tests {
    use super::*;

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
    }

    /// Optional limit samples and a shared rejection log for assertions.
    #[derive(Clone)]
    struct TestPolicy {
        limit: usize,

        max_keys: usize,

        rejected: Arc<Mutex<Vec<Rejection<Resource>>>>,

        limit_gate: Option<Arc<(std::sync::Barrier, std::sync::Barrier)>>,

        limit_samples: Arc<Mutex<VecDeque<usize>>>,
    }

    impl TestPolicy {
        /// Construct fixed aggregate limits with an empty rejection log.
        fn new(limit: usize, max_keys: usize) -> Self {
            Self {
                limit,
                max_keys,
                rejected: Arc::default(),
                limit_gate: None,
                limit_samples: Arc::default(),
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
            if let Some(gate) = &self.limit_gate {
                gate.0.wait();
                gate.1.wait();
            }
            self.limit_samples
                .lock()
                .unwrap()
                .pop_front()
                .unwrap_or(self.limit)
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

    type TestBufferPool = Arc<Mutex<Vec<(Vec<u8>, Charge<TestPolicy>)>>>;

    /// Checks recycler lock availability during a synchronous charge wake.
    struct PoolWake {
        buffers: TestBufferPool,

        unlocked: Arc<AtomicBool>,
    }

    impl std::task::Wake for PoolWake {
        /// Record whether callback reentry can acquire the recycler mutex.
        fn wake(self: Arc<Self>) {
            self.unlocked
                .store(self.buffers.try_lock().is_ok(), Ordering::SeqCst);
        }
    }

    /// Register a callback that probes the recycler mutex when the next charge drops.
    fn watch_buffer_unlock(quotas: &Quotas<TestPolicy>) -> Arc<AtomicBool> {
        let unlocked = Arc::new(AtomicBool::new(false));
        let wake = Arc::new(PoolWake {
            buffers: quotas.buffers.clone(),
            unlocked: unlocked.clone(),
        });
        quotas.shared().register(&std::task::Waker::from(wake));
        unlocked
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

    /// Idle backing retains two live charges and reports pressure before retry.
    #[test]
    fn recycler_retains_two_live_charges_and_reports_pressure_before_retry() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(3 * size, 4));
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
        assert_eq!(quotas.buffers.lock().unwrap().len(), 2);
        assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
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
        assert_eq!(quotas.retained_buffer_bytes(), 2 * size);
        assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 2);
        let charge = quotas.reserve(None, Resource::Payload, 3 * size).unwrap();
        assert_eq!(quotas.retained_buffer_bytes(), 0);
        assert_eq!(quotas.policy.rejected.lock().unwrap().len(), 3);
        assert!(
            matches!(quotas.policy.rejected.lock().unwrap()[2], Rejection::Resource { used, requested, .. } if used == 2 * size && requested == 3 * size)
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

    /// Recycled and reclaimed charges wake only after the recycler mutex is released.
    #[test]
    fn recycler_charge_wakes_after_unlock_on_take_and_reclaim() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(3 * size, 4));
        let mut charge = quotas.reserve(None, Resource::Other, size).unwrap();
        charge.recycle(vec![0xa7; size]);

        let mut owner = quotas.reserve(None, Resource::Other, size).unwrap();
        let unlocked = watch_buffer_unlock(&quotas);
        let bytes = owner.buffer(size).unwrap();
        assert!(unlocked.load(Ordering::SeqCst));
        owner.recycle(bytes);

        let unlocked = watch_buffer_unlock(&quotas);
        quotas.reclaim_buffers();
        assert!(unlocked.load(Ordering::SeqCst));
        assert_eq!(quotas.retained_buffer_bytes(), 0);
    }

    /// Keyed pressure reclamation drops retained charges outside the pool lock.
    #[test]
    fn pressure_reclamation_drops_charges_after_unlock() {
        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(size, 4));
        let mut charge = quotas.reserve(None, Resource::Other, size).unwrap();
        charge.recycle(vec![0xa7; size]);

        let unlocked = watch_buffer_unlock(&quotas);
        let key = "reclaim".to_owned();
        assert!(quotas.reserve(Some(&key), Resource::Other, 1).is_ok());
        assert!(unlocked.load(Ordering::SeqCst));
        assert_eq!(quotas.retained_buffer_bytes(), 0);
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
                4 << 20
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
            let quotas = Rc::new(Quotas::new(ClonePolicy));
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
                            let mut extra = nested.reserve(None, Resource::Payload, size).unwrap();
                            extra.recycle(vec![0xa7; size]);
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
                "fill" => 2 * size,
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

    /// A poisoned recycler remains usable after a prior operation panicked.
    #[test]
    fn recycler_recovers_poisoned_mutex() {
        use std::panic::{AssertUnwindSafe, catch_unwind};

        let size = 1 << 20;
        let quotas = Quotas::new(TestPolicy::new(2 * size, 4));
        let buffers = quotas.buffers.clone();
        assert!(
            catch_unwind(AssertUnwindSafe(|| {
                let _buffers = buffers.lock().unwrap();
                panic!("poison recycler mutex");
            }))
            .is_err()
        );

        let mut charge = quotas.reserve(None, Resource::Other, size).unwrap();
        charge.recycle(vec![0xa7; size]);
        assert_eq!(quotas.retained_buffer_bytes(), size);
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

    /// Key-table rejection callbacks can inspect and admit work for a live key.
    #[test]
    fn key_record_rejection_allows_keyed_reentry() {
        /// Keep a weak link to the authority and guard callback reentry.
        struct ReentrantPolicy {
            quotas: RefCell<std::rc::Weak<Quotas<Self>>>,
            entered: std::cell::Cell<bool>,
            rejected: RefCell<Vec<Rejection<Resource>>>,
        }

        impl Policy for ReentrantPolicy {
            type Class = Resource;
            type Key = String;

            /// Leave room for nested admission under the existing key.
            fn limit(&self, _: Resource) -> usize {
                10
            }

            /// Force a rejection for each new key while the first is live.
            fn max_keys(&self) -> usize {
                1
            }

            /// This test does not register admission waiters.
            fn wakes(_: Resource) -> bool {
                false
            }

            /// This test does not allocate page backing.
            fn covers(_: Resource) -> bool {
                false
            }

            /// Reenter once and keep all rejection facts for assertions.
            fn rejected(&self, rejection: Rejection<Resource>) {
                self.rejected.borrow_mut().push(rejection);
                if self.entered.replace(true) {
                    return;
                }
                let quotas = self.quotas.borrow().upgrade().unwrap();
                let key = "live".to_owned();
                assert_eq!(
                    quotas.reclamation(&key, Resource::Payload, 10),
                    Some((Some(key.clone()), 1))
                );
                let nested = quotas.reserve(Some(&key), Resource::Payload, 2).unwrap();
                assert_eq!(quotas.used(Resource::Payload), 3);
                drop(nested);
                assert_eq!(quotas.used(Resource::Payload), 1);
            }
        }

        let quotas = Rc::new(Quotas::new(ReentrantPolicy {
            quotas: RefCell::default(),
            entered: std::cell::Cell::new(false),
            rejected: RefCell::default(),
        }));
        *quotas.policy.quotas.borrow_mut() = Rc::downgrade(&quotas);
        let (live, other) = ("live".to_owned(), "other".to_owned());
        let charge = quotas.reserve(Some(&live), Resource::Payload, 1).unwrap();
        assert!(matches!(
            quotas.reserve(Some(&other), Resource::Payload, 1),
            Err(Error::Overloaded)
        ));
        assert!(quotas.policy.entered.get());
        assert!(matches!(
            &quotas.policy.rejected.borrow()[..],
            [
                Rejection::Keys { used: 1, limit: 1 },
                Rejection::Keys { used: 1, limit: 1 }
            ]
        ));
        assert_eq!(quotas.used(Resource::Payload), 1);
        assert_eq!(quotas.active_keys.load(Ordering::Acquire), 1);
        drop(charge);
        let replacement = quotas.reserve(Some(&other), Resource::Payload, 1).unwrap();
        assert!(!quotas.keys.borrow().contains_key(&live));
        drop(replacement);
        assert_eq!(quotas.used(Resource::Payload), 0);
        assert_eq!(quotas.active_keys.load(Ordering::Acquire), 0);
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

    /// Stop during the limit callback rolls back only ordinary admission.
    #[test]
    fn shared_reservation_rechecks_stop_after_limit_callback() {
        for class in [Resource::Payload, Resource::Other, Resource::Progress] {
            let mut quotas = Quotas::new(TestPolicy::new(10, 1));
            let existing = quotas.reserve(None, class, 3).unwrap();
            let gate = Arc::new((std::sync::Barrier::new(2), std::sync::Barrier::new(2)));
            quotas.policy.limit_gate = Some(gate.clone());
            let shared = quotas.shared();
            let reservation = std::thread::spawn(move || shared.reserve(class, 7));

            gate.0.wait();
            assert_eq!(quotas.used(class), 3);
            quotas.stop();
            gate.1.wait();

            let result = reservation.join().unwrap();
            if TestPolicy::allows_stopped(class) {
                let charge = result.unwrap();
                assert_eq!(charge.amount(), 7);
                assert_eq!(quotas.used(class), 10);
                drop(charge);
            } else {
                assert!(matches!(result, Err(Error::Unavailable)));
            }
            assert_eq!(quotas.used(class), 3);
            assert!(quotas.policy.rejected.lock().unwrap().is_empty());
            drop(existing);
            assert_eq!(quotas.used(class), 0);
        }
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
