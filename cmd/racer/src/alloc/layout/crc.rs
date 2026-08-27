/// CRC32C (Castagnoli), seeded so `crc32c_with(crc32c(a), b) == crc32c(a ++ b)`.
pub(super) fn crc32c_with(seed: u32, data: &[u8]) -> u32 {
    #[cfg(target_arch = "x86_64")]
    if std::is_x86_feature_detected!("sse4.2") {
        // Safety: guarded by the feature check above.
        return unsafe { crc32c_hw(seed, data) };
    }
    crc32c_sw(seed, data)
}

pub fn crc32c(data: &[u8]) -> u32 {
    crc32c_with(0, data)
}

/// Page checksum seeded with address and version, so a misdirected read or a page left by
/// a lost metadata write fails despite consistent bytes.
pub fn page_crc(addr: u64, version: u64, page: &[u8]) -> u32 {
    let mut seed = [0u8; 16];
    seed[0..8].copy_from_slice(&addr.to_le_bytes());
    seed[8..16].copy_from_slice(&version.to_le_bytes());
    crc32c_with(crc32c(&seed), page)
}

// The CRC32 instruction has three cycles of latency and issues one per cycle, so one
// accumulator runs the pipeline a third full. Three interleaved chains fill it and fold
// back by advancing earlier chains over later bytes: a constant GF(2) matrix per length.
#[cfg(target_arch = "x86_64")]
const LONG: usize = 1024;
#[cfg(target_arch = "x86_64")]
const SHORT: usize = 256;
#[cfg(target_arch = "x86_64")]
static LONG_OP: Shift = shift_table(&zeros(LONG));
#[cfg(target_arch = "x86_64")]
static SHORT_OP: Shift = shift_table(&zeros(SHORT));

#[cfg(target_arch = "x86_64")]
#[target_feature(enable = "sse4.2")]
unsafe fn crc32c_hw(seed: u32, data: &[u8]) -> u32 {
    use std::arch::x86_64::{_mm_crc32_u8, _mm_crc32_u64};
    let mut crc = !seed as u64;
    let mut p = data;
    unsafe {
        while p.len() >= 3 * LONG {
            crc = triple(crc, p, LONG, &LONG_OP);
            p = &p[3 * LONG..];
        }
        while p.len() >= 3 * SHORT {
            crc = triple(crc, p, SHORT, &SHORT_OP);
            p = &p[3 * SHORT..];
        }
    }
    let mut it = p.chunks_exact(8);
    for c in &mut it {
        crc = _mm_crc32_u64(crc, u64::from_le_bytes(c.try_into().unwrap()));
    }
    let mut crc = crc as u32;
    for &b in it.remainder() {
        crc = _mm_crc32_u8(crc, b);
    }
    !crc
}

/// One `3 * n` byte block, `n` a multiple of eight, folded back to a single CRC.
#[cfg(target_arch = "x86_64")]
#[target_feature(enable = "sse4.2")]
unsafe fn triple(seed: u64, block: &[u8], n: usize, op: &Shift) -> u64 {
    use std::arch::x86_64::_mm_crc32_u64;
    let (mut a, mut b, mut c) = (seed, 0u64, 0u64);
    let p = block.as_ptr();
    let mut i = 0;
    // Safety: the caller has checked `block.len() >= 3 * n`.
    unsafe {
        while i < n {
            a = _mm_crc32_u64(a, p.add(i).cast::<u64>().read_unaligned().to_le());
            b = _mm_crc32_u64(b, p.add(n + i).cast::<u64>().read_unaligned().to_le());
            c = _mm_crc32_u64(c, p.add(2 * n + i).cast::<u64>().read_unaligned().to_le());
            i += 8;
        }
    }
    let ab = shift(op, a as u32) ^ b as u32;
    (shift(op, ab) ^ c as u32) as u64
}

/// The bit-reflected CRC32C (Castagnoli) polynomial.
const POLY: u32 = 0x82f6_3b78;

/// Apply a GF(2) matrix, each column a 32-bit vector, to a CRC.
#[cfg(target_arch = "x86_64")]
const fn apply(m: &[u32; 32], mut v: u32) -> u32 {
    let mut sum = 0;
    let mut i = 0;
    while v != 0 {
        if v & 1 != 0 {
            sum ^= m[i];
        }
        v >>= 1;
        i += 1;
    }
    sum
}

/// A matrix flattened to one table per input byte: a fold is four loads and three XORs.
#[cfg(target_arch = "x86_64")]
type Shift = [[u32; 256]; 4];

#[cfg(target_arch = "x86_64")]
const fn shift_table(m: &[u32; 32]) -> Shift {
    let mut t = [[0u32; 256]; 4];
    let mut j = 0;
    while j < 4 {
        let mut b = 0;
        while b < 256 {
            t[j][b] = apply(m, (b as u32) << (8 * j));
            b += 1;
        }
        j += 1;
    }
    t
}

#[cfg(target_arch = "x86_64")]
#[inline]
fn shift(t: &Shift, v: u32) -> u32 {
    t[0][(v & 0xff) as usize]
        ^ t[1][(v >> 8 & 0xff) as usize]
        ^ t[2][(v >> 16 & 0xff) as usize]
        ^ t[3][(v >> 24) as usize]
}

/// `a` after `b`, as one matrix.
#[cfg(target_arch = "x86_64")]
const fn compose(a: &[u32; 32], b: &[u32; 32]) -> [u32; 32] {
    let mut out = [0u32; 32];
    let mut i = 0;
    while i < 32 {
        out[i] = apply(a, b[i]);
        i += 1;
    }
    out
}

/// The operator that advances a CRC over `len` zero bytes.
#[cfg(target_arch = "x86_64")]
const fn zeros(len: usize) -> [u32; 32] {
    // One zero bit: shift right, and reduce by the polynomial when a bit falls off.
    let mut bit = [0u32; 32];
    bit[0] = POLY;
    let mut i = 1;
    while i < 32 {
        bit[i] = 1 << (i - 1);
        i += 1;
    }
    // Square three times to reach one zero byte, then raise that to the `len`th.
    let mut step = compose(&bit, &bit);
    step = compose(&step, &step);
    step = compose(&step, &step);
    let mut out = [0u32; 32];
    let mut i = 0;
    while i < 32 {
        out[i] = 1 << i;
        i += 1;
    }
    let mut n = len;
    while n > 0 {
        if n & 1 != 0 {
            out = compose(&step, &out);
        }
        step = compose(&step, &step);
        n >>= 1;
    }
    out
}

const TABLE: [u32; 256] = {
    let mut t = [0u32; 256];
    let mut i = 0;
    while i < 256 {
        let mut c = i as u32;
        let mut k = 0;
        while k < 8 {
            c = if c & 1 != 0 {
                0x82f6_3b78 ^ (c >> 1)
            } else {
                c >> 1
            };
            k += 1;
        }
        t[i] = c;
        i += 1;
    }
    t
};

pub(super) fn crc32c_sw(seed: u32, data: &[u8]) -> u32 {
    let mut c = !seed;
    for &b in data {
        c = TABLE[((c ^ b as u32) & 0xff) as usize] ^ (c >> 8);
    }
    !c
}
