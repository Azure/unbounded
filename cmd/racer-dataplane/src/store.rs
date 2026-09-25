//! Worker-local encrypted slab storage. No HTTP, plaintext, or origin credentials.
pub mod checkpoint;
pub mod checkpoint_format;
pub mod direct;
pub mod eviction;
pub mod format;
pub mod index;
pub mod reader;
pub mod recovery;
pub mod segment;
pub mod slab;
pub mod writer;

use std::rc::Rc;
pub struct Store {
    pub reader: Rc<reader::StoreReader>,
    pub writer: Rc<writer::StoreWriter>,
    pub checkpoint: checkpoint::Checkpointer,
    pub recovery: recovery::Recovery,
    pub eviction: Rc<eviction::SegmentClock>,
}

impl Store {
    /// Side-effect-free resource wiring, before startup and request admission.
    pub fn configure(
        &self,
        admission: Rc<crate::runtime::admission::Admission>,
        queue_entries: usize,
        page_entries: usize,
    ) -> crate::error::Result<()> {
        self.writer.configure(
            admission,
            self.eviction.clone(),
            queue_entries,
            page_entries,
        )
    }
    /// Open slabs and configure the actual live shard and checkpoint geometry.
    pub fn open(&self) -> crate::error::Operation<'_, direct::DirectAlignment> {
        Box::pin(async move {
            let alignment = self.writer.open().await?;
            let slabs = self.writer.slabs();
            let geometry = checkpoint_format::CheckpointGeometry::new(
                slabs.slab_bytes(),
                slabs.segment_bytes(),
                slabs.slab_bytes() / slabs.segment_bytes(),
                alignment,
            )?;
            self.checkpoint.configure_geometry(geometry)?;
            self.recovery.configure_geometry(geometry)?;
            Ok(alignment)
        })
    }
}

#[cfg(test)]
mod tests;
