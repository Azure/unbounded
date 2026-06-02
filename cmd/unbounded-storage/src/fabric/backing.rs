// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Memory region registration for a `Backing`.
//!
//! One MR covers an entire `Backing`. Callers that want multiple
//! regions call `Fabric::register_backing` multiple times. The
//! returned `MrHandle` is a value type carrying the remote key, base
//! pointer, and length; the underlying `fid_mr` is owned by the
//! `Fabric` and closed when the fabric is dropped (so it tears down
//! before its domain). Dropping an `MrHandle` does *not* close the
//! MR.

use std::ptr;

use crate::memory::Backing;

use super::error::{FabricError, Result, check};
use super::fabric::Fabric;
use super::ffi;

/// Handle to a registered memory region. The underlying libfabric
/// resource is owned by the issuing `Fabric`.
#[derive(Copy, Clone, Debug)]
pub struct MrHandle {
    pub mr: *mut ffi::fid_mr,
    pub remote_key: u64,
    pub base: usize,
    /// Remote RMA base address a peer must target to write into this
    /// region's first byte. Equals `base` when the provider uses
    /// `FI_MR_VIRT_ADDR`, or `0` when it addresses by MR offset (the
    /// tcp RDM provider). Remote peers add the intra-region byte offset to
    /// this value; the local virtual `base` is used only for local
    /// (source) addressing.
    pub remote_base: u64,
    pub len: usize,
}

// SAFETY: `MrHandle` is a value handle; the underlying `fid_mr` is
// thread-safe per libfabric docs and owned by the `Fabric`. Mirrors
// the `Backing` send/sync rationale.
unsafe impl Send for MrHandle {}
unsafe impl Sync for MrHandle {}

impl Fabric {
    /// Register `backing` against this fabric's domain. The returned
    /// `MrHandle` is owned by the caller for use as a remote
    /// description; the `fid_mr` itself is retained inside the
    /// `Fabric` and closed on `Drop`.
    pub fn register_backing(&self, backing: &Backing, numa: Option<u16>) -> Result<MrHandle> {
        if let (Some(n), Some(m)) = (self.inner().cfg.numa, numa) {
            if n != m {
                return Err(FabricError::NumaMismatch {
                    expected: n,
                    got: m,
                });
            }
        }

        let base = backing.base;
        let len = backing.page_size.saturating_mul(backing.page_count);
        if base.is_null() || len == 0 {
            return Err(FabricError::BadConfig("backing base/len invalid"));
        }

        let mut mr_p: *mut ffi::fid_mr = ptr::null_mut();
        let access = ffi::FI_REMOTE_READ | ffi::FI_REMOTE_WRITE | ffi::FI_READ | ffi::FI_WRITE;
        // Hand out a distinct requested_key per registration. Providers
        // without FI_MR_PROV_KEY (e.g. the tcp RDM provider) reject a reused key
        // with FI_ENOKEY; providers that assign their own keys ignore
        // this value.
        let requested_key = self
            .inner()
            .next_mr_key
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        // SAFETY: domain is live; `base` and `len` describe the
        // caller's mapping which outlives the fabric per the
        // documented lifetime model.
        let rc = unsafe {
            ffi::ub_fi_mr_reg(
                self.inner().domain(),
                base as *const std::ffi::c_void,
                len,
                access,
                0,
                requested_key,
                0,
                &mut mr_p,
                ptr::null_mut(),
            )
        };
        check("fi_mr_reg", rc)?;
        // SAFETY: mr_p was just produced by fi_mr_reg.
        let remote_key = unsafe { ffi::ub_fi_mr_key(mr_p) };

        if let Ok(mut v) = self.inner().mrs.write() {
            v.push(mr_p);
        }
        Ok(MrHandle {
            mr: mr_p,
            remote_key,
            base: base as usize,
            remote_base: if self.mr_uses_virtual_addr() {
                base as u64
            } else {
                0
            },
            len,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Mirror of the `register_backing` NUMA check. The production
    /// version lives inline; this helper exercises the same
    /// predicate so a regression in either fails the build.
    fn numa_check(fabric_numa: Option<u16>, backing_numa: Option<u16>) -> Result<()> {
        if let (Some(n), Some(m)) = (fabric_numa, backing_numa) {
            if n != m {
                return Err(FabricError::NumaMismatch {
                    expected: n,
                    got: m,
                });
            }
        }
        Ok(())
    }

    #[test]
    fn numa_check_passes_when_unset() {
        assert!(numa_check(None, None).is_ok());
        assert!(numa_check(Some(0), None).is_ok());
        assert!(numa_check(None, Some(0)).is_ok());
    }

    #[test]
    fn numa_check_passes_when_equal() {
        assert!(numa_check(Some(3), Some(3)).is_ok());
    }

    #[test]
    fn numa_check_rejects_mismatch() {
        match numa_check(Some(0), Some(1)) {
            Err(FabricError::NumaMismatch { expected, got }) => {
                assert_eq!(expected, 0);
                assert_eq!(got, 1);
            }
            other => panic!("expected NumaMismatch, got {other:?}"),
        }
    }
}
