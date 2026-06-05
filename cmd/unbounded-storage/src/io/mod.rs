// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared async-IO completion vocabulary spanning the crate's two
//! asynchronous IO backends: the io_uring engine ([`crate::ring`]) and
//! the libfabric engine ([`crate::fabric`]).
//!
//! The two backends are intentionally NOT one type. io_uring is
//! `!Send` (single-core, `Rc`/`RefCell`, op identity is a monotonic
//! `user_data` into a per-thread slot map); libfabric is `Send + Sync`
//! (`Arc`/atomics, op identity is a boxed `op_context` pointer handed
//! to the provider, completed cross-thread by a progress thread). That
//! divergence is deliberate and is preserved.
//!
//! What this module unifies is the *contract* both backends already
//! honor, expressed as a small shared vocabulary:
//!
//!  * [`IoResult`] / [`Completed`] / [`IoError`] - the one result type
//!    the io_uring `i32` CQE `res` and the libfabric `CompletionInfo`
//!    both collapse to at the consumer boundary.
//!  * [`CompletionOutcome`] - implemented by each backend's raw
//!    completion value so it can collapse to [`IoResult`].
//!  * [`Unified`] - an adapter future that drives any backend
//!    completion future and yields [`IoResult`]. Wrapping does not
//!    change drop behavior, so each backend's "dropping the future
//!    does NOT cancel the in-flight op" contract is preserved exactly.
//!  * [`BackPressure`] / [`BackPressurePolicy`] - names the admission
//!    policy each backend uses (io_uring parks; libfabric fails on a
//!    full registry) and exposes a uniform introspection surface.
//!
//! The shared types are opt-in: backend-specific detail (raw `i32`,
//! `CompletionInfo` with its tag/src_addr, the parking `SubmitSlot`,
//! the capacity-checked `allocate`) stays available where a caller
//! needs it. This module is the common surface, not a replacement.

use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

/// Successful completion outcome at the unified boundary. `bytes` is
/// the transferred byte count (io_uring's non-negative CQE `res`, or
/// libfabric's `CompletionInfo::bytes`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Completed {
    pub bytes: usize,
}

/// Unified failure at the boundary. Backend-specific richness is
/// preserved as far as it maps cleanly; anything that does not is
/// rendered into [`IoError::Other`].
#[derive(Debug, Clone)]
pub enum IoError {
    /// An OS errno. The io_uring path produces these (a negative CQE
    /// `res` of `-errno` maps to `Os(errno)`).
    Os(i32),
    /// A provider/completion-queue error carrying the provider errno
    /// alongside the libfabric error code.
    Provider { prov_errno: i32, err: i32 },
    /// Any other backend error, rendered to a message. Used for the
    /// libfabric error variants that have no numeric boundary form.
    Other(String),
}

impl fmt::Display for IoError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            IoError::Os(errno) => write!(f, "io error: errno={errno}"),
            IoError::Provider { prov_errno, err } => {
                write!(f, "io provider error: err={err} prov_errno={prov_errno}")
            }
            IoError::Other(msg) => write!(f, "io error: {msg}"),
        }
    }
}

impl std::error::Error for IoError {}

/// Unified result both backends collapse to at the consumer boundary.
pub type IoResult = Result<Completed, IoError>;

/// A backend's raw completion value that can collapse to the unified
/// [`IoResult`]. Implemented for the io_uring `i32` CQE `res` here, and
/// for the libfabric `Result<CompletionInfo>` next to that type.
pub trait CompletionOutcome {
    fn into_io_result(self) -> IoResult;
}

impl CompletionOutcome for i32 {
    /// io_uring convention: negative is `-errno`, non-negative is a
    /// byte count (or other success value).
    fn into_io_result(self) -> IoResult {
        if self < 0 {
            Err(IoError::Os(-self))
        } else {
            Ok(Completed {
                bytes: self as usize,
            })
        }
    }
}

/// Adapter future that drives any backend completion future and
/// collapses its backend-specific output to the unified [`IoResult`].
///
/// Wrapping is semantically transparent: dropping a `Unified` drops
/// the inner future, so whatever drop behavior the backend defines
/// (io_uring issues a best-effort cancel and keeps the slot alive
/// until reaped; libfabric lets the result land unwatched) is
/// preserved unchanged. In particular, dropping a `Unified` does NOT
/// cancel the in-flight operation.
pub struct Unified<F> {
    inner: F,
}

impl<F> Unified<F> {
    pub fn new(inner: F) -> Self {
        Self { inner }
    }

    /// Recover the wrapped backend future.
    pub fn into_inner(self) -> F {
        self.inner
    }
}

impl<F> Future for Unified<F>
where
    F: Future + Unpin,
    F::Output: CompletionOutcome,
{
    type Output = IoResult;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        Pin::new(&mut self.inner)
            .poll(cx)
            .map(CompletionOutcome::into_io_result)
    }
}

/// How a backend admits new in-flight operations. Naming the policy
/// keeps the divergence explicit at the shared boundary; the actual
/// admission still lives in each backend (io_uring's `SubmitSlot`
/// park, libfabric's capacity-checked `allocate`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BackPressurePolicy {
    /// Submitters PARK until an in-flight op completes and frees a
    /// slot. Used by the io_uring engine, whose ring depth is a hard
    /// kernel bound.
    Parking,
    /// Admission FAILS immediately once the in-flight count reaches
    /// capacity. Used by the libfabric engine, whose registry returns
    /// an error the transport surfaces as back-pressure.
    Capacity,
}

/// Uniform introspection over a backend's admission control. This
/// describes the bound and the live count; it does not itself admit or
/// release (each backend owns that, to keep its counters authoritative).
pub trait BackPressure {
    /// Maximum number of simultaneously in-flight operations.
    fn capacity(&self) -> usize;

    /// Operations currently submitted and not yet reaped.
    fn in_flight(&self) -> usize;

    /// Which admission policy this backend applies on overflow.
    fn policy(&self) -> BackPressurePolicy;

    /// Remaining admission headroom.
    fn available(&self) -> usize {
        self.capacity().saturating_sub(self.in_flight())
    }

    /// Whether one more operation would be admitted right now without
    /// parking (Parking policy) or erroring (Capacity policy).
    fn admits(&self) -> bool {
        self.in_flight() < self.capacity()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    #[test]
    fn i32_outcome_maps_sign() {
        match 4096_i32.into_io_result() {
            Ok(Completed { bytes }) => assert_eq!(bytes, 4096),
            Err(e) => panic!("expected Ok, got {e}"),
        }
        match (-libc::EIO).into_io_result() {
            Err(IoError::Os(errno)) => assert_eq!(errno, libc::EIO),
            other => panic!("expected Os errno, got {other:?}"),
        }
        match 0_i32.into_io_result() {
            Ok(Completed { bytes }) => assert_eq!(bytes, 0),
            Err(e) => panic!("expected Ok(0), got {e}"),
        }
    }

    /// A minimal backend-agnostic completion future used to exercise
    /// the [`Unified`] adapter without pulling in either real engine.
    struct ReadyOutcome(i32);
    impl Future for ReadyOutcome {
        type Output = i32;
        fn poll(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<i32> {
            Poll::Ready(self.0)
        }
    }

    #[test]
    fn unified_collapses_inner_output() {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);

        let mut ok = Unified::new(ReadyOutcome(512));
        match Pin::new(&mut ok).poll(&mut cx) {
            Poll::Ready(Ok(Completed { bytes })) => assert_eq!(bytes, 512),
            other => panic!("expected Ready Ok, got {other:?}"),
        }

        let mut err = Unified::new(ReadyOutcome(-libc::ENOSPC));
        match Pin::new(&mut err).poll(&mut cx) {
            Poll::Ready(Err(IoError::Os(errno))) => assert_eq!(errno, libc::ENOSPC),
            other => panic!("expected Ready Os err, got {other:?}"),
        }
    }

    struct FakeBp {
        cap: usize,
        live: usize,
        policy: BackPressurePolicy,
    }
    impl BackPressure for FakeBp {
        fn capacity(&self) -> usize {
            self.cap
        }
        fn in_flight(&self) -> usize {
            self.live
        }
        fn policy(&self) -> BackPressurePolicy {
            self.policy
        }
    }

    #[test]
    fn backpressure_defaults_derive_from_capacity_and_live() {
        let full = FakeBp {
            cap: 4,
            live: 4,
            policy: BackPressurePolicy::Capacity,
        };
        assert_eq!(full.available(), 0);
        assert!(!full.admits());
        assert_eq!(full.policy(), BackPressurePolicy::Capacity);

        let room = FakeBp {
            cap: 8,
            live: 3,
            policy: BackPressurePolicy::Parking,
        };
        assert_eq!(room.available(), 5);
        assert!(room.admits());
        assert_eq!(room.policy(), BackPressurePolicy::Parking);

        // `available` saturates rather than underflowing if live ever
        // exceeds capacity (defensive; should not happen in practice).
        let over = FakeBp {
            cap: 2,
            live: 5,
            policy: BackPressurePolicy::Capacity,
        };
        assert_eq!(over.available(), 0);
        assert!(!over.admits());
    }
}
