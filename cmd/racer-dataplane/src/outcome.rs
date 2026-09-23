// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Transport-neutral attempt evidence and semantic peer outcomes.
//!
//! A peer report describes the downstream attempt, not the reporting connection.
//! Authentication and request binding belong to the transport adapter. HTTP status
//! and metric conversion likewise stay at the HTTP boundary.

use std::{fmt, io, net::SocketAddr};

mod classified;
pub(crate) mod legacy;
pub use classified::Classified;
mod wire;

pub(crate) fn failure_reason(error: &crate::cache::Error) -> PeerReason {
    error.evidence().reason()
}
pub(crate) fn peer_failure(
    error: &crate::cache::Error,
    identity: [u8; 32],
    candidate: u32,
) -> PeerFailure {
    error.evidence().peer_failure(identity, candidate)
}
pub(crate) fn owner_failure(error: &crate::cache::Error) -> Option<u32> {
    error.evidence().owner
}
#[allow(dead_code)] // Compatibility accessor for existing owning-module tests.
pub(crate) fn semantic_failure(error: &crate::cache::Error) -> Option<PeerFailure> {
    error.evidence().semantic
}
#[allow(dead_code)] // Compatibility accessor for existing owning-module tests.
pub(crate) fn attempt_evidence(error: &crate::cache::Error) -> Option<&Failure> {
    error.evidence().attempt
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    LocalAdmission,
    Connect,
    Send,
    Headers,
    Body,
    Grant,
    Read,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Transport {
    Http,
    Rdma,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Cause {
    Connection,
    ServiceTimeout,
    CallerDeadline,
    LocalPressure,
    Cancelled,
    Protocol,
    BreakerRejected,
    Other,
}
impl Cause {
    pub fn reason(self) -> PeerReason {
        match self {
            Self::LocalPressure | Self::BreakerRejected => PeerReason::Busy,
            Self::CallerDeadline | Self::ServiceTimeout => PeerReason::Deadline,
            Self::Cancelled => PeerReason::Cancelled,
            Self::Protocol => PeerReason::Protocol,
            Self::Connection | Self::Other => PeerReason::Service,
        }
    }
}
/// Bounded semantic outcome, distinct from failure of the reporting transport.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(u8)]
pub enum PeerReason {
    OwnerUnavailable = 1,
    Busy,
    Unavailable,
    Protocol,
    Service,
    Deadline,
    Cancelled,
    NotFound,
    Gone,
    Precondition,
    Unauthorized,
    Forbidden,
    MetadataChanged,
}
impl PeerReason {
    /// Only validated terminal value semantics establish owner reachability.
    pub fn establishes_owner_reachability(self) -> bool {
        matches!(
            self,
            Self::NotFound
                | Self::Gone
                | Self::Precondition
                | Self::Unauthorized
                | Self::Forbidden
                | Self::MetadataChanged
        )
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PeerFailure {
    pub identity: [u8; 32],
    pub candidate: u32,
    pub reason: PeerReason,
    /// Original downstream attempt, never evidence about the reporting hop.
    pub evidence: Option<PeerEvidence>,
    pub response: ResponseMetadata,
}
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ResponseMetadata {
    pub challenge: crate::header_value::HeaderValue<1024>,
    pub retry_after: crate::header_value::HeaderValue<128>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PeerEvidence {
    pub endpoint: SocketAddr,
    pub transport: Transport,
    pub phase: Phase,
    pub cause: Cause,
    pub initiated: bool,
}
impl PeerEvidence {
    pub fn from_failure(f: &Failure) -> Option<Self> {
        Some(Self {
            endpoint: f.endpoint.tcp()?,
            transport: f.transport,
            phase: f.phase,
            cause: f.cause,
            initiated: f.initiated,
        })
    }
}
impl fmt::Display for PeerFailure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for PeerFailure {}
#[derive(Clone, Debug)]
pub struct Failure {
    pub endpoint: crate::socket::Address,
    pub transport: Transport,
    pub phase: Phase,
    pub cause: Cause,
    pub initiated: bool,
    pub kind: io::ErrorKind,
    pub message: String,
}
impl Failure {
    pub fn owner_evidence(&self) -> bool {
        self.endpoint.tcp().is_some()
            && self.initiated
            && matches!(self.cause, Cause::Connection | Cause::ServiceTimeout)
    }
    pub(crate) fn neutral_for_health(&self) -> bool {
        matches!(
            self.cause,
            Cause::CallerDeadline
                | Cause::LocalPressure
                | Cause::Cancelled
                | Cause::BreakerRejected
        ) || !self.initiated && self.cause != Cause::Protocol
    }
}
impl fmt::Display for Failure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}
impl std::error::Error for Failure {}
impl From<Failure> for io::Error {
    fn from(failure: Failure) -> Self {
        Self::new(failure.kind, failure)
    }
}

#[derive(Debug)]
pub(crate) struct OwnerUnavailable(pub(crate) u32);
impl fmt::Display for OwnerUnavailable {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "owner {} unavailable", self.0)
    }
}
impl std::error::Error for OwnerUnavailable {}

/// Immutable attribution captured when the direct exchange starts. A trusted
/// report is request-bound; this type alone is not a cryptographic proof.
#[derive(Clone, Debug)]
pub(crate) struct AttemptRoute {
    pub(crate) cursor: crate::routing::Cursor,
    pub(crate) candidate: u32,
    pub(crate) endpoint: SocketAddr,
    pub(crate) final_hop: bool,
    pub(crate) context: String,
}
#[derive(Clone, Debug)]
pub(crate) struct AttemptFailure {
    pub(crate) route: AttemptRoute,
    pub(crate) evidence: Option<Failure>,
    pub(crate) reported: bool,
}
impl AttemptFailure {
    pub(crate) fn owner_evidence(&self) -> bool {
        self.reported
            || (self.route.final_hop
                && self.evidence.as_ref().is_some_and(|e| {
                    e.endpoint.tcp() == Some(self.route.endpoint) && e.owner_evidence()
                }))
    }
}
impl fmt::Display for AttemptFailure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for AttemptFailure {}

/// Borrowed causal facts, classified without allocating or wrapping an error.
/// Adapters collect these once; semantic reports take precedence over local I/O.
#[derive(Default)]
pub(crate) struct Evidence<'a> {
    pub(crate) routed: Option<&'a AttemptFailure>,
    pub(crate) semantic: Option<PeerFailure>,
    pub(crate) owner: Option<u32>,
    pub(crate) attempt: Option<&'a Failure>,
    pub(crate) fallback: Option<PeerReason>,
    pub(crate) admission: bool,
    pub(crate) caller_timeout: bool,
    pub(crate) would_block: bool,
    pub(crate) response: ResponseMetadata,
}
impl Evidence<'_> {
    pub(crate) fn reason(&self) -> PeerReason {
        if let Some(f) = self.semantic {
            f.reason
        } else if self.owner.is_some() {
            PeerReason::OwnerUnavailable
        } else if let Some(f) = self.attempt {
            f.cause.reason()
        } else {
            self.fallback.unwrap_or(PeerReason::Service)
        }
    }
    pub(crate) fn cause(&self) -> Option<Cause> {
        self.semantic
            .and_then(|f| f.evidence.map(|e| e.cause))
            .or_else(|| self.attempt.map(|e| e.cause))
    }
    pub(crate) fn neutral_for_health(&self) -> bool {
        matches!(
            self.reason(),
            PeerReason::Busy
                | PeerReason::Cancelled
                | PeerReason::Unauthorized
                | PeerReason::Forbidden
        ) || self.caller_timeout
            || self.admission
            || self.attempt.is_some_and(Failure::neutral_for_health)
    }
    pub(crate) fn peer_failure(&self, identity: [u8; 32], candidate: u32) -> PeerFailure {
        if let Some(f) = self.semantic {
            return f;
        }
        let reason = match self.reason() {
            PeerReason::OwnerUnavailable if self.owner != Some(candidate) => PeerReason::Service,
            reason => reason,
        };
        PeerFailure {
            identity,
            candidate,
            reason,
            evidence: self.attempt.and_then(PeerEvidence::from_failure),
            response: self.response,
        }
    }
}
