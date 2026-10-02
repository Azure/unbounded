//! Worker-local encrypted slab storage. No HTTP, plaintext, or origin credentials.
pub mod catalog;
pub mod checkpoint;
pub mod format;
pub mod writer;

use self::catalog::{Index, RecordLocation};
use crate::runtime::{admission::AdmissionPolicy, reactor::Reactor};
use crate::{
    error::{Error, Operation, Result},
    memory::{page::CiphertextCopy, pool::BufferPool},
    model::{PageId, ResourceClass},
    runtime::deadline::RequestScope,
};
use page_alloc::{Alignment, Segments, Slab};
use std::rc::Rc;

pub struct Store {
    pub reader: Rc<StoreReader>,
    pub writer: Rc<writer::StoreWriter>,
    pub checkpoint: Rc<checkpoint::Checkpointer>,
    pub recovery: checkpoint::Recovery,
    pub eviction: Rc<catalog::SegmentClock>,
}

impl Store {
    /// Side-effect-free resource wiring, before startup and request admission.
    pub fn configure(
        &self,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        queue_entries: usize,
        page_entries: usize,
    ) -> Result<()> {
        self.writer.configure(
            admission,
            self.eviction.clone(),
            queue_entries,
            page_entries,
        )
    }

    /// Open slabs and configure the actual live shard and checkpoint geometry.
    pub fn open(&self) -> Operation<'_, Alignment> {
        Box::pin(async move {
            let alignment = self.writer.open().await?;
            let slabs = self.writer.slabs();
            let geometry = checkpoint::CheckpointGeometry::new(
                slabs.capacity_bytes(),
                slabs.segment_bytes(),
                slabs.capacity_bytes() / slabs.segment_bytes(),
                alignment,
            )?;
            self.checkpoint.configure_geometry(geometry)?;
            self.recovery.configure_geometry(geometry)?;
            Ok(alignment)
        })
    }
}

/// Lease mappings before awaiting I/O and return validated ciphertext framing.
/// AEAD validation remains owned by fill.
pub struct StoreReader {
    metrics: crate::telemetry::metrics::Metrics,
    clock: Rc<catalog::SegmentClock>,
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    buffers: BufferPool,
}
#[derive(Clone)]
/// Permits conditional invalidation without removing a replacement mapping.
pub struct ReadToken {
    page: PageId,
    location: RecordLocation,
}
impl StoreReader {
    pub fn metadata(
        &self,
        version: &crate::model::ObjectVersion,
    ) -> Result<Option<crate::model::VersionMetadata>> {
        self.index.version(version)
    }
    pub fn new(
        clock: Rc<catalog::SegmentClock>,
        index: Rc<Index>,
        segments: Rc<Segments>,
        slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        buffers: BufferPool,
    ) -> Self {
        Self {
            metrics: crate::telemetry::metrics::Metrics::default(),
            clock,
            index,
            segments,
            slabs,
            admission,
            reactor,
            buffers,
        }
    }
    pub fn with_metrics(mut self, metrics: crate::telemetry::metrics::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    fn corrupt_miss(&self) {
        let _ = self
            .metrics
            .record(crate::telemetry::metrics::Event::CorruptMiss, 1);
    }
    pub fn invalidate(&self, token: &ReadToken) -> Result<()> {
        self.index.remove_if_matches(&token.page, &token.location);
        Ok(())
    }
    pub fn read_with_token<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<(CiphertextCopy, ReadToken)>> {
        self.read_with_token_reclaim(page, scope, |amount| {
            self.admission
                .reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Ciphertext,
                    amount,
                )
                .map_err(Into::into)
        })
    }
    /// Admit staging and decoded ciphertext together before submitting disk I/O.
    pub(crate) fn read_with_token_reclaim<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
        reserve: impl Fn(usize) -> Result<flow_control::Charge<AdmissionPolicy>> + 'a,
    ) -> Operation<'a, Option<(CiphertextCopy, ReadToken)>> {
        Box::pin(async move {
            scope.check()?;
            let entry = match self.metrics.lookup(
                crate::telemetry::metrics::LookupTier::DiskIndex,
                self.index.lookup(page),
            )? {
                Some(e) => e,
                None => return Ok(None),
            };
            let token = ReadToken {
                page: page.clone(),
                location: entry.location.clone(),
            };
            // Both checks happen without yielding, so eviction cannot interleave.
            if self
                .segments
                .validate(
                    entry.location.segment,
                    entry.location.generation,
                    &entry.location.extent,
                )
                .is_err()
            {
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
            let length = entry.location.extent.length();
            let decoded_length = entry.metadata.page_length(page)? as usize + 16;
            let amount = length
                .checked_add(decoded_length)
                .ok_or(Error::Overloaded)?;
            let mut reserved = reserve(amount);
            if matches!(reserved, Err(Error::Overloaded)) {
                self.slabs.reclaim_idle();
                reserved = reserve(amount);
            }
            let mut decoded_reservation = reserved?;
            decoded_reservation.validate(ResourceClass::Ciphertext, amount)?;
            if !self.admission.owns(&decoded_reservation)
                || decoded_reservation.key() != Some(&page.version.object.cache)
            {
                return Err(Error::InvalidConfiguration);
            }
            let staging = decoded_reservation.split(length)?;
            let buffer = self.slabs.allocate(length, staging)?;
            let buffer = match self
                .slabs
                .read(&self.reactor, entry.location.extent, buffer, lease, scope)
                .await
            {
                Ok(b) => b,
                Err(error @ (Error::Io | Error::CorruptRecord)) => {
                    if error == Error::CorruptRecord {
                        self.corrupt_miss();
                    }
                    self.invalidate(&token)?;
                    return Ok(None);
                }
                Err(e) => return Err(e),
            };
            let decoded = match format::parse(&buffer, entry.location.extent) {
                Ok(d) => d,
                Err(_) => {
                    self.corrupt_miss();
                    self.invalidate(&token)?;
                    return Ok(None);
                }
            };
            if decoded.header.envelope.page != *page
                || decoded.header.generation != entry.location.generation
                || decoded.header.metadata != entry.metadata
                || decoded.header.envelope.key_id != entry.key_id
            {
                self.corrupt_miss();
                self.invalidate(&token)?;
                return Ok(None);
            }
            // A concurrent retirement/removal must not resurrect a completed copy.
            if self.index.lookup(page)?.is_none_or(|current| {
                current.location != entry.location || current.key_id != entry.key_id
            }) {
                return Ok(None);
            }
            let ciphertext = self.buffers.ciphertext(
                decoded_reservation,
                decoded.header.envelope,
                buffer.bytes()?[decoded.ciphertext].to_vec(),
            )?;
            ciphertext.expected_checksum(decoded.checksum)?;
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
mod tests;
