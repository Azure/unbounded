// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn canonical_checksum_and_fixed_metadata() {
    let checksum = Checksum(std::array::from_fn(|i| i as u8 * 7));
    let tag = checksum.etag();
    assert_eq!(Checksum::from_etag(tag.as_str()).unwrap(), checksum);
    for bad in [
        String::new(),
        "\"v1\"".into(),
        format!("W/{}", tag.as_str()),
        tag.as_str().to_uppercase(),
        tag.as_str()[1..65].into(),
        format!("\"{}\"", "g".repeat(64)),
        format!("{}, {}", tag.as_str(), tag.as_str()),
    ] {
        assert!(Checksum::from_etag(&bad).is_err(), "{bad}");
    }
    let metadata = Metadata {
        content_type: ContentType::new(b"application/vnd.oci.image.manifest.v1+json").unwrap(),
        checksum,
        len: u64::MAX,
        expires: 1234,
    };
    assert_eq!(Metadata::SIZE, 306);
    assert_eq!(
        Metadata::from_bytes(&metadata.to_bytes()).unwrap(),
        metadata
    );
    assert_eq!(&metadata.to_bytes()[..32], &checksum.0);
    assert_eq!(&metadata.to_bytes()[32..40], &u64::MAX.to_le_bytes());
    assert!(Metadata::from_bytes(&[0; 47]).is_err());
    assert!(Metadata::from_bytes(&[0; 49]).is_err());
}

#[test]
fn content_type_exact_bound_and_canonical_disk_padding() {
    for len in [1, 255, 256] {
        let value = vec![b'x'; len];
        let metadata = Metadata {
            checksum: Checksum([9; 32]),
            len: 123,
            expires: 456,
            content_type: ContentType::new(&value).unwrap(),
        };
        let bytes = metadata.to_bytes();
        assert_eq!(
            u16::from_le_bytes(bytes[48..50].try_into().unwrap()) as usize,
            len
        );
        assert_eq!(&bytes[50..50 + len], &value);
        assert!(bytes[50 + len..].iter().all(|b| *b == 0));
        assert_eq!(Metadata::from_bytes(&bytes).unwrap(), metadata);
        if len < 256 {
            let mut bad = bytes;
            bad[305] = 1;
            assert!(Metadata::from_bytes(&bad).is_err());
        }
    }
    assert!(ContentType::new(&[b'x'; 257]).is_err());
    assert!(ContentType::new(b"text/plain\r\ninjected: true").is_err());
    let mut bad = [0; Metadata::SIZE];
    bad[48..50].copy_from_slice(&257u16.to_le_bytes());
    assert!(Metadata::from_bytes(&bad).is_err());
    assert_eq!(
        Metadata::from_bytes(&[0; Metadata::SIZE])
            .unwrap()
            .content_type
            .as_bytes(),
        None
    );
}
