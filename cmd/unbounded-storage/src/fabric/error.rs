// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Error type for the fabric module: maps libfabric return codes,
//! completion-queue errors, and configuration / encoding failures into
//! a single enum the rest of the crate can handle uniformly.

use std::fmt;
use std::sync::Arc;

#[derive(Debug, Clone)]
pub enum FabricError {
    /// A libfabric API call returned a negative errno. `ctx` names the
    /// operation (e.g. "fi_getinfo") and `err` is the raw return code.
    Pkg(&'static str, i32),
    /// A completion queue surfaced an error entry.
    Cq { prov_errno: i32, err: i32 },
    /// Caller-side configuration violated an invariant.
    BadConfig(&'static str),
    /// Endpoint NUMA locality did not match the configured worker.
    NumaMismatch { expected: u16, got: u16 },
    /// Lookup of a peer / address / resource by name failed.
    NotFound(&'static str),
    /// Bounded wait elapsed without progress.
    Timeout,
    /// A higher-level encode/decode step failed. Boxed for object
    /// safety since downstream errors are not `Clone`.
    Encode(Arc<dyn std::error::Error + Send + Sync>),
}

pub type Result<T> = std::result::Result<T, FabricError>;

impl fmt::Display for FabricError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            FabricError::Pkg(ctx, err) => write!(f, "libfabric {ctx} failed: errno={err}"),
            FabricError::Cq { prov_errno, err } => {
                write!(f, "libfabric cq error: err={err} prov_errno={prov_errno}")
            }
            FabricError::BadConfig(msg) => write!(f, "fabric config invalid: {msg}"),
            FabricError::NumaMismatch { expected, got } => {
                write!(f, "fabric NUMA mismatch: expected={expected} got={got}")
            }
            FabricError::NotFound(what) => write!(f, "fabric resource not found: {what}"),
            FabricError::Timeout => write!(f, "fabric operation timed out"),
            FabricError::Encode(e) => write!(f, "fabric encode error: {e}"),
        }
    }
}

impl std::error::Error for FabricError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            FabricError::Encode(e) => Some(&**e),
            _ => None,
        }
    }
}

/// Convert a libfabric return code into a `Result`. Convention:
/// libfabric returns `0` on success, negative on error. Positive
/// returns (e.g. byte counts) are treated as success here; callers
/// that care about the value should inspect it before calling this.
pub fn check(ctx: &'static str, rc: i32) -> Result<()> {
    if rc < 0 {
        Err(FabricError::Pkg(ctx, rc))
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn check_accepts_zero() {
        assert!(check("op", 0).is_ok());
    }

    #[test]
    fn check_accepts_positive() {
        assert!(check("op", 7).is_ok());
    }

    #[test]
    fn check_rejects_negative_with_ctx() {
        let err = check("fi_getinfo", -22).unwrap_err();
        match err {
            FabricError::Pkg(ctx, rc) => {
                assert_eq!(ctx, "fi_getinfo");
                assert_eq!(rc, -22);
            }
            other => panic!("unexpected variant: {other:?}"),
        }
    }

    #[test]
    fn display_includes_ctx_for_pkg() {
        let err = FabricError::Pkg("fi_endpoint", -22);
        let rendered = format!("{err}");
        assert!(rendered.contains("fi_endpoint"));
        assert!(rendered.contains("-22"));
    }
}
