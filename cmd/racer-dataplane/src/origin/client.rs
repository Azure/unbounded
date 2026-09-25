//! Per-cache local adapter operations requiring validated candidate authority.
//!
//! Credentials are only origin-fetch context, never Racer authorization. Do not
//! persist headers or retain them in pooled connections after an operation ends.
use super::{metadata::MetadataReply, page::OriginPage};
use crate::{
    control::snapshot::SnapshotStore,
    error::{Operation, deferred},
    http::{io::HttpIo, pool::HttpPool},
    model::{context::OriginContext, identity::PageId, metadata::MetadataSelector},
    read::candidates::OriginAuthority,
    runtime::deadline::RequestScope,
};
use std::rc::Rc;
pub trait Origin {
    fn metadata<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply>;
    fn page<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage>;
}
pub struct OriginClient {
    snapshots: Rc<SnapshotStore>,
    pool: Rc<HttpPool>,
    io: Rc<HttpIo>,
}
impl OriginClient {
    pub fn new(snapshots: Rc<SnapshotStore>, pool: Rc<HttpPool>, io: Rc<HttpIo>) -> Self {
        Self {
            snapshots,
            pool,
            io,
        }
    }
}
impl Origin for OriginClient {
    fn metadata<'a>(
        &'a self,
        _authority: &'a OriginAuthority,
        _context: &'a OriginContext,
        _selector: MetadataSelector,
        _scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        deferred("origin.metadata")
    }
    fn page<'a>(
        &'a self,
        _authority: &'a OriginAuthority,
        _context: &'a OriginContext,
        _page: &'a PageId,
        _scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        deferred("origin.page")
    }
}
#[cfg(test)]
mod tests { /* Exact key/metadata/auth forwarding, failed credentials, pool cleanup. */
}
