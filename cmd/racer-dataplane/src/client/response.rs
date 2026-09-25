//! Central status mapping and streaming body delivery, including late truncation.
use crate::{
    error::{Error, Operation, Result, deferred, pending},
    http::{codec::MessageHead, io::HttpIo, pool::ConnectionLease},
    memory::delivery::Delivery,
    read::serve::ReadResponse,
    runtime::deadline::RequestScope,
};
use std::rc::Rc;
pub struct Responses {
    io: Rc<HttpIo>,
    delivery: Rc<Delivery>,
}
impl Responses {
    pub fn new(io: Rc<HttpIo>, delivery: Rc<Delivery>) -> Self {
        Self { io, delivery }
    }
    /// Version unavailable -> 412, transient unavailable -> 503, range -> 206.
    /// Define malformed/unsatisfiable/unsupported method mappings in one place.
    pub fn error_head(&self, _error: Error) -> Result<MessageHead> {
        pending("client.error_head")
    }
    pub fn send<'a>(
        &'a self,
        _connection: ConnectionLease,
        _response: ReadResponse,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        deferred("client.send")
    }
}
#[cfg(test)]
mod tests { /* 206/412/503, empty body, Content-Range, late error closes connection. */
}
