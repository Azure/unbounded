//! Cross-component ownership: real control decoding, application publication,
//! immutable identity leases, and completion-retained page crypto work.
use racer_control_wire as wire;
use racer_control_wire::*;
use racer_dataplane::admission::AdmissionPolicy;
use racer_dataplane::admission::ResourceClass;
use racer_dataplane::config::Config;
use racer_dataplane::control::BundleInstaller;
use racer_dataplane::error::Error;
use racer_dataplane::memory::BufferPool;
use racer_dataplane::model::CacheKey;
use racer_dataplane::model::ObjectId;
use racer_dataplane::model::ObjectVersion;
use racer_dataplane::model::PageId;
use racer_dataplane::model::PageNumber;
use racer_dataplane::model::RequestId;
use racer_dataplane::model::StrongEtag;
use racer_dataplane::model::WorkerId;
use racer_dataplane::runtime::RequestScope;
use racer_dataplane::security;
use racer_dataplane::security::CryptoClient;
use racer_dataplane::security::CryptoInput;
use racer_dataplane::security::CryptoOutput;
use racer_dataplane::worker::CryptoRuntime;

use racer_dataplane::security::PageCryptoEngine;
use racer_identity::KeyEpochs;
use racer_identity::KeyPurpose;
use racer_identity::Keyring;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::reactor::IoBuffer;

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const NODE: &str = "22222222-2222-4222-8222-222222222222";
const CACHE: &str = "33333333-3333-4333-8333-333333333333";

fn fixture() -> (Rc<Keyring>, BundleInstaller, KeyringBundle) {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = params.self_signed(&ca_key).unwrap();
    let keys = Rc::new(Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(NODE.into()),
        Arc::new(KeyEpochs::default()),
    ));
    let installer = BundleInstaller::new(keys.clone());
    let bundle = KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        generation: BundleGeneration(1),
        peer_trust_roots: vec![ca.der().to_vec()],
        cache_keys: vec![record(1, 7)],
    };
    install_decoded(&installer, &bundle).unwrap();
    (keys, installer, bundle)
}
fn record(generation: u64, byte: u8) -> CacheEncryptionKey {
    CacheEncryptionKey::new(
        CacheKeyRef {
            cache: CacheId(CACHE.into()),
            id: KeyId::from_generation(generation, 1).unwrap(),
            purpose: CacheKeyPurpose::Page,
        },
        CacheKeyState::Active,
        zeroize::Zeroizing::new([byte; 32]),
    )
}
fn install_decoded(
    installer: &BundleInstaller,
    bundle: &KeyringBundle,
) -> racer_dataplane::error::Result<(BundleGeneration, Vec<Vec<u8>>)> {
    let encoded = zeroize::Zeroizing::new(wire::encode_bundle(bundle).unwrap());
    installer.install(wire::decode_bundle(&encoded).unwrap())
}

#[test]
fn real_decode_installer_rotation_retained_lease_and_rejection_are_atomic() {
    let (keys, installer, mut bundle) = fixture();
    let cache = CacheId(CACHE.into());
    let held = keys.active(&cache, KeyPurpose::Page).unwrap();
    let mut sealed = [0; 19];
    held.seal_page(&cache, &[1; 24], b"aad", b"abc", &mut sealed)
        .unwrap();
    bundle.generation = BundleGeneration(2);
    bundle.cache_keys = vec![record(2, 9)];
    install_decoded(&installer, &bundle).unwrap();
    assert_eq!(installer.generation(), Some(BundleGeneration(2)));
    assert_eq!(keys.generation().unwrap(), Some(2));
    assert!(
        keys.lease(Some(&cache), held.id(), KeyPurpose::Page)
            .is_err()
    );
    let mut opened = [0; 3];
    held.open_page(&cache, held.id(), &[1; 24], b"aad", &sealed, &mut opened)
        .unwrap();
    assert_eq!(&opened, b"abc");
    let active_id = keys.active(&cache, KeyPurpose::Page).unwrap().id();
    let roots = keys.peer_trust_roots().unwrap();
    // This passes wire validation but illegally rebinds an admitted ID.
    bundle.generation = BundleGeneration(3);
    bundle.cache_keys = vec![record(2, 10)];
    let (_, _, foreign) = fixture();
    bundle.peer_trust_roots = foreign.peer_trust_roots;
    assert!(matches!(
        install_decoded(&installer, &bundle),
        Err(Error::InvalidConfiguration)
    ));
    assert_eq!(installer.generation(), Some(BundleGeneration(2)));
    assert_eq!(keys.generation().unwrap(), Some(2));
    assert_eq!(
        keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        active_id
    );
    assert!(Arc::ptr_eq(&roots, &keys.peer_trust_roots().unwrap()));
    // Direct configuration installation rejects malformed generations in identity,
    // independently of the wire decoder's InvalidRequest classification.
    bundle.cache_keys[0].key.id = KeyId([0; 16]);
    assert_eq!(
        keys.install(bundle),
        Err(racer_identity::Error::InvalidConfiguration)
    );
    assert_eq!(
        racer_dataplane::model::key_id_from_generation(0, 1),
        Err(Error::InvalidConfiguration)
    );
}

#[test]
fn active_crypto_operation_completes_after_rotation_with_its_original_key_lease() {
    let (keys, installer, mut bundle) = fixture();
    let cache = CacheId(CACHE.into());
    let config = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some(CLUSTER.into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://controller.invalid:443".into()),
            _ => None,
        })
    })
    .unwrap();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits,
    )));
    let pool = BufferPool::new(admission.clone());
    let (io, engine) = security::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(8).unwrap());
    let client = CryptoClient::new(io);
    let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
    let mut plaintext = pool
        .plaintext(
            admission
                .reserve(Some(&cache), ResourceClass::Plaintext, 3)
                .unwrap(),
            3,
        )
        .unwrap();
    plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: cache.clone(),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"held\"").unwrap(),
        },
        number: PageNumber(0),
    };
    let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
    let old_id = lease.id();
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10)).unwrap();
    let mut operation = client.execute(
        CryptoInput::Encrypt {
            page,
            plaintext,
            ciphertext: admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                .unwrap(),
        },
        lease,
        &scope,
    );
    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    bundle.generation = BundleGeneration(2);
    bundle.cache_keys = vec![record(2, 10)];
    install_decoded(&installer, &bundle).unwrap();
    assert!(keys.lease(Some(&cache), old_id, KeyPurpose::Page).is_err());
    uring_runtime::group::Service::poll_budgeted(
        &mut engine,
        &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        8,
    )
    .unwrap();
    client.poll_budgeted(8).unwrap();
    let std::task::Poll::Ready(Ok(CryptoOutput::Encrypted(verified, ciphertext))) =
        operation.as_mut().poll(&mut cx)
    else {
        panic!("held operation did not complete");
    };
    assert_eq!(verified.bytes(), b"abc");
    assert_eq!(ciphertext.envelope().key_id, old_id);
    // Public purpose operation, not a private weak-owner escape, checks the bytes.
    let mut opened = [0; 3];
    racer_crypto::open(
        &[7; 32],
        &ciphertext.envelope().nonce.0,
        &racer_dataplane::security::page_aad(ciphertext.envelope()).unwrap(),
        ciphertext.bytes(),
        &mut opened,
    )
    .unwrap();
    assert_eq!(&opened, b"abc");
    drop(operation);
}
