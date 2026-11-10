//! Write journal codec: file header and self-checking records.
//!
//! Records are `u32 BE body_length | u8 kind | body | SHA-256(kind || body)`.
//! Integers are big-endian, strings are `u16 BE length` + bytes, optional
//! strings carry a `u8` presence byte, and keys are 32 raw bytes. The file
//! starts with a 64-byte [`encode_header`] header that [`replay`] does not see:
//! callers pass the bytes after it.

use super::{Durability, Holders, Manifest, SessionId, UploadMode, UploadState, WriteSpec};
use sha2::{Digest, Sha256};

/// File header magic.
pub const MAGIC: &[u8; 8] = b"RCRWJNL1";
/// File header format version.
pub const FORMAT: u32 = 1;
/// File header length.
pub const HEADER_BYTES: usize = 64;
/// Bytes before a record body: length and kind.
pub const RECORD_PREFIX: usize = 5;
/// Digest trailer length.
pub const DIGEST_BYTES: usize = 32;
/// Largest accepted record body. A 50,000-page manifest with three holders per
/// page and long node IDs stays well below this.
pub const MAX_BODY: usize = 64 * 1024 * 1024;

/// One journal entry. `cache` is the cache ID, `key` the 32-byte cache key,
/// and `etag` the quoted Racer version tag.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum JournalRecord {
    /// Kind 1: a durable written page on this worker.
    Page {
        cache: String,
        key: [u8; 32],
        etag: String,
        page: u64,
        segment: u64,
        generation: u64,
        offset: u64,
        length: u32,
        session: SessionId,
        hold_until_millis: u64,
    },
    /// Kind 2: a written page is no longer pinned.
    PageRelease {
        cache: String,
        key: [u8; 32],
        etag: String,
        page: u64,
    },
    /// Kind 3: the coordinator opened a session.
    Open {
        cache: String,
        key: [u8; 32],
        etag: String,
        session: SessionId,
        spec: WriteSpec,
        lease_until_millis: u64,
    },
    /// Kind 4: the coordinator (or a manifest replica) committed a manifest.
    Commit {
        cache: String,
        key: [u8; 32],
        manifest: Manifest,
    },
    /// Kind 5: upload state change.
    State {
        cache: String,
        key: [u8; 32],
        etag: String,
        state: UploadState,
        blob_etag: Option<String>,
    },
    /// Kind 6: an uncommitted session ended.
    Abort {
        cache: String,
        key: [u8; 32],
        etag: String,
    },
    /// Kind 7: a written object was deleted or its hold expired.
    Delete {
        cache: String,
        key: [u8; 32],
        etag: String,
    },
    /// Kind 8: a compaction point.
    Checkpoint { sequence: u64 },
}

impl JournalRecord {
    /// The record kind byte.
    pub fn kind(&self) -> u8 {
        match self {
            Self::Page { .. } => 1,
            Self::PageRelease { .. } => 2,
            Self::Open { .. } => 3,
            Self::Commit { .. } => 4,
            Self::State { .. } => 5,
            Self::Abort { .. } => 6,
            Self::Delete { .. } => 7,
            Self::Checkpoint { .. } => 8,
        }
    }
}

/// Why bytes are not a valid record, header, or manifest.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum JournalError {
    /// The input ends inside the record: a torn tail.
    Torn,
    /// Complete but invalid: digest mismatch, unknown kind, or bad body.
    Corrupt,
    /// Encoding only: a field does not fit its length prefix.
    Oversized,
}

/// Encode the 64-byte file header.
pub fn encode_header(worker: u16, layout: &[u8; 32]) -> [u8; HEADER_BYTES] {
    let mut out = [0; HEADER_BYTES];
    out[..8].copy_from_slice(MAGIC);
    out[8..12].copy_from_slice(&FORMAT.to_be_bytes());
    out[12..14].copy_from_slice(&worker.to_be_bytes());
    out[16..48].copy_from_slice(layout);
    out
}

/// Decode a file header into `(worker, layout digest)`.
pub fn decode_header(bytes: &[u8]) -> Result<(u16, [u8; 32]), JournalError> {
    let header = bytes.get(..HEADER_BYTES).ok_or(JournalError::Torn)?;
    if &header[..8] != MAGIC
        || header[8..12] != FORMAT.to_be_bytes()
        || header[14..16] != [0, 0]
        || header[48..].iter().any(|&b| b != 0)
    {
        return Err(JournalError::Corrupt);
    }
    let worker = u16::from_be_bytes([header[12], header[13]]);
    let mut layout = [0; 32];
    layout.copy_from_slice(&header[16..48]);
    Ok((worker, layout))
}

/// Append one framed record.
pub fn encode(record: &JournalRecord, out: &mut Vec<u8>) -> Result<(), JournalError> {
    let start = out.len();
    out.extend_from_slice(&[0; 4]);
    out.push(record.kind());
    if let Err(error) = body(record, out) {
        out.truncate(start);
        return Err(error);
    }
    let len = out.len() - start - RECORD_PREFIX;
    if len > MAX_BODY {
        out.truncate(start);
        return Err(JournalError::Oversized);
    }
    out[start..start + 4].copy_from_slice(&(len as u32).to_be_bytes());
    let digest = Sha256::digest(&out[start + 4..]);
    out.extend_from_slice(&digest);
    Ok(())
}

/// Decode the first record of `bytes`, returning it and its framed length.
pub fn decode(bytes: &[u8]) -> Result<(JournalRecord, usize), JournalError> {
    if bytes.len() < RECORD_PREFIX {
        return Err(JournalError::Torn);
    }
    let len = u32::from_be_bytes([bytes[0], bytes[1], bytes[2], bytes[3]]) as usize;
    if len > MAX_BODY {
        return Err(JournalError::Corrupt);
    }
    let total = RECORD_PREFIX + len + DIGEST_BYTES;
    if bytes.len() < total {
        return Err(JournalError::Torn);
    }
    let digest = Sha256::digest(&bytes[4..RECORD_PREFIX + len]);
    if digest.as_slice() != &bytes[RECORD_PREFIX + len..total] {
        return Err(JournalError::Corrupt);
    }
    let mut reader = Reader(&bytes[RECORD_PREFIX..RECORD_PREFIX + len]);
    let record = read_body(bytes[4], &mut reader)?;
    reader.finish()?;
    Ok((record, total))
}

/// Decode records until the end or the first invalid one. Returns the records
/// and the length of the valid prefix; callers truncate the file there.
pub fn replay(bytes: &[u8]) -> (Vec<JournalRecord>, usize) {
    let mut records = Vec::new();
    let mut used = 0;
    while used < bytes.len() {
        match decode(&bytes[used..]) {
            Ok((record, len)) => {
                records.push(record);
                used += len;
            }
            Err(_) => break,
        }
    }
    (records, used)
}

/// Encode a manifest (the `racer-write-manifest` payload before base64).
pub fn encode_manifest(manifest: &Manifest, out: &mut Vec<u8>) -> Result<(), JournalError> {
    out.extend_from_slice(&manifest.session.0);
    put_str(out, &manifest.etag)?;
    put_spec(out, &manifest.spec)?;
    let holders = manifest
        .holders
        .to_bytes()
        .map_err(|_| JournalError::Oversized)?;
    let len = u32::try_from(holders.len()).map_err(|_| JournalError::Oversized)?;
    out.extend_from_slice(&len.to_be_bytes());
    out.extend_from_slice(&holders);
    out.push(manifest.state.code());
    put_opt_str(out, manifest.blob_etag.as_deref())
}

/// Decode a complete manifest encoding.
pub fn decode_manifest(bytes: &[u8]) -> Result<Manifest, JournalError> {
    let mut reader = Reader(bytes);
    let manifest = read_manifest(&mut reader)?;
    reader.finish()?;
    Ok(manifest)
}

fn body(record: &JournalRecord, out: &mut Vec<u8>) -> Result<(), JournalError> {
    match record {
        JournalRecord::Page {
            cache,
            key,
            etag,
            page,
            segment,
            generation,
            offset,
            length,
            session,
            hold_until_millis,
        } => {
            object(out, cache, key, Some(etag))?;
            for value in [page, segment, generation, offset] {
                out.extend_from_slice(&value.to_be_bytes());
            }
            out.extend_from_slice(&length.to_be_bytes());
            out.extend_from_slice(&session.0);
            out.extend_from_slice(&hold_until_millis.to_be_bytes());
        }
        JournalRecord::PageRelease {
            cache,
            key,
            etag,
            page,
        } => {
            object(out, cache, key, Some(etag))?;
            out.extend_from_slice(&page.to_be_bytes());
        }
        JournalRecord::Open {
            cache,
            key,
            etag,
            session,
            spec,
            lease_until_millis,
        } => {
            object(out, cache, key, Some(etag))?;
            out.extend_from_slice(&session.0);
            put_spec(out, spec)?;
            out.extend_from_slice(&lease_until_millis.to_be_bytes());
        }
        JournalRecord::Commit {
            cache,
            key,
            manifest,
        } => {
            object(out, cache, key, None)?;
            encode_manifest(manifest, out)?;
        }
        JournalRecord::State {
            cache,
            key,
            etag,
            state,
            blob_etag,
        } => {
            object(out, cache, key, Some(etag))?;
            out.push(state.code());
            put_opt_str(out, blob_etag.as_deref())?;
        }
        JournalRecord::Abort { cache, key, etag } | JournalRecord::Delete { cache, key, etag } => {
            object(out, cache, key, Some(etag))?;
        }
        JournalRecord::Checkpoint { sequence } => out.extend_from_slice(&sequence.to_be_bytes()),
    }
    Ok(())
}

fn object(
    out: &mut Vec<u8>,
    cache: &str,
    key: &[u8; 32],
    etag: Option<&str>,
) -> Result<(), JournalError> {
    put_str(out, cache)?;
    out.extend_from_slice(key);
    match etag {
        Some(etag) => put_str(out, etag),
        None => Ok(()),
    }
}

fn put_str(out: &mut Vec<u8>, value: &str) -> Result<(), JournalError> {
    let len = u16::try_from(value.len()).map_err(|_| JournalError::Oversized)?;
    out.extend_from_slice(&len.to_be_bytes());
    out.extend_from_slice(value.as_bytes());
    Ok(())
}

fn put_opt_str(out: &mut Vec<u8>, value: Option<&str>) -> Result<(), JournalError> {
    match value {
        Some(value) => {
            out.push(1);
            put_str(out, value)
        }
        None => {
            out.push(0);
            Ok(())
        }
    }
}

fn put_spec(out: &mut Vec<u8>, spec: &WriteSpec) -> Result<(), JournalError> {
    out.extend_from_slice(&spec.length.to_be_bytes());
    out.extend_from_slice(&spec.page_size.to_be_bytes());
    put_opt_str(out, spec.content_type.as_deref())?;
    out.push(spec.replicas);
    match spec.durability {
        Durability::Replicas(n) => out.extend_from_slice(&[0, n]),
        Durability::Origin => out.extend_from_slice(&[1, 0]),
    }
    out.push(spec.upload.code());
    out.extend_from_slice(&spec.hold_until_millis.to_be_bytes());
    Ok(())
}

/// Bounds-checked cursor; every failure is `Corrupt` because the digest
/// already proved the record is complete.
struct Reader<'a>(&'a [u8]);

impl<'a> Reader<'a> {
    fn take(&mut self, n: usize) -> Result<&'a [u8], JournalError> {
        if self.0.len() < n {
            return Err(JournalError::Corrupt);
        }
        let (head, rest) = self.0.split_at(n);
        self.0 = rest;
        Ok(head)
    }

    fn u8(&mut self) -> Result<u8, JournalError> {
        Ok(self.take(1)?[0])
    }

    fn u32(&mut self) -> Result<u32, JournalError> {
        Ok(u32::from_be_bytes(self.take(4)?.try_into().unwrap()))
    }

    fn u64(&mut self) -> Result<u64, JournalError> {
        Ok(u64::from_be_bytes(self.take(8)?.try_into().unwrap()))
    }

    fn key(&mut self) -> Result<[u8; 32], JournalError> {
        Ok(self.take(32)?.try_into().unwrap())
    }

    fn session(&mut self) -> Result<SessionId, JournalError> {
        Ok(SessionId(self.take(16)?.try_into().unwrap()))
    }

    fn string(&mut self) -> Result<String, JournalError> {
        let len = usize::from(u16::from_be_bytes(self.take(2)?.try_into().unwrap()));
        String::from_utf8(self.take(len)?.to_vec()).map_err(|_| JournalError::Corrupt)
    }

    fn opt_string(&mut self) -> Result<Option<String>, JournalError> {
        match self.u8()? {
            0 => Ok(None),
            1 => self.string().map(Some),
            _ => Err(JournalError::Corrupt),
        }
    }

    fn spec(&mut self) -> Result<WriteSpec, JournalError> {
        let length = self.u64()?;
        let page_size = self.u64()?;
        let content_type = self.opt_string()?;
        let replicas = self.u8()?;
        let durability = match (self.u8()?, self.u8()?) {
            (0, n) => Durability::Replicas(n),
            (1, 0) => Durability::Origin,
            _ => return Err(JournalError::Corrupt),
        };
        let upload = UploadMode::from_code(self.u8()?).ok_or(JournalError::Corrupt)?;
        let hold_until_millis = self.u64()?;
        let spec = WriteSpec {
            length,
            page_size,
            content_type,
            replicas,
            durability,
            upload,
            hold_until_millis,
        };
        spec.validate().map_err(|_| JournalError::Corrupt)?;
        Ok(spec)
    }

    fn finish(&self) -> Result<(), JournalError> {
        if self.0.is_empty() {
            Ok(())
        } else {
            Err(JournalError::Corrupt)
        }
    }
}

fn read_manifest(r: &mut Reader<'_>) -> Result<Manifest, JournalError> {
    let session = r.session()?;
    let etag = r.string()?;
    let spec = r.spec()?;
    let len = r.u32()? as usize;
    let holders = Holders::decode(r.take(len)?).map_err(|_| JournalError::Corrupt)?;
    if holders.pages() != spec.pages() {
        return Err(JournalError::Corrupt);
    }
    let state = UploadState::from_code(r.u8()?).ok_or(JournalError::Corrupt)?;
    let blob_etag = r.opt_string()?;
    Ok(Manifest {
        session,
        etag,
        spec,
        holders,
        state,
        blob_etag,
    })
}

fn read_body(kind: u8, r: &mut Reader<'_>) -> Result<JournalRecord, JournalError> {
    if kind == 8 {
        return Ok(JournalRecord::Checkpoint { sequence: r.u64()? });
    }
    if !(1..=7).contains(&kind) {
        return Err(JournalError::Corrupt);
    }
    let cache = r.string()?;
    let key = r.key()?;
    if kind == 4 {
        let manifest = read_manifest(r)?;
        return Ok(JournalRecord::Commit {
            cache,
            key,
            manifest,
        });
    }
    let etag = r.string()?;
    Ok(match kind {
        1 => JournalRecord::Page {
            cache,
            key,
            etag,
            page: r.u64()?,
            segment: r.u64()?,
            generation: r.u64()?,
            offset: r.u64()?,
            length: r.u32()?,
            session: r.session()?,
            hold_until_millis: r.u64()?,
        },
        2 => JournalRecord::PageRelease {
            cache,
            key,
            etag,
            page: r.u64()?,
        },
        3 => JournalRecord::Open {
            cache,
            key,
            etag,
            session: r.session()?,
            spec: r.spec()?,
            lease_until_millis: r.u64()?,
        },
        5 => JournalRecord::State {
            cache,
            key,
            etag,
            state: UploadState::from_code(r.u8()?).ok_or(JournalError::Corrupt)?,
            blob_etag: r.opt_string()?,
        },
        6 => JournalRecord::Abort { cache, key, etag },
        _ => JournalRecord::Delete { cache, key, etag },
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const ETAG: &str = "\"racer-0123456789abcdef0123456789abcdef\"";

    fn spec() -> WriteSpec {
        WriteSpec {
            length: 10,
            page_size: 4,
            content_type: Some("text/plain".into()),
            replicas: 2,
            durability: Durability::Replicas(2),
            upload: UploadMode::Manual,
            hold_until_millis: 77,
        }
    }

    fn manifest() -> Manifest {
        Manifest {
            session: SessionId([9; 16]),
            etag: ETAG.into(),
            spec: spec(),
            holders: Holders(vec![
                vec![b"a".to_vec(), b"b".to_vec()],
                vec![b"b".to_vec(), b"c".to_vec()],
                vec![b"c".to_vec(), b"a".to_vec()],
            ]),
            state: UploadState::Committed,
            blob_etag: Some("\"0x8D\"".into()),
        }
    }

    fn all() -> Vec<JournalRecord> {
        let cache = || "cache-1".to_string();
        let etag = || ETAG.to_string();
        vec![
            JournalRecord::Page {
                cache: cache(),
                key: [1; 32],
                etag: etag(),
                page: 3,
                segment: 4,
                generation: 5,
                offset: 6,
                length: 7,
                session: SessionId([2; 16]),
                hold_until_millis: u64::MAX,
            },
            JournalRecord::PageRelease {
                cache: cache(),
                key: [1; 32],
                etag: etag(),
                page: 0,
            },
            JournalRecord::Open {
                cache: cache(),
                key: [1; 32],
                etag: etag(),
                session: SessionId([3; 16]),
                spec: WriteSpec {
                    content_type: None,
                    durability: Durability::Origin,
                    upload: UploadMode::Auto,
                    ..spec()
                },
                lease_until_millis: 99,
            },
            JournalRecord::Commit {
                cache: cache(),
                key: [1; 32],
                manifest: manifest(),
            },
            JournalRecord::State {
                cache: cache(),
                key: [1; 32],
                etag: etag(),
                state: UploadState::Failed,
                blob_etag: None,
            },
            JournalRecord::Abort {
                cache: cache(),
                key: [1; 32],
                etag: etag(),
            },
            JournalRecord::Delete {
                cache: String::new(),
                key: [0; 32],
                etag: String::new(),
            },
            JournalRecord::Checkpoint { sequence: 42 },
        ]
    }

    fn bytes(record: &JournalRecord) -> Vec<u8> {
        let mut out = Vec::new();
        encode(record, &mut out).unwrap();
        out
    }

    #[test]
    fn every_kind_round_trips() {
        let records = all();
        let kinds: Vec<u8> = records.iter().map(JournalRecord::kind).collect();
        assert_eq!(kinds, (1..=8).collect::<Vec<_>>());
        let mut stream = Vec::new();
        for record in &records {
            let b = bytes(record);
            assert_eq!(decode(&b), Ok((record.clone(), b.len())));
            stream.extend_from_slice(&b);
        }
        assert_eq!(replay(&stream), (records, stream.len()));
    }

    #[test]
    fn checkpoint_golden() {
        let b = bytes(&JournalRecord::Checkpoint { sequence: 1 });
        assert_eq!(&b[..13], &[0, 0, 0, 8, 8, 0, 0, 0, 0, 0, 0, 0, 1]);
        assert_eq!(&b[13..], Sha256::digest(&b[4..13]).as_slice());
    }

    #[test]
    fn torn_tail_at_every_offset_yields_first_record() {
        let records = all();
        let first = bytes(&records[3]);
        let second = bytes(&records[0]);
        let stream = [first.clone(), second].concat();
        for cut in 0..=stream.len() {
            let (got, used) = replay(&stream[..cut]);
            if cut < first.len() {
                assert_eq!((got.len(), used), (0, 0), "cut {cut}");
                assert_eq!(decode(&stream[..cut]), Err(JournalError::Torn), "cut {cut}");
            } else if cut < stream.len() {
                assert_eq!(got, vec![records[3].clone()], "cut {cut}");
                assert_eq!(used, first.len());
            } else {
                assert_eq!(got.len(), 2);
                assert_eq!(used, stream.len());
            }
        }
    }

    #[test]
    fn any_flipped_bit_is_corrupt() {
        for record in all() {
            let b = bytes(&record);
            // Skip the length prefix: changing it makes the frame torn or corrupt.
            for i in 4..b.len() {
                let mut bad = b.clone();
                bad[i] ^= 0x01;
                assert_eq!(decode(&bad), Err(JournalError::Corrupt), "byte {i}");
            }
            let mut bad = b.clone();
            bad[3] ^= 1;
            assert!(decode(&bad).is_err());
        }
    }

    #[test]
    fn replay_stops_at_corruption() {
        let a = bytes(&all()[7]);
        let mut b = bytes(&all()[5]);
        let last = b.len() - 1;
        b[last] ^= 0xff;
        let stream = [a.clone(), b, a.clone()].concat();
        let (records, used) = replay(&stream);
        assert_eq!(records.len(), 1);
        assert_eq!(used, a.len());
    }

    #[test]
    fn rejects_well_framed_invalid_bodies() {
        let frame = |kind: u8, body: &[u8]| {
            let mut out = (body.len() as u32).to_be_bytes().to_vec();
            out.push(kind);
            out.extend_from_slice(body);
            let digest = Sha256::digest(&out[4..]);
            out.extend_from_slice(&digest);
            out
        };
        assert_eq!(decode(&frame(0, &[])), Err(JournalError::Corrupt));
        assert_eq!(decode(&frame(9, &[0; 8])), Err(JournalError::Corrupt));
        // Trailing bytes after a valid body.
        assert_eq!(decode(&frame(8, &[0; 9])), Err(JournalError::Corrupt));
        // Invalid UTF-8 cache ID.
        let mut body = vec![0, 1, 0xff];
        body.extend_from_slice(&[0; 32]);
        body.extend_from_slice(&[0, 0]);
        assert_eq!(decode(&frame(6, &body)), Err(JournalError::Corrupt));
        // Oversized length is corrupt, not torn.
        let mut huge = ((MAX_BODY + 1) as u32).to_be_bytes().to_vec();
        huge.push(8);
        assert_eq!(decode(&huge), Err(JournalError::Corrupt));
    }

    #[test]
    fn manifest_validation() {
        let mut out = Vec::new();
        encode_manifest(&manifest(), &mut out).unwrap();
        assert_eq!(decode_manifest(&out), Ok(manifest()));
        let mut short = manifest();
        short.holders.0.pop();
        let mut out = Vec::new();
        encode_manifest(&short, &mut out).unwrap();
        assert_eq!(decode_manifest(&out), Err(JournalError::Corrupt));
        let mut bad = manifest();
        bad.spec.replicas = 0;
        let mut out = Vec::new();
        encode_manifest(&bad, &mut out).unwrap();
        assert_eq!(decode_manifest(&out), Err(JournalError::Corrupt));
    }

    #[test]
    fn oversized_fields_leave_buffer_unchanged() {
        let mut out = vec![7];
        let record = JournalRecord::Abort {
            cache: "x".repeat(70_000),
            key: [0; 32],
            etag: String::new(),
        };
        assert_eq!(encode(&record, &mut out), Err(JournalError::Oversized));
        assert_eq!(out, vec![7]);
    }

    #[test]
    fn header_round_trip() {
        let header = encode_header(513, &[4; 32]);
        assert_eq!(&header[..8], MAGIC);
        assert_eq!(decode_header(&header), Ok((513, [4; 32])));
        assert_eq!(decode_header(&header[..63]), Err(JournalError::Torn));
        for i in (0..16).chain(48..64) {
            if (12..14).contains(&i) {
                continue;
            }
            let mut bad = header;
            bad[i] ^= 1;
            assert_eq!(decode_header(&bad), Err(JournalError::Corrupt), "byte {i}");
        }
    }
}
