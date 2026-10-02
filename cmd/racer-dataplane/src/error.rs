//! Typed boundary failures. Error values contain no headers, tokens, or body bytes.

use std::{fmt, future::Future, pin::Pin};

pub type Result<T> = std::result::Result<T, Error>;

/// Worker-local future: deliberately not `Send`, and never drives a hidden executor.
pub type Operation<'a, T> = Pin<Box<dyn Future<Output = Result<T>> + 'a>>;

/// Yield one cooperative turn without retaining an executor or I/O owner.
pub(crate) async fn cooperative_turn() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            std::task::Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    })
    .await
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidConfiguration,
    InvalidRequest,
    MethodNotAllowed,
    HeaderTooLarge,
    InvalidRange,
    UnsatisfiableRange,
    UnsatisfiableRangeWithLength(u64),
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
}

impl fmt::Display for Error {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{self:?}")
    }
}

impl std::error::Error for Error {}

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

impl From<rest_client::Error> for Error {
    fn from(error: rest_client::Error) -> Self {
        match error {
            rest_client::Error::InvalidConfiguration => Self::InvalidConfiguration,
            rest_client::Error::InvalidRequest => Self::InvalidRequest,
            rest_client::Error::Unauthorized => Self::Unauthorized,
            rest_client::Error::Unavailable => Self::Unavailable,
            rest_client::Error::Overloaded => Self::Overloaded,
            rest_client::Error::Io => Self::Io,
            rest_client::Error::Internal => Self::Internal,
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
            page_alloc::Error::Unavailable => Self::Unavailable,
            page_alloc::Error::Io => Self::Io,
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
        }
    }
}

#[cfg(test)]
mod tests {
    use super::Error;

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
            (AllocError::Unavailable, Error::Unavailable),
            (AllocError::Io, Error::Io),
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
        ] {
            assert_eq!(Error::from(runtime), racer);
        }
    }
}
