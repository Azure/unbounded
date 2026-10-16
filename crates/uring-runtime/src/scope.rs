//! Caller-defined cancellation and deadline scopes.
//!
//! Every reactor operation takes a [`Scope`]. The reactor calls
//! [`Scope::check`] before submission and during its bounded cancellation
//! scan, and registers with [`Scope::cancellation`] so that canceling a token
//! wakes the waiting task promptly instead of at the next scan.
//!
//! ```
//! use uring_runtime::{CancelToken, Scope, Unscoped};
//!
//! assert!(Unscoped.check().is_ok());
//!
//! let token = CancelToken::new();
//! token.cancel();
//! assert!(token.check().unwrap_err().is_canceled());
//! ```

use std::time::Instant;

use crate::{CancelToken, Error, ErrorKind};

/// A cancellation and deadline context for one or more operations.
///
/// Implementations are application types: a request context, a connection
/// lifetime, a shutdown signal. The runtime never reads a clock on behalf of a
/// scope: if a scope has a deadline, its [`check`](Scope::check) implementation
/// is responsible for comparing it with the clock it uses and returning an
/// error once it has passed. [`deadline`](Scope::deadline) is a hint the
/// reactor may use to arm a timer so that `check` is re-evaluated promptly.
pub trait Scope: Clone + 'static {
    /// The error returned by operations performed under this scope.
    ///
    /// Runtime failures are converted with `From<Error>`.
    type Error: From<Error> + 'static;

    /// Return an error if work under this scope should stop.
    fn check(&self) -> Result<(), Self::Error>;

    /// A token whose cancellation should immediately wake waiting operations.
    fn cancellation(&self) -> Option<&CancelToken> {
        None
    }

    /// The instant after which [`check`](Scope::check) is expected to fail.
    fn deadline(&self) -> Option<Instant> {
        None
    }
}

/// A scope that never cancels and has no deadline.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub struct Unscoped;

impl Scope for Unscoped {
    type Error = Error;

    fn check(&self) -> Result<(), Error> {
        Ok(())
    }
}

/// A token is a scope that fails with [`ErrorKind::Canceled`] once canceled.
impl Scope for CancelToken {
    type Error = Error;

    fn check(&self) -> Result<(), Error> {
        if self.is_canceled() {
            Err(Error::new(ErrorKind::Canceled))
        } else {
            Ok(())
        }
    }

    fn cancellation(&self) -> Option<&CancelToken> {
        Some(self)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Clone)]
    struct Custom(CancelToken);

    #[derive(Debug, PartialEq)]
    enum AppError {
        Runtime(ErrorKind),
        Stopped,
    }

    impl From<Error> for AppError {
        fn from(error: Error) -> Self {
            Self::Runtime(error.kind())
        }
    }

    impl Scope for Custom {
        type Error = AppError;

        fn check(&self) -> Result<(), AppError> {
            if self.0.is_canceled() {
                Err(AppError::Stopped)
            } else {
                Ok(())
            }
        }
    }

    #[test]
    fn unscoped_never_fails() {
        assert!(Unscoped.check().is_ok());
        assert!(Unscoped.cancellation().is_none());
        assert!(Unscoped.deadline().is_none());
    }

    #[test]
    fn token_scope() {
        let token = CancelToken::new();
        assert!(token.check().is_ok());
        assert!(token.cancellation().is_some());
        token.cancel();
        assert_eq!(token.check().unwrap_err().kind(), ErrorKind::Canceled);
    }

    #[test]
    fn custom_error_type() {
        let scope = Custom(CancelToken::new());
        assert!(scope.check().is_ok());
        assert!(scope.cancellation().is_none());
        scope.0.cancel();
        assert_eq!(scope.check(), Err(AppError::Stopped));
        let converted: AppError = Error::new(ErrorKind::Overloaded).into();
        assert_eq!(converted, AppError::Runtime(ErrorKind::Overloaded));
    }
}
