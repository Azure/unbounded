//! Scripted adapter for changed ETags, credential rejection, and malformed bodies.
use crate::{
    error::{Operation, deferred},
    model::{context::OriginContext, identity::PageId, metadata::MetadataSelector},
    origin::{client::Origin, metadata::MetadataReply, page::OriginPage},
    read::candidates::OriginAuthority,
    runtime::deadline::RequestScope,
};
pub struct ScriptedOrigin;
impl Origin for ScriptedOrigin {
    fn metadata<'a>(
        &'a self,
        _authority: &'a OriginAuthority,
        _context: &'a OriginContext,
        _selector: MetadataSelector,
        _scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        deferred("test.origin.metadata")
    }
    fn page<'a>(
        &'a self,
        _authority: &'a OriginAuthority,
        _context: &'a OriginContext,
        _page: &'a PageId,
        _scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        deferred("test.origin.page")
    }
}
