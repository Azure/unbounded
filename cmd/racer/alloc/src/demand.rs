//! Deterministic demand-driven sizing for small idle buffer pools.
//!
//! [`PoolDemand`] owns no buffers. Its owner reports hits, misses, and return
//! attempts, then calls [`PoolDemand::tick`] from a periodic maintenance path.
//! Each elapsed window may raise the target depth after misses coincided with
//! full-pool rejections, release half of the entries that stayed idle for the
//! whole window, and slowly decay the target after sustained miss-free windows.
//! The owner releases the returned number of entries, oldest first.

use std::time::{Duration, Instant};

/// Smallest target depth by default; an idle pool may become empty.
pub const DEFAULT_MIN_DEPTH: usize = 0;

/// Largest target depth by default, matching the former fixed pool depth.
pub const DEFAULT_MAX_DEPTH: usize = 8;

/// Initial target depth before any demand has been observed.
pub const DEFAULT_INITIAL_TARGET: usize = 2;

/// Length of one observation window.
pub const DEFAULT_WINDOW: Duration = Duration::from_secs(1);

/// Consecutive miss-free windows before the target decays by one.
pub const DEFAULT_DECAY_WINDOWS: u32 = 10;

/// Point-in-time pool counters for telemetry. Counters are cumulative.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct PoolStats {
    /// Idle entries currently retained.
    pub entries: usize,
    /// Idle bytes currently retained.
    pub bytes: usize,
    /// Current demand-driven target depth.
    pub target: usize,
    /// Upper bound on the target depth.
    pub max_depth: usize,
    /// Checkouts served from the pool.
    pub hits: u64,
    /// Eligible checkouts that had to allocate.
    pub misses: u64,
    /// Returns the pool refused.
    pub rejects: u64,
    /// Entries released by maintenance, pressure, or explicit reclaim.
    pub trimmed: u64,
    /// Whether the pool is held at its minimum depth.
    pub constrained: bool,
}

/// Window-based controller for one idle pool's target depth.
#[derive(Clone, Debug)]
pub struct PoolDemand {
    min_depth: usize,
    max_depth: usize,
    target: usize,
    window: Duration,
    decay_windows: u32,
    window_start: Option<Instant>,
    misses: u64,
    hits: u64,
    rejects_full: u64,
    low_water: Option<usize>,
    idle_windows: u32,
    constrained: bool,
    hits_total: u64,
    misses_total: u64,
    rejects_total: u64,
    trimmed_total: u64,
}

impl Default for PoolDemand {
    fn default() -> Self {
        Self::new()
    }
}

impl PoolDemand {
    /// Controller with the default bounds, window, and decay.
    #[must_use]
    pub fn new() -> Self {
        Self {
            min_depth: DEFAULT_MIN_DEPTH,
            max_depth: DEFAULT_MAX_DEPTH,
            target: DEFAULT_INITIAL_TARGET,
            window: DEFAULT_WINDOW,
            decay_windows: DEFAULT_DECAY_WINDOWS,
            window_start: None,
            misses: 0,
            hits: 0,
            rejects_full: 0,
            low_water: None,
            idle_windows: 0,
            constrained: false,
            hits_total: 0,
            misses_total: 0,
            rejects_total: 0,
            trimmed_total: 0,
        }
    }

    /// Set depth bounds; `max` is raised to `min` and the target is clamped.
    #[must_use]
    pub fn with_bounds(mut self, min: usize, max: usize) -> Self {
        self.min_depth = min;
        self.max_depth = max.max(min);
        self.target = self.target.clamp(self.min_depth, self.max_depth);
        self
    }

    /// Set the current target, clamped to the depth bounds.
    #[must_use]
    pub fn with_target(mut self, target: usize) -> Self {
        self.target = target.clamp(self.min_depth, self.max_depth);
        self
    }

    /// Set the observation window; zero is raised to one nanosecond.
    #[must_use]
    pub fn with_window(mut self, window: Duration) -> Self {
        self.window = window.max(Duration::from_nanos(1));
        self
    }

    /// Set the miss-free windows required before the target decays (at least 1).
    #[must_use]
    pub fn with_decay_windows(mut self, windows: u32) -> Self {
        self.decay_windows = windows.max(1);
        self
    }

    /// Current target depth.
    #[must_use]
    pub fn target(&self) -> usize {
        self.target
    }

    /// Upper bound on the target depth.
    #[must_use]
    pub fn max_depth(&self) -> usize {
        self.max_depth
    }

    /// Whether the pool is held at its minimum depth.
    #[must_use]
    pub fn constrained(&self) -> bool {
        self.constrained
    }

    /// Record a checkout served from the pool, leaving `len_after` entries.
    pub fn record_hit(&mut self, len_after: usize) {
        self.hits += 1;
        self.hits_total += 1;
        self.observe_len(len_after);
    }

    /// Record an eligible checkout that the pool could not serve.
    pub fn record_miss(&mut self) {
        self.misses += 1;
        self.misses_total += 1;
    }

    /// Record a return refused by an owner-specific budget.
    pub fn record_reject(&mut self) {
        self.rejects_total += 1;
    }

    /// Record entries released outside [`Self::tick`], such as explicit reclaim.
    pub fn record_trim(&mut self, entries: usize, len_after: usize) {
        self.trimmed_total += entries as u64;
        self.observe_len(len_after);
    }

    fn observe_len(&mut self, len: usize) {
        self.low_water = Some(self.low_water.map_or(len, |low| low.min(len)));
    }

    /// Decide whether a return of `capacity` bytes may join a pool of `len`
    /// entries holding `bytes` bytes under a `budget` byte limit.
    pub fn admit(&mut self, len: usize, bytes: usize, capacity: usize, budget: usize) -> bool {
        if self.constrained || len >= self.target {
            self.rejects_total += 1;
            if !self.constrained {
                self.rejects_full += 1;
            }
            return false;
        }
        if capacity > budget.saturating_sub(bytes) {
            self.rejects_total += 1;
            return false;
        }
        true
    }

    /// Hold the pool at its minimum depth and refuse returns while constrained.
    pub fn set_constrained(&mut self, constrained: bool) {
        self.constrained = constrained;
    }

    /// Advance at most one window and return how many of `len` entries to release.
    ///
    /// The first call only starts the window. Calls before the window elapses,
    /// including those with an earlier `now`, return zero unless constrained.
    pub fn tick(&mut self, now: Instant, len: usize) -> usize {
        if self.constrained {
            let release = len.saturating_sub(self.min_depth);
            self.trimmed_total += release as u64;
            return release;
        }
        let Some(start) = self.window_start else {
            self.window_start = Some(now);
            self.reset_window(len);
            return 0;
        };
        let elapsed = now.checked_duration_since(start).unwrap_or_default();
        if elapsed < self.window {
            return 0;
        }
        if self.misses > 0 && self.rejects_full > 0 {
            let step = usize::try_from(self.misses / 2)
                .unwrap_or(usize::MAX)
                .max(1);
            self.target = self.target.saturating_add(step).min(self.max_depth);
            self.idle_windows = 0;
        } else if self.misses == 0 && !self.fully_used(len) {
            self.idle_windows += 1;
            if self.idle_windows >= self.decay_windows {
                self.target = self.target.saturating_sub(1).max(self.min_depth);
                self.idle_windows = 0;
            }
        } else if self.misses == 0 {
            // Every entry was checked out during the window: nothing sat idle,
            // so the target holds without counting toward decay.
        } else {
            self.idle_windows = 0;
        }
        let low = self.low_water.unwrap_or(len).min(len);
        let surplus = low.div_ceil(2);
        let above = len.saturating_sub(self.target);
        let release = surplus.max(above).min(len.saturating_sub(self.min_depth));
        self.trimmed_total += release as u64;
        self.window_start = Some(now);
        self.reset_window(len - release);
        release
    }

    /// Whether the window served checkouts and drained the pool at some point.
    fn fully_used(&self, len: usize) -> bool {
        self.hits > 0 && self.low_water.unwrap_or(len).min(len) == 0
    }

    fn reset_window(&mut self, len: usize) {
        self.misses = 0;
        self.hits = 0;
        self.rejects_full = 0;
        self.low_water = Some(len);
    }

    /// Counters combined with the owner's current `entries` and `bytes`.
    #[must_use]
    pub fn stats(&self, entries: usize, bytes: usize) -> PoolStats {
        PoolStats {
            entries,
            bytes,
            target: self.target,
            max_depth: self.max_depth,
            hits: self.hits_total,
            misses: self.misses_total,
            rejects: self.rejects_total,
            trimmed: self.trimmed_total,
            constrained: self.constrained,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const W: Duration = DEFAULT_WINDOW;

    #[test]
    fn first_tick_only_initializes() {
        let mut demand = PoolDemand::new();
        let now = Instant::now();
        assert_eq!(demand.tick(now, 2), 0);
        assert_eq!(demand.tick(now + W / 2, 2), 0);
        assert_eq!(demand.target(), DEFAULT_INITIAL_TARGET);
    }

    #[test]
    fn defaults_match_constants() {
        let stats = PoolDemand::default().stats(0, 0);
        assert_eq!(stats.target, DEFAULT_INITIAL_TARGET);
        assert_eq!(stats.max_depth, DEFAULT_MAX_DEPTH);
        assert!(!stats.constrained);
    }

    #[test]
    fn burst_raises_target_within_one_window() {
        let mut demand = PoolDemand::new();
        let start = Instant::now();
        demand.tick(start, 0);
        for _ in 0..6 {
            demand.record_miss();
        }
        assert!(demand.admit(0, 0, 1, 100));
        assert!(demand.admit(1, 1, 1, 100));
        assert!(!demand.admit(2, 2, 1, 100));
        assert_eq!(demand.tick(start + W, 2), 0);
        assert_eq!(demand.target(), 2 + 3);
        assert!(demand.admit(2, 2, 1, 100));
    }

    #[test]
    fn misses_without_full_rejects_do_not_grow() {
        let mut demand = PoolDemand::new();
        let start = Instant::now();
        demand.tick(start, 0);
        demand.record_miss();
        demand.tick(start + W, 0);
        assert_eq!(demand.target(), DEFAULT_INITIAL_TARGET);
    }

    #[test]
    fn idle_windows_halve_surplus_then_decay_to_min() {
        let mut demand = PoolDemand::new().with_target(8);
        let mut now = Instant::now();
        let mut len = 8;
        demand.tick(now, len);
        let mut released = Vec::new();
        for _ in 0..200 {
            now += W;
            let n = demand.tick(now, len);
            len -= n;
            released.push(n);
        }
        assert_eq!(&released[..4], &[4, 2, 1, 1]);
        assert_eq!(len, 0);
        assert_eq!(demand.target(), DEFAULT_MIN_DEPTH);
        assert_eq!(demand.stats(len, 0).trimmed, 8);
    }

    #[test]
    fn decay_waits_for_consecutive_idle_windows() {
        let mut demand = PoolDemand::new().with_decay_windows(3);
        let mut now = Instant::now();
        demand.tick(now, 0);
        for _ in 0..2 {
            now += W;
            demand.tick(now, 0);
        }
        assert_eq!(demand.target(), 2);
        now += W;
        demand.tick(now, 0);
        assert_eq!(demand.target(), 1);
    }

    #[test]
    fn used_entries_are_not_trimmed() {
        let mut demand = PoolDemand::new();
        let start = Instant::now();
        demand.tick(start, 2);
        demand.record_hit(0);
        assert_eq!(demand.tick(start + W, 2), 0);
    }

    #[test]
    fn fully_used_windows_do_not_decay() {
        let mut demand = PoolDemand::new().with_bounds(0, 8).with_target(8);
        let start = Instant::now();
        demand.tick(start, 8);
        for window in 1..=(DEFAULT_DECAY_WINDOWS * 3) {
            demand.record_hit(0);
            assert_eq!(demand.tick(start + W * window, 8), 0);
        }
        assert_eq!(demand.target(), 8);
        // Windows with no checkouts still decay.
        let base = start + W * (DEFAULT_DECAY_WINDOWS * 3);
        for window in 1..=DEFAULT_DECAY_WINDOWS {
            demand.tick(base + W * window, 0);
        }
        assert_eq!(demand.target(), 7);
    }

    #[test]
    fn bounds_are_respected() {
        let mut demand = PoolDemand::new().with_bounds(1, 3).with_target(9);
        assert_eq!(demand.target(), 3);
        let mut now = Instant::now();
        demand.tick(now, 3);
        for _ in 0..5 {
            for _ in 0..100 {
                demand.record_miss();
            }
            demand.admit(3, 0, 1, 10);
            now += W;
            demand.tick(now, 3);
            assert_eq!(demand.target(), 3);
        }
        let mut len = 3;
        for _ in 0..100 {
            now += W;
            len -= demand.tick(now, len);
            assert!(demand.target() >= 1);
        }
        assert_eq!(len, 1);
        assert_eq!(demand.target(), 1);
        let demand = PoolDemand::new().with_bounds(5, 2);
        assert_eq!((demand.target(), demand.max_depth()), (5, 5));
    }

    #[test]
    fn entries_above_target_are_released() {
        let mut demand = PoolDemand::new().with_target(2);
        let start = Instant::now();
        demand.tick(start, 0);
        demand.record_hit(0);
        assert_eq!(demand.tick(start + W, 6), 4);
    }

    #[test]
    fn constrained_releases_to_min_and_rejects() {
        let mut demand = PoolDemand::new().with_bounds(1, 8).with_target(8);
        demand.set_constrained(true);
        assert!(!demand.admit(0, 0, 1, 100));
        assert_eq!(demand.tick(Instant::now(), 5), 4);
        demand.set_constrained(false);
        assert!(demand.admit(0, 0, 1, 100));
        let stats = demand.stats(1, 0);
        assert_eq!((stats.rejects, stats.trimmed), (1, 4));
    }

    #[test]
    fn budget_rejects_do_not_drive_growth() {
        let mut demand = PoolDemand::new();
        let start = Instant::now();
        demand.tick(start, 0);
        demand.record_miss();
        assert!(!demand.admit(0, 90, 20, 100));
        demand.tick(start + W, 0);
        assert_eq!(demand.target(), 2);
        assert_eq!(demand.stats(0, 0).rejects, 1);
    }

    #[test]
    fn backwards_time_is_ignored() {
        let mut demand = PoolDemand::new().with_target(4);
        let start = Instant::now() + W * 10;
        demand.tick(start, 4);
        assert_eq!(demand.tick(start - W * 5, 4), 0);
        assert_eq!(demand.tick(start + W, 4), 2);
    }
}
