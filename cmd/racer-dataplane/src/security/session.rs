//! Certificate-authenticated receiver challenge discovery over plain peer HTTP.
//!
//! Discovery itself confers no read/origin/RDMA authority. A requester retains a
//! fresh random probe until it verifies the response against the expected Node
//! identity and current projected trust. Consuming the probe prevents reuse; its
//! monotonic expiry prevents delayed acceptance after a session restart.
use super::{certificates::Certificates, keyring::Keyring, replay::ReplayWindow};
use crate::{
    error::{Error, Result},
    model::identity::NodeId,
};
use std::time::{Duration, Instant};

const PROBE_DOMAIN: &[u8] = b"racer-peer-v1/challenge-probe\0";
const RESPONSE_DOMAIN: &[u8] = b"racer-peer-v1/challenge-response\0";
const PROBE_LIFETIME: Duration = Duration::from_secs(5);
const REPLY_DOMAIN: &[u8] = b"racer-peer-v1/challenge-reply\0";
pub const MAX_CHALLENGE_REPLY: usize = 64 * 1024;

/// Only the probe constructor may create an outstanding correlation challenge.
pub struct ChallengeProbe {
    local: NodeId,
    remote: NodeId,
    nonce: [u8; 32],
    expires: Instant,
}

/// Public wire data. The transport must bound certificate decoding to 64 KiB and
/// eight certificates before construction. `verify` repeats bounds before crypto.
pub struct ChallengeReply {
    pub challenge: [u8; 32],
    pub certificate_chain: Vec<Vec<u8>>,
    pub signature: Vec<u8>,
}

impl ChallengeReply {
    /// Bounded versioned binary response: domain, challenge, certificate count,
    /// u32-BE length-prefixed DER chain, and exactly one 64-byte Ed25519 signature.
    pub fn encode(&self) -> Result<Vec<u8>> {
        if self.certificate_chain.is_empty()
            || self.certificate_chain.len() > 8
            || self.signature.len() != 64
        {
            return Err(Error::InvalidRequest);
        }
        let mut size = REPLY_DOMAIN.len() + 32 + 1 + 64;
        for certificate in &self.certificate_chain {
            if certificate.is_empty() || certificate.len() > 16 * 1024 {
                return Err(Error::InvalidRequest);
            }
            size = size
                .checked_add(4 + certificate.len())
                .ok_or(Error::InvalidRequest)?;
        }
        if size > MAX_CHALLENGE_REPLY {
            return Err(Error::InvalidRequest);
        }
        let mut bytes = Vec::with_capacity(size);
        bytes.extend_from_slice(REPLY_DOMAIN);
        bytes.extend_from_slice(&self.challenge);
        bytes.push(self.certificate_chain.len() as u8);
        for certificate in &self.certificate_chain {
            bytes.extend_from_slice(&(certificate.len() as u32).to_be_bytes());
            bytes.extend_from_slice(certificate);
        }
        bytes.extend_from_slice(&self.signature);
        Ok(bytes)
    }

    pub fn decode(bytes: &[u8]) -> Result<Self> {
        if bytes.len() > MAX_CHALLENGE_REPLY || !bytes.starts_with(REPLY_DOMAIN) {
            return Err(Error::InvalidRequest);
        }
        let mut rest = &bytes[REPLY_DOMAIN.len()..];
        if rest.len() < 33 + 64 {
            return Err(Error::InvalidRequest);
        }
        let challenge = rest[..32].try_into().map_err(|_| Error::InvalidRequest)?;
        let count = usize::from(rest[32]);
        rest = &rest[33..];
        if count == 0 || count > 8 {
            return Err(Error::InvalidRequest);
        }
        let mut certificate_chain = Vec::with_capacity(count);
        for _ in 0..count {
            if rest.len() < 4 {
                return Err(Error::InvalidRequest);
            }
            let size = u32::from_be_bytes(rest[..4].try_into().map_err(|_| Error::InvalidRequest)?)
                as usize;
            rest = &rest[4..];
            if size == 0 || size > 16 * 1024 || rest.len() < size + 64 {
                return Err(Error::InvalidRequest);
            }
            certificate_chain.push(rest[..size].to_vec());
            rest = &rest[size..];
        }
        if rest.len() != 64 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            challenge,
            certificate_chain,
            signature: rest.to_vec(),
        })
    }
}

/// A capability minted only by verification of a fresh outstanding probe.
pub struct AuthenticatedChallenge {
    peer: NodeId,
    challenge: [u8; 32],
}
impl AuthenticatedChallenge {
    pub fn peer(&self) -> &NodeId {
        &self.peer
    }
    pub fn challenge(&self) -> [u8; 32] {
        self.challenge
    }
}

fn valid_node(node: &NodeId) -> bool {
    node.0.len() == 36
        && node.0.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}

impl ChallengeProbe {
    pub fn new(local: NodeId, remote: NodeId) -> Result<Self> {
        if !valid_node(&local) || !valid_node(&remote) {
            return Err(Error::InvalidRequest);
        }
        let mut nonce = [0; 32];
        getrandom::getrandom(&mut nonce).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            local,
            remote,
            nonce,
            expires: Instant::now() + PROBE_LIFETIME,
        })
    }

    /// Exactly domain + requester UUID + receiver UUID + random probe nonce.
    /// Transport may base64 this value; it must not invent a second encoding.
    pub fn request_bytes(&self) -> Vec<u8> {
        let mut bytes = Vec::with_capacity(PROBE_DOMAIN.len() + 104);
        bytes.extend_from_slice(PROBE_DOMAIN);
        bytes.extend_from_slice(self.local.0.as_bytes());
        bytes.extend_from_slice(self.remote.0.as_bytes());
        bytes.extend_from_slice(&self.nonce);
        bytes
    }

    pub fn verify(
        self,
        certificates: &Certificates,
        reply: ChallengeReply,
    ) -> Result<AuthenticatedChallenge> {
        if Instant::now() >= self.expires {
            return Err(Error::DeadlineExceeded);
        }
        let bytes = response_bytes(&self.request_bytes(), &reply.challenge);
        certificates.verify_signed(
            &reply.certificate_chain,
            &self.remote,
            &bytes,
            &reply.signature,
        )?;
        if Instant::now() >= self.expires {
            return Err(Error::DeadlineExceeded);
        }
        Ok(AuthenticatedChallenge {
            peer: self.remote,
            challenge: reply.challenge,
        })
    }
}

fn response_bytes(probe: &[u8], challenge: &[u8; 32]) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(RESPONSE_DOMAIN.len() + probe.len() + 32);
    bytes.extend_from_slice(RESPONSE_DOMAIN);
    bytes.extend_from_slice(probe);
    bytes.extend_from_slice(challenge);
    bytes
}

/// Answer a bounded challenge probe with the local signing identity. The transport
/// must apply its connection/handshake CPU admission before calling this method.
/// No receiver-side session allocation or nonce insertion occurs for a probe.
pub fn respond(keys: &Keyring, replay: &ReplayWindow, probe: &[u8]) -> Result<ChallengeReply> {
    if probe.len() != PROBE_DOMAIN.len() + 104 || !probe.starts_with(PROBE_DOMAIN) {
        return Err(Error::InvalidRequest);
    }
    let fields = &probe[PROBE_DOMAIN.len()..];
    let requester = NodeId(
        std::str::from_utf8(&fields[..36])
            .map_err(|_| Error::InvalidRequest)?
            .into(),
    );
    let receiver = NodeId(
        std::str::from_utf8(&fields[36..72])
            .map_err(|_| Error::InvalidRequest)?
            .into(),
    );
    if !valid_node(&requester) || !valid_node(&receiver) || &receiver != keys.node() {
        return Err(Error::Unauthorized);
    }
    let identity = keys.signing_identity()?;
    let challenge = replay.challenge()?;
    let signature = identity.sign(&response_bytes(probe, &challenge))?;
    Ok(ChallengeReply {
        challenge,
        certificate_chain: identity.certificate_chain().to_vec(),
        signature,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        control::wire::{BundleGeneration, KeyringBundle},
        model::identity::ClusterId,
        security::{identity::PendingIdentity, keyring::KeyEpochs, replay::ReplayState},
    };
    use std::{rc::Rc, sync::Arc};
    fn node(n: u8) -> NodeId {
        NodeId(format!("{n:08x}-1111-4111-8111-111111111111"))
    }
    #[test]
    fn probe_is_fresh_exact_and_binds_both_node_identities() {
        let first = ChallengeProbe::new(node(1), node(2)).unwrap();
        let second = ChallengeProbe::new(node(1), node(2)).unwrap();
        let wire = first.request_bytes();
        assert_eq!(wire.len(), PROBE_DOMAIN.len() + 104);
        assert_ne!(wire, second.request_bytes());
        assert_eq!(
            &wire[PROBE_DOMAIN.len()..PROBE_DOMAIN.len() + 36],
            node(1).0.as_bytes()
        );
        assert_eq!(
            &wire[PROBE_DOMAIN.len() + 36..PROBE_DOMAIN.len() + 72],
            node(2).0.as_bytes()
        );
        assert_ne!(
            response_bytes(&wire, &[1; 32]),
            response_bytes(&wire, &[2; 32])
        );
        assert!(ChallengeProbe::new(NodeId("invalid".into()), node(2)).is_err());
    }

    fn participants() -> (Rc<Keyring>, Rc<Keyring>, Certificates, ReplayWindow) {
        let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut params = rcgen::CertificateParams::default();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca = params.self_signed(&ca_key).unwrap();
        let root = ca.der().to_vec();
        let cluster = ClusterId("aaaaaaaa-1111-4111-8111-111111111111".into());
        let make = |id: NodeId| {
            let pending = PendingIdentity::generate().unwrap();
            let bytes = pending.export_pkcs8_for_persistence().unwrap();
            let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
                &rustls::pki_types::PrivatePkcs8KeyDer::from(bytes.as_slice()),
                &rcgen::PKCS_ED25519,
            )
            .unwrap();
            let mut leaf = rcgen::CertificateParams::default();
            leaf.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            leaf.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
            leaf.subject_alt_names = vec![rcgen::SanType::URI(
                format!("spiffe://{}/node/{}", cluster.0, id.0)
                    .try_into()
                    .unwrap(),
            )];
            let cert = leaf.signed_by(&key, &ca, &ca_key).unwrap();
            let identity = pending
                .accept(
                    cluster.clone(),
                    id.clone(),
                    vec![cert.der().to_vec()],
                    &[root.clone()],
                )
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                id,
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: 1,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: vec![root.clone()],
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            keys
        };
        let first = make(node(1));
        let second = make(node(2));
        let certificates = Certificates::new(cluster, first.clone());
        (
            first,
            second,
            certificates,
            ReplayWindow::new(Arc::new(ReplayState::default()), 32),
        )
    }

    #[test]
    fn certificate_verified_discovery_rejects_tampering_wrong_probe_and_wrong_receiver() {
        let (first, second, certificates, replay) = participants();
        let probe = ChallengeProbe::new(node(1), node(2)).unwrap();
        let reply = respond(&second, &replay, &probe.request_bytes()).unwrap();
        let wire = reply.encode().unwrap();
        let authenticated = probe
            .verify(&certificates, ChallengeReply::decode(&wire).unwrap())
            .unwrap();
        assert_eq!(authenticated.peer(), &node(2));
        assert_eq!(authenticated.challenge(), replay.challenge().unwrap());

        for tamper in 0..4 {
            let probe = ChallengeProbe::new(node(1), node(2)).unwrap();
            let mut reply = respond(&second, &replay, &probe.request_bytes()).unwrap();
            match tamper {
                0 => reply.challenge[0] ^= 1,
                1 => reply.signature[0] ^= 1,
                2 => {
                    reply.certificate_chain = first
                        .signing_identity()
                        .unwrap()
                        .certificate_chain()
                        .to_vec()
                }
                _ => {
                    reply = respond(
                        &second,
                        &replay,
                        &ChallengeProbe::new(node(1), node(2))
                            .unwrap()
                            .request_bytes(),
                    )
                    .unwrap()
                }
            }
            assert!(probe.verify(&certificates, reply).is_err());
        }
        let probe = ChallengeProbe::new(node(1), node(2)).unwrap();
        assert!(respond(&first, &replay, &probe.request_bytes()).is_err());
        let mut expired = ChallengeProbe::new(node(1), node(2)).unwrap();
        let reply = respond(&second, &replay, &expired.request_bytes()).unwrap();
        expired.expires = Instant::now();
        assert!(matches!(
            expired.verify(&certificates, reply),
            Err(Error::DeadlineExceeded)
        ));
    }

    #[test]
    fn reply_codec_rejects_truncation_overflow_extra_bytes_and_chain_bombs() {
        let (_, second, _, replay) = participants();
        let probe = ChallengeProbe::new(node(1), node(2)).unwrap();
        let wire = respond(&second, &replay, &probe.request_bytes())
            .unwrap()
            .encode()
            .unwrap();
        for length in 0..wire.len() {
            assert!(ChallengeReply::decode(&wire[..length]).is_err());
        }
        let mut bad = wire.clone();
        bad.push(0);
        assert!(ChallengeReply::decode(&bad).is_err());
        let mut bad = wire.clone();
        bad[REPLY_DOMAIN.len() + 32] = 9;
        assert!(ChallengeReply::decode(&bad).is_err());
        let mut bad = wire;
        bad[REPLY_DOMAIN.len() + 33..REPLY_DOMAIN.len() + 37].fill(255);
        assert!(ChallengeReply::decode(&bad).is_err());
        assert!(ChallengeReply::decode(&vec![0; MAX_CHALLENGE_REPLY + 1]).is_err());
    }
}
