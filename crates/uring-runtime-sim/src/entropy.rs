//! A seeded, replayable entropy stream.
//!
//! [`SimEntropy`] produces SHA-256 blocks of
//! `domain || seed_le64 || stream_le64 || counter_le64`, for counter values
//! `0, 1, 2, ...`, concatenated. The bytes depend only on `(domain, seed,
//! stream)`: not on how reads are chunked, and not on which clone reads them.
//! This is byte-for-byte the stream the Racer runtime's simulation produced,
//! so recorded replay seeds keep working.
//!
//! ```
//! use uring_runtime::Entropy;
//! use uring_runtime_sim::{SimEntropy, DEFAULT_ENTROPY_DOMAIN};
//!
//! let a = SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 11);
//! let b = SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 11);
//! let (mut x, mut y) = ([0u8; 40], [0u8; 40]);
//! a.fill(&mut x).unwrap();
//! for chunk in y.chunks_mut(3) {
//!     b.fill(chunk).unwrap();
//! }
//! assert_eq!(x, y);
//! ```

use std::cell::RefCell;
use std::fmt;
use std::rc::Rc;

use sha2::{Digest, Sha256};
use uring_runtime::{Entropy, Error, ErrorKind, Result};

/// The domain used by the Racer runtime's simulation, kept for replay
/// compatibility. Applications with persisted replay corpora should choose
/// their own explicit, versioned domain.
pub const DEFAULT_ENTROPY_DOMAIN: &[u8] = b"uring-runtime.entropy.v1\0";

const BLOCK: usize = 32;

/// A deterministic entropy stream.
///
/// Clones share one cursor: bytes read through a clone are not returned
/// again by the original. Create one stream per simulated worker or role
/// with a distinct `stream` number. `SimEntropy` is single-threaded
/// (`!Send`); create a separate instance on each thread.
#[derive(Clone)]
pub struct SimEntropy(Rc<RefCell<Cursor>>);

struct Cursor {
    domain: Rc<[u8]>,
    seed: u64,
    stream: u64,
    counter: u64,
    block: [u8; BLOCK],
    offset: usize,
}

impl SimEntropy {
    /// Create the stream identified by `(domain, seed, stream)`, positioned at
    /// its first byte.
    pub fn new(domain: impl AsRef<[u8]>, seed: u64, stream: u64) -> Self {
        Self(Rc::new(RefCell::new(Cursor {
            domain: domain.as_ref().into(),
            seed,
            stream,
            counter: 0,
            block: [0; BLOCK],
            offset: BLOCK,
        })))
    }

    /// The seed this stream was created with.
    pub fn seed(&self) -> u64 {
        self.0.borrow().seed
    }

    /// The stream number this stream was created with.
    pub fn stream(&self) -> u64 {
        self.0.borrow().stream
    }

    #[cfg(test)]
    fn set_cursor(&self, counter: u64, offset: usize, block: [u8; BLOCK]) {
        let mut cursor = self.0.borrow_mut();
        cursor.counter = counter;
        cursor.offset = offset;
        cursor.block = block;
    }

    #[cfg(test)]
    fn cursor(&self) -> (u64, usize) {
        let cursor = self.0.borrow();
        (cursor.counter, cursor.offset)
    }
}

impl Cursor {
    fn fill(&mut self, mut buf: &mut [u8]) -> Result<()> {
        while !buf.is_empty() {
            if self.offset == BLOCK {
                let next = self.counter.checked_add(1).ok_or_else(|| {
                    Error::new(ErrorKind::Unavailable).with_context("SimEntropy exhausted")
                })?;
                let mut hash = Sha256::new();
                hash.update(&self.domain);
                hash.update(self.seed.to_le_bytes());
                hash.update(self.stream.to_le_bytes());
                hash.update(self.counter.to_le_bytes());
                self.block = hash.finalize().into();
                self.counter = next;
                self.offset = 0;
            }
            let count = buf.len().min(BLOCK - self.offset);
            let (chunk, rest) = buf.split_at_mut(count);
            chunk.copy_from_slice(&self.block[self.offset..self.offset + count]);
            self.offset += count;
            buf = rest;
        }
        Ok(())
    }
}

impl Entropy for SimEntropy {
    /// Fill `buf` with the next bytes of the stream.
    ///
    /// Fails with [`ErrorKind::Unavailable`] only after 2^64 blocks; bytes
    /// copied before the failure stay in `buf`.
    fn fill(&self, buf: &mut [u8]) -> Result<()> {
        self.0.borrow_mut().fill(buf)
    }
}

impl fmt::Debug for SimEntropy {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let cursor = self.0.borrow();
        f.debug_struct("SimEntropy")
            .field("seed", &cursor.seed)
            .field("stream", &cursor.stream)
            .field("counter", &cursor.counter)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex(bytes: &[u8]) -> String {
        bytes.iter().map(|b| format!("{b:02x}")).collect()
    }

    fn read(entropy: &SimEntropy, n: usize) -> Vec<u8> {
        let mut buf = vec![0; n];
        entropy.fill(&mut buf).unwrap();
        buf
    }

    /// Vectors computed independently from the Racer runtime's algorithm
    /// (SHA-256 over domain || seed || stream || counter, little endian).
    #[test]
    fn known_vectors_match_old_runtime() {
        assert_eq!(
            hex(&read(&SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 11), 40)),
            "5eb0f6598c0f2d9c3ea6f15f9019d7a1defa575d5d1fe54b8cb95f87e7bc5b82627c66322c311609"
        );
        assert_eq!(
            hex(&read(&SimEntropy::new(b"test.entropy.v1\0", 0, 0), 16)),
            "3d6dceba6f8199a9dcdc96164fd17005"
        );
        assert_eq!(
            hex(&read(
                &SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 0xdead_beef, 3),
                8
            )),
            "d0e6b0f665fbfcda"
        );
    }

    #[test]
    fn chunking_and_clones_share_one_stream() {
        let whole = read(&SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 11), 99);
        let entropy = SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 11);
        let clone = entropy.clone();
        let mut chunked = vec![0; 99];
        for (index, chunk) in chunked.chunks_mut(7).enumerate() {
            let source = if index % 2 == 0 { &entropy } else { &clone };
            source.fill(chunk).unwrap();
        }
        assert_eq!(whole, chunked);
        assert_ne!(
            whole,
            read(&SimEntropy::new(b"other\0", 7, 11), 99),
            "domains separate streams"
        );
        assert_ne!(
            whole,
            read(&SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 12), 99)
        );
        assert_eq!((entropy.seed(), entropy.stream()), (7, 11));
        assert!(format!("{entropy:?}").contains("stream: 11"));
    }

    #[test]
    fn exhaustion_fails_without_corrupting_the_cursor() {
        let entropy = SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 1, 0);
        entropy.set_cursor(u64::MAX, 30, [7; BLOCK]);
        entropy.fill(&mut []).unwrap();
        assert_eq!(entropy.cursor(), (u64::MAX, 30));
        let mut bytes = [9; 4];
        let error = entropy.fill(&mut bytes).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Unavailable);
        assert_eq!(bytes, [7, 7, 9, 9]);
        assert_eq!(entropy.cursor(), (u64::MAX, BLOCK));
        assert!(entropy.fill(&mut [0]).is_err());
    }
}
