//! Write-back caching service: client writes, coordinator sessions, store
//! targets, staging, and upload. See `designs/racer-writes.md` ("WP5: write
//! service" and "WP5 as built").
//!
//! Every node runs every role. A request names one object, so each role runs
//! on the worker that owns the object's page zero (`metadata_owner`) or the
//! page (`page_owner`); [`WriteWork`] carries work between the workers of a
//! node, and peer operations carry it between nodes:
//!
//! - The writer ([`client`]) runs on the worker that accepted the client
//!   connection. It mints the version, encrypts and stages pages
//!   ([`staging`]), asks each page's store targets to pull them, and commits.
//! - The coordinator ([`coordinator`]) runs on the page-zero owner of the
//!   first ranked member of page zero. It owns the session state machine
//!   (`racer_core::write::Session`), the journal records of the session, the
//!   [`directory`] that metadata refreshes consult, deletes, holds, and the
//!   upload.
//! - Store targets ([`store_target`]) pull a page from the writer, verify it,
//!   and make it durable with `StoreWriter::persist_written`.
//! - Holders upload their pages with Put Block ([`upload`]); the coordinator
//!   commits the block list.

pub mod client;
pub mod coordinator;
pub mod directory;
pub mod metrics;
pub mod staging;
pub mod store_target;
pub mod upload;

#[cfg(test)]
mod tests;

use crate::config::WriteSettings;
use crate::error::Error;
use crate::error::Result;
use crate::model::ObjectId;
use crate::model::PageId;
use crate::model::StrongEtag;
use crate::origin::upload::UploadLimits;
use crate::origin::upload::UploadSink;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerResponse;
use crate::peer::protocol::WriteStore;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::security::Authorization;
use crate::security::OpaqueMetadata;
use crate::security::OriginContext;
use crate::security::PeerOriginContext;
use crate::topology::Membership;
use racer_core::write::SessionId;
use racer_core::write::WriteReply;
use racer_wire::control::NodeId;
use std::cell::Cell;
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;
use std::time::Instant;

pub use directory::Directory;
pub use metrics::WriteMetrics;
pub use staging::WriteStaging;

/// Interval between write maintenance turns (lease and hold expiry, holder
/// page expiry, metric samples).
pub const TICK: Duration = Duration::from_millis(200);

/// Store targets scanned per page: the exactly ranked members of its
/// placement (`topology::RANKED_DEPTH`).
pub const RANKED_DEPTH: usize = ::topology::RANKED_DEPTH;

/// Pages one write operation may name (the prefetch page-list limit).
pub const OPERATION_PAGES: usize = 256;

/// Work one worker hands another on the same node.
pub enum WriteWork {
    /// Serve a write peer operation on the object's metadata worker.
    Serve {
        requester: NodeId,
        operation: PeerOperation,
        context: Option<PeerOriginContext>,
        membership: Arc<Membership>,
    },
    /// Pull and durably store one page on its owning worker.
    Store {
        page: PageId,
        store: WriteStore,
        membership: Arc<Membership>,
    },
    /// Release (`keep`) or drop one written page on its owning worker.
    Release {
        page: PageId,
        session: SessionId,
        keep: bool,
    },
    /// Upload one written page with Put Block on its owning worker.
    Upload {
        page: PageId,
        session: SessionId,
        context: PeerOriginContext,
    },
}

/// Result of [`WriteWork`].
pub enum WriteValue {
    Reply(WriteReply),
    Page(PageOutcome),
}

/// Outcome of one page of a store, release, or upload.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PageOutcome {
    /// Stored, released, or uploaded.
    Done,
    /// The store target is over its dirty cap or out of written segments.
    Full,
    /// Anything else; the caller tries another target.
    Failed,
}

/// Per-worker write state, owned by the worker's [`Coordinator`].
pub struct WriteService {
    settings: RefCell<WriteSettings>,
    sink: RefCell<Option<Rc<dyn UploadSink>>>,
    limits: RefCell<Option<Arc<UploadLimits>>>,
    reactor: RefCell<Option<Rc<crate::runtime::Reactor>>>,
    directory: Rc<Directory>,
    loaded: Cell<bool>,
    next_tick: Cell<Option<Instant>>,
    ticking: Rc<Cell<bool>>,
}

impl WriteService {
    /// A service over `directory`, which the worker's metadata service also
    /// consults. Unconfigured: default settings and no upload sink.
    pub fn new(directory: Rc<Directory>) -> Self {
        Self {
            settings: RefCell::new(WriteSettings::default()),
            sink: RefCell::new(None),
            limits: RefCell::new(None),
            reactor: RefCell::new(None),
            directory,
            loaded: Cell::new(false),
            next_tick: Cell::new(None),
            ticking: Rc::new(Cell::new(false)),
        }
    }

    /// Install the node settings, the upload sink, the node-wide upload
    /// limits, and the worker reactor (for timed waits).
    pub fn configure(
        &self,
        settings: WriteSettings,
        sink: Option<Rc<dyn UploadSink>>,
        limits: Arc<UploadLimits>,
        reactor: Rc<crate::runtime::Reactor>,
    ) {
        *self.settings.borrow_mut() = settings;
        *self.sink.borrow_mut() = sink;
        *self.limits.borrow_mut() = Some(limits);
        *self.reactor.borrow_mut() = Some(reactor);
    }

    /// Replace the upload sink (tests).
    pub fn set_sink(&self, sink: Option<Rc<dyn UploadSink>>) {
        *self.sink.borrow_mut() = sink;
    }

    /// Replace the node settings.
    pub fn set_settings(&self, settings: WriteSettings) {
        *self.settings.borrow_mut() = settings;
    }

    pub fn settings(&self) -> WriteSettings {
        self.settings.borrow().clone()
    }

    pub fn directory(&self) -> &Rc<Directory> {
        &self.directory
    }

    pub(crate) fn sink(&self) -> Option<Rc<dyn UploadSink>> {
        self.sink.borrow().clone()
    }

    pub(crate) fn limits(&self) -> Option<Arc<UploadLimits>> {
        self.limits.borrow().clone()
    }

    pub(crate) fn reactor(&self) -> Option<Rc<crate::runtime::Reactor>> {
        self.reactor.borrow().clone()
    }

    /// When the next maintenance turn is due, for the worker's park bound.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.next_tick.get()
    }

    /// Run due maintenance from the worker loop. Starts at most one detached
    /// maintenance task at a time and never blocks.
    pub fn poll(&self, local: &Coordinator, now: Instant) {
        if self.ticking.get() {
            return;
        }
        if self.next_tick.get().is_some_and(|at| now < at) {
            return;
        }
        self.next_tick.set(Some(now + TICK));
        let Ok(local) = local.prefetch_directory().local_coordinator() else {
            return;
        };
        let Ok(permit) = local.drivers().reserve() else {
            return;
        };
        let ticking = self.ticking.clone();
        ticking.set(true);
        permit.submit_detached(async move {
            let _ = coordinator::maintain(&local).await;
            ticking.set(false);
            Ok::<(), Error>(())
        });
    }

    /// Whether the journal's sessions were loaded into the directory.
    pub(crate) fn loaded(&self) -> bool {
        self.loaded.get()
    }

    pub(crate) fn set_loaded(&self) {
        self.loaded.set(true);
    }
}

/// Copy `context` into independently owned storage, for an upload that
/// outlives the request. Origin contexts are deliberately not `Clone`; this
/// is the one place a write keeps a second copy, in zeroizing buffers.
pub fn duplicate_context(context: &OriginContext) -> Result<OriginContext> {
    Ok(OriginContext {
        object: context.object.clone(),
        metadata: context
            .metadata
            .as_ref()
            .map(|m| OpaqueMetadata::from_header(m.as_header()))
            .transpose()?,
        authorization: context
            .authorization
            .as_ref()
            .map(|a| Authorization::from_header(a.expose_for_origin()))
            .transpose()?,
    })
}

/// A context for copy-mode operations, which carry no origin credentials.
pub fn bare_context(object: &ObjectId) -> OriginContext {
    OriginContext {
        object: object.clone(),
        metadata: None,
        authorization: None,
    }
}

/// The 32 lowercase hex digits of a Racer version ETag `"racer-<hex>"`.
pub fn version_hex(etag: &StrongEtag) -> Option<[u8; 32]> {
    let text = etag.as_bytes();
    let inner = text.strip_prefix(b"\"racer-")?.strip_suffix(b"\"")?;
    let hex: [u8; 32] = inner.try_into().ok()?;
    hex.iter()
        .all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
        .then_some(hex)
}

/// Whether `etag` names a Racer-written version.
pub fn is_written_etag(etag: &StrongEtag) -> bool {
    version_hex(etag).is_some()
}

/// Mint a new Racer version ETag from 16 random bytes.
pub fn mint_etag(random: [u8; 16]) -> Result<StrongEtag> {
    let mut text = String::with_capacity(40);
    text.push_str("\"racer-");
    for byte in random {
        text.push_str(&format!("{byte:02x}"));
    }
    text.push('"');
    StrongEtag::parse(text.as_bytes()).map_err(Into::into)
}

/// Milliseconds since the Unix epoch on this worker's wall clock.
pub(crate) fn now_millis(local: &Coordinator) -> u64 {
    crate::store::writes::unix_millis(wall_now(local))
}

pub(crate) fn wall_now(local: &Coordinator) -> std::time::SystemTime {
    local.fill.dependencies.candidates.ctx().clock.wall_now()
}

/// Send `operation` to `node` and return its write reply. A local `node`
/// is served in process on the object's metadata worker, without a network
/// hop. Copy-mode operations (`write-store`, `write-release`) carry no
/// origin context.
pub fn send<'a>(
    local: &'a Coordinator,
    membership: &'a Arc<Membership>,
    node: &'a NodeId,
    operation: PeerOperation,
    context: &'a OriginContext,
    scope: &'a RequestScope,
) -> crate::error::Operation<'a, WriteReply> {
    Box::pin(send_inner(local, membership, node, operation, context, scope))
}

async fn send_inner(
    local: &Coordinator,
    membership: &Arc<Membership>,
    node: &NodeId,
    operation: PeerOperation,
    context: &OriginContext,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let object = operation
        .write_object()
        .ok_or(Error::InvalidRequest)?
        .clone();
    let acquire = matches!(
        operation,
        PeerOperation::WriteSession { .. } | PeerOperation::WriteUpload { .. }
    );
    let bare = bare_context(&object);
    let context = if acquire { context } else { &bare };
    if *node == local.local_node() {
        return local
            .prefetch_directory()
            .write_serve(node.clone(), operation, context, membership.clone(), scope)
            .await;
    }
    let mode = if acquire {
        FetchMode::Acquire
    } else {
        FetchMode::CopyOnly
    };
    let mut budget = crate::read::default_budget(scope);
    let response = local
        .fill
        .dependencies
        .candidates
        .request(
            membership,
            node,
            context,
            &operation,
            mode,
            scope,
            &mut budget,
            1,
        )
        .await?;
    match response.response() {
        PeerResponse::Write(reply) => Ok(reply.clone()),
        PeerResponse::Overloaded | PeerResponse::FlightFull => Err(Error::Overloaded),
        PeerResponse::StaleMembership => Err(Error::IncompatibleMembership),
        _ => Err(Error::Unavailable),
    }
}

/// Execute cross-worker write work on this worker.
pub async fn execute(
    local: &Coordinator,
    work: WriteWork,
    scope: &RequestScope,
) -> Result<WriteValue> {
    match work {
        WriteWork::Serve {
            requester,
            operation,
            context,
            membership,
        } => {
            let object = operation
                .write_object()
                .ok_or(Error::InvalidRequest)?
                .clone();
            let reply = match context {
                Some(envelope) => {
                    let context = local.open_context(envelope)?;
                    coordinator::serve(local, &requester, operation, &context, membership, scope)
                        .await?
                }
                None => {
                    let context = bare_context(&object);
                    coordinator::serve(local, &requester, operation, &context, membership, scope)
                        .await?
                }
            };
            Ok(WriteValue::Reply(reply))
        }
        WriteWork::Store {
            page,
            store,
            membership,
        } => store_target::store_page(local, page, &store, membership, scope)
            .await
            .map(WriteValue::Page),
        WriteWork::Release {
            page,
            session,
            keep,
        } => store_target::release_page(local, &page, session, keep)
            .await
            .map(WriteValue::Page),
        WriteWork::Upload {
            page,
            session,
            context,
        } => {
            let context = local.open_context(context)?;
            upload::upload_page(local, &page, session, &context, scope)
                .await
                .map(WriteValue::Page)
        }
    }
}
