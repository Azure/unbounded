// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Panic-safe admission accounting for queued and running RPC requests.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

pub(crate) struct RpcAdmission {
    limit: u64,
    inflight: AtomicU64,
}

impl RpcAdmission {
    pub(crate) fn new(limit: usize) -> Arc<Self> {
        Arc::new(Self {
            limit: limit as u64,
            inflight: AtomicU64::new(0),
        })
    }

    pub(crate) fn try_acquire(self: &Arc<Self>) -> Option<InflightPermit> {
        let previous = self.inflight.fetch_add(1, Ordering::AcqRel);
        if previous >= self.limit {
            self.inflight.fetch_sub(1, Ordering::AcqRel);
            return None;
        }

        crate::metrics::fabric_inflight_delta(1);
        Some(InflightPermit {
            admission: self.clone(),
        })
    }

    #[cfg(test)]
    fn inflight(&self) -> u64 {
        self.inflight.load(Ordering::Acquire)
    }
}

pub(crate) struct InflightPermit {
    admission: Arc<RpcAdmission>,
}

impl Drop for InflightPermit {
    fn drop(&mut self) {
        self.admission.inflight.fetch_sub(1, Ordering::AcqRel);
        crate::metrics::fabric_inflight_delta(-1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn permit_releases_capacity_exactly_once() {
        let admission = RpcAdmission::new(1);
        let permit = admission.try_acquire().expect("first request admitted");

        assert!(admission.try_acquire().is_none());
        assert_eq!(admission.inflight(), 1);

        drop(permit);
        assert_eq!(admission.inflight(), 0);
        assert!(admission.try_acquire().is_some());
    }

    #[test]
    fn permit_releases_capacity_during_unwind() {
        let admission = RpcAdmission::new(1);
        let unwind_admission = admission.clone();

        let result = std::panic::catch_unwind(move || {
            let _permit = unwind_admission
                .try_acquire()
                .expect("request admitted before panic");
            panic!("test panic");
        });

        assert!(result.is_err());
        assert_eq!(admission.inflight(), 0);
        assert!(admission.try_acquire().is_some());
    }
}
