// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Stripe-keyed second-sight admission view over [`AdmissionFilter`].
//!
//! The p2p subsystem speaks in [`StripeKey`] (32-byte content
//! addresses) and uses second-sight admission on the return path:
//! a stripe seen for the first time is recorded but not cached, and
//! a stripe seen a second time within the epoch is admitted to the
//! LRU. The underlying machinery is the same doorkeeper bloom that
//! the local engine uses for page admission; we just project a
//! `StripeKey` into a [`PageKey`] with `page_idx = 0` so the bloom
//! buckets land deterministically.
//!
//! Cheap to clone (it is an `Arc` internally) so multiple
//! return-path workers can share one view.

use std::sync::Arc;

use crate::bufferpool::StripeKey;
use crate::storage::admission::AdmissionFilter;
use crate::storage::types::PageKey;

/// Outcome of consulting the view on the return path.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum AdmitDecision {
    /// Do not insert this stripe into the LRU on the return path.
    /// The doorkeeper has been updated so a subsequent sighting
    /// will be admitted.
    Skip,
    /// Insert this stripe into the LRU on the return path.
    Cache,
}

/// Stripe-keyed second-sight view over [`AdmissionFilter`].
#[derive(Clone)]
pub struct StripeAdmission {
    inner: Arc<AdmissionFilter>,
}

impl StripeAdmission {
    /// Construct with the same capacity semantics as
    /// [`AdmissionFilter`]. Sized for a few million entries at
    /// roughly 1% FPR. The view uses doorkeeper-only mode; the
    /// count-min sketch is not allocated.
    pub fn new(capacity_stripes: u64) -> Self {
        Self {
            inner: Arc::new(AdmissionFilter::new_doorkeeper_only(capacity_stripes)),
        }
    }

    /// Observe a stripe on the return path. First sighting returns
    /// [`AdmitDecision::Skip`] and inserts into the doorkeeper;
    /// subsequent sightings within the current epoch return
    /// [`AdmitDecision::Cache`].
    pub fn observe(&self, key: StripeKey) -> AdmitDecision {
        let pk = PageKey::new(key.0, 0);
        if self.inner.should_admit(&pk) {
            AdmitDecision::Cache
        } else {
            AdmitDecision::Skip
        }
    }

    /// Reset the underlying doorkeeper and sketch. Used by tests
    /// and future warmup boundaries.
    pub fn reset(&self) {
        self.inner.reset();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn first_sight_is_skip_second_is_cache() {
        let view = StripeAdmission::new(1024);
        let key = StripeKey([0xab; 32]);
        assert_eq!(view.observe(key), AdmitDecision::Skip);
        assert_eq!(view.observe(key), AdmitDecision::Cache);
    }

    #[test]
    fn independent_stripes_do_not_cross_contaminate() {
        let view = StripeAdmission::new(1024);
        let a = StripeKey([1u8; 32]);
        let b = StripeKey([2u8; 32]);
        assert_eq!(view.observe(a), AdmitDecision::Skip);
        assert_eq!(view.observe(b), AdmitDecision::Skip);
        assert_eq!(view.observe(a), AdmitDecision::Cache);
        assert_eq!(view.observe(b), AdmitDecision::Cache);
    }

    #[test]
    fn reset_returns_to_skip() {
        let view = StripeAdmission::new(1024);
        let key = StripeKey([7u8; 32]);
        assert_eq!(view.observe(key), AdmitDecision::Skip);
        assert_eq!(view.observe(key), AdmitDecision::Cache);
        view.reset();
        assert_eq!(view.observe(key), AdmitDecision::Skip);
    }
}
