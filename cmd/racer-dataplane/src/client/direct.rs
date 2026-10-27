//! Direct reads write object plaintext into a client descriptor, not the socket.
//!
//! `host-read` writes into a regular file (normally a memfd) with positioned
//! io_uring writes. `gpu-read` hands each page to a [`GpuService`] backend. Both
//! share one request path through the [`DirectSink`] seam: the client code
//! resolves the range, gets each verified page from the Get read path, and asks
//! the sink to deliver it at a destination offset. The sink resolves the future
//! only when the bytes are visible to the client, so the next page frame can
//! go out.
//!
//! The client sends exactly one descriptor with the request head. Any other
//! count, a descriptor on another request, or truncated ancillary data is a 400.

use crate::client::ReadKind;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::Delivery;
use crate::http::ReaderLease;
use crate::read::sealed::SealedPage;
use crate::read::sealed::SealedTarget;
use crate::runtime::RequestScope;
use http1::connection::ReceivedDescriptors;
pub use racer_object_wire::client::Destination;
pub use racer_object_wire::client::DirectDelivery;
use std::os::fd::AsRawFd;
use std::os::fd::OwnedFd;
use std::rc::Rc;
use uring_runtime::reactor::descriptor::Descriptor;

/// The fewest pages the dataplane fetches ahead for one direct read. Clients
/// send no credits, so this window, or `range_window_pages` when larger, alone
/// bounds the pages a direct read pins.
pub const DIRECT_WINDOW_PAGES: usize = 4;

/// One page handed to a [`DirectSink`].
///
/// Pages arrive as verified plaintext unless the sink asked for sealed pages
/// with [`DirectSink::sealed_target`].
#[non_exhaustive]
pub enum DirectPage<'a> {
    /// Verified plaintext already in memory. [`ReaderLease::slice_bytes`] holds
    /// exactly the bytes to write; [`ReaderLease::page`] can be cloned to keep
    /// the page alive past this call.
    Plaintext(&'a ReaderLease),

    /// A whole sealed page, for the sink to verify and decrypt. Only
    /// `range`, a byte range of the page's plaintext, is written. A sink
    /// must not write anything if verification fails, and must then return
    /// [`Error::CorruptRecord`].
    Sealed {
        page: Box<SealedPage>,
        range: std::ops::Range<u64>,
    },
}

impl DirectPage<'_> {
    /// Number of bytes this page writes.
    pub fn len(&self) -> u64 {
        match self {
            Self::Plaintext(reader) => u64::from(reader.slice().length),
            Self::Sealed { range, .. } => range.end.saturating_sub(range.start),
        }
    }

    /// Whether this page writes nothing. Pages are never empty in practice.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// Where a direct read writes. One sink serves one request and owns the
/// client's descriptor; dropping the sink must release it.
///
/// The caller calls [`Self::deliver`] once per page, in page order, and waits
/// for each call before the next. It calls [`Self::finish`] once after the last
/// page, and on a best-effort basis after a failure, before the kind 2 frame.
/// A sink may be dropped without `finish`, for example when the client
/// disconnects; its `Drop` must then keep any resource that a device may still
/// write rather than free it.
pub trait DirectSink {
    /// Finish any setup that can still fail the request with an error status,
    /// such as importing a GPU allocation. Awaited once before the read starts
    /// and before the response head is sent.
    fn ready<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async { Ok(()) })
    }

    /// Write `page` at `destination_offset` in the client's destination. The
    /// caller guarantees the bytes fall inside the request's destination
    /// window. Resolve only when the bytes are visible to the client.
    fn deliver<'a>(
        &'a mut self,
        page: DirectPage<'a>,
        destination_offset: u64,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()>;

    /// Wait for all work that touches the destination, then release it.
    fn finish<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;

    /// Ask for sealed pages ([`DirectPage::Sealed`]) where the read path has
    /// no plaintext in memory. Called after [`Self::ready`]. The default,
    /// None, gets verified plaintext only.
    fn sealed_target(&self) -> Option<SealedTarget> {
        None
    }
}

/// A GPU backend for `gpu-read`. Without one, `gpu-read` answers 501.
pub trait GpuService {
    /// Import the client's exported allocation and return a sink for one
    /// request. Called after the request is validated and before any read.
    /// Return [`Error::NotImplemented`] when the allocation is on a GPU the
    /// dataplane cannot use; [`Error::InvalidRequest`] for a bad handle.
    /// `gpu_uuid` names the client's GPU when the client sent it.
    fn open(
        &self,
        descriptor: OwnedFd,
        destination: Destination,
        allocation_size: u64,
        gpu_uuid: Option<[u8; 16]>,
    ) -> Result<Box<dyn DirectSink>>;
}

/// Writes pages into a client file with positioned io_uring writes.
pub(crate) struct HostSink {
    file: Rc<Descriptor>,

    delivery: Rc<Delivery>,

    window: Destination,
}

impl HostSink {
    /// Accept only a regular file opened for writing. Never map it, so a
    /// client that truncates the file cannot fault the dataplane.
    pub(crate) fn open(
        descriptor: OwnedFd,
        window: Destination,
        delivery: Rc<Delivery>,
    ) -> Result<Self> {
        // SAFETY: fstat writes only into the zeroed stat buffer.
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstat(descriptor.as_raw_fd(), &mut stat) } != 0
            || stat.st_mode & libc::S_IFMT != libc::S_IFREG
        {
            return Err(Error::InvalidRequest);
        }
        // SAFETY: F_GETFL only reads descriptor flags.
        let flags = unsafe { libc::fcntl(descriptor.as_raw_fd(), libc::F_GETFL) };
        if flags < 0 || flags & libc::O_ACCMODE == libc::O_RDONLY || flags & libc::O_APPEND != 0 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            file: Rc::new(Descriptor::from(descriptor)),
            delivery,
            window,
        })
    }
}

impl DirectSink for HostSink {
    fn deliver<'a>(
        &'a mut self,
        page: DirectPage<'a>,
        destination_offset: u64,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let end = destination_offset
                .checked_add(page.len())
                .ok_or(Error::InvalidRange)?;
            if destination_offset < self.window.offset || end > self.window.end() {
                return Err(Error::InvalidRange);
            }
            match page {
                DirectPage::Plaintext(reader) => {
                    self.delivery
                        .write_slice_at(reader, self.file.clone(), destination_offset, scope)
                        .await
                }
                // Never requested: host sinks have no sealed target.
                DirectPage::Sealed { .. } => Err(Error::Internal),
            }
        })
    }

    fn finish<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        // Completed io_uring writes are already visible through the page cache.
        Box::pin(async { Ok(()) })
    }
}

/// Pair the descriptors received with a request head with its parsed kind,
/// and open the sink for a direct read. Unclaimed descriptors close on return.
pub(crate) fn open_sink(
    kind: &ReadKind,
    received: ReceivedDescriptors,
    delivery: &Rc<Delivery>,
    gpu: Option<&Rc<dyn GpuService>>,
) -> Result<Option<Box<dyn DirectSink>>> {
    let ReceivedDescriptors {
        mut descriptors,
        truncated,
    } = received;
    if truncated {
        return Err(Error::InvalidRequest);
    }
    let Some(mode) = kind.direct() else {
        return if descriptors.is_empty() {
            Ok(None)
        } else {
            Err(Error::InvalidRequest)
        };
    };
    let (Some(descriptor), true) = (descriptors.pop(), descriptors.is_empty()) else {
        return Err(Error::InvalidRequest);
    };
    let destination = kind.destination().ok_or(Error::Internal)?;
    match (mode, kind) {
        (DirectDelivery::Host, _) => Ok(Some(Box::new(HostSink::open(
            descriptor,
            destination,
            delivery.clone(),
        )?))),
        (
            DirectDelivery::Gpu,
            ReadKind::GpuRead {
                allocation_size,
                gpu_uuid,
                ..
            },
        ) => gpu
            .ok_or(Error::NotImplemented)?
            .open(descriptor, destination, *allocation_size, *gpu_uuid)
            .map(Some),
        _ => Err(Error::Internal),
    }
}
