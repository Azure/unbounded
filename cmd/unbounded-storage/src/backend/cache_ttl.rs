// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Metadata TTL policy shared by HTTP-family origin backends.

use std::time::{SystemTime, UNIX_EPOCH};

use crate::bufferpool::Error;
use crate::http::StatusCode;
use crate::storage::ObjectMetadata;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct MetadataTtlPolicy {
    metadata_default_secs: u64,
    metadata_max_secs: u64,
    not_found_default_secs: u64,
    not_found_max_secs: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum MetadataTtlKind {
    Found,
    NotFound,
}

impl MetadataTtlPolicy {
    pub(crate) fn new(
        metadata_default_secs: u64,
        metadata_max_secs: u64,
        not_found_default_secs: u64,
        not_found_max_secs: u64,
    ) -> Self {
        Self {
            metadata_default_secs,
            metadata_max_secs,
            not_found_default_secs,
            not_found_max_secs,
        }
    }

    pub(crate) fn expires_at(
        &self,
        kind: MetadataTtlKind,
        cache_control: Option<&str>,
        now_unix_secs: u64,
    ) -> u64 {
        now_unix_secs.saturating_add(self.ttl_secs(kind, cache_control))
    }

    fn ttl_secs(&self, kind: MetadataTtlKind, cache_control: Option<&str>) -> u64 {
        let (default_secs, max_secs) = match kind {
            MetadataTtlKind::Found => (self.metadata_default_secs, self.metadata_max_secs),
            MetadataTtlKind::NotFound => (self.not_found_default_secs, self.not_found_max_secs),
        };
        match cache_control.and_then(cache_control_ttl_secs) {
            Some(ttl) => ttl.min(max_secs),
            None => default_secs.min(max_secs),
        }
    }
}

pub(crate) fn metadata_from_head(
    status: StatusCode,
    content_length: Option<u64>,
    cache_control: Option<&str>,
    policy: MetadataTtlPolicy,
    now_unix_secs: u64,
    missing_len_msg: &'static str,
    non_ok_msg: &'static str,
) -> Result<ObjectMetadata, Error> {
    if status == StatusCode::NOT_FOUND {
        return Ok(ObjectMetadata::not_found(policy.expires_at(
            MetadataTtlKind::NotFound,
            cache_control,
            now_unix_secs,
        )));
    }
    if status != StatusCode::OK {
        return Err(Error::from(non_ok_msg));
    }
    let length = content_length.ok_or_else(|| Error::from(missing_len_msg))?;
    Ok(ObjectMetadata::found(
        length,
        policy.expires_at(MetadataTtlKind::Found, cache_control, now_unix_secs),
    ))
}

pub(crate) fn unix_now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn cache_control_ttl_secs(value: &str) -> Option<u64> {
    let mut max_age = None;
    for directive in value.split(',') {
        let directive = directive.trim();
        let (name, raw_value) = directive
            .split_once('=')
            .map(|(name, value)| (name.trim(), Some(value.trim())))
            .unwrap_or((directive, None));
        if name.eq_ignore_ascii_case("no-store") || name.eq_ignore_ascii_case("no-cache") {
            return Some(0);
        }
        if name.eq_ignore_ascii_case("s-maxage") {
            if let Some(ttl) = raw_value.and_then(parse_delta_seconds) {
                return Some(ttl);
            }
        } else if name.eq_ignore_ascii_case("max-age") {
            max_age = raw_value.and_then(parse_delta_seconds);
        }
    }
    max_age
}

fn parse_delta_seconds(value: &str) -> Option<u64> {
    value.trim_matches('"').parse::<u64>().ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn policy() -> MetadataTtlPolicy {
        MetadataTtlPolicy::new(60, 60, 5, 5)
    }

    #[test]
    fn uses_default_when_cache_control_absent() {
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, None, 1000),
            1060
        );
        assert_eq!(
            policy().expires_at(MetadataTtlKind::NotFound, None, 1000),
            1005
        );
    }

    #[test]
    fn parses_and_clamps_max_age() {
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, Some("max-age=30"), 1000),
            1030
        );
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, Some("max-age=300"), 1000),
            1060
        );
    }

    #[test]
    fn s_maxage_overrides_max_age() {
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, Some("max-age=50, s-maxage=7"), 1000,),
            1007
        );
    }

    #[test]
    fn no_cache_directives_make_page_immediately_expired() {
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, Some("no-cache, max-age=30"), 1000),
            1000
        );
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, Some("no-store"), 1000),
            1000
        );
    }

    #[test]
    fn malformed_cache_control_falls_back_to_default() {
        assert_eq!(
            policy().expires_at(MetadataTtlKind::Found, Some("max-age=abc"), 1000),
            1060
        );
    }

    #[test]
    fn head_200_builds_found_metadata() {
        let meta = metadata_from_head(
            StatusCode::OK,
            Some(123),
            Some("max-age=30"),
            policy(),
            1000,
            "missing",
            "non-ok",
        )
        .unwrap();
        assert!(meta.is_found());
        assert_eq!(meta.length, 123);
        assert_eq!(meta.expires_at_unix_secs, Some(1030));
    }

    #[test]
    fn head_404_builds_negative_metadata_without_length() {
        let meta = metadata_from_head(
            StatusCode::NOT_FOUND,
            None,
            Some("max-age=30"),
            policy(),
            1000,
            "missing",
            "non-ok",
        )
        .unwrap();
        assert!(meta.is_not_found());
        assert_eq!(meta.expires_at_unix_secs, Some(1005));
    }
}
