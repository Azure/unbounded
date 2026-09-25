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
