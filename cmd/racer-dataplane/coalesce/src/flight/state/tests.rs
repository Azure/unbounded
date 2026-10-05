use super::*;
use std::cell::Cell;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::Wake;
use std::time::Duration;

struct Policy {
    due: Instant,
    error: Rc<Cell<Option<u8>>>,
}
impl WaiterPolicy for Policy {
    type Error = u8;
    fn check(&self) -> Option<u8> {
        self.error.get()
    }
    fn deadline(&self) -> Instant {
        self.due
    }
}
type Core = State<u32, u32, Published<u32, u32>, Policy>;
fn split(value: Published<u32, u32>) -> Published<u32, u32> {
    value
}
fn register(core: &mut Core, id: u64, acquisition: bool, complete: bool) -> Rc<Cell<Option<u8>>> {
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
fn identity() -> Identity {
    Identity {
        owner: Rc::new(()),
        incarnation: 1,
        generation: 0,
    }
}
fn refresh(core: &mut Core, idle: bool, wakes: &mut Vec<Waker>) {
    core.refresh(idle, 9, 8, Instant::now, split, wakes);
}

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

#[derive(Default)]
struct Count(AtomicUsize);
impl Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

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
        let slot = &mut core.waiters.get_mut(&id).unwrap().waker;
        store_waker(slot, &Waker::from(old.clone()));
        store_waker(slot, &Waker::from(latest.clone()));
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
