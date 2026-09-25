//! Canonical distributed hashes. Never use Rust's `Hash` encoding on the wire.
use crate::model::identity::{ObjectId, PageNumber};
use sha2::{Digest, Sha256};

pub(super) fn domain(name: &[u8]) -> Sha256 {
    let mut hash = Sha256::new();
    hash.update(name);
    hash
}

pub(super) fn bytes(hash: &mut Sha256, value: &[u8]) {
    // Identity and publication bounds keep all fields below u32::MAX.
    hash.update((value.len() as u32).to_be_bytes());
    hash.update(value);
}

pub(super) fn object(hash: &mut Sha256, object: &ObjectId, page: PageNumber) {
    bytes(hash, object.cache.0.as_bytes());
    hash.update(object.key.0);
    hash.update(page.0.to_be_bytes());
}

pub(super) fn finish(hash: Sha256) -> [u8; 32] {
    hash.finalize().into()
}
