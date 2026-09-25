//! Versioned encrypted record headers and padded on-disk framing.
use super::{
    direct::{AlignedBuffer, DirectAlignment, DirectExtent},
    segment::Generation,
};
use crate::{
    error::{Result, pending},
    memory::page::CiphertextCopy,
    model::{envelope::PageEnvelope, metadata::VersionMetadata},
};
pub struct RecordHeader {
    pub format_version: u32,
    pub generation: Generation,
    pub envelope: PageEnvelope,
    /// Must match the index's immutable descriptor and the envelope's full version.
    pub metadata: VersionMetadata,
    /// Header plus ciphertext, excluding direct-I/O padding.
    pub logical_bytes: u64,
    pub extent: DirectExtent,
}
pub struct EncodedRecord {
    pub header: RecordHeader,
    pub buffer: AlignedBuffer,
}
pub struct RecordCodec;
impl RecordCodec {
    /// Preserve ciphertext exactly; authenticate only the envelope's payload bytes.
    pub fn encode(
        &self,
        _page: &CiphertextCopy,
        _generation: Generation,
        _alignment: DirectAlignment,
        _buffer: AlignedBuffer,
    ) -> Result<EncodedRecord> {
        pending("record.encode")
    }
    pub fn decode(
        &self,
        _buffer: &AlignedBuffer,
        _expected: &RecordHeader,
    ) -> Result<PageEnvelope> {
        pending("record.decode")
    }
}
#[cfg(test)]
mod tests { /* Short final pages, padded framing, torn headers, generation mismatch. */
}
