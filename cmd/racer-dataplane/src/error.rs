//! Typed boundary failures. Error values contain no headers, tokens, or body bytes.

use std::fmt;

pub type Result<T> = std::result::Result<T, Error>;

/// Worker-local future: deliberately not `Send`, and never drives a hidden executor.
pub type Operation<'a, T> = uring_runtime::Operation<'a, T, Error>;

/// Declare root failures once, keeping publication phases out of their own causes.
macro_rules! boundary_errors {
    ($( $(#[$attribute:meta])* $variant:ident $(($binding:ident: $payload:ty))? ),* $(,)?) => {
        #[derive(Clone, Copy, Debug, Eq, PartialEq)]
        pub enum Error {
            $( $(#[$attribute])* $variant $(($payload))?, )*
            /// Rename completion did not certify publication. Reconcile first.
            RenameUncertain(PublicationCause),
            /// Publication occurred, but directory durability was not certified.
            PublishedNotDurable(PublicationCause),
        }

        /// Allocation-free root failure, preserving the outer publication phase.
        #[derive(Clone, Copy, Debug, Eq, PartialEq)]
        pub enum PublicationCause {
            $( $(#[$attribute])* $variant $(($payload))?, )*
        }

        impl From<Error> for PublicationCause {
            fn from(error: Error) -> Self {
                match error {
                    $( Error::$variant $(($binding))? => Self::$variant $(($binding))?, )*
                    Error::RenameUncertain(cause) | Error::PublishedNotDurable(cause) => cause,
                }
            }
        }
    };
}

boundary_errors! {
    InvalidConfiguration,
    InvalidRequest,
    MethodNotAllowed,
    HeaderTooLarge,
    InvalidRange,
    UnsatisfiableRange,
    UnsatisfiableRangeWithLength(length: u64),
    NotFound,
    Forbidden,
    BadGateway,
    Internal,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    DeadlineExceeded,
    Cancelled,
    StaleFlight,
    Unauthorized,
    /// Authenticated Node binding changed; drain the entire graph and restart.
    NodeIdentityChanged,
    /// The origin rejected only the credentials of the supplying caller.
    OriginRejected,
    /// The origin forbids this caller; this is not evidence of page absence.
    OriginForbidden,
    Replay,
    IncompatibleMembership,
    HopBudgetExhausted,
    CorruptRecord,
    MissingKey,
    DirectIoUnsupported,
    Io,
    Os(errno: i32),
}

impl fmt::Display for Error {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{self:?}")
    }
}

impl std::error::Error for Error {}

impl From<controlplane::Error> for Error {
    fn from(error: controlplane::Error) -> Self {
        match error {
            controlplane::Error::Replay => Self::Replay,
            controlplane::Error::Conflict => Self::IncompatibleMembership,
            controlplane::Error::Capacity => Self::Overloaded,
            controlplane::Error::Stale => Self::StaleFlight,
            controlplane::Error::Pending => Self::Unavailable,
            controlplane::Error::Internal => Self::Internal,
        }
    }
}

impl From<racer_crypto::identity::BundleError> for Error {
    fn from(error: racer_crypto::identity::BundleError) -> Self {
        match error {
            racer_crypto::identity::BundleError::Replay => Self::Replay,
            racer_crypto::identity::BundleError::Wire(error) => error.into(),
            racer_crypto::identity::BundleError::Identity(error) => error.into(),
        }
    }
}

impl From<racer_crypto::enrollment::Error> for Error {
    fn from(error: racer_crypto::enrollment::Error) -> Self {
        match error {
            racer_crypto::enrollment::Error::Unauthorized => Self::Unauthorized,
            racer_crypto::enrollment::Error::CorruptRecord => Self::CorruptRecord,
            racer_crypto::enrollment::Error::InvalidConfiguration => Self::InvalidConfiguration,
            racer_crypto::enrollment::Error::Io => Self::Io,
            racer_crypto::enrollment::Error::Wire(error) => error.into(),
        }
    }
}

impl From<uring_runtime::reactor::filesystem::secure::AccessError> for Error {
    fn from(error: uring_runtime::reactor::filesystem::secure::AccessError) -> Self {
        use uring_runtime::reactor::filesystem::secure::AccessError;
        match error {
            AccessError::MissingMetadata => Self::Io,
            AccessError::PermissionDenied => Self::Unauthorized,
        }
    }
}

impl From<racer_object_wire::Error> for Error {
    fn from(error: racer_object_wire::Error) -> Self {
        use racer_object_wire::Error as Wire;
        match error {
            Wire::InvalidRequest => Self::InvalidRequest,
            Wire::MethodNotAllowed => Self::MethodNotAllowed,
            Wire::HeaderTooLarge => Self::HeaderTooLarge,
            Wire::InvalidRange => Self::InvalidRange,
            Wire::UnsatisfiableRange => Self::UnsatisfiableRange,
            Wire::UnsatisfiableRangeWithLength(length) => {
                Self::UnsatisfiableRangeWithLength(length)
            }
            Wire::NotFound => Self::NotFound,
            Wire::BadGateway => Self::BadGateway,
            Wire::Internal => Self::Internal,
            Wire::VersionUnavailable => Self::VersionUnavailable,
            Wire::Unavailable => Self::Unavailable,
            Wire::OriginRejected => Self::OriginRejected,
            Wire::OriginForbidden => Self::OriginForbidden,
            Wire::CorruptRecord => Self::CorruptRecord,
        }
    }
}

impl From<flow_control::Error> for Error {
    fn from(error: flow_control::Error) -> Self {
        match error {
            flow_control::Error::InvalidInput => Self::InvalidConfiguration,
            flow_control::Error::Overloaded => Self::Overloaded,
            flow_control::Error::Unavailable => Self::Unavailable,
            flow_control::Error::Io => Self::Io,
        }
    }
}

impl From<wire_codec::rest::Error> for Error {
    fn from(error: wire_codec::rest::Error) -> Self {
        match error {
            wire_codec::rest::Error::InvalidConfiguration => Self::InvalidConfiguration,
            wire_codec::rest::Error::InvalidRequest => Self::InvalidRequest,
            wire_codec::rest::Error::Unauthorized => Self::Unauthorized,
            wire_codec::rest::Error::Unavailable => Self::Unavailable,
            wire_codec::rest::Error::Overloaded => Self::Overloaded,
            wire_codec::rest::Error::Io => Self::Io,
            wire_codec::rest::Error::Internal => Self::Internal,
        }
    }
}

impl From<rdma_verbs::Error> for Error {
    fn from(error: rdma_verbs::Error) -> Self {
        match error {
            rdma_verbs::Error::InvalidConfiguration => Self::InvalidConfiguration,
            rdma_verbs::Error::InvalidRequest => Self::InvalidRequest,
            rdma_verbs::Error::InvalidRange => Self::InvalidRange,
            rdma_verbs::Error::Unavailable => Self::Unavailable,
            rdma_verbs::Error::Overloaded => Self::Overloaded,
            rdma_verbs::Error::DeadlineExceeded => Self::DeadlineExceeded,
            rdma_verbs::Error::Cancelled => Self::Cancelled,
            rdma_verbs::Error::Io => Self::Io,
        }
    }
}

impl From<http1::Error> for Error {
    fn from(error: http1::Error) -> Self {
        match error {
            http1::Error::Malformed => Self::InvalidRequest,
            http1::Error::HeadTooLarge => Self::HeaderTooLarge,
        }
    }
}

impl From<page_alloc::Error> for Error {
    fn from(error: page_alloc::Error) -> Self {
        match error {
            page_alloc::Error::Unsupported => Self::DirectIoUnsupported,
            page_alloc::Error::InvalidConfiguration => Self::InvalidConfiguration,
            page_alloc::Error::Busy => Self::Overloaded,
            page_alloc::Error::Corrupt => Self::CorruptRecord,
            page_alloc::Error::Stale | page_alloc::Error::Unavailable => Self::Unavailable,
            page_alloc::Error::Io => Self::Io,
            page_alloc::Error::SystemIo { .. } => {
                eprintln!("racer: allocator: {error}");
                Self::Io
            }
            _ => Self::Internal,
        }
    }
}

impl From<uring_runtime::Error> for Error {
    fn from(error: uring_runtime::Error) -> Self {
        match error {
            uring_runtime::Error::Cancelled => Self::Cancelled,
            uring_runtime::Error::DeadlineExceeded => Self::DeadlineExceeded,
            uring_runtime::Error::Overloaded => Self::Overloaded,
            uring_runtime::Error::Unavailable => Self::Unavailable,
            uring_runtime::Error::InvalidConfiguration => Self::InvalidConfiguration,
            uring_runtime::Error::InvalidInput => Self::InvalidRequest,
            uring_runtime::Error::NotFound => Self::MissingKey,
            uring_runtime::Error::AlreadyExists => Self::Replay,
            uring_runtime::Error::Io => Self::Io,
            uring_runtime::Error::Os(errno) => Self::Os(errno),
        }
    }
}

impl From<uring_runtime::reactor::filesystem::operations::ReplacementError<Error>> for Error {
    fn from(
        error: uring_runtime::reactor::filesystem::operations::ReplacementError<Error>,
    ) -> Self {
        use uring_runtime::reactor::filesystem::operations::ReplacementError;
        match error {
            ReplacementError::BeforeRename(cause) => cause,
            ReplacementError::RenameUncertain(cause) => Self::RenameUncertain(cause.into()),
            ReplacementError::Published(cause) => Self::PublishedNotDurable(cause.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    /// Generic HTTP syntax errors retain the application's response classifications.
    #[test]
    fn http_errors_keep_racer_boundary_meanings() {
        assert_eq!(
            super::Error::from(http1::Error::Malformed),
            super::Error::InvalidRequest
        );
        assert_eq!(
            super::Error::from(http1::Error::HeadTooLarge),
            super::Error::HeaderTooLarge
        );
    }
    /// Declaration sharing must retain Debug-based display and flatten either phase.
    #[test]
    fn publication_causes_and_display_preserve_payloads() {
        use super::{Error, PublicationCause};
        for (error, cause, text) in [
            (Error::Cancelled, PublicationCause::Cancelled, "Cancelled"),
            (Error::Os(-1), PublicationCause::Os(-1), "Os(-1)"),
            (
                Error::UnsatisfiableRangeWithLength(u64::MAX),
                PublicationCause::UnsatisfiableRangeWithLength(u64::MAX),
                "UnsatisfiableRangeWithLength(18446744073709551615)",
            ),
        ] {
            assert_eq!(error.to_string(), text);
            assert_eq!(PublicationCause::from(error), cause);
            assert_eq!(PublicationCause::from(Error::RenameUncertain(cause)), cause);
            assert_eq!(
                PublicationCause::from(Error::PublishedNotDurable(cause)),
                cause
            );
            assert_eq!(
                Error::RenameUncertain(cause).to_string(),
                format!("RenameUncertain({text})")
            );
            assert_eq!(
                Error::PublishedNotDurable(cause).to_string(),
                format!("PublishedNotDurable({text})")
            );
        }
    }
    use super::Error;
    use super::Operation;

    /// Foundation errors retain authentication and wire distinctions.
    #[test]
    fn control_foundation_errors_preserve_boundary_meanings() {
        use racer_crypto::enrollment::Error as Enrollment;
        use racer_crypto::identity::BundleError;
        use uring_runtime::reactor::filesystem::secure::AccessError;
        for (source, expected) in [
            (controlplane::Error::Replay, Error::Replay),
            (controlplane::Error::Conflict, Error::IncompatibleMembership),
            (controlplane::Error::Capacity, Error::Overloaded),
            (controlplane::Error::Stale, Error::StaleFlight),
            (controlplane::Error::Pending, Error::Unavailable),
            (controlplane::Error::Internal, Error::Internal),
        ] {
            assert_eq!(Error::from(source), expected);
        }
        for (source, expected) in [
            (Enrollment::Unauthorized, Error::Unauthorized),
            (Enrollment::CorruptRecord, Error::CorruptRecord),
            (
                Enrollment::InvalidConfiguration,
                Error::InvalidConfiguration,
            ),
            (Enrollment::Io, Error::Io),
        ] {
            assert_eq!(Error::from(source), expected);
        }
        for wire in [
            racer_control_wire::Error::InvalidRequest,
            racer_control_wire::Error::IncompatibleMembership,
            racer_control_wire::Error::Overloaded,
            racer_control_wire::Error::Replay,
        ] {
            assert_eq!(Error::from(Enrollment::Wire(wire)), Error::from(wire));
            assert_eq!(Error::from(BundleError::Wire(wire)), Error::from(wire));
        }
        assert_eq!(Error::from(BundleError::Replay), Error::Replay);
        assert_eq!(
            Error::from(BundleError::Identity(
                racer_crypto::identity::Error::Unauthorized
            )),
            Error::Unauthorized
        );
        assert_eq!(Error::from(AccessError::MissingMetadata), Error::Io);
        assert_eq!(
            Error::from(AccessError::PermissionDenied),
            Error::Unauthorized
        );
    }

    /// Wire failures retain their exact application taxonomy at the adapter boundary.
    #[test]
    fn object_wire_errors_keep_racer_boundary_meanings() {
        use racer_object_wire::Error as Wire;
        for (wire, app) in [
            (Wire::InvalidRequest, Error::InvalidRequest),
            (Wire::MethodNotAllowed, Error::MethodNotAllowed),
            (Wire::HeaderTooLarge, Error::HeaderTooLarge),
            (Wire::InvalidRange, Error::InvalidRange),
            (Wire::UnsatisfiableRange, Error::UnsatisfiableRange),
            (
                Wire::UnsatisfiableRangeWithLength(0),
                Error::UnsatisfiableRangeWithLength(0),
            ),
            (
                Wire::UnsatisfiableRangeWithLength(u64::MAX),
                Error::UnsatisfiableRangeWithLength(u64::MAX),
            ),
            (Wire::NotFound, Error::NotFound),
            (Wire::BadGateway, Error::BadGateway),
            (Wire::Internal, Error::Internal),
            (Wire::VersionUnavailable, Error::VersionUnavailable),
            (Wire::Unavailable, Error::Unavailable),
            (Wire::OriginRejected, Error::OriginRejected),
            (Wire::OriginForbidden, Error::OriginForbidden),
            (Wire::CorruptRecord, Error::CorruptRecord),
        ] {
            assert_eq!(Error::from(wire), app);
            assert_eq!(
                super::PublicationCause::from(Error::from(wire)),
                super::PublicationCause::from(app)
            );
        }
    }

    #[test]
    fn operation_preserves_local_borrows_and_racer_results() {
        let value = std::rc::Rc::new(7);
        let operation: Operation<'_, _> = Box::pin(async { Ok(*value) });
        let operation: uring_runtime::Operation<'_, _, Error> = operation;
        assert_eq!(futures::executor::block_on(operation), Ok(7));
        let operation: Operation<'_, ()> = Box::pin(async { Err(Error::HopBudgetExhausted) });
        assert_eq!(
            futures::executor::block_on(operation),
            Err(Error::HopBudgetExhausted)
        );
    }

    #[test]
    fn quota_errors_keep_racer_boundary_meanings() {
        for (flow, racer) in [
            (
                flow_control::Error::InvalidInput,
                Error::InvalidConfiguration,
            ),
            (flow_control::Error::Overloaded, Error::Overloaded),
            (flow_control::Error::Unavailable, Error::Unavailable),
            (flow_control::Error::Io, Error::Io),
        ] {
            assert_eq!(Error::from(flow), racer);
        }
    }

    #[test]
    fn verbs_errors_keep_racer_boundary_meanings() {
        use rdma_verbs::Error as Verbs;
        for (verbs, racer) in [
            (Verbs::InvalidConfiguration, Error::InvalidConfiguration),
            (Verbs::InvalidRequest, Error::InvalidRequest),
            (Verbs::InvalidRange, Error::InvalidRange),
            (Verbs::Unavailable, Error::Unavailable),
            (Verbs::Overloaded, Error::Overloaded),
            (Verbs::DeadlineExceeded, Error::DeadlineExceeded),
            (Verbs::Cancelled, Error::Cancelled),
            (Verbs::Io, Error::Io),
        ] {
            assert_eq!(Error::from(verbs), racer);
        }
    }

    #[test]
    fn allocator_errors_keep_racer_boundary_meanings() {
        use page_alloc::Error as AllocError;
        for (allocator, racer) in [
            (AllocError::Unsupported, Error::DirectIoUnsupported),
            (
                AllocError::InvalidConfiguration,
                Error::InvalidConfiguration,
            ),
            (AllocError::Busy, Error::Overloaded),
            (AllocError::Corrupt, Error::CorruptRecord),
            (AllocError::Stale, Error::Unavailable),
            (AllocError::Unavailable, Error::Unavailable),
            (AllocError::Io, Error::Io),
            (
                AllocError::SystemIo {
                    operation: "open",
                    errno: Some(libc::EACCES),
                },
                Error::Io,
            ),
            (
                AllocError::SystemIo {
                    operation: "statx",
                    errno: None,
                },
                Error::Io,
            ),
        ] {
            assert_eq!(Error::from(allocator), racer);
        }
    }

    #[test]
    fn runtime_errors_keep_racer_boundary_meanings() {
        use uring_runtime::Error as RuntimeError;
        for (runtime, racer) in [
            (RuntimeError::Cancelled, Error::Cancelled),
            (RuntimeError::DeadlineExceeded, Error::DeadlineExceeded),
            (RuntimeError::Overloaded, Error::Overloaded),
            (RuntimeError::Unavailable, Error::Unavailable),
            (
                RuntimeError::InvalidConfiguration,
                Error::InvalidConfiguration,
            ),
            (RuntimeError::InvalidInput, Error::InvalidRequest),
            (RuntimeError::NotFound, Error::MissingKey),
            (RuntimeError::AlreadyExists, Error::Replay),
            (RuntimeError::Io, Error::Io),
            (RuntimeError::Os(libc::EIO), Error::Os(libc::EIO)),
        ] {
            assert_eq!(Error::from(runtime), racer);
        }
    }

    #[test]
    fn replacement_outcomes_preserve_publication_phase() {
        use uring_runtime::reactor::filesystem::operations::ReplacementError;
        assert_eq!(
            Error::from(ReplacementError::BeforeRename(Error::Os(libc::ENOSPC))),
            Error::Os(libc::ENOSPC)
        );
        assert_eq!(
            Error::from(ReplacementError::RenameUncertain(Error::Cancelled)),
            Error::RenameUncertain(super::PublicationCause::Cancelled)
        );
        assert_eq!(
            Error::from(ReplacementError::Published(Error::Os(libc::EIO))),
            Error::PublishedNotDurable(super::PublicationCause::Os(libc::EIO))
        );
        assert_eq!(
            Error::from(ReplacementError::Published(Error::RenameUncertain(
                super::PublicationCause::DeadlineExceeded,
            ))),
            Error::PublishedNotDurable(super::PublicationCause::DeadlineExceeded)
        );
    }
}
