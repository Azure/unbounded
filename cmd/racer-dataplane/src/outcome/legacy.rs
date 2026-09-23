// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Compatibility boundary for cache fanout and public io::Result APIs.
//! Collect dynamic payloads once, then classify typed facts in the neutral owner.
//! New internal callers can construct Evidence directly instead of wrapping it.

use super::{AttemptFailure, Evidence, Failure, OwnerUnavailable, PeerFailure, PeerReason};
use crate::cache;
use std::io;

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
pub(crate) fn evidence(error: &cache::Error) -> Evidence<'_> {
    let mut facts = Evidence::default();
    let mut direct = None;
    let mut routed = None;
    let mut saw_route = false;
    let mut kind = io::ErrorKind::Other;
    for error in error_chain(error) {
        if let Some(f) = error.downcast_ref::<PeerFailure>() {
            facts.semantic.get_or_insert(*f);
        }
        if let Some(f) = error.downcast_ref::<OwnerUnavailable>() {
            facts.owner.get_or_insert(f.0);
        }
        if let Some(f) = error.downcast_ref::<AttemptFailure>()
            && !saw_route
        {
            saw_route = true;
            routed = f.evidence.as_ref();
        }
        if let Some(f) = error.downcast_ref::<Failure>() {
            direct.get_or_insert(f);
        }
        if let Some(error) = error.downcast_ref::<cache::Error>() {
            let reason = match error {
                cache::Error::NotFound => Some(PeerReason::NotFound),
                cache::Error::Gone => Some(PeerReason::Gone),
                cache::Error::Precondition => Some(PeerReason::Precondition),
                cache::Error::Timeout => {
                    facts.caller_timeout = true;
                    Some(PeerReason::Deadline)
                }
                cache::Error::Unavailable => Some(PeerReason::Unavailable),
                cache::Error::Admission(_) => {
                    facts.admission = true;
                    Some(PeerReason::Busy)
                }
                cache::Error::InvalidData(_) => Some(PeerReason::Protocol),
                _ => None,
            };
            if facts.fallback.is_none() {
                facts.fallback = reason;
            }
        }
        if let Some(error) = error.downcast_ref::<io::Error>() {
            kind = error.kind();
            facts.would_block |= kind == io::ErrorKind::WouldBlock;
        }
    }
    facts.attempt = routed.or(direct);
    facts.fallback.get_or_insert(match kind {
        io::ErrorKind::WouldBlock => PeerReason::Busy,
        io::ErrorKind::InvalidData => PeerReason::Protocol,
        io::ErrorKind::TimedOut => PeerReason::Deadline,
        io::ErrorKind::Interrupted => PeerReason::Cancelled,
        _ => PeerReason::Service,
    });
    facts
}
// Compatibility accessors for independently migrating request/cache adapters.
#[allow(dead_code)]
pub(crate) fn attempt_evidence(error: &cache::Error) -> Option<&Failure> {
    evidence(error).attempt
}
pub(crate) fn failure_reason(error: &cache::Error) -> PeerReason {
    evidence(error).reason()
}
#[allow(dead_code)]
pub(crate) fn semantic_failure(error: &cache::Error) -> Option<PeerFailure> {
    evidence(error).semantic
}
pub(crate) fn peer_failure(
    error: &cache::Error,
    identity: [u8; 32],
    candidate: u32,
) -> PeerFailure {
    evidence(error).peer_failure(identity, candidate)
}
pub(crate) fn owner_failure(error: &cache::Error) -> Option<u32> {
    evidence(error).owner
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
