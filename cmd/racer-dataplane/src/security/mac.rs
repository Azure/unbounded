//! HMAC-SHA256 with an explicit request-only derivation from a credential epoch.
//! The derived key is never used for AEAD and cannot be obtained from a page key.
use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

pub(crate) fn hmac(key: &[u8; 32], bytes: &[u8]) -> [u8; 32] {
    let mut inner = Zeroizing::new([0x36; 64]);
    let mut outer = Zeroizing::new([0x5c; 64]);
    for i in 0..32 {
        inner[i] ^= key[i];
        outer[i] ^= key[i];
    }
    let mut hash = Sha256::new();
    hash.update(&*inner);
    hash.update(bytes);
    let digest = hash.finalize();
    let mut hash = Sha256::new();
    hash.update(&*outer);
    hash.update(digest);
    hash.finalize().into()
}

pub(crate) fn request_key(
    key: &super::keyring::KeyLease,
) -> crate::error::Result<Zeroizing<[u8; 32]>> {
    let mut domain = b"racer/request-mac/key/v1\0".to_vec();
    super::aead::field(&mut domain, key.cache().0.as_bytes())?;
    domain.extend_from_slice(&key.id().0);
    Ok(Zeroizing::new(hmac(
        key.material(super::keyring::KeyPurpose::OriginCredentials)?,
        &domain,
    )))
}

pub(crate) fn equal(a: &[u8], b: &[u8; 32]) -> bool {
    if a.len() != 32 {
        return false;
    }
    a.iter().zip(b).fold(0u8, |diff, (a, b)| diff | (a ^ b)) == 0
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn rfc4231_hmac_and_purpose_separation() {
        let mut key = [0; 32];
        key[..20].fill(0x0b);
        assert_eq!(
            hmac(&key, b"Hi There"),
            [
                0xb0, 0x34, 0x4c, 0x61, 0xd8, 0xdb, 0x38, 0x53, 0x5c, 0xa8, 0xaf, 0xce, 0xaf, 0x0b,
                0xf1, 0x2b, 0x88, 0x1d, 0xc2, 0x00, 0xc9, 0x83, 0x3d, 0xa7, 0x26, 0xe9, 0x37, 0x6c,
                0x2e, 0x32, 0xcf, 0xf7
            ]
        );
        let keys = super::super::keyring::tests::keys();
        let cache = crate::model::identity::CacheId(super::super::identity::tests::CACHE.into());
        assert!(
            request_key(
                &keys
                    .active(&cache, super::super::keyring::KeyPurpose::Page)
                    .unwrap()
            )
            .is_err()
        );
        let credential = keys
            .active(&cache, super::super::keyring::KeyPurpose::OriginCredentials)
            .unwrap();
        assert_ne!(
            &*request_key(&credential).unwrap(),
            credential
                .material(super::super::keyring::KeyPurpose::OriginCredentials)
                .unwrap()
        );
    }
}
