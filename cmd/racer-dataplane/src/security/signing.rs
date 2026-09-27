//! HTTP Message Signatures over canonical headers and method/target or status.
//!
//! Cover identities, range, lengths, TTL, membership, freshness, metadata, and
//! encrypted Authorization. Never hash/sign page bodies. Reject duplicate fields.
use super::{
    certificates::{Certificates, VerifiedPeer},
    keyring::Keyring,
    protocol::{self, field, number, push, push_binary},
};
use crate::{
    error::{Error, Result},
    http::codec::{Codec, MessageHead, StartLine},
    model::identity::NodeId,
};
use sha2::{Digest, Sha256};
#[cfg(test)]
use std::time::SystemTime;
use std::{
    collections::BTreeSet,
    rc::Rc,
    time::{Duration, UNIX_EPOCH},
};
pub struct Signatures {
    keys: Rc<Keyring>,
    certificates: Rc<Certificates>,
}
pub struct SignedHead {
    pub head: MessageHead,
    pub signature: Vec<u8>,
}
pub struct VerifiedHead {
    pub(crate) signed: SignedHead,
    pub(crate) peer: VerifiedPeer,
}
impl Signatures {
    pub fn new(keys: Rc<Keyring>, certificates: Rc<Certificates>) -> Self {
        Self { keys, certificates }
    }
    pub fn node(&self) -> &NodeId {
        self.keys.node()
    }
    /// Sign retained provenance. Replay admission belongs exclusively to the
    /// immediate-hop head signed by the owning connection session.
    pub fn sign(&self, head: MessageHead) -> Result<SignedHead> {
        if head.headers.iter().any(|h| {
            is_auth_field(&h.name.to_ascii_lowercase())
                && !h.name.eq_ignore_ascii_case("racer-receiver")
        }) {
            return Err(Error::InvalidRequest);
        }
        receiver(&head)?;
        let signed = self.sign_fields(head)?;
        Codec::new(protocol::MAX_HEAD, u64::MAX).encode_head(&signed.head)?;
        Ok(signed)
    }
    pub(crate) fn sign_fields(&self, mut head: MessageHead) -> Result<SignedHead> {
        let identity = self.keys.signing_identity()?;
        if identity.node() != self.node() {
            return Err(Error::Unauthorized);
        }
        push(&mut head, "racer-profile", protocol::PROFILE);
        protocol::uuid(&self.keys.cluster().0)?;
        protocol::uuid(&self.node().0)?;
        push(&mut head, "racer-cluster", &self.keys.cluster().0);
        push(&mut head, "racer-signer", &self.node().0);
        push_binary(
            &mut head,
            "racer-certificates",
            &encode_chain(identity.certificate_chain())?,
        );
        push(
            &mut head,
            "racer-timestamp",
            protocol::millis(crate::runtime::environment::wall_now())?,
        );
        let input = signature_input(&head)?;
        push(&mut head, "signature-input", format!("racer={input}"));
        let signature = identity.sign(&signature_base(&head)?)?;
        if signature.len() != 64 {
            return Err(Error::Unauthorized);
        }
        push(
            &mut head,
            "signature",
            format!("racer=:{}:", protocol::binary(&signature)),
        );
        Codec::new(crate::peer::wire::MAX_ENVELOPE_HEAD, u64::MAX).encode_head(&head)?;
        Ok(SignedHead { head, signature })
    }
    /// Verify identity and retained provenance, without replay admission. Network
    /// callers must first admit the carrying immediate-hop head on its socket.
    pub fn verify_proof(&self, head: SignedHead) -> Result<VerifiedHead> {
        let peer = self.verify_historical(&head)?;
        if receiver(&head.head)? != *self.node() {
            return Err(Error::Unauthorized);
        }
        Ok(VerifiedHead { signed: head, peer })
    }
    /// Cryptographically validate an original or historical hop, without replay
    /// admission at this node. The owning connection admits the fresh outer head;
    /// forwarding then validates the complete retained chain and logical fields.
    pub(crate) fn verify_historical(&self, signed: &SignedHead) -> Result<VerifiedPeer> {
        let head = &signed.head;
        if field(head, "racer-profile")? != protocol::PROFILE
            || field(head, "racer-cluster")? != self.keys.cluster().0
        {
            return Err(Error::Unauthorized);
        }
        protocol::uuid(&self.keys.cluster().0)?;
        let base = signature_base(head)?;
        if signed.signature.len() != 64
            || field(head, "signature")?
                != format!("racer=:{}:", protocol::binary(&signed.signature))
        {
            return Err(Error::Unauthorized);
        }
        let signer = node_field(head, "racer-signer")?;
        let chain = decode_chain(&protocol::decode_binary(
            field(head, "racer-certificates")?.as_bytes(),
        )?)?;
        let timestamp = UNIX_EPOCH
            .checked_add(Duration::from_millis(number(head, "racer-timestamp")?))
            .ok_or(Error::Unauthorized)?;
        let now = crate::runtime::environment::wall_now();
        if timestamp
            > now
                .checked_add(Duration::from_secs(5))
                .ok_or(Error::Unauthorized)?
            || now
                .duration_since(timestamp)
                .is_ok_and(|age| age >= Duration::from_secs(60))
        {
            return Err(Error::Replay);
        }
        self.certificates
            .verify_signed(&chain, &signer, &base, &signed.signature)
    }
}
pub(crate) fn is_auth_field(name: &str) -> bool {
    matches!(
        name,
        "signature-input"
            | "signature"
            | "racer-profile"
            | "racer-cluster"
            | "racer-signer"
            | "racer-receiver"
            | "racer-certificates"
            | "racer-session"
            | "racer-direction"
            | "racer-sequence"
            | "racer-timestamp"
    )
}
pub fn node_field(head: &MessageHead, name: &str) -> Result<NodeId> {
    let node = field(head, name)?;
    protocol::uuid(&node)?;
    Ok(NodeId(node))
}
pub fn receiver(head: &MessageHead) -> Result<NodeId> {
    node_field(head, "racer-receiver")
}
fn components(head: &MessageHead) -> Result<Vec<String>> {
    Codec::new(crate::peer::wire::MAX_ENVELOPE_HEAD, u64::MAX).encode_head(head)?;
    let mut names = BTreeSet::new();
    for h in &head.headers {
        let name = h.name.to_ascii_lowercase();
        if !names.insert(name)
            || h.value.first().is_some_and(|b| b.is_ascii_whitespace())
            || h.value.last().is_some_and(|b| b.is_ascii_whitespace())
            || !h.value.is_ascii()
        {
            return Err(Error::Unauthorized);
        }
    }
    names.remove("signature");
    names.remove("signature-input");
    let mut components = match head.start {
        StartLine::Request { .. } => vec!["@method".into(), "@request-target".into()],
        StartLine::Response { .. } => vec!["@status".into()],
    };
    components.extend(names);
    Ok(components)
}
fn signature_input(head: &MessageHead) -> Result<String> {
    let components = components(head)?
        .iter()
        .map(|s| format!("\"{s}\""))
        .collect::<Vec<_>>()
        .join(" ");
    let timestamp = number(head, "racer-timestamp")? / 1000;
    let keyid = field(head, "racer-signer")?;
    protocol::uuid(&keyid)?;
    Ok(format!(
        "({components});created={timestamp};keyid=\"{keyid}\";alg=\"ed25519\";tag=\"racer-peer-v2\""
    ))
}
/// RFC 9421 section 2.5 signature base, using the strict Racer profile. The
/// verifier accepts only this canonical structured-field serialization, avoiding
/// duplicate labels, unsupported parameters and alternate parsing ambiguity.
pub fn signature_base(head: &MessageHead) -> Result<Vec<u8>> {
    let input = signature_input(head)?;
    if field(head, "signature-input")? != format!("racer={input}") {
        return Err(Error::Unauthorized);
    }
    let mut lines = Vec::new();
    for name in components(head)? {
        let value = match (name.as_str(), &head.start) {
            ("@method", StartLine::Request { method, .. }) => method.clone(),
            ("@request-target", StartLine::Request { target, .. }) => target.clone(),
            ("@status", StartLine::Response { status }) => status.to_string(),
            // The complete outer head was bounded by components(). Its base64
            // provenance fields can exceed the inner logical field's 64 KiB cap.
            _ => String::from_utf8(head.unique(&name)?.ok_or(Error::Unauthorized)?.to_vec())
                .map_err(|_| Error::Unauthorized)?,
        };
        lines.push(format!("\"{name}\": {value}"));
    }
    lines.push(format!("\"@signature-params\": {input}"));
    Ok(lines.join("\n").into_bytes())
}
/// Domain-separated SHA-256 binding of the exact signature base and Ed25519
/// signature. Includes the signature itself, so a request ID is never sufficient.
pub fn signed_digest(head: &SignedHead) -> Result<[u8; 32]> {
    let base = signature_base(&head.head)?;
    let mut hash = Sha256::new();
    hash.update(b"racer-peer-v2/signed-head\0");
    hash.update((base.len() as u64).to_be_bytes());
    hash.update(base);
    hash.update((head.signature.len() as u64).to_be_bytes());
    hash.update(&head.signature);
    Ok(hash.finalize().into())
}
fn encode_chain(chain: &[Vec<u8>]) -> Result<Vec<u8>> {
    if chain.is_empty() || chain.len() > 8 {
        return Err(Error::Unauthorized);
    }
    let mut bytes = Vec::new();
    for cert in chain {
        if cert.is_empty() || cert.len() > 16384 {
            return Err(Error::Unauthorized);
        }
        bytes.extend_from_slice(&(cert.len() as u32).to_be_bytes());
        bytes.extend_from_slice(cert);
    }
    if bytes.len() > 65536 {
        return Err(Error::Unauthorized);
    }
    Ok(bytes)
}
fn decode_chain(mut bytes: &[u8]) -> Result<Vec<Vec<u8>>> {
    if bytes.len() > 65536 {
        return Err(Error::Unauthorized);
    }
    let mut chain = Vec::new();
    while !bytes.is_empty() {
        if bytes.len() < 4 || chain.len() == 8 {
            return Err(Error::Unauthorized);
        }
        let n =
            u32::from_be_bytes(bytes[..4].try_into().map_err(|_| Error::Unauthorized)?) as usize;
        bytes = &bytes[4..];
        if n == 0 || n > 16384 || bytes.len() < n {
            return Err(Error::Unauthorized);
        }
        chain.push(bytes[..n].to_vec());
        bytes = &bytes[n..];
    }
    if chain.is_empty() {
        return Err(Error::Unauthorized);
    }
    Ok(chain)
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::{
        control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION},
        model::identity::ClusterId,
        security::{identity::PendingIdentity, keyring::KeyEpochs},
    };
    use std::sync::Arc;
    pub(crate) fn node(n: usize) -> NodeId {
        NodeId(format!("{n:08x}-1111-4111-8111-111111111111"))
    }
    pub(crate) fn network(count: usize) -> Vec<Rc<Signatures>> {
        let cluster = ClusterId(node(99).0);
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let ca = params.self_signed(&ca_key).unwrap();
        let roots = vec![ca.der().to_vec()];
        let mut network = Vec::new();
        for i in 0..count {
            let pending = PendingIdentity::generate().unwrap();
            let secret = pending.export_pkcs8_for_persistence().unwrap();
            let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
                &rustls::pki_types::PrivatePkcs8KeyDer::from(secret.as_slice()),
                &rcgen::PKCS_ED25519,
            )
            .unwrap();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.subject_alt_names = vec![rcgen::SanType::URI(
                format!("spiffe://{}/node/{}", cluster.0, node(i).0)
                    .try_into()
                    .unwrap(),
            )];
            params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
            let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
            let identity = pending
                .accept(cluster.clone(), node(i), vec![cert.der().to_vec()], &roots)
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node(i),
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots.clone(),
                cache_keys: Vec::new(),
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certs = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
            network.push(Rc::new(Signatures::new(keys, certs)));
        }
        network
    }
    pub(crate) fn clone_head(head: &SignedHead) -> SignedHead {
        let codec = Codec::new(protocol::MAX_HEAD, u64::MAX);
        let encoded = codec.encode_head(&head.head).unwrap();
        SignedHead {
            head: codec.decode_head(&encoded).unwrap().unwrap().0,
            signature: head.signature.clone(),
        }
    }
    fn head(receiver: usize) -> MessageHead {
        let mut head = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1".into(),
            },
            headers: Vec::new(),
        };
        push(&mut head, "racer-receiver", node(receiver).0);
        push(&mut head, "content-length", 0);
        push(&mut head, "racer-kind", "test");
        head
    }
    #[test]
    fn rfc9421_exact_request_and_response_signature_base_vectors() {
        let mut request = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1?attempt=1".into(),
            },
            headers: Vec::new(),
        };
        // Deliberately unsorted input: canonical components are sorted by name.
        push(&mut request, "racer-timestamp", "1700000000123");
        push(&mut request, "racer-signer", node(0).0);
        push(&mut request, "content-length", "0");
        let params = "(\"@method\" \"@request-target\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v2\"";
        push(&mut request, "signature-input", format!("racer={params}"));
        assert_eq!(signature_base(&request).unwrap(), format!(
            "\"@method\": POST\n\"@request-target\": /racer/peer/v1?attempt=1\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}"
        ).as_bytes());
        request.start = StartLine::Response { status: 200 };
        request.headers.retain(|h| h.name != "signature-input");
        let params = "(\"@status\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v2\"";
        push(&mut request, "signature-input", format!("racer={params}"));
        assert_eq!(signature_base(&request).unwrap(), format!(
            "\"@status\": 200\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}"
        ).as_bytes());
    }
    #[test]
    fn ed25519_rfc9421_base_tamper_replay_and_receiver_challenge() {
        let n = network(3);
        let original = n[0].sign(head(1)).unwrap();
        let base = String::from_utf8(signature_base(&original.head).unwrap()).unwrap();
        assert!(base.starts_with(
            "\"@method\": POST\n\"@request-target\": /racer/peer/v1\n\"content-length\": 0\n"
        ));
        assert!(base.ends_with(";alg=\"ed25519\";tag=\"racer-peer-v2\""));
        for h in &original.head.headers {
            let mut tamper = clone_head(&original);
            tamper
                .head
                .headers
                .iter_mut()
                .find(|v| v.name == h.name)
                .unwrap()
                .value
                .push(b'x');
            assert!(
                n[1].verify_historical(&tamper).is_err(),
                "accepted {} mutation",
                h.name
            );
        }
        let mut target = clone_head(&original);
        target.head.start = StartLine::Request {
            method: "GET".into(),
            target: "/racer/peer/v1".into(),
        };
        assert!(n[1].verify_historical(&target).is_err());
        assert!(n[2].verify_proof(clone_head(&original)).is_err());
        n[1].verify_proof(clone_head(&original)).unwrap();
        // Historical proofs are reusable; only fresh connection heads admit work.
        n[1].verify_proof(clone_head(&original)).unwrap();
        n[2].verify_historical(&original).unwrap();
        super::super::connection::tests::replay_and_binding_checks();
    }
    #[test]
    pub(crate) fn malformed_fields_unknown_algorithm_and_historical_expiry() {
        let n = network(2);
        let original = n[0].sign(head(1)).unwrap();
        let mut duplicate = clone_head(&original);
        push(&mut duplicate.head, "Racer-Kind", "test");
        assert!(n[1].verify_historical(&duplicate).is_err());
        let mut algorithm = clone_head(&original);
        for h in &mut algorithm.head.headers {
            if h.name == "signature-input" {
                h.value = String::from_utf8(h.value.clone())
                    .unwrap()
                    .replace("ed25519", "rsa-pss-sha512")
                    .into_bytes();
            }
        }
        assert!(n[1].verify_historical(&algorithm).is_err());
        for time in [
            SystemTime::now() - Duration::from_secs(61),
            SystemTime::now() + Duration::from_secs(6),
        ] {
            let mut stale = clone_head(&original);
            stale.head.headers.retain(|h| {
                h.name != "signature" && h.name != "signature-input" && h.name != "racer-timestamp"
            });
            push(
                &mut stale.head,
                "racer-timestamp",
                protocol::millis(time).unwrap(),
            );
            let input = signature_input(&stale.head).unwrap();
            push(&mut stale.head, "signature-input", format!("racer={input}"));
            stale.signature = n[0]
                .keys
                .signing_identity()
                .unwrap()
                .sign(&signature_base(&stale.head).unwrap())
                .unwrap();
            push(
                &mut stale.head,
                "signature",
                format!("racer=:{}:", protocol::binary(&stale.signature)),
            );
            assert!(matches!(n[1].verify_historical(&stale), Err(Error::Replay)));
        }
    }
}
