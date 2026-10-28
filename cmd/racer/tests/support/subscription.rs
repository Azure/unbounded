//! Decode subscription framing independently of production encoding.
//!
//! Callers retain payload handling, page-coordinate assertions, and EOF policy.

use std::io::{self, Read, Write};

/// Fixed-width wire fields for a page or completion frame.
#[derive(Debug, PartialEq, Eq)]
pub(crate) struct Frame {
    pub(crate) kind: u8,

    pub(crate) number: u64,

    pub(crate) offset: u64,

    pub(crate) length: u32,
}

impl Frame {
    /// Read exactly one frame, preserving transport and truncation errors.
    pub(crate) fn read(reader: &mut impl Read) -> io::Result<Self> {
        let mut bytes = [0; 21];
        reader.read_exact(&mut bytes)?;
        Ok(Self {
            kind: bytes[0],
            number: u64::from_be_bytes(bytes[1..9].try_into().unwrap()),
            offset: u64::from_be_bytes(bytes[9..17].try_into().unwrap()),
            length: u32::from_be_bytes(bytes[17..].try_into().unwrap()),
        })
    }

    /// Describe completion with its page count and total payload byte count.
    pub(crate) fn completion(pages: u64, bytes: u64) -> Self {
        Self {
            kind: 2,
            number: pages,
            offset: bytes,
            length: 0,
        }
    }

    /// Return exact credit after a validated page's payload has been consumed.
    ///
    /// Completion retires the final lease, so only nonfinal pages send releases.
    pub(crate) fn release_before(&self, writer: &mut impl Write, end: u64) -> io::Result<()> {
        if self.offset + u64::from(self.length) < end {
            writer.write_all(&self.number.to_be_bytes())?;
            writer.write_all(&self.length.to_be_bytes())?;
        }
        Ok(())
    }
}
