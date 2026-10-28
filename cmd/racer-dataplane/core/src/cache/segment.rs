//! Disk segment value used to order CLOCK eviction candidates.
//!
//! The catalog sums live payload value, not padded disk size, over a bounded
//! prefix of a segment's mappings and charges a conservative value for the
//! unseen suffix. Scores are preferences, never eviction exemptions.

/// Most mappings inspected when scoring one segment.
pub const SCAN_BUDGET: usize = 256;

/// Highest retention score of one page: heat 0 to 3 plus 1 for ownership.
pub const MAX_SCORE: u8 = 4;
/// Highest value [`class_score`] returns.
pub const MAX_PAGE_VALUE: u8 = 2 * MAX_SCORE + 1;

/// Value charged for each mapping beyond [`SCAN_BUDGET`]: a full page at the
/// highest retention score.
#[must_use]
pub const fn unseen_value(page_bytes: u64) -> u64 {
    unseen_value_for(page_bytes, false)
}

/// [`unseen_value`] while some cache may be over its fair share: unseen
/// mappings are charged at the highest value [`class_score`] can give.
#[must_use]
pub const fn unseen_value_for(page_bytes: u64, any_over_share: bool) -> u64 {
    page_bytes.saturating_mul(if any_over_share {
        MAX_PAGE_VALUE as u64
    } else {
        MAX_SCORE as u64
    })
}

/// Per-byte value of one page given its retention `score` (at most
/// [`MAX_SCORE`]). While any cache is over share, every under-share page
/// outvalues every over-share page, so segments dominated by over-share data
/// recycle first; the retention score still orders pages within one class.
/// With no cache over share this is the plain retention score.
#[must_use]
pub const fn class_score(score: u8, any_over_share: bool, page_over_share: bool) -> u8 {
    if !any_over_share || page_over_share {
        score
    } else {
        score + MAX_SCORE + 1
    }
}

/// Value of one resident page: payload bytes times retention score. A page
/// whose length cannot be derived counts as a full page.
#[must_use]
pub fn page_value(bytes: Option<u32>, page_bytes: u32, score: u8) -> u64 {
    u64::from(bytes.unwrap_or(page_bytes)) * u64::from(score)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn page_value_uses_length_or_full_page() {
        assert_eq!(page_value(Some(10), 100, 3), 30);
        assert_eq!(page_value(None, 100, 3), 300);
        assert_eq!(page_value(Some(10), 100, 0), 0);
        assert_eq!(
            page_value(Some(u32::MAX), 1, u8::MAX),
            u64::from(u32::MAX) * 255
        );
    }

    #[test]
    fn class_score_puts_under_share_pages_above_over_share_pages() {
        for score in 0..=MAX_SCORE {
            assert_eq!(class_score(score, false, false), score);
            assert_eq!(class_score(score, false, true), score);
            assert_eq!(class_score(score, true, true), score);
            assert!(class_score(0, true, false) > class_score(MAX_SCORE, true, true));
            assert!(class_score(score, true, false) <= MAX_PAGE_VALUE);
        }
        assert_eq!(unseen_value(10), 40);
        assert_eq!(unseen_value_for(10, false), 40);
        assert_eq!(unseen_value_for(10, true), 90);
    }
}
