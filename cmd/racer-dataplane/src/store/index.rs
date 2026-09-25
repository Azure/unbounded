//! Identity mappings and reverse segment membership, published after complete writes.
use super::{
    segment::{Generation, SegmentId},
    slab::SlabLocation,
};
use crate::{
    error::{Result, pending},
    model::{
        identity::{ObjectId, ObjectVersion, PageId, WorkerId},
        metadata::{CurrentVersion, ObjectMetadata, VersionMetadata},
    },
};
#[derive(Clone, Debug)]
pub struct RecordLocation {
    pub segment: SegmentId,
    pub generation: Generation,
    pub location: SlabLocation,
}
/// One storage shard. The catalog budget is independent of retained page entries;
/// evicting a catalog item cannot remove a page's attached immutable descriptor.
pub struct Index {
    worker: WorkerId,
    metadata_capacity: usize,
}
#[derive(Clone, Debug)]
pub struct IndexedPage {
    pub location: RecordLocation,
    pub metadata: VersionMetadata,
}
pub struct IndexSnapshot {
    /// Every entry owns its descriptor: no cross-shard catalog reference can dangle.
    pub entries: Vec<(PageId, IndexedPage)>,
    /// Bounded standalone descriptors, including HEAD-only and zero-length objects,
    /// on their page-zero owner. Current-version freshness is never recovered.
    pub metadata: Vec<VersionMetadata>,
}
impl IndexSnapshot {
    /// Structural checkpoint validation, in addition to checksum, ownership,
    /// capacity, geometry, and generation checks performed during recovery.
    pub fn validate_metadata(&self) -> Result<()> {
        let mut lengths = std::collections::HashMap::new();
        for metadata in self
            .metadata
            .iter()
            .chain(self.entries.iter().map(|(_, entry)| &entry.metadata))
        {
            if lengths
                .insert(&metadata.version, metadata.length)
                .is_some_and(|length| length != metadata.length)
            {
                return Err(crate::error::Error::CorruptRecord);
            }
        }
        for (page, entry) in &self.entries {
            entry.metadata.page_length(page)?;
        }
        Ok(())
    }
}
impl Index {
    pub fn new(worker: WorkerId, metadata_capacity: usize) -> Self {
        Self {
            worker,
            metadata_capacity,
        }
    }
    pub fn lookup(&self, _page: &PageId) -> Result<Option<IndexedPage>> {
        pending("index.lookup")
    }
    /// Atomically publish a completed record with its immutable descriptor. Reject
    /// conflicting lengths for one version; never update current-version freshness.
    pub fn publish(&self, _page: PageId, _entry: IndexedPage) -> Result<()> {
        pending("index.publish")
    }
    /// Look in both the standalone catalog and retained page entries for this exact
    /// version. This operation does not consult TTL or substitute another ETag.
    pub fn version(&self, _version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        pending("index.version")
    }
    /// Page-zero owner only. Supports metadata-only objects without a dirty page,
    /// slab allocation, encryption record, or ciphertext reservation.
    pub fn publish_version(&self, _metadata: VersionMetadata) -> Result<()> {
        pending("index.publish_version")
    }
    pub fn current(&self, _object: &ObjectId) -> Result<Option<CurrentVersion>> {
        pending("index.current")
    }
    /// Page-zero owner only, after fresh revalidation. Atomically retain the
    /// immutable descriptor and advance the volatile pointer; reject length conflicts.
    /// A zero-TTL observation is returned to its waiters without a reusable hit.
    pub fn publish_current(&self, _metadata: ObjectMetadata) -> Result<()> {
        pending("index.publish_current")
    }
    /// Drop volatile freshness on clock uncertainty, preserving version descriptors.
    pub fn invalidate_freshness(&self) -> Result<()> {
        pending("index.invalidate_freshness")
    }
    /// Reclaim standalone catalog entries and any pointers depending on them.
    /// Page-attached descriptors follow page eviction instead; no page is removed.
    pub fn evict_metadata(&self, _entries: usize) -> Result<usize> {
        pending("index.evict_metadata")
    }
    /// Compare the complete mapping before removing, preserving replacement writes.
    pub fn remove_if_matches(&self, _page: &PageId, _location: &RecordLocation) -> Result<()> {
        pending("index.remove")
    }
    pub fn snapshot(&self) -> Result<IndexSnapshot> {
        pending("index.snapshot")
    }
    /// Install only with the matching recovered segment state before admission.
    /// Validate identity/length agreement and catalog bounds; clear all freshness.
    pub fn restore(&self, _snapshot: IndexSnapshot) -> Result<()> {
        pending("index.restore")
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        error::Error,
        model::identity::{CacheId, CacheKey, StrongEtag},
    };

    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
        }
    }

    #[test]
    fn metadata_only_checkpoint_preserves_empty_and_old_versions_without_freshness() {
        let old = descriptor("old", 17);
        let empty = descriptor("new", 0);
        let snapshot = IndexSnapshot {
            entries: vec![],
            metadata: vec![old.clone(), empty.clone()],
        };
        assert_eq!(snapshot.validate_metadata(), Ok(()));
        assert_eq!(snapshot.metadata[0].for_pin().length, 17);
        assert_eq!(snapshot.metadata[1].for_pin().length, 0);
        assert_eq!(
            snapshot.metadata[0].for_pin().expires_at.0,
            std::time::UNIX_EPOCH
        );
    }

    #[test]
    fn checkpoint_rejects_two_lengths_for_one_version() {
        let first = descriptor("v1", 17);
        let conflicting = VersionMetadata {
            length: 18,
            ..first.clone()
        };
        let snapshot = IndexSnapshot {
            entries: vec![],
            metadata: vec![first, conflicting],
        };
        assert_eq!(snapshot.validate_metadata(), Err(Error::CorruptRecord));
    }

    // Compile the complete recovery -> disk copy -> memory -> pending write path.
    // No fake buffers, alignment proof, or successful operational stub is needed.
    fn page_metadata_api_contract(
        snapshot: IndexSnapshot,
        index: &Index,
        page: PageId,
        entry: IndexedPage,
        result: crate::read::fill::PageResult,
        memory: &crate::memory::cache::MemoryCache,
        writer: &crate::store::writer::StoreWriter,
        dirty: crate::runtime::admission::Reservation,
    ) -> Result<()> {
        snapshot.validate_metadata()?;
        index.restore(snapshot)?;
        index.publish(page.clone(), entry)?;
        let _: Option<IndexedPage> = index.lookup(&page)?;
        let _: Option<VersionMetadata> = index.version(&page.version)?;
        let copy = result.copy();
        memory.publish(result.clone())?;
        let _: Option<crate::read::fill::PageResult> = memory.get(&page)?;
        writer.enqueue(copy, dirty)?;
        let _: Option<crate::memory::page::CiphertextCopy> = writer.copy_only(&page)?;
        Ok(())
    }

    // Duplicate writes, reverse membership, and actual restore/eviction behavior
    // remain tests for the storage implementation, not simulated by this scaffold.
}
