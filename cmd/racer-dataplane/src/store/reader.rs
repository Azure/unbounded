//! Lease a mapping before await, validate framing, and return original ciphertext.
//! AEAD is owned by fill; a read token permits conditional invalidation on failure.
use super::{
    format::RecordCodec,
    index::{Index, RecordLocation},
    segment::Segments,
    slab::Slabs,
};
use crate::{
    error::{Error, Operation, Result},
    memory::{page::CiphertextCopy, pool::BufferPool},
    model::{identity::PageId, limits::ResourceClass},
    runtime::{admission::Reservation, deadline::RequestScope, reactor::IoBuffer},
};
use std::rc::Rc;
pub struct StoreReader {
    clock: Rc<super::eviction::SegmentClock>,
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slabs>,
    buffers: Rc<BufferPool>,
    staging: futures::lock::Mutex<()>,
}
#[derive(Clone)]
pub struct ReadToken {
    page: PageId,
    location: RecordLocation,
}
impl StoreReader {
    pub fn metadata(
        &self,
        version: &crate::model::identity::ObjectVersion,
    ) -> Result<Option<crate::model::metadata::VersionMetadata>> {
        self.index.version(version)
    }
    pub fn new(
        clock: Rc<super::eviction::SegmentClock>,
        index: Rc<Index>,
        segments: Rc<Segments>,
        slabs: Rc<Slabs>,
        buffers: Rc<BufferPool>,
    ) -> Self {
        Self {
            clock,
            index,
            segments,
            slabs,
            buffers,
            staging: futures::lock::Mutex::new(()),
        }
    }
    pub fn invalidate(&self, token: &ReadToken) -> Result<()> {
        self.index.remove_if_matches(&token.page, &token.location)
    }
    pub fn read_with_token<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<(CiphertextCopy, ReadToken)>> {
        Box::pin(async move { self.read_reserved(page, scope, &mut None).await })
    }
    /// Consume a fill's output reservation only on a validated disk hit. A miss
    /// or staging failure leaves the charge available for origin acquisition.
    pub(crate) fn read_reserved<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
        reservation: &'a mut Option<Reservation>,
    ) -> Operation<'a, Option<(CiphertextCopy, ReadToken)>> {
        Box::pin(async move {
            scope.check()?;
            // One record staging allocation per worker fits the range window's
            // single-record progress margin. Waiting readers retain their own
            // output charges, not another full aligned record.
            let mut lock = std::pin::pin!(self.staging.lock());
            let cancellation = scope.cancellation.subscribe()?;
            let _staging = std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                std::future::Future::poll(lock.as_mut(), cx).map(Ok)
            })
            .await?;
            let entry = match self.index.lookup(page)? {
                Some(e) => e,
                None => return Ok(None),
            };
            let token = ReadToken {
                page: page.clone(),
                location: entry.location.clone(),
            };
            // Both checks happen without yielding, so eviction cannot interleave.
            if self.segments.validate_location(&entry.location).is_err() {
                self.invalidate(&token)?;
                return Ok(None);
            }
            let lease = match self
                .segments
                .lease(entry.location.segment, entry.location.generation)
            {
                Ok(l) => l,
                Err(_) => {
                    self.invalidate(&token)?;
                    return Ok(None);
                }
            };
            let buffer = self.slabs.allocate(
                entry.location.location.extent.length(),
                Some(&page.version.object.cache),
            )?;
            let buffer = match self
                .slabs
                .read(entry.location.location, buffer, lease, scope)
                .await
            {
                Ok(b) => b,
                Err(Error::Io | Error::CorruptRecord) => {
                    self.invalidate(&token)?;
                    return Ok(None);
                }
                Err(e) => return Err(e),
            };
            let decoded = match RecordCodec.parse(&buffer, entry.location.location.extent) {
                Ok(d) => d,
                Err(_) => {
                    self.invalidate(&token)?;
                    return Ok(None);
                }
            };
            if decoded.header.envelope.page != *page
                || decoded.header.generation != entry.location.generation
                || decoded.header.metadata != entry.metadata
                || decoded.header.envelope.key_id != entry.key_id
            {
                self.invalidate(&token)?;
                return Ok(None);
            }
            // A concurrent retirement/removal must not resurrect a completed copy.
            if self.index.lookup(page)?.is_none_or(|current| {
                current.location != entry.location || current.key_id != entry.key_id
            }) {
                return Ok(None);
            }
            let reservation = match reservation.take() {
                Some(reservation) => reservation,
                None => self.slabs.reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Ciphertext,
                    decoded.ciphertext.len(),
                )?,
            };
            let ciphertext = self.buffers.ciphertext(
                reservation,
                decoded.header.envelope,
                buffer.bytes()?[decoded.ciphertext].to_vec(),
            )?;
            self.clock.mark_read(entry.location.segment)?;
            Ok(Some((
                CiphertextCopy {
                    metadata: entry.metadata.for_pin(),
                    ciphertext,
                },
                token,
            )))
        })
    }
    pub fn read<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<CiphertextCopy>> {
        Box::pin(async move {
            Ok(self
                .read_with_token(page, scope)
                .await?
                .map(|(copy, _)| copy))
        })
    }
}
#[cfg(test)]
mod tests { /* Disk corruption and mapping replacement are covered in store integration tests. */
}
