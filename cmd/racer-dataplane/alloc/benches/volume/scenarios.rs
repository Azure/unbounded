//! Per-worker setup and the benchmark scenarios.

use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Barrier;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use page_alloc::volume::{
    Key, Limits, NodeAddr, SLOT_BYTES, SLOT_UNITS, Slots, Tier, Tree, UNIT_BYTES, Value, Volume,
    VolumeOptions,
};
use page_alloc::{AlignedBuffer, Charge, Error, Slab};

use crate::args::{Args, Scenario};
use crate::exec::{BenchError, BenchReactor, ExecStats, SCOPE, Task, Timers, block_on, run};
use crate::plan::WorkerPlan;
use crate::stats::{Hist, Report, ThreadUsage};

/// Slots `allocate_evicting` may evict per call, as `EVICTION_BUDGET` in
/// `src/store/disk.rs`.
pub const EVICTION_BUDGET: usize = 64;

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
    shadow: Option<&'b RefCell<Tree>>,
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

/// Encode the shadow tree's dirty nodes like a commit would. Returns the
/// prepare time and node count.
fn shadow_commit(tree: &RefCell<Tree>, seq: u64) -> Result<(Duration, u64)> {
    let mut tree = tree.borrow_mut();
    let mut next = 0u64;
    let mut alloc = || {
        next += 1;
        Ok(NodeAddr::new(
            1 + (next / u64::from(SLOT_UNITS)) as u32,
            (next % u64::from(SLOT_UNITS)) as u16,
        ))
    };
    let start = Instant::now();
    let plan = tree.prepare_commit(seq, &mut alloc)?;
    let elapsed = start.elapsed();
    let nodes = plan.writes.len() as u64;
    tree.commit_succeeded(plan);
    drop(tree.take_freed());
    Ok((elapsed, nodes))
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
                VolumeOptions {
                    limits,
                    volume_id,
                    format: true,
                    charge: {
                        let charge = self.charge();
                        Rc::new(move |_| Ok(charge))
                    },
                },
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
                volume.allocate_evicting(config.tier, config.length, now_unix(), EVICTION_BUDGET)
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
            if let Some(shadow) = sink.shadow
                && let Some(value) = volume.get(&key)
            {
                shadow.borrow_mut().insert(key, value);
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
            if now.saturating_duration_since(last) < interval {
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
            shadow: None,
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
            shadow: None,
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
            shadow: None,
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
            let value = volume.get(&key);
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
            let shadow = RefCell::new(Tree::new(mounted.volume.volume_id()));
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
                    shadow: Some(&shadow),
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
            let mut seq = 1;
            let (prepare, nodes) = shadow_commit(&shadow, seq)?;
            let elapsed = self.commit_now(&mounted.volume)?;
            report.sample(format!("{label}_full_commit_ns"), ns(elapsed));
            report.sample(format!("{label}_full_prepare_ns"), ns(prepare));
            report.add(format!("{label}_full_nodes"), nodes);
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
                        shadow: Some(&shadow),
                        budget: &budget,
                        active: &active,
                    },
                    &mut stats,
                )?;
                seq += 1;
                let (prepare, nodes) = shadow_commit(&shadow, seq)?;
                let elapsed = self.commit_now(&mounted.volume)?;
                report.sample(format!("{label}_m{mutations}_commit_ns"), ns(elapsed));
                report.sample(format!("{label}_m{mutations}_prepare_ns"), ns(prepare));
                report.add(format!("{label}_m{mutations}_nodes"), nodes);
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
            {
                let shadow = shadow.borrow();
                report.max(
                    format!("{label}_tree_height_max"),
                    u64::from(shadow.height()),
                );
                report.add(format!("{label}_tree_nodes"), shadow.node_count() as u64);
            }
            self.usage(&mounted.volume, report, &format!("{label}_end_"));
            // Remount: drop the volume, then mount the same slab.
            let Mounted { slab, volume, .. } = mounted;
            drop(volume);
            let start = Instant::now();
            let (volume, mount) = block_on(
                &self.reactor,
                &self.timers,
                Volume::mount(
                    &self.reactor,
                    slab.clone(),
                    VolumeOptions {
                        limits: Limits::UNLIMITED,
                        volume_id: [0; 16],
                        format: false,
                        charge: {
                            let charge = self.charge();
                            Rc::new(move |_| Ok(charge))
                        },
                    },
                    &SCOPE,
                ),
            )?;
            let elapsed = start.elapsed();
            report.sample(format!("{label}_mount_ns"), ns(elapsed));
            report.add(format!("{label}_mount_entries"), mount.entries);
            report.add(
                format!("{label}_mount_invalid_copies"),
                mount.invalid_copies,
            );
            report.add(format!("{label}_mount_pending_nodes"), mount.pending_nodes);
            if mount.entries != entries || mount.formatted || mount.dirty {
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

    /// CPU cost of index operations, apart from device I/O.
    fn cpu(&self, phase: &mut Phase<'_>, report: &mut Report) -> Result<()> {
        phase.start();
        let n = self.args.cpu_entries.max(1);
        let mut rng = Rng(mix(self.index as u64 ^ 0xc0));
        let keys: Vec<Key> = (0..n).map(|i| self.keys.key(1 << 40 | i)).collect();
        let value = |i: u64, generation: u64| Value {
            slot: 1 + (i / u64::from(SLOT_UNITS)) as u32,
            unit: (i % u64::from(SLOT_UNITS)) as u16,
            class: 0,
            tier: Tier::Cache,
            length: UNIT_BYTES as u32,
            flags: 0,
            generation,
            retain_until: 0,
        };
        let mut timed = |name: &str, ops: u64, f: &mut dyn FnMut()| {
            let start = Instant::now();
            f();
            report.add(format!("{name}_ns"), ns(start.elapsed()));
            report.add(format!("{name}_ops"), ops);
        };

        // Part A: the in-memory B+tree alone.
        let tree = RefCell::new(Tree::new([1; 16]));
        timed("tree_insert", n, &mut || {
            let mut tree = tree.borrow_mut();
            for (i, key) in keys.iter().enumerate() {
                tree.insert(*key, value(i as u64, i as u64 + 1));
            }
        });
        let order: Vec<usize> = (0..n).map(|_| rng.below(n as usize)).collect();
        let mut found = 0u64;
        timed("tree_get", n, &mut || {
            let tree = tree.borrow();
            for &i in &order {
                found += u64::from(tree.get(&keys[i]).is_some());
            }
        });
        let (prepare, nodes) = shadow_commit(&tree, 1)?;
        let mut samples: Vec<(String, u64)> = vec![
            ("tree_full_prepare_ns".into(), ns(prepare)),
            ("tree_full_nodes".into(), nodes),
        ];
        let mut seq = 1;
        for &mutations in &self.args.mutations {
            let picks: Vec<usize> = (0..mutations).map(|_| rng.below(n as usize)).collect();
            let start = Instant::now();
            {
                let mut tree = tree.borrow_mut();
                for &i in &picks {
                    tree.insert(keys[i], value(i as u64, n + i as u64 + 1));
                }
            }
            let update = start.elapsed();
            seq += 1;
            let (prepare, nodes) = shadow_commit(&tree, seq)?;
            samples.push((format!("tree_m{mutations}_update_ns"), ns(update)));
            samples.push((format!("tree_m{mutations}_prepare_ns"), ns(prepare)));
            samples.push((format!("tree_m{mutations}_nodes"), nodes));
        }
        let height = tree.borrow().height();
        let tree_nodes = tree.borrow().node_count();
        timed("tree_remove", n / 2, &mut || {
            let mut tree = tree.borrow_mut();
            for key in keys.iter().take((n / 2) as usize) {
                tree.remove(key);
            }
        });
        drop(tree);

        // Part B: the slot allocator alone.
        let slot_count = (n / u64::from(SLOT_UNITS) + 8).max(64);
        let mut slots = Slots::new(slot_count, Limits::UNLIMITED)?;
        let mut allocated = 0u64;
        timed("slots_alloc_commit", n, &mut || {
            for key in &keys {
                let Ok(allocation) = slots.allocate(Tier::Cache, UNIT_BYTES as u64) else {
                    break;
                };
                if slots
                    .commit_record(allocation.slot, allocation.unit, *key, 0)
                    .is_err()
                {
                    break;
                }
                allocated += 1;
            }
        });
        let mut evicted_keys = 0u64;
        let mut evictions = 0u64;
        let now = now_unix();
        timed("slots_evict", 32, &mut || {
            for _ in 0..32 {
                let Some(eviction) = slots.evict_candidate_in(now, None) else {
                    break;
                };
                evicted_keys += eviction.keys.len() as u64;
                let _ = slots.finish_eviction(eviction.slot);
                evictions += 1;
            }
        });
        let start = Instant::now();
        let _ = slots.usage();
        samples.push(("slots_usage_ns".into(), ns(start.elapsed())));
        drop(slots);

        // Part C: the volume API, timing only the synchronous calls.
        let mounted = self.fresh(Limits::UNLIMITED)?;
        let volume = &*mounted.volume;
        let small = (n.min(1 << 14)) as usize;
        let start = Instant::now();
        for _ in 0..small {
            drop(volume.allocate(Tier::Cache, UNIT_BYTES as u64));
        }
        report.add("vol_alloc_abort_ns", ns(start.elapsed()));
        report.add("vol_alloc_abort_ops", small as u64);
        let written = RefCell::new(Vec::with_capacity(small));
        let budget = Cell::new(small as u64);
        let mut stats = ExecStats::default();
        let tasks: Vec<Task<'_>> = (0..self.args.qd)
            .map(|_| Box::pin(self.reserve_lane(&mounted, &written, &budget)) as Task<'_>)
            .collect();
        self.run_tasks(tasks, &mut stats)?;
        let written = written.into_inner();
        let count = written.len() as u64;
        let mut published = Vec::with_capacity(written.len());
        let start = Instant::now();
        for (reservation, key, generation) in written {
            if volume
                .publish(key, reservation, UNIT_BYTES as u32, generation, 0)
                .is_ok()
            {
                published.push(key);
            }
        }
        report.add("vol_publish_ns", ns(start.elapsed()));
        report.add("vol_publish_ops", count);
        let picks: Vec<Key> = (0..published.len())
            .map(|_| published[rng.below(published.len())])
            .collect();
        let start = Instant::now();
        for key in &picks {
            found += u64::from(volume.get(key).is_some());
        }
        report.add("vol_get_ns", ns(start.elapsed()));
        report.add("vol_get_ops", picks.len() as u64);
        let elapsed = self.commit_now(volume)?;
        samples.push((format!("vol_commit_{count}_ns"), ns(elapsed)));
        // Eviction: whole slots of 4 KiB records, each removing its keys.
        let start = Instant::now();
        let mut vol_evicted = 0u64;
        let mut vol_evictions = 0u64;
        for _ in 0..8 {
            let Some(removed) = volume.evict_one(now_unix()) else {
                break;
            };
            vol_evicted += removed.len() as u64;
            vol_evictions += 1;
        }
        report.add("vol_evict_one_ns", ns(start.elapsed()));
        report.add("vol_evict_one_ops", vol_evictions);
        report.add("vol_evicted_entries", vol_evicted);
        let start = Instant::now();
        let mut removed = 0u64;
        for key in &published {
            removed += u64::from(volume.remove(key).is_some());
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
        report.add("tree_height_max", u64::from(height));
        report.add("tree_nodes", tree_nodes as u64);
        report.add("slots_allocated", allocated);
        report.add("slots_evictions", evictions);
        report.add("slots_evicted_keys", evicted_keys);
        report.add("vol_removed", removed);
        report.add("found", found);
        stats.report(report);
        report.note(
            "cpu: wall and CPU include the I/O used to set up part C; *_ns/*_ops pairs are synchronous calls only",
        );
        Ok(())
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
