//! Versioned metadata. TTL controls new unpinned admission, never page eviction.

use super::{
    MAX_WIRE_INTEGER,
    envelope::PageEnvelope,
    identity::{ObjectVersion, PageId},
    parse_decimal,
    range::PAGE_BYTES,
};
use crate::error::{Error, Result};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Absolute Unix-millisecond deadline in 0..=i64::MAX, never a stream deadline.
/// The public tuple field is retained for compatibility; validate before encoding.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ExpiresAt(pub std::time::SystemTime);

impl ExpiresAt {
    pub fn from_unix_millis(milliseconds: u64) -> Result<Self> {
        if milliseconds > MAX_WIRE_INTEGER {
            return Err(Error::InvalidRequest);
        }
        UNIX_EPOCH
            .checked_add(Duration::from_millis(milliseconds))
            .map(Self)
            .ok_or(Error::InvalidRequest)
    }

    /// Reject pre-epoch, overflowing, or sub-millisecond times without rounding.
    pub fn to_unix_millis(self) -> Result<u64> {
        let duration = self
            .0
            .duration_since(UNIX_EPOCH)
            .map_err(|_| Error::InvalidRequest)?;
        if duration.subsec_nanos() % 1_000_000 != 0
            || duration.as_millis() > u128::from(MAX_WIRE_INTEGER)
        {
            return Err(Error::InvalidRequest);
        }
        Ok(duration.as_millis() as u64)
    }

    pub fn parse(value: &[u8]) -> Result<Self> {
        Self::from_unix_millis(parse_decimal(value)?)
    }

    pub fn to_header(self) -> Result<String> {
        Ok(self.to_unix_millis()?.to_string())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ObjectMetadata {
    pub version: ObjectVersion,
    pub length: u64,
    pub expires_at: ExpiresAt,
}

/// Immutable descriptor keyed by the complete object version, never by object alone.
/// Retained by page copies independently of the bounded page-zero metadata catalog.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VersionMetadata {
    pub version: ObjectVersion,
    pub length: u64,
}

/// Page-zero owner's volatile current-version pointer. Page hits and pinned probes
/// cannot publish this value; only a fresh metadata revalidation may do so.
/// It is deliberately absent from checkpoints and invalidated on clock uncertainty.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CurrentVersion {
    pub version: ObjectVersion,
    pub expires_at: ExpiresAt,
}

impl ObjectMetadata {
    pub fn validate(&self) -> Result<()> {
        if self.length > MAX_WIRE_INTEGER {
            return Err(Error::InvalidRequest);
        }
        self.expires_at.to_unix_millis()?;
        Ok(())
    }

    pub fn immutable(&self) -> VersionMetadata {
        VersionMetadata {
            version: self.version.clone(),
            length: self.length,
        }
    }
}

impl VersionMetadata {
    /// A recovered or retained immutable descriptor can answer a pin, but carries
    /// no reusable freshness claim. Never infer total length from a page's bytes.
    pub fn for_pin(&self) -> ObjectMetadata {
        ObjectMetadata {
            version: self.version.clone(),
            length: self.length,
            expires_at: ExpiresAt(UNIX_EPOCH),
        }
    }

    /// Structural consistency only, not authentication of origin/peer/disk input.
    pub fn validate_page(&self, envelope: &PageEnvelope) -> Result<()> {
        envelope.validate()?;
        if self.page_length(&envelope.page)? != envelope.plaintext_length {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }

    pub fn page_length(&self, page: &PageId) -> Result<u32> {
        let start = page
            .number
            .0
            .checked_mul(PAGE_BYTES)
            .ok_or(Error::CorruptRecord)?;
        if self.length > MAX_WIRE_INTEGER || page.version != self.version || start >= self.length {
            return Err(Error::CorruptRecord);
        }
        Ok((self.length - start).min(PAGE_BYTES) as u32)
    }
}

impl CurrentVersion {
    /// Join the freshness pointer to exactly its immutable descriptor. Expiry is
    /// inclusive; a zero-TTL refresh can admit its own waiters, not a cache hit.
    pub fn resolve(
        &self,
        descriptor: &VersionMetadata,
        now: SystemTime,
    ) -> Result<Option<ObjectMetadata>> {
        if self.version != descriptor.version {
            return Err(Error::VersionUnavailable);
        }
        if descriptor.length > MAX_WIRE_INTEGER {
            return Err(Error::CorruptRecord);
        }
        self.expires_at.to_unix_millis()?;
        Ok((now < self.expires_at.0).then(|| ObjectMetadata {
            version: descriptor.version.clone(),
            length: descriptor.length,
            expires_at: self.expires_at,
        }))
    }
}

/// Distinguishes freshness admission from an explicit immutable-version pin.
#[derive(Clone, Debug)]
pub enum MetadataSelector {
    Fresh,
    Pinned(super::identity::StrongEtag),
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{
        envelope::{KeyId, Nonce},
        identity::{CacheId, CacheKey, ObjectId, PageNumber, StrongEtag},
    };
    use std::time::Duration;

    #[test]
    fn expiry_round_trips_sdk_epoch_milliseconds_including_signed63_maximum() {
        for milliseconds in [0, 1, 999, 1000, 123_456_789, MAX_WIRE_INTEGER] {
            let value = milliseconds.to_string();
            let expiry = ExpiresAt::parse(value.as_bytes()).unwrap();
            assert_eq!(expiry.to_unix_millis(), Ok(milliseconds));
            assert_eq!(expiry.to_header(), Ok(value));
        }
    }

    #[test]
    fn expiry_rejects_noncanonical_precision_and_overflow() {
        for value in [
            "",
            "-1",
            "+1",
            "01",
            " 1",
            "1 ",
            "1.0",
            "9223372036854775808",
            "18446744073709551616",
        ] {
            assert_eq!(
                ExpiresAt::parse(value.as_bytes()),
                Err(Error::InvalidRequest)
            );
        }
        assert_eq!(
            ExpiresAt::from_unix_millis(MAX_WIRE_INTEGER + 1),
            Err(Error::InvalidRequest)
        );
        for time in [
            UNIX_EPOCH - Duration::from_millis(1),
            UNIX_EPOCH + Duration::from_nanos(1),
            UNIX_EPOCH + Duration::from_millis(MAX_WIRE_INTEGER + 1),
        ] {
            assert_eq!(ExpiresAt(time).to_unix_millis(), Err(Error::InvalidRequest));
        }
    }

    #[test]
    fn metadata_validation_rejects_invalid_public_numeric_fields() {
        let mut metadata = descriptor("v1", MAX_WIRE_INTEGER).for_pin();
        assert_eq!(metadata.validate(), Ok(()));
        metadata.length += 1;
        assert_eq!(metadata.validate(), Err(Error::InvalidRequest));
        metadata.length = 0;
        metadata.expires_at = ExpiresAt(UNIX_EPOCH + Duration::from_nanos(1));
        assert_eq!(metadata.validate(), Err(Error::InvalidRequest));
    }

    #[test]
    fn current_version_rejects_invalid_descriptors_before_fresh_admission() {
        let descriptor = descriptor("v1", MAX_WIRE_INTEGER + 1);
        let mut current = CurrentVersion {
            version: descriptor.version.clone(),
            expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_secs(1)),
        };
        assert_eq!(
            current.resolve(&descriptor, UNIX_EPOCH),
            Err(Error::CorruptRecord)
        );
        let valid = VersionMetadata {
            length: 1,
            ..descriptor
        };
        current.expires_at = ExpiresAt(UNIX_EPOCH + Duration::from_nanos(1));
        assert_eq!(
            current.resolve(&valid, UNIX_EPOCH),
            Err(Error::InvalidRequest)
        );
    }

    pub(crate) fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
        }
    }

    #[test]
    fn freshness_never_substitutes_another_versions_length() {
        let old = descriptor("old", 7);
        let new = descriptor("new", 500);
        let now = UNIX_EPOCH + Duration::from_secs(100);
        let current = CurrentVersion {
            version: new.version.clone(),
            expires_at: ExpiresAt(now + Duration::from_secs(1)),
        };
        assert_eq!(current.resolve(&old, now), Err(Error::VersionUnavailable));
        assert_eq!(current.resolve(&new, now).unwrap().unwrap().length, 500);
        assert_eq!(old.for_pin().length, 7);
        assert_eq!(old.for_pin().expires_at, ExpiresAt(UNIX_EPOCH));
    }

    #[test]
    fn expired_and_zero_ttl_pointers_cannot_admit_fresh_reads() {
        let descriptor = descriptor("v1", 0);
        let deadline = UNIX_EPOCH + Duration::from_secs(100);
        let current = CurrentVersion {
            version: descriptor.version.clone(),
            expires_at: ExpiresAt(deadline),
        };
        for now in [deadline, deadline + Duration::from_secs(1)] {
            assert_eq!(current.resolve(&descriptor, now).unwrap(), None);
        }
        assert_eq!(descriptor.for_pin().length, 0);
    }

    #[test]
    fn page_bounds_use_total_version_length_and_reject_empty_overflow_and_mismatch() {
        let descriptor = descriptor("v1", PAGE_BYTES + 3);
        let mut envelope = PageEnvelope {
            page: PageId {
                version: descriptor.version.clone(),
                number: PageNumber(1),
            },
            key_id: KeyId([0; 16]),
            nonce: Nonce([0; 24]),
            plaintext_length: 3,
            ciphertext_length: 19,
        };
        assert_eq!(descriptor.validate_page(&envelope), Ok(()));
        envelope.ciphertext_length = 18;
        assert_eq!(
            descriptor.validate_page(&envelope),
            Err(Error::CorruptRecord)
        );
        envelope.ciphertext_length = 19;
        envelope.plaintext_length = 4;
        assert_eq!(
            descriptor.validate_page(&envelope),
            Err(Error::CorruptRecord)
        );
        envelope.plaintext_length = 3;
        for number in [2, u64::MAX] {
            envelope.page.number = PageNumber(number);
            assert_eq!(
                descriptor.validate_page(&envelope),
                Err(Error::CorruptRecord)
            );
        }
        envelope.page.number = PageNumber(0);
        let empty = VersionMetadata {
            length: 0,
            ..descriptor.clone()
        };
        assert_eq!(empty.validate_page(&envelope), Err(Error::CorruptRecord));
        envelope.page.version.etag = StrongEtag::test_value("other");
        assert_eq!(
            descriptor.validate_page(&envelope),
            Err(Error::CorruptRecord)
        );
    }
}
