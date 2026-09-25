//! Stable local page-to-worker dispatch, independent of cluster placement.
//!
//! Construct worker-local service graphs on their selected threads. Rc-owned graphs
//! must never cross threads; only bounded commands and completion-safe leases do.
//! Drain flights before changing the worker map; no live remapping is implied.

use super::{admission::Admission, affinity::AffinityPlan, reactor::Reactor};
use crate::{
    error::{Result, pending},
    model::identity::{PageId, WorkerId},
};
use std::rc::Rc;

pub struct WorkerRuntime {
    pub reactor: Rc<Reactor>,
    pub admission: Rc<Admission>,
}
pub struct WorkerMap {
    workers: Vec<WorkerId>,
}
pub struct WorkerGroup {
    plan: AffinityPlan,
}

/// Application implements this factory without runtime importing read policy.
pub trait WorkerFactory {
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>>;
}
pub trait WorkerService {
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()>;
    fn stop_admission(&mut self) -> Result<()>;
}

impl WorkerMap {
    pub fn owner(&self, _page: &PageId) -> Result<WorkerId> {
        pending("worker.owner")
    }
}
impl WorkerGroup {
    pub fn new(plan: AffinityPlan) -> Self {
        Self { plan }
    }
    pub fn run(&mut self, _factory: &dyn WorkerFactory) -> Result<()> {
        pending("worker.run")
    }
}

#[cfg(test)]
mod tests {
    // Cover stable shard assignment, bounded crypto quanta, and one-core fairness.
}
