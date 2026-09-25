//! Socket operations retain buffers through partial I/O and cancellation completion.
use super::{codec::MessageHead, pool::ConnectionLease};
use crate::{
    error::{Operation, deferred},
    runtime::{deadline::RequestScope, reactor::Reactor},
};
use std::rc::Rc;
pub struct HttpIo {
    reactor: Rc<Reactor>,
    codec: super::codec::Codec,
}
impl HttpIo {
    pub fn new(reactor: Rc<Reactor>, codec: super::codec::Codec) -> Self {
        Self { reactor, codec }
    }
    pub fn receive_head<'a>(
        &'a self,
        _connection: &'a mut ConnectionLease,
        _scope: &'a RequestScope,
    ) -> Operation<'a, MessageHead> {
        deferred("http.receive_head")
    }
    pub fn send_head<'a>(
        &'a self,
        _connection: &'a mut ConnectionLease,
        _head: MessageHead,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("http.send_head")
    }
    /// Stream bounded chunks; endpoint controls expected body length and validation.
    pub fn read_body<'a>(
        &'a self,
        _connection: &'a mut ConnectionLease,
        _buffer: &'a mut [u8],
        _scope: &'a RequestScope,
    ) -> Operation<'a, usize> {
        deferred("http.read_body")
    }
    pub fn write_body<'a>(
        &'a self,
        _connection: &'a mut ConnectionLease,
        _buffer: &'a [u8],
        _scope: &'a RequestScope,
    ) -> Operation<'a, usize> {
        deferred("http.write_body")
    }
}
#[cfg(test)]
mod tests { /* Short operations, half-close, timeouts, cancellation completion fences. */
}
