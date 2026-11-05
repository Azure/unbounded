use super::*;
use std::{
    collections::BTreeSet,
    rc::Rc,
    sync::atomic::{AtomicBool, Ordering},
};

/// Inert lifecycle policy for tests that control shutdown explicitly.
#[derive(Clone)]
struct TestScope;

impl Scope for TestScope {
    /// Use portable runtime failures for lifecycle assertions.
    type Error = Error;

    /// Leave all lifecycle phases authorized.
    fn check(&self) -> Result<()> {
        Ok(())
    }
}

/// Compute wait policy after polling.
#[test]
fn steady_state_wait_is_post_poll_bounded() {
    /// Service that exposes whether polling preceded its wait-policy callback.
    struct Waiting {
        polled: bool,

        wait: Duration,
    }

    impl Service<TestScope> for Waiting {
        /// Complete startup without external resources.
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }

        /// Verify the lane budget and record a completed service turn.
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
            assert_eq!(budget, WORK_BUDGET);
            self.polled = true;
            Ok(())
        }

        /// Assert post-poll ordering.
        fn wait_timeout(&self, maximum: Duration) -> Duration {
            assert!(self.polled, "wait policy must observe the completed poll");
            assert_eq!(maximum, FOREVER);
            self.wait
        }

        /// Complete drain without retaining any resources.
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }

        /// Complete shutdown without additional work.
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }
    }
    let mut cx = Context::from_waker(Waker::noop());
    for wait in [Duration::ZERO, IDLE_WAIT / 2, IDLE_WAIT, IDLE_WAIT * 2] {
        let mut service = Waiting {
            polled: false,
            wait,
        };
        assert_eq!(
            poll_turn(&mut service, &mut cx, &TestScope, false),
            Ok(wait)
        );
        service.polled = false;
        assert_eq!(
            poll_turn(&mut service, &mut cx, &TestScope, true),
            Ok(wait.min(IDLE_WAIT))
        );
    }
}

/// Thread-exit accounting must not grant ownership-fence authority.
#[test]
fn exit_is_not_a_fence() {
    let control = Arc::new(Control::<Error>::new(1, 0, false));
    drop(Exit {
        control: control.clone(),
        lane: Some(0),
    });
    assert!(!control.lock().fenced[0]);
    assert_eq!(control.lock().stats.done, 1);
}

/// Both drivers retain ownership through the second fence until explicit release.
#[test]
fn service_owner_requires_both_fences_and_explicit_release() {
    for helper in [false, true] {
        let events = Arc::new(Mutex::new(Vec::new()));
        let mut owner = ServiceOwner::new(Box::new(Local {
            events: events.clone(),
            owner: Rc::new(thread::current().id()),
            stop: Arc::default(),
            drain_error: None,
        }));
        let control = Control::new(1, usize::from(helper), true);
        let waker = uring_runtime::drivers::thread_waker(None);
        assert_eq!(owner.stage, ReleaseStage::Live);
        if helper {
            drive_helpers(
                vec![Box::pin(owner.fence_helper(&TestScope, &control, &waker))],
                &control,
                &waker,
                None,
                || {},
            );
        } else {
            owner.fence_lane_blocking(&TestScope, &control, &waker);
        }
        assert_eq!(owner.stage, ReleaseStage::Drained);
        assert_eq!(*events.lock().unwrap(), ["fence"]);
        if helper {
            drive_helpers(
                vec![owner.shutdown_helper(&TestScope, &control, &waker)],
                &control,
                &waker,
                None,
                || {},
            );
        } else {
            owner.shutdown_lane_blocking(&TestScope, &control, &waker);
        }
        assert_eq!(owner.stage, ReleaseStage::ShutdownFenced);
        assert_eq!(*events.lock().unwrap(), ["fence", "shutdown", "fence"]);
        owner.release();
        assert_eq!(owner.stage, ReleaseStage::Releasable);
        assert_eq!(*events.lock().unwrap(), ["fence", "shutdown", "fence"]);
        drop(owner);
        assert_eq!(
            *events.lock().unwrap(),
            ["fence", "shutdown", "fence", "drop"]
        );
        assert_eq!(control.result(), Ok(()));
    }
}

/// A requested stop (`Cancelled`) yields to the first real failure after
/// it, so post-stop drain or checkpoint errors are not reported as a clean
/// stop. Real failures are never replaced.
#[test]
fn stop_yields_to_later_failures_but_failures_are_kept() {
    let control = Control::<Error>::new(1, 0, false);
    control.fail(Error::Cancelled);
    assert_eq!(control.result(), Err(Error::Cancelled));
    control.fail(Error::Cancelled);
    assert_eq!(control.result(), Err(Error::Cancelled));
    control.fail(Error::Io);
    assert_eq!(control.result(), Err(Error::Io));
    control.fail(Error::DeadlineExceeded);
    control.fail(Error::Cancelled);
    assert_eq!(control.result(), Err(Error::Io));

    let control = Control::<Error>::new(1, 0, false);
    control.fail(Error::DeadlineExceeded);
    control.fail(Error::Cancelled);
    assert_eq!(control.result(), Err(Error::DeadlineExceeded));
}

/// Failed fences and incomplete ownership stages abort before service destruction.
#[test]
fn fence_failure_aborts_before_service_drop() {
    use std::os::unix::process::ExitStatusExt;

    const CHILD: &str = "RUNTIME_FENCE_FAILURE_CHILD";
    if let Ok(mode) = std::env::var(CHILD) {
        // Do not produce core files for the deliberately fatal child.
        let limit = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        unsafe {
            libc::setrlimit(libc::RLIMIT_CORE, &limit);
            libc::prctl(libc::PR_SET_DUMPABLE, 0);
        }

        /// Service that injects a chosen fence failure and detects unsafe release.
        struct Fatal {
            mode: String,

            fences: usize,
        }

        impl Drop for Fatal {
            /// Exit distinctly if the failed-fence service is ever destroyed.
            fn drop(&mut self) {
                std::process::exit(99);
            }
        }

        impl Service<TestScope> for Fatal {
            /// Finish startup so the test can reach the selected fence.
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Perform no steady-state work before the injected fence failure.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }

            /// Finish drain without masking the injected fence result.
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Finish shutdown to reach the second fence when requested.
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Inject construction, polling, or result failure at the chosen fence.
            fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                self.fences += 1;
                if self.mode.contains("pending") && self.fences == 2 {
                    return Box::pin(std::future::pending());
                }
                let fail = !self.mode.starts_with("owner")
                    && self.fences == if self.mode.contains("second") { 2 } else { 1 };
                assert!(
                    !(fail && self.mode.contains("construct")),
                    "fence construction"
                );
                Box::pin(async move { if fail { Err(Error::Io) } else { Ok(()) } })
            }
        }

        /// Construct a service configured for the child process's failure mode.
        struct FatalFactory(String);

        impl Factory<TestScope> for FatalFactory {
            /// Create a fresh service with neither ownership fence completed.
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                Ok(Box::new(Fatal {
                    mode: self.0.clone(),
                    fences: 0,
                }))
            }
        }
        if mode.starts_with("owner") {
            let control = Control::new(1, 0, true);
            let factory = FatalFactory(mode.clone());
            let mut owner = ServiceOwner::new(factory.build_lane(0).unwrap());
            let waker = uring_runtime::drivers::thread_waker(None);
            if !mode.contains("live") {
                owner.fence_lane_blocking(&TestScope, &control, &waker);
            }
            if mode.contains("shutdown-fenced") {
                owner.shutdown_lane_blocking(&TestScope, &control, &waker);
            }
            if mode.contains("unpolled") || mode.contains("pending") {
                let mut operation = owner.shutdown_helper(&TestScope, &control, &waker);
                if mode.contains("pending") {
                    let mut cx = Context::from_waker(&waker);
                    assert!(operation.as_mut().poll(&mut cx).is_pending());
                }
                drop(operation);
                assert_eq!(owner.stage, ReleaseStage::Drained);
            }
            if mode.ends_with("release") {
                let released = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    owner.release();
                }));
                assert!(released.is_err());
            }
            drop(owner);
        } else if mode.starts_with("helper") {
            let control = Control::new(1, 1, true);
            control.close(0);
            control.fence(0);
            let factory = FatalFactory(mode.clone());
            let mut owner = ServiceOwner::new(factory.build_lane(0).unwrap());
            let waker = uring_runtime::drivers::thread_waker(None);
            drive_helpers(
                vec![helper_service(
                    0,
                    &mut owner,
                    &factory,
                    &TestScope,
                    &control,
                    &waker,
                    &Cell::new(FOREVER),
                )],
                &control,
                &waker,
                None,
                || {},
            );
            // Exercise the post-shutdown helper fence as a separate pass.
            drive_helpers(
                vec![owner.shutdown_helper(&TestScope, &control, &waker)],
                &control,
                &waker,
                None,
                || {},
            );
        } else {
            let mut plan = plan(1);
            plan.max_threads = 1;
            let control = Arc::new(Control::new(1, 0, true));
            control.lock().helper_ready[0] = true;
            control.set_phase(Phase::Shutdown);
            let _ = lane_thread(&FatalFactory(mode), &plan.lanes[0], 0, TestScope, &control);
        }
        std::process::exit(98);
    }
    for mode in [
        "lane-error",
        "lane-construct",
        "lane-second-error",
        "lane-second-construct",
        "helper-error",
        "helper-construct",
        "helper-second-error",
        "helper-second-construct",
        "owner-live-drop",
        "owner-drained-drop",
        "owner-shutdown-fenced-drop",
        "owner-live-release",
        "owner-drained-release",
        "owner-unpolled-release",
        "owner-pending-release",
    ] {
        let status = std::process::Command::new("timeout")
            .args(["--signal=TERM", "--kill-after=1s", "10s"])
            .arg(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "lifecycle::tests::fence_failure_aborts_before_service_drop",
                "--nocapture",
            ])
            .env(CHILD, mode)
            .status()
            .unwrap();
        assert_eq!(status.signal(), Some(libc::SIGABRT), "{mode}: {status}");
    }
}

/// Expired teardown policy reports an error without dropping a pending helper future.
#[test]
fn helper_shutdown_deadline_is_diagnostic_not_detachment() {
    /// Teardown policy that is expired on every check.
    #[derive(Clone)]
    struct Expired;

    impl Scope for Expired {
        /// Use runtime errors to preserve the deadline diagnosis.
        type Error = Error;

        /// Report expiry without granting permission to release live ownership.
        fn check(&self) -> Result<()> {
            Err(Error::DeadlineExceeded)
        }
    }
    let control = Control::new(0, 1, true);
    let polls = std::cell::Cell::new(0);
    // The pending operation wakes itself, standing in for a completion source:
    // helpers park between passes instead of spinning.
    let operation = Box::pin(std::future::poll_fn(|cx| {
        polls.set(polls.get() + 1);
        if polls.get() == 4 {
            Poll::Ready(Ok(()))
        } else {
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    }));
    let waker = uring_runtime::drivers::thread_waker(None);
    drive_helpers(
        vec![diagnose_operation(operation, &Expired, &control)],
        &control,
        &waker,
        None,
        || {},
    );
    assert_eq!(polls.get(), 4);
    assert_eq!(control.result(), Err(Error::DeadlineExceeded));
}

/// Place one lane on an allowed CPU with a caller-selected thread budget.
fn plan(max_threads: usize) -> Plan {
    let cpu = *affinity::current_cpus().unwrap().first().unwrap();
    Plan {
        lanes: vec![Lane {
            name: "generic-lane".into(),
            cpu,
        }],
        helpers: Vec::new(),
        max_threads,
    }
}

/// Construction policy and shared lifecycle instrumentation for a local service.
struct Recipe {
    cpu: usize,

    events: Arc<Mutex<Vec<&'static str>>>,

    stop: Arc<AtomicBool>,

    drain_error: Option<Error>,
}

/// Non-Send service that records lifecycle order and verifies owner-thread destruction.
struct Local {
    events: Arc<Mutex<Vec<&'static str>>>,

    owner: Rc<thread::ThreadId>,

    stop: Arc<AtomicBool>,

    drain_error: Option<Error>,
}

impl Factory<TestScope> for Recipe {
    /// Verify affinity before constructing a thread-confined service.
    fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
        assert_eq!(
            affinity::current_cpus().unwrap(),
            BTreeSet::from([self.cpu])
        );
        Ok(Box::new(Local {
            events: self.events.clone(),
            owner: Rc::new(thread::current().id()),
            stop: self.stop.clone(),
            drain_error: self.drain_error,
        }))
    }

    /// Reject helper construction because this fixture plans only lanes.
    fn build_helper(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
        unreachable!()
    }
}

impl Drop for Local {
    /// Verify owner-thread destruction and record the final lifecycle event.
    fn drop(&mut self) {
        assert_eq!(*self.owner, thread::current().id());
        self.events.lock().unwrap().push("drop");
    }
}

impl Service<TestScope> for Local {
    /// Record startup when its future is driven.
    fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
        Box::pin(async {
            self.events.lock().unwrap().push("start");
            Ok(())
        })
    }

    /// Verify bounded polling and inject an explicit stop request.
    fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
        assert_eq!(budget, 64);
        if self.stop.load(Ordering::SeqCst) {
            Err(Error::Cancelled)
        } else {
            Ok(())
        }
    }

    /// Record that admission stops before drain begins.
    fn stop_admission(&mut self) -> Result<()> {
        self.events.lock().unwrap().push("stop");
        Ok(())
    }

    /// Record completed draining of accepted work.
    fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
        Box::pin(async {
            self.events.lock().unwrap().push("drain");
            self.drain_error.map_or(Ok(()), Err)
        })
    }

    /// Record closure of helper submissions after drain.
    fn close(&mut self) -> Result<()> {
        self.events.lock().unwrap().push("close");
        Ok(())
    }

    /// Record each ownership fence in lifecycle order.
    fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
        Box::pin(async {
            self.events.lock().unwrap().push("fence");
            Ok(())
        })
    }

    /// Record shutdown between the two ownership fences.
    fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
        Box::pin(async {
            self.events.lock().unwrap().push("shutdown");
            Ok(())
        })
    }
}

/// Create lifecycle instrumentation for the plan's coordinator lane.
fn recipe(plan: &Plan) -> Recipe {
    Recipe {
        cpu: plan.lanes[0].cpu,
        events: Arc::default(),
        stop: Arc::default(),
        drain_error: None,
    }
}

/// A stop requested while running is a clean `Cancelled`, but a drain
/// failure after that stop is reported instead of being swallowed.
#[test]
fn drain_failure_after_stop_is_reported() {
    for (drain_error, expected) in [(None, Error::Cancelled), (Some(Error::Io), Error::Io)] {
        let plan = plan(1);
        let mut factory = recipe(&plan);
        factory.drain_error = drain_error;
        factory.stop.store(true, Ordering::SeqCst);
        let mut group = Group::new(plan);
        assert_eq!(group.run(&factory, &TestScope), Err(expected));
        assert!(factory.events.lock().unwrap().contains(&"drain"));
    }
}

/// Dropping an owned group joins its lane after both fences and local destruction.
#[test]
fn owned_drop_joins_local_service_without_helpers() {
    let plan = plan(2);
    let factory = Arc::new(recipe(&plan));
    {
        let mut group = Group::new(plan);
        group.start(factory.clone(), &TestScope).unwrap();
        assert_eq!(group.stats().ready, 1);
    }
    assert_eq!(
        *factory.events.lock().unwrap(),
        [
            "start", "stop", "drain", "close", "fence", "shutdown", "fence", "drop"
        ]
    );
}

/// Reject invalid placement, duplicate helper ownership, and exhausted thread budgets.
#[test]
fn plans_reject_invalid_links_and_count_external_caller() {
    let plan = plan(1);
    let group = Group::<TestScope>::new(plan.clone());
    assert_eq!(group.validate(true), Ok(()));
    assert_eq!(group.validate(false), Err(Error::InvalidConfiguration));
    for lanes in [vec![], vec![1], vec![0, 0]] {
        let mut invalid = plan.clone();
        invalid.max_threads = 3;
        invalid.helpers.push(Helper {
            name: "helper".into(),
            cpu: plan.lanes[0].cpu,
            lanes,
        });
        assert_eq!(
            Group::<TestScope>::new(invalid).validate(true),
            Err(Error::InvalidConfiguration)
        );
    }
    for helper in [false, true] {
        let mut invalid = plan.clone();
        invalid.max_threads = 3;
        if helper {
            invalid.helpers.push(Helper {
                name: "bad\0helper".into(),
                cpu: plan.lanes[0].cpu,
                lanes: vec![0],
            });
        } else {
            invalid.lanes[0].name = "bad\0lane".into();
        }
        assert_eq!(
            Group::<TestScope>::new(invalid).validate(true),
            Err(Error::InvalidConfiguration)
        );
    }
    let mut invalid = plan.clone();
    invalid.lanes[0].cpu = usize::MAX;
    assert_eq!(
        Group::<TestScope>::new(invalid).validate(true),
        Err(Error::InvalidConfiguration)
    );
    let mut invalid = plan.clone();
    invalid.max_threads = 3;
    let helper = Helper {
        name: "helper".into(),
        cpu: plan.lanes[0].cpu,
        lanes: vec![0],
    };
    invalid.helpers = vec![helper.clone(), helper];
    assert_eq!(
        Group::<TestScope>::new(invalid).validate(true),
        Err(Error::InvalidConfiguration)
    );
    let mut empty = plan;
    empty.lanes.clear();
    assert_eq!(
        Group::<TestScope>::new(empty).validate(true),
        Err(Error::InvalidConfiguration)
    );
}

/// Sibling helper fences progress cooperatively before drain accounting and destruction.
#[test]
fn helper_independent_resources_fence_cooperatively_before_barrier_and_drop() {
    use std::sync::atomic::AtomicUsize;

    /// Cross-helper fence-entry, completion, and destruction instrumentation.
    struct Shared {
        entered: [AtomicUsize; 2],

        completed: [AtomicUsize; 2],

        dropped: AtomicUsize,
    }

    /// Factory for lanes and helpers sharing cooperative fence state.
    struct Helpers {
        shared: Arc<Shared>,
    }

    /// Thread-confined helper with independent resources requiring both fences.
    struct Independent {
        shared: Arc<Shared>,

        owner: Rc<thread::ThreadId>,

        fences: usize,
    }

    /// Resource-free lane used to isolate helper-owned fence behavior.
    struct Empty;

    impl Factory<TestScope> for Helpers {
        /// Construct a lane without independent resource dependencies.
        fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
            Ok(Box::new(Empty))
        }

        /// Construct a local helper that must fence alongside its sibling.
        fn build_helper(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
            Ok(Box::new(Independent {
                shared: self.shared.clone(),
                owner: Rc::new(thread::current().id()),
                fences: 0,
            }))
        }
    }

    impl Service<TestScope> for Empty {
        /// Complete the resource-free lane's startup.
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }

        /// Keep the lane idle while helper lifecycles are exercised.
        fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
            Ok(())
        }

        /// Finish draining so the helper can begin its independent fence.
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }

        /// Complete the resource-free lane's shutdown.
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }
    }

    impl Service<TestScope> for Independent {
        /// Finish helper startup before cooperative resource fencing.
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }

        /// Verify that each helper shard receives a bounded cooperative turn.
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
            assert_eq!(budget, 1);
            Ok(())
        }

        /// Finish drain while retaining independently fenced helper resources.
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }

        /// Require the sibling to enter the same fence before completing this one.
        fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            let phase = self.fences;
            self.fences += 1;
            self.shared.entered[phase].fetch_add(1, Ordering::SeqCst);
            Box::pin(std::future::poll_fn(move |_| {
                // Each independent owner needs its sibling's fence to run.
                // Sequential helper fencing would deadlock this lifecycle.
                if self.shared.entered[phase].load(Ordering::SeqCst) != 2 {
                    return Poll::Pending;
                }
                self.shared.completed[phase].fetch_add(1, Ordering::SeqCst);
                Poll::Ready(Ok(()))
            }))
        }

        /// Verify the first fence barrier and inject an ordinary shutdown failure.
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                assert_eq!(self.shared.completed[0].load(Ordering::SeqCst), 2);
                // A shutdown failure still requires the second ownership fence.
                Err(Error::Io)
            })
        }
    }

    impl Drop for Independent {
        /// Verify owner-thread destruction only after both independent fences ran.
        fn drop(&mut self) {
            assert_eq!(*self.owner, thread::current().id());
            assert_eq!(self.fences, 2);
            assert_eq!(self.shared.completed[1].load(Ordering::SeqCst), 2);
            self.shared.dropped.fetch_add(1, Ordering::SeqCst);
        }
    }
    let shared = Arc::new(Shared {
        entered: Default::default(),
        completed: Default::default(),
        dropped: AtomicUsize::new(0),
    });
    let mut plan = plan(4);
    plan.lanes.push(plan.lanes[0].clone());
    plan.helpers.push(Helper {
        name: "independent-helpers".into(),
        cpu: plan.lanes[0].cpu,
        lanes: vec![0, 1],
    });
    let mut group = Group::new(plan);
    group
        .start(
            Arc::new(Helpers {
                shared: shared.clone(),
            }),
            &TestScope,
        )
        .unwrap();
    group.drain(&TestScope).unwrap();
    assert_eq!(
        shared.completed[0].load(Ordering::SeqCst),
        2,
        "drained must include independent helper fences"
    );
    assert_eq!(group.join(), Err(Error::Io));
    assert_eq!(shared.completed[1].load(Ordering::SeqCst), 2);
    assert_eq!(shared.dropped.load(Ordering::SeqCst), 2);
}

/// Counts wakes for `drive_local_with` regressions.
#[derive(Default)]
struct DriveWakeCounter(std::sync::atomic::AtomicUsize);

impl DriveWakeCounter {
    fn count(&self) -> usize {
        self.0.load(std::sync::atomic::Ordering::SeqCst)
    }
}

impl std::task::Wake for DriveWakeCounter {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
}

/// A pending operation does not wake its own task; only its resources do.
#[test]
fn driver_does_not_self_wake() {
    let ready = Cell::new(false);
    let mut driver = drive_local_with(
        Box::pin(std::future::poll_fn(|_| {
            if ready.get() {
                Poll::Ready(Ok::<_, Error>(7))
            } else {
                Poll::Pending
            }
        })),
        None,
        |_| Ok(()),
    );
    let count = Arc::new(DriveWakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    assert!(driver.as_mut().poll(&mut cx).is_pending());
    assert_eq!(count.count(), 0);
    ready.set(true);
    assert_eq!(driver.as_mut().poll(&mut cx), Poll::Ready(Ok(7)));
}

/// Backend failures retain their first error without truncating operation completion.
#[test]
fn driver_retains_operation_and_first_backend_error_until_completion() {
    for fail_poll in [false, true] {
        let ready = Cell::new(false);
        let operation = Box::pin(std::future::poll_fn(|_| {
            if ready.get() {
                Poll::Ready(Err::<(), _>(Error::Unavailable))
            } else {
                Poll::Pending
            }
        }));
        let mut future = drive_local_with(operation, None, |_| {
            if fail_poll { Err(Error::Io) } else { Ok(()) }
        });
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        ready.set(true);
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(if fail_poll {
                Error::Io
            } else {
                Error::Unavailable
            }))
        );
    }
}

/// An already completed operation preserves its result.
#[test]
fn driver_preserves_ready_value_and_does_not_wait() {
    for result in [Ok(7), Err(Error::Unavailable)] {
        let mut future = drive_local_with(Box::pin(async move { result }), None, |_| Ok(()));
        assert_eq!(
            future
                .as_mut()
                .poll(&mut Context::from_waker(Waker::noop())),
            Poll::Ready(result)
        );
    }
}
