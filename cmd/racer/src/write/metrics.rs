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
    /// Whether the write did what the client asked: `201` or `204`.
    pub fn succeeded(self) -> bool {
        matches!(self, Self::Created | Self::Deleted | Self::Persisted)
    }

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
    /// Written pages whose page key left the keyring, last sampled per
    /// worker by the rekey scan.
    unreadable: BTreeMap<u16, u64>,
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

    /// Replace `worker`'s sample of written pages whose page key is no
    /// longer in the keyring, per cache.
    pub fn sample_unreadable(&self, worker: u16, unreadable: &[(String, u64)]) {
        let mut state = self.lock();
        for entry in state.caches.values_mut() {
            entry.unreadable.remove(&worker);
        }
        for (cache, pages) in unreadable {
            Self::cache(&mut state, cache)
                .unreadable
                .insert(worker, *pages);
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
        let mut exposition = self.exposition();
        while exposition.next(out)? {}
        Ok(())
    }

    /// A snapshot written one bounded piece at a time: each family's type
    /// line, then one piece per cache, so the streamed `/metrics` response
    /// never needs room for every cache at once.
    pub fn exposition(&self) -> WriteExposition {
        let state = self.lock();
        let empty = state.caches.is_empty() && state.fsync_count == 0;
        let rows = state
            .caches
            .iter()
            .map(|(cache, entry)| Row {
                cache: cache.clone(),
                requests: entry.requests.iter().map(|(o, n)| (*o, *n)).collect(),
                scalars: [
                    entry.bytes,
                    entry.dirty.values().sum(),
                    entry.upload_bytes,
                    entry.repairs,
                    entry.unreadable.values().sum(),
                ],
                states: std::array::from_fn(|index| entry.states.values().map(|s| s[index]).sum()),
            })
            .collect();
        WriteExposition {
            rows,
            fsync: state.fsync,
            fsync_count: state.fsync_count,
            fsync_micros: state.fsync_micros,
            empty,
            piece: 0,
        }
    }
}

/// Per-cache values captured by [`WriteMetrics::exposition`].
struct Row {
    cache: String,
    requests: Vec<(Outcome, u64)>,
    scalars: [u64; SCALARS.len()],
    states: [u64; STATES.len()],
}

/// Single-value per-cache families, in [`Row::scalars`] order.
const SCALARS: [(&str, &str); 5] = [
    ("racer_write_bytes_total", "counter"),
    ("racer_write_dirty_bytes", "gauge"),
    ("racer_write_upload_bytes_total", "counter"),
    ("racer_write_repairs_total", "counter"),
    ("racer_write_unreadable_pages", "gauge"),
];

/// Per-cache families: requests, the scalars, then upload states.
const FAMILIES: usize = SCALARS.len() + 2;

/// Resumable writer of a [`WriteMetrics`] snapshot. A failed piece leaves
/// the position unchanged, so the caller can retry it with fresh space.
pub struct WriteExposition {
    rows: Vec<Row>,
    fsync: [u64; FSYNC_BUCKETS_MICROS.len()],
    fsync_count: u64,
    fsync_micros: u64,
    empty: bool,
    piece: usize,
}

impl WriteExposition {
    fn pieces(&self) -> usize {
        if self.empty {
            0
        } else {
            FAMILIES * (1 + self.rows.len()) + 1
        }
    }

    /// Write the next piece; `Ok(true)` while more remain.
    pub fn next(&mut self, out: &mut impl Write) -> Result<bool, std::fmt::Error> {
        let total = self.pieces();
        if self.piece >= total {
            return Ok(false);
        }
        let per_family = 1 + self.rows.len();
        let (family, index) = (self.piece / per_family, self.piece % per_family);
        if family == FAMILIES {
            self.write_fsync(out)?;
        } else if index == 0 {
            let (name, kind) = match family {
                0 => ("racer_write_requests_total", "counter"),
                f if f <= SCALARS.len() => SCALARS[f - 1],
                _ => ("racer_write_upload_state", "gauge"),
            };
            writeln!(out, "# TYPE {name} {kind}")?;
        } else {
            let row = &self.rows[index - 1];
            let cache = &row.cache;
            match family {
                0 => {
                    for (outcome, count) in &row.requests {
                        writeln!(
                            out,
                            "racer_write_requests_total{{cache=\"{cache}\",outcome=\"{}\"}} {count}",
                            outcome.as_str()
                        )?;
                    }
                }
                f if f <= SCALARS.len() => {
                    let name = SCALARS[f - 1].0;
                    writeln!(out, "{name}{{cache=\"{cache}\"}} {}", row.scalars[f - 1])?;
                }
                _ => {
                    for (upload, count) in STATES.iter().zip(row.states) {
                        writeln!(
                            out,
                            "racer_write_upload_state{{cache=\"{cache}\",state=\"{}\"}} {count}",
                            upload.as_str()
                        )?;
                    }
                }
            }
        }
        self.piece += 1;
        Ok(self.piece < total)
    }

    fn write_fsync(&self, out: &mut impl Write) -> std::fmt::Result {
        writeln!(out, "# TYPE racer_write_fsync_seconds histogram")?;
        for (bucket, bound) in self.fsync.iter().zip(FSYNC_BUCKETS_MICROS) {
            writeln!(
                out,
                "racer_write_fsync_seconds_bucket{{le=\"{}\"}} {bucket}",
                bound as f64 / 1e6
            )?;
        }
        writeln!(
            out,
            "racer_write_fsync_seconds_bucket{{le=\"+Inf\"}} {}",
            self.fsync_count
        )?;
        writeln!(
            out,
            "racer_write_fsync_seconds_sum {}",
            self.fsync_micros as f64 / 1e6
        )?;
        writeln!(out, "racer_write_fsync_seconds_count {}", self.fsync_count)
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
        metrics.sample_unreadable(0, &[("c1".into(), 3)]);
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        for line in [
            "racer_write_requests_total{cache=\"c1\",outcome=\"created\"} 2",
            "racer_write_requests_total{cache=\"c1\",outcome=\"exists\"} 1",
            "racer_write_bytes_total{cache=\"c1\"} 10",
            "racer_write_dirty_bytes{cache=\"c1\"} 12",
            "racer_write_upload_bytes_total{cache=\"c1\"} 4",
            "racer_write_repairs_total{cache=\"c1\"} 1",
            "racer_write_unreadable_pages{cache=\"c1\"} 3",
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
        metrics.sample_unreadable(0, &[]);
        let mut out = String::new();
        metrics.write(&mut out).unwrap();
        assert!(out.contains("racer_write_dirty_bytes{cache=\"c1\"} 5\n"));
        assert!(out.contains("racer_write_unreadable_pages{cache=\"c1\"} 0\n"));
        assert_eq!(metrics.requests("c1", Outcome::Created), 2);
    }

    /// Each piece holds one family's type line or one cache's series, so a
    /// piece stays small however many caches write, and the pieces together
    /// equal the whole exposition with every family contiguous.
    #[test]
    fn exposition_streams_one_cache_per_piece() {
        let metrics = WriteMetrics::default();
        let caches = 20_000;
        for i in 0..caches {
            let cache = format!("{i:08x}-1111-4111-8111-111111111111");
            metrics.request(&cache, Outcome::Created);
            metrics.bytes(&cache, 1);
            metrics.sample_states(
                0,
                &[(cache.clone(), racer_core::write::UploadState::Pending)],
            );
        }
        let mut whole = String::new();
        metrics.write(&mut whole).unwrap();
        assert!(whole.len() > 1024 * 1024, "{}", whole.len());
        let mut exposition = metrics.exposition();
        let mut joined = String::new();
        let mut largest = 0;
        loop {
            let mut piece = String::new();
            let more = exposition.next(&mut piece).unwrap();
            largest = largest.max(piece.len());
            joined.push_str(&piece);
            if !more {
                break;
            }
        }
        assert_eq!(joined, whole);
        assert!(largest < 4096, "{largest}");
        // Every family's samples follow its single type line.
        let mut seen = std::collections::BTreeSet::new();
        let mut current = String::new();
        for line in whole.lines() {
            if let Some(rest) = line.strip_prefix("# TYPE ") {
                current = rest.split(' ').next().unwrap().to_owned();
                assert!(seen.insert(current.clone()), "{current} repeated");
            } else {
                let name = line.split(['{', ' ']).next().unwrap();
                assert!(name.starts_with(&current), "{name} outside {current}");
            }
        }
        // A failed piece is retried from the same position.
        let mut exposition = metrics.exposition();
        let mut full = String::new();
        exposition.next(&mut full).unwrap();
        let mut failing = Failing;
        assert!(exposition.next(&mut failing).is_err());
        exposition.next(&mut full).unwrap();
        assert!(whole.starts_with(&full));
    }

    struct Failing;
    impl Write for Failing {
        fn write_str(&mut self, _: &str) -> std::fmt::Result {
            Err(std::fmt::Error)
        }
    }
}
