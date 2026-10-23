//! Local worker selection for objects and pages.

use super::error::{Error, Result};
use racer_object_wire::model::{ObjectId, WorkerId};
use sha2::{Digest, Sha256};

/// Canonical local worker set. Assignment is independent of discovery order;
/// changing the set requires draining all flights first.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerMap {
    workers: Vec<WorkerId>,
}

impl WorkerMap {
    /// Reject empty and duplicate worker sets.
    pub fn new(mut workers: Vec<WorkerId>) -> Result<Self> {
        workers.sort_unstable_by_key(|worker| worker.0);
        if workers.is_empty() || workers.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { workers })
    }

    /// Canonical (sorted) workers.
    pub fn workers(&self) -> &[WorkerId] {
        &self.workers
    }

    /// Owner of `page` of `object`; page zero also owns metadata.
    pub fn select(&self, object: &ObjectId, page: u64) -> Result<WorkerId> {
        let mut hash = Sha256::new();
        hash.update(b"racer.local-worker.v1\0");
        let length = u64::try_from(object.cache.0.len()).map_err(|_| Error::InvalidRequest)?;
        hash.update(length.to_be_bytes());
        hash.update(object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.to_be_bytes());
        let digest = hash.finalize();
        Ok(self.select_digest(&digest.into()))
    }

    /// Map a precomputed digest onto a worker.
    pub fn select_digest(&self, digest: &[u8; 32]) -> WorkerId {
        let index = u64::from_be_bytes(digest[..8].try_into().expect("eight digest bytes"))
            % u64::try_from(self.workers.len()).expect("worker count fits u64");
        self.workers[index as usize]
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_control_wire::CacheId;
    use racer_object_wire::model::CacheKey;

    #[test]
    fn rejects_empty_and_duplicates_and_is_order_independent() {
        assert_eq!(WorkerMap::new(vec![]), Err(Error::InvalidConfiguration));
        assert_eq!(
            WorkerMap::new(vec![WorkerId(1), WorkerId(1)]),
            Err(Error::InvalidConfiguration)
        );
        let a = WorkerMap::new(vec![WorkerId(2), WorkerId(1)]).unwrap();
        let b = WorkerMap::new(vec![WorkerId(1), WorkerId(2)]).unwrap();
        let object = ObjectId {
            cache: CacheId("c".into()),
            key: CacheKey([7; 32]),
        };
        let mut seen = [false; 2];
        for page in 0..64 {
            let worker = a.select(&object, page).unwrap();
            assert_eq!(worker, b.select(&object, page).unwrap());
            seen[usize::from(worker.0 - 1)] = true;
        }
        assert_eq!(seen, [true, true]);
    }
}
