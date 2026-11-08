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
        // SAFETY: equal-length live slices; `dst` is exclusive so they do not
        // overlap.
        unsafe { x86::copy(dst, src) };
        return;
    }
    dst.copy_from_slice(src);
}

/// Copy `src` into `dst` (equal lengths) with non-temporal stores at any
/// size. For chunked passes whose chunks are each below
/// [`STREAM_THRESHOLD`] but whose total is not. Falls back to
/// `copy_from_slice` off x86-64.
///
/// # Panics
/// When the lengths differ, like [`slice::copy_from_slice`].
pub fn copy_nontemporal(dst: &mut [u8], src: &[u8]) {
    assert_eq!(dst.len(), src.len(), "copy_nontemporal length mismatch");
    #[cfg(all(target_arch = "x86_64", not(miri)))]
    {
        // SAFETY: equal-length live slices; `dst` is exclusive so they do not
        // overlap.
        unsafe { x86::copy(dst, src) };
    }
    #[cfg(not(all(target_arch = "x86_64", not(miri))))]
    dst.copy_from_slice(src);
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
        time("explicit_bzero", &mut |r| {
            crate::buf::wipe_bytes_libc(&mut dsts[r % 8])
        });
        time("zero_streaming", &mut |r| {
            assert!(zero_streaming(&mut dsts[r % 8]))
        });
    }
}
