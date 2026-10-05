//! Fixed metrics and bounded diagnostics without application policy.
//!
//! Callers own metric names, label cardinality, resource policy, record payloads,
//! and request lifetimes. This crate provides no dynamic metric registration and
//! does not interpret application identities or errors.
//!
//! [`Metrics`] offers cache-line-isolated writer shards and relaxed observations;
//! [`Ring`] retains bounded records; [`SampleBudget`] serializes sampling admission.
//! [`health`] shares lifecycle observations, and [`server`] serves diagnostics on
//! a caller-polled reactor rather than creating an executor or background thread.

use std::{
    sync::{Arc, Mutex, TryLockError},
    time::{Duration, Instant},
};

pub mod server;

pub use metrics::{Lease, Metric, Metrics};

/// Fixed writer shards for caller-defined diagnostic snapshots.
/// Each read clones one shard under its lock; aggregation never holds a writer lock.
pub struct SnapshotShards<T>(Arc<[Mutex<T>]>);

impl<T> Clone for SnapshotShards<T> {
    /// Share the fixed shard collection without copying snapshots.
    fn clone(&self) -> Self {
        Self(self.0.clone())
    }
}

impl<T: Default + Clone> SnapshotShards<T> {
    /// Allocate a fixed number of default-valued shards, including zero if desired.
    pub fn new(count: usize) -> Self {
        Self((0..count).map(|_| Mutex::new(T::default())).collect())
    }

    /// Replace one writer's complete observation; panic on an invalid shard index.
    pub fn replace(&self, shard: usize, value: T) {
        *self.0[shard].lock().unwrap_or_else(|e| e.into_inner()) = value;
    }

    /// Copy each observation independently, recovering retained poisoned state.
    pub fn snapshots(&self) -> impl ExactSizeIterator<Item = T> + '_ {
        self.0
            .iter()
            .map(|shard| shard.lock().unwrap_or_else(|e| e.into_inner()).clone())
    }
}

/// Shared sampling admission and bounded retention under one lock.
/// Callers decide eligibility before acquisition and own the record schema.
pub struct Sampler<T, const N: usize>(Arc<Mutex<SamplerState<T, N>>>);

/// Admission and publication are serialized so snapshots see matching totals.
struct SamplerState<T, const N: usize> {
    budget: SampleBudget,

    entries: Ring<Arc<T>, N>,
}

/// Exclusive sample work ownership. Drop releases admission, never refunds it.
/// Retained records and reader snapshots may outlive this lease.
pub struct SampleLease<T, const N: usize>(Sampler<T, N>);

impl<T, const N: usize> Clone for Sampler<T, N> {
    /// Share admission and retention without copying records or limits.
    fn clone(&self) -> Self {
        Self(self.0.clone())
    }
}

impl<T, const N: usize> Sampler<T, N> {
    /// Create an empty sampler with caller-selected limits and positive retention.
    pub fn new(limits: SampleLimits) -> Self {
        Self(Arc::new(Mutex::new(SamplerState {
            budget: SampleBudget::new(limits),
            entries: Ring::default(),
        })))
    }

    /// Admit and publish one record. The constructor runs under the sampler lock
    /// and must not reenter this sampler. Rejected attempts do not construct data.
    pub fn acquire(
        &self,
        now: Instant,
        record: impl FnOnce(u64) -> T,
    ) -> Option<(Arc<T>, SampleLease<T, N>)> {
        let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
        let sequence = state.budget.acquire(now)?;
        let record = Arc::new(record(sequence));
        state.entries.push(record.clone());
        Some((record, SampleLease(self.clone())))
    }

    /// Copy counters without changing admission or retention.
    pub fn counts(&self) -> SampleCounts {
        self.0
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .budget
            .counts()
    }

    /// Clone retention and its matching counts, releasing the lock before formatting.
    pub fn snapshot(&self) -> (Ring<Arc<T>, N>, SampleCounts) {
        let state = self.0.lock().unwrap_or_else(|e| e.into_inner());
        (state.entries.clone(), state.budget.counts())
    }
}

impl<T, const N: usize> Drop for SampleLease<T, N> {
    /// Release the single busy slot even when sampled work is abandoned.
    fn drop(&mut self) {
        self.0
            .0
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .budget
            .release();
    }
}

/// Shared bounded diagnostics with snapshots that never expose the writer lock.
///
/// Cloning shares the writer; snapshotting clones retention under the lock.
/// Poisoned locks retain records rather than discarding diagnostic evidence.
/// Record-level synchronization remains the caller's responsibility: an `Arc`
/// snapshot retains the same records, not frozen copies of their mutable contents.
pub struct SharedRing<T, const N: usize>(Arc<Mutex<Ring<T, N>>>);

impl<T, const N: usize> Clone for SharedRing<T, N> {
    /// Share the existing writer without cloning any records.
    fn clone(&self) -> Self {
        Self(self.0.clone())
    }
}

impl<T, const N: usize> Default for SharedRing<T, N> {
    /// Create an empty shared ring; panic if capacity is zero.
    fn default() -> Self {
        Self(Arc::new(Mutex::new(Ring::default())))
    }
}

impl<T, const N: usize> SharedRing<T, N> {
    /// Append a record, recovering retained state if a writer poisoned the lock.
    pub fn push(&self, value: T) {
        self.0.lock().unwrap_or_else(|e| e.into_inner()).push(value);
    }
}

impl<T: Clone, const N: usize> SharedRing<T, N> {
    /// Clone retention and release the lock before returning for formatting.
    pub fn snapshot(&self) -> Ring<T, N> {
        self.0.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// Snapshot without waiting for the writer lock; return `None` on contention.
    /// Poisoning is recovered just as in [`Self::snapshot`]. The returned ring
    /// holds no lock, so callers cannot accidentally format under the writer lock.
    pub fn try_snapshot(&self) -> Option<Ring<T, N>> {
        let ring = match self.0.try_lock() {
            Ok(ring) => ring,
            Err(TryLockError::Poisoned(error)) => error.into_inner(),
            Err(TryLockError::WouldBlock) => return None,
        };
        Some(ring.clone())
    }
}

/// Retain the newest `N` records with saturating, one-based sequence numbers.
///
/// Capacity must be positive. Iteration is oldest first even after sequence
/// saturation. Overwrite releases only the ring's ownership of an evicted record.
/// For caller-synchronized rings, clone under the caller's lock and format after
/// releasing it; use [`SharedRing`] when no compound observation is needed.
#[derive(Clone)]
pub struct Ring<T, const N: usize> {
    entries: [Option<(u64, T)>; N],

    total: u64,

    next: usize,

    len: usize,
}

impl<T, const N: usize> Default for Ring<T, N> {
    /// Create empty retention; panic if capacity is zero.
    fn default() -> Self {
        assert!(N > 0, "ring capacity must be positive");
        Self {
            entries: std::array::from_fn(|_| None),
            total: 0,
            next: 0,
            len: 0,
        }
    }
}

impl<T, const N: usize> Ring<T, N> {
    /// Append a record, evicting the oldest when retention is full.
    pub fn push(&mut self, value: T) {
        self.total = self.total.saturating_add(1);
        self.entries[self.next] = Some((self.total, value));
        self.next = (self.next + 1) % N;
        self.len = (self.len + 1).min(N);
    }

    /// Return the saturating number of records ever appended.
    pub fn total(&self) -> u64 {
        self.total
    }

    /// Return the number of retained records.
    pub fn len(&self) -> usize {
        self.len
    }

    /// Report whether retention is empty.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// Borrow retained records oldest first without cloning their values.
    pub fn iter_refs(&self) -> impl ExactSizeIterator<Item = (u64, &T)> + '_ {
        (0..self.len).map(|offset| {
            let index = (self.next + N - self.len + offset) % N;
            let (sequence, value) = self.entries[index].as_ref().expect("retained ring entry");
            (*sequence, value)
        })
    }
}

impl<T: Copy, const N: usize> Ring<T, N> {
    /// Copy retained records oldest first, even after sequence saturation.
    pub fn iter(&self) -> impl ExactSizeIterator<Item = (u64, T)> + '_ {
        self.iter_refs().map(|(sequence, value)| (sequence, *value))
    }
}

/// Caller-owned bounds for a single sampling window.
#[derive(Clone, Copy, Debug)]
pub struct SampleLimits {
    /// Maximum admissions; zero disables sampling.
    pub total: u64,

    /// Exclusive window length from the first eligible attempt.
    pub duration: Duration,

    /// Minimum elapsed time between successful admissions, inclusive.
    pub interval: Duration,
}

/// Saturating sampling totals and the current ownership observation.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct SampleCounts {
    /// Every eligible attempt, including those rejected by a bound.
    pub eligible: u64,

    /// Successful admissions, including work later abandoned.
    pub sampled: u64,

    /// Eligible attempts rejected by any admission bound.
    pub skipped: u64,

    /// Whether a successful admission still owns the sampling slot.
    pub busy: bool,
}

/// Serialized, single-owner sampling admission with caller-provided time.
///
/// Protect this state with the same lock as the caller's sample publication.
/// The first eligible attempt starts the window, including rejected attempts.
/// This budget is deliberately not cloneable: copies could admit extra owners.
pub struct SampleBudget {
    limits: SampleLimits,

    first: Option<Instant>,

    last: Option<Instant>,

    counts: SampleCounts,
}

impl SampleBudget {
    /// Create an unused window with the supplied limits.
    pub fn new(limits: SampleLimits) -> Self {
        Self {
            limits,
            first: None,
            last: None,
            counts: SampleCounts::default(),
        }
    }

    /// Admit an eligible attempt, returning its one-based sequence number.
    /// The interval boundary is inclusive; the total-duration boundary is not.
    /// Backward time saturates to zero elapsed time rather than panicking.
    pub fn acquire(&mut self, now: Instant) -> Option<u64> {
        self.counts.eligible = self.counts.eligible.saturating_add(1);
        let first = *self.first.get_or_insert(now);
        if self.counts.busy
            || self.counts.sampled >= self.limits.total
            || now.saturating_duration_since(first) >= self.limits.duration
            || self
                .last
                .is_some_and(|last| now.saturating_duration_since(last) < self.limits.interval)
        {
            self.counts.skipped = self.counts.skipped.saturating_add(1);
            return None;
        }
        self.counts.busy = true;
        self.last = Some(now);
        self.counts.sampled += 1;
        Some(self.counts.sampled)
    }

    /// Release the active owner, including on abandoned work. Does not refund a
    /// sample or reset the interval/window. Call only for a successful acquire.
    pub fn release(&mut self) {
        self.counts.busy = false;
    }

    /// Copy the counters without changing admission or ownership.
    pub fn counts(&self) -> SampleCounts {
        self.counts
    }
}

/// Fixed metric schemas, cache-line-isolated counters, and shared gauges.
///
/// Counter updates and aggregation saturate; gauge increases wrap. All accesses
/// use relaxed atomics: a scrape observes individual series, not an atomic
/// registry snapshot. No synchronization of application data is implied.
pub mod metrics {
    use std::{
        fmt,
        marker::PhantomData,
        sync::{
            Arc,
            atomic::{AtomicU64, Ordering},
        },
    };

    /// A fixed, dense metric enumeration. `ALL` must contain each metric exactly
    /// once in index order, starting at zero. Prefer [`crate::metrics!`], which
    /// generates the enum and its matching indices together.
    pub trait Metric: Copy + 'static {
        /// Every metric in index order.
        const ALL: &'static [Self];

        /// Return this metric's zero-based registry index.
        fn index(self) -> usize;

        /// Return its trusted Prometheus identifier.
        fn name(self) -> &'static str;
    }

    /// Clones retain their writer shard; reads aggregate all retained shards.
    /// Counter blocks isolate shards without padding every event. Each shared
    /// gauge occupies its own cache line.
    #[derive(Clone)]
    pub struct Metrics<C: Metric, G: Metric> {
        registry: Arc<Registry>,

        shard: usize,

        kinds: PhantomData<fn() -> (C, G)>,
    }

    /// A single gauge unit, released on drop even after all writer handles exit.
    /// This owner cannot be cloned, so one acquisition has exactly one release.
    #[must_use = "dropping the lease immediately releases its gauge unit"]
    pub struct Lease {
        registry: Arc<Registry>,

        index: usize,
    }

    /// A fixed gauge schema with one numeric label. Names must be trusted valid
    /// Prometheus identifiers; dynamic text labels and registration are not
    /// accepted. The caller owns cardinality and observes values at scrape time.
    pub struct LabeledGauges<const N: usize> {
        names: [&'static str; N],

        label: &'static str,
    }

    impl<C: Metric, G: Metric> Metrics<C, G> {
        /// Allocate a fixed registry. Panics if `n` is zero or capacity overflows.
        pub fn shards(n: usize) -> Vec<Self> {
            assert!(n >= 1, "at least one metrics shard is required");
            let stride = C::ALL.len().div_ceil(8);
            let registry = Arc::new(Registry {
                counters: (0..stride.checked_mul(n).expect("metrics capacity overflow"))
                    .map(|_| CounterBlock::default())
                    .collect(),
                stride,
                shards: n,
                gauges: (0..G::ALL.len()).map(|_| Gauge::default()).collect(),
            });
            (0..n)
                .map(|shard| Self {
                    registry: registry.clone(),
                    shard,
                    kinds: PhantomData,
                })
                .collect()
        }

        /// Saturate instead of wrapping a long-lived counter.
        pub fn add(&self, metric: C, amount: u64) {
            let _ = self.counter(self.shard, metric).fetch_update(
                Ordering::Relaxed,
                Ordering::Relaxed,
                |old| Some(old.saturating_add(amount)),
            );
        }

        /// Sum all writer shards, saturating on overflow.
        pub fn count(&self, metric: C) -> u64 {
            (0..self.registry.shards).fold(0u64, |total, shard| {
                total.saturating_add(self.counter(shard, metric).load(Ordering::Relaxed))
            })
        }

        /// Observe a shared gauge with a relaxed load.
        pub fn gauge(&self, metric: G) -> u64 {
            self.registry.gauges[metric.index()]
                .0
                .load(Ordering::Relaxed)
        }

        /// Replace a shared gauge; do not replace values with outstanding leases.
        pub fn set(&self, metric: G, value: u64) {
            self.registry.gauges[metric.index()]
                .0
                .store(value, Ordering::Relaxed);
        }

        /// Add with the wrapping semantics of atomic `fetch_add`.
        pub fn increase(&self, metric: G, value: u64) {
            self.registry.gauges[metric.index()]
                .0
                .fetch_add(value, Ordering::Relaxed);
        }

        /// Increment a gauge unless full; release exactly one unit when dropped.
        /// Do not replace a leased gauge's value while leases are outstanding.
        pub fn lease(&self, metric: G) -> Option<Lease> {
            let index = metric.index();
            self.registry.gauges[index]
                .0
                .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |old| {
                    old.checked_add(1)
                })
                .ok()?;
            Some(Lease {
                registry: self.registry.clone(),
                index,
            })
        }

        /// Stream relaxed per-series observations, not a coherent worker snapshot.
        /// Writes directly to the caller's sink and propagates capacity errors.
        pub fn write_prometheus(&self, out: &mut impl fmt::Write) -> fmt::Result {
            for &metric in C::ALL {
                writeln!(
                    out,
                    "# TYPE {} counter\n{} {}",
                    metric.name(),
                    metric.name(),
                    self.count(metric)
                )?;
            }
            for &metric in G::ALL {
                writeln!(
                    out,
                    "# TYPE {} gauge\n{} {}",
                    metric.name(),
                    metric.name(),
                    self.gauge(metric)
                )?;
            }
            Ok(())
        }

        /// Locate a counter inside its writer's cache-line blocks.
        fn counter(&self, shard: usize, metric: C) -> &AtomicU64 {
            let index = metric.index();
            &self.registry.counters[shard * self.registry.stride + index / 8].0[index % 8]
        }
    }

    impl Drop for Lease {
        /// Release this acquisition against its retained registry.
        fn drop(&mut self) {
            self.registry.gauges[self.index]
                .0
                .fetch_sub(1, Ordering::Relaxed);
        }
    }

    impl<const N: usize> LabeledGauges<N> {
        /// Define a fixed schema using trusted metric and label identifiers.
        pub const fn new(names: [&'static str; N], label: &'static str) -> Self {
            Self { names, label }
        }

        /// Emit type declarations once, only when the caller has installed sources.
        pub fn write_types(&self, out: &mut impl fmt::Write) -> fmt::Result {
            for name in self.names {
                writeln!(out, "# TYPE {name} gauge")?;
            }
            Ok(())
        }

        /// Stream a source's samples without allocating an intermediate string.
        pub fn write_sample(
            &self,
            out: &mut impl fmt::Write,
            label: u64,
            values: [u64; N],
        ) -> fmt::Result {
            for (name, value) in self.names.into_iter().zip(values) {
                writeln!(out, "{name}{{{}=\"{label}\"}} {value}", self.label)?;
            }
            Ok(())
        }
    }

    /// Declare a fixed metric enum, its ordered array, and its count.
    ///
    /// ```
    /// telemetry::metrics! { Event, EVENTS, EVENT_COUNT;
    ///     Self::Request => "requests_total",
    /// }
    /// assert_eq!(Event::ALL, &EVENTS);
    /// assert_eq!(Event::Request.name(), "requests_total");
    /// ```
    #[macro_export]
    macro_rules! metrics {
        ($kind:ident, $all:ident, $count:ident; $(Self::$variant:ident => $name:literal,)*) => {
            #[doc = "Fixed metric identifiers in registry index order."]
            #[derive(Clone, Copy, Debug, Eq, PartialEq)]
            #[repr(usize)]
            pub enum $kind { $(#[doc = $name] $variant,)* }

            /// Number of metrics in this schema.
            pub const $count: usize = [$($name,)*].len();

            /// Every metric in registry index order.
            pub const $all: [$kind; $count] = [$($kind::$variant,)*];

            impl $kind {
                /// Every metric in registry index order.
                pub const ALL: &'static [Self] = &$all;

                /// Return the fixed Prometheus identifier.
                pub fn name(self) -> &'static str {
                    match self { $(Self::$variant => $name,)* }
                }
            }

            impl $crate::Metric for $kind {
                const ALL: &'static [Self] = Self::ALL;

                /// Return the index enforced by the enum's declaration order.
                fn index(self) -> usize { self as usize }

                /// Return the fixed Prometheus identifier.
                fn name(self) -> &'static str { self.name() }
            }
        };
    }

    /// Eight counters fill one cache line without per-event padding.
    #[repr(align(64))]
    #[derive(Default)]
    struct CounterBlock([AtomicU64; 8]);

    /// Isolate a shared gauge from unrelated atomic updates.
    #[repr(align(64))]
    #[derive(Default)]
    struct Gauge(AtomicU64);

    /// Fixed allocation retained by both writer handles and gauge leases.
    struct Registry {
        counters: Box<[CounterBlock]>,

        stride: usize,

        shards: usize,

        gauges: Box<[Gauge]>,
    }

    /// Metric layout, arithmetic, lifetime, and streaming contracts.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Exercise ordered numeric labels, maximum values, and empty schemas.
        #[test]
        fn numeric_labeled_gauges_preserve_order_maxima_and_empty_schema() {
            let gauges = LabeledGauges::new(["used", "limit"], "shard");
            let mut out = String::new();
            gauges.write_types(&mut out).unwrap();
            gauges.write_sample(&mut out, 0, [0, u64::MAX]).unwrap();
            gauges.write_sample(&mut out, u64::MAX, [1, 2]).unwrap();
            assert_eq!(
                out,
                "# TYPE used gauge\n# TYPE limit gauge\nused{shard=\"0\"} 0\nlimit{shard=\"0\"} 18446744073709551615\nused{shard=\"18446744073709551615\"} 1\nlimit{shard=\"18446744073709551615\"} 2\n"
            );
            out.clear();
            let empty = LabeledGauges::<0>::new([], "shard");
            empty.write_types(&mut out).unwrap();
            empty.write_sample(&mut out, 1, []).unwrap();
            assert!(out.is_empty());
        }

        /// A sink that rejects every write without consuming the metric schema.
        struct Full;

        impl fmt::Write for Full {
            /// Report exhausted output capacity.
            fn write_str(&mut self, _: &str) -> fmt::Result {
                Err(fmt::Error)
            }
        }

        /// A failed scrape leaves the schema reusable.
        #[test]
        fn labeled_gauge_output_errors_propagate_without_consuming_schema() {
            let gauges = LabeledGauges::new(["used"], "shard");
            assert!(gauges.write_types(&mut Full).is_err());
            assert!(gauges.write_sample(&mut Full, 1, [2]).is_err());
            let mut out = String::new();
            gauges.write_sample(&mut out, 1, [2]).unwrap();
            assert_eq!(out, "used{shard=\"1\"} 2\n");
        }

        crate::metrics! { Event, EVENTS, EVENT_COUNT;
            Self::A => "a_total", Self::B => "b_total", Self::C => "c_total",
            Self::D => "d_total", Self::E => "e_total", Self::F => "f_total",
            Self::G => "g_total", Self::H => "h_total", Self::I => "i_total",
        }

        crate::metrics! { Level, LEVELS, LEVEL_COUNT;
            Self::Active => "active", Self::Other => "other",
        }

        /// Two counter cache lines and two independently aligned gauges.
        type TestMetrics = Metrics<Event, Level>;

        /// Generated indices and names match the complete Prometheus wire output.
        #[test]
        fn macro_indices_names_and_prometheus_errors() {
            for (index, event) in Event::ALL.iter().enumerate() {
                assert_eq!(event.index(), index);
                assert_eq!(Metric::name(*event), event.name());
            }
            assert_eq!(EVENT_COUNT, 9);
            assert_eq!(LEVEL_COUNT, 2);
            let metrics = TestMetrics::shards(1).pop().unwrap();
            metrics.add(Event::A, 3);
            metrics.set(Level::Active, 7);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            let expected: String = EVENTS
                .into_iter()
                .map(|e| {
                    format!(
                        "# TYPE {} counter\n{} {}\n",
                        e.name(),
                        e.name(),
                        if e == Event::A { 3 } else { 0 }
                    )
                })
                .chain(LEVELS.into_iter().map(|g| {
                    format!(
                        "# TYPE {} gauge\n{} {}\n",
                        g.name(),
                        g.name(),
                        if g == Level::Active { 7 } else { 0 }
                    )
                }))
                .collect();
            assert_eq!(output, expected);
            assert!(metrics.write_prometheus(&mut Full).is_err());
        }

        /// Reject registries without writers.
        #[test]
        #[should_panic(expected = "at least one metrics shard")]
        fn zero_shards_are_rejected() {
            TestMetrics::shards(0);
        }

        /// Check actual allocation alignment and clone-to-writer mapping.
        #[test]
        fn fixed_registry_is_aligned_and_clones_keep_their_writer() {
            let workers = TestMetrics::shards(3);
            assert_eq!(std::mem::align_of::<CounterBlock>(), 64);
            assert_eq!(std::mem::size_of::<CounterBlock>(), 64);
            assert_eq!(std::mem::size_of::<Gauge>(), 64);
            assert_eq!(workers[0].registry.stride, 2);
            for (index, worker) in workers.iter().enumerate() {
                let clone = worker.clone();
                assert_eq!(clone.shard, index);
                assert!(Arc::ptr_eq(&clone.registry, &workers[0].registry));
                for event in EVENTS {
                    clone.add(event, (index + 1) as u64);
                    assert_eq!(
                        worker.counter(index, event).load(Ordering::Relaxed),
                        (index + 1) as u64
                    );
                }
                assert_eq!(worker.counter(index, Event::A) as *const _ as usize % 64, 0);
            }
            for gauge in &workers[0].registry.gauges {
                assert_eq!(std::ptr::from_ref(gauge) as usize % 64, 0);
            }
            assert_eq!(workers[0].count(Event::I), 6);
        }

        /// Distinguish saturating counters, wrapping gauges, and checked leases.
        #[test]
        fn saturation_and_gauge_wrapping_and_lease_lifetime() {
            let workers = TestMetrics::shards(2);
            workers[0].add(Event::A, u64::MAX - 1);
            workers[1].add(Event::A, 2);
            assert_eq!(workers[0].count(Event::A), u64::MAX);
            workers[0].add(Event::A, 9);
            assert_eq!(
                workers[0].counter(0, Event::A).load(Ordering::Relaxed),
                u64::MAX
            );
            workers[0].set(Level::Active, u64::MAX - 1);
            let lease = workers[1].lease(Level::Active).unwrap();
            assert!(workers[0].lease(Level::Active).is_none());
            std::thread::spawn(move || drop(lease)).join().unwrap();
            assert_eq!(workers[0].gauge(Level::Active), u64::MAX - 1);
            workers[1].increase(Level::Active, 3);
            assert_eq!(workers[0].gauge(Level::Active), 1);
            let lease = workers[0].lease(Level::Active).unwrap();
            let registry = lease.registry.clone();
            drop(workers);
            drop(lease);
            assert_eq!(registry.gauges[0].0.load(Ordering::Relaxed), 1);
        }

        /// Shared-shard writers may race scrapes without losing updates or leases.
        #[test]
        fn concurrent_shared_shard_writers_and_scrapes() {
            let workers = TestMetrics::shards(2);
            let reader = workers[0].clone();
            let barrier = Arc::new(std::sync::Barrier::new(5));
            std::thread::scope(|scope| {
                for worker in workers.iter().chain(workers.iter()) {
                    let worker = worker.clone();
                    let barrier = barrier.clone();
                    scope.spawn(move || {
                        barrier.wait();
                        for _ in 0..1000 {
                            let _lease = worker.lease(Level::Active).unwrap();
                            worker.add(Event::I, 1);
                        }
                    });
                }
                barrier.wait();
                let mut last = 0;
                for _ in 0..100 {
                    let count = reader.count(Event::I);
                    assert!((last..=4000).contains(&count));
                    last = count;
                    reader.write_prometheus(&mut String::new()).unwrap();
                }
            });
            drop(workers);
            assert_eq!(reader.count(Event::I), 4000);
            assert_eq!(reader.gauge(Level::Active), 0);
        }
    }
}

/// Shared lifecycle observations and fixed probe responses without resource policy.
pub mod health {
    use crate::server::Response;
    use std::{
        sync::{Arc, Mutex},
        time::Instant,
    };

    /// Service lifecycle; usability can temporarily degrade a ready observation.
    #[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
    pub enum State {
        /// Initial lifecycle before the caller declares readiness.
        #[default]
        Starting,

        /// Ready when the caller's latest resource observation is usable.
        Ready,

        /// Not ready, either explicitly or due to an unusable observation.
        Degraded,

        /// Shutting down; only draining or stopped transitions remain legal.
        Draining,

        /// Final lifecycle; no transition to another state is permitted.
        Stopped,
    }

    /// A rejected transition or a poisoned lifecycle lock.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Unavailable;

    impl std::fmt::Display for Unavailable {
        /// Format a fixed message without exposing resource details.
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str("health unavailable")
        }
    }

    impl std::error::Error for Unavailable {}

    /// Clones share lifecycle and the latest resource observation. Poisoned locks
    /// fail closed. Usability is caller-defined and evaluated under the same lock.
    pub struct Health<R>(Arc<Mutex<Status<R>>>);

    /// Caller-named probe bodies; routing and readiness policy remain outside.
    pub struct Probe {
        success: &'static str,

        failure: &'static str,
    }

    impl<R> Clone for Health<R> {
        /// Share observations without requiring resources to be cloneable.
        fn clone(&self) -> Self {
            Self(self.0.clone())
        }
    }

    impl<R: Default> Default for Health<R> {
        /// Start with the caller's default resource observation.
        fn default() -> Self {
            Self(Arc::new(Mutex::new(Status {
                lifecycle: State::Starting,
                resources: R::default(),
            })))
        }
    }

    impl<R> Health<R> {
        /// Unusable Ready observations report Degraded without changing the
        /// stored lifecycle. Later usable observations can report Ready again.
        pub fn state_at(
            &self,
            now: Instant,
            usable: impl FnOnce(&R, Instant) -> bool,
        ) -> Result<State, Unavailable> {
            let status = self.0.lock().map_err(|_| Unavailable)?;
            Ok(match status.lifecycle {
                State::Ready if !usable(&status.resources, now) => State::Degraded,
                state => state,
            })
        }

        /// Replace the last resource observation without changing lifecycle.
        pub fn observe(&self, resources: R) -> Result<(), Unavailable> {
            self.0.lock().map_err(|_| Unavailable)?.resources = resources;
            Ok(())
        }

        /// Stopped is absorbing; Draining permits only Draining or Stopped. Ready
        /// additionally requires a usable observation. The callback may obtain
        /// the current clock under the lock and is not called for other states.
        pub fn transition(
            &self,
            state: State,
            usable: impl FnOnce(&R) -> bool,
        ) -> Result<(), Unavailable> {
            let mut status = self.0.lock().map_err(|_| Unavailable)?;
            if status.lifecycle == State::Stopped && state != State::Stopped
                || status.lifecycle == State::Draining
                    && !matches!(state, State::Draining | State::Stopped)
                || state == State::Ready && !usable(&status.resources)
            {
                return Err(Unavailable);
            }
            status.lifecycle = state;
            Ok(())
        }
    }

    impl Probe {
        /// Define trusted bodies for usable and unusable probe observations.
        pub const fn new(success: &'static str, failure: &'static str) -> Self {
            Self { success, failure }
        }

        /// Select successful text or unavailable output without allocating.
        pub fn response(&self, usable: bool) -> (Response, &'static str) {
            if usable {
                (Response::Text, self.success)
            } else {
                (Response::Unavailable, self.failure)
            }
        }
    }

    /// Lifecycle and resources are observed and updated under one lock.
    struct Status<R> {
        lifecycle: State,

        resources: R,
    }

    /// Exhaustive lifecycle policy and poisoned-lock behavior.
    #[cfg(test)]
    mod tests {
        use super::*;
        use std::{cell::Cell, time::Duration};

        const STATES: [State; 5] = [
            State::Starting,
            State::Ready,
            State::Degraded,
            State::Draining,
            State::Stopped,
        ];

        /// Every transition preserves state and callback ordering on rejection.
        #[test]
        fn complete_transition_matrix_preserves_state_on_rejection() {
            for initial in STATES {
                for target in STATES {
                    for usable in [false, true] {
                        let health = Health::<()>::default();
                        health.transition(initial, |_| true).unwrap();
                        let called = Cell::new(false);
                        let result = health.transition(target, |_| {
                            called.set(true);
                            usable
                        });
                        let lifecycle_allowed = !(initial == State::Stopped
                            && target != State::Stopped
                            || initial == State::Draining
                                && !matches!(target, State::Draining | State::Stopped));
                        let allowed = lifecycle_allowed && (target != State::Ready || usable);
                        assert_eq!(result, if allowed { Ok(()) } else { Err(Unavailable) });
                        assert_eq!(called.get(), lifecycle_allowed && target == State::Ready);
                        assert_eq!(
                            health.state_at(Instant::now(), |_, _| true).unwrap(),
                            if allowed { target } else { initial }
                        );
                    }
                }
            }
        }

        /// Expiry degrades only the observation and refresh can restore readiness.
        #[test]
        fn shared_observations_expire_inclusively_without_mutating_lifecycle() {
            let health = Health::<Option<Instant>>::default();
            let other = health.clone();
            let now = Instant::now();
            let usable = |expiry: &Option<Instant>, now| expiry.is_some_and(|expiry| now < expiry);
            assert_eq!(
                health.transition(State::Ready, |r| usable(r, now)),
                Err(Unavailable)
            );
            other.observe(Some(now + Duration::from_secs(1))).unwrap();
            health.transition(State::Ready, |r| usable(r, now)).unwrap();
            assert_eq!(other.state_at(now, usable), Ok(State::Ready));
            assert_eq!(
                other.state_at(now + Duration::from_secs(1), usable),
                Ok(State::Degraded)
            );
            other.observe(Some(now + Duration::from_secs(3))).unwrap();
            assert_eq!(
                health.state_at(now + Duration::from_secs(1), usable),
                Ok(State::Ready)
            );
            health
                .transition(State::Degraded, |_| panic!("not Ready"))
                .unwrap();
            assert_eq!(
                health.state_at(now, |_, _| panic!("not Ready")),
                Ok(State::Degraded)
            );
        }

        /// Unlike diagnostic retention, poisoned health must fail closed.
        #[test]
        fn poisoned_health_fails_closed_without_recovering_state() {
            let health = Health::<()>::default();
            let other = health.clone();
            assert!(
                std::thread::spawn(move || {
                    let _guard = other.0.lock().unwrap();
                    panic!("poison health");
                })
                .join()
                .is_err()
            );
            assert_eq!(health.observe(()), Err(Unavailable));
            assert_eq!(
                health.state_at(Instant::now(), |_, _| true),
                Err(Unavailable)
            );
            assert_eq!(
                health.transition(State::Stopped, |_| true),
                Err(Unavailable)
            );
        }

        /// Probe selection returns exact caller-owned text and response kinds.
        #[test]
        fn probes_preserve_caller_bodies_and_response_kind() {
            let probe = Probe::new("healthy\n", "unavailable\n");
            let (response, body) = probe.response(true);
            assert!(matches!(response, Response::Text));
            assert_eq!(body, "healthy\n");
            let (response, body) = probe.response(false);
            assert!(matches!(response, Response::Unavailable));
            assert_eq!(body, "unavailable\n");
            assert_eq!(Probe::new("", "").response(true).1, "");
        }
    }
}

/// Retention ownership, ordering, and nonblocking snapshot contracts.
#[cfg(test)]
mod ring_tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Snapshots detach retention from future writes and release the writer lock.
    #[test]
    fn shared_snapshots_release_lock_and_preserve_retention() {
        let ring = SharedRing::<String, 2>::default();
        let writer = ring.clone();
        assert!(ring.snapshot().is_empty());
        writer.push("first".into());
        ring.push("second".into());
        let snapshot = ring.snapshot();
        assert!(ring.try_snapshot().is_some());
        writer.push("third".into());
        assert_eq!(
            snapshot
                .iter_refs()
                .map(|(n, s)| (n, s.as_str()))
                .collect::<Vec<_>>(),
            [(1, "first"), (2, "second")]
        );
        assert_eq!(
            ring.snapshot()
                .iter_refs()
                .map(|(n, s)| (n, s.as_str()))
                .collect::<Vec<_>>(),
            [(2, "second"), (3, "third")]
        );
    }

    /// An uncontended snapshot succeeds without keeping the lock or consuming data.
    #[test]
    fn try_snapshot_copies_retention_and_releases_lock() {
        let ring = SharedRing::<u8, 2>::default();
        assert!(ring.try_snapshot().unwrap().is_empty());
        ring.push(1);
        ring.push(2);
        let snapshot = ring.try_snapshot().unwrap();
        assert_eq!(snapshot.total(), 2);
        assert_eq!(snapshot.iter().collect::<Vec<_>>(), [(1, 1), (2, 2)]);
        assert!(ring.0.try_lock().is_ok());
        ring.push(3);
        assert_eq!(snapshot.iter().collect::<Vec<_>>(), [(1, 1), (2, 2)]);
        assert_eq!(
            ring.try_snapshot().unwrap().iter().collect::<Vec<_>>(),
            [(2, 2), (3, 3)]
        );
    }

    /// A held writer lock returns no snapshot immediately, without losing records.
    #[test]
    fn try_snapshot_returns_none_while_writer_holds_lock() {
        let ring = SharedRing::<u8, 2>::default();
        ring.push(1);
        let guard = ring.0.lock().unwrap();
        assert!(ring.try_snapshot().is_none());
        drop(guard);
        assert_eq!(
            ring.try_snapshot().unwrap().iter().collect::<Vec<_>>(),
            [(1, 1)]
        );
    }

    /// Both snapshot APIs recover poison while a contended poisoned lock still fails.
    #[test]
    fn shared_ring_recovers_records_after_poisoning() {
        let ring = SharedRing::<u8, 2>::default();
        let writer = ring.clone();
        assert!(
            std::thread::spawn(move || {
                let mut guard = writer.0.lock().unwrap();
                guard.push(1);
                panic!("poison diagnostics");
            })
            .join()
            .is_err()
        );
        assert_eq!(
            ring.try_snapshot().unwrap().iter().collect::<Vec<_>>(),
            [(1, 1)]
        );
        let guard = ring.0.lock().unwrap_or_else(|error| error.into_inner());
        assert!(ring.try_snapshot().is_none());
        drop(guard);
        ring.push(2);
        assert_eq!(ring.snapshot().iter().collect::<Vec<_>>(), [(1, 1), (2, 2)]);
        assert_eq!(
            ring.try_snapshot().unwrap().iter().collect::<Vec<_>>(),
            [(1, 1), (2, 2)]
        );
    }

    /// Shared records retain external ownership and mutable contents across eviction.
    #[test]
    fn clone_handles_preserve_snapshot_and_external_owners_after_overwrite() {
        /// Count final releases independently from mutable record contents.
        struct Record {
            id: usize,

            value: AtomicUsize,

            drops: Arc<[AtomicUsize; 3]>,
        }

        impl Drop for Record {
            /// Record the final release, not individual `Arc` handle drops.
            fn drop(&mut self) {
                self.drops[self.id].fetch_add(1, Ordering::SeqCst);
            }
        }

        let drops = Arc::new(std::array::from_fn(|_| AtomicUsize::new(0)));
        let record = |id| {
            Arc::new(Record {
                id,
                value: AtomicUsize::new(id),
                drops: drops.clone(),
            })
        };
        let mut ring = Ring::<Arc<Record>, 2>::default();
        assert!(ring.is_empty());
        assert_eq!(ring.iter_refs().len(), 0);

        // External operation owners outlive eviction from diagnostic retention.
        let work = record(0);
        let ticket = work.clone();
        ring.push(work.clone());
        ring.push(record(1));
        assert_eq!(Arc::strong_count(&work), 3);
        let snapshot = ring.clone();
        assert_eq!(Arc::strong_count(&work), 4);
        let retained = {
            let mut entries = snapshot.iter_refs();
            let (sequence, retained) = entries.next().unwrap();
            assert_eq!(sequence, 1);
            assert!(Arc::ptr_eq(retained, &work));
            assert_eq!(entries.len(), 1);
            assert_eq!(Arc::strong_count(&work), 4, "iteration must not clone");
            retained
        };

        ring.push(record(2));
        assert_eq!(ring.total(), 3);
        assert_eq!(ring.len(), 2);
        assert_eq!(Arc::strong_count(&work), 3, "only ring ownership ends");
        assert_eq!(
            ring.iter_refs()
                .map(|(seq, r)| (seq, r.id))
                .collect::<Vec<_>>(),
            [(2, 1), (3, 2)]
        );
        assert_eq!(snapshot.total(), 2);
        assert_eq!(
            snapshot
                .iter_refs()
                .map(|(seq, r)| (seq, r.id))
                .collect::<Vec<_>>(),
            [(1, 0), (2, 1)]
        );

        // A handle snapshot retains objects, not frozen copies of their contents.
        work.value.store(42, Ordering::SeqCst);
        assert_eq!(ticket.value.load(Ordering::SeqCst), 42);
        assert_eq!(retained.value.load(Ordering::SeqCst), 42);
        drop(work);
        drop(ticket);
        assert_eq!(drops[0].load(Ordering::SeqCst), 0);
        drop(ring);
        assert_eq!(drops[0].load(Ordering::SeqCst), 0);
        assert_eq!(drops[1].load(Ordering::SeqCst), 0);
        assert_eq!(drops[2].load(Ordering::SeqCst), 1);
        drop(snapshot);
        for count in drops.iter() {
            assert_eq!(count.load(Ordering::SeqCst), 1);
        }
    }

    /// Eviction releases the old record immediately even when it cannot be cloned.
    #[test]
    fn overwrite_drops_the_last_owner_immediately() {
        /// A uniquely owned record with a shared final-drop counter.
        struct Record(Arc<AtomicUsize>);

        impl Drop for Record {
            /// Count this record's destruction.
            fn drop(&mut self) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }

        let drops = Arc::new(AtomicUsize::new(0));
        // The record itself is neither Copy nor Clone.
        let mut ring = Ring::<Record, 1>::default();
        ring.push(Record(drops.clone()));
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        ring.push(Record(drops.clone()));
        assert_eq!(drops.load(Ordering::SeqCst), 1);
        assert_eq!(ring.iter_refs().next().unwrap().0, 2);
        drop(ring);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    /// Retention order does not depend on distinct sequence values.
    #[test]
    fn single_entry_and_saturated_sequence_keep_insertion_order() {
        let mut one = Ring::<u8, 1>::default();
        one.push(1);
        one.push(2);
        assert_eq!(one.iter().collect::<Vec<_>>(), [(2, 2)]);
        let mut ring = Ring::<u8, 2> {
            total: u64::MAX - 1,
            ..Default::default()
        };
        for value in 1..=4 {
            ring.push(value);
        }
        assert_eq!(ring.total(), u64::MAX);
        assert_eq!(
            ring.iter().collect::<Vec<_>>(),
            [(u64::MAX, 3), (u64::MAX, 4)]
        );
    }

    /// Keep invalid capacity rejection at construction.
    #[test]
    #[should_panic(expected = "ring capacity must be positive")]
    fn zero_capacity_is_rejected() {
        let _ = Ring::<u8, 0>::default();
    }
}

/// Sampling boundaries, ownership, and saturating accounting.
#[cfg(test)]
mod sampling_tests {
    use super::*;

    /// Snapshot readers never retain writer locks or observe mutable borrowed data.
    #[test]
    fn snapshot_shards_replace_and_release_each_writer_lock() {
        let shards = SnapshotShards::<Vec<u64>>::new(2);
        shards.replace(0, vec![1]);
        shards.replace(1, vec![2]);
        let other = shards.clone();
        let mut snapshots = shards.snapshots();
        let first = snapshots.next().unwrap();
        other.replace(0, vec![3]);
        other.replace(1, vec![4]);
        assert_eq!(first, vec![1]);
        assert_eq!(snapshots.next(), Some(vec![4]));
        assert_eq!(snapshots.next(), None);
        assert_eq!(
            shards.snapshots().collect::<Vec<_>>(),
            vec![vec![3], vec![4]]
        );
        assert_eq!(SnapshotShards::<u64>::new(0).snapshots().len(), 0);
    }

    /// Admission is shared across threads, while snapshots outlive overwritten records.
    #[test]
    fn shared_sampler_releases_abandoned_work_without_refunding_admission() {
        let now = Instant::now();
        let sampler = Sampler::<u64, 1>::new(SampleLimits {
            total: 2,
            duration: Duration::from_secs(10),
            interval: Duration::ZERO,
        });
        let (first, lease) = sampler.acquire(now, |sequence| sequence).unwrap();
        let other = sampler.clone();
        assert!(
            std::thread::spawn(move || other
                .acquire(now, |_| panic!("busy constructor"))
                .is_none())
            .join()
            .unwrap()
        );
        let (snapshot, counts) = sampler.snapshot();
        assert_eq!(
            (counts.eligible, counts.sampled, counts.skipped, counts.busy),
            (2, 1, 1, true)
        );
        drop(lease);
        assert!(!sampler.counts().busy);
        let (second, lease) = sampler.acquire(now, |sequence| sequence).unwrap();
        assert_eq!((*first, *second), (1, 2));
        assert_eq!(*snapshot.iter_refs().next().unwrap().1.as_ref(), 1);
        assert_eq!(
            *sampler.snapshot().0.iter_refs().next().unwrap().1.as_ref(),
            2
        );
        drop(lease);
        assert!(
            sampler
                .acquire(now, |_| panic!("exhausted constructor"))
                .is_none()
        );
        let counts = sampler.counts();
        assert_eq!(
            (counts.eligible, counts.sampled, counts.skipped, counts.busy),
            (4, 2, 2, false)
        );
    }

    /// Independent count, window, and interval limits for boundary tests.
    fn limits() -> SampleLimits {
        SampleLimits {
            total: 3,
            duration: Duration::from_secs(10),
            interval: Duration::from_secs(2),
        }
    }

    /// Releasing an owner refunds neither the admission count nor its interval.
    #[test]
    fn owner_interval_and_total_are_independent_bounds() {
        let now = Instant::now();
        let mut budget = SampleBudget::new(limits());
        assert_eq!(budget.acquire(now), Some(1));
        assert_eq!(budget.acquire(now + Duration::from_secs(2)), None);
        budget.release();
        assert_eq!(budget.acquire(now - Duration::from_secs(1)), None);
        assert_eq!(
            budget.acquire(now + Duration::from_secs(2) - Duration::from_nanos(1)),
            None
        );
        assert_eq!(budget.acquire(now + Duration::from_secs(2)), Some(2));
        budget.release();
        assert_eq!(budget.acquire(now + Duration::from_secs(4)), Some(3));
        budget.release();
        assert_eq!(budget.acquire(now + Duration::from_secs(6)), None);
        assert_eq!(
            budget.counts(),
            SampleCounts {
                eligible: 7,
                sampled: 3,
                skipped: 4,
                busy: false
            }
        );
    }

    /// The window excludes its end and begins even when admission is disabled.
    #[test]
    fn window_expires_exactly_and_zero_limits_disable_sampling() {
        let now = Instant::now();
        let mut budget = SampleBudget::new(SampleLimits {
            interval: Duration::ZERO,
            ..limits()
        });
        assert_eq!(budget.acquire(now), Some(1));
        budget.release();
        assert_eq!(
            budget.acquire(now + Duration::from_secs(10) - Duration::from_nanos(1)),
            Some(2)
        );
        budget.release();
        assert_eq!(budget.acquire(now + Duration::from_secs(10)), None);
        for limits in [
            SampleLimits {
                total: 0,
                ..limits()
            },
            SampleLimits {
                duration: Duration::ZERO,
                ..limits()
            },
        ] {
            let mut budget = SampleBudget::new(limits);
            assert_eq!(budget.acquire(now), None);
            assert_eq!(budget.first, Some(now));
            assert_eq!(
                budget.counts(),
                SampleCounts {
                    eligible: 1,
                    skipped: 1,
                    ..SampleCounts::default()
                }
            );
        }
    }

    /// Saturated accounting still admits the final sequence only once.
    #[test]
    fn counters_saturate_without_wrapping_sequence() {
        let now = Instant::now();
        let mut budget = SampleBudget::new(SampleLimits {
            total: u64::MAX,
            interval: Duration::ZERO,
            ..limits()
        });
        budget.counts = SampleCounts {
            eligible: u64::MAX,
            sampled: u64::MAX - 1,
            skipped: u64::MAX,
            busy: false,
        };
        assert_eq!(budget.acquire(now), Some(u64::MAX));
        budget.release();
        assert_eq!(budget.acquire(now), None);
        assert_eq!(
            budget.counts(),
            SampleCounts {
                eligible: u64::MAX,
                sampled: u64::MAX,
                skipped: u64::MAX,
                busy: false
            }
        );
    }
}
