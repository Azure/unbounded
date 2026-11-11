//! Worker-local flight state ([`flight`]).
//!
//! Callers own execution, cancellation, admission policy, and result semantics. The keyed table
//! that drives this state lives in racer-core (`read::flight::FlightTable`).

pub mod flight {
    //! Generation fences and result-independent flight transitions ([`state`]).

    use std::collections::BTreeMap;
    use std::rc::Rc;

    /// A monotonic identifier or acquisition budget has been exhausted.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Exhausted;

    /// A registration, generation, or operation no longer matches its owner.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Stale;

    /// Key equality belongs to the adapter; this identity fences owner, incarnation,
    /// and acquisition generation independently of any application key schema.
    #[derive(Clone)]
    pub struct Identity {
        /// Local table owner; pointer identity fences unrelated workers and tables.
        pub owner: Rc<()>,

        /// Monotonic identity of one entry admission.
        pub incarnation: u64,

        /// Acquisition attempt within this incarnation.
        pub generation: u64,
    }

    impl Identity {
        /// Compare entry ownership without invalidating waiters across retries.
        pub fn same_registration(&self, current: &Self) -> bool {
            Rc::ptr_eq(&self.owner, &current.owner) && self.incarnation == current.incarnation
        }

        /// Validate an acquisition capability against the current generation.
        pub fn validate(&self, current: &Self) -> Result<(), Stale> {
            if !self.same_registration(current) || self.generation != current.generation {
                return Err(Stale);
            }
            Ok(())
        }

        /// Called only after the adapter confirms eligibility and completion fences.
        fn advance(&mut self, limit: u64) -> Result<(), Exhausted> {
            if self.generation >= limit {
                return Err(Exhausted);
            }
            self.generation = self.generation.checked_add(1).ok_or(Exhausted)?;
            Ok(())
        }
    }

    /// Stable round-robin selection without a scan, including wrap after removal.
    #[derive(Default)]
    struct Cursor(u64);

    impl Cursor {
        /// Select the next live identifier, wrapping to the first when necessary.
        fn next<'a, V>(&mut self, entries: &'a BTreeMap<u64, V>) -> Option<(u64, &'a V)> {
            let (&id, value) = entries
                .range((
                    std::ops::Bound::Excluded(self.0),
                    std::ops::Bound::Unbounded,
                ))
                .next()
                .or_else(|| entries.first_key_value())?;
            self.0 = id;
            Some((id, value))
        }
    }

    /// Waiter lifecycle and result-independent flight transitions.
    pub mod state {
        use super::{Cursor, Exhausted, Identity, Stale};
        use std::collections::BTreeMap;
        use std::ops::{Deref, DerefMut};
        use std::task::Waker;
        use std::time::Instant;

        /// Request-owned policy facts, not acquisition credits or result interpretation.
        pub trait WaiterPolicy {
            /// Lightweight failure value retained with a waiter.
            type Error: Copy;

            /// Check cancellation, deadline, and any other request-owned policy.
            fn check(&self) -> Option<Self::Error>;

            /// Return the request's fixed deadline for bounded expiry indexing.
            fn deadline(&self) -> Instant;
        }

        /// One attached caller's policy, acquisition eligibility, and notification state.
        /// `T` is the wake target: a [`Waker`] in production, or a plain token for
        /// sans-IO drivers such as the simulator.
        pub struct Waiter<W: WaiterPolicy, T = Waker> {
            /// Caller-owned policy facts, available through dereferencing as well.
            policy: W,

            /// Whether this caller can acquire rather than only observe results.
            acquisition: bool,

            /// Whether partial publication permits another attempt for this caller.
            pub complete: bool,

            /// Whether this caller has already received its acquisition opportunity.
            pub issued: bool,

            /// Sticky policy or cancellation failure.
            pub error: Option<W::Error>,

            /// Latest wake target, taken once when notification is enqueued.
            pub waker: Option<T>,
        }

        impl<W: WaiterPolicy, T> Deref for Waiter<W, T> {
            type Target = W;

            /// Read the caller-owned policy facts.
            fn deref(&self) -> &W {
                &self.policy
            }
        }

        impl<W: WaiterPolicy, T> DerefMut for Waiter<W, T> {
            /// Update caller-owned policy facts without replacing waiter state.
            fn deref_mut(&mut self) -> &mut W {
                &mut self.policy
            }
        }

        impl<W: WaiterPolicy, T> Waiter<W, T> {
            /// Whether this caller can acquire rather than only observe results.
            pub fn acquisition(&self) -> bool {
                self.acquisition
            }

            /// Whether an unissued acquisition opportunity remains usable.
            fn eligible(&self) -> bool {
                self.acquisition && !self.issued && self.error.is_none()
            }
        }

        /// Attempt outcome held behind the retained-operation completion fence.
        pub enum Outcome<O, E> {
            /// A publication awaiting settlement and result interpretation.
            Published(O),

            /// A terminal error awaiting settlement.
            Failed(E),

            /// An attempt that permits another eligible caller after settlement.
            Retry,
        }

        /// Flight lifecycle independent of application-specific result semantics.
        pub enum Phase<C, O, E> {
            /// A leader may acquire and publish a result.
            Acquiring,

            /// No retained attempt blocks election of an eligible caller.
            RetryPending,

            /// An outcome exists but retained operations may still own resources.
            Draining(Outcome<O, E>),

            /// A fully settled complete result is available.
            Complete(C),

            /// A fully settled terminal error is available.
            Failed(E),
        }

        /// Adapter interpretation of a successfully settled publication.
        pub enum Published<C, P> {
            /// A result that satisfies all callers.
            Complete(C),

            /// A reusable intermediate result that may require another acquisition.
            Partial(P),
        }

        /// Worker-local waiter index and transition state driven by an owning adapter.
        /// The adapter supplies actual operation idleness; cancellation is not idleness.
        pub struct State<C, P, O, W: WaiterPolicy, T = Waker> {
            /// Current acquisition or settlement phase.
            pub phase: Phase<C, O, W::Error>,

            /// Current leader's waiter identifier, retained until settlement.
            /// After a handover this is the adopting waiter, not the elected one.
            pub leader: Option<u64>,

            /// Waiter elected for the current acquisition generation. The
            /// operation's publication capability stays bound to it across
            /// handovers.
            pub elected: Option<u64>,

            /// Whether the running acquisition may pass to another waiter when
            /// its leader fails its policy, instead of being revoked.
            pub handover: bool,

            /// Attached callers indexed by monotonic waiter identifier.
            pub waiters: BTreeMap<u64, Waiter<W, T>>,

            /// Deadline index used for bounded expiration work.
            pub deadlines: BTreeMap<(Instant, u64), ()>,

            /// Settled intermediate result retained across another acquisition.
            pub partial: Option<P>,

            cursor: Cursor,

            /// State transitions belong to one worker even for thread-safe policy data.
            local: std::marker::PhantomData<std::rc::Rc<()>>,
        }

        impl<C, P, O, W: WaiterPolicy, T> Default for State<C, P, O, W, T> {
            /// Start without callers or a retained attempt, ready for initial election.
            fn default() -> Self {
                Self {
                    phase: Phase::RetryPending,
                    leader: None,
                    elected: None,
                    handover: false,
                    waiters: BTreeMap::new(),
                    deadlines: BTreeMap::new(),
                    partial: None,
                    cursor: Cursor::default(),
                    local: std::marker::PhantomData,
                }
            }
        }

        impl<C, P, O, W: WaiterPolicy, T> State<C, P, O, W, T> {
            /// Attach a caller using an identifier allocated by the owning table.
            pub fn register(&mut self, id: u64, policy: W, acquisition: bool, complete: bool) {
                self.deadlines.insert((policy.deadline(), id), ());
                self.waiters.insert(
                    id,
                    Waiter {
                        policy,
                        acquisition,
                        complete,
                        issued: false,
                        error: None,
                        waker: None,
                    },
                );
            }

            /// Remove a caller and its deadline without declaring operation completion.
            pub fn detach(&mut self, id: u64) {
                if let Some(waiter) = self.waiters.remove(&id) {
                    self.deadlines.remove(&(waiter.deadline(), id));
                }
            }

            /// Validate attachment and incarnation while allowing acquisition retries.
            pub fn validate_registration(
                &self,
                id: u64,
                attached: bool,
                registered: &Identity,
                current: &Identity,
            ) -> Result<(), Stale> {
                if !attached
                    || !registered.same_registration(current)
                    || !self.waiters.contains_key(&id)
                {
                    return Err(Stale);
                }
                Ok(())
            }

            /// Mark only the supplying caller; refresh decides whether to revoke a leader.
            pub fn cancel(&mut self, id: u64, error: W::Error) {
                if let Some(waiter) = self.waiters.get_mut(&id) {
                    waiter.error = Some(error);
                }
            }

            /// Caller validates its leader capability and publication before this step.
            /// Notification timing remains explicit: publication wakes only on settlement,
            /// while rejection/revocation also notifies before retained operations finish.
            pub fn begin_completion(&mut self, outcome: Outcome<O, W::Error>) {
                self.phase = Phase::Draining(outcome);
            }

            /// Enqueue all parked caller notifications without invoking their wakers.
            pub fn notify(&mut self, wakes: &mut Vec<T>) {
                for waiter in self.waiters.values_mut() {
                    if let Some(waker) = waiter.waker.take() {
                        wakes.push(waker);
                    }
                }
            }

            /// Check one caller in round-robin order without scanning the waiter map.
            pub fn sweep_waiter(&mut self, wakes: &mut Vec<T>) {
                if let Some((id, _)) = self.cursor.next(&self.waiters) {
                    self.refresh_waiter(id, wakes);
                }
            }

            /// Validate policy/budget first in the adapter. Only an eligible waiter can
            /// receive a new generation; retained operations must have settled first.
            pub fn elect(
                &mut self,
                id: u64,
                identity: &mut Identity,
                limit: u64,
            ) -> Result<bool, Exhausted> {
                if !matches!(self.phase, Phase::RetryPending) {
                    return Ok(false);
                }
                let Some(waiter) = self.waiters.get_mut(&id).filter(|waiter| waiter.eligible())
                else {
                    return Ok(false);
                };
                identity.advance(limit)?;
                self.phase = Phase::Acquiring;
                self.leader = Some(id);
                self.elected = Some(id);
                self.handover = false;
                waiter.issued = true;
                Ok(true)
            }

            /// Permit the current acquisition to pass to another eligible waiter
            /// when its leader fails its policy while operations still run. The
            /// adapter allows this only for acquisitions whose caller does not
            /// fence on the operation itself.
            pub fn allow_handover(&mut self) {
                if matches!(self.phase, Phase::Acquiring) {
                    self.handover = true;
                }
            }

            /// Whether the running acquisition now continues on behalf of a
            /// waiter other than the one elected for it.
            pub fn handed_over(&self) -> bool {
                matches!(self.phase, Phase::Acquiring) && self.leader != self.elected
            }

            /// Policy deadline of the waiter the acquisition currently runs for.
            pub fn leader_deadline(&self) -> Option<Instant> {
                if !matches!(self.phase, Phase::Acquiring) {
                    return None;
                }
                self.leader
                    .and_then(|id| self.waiters.get(&id))
                    .map(|waiter| waiter.deadline())
            }

            /// Validate the active leader after the adapter checks its generation fence.
            /// The elected waiter's capability survives a handover: the operation it
            /// started continues, and only its publication settles the generation.
            pub fn validate_leader(&self, id: u64, active: bool) -> Result<(), Stale> {
                if !active
                    || !matches!(self.phase, Phase::Acquiring)
                    || (self.leader != Some(id) && self.elected != Some(id))
                {
                    return Err(Stale);
                }
                Ok(())
            }

            /// Pass the running acquisition to the live eligible waiter with the
            /// latest deadline. It is not marked issued: it spends none of its own
            /// budget unless the acquisition settles to a retry and elects it.
            fn hand_over(&mut self, wakes: &mut Vec<T>) -> bool {
                let current = self.leader;
                let mut candidates: Vec<(Instant, u64)> = self
                    .waiters
                    .iter()
                    .filter(|(id, waiter)| Some(**id) != current && waiter.eligible())
                    .map(|(id, waiter)| (waiter.deadline(), *id))
                    .collect();
                candidates.sort_unstable_by(|a, b| b.cmp(a));
                for (_, id) in candidates {
                    self.refresh_waiter(id, wakes);
                    if self
                        .waiters
                        .get(&id)
                        .is_some_and(|waiter| waiter.error.is_none())
                    {
                        self.leader = Some(id);
                        return true;
                    }
                }
                false
            }

            /// Expose an outcome only after the adapter confirms all operations completed.
            pub fn settle(
                &mut self,
                idle: bool,
                unavailable: W::Error,
                split: fn(O) -> Published<C, P>,
                wakes: &mut Vec<T>,
            ) {
                if !idle || !matches!(self.phase, Phase::Draining(_)) {
                    return;
                }
                let Phase::Draining(outcome) =
                    std::mem::replace(&mut self.phase, Phase::RetryPending)
                else {
                    unreachable!()
                };
                self.leader = None;
                self.elected = None;
                self.handover = false;
                self.phase = match outcome {
                    Outcome::Published(value) => match split(value) {
                        Published::Complete(value) => {
                            self.partial = None;
                            Phase::Complete(value)
                        }
                        Published::Partial(value) => {
                            self.partial = Some(value);
                            for waiter in self.waiters.values_mut() {
                                if waiter.complete {
                                    waiter.issued = false;
                                }
                            }
                            Phase::RetryPending
                        }
                    },
                    Outcome::Failed(error) => Phase::Failed(error),
                    Outcome::Retry if self.waiters.values().any(Waiter::eligible) => {
                        Phase::RetryPending
                    }
                    Outcome::Retry => Phase::Failed(unavailable),
                };
                self.notify(wakes);
            }

            /// Revoke acquisition immediately, retaining its outcome behind completion.
            pub fn revoke(&mut self, error: W::Error, wakes: &mut Vec<T>) {
                // The elected caller's work is revoked; an adopting waiter keeps
                // its own election opportunity.
                if let Some(waiter) = self.elected.and_then(|id| self.waiters.get_mut(&id)) {
                    waiter.error = Some(error);
                }
                self.phase = Phase::Draining(Outcome::Retry);
                self.notify(wakes);
            }

            /// Check the leader and up to 64 expired callers, then settle eligible outcomes.
            pub fn refresh(
                &mut self,
                idle: bool,
                unavailable: W::Error,
                canceled: W::Error,
                now: impl Fn() -> Instant,
                split: fn(O) -> Published<C, P>,
                wakes: &mut Vec<T>,
            ) {
                if let Some(id) = self.leader {
                    self.refresh_waiter(id, wakes);
                }
                self.refresh_expired(&now, wakes);
                if matches!(self.phase, Phase::Acquiring) {
                    let leader = self.leader.and_then(|id| self.waiters.get(&id));
                    if leader.is_none_or(|waiter| waiter.error.is_some()) {
                        let error = leader.and_then(|waiter| waiter.error).unwrap_or(canceled);
                        // A running operation continues for another live waiter
                        // rather than being canceled and restarted from scratch.
                        if !(self.handover && !idle && self.hand_over(wakes)) {
                            self.revoke(error, wakes);
                        }
                    }
                }
                self.settle(idle, unavailable, split, wakes);
                if matches!(self.phase, Phase::RetryPending)
                    && self.partial.is_none()
                    && !self.waiters.values().any(Waiter::eligible)
                {
                    self.phase = Phase::Draining(Outcome::Failed(unavailable));
                    self.settle(idle, unavailable, split, wakes);
                }
            }

            /// Detach and notify all callers while preserving the operation drain fence.
            pub fn stop(&mut self, error: W::Error, wakes: &mut Vec<T>) {
                for waiter in self.waiters.values_mut() {
                    waiter.error = Some(error);
                }
                self.phase = Phase::Draining(Outcome::Failed(error));
                self.notify(wakes);
                self.waiters.clear();
                self.deadlines.clear();
            }

            /// Check one caller and enqueue its wake if its policy has failed.
            fn refresh_waiter(&mut self, id: u64, wakes: &mut Vec<T>) {
                let Some(waiter) = self.waiters.get_mut(&id) else {
                    return;
                };
                if waiter.error.is_none() {
                    waiter.error = waiter.check();
                }
                if waiter.error.is_some() {
                    self.deadlines.remove(&(waiter.deadline(), id));
                    if let Some(waker) = waiter.waker.take() {
                        wakes.push(waker);
                    }
                }
            }

            /// Consume at most 64 due deadline entries, independently of leader checks.
            fn refresh_expired(&mut self, now: &impl Fn() -> Instant, wakes: &mut Vec<T>) {
                for _ in 0..64 {
                    let Some((&(deadline, id), _)) = self.deadlines.first_key_value() else {
                        break;
                    };
                    if deadline > now() {
                        break;
                    }
                    self.deadlines.remove(&(deadline, id));
                    self.refresh_waiter(id, wakes);
                }
            }
        }

        /// Pure transition tests for cancellation, publication, and bounded expiry.
        #[cfg(test)]
        mod tests {
            use super::*;
            use std::cell::Cell;
            use std::rc::Rc;
            use std::sync::Arc;
            use std::sync::atomic::{AtomicUsize, Ordering};
            use std::task::Wake;
            use std::time::Duration;

            /// Mutable policy failure used to drive deterministic transitions.
            struct Policy {
                due: Instant,

                error: Rc<Cell<Option<u8>>>,
            }

            impl WaiterPolicy for Policy {
                type Error = u8;

                /// Read the failure injected by the test.
                fn check(&self) -> Option<u8> {
                    self.error.get()
                }

                /// Return the fixed test deadline.
                fn deadline(&self) -> Instant {
                    self.due
                }
            }

            /// Numeric results keep these tests independent of application semantics.
            type Core = State<u32, u32, Published<u32, u32>, Policy>;

            /// Publications already carry their complete or partial interpretation.
            fn split(value: Published<u32, u32>) -> Published<u32, u32> {
                value
            }

            /// Attach a caller and return its externally mutable policy failure.
            fn register(
                core: &mut Core,
                id: u64,
                acquisition: bool,
                complete: bool,
            ) -> Rc<Cell<Option<u8>>> {
                let error = Rc::new(Cell::new(None));
                core.register(
                    id,
                    Policy {
                        due: Instant::now() + Duration::from_secs(60),
                        error: error.clone(),
                    },
                    acquisition,
                    complete,
                );
                error
            }

            /// Construct the initial identity for an isolated transition scenario.
            fn identity() -> Identity {
                Identity {
                    owner: Rc::new(()),
                    incarnation: 1,
                    generation: 0,
                }
            }

            /// Refresh with fixed unavailable and cancellation error values.
            fn refresh(core: &mut Core, idle: bool, wakes: &mut Vec<Waker>) {
                core.refresh(idle, 9, 8, Instant::now, split, wakes);
            }

            /// Leader cancellation or detach cannot bypass retained-operation drainage.
            #[test]
            fn cancel_or_detach_leader_drains_before_re_election() {
                for detach in [false, true] {
                    let mut core = Core::default();
                    let error = register(&mut core, 1, true, true);
                    register(&mut core, 2, true, true);
                    register(&mut core, 3, false, false);
                    let mut identity = identity();
                    assert_eq!(core.elect(3, &mut identity, 4), Ok(false));
                    assert_eq!(core.elect(1, &mut identity, 4), Ok(true));
                    let old = identity.clone();
                    assert_eq!(core.validate_registration(1, true, &old, &identity), Ok(()));
                    assert_eq!(core.elect(2, &mut identity, 4), Ok(false));
                    if detach {
                        core.detach(1);
                    } else {
                        error.set(Some(7));
                    }
                    let mut wakes = Vec::new();
                    refresh(&mut core, false, &mut wakes);
                    assert!(matches!(core.phase, Phase::Draining(Outcome::Retry)));
                    assert_eq!(core.validate_leader(1, true), Err(Stale));
                    assert_eq!(core.elect(2, &mut identity, 4), Ok(false));
                    refresh(&mut core, true, &mut wakes);
                    assert_eq!(core.elect(2, &mut identity, 4), Ok(true));
                    assert_eq!(old.validate(&identity), Err(Stale));
                    assert_eq!(core.validate_registration(2, true, &old, &identity), Ok(()));
                    assert_eq!(
                        core.validate_registration(2, false, &old, &identity),
                        Err(Stale)
                    );
                    assert_eq!(
                        core.validate_registration(4, true, &old, &identity),
                        Err(Stale)
                    );
                    assert_eq!(core.validate_leader(2, true), Ok(()));
                    assert_eq!(core.validate_leader(2, false), Err(Stale));
                }
            }

            /// Register a caller with an explicit deadline.
            fn register_due(
                core: &mut Core,
                id: u64,
                acquisition: bool,
                due: Duration,
            ) -> Rc<Cell<Option<u8>>> {
                let error = Rc::new(Cell::new(None));
                core.register(
                    id,
                    Policy {
                        due: Instant::now() + due,
                        error: error.clone(),
                    },
                    acquisition,
                    true,
                );
                error
            }

            /// A failed leader's running operation passes to the live acquisition
            /// waiter with the latest deadline, then to the next one, and is revoked
            /// only once no eligible waiter remains. The elected capability stays
            /// valid, and the adopting waiter keeps its own election opportunity.
            #[test]
            fn running_operation_hands_over_to_latest_eligible_waiter() {
                let mut core = Core::default();
                let a = register_due(&mut core, 1, true, Duration::from_secs(10));
                let b = register_due(&mut core, 2, true, Duration::from_secs(20));
                let c = register_due(&mut core, 3, true, Duration::from_secs(30));
                register_due(&mut core, 4, false, Duration::from_secs(90));
                let mut identity = identity();
                assert_eq!(core.elect(1, &mut identity, 4), Ok(true));
                core.allow_handover();
                assert!(!core.handed_over());
                assert_eq!(core.leader_deadline(), Some(core.waiters[&1].deadline()));
                let mut wakes = Vec::new();
                a.set(Some(7));
                refresh(&mut core, false, &mut wakes);
                assert!(matches!(core.phase, Phase::Acquiring));
                assert_eq!(core.leader, Some(3), "latest deadline adopts");
                assert!(core.handed_over());
                assert!(!core.waiters[&3].issued);
                assert_eq!(core.validate_leader(1, true), Ok(()));
                assert_eq!(core.validate_leader(3, true), Ok(()));
                assert_eq!(core.validate_leader(2, true), Err(Stale));
                c.set(Some(7));
                refresh(&mut core, false, &mut wakes);
                assert_eq!(core.leader, Some(2));
                b.set(Some(7));
                refresh(&mut core, false, &mut wakes);
                assert!(
                    matches!(core.phase, Phase::Draining(Outcome::Retry)),
                    "copy waiters cannot adopt"
                );
                assert_eq!(core.validate_leader(1, true), Err(Stale));
                assert_eq!(core.leader_deadline(), None);
                refresh(&mut core, true, &mut wakes);
                assert!(core.elected.is_none() && !core.handover);
            }

            /// Without permission, without a running operation, or with a failed
            /// candidate, the leader is revoked as before.
            #[test]
            fn handover_requires_permission_running_work_and_a_live_waiter() {
                for case in 0..3 {
                    let mut core = Core::default();
                    let a = register_due(&mut core, 1, true, Duration::from_secs(10));
                    let b = register_due(&mut core, 2, true, Duration::from_secs(20));
                    let mut identity = identity();
                    assert_eq!(core.elect(1, &mut identity, 4), Ok(true));
                    if case != 0 {
                        core.allow_handover();
                    }
                    if case == 2 {
                        b.set(Some(5));
                    }
                    a.set(Some(7));
                    let mut wakes = Vec::new();
                    refresh(&mut core, case == 1, &mut wakes);
                    if case == 1 {
                        assert!(matches!(core.phase, Phase::RetryPending), "case {case}");
                    } else {
                        assert!(
                            matches!(core.phase, Phase::Draining(Outcome::Retry)),
                            "case {case}"
                        );
                    }
                    assert!(!core.handed_over());
                }
            }

            /// A handed-over acquisition publishes through the elected capability,
            /// and a new election clears the permission.
            #[test]
            fn handed_over_publication_completes_and_election_resets_permission() {
                let mut core = Core::default();
                let a = register_due(&mut core, 1, true, Duration::from_secs(10));
                register_due(&mut core, 2, true, Duration::from_secs(20));
                let mut identity = identity();
                assert_eq!(core.elect(1, &mut identity, 4), Ok(true));
                core.allow_handover();
                a.set(Some(7));
                let mut wakes = Vec::new();
                refresh(&mut core, false, &mut wakes);
                assert_eq!(core.leader, Some(2));
                core.begin_completion(Outcome::Retry);
                core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::RetryPending));
                assert_eq!(core.elect(2, &mut identity, 4), Ok(true));
                assert!(!core.handover);
                core.begin_completion(Outcome::Published(Published::Complete(5)));
                core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Complete(5)));
            }

            /// Partial results renew only complete-result callers after the fence clears.
            #[test]
            fn partial_then_complete_and_terminal_failure_are_fenced() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, true, false);
                let mut identity = identity();
                assert_eq!(core.elect(1, &mut identity, 3), Ok(true));
                core.waiters.get_mut(&2).unwrap().issued = true;
                let mut wakes = Vec::new();
                core.begin_completion(Outcome::Published(Published::Partial(17)));
                core.settle(false, 9, split, &mut wakes);
                assert!(core.partial.is_none());
                core.settle(true, 9, split, &mut wakes);
                assert_eq!(core.partial, Some(17));
                assert!(!core.waiters[&1].issued);
                assert!(core.waiters[&2].issued);
                assert_eq!(core.elect(1, &mut identity, 3), Ok(true));
                core.begin_completion(Outcome::Published(Published::Complete(18)));
                core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Complete(18)));
                assert!(core.partial.is_none());
                assert_eq!(core.elect(1, &mut identity, 3), Ok(false));
                core.phase = Phase::Draining(Outcome::Failed(6));
                core.settle(false, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Draining(Outcome::Failed(6))));
                core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Failed(6)));
            }

            /// Observers cannot acquire and exhausted generations do not issue a caller.
            #[test]
            fn copy_only_cannot_revive_retry_and_generations_are_bounded() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, false, false);
                let mut identity = identity();
                assert_eq!(core.elect(1, &mut identity, 1), Ok(true));
                core.revoke(7, &mut Vec::new());
                core.settle(true, 9, split, &mut Vec::new());
                assert!(matches!(core.phase, Phase::Failed(9)));
                core.phase = Phase::RetryPending;
                register(&mut core, 3, true, true);
                assert_eq!(core.elect(3, &mut identity, 1), Err(Exhausted));
                assert!(!core.waiters[&3].issued);
                assert_eq!(identity.generation, 1);
            }

            /// Counts wake delivery without changing transition state.
            #[derive(Default)]
            struct Count(AtomicUsize);

            impl Wake for Count {
                /// Record one delivered notification.
                fn wake(self: Arc<Self>) {
                    self.0.fetch_add(1, Ordering::Relaxed);
                }
            }

            /// Expiry work is bounded to 64 callers and only their latest wakes fire.
            #[test]
            fn deadlines_have_exact_quantum_and_latest_wakes_are_taken_once() {
                let mut core = Core::default();
                let now = Instant::now();
                let old = Arc::new(Count::default());
                let latest = Arc::new(Count::default());
                for id in 0..65 {
                    core.register(
                        id,
                        Policy {
                            due: now,
                            error: Rc::new(Cell::new(Some(3))),
                        },
                        true,
                        true,
                    );
                    let old = Waker::from(old.clone());
                    let latest = Waker::from(latest.clone());
                    let retired = {
                        let slot = &mut core.waiters.get_mut(&id).unwrap().waker;
                        (slot.replace(old), slot.replace(latest))
                    };
                    drop(retired);
                }
                let mut wakes = Vec::new();
                core.refresh(true, 9, 8, || now, split, &mut wakes);
                assert_eq!(core.deadlines.len(), 1);
                assert_eq!(wakes.len(), 64);
                assert!(core.waiters[&64].error.is_none());
                core.refresh(true, 9, 8, || now, split, &mut wakes);
                assert!(core.deadlines.is_empty());
                assert_eq!(wakes.len(), 65);
                core.notify(&mut wakes);
                assert_eq!(wakes.len(), 65);
                for wake in wakes {
                    wake.wake();
                }
                assert_eq!(old.0.load(Ordering::Relaxed), 0);
                assert_eq!(latest.0.load(Ordering::Relaxed), 65);
            }

            /// Shutdown clears caller indexes but retains a pending completion outcome.
            #[test]
            fn detach_cleans_deadline_and_stop_preserves_pending_completion() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, true, true);
                core.detach(1);
                assert_eq!(core.deadlines.len(), 1);
                let mut identity = identity();
                assert_eq!(core.elect(2, &mut identity, 3), Ok(true));
                core.cancel(2, 6);
                assert_eq!(core.waiters[&2].error, Some(6));
                let mut wakes = Vec::new();
                core.stop(8, &mut wakes);
                core.settle(false, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Draining(Outcome::Failed(8))));
                assert!(core.waiters.is_empty());
                assert!(core.deadlines.is_empty());
                core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Failed(8)));
            }
        }
    }
}
