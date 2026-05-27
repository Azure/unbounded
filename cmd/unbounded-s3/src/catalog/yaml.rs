// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::HashMap;
use std::io::Read;

use super::types::{ObjectMeta, YamlObject};

/// Hard cap on catalog file size. The catalog is small by design
/// (one entry per S3 object the daemon will serve); operators
/// pointing the daemon at the wrong file (a multi-GB blob, a FIFO,
/// or a hostile manifest) should fail loudly here rather than OOM
/// during the unbounded `read_to_string`.
const MAX_CATALOG_BYTES: u64 = 16 * 1024 * 1024;

/// Catalog backed by a static YAML manifest loaded at startup.
pub struct YamlCatalog {
    entries: HashMap<(String, String), ObjectMeta>,
}

impl YamlCatalog {
    /// Load a catalog from a YAML file at `path`. The file is parsed
    /// once and immutable for the process lifetime. Duplicate
    /// `(bucket, key)` pairs are rejected.
    ///
    /// Files larger than [`MAX_CATALOG_BYTES`] are rejected without
    /// being fully read.
    pub fn load(path: &std::path::Path) -> Result<Self, String> {
        let file = std::fs::File::open(path)
            .map_err(|e| format!("opening catalog {path:?}: {e}"))?;
        // `take(MAX + 1)` bounds the read regardless of file type
        // (regular file, FIFO, socket). After the read, a length
        // strictly greater than MAX means the source had more bytes
        // available; reject. Reading exactly MAX bytes is allowed.
        let mut content = String::new();
        file.take(MAX_CATALOG_BYTES + 1)
            .read_to_string(&mut content)
            .map_err(|e| format!("reading catalog {path:?}: {e}"))?;
        if content.len() as u64 > MAX_CATALOG_BYTES {
            return Err(format!(
                "catalog {path:?} exceeds size limit of {MAX_CATALOG_BYTES} bytes",
            ));
        }
        Self::from_str(&content)
    }

    /// Parse catalog from a YAML string. Exposed for testing.
    pub fn from_str(yaml: &str) -> Result<Self, String> {
        // `deny_unknown_fields` so typos in an operator's catalog
        // (e.g. `last-modified:` with a hyphen) fail loudly at load
        // time rather than silently falling back to defaults.
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Doc {
            objects: Vec<YamlObject>,
        }
        let doc: Doc = serde_yaml::from_str(yaml)
            .map_err(|e| format!("parsing catalog: {e}"))?;

        let mut entries = HashMap::new();
        for obj in doc.objects {
            let (bucket, key, meta) = obj.into_meta()
                .map_err(|e| format!("bad entry: {e}"))?;
            if entries.contains_key(&(bucket.clone(), key.clone())) {
                return Err(format!("duplicate entry: ({bucket}, {key})"));
            }
            entries.insert((bucket, key), meta);
        }
        Ok(Self { entries })
    }

    /// Look up an entry. Returns `None` if the `(bucket, key)` is
    /// not in the catalog.
    pub fn get(&self, bucket: &str, key: &str) -> Option<&ObjectMeta> {
        self.entries.get(&(bucket.to_owned(), key.to_owned()))
    }

    /// Create an empty catalog.
    pub(crate) fn empty() -> Self {
        Self::from_str("objects: []").expect("empty catalog is valid")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn load_valid_yaml() {
        let yaml = r#"
objects:
  - bucket: test
    key: a.bin
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1024
    content_type: application/octet-stream
  - bucket: test
    key: b.bin
    stripe: 0000000000000000000000000000000000000000000000000000000000000000
    size: 0
"#;
        let c = YamlCatalog::from_str(yaml).unwrap();
        let a = c.get("test", "a.bin").unwrap();
        assert_eq!(a.size, 1024);
        assert!(a.stripe.0[0] == 0x5a);
        let b = c.get("test", "b.bin").unwrap();
        assert_eq!(b.size, 0);
        assert!(c.get("test", "nonexistent").is_none());
    }

    #[test]
    fn default_content_type() {
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
"#;
        let c = YamlCatalog::from_str(yaml).unwrap();
        let m = c.get("b", "k").unwrap();
        assert_eq!(m.content_type, "application/octet-stream");
        // last_modified is also optional and falls back to epoch.
        assert_eq!(m.last_modified, "Thu, 01 Jan 1970 00:00:00 GMT");
    }

    #[test]
    fn last_modified_round_trip() {
        // RFC 3339 in YAML is parsed and reformatted as IMF-fixdate so
        // the request path can emit the string straight into the
        // `Last-Modified` response header.
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
    last_modified: "2026-01-15T12:00:00Z"
"#;
        let c = YamlCatalog::from_str(yaml).unwrap();
        let m = c.get("b", "k").unwrap();
        assert_eq!(m.last_modified, "Thu, 15 Jan 2026 12:00:00 GMT");
    }

    #[test]
    fn last_modified_non_utc_offset_is_normalized() {
        // Offsets other than `Z` must be normalized to GMT in the
        // output. `2026-01-15T07:00:00-05:00` is the same instant as
        // `2026-01-15T12:00:00Z`.
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
    last_modified: "2026-01-15T07:00:00-05:00"
"#;
        let c = YamlCatalog::from_str(yaml).unwrap();
        let m = c.get("b", "k").unwrap();
        assert_eq!(m.last_modified, "Thu, 15 Jan 2026 12:00:00 GMT");
    }

    #[test]
    fn bad_last_modified_rejected() {
        // Non-RFC-3339 input fails catalog loading rather than
        // surfacing later as a malformed response header.
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
    last_modified: "not a date"
"#;
        assert!(YamlCatalog::from_str(yaml).is_err());
    }

    #[test]
    fn duplicate_key_rejected() {
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
  - bucket: b
    key: k
    stripe: 0000000000000000000000000000000000000000000000000000000000000000
    size: 2
"#;
        assert!(YamlCatalog::from_str(yaml).is_err());
    }

    #[test]
    fn bad_stripe_length_rejected() {
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: deadbeef
    size: 1
"#;
        assert!(YamlCatalog::from_str(yaml).is_err());
    }

    #[test]
    fn unknown_field_at_doc_level_rejected() {
        // Top-level typos must fail at load time, not be silently
        // ignored.
        let yaml = r#"
extra_top_level_key: oops
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
"#;
        let err = match YamlCatalog::from_str(yaml) {
            Ok(_) => panic!("expected parse error on unknown top-level field"),
            Err(e) => e,
        };
        assert!(
            err.contains("extra_top_level_key"),
            "error should name the unknown field: {err}",
        );
    }

    #[test]
    fn unknown_field_in_object_rejected() {
        // A typo like `last-modified:` (hyphen) instead of
        // `last_modified:` would otherwise silently fall back to the
        // epoch placeholder; deny_unknown_fields surfaces it as a
        // parse error naming the typo.
        let yaml = r#"
objects:
  - bucket: b
    key: k
    stripe: 5a3f000000000000000000000000000000000000000000000000000000000000
    size: 1
    last-modified: "2026-01-15T12:00:00Z"
"#;
        let err = match YamlCatalog::from_str(yaml) {
            Ok(_) => panic!("expected parse error on unknown object field"),
            Err(e) => e,
        };
        assert!(
            err.contains("last-modified"),
            "error should name the unknown field: {err}",
        );
    }

    #[test]
    fn load_rejects_oversize_catalog() {
        use std::io::Write;

        // Build a YAML file that's just over the size cap by padding
        // a comment line. The catalog itself is otherwise valid; we
        // only want to verify the size check fires before parse.
        let mut f = tempfile::NamedTempFile::new()
            .expect("create tempfile for oversize catalog test");
        let padding = "#".repeat((MAX_CATALOG_BYTES + 1) as usize);
        writeln!(f, "{padding}").expect("write padding");
        writeln!(f, "objects: []").expect("write objects");
        f.flush().expect("flush tempfile");

        let err = match YamlCatalog::load(f.path()) {
            Ok(_) => panic!("expected size-limit error"),
            Err(e) => e,
        };
        assert!(
            err.contains("exceeds size limit"),
            "error should mention size limit: {err}",
        );
    }

    #[test]
    fn load_accepts_catalog_at_size_limit() {
        // A file under the size cap loads successfully; the cap is
        // strictly "greater than", not "greater than or equal to".
        use std::io::Write;

        let mut f = tempfile::NamedTempFile::new()
            .expect("create tempfile for at-limit catalog test");
        writeln!(f, "objects: []").expect("write objects");
        f.flush().expect("flush tempfile");

        YamlCatalog::load(f.path()).expect("under-limit catalog should load");
    }
}
