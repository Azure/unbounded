//! Public ownership, capacity, and coalescing workflows without private-state access.

/// Reservation lifetime and target selection contracts.
mod handoff_tests {
    use flow_control::{Error, Handoff, HandoffAdmission, Result};
    use std::{
        sync::{
            Arc, Mutex,
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
        /// Slot owner carried alongside every offered item.
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

    struct ScriptedAdmission {
        key: usize,
        result: Result<usize>,
        registered: AtomicUsize,
        calls: Arc<Mutex<Vec<usize>>>,
    }

    impl HandoffAdmission for ScriptedAdmission {
        type Reservation = usize;

        fn register(&self, _: &Waker) {
            self.registered.fetch_add(1, Ordering::SeqCst);
        }

        fn reserve(&self) -> Result<usize> {
            assert_eq!(self.registered.swap(0, Ordering::SeqCst), 1);
            self.calls.lock().unwrap().push(self.key);
            self.result
        }
    }

    fn scripted_handoff(
        results: &[Result<usize>],
        calls: &Arc<Mutex<Vec<usize>>>,
    ) -> Arc<Handoff<usize, ScriptedAdmission, ()>> {
        let keys: Vec<_> = (0..results.len()).collect();
        let handoff = Arc::new(Handoff::new(&keys));
        for (key, result) in results.iter().copied().enumerate() {
            handoff
                .install(
                    &key,
                    ScriptedAdmission {
                        key,
                        result,
                        registered: AtomicUsize::new(0),
                        calls: calls.clone(),
                    },
                )
                .unwrap();
        }
        handoff
    }

    #[test]
    fn admission_failures_preserve_classification_and_capacity_retry() {
        use Error::{Overloaded, Unavailable};
        for (errors, expected) in [
            (vec![Overloaded], Overloaded),
            (vec![Unavailable], Unavailable),
            (vec![Unavailable, Unavailable], Unavailable),
            (vec![Overloaded, Overloaded], Overloaded),
            (vec![Overloaded, Unavailable], Overloaded),
            (vec![Unavailable, Overloaded], Overloaded),
        ] {
            let calls = Arc::new(Mutex::new(Vec::new()));
            let results: Vec<_> = errors.iter().copied().map(Err).collect();
            let handoff = scripted_handoff(&results, &calls);
            assert_eq!(handoff.reserve(Waker::noop()).err(), Some(expected));
            assert_eq!(
                *calls.lock().unwrap(),
                (0..errors.len()).collect::<Vec<_>>()
            );
        }
    }

    #[test]
    fn fatal_admission_errors_stop_scanning_without_becoming_overload() {
        for error in [Error::InvalidInput, Error::Io] {
            for preceding in [None, Some(Error::Overloaded), Some(Error::Unavailable)] {
                let calls = Arc::new(Mutex::new(Vec::new()));
                let mut results: Vec<_> = preceding.into_iter().map(Err).collect();
                results.extend([Err(error), Ok(99)]);
                let handoff = scripted_handoff(&results, &calls);
                assert_eq!(handoff.reserve(Waker::noop()).err(), Some(error));
                assert_eq!(
                    *calls.lock().unwrap(),
                    (0..results.len() - 1).collect::<Vec<_>>()
                );
            }
        }
    }

    #[test]
    fn successful_admission_skips_retryable_failures_and_keeps_round_robin() {
        for failures in [
            [Error::Overloaded, Error::Unavailable],
            [Error::Unavailable, Error::Overloaded],
        ] {
            let calls = Arc::new(Mutex::new(Vec::new()));
            let handoff = scripted_handoff(
                &[Err(failures[0]), Err(failures[1]), Ok(12), Ok(13)],
                &calls,
            );
            for target in [2, 3, 2] {
                handoff
                    .reserve(Waker::noop())
                    .unwrap()
                    .deliver(|| ())
                    .unwrap();
                let [item] = handoff.pop_batch::<1>(&target, Waker::noop(), 1).unwrap();
                assert_eq!(item.unwrap().into_parts(), ((), target + 10));
            }
            assert_eq!(*calls.lock().unwrap(), [0, 1, 2, 3, 0, 1, 2]);
        }
    }

    #[test]
    fn closed_and_uninstalled_targets_do_not_change_admission_errors() {
        let calls = Arc::new(Mutex::new(Vec::new()));
        let handoff = scripted_handoff(&[Err(Error::Io), Err(Error::Unavailable)], &calls);
        handoff.close(&0);
        assert_eq!(
            handoff.reserve(Waker::noop()).err(),
            Some(Error::Unavailable)
        );
        assert_eq!(*calls.lock().unwrap(), [1]);
        handoff.close(&1);
        assert_eq!(
            handoff.reserve(Waker::noop()).err(),
            Some(Error::Overloaded)
        );
        assert_eq!(*calls.lock().unwrap(), [1]);

        let handoff = Arc::new(Handoff::<_, _, ()>::new(&[0, 1]));
        handoff
            .install(
                &1,
                ScriptedAdmission {
                    key: 1,
                    result: Err(Error::Unavailable),
                    registered: AtomicUsize::new(0),
                    calls: calls.clone(),
                },
            )
            .unwrap();
        assert_eq!(
            handoff.reserve(Waker::noop()).err(),
            Some(Error::Unavailable)
        );
        assert_eq!(*calls.lock().unwrap(), [1, 1]);
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

/// Quota transfers, retained key lifetimes, and exact release notifications.
mod quota_tests {
    use flow_control::{Charge, Error, Policy, Quotas, Rejection, SharedQuotas};
    use std::{
        sync::{
            Arc, Mutex,
            atomic::{AtomicUsize, Ordering},
        },
        task::{Wake, Waker},
    };

    /// Independent wake-enabled and silent resource classes.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum Resource {
        Waking,

        Silent,
    }

    impl flow_control::Class for Resource {
        /// Number of independently accounted fixture classes.
        const COUNT: usize = 2;

        /// Select this resource's independently padded counter.
        fn index(self) -> usize {
            self as usize
        }
    }

    /// Configurable limits with ordered rejection observations.
    #[derive(Clone)]
    struct TestPolicy {
        limit: usize,

        max_keys: usize,

        rejected: Arc<Mutex<Vec<Rejection<Resource>>>>,
    }

    impl TestPolicy {
        /// Start a fixture with identical class limits and an empty event log.
        fn new(limit: usize, max_keys: usize) -> Self {
            Self {
                limit,
                max_keys,
                rejected: Arc::default(),
            }
        }
    }

    impl Policy for TestPolicy {
        /// Resource classes for wake and accounting assertions.
        type Class = Resource;

        /// Small identities make key retirement observable through admission.
        type Key = u8;

        /// Return the fixture's ceiling for either resource class.
        fn limit(&self, _: Resource) -> usize {
            self.limit
        }

        /// Bound simultaneously retained key records.
        fn max_keys(&self) -> usize {
            self.max_keys
        }

        /// Only the waking class notifies admission waiters on drop.
        fn wakes(class: Resource) -> bool {
            class == Resource::Waking
        }

        /// Neither test class is used for page allocator backing.
        fn covers(_: Resource) -> bool {
            false
        }

        /// Preserve every attempted rejection, including reclamation retries.
        fn rejected(&self, rejection: Rejection<Resource>) {
            self.rejected.lock().unwrap().push(rejection);
        }
    }

    /// Count notifications independently of the worker-local quota authority.
    #[derive(Default)]
    struct WakeCount(AtomicUsize);

    impl Wake for WakeCount {
        /// Count one consumed waiter registration.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    /// Splitting is accounting-neutral; shrinking releases both budgets silently.
    #[test]
    fn keyed_split_shrink_and_cross_thread_drop_keep_exact_accounting() {
        /// Assert that local authority does not prevent transferable charges.
        fn transferable<T: Send + Sync>() {}

        transferable::<Charge<TestPolicy>>();
        transferable::<SharedQuotas<TestPolicy>>();
        let quotas = Quotas::new(TestPolicy::new(100, 1));
        let shared = quotas.shared();
        let wake = Arc::new(WakeCount::default());
        shared.register(&Waker::from(wake.clone()));
        let mut charge = quotas.reserve(Some(&1), Resource::Waking, 100).unwrap();
        for amount in [0, 100, usize::MAX] {
            assert!(matches!(charge.split(amount), Err(Error::InvalidInput)));
            assert_eq!(charge.amount(), 100);
            assert_eq!(shared.used(Resource::Waking), 100);
        }
        let split = charge.split(40).unwrap();
        assert_eq!(split.key(), Some(&1));
        assert_eq!(split.class(), Resource::Waking);
        assert!(quotas.owns(&split));
        assert_eq!(shared.used(Resource::Waking), 100);
        for amount in [0, 61, usize::MAX] {
            assert_eq!(charge.shrink(amount), Err(Error::InvalidInput));
            assert_eq!(charge.amount(), 60);
        }
        charge.shrink(60).unwrap();
        charge.shrink(20).unwrap();
        assert_eq!(shared.used(Resource::Waking), 60);
        assert_eq!(wake.0.load(Ordering::SeqCst), 0, "shrink must not wake");
        let refill = quotas.reserve(Some(&1), Resource::Waking, 40).unwrap();
        assert_eq!(shared.used(Resource::Waking), 100);
        std::thread::spawn(move || drop(split)).join().unwrap();
        assert_eq!(shared.used(Resource::Waking), 60);
        assert_eq!(wake.0.load(Ordering::SeqCst), 1);
        drop((refill, charge));
        assert_eq!(shared.used(Resource::Waking), 0);
        let next = quotas.reserve(Some(&2), Resource::Waking, 100).unwrap();
        assert_eq!(next.key(), Some(&2));
        assert!(quotas.policy().rejected.lock().unwrap().is_empty());
    }

    /// Merging is accounting-neutral and refuses charges with another key or authority.
    #[test]
    fn merge_combines_matching_charges_and_returns_mismatches() {
        let quotas = Quotas::new(TestPolicy::new(100, 2));
        let shared = quotas.shared();
        let mut charge = quotas.reserve(Some(&1), Resource::Waking, 30).unwrap();
        let extra = quotas.reserve(Some(&1), Resource::Waking, 20).unwrap();
        assert!(charge.merge(extra).is_ok());
        assert_eq!(charge.amount(), 50);
        assert_eq!(shared.used(Resource::Waking), 50);
        let other_key = quotas.reserve(Some(&2), Resource::Waking, 10).unwrap();
        let Err(other_key) = charge.merge(other_key) else {
            panic!("merged another key");
        };
        assert_eq!(other_key.amount(), 10);
        let foreign = Quotas::new(TestPolicy::new(100, 1));
        let foreign = foreign.reserve(Some(&1), Resource::Waking, 5).unwrap();
        let Err(foreign) = charge.merge(foreign) else {
            panic!("merged a foreign charge");
        };
        assert_eq!(charge.amount(), 50);
        drop((foreign, other_key));
        assert_eq!(shared.used(Resource::Waking), 50);
        drop(charge);
        assert_eq!(shared.used(Resource::Waking), 0);
    }

    /// Checked admission rejects arithmetic overflow with unchanged reported facts.
    #[test]
    fn full_width_rejections_preserve_usage_and_attempt_counts() {
        let quotas = Quotas::new(TestPolicy::new(usize::MAX, 1));
        let shared = quotas.shared();
        let mut charge = shared.reserve(Resource::Silent, usize::MAX).unwrap();
        assert!(matches!(
            shared.reserve(Resource::Silent, 1),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            quotas.reserve(None, Resource::Silent, 1),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            quotas.reserve_completion(None, Resource::Silent, 1),
            Err(Error::Overloaded)
        ));
        {
            let rejected = quotas.policy().rejected.lock().unwrap();
            assert_eq!(rejected.len(), 3, "each failed attempt is reported once");
            for event in rejected.iter() {
                assert!(matches!(
                    event,
                    Rejection::Resource {
                        class: Resource::Silent,
                        used: usize::MAX,
                        limit: usize::MAX,
                        requested: 1,
                        key_used: None,
                        key_limit: None,
                    }
                ));
            }
        }
        charge.shrink(1).unwrap();
        let split = shared.reserve(Resource::Silent, usize::MAX - 1).unwrap();
        assert_eq!(shared.used(Resource::Silent), usize::MAX);
        drop(charge);
        assert_eq!(shared.used(Resource::Silent), usize::MAX - 1);
        drop(split);
        assert_eq!(shared.used(Resource::Silent), 0);
    }

    /// A panicking key clone cannot strand fresh or transferred admission.
    #[test]
    fn key_clone_panic_keeps_split_admission_with_donor() {
        use std::{
            panic::{AssertUnwindSafe, catch_unwind},
            sync::atomic::AtomicBool,
        };

        /// Enable clone failure only for this test's otherwise ordinary key.
        static PANIC_ON_CLONE: AtomicBool = AtomicBool::new(false);

        /// One identity whose clone can fail after successful admission.
        #[derive(Eq, Hash, PartialEq)]
        struct Key;

        impl Clone for Key {
            /// Panic on demand before a new charge can own admission.
            fn clone(&self) -> Self {
                assert!(!PANIC_ON_CLONE.load(Ordering::SeqCst), "key clone failed");
                Self
            }
        }

        /// Minimal policy whose sole key can panic while transferring ownership.
        struct PanickingPolicy;

        impl Policy for PanickingPolicy {
            /// Reuse the accounting fixture's resource classes.
            type Class = Resource;

            /// Key with controllable clone failure.
            type Key = Key;

            /// Allow one retained MiB and one MiB of surplus admission.
            fn limit(&self, _: Resource) -> usize {
                2 << 20
            }

            /// Keep one key record for the donor and its possible split.
            fn max_keys(&self) -> usize {
                1
            }

            /// This test observes accounting rather than wake delivery.
            fn wakes(_: Resource) -> bool {
                false
            }

            /// No page allocator is involved in the transfer.
            fn covers(_: Resource) -> bool {
                false
            }

            /// No admission rejection is expected in this panic path.
            fn rejected(&self, _: Rejection<Resource>) {
                panic!("unexpected rejection");
            }
        }

        let size = 1 << 20;
        let quotas = Quotas::new(PanickingPolicy);
        let mut donor = quotas
            .reserve(Some(&Key), Resource::Silent, 2 * size)
            .unwrap();
        PANIC_ON_CLONE.store(true, Ordering::SeqCst);
        assert!(catch_unwind(AssertUnwindSafe(|| donor.split(size))).is_err());
        assert_eq!(donor.amount(), 2 * size);
        assert_eq!(quotas.used(Resource::Silent), 2 * size);
        assert_eq!(quotas.used(Resource::Silent), 2 * size);
        PANIC_ON_CLONE.store(false, Ordering::SeqCst);
        let split = donor.split(size).unwrap();
        drop((donor, split));
        assert_eq!(quotas.used(Resource::Silent), 0);
        let replacement = quotas
            .reserve(Some(&Key), Resource::Silent, 2 * size)
            .unwrap();
        drop(replacement);
        assert_eq!(quotas.used(Resource::Silent), 0);

        // A live key record isolates the final charge-key clone from table setup.
        // Both ordinary and drain admission must leave counters unchanged on panic.
        for completion in [false, true] {
            let donor = quotas.reserve(Some(&Key), Resource::Silent, size).unwrap();
            PANIC_ON_CLONE.store(true, Ordering::SeqCst);
            let failed = catch_unwind(AssertUnwindSafe(|| {
                if completion {
                    quotas.reserve_completion(Some(&Key), Resource::Silent, size)
                } else {
                    quotas.reserve(Some(&Key), Resource::Silent, size)
                }
            }));
            PANIC_ON_CLONE.store(false, Ordering::SeqCst);
            assert!(failed.is_err());
            assert_eq!(quotas.used(Resource::Silent), size);
            let refill = quotas.reserve(Some(&Key), Resource::Silent, size).unwrap();
            assert_eq!(quotas.used(Resource::Silent), 2 * size);
            drop((donor, refill));
            assert_eq!(quotas.used(Resource::Silent), 0);
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

/// Public ownership boundaries and synchronous reentrant notification contracts.
mod coalesce_tests {
    use flow_control::coalesce::flight;
    use flow_control::coalesce::flight::state::{Outcome, Phase, Published, State, WaiterPolicy};
    use std::cell::Cell;
    use std::rc::Rc;
    use std::time::Instant;

    /// An expired leader is checked separately from the 64-entry deadline quantum.
    #[test]
    fn expired_leader_does_not_consume_deadline_quantum_or_clear_drain_fence() {
        let mut state = State::<u32, u32, Published<u32, u32>, CountingPolicy>::default();
        let mut identity = flight::Identity {
            owner: Rc::new(()),
            incarnation: 1,
            generation: 0,
        };
        let now = Instant::now();
        let checks = Rc::new(Cell::new(0));
        let error = Rc::new(Cell::new(None));
        for id in 0..66 {
            state.register(
                id,
                CountingPolicy {
                    due: now,
                    checks: checks.clone(),
                    error: error.clone(),
                },
                true,
                true,
            );
        }
        assert_eq!(state.elect(0, &mut identity, 2), Ok(true));
        error.set(Some(7));
        let mut wakes = Vec::new();
        state.refresh(false, 9, 8, || now, std::convert::identity, &mut wakes);
        assert_eq!(checks.get(), 65);
        assert_eq!(state.deadlines.len(), 1);
        assert_eq!(state.waiters[&0].error, Some(7));
        assert!(state.waiters[&65].error.is_none());
        assert!(matches!(state.phase, Phase::Draining(Outcome::Retry)));
        assert_eq!(state.elect(65, &mut identity, 2), Ok(false));
        assert_eq!(identity.generation, 1);

        state.refresh(false, 9, 8, || now, std::convert::identity, &mut wakes);
        assert_eq!(checks.get(), 66);
        assert!(state.deadlines.is_empty());
        assert!(matches!(state.phase, Phase::Draining(Outcome::Retry)));
        state.settle(true, 9, std::convert::identity, &mut wakes);
        assert!(matches!(state.phase, Phase::Failed(9)));
    }

    /// Fixed-deadline policy with observable checks and externally injected failures.
    struct CountingPolicy {
        due: Instant,

        checks: Rc<Cell<usize>>,

        error: Rc<Cell<Option<u8>>>,
    }

    impl WaiterPolicy for CountingPolicy {
        /// Identify the failure injected into a waiter.
        type Error = u8;

        /// Count policy evaluation and return the current injected failure.
        fn check(&self) -> Option<Self::Error> {
            self.checks.set(self.checks.get() + 1);
            self.error.get()
        }

        /// Return the fixed deadline shared by this test's waiters.
        fn deadline(&self) -> Instant {
            self.due
        }
    }
}
