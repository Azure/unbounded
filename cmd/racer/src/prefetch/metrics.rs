//! `racer_prefetch_*` metrics. Labels are only `priority` and, for finished
//! tickets, `outcome`, so cardinality is fixed.

use super::Priority;
use std::fmt::Write;
use std::time::Duration;

/// Monotonic per-priority counters.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Counter {
    RequestedBytes,
    AlignedBytes,
    AlreadyResidentBytes,
    FetchedBytes,
    JoinedBytes,
    AdmittedBytes,
    FailedBytes,
    DemandJoins,
    Retries,
    Deferrals,
    WindowUpdates,
    WindowDroppedBytes,
}

const COUNTERS: [(Counter, &str); 12] = [
    (
        Counter::RequestedBytes,
        "racer_prefetch_requested_bytes_total",
    ),
    (Counter::AlignedBytes, "racer_prefetch_aligned_bytes_total"),
    (
        Counter::AlreadyResidentBytes,
        "racer_prefetch_already_resident_bytes_total",
    ),
    (Counter::FetchedBytes, "racer_prefetch_fetched_bytes_total"),
    (Counter::JoinedBytes, "racer_prefetch_joined_bytes_total"),
    (
        Counter::AdmittedBytes,
        "racer_prefetch_admitted_bytes_total",
    ),
    (Counter::FailedBytes, "racer_prefetch_failed_bytes_total"),
    (Counter::DemandJoins, "racer_prefetch_demand_joins_total"),
    (Counter::Retries, "racer_prefetch_retries_total"),
    (Counter::Deferrals, "racer_prefetch_deferrals_total"),
    (
        Counter::WindowUpdates,
        "racer_prefetch_window_updates_total",
    ),
    (
        Counter::WindowDroppedBytes,
        "racer_prefetch_window_dropped_bytes_total",
    ),
];

/// Ticket outcomes for `racer_prefetch_tickets_finished_total`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    Succeeded,
    Partial,
    Failed,
    Canceled,
}

impl Outcome {
    const ALL: [Self; 4] = [Self::Succeeded, Self::Partial, Self::Failed, Self::Canceled];

    fn as_str(self) -> &'static str {
        match self {
            Self::Succeeded => "succeeded",
            Self::Partial => "partial",
            Self::Failed => "failed",
            Self::Canceled => "cancelled",
        }
    }
}

/// Sample lines [`PrefetchMetrics::write`] emits.
pub const SAMPLE_LINES: usize =
    COUNTERS.len() * 3 + 3 + 2 * 3 + 3 * 4 + 3 * (WAIT_BUCKETS_MS.len() + 3);

/// Upper bounds of `racer_prefetch_demand_wait_seconds`, in milliseconds.
const WAIT_BUCKETS_MS: [u64; 10] = [1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 60000];

const COUNTER_SLOTS: usize = COUNTERS.len() * 3;
const THROTTLE_SLOT: usize = COUNTER_SLOTS;
const FINISHED_SLOT: usize = THROTTLE_SLOT + 3;
const HISTOGRAM_SLOT: usize = FINISHED_SLOT + 3 * Outcome::ALL.len();
/// Per priority: every bucket, then count, then the sum in microseconds.
const HISTOGRAM_STRIDE: usize = WAIT_BUCKETS_MS.len() + 2;
const SLOTS: usize = HISTOGRAM_SLOT + 3 * HISTOGRAM_STRIDE;

/// One counter cell in the prefetch registry: a fixed (family, labels) pair.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Slot(u8);

const _: () = assert!(SLOTS <= u8::MAX as usize);

impl crate::telemetry::primitives::Metric for Slot {
    const ALL: &'static [Self] = &{
        let mut all = [Slot(0); SLOTS];
        let mut index = 0;
        while index < SLOTS {
            all[index] = Slot(index as u8);
            index += 1;
        }
        all
    };

    fn index(self) -> usize {
        usize::from(self.0)
    }

    fn name(self) -> &'static str {
        let index = self.index();
        if index < COUNTER_SLOTS {
            COUNTERS[index / 3].1
        } else if index < FINISHED_SLOT {
            "racer_prefetch_throttle_seconds_total"
        } else if index < HISTOGRAM_SLOT {
            "racer_prefetch_tickets_finished_total"
        } else {
            "racer_prefetch_demand_wait_seconds"
        }
    }
}

/// One gauge cell: queued bytes, then tickets, per priority.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct GaugeSlot(u8);

impl crate::telemetry::primitives::Metric for GaugeSlot {
    const ALL: &'static [Self] = &[
        GaugeSlot(0),
        GaugeSlot(1),
        GaugeSlot(2),
        GaugeSlot(3),
        GaugeSlot(4),
        GaugeSlot(5),
    ];

    fn index(self) -> usize {
        usize::from(self.0)
    }

    fn name(self) -> &'static str {
        if self.0 < 3 {
            "racer_prefetch_queued_bytes"
        } else {
            "racer_prefetch_tickets"
        }
    }
}

fn slot(index: usize) -> Slot {
    Slot(index as u8)
}

fn counter_slot(counter: Counter, priority: Priority) -> Slot {
    slot(counter as usize * 3 + priority.index())
}

fn finished_slot(priority: Priority, outcome: Outcome) -> Slot {
    slot(FINISHED_SLOT + priority.index() * Outcome::ALL.len() + outcome as usize)
}

fn histogram_slot(priority: Priority, offset: usize) -> Slot {
    slot(HISTOGRAM_SLOT + priority.index() * HISTOGRAM_STRIDE + offset)
}

/// Prefetch metrics on the per-worker sharded registry. Each worker's
/// `Metrics` handle holds its own shard; the node-wide prefetch service
/// records through the first attached worker's handle from any worker
/// thread, so updates stay atomic `fetch_add`. Counters wrap.
#[derive(Clone)]
pub struct PrefetchMetrics(crate::telemetry::primitives::Metrics<Slot, GaugeSlot>);

impl Default for PrefetchMetrics {
    fn default() -> Self {
        Self::shards(1).pop().expect("one prefetch metrics shard")
    }
}

impl PrefetchMetrics {
    /// One handle per worker over a shared registry.
    pub fn shards(n: usize) -> Vec<Self> {
        crate::telemetry::primitives::Metrics::shards(n)
            .into_iter()
            .map(Self)
            .collect()
    }

    pub fn add(&self, counter: Counter, priority: Priority, value: u64) {
        self.0.add(counter_slot(counter, priority), value);
    }

    pub fn get(&self, counter: Counter, priority: Priority) -> u64 {
        self.0.count(counter_slot(counter, priority))
    }

    pub fn throttled(&self, priority: Priority, wait: Duration) {
        self.0.add(
            slot(THROTTLE_SLOT + priority.index()),
            wait.as_micros().min(u128::from(u64::MAX)) as u64,
        );
    }

    pub fn finished(&self, priority: Priority, outcome: Outcome) {
        self.0.add(finished_slot(priority, outcome), 1);
    }

    pub fn finished_count(&self, priority: Priority, outcome: Outcome) -> u64 {
        self.0.count(finished_slot(priority, outcome))
    }

    /// Replace the node-wide gauges.
    pub fn set_gauges(&self, queued_bytes: [u64; 3], tickets: [u64; 3]) {
        for i in 0..3 {
            self.0.set(GaugeSlot(i as u8), queued_bytes[i]);
            self.0.set(GaugeSlot(3 + i as u8), tickets[i]);
        }
    }

    pub fn tickets(&self, priority: Priority) -> u64 {
        self.0.gauge(GaugeSlot(3 + priority.index() as u8))
    }

    fn queued_bytes(&self, priority: Priority) -> u64 {
        self.0.gauge(GaugeSlot(priority.index() as u8))
    }

    pub fn demand_wait(&self, priority: Priority, wait: Duration) {
        // Compare the exact duration so fractional milliseconds are not
        // truncated into a lower bucket than the recorded sum implies.
        for (offset, bound) in WAIT_BUCKETS_MS.into_iter().enumerate() {
            if wait <= Duration::from_millis(bound) {
                self.0.add(histogram_slot(priority, offset), 1);
            }
        }
        self.0
            .add(histogram_slot(priority, WAIT_BUCKETS_MS.len()), 1);
        self.0.add(
            histogram_slot(priority, WAIT_BUCKETS_MS.len() + 1),
            wait.as_micros().min(u128::from(u64::MAX)) as u64,
        );
    }

    fn bucket(&self, priority: Priority, offset: usize) -> u64 {
        self.0.count(histogram_slot(priority, offset))
    }

    /// Prometheus text exposition.
    pub fn write(&self, out: &mut impl Write) -> std::fmt::Result {
        for (index, (_, name)) in COUNTERS.iter().enumerate() {
            writeln!(out, "# TYPE {name} counter")?;
            for priority in Priority::ALL {
                writeln!(
                    out,
                    "{name}{{priority=\"{}\"}} {}",
                    priority.as_str(),
                    self.0.count(slot(index * 3 + priority.index()))
                )?;
            }
        }
        let name = "racer_prefetch_throttle_seconds_total";
        writeln!(out, "# TYPE {name} counter")?;
        for priority in Priority::ALL {
            let micros = self.0.count(slot(THROTTLE_SLOT + priority.index()));
            writeln!(
                out,
                "{name}{{priority=\"{}\"}} {}.{:06}",
                priority.as_str(),
                micros / 1_000_000,
                micros % 1_000_000
            )?;
        }
        for (name, value) in [
            (
                "racer_prefetch_queued_bytes",
                Self::queued_bytes as fn(&Self, Priority) -> u64,
            ),
            ("racer_prefetch_tickets", Self::tickets),
        ] {
            writeln!(out, "# TYPE {name} gauge")?;
            for priority in Priority::ALL {
                writeln!(
                    out,
                    "{name}{{priority=\"{}\"}} {}",
                    priority.as_str(),
                    value(self, priority)
                )?;
            }
        }
        let name = "racer_prefetch_tickets_finished_total";
        writeln!(out, "# TYPE {name} counter")?;
        for priority in Priority::ALL {
            for outcome in Outcome::ALL {
                writeln!(
                    out,
                    "{name}{{priority=\"{}\",outcome=\"{}\"}} {}",
                    priority.as_str(),
                    outcome.as_str(),
                    self.finished_count(priority, outcome)
                )?;
            }
        }
        let name = "racer_prefetch_demand_wait_seconds";
        writeln!(out, "# TYPE {name} histogram")?;
        for priority in Priority::ALL {
            let label = priority.as_str();
            for (offset, bound) in WAIT_BUCKETS_MS.into_iter().enumerate() {
                writeln!(
                    out,
                    "{name}_bucket{{priority=\"{label}\",le=\"{}.{:03}\"}} {}",
                    bound / 1000,
                    bound % 1000,
                    self.bucket(priority, offset)
                )?;
            }
            let count = self.bucket(priority, WAIT_BUCKETS_MS.len());
            writeln!(
                out,
                "{name}_bucket{{priority=\"{label}\",le=\"+Inf\"}} {count}"
            )?;
            let micros = self.bucket(priority, WAIT_BUCKETS_MS.len() + 1);
            writeln!(
                out,
                "{name}_sum{{priority=\"{label}\"}} {}.{:06}",
                micros / 1_000_000,
                micros % 1_000_000
            )?;
            writeln!(out, "{name}_count{{priority=\"{label}\"}} {count}")?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn bucket_counts(metrics: &PrefetchMetrics, priority: Priority) -> Vec<u64> {
        (0..WAIT_BUCKETS_MS.len())
            .map(|offset| metrics.bucket(priority, offset))
            .collect()
    }

    #[test]
    fn slot_layout_follows_declaration_order() {
        for (index, (counter, _)) in COUNTERS.iter().enumerate() {
            assert_eq!(*counter as usize, index);
        }
        for (index, outcome) in Outcome::ALL.iter().enumerate() {
            assert_eq!(*outcome as usize, index);
        }
        use crate::telemetry::primitives::Metric;
        assert_eq!(Slot::ALL.len(), SLOTS);
        assert_eq!(
            Slot(SLOTS as u8 - 1).name(),
            "racer_prefetch_demand_wait_seconds"
        );
        assert_eq!(
            Slot(THROTTLE_SLOT as u8).name(),
            "racer_prefetch_throttle_seconds_total"
        );
    }

    #[test]
    fn demand_wait_buckets_do_not_truncate_fractional_millis() {
        let cases: [(Duration, usize); 7] = [
            (Duration::ZERO, 0),
            (Duration::from_millis(1), 0),
            (Duration::from_micros(1001), 1),
            (Duration::from_micros(1900), 1),
            (Duration::from_millis(5), 1),
            (Duration::from_micros(5900), 2),
            (Duration::from_nanos(60_000_000_001), WAIT_BUCKETS_MS.len()),
        ];
        for (wait, first) in cases {
            let metrics = PrefetchMetrics::default();
            metrics.demand_wait(Priority::Low, wait);
            let expected: Vec<u64> = (0..WAIT_BUCKETS_MS.len())
                .map(|i| u64::from(i >= first))
                .collect();
            assert_eq!(bucket_counts(&metrics, Priority::Low), expected, "{wait:?}");
        }
        let metrics = PrefetchMetrics::default();
        metrics.demand_wait(Priority::Low, Duration::MAX);
        assert!(
            bucket_counts(&metrics, Priority::Low)
                .iter()
                .all(|&c| c == 0)
        );
        assert_eq!(metrics.bucket(Priority::Low, WAIT_BUCKETS_MS.len()), 1);
        assert_eq!(
            metrics.bucket(Priority::Low, WAIT_BUCKETS_MS.len() + 1),
            u64::MAX
        );
    }

    #[test]
    fn exposition_has_fixed_labels_and_wraps() {
        let shards = PrefetchMetrics::shards(2);
        let metrics = &shards[0];
        metrics.add(Counter::Retries, Priority::Low, u64::MAX);
        shards[1].add(Counter::Retries, Priority::Low, 7);
        metrics.throttled(Priority::Normal, Duration::from_millis(1500));
        metrics.finished(Priority::High, Outcome::Partial);
        metrics.demand_wait(Priority::High, Duration::from_millis(7));
        metrics.set_gauges([1, 2, 3], [4, 5, 6]);
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        assert!(out.contains(&format!(
            "racer_prefetch_retries_total{{priority=\"low\"}} {}\n",
            6
        )));
        assert!(
            out.contains("racer_prefetch_throttle_seconds_total{priority=\"normal\"} 1.500000\n")
        );
        assert!(out.contains(
            "racer_prefetch_tickets_finished_total{priority=\"high\",outcome=\"partial\"} 1\n"
        ));
        assert!(out.contains("racer_prefetch_queued_bytes{priority=\"low\"} 3\n"));
        assert!(out.contains("racer_prefetch_tickets{priority=\"normal\"} 5\n"));
        assert!(out.contains(
            "racer_prefetch_demand_wait_seconds_bucket{priority=\"high\",le=\"0.005\"} 0\n"
        ));
        assert!(out.contains(
            "racer_prefetch_demand_wait_seconds_bucket{priority=\"high\",le=\"0.010\"} 1\n"
        ));
        assert!(out.contains("racer_prefetch_demand_wait_seconds_count{priority=\"high\"} 1\n"));
        assert_eq!(
            out.lines().filter(|l| !l.starts_with('#')).count(),
            SAMPLE_LINES
        );
        for line in out.lines().filter(|l| !l.starts_with('#')) {
            let labels = &line[line.find('{').unwrap() + 1..line.find('}').unwrap()];
            for label in labels.split(',') {
                let name = label.split('=').next().unwrap();
                assert!(matches!(name, "priority" | "outcome" | "le"), "{line}");
            }
        }
    }
}
