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
//! must permit progress when both threads share one CPU.

use super::{
    admission::Admission,
    affinity::AffinityPlan,
    crypto::{CryptoClient, CryptoPort},
    deadline::RequestScope,
    reactor::Reactor,
};
use crate::{
    error::{Operation, Result, pending},
    model::identity::{ObjectId, PageId, WorkerId},
};
use std::rc::Rc;

/// Constructed on I/O; never move the local graph to the crypto thread.
/// ```compile_fail
/// use racer_dataplane::runtime::worker::WorkerRuntime;
/// fn require_send<T: Send>() {}
/// require_send::<WorkerRuntime>();
/// ```
pub struct WorkerRuntime {
    pub reactor: Rc<Reactor>,
    pub admission: Rc<Admission>,
    pub crypto: Rc<CryptoClient>,
}
/// Send endpoint is moved before construction on the crypto thread. No I/O
/// reactor, admission authority, or worker-local Rc can be supplied to the engine.
pub struct CryptoRuntime {
    pub port: CryptoPort,
}
pub struct WorkerMap {
    workers: Vec<WorkerId>,
}
pub struct WorkerGroup<'a> {
    plan: AffinityPlan,
    /// Keep the shared construction recipe alive through startup rollback/join.
    factory: Option<&'a dyn WorkerFactory>,
}

/// Shared construction recipe only. Each build runs on its own already-pinned
/// thread; returned local services/futures need not be Send. The I/O build owns
/// control/diagnostics as budgeted work, never extra userspace threads.
///
/// Rc-backed factories cannot cross the startup boundary:
/// ```compile_fail
/// use std::rc::Rc;
/// use racer_dataplane::{error::Result, model::identity::WorkerId,
///     runtime::worker::{WorkerFactory, WorkerRuntime, WorkerService,
///                       CryptoRuntime, CryptoService}};
/// struct LocalFactory(Rc<()>);
/// impl WorkerFactory for LocalFactory {
///     fn build(&self, _: WorkerId, _: WorkerRuntime) -> Result<Box<dyn WorkerService>> { todo!() }
///     fn build_crypto(&self, _: WorkerId, _: CryptoRuntime) -> Result<Box<dyn CryptoService>> { todo!() }
/// }
/// ```
pub trait WorkerFactory: Sync {
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>>;
    fn build_crypto(
        &self,
        worker: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn CryptoService>>;
}
pub trait WorkerService {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    /// Reap crypto completions before new admission, including abandoned results.
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()>;
    fn stop_admission(&mut self) -> Result<()>;
    /// Drain reads/dirty writes while the engine remains live. Close submissions
    /// only after no I/O producer can submit; keep consuming until fully fenced.
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
}
pub trait CryptoService {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()>;
    /// After submission close, complete every accepted job while I/O reaps results.
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
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
impl<'a> WorkerGroup<'a> {
    pub fn new(plan: AffinityPlan) -> Self {
        Self {
            plan,
            factory: None,
        }
    }
    /// Start exactly two threads per pair, with bounded endpoints created before
    /// either service. Pin (worker, role), build locally, and start crypto before
    /// I/O admission. Readiness requires both roles; partial startup failure stops
    /// admission, closes handoffs, fences accepted work, and joins all started
    /// threads. Scoped thread ownership keeps the Sync factory alive through join.
    pub fn start(&mut self, _factory: &'a dyn WorkerFactory, _scope: &RequestScope) -> Result<()> {
        pending("worker.start")
    }
    /// Stop I/O admission first. Drive both roles during I/O drain, then close
    /// crypto submissions and reap all completions. Deadline expiry requests
    /// cancellation but does not release resources before completion fences.
    pub fn drain(&mut self, _scope: &RequestScope) -> Result<()> {
        pending("worker.drain")
    }
    /// After drain, shut down both services and fence kernel/NIC references.
    /// Never terminate crypto with an outstanding job or unconsumed completion.
    pub fn shutdown(&mut self, _scope: &RequestScope) -> Result<()> {
        pending("worker.shutdown")
    }
    /// Join both OS threads of every pair, including partially started pairs.
    /// Cannot succeed while a service can still access its retained resources.
    pub fn join(&mut self) -> Result<()> {
        pending("worker.join")
    }
    /// Ordered start, budgeted drive, drain, shutdown, and join (also on failure).
    pub fn run(&mut self, _factory: &'a dyn WorkerFactory) -> Result<()> {
        pending("worker.run")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn factory_and_crypto_runtime_have_cross_thread_bounds() {
        fn sync<T: Sync + ?Sized>() {}
        fn send<T: Send + 'static>() {}
        sync::<dyn WorkerFactory>();
        send::<CryptoRuntime>();
    }

    // Cover stable shard assignment, bounded crypto quanta, and one-core fairness.
}
