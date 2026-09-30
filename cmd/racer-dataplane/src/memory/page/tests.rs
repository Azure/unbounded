use super::*;
use crate::{
    error::Error,
    model::{
        CacheId, CacheKey, ExpiresAt, KeyId, Nonce, ObjectId, ObjectVersion, PageEnvelope, PageId,
        PageNumber, StrongEtag,
    },
};

#[test]
fn shared_results_reject_truncated_or_padded_ciphertext() {
    let admission = crate::memory::pool::tests::admission(8);
    for length in [0, 18, 19, 20] {
        let mut page = crate::memory::pool::tests::bundle(&admission, "v1");
        std::sync::Arc::get_mut(&mut page.ciphertext.inner)
            .unwrap()
            .bytes
            .resize(length, 0);
        let expected = if length == 19 {
            Ok(())
        } else {
            Err(Error::CorruptRecord)
        };
        assert_eq!(page.validate_metadata(), expected);
        assert_eq!(page.copy().validate_metadata(), expected);
    }
}

#[test]
fn shared_result_rejects_mixed_metadata_plaintext_and_ciphertext() {
    let metadata = ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        },
        length: 3,
        expires_at: ExpiresAt(std::time::UNIX_EPOCH),
    };
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    let envelope = PageEnvelope {
        page: page.clone(),
        key_id: KeyId([0; 16]),
        nonce: Nonce([0; 24]),
        plaintext_length: 3,
        ciphertext_length: 19,
    };
    assert_eq!(validate_association(&metadata, &page, 3, &envelope), Ok(()));
    assert_eq!(
        validate_association(&metadata, &page, 2, &envelope),
        Err(Error::CorruptRecord)
    );
    let mut wrong = page.clone();
    wrong.version.etag = StrongEtag::test_value("v2");
    assert_eq!(
        validate_association(&metadata, &wrong, 3, &envelope),
        Err(Error::CorruptRecord)
    );
    let mut wrong_metadata = metadata.clone();
    wrong_metadata.version.object.key = CacheKey([1; 32]);
    assert_eq!(
        validate_association(&wrong_metadata, &page, 3, &envelope),
        Err(Error::CorruptRecord)
    );
    wrong_metadata = metadata.clone();
    wrong_metadata.length = 4;
    assert_eq!(
        validate_association(&wrong_metadata, &page, 3, &envelope),
        Err(Error::CorruptRecord)
    );
}
