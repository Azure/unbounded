//! HEAD, initial page-zero GET, pinned single-range reads, and local peer service.
//!
//! Bootstrap retries version changes within its original budget before exposing
//! headers. Empty objects allocate no page. If-Match is the pin, not a read token.
//! Both entry points share this coordinator and its flights; peer transport never
//! owns another coordinator. Origin Authorization has no effect on Racer auth.
use super::{
    fill::Fill,
    metadata::MetadataService,
    range_stream::{RangeStream, RangeStreams},
};
use crate::{
    client::request::ClientRequest,
    control::snapshot::SnapshotStore,
    error::{Operation, deferred},
    model::{metadata::ObjectMetadata, range::ResolvedRange},
    peer::{
        server::LocalPageService,
        wire::{PeerResponse, VerifiedRequest},
    },
    runtime::deadline::RequestScope,
    security::credentials::CredentialCrypto,
};
use std::rc::Rc;
pub struct ReadResponse {
    pub metadata: ObjectMetadata,
    pub range: Option<ResolvedRange>,
    pub body: Option<RangeStream>,
}
pub trait ReadService {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse>;
}
pub struct Coordinator {
    snapshots: Rc<SnapshotStore>,
    metadata: Rc<MetadataService>,
    fill: Rc<Fill>,
    streams: Rc<RangeStreams>,
    credentials: Rc<CredentialCrypto>,
}
impl Coordinator {
    pub fn new(
        snapshots: Rc<SnapshotStore>,
        metadata: Rc<MetadataService>,
        fill: Rc<Fill>,
        streams: Rc<RangeStreams>,
        credentials: Rc<CredentialCrypto>,
    ) -> Self {
        Self {
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
        }
    }
}
impl ReadService for Coordinator {
    fn read<'a>(
        &'a self,
        _request: ClientRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        deferred("read.serve")
    }
}
impl LocalPageService for Coordinator {
    fn serve_peer<'a>(
        &'a self,
        _request: VerifiedRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        deferred("read.serve_peer")
    }
}
#[cfg(test)]
mod tests { /* Multi-node version consistency, empty bootstrap, pinned expiry, copy-only. */
}
