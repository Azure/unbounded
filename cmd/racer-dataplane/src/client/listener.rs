//! Owned per-cache Unix sockets with permissions, bounded connections, and draining.
use super::{request::RequestParser, response::Responses};
use crate::{
    control::caches::CacheDefinition,
    error::{Operation, deferred},
    http::io::HttpIo,
    read::serve::ReadService,
    runtime::{admission::Admission, deadline::RequestScope},
};
use std::rc::Rc;
pub struct ClientListeners {
    reads: Rc<dyn ReadService>,
    parser: RequestParser,
    responses: Rc<Responses>,
    io: Rc<HttpIo>,
    admission: Rc<Admission>,
}
impl ClientListeners {
    pub fn new(
        reads: Rc<dyn ReadService>,
        parser: RequestParser,
        responses: Rc<Responses>,
        io: Rc<HttpIo>,
        admission: Rc<Admission>,
    ) -> Self {
        Self {
            reads,
            parser,
            responses,
            io,
            admission,
        }
    }
    pub fn reconcile<'a>(
        &'a self,
        _caches: &'a [CacheDefinition],
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("client.listen")
    }
    pub fn drain<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("client.drain")
    }
}
#[cfg(test)]
mod tests { /* Permissions, safe socket ownership, admission caps, removal with readers. */
}
