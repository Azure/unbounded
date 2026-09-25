//! Version 1 little-endian encrypted records. Header SHA-256 is framing integrity;
//! payload integrity remains AEAD at the fill boundary. Padding is never returned.
use super::{
    direct::{AlignedBuffer, DirectAlignment, DirectExtent},
    segment::Generation,
};
use crate::{
    error::{Error, Result},
    memory::page::CiphertextCopy,
    model::{
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::{CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag},
        metadata::VersionMetadata,
        range::PAGE_BYTES,
    },
    runtime::reactor::IoBuffer,
};
use sha2::{Digest, Sha256};
pub const FORMAT_VERSION: u32 = 1;
pub const MAX_ID_BYTES: usize = 4096;
pub const MAX_ETAG_BYTES: usize = 8192;
pub const MAX_HEADER_BYTES: usize = 16384;
const MAGIC: &[u8; 8] = b"RCRPAGE1";
#[derive(Clone, Debug)]
pub struct RecordHeader {
    pub format_version: u32,
    pub generation: Generation,
    pub envelope: PageEnvelope,
    pub metadata: VersionMetadata,
    pub logical_bytes: u64,
    pub extent: DirectExtent,
}
pub struct EncodedRecord {
    pub header: RecordHeader,
    pub buffer: AlignedBuffer,
}
pub struct DecodedRecord {
    pub header: RecordHeader,
    pub ciphertext: std::ops::Range<usize>,
}
pub struct RecordCodec;
impl RecordCodec {
    fn header_bytes(&self, page: &CiphertextCopy, generation: Generation) -> Result<Vec<u8>> {
        let envelope = page.ciphertext.envelope();
        let metadata = page.metadata.immutable();
        metadata.validate_page(envelope)?;
        if generation.0 == 0
            || envelope.plaintext_length == 0
            || u64::from(envelope.plaintext_length) > PAGE_BYTES
            || envelope.ciphertext_length
                != envelope
                    .plaintext_length
                    .checked_add(16)
                    .ok_or(Error::CorruptRecord)?
            || page.ciphertext.bytes().len() != envelope.ciphertext_length as usize
        {
            return Err(Error::CorruptRecord);
        }
        let cache = envelope.page.version.object.cache.0.as_bytes();
        let etag = envelope.page.version.etag.as_bytes();
        if cache.is_empty()
            || cache.len() > MAX_ID_BYTES
            || etag.is_empty()
            || etag.len() > MAX_ETAG_BYTES
        {
            return Err(Error::CorruptRecord);
        }
        let mut out = Vec::with_capacity(MAX_HEADER_BYTES);
        out.extend_from_slice(MAGIC);
        out.extend_from_slice(&FORMAT_VERSION.to_le_bytes());
        out.extend_from_slice(&0u32.to_le_bytes());
        out.extend_from_slice(&generation.0.to_le_bytes());
        out.extend_from_slice(&metadata.length.to_le_bytes());
        out.extend_from_slice(&envelope.page.number.0.to_le_bytes());
        out.extend_from_slice(&envelope.plaintext_length.to_le_bytes());
        out.extend_from_slice(&envelope.ciphertext_length.to_le_bytes());
        out.extend_from_slice(&envelope.key_id.0);
        out.extend_from_slice(&envelope.nonce.0);
        out.extend_from_slice(&envelope.page.version.object.key.0);
        out.extend_from_slice(&(cache.len() as u32).to_le_bytes());
        out.extend_from_slice(&(etag.len() as u32).to_le_bytes());
        out.extend_from_slice(cache);
        out.extend_from_slice(etag);
        let len = out.len() + 32;
        if len > MAX_HEADER_BYTES {
            return Err(Error::CorruptRecord);
        }
        out[12..16].copy_from_slice(&(len as u32).to_le_bytes());
        let digest = Sha256::digest(&out);
        out.extend_from_slice(&digest);
        Ok(out)
    }
    pub fn logical_length(&self, page: &CiphertextCopy) -> Result<usize> {
        self.header_bytes(page, Generation(1))?
            .len()
            .checked_add(page.ciphertext.bytes().len())
            .ok_or(Error::CorruptRecord)
    }
    pub fn encode(
        &self,
        page: &CiphertextCopy,
        generation: Generation,
        alignment: DirectAlignment,
        buffer: AlignedBuffer,
    ) -> Result<EncodedRecord> {
        self.encode_at(page, generation, alignment, 0, buffer)
    }
    pub fn encode_at(
        &self,
        page: &CiphertextCopy,
        generation: Generation,
        alignment: DirectAlignment,
        offset: u64,
        mut buffer: AlignedBuffer,
    ) -> Result<EncodedRecord> {
        let header = self.header_bytes(page, generation)?;
        let logical_bytes = header.len() + page.ciphertext.bytes().len();
        let extent = alignment.extent(offset, logical_bytes)?;
        alignment.check(extent, &buffer)?;
        let bytes = buffer.bytes_mut()?;
        bytes.fill(0);
        bytes[..header.len()].copy_from_slice(&header);
        bytes[header.len()..logical_bytes].copy_from_slice(page.ciphertext.bytes());
        Ok(EncodedRecord {
            header: RecordHeader {
                format_version: FORMAT_VERSION,
                generation,
                envelope: page.ciphertext.envelope().clone(),
                metadata: page.metadata.immutable(),
                logical_bytes: logical_bytes as u64,
                extent,
            },
            buffer,
        })
    }
    pub fn parse(&self, buffer: &AlignedBuffer, extent: DirectExtent) -> Result<DecodedRecord> {
        self.parse_bytes(buffer.bytes()?, extent)
    }
    pub fn parse_bytes(&self, bytes: &[u8], extent: DirectExtent) -> Result<DecodedRecord> {
        if bytes.len() != extent.length() {
            return Err(Error::CorruptRecord);
        }
        let mut r = Decoder { bytes, at: 0 };
        if r.take(8)? != MAGIC || r.u32()? != FORMAT_VERSION {
            return Err(Error::CorruptRecord);
        }
        let header_len = r.u32()? as usize;
        if !(160..=MAX_HEADER_BYTES).contains(&header_len) || header_len > bytes.len() {
            return Err(Error::CorruptRecord);
        }
        let digest = Sha256::digest(&bytes[..header_len - 32]);
        if digest[..] != bytes[header_len - 32..header_len] {
            return Err(Error::CorruptRecord);
        }
        r.bytes = &bytes[..header_len - 32];
        let generation = Generation(r.u64()?);
        let length = r.u64()?;
        let number = PageNumber(r.u64()?);
        let plaintext_length = r.u32()?;
        let ciphertext_length = r.u32()?;
        let key_id = KeyId(r.array()?);
        let nonce = Nonce(r.array()?);
        let key = CacheKey(r.array()?);
        let cache_len = r.u32()? as usize;
        let etag_len = r.u32()? as usize;
        if cache_len == 0 || cache_len > MAX_ID_BYTES || etag_len == 0 || etag_len > MAX_ETAG_BYTES
        {
            return Err(Error::CorruptRecord);
        }
        let cache = CacheId(
            std::str::from_utf8(r.take(cache_len)?)
                .map_err(|_| Error::CorruptRecord)?
                .to_owned(),
        );
        let etag = StrongEtag::parse(r.take(etag_len)?).map_err(|_| Error::CorruptRecord)?;
        if r.at != r.bytes.len()
            || generation.0 == 0
            || plaintext_length == 0
            || u64::from(plaintext_length) > PAGE_BYTES
            || plaintext_length.checked_add(16) != Some(ciphertext_length)
        {
            return Err(Error::CorruptRecord);
        }
        let version = ObjectVersion {
            object: ObjectId { cache, key },
            etag,
        };
        let metadata = VersionMetadata {
            version: version.clone(),
            length,
        };
        let envelope = PageEnvelope {
            page: PageId { version, number },
            key_id,
            nonce,
            plaintext_length,
            ciphertext_length,
        };
        metadata.validate_page(&envelope)?;
        let logical_bytes = header_len
            .checked_add(ciphertext_length as usize)
            .ok_or(Error::CorruptRecord)?;
        if logical_bytes > bytes.len() {
            return Err(Error::CorruptRecord);
        }
        Ok(DecodedRecord {
            header: RecordHeader {
                format_version: FORMAT_VERSION,
                generation,
                envelope,
                metadata,
                logical_bytes: logical_bytes as u64,
                extent,
            },
            ciphertext: header_len..logical_bytes,
        })
    }
    pub fn decode(&self, buffer: &AlignedBuffer, expected: &RecordHeader) -> Result<PageEnvelope> {
        let actual = self.parse(buffer, expected.extent)?.header;
        if actual.format_version != expected.format_version
            || actual.generation != expected.generation
            || actual.envelope != expected.envelope
            || actual.metadata != expected.metadata
            || actual.logical_bytes != expected.logical_bytes
        {
            return Err(Error::CorruptRecord);
        }
        Ok(actual.envelope)
    }
}
struct Decoder<'a> {
    bytes: &'a [u8],
    at: usize,
}
impl<'a> Decoder<'a> {
    fn take(&mut self, len: usize) -> Result<&'a [u8]> {
        let end = self.at.checked_add(len).ok_or(Error::CorruptRecord)?;
        let value = self.bytes.get(self.at..end).ok_or(Error::CorruptRecord)?;
        self.at = end;
        Ok(value)
    }
    fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::CorruptRecord)
    }
    fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.array()?))
    }
    fn u64(&mut self) -> Result<u64> {
        Ok(u64::from_le_bytes(self.array()?))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn malformed_frames_are_rejected_without_unbounded_allocations() {
        for len in [1, 16, 512] {
            let bytes = vec![0; len];
            assert!(
                RecordCodec
                    .parse_bytes(&bytes, DirectExtent::checked(0, len).unwrap())
                    .is_err()
            );
        }
        let mut bytes = vec![0; 512];
        bytes[..8].copy_from_slice(MAGIC);
        bytes[8..12].copy_from_slice(&1u32.to_le_bytes());
        bytes[12..16].copy_from_slice(&u32::MAX.to_le_bytes());
        assert!(
            RecordCodec
                .parse_bytes(&bytes, DirectExtent::checked(0, 512).unwrap())
                .is_err()
        );
    }
}
