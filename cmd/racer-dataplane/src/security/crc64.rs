//! CRC-64/ECMA-182, non-reflected, initial/final XOR zero. Not authentication.
//! PCLMUL accelerates polynomial multiplication; table reduction uses the same
//! ECMA polynomial. No CRC32 instruction or alternate wire polynomial is used.
const POLY: u64 = 0x42f0_e1eb_a9ea_3693;

const fn byte(mut crc: u64, value: u8) -> u64 {
    crc ^= (value as u64) << 56;
    let mut i = 0;
    while i < 8 {
        crc = (crc << 1) ^ if crc >> 63 != 0 { POLY } else { 0 };
        i += 1;
    }
    crc
}
const fn reductions() -> [[u64; 256]; 8] {
    let mut tables = [[0; 256]; 8];
    let mut position = 0;
    while position < 8 {
        let mut value = 0;
        while value < 256 {
            let mut crc = (value as u64) << (position * 8);
            let mut i = 0;
            while i < 8 {
                crc = byte(crc, 0);
                i += 1;
            }
            tables[position][value] = crc;
            value += 1;
        }
        position += 1;
    }
    tables
}
static REDUCE: [[u64; 256]; 8] = reductions();

pub fn checksum(bytes: &[u8]) -> u64 {
    #[cfg(target_arch = "x86_64")]
    if std::is_x86_feature_detected!("pclmulqdq") {
        // SAFETY: runtime feature detection guards the only accelerated entry.
        return unsafe { pclmul(bytes) };
    }
    portable(bytes)
}

pub fn portable(bytes: &[u8]) -> u64 {
    bytes.iter().fold(0, |crc, value| byte(crc, *value))
}

#[cfg(target_arch = "x86_64")]
#[target_feature(enable = "pclmulqdq")]
unsafe fn pclmul(bytes: &[u8]) -> u64 {
    use std::arch::x86_64::*;
    let polynomial = _mm_set_epi64x(0, POLY as i64);
    let mut crc = 0;
    let mut chunks = bytes.chunks_exact(8);
    for chunk in &mut chunks {
        let value = crc ^ u64::from_be_bytes(chunk.try_into().unwrap());
        let product = _mm_clmulepi64_si128::<0>(_mm_set_epi64x(0, value as i64), polynomial);
        let low = _mm_cvtsi128_si64(product) as u64;
        let high = _mm_cvtsi128_si64(_mm_srli_si128::<8>(product)) as u64;
        crc = low;
        for (i, table) in REDUCE.iter().enumerate() {
            crc ^= table[((high >> (i * 8)) & 255) as usize];
        }
    }
    chunks
        .remainder()
        .iter()
        .fold(crc, |crc, value| byte(crc, *value))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn ecma_golden_and_hardware_equivalence() {
        assert_eq!(checksum(b""), 0);
        assert_eq!(checksum(b"123456789"), 0x6c40_df5f_0b49_7347);
        let bytes: Vec<_> = (0..65537).map(|i| (i * 73 + i / 19) as u8).collect();
        for length in [1, 7, 8, 9, 15, 16, 31, 4096, 65537] {
            assert_eq!(checksum(&bytes[..length]), portable(&bytes[..length]));
            #[cfg(target_arch = "x86_64")]
            if std::is_x86_feature_detected!("pclmulqdq") {
                assert_eq!(
                    unsafe { pclmul(&bytes[..length]) },
                    portable(&bytes[..length])
                );
            }
        }
    }
}
