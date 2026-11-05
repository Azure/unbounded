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
    use flow_control::coalesce::flight::state::{Outcome, Phase, Published, State, WaiterPolicy};
    use flow_control::coalesce::flight::{self, Entry, Operations, Stale};
    use flow_control::coalesce::shared;
    use futures::executor::block_on;
    use std::cell::{Cell, RefCell};
    use std::future::Future;
    use std::rc::Rc;
    use std::sync::Arc;
    use std::task::{Context, RawWaker, RawWakerVTable, Wake, Waker};
    use std::time::Instant;

    thread_local! {
        /// Callback invoked only by synchronous wakes on the current test worker.
        static ON_WAKE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);

        /// One-shot hooks for raw waker ownership callbacks on this test worker.
        static ON_COHORT_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);

        static ON_COHORT_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    }

    /// Stateless raw waker; callbacks use only the calling thread's test hooks.
    fn cohort_raw_waker() -> RawWaker {
        /// Run the clone hook without retaining its registry borrow.
        unsafe fn clone(_: *const ()) -> RawWaker {
            let callback = ON_COHORT_CLONE.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
            cohort_raw_waker()
        }

        /// Run the drop hook without retaining its registry borrow.
        unsafe fn drop(_: *const ()) {
            let callback = ON_COHORT_DROP.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
        }

        /// Notifications need no action for these ownership callback tests.
        unsafe fn wake(_: *const ()) {}

        RawWaker::new(
            std::ptr::null(),
            &RawWakerVTable::new(clone, wake, wake, drop),
        )
    }

    /// Create a thread-safe stateless waker with worker-local test hooks.
    fn cohort_waker() -> Waker {
        // SAFETY: The vtable never dereferences data or shares thread-local hooks.
        unsafe { Waker::from_raw(cohort_raw_waker()) }
    }

    /// Register a flight wake target through the caller-owned borrow boundary.
    fn register_flight_waker(owner: &RefCell<Option<Waker>>, waker: &Waker) {
        let waker = waker.clone();
        let retired = flight::update(owner, |slot, _| flight::state::store_waker(slot, waker));
        drop(retired);
    }

    /// Empty, different, and equal slots transfer ownership without callbacks.
    #[test]
    fn flight_store_waker_returns_retired_without_callbacks() {
        let owner = RefCell::new(None);
        let waker = cohort_waker();
        let cloned = Rc::new(Cell::new(false));
        let dropped = Rc::new(Cell::new(false));
        ON_COHORT_CLONE.with(|slot| {
            let cloned = cloned.clone();
            *slot.borrow_mut() = Some(Box::new(move || cloned.set(true)));
        });
        ON_COHORT_DROP.with(|slot| {
            let dropped = dropped.clone();
            *slot.borrow_mut() = Some(Box::new(move || dropped.set(true)));
        });
        let duplicate = cohort_waker();
        let retired = flight::update(&owner, |slot, _| {
            assert!(flight::state::store_waker(slot, waker).is_none());
            let retired = flight::state::store_waker(slot, duplicate).unwrap();
            assert!(slot.as_ref().unwrap().will_wake(&retired));
            assert!(!cloned.get());
            assert!(!dropped.get());
            retired
        });
        drop(retired);
        assert!(dropped.get());
        let latest = Waker::from(Arc::new(Reenter));
        let replacement = latest.clone();
        let retired = flight::update(&owner, |slot, _| {
            flight::state::store_waker(slot, replacement)
        });
        assert!(retired.as_ref().unwrap().will_wake(&cohort_waker()));
        assert!(owner.borrow().as_ref().unwrap().will_wake(&latest));
        assert!(!cloned.get());
        ON_COHORT_CLONE.with(|slot| slot.borrow_mut().take());
    }

    /// Clone callbacks can change the slot before registration borrows its owner.
    #[test]
    fn flight_store_waker_clone_reentry() {
        let owner = Rc::new(RefCell::new(None));
        let called = Rc::new(Cell::new(false));
        ON_COHORT_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let called = called.clone();
                move || {
                    register_flight_waker(&owner, Waker::noop());
                    called.set(true);
                }
            }));
        });
        let waker = cohort_waker();
        register_flight_waker(&owner, &waker);
        assert!(called.get());
        assert!(owner.borrow().as_ref().unwrap().will_wake(&waker));
    }

    /// Retired and redundant targets are dropped only after the owner is released.
    fn check_flight_store_waker_drop_reentry(identical: bool) {
        let owner = Rc::new(RefCell::new(None));
        let waker = cohort_waker();
        let latest = Waker::from(Arc::new(Reenter));
        register_flight_waker(&owner, &waker);
        let called = Rc::new(Cell::new(false));
        ON_COHORT_DROP.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let called = called.clone();
                let latest = latest.clone();
                move || {
                    let expected = if identical {
                        cohort_waker()
                    } else {
                        latest.clone()
                    };
                    flight::update(&owner, |slot, _| {
                        assert!(slot.as_ref().unwrap().will_wake(&expected));
                    });
                    register_flight_waker(&owner, &latest);
                    called.set(true);
                }
            }));
        });
        register_flight_waker(&owner, if identical { &waker } else { &latest });
        assert!(called.get());
        assert!(owner.borrow().as_ref().unwrap().will_wake(&latest));
    }

    /// Replacing a different target must return the old waker for deferred drop.
    #[test]
    fn flight_store_waker_replacement_drop_reentry() {
        check_flight_store_waker_drop_reentry(false);
    }

    /// Keeping the same target must return the unused incoming waker for deferred drop.
    #[test]
    fn flight_store_waker_identical_drop_reentry() {
        check_flight_store_waker_drop_reentry(true);
    }

    /// Safe thread-local callback dispatch; no non-Send data enters the Waker itself.
    struct Reenter;

    impl Wake for Reenter {
        /// Invoke the current worker's callback after releasing its registry borrow.
        fn wake(self: Arc<Self>) {
            let callback = ON_WAKE.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
        }
    }

    /// Install a callback and return a wake target that dispatches it synchronously.
    fn on_wake(callback: impl FnOnce() + 'static) -> Waker {
        ON_WAKE.with(|slot| assert!(slot.borrow_mut().replace(Box::new(callback)).is_none()));
        Waker::from(Arc::new(Reenter))
    }

    /// Shared result notification can synchronously start replacement work.
    #[test]
    fn shared_completion_wakes_after_removal_and_keeps_new_owner() {
        let table = Rc::new(shared::Table::default());
        let (mut receive, complete) = table.start(1, 99);
        let replacement = Rc::new(RefCell::new(None));
        let waker = on_wake({
            let table = table.clone();
            let replacement = replacement.clone();
            move || {
                assert!(table.is_empty());
                replacement.replace(Some(table.start(1, 99)));
            }
        });
        assert!(
            std::pin::Pin::new(&mut receive)
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        complete.unwrap().finish(7);
        assert_eq!(block_on(receive), 7);
        let (receive, complete) = replacement.borrow_mut().take().unwrap();
        assert_eq!(table.len(), 1);
        drop(complete);
        assert_eq!(block_on(receive), 99);
        assert_eq!(
            table.len(),
            1,
            "sender loss cannot pretend execution completed"
        );
    }

    /// Losing the sender wakes parked readers without removing the indexed work.
    #[test]
    fn shared_sender_drop_wakes_with_entry_still_indexed() {
        let table = Rc::new(shared::Table::default());
        let (mut receive, complete) = table.start(1, 99);
        let notified = Rc::new(Cell::new(false));
        let waker = on_wake({
            let table = table.clone();
            let notified = notified.clone();
            move || {
                assert_eq!(table.len(), 1);
                assert!(table.get(&1).is_some());
                notified.set(true);
            }
        });
        assert!(
            std::pin::Pin::new(&mut receive)
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        drop(complete);
        assert!(notified.get());
        assert_eq!(block_on(receive), 99);
        assert_eq!(block_on(table.get(&1).unwrap()), 99);
        assert_eq!(table.len(), 1);
    }

    /// Resource whose destructor can synchronously inspect its owning table.
    struct ReentrantResource(Option<Box<dyn FnOnce()>>);

    impl Drop for ReentrantResource {
        /// Reenter the owner while its operation tombstone must still be present.
        fn drop(&mut self) {
            self.0.take().unwrap()();
        }
    }

    /// Only operation completion makes this deliberately waiter-free entry removable.
    #[derive(Default)]
    struct OwnedEntry(Operations<ReentrantResource>);

    impl Entry for OwnedEntry {
        /// This fixture has no caller policy to refresh.
        fn refresh(&mut self, _: &mut Vec<Waker>) {}

        /// Require explicit completion of every retained operation.
        fn quiescent(&self) -> bool {
            self.0.is_empty()
        }
    }

    /// A removable entry can still own application data with a reentrant destructor.
    struct DroppableEntry {
        _resource: ReentrantResource,

        quiescent: bool,
    }

    impl Entry for DroppableEntry {
        fn refresh(&mut self, _: &mut Vec<Waker>) {}

        fn quiescent(&self) -> bool {
            self.quiescent
        }
    }

    /// Rejected entries leave the transaction before their destructors reenter it.
    #[test]
    fn occupied_insertion_keeps_destructors_outside_owner_borrow() {
        for quiescent in [false, true] {
            let table = Rc::new(RefCell::new(flight::Table::<u32, DroppableEntry>::default()));
            let original_dropped = Rc::new(Cell::new(false));
            let rejected_dropped = Rc::new(Cell::new(false));
            let unlocked = Rc::new(Cell::new(false));
            let entry = |dropped: Rc<Cell<bool>>| DroppableEntry {
                _resource: ReentrantResource(Some(Box::new({
                    let table = Rc::downgrade(&table);
                    let unlocked = unlocked.clone();
                    move || {
                        let Some(table) = table.upgrade() else {
                            return;
                        };
                        if let Ok(mut table) = table.try_borrow_mut() {
                            assert!(table.next_waiter_id().is_ok());
                            unlocked.set(true);
                        }
                        dropped.set(true);
                    }
                }))),
                quiescent,
            };
            assert!(
                flight::update(&table, |table, _| {
                    table.insert(1, entry(original_dropped.clone()))
                })
                .is_ok()
            );
            let rejected = flight::update(&table, |table, _| {
                table.insert(1, entry(rejected_dropped.clone()))
            });
            assert!(rejected.is_err());
            assert!(!original_dropped.get(), "occupied entry must stay owned");
            assert!(!rejected_dropped.get(), "rejection must return ownership");
            drop(rejected);
            assert!(rejected_dropped.get());
            assert!(unlocked.get());
            assert!(!original_dropped.get());
            let removed = flight::update(&table, |table, _| {
                assert_eq!(table.len(), 1);
                table.get_mut(&1).unwrap().quiescent = true;
                table.remove_quiescent(&1)
            });
            // The original destructor also reenters after explicit removal.
            unlocked.set(false);
            drop(removed);
            assert!(original_dropped.get());
            assert!(unlocked.get());
        }
    }

    /// Sweeping transfers entry destruction past the owner transaction.
    #[test]
    fn swept_entry_destructor_can_reenter_owner() {
        removed_entry_destructor_can_reenter_owner(true);
    }

    /// Explicit removal has the same destruction boundary as a sweep.
    #[test]
    fn detached_entry_destructor_can_reenter_owner() {
        removed_entry_destructor_can_reenter_owner(false);
    }

    /// Check both removal paths, including ineligible entries and same-key replacement.
    fn removed_entry_destructor_can_reenter_owner(sweep: bool) {
        let table = Rc::new(RefCell::new(flight::Table::<u32, DroppableEntry>::default()));
        let dropped = Rc::new(Cell::new(0));
        let resource = ReentrantResource(Some(Box::new({
            let table = Rc::downgrade(&table);
            let dropped = dropped.clone();
            move || {
                let table = table.upgrade().unwrap();
                drop(flight::update(&table, |table, wakes| {
                    assert!(table.is_empty());
                    let removed = table.sweep(1, wakes);
                    assert!(removed.is_empty());
                    assert_eq!(table.next_waiter_id(), Ok(1));
                    assert!(
                        table
                            .insert(
                                1,
                                DroppableEntry {
                                    _resource: ReentrantResource(Some(Box::new(|| {}))),
                                    quiescent: false,
                                },
                            )
                            .is_ok()
                    );
                    removed
                }));
                dropped.set(dropped.get() + 1);
            }
        })));
        assert!(
            table
                .borrow_mut()
                .insert(
                    1,
                    DroppableEntry {
                        _resource: resource,
                        quiescent: false,
                    },
                )
                .is_ok()
        );
        let removed = flight::update(&table, |table, wakes| {
            assert!(table.remove_quiescent(&2).is_none());
            assert!(table.remove_quiescent(&1).is_none());
            assert!(table.sweep(1, wakes).is_empty());
            table.get_mut(&1).unwrap().quiescent = true;
            assert!(table.sweep(0, wakes).is_empty());
            assert_eq!(table.len(), 1);
            let removed = if sweep {
                table.sweep(1, wakes)
            } else {
                table.remove_quiescent(&1).into_iter().collect()
            };
            assert_eq!(removed.len(), 1);
            assert!(table.is_empty());
            assert!(table.remove_quiescent(&1).is_none());
            assert_eq!(dropped.get(), 0);
            removed
        });
        assert_eq!(dropped.get(), 0);
        drop(removed);
        assert_eq!(dropped.get(), 1);
        assert_eq!(table.borrow_mut().next_waiter_id(), Ok(2));
        drop(flight::update(&table, |table, wakes| table.sweep(1, wakes)));
        assert_eq!(table.borrow().len(), 1, "replacement remains indexed");
        assert_eq!(dropped.get(), 1);
    }

    /// Resource drop reentrancy cannot erase the tombstone, and drain wakes run unlocked.
    #[test]
    fn completion_tombstone_survives_reentrant_destructor_and_shutdown() {
        let table = Rc::new(RefCell::new(flight::Table::<u32, OwnedEntry>::default()));
        let dropped = Rc::new(Cell::new(false));
        let resource = ReentrantResource(Some(Box::new({
            let table = Rc::downgrade(&table);
            let dropped = dropped.clone();
            move || {
                let table = table.upgrade().unwrap();
                drop(flight::update(&table, |table, wakes| {
                    let removed = table.sweep(1, wakes);
                    assert_eq!(table.len(), 1);
                    assert!(table.remove_quiescent(&1).is_none());
                    removed
                }));
                dropped.set(true);
            }
        })));
        let id = flight::update(&table, |table, _| {
            assert_eq!(table.next_waiter_id(), Ok(1));
            assert_eq!(table.next_waiter_id(), Ok(2));
            let id = table.next_operation_id().unwrap();
            assert!(table.insert(1, OwnedEntry::default()).is_ok());
            table.get_mut(&1).unwrap().0.insert(id, resource);
            assert_eq!(table.get_mut(&1).unwrap().0.complete(id), Err(Stale));
            id
        });
        flight::update(&table, |table, wakes| table.stop(wakes, |_, _| {}));
        assert!(table.borrow().is_stopping());
        let resource = flight::update(&table, |table, _| {
            let operations = &mut table.get_mut(&1).unwrap().0;
            let resource = operations.take(id).unwrap();
            assert!(matches!(operations.take(id), Err(Stale)));
            assert!(matches!(operations.take(id + 1), Err(Stale)));
            assert_eq!(operations.complete(id + 1), Err(Stale));
            assert_eq!(operations.len(), 1);
            resource
        });
        drop(resource);
        assert!(dropped.get());
        assert_eq!(table.borrow().len(), 1);
        let notified = Rc::new(Cell::new(false));
        let waker = on_wake({
            let table = table.clone();
            let notified = notified.clone();
            move || {
                assert!(table.borrow().is_empty());
                notified.set(true);
            }
        });
        drop(flight::update(&table, |table, _| {
            table.register_drain(waker)
        }));
        drop(flight::update(&table, |table, wakes| {
            let operations = &mut table.get_mut(&1).unwrap().0;
            operations.complete(id).unwrap();
            assert_eq!(operations.complete(id), Err(Stale));
            table.sweep(1, wakes)
        }));
        assert!(notified.get());
    }

    /// An expired leader is checked separately from the 64-entry deadline quantum.
    #[test]
    fn expired_leader_does_not_consume_deadline_quantum_or_clear_drain_fence() {
        let mut state = State::<u32, u32, Published<u32, u32>, CountingPolicy>::default();
        let mut table = flight::Table::<u32, ()>::default();
        let mut identity = table.identity(Rc::new(())).unwrap();
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
