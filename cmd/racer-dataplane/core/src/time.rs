//! Deterministic randomness passed in by the caller.

/// Small deterministic RNG (SplitMix64). Not for cryptographic use.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SplitMix64 {
    state: u64,
}

impl SplitMix64 {
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

    /// Derive an independent stream, for example one per role or node.
    pub fn fork(&mut self, stream: u64) -> Self {
        Self::new(self.next_u64() ^ stream.wrapping_mul(0xD6E8_FEB8_6659_FD93))
    }

    /// Uniform value in `0..bound`. Returns 0 when `bound` is 0.
    pub fn below(&mut self, bound: u64) -> u64 {
        if bound == 0 {
            return 0;
        }
        ((u128::from(self.next_u64()) * u128::from(bound)) >> 64) as u64
    }

    pub fn fill(&mut self, out: &mut [u8]) {
        for chunk in out.chunks_mut(8) {
            let bytes = self.next_u64().to_le_bytes();
            chunk.copy_from_slice(&bytes[..chunk.len()]);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splitmix_is_reference_seeded_forkable_and_bounded() {
        // Reference output for seed 0 from the published SplitMix64.
        let mut rng = SplitMix64::new(0);
        assert_eq!(rng.next_u64(), 0xE220_A839_7B1D_CDAF);
        assert_eq!(rng.next_u64(), 0x6E78_9E6A_A1B9_65F4);

        let (mut a, mut b) = (SplitMix64::new(42), SplitMix64::new(42));
        for _ in 0..100 {
            assert_eq!(a.next_u64(), b.next_u64());
        }
        assert_ne!(SplitMix64::new(1).next_u64(), SplitMix64::new(2).next_u64());

        let mut root = SplitMix64::new(7);
        let mut a = root.clone().fork(1);
        let mut b = root.fork(2);
        assert_ne!(a.next_u64(), b.next_u64());

        let mut rng = SplitMix64::new(3);
        assert_eq!(rng.below(0), 0);
        assert_eq!(rng.below(1), 0);
        for _ in 0..1000 {
            assert!(rng.below(10) < 10);
        }

        // `fill` handles partial chunks.
        let mut buf = [0u8; 11];
        SplitMix64::new(9).fill(&mut buf);
        let mut b = SplitMix64::new(9);
        assert_eq!(buf[..8], b.next_u64().to_le_bytes());
        assert_eq!(buf[8..], b.next_u64().to_le_bytes()[..3]);
    }
}
