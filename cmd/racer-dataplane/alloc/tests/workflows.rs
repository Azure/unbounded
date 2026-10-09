//! Public allocator workflows across restart, cancellation, accounting, and real I/O.

use page_alloc::{Alignment, DevicePlacement, Error, Generation, SegmentId, Segments, Slab};
#[cfg(feature = "simulation")]
use page_alloc::{Charge, SegmentState};
#[cfg(feature = "simulation")]
use std::{cell::Cell, path::Path, rc::Rc};
use std::{
    fs::File,
    future::Future,
    os::fd::FromRawFd,
    path::PathBuf,
    pin::Pin,
    sync::Arc,
    task::{Context, Poll, Waker},
};
#[cfg(feature = "simulation")]
use uring_runtime::reactor::simulation::{Fault, Simulation};
use uring_runtime::{Operation, Scope, reactor::Reactor};

/// Distinguishes allocator validation errors from runtime completion failures.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum TestError {
    Alloc(Error),

    Runtime(uring_runtime::Error),
}

impl From<Error> for TestError {
    /// Preserve the allocator category in workflow assertions.
    fn from(error: Error) -> Self {
        Self::Alloc(error)
    }
}

impl From<uring_runtime::Error> for TestError {
    /// Preserve runtime errors without enriching them as synchronous allocator errors.
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

/// An always-live caller scope for deterministic ownership tests.
#[derive(Clone)]
struct TestScope;

impl Scope for TestScope {
    type Error = TestError;

    /// Keep the scope live; cancellation is injected by dropping waiting futures.
    fn check(&self) -> Result<(), TestError> {
        Ok(())
    }
}

/// Poll once without assuming that a completion wakes a host executor.
fn poll<T>(future: &mut Pin<Box<dyn Future<Output = T> + '_>>) -> Poll<T> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(Waker::noop()))
}

/// Drive a simulated operation with a fixed, finite reactor-turn budget.
#[cfg(feature = "simulation")]
fn drive<T>(
    reactor: &Reactor<TestScope, ()>,
    mut operation: Pin<Box<dyn Future<Output = T> + '_>>,
) -> T {
    for _ in 0..100 {
        if let Poll::Ready(value) = poll(&mut operation) {
            return value;
        }
        reactor.poll_budgeted(64).unwrap();
    }
    panic!("simulation did not complete in 100 turns");
}

/// Caller-owned accounting for live buffers and the single idle slot.
#[cfg(feature = "simulation")]
struct CountingCharge {
    used: Rc<Cell<usize>>,

    bytes: usize,
}

#[cfg(feature = "simulation")]
impl CountingCharge {
    /// Record admission before passing the guard into allocator ownership.
    fn new(used: &Rc<Cell<usize>>, bytes: usize) -> Self {
        used.set(used.get() + bytes);
        Self {
            used: used.clone(),
            bytes,
        }
    }
}

#[cfg(feature = "simulation")]
impl Charge for CountingCharge {
    /// Cover only the bytes actually admitted by this guard.
    fn covers(&self, bytes: usize) -> bool {
        self.bytes >= bytes
    }
}

#[cfg(feature = "simulation")]
impl Drop for CountingCharge {
    /// Return admission when the final owner releases this guard.
    fn drop(&mut self) {
        self.used.set(self.used.get() - self.bytes);
    }
}

/// Restored records remain readable and the sealed recovery tail is not overwritten.
#[cfg(feature = "simulation")]
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

/// Public pooling enforces admission, zeroization, exact-size reuse, and bounded retention.
#[cfg(feature = "simulation")]
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

/// Bound submissions reject foreign identity and bytes beyond the captured prefix.
#[cfg(feature = "simulation")]
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

/// Thawing and write completion are independent prerequisites for recovery.
#[cfg(feature = "simulation")]
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
        simulation
            .inject("write", Fault::HoldCompletion(8))
            .unwrap();
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

/// Build and bind a deterministic simulated slab with two segments.
#[cfg(feature = "simulation")]
fn setup<C: Charge>() -> (Slab<C>, Segments) {
    let slab = Slab::new(PathBuf::from("/virtual/slab.dat"), 8192, 4096, 512);
    let segments = Segments::new(4096);
    let _ = slab.open_configured(&segments).unwrap();
    (slab, segments)
}

/// Public reads and writes preserve payloads and reject short or failed completions.
#[cfg(feature = "simulation")]
#[test]
fn roundtrip_short_io_and_runtime_errors() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (slab, segments) = setup::<()>();
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    let (lease, extent) = segments.append(4096).unwrap();
    let mut buffer = slab.allocate(extent.length(), ()).unwrap();
    buffer.as_mut_slice().fill(42);
    let buffer = drive(
        &reactor,
        slab.write(&reactor, extent, buffer, lease, &TestScope),
    )
    .unwrap();
    assert_eq!(slab.writes_in_flight(), 0);
    drop(buffer);
    let read = || {
        slab.read(
            &reactor,
            extent,
            slab.allocate(extent.length(), ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        )
    };
    let buffer = drive(&reactor, read()).unwrap();
    assert!(buffer.as_slice().iter().all(|b| *b == 42));
    drop(buffer);
    // Kernel-written bytes must dirty an initially clean pool allocation.
    let reused = slab.allocate(extent.length(), ()).unwrap();
    assert!(reused.as_slice().iter().all(|b| *b == 0));
    drop(reused);
    sim.inject("read", Fault::Short(512)).unwrap();
    assert!(matches!(
        drive(&reactor, read()),
        Err(TestError::Alloc(Error::Io))
    ));
    let reused = slab.allocate(extent.length(), ()).unwrap();
    assert!(reused.as_slice().iter().all(|b| *b == 0));
    drop(reused);
    sim.inject("write", Fault::Short(512)).unwrap();
    let op = slab.write(
        &reactor,
        extent,
        slab.allocate(extent.length(), ()).unwrap(),
        segments.lease(SegmentId(0), Generation(1)).unwrap(),
        &TestScope,
    );
    assert!(matches!(
        drive(&reactor, op),
        Err(TestError::Alloc(Error::Io))
    ));
    assert_eq!(slab.writes_in_flight(), 0);
    sim.inject("read", Fault::Errno(libc::EIO)).unwrap();
    assert!(matches!(
        drive(&reactor, read()),
        Err(TestError::Runtime(uring_runtime::Error::Os(libc::EIO)))
    ));
    assert_eq!(reactor.in_flight(), 0);
    segments.begin_evict(SegmentId(0)).unwrap();
    segments.recycle(SegmentId(0)).unwrap();
}

/// Abandonment cannot release kernel-visible memory, charges, leases, or write count.
#[cfg(feature = "simulation")]
#[test]
fn abandoned_write_retains_lease_charges_and_counter_until_completion() {
    for cancel_first in [false, true] {
        let sim = Simulation::new();
        sim.set_cancel_first(cancel_first);
        let _environment = sim.enter();
        let (slab, segments) = setup::<CountingCharge>();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let used = Rc::new(Cell::new(0));
        let (lease, extent) = segments.append(4096).unwrap();
        let mut buffer = slab
            .allocate(4096, CountingCharge::new(&used, 4096))
            .unwrap();
        buffer.as_mut_slice().fill(42);
        let extra = Rc::new(CountingCharge::new(&used, 512));
        let weak = Rc::downgrade(&extra);
        buffer.retain(extra);
        sim.inject("write", Fault::HoldCompletion(8)).unwrap();
        let mut op = slab.write(&reactor, extent, buffer, lease, &TestScope);
        assert!(poll(&mut op).is_pending());
        assert_eq!(slab.writes_in_flight(), 1);
        reactor.poll_budgeted(1).unwrap();
        drop(op);
        assert_eq!(slab.writes_in_flight(), 1);
        assert_eq!(slab.idle_bytes(), 0);
        assert_eq!(slab.reclaim_idle(), 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(used.get(), 4608);
        segments.begin_evict(SegmentId(0)).unwrap();
        assert_eq!(segments.recycle(SegmentId(0)), Err(Error::Busy));
        let mut fence = slab.fence_writes();
        assert!(poll(&mut fence).is_pending());
        drive(&reactor, fence).unwrap();
        assert_eq!(slab.writes_in_flight(), 0);
        assert_eq!(reactor.in_flight(), 0);
        assert!(weak.upgrade().is_none());
        assert_eq!(used.get(), 4096);
        assert_eq!(slab.idle_bytes(), 4096);
        let reused = slab
            .allocate(4096, CountingCharge::new(&used, 4096))
            .unwrap();
        assert!(reused.as_slice().iter().all(|b| *b == 0));
        drop(reused);
        assert_eq!(slab.reclaim_idle(), 4096);
        assert_eq!(used.get(), 0);
        segments.recycle(SegmentId(0)).unwrap();
    }
}

/// An unpolled write owns no runtime work, but an abandoned accepted read still does.
#[cfg(feature = "simulation")]
#[test]
fn abandoned_read_and_unpolled_write_release_at_the_correct_fence() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (slab, segments) = setup::<()>();
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    let (lease, extent) = segments.append(4096).unwrap();
    let op = slab.write(
        &reactor,
        extent,
        slab.allocate(4096, ()).unwrap(),
        lease,
        &TestScope,
    );
    drop(op);
    assert_eq!(slab.writes_in_flight(), 0);
    assert_eq!(reactor.in_flight(), 0);
    // Seed nonzero bytes so a completed but abandoned read tests erasure, not
    // merely the lifetime of an allocation that happens to remain zero.
    let mut buffer = slab.allocate(4096, ()).unwrap();
    buffer.as_mut_slice().fill(42);
    drop(
        drive(
            &reactor,
            slab.write(
                &reactor,
                extent,
                buffer,
                segments.lease(SegmentId(0), Generation(1)).unwrap(),
                &TestScope,
            ),
        )
        .unwrap(),
    );
    sim.inject("read", Fault::HoldCompletion(8)).unwrap();
    let mut op = slab.read(
        &reactor,
        extent,
        slab.allocate(4096, ()).unwrap(),
        segments.lease(SegmentId(0), Generation(1)).unwrap(),
        &TestScope,
    );
    assert!(poll(&mut op).is_pending());
    reactor.poll_budgeted(1).unwrap();
    drop(op);
    assert_eq!(slab.idle_bytes(), 0);
    assert_eq!(slab.reclaim_idle(), 0);
    segments.begin_evict(SegmentId(0)).unwrap();
    assert_eq!(segments.recycle(SegmentId(0)), Err(Error::Busy));
    drive(&reactor, reactor.drain()).unwrap();
    let reused = slab.allocate(4096, ()).unwrap();
    assert!(reused.as_slice().iter().all(|b| *b == 0));
    segments.recycle(SegmentId(0)).unwrap();
}

/// Simulation preserves file locks, size validation, and synchronous error categories.
#[cfg(feature = "simulation")]
#[test]
fn simulation_open_lock_size_and_faults() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (slab, _) = setup::<()>();
    let conflicting = Slab::<()>::new(PathBuf::from("/virtual/slab.dat"), 8192, 4096, 512);
    assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
    assert_eq!(slab.open_now(), slab.alignment());
    drop(slab);
    let _ = conflicting.open_now().unwrap();
    drop(conflicting);
    let wrong_size = Slab::<()>::new(PathBuf::from("/virtual/slab.dat"), 16384, 4096, 512);
    assert_eq!(wrong_size.open_now(), Err(Error::InvalidConfiguration));
    sim.inject("open", Fault::Errno(libc::EOPNOTSUPP)).unwrap();
    assert_eq!(wrong_size.open_now(), Err(Error::Unsupported));
    sim.inject("open", Fault::Errno(libc::ENOSPC)).unwrap();
    assert_eq!(
        wrong_size.open_now(),
        Err(Error::SystemIo {
            operation: "open",
            errno: Some(libc::ENOSPC)
        })
    );
}

/// Fault hooks cannot swap descriptors while a write completion owns the old file.
#[cfg(feature = "simulation")]
#[test]
fn replacement_hooks_are_fallible_and_refuse_live_writes() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let replacement = || {
        sim.open(
            None,
            Path::new("/replacement"),
            libc::O_CREAT | libc::O_RDWR,
        )
        .unwrap()
    };
    let unopened = Slab::<()>::new(PathBuf::from("/unopened"), 8192, 4096, 512);
    assert_eq!(
        unopened.replace_descriptor_for_test(replacement()),
        Err(Error::Unavailable)
    );
    let (slab, segments) = setup::<()>();
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    let (lease, extent) = segments.append(4096).unwrap();
    sim.inject("write", Fault::HoldCompletion(8)).unwrap();
    let mut write = slab.write(
        &reactor,
        extent,
        slab.allocate(4096, ()).unwrap(),
        lease,
        &TestScope,
    );
    assert!(poll(&mut write).is_pending());
    assert_eq!(
        slab.replace_descriptor_for_test(replacement()),
        Err(Error::Busy)
    );
    drop(write);
    drive(&reactor, slab.fence_writes()).unwrap();
    slab.replace_descriptor_for_test(replacement()).unwrap();
}

/// Owns a unique project-local directory for real kernel workflows.
struct Directory(PathBuf);

impl Directory {
    /// Create an isolated test directory without using the host temporary directory.
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let id = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("page-alloc-workflow-{}-{id}", std::process::id()));
        std::fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}

impl Drop for Directory {
    /// Clean up only this test's owned directory.
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Report explicit capability skips or fail when real-kernel coverage is required.
fn capability_skip(reason: &str) {
    assert_ne!(
        std::env::var("PAGE_ALLOC_REQUIRE_REAL_IO").as_deref(),
        Ok("1"),
        "PAGE_ALLOC_REQUIRE_REAL_IO=1 forbids capability skips: {reason}"
    );
    eprintln!("SKIP real slab test: {reason}");
}

/// Only explicit lack of direct-I/O support may skip real file workflows.
fn real_alignment(result: page_alloc::Result<Alignment>) -> Option<Alignment> {
    match result {
        Ok(alignment) => Some(alignment),
        Err(Error::Unsupported) => {
            capability_skip("filesystem does not support direct I/O geometry");
            None
        }
        Err(error) => panic!("unexpected real slab startup failure: {error}"),
    }
}

/// Discover alignment before choosing any slab dimensions.
fn probe_alignment(directory: &Directory) -> Option<Alignment> {
    use std::os::{fd::AsRawFd, unix::fs::OpenOptionsExt};

    let result = (|| {
        let file = std::fs::OpenOptions::new()
            .create_new(true)
            .read(true)
            .write(true)
            .mode(0o600)
            .custom_flags(libc::O_DIRECT)
            .open(directory.0.join("alignment-probe"))
            .map_err(|error| match error.raw_os_error() {
                Some(libc::EINVAL | libc::EOPNOTSUPP | libc::ENOSYS) => Error::Unsupported,
                errno => Error::SystemIo {
                    operation: "open",
                    errno,
                },
            })?;
        // SAFETY: stat is initialized and the file and empty path remain live.
        let mut stat: libc::statx = unsafe { std::mem::zeroed() };
        if unsafe {
            libc::statx(
                file.as_raw_fd(),
                c"".as_ptr(),
                libc::AT_EMPTY_PATH,
                libc::STATX_DIOALIGN,
                &mut stat,
            )
        } != 0
        {
            return Err(match std::io::Error::last_os_error().raw_os_error() {
                Some(libc::ENOSYS | libc::EOPNOTSUPP) => Error::Unsupported,
                errno => Error::SystemIo {
                    operation: "statx",
                    errno,
                },
            });
        }
        if stat.stx_mask & libc::STATX_DIOALIGN == 0 {
            return Err(Error::Unsupported);
        }
        Alignment::new(
            stat.stx_dio_mem_align as usize,
            stat.stx_dio_offset_align as u64,
            stat.stx_dio_offset_align as usize,
        )
    })();
    real_alignment(result)
}

/// Probe baseline io_uring support without hiding unexpected runtime setup failures.
fn kernel_available() -> bool {
    let mut params = [0u64; 15];
    // SAFETY: initialized, aligned 120-byte UAPI output; success owns a new descriptor.
    let fd = unsafe { libc::syscall(libc::SYS_io_uring_setup, 2u32, params.as_mut_ptr()) };
    if fd >= 0 {
        drop(unsafe { File::from_raw_fd(fd as i32) });
        return true;
    }
    let error = std::io::Error::last_os_error();
    match error.raw_os_error() {
        Some(libc::ENOSYS | libc::EOPNOTSUPP | libc::EPERM | libc::EACCES) => {
            capability_skip(&format!(
                "io_uring kernel capability/permission error: {error}"
            ));
            false
        }
        _ => panic!("unexpected io_uring setup failure: {error}"),
    }
}

/// Drive real I/O with a strict in-test deadline as well as the external test timeout.
fn drive_real<T>(
    reactor: &Reactor<TestScope, ()>,
    mut op: Operation<'_, T, TestError>,
) -> Result<T, TestError> {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
    loop {
        if let Poll::Ready(value) = poll(&mut op) {
            return value;
        }
        reactor.poll_budgeted(64).unwrap();
        assert!(
            std::time::Instant::now() < deadline,
            "real slab I/O did not complete"
        );
        std::thread::yield_now();
    }
}

/// The real kernel preserves payload bytes and releases write completion authority.
#[test]
fn io_uring_roundtrip_and_completion_fence() {
    if !kernel_available() {
        return;
    }
    let directory = Directory::new();
    let Some(alignment) = probe_alignment(&directory) else {
        return;
    };
    let (_, segment_bytes) = device_placement_sizes(alignment);
    let slab = Slab::<()>::new(
        directory.0.join("uring.dat"),
        2 * segment_bytes,
        segment_bytes,
        segment_bytes as usize,
    );
    let segments = Segments::new(segment_bytes);
    assert_eq!(slab.open_configured(&segments), Ok(alignment));
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    reactor.init().unwrap();
    let (lease, extent) = segments.append(segment_bytes as usize).unwrap();
    let mut buffer = slab.allocate(extent.length(), ()).unwrap();
    buffer.as_mut_slice().fill(73);
    drop(
        drive_real(
            &reactor,
            slab.write(&reactor, extent, buffer, lease, &TestScope),
        )
        .unwrap(),
    );
    assert_eq!(slab.writes_in_flight(), 0);
    let mut fence = slab.fence_writes();
    assert_eq!(poll(&mut fence), Poll::Ready(Ok(())));
    let buffer = drive_real(
        &reactor,
        slab.read(
            &reactor,
            extent,
            slab.allocate(extent.length(), ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        ),
    )
    .unwrap();
    assert!(buffer.as_slice().iter().all(|byte| *byte == 73));
    drop(buffer);
    assert_eq!(reactor.in_flight(), 0);
    /// A completed roundtrip has no caller mappings left to remove.
    struct EmptyEntries;

    impl page_alloc::SegmentEntries for EmptyEntries {
        /// The smoke workflow publishes no index entries.
        fn remove_bounded(&self, _: SegmentId, _: usize) -> usize {
            0
        }

        /// Both slots are logically unmapped.
        fn is_empty(&self, _: SegmentId) -> bool {
            true
        }
    }
    let clock = page_alloc::SegmentClock::new(std::rc::Rc::new(segments));
    clock.reclaim(&EmptyEntries, 2, 4, 0).unwrap();
}

/// Keep both halves aligned even when offset and length units differ.
fn device_placement_sizes(alignment: Alignment) -> (usize, u64) {
    let half_bytes = alignment.extent(0, 2048).unwrap().length();
    let segment_bytes = 2 * half_bytes as u64;
    (half_bytes, segment_bytes)
}

/// Placement test geometry must work without relying on the host's alignment.
#[test]
fn device_placement_sizes_support_4096_and_arbitrary_units() {
    for (offset, length, expected_half) in [
        (1, 1, 2048),
        (512, 512, 2048),
        (4096, 4096, 4096),
        (4096, 512, 4096),
        (512, 4096, 4096),
        (8192, 8192, 8192),
        (8192, 512, 8192),
        (512, 8192, 8192),
        (65536, 65536, 65536),
        (768, 512, 3072),
        (3, 5, 2055),
    ] {
        let alignment = Alignment::new(4096, offset, length).unwrap();
        let (half_bytes, segment_bytes) = device_placement_sizes(alignment);
        assert_eq!(half_bytes, expected_half);
        assert_eq!(segment_bytes, 2 * expected_half as u64);
        let geometry =
            page_alloc::SegmentGeometry::new(3 * segment_bytes, segment_bytes, 3, alignment)
                .unwrap();
        let segments = Segments::from_geometry(geometry).unwrap();
        let buffer = alignment.allocate(half_bytes, ()).unwrap();
        if half_bytes != 2048 {
            assert!(matches!(
                segments.append(2048),
                Err(Error::InvalidConfiguration)
            ));
        }
        for (id, physical_start) in [2 * segment_bytes, segment_bytes, 0]
            .into_iter()
            .enumerate()
        {
            for half in 0..2 {
                let (lease, extent) = segments.append(half_bytes).unwrap();
                assert_eq!(lease.id(), SegmentId(id as u64));
                assert_eq!(extent.length(), half_bytes);
                assert_eq!(
                    extent.offset(),
                    id as u64 * segment_bytes + half * half_bytes as u64
                );
                alignment.check(extent, &buffer).unwrap();
                let physical =
                    page_alloc::Extent::new(physical_start + half * half_bytes as u64, half_bytes)
                        .unwrap();
                alignment.check(physical, &buffer).unwrap();
            }
        }
        assert_eq!(segments.free_count(), 0);
        let whole_segments = Segments::from_geometry(geometry).unwrap();
        let buffer = alignment.allocate(segment_bytes as usize, ()).unwrap();
        for id in 0..3 {
            let (lease, extent) = whole_segments.append(segment_bytes as usize).unwrap();
            assert_eq!(lease.id(), SegmentId(id));
            assert_eq!(extent.offset(), id * segment_bytes);
            alignment.check(extent, &buffer).unwrap();
        }
    }
}

/// Device placements translate whole segments and interior extents across real files.
#[test]
fn device_placements_route_real_io_and_reject_short_reads() {
    use std::os::unix::fs::{FileExt, OpenOptionsExt};

    if !kernel_available() {
        return;
    }
    let directory = Directory::new();
    let Some(alignment) = probe_alignment(&directory) else {
        return;
    };
    let (half_bytes, segment_bytes) = device_placement_sizes(alignment);
    let file_bytes = 4 * segment_bytes;
    let files: Vec<_> = ["one", "two"]
        .into_iter()
        .map(|name| {
            let file = std::fs::OpenOptions::new()
                .create_new(true)
                .read(true)
                .write(true)
                .custom_flags(libc::O_DIRECT)
                .open(directory.0.join(name))
                .unwrap();
            file.set_len(file_bytes).unwrap();
            Arc::new(file)
        })
        .collect();
    let slab = Slab::<()>::from_devices(
        vec![
            DevicePlacement {
                file: files[0].clone(),
                offset: 2 * segment_bytes,
            },
            DevicePlacement {
                file: files[1].clone(),
                offset: segment_bytes,
            },
            DevicePlacement {
                file: files[0].clone(),
                offset: 0,
            },
        ],
        segment_bytes,
        segment_bytes as usize,
        alignment,
    )
    .unwrap();
    assert_eq!(slab.capacity_bytes(), 3 * segment_bytes);
    assert_eq!(slab.alignment(), Err(Error::Unavailable));
    let segments = Segments::new(segment_bytes);
    assert_eq!(slab.open_configured(&segments), Ok(alignment));
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    reactor.init().unwrap();
    for id in 0..3 {
        for half in 0..2 {
            let (lease, extent) = segments.append(half_bytes).unwrap();
            assert_eq!(lease.id(), SegmentId(id));
            assert_eq!(
                extent.offset(),
                id * segment_bytes + half * half_bytes as u64
            );
            let mut buffer = slab.allocate(half_bytes, ()).unwrap();
            buffer.as_mut_slice().fill((id * 2 + half + 1) as u8);
            drop(
                drive_real(
                    &reactor,
                    slab.write(&reactor, extent, buffer, lease, &TestScope),
                )
                .unwrap(),
            );
            let read = drive_real(
                &reactor,
                slab.read(
                    &reactor,
                    extent,
                    slab.allocate(half_bytes, ()).unwrap(),
                    segments.lease(SegmentId(id), Generation(1)).unwrap(),
                    &TestScope,
                ),
            )
            .unwrap();
            assert!(
                read.as_slice()
                    .iter()
                    .all(|&byte| byte == (id * 2 + half + 1) as u8)
            );
        }
    }
    // Inspect physical addresses independently so symmetric read/write bugs cannot pass.
    for (file, offset, value) in [
        (0, 0, 5),
        (0, half_bytes as u64, 6),
        (0, segment_bytes, 0),
        (0, 2 * segment_bytes, 1),
        (0, 2 * segment_bytes + half_bytes as u64, 2),
        (0, 3 * segment_bytes, 0),
        (1, 0, 0),
        (1, segment_bytes, 3),
        (1, segment_bytes + half_bytes as u64, 4),
        (1, 2 * segment_bytes, 0),
    ] {
        let mut buffer = alignment.allocate(half_bytes, ()).unwrap();
        assert_eq!(
            files[file].read_at(buffer.as_mut_slice(), offset).unwrap(),
            half_bytes
        );
        assert!(buffer.as_slice().iter().all(|&byte| byte == value));
        assert_eq!(files[file].metadata().unwrap().len(), file_bytes);
    }
    // External truncation violates the startup contract but must still fail closed.
    files[1].set_len(segment_bytes).unwrap();
    let read = slab.read(
        &reactor,
        page_alloc::Extent::new(segment_bytes, segment_bytes as usize).unwrap(),
        slab.allocate(segment_bytes as usize, ()).unwrap(),
        segments.lease(SegmentId(1), Generation(1)).unwrap(),
        &TestScope,
    );
    assert!(matches!(
        drive_real(&reactor, read),
        Err(TestError::Alloc(Error::Io))
    ));
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(slab.writes_in_flight(), 0);
}

/// Failed binding retains the open file but never grants admission to the reactor.
#[test]
fn unbound_and_failed_binding_reject_reads_and_writes_before_reactor_admission() {
    let directory = Directory::new();
    let Some(alignment) = probe_alignment(&directory) else {
        return;
    };
    let (_, segment_bytes) = device_placement_sizes(alignment);
    #[cfg(feature = "simulation")]
    let failures = ["unbound", "wrong-geometry", "frozen-table"].as_slice();
    #[cfg(not(feature = "simulation"))]
    let failures = ["wrong-geometry", "frozen-table"].as_slice();
    for &failure in failures {
        let slab = Slab::<()>::new(
            directory.0.join(failure),
            2 * segment_bytes,
            segment_bytes,
            segment_bytes as usize,
        );
        let correct = Segments::new(segment_bytes);
        match failure {
            #[cfg(feature = "simulation")]
            "unbound" => {
                assert_eq!(slab.open_now(), Ok(alignment));
            }
            "wrong-geometry" => {
                let result = slab.open_configured(&Segments::new(2 * segment_bytes));
                assert_eq!(result, Err(Error::InvalidConfiguration));
            }
            "frozen-table" => {
                let frozen = correct.freeze().unwrap();
                let result = slab.open_configured(&correct);
                assert_eq!(result, Err(Error::Busy));
                drop(frozen);
            }
            _ => unreachable!(),
        }
        assert_eq!(slab.alignment(), Ok(alignment));
        let correct = Segments::from_geometry(slab.geometry().unwrap()).unwrap();
        let (lease, extent) = correct.append(segment_bytes as usize).unwrap();
        let buffer = slab.allocate(extent.length(), ()).unwrap();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let mut read = slab.read(&reactor, extent, buffer, lease, &TestScope);
        assert!(matches!(
            poll(&mut read),
            Poll::Ready(Err(TestError::Alloc(Error::Unavailable)))
        ));
        drop(read);
        let mut write = slab.write(
            &reactor,
            extent,
            slab.allocate(extent.length(), ()).unwrap(),
            correct.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        );
        assert!(matches!(
            poll(&mut write),
            Poll::Ready(Err(TestError::Alloc(Error::Unavailable)))
        ));
        drop(write);
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(slab.writes_in_flight(), 0);
        assert_eq!(slab.open_configured(&correct), Ok(alignment));
        // Acquiring the same binding again proves recovery is stable without kernel admission.
        assert_eq!(slab.open_configured(&correct), Ok(alignment));
        // The successful roundtrip workflow separately proves bound admission and completion.
    }
}

/// Partial tables preserve file geometry, lease-fenced reuse, and unchanged disk bytes.
#[cfg(feature = "simulation")]
#[test]
fn partial_table_reclamation_changes_authority_without_erasing_storage() {
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    let slab = Slab::<()>::new("/alloc-workflows/partial".into(), 16384, 4096, 512);
    let alignment = slab.open_now().unwrap();
    let physical = slab.geometry().unwrap();
    let partial = page_alloc::SegmentGeometry::new(16384, 4096, 2, alignment).unwrap();
    let segments = Rc::new(Segments::from_geometry(partial).unwrap());
    assert_eq!(slab.open_configured(&segments), Ok(alignment));
    assert_eq!(slab.geometry().unwrap(), physical);
    assert_eq!(physical.segment_count(), 4);
    assert_eq!(segments.count(), 2);
    assert_eq!(segments.geometry(), Some(partial));

    let (lease, extent) = segments.append(4096).unwrap();
    let original_generation = lease.generation();
    let mut buffer = slab.allocate(4096, ()).unwrap();
    buffer.as_mut_slice().fill(91);
    drop(
        drive(
            &reactor,
            slab.write(&reactor, extent, buffer, lease, &TestScope),
        )
        .unwrap(),
    );
    assert_eq!(segments.free_count(), 1);

    /// One caller mapping whose removal is observable independently of disk bytes.
    struct Entries {
        populated: Cell<bool>,

        removals: Cell<usize>,
    }

    impl page_alloc::SegmentEntries for Entries {
        /// Forget the mapping only for its actual segment and a positive budget.
        fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize {
            if segment == SegmentId(0) && budget != 0 && self.populated.replace(false) {
                self.removals.set(self.removals.get() + 1);
                1
            } else {
                0
            }
        }

        /// Report current caller ownership rather than allocator occupancy.
        fn is_empty(&self, segment: SegmentId) -> bool {
            segment != SegmentId(0) || !self.populated.get()
        }
    }
    let entries = Entries {
        populated: Cell::new(true),
        removals: Cell::new(0),
    };
    let clock = page_alloc::SegmentClock::new(segments.clone());
    let held = segments.lease(SegmentId(0), original_generation).unwrap();
    assert_eq!(clock.reclaim(&entries, 2, 4, 1), Err(Error::Busy));
    assert_eq!(entries.removals.get(), 1);
    assert!(!entries.populated.get());
    assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
    assert!(matches!(
        segments.lease(SegmentId(0), original_generation),
        Err(Error::Stale)
    ));

    // Existing completion authority still reads bytes after caller index removal.
    let buffer = drive(
        &reactor,
        slab.read(
            &reactor,
            extent,
            slab.allocate(4096, ()).unwrap(),
            held,
            &TestScope,
        ),
    )
    .unwrap();
    assert!(buffer.as_slice().iter().all(|byte| *byte == 91));
    drop(buffer);
    clock.reclaim(&entries, 2, 4, 0).unwrap();
    assert_eq!(segments.free_count(), 2);
    assert_eq!(entries.removals.get(), 1);
    assert_eq!(segments.snapshot()[0].generation, Generation(2));

    // Reuse grants new generation authority, but does not mutate the stored bytes.
    let (replacement, replacement_extent) = segments.append(4096).unwrap();
    assert_eq!(replacement.id(), SegmentId(0));
    assert_eq!(replacement.generation(), Generation(2));
    assert_eq!(replacement_extent, extent);
    assert_eq!(
        segments.validate(SegmentId(0), original_generation, &extent),
        Err(Error::Stale)
    );
    let buffer = drive(
        &reactor,
        slab.read(
            &reactor,
            replacement_extent,
            slab.allocate(4096, ()).unwrap(),
            replacement,
            &TestScope,
        ),
    )
    .unwrap();
    assert!(buffer.as_slice().iter().all(|byte| *byte == 91));
    drop(buffer);
    assert_eq!(slab.writes_in_flight(), 0);
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(slab.geometry().unwrap(), physical);
    let image = segments.snapshot();
    assert_eq!(image.len(), 2);
    assert_eq!(image[0].state, SegmentState::Sealed);
    assert_eq!(image[0].used_bytes, 4096);
    assert_eq!(image[0].generation, Generation(2));
    assert_eq!(image[1].state, SegmentState::Free);
    assert_eq!(image[1].used_bytes, 0);
    assert_eq!(image[1].generation, Generation(1));
    assert_eq!(entries.removals.get(), 1);
    assert_eq!(segments.free_count(), 1);
}
