//! Worker-local aligned storage, with caller-owned accounting and I/O policy.
#![deny(unsafe_op_in_unsafe_fn)]

mod buffer;
mod clock;
mod geometry;
mod segments;
mod slab;

pub use buffer::{AlignedBuffer, Alignment, Charge, Extent};
pub use clock::{SegmentClock, SegmentEntries};
pub use geometry::SegmentGeometry;
pub use segments::{Generation, SegmentId, SegmentLease, SegmentSnapshot, SegmentState, Segments};
pub use slab::Slab;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Unsupported,
    InvalidConfiguration,
    Busy,
    Corrupt,
    Unavailable,
    Io,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Unsupported => "direct I/O is unsupported",
            Self::InvalidConfiguration => "invalid storage configuration",
            Self::Busy => "storage is busy or exhausted",
            Self::Corrupt => "invalid storage extent or generation",
            Self::Unavailable => "storage is unavailable",
            Self::Io => "storage I/O failed",
        })
    }
}
impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;
