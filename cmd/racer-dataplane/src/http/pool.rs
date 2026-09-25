//! Bounded per-endpoint pools. Never reuse a connection with an unread response body.
use crate::{
    error::{Operation, deferred},
    runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor},
};
use std::{os::fd::OwnedFd, path::PathBuf, rc::Rc};
pub enum Endpoint {
    Unix(PathBuf),
    Peer(String),
}
/// Exclusive socket/pool lease. Submitted operations retain the whole lease in
/// the reactor through the final fence, not just a borrowed or copied raw FD.
/// On cancellation/error, fence before closing; only a fully consumed, healthy
/// connection may return to the pool. Dropping a waiter is not a reuse signal.
pub struct ConnectionLease {
    fd: OwnedFd,
    reusable: bool,
}
pub struct HttpPool {
    reactor: Rc<Reactor>,
    admission: Rc<Admission>,
    per_endpoint: usize,
}
impl HttpPool {
    pub fn new(reactor: Rc<Reactor>, admission: Rc<Admission>, per_endpoint: usize) -> Self {
        Self {
            reactor,
            admission,
            per_endpoint,
        }
    }
    pub fn checkout<'a>(
        &'a self,
        _endpoint: &'a Endpoint,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        deferred("http.checkout")
    }
}
#[cfg(test)]
mod tests { /* Exhaustion, deadlines, unread bodies, idle expiry, neighbor transitions. */
}
