//! Adapter contracts: scope precedence, scope scans, scope fences, partitions,
//! and rejection observation. Ring ownership is covered by `racer-io`.
use super::*;
use fixtures::{Admission, Limits, Reactor, RequestScope};
use std::{collections::BTreeMap, num::NonZeroUsize, task::Context, time::Instant};

pub(super) mod fixtures {
    //! Counting guards and a scoped adapter for raw completion ownership tests.
    use super::*;
    use std::ops::Deref;

    /// Independently counted connection leases and request bookkeeping bytes.
    #[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
    pub enum ResourceClass {
        /// Allocation and completion bookkeeping charged through the runtime budget.
        RequestContext,
    }

    /// Queue ceiling selected by each ownership regression.
    pub struct Limits {
        /// Maximum accepted operations across ordinary and reserved partitions.
        pub queue_entries: NonZeroUsize,
    }

    /// Shared accounting ledger for observing exact resource release boundaries.
    pub struct Admission {
        /// Queue ceiling used when constructing the associated reactor.
        pub limits: Limits,

        used: Rc<RefCell<BTreeMap<ResourceClass, usize>>>,
    }

    /// One counted resource amount released by its owner's destructor.
    pub struct Reservation {
        used: Rc<RefCell<BTreeMap<ResourceClass, usize>>>,

        class: ResourceClass,

        amount: usize,
    }

    impl Drop for Reservation {
        /// Return the guard's exact charged amount to the shared ledger.
        fn drop(&mut self) {
            *self.used.borrow_mut().entry(self.class).or_default() -= self.amount;
        }
    }

    impl Admission {
        /// Start an empty resource ledger with the selected queue limit.
        pub fn new(limits: Limits) -> Self {
            Self {
                limits,
                used: Rc::default(),
            }
        }

        /// Charge one class and return an independently owned release guard.
        pub fn reserve(
            &self,
            _: Option<&()>,
            class: ResourceClass,
            amount: usize,
        ) -> Result<Reservation> {
            *self.used.borrow_mut().entry(class).or_default() += amount;
            Ok(Reservation {
                used: self.used.clone(),
                class,
                amount,
            })
        }

        /// Observe live guards without mutating the accounting ledger.
        pub fn used(&self, class: ResourceClass) -> usize {
            *self.used.borrow().get(&class).unwrap_or(&0)
        }
    }

    /// Adapt the ledger to the runtime's public allocation-budget contract.
    pub struct CountingBudget(Rc<Admission>);

    impl Budget for CountingBudget {
        /// Exact ledger guard retained by the charged runtime owner.
        type Charge = Reservation;

        /// Charge completion memory through the observable test ledger.
        fn charge(&self, bytes: usize) -> Result<Reservation> {
            self.0.reserve(None, ResourceClass::RequestContext, bytes)
        }
    }

    /// Scoped reactor with an accessible ledger for private completion regressions.
    pub struct Reactor {
        core: super::super::Reactor<RequestScope, CountingBudget>,

        /// Resource accounting shared with buffers, leases, and completion owners.
        pub admission: Rc<Admission>,
    }

    impl Reactor {
        /// Construct a lazy reactor using the ledger's queue ceiling.
        pub fn new(admission: Rc<Admission>) -> Self {
            Self {
                core: super::super::Reactor::new(
                    admission.limits.queue_entries.get(),
                    CountingBudget(admission.clone()),
                    &crate::reactor::simulation::ambient_env(),
                ),
                admission,
            }
        }

        /// Fence a snapshot of all current operations while leaving admission open.
        pub fn file_fence(&self, _: ()) -> Operation<'_, ()> {
            self.core.fence_matching(|_| true)
        }
    }

    impl Deref for Reactor {
        /// Production implementation exercised by the test adapter.
        type Target = super::super::Reactor<RequestScope, CountingBudget>;

        /// Expose the production reactor without duplicating operation implementations.
        fn deref(&self) -> &Self::Target {
            &self.core
        }
    }

    /// Deadline and cancellation policy retained independently by each operation.
    #[derive(Clone)]
    pub struct RequestScope {
        /// Absolute deadline in the active clock domain.
        pub deadline: Deadline,

        /// Shared cancellation state, checked before deadline expiration.
        pub cancellation: Cancellation,
    }

    impl RequestScope {
        /// Create a fresh cancellation source with a caller-selected deadline.
        pub fn new(_: (), deadline: Instant) -> Result<Self> {
            Ok(Self {
                deadline: Deadline(deadline),
                cancellation: Cancellation::new()?,
            })
        }

        /// Request cancellation without treating the request as a kernel fence.
        pub fn cancel(&self) -> Result<()> {
            self.cancellation.cancel()
        }
    }

    impl Scope for RequestScope {
        /// Runtime classification used by ownership regressions.
        type Error = Error;

        /// Give cancellation precedence over expiration in the active clock domain.
        fn check(&self) -> Result<()> {
            if self.cancellation.is_cancelled() {
                Err(Error::Cancelled)
            } else if crate::environment::now() >= self.deadline.0 {
                Err(Error::DeadlineExceeded)
            } else {
                Ok(())
            }
        }
    }
}
use crate::environment::{Cancellation, Deadline};

/// Build a nonzero queue ceiling for a regression's admitted operation count.
fn limits(capacity: usize) -> Limits {
    Limits {
        queue_entries: NonZeroUsize::new(capacity).unwrap(),
    }
}

/// Create fresh cancellation state and a five-second deadline for a test operation.
pub(super) fn scope() -> RequestScope {
    RequestScope::new((), Instant::now() + Duration::from_secs(5)).unwrap()
}

/// Poll delivery once without advancing the kernel or simulated driver.
pub(super) fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}

/// Drive explicit completion turns under a host deadline until delivery finishes.
pub(super) fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(result) = poll(&mut future) {
            return result;
        }
        assert!(Instant::now() < deadline, "reactor failed to make progress");
        reactor.poll_budgeted(8).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

/// Initialize a host reactor, skipping only unsupported or policy-denied io_uring.
pub(super) fn kernel_reactor(capacity: usize) -> Option<Reactor> {
    // Skip only when the kernel lacks io_uring or policy denies ring creation.
    // Configuration, quota, opcode, and ordinary I/O errors must fail tests.
    match io_uring::IoUring::new(2) {
        Ok(ring) => drop(ring),
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
            ) =>
        {
            assert_ne!(
                std::env::var("RUNTIME_REQUIRE_IO_URING").as_deref(),
                Ok("1"),
                "RUNTIME_REQUIRE_IO_URING=1 but io_uring is unavailable: {error}"
            );
            eprintln!("io_uring kernel test unavailable: {error}");
            return None;
        }
        Err(error) => panic!("unexpected io_uring setup failure: {error}"),
    }
    let reactor = Reactor::new(Rc::new(Admission::new(limits(capacity))));
    reactor.init().unwrap();
    Some(reactor)
}

#[cfg(feature = "simulation")]
#[cfg(feature = "simulation")]
mod adapter {
    use super::*;
    use simulation::Simulation;

    fn sim_reactor(
        capacity: usize,
    ) -> (Simulation, crate::reactor::simulation::Environment, Reactor) {
        let sim = Simulation::new();
        let environment = sim.enter();
        let reactor = Reactor::new(Rc::new(Admission::new(limits(capacity))));
        (sim, environment, reactor)
    }

    #[test]
    fn failed_scope_is_reported_before_submission() {
        let (sim, _environment, r) = sim_reactor(4);
        let (_left, right) = sim.socket_pair();
        let scope = scope();
        scope.cancel().unwrap();
        let mut recv = r.recv(Rc::new(right), vec![0; 1], (), &scope);
        assert!(matches!(
            poll(&mut recv),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn scan_cancels_operations_whose_scope_fails_and_scope_error_wins() {
        let (sim, _environment, r) = sim_reactor(4);
        let (_left, right) = sim.socket_pair();
        let clock = crate::environment::now();
        let scope = RequestScope::new((), clock + Duration::from_secs(60)).unwrap();
        let mut recv = r.recv(Rc::new(right), vec![0; 1], (), &scope);
        assert!(poll(&mut recv).is_pending());
        assert_eq!(r.in_flight(), 1);
        // A deadline-only scope fails its check without a cancel token firing:
        // the scan must cancel the kernel operation.
        let mut expired = scope.clone();
        expired.deadline = Deadline(clock);
        let mut recv_expired = {
            let (_l, rr) = sim.socket_pair();
            r.recv(Rc::new(rr), vec![0; 1], (), &expired)
        };
        assert!(matches!(
            poll(&mut recv_expired),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        scope.cancel().unwrap();
        assert!(matches!(drive(&r, recv), Err(Error::Cancelled)));
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn fence_matching_cancels_only_matching_scopes() {
        let (sim, _environment, r) = sim_reactor(4);
        let (_a, left) = sim.socket_pair();
        let (_b, right) = sim.socket_pair();
        let first = scope();
        let second = scope();
        let mut one = r.recv(Rc::new(left), vec![0; 1], (), &first);
        let mut two = r.recv(Rc::new(right), vec![0; 1], (), &second);
        assert!(poll(&mut one).is_pending());
        assert!(poll(&mut two).is_pending());
        let target = first.cancellation.clone();
        drive(
            &r,
            r.fence_matching(move |scope| scope.cancellation.token().same(target.token())),
        )
        .unwrap();
        assert!(matches!(poll(&mut one), Poll::Ready(Err(Error::Cancelled))));
        assert!(poll(&mut two).is_pending());
        assert_eq!(r.in_flight(), 1);
        drop(two);
        drive(&r, r.drain()).unwrap();
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn reserved_partition_keeps_control_slots_and_reports_rejections() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let rejections = Rc::new(RefCell::new(Vec::new()));
        let seen = rejections.clone();
        let r = super::super::Reactor::<RequestScope, ()>::new(
            2,
            (),
            &crate::reactor::simulation::ambient_env(),
        )
        .with_rejection_observer(move |_, facts: SubmissionRejection| {
            seen.borrow_mut().push(facts.reason)
        });
        let capacity = r.reserve_submissions(1, ()).unwrap();
        assert_eq!(capacity.capacity(), 1);
        assert!(r.reserve_submissions(1, ()).is_err());
        let scope = scope();
        let (_a, a) = sim.socket_pair();
        let (_b, b) = sim.socket_pair();
        let (_c, c) = sim.socket_pair();
        let mut ordinary = r.recv(Rc::new(a), vec![0; 1], (), &scope);
        let mut rejected = r.recv(Rc::new(b), vec![0; 1], (), &scope);
        let mut control = r.recv_reserved(Rc::new(c), vec![0; 1], capacity.clone(), &scope);
        assert!(poll(&mut ordinary).is_pending());
        assert!(matches!(
            poll(&mut rejected),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(poll(&mut control).is_pending());
        assert!(matches!(
            rejections.borrow().as_slice(),
            [RejectionReason::OrdinaryFull]
        ));
        drop((ordinary, control));
        r.shutdown(Duration::from_secs(5)).unwrap();
    }

    #[test]
    fn connect_observation_records_errno() {
        let (sim, _environment, r) = sim_reactor(4);
        let socket = Rc::new(sim.socket(libc::AF_INET).unwrap());
        let observed = Rc::new(Cell::new(None));
        let address = SocketAddress::Inet("127.0.0.1:9".parse().unwrap());
        let result = drive(
            &r,
            r.connect_with_observation(socket, address, (), Some(observed.clone()), &scope()),
        );
        assert!(matches!(result, Err(Error::Os(_))));
        assert!(observed.get().is_some());
    }
}

#[test]
fn constructor_does_not_open_kernel_resources() {
    let r = Reactor::new(Rc::new(Admission::new(limits(1))));
    assert_eq!(r.in_flight(), 0);
    assert_eq!(r.poll_budgeted(8), Ok(0));
}

#[test]
fn kernel_reactor_reads_and_writes_files() {
    let Some(r) = kernel_reactor(4) else {
        return;
    };
    let path = std::env::temp_dir().join(format!("uring-runtime-adapter-{}", std::process::id()));
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(true)
        .read(true)
        .write(true)
        .open(&path)
        .unwrap();
    std::fs::remove_file(&path).unwrap();
    let fd = Rc::new(Descriptor::from(std::os::fd::OwnedFd::from(file)));
    let scope = scope();
    let written = drive(&r, r.write_at(fd.clone(), 3, b"abc".to_vec(), (), &scope)).unwrap();
    assert_eq!(written.bytes, 3);
    let read = drive(&r, r.read_at(fd, 0, vec![9; 6], (), &scope)).unwrap();
    assert_eq!(read.buffer, [0, 0, 0, b'a', b'b', b'c']);
}
