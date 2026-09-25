//! Independent reader cursors and completion-safe release on stalls/disconnects.
use super::{
    pipe::{PipeLease, PipePool},
    pool::VerifiedPage,
};
use crate::{
    error::{Operation, Result, deferred, pending},
    model::range::PageSlice,
    runtime::deadline::RequestScope,
};
use std::rc::Rc;
pub struct Delivery {
    pipes: Rc<PipePool>,
    stall_timeout: std::time::Duration,
}
pub struct ReaderLease {
    page: VerifiedPage,
    pipe: PipeLease,
    slice: PageSlice,
}
impl Delivery {
    pub fn new(pipes: Rc<PipePool>, stall_timeout: std::time::Duration) -> Self {
        Self {
            pipes,
            stall_timeout,
        }
    }
    pub fn attach(&self, _page: VerifiedPage, _slice: PageSlice) -> Result<ReaderLease> {
        pending("delivery.attach")
    }
    pub fn finish<'a>(
        &'a self,
        _reader: ReaderLease,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("delivery.finish")
    }
}
#[cfg(test)]
mod tests { /* Slow-reader timeout does not cancel others or free kernel references. */
}
