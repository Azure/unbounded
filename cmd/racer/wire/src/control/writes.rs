//! Per-cache write settings carried in cache publications.
//!
//! A cache publishes `writes` only when its ClusterCache sets `spec.writes`.
//! Caches without it keep their canonical bytes, and older catalogs that never
//! carry the field still decode, with writes disabled.

use crate::control::{Error, Result};
use std::time::Duration;

/// Default for `WriteDefinition::replicas`.
pub const DEFAULT_WRITE_REPLICAS: u8 = 2;

/// Highest accepted `WriteDefinition::replicas`.
pub const MAX_WRITE_REPLICAS: u8 = 3;

/// Default for `WriteDefinition::max_dirty_percent`.
pub const DEFAULT_WRITE_MAX_DIRTY_PERCENT: u8 = 30;

/// Highest accepted `WriteDefinition::max_dirty_percent`.
pub const MAX_WRITE_MAX_DIRTY_PERCENT: u8 = 90;

/// Default for `WriteDefinition::hold_ttl`.
pub const DEFAULT_WRITE_HOLD_TTL: Duration = Duration::from_secs(24 * 3600);

/// Longest accepted `WriteDefinition::hold_ttl`, matching the store's
/// `MAX_HOLD_TTL` of 30 days.
pub const MAX_WRITE_HOLD_TTL: Duration = Duration::from_secs(720 * 3600);

/// Default for `WriteDefinition::upload_max_concurrent_blocks`.
pub const DEFAULT_WRITE_UPLOAD_MAX_CONCURRENT_BLOCKS: u32 = 16;

/// Highest accepted `WriteDefinition::upload_max_concurrent_blocks`.
pub const MAX_WRITE_UPLOAD_MAX_CONCURRENT_BLOCKS: u32 = 1024;

/// When a written object is uploaded to the cache's origin.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, PartialEq)]
pub enum UploadMode {
    /// Upload after the write commits in the cache.
    #[default]
    Auto,

    /// Upload only after the client asks with `persist`.
    Manual,

    /// Never upload; the object lives only in the cache until its hold ends.
    Never,
}

impl UploadMode {
    /// Wire and header spelling: `auto`, `manual`, or `never`.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Manual => "manual",
            Self::Never => "never",
        }
    }

    /// Parse the wire spelling.
    pub fn parse(s: &str) -> Result<Self> {
        match s {
            "auto" => Ok(Self::Auto),
            "manual" => Ok(Self::Manual),
            "never" => Ok(Self::Never),
            _ => Err(Error::InvalidRequest),
        }
    }
}

/// Write settings of one cache. Present only when writes are enabled.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WriteDefinition {
    /// Copies required in the cache before a write is acknowledged, 1 through
    /// `MAX_WRITE_REPLICAS`.
    pub replicas: u8,

    /// Share of the cache's node disk share that may hold not yet uploaded
    /// data, 1 through `MAX_WRITE_MAX_DIRTY_PERCENT`.
    pub max_dirty_percent: u8,

    /// Default hold for written objects. Positive, at most
    /// `MAX_WRITE_HOLD_TTL`, millisecond precision.
    pub hold_ttl: Duration,

    /// Default upload mode for writes that do not ask for one.
    pub upload_mode: UploadMode,

    /// Upload bandwidth per node for this cache, in bytes per second. Zero is unlimited.
    pub upload_max_bytes_per_second: u64,

    /// Concurrent block uploads per node for this cache, 1 through
    /// `MAX_WRITE_UPLOAD_MAX_CONCURRENT_BLOCKS`.
    pub upload_max_concurrent_blocks: u32,
}

impl Default for WriteDefinition {
    fn default() -> Self {
        Self {
            replicas: DEFAULT_WRITE_REPLICAS,
            max_dirty_percent: DEFAULT_WRITE_MAX_DIRTY_PERCENT,
            hold_ttl: DEFAULT_WRITE_HOLD_TTL,
            upload_mode: UploadMode::Auto,
            upload_max_bytes_per_second: 0,
            upload_max_concurrent_blocks: DEFAULT_WRITE_UPLOAD_MAX_CONCURRENT_BLOCKS,
        }
    }
}

impl WriteDefinition {
    /// Validate bounds and millisecond precision. Whether the cache can use
    /// these settings (mode and origin) is checked by `validate_definitions`.
    pub fn validate(&self) -> Result<()> {
        let ok = (1..=MAX_WRITE_REPLICAS).contains(&self.replicas)
            && (1..=MAX_WRITE_MAX_DIRTY_PERCENT).contains(&self.max_dirty_percent)
            && !self.hold_ttl.is_zero()
            && self.hold_ttl <= MAX_WRITE_HOLD_TTL
            && self.hold_ttl.subsec_nanos().is_multiple_of(1_000_000)
            && (1..=MAX_WRITE_UPLOAD_MAX_CONCURRENT_BLOCKS)
                .contains(&self.upload_max_concurrent_blocks);
        ok.then_some(()).ok_or(Error::InvalidRequest)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_are_valid_and_bounds_reject() {
        let d = WriteDefinition::default();
        d.validate().unwrap();
        let bad = [
            WriteDefinition {
                replicas: 0,
                ..d.clone()
            },
            WriteDefinition {
                replicas: 4,
                ..d.clone()
            },
            WriteDefinition {
                max_dirty_percent: 0,
                ..d.clone()
            },
            WriteDefinition {
                max_dirty_percent: 91,
                ..d.clone()
            },
            WriteDefinition {
                hold_ttl: Duration::ZERO,
                ..d.clone()
            },
            WriteDefinition {
                hold_ttl: MAX_WRITE_HOLD_TTL + Duration::from_millis(1),
                ..d.clone()
            },
            WriteDefinition {
                hold_ttl: Duration::from_micros(1500),
                ..d.clone()
            },
            WriteDefinition {
                upload_max_concurrent_blocks: 0,
                ..d.clone()
            },
            WriteDefinition {
                upload_max_concurrent_blocks: 1025,
                ..d.clone()
            },
        ];
        for b in bad {
            assert_eq!(b.validate(), Err(Error::InvalidRequest), "{b:?}");
        }
        let edge = WriteDefinition {
            replicas: 3,
            max_dirty_percent: 90,
            hold_ttl: MAX_WRITE_HOLD_TTL,
            upload_max_bytes_per_second: u64::MAX,
            upload_max_concurrent_blocks: 1024,
            ..d
        };
        edge.validate().unwrap();
    }

    #[test]
    fn upload_mode_spelling() {
        for m in [UploadMode::Auto, UploadMode::Manual, UploadMode::Never] {
            assert_eq!(UploadMode::parse(m.as_str()), Ok(m));
        }
        assert!(UploadMode::parse("Auto").is_err());
        assert!(UploadMode::parse("").is_err());
    }
}
