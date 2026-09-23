// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! TLS peer membership policy and request-bound failure attribution.
use crate::tls::PeerIdentity;
use std::{collections::BTreeSet, io};

#[derive(Clone)]
pub struct Policy {
    pub universe: [u8; 32],
    pub node: [u8; 32],
    pub peers: BTreeSet<[u8; 32]>,
}
impl Policy {
    /// Authorize an identity verified by the completed mutual TLS handshake.
    /// The runtime separately pins its pod UID to the current topology on every request.
    pub fn authorize(&self, identity: &PeerIdentity) -> io::Result<()> {
        let universe = identity_bytes(&identity.universe)?;
        let node = identity_bytes(&identity.node)?;
        if universe != self.universe || node == self.node || !self.peers.contains(&node) {
            return Err(denied());
        }
        Ok(())
    }
}

fn denied() -> io::Error {
    io::Error::new(io::ErrorKind::PermissionDenied, "unauthorized TLS peer")
}

fn identity_bytes(value: &str) -> io::Result<[u8; 32]> {
    if value.len() != 64 {
        return Err(denied());
    }
    let digit = |byte| match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(denied()),
    };
    let mut bytes = [0; 32];
    for (out, pair) in bytes.iter_mut().zip(value.as_bytes().chunks_exact(2)) {
        *out = digit(pair[0])? * 16 + digit(pair[1])?;
    }
    Ok(bytes)
}

/// Request-bound failure attribution, shared by HTTP and authenticated RDMA.
pub(crate) mod failure {
    use crate::{cache, http::Headers, http_client as client};
    use cache::{
        http_metadata::{decimal, identity_encoding, text},
        peer_wire::unhex,
    };
    use client::attempt::{PeerFailure, PeerReason};
    use std::{io, net::SocketAddr};

    // Invoke only after authenticated TLS framing or authenticated RDMA session,
    // request and descriptor binding has been checked by the transport adapter.
    pub(crate) fn reported(
        failure: PeerFailure,
        attempt: &mut Option<crate::handlers::Attempt>,
    ) -> cache::Result<()> {
        if let Some(a) = attempt {
            if failure.identity != a.route.cursor.identity || failure.candidate != a.route.candidate
            {
                return Err(invalid("foreign peer failure context").into());
            }
            if failure.reason == PeerReason::OwnerUnavailable {
                if let Some(owner) = a.owner.take() {
                    owner.failure(crate::environment::now());
                }
                return Err(io::Error::other(AttemptFailure {
                    route: a.route.clone(),
                    evidence: None,
                    reported: true,
                })
                .into());
            }
            if establishes_owner_reachability(failure.reason) {
                a.owner_reachable();
            }
        } else if failure.identity != [0; 32]
            || failure.candidate != 0
            || failure.reason == PeerReason::OwnerUnavailable
        {
            return Err(invalid("unrouted peer failure context").into());
        }
        Err(io::Error::other(failure).into())
    }
    impl client::Origin {
        pub(crate) fn error(permit: crate::breaker::Permit, error: &cache::Error, peer: bool) {
            let evidence = attempt_evidence(error);
            if matches!(
                failure_reason(error),
                PeerReason::Busy | PeerReason::Cancelled
            ) || error_chain(error).any(|e| {
                matches!(
                    e.downcast_ref::<cache::Error>(),
                    Some(cache::Error::Timeout | cache::Error::Admission(_))
                )
            }) || evidence.is_some_and(|e| {
                matches!(
                    e.cause,
                    client::attempt::Cause::CallerDeadline
                        | client::attempt::Cause::LocalPressure
                        | client::attempt::Cause::Cancelled
                        | client::attempt::Cause::BreakerRejected
                ) || !e.initiated && e.cause != client::attempt::Cause::Protocol
            }) {
                #[cfg(test)]
                if crate::simulation::current().is_some_and(|world| {
                    world.activate_mutant(crate::simulation::history::Mutant::LocalFailureAsRemote)
                }) {
                    permit.failure();
                    return;
                }
                drop(permit);
                return;
            }
            let healthy_status = matches!(
                error,
                cache::Error::NotFound | cache::Error::Gone | cache::Error::Precondition
            ) || matches!(error, cache::Error::Io(error) if error.get_ref().and_then(|e| e.downcast_ref::<cache::http_metadata::HttpStatus>()).is_some_and(|s| s.0 < 500));
            #[cfg(test)]
            if let Some(w) = crate::simulation::current() {
                w.event(
                    "breaker-error",
                    "",
                    format!(
                        "peer={peer} error={error:?} outcome={}",
                        if !peer && healthy_status {
                            "success"
                        } else {
                            "failure"
                        }
                    ),
                );
            }
            if !peer && healthy_status {
                permit.success();
            } else {
                permit.failure();
            }
        }
    }

    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidData, message)
    }
    pub(crate) fn error_status(error: &cache::Error) -> u16 {
        match failure_reason(error) {
            PeerReason::OwnerUnavailable | PeerReason::Busy | PeerReason::Unavailable => 503,
            PeerReason::Deadline => 504,
            PeerReason::NotFound => 404,
            PeerReason::Gone => 410,
            PeerReason::Precondition => 412,
            _ => 502,
        }
    }
    // io::Error::source skips its immediate payload. Preserve adapter/fanout types.
    pub(crate) fn error_chain<'a>(
        error: &'a (dyn std::error::Error + 'static),
    ) -> impl Iterator<Item = &'a (dyn std::error::Error + 'static)> {
        std::iter::successors(Some(error), |error| {
            if let Some(error) = error.downcast_ref::<io::Error>() {
                error
                    .get_ref()
                    .map(|e| e as &(dyn std::error::Error + 'static))
            } else {
                error.source()
            }
        })
    }
    pub(crate) fn error_detail<T: std::error::Error + 'static>(error: &cache::Error) -> Option<&T> {
        error_chain(error).find_map(|e| e.downcast_ref::<T>())
    }
    pub(crate) fn attempt_evidence(error: &cache::Error) -> Option<&client::attempt::Failure> {
        error_detail::<AttemptFailure>(error)
            .and_then(|f| f.evidence.as_ref())
            .or_else(|| error_detail(error))
    }
    pub(crate) fn failure_reason(error: &cache::Error) -> PeerReason {
        use client::attempt::Cause;
        if let Some(f) = semantic_failure(error) {
            return f.reason;
        }
        if owner_failure(error).is_some() {
            return PeerReason::OwnerUnavailable;
        }
        if let Some(f) = attempt_evidence(error) {
            return match f.cause {
                Cause::LocalPressure | Cause::BreakerRejected => PeerReason::Busy,
                Cause::CallerDeadline | Cause::ServiceTimeout => PeerReason::Deadline,
                Cause::Cancelled => PeerReason::Cancelled,
                Cause::Protocol => PeerReason::Protocol,
                Cause::Connection | Cause::Other => PeerReason::Service,
            };
        }
        let mut kind = io::ErrorKind::Other;
        for error in error_chain(error) {
            if let Some(error) = error.downcast_ref::<cache::Error>() {
                match error {
                    cache::Error::NotFound => return PeerReason::NotFound,
                    cache::Error::Gone => return PeerReason::Gone,
                    cache::Error::Precondition => return PeerReason::Precondition,
                    cache::Error::Timeout => return PeerReason::Deadline,
                    cache::Error::Unavailable => return PeerReason::Unavailable,
                    cache::Error::Admission(_) => return PeerReason::Busy,
                    cache::Error::InvalidData(_) => return PeerReason::Protocol,
                    _ => {}
                }
            }
            if let Some(error) = error.downcast_ref::<io::Error>() {
                kind = error.kind();
            }
        }
        match kind {
            io::ErrorKind::WouldBlock => PeerReason::Busy,
            io::ErrorKind::InvalidData => PeerReason::Protocol,
            io::ErrorKind::TimedOut => PeerReason::Deadline,
            io::ErrorKind::Interrupted => PeerReason::Cancelled,
            _ => PeerReason::Service,
        }
    }
    pub(crate) fn metric_failure(error: &cache::Error) -> crate::metrics::HttpFailure {
        use crate::metrics::{HttpErrorReason as R, HttpFailure, HttpPressure as P};
        use client::attempt::Cause;
        let reason = match failure_reason(error) {
            PeerReason::OwnerUnavailable => R::OwnerUnavailable,
            PeerReason::Busy => R::Busy,
            PeerReason::Unavailable => R::Unavailable,
            PeerReason::Protocol => R::Protocol,
            PeerReason::Service => R::Service,
            PeerReason::Deadline => R::Deadline,
            PeerReason::Cancelled => R::Cancelled,
            PeerReason::NotFound => R::NotFound,
            PeerReason::Gone => R::Gone,
            PeerReason::Precondition => R::Precondition,
        };
        // A semantic report describes the downstream cause, not this hop's socket.
        let cause = semantic_failure(error)
            .and_then(|f| f.evidence.map(|e| e.cause))
            .or_else(|| attempt_evidence(error).map(|e| e.cause));
        let pressure = match cause {
            Some(Cause::LocalPressure) => Some(P::LocalPressure),
            Some(Cause::BreakerRejected) => Some(P::BreakerRejected),
            _ if error_chain(error).any(|e| {
                matches!(
                    e.downcast_ref::<cache::Error>(),
                    Some(cache::Error::Admission(_))
                )
            }) =>
            {
                Some(P::Admission)
            }
            _ if reason == R::Busy
                && error_chain(error).any(|e| {
                    e.downcast_ref::<io::Error>()
                        .is_some_and(|e| e.kind() == io::ErrorKind::WouldBlock)
                }) =>
            {
                Some(P::WouldBlock)
            }
            _ => None,
        };
        HttpFailure { reason, pressure }
    }
    #[derive(Debug)]
    pub(crate) struct OwnerUnavailable(pub(crate) u32);
    impl std::fmt::Display for OwnerUnavailable {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "owner {} unavailable", self.0)
        }
    }
    impl std::error::Error for OwnerUnavailable {}
    pub(crate) fn semantic_failure(error: &cache::Error) -> Option<PeerFailure> {
        error_detail::<PeerFailure>(error).copied()
    }
    pub(crate) fn peer_failure(
        error: &cache::Error,
        identity: [u8; 32],
        candidate: u32,
    ) -> PeerFailure {
        if let Some(f) = semantic_failure(error) {
            return f;
        }
        let reason = match failure_reason(error) {
            PeerReason::OwnerUnavailable if owner_failure(error) != Some(candidate) => {
                PeerReason::Service
            }
            reason => reason,
        };
        PeerFailure {
            identity,
            candidate,
            reason,
            evidence: attempt_evidence(error)
                .and_then(crate::http_client::attempt::PeerEvidence::from_failure),
        }
    }
    pub(crate) fn owner_failure(error: &cache::Error) -> Option<u32> {
        error_detail::<OwnerUnavailable>(error).map(|e| e.0)
    }
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
    #[derive(Debug)]
    pub(crate) struct AttemptFailure {
        pub(crate) route: AttemptRoute,
        pub(crate) evidence: Option<client::attempt::Failure>,
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
    impl std::fmt::Display for AttemptFailure {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "{self:?}")
        }
    }
    impl std::error::Error for AttemptFailure {}
    pub(crate) fn validate_owner_report(
        headers: Headers<'_>,
        length: Option<u64>,
        route: &AttemptRoute,
    ) -> io::Result<()> {
        let slot =
            text(headers, "x-racer-owner-unavailable")?.ok_or_else(|| invalid("missing owner"))?;
        if decimal(slot)? != u64::from(route.candidate)
            || length != Some(0)
            || text(headers, "x-racer-attempt")? != Some(route.context.as_str())
        {
            return Err(invalid("mismatched owner report"));
        }
        identity_encoding(headers)?;
        Ok(())
    }
    pub(crate) fn validate_peer_report(
        headers: Headers<'_>,
        length: Option<u64>,
        status: u16,
        route: &AttemptRoute,
    ) -> io::Result<PeerFailure> {
        let failure = PeerFailure::decode(&unhex(
            text(headers, "x-racer-failure")?.ok_or_else(|| invalid("missing peer failure"))?,
        )?)?;
        if length != Some(0)
            || text(headers, "x-racer-attempt")? != Some(route.context.as_str())
            || failure.identity != route.cursor.identity
            || failure.candidate != route.candidate
            || status != error_status(&io::Error::other(failure).into())
        {
            return Err(invalid("invalid peer failure framing/context"));
        }
        if let Some(owner) = text(headers, "x-racer-owner-unavailable")? {
            if failure.reason != PeerReason::OwnerUnavailable
                || decimal(owner)? != u64::from(failure.candidate)
            {
                return Err(invalid("contradictory owner report"));
            }
        }
        identity_encoding(headers)?;
        Ok(failure)
    }
    /// Only validated terminal value semantics establish owner reachability.
    pub(crate) fn establishes_owner_reachability(reason: PeerReason) -> bool {
        matches!(
            reason,
            PeerReason::NotFound | PeerReason::Gone | PeerReason::Precondition
        )
    }
    pub(crate) fn io_error(error: cache::Error) -> io::Error {
        if let cache::Error::Io(error) = error {
            return error;
        }
        let kind = match error.root() {
            cache::Error::Timeout => io::ErrorKind::TimedOut,
            cache::Error::NotFound => io::ErrorKind::NotFound,
            cache::Error::InvalidData(_) => io::ErrorKind::InvalidData,
            cache::Error::Io(error) => error.kind(),
            _ => io::ErrorKind::Other,
        };
        io::Error::new(kind, error)
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/http_auth.rs"
));

#[cfg(test)]
pub(crate) fn test_wall_authentication(world: &crate::simulation::World) {
    tests::wall_authentication(world);
}
