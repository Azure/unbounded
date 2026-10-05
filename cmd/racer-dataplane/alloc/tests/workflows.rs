#![cfg(feature = "simulation")]

use page_alloc::{Charge, Error, Generation, SegmentId, SegmentState, Segments, Slab};
use std::{cell::Cell, rc::Rc};
mod support;
use support::{
    CountingCharge,
    simulated::{TestError, TestScope, drive, poll},
};
use uring_runtime::{
    reactor::Reactor,
    reactor::simulation::{Fault, Simulation},
};

#[test]
fn reopen_snapshot_reads_old_record_and_appends_without_overwriting_it() {
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    let make_slab = || Slab::<()>::new("/alloc-workflows/restart".into(), 16384, 8192, 1024);
    let slab = make_slab();
    let segments = Segments::new(slab.segment_bytes());
    let alignment = slab.open_configured(&segments).unwrap();
    let payload = b"a caller-owned record, not a cache entry";
    let padded = alignment.extent(0, payload.len()).unwrap().length();
    let (lease, extent) = segments.append(padded).unwrap();
    let (id, generation) = (lease.id(), lease.generation());
    let mut buffer = slab.allocate(padded, ()).unwrap();
    buffer.as_mut_slice()[..payload.len()].copy_from_slice(payload);
    drop(
        drive(
            &reactor,
            slab.write(&reactor, extent, buffer, lease, &TestScope),
        )
        .unwrap(),
    );
    drive(&reactor, slab.fence_writes()).unwrap();
    let frozen = segments.freeze().unwrap();
    let snapshot = segments.snapshot();
    assert_eq!(snapshot[0].state, SegmentState::Open);
    assert_eq!(snapshot[0].used_bytes, padded as u64);
    assert_eq!(snapshot[1].state, SegmentState::Free);
    assert_eq!(segments.restore(snapshot.clone()), Err(Error::Busy));
    drop(frozen);
    drop(slab);
    drop(segments);

    let slab = make_slab();
    let segments = Segments::new(slab.segment_bytes());
    let alignment = slab.open_configured(&segments).unwrap();
    // Restore validates the entire image before changing any slot or free list.
    for (invalid, label) in [
        "duplicate segment ID",
        "zero generation",
        "oversized sealed segment",
        "misaligned sealed segment",
        "nonempty free segment",
        "missing segment",
    ]
    .into_iter()
    .enumerate()
    {
        let mut damaged = snapshot.clone();
        match invalid {
            0 => damaged[1].id = SegmentId(0),
            1 => damaged[1].generation = Generation(0),
            2 => {
                damaged[1].state = SegmentState::Sealed;
                damaged[1].used_bytes = slab.segment_bytes() + alignment.length() as u64;
            }
            3 => {
                damaged[1].state = SegmentState::Sealed;
                damaged[1].used_bytes = 513;
            }
            4 => damaged[1].used_bytes = alignment.length() as u64,
            _ => {
                damaged.pop();
            }
        }
        assert_eq!(segments.restore(damaged), Err(Error::Corrupt), "{label}");
        assert_eq!(segments.free_count(), 2, "{label}");
        assert!(
            segments
                .snapshot()
                .iter()
                .all(|s| s.state == SegmentState::Free && s.used_bytes == 0),
            "{label}"
        );
    }
    segments.validate_restore(&snapshot).unwrap();
    segments.restore(snapshot).unwrap();
    assert_eq!(segments.state(id), Ok(SegmentState::Sealed));
    assert_eq!(segments.free_count(), 1);
    segments.validate(id, generation, &extent).unwrap();
    let buffer = drive(
        &reactor,
        slab.read(
            &reactor,
            extent,
            slab.allocate(padded, ()).unwrap(),
            segments.lease(id, generation).unwrap(),
            &TestScope,
        ),
    )
    .unwrap();
    assert_eq!(&buffer.as_slice()[..payload.len()], payload);
    assert!(buffer.as_slice()[payload.len()..].iter().all(|b| *b == 0));
    drop(buffer);

    let (next, next_extent) = segments.append(padded).unwrap();
    assert_eq!(next.id(), SegmentId(1));
    assert_eq!(next_extent.offset(), slab.segment_bytes());
    assert_eq!(segments.free_count(), 0);
    let mut buffer = slab.allocate(padded, ()).unwrap();
    buffer.as_mut_slice().fill(99);
    drop(
        drive(
            &reactor,
            slab.write(&reactor, next_extent, buffer, next, &TestScope),
        )
        .unwrap(),
    );
    let buffer = drive(
        &reactor,
        slab.read(
            &reactor,
            extent,
            slab.allocate(padded, ()).unwrap(),
            segments.lease(id, generation).unwrap(),
            &TestScope,
        ),
    )
    .unwrap();
    assert_eq!(&buffer.as_slice()[..payload.len()], payload);
    assert!(buffer.as_slice()[payload.len()..].iter().all(|b| *b == 0));
    assert_eq!(slab.writes_in_flight(), 0);
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn buffer_pool_keeps_only_zeroed_accounted_storage_and_rejects_undercharging() {
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let slab = Slab::new("/alloc-workflows/pool".into(), 8192, 4096, 1024);
    let segments = Segments::new(slab.segment_bytes());
    let alignment = slab.open_configured(&segments).unwrap();
    let size = alignment.extent(0, 31).unwrap().length();
    let live = Rc::new(Cell::new(0));
    let charge = |bytes| CountingCharge::new(&live, bytes);
    let mut buffer = slab.allocate(size, charge(size)).unwrap();
    assert_eq!(buffer.len(), size);
    assert!(!buffer.is_empty());
    let pointer = buffer.bytes().unwrap().as_ptr();
    assert_eq!(pointer as usize % alignment.memory(), 0);
    assert!(buffer.bytes().unwrap().iter().all(|b| *b == 0));
    buffer.bytes_mut().unwrap().fill(42);
    let retained = Rc::new(charge(17));
    let weak = Rc::downgrade(&retained);
    buffer.retain(retained);
    assert_eq!(live.get(), size + 17);
    drop(buffer);
    assert!(weak.upgrade().is_none());
    assert_eq!(live.get(), size);
    assert_eq!(slab.idle_bytes(), size);

    // Denied admission must release its charge without consuming the idle buffer.
    assert!(matches!(
        slab.allocate(size, charge(size - 1)),
        Err(Error::InvalidConfiguration)
    ));
    assert_eq!(live.get(), size);
    assert_eq!(slab.idle_bytes(), size);
    let reused = slab.allocate(size, charge(size)).unwrap();
    assert_eq!(reused.bytes().unwrap().as_ptr(), pointer);
    assert!(reused.bytes().unwrap().iter().all(|b| *b == 0));
    assert_eq!(live.get(), size);
    assert_eq!(slab.idle_bytes(), 0);
    assert_eq!(slab.reclaim_idle(), 0);

    // Two simultaneous users cannot both return storage to the single idle slot.
    let overflow = slab.allocate(size, charge(size)).unwrap();
    assert_eq!(live.get(), size * 2);
    drop(reused);
    drop(overflow);
    assert_eq!(slab.idle_bytes(), size);
    assert_eq!(live.get(), size);
    let larger = slab.allocate(size * 2, charge(size * 2)).unwrap();
    assert_eq!(larger.len(), size * 2);
    assert_eq!(live.get(), size * 2);
    drop(larger);
    assert_eq!(slab.reclaim_idle(), size * 2);
    assert_eq!(slab.reclaim_idle(), 0);
    assert_eq!(live.get(), 0);

    let outstanding = slab.allocate(size, charge(size)).unwrap();
    drop(slab.allocate(size, charge(size)).unwrap());
    assert_eq!(live.get(), size * 2);
    assert_eq!(slab.idle_bytes(), size);
    drop(slab);
    assert_eq!(live.get(), size);
    assert!(outstanding.as_slice().iter().all(|b| *b == 0));
    drop(outstanding);
    assert_eq!(live.get(), 0);
}

#[test]
fn bound_io_rejects_foreign_tables_and_ranges_appended_after_lease_acquisition() {
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    let slab = Slab::<()>::new("/alloc-workflows/bound".into(), 16384, 8192, 512);
    let segments = Segments::new(slab.segment_bytes());
    let alignment = slab.open_configured(&segments).unwrap();
    let other = Segments::from_geometry(segments.geometry().unwrap()).unwrap();
    assert_eq!(
        slab.configure_segments(&other),
        Err(Error::InvalidConfiguration)
    );
    let size = alignment.extent(0, 31).unwrap().length();

    for write in [false, true] {
        let (foreign, extent) = other.append(size).unwrap();
        let buffer = slab.allocate(size, ()).unwrap();
        let operation = if write {
            slab.write(&reactor, extent, buffer, foreign, &TestScope)
        } else {
            slab.read(&reactor, extent, buffer, foreign, &TestScope)
        };
        assert!(matches!(
            drive(&reactor, operation),
            Err(TestError::Alloc(Error::Stale))
        ));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(slab.writes_in_flight(), 0);
    }

    let (early, first) = segments.append(size).unwrap();
    let early_read = segments.lease(early.id(), early.generation()).unwrap();
    let (later, second) = segments.append(size).unwrap();
    drop(later);
    // Both extents are now used, but the earlier leases only authorize the first.
    for (lease, write) in [(early, true), (early_read, false)] {
        let buffer = slab.allocate(size, ()).unwrap();
        let operation = if write {
            slab.write(&reactor, second, buffer, lease, &TestScope)
        } else {
            slab.read(&reactor, second, buffer, lease, &TestScope)
        };
        assert!(matches!(
            drive(&reactor, operation),
            Err(TestError::Alloc(Error::Corrupt))
        ));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(slab.writes_in_flight(), 0);
    }
    let mut buffer = slab.allocate(size, ()).unwrap();
    buffer.as_mut_slice().fill(73);
    drop(
        drive(
            &reactor,
            slab.write(
                &reactor,
                second,
                buffer,
                segments.lease(SegmentId(0), Generation(1)).unwrap(),
                &TestScope,
            ),
        )
        .unwrap(),
    );
    let untouched = drive(
        &reactor,
        slab.read(
            &reactor,
            first,
            slab.allocate(size, ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        ),
    )
    .unwrap();
    assert!(untouched.as_slice().iter().all(|byte| *byte == 0));
    drop(untouched);
    let written = drive(
        &reactor,
        slab.read(
            &reactor,
            second,
            slab.allocate(size, ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        ),
    )
    .unwrap();
    assert!(written.as_slice().iter().all(|byte| *byte == 73));
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(slab.writes_in_flight(), 0);
}

#[test]
fn restore_waits_for_freeze_guard_and_abandoned_write_completion_independently() {
    for cancel_first in [false, true] {
        let simulation = Simulation::new();
        simulation.set_cancel_first(cancel_first);
        let _environment = simulation.enter();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let slab = Slab::<()>::new("/alloc-workflows/freeze".into(), 8192, 4096, 512);
        let segments = Segments::new(slab.segment_bytes());
        let alignment = slab.open_configured(&segments).unwrap();
        let size = alignment.extent(0, 31).unwrap().length();
        let (lease, extent) = segments.append(size).unwrap();
        simulation.inject("write", Fault::HoldCompletion(8));
        let mut write = slab.write(
            &reactor,
            extent,
            slab.allocate(size, ()).unwrap(),
            lease,
            &TestScope,
        );
        assert!(poll(&mut write).is_pending());
        reactor.poll_budgeted(1).unwrap();
        drop(write);
        assert_eq!(slab.writes_in_flight(), 1);

        let frozen = segments.freeze().unwrap();
        let snapshot = segments.snapshot();
        assert!(matches!(segments.append(size), Err(Error::Busy)));
        assert_eq!(segments.restore(snapshot.clone()), Err(Error::Busy));
        drop(frozen);
        // Thawing is not a kernel completion fence; the abandoned write owns a lease.
        assert_eq!(segments.restore(snapshot.clone()), Err(Error::Busy));
        assert_eq!(segments.snapshot(), snapshot);
        let frozen = segments.freeze().unwrap();
        drive(&reactor, slab.fence_writes()).unwrap();
        assert_eq!(slab.writes_in_flight(), 0);
        assert_eq!(reactor.in_flight(), 0);
        // Conversely, completion does not release a caller's freeze guard.
        assert_eq!(segments.restore(snapshot.clone()), Err(Error::Busy));
        drop(frozen);
        segments.restore(snapshot).unwrap();
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
        assert_eq!(segments.append(size).unwrap().0.id(), SegmentId(1));
    }
}
