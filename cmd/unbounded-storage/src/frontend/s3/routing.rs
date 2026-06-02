// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! S3 request routing: classify a query-stripped request path into the
//! handful of resources this read-only frontend serves.
//!
//! This replaces the axum router the standalone `unbounded-s3` crate
//! used. The path grammar is intentionally tiny:
//!
//! - `/<bucket>/<key>` -> [`Route::Object`] (the key is everything
//!   after the bucket's trailing slash; it may itself contain slashes).
//! - `/<bucket>` and `/<bucket>/` -> [`Route::BucketRoot`] (only
//!   `?location` is answerable; the policy turns anything else into a
//!   405).
//! - anything else (e.g. `/`) -> [`Route::NotAllowed`].

/// A classified request path.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Route<'a> {
    /// `/<bucket>/<key>`: an object GET/HEAD target.
    Object { bucket: &'a str, key: &'a str },
    /// `/<bucket>` or `/<bucket>/`: a bucket-level request, only
    /// `?location` is recognized.
    BucketRoot,
    /// A path this frontend does not serve (resolves to 405).
    NotAllowed,
}

/// Classify a query-stripped request `path` (the part before any `?`).
///
/// Path segments are used verbatim (no percent-decoding): catalog keys
/// are stored raw and v0 targets are simple ASCII keys. Empty bucket or
/// key segments resolve to [`Route::NotAllowed`].
pub(crate) fn route(path: &str) -> Route<'_> {
    // Must be an absolute path; strip the single leading slash.
    let rest = match path.strip_prefix('/') {
        Some(r) => r,
        None => return Route::NotAllowed,
    };
    if rest.is_empty() {
        // "/" - bucket listing, which this read-only frontend does not
        // serve.
        return Route::NotAllowed;
    }
    match rest.split_once('/') {
        // "/<bucket>": no trailing slash, no key.
        None => Route::BucketRoot,
        // "/<bucket>/...": split off the first segment as the bucket.
        Some((bucket, key)) => {
            if bucket.is_empty() {
                return Route::NotAllowed;
            }
            if key.is_empty() {
                // "/<bucket>/": bucket root with a trailing slash.
                Route::BucketRoot
            } else {
                Route::Object { bucket, key }
            }
        }
    }
}

/// Whether a raw query string carries the `location` sub-resource (with
/// or without a value), i.e. `?location` or `?location=`. Presence is
/// all that matters; `s3cmd` sends `?location` with no value.
pub(crate) fn has_location(query: &str) -> bool {
    query
        .split('&')
        .any(|kv| kv == "location" || kv.starts_with("location="))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn object_path() {
        assert_eq!(
            route("/demo/helloworld.txt"),
            Route::Object {
                bucket: "demo",
                key: "helloworld.txt"
            }
        );
    }

    #[test]
    fn object_key_with_slashes() {
        assert_eq!(
            route("/demo/a/b/c.txt"),
            Route::Object {
                bucket: "demo",
                key: "a/b/c.txt"
            }
        );
    }

    #[test]
    fn bucket_root_with_and_without_trailing_slash() {
        assert_eq!(route("/demo"), Route::BucketRoot);
        assert_eq!(route("/demo/"), Route::BucketRoot);
    }

    #[test]
    fn root_is_not_allowed() {
        assert_eq!(route("/"), Route::NotAllowed);
    }

    #[test]
    fn empty_bucket_is_not_allowed() {
        // "//key" has an empty bucket segment.
        assert_eq!(route("//key"), Route::NotAllowed);
    }

    #[test]
    fn missing_leading_slash_is_not_allowed() {
        assert_eq!(route("demo/key"), Route::NotAllowed);
    }

    #[test]
    fn location_query_detection() {
        assert!(has_location("location"));
        assert!(has_location("location="));
        assert!(has_location("list-type=2&location"));
        assert!(!has_location(""));
        assert!(!has_location("list-type=2"));
        assert!(!has_location("locations"));
    }
}
