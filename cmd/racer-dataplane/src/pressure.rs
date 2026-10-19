//! cgroup v2 memory-pressure monitor.
//!
//! One owner (normally a single worker or the control thread) calls
//! [`PressureMonitor::poll`] from its loop. Sampling happens at most once per
//! [`SAMPLE_INTERVAL`] and reads only small sysfs and procfs files. The
//! classified level is published through [`SharedPressure`], which every
//! worker may read cheaply on each tick and translate into work with
//! [`pressure_actions`].

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicU8, Ordering};
use std::time::{Duration, Instant};

/// Mount point of the unified cgroup v2 hierarchy.
pub(crate) const CGROUP_MOUNT: &str = "/sys/fs/cgroup";
/// Membership file listing the process's cgroups.
pub(crate) const PROC_SELF_CGROUP: &str = "/proc/self/cgroup";
/// Process memory statistics in pages.
pub(crate) const PROC_SELF_STATM: &str = "/proc/self/statm";

/// Normal sampling cadence while cgroup files are readable.
pub(crate) const SAMPLE_INTERVAL: Duration = Duration::from_secs(1);
/// Sampling cadence once the cgroup files proved unavailable, so a host
/// without cgroup v2 (or without permission) does not retry every second.
pub(crate) const UNAVAILABLE_INTERVAL: Duration = Duration::from_secs(30);
/// Minimum spacing between allocator trims while pressure persists.
pub(crate) const TRIM_INTERVAL: Duration = Duration::from_secs(10);

/// Utilization (current / limit) at or above which Normal becomes Elevated.
pub(crate) const UTIL_ELEVATED_ENTER: f64 = 0.80;
/// Utilization below which Elevated returns to Normal.
pub(crate) const UTIL_ELEVATED_EXIT: f64 = 0.70;
/// Utilization at or above which any lower level becomes Critical.
pub(crate) const UTIL_CRITICAL_ENTER: f64 = 0.90;
/// Utilization below which Critical drops to Elevated.
pub(crate) const UTIL_CRITICAL_EXIT: f64 = 0.85;
/// PSI `some avg10` percent at or above which Normal becomes Elevated.
pub(crate) const PSI_ELEVATED_ENTER: f64 = 10.0;
/// PSI `some avg10` percent below which Elevated returns to Normal.
pub(crate) const PSI_ELEVATED_EXIT: f64 = 5.0;
/// PSI `some avg10` percent at or above which any lower level becomes Critical.
pub(crate) const PSI_CRITICAL_ENTER: f64 = 40.0;
/// PSI `some avg10` percent below which Critical drops to Elevated.
pub(crate) const PSI_CRITICAL_EXIT: f64 = 20.0;

/// Memory-pressure level, ordered from least to most severe.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
#[repr(u8)]
pub(crate) enum PressureLevel {
    #[default]
    Normal = 0,
    Elevated = 1,
    Critical = 2,
}

impl PressureLevel {
    /// Decode a stored byte; unknown values saturate to Critical.
    pub(crate) fn from_u8(value: u8) -> Self {
        match value {
            0 => Self::Normal,
            1 => Self::Elevated,
            _ => Self::Critical,
        }
    }
}

/// One snapshot of the memory signals. Every field is optional because each
/// file may be absent or malformed independently.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct MemorySample {
    /// cgroup `memory.current` in bytes.
    pub(crate) current: Option<u64>,
    /// Smaller of `memory.high` and `memory.max`, ignoring unset (`max`) values.
    pub(crate) limit: Option<u64>,
    /// PSI `some avg10` from `memory.pressure`, in percent.
    pub(crate) some_avg10: Option<f64>,
    /// Process resident set size in bytes from `/proc/self/statm`.
    pub(crate) resident: Option<u64>,
}

impl MemorySample {
    /// current / limit when both are known and the limit is nonzero.
    pub(crate) fn utilization(&self) -> Option<f64> {
        match (self.current, self.limit) {
            (Some(current), Some(limit)) if limit > 0 => Some(current as f64 / limit as f64),
            _ => None,
        }
    }

    fn has_cgroup_signal(&self) -> bool {
        self.current.is_some() || self.limit.is_some() || self.some_avg10.is_some()
    }
}

/// Parse `memory.current` (a single decimal u64).
pub(crate) fn parse_u64(text: &str) -> Option<u64> {
    text.trim().parse().ok()
}

/// Parse `memory.high` or `memory.max`. `max` means unlimited and yields
/// `Ok(None)`; malformed text yields `Err(())`.
#[allow(clippy::result_unit_err)]
pub(crate) fn parse_limit(text: &str) -> Result<Option<u64>, ()> {
    match text.trim() {
        "max" => Ok(None),
        value => value.parse().map(Some).map_err(|_| ()),
    }
}

/// Extract `some avg10` from a PSI file. A missing `full` line is fine.
pub(crate) fn parse_psi_some_avg10(text: &str) -> Option<f64> {
    let line = text.lines().find(|line| line.starts_with("some "))?;
    let value = line
        .split_ascii_whitespace()
        .skip(1)
        .find_map(|field| field.strip_prefix("avg10="))?;
    let parsed: f64 = value.parse().ok()?;
    (parsed.is_finite() && parsed >= 0.0).then_some(parsed)
}

/// Resident bytes from `/proc/self/statm` (second field, in pages).
pub(crate) fn parse_statm_resident(text: &str, page_size: u64) -> Option<u64> {
    let pages: u64 = text.split_ascii_whitespace().nth(1)?.parse().ok()?;
    pages.checked_mul(page_size)
}

/// Locate the unified (`0::`) cgroup path from `/proc/self/cgroup` content and
/// join it under `mount`. A namespaced `/` maps to `mount` itself.
pub(crate) fn cgroup_dir_from(proc_cgroup: &str, mount: &Path) -> Option<PathBuf> {
    let path = proc_cgroup
        .lines()
        .find_map(|line| line.strip_prefix("0::"))?
        .trim();
    if !path.starts_with('/') || path.split('/').any(|part| part == "..") {
        return None;
    }
    Some(mount.join(path.trim_start_matches('/')))
}

/// Discover the live cgroup v2 directory of this process, if any.
pub(crate) fn discover_cgroup_dir() -> Option<PathBuf> {
    let membership = fs::read_to_string(PROC_SELF_CGROUP).ok()?;
    let dir = cgroup_dir_from(&membership, Path::new(CGROUP_MOUNT))?;
    dir.join("memory.current").is_file().then_some(dir)
}

fn page_size() -> u64 {
    // SAFETY: sysconf has no memory-safety preconditions.
    let size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
    u64::try_from(size).ok().filter(|s| *s > 0).unwrap_or(4096)
}

fn read_small(path: &Path) -> Option<String> {
    fs::read_to_string(path).ok()
}

/// Read one sample from `cgroup` (may be None) and `statm`.
pub(crate) fn read_sample(cgroup: Option<&Path>, statm: &Path, page_size: u64) -> MemorySample {
    let mut sample = MemorySample {
        resident: read_small(statm).and_then(|text| parse_statm_resident(&text, page_size)),
        ..MemorySample::default()
    };
    if let Some(dir) = cgroup {
        sample.current = read_small(&dir.join("memory.current")).and_then(|t| parse_u64(&t));
        let high = read_small(&dir.join("memory.high")).and_then(|t| parse_limit(&t).ok()?);
        let max = read_small(&dir.join("memory.max")).and_then(|t| parse_limit(&t).ok()?);
        sample.limit = match (high, max) {
            (Some(h), Some(m)) => Some(h.min(m)),
            (h, m) => h.or(m),
        };
        sample.some_avg10 =
            read_small(&dir.join("memory.pressure")).and_then(|t| parse_psi_some_avg10(&t));
    }
    sample
}

/// Classify one signal with hysteresis relative to `previous`.
fn classify_signal(
    previous: PressureLevel,
    value: f64,
    elevated: (f64, f64),
    critical: (f64, f64),
) -> PressureLevel {
    let (elevated_enter, elevated_exit) = elevated;
    let (critical_enter, critical_exit) = critical;
    match previous {
        PressureLevel::Critical if value >= critical_exit => PressureLevel::Critical,
        _ if value >= critical_enter => PressureLevel::Critical,
        PressureLevel::Critical | PressureLevel::Elevated if value >= elevated_exit => {
            PressureLevel::Elevated
        }
        _ if value >= elevated_enter => PressureLevel::Elevated,
        _ => PressureLevel::Normal,
    }
}

/// Deterministic classifier: the max of utilization and PSI levels, each
/// evaluated with its own hysteresis. Missing signals contribute Normal.
pub(crate) fn classify(previous: PressureLevel, sample: &MemorySample) -> PressureLevel {
    let util = sample.utilization().map_or(PressureLevel::Normal, |u| {
        classify_signal(
            previous,
            u,
            (UTIL_ELEVATED_ENTER, UTIL_ELEVATED_EXIT),
            (UTIL_CRITICAL_ENTER, UTIL_CRITICAL_EXIT),
        )
    });
    let psi = sample.some_avg10.map_or(PressureLevel::Normal, |p| {
        classify_signal(
            previous,
            p,
            (PSI_ELEVATED_ENTER, PSI_ELEVATED_EXIT),
            (PSI_CRITICAL_ENTER, PSI_CRITICAL_EXIT),
        )
    });
    util.max(psi)
}

/// Process-wide published level, cheap to read from every worker.
#[derive(Clone, Debug, Default)]
pub(crate) struct SharedPressure(Arc<AtomicU8>);

impl SharedPressure {
    pub(crate) fn level(&self) -> PressureLevel {
        PressureLevel::from_u8(self.0.load(Ordering::Acquire))
    }

    pub(crate) fn store(&self, level: PressureLevel) {
        self.0.store(level as u8, Ordering::Release);
    }
}

/// Periodic sampler and classifier. Owned by exactly one thread.
#[derive(Debug)]
pub(crate) struct PressureMonitor {
    root: Option<PathBuf>,
    statm: PathBuf,
    page_size: u64,
    interval: Duration,
    next: Option<Instant>,
    level: PressureLevel,
    last: Option<MemorySample>,
    shared: SharedPressure,
}

impl PressureMonitor {
    /// Monitor this process using the discovered cgroup v2 directory.
    pub(crate) fn new(shared: SharedPressure) -> Self {
        Self::with_paths(
            discover_cgroup_dir(),
            PathBuf::from(PROC_SELF_STATM),
            shared,
        )
    }

    /// Monitor explicit paths; `root` is the cgroup directory, if known.
    pub(crate) fn with_paths(
        root: Option<PathBuf>,
        statm: PathBuf,
        shared: SharedPressure,
    ) -> Self {
        let interval = if root.is_some() {
            SAMPLE_INTERVAL
        } else {
            UNAVAILABLE_INTERVAL
        };
        shared.store(PressureLevel::Normal);
        Self {
            root,
            statm,
            page_size: page_size(),
            interval,
            next: None,
            level: PressureLevel::Normal,
            last: None,
            shared,
        }
    }

    /// Sample if due. Returns `Some(level)` only when the level changed.
    pub(crate) fn poll(&mut self, now: Instant) -> Option<PressureLevel> {
        if self.next.is_some_and(|next| now < next) {
            return None;
        }
        let sample = read_sample(self.root.as_deref(), &self.statm, self.page_size);
        // Degrade to Normal and back off when no cgroup signal is readable.
        self.interval = if sample.has_cgroup_signal() {
            SAMPLE_INTERVAL
        } else {
            UNAVAILABLE_INTERVAL
        };
        self.next = Some(now + self.interval);
        let level = classify(self.level, &sample);
        self.last = Some(sample);
        self.shared.store(level);
        (level != self.level).then(|| {
            self.level = level;
            level
        })
    }

    pub(crate) fn last_sample(&self) -> Option<&MemorySample> {
        self.last.as_ref()
    }

    pub(crate) fn level(&self) -> PressureLevel {
        self.level
    }

    pub(crate) fn interval(&self) -> Duration {
        self.interval
    }

    pub(crate) fn shared(&self) -> &SharedPressure {
        &self.shared
    }
}

/// Return freed heap pages to the kernel. This is process-wide and walks
/// every arena, so call it rarely: on entering Elevated or Critical and at
/// most once per [`TRIM_INTERVAL`] while pressure persists.
pub(crate) fn trim_allocator() -> bool {
    #[cfg(all(target_os = "linux", target_env = "gnu"))]
    {
        // SAFETY: malloc_trim only inspects allocator state it owns.
        unsafe { libc::malloc_trim(0) != 0 }
    }
    #[cfg(not(all(target_os = "linux", target_env = "gnu")))]
    {
        false
    }
}

/// Work each worker should perform for a level on a pressure tick.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct PressureActions {
    pub(crate) constrain_pools: bool,
    pub(crate) reclaim_idle_buffers: bool,
    pub(crate) clear_idle_connections: bool,
    /// Memory-cache reclaim quanta to run per tick.
    pub(crate) reclaim_cache_quanta: usize,
    pub(crate) trim_allocator: bool,
}

/// Policy table mapping a level to actions.
pub(crate) fn pressure_actions(level: PressureLevel) -> PressureActions {
    let pressured = PressureActions {
        constrain_pools: true,
        reclaim_idle_buffers: true,
        clear_idle_connections: true,
        reclaim_cache_quanta: 0,
        trim_allocator: true,
    };
    match level {
        PressureLevel::Normal => PressureActions::default(),
        PressureLevel::Elevated => pressured,
        PressureLevel::Critical => PressureActions {
            reclaim_cache_quanta: 1,
            ..pressured
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use PressureLevel::{Critical, Elevated, Normal};

    struct Scratch(PathBuf);

    impl Scratch {
        fn new(name: &str) -> Self {
            let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../tmp/racer-pressure-tests")
                .join(format!("{name}-{}", std::process::id()));
            let _ = fs::remove_dir_all(&dir);
            fs::create_dir_all(&dir).unwrap();
            Self(dir)
        }

        fn write(&self, name: &str, text: &str) {
            fs::write(self.0.join(name), text).unwrap();
        }
    }

    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn sample(util: Option<f64>, psi: Option<f64>) -> MemorySample {
        MemorySample {
            current: util.map(|u| (u * 1000.0).round() as u64),
            limit: util.map(|_| 1000),
            some_avg10: psi,
            resident: None,
        }
    }

    #[test]
    fn parses_scalars_and_limits() {
        assert_eq!(parse_u64("12345\n"), Some(12345));
        for bad in ["", "-1", "abc", "1.5", "99999999999999999999999"] {
            assert_eq!(parse_u64(bad), None, "{bad:?}");
        }
        assert_eq!(parse_limit("max\n"), Ok(None));
        assert_eq!(parse_limit("4096\n"), Ok(Some(4096)));
        for bad in ["", "MAX", "1k", "-5"] {
            assert_eq!(parse_limit(bad), Err(()), "{bad:?}");
        }
    }

    #[test]
    fn parses_psi_some_avg10() {
        let full = "some avg10=1.23 avg60=0.50 avg300=0.10 total=123\n\
                    full avg10=0.50 avg60=0.10 avg300=0.00 total=45\n";
        assert_eq!(parse_psi_some_avg10(full), Some(1.23));
        assert_eq!(
            parse_psi_some_avg10("some avg10=42.00 avg60=0 avg300=0 total=0"),
            Some(42.0)
        );
        for bad in [
            "",
            "full avg10=1.00 avg60=0 avg300=0 total=0",
            "some avg60=1.00",
            "some avg10=nope",
            "some avg10=-1",
            "some avg10=NaN",
            "some avg10=inf",
        ] {
            assert_eq!(parse_psi_some_avg10(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn parses_statm() {
        assert_eq!(
            parse_statm_resident("100 25 3 4 0 5 0\n", 4096),
            Some(25 * 4096)
        );
        assert_eq!(parse_statm_resident("100", 4096), None);
        assert_eq!(parse_statm_resident("100 x", 4096), None);
        assert_eq!(parse_statm_resident(&format!("1 {}", u64::MAX), 4096), None);
        assert!(page_size() >= 4096);
    }

    #[test]
    fn discovers_unified_cgroup_path() {
        let mount = Path::new("/mnt/cg");
        assert_eq!(
            cgroup_dir_from("0::/kubepods/pod1/c1\n", mount),
            Some(PathBuf::from("/mnt/cg/kubepods/pod1/c1"))
        );
        assert_eq!(
            cgroup_dir_from("0::/\n", mount),
            Some(PathBuf::from("/mnt/cg/"))
        );
        assert_eq!(
            cgroup_dir_from("12:memory:/x\n0::/a\n", mount),
            Some(PathBuf::from("/mnt/cg/a"))
        );
        assert_eq!(cgroup_dir_from("12:memory:/x\n", mount), None);
        assert_eq!(cgroup_dir_from("0::relative\n", mount), None);
        assert_eq!(cgroup_dir_from("0::/a/../../etc\n", mount), None);
        assert_eq!(cgroup_dir_from("", mount), None);
    }

    #[test]
    fn utilization_hysteresis() {
        assert_eq!(classify(Normal, &sample(Some(0.79), None)), Normal);
        assert_eq!(classify(Normal, &sample(Some(0.80), None)), Elevated);
        assert_eq!(classify(Elevated, &sample(Some(0.70), None)), Elevated);
        assert_eq!(classify(Elevated, &sample(Some(0.69), None)), Normal);
        assert_eq!(classify(Normal, &sample(Some(0.90), None)), Critical);
        assert_eq!(classify(Elevated, &sample(Some(0.90), None)), Critical);
        assert_eq!(classify(Critical, &sample(Some(0.85), None)), Critical);
        assert_eq!(classify(Critical, &sample(Some(0.84), None)), Elevated);
        assert_eq!(classify(Critical, &sample(Some(0.10), None)), Normal);
        assert_eq!(classify(Normal, &sample(Some(1.5), None)), Critical);
    }

    #[test]
    fn psi_hysteresis() {
        assert_eq!(classify(Normal, &sample(None, Some(9.99))), Normal);
        assert_eq!(classify(Normal, &sample(None, Some(10.0))), Elevated);
        assert_eq!(classify(Elevated, &sample(None, Some(5.0))), Elevated);
        assert_eq!(classify(Elevated, &sample(None, Some(4.9))), Normal);
        assert_eq!(classify(Normal, &sample(None, Some(40.0))), Critical);
        assert_eq!(classify(Critical, &sample(None, Some(20.0))), Critical);
        assert_eq!(classify(Critical, &sample(None, Some(19.9))), Elevated);
    }

    #[test]
    fn combined_signals_take_max_and_missing_is_normal() {
        assert_eq!(classify(Normal, &sample(Some(0.5), Some(45.0))), Critical);
        assert_eq!(classify(Normal, &sample(Some(0.95), Some(0.0))), Critical);
        assert_eq!(classify(Critical, &sample(None, None)), Normal);
        let no_limit = MemorySample {
            current: Some(1 << 40),
            ..MemorySample::default()
        };
        assert_eq!(classify(Elevated, &no_limit), Normal);
        let zero_limit = MemorySample {
            current: Some(1),
            limit: Some(0),
            ..MemorySample::default()
        };
        assert_eq!(classify(Normal, &zero_limit), Normal);
    }

    #[test]
    fn shared_pressure_round_trips() {
        let shared = SharedPressure::default();
        assert_eq!(shared.level(), Normal);
        let clone = shared.clone();
        clone.store(Critical);
        assert_eq!(shared.level(), Critical);
        assert_eq!(PressureLevel::from_u8(200), Critical);
    }

    #[test]
    fn reads_sample_with_min_limit() {
        let dir = Scratch::new("sample");
        dir.write("memory.current", "800\n");
        dir.write("memory.high", "1000\n");
        dir.write("memory.max", "2000\n");
        dir.write(
            "memory.pressure",
            "some avg10=3.00 avg60=0 avg300=0 total=1\n",
        );
        dir.write("statm", "10 2 0 0 0 0 0\n");
        let s = read_sample(Some(&dir.0), &dir.0.join("statm"), 4096);
        assert_eq!(s.current, Some(800));
        assert_eq!(s.limit, Some(1000));
        assert_eq!(s.some_avg10, Some(3.0));
        assert_eq!(s.resident, Some(8192));
        dir.write("memory.high", "max\n");
        assert_eq!(
            read_sample(Some(&dir.0), &dir.0.join("statm"), 4096).limit,
            Some(2000)
        );
        dir.write("memory.max", "max\n");
        assert_eq!(
            read_sample(Some(&dir.0), &dir.0.join("statm"), 4096).limit,
            None
        );
        dir.write("memory.max", "garbage\n");
        assert_eq!(
            read_sample(Some(&dir.0), &dir.0.join("statm"), 4096).limit,
            None
        );
    }

    #[test]
    fn monitor_rate_limits_and_reports_changes() {
        let dir = Scratch::new("monitor");
        dir.write("memory.current", "500\n");
        dir.write("memory.max", "1000\n");
        let shared = SharedPressure::default();
        let mut monitor =
            PressureMonitor::with_paths(Some(dir.0.clone()), dir.0.join("statm"), shared.clone());
        let t0 = Instant::now();
        assert_eq!(monitor.poll(t0), None);
        assert_eq!(monitor.last_sample().unwrap().current, Some(500));
        dir.write("memory.current", "950\n");
        // Not due yet: no read, no change.
        assert_eq!(monitor.poll(t0 + Duration::from_millis(500)), None);
        assert_eq!(monitor.level(), Normal);
        assert_eq!(monitor.poll(t0 + SAMPLE_INTERVAL), Some(Critical));
        assert_eq!(shared.level(), Critical);
        assert_eq!(monitor.poll(t0 + SAMPLE_INTERVAL * 2), None);
        dir.write("memory.current", "800\n");
        assert_eq!(monitor.poll(t0 + SAMPLE_INTERVAL * 3), Some(Elevated));
        assert_eq!(monitor.shared().level(), Elevated);
        assert_eq!(monitor.interval(), SAMPLE_INTERVAL);
    }

    #[test]
    fn monitor_degrades_and_backs_off_when_unavailable() {
        let dir = Scratch::new("missing");
        let shared = SharedPressure::default();
        shared.store(Critical);
        let mut monitor = PressureMonitor::with_paths(
            Some(dir.0.join("absent")),
            dir.0.join("statm"),
            shared.clone(),
        );
        assert_eq!(shared.level(), Normal);
        let t0 = Instant::now();
        assert_eq!(monitor.poll(t0), None);
        assert_eq!(monitor.level(), Normal);
        assert_eq!(monitor.interval(), UNAVAILABLE_INTERVAL);
        assert_eq!(monitor.poll(t0 + SAMPLE_INTERVAL * 5), None);
        let mut none = PressureMonitor::with_paths(None, dir.0.join("statm"), shared);
        assert_eq!(none.interval(), UNAVAILABLE_INTERVAL);
        assert_eq!(none.poll(t0), None);
        assert_eq!(none.last_sample().unwrap(), &MemorySample::default());
    }

    #[test]
    fn live_discovery_does_not_panic() {
        let mut monitor = PressureMonitor::new(SharedPressure::default());
        let _ = monitor.poll(Instant::now());
        assert!(monitor.last_sample().is_some());
    }

    #[test]
    fn trim_allocator_is_callable() {
        let trimmed = trim_allocator();
        if !cfg!(all(target_os = "linux", target_env = "gnu")) {
            assert!(!trimmed);
        }
    }

    #[test]
    fn action_policy() {
        assert_eq!(pressure_actions(Normal), PressureActions::default());
        let elevated = pressure_actions(Elevated);
        assert!(elevated.constrain_pools && elevated.reclaim_idle_buffers);
        assert!(elevated.clear_idle_connections && elevated.trim_allocator);
        assert_eq!(elevated.reclaim_cache_quanta, 0);
        assert_eq!(
            pressure_actions(Critical),
            PressureActions {
                reclaim_cache_quanta: 1,
                ..elevated
            }
        );
        assert!(Normal < Elevated && Elevated < Critical);
    }
}
