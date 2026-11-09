//! Certificate chains carried once per session and referenced by fingerprint.
//!
//! A proof names its signer's chain by a 32-byte fingerprint inside the signed
//! fields. The handshake carries each side's full chain, and a sender attaches
//! any other chain the first time its fingerprint crosses a session. The
//! receiver keeps every chain admitted on a session for the session's lifetime,
//! so a fingerprint either resolves deterministically or is a protocol error.

use crate::*;
use racer_wire::codec::{Endian, Reader};
use std::{
    cell::RefCell,
    collections::{HashMap, HashSet},
    sync::Weak,
};

/// Maximum encoded chain: length-prefixed DER certificates, leaf first.
///
/// Racer node chains are an Ed25519 leaf and at most a few intermediates, about
/// one KiB per certificate. This bound keeps eight such certificates with wide
/// margin while bounding what a single message can attach.
pub const MAX_CHAIN_BYTES: usize = 16 * 1024;

/// Maximum certificates in one chain.
pub const MAX_CHAIN_CERTS: usize = 8;

/// Largest certificate that fits in a chain alone, after its 4-byte length.
pub const MAX_CERT_BYTES: usize = MAX_CHAIN_BYTES - 4;

/// Maximum distinct chains attached to one message: every proof signer of an
/// envelope (original and [`MAX_HOPS`] hops) plus the outer session signer.
pub const MAX_CHAINS_PER_MESSAGE: usize = MAX_HOPS + 2;

/// Hard cap on chains a session admits. Both ends track the same set, so the
/// sender stops reusing a session before it could push the receiver over it.
pub const MAX_SESSION_CHAINS: usize = 512;

/// Maximum `racer-chains` field value: padded base64 of every attached chain.
pub const MAX_CHAINS_FIELD: usize =
    (MAX_CHAINS_PER_MESSAGE * (4 + MAX_CHAIN_BYTES)).div_ceil(3) * 4;

const DOMAIN: &[u8] = b"racer-peer-v2/certificate-chain\0";

/// One exact certificate chain and its fingerprint.
pub struct Chain {
    fingerprint: [u8; 32],

    certificates: Vec<Vec<u8>>,

    encoded: Vec<u8>,
}

impl Chain {
    /// Encode and fingerprint an accepted chain, leaf first.
    pub fn new(certificates: &[Vec<u8>]) -> Result<Arc<Self>> {
        if certificates.is_empty() || certificates.len() > MAX_CHAIN_CERTS {
            return Err(Error::Unauthorized);
        }
        let mut encoded = Vec::new();
        for cert in certificates {
            if cert.is_empty() || cert.len() > MAX_CERT_BYTES {
                return Err(Error::Unauthorized);
            }
            encoded.extend_from_slice(&(cert.len() as u32).to_be_bytes());
            encoded.extend_from_slice(cert);
        }
        if encoded.len() > MAX_CHAIN_BYTES {
            return Err(Error::Unauthorized);
        }
        Ok(Self::from_parts(certificates.to_vec(), encoded))
    }

    /// Decode one exact length-prefixed chain.
    pub fn decode(encoded: &[u8]) -> Result<Arc<Self>> {
        if encoded.len() > MAX_CHAIN_BYTES {
            return Err(Error::Unauthorized);
        }
        let mut reader = Reader::new(encoded, Endian::Big);
        let mut certificates = Vec::new();
        while !reader.remaining().is_empty() {
            if certificates.len() == MAX_CHAIN_CERTS {
                return Err(Error::Unauthorized);
            }
            let cert = reader
                .length_prefixed(MAX_CERT_BYTES)
                .map_err(|_| Error::Unauthorized)?;
            if cert.is_empty() {
                return Err(Error::Unauthorized);
            }
            certificates.push(cert.to_vec());
        }
        if certificates.is_empty() {
            return Err(Error::Unauthorized);
        }
        Ok(Self::from_parts(certificates, encoded.to_vec()))
    }

    fn from_parts(certificates: Vec<Vec<u8>>, encoded: Vec<u8>) -> Arc<Self> {
        let mut hash = Sha256::new();
        hash.update(DOMAIN);
        hash.update(&encoded);
        let chain = Arc::new(Self {
            fingerprint: hash.finalize().into(),
            certificates,
            encoded,
        });
        #[cfg(any(test, feature = "test-util"))]
        test_util::register(&chain);
        chain
    }

    /// The signed reference to this chain.
    pub fn fingerprint(&self) -> &[u8; 32] {
        &self.fingerprint
    }

    /// The certificates, leaf first.
    pub fn certificates(&self) -> &[Vec<u8>] {
        &self.certificates
    }

    /// The exact length-prefixed encoding.
    pub fn encoded(&self) -> &[u8] {
        &self.encoded
    }
}

/// Resolve a signed fingerprint to the chain its carrier admitted.
pub trait Chains {
    /// Return the admitted chain, or `None` when the carrier never received it.
    fn chain(&self, fingerprint: &[u8; 32]) -> Option<Arc<Chain>>;
}

/// Read the signed fingerprint of a proof.
pub fn fingerprint(head: &MessageHead) -> Result<[u8; 32]> {
    decode_binary(field(head, "racer-certificate")?.as_bytes())?
        .try_into()
        .map_err(|_| Error::Unauthorized)
}

/// Attach the carrier's chain to a decoded proof. A miss is a protocol error.
pub fn resolve(signed: &mut SignedHead, chains: &dyn Chains) -> Result<()> {
    signed.chain = Some(
        chains
            .chain(&fingerprint(&signed.head)?)
            .ok_or(Error::InvalidRequest)?,
    );
    Ok(())
}

/// Encode chains for one `racer-chains` field.
pub fn encode_attachment(chains: &[Arc<Chain>]) -> Result<String> {
    if chains.is_empty() || chains.len() > MAX_CHAINS_PER_MESSAGE {
        return Err(Error::InvalidRequest);
    }
    let mut bytes = Vec::new();
    for chain in chains {
        bytes.extend_from_slice(&(chain.encoded.len() as u32).to_be_bytes());
        bytes.extend_from_slice(&chain.encoded);
    }
    Ok(binary(&bytes))
}

/// Decode one `racer-chains` field, rejecting duplicates and oversize values.
pub fn decode_attachment(value: &[u8]) -> Result<Vec<Arc<Chain>>> {
    if value.len() > MAX_CHAINS_FIELD {
        return Err(Error::InvalidRequest);
    }
    let bytes = STANDARD.decode(value).map_err(|_| Error::InvalidRequest)?;
    if binary(&bytes).as_bytes() != value {
        return Err(Error::InvalidRequest);
    }
    let mut reader = Reader::new(&bytes, Endian::Big);
    let mut chains: Vec<Arc<Chain>> = Vec::new();
    while !reader.remaining().is_empty() {
        if chains.len() == MAX_CHAINS_PER_MESSAGE {
            return Err(Error::InvalidRequest);
        }
        let encoded = reader
            .length_prefixed(MAX_CHAIN_BYTES)
            .map_err(|_| Error::InvalidRequest)?;
        let chain = Chain::decode(encoded).map_err(|_| Error::InvalidRequest)?;
        if chains.iter().any(|c| c.fingerprint == chain.fingerprint) {
            return Err(Error::InvalidRequest);
        }
        chains.push(chain);
    }
    if chains.is_empty() {
        return Err(Error::InvalidRequest);
    }
    Ok(chains)
}

/// Thread-local index from fingerprint to any chain a live owner still holds.
///
/// It never supplies a receiver's chains: those come only from the carrying
/// session. A sender uses it to find the bytes of a chain it must attach, which
/// the proof being sent keeps alive. Workers are threads, so a relay finds the
/// chains its inbound sessions admitted.
#[derive(Default)]
struct Book {
    entries: HashMap<[u8; 32], Weak<Chain>>,

    prune_at: usize,
}

thread_local! {
    static BOOK: RefCell<Book> = RefCell::new(Book::default());
}

/// Record a chain held by a proof or session.
pub(crate) fn remember(chain: &Arc<Chain>) {
    BOOK.with(|book| {
        let mut book = book.borrow_mut();
        if book
            .entries
            .get(&chain.fingerprint)
            .is_some_and(|weak| weak.strong_count() != 0)
        {
            return;
        }
        if book.entries.len() >= book.prune_at {
            book.entries.retain(|_, weak| weak.strong_count() != 0);
            book.prune_at = book.entries.len() * 2 + 64;
        }
        book.entries
            .insert(chain.fingerprint, Arc::downgrade(chain));
    })
}

/// Find a chain that some live owner on this thread still holds.
pub(crate) fn recall(fingerprint: &[u8; 32]) -> Option<Arc<Chain>> {
    BOOK.with(|book| book.borrow().entries.get(fingerprint)?.upgrade())
}

/// Session-owned chain state: what the peer admitted from us, and what we
/// admitted from the peer. Neither side evicts within a session.
pub(crate) struct SessionChains {
    received: HashMap<[u8; 32], Arc<Chain>>,

    sent: HashSet<[u8; 32]>,
}

impl SessionChains {
    /// Seed both directions with the chains the handshake exchanged.
    pub(crate) fn new(peer: Arc<Chain>, local: [u8; 32]) -> Self {
        Self {
            received: HashMap::from([(peer.fingerprint, peer)]),
            sent: HashSet::from([local]),
        }
    }

    /// Whether one more message may attach the most chains any message can need.
    pub(crate) fn has_room(&self) -> bool {
        self.sent.len() + MAX_CHAINS_PER_MESSAGE <= MAX_SESSION_CHAINS
            && self.received.len() + MAX_CHAINS_PER_MESSAGE <= MAX_SESSION_CHAINS
    }

    pub(crate) fn sent(&self, fingerprint: &[u8; 32]) -> bool {
        self.sent.contains(fingerprint)
    }

    /// Record chains only after the message carrying them was signed for sending.
    pub(crate) fn mark_sent(&mut self, fingerprints: impl IntoIterator<Item = [u8; 32]>) {
        self.sent.extend(fingerprints);
    }

    pub(crate) fn received(&self, fingerprint: &[u8; 32]) -> Option<&Arc<Chain>> {
        self.received.get(fingerprint)
    }

    /// Admit authenticated attachments. A repeat or overflow is a protocol error.
    pub(crate) fn admit(&mut self, chains: Vec<Arc<Chain>>) -> Result<()> {
        if self.received.len() + chains.len() > MAX_SESSION_CHAINS
            || chains
                .iter()
                .any(|c| self.received.contains_key(&c.fingerprint))
        {
            return Err(Error::InvalidRequest);
        }
        for chain in chains {
            self.received.insert(chain.fingerprint, chain);
        }
        Ok(())
    }

    #[cfg(feature = "test-util")]
    pub(crate) fn counts(&self) -> (usize, usize) {
        (self.sent.len(), self.received.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn framed(parts: &[&[u8]]) -> Vec<u8> {
        let mut bytes = Vec::new();
        for part in parts {
            bytes.extend_from_slice(&(part.len() as u32).to_be_bytes());
            bytes.extend_from_slice(part);
        }
        bytes
    }

    #[test]
    fn chain_limits_agree_between_new_and_decode() {
        let largest = vec![1; MAX_CERT_BYTES];
        let chain = Chain::new(std::slice::from_ref(&largest)).unwrap();
        assert_eq!(chain.encoded().len(), MAX_CHAIN_BYTES);
        assert_eq!(
            Chain::decode(chain.encoded()).unwrap().certificates(),
            [largest]
        );
        assert!(Chain::new(&[vec![1; MAX_CERT_BYTES + 1]]).is_err());
        let many = vec![vec![1]; MAX_CHAIN_CERTS];
        assert!(Chain::decode(Chain::new(&many).unwrap().encoded()).is_ok());
        let too_many = vec![vec![1]; MAX_CHAIN_CERTS + 1];
        assert!(Chain::new(&too_many).is_err());
        let parts: Vec<&[u8]> = too_many.iter().map(Vec::as_slice).collect();
        assert!(Chain::decode(&framed(&parts)).is_err());
    }

    #[test]
    fn chain_decode_rejects_bad_framing() {
        let good = framed(&[b"ab", b"c"]);
        assert_eq!(Chain::decode(&good).unwrap().certificates().len(), 2);
        for end in (1..good.len()).filter(|&end| end != 6) {
            assert_eq!(
                Chain::decode(&good[..end]).err(),
                Some(Error::Unauthorized),
                "{end}"
            );
        }
        assert!(Chain::decode(&[]).is_err());
        assert!(Chain::decode(&framed(&[b""])).is_err());
    }

    #[test]
    fn attachment_framing_errors_are_invalid_request() {
        let chain = Chain::new(&[vec![1, 2, 3]]).unwrap();
        let other = Chain::new(&[vec![4]]).unwrap();
        let encoded = encode_attachment(&[chain.clone(), other]).unwrap();
        assert_eq!(decode_attachment(encoded.as_bytes()).unwrap().len(), 2);
        let bytes = STANDARD.decode(&encoded).unwrap();
        // 11 bytes is the first framed chain alone: four of length, seven of chain.
        for end in (1..bytes.len()).filter(|&end| end != 11) {
            assert_eq!(
                decode_attachment(binary(&bytes[..end]).as_bytes()).err(),
                Some(Error::InvalidRequest),
                "{end}"
            );
        }
        // A malformed inner chain is the same fault class as bad outer framing.
        assert_eq!(
            decode_attachment(binary(&framed(&[&[0, 0, 0, 0]])).as_bytes()).err(),
            Some(Error::InvalidRequest)
        );
        let duplicate = encode_attachment(&[chain.clone(), chain]).unwrap();
        assert_eq!(
            decode_attachment(duplicate.as_bytes()).err(),
            Some(Error::InvalidRequest)
        );
    }
}

/// Fixtures that decode envelopes without a carrying session.
#[cfg(any(test, feature = "test-util"))]
pub mod test_util {
    use super::*;

    thread_local! {
        static ALL: RefCell<HashMap<[u8; 32], Arc<Chain>>> = RefCell::new(HashMap::new());
    }

    pub(super) fn register(chain: &Arc<Chain>) {
        ALL.with(|all| {
            all.borrow_mut()
                .entry(chain.fingerprint)
                .or_insert_with(|| chain.clone());
        });
    }

    /// Resolve any chain created on this thread, standing in for a session.
    pub struct AnyChain;

    impl Chains for AnyChain {
        fn chain(&self, fingerprint: &[u8; 32]) -> Option<Arc<Chain>> {
            ALL.with(|all| all.borrow().get(fingerprint).cloned())
        }
    }
}
