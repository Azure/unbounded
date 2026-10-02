//! Policy-driven flow control without application resource names or telemetry.
#![deny(unsafe_op_in_unsafe_fn)]

pub mod pipe;
pub mod quota;
pub mod window;
pub use quota::{Charge, Class, Policy, Quotas, Rejection, SharedQuotas};
pub use window::Window;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidInput,
    Overloaded,
    Unavailable,
    Io,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::InvalidInput => "invalid flow-control input",
            Self::Overloaded => "flow-control quota exhausted",
            Self::Unavailable => "flow control stopped",
            Self::Io => "flow-control I/O failed",
        })
    }
}
impl std::error::Error for Error {}
pub type Result<T> = std::result::Result<T, Error>;
