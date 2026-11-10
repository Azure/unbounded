//! Aggregated carry-less-multiply GHASH for x86_64.
//!
//! This is the only module in the crate that uses `unsafe`. It exists because
//! the `ghash`/`polyval` crates process at most four blocks per reduction and
//! re-check CPU features and byte-reverse through a generic copy on every
//! call, which caps tag-only page verification well below memory bandwidth.
//!
//! GHASH is computed in the POLYVAL domain (RFC 8452, appendix A):
//! `GHASH(H, X) = rev(POLYVAL(mulX(rev(H)), rev(X_1), ..., rev(X_n)))`.
//! POLYVAL multiplication `a * b * x^-128` is a field multiplication under an
//! isomorphism, so the sum `X_1 H^n + ... + X_n H^1` may be accumulated
//! unreduced and reduced once per group of blocks.
//!
//! Every `unsafe` block below is either a call to a `#[target_feature]`
//! function, which is sound because [`Ghash::new`] only returns a value after
//! runtime detection confirmed every feature those functions enable (the
//! detected [`Level`] is stored and selects the code path), or an unaligned
//! SIMD load/store on a pointer derived from a slice or array that the
//! surrounding code has bounds-checked to cover the full access width.

#![allow(unsafe_code)]

use core::arch::x86_64::*;

use zeroize::Zeroize;

/// Number of precomputed powers of H (and the widest aggregation).
const POWERS: usize = 16;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Level {
    /// PCLMULQDQ + SSSE3: 8 blocks per reduction.
    Pclmul,
    /// VPCLMULQDQ + AVX-512F/BW: 16 blocks (4 x 512-bit) per reduction.
    Vpclmul,
}

/// Streaming GHASH with the same `update_padded` semantics as
/// `ghash::GHash`: each call zero-pads its own trailing partial block.
pub(crate) struct Ghash {
    level: Level,
    /// `pow[j]` = H^(16 - j) in the POLYVAL domain, so a 512-bit load of
    /// `pow[4k..4k + 4]` lines up with blocks `4k..4k + 4` of a 16-block group.
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
    /// Returns `None` when the CPU lacks PCLMULQDQ/SSSE3; callers then fall
    /// back to the portable `ghash` crate.
    pub(crate) fn new(h: &[u8; 16]) -> Option<Self> {
        Self::with_level(h, detect()?)
    }

    fn with_level(h: &[u8; 16], level: Level) -> Option<Self> {
        // Never trust the caller-chosen level over the hardware.
        if level == Level::Vpclmul && detect()? == Level::Pclmul {
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
            // SAFETY: detect() confirmed pclmulqdq and ssse3.
            pow[j] = unsafe { mul_u128(pow[j + 1], v) };
        }
        v.zeroize();
        Some(Self { level, pow, acc: 0 })
    }

    pub(crate) fn update_padded(&mut self, data: &[u8]) {
        let whole = data.len() & !15;
        let (blocks, tail) = data.split_at(whole);
        // SAFETY: `level` was confirmed by detect() in `with_level`.
        unsafe {
            match self.level {
                Level::Vpclmul => self.blocks_vpclmul(blocks),
                Level::Pclmul => self.blocks_pclmul(blocks),
            }
        }
        if !tail.is_empty() {
            let mut last = [0u8; 16];
            last[..tail.len()].copy_from_slice(tail);
            // SAFETY: as above; pclmulqdq is implied by both levels.
            unsafe { self.blocks_pclmul(&last) };
            last.zeroize();
        }
    }

    pub(crate) fn finalize(self) -> [u8; 16] {
        self.acc.to_be_bytes()
    }

    /// # Safety
    /// Requires pclmulqdq and ssse3. `data.len()` must be a multiple of 16.
    #[target_feature(enable = "pclmulqdq,ssse3,sse2")]
    unsafe fn blocks_pclmul(&mut self, data: &[u8]) {
        let bswap = bswap128();
        let mut acc = ld(&self.acc);
        let mut chunks = data.chunks_exact(128);
        for c in &mut chunks {
            let (mut lo, mut mid, mut hi) = (zero(), zero(), zero());
            for k in 0..8 {
                // SAFETY: c has 128 bytes; offset 16k + 16 <= 128.
                let mut x = _mm_shuffle_epi8(
                    unsafe { _mm_loadu_si128(c.as_ptr().add(16 * k).cast()) },
                    bswap,
                );
                if k == 0 {
                    x = _mm_xor_si128(x, acc);
                }
                let h = ld(&self.pow[8 + k]);
                accumulate(x, h, &mut lo, &mut mid, &mut hi);
            }
            acc = reduce(lo, mid, hi);
        }
        let h1 = ld(&self.pow[POWERS - 1]);
        for b in chunks.remainder().chunks_exact(16) {
            // SAFETY: b has exactly 16 bytes.
            let x = _mm_shuffle_epi8(unsafe { _mm_loadu_si128(b.as_ptr().cast()) }, bswap);
            let (mut lo, mut mid, mut hi) = (zero(), zero(), zero());
            accumulate(_mm_xor_si128(x, acc), h1, &mut lo, &mut mid, &mut hi);
            acc = reduce(lo, mid, hi);
        }
        st(&mut self.acc, acc);
    }

    /// # Safety
    /// Requires vpclmulqdq, avx512f, avx512bw, pclmulqdq and ssse3.
    /// `data.len()` must be a multiple of 16.
    #[target_feature(enable = "vpclmulqdq,avx512f,avx512bw,pclmulqdq,ssse3,sse2")]
    unsafe fn blocks_vpclmul(&mut self, data: &[u8]) {
        let bswap = _mm512_broadcast_i32x4(bswap128());
        let p = self.pow.as_ptr();
        // SAFETY: pow has 16 u128 = 256 bytes; each load reads 64 bytes at
        // offsets 0, 64, 128, 192.
        let pw = unsafe {
            [
                _mm512_loadu_si512(p.cast()),
                _mm512_loadu_si512(p.add(4).cast()),
                _mm512_loadu_si512(p.add(8).cast()),
                _mm512_loadu_si512(p.add(12).cast()),
            ]
        };
        let mut acc = ld(&self.acc);
        let mut chunks = data.chunks_exact(256);
        for c in &mut chunks {
            let (mut lo, mut mid, mut hi) = (z512(), z512(), z512());
            for (k, h) in pw.iter().enumerate() {
                // SAFETY: c has 256 bytes; offset 64k + 64 <= 256.
                let mut x = _mm512_shuffle_epi8(
                    unsafe { _mm512_loadu_si512(c.as_ptr().add(64 * k).cast()) },
                    bswap,
                );
                if k == 0 {
                    x = _mm512_xor_si512(x, _mm512_zextsi128_si512(acc));
                }
                lo = _mm512_xor_si512(lo, _mm512_clmulepi64_epi128(x, *h, 0x00));
                hi = _mm512_xor_si512(hi, _mm512_clmulepi64_epi128(x, *h, 0x11));
                mid = _mm512_xor_si512(mid, _mm512_clmulepi64_epi128(x, *h, 0x01));
                mid = _mm512_xor_si512(mid, _mm512_clmulepi64_epi128(x, *h, 0x10));
            }
            acc = reduce(fold512(lo), fold512(mid), fold512(hi));
        }
        st(&mut self.acc, acc);
        // SAFETY: the caller's features include pclmulqdq and ssse3.
        unsafe { self.blocks_pclmul(chunks.remainder()) };
    }
}

fn detect() -> Option<Level> {
    if !(is_x86_feature_detected!("pclmulqdq") && is_x86_feature_detected!("ssse3")) {
        return None;
    }
    if is_x86_feature_detected!("vpclmulqdq")
        && is_x86_feature_detected!("avx512f")
        && is_x86_feature_detected!("avx512bw")
    {
        Some(Level::Vpclmul)
    } else {
        Some(Level::Pclmul)
    }
}

#[inline(always)]
fn ld(v: &u128) -> __m128i {
    // SAFETY: a u128 is 16 readable bytes; x86_64 is little-endian so the low
    // 64 bits land in the low lane. The load is unaligned.
    unsafe { _mm_loadu_si128((v as *const u128).cast()) }
}

#[inline(always)]
fn st(v: &mut u128, x: __m128i) {
    // SAFETY: a u128 is 16 writable bytes; the store is unaligned.
    unsafe { _mm_storeu_si128((v as *mut u128).cast(), x) }
}

#[inline(always)]
fn zero() -> __m128i {
    // SAFETY: sse2 is part of the x86_64 baseline.
    unsafe { _mm_setzero_si128() }
}

#[target_feature(enable = "avx512f")]
fn z512() -> __m512i {
    _mm512_setzero_si512()
}

#[target_feature(enable = "sse2")]
fn bswap128() -> __m128i {
    _mm_set_epi8(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15)
}

/// Accumulate the unreduced 256-bit product `x * h` as low, middle (cross
/// terms) and high 128-bit parts.
#[target_feature(enable = "pclmulqdq,sse2")]
fn accumulate(x: __m128i, h: __m128i, lo: &mut __m128i, mid: &mut __m128i, hi: &mut __m128i) {
    *lo = _mm_xor_si128(*lo, _mm_clmulepi64_si128(x, h, 0x00));
    *hi = _mm_xor_si128(*hi, _mm_clmulepi64_si128(x, h, 0x11));
    *mid = _mm_xor_si128(*mid, _mm_clmulepi64_si128(x, h, 0x01));
    *mid = _mm_xor_si128(*mid, _mm_clmulepi64_si128(x, h, 0x10));
}

#[target_feature(enable = "avx512f")]
fn fold512(v: __m512i) -> __m128i {
    let a = _mm_xor_si128(
        _mm512_extracti32x4_epi32::<0>(v),
        _mm512_extracti32x4_epi32::<1>(v),
    );
    let b = _mm_xor_si128(
        _mm512_extracti32x4_epi32::<2>(v),
        _mm512_extracti32x4_epi32::<3>(v),
    );
    _mm_xor_si128(a, b)
}

/// POLYVAL reduction: `(hi:mid:lo) * x^-128 mod x^128 + x^127 + x^126 + x^121 + 1`.
#[target_feature(enable = "pclmulqdq,sse2")]
fn reduce(lo: __m128i, mid: __m128i, hi: __m128i) -> __m128i {
    let lo = _mm_xor_si128(lo, _mm_slli_si128(mid, 8));
    let hi = _mm_xor_si128(hi, _mm_srli_si128(mid, 8));
    let poly = _mm_set_epi64x(0xc200_0000_0000_0000_u64 as i64, 1);
    let t = _mm_clmulepi64_si128(lo, poly, 0x10);
    let lo = _mm_xor_si128(_mm_shuffle_epi32(lo, 0x4e), t);
    let t = _mm_clmulepi64_si128(lo, poly, 0x10);
    let lo = _mm_xor_si128(_mm_shuffle_epi32(lo, 0x4e), t);
    _mm_xor_si128(hi, lo)
}

/// # Safety
/// Requires pclmulqdq.
#[target_feature(enable = "pclmulqdq,sse2")]
unsafe fn mul_u128(a: u128, b: u128) -> u128 {
    let (mut lo, mut mid, mut hi) = (zero(), zero(), zero());
    accumulate(ld(&a), ld(&b), &mut lo, &mut mid, &mut hi);
    let mut out = 0;
    st(&mut out, reduce(lo, mid, hi));
    out
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

    fn levels() -> Vec<Level> {
        match detect() {
            None => vec![],
            Some(Level::Pclmul) => vec![Level::Pclmul],
            Some(Level::Vpclmul) => vec![Level::Pclmul, Level::Vpclmul],
        }
    }

    fn check(h: &[u8; 16], calls: &[&[u8]]) {
        let mut reference = ghash::GHash::new(h.into());
        for c in calls {
            reference.update_padded(c);
        }
        let want: [u8; 16] = reference.finalize().into();
        for level in levels() {
            let mut fast = Ghash::with_level(h, level).unwrap();
            for c in calls {
                fast.update_padded(c);
            }
            assert_eq!(fast.finalize(), want, "{level:?}");
        }
    }

    #[test]
    fn matches_ghash_crate_on_random_lengths_and_alignments() {
        let mut seed = 0x9e37_79b9_7f4a_7c15;
        let buf: Vec<u8> = (0..(70 << 10)).map(|_| rng(&mut seed) as u8).collect();
        for _ in 0..300 {
            let mut h = [0u8; 16];
            h.iter_mut().for_each(|b| *b = rng(&mut seed) as u8);
            let off = (rng(&mut seed) % 64) as usize;
            let n = match rng(&mut seed) % 4 {
                0 => (rng(&mut seed) % 64) as usize,
                1 => (rng(&mut seed) % 8600) as usize,
                _ => (rng(&mut seed) % (64 << 10)) as usize,
            };
            let split = (rng(&mut seed) as usize) % (n + 1);
            let data = &buf[off..off + n];
            check(&h, &[data]);
            check(&h, &[&data[..split], &data[split..]]);
        }
    }

    #[test]
    fn matches_ghash_crate_on_edge_cases() {
        let data = vec![0xa5u8; 1 << 12];
        for h in [[0u8; 16], [0xffu8; 16], {
            let mut h = [0u8; 16];
            h[0] = 0x80;
            h
        }] {
            check(&h, &[]);
            check(&h, &[&[]]);
            for n in [1, 15, 16, 17, 127, 128, 129, 255, 256, 257, 4096] {
                check(&h, &[&data[..n]]);
            }
        }
    }

    #[test]
    fn matches_ghash_crate_on_16_mib_plus_16() {
        let n = (16 << 20) + 16;
        let data: Vec<u8> = (0..n)
            .map(|i: usize| i.wrapping_mul(131).wrapping_add(7) as u8)
            .collect();
        check(&[0x42; 16], &[&data[..8600], &data]);
    }
}
