//! Node-wide warm jobs. Progress means disk publication, not writer queue admission.

use super::dispatch::WorkerDirectory;
use crate::error::{Error, Result};
use crate::model::{ObjectMetadata, ObjectVersion, PAGE_BYTES, PageId, PageNumber, RequestId};
use crate::runtime::RequestScope;
use crate::security::ChargedOriginContext;
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
    admissions: usize,
    running: bool,
    until: Option<SystemTime>,
    frontier: u64,
    stopped: bool,
    waiters: BTreeMap<u64, (Waker, std::time::Instant)>,
    sequence: u64,
}

pub(crate) struct Job {
    version: ObjectVersion,
    scope: RequestScope,
    admission: futures::lock::Mutex<()>,
    progress: Mutex<Progress>,
}

pub struct WarmRegistry {
    config: Mutex<Config>,
    jobs: Mutex<BTreeMap<ObjectVersion, Arc<Job>>>,
    waiter_limit: usize,
    draining: std::sync::atomic::AtomicBool,
    releases: Mutex<BTreeMap<u16, ReleaseQueue>>,
}
type ReleaseQueue = (Option<Waker>, std::collections::BTreeSet<ObjectVersion>);

impl WarmRegistry {
    pub(crate) fn new(waiter_limit: usize) -> Self {
        Self {
            config: Mutex::new(Config::default()),
            jobs: Mutex::new(BTreeMap::new()),
            waiter_limit,
            draining: std::sync::atomic::AtomicBool::new(false),
            releases: Mutex::new(BTreeMap::new()),
        }
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
        let versions = {
            let mut releases = self.releases.lock().unwrap();
            let (waker, versions) = releases.entry(worker.0).or_default();
            *waker = Some(cx.waker().clone());
            std::mem::take(versions)
        };
        for version in versions {
            index.release_hold(&version)?;
        }
        Ok(())
    }

    fn release(&self, version: &ObjectVersion, workers: &[crate::model::WorkerId]) {
        let wake = {
            let mut releases = self.releases.lock().unwrap();
            workers
                .iter()
                .filter_map(|worker| {
                    let (waker, versions) = releases.entry(worker.0).or_default();
                    versions.insert(version.clone());
                    waker.clone()
                })
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }

    pub(super) fn begin(&self, version: &ObjectVersion) -> Result<(Arc<Job>, bool)> {
        use std::sync::atomic::Ordering;
        if self.draining.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        let config = self.config();
        let mut jobs = self.jobs.lock().unwrap();
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
            progress.admissions += 1;
            return Ok((job.clone(), false));
        }
        if jobs.len() >= config.max_jobs
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
            version: version.clone(),
            scope: RequestScope::new(
                RequestId(request),
                uring_runtime::environment::now() + config.fill_timeout,
            )?,
            admission: futures::lock::Mutex::new(()),
            progress: Mutex::new(Progress {
                admissions: 1,
                running: true,
                until: None,
                frontier: 0,
                stopped: false,
                waiters: BTreeMap::new(),
                sequence: 0,
            }),
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
        let jobs = self
            .jobs
            .lock()
            .unwrap()
            .values()
            .cloned()
            .collect::<Vec<_>>();
        for job in jobs {
            let progress = job.progress.lock().unwrap();
            let expired = progress.until.is_some_and(|until| until <= wall);
            let cancel = progress.running && (expired || job.scope.deadline.0 <= now);
            let remove = !progress.running && progress.admissions == 0 && expired;
            let wakes = progress
                .waiters
                .values()
                .filter(|(_, deadline)| *deadline <= now || remove)
                .map(|(waker, _)| waker.clone())
                .collect::<Vec<_>>();
            drop(progress);
            if cancel {
                let _ = job.scope.cancel();
            }
            if remove {
                job.stop();
                self.remove(&job);
            }
            for wake in wakes {
                wake.wake();
            }
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
        std::future::poll_fn(|cx| {
            cancellation.register(cx.waker());
            scope.check()?;
            waiter.poll(cx, page.number.0)
        })
        .await
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
        if let Some(id) = self.id {
            self.job.progress.lock().unwrap().waiters.remove(&id);
        }
    }
}

struct JobGuard {
    directory: Arc<WorkerDirectory>,
    job: Arc<Job>,
    complete: bool,
}
impl Drop for JobGuard {
    fn drop(&mut self) {
        self.job.progress.lock().unwrap().admissions -= 1;
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
    caller.check()?;
    let config = directory.warm.config();
    if ttl.is_zero() {
        return Err(Error::InvalidRequest);
    }
    let until = uring_runtime::environment::wall_now()
        .checked_add(ttl.min(config.max_ttl))
        .ok_or(Error::InvalidRequest)?;
    let permit = uring_runtime::drivers::reserve()?;
    let (job, fresh) =
        directory
            .subscriptions
            .admit_warm(&metadata.version, &directory.warm, || {
                directory.warm.begin(&metadata.version)
            })?;
    let (send, receive) = futures::channel::oneshot::channel();
    permit.submit_detached(Box::pin(async move {
        let mut guard = JobGuard {
            directory: directory.clone(),
            job: job.clone(),
            complete: !fresh,
        };
        let lock = job.admission.lock().await;
        if job.progress.lock().unwrap().stopped {
            let _ = send.send(Err(Error::Unavailable));
            return Ok::<(), Error>(());
        }
        let old = job.progress.lock().unwrap().until;
        let target = old.map_or(until, |old| old.max(until));
        let pages = metadata.length.div_ceil(PAGE_BYTES);
        let reservation_scope = if fresh {
            job.scope.clone()
        } else {
            RequestScope::new(
                job.scope.request,
                uring_runtime::environment::now() + config.fill_timeout,
            )?
        };
        let mut fresh_workers = Vec::new();
        let admitted = directory
            .reserve_warm(
                &metadata.version,
                pages,
                target,
                &reservation_scope,
                &mut fresh_workers,
            )
            .await
            .and_then(|accepted| {
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
                    directory.warm.release(&metadata.version, &fresh_workers);
                    job.stop();
                }
                let _ = send.send(Err(error));
                return Ok::<(), Error>(());
            }
        };
        job.update(|progress| progress.until = Some(target));
        drop(lock);
        let _ = send.send(Ok(target));
        if !fresh {
            return Ok(());
        }
        let result = run(
            &directory,
            &job,
            &metadata,
            membership,
            &context,
            config.window,
        )
        .await;
        if result.is_err() {
            let _lock = job.admission.lock().await;
            directory.warm.release(&metadata.version, &fresh_workers);
            job.stop();
        } else {
            job.update(|progress| progress.running = false);
            guard.complete = true;
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
    metadata: &ObjectMetadata,
    membership: Arc<crate::topology::Membership>,
    context: &ChargedOriginContext,
    window: usize,
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
                failure = Some(Error::DeadlineExceeded);
                break;
            }
            let page = PageId {
                version: metadata.version.clone(),
                number: PageNumber(next),
            };
            pending.push_back(directory.warm_page(
                page,
                metadata.immutable(),
                membership.clone(),
                context,
                &job.scope,
            ));
            next += 1;
        }
        if failure.is_some() {
            let _ = job.scope.cancel();
        }
        let Some(result) = pending.next().await else {
            return failure.map_or(Ok(()), Err);
        };
        match result {
            Ok(()) if failure.is_none() => job.update(|progress| progress.frontier += 1),
            Err(error) => {
                failure.get_or_insert(error);
            }
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::future::Future;

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
