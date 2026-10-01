//! WebPKI certificate path validation plus exact SPIFFE and Ed25519 binding.
use super::keyring::Keyring;
use crate::{
    error::{Error, Result},
    model::{ClusterId, NodeId},
};
use ed25519_dalek::{Signature, VerifyingKey};
use rustls::{RootCertStore, pki_types::CertificateDer};
use std::{cell::RefCell, collections::VecDeque, rc::Rc, sync::Arc};
use x509_parser::{extensions::GeneralName, parse_x509_certificate};

pub struct Certificates {
    cluster: ClusterId,
    keys: Rc<Keyring>,
    cache: RefCell<VecDeque<CachedPeer>>,
}
struct CachedPeer {
    roots: Arc<Vec<Vec<u8>>>,
    chain: Vec<Vec<u8>>,
    node: NodeId,
    key: VerifyingKey,
    until: u64,
    checked: u64,
}
/// Conservative validity intersection includes all configured trust anchors.
pub(crate) fn validity<'a>(certificates: impl Iterator<Item = &'a Vec<u8>>) -> Result<(u64, u64)> {
    let mut start = 0;
    let mut end = u64::MAX;
    for der in certificates {
        let (_, cert) = parse_x509_certificate(der).map_err(|_| Error::Unauthorized)?;
        start = start.max(cert.validity().not_before.timestamp().max(0) as u64);
        end = end.min(
            u64::try_from(cert.validity().not_after.timestamp())
                .map_err(|_| Error::Unauthorized)?,
        );
    }
    Ok((start, end))
}
pub struct VerifiedPeer {
    node: NodeId,
}
impl VerifiedPeer {
    pub fn node(&self) -> &NodeId {
        &self.node
    }
}
pub(crate) fn root_store(roots: &[Vec<u8>]) -> Result<RootCertStore> {
    if roots.is_empty() || roots.len() > 32 {
        return Err(Error::Unauthorized);
    }
    let mut store = RootCertStore::empty();
    for root in roots {
        if root.is_empty() || root.len() > 16384 {
            return Err(Error::Unauthorized);
        }
        let (rest, cert) = parse_x509_certificate(root).map_err(|_| Error::Unauthorized)?;
        let now = crate::runtime::environment::unix_time().as_secs();
        let now = i64::try_from(now).map_err(|_| Error::Unauthorized)?;
        let now =
            x509_parser::time::ASN1Time::from_timestamp(now).map_err(|_| Error::Unauthorized)?;
        if !rest.is_empty() || !cert.is_ca() || !cert.validity().is_valid_at(now) {
            return Err(Error::Unauthorized);
        }
        store
            .add(CertificateDer::from(root.clone()))
            .map_err(|_| Error::Unauthorized)?;
    }
    Ok(store)
}
#[cfg(test)]
mod tests {
    use super::super::identity::tests::{CLUSTER, NODE};
    use super::*;
    #[test]
    fn cache_requires_exact_chain_current_trust_time_and_fresh_signature() {
        let clock = crate::runtime::environment::SimulationClock::new(93);
        let _environment = clock.environment(0).enter();
        let (pending, chain, roots) = super::super::identity::tests::issued();
        let keys = Rc::new(Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(super::super::keyring::KeyEpochs::default()),
        ));
        let bundle = |generation, roots| crate::control::wire::KeyringBundle {
            schema_version: 1,
            cluster: ClusterId(CLUSTER.into()),
            generation: crate::control::wire::BundleGeneration(generation),
            peer_trust_roots: roots,
            cache_keys: vec![],
        };
        keys.install(bundle(1, roots.clone())).unwrap();
        let identity = pending
            .accept(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                chain.clone(),
                &roots,
            )
            .unwrap();
        let certs = Certificates::new(ClusterId(CLUSTER.into()), keys.clone());
        let node = NodeId(NODE.into());
        let signature = identity.sign(b"message").unwrap();
        certs
            .verify_signed(&chain, &node, b"message", &signature)
            .unwrap();
        certs
            .verify_signed(&chain, &node, b"message", &signature)
            .unwrap();
        assert!(
            certs
                .verify_signed(&chain, &node, b"tampered", &signature)
                .is_err()
        );
        let mut changed = chain.clone();
        changed[0].push(0);
        assert!(certs.verify(&changed, &node).is_err());
        // Exercise expiration through the clock, not the cache implementation.
        let original = crate::runtime::environment::wall_now();
        let (_, until) = validity(chain.iter().chain(roots.iter())).unwrap();
        clock.set_wall_time(
            std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(until + 1),
        );
        assert!(certs.verify(&chain, &node).is_err());
        clock.set_wall_time(original);
        certs.verify(&chain, &node).unwrap();
        let (_, _, replacement) = super::super::identity::tests::issued();
        keys.install(bundle(2, replacement)).unwrap();
        assert!(certs.verify(&chain, &node).is_err());
    }
    #[test]
    fn validates_chain_node_and_strict_signature() {
        let (pending, chain, roots) = super::super::identity::tests::issued();
        let cluster = ClusterId(CLUSTER.into());
        let node = NodeId(NODE.into());
        let key = verify_chain(&roots, &chain, &cluster, &node).unwrap();
        let identity = pending
            .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
            .unwrap();
        let signature = Signature::from_slice(&identity.sign(b"message").unwrap()).unwrap();
        assert!(key.verify_strict(b"message", &signature).is_ok());
        assert!(key.verify_strict(b"changed", &signature).is_err());
        assert!(verify_chain(&roots, &chain, &cluster, &NodeId("other".into())).is_err());
        assert!(verify_chain(&roots, &chain, &ClusterId("other".into()), &node).is_err());
        let (_, _, foreign_roots) = super::super::identity::tests::issued();
        assert!(verify_chain(&foreign_roots, &chain, &cluster, &node).is_err());
        let mut bad = chain.clone();
        bad[0].push(0);
        assert!(verify_chain(&roots, &bad, &cluster, &node).is_err());
        assert!(verify_chain(&roots, &vec![chain[0].clone(); 9], &cluster, &node).is_err());
    }
    #[test]
    fn rejects_missing_usage_ca_expiration_and_ambiguous_identity() {
        for case in 0..8 {
            let (_, chain, roots) =
                super::super::identity::tests::issued_with(|params| match case {
                    0 => params.key_usages.clear(),
                    1 => params.key_usages = vec![rcgen::KeyUsagePurpose::KeyEncipherment],
                    2 => params.extended_key_usages.clear(),
                    3 => {
                        params.extended_key_usages =
                            vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth]
                    }
                    4 => params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained),
                    5 => {
                        params.not_before = rcgen::date_time_ymd(2000, 1, 1);
                        params.not_after = rcgen::date_time_ymd(2001, 1, 1);
                    }
                    6 => params
                        .subject_alt_names
                        .push(params.subject_alt_names[0].clone()),
                    _ => {
                        params.subject_alt_names = vec![rcgen::SanType::URI(
                            format!("spiffe://{CLUSTER}/node/{NODE}/extra")
                                .try_into()
                                .unwrap(),
                        )]
                    }
                });
            assert!(
                verify_chain(
                    &roots,
                    &chain,
                    &ClusterId(CLUSTER.into()),
                    &NodeId(NODE.into())
                )
                .is_err(),
                "case {case}"
            );
        }
    }
    #[test]
    fn canonical_identity_rejects_aliases() {
        assert!(canonical_uuid(CLUSTER));
        for value in [
            "abc",
            "AAAAAAAA-1111-4111-8111-111111111111",
            "11111111111141118111111111111111",
            "11111111-1111-4111-8111-11111111111/",
        ] {
            assert!(!canonical_uuid(value));
        }
    }
}
pub(crate) fn spiffe(cluster: &ClusterId, node: &NodeId) -> Result<String> {
    if !canonical_uuid(&cluster.0) || !canonical_uuid(&node.0) {
        return Err(Error::Unauthorized);
    }
    Ok(format!("spiffe://{}/node/{}", cluster.0, node.0))
}
pub(crate) fn canonical_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}
pub(crate) fn verify_chain(
    roots: &[Vec<u8>],
    chain: &[Vec<u8>],
    cluster: &ClusterId,
    node: &NodeId,
) -> Result<VerifyingKey> {
    if chain.is_empty()
        || chain.len() > 8
        || chain.iter().any(|c| c.is_empty() || c.len() > 16384)
        || chain.iter().map(Vec::len).sum::<usize>() > 65536
    {
        return Err(Error::Unauthorized);
    }
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(root_store(roots)?),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .build()
    .map_err(|_| Error::Unauthorized)?;
    let leaf = CertificateDer::from(chain[0].as_slice());
    let intermediates: Vec<_> = chain[1..]
        .iter()
        .map(|c| CertificateDer::from(c.as_slice()))
        .collect();
    verifier
        .verify_client_cert(
            &leaf,
            &intermediates,
            crate::runtime::environment::unix_time(),
        )
        .map_err(|_| Error::Unauthorized)?;
    let (rest, cert) = parse_x509_certificate(&chain[0]).map_err(|_| Error::Unauthorized)?;
    if !rest.is_empty()
        || cert.is_ca()
        || cert.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112"
        || cert.public_key().algorithm.parameters.is_some()
    {
        return Err(Error::Unauthorized);
    }
    let san = cert
        .subject_alternative_name()
        .map_err(|_| Error::Unauthorized)?
        .ok_or(Error::Unauthorized)?;
    let usage = cert
        .key_usage()
        .map_err(|_| Error::Unauthorized)?
        .ok_or(Error::Unauthorized)?;
    let extended = cert
        .extended_key_usage()
        .map_err(|_| Error::Unauthorized)?
        .ok_or(Error::Unauthorized)?;
    if !usage.value.digital_signature()
        || usage.value.key_cert_sign()
        || !extended.value.client_auth
    {
        return Err(Error::Unauthorized);
    }
    let expected = spiffe(cluster, node)?;
    let uris: Vec<_> = san
        .value
        .general_names
        .iter()
        .filter_map(|n| {
            if let GeneralName::URI(s) = n {
                Some(*s)
            } else {
                None
            }
        })
        .collect();
    if uris != [expected.as_str()] {
        return Err(Error::Unauthorized);
    }
    let key: &[u8; 32] = cert
        .public_key()
        .subject_public_key
        .data
        .as_ref()
        .try_into()
        .map_err(|_| Error::Unauthorized)?;
    VerifyingKey::from_bytes(key).map_err(|_| Error::Unauthorized)
}
impl Certificates {
    pub fn new(cluster: ClusterId, keys: Rc<Keyring>) -> Self {
        Self {
            cluster,
            keys,
            cache: RefCell::new(VecDeque::new()),
        }
    }
    fn key(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifyingKey> {
        let roots = self.keys.peer_trust_roots()?;
        let now = crate::runtime::environment::unix_time().as_secs();
        let mut cache = self.cache.borrow_mut();
        cache.retain(|entry| {
            Arc::ptr_eq(&entry.roots, &roots) && now >= entry.checked && now < entry.until
        });
        if let Some(entry) = cache
            .iter()
            .find(|entry| &entry.node == expected && entry.chain == chain)
        {
            return Ok(entry.key);
        }
        drop(cache);
        let key = verify_chain(&roots, chain, &self.cluster, expected)?;
        let (_, until) = validity(chain.iter().chain(roots.iter()))?;
        let mut cache = self.cache.borrow_mut();
        // At most 64 exact, wire-bounded chains (4 MiB of chain bytes).
        if cache.len() == 64 {
            cache.pop_front();
        }
        cache.push_back(CachedPeer {
            roots,
            chain: chain.to_vec(),
            node: expected.clone(),
            key,
            until,
            checked: now,
        });
        Ok(key)
    }
    pub fn verify(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifiedPeer> {
        if &self.cluster != self.keys.cluster() {
            return Err(Error::Unauthorized);
        }
        self.key(chain, expected)?;
        Ok(VerifiedPeer {
            node: expected.clone(),
        })
    }
    pub fn verify_signed(
        &self,
        chain: &[Vec<u8>],
        expected: &NodeId,
        message: &[u8],
        signature: &[u8],
    ) -> Result<VerifiedPeer> {
        if &self.cluster != self.keys.cluster() {
            return Err(Error::Unauthorized);
        }
        let key = self.key(chain, expected)?;
        let signature = Signature::from_slice(signature).map_err(|_| Error::Unauthorized)?;
        key.verify_strict(message, &signature)
            .map_err(|_| Error::Unauthorized)?;
        Ok(VerifiedPeer {
            node: expected.clone(),
        })
    }
}
