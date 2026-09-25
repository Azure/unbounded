//! Node-local routing to the sole worker owning a page or metadata flight.
//!
//! Ingress reactors may differ from the owner. Transfer request context through a
//! bounded command exactly once; keep replies and body leases completion-owned.
//! Metadata/bootstrap selects page zero, while a range's pages fan out individually.
//! Worker-local futures never cross threads. No second coordinator is constructed.

use super::serve::{Coordinator, ReadResponse, ReadService};
use crate::{
    client::request::ClientRequest,
    error::{Operation, deferred},
    model::identity::WorkerId,
    peer::{
        server::LocalPageService,
        wire::{PeerResponse, VerifiedRequest},
    },
    runtime::deadline::RequestScope,
};
use std::{rc::Rc, sync::Arc};

/// Node-wide bounded command mailboxes. Implementation must define Send-safe
/// request/reply messages, generation tags, backpressure, and cancellation fencing.
pub struct WorkerDirectory;
impl WorkerDirectory {
    /// Bounded handoff to the object's page-zero owner, independent of ETag.
    pub fn resolve_metadata<'a>(
        &'a self,
        _selector: crate::model::metadata::MetadataSelector,
        _membership: crate::topology::membership::MembershipLease,
        _context: &'a crate::model::context::OriginContext,
        _scope: &'a RequestScope,
    ) -> Operation<'a, crate::model::metadata::ObjectMetadata> {
        deferred("dispatch.resolve_metadata")
    }
    /// Fill completion publishes only immutable version facts, never a current
    /// pointer. Catalog admission may evict descriptors under its own bounded quota.
    pub fn publish_metadata<'a>(
        &'a self,
        _metadata: crate::model::metadata::VersionMetadata,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("dispatch.publish_metadata")
    }
    /// Bounded local-shard lookup of page-attached descriptors after page-zero
    /// catalog eviction or restart. Includes memory, pending writes, and index.
    /// No peer/origin I/O; reject conflicting lengths for the exact same version.
    pub fn retained_metadata<'a>(
        &'a self,
        _version: &'a crate::model::identity::ObjectVersion,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<crate::model::metadata::VersionMetadata>> {
        deferred("dispatch.retained_metadata")
    }
    /// Select the stable page owner and enqueue a bounded request. The transport
    /// implementation moves an owned command, never the borrowed worker future.
    pub fn acquire<'a>(
        &'a self,
        _page: crate::model::identity::PageId,
        _membership: crate::topology::membership::MembershipLease,
        _context: &'a crate::model::context::OriginContext,
        _scope: &'a RequestScope,
    ) -> Operation<'a, super::fill::PageResult> {
        deferred("dispatch.acquire_page")
    }
}

pub struct Dispatcher {
    worker: WorkerId,
    directory: Arc<WorkerDirectory>,
    local: Rc<Coordinator>,
}
impl Dispatcher {
    pub fn new(worker: WorkerId, directory: Arc<WorkerDirectory>, local: Rc<Coordinator>) -> Self {
        Self {
            worker,
            directory,
            local,
        }
    }
}
impl ReadService for Dispatcher {
    fn read<'a>(
        &'a self,
        _request: ClientRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        deferred("dispatch.read")
    }
}
impl LocalPageService for Dispatcher {
    fn serve_peer<'a>(
        &'a self,
        _request: VerifiedRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        deferred("dispatch.peer")
    }
}

#[cfg(test)]
mod tests {
    // Multiple ingress reactors must produce one owner/flight; cover full queues,
    // cancelled replies, metadata/page ownership, and draining before map changes.
}
