//! Deterministic simulation of the extent volume under faults and crashes.
//!
//! A seeded workload allocates, writes, and publishes records (each record
//! header holds its key and generation), reads, removes, changes retention,
//! evicts, and commits (often several commits at once, interleaved with
//! writes). It injects I/O errors and loses power at random points, including
//! in the middle of commits. Some seeds also flip a bit in a metadata page or
//! superblock on the durable image before remounting.
//!
//! The runner mirrors the index in a model and logs every change. After each
//! remount it checks:
//!
//! - The mounted index (plus entries a scrub dropped) equals the model at
//!   some point at or after the call of the last successful commit. Corruption
//!   seeds accept one commit less, and inside lost ranges (found by a scrub)
//!   accept bounded loss and only values that were once published.
//! - Every storage entry reads back a record with its key and generation,
//!   unless it was evicted or its retention had passed (expired storage slots
//!   are reused at once on eviction).
//!   Cache entries from before the crash may point at reused space; entries
//!   published since the mount must match.
//! - No two entries overlap, and each lies in a data slot of its tier and
//!   class.
//! - Generations keep increasing, and the volume keeps working.
//!
//! Replay one case with `DST_SEED=<seed>`; otherwise run `DST_SEEDS` seeds
//! from `DST_START_SEED` (default zero). Tune with `DST_STEPS`.

#![cfg(feature = "simulation")]

mod common;

use common::*;
use page_alloc::volume::{
    Key, Limits, MountReport, RETAIN_FOREVER, Reservation, SLOT_BYTES, ScrubReport, SlotKind,
    Superblock, Tier, UNIT_BYTES, Value, Volume, class_units,
};
use page_alloc::{Error, Slab};
use std::{
    collections::{BTreeMap, HashSet},
    ops::Bound,
    rc::Rc,
    task::{Context, Poll, Waker},
};
use uring_runtime::reactor::simulation::{Fault, Simulation, disk::DiskState};

const SLOTS: u64 = 12;
const CACHES: u8 = 3;
const KEYS: u32 = 250;
const LENGTHS: [u64; 6] = [100, 4096, 5000, 9000, 30_000, 65_536];

/// Stable splitmix64.
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        z ^ (z >> 31)
    }

    fn below(&mut self, bound: u64) -> u64 {
        self.next() % bound
    }

    fn chance(&mut self, percent: u64) -> bool {
        self.below(100) < percent
    }
}

type Index = BTreeMap<Key, Value>;

/// A write in flight, published when it completes.
struct Write<'r> {
    key: Key,
    generation: u64,
    length: u64,
    retain_until: u64,
    task: Task<'r, (Reservation, Result<(), TestError>)>,
    /// Output of a task that completed during [`Run::block`].
    done: Option<(Reservation, Result<(), TestError>)>,
}

/// A commit in flight with the model at its call.
struct Commit<'r> {
    index: usize,
    snapshot: Index,
    task: Task<'r, Result<(), TestError>>,
    /// Output of a task that completed during [`Run::block`].
    done: Option<Result<(), TestError>>,
}

struct Run<'r> {
    seed: u64,
    rng: Rng,
    sim: &'r Simulation,
    reactor: &'r TestReactor,
    corruption: bool,
    volume: Rc<Volume<()>>,
    slab: Rc<Slab<()>>,
    now: u64,
    model: Index,
    /// Every change: key and new value.
    log: Vec<(Key, Option<Value>)>,
    /// Call index and model of the latest successful commit, and the one
    /// before it.
    durable: (usize, Index),
    previous: (usize, Index),
    /// Every value ever published.
    published: HashSet<(Key, u64)>,
    /// Storage values removed by eviction; older trees may still point at
    /// them after their space was reused.
    evicted_storage: HashSet<(Key, u64)>,
    /// Cache values present at the last mount; they may point at reused space.
    stale_cache: HashSet<(Key, u64)>,
    max_generation: u64,
    /// Largest generation in any successfully committed state.
    committed_generation: u64,
    writes: Vec<Write<'r>>,
    commits: Vec<Commit<'r>>,
    successes_since_corruption: u64,
    /// `commit_stats().succeeded` last observed on this mount.
    succeeded_seen: u64,
    corruptions: u64,
    /// A scrub found a lost range on some mount.
    seen_lost: bool,
    crashes: u64,
    budget: usize,
}

fn key(cache: u8, n: u32) -> Key {
    Key::new([cache; 8], [n as u8; 12], n)
}

fn poll<T>(task: &mut Task<'_, T>) -> Poll<T> {
    task.as_mut().poll(&mut Context::from_waker(Waker::noop()))
}

impl<'r> Run<'r> {
    fn new(seed: u64, sim: &'r Simulation, reactor: &'r TestReactor) -> Self {
        let (volume, slab, report) = mount(reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert!(report.formatted);
        Self {
            seed,
            rng: Rng(seed),
            sim,
            reactor,
            corruption: seed % 4 == 3,
            volume,
            slab,
            now: 1000,
            model: Index::new(),
            log: Vec::new(),
            durable: (0, Index::new()),
            previous: (0, Index::new()),
            published: HashSet::new(),
            evicted_storage: HashSet::new(),
            stale_cache: HashSet::new(),
            max_generation: 0,
            committed_generation: 0,
            writes: Vec::new(),
            commits: Vec::new(),
            successes_since_corruption: 0,
            succeeded_seen: 0,
            corruptions: 0,
            seen_lost: false,
            crashes: 0,
            budget: 8,
        }
    }

    fn change(&mut self, key: Key, value: Option<Value>) {
        match value {
            Some(value) => self.model.insert(key, value),
            None => self.model.remove(&key),
        };
        self.log.push((key, value));
    }

    fn evicted(&mut self, removed: Vec<(Key, Value)>) {
        for (key, value) in removed {
            assert_eq!(self.model.get(&key), Some(&value), "evicted unknown entry");
            if value.tier == Tier::Storage {
                assert!(value.retain_until < self.now, "evicted protected storage");
                self.evicted_storage.insert((key, value.generation));
            }
            self.change(key, None);
        }
    }

    fn random_key(&mut self) -> Key {
        key(
            self.rng.below(u64::from(CACHES)) as u8,
            self.rng.below(u64::from(KEYS)) as u32,
        )
    }

    fn retain(&mut self) -> u64 {
        match self.rng.below(4) {
            0 => 0,
            1 => self.now + self.rng.below(50),
            2 => self.now + 1000,
            _ => RETAIN_FOREVER,
        }
    }

    fn start_write(&mut self) {
        let tier = if self.rng.chance(50) {
            Tier::Storage
        } else {
            Tier::Cache
        };
        let length = if self.rng.chance(1) {
            SLOT_BYTES - 64
        } else {
            LENGTHS[self.rng.below(LENGTHS.len() as u64) as usize]
        };
        let reservation = match self.volume.allocate(tier, length) {
            Ok(r) => r,
            Err(Error::Busy) => {
                let now = self.now;
                let (result, removed) = match self.run(|volume, reactor| {
                    Box::pin(async move {
                        volume
                            .allocate_evicting(reactor, tier, length, now, 4, &SCOPE)
                            .await
                    })
                }) {
                    Ok(done) => done,
                    // Injected read faults.
                    Err(TestError::Runtime(_)) => return,
                    Err(error) => panic!("allocate_evicting: {error:?}"),
                };
                self.evicted(removed);
                match result {
                    Ok(r) => r,
                    Err(Error::Busy) => return,
                    Err(error) => panic!("allocate: {error:?}"),
                }
            }
            Err(error) => panic!("allocate: {error:?}"),
        };
        let key = self.random_key();
        let generation = self.volume.next_generation();
        assert!(generation > self.max_generation, "generation went back");
        self.max_generation = generation;
        let buffer = record(&self.slab, &key, generation, length);
        let volume = self.volume.clone();
        let reactor = self.reactor;
        let task: Task<'r, _> = Box::pin(async move {
            let result = volume
                .write(reactor, &reservation, buffer, &SCOPE)
                .await
                .map(drop);
            (reservation, result)
        });
        let retain_until = if tier == Tier::Storage {
            self.retain()
        } else {
            0
        };
        self.writes.push(Write {
            key,
            generation,
            length,
            retain_until,
            task,
            done: None,
        });
    }

    fn start_commit(&mut self) {
        let volume = self.volume.clone();
        let reactor = self.reactor;
        self.commits.push(Commit {
            index: self.log.len(),
            snapshot: self.model.clone(),
            task: Box::pin(async move { volume.commit(reactor, &SCOPE).await }),
            done: None,
        });
    }

    /// Poll every task and the reactor once; finish completed tasks.
    fn turn(&mut self) {
        let mut index = 0;
        while index < self.writes.len() {
            let write = &mut self.writes[index];
            let output = match write.done.take() {
                Some(output) => Poll::Ready(output),
                None => poll(&mut write.task),
            };
            if let Poll::Ready((reservation, result)) = output {
                let write = self.writes.swap_remove(index);
                if result.is_ok() {
                    self.volume
                        .publish(
                            write.key,
                            reservation,
                            write.length as u32,
                            write.generation,
                            write.retain_until,
                        )
                        .unwrap();
                    let value = self.volume.try_get(&write.key).unwrap().unwrap();
                    assert_eq!(value.generation, write.generation);
                    self.published.insert((write.key, value.generation));
                    self.change(write.key, Some(value));
                }
            } else {
                index += 1;
            }
        }
        let mut index = 0;
        while index < self.commits.len() {
            let commit = &mut self.commits[index];
            let output = match commit.done.take() {
                Some(output) => Poll::Ready(output),
                None => poll(&mut commit.task),
            };
            if let Poll::Ready(result) = output {
                let commit = self.commits.swap_remove(index);
                if result.is_ok() {
                    let max = commit.snapshot.values().map(|v| v.generation).max();
                    self.committed_generation = self.committed_generation.max(max.unwrap_or(0));
                    // Concurrent callers share attempts; shift the previous
                    // state only when a new attempt succeeded.
                    let succeeded = self.volume.commit_stats().succeeded;
                    if succeeded != self.succeeded_seen {
                        self.successes_since_corruption += succeeded - self.succeeded_seen;
                        self.succeeded_seen = succeeded;
                        self.previous =
                            std::mem::replace(&mut self.durable, (commit.index, commit.snapshot));
                    } else if commit.index >= self.durable.0 {
                        self.durable = (commit.index, commit.snapshot);
                    }
                }
            } else {
                index += 1;
            }
        }
        self.reactor.poll_budgeted(self.budget).unwrap();
    }

    /// Run `op` like an executor would: poll it, the in-flight writes and
    /// commits, and the reactor until it completes. A page fault led by one
    /// task may be awaited by another, so polling only `op` could stall.
    ///
    /// Tasks that complete meanwhile are not processed (published or
    /// recorded) until the next [`Run::turn`], so the model sees the
    /// operation's changes first, as the volume does.
    fn block<T>(&mut self, mut op: Task<'r, T>) -> T {
        for _ in 0..10_000_000 {
            if let Poll::Ready(value) = poll(&mut op) {
                return value;
            }
            for write in &mut self.writes {
                if write.done.is_none()
                    && let Poll::Ready(output) = poll(&mut write.task)
                {
                    write.done = Some(output);
                }
            }
            for commit in &mut self.commits {
                if commit.done.is_none()
                    && let Poll::Ready(output) = poll(&mut commit.task)
                {
                    commit.done = Some(output);
                }
            }
            self.reactor.poll_budgeted(self.budget).unwrap();
        }
        panic!("operation did not complete");
    }

    /// [`Run::block`] on an operation built from the volume and reactor.
    fn run<T>(&mut self, build: impl FnOnce(Rc<Volume<()>>, &'r TestReactor) -> Task<'r, T>) -> T {
        let op = build(self.volume.clone(), self.reactor);
        self.block(op)
    }

    /// Every index entry. Retries injected read faults.
    fn all(&mut self) -> Index {
        loop {
            match self.run(|volume, reactor| {
                Box::pin(async move {
                    volume
                        .range(
                            reactor,
                            Bound::Unbounded,
                            Bound::Unbounded,
                            usize::MAX,
                            &SCOPE,
                        )
                        .await
                })
            }) {
                Ok(entries) => return entries.into_iter().collect(),
                Err(TestError::Runtime(_)) => continue,
                Err(error) => panic!("range: {error:?}"),
            }
        }
    }

    /// Current value of `key`, retrying injected read faults.
    fn lookup(&mut self, key: &Key) -> Option<Value> {
        loop {
            let key = *key;
            match self.run(|volume, reactor| {
                Box::pin(async move { volume.get(reactor, &key, &SCOPE).await })
            }) {
                Ok(value) => return value,
                Err(TestError::Runtime(_)) => continue,
                Err(error) => panic!("get: {error:?}"),
            }
        }
    }

    fn check_model(&mut self) {
        let actual = self.all();
        assert!(actual == self.model, "index diverged from the model");
        // Exact unless a publish could not tell whether it replaced an entry.
        assert!(self.volume.len() >= self.model.len() as u64);
    }

    fn read_check(&mut self, key: Key, value: Value) {
        let buffer = self.slab.allocate(UNIT_BYTES, ()).unwrap();
        match drive(
            self.reactor,
            self.volume.read(self.reactor, &value, buffer, &SCOPE),
        ) {
            Ok(buffer) => {
                let ok = record_matches(buffer.as_slice(), &key, value.generation);
                let allowed = match value.tier {
                    Tier::Cache => self.stale_cache.contains(&(key, value.generation)),
                    // Expired storage is not protected: evicting its slot
                    // (even after the entry was removed) reuses it at once,
                    // and an older tree may still point at it.
                    Tier::Storage => {
                        self.evicted_storage.contains(&(key, value.generation))
                            || value.retain_until < self.now
                    }
                };
                assert!(ok || allowed, "{key:?} {value:?} reads another record");
            }
            // Injected read faults.
            Err(TestError::Runtime(_)) => {}
            Err(error) => panic!("read: {error:?}"),
        }
    }

    fn step(&mut self) {
        let roll = self.rng.below(100);
        match roll {
            0..=39 => {
                if self.writes.len() < 8 {
                    self.start_write();
                }
            }
            40..=46 => {
                if let Some((key, value)) = self.pick() {
                    self.read_check(key, value);
                }
            }
            47..=51 => {
                let key = self.random_key();
                let Ok(old) = self.run(|volume, reactor| {
                    Box::pin(async move { volume.remove(reactor, &key, &SCOPE).await })
                }) else {
                    return;
                };
                assert_eq!(old.as_ref(), self.model.get(&key));
                if old.is_some() {
                    self.change(key, None);
                }
            }
            52..=54 => {
                if let Some((key, mut value)) = self.pick() {
                    let stale = self.rng.chance(30);
                    if stale {
                        value.generation += 1;
                    }
                    let Ok(removed) = self.run(|volume, reactor| {
                        Box::pin(
                            async move { volume.remove_if(reactor, &key, &value, &SCOPE).await },
                        )
                    }) else {
                        return;
                    };
                    assert_eq!(removed, !stale);
                    if removed {
                        self.change(key, None);
                    }
                }
            }
            55 if self.rng.chance(20) => {
                let cache = self.rng.below(u64::from(CACHES)) as u8;
                let low = Key::new([cache; 8], [0; 12], 0);
                let high = Key::new([cache + 1; 8], [0; 12], 0);
                let Ok(removed) = self.run(|volume, reactor| {
                    Box::pin(async move {
                        let mut removed = Vec::new();
                        volume
                            .remove_range(
                                reactor,
                                Bound::Included(low),
                                Bound::Excluded(high),
                                |batch| removed.extend_from_slice(batch),
                                &SCOPE,
                            )
                            .await
                            .map(|_| removed)
                    })
                }) else {
                    // A read fault part way: the batches before it are removed.
                    self.resync();
                    return;
                };
                for (key, value) in removed {
                    assert_eq!(self.model.get(&key), Some(&value));
                    self.change(key, None);
                }
            }
            56..=61 => {
                if let Some((key, _)) = self.pick() {
                    let until = self.retain();
                    let Ok(found) = self.run(|volume, reactor| {
                        Box::pin(
                            async move { volume.set_retain(reactor, &key, until, &SCOPE).await },
                        )
                    }) else {
                        return;
                    };
                    assert!(found);
                    let value = self.lookup(&key).unwrap();
                    assert_eq!(value.retain_until, until);
                    self.change(key, Some(value));
                }
            }
            62..=65 => {
                let now = self.now;
                match self.run(|volume, reactor| {
                    Box::pin(async move { volume.evict_one(reactor, now, &SCOPE).await })
                }) {
                    Ok(Some(removed)) => self.evicted(removed),
                    Ok(None) => {}
                    // A read fault stops the eviction; entries removed so
                    // far stay removed.
                    Err(_) => self.resync(),
                }
            }
            66..=75 => {
                let count = if self.rng.chance(30) { 3 } else { 1 };
                for _ in 0..count {
                    if self.commits.len() < 6 {
                        self.start_commit();
                    }
                }
            }
            76..=79 => self.now += self.rng.below(40),
            80..=82 => {
                let operation = ["write", "fsync", "read"][self.rng.below(3) as usize];
                self.sim.inject(operation, Fault::Errno(libc::EIO)).unwrap();
            }
            83 => self.budget = 1 + self.rng.below(16) as usize,
            84 => self.crash(),
            _ => {
                for _ in 0..self.rng.below(6) {
                    self.turn();
                }
            }
        }
        self.turn();
    }

    /// Take the volume's index as the model after an operation failed part
    /// way. Its changes are all valid (removals of entries the model holds).
    fn resync(&mut self) {
        let actual = self.all();
        for (key, value) in self.model.clone() {
            if !actual.contains_key(&key) {
                if value.tier == Tier::Storage {
                    self.evicted_storage.insert((key, value.generation));
                }
                self.change(key, None);
            }
        }
        assert!(
            actual == self.model,
            "a failed operation changed more than it removed"
        );
    }

    fn pick(&mut self) -> Option<(Key, Value)> {
        if self.model.is_empty() {
            return None;
        }
        let n = self.rng.below(self.model.len() as u64) as usize;
        self.model.iter().nth(n).map(|(k, v)| (*k, *v))
    }

    /// Lose power with tasks in flight, maybe corrupt one unit, and remount.
    fn crash(&mut self) {
        self.crashes += 1;
        self.writes.clear();
        self.commits.clear();
        self.sim.disk().crash().unwrap();
        while self.reactor.in_flight() != 0 {
            self.reactor.poll_budgeted(64).unwrap();
        }
        let mut corrupted_superblock = false;
        if self.corruption && self.successes_since_corruption >= 2 && self.rng.chance(50) {
            corrupted_superblock = self.corrupt();
            self.successes_since_corruption = 0;
            self.corruptions += 1;
            self.sim.disk().crash().unwrap();
        }
        // Faults left in the queue must not fail the mount's superblock reads.
        let (volume, slab, report) = loop {
            match mount(self.reactor, SLOTS, Limits::UNLIMITED) {
                Ok(mounted) => break mounted,
                Err(TestError::Runtime(_)) => self.sim.disk().crash().unwrap(),
                Err(error) => panic!("mount: {error:?}"),
            }
        };
        self.volume = volume;
        self.slab = slab;
        self.succeeded_seen = 0;
        // The model knows both superblocks' states only after two commits.
        self.successes_since_corruption = 0;
        let scrub = loop {
            match self
                .run(|volume, reactor| Box::pin(async move { volume.scrub(reactor, &SCOPE).await }))
            {
                Ok(scrub) => break scrub,
                Err(TestError::Runtime(_)) => continue,
                Err(error) => panic!("scrub: {error:?}"),
            }
        };
        self.verify(&report, &scrub, corrupted_superblock);
    }

    /// Flip one bit in a metadata page or superblock unit of the meta slot.
    /// Returns whether a superblock was hit.
    fn corrupt(&mut self) -> bool {
        let path = std::path::Path::new(PATH);
        let units = 2 + self.rng.below(400);
        // Superblocks: the newest, so the mount falls back to the older tree
        // and checks that nothing it references was reused.
        let unit = if self.rng.chance(50) {
            let seq = |unit: u64| {
                let bytes = self
                    .sim
                    .disk()
                    .read(
                        path,
                        unit * UNIT_BYTES as u64,
                        UNIT_BYTES,
                        DiskState::Durable,
                    )
                    .unwrap();
                Superblock::decode(&bytes.try_into().unwrap()).map(|sb| sb.seq)
            };
            if seq(0) > seq(1) { 0 } else { 1 }
        } else {
            units
        };
        let offset = unit * UNIT_BYTES as u64 + self.rng.below(UNIT_BYTES as u64);
        let byte = self
            .sim
            .disk()
            .read(path, offset, 1, DiskState::Durable)
            .unwrap()[0];
        let flipped = byte ^ (1 << self.rng.below(8));
        self.sim
            .disk()
            .corrupt(path, offset, &[flipped], DiskState::Durable)
            .unwrap();
        unit < 2
    }

    fn verify(&mut self, report: &MountReport, scrub: &ScrubReport, corrupted_superblock: bool) {
        assert!(!report.formatted, "volume was reformatted");
        let mut mounted = self.all();
        let loose = self.corruption;
        // A lost leaf stays lost until a commit rewrites its range, so after
        // one, later mounts may still see it.
        let clean = !self.seen_lost;
        self.seen_lost |= !scrub.lost.is_empty() || !scrub.lost_records.is_empty();
        if !loose || (corrupted_superblock && clean) {
            // Without page corruption, every page either root set references
            // is intact: nothing was reused early. Node bitmaps may be
            // rebuilt from the trees: an attempt that failed after writing
            // its superblock leaves a superblock that may be durable, and
            // the retry overwrites the bitmap copy it names.
            assert!(
                report.lost.is_empty()
                    && !report.root_lost
                    && report.invalid_copies == 0
                    && scrub.lost.is_empty()
                    && scrub.lost_records.is_empty()
                    && scrub.invalid_copies == 0
                    && scrub.dropped.is_empty(),
                "{report:?} {scrub:?}"
            );
            if !loose {
                assert_eq!(mounted.len() as u64, report.entries);
            }
        }
        if !loose {
            assert_eq!(scrub.entries, report.entries);
        }
        assert_eq!(
            self.volume.len(),
            mounted.len() as u64 - scrub.dropped.len() as u64
        );
        for (key, value) in &scrub.dropped {
            assert!(loose, "dropped {key:?} {value:?}");
            mounted.entry(*key).or_insert(*value);
        }
        assert!(loose || !corrupted_superblock);
        let lost = scrub.lost.clone();
        let in_lost = |key: &Key| {
            lost.iter()
                .any(|(low, high)| key >= low && high.is_none_or(|h| *key < h))
        };
        let base = if loose { &self.previous } else { &self.durable };
        let matched = self.find_match(base, &mounted, &in_lost);
        let Some(state) = matched else {
            eprintln!(
                "durable {} previous {} log {} mounted {mounted:?}\nprevious {:?}\nlog {:?}",
                self.durable.0,
                self.previous.0,
                self.log.len(),
                self.previous.1,
                self.log
            );
            panic!(
                "seed {}: mounted index matches no state since the last commit: {report:?}",
                self.seed
            );
        };
        // Inside lost ranges: only published values, bounded loss.
        let mut missing = 0;
        for (key, value) in &state {
            if in_lost(key) && mounted.get(key) != Some(value) {
                missing += 1;
            }
        }
        for (key, value) in &mounted {
            if in_lost(key) {
                assert!(
                    self.published.contains(&(*key, value.generation)),
                    "salvaged an unknown value"
                );
            }
        }
        assert!(
            missing <= 72 * self.corruptions.max(1) as usize || !loose,
            "lost {missing} entries"
        );
        // Records and placement.
        let mut extents = Vec::new();
        for (key, value) in self.all() {
            let state = self.volume.slot_state(value.slot).unwrap();
            assert!(
                state.kind
                    == SlotKind::Data {
                        tier: value.tier,
                        class: value.class
                    }
                    || (loose && state.kind == SlotKind::Quarantined),
                "{key:?} {value:?} in {state:?}"
            );
            let units = class_units(value.class).unwrap();
            extents.push((value.slot, u32::from(value.unit), units));
            if value.tier == Tier::Cache {
                self.stale_cache.insert((key, value.generation));
            }
            self.read_check(key, value);
        }
        extents.sort();
        for pair in extents.windows(2) {
            assert!(
                pair[0].0 != pair[1].0 || pair[0].1 + pair[0].2 <= pair[1].1,
                "entries overlap: {pair:?}"
            );
        }
        // Continue from the mounted index.
        self.model = self.all();
        self.log.clear();
        self.durable = (0, self.model.clone());
        self.previous = (0, self.model.clone());
        let generation = self.volume.next_generation();
        let mounted_max = self.model.values().map(|v| v.generation).max().unwrap_or(0);
        assert!(
            generation > self.committed_generation.max(mounted_max),
            "generation went back"
        );
        self.max_generation = generation;
    }

    /// The model state at or after `base` that equals `mounted` outside lost
    /// ranges.
    fn find_match(
        &self,
        base: &(usize, Index),
        mounted: &Index,
        in_lost: &dyn Fn(&Key) -> bool,
    ) -> Option<Index> {
        let mut state = base.1.clone();
        let differs = |state: &Index, key: &Key| state.get(key) != mounted.get(key);
        let mut diff = state
            .keys()
            .chain(mounted.keys())
            .collect::<HashSet<_>>()
            .into_iter()
            .filter(|k| !in_lost(k) && differs(&state, k))
            .count();
        if diff == 0 {
            return Some(state);
        }
        for (key, value) in &self.log[base.0..] {
            let tracked = !in_lost(key);
            let before = tracked && differs(&state, key);
            match value {
                Some(value) => state.insert(*key, *value),
                None => state.remove(key),
            };
            let after = tracked && differs(&state, key);
            diff = diff + usize::from(after) - usize::from(before);
            if diff == 0 {
                return Some(state);
            }
        }
        None
    }
}

fn setting(name: &str) -> Option<u64> {
    std::env::var(name).ok().map(|v| v.parse().expect(name))
}

fn run_seed(seed: u64, steps: usize) {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let reactor = TestReactor::new(256, ());
    let mut run = Run::new(seed, &sim, &reactor);
    for step in 0..steps {
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            run.step();
            if step % 16 == 0 {
                run.check_model();
            }
        }));
        if let Err(panic) = result {
            eprintln!("volume DST failed: seed {seed} step {step}");
            std::panic::resume_unwind(panic);
        }
    }
    // Settle, commit, crash, and check once more.
    while !run.writes.is_empty() || !run.commits.is_empty() {
        run.turn();
    }
    run.check_model();
    run.start_commit();
    loop {
        run.turn();
        if run.commits.is_empty() {
            break;
        }
    }
    let entries = run.model.len();
    run.crash();
    eprintln!(
        "seed {seed}: crashes {} corruptions {} published {} entries {entries} usage {:?}",
        run.crashes,
        run.corruptions,
        run.published.len(),
        run.volume.usage(),
    );
}

#[test]
fn volume_dst() {
    let steps = setting("DST_STEPS").unwrap_or(1000) as usize;
    if let Some(seed) = setting("DST_SEED") {
        run_seed(seed, steps);
        return;
    }
    let start = setting("DST_START_SEED").unwrap_or(0);
    let count = setting("DST_SEEDS").unwrap_or(32);
    for seed in start..start + count {
        run_seed(seed, steps);
    }
}
