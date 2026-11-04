//! Production page source: the dataplane's own read path.

use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;

use super::manager::Resolved;
use super::service::{Local, PageSource};
use super::worker::Backend;
use crate::config::UblkDeviceConfig;
use crate::control::ReactorControlIo;
use crate::control::publication::PublicationTarget;
use crate::error::{Error, Result};
use crate::memory::VerifiedPage;
use crate::model::{MetadataSelector, ObjectId, ObjectVersion, PageId, PageNumber, RequestId};
use crate::read::dispatch::WorkerDirectory;
use crate::runtime::{Reactor, RequestScope};
use crate::security::OriginContext;

/// How long to wait before retrying a version lookup that could not run yet.
const RESOLVE_RETRY: Duration = Duration::from_secs(1);

const REQUEST_ID: RequestId = RequestId(*b"racer-ublk-reads");

/// Builds directory-backed page sources on one worker.
pub(crate) struct DirectoryBackend {
    pub directory: Arc<WorkerDirectory>,
    pub snapshots: Rc<PublicationTarget>,
    pub reactor: Rc<Reactor>,
    pub timeout: Duration,
}

fn request_scope(timeout: Duration) -> Result<RequestScope> {
    RequestScope::new(REQUEST_ID, uring_runtime::environment::now() + timeout)
}

/// Errors that mean "not yet", such as a cache missing from the snapshot.
fn transient(error: Error) -> bool {
    matches!(
        error,
        Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded | Error::Cancelled
    )
}

/// Waits until `until` between resolve attempts. A failed timer (for
/// example an overloaded reactor) must not fail the device, so the wait falls
/// back to yielding to the worker until the deadline passes. Cancellation is
/// by dropping the resolve future.
async fn pause<F, Fut>(until: std::time::Instant, mut sleep: F)
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = Result<()>>,
{
    while uring_runtime::environment::now() < until {
        if sleep().await.is_ok() {
            return;
        }
        uring_runtime::drivers::yield_now().await;
    }
}

impl DirectoryBackend {
    /// One lookup attempt.
    async fn resolve_once(&self, device: &UblkDeviceConfig) -> Result<Resolved> {
        let snapshot = self.snapshots.current()?;
        let cache = snapshot
            .caches
            .iter()
            .find(|cache| cache.name == device.cache)
            .map(|cache| cache.id.clone())
            .ok_or(Error::Unavailable)?;
        let object = ObjectId {
            cache: cache.clone(),
            key: device.key,
        };
        let context = OriginContext {
            object: object.clone(),
            metadata: None,
            authorization: None,
        };
        let selector = device
            .etag
            .clone()
            .map_or(MetadataSelector::Fresh, MetadataSelector::Pinned);
        let scope = request_scope(self.timeout)?;
        let mut budget = crate::read::default_budget(&scope);
        let metadata = self
            .directory
            .resolve_with_budget(
                selector,
                snapshot.membership.clone(),
                &context,
                &scope,
                &mut budget,
            )
            .await?;
        if metadata.version.object != object {
            return Err(Error::CorruptRecord);
        }
        if let Some(etag) = &device.etag
            && &metadata.version.etag != etag
        {
            return Err(Error::VersionUnavailable);
        }
        Ok(Resolved {
            cache,
            etag: metadata.version.etag,
            length: metadata.length,
        })
    }
}

impl Backend for DirectoryBackend {
    type Source = DirectorySource;

    fn resolve(self: Rc<Self>, device: UblkDeviceConfig) -> Local<'static, Result<Resolved>> {
        Box::pin(async move {
            let timer = ReactorControlIo::new(self.reactor.clone());
            let mut logged = false;
            loop {
                match self.resolve_once(&device).await {
                    Err(error) if transient(error) => {
                        if !logged {
                            logged = true;
                            eprintln!(
                                "racer: component=ublk cache={} stage=resolve state=waiting error={error:?}",
                                device.cache
                            );
                        }
                        let until = uring_runtime::environment::now() + RESOLVE_RETRY;
                        pause(until, || async {
                            let scope = request_scope(RESOLVE_RETRY * 2)?;
                            timer.sleep(until, &scope).await
                        })
                        .await;
                    }
                    result => return result,
                }
            }
        })
    }

    fn source(&self, device: &UblkDeviceConfig, resolved: &Resolved) -> Result<DirectorySource> {
        let object = ObjectId {
            cache: resolved.cache.clone(),
            key: device.key,
        };
        Ok(DirectorySource {
            directory: self.directory.clone(),
            snapshots: self.snapshots.clone(),
            timeout: self.timeout,
            version: ObjectVersion {
                object: object.clone(),
                etag: resolved.etag.clone(),
            },
            context: OriginContext {
                object,
                metadata: None,
                authorization: None,
            },
        })
    }
}

/// Pages of one pinned version, read through the worker directory.
pub(crate) struct DirectorySource {
    directory: Arc<WorkerDirectory>,
    snapshots: Rc<PublicationTarget>,
    timeout: Duration,
    version: ObjectVersion,
    context: OriginContext,
}

impl PageSource for DirectorySource {
    type Page = VerifiedPage;

    fn page(&self, number: u64) -> Local<'_, Result<VerifiedPage>> {
        Box::pin(async move {
            let membership = self.snapshots.current()?.membership.clone();
            let scope = request_scope(self.timeout)?;
            let mut budget = crate::read::default_budget(&scope);
            let page = PageId {
                version: self.version.clone(),
                number: PageNumber(number),
            };
            let result = self
                .directory
                .acquire(page, membership, &self.context, &scope, &mut budget)
                .await?;
            if result.metadata.version != self.version {
                return Err(Error::VersionUnavailable);
            }
            Ok(result.plaintext)
        })
    }

    /// Only pages this worker owns: their plaintext is charged to this
    /// worker's quota, which sizes the window. A remote owner's page would
    /// hold that owner's quota where its own overload retry cannot free it.
    fn retainable(&self, number: u64) -> bool {
        self.directory.owns_page(&PageId {
            version: self.version.clone(),
            number: PageNumber(number),
        })
    }
}

#[cfg(test)]
pub(super) mod tests {
    //! `DirectoryBackend` and `DirectorySource` over the real read path, with
    //! only the origin adapter scripted.

    use std::future::Future;
    use std::pin::Pin;
    use std::task::{Context, Poll};
    use std::time::Instant;

    use racer_control_wire::{CacheDefinition, CacheId};

    use super::*;
    use crate::admission::AdmissionPolicy;
    use crate::model::{CacheKey, ExpiresAt, ObjectMetadata, StrongEtag};
    use crate::test_support::ReadWorker;
    use crate::ublk::service::PageBytes;

    pub(crate) const NAME: &str = "ublk-directory";
    const LENGTH: u64 = 3000;

    fn etag(value: &[u8]) -> StrongEtag {
        StrongEtag::parse(value).unwrap()
    }

    fn definition() -> CacheDefinition {
        let (client_socket, origin_socket) =
            racer_control_wire::canonical_socket_paths(NAME).unwrap();
        CacheDefinition {
            id: CacheId(crate::test_support::security::CACHE.into()),
            name: NAME.into(),
            client_socket,
            origin_socket,
            priority: racer_control_wire::DEFAULT_PRIORITY,
        }
    }

    fn metadata(etag_value: &[u8], length: u64) -> ObjectMetadata {
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: definition().id,
                    key: CacheKey([5; 32]),
                },
                etag: etag(etag_value),
            },
            length,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        }
    }

    fn body() -> Vec<u8> {
        (0..LENGTH).map(|i| (i * 7 % 251) as u8).collect()
    }

    pub(crate) fn device(cache: &str, etag_value: Option<&[u8]>) -> UblkDeviceConfig {
        UblkDeviceConfig {
            cache: cache.into(),
            key: CacheKey([5; 32]),
            etag: etag_value.map(etag),
            id: None,
        }
    }

    /// One read worker whose origin serves a single object of the cache
    /// `NAME`, and a backend over it.
    pub(crate) struct Fixture {
        pub worker: ReadWorker,
        pub reactor: Rc<Reactor>,
        pub backend: Rc<DirectoryBackend>,
    }

    impl Fixture {
        fn new() -> Self {
            let fixture = Self::object(LENGTH);
            fixture.worker.origin.set_body(body());
            fixture
        }

        /// An object of `length` bytes. Each page holds its page number in
        /// every byte unless the caller sets a body.
        pub(crate) fn object(length: u64) -> Self {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let delivery = Rc::new(crate::http::Delivery::new(
                reactor.clone(),
                Duration::from_secs(5),
            ));
            let worker = ReadWorker::new(
                definition(),
                metadata(b"\"v1\"", length),
                admission,
                reactor.clone(),
                delivery,
                1,
            );
            let backend = Rc::new(DirectoryBackend {
                directory: worker.directory.clone(),
                snapshots: worker.snapshots.clone(),
                reactor: reactor.clone(),
                timeout: Duration::from_secs(5),
            });
            Self {
                worker,
                reactor,
                backend,
            }
        }

        /// Poll `future` with the worker and reactor until it finishes, or
        /// return it unfinished after `limit`.
        fn run_for<T>(
            &self,
            mut future: Pin<Box<dyn Future<Output = T> + '_>>,
            limit: Duration,
        ) -> Option<T> {
            let _queue = self.worker.drivers.enter();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let deadline = Instant::now() + limit;
            loop {
                if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                    return Some(value);
                }
                if Instant::now() >= deadline {
                    return None;
                }
                self.reactor.poll_budgeted(64).unwrap();
                self.worker.poll(&mut cx);
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        fn run<T>(&self, future: Pin<Box<dyn Future<Output = T> + '_>>) -> T {
            self.run_for(future, Duration::from_secs(20))
                .expect("future did not finish")
        }

        fn resolve(&self, device: &UblkDeviceConfig) -> Result<Resolved> {
            self.run(Box::pin(self.backend.resolve_once(device)))
        }
    }

    #[test]
    fn transient_errors_are_retried() {
        for error in [
            Error::Unavailable,
            Error::Overloaded,
            Error::DeadlineExceeded,
            Error::Cancelled,
        ] {
            assert!(transient(error), "{error:?}");
        }
        for error in [
            Error::VersionUnavailable,
            Error::NotFound,
            Error::CorruptRecord,
        ] {
            assert!(!transient(error), "{error:?}");
        }
    }

    #[test]
    fn failed_retry_timer_still_waits_and_returns() {
        let fixture = Fixture::new();
        // A timer that fails a few times, then works.
        let calls = std::cell::Cell::new(0);
        let until = uring_runtime::environment::now() + Duration::from_secs(5);
        let flaky = pause(until, || {
            calls.set(calls.get() + 1);
            let result = if calls.get() < 4 {
                Err(Error::Overloaded)
            } else {
                Ok(())
            };
            async move { result }
        });
        fixture.run(Box::pin(flaky));
        assert_eq!(calls.get(), 4);
        // A timer that never works still ends the wait at the deadline
        // instead of failing.
        let started = Instant::now();
        let until = uring_runtime::environment::now() + Duration::from_millis(50);
        let broken = pause(until, || async { Err(Error::Overloaded) });
        fixture.run(Box::pin(broken));
        assert!(started.elapsed() >= Duration::from_millis(50));
    }

    #[test]
    fn missing_cache_is_unavailable_and_resolve_keeps_waiting() {
        let fixture = Fixture::new();
        let missing = device("absent", None);
        assert_eq!(fixture.resolve(&missing), Err(Error::Unavailable));
        // The retrying lookup neither fails nor asks the origin.
        let waiting = fixture.backend.clone().resolve(missing);
        assert!(
            fixture
                .run_for(waiting, Duration::from_millis(1500))
                .is_none()
        );
        assert_eq!(fixture.worker.origin.calls().len(), 0);
    }

    #[test]
    fn fresh_and_pinned_resolve_to_the_origin_version() {
        let fixture = Fixture::new();
        for pin in [None, Some(&b"\"v1\""[..])] {
            let resolved = fixture.resolve(&device(NAME, pin)).unwrap();
            assert_eq!(resolved.cache, definition().id);
            assert_eq!(resolved.etag, etag(b"\"v1\""));
            assert_eq!(resolved.length, LENGTH);
        }
        let resolved = fixture
            .run(fixture.backend.clone().resolve(device(NAME, None)))
            .unwrap();
        assert_eq!(resolved.etag, etag(b"\"v1\""));
    }

    #[test]
    fn unknown_pin_and_missing_object_fail() {
        let fixture = Fixture::new();
        let pinned = fixture.resolve(&device(NAME, Some(b"\"v9\"")));
        assert_eq!(pinned, Err(Error::VersionUnavailable));
        fixture.worker.origin.set_missing(true);
        let missing = fixture.resolve(&device(NAME, None));
        assert_eq!(missing, Err(Error::NotFound));
    }

    #[test]
    fn pages_match_the_origin_object() {
        let fixture = Fixture::new();
        let config = device(NAME, None);
        let resolved = fixture.resolve(&config).unwrap();
        let source = fixture.backend.source(&config, &resolved).unwrap();
        let page = fixture.run(source.page(0)).unwrap();
        assert_eq!(PageBytes::bytes(&page), &body()[..]);
    }

    #[test]
    fn changed_version_fails_the_page_read() {
        let fixture = Fixture::new();
        let config = device(NAME, None);
        let resolved = fixture.resolve(&config).unwrap();
        let source = fixture.backend.source(&config, &resolved).unwrap();
        fixture
            .worker
            .origin
            .set_version(metadata(b"\"v2\"", LENGTH));
        let result = fixture.run(source.page(0));
        assert_eq!(result.err(), Some(Error::VersionUnavailable));
    }

    #[test]
    fn kept_pages_spare_small_reads_a_decrypt_each() {
        use crate::ublk::service::Windowed;
        use crate::ublk::window::PageWindow;

        let fixture = Fixture::new();
        // Keep the ciphertext cached, so only plaintext comes and goes.
        fixture.worker.retain(true);
        let config = device(NAME, None);
        let resolved = fixture.resolve(&config).unwrap();
        let source = Rc::new(fixture.backend.source(&config, &resolved).unwrap());
        drop(fixture.run(source.page(0)).unwrap());
        let reads = 4;
        // Without a window, each read of the cached page decrypts it again.
        let before = fixture.worker.cpu_decrypts();
        for _ in 0..reads {
            let page = fixture.run(source.page(0)).unwrap();
            assert_eq!(PageBytes::bytes(&page), &body()[..]);
        }
        assert_eq!(fixture.worker.cpu_decrypts() - before, reads);
        // With one, consecutive reads share one decrypt.
        let windowed = Windowed::new(source, Rc::new(PageWindow::default()));
        let before = fixture.worker.cpu_decrypts();
        for _ in 0..reads {
            let page = fixture.run(windowed.page(0)).unwrap();
            assert_eq!(PageBytes::bytes(&page), &body()[..]);
        }
        assert_eq!(fixture.worker.cpu_decrypts() - before, 1);
    }

    #[test]
    fn only_pages_this_worker_owns_are_retainable() {
        let fixture = Fixture::new();
        let config = device(NAME, None);
        let resolved = fixture.resolve(&config).unwrap();
        let source = fixture.backend.source(&config, &resolved).unwrap();
        // The fixture's one worker owns every page.
        assert!(source.retainable(0));
        assert!(source.retainable(1));
        // A thread that is not that worker owns none of them.
        let directory = source.directory.clone();
        let page = PageId {
            version: source.version.clone(),
            number: PageNumber(0),
        };
        let owned = std::thread::spawn(move || directory.owns_page(&page))
            .join()
            .unwrap();
        assert!(!owned);
    }
}
