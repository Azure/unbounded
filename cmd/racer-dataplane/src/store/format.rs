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
const HEADER_PREFIX_BYTES: usize = 128;
const HEADER_DIGEST_BYTES: usize = 32;
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
struct RecordLayout {
    header_bytes: usize,
    logical_bytes: usize,
}
impl RecordCodec {
    fn layout(&self, page: &CiphertextCopy, generation: Generation) -> Result<RecordLayout> {
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
        let header_bytes = HEADER_PREFIX_BYTES
            .checked_add(cache.len())
            .and_then(|len| len.checked_add(etag.len()))
            .and_then(|len| len.checked_add(HEADER_DIGEST_BYTES))
            .filter(|&len| len <= MAX_HEADER_BYTES)
            .ok_or(Error::CorruptRecord)?;
        let logical_bytes = header_bytes
            .checked_add(page.ciphertext.bytes().len())
            .ok_or(Error::CorruptRecord)?;
        Ok(RecordLayout {
            header_bytes,
            logical_bytes,
        })
    }
    fn header_bytes(
        &self,
        page: &CiphertextCopy,
        generation: Generation,
        layout: &RecordLayout,
    ) -> Vec<u8> {
        let envelope = page.ciphertext.envelope();
        let cache = envelope.page.version.object.cache.0.as_bytes();
        let etag = envelope.page.version.etag.as_bytes();
        let mut out = Vec::with_capacity(layout.header_bytes);
        out.extend_from_slice(MAGIC);
        out.extend_from_slice(&FORMAT_VERSION.to_le_bytes());
        out.extend_from_slice(&(layout.header_bytes as u32).to_le_bytes());
        out.extend_from_slice(&generation.0.to_le_bytes());
        out.extend_from_slice(&page.metadata.length.to_le_bytes());
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
        debug_assert_eq!(out.len() + HEADER_DIGEST_BYTES, layout.header_bytes);
        let digest = Sha256::digest(&out);
        out.extend_from_slice(&digest);
        out
    }
    pub fn logical_length(&self, page: &CiphertextCopy) -> Result<usize> {
        Ok(self.layout(page, Generation(1))?.logical_bytes)
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
        let layout = self.layout(page, generation)?;
        let logical_bytes = layout.logical_bytes;
        let extent = alignment.extent(offset, logical_bytes)?;
        alignment.check(extent, &buffer)?;
        let header = self.header_bytes(page, generation, &layout);
        let bytes = buffer.bytes_mut()?;
        bytes[..header.len()].copy_from_slice(&header);
        bytes[header.len()..logical_bytes].copy_from_slice(page.ciphertext.bytes());
        bytes[logical_bytes..].fill(0);
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
    use crate::{
        memory::pool::{CiphertextBytes, CiphertextPage},
        model::{limits::ResourceClass, metadata::ExpiresAt},
        runtime::admission::Admission,
    };
    use std::{sync::Arc, time::UNIX_EPOCH};

    fn admission() -> Admission {
        Admission::new(crate::test_support::cluster::config(false).limits)
    }

    fn page(
        admission: &Admission,
        length: usize,
        number: u64,
        cache: &str,
        etag: &str,
    ) -> CiphertextCopy {
        let metadata = VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(cache.into()),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length: number * PAGE_BYTES + length as u64,
        };
        CiphertextCopy {
            ciphertext: CiphertextPage {
                inner: Arc::new(CiphertextBytes {
                    envelope: PageEnvelope {
                        page: PageId {
                            version: metadata.version.clone(),
                            number: PageNumber(number),
                        },
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: length as u32 + 16,
                    },
                    bytes: vec![2; length + 16],
                    reservation: admission
                        .reserve(None, ResourceClass::Ciphertext, length + 16)
                        .unwrap(),
                }),
            },
            metadata: metadata.for_pin(),
        }
    }

    fn buffer(admission: &Admission, alignment: DirectAlignment, length: usize) -> AlignedBuffer {
        alignment
            .allocate(
                length,
                admission
                    .reserve(None, ResourceClass::Ciphertext, length)
                    .unwrap(),
            )
            .unwrap()
    }

    fn unhex(hex: &str) -> Vec<u8> {
        hex.as_bytes()
            .chunks_exact(2)
            .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap())
            .collect()
    }

    // Independently packed little-endian fixture, verified against the pre-change
    // serializer. Includes the header digest and ciphertext; remaining bytes are zero.
    const GOLDEN_LOGICAL: &str = concat!(
        "524352504147453101000000a900000007000000000000000300000000000000",
        "0000000000000000030000001300000001010101010101010101010101010101",
        "020202020202020202020202020202020202020202020202",
        "0303030303030303030303030303030303030303030303030303030303030303",
        "0500000004000000636163686522763122",
        "c5f4f39966532d8ab0ede8139c81d2a7368448b01ac36f50e5a2d39078b51b3c",
        "02020202020202020202020202020202020202",
    );
    const GOLDEN_RECORD_SHA256: &str =
        "5825196396c3fc29342422518a98d9fe550bcdb05701b97aadcb9c02271f3f03";

    #[test]
    fn legacy_wire_bytes_and_reused_padding_across_page_and_alignment_boundaries() {
        let admission = admission();
        // Normal header is 169 bytes, plus a 16-byte tag. Straddle both units.
        for (length, number, maximum, geometry, offset) in [
            (1, 0, false, (512, 512, 512), 0),
            (326, 0, false, (512, 512, 512), 512),
            (327, 0, false, (512, 512, 512), 1024),
            (328, 0, false, (512, 512, 512), 512),
            (3910, 0, false, (4096, 4096, 4096), 4096),
            (3911, 0, false, (4096, 4096, 4096), 8192),
            (3912, 0, false, (4096, 4096, 4096), 4096),
            (PAGE_BYTES as usize, 0, false, (4096, 512, 4096), 512),
            (PAGE_BYTES as usize, 1, true, (512, 4096, 512), 4096),
            (3, 2, true, (4096, 512, 4096), 1536),
        ] {
            // UTF-8 cache IDs are bounded in bytes; strong ETags are ASCII-only.
            let cache = if maximum {
                "é".repeat(MAX_ID_BYTES / 2)
            } else {
                "cache".into()
            };
            let etag = if maximum {
                "x".repeat(MAX_ETAG_BYTES - 2)
            } else {
                "v1".into()
            };
            let mut page = page(&admission, length, number, &cache, &etag);
            let alignment = DirectAlignment::validate(geometry.0, geometry.1, geometry.2).unwrap();
            let logical = RecordCodec.logical_length(&page).unwrap();
            assert_eq!(logical, LegacyCodec.logical_length(&page).unwrap());
            assert_eq!(
                logical,
                128 + cache.len() + etag.len() + 2 + 32 + length + 16
            );
            let extent = alignment.extent(offset, logical).unwrap();
            let mut staging = buffer(&admission, alignment, extent.length());
            staging.bytes_mut().unwrap().fill(0xa5);
            let mut encoded = RecordCodec
                .encode_at(&page, Generation(u64::MAX), alignment, offset, staging)
                .unwrap();
            let legacy = LegacyCodec
                .encode_at(
                    &page,
                    Generation(u64::MAX),
                    alignment,
                    offset,
                    buffer(&admission, alignment, extent.length()),
                )
                .unwrap();
            assert_eq!(
                encoded.buffer.bytes().unwrap(),
                legacy.buffer.bytes().unwrap()
            );
            assert_eq!(encoded.header.extent, extent);
            assert_eq!(
                RecordCodec
                    .decode(&encoded.buffer, &encoded.header)
                    .unwrap(),
                *page.ciphertext.envelope()
            );
            drop(legacy);

            // Reuse an actual record, shortening ciphertext into the old payload
            // when possible. This catches stale bytes at the new padding boundary.
            let inner = Arc::get_mut(&mut page.ciphertext.inner).unwrap();
            if length > 1 {
                inner.envelope.plaintext_length -= 1;
                inner.envelope.ciphertext_length -= 1;
                inner.bytes.pop();
                page.metadata.length -= 1;
            }
            inner.bytes.fill(0x6b);
            let shortened_logical = logical - usize::from(length > 1);
            // At a rounding boundary, use a same-length replacement instead.
            if alignment.extent(offset, shortened_logical).unwrap() != extent {
                inner.bytes.push(0x6b);
                inner.envelope.plaintext_length += 1;
                inner.envelope.ciphertext_length += 1;
                page.metadata.length += 1;
            }
            encoded = RecordCodec
                .encode_at(&page, Generation(9), alignment, offset, encoded.buffer)
                .unwrap();
            let bytes = encoded.buffer.bytes().unwrap();
            let decoded = RecordCodec.parse(&encoded.buffer, extent).unwrap();
            assert_eq!(&bytes[decoded.ciphertext.clone()], page.ciphertext.bytes());
            assert!(bytes[decoded.ciphertext.end..].iter().all(|&b| b == 0));
            assert_eq!(decoded.header.generation, Generation(9));
            assert_eq!(decoded.header.metadata, page.metadata.immutable());
        }
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn malformed_inputs_keep_legacy_errors_before_geometry_checks() {
        let admission = admission();
        let alignment = DirectAlignment::validate(512, 512, 512).unwrap();
        let mutations: &[fn(&mut CiphertextCopy)] = &[
            |p| p.metadata.length = 0,
            |p| p.metadata.length = 4,
            |p| p.metadata.length = crate::model::MAX_WIRE_INTEGER + 1,
            |p| p.metadata.version.object.key = CacheKey([4; 32]),
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .number = PageNumber(u64::MAX)
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .number = PageNumber(1)
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .plaintext_length = 0
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .plaintext_length = PAGE_BYTES as u32 + 1
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .plaintext_length = u32::MAX
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .ciphertext_length = 18
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner).unwrap().bytes.pop();
            },
            |p| Arc::get_mut(&mut p.ciphertext.inner).unwrap().bytes.push(0),
            |p| {
                p.metadata.version.object.cache.0.clear();
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .version = p.metadata.version.clone();
            },
            |p| {
                p.metadata.version.object.cache.0 = "é".repeat(MAX_ID_BYTES / 2 + 1);
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .version = p.metadata.version.clone();
            },
        ];
        for mutate in mutations {
            let mut page = page(&admission, 3, 0, "cache", "v1");
            mutate(&mut page);
            assert_eq!(RecordCodec.logical_length(&page), Err(Error::CorruptRecord));
            assert_eq!(
                RecordCodec.logical_length(&page),
                LegacyCodec.logical_length(&page)
            );
            let baseline = admission.used(ResourceClass::Ciphertext);
            for generation in [Generation(0), Generation(1)] {
                // Bad offset/size must not mask the record error.
                let actual = RecordCodec
                    .encode_at(
                        &page,
                        generation,
                        alignment,
                        1,
                        buffer(&admission, alignment, 1024),
                    )
                    .err();
                let legacy = LegacyCodec
                    .encode_at(
                        &page,
                        generation,
                        alignment,
                        1,
                        buffer(&admission, alignment, 1024),
                    )
                    .err();
                assert_eq!(actual, Some(Error::CorruptRecord));
                assert_eq!(actual, legacy);
                assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
            }
        }
    }

    #[test]
    fn sizing_uses_generation_one_and_ignores_historical_freshness() {
        let admission = admission();
        let mut page = page(&admission, 3, 0, "cache", "v1");
        let alignment = DirectAlignment::validate(512, 512, 512).unwrap();
        for expiry in [
            UNIX_EPOCH,
            UNIX_EPOCH - std::time::Duration::from_secs(1),
            UNIX_EPOCH + std::time::Duration::from_nanos(1),
        ] {
            page.metadata.expires_at = ExpiresAt(expiry);
            assert_eq!(RecordCodec.logical_length(&page), Ok(188));
            assert_eq!(
                RecordCodec.logical_length(&page),
                LegacyCodec.logical_length(&page)
            );
            let encoded = RecordCodec
                .encode(
                    &page,
                    Generation(7),
                    alignment,
                    buffer(&admission, alignment, 512),
                )
                .unwrap();
            assert_eq!(
                &encoded.buffer.bytes().unwrap()[..188],
                unhex(GOLDEN_LOGICAL)
            );
            assert_eq!(
                RecordCodec
                    .encode_at(
                        &page,
                        Generation(0),
                        alignment,
                        1,
                        buffer(&admission, alignment, 512)
                    )
                    .err(),
                Some(Error::CorruptRecord)
            );
        }
    }

    #[test]
    fn geometry_failures_release_owned_and_retained_charges() {
        let admission = admission();
        let page = page(&admission, 3, 0, "cache", "v1");
        let baseline = admission.used(ResourceClass::Ciphertext);
        let normal = DirectAlignment::validate(512, 512, 512).unwrap();
        for (alignment, offset, length, expected) in [
            (normal, 1, 512, Error::InvalidConfiguration),
            (normal, 0, 1024, Error::InvalidConfiguration),
            (normal, u64::MAX - 511, 512, Error::CorruptRecord),
            (
                DirectAlignment::validate(512, 512, usize::MAX).unwrap(),
                0,
                512,
                Error::InvalidConfiguration,
            ),
            (
                DirectAlignment::validate(512, 1, usize::MAX).unwrap(),
                0,
                512,
                Error::InvalidConfiguration,
            ),
        ] {
            let mut staging = buffer(&admission, normal, length);
            staging.bytes_mut().unwrap().fill(0xa5);
            staging.retain_charge(std::rc::Rc::new(
                admission
                    .reserve(None, ResourceClass::Ciphertext, 17)
                    .unwrap(),
            ));
            assert_eq!(
                RecordCodec
                    .encode_at(&page, Generation(1), alignment, offset, staging)
                    .err(),
                Some(expected)
            );
            assert_eq!(
                LegacyCodec
                    .encode_at(
                        &page,
                        Generation(1),
                        alignment,
                        offset,
                        buffer(&admission, normal, length)
                    )
                    .err(),
                Some(expected)
            );
            assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
        }
    }

    /// CPU/memory microbenchmark only: no slabs, files, reactor, or storage I/O.
    /// Run with cargo test --release --lib memory_only_codec_benchmark -- --ignored --nocapture.
    #[test]
    #[ignore = "bounded release-only memory benchmark"]
    fn memory_only_codec_benchmark() {
        use std::{hint::black_box, time::Instant};
        assert!(!cfg!(debug_assertions), "run with --release");
        let admission = admission();
        let alignment = DirectAlignment::validate(4096, 4096, 4096).unwrap();
        println!(
            "memory-only; 5 samples, alternating legacy/new order; median [min,max] ns/record; GiB/s uses logical record bytes (sizing does not touch payload)"
        );
        for (size, length) in [("tiny", 3), ("full", PAGE_BYTES as usize)] {
            for (ids, cache, etag) in [
                ("normal", "cache".into(), "v1".into()),
                (
                    "max",
                    "c".repeat(MAX_ID_BYTES),
                    "x".repeat(MAX_ETAG_BYTES - 2),
                ),
            ] {
                let page = page(&admission, length, 0, &cache, &etag);
                let logical = RecordCodec.logical_length(&page).unwrap();
                let extent = alignment.extent(4096, logical).unwrap();
                let mut staging = Some(buffer(&admission, alignment, extent.length()));
                staging.as_mut().unwrap().bytes_mut().unwrap().fill(0xa5);
                for mode in ["sizing", "encode-reuse", "two-sizing+encode"] {
                    let iterations = if mode == "sizing" || size == "tiny" {
                        2000
                    } else {
                        32
                    };
                    let mut run = |legacy: bool, count: usize| {
                        let start = Instant::now();
                        for _ in 0..count {
                            let page = black_box(&page);
                            let sizing_count = match mode {
                                "sizing" => 1,
                                "two-sizing+encode" => 2,
                                _ => 0,
                            };
                            for _ in 0..sizing_count {
                                black_box(
                                    if legacy {
                                        LegacyCodec.logical_length(black_box(page))
                                    } else {
                                        RecordCodec.logical_length(black_box(page))
                                    }
                                    .unwrap(),
                                );
                            }
                            if mode != "sizing" {
                                let buffer = black_box(staging.take().unwrap());
                                let encoded = if legacy {
                                    LegacyCodec.encode_at(
                                        page,
                                        black_box(Generation(7)),
                                        black_box(alignment),
                                        black_box(4096),
                                        buffer,
                                    )
                                } else {
                                    RecordCodec.encode_at(
                                        page,
                                        black_box(Generation(7)),
                                        black_box(alignment),
                                        black_box(4096),
                                        buffer,
                                    )
                                }
                                .unwrap();
                                black_box(encoded.buffer.bytes().unwrap());
                                black_box(&encoded.header);
                                staging = Some(encoded.buffer);
                            }
                        }
                        start.elapsed().as_nanos() as f64 / count as f64
                    };
                    for legacy in [true, false] {
                        run(legacy, iterations / 4);
                    }
                    let mut samples = [Vec::new(), Vec::new()];
                    for sample in 0..5 {
                        for index in if sample % 2 == 0 { [0, 1] } else { [1, 0] } {
                            samples[index].push(run(index == 0, iterations));
                        }
                    }
                    for (index, values) in samples.iter_mut().enumerate() {
                        values.sort_by(f64::total_cmp);
                        let ns = values[2];
                        let gib = logical as f64 / (1u64 << 30) as f64 / (ns / 1e9);
                        println!(
                            "{size}/{ids} {mode} {} n={iterations}: {ns:.1} [{:.1},{:.1}] ns/record, {gib:.3} logical GiB/s",
                            if index == 0 { "legacy" } else { "new" },
                            values[0],
                            values[4]
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn frozen_v1_record_and_hash() {
        let admission = admission();
        let page = page(&admission, 3, 0, "cache", "v1");
        let alignment = DirectAlignment::validate(512, 512, 512).unwrap();
        let encoded = RecordCodec
            .encode(
                &page,
                Generation(7),
                alignment,
                buffer(&admission, alignment, 512),
            )
            .unwrap();
        let mut expected = unhex(GOLDEN_LOGICAL);
        assert_eq!(RecordCodec.logical_length(&page).unwrap(), expected.len());
        expected.resize(512, 0);
        assert_eq!(encoded.buffer.bytes().unwrap(), expected);
        assert_eq!(
            Sha256::digest(&expected).as_slice(),
            unhex(GOLDEN_RECORD_SHA256)
        );
        let parsed = RecordCodec
            .parse(&encoded.buffer, encoded.header.extent)
            .unwrap();
        assert_eq!(parsed.header.generation, Generation(7));
        assert_eq!(parsed.ciphertext, 169..188);
        assert_eq!(parsed.header.metadata, page.metadata.immutable());
    }

    // Frozen pre-optimization implementation. Keep independent of the new layout
    // and serializer: both regression comparisons and the memory benchmark use it.
    struct LegacyCodec;
    impl LegacyCodec {
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
        fn logical_length(&self, page: &CiphertextCopy) -> Result<usize> {
            self.header_bytes(page, Generation(1))?
                .len()
                .checked_add(page.ciphertext.bytes().len())
                .ok_or(Error::CorruptRecord)
        }
        fn encode_at(
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
    }
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
