// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Seeded allocator model, checkpoint and crash recovery campaigns.
use super::*;
use crate::buffers::{self, WorkerPool};
use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};

#[path = "recovery_parity.rs"]
mod persistence_parity;

#[path = "recovery_physical_parity.rs"]
mod physical_parity;

static NEXT: AtomicUsize = AtomicUsize::new(0);

struct Fixture(std::path::PathBuf);
impl Fixture {
    fn new() -> (Self, Allocator) {
        Self::with_size(32 * WIDE)
    }
    fn with_size(size: u64) -> (Self, Allocator) {
        let path = std::env::temp_dir().join(format!(
            "racer-allocator-sim-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let mut slab = Slab::create(&path, size, 1).unwrap();
        let a = Allocator::open_inner(slab.take_shard(ShardId::at(0)).unwrap(), config()).unwrap();
        (Self(path), a)
    }

    fn open(&self, image: &Image, g: Geometry) -> io::Result<(Allocator, Sim)> {
        // Another inode: crash probes cannot overwrite the running allocator.
        let path = self
            .0
            .with_extension(format!("crash-{}", NEXT.fetch_add(1, Ordering::Relaxed)));
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(&path)?;
        std::fs::remove_file(path)?;
        file.set_len(g.base + g.len)?;
        lock(&file)?;
        image.install(&file);
        let a = Allocator::open_inner(
            SlabShard {
                pressure: Arc::default(),
                file: Arc::new(SlabFile::Os(file)),
                geometry: g,
            },
            config(),
        )?;
        // Open can zero an invalid magic page and sync it. Carry those writes
        // into the next simulated lifetime, including previously absent sectors.
        let mut image = image.clone();
        for slot in 0..2 {
            let page = read_page(&a.space._file, g, slot)?;
            image.write(g.offset(slot), &page.0);
        }
        let sim = Sim::from_image(&a, image);
        Ok((a, sim))
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}
fn config() -> Config {
    Config {
        max_pending_values: 4096,
        ..Config::default()
    }
}

#[test]
fn checkpoint_budget_bounds_preparation_across_slabs_and_recovers_after_drop() {
    let budget = CheckpointBudget::default();
    let mut fixtures = Vec::new();
    let mut allocators = Vec::new();
    let mut sims = Vec::new();
    for n in 0..3 {
        let (fixture, mut a) = Fixture::new();
        *a.pressure.1.lock().unwrap() = budget.clone();
        assert!(
            a.insert_metadata(
                key(n),
                Metadata {
                    checksum: crate::metadata::Checksum(key(n)),
                    len: 1,
                    expires: 100,
                },
                0
            )
            .unwrap()
        );
        let mut sim = Sim::new(&a);
        a.progress(&mut sim, 32).unwrap(); // maintenance yield
        a.progress(&mut sim, 32).unwrap(); // freeze or wait for permit
        fixtures.push(fixture);
        allocators.push(a);
        sims.push(sim);
    }
    assert!(allocators[0].pipeline.is_some());
    assert!(allocators[1].pipeline.is_some());
    assert!(allocators[2].pipeline.is_none());
    assert!(!allocators[2].is_failed());
    assert!(allocators[2].changed);
    // Old submitted page jobs remain backend-owned. Dropping the preparer
    // releases the permit; it does not revoke retained backend resources.
    drop(allocators.remove(0));
    assert_eq!(budget.0.load(Ordering::Acquire), 1);
    allocators[1].progress(&mut sims[2], 32).unwrap();
    assert!(allocators[1].pipeline.is_some());
    assert_eq!(budget.0.load(Ordering::Acquire), 2);
    for (a, sim) in allocators.iter_mut().zip(sims.iter_mut().skip(1)) {
        for _ in 0..100 {
            a.progress(sim, 32).unwrap();
            for id in sim.pending() {
                sim.execute(id);
                sim.deliver(id);
            }
            if a.is_idle() {
                break;
            }
        }
        assert!(a.is_idle());
        assert_eq!(a.generation(), 3);
    }
    assert_eq!(budget.0.load(Ordering::Acquire), 0);
}
fn key(n: u64) -> Key {
    let mut key = [0; 32];
    key[..8].copy_from_slice(&n.to_be_bytes());
    key
}
fn buffer(pool: &WorkerPool, n: u64, bytes: &[u8]) -> Buffer {
    let mut fill = pool.stage(buffers::Key::new(key(n))).unwrap();
    fill.as_mut_slice()[..bytes.len()].copy_from_slice(bytes);
    fill.publish(bytes.len()).unwrap()
}

#[derive(Clone, Default)]
struct Image(BTreeMap<u64, Vec<u8>>);
impl Image {
    fn write(&mut self, offset: u64, bytes: &[u8]) {
        assert!(offset.is_multiple_of(512));
        for (i, chunk) in bytes.chunks(512).enumerate() {
            let sector = self
                .0
                .entry(offset + i as u64 * 512)
                .or_insert_with(|| vec![0; 512]);
            sector[..chunk.len()].copy_from_slice(chunk);
        }
    }
    fn read(&self, offset: u64, len: usize) -> Vec<u8> {
        assert!(offset.is_multiple_of(512));
        let mut bytes = vec![0; len];
        for (i, chunk) in bytes.chunks_mut(512).enumerate() {
            if let Some(sector) = self.0.get(&(offset + i as u64 * 512)) {
                chunk.copy_from_slice(&sector[..chunk.len()]);
            }
        }
        bytes
    }
    fn install(&self, file: &File) {
        for (&offset, sector) in &self.0 {
            file.write_all_at(sector, offset).unwrap();
        }
    }
}

#[derive(Clone, Copy, Debug)]
enum Persistence {
    None,
    Alternating,
    All,
}
#[derive(Clone, Copy, Debug)]
enum Fault {
    Submit(io::ErrorKind),
    Write(usize),
    Sync(Persistence),
    Observe,
}
struct Request {
    job: Option<Job>,
    executed: bool,
    available: bool,
    result: Option<io::Result<()>>,
    fault: Option<Fault>,
}
struct Sim {
    volatile: Image,
    durable: Image,
    requests: Vec<Request>,
    next_submission: Option<Fault>,
    // Like RingIo's retained Space: even page/sync requests retain the lock.
    space: Rc<Space>,
    pages: usize,
    values: usize,
    syncs: usize,
}
impl Sim {
    fn new(a: &Allocator) -> Self {
        let mut image = Image::default();
        for slot in 0..2 {
            image.write(
                a.space.geometry.offset(slot),
                &read_page(&a.space._file, a.space.geometry, slot).unwrap().0,
            );
        }
        Self::from_image(a, image)
    }
    fn from_image(a: &Allocator, image: Image) -> Self {
        Self {
            volatile: image.clone(),
            durable: image,
            requests: Vec::new(),
            next_submission: None,
            space: a.space.clone(),
            pages: 0,
            values: 0,
            syncs: 0,
        }
    }
    fn pending(&self) -> Vec<usize> {
        self.requests
            .iter()
            .enumerate()
            .filter(|(_, r)| r.job.is_some() && !r.executed)
            .map(|(i, _)| i)
            .collect()
    }
    fn deliveries(&self) -> Vec<usize> {
        self.requests
            .iter()
            .enumerate()
            .filter(|(_, r)| r.job.is_some() && r.executed && !r.available)
            .map(|(i, _)| i)
            .collect()
    }
    fn persist(&mut self, how: Persistence) {
        for (&offset, bytes) in &self.volatile.0 {
            if matches!(how, Persistence::All)
                || (matches!(how, Persistence::Alternating) && (offset / 512).is_multiple_of(2))
            {
                self.durable.0.insert(offset, bytes.clone());
            }
        }
    }
    fn crash_image(&self, mask: u64) -> Image {
        let mut image = self.durable.clone();
        for (&offset, bytes) in &self.volatile.0 {
            if mask & (1 << ((offset / 512) % 64)) != 0 {
                image.0.insert(offset, bytes.clone());
            }
        }
        image
    }
    fn execute(&mut self, id: usize) {
        let r = &mut self.requests[id];
        assert!(!r.executed && r.job.is_some(), "execute request {id}");
        r.executed = true;
        let fault = r.fault;
        match r.job.as_ref().unwrap() {
            Job::Page(page, offset) => {
                let len = if let Some(Fault::Write(n)) = fault {
                    n.min(PAGE_SIZE)
                } else {
                    PAGE_SIZE
                };
                self.volatile.write(*offset, &page.0[..len]);
                self.pages += 1;
            }
            Job::Value(v) => {
                let bytes = v.buffer.borrow();
                let bytes = bytes.as_ref().unwrap().as_slice();
                let len = if let Some(Fault::Write(n)) = fault {
                    n.min(bytes.len())
                } else {
                    bytes.len()
                };
                self.volatile.write(v.allocation.offset(), &bytes[..len]);
                self.values += 1;
            }
            Job::Sync => {
                let how = if let Some(Fault::Sync(how)) = fault {
                    how
                } else {
                    Persistence::All
                };
                self.persist(how);
                self.syncs += 1;
            }
        }
        self.requests[id].result =
            Some(if matches!(fault, Some(Fault::Write(_) | Fault::Sync(_))) {
                Err(io::Error::other("injected transfer/sync failure"))
            } else {
                Ok(())
            });
    }
    fn deliver(&mut self, id: usize) {
        let r = &mut self.requests[id];
        assert!(
            r.executed && !r.available && r.job.is_some(),
            "deliver request {id}"
        );
        r.available = true;
    }
    fn tick(&mut self) {
        for id in self.pending().into_iter().rev() {
            self.execute(id);
        }
        for id in self.deliveries().into_iter().rev() {
            self.deliver(id);
        }
    }
}
impl Storage for Sim {
    type Ticket = IoTicket;
    fn submit(&mut self, job: Job) -> Result<IoTicket, uring::Rejected<Job>> {
        if let Some(fault) = self.next_submission.take() {
            let Fault::Submit(kind) = fault else {
                panic!("not a submission fault")
            };
            return Err(uring::Rejected {
                error: kind.into(),
                resource: job,
            });
        }
        let id = self.requests.len();
        self.requests.push(Request {
            job: Some(job),
            executed: false,
            available: false,
            result: None,
            fault: None,
        });
        Ok(IoTicket::Sim(id))
    }
    fn complete(&mut self, ticket: &mut IoTicket) -> io::Result<Option<io::Result<()>>> {
        let IoTicket::Sim(id) = ticket else {
            unreachable!()
        };
        let r = &mut self.requests[*id];
        if matches!(r.fault, Some(Fault::Observe)) {
            return Err(io::Error::other("injected observation failure"));
        }
        if !r.available {
            return Ok(None);
        }
        let result = r.result.take().expect("completion collected twice");
        if let Some(Job::Value(value)) = r.job.take() {
            if result.is_ok() {
                value.written.set(true);
                value.buffer.borrow_mut().take();
            }
        }
        Ok(Some(result))
    }
}

fn stage(a: &Allocator) -> usize {
    match a.pipeline {
        Some(Pipeline::Writes(_)) => 0,
        Some(Pipeline::DataSync(_)) => 1,
        Some(Pipeline::DataSynced(_)) => 2,
        Some(Pipeline::MagicWritten(_)) => 3,
        None => 4,
    }
}
#[derive(Clone, Debug, Eq, PartialEq)]
struct Expected {
    info: ValueInfo,
    bytes: Vec<u8>,
}
type Snapshot = BTreeMap<Key, Expected>;
fn insert(
    a: &mut Allocator,
    pool: &WorkerPool,
    model: &mut Snapshot,
    k: u64,
    version: u64,
    len: usize,
    kind: Kind,
    expires: u64,
) {
    if kind == Kind::Metadata {
        let metadata = Metadata {
            checksum: crate::metadata::Checksum(*blake3::hash(&version.to_le_bytes()).as_bytes()),
            len: len as u64,
            expires: if expires == 0 { u64::MAX } else { expires },
        };
        assert!(a.insert_metadata(key(k), metadata, 0).unwrap());
        model.insert(key(k), expected_metadata(metadata));
        return;
    }
    let bytes: Vec<_> = (0..len)
        .map(|i| version.to_le_bytes()[i % 8].wrapping_add((i / 8) as u8))
        .collect();
    let info = ValueInfo {
        kind,
        len,
        expires,
        crc64: crc64(&bytes),
    };
    a.insert_payload(key(k), buffer(pool, version, &bytes), Some(info.crc64))
        .unwrap();
    model.insert(key(k), Expected { info, bytes });
}
fn expected_metadata(metadata: Metadata) -> Expected {
    Expected {
        info: ValueInfo {
            kind: Kind::Metadata,
            len: Metadata::SIZE,
            expires: metadata.expires,
            crc64: 0,
        },
        bytes: metadata.to_bytes().to_vec(),
    }
}
fn assert_snapshot(root: &Node, image: &Image, expected: &Snapshot) {
    let mut actual = Snapshot::new();
    root.visit(&mut |k, v| {
        let v = match v {
            Entry::Metadata(metadata) => {
                actual.insert(*k, expected_metadata(*metadata));
                return;
            }
            Entry::Payload(v) => v,
        };
        let bytes = v.buffer.borrow().as_ref().map_or_else(
            || image.read(v.allocation.offset(), v.info.len),
            |b| b.as_slice().to_vec(),
        );
        assert!(
            actual
                .insert(
                    *k,
                    Expected {
                        info: v.info,
                        bytes
                    }
                )
                .is_none()
        );
    });
    assert_eq!(&actual, expected);
}
fn assert_recovery(
    f: &Fixture,
    image: &Image,
    g: Geometry,
    snapshots: &BTreeMap<u64, Snapshot>,
    min: u64,
    max: u64,
) -> (Allocator, Sim) {
    let (a, sim) = f.open(image, g).unwrap();
    assert!(
        (min..=max).contains(&a.generation()),
        "generation {} outside {min}..={max}",
        a.generation()
    );
    // Check both retained roots, not just the selected one.
    for c in a.checkpoints.iter().flatten() {
        assert_snapshot(&c.root, &sim.durable, &snapshots[&c.generation]);
    }
    (a, sim)
}

#[test]
fn original_crc_survives_checkpoint_and_recovery() {
    let (fixture, mut allocator) = Fixture::new();
    let pool = buffers::io_test_pool(4);
    let mut sim = Sim::new(&allocator);
    let mut model = Model::new();
    for (n, kind, expires) in [(1, Kind::Payload, 0), (2, Kind::Payload, 0)] {
        let mut fill = pool.stage(buffers::Key::new(key(n))).unwrap();
        fill.as_mut_slice()[..4096].fill(n as u8);
        let crc = crc64(&fill.as_mut_slice()[..4096]);
        let buffer = fill.publish_checked(4096, crc).unwrap();
        model.live.insert(
            key(n),
            Expected {
                info: ValueInfo {
                    kind,
                    len: 4096,
                    crc64: crc,
                    expires,
                },
                bytes: buffer.as_slice().to_vec(),
            },
        );
        allocator
            .insert_payload(key(n), buffer, Some(crc ^ 1))
            .unwrap();
        model.flush(&mut allocator, &mut sim);
        let (mut recovered, _) = model.probe(&fixture, &allocator, &sim.durable);
        let lease = recovered.lookup(&key(n), 0).unwrap();
        assert_eq!(lease.info().crc64, crc);
    }
}

#[test]
fn inline_metadata_bound_cow_recovery_and_independent_payload_lease() {
    fn drain(a: &mut Allocator, sim: &mut Sim) {
        for _ in 0..10000 {
            a.progress(sim, 32).unwrap();
            sim.tick();
            if a.is_idle() {
                return;
            }
        }
        panic!("inline checkpoint stalled");
    }
    let (fixture, mut a) = Fixture::with_size(8 * WIDE);
    let mut sim = Sim::new(&a);
    let pool = buffers::io_test_pool(1);
    a.insert_payload(key(u64::MAX), buffer(&pool, 1, b"immutable"), None)
        .unwrap();
    drain(&mut a, &mut sim);
    let lease = a.lookup(&key(u64::MAX), 0).unwrap();
    let file = lease.ready().unwrap();
    let payload_free = a.space.maps[Class::Payload.index()].borrow().free;
    let capacity = a.metadata_capacity();
    assert_eq!(capacity, 162);
    let record = Metadata {
        checksum: crate::metadata::Checksum([7; 32]),
        len: 0,
        expires: 100,
    };
    for expires in [0, 9, 10] {
        assert!(
            !a.insert_metadata(key(0), Metadata { expires, ..record }, 10)
                .unwrap()
        );
    }
    assert_eq!(a.len(), 1);
    for n in 0..capacity {
        assert!(a.insert_metadata(key(n as u64), record, 10).unwrap());
    }
    assert_eq!(a.metadata_count, capacity);
    assert_eq!(a.pending.len(), 0);
    drain(&mut a, &mut sim);
    let copied = a.lookup_metadata(&key(0), 99).unwrap();
    assert_eq!(copied, record);
    // Refill beyond the hard limit while each checkpoint is frozen, retaining
    // both prior roots and admitting a distinct live tree during I/O.
    for batch in 1..=4 {
        a.rotate = true;
        a.progress(&mut sim, 1).unwrap();
        // The drained boundary yields once for owner-side maintenance before
        // freezing. No storage effect or checkpoint is skipped by this turn.
        if a.pipeline.is_none() {
            a.progress(&mut sim, 1).unwrap();
        }
        assert!(a.pipeline.is_some());
        for n in 0..capacity * 2 {
            let metadata = Metadata {
                len: n as u64,
                expires: 100 + batch,
                ..record
            };
            assert!(
                a.insert_metadata(
                    key((batch as usize * capacity * 2 + n) as u64),
                    metadata,
                    99
                )
                .unwrap()
            );
            assert_eq!(a.metadata_count, capacity);
            assert_eq!(a.len(), capacity + 1);
        }
        drain(&mut a, &mut sim);
        let (mut recovered, _) = fixture.open(&sim.durable, a.space.geometry).unwrap();
        assert_eq!(recovered.metadata_count, capacity);
        a.root.visit(&mut |k, entry| {
            if let Entry::Metadata(metadata) = entry {
                assert_eq!(recovered.lookup_metadata(k, 99), Some(*metadata));
            }
        });
        a.root.visit(&mut |k, entry| {
            if let Entry::Metadata(_) = entry {
                assert_eq!(recovered.lookup_metadata(k, 104), None);
            }
        });
        assert!(recovered.lookup(&key(u64::MAX), u64::MAX).is_some());
    }
    assert_eq!(sim.values, 1, "metadata must never submit value I/O");
    assert_eq!(
        a.space.maps[Class::Payload.index()].borrow().free,
        payload_free
    );
    assert_eq!(sim.durable.read(file.offset(), 9), b"immutable");
    assert_eq!(
        copied, record,
        "a copied record survives replacement and eviction"
    );
    assert!(lease.buffer().is_none());
    // Shrink several tree levels, recovering each merge/redistribution. Only
    // inline entries change; the payload remains independently leased.
    let mut keys = Vec::new();
    a.root.visit(&mut |k, entry| {
        if matches!(entry, Entry::Metadata(_)) {
            keys.push(*k);
        }
    });
    for chunk in keys.chunks(13) {
        for k in chunk {
            assert!(a.remove(k));
        }
        drain(&mut a, &mut sim);
        let (recovered, _) = fixture.open(&sim.durable, a.space.geometry).unwrap();
        assert_eq!(recovered.generation(), a.generation());
        assert_eq!(recovered.metadata_count, a.metadata_count);
    }
    assert_eq!(a.len(), 1);
}

// This model contains only logical versions supplied by the workload. Generation
// numbers come from the state machine, but expected contents never come from it.
struct Model {
    live: Snapshot,
    snapshots: BTreeMap<u64, Snapshot>,
    durable_generation: u64,
}
impl Model {
    fn new() -> Self {
        Self {
            live: Snapshot::new(),
            snapshots: [(1, Snapshot::new()), (2, Snapshot::new())].into(),
            durable_generation: 2,
        }
    }
    fn progress(&mut self, a: &mut Allocator, sim: &mut Sim, budget: usize) -> io::Result<bool> {
        if a.pipeline.is_none() && (a.changed || a.rotate) {
            self.snapshots.insert(a.generation() + 1, self.live.clone());
        }
        a.progress(sim, budget)
    }
    fn execute(&mut self, a: &Allocator, sim: &mut Sim, id: usize) {
        let final_sync = matches!(sim.requests[id].job, Some(Job::Sync)) && stage(a) == 3;
        sim.execute(id);
        if final_sync && sim.requests[id].result.as_ref().unwrap().is_ok() {
            self.durable_generation = a.generation() + 1;
        }
    }
    fn flush(&mut self, a: &mut Allocator, sim: &mut Sim) {
        for _ in 0..10000 {
            self.progress(a, sim, 32).unwrap();
            for id in sim.pending().into_iter().rev() {
                self.execute(a, sim, id);
            }
            for id in sim.deliveries().into_iter().rev() {
                sim.deliver(id);
            }
            if a.is_idle() {
                self.check_live(a, sim);
                return;
            }
        }
        panic!("model flush did not converge");
    }
    fn check_live(&self, a: &Allocator, sim: &Sim) {
        assert_snapshot(&a.root, &sim.volatile, &self.live);
        assert_eq!(a.len(), self.live.len());
        assert_eq!(a.positions.len(), self.live.len());
        assert_eq!(
            a.metadata_count,
            self.live
                .values()
                .filter(|v| v.info.kind == Kind::Metadata)
                .count()
        );
        assert_eq!(
            a.live[Class::Payload.index()],
            self.live
                .values()
                .filter(|v| v.info.kind == Kind::Payload)
                .count()
        );
        for (i, h) in a.heat.iter().enumerate() {
            assert_eq!(a.positions[&h.key], i);
            assert!(self.live.contains_key(&h.key));
        }
        for map in &a.space.maps {
            let b = map.borrow();
            assert_eq!(
                b.free,
                b.words
                    .iter()
                    .map(|w| w.count_ones() as usize)
                    .sum::<usize>()
            );
            for (i, word) in b.words.iter().enumerate() {
                assert_eq!(b.summary[i / 64] & (1 << (i % 64)) != 0, *word != 0);
            }
        }
    }
    fn probe(&self, f: &Fixture, a: &Allocator, image: &Image) -> (Allocator, Sim) {
        assert_recovery(
            f,
            image,
            a.space.geometry,
            &self.snapshots,
            self.durable_generation,
            *self.snapshots.last_key_value().unwrap().0,
        )
    }
}

fn reach_stage(a: &mut Allocator, sim: &mut Sim, model: &mut Model, target: usize) {
    // Stop before that stage submits anything. prepare() alone has no I/O.
    if target == 0 && a.pipeline.is_none() {
        model
            .snapshots
            .insert(a.generation() + 1, model.live.clone());
        a.pipeline = Some(a.prepare().unwrap());
    }
    for _ in 0..100 {
        if stage(a) == target {
            assert!(sim.pending().is_empty());
            return;
        }
        model.progress(a, sim, 32).unwrap();
        for id in sim.pending() {
            model.execute(a, sim, id);
            sim.deliver(id);
        }
    }
    panic!("did not reach stage {target}");
}

#[test]
fn coalesces_overwrites_and_rewrites_only_dirty_paths() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(128);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    for n in 0..100 {
        insert(&mut a, &pool, &mut model.live, n, n, 64, Kind::Metadata, 0);
    }
    model.flush(&mut a, &mut sim);
    assert_eq!(sim.values, 0, "inline metadata has no value writes");
    let pages = sim.pages;
    for n in 100..120 {
        insert(&mut a, &pool, &mut model.live, 1, n, 64, Kind::Metadata, 0);
    }
    assert_eq!(
        a.lookup_metadata(&key(1), 0).unwrap().to_bytes().as_slice(),
        model.live[&key(1)].bytes
    );
    model.flush(&mut a, &mut sim);
    assert_eq!(sim.values, 0, "inline replacements only rewrite tree paths");
    assert!(
        sim.pages - pages <= 4,
        "leaf, ancestors, bitmap, magic only"
    );
    let pages = sim.pages;
    for _ in 0..1000 {
        assert!(a.lookup_metadata(&key(1), 0).is_some());
    }
    model.flush(&mut a, &mut sim);
    assert_eq!(sim.pages, pages, "hits must not write metadata");
    model.probe(&f, &a, &sim.durable);
}

#[test]
fn submission_backpressure_retries_each_stage_without_poisoning() {
    for target in 0..4 {
        let (f, mut a) = Fixture::new();
        let pool = buffers::io_test_pool(2);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        insert(&mut a, &pool, &mut model.live, 1, 1, 64, Kind::Metadata, 0);
        reach_stage(&mut a, &mut sim, &mut model, target);
        let count = sim.requests.len();
        for _ in 0..3 {
            sim.next_submission = Some(Fault::Submit(io::ErrorKind::WouldBlock));
            model.progress(&mut a, &mut sim, 1).unwrap();
            assert!(!a.failed);
            assert_eq!(stage(&a), target);
            assert_eq!(sim.requests.len(), count);
            model.probe(&f, &a, &sim.durable);
        }
        model.flush(&mut a, &mut sim);
        assert_eq!(a.generation(), 3);
        assert_eq!(sim.values, 0);
        model.probe(&f, &a, &sim.durable);
    }
}

#[test]
fn permanent_fault_matrix_poisoning_and_ambiguous_persistence() {
    for target in 0..4 {
        let mut faults = vec![Fault::Submit(io::ErrorKind::Other), Fault::Observe];
        if target == 0 || target == 2 {
            faults.extend([Fault::Write(0), Fault::Write(512), Fault::Write(usize::MAX)]);
        } else {
            faults.extend([
                Fault::Sync(Persistence::None),
                Fault::Sync(Persistence::Alternating),
                Fault::Sync(Persistence::All),
            ]);
        }
        for fault in faults {
            // Writes includes value, node and bitmap jobs; target every one.
            for position in 0..if target == 0 { 3 } else { 1 } {
                let (f, mut a) = Fixture::new();
                let pool = buffers::io_test_pool(4);
                let mut sim = Sim::new(&a);
                let mut model = Model::new();
                insert(
                    &mut a,
                    &pool,
                    &mut model.live,
                    1,
                    1,
                    PAGE_SIZE,
                    Kind::Metadata,
                    0,
                );
                model.flush(&mut a, &mut sim);
                insert(
                    &mut a,
                    &pool,
                    &mut model.live,
                    1,
                    2,
                    PAGE_SIZE,
                    Kind::Metadata,
                    0,
                );
                insert(&mut a, &pool, &mut model.live, 2, 3, 512, Kind::Payload, 0);
                reach_stage(&mut a, &mut sim, &mut model, target);
                if matches!(fault, Fault::Submit(_)) {
                    for _ in 0..position {
                        model.progress(&mut a, &mut sim, 1).unwrap();
                    }
                    sim.next_submission = Some(fault);
                    assert!(model.progress(&mut a, &mut sim, 1).is_err());
                } else {
                    model.progress(&mut a, &mut sim, 32).unwrap();
                    let ids = sim.pending();
                    // Payload value, inline leaf and bitmap all fail.
                    let id = ids[position];
                    sim.requests[id].fault = Some(fault);
                    for id in ids {
                        sim.execute(id);
                        sim.deliver(id);
                    }
                    assert!(
                        model.progress(&mut a, &mut sim, 32).is_err(),
                        "stage {target}, {fault:?}"
                    );
                }
                assert!(a.failed, "stage {target}, {fault:?}");
                assert_eq!(
                    a.generation(),
                    3,
                    "failed publication must not be acknowledged"
                );
                assert!(
                    a.insert_metadata(
                        key(9),
                        Metadata {
                            checksum: crate::metadata::Checksum([9; 32]),
                            len: 1,
                            expires: 100
                        },
                        0
                    )
                    .is_err()
                );
                assert!(a.progress(&mut sim, 32).is_err());
                for mask in [0, u64::MAX, 0x5555_5555_5555_5555] {
                    model.probe(&f, &a, &sim.crash_image(mask));
                }
                // Errors abandon tickets, not backend requests. They may still
                // execute, but the poisoned allocator must never resume reuse.
                sim.tick();
                model.probe(&f, &a, &sim.crash_image(u64::MAX));
            }
        }
    }
}

#[test]
fn publication_boundaries_and_all_torn_magic_sector_masks() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(4);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    insert(
        &mut a,
        &pool,
        &mut model.live,
        1,
        1,
        PAGE_SIZE,
        Kind::Metadata,
        0,
    );
    model.flush(&mut a, &mut sim);
    insert(
        &mut a,
        &pool,
        &mut model.live,
        1,
        2,
        PAGE_SIZE,
        Kind::Metadata,
        0,
    );
    insert(&mut a, &pool, &mut model.live, 2, 3, 513, Kind::Payload, 0);
    let mut masks_tested = 0;
    let mut final_sync_tested = false;
    for _ in 0..100 {
        model.progress(&mut a, &mut sim, 1).unwrap();
        for id in sim.pending().into_iter().rev() {
            for mask in [0, u64::MAX, 0xaaaa_aaaa_aaaa_aaaa] {
                model.probe(&f, &a, &sim.crash_image(mask));
            }
            let magic_offset = if stage(&a) == 2 {
                let Some(Job::Page(_, offset)) = sim.requests[id].job.as_ref() else {
                    panic!()
                };
                Some(*offset)
            } else {
                None
            };
            let before = sim.durable.clone();
            model.execute(&a, &mut sim, id);
            if let Some(offset) = magic_offset {
                for mask in 0..256u64 {
                    let mut crash = before.clone();
                    for sector in 0..8 {
                        if mask & (1 << sector) != 0 {
                            let at = offset + sector * 512;
                            crash.0.insert(at, sim.volatile.0[&at].clone());
                        }
                    }
                    model.probe(&f, &a, &crash);
                    masks_tested += 1;
                }
            }
            for mask in [0, u64::MAX, 0x5555_5555_5555_5555] {
                model.probe(&f, &a, &sim.crash_image(mask));
            }
            if stage(&a) == 3 {
                assert_eq!(a.generation(), 3);
                assert_eq!(model.probe(&f, &a, &sim.durable).0.generation(), 4);
                final_sync_tested = true;
            }
            // Executed I/O is still invisible to the allocator until delivery.
            let current = stage(&a);
            model.progress(&mut a, &mut sim, 1).unwrap();
            assert_eq!(stage(&a), current);
            sim.deliver(id);
            model.probe(&f, &a, &sim.durable);
        }
        if a.is_idle() {
            break;
        }
    }
    assert!(a.is_idle());
    assert_eq!(masks_tested, 256);
    assert!(final_sync_tested);
    assert_eq!(a.generation(), 4);
}

#[test]
fn generated_pinned_version_lifecycle() {
    for seed in 0..12 {
        let mut random = Random(seed);
        let (f, mut a) = Fixture::new();
        let pool = buffers::io_test_pool(8);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        let kind = Kind::Payload;
        let len = if seed == 0 {
            BUFFER_SIZE
        } else {
            1 + random.index(512)
        };
        insert(&mut a, &pool, &mut model.live, 1, 1, len, kind, 0);
        insert(&mut a, &pool, &mut model.live, 2, 2, 64, kind, 0);
        model.progress(&mut a, &mut sim, 32).unwrap();
        let lease = a.lookup(&key(1), 0).unwrap();
        let old = model.live[&key(1)].clone();
        let allocation = (lease.value.allocation.class, lease.value.allocation.index);
        insert(&mut a, &pool, &mut model.live, 1, 3, 64, kind, 0);
        assert!(a.remove(&key(2)));
        model.live.remove(&key(2));
        let mut pending = sim.pending();
        while !pending.is_empty() {
            let id = pending.remove(random.index(pending.len()));
            model.execute(&a, &mut sim, id);
        }
        assert!(!lease.value.written.get());
        assert_eq!(lease.buffer().unwrap().as_slice(), old.bytes);
        model.check_live(&a, &sim);
        let mut deliveries = sim.deliveries();
        while !deliveries.is_empty() {
            let id = deliveries.remove(random.index(deliveries.len()));
            sim.deliver(id);
        }
        model.progress(&mut a, &mut sim, 32).unwrap();
        assert!(lease.value.written.get());
        assert!(lease.buffer().is_none());
        assert_eq!(
            sim.volatile
                .read(lease.value.allocation.offset(), old.info.len),
            old.bytes
        );
        model.flush(&mut a, &mut sim);
        assert_eq!(a.evict(kind, 0), Some(key(1)));
        model.live.clear();
        model.flush(&mut a, &mut sim);
        a.rotate = true;
        model.flush(&mut a, &mut sim);
        for n in 3..12 {
            insert(
                &mut a,
                &pool,
                &mut model.live,
                n,
                n,
                1 + random.index(512),
                kind,
                0,
            );
            model.flush(&mut a, &mut sim);
            assert!(occupied(&a.space, allocation.0, allocation.1));
            assert_eq!(
                sim.durable
                    .read(lease.value.allocation.offset(), old.info.len),
                old.bytes
            );
        }
        model.probe(&f, &a, &sim.durable);
        drop(lease);
        assert!(!occupied(&a.space, allocation.0, allocation.1));
    }
}

fn occupied(space: &Space, class: Class, index: usize) -> bool {
    space.maps[class.index()].borrow().words[index / 64] & (1 << (index % 64)) == 0
}

#[test]
fn abandoned_allocator_leaves_backend_value_requests_pinned() {
    let (_f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(2);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    insert(&mut a, &pool, &mut model.live, 1, 1, 512, Kind::Payload, 0);
    let spare = pool.private_fill().unwrap();
    assert!(
        pool.private_fill().is_err(),
        "unpublished value retains its slot"
    );
    let lease = a.lookup(&key(1), 0).unwrap();
    let index = lease.value.allocation.index;
    let value = Rc::downgrade(&lease.value);
    drop(lease);
    model.progress(&mut a, &mut sim, 32).unwrap();
    let id = sim
        .pending()
        .into_iter()
        .find(|&id| matches!(sim.requests[id].job, Some(Job::Value(_))))
        .unwrap();
    drop(a);
    assert!(
        pool.private_fill().is_err(),
        "backend owns abandoned write storage"
    );
    assert!(occupied(&sim.space, Class::Payload, index));
    assert!(value.upgrade().is_some());
    sim.execute(id);
    assert!(
        pool.private_fill().is_err(),
        "execution is not completion collection"
    );
    assert!(occupied(&sim.space, Class::Payload, index));
    sim.deliver(id);
    assert!(
        pool.private_fill().is_err(),
        "delivered completion still owns storage"
    );
    // Model terminal collection of an abandoned request by the backend.
    sim.complete(&mut IoTicket::Sim(id))
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(value.upgrade().is_none());
    assert!(!occupied(&sim.space, Class::Payload, index));
    drop(spare);
    pool.assert_recovered();
}

#[test]
fn predecessor_extent_is_reclaimed_only_after_final_sync_collection() {
    let (_f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(2);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    insert(&mut a, &pool, &mut model.live, 1, 1, 512, Kind::Payload, 0);
    model.flush(&mut a, &mut sim);
    // Record numbers only: an extra strong reference would hide early release.
    let index = a
        .root
        .get(&key(1))
        .unwrap()
        .payload()
        .unwrap()
        .allocation
        .index;
    assert!(a.remove(&key(1)));
    model.live.clear();
    model.flush(&mut a, &mut sim);
    assert!(occupied(&a.space, Class::Payload, index));
    a.rotate = true;
    reach_stage(&mut a, &mut sim, &mut model, 3);
    model.progress(&mut a, &mut sim, 1).unwrap();
    let id = sim.pending()[0];
    assert!(occupied(&a.space, Class::Payload, index));
    model.execute(&a, &mut sim, id);
    assert!(occupied(&a.space, Class::Payload, index));
    sim.deliver(id);
    assert!(occupied(&a.space, Class::Payload, index));
    model.progress(&mut a, &mut sim, 1).unwrap();
    assert!(!occupied(&a.space, Class::Payload, index));
}

fn corrupt_latest(a: &Allocator, image: &Image, what: usize) -> Image {
    let (slot, c) = a
        .checkpoints
        .iter()
        .enumerate()
        .filter_map(|(i, c)| c.as_ref().map(|c| (i, c)))
        .max_by_key(|(_, c)| c.generation)
        .unwrap();
    let offset = match what {
        0 => a.space.geometry.offset(slot),
        1 => c.root.disk.as_ref().unwrap().offset(),
        2 => c.bitmaps[0].0.offset(),
        _ => unreachable!(),
    };
    let mut image = image.clone();
    image.0.get_mut(&offset).unwrap()[100] ^= 1;
    image
}

#[test]
fn corrupt_magic_tree_bitmap_fallback_and_both_slots_invalid() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(2);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    insert(&mut a, &pool, &mut model.live, 1, 1, 64, Kind::Metadata, 0);
    model.flush(&mut a, &mut sim);
    insert(&mut a, &pool, &mut model.live, 1, 2, 64, Kind::Metadata, 0);
    model.flush(&mut a, &mut sim);
    for what in 0..3 {
        let image = corrupt_latest(&a, &sim.durable, what);
        let (mut recovered, mut backend) =
            assert_recovery(&f, &image, a.space.geometry, &model.snapshots, 3, 3);
        let slot = a
            .checkpoints
            .iter()
            .position(|c| c.as_ref().unwrap().generation == 4)
            .unwrap();
        assert_eq!(
            backend
                .durable
                .read(a.space.geometry.offset(slot), PAGE_SIZE),
            vec![0; PAGE_SIZE]
        );
        // Restart again before writing anything: invalid-slot zeroing belongs to
        // the carried-forward image rather than the original corrupt image.
        (recovered, backend) = f.open(&backend.durable, recovered.space.geometry).unwrap();
        assert_eq!(recovered.generation(), 3);
        let mut resumed = Model {
            live: model.snapshots[&3].clone(),
            snapshots: model.snapshots.clone(),
            durable_generation: 3,
        };
        insert(
            &mut recovered,
            &pool,
            &mut resumed.live,
            2,
            10 + what as u64,
            513,
            Kind::Payload,
            0,
        );
        resumed.flush(&mut recovered, &mut backend);
        resumed.probe(&f, &recovered, &backend.durable);
    }
    let mut image = sim.durable.clone();
    for slot in 0..2 {
        image.0.get_mut(&a.space.geometry.offset(slot)).unwrap()[0] ^= 1;
    }
    assert!(
        matches!(f.open(&image, a.space.geometry), Err(e) if e.kind() == io::ErrorKind::InvalidData)
    );
}

#[test]
fn full_cache_churn_restarts_and_preserves_exact_fallback() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(4);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    let count = a.space.geometry.range(Class::Payload).1;
    let mut rejections = 0;
    let mut restarts = 0;
    for n in 0..count * 4 {
        let bytes = (n as u64).to_le_bytes().repeat(64);
        let info = ValueInfo {
            kind: Kind::Payload,
            len: bytes.len(),
            crc64: crc64(&bytes),
            expires: 0,
        };
        let mut resource = buffer(&pool, n as u64, &bytes);
        let mut admitted = false;
        for _ in 0..8 {
            match a.insert_payload(key(n as u64), resource, Some(info.crc64)) {
                Ok(()) => {
                    model.live.insert(
                        key(n as u64),
                        Expected {
                            info,
                            bytes: bytes.clone(),
                        },
                    );
                    admitted = true;
                    break;
                }
                Err(e) => {
                    assert_eq!(e.error.kind(), io::ErrorKind::WouldBlock);
                    resource = e.resource;
                    assert_eq!(resource.as_slice(), bytes);
                    rejections += 1;
                    // Approximate LFU's choice is deliberately not duplicated.
                    // A pressure rejection removes a bounded batch of payloads;
                    // validate all remaining versions rather than resnapshotting.
                    let removed: Vec<_> = model
                        .live
                        .keys()
                        .filter(|k| a.root.get(k).is_none())
                        .copied()
                        .collect();
                    assert!(removed.len() <= a.reclaim_target(Class::Payload));
                    for k in removed {
                        assert_eq!(model.live.remove(&k).unwrap().info.kind, Kind::Payload);
                    }
                    model.check_live(&a, &sim);
                    model.flush(&mut a, &mut sim);
                }
            }
        }
        assert!(admitted);
        model.flush(&mut a, &mut sim);
        model.probe(&f, &a, &sim.durable);
        if n >= count {
            let fallback = a
                .checkpoints
                .iter()
                .flatten()
                .map(|c| c.generation)
                .min()
                .unwrap();
            let crash = corrupt_latest(&a, &sim.durable, 0);
            assert_recovery(
                &f,
                &crash,
                a.space.geometry,
                &model.snapshots,
                fallback,
                fallback,
            );
        }
        if n % 7 == 6 {
            let g = a.space.geometry;
            let image = sim.durable.clone();
            drop(a);
            drop(sim);
            (a, sim) = assert_recovery(
                &f,
                &image,
                g,
                &model.snapshots,
                model.durable_generation,
                model.durable_generation,
            );
            model.live = model.snapshots[&a.generation()].clone();
            restarts += 1;
        }
    }
    assert!(rejections > 0 && restarts > 1);
}

fn pressure(a: &mut Allocator, payload: &Buffer, model: &mut Model, kind: Kind) -> usize {
    let error = a
        .insert_payload(key(u64::MAX), payload.clone(), Some(0))
        .unwrap_err();
    assert_eq!(error.error.kind(), io::ErrorKind::WouldBlock);
    assert_eq!(error.resource.as_slice(), payload.as_slice());
    let removed: Vec<_> = model
        .live
        .keys()
        .filter(|k| a.root.get(k).is_none())
        .copied()
        .collect();
    for k in &removed {
        assert_eq!(model.live.remove(k).unwrap().info.kind, kind);
    }
    removed.len()
}

#[test]
fn pressure_batches_amortize_checkpoints_with_tiny_shards_and_budgets() {
    for (size, pending, io_budget, kind, expected_batch) in [
        (32 * WIDE, 64, 32, Kind::Payload, 7),
        (8 * WIDE, 64, 1, Kind::Payload, 1),
        (32 * WIDE, 2, 1, Kind::Payload, 2),
    ] {
        let (f, mut a) = Fixture::with_size(size);
        a.config.max_pending_values = pending;
        a.config.max_io = io_budget;
        let pool = buffers::io_test_pool(66);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        let class = Class::Payload;
        let capacity = a.space.geometry.range(class).1;
        let payload = buffer(&pool, u64::MAX, &[1; 64]);
        for n in 0..capacity {
            insert(
                &mut a,
                &pool,
                &mut model.live,
                n as u64,
                n as u64,
                64,
                kind,
                0,
            );
            model.flush(&mut a, &mut sim);
        }
        let generation = a.generation();
        let syncs = sim.syncs;
        for batch in 0..4 {
            assert_eq!(pressure(&mut a, &payload, &mut model, kind), expected_batch);
            for _ in 0..100 {
                assert_eq!(pressure(&mut a, &payload, &mut model, kind), 0);
            }
            // A single rejected admission must schedule both rotations. No
            // retries are needed to release the entire batch after final sync.
            // Alternate with retries throughout every in-flight stage: these
            // must neither evict more entries nor add redundant checkpoints.
            if batch % 2 == 1 {
                for _ in 0..10000 {
                    if a.is_idle() {
                        break;
                    }
                    assert_eq!(pressure(&mut a, &payload, &mut model, kind), 0);
                    model.progress(&mut a, &mut sim, 1).unwrap();
                    for id in sim.pending().into_iter().rev() {
                        model.execute(&a, &mut sim, id);
                        sim.deliver(id);
                    }
                }
                assert!(a.is_idle());
            }
            model.flush(&mut a, &mut sim);
            assert_eq!(a.space.maps[class.index()].borrow().free, expected_batch);
            for i in 0..expected_batch {
                let n = (capacity + batch * expected_batch + i) as u64;
                insert(&mut a, &pool, &mut model.live, n, n, 64, kind, 0);
            }
            model.flush(&mut a, &mut sim);
            assert_eq!(a.len(), capacity);
            model.probe(&f, &a, &sim.durable);
            let fallback = a.generation() - 1;
            assert_recovery(
                &f,
                &corrupt_latest(&a, &sim.durable, 0),
                a.space.geometry,
                &model.snapshots,
                fallback,
                fallback,
            );
        }
        // Two removal checkpoints plus one admission checkpoint per batch,
        // with both sync barriers on EVERY checkpoint.
        assert_eq!(a.generation() - generation, 12);
        assert_eq!(sim.syncs - syncs, 24);
        if expected_batch == 7 {
            assert!((sim.syncs - syncs) < 4 * expected_batch);
        }
    }
}

#[test]
fn pressure_waits_for_checkpointed_values_and_retained_leases() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(32);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    let count = a.space.geometry.range(Class::Payload).1;
    let payload = buffer(&pool, u64::MAX, &[1; 64]);
    for n in 0..count {
        insert(
            &mut a,
            &pool,
            &mut model.live,
            n as u64,
            n as u64,
            64,
            Kind::Payload,
            0,
        );
    }
    // Neither pending values nor written values in an unpublished snapshot are
    // automatic victims, even with unlimited admission retries.
    for target in 0..4 {
        reach_stage(&mut a, &mut sim, &mut model, target);
        for _ in 0..20 {
            assert_eq!(pressure(&mut a, &payload, &mut model, Kind::Payload), 0);
        }
        assert_eq!(a.len(), count);
    }
    model.flush(&mut a, &mut sim);
    let mut leases: Vec<_> = (0..count)
        .map(|n| a.lookup(&key(n as u64), 0).unwrap())
        .collect();
    let generation = a.generation();
    assert_eq!(pressure(&mut a, &payload, &mut model, Kind::Payload), 7);
    model.flush(&mut a, &mut sim);
    assert_eq!(a.generation(), generation + 2);
    assert_eq!(a.space.maps[Class::Payload.index()].borrow().free, 0);
    for _ in 0..100 {
        assert_eq!(pressure(&mut a, &payload, &mut model, Kind::Payload), 0);
        model.flush(&mut a, &mut sim);
    }
    assert_eq!(
        a.generation(),
        generation + 2,
        "leases must not cause endless checkpoints"
    );
    for (n, lease) in leases.iter().enumerate() {
        assert!(occupied(
            &a.space,
            Class::Payload,
            lease.value.allocation.index
        ));
        let expected = &model.snapshots[&generation][&key(n as u64)];
        assert_eq!(
            sim.durable
                .read(lease.value.allocation.offset(), lease.info().len),
            expected.bytes
        );
    }
    let victim = (0..count)
        .find(|&n| !model.live.contains_key(&key(n as u64)))
        .unwrap();
    drop(leases.remove(victim));
    assert_eq!(a.space.maps[Class::Payload.index()].borrow().free, 1);
    insert(
        &mut a,
        &pool,
        &mut model.live,
        100,
        100,
        64,
        Kind::Payload,
        0,
    );
    model.flush(&mut a, &mut sim);
    // Other victim leases remain held. Reclaim only the one newly consumed
    // extent instead of draining another whole batch.
    assert_eq!(pressure(&mut a, &payload, &mut model, Kind::Payload), 1);
    model.flush(&mut a, &mut sim);
    drop(leases);
    assert_eq!(a.space.maps[Class::Payload.index()].borrow().free, 7);
    model.probe(&f, &a, &sim.durable);
}

#[test]
fn pressure_counts_replacements_and_active_snapshots_as_retired_space() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(10);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    let payload = buffer(&pool, u64::MAX, &[1; 64]);
    let count = a.space.geometry.range(Class::Payload).1;
    for n in 0..count - 7 {
        insert(
            &mut a,
            &pool,
            &mut model.live,
            n as u64,
            n as u64,
            64,
            Kind::Payload,
            0,
        );
        model.flush(&mut a, &mut sim);
    }
    for n in 0..7 {
        insert(
            &mut a,
            &pool,
            &mut model.live,
            n,
            count as u64 + n,
            64,
            Kind::Payload,
            0,
        );
    }
    reach_stage(&mut a, &mut sim, &mut model, 0);
    // Seven superseded checkpoint versions already meet the reclamation target.
    assert_eq!(pressure(&mut a, &payload, &mut model, Kind::Payload), 0);
    // Removing a snapshot version needs two publications AFTER that snapshot.
    assert!(a.remove(&key(0)));
    model.live.remove(&key(0));
    assert_eq!(pressure(&mut a, &payload, &mut model, Kind::Payload), 0);
    let generation = a.generation();
    model.flush(&mut a, &mut sim);
    assert_eq!(a.generation(), generation + 3);
    assert_eq!(a.space.maps[Class::Payload.index()].borrow().free, 8);
    model.probe(&f, &a, &sim.durable);
}

use crate::simulation::corpus::Random;

#[test]
fn randomized_checkpoint_splits_merges_and_bitmap_validation() {
    let (f, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(64);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    let mut random = Random(3);
    for batch in 0..80 {
        for i in 0..48 {
            let r = random.next();
            let k = r % 400;
            if r.is_multiple_of(4) {
                assert_eq!(a.remove(&key(k)), model.live.remove(&key(k)).is_some());
            } else {
                insert(
                    &mut a,
                    &pool,
                    &mut model.live,
                    k,
                    batch * 48 + i,
                    16,
                    Kind::Metadata,
                    0,
                );
            }
        }
        model.flush(&mut a, &mut sim);
        model.probe(&f, &a, &sim.durable);
    }
    for k in model.live.keys() {
        assert!(a.remove(k));
    }
    model.live.clear();
    model.flush(&mut a, &mut sim);
    assert_eq!(a.root.len(), 0);
    model.probe(&f, &a, &sim.durable);
}

// The seed reproduces all workload and scheduler choices. A trace is emitted
// only on failure, including the state BEFORE the failing action.
fn campaign(seed: u64, steps: usize) {
    let mut trace = Vec::new();
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let (f, mut a) = Fixture::new();
        let pool = buffers::io_test_pool(24);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        let mut rng = Random(seed);
        let mut now = 0;
        let mut leases: Vec<(Metadata, Expected)> = Vec::new();
        let mut crashes = 0;
        let mut executions = 0;
        let mut mutations = 0;
        for step in 0..steps {
            let r = rng.next();
            let action = r % 12;
            let k = rng.next() % 80;
            let budget = 1 + (rng.next() % 8) as usize;
            trace.push(format!("{step}: action={action} key={k} stage={} generation={} budget={budget} pending={:?} deliveries={:?}",
                stage(&a), a.generation(), sim.pending(), sim.deliveries()));
            match action {
                0..=2 => {
                    // Bound pinned buffers, while still admitting during an
                    // active checkpoint. Pressure itself has a dedicated test.
                    let retained = sim.requests.iter().filter(|r| r.job.is_some()).count()
                        + a.pending.len()
                        + leases.len();
                    if retained < 16 {
                        let expires = if r & 16 != 0 { now + 3 } else { 0 };
                        insert(
                            &mut a,
                            &pool,
                            &mut model.live,
                            k,
                            step as u64,
                            17 + (r % 600) as usize,
                            Kind::Metadata,
                            expires,
                        );
                        mutations += 1;
                    }
                }
                3 => {
                    assert_eq!(a.remove(&key(k)), model.live.remove(&key(k)).is_some());
                }
                4 => {
                    now += 1;
                    if model
                        .live
                        .get(&key(k))
                        .is_some_and(|v| v.info.expires != 0 && v.info.expires <= now)
                    {
                        model.live.remove(&key(k));
                    }
                    let lease = a.lookup_metadata(&key(k), now);
                    assert_eq!(lease.is_some(), model.live.contains_key(&key(k)));
                    if let Some(lease) = lease {
                        assert_eq!(expected_metadata(lease), model.live[&key(k)]);
                        if leases.len() < 3 {
                            leases.push((lease, model.live[&key(k)].clone()));
                        }
                    }
                }
                5 => {
                    if let Some(k) = a.evict(Kind::Metadata, now) {
                        assert!(model.live.remove(&k).is_some());
                    }
                    if !leases.is_empty() {
                        drop(leases.remove((r as usize / 12) % leases.len()));
                    }
                }
                6 => {
                    if r & 16 != 0 {
                        sim.next_submission = Some(Fault::Submit(io::ErrorKind::WouldBlock));
                    }
                    model.progress(&mut a, &mut sim, budget).unwrap();
                    // A one-step injection does not leak into later actions.
                    sim.next_submission = None;
                }
                7 => {
                    let ids = sim.pending();
                    if !ids.is_empty() {
                        let id = ids[(rng.next() as usize) % ids.len()];
                        trace.push(format!("execute request={id}"));
                        model.execute(&a, &mut sim, id);
                        executions += 1;
                    }
                }
                8 => {
                    let ids = sim.deliveries();
                    if !ids.is_empty() {
                        let id = ids[(rng.next() as usize) % ids.len()];
                        trace.push(format!("deliver request={id}"));
                        sim.deliver(id);
                    }
                }
                9 => {
                    let mask = rng.next();
                    trace.push(format!("probe mask={mask:#x}"));
                    model.probe(&f, &a, &sim.crash_image(mask));
                }
                10 if step % 3 == 0 => {
                    let mask = rng.next();
                    trace.push(format!("power loss mask={mask:#x}"));
                    let image = sim.crash_image(mask);
                    let g = a.space.geometry;
                    leases.clear();
                    drop(a);
                    drop(sim);
                    (a, sim) = assert_recovery(
                        &f,
                        &image,
                        g,
                        &model.snapshots,
                        model.durable_generation,
                        *model.snapshots.last_key_value().unwrap().0,
                    );
                    model.live = model.snapshots[&a.generation()].clone();
                    model
                        .snapshots
                        .retain(|generation, _| *generation <= a.generation());
                    // Restart uses the selected surviving bytes as its stable
                    // base. Generations lost in this lifetime may be reused.
                    model.durable_generation = a.generation();
                    crashes += 1;
                }
                _ => {
                    model.flush(&mut a, &mut sim);
                }
            }
            model.check_live(&a, &sim);
            for (lease, expected) in &leases {
                assert_eq!(&expected_metadata(*lease), expected);
            }
        }
        model.flush(&mut a, &mut sim);
        model.probe(&f, &a, &sim.durable);
        if steps >= 1000 {
            assert!(crashes > 0 && executions > 0 && mutations > 0);
        }
    }));
    if let Err(error) = result {
        eprintln!(
            "allocator simulation failed: RACER_ALLOCATOR_SEED={seed} RACER_ALLOCATOR_STEPS={steps}\n{}",
            trace.join("\n")
        );
        std::panic::resume_unwind(error);
    }
}
fn env_number(name: &str, default: u64) -> u64 {
    std::env::var(name)
        .map(|v| v.parse().expect("expected unsigned decimal integer"))
        .unwrap_or(default)
}

#[test]
fn seeded_checkpoint_crash_workload() {
    let steps = env_number("RACER_ALLOCATOR_STEPS", 1200) as usize;
    if std::env::var_os("RACER_ALLOCATOR_SEED").is_some() {
        campaign(env_number("RACER_ALLOCATOR_SEED", 0), steps);
    } else {
        for seed in [0, 1, 3, 0xdead_beef] {
            campaign(seed, steps);
        }
    }
}

#[test]
#[ignore = "long allocator campaign; override RACER_ALLOCATOR_SEED/STEPS to replay"]
fn extended_seeded_checkpoint_crash_workload() {
    let first = env_number("RACER_ALLOCATOR_SEED", 0);
    let steps = env_number("RACER_ALLOCATOR_STEPS", 10000) as usize;
    for seed in first..first + 32 {
        campaign(seed, steps);
    }
}
