//! Bounded sliding whole-page window, ordered slices, and independent reader leases.
//!
//! Every page is authenticated before its first byte is exposed. Pin one ETag and
//! length for the stream. A late error closes/truncates the HTTP response, never
//! emits a second status or silently switches versions. Do not buffer whole objects.
use super::fill::Fill;
use crate::{
    error::{Operation, Result, deferred, pending},
    memory::delivery::{Delivery, ReaderLease},
    model::{context::OriginContext, metadata::ObjectMetadata, range::ResolvedRange},
    runtime::deadline::RequestScope,
    topology::membership::MembershipLease,
};
use std::rc::Rc;
pub struct RangeStreams {
    fill: Rc<Fill>,
    directory: std::sync::Arc<super::dispatch::WorkerDirectory>,
    delivery: Rc<Delivery>,
    window_pages: usize,
}
pub struct RangeStream {
    metadata: ObjectMetadata,
    range: ResolvedRange,
    context: OriginContext,
    membership: MembershipLease,
    scope: RequestScope,
    fill: Rc<Fill>,
    directory: std::sync::Arc<super::dispatch::WorkerDirectory>,
    delivery: Rc<Delivery>,
    window_pages: usize,
}
impl RangeStreams {
    pub fn new(
        fill: Rc<Fill>,
        directory: std::sync::Arc<super::dispatch::WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
    ) -> Self {
        Self {
            fill,
            directory,
            delivery,
            window_pages,
        }
    }
    pub fn open(
        &self,
        _metadata: ObjectMetadata,
        _range: ResolvedRange,
        _context: OriginContext,
        _membership: MembershipLease,
        _scope: RequestScope,
    ) -> Result<RangeStream> {
        pending("range_stream.open")
    }
}
impl RangeStream {
    pub fn next_slice(&mut self) -> Operation<'_, Option<ReaderLease>> {
        deferred("range_stream.next")
    }
    pub fn cancel(&mut self) -> Operation<'_, ()> {
        deferred("range_stream.cancel")
    }
}
#[cfg(test)]
mod tests { /* Multi-page order, sliding limits, late-page error, disconnect cleanup. */
}
