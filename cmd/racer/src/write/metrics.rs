//! `racer_write_*` metrics. Node-wide and keyed by cache ID, so a series
//! appears only for caches that saw write activity.

use std::collections::BTreeMap;
use std::fmt::Write;
use std::sync::Mutex;
use std::time::Duration;

/// Outcomes of `racer_write_requests_total`.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum Outcome {
    Created,
    Deleted,
    Persisted,
    Exists,
    Rejected,
    Unavailable,
    Failed,
}

impl Outcome {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Created => "created",
            Self::Deleted => "deleted",
            Self::Persisted => "persisted",
            Self::Exists => "exists",
            Self::Rejected => "rejected",
            Self::Unavailable => "unavailable",
            Self::Failed => "failed",
        }
    }
}

/// Upper bounds of `racer_write_fsync_seconds`, in microseconds.
const FSYNC_BUCKETS_MICROS: [u64; 10] = [
    100, 500, 1_000, 2_500, 5_000, 10_000, 25_000, 50_000, 100_000, 1_000_000,
];

/// Every `racer_write_upload_state` label, in order.
const STATES: [racer_core::write::UploadState; 6] = [
    racer_core::write::UploadState::Pending,
    racer_core::write::UploadState::Manual,
    racer_core::write::UploadState::Uploading,
    racer_core::write::UploadState::Committed,
    racer_core::write::UploadState::Failed,
    racer_core::write::UploadState::Never,
];

#[derive(Default)]
struct CacheMetrics {
    requests: BTreeMap<Outcome, u64>,
    bytes: u64,
    upload_bytes: u64,
    repairs: u64,
    /// Dirty bytes and upload-state counts, last sampled per worker.
    dirty: BTreeMap<u16, u64>,
    states: BTreeMap<u16, [u64; STATES.len()]>,
}

#[derive(Default)]
struct State {
    caches: BTreeMap<String, CacheMetrics>,
    /// Bucket counts, then count and sum in microseconds.
    fsync: [u64; FSYNC_BUCKETS_MICROS.len()],
    fsync_count: u64,
    fsync_micros: u64,
}

/// Node-wide write metrics, shared by every worker's telemetry handle.
#[derive(Default)]
pub struct WriteMetrics(Mutex<State>);

impl WriteMetrics {
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.0.lock().unwrap_or_else(|poison| poison.into_inner())
    }

    fn cache<'a>(state: &'a mut State, cache: &str) -> &'a mut CacheMetrics {
        if !state.caches.contains_key(cache) {
            state
                .caches
                .insert(cache.to_owned(), CacheMetrics::default());
        }
        state.caches.get_mut(cache).expect("inserted")
    }

    /// Count one finished client write request.
    pub fn request(&self, cache: &str, outcome: Outcome) {
        let mut state = self.lock();
        *Self::cache(&mut state, cache)
            .requests
            .entry(outcome)
            .or_default() += 1;
    }

    /// Count object bytes accepted from clients.
    pub fn bytes(&self, cache: &str, bytes: u64) {
        let mut state = self.lock();
        let entry = Self::cache(&mut state, cache);
        entry.bytes = entry.bytes.wrapping_add(bytes);
    }

    /// Count plaintext bytes uploaded to the origin by Put Block.
    pub fn upload_bytes(&self, cache: &str, bytes: u64) {
        let mut state = self.lock();
        let entry = Self::cache(&mut state, cache);
        entry.upload_bytes = entry.upload_bytes.wrapping_add(bytes);
    }

    /// Count one repaired copy (WP8).
    pub fn repair(&self, cache: &str) {
        let mut state = self.lock();
        let entry = Self::cache(&mut state, cache);
        entry.repairs = entry.repairs.wrapping_add(1);
    }

    /// Record one durability barrier (a durable written page or journaled
    /// session record) that took `elapsed`.
    pub fn fsync(&self, elapsed: Duration) {
        let micros = u64::try_from(elapsed.as_micros()).unwrap_or(u64::MAX);
        let mut state = self.lock();
        for (bucket, bound) in state.fsync.iter_mut().zip(FSYNC_BUCKETS_MICROS) {
            if micros <= bound {
                *bucket += 1;
            }
        }
        state.fsync_count = state.fsync_count.wrapping_add(1);
        state.fsync_micros = state.fsync_micros.wrapping_add(micros);
    }

    /// Replace `worker`'s dirty-byte sample of each cache in `dirty`; caches
    /// absent from `known` keep no sample from this worker.
    pub fn sample_dirty(&self, worker: u16, dirty: &[(String, u64)]) {
        let mut state = self.lock();
        for entry in state.caches.values_mut() {
            entry.dirty.remove(&worker);
        }
        for (cache, bytes) in dirty {
            Self::cache(&mut state, cache).dirty.insert(worker, *bytes);
        }
    }

    /// Replace `worker`'s upload-state counts.
    pub fn sample_states(&self, worker: u16, states: &[(String, racer_core::write::UploadState)]) {
        let mut state = self.lock();
        for entry in state.caches.values_mut() {
            entry.states.remove(&worker);
        }
        for (cache, upload) in states {
            let index = STATES.iter().position(|s| s == upload).unwrap_or(0);
            Self::cache(&mut state, cache)
                .states
                .entry(worker)
                .or_insert([0; STATES.len()])[index] += 1;
        }
    }

    /// Requests counted for `cache` with `outcome`, for tests.
    pub fn requests(&self, cache: &str, outcome: Outcome) -> u64 {
        self.lock()
            .caches
            .get(cache)
            .and_then(|c| c.requests.get(&outcome).copied())
            .unwrap_or(0)
    }

    /// Write every series in the Prometheus text format. Nothing is written
    /// before the first write activity.
    pub fn write(&self, out: &mut impl Write) -> std::fmt::Result {
        let state = self.lock();
        if state.caches.is_empty() && state.fsync_count == 0 {
            return Ok(());
        }
        writeln!(out, "# TYPE racer_write_requests_total counter")?;
        for (cache, entry) in &state.caches {
            for (outcome, count) in &entry.requests {
                writeln!(
                    out,
                    "racer_write_requests_total{{cache=\"{cache}\",outcome=\"{}\"}} {count}",
                    outcome.as_str()
                )?;
            }
        }
        for (name, kind, value) in [
            (
                "racer_write_bytes_total",
                "counter",
                (|e: &CacheMetrics| e.bytes) as fn(&CacheMetrics) -> u64,
            ),
            ("racer_write_dirty_bytes", "gauge", |e| {
                e.dirty.values().sum()
            }),
            ("racer_write_upload_bytes_total", "counter", |e| {
                e.upload_bytes
            }),
            ("racer_write_repairs_total", "counter", |e| e.repairs),
        ] {
            writeln!(out, "# TYPE {name} {kind}")?;
            for (cache, entry) in &state.caches {
                writeln!(out, "{name}{{cache=\"{cache}\"}} {}", value(entry))?;
            }
        }
        writeln!(out, "# TYPE racer_write_upload_state gauge")?;
        for (cache, entry) in &state.caches {
            for (index, upload) in STATES.iter().enumerate() {
                let count: u64 = entry.states.values().map(|s| s[index]).sum();
                writeln!(
                    out,
                    "racer_write_upload_state{{cache=\"{cache}\",state=\"{}\"}} {count}",
                    upload.as_str()
                )?;
            }
        }
        writeln!(out, "# TYPE racer_write_fsync_seconds histogram")?;
        for (bucket, bound) in state.fsync.iter().zip(FSYNC_BUCKETS_MICROS) {
            writeln!(
                out,
                "racer_write_fsync_seconds_bucket{{le=\"{}\"}} {bucket}",
                bound as f64 / 1e6
            )?;
        }
        writeln!(
            out,
            "racer_write_fsync_seconds_bucket{{le=\"+Inf\"}} {}",
            state.fsync_count
        )?;
        writeln!(
            out,
            "racer_write_fsync_seconds_sum {}",
            state.fsync_micros as f64 / 1e6
        )?;
        writeln!(out, "racer_write_fsync_seconds_count {}", state.fsync_count)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nothing_is_written_before_activity() {
        let metrics = WriteMetrics::default();
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        assert!(out.is_empty());
    }

    #[test]
    fn series_are_labeled_by_cache() {
        let metrics = WriteMetrics::default();
        metrics.request("c1", Outcome::Created);
        metrics.request("c1", Outcome::Created);
        metrics.request("c1", Outcome::Exists);
        metrics.bytes("c1", 10);
        metrics.upload_bytes("c1", 4);
        metrics.sample_dirty(0, &[("c1".into(), 7)]);
        metrics.sample_dirty(1, &[("c1".into(), 5)]);
        metrics.sample_states(
            0,
            &[
                ("c1".into(), racer_core::write::UploadState::Never),
                ("c1".into(), racer_core::write::UploadState::Never),
            ],
        );
        metrics.fsync(Duration::from_micros(700));
        metrics.repair("c1");
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        for line in [
            "racer_write_requests_total{cache=\"c1\",outcome=\"created\"} 2",
            "racer_write_requests_total{cache=\"c1\",outcome=\"exists\"} 1",
            "racer_write_bytes_total{cache=\"c1\"} 10",
            "racer_write_dirty_bytes{cache=\"c1\"} 12",
            "racer_write_upload_bytes_total{cache=\"c1\"} 4",
            "racer_write_repairs_total{cache=\"c1\"} 1",
            "racer_write_upload_state{cache=\"c1\",state=\"never\"} 2",
            "racer_write_upload_state{cache=\"c1\",state=\"pending\"} 0",
            "racer_write_fsync_seconds_bucket{le=\"0.0005\"} 0",
            "racer_write_fsync_seconds_bucket{le=\"0.001\"} 1",
            "racer_write_fsync_seconds_count 1",
        ] {
            assert!(out.lines().any(|l| l == line), "{line} missing in\n{out}");
        }
        // A new sample replaces the worker's previous one.
        metrics.sample_dirty(0, &[]);
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        assert!(out.contains("racer_write_dirty_bytes{cache=\"c1\"} 5\n"));
        assert_eq!(metrics.requests("c1", Outcome::Created), 2);
    }
}
