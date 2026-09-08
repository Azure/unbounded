// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Shared per-request metrics plumbing for the HTTP and S3 frontends.
//!
//! Both serve paths capture the same observable outcome (method, HTTP
//! status, body bytes) and bracket the same active-connection gauge, so
//! that machinery lives here rather than being duplicated per frontend.
//! These are intentionally independent of [`crate::obs`]: metrics are
//! always recorded, regardless of the configured log level.

/// Per-request outcome captured for metrics, recorded once when the
/// serve future completes regardless of the log level. `method` stays
/// `"-"` until the request line is parsed; `status` stays `0` (recorded
/// as `"other"`) if the connection failed before any response was
/// chosen.
pub(crate) struct ReqOutcome {
    pub(crate) method: &'static str,
    pub(crate) status: u16,
    pub(crate) bytes: u64,
}

impl Default for ReqOutcome {
    fn default() -> Self {
        ReqOutcome {
            method: "-",
            status: 0,
            bytes: 0,
        }
    }
}

impl ReqOutcome {
    /// Publish the captured outcome to the `frontend_*` metric families
    /// under the given frontend id label.
    pub(crate) fn record(&self, frontend_id: &str, duration_secs: f64) {
        crate::metrics::frontend_request(
            frontend_id,
            self.method,
            self.status,
            self.bytes,
            duration_secs,
        );
    }
}

/// RAII bracket for the `frontend_active_connections` gauge: increments
/// on accept, decrements when the serve future completes or is dropped.
pub(crate) struct ConnGuard;

impl ConnGuard {
    pub(crate) fn new() -> Self {
        crate::metrics::frontend_connections_delta(1);
        ConnGuard
    }
}

impl Drop for ConnGuard {
    fn drop(&mut self) {
        crate::metrics::frontend_connections_delta(-1);
    }
}
