//! Immutable worker normalization and selection from caller-supplied digests.

pub struct StaticWorkerMap<W> {
    workers: Vec<W>,
}

impl<W> StaticWorkerMap<W> {
    /// Reject empty sets and duplicate keys; retain canonical ascending order.
    /// The key must be a stable identity. Replacing a map requires the caller to
    /// drain or migrate work assigned under the old map.
    pub fn new_by_key<K: Ord>(mut workers: Vec<W>, key: impl Fn(&W) -> K) -> Option<Self> {
        workers.sort_by_key(&key);
        if workers.is_empty()
            || workers
                .windows(2)
                .any(|pair| key(&pair[0]) == key(&pair[1]))
        {
            return None;
        }
        Some(Self { workers })
    }

    /// Reduce the first eight big-endian bytes of an opaque 256-bit digest.
    /// Hash algorithm, domain separation, and input encoding belong to callers.
    pub fn select(&self, digest: &[u8; 32]) -> &W {
        let index = u64::from_be_bytes(digest[..8].try_into().expect("eight digest bytes"))
            % self.workers.len() as u64;
        &self.workers[index as usize]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_order_rejects_empty_and_duplicate_identities() {
        assert!(StaticWorkerMap::<u16>::new_by_key(vec![], |w| *w).is_none());
        assert!(StaticWorkerMap::new_by_key(vec![(1, "a"), (1, "b")], |w| w.0).is_none());
        let map = StaticWorkerMap::new_by_key(vec![9, 1, 3], |w| *w).unwrap();
        let ordered = StaticWorkerMap::new_by_key(vec![1, 3, 9], |w| *w).unwrap();
        for value in 0..100_u64 {
            let mut digest = [0; 32];
            digest[..8].copy_from_slice(&value.to_be_bytes());
            assert_eq!(map.select(&digest), ordered.select(&digest));
            assert_eq!(*map.select(&digest), [1, 3, 9][(value % 3) as usize]);
        }
    }

    #[test]
    fn selection_uses_big_endian_prefix_only_and_handles_singletons() {
        let map = StaticWorkerMap::new_by_key(vec![6, 5, 4, 3, 2, 1, 0], |w| *w).unwrap();
        let mut digest = [255; 32];
        digest[..8].copy_from_slice(&256_u64.to_be_bytes());
        assert_eq!(*map.select(&digest), 4);
        digest[8..].fill(0);
        assert_eq!(*map.select(&digest), 4);
        assert_eq!(*map.select(&[255; 32]), 1);
        let singleton = StaticWorkerMap::new_by_key(vec!["only"], |w| *w).unwrap();
        assert_eq!(*singleton.select(&[255; 32]), "only");
        assert_eq!(*singleton.select(&[0; 32]), "only");
    }
}
