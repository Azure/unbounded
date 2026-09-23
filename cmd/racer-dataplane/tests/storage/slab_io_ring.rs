// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::slab_io::Io;

fn disk(world: &crate::simulation::World, io: &Io) -> File {
    File::simulated(world.disk(crate::simulation::Disk::new(BUFFER_SIZE as u64)))
        .with_slab_io(io.clone())
}

fn metrics(io: &Io, name: &str) -> u64 {
    let mut text = String::new();
    io.render(&mut text);
    text.lines()
        .find_map(|line| {
            line.strip_prefix(&format!("racer_dataplane_slab_io_{name} "))?
                .parse()
                .ok()
        })
        .unwrap()
}

#[test]
fn shared_budget_parks_workers_and_preserves_network_capacity() {
    let world = crate::simulation::World::new(711);
    let _scope = world.enter();
    world.enable_scheduler();
    let io = Io::testing(1, 1, false);
    let mut a = crate::conformance::ring(1, Default::default());
    let mut b = crate::conformance::ring(1, Default::default());
    let mut first = a.sync_data(disk(&world, &io).into()).unwrap();
    let mut second = b.sync_data(disk(&world, &io).into()).unwrap();
    a.progress().unwrap();
    b.progress().unwrap();
    assert_eq!(metrics(&io, "operations_total"), 1);
    assert!(b.take_control(&mut second).unwrap().is_none());
    assert!(!b.progress().unwrap(), "token wait must allow parking");
    assert_eq!(
        b.slab_deadline(),
        Some(crate::environment::now() + Duration::from_secs(1))
    );
    // Ordinary non-slab work still uses the SQ while the slab queue is blocked.
    let mut nop: Ticket<Cancel> = b
        .enqueue(abi::Sqe::default(), Resource::None, None)
        .unwrap_or_else(|_| panic!("network reserve lost"));
    for _ in 0..8 {
        world.service_tick();
        b.progress().unwrap();
    }
    b.take_cancel(&mut nop).unwrap().unwrap().result.unwrap();
    assert_eq!(metrics(&io, "operations_total"), 1);
    world.advance(Duration::from_secs(1));
    for _ in 0..8 {
        world.service_tick();
        a.progress().unwrap();
        b.progress().unwrap();
    }
    a.take_control(&mut first).unwrap().unwrap().result.unwrap();
    b.take_control(&mut second)
        .unwrap()
        .unwrap()
        .result
        .unwrap();
    assert_eq!(metrics(&io, "operations_total"), 2);
    assert_eq!(metrics(&io, "waits_total"), 1);
    a.shutdown().unwrap();
    b.shutdown().unwrap();
    drop((a, b));
    world.assert_clean();
}

#[test]
fn queued_cancel_retains_fill_and_owner_without_charging_or_queue_growth() {
    let world = crate::simulation::World::new(712);
    let _scope = world.enter();
    world.enable_scheduler();
    let io = Io::testing(1, 1, false);
    io.reserve(0, crate::environment::now()).unwrap().finish(0);
    let mut ring = crate::conformance::ring(
        1,
        Config {
            requests: 8,
            progress_reserve: 2,
            ..Default::default()
        },
    );
    let file = disk(&world, &io);
    for _ in 0..32 {
        let owner = Rc::new(());
        let weak = Rc::downgrade(&owner);
        let mut read = ring
            .read(
                file.clone().into(),
                fill(ring.pool(), 1),
                BufferRange::new(0..16).unwrap(),
                FileOffset::new(0).unwrap(),
            )
            .unwrap();
        ring.retain(&read, owner.clone());
        drop(owner);
        let mut ack = ring.cancel(&read).unwrap();
        assert!(weak.upgrade().is_some());
        assert!(ring.pool().stage(Key::new([2; 32])).is_err());
        assert!(ring.slab_queue.is_empty());
        let done = ring.take_read(&mut read).unwrap().unwrap();
        assert_eq!(
            done.result.unwrap_err().raw_os_error(),
            Some(libc::ECANCELED)
        );
        drop(done.resource);
        assert!(weak.upgrade().is_none());
        for _ in 0..16 {
            world.service_tick();
            ring.progress().unwrap();
        }
        ring.take_cancel(&mut ack).unwrap().unwrap().result.unwrap();
    }
    assert_eq!(metrics(&io, "operations_total"), 1);
    let pending = ring
        .read(
            file.into(),
            fill(ring.pool(), 3),
            BufferRange::new(0..16).unwrap(),
            FileOffset::new(0).unwrap(),
        )
        .unwrap();
    ring.shutdown().unwrap();
    drop(pending);
    ring.pool().assert_recovered();
    drop(ring);
    world.assert_clean();
}

#[test]
fn bounded_queue_reserves_control_slots_and_shutdown_ignores_token_deadline() {
    let world = crate::simulation::World::new(713);
    let _scope = world.enter();
    world.enable_scheduler();
    let io = Io::testing(1, 1, false);
    io.reserve(0, crate::environment::now()).unwrap().finish(0);
    let mut ring = crate::conformance::ring(
        1,
        Config {
            requests: 8,
            progress_reserve: 2,
            ..Default::default()
        },
    );
    let file = disk(&world, &io);
    let mut tickets = Vec::new();
    for _ in 0..6 {
        tickets.push(ring.sync_data(file.clone().into()).unwrap());
    }
    assert_eq!(
        ring.sync_data(file.into()).unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
    assert_eq!(ring.slab_queue.len(), 6);
    let ack = ring.cancel(&tickets[0]).unwrap();
    drop((tickets, ack));
    let start = crate::environment::now();
    ring.shutdown().unwrap();
    assert!(crate::environment::now() - start < Duration::from_secs(1));
    drop(ring);
    world.assert_clean();
}

#[test]
fn splice_charges_input_only_and_refunds_short_completions() {
    let world = crate::simulation::World::new(714);
    let _scope = world.enter();
    world.enable_scheduler();
    world.short_transfers(7);
    let io = Io::testing(1, BUFFER_SIZE as u64, true);
    let mut ring = crate::conformance::ring(1, Default::default());
    let file = disk(&world, &io);
    let (read, write) = File::pipe().unwrap();
    let mut first = ring
        .splice(
            file,
            write.into(),
            Some(FileOffset::new(0).unwrap()),
            BUFFER_SIZE,
            Rc::new(()),
        )
        .unwrap();
    for _ in 0..8 {
        world.service_tick();
        ring.progress().unwrap();
    }
    let n = ring.take_splice(&mut first).unwrap().unwrap().unwrap();
    assert!(n > 0 && n < BUFFER_SIZE);
    assert_eq!(metrics(&io, "bytes_total"), n as u64);
    let charge = io
        .reserve(BUFFER_SIZE - n, crate::environment::now())
        .expect("unused requested bytes refunded");
    charge.finish(0);
    let (_other_read, other_write) = File::pipe().unwrap();
    let mut second = ring
        .splice(read, other_write.into(), None, n, Rc::new(()))
        .unwrap();
    assert!(ring.request(&second).unwrap().slab_pending.is_none());
    for _ in 0..8 {
        world.service_tick();
        ring.progress().unwrap();
    }
    ring.take_splice(&mut second).unwrap().unwrap().unwrap();
    assert_eq!(metrics(&io, "bytes_total"), n as u64);
    ring.shutdown().unwrap();
    drop((ring, _other_read));
    world.assert_clean();
}

pub(super) fn kernel_wait_and_cancel(ring: &mut Ring) {
    let name = std::ffi::CString::new("racer-slab-flow").unwrap();
    let raw = unsafe { libc::memfd_create(name.as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0);
    let io = Io::testing(10, 1, false);
    let file = File::new(unsafe { OwnedFd::from_raw_fd(raw) }).with_slab_io(io.clone());
    io.reserve(0, crate::environment::now()).unwrap().finish(0);
    let mut sync = ring.sync_data(file.clone().into()).unwrap();
    ring.progress().unwrap();
    assert!(ring.take_control(&mut sync).unwrap().is_none());
    let start = Instant::now();
    drive(ring, |r| {
        r.take_control(&mut sync).unwrap().is_some_and(|c| {
            c.result.unwrap();
            true
        })
    });
    assert!(start.elapsed() >= Duration::from_millis(50));
    let mut pending = ring.sync_data(file.into()).unwrap();
    let mut ack = ring.cancel(&pending).unwrap();
    assert_eq!(
        ring.take_control(&mut pending)
            .unwrap()
            .unwrap()
            .result
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ECANCELED)
    );
    drive(ring, |r| r.take_cancel(&mut ack).unwrap().is_some());
    assert_eq!(metrics(&io, "operations_total"), 2);
}
