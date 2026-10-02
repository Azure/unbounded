use crate::Member;
use sha2::{Digest, Sha256};

pub(crate) fn domain<M: Member>(suffix: &[u8]) -> Sha256 {
    let mut hash = Sha256::new();
    hash.update(M::DOMAIN.as_bytes());
    hash.update(suffix);
    hash
}

pub(crate) fn bytes(hash: &mut Sha256, value: &[u8]) {
    hash.update((value.len() as u32).to_be_bytes());
    hash.update(value);
}

pub(crate) fn finish(hash: Sha256) -> [u8; 32] {
    hash.finalize().into()
}
