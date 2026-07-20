// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Persistent shard-local client lanes for the TLS TCP RPC transport.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::future::Future;
use std::marker::PhantomData;
use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::{Duration, Instant};

use serde::Serialize;

use crate::bufferpool::{BulkRef, Error as PoolError, PageRef, PageStream, Req, Transport};
use crate::p2p::{NodeId, RouteTableHandle, stripe_to_ring};
use crate::ring::{
    NetHandle, SockAddr, TLS_RECORD_TYPE_ALERT, TLS_RECORD_TYPE_APPLICATION_DATA,
    TLS_RECORD_TYPE_HANDSHAKE,
};
use crate::tls::PeerTlsContext;

use super::MAX_REQUESTS_PER_CONNECTION;
use super::server::{TcpRpcError, record_error};
use super::wire::{
    DecodeStatus, DecodedMetadata, ErrorMetadata, FrameHeader, FrameKind, FramePrefix, Handshake,
    MAX_DESTINATION_PAGE_COUNT, PageMetadata, RequestMetadata, decode_prefix, encode_handshake,
    encode_request,
};

pub const DEFAULT_TTL: u8 = 64;

const HEADER_LEN: usize = super::wire::HEADER_LEN;
const ERROR_ORIGIN_NOT_FOUND: u32 = 404;

type LocalFuture<T> = Pin<Box<dyn Future<Output = T> + 'static>>;

/// Fixed shard-local settings for outbound peer lanes.
#[derive(Clone, Debug)]
pub struct ClientPeerDirectoryConfig {
    pub local_peer_name: String,
    pub lane_count: u16,
    pub max_waiters_per_peer: usize,
    pub max_page: u32,
    pub socket_buffer_bytes: u32,
    pub request_timeout: Duration,
}

impl ClientPeerDirectoryConfig {
    fn validate(&self) -> Result<(), TcpRpcError> {
        if self.local_peer_name.is_empty() {
            return Err(TcpRpcError::Config("local peer name must not be empty"));
        }
        if self.lane_count == 0 {
            return Err(TcpRpcError::Config("client lane count must be nonzero"));
        }
        if self.max_waiters_per_peer == 0 {
            return Err(TcpRpcError::Config("client waiter limit must be nonzero"));
        }
        if self.max_page == 0 || self.max_page as usize > super::wire::MAX_PAGE_BODY_LEN {
            return Err(TcpRpcError::Config(
                "client max page is outside the wire limit",
            ));
        }
        if self.socket_buffer_bytes == 0 || self.socket_buffer_bytes > i32::MAX as u32 {
            return Err(TcpRpcError::Config("client socket buffer size is invalid"));
        }
        if self.request_timeout.is_zero() {
            return Err(TcpRpcError::Config(
                "client request timeout must be nonzero",
            ));
        }
        Ok(())
    }
}

/// Live outbound peer entry, publishable before constructing the buffer pool.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TcpRpcClientPeer {
    pub node: NodeId,
    pub name: String,
    pub server_name: String,
    pub address: SocketAddr,
}

/// Live peer directory and bounded persistent lane pool for one shard.
#[derive(Clone)]
pub struct ClientPeerDirectory {
    inner: Rc<RefCell<DirectoryInner>>,
}

impl ClientPeerDirectory {
    pub fn new(
        handle: NetHandle,
        tls: Rc<PeerTlsContext>,
        config: ClientPeerDirectoryConfig,
    ) -> Result<Self, TcpRpcError> {
        config.validate()?;
        Ok(Self {
            inner: Rc::new(RefCell::new(DirectoryInner {
                handle,
                tls,
                config,
                peers: HashMap::new(),
            })),
        })
    }

    /// Publish or replace a peer. Existing busy lanes finish on the retired entry.
    pub fn insert(&self, peer: TcpRpcClientPeer) -> Result<(), TcpRpcError> {
        validate_peer(&peer)?;
        let mut inner = self.inner.borrow_mut();
        let state = Rc::new(RefCell::new(PeerState::new(
            peer.clone(),
            inner.config.lane_count,
        )));
        if let Some(previous) = inner.peers.insert(peer.node, state) {
            previous.borrow_mut().retire();
        }
        Ok(())
    }

    pub fn remove(&self, node: NodeId) -> bool {
        let previous = self.inner.borrow_mut().peers.remove(&node);
        if let Some(previous) = previous {
            previous.borrow_mut().retire();
            true
        } else {
            false
        }
    }

    fn acquire(
        &self,
        node: NodeId,
        token: u64,
        waker: &Waker,
    ) -> Result<Option<LaneLease>, TcpRpcError> {
        let (peer, waiter_limit) = {
            let inner = self.inner.borrow();
            let peer = inner.peers.get(&node).cloned().ok_or_else(|| {
                TcpRpcError::Protocol(format!("outbound peer {} is unavailable", node.0))
            })?;
            (peer, inner.config.max_waiters_per_peer)
        };
        let lane_index = peer.borrow_mut().acquire(token, waker, waiter_limit)?;
        Ok(lane_index.map(|lane_index| {
            let fd = peer.borrow_mut().take_fd(lane_index);
            LaneLease {
                peer,
                lane_index,
                fd,
                released: false,
            }
        }))
    }

    fn cancel_waiter(&self, node: NodeId, token: u64) {
        if let Some(peer) = self.inner.borrow().peers.get(&node).cloned() {
            peer.borrow_mut().waiters.remove(&token);
        }
    }

    fn begin_connect(&self, lease: &LaneLease) -> LocalFuture<Result<RawFd, TcpRpcError>> {
        let (handle, tls, config) = {
            let inner = self.inner.borrow();
            (
                inner.handle.clone(),
                Rc::clone(&inner.tls),
                inner.config.clone(),
            )
        };
        let peer = lease.peer.borrow().peer.clone();
        let lane_index = lease.lane_index as u16;
        let address = peer.address;
        Box::pin(async move {
            let fd = create_socket(address, config.socket_buffer_bytes)?;
            let guard = FdGuard(fd);
            handle.connect(fd, socket_address(address)).await?;
            tls.connect(&handle, fd, &peer.server_name)
                .await
                .map_err(|error| {
                    TcpRpcError::Auth(format!("TLS handshake with {} failed: {error}", peer.name))
                })?;
            let handshake = Handshake {
                peer_name: config.local_peer_name.clone(),
                lane_index,
                lane_count: config.lane_count,
                max_page: config.max_page,
            };
            send_all(handle.clone(), fd, encode_handshake(&handshake)?).await?;
            let remote = recv_prefix(handle, fd).await?;
            validate_handshake(&remote, &peer, &config, lane_index)?;
            Ok(guard.into_raw())
        })
    }

    fn handle(&self) -> NetHandle {
        self.inner.borrow().handle.clone()
    }

    fn request_deadline(&self) -> Instant {
        Instant::now() + self.inner.borrow().config.request_timeout
    }
}

/// Bufferpool transport backed by the live peer directory.
pub struct TcpRpcTransport<R> {
    directory: ClientPeerDirectory,
    routes: RouteTableHandle,
    page_size: usize,
    page_count: u32,
    next_request_id: Rc<Cell<u64>>,
    _request: PhantomData<fn() -> R>,
}

impl<R> TcpRpcTransport<R> {
    pub fn new(
        directory: ClientPeerDirectory,
        routes: RouteTableHandle,
        page_size: usize,
        page_count: u32,
    ) -> Result<Self, TcpRpcError> {
        if page_size == 0 || page_count == 0 {
            return Err(TcpRpcError::Config("client page geometry must be nonzero"));
        }
        if page_size > super::wire::MAX_PAGE_BODY_LEN {
            return Err(TcpRpcError::Config(
                "client page size exceeds the wire limit",
            ));
        }
        page_size
            .checked_mul(page_count as usize)
            .ok_or(TcpRpcError::Config(
                "client backing geometry overflows usize",
            ))?;
        Ok(Self {
            directory,
            routes,
            page_size,
            page_count,
            next_request_id: Rc::new(Cell::new(1)),
            _request: PhantomData,
        })
    }

    pub fn bulk_get_with_ttl(
        &self,
        req: &R,
        src: BulkRef,
        dsts: &[PageRef],
        ttl: u8,
    ) -> TcpRpcStream
    where
        R: Req + Serialize,
    {
        let request_id = allocate_request_id(&self.next_request_id);
        match self.prepare(req, src, dsts, ttl, request_id) {
            Ok((peer, request)) => TcpRpcStream::new(
                self.directory.clone(),
                peer,
                request_id,
                request,
                dsts.to_vec(),
                self.page_size,
                self.page_count,
                src.offset,
            ),
            Err(error) => TcpRpcStream::failed(request_id, error),
        }
    }

    fn prepare(
        &self,
        req: &R,
        src: BulkRef,
        dsts: &[PageRef],
        ttl: u8,
        request_id: u64,
    ) -> Result<(NodeId, Vec<u8>), TcpRpcError>
    where
        R: Req + Serialize,
    {
        if dsts.is_empty() || dsts.len() > MAX_DESTINATION_PAGE_COUNT as usize {
            return Err(TcpRpcError::Config(
                "destination page count is out of range",
            ));
        }
        let destination_len = dsts
            .iter()
            .try_fold(0u64, |length, page| length.checked_add(page.len as u64));
        if destination_len != Some(src.len as u64) {
            return Err(TcpRpcError::Protocol(
                "destination page lengths do not cover the source range".to_string(),
            ));
        }
        PageValidator::new(dsts, self.page_size, self.page_count, src.offset)?;
        let peer = self
            .routes
            .route_for_req(req)
            .and_then(|fingers| fingers.next_hop(stripe_to_ring(req.key())))
            .map(|peer| peer.node)
            .ok_or_else(|| {
                TcpRpcError::Protocol("request has no outbound peer route".to_string())
            })?;
        let payload = bincode::serialize(req)
            .map_err(|error| TcpRpcError::Protocol(format!("request encode failed: {error}")))?;
        let metadata = RequestMetadata {
            stripe: src.stripe.0,
            src_offset: src.offset,
            src_len: src.len as u64,
            ttl,
            destination_page_count: dsts.len() as u32,
        };
        Ok((peer, encode_request(request_id, metadata, &payload)?))
    }
}

impl<R> Transport<R> for TcpRpcTransport<R>
where
    R: Req + Serialize + 'static,
{
    type Stream<'a>
        = TcpRpcStream
    where
        Self: 'a,
        R: 'a;

    fn bulk_get<'a>(&'a self, req: &'a R, src: BulkRef, dsts: &'a [PageRef]) -> Self::Stream<'a> {
        self.bulk_get_with_ttl(req, src, dsts, DEFAULT_TTL)
    }
}

/// Page stream for one request. It owns request bytes and destination metadata.
pub struct TcpRpcStream {
    directory: Option<ClientPeerDirectory>,
    peer: Option<NodeId>,
    request_id: u64,
    request: Option<Vec<u8>>,
    lease: Option<LaneLease>,
    validator: Option<PageValidator>,
    state: StreamState,
    deadline: Option<Instant>,
}

impl TcpRpcStream {
    fn new(
        directory: ClientPeerDirectory,
        peer: NodeId,
        request_id: u64,
        request: Vec<u8>,
        dsts: Vec<PageRef>,
        page_size: usize,
        page_count: u32,
        source_offset: u64,
    ) -> Self {
        let validator = PageValidator::new(&dsts, page_size, page_count, source_offset)
            .expect("destination pages were validated during request preparation");
        let deadline = directory.request_deadline();
        Self {
            directory: Some(directory),
            peer: Some(peer),
            request_id,
            request: Some(request),
            lease: None,
            validator: Some(validator),
            state: StreamState::Acquire,
            deadline: Some(deadline),
        }
    }

    fn failed(request_id: u64, error: TcpRpcError) -> Self {
        Self {
            directory: None,
            peer: None,
            request_id,
            request: None,
            lease: None,
            validator: None,
            state: StreamState::Failed(Some(error)),
            deadline: None,
        }
    }

    fn fail(&mut self, error: TcpRpcError) -> Poll<Option<Result<PageRef, PoolError>>> {
        record_error(&error);
        if let Some(lease) = self.lease.take() {
            lease.release(false);
        }
        self.deadline = None;
        self.state = StreamState::Done;
        Poll::Ready(Some(Err(to_pool_error(error))))
    }

    fn finish(&mut self, reusable: bool) {
        if let Some(lease) = self.lease.take() {
            lease.release(reusable);
        }
        self.deadline = None;
        self.state = StreamState::Done;
    }

    fn deadline_elapsed(&self) -> bool {
        self.deadline
            .is_some_and(|deadline| Instant::now() >= deadline)
    }
}

impl PageStream for TcpRpcStream {
    fn poll_next(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, PoolError>>> {
        loop {
            if self.deadline_elapsed() {
                if let (Some(directory), Some(peer)) = (&self.directory, self.peer) {
                    directory.cancel_waiter(peer, self.request_id);
                }
                return self.fail(TcpRpcError::Protocol(
                    "request deadline elapsed".to_string(),
                ));
            }
            let state = std::mem::replace(&mut self.state, StreamState::Done);
            match state {
                StreamState::Acquire => {
                    let directory = self.directory.as_ref().unwrap().clone();
                    match directory.acquire(self.peer.unwrap(), self.request_id, context.waker()) {
                        Ok(Some(lease)) => {
                            if let Some(fd) = lease.fd() {
                                let bytes = self.request.take().unwrap();
                                self.lease = Some(lease);
                                self.state =
                                    StreamState::Sending(send_all(directory.handle(), fd, bytes));
                            } else {
                                let future = directory.begin_connect(&lease);
                                self.lease = Some(lease);
                                self.state = StreamState::Connecting(future);
                            }
                        }
                        Ok(None) => {
                            self.state = StreamState::Acquire;
                            return Poll::Pending;
                        }
                        Err(error) => return self.fail(error),
                    }
                }
                StreamState::Connecting(mut future) => match future.as_mut().poll(context) {
                    Poll::Pending => {
                        self.state = StreamState::Connecting(future);
                        return Poll::Pending;
                    }
                    Poll::Ready(Ok(fd)) => {
                        self.lease.as_mut().unwrap().set_fd(fd);
                        let bytes = self.request.take().unwrap();
                        self.state = StreamState::Sending(send_all(
                            self.directory.as_ref().unwrap().handle(),
                            fd,
                            bytes,
                        ));
                    }
                    Poll::Ready(Err(error)) => return self.fail(error),
                },
                StreamState::Sending(mut future) => match future.as_mut().poll(context) {
                    Poll::Pending => {
                        self.state = StreamState::Sending(future);
                        return Poll::Pending;
                    }
                    Poll::Ready(Ok(())) => {
                        let fd = self.lease.as_ref().unwrap().fd().unwrap();
                        self.state = StreamState::ReadingPrefix(recv_prefix(
                            self.directory.as_ref().unwrap().handle(),
                            fd,
                        ));
                    }
                    Poll::Ready(Err(error)) => return self.fail(error),
                },
                StreamState::ReadingPrefix(mut future) => match future.as_mut().poll(context) {
                    Poll::Pending => {
                        self.state = StreamState::ReadingPrefix(future);
                        return Poll::Pending;
                    }
                    Poll::Ready(Ok(prefix)) => {
                        if prefix.header.request_id != self.request_id {
                            let request_id = self.request_id;
                            return self.fail(TcpRpcError::Protocol(format!(
                                "response request id {} does not match {}",
                                prefix.header.request_id, request_id
                            )));
                        }
                        match begin_response(&prefix, self.validator.as_mut().unwrap()) {
                            Ok(ResponseAction::Page(plan)) => {
                                let fd = self.lease.as_ref().unwrap().fd().unwrap();
                                self.state = StreamState::ReadingPage {
                                    plan,
                                    future: recv_fixed_exact(
                                        self.directory.as_ref().unwrap().handle(),
                                        fd,
                                        plan.byte_offset,
                                        plan.len,
                                    ),
                                };
                            }
                            Ok(ResponseAction::End) => {
                                if let Err(error) = self.validator.as_ref().unwrap().finish() {
                                    return self.fail(error);
                                }
                                self.finish(true);
                                return Poll::Ready(None);
                            }
                            Ok(ResponseAction::Error(error)) => {
                                self.finish(true);
                                if error.code == ERROR_ORIGIN_NOT_FOUND {
                                    return Poll::Ready(Some(Err(PoolError::OriginNotFound)));
                                }
                                return Poll::Ready(Some(Err(to_pool_error(
                                    TcpRpcError::Protocol(format!(
                                        "server error {}: {}",
                                        error.code, error.message
                                    )),
                                ))));
                            }
                            Err(error) => return self.fail(error),
                        }
                    }
                    Poll::Ready(Err(error)) => return self.fail(error),
                },
                StreamState::ReadingPage { plan, mut future } => {
                    match future.as_mut().poll(context) {
                        Poll::Pending => {
                            self.state = StreamState::ReadingPage { plan, future };
                            return Poll::Pending;
                        }
                        Poll::Ready(Ok(())) => {
                            let fd = self.lease.as_ref().unwrap().fd().unwrap();
                            self.state = StreamState::ReadingPrefix(recv_prefix(
                                self.directory.as_ref().unwrap().handle(),
                                fd,
                            ));
                            return Poll::Ready(Some(Ok(plan.page)));
                        }
                        Poll::Ready(Err(error)) => return self.fail(error),
                    }
                }
                StreamState::Failed(mut error) => {
                    self.state = StreamState::Done;
                    return Poll::Ready(Some(Err(to_pool_error(
                        error.take().expect("failed state is yielded once"),
                    ))));
                }
                StreamState::Done => {
                    self.state = StreamState::Done;
                    return Poll::Ready(None);
                }
            }
        }
    }
}

impl Drop for TcpRpcStream {
    fn drop(&mut self) {
        if matches!(self.state, StreamState::Done) {
            return;
        }
        if let (Some(directory), Some(peer)) = (&self.directory, self.peer) {
            directory.cancel_waiter(peer, self.request_id);
        }
        if let Some(lease) = self.lease.take() {
            // The server processes one request at a time and does not read
            // cancellation frames concurrently. Closing the lane is the only
            // unambiguous cancellation signal.
            lease.release(false);
        }
    }
}

enum StreamState {
    Acquire,
    Connecting(LocalFuture<Result<RawFd, TcpRpcError>>),
    Sending(LocalFuture<Result<(), TcpRpcError>>),
    ReadingPrefix(LocalFuture<Result<OwnedPrefix, TcpRpcError>>),
    ReadingPage {
        plan: PageReceivePlan,
        future: LocalFuture<Result<(), TcpRpcError>>,
    },
    Failed(Option<TcpRpcError>),
    Done,
}

struct DirectoryInner {
    handle: NetHandle,
    tls: Rc<PeerTlsContext>,
    config: ClientPeerDirectoryConfig,
    peers: HashMap<NodeId, Rc<RefCell<PeerState>>>,
}

struct PeerState {
    peer: TcpRpcClientPeer,
    lanes: Vec<Lane>,
    cursor: usize,
    retired: bool,
    waiters: HashMap<u64, Waker>,
}

impl PeerState {
    fn new(peer: TcpRpcClientPeer, lane_count: u16) -> Self {
        Self {
            peer,
            lanes: (0..lane_count).map(|_| Lane::default()).collect(),
            cursor: 0,
            retired: false,
            waiters: HashMap::new(),
        }
    }

    fn acquire(
        &mut self,
        token: u64,
        waker: &Waker,
        waiter_limit: usize,
    ) -> Result<Option<usize>, TcpRpcError> {
        if self.retired {
            return Err(TcpRpcError::Protocol("peer entry was retired".to_string()));
        }
        for offset in 0..self.lanes.len() {
            let index = (self.cursor + offset) % self.lanes.len();
            if !self.lanes[index].busy {
                self.lanes[index].busy = true;
                self.cursor = (index + 1) % self.lanes.len();
                self.waiters.remove(&token);
                return Ok(Some(index));
            }
        }
        if !self.waiters.contains_key(&token) && self.waiters.len() >= waiter_limit {
            return Err(TcpRpcError::Protocol(format!(
                "peer {} client lane wait queue is full",
                self.peer.node.0
            )));
        }
        if self.waiters.insert(token, waker.clone()).is_none() {
            crate::metrics::tcp_rpc_lane_wait();
        }
        Ok(None)
    }

    fn take_fd(&mut self, lane_index: usize) -> Option<RawFd> {
        self.lanes[lane_index].fd.take()
    }

    fn release(&mut self, lane_index: usize, fd: Option<RawFd>) {
        let lane = &mut self.lanes[lane_index];
        lane.busy = false;
        if self.retired {
            close_optional(fd);
            close_optional(lane.fd.take());
        } else if let Some(fd) = fd {
            lane.completed_requests += 1;
            if lane.completed_requests >= MAX_REQUESTS_PER_CONNECTION {
                close_optional(Some(fd));
                close_optional(lane.fd.take());
                lane.completed_requests = 0;
            } else {
                close_optional(lane.fd.replace(fd));
            }
        } else {
            close_optional(lane.fd.take());
            lane.completed_requests = 0;
        }
        if let Some(waker) = self.waiters.values().next().cloned() {
            waker.wake();
        }
    }

    fn retire(&mut self) {
        self.retired = true;
        for lane in &mut self.lanes {
            if !lane.busy {
                close_optional(lane.fd.take());
            }
        }
        for (_, waker) in self.waiters.drain() {
            waker.wake();
        }
    }
}

#[derive(Default)]
struct Lane {
    fd: Option<RawFd>,
    busy: bool,
    completed_requests: usize,
}

impl Drop for Lane {
    fn drop(&mut self) {
        close_optional(self.fd.take());
    }
}

struct LaneLease {
    peer: Rc<RefCell<PeerState>>,
    lane_index: usize,
    fd: Option<RawFd>,
    released: bool,
}

impl LaneLease {
    fn fd(&self) -> Option<RawFd> {
        self.fd
    }

    fn set_fd(&mut self, fd: RawFd) {
        self.fd = Some(fd);
    }

    fn release(mut self, reusable: bool) {
        let fd = self.fd.take();
        self.peer
            .borrow_mut()
            .release(self.lane_index, fd.filter(|_| reusable));
        if !reusable {
            close_optional(fd);
        }
        self.released = true;
    }
}

impl Drop for LaneLease {
    fn drop(&mut self) {
        if !self.released {
            let fd = self.fd.take();
            self.peer.borrow_mut().release(self.lane_index, fd);
        }
    }
}

struct PageValidator {
    dsts: Vec<PageRef>,
    seen: Vec<bool>,
    page_size: usize,
    source_offset: u64,
    delivered: usize,
}

impl PageValidator {
    fn new(
        dsts: &[PageRef],
        page_size: usize,
        page_count: u32,
        source_offset: u64,
    ) -> Result<Self, TcpRpcError> {
        for page in dsts {
            validate_page_ref(*page, page_size, page_count)?;
        }
        Ok(Self {
            dsts: dsts.to_vec(),
            seen: vec![false; dsts.len()],
            page_size,
            source_offset,
            delivered: 0,
        })
    }

    fn prepare(
        &mut self,
        metadata: PageMetadata,
        body_len: usize,
    ) -> Result<PageReceivePlan, TcpRpcError> {
        let ordinal = metadata.ordinal as usize;
        let page = *self.dsts.get(ordinal).ok_or_else(|| {
            TcpRpcError::Protocol(format!("page ordinal {} is out of range", metadata.ordinal))
        })?;
        if self.seen[ordinal] {
            return Err(TcpRpcError::Protocol(format!(
                "duplicate page ordinal {}",
                metadata.ordinal
            )));
        }
        let expected_offset = self
            .dsts
            .iter()
            .take(ordinal)
            .try_fold(self.source_offset, |offset, page| {
                offset.checked_add(page.len as u64)
            })
            .ok_or_else(|| TcpRpcError::Protocol("source page offset overflow".to_string()))?;
        if metadata.page_offset != expected_offset || body_len != page.len as usize {
            return Err(TcpRpcError::Protocol(format!(
                "page ordinal {} range does not match destination",
                metadata.ordinal
            )));
        }
        let byte_offset = (page.page_idx as usize)
            .checked_mul(self.page_size)
            .and_then(|offset| offset.checked_add(page.offset as usize))
            .ok_or_else(|| TcpRpcError::Protocol("destination offset overflow".to_string()))?;
        self.seen[ordinal] = true;
        self.delivered += 1;
        Ok(PageReceivePlan {
            page,
            byte_offset,
            len: body_len,
        })
    }

    fn finish(&self) -> Result<(), TcpRpcError> {
        if self.delivered == self.dsts.len() {
            Ok(())
        } else {
            Err(TcpRpcError::Protocol(format!(
                "response ended after {} of {} pages",
                self.delivered,
                self.dsts.len()
            )))
        }
    }
}

#[derive(Clone, Copy)]
struct PageReceivePlan {
    page: PageRef,
    byte_offset: usize,
    len: usize,
}

struct OwnedPrefix {
    header: FrameHeader,
    metadata: Vec<u8>,
}

impl OwnedPrefix {
    fn borrowed(&self) -> FramePrefix<'_> {
        FramePrefix {
            header: self.header,
            metadata: &self.metadata,
        }
    }
}

enum ResponseAction {
    Page(PageReceivePlan),
    End,
    Error(ErrorMetadata),
}

fn begin_response(
    prefix: &OwnedPrefix,
    validator: &mut PageValidator,
) -> Result<ResponseAction, TcpRpcError> {
    match prefix.header.kind {
        FrameKind::Page => {
            let DecodedMetadata::Page(metadata) = prefix.borrowed().decode_metadata()? else {
                unreachable!();
            };
            Ok(ResponseAction::Page(
                validator.prepare(metadata, prefix.header.payload_len as usize)?,
            ))
        }
        FrameKind::End => Ok(ResponseAction::End),
        FrameKind::Error => {
            let DecodedMetadata::Error(error) = prefix.borrowed().decode_metadata()? else {
                unreachable!();
            };
            Ok(ResponseAction::Error(error))
        }
        kind => Err(TcpRpcError::Protocol(format!(
            "unexpected response frame {kind:?}"
        ))),
    }
}

fn validate_handshake(
    prefix: &OwnedPrefix,
    peer: &TcpRpcClientPeer,
    config: &ClientPeerDirectoryConfig,
    lane_index: u16,
) -> Result<(), TcpRpcError> {
    if prefix.header.kind != FrameKind::Handshake || prefix.header.payload_len != 0 {
        return Err(TcpRpcError::Protocol(
            "peer did not answer with a handshake".to_string(),
        ));
    }
    let DecodedMetadata::Handshake(remote) = prefix.borrowed().decode_metadata()? else {
        unreachable!();
    };
    if remote.peer_name != peer.name
        || remote.lane_index != lane_index
        || remote.lane_count != config.lane_count
        || remote.max_page < config.max_page
    {
        return Err(TcpRpcError::Auth(
            "peer handshake identity or lane geometry mismatch".to_string(),
        ));
    }
    Ok(())
}

fn validate_page_ref(page: PageRef, page_size: usize, page_count: u32) -> Result<(), TcpRpcError> {
    if page.page_idx >= page_count {
        return Err(TcpRpcError::Protocol(format!(
            "destination page {} is outside backing",
            page.page_idx
        )));
    }
    let end = (page.offset as usize)
        .checked_add(page.len as usize)
        .ok_or_else(|| TcpRpcError::Protocol("destination page range overflow".to_string()))?;
    if page.len == 0 || end > page_size {
        return Err(TcpRpcError::Protocol(
            "destination page range is outside page".to_string(),
        ));
    }
    Ok(())
}

fn allocate_request_id(next: &Cell<u64>) -> u64 {
    let request_id = next.get().max(1);
    next.set(request_id.wrapping_add(1).max(1));
    request_id
}

fn send_all(handle: NetHandle, fd: RawFd, bytes: Vec<u8>) -> LocalFuture<Result<(), TcpRpcError>> {
    Box::pin(async move {
        let mut sent = 0;
        while sent < bytes.len() {
            let count = handle.send(fd, bytes[sent..].to_vec()).await?;
            if count == 0 {
                return Err(TcpRpcError::Protocol(
                    "peer disconnected during send".to_string(),
                ));
            }
            sent += count;
        }
        Ok(())
    })
}

fn recv_prefix(handle: NetHandle, fd: RawFd) -> LocalFuture<Result<OwnedPrefix, TcpRpcError>> {
    Box::pin(async move {
        let header_bytes = recv_exact_heap(&handle, fd, HEADER_LEN).await?;
        let header = match FrameHeader::decode(&header_bytes)? {
            DecodeStatus::Complete { value, .. } => value,
            DecodeStatus::Incomplete { .. } => {
                return Err(TcpRpcError::Protocol(
                    "complete header decoded as partial".to_string(),
                ));
            }
        };
        let metadata = recv_exact_heap(&handle, fd, header.metadata_len as usize).await?;
        let mut encoded = header_bytes;
        encoded.extend_from_slice(&metadata);
        match decode_prefix(&encoded)? {
            DecodeStatus::Complete { .. } => Ok(OwnedPrefix { header, metadata }),
            DecodeStatus::Incomplete { .. } => Err(TcpRpcError::Protocol(
                "complete prefix decoded as partial".to_string(),
            )),
        }
    })
}

fn recv_fixed_exact(
    handle: NetHandle,
    fd: RawFd,
    page_byte_offset: usize,
    len: usize,
) -> LocalFuture<Result<(), TcpRpcError>> {
    Box::pin(async move {
        let mut received = 0;
        while received < len {
            let offset = page_byte_offset.checked_add(received).ok_or_else(|| {
                TcpRpcError::Protocol("fixed receive offset overflow".to_string())
            })?;
            let count = handle
                .recv_fixed_with_flags(fd, 0, offset, len - received, libc::MSG_WAITALL)
                .await?;
            if count == 0 {
                return Err(TcpRpcError::Protocol(
                    "peer disconnected during page receive".to_string(),
                ));
            }
            crate::metrics::tcp_rpc_payload_received(count as u64);
            received += count;
        }
        crate::metrics::tcp_rpc_page_received();
        Ok(())
    })
}

async fn recv_exact_heap(
    handle: &NetHandle,
    fd: RawFd,
    len: usize,
) -> Result<Vec<u8>, TcpRpcError> {
    let mut output = Vec::with_capacity(len);
    while output.len() < len {
        let data = recv_heap_application(handle, fd, len - output.len()).await?;
        if data.is_empty() {
            return Err(TcpRpcError::Protocol("peer disconnected".to_string()));
        }
        output.extend_from_slice(&data);
    }
    Ok(output)
}

async fn recv_heap_application(
    handle: &NetHandle,
    fd: RawFd,
    max_len: usize,
) -> Result<Vec<u8>, TcpRpcError> {
    let (data, record_type) = handle.recv_msg(fd, max_len).await?;
    if data.is_empty() {
        return Ok(data);
    }
    match record_type {
        TLS_RECORD_TYPE_APPLICATION_DATA => Ok(data),
        TLS_RECORD_TYPE_ALERT => classify_alert(data.get(1).copied()),
        TLS_RECORD_TYPE_HANDSHAKE => Err(post_handshake_record_error()),
        record_type => Err(TcpRpcError::Protocol(format!(
            "unsupported TLS record type {record_type}"
        ))),
    }
}

fn post_handshake_record_error() -> TcpRpcError {
    TcpRpcError::Protocol("TLS post-handshake update requires connection rotation".to_string())
}

fn classify_alert<T: Default>(description: Option<u8>) -> Result<T, TcpRpcError> {
    match description {
        Some(0) | None => Ok(T::default()),
        Some(description) => Err(TcpRpcError::Protocol(format!(
            "fatal TLS alert {description}"
        ))),
    }
}

fn validate_peer(peer: &TcpRpcClientPeer) -> Result<(), TcpRpcError> {
    if peer.name.is_empty() || peer.server_name.is_empty() {
        return Err(TcpRpcError::Config(
            "peer name and server name must be nonempty",
        ));
    }
    if peer.address.port() == 0 {
        return Err(TcpRpcError::Config("peer port must be nonzero"));
    }
    Ok(())
}

fn create_socket(address: SocketAddr, socket_buffer_bytes: u32) -> Result<RawFd, TcpRpcError> {
    let family = if address.is_ipv4() {
        libc::AF_INET
    } else {
        libc::AF_INET6
    };
    // SAFETY: socket and setsockopt receive initialized scalar arguments.
    unsafe {
        let fd = libc::socket(family, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0);
        if fd < 0 {
            return Err(std::io::Error::last_os_error().into());
        }
        let guard = FdGuard(fd);
        let buffer_value = socket_buffer_bytes as libc::c_int;
        let buffer_pointer = (&buffer_value as *const libc::c_int).cast();
        let length = std::mem::size_of_val(&buffer_value) as libc::socklen_t;
        let enabled = 1 as libc::c_int;
        if libc::setsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_SNDBUF,
            buffer_pointer,
            length,
        ) != 0
            || libc::setsockopt(
                fd,
                libc::SOL_SOCKET,
                libc::SO_RCVBUF,
                buffer_pointer,
                length,
            ) != 0
            || libc::setsockopt(
                fd,
                libc::IPPROTO_TCP,
                libc::TCP_NODELAY,
                (&enabled as *const libc::c_int).cast(),
                std::mem::size_of_val(&enabled) as libc::socklen_t,
            ) != 0
        {
            return Err(std::io::Error::last_os_error().into());
        }
        Ok(guard.into_raw())
    }
}

fn socket_address(address: SocketAddr) -> SockAddr {
    match address {
        SocketAddr::V4(address) => {
            let raw = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: address.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(*address.ip()).to_be(),
                },
                sin_zero: [0; 8],
            };
            SockAddr::from_sockaddr_in(raw)
        }
        SocketAddr::V6(address) => {
            let raw = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as libc::sa_family_t,
                sin6_port: address.port().to_be(),
                sin6_flowinfo: address.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: address.ip().octets(),
                },
                sin6_scope_id: address.scope_id(),
            };
            // SAFETY: raw is a fully initialized sockaddr_in6.
            unsafe {
                SockAddr::from_raw(
                    (&raw as *const libc::sockaddr_in6).cast(),
                    std::mem::size_of_val(&raw) as libc::socklen_t,
                )
            }
        }
    }
}

fn to_pool_error(error: TcpRpcError) -> PoolError {
    match error {
        TcpRpcError::Io(error) => match error.raw_os_error() {
            Some(code) => PoolError::Io(code),
            None => PoolError::transport(error),
        },
        error => PoolError::transport(error),
    }
}

fn close_optional(fd: Option<RawFd>) {
    if let Some(fd) = fd {
        drop(FdGuard(fd));
    }
}

struct FdGuard(RawFd);

impl FdGuard {
    fn into_raw(self) -> RawFd {
        let fd = self.0;
        std::mem::forget(self);
        fd
    }
}

impl Drop for FdGuard {
    fn drop(&mut self) {
        // SAFETY: the guard uniquely owns the descriptor.
        unsafe {
            libc::close(self.0);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    #[derive(Serialize)]
    struct TestReq {
        key: crate::bufferpool::StripeKey,
    }

    impl Req for TestReq {
        fn key(&self) -> crate::bufferpool::StripeKey {
            self.key
        }
    }

    fn peer() -> TcpRpcClientPeer {
        TcpRpcClientPeer {
            node: NodeId(7),
            name: "peer.test".to_string(),
            server_name: "peer.test".to_string(),
            address: "127.0.0.1:9443".parse().unwrap(),
        }
    }

    fn pages() -> Vec<PageRef> {
        vec![
            PageRef {
                page_idx: 3,
                offset: 16,
                len: 32,
            },
            PageRef {
                page_idx: 7,
                offset: 0,
                len: 64,
            },
        ]
    }

    #[test]
    fn config_validates_request_timeout() {
        let mut config = ClientPeerDirectoryConfig {
            local_peer_name: "local.test".to_string(),
            lane_count: 1,
            max_waiters_per_peer: 1,
            max_page: 4096,
            socket_buffer_bytes: 4096,
            request_timeout: Duration::ZERO,
        };
        assert!(matches!(config.validate(), Err(TcpRpcError::Config(_))));
        config.request_timeout = Duration::from_millis(1);
        assert!(config.validate().is_ok());
    }

    #[test]
    fn peer_requires_nonzero_port() {
        let mut peer = peer();
        peer.address = "127.0.0.1:0".parse().unwrap();
        assert!(matches!(validate_peer(&peer), Err(TcpRpcError::Config(_))));
    }

    #[test]
    fn elapsed_deadline_yields_one_transport_error() {
        let mut stream = Box::pin(TcpRpcStream {
            directory: None,
            peer: None,
            request_id: 1,
            request: None,
            lease: None,
            validator: None,
            state: StreamState::Acquire,
            deadline: Some(Instant::now()),
        });
        let waker = noop_waker();
        let mut context = Context::from_waker(&waker);
        assert!(matches!(
            stream.as_mut().poll_next(&mut context),
            Poll::Ready(Some(Err(PoolError::Transport(_))))
        ));
        assert!(matches!(
            stream.as_mut().poll_next(&mut context),
            Poll::Ready(None)
        ));
    }

    #[test]
    fn lane_admission_is_bounded_and_round_robin() {
        let mut state = PeerState::new(peer(), 2);
        let waker = noop_waker();
        assert_eq!(state.acquire(1, &waker, 1).unwrap(), Some(0));
        assert_eq!(state.acquire(2, &waker, 1).unwrap(), Some(1));
        assert_eq!(state.acquire(3, &waker, 1).unwrap(), None);
        assert!(state.acquire(4, &waker, 1).is_err());
        state.release(0, None);
        assert_eq!(state.acquire(3, &waker, 1).unwrap(), Some(0));
    }

    #[test]
    fn completed_request_limit_retires_lane_connection() {
        let mut state = PeerState::new(peer(), 1);
        state.lanes[0].busy = true;
        state.lanes[0].completed_requests = MAX_REQUESTS_PER_CONNECTION - 1;
        let fd = unsafe { libc::dup(libc::STDIN_FILENO) };
        assert!(fd >= 0);

        state.release(0, Some(fd));

        assert!(state.lanes[0].fd.is_none());
        assert_eq!(state.lanes[0].completed_requests, 0);
        assert_eq!(unsafe { libc::fcntl(fd, libc::F_GETFD) }, -1);
    }

    #[test]
    fn page_validation_maps_ordinal_to_fixed_buffer_zero_offset() {
        let mut validator = PageValidator::new(&pages(), 4096, 8, 0).unwrap();
        let plan = validator
            .prepare(
                PageMetadata {
                    ordinal: 1,
                    page_offset: 32,
                },
                64,
            )
            .unwrap();
        assert_eq!(plan.page, pages()[1]);
        assert_eq!(plan.byte_offset, 7 * 4096);
    }

    #[test]
    fn page_validation_rejects_bad_ordinal_range_and_duplicate() {
        let mut validator = PageValidator::new(&pages(), 4096, 8, 0).unwrap();
        assert!(
            validator
                .prepare(
                    PageMetadata {
                        ordinal: 2,
                        page_offset: 0,
                    },
                    64,
                )
                .is_err()
        );
        assert!(
            validator
                .prepare(
                    PageMetadata {
                        ordinal: 0,
                        page_offset: 1,
                    },
                    32,
                )
                .is_err()
        );
        validator
            .prepare(
                PageMetadata {
                    ordinal: 0,
                    page_offset: 0,
                },
                32,
            )
            .unwrap();
        assert!(
            validator
                .prepare(
                    PageMetadata {
                        ordinal: 0,
                        page_offset: 0,
                    },
                    32,
                )
                .is_err()
        );
    }

    #[test]
    fn end_requires_every_destination() {
        let mut validator = PageValidator::new(&pages(), 4096, 8, 0).unwrap();
        validator
            .prepare(
                PageMetadata {
                    ordinal: 0,
                    page_offset: 0,
                },
                32,
            )
            .unwrap();
        assert!(validator.finish().is_err());
        validator
            .prepare(
                PageMetadata {
                    ordinal: 1,
                    page_offset: 32,
                },
                64,
            )
            .unwrap();
        assert!(validator.finish().is_ok());
    }

    #[test]
    fn request_ids_skip_zero_after_wraparound() {
        let next = Cell::new(u64::MAX);
        assert_eq!(allocate_request_id(&next), u64::MAX);
        assert_eq!(allocate_request_id(&next), 1);
        assert_eq!(allocate_request_id(&next), 2);
    }

    #[test]
    fn request_metadata_carries_source_offset_and_ttl() {
        let req = TestReq {
            key: crate::bufferpool::StripeKey([9; 32]),
        };
        let src = BulkRef {
            stripe: req.key(),
            offset: 123,
            len: 96,
        };
        let payload = bincode::serialize(&req).unwrap();
        let encoded = encode_request(
            7,
            RequestMetadata {
                stripe: src.stripe.0,
                src_offset: src.offset,
                src_len: src.len as u64,
                ttl: 5,
                destination_page_count: 2,
            },
            &payload,
        )
        .unwrap();
        let frame = match super::super::wire::decode_frame(&encoded).unwrap() {
            DecodeStatus::Complete { value, .. } => value,
            DecodeStatus::Incomplete { .. } => panic!("request frame is incomplete"),
        };
        let DecodedMetadata::Request(metadata) = FramePrefix {
            header: frame.header,
            metadata: frame.metadata,
        }
        .decode_metadata()
        .unwrap() else {
            panic!("request metadata expected");
        };
        assert_eq!(metadata.src_offset, 123);
        assert_eq!(metadata.src_len, 96);
        assert_eq!(metadata.ttl, 5);
        assert_eq!(metadata.destination_page_count, 2);
    }
}
