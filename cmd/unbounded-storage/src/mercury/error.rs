// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Errors returned by the Mercury transport. Mercury returns `int`
//! status codes from every C entry point; we wrap the failure path
//! in a single struct that implements `std::error::Error` so it
//! flows cleanly through `bufferpool::Error::transport`.

use std::fmt;

use crate::mercury::ffi;

/// Result alias for the Mercury transport.
pub type Result<T> = std::result::Result<T, HgError>;

/// Failure returned from a Mercury entry point. `code` is the raw
/// `hg_return_t`; `ctx` names the call site so log messages identify
/// which step failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HgError {
    pub code: i32,
    pub ctx: &'static str,
}

impl HgError {
    pub fn new(code: i32, ctx: &'static str) -> Self {
        Self { code, ctx }
    }
}

impl fmt::Display for HgError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "mercury {ctx} failed: code {code}",
            ctx = self.ctx,
            code = self.code
        )
    }
}

impl std::error::Error for HgError {}

/// Translate a Mercury return code into `Result<()>`. Use at every
/// FFI boundary; the `ctx` argument is a static string identifying
/// the caller so error messages are debuggable without backtraces.
pub fn check(ret: ffi::hg_return_t, ctx: &'static str) -> Result<()> {
    if ret == ffi::HG_SUCCESS {
        Ok(())
    } else {
        Err(HgError::new(ret as i32, ctx))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn check_success_returns_ok() {
        assert!(check(ffi::HG_SUCCESS, "ctx").is_ok());
    }

    #[test]
    fn check_nonzero_returns_error_with_code_and_ctx() {
        let e = check(-7, "did-fail").unwrap_err();
        assert_eq!(e.code, -7);
        assert_eq!(e.ctx, "did-fail");
    }

    #[test]
    fn display_includes_code_and_ctx() {
        let e = HgError::new(42, "site");
        let s = format!("{e}");
        assert!(s.contains("42"), "missing code: {s}");
        assert!(s.contains("site"), "missing ctx: {s}");
    }

    #[test]
    fn error_implements_std_error() {
        fn assert_err<E: std::error::Error>(_: &E) {}
        assert_err(&HgError::new(0, "x"));
    }

    #[test]
    fn equality_is_by_code_and_ctx() {
        assert_eq!(HgError::new(1, "a"), HgError::new(1, "a"));
        assert_ne!(HgError::new(1, "a"), HgError::new(2, "a"));
        assert_ne!(HgError::new(1, "a"), HgError::new(1, "b"));
    }
}
