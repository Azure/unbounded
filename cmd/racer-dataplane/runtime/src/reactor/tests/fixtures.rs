//! Minimal counting guards and scope for raw completion ownership tests.
use super::*;
use std::ops::Deref;

#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum ResourceClass {
    Connection,
    RequestContext,
}
pub struct Limits {
    pub queue_entries: NonZeroUsize,
}
pub struct Admission {
    pub limits: Limits,
    used: Rc<RefCell<BTreeMap<ResourceClass, usize>>>,
}
pub struct Reservation {
    used: Rc<RefCell<BTreeMap<ResourceClass, usize>>>,
    class: ResourceClass,
    amount: usize,
}
impl Drop for Reservation {
    fn drop(&mut self) {
        *self.used.borrow_mut().entry(self.class).or_default() -= self.amount;
    }
}
impl Admission {
    pub fn new(limits: Limits) -> Self {
        Self {
            limits,
            used: Rc::default(),
        }
    }
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
    pub fn used(&self, class: ResourceClass) -> usize {
        *self.used.borrow().get(&class).unwrap_or(&0)
    }
}
pub struct CountingBudget(Rc<Admission>);
impl Budget for CountingBudget {
    type Charge = Reservation;
    fn charge(&self, bytes: usize) -> Result<Reservation> {
        self.0.reserve(None, ResourceClass::RequestContext, bytes)
    }
}
pub struct Reactor {
    core: super::super::Reactor<RequestScope, CountingBudget>,
    pub admission: Rc<Admission>,
}
impl Reactor {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self {
            core: super::super::Reactor::new(
                admission.limits.queue_entries.get(),
                CountingBudget(admission.clone()),
            ),
            admission,
        }
    }
    pub fn file_fence(&self, _: ()) -> Operation<'_, ()> {
        self.core.fence_matching(|_| true)
    }
}
impl Deref for Reactor {
    type Target = super::super::Reactor<RequestScope, CountingBudget>;
    fn deref(&self) -> &Self::Target {
        &self.core
    }
}
#[derive(Clone)]
pub struct RequestScope {
    pub deadline: Deadline,
    pub cancellation: Cancellation,
    pub request: (),
}
impl RequestScope {
    pub fn new(_: (), deadline: Instant) -> Result<Self> {
        Ok(Self {
            deadline: Deadline(deadline),
            cancellation: Cancellation::new()?,
            request: (),
        })
    }
    pub fn cancel(&self) -> Result<()> {
        self.cancellation.cancel()
    }
}
impl Scope for RequestScope {
    type Error = Error;
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
