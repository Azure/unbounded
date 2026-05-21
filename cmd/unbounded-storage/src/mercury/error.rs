// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Mercury error type and result alias.
//!
//! `HgError` enumerates the failure modes the Mercury transport can surface:
//! one variant per Mercury entry point that returns `hg_return_t`, plus a
//! handful of higher-level codec/protocol errors. The `check` helper turns a
//! raw `hg_return_t` into a `Result<()>` using a caller-supplied constructor,
//! which keeps call sites free of boilerplate `match` on `HG_SUCCESS`.

use std::fmt;

use crate::mercury::ffi::HG_SUCCESS;

#[derive(Debug)]
pub enum HgError {
    HgInit(i32),
    HgFinalize(i32),
    HgRegister(i32),
    HgAddrLookup(i32),
    HgCreate(i32),
    HgForward(i32),
    HgRespond(i32),
    HgBulkCreate(i32),
    HgBulkTransfer(i32),
    HgGetInput(i32),
    HgFreeInput(i32),
    HgGetOutput(i32),
    HgFreeOutput(i32),
    Encode(&'static str),
    Decode(&'static str),
    ShortRead { expected: u32, got: u32 },
    Closed,
    Capacity,
    BadConfig(&'static str),
}

pub type Result<T> = std::result::Result<T, HgError>;

impl fmt::Display for HgError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            HgError::HgInit(rc) => write!(f, "HG_Init failed: rc={rc}"),
            HgError::HgFinalize(rc) => write!(f, "HG_Finalize failed: rc={rc}"),
            HgError::HgRegister(rc) => write!(f, "HG_Register failed: rc={rc}"),
            HgError::HgAddrLookup(rc) => write!(f, "HG_Addr_lookup failed: rc={rc}"),
            HgError::HgCreate(rc) => write!(f, "HG_Create failed: rc={rc}"),
            HgError::HgForward(rc) => write!(f, "HG_Forward failed: rc={rc}"),
            HgError::HgRespond(rc) => write!(f, "HG_Respond failed: rc={rc}"),
            HgError::HgBulkCreate(rc) => write!(f, "HG_Bulk_create failed: rc={rc}"),
            HgError::HgBulkTransfer(rc) => write!(f, "HG_Bulk_transfer failed: rc={rc}"),
            HgError::HgGetInput(rc) => write!(f, "HG_Get_input failed: rc={rc}"),
            HgError::HgFreeInput(rc) => write!(f, "HG_Free_input failed: rc={rc}"),
            HgError::HgGetOutput(rc) => write!(f, "HG_Get_output failed: rc={rc}"),
            HgError::HgFreeOutput(rc) => write!(f, "HG_Free_output failed: rc={rc}"),
            HgError::Encode(s) => write!(f, "encode error: {s}"),
            HgError::Decode(s) => write!(f, "decode error: {s}"),
            HgError::ShortRead { expected, got } => {
                write!(f, "short read: expected {expected} bytes, got {got}")
            }
            HgError::Closed => write!(f, "transport closed"),
            HgError::Capacity => write!(f, "transport at capacity"),
            HgError::BadConfig(s) => write!(f, "bad config: {s}"),
        }
    }
}

impl std::error::Error for HgError {}

impl From<HgError> for crate::bufferpool::Error {
    fn from(e: HgError) -> Self {
        crate::bufferpool::Error::transport(e)
    }
}

/// Convert a raw `hg_return_t` into `Result<()>`. On `HG_SUCCESS` returns
/// `Ok(())`; otherwise builds the error via `into_err(rc)`.
///
/// Usage: `check(rc, HgError::HgInit)?;`
pub(crate) fn check<F>(rc: i32, into_err: F) -> Result<()>
where
    F: FnOnce(i32) -> HgError,
{
    if rc == HG_SUCCESS {
        Ok(())
    } else {
        Err(into_err(rc))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn all_variants() -> Vec<HgError> {
        vec![
            HgError::HgInit(1),
            HgError::HgFinalize(2),
            HgError::HgRegister(3),
            HgError::HgAddrLookup(4),
            HgError::HgCreate(5),
            HgError::HgForward(6),
            HgError::HgRespond(7),
            HgError::HgBulkCreate(8),
            HgError::HgBulkTransfer(9),
            HgError::HgGetInput(10),
            HgError::HgFreeInput(11),
            HgError::HgGetOutput(12),
            HgError::HgFreeOutput(13),
            HgError::Encode("enc"),
            HgError::Decode("dec"),
            HgError::ShortRead {
                expected: 16,
                got: 8,
            },
            HgError::Closed,
            HgError::Capacity,
            HgError::BadConfig("cfg"),
        ]
    }

    #[test]
    fn display_is_non_empty_for_every_variant() {
        for v in all_variants() {
            let s = format!("{v}");
            assert!(!s.is_empty(), "empty display for {v:?}");
        }
    }

    #[test]
    fn check_success_is_ok() {
        assert!(matches!(check(HG_SUCCESS, HgError::HgInit), Ok(())));
    }

    #[test]
    fn check_failure_builds_error() {
        let r = check(1, HgError::HgInit);
        match r {
            Err(HgError::HgInit(1)) => {}
            other => panic!("expected Err(HgInit(1)), got {other:?}"),
        }
    }

    #[test]
    fn into_bufferpool_error_is_transport() {
        let e: crate::bufferpool::Error = HgError::Closed.into();
        match e {
            crate::bufferpool::Error::Transport(_) => {}
            other => panic!("expected Transport(_), got {other:?}"),
        }
    }
}
