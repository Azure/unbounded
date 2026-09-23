// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Compatibility boundary for public io::Result APIs and explicit Error::Io.
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
pub(crate) fn collect<'a>(error: &'a (dyn std::error::Error + 'static)) -> Evidence<'a> {
    let mut facts = Evidence::default();
    let mut direct = None;
    let mut routed = None;
    let mut saw_route = false;
    let mut kind = io::ErrorKind::Other;
    for error in error_chain(error) {
        if let Some(classified) = error.downcast_ref::<super::Classified>() {
            let inner = classified.evidence();
            facts.semantic = facts.semantic.or(inner.semantic);
            facts.owner = facts.owner.or(inner.owner);
            facts.fallback = facts.fallback.or(inner.fallback);
            facts.admission |= inner.admission;
            facts.caller_timeout |= inner.caller_timeout;
            facts.would_block |= inner.would_block;
            facts.response = inner.response;
        }
        if let Some(f) = error.downcast_ref::<PeerFailure>() {
            facts.semantic.get_or_insert(*f);
        }
        if let Some(status) = error.downcast_ref::<crate::cache::http_metadata::HttpStatus>() {
            facts.response = status.1;
            facts.fallback = Some(match status.0 {
                401 => PeerReason::Unauthorized,
                403 => PeerReason::Forbidden,
                404 => PeerReason::NotFound,
                410 => PeerReason::Gone,
                412 => PeerReason::Precondition,
                _ => PeerReason::Service,
            });
        }
        if let Some(f) = error.downcast_ref::<OwnerUnavailable>() {
            facts.owner.get_or_insert(f.0);
        }
        if let Some(f) = error.downcast_ref::<AttemptFailure>()
            && !saw_route
        {
            saw_route = true;
            facts.routed = Some(f);
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
#[allow(unused_imports)]
pub(crate) use super::{
    attempt_evidence, failure_reason, owner_failure, peer_failure, semantic_failure,
};
#[allow(dead_code)] // Retain the historical adapter/test path.
pub(crate) fn io_error(error: cache::Error) -> io::Error {
    error.into_io()
}
