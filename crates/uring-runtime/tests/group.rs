//! Group lifecycle, failure injection, helper cooperation and fatal policies.

use std::rc::Rc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::thread::{self, ThreadId};
use std::time::Duration;

use uring_runtime::affinity::{CpuSet, current_cpus};
use uring_runtime::backend::RemoteWaker;
use uring_runtime::group::{
    Factory, FailureReporter, FatalCause, FatalEvent, FatalPolicy, Group, GroupConfig, GroupError,
    LifecycleFuture, Plan, Role, Service, Stage, thread_waker,
};
use uring_runtime::{CancelToken, Error, ErrorKind, Scope, Unscoped};

type Events = Arc<Mutex<Vec<&'static str>>>;

const FULL: [&str; 8] = [
    "start",
    "stop_admission",
    "drain",
    "close",
    "fence1",
    "shutdown",
    "fence2",
    "drop",
];

fn first_cpu() -> usize {
    current_cpus().unwrap().first().unwrap()
}

fn one_lane() -> Plan {
    Plan::builder()
        .lane("test-lane", first_cpu())
        .build()
        .unwrap()
}

fn injected() -> Error {
    Error::new(ErrorKind::Overloaded).with_context("injected")
}

/// Which lifecycle step misbehaves, and how.
#[derive(Clone, Copy, Default)]
struct Fault {
    step: Option<&'static str>,
    panic: bool,
}

/// A non-`Send` service that records its lifecycle and injects one fault.
struct Scripted {
    events: Events,
    fault: Fault,
    fences: usize,
    owner: ThreadId,
    reporter: Option<FailureReporter<Error>>,
    report: bool,
    _local: Rc<()>,
}

impl Scripted {
    fn step(&mut self, name: &'static str) -> Result<(), Error> {
        let name = match name {
            "fence" => {
                self.fences += 1;
                if self.fences == 1 { "fence1" } else { "fence2" }
            }
            name => name,
        };
        self.events.lock().unwrap().push(name);
        self.check(name)
    }

    fn check(&self, name: &str) -> Result<(), Error> {
        if self.fault.step == Some(name) {
            assert!(!self.fault.panic, "injected {name} panic");
            return Err(injected());
        }
        Ok(())
    }
}

impl Drop for Scripted {
    fn drop(&mut self) {
        assert_eq!(self.owner, thread::current().id(), "dropped off its thread");
        self.events.lock().unwrap().push("drop");
    }
}

impl Service<Unscoped> for Scripted {
    fn set_failure_reporter(&mut self, reporter: FailureReporter<Error>) {
        self.reporter = Some(reporter);
    }

    fn start<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
        Box::pin(async move { self.step("start") })
    }

    fn poll(&mut self, _: &mut Context<'_>, budget: usize) -> Result<(), Error> {
        assert_eq!(budget, 64);
        if self.report {
            self.reporter.as_ref().unwrap().report(injected());
        }
        self.check("poll")
    }

    fn wait_timeout(&self, maximum: Duration) -> Duration {
        assert_eq!(maximum, Duration::from_millis(1));
        maximum
    }

    fn stop_admission(&mut self) -> Result<(), Error> {
        self.step("stop_admission")
    }

    fn drain<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
        Box::pin(async move { self.step("drain") })
    }

    fn close(&mut self) -> Result<(), Error> {
        self.step("close")
    }

    fn fence<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
        Box::pin(async move { self.step("fence") })
    }

    fn shutdown<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
        Box::pin(async move { self.step("shutdown") })
    }
}

#[derive(Clone, Default)]
struct Recipe {
    events: Events,
    fault: Fault,
    report: bool,
    cpu: Option<usize>,
    abandoned: Arc<AtomicUsize>,
}

impl Factory<Unscoped> for Recipe {
    fn build_lane(&self, _: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
        if self.fault.step == Some("abandon") {
            return Err(injected());
        }
        if let Some(cpu) = self.cpu {
            assert_eq!(current_cpus().unwrap(), CpuSet::from_iter([cpu]));
        }
        Ok(Box::new(Scripted {
            events: self.events.clone(),
            fault: self.fault,
            fences: 0,
            owner: thread::current().id(),
            reporter: None,
            report: self.report,
            _local: Rc::new(()),
        }))
    }

    fn abandon_lane(&self, _: usize) -> Result<(), Error> {
        self.abandoned.fetch_add(1, Ordering::SeqCst);
        if self.fault.step == Some("abandon") {
            return Err(injected());
        }
        Ok(())
    }
}

fn events(recipe: &Recipe) -> Vec<&'static str> {
    recipe.events.lock().unwrap().clone()
}

fn group<S>(plan: Plan) -> Group<S>
where
    S: Scope + Send,
    S::Error: Clone + Send,
{
    Group::new(plan, GroupConfig::default()).unwrap()
}

#[test]
fn owned_group_runs_the_full_lifecycle_on_the_pinned_thread() {
    let recipe = Recipe {
        cpu: Some(first_cpu()),
        ..Recipe::default()
    };
    let factory = Arc::new(recipe.clone());
    {
        let mut group = group(one_lane());
        group.start(factory, &Unscoped).unwrap();
        let stats = group.stats();
        assert_eq!((stats.total, stats.ready, stats.done), (1, 1, 0));
        assert!(stats.joinable);
        // Dropping joins after both fences.
    }
    assert_eq!(events(&recipe), FULL);
}

#[test]
fn explicit_drain_then_shutdown() {
    let recipe = Recipe::default();
    let mut group = group(one_lane());
    group.start(Arc::new(recipe.clone()), &Unscoped).unwrap();
    group.drain(&Unscoped).unwrap();
    assert_eq!(group.stats().drained, 1);
    assert_eq!(
        events(&recipe),
        ["start", "stop_admission", "drain", "close", "fence1"]
    );
    group.shutdown(&Unscoped).unwrap();
    assert_eq!(group.stats().done, 1);
    group.join().unwrap();
    assert!(!group.stats().joinable);
    assert_eq!(events(&recipe), FULL);
}

#[test]
fn run_uses_the_caller_as_lane_zero_and_restores_affinity() {
    let before = current_cpus().unwrap();
    let recipe = Recipe {
        fault: Fault {
            step: Some("poll"),
            panic: true,
        },
        cpu: Some(first_cpu()),
        ..Recipe::default()
    };
    let mut group = group(one_lane());
    let error = group.run(&recipe, &Unscoped).unwrap_err();
    assert_eq!(error.role(), Role::Lane(0));
    assert_eq!(error.stage(), Stage::Run);
    assert_eq!(error.panic_message(), Some("injected poll panic"));
    assert_eq!(current_cpus().unwrap(), before);
    assert_eq!(group.stats().done, 1);
    assert_eq!(events(&recipe), FULL);

    // A group can be run again after it finished.
    let recipe = Recipe {
        fault: Fault {
            step: Some("poll"),
            panic: false,
        },
        ..Recipe::default()
    };
    let error = group.run(&recipe, &Unscoped).unwrap_err();
    assert!(
        matches!(&error, GroupError::Failed { error, .. } if error.context() == Some("injected"))
    );
    assert_eq!(events(&recipe), FULL);
}

#[test]
fn every_non_fence_fault_still_drains_fences_and_drops() {
    for step in [
        "start",
        "stop_admission",
        "drain",
        "close",
        "shutdown",
        "poll",
    ] {
        for panic in [false, true] {
            let recipe = Recipe {
                fault: Fault {
                    step: Some(step),
                    panic,
                },
                ..Recipe::default()
            };
            let mut group = group(one_lane());
            let started = group.start(Arc::new(recipe.clone()), &Unscoped);
            // A steady-state fault may race with the startup wait.
            if step != "poll" {
                assert_eq!(started.is_err(), step == "start", "{step}");
            }
            let error = group.join().unwrap_err();
            let stage = match step {
                "start" => Stage::Start,
                "stop_admission" => Stage::StopAdmission,
                "drain" => Stage::Drain,
                "close" => Stage::Close,
                "shutdown" => Stage::Shutdown,
                _ => Stage::Run,
            };
            assert_eq!(
                (error.role(), error.stage()),
                (Role::Lane(0), stage),
                "{step}"
            );
            if panic {
                let message = format!("injected {step} panic");
                assert_eq!(error.panic_message(), Some(message.as_str()));
            } else {
                assert_eq!(error.error(), Some(&injected()), "{step}");
            }
            assert!(!error.is_fatal());
            assert_eq!(events(&recipe), FULL, "{step} panic={panic}");
            assert_eq!(group.stats().fatal, 0);
        }
    }
}

#[test]
fn unconfirmed_fences_leak_the_service_and_report() {
    for (step, kept) in [("fence1", 5), ("fence2", 7)] {
        for panic in [false, true] {
            let recipe = Recipe {
                fault: Fault {
                    step: Some(step),
                    panic,
                },
                ..Recipe::default()
            };
            let mut group = group(one_lane());
            group.start(Arc::new(recipe.clone()), &Unscoped).unwrap();
            let error = group.join().unwrap_err();
            let GroupError::Fatal { event, error, .. } = error else {
                panic!("expected a fatal error, got {error:?}");
            };
            assert_eq!((event.role, event.stage), (Role::Lane(0), Stage::Fence));
            if panic {
                let message = format!("injected {step} panic");
                assert_eq!(event.cause, FatalCause::FencePanicked(message));
                assert_eq!(error, None);
            } else {
                assert_eq!(event.cause, FatalCause::FenceFailed);
                assert_eq!(error, Some(injected()));
            }
            // The service is never dropped and nothing runs after the failed fence.
            assert_eq!(events(&recipe), &FULL[..kept], "{step} panic={panic}");
            assert_eq!(group.stats().fatal, 1);
        }
    }
}

#[test]
fn fatal_errors_replace_earlier_ordinary_errors() {
    struct Both(Events);

    impl Service<Unscoped> for Both {
        fn poll(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Error> {
            Err(Error::new(ErrorKind::Canceled))
        }

        fn fence<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
            self.0.lock().unwrap().push("fence");
            Box::pin(async { Err(injected()) })
        }
    }

    struct Make(Events);

    impl Factory<Unscoped> for Make {
        fn build_lane(&self, _: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            Ok(Box::new(Both(self.0.clone())))
        }
    }

    let events = Events::default();
    let error = group(one_lane())
        .run(&Make(events.clone()), &Unscoped)
        .unwrap_err();
    assert!(error.is_fatal());
    assert_eq!(error.error(), Some(&injected()));
    assert_eq!(*events.lock().unwrap(), ["fence"]);
}

#[test]
fn custom_fatal_policy_runs_the_hook_then_leaks() {
    let seen = Arc::new(Mutex::new(Vec::<FatalEvent>::new()));
    let hook_seen = seen.clone();
    let config = GroupConfig::default().with_fatal(FatalPolicy::Custom(Arc::new(move |event| {
        hook_seen.lock().unwrap().push(event.clone());
    })));
    let recipe = Recipe {
        fault: Fault {
            step: Some("fence1"),
            panic: false,
        },
        ..Recipe::default()
    };
    let mut group = Group::new(one_lane(), config).unwrap();
    group.start(Arc::new(recipe.clone()), &Unscoped).unwrap();
    assert!(group.join().unwrap_err().is_fatal());
    let seen = seen.lock().unwrap();
    assert_eq!(seen.len(), 1);
    assert_eq!(seen[0].role, Role::Lane(0));
    assert_eq!(seen[0].cause, FatalCause::FenceFailed);
    assert!(!events(&recipe).contains(&"drop"));
}

#[test]
fn failure_reporter_stops_the_group() {
    let recipe = Recipe {
        report: true,
        ..Recipe::default()
    };
    let error = group(one_lane()).run(&recipe, &Unscoped).unwrap_err();
    assert_eq!(
        (error.role(), error.stage()),
        (Role::Lane(0), Stage::Report)
    );
    assert_eq!(error.error(), Some(&injected()));
    assert_eq!(events(&recipe), FULL);
}

#[test]
fn preparation_rejects_unusable_plans() {
    let outside = Plan::builder()
        .lane("test-lane", CpuSet::MAX_CPU)
        .build()
        .unwrap();
    let recipe = Recipe::default();
    let error = group(outside)
        .start(Arc::new(recipe.clone()), &Unscoped)
        .unwrap_err();
    assert_eq!((error.role(), error.stage()), (Role::Group, Stage::Prepare));
    assert_eq!(
        error.error().unwrap().kind(),
        ErrorKind::InvalidConfiguration
    );

    // `start` counts its caller as a thread; `run` does not.
    let tight = Plan::builder()
        .lane("test-lane", None)
        .max_threads(1)
        .build()
        .unwrap();
    let mut group = group(tight);
    let error = group
        .start(Arc::new(recipe.clone()), &Unscoped)
        .unwrap_err();
    assert_eq!(
        error.error().unwrap().context(),
        Some("plan exceeds max_threads")
    );
    assert!(events(&recipe).is_empty());
    let recipe = Recipe {
        report: true,
        ..Recipe::default()
    };
    assert_eq!(
        group.run(&recipe, &Unscoped).unwrap_err().stage(),
        Stage::Report
    );

    let error =
        Group::<Unscoped>::new(one_lane(), GroupConfig::default().with_work_budget(0)).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);
}

#[test]
fn panicking_scope_clones_abandon_every_unstarted_lane() {
    struct CloneScope {
        clones: Arc<AtomicUsize>,
        fail: usize,
    }

    impl Clone for CloneScope {
        fn clone(&self) -> Self {
            let count = self.clones.fetch_add(1, Ordering::SeqCst);
            assert_ne!(count, self.fail, "clone failure");
            Self {
                clones: self.clones.clone(),
                fail: self.fail,
            }
        }
    }

    impl Scope for CloneScope {
        type Error = Error;

        fn check(&self) -> Result<(), Error> {
            Ok(())
        }
    }

    struct NeverBuilt(AtomicUsize);

    impl Factory<CloneScope> for NeverBuilt {
        fn build_lane(&self, _: usize) -> Result<Box<dyn Service<CloneScope>>, Error> {
            panic!("must not build")
        }

        fn abandon_lane(&self, _: usize) -> Result<(), Error> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }
    }

    for fail in [0, 1] {
        let factory = Arc::new(NeverBuilt(AtomicUsize::new(0)));
        let mut group = group(one_lane());
        let scope = CloneScope {
            clones: Arc::default(),
            fail,
        };
        let error = group.start(factory.clone(), &scope).unwrap_err();
        assert_eq!((error.role(), error.stage()), (Role::Group, Stage::Prepare));
        assert!(error.panic_message().unwrap().contains("clone failure"));
        assert_eq!(group.join().unwrap_err(), error);
        let stats = group.stats();
        assert_eq!((stats.done, stats.drained), (stats.total, stats.total));
        assert_eq!(factory.0.load(Ordering::SeqCst), 1);
    }
}

#[test]
fn teardown_scope_panic_falls_back_to_the_startup_scope() {
    struct Panicking(Recipe);

    impl Factory<Unscoped> for Panicking {
        fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            self.0.build_lane(lane)
        }

        fn teardown_scope(&self, _: &Unscoped) -> Unscoped {
            panic!("teardown scope");
        }
    }

    let recipe = Recipe::default();
    let mut group = group(one_lane());
    group
        .start(Arc::new(Panicking(recipe.clone())), &Unscoped)
        .unwrap();
    let error = group.join().unwrap_err();
    assert_eq!(error.panic_message(), Some("teardown scope"));
    assert_eq!((error.role(), error.stage()), (Role::Lane(0), Stage::Drain));
    assert_eq!(events(&recipe), FULL);
}

/// A scope whose cancellation hook panics on the given call (0-based).
#[derive(Clone)]
struct HookScope {
    calls: Arc<AtomicUsize>,
    panic_on: usize,
}

impl Scope for HookScope {
    type Error = Error;

    fn check(&self) -> Result<(), Error> {
        Ok(())
    }

    fn cancellation(&self) -> Option<&CancelToken> {
        let call = self.calls.fetch_add(1, Ordering::SeqCst);
        assert_ne!(call, self.panic_on, "cancellation hook");
        None
    }
}

/// A resource-free service that counts its fences and drops.
struct Counted {
    fences: Arc<AtomicUsize>,
    drops: Arc<AtomicUsize>,
    poll_panics: bool,
}

impl<S: Scope<Error = Error>> Service<S> for Counted {
    fn poll(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Error> {
        assert!(!self.poll_panics, "steady state must not run");
        Ok(())
    }

    fn fence<'a>(&'a mut self, _: &'a S) -> LifecycleFuture<'a, Error> {
        self.fences.fetch_add(1, Ordering::SeqCst);
        Box::pin(async { Ok(()) })
    }
}

impl Drop for Counted {
    fn drop(&mut self) {
        self.drops.fetch_add(1, Ordering::SeqCst);
    }
}

#[derive(Default)]
struct CountedFactory {
    fences: Arc<AtomicUsize>,
    drops: Arc<AtomicUsize>,
    poll_panics: bool,
}

impl CountedFactory {
    fn make<S: Scope<Error = Error>>(&self) -> Box<dyn Service<S>> {
        Box::new(Counted {
            fences: self.fences.clone(),
            drops: self.drops.clone(),
            poll_panics: self.poll_panics,
        })
    }
}

impl<S: Scope<Error = Error>> Factory<S> for CountedFactory {
    fn build_lane(&self, _: usize) -> Result<Box<dyn Service<S>>, Error> {
        Ok(self.make())
    }

    fn build_helper(&self, _: usize) -> Result<Box<dyn Service<S>>, Error> {
        Ok(self.make())
    }
}

#[test]
fn cancellation_hook_panics_still_tear_down_lanes_and_helpers() {
    for helper in [false, true] {
        let mut plan = Plan::builder().lane("test-lane", first_cpu());
        if helper {
            plan = plan.helper("test-helper", first_cpu(), [0]);
        }
        let factory = CountedFactory::default();
        let scope = HookScope {
            calls: Arc::default(),
            panic_on: 0,
        };
        let mut group = group(plan.build().unwrap());
        let error = group.run_scoped(&factory, &scope).unwrap_err();
        assert!(error.panic_message().unwrap().contains("cancellation hook"));
        assert_eq!(group.stats().done, group.stats().total);
        // Whatever was built was fenced twice and dropped.
        let built = factory.drops.load(Ordering::SeqCst);
        assert_eq!(factory.fences.load(Ordering::SeqCst), 2 * built);
    }
}

#[test]
fn steady_state_hook_panic_stops_before_polling_and_fences_twice() {
    let factory = CountedFactory {
        poll_panics: true,
        ..CountedFactory::default()
    };
    let scope = HookScope {
        calls: Arc::default(),
        panic_on: 1,
    };
    let mut group = group(one_lane());
    let error = group.run_scoped(&factory, &scope).unwrap_err();
    assert_eq!((error.role(), error.stage()), (Role::Lane(0), Stage::Run));
    assert_eq!(scope.calls.load(Ordering::SeqCst), 2);
    assert_eq!(factory.fences.load(Ordering::SeqCst), 2);
    assert_eq!(factory.drops.load(Ordering::SeqCst), 1);
}

#[test]
fn run_scoped_stops_when_the_scope_is_canceled() {
    let token = CancelToken::new();
    let canceler = {
        let token = token.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(20));
            token.cancel();
        })
    };
    let factory = CountedFactory::default();
    let error = group(one_lane()).run_scoped(&factory, &token).unwrap_err();
    canceler.join().unwrap();
    assert_eq!(error.stage(), Stage::Run);
    assert!(error.error().unwrap().is_canceled());
    assert_eq!(factory.fences.load(Ordering::SeqCst), 2);
}

#[test]
fn panicking_wait_scope_is_an_error() {
    #[derive(Clone)]
    struct Flagged(Arc<AtomicBool>);

    impl Scope for Flagged {
        type Error = Error;

        fn check(&self) -> Result<(), Error> {
            assert!(!self.0.load(Ordering::SeqCst), "scope check");
            Ok(())
        }
    }

    let flag = Arc::new(AtomicBool::new(false));
    let factory = Arc::new(CountedFactory::default());
    let mut group = group(one_lane());
    group
        .start(factory.clone(), &Flagged(flag.clone()))
        .unwrap();
    flag.store(true, Ordering::SeqCst);
    let error = group.drain(&Flagged(flag.clone())).unwrap_err();
    assert_eq!(error.panic_message(), Some("scope check"));
    flag.store(false, Ordering::SeqCst);
    // The failed wait did not stop teardown. Lanes may or may not have seen
    // the panicking scope as a teardown diagnosis, so only completion is checked.
    let _ = group.join();
    assert_eq!(group.stats().done, 1);
    assert_eq!(factory.fences.load(Ordering::SeqCst), 2);
}

#[test]
fn helper_start_and_drain_panics_keep_polling_until_the_lane_fence() {
    struct Shard {
        polls: Arc<AtomicUsize>,
        dropped: Arc<AtomicBool>,
    }

    impl Service<Unscoped> for Shard {
        fn start<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
            Box::pin(async { panic!("injected helper start panic") })
        }

        fn poll(&mut self, _: &mut Context<'_>, budget: usize) -> Result<(), Error> {
            assert_eq!(budget, 1);
            self.polls.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }

        fn drain<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
            Box::pin(async { panic!("injected helper drain panic") })
        }
    }

    impl Drop for Shard {
        fn drop(&mut self) {
            self.dropped.store(true, Ordering::SeqCst);
        }
    }

    #[derive(Default)]
    struct Shards {
        polls: Arc<AtomicUsize>,
        dropped: Arc<AtomicBool>,
        lanes: Recipe,
    }

    impl Factory<Unscoped> for Shards {
        fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            self.lanes.build_lane(lane)
        }

        fn build_helper(&self, _: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            Ok(Box::new(Shard {
                polls: self.polls.clone(),
                dropped: self.dropped.clone(),
            }))
        }

        fn abandon_lane(&self, lane: usize) -> Result<(), Error> {
            self.lanes.abandon_lane(lane)
        }
    }

    let plan = Plan::builder()
        .lane("test-lane", first_cpu())
        .helper("test-helper", first_cpu(), [0])
        .build()
        .unwrap();
    let factory = Arc::new(Shards::default());
    let mut group = group(plan);
    let error = group.start(factory.clone(), &Unscoped).unwrap_err();
    assert_eq!(
        (error.role(), error.stage()),
        (
            Role::Helper {
                helper: 0,
                lane: Some(0)
            },
            Stage::Start
        )
    );
    assert_eq!(error.panic_message(), Some("injected helper start panic"));
    // The lane never started: it was abandoned, which released the shard.
    assert!(events(&factory.lanes).is_empty());
    assert_eq!(factory.lanes.abandoned.load(Ordering::SeqCst), 1);
    assert!(factory.polls.load(Ordering::SeqCst) >= 2);
    assert!(factory.dropped.load(Ordering::SeqCst));
    assert_eq!(group.stats().done, 2);
}

#[test]
fn helper_fences_progress_cooperatively_before_drain_and_drop() {
    struct Shared {
        entered: [AtomicUsize; 2],
        completed: [AtomicUsize; 2],
        dropped: AtomicUsize,
    }

    struct Independent {
        shared: Arc<Shared>,
        owner: ThreadId,
        fences: usize,
    }

    impl Service<Unscoped> for Independent {
        fn poll(&mut self, _: &mut Context<'_>, budget: usize) -> Result<(), Error> {
            assert_eq!(budget, 1);
            Ok(())
        }

        fn fence<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
            let phase = self.fences;
            self.fences += 1;
            self.shared.entered[phase].fetch_add(1, Ordering::SeqCst);
            // Each shard's fence needs its sibling's fence to start: sequential
            // fencing would deadlock.
            Box::pin(std::future::poll_fn(move |_| {
                if self.shared.entered[phase].load(Ordering::SeqCst) != 2 {
                    return Poll::Pending;
                }
                self.shared.completed[phase].fetch_add(1, Ordering::SeqCst);
                Poll::Ready(Ok(()))
            }))
        }

        fn shutdown<'a>(&'a mut self, _: &'a Unscoped) -> LifecycleFuture<'a, Error> {
            Box::pin(async move {
                assert_eq!(self.shared.completed[0].load(Ordering::SeqCst), 2);
                // A shutdown failure still requires the second fence.
                Err(injected())
            })
        }
    }

    impl Drop for Independent {
        fn drop(&mut self) {
            assert_eq!(self.owner, thread::current().id());
            assert_eq!(self.fences, 2);
            assert_eq!(self.shared.completed[1].load(Ordering::SeqCst), 2);
            self.shared.dropped.fetch_add(1, Ordering::SeqCst);
        }
    }

    struct Helpers(Arc<Shared>);

    impl Factory<Unscoped> for Helpers {
        fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            CountedFactory::default().build_lane(lane)
        }

        fn build_helper(&self, _: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            Ok(Box::new(Independent {
                shared: self.0.clone(),
                owner: thread::current().id(),
                fences: 0,
            }))
        }
    }

    let shared = Arc::new(Shared {
        entered: Default::default(),
        completed: Default::default(),
        dropped: AtomicUsize::new(0),
    });
    let cpu = first_cpu();
    let plan = Plan::builder()
        .lane("test-lane-0", cpu)
        .lane("test-lane-1", cpu)
        .helper("test-helpers", cpu, [0, 1])
        .max_threads(4)
        .build()
        .unwrap();
    let mut group = group(plan);
    group
        .start(Arc::new(Helpers(shared.clone())), &Unscoped)
        .unwrap();
    assert_eq!(group.stats().ready, 3);
    group.drain(&Unscoped).unwrap();
    assert_eq!(shared.completed[0].load(Ordering::SeqCst), 2);
    let error = group.join().unwrap_err();
    assert_eq!(error.stage(), Stage::Shutdown);
    assert!(matches!(error.role(), Role::Helper { helper: 0, .. }));
    assert_eq!(shared.completed[1].load(Ordering::SeqCst), 2);
    assert_eq!(shared.dropped.load(Ordering::SeqCst), 2);
}

#[test]
fn failed_lane_fence_leaks_its_helper_shard() {
    struct Shard(Arc<AtomicUsize>);

    impl Service<Unscoped> for Shard {
        fn poll(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Error> {
            Ok(())
        }
    }

    impl Drop for Shard {
        fn drop(&mut self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    struct Make {
        lanes: Recipe,
        shard_drops: Arc<AtomicUsize>,
    }

    impl Factory<Unscoped> for Make {
        fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            self.lanes.build_lane(lane)
        }

        fn build_helper(&self, _: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
            Ok(Box::new(Shard(self.shard_drops.clone())))
        }
    }

    let factory = Arc::new(Make {
        lanes: Recipe {
            fault: Fault {
                step: Some("fence1"),
                panic: false,
            },
            ..Recipe::default()
        },
        shard_drops: Arc::default(),
    });
    let plan = Plan::builder()
        .lane("test-lane", None)
        .helper("test-helper", None, [0])
        .build()
        .unwrap();
    let mut group = group(plan);
    group.start(factory.clone(), &Unscoped).unwrap();
    let error = group.join().unwrap_err();
    assert!(error.is_fatal());
    assert_eq!(error.role(), Role::Lane(0));
    assert_eq!(group.stats().fatal, 1);
    assert_eq!(factory.shard_drops.load(Ordering::SeqCst), 0);
    assert_eq!(group.stats().done, 2);
}

#[test]
fn thread_waker_unparks_its_creator_and_wakes_the_extra_hook() {
    let woken = Arc::new(AtomicUsize::new(0));
    let hook = woken.clone();
    let (send, receive) = std::sync::mpsc::channel();
    let target = thread::spawn(move || {
        let extra = RemoteWaker::new(move || {
            hook.fetch_add(1, Ordering::SeqCst);
        });
        send.send(thread_waker(Some(extra))).unwrap();
        thread::park_timeout(Duration::from_secs(10));
        send.send(thread_waker(None)).unwrap();
    });
    let waker = receive.recv_timeout(Duration::from_secs(5)).unwrap();
    waker.wake_by_ref();
    // The second message proves the target resumed before its 10 s timeout.
    receive.recv_timeout(Duration::from_secs(5)).unwrap();
    target.join().unwrap();
    assert_eq!(woken.load(Ordering::SeqCst), 1);
}

#[cfg(unix)]
mod abort {
    //! The `Abort` policy, checked in a child process re-running this test binary.

    use super::*;
    use std::os::unix::process::ExitStatusExt;
    use std::process::{Command, ExitStatus, Stdio};
    use std::time::Instant;

    const CHILD: &str = "URING_RUNTIME_GROUP_ABORT_CHILD";

    fn run_child(test: &str, mode: &str) -> ExitStatus {
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", test, "--nocapture", "--test-threads=1"])
            .env(CHILD, mode)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(60);
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                return status;
            }
            if Instant::now() > deadline {
                let _ = child.kill();
                let _ = child.wait();
                panic!("{mode}: child timed out");
            }
            thread::sleep(Duration::from_millis(10));
        }
    }

    fn child(mode: &str) -> ! {
        let limit = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        // SAFETY: setrlimit reads a valid, initialized rlimit and prctl takes
        // no pointers. Disabling core dumps only affects this aborting child.
        unsafe {
            libc::setrlimit(libc::RLIMIT_CORE, &limit);
            libc::prctl(libc::PR_SET_DUMPABLE, 0);
        }
        let step = match mode {
            "fence1-error" | "fence1-panic" => "fence1",
            "fence2-error" => "fence2",
            _ => "abandon",
        };
        let recipe = Recipe {
            fault: Fault {
                step: Some(step),
                panic: mode.ends_with("panic"),
            },
            ..Recipe::default()
        };
        let config = GroupConfig::default().with_fatal(FatalPolicy::Abort);
        let mut group = Group::new(one_lane(), config).unwrap();
        // The report ends steady state; "abandon" fails before building.
        let recipe = Recipe {
            report: true,
            ..recipe
        };
        let _ = group.run(&recipe, &Unscoped);
        // Reaching this point means the policy did not abort.
        std::process::exit(98);
    }

    #[test]
    fn abort_policy_aborts_the_process() {
        if let Ok(mode) = std::env::var(CHILD) {
            child(&mode);
        }
        for mode in [
            "fence1-error",
            "fence1-panic",
            "fence2-error",
            "abandon-error",
        ] {
            let status = run_child("abort::abort_policy_aborts_the_process", mode);
            assert_eq!(status.signal(), Some(libc::SIGABRT), "{mode}: {status}");
        }
    }
}
