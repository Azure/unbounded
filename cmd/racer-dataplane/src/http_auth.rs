// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! TLS peer membership policy and request-bound failure attribution.
use crate::tls::PeerIdentity;
use std::{collections::BTreeMap, io, sync::Arc};

/// One index per catalog: exact process UID and RDMA fabric by binary node ID.
pub(crate) type Members = BTreeMap<[u8; 32], (String, String)>;

#[derive(Clone)]
pub struct Policy {
    pub universe: [u8; 32],
    pub node: [u8; 32],
    pub(crate) members: Arc<Members>,
}
impl Policy {
    /// Authorize an identity verified by the completed mutual TLS handshake.
    /// Runtime policies pin its pod UID to the selected volume's member catalog.
    pub fn authorize(&self, identity: &PeerIdentity) -> io::Result<()> {
        let universe = identity_bytes(&identity.universe)?;
        let node = identity_bytes(&identity.node)?;
        let member = self
            .members
            .get(&node)
            .is_some_and(|(pod, _)| *pod == identity.pod_uid);
        if universe != self.universe || node == self.node || !member {
            return Err(denied());
        }
        Ok(())
    }
}

fn denied() -> io::Error {
    io::Error::new(io::ErrorKind::PermissionDenied, "unauthorized TLS peer")
}

pub(crate) fn identity_bytes(value: &str) -> io::Result<[u8; 32]> {
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
    use crate::outcome::{AttemptFailure, AttemptRoute, failure_reason};
    use crate::outcome::{PeerFailure, PeerReason};
    use crate::{cache, http::Headers};
    use cache::{
        http_metadata::{identity_encoding, text},
        peer_wire::unhex,
    };
    use std::io;

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
                return Err(AttemptFailure {
                    route: a.route.clone(),
                    evidence: None,
                    reported: true,
                }
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
        Err(failure.into())
    }

    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidData, message)
    }
    pub(crate) fn error_status(error: &cache::Error) -> u16 {
        status(failure_reason(error))
    }
    fn status(reason: PeerReason) -> u16 {
        match reason {
            PeerReason::OwnerUnavailable | PeerReason::Busy | PeerReason::Unavailable => 503,
            PeerReason::Deadline => 504,
            PeerReason::NotFound => 404,
            PeerReason::Gone => 410,
            PeerReason::Precondition | PeerReason::MetadataChanged => 412,
            PeerReason::Unauthorized => 401,
            PeerReason::Forbidden => 403,
            _ => 502,
        }
    }
    pub(crate) fn metric_failure(error: &cache::Error) -> crate::metrics::HttpFailure {
        use crate::metrics::{HttpErrorReason as R, HttpFailure, HttpPressure as P};
        use crate::outcome::Cause;
        let evidence = error.evidence();
        let reason = match evidence.reason() {
            PeerReason::OwnerUnavailable => R::OwnerUnavailable,
            PeerReason::Busy => R::Busy,
            PeerReason::Unavailable => R::Unavailable,
            PeerReason::Protocol => R::Protocol,
            PeerReason::Service => R::Service,
            PeerReason::Deadline => R::Deadline,
            PeerReason::Cancelled => R::Cancelled,
            PeerReason::NotFound => R::NotFound,
            PeerReason::Gone => R::Gone,
            PeerReason::Precondition | PeerReason::MetadataChanged => R::Precondition,
            PeerReason::Unauthorized | PeerReason::Forbidden => R::Other,
        };
        // A semantic report describes the downstream cause, not this hop's socket.
        let pressure = match evidence.cause() {
            Some(Cause::LocalPressure) => Some(P::LocalPressure),
            Some(Cause::BreakerRejected) => Some(P::BreakerRejected),
            _ if evidence.admission => Some(P::Admission),
            _ if reason == R::Busy && evidence.would_block => Some(P::WouldBlock),
            _ => None,
        };
        HttpFailure { reason, pressure }
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
            || status != self::status(failure.reason)
        {
            return Err(invalid("invalid peer failure framing/context"));
        }
        identity_encoding(headers)?;
        if crate::header_value::HeaderValue::parse(headers, "www-authenticate")?
            != failure.response.challenge
            || crate::header_value::HeaderValue::parse(headers, "retry-after")?
                != failure.response.retry_after
        {
            return Err(invalid("contradictory failure metadata"));
        }
        Ok(failure)
    }
    /// Only validated terminal value semantics establish owner reachability.
    pub(crate) fn establishes_owner_reachability(reason: PeerReason) -> bool {
        reason.establishes_owner_reachability()
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/http_auth.rs"
));
