//! Bounded byte codecs and optional owner-local REST transport.
//!
//! Codec callers choose byte order, bounds, and error mapping. The `rest` feature
//! adds TLS JSON HTTP on caller-owned readiness, deadlines, and admission leases.

#[cfg(feature = "rest")]
pub mod rest;

// Shared REST fixtures use the same imports in unit and integration builds.
#[cfg(all(test, feature = "rest"))]
extern crate self as wire_codec;

/// A malformed field or a caller-supplied bound prevented a codec operation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// A fixed-width read exceeds the remaining input.
    Truncated,

    /// A length, count, or output buffer exceeds its allowed bound.
    Limit,

    /// Hex input has the wrong length or is not canonical lowercase ASCII.
    InvalidHex,
}

/// The result of reading or writing a bounded field.
pub type Result<T> = std::result::Result<T, Error>;

/// The byte order used for integer fields and length prefixes.
#[derive(Clone, Copy, Debug)]
pub enum Endian {
    /// Least significant byte first.
    Little,

    /// Most significant byte first.
    Big,
}

/// Reads borrowed fields without advancing past the input slice.
pub struct Reader<'a> {
    remaining: &'a [u8],

    endian: Endian,
}

/// Appends whole fields, including existing buffer bytes in the total bound.
pub struct Writer<'a> {
    bytes: &'a mut Vec<u8>,

    maximum: usize,

    endian: Endian,
}

impl<'a> Reader<'a> {
    /// Uses the input slice as the total read bound and the chosen byte order.
    pub fn new(bytes: &'a [u8], endian: Endian) -> Self {
        Self {
            remaining: bytes,
            endian,
        }
    }

    /// Returns the unread input without advancing the cursor.
    pub fn remaining(&self) -> &'a [u8] {
        self.remaining
    }

    /// Borrows exactly `length` bytes, leaving the cursor unchanged on failure.
    pub fn take(&mut self, length: usize) -> Result<&'a [u8]> {
        let (value, rest) = self
            .remaining
            .split_at_checked(length)
            .ok_or(Error::Truncated)?;
        self.remaining = rest;
        Ok(value)
    }

    /// Copies a fixed-width field, leaving the cursor unchanged on failure.
    pub fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::Truncated)
    }

    /// Reads a 32-bit integer in the chosen byte order.
    pub fn u32(&mut self) -> Result<u32> {
        Ok(match self.endian {
            Endian::Little => u32::from_le_bytes(self.array()?),
            Endian::Big => u32::from_be_bytes(self.array()?),
        })
    }

    /// Reads a 64-bit integer in the chosen byte order.
    pub fn u64(&mut self) -> Result<u64> {
        Ok(match self.endian {
            Endian::Little => u64::from_le_bytes(self.array()?),
            Endian::Big => u64::from_be_bytes(self.array()?),
        })
    }

    /// Read a u32 count bounded by both caller policy and remaining storage.
    ///
    /// A zero minimum permits zero-size items while still enforcing `maximum`.
    /// A complete count is consumed even when it exceeds either bound.
    pub fn count(&mut self, maximum: usize, minimum_bytes: usize) -> Result<usize> {
        let count = usize::try_from(self.u32()?).map_err(|_| Error::Limit)?;
        if count > maximum || (minimum_bytes != 0 && count > self.remaining.len() / minimum_bytes) {
            return Err(Error::Limit);
        }
        Ok(count)
    }

    /// Borrows a u32-length-prefixed field bounded by `maximum` and input size.
    ///
    /// A complete prefix is consumed even when the field exceeds either bound.
    pub fn length_prefixed(&mut self, maximum: usize) -> Result<&'a [u8]> {
        let length = self.count(maximum, 1)?;
        self.take(length)
    }
}

impl<'a> Writer<'a> {
    /// Borrows an output buffer with a total size bound and chosen byte order.
    pub fn new(bytes: &'a mut Vec<u8>, maximum: usize, endian: Endian) -> Self {
        Self {
            bytes,
            maximum,
            endian,
        }
    }

    /// Appends bytes, leaving the output unchanged if its total bound is exceeded.
    pub fn bytes(&mut self, value: &[u8]) -> Result<()> {
        self.append([value])
    }

    /// Appends a 32-bit integer in the chosen byte order.
    pub fn u32(&mut self, value: u32) -> Result<()> {
        self.bytes(&match self.endian {
            Endian::Little => value.to_le_bytes(),
            Endian::Big => value.to_be_bytes(),
        })
    }

    /// Appends a 64-bit integer in the chosen byte order.
    pub fn u64(&mut self, value: u64) -> Result<()> {
        self.bytes(&match self.endian {
            Endian::Little => value.to_le_bytes(),
            Endian::Big => value.to_be_bytes(),
        })
    }

    /// Appends a count only if it fits in a 32-bit integer and the output bound.
    pub fn count(&mut self, value: usize) -> Result<()> {
        self.u32(u32::try_from(value).map_err(|_| Error::Limit)?)
    }

    /// Appends a u32 length and its bytes, bounded by `maximum` and output size.
    ///
    /// A failed field append leaves the entire buffer unchanged.
    pub fn length_prefixed(&mut self, value: &[u8], maximum: usize) -> Result<()> {
        if value.len() > maximum {
            return Err(Error::Limit);
        }
        let length = u32::try_from(value.len()).map_err(|_| Error::Limit)?;
        let prefix = match self.endian {
            Endian::Little => length.to_le_bytes(),
            Endian::Big => length.to_be_bytes(),
        };
        self.append([&prefix, value])
    }

    /// Checks every part before appending any bytes of a field.
    fn append<const N: usize>(&mut self, parts: [&[u8]; N]) -> Result<()> {
        let length = parts.iter().try_fold(self.bytes.len(), |length, part| {
            length.checked_add(part.len())
        });
        if length.is_none_or(|length| length > self.maximum) {
            return Err(Error::Limit);
        }
        for part in parts {
            self.bytes.extend_from_slice(part);
        }
        Ok(())
    }
}

/// Decodes exactly `N` bytes of canonical lowercase ASCII hex.
pub fn decode_hex<const N: usize>(value: &[u8]) -> Result<[u8; N]> {
    if N.checked_mul(2) != Some(value.len()) {
        return Err(Error::InvalidHex);
    }
    let nibble = |byte| match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(Error::InvalidHex),
    };
    let mut bytes = [0; N];
    for (output, pair) in bytes.iter_mut().zip(value.chunks_exact(2)) {
        *output = nibble(pair[0])? << 4 | nibble(pair[1])?;
    }
    Ok(bytes)
}

/// Encodes bytes as canonical lowercase ASCII hex without a prefix.
pub fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut value = String::new();
    for &byte in bytes {
        value.push(char::from(HEX[usize::from(byte >> 4)]));
        value.push(char::from(HEX[usize::from(byte & 15)]));
    }
    value
}

/// Byte-level compatibility and boundary checks for the binary codecs.
#[cfg(test)]
mod tests {
    use super::*;

    /// Every byte round-trips and noncanonical representations are rejected.
    #[test]
    fn canonical_hex() {
        let bytes = std::array::from_fn::<_, 256, _>(|i| i as u8);
        let hex = encode_hex(&bytes);
        assert_eq!(hex.len(), 512);
        assert!(
            hex.bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        );
        assert_eq!(decode_hex::<256>(hex.as_bytes()), Ok(bytes));
        assert_eq!(decode_hex::<0>(b""), Ok([]));
        assert_eq!(encode_hex(&[]), "");
        for value in [
            b"0".as_slice(),
            b"000",
            b"AA",
            b"aF",
            b"gg",
            b" 0",
            b"0\n",
            &[0xff, 0xff],
        ] {
            assert_eq!(decode_hex::<1>(value), Err(Error::InvalidHex));
        }
    }

    /// Both byte orders match fixed vectors and reject every truncated record.
    #[test]
    fn endian_vectors_and_every_truncation() {
        for (endian, expected) in [
            (
                Endian::Little,
                vec![4, 3, 2, 1, 8, 7, 6, 5, 4, 3, 2, 1, 2, 0, 0, 0, b'a', b'b'],
            ),
            (
                Endian::Big,
                vec![1, 2, 3, 4, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 2, b'a', b'b'],
            ),
        ] {
            let mut bytes = Vec::new();
            let mut writer = Writer::new(&mut bytes, 18, endian);
            writer.u32(0x01020304).unwrap();
            writer.u64(0x0102030405060708).unwrap();
            writer.length_prefixed(b"ab", 2).unwrap();
            assert_eq!(bytes, expected);
            let parse = |bytes: &[u8]| -> Result<()> {
                let mut reader = Reader::new(bytes, endian);
                assert_eq!(reader.u32()?, 0x01020304);
                assert_eq!(reader.u64()?, 0x0102030405060708);
                assert_eq!(reader.length_prefixed(2)?, b"ab");
                assert!(reader.remaining().is_empty());
                Ok(())
            };
            parse(&bytes).unwrap();
            for length in 0..bytes.len() {
                assert!(parse(&bytes[..length]).is_err(), "{length}");
            }
        }
    }

    /// Empty fields, impossible sizes, and full buffers retain their contracts.
    #[test]
    fn limits_empty_fields_and_overflow() {
        let mut bytes = Vec::new();
        let mut writer = Writer::new(&mut bytes, 4, Endian::Little);
        assert_eq!(writer.length_prefixed(b"x", 0), Err(Error::Limit));
        assert_eq!(writer.length_prefixed(b"x", 1), Err(Error::Limit));
        writer.length_prefixed(b"", 0).unwrap();
        assert_eq!(writer.bytes(b"x"), Err(Error::Limit));
        assert_eq!(bytes, [0; 4]);
        assert_eq!(
            Reader::new(&bytes, Endian::Little).length_prefixed(0),
            Ok(b"".as_slice())
        );
        let mut reader = Reader::new(&[1, 2], Endian::Big);
        assert_eq!(reader.take(usize::MAX), Err(Error::Truncated));
        assert_eq!(reader.remaining(), &[1, 2]);
        assert_eq!(
            Reader::new(&[2, 0, 0, 0, 0], Endian::Little).count(2, 1),
            Err(Error::Limit)
        );
        assert_eq!(
            Reader::new(&[2, 0, 0, 0], Endian::Little).count(1, 0),
            Err(Error::Limit)
        );
        assert_eq!(
            Reader::new(&[2, 0, 0, 0], Endian::Little).count(2, 0),
            Ok(2)
        );
        let mut oversized = vec![0; 5];
        assert_eq!(
            Writer::new(&mut oversized, 4, Endian::Big).bytes(&[]),
            Err(Error::Limit)
        );
        if usize::BITS > 32 {
            assert_eq!(
                Writer::new(&mut bytes, usize::MAX, Endian::Little).count(usize::MAX),
                Err(Error::Limit)
            );
        }
    }

    /// Every output and field bound either appends the whole field or nothing.
    #[test]
    fn length_prefixed_append_is_atomic_at_every_bound() {
        for endian in [Endian::Little, Endian::Big] {
            for length in 0..=8 {
                let value = vec![0x7f; length];
                for existing in 0..=4 {
                    for capacity in 0..=20 {
                        for maximum in 0..=8 {
                            let mut bytes = vec![0xa5; existing];
                            let result = Writer::new(&mut bytes, capacity, endian)
                                .length_prefixed(&value, maximum);
                            if length <= maximum && existing + 4 + length <= capacity {
                                assert_eq!(result, Ok(()));
                                assert_eq!(&bytes[..existing], vec![0xa5; existing]);
                                let mut reader = Reader::new(&bytes[existing..], endian);
                                assert_eq!(reader.length_prefixed(maximum), Ok(value.as_slice()));
                                assert!(reader.remaining().is_empty());
                            } else {
                                assert_eq!(result, Err(Error::Limit));
                                assert_eq!(bytes, vec![0xa5; existing]);
                            }
                        }
                    }
                }
            }
        }
    }

    /// Short prefixes leave the cursor intact; rejected complete counts consume it.
    #[test]
    fn count_failures_preserve_cursor_contract() {
        for endian in [Endian::Little, Endian::Big] {
            for count in 0..=8 {
                let mut bytes = Vec::new();
                Writer::new(&mut bytes, 4, endian).count(count).unwrap();
                for prefix in 0..4 {
                    let mut reader = Reader::new(&bytes[..prefix], endian);
                    assert_eq!(reader.count(8, 0), Err(Error::Truncated));
                    assert_eq!(reader.remaining(), &bytes[..prefix]);
                }
                bytes.extend_from_slice(&[0xa5; 8]);
                for available in 0..=8 {
                    for maximum in 0..=8 {
                        for minimum in [0, 1, 2, 3, usize::MAX] {
                            let input = &bytes[..4 + available];
                            let mut reader = Reader::new(input, endian);
                            let expected = if count <= maximum
                                && (minimum == 0 || count <= available / minimum)
                            {
                                Ok(count)
                            } else {
                                Err(Error::Limit)
                            };
                            assert_eq!(reader.count(maximum, minimum), expected);
                            assert_eq!(reader.remaining(), &input[4..]);
                        }
                        let input = &bytes[..4 + available];
                        let mut reader = Reader::new(input, endian);
                        if count <= maximum && count <= available {
                            assert_eq!(reader.length_prefixed(maximum), Ok(&input[4..4 + count]));
                            assert_eq!(reader.remaining(), &input[4 + count..]);
                        } else {
                            assert_eq!(reader.length_prefixed(maximum), Err(Error::Limit));
                            assert_eq!(reader.remaining(), &input[4..]);
                        }
                    }
                }
            }
        }
    }
}
