//! Randomness passed in by callers.
//!
//! Production uses [`OsEntropy`] (`getrandom`). The simulator and tests pass
//! [`SplitMix64`], so key ids, key material, jitter, and claim delays
//! replay exactly. `SplitMix64` is not for production key material.

use racer_core::time::SplitMix64;

/// Entropy failed. Production callers fail closed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct EntropyError;

/// A source of random bytes.
pub trait Entropy {
    fn fill(&mut self, out: &mut [u8]) -> Result<(), EntropyError>;
}

/// Operating system entropy.
#[derive(Clone, Copy, Debug, Default)]
pub struct OsEntropy;

impl Entropy for OsEntropy {
    fn fill(&mut self, out: &mut [u8]) -> Result<(), EntropyError> {
        getrandom::getrandom(out).map_err(|_| EntropyError)
    }
}

impl Entropy for SplitMix64 {
    fn fill(&mut self, out: &mut [u8]) -> Result<(), EntropyError> {
        SplitMix64::fill(self, out);
        Ok(())
    }
}

/// Always fails. Tests use it to cover fail-closed paths.
#[derive(Clone, Copy, Debug, Default)]
pub struct Failing;

impl Entropy for Failing {
    fn fill(&mut self, _: &mut [u8]) -> Result<(), EntropyError> {
        Err(EntropyError)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn seeded_replays() {
        let (mut a, mut b) = (SplitMix64::new(9), SplitMix64::new(9));
        let (mut x, mut y) = ([0u8; 13], [0u8; 13]);
        Entropy::fill(&mut a, &mut x).unwrap();
        Entropy::fill(&mut b, &mut y).unwrap();
        assert_eq!(x, y);
        assert_ne!(x, [0u8; 13]);
        assert!(Failing.fill(&mut [0u8; 1]).is_err());
        OsEntropy.fill(&mut [0u8; 8]).unwrap();
    }
}
