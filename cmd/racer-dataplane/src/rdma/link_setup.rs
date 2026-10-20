//! Version 2 link setup codec, link id derivation, and simultaneous-setup
//! tie-break for persistent RDMA links (`designs/racer-rdma-sessions.md`,
//! "Link setup over HTTP" and "Link table and state machine").
//!
//! Pure code: no verbs or HTTP state. The endpoint is carried as opaque bytes
//! so this module does not depend on verbs-crate types.

use crate::error::Error;
use crate::error::Result;
use racer_control_wire::NodeId;
use racer_control_wire::RailId;
use sha2::Digest;
use sha2::Sha256;

/// Version tag that starts every v2 setup value.
pub(crate) const SETUP_PREFIX: &[u8] = b"racer-rdma-setup-v2\0";

/// Encoded endpoint length (`rdma_verbs::Endpoint::ENCODED_LEN`).
pub(crate) const ENDPOINT_LEN: usize = 32;

/// Exact encoded setup length.
pub(crate) const SETUP_LEN: usize = SETUP_PREFIX.len() + 2 + 16 + ENDPOINT_LEN + 4 + 4;

const LINK_ID_DOMAIN: &[u8] = b"racer-rdma-session-v2\0";

/// Decoded v2 setup value.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Setup {
    pub rail: RailId,
    pub nonce: [u8; 16],
    pub endpoint: [u8; ENDPOINT_LEN],
    pub recv_credits: u32,
    pub grants_per_link: u32,
}

impl Setup {
    /// Build a setup with a fresh random nonce.
    pub fn new(
        rail: RailId,
        endpoint: [u8; ENDPOINT_LEN],
        recv_credits: u32,
        grants_per_link: u32,
    ) -> Result<Self> {
        let mut nonce = [0; 16];
        racer_platform::environment::fill_random(&mut nonce).map_err(|_| Error::Io)?;
        let setup = Self {
            rail,
            nonce,
            endpoint,
            recv_credits,
            grants_per_link,
        };
        setup.validate()?;
        Ok(setup)
    }

    fn validate(&self) -> Result<()> {
        if self.nonce == [0; 16] || self.recv_credits == 0 || self.grants_per_link == 0 {
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }

    /// Canonical bytes carried base64-encoded in the signed setup header.
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(SETUP_LEN);
        out.extend_from_slice(SETUP_PREFIX);
        out.extend_from_slice(&self.rail.0.to_be_bytes());
        out.extend_from_slice(&self.nonce);
        out.extend_from_slice(&self.endpoint);
        out.extend_from_slice(&self.recv_credits.to_be_bytes());
        out.extend_from_slice(&self.grants_per_link.to_be_bytes());
        out
    }

    /// Decode exact v2 bytes for the expected rail. Rejects other versions,
    /// trailing or missing bytes, a zero nonce, and zero credit limits.
    pub fn decode(bytes: &[u8], rail: RailId) -> Result<Self> {
        if bytes.len() != SETUP_LEN || !bytes.starts_with(SETUP_PREFIX) {
            return Err(Error::InvalidRequest);
        }
        let b = &bytes[SETUP_PREFIX.len()..];
        let u32_at = |i: usize| u32::from_be_bytes(b[i..i + 4].try_into().unwrap());
        let setup = Self {
            rail: RailId(u16::from_be_bytes([b[0], b[1]])),
            nonce: b[2..18].try_into().unwrap(),
            endpoint: b[18..18 + ENDPOINT_LEN].try_into().unwrap(),
            recv_credits: u32_at(18 + ENDPOINT_LEN),
            grants_per_link: u32_at(22 + ENDPOINT_LEN),
        };
        if setup.rail != rail {
            return Err(Error::InvalidRequest);
        }
        setup.validate()?;
        Ok(setup)
    }
}

/// SHA-256 of the initiator's exact setup bytes, sent by the responder.
pub(crate) fn binding(initiator: &[u8]) -> [u8; 32] {
    Sha256::digest(initiator).into()
}

/// Initiator check of a response: the binding must name our exact setup, and
/// the responder's setup must not echo ours.
pub(crate) fn check_response(ours: &[u8], theirs: &[u8], binding_value: &[u8]) -> Result<()> {
    if binding_value != binding(ours) {
        return Err(Error::Unauthorized);
    }
    if theirs == ours {
        return Err(Error::Replay);
    }
    Ok(())
}

/// Identifier carried in every in-band control message of one link.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub(crate) struct LinkId(pub u64);

impl LinkId {
    /// First 8 bytes (big-endian) of
    /// `SHA-256(domain || min(setups) || max(setups))`; symmetric in its arguments.
    ///
    /// Unused: the responder must name the link id before its QP endpoint
    /// exists, so links use [`Self::for_setup`] (see the design's "Deviations").
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn derive(a: &[u8], b: &[u8]) -> Self {
        let (low, high) = if a <= b { (a, b) } else { (b, a) };
        let mut hash = Sha256::new();
        hash.update(LINK_ID_DOMAIN);
        hash.update(low);
        hash.update(high);
        Self(u64::from_be_bytes(hash.finalize()[..8].try_into().unwrap()))
    }
}

impl LinkId {
    /// First 8 bytes (big-endian) of
    /// `SHA-256(domain || initiator setup || responder nonce)`.
    ///
    /// The responder picks its nonce before it creates its QP, so both sides
    /// know the id before either posts a control message. Both inputs are in
    /// the signed exchange: the initiator setup in the request, the nonce in
    /// the response setup.
    pub fn for_setup(initiator: &[u8], responder_nonce: &[u8; 16]) -> Self {
        let mut hash = Sha256::new();
        hash.update(LINK_ID_DOMAIN);
        hash.update(initiator);
        hash.update(responder_nonce);
        Self(u64::from_be_bytes(hash.finalize()[..8].try_into().unwrap()))
    }
}

/// Rail named by encoded setup bytes, before the full decode.
pub(crate) fn rail_of(bytes: &[u8]) -> Result<RailId> {
    if bytes.len() != SETUP_LEN || !bytes.starts_with(SETUP_PREFIX) {
        return Err(Error::InvalidRequest);
    }
    let b = &bytes[SETUP_PREFIX.len()..];
    Ok(RailId(u16::from_be_bytes([b[0], b[1]])))
}

/// Outcome when an initiator in `Connecting` receives the peer's offer for the
/// same rail.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum TieBreak {
    /// Peer NodeId is lower: break our attempt and accept the peer's offer.
    AcceptPeer,
    /// Our NodeId is lower (or equal): answer `409 Conflict`.
    Reject,
}

/// The lower NodeId (byte order of the string) wins.
pub(crate) fn tie_break(local: &NodeId, peer: &NodeId) -> TieBreak {
    if peer.0.as_bytes() < local.0.as_bytes() {
        TieBreak::AcceptPeer
    } else {
        TieBreak::Reject
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn setup(rail: u16, n: u8) -> Setup {
        Setup {
            rail: RailId(rail),
            nonce: [n; 16],
            endpoint: [n.wrapping_add(1); ENDPOINT_LEN],
            recv_credits: 64,
            grants_per_link: 16,
        }
    }

    #[test]
    fn encode_layout_and_roundtrip() {
        let s = setup(3, 7);
        let bytes = s.encode();
        assert_eq!(bytes.len(), SETUP_LEN);
        assert_eq!(SETUP_LEN, 78);
        assert!(bytes.starts_with(SETUP_PREFIX));
        assert_eq!(&bytes[20..22], &[0, 3]);
        assert_eq!(&bytes[70..74], &64u32.to_be_bytes());
        assert_eq!(&bytes[74..78], &16u32.to_be_bytes());
        assert_eq!(Setup::decode(&bytes, RailId(3)).unwrap(), s);
    }

    #[test]
    fn new_draws_nonzero_distinct_nonces_and_rejects_zero_limits() {
        let a = Setup::new(RailId(1), [2; 32], 8, 4).unwrap();
        let b = Setup::new(RailId(1), [2; 32], 8, 4).unwrap();
        assert_ne!(a.nonce, [0; 16]);
        assert_ne!(a.nonce, b.nonce);
        assert!(Setup::new(RailId(1), [2; 32], 0, 4).is_err());
        assert!(Setup::new(RailId(1), [2; 32], 8, 0).is_err());
    }

    #[test]
    fn decode_rejects_malformed_input() {
        let good = setup(1, 5).encode();
        // Truncated, empty, and trailing bytes.
        for len in [0, 1, SETUP_PREFIX.len(), SETUP_LEN - 1] {
            assert!(Setup::decode(&good[..len], RailId(1)).is_err());
        }
        let mut long = good.clone();
        long.push(0);
        assert!(Setup::decode(&long, RailId(1)).is_err());
        // Version mismatch: a v1 prefix of the same length.
        let mut v1 = good.clone();
        v1[..SETUP_PREFIX.len()].copy_from_slice(b"racer-rdma-setup-v1\0");
        assert!(Setup::decode(&v1, RailId(1)).is_err());
        // Rail mismatch.
        assert!(Setup::decode(&good, RailId(2)).is_err());
        // Zero nonce, zero credits, zero grants.
        for (range, _) in [(22..38, "nonce"), (70..74, "credits"), (74..78, "grants")] {
            let mut bad = good.clone();
            bad[range].fill(0);
            assert!(Setup::decode(&bad, RailId(1)).is_err());
        }
        // Maximum field values are accepted.
        let max = Setup {
            rail: RailId(u16::MAX),
            recv_credits: u32::MAX,
            grants_per_link: u32::MAX,
            ..setup(0, 9)
        };
        assert_eq!(Setup::decode(&max.encode(), RailId(u16::MAX)).unwrap(), max);
    }

    #[test]
    fn link_id_is_symmetric_and_input_sensitive() {
        let a = setup(1, 1).encode();
        let b = setup(1, 2).encode();
        let c = setup(1, 3).encode();
        assert_eq!(LinkId::derive(&a, &b), LinkId::derive(&b, &a));
        assert_ne!(LinkId::derive(&a, &b), LinkId::derive(&a, &c));
        assert_eq!(LinkId::derive(&a, &a), LinkId::derive(&a, &a));
        // Known value guards the domain separator and ordering.
        let mut hash = Sha256::new();
        hash.update(b"racer-rdma-session-v2\0");
        hash.update(&a);
        hash.update(&b);
        let expected = u64::from_be_bytes(hash.finalize()[..8].try_into().unwrap());
        assert_eq!(LinkId::derive(&b, &a), LinkId(expected));
    }

    #[test]
    fn setup_link_id_binds_initiator_and_responder_nonce() {
        let a = setup(1, 1).encode();
        let b = setup(1, 2).encode();
        assert_eq!(
            LinkId::for_setup(&a, &[3; 16]),
            LinkId::for_setup(&a, &[3; 16])
        );
        assert_ne!(
            LinkId::for_setup(&a, &[3; 16]),
            LinkId::for_setup(&b, &[3; 16])
        );
        assert_ne!(
            LinkId::for_setup(&a, &[3; 16]),
            LinkId::for_setup(&a, &[4; 16])
        );
        let mut hash = Sha256::new();
        hash.update(b"racer-rdma-session-v2\0");
        hash.update(&a);
        hash.update([3; 16]);
        let expected = u64::from_be_bytes(hash.finalize()[..8].try_into().unwrap());
        assert_eq!(LinkId::for_setup(&a, &[3; 16]), LinkId(expected));
    }

    #[test]
    fn rail_of_reads_rail_and_rejects_malformed() {
        let bytes = setup(9, 1).encode();
        assert_eq!(rail_of(&bytes).unwrap(), RailId(9));
        assert!(rail_of(&bytes[1..]).is_err());
        let mut wrong = bytes.clone();
        wrong[0] ^= 1;
        assert!(rail_of(&wrong).is_err());
    }

    #[test]
    fn response_check_binds_and_rejects_replay() {
        let ours = setup(1, 1).encode();
        let theirs = setup(1, 2).encode();
        check_response(&ours, &theirs, &binding(&ours)).unwrap();
        assert!(matches!(
            check_response(&ours, &theirs, &binding(&theirs)),
            Err(Error::Unauthorized)
        ));
        assert!(matches!(
            check_response(&ours, &theirs, &binding(&ours)[..31]),
            Err(Error::Unauthorized)
        ));
        assert!(matches!(
            check_response(&ours, &ours, &binding(&ours)),
            Err(Error::Replay)
        ));
    }

    #[test]
    fn tie_break_lower_node_wins_in_both_orderings() {
        let low = NodeId("00000001-1111-4111-8111-111111111111".into());
        let high = NodeId("00000002-1111-4111-8111-111111111111".into());
        // The higher node yields to the lower node's offer...
        assert_eq!(tie_break(&high, &low), TieBreak::AcceptPeer);
        // ...and the lower node rejects the higher node's offer.
        assert_eq!(tie_break(&low, &high), TieBreak::Reject);
        // Exactly one side accepts.
        assert_ne!(tie_break(&high, &low), tie_break(&low, &high));
        // Byte order, not length: a prefix sorts first.
        let short = NodeId("a".into());
        let long = NodeId("ab".into());
        assert_eq!(tie_break(&long, &short), TieBreak::AcceptPeer);
        assert_eq!(tie_break(&short, &short), TieBreak::Reject);
    }
}
