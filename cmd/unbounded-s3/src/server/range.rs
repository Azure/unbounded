// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

/// Parsed byte range within an object.
///
/// An S3 GET with Range contains a single byte-range-spec.
/// Multi-range requests (`bytes=0-1,5-6`) are not supported
/// and treated as invalid.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct ByteRange {
    pub offset: u64,
    pub len: u64,
}

/// Parse a single `Range` header value supporting:
///
/// - `bytes=N-M` — the inclusive interval `[N, M]`.
/// - `bytes=N-`  — suffix from `N` to EOF.
/// - `-N`        — the last `N` bytes of the object.
///
/// Returns `Err(Unsatisfiable)` when the requested range does not
/// intersect the object (RFC 9110 §14.1.1). Returns `Err(Invalid)`
/// when the syntax is wrong or multi-range is attempted.
pub fn parse_range_header(
    header: Option<&str>,
    object_size: u64,
) -> Result<ByteRange, RangeError> {
    let h = match header {
        Some(h) => h.trim(),
        None => return Ok(ByteRange { offset: 0, len: object_size }),
    };

    if !h.starts_with("bytes=") {
        return Err(RangeError::Invalid);
    }
    let spec = &h["bytes=".len()..].trim();

    // Reject multi-range
    if spec.contains(',') {
        return Err(RangeError::Invalid);
    }

    if let Some((start_str, end_str)) = spec.split_once('-') {
        let (start_str, end_str) = (start_str.trim(), end_str.trim());
        if start_str.is_empty() && end_str.is_empty() {
            return Err(RangeError::Invalid);
        }

        if start_str.is_empty() {
            // -N : last N bytes
            let n: u64 = parse_u64(end_str).ok_or(RangeError::Invalid)?;
            if object_size == 0 {
                return Err(RangeError::Unsatisfiable);
            }
            if n == 0 {
                return Err(RangeError::Unsatisfiable);
            }
            if n >= object_size {
                return Ok(ByteRange { offset: 0, len: object_size });
            }
            let offset = object_size - n;
            return Ok(ByteRange { offset, len: n });
        }

        let start: u64 = parse_u64(start_str).ok_or(RangeError::Invalid)?;
        if start >= object_size {
            return Err(RangeError::Unsatisfiable);
        }

        if end_str.is_empty() {
            // N- : from N to EOF
            return Ok(ByteRange {
                offset: start,
                len: object_size - start,
            });
        }

        let end: u64 = parse_u64(end_str).ok_or(RangeError::Invalid)?;
        if end < start {
            return Err(RangeError::Unsatisfiable);
        }
        // `end` is inclusive per RFC. Clip to EOF first, before doing
        // any `+ 1` arithmetic, so a header like `bytes=0-u64::MAX`
        // cannot overflow. After clipping, `end < object_size`, so
        // `end - start + 1 <= object_size - start` and fits in `u64`.
        if end >= object_size {
            return Ok(ByteRange {
                offset: start,
                len: object_size - start,
            });
        }
        Ok(ByteRange {
            offset: start,
            len: end - start + 1,
        })
    } else {
        Err(RangeError::Invalid)
    }
}

fn parse_u64(s: &str) -> Option<u64> {
    if s.is_empty() || s.as_bytes().first() == Some(&b'+') || s.as_bytes().first() == Some(&b'-')
    {
        return None;
    }
    s.chars().all(|c| c.is_ascii_digit()).then(|| s.parse::<u64>().ok())?
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RangeError {
    /// The syntax is malformed or unsupported (multi-range).
    Invalid,
    /// The range does not intersect the object (416).
    Unsatisfiable,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_header_returns_full() {
        assert_eq!(
            parse_range_header(None, 1000).unwrap(),
            ByteRange { offset: 0, len: 1000 },
        );
    }

    #[test]
    fn full_object_range() {
        let r = parse_range_header(Some("bytes=0-"), 500).unwrap();
        assert_eq!(r, ByteRange { offset: 0, len: 500 });
        let r = parse_range_header(Some("bytes=0-499"), 500).unwrap();
        assert_eq!(r, ByteRange { offset: 0, len: 500 });
    }

    #[test]
    fn middle_range() {
        let r = parse_range_header(Some("bytes=10-19"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 10, len: 10 });
    }

    #[test]
    fn clipped_to_eof() {
        let r = parse_range_header(Some("bytes=90-199"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 90, len: 10 });
    }

    #[test]
    fn suffix_range() {
        let r = parse_range_header(Some("bytes=-10"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 90, len: 10 });
    }

    #[test]
    fn suffix_larger_than_object_returns_full() {
        let r = parse_range_header(Some("bytes=-200"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 0, len: 100 });
    }

    #[test]
    fn start_past_end_is_unsatisfiable() {
        assert_eq!(
            parse_range_header(Some("bytes=100-"), 100),
            Err(RangeError::Unsatisfiable),
        );
        assert_eq!(
            parse_range_header(Some("bytes=100-200"), 100),
            Err(RangeError::Unsatisfiable),
        );
    }

    #[test]
    fn end_before_start_is_unsatisfiable() {
        assert_eq!(
            parse_range_header(Some("bytes=10-5"), 100),
            Err(RangeError::Unsatisfiable),
        );
    }

    #[test]
    fn zero_byte_object_full() {
        let r = parse_range_header(None, 0).unwrap();
        assert_eq!(r, ByteRange { offset: 0, len: 0 });
    }

    #[test]
    fn zero_byte_object_range_is_unsatisfiable() {
        assert_eq!(
            parse_range_header(Some("bytes=0-"), 0),
            Err(RangeError::Unsatisfiable),
        );
        assert_eq!(
            parse_range_header(Some("bytes=-1"), 0),
            Err(RangeError::Unsatisfiable),
        );
    }

    #[test]
    fn multi_range_is_invalid() {
        assert_eq!(
            parse_range_header(Some("bytes=0-1,5-6"), 100),
            Err(RangeError::Invalid),
        );
    }

    #[test]
    fn bad_prefix_is_invalid() {
        assert_eq!(
            parse_range_header(Some("range=0-"), 100),
            Err(RangeError::Invalid),
        );
    }

    #[test]
    fn empty_spec_is_invalid() {
        assert_eq!(
            parse_range_header(Some("bytes="), 100),
            Err(RangeError::Invalid),
        );
    }

    #[test]
    fn minus_only_is_invalid() {
        assert_eq!(
            parse_range_header(Some("bytes=-"), 100),
            Err(RangeError::Invalid),
        );
    }

    // ---- u64 end overflow regressions --------------------------------------

    #[test]
    fn end_u64_max_from_zero_clips_to_eof() {
        // `bytes=0-18446744073709551615` must not overflow `end + 1`;
        // the range is clipped to the whole object instead.
        let r = parse_range_header(Some("bytes=0-18446744073709551615"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 0, len: 100 });
    }

    #[test]
    fn end_u64_max_from_offset_clips_to_eof() {
        let r = parse_range_header(Some("bytes=5-18446744073709551615"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 5, len: 95 });
    }

    #[test]
    fn single_byte_range_against_one_byte_object() {
        // `bytes=0-0` over a 1-byte object exercises `end - start + 1`
        // with `end + 1 == object_size`.
        let r = parse_range_header(Some("bytes=0-0"), 1).unwrap();
        assert_eq!(r, ByteRange { offset: 0, len: 1 });
    }

    #[test]
    fn last_byte_inclusive_at_eof() {
        // `bytes=99-99` over a 100-byte object: `end + 1 == object_size`
        // exactly, so the clip branch and the arithmetic branch must
        // both agree.
        let r = parse_range_header(Some("bytes=99-99"), 100).unwrap();
        assert_eq!(r, ByteRange { offset: 99, len: 1 });
    }
}
