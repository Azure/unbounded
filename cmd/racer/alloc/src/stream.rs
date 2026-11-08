//! Bulk page-sized copies and fills that bypass the CPU cache.
//!
//! Racer moves whole pages (up to 16 MiB) whose destination is not read again
//! soon by the writing core: ciphertext detached from a registered RDMA buffer
//! is next read by an `O_DIRECT` DMA write, and a wiped plaintext buffer is
//! idle until its next checkout. Regular (temporal) stores at that size read
//! every destination line into cache before overwriting it (read for
//! ownership) and evict useful data. Non-temporal stores skip both, so the
//! memory traffic for a copy drops from three passes to two and for a fill
//! from two passes to one.
//!
//! glibc switches to non-temporal stores only above a host-dependent
//! threshold (24 MiB on the benchmark hosts), above Racer's page size.
//!
//! Below [`STREAM_THRESHOLD`] or off x86-64, these fall back to ordinary
//! `copy_from_slice` and the caller's fill.

/// Smallest length that uses non-temporal stores. Smaller buffers likely fit
/// in cache, where ordinary stores are faster.
pub const STREAM_THRESHOLD: usize = 1 << 20;

/// Copy `src` into `dst` (equal lengths) with non-temporal stores when large.
///
/// # Panics
/// When the lengths differ, like [`slice::copy_from_slice`].
pub fn copy_streaming(dst: &mut [u8], src: &[u8]) {
    assert_eq!(dst.len(), src.len(), "copy_streaming length mismatch");
    #[cfg(all(target_arch = "x86_64", not(miri)))]
    if dst.len() >= STREAM_THRESHOLD {
        copy_nontemporal_as(CopyKind::best(), dst, src);
        return;
    }
    dst.copy_from_slice(src);
}

/// Copy `src` into `dst` (equal lengths) with non-temporal stores at any
/// size. For chunked passes whose chunks are each below
/// [`STREAM_THRESHOLD`] but whose total is not. Uses [`CopyKind::best`]. Falls back to `copy_from_slice` off x86-64.
///
/// # Panics
/// When the lengths differ, like [`slice::copy_from_slice`].
pub fn copy_nontemporal(dst: &mut [u8], src: &[u8]) {
    copy_nontemporal_as(CopyKind::best(), dst, src);
}

/// Store width and source prefetch of a non-temporal copy, for
/// [`copy_nontemporal_as`] and benchmarks.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CopyKind {
    /// 16-byte SSE2 stores, baseline on x86-64.
    Sse2,
    /// 32-byte AVX stores.
    Avx2,
    /// [`Self::Avx2`] with a software prefetch of the source
    /// [`PREFETCH_AHEAD`] bytes ahead.
    Avx2Prefetch,
    /// 64-byte AVX-512 stores: one store per cache line.
    Avx512,
    /// [`Self::Avx512`] with a software prefetch of the source
    /// [`PREFETCH_AHEAD`] bytes ahead.
    Avx512Prefetch,
}

/// Distance in bytes that [`CopyKind::Avx512Prefetch`] prefetches ahead.
pub const PREFETCH_AHEAD: usize = 2048;

impl CopyKind {
    /// Every kind, narrowest first.
    pub const ALL: [Self; 5] = [
        Self::Sse2,
        Self::Avx2,
        Self::Avx2Prefetch,
        Self::Avx512,
        Self::Avx512Prefetch,
    ];

    /// Whether this CPU can run the kind. Off x86-64 every kind is a plain
    /// copy, so all are available.
    pub fn available(self) -> bool {
        #[cfg(all(target_arch = "x86_64", not(miri)))]
        {
            match self {
                Self::Sse2 => true,
                Self::Avx2 | Self::Avx2Prefetch => std::is_x86_feature_detected!("avx"),
                Self::Avx512 | Self::Avx512Prefetch => std::is_x86_feature_detected!("avx512f"),
            }
        }
        #[cfg(not(all(target_arch = "x86_64", not(miri))))]
        {
            let _ = self;
            true
        }
    }

    /// The kind [`copy_nontemporal`] uses: [`Self::Avx2Prefetch`] when AVX is
    /// available, else [`Self::Sse2`].
    ///
    /// On an AMD EPYC 9V74 (Zen 4) VM, `stream_bench` copies 16 MiB in
    /// 128 KiB chunks at about 22 GB/s with SSE2, 24 with AVX2, 22 with
    /// AVX-512, and 25 with AVX2 or AVX-512 plus prefetch. In the fused
    /// decrypt (`racer-crypto` `fused_numa` bench) all kinds are within 3%,
    /// the prefetching ones slightly ahead, because the pass waits on DRAM
    /// reads of the source rather than on stores. AVX2 avoids the AVX-512
    /// frequency penalty of some Intel parts.
    pub fn best() -> Self {
        use std::sync::atomic::{AtomicU8, Ordering};
        static BEST: AtomicU8 = AtomicU8::new(u8::MAX);
        let cached = BEST.load(Ordering::Relaxed);
        if let Some(kind) = Self::ALL.get(usize::from(cached)) {
            return *kind;
        }
        let kind = if Self::Avx2Prefetch.available() {
            Self::Avx2Prefetch
        } else {
            Self::Sse2
        };
        let index = Self::ALL.iter().position(|k| *k == kind).unwrap_or(0);
        BEST.store(index as u8, Ordering::Relaxed);
        kind
    }
}

/// [`copy_nontemporal`] with an explicit [`CopyKind`]. A kind this CPU cannot
/// run ([`CopyKind::available`]) falls back to SSE2.
///
/// # Panics
/// When the lengths differ, like [`slice::copy_from_slice`].
pub fn copy_nontemporal_as(kind: CopyKind, dst: &mut [u8], src: &[u8]) {
    assert_eq!(dst.len(), src.len(), "copy_nontemporal length mismatch");
    #[cfg(all(target_arch = "x86_64", not(miri)))]
    {
        // SAFETY: equal-length live slices; `dst` is exclusive so they do not
        // overlap. Each wide kind runs only after its feature is detected.
        unsafe {
            match kind {
                CopyKind::Avx2 if kind.available() => x86::copy_avx::<false>(dst, src),
                CopyKind::Avx2Prefetch if kind.available() => x86::copy_avx::<true>(dst, src),
                CopyKind::Avx512 if kind.available() => x86::copy_avx512::<false>(dst, src),
                CopyKind::Avx512Prefetch if kind.available() => x86::copy_avx512::<true>(dst, src),
                _ => x86::copy(dst, src),
            }
        }
    }
    #[cfg(not(all(target_arch = "x86_64", not(miri))))]
    {
        let _ = kind;
        dst.copy_from_slice(src);
    }
}

/// Zero `bytes` with non-temporal stores when large. Returns `false` without
/// touching `bytes` when it is too small or the target has no streaming
/// path, so the caller can use its own fill.
///
/// The stores are fenced and the buffer is passed through
/// [`core::hint::black_box`] afterwards, so they cannot be elided; this is
/// safe to use for erasing secrets.
pub fn zero_streaming(bytes: &mut [u8]) -> bool {
    #[cfg(all(target_arch = "x86_64", not(miri)))]
    if bytes.len() >= STREAM_THRESHOLD {
        // SAFETY: live exclusive slice.
        unsafe { x86::zero(bytes) };
        core::sync::atomic::compiler_fence(core::sync::atomic::Ordering::SeqCst);
        core::hint::black_box(bytes);
        return true;
    }
    let _ = bytes;
    false
}

#[cfg(all(target_arch = "x86_64", not(miri)))]
mod x86 {
    use core::arch::x86_64::{
        __m128i, _mm_loadu_si128, _mm_setzero_si128, _mm_sfence, _mm_stream_si128,
    };

    const LANE: usize = 16;

    // Bytes before the first 16-byte aligned address in `dst`.
    fn head(dst: *const u8, len: usize) -> usize {
        dst.align_offset(LANE).min(len)
    }

    /// # Safety
    /// `dst` and `src` are live, equal-length, and non-overlapping.
    pub(super) unsafe fn copy(dst: &mut [u8], src: &[u8]) {
        let len = dst.len();
        let lead = head(dst.as_ptr(), len);
        dst[..lead].copy_from_slice(&src[..lead]);
        let body = (len - lead) / LANE * LANE;
        let d = dst[lead..].as_mut_ptr();
        let s = src[lead..].as_ptr();
        let mut offset = 0;
        // Four lanes per iteration keeps the store buffer full.
        while offset + 4 * LANE <= body {
            // SAFETY: `offset + 64 <= body` stays inside both slices; `d` is
            // 16-byte aligned, loads are unaligned.
            unsafe {
                let a = _mm_loadu_si128(s.add(offset).cast::<__m128i>());
                let b = _mm_loadu_si128(s.add(offset + LANE).cast::<__m128i>());
                let c = _mm_loadu_si128(s.add(offset + 2 * LANE).cast::<__m128i>());
                let e = _mm_loadu_si128(s.add(offset + 3 * LANE).cast::<__m128i>());
                _mm_stream_si128(d.add(offset).cast::<__m128i>(), a);
                _mm_stream_si128(d.add(offset + LANE).cast::<__m128i>(), b);
                _mm_stream_si128(d.add(offset + 2 * LANE).cast::<__m128i>(), c);
                _mm_stream_si128(d.add(offset + 3 * LANE).cast::<__m128i>(), e);
            }
            offset += 4 * LANE;
        }
        while offset < body {
            // SAFETY: as above, one lane.
            unsafe {
                let a = _mm_loadu_si128(s.add(offset).cast::<__m128i>());
                _mm_stream_si128(d.add(offset).cast::<__m128i>(), a);
            }
            offset += LANE;
        }
        // SAFETY: SSE2 is baseline on x86-64. Orders the streaming stores
        // before any later release of the buffer to another thread.
        unsafe { _mm_sfence() };
        let tail = lead + body;
        dst[tail..].copy_from_slice(&src[tail..]);
    }

    /// 32-byte non-temporal stores, four per iteration. With `PREFETCH`, the
    /// source is prefetched [`super::PREFETCH_AHEAD`] bytes ahead.
    ///
    /// # Safety
    /// `dst` and `src` are live, equal-length, and non-overlapping, and the
    /// CPU supports AVX.
    #[target_feature(enable = "avx")]
    pub(super) unsafe fn copy_avx<const PREFETCH: bool>(dst: &mut [u8], src: &[u8]) {
        use core::arch::x86_64::{
            __m256i, _MM_HINT_T0, _mm_prefetch, _mm256_loadu_si256, _mm256_stream_si256,
        };
        const WIDE: usize = 32;
        let len = dst.len();
        let lead = dst.as_ptr().align_offset(WIDE).min(len);
        dst[..lead].copy_from_slice(&src[..lead]);
        let body = (len - lead) / (4 * WIDE) * (4 * WIDE);
        let d = dst[lead..].as_mut_ptr();
        let s = src[lead..].as_ptr();
        let mut offset = 0;
        while offset < body {
            if PREFETCH && offset + super::PREFETCH_AHEAD < body {
                // SAFETY: prefetch never faults; the address is in bounds.
                unsafe {
                    let ahead = s.add(offset + super::PREFETCH_AHEAD).cast::<i8>();
                    _mm_prefetch::<_MM_HINT_T0>(ahead);
                    _mm_prefetch::<_MM_HINT_T0>(ahead.add(2 * WIDE));
                }
            }
            // SAFETY: `offset + 128 <= body` stays inside both slices; `d` is
            // 32-byte aligned, loads are unaligned.
            unsafe {
                let a = _mm256_loadu_si256(s.add(offset).cast::<__m256i>());
                let b = _mm256_loadu_si256(s.add(offset + WIDE).cast::<__m256i>());
                let c = _mm256_loadu_si256(s.add(offset + 2 * WIDE).cast::<__m256i>());
                let e = _mm256_loadu_si256(s.add(offset + 3 * WIDE).cast::<__m256i>());
                _mm256_stream_si256(d.add(offset).cast::<__m256i>(), a);
                _mm256_stream_si256(d.add(offset + WIDE).cast::<__m256i>(), b);
                _mm256_stream_si256(d.add(offset + 2 * WIDE).cast::<__m256i>(), c);
                _mm256_stream_si256(d.add(offset + 3 * WIDE).cast::<__m256i>(), e);
            }
            offset += 4 * WIDE;
        }
        // SAFETY: the remainder is a live equal-length pair; SSE2 is baseline.
        unsafe { copy(&mut dst[lead + body..], &src[lead + body..]) };
    }

    /// 64-byte non-temporal stores, one cache line each, four per
    /// iteration. With `PREFETCH`, the source is prefetched
    /// [`super::PREFETCH_AHEAD`] bytes ahead.
    ///
    /// # Safety
    /// `dst` and `src` are live, equal-length, and non-overlapping, and the
    /// CPU supports AVX-512F.
    #[target_feature(enable = "avx512f")]
    pub(super) unsafe fn copy_avx512<const PREFETCH: bool>(dst: &mut [u8], src: &[u8]) {
        use core::arch::x86_64::{
            __m512i, _MM_HINT_T0, _mm_prefetch, _mm512_loadu_si512, _mm512_stream_si512,
        };
        const WIDE: usize = 64;
        let len = dst.len();
        let lead = dst.as_ptr().align_offset(WIDE).min(len);
        dst[..lead].copy_from_slice(&src[..lead]);
        let body = (len - lead) / (4 * WIDE) * (4 * WIDE);
        let d = dst[lead..].as_mut_ptr();
        let s = src[lead..].as_ptr();
        let mut offset = 0;
        while offset < body {
            if PREFETCH && offset + super::PREFETCH_AHEAD < body {
                // SAFETY: prefetch never faults; the address is in bounds.
                unsafe {
                    let ahead = s.add(offset + super::PREFETCH_AHEAD).cast::<i8>();
                    _mm_prefetch::<_MM_HINT_T0>(ahead);
                    _mm_prefetch::<_MM_HINT_T0>(ahead.add(WIDE));
                    _mm_prefetch::<_MM_HINT_T0>(ahead.add(2 * WIDE));
                    _mm_prefetch::<_MM_HINT_T0>(ahead.add(3 * WIDE));
                }
            }
            // SAFETY: `offset + 256 <= body` stays inside both slices; `d` is
            // 64-byte aligned, loads are unaligned.
            unsafe {
                let a = _mm512_loadu_si512(s.add(offset).cast::<__m512i>());
                let b = _mm512_loadu_si512(s.add(offset + WIDE).cast::<__m512i>());
                let c = _mm512_loadu_si512(s.add(offset + 2 * WIDE).cast::<__m512i>());
                let e = _mm512_loadu_si512(s.add(offset + 3 * WIDE).cast::<__m512i>());
                _mm512_stream_si512(d.add(offset).cast::<__m512i>(), a);
                _mm512_stream_si512(d.add(offset + WIDE).cast::<__m512i>(), b);
                _mm512_stream_si512(d.add(offset + 2 * WIDE).cast::<__m512i>(), c);
                _mm512_stream_si512(d.add(offset + 3 * WIDE).cast::<__m512i>(), e);
            }
            offset += 4 * WIDE;
        }
        // SAFETY: the remainder is a live equal-length pair; SSE2 is baseline
        // and its copy ends with the sfence that orders these stores too.
        unsafe { copy(&mut dst[lead + body..], &src[lead + body..]) };
    }

    /// # Safety
    /// `bytes` is a live exclusive slice.
    pub(super) unsafe fn zero(bytes: &mut [u8]) {
        let len = bytes.len();
        let lead = head(bytes.as_ptr(), len);
        bytes[..lead].fill(0);
        let body = (len - lead) / LANE * LANE;
        let d = bytes[lead..].as_mut_ptr();
        // SAFETY: SSE2 is baseline on x86-64.
        let zero = unsafe { _mm_setzero_si128() };
        let mut offset = 0;
        while offset < body {
            // SAFETY: `offset + 16 <= body`; `d` is 16-byte aligned.
            unsafe { _mm_stream_si128(d.add(offset).cast::<__m128i>(), zero) };
            offset += LANE;
        }
        // SAFETY: SSE2 is baseline on x86-64.
        unsafe { _mm_sfence() };
        bytes[lead + body..].fill(0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pattern(len: usize) -> Vec<u8> {
        (0..len).map(|i| (i * 31 + 7) as u8).collect()
    }

    #[test]
    fn copy_matches_for_every_alignment_and_odd_lengths() {
        for len in [0, 1, 15, 4096, STREAM_THRESHOLD - 1, STREAM_THRESHOLD + 17] {
            for shift in [0, 1, 7, 15] {
                let src = pattern(len + shift);
                let mut dst = vec![0u8; len + 32];
                copy_streaming(&mut dst[shift..shift + len], &src[shift..]);
                assert_eq!(
                    &dst[shift..shift + len],
                    &src[shift..],
                    "len {len} shift {shift}"
                );
                assert!(dst[..shift].iter().all(|&b| b == 0));
                assert!(dst[shift + len..].iter().all(|&b| b == 0));
            }
        }
    }

    #[test]
    fn copy_nontemporal_matches_for_small_and_unaligned_lengths() {
        for len in [0, 1, 15, 17, 4096 + 3, 128 << 10] {
            for shift in [0, 1, 15] {
                let src = pattern(len + shift);
                let mut dst = vec![0u8; len + 32];
                copy_nontemporal(&mut dst[shift..shift + len], &src[shift..]);
                assert_eq!(&dst[shift..shift + len], &src[shift..]);
                assert!(dst[..shift].iter().all(|&b| b == 0));
                assert!(dst[shift + len..].iter().all(|&b| b == 0));
            }
        }
    }

    #[test]
    fn every_copy_kind_matches_for_small_and_unaligned_lengths() {
        for kind in CopyKind::ALL {
            for len in [
                0,
                1,
                31,
                63,
                64,
                255,
                257,
                4096 + 3,
                128 << 10,
                (128 << 10) + 65,
            ] {
                for shift in [0, 1, 15, 33, 63] {
                    let src = pattern(len + shift);
                    let mut dst = vec![0u8; len + 128];
                    copy_nontemporal_as(kind, &mut dst[shift..shift + len], &src[shift..]);
                    assert_eq!(
                        &dst[shift..shift + len],
                        &src[shift..],
                        "{kind:?} {len} {shift}"
                    );
                    assert!(dst[..shift].iter().all(|&b| b == 0));
                    assert!(dst[shift + len..].iter().all(|&b| b == 0));
                }
            }
        }
        assert!(CopyKind::Sse2.available());
        assert!(CopyKind::best().available());
    }

    #[test]
    #[should_panic(expected = "length mismatch")]
    fn copy_rejects_mismatched_lengths() {
        copy_streaming(&mut [0u8; 4], &[0u8; 5]);
    }

    #[test]
    fn zero_streams_large_buffers_and_declines_small_ones() {
        let mut small = vec![0xAAu8; STREAM_THRESHOLD - 1];
        assert!(!zero_streaming(&mut small));
        assert!(
            small.iter().all(|&b| b == 0xAA),
            "declined fill must not touch bytes"
        );
        for shift in [0, 3, 15] {
            let mut large = vec![0xAAu8; STREAM_THRESHOLD + 33];
            let streamed = zero_streaming(&mut large[shift..]);
            assert_eq!(streamed, cfg!(all(target_arch = "x86_64", not(miri))));
            if streamed {
                assert!(large[shift..].iter().all(|&b| b == 0));
                assert!(large[..shift].iter().all(|&b| b == 0xAA));
            }
        }
    }

    /// Per-page cost of the streaming paths against the libc defaults.
    /// Run with `cargo test -p page-alloc --release -- --ignored stream_bench --nocapture`.
    #[test]
    #[ignore = "microbenchmark"]
    fn stream_bench() {
        const PAGE: usize = 16 << 20;
        const ROUNDS: usize = 64;
        // Distinct buffers per round defeat the cache like the real pool.
        let srcs: Vec<Vec<u8>> = (0..8).map(|_| pattern(PAGE)).collect();
        let mut dsts: Vec<Vec<u8>> = (0..8).map(|_| vec![1u8; PAGE]).collect();
        let time = |name: &str, f: &mut dyn FnMut(usize)| {
            let start = std::time::Instant::now();
            for round in 0..ROUNDS {
                f(round);
            }
            let secs = start.elapsed().as_secs_f64();
            let gbs = (PAGE * ROUNDS) as f64 / secs / 1e9;
            println!(
                "{name}: {:.2} ms/page, {gbs:.2} GB/s",
                secs * 1e3 / ROUNDS as f64
            );
        };
        time("copy_from_slice", &mut |r| {
            dsts[r % 8].copy_from_slice(&srcs[(r + 3) % 8])
        });
        time("copy_streaming", &mut |r| {
            copy_streaming(&mut dsts[r % 8], &srcs[(r + 3) % 8])
        });
        for kind in CopyKind::ALL.into_iter().filter(|k| k.available()) {
            // 128 KiB chunks, as the fused decrypt copies.
            time(
                &format!("copy_nontemporal {kind:?} 128 KiB chunks"),
                &mut |r| {
                    let (dst, src) = (&mut dsts[r % 8], &srcs[(r + 3) % 8]);
                    for (d, s) in dst.chunks_mut(128 << 10).zip(src.chunks(128 << 10)) {
                        copy_nontemporal_as(kind, d, s);
                    }
                },
            );
        }
        time("explicit_bzero", &mut |r| {
            crate::buf::wipe_bytes_libc(&mut dsts[r % 8])
        });
        time("zero_streaming", &mut |r| {
            assert!(zero_streaming(&mut dsts[r % 8]))
        });
    }
}
