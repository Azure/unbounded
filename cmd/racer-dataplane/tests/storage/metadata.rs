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
        checksum,
        len: u64::MAX,
        expires: 1234,
    };
    assert_eq!(std::mem::size_of::<Metadata>(), 48);
    assert_eq!(
        Metadata::from_bytes(&metadata.to_bytes()).unwrap(),
        metadata
    );
    assert_eq!(&metadata.to_bytes()[..32], &checksum.0);
    assert_eq!(&metadata.to_bytes()[32..40], &u64::MAX.to_le_bytes());
    assert!(Metadata::from_bytes(&[0; 47]).is_err());
    assert!(Metadata::from_bytes(&[0; 49]).is_err());
}
