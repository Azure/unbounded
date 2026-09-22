// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

pub(crate) fn stream_policies(world: &World) {
    let a = world.socket();
    let b = world.socket();
    {
        let mut s = world.0.borrow_mut();
        for (id, other) in [(a.id, b.id), (b.id, a.id)] {
            if let Object::Socket { peer, .. } = s.objects.get_mut(&id).unwrap() {
                *peer = Some(other);
            }
        }
    }
    let bytes = [1u8, 2, 3, 4];
    let send = |fd| unsafe {
        world
            .operation(26, fd, bytes.as_ptr() as u64, 4, 0, 0, 0)
            .unwrap()
            .0
    };
    let recv = |fd, out: &mut [u8; 4]| unsafe {
        world
            .operation(27, fd, out.as_mut_ptr() as u64, 4, 0, 0, 0)
            .unwrap()
            .0
    };
    assert_eq!(send(a.id), 4);
    a.shutdown_write();
    assert!(world.operation_ready(26, a.id));
    assert_eq!(send(a.id), -libc::EPIPE);
    let mut out = [0; 4];
    assert_eq!(recv(b.id, &mut out), 4);
    assert_eq!(out, bytes, "FIN must follow queued bytes");
    assert!(world.operation_ready(27, b.id));
    assert_eq!(recv(b.id, &mut out), 0);
    assert_eq!(send(b.id), 4, "reverse direction survives FIN");
    assert_eq!(recv(a.id, &mut out), 4);
    assert_eq!(out, bytes);

    // A rejected splice must leave its source queue and page references intact.
    let disk = Disk::new(4096);
    disk.write_all_at(&bytes, 0).unwrap();
    let disk = world.disk(disk);
    let (reader, writer) = world.pipe();
    assert_eq!(
        unsafe { world.operation(30, writer.id, 0, 4, 0, 0, disk.id) }
            .unwrap()
            .0,
        4
    );
    assert_eq!(
        unsafe { world.operation(30, a.id, 0, 4, 0, 0, reader.id) }
            .unwrap()
            .0,
        -libc::EPIPE
    );
    world.observation(history::Transition::StreamPolicyChecked {
        policy: "half-close".into(),
    });

    assert_eq!(send(b.id), 4, "queue bytes that the reset will discard");
    b.reset();
    for fd in [a.id, b.id] {
        assert!(world.operation_ready(27, fd));
        assert!(world.operation_ready(26, fd));
        assert_eq!(recv(fd, &mut out), -libc::ECONNRESET);
        assert_eq!(send(fd), -libc::EPIPE);
        assert_eq!(
            unsafe { world.operation(30, fd, 0, 4, 0, 0, reader.id) }
                .unwrap()
                .0,
            -libc::EPIPE
        );
    }
    let s = world.0.borrow();
    let Object::Pipe(queue) = s.objects.get(&reader.id).unwrap() else {
        unreachable!()
    };
    let mut preserved = [0; 4];
    assert_eq!(queue.borrow().len(), 4);
    queue.borrow_mut().read(&mut preserved);
    assert_eq!(preserved, bytes);
    drop(s);
    world.observation(history::Transition::StreamPolicyChecked {
        policy: "reset".into(),
    });
}

mod tests {
    #[test]
    fn ready_callback_batches_are_fair_replayable_and_incarnation_fenced() {
        fn run(replay: Option<Vec<super::Choice>>) -> (Vec<usize>, Vec<super::Choice>) {
            use super::*;
            let world = World::new(19);
            let _scope = world.enter();
            world.enable_scheduler();
            world.callback_policy(CallbackPolicy::ReadyBatch);
            if let Some(choices) = replay {
                world.replay(choices);
            }
            let seen = Rc::new(RefCell::new(Vec::new()));
            for id in 0..65 {
                let _process = world.scoped_node(Some(0));
                let seen = seen.clone();
                let next = world.clone();
                world.schedule(move || {
                    seen.borrow_mut().push(id);
                    if id == 0 {
                        let seen = seen.clone();
                        next.schedule(move || seen.borrow_mut().push(100));
                        next.advance(Duration::from_secs(1));
                        next.run_tasks(); // Must not recursively overtake this batch.
                    }
                });
            }
            {
                let _process = world.scoped_node(Some(1));
                world.schedule(|| panic!("retired callback ran"));
            }
            world.restart_node(Some(1));
            let initial: BTreeSet<_> = world
                .0
                .borrow()
                .tasks
                .keys()
                .take(64)
                .map(|k| k.1 as usize)
                .collect();
            world.run_tasks();
            assert!(seen.borrow().is_empty(), "callback ran before its due time");
            world.advance(Duration::from_secs(1));
            world.run_tasks();
            let observed = seen.borrow().clone();
            assert_eq!(observed.len(), 66);
            assert_eq!(
                observed[..64].iter().copied().collect::<BTreeSet<_>>(),
                initial
            );
            assert_eq!(observed.iter().copied().collect::<BTreeSet<_>>().len(), 66);
            world.assert_replay_consumed();
            (observed, world.choices())
        }
        let (observed, choices) = run(None);
        assert_eq!(run(Some(choices)).0, observed);
    }
    use super::*;
    use crate::{
        buffers,
        uring::{self, Application, Work},
        workers::{Driver as _, Wake as _},
    };
    fn raw(world: &World) -> SimRing {
        SimRing::new(world.clone(), 8)
    }
    fn poll_mask(mask: i16) -> u32 {
        let mask = mask as u32;
        if cfg!(target_endian = "big") {
            mask.rotate_left(16)
        } else {
            mask
        }
    }
    fn poll_pair(world: &World) -> (Handle, Handle) {
        let a = world.socket();
        let b = world.socket();
        let mut s = world.0.borrow_mut();
        for (id, other) in [(a.id, b.id), (b.id, a.id)] {
            let Object::Socket { peer, .. } = s.objects.get_mut(&id).unwrap() else {
                unreachable!()
            };
            *peer = Some(other);
        }
        (a, b)
    }
    #[test]
    fn poll_add_bounded_socket_wakes_after_receive_and_delays_completion() {
        let world = World::new(19);
        world.enable_scheduler();
        world.socket_capacity(4);
        let (a, b) = poll_pair(&world);
        let bytes = [1u8, 2, 3, 4];
        let send = || unsafe { world.operation(26, a.id, bytes.as_ptr() as u64, 4, 0, 0, 0) };
        assert_eq!(send().unwrap().0, 4);
        let disk = Disk::new(512);
        disk.write_all_at(&bytes, 0).unwrap();
        let file = world.disk(disk);
        let (read, write) = world.pipe();
        assert_eq!(
            unsafe { world.operation(30, write.id, 0, 4, 0, 0, file.id) }
                .unwrap()
                .0,
            4
        );
        assert_eq!(
            unsafe { world.operation(30, a.id, 0, 4, 0, 0, read.id) }
                .unwrap()
                .0,
            -libc::EAGAIN
        );
        let mut ring = raw(&world);
        *ring.fixed.borrow_mut() = vec![a.id];
        ring.staged.push((
            Sqe {
                opcode: 6,
                flags: 1, // Production HTTP polls a fixed socket descriptor.
                fd: 0,
                op_flags: poll_mask(libc::POLLOUT),
                user_data: 2,
                ..Default::default()
            },
            1,
        ));
        assert_eq!(ring.next_tick(), None);
        world.service_tick();
        ring.enter();
        assert_eq!(ring.pending.len(), 1);
        assert!(ring.completions.is_empty());
        assert!(ring.cq.is_empty());
        assert_eq!(world.counts()[6], 0);
        assert!(
            unsafe { world.operation(6, a.id, 0, 0, 0, poll_mask(libc::POLLOUT), 0) }.is_none()
        );
        assert_eq!(
            unsafe { world.operation(6, b.id, 0, 0, 0, poll_mask(libc::POLLIN), 0) }
                .unwrap()
                .0,
            i32::from(libc::POLLIN)
        );
        let mut out = [0; 4];
        assert_eq!(
            unsafe { world.operation(27, b.id, out.as_mut_ptr() as u64, 4, 0, 0, 0) }
                .unwrap()
                .0,
            4
        );
        assert_eq!(out, bytes, "poll did not consume queued bytes");
        assert_eq!(ring.next_tick(), Some(1));
        ring.enter();
        assert!(ring.pending.is_empty());
        assert!(ring.cq.is_empty(), "effect is separate from CQ delivery");
        assert_eq!(ring.completions.len(), 1);
        assert_eq!(ring.completions[0].0.res, i32::from(libc::POLLOUT));
        assert_eq!(world.counts()[6], 1);
        // Retry the blocked splice; readiness can disappear before CQ delivery.
        assert_eq!(
            unsafe { world.operation(30, a.id, 0, 4, 0, 0, read.id) }
                .unwrap()
                .0,
            4
        );
        assert!(
            unsafe { world.operation(6, a.id, 0, 0, 0, poll_mask(libc::POLLOUT), 0) }.is_none()
        );
        world.advance(Duration::from_millis(3));
        ring.enter();
        assert_eq!(ring.cq.len(), 1);
        assert_eq!(ring.cq[0].user_data, 2);
        assert_eq!(ring.cq[0].res, i32::from(libc::POLLOUT));
        assert_eq!(ring.cq[0].flags, 0);
        assert_eq!(world.counts()[6], 1, "one-shot poll must not reexecute");
        drop((ring, file, read, write, a, b));
        world.assert_clean();
    }
    #[test]
    fn poll_add_pipe_pressure_wakes_after_splice_drain() {
        let world = World::new(19);
        world.enable_scheduler();
        world.short_transfers(65536);
        let disk = Disk::new(65536);
        disk.write_all_at(&vec![7; 65536], 0).unwrap();
        let file = world.disk(disk);
        let (read, write) = world.pipe();
        let (a, b) = poll_pair(&world);
        let poll = |fd, mask| unsafe {
            world
                .operation(6, fd, 0, 0, 0, poll_mask(mask), 0)
                .map(|r| r.0)
        };
        assert_eq!(poll(read.id, libc::POLLIN), None);
        assert_eq!(
            poll(write.id, libc::POLLOUT),
            Some(i32::from(libc::POLLOUT))
        );
        assert_eq!(
            unsafe { world.operation(30, write.id, 0, 65536, 0, 0, file.id) }
                .unwrap()
                .0,
            65536
        );
        assert_eq!(
            unsafe { world.operation(30, write.id, 0, 1, 0, 0, file.id) }
                .unwrap()
                .0,
            -libc::EAGAIN
        );
        assert_eq!(poll(read.id, libc::POLLIN), Some(i32::from(libc::POLLIN)));
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                opcode: 6,
                fd: write.id,
                op_flags: poll_mask(libc::POLLOUT),
                user_data: 2,
                ..Default::default()
            },
            1,
        ));
        world.service_tick();
        ring.enter();
        assert_eq!(ring.next_tick(), None);
        assert_eq!(ring.pending.len(), 1);
        assert_eq!(
            unsafe { world.operation(30, a.id, 0, 512, 0, 0, read.id) }
                .unwrap()
                .0,
            512
        );
        assert_eq!(ring.next_tick(), Some(1));
        ring.enter();
        assert!(ring.pending.is_empty());
        assert!(ring.cq.is_empty());
        world.advance(Duration::from_millis(3));
        ring.enter();
        assert_eq!(ring.cq.len(), 1);
        assert_eq!(ring.cq[0].res, i32::from(libc::POLLOUT));
        let mut out = [0u8; 512];
        assert_eq!(
            unsafe { world.operation(27, b.id, out.as_mut_ptr() as u64, 512, 0, 0, 0) }
                .unwrap()
                .0,
            512
        );
        assert_eq!(out, [7; 512]);
        drop((ring, file, read, write, a, b));
        world.assert_clean();
    }
    #[test]
    fn poll_add_fin_reset_and_close_report_directional_terminal_readiness() {
        let world = World::new(19);
        world.enable_scheduler();
        world.socket_capacity(4);
        let (a, b) = poll_pair(&world);
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                opcode: 6,
                fd: b.id,
                op_flags: poll_mask(libc::POLLIN | libc::POLLRDHUP),
                user_data: 2,
                ..Default::default()
            },
            1,
        ));
        world.service_tick();
        ring.enter();
        assert_eq!(
            ring.next_tick(),
            None,
            "writability must not wake a read poll"
        );
        assert_eq!(ring.pending.len(), 1);
        let poll = |fd, mask| unsafe {
            world
                .operation(6, fd, 0, 0, 0, poll_mask(mask), 0)
                .map(|r| r.0)
        };
        assert_eq!(poll(b.id, libc::POLLIN), None);
        let bytes = [5u8; 4];
        assert_eq!(
            unsafe { world.operation(26, a.id, bytes.as_ptr() as u64, 4, 0, 0, 0) }
                .unwrap()
                .0,
            4
        );
        a.shutdown_write();
        assert_eq!(ring.next_tick(), Some(1));
        ring.enter();
        assert!(ring.pending.is_empty());
        assert!(ring.cq.is_empty());
        assert_eq!(
            ring.completions[0].0.res,
            i32::from(libc::POLLIN | libc::POLLRDHUP)
        );
        assert_eq!(poll(b.id, libc::POLLIN), Some(i32::from(libc::POLLIN)));
        assert_eq!(
            poll(b.id, libc::POLLRDHUP),
            Some(i32::from(libc::POLLRDHUP))
        );
        assert_eq!(
            poll(b.id, libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP),
            Some(i32::from(libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP))
        );
        assert_eq!(poll(b.id, 0), None, "FIN alone is not full HUP");
        assert_eq!(
            poll(a.id, libc::POLLIN),
            None,
            "reverse stream remains open"
        );
        assert_eq!(
            poll(a.id, libc::POLLOUT),
            Some(i32::from(libc::POLLOUT)),
            "write shutdown wakes a writer to observe EPIPE despite full queue"
        );
        let mut out = [0; 4];
        assert_eq!(
            unsafe { world.operation(27, b.id, out.as_mut_ptr() as u64, 4, 0, 0, 0) }
                .unwrap()
                .0,
            4
        );
        assert_eq!(out, bytes);
        assert_eq!(poll(b.id, libc::POLLIN), Some(i32::from(libc::POLLIN)));
        assert_eq!(
            unsafe { world.operation(27, b.id, out.as_mut_ptr() as u64, 4, 0, 0, 0) }
                .unwrap()
                .0,
            0
        );
        b.shutdown_write();
        for fd in [a.id, b.id] {
            assert_eq!(poll(fd, 0), Some(i32::from(libc::POLLHUP)));
        }
        b.reset();
        for fd in [a.id, b.id] {
            let terminal = libc::POLLHUP | libc::POLLERR;
            assert_eq!(poll(fd, 0), Some(i32::from(terminal)));
            assert_eq!(
                poll(fd, libc::POLLOUT),
                Some(i32::from(terminal | libc::POLLOUT))
            );
            assert_eq!(
                poll(fd, libc::POLLIN),
                Some(i32::from(terminal | libc::POLLIN))
            );
        }
        drop((a, b));
        let (a, b) = poll_pair(&world);
        a.shutdown();
        assert_eq!(poll(b.id, 0), Some(i32::from(libc::POLLHUP)));
        drop(a);
        assert_eq!(
            poll(b.id, libc::POLLOUT),
            Some(i32::from(libc::POLLHUP | libc::POLLOUT))
        );
        drop(b);
        world.advance(Duration::from_millis(3));
        ring.enter();
        assert_eq!(ring.cq.len(), 1);
        assert_eq!(ring.cq[0].res, i32::from(libc::POLLIN | libc::POLLRDHUP));
        drop(ring);
        world.assert_clean();
    }
    #[test]
    fn poll_add_rejects_unsupported_requests_and_cancels_pending_poll() {
        let world = World::new(19);
        world.enable_scheduler();
        let (a, b) = poll_pair(&world);
        for (fd, len, mask, expected) in [
            (a.id, 1, libc::POLLIN, -libc::EINVAL),
            (a.id, 0, libc::POLLPRI, -libc::EINVAL),
            (-1, 0, libc::POLLIN, -libc::EBADF),
        ] {
            assert_eq!(
                unsafe { world.operation(6, fd, 0, len, 0, poll_mask(mask), 0) }
                    .unwrap()
                    .0,
                expected
            );
        }
        let file = world.disk(Disk::new(512));
        assert_eq!(
            unsafe { world.operation(6, file.id, 0, 0, 0, poll_mask(libc::POLLIN), 0) }
                .unwrap()
                .0,
            -libc::EOPNOTSUPP
        );
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                opcode: 6,
                fd: a.id,
                op_flags: poll_mask(libc::POLLIN),
                user_data: 2,
                ..Default::default()
            },
            1,
        ));
        world.service_tick();
        ring.enter();
        assert_eq!(ring.next_tick(), None);
        ring.staged.push((
            Sqe {
                opcode: 14,
                addr: 2,
                user_data: 3,
                ..Default::default()
            },
            1,
        ));
        ring.enter();
        assert!(ring.pending.is_empty());
        assert!(ring.cq.is_empty());
        world.advance(Duration::from_millis(3));
        ring.enter();
        assert_eq!(ring.cq.len(), 2);
        assert!(
            ring.cq
                .iter()
                .any(|c| c.user_data == 2 && c.res == -libc::ECANCELED)
        );
        assert!(ring.cq.iter().any(|c| c.user_data == 3 && c.res == 0));
        assert_eq!(world.counts()[6], 0);
        drop((ring, file, a, b));
        world.assert_clean();
    }
    #[test]
    fn stream_half_close_and_reset_preserve_direction_and_source_ownership() {
        let world = World::new(19);
        world.enable_scheduler();
        stream_policies(&world);
        world.assert_clean();
    }
    #[test]
    fn wall_steps_leave_monotonic_time_and_other_nodes_unchanged() {
        let world = World::new(19);
        world.enable_scheduler();
        world.node(Some(0));
        let now = world.now();
        let wall = world.wall();
        world.wall_offset(Some(0), -5000);
        assert_eq!(world.now(), now);
        assert_eq!(world.wall(), wall - Duration::from_secs(5));
        world.node(Some(1));
        assert_eq!(world.wall(), wall);
        world.advance(Duration::from_secs(1));
        assert_eq!(world.now(), now + Duration::from_secs(1));
        world.node(Some(0));
        world.wall_offset(Some(0), 7000);
        assert_eq!(world.wall(), wall + Duration::from_secs(8));
    }
    #[test]
    fn persist_sectors_propagates_nonprefix_overwrites_and_holes_while_sync_held() {
        let world = World::new(19);
        let disk = Disk::new(8192);
        disk.write_all_at(&[1; 8192], 0).unwrap();
        disk.sync_data().unwrap();
        disk.write_all_at(&[2; 4096], 0).unwrap();
        disk.write_all_at(&[3; 256], 3 * 512 + 128).unwrap();
        disk.punch(4096, 4096);
        disk.write_all_at(&[4; 512], 10 * 512).unwrap();
        disk.hold_sync(true);
        disk.fail_after_effect(3);
        let file = world.disk(disk.clone());
        let mut live = [0; 8192];
        disk.read_exact_at(&mut live, 0).unwrap();
        let page = disk.0.lock().unwrap().live[&3].clone();

        disk.persist_sectors(&[15, 3, 9, 10]).unwrap();

        let mut actual = [0; 8192];
        disk.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(actual, live);
        assert!(Arc::ptr_eq(&page, &disk.0.lock().unwrap().live[&3]));
        assert_eq!(
            disk.dirty_sectors(),
            vec![0, 1, 2, 4, 5, 6, 7, 8, 11, 12, 13, 14]
        );
        assert!(disk.sync_held());
        assert!(!world.operation_ready(3, file.id));
        assert!(unsafe { world.operation(3, file.id, 0, 0, 0, 0, 0) }.is_none());
        assert!(!disk.completion_fault_fired());
        {
            let d = disk.0.lock().unwrap();
            assert!(!d.durable.contains_key(&9));
            assert!(!d.durable.contains_key(&15));
        }
        let durable = disk.digest();
        disk.persist_sectors(&[]).unwrap();
        disk.persist_sectors(&[3, 9, 10, 15]).unwrap();
        assert_eq!(disk.digest(), durable);
        disk.crash(0);
        disk.read_exact_at(&mut actual, 0).unwrap();
        let mut expected = [1; 8192];
        expected[3 * 512..4 * 512].copy_from_slice(&live[3 * 512..4 * 512]);
        expected[9 * 512..10 * 512].fill(0);
        expected[10 * 512..11 * 512].fill(4);
        expected[15 * 512..].fill(0);
        assert_eq!(actual, expected);
        drop(file);
        world.assert_clean();
    }
    #[test]
    fn persist_sectors_validation_and_armed_conflicts_are_atomic() {
        let disk = Disk::new(1025);
        disk.write_all_at(&[1; 1025], 0).unwrap();
        disk.sync_data().unwrap();
        disk.track_versions(8).unwrap();
        disk.write_all_at(&[2; 1025], 0).unwrap();
        disk.write_all_at(&[3; 512], 0).unwrap();
        disk.hold_sync(true);
        disk.fail_after_effect(3);
        let snapshot = || {
            let d = disk.0.lock().unwrap();
            (
                d.live
                    .iter()
                    .map(|(sector, bytes)| (*sector, *bytes.lock().unwrap()))
                    .collect::<BTreeMap<_, _>>(),
                d.durable.clone(),
                d.dirty.clone(),
                d.versions.clone(),
                d.version_count,
                d.crash_selection.clone(),
                d.crash_versions.clone(),
                d.hold_sync,
                d.completion_fault,
            )
        };
        let before = snapshot();
        for invalid in [vec![0, 1, 0], vec![0, 3], vec![2, u64::MAX]] {
            assert_eq!(
                disk.persist_sectors(&invalid).unwrap_err().kind(),
                io::ErrorKind::InvalidInput
            );
            assert_eq!(snapshot(), before);
        }
        // Even empty armed policies conflict, including an empty propagation.
        for versions in [false, true] {
            for empty in [false, true] {
                if versions {
                    disk.select_crash_versions(if empty { vec![] } else { vec![(0, 1)] })
                        .unwrap();
                } else {
                    disk.select_crash_sectors(if empty { vec![] } else { vec![1] });
                }
                let armed = snapshot();
                for selection in [&[][..], &[0, 2][..]] {
                    let error = disk.persist_sectors(selection).unwrap_err();
                    assert!(error.to_string().contains("infrastructure:"));
                    assert_eq!(snapshot(), armed);
                }
                // Disarm only after verifying the complete policy was retained.
                let mut d = disk.0.lock().unwrap();
                d.crash_selection = None;
                d.crash_versions = None;
            }
        }
        assert_eq!(snapshot(), before);
        disk.persist_sectors(&[2]).unwrap();
        assert_eq!(disk.dirty_sectors(), vec![0, 1]);
        assert_eq!(disk.pending_versions(), vec![(0, 2), (1, 1)]);
        disk.crash_versions(&[]).unwrap();
        let mut actual = [0; 1025];
        disk.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(&actual[..1024], &[1; 1024]);
        assert_eq!(actual[1024], 2, "the final partial sector is in range");

        let empty = Disk::new(0);
        empty.persist_sectors(&[]).unwrap();
        assert_eq!(
            empty.persist_sectors(&[0]).unwrap_err().kind(),
            io::ErrorKind::InvalidInput
        );
    }
    #[test]
    fn persist_sectors_reclaims_exact_version_budget_and_rebases_selected_history() {
        let disk = Disk::new(1024);
        disk.write_all_at(&[1; 1024], 0).unwrap();
        disk.sync_data().unwrap();
        let durable = disk.digest();
        disk.track_versions(6).unwrap();
        for value in 2..=3 {
            disk.write_all_at(&[value; 1024], 0).unwrap();
        }
        for value in [4, 1] {
            disk.write_all_at(&[value; 512], 0).unwrap();
        }
        assert!(disk.write_all_at(&[9; 512], 0).is_err());
        disk.persist_sectors(&[0]).unwrap();
        assert_eq!(disk.digest(), durable, "latest bytes equal the old floor");
        assert_eq!(disk.dirty_sectors(), vec![1]);
        assert_eq!(disk.pending_versions(), vec![(1, 2)]);
        assert_eq!(disk.0.lock().unwrap().version_count, 2);
        assert!(disk.select_crash_versions(vec![(0, 1)]).is_err());
        disk.persist_sectors(&[0]).unwrap();
        disk.persist_sectors(&[]).unwrap();
        assert_eq!(disk.0.lock().unwrap().version_count, 2);
        for value in 6..=7 {
            disk.write_all_at(&[value; 1024], 0).unwrap();
        }
        assert_eq!(disk.pending_versions(), vec![(0, 2), (1, 4)]);
        assert_eq!(disk.0.lock().unwrap().version_count, 6);
        assert!(disk.write_all_at(&[9; 512], 0).is_err());
        disk.crash_versions(&[(0, 1), (1, 1)]).unwrap();
        let mut actual = [0; 1024];
        disk.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(&actual[..512], &[6; 512], "selected history rebases");
        assert_eq!(
            &actual[512..],
            &[2; 512],
            "unselected history retains indices"
        );
    }
    #[test]
    fn persist_sectors_version_choices_respect_byte_and_hole_floors() {
        for hole in [false, true] {
            for armed in [false, true] {
                for selected in 0..=2 {
                    for other in 0..=2 {
                        let disk = Disk::new(8192);
                        disk.write_all_at(&[1; 8192], 0).unwrap();
                        disk.sync_data().unwrap();
                        disk.track_versions(16).unwrap();
                        disk.write_all_at(&[2; 512], 0).unwrap();
                        disk.write_all_at(&[3; 512], 0).unwrap();
                        disk.write_all_at(&[4; 512], 4096).unwrap();
                        disk.write_all_at(&[5; 512], 4096).unwrap();
                        if hole {
                            disk.punch(0, 4096);
                        }
                        disk.persist_sectors(&[0]).unwrap();
                        assert!(!disk.pending_versions().iter().any(|(s, _)| *s == 0));
                        assert_eq!(
                            disk.0.lock().unwrap().version_count,
                            if hole { 9 } else { 2 }
                        );
                        assert!(disk.select_crash_versions(vec![(0, 1)]).is_err());
                        disk.write_all_at(&[6; 128], 0).unwrap();
                        disk.write_all_at(&[7; 128], 128).unwrap();
                        let selection = [(0, selected), (8, other)];
                        if armed {
                            disk.select_crash_versions(selection.to_vec()).unwrap();
                            disk.write_all_at(&[9; 512], 0).unwrap();
                            disk.crash(usize::MAX);
                        } else {
                            disk.crash_versions(&selection).unwrap();
                        }
                        let mut expected = [1; 8192];
                        expected[..512].fill(if hole { 0 } else { 3 });
                        if selected >= 1 {
                            expected[..128].fill(6);
                        }
                        if selected == 2 {
                            expected[128..256].fill(7);
                        }
                        expected[4096..4608].fill([1, 4, 5][other]);
                        let mut actual = [0; 8192];
                        disk.read_exact_at(&mut actual, 0).unwrap();
                        assert_eq!(actual, expected);
                        assert!(disk.pending_versions().is_empty());
                        assert_eq!(disk.0.lock().unwrap().version_count, 0);
                    }
                }
            }
        }
    }
    #[test]
    fn pending_versions_preserve_ordered_overlaps_holes_and_sync_floor() {
        let disk = Disk::new(8192);
        disk.write_all_at(&[1; 8192], 0).unwrap();
        disk.sync_data().unwrap();
        disk.track_versions(64).unwrap();
        disk.write_all_at(&[2; 1024], 0).unwrap();
        disk.write_all_at(&[3; 512], 256).unwrap();
        disk.punch(4096, 4096);
        disk.write_all_at(&[4; 512], 4096).unwrap();
        // Earlier write on sector zero, cumulative partial write on sector one,
        // and an earlier hole despite a later write to that same sector.
        disk.crash_versions(&[(0, 1), (1, 2), (8, 1)]).unwrap();
        let mut actual = [0; 8192];
        disk.read_exact_at(&mut actual, 0).unwrap();
        let mut expected = [1; 8192];
        expected[..1024].fill(2);
        expected[512..768].fill(3);
        expected[4096..4608].fill(0);
        assert_eq!(actual, expected);
        disk.write_all_at(&[5; 512], 0).unwrap();
        disk.sync_data().unwrap();
        disk.write_all_at(&[6; 512], 0).unwrap();
        disk.crash_versions(&[(0, 0)]).unwrap();
        disk.read_exact_at(&mut actual, 0).unwrap();
        expected[..512].fill(5);
        assert_eq!(actual, expected, "sync is a floor, not a pending version");
        assert!(disk.0.lock().unwrap().versions.is_empty());
    }
    #[test]
    fn pending_versions_enumerate_each_sector_prefix_independently() {
        for a in 0..=3 {
            for b in 0..=3 {
                let disk = Disk::new(1024);
                disk.write_all_at(&[1; 1024], 0).unwrap();
                disk.sync_data().unwrap();
                disk.track_versions(6).unwrap();
                for value in 2..=4 {
                    disk.write_all_at(&[value; 1024], 0).unwrap();
                }
                disk.crash_versions(&[(0, a), (1, b)]).unwrap();
                let mut actual = [0; 1024];
                disk.read_exact_at(&mut actual, 0).unwrap();
                assert_eq!(&actual[..512], &[a as u8 + 1; 512]);
                assert_eq!(&actual[512..], &[b as u8 + 1; 512]);
            }
        }
    }
    #[test]
    fn armed_version_crash_defers_effect_and_rejects_intervening_sync() {
        let disk = Disk::new(512);
        disk.write_all_at(&[1; 512], 0).unwrap();
        disk.sync_data().unwrap();
        disk.track_versions(3).unwrap();
        disk.write_all_at(&[2; 512], 0).unwrap();
        disk.write_all_at(&[3; 512], 0).unwrap();
        assert_eq!(disk.pending_versions(), vec![(0, 2)]);
        disk.select_crash_versions(vec![(0, 1)]).unwrap();
        let mut bytes = [0; 512];
        disk.read_exact_at(&mut bytes, 0).unwrap();
        assert_eq!(bytes, [3; 512], "arming must not crash live IO");
        assert!(disk.select_crash_versions(vec![(0, 2)]).is_err());
        assert!(
            disk.sync_data()
                .unwrap_err()
                .to_string()
                .contains("infrastructure:")
        );
        disk.write_all_at(&[4; 512], 0).unwrap();
        disk.crash(usize::MAX);
        disk.read_exact_at(&mut bytes, 0).unwrap();
        assert_eq!(
            bytes, [2; 512],
            "later writes cannot change the selected prefix"
        );
        assert!(disk.pending_versions().is_empty());
        disk.sync_data().unwrap();
    }
    #[test]
    fn pending_version_validation_and_budget_reject_before_mutation() {
        let disk = Disk::new(8192);
        assert!(disk.crash_versions(&[]).is_err());
        assert!(disk.track_versions(0).is_err());
        disk.track_versions(2).unwrap();
        disk.write_all_at(&[7; 1024], 0).unwrap();
        let digest = disk.digest();
        for invalid in [vec![(0, 2)], vec![(16, 0)], vec![(0, 1), (0, 0)]] {
            assert_eq!(
                disk.crash_versions(&invalid).unwrap_err().kind(),
                io::ErrorKind::InvalidInput
            );
            assert_eq!(disk.digest(), digest);
        }
        let error = disk.write_all_at(&[8; 1024], 0).unwrap_err();
        assert!(error.to_string().contains("infrastructure:"));
        assert_eq!(disk.digest(), digest);
        assert!(disk.pages(4096, 1024).is_err());
        assert_eq!(disk.digest(), digest);
        disk.sync_data().unwrap();
        disk.write_all_at(&[9; 512], 0).unwrap();
        disk.crash_versions(&[(0, 1)]).unwrap();
        let mut actual = [0; 1024];
        disk.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(&actual[..512], &[9; 512]);
        assert_eq!(&actual[512..], &[7; 512]);
        disk.write_all_at(&[10; 512], 0).unwrap();
        disk.select_crash_sectors(vec![]);
        assert!(disk.crash_versions(&[(0, 1)]).is_err());
        disk.crash(0);
        assert!(disk.0.lock().unwrap().versions.is_empty());
    }
    #[test]
    fn crash_subset_preserves_barriers_and_nonprefix_holes() {
        let disk = Disk::new(8192);
        disk.write_all_at(&[1; 8192], 0).unwrap();
        disk.sync_data().unwrap();
        disk.write_all_at(&[2; 4096], 0).unwrap();
        disk.punch(4096, 4096);
        disk.select_crash_sectors(vec![3, 9, 15]);
        disk.crash(usize::MAX);
        let mut actual = [0; 8192];
        disk.read_exact_at(&mut actual, 0).unwrap();
        let mut expected = [1; 8192];
        expected[3 * 512..4 * 512].fill(2);
        expected[9 * 512..10 * 512].fill(0);
        expected[15 * 512..16 * 512].fill(0);
        assert_eq!(actual, expected);
        disk.write_all_at(&[4; 512], 0).unwrap();
        disk.sync_data().unwrap();
        disk.select_crash_sectors(vec![]);
        disk.crash(0);
        disk.read_exact_at(&mut actual, 0).unwrap();
        expected[..512].fill(4);
        assert_eq!(actual, expected);
    }
    #[test]
    fn bounded_socket_directions_backpressure_without_consuming_bytes() {
        let world = World::new(19);
        world.socket_capacity(4);
        let a = world.socket();
        let b = world.socket();
        {
            let mut s = world.0.borrow_mut();
            if let Object::Socket { peer, .. } = s.objects.get_mut(&a.id).unwrap() {
                *peer = Some(b.id);
            }
            if let Object::Socket { peer, .. } = s.objects.get_mut(&b.id).unwrap() {
                *peer = Some(a.id);
            }
        }
        let bytes = [1u8, 2, 3, 4, 5, 6];
        let send = |fd| unsafe { world.operation(26, fd, bytes.as_ptr() as u64, 6, 0, 0, 0) };
        assert_eq!(send(a.id).unwrap().0, 4);
        assert!(!world.operation_ready(26, a.id));
        assert!(send(a.id).is_none());
        assert_eq!(send(b.id).unwrap().0, 4, "reverse direction stays writable");
        let mut out = [0u8; 4];
        let read = unsafe { world.operation(27, b.id, out.as_mut_ptr() as u64, 4, 0, 0, 0) };
        assert_eq!(read.unwrap().0, 4);
        assert_eq!(out, [1, 2, 3, 4]);
        assert!(world.operation_ready(26, a.id));
        assert_eq!(send(a.id).unwrap().0, 4);
    }
    #[test]
    fn invalid_action_nodes_are_rejected_without_panicking() {
        use corpus::{Action, get, valid};
        for action in [
            Action::Get(get(2, "/x")),
            Action::Cancel(2),
            Action::Hold(0, 2, "/x".into()),
        ] {
            assert!(!valid(&[action], 2));
        }
        assert!(!valid(&[], 0));
        assert!(!valid(&[Action::WallOffset(0, i64::MIN)], 2));
    }
    #[test]
    fn process_seeded_multishard_pressure_and_expiry() {
        let run = |seed| {
            let world = World::new(seed);
            world.enable_scheduler();
            world.node(Some(3));
            world.configure_replay(crate::http_auth::replay::Config {
                capacity: 32,
                shards: 8,
            });
            let mut outcomes = Vec::new();
            for value in 0..128u64 {
                let nonce = *blake3::hash(&value.to_le_bytes()).as_bytes();
                outcomes.push(world.accept_nonce(nonce).map_err(|e| e.kind()));
            }
            assert_eq!(outcomes.iter().filter(|r| r.is_ok()).count(), 32);
            world.advance(Duration::from_secs(3600));
            for value in 0..128u64 {
                outcomes.push(
                    world
                        .accept_nonce(*blake3::hash(&value.to_le_bytes()).as_bytes())
                        .map_err(|e| e.kind()),
                );
            }
            outcomes
        };
        assert_eq!(run(19), run(19));
        assert_ne!(run(19), run(71));
    }
    #[test]
    fn process_entropy_is_independent_of_other_nodes_and_restarts() {
        let a = World::new(41);
        let b = World::new(41);
        a.enable_scheduler();
        b.enable_scheduler();
        let mut first = [0; 32];
        let mut second = [0; 32];
        a.node(Some(1023));
        a.random(&mut first);
        a.node(Some(2));
        a.random(&mut [0; 97]);
        a.restart_node(Some(2));
        a.random(&mut [0; 15]);
        b.node(Some(1023));
        b.random(&mut second);
        assert_eq!(first, second);
        a.node(Some(1023));
        a.random(&mut first);
        b.random(&mut second);
        assert_eq!(first, second);
        a.restart_node(Some(1023));
        a.random(&mut first);
        assert_ne!(first, second);
    }
    #[test]
    fn strict_replay_rejects_same_count_different_enabled_events() {
        let a = World::new(43);
        a.enable_scheduler();
        assert_eq!(a.choose_enabled("only-effect", &[91]), 0);
        assert_eq!(a.choice_count(), 0);
        let selected = a.choose_enabled("worker", &[2, 4, 8]);
        let prefix = a.choices();
        let b = World::new(43);
        b.enable_scheduler();
        b.replay(prefix.clone());
        b.choose_enabled("only-effect", &[91]);
        assert_eq!(b.choose_enabled("worker", &[2, 4, 8]), selected);
        let c = World::new(43);
        c.enable_scheduler();
        c.replay(prefix.clone());
        c.choose_enabled("only-effect", &[91]);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                c.choose_enabled("worker", &[2, 5, 8]);
            }))
            .is_err()
        );
        let d = World::new(43);
        d.enable_scheduler();
        d.replay(prefix);
        d.choose_enabled("only-effect", &[92]);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                d.choose_enabled("worker", &[2, 4, 8]);
            }))
            .is_err(),
            "omitted singleton history must still fence strict replay"
        );
    }
    #[test]
    fn callback_drain_is_nonrecursive_and_process_local() {
        let world = World::new(47);
        world.enable_scheduler();
        let seen = Rc::new(RefCell::new(Vec::new()));
        world.node(Some(1));
        let output = seen.clone();
        world.schedule(move || {
            let world = current().unwrap();
            world.run_tasks();
            output.borrow_mut().push(1);
            world.schedule(move || output.borrow_mut().push(2));
        });
        world.node(Some(2));
        let output = seen.clone();
        world.schedule(move || output.borrow_mut().push(3));
        world.finish_process_tasks(Process {
            node: Some(1),
            incarnation: 0,
            worker: 0,
        });
        assert_eq!(&*seen.borrow(), &[1, 2]);
        assert_eq!(world.tick(), 0);
        assert_eq!(world.process().node, Some(2));
        for _ in 0..3 {
            world.service_tick();
        }
        assert_eq!(&*seen.borrow(), &[1, 2, 3]);
        world.assert_clean();
    }
    #[test]
    fn workers_share_replay_ledger_but_not_entropy_or_listener_ownership() {
        let world = World::new(71);
        world.enable_scheduler();
        let address = "127.0.0.1:12345".parse().unwrap();
        let mut entropy = [[0; 32]; 2];
        let mut listeners = Vec::new();
        let mut old = Vec::new();
        for worker in 0..2 {
            let _scope = world.scoped_worker(Some(3), worker);
            old.push(world.process());
            world.random(&mut entropy[worker as usize]);
            let admitted = world.accept_nonce([7; 32]);
            assert_eq!(admitted.is_ok(), worker == 0, "ledger is process shared");
            listeners.push(world.listen(address).unwrap());
            assert_eq!(
                world.listen(address).err().unwrap().kind(),
                io::ErrorKind::AddrInUse
            );
        }
        assert_ne!(entropy[0], entropy[1]);
        let raw = libc::sockaddr_in {
            sin_family: libc::AF_INET as _,
            sin_port: 12345u16.to_be(),
            sin_addr: libc::in_addr {
                s_addr: u32::from_ne_bytes([127, 0, 0, 1]),
            },
            sin_zero: [0; 8],
        };
        let connect = || {
            let socket = world.socket();
            let result = unsafe {
                world.operation(
                    16,
                    socket.id,
                    (&raw as *const libc::sockaddr_in) as u64,
                    0,
                    0,
                    0,
                    0,
                )
            }
            .unwrap()
            .0;
            (socket, result)
        };
        let mut seen = BTreeSet::new();
        for _ in 0..32 {
            let (_client, result) = connect();
            assert_eq!(result, 0);
            for (worker, listener) in listeners.iter().enumerate() {
                if let Some((_, accepted)) =
                    unsafe { world.operation(13, listener.id, 0, 0, 0, 0, 0) }
                {
                    seen.insert(worker);
                    drop(accepted);
                }
            }
        }
        assert_eq!(
            seen,
            BTreeSet::from([0, 1]),
            "both live workers must accept"
        );
        listeners.remove(0).shutdown();
        let (_client, result) = connect();
        assert_eq!(result, 0, "closing one member must preserve its peer");
        let accepted = unsafe { world.operation(13, listeners[0].id, 0, 0, 0, 0, 0) }.unwrap();
        drop(accepted);
        world.restart_node(Some(3));
        for process in old {
            assert!(!world.is_current(process));
            let _scope = world.scoped_process(process);
            assert!(world.accept_nonce([8; 32]).is_err());
            assert!(world.listen(address).is_err());
        }
        assert_eq!(
            connect().1,
            -libc::ECONNREFUSED,
            "stale members are not eligible"
        );
        let replacement = {
            let _scope = world.scoped_worker(Some(3), 0);
            assert!(world.accept_nonce([7; 32]).is_ok());
            world.listen(address).unwrap()
        };
        drop(listeners);
        assert_eq!(connect().1, 0, "old close cannot unregister replacement");
        drop(replacement);
        drop(_client);
        world.assert_clean();
    }
    #[test]
    fn gate_readiness_reports_first_hit_without_socket_bytes() {
        let world = World::new(53);
        world.enable_scheduler();
        world.node(Some(1));
        let a = world.socket();
        let b = world.socket();
        if let Some(Object::Socket { peer, .. }) = world.0.borrow_mut().objects.get_mut(&a.id) {
            *peer = Some(b.id);
        }
        let endpoint = "127.0.0.1:1234".parse().unwrap();
        world.tag_socket(a.id, endpoint, "/held".into());
        world.socket_phase(a.id, Phase::Headers);
        world.gate(Gate::new(1, endpoint, "/held", Phase::Headers, None));
        assert!(world.operation_ready(27, a.id));
        assert_eq!(
            world.intercept(Some(1), endpoint, "/held", Phase::Headers),
            Some(None)
        );
        assert!(!world.operation_ready(27, a.id));
        world.release(0);
        assert!(!world.operation_ready(27, a.id));
        b.shutdown();
        assert!(world.operation_ready(27, a.id));
        drop((a, b));
        world.assert_clean();
    }
    #[test]
    fn scoped_compute_replay_and_incarnation_fencing() {
        let world = World::new(3);
        let _scope = world.enter();
        world.enable_scheduler();
        world.node(Some(1));
        world.accept_nonce([1; 32]).unwrap();
        assert!(world.accept_nonce([1; 32]).is_err());
        let seen = Rc::new(RefCell::new(Vec::new()));
        let output = seen.clone();
        world.schedule(move || output.borrow_mut().push(current().unwrap().process()));
        world.node(Some(2));
        world.accept_nonce([1; 32]).unwrap();
        for _ in 0..3 {
            world.service_tick();
        }
        assert_eq!(seen.borrow()[0].node, Some(1));
        assert_eq!(world.process().node, Some(2));
        world.schedule(|| panic!("old incarnation callback"));
        world.restart_node(Some(2));
        world.accept_nonce([1; 32]).unwrap();
        for _ in 0..3 {
            world.service_tick();
        }
        world.assert_clean();
    }
    #[test]
    fn replay_entropy_trace_and_sector_boundaries() {
        let a = World::new(4);
        let b = World::new(4);
        a.enable_scheduler();
        b.enable_scheduler();
        a.random(&mut [0; 97]);
        assert_eq!(a.delay(), b.delay());
        assert_eq!(a.choose(17), b.choose(17));
        let c = World::new(4);
        c.enable_scheduler();
        c.script(vec![2, 0]);
        c.limits(3, 3, 2);
        assert_eq!(c.choose(3), 2);
        assert_eq!(c.choose(1), 0);
        assert_eq!(c.choice_count(), 1);
        assert_eq!(c.choose(2), 0);
        c.assert_replay_consumed();
        assert_eq!(c.choices()[0].enabled, 3);
        let mut cursor = 0;
        c.event("a", "", "");
        c.event("b", "", "");
        assert_eq!(c.events_since(&mut cursor).unwrap().len(), 2);
        c.event("c", "", "");
        assert_eq!(c.events_since(&mut cursor).unwrap().len(), 1);
        assert!(c.events_since(&mut 0).is_err());
        let disk = Disk::new(2048);
        disk.write_all_at(&[7; 1026], 511).unwrap();
        disk.sync_data().unwrap();
        let digest = disk.digest();
        disk.write_all_at(&[8; 2], 512).unwrap();
        disk.crash(0);
        let mut out = [0; 1028];
        disk.read_exact_at(&mut out, 510).unwrap();
        assert_eq!(out[0], 0);
        assert_eq!(&out[1..1027], &[7; 1026]);
        assert_eq!(out[1027], 0);
        assert_eq!(disk.digest(), digest);
        assert!(disk.read_exact_at(&mut out, u64::MAX).is_err());
    }
    struct App {
        polls: usize,
        busy: bool,
        wake_recheck: bool,
        deadline: Option<Instant>,
    }
    impl Application for App {
        fn poll(&mut self, ring: &mut uring::Ring, _: usize) -> io::Result<Work> {
            self.polls += 1;
            if self.wake_recheck && self.polls == 2 {
                ring.wake_handle().wake();
            }
            Ok(Work {
                runnable: self.busy,
                deadline: self.deadline,
            })
        }
        fn shutdown(&mut self, _: &mut uring::Ring) -> io::Result<()> {
            Ok(())
        }
    }
    #[test]
    fn production_driver_parks_retains_cross_thread_wakes_and_services_busy_time() {
        let world = World::new(8);
        let _scope = world.enter();
        world.enable_scheduler();
        let ring = uring::Ring::http_test_ring(buffers::io_test_pool(1), uring::Config::default())
            .unwrap();
        let mut driver = uring::Driver::new(
            ring,
            App {
                polls: 0,
                busy: false,
                wake_recheck: true,
                deadline: None,
            },
            1,
        )
        .unwrap();
        driver.turn().unwrap();
        assert!(!driver.parked());
        assert!(driver.ready());
        driver.turn().unwrap();
        driver.turn().unwrap();
        assert!(driver.parked());
        assert!(!driver.ready());
        assert_eq!(world.tick(), 0);
        let wake = driver.wake_handle();
        std::thread::spawn(move || wake.wake()).join().unwrap();
        assert!(driver.ready());
        driver.turn().unwrap();
        driver.application_mut().busy = true;
        for _ in 0..5 {
            driver.turn().unwrap();
            world.service_tick();
        }
        assert_eq!(world.tick(), 5);
        driver.application_mut().busy = false;
        driver.application_mut().deadline = Some(world.now() + Duration::from_millis(2));
        driver.turn().unwrap();
        assert!(driver.parked());
        assert!(!driver.ready());
        world.service_tick();
        world.service_tick();
        assert!(driver.ready());
        let tick = world.tick();
        driver.shutdown().unwrap();
        assert_eq!(world.tick(), tick);
        world.assert_clean();
    }
    #[test]
    fn cancellation_due_time_and_effect_completion_separation() {
        let world = World::new(5);
        world.enable_scheduler();
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                user_data: 2,
                ..Default::default()
            },
            1,
        ));
        ring.staged.push((
            Sqe {
                opcode: 14,
                user_data: 3,
                addr: 2,
                ..Default::default()
            },
            10,
        ));
        world.service_tick();
        ring.enter();
        assert!(ring.cq.is_empty());
        assert_eq!(ring.completions[0].0.res, 0);
        world.advance(Duration::from_millis(9));
        ring.enter();
        world.advance(Duration::from_millis(3));
        ring.enter();
        assert!(ring.cq.iter().any(|c| c.user_data == 2 && c.res == 0));
        assert!(
            ring.cq
                .iter()
                .any(|c| c.user_data == 3 && c.res == -libc::EALREADY)
        );
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                user_data: 4,
                ..Default::default()
            },
            100,
        ));
        ring.staged.push((
            Sqe {
                opcode: 14,
                user_data: 5,
                addr: 4,
                ..Default::default()
            },
            14,
        ));
        ring.enter();
        assert_eq!(ring.pending.len(), 2);
        world.service_tick();
        ring.enter();
        assert!(ring.pending.is_empty());
        assert!(ring.cq.is_empty());
        ring.quiesce();
        assert!(
            ring.cq
                .iter()
                .any(|c| c.user_data == 4 && c.res == -libc::ECANCELED)
        );
    }
    #[test]
    fn send_zc_alternatives_and_old_ring_effect_fencing() {
        let mut alternatives = [false; 2];
        for seed in 0..32 {
            let world = World::new(seed);
            world.enable_scheduler();
            world.node(Some(7));
            let a = world.socket();
            let b = world.socket();
            if let Some(Object::Socket { peer, .. }) = world.0.borrow_mut().objects.get_mut(&a.id) {
                *peer = Some(b.id);
            }
            let input = [9u8; 8];
            let mut ring = raw(&world);
            ring.staged.push((
                Sqe {
                    opcode: 47,
                    fd: a.id,
                    user_data: 2,
                    addr: input.as_ptr() as u64,
                    len: 8,
                    ..Default::default()
                },
                1,
            ));
            world.service_tick();
            ring.enter();
            let more = ring.completions[0].0.flags == 2;
            alternatives[usize::from(more)] = true;
            assert_eq!(ring.completions.len(), if more { 2 } else { 1 });
            assert!(ring.cq.is_empty());
            world.advance(Duration::from_millis(20));
            ring.enter();
            assert_eq!(ring.cq[0].res, 8);
            if more {
                assert_eq!(ring.cq[1].flags, 8);
            }
            ring.staged.push((
                Sqe {
                    opcode: 47,
                    fd: a.id,
                    user_data: 3,
                    addr: input.as_ptr() as u64,
                    len: 8,
                    ..Default::default()
                },
                22,
            ));
            world.restart_node(Some(7));
            world.service_tick();
            ring.enter();
            let s = world.0.borrow();
            let Object::Socket { bytes, .. } = &s.objects[&b.id] else {
                unreachable!()
            };
            assert_eq!(bytes.len(), 8, "old incarnation performed an effect");
            drop(s);
            ring.quiesce();
            drop((a, b));
            world.assert_clean();
        }
        assert_eq!(alternatives, [true, true]);
    }
    #[test]
    fn idle_accept_parks_until_connection_effect() {
        let world = World::new(12);
        world.enable_scheduler();
        let listener = world.listen("127.0.0.1:4567".parse().unwrap()).unwrap();
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                opcode: 13,
                fd: listener.id,
                user_data: 2,
                ..Default::default()
            },
            1,
        ));
        world.service_tick();
        ring.enter();
        assert_eq!(world.choice_count(), 0);
        assert_eq!(ring.next_tick(), None);
        let remote = world.socket();
        if let Some(Object::Listener { queue, .. }) =
            world.0.borrow_mut().objects.get_mut(&listener.id)
        {
            queue.push_back(remote);
        }
        assert_eq!(ring.next_tick(), Some(1));
        ring.enter();
        assert_eq!(
            world.choice_count(),
            0,
            "single accept is not a scheduling branch"
        );
        assert!(
            ring.accepted.contains_key(&2),
            "deterministic accept still executes"
        );
        ring.quiesce();
        drop(ring);
        drop(listener);
        world.assert_clean();
    }
    #[test]
    fn managed_splice_retains_queued_generation_after_punch() {
        let world = World::new(16);
        world.enable_scheduler();
        let disk = Disk::new(4096);
        disk.write_all_at(&[11; 4096], 0).unwrap();
        let file = world.disk(disk.clone());
        let (read, write) = world.pipe();
        let a = world.socket();
        let b = world.socket();
        if let Some(Object::Socket { peer, .. }) = world.0.borrow_mut().objects.get_mut(&a.id) {
            *peer = Some(b.id);
        }
        let mut ring = raw(&world);
        ring.staged.push((
            Sqe {
                opcode: 30,
                fd: write.id,
                file_index: file.id as u32,
                user_data: 2,
                addr: 0,
                off: u64::MAX,
                len: 4096,
                ..Default::default()
            },
            1,
        ));
        world.service_tick();
        ring.enter();
        assert!(ring.cq.is_empty());
        ring.staged.push((
            Sqe {
                opcode: 30,
                fd: a.id,
                file_index: read.id as u32,
                user_data: 3,
                addr: u64::MAX,
                off: u64::MAX,
                len: 4096,
                ..Default::default()
            },
            2,
        ));
        world.service_tick();
        ring.enter();
        disk.punch(0, 4096);
        disk.write_all_at(&[22; 4096], 0).unwrap();
        let mut out = [0u8; 4096];
        // SAFETY: output lives through this synchronous simulated receive.
        let result =
            unsafe { world.operation(27, b.id, out.as_mut_ptr() as u64, 4096, 0, 0, 0) }.unwrap();
        assert_eq!(result.0, 4096);
        assert_eq!(out, [11; 4096]);
        ring.quiesce();
        drop(ring);
        drop((a, b, read, write, file));
        world.assert_clean();
    }
    #[test]
    fn driver_shutdown_retires_pending_io_without_clock_or_recursion() {
        struct Closing(Option<uring::Ticket<uring::Bytes>>);
        impl Application for Closing {
            fn poll(&mut self, _: &mut uring::Ring, _: usize) -> io::Result<Work> {
                Ok(Work::default())
            }
            fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
                for _ in 0..4 {
                    ring.progress()?;
                    if let Some(done) = ring.take_bytes(self.0.as_mut().unwrap())? {
                        assert_eq!(
                            done.result.unwrap_err().raw_os_error(),
                            Some(libc::ECANCELED)
                        );
                        self.0.take();
                        return Ok(());
                    }
                }
                panic!("shutdown failed to retire pending receive")
            }
        }
        let world = World::new(19);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut ring =
            uring::Ring::http_test_ring(buffers::io_test_pool(1), uring::Config::default())
                .unwrap();
        let a = world.socket();
        let b = world.socket();
        if let Some(Object::Socket { peer, .. }) = world.0.borrow_mut().objects.get_mut(&a.id) {
            *peer = Some(b.id);
        }
        let ticket = ring
            .recv_bytes(File::simulated(a).into(), vec![0; 8].into_boxed_slice())
            .unwrap();
        let mut driver = uring::Driver::new(ring, Closing(Some(ticket)), 1).unwrap();
        driver.turn().unwrap();
        let tick = world.tick();
        driver.shutdown().unwrap();
        assert_eq!(world.tick(), tick);
        drop(driver);
        drop(b);
        world.assert_clean();
    }
}

/// Runtime-independent workload descriptions, byte references and graph oracles.
pub(crate) mod corpus {
    use std::collections::{BTreeSet, VecDeque};
    use std::net::SocketAddr;

    pub const VERSION: u64 = 7;
    pub const SMALL: usize = 257;

    pub fn identity(node: usize) -> [u8; 32] {
        let mut id = *blake3::hash(b"racer invariant cluster node").as_bytes();
        id[..8].copy_from_slice(&(node as u64).to_le_bytes());
        id
    }
    pub fn address(node: usize, origin: bool) -> SocketAddr {
        SocketAddr::from((
            [127, 0, 0, 1],
            (if origin { 20000 } else { 10000 }) + node as u16,
        ))
    }
    pub fn length(target: &str) -> usize {
        if let Some(size) = target
            .strip_prefix("/sized/")
            .and_then(|s| s.split('/').next())
        {
            size.parse().expect("corpus object length")
        } else if target.starts_with("/large/") {
            crate::buffers::BUFFER_SIZE + 17
        } else {
            SMALL
        }
    }
    pub fn origin_bytes(target: &str, offset: usize, bytes: &mut [u8]) {
        let mut salt = VERSION;
        for byte in target.bytes() {
            salt = salt.wrapping_mul(257).wrapping_add(byte as u64);
        }
        for (i, byte) in bytes.iter_mut().enumerate() {
            let at = (offset + i) as u64;
            *byte = (salt.rotate_right((at % 8) as u32 * 8) as u8)
                .wrapping_add(at.wrapping_mul(17) as u8)
                .wrapping_add((at / 251) as u8);
        }
    }
    // Independent arithmetic/representation; never reads a returned cache buffer.
    pub fn reference(target: &str, offset: usize, len: usize) -> Vec<u8> {
        let salt = target.bytes().fold(VERSION as u128, |s, b| {
            (s * 257 + b as u128) % (1u128 << 64)
        });
        (offset..offset + len)
            .map(|at| {
                (((salt / 256u128.pow((at % 8) as u32)) % 256 + at as u128 * 17 + at as u128 / 251)
                    % 256) as u8
            })
            .collect()
    }
    pub fn owner(target: &str, count: usize) -> usize {
        (u64::from_le_bytes(
            blake3::hash(target.as_bytes()).as_bytes()[..8]
                .try_into()
                .unwrap(),
        ) % count as u64) as usize
    }
    #[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
    pub struct Request {
        pub node: usize,
        pub target: String,
        pub range: Option<(usize, usize)>,
    }
    pub fn get(node: usize, target: impl Into<String>) -> Request {
        Request {
            node,
            target: target.into(),
            range: None,
        }
    }
    #[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
    pub enum Action {
        Get(Request),
        Head(Request),
        Turn(usize),
        Drain,
        Settle,
        Cancel(usize),
        Reload(usize),
        Topology(usize),
        ReloadAll,
        Restart(usize),
        CrashSectors(usize, Vec<u64>),
        WallOffset(usize, i64),
        OriginOff(usize),
        Durable(usize, String),
        CorruptRead,
        Refuse(usize, usize, String),
        Hold(usize, usize, String),
        AwaitGate,
        AwaitOverlap,
        Release,
    }
    pub fn degree(count: usize) -> usize {
        let mut degree = 1;
        while degree * degree * degree < count {
            degree += 1;
        }
        degree
    }
    pub fn graph(count: usize) -> (Vec<BTreeSet<usize>>, Vec<Vec<u8>>) {
        let degree = degree(count);
        let mut incoming = vec![BTreeSet::new(); count];
        for source in 0..count {
            for digit in 0..degree {
                incoming[(source * degree + digit) % count].insert(source);
            }
        }
        let distance = (0..count)
            .map(|owner| {
                let mut rank = vec![u8::MAX; count];
                rank[owner] = 0;
                let mut queue = VecDeque::from([owner]);
                while let Some(next) = queue.pop_front() {
                    for &source in &incoming[next] {
                        if rank[source] == u8::MAX {
                            rank[source] = rank[next] + 1;
                            queue.push_back(source);
                        }
                    }
                }
                assert!(rank.iter().all(|r| *r <= 3));
                rank
            })
            .collect();
        (incoming, distance)
    }
    pub fn buckets(count: usize) -> Vec<Vec<String>> {
        let mut buckets = vec![Vec::new(); count];
        let mut remaining = count * 4;
        for serial in 0..count * 128 {
            let target = format!("/object/{serial}?exact=%2f&version={VERSION}");
            let bucket = &mut buckets[owner(&target, count)];
            if bucket.len() < 4 {
                bucket.push(target);
                remaining -= 1;
            }
            if remaining == 0 {
                break;
            }
        }
        assert_eq!(remaining, 0, "bounded target corpus exhausted");
        buckets
    }
    /// Cover both READ roles; a fixed digit fails when gcd(degree, count) != 1.
    pub fn covering_edges(count: usize) -> Vec<(usize, usize)> {
        let d = degree(count);
        fn assign(
            source: usize,
            d: usize,
            matched: &mut [Option<usize>],
            seen: &mut [bool],
        ) -> bool {
            for digit in 0..d {
                let destination = (source * d + digit) % matched.len();
                if destination == source || seen[destination] {
                    continue;
                }
                seen[destination] = true;
                if matched[destination].is_none_or(|previous| assign(previous, d, matched, seen)) {
                    matched[destination] = Some(source);
                    return true;
                }
            }
            false
        }
        let mut matched = vec![None; count];
        for source in 0..count {
            if !assign(source, d, &mut matched, &mut vec![false; count]) {
                let mut edges: Vec<_> = (0..count)
                    .map(|a| {
                        (
                            a,
                            (0..d)
                                .map(|digit| (a * d + digit) % count)
                                .find(|b| *b != a)
                                .unwrap(),
                        )
                    })
                    .collect();
                for b in 0..count {
                    if !edges.iter().any(|(_, destination)| *destination == b) {
                        let a = (0..count)
                            .find(|a| *a != b && (0..d).any(|digit| (*a * d + digit) % count == b))
                            .unwrap();
                        edges.push((a, b));
                    }
                }
                return edges;
            }
        }
        let mut edges: Vec<_> = matched
            .into_iter()
            .enumerate()
            .map(|(destination, source)| (source.unwrap(), destination))
            .collect();
        edges.sort_unstable();
        assert_eq!(
            edges
                .iter()
                .map(|(_, destination)| *destination)
                .collect::<BTreeSet<_>>()
                .len(),
            count
        );
        assert!(
            edges
                .iter()
                .enumerate()
                .all(|(source, edge)| source == edge.0 && edge.0 != edge.1)
        );
        edges
    }
    /// Fresh targets for each coverage round, generated in one bounded pass.
    pub fn cold_targets(count: usize, round: usize) -> Vec<String> {
        let mut targets = vec![String::new(); count];
        let mut remaining = count;
        for serial in 0..count * 128 {
            let target = format!("/read-coverage/{round}/{serial}?exact=%2f&version={VERSION}");
            let slot = owner(&target, count);
            if targets[slot].is_empty() {
                targets[slot] = target;
                remaining -= 1;
            }
            if remaining == 0 {
                return targets;
            }
        }
        panic!("bounded cold coverage targets exhausted");
    }
    /// Each wave has at most one outgoing and one incoming request per node.
    pub fn edge_waves(edges: &[(usize, usize)]) -> Vec<Vec<(usize, usize)>> {
        let mut waves: Vec<Vec<(usize, usize)>> = Vec::new();
        for &(a, b) in edges {
            if let Some(wave) = waves.iter_mut().find(|wave| {
                wave.iter()
                    .all(|(source, destination)| *source != a && *destination != b)
            }) {
                wave.push((a, b));
            } else {
                waves.push(vec![(a, b)]);
            }
        }
        waves
    }
    pub fn read_coverage(
        label: &str,
        initiated: &[usize],
        served: &[usize],
        before: &(Vec<usize>, Vec<usize>),
        required: usize,
    ) -> bool {
        let missing: Vec<_> = (0..initiated.len())
            .filter_map(|node| {
                let reads = (
                    initiated[node] - before.0[node],
                    served[node] - before.1[node],
                );
                (reads.0 < required || reads.1 < required).then_some((node, reads.0, reads.1))
            })
            .collect();
        eprintln!(
            "DST {label} required={required} deficit_count={} first16(node,initiated,served)={:?} totals=({}, {})",
            missing.len(),
            &missing[..missing.len().min(16)],
            initiated.iter().sum::<usize>(),
            served.iter().sum::<usize>()
        );
        missing.is_empty()
    }
    pub fn ready_permutation(world: &super::World, mut nodes: Vec<(usize, u64)>) -> Vec<usize> {
        let mut history = blake3::Hasher::new();
        history.update(b"cluster-ready-pool/v1");
        for (_, identity) in &nodes {
            history.update(&identity.to_le_bytes());
        }
        let mut order = Vec::with_capacity(nodes.len());
        while !nodes.is_empty() {
            // Initial ordered identities + prior removals uniquely determine the
            // current indexed pool. No rehash of its shrinking contents is needed.
            let selected =
                world.choose_fingerprint(nodes.len(), *history.clone().finalize().as_bytes());
            let (node, identity) = nodes.swap_remove(selected);
            history.update(&(selected as u64).to_le_bytes());
            history.update(&identity.to_le_bytes());
            order.push(node);
        }
        order
    }
    /// Production routes; cluster independently checks edges with reverse BFS.
    pub fn relay_paths(count: usize, distances: &[Vec<u8>]) -> Vec<Vec<usize>> {
        let topology =
            crate::topology::Topology::new(count as u32, crate::topology::Epoch::new(1)).unwrap();
        [0, count / 2, count - 1]
            .into_iter()
            .collect::<BTreeSet<_>>()
            .into_iter()
            .filter_map(|source| {
                let owner = (0..count)
                    .max_by_key(|owner| distances[*owner][source])
                    .unwrap();
                if distances[owner][source] < 2 {
                    return None;
                }
                let mut route = topology
                    .route(
                        topology.slot(source as u32).unwrap(),
                        topology.slot(owner as u32).unwrap(),
                    )
                    .unwrap();
                let mut path = vec![source];
                while let crate::topology::Step::Forward { next } = route.advance() {
                    path.push(next.get() as usize);
                }
                assert_eq!(path.len() - 1, distances[owner][source] as usize);
                Some(path)
            })
            .collect()
    }
    pub fn random_requests(seed: u64, count: usize, len: usize) -> Vec<Action> {
        let mut random = Random(seed);
        let mut actions = Vec::new();
        for i in 0..len {
            let state = random.next();
            let size = [
                0,
                1,
                SMALL,
                crate::buffers::BUFFER_SIZE - 1,
                crate::buffers::BUFFER_SIZE,
                crate::buffers::BUFFER_SIZE + 1,
            ][i % 6];
            let target = format!("/sized/{size}/random/{}?exact=%2f", state % 3);
            let node = random.index(count);
            // Large objects use boundary ranges: the response destination is one
            // page, while the request may span two independently cached pages.
            let range = match i % 6 {
                0 | 1 => None,
                2 => Some((size, size + 1)),
                _ => Some((size.saturating_sub(9), size + 7)),
            };
            actions.push(Action::Head(get(node, target.clone())));
            actions.push(Action::Get(Request {
                node,
                target: target.clone(),
                range,
            }));
            actions.push(Action::Get(Request {
                node: (node + 1) % count,
                target,
                range,
            }));
            actions.push(Action::Turn(random.index(5)));
            // Bound destinations and retain overlap within each pair, rather
            // than relying on timeouts to free a saturated response pool.
            actions.push(Action::Drain);
            if i % 6 != 5 {
                continue;
            }
            actions.push(Action::Settle);
            if state & 1 == 0 {
                actions.push(Action::Restart(node));
            }
            if state & 4 == 0 {
                actions.push(Action::ReloadAll);
            } else {
                for node in 0..count {
                    actions.push(Action::Topology(node));
                }
            }
            let target = format!("/injected/{seed}/{i}");
            let owner = owner(&target, count);
            let d = degree(count);
            let source = (0..count)
                .find(|a| *a != owner && (0..d).any(|digit| (*a * d + digit) % count == owner))
                .unwrap();
            let hold = state & 2 == 0;
            actions.push(if hold {
                Action::Hold(source, owner, target.clone())
            } else {
                Action::Refuse(source, owner, target.clone())
            });
            actions.push(Action::Get(get(source, target)));
            if hold {
                actions.push(Action::AwaitGate);
                actions.push(Action::Cancel(source));
            } else {
                actions.push(Action::Drain);
            }
            actions.push(Action::Release);
        }
        actions
    }
    /// Workload entropy is independent of World/scheduler entropy.
    pub struct Random(pub u64);
    impl Random {
        pub fn next(&mut self) -> u64 {
            self.0 = self.0.wrapping_add(0x9e3779b97f4a7c15);
            let mut z = self.0;
            z = (z ^ (z >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
            z = (z ^ (z >> 27)).wrapping_mul(0x94d049bb133111eb);
            z ^ (z >> 31)
        }
        pub fn index(&mut self, count: usize) -> usize {
            self.next() as usize % count
        }
    }
    /// Execute the same release/heal contract after either cancellation or refusal.
    pub fn gated_request(
        source: usize,
        destination: usize,
        target: String,
        refuse: bool,
    ) -> Vec<Action> {
        vec![
            if refuse {
                Action::Refuse(source, destination, target.clone())
            } else {
                Action::Hold(source, destination, target.clone())
            },
            Action::Get(get(source, target)),
            Action::AwaitGate,
            if refuse {
                Action::Drain
            } else {
                Action::Cancel(source)
            },
            Action::Release,
        ]
    }
    pub fn valid(actions: &[Action], count: usize) -> bool {
        if count == 0 || count > 1024 {
            return false;
        }
        let mut gated = false;
        let mut overlap_gate = None;
        let mut overlap_callers = 0;
        let mut overlap_intact = false;
        let mut pending = vec![0usize; count];
        for action in actions {
            let nodes: Vec<usize> = match action {
                Action::Get(r) | Action::Head(r) => vec![r.node],
                Action::Cancel(n)
                | Action::Reload(n)
                | Action::Topology(n)
                | Action::Restart(n)
                | Action::OriginOff(n)
                | Action::Durable(n, _)
                | Action::CrashSectors(n, _)
                | Action::WallOffset(n, _) => vec![*n],
                Action::Hold(a, b, _) | Action::Refuse(a, b, _) => vec![*a, *b],
                _ => vec![],
            };
            if nodes.iter().any(|n| *n >= count) {
                return false;
            }
            if let Action::WallOffset(_, millis) = action
                && millis.unsigned_abs() > 86_400_000
            {
                return false;
            }
            match action {
                Action::Hold(source, destination, target) => {
                    overlap_gate = Some((*source, *destination, target.as_str()));
                    overlap_callers = 0;
                    overlap_intact = true;
                }
                Action::Refuse(..) | Action::Release => overlap_gate = None,
                Action::Get(request) | Action::Head(request) => {
                    if overlap_gate.is_some_and(|(source, _, target)| {
                        request.node == source && request.target == target
                    }) {
                        overlap_callers += 1;
                    }
                }
                Action::Cancel(node) | Action::Restart(node) | Action::CrashSectors(node, _)
                    if overlap_gate.is_some_and(|(source, _, _)| source == *node) =>
                {
                    overlap_intact = false;
                }
                Action::Drain | Action::Settle | Action::ReloadAll => overlap_intact = false,
                Action::AwaitOverlap => {
                    if !overlap_intact
                        || overlap_callers < 2
                        || !overlap_gate.is_some_and(|(source, destination, target)| {
                            source != destination && target != "/"
                        })
                    {
                        return false;
                    }
                }
                _ => {}
            }
            match action {
                Action::Get(r) | Action::Head(r) => pending[r.node] += 1,
                Action::Settle | Action::ReloadAll if gated => return false,
                Action::Drain | Action::Settle | Action::ReloadAll => pending.fill(0),
                Action::Reload(_) | Action::Topology(_) if pending.iter().any(|n| *n != 0) => {
                    return false;
                }
                Action::Cancel(node) if pending[*node] == 0 => return false,
                Action::Cancel(node) => pending[*node] -= 1,
                Action::Restart(node) | Action::CrashSectors(node, _) => pending[*node] = 0,
                Action::Hold(..) | Action::Refuse(..) if gated => return false,
                Action::Hold(..) | Action::Refuse(..) => gated = true,
                Action::AwaitGate | Action::AwaitOverlap | Action::Release if !gated => {
                    return false;
                }
                Action::Release => {
                    gated = false;
                    pending.fill(0);
                }
                _ => {}
            }
        }
        !gated
    }
    #[derive(Default)]
    pub struct Dependencies {
        producers: BTreeSet<u64>,
        edges: std::collections::BTreeMap<u64, u64>,
    }
    impl Dependencies {
        pub fn observe(&mut self, event: &super::Event) {
            if event.kind == "flight-fill" {
                if let Some(id) = event.flight {
                    self.producers.insert(id);
                    self.edges.remove(&id);
                }
            }
            if let Some(parent) = event.depends_on {
                assert!(
                    self.producers.contains(&parent),
                    "wait before producer: {event:?}"
                );
                let child = event.flight.expect("dependent event must name a flight");
                self.edges.insert(child, parent);
                let mut visited = BTreeSet::from([child]);
                let mut next = parent;
                while let Some(parent) = self.edges.get(&next) {
                    assert!(
                        visited.insert(next),
                        "cycle in observed flight dependencies"
                    );
                    next = *parent;
                }
                assert!(visited.insert(next), "cycle closes at producing flight");
            }
        }
    }

    use crate::{buffers::Key, http::Progress, http_server as http, uring};
    use std::{cell::RefCell, io, rc::Rc};

    pub struct Origin {
        pub node: usize,
        pub hits: Rc<RefCell<Vec<(usize, String)>>>,
        pub scenario: bool,
    }
    pub struct Reply {
        pub request: Request,
        pub status: u16,
        pub length: Option<u64>,
        pub bytes: Vec<u8>,
        pub elapsed: std::time::Duration,
        pub refusal: Option<(usize, super::Phase)>,
    }
    pub fn check_reply(world: &super::World, reply: &Reply) {
        let r = &reply.request;
        if matches!(reply.status, 502 | 503) {
            let (gate, phase) = reply.refusal.unwrap_or_else(|| {
                let events: Vec<_> = world
                    .events()
                    .into_iter()
                    .filter(|e| {
                        (e.target == r.target && e.kind != "network-wait")
                            || e.kind == "breaker-error"
                    })
                    .rev()
                    .take(12)
                    .collect();
                panic!(
                    "healthy request returned {}: {r:?}\nrecent={events:?} elapsed={:?} io={:?}",
                    reply.status,
                    reply.elapsed,
                    world.counts()
                )
            });
            assert!(
                world.hits(gate) > 0,
                "failure without injected refusal: {r:?}"
            );
            assert!(
                reply.status == 503
                    || matches!(phase, super::Phase::Headers | super::Phase::PartialBody),
                "502 allowed only for interrupted response: {phase:?} {r:?}"
            );
            assert_eq!(reply.length, Some(0));
            assert!(
                reply.bytes.is_empty(),
                "failed response leaked bytes: {r:?}"
            );
        } else if r
            .range
            .is_some_and(|(a, b)| a >= length(&r.target) || a > b)
        {
            assert_eq!(reply.status, 416, "{r:?}");
            assert!(reply.bytes.is_empty());
        } else {
            super::history::require(
                reply.status == if r.range.is_some() { 206 } else { 200 },
                "response.status",
                format!("status={} request={r:?}", reply.status),
            );
            let size = length(&r.target);
            let (a, len) = r
                .range
                .map_or((0, size), |(a, b)| (a, b.min(size - 1) - a + 1));
            assert_eq!(reply.length, Some(len as u64), "{r:?}");
            assert_eq!(reply.bytes, reference(&r.target, a, len), "{r:?}");
        }
        assert!(
            reply.elapsed < std::time::Duration::from_secs(15),
            "request used timeout recovery: {r:?}"
        );
    }
    pub enum OriginTask {
        Scenario(crate::http_server::scenario_origin::OriginTask),
        Head(http::SendingHeadHeaders),
        Headers(http::SendingGetHeaders, String, usize, usize),
        Body(http::SendingBody),
        Done,
    }
    impl http::Handler for Origin {
        type Task = OriginTask;
        fn start(&mut self, request: http::Request) -> OriginTask {
            if self.scenario {
                return OriginTask::Scenario(
                    crate::http_server::scenario_origin::Origin {
                        node: self.node,
                        hits: self.hits.clone(),
                    }
                    .start(request),
                );
            }
            let target = request.target().to_owned();
            self.hits.borrow_mut().push((self.node, target.clone()));
            let len = length(&target);
            let mut object = vec![0; len];
            origin_bytes(&target, 0, &mut object);
            let tag = crate::conformance::etag(&object);
            // Target-scoped metadata policies for distributed freshness regressions.
            let policy = match target.split('/').nth(2) {
                Some("missing") if target.starts_with("/ttl/") => None,
                Some("zero") if target.starts_with("/ttl/") => Some("max-age=0"),
                Some("nostore") if target.starts_with("/ttl/") => Some("no-store"),
                Some("positive") if target.starts_with("/ttl/") => Some("max-age=2"),
                _ => Some("max-age=60"),
            };
            let mut headers = vec![("ETag", tag.as_bytes())];
            if let Some(policy) = policy {
                headers.push(("Cache-Control", policy.as_bytes()));
            }
            match request {
                http::Request::Head(r) => OriginTask::Head(
                    r.respond(
                        http::ResponseHead::new(200, Some(len as u64), &headers)
                            .unwrap()
                            .close(),
                    )
                    .unwrap(),
                ),
                http::Request::Get(r) => {
                    let range = std::str::from_utf8(r.headers().get("range").unwrap()).unwrap();
                    let (a, b) = range
                        .strip_prefix("bytes=")
                        .unwrap()
                        .split_once('-')
                        .unwrap();
                    let (a, b): (usize, usize) = (a.parse().unwrap(), b.parse().unwrap());
                    assert!(a.is_multiple_of(crate::buffers::BUFFER_SIZE));
                    assert_eq!(b, (a + crate::buffers::BUFFER_SIZE).min(len) - 1);
                    if let Some(world) = super::current() {
                        world.event(
                            "origin-page",
                            &target,
                            format!("node={} offset={a} len={}", self.node, b - a + 1),
                        );
                    }
                    let content_range = format!("bytes {a}-{b}/{len}");
                    let headers = [
                        ("ETag", tag.as_bytes()),
                        ("Content-Range", content_range.as_bytes()),
                    ];
                    let head = http::ResponseHead::new(206, Some((b - a + 1) as u64), &headers)
                        .unwrap()
                        .close();
                    OriginTask::Headers(r.respond(head).unwrap(), target, a, b - a + 1)
                }
            }
        }
        fn poll(
            &mut self,
            task: &mut OriginTask,
            ring: &mut uring::Ring,
            budget: usize,
        ) -> io::Result<Progress<http::Completed>> {
            let progress = match task {
                OriginTask::Scenario(task) => return task.poll(ring, budget),
                OriginTask::Head(h) => return h.poll(ring, budget),
                OriginTask::Headers(h, ..) => h.poll(ring, budget)?,
                OriginTask::Body(b) => b.poll(ring, budget)?,
                OriginTask::Done => panic!("completed origin task"),
            };
            match progress {
                Progress::Pending(w) => Ok(Progress::Pending(w)),
                Progress::Ready(http::BodyProgress::Done(done)) => {
                    *task = OriginTask::Done;
                    Ok(Progress::Ready(done))
                }
                Progress::Ready(http::BodyProgress::More(writer)) => {
                    let OriginTask::Headers(_, target, offset, len) = task else {
                        panic!("unexpected body continuation")
                    };
                    let key = Key::new(
                        *blake3::hash(format!("origin:{VERSION}:{target}:{offset}").as_bytes())
                            .as_bytes(),
                    );
                    let mut fill = ring.pool().stage(key).map_err(io::Error::other)?;
                    origin_bytes(target, *offset, &mut fill.as_mut_slice()[..*len]);
                    let bytes = fill.publish(*len).unwrap();
                    let chunk = http::BodyChunk::new(bytes, 0..*len).map_err(|e| e.error)?;
                    *task = OriginTask::Body(writer.send(chunk).map_err(|e| e.error)?);
                    Ok(Progress::Pending(uring::Work {
                        runnable: true,
                        deadline: None,
                    }))
                }
            }
        }
    }
}
#[test]
fn held_sync_has_no_effect_until_release_and_crash_clears_hold() {
    let world = World::new(19);
    let _scope = world.enter();
    let disk = Disk::new(4096);
    disk.write_all_at(&[1; 512], 0).unwrap();
    disk.sync_data().unwrap();
    disk.write_all_at(&[2; 512], 0).unwrap();
    let handle = world.disk(disk.clone());
    disk.hold_sync(true);
    assert!(!world.operation_ready(3, handle.id));
    assert!(unsafe { world.operation(3, handle.id, 0, 0, 0, 0, -1) }.is_none());
    assert_eq!(disk.dirty_sectors(), vec![0]);
    disk.crash(0);
    let mut bytes = [0; 512];
    disk.read_exact_at(&mut bytes, 0).unwrap();
    assert_eq!(bytes, [1; 512]);
    assert!(world.operation_ready(3, handle.id));
    disk.write_all_at(&[3; 512], 0).unwrap();
    disk.hold_sync(true);
    disk.hold_sync(false);
    assert_eq!(
        unsafe { world.operation(3, handle.id, 0, 0, 0, 0, -1) }
            .unwrap()
            .0,
        0
    );
    disk.crash(0);
    disk.read_exact_at(&mut bytes, 0).unwrap();
    assert_eq!(bytes, [3; 512]);
}
