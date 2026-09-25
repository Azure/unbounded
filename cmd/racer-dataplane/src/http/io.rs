//! Socket operations retain buffers through partial I/O and cancellation completion.
//!
//! Connections transfer by value so an abandoned future cannot close/recycle the
//! socket while the kernel uses it. Before submission, move the connection and
//! owned buffers into reactor-owned state; return them only after the final fence.
//! Head operations must stage encoded/received bytes in owned IoBuffer storage.
use super::{codec::MessageHead, pool::ConnectionLease};
use crate::{
    error::{Operation, deferred},
    runtime::{
        deadline::RequestScope,
        reactor::{Completion, IoBuffer, Reactor},
    },
};
use std::rc::Rc;
pub struct HttpIo {
    reactor: Rc<Reactor>,
    codec: super::codec::Codec,
}
/// A completed head operation, including the still-exclusively-owned connection.
///
/// Head and body operations transfer ownership in both directions:
/// ```no_run
/// use racer_dataplane::{error::Result,
///     http::{io::HttpIo, pool::ConnectionLease}, memory::pool::PlaintextBuffer,
///     runtime::{deadline::RequestScope, reactor::Completion}};
/// async fn exchange(io: &HttpIo, connection: ConnectionLease,
///     buffer: PlaintextBuffer, scope: &RequestScope)
///     -> Result<Completion<PlaintextBuffer, ConnectionLease>> {
///     let head = io.receive_head(connection, scope).await?;
///     let sent = io.send_head(head.connection, head.value, scope).await?;
///     let body = io.read_body(sent.connection, buffer, scope).await?;
///     io.write_body(body.lease, body.buffer, scope).await
/// }
/// ```
pub struct HeadCompletion<T> {
    pub connection: ConnectionLease,
    pub value: T,
}
impl HttpIo {
    pub fn new(reactor: Rc<Reactor>, codec: super::codec::Codec) -> Self {
        Self { reactor, codec }
    }
    pub fn receive_head<'a>(
        &'a self,
        _connection: ConnectionLease,
        _scope: &'a RequestScope,
    ) -> Operation<'a, HeadCompletion<MessageHead>> {
        deferred("http.receive_head")
    }
    pub fn send_head<'a>(
        &'a self,
        _connection: ConnectionLease,
        _head: MessageHead,
        _scope: &'a RequestScope,
    ) -> Operation<'a, HeadCompletion<()>> {
        deferred("http.send_head")
    }
    /// Stream bounded chunks; endpoint controls expected body length and validation.
    /// Borrowed destinations cannot survive abandonment of a submitted operation:
    /// ```compile_fail
    /// use racer_dataplane::{http::{io::HttpIo, pool::ConnectionLease},
    ///     runtime::deadline::RequestScope};
    /// fn borrowed(io: &HttpIo, connection: ConnectionLease, scope: &RequestScope) {
    ///     let mut bytes = [0; 16];
    ///     let _future = io.read_body(connection, &mut bytes[..], scope);
    /// }
    /// ```
    pub fn read_body<'a, B: IoBuffer>(
        &'a self,
        _connection: ConnectionLease,
        _buffer: B,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, ConnectionLease>> {
        deferred("http.read_body")
    }
    /// Stage borrowed/shared page slices in an owned buffer before calling.
    /// ```compile_fail
    /// use racer_dataplane::{http::{io::HttpIo, pool::ConnectionLease},
    ///     runtime::deadline::RequestScope};
    /// fn borrowed(io: &HttpIo, connection: ConnectionLease, scope: &RequestScope) {
    ///     let bytes = [0; 16];
    ///     let _future = io.write_body(connection, &bytes[..], scope);
    /// }
    /// ```
    pub fn write_body<'a, B: IoBuffer>(
        &'a self,
        _connection: ConnectionLease,
        _buffer: B,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, ConnectionLease>> {
        deferred("http.write_body")
    }
}
#[cfg(test)]
mod tests { /* Short operations, half-close, timeouts, cancellation completion fences. */
}
