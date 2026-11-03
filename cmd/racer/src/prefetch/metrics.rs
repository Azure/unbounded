//! `racer_prefetch_*` metrics. Labels are only `priority` and, for finished
//! tickets, `outcome`, so cardinality is fixed.

use super::Priority;
use std::fmt::Write;
use std::sync::atomic::{AtomicU64, Ordering};
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
    Cancelled,
}

impl Outcome {
    const ALL: [Self; 4] = [
        Self::Succeeded,
        Self::Partial,
        Self::Failed,
        Self::Cancelled,
    ];

    fn as_str(self) -> &'static str {
        match self {
            Self::Succeeded => "succeeded",
            Self::Partial => "partial",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }
}

/// Sample lines [`PrefetchMetrics::write`] emits.
pub const SAMPLE_LINES: usize =
    COUNTERS.len() * 3 + 3 + 2 * 3 + 3 * 4 + 3 * (WAIT_BUCKETS_MS.len() + 3);

/// Upper bounds of `racer_prefetch_demand_wait_seconds`, in milliseconds.
const WAIT_BUCKETS_MS: [u64; 10] = [1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 60000];

#[derive(Default)]
struct Histogram {
    buckets: [AtomicU64; WAIT_BUCKETS_MS.len()],
    count: AtomicU64,
    sum_micros: AtomicU64,
}

/// Node-wide prefetch metrics, shared by every worker's `Metrics` handle.
#[derive(Default)]
pub struct PrefetchMetrics {
    counters: [[AtomicU64; 3]; COUNTERS.len()],
    throttle_micros: [AtomicU64; 3],
    queued_bytes: [AtomicU64; 3],
    tickets: [AtomicU64; 3],
    finished: [[AtomicU64; 4]; 3],
    demand_wait: [Histogram; 3],
}

impl PrefetchMetrics {
    pub fn add(&self, counter: Counter, priority: Priority, value: u64) {
        let index = COUNTERS
            .iter()
            .position(|(c, _)| *c == counter)
            .expect("listed counter");
        saturating_add(&self.counters[index][priority.index()], value);
    }

    pub fn get(&self, counter: Counter, priority: Priority) -> u64 {
        let index = COUNTERS
            .iter()
            .position(|(c, _)| *c == counter)
            .expect("listed counter");
        self.counters[index][priority.index()].load(Ordering::Relaxed)
    }

    pub fn throttled(&self, priority: Priority, wait: Duration) {
        saturating_add(
            &self.throttle_micros[priority.index()],
            wait.as_micros().min(u128::from(u64::MAX)) as u64,
        );
    }

    pub fn finished(&self, priority: Priority, outcome: Outcome) {
        let index = Outcome::ALL
            .iter()
            .position(|o| *o == outcome)
            .expect("listed outcome");
        saturating_add(&self.finished[priority.index()][index], 1);
    }

    pub fn finished_count(&self, priority: Priority, outcome: Outcome) -> u64 {
        let index = Outcome::ALL
            .iter()
            .position(|o| *o == outcome)
            .expect("listed outcome");
        self.finished[priority.index()][index].load(Ordering::Relaxed)
    }

    /// Replace the node-wide gauges.
    pub fn set_gauges(&self, queued_bytes: [u64; 3], tickets: [u64; 3]) {
        for i in 0..3 {
            self.queued_bytes[i].store(queued_bytes[i], Ordering::Relaxed);
            self.tickets[i].store(tickets[i], Ordering::Relaxed);
        }
    }

    pub fn tickets(&self, priority: Priority) -> u64 {
        self.tickets[priority.index()].load(Ordering::Relaxed)
    }

    pub fn demand_wait(&self, priority: Priority, wait: Duration) {
        let histogram = &self.demand_wait[priority.index()];
        let millis = wait.as_millis().min(u128::from(u64::MAX)) as u64;
        for (bucket, bound) in histogram.buckets.iter().zip(WAIT_BUCKETS_MS) {
            if millis <= bound {
                saturating_add(bucket, 1);
            }
        }
        saturating_add(&histogram.count, 1);
        saturating_add(
            &histogram.sum_micros,
            wait.as_micros().min(u128::from(u64::MAX)) as u64,
        );
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
                    self.counters[index][priority.index()].load(Ordering::Relaxed)
                )?;
            }
        }
        let name = "racer_prefetch_throttle_seconds_total";
        writeln!(out, "# TYPE {name} counter")?;
        for priority in Priority::ALL {
            let micros = self.throttle_micros[priority.index()].load(Ordering::Relaxed);
            writeln!(
                out,
                "{name}{{priority=\"{}\"}} {}.{:06}",
                priority.as_str(),
                micros / 1_000_000,
                micros % 1_000_000
            )?;
        }
        for (name, values) in [
            ("racer_prefetch_queued_bytes", &self.queued_bytes),
            ("racer_prefetch_tickets", &self.tickets),
        ] {
            writeln!(out, "# TYPE {name} gauge")?;
            for priority in Priority::ALL {
                writeln!(
                    out,
                    "{name}{{priority=\"{}\"}} {}",
                    priority.as_str(),
                    values[priority.index()].load(Ordering::Relaxed)
                )?;
            }
        }
        let name = "racer_prefetch_tickets_finished_total";
        writeln!(out, "# TYPE {name} counter")?;
        for priority in Priority::ALL {
            for (index, outcome) in Outcome::ALL.iter().enumerate() {
                writeln!(
                    out,
                    "{name}{{priority=\"{}\",outcome=\"{}\"}} {}",
                    priority.as_str(),
                    outcome.as_str(),
                    self.finished[priority.index()][index].load(Ordering::Relaxed)
                )?;
            }
        }
        let name = "racer_prefetch_demand_wait_seconds";
        writeln!(out, "# TYPE {name} histogram")?;
        for priority in Priority::ALL {
            let histogram = &self.demand_wait[priority.index()];
            let label = priority.as_str();
            for (bucket, bound) in histogram.buckets.iter().zip(WAIT_BUCKETS_MS) {
                writeln!(
                    out,
                    "{name}_bucket{{priority=\"{label}\",le=\"{}.{:03}\"}} {}",
                    bound / 1000,
                    bound % 1000,
                    bucket.load(Ordering::Relaxed)
                )?;
            }
            let count = histogram.count.load(Ordering::Relaxed);
            writeln!(
                out,
                "{name}_bucket{{priority=\"{label}\",le=\"+Inf\"}} {count}"
            )?;
            let micros = histogram.sum_micros.load(Ordering::Relaxed);
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

fn saturating_add(counter: &AtomicU64, value: u64) {
    let _ = counter.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |old| {
        Some(old.saturating_add(value))
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exposition_has_fixed_labels_and_saturates() {
        let metrics = PrefetchMetrics::default();
        metrics.add(Counter::Retries, Priority::Low, u64::MAX);
        metrics.add(Counter::Retries, Priority::Low, 7);
        metrics.throttled(Priority::Normal, Duration::from_millis(1500));
        metrics.finished(Priority::High, Outcome::Partial);
        metrics.demand_wait(Priority::High, Duration::from_millis(7));
        metrics.set_gauges([1, 2, 3], [4, 5, 6]);
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        assert!(out.contains(&format!(
            "racer_prefetch_retries_total{{priority=\"low\"}} {}\n",
            u64::MAX
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
