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
                        let scope = request_scope(RESOLVE_RETRY * 2)?;
                        timer
                            .sleep(uring_runtime::environment::now() + RESOLVE_RETRY, &scope)
                            .await?;
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
}
