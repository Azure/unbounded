//! Simulator-owned copies of allocator models that production no longer
//! uses: the scored segment clock with payload-free mappings, and
//! per-page heat with second-sight history. Hidden from docs; not an API.
mod clock;
mod index;
mod retention;

pub use clock::{PhantomEntries, SegmentClock, SegmentEntries};
pub use retention::{Heat, SecondSight};
