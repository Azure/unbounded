//! Local keyed fairness with thread-safe release, unkeyed admission, and recycling.
use crate::{Error, Result};
use std::{
    cell::RefCell,
    collections::{HashMap, VecDeque},
    hash::Hash,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

/// A dense, stable index in `0..COUNT`, unique to each resource class.
pub trait Class: Copy + Send + Sync + 'static {
    const COUNT: usize;
    fn index(self) -> usize;
}

/// Application policy. Charges do not retain this value, so suitability and
/// release notifications are static properties of the class.
pub trait Policy: 'static {
    type Class: Class;
    type Key: Clone + Eq + Hash + Send + Sync + 'static;
    fn limit(&self, class: Self::Class) -> usize;
    fn floor(&self, _class: Self::Class) -> usize {
        1
    }
    fn max_keys(&self) -> usize;
    fn wakes(class: Self::Class) -> bool;
    fn allows_stopped(_class: Self::Class) -> bool {
        false
    }
    fn covers(class: Self::Class) -> bool;
    fn rejected(&self, rejection: Rejection<Self::Class>);
}

#[derive(Clone, Copy, Debug)]
pub enum Rejection<C> {
    Keys {
        used: usize,
        limit: usize,
    },
    Resource {
        class: C,
        used: usize,
        limit: usize,
        requested: usize,
        key_used: Option<usize>,
        key_limit: Option<usize>,
    },
}

#[repr(align(64))]
struct Counter(AtomicUsize);
struct Counters<K> {
    active: Option<Arc<AtomicUsize>>,
    retired: Option<(K, Arc<Mutex<VecDeque<K>>>)>,
    used: Box<[Counter]>,
    wake: futures::task::AtomicWaker,
}
impl<K> Counters<K> {
    fn new(classes: usize) -> Self {
        Self {
            active: None,
            retired: None,
            used: (0..classes).map(|_| Counter(AtomicUsize::new(0))).collect(),
            wake: futures::task::AtomicWaker::new(),
        }
    }
    fn counter<C: Class>(&self, class: C) -> &AtomicUsize {
        &self.used[class.index()].0
    }
}
impl<K> Drop for Counters<K> {
    fn drop(&mut self) {
        if let Some(active) = &self.active {
            active.fetch_sub(1, Ordering::AcqRel);
        }
        if let Some((key, queue)) = self.retired.take() {
            queue
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .push_back(key);
        }
    }
}

type Buffers<P> = Mutex<Vec<(Vec<u8>, Charge<P>)>>;
type Keys<K> = RefCell<HashMap<K, Weak<Counters<K>>>>;

pub struct Quotas<P: Policy> {
    policy: P,
    totals: Arc<Counters<P::Key>>,
    keys: Keys<P::Key>,
    active_keys: Arc<AtomicUsize>,
    retired_keys: Arc<Mutex<VecDeque<P::Key>>>,
    stopped: Arc<AtomicBool>,
    buffers: Arc<Buffers<P>>,
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
    fn clone(&self) -> Self {
        Self {
            policy: self.policy.clone(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
        }
    }
}
impl<P: Policy> SharedQuotas<P> {
    pub fn policy(&self) -> &P {
        &self.policy
    }
    pub fn register(&self, waker: &std::task::Waker) {
        self.totals.wake.register(waker);
    }
    pub fn used(&self, class: P::Class) -> usize {
        self.totals.counter(class).load(Ordering::Acquire)
    }
    pub fn limit(&self, class: P::Class) -> usize {
        self.policy.limit(class)
    }
    pub fn is_stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire)
    }
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
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(amount).filter(|next| *next <= limit)
            })
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
        Ok(Charge {
            class,
            amount,
            key: None,
            totals: self.totals.clone(),
            local: None,
            buffers: Weak::new(),
            stopped: self.stopped.clone(),
        })
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

impl<P: Policy> Charge<P> {
    pub fn amount(&self) -> usize {
        self.amount
    }
    pub fn class(&self) -> P::Class {
        self.class
    }
    pub fn key(&self) -> Option<&P::Key> {
        self.key.as_ref()
    }
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
        self.amount -= amount;
        Ok(Self {
            class: self.class,
            amount,
            key: self.key.clone(),
            totals: self.totals.clone(),
            local: self.local.clone(),
            buffers: self.buffers.clone(),
            stopped: self.stopped.clone(),
        })
    }
    /// Caller must retain at least the capacity of every live backing allocation.
    pub fn shrink(&mut self, amount: usize) -> Result<()> {
        if amount == 0 || amount > self.amount {
            return Err(Error::InvalidInput);
        }
        let released = self.amount - amount;
        self.amount = amount;
        self.totals
            .counter(self.class)
            .fetch_sub(released, Ordering::AcqRel);
        if let Some(local) = &self.local {
            local
                .counter(self.class)
                .fetch_sub(released, Ordering::AcqRel);
        }
        Ok(())
    }
    pub fn buffer(&self, length: usize) -> Result<Vec<u8>> {
        if length > self.amount {
            return Err(Error::InvalidInput);
        }
        if let Some(pool) = self.buffers.upgrade() {
            let mut pool = pool.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(index) = pool
                .iter()
                .position(|(bytes, _)| bytes.capacity() == length)
            {
                let (bytes, old) = pool.swap_remove(index);
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
        let Ok(mut pool) = pool.try_lock() else {
            return;
        };
        if self.stopped.load(Ordering::Acquire) || pool.len() >= 2 {
            return;
        }
        // SAFETY: wipe_payload initialized the entire capacity above.
        unsafe { bytes.set_len(bytes.capacity()) };
        let charge = Self {
            class: self.class,
            amount: std::mem::take(&mut self.amount),
            key: self.key.clone(),
            totals: self.totals.clone(),
            local: self.local.clone(),
            buffers: Weak::new(),
            stopped: self.stopped.clone(),
        };
        pool.push((bytes, charge));
    }
}
impl<P: Policy> Drop for Charge<P> {
    fn drop(&mut self) {
        self.totals
            .counter(self.class)
            .fetch_sub(self.amount, Ordering::AcqRel);
        if let Some(local) = &self.local {
            local
                .counter(self.class)
                .fetch_sub(self.amount, Ordering::AcqRel);
        }
        if P::wakes(self.class) {
            self.totals.wake.wake();
        }
    }
}

impl<P: Policy> Quotas<P> {
    pub fn new(policy: P) -> Self {
        Self {
            policy,
            totals: Arc::new(Counters::new(P::Class::COUNT)),
            keys: RefCell::default(),
            active_keys: Arc::new(AtomicUsize::new(0)),
            retired_keys: Arc::new(Mutex::new(VecDeque::new())),
            stopped: Arc::new(AtomicBool::new(false)),
            buffers: Arc::new(Mutex::new(Vec::new())),
        }
    }
    pub fn policy(&self) -> &P {
        &self.policy
    }
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
    pub fn used(&self, class: P::Class) -> usize {
        self.totals.counter(class).load(Ordering::Acquire)
    }
    pub fn limit(&self, class: P::Class) -> usize {
        self.policy.limit(class)
    }
    pub fn owns(&self, charge: &Charge<P>) -> bool {
        Arc::ptr_eq(&self.totals, &charge.totals)
    }
    pub fn is_stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire)
    }
    pub fn stop(&self) {
        self.stopped.store(true, Ordering::Release);
        self.reclaim_buffers();
        self.totals.wake.wake();
    }
    fn fair_limit(&self, class: P::Class, active: usize) -> usize {
        let limit = self.limit(class);
        (limit / active.max(1))
            .max(self.policy.floor(class))
            .min(limit)
    }
    /// A keyed fair-share deficit must be reclaimed from that key. Global
    /// pressure can use any idle key; impossible requests have no byte remedy.
    pub fn reclamation(
        &self,
        key: &P::Key,
        class: P::Class,
        amount: usize,
    ) -> Option<(Option<P::Key>, usize)> {
        let keys = self.keys.borrow();
        let local = keys.get(key).and_then(Weak::upgrade);
        let fair = self.fair_limit(
            class,
            self.active_keys.load(Ordering::Acquire) + usize::from(local.is_none()),
        );
        if amount > fair {
            return None;
        }
        let local_deficit = local
            .as_ref()
            .map_or(0, |local| local.counter(class).load(Ordering::Acquire))
            .saturating_sub(fair - amount);
        if local_deficit != 0 {
            return Some((Some(key.clone()), local_deficit));
        }
        let deficit = self.used(class).saturating_sub(self.limit(class) - amount);
        (deficit != 0).then_some((None, deficit))
    }
    pub fn reserve(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> Result<Charge<P>> {
        self.reserve_reclaiming(key, class, amount, false)
    }
    /// For already-admitted work during drain only. Aggregate limits still apply.
    pub fn reserve_completion(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
    ) -> Result<Charge<P>> {
        self.reserve_reclaiming(key, class, amount, true)
    }
    pub fn reserve_reclaiming(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        completing: bool,
    ) -> Result<Charge<P>> {
        let result = self.reserve_inner(key, class, amount, completing);
        if matches!(result, Err(Error::Overloaded)) {
            self.reclaim_buffers_for(key, class);
            return self.reserve_inner(key, class, amount, completing);
        }
        result
    }
    pub fn retained_buffer_bytes(&self) -> usize {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .map(|(_, charge)| charge.amount())
            .sum()
    }
    pub fn reclaim_buffers(&self) {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clear();
    }
    fn reclaim_buffers_for(&self, key: Option<&P::Key>, class: P::Class) {
        let mut buffers = self.buffers.lock().unwrap_or_else(|e| e.into_inner());
        // Keyed admission can exhaust records or fair shares too. Unkeyed
        // admission only reclaims when this class has an idle charge.
        if key.is_some()
            || buffers
                .iter()
                .any(|(_, charge)| charge.class.index() == class.index())
        {
            buffers.clear();
        }
    }
    fn reserve_inner(
        &self,
        key: Option<&P::Key>,
        class: P::Class,
        amount: usize,
        completing: bool,
    ) -> Result<Charge<P>> {
        if self.is_stopped() && !completing && !P::allows_stopped(class) {
            return Err(Error::Unavailable);
        }
        if amount == 0 {
            return Err(Error::InvalidInput);
        }
        let limit = self.limit(class);
        let local = if let Some(key) = key {
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
                self.policy.rejected(Rejection::Keys {
                    used: keys.len(),
                    limit: self.policy.max_keys(),
                });
                return Err(Error::Overloaded);
            }
            Some(
                if let Some(counts) = keys.get(key).and_then(Weak::upgrade) {
                    counts
                } else {
                    let mut counts = Counters::new(P::Class::COUNT);
                    counts.active = Some(self.active_keys.clone());
                    counts.retired = Some((key.clone(), self.retired_keys.clone()));
                    self.active_keys.fetch_add(1, Ordering::AcqRel);
                    let counts = Arc::new(counts);
                    keys.insert(key.clone(), Arc::downgrade(&counts));
                    counts
                },
            )
        } else {
            None
        };
        if let Some(local) = local.as_ref().filter(|_| !completing) {
            let fair = self.fair_limit(class, self.active_keys.load(Ordering::Acquire).max(1));
            let used = local.counter(class).load(Ordering::Acquire);
            if used.checked_add(amount).is_none_or(|next| next > fair) {
                self.rejected(class, amount, Some(used), Some(fair));
                return Err(Error::Overloaded);
            }
        }
        self.totals
            .counter(class)
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(amount).filter(|next| *next <= limit)
            })
            .map_err(|_| {
                self.rejected(class, amount, None, None);
                Error::Overloaded
            })?;
        if let Some(local) = &local {
            local.counter(class).fetch_add(amount, Ordering::AcqRel);
        }
        Ok(Charge {
            class,
            amount,
            key: key.cloned(),
            totals: self.totals.clone(),
            local,
            buffers: Arc::downgrade(&self.buffers),
            stopped: self.stopped.clone(),
        })
    }
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

#[cfg(test)]
mod tests;
