// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Generation leases shared by local ingress, peer dispatch, and admitted tasks.
use super::*;

pub(super) struct Generation {
    pub(super) volume: String,
    pub(super) handlers: Vec<Rc<RefCell<Handler>>>,
    pub(super) _config: Arc<Prepared>,
    pub(super) manager: Option<RefCell<Manager>>,
    pub(super) active: Cell<bool>,
    pub(super) drain: Cell<Option<Instant>>,
    pub(super) expired: Cell<bool>,
}
impl Generation {
    pub(super) fn retire(&self, now: Instant) {
        self.active.set(false);
        // Reusing a removed listener must not renew its old generations' leases.
        if self.drain.get().is_none() {
            self.drain.set(Some(now + DRAIN_TIMEOUT));
        }
        if let Some(manager) = &self.manager {
            for path in &mut manager.borrow_mut().outbound {
                path.client = None;
            }
        }
    }
    pub(super) fn expire(&self) {
        self.active.set(false);
        self.expired.set(true);
        if let Some(manager) = &self.manager {
            manager.borrow_mut().clear(&self.handlers);
        }
    }
    pub(super) fn poll(&self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        if self
            .drain
            .get()
            .is_some_and(|d| crate::environment::now() >= d)
        {
            self.expire();
        }
        if self.expired.get() {
            return Ok(uring::Work::default());
        }
        let mut work = uring::Work {
            runnable: false,
            deadline: self.drain.get(),
        };
        if let Some(manager) = &self.manager {
            work.merge(manager.borrow_mut().poll(self, ring, budget));
        }
        for handler in &self.handlers {
            let mut handler = handler.borrow_mut();
            work.merge(handler.poll_background(ring, budget)?);
            let negotiations = handler.take_negotiations();
            if self.active.get()
                && let Some(manager) = &self.manager
            {
                for (id, target) in negotiations {
                    manager.borrow_mut().trigger(&id, &target);
                    work.runnable = true;
                }
            }
        }
        Ok(work)
    }
}
