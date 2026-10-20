//! Node-wide warm jobs. Progress means disk publication, not writer queue admission.

use super::dispatch::WorkerDirectory;
use super::flight::AcquisitionBudget;
use crate::error::{Error, Result};
use crate::model::{ObjectMetadata, ObjectVersion, PAGE_BYTES, PageId, PageNumber, RequestId};
use crate::peer::protocol::{FetchMode, Operation as PeerOperation, PeerResponse, WarmAction};
use crate::runtime::RequestScope;
use crate::security::ChargedOriginContext;
use crate::security::OriginContext;
use crate::store::holds::HoldPages;
use crate::telemetry::{Metrics, WarmEvent};
use crate::topology::Membership;
use racer_control_wire::NodeId;
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};
use std::time::{Duration, SystemTime};

#[derive(Clone, Copy, Debug)]
pub struct Config {
    pub max_ttl: Duration,
    pub fill_timeout: Duration,
    pub max_jobs: usize,
    pub window: usize,
    pub retained_percent: usize,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            max_ttl: Duration::from_secs(3600),
            fill_timeout: Duration::from_secs(600),
            max_jobs: 16,
            window: 2,
            retained_percent: 50,
        }
    }
}

impl Config {
    pub fn validate(self) -> Result<()> {
        if self.max_ttl.is_zero()
            || self.max_ttl > Duration::from_secs(86400)
            || self.fill_timeout.is_zero()
            || self.fill_timeout > Duration::from_secs(86400)
            || !(1..=1024).contains(&self.max_jobs)
            || !(1..=64).contains(&self.window)
            || !(1..=100).contains(&self.retained_percent)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

struct Progress {
    runners: BTreeMap<u64, Waker>,
    next_runner: u64,
    revision: u64,
    admissions: usize,
    running: bool,
    until: Option<SystemTime>,
    frontier: u64,
    stopped: bool,
    waiters: BTreeMap<u64, (Waker, std::time::Instant)>,
    sequence: u64,
}

pub(crate) struct Job {
    previous_until: Option<SystemTime>,
    /// Lease id, inherited from the completed job this one reverifies so
    /// repeated warms renew one lease per requester and version.
    lease: [u8; 16],
    metrics: Metrics,
    version: ObjectVersion,
    scope: RequestScope,
    admission: futures::lock::Mutex<()>,
    progress: Mutex<Progress>,
    plan: Mutex<Option<Arc<Plan>>>,
}

/// Primary-owner placement of one warm job, fixed at its first admission.
/// Later membership changes are not re-homed; see the reference docs.
pub(crate) struct Plan {
    node: NodeId,
    membership: Arc<Membership>,
    local: Vec<u64>,
    held: BTreeMap<u16, HoldPages>,
    remote: Vec<NodeId>,
}

impl Plan {
    fn is_local(&self, page: u64) -> bool {
        usize::try_from(page / 64)
            .ok()
            .and_then(|word| self.local.get(word))
            .is_none_or(|word| word >> (page % 64) & 1 == 1)
    }

    fn lease(&self, job: &Job) -> LeaseKey {
        (self.node.clone(), job.lease)
    }
}

pub struct WarmRegistry {
    metrics: std::sync::OnceLock<Metrics>,
    config: Mutex<Config>,
    jobs: Mutex<BTreeMap<ObjectVersion, Arc<Job>>>,
    waiter_limit: usize,
    draining: std::sync::atomic::AtomicBool,
    releases: Mutex<BTreeMap<u16, ReleaseQueue>>,
    /// Live hold leases per version. Local jobs and remote requesters both hold
    /// leases; unused capacity is released only after the last lease is gone.
    leases: Mutex<LeaseState>,
}
/// Lease table plus a generation fence. Every grant stamps its version with a
/// fresh generation; a queued release carries the generation observed when the
/// last lease was dropped and is discarded if a newer grant has since occurred.
#[derive(Default)]
struct LeaseState {
    next: u64,
    versions: BTreeMap<ObjectVersion, (u64, BTreeMap<LeaseKey, SystemTime>)>,
}
/// A requester node and the warm job identifier it chose for one version.
type LeaseKey = (NodeId, [u8; 16]);
/// Live lease entries allowed per configured warm job slot, node-wide.
pub(crate) const LEASES_PER_JOB: usize = 64;
/// Live lease entries allowed for one object version.
pub(crate) const MAX_LEASES_PER_VERSION: usize = 64;

impl LeaseState {
    /// Drop expired leases everywhere. Versions left empty are removed and
    /// returned with the current generation, so the caller can queue the same
    /// fenced release used on explicit drop. A shared hold keeps its longest
    /// deadline, so unused capacity must not wait for the hold to lapse.
    #[must_use]
    fn prune(&mut self, now: SystemTime) -> Vec<(ObjectVersion, u64)> {
        let generation = self.next;
        let mut emptied = Vec::new();
        self.versions.retain(|version, (_, entry)| {
            entry.retain(|_, expiry| *expiry > now);
            if entry.is_empty() {
                emptied.push((version.clone(), generation));
            }
            !entry.is_empty()
        });
        emptied
    }

    fn live(&self) -> usize {
        self.versions.values().map(|(_, entry)| entry.len()).sum()
    }
}
type ReleaseQueue = (Option<Waker>, BTreeMap<ObjectVersion, u64>);

impl WarmRegistry {
    pub(crate) fn new(waiter_limit: usize) -> Self {
        Self {
            metrics: std::sync::OnceLock::new(),
            config: Mutex::new(Config::default()),
            jobs: Mutex::new(BTreeMap::new()),
            waiter_limit,
            draining: std::sync::atomic::AtomicBool::new(false),
            releases: Mutex::new(BTreeMap::new()),
            leases: Mutex::new(LeaseState::default()),
        }
    }

    /// Record or extend a lease before reserving capacity for it, so a racing
    /// release of another lease cannot trim this reservation.
    ///
    /// The same (requester, job) key coalesces into one entry whose expiry only
    /// grows. A new key is refused with `Overloaded` once its version holds
    /// [`MAX_LEASES_PER_VERSION`] live leases or the node holds
    /// [`LEASES_PER_JOB`] times `max_jobs` live leases; expired leases are
    /// pruned first so only live leases count. Refusal never touches existing
    /// leases or their generation stamp.
    pub(crate) fn grant_lease(
        &self,
        version: &ObjectVersion,
        key: &LeaseKey,
        until: SystemTime,
    ) -> Result<()> {
        let now = uring_runtime::environment::wall_now();
        let cap = self.config().max_jobs.saturating_mul(LEASES_PER_JOB);
        let mut emptied = Vec::new();
        let outcome = {
            let mut leases = self.leases.lock().unwrap();
            let known = leases
                .versions
                .get(version)
                .is_some_and(|(_, entry)| entry.get(key).is_some_and(|expiry| *expiry > now));
            if !known {
                emptied = leases.prune(now);
            }
            let per_version = leases.versions.get(version).map_or(0, |(_, e)| e.len());
            if !known && (per_version >= MAX_LEASES_PER_VERSION || leases.live() >= cap) {
                Err(Error::Overloaded)
            } else {
                leases.next += 1;
                let generation = leases.next;
                let (stamp, entry) = leases.versions.entry(version.clone()).or_default();
                *stamp = generation;
                entry.retain(|_, expiry| *expiry > now);
                let expiry = entry.entry(key.clone()).or_insert(until);
                *expiry = (*expiry).max(until);
                Ok(())
            }
        };
        // Queued after the new stamp: a release for this version is fenced off.
        self.release_expired(emptied);
        outcome
    }

    /// Queue fenced releases for versions whose last lease expired, on every
    /// worker that polls releases. A worker that never polled holds nothing.
    fn release_expired(&self, emptied: Vec<(ObjectVersion, u64)>) {
        if emptied.is_empty() {
            return;
        }
        let workers = self
            .releases
            .lock()
            .unwrap()
            .keys()
            .map(|id| crate::model::WorkerId(*id))
            .collect::<Vec<_>>();
        for (version, generation) in emptied {
            self.release(&version, generation, &workers);
        }
    }

    #[cfg(test)]
    pub(crate) fn lease_total(&self) -> usize {
        self.leases.lock().unwrap().live()
    }

    pub(crate) fn has_lease(&self, version: &ObjectVersion, key: &LeaseKey) -> bool {
        let now = uring_runtime::environment::wall_now();
        self.leases
            .lock()
            .unwrap()
            .versions
            .get(version)
            .and_then(|(_, entry)| entry.get(key))
            .is_some_and(|expiry| *expiry > now)
    }

    /// Drop one lease. Unused hold capacity is released on every local worker
    /// once no live lease remains. Returns whether a release was queued.
    pub(crate) fn drop_lease(
        &self,
        version: &ObjectVersion,
        key: &LeaseKey,
        workers: &[crate::model::WorkerId],
    ) -> bool {
        let now = uring_runtime::environment::wall_now();
        let last = {
            let mut leases = self.leases.lock().unwrap();
            let generation = leases.next;
            match leases.versions.get_mut(version) {
                Some((_, entry)) => {
                    entry.remove(key);
                    entry.retain(|_, expiry| *expiry > now);
                    if entry.is_empty() {
                        leases.versions.remove(version);
                        Some(generation)
                    } else {
                        None
                    }
                }
                None => Some(generation),
            }
        };
        if let Some(generation) = last {
            self.release(version, generation, workers);
        }
        last.is_some()
    }

    #[cfg(test)]
    pub(crate) fn lease_count(&self, version: &ObjectVersion) -> usize {
        self.leases
            .lock()
            .unwrap()
            .versions
            .get(version)
            .map_or(0, |(_, entry)| entry.len())
    }

    /// Starts a job whose pages are all held by remote primary owners.
    #[cfg(test)]
    pub(crate) fn begin_remote(
        &self,
        version: &ObjectVersion,
        node: NodeId,
        membership: Arc<Membership>,
        pages: u64,
    ) -> Result<Arc<Job>> {
        let (job, _) = self.begin(version)?;
        *job.plan.lock().unwrap() = Some(Arc::new(Plan {
            node,
            membership,
            local: vec![0; usize::try_from(pages.div_ceil(64)).unwrap()],
            held: BTreeMap::new(),
            remote: Vec::new(),
        }));
        Ok(job)
    }

    /// Publishes the first `frontier` pages of a test job.
    #[cfg(test)]
    pub(crate) fn publish_through(job: &Job, frontier: u64) {
        job.update(|progress| progress.frontier = frontier);
    }

    /// Pages a remote primary owner holds are not stored on this node.
    pub(crate) fn is_local_page(&self, page: &PageId) -> bool {
        let job = self.jobs.lock().unwrap().get(&page.version).cloned();
        job.and_then(|job| job.plan.lock().unwrap().clone())
            .is_none_or(|plan| plan.is_local(page.number.0))
    }

    pub fn configure(&self, config: Config) -> Result<()> {
        config.validate()?;
        if !self.jobs.lock().unwrap().is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        *self.config.lock().unwrap() = config;
        Ok(())
    }

    pub fn config(&self) -> Config {
        *self.config.lock().unwrap()
    }

    pub(super) fn attach_metrics(&self, metrics: &Metrics) {
        self.metrics.get_or_init(|| metrics.clone());
    }

    fn metrics(&self) -> Metrics {
        self.metrics.get().cloned().unwrap_or_default()
    }

    pub(crate) fn contains(&self, version: &ObjectVersion) -> bool {
        self.jobs.lock().unwrap().contains_key(version)
    }

    pub(crate) fn drained(&self, worker: crate::model::WorkerId) -> bool {
        !self.jobs.lock().unwrap().values().any(|job| {
            let p = job.progress.lock().unwrap();
            p.running || p.admissions != 0
        }) && self
            .releases
            .lock()
            .unwrap()
            .get(&worker.0)
            .is_none_or(|(_, versions)| versions.is_empty())
    }

    fn wake_workers(&self) {
        let wake = self
            .releases
            .lock()
            .unwrap()
            .values()
            .filter_map(|(waker, _)| waker.clone())
            .collect::<Vec<_>>();
        for waker in wake {
            waker.wake();
        }
    }

    pub(crate) fn poll_releases(
        &self,
        worker: crate::model::WorkerId,
        index: &crate::store::catalog::Index,
        cx: &mut Context<'_>,
    ) -> Result<()> {
        self.poll_releases_with(worker, cx, |version| index.release_hold(version))
    }

    fn poll_releases_with(
        &self,
        worker: crate::model::WorkerId,
        cx: &mut Context<'_>,
        mut release: impl FnMut(&ObjectVersion) -> Result<()>,
    ) -> Result<()> {
        let versions = {
            let mut releases = self.releases.lock().unwrap();
            let (waker, versions) = releases.entry(worker.0).or_default();
            *waker = Some(cx.waker().clone());
            versions
                .iter()
                .map(|(version, generation)| (version.clone(), *generation))
                .collect::<Vec<_>>()
        };
        for (version, generation) in versions {
            // Check the fence and trim under the lease lock so a grant cannot
            // interleave. Grants precede reservations, so a newer generation
            // means a newer lease owns capacity this release must not trim.
            let outcome = {
                let leases = self.leases.lock().unwrap();
                match leases.versions.get(&version) {
                    Some((stamp, _)) if *stamp > generation => Ok(()),
                    _ => release(&version),
                }
            };
            if let Err(error) = outcome {
                // Retry on the next worker turn, never self-wake or lose the tail.
                return if error == Error::Overloaded {
                    Ok(())
                } else {
                    Err(error)
                };
            }
            // Keep a release queued for a newer generation while this one ran.
            let mut releases = self.releases.lock().unwrap();
            let queue = &mut releases.entry(worker.0).or_default().1;
            if queue.get(&version) == Some(&generation) {
                queue.remove(&version);
            }
        }
        Ok(())
    }

    fn release(
        &self,
        version: &ObjectVersion,
        generation: u64,
        workers: &[crate::model::WorkerId],
    ) {
        let wake = {
            let mut releases = self.releases.lock().unwrap();
            workers
                .iter()
                .filter_map(|worker| {
                    let (waker, versions) = releases.entry(worker.0).or_default();
                    let queued = versions.entry(version.clone()).or_insert(generation);
                    *queued = (*queued).max(generation);
                    waker.clone()
                })
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }

    #[cfg(test)]
    pub(super) fn begin(&self, version: &ObjectVersion) -> Result<(Arc<Job>, bool)> {
        self.begin_selected(version, false)
    }

    pub(super) fn begin_selected(
        &self,
        version: &ObjectVersion,
        selecting: bool,
    ) -> Result<(Arc<Job>, bool)> {
        use std::sync::atomic::Ordering;
        if self.draining.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        let config = self.config();
        let mut jobs = self.jobs.lock().unwrap();
        let mut previous_until = None;
        let mut lease = None;
        jobs.retain(|_, job| {
            let progress = job.progress.lock().unwrap();
            let expired = progress
                .until
                .is_some_and(|until| until <= uring_runtime::environment::wall_now());
            progress.running || progress.admissions != 0 || (!expired && !progress.stopped)
        });
        if let Some(job) = jobs.get(version) {
            let mut progress = job.progress.lock().unwrap();
            if progress.stopped || progress.admissions >= self.waiter_limit {
                return Err(Error::Overloaded);
            }
            if progress.running {
                job.scope.check()?;
                progress.admissions += 1;
                return Ok((job.clone(), false));
            }
            if progress.admissions != 0 {
                return Err(Error::Overloaded);
            }
            previous_until = progress.until;
            lease = Some(job.lease);
        }
        if selecting {
            return Err(Error::Overloaded);
        }
        if (!jobs.contains_key(version) && jobs.len() >= config.max_jobs)
            || self
                .releases
                .lock()
                .unwrap()
                .values()
                .any(|(_, versions)| !versions.is_empty())
        {
            return Err(Error::Overloaded);
        }
        let mut request = [0; 16];
        uring_runtime::environment::fill_random(&mut request).map_err(|_| Error::Unavailable)?;
        let job = Arc::new(Job {
            previous_until,
            lease: lease.unwrap_or(request),
            metrics: self.metrics(),
            version: version.clone(),
            scope: RequestScope::new(
                RequestId(request),
                uring_runtime::environment::now() + config.fill_timeout,
            )?,
            admission: futures::lock::Mutex::new(()),
            progress: Mutex::new(Progress {
                runners: BTreeMap::new(),
                next_runner: 0,
                revision: 0,
                admissions: 1,
                running: true,
                until: None,
                frontier: 0,
                stopped: false,
                waiters: BTreeMap::new(),
                sequence: 0,
            }),
            plan: Mutex::new(None),
        });
        jobs.insert(version.clone(), job.clone());
        Ok((job, true))
    }

    pub(crate) fn stop(&self) {
        self.draining
            .store(true, std::sync::atomic::Ordering::Release);
        let jobs = self
            .jobs
            .lock()
            .unwrap()
            .values()
            .cloned()
            .collect::<Vec<_>>();
        for job in jobs {
            let _ = job.scope.cancel();
        }
    }

    pub(crate) fn poll_deadlines(&self) {
        let now = uring_runtime::environment::now();
        let wall = uring_runtime::environment::wall_now();
        let mut jobs = self.jobs.lock().unwrap();
        let mut cancel = Vec::new();
        let mut wake = Vec::new();
        jobs.retain(|_, job| {
            let mut progress = job.progress.lock().unwrap();
            let expired = progress.until.is_some_and(|until| until <= wall);
            if progress.running
                && ((expired && progress.admissions <= 1) || job.scope.deadline.0 <= now)
            {
                cancel.push(job.clone());
            }
            let remove = !progress.running && progress.admissions == 0 && expired;
            if remove {
                progress.stopped = true;
            }
            wake.extend(
                progress
                    .waiters
                    .values()
                    .filter(|(_, deadline)| *deadline <= now || remove)
                    .map(|(waker, _)| waker.clone()),
            );
            !remove
        });
        drop(jobs);
        // A shared hold keeps its longest deadline, so release unused capacity
        // once the last lease expires instead of waiting for the hold to lapse.
        let emptied = self.leases.lock().unwrap().prune(wall);
        self.release_expired(emptied);
        for job in cancel {
            let _ = job.scope.cancel();
        }
        for waker in wake {
            waker.wake();
        }
    }

    /// Wait even for pages outside the current warm window. A miss permits normal Fill.
    pub(crate) async fn wait(&self, page: &PageId, scope: &RequestScope) -> Result<bool> {
        let job = self.jobs.lock().unwrap().get(&page.version).cloned();
        let Some(job) = job else {
            return Ok(false);
        };
        let mut waiter = Waiter {
            job,
            id: None,
            limit: self.waiter_limit,
            deadline: scope.deadline.0,
        };
        let cancellation = scope.cancellation.subscribe()?;
        let result = std::future::poll_fn(|cx| {
            cancellation.register(cx.waker());
            scope.check()?;
            waiter.poll(cx, page.number.0)
        })
        .await;
        waiter.finish(match result {
            Ok(true) => WarmEvent::WaitCompleted,
            Ok(false) => WarmEvent::WaitFallback,
            Err(_) => WarmEvent::WaitFailed,
        });
        result
    }

    fn remove(&self, job: &Arc<Job>) {
        let mut jobs = self.jobs.lock().unwrap();
        if jobs
            .get(&job.version)
            .is_some_and(|old| Arc::ptr_eq(old, job))
        {
            jobs.remove(&job.version);
        }
    }

    pub fn snapshot(&self) -> (usize, usize) {
        let jobs = self.jobs.lock().unwrap();
        (
            jobs.len(),
            jobs.values()
                .map(|job| job.progress.lock().unwrap().waiters.len())
                .sum(),
        )
    }
}

impl Job {
    fn update(&self, change: impl FnOnce(&mut Progress)) {
        let wake = {
            let mut progress = self.progress.lock().unwrap();
            change(&mut progress);
            progress
                .waiters
                .values()
                .map(|(waker, _)| waker.clone())
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }
    pub(super) fn stop(&self) {
        self.update(|progress| {
            progress.stopped = true;
            progress.running = false;
        });
    }
}

struct Waiter {
    job: Arc<Job>,
    id: Option<u64>,
    limit: usize,
    deadline: std::time::Instant,
}
impl Waiter {
    fn finish(&mut self, event: WarmEvent) {
        if let Some(id) = self.id.take() {
            self.job.progress.lock().unwrap().waiters.remove(&id);
            self.job.metrics.warm_event(event);
        }
    }

    fn poll(&mut self, cx: &mut Context<'_>, number: u64) -> Poll<Result<bool>> {
        let mut progress = self.job.progress.lock().unwrap();
        if progress.stopped
            || (!progress.running
                && progress
                    .until
                    .is_some_and(|until| until <= uring_runtime::environment::wall_now()))
        {
            return Poll::Ready(Ok(false));
        }
        if number < progress.frontier {
            return Poll::Ready(Ok(true));
        }
        let id = match self.id {
            Some(id) => id,
            None => {
                if progress.waiters.len() >= self.limit {
                    return Poll::Ready(Err(Error::Overloaded));
                }
                progress.sequence = progress.sequence.checked_add(1).ok_or(Error::Overloaded)?;
                self.id = Some(progress.sequence);
                self.job.metrics.warm_event(WarmEvent::WaitStarted);
                progress.sequence
            }
        };
        progress
            .waiters
            .insert(id, (cx.waker().clone(), self.deadline));
        Poll::Pending
    }
}
impl Drop for Waiter {
    fn drop(&mut self) {
        self.finish(WarmEvent::WaitFailed);
    }
}

/// A decision survives caller cancellation when moved into the detached driver.
struct Outcome {
    metrics: Metrics,
    event: WarmEvent,
}
impl Drop for Outcome {
    fn drop(&mut self) {
        self.metrics.warm_event(self.event);
    }
}

struct JobGuard {
    directory: Arc<WorkerDirectory>,
    job: Arc<Job>,
    complete: bool,
}
struct Runner {
    job: Arc<Job>,
    id: u64,
}
impl Runner {
    fn new(job: Arc<Job>) -> Result<Self> {
        let mut progress = job.progress.lock().unwrap();
        progress.next_runner = progress
            .next_runner
            .checked_add(1)
            .ok_or(Error::Overloaded)?;
        let id = progress.next_runner;
        drop(progress);
        Ok(Self { job, id })
    }
}
impl Drop for Runner {
    fn drop(&mut self) {
        self.job.progress.lock().unwrap().runners.remove(&self.id);
    }
}
impl Drop for JobGuard {
    fn drop(&mut self) {
        let runners = {
            let mut progress = self.job.progress.lock().unwrap();
            progress.admissions -= 1;
            std::mem::take(&mut progress.runners)
        };
        for runner in runners.into_values() {
            runner.wake();
        }
        if !self.complete {
            let _ = self.job.scope.cancel();
            self.job.stop();
        }
        let progress = self.job.progress.lock().unwrap();
        let remove = progress.stopped && progress.admissions == 0;
        drop(progress);
        if remove {
            self.directory.warm.remove(&self.job);
        }
        self.directory.warm.wake_workers();
    }
}

/// Admission runs under an owned driver so a disconnected HEAD cannot leak a partial hold.
pub(crate) async fn admit(
    directory: Arc<WorkerDirectory>,
    metadata: ObjectMetadata,
    membership: Arc<crate::topology::Membership>,
    context: ChargedOriginContext,
    ttl: Duration,
    caller: &RequestScope,
) -> Result<SystemTime> {
    let mut admission = Outcome {
        metrics: directory.warm.metrics(),
        event: WarmEvent::Refused,
    };
    caller.check()?;
    let config = directory.warm.config();
    if ttl.is_zero() {
        return Err(Error::InvalidRequest);
    }
    let until = uring_runtime::environment::wall_now()
        .checked_add(ttl.min(config.max_ttl))
        .ok_or(Error::InvalidRequest)?;
    let permit = uring_runtime::drivers::reserve()?;
    let (job, fresh) = directory
        .subscriptions
        .admit_warm(&metadata.version, &directory.warm)?;
    let (send, receive) = futures::channel::oneshot::channel();
    permit.submit_detached(Box::pin(async move {
        let mut guard = JobGuard {
            directory: directory.clone(),
            job: job.clone(),
            complete: !fresh,
        };
        // The first caller owns startup, even if another worker polls its
        // coalesced admission first. Do not extend a reservation not yet admitted.
        if !fresh {
            let runner = Runner::new(job.clone())?;
            let cancellation = job.scope.cancellation.subscribe()?;
            std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                job.scope.check()?;
                let mut progress = job.progress.lock().unwrap();
                if progress.stopped {
                    return Poll::Ready(Err(Error::Unavailable));
                }
                if progress.until.is_some() {
                    return Poll::Ready(Ok(()));
                }
                progress.runners.insert(runner.id, cx.waker().clone());
                Poll::Pending
            })
            .await?;
        }
        let lock = job.admission.lock().await;
        if job.progress.lock().unwrap().stopped {
            let _ = send.send(Err(Error::Unavailable));
            return Ok::<(), Error>(());
        }
        let old = job.progress.lock().unwrap().until.or(job.previous_until);
        let target = old.map_or(until, |old| old.max(until));
        let pages = metadata.length.div_ceil(PAGE_BYTES);
        // Every reservation pass can cross expiry on an untouched owner. Hide
        // the old frontier before yielding and reverify all pages afterward.
        let revision = {
            let mut progress = job.progress.lock().unwrap();
            progress.revision = progress.revision.checked_add(1).ok_or(Error::Overloaded)?;
            progress.frontier = 0;
            progress.revision
        };
        let reservation_scope = if fresh {
            job.scope.clone()
        } else {
            RequestScope::new(
                job.scope.request,
                uring_runtime::environment::now() + config.fill_timeout,
            )?
        };
        let existing = job.plan.lock().unwrap().clone();
        let plan = match existing {
            Some(plan) => Ok(plan),
            None => plan(
                &directory,
                &metadata.version,
                pages,
                membership,
                &reservation_scope,
            )
            .await
            .map(|plan| {
                let plan = Arc::new(plan);
                *job.plan.lock().unwrap() = Some(plan.clone());
                plan
            }),
        };
        let mut fresh_workers = Vec::new();
        let admitted = match &plan {
            Ok(plan) => {
                reserve(
                    &directory,
                    &job,
                    plan,
                    &context,
                    &metadata.version,
                    pages,
                    target,
                    &reservation_scope,
                    &mut fresh_workers,
                )
                .await
            }
            Err(error) => Err(*error),
        }
        .and_then(|accepted| {
            job.scope.check()?;
            if directory
                .warm
                .draining
                .load(std::sync::atomic::Ordering::Acquire)
            {
                Err(Error::Cancelled)
            } else {
                Ok(accepted)
            }
        });
        let target = match admitted {
            Ok(target) => target,
            Err(error) => {
                if fresh {
                    if let Ok(plan) = &plan {
                        release_all(&directory, &job, plan, &context, &metadata.version, config)
                            .await;
                    }
                    job.stop();
                }
                let _ = send.send(Err(error));
                return Ok::<(), Error>(());
            }
        };
        let plan = plan?;
        job.update(|progress| progress.until = Some(target));
        let runners = std::mem::take(&mut job.progress.lock().unwrap().runners);
        for runner in runners.into_values() {
            runner.wake();
        }
        admission.event = WarmEvent::Accepted;
        drop(admission);
        drop(lock);
        let _ = send.send(Ok(target));
        if !fresh {
            return Ok(());
        }
        let mut outcome = Outcome {
            metrics: job.metrics.clone(),
            event: WarmEvent::Failed,
        };
        let result = run(
            &directory,
            &job,
            &plan,
            &metadata,
            &context,
            config.window,
            revision,
        )
        .await;
        if result.is_err() {
            let _lock = job.admission.lock().await;
            release_all(&directory, &job, &plan, &context, &metadata.version, config).await;
            job.stop();
        } else {
            guard.complete = true;
            outcome.event = WarmEvent::Completed;
        }
        Ok(())
    }));
    let mut receive = Box::pin(receive);
    let cancellation = caller.cancellation.subscribe()?;
    std::future::poll_fn(|cx| {
        cancellation.register(cx.waker());
        caller.check()?;
        use std::future::Future;
        receive.as_mut().poll(cx).map(|result| {
            result
                .map_err(|_| Error::Unavailable)
                .and_then(|result| result)
        })
    })
    .await
}

async fn run(
    directory: &Arc<WorkerDirectory>,
    job: &Arc<Job>,
    plan: &Arc<Plan>,
    metadata: &ObjectMetadata,
    context: &ChargedOriginContext,
    window: usize,
    mut revision: u64,
) -> Result<()> {
    use futures::{StreamExt, stream::FuturesOrdered};
    let count = metadata.length.div_ceil(PAGE_BYTES);
    let mut pending = FuturesOrdered::new();
    let mut next = 0;
    let mut failure = None;
    loop {
        while failure.is_none() && next < count && pending.len() < window {
            if let Err(error) = job.scope.check() {
                failure = Some(error);
                break;
            }
            if job
                .progress
                .lock()
                .unwrap()
                .until
                .is_none_or(|until| until <= uring_runtime::environment::wall_now())
            {
                let _lock = job.admission.lock().await;
                if job
                    .progress
                    .lock()
                    .unwrap()
                    .until
                    .is_none_or(|until| until <= uring_runtime::environment::wall_now())
                {
                    failure = Some(Error::DeadlineExceeded);
                    break;
                }
            }
            let page = PageId {
                version: metadata.version.clone(),
                number: PageNumber(next),
            };
            pending.push_back(warm_one(
                directory,
                job,
                plan,
                page,
                metadata.immutable(),
                context,
            ));
            next += 1;
        }
        if failure.is_some() {
            let _ = job.scope.cancel();
        }
        let Some(result) = pending.next().await else {
            if let Some(error) = failure {
                return Err(error);
            }
            let runner = Runner::new(job.clone())?;
            std::future::poll_fn(|cx| {
                let mut progress = job.progress.lock().unwrap();
                if progress.admissions <= 1 {
                    Poll::Ready(())
                } else {
                    progress.runners.insert(runner.id, cx.waker().clone());
                    Poll::Pending
                }
            })
            .await;
            let _lock = job.admission.lock().await;
            let mut progress = job.progress.lock().unwrap();
            if progress.admissions > 1 {
                continue;
            }
            if progress.revision != revision {
                revision = progress.revision;
                progress.frontier = 0;
                next = 0;
                continue;
            }
            progress.running = false;
            return Ok(());
        };
        match result {
            Ok(()) if failure.is_none() => job.update(|progress| {
                if progress.revision == revision {
                    progress.frontier += 1;
                }
            }),
            Err(error) => {
                failure.get_or_insert(error);
            }
            _ => {}
        }
    }
}

/// Attempts per remote warm page before the job fails. Retries are immediate
/// and stay inside the job's fill timeout; only transient refusals retry.
const REMOTE_FILL_ATTEMPTS: usize = 3;

/// Bound for best-effort remote releases after a job fails or is refused.
const RELEASE_TIMEOUT: Duration = Duration::from_secs(5);

/// Map each page to its primary owner under the admission membership. Pages
/// whose primary is this node (or that have no candidates) are held locally.
async fn plan(
    directory: &Arc<WorkerDirectory>,
    version: &ObjectVersion,
    pages: u64,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<Plan> {
    if pages > directory.max_warm_pages() {
        return Err(Error::Overloaded);
    }
    let local = directory.local()?;
    let candidates = local.fill.dependencies.candidates.clone();
    drop(local);
    let node = candidates.node.clone();
    let words = usize::try_from(pages.div_ceil(64)).map_err(|_| Error::Overloaded)?;
    let mut plan = Plan {
        node,
        membership: membership.clone(),
        local: vec![0; words],
        held: BTreeMap::new(),
        remote: Vec::new(),
    };
    let mut remote = std::collections::BTreeSet::new();
    for number in 0..pages {
        scope.check()?;
        let ranked = candidates
            .candidates_scoped(
                membership.clone(),
                &version.object,
                PageNumber(number),
                scope,
            )
            .await?;
        match ranked.ordered.first() {
            Some(primary) if primary != &plan.node => {
                remote.insert(primary.clone());
            }
            _ => {
                plan.local[(number / 64) as usize] |= 1 << (number % 64);
                let owner = directory.page_owner(&PageId {
                    version: version.clone(),
                    number: PageNumber(number),
                })?;
                plan.held
                    .entry(owner.0)
                    .or_default()
                    .insert(PageNumber(number));
            }
        }
        if number % 1024 == 1023 {
            uring_runtime::drivers::yield_now().await;
        }
    }
    plan.remote = remote.into_iter().collect();
    Ok(plan)
}

/// All-or-nothing hold admission: local primaries reserve through the worker
/// mailboxes and every remote primary owner must accept a `Hold`. The result is
/// the earliest expiry any owner promised, so every page is held until then.
#[allow(clippy::too_many_arguments)] // Explicit admission inputs, no parameter shim.
async fn reserve(
    directory: &Arc<WorkerDirectory>,
    job: &Job,
    plan: &Plan,
    context: &OriginContext,
    version: &ObjectVersion,
    pages: u64,
    target: SystemTime,
    scope: &RequestScope,
    fresh: &mut Vec<crate::model::WorkerId>,
) -> Result<SystemTime> {
    let lease = plan.lease(job);
    directory.warm.grant_lease(version, &lease, target)?;
    let mut accepted = directory
        .reserve_warm(version, &plan.held, target, scope, fresh)
        .await?;
    // Local holds may inherit a later deadline from an existing hold. Cover it
    // so expiry pruning cannot release unfilled capacity before the promised
    // RetainUntil, whether it is reported alone or as the earliest owner.
    if accepted > target {
        directory.warm.grant_lease(version, &lease, accepted)?;
    }
    if pages == 0 || plan.remote.is_empty() {
        return Ok(accepted);
    }
    let action = WarmAction::Hold {
        pages,
        until: target,
    };
    let replies = futures::future::join_all(plan.remote.iter().map(|owner| {
        remote(
            directory,
            plan,
            owner,
            context,
            version,
            lease.1,
            action.clone(),
            scope,
        )
    }))
    .await;
    let mut failure = None;
    for reply in replies {
        match reply {
            Ok(Some(until)) => {
                job.metrics.warm_event(WarmEvent::RemoteHoldAccepted);
                accepted = accepted.min(until);
            }
            Ok(None) => {
                job.metrics.warm_event(WarmEvent::RemoteHoldRefused);
                failure.get_or_insert(Error::CorruptRecord);
            }
            Err(error) => {
                job.metrics.warm_event(WarmEvent::RemoteHoldRefused);
                failure.get_or_insert(error);
            }
        }
    }
    match failure {
        Some(error) => Err(error),
        None => Ok(accepted),
    }
}

/// Release the job's local lease and, best effort, every remote owner's lease.
/// Unreachable owners keep their holds until expiry.
async fn release_all(
    directory: &Arc<WorkerDirectory>,
    job: &Job,
    plan: &Plan,
    context: &OriginContext,
    version: &ObjectVersion,
    config: Config,
) {
    let lease = plan.lease(job);
    directory
        .warm
        .drop_lease(version, &lease, &directory.workers());
    if plan.remote.is_empty() {
        return;
    }
    let Ok(scope) = RequestScope::new(
        job.scope.request,
        uring_runtime::environment::now() + RELEASE_TIMEOUT.min(config.fill_timeout),
    ) else {
        return;
    };
    let replies = futures::future::join_all(plan.remote.iter().map(|owner| {
        remote(
            directory,
            plan,
            owner,
            context,
            version,
            lease.1,
            WarmAction::Release,
            &scope,
        )
    }))
    .await;
    for reply in replies {
        job.metrics.warm_event(if reply.is_ok() {
            WarmEvent::RemoteReleaseSent
        } else {
            WarmEvent::RemoteReleaseFailed
        });
    }
}

/// Warm one page: local primaries publish to this node's disk; remote
/// primaries fill under their hold and acknowledge without a payload.
async fn warm_one(
    directory: &Arc<WorkerDirectory>,
    job: &Job,
    plan: &Plan,
    page: PageId,
    metadata: crate::model::VersionMetadata,
    context: &ChargedOriginContext,
) -> Result<()> {
    if plan.is_local(page.number.0) {
        return directory
            .warm_page(page, metadata, plan.membership.clone(), context, &job.scope)
            .await;
    }
    let candidates = directory.local()?.fill.dependencies.candidates.clone();
    let primary = candidates
        .candidates_scoped(
            plan.membership.clone(),
            &page.version.object,
            page.number,
            &job.scope,
        )
        .await?
        .ordered
        .first()
        .cloned()
        .ok_or(Error::IncompatibleMembership)?;
    retry_remote_fill(&job.scope, &job.metrics, || {
        remote(
            directory,
            plan,
            &primary,
            context,
            &page.version,
            job.lease,
            WarmAction::Fill { page: page.number },
            &job.scope,
        )
    })
    .await
}

/// Whether a remote fill error is transient and worth another bounded attempt.
///
/// Matches ordinary candidate acquisition, which treats socket failures
/// (`Error::Os`) like `Unavailable` and `Io`.
fn remote_fill_retryable(error: &Error) -> bool {
    matches!(
        error,
        Error::Overloaded | Error::Unavailable | Error::Io | Error::Os(_)
    )
}

/// Run `attempt` up to `REMOTE_FILL_ATTEMPTS` times while it fails transiently.
async fn retry_remote_fill<T, F, Fut>(
    scope: &RequestScope,
    metrics: &Metrics,
    mut attempt: F,
) -> Result<()>
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = Result<T>>,
{
    let mut attempts = 0;
    loop {
        attempts += 1;
        scope.check()?;
        match attempt().await {
            Ok(_) => {
                metrics.warm_event(WarmEvent::RemoteFillCompleted);
                return Ok(());
            }
            Err(error)
                if remote_fill_retryable(&error)
                    && attempts < REMOTE_FILL_ATTEMPTS
                    && scope.check().is_ok() =>
            {
                metrics.warm_event(WarmEvent::RemoteFillRetried);
            }
            Err(error) => {
                metrics.warm_event(WarmEvent::RemoteFillFailed);
                return Err(error);
            }
        }
    }
}

/// Send one warm action directly to `owner` and map its acknowledgement.
#[allow(clippy::too_many_arguments)] // Explicit route and lease inputs.
async fn remote(
    directory: &Arc<WorkerDirectory>,
    plan: &Plan,
    owner: &NodeId,
    context: &OriginContext,
    version: &ObjectVersion,
    lease: [u8; 16],
    action: WarmAction,
    scope: &RequestScope,
) -> Result<Option<SystemTime>> {
    let candidates = directory.local()?.fill.dependencies.candidates.clone();
    let mut budget = super::default_budget(scope);
    let expected = action.clone();
    let response = candidates
        .request(
            &plan.membership,
            owner,
            context,
            &PeerOperation::Warm {
                version: version.clone(),
                lease,
                action,
            },
            FetchMode::Acquire,
            scope,
            &mut budget,
            1,
        )
        .await?;
    match (response.response(), expected) {
        (PeerResponse::Held { until }, WarmAction::Hold { .. }) => Ok(Some(*until)),
        (PeerResponse::Warmed, WarmAction::Fill { .. })
        | (PeerResponse::Released, WarmAction::Release) => Ok(None),
        (PeerResponse::Overloaded, _) => Err(Error::Overloaded),
        (PeerResponse::Unavailable | PeerResponse::Miss, _) => Err(Error::Unavailable),
        (PeerResponse::StaleMembership, _) => Err(Error::IncompatibleMembership),
        (PeerResponse::VersionUnavailable, _) => Err(Error::VersionUnavailable),
        (PeerResponse::NotFound, _) => Err(Error::NotFound),
        (PeerResponse::OriginRejected, _) => Err(Error::OriginRejected),
        (PeerResponse::OriginForbidden, _) => Err(Error::OriginForbidden),
        _ => Err(Error::CorruptRecord),
    }
}

/// The authenticated requester's lease on one object version.
pub(crate) struct Lease {
    pub requester: NodeId,
    pub version: ObjectVersion,
    pub lease: [u8; 16],
}

/// Owner side of the warm protocol. Holds are accepted only for pages this
/// node is the primary owner of under the request's membership.
#[allow(clippy::too_many_arguments)] // Explicit peer inputs, no parameter shim.
pub(crate) async fn serve(
    local: &super::Coordinator,
    lease: Lease,
    action: WarmAction,
    membership: Arc<Membership>,
    context: &OriginContext,
    scope: &RequestScope,
    budget: &mut AcquisitionBudget,
) -> Result<PeerResponse> {
    let directory = local.streams.directory().clone();
    let metrics = local.fill.metrics.clone();
    if directory
        .warm
        .draining
        .load(std::sync::atomic::Ordering::Acquire)
    {
        return Err(Error::Unavailable);
    }
    let candidates = local.fill.dependencies.candidates.clone();
    let node = candidates.node.clone();
    let Lease {
        requester,
        version,
        lease,
    } = lease;
    let key = (requester, lease);
    match action {
        WarmAction::Hold { pages, until } => {
            let now = uring_runtime::environment::wall_now();
            let until = until.min(
                now.checked_add(directory.warm.config().max_ttl)
                    .ok_or(Error::InvalidRequest)?,
            );
            // Report whole milliseconds, never later than the wire request.
            let until = std::time::UNIX_EPOCH
                + Duration::from_millis(crate::peer::protocol::millis(until)?);
            if until <= now || pages > directory.max_warm_pages() {
                metrics.warm_event(WarmEvent::OwnerHoldRefused);
                return Err(Error::Overloaded);
            }
            let mut held = BTreeMap::new();
            for number in 0..pages {
                scope.check()?;
                let ranked = candidates
                    .candidates_scoped(
                        membership.clone(),
                        &version.object,
                        PageNumber(number),
                        scope,
                    )
                    .await?;
                if ranked.ordered.first() == Some(&node) {
                    let owner = directory.page_owner(&PageId {
                        version: version.clone(),
                        number: PageNumber(number),
                    })?;
                    held.entry(owner.0)
                        .or_insert_with(HoldPages::default)
                        .insert(PageNumber(number));
                }
                if number % 1024 == 1023 {
                    uring_runtime::drivers::yield_now().await;
                }
            }
            if held.is_empty() {
                metrics.warm_event(WarmEvent::OwnerHoldRefused);
                return Ok(PeerResponse::StaleMembership);
            }
            let renewal = directory.warm.has_lease(&version, &key);
            if let Err(error) = directory.warm.grant_lease(&version, &key, until) {
                metrics.warm_event(WarmEvent::OwnerHoldRefused);
                return Err(error);
            }
            let mut fresh = Vec::new();
            match directory
                .reserve_warm(&version, &held, until, scope, &mut fresh)
                .await
            {
                Ok(_) => {
                    metrics.warm_event(WarmEvent::OwnerHoldAccepted);
                    Ok(PeerResponse::Held { until })
                }
                Err(error) => {
                    metrics.warm_event(WarmEvent::OwnerHoldRefused);
                    // A failed renewal keeps the lease it already had.
                    if !renewal {
                        directory
                            .warm
                            .drop_lease(&version, &key, &directory.workers());
                    }
                    Err(error)
                }
            }
        }
        WarmAction::Fill { page } => {
            let page = PageId {
                version,
                number: page,
            };
            if !directory.warm.has_lease(&page.version, &key) {
                metrics.warm_event(WarmEvent::OwnerFillFailed);
                return Ok(PeerResponse::Unavailable);
            }
            let ranked = candidates
                .candidates_scoped(membership.clone(), &page.version.object, page.number, scope)
                .await?;
            if ranked.ordered.first() != Some(&node) {
                metrics.warm_event(WarmEvent::OwnerFillFailed);
                return Ok(PeerResponse::StaleMembership);
            }
            let result = async {
                let acquired = local
                    .fill
                    .acquire_warm(page, membership, context, scope, budget)
                    .await?;
                local
                    .fill
                    .dependencies
                    .writer
                    .persist_held(acquired.copy(), scope)
                    .await
            }
            .await;
            match result {
                Ok(()) => {
                    metrics.warm_event(WarmEvent::OwnerFillCompleted);
                    Ok(PeerResponse::Warmed)
                }
                Err(error) => {
                    metrics.warm_event(WarmEvent::OwnerFillFailed);
                    Err(error)
                }
            }
        }
        WarmAction::Release => {
            directory
                .warm
                .drop_lease(&version, &key, &directory.workers());
            metrics.warm_event(WarmEvent::OwnerReleased);
            Ok(PeerResponse::Released)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::telemetry::Event;
    use std::future::Future;

    #[test]
    fn admission_and_job_metrics_survive_caller_drop_and_count_coalescing_once() {
        let queue = std::rc::Rc::new(uring_runtime::drivers::DriverQueue::new(64));
        let _queue = queue.enter();
        let (local, _fixture) = crate::read::tests::admission_coordinator();
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(
                    super::super::dispatch::WorkerMap::new(vec![crate::model::WorkerId(0)])
                        .unwrap(),
                ),
                vec![crate::model::WorkerId(0)],
                8,
            )
            .unwrap(),
        );
        let metrics = Metrics::default();
        directory.warm.attach_metrics(&metrics);
        let mut endpoint = directory
            .install(crate::model::WorkerId(0), local.clone())
            .unwrap();
        let caller = scope();
        let origin = crate::security::OriginContext {
            object: version("metrics").object,
            metadata: None,
            authorization: None,
        };
        let membership = Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        let metadata = ObjectMetadata {
            version: version("metrics"),
            length: 0,
            content_type: None,
            expires_at: crate::model::ExpiresAt::from_system_time(SystemTime::UNIX_EPOCH).unwrap(),
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut admission = Box::pin(admit(
            directory.clone(),
            metadata.clone(),
            membership.clone(),
            local.credentials.local_context(&origin, &caller).unwrap(),
            Duration::from_secs(60),
            &caller,
        ));
        assert!(admission.as_mut().poll(&mut cx).is_pending());
        drop(admission);
        caller.cancel().unwrap();
        for _ in 0..32 {
            queue.poll(&mut cx, 64);
            endpoint.poll(&mut cx, 64).unwrap();
            if queue.pending() == 0 {
                break;
            }
        }
        assert_eq!(queue.pending(), 0);
        assert_eq!(metrics.count(Event::WarmAccepted), 1);
        assert_eq!(metrics.count(Event::WarmCompleted), 1);
        assert_eq!(metrics.count(Event::WarmRefused), 0);
        let caller = scope();
        for ttl in [Duration::from_secs(60), Duration::ZERO] {
            let mut admission = Box::pin(admit(
                directory.clone(),
                metadata.clone(),
                membership.clone(),
                local.credentials.local_context(&origin, &caller).unwrap(),
                ttl,
                &caller,
            ));
            let mut result = None;
            for _ in 0..32 {
                if let Poll::Ready(value) = admission.as_mut().poll(&mut cx) {
                    result = Some(value);
                    break;
                }
                queue.poll(&mut cx, 64);
                endpoint.poll(&mut cx, 64).unwrap();
            }
            assert_eq!(result.unwrap().is_ok(), !ttl.is_zero());
        }
        assert_eq!(metrics.count(Event::WarmAccepted), 2);
        assert_eq!(
            metrics.count(Event::WarmCompleted),
            2,
            "completed HEAD starts one new verification driver"
        );
        assert_eq!(metrics.count(Event::WarmRefused), 1);
        assert_eq!(metrics.count(Event::WarmFailed), 0);

        let mut refused = Box::pin(admit(
            directory.clone(),
            ObjectMetadata {
                version: version("too-large"),
                length: 100 * PAGE_BYTES,
                ..metadata.clone()
            },
            membership.clone(),
            local.credentials.local_context(&origin, &caller).unwrap(),
            Duration::from_secs(60),
            &caller,
        ));
        let mut result = None;
        for _ in 0..32 {
            if let Poll::Ready(value) = refused.as_mut().poll(&mut cx) {
                result = Some(value);
                break;
            }
            queue.poll(&mut cx, 64);
            endpoint.poll(&mut cx, 64).unwrap();
        }
        assert_eq!(result.unwrap(), Err(Error::Overloaded));
        drop(refused);
        endpoint.poll(&mut cx, 64).unwrap();
        assert_eq!(metrics.count(Event::WarmRefused), 2);
        assert_eq!(
            metrics.count(Event::WarmFailed),
            0,
            "unadmitted jobs are refusals"
        );

        let metadata = ObjectMetadata {
            version: version("failed"),
            length: 3,
            ..metadata
        };
        let mut admission = Box::pin(admit(
            directory.clone(),
            metadata,
            membership,
            local.credentials.local_context(&origin, &caller).unwrap(),
            Duration::from_secs(60),
            &caller,
        ));
        assert!(admission.as_mut().poll(&mut cx).is_pending());
        queue.poll(&mut cx, 64);
        endpoint.poll(&mut cx, 64).unwrap();
        queue.poll(&mut cx, 64);
        assert!(matches!(
            admission.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(admission);
        directory.warm.stop();
        for _ in 0..32 {
            endpoint.poll(&mut cx, 64).unwrap();
            queue.poll(&mut cx, 64);
            if queue.pending() == 0 {
                break;
            }
        }
        assert_eq!(queue.pending(), 0);
        assert_eq!(metrics.count(Event::WarmAccepted), 3);
        assert_eq!(metrics.count(Event::WarmCompleted), 2);
        assert_eq!(metrics.count(Event::WarmFailed), 1);
        assert_eq!(metrics.count(Event::WarmRefused), 2);
    }

    #[test]
    fn completed_jobs_renew_one_lease_across_many_heads() {
        let queue = std::rc::Rc::new(uring_runtime::drivers::DriverQueue::new(64));
        let _queue = queue.enter();
        let (local, _fixture) = crate::read::tests::admission_coordinator();
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(
                    super::super::dispatch::WorkerMap::new(vec![crate::model::WorkerId(0)])
                        .unwrap(),
                ),
                vec![crate::model::WorkerId(0)],
                8,
            )
            .unwrap(),
        );
        let mut endpoint = directory
            .install(crate::model::WorkerId(0), local.clone())
            .unwrap();
        let origin = crate::security::OriginContext {
            object: version("renew").object,
            metadata: None,
            authorization: None,
        };
        let membership = Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        let metadata = ObjectMetadata {
            version: version("renew"),
            length: 0,
            content_type: None,
            expires_at: crate::model::ExpiresAt::from_system_time(SystemTime::UNIX_EPOCH).unwrap(),
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut lease = None;
        for _ in 0..MAX_LEASES_PER_VERSION + 6 {
            let caller = scope();
            let mut admission = Box::pin(admit(
                directory.clone(),
                metadata.clone(),
                membership.clone(),
                local.credentials.local_context(&origin, &caller).unwrap(),
                Duration::from_secs(60),
                &caller,
            ));
            let mut result = None;
            for _ in 0..32 {
                if let Poll::Ready(value) = admission.as_mut().poll(&mut cx) {
                    result = Some(value);
                    break;
                }
                queue.poll(&mut cx, 64);
                endpoint.poll(&mut cx, 64).unwrap();
            }
            let until = result.unwrap().unwrap();
            assert!(until > SystemTime::UNIX_EPOCH);
            drop(admission);
            for _ in 0..32 {
                queue.poll(&mut cx, 64);
                endpoint.poll(&mut cx, 64).unwrap();
                if queue.pending() == 0 {
                    break;
                }
            }
            assert_eq!(queue.pending(), 0);
            assert_eq!(directory.warm.lease_count(&metadata.version), 1);
            assert_eq!(directory.warm.lease_total(), 1);
            let current = directory.warm.jobs.lock().unwrap()[&metadata.version].lease;
            assert_eq!(
                *lease.get_or_insert(current),
                current,
                "lease id is preserved"
            );
        }
    }

    #[test]
    fn remote_fill_retries_socket_errors_then_succeeds() {
        let metrics = Metrics::default();
        let scope = scope();
        let calls = std::cell::Cell::new(0);
        let result = futures::executor::block_on(retry_remote_fill(&scope, &metrics, || {
            calls.set(calls.get() + 1);
            let outcome = if calls.get() == 1 {
                Err(Error::Os(104)) // ECONNRESET
            } else {
                Ok(())
            };
            std::future::ready(outcome)
        }));
        assert_eq!(result, Ok(()));
        assert_eq!(calls.get(), 2);
        assert_eq!(metrics.count(Event::WarmRemoteFillRetried), 1);
        assert_eq!(metrics.count(Event::WarmRemoteFillFailed), 0);
    }

    #[test]
    fn remote_fill_fails_after_bounded_socket_errors() {
        let metrics = Metrics::default();
        let scope = scope();
        let calls = std::cell::Cell::new(0);
        let result = futures::executor::block_on(retry_remote_fill(&scope, &metrics, || {
            calls.set(calls.get() + 1);
            std::future::ready(Err::<(), _>(Error::Os(111))) // ECONNREFUSED
        }));
        assert_eq!(result, Err(Error::Os(111)));
        assert_eq!(calls.get(), REMOTE_FILL_ATTEMPTS);
        assert_eq!(
            metrics.count(Event::WarmRemoteFillRetried),
            (REMOTE_FILL_ATTEMPTS - 1) as u64
        );
        assert_eq!(metrics.count(Event::WarmRemoteFillFailed), 1);
    }

    #[test]
    fn remote_fill_does_not_retry_permanent_errors() {
        let metrics = Metrics::default();
        let scope = scope();
        let calls = std::cell::Cell::new(0);
        let result = futures::executor::block_on(retry_remote_fill(&scope, &metrics, || {
            calls.set(calls.get() + 1);
            std::future::ready(Err::<(), _>(Error::VersionUnavailable))
        }));
        assert_eq!(result, Err(Error::VersionUnavailable));
        assert_eq!(calls.get(), 1);
    }

    #[test]
    fn wait_metrics_count_registration_and_one_terminal_outcome() {
        let registry = WarmRegistry::new(1);
        let metrics = Metrics::default();
        registry.attach_metrics(&metrics);
        let page = PageId {
            version: version("metrics"),
            number: PageNumber(0),
        };
        let (job, _) = registry.begin(&page.version).unwrap();
        let scope = scope();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut waiting = Box::pin(registry.wait(&page, &scope));
        for _ in 0..3 {
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(metrics.count(Event::WarmWaitStarted), 1);
        assert_eq!(
            futures::executor::block_on(registry.wait(&page, &scope)),
            Err(Error::Overloaded)
        );
        job.update(|p| p.frontier = 1);
        assert_eq!(waiting.as_mut().poll(&mut cx), Poll::Ready(Ok(true)));
        drop(waiting);
        assert_eq!(metrics.count(Event::WarmWaitCompleted), 1);
        assert_eq!(metrics.count(Event::WarmWaitFailed), 0);
        assert_eq!(
            futures::executor::block_on(registry.wait(&page, &scope)),
            Ok(true)
        );
        let page = PageId {
            number: PageNumber(1),
            ..page
        };
        let mut waiting = Box::pin(registry.wait(&page, &scope));
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        drop(waiting);
        assert_eq!(metrics.count(Event::WarmWaitFailed), 1);
        let canceled = self::scope();
        let mut waiting = Box::pin(registry.wait(&page, &canceled));
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        canceled.cancel().unwrap();
        assert_eq!(
            waiting.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        drop(waiting);
        assert_eq!(metrics.count(Event::WarmWaitFailed), 2);
        let mut waiting = Box::pin(registry.wait(&page, &scope));
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        job.stop();
        assert_eq!(waiting.as_mut().poll(&mut cx), Poll::Ready(Ok(false)));
        drop(waiting);
        assert_eq!(metrics.count(Event::WarmWaitFallback), 1);
        assert_eq!(
            futures::executor::block_on(registry.wait(&page, &scope)),
            Ok(false)
        );
        registry.remove(&job);
        assert_eq!(
            futures::executor::block_on(registry.wait(&page, &scope)),
            Ok(false)
        );
        assert_eq!(metrics.count(Event::WarmWaitStarted), 4);
        assert_eq!(metrics.count(Event::WarmWaitFailed), 2);
    }

    fn version(etag: &str) -> ObjectVersion {
        ObjectVersion {
            object: crate::model::ObjectId {
                cache: racer_control_wire::CacheId(crate::test_support::security::CACHE.into()),
                key: crate::model::CacheKey([31; 32]),
            },
            etag: crate::model::StrongEtag::test_value(etag),
        }
    }
    fn scope() -> RequestScope {
        RequestScope::new(
            RequestId([7; 16]),
            uring_runtime::environment::now() + Duration::from_secs(30),
        )
        .unwrap()
    }

    #[test]
    fn retryable_release_preserves_the_entire_queue_without_self_waking() {
        let registry = WarmRegistry::new(4);
        let worker = crate::model::WorkerId(0);
        registry.release(&version("a"), 0, &[worker]);
        registry.release(&version("b"), 0, &[worker]);
        let wakes = Arc::new(crate::test_support::WakeCounter::default());
        let waker = Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        let mut calls = 0;
        registry
            .poll_releases_with(worker, &mut cx, |_| {
                calls += 1;
                Err(Error::Overloaded)
            })
            .unwrap();
        assert_eq!(calls, 1);
        assert_eq!(wakes.count(), 0);
        assert!(!registry.drained(worker));
        let mut released = Vec::new();
        registry
            .poll_releases_with(worker, &mut cx, |version| {
                released.push(version.clone());
                Ok(())
            })
            .unwrap();
        assert_eq!(released, vec![version("a"), version("b")]);
        assert!(registry.drained(worker));
    }

    fn release_all(registry: &WarmRegistry, worker: crate::model::WorkerId) -> Vec<ObjectVersion> {
        let waker = Waker::from(Arc::new(crate::test_support::WakeCounter::default()));
        let mut cx = Context::from_waker(&waker);
        let mut released = Vec::new();
        registry
            .poll_releases_with(worker, &mut cx, |version| {
                released.push(version.clone());
                Ok(())
            })
            .unwrap();
        released
    }

    #[test]
    fn repeated_grant_for_same_lease_coalesces_without_shortening() {
        let registry = WarmRegistry::new(4);
        let v = version("a");
        let now = uring_runtime::environment::wall_now();
        let a = (NodeId("a".into()), [1; 16]);
        let far = now + Duration::from_secs(120);
        registry.grant_lease(&v, &a, far).unwrap();
        for _ in 0..1000 {
            registry
                .grant_lease(&v, &a, now + Duration::from_secs(60))
                .unwrap();
        }
        assert_eq!(registry.lease_count(&v), 1);
        assert_eq!(registry.lease_total(), 1);
        let expiry = registry.leases.lock().unwrap().versions[&v].1[&a];
        assert_eq!(expiry, far, "expiry never shrinks");
    }

    #[test]
    fn lease_caps_refuse_new_keys_without_touching_existing() {
        let registry = WarmRegistry::new(4);
        registry
            .configure(Config {
                max_jobs: 1,
                ..Config::default()
            })
            .unwrap();
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        let v = version("a");
        let key = |i: u8| (NodeId(format!("n{i}")), [i; 16]);
        for i in 0..MAX_LEASES_PER_VERSION as u8 {
            registry.grant_lease(&v, &key(i), until).unwrap();
        }
        let stamp = registry.leases.lock().unwrap().versions[&v].0;
        assert_eq!(
            registry.grant_lease(&v, &key(200), until),
            Err(Error::Overloaded)
        );
        assert_eq!(registry.lease_count(&v), MAX_LEASES_PER_VERSION);
        assert_eq!(registry.leases.lock().unwrap().versions[&v].0, stamp);
        // Renewing an existing lease still succeeds at the cap.
        registry.grant_lease(&v, &key(0), until).unwrap();
        // The node-wide cap (LEASES_PER_JOB * max_jobs) also refuses other versions.
        assert_eq!(
            registry.grant_lease(&version("b"), &key(201), until),
            Err(Error::Overloaded)
        );
        assert!(registry.has_lease(&v, &key(1)));
        assert_eq!(registry.lease_total(), LEASES_PER_JOB);
    }

    #[test]
    fn expired_leases_are_pruned_to_free_capacity() {
        let registry = WarmRegistry::new(4);
        registry
            .configure(Config {
                max_jobs: 1,
                ..Config::default()
            })
            .unwrap();
        let now = uring_runtime::environment::wall_now();
        let key = |i: u8| (NodeId(format!("n{i}")), [i; 16]);
        // Fill the node-wide cap with already expired leases on other versions.
        for i in 0..LEASES_PER_JOB as u8 {
            registry
                .leases
                .lock()
                .unwrap()
                .versions
                .entry(version(&format!("old{i}")))
                .or_default()
                .1
                .insert(key(i), now - Duration::from_secs(1));
        }
        let v = version("a");
        registry
            .grant_lease(&v, &key(250), now + Duration::from_secs(60))
            .unwrap();
        assert_eq!(registry.lease_total(), 1);
        assert_eq!(registry.leases.lock().unwrap().versions.len(), 1);
    }

    #[test]
    fn release_queued_after_newer_grant_does_not_trim_new_lease() {
        let registry = WarmRegistry::new(4);
        let worker = crate::model::WorkerId(0);
        let v = version("a");
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        let a = (NodeId("a".into()), [1; 16]);
        let b = (NodeId("b".into()), [2; 16]);
        registry.grant_lease(&v, &a, until).unwrap();
        // Simulate the enqueue race: A observes it is last, then B is granted
        // and reserves before A's release reaches the queue.
        let observed = {
            let mut leases = registry.leases.lock().unwrap();
            leases.versions.remove(&v);
            leases.next
        };
        registry.grant_lease(&v, &b, until).unwrap();
        registry.release(&v, observed, &[worker]);
        assert!(release_all(&registry, worker).is_empty());
        assert!(registry.drained(worker), "stale release is discarded");
        assert!(registry.has_lease(&v, &b));
        // Dropping B, the actual last lease, still releases.
        assert!(registry.drop_lease(&v, &b, &[worker]));
        assert_eq!(release_all(&registry, worker), vec![v]);
    }

    /// Grants a long and a short lease, drops the long one, and expires the
    /// short one in place. Returns the version and registered worker.
    #[test]
    fn retry_with_shorter_ttl_keeps_lease_through_advertised_deadline() {
        let queue = std::rc::Rc::new(uring_runtime::drivers::DriverQueue::new(64));
        let _queue = queue.enter();
        let (local, _fixture) = crate::read::tests::admission_coordinator();
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(
                    super::super::dispatch::WorkerMap::new(vec![crate::model::WorkerId(0)])
                        .unwrap(),
                ),
                vec![crate::model::WorkerId(0)],
                8,
            )
            .unwrap(),
        );
        let mut endpoint = directory
            .install(crate::model::WorkerId(0), local.clone())
            .unwrap();
        let origin = crate::security::OriginContext {
            object: version("shorter").object,
            metadata: None,
            authorization: None,
        };
        let membership = Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        let metadata = ObjectMetadata {
            version: version("shorter"),
            length: 0,
            content_type: None,
            expires_at: crate::model::ExpiresAt::from_system_time(SystemTime::UNIX_EPOCH).unwrap(),
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let max = directory.warm.config().max_ttl;
        // A hold that outlived its lease, such as completed pages kept after a
        // failed warm, already promises a long deadline.
        let long = uring_runtime::environment::wall_now() + max;
        let caller = scope();
        let mut seed = Box::pin({
            let directory = directory.clone();
            let version = metadata.version.clone();
            async move {
                let held = BTreeMap::from([(0, crate::store::holds::HoldPages::prefix(1))]);
                let mut fresh = Vec::new();
                directory
                    .reserve_warm(&version, &held, long, &caller, &mut fresh)
                    .await
            }
        });
        let mut seeded = None;
        for _ in 0..32 {
            if let Poll::Ready(value) = seed.as_mut().poll(&mut cx) {
                seeded = Some(value);
                break;
            }
            queue.poll(&mut cx, 64);
            endpoint.poll(&mut cx, 64).unwrap();
        }
        assert_eq!(seeded.unwrap(), Ok(long));
        assert_eq!(directory.warm.lease_count(&metadata.version), 0);

        // Retry with a shorter TTL: the hold keeps its longer deadline.
        let ttl = max / 4;
        let caller = scope();
        let requested = uring_runtime::environment::wall_now() + ttl;
        let mut admission = Box::pin(admit(
            directory.clone(),
            metadata.clone(),
            membership.clone(),
            local.credentials.local_context(&origin, &caller).unwrap(),
            ttl,
            &caller,
        ));
        let mut result = None;
        for _ in 0..32 {
            if let Poll::Ready(value) = admission.as_mut().poll(&mut cx) {
                result = Some(value);
                break;
            }
            queue.poll(&mut cx, 64);
            endpoint.poll(&mut cx, 64).unwrap();
        }
        let advertised = result.unwrap().unwrap();
        assert_eq!(advertised, long, "advertises the inherited deadline");
        assert!(advertised > requested);
        drop(admission);
        for _ in 0..32 {
            queue.poll(&mut cx, 64);
            endpoint.poll(&mut cx, 64).unwrap();
            if queue.pending() == 0 {
                break;
            }
        }
        let expiry = directory.warm.leases.lock().unwrap().versions[&metadata.version]
            .1
            .values()
            .copied()
            .max()
            .unwrap();
        assert!(
            expiry >= advertised,
            "lease covers the advertised RetainUntil"
        );
        // Pruning past the requested deadline but before the advertised one
        // must not release the hold.
        let between = requested + Duration::from_secs(1);
        let emptied = directory.warm.leases.lock().unwrap().prune(between);
        assert!(emptied.is_empty(), "no release before advertised expiry");
        assert_eq!(directory.warm.lease_count(&metadata.version), 1);
        // Once the advertised deadline passes, the lease is released.
        let after = advertised + Duration::from_secs(1);
        let emptied = directory.warm.leases.lock().unwrap().prune(after);
        assert_eq!(emptied.len(), 1);
        assert_eq!(emptied[0].0, metadata.version);
    }

    fn expire_last_lease(registry: &WarmRegistry) -> (ObjectVersion, crate::model::WorkerId) {
        let worker = crate::model::WorkerId(0);
        assert!(release_all(registry, worker).is_empty(), "registers worker");
        let v = version("a");
        let now = uring_runtime::environment::wall_now();
        let long = (NodeId("long".into()), [1; 16]);
        let short = (NodeId("short".into()), [2; 16]);
        registry
            .grant_lease(&v, &long, now + Duration::from_secs(3600))
            .unwrap();
        registry
            .grant_lease(&v, &short, now + Duration::from_secs(60))
            .unwrap();
        assert!(!registry.drop_lease(&v, &long, &[worker]));
        assert!(registry.drained(worker), "short lease still live");
        *registry
            .leases
            .lock()
            .unwrap()
            .versions
            .get_mut(&v)
            .unwrap()
            .1
            .get_mut(&short)
            .unwrap() = now - Duration::from_secs(1);
        (v, worker)
    }

    #[test]
    fn last_lease_expiry_releases_unused_capacity() {
        let registry = WarmRegistry::new(4);
        let (v, worker) = expire_last_lease(&registry);
        registry.poll_deadlines();
        assert_eq!(registry.lease_count(&v), 0);
        assert_eq!(release_all(&registry, worker), vec![v]);
        assert!(registry.drained(worker));
    }

    #[test]
    fn prune_through_unrelated_grant_releases_unused_capacity() {
        let registry = WarmRegistry::new(4);
        let (v, worker) = expire_last_lease(&registry);
        let other = (NodeId("other".into()), [3; 16]);
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        registry.grant_lease(&version("b"), &other, until).unwrap();
        assert_eq!(registry.lease_count(&v), 0);
        assert_eq!(release_all(&registry, worker), vec![v]);
    }

    #[test]
    fn expiry_release_does_not_trim_newer_reservation() {
        let registry = WarmRegistry::new(4);
        let (v, worker) = expire_last_lease(&registry);
        let fresh = (NodeId("fresh".into()), [4; 16]);
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        // The grant prunes the expired lease, queuing a release, then stamps a
        // newer generation that fences it off.
        registry.grant_lease(&v, &fresh, until).unwrap();
        assert!(!registry.drained(worker));
        assert!(release_all(&registry, worker).is_empty());
        assert!(registry.drained(worker));
        // Expiry with a pending release followed by a newer grant also holds.
        registry.drop_lease(&v, &fresh, &[worker]);
        registry.grant_lease(&v, &fresh, until).unwrap();
        assert!(release_all(&registry, worker).is_empty());
        assert!(registry.has_lease(&v, &fresh));
    }

    #[test]
    fn retrying_release_does_not_survive_newer_grant() {
        let registry = WarmRegistry::new(4);
        let worker = crate::model::WorkerId(0);
        let v = version("a");
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        let a = (NodeId("a".into()), [1; 16]);
        let b = (NodeId("b".into()), [2; 16]);
        registry.grant_lease(&v, &a, until).unwrap();
        assert!(registry.drop_lease(&v, &a, &[worker]));
        let waker = Waker::from(Arc::new(crate::test_support::WakeCounter::default()));
        let mut cx = Context::from_waker(&waker);
        registry
            .poll_releases_with(worker, &mut cx, |_| Err(Error::Overloaded))
            .unwrap();
        assert!(!registry.drained(worker), "release is deferred");
        registry.grant_lease(&v, &b, until).unwrap();
        assert!(release_all(&registry, worker).is_empty());
        assert!(registry.drained(worker));
        assert!(registry.has_lease(&v, &b));
    }

    #[test]
    fn newer_release_queued_during_trim_is_kept() {
        let registry = WarmRegistry::new(4);
        let worker = crate::model::WorkerId(0);
        let v = version("a");
        registry.release(&v, 1, &[worker]);
        let waker = Waker::from(Arc::new(crate::test_support::WakeCounter::default()));
        let mut cx = Context::from_waker(&waker);
        registry
            .poll_releases_with(worker, &mut cx, |version| {
                registry.release(version, 2, &[worker]);
                Ok(())
            })
            .unwrap();
        assert!(!registry.drained(worker));
        assert_eq!(release_all(&registry, worker), vec![v]);
        assert!(registry.drained(worker));
    }

    #[test]
    fn expired_completed_job_cannot_replace_active_provider_selection() {
        let registry = WarmRegistry::new(4);
        let version = version("a");
        let (job, _) = registry.begin(&version).unwrap();
        job.update(|p| {
            p.running = false;
            p.admissions = 0;
            p.until = Some(SystemTime::UNIX_EPOCH);
        });
        let scheduler = super::super::range_stream::Scheduler::new(4);
        let range = crate::model::ByteRange::From(0).resolve(3).unwrap();
        let mut demand = scheduler
            .register(version.clone(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(super::super::range_stream::Next::Select(selection))) =
            demand.poll_next(&mut cx)
        else {
            panic!("selection");
        };
        assert!(matches!(
            scheduler.admit_warm(&version, &registry),
            Err(Error::Overloaded)
        ));
        assert_eq!(registry.snapshot().0, 0);
        drop(selection);
        assert!(scheduler.admit_warm(&version, &registry).unwrap().1);
    }

    #[test]
    fn failed_job_keeps_queued_admissions_visible_until_their_guards_drop() {
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(
                    super::super::dispatch::WorkerMap::new(vec![crate::model::WorkerId(0)])
                        .unwrap(),
                ),
                vec![crate::model::WorkerId(0)],
                4,
            )
            .unwrap(),
        );
        let version = version("a");
        let (job, _) = directory.warm.begin(&version).unwrap();
        directory.warm.begin(&version).unwrap();
        let owner = JobGuard {
            directory: directory.clone(),
            job: job.clone(),
            complete: false,
        };
        let extension = JobGuard {
            directory: directory.clone(),
            job: job.clone(),
            complete: true,
        };
        drop(owner);
        assert_eq!(directory.warm.snapshot().0, 1);
        assert!(!directory.warm.drained(crate::model::WorkerId(0)));
        assert!(matches!(
            directory.warm.begin(&version),
            Err(Error::Overloaded)
        ));
        drop(extension);
        assert_eq!(directory.warm.snapshot().0, 0);
        assert!(directory.warm.drained(crate::model::WorkerId(0)));
    }

    #[test]
    fn future_pages_wait_for_disk_frontier_and_cleanup_on_drop() {
        let registry = WarmRegistry::new(2);
        let version = version("a");
        let (job, fresh) = registry.begin(&version).unwrap();
        assert!(fresh);
        let (same, fresh) = registry.begin(&version).unwrap();
        assert!(!fresh);
        assert!(Arc::ptr_eq(&job, &same));
        let scope = scope();
        let page = PageId {
            version,
            number: PageNumber(7),
        };
        let wakes = Arc::new(crate::test_support::WakeCounter::default());
        let waker = Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        let mut waiting = Box::pin(registry.wait(&page, &scope));
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        assert_eq!(registry.snapshot(), (1, 1));
        job.update(|progress| progress.frontier = 7);
        assert!(wakes.count() > 0);
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        job.update(|progress| progress.frontier = 8);
        assert_eq!(waiting.as_mut().poll(&mut cx), Poll::Ready(Ok(true)));
        drop(waiting);
        assert_eq!(registry.snapshot(), (1, 0));
    }

    #[test]
    fn cancellation_is_not_a_completion_fence_and_failure_wakes_fallback() {
        let registry = WarmRegistry::new(2);
        let version = version("a");
        let (job, _) = registry.begin(&version).unwrap();
        let scope = scope();
        let page = PageId {
            version: version.clone(),
            number: PageNumber(99),
        };
        let mut waiting = Box::pin(registry.wait(&page, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        registry.stop();
        assert!(job.scope.cancellation.is_cancelled());
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        assert!(matches!(registry.begin(&version), Err(Error::Unavailable)));
        job.stop();
        registry.remove(&job);
        assert_eq!(waiting.as_mut().poll(&mut cx), Poll::Ready(Ok(false)));
        assert_eq!(registry.snapshot(), (0, 0));
    }

    #[test]
    fn waiter_limit_deadline_and_exact_version_are_enforced() {
        let registry = WarmRegistry::new(1);
        let version = version("a");
        let (_job, _) = registry.begin(&version).unwrap();
        let scope = scope();
        let page = PageId {
            version,
            number: PageNumber(1),
        };
        let mut first = Box::pin(registry.wait(&page, &scope));
        let mut second = Box::pin(registry.wait(&page, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            second.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        );
        drop(second);
        scope.cancel().unwrap();
        assert_eq!(
            first.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        drop(first);
        assert_eq!(registry.snapshot(), (1, 0));
        let expired =
            RequestScope::new(RequestId([1; 16]), uring_runtime::environment::now()).unwrap();
        assert_eq!(
            futures::executor::block_on(registry.wait(&page, &expired)),
            Err(Error::DeadlineExceeded)
        );
        let mut other = page;
        other.version.etag = crate::model::StrongEtag::test_value("b");
        assert_eq!(
            futures::executor::block_on(registry.wait(&other, &scope)),
            Ok(false)
        );
    }

    #[test]
    fn empty_jobs_expire_and_registry_bounds_versions() {
        let registry = WarmRegistry::new(1);
        registry
            .configure(Config {
                max_jobs: 1,
                ..Default::default()
            })
            .unwrap();
        let (job, _) = registry.begin(&version("a")).unwrap();
        assert!(matches!(
            registry.begin(&version("b")),
            Err(Error::Overloaded)
        ));
        job.update(|progress| {
            progress.running = false;
            progress.admissions = 0;
            progress.until = Some(SystemTime::UNIX_EPOCH);
        });
        registry.poll_deadlines();
        assert_eq!(registry.snapshot(), (0, 0));
        assert!(registry.begin(&version("b")).unwrap().1);
        let restarted = WarmRegistry::new(1);
        assert_eq!(restarted.snapshot(), (0, 0));
    }

    #[test]
    fn invalid_warm_limits_fail_closed() {
        for config in [
            Config {
                window: 0,
                ..Default::default()
            },
            Config {
                max_jobs: 0,
                ..Default::default()
            },
            Config {
                retained_percent: 101,
                ..Default::default()
            },
            Config {
                fill_timeout: Duration::ZERO,
                ..Default::default()
            },
            Config {
                max_ttl: Duration::ZERO,
                ..Default::default()
            },
        ] {
            assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        }
        Config::default().validate().unwrap();
    }
}
