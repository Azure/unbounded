// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Additive parity for logical Job effects, not physical RingIo execution.
//! The common domain is latest-sector persistence without punches or historical
//! sector versions. Inputs are copied before Sim::execute; its output images are
//! only comparison oracles, never the source of replayed writes.
//! Partial-sync propagation is opt-in; the default profile rejects Alternating.

use super::*;
use crate::simulation::{Disk, World};
use std::collections::BTreeSet;

#[derive(Clone, Copy)]
enum Profile {
    LatestOnly,
    PartialSync,
}

enum Effect {
    Write {
        offset: u64,
        bytes: Vec<u8>,
        prefix: usize,
    },
    Sync(Persistence),
}

impl Effect {
    fn capture(request: &Request) -> Self {
        Self::capture_with_profile(request, Profile::LatestOnly)
    }

    fn capture_with_profile(request: &Request, profile: Profile) -> Self {
        assert!(!request.executed);
        match request.job.as_ref().unwrap() {
            Job::Page(page, offset) => Self::write(*offset, page.0.to_vec(), request.fault),
            Job::Value(value) => Self::write(
                value.allocation.offset(),
                value.buffer.borrow().as_ref().unwrap().as_slice().to_vec(),
                request.fault,
            ),
            Job::Sync => Self::Sync(match request.fault {
                None => Persistence::All,
                Some(Fault::Sync(Persistence::None)) => Persistence::None,
                Some(Fault::Sync(Persistence::All)) => Persistence::All,
                Some(Fault::Sync(Persistence::Alternating))
                    if matches!(profile, Profile::PartialSync) =>
                {
                    Persistence::Alternating
                }
                Some(Fault::Sync(Persistence::Alternating)) => {
                    panic!("Alternating sync persistence is outside the parity domain")
                }
                _ => panic!("unsupported sync fault in persistence parity"),
            }),
        }
    }

    fn write(offset: u64, bytes: Vec<u8>, fault: Option<Fault>) -> Self {
        assert!(offset.is_multiple_of(512));
        let prefix = match fault {
            None => bytes.len(),
            Some(Fault::Write(n)) => n.min(bytes.len()),
            _ => panic!("unsupported write fault in persistence parity"),
        };
        Self::Write {
            offset,
            bytes,
            prefix,
        }
    }

    fn apply(&self, disk: &Disk) {
        match self {
            Self::Write {
                offset,
                bytes,
                prefix,
            } => disk.write_all_at(&bytes[..*prefix], *offset).unwrap(),
            Self::Sync(Persistence::All) => disk.sync_data().unwrap(),
            Self::Sync(Persistence::None) => {}
            Self::Sync(Persistence::Alternating) => {
                // Disk reports sector indices, while Sim::persist uses byte
                // offsets. Select independently without importing Sim's image.
                let sectors: Vec<_> = disk
                    .dirty_sectors()
                    .into_iter()
                    .filter(|sector| sector.is_multiple_of(2))
                    .collect();
                disk.persist_sectors(&sectors).unwrap();
            }
        }
    }
}

struct Replay {
    profile: Profile,
    initial: Image,
    geometry: Geometry,
    effects: Vec<Effect>,
    // Byte offsets, like Image. Disk crash selection takes sector indices.
    touched: BTreeSet<u64>,
}

impl Replay {
    fn new(a: &Allocator, sim: &Sim) -> Self {
        Self::with_profile(a, sim, Profile::LatestOnly)
    }

    fn with_profile(a: &Allocator, sim: &Sim, profile: Profile) -> Self {
        assert!(sim.requests.is_empty());
        assert_eq!(sim.volatile.0, sim.durable.0);
        Self {
            profile,
            initial: sim.durable.clone(),
            geometry: a.space.geometry,
            effects: Vec::new(),
            touched: sim.durable.0.keys().copied().collect(),
        }
    }

    fn execute(&mut self, a: &Allocator, sim: &mut Sim, model: &mut Model, id: usize) {
        assert_eq!(
            self.effects.len(),
            sim.requests.iter().filter(|r| r.executed).count(),
            "every executed Job must be captured"
        );
        // Copy while the request still owns its page/value buffer, immediately
        // before the legacy model applies the effect. Collection may drop it.
        let effect = match self.profile {
            Profile::LatestOnly => Effect::capture(&sim.requests[id]),
            Profile::PartialSync => Effect::capture_with_profile(&sim.requests[id], self.profile),
        };
        if let Effect::Write { offset, prefix, .. } = &effect {
            self.touched
                .extend((0..prefix.div_ceil(512)).map(|i| offset + i as u64 * 512));
        }
        self.effects.push(effect);
        // Model::execute advances the durability floor at successful final-sync
        // EFFECT, even while its completion is undelivered and uncollected.
        model.execute(a, sim, id);
    }

    fn rebuild(&self) -> Disk {
        // Disk::clone shares an Arc. Every probe instead replays owned inputs
        // into new storage, including all barriers before the chosen crash.
        let disk = Disk::new(self.geometry.base + self.geometry.len);
        for (&offset, bytes) in &self.initial.0 {
            disk.write_all_at(bytes, offset).unwrap();
        }
        disk.sync_data().unwrap();
        for effect in &self.effects {
            effect.apply(&disk);
        }
        assert!(disk.pending_versions().is_empty());
        disk
    }

    fn export(&self, disk: &Disk) -> Image {
        // Geometry is sparse and payload offsets can be far apart. Never read
        // the whole slab just to feed the existing exact-byte snapshot oracle.
        let mut image = Image::default();
        for &offset in &self.touched {
            let mut sector = vec![0; 512];
            disk.read_exact_at(&mut sector, offset).unwrap();
            image.0.insert(offset, sector);
        }
        image
    }

    fn assert_bytes(&self, disk: &Disk, expected: &Image) {
        assert!(expected.0.keys().all(|k| self.touched.contains(k)));
        for (&offset, sector) in &self.export(disk).0 {
            assert_eq!(*sector, expected.read(offset, 512), "sector at {offset}");
        }
    }

    fn probe(
        &self,
        fixture: &Fixture,
        a: &Allocator,
        sim: &Sim,
        model: &Model,
        selected: Vec<u64>,
        legacy_image: &Image,
    ) -> [Option<u64>; 2] {
        assert_eq!(self.geometry, a.space.geometry);
        assert_eq!(
            self.effects.len(),
            sim.requests.iter().filter(|r| r.executed).count()
        );
        let disk = self.rebuild();
        self.assert_bytes(&disk, &sim.volatile);
        disk.select_crash_sectors(selected);
        disk.crash(0);
        self.assert_bytes(&disk, legacy_image);

        // Fixture::open uses an independent OS inode and checks each legacy root
        // against workload snapshots. Sim recovery uses the production parser.
        let (legacy, legacy_sim) = model.probe(fixture, a, legacy_image);
        let world = World::new(0);
        let _scope = world.enter();
        let recovered = open_disk(&disk, self.geometry);
        let image = self.export(&disk);
        assert_eq!(
            retained_generations(&recovered),
            retained_generations(&legacy)
        );
        assert_eq!(recovered.generation(), legacy.generation());
        assert!(recovered.generation() >= model.durable_generation);
        assert!(recovered.generation() <= *model.snapshots.last_key_value().unwrap().0);
        for checkpoint in recovered.checkpoints.iter().flatten() {
            assert_snapshot(
                &checkpoint.root,
                &image,
                &model.snapshots[&checkpoint.generation],
            );
        }
        // Recovery may durably zero an invalid magic slot. Compare those writes
        // too, rather than treating the pre-open crash image as the final image.
        self.assert_bytes(&disk, &legacy_sim.durable);
        disk.crash(0);
        self.assert_bytes(&disk, &legacy_sim.durable);
        retained_generations(&recovered)
    }

    fn probe_mask(
        &self,
        fixture: &Fixture,
        a: &Allocator,
        sim: &Sim,
        model: &Model,
        mask: u64,
    ) -> [Option<u64>; 2] {
        let selected = self
            .touched
            .iter()
            .map(|offset| offset / 512)
            .filter(|sector| mask & (1u64 << (sector % 64)) != 0)
            .collect();
        self.probe(fixture, a, sim, model, selected, &sim.crash_image(mask))
    }

    fn flush(&mut self, a: &mut Allocator, sim: &mut Sim, model: &mut Model) {
        for _ in 0..10000 {
            model.progress(a, sim, 32).unwrap();
            for id in sim.pending().into_iter().rev() {
                self.execute(a, sim, model, id);
            }
            for id in sim.deliveries().into_iter().rev() {
                sim.deliver(id);
            }
            if a.is_idle() {
                model.check_live(a, sim);
                return;
            }
        }
        panic!("parity flush did not converge");
    }

    fn reach_stage(&mut self, a: &mut Allocator, sim: &mut Sim, model: &mut Model, target: usize) {
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
                self.execute(a, sim, model, id);
                sim.deliver(id);
            }
        }
        panic!("parity did not reach stage {target}");
    }
}

fn open_disk(disk: &Disk, geometry: Geometry) -> Allocator {
    Allocator::open_inner(
        SlabShard {
            pressure: Arc::default(),
            file: Arc::new(SlabFile::Sim(disk.clone())),
            geometry,
        },
        config(),
    )
    .unwrap()
}

fn retained_generations(a: &Allocator) -> [Option<u64>; 2] {
    a.checkpoints
        .each_ref()
        .map(|checkpoint| checkpoint.as_ref().map(|c| c.generation))
}

// Both generations have distinct metadata and payload bytes. Merely agreeing on
// the newest root cannot satisfy the independent older-root snapshot assertion.
fn replace(a: &mut Allocator, pool: &WorkerPool, model: &mut Model, version: u64) {
    insert(a, pool, &mut model.live, 1, version, 64, Kind::Metadata, 0);
    insert(a, pool, &mut model.live, 2, version, 513, Kind::Payload, 0);
}

#[test]
fn publication_boundaries_and_all_torn_magic_sector_masks() {
    let (fixture, mut a) = Fixture::new();
    let pool = buffers::io_test_pool(4);
    let mut sim = Sim::new(&a);
    let mut model = Model::new();
    let mut replay = Replay::new(&a, &sim);
    replace(&mut a, &pool, &mut model, 1);
    replay.flush(&mut a, &mut sim, &mut model);
    replace(&mut a, &pool, &mut model, 2);
    let mut masks_tested = 0;
    let mut final_sync_tested = false;
    for _ in 0..100 {
        model.progress(&mut a, &mut sim, 1).unwrap();
        for id in sim.pending().into_iter().rev() {
            for mask in [0, u64::MAX, 0xaaaa_aaaa_aaaa_aaaa] {
                replay.probe_mask(&fixture, &a, &sim, &model, mask);
            }
            let magic_offset = if stage(&a) == 2 {
                let Some(Job::Page(_, offset)) = sim.requests[id].job.as_ref() else {
                    panic!("expected magic page");
                };
                Some(*offset)
            } else {
                None
            };
            let before = sim.durable.clone();
            replay.execute(&a, &mut sim, &mut model, id);
            if let Some(offset) = magic_offset {
                for mask in 0..256u64 {
                    let mut crash = before.clone();
                    let mut selected = Vec::new();
                    for sector in 0..8 {
                        if mask & (1 << sector) != 0 {
                            let at = offset + sector * 512;
                            crash.0.insert(at, sim.volatile.0[&at].clone());
                            selected.push(at / 512);
                        }
                    }
                    replay.probe(&fixture, &a, &sim, &model, selected, &crash);
                    masks_tested += 1;
                }
            }
            for mask in [0, u64::MAX, 0x5555_5555_5555_5555] {
                replay.probe_mask(&fixture, &a, &sim, &model, mask);
            }
            if stage(&a) == 3 {
                assert_eq!(a.generation(), 3);
                assert_eq!(model.durable_generation, 4);
                assert!(!sim.requests[id].available);
                let slots = replay.probe_mask(&fixture, &a, &sim, &model, 0);
                assert!(slots.contains(&Some(3)) && slots.contains(&Some(4)));
                final_sync_tested = true;
            }
            let current = stage(&a);
            model.progress(&mut a, &mut sim, 1).unwrap();
            assert_eq!(stage(&a), current);
            sim.deliver(id);
            replay.probe_mask(&fixture, &a, &sim, &model, 0);
        }
        if a.is_idle() {
            break;
        }
    }
    assert!(a.is_idle());
    assert_eq!(masks_tested, 256);
    assert!(final_sync_tested);
    assert_eq!(a.generation(), 4);

    // Sensitivity: the same slot comparison used by every probe rejects loss of
    // the older root even when the newest generation and its bytes still match.
    let expected_slots = replay.probe_mask(&fixture, &a, &sim, &model, 0);
    let disk = replay.rebuild();
    disk.crash(0);
    let world = World::new(0);
    let _scope = world.enter();
    let recovered = open_disk(&disk, a.space.geometry);
    let older = recovered
        .checkpoints
        .iter()
        .position(|c| c.as_ref().is_some_and(|c| c.generation == 3))
        .unwrap();
    assert_snapshot(
        &recovered.checkpoints[older].as_ref().unwrap().root,
        &replay.export(&disk),
        &model.snapshots[&3],
    );
    drop(recovered);
    disk.write_all_at(&[0; PAGE_SIZE], a.space.geometry.offset(older))
        .unwrap();
    disk.sync_data().unwrap();
    let recovered = open_disk(&disk, a.space.geometry);
    assert!(recovered.checkpoints[older].is_none());
    assert_eq!(recovered.generation(), 4);
    assert_snapshot(&recovered.root, &replay.export(&disk), &model.snapshots[&4]);
    assert_ne!(retained_generations(&recovered), expected_slots);
}

#[test]
fn failed_write_prefixes_and_sync_none_all() {
    for target in 0..4 {
        let faults = if target == 0 || target == 2 {
            vec![
                Fault::Write(0),
                Fault::Write(1),
                Fault::Write(512),
                Fault::Write(513),
                Fault::Write(usize::MAX),
            ]
        } else {
            vec![
                Fault::Sync(Persistence::None),
                Fault::Sync(Persistence::All),
            ]
        };
        for fault in faults {
            // The Writes stage includes payload, tree and bitmap jobs.
            for position in 0..if target == 0 { 3 } else { 1 } {
                let (fixture, mut a) = Fixture::new();
                let pool = buffers::io_test_pool(4);
                let mut sim = Sim::new(&a);
                let mut model = Model::new();
                let mut replay = Replay::new(&a, &sim);
                replace(&mut a, &pool, &mut model, 1);
                replay.flush(&mut a, &mut sim, &mut model);
                replace(&mut a, &pool, &mut model, 2);
                replay.reach_stage(&mut a, &mut sim, &mut model, target);
                model.progress(&mut a, &mut sim, 32).unwrap();
                let ids = sim.pending();
                assert_eq!(ids.len(), if target == 0 { 3 } else { 1 });
                sim.requests[ids[position]].fault = Some(fault);
                for id in ids {
                    replay.execute(&a, &mut sim, &mut model, id);
                    for mask in [0, u64::MAX, 0x5555_5555_5555_5555] {
                        replay.probe_mask(&fixture, &a, &sim, &model, mask);
                    }
                    sim.deliver(id);
                }
                assert!(model.progress(&mut a, &mut sim, 32).is_err());
                assert!(a.failed, "stage {target}, {fault:?}");
                assert_eq!(a.generation(), 3);
                assert_eq!(model.durable_generation, 3);
                for mask in [0, u64::MAX, 0xaaaa_aaaa_aaaa_aaaa] {
                    let slots = replay.probe_mask(&fixture, &a, &sim, &model, mask);
                    if target == 3 && matches!(fault, Fault::Sync(Persistence::All)) {
                        assert!(slots.contains(&Some(3)) && slots.contains(&Some(4)));
                    }
                }
            }
        }
    }
}

// Use all sectors of a page so that both parity classes actually differ from
// the durable floor. Small magic pages may differ only in their first sector.
fn scratch_write(sim: &mut Sim, offset: u64, byte: u8, fault: Option<Fault>) -> usize {
    let ticket = sim
        .submit(Job::Page(Box::new(uring::Page([byte; PAGE_SIZE])), offset))
        .unwrap_or_else(|_| panic!("scratch submission rejected"));
    let IoTicket::Sim(id) = ticket else {
        unreachable!()
    };
    sim.requests[id].fault = fault;
    id
}

fn assert_partial_sync_masks(
    replay: &Replay,
    fixture: &Fixture,
    a: &Allocator,
    sim: &Sim,
    model: &Model,
) {
    for mask in [0, u64::MAX, 0x5555_5555_5555_5555, 0xaaaa_aaaa_aaaa_aaaa] {
        let slots = replay.probe_mask(fixture, a, sim, model, mask);
        assert!(slots.contains(&Some(3)), "durable predecessor missing");
    }
}

#[test]
fn partial_sync_profile_failed_alternating_at_both_checkpoint_barriers() {
    for target in [1, 3] {
        let (fixture, mut a) = Fixture::new();
        let pool = buffers::io_test_pool(4);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        let mut replay = Replay::with_profile(&a, &sim, Profile::PartialSync);
        replace(&mut a, &pool, &mut model, 1);
        replay.flush(&mut a, &mut sim, &mut model);
        replace(&mut a, &pool, &mut model, 2);
        replay.reach_stage(&mut a, &mut sim, &mut model, target);
        model.progress(&mut a, &mut sim, 32).unwrap();
        let pending = sim.pending();
        assert_eq!(pending.len(), 1);
        let sync = pending[0];
        assert!(matches!(sim.requests[sync].job, Some(Job::Sync)));
        sim.requests[sync].fault = Some(Fault::Sync(Persistence::Alternating));

        // An independently reserved scratch page is outside both roots and the
        // frozen checkpoint. It makes None/All substitutions observably wrong
        // at either barrier without corrupting the logical snapshot oracle.
        let scratch = a.space.allocate(Class::Index).unwrap();
        let offset = scratch.offset();
        assert!(offset.is_multiple_of(PAGE_SIZE as u64));
        let id = scratch_write(&mut sim, offset, 0xa5, None);
        replay.execute(&a, &mut sim, &mut model, id);
        sim.deliver(id);
        sim.complete(&mut IoTicket::Sim(id))
            .unwrap()
            .unwrap()
            .unwrap();
        let before = sim.durable.clone();
        let live_before = sim.volatile.clone();
        replay.execute(&a, &mut sim, &mut model, sync);
        assert_eq!(sim.volatile.0, live_before.0);
        assert!(sim.requests[sync].result.as_ref().unwrap().is_err());
        assert!(!sim.requests[sync].available);
        assert_eq!(model.durable_generation, 3);
        let disk = replay.rebuild();
        replay.assert_bytes(&disk, &sim.volatile);
        disk.crash(0);
        for sector in 0..8 {
            let at = offset + sector * 512;
            let expected = if sector.is_multiple_of(2) {
                vec![0xa5; 512]
            } else {
                before.read(at, 512)
            };
            assert_eq!(sim.durable.read(at, 512), expected);
            let mut actual = [0; 512];
            disk.read_exact_at(&mut actual, at).unwrap();
            assert_eq!(actual.as_slice(), expected);
        }
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);
        model.progress(&mut a, &mut sim, 1).unwrap();
        assert_eq!(stage(&a), target, "effect does not collect failed sync");
        sim.deliver(sync);
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);
        assert!(model.progress(&mut a, &mut sim, 32).is_err());
        assert!(a.failed);
        assert_eq!(a.generation(), 3);
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);
        let slots = replay.probe_mask(&fixture, &a, &sim, &model, u64::MAX);
        assert!(slots.contains(&Some(if target == 1 { 2 } else { 4 })));
    }
}

#[test]
fn partial_sync_profile_preserves_floor_across_later_writes_and_barrier() {
    for prefix in [0, 1, 512, 513, usize::MAX] {
        let (fixture, mut a) = Fixture::new();
        let pool = buffers::io_test_pool(4);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        let mut replay = Replay::with_profile(&a, &sim, Profile::PartialSync);
        replace(&mut a, &pool, &mut model, 1);
        replay.flush(&mut a, &mut sim, &mut model);
        replace(&mut a, &pool, &mut model, 2);
        replay.flush(&mut a, &mut sim, &mut model);
        let slots = retained_generations(&a);
        assert!(slots.contains(&Some(3)) && slots.contains(&Some(4)));
        let scratch = a.space.allocate(Class::Index).unwrap();
        let offset = scratch.offset();

        // This suffix exercises the Storage/Disk effect contract directly. It
        // never resumes a poisoned allocator or claims these are new checkpoints.
        let before = sim.durable.clone();
        let first = scratch_write(&mut sim, offset, 0x39, None);
        replay.execute(&a, &mut sim, &mut model, first);
        let IoTicket::Sim(sync) = sim
            .submit(Job::Sync)
            .unwrap_or_else(|_| panic!("sync rejected"))
        else {
            unreachable!()
        };
        sim.requests[sync].fault = Some(Fault::Sync(Persistence::Alternating));
        replay.execute(&a, &mut sim, &mut model, sync);
        assert!(sim.requests[sync].result.as_ref().unwrap().is_err());
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);
        let floor = sim.durable.clone();
        let later = scratch_write(&mut sim, offset, 0xc7, Some(Fault::Write(prefix)));
        replay.execute(&a, &mut sim, &mut model, later);
        assert!(sim.requests[later].result.as_ref().unwrap().is_err());
        let mut expected_page = vec![0x39; PAGE_SIZE];
        expected_page[..prefix.min(PAGE_SIZE)].fill(0xc7);
        assert_eq!(sim.volatile.read(offset, PAGE_SIZE), expected_page);
        assert_eq!(
            sim.durable.0, floor.0,
            "later writes cannot move the sync floor"
        );
        let disk = replay.rebuild();
        replay.assert_bytes(&disk, &sim.volatile);
        disk.crash(0);
        replay.assert_bytes(&disk, &floor);
        for sector in 0..8 {
            assert_eq!(
                floor.read(offset + sector * 512, 512),
                if sector.is_multiple_of(2) {
                    vec![0x39; 512]
                } else {
                    before.read(offset + sector * 512, 512)
                },
            );
        }
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);

        // Deliver in a different order from effects, including the failed sync
        // only after the subsequent failed-prefix write has already executed.
        for id in [later, sync, first] {
            sim.deliver(id);
            let result = sim.complete(&mut IoTicket::Sim(id)).unwrap().unwrap();
            assert_eq!(result.is_ok(), id == first);
        }
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);
        let IoTicket::Sim(barrier) = sim
            .submit(Job::Sync)
            .unwrap_or_else(|_| panic!("sync rejected"))
        else {
            unreachable!()
        };
        replay.execute(&a, &mut sim, &mut model, barrier);
        assert_eq!(sim.durable.0, sim.volatile.0);
        assert_eq!(sim.durable.read(offset, PAGE_SIZE), expected_page);
        assert_partial_sync_masks(&replay, &fixture, &a, &sim, &model);
        sim.deliver(barrier);
        sim.complete(&mut IoTicket::Sim(barrier))
            .unwrap()
            .unwrap()
            .unwrap();
        assert_eq!(replay.probe_mask(&fixture, &a, &sim, &model, 0), slots);
        assert_eq!(a.generation(), 4);
        assert_eq!(model.durable_generation, 4);
    }
}

#[test]
#[should_panic(expected = "Alternating sync persistence is outside the parity domain")]
fn alternating_sync_is_explicitly_rejected() {
    Effect::capture(&Request {
        job: Some(Job::Sync),
        executed: false,
        available: false,
        result: None,
        fault: Some(Fault::Sync(Persistence::Alternating)),
    });
}
