//! Persistent RDMA links for a two-thread runtime: an I/O thread and a
//! native thread.
//!
//! All RDMA (libibverbs) calls run on the native thread. The I/O thread never
//! touches RDMA. It talks to the native thread through bounded command queues.
//!
//! This crate spawns no threads and runs no executor. Time and randomness come
//! from the current `racer-platform` environment.
//!
//! # Basics
//!
//! There is one persistent, bidirectional RC queue pair (QP) per (peer, rail)
//! and one node-wide pool of registered page buffers per rail.
//!
//! 1. Call [`node`] to get one [`Client`] per I/O worker and an [`OwnerPort`].
//! 2. On the native thread, build the [`Owner`] and poll it, or host it in a
//!    `uring-runtime` service with [`WithNative::with_host`].
//! 3. Call [`Client::activate`] with a [`NodeConfig`]. The owner finds RDMA
//!    ports, your selector picks some, and each picked port gets a page pool.
//! 4. Set up a link with [`Client::open`] and [`Client::accept`], exchanging
//!    [`Setup`] values with the peer over your own channel, then
//!    [`Client::connect`].
//! 5. Per page, the receiver calls [`Client::grant`], the sender
//!    [`Client::write`] or [`Client::release`], and the receiver gets an
//!    [`Arrival`] holding a [`RegisteredPage`].
//!
//! See `designs/racer-verbs.md`.
//!
//! # Zero-copy sends
//!
//! With [`NodeConfig::zero_copy`], each device also gets one implicit
//! on-demand paging (ODP) MR over the whole address space. A
//! [`Source::Bytes`] write then posts the caller's memory directly as its
//! SGE; the owner holds the source `Arc` until the send completion, or until
//! the QP is destroyed if the link breaks, and leaks it if destroy fails.
//! Devices without implicit ODP for RC writes copy into a page buffer
//! instead. [`Client::zero_copy`] reports which applies per rail.
//!
//! # Stopping is the only fence
//!
//! A timeout or an invalidate does not stop the NIC from writing memory. Only
//! destroying the QP does. If the native side cannot free something, it leaks
//! it on purpose instead of freeing memory the NIC might still use. The
//! [`NodeConfig`] guard leaks with it, so the leak counts against your limits.
//!
//! # Shutdown and reuse
//!
//! [`Client::close`] breaks every link. The owner frees everything once no
//! page is held. Once it has drained, [`Client::reopen`] can start a new
//! round. [`WithNative`] adds a native host to a `uring-runtime` service so
//! drain and fence also finish the native side.
//!
//! # Backends
//!
//! - `native`: loads `librdma_verbs.so.1`, built from `native/verbs.c`.
//! - `simulation`: an in-process fake fabric for tests.
//! - Neither: discovery returns [`Error::Unavailable`].
//!
//! # Limits
//!
//! - At most 64 ports. Uses GID index 0.
//! - Poll budgets count steps, not time. A slow NIC call blocks the thread.
//! - The caller owns auth, topology, admission sizing, retries, and timeouts.
//!
//! Hardware-free tests: `cargo test -p rdma-verbs --features simulation`.
mod channel;
mod control;
mod ffi;
mod link;
mod node;
mod pool;

pub use channel::Op;
pub use ffi::Endpoint;
pub use link::{Arrival, DirectBytes, Epoch, Granted, Source};
pub use node::{
    BACKOFF_MAX, BACKOFF_MIN, BACKOFF_RESET, COMMAND_QUEUE, Client, LinkKey, LinkSelector,
    LinkState, NodeConfig, Opened, Owner, OwnerPort, SETUP_TIMEOUT, Setup, node,
};
pub use pool::{BufferState, RegisteredPage, SendBuffer};

#[cfg(any(test, feature = "simulation"))]
pub use ffi::simulation;

pub mod discovery;

/// List RDMA ports on this host. Opens and closes devices on this thread.
///
/// Returns [`Error::Unavailable`] with no RDMA backend. Use
/// [`discovery::inventory_at`] to fill in NUMA info.
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

/// Any value you want kept alive as long as a slot's RDMA resources live.
///
/// Use it to count admission. If a resource is leaked, its guard leaks too.
pub type Guard = Arc<dyn Send + Sync>;

/// Result type for this crate.
pub type Result<T> = std::result::Result<T, Error>;

/// Errors from this crate.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// Bad pool or selector setup.
    InvalidConfiguration,

    /// Bad arguments, or the call is not allowed right now.
    InvalidRequest,

    /// Bad length or memory range.
    InvalidRange,

    /// No RDMA, pool closed, or slot canceled.
    Unavailable,

    /// No free slot, region, or command space.
    Overloaded,

    /// The deadline passed.
    DeadlineExceeded,

    /// Canceled before it finished.
    Cancelled,

    /// An RDMA call failed, or a lock was poisoned.
    Io,
}

impl std::fmt::Display for Error {
    /// Print the variant name.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}

/// One RDMA port, as shown to the selector.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PortInfo {
    /// Device name, like `mlx5_0`.
    pub device: String,

    /// Port number on the device.
    pub port: u8,

    /// Port GID at index 0.
    pub gid: [u8; 16],

    /// NUMA node, if known.
    pub numa_node: Option<usize>,
}

/// A port the selector picked, with your tag for it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Selection {
    /// Your name for this port. The owner uses it as the rail.
    pub tag: u32,

    /// Position in the discovered port list.
    pub index: usize,

    /// The picked port.
    pub port: PortInfo,
}

/// A `uring-runtime` service with a [`NativeHost`] attached.
///
/// - Poll: polls both with the same budget.
/// - Stop admission: only the inner service. Native work keeps going.
/// - Drain and fence: close the native side, then drive it and the inner
///   drain or fence together until both finish. The native side always
///   drains fully, even if the scope has expired or the inner side failed.
///   Driving them together matters: inner work (such as a queued crypto job)
///   can own a pool buffer the native side waits for.
/// - Close: closes both, native first.
pub struct WithNative<T, N> {
    inner: T,

    native: N,
}

/// A native-thread service [`WithNative`] can host, such as the link
/// [`Owner`].
pub trait NativeHost {
    /// Wake `waker` when the I/O side has work.
    fn register_driver(&self, waker: &Waker);

    /// Do up to `budget` steps of work.
    fn poll_budgeted(&mut self, budget: usize) -> Result<()>;

    /// Start shutdown. Does not wait.
    fn close(&self);

    /// True once shutdown finished.
    fn drained(&self) -> bool;
}

impl Endpoint {
    /// Size of the wire encoding.
    pub const ENCODED_LEN: usize = 32;

    /// Encode as 32 big-endian bytes. Does not validate.
    pub fn to_bytes(&self) -> [u8; Self::ENCODED_LEN] {
        let mut bytes = [0; Self::ENCODED_LEN];
        bytes[..16].copy_from_slice(&self.gid);
        bytes[16..20].copy_from_slice(&self.qpn.to_be_bytes());
        bytes[20..24].copy_from_slice(&self.psn.to_be_bytes());
        bytes[24..28].copy_from_slice(&self.mtu.to_be_bytes());
        bytes[28..30].copy_from_slice(&self.lid.to_be_bytes());
        bytes[30] = self.port;
        bytes[31] = self.link_layer;
        bytes
    }

    /// Decode 32 bytes and validate the fields.
    pub fn from_bytes(bytes: &[u8]) -> Result<Self> {
        if bytes.len() != Self::ENCODED_LEN {
            return Err(Error::InvalidRequest);
        }
        let endpoint = Self {
            gid: bytes[..16].try_into().unwrap(),
            qpn: u32::from_be_bytes(bytes[16..20].try_into().unwrap()),
            psn: u32::from_be_bytes(bytes[20..24].try_into().unwrap()),
            mtu: u32::from_be_bytes(bytes[24..28].try_into().unwrap()),
            lid: u16::from_be_bytes(bytes[28..30].try_into().unwrap()),
            port: bytes[30],
            link_layer: bytes[31],
        };
        endpoint.validate()?;
        Ok(endpoint)
    }
}

use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
};

use racer_platform::{
    Operation, Scope,
    group::{FailureReporter, Service},
};

impl<T, N: NativeHost> WithNative<T, N> {
    /// Wrap `inner` with an already built native host, for example an
    /// [`Owner`]. Call on the native thread.
    pub fn with_host(inner: T, native: N) -> Self {
        Self { inner, native }
    }
}

impl<S: Scope, T: Service<S>, N: NativeHost> Service<S> for WithNative<T, N>
where
    S::Error: From<Error>,
{
    /// Pass through to the inner service.
    fn set_failure_reporter(&mut self, reporter: FailureReporter<S::Error>) {
        self.inner.set_failure_reporter(reporter);
    }

    /// Pass through to the inner service.
    fn waker(&self) -> std::result::Result<Waker, S::Error> {
        self.inner.waker()
    }

    /// Register `waker` with both.
    fn register_driver(&self, waker: &Waker) {
        self.inner.register_driver(waker);
        self.native.register_driver(waker);
    }

    /// Drive native activation while the inner service starts.
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        let mut startup = self.inner.start(scope);
        let native = &mut self.native;
        Box::pin(std::future::poll_fn(move |cx| {
            native.register_driver(cx.waker());
            native.poll_budgeted(1)?;
            startup.as_mut().poll(cx)
        }))
    }

    /// Poll the inner service, then the native side, each with `budget`.
    fn poll(&mut self, cx: &mut Context<'_>, budget: usize) -> std::result::Result<(), S::Error> {
        self.inner.poll(cx, budget)?;
        self.native.poll_budgeted(budget).map_err(Into::into)
    }

    /// Pass through to the inner service.
    fn wait_timeout(&self, maximum: std::time::Duration) -> std::time::Duration {
        self.inner.wait_timeout(maximum)
    }

    /// Stop the inner service only. Native work keeps going.
    fn stop_admission(&mut self) -> std::result::Result<(), S::Error> {
        self.inner.stop_admission()
    }

    /// Drain the native side and the inner service together.
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        let inner = self.inner.drain(scope);
        close_alongside(&mut self.native, inner)
    }

    /// Close the native side, then the inner service.
    fn close(&mut self) -> std::result::Result<(), S::Error> {
        self.native.close();
        self.inner.close()
    }

    /// Drain the native side and fence the inner service together.
    fn fence<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        let inner = self.inner.fence(scope);
        close_alongside(&mut self.native, inner)
    }

    /// Pass through to the inner service.
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.shutdown(scope)
    }
}

/// Close `native`, then poll it and `inner` until both finish. Ignores scope
/// deadlines for the native side: the NIC must be stopped before memory is
/// freed, so an inner error is held until the native side has drained. A
/// native error returns at once, as before.
fn close_alongside<'a, N: NativeHost, E: From<Error> + 'a>(
    native: &'a mut N,
    mut inner: Operation<'a, (), E>,
) -> Operation<'a, (), E> {
    native.close();
    let mut outcome: Option<std::result::Result<(), E>> = None;
    Box::pin(std::future::poll_fn(move |cx| {
        native.register_driver(cx.waker());
        if outcome.is_none()
            && let Poll::Ready(result) = inner.as_mut().poll(cx)
        {
            outcome = Some(result);
        }
        // Poll after the inner side so buffers it just returned count.
        native.poll_budgeted(1)?;
        if !native.drained() {
            // Relies on the runtime's periodic ticks to retry.
            return Poll::Pending;
        }
        match outcome.take() {
            Some(result) => Poll::Ready(result),
            None => Poll::Pending,
        }
    }))
}

/// Our own `Arc` around a caller's [`Guard`].
///
/// Its strong count shows whether anything leaked, no matter how many clones
/// the caller keeps.
struct GuardOwner {
    _guard: Guard,

    /// A native allocation escaped destruction; this slot must never replenish it.
    quarantined: AtomicBool,
}

impl GuardOwner {
    /// Wrap `guard`.
    fn new(guard: Guard) -> Arc<Self> {
        Arc::new(Self {
            _guard: guard,
            quarantined: AtomicBool::new(false),
        })
    }

    /// Permanently retire the charged slot after a failed native free.
    fn quarantine(&self) {
        self.quarantined.store(true, Ordering::Release);
    }

    /// Whether a native allocation has been leaked against this charge.
    fn quarantined(&self) -> bool {
        self.quarantined.load(Ordering::Acquire)
    }
}

/// Test-only lifetime charges shared with the private native boundary tests.
#[cfg(test)]
mod test_guard {
    use super::*;

    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Observe how many lifetime charges are still retained.
    pub struct Observer(Arc<AtomicUsize>);

    impl Observer {
        /// Read the outstanding charge count.
        pub fn get(&self) -> usize {
            self.0.load(Ordering::Acquire)
        }
    }

    /// Decrement the observed count when the last guard owner disappears.
    struct Charge(Arc<AtomicUsize>);

    impl Drop for Charge {
        /// Observe final guard release without retaining the original charge.
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::AcqRel);
        }
    }

    /// Create one lifetime charge and its independent observer.
    pub fn guard() -> (Guard, Observer) {
        let count = Arc::new(AtomicUsize::new(1));
        (Arc::new(Charge(count.clone())), Observer(count))
    }
}

/// Pure endpoint encoding and validation boundary tests.
#[cfg(test)]
mod endpoint_tests {
    use super::*;

    /// Construct valid fields at useful encoding boundaries.
    fn endpoint() -> Endpoint {
        Endpoint {
            gid: [1; 16],
            qpn: 0x123456,
            psn: 0xabcdef,
            mtu: 5,
            lid: 0x1234,
            port: 255,
            link_layer: 2,
        }
    }

    /// The wire format is fixed-width network order, not native ABI serialization.
    #[test]
    fn exact_network_byte_layout_and_roundtrip() {
        let endpoint = endpoint();
        let bytes = endpoint.to_bytes();
        assert_eq!(&bytes[..16], &[1; 16]);
        assert_eq!(
            &bytes[16..],
            &[
                0, 0x12, 0x34, 0x56, 0, 0xab, 0xcd, 0xef, 0, 0, 0, 5, 0x12, 0x34, 255, 2
            ]
        );
        assert_eq!(Endpoint::from_bytes(&bytes), Ok(endpoint));
        for len in 0..32 {
            assert_eq!(
                Endpoint::from_bytes(&bytes[..len]),
                Err(Error::InvalidRequest)
            );
        }
        assert_eq!(Endpoint::from_bytes(&[0; 33]), Err(Error::InvalidRequest));
    }

    /// Decode rejects each invalid field while preserving valid boundary values.
    #[test]
    fn decode_rejects_invalid_fields_and_accepts_boundaries() {
        let e = endpoint();
        for invalid in [
            Endpoint { gid: [0; 16], ..e },
            Endpoint { qpn: 0, ..e },
            Endpoint {
                qpn: 0x1000000,
                ..e
            },
            Endpoint {
                psn: 0x1000000,
                ..e
            },
            Endpoint { mtu: 0, ..e },
            Endpoint { mtu: 6, ..e },
            Endpoint { port: 0, ..e },
            Endpoint { link_layer: 0, ..e },
            Endpoint { link_layer: 3, ..e },
        ] {
            assert_eq!(
                Endpoint::from_bytes(&invalid.to_bytes()),
                Err(Error::InvalidRequest)
            );
        }
        for valid in [
            Endpoint {
                qpn: 1,
                psn: 0,
                mtu: 1,
                lid: 0,
                port: 1,
                link_layer: 1,
                ..e
            },
            Endpoint {
                qpn: 0xffffff,
                psn: 0xffffff,
                lid: u16::MAX,
                ..e
            },
        ] {
            assert_eq!(Endpoint::from_bytes(&valid.to_bytes()), Ok(valid));
        }
    }
}

/// Inventory filtering and metadata ordering tests with controlled filesystem input.
#[cfg(test)]
mod discovery_tests {
    use super::{PortInfo, discovery::*};

    use std::path::Path;

    /// Construct a port whose provider locality should be replaced by normalization.
    fn port(device: &str, port: u8, gid: u8) -> PortInfo {
        PortInfo {
            device: device.into(),
            port,
            gid: [gid; 16],
            numa_node: Some(99),
        }
    }

    /// Device names obey the native ABI and sysfs component limits.
    #[test]
    fn names_obey_native_and_path_bounds() {
        for name in [
            "",
            ".",
            "..",
            "a/b",
            "a\0b",
            "a\rb",
            "a\nb",
            &"x".repeat(64),
        ] {
            assert!(!valid_device(name));
        }
        for name in ["mlx5_0", "a b", "a\"b", &"x".repeat(63)] {
            assert!(valid_device(name));
        }
    }

    /// Missing metadata gives deterministic order and preserves the first duplicate.
    #[test]
    fn unknown_pci_sorts_by_port_then_name_and_keeps_first_duplicate() {
        let ports = inventory_at(
            vec![
                port("z", 2, 1),
                port("b", 1, 2),
                port("a", 1, 3),
                port("b", 1, 4),
                port("../bad", 1, 1),
                port("zero", 0, 1),
                port("gid", 1, 0),
            ],
            Path::new("/nonexistent-rdma-inventory"),
        );
        assert_eq!(
            ports
                .iter()
                .map(|p| (p.device.as_str(), p.port, p.gid[0], p.numa_node))
                .collect::<Vec<_>>(),
            [("a", 1, 3, None), ("b", 1, 2, None), ("z", 2, 1, None)]
        );
        assert!(inventory_at(vec![], Path::new(".")).is_empty());
    }

    /// PCI and NUMA normalization does not impose the caller's inventory cap.
    #[test]
    fn pci_order_and_numa_metadata_are_preserved_without_a_policy_cap() {
        /// Remove the project-local metadata fixture when the test finishes.
        struct Fixture(std::path::PathBuf);

        impl Drop for Fixture {
            /// Remove metadata files created inside the project's test directory.
            fn drop(&mut self) {
                std::fs::remove_dir_all(&self.0).unwrap();
            }
        }

        let root = Fixture(
            std::env::current_dir()
                .unwrap()
                .join(format!(".inventory-test-{}", std::process::id())),
        );
        std::fs::create_dir(&root.0).unwrap();
        for (name, bdf, numa) in [("z", "0000:01:00.0", " 7\n"), ("a", "0000:02:00.0", "-1\n")] {
            let physical = root.0.join(bdf);
            std::fs::create_dir(&physical).unwrap();
            std::fs::write(physical.join("numa_node"), numa).unwrap();
            std::fs::create_dir(root.0.join(name)).unwrap();
            std::os::unix::fs::symlink(&physical, root.0.join(name).join("device")).unwrap();
        }
        let mut input = vec![port("a", 1, 1), port("z", 2, 1), port("z", 1, 1)];
        input.extend((0..65).map(|i| port(&format!("unknown{i}"), 1, 1)));
        let ports = inventory_at(input, &root.0);
        assert_eq!(ports.len(), 68);
        assert_eq!(
            ports[..3]
                .iter()
                .map(|p| (p.device.as_str(), p.port, p.numa_node))
                .collect::<Vec<_>>(),
            [("z", 1, Some(7)), ("z", 2, Some(7)), ("a", 1, None)]
        );
    }
}
