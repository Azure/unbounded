//! Randomness passed in by callers.
//!
//! Production uses [`OsEntropy`] (`getrandom`). The simulator and tests pass
//! [`Seeded`], a deterministic SplitMix64 stream, so key ids, key material,
//! jitter, and claim delays replay exactly. `Seeded` is not for production
//! key material.

/// Entropy failed. Production callers fail closed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct EntropyError;

/// A source of random bytes.
pub trait Entropy {
    fn fill(&mut self, out: &mut [u8]) -> Result<(), EntropyError>;

    /// Random `u32`; 0 when entropy fails, matching the previous
    /// `let _ = getrandom(..)` jitter call sites.
    fn u32_or_zero(&mut self) -> u32 {
        let mut b = [0u8; 4];
        let _ = self.fill(&mut b);
        u32::from_ne_bytes(b)
    }

    /// Random `u16`; 0 when entropy fails.
    fn u16_or_zero(&mut self) -> u16 {
        let mut b = [0u8; 2];
        let _ = self.fill(&mut b);
        u16::from_ne_bytes(b)
    }
}

/// Operating system entropy.
#[derive(Clone, Copy, Debug, Default)]
pub struct OsEntropy;

impl Entropy for OsEntropy {
    fn fill(&mut self, out: &mut [u8]) -> Result<(), EntropyError> {
        getrandom::getrandom(out).map_err(|_| EntropyError)
    }
}

/// Deterministic SplitMix64 stream for simulation and tests.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Seeded {
    state: u64,
}

impl Seeded {
    pub const fn new(seed: u64) -> Self {
        Self { state: seed }
    }

    pub fn next_u64(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.state;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }
}

impl Entropy for Seeded {
    fn fill(&mut self, out: &mut [u8]) -> Result<(), EntropyError> {
        for chunk in out.chunks_mut(8) {
            let bytes = self.next_u64().to_le_bytes();
            chunk.copy_from_slice(&bytes[..chunk.len()]);
        }
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
        let (mut a, mut b) = (Seeded::new(9), Seeded::new(9));
        let (mut x, mut y) = ([0u8; 13], [0u8; 13]);
        a.fill(&mut x).unwrap();
        b.fill(&mut y).unwrap();
        assert_eq!(x, y);
        assert_ne!(x, [0u8; 13]);
        assert_ne!(Seeded::new(1).u32_or_zero(), Seeded::new(2).u32_or_zero());
    }

    #[test]
    fn splitmix_reference() {
        // Same reference vector as racer_core::time::SplitMix64.
        assert_eq!(Seeded::new(0).next_u64(), 0xE220_A839_7B1D_CDAF);
    }

    #[test]
    fn failing_yields_zero_jitter() {
        assert!(Failing.fill(&mut [0u8; 1]).is_err());
        assert_eq!(Failing.u32_or_zero(), 0);
        assert_eq!(Failing.u16_or_zero(), 0);
        let mut os = OsEntropy;
        os.fill(&mut [0u8; 8]).unwrap();
    }
}
