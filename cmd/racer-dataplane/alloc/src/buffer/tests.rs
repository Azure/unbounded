use super::*;
use std::cell::Cell;

// Deliberately not Debug: accounting implementations need not expose internals.
struct TrackedCharge {
    live: Rc<Cell<usize>>,
    bytes: usize,
}
impl TrackedCharge {
    fn new(live: &Rc<Cell<usize>>, bytes: usize) -> Self {
        live.set(live.get() + bytes);
        Self {
            live: live.clone(),
            bytes,
        }
    }
}
impl Charge for TrackedCharge {
    fn covers(&self, bytes: usize) -> bool {
        self.bytes >= bytes
    }
}
impl Drop for TrackedCharge {
    fn drop(&mut self) {
        self.live.set(self.live.get() - self.bytes);
    }
}

#[test]
fn transfer_limit_is_enforced_before_allocation_or_charge_inspection() {
    struct UncheckedCharge;
    impl Charge for UncheckedCharge {
        fn covers(&self, _: usize) -> bool {
            panic!("invalid lengths must be rejected before inspecting accounting");
        }
    }

    let alignment = Alignment::new(1, 1, 1).unwrap();
    let max = Alignment::MAX_TRANSFER_LENGTH;
    assert_eq!(Extent::new(0, max).unwrap().length(), max);
    assert_eq!(alignment.extent(0, max).unwrap().length(), max);
    for length in [0, max + 1, i32::MAX as usize, u32::MAX as usize, usize::MAX] {
        assert_eq!(Extent::new(0, length), Err(Error::Corrupt));
        assert_eq!(
            alignment.extent(0, length),
            Err(Error::InvalidConfiguration)
        );
        assert!(matches!(
            alignment.allocate(length, UncheckedCharge),
            Err(Error::InvalidConfiguration)
        ));
    }
    let alignment = Alignment::new(8, 3, 5).unwrap();
    let rounded_limit = max / 15 * 15;
    assert_eq!(
        alignment.extent(0, rounded_limit).unwrap().length(),
        rounded_limit
    );
    assert_eq!(
        alignment.extent(0, rounded_limit + 1),
        Err(Error::InvalidConfiguration)
    );
    // A valid length can still overflow the file endpoint.
    assert_eq!(Extent::new(u64::MAX, 1), Err(Error::Corrupt));
    assert_eq!(alignment.extent(u64::MAX, 1), Err(Error::Corrupt));
    assert_eq!(Extent::new(u64::MAX - 1, 1).unwrap().offset(), u64::MAX - 1);
}

#[test]
fn arbitrary_units_preserve_lcm_rounding_and_detect_overflow() {
    for (offset_unit, length_unit, lcm) in [(3, 5, 15), (6, 9, 18), (768, 512, 1536)] {
        let alignment = Alignment::new(64, offset_unit, length_unit).unwrap();
        assert_eq!(alignment.memory(), 64);
        assert_eq!(alignment.offset(), offset_unit);
        assert_eq!(alignment.length(), length_unit);
        for (logical, expected) in [(1, lcm), (lcm, lcm), (lcm + 1, lcm * 2)] {
            let extent = alignment.extent(offset_unit, logical).unwrap();
            assert_eq!(extent.offset(), offset_unit);
            assert_eq!(extent.length(), expected);
            let buffer = alignment.allocate(expected, ()).unwrap();
            alignment.check(extent, &buffer).unwrap();
        }
        assert_eq!(alignment.extent(1, 1), Err(Error::InvalidConfiguration));
    }
    for alignment in [
        Alignment::new(1, u64::MAX, usize::MAX - 1).unwrap(),
        Alignment::new(1, 1, usize::MAX).unwrap(),
        Alignment::new(1, 2, usize::MAX).unwrap(),
    ] {
        assert_eq!(alignment.extent(0, 2), Err(Error::InvalidConfiguration));
    }
    for (memory, offset, length) in [(0, 1, 1), (3, 1, 1), (1, 0, 1), (1, 1, 0)] {
        assert_eq!(
            Alignment::new(memory, offset, length),
            Err(Error::Unsupported)
        );
    }
    assert_eq!(
        Alignment::new(1usize << (usize::BITS - 1), 1, 1),
        Err(Error::Unsupported)
    );
}

#[test]
fn initialized_storage_remains_stable_across_moves_and_trait_access() {
    let alignment = Alignment::new(64, 3, 5).unwrap();
    let mut buffer = alignment.allocate(15, ()).unwrap();
    assert!(!buffer.is_empty());
    assert_eq!(buffer.len(), 15);
    assert_eq!(buffer.as_slice(), &[0; 15]);
    let pointer = IoBuffer::bytes_mut(&mut buffer).unwrap().as_mut_ptr();
    assert!((pointer as usize).is_multiple_of(64));
    let moved = std::hint::black_box(Some(buffer));
    // SAFETY: moving the owner must not invalidate the exclusively borrowed raw
    // pointer. No accessor reborrows the allocation between derivation and use.
    unsafe { pointer.write(42) };
    let mut buffer = moved.unwrap();
    assert_eq!(IoBuffer::bytes(&buffer).unwrap()[0], 42);
    buffer.bytes_mut().unwrap()[1] = 17;
    assert_eq!(buffer.bytes().unwrap()[1], 17);
    assert_eq!(buffer.as_slice().as_ptr(), pointer);
    alignment
        .check(Extent::new(3, 15).unwrap(), &buffer)
        .unwrap();
    for extent in [Extent::new(1, 15).unwrap(), Extent::new(3, 10).unwrap()] {
        assert_eq!(
            alignment.check(extent, &buffer),
            Err(Error::InvalidConfiguration)
        );
    }
    assert_eq!(
        Alignment::new(64, 3, 2)
            .unwrap()
            .check(Extent::new(3, 15).unwrap(), &buffer),
        Err(Error::InvalidConfiguration)
    );
}

#[test]
fn charge_validation_rebinding_and_debug_do_not_require_charge_debug() {
    let live = Rc::new(Cell::new(0));
    let alignment = Alignment::new(8, 3, 5).unwrap();
    for (length, charge) in [
        (15, 14),
        (14, 15),
        (0, 15),
        (Alignment::MAX_TRANSFER_LENGTH + 1, 15),
    ] {
        assert_eq!(
            alignment
                .allocate(length, TrackedCharge::new(&live, charge))
                .unwrap_err(),
            Error::InvalidConfiguration
        );
        assert_eq!(live.get(), 0);
    }
    let mut buffer = alignment
        .allocate(15, TrackedCharge::new(&live, 15))
        .unwrap();
    assert_eq!(
        buffer.rebind(TrackedCharge::new(&live, 14)),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(live.get(), 15);
    buffer.rebind(TrackedCharge::new(&live, 20)).unwrap();
    assert_eq!(live.get(), 20);
    let debug = format!("{buffer:?}");
    assert!(debug.contains("length: 15"));
    assert!(debug.contains("alignment: 8"));
    assert!(!debug.contains("TrackedCharge"));
    drop(buffer);
    assert_eq!(live.get(), 0);
}

#[test]
fn pool_transfers_allocation_and_primary_charge_but_not_retained_guards() {
    let live = Rc::new(Cell::new(0));
    let pool = Rc::new(RefCell::new(None));
    let alignment = Alignment::new(64, 3, 5).unwrap();
    let mut buffer = alignment
        .allocate(15, TrackedCharge::new(&live, 15))
        .unwrap()
        .pooled(&pool);
    let pointer = buffer.as_slice().as_ptr();
    buffer.as_mut_slice().fill(42);
    let extra = Rc::new(TrackedCharge::new(&live, 7));
    let weak = Rc::downgrade(&extra);
    buffer.retain(extra);
    assert_eq!(live.get(), 22);
    drop(buffer);
    assert_eq!(live.get(), 15);
    assert!(weak.upgrade().is_none());
    let mut reused = pool.borrow_mut().take().unwrap();
    assert_eq!(reused.as_slice().as_ptr(), pointer);
    assert_eq!(reused.as_slice(), &[0; 15]);
    reused.rebind(TrackedCharge::new(&live, 20)).unwrap();
    assert_eq!(live.get(), 20);
    drop(reused.pooled(&pool));
    // Removing the pool must release idle storage without recursive repooling.
    drop(pool);
    assert_eq!(live.get(), 0);
}

#[test]
fn unavailable_borrowed_and_occupied_pools_release_exactly_once() {
    let alignment = Alignment::new(8, 1, 1).unwrap();
    for scenario in 0..4 {
        let live = Rc::new(Cell::new(0));
        let pool = Rc::new(RefCell::new(None));
        let mut buffer = alignment
            .allocate(8, TrackedCharge::new(&live, 8))
            .unwrap()
            .pooled(&pool);
        buffer.retain(Rc::new(TrackedCharge::new(&live, 3)));
        match scenario {
            0 => {
                drop(pool);
                drop(buffer);
            }
            1 => {
                let borrow = pool.borrow();
                drop(buffer);
                assert_eq!(live.get(), 0);
                assert!(borrow.is_none());
            }
            2 => {
                let borrow = pool.borrow_mut();
                drop(buffer);
                assert_eq!(live.get(), 0);
                assert!(borrow.is_none());
            }
            _ => {
                let idle = alignment.allocate(8, TrackedCharge::new(&live, 8)).unwrap();
                let pointer = idle.as_slice().as_ptr();
                *pool.borrow_mut() = Some(idle);
                drop(buffer);
                assert_eq!(live.get(), 8);
                assert_eq!(pool.borrow().as_ref().unwrap().as_slice().as_ptr(), pointer);
                drop(pool);
            }
        }
        assert_eq!(live.get(), 0);
    }
}

#[test]
fn retained_guard_can_inspect_pool_before_buffer_is_returned() {
    struct Guard(Option<Box<dyn FnOnce()>>);
    impl Charge for Guard {
        fn covers(&self, _: usize) -> bool {
            true
        }
    }
    impl Drop for Guard {
        fn drop(&mut self) {
            if let Some(callback) = self.0.take() {
                callback();
            }
        }
    }
    let pool = Rc::new(RefCell::new(None));
    let called = Rc::new(Cell::new(false));
    let mut buffer = Alignment::new(8, 1, 1)
        .unwrap()
        .allocate(8, Guard(None))
        .unwrap()
        .pooled(&pool);
    let observed_pool = pool.clone();
    let observed_called = called.clone();
    buffer.retain(Rc::new(Guard(Some(Box::new(move || {
        assert!(observed_pool.borrow_mut().is_none());
        observed_called.set(true);
    })))));
    drop(buffer);
    assert!(called.get());
    assert!(pool.borrow().is_some());
}

#[test]
fn retained_guard_unwind_still_drops_owned_allocation_and_primary_charge() {
    struct Guard {
        live: Rc<Cell<usize>>,
        panic: bool,
    }
    impl Charge for Guard {
        fn covers(&self, _: usize) -> bool {
            true
        }
    }
    impl Drop for Guard {
        fn drop(&mut self) {
            self.live.set(self.live.get() - 1);
            assert!(!self.panic, "injected retained guard panic");
        }
    }
    let live = Rc::new(Cell::new(2));
    let pool = Rc::new(RefCell::new(None));
    let mut buffer = Alignment::new(8, 1, 1)
        .unwrap()
        .allocate(
            8,
            Guard {
                live: live.clone(),
                panic: false,
            },
        )
        .unwrap()
        .pooled(&pool);
    buffer.retain(Rc::new(Guard {
        live: live.clone(),
        panic: true,
    }));
    assert!(std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| drop(buffer))).is_err());
    assert_eq!(live.get(), 0);
    assert!(pool.borrow().is_none());
}
