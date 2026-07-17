// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Authenticated connection protocol and ordered zero-copy response path.

use std::cmp::Ordering;
use std::collections::BinaryHeap;
use std::os::fd::RawFd;
use std::rc::Rc;

use crate::bufferpool::{Error as PoolError, Req, StripeKey};
use crate::fanout::{FetchEvent, FetchPage, FetchStream, Owner};
use crate::metrics;
use crate::p2p::stripe_to_ring;
use crate::ring::{
    TLS_RECORD_TYPE_ALERT, TLS_RECORD_TYPE_APPLICATION_DATA, TLS_RECORD_TYPE_HANDSHAKE,
};
use crate::storage::StripeReq;

use super::server::{AdmissionPermit, TcpRpcError, TcpRpcService, record_error};
use super::wire::{
    DecodeStatus, ErrorMetadata, FrameHeader, FrameKind, Handshake, PageMetadata, RequestMetadata,
    decode_frame, encode_end, encode_error, encode_handshake, encode_page_prefix,
};

const RECV_CHUNK: usize = 64 * 1024;
// Bound aggregate queued bursts while avoiding excessive completion round
// trips when a peer is configured with only a few lanes.
const REGISTERED_SEND_BUDGET: usize = 1024 * 1024;
const MIN_REGISTERED_SEND_CHUNK: usize = 128 * 1024;
const ERROR_BAD_REQUEST: u32 = 400;
const ERROR_UNAUTHORIZED: u32 = 401;
const ERROR_ORIGIN_NOT_FOUND: u32 = 404;
const ERROR_OVERLOADED: u32 = 429;
const ERROR_SERVICE: u32 = 500;
const ERROR_HOP_LIMIT: u32 = 508;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum RegisteredSendMode {
    SendZc,
    KtlsWriteFixed,
}

pub(super) async fn serve_connection(service: Rc<TcpRpcService>, fd: RawFd) {
    let _fd = FdGuard(fd);
    let _connection = ConnectionMetricGuard::new();
    let identity = match service.tls.accept(&service.handle, fd).await {
        Ok(identity) => identity,
        Err(_) => {
            metrics::tcp_rpc_auth_error();
            return;
        }
    };
    let mut reader = FrameReader::new(service.handle.clone(), fd);
    let handshake_frame = match reader.next(FrameKind::Handshake).await {
        Ok(Some(frame)) => frame,
        Ok(None) => {
            metrics::tcp_rpc_connection_error();
            return;
        }
        Err(error) => {
            record_error(&error);
            return;
        }
    };
    let handshake = match decode_handshake(&handshake_frame) {
        Ok(handshake) => handshake,
        Err(error) => {
            record_error(&error);
            return;
        }
    };
    if !service.peers.authenticate(&handshake.peer_name, &identity)
        || handshake.lane_count != service.config.lane_count
        || handshake.max_page < service.config.max_page
    {
        metrics::tcp_rpc_auth_error();
        let _ = send_error(
            &service,
            fd,
            1,
            ERROR_UNAUTHORIZED,
            "peer handshake is not authorized",
        )
        .await;
        return;
    }

    let response = Handshake {
        peer_name: service.config.local_peer_name.clone(),
        lane_index: handshake.lane_index,
        lane_count: service.config.lane_count,
        max_page: service.config.max_page,
    };
    let Ok(bytes) = encode_handshake(&response) else {
        metrics::tcp_rpc_protocol_error();
        return;
    };
    if let Err(error) = send_heap_all(&service, fd, bytes).await {
        record_error(&error);
        return;
    }

    let mut requests = 0usize;
    let mut send_mode = RegisteredSendMode::SendZc;
    while requests < service.config.max_requests_per_connection {
        let frame = match reader.next(FrameKind::Request).await {
            Ok(Some(frame)) => frame,
            Ok(None) => return,
            Err(error) => {
                record_error(&error);
                return;
            }
        };
        requests += 1;

        let Some(permit) = service.try_admit() else {
            metrics::tcp_rpc_request(metrics::Outcome::Err);
            if send_error(
                &service,
                fd,
                frame.header.request_id,
                ERROR_OVERLOADED,
                "server request limit reached",
            )
            .await
            .is_err()
            {
                return;
            }
            continue;
        };
        if let Err(error) = serve_request(&service, fd, frame, permit, &mut send_mode).await {
            record_error(&error);
            return;
        }
    }
}

async fn serve_request(
    service: &Rc<TcpRpcService>,
    fd: RawFd,
    frame: OwnedFrame,
    mut permit: AdmissionPermit,
    send_mode: &mut RegisteredSendMode,
) -> Result<(), TcpRpcError> {
    let request_id = frame.header.request_id;
    let metadata = match RequestMetadata::decode_metadata(&frame.metadata) {
        Ok(metadata) => metadata,
        Err(error) => {
            return send_request_error(service, fd, request_id, error.to_string()).await;
        }
    };
    let mut request: StripeReq = match bincode::deserialize(&frame.payload) {
        Ok(request) => request,
        Err(error) => {
            return send_request_error(service, fd, request_id, error.to_string()).await;
        }
    };
    if request.key() != StripeKey(metadata.stripe) {
        return send_request_error(
            service,
            fd,
            request_id,
            "request key does not match frame metadata".to_string(),
        )
        .await;
    }
    let forwarding_needed = service
        .routes
        .route_for_req(&request)
        .is_some_and(|fingers| fingers.next_hop(stripe_to_ring(request.key())).is_some());
    let peer_ttl = match forwarded_peer_ttl(metadata.ttl, forwarding_needed) {
        Ok(peer_ttl) => peer_ttl,
        Err(()) => {
            return send_error(
                service,
                fd,
                request_id,
                ERROR_HOP_LIMIT,
                "recursive forward hop limit exceeded",
            )
            .await;
        }
    };
    if peer_ttl.is_some() {
        request = request.with_peer_ttl(peer_ttl);
    }
    let expected_pages = match expected_page_count(
        metadata.src_offset,
        metadata.src_len,
        service.config.max_page,
    ) {
        Some(count)
            if count == metadata.destination_page_count
                && count <= service.config.max_pages_per_request =>
        {
            count
        }
        _ => {
            return send_request_error(
                service,
                fd,
                request_id,
                "destination page count does not match the source range".to_string(),
            )
            .await;
        }
    };

    let (channel, buf_index) =
        match service
            .fanout
            .owner_of_cache(&request.key(), request.cache_id(), metadata.src_offset)
        {
            Owner::Local => (service.local_fetch.clone(), 0),
            Owner::Peer(peer) => (peer.channel.clone(), peer.buf_index),
        };
    let mut stream = match channel.fetch(request, metadata.src_offset, metadata.src_len) {
        Ok(stream) => stream,
        Err(error) => {
            return send_fetch_error(service, fd, request_id, &error).await;
        }
    };

    match send_fetch(
        service,
        fd,
        request_id,
        metadata,
        expected_pages,
        buf_index,
        &mut stream,
        send_mode,
    )
    .await
    {
        Ok(()) => {
            send_heap_all(service, fd, encode_end(request_id)?).await?;
            permit.set_outcome(metrics::Outcome::Ok);
            Ok(())
        }
        Err(FetchFailure::Reply(message)) => {
            send_service_error(service, fd, request_id, message).await?;
            permit.set_outcome(metrics::Outcome::Err);
            Ok(())
        }
        Err(FetchFailure::Fetch(error)) => {
            send_fetch_error(service, fd, request_id, &error).await?;
            permit.set_outcome(metrics::Outcome::Err);
            Ok(())
        }
        Err(FetchFailure::Connection(error)) => Err(error),
    }
}

async fn send_fetch(
    service: &TcpRpcService,
    fd: RawFd,
    request_id: u64,
    metadata: RequestMetadata,
    expected_pages: u32,
    buf_index: u16,
    stream: &mut FetchStream,
    send_mode: &mut RegisteredSendMode,
) -> Result<(), FetchFailure> {
    let mut heap: BinaryHeap<OrderedPage> = BinaryHeap::new();
    let mut seen = vec![false; expected_pages as usize];
    let mut next_ordinal = 0u32;
    let mut logical_offset = metadata.src_offset;
    let range_end = metadata
        .src_offset
        .checked_add(metadata.src_len)
        .ok_or_else(|| FetchFailure::Reply("source range overflow".to_string()))?;
    let mut done = false;

    loop {
        while heap
            .peek()
            .is_some_and(|page| page.page.ordinal == next_ordinal as usize)
        {
            let page = heap.pop().expect("peeked page remains").page;
            let page_end = logical_offset
                .checked_add(page.loc.len as u64)
                .ok_or_else(|| FetchFailure::Reply("page range overflow".to_string()))?;
            if page_end > range_end || page.loc.len == 0 {
                stream.release(page.pin_token);
                return Err(FetchFailure::Reply(
                    "owner returned a page outside the requested range".to_string(),
                ));
            }
            send_page(
                service,
                fd,
                request_id,
                next_ordinal,
                logical_offset,
                buf_index,
                stream,
                page,
                send_mode,
            )
            .await?;
            logical_offset = page_end;
            next_ordinal += 1;
        }

        if done {
            return if next_ordinal == expected_pages && logical_offset == range_end {
                Ok(())
            } else {
                Err(FetchFailure::Reply(
                    "owner ended before returning the complete range".to_string(),
                ))
            };
        }

        match stream.next_event().await {
            Ok(FetchEvent::Page(page)) => {
                if page.ordinal >= seen.len() || seen[page.ordinal] {
                    stream.release(page.pin_token);
                    return Err(FetchFailure::Reply(
                        "owner returned an invalid page ordinal".to_string(),
                    ));
                }
                seen[page.ordinal] = true;
                heap.push(OrderedPage { page });
            }
            Ok(FetchEvent::Done) => done = true,
            Err(error) => return Err(FetchFailure::Fetch(error)),
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn send_page(
    service: &TcpRpcService,
    fd: RawFd,
    request_id: u64,
    ordinal: u32,
    logical_offset: u64,
    buf_index: u16,
    stream: &FetchStream,
    page: FetchPage,
    send_mode: &mut RegisteredSendMode,
) -> Result<(), FetchFailure> {
    let mut offset = usize::try_from(page.loc.page_byte_offset)
        .map_err(|_| FetchFailure::Reply("registered page offset exceeds usize".to_string()))?;
    let prefix = encode_page_prefix(
        request_id,
        PageMetadata {
            ordinal,
            page_offset: logical_offset,
        },
        page.loc.len as usize,
    )
    .map_err(TcpRpcError::from)
    .map_err(FetchFailure::Connection)?;
    send_heap_all_with_flags(service, fd, prefix, libc::MSG_MORE)
        .await
        .map_err(FetchFailure::Connection)?;

    let release = stream.pin_release_hold(page.pin_token);
    let mut remaining = page.loc.len as usize;
    let mut page_used_fallback = *send_mode == RegisteredSendMode::KtlsWriteFixed;
    while remaining > 0 {
        let requested = registered_send_len(remaining, service.config.lane_count);
        let sent = match *send_mode {
            RegisteredSendMode::SendZc => match service
                .handle
                .send_zc_fixed_with_completion(
                    fd,
                    buf_index,
                    offset,
                    requested,
                    Box::new(release.token()),
                )
                .await
            {
                Ok(sent) => sent,
                Err(error) if error.raw_os_error() == Some(libc::EOPNOTSUPP) => {
                    *send_mode = RegisteredSendMode::KtlsWriteFixed;
                    page_used_fallback = true;
                    service
                        .handle
                        .write_fixed_with_completion(
                            fd,
                            buf_index,
                            offset,
                            requested,
                            Box::new(release.token()),
                        )
                        .await
                        .map_err(TcpRpcError::from)
                        .map_err(FetchFailure::Connection)?
                }
                Err(error) => {
                    return Err(FetchFailure::Connection(TcpRpcError::from(error)));
                }
            },
            RegisteredSendMode::KtlsWriteFixed => service
                .handle
                .write_fixed_with_completion(
                    fd,
                    buf_index,
                    offset,
                    requested,
                    Box::new(release.token()),
                )
                .await
                .map_err(TcpRpcError::from)
                .map_err(FetchFailure::Connection)?,
        };
        if sent < requested {
            metrics::tcp_rpc_short_send();
        }
        if sent == 0 {
            return Err(FetchFailure::Connection(TcpRpcError::Protocol(
                "zero-length registered-source SEND_ZC completion".to_string(),
            )));
        }
        metrics::tcp_rpc_payload_sent(sent as u64);
        offset += sent;
        remaining -= sent;
    }
    release.close();
    if page_used_fallback {
        metrics::tcp_rpc_send_zc_fallback();
    }
    metrics::tcp_rpc_page_sent();
    Ok(())
}

fn registered_send_len(remaining: usize, lane_count: u16) -> usize {
    let chunk =
        (REGISTERED_SEND_BUDGET / usize::from(lane_count.max(1))).max(MIN_REGISTERED_SEND_CHUNK);
    remaining.min(chunk)
}

async fn send_request_error(
    service: &TcpRpcService,
    fd: RawFd,
    request_id: u64,
    message: String,
) -> Result<(), TcpRpcError> {
    metrics::tcp_rpc_protocol_error();
    send_error(service, fd, request_id, ERROR_BAD_REQUEST, &message).await
}

async fn send_service_error(
    service: &TcpRpcService,
    fd: RawFd,
    request_id: u64,
    message: String,
) -> Result<(), TcpRpcError> {
    send_error(service, fd, request_id, ERROR_SERVICE, &message).await
}

async fn send_fetch_error(
    service: &TcpRpcService,
    fd: RawFd,
    request_id: u64,
    error: &PoolError,
) -> Result<(), TcpRpcError> {
    send_error(
        service,
        fd,
        request_id,
        fetch_error_code(error),
        &error.to_string(),
    )
    .await
}

fn fetch_error_code(error: &PoolError) -> u32 {
    match error {
        PoolError::OriginNotFound => ERROR_ORIGIN_NOT_FOUND,
        _ => ERROR_SERVICE,
    }
}

async fn send_error(
    service: &TcpRpcService,
    fd: RawFd,
    request_id: u64,
    code: u32,
    message: &str,
) -> Result<(), TcpRpcError> {
    let message = truncate_message(message);
    let bytes = encode_error(request_id, &ErrorMetadata { code, message })?;
    send_heap_all(service, fd, bytes).await
}

async fn send_heap_all(
    service: &TcpRpcService,
    fd: RawFd,
    bytes: Vec<u8>,
) -> Result<(), TcpRpcError> {
    send_heap_all_with_flags(service, fd, bytes, 0).await
}

async fn send_heap_all_with_flags(
    service: &TcpRpcService,
    fd: RawFd,
    bytes: Vec<u8>,
    flags: i32,
) -> Result<(), TcpRpcError> {
    let mut sent = 0usize;
    while sent < bytes.len() {
        let count = service
            .handle
            .send_with_flags(fd, bytes[sent..].to_vec(), flags)
            .await?;
        if count == 0 {
            return Err(TcpRpcError::Protocol(
                "peer closed during heap send".to_string(),
            ));
        }
        sent += count;
    }
    Ok(())
}

fn decode_handshake(frame: &OwnedFrame) -> Result<Handshake, TcpRpcError> {
    if frame.header.kind != FrameKind::Handshake || !frame.payload.is_empty() {
        return Err(TcpRpcError::Protocol(
            "first application frame must be a handshake".to_string(),
        ));
    }
    Ok(Handshake::decode_metadata(&frame.metadata)?)
}

fn expected_page_count(offset: u64, len: u64, page_size: u32) -> Option<u32> {
    offset.checked_add(len)?;
    let page_size = page_size as u64;
    let first = offset % page_size;
    let covered = first.checked_add(len)?;
    let count = covered.checked_add(page_size - 1)? / page_size;
    u32::try_from(count).ok()
}

fn forwarded_peer_ttl(ttl: u8, forwarding_needed: bool) -> Result<Option<u8>, ()> {
    if !forwarding_needed {
        return Ok(None);
    }
    ttl.checked_sub(1).map(Some).ok_or(())
}

fn truncate_message(message: &str) -> String {
    const LIMIT: usize = super::wire::MAX_ERROR_MESSAGE_LEN;
    if message.len() <= LIMIT {
        return message.to_string();
    }
    let mut end = LIMIT;
    while !message.is_char_boundary(end) {
        end -= 1;
    }
    message[..end].to_string()
}

struct OrderedPage {
    page: FetchPage,
}

impl PartialEq for OrderedPage {
    fn eq(&self, other: &Self) -> bool {
        self.page.ordinal == other.page.ordinal
    }
}

impl Eq for OrderedPage {}

impl PartialOrd for OrderedPage {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for OrderedPage {
    fn cmp(&self, other: &Self) -> Ordering {
        other.page.ordinal.cmp(&self.page.ordinal)
    }
}

enum FetchFailure {
    Reply(String),
    Fetch(PoolError),
    Connection(TcpRpcError),
}

struct OwnedFrame {
    header: FrameHeader,
    metadata: Vec<u8>,
    payload: Vec<u8>,
}

struct FrameReader {
    handle: crate::ring::NetHandle,
    fd: RawFd,
    buffered: Vec<u8>,
}

impl FrameReader {
    fn new(handle: crate::ring::NetHandle, fd: RawFd) -> Self {
        Self {
            handle,
            fd,
            buffered: Vec::new(),
        }
    }

    async fn next(&mut self, expected_kind: FrameKind) -> Result<Option<OwnedFrame>, TcpRpcError> {
        loop {
            if let DecodeStatus::Complete { value: header, .. } =
                FrameHeader::decode(&self.buffered)?
                && header.kind != expected_kind
            {
                return Err(TcpRpcError::Protocol(format!(
                    "expected {expected_kind:?} frame, received {:?}",
                    header.kind
                )));
            }
            match decode_frame(&self.buffered)? {
                DecodeStatus::Complete { value, consumed } => {
                    let header = value.header;
                    let metadata = value.metadata.to_vec();
                    let payload = value.payload.to_vec();
                    self.buffered.drain(..consumed);
                    return Ok(Some(OwnedFrame {
                        header,
                        metadata,
                        payload,
                    }));
                }
                DecodeStatus::Incomplete { .. } => {}
            }

            let bytes = recv_application_data(&self.handle, self.fd).await?;
            if bytes.is_empty() {
                return Ok(None);
            }
            self.buffered.extend_from_slice(&bytes);
        }
    }
}

async fn recv_application_data(
    handle: &crate::ring::NetHandle,
    fd: RawFd,
) -> Result<Vec<u8>, TcpRpcError> {
    let (bytes, record_type) = handle.recv_msg(fd, RECV_CHUNK).await?;
    if bytes.is_empty() {
        return Ok(bytes);
    }
    match record_type {
        TLS_RECORD_TYPE_APPLICATION_DATA => Ok(bytes),
        TLS_RECORD_TYPE_ALERT => {
            if bytes.get(1).copied().unwrap_or(0) == 0 {
                return Ok(Vec::new());
            }
            return Err(TcpRpcError::Protocol(
                "fatal TLS alert before stream end".to_string(),
            ));
        }
        TLS_RECORD_TYPE_HANDSHAKE => Err(TcpRpcError::Protocol(
            "TLS post-handshake update requires connection rotation".to_string(),
        )),
        record_type => Err(TcpRpcError::Protocol(format!(
            "unsupported TLS record type {record_type}"
        ))),
    }
}

struct FdGuard(RawFd);

impl Drop for FdGuard {
    fn drop(&mut self) {
        // SAFETY: the connection future owns the accepted descriptor.
        unsafe {
            libc::close(self.0);
        }
    }
}

struct ConnectionMetricGuard;

impl ConnectionMetricGuard {
    fn new() -> Self {
        metrics::tcp_rpc_connections_delta(1);
        Self
    }
}

impl Drop for ConnectionMetricGuard {
    fn drop(&mut self) {
        metrics::tcp_rpc_connections_delta(-1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fanout::PageLoc;

    fn page(ordinal: usize) -> OrderedPage {
        OrderedPage {
            page: FetchPage {
                ordinal,
                pin_token: ordinal as u64,
                loc: PageLoc {
                    page_byte_offset: 0,
                    len: 1,
                },
            },
        }
    }

    #[test]
    fn response_heap_yields_pages_in_ordinal_order() {
        let mut heap = BinaryHeap::new();
        heap.push(page(3));
        heap.push(page(1));
        heap.push(page(2));
        assert_eq!(heap.pop().unwrap().page.ordinal, 1);
        assert_eq!(heap.pop().unwrap().page.ordinal, 2);
        assert_eq!(heap.pop().unwrap().page.ordinal, 3);
    }

    #[test]
    fn page_count_accounts_for_unaligned_first_page() {
        assert_eq!(expected_page_count(0, 4096, 4096), Some(1));
        assert_eq!(expected_page_count(1, 4096, 4096), Some(2));
        assert_eq!(expected_page_count(4095, 2, 4096), Some(2));
        assert_eq!(expected_page_count(u64::MAX, 2, 4096), None);
    }

    #[test]
    fn error_truncation_preserves_utf8_boundaries() {
        let suffix = String::from_utf8(vec![0xc3, 0xa9]).unwrap();
        let message = "a".repeat(super::super::wire::MAX_ERROR_MESSAGE_LEN - 1) + &suffix;
        let truncated = truncate_message(&message);
        assert!(truncated.len() <= super::super::wire::MAX_ERROR_MESSAGE_LEN);
        assert!(std::str::from_utf8(truncated.as_bytes()).is_ok());
    }

    #[test]
    fn origin_not_found_has_distinct_wire_error_code() {
        assert_eq!(fetch_error_code(&PoolError::OriginNotFound), 404);
        assert_eq!(fetch_error_code(&PoolError::Io(libc::EIO)), 500);
    }

    #[test]
    fn forwarding_decrements_ttl_but_local_owner_accepts_zero() {
        assert_eq!(forwarded_peer_ttl(4, true), Ok(Some(3)));
        assert_eq!(forwarded_peer_ttl(0, true), Err(()));
        assert_eq!(forwarded_peer_ttl(0, false), Ok(None));
    }

    #[test]
    fn registered_sends_are_split_into_bounded_bursts() {
        assert_eq!(registered_send_len(1, 8), 1);
        assert_eq!(registered_send_len(2 * 1024 * 1024, 1), 1024 * 1024);
        assert_eq!(registered_send_len(2 * 1024 * 1024, 4), 256 * 1024);
        assert_eq!(
            registered_send_len(2 * 1024 * 1024, 8),
            MIN_REGISTERED_SEND_CHUNK
        );
        assert_eq!(
            registered_send_len(2 * 1024 * 1024, 32),
            MIN_REGISTERED_SEND_CHUNK
        );
    }
}
