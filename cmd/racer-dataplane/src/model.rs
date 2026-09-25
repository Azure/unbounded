//! Shared semantic values. This layer imports neither I/O nor read policy.

pub mod context;
pub mod envelope;
pub mod identity;
pub mod limits;
pub mod metadata;
pub mod range;

/// Client/origin v1 maximum size of one field value, in bytes.
pub const MAX_FIELD_BYTES: usize = 8192;
/// Client/origin v1 lengths, offsets, and Unix milliseconds are nonnegative i64s.
pub const MAX_WIRE_INTEGER: u64 = i64::MAX as u64;

/// Canonical decimal: no sign, whitespace, leading zeros, or values above i64::MAX.
pub(crate) fn parse_decimal(value: &[u8]) -> crate::error::Result<u64> {
    use crate::error::Error;

    if value.is_empty() || value.len() > 19 || (value.len() > 1 && value[0] == b'0') {
        return Err(Error::InvalidRequest);
    }
    let mut result = 0u64;
    for &byte in value {
        if !byte.is_ascii_digit() {
            return Err(Error::InvalidRequest);
        }
        result = result
            .checked_mul(10)
            .and_then(|n| n.checked_add(u64::from(byte - b'0')))
            .filter(|&n| n <= MAX_WIRE_INTEGER)
            .ok_or(Error::InvalidRequest)?;
    }
    Ok(result)
}
