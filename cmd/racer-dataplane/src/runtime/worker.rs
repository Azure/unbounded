//! Stable local page-to-worker dispatch, independent of cluster placement.
//!
//! Construct worker-local service graphs on their selected threads. Rc-owned graphs
//! must never cross threads; only bounded commands and completion-safe leases do.
//! Drain flights before changing the worker map; no live remapping is implied.
//!
//! Each worker is an I/O/crypto thread pair. The I/O thread owns the service graph,
//! flights, storage shard, and admission. Page AEAD runs on the paired crypto thread
//! through bounded owned job/completion messages, retaining buffers and key leases
//! until completion even after cancellation. Queue wakeups and completion capacity
//! must permit progress when both threads share one CPU. The crypto handoff types
//! and paired service lifecycle remain to be specified before implementation.

use super::{admission::Admission, affinity::AffinityPlan, reactor::Reactor};
use crate::{
    error::{Result, pending},
    model::identity::{ObjectId, PageId, WorkerId},
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
    /// Same owner as this object's page zero, without needing an ETag first.
    pub fn metadata_owner(&self, _object: &ObjectId) -> Result<WorkerId> {
        pending("worker.metadata_owner")
    }
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
