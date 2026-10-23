//! Wire identities for simulated pages and nodes.
//!
//! Simulator pages are dense `u64` ids (`object * pages_per_object + page`,
//! see [`crate::workload::page_id`]). The real decision code takes wire
//! types, so this module builds them: one cache (`sim`), a 32-byte cache key
//! holding the object number, one fixed strong ETag, and node ids that sort
//! in node order.

use racer_control_wire::{CacheId, NodeId};
use racer_core::read::worker::WorkerMap;
use racer_object_wire::model::{
    CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag, WorkerId,
};

/// Interned cache key used for quota fairness (the only simulated cache).
pub const CACHE: u32 = 0;

pub struct Ids {
    pages_per_object: u64,
    nodes: Vec<NodeId>,
    etag: StrongEtag,
    workers: WorkerMap,
}

impl Ids {
    pub fn new(nodes: u32, workers: u8, pages_per_object: u32) -> Self {
        let map = WorkerMap::new((0..u16::from(workers.max(1))).map(WorkerId).collect())
            .expect("distinct workers");
        Self {
            pages_per_object: u64::from(pages_per_object.max(1)),
            nodes: (0..nodes).map(|n| NodeId(format!("n{n:08}"))).collect(),
            etag: StrongEtag::parse(b"\"v1\"").expect("valid etag"),
            workers: map,
        }
    }

    pub fn node(&self, node: u32) -> &NodeId {
        &self.nodes[node as usize]
    }

    pub fn object(&self, page: u64) -> ObjectId {
        let mut key = [0u8; 32];
        key[..8].copy_from_slice(&(page / self.pages_per_object).to_be_bytes());
        ObjectId {
            cache: CacheId("sim".to_owned()),
            key: CacheKey(key),
        }
    }

    pub fn number(&self, page: u64) -> PageNumber {
        PageNumber(page % self.pages_per_object)
    }

    pub fn page(&self, page: u64) -> PageId {
        PageId {
            version: ObjectVersion {
                object: self.object(page),
                etag: self.etag.clone(),
            },
            number: self.number(page),
        }
    }

    /// Inverse of [`Self::page`].
    pub fn key(&self, page: &PageId) -> u64 {
        let object = u64::from_be_bytes(
            page.version.object.key.0[..8]
                .try_into()
                .expect("eight bytes"),
        );
        object * self.pages_per_object + page.number.0
    }

    /// Local worker for `page` (`WorkerMap::select`, production dispatch).
    pub fn worker(&self, page: u64) -> u8 {
        let object = self.object(page);
        self.workers
            .select(&object, self.number(page).0)
            .expect("valid object")
            .0 as u8
    }

    pub fn approximate_bytes(&self) -> usize {
        self.nodes.capacity() * std::mem::size_of::<NodeId>()
            + self.nodes.iter().map(|n| n.0.capacity()).sum::<usize>()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trips_and_spreads_workers() {
        let ids = Ids::new(4, 2, 16);
        for page in [0, 15, 16, 12_345] {
            assert_eq!(ids.key(&ids.page(page)), page);
        }
        assert_eq!(ids.page(17).number, PageNumber(1));
        assert!(ids.node(0).0 < ids.node(3).0);
        let workers: std::collections::BTreeSet<u8> = (0..64).map(|p| ids.worker(p)).collect();
        assert_eq!(workers.into_iter().collect::<Vec<_>>(), [0, 1]);
        assert_eq!(ids.worker(5), ids.worker(5));
    }
}
