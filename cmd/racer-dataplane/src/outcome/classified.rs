// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Owned typed failures carried through cache fanout and request routing.
use super::*;

/// Opaque classified failure. Internal constructors retain typed attribution;
/// foreign I/O errors are inspected once at the conversion boundary.
#[derive(Debug)]
pub struct Classified {
    detail: Detail,
}
#[derive(Debug)]
enum Detail {
    Transport(Failure),
    Routed(AttemptFailure),
    Peer(PeerFailure),
    Owner(OwnerUnavailable),
    HttpStatus(crate::cache::http_metadata::HttpStatus),
    Boundary {
        source: io::Error,
        healthy_status: bool,
        semantic: Option<PeerFailure>,
        owner: Option<u32>,
        attempt: Option<Failure>,
        route: Option<AttemptFailure>,
        fallback: Option<PeerReason>,
        admission: bool,
        caller_timeout: bool,
        would_block: bool,
        response: ResponseMetadata,
    },
}
impl Classified {
    pub(crate) fn healthy_status(&self) -> bool {
        if let Detail::HttpStatus(status) = &self.detail {
            return status.0 < 500;
        }
        matches!(
            self.detail,
            Detail::Boundary {
                healthy_status: true,
                ..
            }
        )
    }
    pub(crate) fn io_kind(&self) -> io::ErrorKind {
        match &self.detail {
            Detail::Transport(f) => f.kind,
            Detail::Boundary { source, .. } => source.kind(),
            _ => io::ErrorKind::Other,
        }
    }
    pub(crate) fn into_io(self) -> io::Error {
        match self.detail {
            Detail::Transport(f) => f.into(),
            Detail::Boundary { source, .. } => source,
            Detail::Routed(f) => io::Error::other(f),
            Detail::Peer(f) => io::Error::other(f),
            Detail::Owner(f) => io::Error::other(f),
            Detail::HttpStatus(status) => io::Error::other(status),
        }
    }
    pub(crate) fn boundary(source: io::Error) -> Self {
        let facts = io_adapter::collect(&source);
        let route = facts.routed.cloned();
        let detail = Detail::Boundary {
            healthy_status: source
                .get_ref()
                .and_then(|e| e.downcast_ref::<crate::cache::http_metadata::HttpStatus>())
                .is_some_and(|s| s.0 < 500),
            semantic: facts.semantic,
            owner: facts.owner,
            attempt: facts.attempt.cloned(),
            route,
            fallback: facts.fallback,
            admission: facts.admission,
            caller_timeout: facts.caller_timeout,
            would_block: facts.would_block,
            response: facts.response,
            source,
        };
        Self { detail }
    }
    pub(crate) fn evidence(&self) -> Evidence<'_> {
        match &self.detail {
            Detail::Transport(f) => Evidence {
                attempt: Some(f),
                would_block: f.kind == io::ErrorKind::WouldBlock,
                ..Evidence::default()
            },
            Detail::Routed(f) => Evidence {
                routed: Some(f),
                attempt: f.evidence.as_ref(),
                ..Evidence::default()
            },
            Detail::Peer(f) => Evidence {
                semantic: Some(*f),
                ..Evidence::default()
            },
            Detail::Owner(f) => Evidence {
                owner: Some(f.0),
                ..Evidence::default()
            },
            Detail::HttpStatus(status) => Evidence {
                fallback: Some(match status.0 {
                    401 => PeerReason::Unauthorized,
                    403 => PeerReason::Forbidden,
                    404 => PeerReason::NotFound,
                    410 => PeerReason::Gone,
                    412 => PeerReason::Precondition,
                    _ => PeerReason::Service,
                }),
                response: status.1,
                ..Evidence::default()
            },
            Detail::Boundary {
                route,
                semantic,
                owner,
                attempt,
                fallback,
                admission,
                caller_timeout,
                would_block,
                response,
                ..
            } => Evidence {
                routed: route.as_ref(),
                semantic: *semantic,
                owner: *owner,
                attempt: attempt.as_ref(),
                fallback: *fallback,
                admission: *admission,
                caller_timeout: *caller_timeout,
                would_block: *would_block,
                response: *response,
            },
        }
    }
    pub(crate) fn routed(&self) -> Option<&AttemptFailure> {
        match &self.detail {
            Detail::Routed(f) => Some(f),
            Detail::Boundary { route, .. } => route.as_ref(),
            _ => None,
        }
    }
    fn detail(&self) -> &(dyn std::error::Error + 'static) {
        match &self.detail {
            Detail::Transport(f) => f,
            Detail::Routed(f) => f,
            Detail::Peer(f) => f,
            Detail::Owner(f) => f,
            Detail::HttpStatus(status) => status,
            Detail::Boundary { source, .. } => source,
        }
    }
}
impl fmt::Display for Classified {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Display::fmt(self.detail(), f)
    }
}
impl std::error::Error for Classified {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(self.detail())
    }
}
impl From<Failure> for crate::cache::Error {
    fn from(f: Failure) -> Self {
        Self::Outcome(Box::new(Classified {
            detail: Detail::Transport(f),
        }))
    }
}
impl From<AttemptFailure> for crate::cache::Error {
    fn from(f: AttemptFailure) -> Self {
        Self::Outcome(Box::new(Classified {
            detail: Detail::Routed(f),
        }))
    }
}
impl From<PeerFailure> for crate::cache::Error {
    fn from(f: PeerFailure) -> Self {
        Self::Outcome(Box::new(Classified {
            detail: Detail::Peer(f),
        }))
    }
}
impl From<OwnerUnavailable> for crate::cache::Error {
    fn from(f: OwnerUnavailable) -> Self {
        Self::Outcome(Box::new(Classified {
            detail: Detail::Owner(f),
        }))
    }
}
impl From<crate::cache::http_metadata::HttpStatus> for crate::cache::Error {
    fn from(status: crate::cache::http_metadata::HttpStatus) -> Self {
        Self::Outcome(Box::new(Classified {
            detail: Detail::HttpStatus(status),
        }))
    }
}
