//! Stable SHA-256 building blocks for domain-separated topology keys.
//! Application schemas own domain bytes and field order; these helpers neither
//! invent delimiters nor validate application identities.
use crate::Member;
use sha2::{Digest, Sha256};

pub(crate) fn domain<M: Member>(suffix: &[u8]) -> Sha256 {
    let mut hash = named_domain(M::DOMAIN.as_bytes());
    hash.update(suffix);
    hash
}

/// Start a hash with the exact supplied domain, including any schema terminator.
pub fn named_domain(name: &[u8]) -> Sha256 {
    let mut hash = Sha256::new();
    hash.update(name);
    hash
}

/// Append a byte field prefixed with its big-endian u32 length. Callers must
/// bound fields to u32::MAX bytes, as required by the existing hash schema.
pub fn bytes(hash: &mut Sha256, value: &[u8]) {
    hash.update((value.len() as u32).to_be_bytes());
    hash.update(value);
}

/// Finalize a domain-separated key without changing the underlying digest.
pub fn finish(hash: Sha256) -> [u8; 32] {
    hash.finalize().into()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exact_domain_and_length_prefix_bytes() {
        let mut hash = named_domain(b"app/key/v1\0");
        bytes(&mut hash, b"abc");
        bytes(&mut hash, b"");
        let expected: [u8; 32] = Sha256::digest(b"app/key/v1\0\0\0\0\x03abc\0\0\0\0").into();
        assert_eq!(finish(hash), expected);
        assert_eq!(
            finish(named_domain(b"")),
            <[u8; 32]>::from(Sha256::digest(b""))
        );
    }

    #[test]
    fn field_boundaries_and_domains_are_distinct() {
        let key = |domain: &[u8], a: &[u8], b: &[u8]| {
            let mut hash = named_domain(domain);
            bytes(&mut hash, a);
            bytes(&mut hash, b);
            finish(hash)
        };
        assert_ne!(key(b"a\0", b"ab", b"c"), key(b"a\0", b"a", b"bc"));
        assert_ne!(key(b"a\0", b"ab", b"c"), key(b"b\0", b"ab", b"c"));
    }
}
