// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Mutually authenticated, shard-local TCP RPC transport.

mod client;
mod connection;
mod server;
mod wire;

pub const MAX_REQUESTS_PER_CONNECTION: usize = 2_048;

pub use client::{
    ClientPeerDirectory, ClientPeerDirectoryConfig, DEFAULT_TTL, TcpRpcClientPeer, TcpRpcStream,
    TcpRpcTransport,
};
pub use server::{
    PeerDirectory, TcpRpcConfig, TcpRpcDriver, TcpRpcError, TcpRpcService, bind_reuseport_listener,
};
pub use wire::{
    DecodeStatus, DecodedMessage, DecodedMetadata, ErrorMetadata, Frame, FrameHeader, FrameKind,
    FramePrefix, HEADER_LEN, Handshake, MAGIC, MAX_DESTINATION_PAGE_COUNT, MAX_ERROR_MESSAGE_LEN,
    MAX_METADATA_LEN, MAX_PAGE_BODY_LEN, MAX_PEER_NAME_LEN, MAX_REQUEST_BYTES, PageMetadata,
    RequestMetadata, VERSION, WireError, decode_frame, decode_prefix, encode_cancel, encode_end,
    encode_error, encode_frame, encode_handshake, encode_page_prefix, encode_request,
};
