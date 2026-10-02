use std::{
    fmt,
    marker::PhantomData,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
};

/// A fixed, dense metric enumeration. `ALL` must contain each metric exactly once
/// in index order, with indices starting at zero. Prefer [`crate::metrics!`].
pub trait Metric: Copy + 'static {
    const ALL: &'static [Self];
    fn index(self) -> usize;
    fn name(self) -> &'static str;
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
        #[derive(Clone, Copy, Debug, Eq, PartialEq)]
        #[repr(usize)]
        pub enum $kind { $($variant,)* }
        pub const $count: usize = [$($name,)*].len();
        pub const $all: [$kind; $count] = [$($kind::$variant,)*];
        impl $kind {
            pub const ALL: &'static [Self] = &$all;
            pub fn name(self) -> &'static str {
                match self { $(Self::$variant => $name,)* }
            }
        }
        impl $crate::Metric for $kind {
            const ALL: &'static [Self] = Self::ALL;
            fn index(self) -> usize { self as usize }
            fn name(self) -> &'static str { self.name() }
        }
    };
}

#[repr(align(64))]
#[derive(Default)]
struct CounterBlock([AtomicU64; 8]);

#[repr(align(64))]
#[derive(Default)]
struct Gauge(AtomicU64);

struct Registry {
    // Flat cache-line blocks isolate shards without padding every event.
    counters: Box<[CounterBlock]>,
    stride: usize,
    shards: usize,
    gauges: Box<[Gauge]>,
}

/// Clones retain their writer shard. Reads aggregate all retained writer shards.
#[derive(Clone)]
pub struct Metrics<C: Metric, G: Metric> {
    registry: Arc<Registry>,
    shard: usize,
    kinds: PhantomData<fn() -> (C, G)>,
}

impl<C: Metric, G: Metric> Metrics<C, G> {
    /// Allocate a fixed registry. Panics if `n` is zero.
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

    fn counter(&self, shard: usize, metric: C) -> &AtomicU64 {
        let index = metric.index();
        &self.registry.counters[shard * self.registry.stride + index / 8].0[index % 8]
    }

    /// Saturate instead of wrapping a long-lived counter.
    pub fn add(&self, metric: C, amount: u64) {
        let _ = self.counter(self.shard, metric).fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |old| Some(old.saturating_add(amount)),
        );
    }

    pub fn count(&self, metric: C) -> u64 {
        (0..self.registry.shards).fold(0u64, |total, shard| {
            total.saturating_add(self.counter(shard, metric).load(Ordering::Relaxed))
        })
    }

    pub fn gauge(&self, metric: G) -> u64 {
        self.registry.gauges[metric.index()]
            .0
            .load(Ordering::Relaxed)
    }

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
        self.registry.gauges[metric.index()]
            .0
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |old| {
                old.checked_add(1)
            })
            .ok()?;
        Some(Lease {
            registry: self.registry.clone(),
            index: metric.index(),
        })
    }

    /// Relaxed per-series observations, not a coherent snapshot across workers.
    /// Writes directly to the caller's sink, propagating its capacity errors.
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
}

/// Owns the registry so release remains valid after all writer handles exit.
pub struct Lease {
    registry: Arc<Registry>,
    index: usize,
}

impl Drop for Lease {
    fn drop(&mut self) {
        self.registry.gauges[self.index]
            .0
            .fetch_sub(1, Ordering::Relaxed);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    crate::metrics! { Event, EVENTS, EVENT_COUNT;
        Self::A => "a_total", Self::B => "b_total", Self::C => "c_total",
        Self::D => "d_total", Self::E => "e_total", Self::F => "f_total",
        Self::G => "g_total", Self::H => "h_total", Self::I => "i_total",
    }
    crate::metrics! { Level, LEVELS, LEVEL_COUNT;
        Self::Active => "active", Self::Other => "other",
    }
    type TestMetrics = Metrics<Event, Level>;

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
        struct Full;
        impl fmt::Write for Full {
            fn write_str(&mut self, _: &str) -> fmt::Result {
                Err(fmt::Error)
            }
        }
        assert!(metrics.write_prometheus(&mut Full).is_err());
    }

    #[test]
    #[should_panic(expected = "at least one metrics shard")]
    fn zero_shards_are_rejected() {
        TestMetrics::shards(0);
    }

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

    #[test]
    fn concurrent_shared_shard_writers_and_scrapes() {
        let workers = TestMetrics::shards(2);
        let reader = workers[0].clone();
        let barrier = Arc::new(std::sync::Barrier::new(5));
        std::thread::scope(|scope| {
            for worker in workers.iter().chain(workers.iter()).cloned() {
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
