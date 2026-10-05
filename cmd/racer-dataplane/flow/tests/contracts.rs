//! Public ownership and capacity contracts exercised without private-state access.

/// Reservation lifetime and target selection contracts.
mod handoff_tests {
    use flow_control::{Error, Handoff, HandoffAdmission, Result};
    use std::{
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        task::Waker,
    };

    /// One-slot admission used to expose early reservation release.
    struct Quota(Arc<AtomicUsize>);
    /// A counted reservation released only when its owner drops.
    struct Held(Arc<AtomicUsize>);
    impl Drop for Held {
        /// Return the fixture's single slot.
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::SeqCst);
        }
    }
    impl HandoffAdmission for Quota {
        type Reservation = Held;

        /// This synchronous fixture does not arrange wakeups.
        fn register(&self, _: &Waker) {}

        /// Admit only when the fixture's slot is empty.
        fn reserve(&self) -> Result<Held> {
            self.0
                .compare_exchange(0, 1, Ordering::SeqCst, Ordering::SeqCst)
                .map_err(|_| Error::Overloaded)?;
            Ok(Held(self.0.clone()))
        }
    }
    /// Offers, envelopes, and popped items retain the selected target's slot.
    #[test]
    fn round_robin_reserves_before_delivery_and_releases_after_close() {
        let handoff = Arc::new(Handoff::<_, _, u8>::new(&[1, 2]));
        let counts = [Arc::new(AtomicUsize::new(0)), Arc::new(AtomicUsize::new(0))];
        let waker = Waker::noop();
        assert!(matches!(handoff.reserve(waker), Err(Error::Overloaded)));
        for (key, count) in [1, 2].into_iter().zip(&counts) {
            handoff.install(&key, Quota(count.clone())).unwrap();
        }
        assert_eq!(
            handoff.install(&1, Quota(counts[0].clone())),
            Err(Error::InvalidInput)
        );
        let first = handoff.reserve(waker).unwrap();
        let second = handoff.reserve(waker).unwrap();
        assert!(matches!(handoff.reserve(waker), Err(Error::Overloaded)));
        first.deliver(|| 7).unwrap();
        assert!(
            handoff
                .pop_batch::<2>(&1, waker, 0)
                .unwrap()
                .iter()
                .all(Option::is_none)
        );
        let [item, empty] = handoff.pop_batch::<2>(&1, waker, 1).unwrap();
        let (payload, held) = item.unwrap().into_parts();
        assert_eq!(payload, 7);
        assert!(empty.is_none());
        handoff.close(&1);
        assert_eq!(counts[0].load(Ordering::SeqCst), 1);
        drop(held);
        assert_eq!(counts[0].load(Ordering::SeqCst), 0);
        handoff.close(&2);
        assert_eq!(
            second.deliver(|| panic!("closed target built item")),
            Err(Error::Unavailable)
        );
        assert_eq!(counts[1].load(Ordering::SeqCst), 0);
        assert!(matches!(
            handoff.pop_batch::<1>(&3, waker, 1),
            Err(Error::InvalidInput)
        ));
        assert!(matches!(
            Arc::new(Handoff::<u8, Quota, ()>::new(&[])).reserve(waker),
            Err(Error::Overloaded)
        ));
    }
    /// Closing and abandoning work release exactly the retained reservations.
    #[test]
    fn close_drains_queued_reservations_and_abandoned_offer_releases() {
        let handoff = Arc::new(Handoff::<_, _, ()>::new(&[1]));
        let count = Arc::new(AtomicUsize::new(0));
        handoff.install(&1, Quota(count.clone())).unwrap();
        drop(handoff.reserve(Waker::noop()).unwrap());
        assert_eq!(count.load(Ordering::SeqCst), 0);
        handoff
            .reserve(Waker::noop())
            .unwrap()
            .deliver(|| ())
            .unwrap();
        handoff.close(&1);
        handoff.close(&1);
        handoff.close(&2);
        assert_eq!(count.load(Ordering::SeqCst), 0);
        assert_eq!(handoff.install(&1, Quota(count)), Err(Error::InvalidInput));
    }
    /// Payload-only construction cannot discard admission before dequeue.
    #[test]
    fn envelope_retains_reservation_through_queue_pop_and_owner_transfer() {
        let handoff = Arc::new(Handoff::<_, _, u8>::new(&[1]));
        let count = Arc::new(AtomicUsize::new(0));
        handoff.install(&1, Quota(count.clone())).unwrap();
        handoff
            .reserve(Waker::noop())
            .unwrap()
            .deliver(|| 9)
            .unwrap();
        assert!(matches!(
            handoff.reserve(Waker::noop()),
            Err(Error::Overloaded)
        ));
        let [item] = handoff.pop_batch::<1>(&1, Waker::noop(), 1).unwrap();
        assert!(matches!(
            handoff.reserve(Waker::noop()),
            Err(Error::Overloaded)
        ));
        let (payload, held) = item.unwrap().into_parts();
        assert_eq!(payload, 9);
        assert_eq!(count.load(Ordering::SeqCst), 1);
        drop(held);
        assert!(handoff.reserve(Waker::noop()).is_ok());
    }
}

/// Fixed backing and admission transfer contracts.
mod buffer_tests {
    use flow_control::{ChargedBuffer, Error, Policy, Quotas, Rejection};
    /// Single fixture class for fixed backing.
    #[derive(Clone, Copy)]
    struct Class;
    impl flow_control::Class for Class {
        const COUNT: usize = 1;

        /// Select the fixture's sole counter.
        fn index(self) -> usize {
            0
        }
    }
    /// Fixed four-MiB backing budget without wake or page coverage policy.
    struct TestPolicy;
    impl Policy for TestPolicy {
        type Class = Class;

        type Key = ();

        /// Allow four MiB of fixed backing.
        fn limit(&self, _: Class) -> usize {
            4 * 1024 * 1024
        }

        /// Permit one fixture key.
        fn max_keys(&self) -> usize {
            1
        }

        /// Buffer releases do not wake this synchronous fixture.
        fn wakes(_: Class) -> bool {
            false
        }

        /// This fixture does not admit page allocator backing.
        fn covers(_: Class) -> bool {
            false
        }

        /// Rejection details are irrelevant to these backing assertions.
        fn rejected(&self, _: Rejection<Class>) {}
    }
    /// Moves, reuse, and final transfer preserve backing and its exact charge.
    #[test]
    fn fixed_backing_recycles_and_transfers_charge_without_early_release() {
        let quotas = Quotas::new(TestPolicy);
        let length = 1024 * 1024;
        let mut buffer =
            ChargedBuffer::new(quotas.reserve(None, Class, 2 * length).unwrap(), length).unwrap();
        assert_eq!(quotas.used(Class), length);
        assert_eq!(buffer.charge().amount(), length);
        let pointer = buffer.bytes().as_ptr();
        buffer.bytes_mut().fill(0xa7);
        let moved = buffer;
        assert_eq!(moved.bytes().as_ptr(), pointer);
        drop(moved);
        assert_eq!(quotas.used(Class), length);
        let buffer =
            ChargedBuffer::new(quotas.reserve(None, Class, length).unwrap(), length).unwrap();
        assert_eq!(buffer.bytes().as_ptr(), pointer);
        assert!(buffer.bytes().iter().all(|byte| *byte == 0));
        let (bytes, charge) = buffer.into_parts();
        assert_eq!(bytes.as_ptr(), pointer);
        assert_eq!(quotas.used(Class), length);
        drop((bytes, charge));
        assert_eq!(quotas.used(Class), 0);
    }
    /// Invalid backing geometry releases the consumed reservation.
    #[test]
    fn invalid_lengths_return_charge() {
        let quotas = Quotas::new(TestPolicy);
        for length in [0, 9] {
            assert!(matches!(
                ChargedBuffer::new(quotas.reserve(None, Class, 8).unwrap(), length),
                Err(Error::InvalidInput)
            ));
            assert_eq!(quotas.used(Class), 0);
        }
    }
}

/// Credit-window transition and full-width arithmetic contracts.
mod window_tests {
    use flow_control::{Error, Window};
    /// Invalid geometry never consumes credit.
    #[test]
    fn validates_configuration_and_item_lengths_without_consuming_credit() {
        for (slots, bytes, max_item) in [(0, 1, 1), (1, 0, 1), (1, 1, 0)] {
            assert!(matches!(
                Window::<String>::new(slots, bytes, max_item),
                Err(Error::InvalidInput)
            ));
        }
        let mut window = Window::new(2, 10, 6).unwrap();
        for length in [0, 7, u64::MAX] {
            assert!(!window.can_reserve(length));
            assert_eq!(window.reserve("item", length), Err(Error::InvalidInput));
            assert!(window.is_empty());
        }
        window.reserve("item", 6).unwrap();
        assert!(!window.is_empty());
        assert!(window.can_reserve(4));
        assert!(!window.can_reserve(5));
    }
    /// Only one exact acknowledgment of an issued item releases capacity.
    #[test]
    fn pending_and_issued_share_limits_and_release_exactly_once() {
        let mut window = Window::new(2, 10, 10).unwrap();
        window.reserve(String::from("first"), 4).unwrap();
        assert_eq!(window.release("first".into(), 4), Err(Error::InvalidInput));
        window.issued("first".into()).unwrap();
        assert_eq!(window.issued("first".into()), Err(Error::InvalidInput));
        assert_eq!(window.reserve("first".into(), 1), Err(Error::Overloaded));
        window.reserve("second".into(), 6).unwrap();
        assert!(!window.can_reserve(1));
        assert_eq!(window.reserve("third".into(), 1), Err(Error::Overloaded));
        assert_eq!(window.release("first".into(), 3), Err(Error::InvalidInput));
        assert_eq!(window.release("first".into(), 5), Err(Error::InvalidInput));
        assert_eq!(
            window.release("missing".into(), 4),
            Err(Error::InvalidInput)
        );
        assert_eq!(window.issued("missing".into()), Err(Error::InvalidInput));
        assert!(!window.can_reserve(1));
        window.release("first".into(), 4).unwrap();
        assert_eq!(window.release("first".into(), 4), Err(Error::InvalidInput));
        assert!(window.can_reserve(4));
        assert!(!window.can_reserve(5));
        window.issued("second".into()).unwrap();
        window.release("second".into(), 6).unwrap();
        assert!(window.is_empty());
        window.reserve("first".into(), 10).unwrap();
        window.issued("first".into()).unwrap();
        window.release("first".into(), 10).unwrap();
        assert!(window.is_empty());
    }
    /// Neither issuance nor oversized item ceilings weaken independent budgets.
    #[test]
    fn slot_and_byte_limits_are_independent() {
        let mut slots = Window::new(1, 10, 10).unwrap();
        slots.reserve(1, 1).unwrap();
        assert!(!slots.can_reserve(1));
        assert_eq!(slots.reserve(2, 1), Err(Error::Overloaded));
        slots.issued(1).unwrap();
        assert!(!slots.can_reserve(1));
        slots.release(1, 1).unwrap();
        assert!(slots.can_reserve(10));
        let mut bytes = Window::new(100, 3, 10).unwrap();
        assert_eq!(bytes.reserve(1, 4), Err(Error::Overloaded));
        assert!(bytes.is_empty());
        bytes.reserve(1, 2).unwrap();
        assert!(bytes.can_reserve(1));
        assert_eq!(bytes.reserve(2, 2), Err(Error::Overloaded));
        bytes.reserve(2, 1).unwrap();
        assert!(!bytes.can_reserve(1));
    }
    /// Full-width arithmetic works without requiring cloneable keys.
    #[test]
    fn full_u64_budget_does_not_overflow_and_generic_keys_need_only_ord() {
        /// Ordered fixture identity deliberately lacking Clone.
        #[derive(Eq, PartialEq, Ord, PartialOrd)]
        struct Key(u8);
        let mut window = Window::new(usize::MAX, u64::MAX, u64::MAX).unwrap();
        window.reserve(Key(1), u64::MAX - 1).unwrap();
        window.reserve(Key(2), 1).unwrap();
        assert!(!window.can_reserve(1));
        assert_eq!(window.reserve(Key(3), 1), Err(Error::Overloaded));
        window.issued(Key(1)).unwrap();
        window.release(Key(1), u64::MAX - 1).unwrap();
        assert!(window.can_reserve(u64::MAX - 1));
        assert!(!window.can_reserve(u64::MAX));
        window.issued(Key(2)).unwrap();
        window.release(Key(2), 1).unwrap();
        assert!(window.is_empty());
        assert!(window.can_reserve(u64::MAX));
    }
}
