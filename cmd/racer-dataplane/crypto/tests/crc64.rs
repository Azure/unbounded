use racer_crypto::crc64;

// Independent bitwise oracle, with no library tables or folding constants.
fn reference(bytes: &[u8]) -> u64 {
    let mut crc = u64::MAX;
    for byte in bytes {
        crc ^= u64::from(*byte);
        for _ in 0..8 {
            crc = (crc >> 1)
                ^ if crc & 1 != 0 {
                    0xc96c_5795_d787_0f42
                } else {
                    0
                };
        }
    }
    !crc
}

fn random_bytes(length: usize) -> Vec<u8> {
    let mut state = 0x6a09_e667_f3bc_c909u64;
    (0..length)
        .map(|_| {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            state as u8
        })
        .collect()
}

fn assert_equivalent(bytes: &[u8]) {
    let expected = reference(bytes);
    assert_eq!(crc64(bytes), expected, "dispatch, length={}", bytes.len());
    let mut table = crc64fast::Digest::new_table();
    table.write(bytes);
    assert_eq!(table.sum64(), expected, "table, length={}", bytes.len());
}

#[test]
fn xz_golden_and_hardware_equivalence() {
    for (input, expected) in [(b"".as_slice(), 0), (b"123456789", 0x995d_c9bb_df19_39fa)] {
        assert_eq!(reference(input), expected);
        assert_eq!(crc64(input), expected);
    }
    let bytes = random_bytes(65537 + 32);
    for offset in 0..32 {
        for length in [
            0, 1, 7, 8, 9, 15, 16, 17, 31, 32, 63, 64, 127, 128, 129, 143, 144, 255, 256, 257,
            1023, 1024, 1025, 4095, 4096, 4097, 65535, 65536, 65537,
        ] {
            assert_equivalent(&bytes[offset..offset + length]);
        }
    }
    for chunk in bytes.chunks_exact(3).take(128) {
        let length = usize::from(u16::from_le_bytes([chunk[0], chunk[1]]));
        let offset = usize::from(chunk[2] & 31);
        assert_equivalent(&bytes[offset..offset + length]);
    }
}

#[test]
fn sixteen_mib_and_tag_match_independent_reference() {
    let length = 16 * 1024 * 1024 + 16;
    let bytes = random_bytes(length + 1);
    assert_equivalent(&bytes[1..]);
    let mut digest = crc64fast::Digest::new();
    for chunk in bytes[1..].chunks(4093) {
        digest.write(chunk);
    }
    assert_eq!(digest.sum64(), crc64(&bytes[1..]));
}

// Fail rather than silently skipping on the wrong CPU.
#[test]
#[ignore = "requires x86 PCLMULQDQ, SSE2 and SSE4.1; run explicitly on supported hardware"]
fn pclmul_hardware_path_executes_when_available() {
    #[cfg(any(target_arch = "x86", target_arch = "x86_64"))]
    {
        assert!(std::is_x86_feature_detected!("pclmulqdq"));
        assert!(std::is_x86_feature_detected!("sse2"));
        assert!(std::is_x86_feature_detected!("sse4.1"));
        assert_equivalent(&random_bytes(16384));
        eprintln!("CRC64/XZ crc64fast dispatch executed: x86 PCLMULQDQ");
    }
    #[cfg(not(any(target_arch = "x86", target_arch = "x86_64")))]
    panic!("x86 hardware verification requires an x86 host");
}

#[test]
#[ignore = "requires AArch64 PMULL and NEON; run explicitly on supported hardware"]
fn pmull_hardware_path_executes_when_available() {
    #[cfg(target_arch = "aarch64")]
    {
        assert!(std::arch::is_aarch64_feature_detected!("pmull"));
        assert!(std::arch::is_aarch64_feature_detected!("neon"));
        assert_equivalent(&random_bytes(16384));
        eprintln!("CRC64/XZ crc64fast dispatch executed: AArch64 PMULL");
    }
    #[cfg(not(target_arch = "aarch64"))]
    panic!("PMULL hardware verification requires an AArch64 host");
}
