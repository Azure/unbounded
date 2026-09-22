// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Physical RingIo refinement of logical workload snapshots. The oracle copies
//! submitted inputs, never Disk output. One outstanding operation makes effect,
//! CQ delivery, and allocator collection separately observable without raw SQEs.

use super::*;
use crate::simulation::{Disk, World};
use std::collections::BTreeSet;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Phase {
    Punch,
    Value,
    Page,
    DataSync,
    Magic,
    FinalSync,
}

impl Phase {
    fn opcode(self) -> usize {
        match self {
            Self::Punch => 17,
            Self::Value => 5,
            Self::Page | Self::Magic => 23,
            Self::DataSync | Self::FinalSync => 3,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Event {
    Submit(Phase),
    Effect(Phase),
    Deliver(Phase),
    Collect(Phase, bool),
    Published,
}

enum Input {
    Punch(u64),
    Write(u64, Vec<u8>),
    Sync,
}

struct Operation {
    phase: Phase,
    input: Input,
    fault: Option<bool>, // false: before effect; true: error after full effect
    executed: bool,
    delivered: bool,
}

struct Transcript {
    events: Vec<Event>,
    active: Option<Operation>,
    volatile: Image,
    durable: Image,
    touched: BTreeSet<u64>,
    failure: Option<(Phase, bool)>,
    floor: u64,
}

impl Transcript {
    fn new(image: Image, failure: Option<(Phase, bool)>) -> Self {
        Self {
            touched: image.0.keys().copied().collect(),
            volatile: image.clone(),
            durable: image,
            events: Vec::new(),
            active: None,
            failure,
            floor: 4,
        }
    }

    fn submitted(&mut self, phase: Phase, input: Input, world: &World, disk: &Disk) {
        assert!(
            self.active.is_none(),
            "serialized fixture: {:?}",
            self.events
        );
        let fault = self.failure.filter(|(p, _)| *p == phase).map(|(_, f)| f);
        if let Some(after) = fault {
            self.failure = None;
            if after {
                disk.fail_after_effect(phase.opcode() as u8);
            } else {
                world.fail_next_errno(phase.opcode() as u8, libc::ENOSPC);
            }
        }
        if let Input::Write(offset, bytes) = &input {
            self.touched
                .extend((0..bytes.len().div_ceil(512)).map(|i| offset + i as u64 * 512));
        }
        self.events.push(Event::Submit(phase));
        self.active = Some(Operation {
            phase,
            input,
            fault,
            executed: false,
            delivered: false,
        });
    }

    fn effect(&mut self) {
        let op = self.active.as_mut().unwrap();
        assert!(!op.executed);
        op.executed = true;
        if op.fault != Some(false) {
            match &op.input {
                Input::Punch(offset) => {
                    // Byte refinement only: the historical-hole test below also
                    // distinguishes the punch version from the later write.
                    for (&at, sector) in &mut self.volatile.0 {
                        if (*offset..offset + WIDE).contains(&at) {
                            sector.fill(0);
                        }
                    }
                }
                Input::Write(offset, bytes) => self.volatile.write(*offset, bytes),
                Input::Sync => self.durable = self.volatile.clone(),
            }
            if op.phase == Phase::FinalSync {
                self.floor = 5;
            }
        }
        self.events.push(Event::Effect(op.phase));
    }

    fn collected(&mut self, phase: Phase, success: bool) {
        let op = self.active.take().unwrap();
        assert_eq!(op.phase, phase);
        assert!(op.executed && op.delivered);
        assert_eq!(success, op.fault.is_none());
        self.events.push(Event::Collect(phase, success));
    }

    fn assert_bytes(&self, disk: &Disk, expected: &Image) {
        for &offset in &self.touched {
            let mut sector = [0; 512];
            disk.read_exact_at(&mut sector, offset).unwrap();
            assert_eq!(
                sector.as_slice(),
                expected.read(offset, 512),
                "sector {offset}, transcript {:?}",
                self.events
            );
        }
    }
}

struct RecordingIo<'a> {
    io: RingIo<'a>,
    transcript: &'a mut Transcript,
    world: &'a World,
    disk: &'a Disk,
    stage: usize,
}

impl Storage for RecordingIo<'_> {
    type Ticket = IoTicket;

    fn submit(&mut self, job: Job) -> Result<IoTicket, uring::Rejected<Job>> {
        // Capture before handing ownership to production RingIo.
        let (phase, input) = match &job {
            Job::Value(value) => (Phase::Punch, Input::Punch(value.allocation.offset())),
            Job::Page(page, offset) => (
                if self.stage == 2 {
                    Phase::Magic
                } else {
                    Phase::Page
                },
                Input::Write(*offset, page.0.to_vec()),
            ),
            Job::Sync => (
                if self.stage == 3 {
                    Phase::FinalSync
                } else {
                    Phase::DataSync
                },
                Input::Sync,
            ),
        };
        let ticket = self.io.submit(job)?;
        self.transcript
            .submitted(phase, input, self.world, self.disk);
        Ok(ticket)
    }

    fn complete(&mut self, ticket: &mut IoTicket) -> io::Result<Option<io::Result<()>>> {
        let phase = self.transcript.active.as_ref().unwrap().phase;
        let write = match ticket {
            IoTicket::Punch(_, value) => Some(Input::Write(
                value.allocation.offset(),
                value.buffer.borrow().as_ref().unwrap().as_slice().to_vec(),
            )),
            _ => None,
        };
        let result = self.io.complete(ticket)?;
        if let Some(done) = &result {
            if let Err(error) = done {
                assert_eq!(error.raw_os_error(), Some(libc::ENOSPC));
            }
            self.transcript.collected(phase, done.is_ok());
        } else if let Some(input) = write {
            if matches!(ticket, IoTicket::Value(..)) {
                self.transcript.collected(Phase::Punch, true);
                self.transcript
                    .submitted(Phase::Value, input, self.world, self.disk);
            } else {
                assert!(matches!(ticket, IoTicket::Punch(..)));
            }
        }
        Ok(result)
    }
}

struct Baseline {
    fixture: Fixture,
    geometry: Geometry,
    image: Image,
    model: Model,
}

impl Baseline {
    fn new() -> Self {
        // One leaf and one bitmap keep the serialized transcript explicit.
        let (fixture, mut a) = Fixture::with_size(8 * WIDE);
        let pool = buffers::io_test_pool(2);
        let mut sim = Sim::new(&a);
        let mut model = Model::new();
        for version in 1..=2 {
            insert(
                &mut a,
                &pool,
                &mut model.live,
                1,
                version,
                64,
                Kind::Metadata,
                0,
            );
            insert(
                &mut a,
                &pool,
                &mut model.live,
                2,
                version,
                513,
                Kind::Payload,
                0,
            );
            model.flush(&mut a, &mut sim);
        }
        assert_eq!(a.generation(), 4);
        Self {
            fixture,
            geometry: a.space.geometry,
            image: sim.durable.clone(),
            model,
        }
    }
}

struct Run {
    a: Allocator,
    ring: Ring,
    disk: Disk,
    pool: WorkerPool,
    model: Model,
    transcript: Transcript,
    offset: u64,
    index: usize,
}

impl Run {
    fn new(base: &Baseline, failure: Option<(Phase, bool)>) -> Self {
        let disk = Disk::new(base.geometry.base + base.geometry.len);
        for (&offset, bytes) in &base.image.0 {
            disk.write_all_at(bytes, offset).unwrap();
        }
        disk.sync_data().unwrap();
        let mut a = open(&disk, base.geometry);
        a.config.max_io = 1;
        let pool = buffers::io_test_pool(1);
        let ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
        let mut model = Model {
            live: base.model.live.clone(),
            snapshots: base.model.snapshots.clone(),
            durable_generation: 4,
        };
        insert(&mut a, &pool, &mut model.live, 1, 3, 64, Kind::Metadata, 0);
        insert(&mut a, &pool, &mut model.live, 2, 3, 513, Kind::Payload, 0);
        model.snapshots.insert(5, model.live.clone());
        let value = a.root.get(&key(2)).unwrap().payload().unwrap();
        let offset = value.allocation.offset();
        let index = value.allocation.index;
        // Model stale bytes in a free extent before its next physical reuse.
        // Neither retained root may own the extent being punched.
        for checkpoint in a.checkpoints.iter().flatten() {
            checkpoint.root.visit(&mut |_, entry| {
                if let Entry::Payload(old) = entry {
                    assert_ne!(old.allocation.offset(), offset);
                }
            });
        }
        let mut image = base.image.clone();
        for (at, bytes) in [
            (offset, vec![0xa5; 1024]),
            (offset + WIDE - 512, vec![0x5a; 512]),
        ] {
            disk.write_all_at(&bytes, at).unwrap();
            image.write(at, &bytes);
        }
        disk.sync_data().unwrap();
        disk.track_versions(256).unwrap();
        Self {
            a,
            ring,
            disk,
            pool,
            model,
            transcript: Transcript::new(image, failure),
            offset,
            index,
        }
    }

    fn progress(&mut self, world: &World) -> io::Result<()> {
        let before = self.a.generation();
        let mut io = RecordingIo {
            io: RingIo {
                ring: &mut self.ring,
                file: self.a.file.clone(),
                space: self.a.space.clone(),
            },
            transcript: &mut self.transcript,
            world,
            disk: &self.disk,
            stage: stage(&self.a),
        };
        self.a.progress(&mut io, 1)?;
        if self.a.generation() != before {
            assert_eq!(self.a.generation(), 5);
            self.transcript.events.push(Event::Published);
        }
        Ok(())
    }

    fn tick(&mut self, world: &World) {
        let before = world.counts();
        let epoch = self.ring.completion_epoch();
        world.service_tick();
        self.ring.progress().unwrap();
        let after = world.counts();
        let effects: Vec<_> = (0..64).filter(|&i| after[i] != before[i]).collect();
        if !effects.is_empty() {
            let op = self.transcript.active.as_ref().unwrap().phase.opcode();
            assert_eq!(effects, vec![op]);
            assert_eq!(after[op], before[op] + 1);
            self.transcript.effect();
        }
        if self.ring.completion_epoch() != epoch {
            let op = self.transcript.active.as_mut().unwrap();
            assert!(op.executed && !op.delivered);
            op.delivered = true;
            self.transcript.events.push(Event::Deliver(op.phase));
        }
        self.transcript
            .assert_bytes(&self.disk, &self.transcript.volatile);
    }

    fn reach(&mut self, world: &World, target: Event) {
        for _ in 0..200 {
            if self.transcript.events.contains(&target) {
                return;
            }
            // Examine effect/delivery before allowing allocator collection.
            self.progress(world).unwrap();
            if self.transcript.events.contains(&target) {
                return;
            }
            self.tick(world);
        }
        panic!("unreached {target:?}: {:?}", self.transcript.events);
    }

    fn crash(mut self, base: &Baseline, mask: u64) {
        let mut expected = self.transcript.durable.clone();
        let mut selected = Vec::new();
        for (&offset, bytes) in &self.transcript.volatile.0 {
            if mask & (1 << ((offset / 512) % 64)) != 0 {
                expected.0.insert(offset, bytes.clone());
                selected.push(offset / 512);
            }
        }
        // Stop pointer accesses before freeing allocator-owned buffers. No
        // graceful drain or sync may run between the chosen cut and the crash.
        self.ring.simulated_crash();
        self.disk.select_crash_sectors(selected);
        self.disk.crash(0);
        self.finish_crash(base, &expected);
    }

    fn finish_crash(self, base: &Baseline, expected: &Image) {
        self.transcript.assert_bytes(&self.disk, expected);
        self.check_recovery(base, expected);
        drop(self.a);
        self.pool.assert_recovered();
    }

    fn check_recovery(&self, base: &Baseline, expected: &Image) {
        let (legacy, legacy_sim) = assert_recovery(
            &base.fixture,
            expected,
            base.geometry,
            &self.model.snapshots,
            self.transcript.floor,
            5,
        );
        let recovered = open(&self.disk, base.geometry);
        assert_eq!(
            slots(&recovered),
            slots(&legacy),
            "{:?}",
            self.transcript.events
        );
        assert!(slots(&recovered).contains(&Some(4)), "retain predecessor");
        if self.transcript.floor == 5 {
            assert_eq!(slots(&recovered), [Some(5), Some(4)]);
        } else if !self
            .transcript
            .events
            .contains(&Event::Effect(Phase::Magic))
        {
            assert_eq!(slots(&recovered), [Some(3), Some(4)]);
        }
        for checkpoint in recovered.checkpoints.iter().flatten() {
            assert_snapshot(
                &checkpoint.root,
                expected,
                &self.model.snapshots[&checkpoint.generation],
            );
        }
        // Invalid-slot cleanup is part of recovery, including its durable sync.
        self.transcript
            .assert_bytes(&self.disk, &legacy_sim.durable);
        self.disk.crash(0);
        self.transcript
            .assert_bytes(&self.disk, &legacy_sim.durable);
    }
}

fn open(disk: &Disk, geometry: Geometry) -> Allocator {
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

fn slots(a: &Allocator) -> [Option<u64>; 2] {
    a.checkpoints
        .each_ref()
        .map(|c| c.as_ref().map(|c| c.generation))
}

#[test]
fn punch_write_and_checkpoint_crash_boundaries_preserve_both_roots() {
    let base = Baseline::new();
    let cuts = [
        Event::Submit(Phase::Punch),
        Event::Effect(Phase::Punch),
        Event::Deliver(Phase::Punch),
        Event::Submit(Phase::Value),
        Event::Effect(Phase::Value),
        Event::Deliver(Phase::Value),
        Event::Collect(Phase::Value, true),
        Event::Effect(Phase::DataSync),
        Event::Deliver(Phase::DataSync),
        Event::Effect(Phase::Magic),
        Event::Deliver(Phase::Magic),
        Event::Effect(Phase::FinalSync),
        Event::Deliver(Phase::FinalSync),
        Event::Published,
    ];
    for cut in cuts {
        for mask in [0, u64::MAX, 0x5555_5555_5555_5555] {
            let world = World::new(29);
            world.enable_scheduler();
            let _scope = world.enter();
            let mut run = Run::new(&base, None);
            run.reach(&world, cut);
            if cut == Event::Published {
                let mut expected = Vec::new();
                for phase in [
                    Phase::Punch,
                    Phase::Value,
                    Phase::Page,
                    Phase::Page,
                    Phase::DataSync,
                    Phase::Magic,
                    Phase::FinalSync,
                ] {
                    expected.extend([
                        Event::Submit(phase),
                        Event::Effect(phase),
                        Event::Deliver(phase),
                        Event::Collect(phase, true),
                    ]);
                }
                expected.push(Event::Published);
                assert_eq!(run.transcript.events, expected);
            }
            if matches!(
                cut,
                Event::Effect(Phase::FinalSync) | Event::Deliver(Phase::FinalSync)
            ) {
                assert_eq!(run.a.generation(), 4, "effect/delivery is not collection");
                assert_eq!(run.transcript.floor, 5);
            }
            if matches!(
                cut,
                Event::Effect(Phase::Value) | Event::Deliver(Phase::Value)
            ) {
                let value = run.a.root.get(&key(2)).unwrap().payload().unwrap();
                assert!(!value.written.get());
                assert_eq!(
                    value.buffer.borrow().as_ref().unwrap().as_slice(),
                    run.model.live[&key(2)].bytes
                );
                assert!(run.pool.private_fill().is_err());
                assert!(occupied(&run.a.space, Class::Payload, run.index));
            }
            if cut == Event::Collect(Phase::Value, true) {
                let value = run.a.root.get(&key(2)).unwrap().payload().unwrap();
                assert!(value.written.get());
                assert!(value.buffer.borrow().is_none());
                run.pool.assert_recovered();
                assert!(occupied(&run.a.space, Class::Payload, run.index));
            }
            run.crash(&base, mask);
            world.assert_clean();
        }
    }
}

#[test]
fn physical_failures_before_and_after_effect_preserve_snapshot_floor() {
    let base = Baseline::new();
    for phase in [
        Phase::Punch,
        Phase::Value,
        Phase::Page,
        Phase::DataSync,
        Phase::Magic,
        Phase::FinalSync,
    ] {
        for after in [false, true] {
            for mask in [0, u64::MAX] {
                let world = World::new(31);
                world.enable_scheduler();
                let _scope = world.enter();
                let mut run = Run::new(&base, Some((phase, after)));
                run.reach(&world, Event::Deliver(phase));
                assert!(run.progress(&world).is_err());
                assert!(run.a.failed);
                assert_eq!(run.a.generation(), 4);
                assert_eq!(
                    run.transcript.events.last(),
                    Some(&Event::Collect(phase, false))
                );
                assert!(run.transcript.failure.is_none());
                assert!(world.fault_fired() && run.disk.completion_fault_fired());
                if phase == Phase::Punch {
                    assert!(!run.transcript.events.contains(&Event::Submit(Phase::Value)));
                }
                if phase == Phase::FinalSync && after {
                    assert_eq!(
                        run.transcript.floor, 5,
                        "error can follow durable publication"
                    );
                }
                run.crash(&base, mask);
                world.assert_clean();
            }
        }
    }
}

#[test]
fn historical_punch_then_write_sector_prefixes_preserve_both_roots() {
    let base = Baseline::new();
    // Independent choices for both written sectors and a stale tail sector.
    // Version 1 is the hole, not the latest 513-byte write (version 2).
    for first in 0..=2 {
        for second in 0..=2 {
            for tail in 0..=1 {
                let world = World::new(37);
                world.enable_scheduler();
                let _scope = world.enter();
                let mut run = Run::new(&base, None);
                run.reach(&world, Event::Effect(Phase::Value));
                let sector = run.offset / 512;
                let last = (run.offset + WIDE - 512) / 512;
                assert_eq!(
                    run.disk.pending_versions(),
                    vec![(sector, 2), (sector + 1, 2), (last, 1)]
                );
                let mut expected = run.transcript.durable.clone();
                let payload = &run.model.live[&key(2)].bytes;
                for (i, version) in [first, second].into_iter().enumerate() {
                    if version != 0 {
                        let mut bytes = vec![0; 512];
                        if version == 2 {
                            let start = i * 512;
                            let n = (payload.len() - start).min(512);
                            bytes[..n].copy_from_slice(&payload[start..start + n]);
                        }
                        expected.write(run.offset + i as u64 * 512, &bytes);
                    }
                }
                if tail != 0 {
                    expected.write(last * 512, &[0; 512]);
                }
                run.ring.simulated_crash();
                run.disk
                    .crash_versions(&[(sector, first), (sector + 1, second), (last, tail)])
                    .unwrap();
                run.finish_crash(&base, &expected);
                world.assert_clean();
            }
        }
    }
}

#[test]
fn abandoned_physical_write_retains_buffer_and_extent_until_process_death() {
    let base = Baseline::new();
    for cut in [
        Event::Submit(Phase::Value),
        Event::Effect(Phase::Value),
        Event::Deliver(Phase::Value),
    ] {
        let world = World::new(41);
        world.enable_scheduler();
        let _scope = world.enter();
        let mut run = Run::new(&base, None);
        run.reach(&world, cut);
        let space = run.a.space.clone();
        let allocation = Rc::downgrade(
            &run.a
                .root
                .get(&key(2))
                .unwrap()
                .payload()
                .unwrap()
                .allocation,
        );
        drop(run.a);
        assert!(
            run.pool.private_fill().is_err(),
            "ring owns the abandoned buffer at {cut:?}"
        );
        assert!(allocation.upgrade().is_some());
        assert!(occupied(&space, Class::Payload, run.index));
        run.ring.simulated_crash();
        assert!(allocation.upgrade().is_none());
        assert!(!occupied(&space, Class::Payload, run.index));
        run.pool.assert_recovered();
        run.disk.crash(0);
        run.transcript
            .assert_bytes(&run.disk, &run.transcript.durable);
        let recovered = open(&run.disk, base.geometry);
        assert_eq!(slots(&recovered), [Some(3), Some(4)]);
        for checkpoint in recovered.checkpoints.iter().flatten() {
            assert_snapshot(
                &checkpoint.root,
                &run.transcript.durable,
                &run.model.snapshots[&checkpoint.generation],
            );
        }
        drop((recovered, space, run.ring));
        world.assert_clean();
    }
}
