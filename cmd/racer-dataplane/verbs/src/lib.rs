//! Bounded paired-thread RDMA operations. Native owners never cross threads.
pub mod discovery;
mod endpoint;
mod ffi;
mod lifecycle;
mod scoped;
pub use scoped::WithNative;

pub use ffi::Endpoint;
#[cfg(any(test, feature = "simulation"))]
pub use ffi::simulation;
#[cfg(feature = "simulation")]
pub use lifecycle::testing;
pub use lifecycle::{
    Configuration, DeviceHandle, IoPort, NativePort, NativeService, PortInfo, QueuePairHandle,
    Region, Selection, Ticket, Window, pair,
};

/// A synchronous pre-enrollment inventory. Native handles are dropped on this
/// thread; only owned descriptions leave it. Missing providers return Unavailable.
pub fn inventory() -> Result<Vec<PortInfo>> {
    Ok(ffi::discover()?
        .iter()
        .map(|device| PortInfo {
            device: device.name.clone(),
            port: device.endpoint.port,
            gid: device.endpoint.gid,
            numa_node: None,
        })
        .collect())
}

use std::sync::Arc;

/// Opaque caller-owned lifetime charge. The crate never inspects its contents.
pub type Guard = Arc<dyn Send + Sync>;

// This unique wrapper, not the caller's Arc, is counted to detect native leaks.
// A caller may retain any number of references without preventing pool reopen.
struct GuardOwner {
    _guard: Guard,
}
impl GuardOwner {
    fn new(guard: Guard) -> Arc<Self> {
        Arc::new(Self { _guard: guard })
    }
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidConfiguration,
    InvalidRequest,
    InvalidRange,
    Unavailable,
    Overloaded,
    DeadlineExceeded,
    Cancelled,
    Io,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Error {}

#[cfg(test)]
mod test_guard {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    pub struct Observer(Arc<AtomicUsize>);
    impl Observer {
        pub fn get(&self) -> usize {
            self.0.load(Ordering::Acquire)
        }
    }
    struct Charge(Arc<AtomicUsize>);
    impl Drop for Charge {
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::AcqRel);
        }
    }
    pub fn guard() -> (Guard, Observer) {
        let count = Arc::new(AtomicUsize::new(1));
        (Arc::new(Charge(count.clone())), Observer(count))
    }
}
