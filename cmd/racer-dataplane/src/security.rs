//! Verified identities, signature chains, and separate page/credential AEAD domains.
pub mod aead;
pub mod connection;
pub mod crc64 {
    //! CRC-64/XZ record-v4 integrity, never authentication.
    pub fn checksum(bytes: &[u8]) -> u64 {
        let mut digest = crc64fast::Digest::new();
        digest.write(bytes);
        digest.sum64()
    }
    #[cfg(test)]
    mod tests {
        use super::*;
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
            assert_eq!(
                checksum(bytes),
                expected,
                "dispatch, length={}",
                bytes.len()
            );
            let mut table = crc64fast::Digest::new_table();
            table.write(bytes);
            assert_eq!(table.sum64(), expected, "table, length={}", bytes.len());
        }
        #[test]
        fn ecma_golden_and_hardware_equivalence() {
            for (input, expected) in [(b"".as_slice(), 0), (b"123456789", 0x995d_c9bb_df19_39fa)] {
                assert_eq!(reference(input), expected);
                assert_eq!(checksum(input), expected);
            }
            let bytes = random_bytes(65537 + 32);
            for offset in 0..32 {
                for length in [
                    0, 1, 7, 8, 9, 15, 16, 17, 31, 32, 63, 64, 127, 128, 129, 143, 144, 255, 256,
                    257, 1023, 1024, 1025, 4095, 4096, 4097, 65535, 65536, 65537,
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
        fn full_page_and_tag_match_independent_reference() {
            let length = crate::model::PAGE_BYTES as usize + 16;
            let bytes = random_bytes(length + 1);
            assert_equivalent(&bytes[1..]);
            let mut digest = crc64fast::Digest::new();
            for chunk in bytes[1..].chunks(4093) {
                digest.write(chunk);
            }
            assert_eq!(digest.sum64(), checksum(&bytes[1..]));
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
    }
}
pub mod credentials;
pub mod forwarding;
pub mod identity;
pub mod protocol;
#[cfg(test)]
pub(crate) mod test_support {
    use super::{
        connection::Signatures,
        identity::{Certificates, KeyEpochs, Keyring, PendingIdentity},
    };
    use crate::{
        control::wire::{BundleGeneration, CacheEncryptionKey, KeyringBundle, SCHEMA_VERSION},
        model::{ClusterId, NodeId},
    };
    use std::{rc::Rc, sync::Arc};
    pub struct Identity {
        pub keys: Rc<Keyring>,
        pub certificates: Rc<Certificates>,
        pub signatures: Rc<Signatures>,
    }
    pub(crate) fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        (params.self_signed(&key).unwrap(), key)
    }
    pub(crate) fn issue(
        ca: &rcgen::Certificate,
        ca_key: &rcgen::KeyPair,
        cluster: &ClusterId,
        node: &NodeId,
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> (PendingIdentity, Vec<Vec<u8>>) {
        let pending = PendingIdentity::generate().unwrap();
        let secret = pending.export_pkcs8_for_persistence().unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(secret.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{}/node/{}", cluster.0, node.0)
                .try_into()
                .unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        customize(&mut params);
        let cert = params.signed_by(&key, ca, ca_key).unwrap();
        (pending, vec![cert.der().to_vec()])
    }
    pub fn identities(
        cluster: ClusterId,
        nodes: &[NodeId],
        cache_keys: impl Fn() -> Vec<CacheEncryptionKey>,
    ) -> Vec<Identity> {
        let (ca, ca_key) = ca();
        let roots = vec![ca.der().to_vec()];
        nodes
            .iter()
            .map(|node| {
                let (pending, chain) = issue(&ca, &ca_key, &cluster, node, |_| {});
                let identity = pending
                    .accept(cluster.clone(), node.clone(), chain, &roots)
                    .unwrap();
                let keys = Rc::new(Keyring::new(
                    cluster.clone(),
                    node.clone(),
                    Arc::new(KeyEpochs::default()),
                ));
                keys.install(KeyringBundle {
                    schema_version: SCHEMA_VERSION,
                    cluster: cluster.clone(),
                    generation: BundleGeneration(1),
                    peer_trust_roots: roots.clone(),
                    cache_keys: cache_keys(),
                })
                .unwrap();
                keys.install_signing_identity(Arc::new(identity)).unwrap();
                let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
                let signatures = Rc::new(Signatures::new(keys.clone(), certificates.clone()));
                Identity {
                    keys,
                    certificates,
                    signatures,
                }
            })
            .collect()
    }
}

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

/// Request-only derivation from a credential epoch, never an AEAD or page key.
pub(crate) fn request_key(key: &identity::KeyLease) -> crate::error::Result<Zeroizing<[u8; 32]>> {
    let mut domain = b"racer/request-mac/key/v1\0".to_vec();
    aead::field(&mut domain, key.cache().0.as_bytes())?;
    domain.extend_from_slice(&key.id().0);
    Ok(Zeroizing::new(hmac(
        key.material(identity::KeyPurpose::OriginCredentials)?,
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
        let keys = identity::keyring_tests::keys();
        let cache = crate::model::CacheId(identity::tests::CACHE.into());
        assert!(request_key(&keys.active(&cache, identity::KeyPurpose::Page).unwrap()).is_err());
        let credential = keys
            .active(&cache, identity::KeyPurpose::OriginCredentials)
            .unwrap();
        assert_ne!(
            &*request_key(&credential).unwrap(),
            credential
                .material(identity::KeyPurpose::OriginCredentials)
                .unwrap()
        );
    }
}
