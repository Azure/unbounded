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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::topology::fixtures;

    #[test]
    fn full_domain_separated_digest_vectors() {
        fn hex(hash: Sha256) -> String {
            finish(hash)
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect()
        }
        let mut slot = domain(b"racer/slot/v1\0");
        object(&mut slot, &fixtures::object(), PageNumber(0));
        assert_eq!(
            hex(slot),
            "d8b632a58acf4dc92ccc3abe711290968975c95221982118f58393fbdead4781"
        );
        let mut hrw = domain(b"racer/hrw/v1\0");
        hrw.update(887651u32.to_be_bytes());
        bytes(&mut hrw, b"node-000000");
        assert_eq!(
            hex(hrw),
            "e41095812e885f6f0ae7e3c1a93d8ec04999df01dbb5820765c7e972b2c07a9c"
        );
        let mut rail = domain(b"racer/rail/v1\0");
        object(&mut rail, &fixtures::object(), PageNumber(0));
        bytes(&mut rail, b"\"v1\"");
        assert_eq!(
            hex(rail),
            "500304ace38caf7cc36f8f97ae12bdfc89b92f7eb64c8bb060d1daf49f76314c"
        );
    }
}
