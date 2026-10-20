//! Volume key derivation for disk pages.
//!
//! Layout follows `designs/racer-extent-volume.md` "Key": an 8 byte cache hash,
//! a 12 byte object version hash, and a 4 byte big-endian page number. Lengths
//! inside the hashed input are 4 byte big-endian, so distinct field splits never
//! hash the same input.

use crate::model::{ObjectVersion, PageId};
use page_alloc::volume::Key;
use racer_control_wire::CacheId;
use sha2::{Digest, Sha256};

const CACHE_DOMAIN: &[u8] = b"racer.volume.cache.v1\0";
const VERSION_DOMAIN: &[u8] = b"racer.volume.version.v1\0";

/// Bytes of a cache hash prefix.
pub(crate) const CACHE_HASH_BYTES: usize = 8;
/// Bytes of a version hash.
pub(crate) const VERSION_HASH_BYTES: usize = 12;
/// Bytes identifying one object version on disk: cache hash then version hash.
pub(crate) const VERSION_KEY_BYTES: usize = CACHE_HASH_BYTES + VERSION_HASH_BYTES;

/// Cache hash followed by version hash; the key prefix shared by a version's pages.
pub(crate) type VersionKey = [u8; VERSION_KEY_BYTES];

/// First 8 bytes of SHA-256("racer.volume.cache.v1\0" || cache).
pub(crate) fn cache_hash(cache: &CacheId) -> [u8; CACHE_HASH_BYTES] {
    let digest = Sha256::new()
        .chain_update(CACHE_DOMAIN)
        .chain_update(cache.0.as_bytes())
        .finalize();
    let mut out = [0; CACHE_HASH_BYTES];
    out.copy_from_slice(&digest[..CACHE_HASH_BYTES]);
    out
}

/// First 12 bytes of SHA-256 over the version domain and the length-prefixed
/// cache, object key, and etag.
pub(crate) fn version_hash(version: &ObjectVersion) -> [u8; VERSION_HASH_BYTES] {
    let mut hash = Sha256::new();
    hash.update(VERSION_DOMAIN);
    for field in [
        version.object.cache.0.as_bytes(),
        &version.object.key.0[..],
        version.etag.as_bytes(),
    ] {
        // Fields are bounded far below 4 GiB by the wire formats.
        let length = u32::try_from(field.len()).unwrap_or(u32::MAX);
        hash.update(length.to_be_bytes());
        hash.update(field);
    }
    let digest = hash.finalize();
    let mut out = [0; VERSION_HASH_BYTES];
    out.copy_from_slice(&digest[..VERSION_HASH_BYTES]);
    out
}

/// Cache hash followed by version hash for one object version.
pub(crate) fn version_key(version: &ObjectVersion) -> VersionKey {
    let mut out = [0; VERSION_KEY_BYTES];
    out[..CACHE_HASH_BYTES].copy_from_slice(&cache_hash(&version.object.cache));
    out[CACHE_HASH_BYTES..].copy_from_slice(&version_hash(version));
    out
}

/// Volume key of one page, or `None` when the page number exceeds `u32::MAX`.
/// Such pages are never stored on disk.
pub(crate) fn volume_key(page: &PageId) -> Option<Key> {
    let number = u32::try_from(page.number.0).ok()?;
    Some(Key::new(
        cache_hash(&page.version.object.cache),
        version_hash(&page.version),
        number,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{CacheKey, ObjectId, PageNumber, StrongEtag};

    fn version(cache: &str, key: [u8; 32], etag: &str) -> ObjectVersion {
        ObjectVersion {
            object: ObjectId {
                cache: CacheId(cache.into()),
                key: CacheKey(key),
            },
            etag: StrongEtag::test_value(etag),
        }
    }

    fn page(number: u64) -> PageId {
        PageId {
            version: version("cache", [7; 32], "v1"),
            number: PageNumber(number),
        }
    }

    fn hex(bytes: &[u8]) -> String {
        bytes.iter().map(|b| format!("{b:02x}")).collect()
    }

    fn reference(domain: &[u8], fields: &[&[u8]]) -> Vec<u8> {
        let mut input = domain.to_vec();
        for field in fields {
            input.extend_from_slice(&(field.len() as u32).to_be_bytes());
            input.extend_from_slice(field);
        }
        Sha256::digest(&input).to_vec()
    }

    #[test]
    fn cache_hash_golden() {
        assert_eq!(
            hex(&cache_hash(&CacheId("cache".into()))),
            "fdfde7f30e5c6ddf"
        );
        assert_eq!(
            hex(&cache_hash(&CacheId(String::new()))),
            "d3c86e6eaea06b9c"
        );
    }

    #[test]
    fn version_hash_golden() {
        assert_eq!(
            hex(&version_hash(&page(0).version)),
            "1cb27e40c29cbdaaee119e75"
        );
    }

    #[test]
    fn volume_key_golden() {
        let key = volume_key(&page(0x0102_0304)).unwrap();
        assert_eq!(
            hex(&key.0),
            "fdfde7f30e5c6ddf1cb27e40c29cbdaaee119e7501020304"
        );
    }

    #[test]
    fn hashes_match_independent_reference() {
        let v = version("cache", [7; 32], "v1");
        let mut cache = CACHE_DOMAIN.to_vec();
        cache.extend_from_slice(b"cache");
        assert_eq!(cache_hash(&v.object.cache)[..], Sha256::digest(&cache)[..8]);
        let expected = reference(VERSION_DOMAIN, &[b"cache", &[7; 32], v.etag.as_bytes()]);
        assert_eq!(version_hash(&v)[..], expected[..12]);
    }

    #[test]
    fn key_layout_is_cache_version_page() {
        let p = page(5);
        let key = volume_key(&p).unwrap();
        assert_eq!(key.cache(), cache_hash(&p.version.object.cache));
        assert_eq!(key.version(), version_hash(&p.version));
        assert_eq!(key.page(), 5);
        assert_eq!(key.0[..VERSION_KEY_BYTES], version_key(&p.version));
    }

    #[test]
    fn page_bounds() {
        assert_eq!(volume_key(&page(0)).unwrap().page(), 0);
        assert_eq!(
            volume_key(&page(u64::from(u32::MAX))).unwrap().page(),
            u32::MAX
        );
        assert!(volume_key(&page(u64::from(u32::MAX) + 1)).is_none());
        assert!(volume_key(&page(u64::MAX)).is_none());
    }

    #[test]
    fn pages_of_one_version_sort_contiguously_by_number() {
        let a = volume_key(&page(1)).unwrap();
        let b = volume_key(&page(256)).unwrap();
        assert!(a.0 < b.0);
    }

    #[test]
    fn length_prefix_separates_fields() {
        // Moving bytes across the cache/key boundary must change the hash.
        let a = version("ab", [0; 32], "c");
        let b = version("a", [0; 32], "bc");
        assert_ne!(version_hash(&a), version_hash(&b));
        // Without length prefixes these two inputs would be identical.
        let raw = |v: &ObjectVersion| {
            let mut out = v.object.cache.0.as_bytes().to_vec();
            out.extend_from_slice(&v.object.key.0);
            out.extend_from_slice(v.etag.as_bytes());
            out
        };
        let c = version("ab", [0; 32], "x");
        let mut shifted_key = [0; 32];
        shifted_key[0] = b'b';
        let d = version("a", shifted_key, "x");
        assert_eq!(raw(&c)[..33], raw(&d)[..33]);
        assert_ne!(version_hash(&c), version_hash(&d));
    }

    #[test]
    fn domains_separate_cache_and_version_hashes() {
        let v = version("", [0; 32], "e");
        assert_ne!(cache_hash(&v.object.cache)[..], version_hash(&v)[..8]);
        assert_ne!(
            version_hash(&version("x", [0; 32], "e")),
            version_hash(&version("y", [0; 32], "e"))
        );
        assert_ne!(
            version_hash(&version("x", [0; 32], "e")),
            version_hash(&version("x", [0; 32], "f"))
        );
    }
}
