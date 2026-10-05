//! Single byte-range grammar. Numeric canonicalization and limits belong to the caller.
use crate::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ByteRange {
    Closed { first: u64, last: u64 },
    From(u64),
    Suffix(u64),
}

impl ByteRange {
    /// Parse one range without trimming, normalizing, or merging. The numeric
    /// parser receives exact wire bytes, including empty or malformed components.
    pub fn parse_with<E: From<Error>>(
        value: &[u8],
        mut decimal: impl FnMut(&[u8]) -> Result<u64, E>,
    ) -> Result<Self, E> {
        let bounds = value.strip_prefix(b"bytes=").ok_or(Error::Malformed)?;
        let separator = bounds
            .iter()
            .position(|&b| b == b'-')
            .ok_or(Error::Malformed)?;
        let (first, rest) = bounds.split_at(separator);
        let last = &rest[1..];
        match (first.is_empty(), last.is_empty()) {
            (true, true) => Err(Error::Malformed.into()),
            (true, false) => Ok(Self::Suffix(decimal(last)?)),
            (false, true) => Ok(Self::From(decimal(first)?)),
            (false, false) => {
                let (first, last) = (decimal(first)?, decimal(last)?);
                if first > last {
                    return Err(Error::Malformed.into());
                }
                Ok(Self::Closed { first, last })
            }
        }
    }
}

/// A satisfied Content-Range with a known complete representation length.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ContentRange {
    pub first: u64,
    pub last: u64,
    pub total: u64,
}

impl ContentRange {
    /// Parse exact byte syntax and enforce first <= last < total. Wildcard
    /// representations and unsatisfied ranges are separate caller contracts.
    pub fn parse_with<E: From<Error>>(
        value: &[u8],
        mut decimal: impl FnMut(&[u8]) -> Result<u64, E>,
    ) -> Result<Self, E> {
        let value = value.strip_prefix(b"bytes ").ok_or(Error::Malformed)?;
        let slash = value
            .iter()
            .position(|&b| b == b'/')
            .ok_or(Error::Malformed)?;
        let bounds = &value[..slash];
        let dash = bounds
            .iter()
            .position(|&b| b == b'-')
            .ok_or(Error::Malformed)?;
        let first = decimal(&bounds[..dash])?;
        let last = decimal(&bounds[dash + 1..])?;
        let total = decimal(&value[slash + 1..])?;
        if first > last || last >= total {
            return Err(Error::Malformed.into());
        }
        Ok(Self { first, last, total })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn canonical(bytes: &[u8]) -> Result<u64, Error> {
        if bytes.is_empty() || (bytes.len() > 1 && bytes[0] == b'0') {
            return Err(Error::Malformed);
        }
        bytes.iter().try_fold(0u64, |n, b| {
            if !b.is_ascii_digit() {
                return Err(Error::Malformed);
            }
            n.checked_mul(10)
                .and_then(|n| n.checked_add(u64::from(b - b'0')))
                .ok_or(Error::Malformed)
        })
    }

    #[test]
    fn single_range_preserves_numeric_policy_and_zero_suffix() {
        for (wire, expected) in [
            ("bytes=0-1", ByteRange::Closed { first: 0, last: 1 }),
            ("bytes=2-", ByteRange::From(2)),
            ("bytes=-0", ByteRange::Suffix(0)),
            ("bytes=-18446744073709551615", ByteRange::Suffix(u64::MAX)),
        ] {
            assert_eq!(
                ByteRange::parse_with(wire.as_bytes(), canonical),
                Ok(expected)
            );
        }
        for wire in [
            "bytes=-",
            "Bytes=0-1",
            " bytes=0-1",
            "bytes=0-1 ",
            "bytes=00-1",
            "bytes=-01",
            "bytes=2-1",
            "bytes=0-1,2-3",
            "bytes=+1-2",
            "bytes=1--2",
            "bytes=-18446744073709551616",
        ] {
            assert_eq!(
                ByteRange::parse_with(wire.as_bytes(), canonical),
                Err(Error::Malformed),
                "{wire}"
            );
        }
    }

    #[test]
    fn content_range_checks_structure_and_exact_numeric_inputs() {
        assert_eq!(
            ContentRange::parse_with(b"bytes 0-1/2", canonical),
            Ok(ContentRange {
                first: 0,
                last: 1,
                total: 2
            })
        );
        for wire in [
            "bytes */2",
            "bytes 0-1/*",
            "bytes 0-1/1",
            "bytes 2-1/3",
            "bytes 00-1/2",
            "bytes 0-01/2",
            "bytes 0-1/02",
            "bytes 0-1/2 ",
            "bytes  0-1/2",
            "bytes 0-1/2/3",
            "bytes 0--1/2",
            "bytes 0-0/0",
        ] {
            assert_eq!(
                ContentRange::parse_with(wire.as_bytes(), canonical),
                Err(Error::Malformed),
                "{wire}"
            );
        }
        let mut seen = Vec::new();
        let result = ContentRange::parse_with(b"bytes 01-02/03", |bytes| {
            seen.push(bytes.to_vec());
            Ok::<_, Error>(seen.len() as u64)
        });
        assert!(result.is_ok());
        assert_eq!(seen, [b"01", b"02", b"03"]);
    }
}
