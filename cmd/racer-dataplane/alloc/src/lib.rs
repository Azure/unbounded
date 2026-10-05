//! Worker-local aligned storage, with caller-owned accounting and I/O policy.
#![deny(unsafe_op_in_unsafe_fn)]

mod buffer;
mod clock;
mod geometry;
mod segments;
mod slab;

pub use buffer::{AlignedBuffer, Alignment, Charge, Extent};
pub use clock::{SegmentClock, SegmentEntries};
pub use geometry::{MAX_SEGMENTS, SegmentGeometry};
pub use segments::{
    FreezeGuard, Generation, SegmentId, SegmentLease, SegmentSnapshot, SegmentState, Segments,
};
pub use slab::Slab;

/// Storage failures, separated from application record and admission policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
pub enum Error {
    /// The filesystem or kernel cannot provide the required direct I/O support.
    Unsupported,
    /// Caller configuration or an implementation contract is invalid.
    InvalidConfiguration,
    /// A transient lease, freeze, or resource limit prevents progress.
    Busy,
    /// An extent or persisted allocator image is malformed.
    Corrupt,
    /// A generation, segment state, or table identity is no longer valid.
    Stale,
    /// Storage has not been opened or is exclusively locked elsewhere.
    Unavailable,
    /// An I/O completion was short or failed without OS error detail.
    Io,
    /// An operating-system operation failed; preserve its diagnostic context.
    SystemIo {
        operation: &'static str,
        errno: Option<i32>,
    },
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if let Self::SystemIo { operation, errno } = self {
            return match errno {
                Some(errno) => write!(
                    f,
                    "{operation}: {} (errno {errno})",
                    std::io::Error::from_raw_os_error(*errno)
                ),
                None => write!(f, "{operation}: storage I/O failed"),
            };
        }
        f.write_str(match self {
            Self::Unsupported => "direct I/O is unsupported",
            Self::InvalidConfiguration => "invalid storage configuration",
            Self::Busy => "storage is busy or exhausted",
            Self::Corrupt => "invalid storage extent or generation",
            Self::Stale => "stale storage generation, state, or table",
            Self::Unavailable => "storage is unavailable",
            Self::Io => "storage I/O failed",
            Self::SystemIo { .. } => unreachable!(),
        })
    }
}
impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;
