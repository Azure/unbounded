//! HEAD/initial-GET validation; empty objects produce metadata without a page.
use super::page::OriginPage;
use crate::{
    error::{Result, pending},
    http::codec::MessageHead,
    model::{identity::ObjectId, metadata::ObjectMetadata},
};
pub struct MetadataReply {
    pub metadata: ObjectMetadata,
    pub page_zero: Option<OriginPage>,
}
pub fn validate(_head: &MessageHead, _object: &ObjectId) -> Result<ObjectMetadata> {
    pending("origin.validate_metadata")
}
#[cfg(test)]
mod tests { /* Strong ETag/TTL, empty objects, bounded bootstrap version-change retry. */
}
