// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Object metadata payload: the on-page format of an object's
//! dedicated metadata entry.
//!
//! A metadata entry is a content-addressed cache entry (see
//! [`OriginRef::metadata_entry`](crate::storage::OriginRef::metadata_entry))
//! whose page does not hold object data. Instead it carries
//! [`ObjectMetadata`]: the object's byte length plus a small set of
//! opaque key/value pairs passed through the system between a backend
//! (writer) and a frontend (reader). Every other layer (bufferpool,
//! btree, p2p, fabric) treats the page as opaque bytes; only the
//! origin-facing backends and the client-facing frontends interpret it
//! through this codec.
//!
//! ## On-page layout
//!
//! ```text
//! [0..8)            u64 little-endian  blob_len
//! [8..8+blob_len)   bincode(ObjectMetadata)
//! [8+blob_len..)    zero padding to the end of the page
//! ```
//!
//! The explicit `blob_len` prefix lets [`ObjectMetadata::decode`]
//! deserialize an exact sub-slice without depending on bincode's
//! trailing-byte policy, so the zero padding a backend writes after the
//! payload is ignored. The only size limit on the encoded metadata is
//! the cache page itself: the backend write path rejects a payload that
//! does not fit in the entry's page.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use crate::bufferpool::Error;

/// Width of the little-endian `blob_len` prefix that precedes the
/// bincode payload on the page.
const BLOB_LEN_PREFIX: usize = 8;

/// An object's metadata: its byte length plus a small set of opaque
/// key/value pairs.
///
/// The key/value pairs are passed through unchanged between a backend
/// and a frontend; this crate attaches no semantics to them and does
/// not index them. Entries are held in a [`BTreeMap`] so the encoded
/// bytes are deterministic for a given logical set, which keeps the
/// content-addressed entry stable.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ObjectMetadata {
    pub length: u64,
    pub entries: BTreeMap<String, String>,
}

impl ObjectMetadata {
    /// Construct metadata for an object of `length` bytes with no
    /// key/value pairs.
    pub fn new(length: u64) -> Self {
        Self {
            length,
            entries: BTreeMap::new(),
        }
    }

    /// Insert a key/value pair, returning the previous value for the
    /// key if one was present.
    pub fn insert(
        &mut self,
        key: impl Into<String>,
        value: impl Into<String>,
    ) -> Option<String> {
        self.entries.insert(key.into(), value.into())
    }

    /// Look up the value for `key`, if present.
    pub fn get(&self, key: &str) -> Option<&str> {
        self.entries.get(key).map(String::as_str)
    }

    /// Iterate the key/value pairs in deterministic (sorted-key) order.
    pub fn iter(&self) -> impl Iterator<Item = (&str, &str)> {
        self.entries.iter().map(|(k, v)| (k.as_str(), v.as_str()))
    }

    /// Whether there are no key/value pairs.
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// Encode into the on-page byte layout: an 8-byte little-endian
    /// payload-length prefix followed by the bincode payload. The
    /// caller writes this into the metadata entry's page (zero-filling
    /// any remainder); the page-capacity check at the write edge is the
    /// only size limit on the result.
    pub fn encode(&self) -> Result<Vec<u8>, Error> {
        let blob = bincode::serialize(self)
            .map_err(|_| Error::from("storage metadata: bincode serialize failed"))?;
        let mut out = Vec::with_capacity(BLOB_LEN_PREFIX + blob.len());
        out.extend_from_slice(&(blob.len() as u64).to_le_bytes());
        out.extend_from_slice(&blob);
        Ok(out)
    }

    /// Decode from a metadata entry's page. Reads the `blob_len` prefix,
    /// bounds-checks it against the page, and deserializes exactly that
    /// many payload bytes, ignoring any trailing zero padding.
    pub fn decode(page: &[u8]) -> Result<ObjectMetadata, Error> {
        if page.len() < BLOB_LEN_PREFIX {
            return Err(Error::from(
                "storage metadata: page smaller than length prefix",
            ));
        }
        let blob_len =
            u64::from_le_bytes(page[..BLOB_LEN_PREFIX].try_into().unwrap()) as usize;
        let end = BLOB_LEN_PREFIX
            .checked_add(blob_len)
            .ok_or_else(|| Error::from("storage metadata: blob length overflow"))?;
        if end > page.len() {
            return Err(Error::from(
                "storage metadata: blob length exceeds page",
            ));
        }
        bincode::deserialize(&page[BLOB_LEN_PREFIX..end])
            .map_err(|_| Error::from("storage metadata: malformed bincode payload"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn with_entries() -> ObjectMetadata {
        let mut m = ObjectMetadata::new(4096);
        m.insert("content-type", "application/octet-stream");
        m.insert("etag", "\"abc123\"");
        m
    }

    #[test]
    fn round_trips_empty() {
        let m = ObjectMetadata::new(0);
        let decoded = ObjectMetadata::decode(&m.encode().unwrap()).unwrap();
        assert_eq!(decoded, m);
        assert_eq!(decoded.length, 0);
        assert!(decoded.is_empty());
    }

    #[test]
    fn round_trips_with_entries() {
        let m = with_entries();
        let decoded = ObjectMetadata::decode(&m.encode().unwrap()).unwrap();
        assert_eq!(decoded, m);
        assert_eq!(decoded.length, 4096);
        assert_eq!(decoded.get("content-type"), Some("application/octet-stream"));
        assert_eq!(decoded.get("etag"), Some("\"abc123\""));
        assert_eq!(decoded.get("missing"), None);
    }

    #[test]
    fn encoding_is_deterministic_regardless_of_insertion_order() {
        let mut a = ObjectMetadata::new(7);
        a.insert("b", "2");
        a.insert("a", "1");

        let mut b = ObjectMetadata::new(7);
        b.insert("a", "1");
        b.insert("b", "2");

        assert_eq!(a.encode().unwrap(), b.encode().unwrap());
    }

    #[test]
    fn iter_yields_sorted_keys() {
        let mut m = ObjectMetadata::new(1);
        m.insert("zeta", "z");
        m.insert("alpha", "a");
        m.insert("mu", "m");
        let keys: Vec<&str> = m.iter().map(|(k, _)| k).collect();
        assert_eq!(keys, ["alpha", "mu", "zeta"]);
    }

    #[test]
    fn decode_ignores_trailing_page_padding() {
        // Simulate a real cache page: payload followed by zero fill.
        let m = with_entries();
        let mut page = m.encode().unwrap();
        page.resize(4096, 0);
        let decoded = ObjectMetadata::decode(&page).unwrap();
        assert_eq!(decoded, m);
    }

    #[test]
    fn decode_rejects_page_shorter_than_prefix() {
        assert!(ObjectMetadata::decode(&[0u8; 4]).is_err());
        assert!(ObjectMetadata::decode(&[]).is_err());
    }

    #[test]
    fn decode_rejects_blob_len_exceeding_page() {
        // Prefix claims 100 payload bytes but the page has only a few.
        let mut page = Vec::new();
        page.extend_from_slice(&100u64.to_le_bytes());
        page.extend_from_slice(&[0u8; 8]);
        assert!(ObjectMetadata::decode(&page).is_err());
    }

    #[test]
    fn decode_rejects_malformed_payload() {
        // Valid prefix, but the payload is not a valid bincode encoding
        // of ObjectMetadata (a u64 length followed by a map count that
        // overruns the available bytes).
        let mut page = Vec::new();
        let garbage = [0xffu8; 16];
        page.extend_from_slice(&(garbage.len() as u64).to_le_bytes());
        page.extend_from_slice(&garbage);
        assert!(ObjectMetadata::decode(&page).is_err());
    }

    #[test]
    fn large_metadata_that_fits_round_trips() {
        let mut m = ObjectMetadata::new(u64::MAX);
        for i in 0..64 {
            m.insert(format!("key-{i:04}"), format!("value-{i:08}"));
        }
        let encoded = m.encode().unwrap();
        let decoded = ObjectMetadata::decode(&encoded).unwrap();
        assert_eq!(decoded, m);
        assert_eq!(decoded.entries.len(), 64);
    }
}
