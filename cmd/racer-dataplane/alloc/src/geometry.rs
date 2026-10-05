//! Validated allocation geometry without checkpoint or record-format policy.
use crate::{Alignment, Error, Result, Segments};

/// Maximum number of slots retained by a segment allocation table.
pub const MAX_SEGMENTS: u64 = 1_000_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SegmentGeometry {
    slab_bytes: u64,
    segment_bytes: u64,
    segment_count: u64,
    alignment: Alignment,
}
impl SegmentGeometry {
    /// Validate physical geometry, independently of retained allocation table size.
    /// Segments::configure separately enforces MAX_SEGMENTS on retained slots.
    pub fn new(
        slab_bytes: u64,
        segment_bytes: u64,
        segment_count: u64,
        alignment: Alignment,
    ) -> Result<Self> {
        if segment_bytes == 0
            || slab_bytes == 0
            || segment_count == 0
            || !slab_bytes.is_multiple_of(segment_bytes)
            || segment_count > slab_bytes / segment_bytes
            || !segment_bytes.is_multiple_of(alignment.offset())
            || !segment_bytes.is_multiple_of(alignment.length() as u64)
            || segment_count.checked_mul(segment_bytes).is_none()
        {
            return Err(Error::Corrupt);
        }
        Ok(Self {
            slab_bytes,
            segment_bytes,
            segment_count,
            alignment,
        })
    }
    pub fn slab_bytes(self) -> u64 {
        self.slab_bytes
    }
    pub fn segment_bytes(self) -> u64 {
        self.segment_bytes
    }
    pub fn segment_count(self) -> u64 {
        self.segment_count
    }
    pub fn alignment(self) -> Alignment {
        self.alignment
    }
    /// Compare the live allocation table dimensions, not its current occupancy.
    /// Direct-I/O alignment compatibility is a separate caller-owned check.
    pub fn matches_segments(&self, segments: &Segments) -> bool {
        segments.capacity_bytes() == self.slab_bytes
            && segments.segment_bytes() == self.segment_bytes
            && segments.count() as u64 == self.segment_count
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn alignment() -> Alignment {
        Alignment::new(512, 512, 512).unwrap()
    }

    #[test]
    fn geometry_accepts_partial_tables_without_checkpoint_item_policy() {
        let geometry = SegmentGeometry::new(4096, 1024, 2, alignment()).unwrap();
        assert_eq!(geometry.slab_bytes(), 4096);
        assert_eq!(geometry.segment_bytes(), 1024);
        assert_eq!(geometry.segment_count(), 2);
        assert_eq!(geometry.alignment(), alignment());
        assert!(SegmentGeometry::new(1024 * MAX_SEGMENTS, 1024, MAX_SEGMENTS, alignment()).is_ok());
        assert!(
            SegmentGeometry::new(
                1024 * (MAX_SEGMENTS + 1),
                1024,
                MAX_SEGMENTS + 1,
                alignment()
            )
            .is_ok()
        );
        assert!(
            SegmentGeometry::new(u64::MAX, 1, u64::MAX, Alignment::new(1, 1, 1).unwrap()).is_ok()
        );
    }
    #[test]
    fn geometry_rejects_zero_misalignment_capacity_and_overflow() {
        for (slab, segment, count) in [
            (0, 512, 1),
            (512, 0, 1),
            (512, 512, 0),
            (513, 512, 1),
            (512, 512, 2),
            (514, 257, 2),
            (u64::MAX, u64::MAX, 2),
        ] {
            assert_eq!(
                SegmentGeometry::new(slab, segment, count, alignment()),
                Err(Error::Corrupt)
            );
        }
        assert_eq!(
            SegmentGeometry::new(1024, 1024, 1, Alignment::new(512, 512, 768).unwrap()),
            Err(Error::Corrupt)
        );
    }
    #[test]
    fn matching_live_table_ignores_occupancy_but_requires_dimensions() {
        let geometry = SegmentGeometry::new(4096, 1024, 2, alignment()).unwrap();
        let segments = Segments::new(1024);
        assert!(!geometry.matches_segments(&segments));
        segments.configure(4096, 2, alignment()).unwrap();
        assert!(geometry.matches_segments(&segments));
        let _held = segments.append(512).unwrap();
        assert!(geometry.matches_segments(&segments));
        assert!(
            !SegmentGeometry::new(4096, 1024, 3, alignment())
                .unwrap()
                .matches_segments(&segments)
        );
        assert!(
            !SegmentGeometry::new(2048, 1024, 2, alignment())
                .unwrap()
                .matches_segments(&segments)
        );
        assert!(
            !SegmentGeometry::new(4096, 512, 2, alignment())
                .unwrap()
                .matches_segments(&segments)
        );
    }

    #[test]
    fn divisibility_by_each_unit_is_equivalent_to_lcm_divisibility() {
        let alignment = Alignment::new(512, 512, 768).unwrap();
        assert!(SegmentGeometry::new(3072, 1536, 2, alignment).is_ok());
        assert_eq!(
            SegmentGeometry::new(2048, 1024, 2, alignment),
            Err(Error::Corrupt)
        );
        assert_eq!(
            SegmentGeometry::new(1536, 768, 2, alignment),
            Err(Error::Corrupt)
        );
        assert!(SegmentGeometry::new(u64::MAX, 1, 1, Alignment::new(1, 1, 1).unwrap()).is_ok());
    }
}
