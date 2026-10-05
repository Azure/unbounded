//! Byte mechanics only: callers choose byte order, bounds, and error mapping.

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Truncated,
    Limit,
    InvalidHex,
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Copy, Debug)]
pub enum Endian {
    Little,
    Big,
}

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

pub fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut value = String::new();
    for &byte in bytes {
        value.push(char::from(HEX[usize::from(byte >> 4)]));
        value.push(char::from(HEX[usize::from(byte & 15)]));
    }
    value
}

pub struct Reader<'a> {
    remaining: &'a [u8],
    endian: Endian,
}

impl<'a> Reader<'a> {
    /// The input slice is the total read bound; field bounds are supplied below.
    pub fn new(bytes: &'a [u8], endian: Endian) -> Self {
        Self {
            remaining: bytes,
            endian,
        }
    }

    pub fn remaining(&self) -> &'a [u8] {
        self.remaining
    }

    pub fn take(&mut self, length: usize) -> Result<&'a [u8]> {
        let (value, rest) = self
            .remaining
            .split_at_checked(length)
            .ok_or(Error::Truncated)?;
        self.remaining = rest;
        Ok(value)
    }

    pub fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::Truncated)
    }

    pub fn u32(&mut self) -> Result<u32> {
        Ok(match self.endian {
            Endian::Little => u32::from_le_bytes(self.array()?),
            Endian::Big => u32::from_be_bytes(self.array()?),
        })
    }

    pub fn u64(&mut self) -> Result<u64> {
        Ok(match self.endian {
            Endian::Little => u64::from_le_bytes(self.array()?),
            Endian::Big => u64::from_be_bytes(self.array()?),
        })
    }

    /// Read a u32 count bounded by both caller policy and remaining storage.
    /// A zero minimum permits zero-size items while still enforcing `maximum`.
    pub fn count(&mut self, maximum: usize, minimum_bytes: usize) -> Result<usize> {
        let count = usize::try_from(self.u32()?).map_err(|_| Error::Limit)?;
        if count > maximum || (minimum_bytes != 0 && count > self.remaining.len() / minimum_bytes) {
            return Err(Error::Limit);
        }
        Ok(count)
    }

    pub fn length_prefixed(&mut self, maximum: usize) -> Result<&'a [u8]> {
        let length = self.count(maximum, 1)?;
        self.take(length)
    }
}

/// Appends to a caller-owned buffer, including its existing bytes in the bound.
pub struct Writer<'a> {
    bytes: &'a mut Vec<u8>,
    maximum: usize,
    endian: Endian,
}

impl<'a> Writer<'a> {
    pub fn new(bytes: &'a mut Vec<u8>, maximum: usize, endian: Endian) -> Self {
        Self {
            bytes,
            maximum,
            endian,
        }
    }

    pub fn bytes(&mut self, value: &[u8]) -> Result<()> {
        if self
            .bytes
            .len()
            .checked_add(value.len())
            .is_none_or(|n| n > self.maximum)
        {
            return Err(Error::Limit);
        }
        self.bytes.extend_from_slice(value);
        Ok(())
    }

    pub fn u32(&mut self, value: u32) -> Result<()> {
        self.bytes(&match self.endian {
            Endian::Little => value.to_le_bytes(),
            Endian::Big => value.to_be_bytes(),
        })
    }

    pub fn u64(&mut self, value: u64) -> Result<()> {
        self.bytes(&match self.endian {
            Endian::Little => value.to_le_bytes(),
            Endian::Big => value.to_be_bytes(),
        })
    }

    pub fn count(&mut self, value: usize) -> Result<()> {
        self.u32(u32::try_from(value).map_err(|_| Error::Limit)?)
    }

    /// A failed field append leaves the buffer unchanged.
    pub fn length_prefixed(&mut self, value: &[u8], maximum: usize) -> Result<()> {
        if value.len() > maximum {
            return Err(Error::Limit);
        }
        let length = u32::try_from(value.len()).map_err(|_| Error::Limit)?;
        if self
            .bytes
            .len()
            .checked_add(4)
            .and_then(|n| n.checked_add(value.len()))
            .is_none_or(|n| n > self.maximum)
        {
            return Err(Error::Limit);
        }
        self.u32(length)?;
        self.bytes(value)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
}
