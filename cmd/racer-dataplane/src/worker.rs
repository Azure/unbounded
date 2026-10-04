//! Stable local page-to-worker dispatch, independent of cluster placement.
//!
//! Construct worker-local service graphs on their selected threads. Rc-owned graphs
//! must never cross threads; only bounded commands and completion-safe leases do.
//! Drain flights before changing the worker map; no live remapping is implied.
//!
//! Each worker is an I/O shard. The I/O thread owns the service graph,
//! flights, storage shard, and admission. Page AEAD runs on a shared crypto thread
//! through bounded owned job/completion messages, retaining buffers and key leases
//! until completion even after cancellation. Queue wakeups and completion capacity
//! must permit progress when both threads share one CPU.

use crate::admission::AdmissionPolicy;
use crate::config::Config;
use crate::config::Limits;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::RequestId;
use crate::model::WorkerId;
use crate::runtime::Cancellation;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use crate::security;
use crate::security::CryptoClient;
use crate::security::CryptoPort;
use crate::security::IoCryptoPort;
use racer_control_wire::RailMapping;
use std::collections::BTreeMap;
use std::collections::HashMap;
use std::collections::HashSet;
#[cfg(test)]
use std::num::NonZeroUsize;
#[cfg(test)]
use std::ops::Deref;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::task::Context;
use std::task::Poll;
use std::task::Wake;
use std::task::Waker;
use std::thread;
use std::time::Duration;
#[cfg(test)]
use std::time::Instant;
use uring_runtime::affinity::CpuLocation;
use uring_runtime::affinity::EffectiveTopology;
use uring_runtime::affinity::NicLocality;
#[cfg(test)]
use uring_runtime::affinity::current_cpus;
use uring_runtime::deadline::Deadline;
use uring_runtime::group::Factory;
use uring_runtime::group::FailureReporter;
use uring_runtime::group::Group;
use uring_runtime::group::Helper;
use uring_runtime::group::Lane;
use uring_runtime::group::Plan;
use uring_runtime::group::Service;
use uring_runtime::reactor::ReactorWake;

const WORK_BUDGET: usize = 64;
const IDLE_WAIT: Duration = Duration::from_millis(1);
/// One I/O shard and its crypto execution placement. Equal crypto CPU IDs identify
/// the same execution thread. NIC locality never overrides the end-to-end rail.
#[derive(Clone, Debug)]
pub struct WorkerPair {
    pub worker: WorkerId,
    pub io: CpuLocation,
    pub crypto: CpuLocation,
    pub nic: Option<NicLocality>,
}
/// NUMA-local roughly 2:1 I/O/crypto placement, bounded by CPU and thread budgets.
pub struct AffinityPlan {
    pub pairs: Vec<WorkerPair>,
    /// Includes the caller. Owned startup reserves an additional coordinator slot.
    pub max_threads: usize,
}
#[cfg(test)]
pub fn pair_count(max_threads: usize, topology: &EffectiveTopology) -> Result<usize> {
    pair_count_with_policy(max_threads, topology, false)
}
#[cfg(test)]
fn pair_count_with_policy(
    max_threads: usize,
    topology: &EffectiveTopology,
    allow_smt: bool,
) -> Result<usize> {
    Ok(
        AffinityPlan::place_with_policy(max_threads, topology.clone(), &[], allow_smt)?
            .pairs
            .len(),
    )
}
impl AffinityPlan {
    /// Honor allowed CPUs, quotas, and the total thread cap.
    pub fn discover(config: &Config) -> Result<Self> {
        Self::from_topology(
            config,
            EffectiveTopology::discover().map_err(Error::from)?,
            &[],
        )
    }
    /// Plan from discovered constraints and accepted rails. NIC/NUMA compatibility
    /// never changes membership or page-to-rail selection; unavailable hardware
    /// preserves HTTP fallback. No extra control/diagnostic threads are created.
    pub fn from_topology(
        config: &Config,
        topology: EffectiveTopology,
        rails: &[RailMapping],
    ) -> Result<Self> {
        Self::place_with_policy(config.max_threads, topology, rails, config.allow_smt)
    }
    #[cfg(test)]
    fn place(
        max_threads: usize,
        topology: EffectiveTopology,
        rails: &[RailMapping],
    ) -> Result<Self> {
        Self::place_with_policy(max_threads, topology, rails, false)
    }
    fn place_with_policy(
        max_threads: usize,
        topology: EffectiveTopology,
        rails: &[RailMapping],
        allow_smt: bool,
    ) -> Result<Self> {
        if max_threads < 2 || topology.cpus.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        // Fractional CPU capacity still permits one pair sharing allowed CPUs.
        let quota = topology.quota.map(|quota| {
            usize::try_from(quota.quota.get() / quota.period.get())
                .unwrap_or(usize::MAX)
                .max(1)
        });
        let mut cpus = topology.cpus;
        cpus.sort_by_key(|cpu| cpu.cpu);
        if cpus.windows(2).any(|pair| pair[0].cpu == pair[1].cpu) {
            return Err(Error::InvalidConfiguration);
        }
        if !allow_smt {
            let mut cores = HashSet::new();
            cpus.retain(|cpu| cores.insert((cpu.package, cpu.core)));
        }
        let mut nics = topology.nics;
        nics.sort_by(|a, b| a.device.cmp(&b.device));
        // NUMA compatibility is a hint, not proof of physical RDMA eligibility.
        nics.retain(|nic| {
            nic.numa_node.is_some()
                && (rails.is_empty() || rails.iter().any(|rail| rail.numa_node == nic.numa_node))
        });
        let capacity = quota.unwrap_or(cpus.len()).min(cpus.len());
        let mut remaining = max_threads;
        let mut available = capacity;
        let mut nodes = BTreeMap::<_, Vec<_>>::new();
        for cpu in cpus {
            nodes.entry(cpu.numa_node).or_default().push(cpu);
        }
        let mut nodes = nodes.into_iter().collect::<Vec<_>>();
        nodes.sort_by_key(|(node, _)| (!nics.iter().any(|nic| nic.numa_node == *node), *node));
        let mut pairs = Vec::new();
        for (node, local) in nodes {
            if remaining < 2 || available == 0 {
                break;
            }
            // Prefer physical-core diversity among reactors even with SMT.
            let mut cores = HashSet::new();
            let mut ordered = local
                .into_iter()
                .map(|cpu| (!cores.insert((cpu.package, cpu.core)), cpu))
                .collect::<Vec<_>>();
            ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
            let mut local = ordered.into_iter().map(|(_, cpu)| cpu).collect::<Vec<_>>();
            let count = local.len().min(remaining).min(available);
            local.truncate(count);
            // Nearest integral 2:1 split, spending both remainder cores for n%3=2.
            let crypto_count = ((count + 1) / 3).max(1);
            let io_count = count.saturating_sub(crypto_count).max(1);
            let crypto = if count == 1 {
                &local[..]
            } else {
                &local[io_count..]
            };
            let nic = nics.iter().find(|nic| nic.numa_node == node).cloned();
            for (index, io) in local[..io_count].iter().enumerate() {
                if pairs.len() > usize::from(u16::MAX) {
                    break;
                }
                pairs.push(WorkerPair {
                    worker: WorkerId(pairs.len() as u16),
                    io: io.clone(),
                    crypto: crypto[index % crypto.len()].clone(),
                    nic: nic.clone(),
                });
            }
            remaining -= count.max(2);
            available -= count;
        }
        Ok(Self { pairs, max_threads })
    }
    /// Deterministic execution groups ordered by crypto CPU, then pair index.
    pub fn crypto_groups(&self) -> Vec<Vec<usize>> {
        let mut groups = BTreeMap::<usize, Vec<usize>>::new();
        for (index, pair) in self.pairs.iter().enumerate() {
            groups.entry(pair.crypto.cpu).or_default().push(index);
        }
        groups.into_values().collect()
    }
    /// Drop unfunded shards and rebalance surviving local groups without adding
    /// threads or moving an assignment across a known NUMA boundary.
    pub(crate) fn reduce_workers(&mut self, count: usize) {
        self.pairs.truncate(count);
        let mut nodes = BTreeMap::<_, Vec<usize>>::new();
        for (index, pair) in self.pairs.iter().enumerate() {
            nodes.entry(pair.io.numa_node).or_default().push(index);
        }
        for indices in nodes.into_values() {
            let mut crypto = BTreeMap::new();
            for &index in &indices {
                let cpu = &self.pairs[index].crypto;
                crypto.entry(cpu.cpu).or_insert_with(|| cpu.clone());
            }
            let crypto = crypto
                .into_values()
                .take(indices.len().div_ceil(2))
                .collect::<Vec<_>>();
            for (offset, index) in indices.into_iter().enumerate() {
                self.pairs[index].crypto = crypto[offset % crypto.len()].clone();
            }
        }
    }
}

/// Constructed on I/O; never move the local graph to the crypto thread.
/// ```compile_fail
/// use racer_dataplane::worker::WorkerRuntime;
/// fn require_send<T: Send>() {}
/// require_send::<WorkerRuntime>();
/// ```
pub struct WorkerRuntime {
    pub reactor: Rc<Reactor>,
    pub admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    pub crypto: Rc<CryptoClient>,
}
/// Send endpoint is moved before construction on the crypto thread. No I/O
/// reactor, admission authority, or worker-local Rc can be supplied to the engine.
pub struct CryptoRuntime {
    pub port: CryptoPort,
}

/// Rc-backed factories cannot cross the startup boundary:
/// ```compile_fail
/// use std::rc::Rc;
/// use racer_dataplane::{error::Result, runtime::RequestScope};
/// use uring_runtime::group::{Factory, Service};
/// struct LocalFactory(Rc<()>);
/// impl Factory<RequestScope> for LocalFactory {
///     fn build_lane(&self, _: usize) -> Result<Box<dyn Service<RequestScope>>> { todo!() }
/// }
/// ```
pub struct WorkerGroup {
    plan: AffinityPlan,
    runtime: Group<RequestScope>,
    generation: u64,
}

#[cfg(test)]
trait FaultRecipe: Sync {
    /// Per-worker limits. Application factories should return their validated
    /// configuration limits here; defaults allow bounded maximum-page progress.
    /// No Config is available to WorkerGroup::new, so this is the composition hook.
    fn limits(&self) -> Limits {
        let n = |value| NonZeroUsize::new(value).expect("positive default");
        Limits {
            plaintext_bytes: n(32 * 1024 * 1024),
            ciphertext_bytes: n(32 * 1024 * 1024),
            dirty_bytes: n(32 * 1024 * 1024),
            registered_bytes: n(32 * 1024 * 1024),
            request_context_bytes: n(4 * 1024 * 1024),
            flights: n(64),
            waiters_per_flight: n(64),
            queue_entries: n(64),
            connections_per_neighbor: n(2),
            client_connections: n(128),
            pipes: n(16),
            range_window_pages: n(2),
            header_bytes: n(32768),
            cached_rankings: n(128),
            cached_paths: n(128),
            retained_snapshots: n(2),
            metadata_entries: n(1024),
            relay_transfers: n(16),
        }
    }
    fn build(
        &self,
        worker: WorkerId,
        runtime: WorkerRuntime,
    ) -> Result<Box<dyn Service<RequestScope>>>;
    fn build_crypto(
        &self,
        worker: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn Service<RequestScope>>>;
}

impl WorkerGroup {
    pub fn new(plan: AffinityPlan) -> Self {
        Self {
            runtime: Group::new(runtime_plan(&plan)),
            plan,
            generation: 0,
        }
    }
    /// Driver-controlled adapter over the same scoped execution used by `run`.
    /// The coordinator becomes the first I/O worker, not an extra helper thread.
    /// The external driver still requires one slot in the whole-process budget.
    pub fn start(
        &mut self,
        factory: Arc<dyn Factory<RequestScope> + Send>,
        scope: &RequestScope,
    ) -> Result<()> {
        self.prepare(false)?;
        self.runtime.start(factory, scope)
    }
    pub fn run(&mut self, factory: &dyn Factory<RequestScope>) -> Result<()> {
        let scope = lifecycle_scope()?;
        self.prepare(true)?;
        self.runtime.run(factory, &scope)
    }
    pub fn run_with_scope(
        &mut self,
        factory: &dyn Factory<RequestScope>,
        scope: &RequestScope,
    ) -> Result<()> {
        self.prepare(true)?;
        self.runtime.run_with_scope(factory, scope)
    }
    #[cfg(test)]
    fn start_recipe(
        &mut self,
        factory: Arc<dyn FaultRecipe + Send>,
        scope: &RequestScope,
    ) -> Result<()> {
        self.start_with_allocator(factory, scope, security::try_pair)
    }

    #[cfg(test)]
    fn start_with_allocator(
        &mut self,
        factory: Arc<dyn FaultRecipe + Send>,
        scope: &RequestScope,
        allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<()> {
        self.prepare(false)?;
        let resources = self.allocate(&*factory, allocate)?;
        self.runtime
            .start(Arc::new(Adapter { factory, resources }), scope)
    }
    /// Stop I/O admission first. Drive both roles during I/O drain, then close
    /// crypto submissions and reap all completions. Deadline expiry requests
    /// cancellation but does not release resources before completion fences.
    pub fn drain(&mut self, scope: &RequestScope) -> Result<()> {
        self.runtime.drain(scope)
    }
    /// After drain, shut down both services and fence kernel/NIC references.
    /// Never terminate crypto with an outstanding job or unconsumed completion.
    pub fn shutdown(&mut self, scope: &RequestScope) -> Result<()> {
        self.runtime.shutdown(scope)
    }
    /// Join every I/O and unique crypto OS thread, including partial startup.
    /// Cannot succeed while a service can still access its retained resources.
    pub fn join(&mut self) -> Result<()> {
        self.runtime.join()
    }
    /// Ordered start, budgeted drive, drain, shutdown, and join (also on failure).
    #[cfg(test)]
    fn run_recipe(&mut self, factory: &dyn FaultRecipe) -> Result<()> {
        let scope = lifecycle_scope()?;
        self.run_inner(factory, &scope, false)
    }

    /// Unlike `run`, also treats this scope's cancellation/deadline as a request
    /// to stop the steady-state loop. Neither form truncates completion fencing.
    #[cfg(test)]
    fn run_recipe_with_scope(
        &mut self,
        factory: &dyn FaultRecipe,
        scope: &RequestScope,
    ) -> Result<()> {
        self.run_inner(factory, scope, true)
    }

    fn prepare(&mut self, caller_is_worker: bool) -> Result<()> {
        if self.runtime.stats().coordinators != 0 || self.plan.pairs.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let mut workers = HashSet::new();
        let mut crypto_locations = HashMap::new();
        for pair in &self.plan.pairs {
            if !workers.insert(pair.worker) {
                return Err(Error::InvalidConfiguration);
            }
            if matches!((pair.io.numa_node, pair.crypto.numa_node), (Some(io), Some(crypto)) if io != crypto)
            {
                return Err(Error::InvalidConfiguration);
            }
            // Public plan literals bypass discovery. One execution CPU must have
            // one consistent topology record across all of its shared shards.
            let location = (pair.crypto.package, pair.crypto.core, pair.crypto.numa_node);
            if crypto_locations
                .insert(pair.crypto.cpu, location)
                .is_some_and(|previous| previous != location)
            {
                return Err(Error::InvalidConfiguration);
            }
        }
        self.runtime = Group::new(runtime_plan(&self.plan));
        self.runtime.validate(caller_is_worker)?;
        self.generation = self
            .generation
            .checked_add(1)
            .ok_or(Error::InvalidConfiguration)?;
        Ok(())
    }

    #[cfg(test)]
    fn allocate(
        &self,
        factory: &dyn FaultRecipe,
        mut allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<Resources> {
        // Allocate every endpoint before spawning even the coordinator. Panics
        // in caller policy and fallible queue allocation have identical rollback.
        std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let limits = factory.limits();
            let mut ports = Vec::new();
            for pair in &self.plan.pairs {
                let (io, crypto) = allocate(pair.worker, self.generation, limits.queue_entries)?;
                ports.push(Mutex::new((Some(io), Some(crypto))));
            }
            Ok(Resources {
                limits,
                workers: self.plan.pairs.iter().map(|p| p.worker).collect(),
                ports,
            })
        }))
        .unwrap_or(Err(Error::Io))
    }

    #[cfg(test)]
    fn run_inner(
        &mut self,
        factory: &dyn FaultRecipe,
        scope: &RequestScope,
        check_scope: bool,
    ) -> Result<()> {
        self.run_with_allocator(factory, scope, check_scope, security::try_pair)
    }

    #[cfg(test)]
    fn run_with_allocator(
        &mut self,
        factory: &dyn FaultRecipe,
        scope: &RequestScope,
        check_scope: bool,
        allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<()> {
        self.prepare(true)?;
        let resources = self.allocate(factory, allocate)?;
        let adapter = Adapter { factory, resources };
        if check_scope {
            self.runtime.run_with_scope(&adapter, scope)
        } else {
            self.runtime.run(&adapter, scope)
        }
    }
}

impl Drop for WorkerGroup {
    fn drop(&mut self) {
        let _ = self.join();
    }
}

struct ThreadWake(thread::Thread, Option<ReactorWake>);
impl Wake for ThreadWake {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }
    fn wake_by_ref(self: &Arc<Self>) {
        self.0.unpark();
        if let Some(reactor) = &self.1 {
            let _ = reactor.wake();
        }
    }
}

fn driver_waker(runtime: Option<&WorkerRuntime>) -> Result<Waker> {
    let reactor = runtime.map(|runtime| runtime.reactor.waker()).transpose()?;
    Ok(Waker::from(Arc::new(ThreadWake(
        thread::current(),
        reactor,
    ))))
}

/// Independently drive resource completions while a service future borrows its
/// graph. The generic group owns scheduling, cancellation, and lifecycle phases.
fn drive_local<'a>(
    operation: Operation<'a, ()>,
    runtime: &'a WorkerRuntime,
    reporter: Option<&'a FailureReporter<Error>>,
) -> Operation<'a, ()> {
    drive_local_with(
        operation,
        reporter,
        move |waker| {
            runtime.crypto.register_driver(waker);
            poll_runtime(runtime)
        },
        move || runtime.reactor.wait(IDLE_WAIT),
    )
}

fn drive_local_with<'a>(
    mut operation: Operation<'a, ()>,
    reporter: Option<&'a FailureReporter<Error>>,
    mut poll: impl FnMut(&Waker) -> Result<()> + 'a,
    mut wait: impl FnMut() -> Result<()> + 'a,
) -> Operation<'a, ()> {
    let mut error = None;
    Box::pin(std::future::poll_fn(move |cx| {
        if let Err(failure) = poll(cx.waker()) {
            if let Some(reporter) = reporter {
                reporter.report(failure);
            }
            error.get_or_insert(failure);
        }
        if let Poll::Ready(result) = operation.as_mut().poll(cx) {
            return Poll::Ready(error.map_or(result, Err));
        }
        if let Err(failure) = wait() {
            if let Some(reporter) = reporter {
                reporter.report(failure);
            }
            error.get_or_insert(failure);
        }
        // The reactor already performed the bounded wait. Do not add a second
        // generic executor sleep before consuming its newly available CQEs.
        thread::current().unpark();
        Poll::Pending
    }))
}

fn lifecycle_scope() -> Result<RequestScope> {
    Ok(RequestScope {
        body_deadlines: None,
        request: RequestId([0; 16]),
        deadline: Deadline(uring_runtime::environment::now() + Duration::from_secs(30)),
        cancellation: Cancellation::new()?,
    })
}

fn poll_runtime(runtime: &WorkerRuntime) -> Result<()> {
    let crypto = runtime.crypto.poll_budgeted(WORK_BUDGET);
    let reactor = runtime.reactor.poll_budgeted(WORK_BUDGET);
    crypto.and(reactor.map(|_| ()))
}

fn fence_runtime<'a>(
    runtime: &'a WorkerRuntime,
    scope: &'a RequestScope,
    reporter: Option<&'a FailureReporter<Error>>,
) -> Operation<'a, ()> {
    // Service drain has ended: detach any delivery waiters it left behind, while
    // still retaining every accepted job until the engine returns its completion.
    Box::pin(async move {
        let fence_scope = lifecycle_scope().unwrap_or_else(|_| scope.clone());
        let cancelled = fence_scope.cancel();
        let drained = drive_local(
            Box::pin(async {
                let (crypto, reactor) =
                    futures::join!(runtime.crypto.drain(&fence_scope), runtime.reactor.drain());
                crypto.and(reactor)
            }),
            runtime,
            reporter,
        )
        .await;
        // Backend failures do not relax ownership fences. A permanently failed
        // backend can therefore prevent join; leaking live owners is not success.
        let mut error = None;
        std::future::poll_fn(|cx| {
            runtime.crypto.register_driver(cx.waker());
            if let Err(failure) = poll_runtime(runtime) {
                if let Some(reporter) = reporter {
                    reporter.report(failure);
                }
                error.get_or_insert(failure);
            }
            if runtime.crypto.outstanding() == 0 && runtime.reactor.in_flight() == 0 {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        cancelled.and(drained).and(error.map_or(Ok(()), Err))
    })
}

fn runtime_plan(plan: &AffinityPlan) -> Plan {
    Plan {
        lanes: plan
            .pairs
            .iter()
            .map(|pair| Lane {
                name: format!("racer-io-{}", pair.worker.0),
                cpu: pair.io.cpu,
            })
            .collect(),
        helpers: plan
            .crypto_groups()
            .into_iter()
            .map(|lanes| {
                let cpu = plan.pairs[lanes[0]].crypto.cpu;
                Helper {
                    name: format!("racer-crypto-{cpu}"),
                    cpu,
                    lanes,
                }
            })
            .collect(),
        max_threads: plan.max_threads,
    }
}
pub struct Resources {
    limits: Limits,
    workers: Vec<WorkerId>,
    ports: Vec<Mutex<(Option<IoCryptoPort>, Option<CryptoPort>)>>,
}
impl Resources {
    pub fn new(limits: Limits, workers: Vec<WorkerId>, generation: u64) -> Result<Self> {
        let mut unique = HashSet::new();
        if workers.is_empty() || workers.iter().any(|worker| !unique.insert(*worker)) {
            return Err(Error::InvalidConfiguration);
        }
        let mut ports = Vec::new();
        ports
            .try_reserve_exact(workers.len())
            .map_err(|_| Error::Overloaded)?;
        for worker in &workers {
            let (io, crypto) = security::try_pair(*worker, generation, limits.queue_entries)?;
            ports.push(Mutex::new((Some(io), Some(crypto))));
        }
        Ok(Self {
            limits,
            workers,
            ports,
        })
    }
    pub fn abandon_lane(&self, lane: usize) -> Result<()> {
        if let Some(port) = self
            .ports
            .get(lane)
            .ok_or(Error::InvalidConfiguration)?
            .lock()
            .map_err(|_| Error::Io)?
            .0
            .take()
        {
            port.close_submissions()?;
        }
        Ok(())
    }
    /// Construct on the owning I/O thread. Retain resources even when graph
    /// construction fails or panics so group teardown closes and fences them.
    pub fn build_lane(
        &self,
        lane: usize,
        build: impl FnOnce(WorkerId, WorkerRuntime) -> Result<Box<dyn Service<RequestScope>>>,
    ) -> Result<Box<dyn Service<RequestScope>>> {
        let port = self
            .ports
            .get(lane)
            .ok_or(Error::InvalidConfiguration)?
            .lock()
            .map_err(|_| Error::Io)?
            .0
            .take()
            .ok_or(Error::Io)?;
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            self.limits.clone(),
        )));
        let runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
            admission,
            crypto: Rc::new(CryptoClient::new(port)),
        };
        let built = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            build(
                self.workers[lane],
                WorkerRuntime {
                    reactor: runtime.reactor.clone(),
                    admission: runtime.admission.clone(),
                    crypto: runtime.crypto.clone(),
                },
            )
        }))
        .unwrap_or(Err(Error::Io));
        Ok(Box::new(IoService {
            runtime,
            built,
            reporter: None,
        }))
    }
    pub fn build_helper(
        &self,
        lane: usize,
        build: impl FnOnce(WorkerId, CryptoRuntime) -> Result<Box<dyn Service<RequestScope>>>,
    ) -> Result<Box<dyn Service<RequestScope>>> {
        let port = self
            .ports
            .get(lane)
            .ok_or(Error::InvalidConfiguration)?
            .lock()
            .map_err(|_| Error::Io)?
            .1
            .take()
            .ok_or(Error::Io)?;
        build(self.workers[lane], CryptoRuntime { port })
    }
}
#[cfg(test)]
struct Adapter<F> {
    factory: F,
    resources: Resources,
}
#[cfg(test)]
impl<F> Factory<RequestScope> for Adapter<F>
where
    F: Deref + Sync,
    F::Target: FaultRecipe,
{
    fn abandon_lane(&self, lane: usize) -> Result<()> {
        self.resources.abandon_lane(lane)
    }
    fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<RequestScope>>> {
        self.resources
            .build_lane(lane, |worker, runtime| self.factory.build(worker, runtime))
    }
    fn build_helper(&self, lane: usize) -> Result<Box<dyn Service<RequestScope>>> {
        self.resources.build_helper(lane, |worker, runtime| {
            self.factory.build_crypto(worker, runtime)
        })
    }
    fn teardown_scope(&self, startup: &RequestScope) -> RequestScope {
        lifecycle_scope().unwrap_or_else(|_| startup.clone())
    }
}
struct IoService {
    runtime: WorkerRuntime,
    built: Result<Box<dyn Service<RequestScope>>>,
    reporter: Option<FailureReporter<Error>>,
}
impl Service<RequestScope> for IoService {
    fn set_failure_reporter(&mut self, reporter: FailureReporter<Error>) {
        self.reporter = Some(reporter);
    }
    fn waker(&self) -> Result<Waker> {
        driver_waker(Some(&self.runtime))
    }
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        match &mut self.built {
            Ok(service) => drive_local(service.start(scope), &self.runtime, self.reporter.as_ref()),
            Err(error) => {
                let error = *error;
                Box::pin(async move { Err(error) })
            }
        }
    }
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
        self.runtime.crypto.register_driver(cx.waker());
        poll_runtime(&self.runtime)?;
        if let Ok(service) = &mut self.built {
            service.poll_budgeted(cx, budget)?;
        }
        self.runtime.reactor.wait(IDLE_WAIT)?;
        thread::current().unpark();
        Ok(())
    }
    fn stop_admission(&mut self) -> Result<()> {
        self.runtime.admission.stop();
        match &mut self.built {
            Ok(service) => service.stop_admission(),
            Err(_) => Ok(()),
        }
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        match &mut self.built {
            Ok(service) => drive_local(service.drain(scope), &self.runtime, self.reporter.as_ref()),
            Err(_) => Box::pin(async { Ok(()) }),
        }
    }
    fn close(&mut self) -> Result<()> {
        self.runtime.crypto.close_submissions()
    }
    fn fence<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        fence_runtime(&self.runtime, scope, self.reporter.as_ref())
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        match &mut self.built {
            Ok(service) => drive_local(
                service.shutdown(scope),
                &self.runtime,
                self.reporter.as_ref(),
            ),
            Err(_) => Box::pin(async { Ok(()) }),
        }
    }
}

#[cfg(test)]
fn colocated_plan(max_threads: usize, workers: u16) -> AffinityPlan {
    let location = uring_runtime::affinity::CpuLocation {
        cpu: *current_cpus().unwrap().first().unwrap(),
        package: 0,
        core: 0,
        numa_node: None,
    };
    AffinityPlan {
        pairs: (0..workers)
            .map(|id| WorkerPair {
                worker: WorkerId(id),
                io: location.clone(),
                crypto: location.clone(),
                nic: None,
            })
            .collect(),
        max_threads,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn direct_factory_resources_validate_sets_consume_once_and_fence() {
        use uring_runtime::group::Factory;
        let limits = crate::test_support::cluster::config(false).limits;
        for workers in [vec![], vec![WorkerId(7), WorkerId(7)]] {
            assert!(matches!(
                Resources::new(limits.clone(), workers, 9),
                Err(Error::InvalidConfiguration)
            ));
        }
        let resources = Resources::new(limits.clone(), vec![WorkerId(7)], 9).unwrap();
        assert_eq!(resources.abandon_lane(1), Err(Error::InvalidConfiguration));
        resources.abandon_lane(0).unwrap();
        resources.abandon_lane(0).unwrap();
        assert!(matches!(
            resources.build_lane(0, |_, _| unreachable!("abandoned lane")),
            Err(Error::Io)
        ));
        assert!(matches!(
            resources.build_helper(1, |_, _| unreachable!("invalid lane")),
            Err(Error::InvalidConfiguration)
        ));
        let helper = resources
            .build_helper(0, |worker, runtime| {
                assert_eq!(worker, WorkerId(7));
                Ok(Box::new(crate::security::PageCryptoEngine::new(runtime)))
            })
            .unwrap();
        assert!(matches!(
            resources.build_helper(0, |_, _| unreachable!("consumed helper")),
            Err(Error::Io)
        ));
        drop(helper);

        struct DirectFactory(Resources);
        impl Factory<RequestScope> for DirectFactory {
            fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<RequestScope>>> {
                self.0.build_lane(lane, |_, _| Ok(Box::new(Local)))
            }
            fn build_helper(&self, lane: usize) -> Result<Box<dyn Service<RequestScope>>> {
                self.0.build_helper(lane, |_, runtime| {
                    Ok(Box::new(crate::security::PageCryptoEngine::new(runtime)))
                })
            }
            fn abandon_lane(&self, lane: usize) -> Result<()> {
                self.0.abandon_lane(lane)
            }
            fn teardown_scope(&self, _: &RequestScope) -> RequestScope {
                lifecycle_scope().unwrap()
            }
        }
        struct Local;
        impl Service<RequestScope> for Local {
            fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move { scope.check() })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        let mut group = WorkerGroup::new(colocated_plan(3, 1));
        let factory = Arc::new(DirectFactory(
            Resources::new(limits, vec![WorkerId(0)], 10).unwrap(),
        ));
        let scope = lifecycle_scope().unwrap();
        group.start(factory, &scope).unwrap();
        group.drain(&scope).unwrap();
        group.shutdown(&scope).unwrap();
        group.join().unwrap();
    }
    use racer_control_wire::CacheId;
    mod affinity;
    mod shared_tests {
        use super::*;
        use crate::admission::ResourceClass;
        use crate::memory::BufferPool;
        use crate::model::*;
        use crate::security::CryptoInput;
        use crate::security::CryptoOutput;
        use crate::security::PageCryptoEngine;
        use racer_identity::KeyPurpose;
        use racer_identity::Keyring;
        use std::sync::atomic::AtomicBool;
        use std::sync::atomic::AtomicUsize;
        use std::sync::atomic::Ordering;

        #[derive(Default)]
        struct Observed {
            events: Mutex<Vec<(u16, &'static str, thread::ThreadId)>>,
            polls: Mutex<Vec<u16>>,
            submitted: AtomicUsize,
            release: AtomicBool,
            io_draining: AtomicUsize,
            shutdown: AtomicUsize,
            completed: AtomicUsize,
            native: Mutex<Vec<rdma_verbs::IoPort>>,
        }
        impl Observed {
            fn event(&self, worker: WorkerId, name: &'static str) {
                self.events
                    .lock()
                    .unwrap()
                    .push((worker.0, name, thread::current().id()));
            }
        }

        #[derive(Clone, Copy, PartialEq, Eq)]
        enum Failure {
            None,
            BuildCrypto,
            StartCrypto,
            PendingStart,
            BuildIo,
            StartIo,
            PollCrypto,
            DrainCrypto,
            ShutdownCrypto,
        }

        struct Factory {
            observed: Arc<Observed>,
            failure: Failure,
            jobs: bool,
            roundtrip: bool,
            cpu: usize,
            second_crypto_cpu: usize,
            native: bool,
        }

        fn fixture(cap: usize, failure: Failure, jobs: bool) -> (AffinityPlan, Factory) {
            let plan = colocated_plan(cap, 2);
            let cpu = plan.pairs[0].io.cpu;
            (
                plan,
                Factory {
                    observed: Arc::new(Observed::default()),
                    failure,
                    jobs,
                    roundtrip: false,
                    cpu,
                    second_crypto_cpu: cpu,
                    native: false,
                },
            )
        }

        impl FaultRecipe for Factory {
            fn limits(&self) -> Limits {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.queue_entries = NonZeroUsize::new(4).unwrap();
                limits
            }
            fn build(
                &self,
                worker: WorkerId,
                runtime: WorkerRuntime,
            ) -> Result<Box<dyn Service<RequestScope>>> {
                assert_eq!(
                    current_cpus().unwrap(),
                    std::collections::BTreeSet::from([self.cpu])
                );
                self.observed.event(worker, "io-build");
                if worker.0 == 1 && self.failure == Failure::BuildIo {
                    return Err(Error::InvalidRequest);
                }
                Ok(Box::new(Io {
                    worker,
                    runtime,
                    observed: self.observed.clone(),
                    keys: crate::test_support::security::keys(),
                    failure: self.failure,
                    jobs: self.jobs,
                    roundtrip: self.roundtrip,
                }))
            }
            fn build_crypto(
                &self,
                worker: WorkerId,
                runtime: CryptoRuntime,
            ) -> Result<Box<dyn Service<RequestScope>>> {
                let cpu = if worker.0 == 1 {
                    self.second_crypto_cpu
                } else {
                    self.cpu
                };
                assert_eq!(
                    current_cpus().unwrap(),
                    std::collections::BTreeSet::from([cpu])
                );
                self.observed.event(worker, "crypto-build");
                if worker.0 == 1 && self.failure == Failure::BuildCrypto {
                    return Err(Error::Unauthorized);
                }
                let page = PageCryptoEngine::new(runtime);
                let engine: Box<dyn Service<RequestScope>> = if self.native {
                    let (io, native) = rdma_verbs::pair(1)?;
                    self.observed.native.lock().unwrap().push(io);
                    Box::new(crate::rdma::WithNative::new(page, native))
                } else {
                    Box::new(page)
                };
                Ok(Box::new(Engine {
                    worker,
                    engine: Some(engine),
                    observed: self.observed.clone(),
                    failure: self.failure,
                    local: Rc::new(thread::current().id()),
                }))
            }
        }

        struct Io {
            worker: WorkerId,
            runtime: WorkerRuntime,
            observed: Arc<Observed>,
            keys: Keyring,
            failure: Failure,
            jobs: bool,
            roundtrip: bool,
        }

        impl Io {
            fn input(&self) -> CryptoInput {
                let cache = CacheId(crate::test_support::security::CACHE.into());
                CryptoInput::Encrypt {
                    page: PageId {
                        version: ObjectVersion {
                            object: ObjectId {
                                cache: cache.clone(),
                                key: CacheKey([0; 32]),
                            },
                            etag: StrongEtag::test_value("v1"),
                        },
                        number: PageNumber(0),
                    },
                    plaintext: BufferPool::new(self.runtime.admission.clone())
                        .plaintext(
                            self.runtime
                                .admission
                                .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                                .unwrap(),
                            1,
                        )
                        .unwrap(),
                    ciphertext: self
                        .runtime
                        .admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                        .unwrap(),
                }
            }
        }

        impl Service<RequestScope> for Io {
            fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.observed.event(self.worker, "io-start");
                    if self.roundtrip {
                        let cache = CacheId(crate::test_support::security::CACHE.into());
                        let key = self.keys.active(&cache, KeyPurpose::Page)?;
                        let CryptoOutput::Encrypted(plain, ciphertext) = self
                            .runtime
                            .crypto
                            .execute(self.input(), key, scope)
                            .await?
                        else {
                            panic!("encrypted output")
                        };
                        drop(plain);
                        let key = self.keys.active(&cache, KeyPurpose::Page)?;
                        let input = CryptoInput::Decrypt {
                            ciphertext,
                            plaintext: self.runtime.admission.reserve(
                                Some(&cache),
                                ResourceClass::Plaintext,
                                1,
                            )?,
                        };
                        let CryptoOutput::Decrypted(plain, _) =
                            self.runtime.crypto.execute(input, key, scope).await?
                        else {
                            panic!("decrypted output")
                        };
                        assert_eq!(plain.bytes(), &[0]);
                        self.observed.completed.fetch_add(1, Ordering::SeqCst);
                    }
                    if self.jobs {
                        // Submit several real AEAD jobs, abandoning delivery while their
                        // bounded queue permits and allocations remain in flight.
                        for _ in 0..4 {
                            let key = self
                                .keys
                                .active(
                                    &CacheId(crate::test_support::security::CACHE.into()),
                                    KeyPurpose::Page,
                                )
                                .unwrap();
                            let mut operation =
                                self.runtime.crypto.execute(self.input(), key, scope);
                            assert!(
                                operation
                                    .as_mut()
                                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                                    .is_pending()
                            );
                            drop(operation);
                            self.observed.submitted.fetch_add(1, Ordering::SeqCst);
                        }
                    }
                    if self.worker.0 == 1 && self.failure == Failure::StartIo {
                        Err(Error::InvalidRequest)
                    } else {
                        Ok(())
                    }
                })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }
            fn stop_admission(&mut self) -> Result<()> {
                self.observed.event(self.worker, "io-stop");
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.observed.event(self.worker, "io-drain");
                    self.observed.io_draining.fetch_add(1, Ordering::SeqCst);
                    Ok(())
                })
            }
            fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    assert_eq!(self.runtime.crypto.outstanding(), 0);
                    assert_eq!(self.runtime.admission.used(ResourceClass::Plaintext), 0);
                    assert_eq!(self.runtime.admission.used(ResourceClass::Ciphertext), 0);
                    self.observed.event(self.worker, "io-shutdown");
                    Ok(())
                })
            }
        }

        struct Engine {
            worker: WorkerId,
            // Observe the concrete native/page service from outside, including destruction.
            engine: Option<Box<dyn Service<RequestScope>>>,
            observed: Arc<Observed>,
            failure: Failure,
            local: Rc<thread::ThreadId>,
        }
        impl Drop for Engine {
            fn drop(&mut self) {
                assert_eq!(*self.local, thread::current().id());
                drop(self.engine.take());
                self.observed.event(self.worker, "crypto-drop");
            }
        }
        impl Service<RequestScope> for Engine {
            fn register_driver(&self, waker: &Waker) {
                self.engine.as_ref().unwrap().register_driver(waker);
            }
            fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    std::future::poll_fn(|_| {
                        if self.worker.0 == 1 {
                            if self.failure == Failure::StartCrypto {
                                return Poll::Ready(Err(Error::Unauthorized));
                            }
                            if self.failure == Failure::PendingStart {
                                return Poll::Pending;
                            }
                        }
                        self.observed.event(self.worker, "crypto-start");
                        Poll::Ready(Ok(()))
                    })
                    .await?;
                    self.engine.as_mut().unwrap().start(scope).await
                })
            }
            fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(*self.local, thread::current().id());
                assert_eq!(budget, 1, "one page per shard per pass");
                let mut polls = self.observed.polls.lock().unwrap();
                if polls.len() < 128 {
                    polls.push(self.worker.0);
                }
                drop(polls);
                // Retain outstanding jobs until tests request release or drain begins.
                if self.observed.release.load(Ordering::SeqCst)
                    || self.observed.io_draining.load(Ordering::SeqCst) > 0
                {
                    self.engine.as_mut().unwrap().poll_budgeted(cx, budget)?;
                }
                if self.worker.0 == 1 && self.failure == Failure::PollCrypto {
                    Err(Error::Io)
                } else {
                    Ok(())
                }
            }
            fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.observed.event(self.worker, "crypto-drain");
                    // A pending drain of one shard needs the other shard to drain. This
                    // deadlocks if the shared thread blocks on lifecycle futures in order.
                    std::future::poll_fn(|_| {
                        let sibling_entered =
                            self.observed
                                .events
                                .lock()
                                .unwrap()
                                .iter()
                                .any(|(worker, name, _)| {
                                    *worker != self.worker.0 && *name == "crypto-drain"
                                });
                        if self.worker.0 == 0
                            && self.failure != Failure::BuildCrypto
                            && !sibling_entered
                        {
                            Poll::Pending
                        } else {
                            Poll::Ready(())
                        }
                    })
                    .await;
                    self.engine.as_mut().unwrap().drain(scope).await?;
                    if self.worker.0 == 1 && self.failure == Failure::DrainCrypto {
                        Err(Error::Io)
                    } else {
                        Ok(())
                    }
                })
            }
            fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.observed.event(self.worker, "crypto-shutdown");
                    self.observed.shutdown.fetch_add(1, Ordering::SeqCst);
                    std::future::poll_fn(|_| {
                        let expected = if self.failure == Failure::BuildCrypto {
                            1
                        } else {
                            2
                        };
                        if self.observed.shutdown.load(Ordering::SeqCst) == expected {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    })
                    .await;
                    self.engine.as_mut().unwrap().shutdown(scope).await?;
                    if self.worker.0 == 1 && self.failure == Failure::ShutdownCrypto {
                        Err(Error::Io)
                    } else {
                        Ok(())
                    }
                })
            }
        }

        fn scope() -> RequestScope {
            RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(3)).unwrap()
        }

        #[test]
        fn two_io_share_one_actual_crypto_thread_and_drain_outstanding_jobs() {
            let (plan, factory) = fixture(4, Failure::None, true);
            let observed = factory.observed.clone();
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            group.start_recipe(Arc::new(factory), &scope).unwrap();
            assert_eq!(
                group.runtime.stats().coordinators,
                1,
                "one coordinator owns the scoped workers"
            );
            assert_eq!(group.runtime.stats().total, 3);
            assert_eq!(group.runtime.stats().ready, 3);
            assert_eq!(observed.submitted.load(Ordering::SeqCst), 8);
            group.drain(&scope).unwrap();
            assert_eq!(group.runtime.stats().drained, 3);
            group.shutdown(&scope).unwrap();
            group.join().unwrap();
            assert_eq!(group.runtime.stats().done, 3);
            let events = observed.events.lock().unwrap();
            let thread_for = |worker, name| {
                events
                    .iter()
                    .find(|(id, event, _)| *id == worker && *event == name)
                    .unwrap()
                    .2
            };
            let crypto = thread_for(0, "crypto-build");
            assert_eq!(crypto, thread_for(1, "crypto-build"));
            assert_ne!(crypto, thread_for(0, "io-build"));
            assert_ne!(crypto, thread_for(1, "io-build"));
            assert_ne!(thread_for(0, "io-build"), thread_for(1, "io-build"));
            for worker in 0..2 {
                assert_eq!(crypto, thread_for(worker, "crypto-drop"));
                let position = |name| {
                    events
                        .iter()
                        .position(|(id, event, _)| *id == worker && *event == name)
                        .unwrap()
                };
                assert!(position("crypto-start") < position("io-build"));
                assert!(position("io-drain") < position("crypto-drain"));
                assert!(position("crypto-drain") < position("crypto-shutdown"));
                assert!(position("crypto-shutdown") < position("crypto-drop"));
            }
            // While both shards remain active, every pass grants each a single page and
            // reverses its starting order on the next pass, even with saturated queues.
            let polls = observed.polls.lock().unwrap();
            assert!(polls.len() >= 8);
            assert_eq!(&polls[..8], &[0, 1, 1, 0, 0, 1, 1, 0]);
        }

        #[test]
        fn shared_crypto_borrowed_run_counts_caller_and_restores_affinity() {
            let (plan, factory) = fixture(3, Failure::None, true);
            let mut group = WorkerGroup::new(plan);
            let before = current_cpus().unwrap();
            let mut scope = scope();
            // Leave startup enough room under concurrent builds: this scenario checks
            // deadline-driven teardown after all eight jobs, not startup latency.
            scope.deadline = Deadline(Instant::now() + Duration::from_secs(1));
            assert_eq!(
                group.run_recipe_with_scope(&factory, &scope),
                Err(Error::DeadlineExceeded)
            );
            assert_eq!(current_cpus().unwrap(), before);
            assert_eq!(group.runtime.stats().done, 3);
            assert_eq!(factory.observed.submitted.load(Ordering::SeqCst), 8);
            let (plan, factory) = fixture(3, Failure::None, false);
            assert_eq!(
                WorkerGroup::new(plan).start_recipe(Arc::new(factory), &scope),
                Err(Error::InvalidConfiguration)
            );
        }

        #[test]
        fn partial_group_build_start_and_pending_start_failures_teardown_every_built_service() {
            for failure in [
                Failure::BuildCrypto,
                Failure::StartCrypto,
                Failure::PendingStart,
                Failure::BuildIo,
                Failure::StartIo,
            ] {
                for borrowed in [false, true] {
                    let (plan, factory) = fixture(if borrowed { 3 } else { 4 }, failure, true);
                    let observed = factory.observed.clone();
                    let mut group = WorkerGroup::new(plan);
                    let mut scope = scope();
                    if failure == Failure::PendingStart {
                        scope.deadline = Deadline(Instant::now() + Duration::from_millis(30));
                    }
                    let expected = match failure {
                        Failure::PendingStart => Error::DeadlineExceeded,
                        Failure::BuildIo | Failure::StartIo => Error::InvalidRequest,
                        _ => Error::Unauthorized,
                    };
                    let result = if borrowed {
                        group.run_recipe_with_scope(&factory, &scope)
                    } else {
                        group.start_recipe(Arc::new(factory), &scope)
                    };
                    assert_eq!(result, Err(expected));
                    assert_eq!(group.runtime.stats().done, 3);
                    let events = observed.events.lock().unwrap();
                    for id in 0..if failure == Failure::BuildCrypto {
                        1
                    } else {
                        2
                    } {
                        assert!(
                            events
                                .iter()
                                .any(|(worker, event, _)| *worker == id
                                    && *event == "crypto-shutdown")
                        );
                        assert!(
                            events
                                .iter()
                                .any(|(worker, event, _)| *worker == id && *event == "crypto-drop")
                        );
                    }
                }
            }
        }

        #[test]
        fn shared_crypto_errors_do_not_skip_sibling_drain_or_shutdown() {
            for failure in [
                Failure::PollCrypto,
                Failure::DrainCrypto,
                Failure::ShutdownCrypto,
            ] {
                let (plan, factory) = fixture(4, failure, false);
                let observed = factory.observed.clone();
                let mut group = WorkerGroup::new(plan);
                let scope = scope();
                let started = group.start_recipe(Arc::new(factory), &scope);
                if failure == Failure::PollCrypto {
                    // The poll error may race the parent's readiness observation.
                    assert!(started == Ok(()) || started == Err(Error::Io));
                } else {
                    started.unwrap();
                }
                let _ = group.drain(&scope);
                let _ = group.shutdown(&scope);
                assert_eq!(group.join(), Err(Error::Io));
                assert_eq!(group.runtime.stats().done, 3);
                assert_eq!(observed.shutdown.load(Ordering::SeqCst), 2);
            }
        }

        #[test]
        fn shared_services_deliver_independent_real_encrypt_decrypt_results() {
            let (plan, mut factory) = fixture(4, Failure::None, false);
            factory.roundtrip = true;
            let observed = factory.observed.clone();
            observed.release.store(true, Ordering::SeqCst);
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            group.start_recipe(Arc::new(factory), &scope).unwrap();
            assert_eq!(observed.completed.load(Ordering::SeqCst), 2);
            group.drain(&scope).unwrap();
            group.shutdown(&scope).unwrap();
            group.join().unwrap();
        }

        #[test]
        fn shared_group_cancellation_during_live_jobs_fences_before_join() {
            let (plan, factory) = fixture(4, Failure::None, true);
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            group.start_recipe(Arc::new(factory), &scope).unwrap();
            scope.cancel().unwrap();
            // A canceled drain wait may return immediately, but join must still run the
            // real crypto engines and fence every abandoned accepted job.
            let drained = group.drain(&scope);
            assert!(drained == Err(Error::Cancelled) || drained == Ok(()));
            group.join().unwrap();
            assert_eq!(group.runtime.stats().done, 3);
        }

        #[test]
        fn pending_shard_start_does_not_block_ready_sibling_crypto_work() {
            let (plan, mut factory) = fixture(4, Failure::PendingStart, false);
            factory.roundtrip = true;
            let observed = factory.observed.clone();
            observed.release.store(true, Ordering::SeqCst);
            let mut group = WorkerGroup::new(plan);
            let mut scope = scope();
            scope.deadline = Deadline(Instant::now() + Duration::from_millis(100));
            assert_eq!(
                group.start_recipe(Arc::new(factory), &scope),
                Err(Error::DeadlineExceeded)
            );
            assert_eq!(observed.completed.load(Ordering::SeqCst), 1);
            assert_eq!(group.runtime.stats().done, 3);
        }

        #[test]
        fn later_crypto_group_allocation_failure_rolls_back_live_first_group() {
            let allowed = current_cpus().unwrap();
            let Some(&second_cpu) = allowed.iter().nth(1) else {
                return;
            };
            for borrowed in [false, true] {
                // Allocation is transactional across every helper group: neither
                // owned nor borrowed entry points may build even the first service.
                let (mut plan, mut factory) =
                    fixture(if borrowed { 4 } else { 5 }, Failure::BuildCrypto, false);
                plan.pairs[1].crypto.cpu = second_cpu;
                factory.second_crypto_cpu = second_cpu;
                let observed = factory.observed.clone();
                let mut group = WorkerGroup::new(plan);
                let scope = scope();
                let allocator_observed = observed.clone();
                let allocate = move |worker, generation, capacity| {
                    assert!(allocator_observed.events.lock().unwrap().is_empty());
                    if worker == WorkerId(0) {
                        return security::try_pair(worker, generation, capacity);
                    }
                    Err(Error::Overloaded)
                };
                let result = if borrowed {
                    group.run_with_allocator(&factory, &scope, false, allocate)
                } else {
                    group.start_with_allocator(Arc::new(factory), &scope, allocate)
                };
                assert_eq!(result, Err(Error::Overloaded));
                assert_eq!(group.runtime.stats().done, 0);
                assert_eq!(group.runtime.stats().coordinators, 0);
                assert_eq!(current_cpus().unwrap(), allowed);
                let events = observed.events.lock().unwrap();
                assert!(events.is_empty());
            }
        }

        #[test]
        fn distinct_crypto_cpus_create_distinct_execution_threads() {
            let allowed = current_cpus().unwrap();
            let Some(&second_cpu) = allowed.iter().nth(1) else {
                return;
            };
            let (mut plan, mut factory) = fixture(5, Failure::None, false);
            plan.pairs[1].crypto.cpu = second_cpu;
            factory.second_crypto_cpu = second_cpu;
            let observed = factory.observed.clone();
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            group.start_recipe(Arc::new(factory), &scope).unwrap();
            assert_eq!(group.runtime.stats().coordinators, 1);
            assert_eq!(group.runtime.stats().ready, 4);
            group.drain(&scope).unwrap();
            group.shutdown(&scope).unwrap();
            group.join().unwrap();
            let events = observed.events.lock().unwrap();
            let crypto = events
                .iter()
                .filter(|(_, name, _)| *name == "crypto-build")
                .map(|(_, _, id)| *id)
                .collect::<HashSet<_>>();
            assert_eq!(crypto.len(), 2);
        }

        #[test]
        fn shared_native_wrappers_are_drained_and_destroyed_on_owner_thread() {
            let (plan, mut factory) = fixture(4, Failure::None, true);
            factory.native = true;
            let observed = factory.observed.clone();
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            group.start_recipe(Arc::new(factory), &scope).unwrap();
            assert_eq!(observed.native.lock().unwrap().len(), 2);
            group.drain(&scope).unwrap();
            for io in observed.native.lock().unwrap().iter() {
                assert!(io.closed());
                assert!(io.pool_drained());
            }
            group.shutdown(&scope).unwrap();
            group.join().unwrap();
            for io in observed.native.lock().unwrap().iter() {
                assert_eq!(
                    io.reopen().map_err(Error::from),
                    Err(Error::Unavailable),
                    "native owner was destroyed"
                );
            }
            assert_eq!(observed.shutdown.load(Ordering::SeqCst), 2);
            assert_eq!(group.runtime.stats().done, 3);
            let events = observed.events.lock().unwrap();
            for worker in 0..2 {
                let event = |name| {
                    events
                        .iter()
                        .enumerate()
                        .find(|(_, (id, kind, _))| *id == worker && *kind == name)
                        .unwrap()
                };
                let (built, (_, _, owner)) = event("crypto-build");
                let (drained, (_, _, drain_thread)) = event("crypto-drain");
                let (shutdown, (_, _, shutdown_thread)) = event("crypto-shutdown");
                let (dropped, (_, _, drop_thread)) = event("crypto-drop");
                assert!(built < drained && drained < shutdown && shutdown < dropped);
                assert_eq!(owner, drain_thread);
                assert_eq!(owner, shutdown_thread);
                assert_eq!(
                    owner, drop_thread,
                    "outer observer records completed native destruction"
                );
            }
        }
    }

    mod lifecycle {
        use super::*;

        #[test]
        fn factory_and_crypto_runtime_have_cross_thread_bounds() {
            fn sync<T: Sync + ?Sized>() {}
            fn send<T: Send + 'static>() {}
            sync::<dyn uring_runtime::group::Factory<RequestScope>>();
            send::<CryptoRuntime>();
        }

        #[derive(Clone)]
        struct TestFactory {
            events: Arc<Mutex<Vec<&'static str>>>,
            crypto_polls: Arc<std::sync::atomic::AtomicUsize>,
            fail_io: bool,
            fail_crypto: bool,
            stop_on_poll: bool,
            cpu: usize,
        }
        struct TestIo {
            factory: TestFactory,
            local: Rc<()>,
            driver: Option<Waker>,
        }
        struct TestCrypto {
            factory: TestFactory,
            _port: CryptoPort,
        }
        impl TestFactory {
            fn event(&self, event: &'static str) {
                self.events.lock().unwrap().push(event);
            }
        }
        impl FaultRecipe for TestFactory {
            fn build(
                &self,
                _: WorkerId,
                _: WorkerRuntime,
            ) -> Result<Box<dyn Service<RequestScope>>> {
                assert_eq!(
                    current_cpus().unwrap(),
                    std::collections::BTreeSet::from([self.cpu])
                );
                self.event("io-build");
                if self.fail_io {
                    return Err(Error::InvalidRequest);
                }
                Ok(Box::new(TestIo {
                    factory: self.clone(),
                    local: Rc::new(()),
                    driver: None,
                }))
            }
            fn build_crypto(
                &self,
                _: WorkerId,
                runtime: CryptoRuntime,
            ) -> Result<Box<dyn Service<RequestScope>>> {
                assert_eq!(
                    current_cpus().unwrap(),
                    std::collections::BTreeSet::from([self.cpu])
                );
                self.event("crypto-build");
                if self.fail_crypto {
                    return Err(Error::Unauthorized);
                }
                Ok(Box::new(TestCrypto {
                    factory: self.clone(),
                    _port: runtime.port,
                }))
            }
        }
        impl Service<RequestScope> for TestIo {
            fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(std::future::poll_fn(move |cx| {
                    self.driver = Some(cx.waker().clone());
                    self.factory.event("io-start");
                    Poll::Ready(Ok(()))
                }))
            }
            fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
                assert!(
                    self.driver.as_ref().unwrap().will_wake(cx.waker()),
                    "steady-state polling must retain the lifecycle driver waker"
                );
                assert!(budget <= WORK_BUDGET);
                assert_eq!(Rc::strong_count(&self.local), 1);
                if self.factory.stop_on_poll {
                    Err(Error::Cancelled)
                } else {
                    Ok(())
                }
            }
            fn stop_admission(&mut self) -> Result<()> {
                self.factory.event("io-stop");
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                // The engine must still be running while I/O is draining on one CPU.
                let before = self
                    .factory
                    .crypto_polls
                    .load(std::sync::atomic::Ordering::SeqCst);
                Box::pin(std::future::poll_fn(move |cx| {
                    if self
                        .factory
                        .crypto_polls
                        .load(std::sync::atomic::Ordering::SeqCst)
                        == before
                    {
                        cx.waker().wake_by_ref();
                        return Poll::Pending;
                    }
                    self.factory.event("io-drain");
                    Poll::Ready(Ok(()))
                }))
            }
            fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.factory.event("io-shutdown");
                    Ok(())
                })
            }
        }
        impl Service<RequestScope> for TestCrypto {
            fn register_driver(&self, waker: &Waker) {
                self._port.register_driver(waker);
            }
            fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.factory.event("crypto-start");
                    Ok(())
                })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert!(budget <= WORK_BUDGET);
                self.factory
                    .crypto_polls
                    .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.factory.event("crypto-drain");
                    Ok(())
                })
            }
            fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    self.factory.event("crypto-shutdown");
                    Ok(())
                })
            }
        }
        fn fixture(max_threads: usize) -> (WorkerGroup, TestFactory) {
            let plan = colocated_plan(max_threads, 1);
            let cpu = plan.pairs[0].io.cpu;
            (
                WorkerGroup::new(plan),
                TestFactory {
                    events: Arc::new(Mutex::new(Vec::new())),
                    crypto_polls: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
                    fail_io: false,
                    fail_crypto: false,
                    stop_on_poll: false,
                    cpu,
                },
            )
        }

        #[test]
        fn prepare_rejects_known_numa_mismatch() {
            let (mut group, _) = fixture(3);
            group.plan.pairs[0].io.numa_node = Some(0);
            group.plan.pairs[0].crypto.numa_node = Some(1);
            assert_eq!(group.prepare(false), Err(Error::InvalidConfiguration));
            assert_eq!(group.generation, 0);
        }

        #[test]
        fn prepare_rejects_contradictory_shared_crypto_metadata() {
            for dimension in 0..3 {
                let (mut group, _) = fixture(4);
                let mut second = group.plan.pairs[0].clone();
                second.worker = WorkerId(1);
                match dimension {
                    0 => second.crypto.package += 1,
                    1 => second.crypto.core += 1,
                    _ => second.crypto.numa_node = Some(1),
                }
                group.plan.pairs.push(second);
                assert_eq!(group.prepare(false), Err(Error::InvalidConfiguration));
                assert_eq!(group.generation, 0);
            }
        }

        #[test]
        fn prepare_accepts_colocated_io_and_unknown_numa() {
            for (io, crypto) in [
                (None, None),
                (Some(0), None),
                (None, Some(0)),
                (Some(0), Some(0)),
            ] {
                let (mut group, _) = fixture(4);
                group.plan.pairs[0].io.numa_node = io;
                group.plan.pairs[0].crypto.numa_node = crypto;
                let mut second = group.plan.pairs[0].clone();
                second.worker = WorkerId(1);
                group.plan.pairs.push(second);
                group.prepare(false).unwrap();
                assert_eq!(
                    runtime_plan(&group.plan).lanes.len() + runtime_plan(&group.plan).helpers.len(),
                    3
                );
            }
        }

        #[test]
        fn owned_start_drains_and_joins_both_pinned_local_services() {
            let (mut group, factory) = fixture(3);
            let events = factory.events.clone();
            let scope = lifecycle_scope().unwrap();
            group.start_recipe(Arc::new(factory), &scope).unwrap();
            group.drain(&scope).unwrap();
            group.shutdown(&scope).unwrap();
            group.join().unwrap();
            let events = events.lock().unwrap();
            let position = |event| events.iter().position(|found| *found == event).unwrap();
            assert!(position("crypto-start") < position("io-build"));
            assert!(position("io-stop") < position("io-drain"));
            assert!(position("io-drain") < position("crypto-drain"));
            assert!(position("crypto-drain") < position("crypto-shutdown"));
            assert!(events.contains(&"io-shutdown"));
            assert_eq!(group.runtime.stats().done, 2);
            // Completed workers cannot retain cancellation slots in the caller's scope.
            let registrations = (0..1024)
                .map(|_| scope.cancellation.subscribe().unwrap())
                .collect::<Vec<_>>();
            assert!(matches!(
                scope.cancellation.subscribe(),
                Err(Error::Overloaded)
            ));
            drop(registrations);
        }

        #[test]
        fn cancellation_subscription_failure_tears_down_built_services() {
            let (mut group, factory) = fixture(3);
            let events = factory.events.clone();
            let scope = lifecycle_scope().unwrap();
            let _registrations = (0..1024)
                .map(|_| scope.cancellation.subscribe().unwrap())
                .collect::<Vec<_>>();
            assert_eq!(
                group.start_recipe(Arc::new(factory), &scope),
                Err(Error::Overloaded)
            );
            assert_eq!(group.runtime.stats().coordinators, 0);
            assert_eq!(group.runtime.stats().done, 2);
            assert!(events.lock().unwrap().contains(&"crypto-shutdown"));
        }

        #[test]
        fn startup_failure_rolls_back_and_owned_start_counts_caller() {
            let scope = lifecycle_scope().unwrap();
            let (mut group, factory) = fixture(2);
            assert_eq!(
                group.start_recipe(Arc::new(factory), &scope),
                Err(Error::InvalidConfiguration)
            );
            for fail_crypto in [false, true] {
                let (mut group, mut factory) = fixture(3);
                factory.fail_crypto = fail_crypto;
                factory.fail_io = !fail_crypto;
                let expected = if fail_crypto {
                    Error::Unauthorized
                } else {
                    Error::InvalidRequest
                };
                assert_eq!(group.start_recipe(Arc::new(factory), &scope), Err(expected));
                assert_eq!(group.runtime.stats().coordinators, 0);
                assert_eq!(group.runtime.stats().done, 2);
            }
        }

        #[test]
        fn borrowed_run_uses_caller_and_restores_affinity_on_failure() {
            let (group, mut factory) = fixture(2);
            // Reconstruct to infer the borrowed factory lifetime instead of 'static.
            let plan = AffinityPlan {
                pairs: group.plan.pairs.clone(),
                max_threads: 2,
            };
            factory.stop_on_poll = true;
            let before = current_cpus().unwrap();
            let mut group = WorkerGroup::new(plan);
            assert_eq!(group.run_recipe(&factory), Err(Error::Cancelled));
            assert_eq!(current_cpus().unwrap(), before);
            assert_eq!(group.runtime.stats().done, 2);
            assert!(factory.events.lock().unwrap().contains(&"crypto-shutdown"));
        }

        #[test]
        fn borrowed_run_scope_stops_and_joins_on_deadline() {
            let (group, factory) = fixture(2);
            let mut scoped = WorkerGroup::new(AffinityPlan {
                pairs: group.plan.pairs.clone(),
                max_threads: 2,
            });
            let mut scope = lifecycle_scope().unwrap();
            scope.deadline = Deadline(Instant::now() + Duration::from_millis(50));
            assert_eq!(
                scoped.run_recipe_with_scope(&factory, &scope),
                Err(Error::DeadlineExceeded)
            );
            assert_eq!(scoped.runtime.stats().done, 2);
        }

        #[test]
        fn engine_driver_wake_survives_noop_operation_poll() {
            struct Count(std::sync::atomic::AtomicUsize);
            impl Wake for Count {
                fn wake(self: Arc<Self>) {
                    self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                }
            }
            let (_, factory) = fixture(3);
            let (io, port) = security::pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
            let mut engine = TestCrypto {
                factory,
                _port: port,
            };
            let count = Arc::new(Count(std::sync::atomic::AtomicUsize::new(0)));
            engine.register_driver(&Waker::from(count.clone()));
            assert!(
                engine
                    ._port
                    .poll_job(&mut Context::from_waker(futures::task::noop_waker_ref()))
                    .is_pending()
            );
            io.close_submissions().unwrap();
            assert_eq!(count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
        }

        #[test]
        fn coordinator_panic_reports_failure_and_joins_without_waiting_for_startup_deadline() {
            let (mut group, factory) = fixture(3);
            let scope = lifecycle_scope().unwrap();
            assert_eq!(
                group.start_with_allocator(Arc::new(factory), &scope, |_, _, _| {
                    panic!("injected allocation panic")
                }),
                Err(Error::Io)
            );
            assert_eq!(group.runtime.stats().coordinators, 0);
            assert_eq!(group.runtime.stats().done, 0);
            let factory = fixture(3).1;
            let mut borrowed = WorkerGroup::new(colocated_plan(2, 1));
            let before = current_cpus().unwrap();
            assert_eq!(
                borrowed.run_with_allocator(&factory, &scope, false, |_, _, _| {
                    panic!("injected borrowed allocation panic")
                }),
                Err(Error::Io)
            );
            assert_eq!(borrowed.runtime.stats().done, 0);
            assert_eq!(current_cpus().unwrap(), before);
        }

        #[test]
        fn limits_panic_starts_no_owned_or_borrowed_threads() {
            struct PanicLimits;
            impl FaultRecipe for PanicLimits {
                fn limits(&self) -> Limits {
                    panic!("injected limits panic")
                }
                fn build(
                    &self,
                    _: WorkerId,
                    _: WorkerRuntime,
                ) -> Result<Box<dyn Service<RequestScope>>> {
                    panic!("must not build")
                }
                fn build_crypto(
                    &self,
                    _: WorkerId,
                    _: CryptoRuntime,
                ) -> Result<Box<dyn Service<RequestScope>>> {
                    panic!("must not build")
                }
            }
            let before = current_cpus().unwrap();
            let scope = lifecycle_scope().unwrap();
            let mut group = WorkerGroup::new(colocated_plan(3, 1));
            assert_eq!(
                group.start_recipe(Arc::new(PanicLimits), &scope),
                Err(Error::Io)
            );
            assert_eq!(group.runtime.stats().done, 0);
            assert_eq!(group.runtime.stats().coordinators, 0);
            assert_eq!(
                group.run_recipe_with_scope(&PanicLimits, &scope),
                Err(Error::Io)
            );
            assert_eq!(group.runtime.stats().done, 0);
            assert_eq!(current_cpus().unwrap(), before);
        }

        #[test]
        fn pending_start_backend_failure_notifies_group_before_deadline() {
            use std::sync::atomic::AtomicBool;
            use std::sync::atomic::AtomicUsize;
            use std::sync::atomic::Ordering;
            struct BackendFactory {
                sibling_started: Arc<AtomicBool>,
                sibling_drained: Arc<AtomicBool>,
                fences: Arc<AtomicUsize>,
                fail_wait: bool,
            }
            struct BackendService {
                sibling_started: Arc<AtomicBool>,
                lane: usize,
                reporter: Option<FailureReporter<Error>>,
                sibling_drained: Arc<AtomicBool>,
                fences: Arc<AtomicUsize>,
                fail_wait: bool,
            }
            impl Factory<RequestScope> for BackendFactory {
                fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<RequestScope>>> {
                    Ok(Box::new(BackendService {
                        sibling_started: self.sibling_started.clone(),
                        lane,
                        reporter: None,
                        sibling_drained: self.sibling_drained.clone(),
                        fences: self.fences.clone(),
                        fail_wait: self.fail_wait,
                    }))
                }
            }
            impl Service<RequestScope> for BackendService {
                fn set_failure_reporter(&mut self, reporter: FailureReporter<Error>) {
                    self.reporter = Some(reporter);
                }
                fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    if self.lane != 0 {
                        self.sibling_started.store(true, Ordering::SeqCst);
                        return Box::pin(async { Ok(()) });
                    }
                    let fail_wait = self.fail_wait;
                    let poll_ready = self.sibling_started.clone();
                    let wait_ready = self.sibling_started.clone();
                    // Exercise the exact resource-driver wrapper with a permanently
                    // pending application future and either backend failure site.
                    drive_local_with(
                        Box::pin(std::future::pending()),
                        self.reporter.as_ref(),
                        move |_| {
                            if !fail_wait && poll_ready.load(Ordering::SeqCst) {
                                Err(Error::Io)
                            } else {
                                Ok(())
                            }
                        },
                        move || {
                            if fail_wait && wait_ready.load(Ordering::SeqCst) {
                                Err(Error::Io)
                            } else {
                                Ok(())
                            }
                        },
                    )
                }
                fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                    Ok(())
                }
                fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    Box::pin(std::future::poll_fn(move |_| {
                        if self.lane == 1 {
                            self.sibling_drained.store(true, Ordering::SeqCst);
                        }
                        if self.sibling_drained.load(Ordering::SeqCst) {
                            Poll::Ready(Ok(()))
                        } else {
                            Poll::Pending
                        }
                    }))
                }
                fn fence<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    Box::pin(async move {
                        assert!(self.sibling_drained.load(Ordering::SeqCst));
                        self.fences.fetch_add(1, Ordering::SeqCst);
                        Ok(())
                    })
                }
                fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }
            }
            for fail_wait in [false, true] {
                let cpu = *current_cpus().unwrap().first().unwrap();
                let factory = Arc::new(BackendFactory {
                    sibling_started: Arc::default(),
                    sibling_drained: Arc::default(),
                    fences: Arc::default(),
                    fail_wait,
                });
                let mut group = Group::new(Plan {
                    lanes: (0..2)
                        .map(|i| Lane {
                            name: format!("failure-lane-{i}"),
                            cpu,
                        })
                        .collect(),
                    helpers: Vec::new(),
                    max_threads: 3,
                });
                let scope =
                    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(3))
                        .unwrap();
                let started = Instant::now();
                assert_eq!(group.start(factory.clone(), &scope), Err(Error::Io));
                assert!(
                    started.elapsed() < Duration::from_secs(1),
                    "backend failure must not wait for the startup deadline"
                );
                assert_eq!(group.stats().done, 2);
                assert_eq!(factory.fences.load(Ordering::SeqCst), 4);
            }
        }

        #[test]
        fn pending_drain_backend_failure_notifies_without_releasing_ownership() {
            use std::sync::atomic::AtomicBool;
            use std::sync::atomic::AtomicUsize;
            use std::sync::atomic::Ordering;
            struct PendingFactory {
                release: Arc<AtomicBool>,
                fences: Arc<AtomicUsize>,
            }
            struct PendingDrain {
                release: Arc<AtomicBool>,
                fences: Arc<AtomicUsize>,
                reporter: Option<FailureReporter<Error>>,
            }
            impl Factory<RequestScope> for PendingFactory {
                fn build_lane(&self, _: usize) -> Result<Box<dyn Service<RequestScope>>> {
                    Ok(Box::new(PendingDrain {
                        release: self.release.clone(),
                        fences: self.fences.clone(),
                        reporter: None,
                    }))
                }
            }
            impl Service<RequestScope> for PendingDrain {
                fn set_failure_reporter(&mut self, reporter: FailureReporter<Error>) {
                    self.reporter = Some(reporter);
                }
                fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }
                fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                    Ok(())
                }
                fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    let release = self.release.clone();
                    drive_local_with(
                        Box::pin(std::future::poll_fn(move |_| {
                            if release.load(Ordering::SeqCst) {
                                Poll::Ready(Ok(()))
                            } else {
                                Poll::Pending
                            }
                        })),
                        self.reporter.as_ref(),
                        |_| Err(Error::Io),
                        || Ok(()),
                    )
                }
                fn fence<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    Box::pin(async move {
                        assert!(self.release.load(Ordering::SeqCst));
                        self.fences.fetch_add(1, Ordering::SeqCst);
                        Ok(())
                    })
                }
                fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }
            }
            let cpu = *current_cpus().unwrap().first().unwrap();
            let factory = Arc::new(PendingFactory {
                release: Arc::default(),
                fences: Arc::default(),
            });
            let mut group = Group::new(Plan {
                lanes: vec![Lane {
                    name: "pending-drain".into(),
                    cpu,
                }],
                helpers: Vec::new(),
                max_threads: 2,
            });
            let scope =
                RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(3))
                    .unwrap();
            group.start(factory.clone(), &scope).unwrap();
            let started = Instant::now();
            let result = group.drain(&scope);
            let elapsed = started.elapsed();
            let before = group.stats();
            let fences_before = factory.fences.load(Ordering::SeqCst);
            // Release before assertions so a failed regression cannot hang Drop.
            factory.release.store(true, Ordering::SeqCst);
            let joined = group.join();
            assert_eq!(result, Err(Error::Io));
            assert!(elapsed < Duration::from_secs(1));
            assert_eq!(before.drained, 0);
            assert_eq!(before.done, 0);
            assert_eq!(fences_before, 0);
            assert_eq!(joined, Err(Error::Io));
            assert_eq!(factory.fences.load(Ordering::SeqCst), 2);
            assert_eq!(group.stats().done, 1);
        }

        #[test]
        fn later_pair_allocation_failure_stops_and_joins_started_pair() {
            let (mut group, factory) = fixture(5);
            let events = factory.events.clone();
            let mut second = group.plan.pairs[0].clone();
            second.worker = WorkerId(1);
            group.plan.pairs.push(second);
            let scope = lifecycle_scope().unwrap();
            let result = group.start_with_allocator(
                Arc::new(factory),
                &scope,
                |worker, generation, capacity| {
                    if worker == WorkerId(0) {
                        return security::try_pair(worker, generation, capacity);
                    }
                    // All endpoints are allocated before any owning thread starts.
                    Err(Error::Overloaded)
                },
            );
            assert_eq!(result, Err(Error::Overloaded));
            assert_eq!(group.runtime.stats().coordinators, 0);
            assert_eq!(group.runtime.stats().done, 0);
            let events = events.lock().unwrap();
            assert!(events.is_empty());
        }

        #[test]
        fn scoped_allocation_failure_joins_started_crypto_and_restores_affinity() {
            let (group, factory) = fixture(4);
            let mut plan = AffinityPlan {
                pairs: group.plan.pairs.clone(),
                max_threads: 4,
            };
            let mut second = plan.pairs[0].clone();
            second.worker = WorkerId(1);
            plan.pairs.push(second);
            let mut scoped = WorkerGroup::new(plan);
            let scope = lifecycle_scope().unwrap();
            let before = current_cpus().unwrap();
            let result = scoped.run_with_allocator(
                &factory,
                &scope,
                false,
                |worker, generation, capacity| {
                    if worker == WorkerId(0) {
                        return security::try_pair(worker, generation, capacity);
                    }
                    Err(Error::Overloaded)
                },
            );
            assert_eq!(result, Err(Error::Overloaded));
            assert_eq!(current_cpus().unwrap(), before);
            assert_eq!(scoped.runtime.stats().done, 0);
            assert_eq!(scoped.runtime.stats().coordinators, 0);
            let events = factory.events.lock().unwrap();
            assert!(events.is_empty());
        }
    }
}
