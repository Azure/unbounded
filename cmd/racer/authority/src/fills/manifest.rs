//! Fill manifest: the sorted object set one process listed from the origin.
//!
//! The digest is the SHA-256 of a canonical JSON array of the sorted objects,
//! exactly `[{"etag":<etag>,"key":<key hex>,"length":<decimal>},..]` with no
//! whitespace and keys in that order. Two processes that list the same objects
//! produce the same digest. `metadata` is not part of the digest; it is
//! derived from the key and bucket by [`super::identity`].

use super::identity;
use racer_wire::object::{CacheKey, ContentType, StrongEtag};
use sha2::Digest;

/// Most objects one fill may pin. A larger listing is rejected, never
/// truncated.
pub const MAX_FILL_OBJECTS: usize = 8192;

/// One listed blob as returned by the origin.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ListedObject {
    pub name: String,
    pub etag: String,
    pub length: u64,
    /// The origin's `Content-Type`, when it reported one.
    pub content_type: Option<String>,
}

/// One pinned object.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FillObject {
    pub key: CacheKey,
    pub etag: StrongEtag,
    pub length: u64,
    /// The origin's `Content-Type`, when it reported one. Pages carry this
    /// value, so a fill must reproduce it to reuse the object's metadata.
    pub content_type: Option<ContentType>,
    /// Canonical schema-1 identity bytes for `Racer-Metadata`.
    pub metadata: Vec<u8>,
}

/// A complete listed object set.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Manifest {
    /// Lowercase hex SHA-256 of the canonical object list.
    pub digest: String,
    pub logical_bytes: u64,
    /// Sorted by key.
    pub objects: Vec<FillObject>,
    /// Unix seconds when the list completed.
    pub listed_at: u64,
}

/// Why a listing cannot become a manifest.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ManifestError {
    /// More than [`MAX_FILL_OBJECTS`] objects matched.
    TooManyObjects,
    /// A listed blob has an identity or ETag the wire cannot carry.
    InvalidObject,
}

/// Build a manifest from a raw listing. Objects are filtered by `prefixes`
/// (a blob matches when its name starts with any prefix; an empty prefix
/// matches every blob), deduplicated by canonical key, and sorted by key.
pub fn build(
    container: &str,
    listed: &[ListedObject],
    prefixes: &[String],
    listed_at: u64,
) -> Result<Manifest, ManifestError> {
    let mut objects = Vec::new();
    let mut seen = std::collections::HashSet::new();
    for blob in listed {
        if !prefixes.iter().any(|p| blob.name.starts_with(p)) {
            continue;
        }
        let (metadata, key) =
            identity::canonical(container, &blob.name).map_err(|_| ManifestError::InvalidObject)?;
        if !seen.insert(key) {
            continue;
        }
        let etag =
            StrongEtag::parse(blob.etag.as_bytes()).map_err(|_| ManifestError::InvalidObject)?;
        objects.push(FillObject {
            key,
            etag,
            length: blob.length,
            content_type: blob
                .content_type
                .as_deref()
                .and_then(|value| ContentType::parse(value.as_bytes()).ok()),
            metadata,
        });
        if objects.len() > MAX_FILL_OBJECTS {
            return Err(ManifestError::TooManyObjects);
        }
    }
    objects.sort_by(|a, b| a.key.cmp(&b.key));
    let logical_bytes = objects.iter().map(|o| o.length).sum();
    Ok(Manifest {
        digest: digest(&objects),
        logical_bytes,
        objects,
        listed_at,
    })
}

/// Canonical digest of a sorted object list, documented on the module.
pub fn digest(objects: &[FillObject]) -> String {
    let mut out = String::with_capacity(2 + objects.len() * 160);
    out.push('[');
    for (i, object) in objects.iter().enumerate() {
        if i > 0 {
            out.push(',');
        }
        out.push_str(r#"{"etag":"#);
        out.push_str(&json_string(object.etag.as_str()));
        out.push_str(r#","key":"#);
        out.push_str(&json_string(&object.key.to_hex()));
        out.push_str(r#","length":"#);
        out.push_str(&object.length.to_string());
        out.push('}');
    }
    out.push(']');
    hex(&sha2::Sha256::digest(out.as_bytes()))
}

/// Footprint of a manifest, in bytes: every object's page count rounded up to
/// the page size, times `replicas`, summed.
pub fn planned_footprint(manifest: &Manifest, page_size: u64, replicas: u8) -> u64 {
    let page_size = page_size.max(1);
    manifest
        .objects
        .iter()
        .map(|o| o.length.div_ceil(page_size).saturating_mul(page_size))
        .fold(0u64, u64::saturating_add)
        .saturating_mul(u64::from(replicas))
}

/// Minimal JSON string encoding for the digest input. Values come from the
/// wire types, so only ASCII is escaped.
fn json_string(value: &str) -> String {
    let mut out = String::with_capacity(value.len() + 2);
    out.push('"');
    for c in value.chars() {
        match c {
            '"' | '\\' => {
                out.push('\\');
                out.push(c);
            }
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

fn hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        out.push(HEX[usize::from(b >> 4)] as char);
        out.push(HEX[usize::from(b & 0xf)] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn object(name: &str, etag: &str, length: u64) -> ListedObject {
        ListedObject {
            name: name.into(),
            etag: etag.into(),
            length,
            content_type: None,
        }
    }

    fn listed() -> Vec<ListedObject> {
        vec![
            object("a/2", "\"b\"", 3),
            object("a/1", "\"a\"", 1),
            object("b/1", "\"c\"", 2),
        ]
    }

    #[test]
    fn matches_prefixes_and_sorts_by_key() {
        let m = build("models", &listed(), &["a/".into()], 7).unwrap();
        assert_eq!(m.objects.len(), 2);
        assert_eq!(m.logical_bytes, 4);
        assert_eq!(m.listed_at, 7);
        // Sorting is by key, and every manifest has the same key order.
        let mut keys: Vec<_> = m.objects.iter().map(|o| o.key).collect();
        let sorted = keys.clone();
        keys.sort();
        assert_eq!(keys, sorted);
        let all = build("models", &listed(), &["".into()], 7).unwrap();
        assert_eq!(all.objects.len(), 3);
        // Overlapping prefixes select a blob once.
        let dup = build("models", &listed(), &["a/".into(), "a/1".into()], 7).unwrap();
        assert_eq!(dup.objects.len(), 2);
    }

    #[test]
    fn digest_is_stable_and_order_independent() {
        let m = build("models", &listed(), &["".into()], 1).unwrap();
        let mut shuffled = listed();
        shuffled.reverse();
        let n = build("models", &shuffled, &["".into()], 99).unwrap();
        assert_eq!(m.digest, n.digest);
        assert_eq!(m.digest.len(), 64);
        assert!(
            m.digest
                .bytes()
                .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
        );
        let mut different = listed();
        different[0].length += 1;
        let o = build("models", &different, &["".into()], 1).unwrap();
        assert_ne!(m.digest, o.digest);
    }

    #[test]
    fn object_cap_rejects_instead_of_truncating() {
        let many: Vec<_> = (0..=MAX_FILL_OBJECTS)
            .map(|i| object(&format!("k{i}"), "\"e\"", 1))
            .collect();
        assert_eq!(
            build("models", &many, &["".into()], 1),
            Err(ManifestError::TooManyObjects)
        );
        let at_cap: Vec<_> = many[..MAX_FILL_OBJECTS].to_vec();
        assert_eq!(
            build("models", &at_cap, &["".into()], 1)
                .unwrap()
                .objects
                .len(),
            MAX_FILL_OBJECTS
        );
    }

    #[test]
    fn planned_footprint_rounds_each_object_to_pages() {
        let m = build("models", &listed(), &["".into()], 1).unwrap();
        // Each object is one page at 16 bytes, times 2 replicas.
        assert_eq!(planned_footprint(&m, 16, 2), 3 * 16 * 2);
        assert_eq!(planned_footprint(&m, 4, 1), (4 + 4 + 4) as u64);
    }

    #[test]
    fn invalid_identity_or_etag_is_rejected() {
        let mut bad = listed();
        bad[0].name = String::new();
        assert_eq!(
            build("models", &bad, &["".into()], 1),
            Err(ManifestError::InvalidObject)
        );
        let mut bad = listed();
        bad[0].etag = "unquoted".into();
        assert_eq!(
            build("models", &bad, &["".into()], 1),
            Err(ManifestError::InvalidObject)
        );
    }
}
