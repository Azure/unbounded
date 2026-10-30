//! Histograms, counters, thread CPU time, and block device counters.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::path::PathBuf;
use std::time::Duration;

/// Sub-buckets per power of two (about 12% resolution).
const SUB_BITS: u32 = 3;
const SUB: usize = 1 << SUB_BITS;
const BUCKETS: usize = 64 * SUB;

/// Log-linear histogram of nanosecond (or unitless) samples.
#[derive(Clone)]
pub struct Hist {
    buckets: Box<[u64; BUCKETS]>,
    count: u64,
    sum: u128,
    min: u64,
    max: u64,
}

impl Default for Hist {
    fn default() -> Self {
        Self {
            buckets: Box::new([0; BUCKETS]),
            count: 0,
            sum: 0,
            min: u64::MAX,
            max: 0,
        }
    }
}

fn bucket(value: u64) -> usize {
    if value < SUB as u64 {
        return value as usize;
    }
    let power = 63 - value.leading_zeros();
    let shift = power - SUB_BITS;
    let sub = ((value >> shift) as usize) & (SUB - 1);
    ((power - SUB_BITS + 1) as usize) * SUB + sub
}

fn bucket_high(index: usize) -> u64 {
    if index < SUB {
        return index as u64;
    }
    let group = (index / SUB) as u32;
    let sub = (index % SUB) as u64;
    let shift = group - 1;
    let low = ((SUB as u64) | sub) << shift;
    low + (1u64 << shift) - 1
}

impl Hist {
    /// Add one sample.
    #[inline]
    pub fn record(&mut self, value: u64) {
        self.buckets[bucket(value)] += 1;
        self.count += 1;
        self.sum += u128::from(value);
        self.min = self.min.min(value);
        self.max = self.max.max(value);
    }

    /// Add one duration sample in nanoseconds.
    #[inline]
    pub fn record_duration(&mut self, value: Duration) {
        self.record(value.as_nanos().min(u128::from(u64::MAX)) as u64);
    }

    /// Merge another histogram.
    pub fn merge(&mut self, other: &Hist) {
        for (a, b) in self.buckets.iter_mut().zip(other.buckets.iter()) {
            *a += b;
        }
        self.count += other.count;
        self.sum += other.sum;
        self.min = self.min.min(other.min);
        self.max = self.max.max(other.max);
    }

    /// Samples recorded.
    pub fn count(&self) -> u64 {
        self.count
    }

    /// Sum of samples.
    pub fn sum(&self) -> u128 {
        self.sum
    }

    /// Mean sample, 0 when empty.
    pub fn mean(&self) -> f64 {
        if self.count == 0 {
            0.0
        } else {
            self.sum as f64 / self.count as f64
        }
    }

    /// Largest sample, 0 when empty.
    pub fn max(&self) -> u64 {
        self.max
    }

    /// Approximate quantile `q` in `[0, 1]` (bucket upper bound, clamped to max).
    pub fn quantile(&self, q: f64) -> u64 {
        if self.count == 0 {
            return 0;
        }
        let rank = ((self.count as f64 * q).ceil() as u64).clamp(1, self.count);
        let mut seen = 0;
        for (index, &n) in self.buckets.iter().enumerate() {
            seen += n;
            if seen >= rank {
                return bucket_high(index).min(self.max).max(self.min);
            }
        }
        self.max
    }
}

/// Named counters and histograms produced by one worker for one job.
#[derive(Clone, Default)]
pub struct Report {
    /// Counters. Keys ending in `_max` merge with max, others sum.
    pub counters: BTreeMap<String, u64>,

    /// Histograms. Keys ending in `_ns` are durations.
    pub hists: BTreeMap<String, Hist>,

    /// Free-form notes, deduplicated across workers.
    pub notes: Vec<String>,
}

impl Report {
    /// Add to a counter.
    pub fn add(&mut self, key: impl Into<String>, value: u64) {
        *self.counters.entry(key.into()).or_default() += value;
    }

    /// Set a max-merged counter.
    pub fn max(&mut self, key: impl Into<String>, value: u64) {
        let entry = self.counters.entry(key.into()).or_default();
        *entry = (*entry).max(value);
    }

    /// Add a histogram, merging if present.
    pub fn hist(&mut self, key: impl Into<String>, hist: &Hist) {
        if hist.count() == 0 {
            return;
        }
        self.hists.entry(key.into()).or_default().merge(hist);
    }

    /// Add a single sample to a histogram.
    pub fn sample(&mut self, key: impl Into<String>, value: u64) {
        self.hists.entry(key.into()).or_default().record(value);
    }

    /// Add a note once.
    pub fn note(&mut self, note: impl Into<String>) {
        let note = note.into();
        if !self.notes.contains(&note) {
            self.notes.push(note);
        }
    }

    /// Merge another worker's report.
    pub fn merge(&mut self, other: &Report) {
        for (key, &value) in &other.counters {
            if key.ends_with("_max") {
                self.max(key.clone(), value);
            } else {
                self.add(key.clone(), value);
            }
        }
        for (key, hist) in &other.hists {
            self.hist(key.clone(), hist);
        }
        for note in &other.notes {
            self.note(note.clone());
        }
    }

    /// Counter value, 0 when absent.
    pub fn get(&self, key: &str) -> u64 {
        self.counters.get(key).copied().unwrap_or(0)
    }
}

/// CPU time and context switches of the calling thread.
#[derive(Clone, Copy, Default)]
pub struct ThreadUsage {
    pub user_ns: u64,
    pub system_ns: u64,
    pub voluntary: u64,
    pub involuntary: u64,
}

impl ThreadUsage {
    /// Sample `RUSAGE_THREAD`.
    pub fn now() -> Self {
        // SAFETY: getrusage writes one initialized rusage struct.
        let mut usage: libc::rusage = unsafe { std::mem::zeroed() };
        unsafe { libc::getrusage(libc::RUSAGE_THREAD, &mut usage) };
        let ns = |t: libc::timeval| t.tv_sec as u64 * 1_000_000_000 + t.tv_usec as u64 * 1_000;
        Self {
            user_ns: ns(usage.ru_utime),
            system_ns: ns(usage.ru_stime),
            voluntary: usage.ru_nvcsw as u64,
            involuntary: usage.ru_nivcsw as u64,
        }
    }

    /// Record the difference since `start` into `report`.
    pub fn since(self, start: Self, report: &mut Report) {
        report.add("cpu_user_ns", self.user_ns.saturating_sub(start.user_ns));
        report.add(
            "cpu_system_ns",
            self.system_ns.saturating_sub(start.system_ns),
        );
        report.add(
            "ctx_voluntary",
            self.voluntary.saturating_sub(start.voluntary),
        );
        report.add(
            "ctx_involuntary",
            self.involuntary.saturating_sub(start.involuntary),
        );
    }
}

/// One backing block device, for `/sys/dev/block/M:m/stat` sampling.
#[derive(Clone)]
pub struct DeviceStat {
    pub name: String,
    pub path: PathBuf,
}

/// Counters from one `stat` sample.
#[derive(Clone, Copy, Default)]
pub struct DeviceSample {
    pub reads: u64,
    pub read_sectors: u64,
    pub writes: u64,
    pub write_sectors: u64,
    pub io_ticks_ms: u64,
    pub flushes: u64,
    pub flush_ticks_ms: u64,
}

impl DeviceStat {
    /// Stat file for a device number.
    pub fn for_dev(name: String, dev: u64) -> Self {
        let major = ((dev >> 8) & 0xfff) | ((dev >> 32) & !0xfff);
        let minor = (dev & 0xff) | ((dev >> 12) & !0xff);
        Self {
            name,
            path: PathBuf::from(format!("/sys/dev/block/{major}:{minor}/stat")),
        }
    }

    /// Read the counters, or zeros when unavailable.
    pub fn sample(&self) -> DeviceSample {
        let Ok(text) = std::fs::read_to_string(&self.path) else {
            return DeviceSample::default();
        };
        let f: Vec<u64> = text
            .split_whitespace()
            .map(|s| s.parse().unwrap_or(0))
            .collect();
        let at = |i: usize| f.get(i).copied().unwrap_or(0);
        DeviceSample {
            reads: at(0),
            read_sectors: at(2),
            writes: at(4),
            write_sectors: at(6),
            io_ticks_ms: at(9),
            flushes: at(15),
            flush_ticks_ms: at(16),
        }
    }
}

/// Format a JSON string literal.
pub fn json_str(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if (c as u32) < 0x20 => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// Format a float for JSON (finite only).
pub fn json_f64(v: f64) -> String {
    if v.is_finite() {
        format!("{v:.6}")
    } else {
        "null".into()
    }
}
