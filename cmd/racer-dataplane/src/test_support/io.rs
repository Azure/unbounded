//! Partial operations, delayed CQEs, disconnects, and resource-accounting fixtures.
use super::clock::{Clock, Schedule};
use crate::{
    error::{Error, Result},
    runtime::reactor::IoId,
};
use std::{
    cell::RefCell,
    collections::{HashMap, HashSet},
    time::Duration,
};

#[derive(Clone, Copy, Debug)]
pub enum IoEvent {
    Complete(IoId, usize),
    Fail(IoId, Error),
    CancelComplete(IoId),
}

struct Submission<L> {
    resources: L,
    length: usize,
    result: Option<Result<usize>>,
    cancel_requested: bool,
    cancel_complete: bool,
    abandoned: bool,
}

impl<L> Submission<L> {
    fn fenced(&self) -> bool {
        self.result.is_some() && (!self.cancel_requested || self.cancel_complete)
    }
}

/// Completion ownership model for tests, never a Reactor replacement. `L` can hold
/// real buffers/reservations or drop probes. Short CQEs finish one submission; the
/// caller must explicitly submit its remainder under a new ID, as with real I/O.
pub struct FaultIo<L = ()> {
    submissions: RefCell<HashMap<IoId, Submission<L>>>,
    seen: RefCell<HashSet<IoId>>,
    events: RefCell<Schedule<IoEvent>>,
}

impl<L> Default for FaultIo<L> {
    fn default() -> Self {
        Self::new(Clock::default())
    }
}

impl<L> FaultIo<L> {
    pub fn new(clock: Clock) -> Self {
        Self {
            submissions: RefCell::new(HashMap::new()),
            seen: RefCell::new(HashSet::new()),
            events: RefCell::new(Schedule::new(clock)),
        }
    }

    /// Rejection returns ownership, and retired IDs cannot be reused by stale CQEs.
    pub fn submit(
        &self,
        id: IoId,
        length: usize,
        resources: L,
    ) -> std::result::Result<(), (Error, L)> {
        if !self.seen.borrow_mut().insert(id) {
            return Err((Error::InvalidRequest, resources));
        }
        self.submissions.borrow_mut().insert(
            id,
            Submission {
                resources,
                length,
                result: None,
                cancel_requested: false,
                cancel_complete: false,
                abandoned: false,
            },
        );
        Ok(())
    }

    pub fn complete(&self, id: IoId, bytes: usize) -> Result<()> {
        self.finish(id, Ok(bytes))
    }

    pub fn fail(&self, id: IoId, error: Error) -> Result<()> {
        self.finish(id, Err(error))
    }

    fn finish(&self, id: IoId, result: Result<usize>) -> Result<()> {
        let mut submissions = self.submissions.borrow_mut();
        let submission = submissions.get_mut(&id).ok_or(Error::InvalidRequest)?;
        if submission.result.is_some() {
            return Err(Error::InvalidRequest);
        }
        if result.is_ok_and(|bytes| bytes > submission.length) {
            return Err(Error::InvalidRange);
        }
        submission.result = Some(result);
        if submission.abandoned && submission.fenced() {
            // Release outside the RefCell borrow so resource destructors may inspect us.
            let retired = submissions.remove(&id);
            drop(submissions);
            drop(retired);
        }
        Ok(())
    }

    pub fn cancel(&self, id: IoId) -> Result<()> {
        let mut submissions = self.submissions.borrow_mut();
        let submission = submissions.get_mut(&id).ok_or(Error::InvalidRequest)?;
        if submission.cancel_requested || submission.result.is_some() {
            return Err(Error::InvalidRequest);
        }
        submission.cancel_requested = true;
        Ok(())
    }

    pub fn cancel_complete(&self, id: IoId) -> Result<()> {
        let mut submissions = self.submissions.borrow_mut();
        let submission = submissions.get_mut(&id).ok_or(Error::InvalidRequest)?;
        if !submission.cancel_requested || submission.cancel_complete {
            return Err(Error::InvalidRequest);
        }
        submission.cancel_complete = true;
        if submission.abandoned && submission.fenced() {
            let retired = submissions.remove(&id);
            drop(submissions);
            drop(retired);
        }
        Ok(())
    }

    /// Abandoning a waiter is not proof of either kernel fence.
    pub fn abandon(&self, id: IoId) -> Result<()> {
        let mut submissions = self.submissions.borrow_mut();
        let submission = submissions.get_mut(&id).ok_or(Error::InvalidRequest)?;
        submission.abandoned = true;
        if submission.fenced() {
            let retired = submissions.remove(&id);
            drop(submissions);
            drop(retired);
        }
        Ok(())
    }

    pub fn take(&self, id: IoId) -> Result<Option<(L, Result<usize>)>> {
        let mut submissions = self.submissions.borrow_mut();
        let submission = submissions.get(&id).ok_or(Error::InvalidRequest)?;
        if !submission.fenced() {
            return Ok(None);
        }
        let submission = submissions.remove(&id).unwrap();
        Ok(Some((submission.resources, submission.result.unwrap())))
    }

    pub fn retained(&self) -> usize {
        self.submissions.borrow().len()
    }

    pub fn schedule(&self, at: Duration, event: IoEvent) -> Result<()> {
        self.events.borrow_mut().push(at, event)
    }

    /// Explicit bounded delivery permits deterministic interleaving with other work.
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        let mut delivered = 0;
        while delivered < budget {
            let event = self.events.borrow_mut().pop_ready();
            let Some(event) = event else { break };
            match event {
                IoEvent::Complete(id, bytes) => self.complete(id, bytes)?,
                IoEvent::Fail(id, error) => self.fail(id, error)?,
                IoEvent::CancelComplete(id) => self.cancel_complete(id)?,
            }
            delivered += 1;
        }
        Ok(delivered)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{cell::Cell, rc::Rc};

    #[derive(Debug)]
    struct Probe(Rc<Cell<usize>>);
    impl Drop for Probe {
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }

    #[test]
    fn abandoned_resources_wait_for_both_fences_in_either_order() {
        for cancel_first in [false, true] {
            let io = FaultIo::default();
            let drops = Rc::new(Cell::new(0));
            io.submit(IoId(1), 8, Probe(drops.clone())).unwrap();
            io.cancel(IoId(1)).unwrap();
            io.abandon(IoId(1)).unwrap();
            if cancel_first {
                io.cancel_complete(IoId(1)).unwrap();
            } else {
                io.fail(IoId(1), Error::Cancelled).unwrap();
            }
            assert_eq!(drops.get(), 0);
            assert_eq!(io.retained(), 1);
            assert!(io.take(IoId(1)).unwrap().is_none());
            if cancel_first {
                io.fail(IoId(1), Error::Cancelled).unwrap();
            } else {
                io.cancel_complete(IoId(1)).unwrap();
            }
            assert_eq!(drops.get(), 1);
            assert_eq!(io.retained(), 0);
            assert_eq!(io.complete(IoId(1), 8), Err(Error::InvalidRequest));
        }
    }

    #[test]
    fn scheduled_short_io_disconnect_and_budget_preserve_resource_ownership() {
        let clock = Clock::default();
        let io = FaultIo::new(clock.clone());
        io.submit(IoId(1), 8, vec![1; 8]).unwrap();
        io.submit(IoId(2), 8, vec![2; 8]).unwrap();
        io.schedule(Duration::from_secs(2), IoEvent::Complete(IoId(1), 3))
            .unwrap();
        io.schedule(Duration::from_secs(1), IoEvent::Fail(IoId(2), Error::Io))
            .unwrap();
        assert_eq!(io.poll_budgeted(8), Ok(0));
        clock.advance(Duration::from_secs(2)).unwrap();
        assert_eq!(io.poll_budgeted(0), Ok(0));
        assert_eq!(io.poll_budgeted(1), Ok(1));
        assert!(io.take(IoId(1)).unwrap().is_none());
        let (buffer, result) = io.take(IoId(2)).unwrap().unwrap();
        assert_eq!(buffer, vec![2; 8]);
        assert_eq!(result, Err(Error::Io));
        assert_eq!(io.poll_budgeted(1), Ok(1));
        let (buffer, result) = io.take(IoId(1)).unwrap().unwrap();
        assert_eq!(result, Ok(3));
        io.submit(IoId(3), 5, buffer).unwrap();
        assert_eq!(io.complete(IoId(3), 6), Err(Error::InvalidRange));
        assert_eq!(io.cancel_complete(IoId(3)), Err(Error::InvalidRequest));
        io.complete(IoId(3), 0).unwrap();
        assert_eq!(io.take(IoId(3)).unwrap().unwrap().1, Ok(0));
        let rejected = io.submit(IoId(1), 8, vec![9; 8]).unwrap_err();
        assert_eq!(rejected, (Error::InvalidRequest, vec![9; 8]));
    }
}
