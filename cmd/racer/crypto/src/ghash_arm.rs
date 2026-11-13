//! Aggregated carry-less-multiply GHASH for aarch64 (PMULL).
//!
//! Mirrors `ghash_x86`: GHASH is computed in the POLYVAL domain (RFC 8452,
//! appendix A), four blocks are multiplied by H^4..H^1 and accumulated
//! unreduced, then reduced once per group.
//!
//! Every `unsafe` block below is either a call to a `#[target_feature]`
//! function, which is sound because [`Ghash::new`] only returns a value after
//! runtime detection confirmed the `aes` (PMULL) and `neon` features, or a
//! NEON load on a pointer derived from a slice that the surrounding code has
//! bounds-checked to cover 16 bytes.

#![allow(unsafe_code)]

use core::arch::aarch64::*;

use zeroize::Zeroize;

/// Number of precomputed powers of H (and the aggregation width).
const POWERS: usize = 4;

/// Streaming GHASH with the same `update_padded` semantics as
/// `ghash::GHash`: each call zero-pads its own trailing partial block.
pub(crate) struct Ghash {
    /// `pow[j]` = H^(4 - j) in the POLYVAL domain.
    pow: [u128; POWERS],
    acc: u128,
}

impl Drop for Ghash {
    fn drop(&mut self) {
        self.pow.zeroize();
        self.acc.zeroize();
    }
}

impl Ghash {
    /// Returns `None` when the CPU lacks PMULL; callers then fall back to the
    /// portable `ghash` crate.
    pub(crate) fn new(h: &[u8; 16]) -> Option<Self> {
        if !detect() {
            return None;
        }
        // mulX_POLYVAL(rev(H)).
        let mut v = u128::from_be_bytes(*h);
        let top = v >> 127;
        v <<= 1;
        v ^= top ^ (top << 127) ^ (top << 126) ^ (top << 121);
        let mut pow = [0u128; POWERS];
        pow[POWERS - 1] = v;
        for j in (0..POWERS - 1).rev() {
            // SAFETY: detect() confirmed aes (pmull) and neon.
            pow[j] = unsafe { mul_u128(pow[j + 1], v) };
        }
        v.zeroize();
        Some(Self { pow, acc: 0 })
    }

    pub(crate) fn update_padded(&mut self, data: &[u8]) {
        let whole = data.len() & !15;
        let (blocks, tail) = data.split_at(whole);
        // SAFETY: features confirmed by detect() in `new`.
        unsafe { self.blocks(blocks) };
        if !tail.is_empty() {
            let mut last = [0u8; 16];
            last[..tail.len()].copy_from_slice(tail);
            // SAFETY: as above.
            unsafe { self.blocks(&last) };
            last.zeroize();
        }
    }

    pub(crate) fn finalize(self) -> [u8; 16] {
        self.acc.to_be_bytes()
    }

    /// # Safety
    /// Requires aes (pmull) and neon. `data.len()` must be a multiple of 16.
    #[target_feature(enable = "neon,aes")]
    unsafe fn blocks(&mut self, data: &[u8]) {
        let mut acc = self.acc;
        let mut chunks = data.chunks_exact(16 * POWERS);
        for c in &mut chunks {
            let (mut lo, mut mid, mut hi) = (0, 0, 0);
            for k in 0..POWERS {
                let mut x = load_rev(&c[16 * k..16 * k + 16]);
                if k == 0 {
                    x ^= acc;
                }
                accumulate(x, self.pow[k], &mut lo, &mut mid, &mut hi);
            }
            acc = reduce(lo, mid, hi);
        }
        let h1 = self.pow[POWERS - 1];
        for b in chunks.remainder().chunks_exact(16) {
            let (mut lo, mut mid, mut hi) = (0, 0, 0);
            accumulate(load_rev(b) ^ acc, h1, &mut lo, &mut mid, &mut hi);
            acc = reduce(lo, mid, hi);
        }
        self.acc = acc;
    }
}

fn detect() -> bool {
    std::arch::is_aarch64_feature_detected!("neon")
        && std::arch::is_aarch64_feature_detected!("aes")
}

/// Load 16 bytes and byte-reverse them into a little-endian u128, the
/// equivalent of `u128::from_be_bytes`.
#[target_feature(enable = "neon")]
fn load_rev(b: &[u8]) -> u128 {
    assert!(b.len() >= 16);
    // SAFETY: b has at least 16 readable bytes; vld1q_u8 is unaligned.
    let v = unsafe { vld1q_u8(b.as_ptr()) };
    let r = vrev64q_u8(v);
    let r = vextq_u8::<8>(r, r);
    let r = vreinterpretq_u64_u8(r);
    u128::from(vgetq_lane_u64::<0>(r)) | (u128::from(vgetq_lane_u64::<1>(r)) << 64)
}

#[inline(always)]
fn halves(v: u128) -> (u64, u64) {
    (v as u64, (v >> 64) as u64)
}

#[target_feature(enable = "neon,aes")]
fn clmul(a: u64, b: u64) -> u128 {
    vmull_p64(a, b)
}

/// Accumulate the unreduced 256-bit product `x * h` as low, middle (cross
/// terms) and high 128-bit parts.
#[target_feature(enable = "neon,aes")]
fn accumulate(x: u128, h: u128, lo: &mut u128, mid: &mut u128, hi: &mut u128) {
    let (xl, xh) = halves(x);
    let (hl, hh) = halves(h);
    *lo ^= clmul(xl, hl);
    *hi ^= clmul(xh, hh);
    *mid ^= clmul(xh, hl) ^ clmul(xl, hh);
}

/// POLYVAL reduction: `(hi:mid:lo) * x^-128 mod x^128 + x^127 + x^126 + x^121 + 1`.
#[target_feature(enable = "neon,aes")]
fn reduce(lo: u128, mid: u128, hi: u128) -> u128 {
    const POLY_HI: u64 = 0xc200_0000_0000_0000;
    let lo = lo ^ (mid << 64);
    let hi = hi ^ (mid >> 64);
    let t = clmul(lo as u64, POLY_HI);
    let lo = lo.rotate_left(64) ^ t;
    let t = clmul(lo as u64, POLY_HI);
    let lo = lo.rotate_left(64) ^ t;
    hi ^ lo
}

/// # Safety
/// Requires aes (pmull) and neon.
#[target_feature(enable = "neon,aes")]
unsafe fn mul_u128(a: u128, b: u128) -> u128 {
    let (mut lo, mut mid, mut hi) = (0, 0, 0);
    accumulate(a, b, &mut lo, &mut mid, &mut hi);
    reduce(lo, mid, hi)
}

#[cfg(test)]
mod tests {
    use ghash::universal_hash::UniversalHash;

    use super::*;

    fn rng(seed: &mut u64) -> u64 {
        *seed ^= *seed << 13;
        *seed ^= *seed >> 7;
        *seed ^= *seed << 17;
        *seed
    }

    fn check(h: &[u8; 16], calls: &[&[u8]]) {
        let mut reference = ghash::GHash::new(h.into());
        for c in calls {
            reference.update_padded(c);
        }
        let want: [u8; 16] = reference.finalize().into();
        let Some(mut fast) = Ghash::new(h) else {
            return;
        };
        for c in calls {
            fast.update_padded(c);
        }
        assert_eq!(fast.finalize(), want);
    }

    fn hex(s: &str) -> Vec<u8> {
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
            .collect()
    }

    /// GCM spec (McGrew-Viega) test case 2: GHASH(H, {}, C) with lengths block.
    #[test]
    fn known_answer_gcm_test_case_2() {
        let Some(mut g) = Ghash::new(&hex("66e94bd4ef8a2c3b884cfa59ca342b2e").try_into().unwrap())
        else {
            return;
        };
        g.update_padded(&hex("0388dace60b6a392f328c2b971b2fe78"));
        g.update_padded(&hex("00000000000000000000000000000080"));
        assert_eq!(
            g.finalize().to_vec(),
            hex("f38cbb1ad69223dcc3457ae5b6b0f885")
        );
    }

    #[test]
    fn matches_ghash_crate_on_edge_cases() {
        let mut seed = 0x1234_5678_9abc_def1;
        let data: Vec<u8> = (0..8192).map(|_| rng(&mut seed) as u8).collect();
        for _ in 0..32 {
            let mut h = [0u8; 16];
            h.iter_mut().for_each(|b| *b = rng(&mut seed) as u8);
            check(&h, &[]);
            check(&h, &[&[]]);
            for n in [1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 4096] {
                check(&h, &[&data[..n]]);
            }
            let n = (rng(&mut seed) % 8192) as usize;
            let split = (rng(&mut seed) as usize) % (n + 1);
            check(&h, &[&data[..split], &data[split..n]]);
        }
        for h in [[0u8; 16], [0xffu8; 16]] {
            check(&h, &[&data[..4096]]);
        }
    }

    #[test]
    fn matches_ghash_crate_on_1_mib_plus_16() {
        let n = (1 << 20) + 16;
        let data: Vec<u8> = (0..n)
            .map(|i: usize| i.wrapping_mul(131).wrapping_add(7) as u8)
            .collect();
        check(&[0x42; 16], &[&data[..8600], &data]);
    }
}
