// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local endpoint/connection health. Owned permits allow healthy requests
//! to overlap without borrowing the breaker across asynchronous operations.
use std::{
    cell::RefCell,
    rc::Rc,
    time::{Duration, Instant},
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Rejected;
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Status {
    Closed,
    Open,
    HalfOpen,
}
enum Phase {
    Closed,
    Open(Option<Instant>),
    Probe,
}
struct State {
    phase: Phase,
    active: usize,
    generation: Rc<()>,
    cooldown: Duration,
}
struct Inner {
    state: RefCell<State>,
    clock: Box<dyn Fn() -> Instant>,
}

/// Clones share one failure scope. Separate origins and RDMA connections get
/// separate breakers; logical shards using one transport share its breaker.
#[derive(Clone)]
pub struct CircuitBreaker {
    inner: Rc<Inner>,
}
impl CircuitBreaker {
    /// Observing a cooldown never starts a probe or changes admission.
    pub(crate) fn status(&self) -> Status {
        match self.inner.state.borrow().phase {
            Phase::Closed => Status::Closed,
            Phase::Open(_) => Status::Open,
            Phase::Probe => Status::HalfOpen,
        }
    }
    pub(crate) fn active(&self) -> usize {
        self.inner.state.borrow().active
    }
    pub(crate) fn available(&self) -> bool {
        matches!(self.inner.state.borrow().phase, Phase::Closed)
            || matches!(self.inner.state.borrow().phase, Phase::Open(Some(at)) if (self.inner.clock)() >= at)
    }
    pub(crate) fn evictable(&self) -> bool {
        Rc::strong_count(&self.inner) == 1 && self.available()
    }
    pub fn new(cooldown: Duration) -> Self {
        Self::with_clock(cooldown, crate::environment::now)
    }
    pub fn with_clock(cooldown: Duration, clock: impl Fn() -> Instant + 'static) -> Self {
        Self {
            inner: Rc::new(Inner {
                state: RefCell::new(State {
                    phase: Phase::Closed,
                    active: 0,
                    generation: Rc::new(()),
                    cooldown,
                }),
                clock: Box::new(clock),
            }),
        }
    }
    pub fn try_acquire(&self) -> Result<Permit, Rejected> {
        let now = (self.inner.clock)();
        let mut state = self.inner.state.borrow_mut();
        let probe = match state.phase {
            Phase::Closed => false,
            Phase::Open(Some(at)) if now >= at => {
                state.phase = Phase::Probe;
                true
            }
            _ => return Err(Rejected),
        };
        state.active += 1;
        Ok(Permit {
            inner: self.inner.clone(),
            generation: state.generation.clone(),
            probe,
            completed: false,
        })
    }
}

/// Non-clone completion authority tied to one breaker generation. Old completions
/// cannot reset a newer failure. Dropping a probe returns it to cooldown; dropping
/// an ordinary healthy request leaves health unchanged.
/// ```compile_fail
/// use racer_dataplane::breaker::Permit;
/// fn twice(p: Permit) { p.success(); p.failure(); }
/// ```
/// ```compile_fail
/// use racer_dataplane::breaker::Permit;
/// fn duplicate(p: Permit) { let _ = p.clone(); }
/// ```
#[must_use = "complete the request with success/failure, or drop to cancel"]
pub struct Permit {
    inner: Rc<Inner>,
    generation: Rc<()>,
    probe: bool,
    completed: bool,
}
impl Permit {
    pub(crate) fn current(&self) -> bool {
        Rc::ptr_eq(&self.inner.state.borrow().generation, &self.generation)
    }
    pub fn success(mut self) {
        self.finish(true);
    }
    pub fn failure(mut self) {
        self.finish(false);
    }
    fn finish(&mut self, success: bool) {
        self.completed = true;
        let mut state = self.inner.state.borrow_mut();
        if !Rc::ptr_eq(&state.generation, &self.generation) {
            return;
        }
        if !success {
            state.phase = Phase::Open((self.inner.clock)().checked_add(state.cooldown));
            state.generation = Rc::new(());
        } else if self.probe {
            state.phase = Phase::Closed;
            state.generation = Rc::new(());
        }
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        if !self.completed && self.probe {
            self.finish(false);
        }
        self.inner.state.borrow_mut().active -= 1;
    }
}
