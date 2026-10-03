//! Versioned little-endian encrypted records. Header SHA-256 is framing integrity;
//! payload integrity remains AEAD at the fill boundary. Padding is never returned.
use crate::runtime::admission::AdmissionPolicy;
use crate::{
    error::{Error, Result},
    memory::page::CiphertextCopy,
    model::{
        CacheId, CacheKey, KeyId, Nonce, ObjectId, ObjectVersion, PageEnvelope, PageId, PageNumber,
        StrongEtag, VersionMetadata,
    },
};
use page_alloc::{AlignedBuffer, Alignment, Extent, Generation};
use sha2::{Digest, Sha256};
// Version 4 is the sole format: mandatory CRC-64/XZ plus optional content type.
pub const FORMAT_VERSION: u32 = 4;
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
    pub extent: Extent,
}
pub struct EncodedRecord {
    pub header: RecordHeader,
    pub buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
}
pub struct DecodedRecord {
    pub header: RecordHeader,
    pub ciphertext: std::ops::Range<usize>,
    pub checksum: u64,
}
struct RecordLayout {
    header_bytes: usize,
    logical_bytes: usize,
}
fn layout(page: &CiphertextCopy, generation: Generation) -> Result<RecordLayout> {
    let envelope = page.ciphertext.envelope();
    let metadata = page.metadata.immutable();
    metadata.validate_page(envelope)?;
    if generation.0 == 0 || page.ciphertext.bytes().len() != envelope.ciphertext_length as usize {
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
        .checked_add(8)
        .ok_or(Error::CorruptRecord)?
        .checked_add(cache.len())
        .and_then(|len| len.checked_add(etag.len()))
        .and_then(|len| {
            len.checked_add(
                metadata
                    .content_type
                    .as_ref()
                    .map_or(4, |v| 4 + v.as_bytes().len()),
            )
        })
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
fn header_bytes(page: &CiphertextCopy, generation: Generation, layout: &RecordLayout) -> Vec<u8> {
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
    out.extend_from_slice(&page.ciphertext.checksum().to_le_bytes());
    out.extend_from_slice(
        &(page
            .metadata
            .content_type
            .as_ref()
            .map_or(0, |v| v.as_bytes().len()) as u32)
            .to_le_bytes(),
    );
    if let Some(content_type) = &page.metadata.content_type {
        out.extend_from_slice(content_type.as_bytes());
    }
    debug_assert_eq!(out.len() + HEADER_DIGEST_BYTES, layout.header_bytes);
    let digest = Sha256::digest(&out);
    out.extend_from_slice(&digest);
    out
}
pub fn logical_length(page: &CiphertextCopy) -> Result<usize> {
    Ok(layout(page, Generation(1))?.logical_bytes)
}
pub fn encode(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
) -> Result<EncodedRecord> {
    encode_at(page, generation, alignment, 0, buffer)
}
pub fn encode_at(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    offset: u64,
    mut buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
) -> Result<EncodedRecord> {
    let layout = layout(page, generation)?;
    let logical_bytes = layout.logical_bytes;
    let extent = alignment.extent(offset, logical_bytes)?;
    alignment.check(extent, &buffer)?;
    let header = header_bytes(page, generation, &layout);
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
pub fn parse(
    buffer: &AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
    extent: Extent,
) -> Result<DecodedRecord> {
    parse_bytes(buffer.bytes()?, extent)
}
pub fn parse_bytes(bytes: &[u8], extent: Extent) -> Result<DecodedRecord> {
    if bytes.len() != extent.length() {
        return Err(Error::CorruptRecord);
    }
    let mut r = Decoder(bytes);
    if r.take(8)? != MAGIC {
        return Err(Error::CorruptRecord);
    }
    let format_version = r.u32()?;
    if format_version != FORMAT_VERSION {
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
    r.0 = &bytes[16..header_len - 32];
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
    if cache_len == 0 || cache_len > MAX_ID_BYTES || etag_len == 0 || etag_len > MAX_ETAG_BYTES {
        return Err(Error::CorruptRecord);
    }
    let cache = CacheId(
        std::str::from_utf8(r.take(cache_len)?)
            .map_err(|_| Error::CorruptRecord)?
            .to_owned(),
    );
    let etag = StrongEtag::parse(r.take(etag_len)?).map_err(|_| Error::CorruptRecord)?;
    let checksum = r.u64()?;
    let content_type_length = r.u32()? as usize;
    let content_type = if content_type_length == 0 {
        None
    } else {
        Some(
            crate::model::ContentType::parse(r.take(content_type_length)?)
                .map_err(|_| Error::CorruptRecord)?,
        )
    };
    if !r.0.is_empty() || generation.0 == 0 {
        return Err(Error::CorruptRecord);
    }
    let version = ObjectVersion {
        object: ObjectId { cache, key },
        etag,
    };
    let metadata = VersionMetadata {
        content_type,
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
            format_version,
            generation,
            envelope,
            metadata,
            logical_bytes: logical_bytes as u64,
            extent,
        },
        ciphertext: header_len..logical_bytes,
        checksum,
    })
}
pub fn decode(
    buffer: &AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
    expected: &RecordHeader,
) -> Result<PageEnvelope> {
    let actual = parse(buffer, expected.extent)?.header;
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
pub(super) struct Decoder<'a>(pub(super) &'a [u8]);
impl<'a> Decoder<'a> {
    pub(super) fn take(&mut self, len: usize) -> Result<&'a [u8]> {
        let (value, rest) = self.0.split_at_checked(len).ok_or(Error::CorruptRecord)?;
        self.0 = rest;
        Ok(value)
    }
    pub(super) fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::CorruptRecord)
    }
    pub(super) fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.array()?))
    }
    pub(super) fn u64(&mut self) -> Result<u64> {
        Ok(u64::from_le_bytes(self.array()?))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        memory::{CiphertextBytes, CiphertextPage},
        model::{ExpiresAt, PAGE_BYTES, ResourceClass},
    };
    use std::{sync::Arc, time::UNIX_EPOCH};

    fn admission() -> flow_control::Quotas<AdmissionPolicy> {
        flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ))
    }

    fn page(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        length: usize,
        number: u64,
        cache: &str,
        etag: &str,
    ) -> CiphertextCopy {
        let metadata = VersionMetadata {
            content_type: None,
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
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: std::sync::OnceLock::new(),
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

    fn buffer(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        alignment: Alignment,
        length: usize,
    ) -> AlignedBuffer<flow_control::Charge<AdmissionPolicy>> {
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
    fn current_wire_bytes_and_reused_padding_across_page_and_alignment_boundaries() {
        let admission = admission();
        // Normal v4 header is 181 bytes, plus a 16-byte tag. Straddle both units.
        for (length, number, maximum, geometry, offset) in [
            (1, 0, false, (512, 512, 512), 0),
            (314, 0, false, (512, 512, 512), 512),
            (315, 0, false, (512, 512, 512), 1024),
            (316, 0, false, (512, 512, 512), 512),
            (3898, 0, false, (4096, 4096, 4096), 4096),
            (3899, 0, false, (4096, 4096, 4096), 8192),
            (3900, 0, false, (4096, 4096, 4096), 4096),
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
            let alignment = Alignment::new(geometry.0, geometry.1, geometry.2).unwrap();
            let logical = logical_length(&page).unwrap();
            assert_eq!(
                logical,
                128 + 12 + cache.len() + etag.len() + 2 + 32 + length + 16
            );
            let extent = alignment.extent(offset, logical).unwrap();
            let mut staging = buffer(&admission, alignment, extent.length());
            staging.bytes_mut().unwrap().fill(0xa5);
            let mut encoded =
                encode_at(&page, Generation(u64::MAX), alignment, offset, staging).unwrap();
            assert_eq!(
                parse(&encoded.buffer, encoded.header.extent)
                    .unwrap()
                    .header
                    .envelope,
                *page.ciphertext.envelope()
            );
            assert_eq!(encoded.header.extent, extent);
            assert_eq!(
                decode(&encoded.buffer, &encoded.header).unwrap(),
                *page.ciphertext.envelope()
            );

            // Reuse an actual record, shortening ciphertext into the old payload
            // when possible. This catches stale bytes at the new padding boundary.
            let inner = Arc::get_mut(&mut page.ciphertext.inner).unwrap();
            inner.checksum.take();
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
            encoded = encode_at(&page, Generation(9), alignment, offset, encoded.buffer).unwrap();
            let bytes = encoded.buffer.bytes().unwrap();
            let decoded = parse(&encoded.buffer, extent).unwrap();
            assert_eq!(&bytes[decoded.ciphertext.clone()], page.ciphertext.bytes());
            assert!(bytes[decoded.ciphertext.end..].iter().all(|&b| b == 0));
            assert_eq!(decoded.header.generation, Generation(9));
            assert_eq!(decoded.header.metadata, page.metadata.immutable());
        }
        admission.reclaim_buffers();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn malformed_inputs_reject_record_errors_before_geometry_checks() {
        let admission = admission();
        let alignment = Alignment::new(512, 512, 512).unwrap();
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
            assert_eq!(logical_length(&page), Err(Error::CorruptRecord));
            let baseline = admission.used(ResourceClass::Ciphertext);
            for generation in [Generation(0), Generation(1)] {
                // Bad offset/size must not mask the record error.
                let actual = encode_at(
                    &page,
                    generation,
                    alignment,
                    1,
                    buffer(&admission, alignment, 1024),
                )
                .err();
                assert_eq!(actual, Some(Error::CorruptRecord));
                assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
            }
        }
    }

    #[test]
    fn sizing_uses_generation_one_and_ignores_historical_freshness() {
        let admission = admission();
        let mut page = page(&admission, 3, 0, "cache", "v1");
        let alignment = Alignment::new(512, 512, 512).unwrap();
        for expiry in [
            UNIX_EPOCH,
            UNIX_EPOCH + std::time::Duration::from_secs(1),
            UNIX_EPOCH + std::time::Duration::from_millis(1),
        ] {
            page.metadata.expires_at = ExpiresAt::from_system_time(expiry).unwrap();
            assert_eq!(logical_length(&page), Ok(200));
            let encoded = encode(
                &page,
                Generation(7),
                alignment,
                buffer(&admission, alignment, 512),
            )
            .unwrap();
            assert_eq!(
                parse(&encoded.buffer, encoded.header.extent)
                    .unwrap()
                    .header
                    .metadata,
                page.metadata.immutable()
            );
            assert_eq!(
                parse(&encoded.buffer, encoded.header.extent)
                    .unwrap()
                    .checksum,
                page.ciphertext.checksum()
            );
            assert_eq!(
                encode_at(
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
        let normal = Alignment::new(512, 512, 512).unwrap();
        for (alignment, offset, length, expected) in [
            (normal, 1, 512, Error::InvalidConfiguration),
            (normal, 0, 1024, Error::InvalidConfiguration),
            (normal, u64::MAX - 511, 512, Error::CorruptRecord),
            (
                Alignment::new(512, 512, usize::MAX).unwrap(),
                0,
                512,
                Error::InvalidConfiguration,
            ),
            (
                Alignment::new(512, 1, usize::MAX).unwrap(),
                0,
                512,
                Error::InvalidConfiguration,
            ),
        ] {
            let mut staging = buffer(&admission, normal, length);
            staging.bytes_mut().unwrap().fill(0xa5);
            staging.retain(std::rc::Rc::new(
                admission
                    .reserve(None, ResourceClass::Ciphertext, 17)
                    .unwrap(),
            ));
            assert_eq!(
                encode_at(&page, Generation(1), alignment, offset, staging).err(),
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
        let alignment = Alignment::new(4096, 4096, 4096).unwrap();
        println!(
            "memory-only; 5 samples; median [min,max] ns/record; GiB/s uses logical record bytes (sizing does not touch payload)"
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
                let logical = logical_length(&page).unwrap();
                let extent = alignment.extent(4096, logical).unwrap();
                let mut staging = Some(buffer(&admission, alignment, extent.length()));
                staging.as_mut().unwrap().bytes_mut().unwrap().fill(0xa5);
                for mode in ["sizing", "encode-reuse", "two-sizing+encode"] {
                    let iterations = if mode == "sizing" || size == "tiny" {
                        2000
                    } else {
                        32
                    };
                    let mut run = |count: usize| {
                        let start = Instant::now();
                        for _ in 0..count {
                            let page = black_box(&page);
                            let sizing_count = match mode {
                                "sizing" => 1,
                                "two-sizing+encode" => 2,
                                _ => 0,
                            };
                            for _ in 0..sizing_count {
                                black_box(logical_length(black_box(page)).unwrap());
                            }
                            if mode != "sizing" {
                                let buffer = black_box(staging.take().unwrap());
                                let encoded = encode_at(
                                    page,
                                    black_box(Generation(7)),
                                    black_box(alignment),
                                    black_box(4096),
                                    buffer,
                                )
                                .unwrap();
                                black_box(encoded.buffer.bytes().unwrap());
                                black_box(&encoded.header);
                                staging = Some(encoded.buffer);
                            }
                        }
                        start.elapsed().as_nanos() as f64 / count as f64
                    };
                    run(iterations / 4);
                    let mut values: Vec<_> = (0..5).map(|_| run(iterations)).collect();
                    values.sort_by(f64::total_cmp);
                    let ns = values[2];
                    let gib = logical as f64 / (1u64 << 30) as f64 / (ns / 1e9);
                    println!(
                        "{size}/{ids} {mode} n={iterations}: {ns:.1} [{:.1},{:.1}] ns/record, {gib:.3} logical GiB/s",
                        values[0], values[4]
                    );
                }
            }
        }
    }

    #[test]
    fn frozen_v1_record_is_rejected() {
        let mut expected = unhex(GOLDEN_LOGICAL);
        expected.resize(512, 0);
        assert_eq!(
            Sha256::digest(&expected).as_slice(),
            unhex(GOLDEN_RECORD_SHA256)
        );
        assert!(matches!(
            parse_bytes(&expected, Extent::new(0, 512).unwrap()),
            Err(Error::CorruptRecord)
        ));
    }

    #[test]
    fn current_records_preserve_bounded_content_type_and_reject_malformed_values() {
        let admission = admission();
        let mut page = page(&admission, 3, 0, "cache", "v1");
        page.metadata.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let encoded = encode(
            &page,
            Generation(7),
            alignment,
            buffer(&admission, alignment, 512),
        )
        .unwrap();
        let parsed = parse(&encoded.buffer, encoded.header.extent).unwrap();
        assert_eq!(parsed.header.format_version, FORMAT_VERSION);
        assert_eq!(parsed.header.metadata, page.metadata.immutable());
        assert_eq!(parsed.checksum, page.ciphertext.checksum());

        // Reconstruct the obsolete v2 layout: content type, without a CRC.
        let mut version_two = encoded.buffer.bytes().unwrap().to_vec();
        let old_header = u32::from_le_bytes(version_two[12..16].try_into().unwrap()) as usize;
        let crc_offset = 128 + "cache".len() + "\"v1\"".len();
        version_two.drain(crc_offset..crc_offset + 8);
        version_two.resize(512, 0);
        version_two[8..12].copy_from_slice(&2u32.to_le_bytes());
        let header = old_header - 8;
        version_two[12..16].copy_from_slice(&(header as u32).to_le_bytes());
        let digest = Sha256::digest(&version_two[..header - 32]);
        version_two[header - 32..header].copy_from_slice(&digest);
        assert!(matches!(
            parse_bytes(&version_two, encoded.header.extent),
            Err(Error::CorruptRecord)
        ));
        let mut corrupted = encoded.buffer.bytes().unwrap().to_vec();
        let start = corrupted
            .windows(10)
            .position(|w| w == b"text/plain")
            .unwrap();
        corrupted[start] = b'\r';
        let end = u32::from_le_bytes(corrupted[12..16].try_into().unwrap()) as usize;
        let digest = Sha256::digest(&corrupted[..end - 32]);
        corrupted[end - 32..end].copy_from_slice(&digest);
        assert!(parse_bytes(&corrupted, encoded.header.extent).is_err());
    }

    #[test]
    fn only_current_version_is_accepted_even_with_valid_header_digest() {
        let admission = admission();
        let page = page(&admission, 3, 0, "cache", "v1");
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let encoded = encode(
            &page,
            Generation(7),
            alignment,
            buffer(&admission, alignment, 512),
        )
        .unwrap();
        let original = encoded.buffer.bytes().unwrap();
        assert_eq!(&original[8..12], &4u32.to_le_bytes());
        let header_len = u32::from_le_bytes(original[12..16].try_into().unwrap()) as usize;
        let crc_offset = 128 + "cache".len() + "\"v1\"".len();
        assert_eq!(
            &original[crc_offset..crc_offset + 8],
            &page.ciphertext.checksum().to_le_bytes()
        );
        for version in [0u32, 1, 2, 3, 4, 5, u32::MAX] {
            let mut bytes = original.to_vec();
            bytes[8..12].copy_from_slice(&version.to_le_bytes());
            if version == 3 {
                // Reconstruct an actual v3 checksum, not just its version label.
                let mut ecma = 0u64;
                for byte in page.ciphertext.bytes() {
                    ecma ^= u64::from(*byte) << 56;
                    for _ in 0..8 {
                        ecma = (ecma << 1)
                            ^ if ecma >> 63 != 0 {
                                0x42f0_e1eb_a9ea_3693
                            } else {
                                0
                            };
                    }
                }
                assert_ne!(ecma, page.ciphertext.checksum());
                bytes[crc_offset..crc_offset + 8].copy_from_slice(&ecma.to_le_bytes());
            }
            let digest = Sha256::digest(&bytes[..header_len - 32]);
            bytes[header_len - 32..header_len].copy_from_slice(&digest);
            let parsed = parse_bytes(&bytes, encoded.header.extent);
            if version == FORMAT_VERSION {
                assert_eq!(parsed.unwrap().checksum, page.ciphertext.checksum());
            } else {
                assert!(
                    matches!(parsed, Err(Error::CorruptRecord)),
                    "version {version}"
                );
            }
        }

        // A current-version header cannot omit the mandatory checksum even if
        // the framing hash and lengths have been recomputed by the producer.
        let mut missing_crc = original.to_vec();
        missing_crc.drain(crc_offset..crc_offset + 8);
        missing_crc.resize(original.len(), 0);
        let shorter_header = header_len - 8;
        missing_crc[12..16].copy_from_slice(&(shorter_header as u32).to_le_bytes());
        let digest = Sha256::digest(&missing_crc[..shorter_header - 32]);
        missing_crc[shorter_header - 32..shorter_header].copy_from_slice(&digest);
        assert!(matches!(
            parse_bytes(&missing_crc, encoded.header.extent),
            Err(Error::CorruptRecord)
        ));

        let mut damaged_crc = original.to_vec();
        damaged_crc[crc_offset] ^= 1;
        assert!(matches!(
            parse_bytes(&damaged_crc, encoded.header.extent),
            Err(Error::CorruptRecord)
        ));
    }

    #[test]
    fn malformed_frames_are_rejected_without_unbounded_allocations() {
        for len in [1, 16, 512] {
            let bytes = vec![0; len];
            assert!(parse_bytes(&bytes, Extent::new(0, len).unwrap()).is_err());
        }
        let mut bytes = vec![0; 512];
        bytes[..8].copy_from_slice(MAGIC);
        bytes[8..12].copy_from_slice(&FORMAT_VERSION.to_le_bytes());
        bytes[12..16].copy_from_slice(&u32::MAX.to_le_bytes());
        assert!(parse_bytes(&bytes, Extent::new(0, 512).unwrap()).is_err());
    }
}
