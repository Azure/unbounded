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
}
