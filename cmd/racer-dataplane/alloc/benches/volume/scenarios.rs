//! Per-worker setup and the benchmark scenarios.

use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Barrier;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use page_alloc::volume::{
    Key, Limits, MountReport, SLOT_BYTES, SLOT_UNITS, Tier, UNIT_BYTES, Volume, VolumeOptions,
};
use page_alloc::{AlignedBuffer, Charge, Error, Slab};

use crate::args::{Args, Scenario};
use crate::exec::{BenchError, BenchReactor, ExecStats, SCOPE, Task, Timers, block_on, run};
use crate::plan::WorkerPlan;
use crate::stats::{Hist, Report, ThreadUsage};

/// Slots `allocate_evicting` may evict per call, as `EVICTION_BUDGET` in
/// `src/store/disk.rs`.
pub const EVICTION_BUDGET: usize = 64;

/// Free slots the lazy committer keeps ahead of allocation, as the
/// dataplane's maintenance does.
const FREE_RESERVE: u64 = 2;

type Result<T> = std::result::Result<T, BenchError>;

/// Buffer accounting for the benchmark. Production charges volume buffers as
/// `ResourceClass::Ciphertext` (`src/store.rs`), which does not require a wipe
/// on release (`src/admission.rs`), so neither does this by default. `--wipe`
/// restores the wipe that the unit `()` charge would do.
#[derive(Clone, Copy, Debug)]
pub struct BenchCharge {
    wipe: bool,
}

impl Charge for BenchCharge {
    fn covers(&self, _bytes: usize) -> bool {
        true
    }

    fn requires_wipe(&self) -> bool {
        self.wipe
    }
}

/// One unit of work every worker runs together.
#[derive(Clone, Copy, Debug)]
pub struct Job {
    pub scenario: Scenario,
    pub page_bytes: u64,
}

/// Seconds since the Unix epoch.
fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

/// SplitMix64 step.
pub fn mix(mut z: u64) -> u64 {
    z = z.wrapping_add(0x9e37_79b9_7f4a_7c15);
    z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    z ^ (z >> 31)
}

/// Small deterministic RNG.
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(1);
        mix(self.0)
    }

    fn below(&mut self, n: usize) -> usize {
        (self.next() % n.max(1) as u64) as usize
    }
}

/// Production-like keys: random cache and version hashes, sequential pages.
pub struct KeyGen {
    cache: [u8; 8],
    seed: u64,
    pages_per_version: u64,
    next: Cell<u64>,
}

impl KeyGen {
    fn new(worker: usize, pages_per_version: u32) -> Self {
        let seed = mix(0x5eed ^ worker as u64);
        Self {
            cache: mix(seed).to_le_bytes(),
            seed,
            pages_per_version: u64::from(pages_per_version),
            next: Cell::new(0),
        }
    }

    /// Key number `n`.
    fn key(&self, n: u64) -> Key {
        let version = n / self.pages_per_version;
        let a = mix(self.seed ^ version);
        let b = mix(a);
        let mut hash = [0u8; 12];
        hash[..8].copy_from_slice(&a.to_le_bytes());
        hash[8..].copy_from_slice(&b.to_le_bytes()[..4]);
        Key::new(self.cache, hash, (n % self.pages_per_version) as u32)
    }

    /// A key never returned before on this worker.
    fn fresh(&self) -> Key {
        let n = self.next.get();
        self.next.set(n + 1);
        self.key(n)
    }
}

/// Barrier pair around the timed part of a job. Main samples device counters
/// and wall time between them.
pub struct Phase<'a> {
    barrier: &'a Barrier,
    started: bool,
    ended: bool,
    t0: Instant,
    usage: ThreadUsage,
}

impl<'a> Phase<'a> {
    pub fn new(barrier: &'a Barrier) -> Self {
        Self {
            barrier,
            started: false,
            ended: false,
            t0: Instant::now(),
            usage: ThreadUsage::default(),
        }
    }

    /// Wait for every worker, then start the clock.
    fn start(&mut self) -> Instant {
        assert!(!self.started);
        self.barrier.wait();
        self.started = true;
        self.usage = ThreadUsage::now();
        self.t0 = Instant::now();
        self.t0
    }

    /// Stop the clock and record wall and CPU time, then wait for every worker.
    fn end(&mut self, report: &mut Report) {
        assert!(self.started && !self.ended);
        let wall = self.t0.elapsed();
        ThreadUsage::now().since(self.usage, report);
        report.add("wall_ns", wall.as_nanos() as u64);
        self.ended = true;
        self.barrier.wait();
    }

    /// Complete missing barrier waits after an error.
    pub fn finish(&mut self) {
        if !self.started {
            self.started = true;
            self.barrier.wait();
        }
        if !self.ended {
            self.ended = true;
            self.barrier.wait();
        }
    }
}

/// A mounted volume on its slab.
struct Mounted {
    slab: Rc<Slab<BenchCharge>>,
    volume: Rc<Volume<BenchCharge>>,
}

/// Hot-path counters shared by a worker's tasks.
#[derive(Default)]
struct Hot {
    ops: u64,
    bytes: u64,
    busy: u64,
    errors: u64,
    first_error: Option<String>,
    evicted_entries: u64,
    evicting_allocs: u64,
    dirty_commits: u64,
    background_evictions: u64,
    background_evict: Hist,
    alloc: Hist,
    evict_alloc: Hist,
    write: Hist,
    publish: Hist,
    op: Hist,
    commit_wait: Hist,
    commit_calls: u64,
    commit_errors: u64,
    lazy_commit: Hist,
    lazy_commits: u64,
    get: Hist,
    read: Hist,
    reads: u64,
    read_bytes: u64,
    misses: u64,
    mismatches: u64,
}

impl Hot {
    fn error(&mut self, what: &str, error: impl std::fmt::Debug) {
        self.errors += 1;
        if self.first_error.is_none() {
            self.first_error = Some(format!("{what}: {error:?}"));
        }
    }

    fn report(&self, report: &mut Report, prefix: &str) {
        let p = |name: &str| format!("{prefix}{name}");
        for (name, value) in [
            ("ops", self.ops),
            ("bytes", self.bytes),
            ("busy", self.busy),
            ("errors", self.errors),
            ("evicted_entries", self.evicted_entries),
            ("evicting_allocs", self.evicting_allocs),
            ("background_evictions", self.background_evictions),
            ("dirty_commits", self.dirty_commits),
            ("commit_calls", self.commit_calls),
            ("commit_errors", self.commit_errors),
            ("lazy_commits", self.lazy_commits),
            ("reads", self.reads),
            ("read_bytes", self.read_bytes),
            ("misses", self.misses),
            ("mismatches", self.mismatches),
        ] {
            if value != 0 {
                report.add(p(name), value);
            }
        }
        for (name, hist) in [
            ("alloc_ns", &self.alloc),
            ("alloc_evicting_ns", &self.evict_alloc),
            ("background_evict_ns", &self.background_evict),
            ("write_ns", &self.write),
            ("publish_ns", &self.publish),
            ("op_ns", &self.op),
            ("commit_wait_ns", &self.commit_wait),
            ("lazy_commit_ns", &self.lazy_commit),
            ("get_ns", &self.get),
            ("read_ns", &self.read),
        ] {
            report.hist(p(name), hist);
        }
        if let Some(error) = &self.first_error {
            report.note(format!("{prefix}first error: {error}"));
        }
    }
}

/// How a put lane writes.
#[derive(Clone, Copy)]
struct PutConfig {
    tier: Tier,
    length: u64,
    evict: bool,
    commit_each: bool,
    retain_secs: u64,
    stop_on_busy: bool,
}

/// Where a put lane gets keys and reports published entries.
struct PutSink<'b> {
    next_key: &'b dyn Fn() -> Key,
    keys: Option<&'b RefCell<Vec<Key>>>,
    /// Operations left; `u64::MAX` for unbounded.
    budget: &'b Cell<u64>,
    /// Lanes still running.
    active: &'b Cell<usize>,
}

/// One worker thread's state.
pub struct Worker<'a> {
    pub index: usize,
    pub args: &'a Args,
    pub plan: &'a WorkerPlan,
    pub reactor: BenchReactor,
    pub timers: Timers,
    keys: KeyGen,
}

/// Decrements a counter when dropped.
struct Active<'b>(&'b Cell<usize>);

impl Drop for Active<'_> {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}

fn round_up(bytes: u64) -> usize {
    bytes.div_ceil(UNIT_BYTES as u64) as usize * UNIT_BYTES
}

fn stamp(buffer: &mut AlignedBuffer<BenchCharge>, key: &Key, generation: u64) {
    let bytes = buffer.as_mut_slice();
    bytes[..24].copy_from_slice(&key.0);
    bytes[24..32].copy_from_slice(&generation.to_le_bytes());
}

fn matches(bytes: &[u8], key: &Key, generation: u64) -> bool {
    bytes[..24] == key.0 && bytes[24..32] == generation.to_le_bytes()
}

fn ns(d: Duration) -> u64 {
    d.as_nanos() as u64
}

impl<'a> Worker<'a> {
    pub fn new(index: usize, args: &'a Args, plan: &'a WorkerPlan) -> Result<Self> {
        let reactor = BenchReactor::new(args.ring_entries, ());
        reactor.init()?;
        Ok(Self {
            index,
            args,
            plan,
            reactor,
            timers: Timers::default(),
            keys: KeyGen::new(index, args.pages_per_version),
        })
    }

    fn charge(&self) -> BenchCharge {
        BenchCharge {
            wipe: self.args.wipe,
        }
    }

    fn slot_count(&self) -> u64 {
        self.plan.placements.len() as u64
    }

    /// Mount options with this run's charge and metadata cache budget.
    fn options(
        &self,
        limits: Limits,
        volume_id: [u8; 16],
        format: bool,
    ) -> VolumeOptions<BenchCharge> {
        let charge = self.charge();
        let mut options =
            VolumeOptions::new(limits, volume_id, format, Rc::new(move |_| Ok(charge)));
        options.metadata_cache_bytes = self.args.metadata_cache as usize;
        options.dirty_bytes = self.args.dirty_bytes as usize;
        options
    }

    /// Mount the volume already on `slab`. Returns it, its report, and the
    /// mount time.
    fn remount(
        &self,
        slab: &Rc<Slab<BenchCharge>>,
    ) -> Result<(Rc<Volume<BenchCharge>>, MountReport, Duration)> {
        let start = Instant::now();
        let (volume, report) = block_on(
            &self.reactor,
            &self.timers,
            Volume::mount(
                &self.reactor,
                slab.clone(),
                self.options(Limits::UNLIMITED, [0; 16], false),
                &SCOPE,
            ),
        )?;
        Ok((volume, report, start.elapsed()))
    }

    /// Format a fresh volume: wipe both superblocks, then mount with format.
    fn fresh(&self, limits: Limits) -> Result<Mounted> {
        let slab = Rc::new(Slab::from_devices(
            self.plan.placements.clone(),
            SLOT_BYTES,
            SLOT_BYTES as usize,
            self.plan.alignment,
        )?);
        let _ = slab.open_volume(self.slot_count())?;
        let mut zero = slab.allocate(2 * UNIT_BYTES, self.charge())?;
        zero.as_mut_slice().fill(0);
        let _ = block_on(
            &self.reactor,
            &self.timers,
            slab.write_at(&self.reactor, 0, zero, (), &SCOPE),
        )?;
        block_on(
            &self.reactor,
            &self.timers,
            slab.sync(&self.reactor, &SCOPE),
        )?;
        let mut volume_id = [0u8; 16];
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_nanos() as u64);
        volume_id[..8].copy_from_slice(&mix(nanos ^ self.index as u64).to_le_bytes());
        volume_id[8..].copy_from_slice(&mix(nanos.rotate_left(17)).to_le_bytes());
        let (volume, report) = block_on(
            &self.reactor,
            &self.timers,
            Volume::mount(
                &self.reactor,
                slab.clone(),
                self.options(limits, volume_id, true),
                &SCOPE,
            ),
        )?;
        if !report.formatted {
            return Err(Error::Corrupt.into());
        }
        Ok(Mounted { slab, volume })
    }

    /// A buffer of `length` bytes rounded to units, filled once.
    fn buffer(&self, slab: &Slab<BenchCharge>, length: u64) -> Result<AlignedBuffer<BenchCharge>> {
        let mut buffer = slab.allocate(round_up(length), self.charge())?;
        buffer.as_mut_slice().fill(0xa5);
        Ok(buffer)
    }

    fn run_tasks(&self, tasks: Vec<Task<'_>>, stats: &mut ExecStats) -> Result<()> {
        run(&self.reactor, &self.timers, tasks, self.args.spin, stats)?;
        Ok(())
    }

    /// Run one job.
    pub fn job(&self, job: Job, phase: &mut Phase<'_>) -> Result<Report> {
        let mut report = Report::default();
        match job.scenario {
            Scenario::Write => self.write(job.page_bytes, phase, &mut report)?,
            Scenario::Held => self.held(job.page_bytes, phase, &mut report)?,
            Scenario::Read => self.read(job.page_bytes, phase, &mut report)?,
            Scenario::Commit => self.commit(phase, &mut report)?,
            Scenario::Cpu => self.cpu(phase, &mut report)?,
            Scenario::Lookup => self.lookup(phase, &mut report)?,
        }
        Ok(report)
    }

    /// Allocate, write, publish (and commit) until stopped.
    async fn put_lane(
        &self,
        volume: &Volume<BenchCharge>,
        slab: &Slab<BenchCharge>,
        hot: &RefCell<Hot>,
        config: PutConfig,
        deadline: Option<Instant>,
        sink: &PutSink<'_>,
    ) {
        let _active = Active(sink.active);
        let Ok(mut buffer) = self.buffer(slab, config.length) else {
            hot.borrow_mut().error("buffer", ());
            return;
        };
        loop {
            if deadline.is_some_and(|d| Instant::now() >= d) || sink.budget.get() == 0 {
                break;
            }
            sink.budget.set(sink.budget.get().saturating_sub(1));
            let start = Instant::now();
            let (result, evicted) = if config.evict {
                match volume
                    .allocate_evicting(
                        &self.reactor,
                        config.tier,
                        config.length,
                        now_unix(),
                        EVICTION_BUDGET,
                        &SCOPE,
                    )
                    .await
                {
                    Ok(done) => done,
                    Err(error) => {
                        hot.borrow_mut().error("allocate_evicting", error);
                        self.timers.sleep(Duration::from_millis(1)).await;
                        continue;
                    }
                }
            } else {
                (volume.allocate(config.tier, config.length), Vec::new())
            };
            let allocated = Instant::now();
            {
                let mut h = hot.borrow_mut();
                h.alloc.record_duration(allocated - start);
                if !evicted.is_empty() {
                    h.evicting_allocs += 1;
                    h.evicted_entries += evicted.len() as u64;
                    h.evict_alloc.record_duration(allocated - start);
                }
            }
            drop(evicted);
            let reservation = match result {
                Ok(reservation) => reservation,
                Err(error) => {
                    if sink.budget.get() != u64::MAX {
                        sink.budget.set(sink.budget.get() + 1);
                    }
                    if error == Error::Busy && volume.needs_commit() {
                        // The overlay of uncommitted changes is full: commit
                        // (concurrent lanes share the attempt), as the
                        // dataplane's writer does, and retry.
                        hot.borrow_mut().dirty_commits += 1;
                        if let Err(error) = volume.commit(&self.reactor, &SCOPE).await {
                            hot.borrow_mut().error("dirty-limit commit", error);
                        }
                        continue;
                    }
                    if error == Error::Busy {
                        hot.borrow_mut().busy += 1;
                    } else {
                        hot.borrow_mut().error("allocate", error);
                    }
                    if config.stop_on_busy {
                        break;
                    }
                    self.timers.sleep(Duration::from_millis(1)).await;
                    continue;
                }
            };
            let key = (sink.next_key)();
            let generation = volume.next_generation();
            stamp(&mut buffer, &key, generation);
            let write_start = Instant::now();
            match volume
                .write(&self.reactor, &reservation, buffer, &SCOPE)
                .await
            {
                Ok(returned) => buffer = returned,
                Err(error) => {
                    hot.borrow_mut().error("write", error);
                    match self.buffer(slab, config.length) {
                        Ok(fresh) => buffer = fresh,
                        Err(_) => break,
                    }
                    continue;
                }
            }
            let written = Instant::now();
            let retain = if config.tier == Tier::Storage {
                now_unix() + config.retain_secs
            } else {
                0
            };
            let published =
                volume.publish(key, reservation, config.length as u32, generation, retain);
            let publish_end = Instant::now();
            {
                let mut h = hot.borrow_mut();
                h.write.record_duration(written - write_start);
                h.publish.record_duration(publish_end - written);
                if let Err(error) = published {
                    h.error("publish", error);
                    continue;
                }
            }
            if let Some(keys) = sink.keys {
                keys.borrow_mut().push(key);
            }
            if config.commit_each {
                let commit_start = Instant::now();
                let result = volume.commit(&self.reactor, &SCOPE).await;
                let mut h = hot.borrow_mut();
                h.commit_calls += 1;
                h.commit_wait.record_duration(commit_start.elapsed());
                if let Err(error) = result {
                    h.commit_errors += 1;
                    h.error("commit", error);
                    continue;
                }
            }
            let mut h = hot.borrow_mut();
            h.ops += 1;
            h.bytes += config.length;
            h.op.record_duration(start.elapsed());
        }
    }

    /// Lazy commit like the dataplane: every interval, when dirty.
    async fn commit_loop(
        &self,
        volume: &Volume<BenchCharge>,
        hot: &RefCell<Hot>,
        deadline: Option<Instant>,
        active: &Cell<usize>,
    ) {
        let interval = self.args.commit_interval;
        let mut last = Instant::now();
        loop {
            let tick = (last + interval).min(Instant::now() + Duration::from_millis(10));
            let tick = deadline.map_or(tick, |d| tick.min(d));
            self.timers.sleep_until(tick).await;
            let now = Instant::now();
            if deadline.is_some_and(|d| now >= d) || active.get() == 0 {
                break;
            }
            // Keep a few slots free ahead of allocation, as the dataplane's
            // maintenance does, so foreground writes rarely evict.
            if volume.free_slots() < FREE_RESERVE {
                let start = Instant::now();
                match volume
                    .reserve_free(&self.reactor, FREE_RESERVE, now_unix(), 2, &SCOPE)
                    .await
                {
                    Ok(evicted) if !evicted.is_empty() => {
                        let mut h = hot.borrow_mut();
                        h.background_evictions += 1;
                        h.evicted_entries += evicted.len() as u64;
                        h.background_evict.record_duration(start.elapsed());
                    }
                    Ok(_) => {}
                    Err(error) => hot.borrow_mut().error("reserve_free", error),
                }
            }
            // Commit early when uncommitted changes reach the dirty limit:
            // allocation returns Busy until they drain.
            if now.saturating_duration_since(last) < interval && !volume.needs_commit() {
                continue;
            }
            if !volume.is_dirty() {
                // Restart the interval like a ticker; leaving `last` behind
                // would make the next tick already expired, and the loop
                // would spin inside one poll without ever yielding.
                last = now;
                continue;
            }
            last = now;
            let result = volume.commit(&self.reactor, &SCOPE).await;
            let mut h = hot.borrow_mut();
            h.lazy_commits += 1;
            h.lazy_commit.record_duration(now.elapsed());
            if let Err(error) = result {
                h.commit_errors += 1;
                h.error("lazy commit", error);
            }
        }
    }

    /// Run `lanes` put lanes plus an optional lazy committer until done.
    #[allow(clippy::too_many_arguments)]
    fn puts(
        &self,
        mounted: &Mounted,
        hot: &RefCell<Hot>,
        config: PutConfig,
        lanes: usize,
        deadline: Option<Instant>,
        lazy: bool,
        sink: &PutSink<'_>,
        stats: &mut ExecStats,
    ) -> Result<()> {
        sink.active.set(lanes);
        let volume = &*mounted.volume;
        let slab = &*mounted.slab;
        let mut tasks: Vec<Task<'_>> = (0..lanes)
            .map(|_| Box::pin(self.put_lane(volume, slab, hot, config, deadline, sink)) as Task<'_>)
            .collect();
        if lazy {
            tasks.push(Box::pin(self.commit_loop(
                volume,
                hot,
                deadline,
                sink.active,
            )));
        }
        self.run_tasks(tasks, stats)
    }

    fn commit_now(&self, volume: &Volume<BenchCharge>) -> Result<Duration> {
        let start = Instant::now();
        block_on(
            &self.reactor,
            &self.timers,
            volume.commit(&self.reactor, &SCOPE),
        )?;
        Ok(start.elapsed())
    }

    fn usage(&self, volume: &Volume<BenchCharge>, report: &mut Report, prefix: &str) {
        let usage = volume.usage();
        for (name, value) in [
            ("slots", usage.slot_count),
            ("free_slots", usage.free_slots),
            ("node_slots", usage.node_slots),
            ("cache_slots", usage.cache_slots),
            ("storage_slots", usage.storage_slots),
            ("node_units", usage.node_units),
            ("pending_units", usage.pending_units),
            ("records", usage.records),
            ("entries", volume.len()),
        ] {
            report.add(format!("{prefix}{name}"), value);
        }
    }

    fn commit_counters(
        &self,
        volume: &Volume<BenchCharge>,
        before: (u64, u64, u64),
        report: &mut Report,
    ) {
        let after = volume.commit_stats();
        report.add("commit_attempts", after.attempts - before.0);
        report.add("commit_succeeded", after.succeeded - before.1);
        report.add("commit_failed", after.failed - before.2);
    }

    /// Steady-state cache-tier ingest with eviction and lazy commits.
    fn write(&self, page: u64, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        let mounted = self.fresh(Limits::UNLIMITED)?;
        let hot = RefCell::new(Hot::default());
        let budget = Cell::new(u64::MAX);
        let active = Cell::new(0);
        let next_key = || self.keys.fresh();
        let sink = PutSink {
            next_key: &next_key,
            keys: None,
            budget: &budget,
            active: &active,
        };
        let config = PutConfig {
            tier: Tier::Cache,
            length: page,
            evict: true,
            commit_each: false,
            retain_secs: 0,
            stop_on_busy: false,
        };
        let stats0 = mounted.volume.commit_stats();
        let mut stats = ExecStats::default();
        let start = phase.start();
        self.puts(
            &mounted,
            &hot,
            config,
            self.args.qd,
            Some(start + self.args.duration),
            true,
            &sink,
            &mut stats,
        )?;
        phase.end(report);
        stats.report(report);
        hot.borrow().report(report, "");
        self.commit_counters(
            &mounted.volume,
            (stats0.attempts, stats0.succeeded, stats0.failed),
            report,
        );
        self.usage(&mounted.volume, report, "end_");
        Ok(())
    }

    /// Storage-tier writes that commit before acknowledging.
    fn held(&self, page: u64, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        let storage = (self.slot_count() * self.args.hold_percent / 100).max(1);
        let mounted = self.fresh(Limits {
            storage_slots: storage,
            small_bytes: u64::MAX,
        })?;
        report.add("storage_slot_limit", storage);
        let hot = RefCell::new(Hot::default());
        let budget = Cell::new(u64::MAX);
        let active = Cell::new(0);
        let next_key = || self.keys.fresh();
        let sink = PutSink {
            next_key: &next_key,
            keys: None,
            budget: &budget,
            active: &active,
        };
        let config = PutConfig {
            tier: Tier::Storage,
            length: page,
            evict: true,
            commit_each: true,
            retain_secs: self.args.retain_secs,
            stop_on_busy: false,
        };
        let stats0 = mounted.volume.commit_stats();
        let mut stats = ExecStats::default();
        let start = phase.start();
        self.puts(
            &mounted,
            &hot,
            config,
            self.args.qd,
            Some(start + self.args.duration),
            false,
            &sink,
            &mut stats,
        )?;
        phase.end(report);
        stats.report(report);
        hot.borrow().report(report, "");
        self.commit_counters(
            &mounted.volume,
            (stats0.attempts, stats0.succeeded, stats0.failed),
            report,
        );
        self.usage(&mounted.volume, report, "end_");
        Ok(())
    }

    /// Fill (untimed), commit, then random reads of published records.
    fn read(&self, page: u64, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        let mounted = self.fresh(Limits::UNLIMITED)?;
        let keys = RefCell::new(Vec::new());
        let fill = RefCell::new(Hot::default());
        let budget = Cell::new(
            self.args
                .read_fill_bytes
                .map_or(u64::MAX, |b| b.div_ceil(page).max(1)),
        );
        let active = Cell::new(0);
        let next_key = || self.keys.fresh();
        let sink = PutSink {
            next_key: &next_key,
            keys: Some(&keys),
            budget: &budget,
            active: &active,
        };
        let config = PutConfig {
            tier: Tier::Cache,
            length: page,
            evict: false,
            commit_each: false,
            retain_secs: 0,
            stop_on_busy: true,
        };
        let fill_start = Instant::now();
        let mut fill_stats = ExecStats::default();
        self.puts(
            &mounted,
            &fill,
            config,
            self.args.qd,
            None,
            true,
            &sink,
            &mut fill_stats,
        )?;
        let commit = self.commit_now(&mounted.volume)?;
        report.add("fill_ns", ns(fill_start.elapsed()));
        report.add("fill_ops", fill.borrow().ops);
        report.add("fill_bytes", fill.borrow().bytes);
        report.sample("fill_final_commit_ns", ns(commit));
        let keys = keys.into_inner();
        if keys.is_empty() {
            return Err(Error::Busy.into());
        }
        let hot = RefCell::new(Hot::default());
        let mut stats = ExecStats::default();
        let start = phase.start();
        let deadline = start + self.args.duration;
        let tasks: Vec<Task<'_>> = (0..self.args.qd)
            .map(|lane| {
                Box::pin(self.read_lane(
                    &mounted,
                    &hot,
                    page,
                    &keys,
                    deadline,
                    mix(lane as u64 ^ (self.index as u64) << 32),
                )) as Task<'_>
            })
            .collect();
        self.run_tasks(tasks, &mut stats)?;
        phase.end(report);
        stats.report(report);
        hot.borrow().report(report, "");
        self.usage(&mounted.volume, report, "end_");
        Ok(())
    }

    async fn read_lane(
        &self,
        mounted: &Mounted,
        hot: &RefCell<Hot>,
        page: u64,
        keys: &[Key],
        deadline: Instant,
        seed: u64,
    ) {
        let mut rng = Rng(seed);
        let Ok(mut buffer) = self.buffer(&mounted.slab, page) else {
            hot.borrow_mut().error("buffer", ());
            return;
        };
        let volume = &*mounted.volume;
        while Instant::now() < deadline {
            let key = keys[rng.below(keys.len())];
            let start = Instant::now();
            let value = match volume.get(&self.reactor, &key, &SCOPE).await {
                Ok(value) => value,
                Err(error) => {
                    hot.borrow_mut().error("get", error);
                    continue;
                }
            };
            let looked = Instant::now();
            hot.borrow_mut().get.record_duration(looked - start);
            let Some(value) = value else {
                hot.borrow_mut().misses += 1;
                continue;
            };
            match volume.read(&self.reactor, &value, buffer, &SCOPE).await {
                Ok(returned) => buffer = returned,
                Err(error) => {
                    hot.borrow_mut().error("read", error);
                    match self.buffer(&mounted.slab, page) {
                        Ok(fresh) => buffer = fresh,
                        Err(_) => return,
                    }
                    continue;
                }
            }
            let done = Instant::now();
            let mut h = hot.borrow_mut();
            h.read.record_duration(done - looked);
            h.op.record_duration(done - start);
            h.reads += 1;
            h.read_bytes += page;
            if self.args.verify && !matches(buffer.as_slice(), &key, value.generation) {
                h.mismatches += 1;
            }
        }
    }

    /// Commit and mount cost against index size.
    fn commit(&self, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        phase.start();
        let mut stats = ExecStats::default();
        let page = UNIT_BYTES as u64;
        for &target in &self.args.entries {
            let label = format!("e{target}");
            let mounted = self.fresh(Limits::UNLIMITED)?;
            let keys = RefCell::new(Vec::new());
            let hot = RefCell::new(Hot::default());
            let budget = Cell::new(target);
            let active = Cell::new(0);
            let next_key = || self.keys.fresh();
            let config = PutConfig {
                tier: Tier::Cache,
                length: page,
                evict: false,
                commit_each: false,
                retain_secs: 0,
                stop_on_busy: true,
            };
            let fill_start = Instant::now();
            self.puts(
                &mounted,
                &hot,
                config,
                self.args.qd,
                None,
                false,
                &PutSink {
                    next_key: &next_key,
                    keys: Some(&keys),
                    budget: &budget,
                    active: &active,
                },
                &mut stats,
            )?;
            let entries = mounted.volume.len();
            report.add(format!("{label}_entries"), entries);
            report.add(format!("{label}_fill_ns"), ns(fill_start.elapsed()));
            if entries < target {
                report.note(format!(
                    "{label}: volume full after {entries} entries (raise slots or lower --entries)"
                ));
            }
            hot.borrow().report(report, &format!("{label}_fill_"));
            let elapsed = self.commit_now(&mounted.volume)?;
            let commit = mounted.volume.commit_stats();
            report.sample(format!("{label}_full_commit_ns"), ns(elapsed));
            report.add(format!("{label}_full_pages"), commit.pages_written);
            report.add(format!("{label}_full_writes"), commit.writes);
            let keys = keys.into_inner();
            let mut rng = Rng(mix(self.index as u64 ^ target));
            for &mutations in &self.args.mutations {
                if mutations > keys.len() as u64 {
                    continue;
                }
                let budget = Cell::new(mutations);
                let chosen: Vec<Key> = (0..mutations)
                    .map(|_| keys[rng.below(keys.len())])
                    .collect();
                let cursor = Cell::new(0usize);
                let next_key = || {
                    let i = cursor.get();
                    cursor.set(i + 1);
                    chosen[i % chosen.len()]
                };
                let hot = RefCell::new(Hot::default());
                self.puts(
                    &mounted,
                    &hot,
                    config,
                    self.args.qd.min(mutations as usize),
                    None,
                    false,
                    &PutSink {
                        next_key: &next_key,
                        keys: None,
                        budget: &budget,
                        active: &active,
                    },
                    &mut stats,
                )?;
                let elapsed = self.commit_now(&mounted.volume)?;
                let commit = mounted.volume.commit_stats();
                report.sample(format!("{label}_m{mutations}_commit_ns"), ns(elapsed));
                report.add(format!("{label}_m{mutations}_pages"), commit.pages_written);
                report.add(format!("{label}_m{mutations}_writes"), commit.writes);
            }
            // No-op commit and bare fsync: the floor under every commit.
            let elapsed = self.commit_now(&mounted.volume)?;
            report.sample(format!("{label}_clean_commit_ns"), ns(elapsed));
            for _ in 0..5 {
                let start = Instant::now();
                block_on(
                    &self.reactor,
                    &self.timers,
                    mounted.slab.sync(&self.reactor, &SCOPE),
                )?;
                report.sample("fsync_ns", ns(start.elapsed()));
            }
            report.add(
                format!("{label}_cache_bytes"),
                mounted.volume.pager_stats().bytes,
            );
            self.usage(&mounted.volume, report, &format!("{label}_end_"));
            // Remount: drop the volume, then mount the same slab.
            let Mounted { slab, volume, .. } = mounted;
            drop(volume);
            let (volume, mount, elapsed) = self.remount(&slab)?;
            report.sample(format!("{label}_mount_ns"), ns(elapsed));
            report.add(format!("{label}_mount_entries"), mount.entries);
            report.add(format!("{label}_mount_pages_read"), mount.pages_read);
            report.add(
                format!("{label}_mount_invalid_copies"),
                mount.invalid_copies,
            );
            report.add(format!("{label}_mount_pending_nodes"), mount.pending_nodes);
            if mount.entries != entries || mount.formatted || mount.dirty || mount.fell_back {
                report.note(format!(
                    "{label}: remount found {} entries (expected {entries}), formatted={}, dirty={}",
                    mount.entries, mount.formatted, mount.dirty
                ));
            }
            drop(volume);
        }
        phase.end(report);
        stats.report(report);
        Ok(())
    }

    /// CPU cost of index operations, apart from device I/O. The index lives
    /// on disk behind the page cache; with the default cache every page used
    /// here stays cached, so the synchronous calls (`allocate`, `publish`,
    /// `try_get`) and the cached async ones (`get`, `remove`, `evict_one`)
    /// do no metadata I/O. `--cpu-entries` is capped by the volume capacity.
    fn cpu(&self, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        phase.start();
        let mut rng = Rng(mix(self.index as u64 ^ 0xc0));
        let mounted = self.fresh(Limits::UNLIMITED)?;
        let volume = &*mounted.volume;
        let capacity = (self.slot_count() - 2) * u64::from(SLOT_UNITS) * 3 / 4;
        let n = self.args.cpu_entries.clamp(1, capacity) as usize;
        let mut samples: Vec<(String, u64)> = Vec::new();
        let small = n.min(1 << 14);
        let start = Instant::now();
        for _ in 0..small {
            drop(volume.allocate(Tier::Cache, UNIT_BYTES as u64));
        }
        report.add("vol_alloc_abort_ns", ns(start.elapsed()));
        report.add("vol_alloc_abort_ops", small as u64);
        // Write n records (I/O, untimed below), then publish them (timed).
        let written = RefCell::new(Vec::with_capacity(n));
        let budget = Cell::new(n as u64);
        let mut stats = ExecStats::default();
        let mut published = Vec::with_capacity(n);
        let mut publish_ns = 0;
        while budget.get() > 0 {
            let before = budget.get();
            let tasks: Vec<Task<'_>> = (0..self.args.qd)
                .map(|_| Box::pin(self.reserve_lane(&mounted, &written, &budget)) as Task<'_>)
                .collect();
            self.run_tasks(tasks, &mut stats)?;
            let batch = std::mem::take(&mut *written.borrow_mut());
            let start = Instant::now();
            for (reservation, key, generation) in batch {
                if volume
                    .publish(key, reservation, UNIT_BYTES as u32, generation, 0)
                    .is_ok()
                {
                    published.push(key);
                }
            }
            publish_ns += ns(start.elapsed());
            if volume.needs_commit() {
                self.commit_now(volume)?;
            }
            if budget.get() == before {
                break;
            }
        }
        report.add("vol_publish_ns", publish_ns);
        report.add("vol_publish_ops", published.len() as u64);
        let elapsed = self.commit_now(volume)?;
        samples.push((format!("vol_commit_{}_ns", published.len()), ns(elapsed)));
        samples.push((
            format!("vol_commit_{}_pages", published.len()),
            volume.commit_stats().pages_written,
        ));
        let picks: Vec<Key> = (0..published.len())
            .map(|_| published[rng.below(published.len())])
            .collect();
        let mut found = 0u64;
        let reads = volume.pager_stats().reads;
        let start = Instant::now();
        for key in &picks {
            found += u64::from(volume.try_get(key).is_some_and(|v| v.is_some()));
        }
        report.add("vol_try_get_ns", ns(start.elapsed()));
        report.add("vol_try_get_ops", picks.len() as u64);
        let start = Instant::now();
        for key in &picks {
            let value = block_on(
                &self.reactor,
                &self.timers,
                volume.get(&self.reactor, key, &SCOPE),
            )?;
            found += u64::from(value.is_some());
        }
        report.add("vol_get_ns", ns(start.elapsed()));
        report.add("vol_get_ops", picks.len() as u64);
        report.add("vol_get_page_reads", volume.pager_stats().reads - reads);
        // Mutations then commit: CPU of applying a batch to the trees.
        for &mutations in &self.args.mutations {
            let mutations = (mutations as usize).min(published.len());
            let start = Instant::now();
            for key in published.iter().take(mutations) {
                let _ = block_on(
                    &self.reactor,
                    &self.timers,
                    volume.set_retain(&self.reactor, key, 1, &SCOPE),
                )?;
            }
            let update = start.elapsed();
            let elapsed = self.commit_now(volume)?;
            samples.push((format!("vol_m{mutations}_update_ns"), ns(update)));
            samples.push((format!("vol_m{mutations}_commit_ns"), ns(elapsed)));
            samples.push((
                format!("vol_m{mutations}_pages"),
                volume.commit_stats().pages_written,
            ));
        }
        // Eviction: whole slots of 4 KiB records, each removing its keys.
        let evict_reads = volume.pager_stats().reads;
        let start = Instant::now();
        let mut vol_evicted = 0u64;
        let mut vol_evictions = 0u64;
        for _ in 0..8 {
            let Some(removed) = block_on(
                &self.reactor,
                &self.timers,
                volume.evict_one(&self.reactor, now_unix(), &SCOPE),
            )?
            else {
                break;
            };
            vol_evicted += removed.len() as u64;
            vol_evictions += 1;
        }
        report.add("vol_evict_one_ns", ns(start.elapsed()));
        report.add("vol_evict_one_ops", vol_evictions);
        report.add("vol_evicted_entries", vol_evicted);
        report.add(
            "vol_evict_page_reads",
            volume.pager_stats().reads - evict_reads,
        );
        let start = Instant::now();
        let mut removed = 0u64;
        for key in &published {
            let old = block_on(
                &self.reactor,
                &self.timers,
                volume.remove(&self.reactor, key, &SCOPE),
            )?;
            removed += u64::from(old.is_some());
        }
        report.add("vol_remove_ns", ns(start.elapsed()));
        report.add("vol_remove_ops", published.len() as u64);
        let start = Instant::now();
        let _ = volume.usage();
        samples.push(("vol_usage_ns".into(), ns(start.elapsed())));
        phase.end(report);
        for (name, value) in samples {
            report.sample(name, value);
        }
        let pager = volume.pager_stats();
        report.add("pager_hits", pager.hits);
        report.add("pager_misses", pager.misses);
        report.add("pager_bytes", pager.bytes);
        report.add("vol_removed", removed);
        report.add("found", found);
        stats.report(report);
        report.note(
            "cpu: wall and CPU include the record writes used for setup; *_ns/*_ops pairs time the index calls only",
        );
        Ok(())
    }

    /// Cold and hot random lookups through a bounded metadata cache: fill
    /// `--lookup-entries` 4 KiB cache records (real record writes, untimed),
    /// commit, remount with the `--metadata-cache` budget, then time random
    /// `get`s over every key (cold) and over a small hot set (hot), each on
    /// `--qd` concurrent lanes per worker for `--duration` each.
    fn lookup(&self, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        let mounted = self.fresh(Limits::UNLIMITED)?;
        let capacity = (self.slot_count() - 2) * u64::from(SLOT_UNITS) * 3 / 4;
        let target = self.args.lookup_entries.clamp(1, capacity);
        if target < self.args.lookup_entries {
            report.note(format!(
                "lookup: capped at {target} entries by volume capacity"
            ));
        }
        let keys = RefCell::new(Vec::new());
        let fill = RefCell::new(Hot::default());
        let budget = Cell::new(target);
        let active = Cell::new(0);
        let next_key = || self.keys.fresh();
        let sink = PutSink {
            next_key: &next_key,
            keys: Some(&keys),
            budget: &budget,
            active: &active,
        };
        let config = PutConfig {
            tier: Tier::Cache,
            length: UNIT_BYTES as u64,
            evict: false,
            commit_each: false,
            retain_secs: 0,
            stop_on_busy: false,
        };
        let fill_start = Instant::now();
        let mut fill_stats = ExecStats::default();
        // The dirty limit applies backpressure; the lazy committer drains it.
        self.puts(
            &mounted,
            &fill,
            config,
            self.args.qd,
            None,
            true,
            &sink,
            &mut fill_stats,
        )?;
        self.commit_now(&mounted.volume)?;
        report.add("fill_ns", ns(fill_start.elapsed()));
        report.add("fill_entries", mounted.volume.len());
        let keys = keys.into_inner();
        if keys.is_empty() {
            return Err(Error::Busy.into());
        }
        let Mounted { slab, volume } = mounted;
        drop(volume);
        let (volume, mount, mount_time) = self.remount(&slab)?;
        report.sample("mount_ns", ns(mount_time));
        report.add("mount_pages_read", mount.pages_read);
        report.add("cache_budget_bytes", self.args.metadata_cache);
        let mounted = Mounted { slab, volume };
        let mut stats = ExecStats::default();
        let start = phase.start();
        let half = self.args.duration / 2;
        let hot_set: Vec<Key> = {
            let mut rng = Rng(mix(self.index as u64 ^ 0x407));
            (0..64).map(|_| keys[rng.below(keys.len())]).collect()
        };
        for (name, set, deadline) in [
            ("cold_", &keys[..], start + half),
            ("hot_", &hot_set[..], start + half + half),
        ] {
            let before = mounted.volume.pager_stats();
            let hot = RefCell::new(Hot::default());
            let tasks: Vec<Task<'_>> = (0..self.args.qd)
                .map(|lane| {
                    Box::pin(self.lookup_lane(
                        &mounted,
                        &hot,
                        set,
                        deadline,
                        mix(lane as u64 ^ (self.index as u64) << 32 ^ name.len() as u64),
                    )) as Task<'_>
                })
                .collect();
            let lane_start = Instant::now();
            self.run_tasks(tasks, &mut stats)?;
            let elapsed = lane_start.elapsed();
            let after = mounted.volume.pager_stats();
            let h = hot.borrow();
            h.report(report, name);
            let hits = after.hits - before.hits;
            let misses = after.misses - before.misses;
            report.add(format!("{name}page_hits"), hits);
            report.add(format!("{name}page_misses"), misses);
            report.add(format!("{name}page_reads"), after.reads - before.reads);
            report.add(format!("{name}ns"), ns(elapsed));
            report.add(
                format!("{name}hit_permille"),
                (hits * 1000).checked_div(hits + misses).unwrap_or(0),
            );
            report.max(format!("{name}cache_bytes_max"), after.bytes);
        }
        phase.end(report);
        stats.report(report);
        Ok(())
    }

    async fn lookup_lane(
        &self,
        mounted: &Mounted,
        hot: &RefCell<Hot>,
        keys: &[Key],
        deadline: Instant,
        seed: u64,
    ) {
        let mut rng = Rng(seed);
        let volume = &*mounted.volume;
        let mut ops = 0u64;
        while Instant::now() < deadline {
            // Cached lookups complete without awaiting; yield now and then
            // so one lane cannot starve the others.
            ops += 1;
            if ops.is_multiple_of(64) {
                uring_runtime::drivers::yield_now().await;
            }
            let key = keys[rng.below(keys.len())];
            let start = Instant::now();
            match volume.get(&self.reactor, &key, &SCOPE).await {
                Ok(Some(_)) => {
                    let mut h = hot.borrow_mut();
                    h.ops += 1;
                    h.get.record_duration(start.elapsed());
                }
                Ok(None) => hot.borrow_mut().misses += 1,
                Err(error) => hot.borrow_mut().error("get", error),
            }
        }
    }

    /// Allocate and write 4 KiB reservations without publishing them.
    async fn reserve_lane(
        &self,
        mounted: &Mounted,
        out: &RefCell<Vec<(page_alloc::volume::Reservation, Key, u64)>>,
        budget: &Cell<u64>,
    ) {
        let volume = &*mounted.volume;
        let Ok(mut buffer) = self.buffer(&mounted.slab, UNIT_BYTES as u64) else {
            return;
        };
        while budget.get() > 0 {
            budget.set(budget.get() - 1);
            let Ok(reservation) = volume.allocate(Tier::Cache, UNIT_BYTES as u64) else {
                return;
            };
            let key = self.keys.fresh();
            let generation = volume.next_generation();
            stamp(&mut buffer, &key, generation);
            match volume
                .write(&self.reactor, &reservation, buffer, &SCOPE)
                .await
            {
                Ok(returned) => buffer = returned,
                Err(_) => return,
            }
            out.borrow_mut().push((reservation, key, generation));
        }
    }
}
