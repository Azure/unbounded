//! Demand-driven provisioning for pools of reusable buffers.
//!
//! [`Provision`] owns no buffers. Its owner reports how many items are in use
//! (checked out or waited for), demand it could not observe as in use, and how
//! long acquiring new capacity took, then calls [`Provision::tick`] from a
//! periodic maintenance path. The returned [`Plan`] says whether to acquire
//! more units, release some, or hold.
//!
//! The controller tracks demand, not idle time. Its target is the recent
//! peak, held for the hold time `H`, or a typical level if that is higher,
//! plus a look-ahead while demand keeps rising and a correction for demand the
//! pool could not observe, which decays over `H`. The typical level is the
//! smoothed mean plus two smoothed mean deviations of the per-window peaks,
//! capped at the largest peak of the last `4 x H`, so it keeps capacity for
//! demand that recurs a little slower than `H` without ever provisioning more
//! than was recently used. `H` is derived from the measured cost of acquiring
//! a unit, so a pool whose units are expensive to acquire, such as registered
//! RDMA memory, keeps surplus for longer than one whose units are cheap to
//! map. Capacity grows as soon as the target exceeds it. It shrinks only once
//! the target falls below it, by half the surplus per step, with steps spaced
//! `H / 8` apart and never within one step of growth or a shortfall, so demand
//! that recurs within `H` causes no acquire and release churn.
//!
//! Memory pressure shortens `H` to a quarter, freezes the correction, and caps
//! the target one unit above the current in-use level. Critical pressure caps
//! it at the in-use level and releases all surplus at once. All arithmetic is
//! integer, so the controller is deterministic for a given sequence of inputs
//! and times.

use std::time::{Duration, Instant};

/// Interval between samples used for the look-ahead and correction decay.
pub const SAMPLE_PERIOD: Duration = Duration::from_millis(100);

/// Shortest hold time, used when acquiring a unit is nearly free.
pub const MIN_HOLD: Duration = Duration::from_secs(1);

/// Longest hold time, used when acquiring a unit is very expensive.
pub const MAX_HOLD: Duration = Duration::from_secs(300);

/// Hold time per unit of acquire time. Holding surplus for this many acquire
/// times bounds the time spent re-acquiring released capacity to roughly 0.1%.
const HOLD_FACTOR: u64 = 1000;

/// Windows per hold time; the recent peak spans this many windows.
const WINDOWS: usize = 8;

/// Windows retained for the cap on the typical level, four hold times.
const HISTORY: usize = 4 * WINDOWS;

/// Fixed-point scale for the smoothed estimators.
const SCALE: i128 = 256;

/// Gains, as right shifts: mean 1/8, deviation 1/4, acquire time 1/4.
const MEAN_SHIFT: u32 = 3;
const DEVIATION_SHIFT: u32 = 2;
const ACQUIRE_SHIFT: u32 = 2;

/// Deviations above the mean that the typical level covers.
const DEVIATIONS: i128 = 2;

/// Largest look-ahead while demand rises, in units.
const LEAD_UNITS: usize = 2;

/// Most missed windows or samples replayed into the estimators after a long
/// gap between ticks. The estimators have converged by then. The correction
/// still decays over the whole gap.
const CATCH_UP: u64 = 64;

/// Point-in-time pool counters for telemetry. Counters are cumulative.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct PoolStats {
    /// Idle entries currently retained and ready for reuse.
    pub entries: usize,
    /// Idle bytes currently retained and ready for reuse.
    pub bytes: usize,
    /// Bytes currently checked out of the pool.
    pub in_use: usize,
    /// Bytes the controllers currently want provisioned, in use plus idle.
    pub target: usize,
    /// Idle bytes released to the kernel but still mapped for reuse.
    pub cold: usize,
    /// Longest current hold time across the pool's controllers.
    pub hold: Duration,
    /// Checkouts served from ready idle entries.
    pub hits: u64,
    /// Checkouts that had to allocate or fault in memory.
    pub misses: u64,
    /// Checkouts served from cold entries, which fault their pages back in.
    pub cold_hits: u64,
    /// Returns the pool refused.
    pub rejects: u64,
    /// Entries released by maintenance, pressure, or explicit reclaim.
    pub trimmed: u64,
    /// Whether memory pressure currently constrains the pool.
    pub constrained: bool,
    /// Checkouts that could not be pooled (no size class was available).
    pub unpooled: u64,
}

/// Bounds and initial estimates for one [`Provision`] controller. Quantities
/// are in items, the owner's unit of reuse (for example, buffers).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ProvisionConfig {
    /// Items acquired or released together; raised to at least 1.
    pub unit: usize,
    /// Items kept provisioned regardless of demand.
    pub floor: usize,
    /// Most items ever provisioned; the floor is lowered to it if needed.
    pub ceiling: usize,
    /// Expected time to acquire one unit before any acquisition is measured.
    pub acquire: Duration,
}

/// Memory pressure as seen by a pool controller.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Pressure {
    /// Provision for demand.
    #[default]
    Normal,
    /// Keep at most one unit above the in-use level and release surplus sooner.
    Elevated,
    /// Keep nothing above the in-use level and release all surplus now.
    Critical,
}

/// Action requested by [`Provision::tick`], in units.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Plan {
    /// Keep the current capacity.
    Hold,
    /// Acquire this many more units.
    Grow(usize),
    /// Release this many idle units.
    Release(usize),
}

/// Demand-driven target for one pool's provisioned capacity.
#[derive(Clone, Debug)]
pub struct Provision {
    config: ProvisionConfig,
    pressure: Pressure,
    saturated: bool,
    acquire: u64,
    in_use: usize,
    sample_peak: usize,
    tick_peak: usize,
    next_sample: Option<Instant>,
    previous: usize,
    rises: [usize; 2],
    mean: i128,
    deviation: i128,
    correction: i128,
    windows: [usize; HISTORY],
    window: usize,
    window_start: Option<Instant>,
    last_change: Option<Instant>,
    last_release: Option<Instant>,
    target: usize,
}

impl Provision {
    /// Controller with no observed demand; its target starts at the floor.
    /// Its clock starts at the first [`Provision::tick`], so it follows
    /// whatever clock the owner passes in.
    #[must_use]
    pub fn new(config: ProvisionConfig) -> Self {
        let config = ProvisionConfig {
            unit: config.unit.max(1),
            floor: config.floor.min(config.ceiling),
            ..config
        };
        Self {
            config,
            pressure: Pressure::Normal,
            saturated: false,
            acquire: nanos(config.acquire),
            in_use: 0,
            sample_peak: 0,
            tick_peak: 0,
            next_sample: None,
            previous: 0,
            rises: [0; 2],
            mean: 0,
            deviation: 0,
            correction: 0,
            windows: [0; HISTORY],
            window: 0,
            window_start: None,
            last_change: None,
            last_release: None,
            target: config.floor,
        }
    }

    /// Bounds this controller was built with, after normalization.
    #[must_use]
    pub fn config(&self) -> ProvisionConfig {
        self.config
    }

    /// Items the controller currently wants provisioned.
    #[must_use]
    pub fn target(&self) -> usize {
        self.target
    }

    /// Most recently observed in-use level.
    #[must_use]
    pub fn in_use(&self) -> usize {
        self.in_use
    }

    /// Smoothed time to acquire one unit.
    #[must_use]
    pub fn acquire(&self) -> Duration {
        Duration::from_nanos(self.acquire)
    }

    /// Time surplus capacity is kept after demand falls, without pressure.
    #[must_use]
    pub fn hold(&self) -> Duration {
        let hold = self.acquire.saturating_mul(HOLD_FACTOR);
        Duration::from_nanos(hold.clamp(nanos(MIN_HOLD), nanos(MAX_HOLD)))
    }

    /// Hold time under the current pressure.
    #[must_use]
    pub fn effective_hold(&self) -> Duration {
        match self.pressure {
            Pressure::Normal => self.hold(),
            Pressure::Elevated | Pressure::Critical => self.hold() / 4,
        }
    }

    /// Record the current number of items in use, including demand waiting
    /// for an item. Cheap enough for every checkout.
    pub fn observe(&mut self, in_use: usize) {
        self.in_use = in_use;
        self.sample_peak = self.sample_peak.max(in_use);
        self.tick_peak = self.tick_peak.max(in_use);
        let window = &mut self.windows[self.window];
        *window = (*window).max(in_use);
    }

    /// Record `items` of demand the pool turned away without it ever showing
    /// up as in use, such as a request refused because the pool was empty.
    /// The correction is frozen while the pool is saturated or pressured,
    /// because more capacity could not have helped.
    pub fn shortfall(&mut self, now: Instant, items: usize) {
        self.last_change = Some(now);
        if self.saturated || self.pressure != Pressure::Normal {
            return;
        }
        self.correction = self
            .correction
            .saturating_add((items as i128).saturating_mul(SCALE));
        self.retarget();
    }

    /// Record that acquiring `units` units took `took`.
    pub fn acquired(&mut self, now: Instant, units: usize, took: Duration) {
        self.last_change = Some(now);
        if units == 0 {
            return;
        }
        let sample = nanos(took) / units as u64;
        self.acquire = self.acquire - (self.acquire >> ACQUIRE_SHIFT) + (sample >> ACQUIRE_SHIFT);
    }

    /// Mark whether the pool cannot grow for reasons outside the controller,
    /// such as an admission budget held by other users.
    pub fn set_saturated(&mut self, saturated: bool) {
        self.saturated = saturated;
    }

    /// Advance time and decide what to do with `capacity` provisioned items
    /// and `pending` items being acquired. Growth is withheld while an
    /// acquisition is pending. The owner applies the plan and reports the
    /// result through [`Self::acquired`] and the next `capacity`.
    pub fn tick(
        &mut self,
        now: Instant,
        capacity: usize,
        pending: usize,
        pressure: Pressure,
    ) -> Plan {
        if pressure != self.pressure {
            self.pressure = pressure;
            self.last_release = None;
        }
        self.advance(now);
        self.retarget();
        let unit = self.config.unit;
        if pressure == Pressure::Critical {
            let units = capacity.saturating_sub(self.target) / unit;
            if units > 0 {
                self.last_release = Some(now);
                return Plan::Release(units);
            }
            return Plan::Hold;
        }
        let have = capacity.saturating_add(pending);
        if self.target > have {
            if pending > 0 {
                return Plan::Hold;
            }
            let wanted = (self.target - have).div_ceil(unit);
            let room = self.config.ceiling.saturating_sub(have) / unit;
            let units = wanted.min(room);
            if units > 0 {
                self.last_change = Some(now);
                return Plan::Grow(units);
            }
            return Plan::Hold;
        }
        let surplus = capacity.saturating_sub(self.target) / unit;
        if surplus == 0 {
            return Plan::Hold;
        }
        let step = self.effective_hold() / WINDOWS as u32;
        let rested =
            |at: Option<Instant>| at.is_none_or(|at| now.saturating_duration_since(at) >= step);
        if !rested(self.last_change) || !rested(self.last_release) {
            return Plan::Hold;
        }
        self.last_release = Some(now);
        Plan::Release(surplus.div_ceil(2))
    }

    fn advance(&mut self, now: Instant) {
        let fresh = std::mem::take(&mut self.tick_peak);
        let next_sample = *self.next_sample.get_or_insert(now + SAMPLE_PERIOD);
        let window_start = *self.window_start.get_or_insert(now);
        if now >= next_sample {
            let missed = nanos(now.saturating_duration_since(next_sample));
            let samples = (missed / nanos(SAMPLE_PERIOD)).saturating_add(1);
            self.sample(self.sample_peak);
            for _ in 1..samples.min(CATCH_UP) {
                self.sample(self.in_use);
            }
            self.decay(samples);
            self.sample_peak = self.in_use;
            self.next_sample = Some(if samples > CATCH_UP {
                now + SAMPLE_PERIOD
            } else {
                next_sample + SAMPLE_PERIOD * samples as u32
            });
        }
        let width = (self.effective_hold() / WINDOWS as u32).max(Duration::from_millis(1));
        let elapsed = now.saturating_duration_since(window_start);
        if elapsed < width {
            return;
        }
        let steps = nanos(elapsed) / nanos(width);
        for step in 0..steps.min(CATCH_UP) {
            let closed = if step == 0 {
                self.windows[self.window]
            } else {
                self.in_use
            };
            self.settle(closed);
            self.window = (self.window + 1) % HISTORY;
            self.windows[self.window] = self.in_use;
        }
        if steps >= HISTORY as u64 {
            self.windows = [self.in_use; HISTORY];
        }
        // Demand observed since the last tick may have come at any point in
        // the gap, so hold it from now rather than from the start of the gap.
        let newest = &mut self.windows[self.window];
        *newest = (*newest).max(fresh);
        self.window_start = Some(if steps > CATCH_UP {
            now
        } else {
            window_start + width * steps as u32
        });
    }

    /// Fold one closed window's peak into the typical level.
    fn settle(&mut self, peak: usize) {
        let error = (peak as i128).saturating_mul(SCALE) - self.mean;
        self.mean += error >> MEAN_SHIFT;
        self.deviation += (error.abs() - self.deviation) >> DEVIATION_SHIFT;
    }

    /// Fold one sample period's peak into the look-ahead.
    fn sample(&mut self, peak: usize) {
        self.rises = [self.rises[1], peak.saturating_sub(self.previous)];
        self.previous = peak;
    }

    /// Decay the correction over `samples` sample periods. Each period
    /// removes `correction x SAMPLE_PERIOD / H`, at least one scaled unit.
    /// Runs of periods that remove the same amount are applied together, and
    /// after `CATCH_UP` runs the rest of a long gap is applied by
    /// [`Self::gallop`], so the whole gap decays in a bounded number of steps.
    fn decay(&mut self, mut samples: u64) {
        let hold = i128::from(nanos(self.effective_hold()).max(1));
        let period = i128::from(nanos(SAMPLE_PERIOD));
        let mut runs = 0;
        while samples > 0 && self.correction > 0 {
            let per = self.correction.saturating_mul(period) / hold;
            if per == 0 {
                self.correction = (self.correction - i128::from(samples)).max(0);
                return;
            }
            if runs == CATCH_UP {
                samples = self.gallop(samples, hold, period);
                runs += 1;
                continue;
            }
            runs += 1;
            // Smallest correction from which a period still removes `per`.
            let low = (per * hold + period - 1) / period;
            let steps = ((self.correction - low) / per + 1).min(i128::from(samples));
            self.correction -= steps * per;
            samples -= steps as u64;
        }
    }

    /// Apply up to `samples` periods of decay as powers of the per-period
    /// factor `1 - SAMPLE_PERIOD / H`, while a period would still remove at
    /// least `CATCH_UP` scaled units, then return the periods left. Rounding
    /// down makes this decay at least as fast as period by period, and the
    /// runs left before the correction falls below that level are few.
    fn gallop(&mut self, mut samples: u64, hold: i128, period: i128) -> u64 {
        const SHIFT: u32 = 32;
        let keep = (((hold - period).max(0) as u128) << SHIFT) / hold as u128;
        let mut powers = [keep; u64::BITS as usize];
        for bit in 1..powers.len() {
            powers[bit] = (powers[bit - 1] * powers[bit - 1]) >> SHIFT;
        }
        for (bit, &power) in powers.iter().enumerate().rev() {
            let span = 1 << bit;
            if span > samples {
                continue;
            }
            let correction = self.correction as u128;
            let mask = (1 << SHIFT) - 1;
            let next =
                ((correction >> SHIFT) * power + (((correction & mask) * power) >> SHIFT)) as i128;
            if next.saturating_mul(period) / hold < i128::from(CATCH_UP) {
                continue;
            }
            self.correction = next;
            samples -= span;
        }
        samples
    }

    fn retarget(&mut self) {
        let config = self.config;
        let recent = (0..WINDOWS)
            .map(|age| self.windows[(self.window + HISTORY - age) % HISTORY])
            .max()
            .unwrap_or(0)
            .max(self.in_use);
        let longest = self.windows.iter().copied().max().unwrap_or(0);
        let typical = to_items(self.mean + DEVIATIONS * self.deviation).min(longest);
        let base = recent.max(typical);
        // Look ahead only while demand has risen for two samples in a row,
        // by the smaller rise, over the time it takes to acquire a unit.
        let rise = self.rises[0].min(self.rises[1]);
        let horizon = self.acquire.saturating_add(nanos(SAMPLE_PERIOD));
        let lead = u64::try_from(rise)
            .unwrap_or(u64::MAX)
            .saturating_mul(horizon.div_ceil(nanos(SAMPLE_PERIOD)));
        let lead = usize::try_from(lead)
            .unwrap_or(usize::MAX)
            .min(LEAD_UNITS.saturating_mul(config.unit));
        let room = config.ceiling.saturating_sub(base);
        self.correction = self.correction.min((room as i128).saturating_mul(SCALE));
        let ceiling = match self.pressure {
            Pressure::Normal => config.ceiling,
            Pressure::Elevated => self.in_use.saturating_add(config.unit).min(config.ceiling),
            Pressure::Critical => self.in_use.min(config.ceiling),
        }
        .max(config.floor);
        self.target = base
            .saturating_add(lead)
            .saturating_add(usize::try_from(self.correction / SCALE).unwrap_or(usize::MAX))
            .clamp(config.floor, ceiling);
    }
}

fn nanos(duration: Duration) -> u64 {
    u64::try_from(duration.as_nanos()).unwrap_or(u64::MAX)
}

/// Round a scaled estimate up to whole items.
fn to_items(scaled: i128) -> usize {
    if scaled <= 0 {
        return 0;
    }
    usize::try_from((scaled + SCALE - 1) / SCALE).unwrap_or(usize::MAX)
}

#[cfg(test)]
mod tests {
    use super::*;

    const HOST: ProvisionConfig = ProvisionConfig {
        unit: 1,
        floor: 0,
        ceiling: 64,
        acquire: Duration::from_millis(2),
    };

    /// Pool model that applies every plan at once.
    struct Model {
        provision: Provision,
        start: Instant,
        now: Instant,
        capacity: usize,
        grown: usize,
        released: usize,
    }

    impl Model {
        fn new(config: ProvisionConfig) -> Self {
            let start = Instant::now();
            Self {
                provision: Provision::new(config),
                start,
                now: start,
                capacity: config.floor,
                grown: 0,
                released: 0,
            }
        }

        /// Hold `in_use` for `duration`, ticking every 100 ms. A level above
        /// the capacity is served by growing at once, like an allocation miss.
        fn run(&mut self, in_use: usize, duration: Duration, pressure: Pressure) {
            let end = self.now + duration;
            while self.now < end {
                self.now += SAMPLE_PERIOD;
                self.provision.observe(in_use);
                if in_use > self.capacity {
                    self.grown += in_use - self.capacity;
                    self.capacity = in_use;
                }
                self.step(pressure);
            }
        }

        fn step(&mut self, pressure: Pressure) {
            let unit = self.provision.config().unit;
            match self.provision.tick(self.now, self.capacity, 0, pressure) {
                Plan::Hold => {}
                Plan::Grow(units) => {
                    self.capacity += units * unit;
                    self.grown += units * unit;
                    self.provision
                        .acquired(self.now, units, self.provision.acquire());
                }
                Plan::Release(units) => {
                    assert!(self.capacity >= units * unit + self.provision.in_use());
                    self.capacity -= units * unit;
                    self.released += units * unit;
                }
            }
        }

        fn elapsed(&self) -> Duration {
            self.now - self.start
        }
    }

    #[test]
    fn idle_after_a_spike_returns_to_the_floor() {
        let mut model = Model::new(HOST);
        model.run(10, Duration::from_secs(1), Pressure::Normal);
        assert_eq!(model.capacity, 10);
        model.run(0, Duration::from_secs(1), Pressure::Normal);
        assert_eq!(model.capacity, 10, "the peak is held for the hold time");
        model.run(0, Duration::from_secs(12), Pressure::Normal);
        assert_eq!(model.capacity, 0);
        assert_eq!(model.provision.target(), 0);
    }

    #[test]
    fn a_trickle_after_a_spike_does_not_pin_the_spike() {
        let mut model = Model::new(HOST);
        model.run(16, Duration::from_secs(1), Pressure::Normal);
        for _ in 0..100 {
            model.run(1, SAMPLE_PERIOD, Pressure::Normal);
            model.run(0, SAMPLE_PERIOD * 4, Pressure::Normal);
        }
        assert!(model.capacity <= 2, "capacity {}", model.capacity);
    }

    #[test]
    fn spikes_within_the_hold_time_keep_capacity() {
        let mut model = Model::new(HOST);
        for _ in 0..30 {
            model.run(12, SAMPLE_PERIOD * 2, Pressure::Normal);
            model.run(1, SAMPLE_PERIOD * 8, Pressure::Normal);
        }
        assert_eq!(model.released, 0);
        assert_eq!(model.grown, 12);
    }

    #[test]
    fn spikes_further_apart_than_the_hold_time_release_between() {
        let mut model = Model::new(HOST);
        for _ in 0..3 {
            model.run(12, SAMPLE_PERIOD * 2, Pressure::Normal);
            model.run(0, Duration::from_secs(10), Pressure::Normal);
            assert_eq!(model.capacity, 0);
        }
        assert_eq!(model.grown, 36);
        assert_eq!(model.released, 36);
    }

    #[test]
    fn steady_demand_does_not_churn() {
        let mut model = Model::new(HOST);
        model.run(5, Duration::from_secs(60), Pressure::Normal);
        assert_eq!(model.grown, 5);
        assert_eq!(model.released, 0);
        assert_eq!(model.provision.target(), 5);
    }

    #[test]
    fn noisy_demand_settles_without_flapping() {
        let mut model = Model::new(HOST);
        for round in 0..200 {
            model.run(4 + round % 3, SAMPLE_PERIOD, Pressure::Normal);
        }
        let (grown, released) = (model.grown, model.released);
        for round in 0..200 {
            model.run(4 + round % 3, SAMPLE_PERIOD, Pressure::Normal);
        }
        assert_eq!((model.grown, model.released), (grown, released));
        assert!(model.capacity >= 6);
    }

    #[test]
    fn rising_demand_grows_ahead_of_use() {
        let mut model = Model::new(ProvisionConfig {
            unit: 1,
            floor: 0,
            ceiling: 64,
            acquire: Duration::from_millis(100),
        });
        let mut ahead = false;
        for level in 1..20 {
            model.run(level, SAMPLE_PERIOD, Pressure::Normal);
            ahead |= model.provision.target() > level;
        }
        assert!(ahead);
        assert!(model.provision.target() <= 19 + 2);
    }

    #[test]
    fn shortfall_grows_now_and_decays_over_the_hold_time() {
        let config = ProvisionConfig {
            unit: 2,
            floor: 2,
            ceiling: 16,
            acquire: Duration::from_millis(100),
        };
        let mut model = Model::new(config);
        let hold = model.provision.hold();
        assert_eq!(hold, Duration::from_secs(100));
        model.provision.observe(2);
        model.provision.shortfall(model.now, 3);
        assert_eq!(model.provision.target(), 5);
        assert_eq!(
            model.provision.tick(model.now, 2, 0, Pressure::Normal),
            Plan::Grow(2)
        );
        assert_eq!(
            model.provision.tick(model.now, 2, 2, Pressure::Normal),
            Plan::Hold,
            "growth waits for the pending acquisition"
        );
        model.capacity = 6;
        model.run(0, hold * 3, Pressure::Normal);
        assert_eq!(model.capacity, 2, "surplus falls back to the floor");
        assert_eq!(model.provision.target(), 2);
    }

    /// Controller configured like a registered RDMA rail that recorded a
    /// shortfall of `items` at `start`.
    fn rail_with_shortfall(start: Instant, items: usize) -> Provision {
        let mut provision = Provision::new(ProvisionConfig {
            unit: 2,
            floor: 2,
            ceiling: 4096,
            acquire: Duration::from_millis(100),
        });
        assert_eq!(provision.hold(), Duration::from_secs(100));
        assert_eq!(provision.tick(start, 2, 0, Pressure::Normal), Plan::Hold);
        provision.shortfall(start, items);
        assert_eq!(provision.target(), items.max(2));
        provision
    }

    #[test]
    fn a_late_tick_decays_the_correction_over_the_whole_gap() {
        let start = Instant::now();
        let mut provision = rail_with_shortfall(start, 6);
        assert_eq!(
            provision.tick(start + Duration::from_secs(86_400), 6, 0, Pressure::Normal),
            Plan::Release(1)
        );
        assert_eq!(provision.target(), 2);
    }

    #[test]
    fn a_late_tick_decays_the_correction_like_timely_ones() {
        for items in [6, 40, 400, 4000] {
            for gap in [1, 7, 30, 60, 100, 150, 300, 600, 1_200] {
                let gap = Duration::from_secs(gap);
                let start = Instant::now();
                let mut timely = rail_with_shortfall(start, items);
                let mut now = start;
                while now < start + gap {
                    now += SAMPLE_PERIOD;
                    timely.tick(now, items, 0, Pressure::Normal);
                }
                let mut late = rail_with_shortfall(start, items);
                late.tick(start + gap, items, 0, Pressure::Normal);
                assert!(
                    late.target().abs_diff(timely.target()) <= 1 + timely.target() / 256,
                    "{items} items after {gap:?}: late {} timely {}",
                    late.target(),
                    timely.target()
                );
                assert!(
                    late.correction <= timely.correction,
                    "{items} items after {gap:?}: late {} timely {}",
                    late.correction,
                    timely.correction
                );
            }
        }
    }

    #[test]
    fn saturation_and_pressure_freeze_the_correction() {
        let mut provision = Provision::new(HOST);
        provision.set_saturated(true);
        provision.shortfall(Instant::now(), 8);
        assert_eq!(provision.target(), 0);
        provision.set_saturated(false);
        provision.tick(Instant::now(), 0, 0, Pressure::Elevated);
        provision.shortfall(Instant::now(), 8);
        assert_eq!(provision.target(), 0);
    }

    #[test]
    fn growth_respects_the_ceiling_and_unit() {
        let config = ProvisionConfig {
            unit: 4,
            floor: 0,
            ceiling: 10,
            acquire: Duration::from_millis(100),
        };
        let now = Instant::now();
        let mut provision = Provision::new(config);
        provision.shortfall(now, 9);
        assert_eq!(provision.tick(now, 0, 0, Pressure::Normal), Plan::Grow(2));
        assert_eq!(provision.tick(now, 8, 0, Pressure::Normal), Plan::Hold);
    }

    #[test]
    fn the_floor_is_kept_without_demand() {
        let config = ProvisionConfig {
            unit: 2,
            floor: 2,
            ceiling: 8,
            acquire: Duration::ZERO,
        };
        let mut model = Model::new(config);
        assert_eq!(
            model.provision.tick(model.now, 0, 0, Pressure::Normal),
            Plan::Grow(1)
        );
        model.capacity = 2;
        model.run(0, Duration::from_secs(30), Pressure::Critical);
        assert_eq!(model.capacity, 2);
    }

    #[test]
    fn elevated_pressure_trims_to_one_unit_above_use() {
        let mut model = Model::new(HOST);
        model.run(20, Duration::from_secs(1), Pressure::Normal);
        model.run(4, Duration::from_secs(1), Pressure::Elevated);
        assert_eq!(model.provision.target(), 5);
        assert!(model.capacity <= 5, "capacity {}", model.capacity);
        assert!(model.elapsed() < model.provision.hold() * 2);
    }

    #[test]
    fn critical_pressure_releases_all_surplus_at_once() {
        let mut model = Model::new(HOST);
        model.run(20, Duration::from_secs(1), Pressure::Normal);
        model.provision.observe(3);
        assert_eq!(
            model.provision.tick(model.now, 20, 0, Pressure::Critical),
            Plan::Release(17)
        );
    }

    #[test]
    fn hold_time_follows_measured_acquire_time_within_bounds() {
        let now = Instant::now();
        let mut provision = Provision::new(ProvisionConfig {
            acquire: Duration::ZERO,
            ..HOST
        });
        assert_eq!(provision.hold(), MIN_HOLD);
        for _ in 0..64 {
            provision.acquired(now, 2, Duration::from_millis(40));
        }
        assert!(provision.acquire() <= Duration::from_millis(20));
        assert!(provision.acquire() >= Duration::from_millis(19));
        assert!(provision.hold() >= Duration::from_secs(19));
        for _ in 0..64 {
            provision.acquired(now, 1, Duration::from_secs(10));
        }
        assert_eq!(provision.hold(), MAX_HOLD);
        provision.acquired(now, 0, Duration::from_secs(10));
    }

    /// Report a burst that came and went since the last tick, then tick
    /// `gap` after `start` with the burst's capacity still provisioned.
    fn burst_after_gap(provision: &mut Provision, start: Instant, gap: Duration) -> Plan {
        provision.observe(10);
        provision.observe(0);
        provision.tick(start + gap, 10, 0, Pressure::Normal)
    }

    #[test]
    fn a_burst_right_after_an_idle_gap_is_held() {
        let config = ProvisionConfig {
            acquire: Duration::ZERO,
            ..HOST
        };
        let hold = Provision::new(config).hold();
        assert_eq!(hold, MIN_HOLD);
        let width = hold / WINDOWS as u32;
        for gap in [
            width + Duration::from_millis(1),
            hold + width,
            hold * 4 + width,
            Duration::from_secs(40),
        ] {
            let start = Instant::now();
            let mut provision = Provision::new(config);
            assert_eq!(provision.tick(start, 0, 0, Pressure::Normal), Plan::Hold);
            assert_eq!(
                burst_after_gap(&mut provision, start, gap),
                Plan::Hold,
                "gap {gap:?}"
            );
            assert_eq!(provision.target(), 10, "gap {gap:?}");
            let mut now = start + gap;
            let mut capacity = 10;
            let mut released_at = None;
            while released_at.is_none() {
                now += SAMPLE_PERIOD;
                match provision.tick(now, capacity, 0, Pressure::Normal) {
                    Plan::Hold => {}
                    Plan::Release(units) => {
                        capacity -= units;
                        released_at = Some(now);
                    }
                    Plan::Grow(units) => panic!("gap {gap:?}: grew {units}"),
                }
                assert!(now < start + gap + hold * 8, "gap {gap:?}: never released");
            }
            let held = released_at.unwrap() - (start + gap);
            assert!(held >= hold - width, "gap {gap:?}: released after {held:?}");
        }
    }

    #[test]
    fn time_may_stall_or_jump() {
        let now = Instant::now();
        let mut provision = Provision::new(HOST);
        assert_eq!(
            provision.tick(now + Duration::from_secs(5), 0, 0, Pressure::Normal),
            Plan::Hold
        );
        provision.observe(3);
        assert_eq!(provision.tick(now, 0, 0, Pressure::Normal), Plan::Grow(3));
        let later = now + Duration::from_secs(86_400);
        provision.observe(0);
        assert_eq!(
            provision.tick(later, 3, 0, Pressure::Normal),
            Plan::Release(2)
        );
        assert_eq!(provision.target(), 0);
    }
}
