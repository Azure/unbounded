//! Azure Storage CRC-64 for `x-ms-content-crc64`.
//!
//! The Azure SDK for Go (`sdk/storage/azblob/internal/shared/shared.go`)
//! builds its table with `crc64.MakeTable(0x9A6C9329AC4BC9B5)` and hashes
//! with `crc64.Checksum`. Go's `hash/crc64` treats the polynomial as
//! reflected (LSB first) and inverts the register on entry and exit, so this
//! is CRC-64/NVME (normal polynomial `0xAD93D23594C93659`, check value
//! `0xAE8B14860A799888` for `"123456789"`). The header value is the 8-byte
//! little-endian encoding of the CRC in standard base64
//! (`internal/exported/transfer_validation_option.go`,
//! `binary.LittleEndian.PutUint64`).

use base64::Engine;

/// Reflected Azure Storage polynomial, as passed to Go `crc64.MakeTable`.
pub const POLYNOMIAL: u64 = 0x9A6C_9329_AC4B_C9B5;

const TABLE: [u64; 256] = table();

const fn table() -> [u64; 256] {
    let mut table = [0u64; 256];
    let mut i = 0;
    while i < 256 {
        let mut crc = i as u64;
        let mut bit = 0;
        while bit < 8 {
            crc = if crc & 1 == 1 {
                (crc >> 1) ^ POLYNOMIAL
            } else {
                crc >> 1
            };
            bit += 1;
        }
        table[i] = crc;
        i += 1;
    }
    table
}

/// Continue a checksum over `data`, like Go `crc64.Update`. Start from `0`.
pub fn update(crc: u64, data: &[u8]) -> u64 {
    let mut crc = !crc;
    for byte in data {
        crc = TABLE[usize::from(crc as u8 ^ byte)] ^ (crc >> 8);
    }
    !crc
}

/// The Azure CRC-64 of `data`, like Go `crc64.Checksum`.
pub fn checksum(data: &[u8]) -> u64 {
    update(0, data)
}

/// The `x-ms-content-crc64` header value for `crc`.
pub fn header_value(crc: u64) -> String {
    base64::engine::general_purpose::STANDARD.encode(crc.to_le_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Vectors from Go `crc64.Checksum(data, crc64.MakeTable(0x9A6C9329AC4BC9B5))`
    /// with the header encoding azblob uses (go1, `hash/crc64`).
    #[test]
    fn matches_azure_sdk_for_go() {
        let mut big = vec![0u8; 4096];
        for (i, byte) in big.iter_mut().enumerate() {
            *byte = (i * 31 + 7) as u8;
        }
        let vectors: [(&[u8], u64, &str); 5] = [
            (b"", 0, "AAAAAAAAAAA="),
            (b"a", 0x8c2f_8445_b4cb_fc3c, "PPzLtEWEL4w="),
            (b"123456789", 0xae8b_1486_0a79_9888, "iJh5CoYUi64="),
            (b"Hello, World!", 0xd4a9_be43_26ad_d24d, "TdKtJkO+qdQ="),
            (&big, 0x4b43_77fd_e5ba_15fc, "/BW65f13Q0s="),
        ];
        for (data, crc, header) in vectors {
            assert_eq!(checksum(data), crc);
            assert_eq!(header_value(crc), header);
        }
    }

    #[test]
    fn update_is_incremental() {
        let data = b"the quick brown fox jumps over the lazy dog";
        for split in 0..=data.len() {
            let (head, tail) = data.split_at(split);
            assert_eq!(update(update(0, head), tail), checksum(data));
        }
    }

    #[test]
    fn table_matches_bitwise_definition() {
        assert_eq!(TABLE[0], 0);
        assert_eq!(TABLE[128], POLYNOMIAL);
    }
}
